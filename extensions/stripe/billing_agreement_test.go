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

// Billing agreements through the adapter's JSON-RPC surface (PAY-10, PAY-11, PAY-13).
//
// The assertions are on the bytes sent to Stripe and the shape returned to the core, not on
// Go structs: a renamed form key is a production failure that an in-process call would hide.

type agreementStripe struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	seq      int
	sessions map[string]*agreementSession
	byKey    map[string]string
	requests []agreementRequest
}

type agreementRequest struct {
	Method         string
	Path           string
	IdempotencyKey string
	Form           map[string]string
}

type agreementSession struct {
	ID        string
	Status    string
	Confirmed bool
	Created   int64
	ExpiresAt int64
	Customer  string
	MethodID  string
	Brand     string
	Last4     string
	ExpMonth  int
	ExpYear   int
}

func newAgreementStripe(t *testing.T) *agreementStripe {
	t.Helper()
	f := &agreementStripe{
		t:        t,
		sessions: make(map[string]*agreementSession),
		byKey:    make(map[string]string),
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *agreementStripe) config() config {
	return config{
		secretKey:     "sk_test_123",
		webhookSecret: "whsec_test",
		apiBase:       f.server.URL,
		currencies:    []string{"USD"},
		timeout:       5 * time.Second,
		tolerance:     5 * time.Minute,
		savedCards:    true,
	}
}

func (f *agreementStripe) lastRequest() agreementRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		f.t.Fatal("no request recorded")
	}
	return f.requests[len(f.requests)-1]
}

func (f *agreementStripe) confirm(t *testing.T, id string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sessions[id]
	if s == nil {
		t.Fatalf("no session %s", id)
	}
	s.Confirmed = true
	s.MethodID = "pm_visa_4242"
	s.Brand = "visa"
	s.Last4 = "4242"
	s.ExpMonth = 12
	s.ExpYear = 2030
}

func (f *agreementStripe) handle(w http.ResponseWriter, r *http.Request) {
	rec := agreementRequest{Method: r.Method, Path: r.URL.Path}
	rec.IdempotencyKey = r.Header.Get("Idempotency-Key")
	_ = r.ParseForm()
	rec.Form = make(map[string]string, len(r.PostForm))
	for k := range r.PostForm {
		rec.Form[k] = r.PostForm.Get(k)
	}
	f.mu.Lock()
	f.requests = append(f.requests, rec)
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/checkout/sessions":
		if rec.IdempotencyKey == "" {
			writeStripeError(w, http.StatusBadRequest, "invalid_request_error", "parameter_missing", "Idempotency-Key required")
			return
		}
		if rec.Form["mode"] != "setup" {
			writeStripeError(w, http.StatusBadRequest, "invalid_request_error", "parameter_invalid", "mode must be setup")
			return
		}
		f.mu.Lock()
		if id, ok := f.byKey["setup:"+rec.IdempotencyKey]; ok {
			s := f.sessions[id]
			f.mu.Unlock()
			fmt.Fprintf(w, `{"id":%q,"url":"https://checkout.stripe.test/%s","status":"open","expires_at":%d,"customer":"cus_test_1"}`,
				s.ID, s.ID, s.ExpiresAt)
			return
		}
		f.seq++
		id := fmt.Sprintf("cs_setup_%d", f.seq)
		now := time.Now().Unix()
		s := &agreementSession{ID: id, Status: "open", Created: now, ExpiresAt: now + 3600, Customer: "cus_test_1"}
		f.sessions[id] = s
		f.byKey["setup:"+rec.IdempotencyKey] = id
		f.mu.Unlock()
		fmt.Fprintf(w, `{"id":%q,"url":"https://checkout.stripe.test/%s","status":"open","expires_at":%d,"customer":"cus_test_1"}`,
			id, id, s.ExpiresAt)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/checkout/sessions/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/checkout/sessions/")
		f.mu.Lock()
		s := f.sessions[id]
		f.mu.Unlock()
		if s == nil {
			writeStripeError(w, http.StatusNotFound, "invalid_request_error", "resource_missing", "No such session")
			return
		}
		status := s.Status
		setupStatus := "requires_payment_method"
		var pm any = nil
		if s.Confirmed {
			status = "complete"
			setupStatus = "succeeded"
			pm = map[string]any{
				"id": s.MethodID, "customer": s.Customer,
				"card": map[string]any{"brand": s.Brand, "last4": s.Last4, "exp_month": s.ExpMonth, "exp_year": s.ExpYear},
			}
		}
		fmt.Fprintf(w, `{"id":%q,"status":%q,"created":%d,"customer":%q,"setup_intent":{"id":"seti_1","status":%q,"payment_method":%s}}`,
			s.ID, status, s.Created, s.Customer, setupStatus, mustJSON(pm))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/payment_intents":
		if rec.IdempotencyKey == "" {
			writeStripeError(w, http.StatusBadRequest, "invalid_request_error", "parameter_missing", "Idempotency-Key required")
			return
		}
		if rec.Form["payment_method"] == "pm_decline" {
			writeStripeError(w, http.StatusPaymentRequired, "card_error", "card_declined", "Your card was declined.")
			return
		}
		f.mu.Lock()
		if id, ok := f.byKey["charge:"+rec.IdempotencyKey]; ok {
			f.mu.Unlock()
			fmt.Fprintf(w, `{"id":%q,"status":"succeeded","amount":%s,"amount_received":%s,"currency":%q,"customer":"cus_test_1","latest_charge":{"balance_transaction":{"fee":59,"currency":"usd"}}}`,
				id, rec.Form["amount"], rec.Form["amount"], rec.Form["currency"])
			return
		}
		f.seq++
		id := fmt.Sprintf("pi_off_%d", f.seq)
		f.byKey["charge:"+rec.IdempotencyKey] = id
		f.mu.Unlock()
		fmt.Fprintf(w, `{"id":%q,"status":"succeeded","amount":%s,"amount_received":%s,"currency":%q,"customer":"cus_test_1","latest_charge":{"balance_transaction":{"fee":59,"currency":"usd"}}}`,
			id, rec.Form["amount"], rec.Form["amount"], rec.Form["currency"])
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/payment_methods/"):
		fmt.Fprint(w, `{"id":"pm_visa_4242","object":"payment_method"}`)
	default:
		writeStripeError(w, http.StatusNotFound, "invalid_request_error", "resource_missing", "No such resource")
	}
}

func mustJSON(v any) string {
	if v == nil {
		return "null"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func newAgreementServer(t *testing.T) (*server, *agreementStripe) {
	t.Helper()
	f := newAgreementStripe(t)
	cfg := f.config()
	return &server{client: newStripeClient(cfg), webhook: cfg.webhookSecret, cfg: cfg}, f
}

func TestCreateBillingAgreementStartsASetupSession(t *testing.T) {
	s, f := newAgreementServer(t)
	raw := call(t, s, "gateway.create_billing_agreement", map[string]any{
		"operation_key": "agreement-op-1",
		"customer_id":   "cust_1",
		"success_url":   "https://panel.test/portal/payment-methods?agreement=1",
		"cancel_url":    "https://panel.test/portal/payment-methods",
		"metadata":      map[string]string{"agreement_id": "ag_1"},
	})
	var res struct {
		AgreementID string `json:"agreement_id"`
		URL         string `json:"checkout_url"`
		Status      string `json:"status"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.AgreementID == "" || res.URL == "" || res.Status != "pending" {
		t.Fatalf("result = %+v", res)
	}
	req := f.lastRequest()
	if req.Form["mode"] != "setup" {
		t.Fatalf("mode = %q, want setup", req.Form["mode"])
	}
	if req.Form["success_url"] == "" || req.Form["cancel_url"] == "" {
		t.Fatalf("return urls missing: %v", req.Form)
	}
	if req.IdempotencyKey != "agreement-op-1" {
		t.Fatalf("idempotency key = %q", req.IdempotencyKey)
	}
	// A replay must not open a second provider session.
	again := call(t, s, "gateway.create_billing_agreement", map[string]any{
		"operation_key": "agreement-op-1", "customer_id": "cust_1",
		"success_url": "https://panel.test/portal/payment-methods?agreement=1",
	})
	var second struct {
		AgreementID string `json:"agreement_id"`
	}
	if err := json.Unmarshal(again, &second); err != nil {
		t.Fatal(err)
	}
	if second.AgreementID != res.AgreementID {
		t.Fatalf("replay created a second setup session: %q then %q", res.AgreementID, second.AgreementID)
	}
}

func TestQueryBillingAgreementReturnsTheTokenOnlyWhenConfirmed(t *testing.T) {
	s, f := newAgreementServer(t)
	var created struct {
		AgreementID string `json:"agreement_id"`
	}
	if err := json.Unmarshal(call(t, s, "gateway.create_billing_agreement", map[string]any{
		"operation_key": "agreement-op-2", "customer_id": "cust_1",
		"success_url": "https://panel.test/ok",
	}), &created); err != nil {
		t.Fatal(err)
	}

	raw := call(t, s, "gateway.query_billing_agreement", map[string]any{"agreement_id": created.AgreementID})
	var pending struct {
		Status      string `json:"status"`
		MethodToken string `json:"method_token"`
	}
	if err := json.Unmarshal(raw, &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Status != "pending" || pending.MethodToken != "" {
		t.Fatalf("pending result = %+v, want pending with no token", pending)
	}

	f.confirm(t, created.AgreementID)
	raw = call(t, s, "gateway.query_billing_agreement", map[string]any{"agreement_id": created.AgreementID})
	var got struct {
		Status             string `json:"status"`
		MethodToken        string `json:"method_token"`
		ProviderCustomerID string `json:"provider_customer_id"`
		Brand              string `json:"brand"`
		Last4              string `json:"last4"`
		ExpiryMonth        int    `json:"expiry_month"`
		ExpiryYear         int    `json:"expiry_year"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" || got.MethodToken != "pm_visa_4242" {
		t.Fatalf("confirmed result = %+v", got)
	}
	if got.Brand != "visa" || got.Last4 != "4242" || got.ExpiryMonth != 12 || got.ExpiryYear != 2030 {
		t.Fatalf("display metadata = %+v", got)
	}
	if got.ProviderCustomerID != "cus_test_1" {
		t.Fatalf("provider customer = %q", got.ProviderCustomerID)
	}
}

func TestChargeSavedMethodSendsOffSessionAndRecordsTheFee(t *testing.T) {
	s, f := newAgreementServer(t)
	raw := call(t, s, "gateway.charge", map[string]any{
		"operation_key":        "offsession:inv_1:pm_1",
		"customer_id":          "cust_1",
		"method_token":         "pm_visa_4242",
		"provider_customer_id": "cus_test_1",
		"amount":               map[string]any{"minor": 1200, "currency": "USD"},
		"description":          "Invoice inv_1",
	})
	var res struct {
		PaymentID string                `json:"payment_id"`
		Status    string                `json:"status"`
		Amount    struct{ Minor int64 } `json:"amount"`
		Fee       struct{ Minor int64 } `json:"fee"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.PaymentID == "" || res.Status != "succeeded" || res.Amount.Minor != 1200 || res.Fee.Minor != 59 {
		t.Fatalf("result = %+v", res)
	}
	req := f.lastRequest()
	if req.Form["amount"] != "1200" || req.Form["currency"] != "usd" {
		t.Fatalf("amount/currency = %q/%q, want 1200/usd", req.Form["amount"], req.Form["currency"])
	}
	if req.Form["payment_method"] != "pm_visa_4242" || req.Form["off_session"] != "true" || req.Form["confirm"] != "true" {
		t.Fatalf("charge form = %v", req.Form)
	}
	if req.Form["customer"] != "cus_test_1" {
		t.Fatalf("customer = %q", req.Form["customer"])
	}
	if req.IdempotencyKey != "offsession:inv_1:pm_1" {
		t.Fatalf("idempotency key = %q", req.IdempotencyKey)
	}
}

func TestChargeDeclineIsARejection(t *testing.T) {
	s, _ := newAgreementServer(t)
	err := callErr(t, s, "gateway.charge", map[string]any{
		"operation_key": "offsession:inv_1:decline",
		"customer_id":   "cust_1",
		"method_token":  "pm_decline",
		"amount":        map[string]any{"minor": 1200, "currency": "USD"},
	})
	if err.Code != codeRejected {
		t.Fatalf("code = %d, want %d", err.Code, codeRejected)
	}
	if !strings.Contains(err.Message, "declined") {
		t.Fatalf("message = %q", err.Message)
	}
}

func TestCancelBillingAgreementDetachesTheMethod(t *testing.T) {
	s, f := newAgreementServer(t)
	raw := call(t, s, "gateway.cancel_billing_agreement", map[string]any{
		"operation_key": "agreement-cancel-1",
		"agreement_id":  "cs_setup_1",
		"method_token":  "pm_visa_4242",
	})
	var res struct {
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("cancel did not report a change")
	}
	req := f.lastRequest()
	if req.Path != "/v1/payment_methods/pm_visa_4242/detach" || req.IdempotencyKey != "agreement-cancel-1" {
		t.Fatalf("cancel request = %+v", req)
	}
}

func TestSavedCardsCanBeSwitchedOff(t *testing.T) {
	f := newAgreementStripe(t)
	cfg := f.config()
	cfg.savedCards = false
	s := &server{client: newStripeClient(cfg), cfg: cfg}
	raw := call(t, s, "initialize", map[string]any{"protocol": "billing.ext/1", "versions": []int{2, 1}})
	var init struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &init); err != nil {
		t.Fatal(err)
	}
	for _, c := range init.Capabilities {
		if c == "gateway.charge" || c == "gateway.create_billing_agreement" {
			t.Fatalf("a disabled adapter advertised %s", c)
		}
	}
	err := callErr(t, s, "gateway.charge", map[string]any{
		"operation_key": "x", "customer_id": "c", "method_token": "pm_x",
		"amount": map[string]any{"minor": 1, "currency": "USD"},
	})
	if err.Code != codeMethodNotFound {
		t.Fatalf("charge with saved cards off: code %d, want %d", err.Code, codeMethodNotFound)
	}
}
