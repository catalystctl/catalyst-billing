package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/fulfilment"
	"github.com/catalyst/billing/internal/httpapi"
	"github.com/catalyst/billing/internal/payments"
)

// Saved payment methods over HTTP (PAY-10, PAY-11, PAY-13).
//
// The domain rules are proven in internal/payments. What is proven here is the boundary,
// and the boundary has one property that matters more than the rest: the provider token
// never appears in a response body. The assertions below therefore look at raw bytes as
// well as shapes; a struct assertion would pass the day someone added a token field.

type checkoutStub struct{ name string }

func (g checkoutStub) Name() string { return g.name }
func (g checkoutStub) CreateCheckout(context.Context, extproto.CreateCheckoutParams) (extproto.CreateCheckoutResult, error) {
	return extproto.CreateCheckoutResult{}, extproto.ErrInvalidParams
}

// savedGatewayStub is the configured extension for the hosted-agreement tests. It records
// what the handler asked for and answers as a provider would.
type savedGatewayStub struct {
	mu       sync.Mutex
	create   int
	query    int
	cancel   int
	lastPath string
}

func (s *savedGatewayStub) Call(_ context.Context, method string, params, result any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch method {
	case payments.MethodGatewayCreateBillingAgreement:
		s.create++
		s.lastPath = params.(*payments.CreateBillingAgreementParams).SuccessURL
		out := result.(*payments.CreateBillingAgreementResult)
		out.AgreementID = "cs_setup_stub"
		out.CheckoutURL = "https://checkout.stripe.test/cs_setup_stub"
		out.Status = "pending"
	case payments.MethodGatewayQueryBillingAgreement:
		s.query++
		out := result.(*payments.QueryBillingAgreementResult)
		out.AgreementID = "cs_setup_stub"
		out.Status = "succeeded"
		out.MethodToken = "pm_HTTP_SECRET_4242"
		out.ProviderCustomerID = "cus_stub"
		out.Brand = "visa"
		out.Last4 = "4242"
		out.ExpiryMonth = 12
		out.ExpiryYear = 2030
	case payments.MethodGatewayCancelBillingAgreement:
		s.cancel++
		out := result.(*payments.CancelBillingAgreementResult)
		out.Changed = true
	default:
		return extproto.ErrInvalidParams
	}
	return nil
}

func (s *savedGatewayStub) Info() extproto.InitializeResult {
	return extproto.InitializeResult{Capabilities: []string{payments.CapGatewayCharge}}
}
func (s *savedGatewayStub) Close() error { return nil }

func useSavedGateway(t *testing.T, gw httpapi.SavedGateway) {
	t.Helper()
	old := httpapi.OpenSavedGateway
	httpapi.OpenSavedGateway = func(context.Context, *httpapi.Server) (httpapi.SavedGateway, string, error) {
		return gw, "stripe", nil
	}
	t.Cleanup(func() { httpapi.OpenSavedGateway = old })
}

type httpCharger struct {
	mu    sync.Mutex
	calls []payments.OffSessionCharge
}

func (c *httpCharger) Name() string { return "fakegateway" }
func (c *httpCharger) ChargeOffSession(_ context.Context, req payments.OffSessionCharge) (payments.OffSessionResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, req)
	return payments.OffSessionResult{
		ProviderPaymentID: "pi_http_1",
		Status:            "succeeded",
		AmountMinor:       req.AmountMinor,
		FeeMinor:          59,
	}, nil
}
func (c *httpCharger) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func (e *authEnv) saveMethod(t *testing.T, cookie *http.Cookie, csrf string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return e.postJSON(t, "/api/v1/portal/payment-methods/saved", cookie, csrf, body)
}

func (e *authEnv) deleteMethod(t *testing.T, path string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	req.AddCookie(cookie)
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	req.Header.Set("Idempotency-Key", "delete-"+path+"-"+strings.ReplaceAll(t.Name(), "/", "-"))
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

type methodsBody struct {
	Methods []struct {
		ID          string `json:"id"`
		Provider    string `json:"provider"`
		Brand       string `json:"brand"`
		Last4       string `json:"last4"`
		Label       string `json:"label"`
		Display     string `json:"display"`
		Default     bool   `json:"default"`
		ExpiryMonth int    `json:"expiry_month"`
		ExpiryYear  int    `json:"expiry_year"`
	} `json:"methods"`
	HasDefault bool `json:"has_default"`
	OffSession struct {
		Enabled  bool   `json:"enabled"`
		Reason   string `json:"reason"`
		Provider string `json:"provider"`
	} `json:"off_session"`
}

func TestSavedPaymentMethodLifecycle(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portalFor(env))
	env.srv.WithGateway(checkoutStub{name: "fakegateway"})
	tenant := env.createTenant(t, "methods@example.test", "12.00")
	cookie, csrf := env.login(t, "methods@example.test", "a long enough password")

	const token = "pm_tok_live_SECRET_should_never_be_returned_4242"
	created := env.saveMethod(t, cookie, csrf, map[string]any{
		"token": token, "brand": "visa", "last4": "4242", "label": "Work card",
		"expiry_month": 12, "expiry_year": 2030, "terms_version": "recurring-2026-01",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("save: status %d; body %s", created.Code, created.Body)
	}
	if strings.Contains(created.Body.String(), token) {
		t.Fatalf("the save response echoed the provider token: %s", created.Body)
	}
	var saved map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	methodID, _ := saved["id"].(string)
	if methodID == "" {
		t.Fatalf("save body has no id: %s", created.Body)
	}
	if isDefault, _ := saved["default"].(bool); !isDefault {
		t.Fatal("the first saved method did not become the default")
	}

	list := env.get(t, "/api/v1/portal/payment-methods/saved", cookie)
	if list.Code != http.StatusOK {
		t.Fatalf("list: status %d; body %s", list.Code, list.Body)
	}
	if strings.Contains(list.Body.String(), token) {
		t.Fatalf("the list response contains the provider token: %s", list.Body)
	}
	var body methodsBody
	if err := json.Unmarshal(list.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Methods) != 1 {
		t.Fatalf("list has %d methods, want 1", len(body.Methods))
	}
	got := body.Methods[0]
	if got.Brand != "visa" || got.Last4 != "4242" || got.Display != "Visa ending 4242" {
		t.Fatalf("method = %+v, want visa ending 4242", got)
	}
	if got.Label != "Work card" || got.ExpiryMonth != 12 || got.ExpiryYear != 2030 {
		t.Fatalf("metadata = %+v", got)
	}
	if !body.HasDefault {
		t.Fatal("has_default is false after saving the first method")
	}
	if body.OffSession.Enabled {
		t.Fatalf("off_session.enabled = true with the setting off (%s)", body.OffSession.Reason)
	}
	if body.OffSession.Reason == "" {
		t.Fatal("off_session has no reason for being disabled")
	}

	detail := env.get(t, "/api/v1/portal/payment-methods/saved/"+methodID, cookie)
	if detail.Code != http.StatusOK {
		t.Fatalf("detail: status %d; body %s", detail.Code, detail.Body)
	}
	if strings.Contains(detail.Body.String(), token) {
		t.Fatalf("the detail response contains the provider token: %s", detail.Body)
	}

	// The mandate is a row, and its consent evidence came from the request.
	var status, terms, ip string
	if err := env.pool.QueryRow(context.Background(), `
		SELECT status, terms_version, COALESCE(consent_ip, '')
		FROM billing_agreements WHERE payment_method_id = $1`, methodID).
		Scan(&status, &terms, &ip); err != nil {
		t.Fatalf("read agreement: %v", err)
	}
	if status != "active" || terms != "recurring-2026-01" || ip == "" {
		t.Fatalf("agreement = %s/%s/%s", status, terms, ip)
	}

	// A second method does not steal the default.
	second := env.saveMethod(t, cookie, csrf, map[string]any{
		"token": "pm_tok_live_SECRET_second_4444", "brand": "mastercard", "last4": "4444",
	})
	if second.Code != http.StatusCreated {
		t.Fatalf("second save: status %d; body %s", second.Code, second.Body)
	}
	var secondBody map[string]any
	if err := json.Unmarshal(second.Body.Bytes(), &secondBody); err != nil {
		t.Fatal(err)
	}
	secondID, _ := secondBody["id"].(string)
	if isDefault, _ := secondBody["default"].(bool); isDefault {
		t.Fatal("the second method stole the default")
	}

	// Setting it default clears the previous one.
	def := env.postJSON(t, "/api/v1/portal/payment-methods/saved/"+secondID+"/default", cookie, csrf, map[string]any{})
	if def.Code != http.StatusOK {
		t.Fatalf("set default: status %d; body %s", def.Code, def.Body)
	}
	var defaults int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM payment_methods WHERE customer_id = $1 AND is_default`,
		tenant.customerID).Scan(&defaults); err != nil {
		t.Fatal(err)
	}
	if defaults != 1 {
		t.Fatalf("%d methods are default, want 1", defaults)
	}

	// Removing a method removes its mandate with it.
	if rec := env.deleteMethod(t, "/api/v1/portal/payment-methods/saved/"+methodID, cookie, csrf); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d; body %s", rec.Code, rec.Body)
	}
	var left int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM billing_agreements WHERE payment_method_id = $1`, methodID).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d mandates survived their method", left)
	}
}

// A customer cannot see or delete another customer's method.
func TestSavedPaymentMethodIsScopedToItsCustomer(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portalFor(env))
	env.srv.WithGateway(checkoutStub{name: "fakegateway"})
	env.createTenant(t, "alice-methods@example.test", "12.00")
	env.createTenant(t, "bob-methods@example.test", "12.00")
	alice, aliceCSRF := env.login(t, "alice-methods@example.test", "a long enough password")
	bob, _ := env.login(t, "bob-methods@example.test", "a long enough password")

	created := env.saveMethod(t, alice, aliceCSRF, map[string]any{
		"token": "pm_alice_secret", "brand": "visa", "last4": "4242",
	})
	var body map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("save: %s", created.Body)
	}

	if rec := env.get(t, "/api/v1/portal/payment-methods/saved/"+id, bob); rec.Code != http.StatusNotFound {
		t.Fatalf("bob reading alice's method: status %d, want 404", rec.Code)
	}
	if rec := env.get(t, "/api/v1/portal/payment-methods/saved", bob); rec.Code != http.StatusOK {
		t.Fatalf("bob's list: %d", rec.Code)
	} else if strings.Contains(rec.Body.String(), "4242") {
		t.Fatalf("bob sees alice's method: %s", rec.Body)
	}
}

// The hosted flow never takes a card number: the provider returns a method token, the
// handler stores it, and the response carries only display metadata.
func TestHostedAgreementAddsAMethodWithoutLeakingTheToken(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portalFor(env))
	env.srv.WithGateway(checkoutStub{name: "stripe"})
	stub := &savedGatewayStub{}
	useSavedGateway(t, stub)
	tenant := env.createTenant(t, "hosted@example.test", "12.00")
	cookie, csrf := env.login(t, "hosted@example.test", "a long enough password")

	start := env.postJSON(t, "/api/v1/portal/payment-methods/agreements", cookie, csrf, map[string]any{})
	if start.Code != http.StatusCreated {
		t.Fatalf("start: status %d; body %s", start.Code, start.Body)
	}
	var started struct {
		AgreementID string `json:"agreement_id"`
		RedirectURL string `json:"redirect_url"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.AgreementID == "" || started.RedirectURL == "" {
		t.Fatalf("start = %+v", started)
	}
	if stub.create != 1 {
		t.Fatalf("create calls = %d, want 1", stub.create)
	}
	if !strings.Contains(stub.lastPath, "agreement="+started.AgreementID) {
		t.Fatalf("success url %q does not name the local agreement", stub.lastPath)
	}

	confirm := env.get(t, "/api/v1/portal/payment-methods/agreements/"+started.AgreementID, cookie)
	if confirm.Code != http.StatusOK {
		t.Fatalf("confirm: status %d; body %s", confirm.Code, confirm.Body)
	}
	if strings.Contains(confirm.Body.String(), "pm_HTTP_SECRET_4242") {
		t.Fatalf("the confirmation response leaked the provider token: %s", confirm.Body)
	}
	var confirmed struct {
		Method struct {
			ID      string `json:"id"`
			Brand   string `json:"brand"`
			Last4   string `json:"last4"`
			Default bool   `json:"default"`
		} `json:"method"`
		Agreement struct {
			Status string `json:"status"`
		} `json:"agreement"`
	}
	if err := json.Unmarshal(confirm.Body.Bytes(), &confirmed); err != nil {
		t.Fatal(err)
	}
	if confirmed.Method.ID == "" || confirmed.Method.Brand != "visa" || confirmed.Method.Last4 != "4242" {
		t.Fatalf("confirmed method = %+v", confirmed.Method)
	}
	if confirmed.Agreement.Status != "active" {
		t.Fatalf("agreement status = %q", confirmed.Agreement.Status)
	}

	// The stored token is the provider's, and it is not stored anywhere in the response.
	var token string
	if err := env.pool.QueryRow(context.Background(), `
		SELECT provider_token FROM payment_methods WHERE id = $1`, confirmed.Method.ID).Scan(&token); err != nil {
		t.Fatal(err)
	}
	if token != "pm_HTTP_SECRET_4242" {
		t.Fatalf("stored token = %q", token)
	}
	var methodID, providerRef string
	if err := env.pool.QueryRow(context.Background(), `
		SELECT COALESCE(payment_method_id::text, ''), COALESCE(provider_agreement_id, '')
		FROM billing_agreements WHERE id = $1`, started.AgreementID).Scan(&methodID, &providerRef); err != nil {
		t.Fatal(err)
	}
	if methodID == "" || providerRef != "cs_setup_stub" {
		t.Fatalf("agreement = method %q ref %q", methodID, providerRef)
	}
	_ = tenant
}

// Staff can see the methods and the mandates behind them, without seeing a token.
func TestStaffSeesCustomerPaymentMethodsAndAgreements(t *testing.T) {
	env := newStaffEnv(t)
	env.srv.WithPortal(portalFor(env.authEnv))
	env.srv.WithGateway(checkoutStub{name: "fakegateway"})
	customerID, _ := env.seedCustomer(t, "visible@example.test", "Visible")
	cookie, csrf := env.login(t, "visible@example.test", "customer password 1")
	rec := env.saveMethod(t, cookie, csrf, map[string]any{
		"token": "pm_staff_visible_SECRET", "brand": "amex", "last4": "0005",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}

	staff := env.get(t, "/api/v1/staff/customers/"+customerID+"/payment-methods", env.admin)
	if staff.Code != http.StatusOK {
		t.Fatalf("staff list: %d %s", staff.Code, staff.Body)
	}
	if strings.Contains(staff.Body.String(), "pm_staff_visible_SECRET") {
		t.Fatalf("staff response contains the provider token: %s", staff.Body)
	}
	var body struct {
		Methods []struct {
			ID    string `json:"id"`
			Brand string `json:"brand"`
			Last4 string `json:"last4"`
		} `json:"methods"`
		Agreements []struct {
			Status string `json:"status"`
		} `json:"agreements"`
	}
	if err := json.Unmarshal(staff.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Methods) != 1 || body.Methods[0].Brand != "amex" || body.Methods[0].Last4 != "0005" {
		t.Fatalf("staff methods = %+v", body.Methods)
	}
	if len(body.Agreements) != 1 || body.Agreements[0].Status != "active" {
		t.Fatalf("staff agreements = %+v", body.Agreements)
	}
}

// Charging a renewal from the staff surface is idempotent, records the fee, and pays the
// invoice through the ordinary settlement path.
func TestStaffChargesASavedMethodIdempotently(t *testing.T) {
	env := newStaffEnv(t)
	env.srv.WithPortal(portalFor(env.authEnv))
	env.srv.WithGateway(checkoutStub{name: "fakegateway"})
	customerID, _ := env.seedCustomer(t, "autopay@example.test", "Autopay")
	due := time.Now().Add(-24 * time.Hour)
	invoiceID := env.seedInvoice(t, customerID, "open", "USD", 1200, 0, &due)

	// The operator's switch, written the way the settings screen writes it.
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO settings (group_key, key, value) VALUES ('gateway', 'off_session_enabled', 'true')
		ON CONFLICT (group_key, key) DO UPDATE SET value = 'true'`); err != nil {
		t.Fatalf("enable off-session: %v", err)
	}

	cookie, csrf := env.login(t, "autopay@example.test", "customer password 1")
	saved := env.saveMethod(t, cookie, csrf, map[string]any{
		"token": "pm_autopay_SECRET", "brand": "visa", "last4": "4242",
	})
	if saved.Code != http.StatusCreated {
		t.Fatalf("save: %d %s", saved.Code, saved.Body)
	}

	charger := &httpCharger{}
	env.srv.WithOffSessionCharging(charger, fulfilment.NewApplier(env.pool), "acct_test")
	t.Cleanup(func() { env.srv.WithOffSessionCharging(nil, nil, "") })

	rec := env.postJSONWithKey(t, "/api/v1/staff/invoices/"+invoiceID+"/charge-saved-method",
		env.admin, env.adminCSRF, map[string]any{}, "charge-"+invoiceID)
	if rec.Code != http.StatusOK {
		t.Fatalf("charge: status %d; body %s", rec.Code, rec.Body)
	}
	var out struct {
		Status    string                `json:"status"`
		PaymentID string                `json:"payment_id"`
		Amount    struct{ Minor int64 } `json:"amount"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "settled" || out.PaymentID != "pi_http_1" || out.Amount.Minor != 1200 {
		t.Fatalf("outcome = %+v", out)
	}
	if charger.callCount() != 1 {
		t.Fatalf("charger calls = %d, want 1", charger.callCount())
	}
	paid, status := env.invoicePaidMinor(t, invoiceID)
	if paid != 1200 || status != "paid" {
		t.Fatalf("invoice = %d/%s, want 1200/paid", paid, status)
	}
	var fee int64
	var methodID string
	if err := env.pool.QueryRow(context.Background(), `
		SELECT fee_minor, COALESCE(payment_method_id::text, '')
		FROM payments WHERE provider_payment_id = 'pi_http_1'`).Scan(&fee, &methodID); err != nil {
		t.Fatal(err)
	}
	if fee != 59 || methodID == "" {
		t.Fatalf("payment fee/method = %d/%q", fee, methodID)
	}

	// The same Idempotency-Key replays the stored response and does not charge twice.
	replay := env.postJSONWithKey(t, "/api/v1/staff/invoices/"+invoiceID+"/charge-saved-method",
		env.admin, env.adminCSRF, map[string]any{}, "charge-"+invoiceID)
	if replay.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", replay.Code, replay.Body)
	}
	if charger.callCount() != 1 {
		t.Fatalf("a replay charged again: %d calls", charger.callCount())
	}
}

// A decline leaves the invoice open and does not suspend anything. Dunning owns escalation.
func TestStaffChargeDeclineLeavesTheInvoiceOpen(t *testing.T) {
	env := newStaffEnv(t)
	env.srv.WithPortal(portalFor(env.authEnv))
	env.srv.WithGateway(checkoutStub{name: "fakegateway"})
	customerID, _ := env.seedCustomer(t, "declined-autopay@example.test", "Declined")
	due := time.Now().Add(-24 * time.Hour)
	invoiceID := env.seedInvoice(t, customerID, "open", "USD", 1200, 0, &due)
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO settings (group_key, key, value) VALUES ('gateway', 'off_session_enabled', 'true')
		ON CONFLICT (group_key, key) DO UPDATE SET value = 'true'`); err != nil {
		t.Fatalf("enable off-session: %v", err)
	}

	cookie, csrf := env.login(t, "declined-autopay@example.test", "customer password 1")
	if rec := env.saveMethod(t, cookie, csrf, map[string]any{
		"token": "pm_declined_SECRET", "brand": "visa", "last4": "4000",
	}); rec.Code != http.StatusCreated {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}

	decliner := &decliningCharger{}
	env.srv.WithOffSessionCharging(decliner, fulfilment.NewApplier(env.pool), "acct_test")
	t.Cleanup(func() { env.srv.WithOffSessionCharging(nil, nil, "") })

	rec := env.postJSONWithKey(t, "/api/v1/staff/invoices/"+invoiceID+"/charge-saved-method",
		env.admin, env.adminCSRF, map[string]any{}, "decline-"+invoiceID)
	if rec.Code != http.StatusOK {
		t.Fatalf("charge: status %d; body %s", rec.Code, rec.Body)
	}
	var out struct {
		Status string `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "declined" || !strings.Contains(out.Detail, "declined") {
		t.Fatalf("outcome = %+v", out)
	}
	paid, status := env.invoicePaidMinor(t, invoiceID)
	if paid != 0 || status != "open" {
		t.Fatalf("a declined charge changed the invoice to %d/%s", paid, status)
	}
	var payments int
	if err := env.pool.QueryRow(context.Background(), `SELECT count(*) FROM payments`).Scan(&payments); err != nil {
		t.Fatal(err)
	}
	if payments != 0 {
		t.Fatalf("a declined charge recorded %d payments", payments)
	}
}

type decliningCharger struct{}

func (decliningCharger) Name() string { return "fakegateway" }
func (decliningCharger) ChargeOffSession(context.Context, payments.OffSessionCharge) (payments.OffSessionResult, error) {
	return payments.OffSessionResult{
		Status: "failed", FailureCode: "card_declined", FailureMessage: "Your card was declined.",
	}, nil
}
