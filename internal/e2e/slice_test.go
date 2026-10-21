// Package e2e drives the whole vertical slice against real extensions: a product
// is bought, the fake gateway reports the payment through a webhook, the payment
// is applied, and the fake provisioner creates the service.
//
// These are the tests that correspond to the plan's mandatory acceptance
// scenarios 1-4. They use real processes over the real protocol, a real database,
// and no mocks of the components under test.
package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/checkout"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/fulfilment"
	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/provisioning"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

var usd = money.MustLookupCurrency("USD")

// ------------------------------------------------------------- extensions --

var (
	buildOnce sync.Once
	buildErr  error
	binDir    string
)

// buildExtensions compiles the fake gateway and provisioner once per test binary.
// They are ordinary Go programs, so they are built rather than scripted: the test
// then exercises exactly the artifact an operator would install.
func buildExtensions(t *testing.T) (gateway, prov string) {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "billing-ext-*")
		if err != nil {
			buildErr = err
			return
		}
		binDir = dir
		for _, name := range []string{"fakegateway", "fakeprov"} {
			cmd := exec.Command("go", "build", "-o", filepath.Join(dir, name),
				"github.com/catalyst/billing/extensions/"+name)
			cmd.Dir = moduleRoot()
			out, err := cmd.CombinedOutput()
			if err != nil {
				buildErr = fmt.Errorf("build %s: %v\n%s", name, err, out)
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatalf("build extensions: %v", buildErr)
	}
	return filepath.Join(binDir, "fakegateway"), filepath.Join(binDir, "fakeprov")
}

func moduleRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}

// ------------------------------------------------------------ adapters --

// gatewayAdapter wraps the extension client as a payments.Gateway and
// payments.WebhookVerifier.
type gatewayAdapter struct {
	client *extproto.Client
}

func (g gatewayAdapter) Name() string { return "fakegateway" }

func (g gatewayAdapter) QueryPayment(ctx context.Context, paymentID string) (extproto.QueryPaymentResult, error) {
	var out extproto.QueryPaymentResult
	err := g.client.Call(ctx, "gateway.query_payment", &extproto.QueryPaymentParams{PaymentID: paymentID}, &out)
	return out, err
}

func (g gatewayAdapter) VerifyWebhook(ctx context.Context, raw []byte, headers map[string]string) (extproto.VerifyWebhookResult, error) {
	var out extproto.VerifyWebhookResult
	err := g.client.Call(ctx, "gateway.verify_webhook",
		&extproto.VerifyWebhookParams{Headers: headers, Body: raw}, &out)
	return out, err
}

func (g gatewayAdapter) CreateCheckout(ctx context.Context, p extproto.CreateCheckoutParams) (extproto.CreateCheckoutResult, error) {
	var out extproto.CreateCheckoutResult
	err := g.client.Call(ctx, "gateway.create_checkout", &p, &out)
	return out, err
}

// provAdapter wraps the extension client as a provisioning.Adapter.
type provAdapter struct {
	client *extproto.Client
}

func (p provAdapter) Name() string           { return "fakeprov" }
func (p provAdapter) Capabilities() []string { return p.client.Capabilities() }

func (p provAdapter) Create(ctx context.Context, req provisioning.CreateRequest) (extproto.ProvisionCreateResult, error) {
	var out extproto.ProvisionCreateResult
	err := p.client.Call(ctx, "provisioner.create", &extproto.ProvisionCreateParams{
		MutatingMeta:   extproto.MutatingMeta{OperationKey: req.OperationKey},
		SubscriptionID: req.SubscriptionID,
		PlanID:         req.PlanID,
		Region:         req.Region,
		Quantity:       req.Quantity,
		Metadata:       req.Metadata,
	}, &out)
	return out, err
}

func (p provAdapter) Lookup(ctx context.Context, operationKey string) (extproto.ProvisionLookupResult, error) {
	var out extproto.ProvisionLookupResult
	err := p.client.Call(ctx, extproto.MethodProvisionerLookup,
		&extproto.ProvisionLookupParams{OperationKey: operationKey}, &out)
	return out, err
}

func (p provAdapter) Inspect(ctx context.Context, externalID string) (extproto.ProvisionInspectResult, error) {
	var out extproto.ProvisionInspectResult
	err := p.client.Call(ctx, "provisioner.inspect",
		&extproto.ProvisionInspectParams{ExternalID: externalID}, &out)
	return out, err
}

func (p provAdapter) Suspend(ctx context.Context, externalID, reason string) (extproto.ProvisionStateResult, error) {
	var out extproto.ProvisionStateResult
	err := p.client.Call(ctx, "provisioner.suspend", &extproto.ProvisionSuspendParams{
		MutatingMeta: extproto.MutatingMeta{OperationKey: "suspend:" + externalID},
		ExternalID:   externalID, Reason: reason,
	}, &out)
	return out, err
}

func (p provAdapter) Resume(ctx context.Context, externalID string) (extproto.ProvisionStateResult, error) {
	var out extproto.ProvisionStateResult
	err := p.client.Call(ctx, "provisioner.resume", &extproto.ProvisionResumeParams{
		MutatingMeta: extproto.MutatingMeta{OperationKey: "resume:" + externalID},
		ExternalID:   externalID,
	}, &out)
	return out, err
}

func (p provAdapter) Terminate(ctx context.Context, externalID, reason string) (extproto.ProvisionStateResult, error) {
	var out extproto.ProvisionStateResult
	err := p.client.Call(ctx, "provisioner.terminate", &extproto.ProvisionTerminateParams{
		MutatingMeta: extproto.MutatingMeta{OperationKey: "terminate:" + externalID},
		ExternalID:   externalID, Reason: reason,
	}, &out)
	return out, err
}

// ---------------------------------------------------------------- world --

type world struct {
	pool       *store.Pool
	gateway    gatewayAdapter
	prov       provAdapter
	inbox      *payments.Inbox
	processor  *payments.Processor
	worker     *provisioning.Worker
	customerID string
	orderID    string
	priceID    string
	invoiceID  string
	seq        atomic.Int64
}

// newWorld starts both extensions and prepares a customer with one order.
func newWorld(t *testing.T, gatewayMode, provMode string) *world {
	t.Helper()
	gatewayBin, provBin := buildExtensions(t)

	ctx := context.Background()
	gwClient, err := extproto.Launch(ctx, extproto.Spec{
		Path: gatewayBin,
		Env:  []string{"FAKE_GATEWAY_MODE=" + gatewayMode},
	})
	if err != nil {
		t.Fatalf("launch gateway: %v", err)
	}
	t.Cleanup(func() { _ = gwClient.Close() })

	provClient, err := extproto.Launch(ctx, extproto.Spec{
		Path: provBin,
		Env:  []string{"FAKE_PROV_MODE=" + provMode},
	})
	if err != nil {
		t.Fatalf("launch provisioner: %v", err)
	}
	t.Cleanup(func() { _ = provClient.Close() })

	w := &world{
		pool:    storetest.DB(t),
		gateway: gatewayAdapter{client: gwClient},
		prov:    provAdapter{client: provClient},
	}
	w.inbox = payments.NewInbox(w.pool)
	w.processor = payments.NewProcessor(w.pool, w.inbox, w.gateway, "acct_test",
		fulfilment.NewApplier(w.pool)).WithAttempts(checkout.LookupFor(w.pool))
	w.worker = provisioning.NewWorker(w.pool, w.prov)

	err = w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		n := w.seq.Add(1)
		if err := tx.QueryRow(ctx,
			`INSERT INTO customers (email, name) VALUES ($1, 'Buyer') RETURNING id`,
			fmt.Sprintf("buyer+%d@example.test", n)).Scan(&w.customerID); err != nil {
			return err
		}
		var productID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO products (slug, name) VALUES ($1, 'VPS Small') RETURNING id`,
			fmt.Sprintf("vps-%d", n)).Scan(&productID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO price_versions (product_id, version, currency, amount_minor, interval, anchor_day, anchor_timezone, provision_plan, provision_region)
			VALUES ($1, 1, 'USD', 1200, 'monthly', 1, 'UTC', 'small', 'eu-west') RETURNING id`, productID).Scan(&w.priceID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO orders (customer_id, status, currency, total_minor)
			VALUES ($1, 'pending_payment', 'USD', 1200) RETURNING id`, w.customerID).Scan(&w.orderID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO order_items (order_id, price_version_id, description, quantity, unit_minor, line_minor, position)
			VALUES ($1, $2, 'VPS Small monthly', 1, 1200, 1200, 1)`, w.orderID, w.priceID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fixtures: %v", err)
	}

	// Issue the invoice the customer is paying.
	issuer := billing.NewIssuer(billing.FixedTaxPricer{
		Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
	})
	err = w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		id, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: w.customerID,
			OrderID:    w.orderID,
			Lines: []billing.Line{{
				Description: "VPS Small monthly", QuantityMilli: 1000,
				UnitPrice: money.New(1200, usd),
			}},
			Merchant: billing.Merchant{Name: "Catalyst Hosting Ltd", CountryCode: "GB"},
			Customer: billing.CustomerDetails{ID: w.customerID, Name: "Buyer", Email: "buyer@example.test"},
			Terms:    "Renews monthly.",
		})
		if err != nil {
			return err
		}
		issued, err := issuer.Issue(ctx, tx, id, billing.DefaultSeries)
		if err != nil {
			return err
		}
		w.invoiceID = issued.InvoiceID
		return nil
	})
	if err != nil {
		t.Fatalf("issue invoice: %v", err)
	}
	return w
}

// checkout starts a gateway session through the real checkout service, so the
// attempt row that amount verification depends on is created exactly as it is in
// production.
func (w *world) checkout(t *testing.T) checkout.Session {
	t.Helper()
	svc := checkout.New(w.pool, w.gateway, "acct_test", "", "")
	session, err := svc.Start(context.Background(), w.invoiceID)
	if err != nil {
		t.Fatalf("create checkout: %v", err)
	}
	return session
}

// paymentID returns the provider payment id recorded on the invoice's attempt.
func (w *world) paymentID(t *testing.T) string {
	t.Helper()
	var ref string
	if err := w.pool.QueryRow(context.Background(),
		`SELECT provider_ref FROM payment_attempts WHERE invoice_id = $1`, w.invoiceID).Scan(&ref); err != nil {
		t.Fatalf("look up provider ref: %v", err)
	}
	return ref
}

// completePayment simulates the customer finishing the hosted checkout: the
// provider marks the payment as settled. The core must still verify that state
// itself rather than trusting the webhook body.
func (w *world) completePayment(t *testing.T, paymentID string) {
	t.Helper()
	var out map[string]any
	if err := w.gateway.client.Call(context.Background(), "fixture.complete_payment",
		map[string]string{"payment_id": paymentID}, &out); err != nil {
		t.Fatalf("complete payment: %v", err)
	}
}

// deliverWebhook runs the ingest path: verify, then persist.
func (w *world) deliverWebhook(t *testing.T, paymentID, eventID string) (accepted bool) {
	t.Helper()
	body := []byte(fmt.Sprintf(
		`{"event_id":%q,"event_type":"payment.succeeded","payment_id":%q,"amount":{"minor":1200,"currency":"USD"}}`,
		eventID, paymentID))
	headers := map[string]string{"X-Signature": "sha256=fake"}

	ev, err := payments.ExtensionVerifier{Gateway: w.gateway}.Verify(context.Background(), body, headers)
	if err != nil {
		t.Fatalf("verify webhook: %v", err)
	}
	accepted, err = w.inbox.Receive(context.Background(), "fakegateway", "acct_test", ev, body)
	if err != nil {
		t.Fatalf("receive webhook: %v", err)
	}
	return accepted
}

// ---------------------------------------------------------------- tests --

// Acceptance scenario 1: a replayed webhook 100 times produces one payment
// allocation and one logical provisioning operation.
func TestVerticalSliceReplayedWebhook(t *testing.T) {
	w := newWorld(t, "ok", "ok")
	w.checkout(t)
	w.completePayment(t, w.paymentID(t))

	// The provider delivers the same event 100 times, as a retrying provider does.
	newEvents := 0
	for i := 0; i < 100; i++ {
		if w.deliverWebhook(t, w.paymentID(t), "evt_1") {
			newEvents++
		}
	}
	if newEvents != 1 {
		t.Fatalf("%d deliveries were treated as new, want 1", newEvents)
	}

	// Processing is also idempotent: run it repeatedly.
	for i := 0; i < 3; i++ {
		if _, err := w.processor.Process(context.Background(), 100); err != nil {
			t.Fatalf("process run %d: %v", i, err)
		}
	}

	ctx := context.Background()
	var paymentsCount, allocations, ops, services int
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM payments`).Scan(&paymentsCount); err != nil {
		t.Fatal(err)
	}
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM payment_allocations`).Scan(&allocations); err != nil {
		t.Fatal(err)
	}
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM service_operations`).Scan(&ops); err != nil {
		t.Fatal(err)
	}
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM services`).Scan(&services); err != nil {
		t.Fatal(err)
	}
	if paymentsCount != 1 || allocations != 1 || ops != 1 || services != 1 {
		t.Fatalf("payments=%d allocations=%d operations=%d services=%d, want 1/1/1/1",
			paymentsCount, allocations, ops, services)
	}

	// The invoice is settled and the ledger balances.
	var status string
	if err := w.pool.QueryRow(ctx, `SELECT status FROM invoices WHERE id = $1`, w.invoiceID).
		Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "paid" {
		t.Fatalf("invoice status = %s, want paid", status)
	}
	tb, err := ledger.TrialBalance(ctx, w.pool)
	if err != nil {
		t.Fatal(err)
	}
	for code, net := range tb {
		if !net.IsZero() {
			t.Fatalf("trial balance for %s = %s, want 0", code, net)
		}
	}

	// The provisioning worker creates the service.
	results, err := w.worker.Run(ctx, 10)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if len(results) != 1 || results[0].Outcome != "succeeded" {
		t.Fatalf("provisioning results = %+v, want one success", results)
	}
	var svcStatus, externalID string
	if err := w.pool.QueryRow(ctx, `SELECT status, external_id FROM services`).
		Scan(&svcStatus, &externalID); err != nil {
		t.Fatal(err)
	}
	if svcStatus != "active" || externalID == "" {
		t.Fatalf("service status=%s external_id=%q, want active with an external id",
			svcStatus, externalID)
	}
}

// Acceptance scenario 2: two distinct events about the same payment cannot
// double-credit the customer.
func TestTwoEventsForOnePayment(t *testing.T) {
	w := newWorld(t, "ok", "ok")
	w.checkout(t)
	w.completePayment(t, w.paymentID(t))

	if !w.deliverWebhook(t, w.paymentID(t), "evt_a") {
		t.Fatal("first event was not accepted")
	}
	if !w.deliverWebhook(t, w.paymentID(t), "evt_b") {
		t.Fatal("second, distinct event was not accepted")
	}

	// Both events are processed; the second must observe the payment as already
	// applied rather than crediting again.
	results, err := w.processor.Process(context.Background(), 10)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("%d events processed, want 2", len(results))
	}
	duplicates := 0
	for _, r := range results {
		if r.Duplicate {
			duplicates++
		}
	}
	if duplicates != 1 {
		t.Fatalf("%d duplicate applications, want exactly 1", duplicates)
	}

	ctx := context.Background()
	var paid int64
	if err := w.pool.QueryRow(ctx, `SELECT amount_paid_minor FROM invoices WHERE id = $1`, w.invoiceID).
		Scan(&paid); err != nil {
		t.Fatal(err)
	}
	if paid != 1200 {
		t.Fatalf("invoice paid %d, want 1200", paid)
	}
	var allocations int
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM payment_allocations`).Scan(&allocations); err != nil {
		t.Fatal(err)
	}
	if allocations != 1 {
		t.Fatalf("%d allocations, want 1", allocations)
	}
}

// Acceptance scenario 3: the payment succeeds remotely but the process crashes
// before the local commit. Reconciliation records it once without recharging.
func TestCrashBeforeCommitRecordsOnce(t *testing.T) {
	w := newWorld(t, "ok", "ok")
	w.checkout(t)
	w.completePayment(t, w.paymentID(t))

	// Simulate the crash: the event is durably ingested, then the process dies
	// before applying it. Nothing is applied here.
	if !w.deliverWebhook(t, w.paymentID(t), "evt_crash") {
		t.Fatal("event was not ingested")
	}

	// A restarted worker processes the pending event. It must find the payment at
	// the provider and record it exactly once -- without creating a new charge.
	results, err := w.processor.Process(context.Background(), 10)
	if err != nil {
		t.Fatalf("process after restart: %v", err)
	}
	if len(results) != 1 || results[0].Duplicate {
		t.Fatalf("results = %+v, want one fresh application", results)
	}

	// A second restart finds nothing pending and changes nothing.
	if _, err := w.processor.Process(context.Background(), 10); err != nil {
		t.Fatalf("second process: %v", err)
	}

	ctx := context.Background()
	var paymentsCount int
	var paid int64
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM payments`).Scan(&paymentsCount); err != nil {
		t.Fatal(err)
	}
	if err := w.pool.QueryRow(ctx, `SELECT amount_paid_minor FROM invoices WHERE id = $1`, w.invoiceID).
		Scan(&paid); err != nil {
		t.Fatal(err)
	}
	if paymentsCount != 1 || paid != 1200 {
		t.Fatalf("payments=%d paid=%d, want 1 and 1200", paymentsCount, paid)
	}
}

// A webhook that does not authenticate must be refused and must not be stored.
func TestInvalidSignatureRejected(t *testing.T) {
	w := newWorld(t, "ok", "ok")
	w.checkout(t)

	body := []byte(fmt.Sprintf(`{"event_id":"evt_bad","event_type":"payment.succeeded","payment_id":%q}`,
		w.paymentID(t)))
	_, err := payments.ExtensionVerifier{Gateway: w.gateway}.Verify(context.Background(), body,
		map[string]string{"X-Signature": "sha256=wrong"})
	if !errors.Is(err, payments.ErrInvalidSignature) {
		t.Fatalf("got %v, want ErrInvalidSignature", err)
	}

	var stored int
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM gateway_events`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Fatalf("%d unauthenticated events stored, want 0", stored)
	}
}

// Acceptance scenario 4: the provisioner creates the server but the reply is lost.
// A retry must discover the existing server instead of duplicating it.
func TestProvisionerTimeoutDoesNotDuplicate(t *testing.T) {
	w := newWorld(t, "ok", "create_then_timeout")
	w.checkout(t)
	w.completePayment(t, w.paymentID(t))
	w.deliverWebhook(t, w.paymentID(t), "evt_1")
	if _, err := w.processor.Process(context.Background(), 10); err != nil {
		t.Fatalf("process: %v", err)
	}

	// The first attempt creates the resource and then loses the reply.
	w.worker.CallTimeout = 300 * time.Millisecond
	results, err := w.worker.Run(context.Background(), 1)
	if err != nil {
		t.Fatalf("first provision: %v", err)
	}
	if len(results) != 1 || results[0].Outcome != "unknown" {
		t.Fatalf("first attempt = %+v, want unknown outcome", results)
	}

	// Reconciliation asks the adapter again with the same operation key; a
	// conforming adapter returns the existing resource.
	recon, err := w.worker.Reconcile(context.Background(), 10)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(recon) != 1 || recon[0].Outcome != "succeeded" {
		t.Fatalf("reconcile = %+v, want one success", recon)
	}

	ctx := context.Background()
	var svcStatus, externalID string
	if err := w.pool.QueryRow(ctx, `SELECT status, external_id FROM services`).
		Scan(&svcStatus, &externalID); err != nil {
		t.Fatal(err)
	}
	if svcStatus != "active" || externalID == "" {
		t.Fatalf("service status=%s external=%q, want active with an id", svcStatus, externalID)
	}

	// Exactly one operation, one service, one invoice.
	var ops, services int
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM service_operations`).Scan(&ops); err != nil {
		t.Fatal(err)
	}
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM services`).Scan(&services); err != nil {
		t.Fatal(err)
	}
	if ops != 1 || services != 1 {
		t.Fatalf("operations=%d services=%d, want 1/1", ops, services)
	}
}

// A stale worker must not apply an obsolete destructive intent.
func TestStaleWorkerCannotSuspendPaidService(t *testing.T) {
	w := newWorld(t, "ok", "ok")
	w.checkout(t)
	w.completePayment(t, w.paymentID(t))
	w.deliverWebhook(t, w.paymentID(t), "evt_1")
	if _, err := w.processor.Process(context.Background(), 10); err != nil {
		t.Fatalf("process: %v", err)
	}
	if _, err := w.worker.Run(context.Background(), 10); err != nil {
		t.Fatalf("provision: %v", err)
	}

	ctx := context.Background()
	var serviceID string
	if err := w.pool.QueryRow(ctx, `SELECT id FROM services`).Scan(&serviceID); err != nil {
		t.Fatal(err)
	}

	// A suspend operation is queued against generation 1 ...
	if _, err := w.pool.Exec(ctx, `
		INSERT INTO service_operations (service_id, kind, operation_key, generation, status)
		VALUES ($1, 'suspend', 'suspend:test', 1, 'queued')`, serviceID); err != nil {
		t.Fatal(err)
	}
	// ... and then the service is resumed by a payment, bumping the generation.
	if _, err := w.pool.Exec(ctx, `
		UPDATE services SET generation = generation + 1 WHERE id = $1`, serviceID); err != nil {
		t.Fatal(err)
	}

	results, err := w.worker.Run(ctx, 10)
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	if len(results) != 1 || results[0].Outcome != "needs_review" {
		t.Fatalf("results = %+v, want needs_review for a stale operation", results)
	}

	// The service is still active: the stale intent was not applied.
	var status string
	if err := w.pool.QueryRow(ctx, `SELECT status FROM services WHERE id = $1`, serviceID).
		Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("service status = %s, want active", status)
	}
}

// A gateway that cannot be reached must leave the event pending, not lost.
func TestGatewayFailureKeepsEventPending(t *testing.T) {
	w := newWorld(t, "ok", "ok")
	w.checkout(t)
	w.completePayment(t, w.paymentID(t))
	w.deliverWebhook(t, w.paymentID(t), "evt_1")

	// The gateway becomes unreachable after the event was ingested: the event must
	// stay pending with the reason recorded, so a later attempt can succeed.
	if err := w.gateway.client.Close(); err != nil {
		t.Fatalf("close gateway: %v", err)
	}

	if _, err := w.processor.Process(context.Background(), 10); err == nil {
		t.Fatal("processing should have failed while the gateway is failing")
	}

	ctx := context.Background()
	var processed *time.Time
	var lastError *string
	if err := w.pool.QueryRow(ctx,
		`SELECT processed_at, last_error FROM gateway_events`).Scan(&processed, &lastError); err != nil {
		t.Fatal(err)
	}
	if processed != nil {
		t.Fatal("event was marked processed despite the failure")
	}
	if lastError == nil || *lastError == "" {
		t.Fatal("failure reason was not recorded")
	}
}
