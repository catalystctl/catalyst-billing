package coupon_test

// The renewal half of a coupon, against a real database and a real draft invoice.
//
// These tests exist because the previous behaviour was silently wrong rather than loudly broken: a
// "first three cycles" coupon discounted the first invoice and then stopped, and nothing failed. So
// each test here asserts the *money on the draft document*, not just that a decision was returned:
// removing the discount line, the tax re-derivation, the redemption or the counter breaks one.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/coupon"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

var (
	usd = money.MustLookupCurrency("USD")
	t0  = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
)

type env struct {
	pool     *store.Pool
	customer string
	priceID  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := storetest.DB(t)
	ctx := context.Background()
	e := &env{pool: pool}
	if err := pool.QueryRow(ctx,
		`INSERT INTO customers (email) VALUES ('renewal-coupon@example.test') RETURNING id`).
		Scan(&e.customer); err != nil {
		t.Fatalf("create customer: %v", err)
	}
	var productID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO products (slug, name) VALUES ('renewal-vps', 'Renewal VPS') RETURNING id`).
		Scan(&productID); err != nil {
		t.Fatalf("create product: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, setup_minor,
			interval, anchor_day, anchor_timezone, name)
		VALUES ($1, 1, 'USD', 10000, 0, 'monthly', 1, 'UTC', 'Monthly')
		RETURNING id`, productID).Scan(&e.priceID); err != nil {
		t.Fatalf("create price: %v", err)
	}
	return e
}

// coupon inserts a promotion and returns its id.
func (e *env) coupon(t *testing.T, code, kind, percentage string, amount *int64,
	appliesTo string, recurring *int, maxUses *int) string {
	t.Helper()
	var id string
	var pct, cur *string
	if percentage != "" {
		pct = &percentage
	}
	if kind == "fixed" {
		currency := "USD"
		cur = &currency
	}
	if err := e.pool.QueryRow(context.Background(), `
		INSERT INTO coupons (code, kind, percentage, amount_minor, currency, applies_to,
			recurring_cycles, max_uses, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true)
		RETURNING id`,
		code, kind, pct, amount, cur, appliesTo, recurring, maxUses).Scan(&id); err != nil {
		t.Fatalf("create coupon: %v", err)
	}
	return id
}

// subscription creates a subscription carrying a coupon and returns its id.
func (e *env) subscription(t *testing.T, couponID string, cyclesApplied int) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `
		INSERT INTO subscriptions (customer_id, price_version_id, status, interval, anchor_day,
			timezone, current_period_start, current_period_end, next_billing_at,
			coupon_id, coupon_cycles_applied)
		VALUES ($1, $2, 'active', 'monthly', 1, 'UTC', $3, $4, $5, NULLIF($6, '')::uuid, $7)
		RETURNING id`,
		e.customer, e.priceID, t0.AddDate(0, -1, 0), t0, t0.AddDate(0, 1, 0), couponID, cyclesApplied).
		Scan(&id); err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	return id
}

func TestRenewalDiscountAppliesForTheFirstNCycles(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	recurring := 2
	couponID := e.coupon(t, "TEN", "percentage", "10", nil, "all", &recurring, nil)
	subID := e.subscription(t, couponID, 1) // checkout already used cycle 1

	decision, err := coupon.RenewalDiscount(ctx, e.pool, subID, 10000, usd, t0)
	if err != nil {
		t.Fatalf("RenewalDiscount: %v", err)
	}
	if !decision.Applies {
		t.Fatalf("the coupon did not apply on cycle 2 of 2: %+v", decision)
	}
	if decision.Cycle != 2 {
		t.Fatalf("cycle = %d, want 2", decision.Cycle)
	}
	// 10% of 10000, exactly, as minor units.
	if decision.AmountMinor != 1000 {
		t.Fatalf("discount = %d, want 1000", decision.AmountMinor)
	}

	// The third renewal is past the coupon's window.
	if _, err := e.pool.Exec(ctx,
		`UPDATE subscriptions SET coupon_cycles_applied = 2 WHERE id = $1`, subID); err != nil {
		t.Fatal(err)
	}
	decision, err = coupon.RenewalDiscount(ctx, e.pool, subID, 10000, usd, t0)
	if err != nil {
		t.Fatalf("RenewalDiscount: %v", err)
	}
	if decision.Applies {
		t.Fatalf("the coupon applied after its last cycle: %+v", decision)
	}
	if decision.Reason == "" {
		t.Fatal("a coupon that does not apply must say why")
	}
}

func TestRenewalDiscountLifetimeNeverExhausts(t *testing.T) {
	e := newEnv(t)
	couponID := e.coupon(t, "FOREVER", "percentage", "12.5", nil, "all", nil, nil)
	subID := e.subscription(t, couponID, 40)

	decision, err := coupon.RenewalDiscount(context.Background(), e.pool, subID, 8000, usd, t0)
	if err != nil {
		t.Fatalf("RenewalDiscount: %v", err)
	}
	if !decision.Applies || decision.Cycle != 41 {
		t.Fatalf("decision = %+v, want an applying lifetime coupon on cycle 41", decision)
	}
	// 12.5% of 8000 = 1000, computed as an exact rational, not a float.
	if decision.AmountMinor != 1000 {
		t.Fatalf("discount = %d, want 1000", decision.AmountMinor)
	}
}

func TestRenewalDiscountSkipsCouponsThatCannotApplyToARenewal(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	amount := int64(1500)

	setupOnly := e.coupon(t, "SETUP", "percentage", "10", nil, "setup", nil, nil)
	subID := e.subscription(t, setupOnly, 1)
	decision, err := coupon.RenewalDiscount(ctx, e.pool, subID, 10000, usd, t0)
	if err != nil {
		t.Fatalf("RenewalDiscount: %v", err)
	}
	if decision.Applies {
		t.Fatal("a setup-only coupon must not discount a renewal: there is no setup fee on one")
	}

	foreign := e.coupon(t, "EUROS", "fixed", "", &amount, "all", nil, nil)
	if _, err := e.pool.Exec(ctx, `UPDATE coupons SET currency = 'EUR' WHERE id = $1`, foreign); err != nil {
		t.Fatal(err)
	}
	subID = e.subscription(t, foreign, 1)
	decision, err = coupon.RenewalDiscount(ctx, e.pool, subID, 10000, usd, t0)
	if err != nil {
		t.Fatalf("RenewalDiscount: %v", err)
	}
	if decision.Applies {
		t.Fatal("a USD invoice must not be discounted by a coupon issued in another currency")
	}
	if decision.Reason == "" {
		t.Fatal("the reason must explain the currency mismatch")
	}

	// A use cap is a promise about total uses, and a renewal is a use.
	limit := 1
	capped := e.coupon(t, "ONCE", "percentage", "10", nil, "all", nil, &limit)
	subID = e.subscription(t, capped, 1)
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO coupon_redemptions (coupon_id, customer_id, discount_minor, currency)
		VALUES ($1, $2, 1000, 'USD')`, capped, e.customer); err != nil {
		t.Fatalf("insert redemption: %v", err)
	}
	decision, err = coupon.RenewalDiscount(ctx, e.pool, subID, 10000, usd, t0)
	if err != nil {
		t.Fatalf("RenewalDiscount: %v", err)
	}
	if decision.Applies {
		t.Fatal("a coupon that has reached its use cap must not discount another renewal")
	}
}

func TestRenewalDiscountIsEmptyWithoutACoupon(t *testing.T) {
	e := newEnv(t)
	subID := e.subscription(t, "", 0)
	decision, err := coupon.RenewalDiscount(context.Background(), e.pool, subID, 10000, usd, t0)
	if err != nil {
		t.Fatalf("RenewalDiscount: %v", err)
	}
	if decision.Applies || decision.CouponID != "" {
		t.Fatalf("a subscription without a coupon produced a discount: %+v", decision)
	}
}

// The applied discount is asserted on the document: the discount line, the re-derived tax, the
// totals, the snapshot, the redemption and the counter.
func TestApplyRenewalDiscountWritesTheDocument(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	recurring := 3
	couponID := e.coupon(t, "TEN", "percentage", "10", nil, "all", &recurring, nil)
	subID := e.subscription(t, couponID, 1)

	decision, err := coupon.RenewalDiscount(ctx, e.pool, subID, 10000, usd, t0)
	if err != nil {
		t.Fatalf("RenewalDiscount: %v", err)
	}
	if !decision.Applies {
		t.Fatalf("decision = %+v", decision)
	}

	// 10% of 10000 = 1000 discount; 9000 net at 19% = 1710 tax; total 10710.
	var applied coupon.Applied
	var invoiceID string
	err = e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		issuer := billing.NewIssuer(billing.FixedTaxPricer{
			Rate: money.MustParseRatio("0.19"), Policy: "test-vat", Jurisdiction: "DE/standard",
		})
		id, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID:     e.customer,
			SubscriptionID: subID,
			PeriodKey:      "2026-03",
			Lines: []billing.Line{{
				Description:   "Renewal VPS",
				QuantityMilli: 1000,
				UnitPrice:     money.New(10000, usd),
			}},
			Merchant: billing.Merchant{Name: "Catalyst Hosting", CountryCode: "DE"},
			Customer: billing.CustomerDetails{ID: e.customer},
			Terms:    "Monthly, paid in advance",
			DueAt:    t0.AddDate(0, 0, 7),
		})
		if err != nil {
			return err
		}
		invoiceID = id
		applied, err = coupon.ApplyRenewalDiscount(ctx, tx, coupon.ApplyRequest{
			InvoiceID:      id,
			SubscriptionID: subID,
			CustomerID:     e.customer,
			Discount:       decision,
			Currency:       usd,
		})
		return err
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applied.DiscountMinor != 1000 || applied.Subtotal.Minor() != 9000 ||
		applied.Tax.Minor() != 1710 || applied.Total.Minor() != 10710 {
		t.Fatalf("applied = %+v, want 1000 off, 9000 net, 1710 tax, 10710 total", applied)
	}

	// The invoice row agrees with the result.
	var subtotal, tax, total int64
	var snapJSON []byte
	if err := e.pool.QueryRow(ctx,
		`SELECT subtotal_minor, tax_minor, total_minor, snapshot FROM invoices WHERE id = $1`,
		invoiceID).Scan(&subtotal, &tax, &total, &snapJSON); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	if subtotal != 9000 || tax != 1710 || total != 10710 {
		t.Fatalf("invoice = %d/%d/%d, want 9000/1710/10710", subtotal, tax, total)
	}

	// The discount is a line of its own, untaxed, and the charge line's net is still the catalog
	// price: the customer can compare it to the price list.
	var (
		lines       int
		discountNet int64
		discountTax int64
		chargeNet   int64
		chargeTax   int64
		chargeRate  string
	)
	rows, err := e.pool.Query(ctx, `
		SELECT line_kind, net_minor, tax_minor, tax_rate FROM invoice_lines
		 WHERE invoice_id = $1 ORDER BY position`, invoiceID)
	if err != nil {
		t.Fatalf("read lines: %v", err)
	}
	for rows.Next() {
		var kind, rate string
		var net, lineTax int64
		if err := rows.Scan(&kind, &net, &lineTax, &rate); err != nil {
			t.Fatal(err)
		}
		lines++
		if kind == "discount" {
			discountNet, discountTax = net, lineTax
			continue
		}
		chargeNet, chargeTax, chargeRate = net, lineTax, rate
	}
	rows.Close()
	if lines != 2 {
		t.Fatalf("lines = %d, want the charge and one discount line", lines)
	}
	if discountNet != -1000 || discountTax != 0 {
		t.Fatalf("discount line = net %d tax %d, want -1000 and 0", discountNet, discountTax)
	}
	if chargeNet != 10000 {
		t.Fatalf("charge line net = %d, want the catalog 10000", chargeNet)
	}
	if chargeTax != 1710 {
		t.Fatalf("charge line tax = %d, want 1710 (tax on the discounted net)", chargeTax)
	}
	if chargeRate != "19/100" && chargeRate != "0.19" {
		t.Fatalf("charge line rate = %q, want the exact rational", chargeRate)
	}

	// The snapshot says which coupon, which cycle and how much — the facts an audit asks for.
	var snap struct {
		Discount struct {
			CouponID    string `json:"coupon_id"`
			Code        string `json:"code"`
			AmountMinor int64  `json:"amount_minor"`
			Cycle       int    `json:"cycle"`
			CyclesTotal int    `json:"cycles_total"`
		} `json:"discount"`
		Lines []struct {
			Description string `json:"Description"`
			Net         struct {
				Minor int64 `json:"minor"`
			} `json:"Net"`
		} `json:"lines"`
	}
	if err := json.Unmarshal(snapJSON, &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snap.Discount.CouponID != couponID || snap.Discount.Code != "TEN" ||
		snap.Discount.AmountMinor != 1000 || snap.Discount.Cycle != 2 || snap.Discount.CyclesTotal != 3 {
		t.Fatalf("snapshot discount = %+v", snap.Discount)
	}
	if len(snap.Lines) != 2 {
		t.Fatalf("snapshot lines = %d, want the charge and the discount", len(snap.Lines))
	}
	// The snapshot's last line is the discount, negative, so a reprint reproduces the document.
	if last := snap.Lines[len(snap.Lines)-1]; last.Net.Minor != -1000 {
		t.Fatalf("snapshot discount line net = %d, want -1000", last.Net.Minor)
	}

	// The redemption records the use against the invoice, and the counter advanced.
	var redemptions int
	if err := e.pool.QueryRow(ctx, `
		SELECT count(*) FROM coupon_redemptions WHERE coupon_id = $1 AND invoice_id = $2`,
		couponID, invoiceID).Scan(&redemptions); err != nil {
		t.Fatal(err)
	}
	if redemptions != 1 {
		t.Fatalf("redemptions = %d, want 1", redemptions)
	}
	var cycles int
	if err := e.pool.QueryRow(ctx,
		`SELECT coupon_cycles_applied FROM subscriptions WHERE id = $1`, subID).Scan(&cycles); err != nil {
		t.Fatal(err)
	}
	if cycles != 2 {
		t.Fatalf("coupon_cycles_applied = %d, want 2", cycles)
	}
}

// An inclusive invoice discounts the gross: the total the customer pays falls by exactly the
// discount, and the tax is reduced by its share rather than by the rate on the discount.
func TestApplyRenewalDiscountHandlesInclusiveTax(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	couponID := e.coupon(t, "TEN", "percentage", "10", nil, "all", nil, nil)
	subID := e.subscription(t, couponID, 1)

	decision, err := coupon.RenewalDiscount(ctx, e.pool, subID, 10000, usd, t0)
	if err != nil {
		t.Fatalf("RenewalDiscount: %v", err)
	}
	var applied coupon.Applied
	err = e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		issuer := billing.NewIssuer(billing.FixedTaxPricer{
			Rate: money.MustParseRatio("0.19"), Inclusive: true, Policy: "test-vat",
		})
		id, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID:     e.customer,
			SubscriptionID: subID,
			Lines: []billing.Line{{
				Description:   "Renewal VPS",
				QuantityMilli: 1000,
				UnitPrice:     money.New(10000, usd),
			}},
			Merchant: billing.Merchant{Name: "Catalyst Hosting", CountryCode: "DE"},
			Customer: billing.CustomerDetails{ID: e.customer},
			DueAt:    t0.AddDate(0, 0, 7),
		})
		if err != nil {
			return err
		}
		applied, err = coupon.ApplyRenewalDiscount(ctx, tx, coupon.ApplyRequest{
			InvoiceID: id, SubscriptionID: subID, CustomerID: e.customer,
			Discount: decision, Currency: usd,
		})
		return err
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applied.Total.Minor() != 9000 {
		t.Fatalf("total = %d, want 9000: the quoted price contains the tax, so it falls by the discount exactly",
			applied.Total.Minor())
	}
	if applied.Tax.Minor() != 1437 {
		t.Fatalf("tax = %d, want 1437 (the tax contained in 9000 inclusive at 19%%)", applied.Tax.Minor())
	}
	// subtotal + tax is the total, always.
	if applied.Subtotal.Minor()+applied.Tax.Minor() != applied.Total.Minor() {
		t.Fatalf("subtotal %d + tax %d != total %d",
			applied.Subtotal.Minor(), applied.Tax.Minor(), applied.Total.Minor())
	}
}

// An issued invoice is evidence. A coupon that arrives after issuance is a credit note, not an edit.
func TestApplyRenewalDiscountRefusesAnIssuedInvoice(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	couponID := e.coupon(t, "TEN", "percentage", "10", nil, "all", nil, nil)
	subID := e.subscription(t, couponID, 1)
	decision, err := coupon.RenewalDiscount(ctx, e.pool, subID, 10000, usd, t0)
	if err != nil {
		t.Fatalf("RenewalDiscount: %v", err)
	}

	err = e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		issuer := billing.NewIssuer(billing.FixedTaxPricer{
			Rate: money.MustParseRatio("0.19"), Policy: "test-vat",
		})
		id, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: e.customer,
			Lines: []billing.Line{{
				Description: "Renewal VPS", QuantityMilli: 1000, UnitPrice: money.New(10000, usd),
			}},
			Merchant: billing.Merchant{Name: "Catalyst Hosting", CountryCode: "DE"},
			Customer: billing.CustomerDetails{ID: e.customer},
			DueAt:    t0.AddDate(0, 0, 7),
		})
		if err != nil {
			return err
		}
		if _, err := issuer.Issue(ctx, tx, id, billing.DefaultSeries); err != nil {
			return err
		}
		_, err = coupon.ApplyRenewalDiscount(ctx, tx, coupon.ApplyRequest{
			InvoiceID: id, SubscriptionID: subID, CustomerID: e.customer,
			Discount: decision, Currency: usd,
		})
		return err
	})
	if !errors.Is(err, coupon.ErrInvoiceNotDraft) {
		t.Fatalf("err = %v, want ErrInvoiceNotDraft", err)
	}
}

// A rate that is not an exact rational is a bug in the pricer, and silently treating it as zero
// would drop tax from a document.
func TestApplyRenewalDiscountRefusesAnUnreadableRate(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	couponID := e.coupon(t, "TEN", "percentage", "10", nil, "all", nil, nil)
	subID := e.subscription(t, couponID, 1)
	decision, err := coupon.RenewalDiscount(ctx, e.pool, subID, 10000, usd, t0)
	if err != nil {
		t.Fatalf("RenewalDiscount: %v", err)
	}

	err = e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		issuer := billing.NewIssuer(billing.FixedTaxPricer{
			Rate: money.MustParseRatio("0.19"), Policy: "test-vat",
		})
		id, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: e.customer,
			Lines: []billing.Line{{
				Description: "Renewal VPS", QuantityMilli: 1000, UnitPrice: money.New(10000, usd),
			}},
			Merchant: billing.Merchant{Name: "Catalyst Hosting", CountryCode: "DE"},
			Customer: billing.CustomerDetails{ID: e.customer},
			DueAt:    t0.AddDate(0, 0, 7),
		})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE invoice_lines SET tax_rate = 'not-a-number' WHERE invoice_id = $1`, id); err != nil {
			return err
		}
		_, err = coupon.ApplyRenewalDiscount(ctx, tx, coupon.ApplyRequest{
			InvoiceID: id, SubscriptionID: subID, CustomerID: e.customer,
			Discount: decision, Currency: usd,
		})
		return err
	})
	if err == nil {
		t.Fatal("an unreadable tax rate was silently treated as zero")
	}
}

// The percentage is an exact rational: 12.5% of 8000 is 1000, not 999 or 1001.
func TestRenewalDiscountPercentageIsExact(t *testing.T) {
	e := newEnv(t)
	couponID := e.coupon(t, "HALF", "percentage", "12.5", nil, "all", nil, nil)
	subID := e.subscription(t, couponID, 0)
	decision, err := coupon.RenewalDiscount(context.Background(), e.pool, subID, 8000, usd, t0)
	if err != nil {
		t.Fatalf("RenewalDiscount: %v", err)
	}
	if decision.AmountMinor != 1000 {
		t.Fatalf("discount = %d, want 1000", decision.AmountMinor)
	}
	// The same arithmetic the money package uses, checked against a rational: nothing in this path
	// touched a float.
	expected := new(big.Rat).Mul(big.NewRat(8000, 1), big.NewRat(125, 1000))
	if expected.Num().Int64()/expected.Denom().Int64() != decision.AmountMinor {
		t.Fatalf("discount = %d does not match 12.5%% of 8000", decision.AmountMinor)
	}
}

// A subscription with no coupon is unchanged by any of this: the decision is empty and Apply
// refuses rather than writing a zero line.
func TestApplyRenewalDiscountRefusesAnEmptyDecision(t *testing.T) {
	e := newEnv(t)
	subID := e.subscription(t, "", 0)
	err := e.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := coupon.ApplyRenewalDiscount(ctx, tx, coupon.ApplyRequest{
			InvoiceID: "", SubscriptionID: subID, CustomerID: e.customer,
			Discount: coupon.Discount{}, Currency: usd,
		})
		return err
	})
	if !errors.Is(err, coupon.ErrNoDiscount) {
		t.Fatalf("err = %v, want ErrNoDiscount", err)
	}
}

var _ = fmt.Sprintf

// The screen's projection: applied and remaining cycles on every subscription carrying a coupon.
// A lifetime coupon has no remainder, which is not the same as a remainder of zero.
func TestSubscriptionsCarryingReportsRemainingCycles(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	recurring := 3
	couponID := e.coupon(t, "THREE", "percentage", "10", nil, "all", &recurring, nil)
	subID := e.subscription(t, couponID, 1)

	subs, err := coupon.SubscriptionsCarrying(ctx, e.pool, couponID)
	if err != nil {
		t.Fatalf("SubscriptionsCarrying: %v", err)
	}
	if len(subs) != 1 || subs[0].SubscriptionID != subID {
		t.Fatalf("subscriptions = %+v, want the one carrying the coupon", subs)
	}
	if subs[0].CyclesApplied != 1 {
		t.Fatalf("applied = %d, want 1", subs[0].CyclesApplied)
	}
	if subs[0].Remaining == nil || *subs[0].Remaining != 2 {
		t.Fatalf("remaining = %v, want 2", subs[0].Remaining)
	}

	// Past the window the remainder is zero, not negative: "no cycles left" is the answer.
	if _, err := e.pool.Exec(ctx,
		`UPDATE subscriptions SET coupon_cycles_applied = 5 WHERE id = $1`, subID); err != nil {
		t.Fatal(err)
	}
	subs, err = coupon.SubscriptionsCarrying(ctx, e.pool, couponID)
	if err != nil {
		t.Fatalf("SubscriptionsCarrying: %v", err)
	}
	if subs[0].Remaining == nil || *subs[0].Remaining != 0 {
		t.Fatalf("remaining past the window = %v, want 0", subs[0].Remaining)
	}

	// A lifetime coupon discounts every cycle, so it has no remainder at all.
	lifetime := e.coupon(t, "LIFE", "percentage", "5", nil, "all", nil, nil)
	e.subscription(t, lifetime, 12)
	subs, err = coupon.SubscriptionsCarrying(ctx, e.pool, lifetime)
	if err != nil {
		t.Fatalf("SubscriptionsCarrying: %v", err)
	}
	if len(subs) != 1 || subs[0].Remaining != nil {
		t.Fatalf("lifetime coupon remainder = %+v, want nil (every cycle)", subs)
	}
}

// A second application to the same invoice is refused: the redemption's unique index makes the
// replay visible, and discounting twice would under-collect by the discount and advance the cycle
// counter twice.
func TestApplyRenewalDiscountRefusesAReplay(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	couponID := e.coupon(t, "TEN", "percentage", "10", nil, "all", nil, nil)
	subID := e.subscription(t, couponID, 1)

	decision, err := coupon.RenewalDiscount(ctx, e.pool, subID, 10000, usd, t0)
	if err != nil {
		t.Fatalf("RenewalDiscount: %v", err)
	}

	apply := func() error {
		return e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			issuer := billing.NewIssuer(billing.FixedTaxPricer{
				Rate: money.MustParseRatio("0.19"), Policy: "test-vat",
			})
			id, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
				CustomerID: e.customer,
				Lines: []billing.Line{{
					Description: "Renewal VPS", QuantityMilli: 1000, UnitPrice: money.New(10000, usd),
				}},
				Merchant: billing.Merchant{Name: "Catalyst Hosting", CountryCode: "DE"},
				Customer: billing.CustomerDetails{ID: e.customer},
				DueAt:    t0.AddDate(0, 0, 7),
			})
			if err != nil {
				return err
			}
			_, err = coupon.ApplyRenewalDiscount(ctx, tx, coupon.ApplyRequest{
				InvoiceID: id, SubscriptionID: subID, CustomerID: e.customer,
				Discount: decision, Currency: usd,
			})
			return err
		})
	}
	if err := apply(); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	// The second application targets the *same* invoice, which is what a replay looks like.
	var invoiceID string
	if err := e.pool.QueryRow(ctx, `SELECT id FROM invoices LIMIT 1`).Scan(&invoiceID); err != nil {
		t.Fatal(err)
	}
	err = e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := coupon.ApplyRenewalDiscount(ctx, tx, coupon.ApplyRequest{
			InvoiceID: invoiceID, SubscriptionID: subID, CustomerID: e.customer,
			Discount: decision, Currency: usd,
		})
		return err
	})
	if !errors.Is(err, coupon.ErrAlreadyApplied) {
		t.Fatalf("err = %v, want ErrAlreadyApplied", err)
	}

	// The first application's counter stands at one increment, not two.
	var cycles int
	if err := e.pool.QueryRow(ctx,
		`SELECT coupon_cycles_applied FROM subscriptions WHERE id = $1`, subID).Scan(&cycles); err != nil {
		t.Fatal(err)
	}
	if cycles != 2 {
		t.Fatalf("coupon_cycles_applied = %d, want 2 (checkout's 1 plus one renewal)", cycles)
	}
}
