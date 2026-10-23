package payments_test

// Regression tests for the first real gateway checkout.
//
// The sandbox run failed with:
//
//	payments: event evt_3UTWieERFJuvmOf21CDXZJaf carries no payment id
//
// The delivery had been acknowledged and stored, the invoice was still unpaid, and the event
// was retried forever. The cause was that the processor re-derived the provider payment id by
// parsing the raw delivery for a top-level `payment_id` — the shape of this repo's own fixture
// body — while a real provider nests its payment (`data.object.id` for Stripe) and the
// verifier's normalised id had been thrown away at ingest. Every fixture-shaped test passed
// because the fixture body is the shape the processor expected.
//
// These tests therefore use a *provider-shaped* body: it has no `payment_id` anywhere at the
// top level, and the only place the payment id exists is the verifier's normalisation.

import (
	"context"
	"errors"
	"testing"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

var usd = money.MustLookupCurrency("USD")

// stripeShapedBody is a real Stripe delivery: the payment is nested, and there is no
// top-level payment_id. This is what the sandbox actually posted.
func stripeShapedBody(eventID, sessionID string) []byte {
	return []byte(`{
		"id": "` + eventID + `",
		"type": "checkout.session.completed",
		"created": 1729600000,
		"data": {"object": {
			"id": "` + sessionID + `",
			"object": "checkout.session",
			"payment_status": "paid",
			"amount_total": 1200,
			"currency": "usd",
			"payment_intent": "pi_test_1"
		}}
	}`)
}

// fakeGateway answers QueryPayment with a settled payment carrying the invoice metadata the
// core set when it created the checkout.
type fakeGateway struct {
	invoiceID string
	amount    extproto.Amount
	queried   []string
}

func (f *fakeGateway) Name() string { return "stripe" }

func (f *fakeGateway) QueryPayment(_ context.Context, paymentID string) (extproto.QueryPaymentResult, error) {
	f.queried = append(f.queried, paymentID)
	return extproto.QueryPaymentResult{
		PaymentID: paymentID,
		Status:    extproto.PaymentSucceeded,
		Amount:    f.amount,
		Metadata:  map[string]string{"invoice_id": f.invoiceID},
	}, nil
}

// recordingApplier stands in for the fulfilment applier and records what it was asked to apply.
type recordingApplier struct {
	applied []payments.SettledPayment
}

func (a *recordingApplier) ApplySettledPayment(_ context.Context, _ pgx.Tx, p payments.SettledPayment) (payments.ApplyResult, error) {
	a.applied = append(a.applied, p)
	return payments.ApplyResult{InvoicePaid: true, PaymentID: p.ProviderPaymentID}, nil
}

type fixture struct {
	pool       *store.Pool
	invoiceID  string
	customerID string
	gateway    *fakeGateway
	applier    *recordingApplier
	inbox      *payments.Inbox
	processor  *payments.Processor
}

// newFixture builds an invoice with a recorded checkout attempt, which is what amount
// verification compares the settled payment against.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := storetest.DB(t)
	ctx := context.Background()
	f := &fixture{pool: pool, gateway: &fakeGateway{amount: extproto.Amount{Minor: 1200, Currency: "USD"}},
		applier: &recordingApplier{}}

	err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO customers (email, name) VALUES ('stripe@example.test', 'C') RETURNING id`,
		).Scan(&f.customerID); err != nil {
			return err
		}
		issuer := billing.NewIssuer(billing.FixedTaxPricer{
			Rate: money.MustParseRatio("0%"), Rounding: money.RoundHalfUp, Policy: "none",
		})
		id, _, _, err := issuer.CreateDraft(ctx, tx, billing.IssueRequest{
			CustomerID: f.customerID,
			Lines: []billing.Line{{
				Description: "VPS", QuantityMilli: 1000, UnitPrice: money.New(1200, usd),
			}},
			Customer: billing.CustomerDetails{ID: f.customerID},
		})
		if err != nil {
			return err
		}
		issued, err := issuer.Issue(ctx, tx, id, billing.DefaultSeries)
		if err != nil {
			return err
		}
		f.invoiceID = issued.InvoiceID
		// The attempt the checkout recorded: the core keys it on the provider's session id.
		_, err = tx.Exec(ctx, `
			INSERT INTO payment_attempts
				(invoice_id, gateway, operation_key, status, amount_minor, currency, provider_ref)
			VALUES ($1, 'stripe', 'attempt_1', 'pending', 1200, 'USD', 'cs_test_1')`, f.invoiceID)
		return err
	})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}

	f.gateway.invoiceID = f.invoiceID
	f.inbox = payments.NewInbox(pool)
	f.processor = payments.NewProcessor(pool, f.inbox, f.gateway, "acct_test", f.applier).
		WithAttempts(attemptLookup(pool))
	return f
}

// attemptLookup is the production attempt lookup, inlined so this package does not depend on
// the checkout package for a three-line query.
func attemptLookup(q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) payments.AttemptLookup {
	return func(ctx context.Context, provider, providerRef string) (string, int64, string, bool, error) {
		var invoiceID, currency string
		var amount int64
		err := q.QueryRow(ctx, `
			SELECT invoice_id, amount_minor, currency FROM payment_attempts
			WHERE gateway = $1 AND provider_ref = $2`, provider, providerRef).
			Scan(&invoiceID, &amount, &currency)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", 0, "", false, nil
		}
		if err != nil {
			return "", 0, "", false, err
		}
		return invoiceID, amount, currency, true, nil
	}
}

// pendingRow returns the single inbox row, for assertions about what was persisted.
func (f *fixture) pendingRow(t *testing.T) payments.PendingEvent {
	t.Helper()
	rows, err := f.inbox.ClaimPending(context.Background(), 10)
	if err != nil {
		t.Fatalf("claim pending: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("pending rows = %d, want 1", len(rows))
	}
	return rows[0]
}

// The reported failure: a provider-shaped delivery is applied, because the verifier's
// normalised payment id is persisted and used.
func TestProviderShapedDeliveryIsApplied(t *testing.T) {
	f := newFixture(t)
	body := stripeShapedBody("evt_3UTWie", "cs_test_1")

	isNew, err := f.inbox.Receive(context.Background(), "stripe", "acct_test", payments.VerifiedEvent{
		EventID:   "evt_3UTWie",
		EventType: payments.EventPaymentSucceeded,
		PaymentID: "cs_test_1",
	}, body)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if !isNew {
		t.Fatal("the first delivery was not recorded as new")
	}

	// The id is a stored fact, not something the processor has to guess from the body.
	if got := f.pendingRow(t).PaymentID; got != "cs_test_1" {
		t.Fatalf("stored payment id = %q, want cs_test_1", got)
	}

	results, err := f.processor.Process(context.Background(), 10)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(results) != 1 || results[0].InvoiceID != f.invoiceID {
		t.Fatalf("results = %+v, want one applied event for %s", results, f.invoiceID)
	}
	if len(f.applier.applied) != 1 {
		t.Fatalf("applied = %+v, want one payment", f.applier.applied)
	}
	got := f.applier.applied[0]
	if got.ProviderPaymentID != "cs_test_1" || got.InvoiceID != f.invoiceID || got.AmountMinor != 1200 {
		t.Fatalf("applied payment = %+v", got)
	}
	if len(f.gateway.queried) != 1 || f.gateway.queried[0] != "cs_test_1" {
		t.Fatalf("queried = %v, want the payment id from the normalisation", f.gateway.queried)
	}
}

// A row stored before the normalised id was persisted cannot be applied, and says why. This is
// the state the sandbox event was left in.
func TestLegacyRowWithoutANormalisedIDFailsVisibly(t *testing.T) {
	f := newFixture(t)
	body := stripeShapedBody("evt_legacy", "cs_test_1")

	// An empty PaymentID is exactly what Receive stored before the column existed.
	if _, err := f.inbox.Receive(context.Background(), "stripe", "acct_test", payments.VerifiedEvent{
		EventID:   "evt_legacy",
		EventType: payments.EventPaymentSucceeded,
	}, body); err != nil {
		t.Fatalf("receive: %v", err)
	}

	if _, err := f.processor.Process(context.Background(), 10); !errors.Is(err, payments.ErrMissingPaymentID) {
		t.Fatalf("got %v, want ErrMissingPaymentID", err)
	}
	if len(f.applier.applied) != 0 {
		t.Fatal("a payment with no identity was applied")
	}

	// The row stays pending with the reason recorded, so it is visible rather than lost.
	var processed bool
	var lastError string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT processed_at IS NOT NULL, COALESCE(last_error, '') FROM gateway_events`).
		Scan(&processed, &lastError); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if processed {
		t.Fatal("the event was marked processed despite failing")
	}
	if lastError == "" {
		t.Fatal("the failure was not recorded")
	}
}

// A provider resend repairs such a row: the redelivery is still "not new" (nothing is
// re-processed, the duplicate metric stays honest) but it fills in the normalisation, and the
// next processor run applies the payment. Without this, a resend of an already-stored event id
// could never fix anything — the dedupe key would make it a permanent no-op.
func TestResendRepairsANormalisedID(t *testing.T) {
	f := newFixture(t)
	body := stripeShapedBody("evt_resend", "cs_test_1")

	if _, err := f.inbox.Receive(context.Background(), "stripe", "acct_test", payments.VerifiedEvent{
		EventID: "evt_resend",
		// The state the incident left behind: the adapter passed the provider's own event
		// type through, the gate happened to accept it ("payment_intent.succeeded" is a
		// settlement kind), and the normalised payment id was dropped at ingest.
		EventType: payments.EventPaymentIntentSucceeded,
	}, body); err != nil {
		t.Fatalf("first receive: %v", err)
	}
	if _, err := f.processor.Process(context.Background(), 10); !errors.Is(err, payments.ErrMissingPaymentID) {
		t.Fatalf("first process: got %v, want ErrMissingPaymentID", err)
	}
	if len(f.applier.applied) != 0 {
		t.Fatal("a payment with no identity was applied")
	}

	// The resend carries the normalisation.
	isNew, err := f.inbox.Receive(context.Background(), "stripe", "acct_test", payments.VerifiedEvent{
		EventID:   "evt_resend",
		EventType: payments.EventPaymentSucceeded,
		PaymentID: "cs_test_1",
	}, body)
	if err != nil {
		t.Fatalf("resend: %v", err)
	}
	if isNew {
		t.Fatal("a resend was recorded as a new event; the dedupe key was bypassed")
	}

	row := f.pendingRow(t)
	if row.PaymentID != "cs_test_1" {
		t.Fatalf("resend did not repair the payment id: %q", row.PaymentID)
	}
	if row.EventType != payments.EventPaymentSucceeded {
		t.Fatalf("resend did not repair the event type: %q", row.EventType)
	}

	if _, err := f.processor.Process(context.Background(), 10); err != nil {
		t.Fatalf("process after repair: %v", err)
	}
	if len(f.applier.applied) != 1 || f.applier.applied[0].ProviderPaymentID != "cs_test_1" {
		t.Fatalf("applied = %+v, want the repaired payment", f.applier.applied)
	}
}

// A verifier that names a payment under a kind the core does not know is a contract error, not
// a no-op: skipping it would mark the event processed and leave the invoice unpaid silently.
func TestPaymentUnderAnUnknownKindStaysPending(t *testing.T) {
	f := newFixture(t)
	body := stripeShapedBody("evt_unknown_kind", "cs_test_1")

	if _, err := f.inbox.Receive(context.Background(), "stripe", "acct_test", payments.VerifiedEvent{
		EventID:   "evt_unknown_kind",
		EventType: "checkout.session.completed",
		PaymentID: "cs_test_1",
	}, body); err != nil {
		t.Fatalf("receive: %v", err)
	}

	if _, err := f.processor.Process(context.Background(), 10); !errors.Is(err, payments.ErrUnsupportedEvent) {
		t.Fatalf("got %v, want ErrUnsupportedEvent", err)
	}
	if len(f.applier.applied) != 0 {
		t.Fatal("a payment under an unrecognised kind was applied")
	}
	if len(f.gateway.queried) != 0 {
		t.Fatal("the provider was queried for an event the core does not understand")
	}

	var processed bool
	if err := f.pool.QueryRow(context.Background(),
		`SELECT processed_at IS NOT NULL FROM gateway_events`).Scan(&processed); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if processed {
		t.Fatal("the event was marked processed, hiding the contract error")
	}
}

// An event that names no payment and is not a settlement is skipped, not retried: a provider
// emits far more of those than payments, and a retry queue full of them hides the real failures.
func TestNonSettlementEventIsSkipped(t *testing.T) {
	f := newFixture(t)

	if _, err := f.inbox.Receive(context.Background(), "stripe", "acct_test", payments.VerifiedEvent{
		EventID:   "evt_noise",
		EventType: "checkout.session.expired",
	}, []byte(`{"id":"evt_noise","type":"checkout.session.expired"}`)); err != nil {
		t.Fatalf("receive: %v", err)
	}

	results, err := f.processor.Process(context.Background(), 10)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(results) != 1 || !results[0].Skipped {
		t.Fatalf("results = %+v, want one skipped event", results)
	}
}
