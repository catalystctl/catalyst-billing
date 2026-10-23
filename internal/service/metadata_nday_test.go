package service_test

// The end-to-end derived description for a day-count plan, which is the test that could not be
// written anywhere else: it needs the migration that lets the database store `every_45_days`, so
// it lives on the branch that carries 0059 rather than in a metadata-only change set.

import (
	"context"
	"strings"
	"testing"
)

// TestDerivedDescriptionNamesADayCountPeriod: a service sold "every 45 days" says so in the
// sentence a support agent reads. Without this the description would read "VPS Small · Small"
// with no billing cycle at all — the same class of defect as a blank service, and the reason the
// calendar-period branch exists next to it.
func TestDerivedDescriptionNamesADayCountPeriod(t *testing.T) {
	f := newFixture(t, 4500, "every_45_days")
	ctx := context.Background()

	// The fixture writes the interval straight into both tables, which is the assertion that
	// the migration's CHECK accepts it on the price *and* on the subscription.
	var priceInterval, subInterval string
	if err := f.pool.QueryRow(ctx, `
		SELECT pv.interval, sub.interval
		FROM subscriptions sub JOIN price_versions pv ON pv.id = sub.price_version_id
		WHERE sub.id = $1`, f.subID).Scan(&priceInterval, &subInterval); err != nil {
		t.Fatalf("read intervals: %v", err)
	}
	if priceInterval != "every_45_days" || subInterval != "every_45_days" {
		t.Fatalf("stored intervals = %q / %q, want every_45_days on both", priceInterval, subInterval)
	}

	m, err := f.store.ServiceMetadata(ctx, f.serviceID)
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if !strings.HasSuffix(m.DerivedDescription, "billed every 45 days") {
		t.Fatalf("derived description = %q, want it to end with the 45-day cycle", m.DerivedDescription)
	}
	// The rest of the sentence is unchanged: the product, then the plan.
	if !strings.HasPrefix(m.DerivedDescription, "VPS Small · Small") {
		t.Fatalf("derived description = %q, want the product and the plan first", m.DerivedDescription)
	}
	// The interval travels with the answer, so a client renders the cycle from the row rather
	// than parsing the sentence.
	if m.Interval != "every_45_days" {
		t.Fatalf("interval = %q, want every_45_days", m.Interval)
	}
}

// TestDerivedDescriptionForRecurringIntervals runs every recurring kind through the same path,
// so an edit to intervalPhrase cannot satisfy the day-count case and break a calendar one.
func TestDerivedDescriptionForRecurringIntervals(t *testing.T) {
	for _, tc := range []struct {
		interval string
		want     string
	}{
		{"monthly", "billed monthly"},
		{"quarterly", "billed quarterly"},
		{"semiannual", "billed every six months"},
		{"annual", "billed annually"},
		{"every_1_days", "billed every 1 days"},
		{"every_366_days", "billed every 366 days"},
	} {
		t.Run(tc.interval, func(t *testing.T) {
			f := newFixture(t, 1200, tc.interval)
			m, err := f.store.ServiceMetadata(context.Background(), f.serviceID)
			if err != nil {
				t.Fatalf("metadata: %v", err)
			}
			if !strings.HasSuffix(m.DerivedDescription, tc.want) {
				t.Fatalf("derived description = %q, want it to end with %q", m.DerivedDescription, tc.want)
			}
		})
	}
}

// TestNonRecurringServiceHasNoCyclePhrase: a one-time or free purchase creates a service with
// **no** subscription (0028, decision 1), so there is no cycle to name. The assertion is on the
// real shape rather than on a hand-written interval string, because that is what the read path
// actually sees: the subscription is absent, so the interval is NULL.
//
// The fixture is built with a recurring interval and then rewritten, because the database
// correctly refuses `one_time` or `free` on a *subscription* row — which is itself the guarantee
// under test, one layer down.
func TestNonRecurringServiceHasNoCyclePhrase(t *testing.T) {
	for _, interval := range []string{"one_time", "free"} {
		t.Run(interval, func(t *testing.T) {
			f := newFixture(t, 2500, "monthly")
			ctx := context.Background()

			// Make the price non-recurring (a free price carries no amount), and detach the
			// subscription, which is the state a one-time or free purchase leaves the service
			// in.
			amount := int64(2500)
			if interval == "free" {
				amount = 0
			}
			if _, err := f.pool.Exec(ctx,
				`UPDATE price_versions SET interval = $2, amount_minor = $3 WHERE id = $1`,
				f.priceID, interval, amount); err != nil {
				t.Fatalf("set price non-recurring: %v", err)
			}
			if _, err := f.pool.Exec(ctx,
				`UPDATE services SET subscription_id = NULL WHERE id = $1`, f.serviceID); err != nil {
				t.Fatalf("detach subscription: %v", err)
			}

			m, err := f.store.ServiceMetadata(ctx, f.serviceID)
			if err != nil {
				t.Fatalf("metadata: %v", err)
			}
			if strings.Contains(m.DerivedDescription, "billed") {
				t.Fatalf("derived description = %q, want no billing cycle for a non-recurring price",
					m.DerivedDescription)
			}
			if m.Interval != "" {
				t.Fatalf("interval = %q, want empty for a service with no subscription", m.Interval)
			}
			// The service is still described: it is not blank just because it does not renew.
			if !strings.Contains(m.DerivedDescription, "VPS Small") {
				t.Fatalf("derived description = %q, want the product name", m.DerivedDescription)
			}
		})
	}
}
