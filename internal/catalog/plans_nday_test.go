package catalog_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/calendar"
	"github.com/catalyst/billing/internal/catalog"
)

// A price can be sold as a fixed number of days ("every 45 days"). These tests pin the
// vocabulary, the persisted form, and the fact that the database refuses a day count outside
// the range the calendar can schedule — so a row written by hand cannot create a period the
// renewal engine would have to guess about.

func TestDayCountIntervalVocabulary(t *testing.T) {
	// The canonical spelling is valid and recurs.
	for _, days := range []int{1, 7, 45, 90, 366} {
		name := calendar.DayInterval(days)
		if err := catalog.ValidateInterval(name); err != nil {
			t.Fatalf("%q must be a valid interval: %v", name, err)
		}
		if !catalog.Recurring(name) {
			t.Fatalf("%q must recur: a day-count period is a schedule", name)
		}
		if got, want := catalog.IntervalLabel(name), "every "+itoa(days)+" days"; got != want {
			t.Fatalf("label of %q = %q, want %q", name, got, want)
		}
	}

	// Anything else is refused, including the non-canonical spellings the calendar rejects.
	for _, bad := range []string{
		"every_0_days", "every_367_days", "every_045_days", "every_-1_days",
		"every_days", "every_45_day", "45_days", "weekly", "",
	} {
		if err := catalog.ValidateInterval(bad); !errors.Is(err, catalog.ErrInvalidInput) {
			t.Fatalf("ValidateInterval(%q) = %v, want ErrInvalidInput", bad, err)
		}
		if catalog.Recurring(bad) {
			t.Fatalf("%q must not recur", bad)
		}
	}

	// The non-recurring values are still non-recurring.
	for _, name := range []string{catalog.IntervalOneTime, catalog.IntervalFree} {
		if catalog.Recurring(name) {
			t.Fatalf("%q must not recur", name)
		}
	}
}

// A day-count price is stored and read back as the same interval, and the label a page renders
// is the customer-facing wording rather than the stored token.
func TestDayCountPriceIsPersisted(t *testing.T) {
	s, pool := newStoreWithPool(t)
	ctx := context.Background()
	cat := mustCategory(t, s, ctx, "nday-cat")
	p, err := s.CreateProduct(ctx, catalog.ProductInput{Slug: "nday", Name: "Every 45 days", CategoryID: &cat.ID})
	if err != nil {
		t.Fatalf("product: %v", err)
	}

	price, err := s.AddPrice(ctx, p.ID, catalog.Price{
		Name: "45 days", Currency: "USD", AmountMinor: 4500,
		Interval: calendar.DayInterval(45), AnchorDay: 20, AnchorTimezone: "Europe/Berlin",
	})
	if err != nil {
		t.Fatalf("add day-count price: %v", err)
	}
	if got, want := price.Interval, "every_45_days"; got != want {
		t.Fatalf("stored interval = %q, want %q", got, want)
	}
	if !price.Recurring() {
		t.Fatal("a day-count price must be recurring")
	}

	// The read path a screen uses returns it unchanged.
	read, err := s.Product(ctx, p.ID)
	if err != nil {
		t.Fatalf("read product: %v", err)
	}
	if len(read.Prices) != 1 || read.Prices[0].Interval != "every_45_days" {
		t.Fatalf("read back %+v, want one every_45_days price", read.Prices)
	}
	if got, want := catalog.IntervalLabel(read.Prices[0].Interval), "every 45 days"; got != want {
		t.Fatalf("label = %q, want %q", got, want)
	}

	// The database refuses a day count the calendar cannot schedule, and refuses the
	// non-canonical spelling that would make one period have two names.
	for _, bad := range []string{"every_0_days", "every_367_days", "every_045_days"} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO price_versions (product_id, version, name, currency, amount_minor, interval,
			                            anchor_day, anchor_timezone)
			VALUES ($1, 77, 'bad', 'USD', 100, $2, 1, 'UTC')`, p.ID, bad); err == nil {
			t.Fatalf("the database accepted the interval %q", bad)
		}
	}

	// The subscription set grew by exactly the recurring day-count form: a day count is
	// accepted there, and a one-time price is still refused.
	var customerID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO customers (email, name) VALUES ('nday@example.test', 'N') RETURNING id`).
		Scan(&customerID); err != nil {
		t.Fatalf("customer: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO subscriptions (customer_id, price_version_id, status, interval, anchor_day,
			timezone, current_period_start, current_period_end, next_billing_at)
		VALUES ($1, $2, 'active', 'every_45_days', 20, 'UTC',
		        $3::timestamptz, $3::timestamptz + interval '45 days',
		        $3::timestamptz + interval '45 days')`,
		customerID, price.ID, time.Date(2024, 1, 20, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("a day-count subscription must be storable: %v", err)
	}
	for _, bad := range []string{"one_time", "free", "every_0_days"} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO subscriptions (customer_id, price_version_id, status, interval, anchor_day,
				timezone, current_period_start, current_period_end, next_billing_at)
			VALUES ($1, $2, 'active', $3, 20, 'UTC',
			        $4::timestamptz, $4::timestamptz + interval '45 days',
			        $4::timestamptz + interval '45 days')`,
			customerID, price.ID, bad, time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)); err == nil {
			t.Fatalf("the database accepted the subscription interval %q", bad)
		}
	}
}

// itoa avoids pulling strconv in for one call in a table of expectations.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// An explicit week or month count is part of the same closed vocabulary: it recurs,
// carries a customer-facing label, and is storable as a price and as a subscription
// interval. The database bounds match internal/calendar's, so a row written by hand
// cannot create a period the renewal engine would have to guess about.
func TestUnitIntervalsArePersisted(t *testing.T) {
	for _, tc := range []struct {
		name, label string
	}{
		{calendar.WeekInterval(2), "every 2 weeks"},
		{calendar.WeekInterval(52), "every 52 weeks"},
		{calendar.MonthInterval(3), "every 3 months"},
		{calendar.MonthInterval(12), "every 12 months"},
	} {
		if err := catalog.ValidateInterval(tc.name); err != nil {
			t.Fatalf("%q must be a valid interval: %v", tc.name, err)
		}
		if !catalog.Recurring(tc.name) {
			t.Fatalf("%q must recur: it is a schedule", tc.name)
		}
		if got := catalog.IntervalLabel(tc.name); got != tc.label {
			t.Fatalf("label of %q = %q, want %q", tc.name, got, tc.label)
		}
	}
	for _, bad := range []string{
		"every_0_weeks", "every_53_weeks", "every_02_weeks", "every_2_week",
		"every_0_months", "every_13_months", "every_03_months", "every_3_month",
	} {
		if err := catalog.ValidateInterval(bad); !errors.Is(err, catalog.ErrInvalidInput) {
			t.Fatalf("ValidateInterval(%q) = %v, want ErrInvalidInput", bad, err)
		}
		if catalog.Recurring(bad) {
			t.Fatalf("%q must not recur", bad)
		}
	}

	s, pool := newStoreWithPool(t)
	ctx := context.Background()
	cat := mustCategory(t, s, ctx, "unit-cat")
	p, err := s.CreateProduct(ctx, catalog.ProductInput{Slug: "unit-periods", Name: "Units", CategoryID: &cat.ID})
	if err != nil {
		t.Fatalf("product: %v", err)
	}
	var priceID string
	for _, interval := range []string{
		calendar.WeekInterval(2), calendar.MonthInterval(3),
	} {
		price, err := s.AddPrice(ctx, p.ID, catalog.Price{
			Name: interval, Currency: "USD", AmountMinor: 1000,
			Interval: interval, AnchorDay: 20, AnchorTimezone: "UTC",
		})
		if err != nil {
			t.Fatalf("add %s price: %v", interval, err)
		}
		if price.Interval != interval || !price.Recurring() {
			t.Fatalf("stored price = %+v, want recurring %q", price, interval)
		}
		priceID = price.ID
	}

	// The database enforces the same bounds and canonical spelling.
	for _, bad := range []string{"every_0_weeks", "every_53_weeks", "every_02_weeks",
		"every_0_months", "every_13_months", "every_03_months"} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO price_versions (product_id, version, name, currency, amount_minor, interval,
			                            anchor_day, anchor_timezone)
			VALUES ($1, 78, 'bad', 'USD', 100, $2, 1, 'UTC')`, p.ID, bad); err == nil {
			t.Fatalf("the database accepted the interval %q", bad)
		}
	}

	var customerID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO customers (email, name) VALUES ('unit@example.test', 'U') RETURNING id`).
		Scan(&customerID); err != nil {
		t.Fatalf("customer: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO subscriptions (customer_id, price_version_id, status, interval, anchor_day,
			timezone, current_period_start, current_period_end, next_billing_at)
		VALUES ($1, $2, 'active', 'every_2_weeks', 20, 'UTC',
		        $3::timestamptz, $3::timestamptz + interval '14 days',
		        $3::timestamptz + interval '14 days')`,
		customerID, priceID, time.Date(2024, 1, 20, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("a week-count subscription must be storable: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO subscriptions (customer_id, price_version_id, status, interval, anchor_day,
			timezone, current_period_start, current_period_end, next_billing_at)
		VALUES ($1, $2, 'active', 'every_3_months', 20, 'UTC',
		        $3::timestamptz, $3::timestamptz + interval '3 months',
		        $3::timestamptz + interval '3 months')`,
		customerID, priceID, time.Date(2024, 1, 20, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("a month-count subscription must be storable: %v", err)
	}
}
