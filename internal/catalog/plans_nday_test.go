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
