package renewal_test

import (
	"context"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/calendar"
	"github.com/catalyst/billing/internal/renewal"
)

// The renewal-engine half of the proof that nothing existing moved.
//
// The calendar package is compared against a frozen copy of its own pre-change algorithm in
// internal/calendar/unchanged_test.go. This file asserts the same property one layer up, where
// money is actually charged: a subscription on each existing interval issues its invoice on the
// exact date it always did, and a price that does not recur is still never renewed.

// TestCalendarRenewalsAreUnchanged pins the exact period key of an issued renewal for monthly,
// quarterly and annual, including a month-end anchor across the leap February of 2028 and an
// annual anchor on Feb 29. A period key is the invoice's uniqueness key and the dates printed on
// the document, so a silent shift here would re-bill or skip a period for every subscription.
func TestCalendarRenewalsAreUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name       string
		now        time.Time
		currentMon time.Time // the month whose period the customer has already paid for
		interval   calendar.Interval
		anchorDay  int
		wantKey    string
		wantNext   time.Time
	}{
		{
			// A monthly anchor on the 31st, whose current period is the clamped February of
			// the leap year: 2028-01-31 -> 2028-02-29. The renewal is the Feb 29 -> Mar 31
			// period, and the anchor re-expands in March.
			name:       "monthly anchor 31 across the leap February",
			now:        time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC),
			currentMon: time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC),
			interval:   calendar.Monthly,
			anchorDay:  31,
			wantKey:    "2028-02-29/2028-03-31",
			wantNext:   time.Date(2028, 3, 31, 0, 0, 0, 0, time.UTC),
		},
		{
			// The same anchor in a common year clamps to the 28th.
			name:       "monthly anchor 31 across a common February",
			now:        time.Date(2027, 2, 28, 0, 0, 0, 0, time.UTC),
			currentMon: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
			interval:   calendar.Monthly,
			anchorDay:  31,
			wantKey:    "2027-02-28/2027-03-31",
			wantNext:   time.Date(2027, 3, 31, 0, 0, 0, 0, time.UTC),
		},
		{
			// A monthly anchor on the 31st ending in a 30-day month.
			name:       "monthly anchor 31 into April",
			now:        time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC),
			currentMon: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
			interval:   calendar.Monthly,
			anchorDay:  31,
			wantKey:    "2026-03-31/2026-04-30",
			wantNext:   time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC),
		},
		{
			name:       "quarterly",
			now:        time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
			currentMon: time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC),
			interval:   calendar.Quarterly,
			anchorDay:  15,
			wantKey:    "2026-01-15/2026-04-15",
			wantNext:   time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC),
		},
		{
			name:       "semiannual",
			now:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			currentMon: time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC),
			interval:   calendar.SemiAnnual,
			anchorDay:  1,
			wantKey:    "2026-01-01/2026-07-01",
			wantNext:   time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			// An annual anchor on Feb 29: the leap year renews to the next Feb 29.
			name:       "annual anchor Feb 29",
			now:        time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC),
			currentMon: time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC),
			interval:   calendar.Annual,
			anchorDay:  29,
			wantKey:    "2028-02-29/2029-02-28",
			wantNext:   time.Date(2029, 2, 28, 0, 0, 0, 0, time.UTC),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.now)
			subID := h.subscribe(t, tc.currentMon, tc.interval, tc.anchorDay, "UTC")

			results := h.run(t)
			if len(results) != 1 {
				t.Fatalf("%d results, want 1: %+v", len(results), results)
			}
			if results[0].Outcome != renewal.OutcomeInvoiced {
				t.Fatalf("outcome = %s (%s), want invoiced", results[0].Outcome, results[0].Detail)
			}
			if got := results[0].PeriodKey; got != tc.wantKey {
				t.Fatalf("period key = %q, want %q", got, tc.wantKey)
			}

			key, start, end, _ := h.invoicePeriod(t, results[0].InvoiceID)
			if key != tc.wantKey {
				t.Fatalf("invoice period_key = %q, want %q", key, tc.wantKey)
			}
			if want := tc.wantNext; !end.Equal(want) {
				t.Fatalf("invoice period end = %s, want %s", end, want)
			}
			if !start.Before(end) {
				t.Fatalf("invoice period %s..%s is empty", start, end)
			}

			_, _, next := h.schedule(t, subID)
			if !next.Equal(tc.wantNext) {
				t.Fatalf("next billing at = %s, want %s", next, tc.wantNext)
			}
		})
	}
}

// TestDayCountRenewalOnThe2028LeapDay: a day-count renewal that lands on 2028-02-29 issues the
// invoice for the boundary after it, with the exact dates.
func TestDayCountRenewalOnThe2028LeapDay(t *testing.T) {
	// The current period starts 2028-01-15, so the boundary being billed is 2028-02-29 (45
	// days later, counting the leap day) and the period billed runs to 2028-04-14.
	h := newHarness(t, time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC))
	subID := h.subscribeEveryDays(t, time.Date(2028, 1, 15, 0, 0, 0, 0, time.UTC), 45, 15, "UTC")

	results := h.run(t)
	if len(results) != 1 || results[0].Outcome != renewal.OutcomeInvoiced {
		t.Fatalf("run: %+v", results)
	}
	if got, want := results[0].PeriodKey, "2028-02-29/2028-04-14"; got != want {
		t.Fatalf("period key = %q, want %q", got, want)
	}
	_, start, end, _ := h.invoicePeriod(t, results[0].InvoiceID)
	if want := time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Fatalf("invoice period start = %s, want %s", start, want)
	}
	if want := time.Date(2028, 4, 14, 0, 0, 0, 0, time.UTC); !end.Equal(want) {
		t.Fatalf("invoice period end = %s, want %s", end, want)
	}
	_, _, next := h.schedule(t, subID)
	if want := time.Date(2028, 4, 14, 0, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next billing at = %s, want %s", next, want)
	}
}

// TestNonRecurringPricesAreNeverRenewed is the `one_time` / `free` half of "unchanged". Neither
// is a schedule: a one-time purchase creates a service with no subscription at all, and a
// subscription that somehow points at such a price is ended rather than billed. That behaviour
// predates this change and is asserted here so a future interval edit cannot quietly start
// charging a customer for the same one-time purchase every period.
func TestNonRecurringPricesAreNeverRenewed(t *testing.T) {
	for _, interval := range []string{"one_time", "free"} {
		t.Run(interval, func(t *testing.T) {
			h := newHarness(t, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
			ctx := context.Background()

			// A price that does not recur, and a subscription pointing at it — the data
			// defect the engine has to survive. A free price must carry no amount (the
			// database refuses one), so the amount is zeroed with it.
			if _, err := h.pool.Exec(ctx,
				`UPDATE price_versions SET interval = $2, amount_minor = CASE WHEN $2 = 'free' THEN 0 ELSE amount_minor END WHERE id = $1`,
				h.priceID, interval); err != nil {
				t.Fatalf("set price interval: %v", err)
			}
			var subID string
			if err := h.pool.QueryRow(ctx, `
				INSERT INTO subscriptions (customer_id, price_version_id, status, interval, anchor_day,
					timezone, current_period_start, current_period_end, next_billing_at)
				VALUES ($1, $2, 'active', 'monthly', 1, 'UTC', $3, $4, $4)
				RETURNING id`,
				h.customerID, h.priceID,
				time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
				time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)).Scan(&subID); err != nil {
				t.Fatalf("subscription: %v", err)
			}

			results := h.run(t)
			if len(results) != 1 {
				t.Fatalf("%d results, want 1: %+v", len(results), results)
			}
			if results[0].Outcome != renewal.OutcomeSkippedNonRecurring {
				t.Fatalf("outcome = %s (%s), want skipped_non_recurring", results[0].Outcome, results[0].Detail)
			}
			if n := h.invoiceCount(t); n != 0 {
				t.Fatalf("%d invoices created for a non-recurring price, want 0", n)
			}

			// The schedule is ended rather than left to fail on every future run.
			var status string
			if err := h.pool.QueryRow(ctx, `SELECT status FROM subscriptions WHERE id = $1`, subID).
				Scan(&status); err != nil {
				t.Fatalf("read subscription: %v", err)
			}
			if status != "ended" {
				t.Fatalf("subscription status = %q, want ended", status)
			}
		})
	}
}
