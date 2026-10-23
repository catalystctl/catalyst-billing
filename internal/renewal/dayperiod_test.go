package renewal_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/calendar"
	"github.com/catalyst/billing/internal/renewal"
	"github.com/jackc/pgx/v5"
)

// subscribeEveryDays creates an active subscription on a day-count price: every period is
// exactly days civil days, and currentStart is the start of the period the customer has
// already paid for. next_billing_at is therefore the boundary after it, which is the period
// the engine bills on the next run.
func (h *harness) subscribeEveryDays(t *testing.T, currentStart time.Time, days, anchorDay int, timezone string) string {
	t.Helper()
	ctx := context.Background()
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		t.Fatalf("timezone: %v", err)
	}
	intervalName := calendar.DayInterval(days)
	schedule, err := calendar.ScheduleFor(intervalName, calendar.Anchor{Day: anchorDay}, currentStart, loc)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	period := schedule.PeriodAt(currentStart)

	var id string
	err = h.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE price_versions SET interval = $2, anchor_day = $3, anchor_timezone = $4 WHERE id = $1`,
			h.priceID, intervalName, anchorDay, timezone); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO subscriptions (customer_id, price_version_id, status, interval,
				anchor_day, timezone, current_period_start, current_period_end, next_billing_at)
			VALUES ($1, $2, 'active', $3, $4, $5, $6, $7, $7)
			RETURNING id`,
			h.customerID, h.priceID, intervalName, anchorDay, timezone,
			period.Start, period.End).Scan(&id)
	})
	if err != nil {
		t.Fatalf("subscribe every %d days: %v", days, err)
	}
	return id
}

// schedule reads back the subscription's persisted boundaries.
func (h *harness) schedule(t *testing.T, subscriptionID string) (start, end, next time.Time) {
	t.Helper()
	if err := h.pool.QueryRow(context.Background(), `
		SELECT current_period_start, current_period_end, next_billing_at
		FROM subscriptions WHERE id = $1`, subscriptionID).
		Scan(&start, &end, &next); err != nil {
		t.Fatalf("read schedule: %v", err)
	}
	return start, end, next
}

// invoicePeriod reads the period an invoice recorded, from the column the uniqueness
// constraint uses and from the snapshot the document was issued with, plus the date the
// document fell due.
func (h *harness) invoicePeriod(t *testing.T, invoiceID string) (key string, start, end, dueAt time.Time) {
	t.Helper()
	var snapshot []byte
	if err := h.pool.QueryRow(context.Background(), `
		SELECT COALESCE(period_key, ''), due_at, snapshot FROM invoices WHERE id = $1`, invoiceID).
		Scan(&key, &dueAt, &snapshot); err != nil {
		t.Fatalf("read invoice period: %v", err)
	}
	var decoded struct {
		Period *struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"period"`
	}
	if err := json.Unmarshal(snapshot, &decoded); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if decoded.Period == nil {
		t.Fatal("the invoice snapshot records no period")
	}
	var err error
	if start, err = time.Parse(time.RFC3339, decoded.Period.Start); err != nil {
		t.Fatalf("snapshot period start %q: %v", decoded.Period.Start, err)
	}
	if end, err = time.Parse(time.RFC3339, decoded.Period.End); err != nil {
		t.Fatalf("snapshot period end %q: %v", decoded.Period.End, err)
	}
	return key, start, end, dueAt
}

// A renewal on a day-count period issues the invoice on the boundary date, for a period of
// exactly N civil days, and the period that spans a leap day is one day longer than the same
// period in a common year.
func TestDayPeriodRenewalAcrossLeapDay(t *testing.T) {
	// The subscription's next boundary is Feb 1, so the period billed is Feb 1 -> Mar 17:
	// 45 days later, counting the leap day. In a common year the same period ends Mar 18.
	h := newHarness(t, time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC))
	subID := h.subscribeEveryDays(t, time.Date(2023, 12, 18, 0, 0, 0, 0, time.UTC), 45, 1, "UTC")

	results := h.run(t)
	if len(results) != 1 {
		t.Fatalf("%d results, want 1: %+v", len(results), results)
	}
	if results[0].Outcome != renewal.OutcomeInvoiced {
		t.Fatalf("outcome = %s (%s), want invoiced", results[0].Outcome, results[0].Detail)
	}
	if got, want := results[0].PeriodKey, "2024-02-01/2024-03-17"; got != want {
		t.Fatalf("period key = %q, want %q", got, want)
	}

	invoiceID := h.onlyInvoice(t)
	key, start, end, dueAt := h.invoicePeriod(t, invoiceID)
	if want := "2024-02-01/2024-03-17"; key != want {
		t.Fatalf("invoice period_key = %q, want %q", key, want)
	}
	if want := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Fatalf("invoice period start = %s, want %s", start, want)
	}
	if want := time.Date(2024, 3, 17, 0, 0, 0, 0, time.UTC); !end.Equal(want) {
		t.Fatalf("invoice period end = %s, want %s", end, want)
	}
	// The invoice is dated by the run instant the engine was given (plus the policy's
	// payment terms), which is the leap-year boundary, not by whatever the clock said.
	if want := h.now.AddDate(0, 0, 7); !dueAt.Equal(want) {
		t.Fatalf("invoice due at = %s, want %s", dueAt, want)
	}

	// The schedule advanced to the boundary the invoice ended on.
	_, _, next := h.schedule(t, subID)
	if want := time.Date(2024, 3, 17, 0, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next billing at = %s, want %s", next, want)
	}

	// The next run bills the following 45-day period, again landing on a computed date
	// rather than on a remembered one.
	h.now = time.Date(2024, 3, 17, 0, 0, 0, 0, time.UTC)
	results = h.run(t)
	if len(results) != 1 || results[0].Outcome != renewal.OutcomeInvoiced {
		t.Fatalf("second run: %+v", results)
	}
	if got, want := results[0].PeriodKey, "2024-03-17/2024-05-01"; got != want {
		t.Fatalf("second period key = %q, want %q", got, want)
	}
}

// The same 45-day period in a common year is one day shorter, which is the proof the
// arithmetic counts real days instead of assuming a February length.
func TestDayPeriodRenewalInACommonYear(t *testing.T) {
	h := newHarness(t, time.Date(2023, 2, 1, 0, 0, 0, 0, time.UTC))
	h.subscribeEveryDays(t, time.Date(2022, 12, 18, 0, 0, 0, 0, time.UTC), 45, 1, "UTC")

	results := h.run(t)
	if len(results) != 1 || results[0].Outcome != renewal.OutcomeInvoiced {
		t.Fatalf("run: %+v", results)
	}
	if got, want := results[0].PeriodKey, "2023-02-01/2023-03-18"; got != want {
		t.Fatalf("period key = %q, want %q", got, want)
	}
}

// A day-count subscription in a DST timezone renews at the same wall-clock time before and
// after the transition, and its invoice records the period the customer was told about.
func TestDayPeriodRenewalAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("timezone: %v", err)
	}
	// The period billed is Apr 15 -> May 30 00:00 Berlin; both boundaries are in summer
	// time, and the period before it contained the spring transition.
	h := newHarness(t, time.Date(2024, 4, 15, 0, 0, 0, 0, loc))
	subID := h.subscribeEveryDays(t, time.Date(2024, 3, 1, 0, 0, 0, 0, loc), 45, 1, "Europe/Berlin")

	results := h.run(t)
	if len(results) != 1 || results[0].Outcome != renewal.OutcomeInvoiced {
		t.Fatalf("run: %+v", results)
	}
	if got, want := results[0].PeriodKey, "2024-04-15/2024-05-30"; got != want {
		t.Fatalf("period key = %q, want %q", got, want)
	}

	_, _, next := h.schedule(t, subID)
	if want := time.Date(2024, 5, 30, 0, 0, 0, 0, loc); !next.Equal(want) {
		t.Fatalf("next billing at = %s, want %s (00:00 Berlin, not an hour off)", next, want)
	}
}

// A day-count subscription whose price no longer matches is not billed silently: the anchor
// mismatch check still applies, because a day count does not exempt a subscription from the
// rules that protect a live billing date.
func TestDayPeriodAnchorMismatchNeedsReview(t *testing.T) {
	h := newHarness(t, time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC))
	h.subscribeEveryDays(t, time.Date(2023, 12, 18, 0, 0, 0, 0, time.UTC), 45, 1, "UTC")

	// The catalogue's anchor day is edited; the subscription keeps its own.
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE price_versions SET anchor_day = 5 WHERE id = $1`, h.priceID); err != nil {
		t.Fatalf("edit price anchor: %v", err)
	}

	results := h.run(t)
	if len(results) != 1 {
		t.Fatalf("%d results, want 1", len(results))
	}
	if results[0].Outcome != renewal.OutcomeNeedsReview {
		t.Fatalf("outcome = %s (%s), want needs_review", results[0].Outcome, results[0].Detail)
	}
	if n := h.invoiceCount(t); n != 0 {
		t.Fatalf("%d invoices created, want 0", n)
	}
}
