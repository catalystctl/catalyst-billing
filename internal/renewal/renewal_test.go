package renewal_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/calendar"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/renewal"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

var usd = money.MustLookupCurrency("USD")

// harnessSeq makes fixture identifiers unique within a test binary.
var harnessSeq atomic.Int64

type harness struct {
	pool       *store.Pool
	engine     *renewal.Engine
	customerID string
	productID  string
	priceID    string
	now        time.Time
}

func newHarness(t *testing.T, now time.Time) *harness {
	t.Helper()
	pool := storetest.DB(t)
	ctx := context.Background()
	h := &harness{pool: pool, now: now}

	pricer := billing.FixedTaxPricer{
		Rate:     money.MustParseRatio("0%"),
		Rounding: money.RoundHalfUp,
		Policy:   "none",
	}
	issuer := billing.NewIssuer(pricer)
	h.engine = renewal.NewEngine(pool, issuer,
		billing.Merchant{Name: "Catalyst Hosting Ltd", CountryCode: "GB"},
		"Renews automatically. Cancel any time before the renewal date.")
	h.engine.Now = func() time.Time { return h.now }

	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// A unique email per harness: one test may build several worlds, and the
		// schema enforces one account per address.
		email := fmt.Sprintf("a+%d@example.test", harnessSeq.Add(1))
		if err := tx.QueryRow(ctx,
			`INSERT INTO customers (email, name) VALUES ($1, 'A') RETURNING id`, email,
		).Scan(&h.customerID); err != nil {
			return err
		}
		slug := fmt.Sprintf("vps-%d", harnessSeq.Add(1))
		if err := tx.QueryRow(ctx,
			`INSERT INTO products (slug, name) VALUES ($1, 'VPS') RETURNING id`, slug,
		).Scan(&h.productID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO price_versions (product_id, version, currency, amount_minor, interval, anchor_day, anchor_timezone)
			VALUES ($1, 1, 'USD', 1200, 'monthly', 31, 'UTC') RETURNING id`, h.productID,
		).Scan(&h.priceID)
	})
	if err != nil {
		t.Fatalf("fixtures: %v", err)
	}
	return h
}

// subscribe creates an active subscription whose *current* period is the one
// starting in the month of `currentPeriodMonth`, using the given anchor. The
// next billing instant is therefore the end of that period.
func (h *harness) subscribe(t *testing.T, start time.Time, interval calendar.Interval, anchorDay int, timezone string) string {
	t.Helper()
	ctx := context.Background()
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		t.Fatalf("timezone: %v", err)
	}
	schedule, err := calendar.NewSchedule(interval, calendar.Anchor{Day: anchorDay}, loc)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	// The month is read in UTC: the fixture names a calendar month, and
	// converting a UTC instant into a western business timezone would silently
	// move it into the previous month.
	period := schedule.PeriodStartingIn(start.UTC().Year(), start.UTC().Month())

	var id string
	err = h.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE price_versions SET interval = $2, anchor_day = $3, anchor_timezone = $4 WHERE id = $1`,
			h.priceID, interval.String(), anchorDay, timezone); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO subscriptions (customer_id, price_version_id, status, interval,
				anchor_day, timezone, current_period_start, current_period_end, next_billing_at)
			VALUES ($1, $2, 'active', $3, $4, $5, $6, $7, $7)
			RETURNING id`,
			h.customerID, h.priceID, interval.String(), anchorDay, timezone,
			period.Start, period.End).Scan(&id)
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	return id
}

func (h *harness) run(t *testing.T) []renewal.Result {
	t.Helper()
	results, err := h.engine.Run(context.Background(), 100)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return results
}

// ------------------------------------------------------- config option fixtures --

// addOption adds a dropdown option to the harness product with one choice, and prices
// that choice at the harness price version — the shape a paid product option has.
func (h *harness) addOption(t *testing.T, name, choice string, amountMinor, setupMinor int64) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO config_options (product_id, name, option_type, choices, sort_order)
		VALUES ($1, $2, 'dropdown', jsonb_build_array($3::text),
		        (SELECT count(*) FROM config_options WHERE product_id = $1)::int)
		RETURNING id`, h.productID, name, choice).Scan(&id); err != nil {
		t.Fatalf("config option: %v", err)
	}
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO config_option_prices
			(config_option_id, price_version_id, choice, amount_minor, setup_minor)
		VALUES ($1, $2, $3, $4, $5)`,
		id, h.priceID, choice, amountMinor, setupMinor); err != nil {
		t.Fatalf("config option price: %v", err)
	}
	return id
}

// addFreeTextOption adds a free-form option, which by design has no price row: the customer
// types a value and nothing in the catalogue prices it.
func (h *harness) addFreeTextOption(t *testing.T, name string) string {
	t.Helper()
	var id string
	if err := h.pool.QueryRow(context.Background(), `
		INSERT INTO config_options (product_id, name, option_type, choices, sort_order)
		VALUES ($1, $2, 'text', '[]'::jsonb,
		        (SELECT count(*) FROM config_options WHERE product_id = $1)::int)
		RETURNING id`, h.productID, name).Scan(&id); err != nil {
		t.Fatalf("free text option: %v", err)
	}
	return id
}

// service creates the service row a subscription delivers, which is what carries the
// config-option selection.
func (h *harness) service(t *testing.T, subscriptionID string) string {
	t.Helper()
	var id string
	if err := h.pool.QueryRow(context.Background(), `
		INSERT INTO services (customer_id, subscription_id, product_id, status)
		VALUES ($1, $2, $3, 'active') RETURNING id`,
		h.customerID, subscriptionID, h.productID).Scan(&id); err != nil {
		t.Fatalf("service: %v", err)
	}
	return id
}

// choose stores a selection the way checkout does: the value plus the price it contributed
// at selection time.
func (h *harness) choose(t *testing.T, serviceID, optionID, value string, amountMinor, setupMinor int64) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(), `
		INSERT INTO service_configs (service_id, config_option_id, value, amount_minor, setup_minor)
		VALUES ($1, $2, $3, $4, $5)`,
		serviceID, optionID, value, amountMinor, setupMinor); err != nil {
		t.Fatalf("service config: %v", err)
	}
}

// invoiceLine is one persisted document line.
//
// Quantity is kept as the database renders it so a comparison is exact rather than
// approximate.
type invoiceLine struct {
	Position    int
	Description string
	Quantity    string
	UnitMinor   int64
	NetMinor    int64
	TaxMinor    int64
	TotalMinor  int64
}

// invoiceIDs lists the invoices in issuance order.
func (h *harness) invoiceIDs(t *testing.T) []string {
	t.Helper()
	rows, err := h.pool.Query(context.Background(),
		`SELECT id FROM invoices ORDER BY number NULLS LAST, issued_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// lines reads an invoice's lines in document order.
func (h *harness) lines(t *testing.T, invoiceID string) []invoiceLine {
	t.Helper()
	rows, err := h.pool.Query(context.Background(), `
		SELECT position, description, quantity::text, unit_minor, net_minor, tax_minor,
		       total_minor
		FROM invoice_lines WHERE invoice_id = $1 ORDER BY position`, invoiceID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []invoiceLine
	for rows.Next() {
		var l invoiceLine
		if err := rows.Scan(&l.Position, &l.Description, &l.Quantity, &l.UnitMinor,
			&l.NetMinor, &l.TaxMinor, &l.TotalMinor); err != nil {
			t.Fatal(err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// invoiceTotal reads the invoice's own total.
func (h *harness) invoiceTotal(t *testing.T, invoiceID string) int64 {
	t.Helper()
	var total int64
	if err := h.pool.QueryRow(context.Background(),
		`SELECT total_minor FROM invoices WHERE id = $1`, invoiceID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	return total
}

// onlyInvoice is the single invoice a one-renewal test produced, asserting there is exactly
// one.
func (h *harness) onlyInvoice(t *testing.T) string {
	t.Helper()
	ids := h.invoiceIDs(t)
	if len(ids) != 1 {
		t.Fatalf("%d invoices, want exactly 1", len(ids))
	}
	return ids[0]
}

// assertLines compares a document's line set exactly: a fix that adds a line, drops one,
// reorders one or changes an amount must fail here.
func assertLines(t *testing.T, got, want []invoiceLine) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("invoice has %d lines, want %d:\n got: %+v\nwant: %+v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func (h *harness) invoiceCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM invoices`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// -------------------------------------------------------------------- tests --

// A subscription is not billed before its lead time.
func TestNotDueYet(t *testing.T) {
	h := newHarness(t, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	// Current period is Feb 28 -> Mar 31, so the next billing date is Mar 31.
	// Now is Mar 1 and the lead time is 7 days, so it is not due yet.
	h.subscribe(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), calendar.Monthly, 31, "UTC")

	results := h.run(t)
	if len(results) != 0 {
		t.Fatalf("%d subscriptions scanned, want 0: %+v", len(results), results)
	}
	if n := h.invoiceCount(t); n != 0 {
		t.Fatalf("%d invoices created, want 0", n)
	}
}

func TestDueWithinLeadTime(t *testing.T) {
	// The next billing date is Mar 31; now is Mar 26, inside the 7-day lead time.
	h := newHarness(t, time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC))
	subID := h.subscribe(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), calendar.Monthly, 31, "UTC")

	results := h.run(t)
	if len(results) != 1 {
		t.Fatalf("%d results, want 1", len(results))
	}
	if results[0].Outcome != renewal.OutcomeInvoiced {
		t.Fatalf("outcome = %s (%s), want invoiced", results[0].Outcome, results[0].Detail)
	}
	if results[0].PeriodKey != "2026-03-31/2026-04-30" {
		t.Fatalf("period = %q, want 2026-03-31/2026-04-30", results[0].PeriodKey)
	}

	// The schedule advanced to the next period, keeping the month-end anchor.
	var next time.Time
	var version int64
	if err := h.pool.QueryRow(context.Background(),
		`SELECT next_billing_at, version FROM subscriptions WHERE id = $1`, subID,
	).Scan(&next, &version); err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next billing = %s, want %s", next, want)
	}
	if version != 2 {
		t.Fatalf("version = %d, want 2", version)
	}

	// The invoice carries the period it covers.
	var periodKey *string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT period_key FROM invoices`).Scan(&periodKey); err != nil {
		t.Fatal(err)
	}
	if periodKey == nil || *periodKey != "2026-03-31/2026-04-30" {
		t.Fatalf("invoice period_key = %v", periodKey)
	}
}

// Acceptance scenario 5: two workers generating the same renewal concurrently
// produce one invoice.
func TestConcurrentRenewalRuns(t *testing.T) {
	h := newHarness(t, time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC))
	h.subscribe(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), calendar.Monthly, 31, "UTC")

	// Two independent engines, as if two processes were running the scanner.
	issuer := billing.NewIssuer(billing.FixedTaxPricer{
		Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
	})
	mk := func() *renewal.Engine {
		e := renewal.NewEngine(h.pool, issuer,
			billing.Merchant{Name: "Catalyst Hosting Ltd", CountryCode: "GB"}, "terms")
		e.Now = func() time.Time { return h.now }
		return e
	}

	const workers = 6
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Errors are expected: the loser of the race either finds the
			// subscription already advanced or hits the unique index.
			_, _ = mk().Run(context.Background(), 100)
		}()
	}
	close(start)
	wg.Wait()

	if n := h.invoiceCount(t); n != 1 {
		t.Fatalf("%d invoices created by %d concurrent workers, want 1", n, workers)
	}
}

// Acceptance scenario 6: a cancelled subscription does not renew.
func TestCancelledSubscriptionDoesNotRenew(t *testing.T) {
	h := newHarness(t, time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC))
	subID := h.subscribe(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), calendar.Monthly, 31, "UTC")

	if _, err := h.pool.Exec(context.Background(), `
		UPDATE subscriptions SET cancel_at_period_end = true, cancelled_at = now() WHERE id = $1`,
		subID); err != nil {
		t.Fatal(err)
	}

	results := h.run(t)
	if len(results) != 1 || results[0].Outcome != renewal.OutcomeSkippedCancelled {
		t.Fatalf("results = %+v, want one skipped_cancelled", results)
	}
	if n := h.invoiceCount(t); n != 0 {
		t.Fatalf("%d invoices created for a cancelled subscription, want 0", n)
	}

	var status string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT status FROM subscriptions WHERE id = $1`, subID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "ended" {
		t.Fatalf("status = %s, want ended", status)
	}

	// A second run finds nothing to do: the subscription is no longer active.
	if results := h.run(t); len(results) != 0 {
		t.Fatalf("second run produced %+v, want nothing", results)
	}
}

// Acceptance scenario 6: missed scheduler runs recover after downtime, but an
// ancient backlog is routed to review instead of being billed automatically.
func TestCatchUpAfterDowntime(t *testing.T) {
	// The scheduler was down from mid-March until April 20, so the Mar 31
	// billing date was never processed.
	h := newHarness(t, time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC))
	h.subscribe(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), calendar.Monthly, 31, "UTC")

	results := h.run(t)
	if len(results) != 1 {
		t.Fatalf("%d results, want 1", len(results))
	}
	// The March 31 period is 20 days old, inside the 30-day limit, so it is
	// billed; the customer then owes for the period they are in.
	if results[0].Outcome != renewal.OutcomeInvoiced {
		t.Fatalf("outcome = %s (%s), want invoiced", results[0].Outcome, results[0].Detail)
	}

	// A backlog older than the limit goes to review rather than being charged.
	// The same database is reused, so the run sees every subscription that is
	// now far past its billing date; all of them must be held for review.
	h.now = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	before := h.invoiceCount(t)
	results = h.run(t)
	if len(results) == 0 {
		t.Fatal("no subscriptions scanned for an ancient backlog")
	}
	for _, r := range results {
		if r.Outcome != renewal.OutcomeNeedsReview {
			t.Fatalf("outcome %s (%s), want needs_review for every ancient backlog entry",
				r.Outcome, r.Detail)
		}
	}
	if after := h.invoiceCount(t); after != before {
		t.Fatalf("%d invoices created for an ancient backlog, want 0 new", after-before)
	}
}

// Month-end anchors must not drift after a renewal.
func TestMonthEndAnchorSurvivesRenewal(t *testing.T) {
	// Current period is Jan 31 -> Feb 28, so the next billing date is Feb 28.
	h := newHarness(t, time.Date(2026, 2, 24, 0, 0, 0, 0, time.UTC))
	subID := h.subscribe(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), calendar.Monthly, 31, "UTC")

	// Feb 28 period is due on Feb 24 with a 7-day lead.
	results := h.run(t)
	if len(results) != 1 || results[0].Outcome != renewal.OutcomeInvoiced {
		t.Fatalf("results = %+v", results)
	}
	if results[0].PeriodKey != "2026-02-28/2026-03-31" {
		t.Fatalf("period = %q, want 2026-02-28/2026-03-31", results[0].PeriodKey)
	}

	// Walk forward a few months; March is 31 days, April is 30, and the anchor
	// must keep producing the last day of each month.
	h.now = time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC)
	results = h.run(t)
	if len(results) != 1 || results[0].PeriodKey != "2026-03-31/2026-04-30" {
		t.Fatalf("march results = %+v, want 2026-03-31/2026-04-30", results)
	}

	h.now = time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	results = h.run(t)
	if len(results) != 1 || results[0].PeriodKey != "2026-04-30/2026-05-31" {
		t.Fatalf("april results = %+v, want 2026-04-30/2026-05-31", results)
	}

	var next time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT next_billing_at FROM subscriptions WHERE id = $1`, subID).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next billing = %s, want %s", next, want)
	}
}

// A leap-day annual subscription renews on Feb 29 in leap years and Feb 28
// otherwise, and the anchor is not rewritten.
func TestLeapDayAnnualRenewal(t *testing.T) {
	// Current period is 2027-02-28 -> 2028-02-29.
	h := newHarness(t, time.Date(2028, 2, 23, 0, 0, 0, 0, time.UTC))
	h.subscribe(t, time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC), calendar.Annual, 29, "UTC")

	results := h.run(t)
	if len(results) != 1 || results[0].Outcome != renewal.OutcomeInvoiced {
		t.Fatalf("results = %+v", results)
	}
	if results[0].PeriodKey != "2028-02-29/2029-02-28" {
		t.Fatalf("period = %q, want 2028-02-29/2029-02-28", results[0].PeriodKey)
	}

	// Next renewal is Feb 28 2029; the anchor day stays 29.
	var anchorDay int
	var next time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT anchor_day, next_billing_at FROM subscriptions`).Scan(&anchorDay, &next); err != nil {
		t.Fatal(err)
	}
	if anchorDay != 29 {
		t.Fatalf("anchor day = %d, want 29 (leap anchor must not be rewritten)", anchorDay)
	}
	if want := time.Date(2029, 2, 28, 0, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next billing = %s, want %s", next, want)
	}
}

// A business timezone in a DST transition must not shift the billing date.
func TestBusinessTimezoneRenewal(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// Anchor on the 8th at 00:00 New York time, which in March 2026 is the DST
	// transition day.
	h := newHarness(t, time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC))
	subID := h.subscribe(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), calendar.Monthly, 8, "America/New_York")

	results := h.run(t)
	if len(results) != 1 || results[0].Outcome != renewal.OutcomeInvoiced {
		t.Fatalf("results = %+v", results)
	}
	if results[0].PeriodKey != "2026-03-08/2026-04-08" {
		t.Fatalf("period = %q, want 2026-03-08/2026-04-08", results[0].PeriodKey)
	}

	// The stored instant is midnight New York on the 8th, which is 05:00 UTC
	// because the clocks had already sprung forward.
	var start time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT current_period_start FROM subscriptions WHERE id = $1`, subID).Scan(&start); err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Fatalf("period start = %s, want %s", start.UTC(), want)
	}
	if got := start.In(ny).Format("2006-01-02 15:04"); got != "2026-03-08 00:00" {
		t.Fatalf("business wall clock = %s, want 2026-03-08 00:00", got)
	}
}

// The renewal invoice uses the price snapshot of the subscription's price
// version, not the current catalog.
func TestRenewalUsesSubscriptionPrice(t *testing.T) {
	h := newHarness(t, time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC))
	h.subscribe(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), calendar.Monthly, 31, "UTC")

	ctx := context.Background()
	// Publish a new, cheaper price version. The subscription still points at v1.
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, interval, anchor_day, anchor_timezone)
		VALUES ($1, 2, 'USD', 100, 'monthly', 31, 'UTC')`, h.productID); err != nil {
		t.Fatal(err)
	}

	results := h.run(t)
	if len(results) != 1 || results[0].Outcome != renewal.OutcomeInvoiced {
		t.Fatalf("results = %+v", results)
	}
	var total int64
	if err := h.pool.QueryRow(ctx, `SELECT total_minor FROM invoices`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 1200 {
		t.Fatalf("total = %d, want 1200 (the subscribed price, not the new catalog price)", total)
	}
}

// A renewal run records its own audit trail.
func TestRenewalRunIsRecorded(t *testing.T) {
	h := newHarness(t, time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC))
	h.subscribe(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), calendar.Monthly, 31, "UTC")
	h.run(t)

	ctx := context.Background()
	var scanned, invoiced, skipped, review int
	var finished *time.Time
	if err := h.pool.QueryRow(ctx, `
		SELECT scanned, invoiced, skipped, needs_review, finished_at FROM renewal_runs`).
		Scan(&scanned, &invoiced, &skipped, &review, &finished); err != nil {
		t.Fatal(err)
	}
	if scanned != 1 || invoiced != 1 || skipped != 0 || review != 0 {
		t.Fatalf("run counters = scanned %d invoiced %d skipped %d review %d, want 1/1/0/0",
			scanned, invoiced, skipped, review)
	}
	if finished == nil {
		t.Fatal("run was not marked finished")
	}

	var outcomes int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM renewal_events`).Scan(&outcomes); err != nil {
		t.Fatal(err)
	}
	if outcomes != 1 {
		t.Fatalf("%d renewal events, want 1", outcomes)
	}
}

// A catalog edit that changes the anchor day must not wedge the subscription: the
// subscription owns its schedule, so billing proceeds and the divergence is
// reported for review rather than failing every run forever.
func TestAnchorDivergenceDoesNotWedgeRenewal(t *testing.T) {
	h := newHarness(t, time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC))
	h.subscribe(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), calendar.Monthly, 31, "UTC")

	// The price version now claims a different anchor day.
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE price_versions SET anchor_day = 15`); err != nil {
		t.Fatal(err)
	}

	results, err := h.engine.Run(context.Background(), 100)
	if err != nil {
		t.Fatalf("run must not fail on anchor divergence: %v", err)
	}
	if len(results) != 1 || results[0].Outcome != renewal.OutcomeNeedsReview {
		t.Fatalf("results = %+v, want one needs_review", results)
	}
	// No invoice was issued on a schedule the customer never agreed to.
	if n := h.invoiceCount(t); n != 0 {
		t.Fatalf("%d invoices issued despite the divergence, want 0", n)
	}
	// The subscription is untouched, so it is still visible for a human to fix.
	var version int64
	if err := h.pool.QueryRow(context.Background(),
		`SELECT version FROM subscriptions`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("subscription version = %d, want 1 (unchanged)", version)
	}
}

// A subscription whose price version is not a schedule is a data defect: nothing may renew a
// one-time or free price. Billing it would charge the customer for the same one-time purchase
// every period, for ever, so the engine ends the schedule and records why — instead of failing
// on the same row every run, or (worse) invoicing it.
func TestNonRecurringPriceIsNeverRenewed(t *testing.T) {
	h := newHarness(t, time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC))
	subID := h.subscribe(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), calendar.Monthly, 31, "UTC")

	// Hand-write the defect: the price version behind an active subscription becomes one-time.
	// The interval CHECK on `subscriptions` deliberately does not allow this value, so the only
	// way to reach it is through the price the subscription points at — which is exactly the
	// shape a catalog bug would produce.
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE price_versions SET interval = 'one_time'`); err != nil {
		t.Fatal(err)
	}

	results, err := h.engine.Run(context.Background(), 100)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(results) != 1 || results[0].Outcome != renewal.OutcomeSkippedNonRecurring {
		t.Fatalf("results = %+v, want one skipped_non_recurring", results)
	}
	if n := h.invoiceCount(t); n != 0 {
		t.Fatalf("%d invoices issued for a one-time price, want 0", n)
	}

	// The schedule is ended, so the scanner does not visit it again and the customer's portal
	// stops claiming a next-bill date that will never happen.
	var status string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT status FROM subscriptions WHERE id = $1`, subID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "ended" {
		t.Fatalf("subscription status = %q, want ended", status)
	}
	// A second run has nothing left to do.
	again, err := h.engine.Run(context.Background(), 100)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second run scanned %d subscriptions, want 0", len(again))
	}
}

// A free price never renews either, and a one-time purchase never has a subscription for the
// engine to find in the first place. The second half of that is asserted where the purchase is
// made (internal/orders); this is the price-side guard.
func TestFreePriceIsNeverRenewed(t *testing.T) {
	h := newHarness(t, time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC))
	h.subscribe(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), calendar.Monthly, 31, "UTC")

	if _, err := h.pool.Exec(context.Background(),
		`UPDATE price_versions SET interval = 'free', amount_minor = 0`); err != nil {
		t.Fatal(err)
	}
	results, err := h.engine.Run(context.Background(), 100)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(results) != 1 || results[0].Outcome != renewal.OutcomeSkippedNonRecurring {
		t.Fatalf("results = %+v, want one skipped_non_recurring", results)
	}
	if n := h.invoiceCount(t); n != 0 {
		t.Fatalf("%d invoices issued for a free price, want 0", n)
	}
}

// ------------------------------------------------------------- surcharge tests --

// baseLine is the renewal line every subscription produces for the harness product: the
// price version's 1200 minor, quantity 1. It is written out in full because the base line
// must not move when surcharges are added.
func baseLine() invoiceLine {
	return invoiceLine{
		Position:    1,
		Description: "VPS",
		Quantity:    "1.000000",
		UnitMinor:   1200,
		NetMinor:    1200,
		TaxMinor:    0,
		TotalMinor:  1200,
	}
}

// The leak: a subscription whose service has a paid config option must renew at base+option,
// not at base. Before the fix the second period onward dropped the option while the
// provisioned resource kept running.
func TestRenewalBillsPaidConfigOption(t *testing.T) {
	h := newHarness(t, time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC))
	subID := h.subscribe(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), calendar.Monthly, 31, "UTC")
	svc := h.service(t, subID)
	ram := h.addOption(t, "RAM", "32 GB", 1000, 0)
	h.choose(t, svc, ram, "32 GB", 1000, 0)

	results := h.run(t)
	if len(results) != 1 || results[0].Outcome != renewal.OutcomeInvoiced {
		t.Fatalf("results = %+v, want one invoiced", results)
	}

	id := h.onlyInvoice(t)
	if total := h.invoiceTotal(t, id); total != 2200 {
		t.Fatalf("invoice total = %d, want 2200 (1200 base + 1000 RAM)", total)
	}
	assertLines(t, h.lines(t, id), []invoiceLine{
		baseLine(),
		{
			Position:    2,
			Description: "RAM: 32 GB",
			Quantity:    "1.000000",
			UnitMinor:   1000,
			NetMinor:    1000,
			TaxMinor:    0,
			TotalMinor:  1000,
		},
	})
}

// Two paid options each get their own line and sum into the total.
func TestRenewalSumsMultipleConfigOptions(t *testing.T) {
	h := newHarness(t, time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC))
	subID := h.subscribe(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), calendar.Monthly, 31, "UTC")
	svc := h.service(t, subID)
	ram := h.addOption(t, "RAM", "32 GB", 1000, 0)
	backup := h.addOption(t, "Backup", "daily", 500, 0)
	h.choose(t, svc, ram, "32 GB", 1000, 0)
	h.choose(t, svc, backup, "daily", 500, 0)

	results := h.run(t)
	if len(results) != 1 || results[0].Outcome != renewal.OutcomeInvoiced {
		t.Fatalf("results = %+v, want one invoiced", results)
	}

	id := h.onlyInvoice(t)
	if total := h.invoiceTotal(t, id); total != 2700 {
		t.Fatalf("invoice total = %d, want 2700 (1200 + 1000 + 500)", total)
	}
	assertLines(t, h.lines(t, id), []invoiceLine{
		baseLine(),
		{
			Position: 2, Description: "RAM: 32 GB", Quantity: "1.000000",
			UnitMinor: 1000, NetMinor: 1000, TotalMinor: 1000,
		},
		{
			Position: 3, Description: "Backup: daily", Quantity: "1.000000",
			UnitMinor: 500, NetMinor: 500, TotalMinor: 500,
		},
	})
}

// An option priced at zero contributes no line at all. A zero-amount line is noise on a
// document the customer keeps, and it would make every free checkbox visible on the invoice.
func TestZeroPricedOptionProducesNoLine(t *testing.T) {
	h := newHarness(t, time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC))
	subID := h.subscribe(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), calendar.Monthly, 31, "UTC")
	svc := h.service(t, subID)
	location := h.addOption(t, "Location", "EU", 0, 0)
	h.choose(t, svc, location, "EU", 0, 0)

	results := h.run(t)
	if len(results) != 1 || results[0].Outcome != renewal.OutcomeInvoiced {
		t.Fatalf("results = %+v, want one invoiced", results)
	}

	id := h.onlyInvoice(t)
	if total := h.invoiceTotal(t, id); total != 1200 {
		t.Fatalf("invoice total = %d, want 1200", total)
	}
	// Exactly the base line: no zero-amount surcharge line was written.
	assertLines(t, h.lines(t, id), []invoiceLine{baseLine()})
}

// Setup is charged once, when the service is created. It must not recur on any renewal.
func TestSurchargeSetupDoesNotRecur(t *testing.T) {
	h := newHarness(t, time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC))
	subID := h.subscribe(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), calendar.Monthly, 31, "UTC")
	svc := h.service(t, subID)
	// 1000 recurring plus a 500 one-off setup.
	ram := h.addOption(t, "RAM", "32 GB", 1000, 500)
	h.choose(t, svc, ram, "32 GB", 1000, 500)

	if results := h.run(t); len(results) != 1 || results[0].Outcome != renewal.OutcomeInvoiced {
		t.Fatalf("first renewal: %+v", results)
	}
	// Walk into the next period and renew again: the second renewal is the one that used to
	// repeat a setup fee when it repeated anything.
	h.now = time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	if results := h.run(t); len(results) != 1 || results[0].Outcome != renewal.OutcomeInvoiced {
		t.Fatalf("second renewal: %+v", results)
	}

	ids := h.invoiceIDs(t)
	if len(ids) != 2 {
		t.Fatalf("%d invoices, want 2", len(ids))
	}
	want := []invoiceLine{
		baseLine(),
		{
			Position: 2, Description: "RAM: 32 GB", Quantity: "1.000000",
			UnitMinor: 1000, NetMinor: 1000, TotalMinor: 1000,
		},
	}
	for i, id := range ids {
		if total := h.invoiceTotal(t, id); total != 2200 {
			t.Fatalf("invoice %d total = %d, want 2200 (setup must not recur)", i+1, total)
		}
		assertLines(t, h.lines(t, id), want)
	}
}

// A subscription with no config options renews exactly as it did before the fix: one base
// line, nothing else. This is the common case, and it must not regress.
func TestNoConfigOptionsRenewsAsBefore(t *testing.T) {
	h := newHarness(t, time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC))
	subID := h.subscribe(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), calendar.Monthly, 31, "UTC")
	h.service(t, subID) // a service exists; it simply has no options chosen

	results := h.run(t)
	if len(results) != 1 || results[0].Outcome != renewal.OutcomeInvoiced {
		t.Fatalf("results = %+v, want one invoiced", results)
	}

	id := h.onlyInvoice(t)
	if total := h.invoiceTotal(t, id); total != 1200 {
		t.Fatalf("invoice total = %d, want 1200", total)
	}
	assertLines(t, h.lines(t, id), []invoiceLine{baseLine()})
}

// The invoice total is the sum of its lines, and the ledger posting still balances when a
// surcharge changes that total.
func TestSurchargeTotalAndPostingBalance(t *testing.T) {
	h := newHarness(t, time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC))
	subID := h.subscribe(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), calendar.Monthly, 31, "UTC")
	svc := h.service(t, subID)
	ram := h.addOption(t, "RAM", "32 GB", 1000, 0)
	h.choose(t, svc, ram, "32 GB", 1000, 0)

	if results := h.run(t); len(results) != 1 || results[0].Outcome != renewal.OutcomeInvoiced {
		t.Fatalf("results = %+v", results)
	}

	ctx := context.Background()
	id := h.onlyInvoice(t)
	total := h.invoiceTotal(t, id)

	var lineSum int64
	for _, l := range h.lines(t, id) {
		lineSum += l.TotalMinor
	}
	if lineSum != total {
		t.Fatalf("lines sum to %d, invoice total is %d", lineSum, total)
	}

	var debit, credit int64
	if err := h.pool.QueryRow(ctx, `
		SELECT COALESCE(sum(debit_minor), 0), COALESCE(sum(credit_minor), 0)
		FROM journal_entries`).Scan(&debit, &credit); err != nil {
		t.Fatal(err)
	}
	if debit != credit {
		t.Fatalf("posting does not balance: debits %d, credits %d", debit, credit)
	}
	if debit != total {
		t.Fatalf("posting is %d, invoice total is %d", debit, total)
	}

	// The receivable carries the whole invoice, surcharge included, so dunning and payment
	// allocation see the amount the customer was actually billed.
	var receivable int64
	if err := h.pool.QueryRow(ctx,
		`SELECT COALESCE(sum(debit_minor), 0) FROM journal_entries WHERE account_code = 'ar'`,
	).Scan(&receivable); err != nil {
		t.Fatal(err)
	}
	if receivable != total {
		t.Fatalf("accounts receivable = %d, want %d", receivable, total)
	}

	// Every individual posting balances, not merely the sum of all of them.
	var unbalanced int
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT transaction_id FROM journal_entries
			GROUP BY transaction_id
			HAVING sum(debit_minor) <> sum(credit_minor)
		) bad`).Scan(&unbalanced); err != nil {
		t.Fatal(err)
	}
	if unbalanced != 0 {
		t.Fatalf("%d unbalanced journal transactions", unbalanced)
	}
}

// A selection that was sold as paid, whose price row the catalogue has since dropped, is
// refused as needs_review rather than renewed at zero. Renewing at zero would give the
// resource away — the same leak this fix closes — and renewing at the recorded amount would
// price the period from two different sources.
func TestUnpricedPaidOptionNeedsReview(t *testing.T) {
	h := newHarness(t, time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC))
	subID := h.subscribe(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), calendar.Monthly, 31, "UTC")
	svc := h.service(t, subID)
	ram := h.addOption(t, "RAM", "32 GB", 1000, 0)
	h.choose(t, svc, ram, "32 GB", 1000, 0)

	// The catalogue stops pricing a choice this service is on.
	if _, err := h.pool.Exec(context.Background(),
		`DELETE FROM config_option_prices WHERE config_option_id = $1`, ram); err != nil {
		t.Fatal(err)
	}

	results, err := h.engine.Run(context.Background(), 100)
	if err != nil {
		t.Fatalf("run must not fail on an unpriced option: %v", err)
	}
	if len(results) != 1 || results[0].Outcome != renewal.OutcomeNeedsReview {
		t.Fatalf("results = %+v, want one needs_review", results)
	}
	if n := h.invoiceCount(t); n != 0 {
		t.Fatalf("%d invoices issued for an unpriced paid option, want 0", n)
	}

	// Nothing was billed, so nothing may have moved: the schedule is where it was and a human
	// can still fix the catalogue or the service.
	var version int64
	var next time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT version, next_billing_at FROM subscriptions WHERE id = $1`, subID,
	).Scan(&version, &next); err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("subscription version = %d, want 1 (unchanged)", version)
	}
	if want := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next billing = %s, want %s", next, want)
	}

	// The reason names the option, so the operator does not have to guess which one.
	if detail := results[0].Detail; !strings.Contains(detail, "RAM: 32 GB") {
		t.Fatalf("detail = %q, want it to name the unpriced option", detail)
	}
}

// The other half of that decision: a free-form option has no price row by design and must not
// block the renewal. Treating every missing row as a fault would wedge every service with a
// text or number option.
func TestUnpricedFreeOptionStillRenews(t *testing.T) {
	h := newHarness(t, time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC))
	subID := h.subscribe(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), calendar.Monthly, 31, "UTC")
	svc := h.service(t, subID)
	hostname := h.addFreeTextOption(t, "Hostname")
	h.choose(t, svc, hostname, "web-01", 0, 0)

	results := h.run(t)
	if len(results) != 1 || results[0].Outcome != renewal.OutcomeInvoiced {
		t.Fatalf("results = %+v, want one invoiced", results)
	}
	id := h.onlyInvoice(t)
	assertLines(t, h.lines(t, id), []invoiceLine{baseLine()})
}

// The option is priced from the subscription's own price version, the same one the base line
// comes from, so a later catalog version cannot re-price an option underneath a live base
// price (or the reverse).
func TestSurchargeUsesSubscriptionPriceVersion(t *testing.T) {
	h := newHarness(t, time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC))
	subID := h.subscribe(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), calendar.Monthly, 31, "UTC")
	svc := h.service(t, subID)
	ram := h.addOption(t, "RAM", "32 GB", 1000, 0)
	h.choose(t, svc, ram, "32 GB", 1000, 0)

	// A new price version prices the same option at 5000. The subscription stays on v1.
	ctx := context.Background()
	var v2 string
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO price_versions
			(product_id, version, currency, amount_minor, interval, anchor_day, anchor_timezone)
		VALUES ($1, 2, 'USD', 1200, 'monthly', 31, 'UTC') RETURNING id`, h.productID,
	).Scan(&v2); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO config_option_prices
			(config_option_id, price_version_id, choice, amount_minor)
		VALUES ($1, $2, '32 GB', 5000)`, ram, v2); err != nil {
		t.Fatal(err)
	}

	if results := h.run(t); len(results) != 1 || results[0].Outcome != renewal.OutcomeInvoiced {
		t.Fatalf("results = %+v", results)
	}
	id := h.onlyInvoice(t)
	if total := h.invoiceTotal(t, id); total != 2200 {
		t.Fatalf("invoice total = %d, want 2200 (the subscribed version's option price, not 5000)", total)
	}
}
