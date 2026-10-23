package httpapi_test

// The webhook ingest path for a real provider delivery.
//
// The core delegates verification to the gateway extension because only the extension knows a
// provider's payload shape. The extension answers with a *normalised* event — event id, the
// core's event kind, the provider payment id — and this endpoint is where that normalisation
// meets storage. It used to keep only the raw body and drop the rest, which meant the payment
// processor later re-derived the payment id by parsing the provider's payload and failed:
//
//	payments: event evt_3UTWieERFJuvmOf21CDXZJaf carries no payment id
//
// These tests post a Stripe-shaped body (payment nested under data.object, no top-level
// payment_id) and assert that the normalisation is what gets stored.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/httpapi"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
)

// stubVerifier stands in for the gateway extension: it accepts any body and reports whatever
// normalisation the test is exercising.
type stubVerifier struct {
	ev payments.VerifiedEvent
}

func (s stubVerifier) Verify(context.Context, []byte, map[string]string) (payments.VerifiedEvent, error) {
	return s.ev, nil
}

// stripeBody is a real Stripe delivery. There is no `payment_id` at the top level: the payment
// is the session id nested under data.object.
func stripeBody(eventID, sessionID string) string {
	return `{"id":"` + eventID + `","type":"checkout.session.completed","created":1729600000,` +
		`"data":{"object":{"id":"` + sessionID + `","object":"checkout.session",` +
		`"payment_status":"paid","amount_total":1200,"currency":"usd"}}}`
}

func newWebhookServer(t *testing.T, pool *store.Pool, verifier payments.Verifier) (*httpapi.Server, *payments.Inbox) {
	t.Helper()
	inbox := payments.NewInbox(pool)
	srv := httpapi.New(httpapi.Config{
		Version: "test",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, pool, inbox, "stripe", "acct_test", verifier)
	return srv, inbox
}

func postWebhook(t *testing.T, srv *httpapi.Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// The reported failure, at the ingest end: a provider-shaped delivery is stored with the
// verifier's payment id, so the processor never has to parse the payload.
func TestWebhookStoresTheNormalisedPaymentID(t *testing.T) {
	srv, inbox := newWebhookServer(t, storetest.DB(t), stubVerifier{ev: payments.VerifiedEvent{
		EventID:   "evt_3UTWie",
		EventType: payments.EventPaymentSucceeded,
		PaymentID: "cs_test_1",
	}})

	body := stripeBody("evt_3UTWie", "cs_test_1")
	rec := postWebhook(t, srv, body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", rec.Code, rec.Body)
	}

	pending, err := inbox.ClaimPending(context.Background(), 10)
	if err != nil {
		t.Fatalf("claim pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("stored events = %d, want 1", len(pending))
	}
	row := pending[0]
	if row.PaymentID != "cs_test_1" {
		t.Fatalf("stored payment id = %q, want cs_test_1", row.PaymentID)
	}
	if row.EventType != payments.EventPaymentSucceeded {
		t.Fatalf("stored event type = %q, want %q", row.EventType, payments.EventPaymentSucceeded)
	}
	// The raw delivery is still the provider's own document: the normalisation is stored
	// beside it, not in place of it, so the audit trail keeps Stripe's own event type.
	if !strings.Contains(string(row.Raw), `"checkout.session.completed"`) {
		t.Fatalf("the raw provider body was not preserved: %s", row.Raw)
	}
}

// A redelivery of an already-stored event id repairs the normalisation on an unprocessed row.
// This is the recovery path for the events that were stored before the payment id was kept:
// the provider's "resend" fills in what the first delivery lost, and the row is then applied.
func TestWebhookRedeliveryRepairsAnUnprocessedRow(t *testing.T) {
	// The first delivery is normalised by an adapter that does not report a payment id, which
	// is what happened before this was fixed.
	pool := storetest.DB(t)
	srv, inbox := newWebhookServer(t, pool, stubVerifier{ev: payments.VerifiedEvent{
		EventID:   "evt_stuck",
		EventType: "checkout.session.completed",
	}})
	body := stripeBody("evt_stuck", "cs_test_1")
	if rec := postWebhook(t, srv, body); rec.Code != http.StatusAccepted {
		t.Fatalf("first delivery: status = %d, want 202", rec.Code)
	}

	pending, err := inbox.ClaimPending(context.Background(), 10)
	if err != nil {
		t.Fatalf("claim pending: %v", err)
	}
	if len(pending) != 1 || pending[0].PaymentID != "" {
		t.Fatalf("pending = %+v, want one row with no payment id", pending)
	}

	// The same event id arrives again, this time normalised.
	srv2, _ := newWebhookServer(t, pool, stubVerifier{ev: payments.VerifiedEvent{
		EventID:   "evt_stuck",
		EventType: payments.EventPaymentSucceeded,
		PaymentID: "cs_test_1",
	}})
	rec := postWebhook(t, srv2, body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "duplicate") {
		t.Fatalf("redelivery: status = %d, body = %s; want 200 duplicate", rec.Code, rec.Body)
	}

	pending, err = inbox.ClaimPending(context.Background(), 10)
	if err != nil {
		t.Fatalf("claim pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("stored events = %d, want 1 (the redelivery must not add a row)", len(pending))
	}
	if pending[0].PaymentID != "cs_test_1" {
		t.Fatalf("payment id after redelivery = %q, want cs_test_1", pending[0].PaymentID)
	}
	if pending[0].EventType != payments.EventPaymentSucceeded {
		t.Fatalf("event type after redelivery = %q, want %q", pending[0].EventType, payments.EventPaymentSucceeded)
	}
}
