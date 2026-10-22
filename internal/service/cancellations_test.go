package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/service"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// The fixtures build the chain a service actually has — customer, product, price version,
// subscription, service — rather than a shortcut, because every rule under test joins along
// exactly that chain and a shortcut would exercise a different query.

var usd = money.MustLookupCurrency("USD")

type fixture struct {
	pool       *store.Pool
	store      *service.Store
	customerID string
	userID     string
	productID  string
	priceID    string
	subID      string
	serviceID  string
	// periodStart and periodEnd are the subscription's stored billing boundaries.
	periodStart time.Time
	periodEnd   time.Time
}

// newStore builds a Store with the invoice issuer attached and a fixed clock.
func newStore(pool *store.Pool, now time.Time) *service.Store {
	s := service.New(pool)
	s.Now = func() time.Time { return now }
	s.Issuer = billing.NewIssuer(billing.FixedTaxPricer{
		Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
	})
	s.Merchant = billing.Merchant{Name: "Catalyst Hosting", CountryCode: "US"}
	s.Series = billing.DefaultSeries
	return s
}

// newFixture creates a customer with a login, a product, a price version, an active
// subscription in the middle of its period, and an active service.
//
// The period is deliberately mid-flight rather than already ended: an ended period would make
// "the paid-through date is in the future" trivially false and hide the difference between a
// scheduled cancellation and a due one.
func newFixture(t *testing.T, amountMinor int64, interval string) *fixture {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	return newFixtureWithPeriod(t, amountMinor, interval, now, now.AddDate(0, 0, -15), now.AddDate(0, 0, 15))
}

// newFixtureWithPeriod is newFixture with an explicit billing window and clock.
func newFixtureWithPeriod(t *testing.T, amountMinor int64, interval string,
	now, periodStart, periodEnd time.Time) *fixture {
	t.Helper()
	pool := storetest.DB(t)
	ctx := context.Background()

	f := &fixture{pool: pool, periodStart: periodStart, periodEnd: periodEnd}
	f.store = newStore(pool, now)

	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO customers (email, name) VALUES ($1, 'Test Customer') RETURNING id`,
			"svc-"+now.Format("150405.000000000")+"@example.test").Scan(&f.customerID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO users (email, password_hash, email_verified_at)
			VALUES ($1, 'x', now()) RETURNING id`,
			"svc-user-"+now.Format("150405.000000000")+"@example.test").Scan(&f.userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO customer_accounts (user_id, customer_id) VALUES ($1, $2)`,
			f.userID, f.customerID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO products (slug, name) VALUES ($1, 'VPS Small') RETURNING id`,
			"vps-"+now.Format("150405.000000000")).Scan(&f.productID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO price_versions (product_id, version, name, currency, amount_minor, interval,
				anchor_day, anchor_timezone, provision_plan, provision_region)
			VALUES ($1, 1, 'Small', 'USD', $2, $3, 1, 'UTC', 'plan-small', 'eu-west')
			RETURNING id`, f.productID, amountMinor, interval).Scan(&f.priceID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO subscriptions (customer_id, price_version_id, status, interval, anchor_day,
				anchor_hour, anchor_minute, timezone, current_period_start, current_period_end,
				next_billing_at)
			VALUES ($1, $2, 'active', $3, 1, 0, 0, 'UTC', $4, $5, $5)
			RETURNING id`,
			f.customerID, f.priceID, interval, f.periodStart, f.periodEnd).Scan(&f.subID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO services (customer_id, subscription_id, product_id, status, external_id)
			VALUES ($1, $2, $3, 'active', 'ext-123') RETURNING id`,
			f.customerID, f.subID, f.productID).Scan(&f.serviceID)
	})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return f
}

// countRows counts rows in a table, for asserting "exactly one" of something.
func countRows(t *testing.T, pool *store.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// ------------------------------------------------------------ cancellation --

// An end-of-period cancellation sets cancel_at_period_end, records a scheduled row with the
// paid-through date, and terminates nothing. That last part is the property that matters: a
// customer who has paid for a month must keep the month.
func TestEndOfPeriodCancellationKeepsTheService(t *testing.T) {
	f := newFixture(t, 1000, "monthly")

	out, err := f.store.RequestCancellation(context.Background(), service.CancellationRequest{
		ServiceID: f.serviceID,
		UserID:    f.userID,
		Reason:    "moving to another provider",
		Type:      service.CancelEndOfPeriod,
	})
	if err != nil {
		t.Fatalf("request cancellation: %v", err)
	}

	if out.Status != service.StatusScheduled {
		t.Fatalf("status = %q, want scheduled", out.Status)
	}
	if out.EffectiveAt == nil || !out.EffectiveAt.Equal(f.periodEnd) {
		t.Fatalf("effective_at = %v, want the paid-through date %v", out.EffectiveAt, f.periodEnd)
	}

	var (
		cancelAtEnd bool
		status      string
		reason      string
	)
	if err := f.pool.QueryRow(context.Background(), `
		SELECT sub.cancel_at_period_end, s.status, c.reason
		FROM subscriptions sub
		JOIN services s ON s.subscription_id = sub.id
		JOIN service_cancellations c ON c.service_id = s.id
		WHERE sub.id = $1`, f.subID).Scan(&cancelAtEnd, &status, &reason); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !cancelAtEnd {
		t.Fatal("cancel_at_period_end is false; the renewal engine would bill again")
	}
	if status != "active" {
		t.Fatalf("service status = %q, want active: an end-of-period request must not stop the service", status)
	}
	if reason != "moving to another provider" {
		t.Fatalf("reason = %q", reason)
	}

	// Nothing was queued: an end-of-period request changes no infrastructure.
	if n := countRows(t, f.pool, "service_operations"); n != 0 {
		t.Fatalf("service_operations has %d rows; an end-of-period request must enqueue nothing", n)
	}
}

// An immediate cancellation records a pending request and terminates nothing by itself.
func TestImmediateCancellationIsOnlyARequest(t *testing.T) {
	f := newFixture(t, 1000, "monthly")

	out, err := f.store.RequestCancellation(context.Background(), service.CancellationRequest{
		ServiceID: f.serviceID,
		UserID:    f.userID,
		Reason:    "please delete it now",
		Type:      service.CancelImmediate,
	})
	if err != nil {
		t.Fatalf("request cancellation: %v", err)
	}
	if out.Status != service.StatusPending {
		t.Fatalf("status = %q, want pending", out.Status)
	}

	var (
		status        string
		cancelAtEnd   bool
		operationRows int
	)
	if err := f.pool.QueryRow(context.Background(), `
		SELECT s.status, sub.cancel_at_period_end
		FROM services s JOIN subscriptions sub ON sub.id = s.subscription_id
		WHERE s.id = $1`, f.serviceID).Scan(&status, &cancelAtEnd); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "active" {
		t.Fatalf("service status = %q; an immediate request must not terminate anything itself", status)
	}
	if cancelAtEnd {
		t.Fatal("cancel_at_period_end was set by an immediate request; the schedule must be untouched until execution")
	}
	if operationRows = countRows(t, f.pool, "service_operations"); operationRows != 0 {
		t.Fatalf("service_operations has %d rows; the customer's request must enqueue nothing", operationRows)
	}
}

// Executing an immediate cancellation enqueues exactly one terminate operation, and a second
// execution is refused rather than enqueuing a second.
func TestExecuteImmediateCancellationEnqueuesOneTerminate(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()

	req, err := f.store.RequestCancellation(ctx, service.CancellationRequest{
		ServiceID: f.serviceID, UserID: f.userID, Reason: "now", Type: service.CancelImmediate,
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	res, err := f.store.ExecuteCancellation(ctx, req.ID, "", "operator decided")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.OperationID == "" {
		t.Fatal("no operation id was returned")
	}
	if res.AlreadyTerminated {
		t.Fatal("the service was reported already terminated")
	}

	var (
		kind   string
		status string
		n      int
	)
	if err := f.pool.QueryRow(ctx, `
		SELECT kind, status, count(*) OVER () FROM service_operations`).
		Scan(&kind, &status, &n); err != nil {
		t.Fatalf("read operation: %v", err)
	}
	if n != 1 {
		t.Fatalf("service_operations has %d rows, want exactly 1", n)
	}
	if kind != "terminate" {
		t.Fatalf("operation kind = %q, want terminate", kind)
	}
	if status != "queued" {
		t.Fatalf("operation status = %q, want queued", status)
	}

	// The request is executed and the schedule stops, so the service cannot renew while the
	// termination is in flight.
	var cancelAtEnd bool
	if err := f.pool.QueryRow(ctx,
		`SELECT cancel_at_period_end FROM subscriptions WHERE id = $1`, f.subID).Scan(&cancelAtEnd); err != nil {
		t.Fatalf("read schedule: %v", err)
	}
	if !cancelAtEnd {
		t.Fatal("billing was not stopped by the execution")
	}

	// A second execution is refused: the row is no longer pending.
	if _, err := f.store.ExecuteCancellation(ctx, req.ID, "", "again"); err == nil {
		t.Fatal("a second execution succeeded; it must be refused as not_pending")
	}
	if got := countRows(t, f.pool, "service_operations"); got != 1 {
		t.Fatalf("service_operations has %d rows after a second execute, want 1", got)
	}
}

// Rejecting an end-of-period request clears the schedule it set. Without that, the queue would
// say rejected while the renewal engine quietly ended the subscription.
func TestRejectingEndOfPeriodClearsTheSchedule(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()

	req, err := f.store.RequestCancellation(ctx, service.CancellationRequest{
		ServiceID: f.serviceID, UserID: f.userID, Reason: "changed my mind later", Type: service.CancelEndOfPeriod,
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := f.store.RejectCancellation(ctx, req.ID, "", "customer asked to stay"); err != nil {
		t.Fatalf("reject: %v", err)
	}

	var cancelAtEnd bool
	var status string
	if err := f.pool.QueryRow(ctx, `
		SELECT sub.cancel_at_period_end, c.status
		FROM subscriptions sub
		JOIN service_cancellations c ON c.service_id = $2
		WHERE sub.id = $1`, f.subID, f.serviceID).Scan(&cancelAtEnd, &status); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if cancelAtEnd {
		t.Fatal("cancel_at_period_end is still true after the request was rejected")
	}
	if status != service.StatusRejected {
		t.Fatalf("status = %q, want rejected", status)
	}
}

// A customer cannot cancel another customer's service: the scoping is in the query, so the
// answer is the same 404 a nonexistent identifier produces.
func TestCancellationIsScopedToTheCustomer(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()

	var otherUser string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, email_verified_at)
		VALUES ('other@example.test', 'x', now()) RETURNING id`).Scan(&otherUser); err != nil {
		t.Fatalf("other user: %v", err)
	}

	_, err := f.store.RequestCancellation(ctx, service.CancellationRequest{
		ServiceID: f.serviceID, UserID: otherUser, Reason: "not mine", Type: service.CancelEndOfPeriod,
	})
	if err == nil {
		t.Fatal("another customer's cancellation succeeded")
	}
	if err != service.ErrNotFound {
		t.Fatalf("error = %v, want ErrNotFound so the id is indistinguishable from a missing one", err)
	}
	if n := countRows(t, f.pool, "service_cancellations"); n != 0 {
		t.Fatalf("%d cancellation rows exist after a refused request", n)
	}
}

// A second open request is refused rather than silently replacing the first.
func TestSecondCancellationRequestIsRefused(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()

	if _, err := f.store.RequestCancellation(ctx, service.CancellationRequest{
		ServiceID: f.serviceID, UserID: f.userID, Reason: "first", Type: service.CancelEndOfPeriod,
	}); err != nil {
		t.Fatalf("first request: %v", err)
	}
	_, err := f.store.RequestCancellation(ctx, service.CancellationRequest{
		ServiceID: f.serviceID, UserID: f.userID, Reason: "second", Type: service.CancelImmediate,
	})
	if err == nil {
		t.Fatal("a second open request succeeded")
	}
	if n := countRows(t, f.pool, "service_cancellations"); n != 1 {
		t.Fatalf("%d cancellation rows, want 1", n)
	}
}

// ExecuteDue executes only the scheduled cancellations whose date has passed.
func TestExecuteDueRunsOnlyDueScheduledCancellations(t *testing.T) {
	f := newFixture(t, 1000, "monthly")

	// A scheduled request whose effective date has passed.
	req, err := f.store.RequestCancellation(context.Background(), service.CancellationRequest{
		ServiceID: f.serviceID, UserID: f.userID, Reason: "end", Type: service.CancelEndOfPeriod,
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	// The clock is still before the effective date: nothing is due.
	results, err := f.store.ExecuteDue(context.Background())
	if err != nil {
		t.Fatalf("execute due: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("execute due acted on %d cancellations before their effective date", len(results))
	}

	// Move the clock past the paid-through date and run again.
	after := f.periodEnd.Add(time.Hour)
	f.store.Now = func() time.Time { return after }

	results, err = f.store.ExecuteDue(context.Background())
	if err != nil {
		t.Fatalf("execute due: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("execute due acted on %d cancellations, want 1", len(results))
	}
	if results[0].CancellationID != req.ID {
		t.Fatalf("executed %s, want %s", results[0].CancellationID, req.ID)
	}
	if results[0].OperationID == "" {
		t.Fatal("no terminate operation was enqueued")
	}

	// A second run does nothing: the row is executed, not scheduled.
	again, err := f.store.ExecuteDue(context.Background())
	if err != nil {
		t.Fatalf("execute due again: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("a second run acted on %d cancellations, want 0", len(again))
	}
	if n := countRows(t, f.pool, "service_operations"); n != 1 {
		t.Fatalf("%d operations, want exactly 1", n)
	}
}

// The staff queue filters by status and defaults to the open requests.
func TestCancellationQueueFiltersByStatus(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()

	if _, err := f.store.RequestCancellation(ctx, service.CancellationRequest{
		ServiceID: f.serviceID, UserID: f.userID, Reason: "open", Type: service.CancelEndOfPeriod,
	}); err != nil {
		t.Fatalf("request: %v", err)
	}

	open, err := f.store.Cancellations(ctx, service.CancellationFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("the default queue returned %d rows, want 1", len(open))
	}
	if open[0].CustomerName == "" || open[0].ProductName == "" {
		t.Fatal("the queue row is missing the customer or product name it joins")
	}

	executed, err := f.store.Cancellations(ctx, service.CancellationFilter{Status: service.StatusExecuted})
	if err != nil {
		t.Fatalf("list executed: %v", err)
	}
	if len(executed) != 0 {
		t.Fatalf("the executed filter returned %d rows, want 0", len(executed))
	}
}

// A reason longer than the column allows is refused with a domain error rather than a
// constraint violation surfacing as a 500.
func TestCancellationReasonIsBounded(t *testing.T) {
	f := newFixture(t, 1000, "monthly")

	long := make([]byte, service.MaxReasonLength+1)
	for i := range long {
		long[i] = 'x'
	}
	_, err := f.store.RequestCancellation(context.Background(), service.CancellationRequest{
		ServiceID: f.serviceID, UserID: f.userID, Reason: string(long), Type: service.CancelEndOfPeriod,
	})
	if err == nil {
		t.Fatal("an over-long reason was accepted")
	}
}

// An unknown type is refused: the set is closed by a database CHECK, and a caller should get a
// clear error rather than a constraint violation.
func TestCancellationTypeIsClosed(t *testing.T) {
	f := newFixture(t, 1000, "monthly")

	_, err := f.store.RequestCancellation(context.Background(), service.CancellationRequest{
		ServiceID: f.serviceID, UserID: f.userID, Reason: "x", Type: "whenever",
	})
	if err == nil {
		t.Fatal("an unknown cancellation type was accepted")
	}
}

// A terminated service cannot be cancelled again.
func TestTerminatedServiceIsNotCancellable(t *testing.T) {
	f := newFixture(t, 1000, "monthly")
	ctx := context.Background()

	if _, err := f.pool.Exec(ctx, `UPDATE services SET status = 'terminated' WHERE id = $1`,
		f.serviceID); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	_, err := f.store.RequestCancellation(ctx, service.CancellationRequest{
		ServiceID: f.serviceID, UserID: f.userID, Reason: "x", Type: service.CancelEndOfPeriod,
	})
	if err == nil {
		t.Fatal("a terminated service accepted a cancellation request")
	}
}
