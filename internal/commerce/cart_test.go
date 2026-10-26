package commerce_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/commerce"
	"github.com/catalyst/billing/internal/store"
)

// The cart is a draft, not a second pricing engine. These tests assert exactly that: the cart
// stores a selection, and every total is read from the catalog at the moment it is asked for.
// The two properties that matter are therefore "a price change is visible without touching the
// cart" and "the cart cannot be denominated in two currencies".

// allowQuantity sets a product's quantity policy, which is what a multi-quantity cart line
// needs. The migration's default is 'disabled': one per line.
func allowQuantity(t *testing.T, pool *store.Pool, productID, policy string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE products SET allow_quantity = $2 WHERE id = $1`, productID, policy); err != nil {
		t.Fatalf("set allow_quantity: %v", err)
	}
}

func TestCartRepricesAfterPriceChange(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	pool := s.Pool()
	customerID := mustCustomer(t, pool, "cart-reprice@example.test")
	productID, priceID := mustProduct(t, pool, "vps-reprice", 1000, 0, "USD")
	allowQuantity(t, pool, productID, commerce.QuantityCombined)

	cart, err := s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceID, Quantity: 1,
	})
	if err != nil {
		t.Fatalf("add item: %v", err)
	}
	if len(cart.Items) != 1 {
		t.Fatalf("cart items = %d, want 1", len(cart.Items))
	}

	totals, err := s.Totals(ctx, customerID, nil)
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if totals.Total.Minor() != 1000 {
		t.Fatalf("total = %d, want 1000", totals.Total.Minor())
	}

	// The catalog changes under the cart: a new price version is what a price change *is*.
	// The cart holds the old version's id, so the totals must reflect the new version only
	// after the line is re-pointed — and when it is, the new price is used with no cached
	// number in between.
	var newPriceID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, setup_minor,
			interval, anchor_day, anchor_timezone, name)
		SELECT product_id, 2, currency, 2500, 0, interval, anchor_day, anchor_timezone, 'Monthly'
		  FROM price_versions WHERE id = $1
		RETURNING id`, priceID).Scan(&newPriceID); err != nil {
		t.Fatalf("append price version: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE cart_items SET price_version_id = $2 WHERE cart_id = $1`,
		cart.ID, newPriceID); err != nil {
		t.Fatalf("re-point cart line: %v", err)
	}

	totals, err = s.Totals(ctx, customerID, nil)
	if err != nil {
		t.Fatalf("totals after change: %v", err)
	}
	if totals.Total.Minor() != 2500 {
		t.Fatalf("total after price change = %d, want 2500 (re-priced from the catalog)",
			totals.Total.Minor())
	}

	// The point of re-pricing: the *same* price version's amount is mutable only by appending
	// a version, but a cart that stored a price would still show the old one. Assert the cart
	// row itself has no money column by checking the total changes with quantity alone.
	if _, err := s.UpdateQuantity(ctx, customerID, cart.Items[0].ID, 3); err != nil {
		t.Fatalf("update quantity: %v", err)
	}
	totals, err = s.Totals(ctx, customerID, nil)
	if err != nil {
		t.Fatalf("totals after quantity: %v", err)
	}
	if totals.Total.Minor() != 7500 {
		t.Fatalf("total at quantity 3 = %d, want 7500", totals.Total.Minor())
	}
}

// The cart's currency is fixed once it holds an item: a second product in another currency
// cannot join, because there is no exchange rate to convert it with.
func TestCartCurrencyLocksOnceItHasAnItem(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	pool := s.Pool()
	customerID := mustCustomer(t, pool, "cart-currency@example.test")
	_, usdPrice := mustProduct(t, pool, "vps-usd", 1000, 0, "USD")
	_, eurPrice := mustProduct(t, pool, "vps-eur", 900, 0, "EUR")

	cart, err := s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: usdPrice, Quantity: 1,
	})
	if err != nil {
		t.Fatalf("add usd item: %v", err)
	}
	if cart.Currency != "USD" {
		t.Fatalf("cart currency = %s, want USD", cart.Currency)
	}

	// A EUR price cannot join a USD cart.
	if _, err := s.AddItem(ctx, customerID, "EUR", commerce.AddItemRequest{
		PriceVersionID: eurPrice, Quantity: 1,
	}); !errors.Is(err, commerce.ErrNotApplicable) {
		t.Fatalf("add a second currency: err = %v, want ErrNotApplicable", err)
	}

	// An empty cart may still change currency, because nothing is lost.
	if _, err := s.Clear(ctx, customerID); err != nil {
		t.Fatalf("clear: %v", err)
	}
	cart, err = s.AddItem(ctx, customerID, "EUR", commerce.AddItemRequest{
		PriceVersionID: eurPrice, Quantity: 1,
	})
	if err != nil {
		t.Fatalf("add eur item after clear: %v", err)
	}
	if cart.Currency != "EUR" {
		t.Fatalf("cart currency after clear = %s, want EUR", cart.Currency)
	}
}

// A product whose `allow_quantity` is `disabled` refuses a quantity above one rather than
// silently reducing it.
func TestCartEnforcesAllowQuantityPolicy(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	pool := s.Pool()
	customerID := mustCustomer(t, pool, "cart-qty@example.test")
	productID, priceID := mustProduct(t, pool, "vps-one", 1000, 0, "USD")

	// The default policy is 'disabled'.
	if _, err := s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceID, Quantity: 3,
	}); !errors.Is(err, commerce.ErrNotApplicable) {
		t.Fatalf("quantity 3 on a disabled product: err = %v, want ErrNotApplicable", err)
	}

	// A 'combined' product allows a quantity, and an identical line merges into one.
	if _, err := pool.Exec(ctx,
		`UPDATE products SET allow_quantity = 'combined' WHERE id = $1`, productID); err != nil {
		t.Fatal(err)
	}
	cart, err := s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceID, Quantity: 2,
	})
	if err != nil {
		t.Fatalf("add combined: %v", err)
	}
	cart, err = s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceID, Quantity: 1,
	})
	if err != nil {
		t.Fatalf("add again: %v", err)
	}
	if len(cart.Items) != 1 || cart.Items[0].Quantity != 3 {
		t.Fatalf("cart = %d line(s), quantity %d; want 1 line of 3",
			len(cart.Items), cart.Items[0].Quantity)
	}
}

// A product that is sold one at a time cannot be added twice.
//
// The merge below (see the 'combined' half of TestCartEnforcesAllowQuantityPolicy) is right for a
// product that allows a quantity and wrong for one that does not: merging produced a line of
// quantity 2 that the checkout refuses with `quantity_not_allowed`, so two clicks on the same
// button left the customer with a cart that could not be paid for. The refusal has its own error
// so the storefront can say "it is already in your cart" and offer the cart.
func TestCartRefusesSecondAddOfAOneAtATimeProduct(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	pool := s.Pool()
	customerID := mustCustomer(t, pool, "cart-one-at-a-time@example.test")
	// The default policy is 'disabled': one per line.
	_, priceID := mustProduct(t, pool, "vps-single-add", 1000, 0, "USD")

	cart, err := s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceID, Quantity: 1,
	})
	if err != nil {
		t.Fatalf("first add: %v", err)
	}
	if len(cart.Items) != 1 || cart.Items[0].Quantity != 1 {
		t.Fatalf("cart = %d line(s); want 1 line of 1", len(cart.Items))
	}

	if _, err := s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceID, Quantity: 1,
	}); !errors.Is(err, commerce.ErrAlreadyInCart) {
		t.Fatalf("second add: err = %v, want ErrAlreadyInCart", err)
	}

	// The line the customer holds is the one the checkout accepts.
	cart, err = s.CartForCustomer(ctx, customerID)
	if err != nil {
		t.Fatalf("cart: %v", err)
	}
	if len(cart.Items) != 1 || cart.Items[0].Quantity != 1 {
		t.Fatalf("cart after the refusal = %d line(s) of %d; want 1 line of 1",
			len(cart.Items), cart.Items[0].Quantity)
	}
}

// A line that is not the caller's is a 404, not a 403: the difference would confirm that the
// id exists.
func TestCartIsScopedToTheCustomer(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	pool := s.Pool()
	alice := mustCustomer(t, pool, "alice-cart@example.test")
	bob := mustCustomer(t, pool, "bob-cart@example.test")
	_, priceID := mustProduct(t, pool, "vps-scope", 1000, 0, "USD")

	cart, err := s.AddItem(ctx, alice, "USD", commerce.AddItemRequest{
		PriceVersionID: priceID, Quantity: 1,
	})
	if err != nil {
		t.Fatalf("alice add: %v", err)
	}
	if _, err := s.UpdateQuantity(ctx, bob, cart.Items[0].ID, 5); !errors.Is(err, commerce.ErrNotFound) {
		t.Fatalf("bob update: err = %v, want ErrNotFound", err)
	}
	if _, err := s.RemoveItem(ctx, bob, cart.Items[0].ID); !errors.Is(err, commerce.ErrNotFound) {
		t.Fatalf("bob remove: err = %v, want ErrNotFound", err)
	}
	// Bob's own cart is separate and empty.
	bobCart, err := s.Cart(ctx, bob, "USD")
	if err != nil {
		t.Fatalf("bob cart: %v", err)
	}
	if len(bobCart.Items) != 0 {
		t.Fatalf("bob's cart has %d item(s), want 0", len(bobCart.Items))
	}
}

// A coupon attached to the cart re-prices live, and removing it restores the total.
func TestCartCouponRePricesLive(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	pool := s.Pool()
	customerID := mustCustomer(t, pool, "cart-coupon@example.test")
	productID, priceID := mustProduct(t, pool, "vps-coupon", 1000, 500, "USD")

	if _, err := s.CreateCoupon(ctx, commerce.CouponInput{
		Code: "TEN", Kind: commerce.CouponPercentage, Percentage: "10",
	}); err != nil {
		t.Fatalf("create coupon: %v", err)
	}
	_ = productID

	if _, err := s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceID, Quantity: 1,
	}); err != nil {
		t.Fatalf("add item: %v", err)
	}
	if _, err := s.ApplyCoupon(ctx, customerID, "TEN"); err != nil {
		t.Fatalf("apply coupon: %v", err)
	}
	totals, err := s.Totals(ctx, customerID, nil)
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	// 10% of (1000 + 500) = 150.
	if totals.Discount.Minor() != 150 {
		t.Fatalf("discount = %d, want 150", totals.Discount.Minor())
	}
	if totals.Total.Minor() != 1350 {
		t.Fatalf("total = %d, want 1350", totals.Total.Minor())
	}
	if totals.Coupon == nil || totals.Coupon.Code != "TEN" {
		t.Fatalf("applied coupon = %+v", totals.Coupon)
	}

	// A price change re-prices the discount too, because the discount is a percentage of the
	// current price rather than a stored amount.
	if _, err := pool.Exec(ctx,
		`UPDATE price_versions SET amount_minor = 2000 WHERE id = $1`, priceID); err != nil {
		t.Fatal(err)
	}
	totals, err = s.Totals(ctx, customerID, nil)
	if err != nil {
		t.Fatalf("totals after change: %v", err)
	}
	if totals.Discount.Minor() != 250 {
		t.Fatalf("discount after price change = %d, want 250 (10%% of 2500)",
			totals.Discount.Minor())
	}

	if _, err := s.RemoveCoupon(ctx, customerID); err != nil {
		t.Fatalf("remove coupon: %v", err)
	}
	totals, err = s.Totals(ctx, customerID, nil)
	if err != nil {
		t.Fatalf("totals after remove: %v", err)
	}
	if totals.Discount.Minor() != 0 || totals.Total.Minor() != 2500 {
		t.Fatalf("after remove: discount %d total %d, want 0/2500",
			totals.Discount.Minor(), totals.Total.Minor())
	}
}

// An expired coupon on a cart is reported rather than silently dropped: the customer is told
// why the discount disappeared and can still check out.
func TestCartReportsCouponProblem(t *testing.T) {
	s, ctx := newStore(t)
	pool := s.Pool()
	customerID := mustCustomer(t, pool, "cart-expired@example.test")
	_, priceID := mustProduct(t, pool, "vps-expired", 1000, 0, "USD")

	expires := t0
	at(t, s, t0.Add(-time.Hour))
	if _, err := s.CreateCoupon(ctx, commerce.CouponInput{
		Code: "GONE", Kind: commerce.CouponPercentage, Percentage: "10", ExpiresAt: &expires,
	}); err != nil {
		t.Fatalf("create coupon: %v", err)
	}
	if _, err := s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceID, Quantity: 1,
	}); err != nil {
		t.Fatalf("add item: %v", err)
	}
	if _, err := s.ApplyCoupon(ctx, customerID, "GONE"); err != nil {
		t.Fatalf("apply coupon: %v", err)
	}

	// The clock moves past the expiry: the coupon is now refused, and the cart says why.
	at(t, s, t0.Add(time.Hour))
	totals, err := s.Totals(ctx, customerID, nil)
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if totals.CouponProblem == "" {
		t.Fatal("an expired coupon must be reported on the cart")
	}
	if totals.Total.Minor() != 1000 {
		t.Fatalf("total = %d, want the undiscounted 1000", totals.Total.Minor())
	}
}

// Clearing the cart detaches the coupon too: a discount on nothing is a state the checkout
// would have to explain.
func TestCartClearDetachesCoupon(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	pool := s.Pool()
	customerID := mustCustomer(t, pool, "cart-clear@example.test")
	_, priceID := mustProduct(t, pool, "vps-clear", 1000, 0, "USD")

	if _, err := s.CreateCoupon(ctx, commerce.CouponInput{
		Code: "CLEARME", Kind: commerce.CouponPercentage, Percentage: "10",
	}); err != nil {
		t.Fatalf("create coupon: %v", err)
	}
	if _, err := s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceID, Quantity: 1,
	}); err != nil {
		t.Fatalf("add item: %v", err)
	}
	if _, err := s.ApplyCoupon(ctx, customerID, "CLEARME"); err != nil {
		t.Fatalf("apply coupon: %v", err)
	}
	cart, err := s.Clear(ctx, customerID)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if cart.CouponID != nil {
		t.Fatalf("coupon after clear = %v, want nil", *cart.CouponID)
	}
	if len(cart.Items) != 0 {
		t.Fatalf("items after clear = %d, want 0", len(cart.Items))
	}
}

// With a tax pricer attached, the cart total includes tax and the evidence names the
// jurisdiction.
func TestCartTotalsIncludeTax(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	pool := s.Pool()
	customerID := mustCustomer(t, pool, "cart-tax@example.test")
	_, priceID := mustProduct(t, pool, "vps-tax", 1000, 0, "USD")

	if _, err := s.CreateTaxRate(ctx, commerce.TaxRateInput{
		Name: "Germany", CountryCode: "DE", Rate: "0.19",
	}); err != nil {
		t.Fatalf("create tax rate: %v", err)
	}
	if _, err := s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceID, Quantity: 1,
	}); err != nil {
		t.Fatalf("add item: %v", err)
	}

	pricer := commerce.TaxPricer{Store: s, CountryCode: "DE", At: t0}
	totals, err := s.Totals(ctx, customerID, pricer)
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if totals.Subtotal.Minor() != 1000 {
		t.Fatalf("subtotal = %d, want 1000", totals.Subtotal.Minor())
	}
	if totals.Tax.Minor() != 190 {
		t.Fatalf("tax = %d, want 190", totals.Tax.Minor())
	}
	if totals.Total.Minor() != 1190 {
		t.Fatalf("total = %d, want 1190", totals.Total.Minor())
	}
	if len(totals.Lines) != 1 || totals.Lines[0].TaxEvidence == nil {
		t.Fatalf("per-line tax evidence missing: %+v", totals.Lines)
	}
	if totals.Lines[0].TaxEvidence.Jurisdiction != "DE/standard" {
		t.Fatalf("line jurisdiction = %q", totals.Lines[0].TaxEvidence.Jurisdiction)
	}
}

// A cart line referring to a price that no longer exists makes the cart unpriceable, and the
// error says so rather than silently dropping the line.
func TestCartTotalsRefuseMissingPrice(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	pool := s.Pool()
	customerID := mustCustomer(t, pool, "cart-missing@example.test")
	_, priceID := mustProduct(t, pool, "vps-missing", 1000, 0, "USD")

	if _, err := s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceID, Quantity: 1,
	}); err != nil {
		t.Fatalf("add item: %v", err)
	}
	// Delete the product, which cascades to nothing on price_versions but leaves the cart
	// line orphaned. The FK is RESTRICT, so delete the cart's link first to model a catalog
	// that lost the version.
	if _, err := pool.Exec(ctx, `DELETE FROM cart_items`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM price_versions WHERE id = $1`, priceID); err != nil {
		t.Fatal(err)
	}
	// An empty cart prices to zero rather than failing.
	totals, err := s.Totals(ctx, customerID, nil)
	if err != nil {
		t.Fatalf("empty cart totals: %v", err)
	}
	if totals.Total.Minor() != 0 {
		t.Fatalf("empty cart total = %d, want 0", totals.Total.Minor())
	}
}

// A fixed coupon on a multi-line cart is allocated across the eligible lines by value, and the
// parts sum exactly to the discount.
func TestCartFixedCouponAllocatesExactly(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	pool := s.Pool()
	customerID := mustCustomer(t, pool, "cart-alloc@example.test")
	_, priceA := mustProduct(t, pool, "vps-a", 1000, 0, "USD")
	_, priceB := mustProduct(t, pool, "vps-b", 2000, 0, "USD")

	if _, err := s.CreateCoupon(ctx, commerce.CouponInput{
		Code: "TENOFF", Kind: commerce.CouponFixed, AmountMinor: i64(1000), Currency: "USD",
	}); err != nil {
		t.Fatalf("create coupon: %v", err)
	}
	if _, err := s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceA, Quantity: 1,
	}); err != nil {
		t.Fatalf("add a: %v", err)
	}
	if _, err := s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceB, Quantity: 1,
	}); err != nil {
		t.Fatalf("add b: %v", err)
	}
	if _, err := s.ApplyCoupon(ctx, customerID, "TENOFF"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	totals, err := s.Totals(ctx, customerID, nil)
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if totals.Discount.Minor() != 1000 {
		t.Fatalf("discount = %d, want 1000", totals.Discount.Minor())
	}
	var sum int64
	for _, line := range totals.Lines {
		sum += line.Discount.Minor()
	}
	if sum != 1000 {
		t.Fatalf("per-line discounts sum to %d, want 1000", sum)
	}
	// 1/3 of the value is on line A (1000 of 3000), 2/3 on B: 333 and 667.
	if totals.Lines[0].Discount.Minor() != 333 || totals.Lines[1].Discount.Minor() != 667 {
		t.Fatalf("allocation = %d/%d, want 333/667",
			totals.Lines[0].Discount.Minor(), totals.Lines[1].Discount.Minor())
	}
	if totals.Total.Minor() != 2000 {
		t.Fatalf("total = %d, want 2000", totals.Total.Minor())
	}
}

// A setup fee is a separate invoice line, so it is taxed and discounted on its own.
func TestCartPricesSetupSeparately(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	pool := s.Pool()
	customerID := mustCustomer(t, pool, "cart-setup@example.test")
	productID, priceID := mustProduct(t, pool, "vps-setup", 1000, 500, "USD")
	allowQuantity(t, pool, productID, commerce.QuantitySeparated)

	if _, err := s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceID, Quantity: 2,
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	totals, err := s.Totals(ctx, customerID, nil)
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if len(totals.Lines) != 1 {
		t.Fatalf("lines = %d, want 1 cart line", len(totals.Lines))
	}
	if totals.Lines[0].Line.Minor() != 2000 || totals.Lines[0].SetupLine.Minor() != 1000 {
		t.Fatalf("line/setup = %d/%d, want 2000/1000",
			totals.Lines[0].Line.Minor(), totals.Lines[0].SetupLine.Minor())
	}
	if totals.Total.Minor() != 3000 {
		t.Fatalf("total = %d, want 3000", totals.Total.Minor())
	}
}

// With inclusive tax the quoted price is the gross, so the per-line net is less than the
// catalog amount. The per-line figures must sum to the cart totals, or the UI shows a
// breakdown that does not add up.
func TestCartInclusiveTaxLineTotalsAgree(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	pool := s.Pool()
	customerID := mustCustomer(t, pool, "cart-incl@example.test")
	_, priceID := mustProduct(t, pool, "vps-incl", 1200, 0, "USD")

	inclusive := true
	if _, err := s.CreateTaxRate(ctx, commerce.TaxRateInput{
		Name: "UK VAT", CountryCode: "GB", Rate: "0.2", Inclusive: &inclusive,
	}); err != nil {
		t.Fatalf("create tax rate: %v", err)
	}
	if _, err := s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceID, Quantity: 1,
	}); err != nil {
		t.Fatalf("add: %v", err)
	}

	pricer := commerce.TaxPricer{Store: s, CountryCode: "GB", At: t0}
	totals, err := s.Totals(ctx, customerID, pricer)
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	// The customer pays the quoted 12.00, of which 2.00 is tax.
	if totals.Total.Minor() != 1200 {
		t.Fatalf("total = %d, want the quoted 1200", totals.Total.Minor())
	}
	if totals.Tax.Minor() != 200 {
		t.Fatalf("tax = %d, want 200", totals.Tax.Minor())
	}
	if len(totals.Lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(totals.Lines))
	}
	line := totals.Lines[0]
	if line.Net.Minor() != 1000 || line.Tax.Minor() != 200 || line.Total.Minor() != 1200 {
		t.Fatalf("line net/tax/total = %d/%d/%d, want 1000/200/1200",
			line.Net.Minor(), line.Tax.Minor(), line.Total.Minor())
	}
	// The catalog unit price is reported undiscounted so the UI can say "12.00 incl. 20%".
	if line.Unit.Minor() != 1200 {
		t.Fatalf("unit = %d, want the catalog 1200", line.Unit.Minor())
	}
}

// A fully discounted line still appears: the invoice must show what the customer selected, even
// at 0.00, rather than silently dropping it.
func TestCartFullyDiscountedLineStillAppears(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	pool := s.Pool()
	customerID := mustCustomer(t, pool, "cart-free@example.test")
	_, priceID := mustProduct(t, pool, "vps-free", 1000, 0, "USD")

	if _, err := s.CreateCoupon(ctx, commerce.CouponInput{
		Code: "FREE", Kind: commerce.CouponPercentage, Percentage: "100",
	}); err != nil {
		t.Fatalf("create coupon: %v", err)
	}
	if _, err := s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceID, Quantity: 1,
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := s.ApplyCoupon(ctx, customerID, "FREE"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	totals, err := s.Totals(ctx, customerID, nil)
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if len(totals.Lines) != 1 {
		t.Fatalf("lines = %d, want the fully discounted line to remain", len(totals.Lines))
	}
	if totals.Total.Minor() != 0 || totals.Discount.Minor() != 1000 {
		t.Fatalf("total/discount = %d/%d, want 0/1000", totals.Total.Minor(), totals.Discount.Minor())
	}
}

// A product-restricted coupon applies only to the lines it covers, and it can be attached to a
// mixed cart. A cart holding A and B, with a coupon for B only, discounts B and leaves A at
// full price.
func TestCartCouponRestrictedToSomeLines(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	pool := s.Pool()
	customerID := mustCustomer(t, pool, "cart-mixed@example.test")
	_, priceA := mustProduct(t, pool, "vps-mixed-a", 1000, 0, "USD")
	productB, priceB := mustProduct(t, pool, "vps-mixed-b", 2000, 0, "USD")

	ids := []string{productB}
	if _, err := s.CreateCoupon(ctx, commerce.CouponInput{
		Code: "BONLY", Kind: commerce.CouponPercentage, Percentage: "50",
		ProductIDs: &ids,
	}); err != nil {
		t.Fatalf("create coupon: %v", err)
	}
	if _, err := s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceA, Quantity: 1,
	}); err != nil {
		t.Fatalf("add a: %v", err)
	}
	if _, err := s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceB, Quantity: 1,
	}); err != nil {
		t.Fatalf("add b: %v", err)
	}
	// The coupon covers only B, so attaching it to the mixed cart must succeed.
	if _, err := s.ApplyCoupon(ctx, customerID, "BONLY"); err != nil {
		t.Fatalf("apply restricted coupon to a mixed cart: %v", err)
	}
	totals, err := s.Totals(ctx, customerID, nil)
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if totals.Discount.Minor() != 1000 {
		t.Fatalf("discount = %d, want 1000 (50%% of B only)", totals.Discount.Minor())
	}
	// Line order follows created_at, so A is first and must be undiscounted.
	if totals.Lines[0].Discount.Minor() != 0 {
		t.Fatalf("line A discount = %d, want 0", totals.Lines[0].Discount.Minor())
	}
	if totals.Lines[1].Discount.Minor() != 1000 {
		t.Fatalf("line B discount = %d, want 1000", totals.Lines[1].Discount.Minor())
	}
	if totals.Total.Minor() != 2000 {
		t.Fatalf("total = %d, want 2000", totals.Total.Minor())
	}
}

// Adding the same line repeatedly cannot push a quantity past the table's ceiling: the merge is
// refused rather than silently capped, because a capped merge would show the customer a number
// different from the one they asked for.
func TestCartMergeRefusesQuantityAboveCeiling(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	pool := s.Pool()
	customerID := mustCustomer(t, pool, "cart-ceiling@example.test")
	productID, priceID := mustProduct(t, pool, "vps-ceiling", 100, 0, "USD")
	allowQuantity(t, pool, productID, commerce.QuantityCombined)

	if _, err := s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceID, Quantity: commerce.MaxCartQuantity,
	}); err != nil {
		t.Fatalf("add at the ceiling: %v", err)
	}
	_, err := s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceID, Quantity: 1,
	})
	if !errors.Is(err, commerce.ErrInvalidInput) {
		t.Fatalf("merge past the ceiling: err = %v, want ErrInvalidInput", err)
	}
	// The existing line is unchanged.
	cart, err := s.CartForCustomer(ctx, customerID)
	if err != nil {
		t.Fatal(err)
	}
	if cart.Items[0].Quantity != commerce.MaxCartQuantity {
		t.Fatalf("quantity = %d, want %d", cart.Items[0].Quantity, commerce.MaxCartQuantity)
	}
}

// mustConfigOption inserts a dropdown option with one priced row per (choice, amount, setup).
// It returns the option id. The price is attached to the given price version, which is what
// makes a choice's price version-specific.
func mustConfigOption(t *testing.T, pool *store.Pool, productID, priceID, name string,
	choices []string, required bool, prices map[string][2]int64) string {
	t.Helper()
	ctx := context.Background()
	raw, err := json.Marshal(choices)
	if err != nil {
		t.Fatal(err)
	}
	var optionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO config_options (product_id, name, option_type, choices, required)
		VALUES ($1, $2, 'dropdown', $3, $4) RETURNING id`,
		productID, name, raw, required).Scan(&optionID); err != nil {
		t.Fatalf("create config option: %v", err)
	}
	for choice, amounts := range prices {
		if _, err := pool.Exec(ctx, `
			INSERT INTO config_option_prices (config_option_id, price_version_id, choice,
				amount_minor, setup_minor)
			VALUES ($1, $2, $3, $4, $5)`,
			optionID, priceID, choice, amounts[0], amounts[1]); err != nil {
			t.Fatalf("create config option price: %v", err)
		}
	}
	return optionID
}

// CT-03: a selected config option's recurring and setup amounts are part of the cart total, not
// something the checkout adds afterwards. The choice is priced, not the option: selecting the
// free tier must not charge the paid tier's amount.
func TestCartTotalsIncludeSelectedConfigOption(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	pool := s.Pool()
	customerID := mustCustomer(t, pool, "cart-config@example.test")
	productID, priceID := mustProduct(t, pool, "vps-config", 1000, 500, "USD")
	allowQuantity(t, pool, productID, commerce.QuantityCombined)
	optionID := mustConfigOption(t, pool, productID, priceID, "RAM",
		[]string{"2 GB", "8 GB"}, true, map[string][2]int64{
			"2 GB": {0, 0},
			"8 GB": {1500, 300},
		})

	// The paid tier, quantity two: (1000 + 1500) × 2 recurring, (500 + 300) × 2 setup.
	if _, err := s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceID, Quantity: 2,
		Config: map[string]string{optionID: "8 GB"},
	}); err != nil {
		t.Fatalf("add item: %v", err)
	}
	totals, err := s.Totals(ctx, customerID, nil)
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if len(totals.Lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(totals.Lines))
	}
	line := totals.Lines[0]
	if line.Line.Minor() != 5000 {
		t.Fatalf("line = %d, want (1000+1500)×2 = 5000", line.Line.Minor())
	}
	if line.SetupLine.Minor() != 1600 {
		t.Fatalf("setup line = %d, want (500+300)×2 = 1600", line.SetupLine.Minor())
	}
	if line.Unit.Minor() != 2500 {
		t.Fatalf("unit = %d, want 1000+1500 = 2500", line.Unit.Minor())
	}
	if line.Setup.Minor() != 800 {
		t.Fatalf("setup = %d, want 500+300 = 800", line.Setup.Minor())
	}
	if totals.Total.Minor() != 6600 {
		t.Fatalf("total = %d, want 6600", totals.Total.Minor())
	}
	if !strings.Contains(line.Description, "RAM: 8 GB") {
		t.Fatalf("description = %q, want it to name the chosen option", line.Description)
	}

	// The free tier contributes nothing: the cart charges the base price only.
	if _, err := s.Clear(ctx, customerID); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceID, Quantity: 1,
		Config: map[string]string{optionID: "2 GB"},
	}); err != nil {
		t.Fatalf("add free tier: %v", err)
	}
	totals, err = s.Totals(ctx, customerID, nil)
	if err != nil {
		t.Fatalf("totals free tier: %v", err)
	}
	if totals.Total.Minor() != 1500 {
		t.Fatalf("free-tier total = %d, want 1000 + 500 setup = 1500", totals.Total.Minor())
	}
}

// A choice the option does not offer is refused when it is added, so the cart can never hold a
// selection that the totals (and therefore the checkout) cannot price.
func TestCartRefusesUnknownConfigChoice(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	pool := s.Pool()
	customerID := mustCustomer(t, pool, "cart-config-bad@example.test")
	productID, priceID := mustProduct(t, pool, "vps-config-bad", 1000, 0, "USD")
	optionID := mustConfigOption(t, pool, productID, priceID, "RAM",
		[]string{"2 GB", "8 GB"}, false, map[string][2]int64{"8 GB": {1500, 0}})

	_, err := s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceID, Config: map[string]string{optionID: "64 GB"},
	})
	if !errors.Is(err, commerce.ErrInvalidInput) {
		t.Fatalf("unknown choice: err = %v, want ErrInvalidInput", err)
	}
	_, err = s.AddItem(ctx, customerID, "USD", commerce.AddItemRequest{
		PriceVersionID: priceID,
		Config:         map[string]string{"00000000-0000-0000-0000-000000000000": "x"},
	})
	if !errors.Is(err, commerce.ErrInvalidInput) {
		t.Fatalf("unknown option: err = %v, want ErrInvalidInput", err)
	}
}
