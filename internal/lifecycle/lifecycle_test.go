package lifecycle_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/dunning"
	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/lifecycle"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// The lifecycle jobs are tested against the real database, because every property that
// matters here is a database property: the `open -> void` transition is guarded by a trigger,
// the terminate operation is unique per (service, generation), the audit row is written in the
// same transaction, and the notification dedupe is a unique index. A fake would prove none of
// that.

var usd = money.MustLookupCurrency("USD")

// baseTime is fixed so "older than N days" has one answer. Rows carry explicit timestamps;
// nothing here depends on the wall clock.
var baseTime = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

// orderTotal is the price of one unit in every fixture.
const orderTotal int64 = 1200

type fixture struct {
	pool       *store.Pool
	customerID string
	productID  string
	priceID    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := storetest.DB(t)
	ctx := context.Background()

	// The termination event renders through the template catalogue. A missing template is a
	// hard error in internal/notify by design, so the fixture seeds the defaults the way a
	// booting process does.
	if _, err := notify.SeedTemplates(ctx, pool); err != nil {
		t.Fatalf("seed templates: %v", err)
	}

	f := &fixture{pool: pool}
	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO customers (email, name) VALUES ('customer@example.test', 'Customer')
			RETURNING id`).Scan(&f.customerID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO products (slug, name, stock) VALUES ('vps', 'VPS', 5)
			RETURNING id`).Scan(&f.productID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO price_versions
				(product_id, version, currency, amount_minor, interval, anchor_day, anchor_timezone)
			VALUES ($1, 1, 'USD', $2, 'monthly', 1, 'UTC')
			RETURNING id`, f.productID, orderTotal).Scan(&f.priceID)
	})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return f
}

// runner builds a Runner on the fixed clock.
func (f *fixture) runner() *lifecycle.Runner {
	return lifecycle.New(f.pool).WithClock(func() time.Time { return baseTime })
}

// setSetting writes one settings row for the duration of a test.
//
// The settings table is seeded by a migration and is *not* truncated between tests, so a test
// that changes a threshold must put it back or it leaks into the next one.
func (f *fixture) setSetting(t *testing.T, key, value string) {
	t.Helper()
	ctx := context.Background()
	var previous string
	if err := f.pool.QueryRow(ctx, `
		SELECT value FROM settings WHERE group_key = 'cronjob' AND key = $1`, key).
		Scan(&previous); err != nil {
		t.Fatalf("read setting %s: %v", key, err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `
			UPDATE settings SET value = $2 WHERE group_key = 'cronjob' AND key = $1`,
			key, previous)
	})
	if _, err := f.pool.Exec(ctx, `
		UPDATE settings SET value = $2 WHERE group_key = 'cronjob' AND key = $1`,
		key, value); err != nil {
		t.Fatalf("write setting %s: %v", key, err)
	}
}

type orderSpec struct {
	createdAt time.Time
	total     int64
	status    string
	quantity  int
	// noInvoice leaves the order without a document, which is how a stuck row looks.
	noInvoice bool
	// skipStock leaves the stock alone, which is what the staff order path does today.
	skipStock bool
}

type orderFixture struct {
	orderID        string
	invoiceID      string
	subscriptionID string
}

// order writes the four rows a purchase leaves behind, at the timestamps the test needs.
func (f *fixture) order(t *testing.T, spec orderSpec) orderFixture {
	t.Helper()
	ctx := context.Background()

	if spec.quantity == 0 {
		spec.quantity = 1
	}
	if spec.status == "" {
		spec.status = "pending_payment"
	}

	out := orderFixture{}
	err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		periodStart := spec.createdAt
		periodEnd := periodStart.AddDate(0, 1, 0)
		if err := tx.QueryRow(ctx, `
			INSERT INTO subscriptions
				(customer_id, price_version_id, status, interval, anchor_day, timezone,
				 current_period_start, current_period_end, next_billing_at, created_at)
			VALUES ($1, $2, 'pending', 'monthly', 1, 'UTC', $3, $4, $4, $5)
			RETURNING id`,
			f.customerID, f.priceID, periodStart, periodEnd, spec.createdAt).
			Scan(&out.subscriptionID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO orders (customer_id, status, currency, total_minor, created_at, updated_at)
			VALUES ($1, $2, 'USD', $3, $4, $4)
			RETURNING id`,
			f.customerID, spec.status, spec.total, spec.createdAt).Scan(&out.orderID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO order_items
				(order_id, price_version_id, description, quantity, unit_minor, line_minor,
				 setup_minor, position)
			VALUES ($1, $2, 'VPS', $3, $4, $5, 0, 1)`,
			out.orderID, f.priceID, spec.quantity, orderTotal,
			orderTotal*int64(spec.quantity)); err != nil {
			return err
		}
		// The checkout path takes the stock in the same transaction as the order; this
		// mirrors it so the restore has something to restore.
		if !spec.skipStock {
			if _, err := tx.Exec(ctx, `
				UPDATE products SET stock = stock - $2
				WHERE id = $1 AND stock IS NOT NULL`, f.productID, spec.quantity); err != nil {
				return err
			}
		}
		if spec.noInvoice {
			return nil
		}
		return f.issue(ctx, tx, out.orderID, out.subscriptionID, &out.invoiceID, spec.createdAt)
	})
	if err != nil {
		t.Fatalf("order fixture: %v", err)
	}
	return out
}

// issue writes an issued invoice for an order or a subscription.
func (f *fixture) issue(ctx context.Context, tx pgx.Tx, orderID, subscriptionID string,
	into *string, now time.Time) error {

	issuer := billing.NewIssuer(billing.FixedTaxPricer{
		Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
	})
	req := billing.IssueRequest{
		CustomerID:     f.customerID,
		SubscriptionID: subscriptionID,
		// The period key is derived from the creation instant so two fixtures for the same
		// customer cannot collide on the one-invoice-per-period index.
		PeriodKey: now.UTC().Format("2006-01-02T15:04:05Z"),
		Lines: []billing.Line{{
			Description: "VPS", QuantityMilli: 1000, UnitPrice: money.New(orderTotal, usd),
		}},
		Customer: billing.CustomerDetails{ID: f.customerID},
		DueAt:    now.AddDate(0, 0, 7),
	}
	if orderID != "" {
		req.OrderID = orderID
	}
	id, _, _, err := issuer.CreateDraft(ctx, tx, req)
	if err != nil {
		return err
	}
	issued, err := issuer.Issue(ctx, tx, id, billing.DefaultSeries)
	if err != nil {
		return err
	}
	*into = issued.InvoiceID
	return nil
}

// suspendedService builds a suspended service with a funded, open invoice and a successful
// suspend operation at suspendedAt.
func (f *fixture) suspendedService(t *testing.T, suspendedAt time.Time) (serviceID, invoiceID string) {
	t.Helper()
	ctx := context.Background()

	err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		periodStart := suspendedAt.AddDate(0, -1, 0)
		periodEnd := suspendedAt
		var subscriptionID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO subscriptions
				(customer_id, price_version_id, status, interval, anchor_day, timezone,
				 current_period_start, current_period_end, next_billing_at)
			VALUES ($1, $2, 'active', 'monthly', 1, 'UTC', $3, $4, $4)
			RETURNING id`,
			f.customerID, f.priceID, periodStart, periodEnd).Scan(&subscriptionID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO services (customer_id, subscription_id, product_id, status, generation)
			VALUES ($1, $2, $3, 'suspended', 1)
			RETURNING id`, f.customerID, subscriptionID, f.productID).Scan(&serviceID); err != nil {
			return err
		}
		// The invoice that was never paid: this is what "suspended for non-payment" means, and
		// it is also what the termination rule asks about before acting.
		if err := f.issue(ctx, tx, "", subscriptionID, &invoiceID, periodStart); err != nil {
			return err
		}
		// The suspension date is persisted state: a succeeded suspend operation.
		_, err := tx.Exec(ctx, `
			INSERT INTO service_operations
				(service_id, kind, operation_key, generation, status, created_at, started_at, finished_at)
			VALUES ($1, 'suspend', $2, 1, 'succeeded', $3, $3, $3)`,
			serviceID, "suspend:"+serviceID+":1", suspendedAt)
		return err
	})
	if err != nil {
		t.Fatalf("suspended service fixture: %v", err)
	}
	return serviceID, invoiceID
}

// ---------------------------------------------------------------- assertions --

func (f *fixture) orderStatus(t *testing.T, orderID string) string {
	t.Helper()
	var status string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status FROM orders WHERE id = $1`, orderID).Scan(&status); err != nil {
		t.Fatalf("read order status: %v", err)
	}
	return status
}

func (f *fixture) invoiceStatus(t *testing.T, invoiceID string) (status, voidReason string) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status, void_reason FROM invoices WHERE id = $1`, invoiceID).
		Scan(&status, &voidReason); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	return status, voidReason
}

func (f *fixture) stock(t *testing.T) int {
	t.Helper()
	var stock int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT stock FROM products WHERE id = $1`, f.productID).Scan(&stock); err != nil {
		t.Fatalf("read stock: %v", err)
	}
	return stock
}

func (f *fixture) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func (f *fixture) receivable(t *testing.T) money.Amount {
	t.Helper()
	amount, err := ledger.ReceivableForCustomer(context.Background(), f.pool, f.customerID, usd)
	if err != nil {
		t.Fatalf("receivable: %v", err)
	}
	return amount
}

// ------------------------------------------------------------------- orders --

// An order older than the threshold is cancelled, its invoice is voided (not deleted), the
// stock it held goes back, and the receivable it posted is reversed.
func TestUnpaidOrderOlderThanThresholdIsCancelled(t *testing.T) {
	f := newFixture(t)
	o := f.order(t, orderSpec{createdAt: baseTime.AddDate(0, 0, -20), total: orderTotal})

	if got := f.stock(t); got != 4 {
		t.Fatalf("stock before the run = %d, want 4 (the order took one)", got)
	}

	run, err := f.runner().CancelUnpaidOrders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Cancelled) != 1 || run.Cancelled[0].OrderID != o.orderID {
		t.Fatalf("cancelled = %+v, want exactly %s", run.Cancelled, o.orderID)
	}
	if run.Days != 14 {
		t.Fatalf("days = %d, want the configured default 14", run.Days)
	}

	if got := f.orderStatus(t, o.orderID); got != "cancelled" {
		t.Fatalf("order status = %q, want cancelled", got)
	}

	status, reason := f.invoiceStatus(t, o.invoiceID)
	if status != "void" {
		t.Fatalf("invoice status = %q, want void", status)
	}
	if reason == "" {
		t.Fatal("the voided invoice has no reason: a customer reading it would not know why")
	}
	if n := f.count(t, `SELECT count(*) FROM invoices WHERE id = $1`, o.invoiceID); n != 1 {
		t.Fatalf("invoice rows = %d, want 1: a void burns the number, it does not delete the document", n)
	}

	if got := f.stock(t); got != 5 {
		t.Fatalf("stock after the run = %d, want 5 (restored)", got)
	}

	var subscriptionStatus string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status FROM subscriptions WHERE id = $1`, o.subscriptionID).
		Scan(&subscriptionStatus); err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if subscriptionStatus != "cancelled" {
		t.Fatalf("subscription status = %q, want cancelled: an abandoned order must not keep a "+
			"per-product slot", subscriptionStatus)
	}

	// The subledger must not keep a receivable for a purchase the panel abandoned.
	if got := f.receivable(t); got.IsPositive() {
		t.Fatalf("receivable after cancellation = %s, want nothing owed", got.Format())
	}

	if n := f.count(t, `SELECT count(*) FROM audit_events WHERE action = 'order.cancel' AND target_id = $1`,
		o.orderID); n != 1 {
		t.Fatalf("order.cancel audit rows = %d, want 1", n)
	}
	if n := f.count(t, `SELECT count(*) FROM audit_events WHERE action = 'invoice.void' AND target_id = $1`,
		o.invoiceID); n != 1 {
		t.Fatalf("invoice.void audit rows = %d, want 1", n)
	}
}

// An order younger than the threshold is left alone, and so is one that has nothing to pay.
func TestYoungerOrderAndZeroTotalOrderAreUntouched(t *testing.T) {
	f := newFixture(t)
	young := f.order(t, orderSpec{createdAt: baseTime.AddDate(0, 0, -13), total: orderTotal})
	free := f.order(t, orderSpec{createdAt: baseTime.AddDate(0, 0, -40), total: 0, noInvoice: true})

	run, err := f.runner().CancelUnpaidOrders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Cancelled) != 0 {
		t.Fatalf("cancelled = %+v, want none", run.Cancelled)
	}

	if got := f.orderStatus(t, young.orderID); got != "pending_payment" {
		t.Fatalf("young order status = %q, want pending_payment", got)
	}
	if status, _ := f.invoiceStatus(t, young.invoiceID); status != "open" {
		t.Fatalf("young invoice status = %q, want open", status)
	}
	if got := f.orderStatus(t, free.orderID); got != "pending_payment" {
		t.Fatalf("zero-total order status = %q, want pending_payment: it is a stuck row, not a debt", got)
	}
	// One unit is still held by each order: the young one will be paid for, and the free one
	// took its unit before the checkout decided it was free.
	if got := f.stock(t); got != 3 {
		t.Fatalf("stock = %d, want 3", got)
	}
}

// An order whose invoice took money is never cancelled, whatever its status column says.
func TestOrderWithAPaidInvoiceIsNeverCancelled(t *testing.T) {
	f := newFixture(t)
	o := f.order(t, orderSpec{createdAt: baseTime.AddDate(0, 0, -40), total: orderTotal})

	ctx := context.Background()
	if err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := billing.ApplyPayment(ctx, tx, billing.AllocationRequest{
			Provider:          "stripe",
			ProviderAccount:   "acct",
			ProviderPaymentID: "pi_" + o.invoiceID,
			InvoiceID:         o.invoiceID,
			Amount:            money.New(orderTotal, usd),
		})
		return err
	}); err != nil {
		t.Fatalf("pay the invoice: %v", err)
	}
	// The order status lags the payment in this fixture on purpose: the job must not rely on
	// the status column alone.
	if _, err := f.pool.Exec(ctx, `UPDATE orders SET status = 'pending_payment' WHERE id = $1`,
		o.orderID); err != nil {
		t.Fatalf("reset order status: %v", err)
	}

	run, err := f.runner().CancelUnpaidOrders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Cancelled) != 0 {
		t.Fatalf("cancelled = %+v, want none: the invoice was paid", run.Cancelled)
	}
	if got := f.orderStatus(t, o.orderID); got != "pending_payment" {
		t.Fatalf("order status = %q, want pending_payment", got)
	}
	if status, _ := f.invoiceStatus(t, o.invoiceID); status != "paid" {
		t.Fatalf("invoice status = %q, want paid", status)
	}
	if got := f.stock(t); got != 4 {
		t.Fatalf("stock = %d, want 4: a paid order keeps its unit", got)
	}
}

// An order with a live gateway session is left alone: the customer may be paying right now.
func TestOrderWithALivePaymentAttemptIsNotCancelled(t *testing.T) {
	f := newFixture(t)
	o := f.order(t, orderSpec{createdAt: baseTime.AddDate(0, 0, -40), total: orderTotal})

	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO payment_attempts (invoice_id, gateway, operation_key, status, amount_minor, currency)
		VALUES ($1, 'stripe', $2, 'pending', $3, 'USD')`,
		o.invoiceID, "checkout:"+o.invoiceID, orderTotal); err != nil {
		t.Fatalf("create payment attempt: %v", err)
	}

	run, err := f.runner().CancelUnpaidOrders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Cancelled) != 0 {
		t.Fatalf("cancelled = %+v, want none while a gateway session is open", run.Cancelled)
	}
	if status, _ := f.invoiceStatus(t, o.invoiceID); status != "open" {
		t.Fatalf("invoice status = %q, want open", status)
	}
}

// Running the job twice cancels once: one void, one restore, one audit row.
func TestOrderCancellationIsIdempotent(t *testing.T) {
	f := newFixture(t)
	o := f.order(t, orderSpec{createdAt: baseTime.AddDate(0, 0, -20), total: orderTotal})

	first, err := f.runner().CancelUnpaidOrders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Cancelled) != 1 {
		t.Fatalf("first run cancelled %d orders, want 1", len(first.Cancelled))
	}

	second, err := f.runner().CancelUnpaidOrders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Cancelled) != 0 {
		t.Fatalf("second run cancelled %+v, want nothing", second.Cancelled)
	}
	if second.Scanned != 0 {
		t.Fatalf("second run scanned %d orders, want 0: a cancelled order is not a candidate", second.Scanned)
	}
	if got := f.stock(t); got != 5 {
		t.Fatalf("stock = %d after two runs, want 5: the restore must happen once", got)
	}
	if n := f.count(t, `SELECT count(*) FROM audit_events WHERE action = 'order.cancel'`); n != 1 {
		t.Fatalf("order.cancel audit rows = %d, want 1", n)
	}
	if n := f.count(t, `SELECT count(*) FROM audit_events WHERE action = 'invoice.void'`); n != 1 {
		t.Fatalf("invoice.void audit rows = %d, want 1", n)
	}
	if n := f.count(t, `SELECT count(*) FROM journal_transactions WHERE reference_id = $1`, o.invoiceID); n != 2 {
		t.Fatalf("journal transactions for the invoice = %d, want 2 (issue and void reversal)", n)
	}
}

// A kill switch stops the job completely.
func TestOrderCancellationKillSwitchDoesNothing(t *testing.T) {
	f := newFixture(t)
	o := f.order(t, orderSpec{createdAt: baseTime.AddDate(0, 0, -20), total: orderTotal})
	t.Setenv(lifecycle.OrderCancelKillSwitch, "1")

	run, err := f.runner().CancelUnpaidOrders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if run.Reason != lifecycle.ReasonKillSwitch {
		t.Fatalf("reason = %q, want %q", run.Reason, lifecycle.ReasonKillSwitch)
	}
	if got := f.orderStatus(t, o.orderID); got != "pending_payment" {
		t.Fatalf("order status = %q, want pending_payment", got)
	}
	if got := f.stock(t); got != 4 {
		t.Fatalf("stock = %d, want 4", got)
	}

	for _, job := range lifecycle.Jobs(f.pool, nil) {
		if job.Name == lifecycle.OrderCancelJob {
			t.Fatal("the disabled job is still scheduled")
		}
	}
}

// A zero or negative threshold means "this rule is off", not "everything is overdue". An
// installation with a misconfigured threshold must do nothing rather than cancel every order
// it has.
func TestOrderCancellationWithNoThresholdActsOnNothing(t *testing.T) {
	f := newFixture(t)
	// Young enough that a 0-day threshold would catch it if 0 were read as a number.
	o := f.order(t, orderSpec{createdAt: baseTime.Add(-time.Hour), total: orderTotal})
	f.setSetting(t, "order_cancel_days", "0")

	run, err := f.runner().CancelUnpaidOrders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if run.Reason != lifecycle.ReasonThresholdDisabled {
		t.Fatalf("reason = %q, want %q", run.Reason, lifecycle.ReasonThresholdDisabled)
	}
	if run.Scanned != 0 {
		t.Fatalf("scanned %d orders, want 0", run.Scanned)
	}
	if got := f.orderStatus(t, o.orderID); got != "pending_payment" {
		t.Fatalf("order status = %q, want pending_payment", got)
	}

	// The same for a value that is not a number at all.
	f.setSetting(t, "order_cancel_days", "fourteen")
	run, err = f.runner().CancelUnpaidOrders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if run.Reason != lifecycle.ReasonThresholdUnreadable {
		t.Fatalf("reason = %q, want %q", run.Reason, lifecycle.ReasonThresholdUnreadable)
	}
	if got := f.orderStatus(t, o.orderID); got != "pending_payment" {
		t.Fatalf("order status = %q, want pending_payment", got)
	}
}

// A configured threshold is the one that applies.
func TestOrderCancellationUsesTheConfiguredThreshold(t *testing.T) {
	f := newFixture(t)
	o := f.order(t, orderSpec{createdAt: baseTime.AddDate(0, 0, -5), total: orderTotal})

	// Five days old, so the default 14 leaves it alone...
	run, err := f.runner().CancelUnpaidOrders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Cancelled) != 0 {
		t.Fatalf("cancelled = %+v, want none under the 14-day default", run.Cancelled)
	}

	// ...and a 3-day threshold cancels it.
	f.setSetting(t, "order_cancel_days", "3")
	run, err = f.runner().CancelUnpaidOrders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Cancelled) != 1 {
		t.Fatalf("cancelled = %+v, want one under a 3-day threshold", run.Cancelled)
	}
	if got := f.orderStatus(t, o.orderID); got != "cancelled" {
		t.Fatalf("order status = %q, want cancelled", got)
	}
}

// -------------------------------------------------------------- termination --

// A service suspended for longer than the threshold is terminated through the queue: the
// service row is untouched, an operation is queued, the invoice is voided and the customer is
// told.
func TestSuspendedServiceIsTerminatedThroughTheQueue(t *testing.T) {
	f := newFixture(t)
	serviceID, invoiceID := f.suspendedService(t, baseTime.AddDate(0, 0, -40))

	run, err := f.runner().TerminateSuspended(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Terminated) != 1 || run.Terminated[0].ServiceID != serviceID {
		t.Fatalf("terminated = %+v, want exactly %s", run.Terminated, serviceID)
	}
	if run.Days != 30 {
		t.Fatalf("days = %d, want the configured default 30", run.Days)
	}

	// The service row is not touched by this job. The worker applies the transition when the
	// adapter call succeeds, and only then.
	var status string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status FROM services WHERE id = $1`, serviceID).Scan(&status); err != nil {
		t.Fatalf("read service: %v", err)
	}
	if status != "suspended" {
		t.Fatalf("service status = %q, want suspended: the job enqueues, it does not terminate", status)
	}

	var kind, opStatus string
	var generation int64
	if err := f.pool.QueryRow(context.Background(), `
		SELECT kind, status, generation FROM service_operations
		WHERE service_id = $1 AND kind = 'terminate'`, serviceID).
		Scan(&kind, &opStatus, &generation); err != nil {
		t.Fatalf("read terminate operation: %v", err)
	}
	if kind != "terminate" || opStatus != "queued" || generation != 1 {
		t.Fatalf("operation = %s/%s generation %d, want terminate/queued generation 1",
			kind, opStatus, generation)
	}

	if s, _ := f.invoiceStatus(t, invoiceID); s != "void" {
		t.Fatalf("invoice status = %q, want void", s)
	}

	if n := f.count(t, `
		SELECT count(*) FROM notification_deliveries
		WHERE kind = 'server_terminated'`); n != 1 {
		t.Fatalf("server_terminated deliveries = %d, want 1", n)
	}
	if n := f.count(t, `
		SELECT count(*) FROM audit_events WHERE action = 'service.terminate' AND target_id = $1`,
		serviceID); n != 1 {
		t.Fatalf("service.terminate audit rows = %d, want 1", n)
	}
}

// A manual hold is an operator's veto and outranks the rule.
func TestHeldServiceIsNotTerminated(t *testing.T) {
	f := newFixture(t)
	serviceID, invoiceID := f.suspendedService(t, baseTime.AddDate(0, 0, -40))

	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO service_holds (service_id, reason, created_by) VALUES ($1, 'dispute', 'ops')`,
		serviceID); err != nil {
		t.Fatalf("place hold: %v", err)
	}

	// dunning, which owns the definition, agrees the service is held.
	engine := dunning.NewEngine(f.pool, dunning.DefaultPolicy())
	engine.Now = func() time.Time { return baseTime }
	if _, held, err := engine.ActiveHold(ctx, serviceID); err != nil {
		t.Fatalf("dunning ActiveHold: %v", err)
	} else if !held {
		t.Fatal("dunning says the service is not held, so this test would prove nothing")
	}

	run, err := f.runner().TerminateSuspended(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Terminated) != 0 {
		t.Fatalf("terminated = %+v, want none: a hold is in place", run.Terminated)
	}
	if n := f.count(t, `
		SELECT count(*) FROM service_operations WHERE service_id = $1 AND kind = 'terminate'`,
		serviceID); n != 0 {
		t.Fatalf("terminate operations = %d, want 0", n)
	}
	if s, _ := f.invoiceStatus(t, invoiceID); s != "open" {
		t.Fatalf("invoice status = %q, want open: a held service keeps its invoice", s)
	}
}

// The threshold is a boundary, not an approximation: N-1 days is not enough.
func TestServiceSuspendedOneDayShortIsNotTerminated(t *testing.T) {
	f := newFixture(t)
	serviceID, invoiceID := f.suspendedService(t, baseTime.AddDate(0, 0, -29))

	run, err := f.runner().TerminateSuspended(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Terminated) != 0 {
		t.Fatalf("terminated = %+v, want none at 29 days", run.Terminated)
	}
	if n := f.count(t, `
		SELECT count(*) FROM service_operations WHERE service_id = $1 AND kind = 'terminate'`,
		serviceID); n != 0 {
		t.Fatalf("terminate operations = %d, want 0", n)
	}
	if s, _ := f.invoiceStatus(t, invoiceID); s != "open" {
		t.Fatalf("invoice status = %q, want open", s)
	}

	// Two more days: 31 days suspended. The boundary is exclusive — "suspended for longer
	// than 30 days" — so this is the first tick that may act. The rule ages from the
	// persisted suspension, so moving the clock is the same thing as time passing.
	later := lifecycle.New(f.pool).WithClock(func() time.Time { return baseTime.AddDate(0, 0, 2) })
	run, err = later.TerminateSuspended(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Terminated) != 1 {
		t.Fatalf("terminated = %+v at 31 days, want one", run.Terminated)
	}
}

// Running the job twice enqueues one termination, voids one invoice and sends one message.
func TestTerminationIsIdempotent(t *testing.T) {
	f := newFixture(t)
	serviceID, invoiceID := f.suspendedService(t, baseTime.AddDate(0, 0, -40))

	first, err := f.runner().TerminateSuspended(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Terminated) != 1 {
		t.Fatalf("first run terminated %d services, want 1", len(first.Terminated))
	}

	second, err := f.runner().TerminateSuspended(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Terminated) != 0 {
		t.Fatalf("second run terminated %+v, want nothing", second.Terminated)
	}
	if second.Scanned != 0 {
		t.Fatalf("second run scanned %d services, want 0: one terminate intent per service",
			second.Scanned)
	}

	if n := f.count(t, `
		SELECT count(*) FROM service_operations WHERE service_id = $1 AND kind = 'terminate'`,
		serviceID); n != 1 {
		t.Fatalf("terminate operations = %d, want 1", n)
	}
	if n := f.count(t, `SELECT count(*) FROM notification_deliveries WHERE kind = 'server_terminated'`); n != 1 {
		t.Fatalf("server_terminated deliveries = %d, want 1", n)
	}
	if n := f.count(t, `SELECT count(*) FROM audit_events WHERE action = 'service.terminate'`); n != 1 {
		t.Fatalf("service.terminate audit rows = %d, want 1", n)
	}
	if n := f.count(t, `SELECT count(*) FROM journal_transactions WHERE reference_id = $1`, invoiceID); n != 2 {
		t.Fatalf("journal transactions = %d, want 2 (issue and void reversal)", n)
	}
}

// A customer who has paid owes nothing, so the resume rule applies and the termination rule
// must stay out of the way.
func TestServiceWhoseBalanceIsClearIsNotTerminated(t *testing.T) {
	f := newFixture(t)
	serviceID, invoiceID := f.suspendedService(t, baseTime.AddDate(0, 0, -40))

	ctx := context.Background()
	if err := f.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := billing.ApplyPayment(ctx, tx, billing.AllocationRequest{
			Provider:          "stripe",
			ProviderAccount:   "acct",
			ProviderPaymentID: "pi_" + invoiceID,
			InvoiceID:         invoiceID,
			Amount:            money.New(orderTotal, usd),
		})
		return err
	}); err != nil {
		t.Fatalf("pay the invoice: %v", err)
	}

	run, err := f.runner().TerminateSuspended(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Terminated) != 0 {
		t.Fatalf("terminated = %+v, want none: nothing is owed", run.Terminated)
	}
	if n := f.count(t, `
		SELECT count(*) FROM service_operations WHERE service_id = $1 AND kind = 'terminate'`,
		serviceID); n != 0 {
		t.Fatalf("terminate operations = %d, want 0", n)
	}
}

// A kill switch stops the termination rule.
func TestTerminationKillSwitchDoesNothing(t *testing.T) {
	f := newFixture(t)
	serviceID, _ := f.suspendedService(t, baseTime.AddDate(0, 0, -40))
	t.Setenv(lifecycle.TerminationKillSwitch, "1")

	run, err := f.runner().TerminateSuspended(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if run.Reason != lifecycle.ReasonKillSwitch {
		t.Fatalf("reason = %q, want %q", run.Reason, lifecycle.ReasonKillSwitch)
	}
	if n := f.count(t, `
		SELECT count(*) FROM service_operations WHERE service_id = $1 AND kind = 'terminate'`,
		serviceID); n != 0 {
		t.Fatalf("terminate operations = %d, want 0", n)
	}

	for _, job := range lifecycle.Jobs(f.pool, nil) {
		if job.Name == lifecycle.TerminationJob {
			t.Fatal("the disabled job is still scheduled")
		}
	}
}

// A zero threshold turns the rule off rather than terminating every suspended service.
func TestTerminationWithNoThresholdActsOnNothing(t *testing.T) {
	f := newFixture(t)
	serviceID, _ := f.suspendedService(t, baseTime.Add(-time.Hour))
	f.setSetting(t, "terminate_days", "0")

	run, err := f.runner().TerminateSuspended(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if run.Reason != lifecycle.ReasonThresholdDisabled {
		t.Fatalf("reason = %q, want %q", run.Reason, lifecycle.ReasonThresholdDisabled)
	}
	if run.Scanned != 0 {
		t.Fatalf("scanned %d services, want 0", run.Scanned)
	}
	if n := f.count(t, `
		SELECT count(*) FROM service_operations WHERE service_id = $1 AND kind = 'terminate'`,
		serviceID); n != 0 {
		t.Fatalf("terminate operations = %d, want 0", n)
	}
}

// --------------------------------------------------------------------- jobs --

// Jobs returns every rule with its name and interval, which is what the worker needs.
func TestJobsAreExposed(t *testing.T) {
	f := newFixture(t)

	names := map[string]time.Duration{}
	for _, job := range lifecycle.Jobs(f.pool, nil) {
		if job.Run == nil {
			t.Fatalf("job %s has no Run", job.Name)
		}
		names[job.Name] = job.Interval
	}
	for _, want := range []string{lifecycle.OrderCancelJob, lifecycle.TerminationJob,
		lifecycle.ServiceUpgradesJob} {
		if _, ok := names[want]; !ok {
			t.Fatalf("job %s is missing from Jobs()", want)
		}
	}
	if names[lifecycle.OrderCancelJob] <= 0 || names[lifecycle.TerminationJob] <= 0 {
		t.Fatalf("intervals = %v, want both positive", names)
	}
}

// A kill switch removes the job it names from the list, which is what the worker schedules from.
//
// The assertion is per job rather than "the list is empty": the package also schedules the
// price-change queue (UPG-05/UPG-06/CRD-06), which has its own switch, and a test that required an
// empty list would either hide that job or force its switch to be set here — and a test that sets
// every switch only proves the switches parse.
func TestJobsRespectBothKillSwitches(t *testing.T) {
	f := newFixture(t)
	t.Setenv(lifecycle.OrderCancelKillSwitch, "true")
	t.Setenv(lifecycle.TerminationKillSwitch, "yes")

	for _, job := range lifecycle.Jobs(f.pool, nil) {
		if job.Name == lifecycle.OrderCancelJob || job.Name == lifecycle.TerminationJob {
			t.Fatalf("job %s is scheduled although its kill switch is set", job.Name)
		}
	}

	// And the price-change queue's own switch stops it, so an operator can stop the queue that
	// voids invoices without stopping the rules that cancel orders.
	t.Setenv(lifecycle.ServiceUpgradesKillSwitch, "true")
	for _, job := range lifecycle.Jobs(f.pool, nil) {
		if job.Name == lifecycle.ServiceUpgradesJob {
			t.Fatal("the service-upgrades job is scheduled although its kill switch is set")
		}
	}
}

// ---------------------------------------------------------------- two workers --

// Two workers running the same tick cancel an order once between them.
//
// This is the property the row lock exists for. Both runs scan the same candidate; the first
// one to take the lock cancels it, and the second, waking up after the commit, re-reads the
// status and finds nothing to do. Without the lock and the guarded update they would both
// restore stock, and the product would end up with a unit that does not exist.
func TestTwoWorkersCancelOneOrderOnce(t *testing.T) {
	f := newFixture(t)
	o := f.order(t, orderSpec{createdAt: baseTime.AddDate(0, 0, -20), total: orderTotal})

	const workers = 2
	var wg sync.WaitGroup
	cancelled := make([]int, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			run, err := f.runner().CancelUnpaidOrders(context.Background())
			cancelled[i] = len(run.Cancelled)
			errs[i] = err
		}(i)
	}
	wg.Wait()

	total := 0
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		total += cancelled[i]
	}
	if total != 1 {
		t.Fatalf("the two workers cancelled %d orders between them, want 1", total)
	}
	if got := f.orderStatus(t, o.orderID); got != "cancelled" {
		t.Fatalf("order status = %q, want cancelled", got)
	}
	if got := f.stock(t); got != 5 {
		t.Fatalf("stock = %d, want 5: the restore must happen exactly once", got)
	}
	if n := f.count(t, `SELECT count(*) FROM audit_events WHERE action = 'order.cancel'`); n != 1 {
		t.Fatalf("order.cancel audit rows = %d, want 1", n)
	}
}

// The same for termination: the enqueue is the claim, so two workers queue one operation and
// send one message.
func TestTwoWorkersTerminateOneServiceOnce(t *testing.T) {
	f := newFixture(t)
	serviceID, _ := f.suspendedService(t, baseTime.AddDate(0, 0, -40))

	const workers = 2
	var wg sync.WaitGroup
	terminated := make([]int, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			run, err := f.runner().TerminateSuspended(context.Background())
			terminated[i] = len(run.Terminated)
			errs[i] = err
		}(i)
	}
	wg.Wait()

	total := 0
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		total += terminated[i]
	}
	if total != 1 {
		t.Fatalf("the two workers enqueued %d terminations between them, want 1", total)
	}
	if n := f.count(t, `
		SELECT count(*) FROM service_operations WHERE service_id = $1 AND kind = 'terminate'`,
		serviceID); n != 1 {
		t.Fatalf("terminate operations = %d, want 1", n)
	}
	if n := f.count(t, `SELECT count(*) FROM notification_deliveries WHERE kind = 'server_terminated'`); n != 1 {
		t.Fatalf("server_terminated deliveries = %d, want 1", n)
	}
}
