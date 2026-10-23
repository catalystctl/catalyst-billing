package operations_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/operations"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/settings"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/catalyst/billing/internal/updates"
	"github.com/jackc/pgx/v5"
)

// The two maintenance jobs (OPS-01, OPS-13).
//
// Both features already existed and were reachable only by a human pressing a button: the
// off-session charge sweep and the telemetry send. These tests are about the *scheduled* half —
// which invoices a tick charges, when it declines to re-offer a card, and that a payload is
// really delivered once a day — because that is the half that was missing.

// ---------------------------------------------------------------- fixtures --

type fixture struct {
	pool       *store.Pool
	customerID string
	invoiceID  string
	methodID   string
	now        time.Time
}

// feeCharger is a fake off-session adapter that succeeds.
type feeCharger struct {
	mu    sync.Mutex
	calls int
}

func (c *feeCharger) Name() string { return "fakegateway" }
func (c *feeCharger) ChargeOffSession(_ context.Context, req payments.OffSessionCharge) (payments.OffSessionResult, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return payments.OffSessionResult{
		ProviderPaymentID: "pi_ops_1",
		Status:            "succeeded",
		AmountMinor:       req.AmountMinor,
		FeeMinor:          30,
	}, nil
}
func (c *feeCharger) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// decliningCharger refuses every charge, which is what a real expired card does.
type decliningCharger struct {
	mu    sync.Mutex
	calls int
}

func (c *decliningCharger) Name() string { return "fakegateway" }
func (c *decliningCharger) ChargeOffSession(_ context.Context, _ payments.OffSessionCharge) (payments.OffSessionResult, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return payments.OffSessionResult{Status: "declined", FailureCode: "card_declined",
		FailureMessage: "the card was declined"}, nil
}
func (c *decliningCharger) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// newFixture builds a customer with an open, past-due invoice, a default saved card and an active
// mandate for it — the state a renewal leaves behind once the customer has saved a card.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := storetest.DB(t)
	ctx := context.Background()
	f := &fixture{pool: pool, now: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)}
	due := f.now.AddDate(0, 0, -1)

	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO customers (email, name) VALUES ('ops@example.test', 'Ops') RETURNING id`,
		).Scan(&f.customerID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO payment_methods (customer_id, provider, provider_token, brand, last4, is_default)
			VALUES ($1, 'fakegateway', 'pm_ops_TOKEN', 'visa', '4242', true) RETURNING id`,
			f.customerID).Scan(&f.methodID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO billing_agreements (customer_id, payment_method_id, provider, status, consent_at)
			VALUES ($1, $2, 'fakegateway', 'active', now())`, f.customerID, f.methodID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO invoices (customer_id, number, status, currency, subtotal_minor, tax_minor,
				total_minor, amount_paid_minor, issued_at, due_at, snapshot)
			VALUES ($1, 'INV-OPS-1', 'open', 'USD', 1200, 0, 1200, 0, now(), $2,
			        '{"schema_version":1}'::jsonb)
			RETURNING id`, f.customerID, due).Scan(&f.invoiceID)
	}); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return f
}

// set writes one setting the way the settings screen does.
func set(t *testing.T, pool *store.Pool, group, key, value string) {
	t.Helper()
	if _, err := settings.New(pool).Set(context.Background(), group, key, value, "", nil); err != nil {
		t.Fatalf("set %s.%s: %v", group, key, err)
	}
}

// chargeOptions points the job at a fixed clock and a fake adapter.
func chargeOptions(charger payments.OffSessionCharger) operations.Options {
	return operations.Options{
		ChargerOverride: func(context.Context) (payments.OffSessionCharger, string, func(), error) {
			return charger, "acct_test", func() {}, nil
		},
	}
}

// ---------------------------------------------------------------- charging --

// Nothing is charged while `gateway.off_session_enabled` is off, and the run says so rather than
// failing: this is the default state of an installation.
func TestChargeDueSkipsWhenOffSessionIsOff(t *testing.T) {
	f := newFixture(t)
	charger := &feeCharger{}

	run, err := operations.ChargeDue(context.Background(), f.pool, chargeOptions(charger))
	if err != nil {
		t.Fatalf("charge due: %v", err)
	}
	if run.Reason != operations.ReasonDisabled {
		t.Fatalf("reason = %q, want %q", run.Reason, operations.ReasonDisabled)
	}
	if charger.callCount() != 0 {
		t.Fatalf("the adapter was called %d time(s) with the setting off", charger.callCount())
	}
}

// A due invoice with a default card and an active mandate is charged, and the invoice is settled
// through the ordinary settlement path — the same one a webhook uses.
func TestChargeDueChargesADueInvoice(t *testing.T) {
	f := newFixture(t)
	set(t, f.pool, "gateway", "off_session_enabled", "true")
	charger := &feeCharger{}

	run, err := operations.ChargeDue(context.Background(), f.pool, chargeOptions(charger))
	if err != nil {
		t.Fatalf("charge due: %v", err)
	}
	if len(run.Outcomes) != 1 || run.Outcomes[0].Status != payments.ChargeSettled {
		t.Fatalf("outcomes = %+v, want one settled charge", run.Outcomes)
	}
	if run.Settled != 1 || run.Declined != 0 {
		t.Fatalf("settled/declined = %d/%d, want 1/0", run.Settled, run.Declined)
	}
	if charger.callCount() != 1 {
		t.Fatalf("adapter calls = %d, want 1", charger.callCount())
	}

	var (
		status string
		paid   int64
	)
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status, amount_paid_minor FROM invoices WHERE id = $1`, f.invoiceID).
		Scan(&status, &paid); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	if status != "paid" || paid != 1200 {
		t.Fatalf("invoice = %s/%d, want paid/1200", status, paid)
	}
}

// A declined card is not re-offered inside `cronjob.charge_retry_hours`, and is offered again once
// the window has passed.
//
// Without this the ticker would re-attempt a declined card every fifteen minutes, which is how a
// customer's bank starts refusing every charge from the merchant.
func TestChargeDueDoesNotRetryADeclinedCardInsideTheWindow(t *testing.T) {
	f := newFixture(t)
	set(t, f.pool, "gateway", "off_session_enabled", "true")
	ctx := context.Background()
	charger := &decliningCharger{}

	run, err := operations.ChargeDue(ctx, f.pool, chargeOptions(charger))
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if run.Declined != 1 || charger.callCount() != 1 {
		t.Fatalf("first sweep: declined=%d calls=%d, want 1/1", run.Declined, charger.callCount())
	}

	// The next tick: the decline is inside the window, so the invoice is not offered again.
	run, err = operations.ChargeDue(ctx, f.pool, chargeOptions(charger))
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if run.Reason != operations.ReasonNothingDue {
		t.Fatalf("second sweep reason = %q, want %q", run.Reason, operations.ReasonNothingDue)
	}
	if charger.callCount() != 1 {
		t.Fatalf("the adapter was called again inside the retry window (%d calls)", charger.callCount())
	}

	// Age the attempt past the window and the invoice is offered again — and refused again by
	// internal/payments, which remembers the declined attempt. That second refusal is the
	// safety property: the sweep may only ever *offer* an invoice, never force a charge, so a
	// provider is never asked twice for a card that said no. The retry window exists so the
	// ticker does not even make that offer every fifteen minutes.
	if _, err := f.pool.Exec(ctx, `
		UPDATE payment_attempts SET created_at = now() - interval '48 hours'
		WHERE invoice_id = $1`, f.invoiceID); err != nil {
		t.Fatalf("age the attempt: %v", err)
	}
	run, err = operations.ChargeDue(ctx, f.pool, chargeOptions(charger))
	if err != nil {
		t.Fatalf("third sweep: %v", err)
	}
	if len(run.Outcomes) != 1 {
		t.Fatalf("after the window: %d outcomes, want the invoice offered again", len(run.Outcomes))
	}
	if run.Outcomes[0].Status != payments.ChargeDeclined {
		t.Fatalf("outcome after the window = %+v, want a refusal", run.Outcomes[0])
	}
	if charger.callCount() != 1 {
		t.Fatalf("the provider was asked %d time(s) for a declined card, want 1", charger.callCount())
	}
}

// An invoice whose customer is in a checkout is not charged by the sweep: one payment, not two.
func TestChargeDueSkipsAnInvoiceWithAnOpenCheckout(t *testing.T) {
	f := newFixture(t)
	set(t, f.pool, "gateway", "off_session_enabled", "true")
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO payment_attempts (invoice_id, gateway, operation_key, status, amount_minor, currency)
		VALUES ($1, 'fakegateway', 'checkout:live', 'pending', 1200, 'USD')`, f.invoiceID); err != nil {
		t.Fatalf("open checkout: %v", err)
	}
	charger := &feeCharger{}

	run, err := operations.ChargeDue(ctx, f.pool, chargeOptions(charger))
	if err != nil {
		t.Fatalf("charge due: %v", err)
	}
	if run.Reason != operations.ReasonNothingDue || charger.callCount() != 0 {
		t.Fatalf("reason=%q calls=%d, want nothing_due/0", run.Reason, charger.callCount())
	}
}

// --------------------------------------------------------------- telemetry --

// A local receiver, so "what was sent" is the bytes that arrived rather than a mock's claim.
type receiver struct {
	mu     sync.Mutex
	bodies [][]byte
	status int
	srv    *httptest.Server
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{status: http.StatusNoContent}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Errorf("decode telemetry body: %v", err)
		}
		encoded, _ := json.Marshal(body)
		r.mu.Lock()
		r.bodies = append(r.bodies, encoded)
		r.mu.Unlock()
		w.WriteHeader(r.status)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

// setEndpoint stores the receiver's URL where the updates screen stores one.
func setEndpoint(t *testing.T, pool *store.Pool, url string) {
	t.Helper()
	ctx := context.Background()
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return updates.New(pool).SetTelemetryEndpoint(ctx, tx, url, "")
	}); err != nil {
		t.Fatalf("set telemetry endpoint: %v", err)
	}
}

// Nothing is delivered while `updates.telemetry` is off, whatever endpoint is configured.
func TestTelemetryNotSentWhileOff(t *testing.T) {
	pool := storetest.DB(t)
	recv := newReceiver(t)
	setEndpoint(t, pool, recv.srv.URL)

	run, err := operations.SendTelemetry(context.Background(), pool, operations.Options{Version: "test"})
	if err != nil {
		t.Fatalf("send telemetry: %v", err)
	}
	if run.Reason != operations.ReasonDisabled {
		t.Fatalf("reason = %q, want %q", run.Reason, operations.ReasonDisabled)
	}
	if recv.count() != 0 {
		t.Fatalf("%d payload(s) were sent with telemetry off", recv.count())
	}

	var sends int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM telemetry_sends`).
		Scan(&sends); err != nil {
		t.Fatal(err)
	}
	if sends != 0 {
		t.Fatalf("%d telemetry_sends row(s) exist for a run that sent nothing", sends)
	}
}

// With telemetry on and an endpoint stored, the payload is delivered and recorded.
func TestTelemetrySentWhenEnabled(t *testing.T) {
	pool := storetest.DB(t)
	recv := newReceiver(t)
	setEndpoint(t, pool, recv.srv.URL)
	set(t, pool, "updates", "telemetry", "true")

	run, err := operations.SendTelemetry(context.Background(), pool, operations.Options{Version: "9.9.9"})
	if err != nil {
		t.Fatalf("send telemetry: %v", err)
	}
	if run.Reason != operations.ReasonSent || run.Result == nil {
		t.Fatalf("run = %+v, want a recorded send", run)
	}
	if run.Result.Status != "sent" {
		t.Fatalf("delivery status = %q, want sent", run.Result.Status)
	}
	if recv.count() != 1 {
		t.Fatalf("%d payload(s) arrived, want 1", recv.count())
	}

	// The receiver's bytes carry the running version and the fixed key set — nothing added by
	// the job, which is the property the payload's own test asserts.
	var body map[string]any
	recv.mu.Lock()
	err = json.Unmarshal(recv.bodies[0], &body)
	recv.mu.Unlock()
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if body["panel_version"] != "9.9.9" {
		t.Fatalf("panel_version = %v, want 9.9.9", body["panel_version"])
	}

	// A send that happened is a row an operator can see, with no person attached to it.
	var (
		status string
		sentBy *string
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT status, sent_by::text FROM telemetry_sends ORDER BY sent_at DESC LIMIT 1`).
		Scan(&status, &sentBy); err != nil {
		t.Fatalf("read telemetry_sends: %v", err)
	}
	if status != "sent" || sentBy != nil {
		t.Fatalf("telemetry_sends = %s/%v, want sent with no actor", status, sentBy)
	}
}

// The job list carries both rules under stable names, and a kill switch removes one job rather
// than disabling it silently.
func TestJobsAreNamedAndSwitchable(t *testing.T) {
	pool := storetest.DB(t)
	jobs := operations.Jobs(pool, operations.Options{})
	names := map[string]bool{}
	for _, job := range jobs {
		names[job.Name] = true
		if job.Interval <= 0 || job.Run == nil {
			t.Fatalf("job %q has no interval or no runner", job.Name)
		}
	}
	if !names[operations.ChargeDueJob] || !names[operations.TelemetryJob] {
		t.Fatalf("jobs = %v, want both maintenance rules", names)
	}

	t.Setenv(operations.ChargeDueKillSwitch, "1")
	for _, job := range operations.Jobs(pool, operations.Options{}) {
		if job.Name == operations.ChargeDueJob {
			t.Fatal("the charge job is still scheduled with its kill switch set")
		}
	}
}

// A restart does not become a second daily report.
//
// The worker ticks every job once on start and then on its interval, so a deployment that restarts
// an hour later would otherwise send again — the setting says "daily", and a schedule that depends
// on uptime is not one. The guard reads the recorded history, so two workers restarting together
// agree.
func TestTelemetryIsNotResentOnRestart(t *testing.T) {
	pool := storetest.DB(t)
	recv := newReceiver(t)
	setEndpoint(t, pool, recv.srv.URL)
	set(t, pool, "updates", "telemetry", "true")
	ctx := context.Background()
	opts := operations.Options{Version: "1.0.0"}

	first, err := operations.SendTelemetry(ctx, pool, opts)
	if err != nil {
		t.Fatalf("first send: %v", err)
	}
	if first.Reason != operations.ReasonSent {
		t.Fatalf("first run = %+v, want a delivery", first)
	}

	again, err := operations.SendTelemetry(ctx, pool, opts)
	if err != nil {
		t.Fatalf("second send: %v", err)
	}
	if again.Reason != operations.ReasonRecentlySent {
		t.Fatalf("second run = %+v, want %q", again, operations.ReasonRecentlySent)
	}
	if recv.count() != 1 {
		t.Fatalf("%d payload(s) were delivered on a restart, want 1", recv.count())
	}

	// Age the recorded delivery past the gap: a day later the job sends again.
	if _, err := pool.Exec(ctx, `
		UPDATE telemetry_sends SET sent_at = now() - interval '25 hours'`); err != nil {
		t.Fatalf("age the send: %v", err)
	}
	third, err := operations.SendTelemetry(ctx, pool, opts)
	if err != nil {
		t.Fatalf("third send: %v", err)
	}
	if third.Reason != operations.ReasonSent || recv.count() != 2 {
		t.Fatalf("third run = %+v after %d deliveries, want a new delivery", third, recv.count())
	}
}
