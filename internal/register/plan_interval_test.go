package register_test

import (
	"context"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/register"
)

// Plan types at checkout (SF-13): a one-time purchase is charged once and creates a service with
// no renewal, and a free one activates without an invoice. These tests drive the real checkout
// against the real database, because the property under test is an absent row — and an absent row
// is exactly what a mock would let you assert without proving.
//
// The recurring path is asserted alongside them: the new branch must not have moved it.

// planProduct creates a sellable product whose price version has the given interval.
func (f *fixture) planProduct(t *testing.T, slug, interval string, amount int64) (productID, priceID string) {
	t.Helper()
	ctx := context.Background()
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO products (slug, name) VALUES ($1, $1) RETURNING id`, slug).
		Scan(&productID); err != nil {
		t.Fatalf("create product: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, interval,
			anchor_day, anchor_timezone, name)
		VALUES ($1, 1, 'USD', $2, $3, 1, 'UTC', $3)
		RETURNING id`, productID, amount, interval).Scan(&priceID); err != nil {
		t.Fatalf("create price: %v", err)
	}
	return productID, priceID
}

// A one-time product is invoiced once and produces no subscription. It is the invoice that
// carries the money; the absence of a subscription is what makes a renewal impossible.
func TestOneTimeLineCreatesNoSubscription(t *testing.T) {
	f := newFixture(t, noTax())
	customerID, userID := f.customer(t, "onetime@example.test")
	productID, priceID := f.planProduct(t, "setup-package", "one_time", 25000)

	res, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID: userID,
		Lines:  []register.CheckoutLine{{PriceVersionID: priceID}},
	})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	ctx := context.Background()

	if res.InvoiceID == "" || res.InvoiceNumber == "" {
		t.Fatalf("a one-time purchase must be invoiced: %+v", res)
	}
	if res.Activated {
		t.Fatal("a paid one-time purchase was activated before it was paid")
	}
	if len(res.SubscriptionIDs) != 0 {
		t.Fatalf("subscription ids = %v, want none", res.SubscriptionIDs)
	}
	var subscriptions int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM subscriptions WHERE customer_id = $1`, customerID).Scan(&subscriptions); err != nil {
		t.Fatal(err)
	}
	if subscriptions != 0 {
		t.Fatalf("subscriptions = %d, want 0: nothing may renew a one-time purchase", subscriptions)
	}
	// The invoice is outside the billing series, so the one-invoice-per-period index can never
	// consider it and dunning (which joins subscriptions) can never suspend for it.
	var subscriptionID *string
	var periodKey *string
	if err := f.pool.QueryRow(ctx,
		`SELECT subscription_id, period_key FROM invoices WHERE id = $1`, res.InvoiceID).
		Scan(&subscriptionID, &periodKey); err != nil {
		t.Fatal(err)
	}
	if subscriptionID != nil || periodKey != nil {
		t.Fatalf("invoice subscription=%v period_key=%v, want both null", subscriptionID, periodKey)
	}
	// It is priced once, in full.
	if got, want := res.Total.Minor(), int64(25000); got != want {
		t.Fatalf("total = %d, want %d", got, want)
	}
	_ = productID
}

// A free product activates without an invoice, and its service is written with no subscription.
// This is the zero-total path with a `free` interval, which is the case that used to break: the
// period could not be computed, so checkout refused a product that costs nothing.
func TestFreeLineActivatesWithoutAnInvoiceOrSubscription(t *testing.T) {
	f := newFixture(t, noTax())
	customerID, userID := f.customer(t, "free-plan@example.test")
	productID, priceID := f.planProduct(t, "free-tier", "free", 0)

	res, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID: userID,
		Lines:  []register.CheckoutLine{{PriceVersionID: priceID}},
	})
	if err != nil {
		t.Fatalf("checkout of a free product: %v", err)
	}
	ctx := context.Background()

	if !res.Activated || res.Status != "paid" {
		t.Fatalf("free order = %+v, want activated and paid", res)
	}
	if res.InvoiceID != "" {
		t.Fatalf("a free order issued invoice %q, want none", res.InvoiceNumber)
	}
	if len(res.ServiceIDs) != 1 {
		t.Fatalf("services = %v, want the one it activated", res.ServiceIDs)
	}
	if len(res.SubscriptionIDs) != 0 {
		t.Fatalf("subscription ids = %v, want none", res.SubscriptionIDs)
	}

	var invoices, subscriptions int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM invoices WHERE customer_id = $1`, customerID).Scan(&invoices); err != nil {
		t.Fatal(err)
	}
	if invoices != 0 {
		t.Fatalf("invoices = %d, want 0", invoices)
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM subscriptions WHERE customer_id = $1`, customerID).Scan(&subscriptions); err != nil {
		t.Fatal(err)
	}
	if subscriptions != 0 {
		t.Fatalf("subscriptions = %d, want 0", subscriptions)
	}

	// The service exists, is queued for provisioning, and carries no schedule.
	var (
		status         string
		subscriptionID *string
		product        string
	)
	if err := f.pool.QueryRow(ctx,
		`SELECT status, subscription_id, product_id FROM services WHERE id = $1`, res.ServiceIDs[0]).
		Scan(&status, &subscriptionID, &product); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || subscriptionID != nil || product != productID {
		t.Fatalf("service status=%q subscription=%v product=%s", status, subscriptionID, product)
	}
	var ops int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM service_operations
		WHERE service_id = $1 AND kind = 'create' AND status = 'queued'`,
		res.ServiceIDs[0]).Scan(&ops); err != nil {
		t.Fatal(err)
	}
	if ops != 1 {
		t.Fatalf("queued create operations = %d, want 1: a free service nobody provisions is not delivered", ops)
	}
}

// A recurring line still produces exactly one pending subscription with a next-bill date. The new
// branch must not have moved the old one.
func TestRecurringLineStillCreatesItsSubscription(t *testing.T) {
	f := newFixture(t, noTax())
	_, userID := f.customer(t, "recurring@example.test")
	_, priceID := f.planProduct(t, "monthly-plan", "monthly", 1200)

	res, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID: userID,
		Lines:  []register.CheckoutLine{{PriceVersionID: priceID}},
	})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if len(res.SubscriptionIDs) != 1 || res.SubscriptionIDs[0] == "" {
		t.Fatalf("subscription ids = %v, want exactly one", res.SubscriptionIDs)
	}
	var status, interval string
	var nextBill time.Time
	if err := f.pool.QueryRow(context.Background(), `
		SELECT status, interval, next_billing_at FROM subscriptions WHERE id = $1`,
		res.SubscriptionIDs[0]).Scan(&status, &interval, &nextBill); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || interval != "monthly" {
		t.Fatalf("subscription status=%q interval=%q, want pending and monthly", status, interval)
	}
	if nextBill.IsZero() {
		t.Fatal("the subscription has no next-bill date")
	}
	// The invoice is linked to the schedule, which is what dunning and renewal need.
	var linked string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COALESCE(subscription_id::text, '') FROM invoices WHERE id = $1`, res.InvoiceID).
		Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked != res.SubscriptionIDs[0] {
		t.Fatalf("invoice subscription = %q, want %q", linked, res.SubscriptionIDs[0])
	}
}

// A mixed cart is the case a per-line decision has to get right: the one-time line must not get a
// subscription, and the recurring line must.
func TestMixedCartSchedulesOnlyTheRecurringLine(t *testing.T) {
	f := newFixture(t, noTax())
	customerID, userID := f.customer(t, "mixed@example.test")
	_, oneTime := f.planProduct(t, "migration-fee", "one_time", 5000)
	_, monthly := f.planProduct(t, "hosted-plan", "monthly", 1200)

	res, err := f.checkout.Checkout(context.Background(), register.CheckoutRequest{
		UserID: userID,
		Lines: []register.CheckoutLine{
			{PriceVersionID: oneTime},
			{PriceVersionID: monthly},
		},
	})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if len(res.SubscriptionIDs) != 1 {
		t.Fatalf("subscription ids = %v, want exactly one (the recurring line)", res.SubscriptionIDs)
	}
	var subscriptions int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM subscriptions WHERE customer_id = $1`, customerID).Scan(&subscriptions); err != nil {
		t.Fatal(err)
	}
	if subscriptions != 1 {
		t.Fatalf("subscriptions = %d, want 1", subscriptions)
	}
	// The invoice is linked to the schedule the order did create, so dunning can find it.
	var linked string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COALESCE(subscription_id::text, '') FROM invoices WHERE id = $1`, res.InvoiceID).
		Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked != res.SubscriptionIDs[0] {
		t.Fatalf("invoice subscription = %q, want the recurring line's subscription", linked)
	}
}
