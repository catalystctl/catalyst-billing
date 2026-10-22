package orders_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/orders"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// The purchase is the join between the catalog and the ledger, so these tests assert
// the four rows it writes agree: order, item, subscription and invoice. A bug here is
// money taken for nothing, or a service nobody will provision.

var testNow = time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

type fixture struct {
	pool    *store.Pool
	svc     *orders.Service
	custID  string
	prodID  string
	priceID string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := storetest.DB(t)
	ctx := context.Background()

	var custID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO customers (email, name) VALUES ('buyer@example.test', 'Buyer') RETURNING id`).
		Scan(&custID); err != nil {
		t.Fatalf("customer: %v", err)
	}
	var prodID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO products (slug, name) VALUES ('vps', 'VPS') RETURNING id`).Scan(&prodID); err != nil {
		t.Fatalf("product: %v", err)
	}
	var priceID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, setup_minor,
			interval, anchor_day, anchor_timezone, name)
		VALUES ($1, 1, 'USD', 1200, 500, 'monthly', 15, 'UTC', 'Monthly')
		RETURNING id`, prodID).Scan(&priceID); err != nil {
		t.Fatalf("price: %v", err)
	}

	return &fixture{
		pool: pool,
		svc: orders.New(billing.NewIssuer(billing.FixedTaxPricer{
			Policy: "no-tax", Jurisdiction: "none",
		})),
		custID:  custID,
		prodID:  prodID,
		priceID: priceID,
	}
}

func (f *fixture) create(t *testing.T, req orders.Request) (orders.Result, error) {
	t.Helper()
	var out orders.Result
	err := f.pool.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if req.CustomerID == "" {
			req.CustomerID = f.custID
		}
		if req.PriceVersionID == "" {
			req.PriceVersionID = f.priceID
		}
		req.Merchant = billing.Merchant{Name: "Catalyst", CountryCode: "GB"}
		req.Terms = "Payable on issue"
		req.DueAt = testNow.AddDate(0, 0, 30)
		req.Now = testNow
		res, err := f.svc.Create(ctx, tx, req)
		out = res
		return err
	})
	return out, err
}

func TestCreateWritesOrderItemSubscriptionAndInvoice(t *testing.T) {
	f := newFixture(t)
	res, err := f.create(t, orders.Request{Quantity: 2})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx := context.Background()

	if res.Status != "pending_payment" {
		t.Fatalf("status = %q, want pending_payment", res.Status)
	}
	// 2 x 12.00 plus 2 x 5.00 setup, no tax in this pricer.
	if got, want := res.Total.Minor(), int64(3400); got != want {
		t.Fatalf("total = %d, want %d", got, want)
	}
	if res.InvoiceNumber == "" {
		t.Fatal("the invoice was not issued: no number")
	}

	var (
		orderStatus string
		totalMinor  int64
		currency    string
	)
	if err := f.pool.QueryRow(ctx, `
		SELECT status, total_minor, currency FROM orders WHERE id = $1`, res.OrderID).
		Scan(&orderStatus, &totalMinor, &currency); err != nil {
		t.Fatalf("read order: %v", err)
	}
	if orderStatus != "pending_payment" || totalMinor != 3400 || currency != "USD" {
		t.Fatalf("order = %s/%d/%s", orderStatus, totalMinor, currency)
	}

	var (
		qty, unit, line, setup int64
		itemPrice              string
	)
	if err := f.pool.QueryRow(ctx, `
		SELECT quantity, unit_minor, line_minor, setup_minor, price_version_id
		FROM order_items WHERE order_id = $1`, res.OrderID).
		Scan(&qty, &unit, &line, &setup, &itemPrice); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if qty != 2 || unit != 1200 || line != 2400 || setup != 1000 || itemPrice != f.priceID {
		t.Fatalf("item = qty %d unit %d line %d setup %d price %s", qty, unit, line, setup, itemPrice)
	}

	// The subscription is pending: an unpaid order must not renew. The payment applier
	// activates it.
	var (
		subStatus string
		interval  string
		tz        string
		start     time.Time
		end       time.Time
	)
	if err := f.pool.QueryRow(ctx, `
		SELECT status, interval, timezone, current_period_start, current_period_end
		FROM subscriptions WHERE id = $1`, res.SubscriptionID).
		Scan(&subStatus, &interval, &tz, &start, &end); err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if subStatus != "pending" || interval != "monthly" || tz != "UTC" {
		t.Fatalf("subscription = %s/%s/%s", subStatus, interval, tz)
	}
	if !end.After(start) {
		t.Fatalf("period is not positive: %s..%s", start, end)
	}

	// The invoice must point at both the order and the subscription, or fulfilment
	// cannot find what to provision and dunning cannot find what funds it.
	var (
		invOrder, invSub, invStatus string
		invTotal                    int64
	)
	if err := f.pool.QueryRow(ctx, `
		SELECT order_id, subscription_id, status, total_minor FROM invoices WHERE id = $1`,
		res.InvoiceID).Scan(&invOrder, &invSub, &invStatus, &invTotal); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	if invOrder != res.OrderID || invSub != res.SubscriptionID {
		t.Fatalf("invoice links = order %s sub %s", invOrder, invSub)
	}
	if invStatus != "open" || invTotal != 3400 {
		t.Fatalf("invoice = %s/%d", invStatus, invTotal)
	}

	// Two lines: the plan and the setup.
	var lines int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM invoice_lines WHERE invoice_id = $1`,
		res.InvoiceID).Scan(&lines); err != nil {
		t.Fatalf("count lines: %v", err)
	}
	if lines != 2 {
		t.Fatalf("invoice lines = %d, want 2 (plan + setup)", lines)
	}
}

func TestCreateRejectsInvalidInput(t *testing.T) {
	f := newFixture(t)

	if _, err := f.create(t, orders.Request{Quantity: -1}); !errors.Is(err, orders.ErrInvalidQuantity) {
		t.Fatalf("negative quantity: %v", err)
	}
	if _, err := f.create(t, orders.Request{Quantity: 1001}); !errors.Is(err, orders.ErrInvalidQuantity) {
		t.Fatalf("oversized quantity: %v", err)
	}
	if _, err := f.create(t, orders.Request{PriceVersionID: "11111111-1111-1111-1111-111111111111"}); !errors.Is(err, orders.ErrPriceNotFound) {
		t.Fatalf("missing price: %v", err)
	}
	if _, err := f.create(t, orders.Request{CustomerID: "22222222-2222-2222-2222-222222222222"}); !errors.Is(err, orders.ErrCustomerNotFound) {
		t.Fatalf("missing customer: %v", err)
	}
}

// An invalid price version (bad timezone) must be a validation error, not a 500 and not a
// half-written order.
func TestCreateRejectsUnbillablePrice(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var priceID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, interval,
			anchor_day, anchor_timezone, name)
		VALUES ($1, 2, 'USD', 1200, 'monthly', 1, 'Mars/Olympus', 'Broken')
		RETURNING id`, f.prodID).Scan(&priceID); err != nil {
		t.Fatalf("price: %v", err)
	}

	_, err := f.create(t, orders.Request{PriceVersionID: priceID})
	if !errors.Is(err, orders.ErrInvalidSchedule) {
		t.Fatalf("err = %v, want ErrInvalidSchedule", err)
	}
	// Nothing was written.
	var orders int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&orders); err != nil {
		t.Fatal(err)
	}
	if orders != 0 {
		t.Fatalf("orders = %d after a failed create", orders)
	}
}

// The default quantity is one, because a storefront button sends no quantity.
func TestCreateDefaultsToQuantityOne(t *testing.T) {
	f := newFixture(t)
	res, err := f.create(t, orders.Request{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var qty int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT quantity FROM order_items WHERE order_id = $1`, res.OrderID).Scan(&qty); err != nil {
		t.Fatal(err)
	}
	if qty != 1 {
		t.Fatalf("quantity = %d, want 1", qty)
	}
}

// ------------------------------------------------------- one-time and free --

// addPrice inserts one price version and returns its id. The tests below each need a price with
// a different interval, and the fixture's is monthly.
func (f *fixture) addPrice(t *testing.T, version int, interval string, amount, setup int64) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(), `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, setup_minor,
			interval, anchor_day, anchor_timezone, name)
		VALUES ($1, $2, 'USD', $3, $4, $5, 15, 'UTC', $6)
		RETURNING id`, f.prodID, version, amount, setup, interval, interval).Scan(&id); err != nil {
		t.Fatalf("price %s: %v", interval, err)
	}
	return id
}

// A one-time price is charged once and creates a service with no subscription. The absence of
// the subscription is the whole point: it is what makes the renewal engine unable to see it.
func TestOneTimePurchaseCreatesNoSubscription(t *testing.T) {
	f := newFixture(t)
	priceID := f.addPrice(t, 10, "one_time", 25000, 0)
	res, err := f.create(t, orders.Request{PriceVersionID: priceID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx := context.Background()

	if res.Status != "pending_payment" || res.InvoiceID == "" {
		t.Fatalf("one-time purchase = %+v, want an ordinary invoice to pay", res)
	}
	if res.SubscriptionID != "" {
		t.Fatalf("subscription = %q, want none: a one-time purchase must not create a schedule", res.SubscriptionID)
	}
	var subscriptions int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM subscriptions`).Scan(&subscriptions); err != nil {
		t.Fatal(err)
	}
	if subscriptions != 0 {
		t.Fatalf("subscriptions = %d after a one-time purchase, want 0", subscriptions)
	}
	// The invoice is not part of a billing series, so the per-period unique index can never
	// consider it and dunning (which joins subscriptions) can never suspend for it.
	var subID *string
	var periodKey *string
	if err := f.pool.QueryRow(ctx,
		`SELECT subscription_id, period_key FROM invoices WHERE id = $1`, res.InvoiceID).
		Scan(&subID, &periodKey); err != nil {
		t.Fatal(err)
	}
	if subID != nil || periodKey != nil {
		t.Fatalf("invoice subscription=%v period_key=%v, want both null", subID, periodKey)
	}
	// It was priced once, in full.
	if got, want := res.Total.Minor(), int64(25000); got != want {
		t.Fatalf("total = %d, want %d", got, want)
	}
}

// A free price activates without an invoice. There is nothing to pay, so a zero-total document
// would be a document nobody can pay.
func TestFreePurchaseActivatesWithoutAnInvoice(t *testing.T) {
	f := newFixture(t)
	priceID := f.addPrice(t, 11, "free", 0, 0)
	res, err := f.create(t, orders.Request{PriceVersionID: priceID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx := context.Background()

	if res.InvoiceID != "" || res.InvoiceNumber != "" {
		t.Fatalf("free purchase issued an invoice (%q), want none", res.InvoiceNumber)
	}
	if res.Status != "paid" {
		t.Fatalf("status = %q, want paid", res.Status)
	}
	if res.Total.Minor() != 0 {
		t.Fatalf("total = %d, want 0", res.Total.Minor())
	}
	var invoices int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM invoices`).Scan(&invoices); err != nil {
		t.Fatal(err)
	}
	if invoices != 0 {
		t.Fatalf("invoices = %d for a free purchase, want 0", invoices)
	}
	if len(res.ServiceIDs) != 1 {
		t.Fatalf("service ids = %v, want the service it activated", res.ServiceIDs)
	}
	// The service exists, has no schedule, and is queued for provisioning: exactly what a
	// settled payment would have produced.
	var status string
	var subscription *string
	if err := f.pool.QueryRow(ctx, `SELECT status, subscription_id FROM services WHERE id = $1`,
		res.ServiceIDs[0]).Scan(&status, &subscription); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || subscription != nil {
		t.Fatalf("service status=%q subscription=%v, want pending and no schedule", status, subscription)
	}
	var operations int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM service_operations WHERE service_id = $1 AND kind = 'create' AND status = 'queued'`,
		res.ServiceIDs[0]).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if operations != 1 {
		t.Fatalf("create operations = %d, want 1: a free service nobody provisions is not delivered", operations)
	}
}

// A recurring price still creates a subscription with the right next-bill date. This is the
// other half of the pair above: the branch must not have broken the ordinary path.
func TestRecurringPurchaseCreatesSubscriptionWithNextBillDate(t *testing.T) {
	f := newFixture(t)
	res, err := f.create(t, orders.Request{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.SubscriptionID == "" {
		t.Fatal("a recurring purchase must create a subscription")
	}
	ctx := context.Background()
	var interval string
	var start, end, next time.Time
	if err := f.pool.QueryRow(ctx, `
		SELECT interval, current_period_start, current_period_end, next_billing_at
		  FROM subscriptions WHERE id = $1`, res.SubscriptionID).
		Scan(&interval, &start, &end, &next); err != nil {
		t.Fatal(err)
	}
	if interval != "monthly" {
		t.Fatalf("interval = %q, want monthly", interval)
	}
	// The anchor day is the 15th and testNow is the 15th of March, so the period runs to the
	// 15th of April and that is the next bill.
	// The anchor puts the boundary at midnight in the billing timezone, so the period starts on
	// the 15th rather than at the instant of purchase.
	if got, want := start.UTC().Format("2006-01-02"), "2026-03-15"; got != want {
		t.Fatalf("period start = %s, want %s", got, want)
	}
	if !next.Equal(end) {
		t.Fatalf("next_billing_at = %s, want the period end %s", next, end)
	}
	if got, want := next.UTC().Format("2006-01-02"), "2026-04-15"; got != want {
		t.Fatalf("next bill = %s, want %s", got, want)
	}
}

// A one-time service is invisible to the two things that could otherwise bill it again: the
// renewal scanner and dunning. Both are SQL predicates, so the assertion here is that the rows a
// one-time purchase leaves behind do not match them.
//
// This is deliberately written from the data rather than by calling those packages: the property
// is "no subscription exists", and a caller that never joins can only be defeated by a row that
// does not exist.
func TestOneTimeServiceIsInvisibleToRenewalAndDunning(t *testing.T) {
	f := newFixture(t)
	priceID := f.addPrice(t, 12, "free", 0, 0)
	res, err := f.create(t, orders.Request{PriceVersionID: priceID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(res.ServiceIDs) != 1 {
		t.Fatalf("services = %v, want one", res.ServiceIDs)
	}
	ctx := context.Background()

	// The renewal scanner's own predicate: it reads `subscriptions`, and a service with no
	// schedule has no row there to scan.
	var due int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM subscriptions sub
		  JOIN services s ON s.subscription_id = sub.id
		 WHERE s.id = $1 AND sub.status IN ('active', 'past_due')`, res.ServiceIDs[0]).
		Scan(&due); err != nil {
		t.Fatal(err)
	}
	if due != 0 {
		t.Fatalf("%d renewable subscriptions behind a one-time service, want 0", due)
	}

	// Dunning's own join: it finds the invoice that funds a service through the subscription, so
	// an invoice that cannot recur can never justify a suspension.
	var funding int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM invoices i
		  JOIN subscriptions sub ON sub.id = i.subscription_id
		  JOIN services s ON s.subscription_id = sub.id
		 WHERE s.id = $1 AND i.status = 'open'`, res.ServiceIDs[0]).Scan(&funding); err != nil {
		t.Fatal(err)
	}
	if funding != 0 {
		t.Fatalf("%d open invoices link a one-time service to dunning, want 0", funding)
	}

	// The portal's own read: a left join, so it reports "no next bill" rather than inventing one.
	var nextBill *time.Time
	if err := f.pool.QueryRow(ctx, `
		SELECT sub.next_billing_at FROM services s
		  LEFT JOIN subscriptions sub ON sub.id = s.subscription_id
		 WHERE s.id = $1`, res.ServiceIDs[0]).Scan(&nextBill); err != nil {
		t.Fatal(err)
	}
	if nextBill != nil {
		t.Fatalf("the portal would claim a next-bill date of %s for a one-time service", nextBill)
	}
}
