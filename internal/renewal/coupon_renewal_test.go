package renewal_test

// A coupon is a promise about what an invoice costs, and a renewal is where the promise is either
// kept or quietly broken (CPN-06, CPN-08).
//
// This test exists because the discount code was correct, fully tested, and called by nothing. The
// checkout path applied a coupon to the first invoice; every renewal after it charged list price
// while the subscription still carried the coupon and its counter never moved. Nothing failed, no
// error was logged, and the invoice looked exactly like a correct one — which is the worst shape a
// billing bug can take, because the customer is the only person who can see it.
//
// So the assertion is on the money: a 25% coupon makes the next renewal's subtotal smaller by
// exactly a quarter, the discount appears as a line on the document rather than as a silently
// altered total, and the counter advances so a "first N cycles" coupon eventually stops.
//
// Each test builds ONE harness. Two harnesses in one test would share the pooled test database and
// each renewal would see both subscriptions, which is a fixture problem that looks like a product
// bug — the baseline is taken from the same subscription's previous period instead.

import (
	"context"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/calendar"
)

// seedCoupon stores a coupon on the subscription the way checkout does, with a zeroed counter.
// A cycles value of 0 means "every cycle" and is stored as NULL, which is the schema's spelling.
func (h *harness) seedCoupon(t *testing.T, subscriptionID, percentage string, cycles int) string {
	t.Helper()
	ctx := context.Background()
	var recurring any
	if cycles > 0 {
		recurring = cycles
	}
	var couponID string
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO coupons (code, kind, percentage, recurring_cycles, enabled)
		VALUES ($1, 'percentage', $2, $3, true) RETURNING id`,
		"promo-"+subscriptionID[:8], percentage, recurring).Scan(&couponID); err != nil {
		t.Fatalf("create coupon: %v", err)
	}
	if _, err := h.pool.Exec(ctx, `
		UPDATE subscriptions SET coupon_id = $2, coupon_cycles_applied = 0 WHERE id = $1`,
		subscriptionID, couponID); err != nil {
		t.Fatalf("attach coupon: %v", err)
	}
	return couponID
}

// cyclesApplied reads the subscription's counter.
func (h *harness) cyclesApplied(t *testing.T, subscriptionID string) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT coupon_cycles_applied FROM subscriptions WHERE id = $1`, subscriptionID).Scan(&n); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return n
}

// invoiceSubtotal reads an invoice's subtotal in minor units.
func (h *harness) invoiceSubtotal(t *testing.T, invoiceID string) int64 {
	t.Helper()
	var subtotal int64
	if err := h.pool.QueryRow(context.Background(),
		`SELECT subtotal_minor FROM invoices WHERE id = $1`, invoiceID).Scan(&subtotal); err != nil {
		t.Fatalf("read invoice subtotal: %v", err)
	}
	return subtotal
}

// discountLines counts the negative lines on an invoice, which is how a discount is recorded: as a
// line the customer can read, never as a quietly different unit price (migration 0022).
func (h *harness) discountLines(t *testing.T, invoiceID string) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM invoice_lines WHERE invoice_id = $1 AND unit_minor < 0`,
		invoiceID).Scan(&n); err != nil {
		t.Fatalf("count discount lines: %v", err)
	}
	return n
}

// renewPeriod runs the engine once and returns the single invoice it issued.
//
// `renewOne` builds a fresh subscription on every call, which is right for a test that wants a
// clean world and useless for a test that wants the SAME subscription across several periods. This
// runs the engine against whatever is already there, and refuses anything but one outcome so a
// second subscription cannot make the assertions compare the wrong invoices.
func (h *harness) renewPeriod(t *testing.T) string {
	t.Helper()
	results := h.run(t)
	if len(results) != 1 || results[0].Outcome != "invoiced" {
		t.Fatalf("renewal results = %+v, want one invoiced period", results)
	}
	return results[0].InvoiceID
}

// A 25% coupon discounts the next renewal by exactly a quarter, and the discount is visible on the
// document.
func TestCouponDiscountsTheRenewalInvoice(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	h := newHarness(t, now)
	subID := h.subscribe(t, now.AddDate(0, -1, 0), calendar.Monthly, 1, "UTC")

	// The baseline: this subscription's own first period, with no coupon attached.
	firstID := h.renewPeriod(t)
	baseline := h.invoiceSubtotal(t, firstID)
	if baseline <= 0 {
		t.Fatalf("the baseline invoice has a subtotal of %d, so a discount cannot be measured", baseline)
	}
	if n := h.discountLines(t, firstID); n != 0 {
		t.Fatalf("the baseline invoice already carries %d discount lines, so the test proves nothing", n)
	}

	// Attach the coupon, as a customer buying a promotion would have, and renew the next period.
	couponID := h.seedCoupon(t, subID, "25", 0)
	h.now = h.now.AddDate(0, 1, 0)
	secondID := h.renewPeriod(t)
	discounted := h.invoiceSubtotal(t, secondID)

	if discounted == baseline {
		t.Fatalf("the renewal charged the full %d even though the subscription carries coupon %s: "+
			"the customer paid for a discount and did not receive it", baseline, couponID)
	}
	// Exactly a quarter. A discount that is merely "smaller" would pass a laxer assertion while
	// being the wrong amount, and the wrong amount on a renewal is found by the customer.
	want := baseline - baseline/4
	if discounted != want {
		t.Fatalf("subtotal = %d with a 25%% coupon, want %d (a quarter off %d)", discounted, want, baseline)
	}

	if n := h.discountLines(t, secondID); n == 0 {
		t.Fatal("the renewal is cheaper but carries no discount line: nothing on the document explains why")
	}
	if got := h.cyclesApplied(t, subID); got != 1 {
		t.Fatalf("coupon_cycles_applied = %d after one discounted renewal, want 1", got)
	}
}

// A "first one cycle only" coupon discounts one renewal and then stops. Without this,
// `recurring_cycles` is a setting that is stored and never honoured, and a customer keeps a
// discount forever.
func TestCouponStopsAfterItsConfiguredCycles(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	h := newHarness(t, now)
	subID := h.subscribe(t, now.AddDate(0, -1, 0), calendar.Monthly, 1, "UTC")

	// List price first, then a coupon good for exactly one cycle, then two more renewals.
	listID := h.renewPeriod(t)
	list := h.invoiceSubtotal(t, listID)
	h.seedCoupon(t, subID, "25", 1)

	h.now = h.now.AddDate(0, 1, 0)
	discountedID := h.renewPeriod(t)
	discounted := h.invoiceSubtotal(t, discountedID)

	h.now = h.now.AddDate(0, 1, 0)
	afterID := h.renewPeriod(t)
	after := h.invoiceSubtotal(t, afterID)

	if discounted >= list {
		t.Fatalf("the discounted renewal is %d and list price is %d: the coupon never applied",
			discounted, list)
	}
	if after != list {
		t.Fatalf("the third renewal is %d, want list price %d: a one-cycle coupon did not stop",
			after, list)
	}
	// The counter stops at the coupon's limit rather than climbing past it: it is the value an
	// operator reads to explain a bill.
	if got := h.cyclesApplied(t, subID); got != 1 {
		t.Fatalf("coupon_cycles_applied = %d, want 1: the counter ran past the coupon's limit", got)
	}
}
