package commerce_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/commerce"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// These tests run against a real PostgreSQL, because every property worth asserting here is a
// database property: a partial unique index enforcing one default currency, a row lock making
// concurrent redemptions serialise, a CHECK refusing a coupon that carries both a percentage
// and an amount. An in-memory fake would pass while the real system oversold the last use.

func newStore(t *testing.T) (*commerce.Store, context.Context) {
	t.Helper()
	s, _ := newStoreWithPool(t)
	// `currencies` is reference data the harness does not truncate, so a test that edits it
	// would leak into the next one. Restoring the seeded rows here is the same rule the
	// harness applies to `settings`: every test starts from the state a fresh installation has.
	resetCurrencies(t, s.Pool())
	return s, context.Background()
}

func newStoreWithPool(t *testing.T) (*commerce.Store, *store.Pool) {
	t.Helper()
	pool := storetest.DB(t)
	return commerce.New(pool), pool
}

// at pins the store's clock so a dated assertion is about the data, not about when the test
// happened to run.
func at(t *testing.T, s *commerce.Store, when time.Time) {
	t.Helper()
	s.Now = func() time.Time { return when }
}

var (
	t0  = time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	usd = money.MustLookupCurrency("USD")
)

// mustCustomer creates a customer row.
func mustCustomer(t *testing.T, pool *store.Pool, email string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO customers (email, name) VALUES ($1, $1) RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("create customer: %v", err)
	}
	return id
}

// mustProduct creates a product with one price version and returns both ids.
func mustProduct(t *testing.T, pool *store.Pool, slug string, amount, setup int64, currency string) (productID, priceID string) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx,
		`INSERT INTO products (slug, name) VALUES ($1, $1) RETURNING id`, slug).Scan(&productID); err != nil {
		t.Fatalf("create product: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, setup_minor,
			interval, anchor_day, anchor_timezone, name)
		VALUES ($1, 1, $2, $3, $4, 'monthly', 1, 'UTC', 'Monthly')
		RETURNING id`, productID, currency, amount, setup).Scan(&priceID); err != nil {
		t.Fatalf("create price: %v", err)
	}
	return productID, priceID
}

// --------------------------------------------------------------- coupon math --

// A percentage discount is exact. 12.5% of 10.00 is 1.25 and nothing else: the assertion is
// on the integer minor units, so a float implementation that produced 124 would fail here.
func TestPercentageCouponIsExact(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	customerID := mustCustomer(t, s.Pool(), "pct@example.test")
	productID, _ := mustProduct(t, s.Pool(), "vps-pct", 1000, 0, "USD")

	if _, err := s.CreateCoupon(ctx, commerce.CouponInput{
		Code:       "TWELVE5",
		Kind:       commerce.CouponPercentage,
		Percentage: "12.5",
	}); err != nil {
		t.Fatalf("create coupon: %v", err)
	}

	d, err := s.ValidateCoupon(ctx, commerce.DiscountRequest{
		Code:       "TWELVE5",
		CustomerID: customerID,
		ProductID:  productID,
		Amount:     money.New(1000, usd),
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if d.Total.Minor() != 125 {
		t.Fatalf("12.5%% of 10.00 = %d minor, want 125", d.Total.Minor())
	}

	// A third of a cent is not representable, so the rounding is explicit and reproducible:
	// 33.333% of 100 = 33.333 -> 33 half-up.
	if _, err := s.CreateCoupon(ctx, commerce.CouponInput{
		Code: "THIRD", Kind: commerce.CouponPercentage, Percentage: "33.333",
	}); err != nil {
		t.Fatalf("create coupon: %v", err)
	}
	d, err = s.ValidateCoupon(ctx, commerce.DiscountRequest{
		Code: "THIRD", CustomerID: customerID, ProductID: productID,
		Amount: money.New(100, usd),
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if d.Total.Minor() != 33 {
		t.Fatalf("33.333%% of 1.00 = %d minor, want 33", d.Total.Minor())
	}
}

// A fixed coupon is capped at what it is discounting: a 50.00 coupon on a 10.00 product
// reduces it to zero rather than producing a negative line.
func TestFixedCouponIsCappedAtThePrice(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	customerID := mustCustomer(t, s.Pool(), "fixed@example.test")
	productID, _ := mustProduct(t, s.Pool(), "vps-fixed", 1000, 500, "USD")

	if _, err := s.CreateCoupon(ctx, commerce.CouponInput{
		Code: "FIFTY", Kind: commerce.CouponFixed, AmountMinor: i64(5000), Currency: "USD",
	}); err != nil {
		t.Fatalf("create coupon: %v", err)
	}

	d, err := s.ValidateCoupon(ctx, commerce.DiscountRequest{
		Code: "FIFTY", CustomerID: customerID, ProductID: productID,
		Amount: money.New(1000, usd), Setup: money.New(500, usd),
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	// Price is reduced first, so 10.00 off the price and 5.00 off the setup.
	if d.Price.Minor() != 1000 || d.Setup.Minor() != 500 {
		t.Fatalf("discount = price %d setup %d, want 1000/500", d.Price.Minor(), d.Setup.Minor())
	}
	if d.Total.Minor() != 1500 {
		t.Fatalf("total discount = %d, want 1500 (capped at the price+setup)", d.Total.Minor())
	}

	// A coupon scoped to the setup fee must not touch the recurring price.
	if _, err := s.CreateCoupon(ctx, commerce.CouponInput{
		Code: "SETUPONLY", Kind: commerce.CouponFixed, AmountMinor: i64(200), Currency: "USD",
		AppliesTo: commerce.AppliesToSetup,
	}); err != nil {
		t.Fatalf("create coupon: %v", err)
	}
	d, err = s.ValidateCoupon(ctx, commerce.DiscountRequest{
		Code: "SETUPONLY", CustomerID: customerID, ProductID: productID,
		Amount: money.New(1000, usd), Setup: money.New(500, usd),
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if d.Price.Minor() != 0 || d.Setup.Minor() != 200 {
		t.Fatalf("setup-only discount = price %d setup %d, want 0/200", d.Price.Minor(), d.Setup.Minor())
	}
}

// ------------------------------------------------------------------- expiry --

// A coupon outside its window is refused with ErrExpired, and the boundary is exclusive at
// the end: a coupon expiring at 12:00 is not usable at 12:00.
func TestCouponExpiryWindow(t *testing.T) {
	s, ctx := newStore(t)
	customerID := mustCustomer(t, s.Pool(), "window@example.test")
	productID, _ := mustProduct(t, s.Pool(), "vps-window", 1000, 0, "USD")

	starts := t0
	expires := t0.Add(24 * time.Hour)
	if _, err := s.CreateCoupon(ctx, commerce.CouponInput{
		Code: "WINDOW", Kind: commerce.CouponPercentage, Percentage: "10",
		StartsAt: &starts, ExpiresAt: &expires,
	}); err != nil {
		t.Fatalf("create coupon: %v", err)
	}
	req := commerce.DiscountRequest{
		Code: "WINDOW", CustomerID: customerID, ProductID: productID,
		Amount: money.New(1000, usd),
	}

	// Before the window opens.
	at(t, s, t0.Add(-time.Second))
	if _, err := s.ValidateCoupon(ctx, req); !errors.Is(err, commerce.ErrExpired) {
		t.Fatalf("before start: err = %v, want ErrExpired", err)
	}
	// Inside the window.
	at(t, s, t0.Add(time.Hour))
	if _, err := s.ValidateCoupon(ctx, req); err != nil {
		t.Fatalf("inside window: %v", err)
	}
	// Exactly at expiry: closed.
	at(t, s, expires)
	if _, err := s.ValidateCoupon(ctx, req); !errors.Is(err, commerce.ErrExpired) {
		t.Fatalf("at expiry: err = %v, want ErrExpired", err)
	}
}

// ----------------------------------------------------------------- product --

// A product-restricted coupon applies to the products it names and nothing else.
func TestCouponProductRestriction(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	customerID := mustCustomer(t, s.Pool(), "restrict@example.test")
	inProduct, _ := mustProduct(t, s.Pool(), "vps-in", 1000, 0, "USD")
	outProduct, _ := mustProduct(t, s.Pool(), "vps-out", 1000, 0, "USD")

	ids := []string{inProduct}
	if _, err := s.CreateCoupon(ctx, commerce.CouponInput{
		Code: "ONLYIN", Kind: commerce.CouponPercentage, Percentage: "10",
		ProductIDs: &ids,
	}); err != nil {
		t.Fatalf("create coupon: %v", err)
	}

	d, err := s.ValidateCoupon(ctx, commerce.DiscountRequest{
		Code: "ONLYIN", CustomerID: customerID, ProductID: inProduct,
		Amount: money.New(1000, usd),
	})
	if err != nil {
		t.Fatalf("eligible product: %v", err)
	}
	if d.Total.Minor() != 100 {
		t.Fatalf("discount = %d, want 100", d.Total.Minor())
	}

	if _, err := s.ValidateCoupon(ctx, commerce.DiscountRequest{
		Code: "ONLYIN", CustomerID: customerID, ProductID: outProduct,
		Amount: money.New(1000, usd),
	}); !errors.Is(err, commerce.ErrNotApplicable) {
		t.Fatalf("other product: err = %v, want ErrNotApplicable", err)
	}

	// A fixed coupon in the wrong currency is not applicable either: "10 off" means nothing
	// across currencies, and there is no exchange rate to convert with.
	if _, err := s.CreateCoupon(ctx, commerce.CouponInput{
		Code: "EURO10", Kind: commerce.CouponFixed, AmountMinor: i64(1000), Currency: "EUR",
	}); err != nil {
		t.Fatalf("create coupon: %v", err)
	}
	if _, err := s.ValidateCoupon(ctx, commerce.DiscountRequest{
		Code: "EURO10", CustomerID: customerID, ProductID: inProduct,
		Amount: money.New(1000, usd),
	}); !errors.Is(err, commerce.ErrNotApplicable) {
		t.Fatalf("wrong currency: err = %v, want ErrNotApplicable", err)
	}
}

// --------------------------------------------------------- redemption locks --

// max_uses is enforced under a row lock. Ten goroutines race for three uses; exactly three
// succeed, and the redemption table has exactly three rows. A read-then-insert without the
// lock overshoots here, which is the defect this test exists to catch.
func TestMaxUsesEnforcedUnderConcurrentRedemption(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	pool := s.Pool()
	productID, _ := mustProduct(t, pool, "vps-race", 1000, 0, "USD")
	_ = productID

	three := 3
	coupon, err := s.CreateCoupon(ctx, commerce.CouponInput{
		Code: "RACE", Kind: commerce.CouponPercentage, Percentage: "10",
		MaxUses: &three,
	})
	if err != nil {
		t.Fatalf("create coupon: %v", err)
	}

	const racers = 10
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		ok      int
		refused int
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		customerID := mustCustomer(t, pool, "racer"+string(rune('a'+i))+"@example.test")
		wg.Add(1)
		go func(customerID string) {
			defer wg.Done()
			<-start
			err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				_, err := s.Redeem(ctx, tx, commerce.RedemptionRequest{
					CouponID:      coupon.ID,
					CustomerID:    customerID,
					DiscountMinor: 100,
					Currency:      "USD",
				})
				return err
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, commerce.ErrExhausted):
				refused++
			default:
				t.Errorf("unexpected redemption error: %v", err)
			}
		}(customerID)
	}
	close(start)
	wg.Wait()

	if ok != 3 {
		t.Fatalf("successful redemptions = %d, want 3", ok)
	}
	if refused != racers-3 {
		t.Fatalf("refused = %d, want %d", refused, racers-3)
	}
	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM coupon_redemptions WHERE coupon_id = $1`, coupon.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 3 {
		t.Fatalf("redemption rows = %d, want 3", rows)
	}
}

// max_uses_per_user counts only the named customer's uses, under the same lock.
func TestMaxUsesPerUserIsPerCustomer(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	pool := s.Pool()
	alice := mustCustomer(t, pool, "alice@example.test")
	bob := mustCustomer(t, pool, "bob@example.test")

	one := 1
	coupon, err := s.CreateCoupon(ctx, commerce.CouponInput{
		Code: "ONCE", Kind: commerce.CouponPercentage, Percentage: "10",
		MaxUsesPerUser: &one,
	})
	if err != nil {
		t.Fatalf("create coupon: %v", err)
	}
	redeem := func(customerID string) error {
		return pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := s.Redeem(ctx, tx, commerce.RedemptionRequest{
				CouponID: coupon.ID, CustomerID: customerID,
				DiscountMinor: 100, Currency: "USD",
			})
			return err
		})
	}
	if err := redeem(alice); err != nil {
		t.Fatalf("alice first use: %v", err)
	}
	if err := redeem(alice); !errors.Is(err, commerce.ErrExhausted) {
		t.Fatalf("alice second use: err = %v, want ErrExhausted", err)
	}
	if err := redeem(bob); err != nil {
		t.Fatalf("bob first use: %v", err)
	}
}

// A retired coupon refuses new redemptions but keeps its redemption history.
func TestRetiredCouponKeepsHistory(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	pool := s.Pool()
	customerID := mustCustomer(t, pool, "retire@example.test")

	coupon, err := s.CreateCoupon(ctx, commerce.CouponInput{
		Code: "RETIRED", Kind: commerce.CouponPercentage, Percentage: "10",
	})
	if err != nil {
		t.Fatalf("create coupon: %v", err)
	}
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.Redeem(ctx, tx, commerce.RedemptionRequest{
			CouponID: coupon.ID, CustomerID: customerID,
			DiscountMinor: 100, Currency: "USD",
		})
		return err
	}); err != nil {
		t.Fatalf("redeem: %v", err)
	}

	off := false
	if _, err := s.UpdateCoupon(ctx, coupon.ID, commerce.CouponInput{Enabled: &off}); err != nil {
		t.Fatalf("retire: %v", err)
	}
	// Retired reads as "not found" to a customer: the promotion no longer exists as far as
	// the storefront is concerned.
	if _, err := s.ValidateCoupon(ctx, commerce.DiscountRequest{
		Code: "RETIRED", CustomerID: customerID, Amount: money.New(1000, usd),
	}); !errors.Is(err, commerce.ErrNotFound) {
		t.Fatalf("retired coupon: err = %v, want ErrNotFound", err)
	}
	redemptions, err := s.Redemptions(ctx, coupon.ID)
	if err != nil {
		t.Fatalf("redemptions: %v", err)
	}
	if len(redemptions) != 1 {
		t.Fatalf("redemption history = %d rows, want 1", len(redemptions))
	}
}

// A code is unique case-insensitively: "SAVE10" and "save10" are one promotion.
func TestCouponCodeIsUniqueCaseInsensitively(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	if _, err := s.CreateCoupon(ctx, commerce.CouponInput{
		Code: "SAVE10", Kind: commerce.CouponPercentage, Percentage: "10",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.CreateCoupon(ctx, commerce.CouponInput{
		Code: "save10", Kind: commerce.CouponPercentage, Percentage: "10",
	}); !errors.Is(err, commerce.ErrCodeTaken) {
		t.Fatalf("duplicate code: err = %v, want ErrCodeTaken", err)
	}
	// And a lookup by either spelling finds it.
	if _, err := s.CouponByCode(ctx, "sAvE10"); err != nil {
		t.Fatalf("lookup: %v", err)
	}
}

// A coupon carrying both a percentage and an amount is refused before it reaches the
// database's CHECK, so the error is a domain error rather than a constraint violation.
func TestCouponRejectsBothValueKinds(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	_, err := s.CreateCoupon(ctx, commerce.CouponInput{
		Code: "BOTH", Kind: commerce.CouponPercentage, Percentage: "10",
		AmountMinor: i64(500), Currency: "USD",
	})
	if !errors.Is(err, commerce.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

func i64(v int64) *int64 { return &v }

// Switching a coupon's kind in place would reinterpret what every historic redemption meant,
// so it is refused with an explanation rather than a confusing constraint error.
func TestCouponKindIsImmutable(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	coupon, err := s.CreateCoupon(ctx, commerce.CouponInput{
		Code: "KIND", Kind: commerce.CouponPercentage, Percentage: "10",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = s.UpdateCoupon(ctx, coupon.ID, commerce.CouponInput{
		Kind: commerce.CouponFixed, AmountMinor: i64(500), Currency: "USD",
	})
	if !errors.Is(err, commerce.ErrInvalidInput) {
		t.Fatalf("kind change: err = %v, want ErrInvalidInput", err)
	}
	// Editing the value of the same kind is fine.
	if _, err := s.UpdateCoupon(ctx, coupon.ID, commerce.CouponInput{Percentage: "15"}); err != nil {
		t.Fatalf("edit percentage: %v", err)
	}
}
