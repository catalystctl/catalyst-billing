package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The adapter's unit tests, driven through its own JSON-RPC surface rather than by
// calling the Go methods directly.
//
// That is deliberate: the wire is what the core speaks, so a test at this level
// catches a mis-shaped result (a field renamed, a time serialised as a string) that
// an in-process call would hide.

// call invokes one method the way the core does: marshal params, unmarshal the
// result, and fail on a JSON-RPC error.
func call(t *testing.T, s *server, method string, params any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	id := uint64(1)
	rep := s.handle(&request{JSONRPC: "2.0", ID: &id, Method: method, Params: raw})
	if rep.rpcErr != nil {
		t.Fatalf("%s: rpc error %d: %s", method, rep.rpcErr.Code, rep.rpcErr.Message)
	}
	out, err := json.Marshal(rep.result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	return out
}

// callErr invokes one method and returns the JSON-RPC error it produced, failing
// when the call unexpectedly succeeded.
func callErr(t *testing.T, s *server, method string, params any) *rpcError {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	id := uint64(1)
	rep := s.handle(&request{JSONRPC: "2.0", ID: &id, Method: method, Params: raw})
	if rep.rpcErr == nil {
		t.Fatalf("%s: expected an error, got result %v", method, rep.result)
	}
	return rep.rpcErr
}

func newTestServer(t *testing.T) (*server, *fakeStripe) {
	t.Helper()
	f := newFakeStripe(t)
	s := &server{client: newStripeClient(f.config()), webhook: f.webhookKey, cfg: f.config()}
	return s, f
}

func checkoutParams(key, currency string, minor int64) map[string]any {
	return map[string]any{
		"operation_key": key,
		"customer_id":   "cust_1",
		"amount":        map[string]any{"minor": minor, "currency": currency},
		"description":   "Pro plan, March",
		"success_url":   "https://panel.test/checkout/complete",
		"cancel_url":    "https://panel.test/checkout/cancelled",
		"metadata":      map[string]string{"invoice_id": "inv_1"},
	}
}

// The handshake must advertise exactly what the adapter implements, and must
// refuse a core that speaks a version it does not.
func TestInitializeAdvertisesItsMethods(t *testing.T) {
	s, _ := newTestServer(t)
	raw := call(t, s, "initialize", map[string]any{
		"protocol": "billing.ext/1", "versions": []int{2, 1}, "core_version": "test",
	})
	var res struct {
		Version      int      `json:"version"`
		Name         string   `json:"name"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Version != 1 {
		t.Fatalf("version = %d, want 1", res.Version)
	}
	if res.Name != "stripe-gateway" {
		t.Fatalf("name = %q", res.Name)
	}
	want := []string{"gateway.describe", "gateway.create_checkout", "gateway.query_payment",
		"gateway.refund", "gateway.verify_webhook", "gateway.create_billing_agreement",
		"gateway.query_billing_agreement", "gateway.cancel_billing_agreement", "gateway.charge"}
	if len(res.Capabilities) != len(want) {
		t.Fatalf("capabilities = %v, want %v", res.Capabilities, want)
	}
	for i := range want {
		if res.Capabilities[i] != want[i] {
			t.Fatalf("capabilities = %v, want %v", res.Capabilities, want)
		}
	}

	// A core that offers only version 2 must be refused, not guessed at.
	id := uint64(7)
	rep := s.handle(&request{JSONRPC: "2.0", ID: &id, Method: "initialize",
		Params: json.RawMessage(`{"protocol":"billing.ext/1","versions":[2]}`)})
	if rep.rpcErr == nil || rep.rpcErr.Code != codeVersionMismatch {
		t.Fatalf("expected a version mismatch, got %+v", rep)
	}
}

func TestDescribe(t *testing.T) {
	s, _ := newTestServer(t)
	raw := call(t, s, "gateway.describe", map[string]any{})
	var res struct {
		Name             string   `json:"name"`
		Currencies       []string `json:"currencies"`
		SupportsRefunds  bool     `json:"supports_refunds"`
		SupportsWebhooks bool     `json:"supports_webhooks"`
		Methods          []struct {
			ID string `json:"id"`
		} `json:"methods"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Name != "stripe" {
		t.Fatalf("name = %q", res.Name)
	}
	if !res.SupportsRefunds || !res.SupportsWebhooks {
		t.Fatalf("describe understates capabilities: %+v", res)
	}
	if len(res.Currencies) == 0 || res.Currencies[0] != "USD" {
		t.Fatalf("currencies = %v", res.Currencies)
	}
	if len(res.Methods) != 1 || res.Methods[0].ID != "card" {
		t.Fatalf("methods = %+v", res.Methods)
	}
}

// A checkout must send exactly what Stripe requires: the amount in minor units as
// a bare integer, the currency lower-case, both return URLs, the customer as
// client_reference_id, and the metadata on the intent as well as the session.
func TestCreateCheckoutSendsStripeShapedRequest(t *testing.T) {
	s, f := newTestServer(t)
	raw := call(t, s, "gateway.create_checkout", checkoutParams("op-key-1", "USD", 1999))

	var res struct {
		PaymentID   string `json:"payment_id"`
		CheckoutURL string `json:"checkout_url"`
		Status      string `json:"status"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.PaymentID == "" || res.CheckoutURL == "" {
		t.Fatalf("incomplete checkout: %+v", res)
	}
	if res.Status != "pending" {
		t.Fatalf("status = %q, want pending", res.Status)
	}

	req := f.lastRequest()
	if req.Auth != "Bearer sk_test_123" {
		t.Fatalf("authorization = %q", req.Auth)
	}
	if req.IdempotencyKey != "op-key-1" {
		t.Fatalf("idempotency key = %q, want the operation key", req.IdempotencyKey)
	}
	if got := req.Form["line_items[0][price_data][unit_amount]"]; got != "1999" {
		t.Fatalf("unit_amount = %q, want 1999 (minor units, no decimal point)", got)
	}
	if got := req.Form["line_items[0][price_data][currency]"]; got != "usd" {
		t.Fatalf("currency = %q, want lower-case usd", got)
	}
	if got := req.Form["line_items[0][price_data][product_data][name]"]; got != "Pro plan, March" {
		t.Fatalf("product name = %q", got)
	}
	if got := req.Form["mode"]; got != "payment" {
		t.Fatalf("mode = %q", got)
	}
	if got := req.Form["client_reference_id"]; got != "cust_1" {
		t.Fatalf("client_reference_id = %q", got)
	}
	if got := req.Form["metadata[invoice_id]"]; got != "inv_1" {
		t.Fatalf("metadata[invoice_id] = %q", got)
	}
	// The invoice has to be on the intent too: a refund or a dispute investigation
	// reads the intent, not the session.
	if got := req.Form["payment_intent_data[metadata][invoice_id]"]; got != "inv_1" {
		t.Fatalf("payment_intent_data[metadata][invoice_id] = %q", got)
	}
	if req.Form["success_url"] == "" || req.Form["cancel_url"] == "" {
		t.Fatalf("return urls missing: %v", req.Form)
	}
}

// Replaying the operation key must return the same session. Stripe enforces this
// itself, so the assertion is that the adapter sends the key and honours the
// answer — one session, not two.
func TestCreateCheckoutIsIdempotentByOperationKey(t *testing.T) {
	s, _ := newTestServer(t)
	first := call(t, s, "gateway.create_checkout", checkoutParams("same-key", "USD", 500))
	second := call(t, s, "gateway.create_checkout", checkoutParams("same-key", "USD", 500))

	var a, b struct {
		PaymentID string `json:"payment_id"`
	}
	if err := json.Unmarshal(first, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second, &b); err != nil {
		t.Fatal(err)
	}
	if a.PaymentID != b.PaymentID {
		t.Fatalf("replay created a second session: %q then %q", a.PaymentID, b.PaymentID)
	}
}

// A missing operation key is a programming error in the core, and must be refused
// rather than silently sent without Stripe's idempotency protection.
func TestCreateCheckoutRequiresOperationKey(t *testing.T) {
	s, f := newTestServer(t)
	p := checkoutParams("", "USD", 500)
	err := callErr(t, s, "gateway.create_checkout", p)
	if err.Code != codeInvalidParams {
		t.Fatalf("code = %d, want %d", err.Code, codeInvalidParams)
	}
	if f.requestCount() != 0 {
		t.Fatalf("a request reached Stripe with no operation key")
	}
}

// A declined card is a refusal on the merits: the core must be able to tell it
// apart from a broken integration, because the invoice stays open and the customer
// gets a message they can act on.
func TestDeclinedCardIsARejectionNotAFailure(t *testing.T) {
	s, f := newTestServer(t)
	f.failWith(402, `{"error":{"type":"card_error","code":"card_declined","decline_code":"insufficient_funds","message":"Your card has insufficient funds."}}`)

	err := callErr(t, s, "gateway.create_checkout", checkoutParams("declined", "USD", 500))
	if err.Code != codeRejected {
		t.Fatalf("code = %d, want %d (a decline is not a failure)", err.Code, codeRejected)
	}
	if !strings.Contains(err.Message, "insufficient funds") {
		t.Fatalf("message %q does not carry the provider's explanation", err.Message)
	}
	data, ok := err.Data.(map[string]any)
	if !ok || data["decline_code"] != "insufficient_funds" {
		t.Fatalf("data = %#v, want the decline code", err.Data)
	}
}

// An unreachable or broken provider is a failure, not a rejection: the core
// retries it with the same operation key.
func TestServerErrorIsNotARejection(t *testing.T) {
	s, f := newTestServer(t)
	f.failWith(500, `{"error":{"type":"api_error","message":"Stripe is down"}}`)
	err := callErr(t, s, "gateway.create_checkout", checkoutParams("boom", "USD", 500))
	if err.Code == codeRejected {
		t.Fatal("a 500 was reported as a rejection, so the invoice would stay open forever")
	}
	if err.Code != codeInternal {
		t.Fatalf("code = %d, want %d", err.Code, codeInternal)
	}
}

// An unknown payment is not_found, which the core treats as "the provider has
// never heard of this" rather than a transient failure.
func TestUnknownPaymentIsNotFound(t *testing.T) {
	s, _ := newTestServer(t)
	err := callErr(t, s, "gateway.query_payment", map[string]any{"payment_id": "cs_test_nope"})
	if err.Code != codeNotFound {
		t.Fatalf("code = %d, want %d", err.Code, codeNotFound)
	}
}

// Querying a paid session must report succeeded with the amount the provider
// actually charged — that number is what the core verifies against its recorded
// checkout, so it cannot come from the adapter's own bookkeeping.
func TestQueryPaymentReportsTheProvidersAmount(t *testing.T) {
	s, f := newTestServer(t)
	raw := call(t, s, "gateway.create_checkout", checkoutParams("q1", "USD", 2500))
	var created struct {
		PaymentID string `json:"payment_id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}

	// Before the customer pays: pending.
	raw = call(t, s, "gateway.query_payment", map[string]any{"payment_id": created.PaymentID})
	var q struct {
		PaymentID string `json:"payment_id"`
		Status    string `json:"status"`
		Amount    struct {
			Minor    int64  `json:"minor"`
			Currency string `json:"currency"`
		} `json:"amount"`
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		t.Fatal(err)
	}
	if q.Status != "pending" {
		t.Fatalf("status = %q, want pending before payment", q.Status)
	}

	f.markPaid(created.PaymentID)
	raw = call(t, s, "gateway.query_payment", map[string]any{"payment_id": created.PaymentID})
	if err := json.Unmarshal(raw, &q); err != nil {
		t.Fatal(err)
	}
	if q.Status != "succeeded" {
		t.Fatalf("status = %q, want succeeded", q.Status)
	}
	if q.Amount.Minor != 2500 || q.Amount.Currency != "USD" {
		t.Fatalf("amount = %+v, want 2500 USD", q.Amount)
	}
}

// A refund names a PaymentIntent, but the core records a Checkout Session id, so
// the adapter must resolve one to the other. Getting this wrong refunds nothing.
func TestRefundResolvesSessionToPaymentIntent(t *testing.T) {
	s, f := newTestServer(t)
	raw := call(t, s, "gateway.create_checkout", checkoutParams("r1", "USD", 4000))
	var created struct {
		PaymentID string `json:"payment_id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	f.markPaid(created.PaymentID)

	raw = call(t, s, "gateway.refund", map[string]any{
		"operation_key": "refund-op-1",
		"payment_id":    created.PaymentID,
		"amount":        map[string]any{"minor": 1500, "currency": "USD"},
		"reason":        "requested_by_customer",
	})
	var res struct {
		RefundID string `json:"refund_id"`
		Status   string `json:"status"`
		Amount   struct {
			Minor int64 `json:"minor"`
		} `json:"amount"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.RefundID == "" || res.Status != "succeeded" {
		t.Fatalf("refund = %+v", res)
	}
	if res.Amount.Minor != 1500 {
		t.Fatalf("refunded %d, want 1500", res.Amount.Minor)
	}
	req := f.lastRequest()
	if !strings.HasPrefix(req.Form["payment_intent"], "pi_test_") {
		t.Fatalf("payment_intent = %q, want the resolved intent", req.Form["payment_intent"])
	}
	if req.Form["amount"] != "1500" {
		t.Fatalf("amount = %q", req.Form["amount"])
	}
	if req.Form["reason"] != "requested_by_customer" {
		t.Fatalf("reason = %q", req.Form["reason"])
	}
	if req.IdempotencyKey != "refund-op-1" {
		t.Fatalf("idempotency key = %q", req.IdempotencyKey)
	}
}

// A replayed refund must not refund twice. The adapter sends the operation key as
// Stripe's Idempotency-Key and returns whatever Stripe replays.
func TestRefundIsIdempotentByOperationKey(t *testing.T) {
	s, f := newTestServer(t)
	raw := call(t, s, "gateway.create_checkout", checkoutParams("r2", "USD", 4000))
	var created struct {
		PaymentID string `json:"payment_id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	f.markPaid(created.PaymentID)

	body := map[string]any{
		"operation_key": "refund-op-2",
		"payment_id":    created.PaymentID,
		"amount":        map[string]any{"minor": 1000, "currency": "USD"},
	}
	first := call(t, s, "gateway.refund", body)
	second := call(t, s, "gateway.refund", body)

	var a, b struct {
		RefundID string `json:"refund_id"`
	}
	if err := json.Unmarshal(first, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second, &b); err != nil {
		t.Fatal(err)
	}
	if a.RefundID != b.RefundID {
		t.Fatalf("a replayed refund produced two refunds: %q then %q", a.RefundID, b.RefundID)
	}
}

// Refunding more than is outstanding is refused before the request is sent, so a
// bug in the caller cannot reach the provider as a partial success.
func TestRefundBeyondOutstandingIsRefused(t *testing.T) {
	s, f := newTestServer(t)
	raw := call(t, s, "gateway.create_checkout", checkoutParams("r3", "USD", 1000))
	var created struct {
		PaymentID string `json:"payment_id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	f.markPaid(created.PaymentID)

	err := callErr(t, s, "gateway.refund", map[string]any{
		"operation_key": "refund-op-3",
		"payment_id":    created.PaymentID,
		"amount":        map[string]any{"minor": 5000, "currency": "USD"},
	})
	if err.Code != codeRejected {
		t.Fatalf("code = %d, want %d", err.Code, codeRejected)
	}
	if !strings.Contains(err.Message, "refundable") {
		t.Fatalf("message %q does not explain the bound", err.Message)
	}
}

// An amount that is not a positive minor-unit integer is refused locally: a
// decimal amount is how a rounding bug reaches a provider.
func TestAmountMustBeExactMinorUnits(t *testing.T) {
	s, f := newTestServer(t)
	err := callErr(t, s, "gateway.create_checkout", checkoutParams("bad", "usd", 0))
	if err.Code != codeInvalidParams {
		t.Fatalf("code = %d, want %d", err.Code, codeInvalidParams)
	}
	if f.requestCount() != 0 {
		t.Fatal("an invalid amount reached Stripe")
	}
}

// A checkout with no return URL would strand the customer, so it is refused here
// rather than by Stripe.
func TestCheckoutRequiresReturnURLs(t *testing.T) {
	s, _ := newTestServer(t)
	p := checkoutParams("nourl", "USD", 500)
	delete(p, "success_url")
	if err := callErr(t, s, "gateway.create_checkout", p); err.Code != codeInvalidParams {
		t.Fatalf("code = %d, want %d", err.Code, codeInvalidParams)
	}
}

// A refund reason outside Stripe's closed set is omitted from the typed field and
// preserved in metadata, so free text is not lost and the call does not 400.
func TestFreeTextRefundReasonTravelsInMetadata(t *testing.T) {
	s, f := newTestServer(t)
	raw := call(t, s, "gateway.create_checkout", checkoutParams("r4", "USD", 1000))
	var created struct {
		PaymentID string `json:"payment_id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	f.markPaid(created.PaymentID)
	call(t, s, "gateway.refund", map[string]any{
		"operation_key": "refund-op-4",
		"payment_id":    created.PaymentID,
		"reason":        "customer changed their mind after the trial",
	})
	req := f.lastRequest()
	if req.Form["reason"] != "" {
		t.Fatalf("reason = %q, want it omitted from the typed field", req.Form["reason"])
	}
	if got := req.Form["metadata[catalyst_reason]"]; got == "" {
		t.Fatal("the free-text reason was dropped instead of travelling in metadata")
	}
}

// clockFor returns a fixed time so a signature test is deterministic.
func clockFor(at time.Time) func() time.Time { return func() time.Time { return at } }

// Querying a settled payment carries the provider's own fee in namespaced metadata
// (PAY-13). The protocol has no fee field, so the adapter's convention is what makes the
// webhook path able to record it; a test that only checked the charge result would miss it.
func TestQueryPaymentReportsTheProviderFee(t *testing.T) {
	s, f := newTestServer(t)
	f.mu.Lock()
	f.sessions["cs_fee_1"] = &fakeSession{
		ID: "cs_fee_1", Status: "complete", PaymentStatus: "paid", Amount: 1200,
		Currency: "usd", Created: time.Now().Unix(), IntentID: "pi_fee_1",
		Fee: 59, FeeCurrency: "usd",
	}
	f.mu.Unlock()
	raw := call(t, s, "gateway.query_payment", map[string]any{"payment_id": "cs_fee_1"})
	var res struct {
		Metadata map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Metadata["catalyst_fee_minor"] != "59" || res.Metadata["catalyst_fee_currency"] != "USD" {
		t.Fatalf("fee metadata = %v", res.Metadata)
	}
}
