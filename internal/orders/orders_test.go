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
