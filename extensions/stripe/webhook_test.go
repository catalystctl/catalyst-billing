package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// Webhook verification tests.
//
// Each of these is a real attack if the check it covers is missing, so each is
// written as "the adapter must refuse this", not "the adapter happens to accept
// the good case".

func signedDelivery(t *testing.T, s *server, f *fakeStripe, body []byte, at time.Time) map[string]string {
	t.Helper()
	return map[string]string{"Stripe-Signature": f.signWebhook(body, at)}
}

func verify(t *testing.T, s *server, headers map[string]string, body []byte) verifyResult {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"headers": headers, "body": body})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	id := uint64(1)
	rep := s.handle(&request{JSONRPC: "2.0", ID: &id, Method: "gateway.verify_webhook", Params: raw})
	if rep.rpcErr != nil {
		t.Fatalf("verify_webhook: rpc error %d: %s", rep.rpcErr.Code, rep.rpcErr.Message)
	}
	out, err := json.Marshal(rep.result)
	if err != nil {
		t.Fatal(err)
	}
	var res verifyResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return res
}

func sessionCompletedBody(sessionID, intentID string, amount int64) []byte {
	return []byte(fmt.Sprintf(`{
		"id": "evt_1",
		"type": "checkout.session.completed",
		"created": %d,
		"data": {"object": {
			"id": %q,
			"object": "checkout.session",
			"payment_status": "paid",
			"amount_total": %d,
			"currency": "usd",
			"payment_intent": %q
		}}
	}`, time.Now().Unix(), sessionID, amount, intentID))
}

// The good case: an authentic delivery naming a paid session.
func TestWebhookAcceptsAnAuthenticDelivery(t *testing.T) {
	s, f := newTestServer(t)
	now := time.Now()
	s.now = clockFor(now)
	body := sessionCompletedBody("cs_test_1", "pi_test_1", 1999)

	res := verify(t, s, signedDelivery(t, s, f, body, now), body)
	if !res.Valid {
		t.Fatalf("an authentic delivery was refused: %s", res.Reason)
	}
	if res.EventID != "evt_1" {
		t.Fatalf("event id = %q", res.EventID)
	}
	// The core keys its checkout attempt on the session id, so that is what the
	// delivery must name.
	if res.PaymentID != "cs_test_1" {
		t.Fatalf("payment id = %q, want the session id", res.PaymentID)
	}
	if res.Amount == nil || res.Amount.Minor != 1999 || res.Amount.Currency != "USD" {
		t.Fatalf("amount = %+v", res.Amount)
	}
	// The core acts on its own settlement vocabulary. Reporting Stripe's own event type here
	// left the core with a kind it did not recognise, so it skipped the event and the invoice
	// stayed unpaid — silently. This is the assertion that keeps the mapping honest.
	if res.EventType != "payment.succeeded" {
		t.Fatalf("event type = %q, want the core's settlement kind payment.succeeded", res.EventType)
	}
}

// A delivery that is valid but says nothing about a settled payment keeps the provider's own
// event type: the core records it and skips it, and the inbox row is more useful naming what
// Stripe actually sent.
func TestWebhookKeepsTheProviderTypeForNonPayments(t *testing.T) {
	s, f := newTestServer(t)
	now := time.Now()
	s.now = clockFor(now)
	body := []byte(fmt.Sprintf(`{
		"id": "evt_expired",
		"type": "checkout.session.expired",
		"created": %d,
		"data": {"object": {"id": "cs_test_1", "object": "checkout.session"}}
	}`, now.Unix()))

	res := verify(t, s, signedDelivery(t, s, f, body, now), body)
	if !res.Valid {
		t.Fatalf("an authentic delivery was refused: %s", res.Reason)
	}
	if res.PaymentID != "" {
		t.Fatalf("a non-payment delivery named payment %q", res.PaymentID)
	}
	if res.EventType != "checkout.session.expired" {
		t.Fatalf("event type = %q, want the provider's own type", res.EventType)
	}
}

// A body that does not match its signature is a forgery attempt. It must be
// refused even though the header is otherwise well-formed.
func TestWebhookRefusesATamperedBody(t *testing.T) {
	s, f := newTestServer(t)
	now := time.Now()
	s.now = clockFor(now)
	body := sessionCompletedBody("cs_test_1", "pi_test_1", 1999)
	headers := signedDelivery(t, s, f, body, now)

	tampered := []byte(string(body) + " ")
	res := verify(t, s, headers, tampered)
	if res.Valid {
		t.Fatal("a tampered body was accepted")
	}
	if res.Reason == "" {
		t.Fatal("a refusal must say why")
	}
}

// A delivery signed with the wrong secret is refused. This is the case a
// misconfigured endpoint produces, and it must not be silently trusted.
func TestWebhookRefusesTheWrongSecret(t *testing.T) {
	s, f := newTestServer(t)
	now := time.Now()
	s.now = clockFor(now)
	body := sessionCompletedBody("cs_test_1", "pi_test_1", 1999)
	headers := map[string]string{
		"Stripe-Signature": fmt.Sprintf("t=%d,v1=%s", now.Unix(), signPayload("whsec_wrong", now.Unix(), body)),
	}
	_ = f
	if res := verify(t, s, headers, body); res.Valid {
		t.Fatal("a delivery signed with the wrong secret was accepted")
	}
}

// An old delivery is refused even though its signature is genuine: a captured
// request must not be replayable a week later.
func TestWebhookRefusesAStaleDelivery(t *testing.T) {
	s, f := newTestServer(t)
	s.now = clockFor(time.Now())
	body := sessionCompletedBody("cs_test_1", "pi_test_1", 1999)
	old := time.Now().Add(-2 * time.Hour)

	res := verify(t, s, signedDelivery(t, s, f, body, old), body)
	if res.Valid {
		t.Fatal("a two-hour-old delivery was accepted")
	}
	if res.Reason == "" {
		t.Fatal("a refusal must say why")
	}
}

// A timestamp far in the future is refused for the same reason: it would make a
// delivery valid for as long as the sender likes.
func TestWebhookRefusesAFutureDelivery(t *testing.T) {
	s, f := newTestServer(t)
	s.now = clockFor(time.Now())
	body := sessionCompletedBody("cs_test_1", "pi_test_1", 1999)
	future := time.Now().Add(2 * time.Hour)
	if res := verify(t, s, signedDelivery(t, s, f, body, future), body); res.Valid {
		t.Fatal("a delivery timestamped in the future was accepted")
	}
}

// A missing or unparseable header is a refusal, not a crash.
func TestWebhookRefusesMalformedHeaders(t *testing.T) {
	s, f := newTestServer(t)
	body := sessionCompletedBody("cs_test_1", "pi_test_1", 1999)
	for name, headers := range map[string]map[string]string{
		"absent":      {},
		"empty":       {"Stripe-Signature": ""},
		"no v1":       {"Stripe-Signature": "t=12345"},
		"no t":        {"Stripe-Signature": "v1=abc"},
		"t not int":   {"Stripe-Signature": "t=abc,v1=def"},
		"junk header": {"Stripe-Signature": "nonsense"},
	} {
		t.Run(name, func(t *testing.T) {
			if res := verify(t, s, headers, body); res.Valid {
				t.Fatalf("%s header was accepted", name)
			}
		})
	}
	_ = f
}

// A rotation sends several v1 signatures; any one matching is correct, so a
// header with a stale signature first must still be accepted.
func TestWebhookAcceptsOneOfSeveralSignatures(t *testing.T) {
	s, f := newTestServer(t)
	now := time.Now()
	s.now = clockFor(now)
	body := sessionCompletedBody("cs_test_1", "pi_test_1", 1999)
	good := signPayload(f.webhookKey, now.Unix(), body)
	header := fmt.Sprintf("t=%d,v1=%s,v1=%s", now.Unix(), "deadbeef", good)
	if res := verify(t, s, map[string]string{"Stripe-Signature": header}, body); !res.Valid {
		t.Fatalf("a delivery with one matching signature was refused: %s", res.Reason)
	}
}

// A delivery that names only the PaymentIntent is resolved back to its Checkout
// Session, because the session is what the core recorded and what its amount
// verification looks up.
func TestWebhookResolvesPaymentIntentToSession(t *testing.T) {
	s, f := newTestServer(t)
	now := time.Now()
	s.now = clockFor(now)

	// Create a real session through the adapter so the fake holds the relation.
	raw := call(t, s, "gateway.create_checkout", checkoutParams("wh-1", "USD", 3000))
	var created struct {
		PaymentID string `json:"payment_id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	sess := f.sessions[created.PaymentID]

	body := []byte(fmt.Sprintf(`{
		"id": "evt_pi",
		"type": "payment_intent.succeeded",
		"created": %d,
		"data": {"object": {
			"id": %q,
			"object": "payment_intent",
			"status": "succeeded",
			"amount": 3000,
			"amount_received": 3000,
			"currency": "usd"
		}}
	}`, now.Unix(), sess.IntentID))

	res := verify(t, s, signedDelivery(t, s, f, body, now), body)
	if !res.Valid {
		t.Fatalf("an authentic payment_intent delivery was refused: %s", res.Reason)
	}
	if res.PaymentID != created.PaymentID {
		t.Fatalf("payment id = %q, want the session id %q", res.PaymentID, created.PaymentID)
	}
	if res.Amount == nil || res.Amount.Minor != 3000 {
		t.Fatalf("amount = %+v", res.Amount)
	}
}

// An event that is authentic but not about a settled payment is reported valid
// with a reason, so the core records it and skips instead of retrying forever.
func TestWebhookReportsNonPaymentEventsAsValidButUninteresting(t *testing.T) {
	s, f := newTestServer(t)
	now := time.Now()
	s.now = clockFor(now)
	body := []byte(fmt.Sprintf(`{"id":"evt_x","type":"customer.created","created":%d,"data":{"object":{"id":"cus_1"}}}`,
		now.Unix()))
	res := verify(t, s, signedDelivery(t, s, f, body, now), body)
	if !res.Valid {
		t.Fatalf("an authentic non-payment event was refused: %s", res.Reason)
	}
	if res.Reason == "" {
		t.Fatal("a skipped event should say why it was skipped")
	}
	if res.PaymentID != "" {
		t.Fatalf("a non-payment event named a payment: %q", res.PaymentID)
	}
}

// A completed session that is still unpaid is an asynchronous method in flight,
// not a settlement. Reporting it as a payment would credit an invoice the customer
// has not paid.
func TestWebhookRefusesAnUnpaidCompletedSession(t *testing.T) {
	s, f := newTestServer(t)
	now := time.Now()
	s.now = clockFor(now)
	body := []byte(fmt.Sprintf(`{
		"id": "evt_async",
		"type": "checkout.session.completed",
		"created": %d,
		"data": {"object": {"id": "cs_test_9", "payment_status": "unpaid", "amount_total": 500, "currency": "usd"}}
	}`, now.Unix()))

	res := verify(t, s, signedDelivery(t, s, f, body, now), body)
	if res.PaymentID != "" {
		t.Fatalf("an unpaid session was reported as payment %q", res.PaymentID)
	}
	if res.Reason == "" {
		t.Fatal("an unpaid session should say why it is not a settlement")
	}
}

// An event with no id cannot be deduplicated, so applying it would risk a double
// settlement on a redelivery. It is refused.
func TestWebhookRefusesAnEventWithoutAnID(t *testing.T) {
	s, f := newTestServer(t)
	now := time.Now()
	s.now = clockFor(now)
	body := []byte(`{"type":"checkout.session.completed","data":{"object":{"id":"cs_test_1","payment_status":"paid","amount_total":500,"currency":"usd"}}}`)
	if res := verify(t, s, signedDelivery(t, s, f, body, now), body); res.Valid {
		t.Fatal("an event with no id was accepted, so a redelivery would settle twice")
	}
}

// With no signing secret configured, nothing can be authenticated, so every
// delivery is refused. A gateway that accepts anything is worse than one that is
// unconfigured.
func TestWebhookWithoutASecretRefusesEverything(t *testing.T) {
	s, f := newTestServer(t)
	s.webhook = ""
	now := time.Now()
	s.now = clockFor(now)
	body := sessionCompletedBody("cs_test_1", "pi_test_1", 1999)
	headers := map[string]string{"Stripe-Signature": f.signWebhook(body, now)}
	if res := verify(t, s, headers, body); res.Valid {
		t.Fatal("a delivery was accepted with no signing secret configured")
	}
}
