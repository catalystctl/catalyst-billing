package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStripe is a stand-in for Stripe's REST API, small enough to read and strict
// enough to catch a wrong request.
//
// It is strict on purpose. A permissive fake proves only that the adapter can
// parse its own output; this one rejects a missing Authorization header, records
// every Idempotency-Key and replays by it, and parses the form body the way Stripe
// does (nested keys, minor units as bare integers), so an adapter that sends
// `amount=10.00` or forgets the key fails here rather than in production.
type fakeStripe struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	seq      int
	sessions map[string]*fakeSession
	intents  map[string]*fakeIntent
	refunds  map[string]*fakeRefund
	byKey    map[string]string // idempotency key -> created object id
	// requests records what the adapter actually sent, for assertions.
	requests []recordedRequest
	// failNext makes the next POST answer with this error body and status.
	failNext *failMode
	// webhookSigningKey is what a valid delivery must be signed with.
	webhookKey string
}

type recordedRequest struct {
	Method         string
	Path           string
	IdempotencyKey string
	Form           map[string]string
	Auth           string
}

type failMode struct {
	status int
	body   string
}

type fakeSession struct {
	ID            string
	Status        string
	PaymentStatus string
	Amount        int64
	Currency      string
	Created       int64
	Metadata      map[string]string
	IntentID      string
}

type fakeIntent struct {
	ID             string
	Status         string
	Amount         int64
	AmountReceived int64
	AmountRefunded int64
	Currency       string
	Metadata       map[string]string
}

type fakeRefund struct {
	ID            string
	PaymentIntent string
	Amount        int64
	Currency      string
	Status        string
	Created       int64
}

func newFakeStripe(t *testing.T) *fakeStripe {
	t.Helper()
	f := &fakeStripe{
		t:          t,
		sessions:   make(map[string]*fakeSession),
		intents:    make(map[string]*fakeIntent),
		refunds:    make(map[string]*fakeRefund),
		byKey:      make(map[string]string),
		webhookKey: "whsec_test_secret",
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

// baseURL is what STRIPE_API_BASE must be set to.
func (f *fakeStripe) baseURL() string { return f.server.URL }

// config returns a config pointed at this fake.
func (f *fakeStripe) config() config {
	return config{
		secretKey:     "sk_test_123",
		webhookSecret: f.webhookKey,
		apiBase:       f.server.URL,
		currencies:    []string{"USD", "EUR"},
		timeout:       5 * time.Second,
		tolerance:     5 * time.Minute,
	}
}

func (f *fakeStripe) lastRequest() recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		f.t.Fatal("no request was recorded")
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakeStripe) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeStripe) failWith(status int, body string) {
	f.mu.Lock()
	f.failNext = &failMode{status: status, body: body}
	f.mu.Unlock()
}

func (f *fakeStripe) handle(w http.ResponseWriter, r *http.Request) {
	rec := recordedRequest{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization")}
	rec.IdempotencyKey = r.Header.Get("Idempotency-Key")
	if err := r.ParseForm(); err == nil {
		rec.Form = make(map[string]string, len(r.PostForm))
		for k := range r.PostForm {
			rec.Form[k] = r.PostForm.Get(k)
		}
	}

	f.mu.Lock()
	f.requests = append(f.requests, rec)
	fail := f.failNext
	f.failNext = nil
	f.mu.Unlock()

	if fail != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fail.status)
		fmt.Fprint(w, fail.body)
		return
	}
	// Stripe refuses an unauthenticated call before it looks at anything else.
	if !strings.HasPrefix(rec.Auth, "Bearer sk_") {
		writeStripeError(w, http.StatusUnauthorized, "authentication_error", "invalid_api_key",
			"Invalid API Key provided")
		return
	}

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/checkout/sessions":
		f.createSession(w, r, rec)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/checkout/sessions/"):
		f.getSession(w, strings.TrimPrefix(r.URL.Path, "/v1/checkout/sessions/"))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/checkout/sessions":
		f.listSessions(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/payment_intents/"):
		f.getIntent(w, strings.TrimPrefix(r.URL.Path, "/v1/payment_intents/"))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/refunds":
		f.createRefund(w, r, rec)
	default:
		writeStripeError(w, http.StatusNotFound, "invalid_request_error", "resource_missing",
			"No such resource: "+r.URL.Path)
	}
}

func (f *fakeStripe) createSession(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
	if rec.IdempotencyKey == "" {
		writeStripeError(w, http.StatusBadRequest, "invalid_request_error", "parameter_missing",
			"You must provide an Idempotency-Key for this operation")
		return
	}
	unitAmount := r.PostForm.Get("line_items[0][price_data][unit_amount]")
	currency := r.PostForm.Get("line_items[0][price_data][currency]")
	name := r.PostForm.Get("line_items[0][price_data][product_data][name]")
	successURL := r.PostForm.Get("success_url")
	if unitAmount == "" || currency == "" || successURL == "" {
		writeStripeError(w, http.StatusBadRequest, "invalid_request_error", "parameter_missing",
			"line_items[0][price_data][unit_amount], currency and success_url are required")
		return
	}
	var minor int64
	if _, err := fmt.Sscan(unitAmount, &minor); err != nil {
		writeStripeError(w, http.StatusBadRequest, "invalid_request_error", "parameter_invalid_integer",
			"unit_amount must be an integer in minor units")
		return
	}

	f.mu.Lock()
	if id, ok := f.byKey["create:"+rec.IdempotencyKey]; ok {
		sess := f.sessions[id]
		f.mu.Unlock()
		writeSession(w, sess)
		return
	}
	f.seq++
	id := fmt.Sprintf("cs_test_%d", f.seq)
	intentID := fmt.Sprintf("pi_test_%d", f.seq)
	now := time.Now().Unix()
	sess := &fakeSession{
		ID:            id,
		Status:        "open",
		PaymentStatus: "unpaid",
		Amount:        minor,
		Currency:      currency,
		Created:       now,
		Metadata:      metadataFromForm(r),
		IntentID:      intentID,
	}
	f.sessions[id] = sess
	f.intents[intentID] = &fakeIntent{
		ID: intentID, Status: "requires_payment_method", Amount: minor,
		Currency: currency, Metadata: metadataFromForm(r),
	}
	f.byKey["create:"+rec.IdempotencyKey] = id
	f.mu.Unlock()
	_ = name
	writeSession(w, sess)
}

func metadataFromForm(r *http.Request) map[string]string {
	out := map[string]string{}
	for key := range r.PostForm {
		if strings.HasPrefix(key, "metadata[") && strings.HasSuffix(key, "]") {
			out[strings.TrimSuffix(strings.TrimPrefix(key, "metadata["), "]")] = r.PostForm.Get(key)
		}
	}
	return out
}

func writeSession(w http.ResponseWriter, sess *fakeSession) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":             sess.ID,
		"object":         "checkout.session",
		"status":         sess.Status,
		"payment_status": sess.PaymentStatus,
		"amount_total":   sess.Amount,
		"currency":       sess.Currency,
		"created":        sess.Created,
		"expires_at":     sess.Created + 1800,
		"url":            "https://checkout.stripe.test/pay/" + sess.ID,
		"metadata":       sess.Metadata,
		"payment_intent": sess.IntentID,
	})
}

func (f *fakeStripe) getSession(w http.ResponseWriter, id string) {
	f.mu.Lock()
	sess, ok := f.sessions[id]
	f.mu.Unlock()
	if !ok {
		writeStripeError(w, http.StatusNotFound, "invalid_request_error", "resource_missing",
			"No such checkout.session: "+id)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":             sess.ID,
		"object":         "checkout.session",
		"status":         sess.Status,
		"payment_status": sess.PaymentStatus,
		"amount_total":   sess.Amount,
		"currency":       sess.Currency,
		"created":        sess.Created,
		"metadata":       sess.Metadata,
		"payment_intent": sess.IntentID,
	})
}

// listSessions implements the payment_intent filter Stripe exposes, which is how a
// webhook that names only the intent is resolved back to its session.
func (f *fakeStripe) listSessions(w http.ResponseWriter, r *http.Request) {
	intentID := r.URL.Query().Get("payment_intent")
	f.mu.Lock()
	var out []map[string]any
	for _, sess := range f.sessions {
		if intentID != "" && sess.IntentID != intentID {
			continue
		}
		out = append(out, map[string]any{"id": sess.ID})
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": out})
}

func (f *fakeStripe) getIntent(w http.ResponseWriter, id string) {
	f.mu.Lock()
	in, ok := f.intents[id]
	f.mu.Unlock()
	if !ok {
		writeStripeError(w, http.StatusNotFound, "invalid_request_error", "resource_missing",
			"No such payment_intent: "+id)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": in.ID, "object": "payment_intent", "status": in.Status,
		"amount": in.Amount, "amount_received": in.AmountReceived,
		"amount_refunded": in.AmountRefunded, "currency": in.Currency,
		"metadata": in.Metadata,
	})
}

func (f *fakeStripe) createRefund(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
	if rec.IdempotencyKey == "" {
		writeStripeError(w, http.StatusBadRequest, "invalid_request_error", "parameter_missing",
			"You must provide an Idempotency-Key for this operation")
		return
	}
	intentID := r.PostForm.Get("payment_intent")
	var amount int64
	if raw := r.PostForm.Get("amount"); raw != "" {
		if _, err := fmt.Sscan(raw, &amount); err != nil {
			writeStripeError(w, http.StatusBadRequest, "invalid_request_error", "parameter_invalid_integer",
				"amount must be an integer in minor units")
			return
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.byKey["refund:"+rec.IdempotencyKey]; ok {
		ref := f.refunds[id]
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": ref.ID, "object": "refund", "status": ref.Status,
			"amount": ref.Amount, "currency": ref.Currency, "created": ref.Created,
		})
		return
	}
	in, ok := f.intents[intentID]
	if !ok {
		writeStripeError(w, http.StatusNotFound, "invalid_request_error", "resource_missing",
			"No such payment_intent: "+intentID)
		return
	}
	outstanding := in.Amount - in.AmountRefunded
	if amount == 0 {
		amount = outstanding
	}
	if amount <= 0 || amount > outstanding {
		writeStripeError(w, http.StatusBadRequest, "invalid_request_error", "amount_too_large",
			fmt.Sprintf("Refund amount (%d) is greater than the refundable amount (%d)", amount, outstanding))
		return
	}
	f.seq++
	ref := &fakeRefund{
		ID: fmt.Sprintf("re_test_%d", f.seq), PaymentIntent: intentID,
		Amount: amount, Currency: in.Currency, Status: "succeeded", Created: time.Now().Unix(),
	}
	f.refunds[ref.ID] = ref
	f.byKey["refund:"+rec.IdempotencyKey] = ref.ID
	in.AmountRefunded += amount
	if in.AmountRefunded >= in.Amount {
		in.Status = "succeeded"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": ref.ID, "object": "refund", "status": ref.Status,
		"amount": ref.Amount, "currency": ref.Currency, "created": ref.Created,
	})
}

func writeStripeError(w http.ResponseWriter, status int, typ, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Request-Id", "req_test")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"type": typ, "code": code, "message": message},
	})
}

// markPaid makes a session and its intent paid, as the customer completing the
// hosted checkout would.
func (f *fakeStripe) markPaid(sessionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sess, ok := f.sessions[sessionID]
	if !ok {
		f.t.Fatalf("markPaid: no session %s", sessionID)
	}
	sess.Status = "complete"
	sess.PaymentStatus = "paid"
	if in, ok := f.intents[sess.IntentID]; ok {
		in.Status = "succeeded"
		in.AmountReceived = in.Amount
	}
}

// signWebhook builds the header a real delivery would carry.
func (f *fakeStripe) signWebhook(body []byte, at time.Time) string {
	return fmt.Sprintf("t=%d,v1=%s", at.Unix(), signPayload(f.webhookKey, at.Unix(), body))
}
