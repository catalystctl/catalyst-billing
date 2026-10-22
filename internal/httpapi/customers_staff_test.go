package httpapi_test

// The management surface: creating a customer and creating a service for one.
//
// These tests assert the properties the screens depend on rather than the screens themselves: the
// invitation really is the way in (create, follow the mailed link, set a password, sign in), a
// duplicate address is refused with a code the form can branch on, a service created for a customer
// that does not exist is a 404 rather than a 500, a sold-out product is refused exactly as checkout
// refuses it, and the audit row names the operator.

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// createCustomerBody is the smallest useful create request.
func createCustomerBody(email, name string) map[string]any {
	return map[string]any{
		"email": email,
		"name":  name,
		"billing_address": map[string]any{
			"legal_name":    name,
			"address_line1": "1 Example Street",
			"city":          "Berlin",
			"postal_code":   "10115",
			"country_code":  "DE",
		},
	}
}

// The whole point of the account: it exists, it has no usable password, and the mailed invitation is
// how the customer takes it over. This drives the real reset flow end to end.
func TestStaffCreateCustomerThenSignInThroughTheResetFlow(t *testing.T) {
	e := newStaffEnv(t)

	rec := e.postJSON(t, "/api/v1/staff/customers", e.admin, e.adminCSRF,
		createCustomerBody("phone-order@example.test", "Phone Order"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create customer: status %d, body %s", rec.Code, rec.Body)
	}
	// The response must never carry a credential. A password field here would be a credential in a
	// response body, a log and a browser history entry.
	for _, forbidden := range []string{"password", "password_hash", "credential"} {
		if strings.Contains(strings.ToLower(rec.Body.String()), forbidden) {
			t.Fatalf("the create response mentions %q: %s", forbidden, rec.Body)
		}
	}

	var created struct {
		Customer struct {
			ID       string `json:"id"`
			Email    string `json:"email"`
			HasLogin bool   `json:"has_login"`
		} `json:"customer"`
		InvitationSent bool `json:"invitation_sent"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if !created.Customer.HasLogin {
		t.Fatal("the created customer has no login, so the account would be unreachable")
	}
	if !created.InvitationSent {
		t.Fatal("no invitation was queued, so the customer never learns the account exists")
	}

	// The invitation is in the durable outbox, with the link the customer follows.
	var body string
	if err := e.pool.QueryRow(context.Background(), `
		SELECT body FROM notification_deliveries
		WHERE kind = 'account_created' AND to_address = $1
		ORDER BY created_at DESC LIMIT 1`, "phone-order@example.test").Scan(&body); err != nil {
		t.Fatalf("read the invitation: %v", err)
	}
	match := regexp.MustCompile(`token=([A-Za-z0-9_-]+)`).FindStringSubmatch(body)
	if match == nil {
		t.Fatalf("the invitation carries no reset link: %s", body)
	}

	// Redeem the token, then sign in with the password the customer chose. A staff-created account
	// is useless if this does not work, and it is the only thing that proves the account was created
	// without a password rather than with one nobody was told.
	reset := e.postAnonymously(t, "/api/v1/auth/password-reset/complete",
		map[string]any{"token": match[1], "password": "chosen password 1"})
	if reset.Code != http.StatusOK {
		t.Fatalf("reset complete: status %d, body %s", reset.Code, reset.Body)
	}
	cookie, _ := e.login(t, "phone-order@example.test", "chosen password 1")
	if cookie == nil {
		t.Fatal("the customer could not sign in after setting a password")
	}
}

// A duplicate address is refused, with a code the form can act on, and nothing is written.
func TestStaffCreateCustomerRefusesADuplicateAddress(t *testing.T) {
	e := newStaffEnv(t)

	first := e.postJSON(t, "/api/v1/staff/customers", e.admin, e.adminCSRF,
		createCustomerBody("dup-staff@example.test", "First"))
	if first.Code != http.StatusCreated {
		t.Fatalf("first create: status %d, body %s", first.Code, first.Body)
	}

	second := e.postJSON(t, "/api/v1/staff/customers", e.admin, e.adminCSRF,
		createCustomerBody("DUP-STAFF@example.test", "Second"))
	if second.Code != http.StatusConflict {
		t.Fatalf("duplicate create: status %d, body %s", second.Code, second.Body)
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Code != "email_taken" {
		t.Fatalf("duplicate create code = %q, want email_taken", problem.Code)
	}
}

// Creating a customer needs customer:write:all. A support agent can read customers and must not be
// able to invent one.
func TestStaffCreateCustomerRequiresTheWritePermission(t *testing.T) {
	e := newStaffEnv(t)
	rec := e.postJSON(t, "/api/v1/staff/customers", e.support, e.supportCSRF,
		createCustomerBody("not-allowed@example.test", "Nope"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("support create: status %d, want 403", rec.Code)
	}
}

// A service created by hand has a subscription the renewal engine will bill, no invoice, and an
// audit row naming the operator and the reason.
func TestStaffCreateServiceWritesTheSubscriptionAndNoInvoice(t *testing.T) {
	e := newStaffEnv(t)
	customerID, _ := e.seedCustomer(t, "manual-service@example.test", "Manual")
	_, priceVersionID := seedStaffProduct(t, e, "manual-svc", 5)

	rec := e.postJSON(t, "/api/v1/staff/services", e.admin, e.adminCSRF, map[string]any{
		"customer_id":      customerID,
		"price_version_id": priceVersionID,
		"reason":           "phone order paid by bank transfer",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create service: status %d, body %s", rec.Code, rec.Body)
	}

	var created struct {
		Service struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Origin string `json:"origin"`
		} `json:"service"`
		Subscription *struct {
			ID            string `json:"id"`
			Status        string `json:"status"`
			Interval      string `json:"interval"`
			NextBillingAt string `json:"next_billing_at"`
		} `json:"subscription"`
		Invoiced bool `json:"invoiced"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Subscription == nil {
		t.Fatal("no subscription was reported, so nothing would ever bill this service")
	}
	if created.Subscription.Status != "active" || created.Subscription.Interval != "monthly" {
		t.Fatalf("subscription = %+v, want an active monthly schedule", created.Subscription)
	}
	if created.Invoiced {
		t.Fatal("the response claims an invoice was raised; this path never raises one")
	}
	if created.Service.Origin != "manual" {
		t.Fatalf("origin = %q, want manual", created.Service.Origin)
	}

	// No invoice exists, which is the decision this endpoint makes: a manual service is a delivery,
	// not a demand for money.
	var invoices int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM invoices WHERE customer_id = $1`, customerID).Scan(&invoices); err != nil {
		t.Fatalf("count invoices: %v", err)
	}
	if invoices != 0 {
		t.Fatalf("invoices = %d, want 0", invoices)
	}

	// The audit row names the operator — the actor comes from the session, never from the body.
	var actor string
	if err := e.pool.QueryRow(context.Background(), `
		SELECT actor_id FROM audit_events
		WHERE action = 'service.create_manual' AND target_id = $1`, created.Service.ID).Scan(&actor); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if actor == "" {
		t.Fatal("the audit row does not name an operator")
	}
	var operatorID string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT id FROM users WHERE email = 'admin@example.test'`).Scan(&operatorID); err != nil {
		t.Fatalf("read operator: %v", err)
	}
	if actor != operatorID {
		t.Fatalf("audit actor = %q, want the signed-in operator %q", actor, operatorID)
	}
}

// A service for a customer that does not exist is a 404, not a 500 from a foreign-key violation.
func TestStaffCreateServiceForAMissingCustomerIs404(t *testing.T) {
	e := newStaffEnv(t)
	_, priceVersionID := seedStaffProduct(t, e, "missing-cust", 5)

	rec := e.postJSON(t, "/api/v1/staff/services", e.admin, e.adminCSRF, map[string]any{
		"customer_id":      "11111111-1111-1111-1111-111111111111",
		"price_version_id": priceVersionID,
		"reason":           "typo in the customer id",
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("create for a missing customer: status %d, body %s", rec.Code, rec.Body)
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", problem.Code)
	}
}

// A sold-out product cannot be handed out, which is the same rule checkout enforces.
func TestStaffCreateServiceRefusesASoldOutProduct(t *testing.T) {
	e := newStaffEnv(t)
	customerID, _ := e.seedCustomer(t, "sold-out@example.test", "Sold Out")
	_, priceVersionID := seedStaffProduct(t, e, "sold-out", 0)

	rec := e.postJSON(t, "/api/v1/staff/services", e.admin, e.adminCSRF, map[string]any{
		"customer_id":      customerID,
		"price_version_id": priceVersionID,
		"reason":           "operator tried anyway",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("create on a sold-out product: status %d, body %s", rec.Code, rec.Body)
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Code != "out_of_stock" {
		t.Fatalf("code = %q, want out_of_stock", problem.Code)
	}
}

// A comped service has no schedule behind it and raises nothing.
func TestStaffCreateServiceCompedRaisesNoInvoice(t *testing.T) {
	e := newStaffEnv(t)
	customerID, _ := e.seedCustomer(t, "comped-service@example.test", "Comped")
	_, priceVersionID := seedStaffProduct(t, e, "comped", 5)

	rec := e.postJSON(t, "/api/v1/staff/services", e.admin, e.adminCSRF, map[string]any{
		"customer_id":      customerID,
		"price_version_id": priceVersionID,
		"reason":           "staff account, comped",
		"comp":             true,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create comped service: status %d, body %s", rec.Code, rec.Body)
	}

	var created struct {
		Subscription any  `json:"subscription"`
		Comped       bool `json:"comped"`
		Invoiced     bool `json:"invoiced"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Subscription != nil {
		t.Fatalf("subscription = %v, want null for a comped service", created.Subscription)
	}
	if !created.Comped || created.Invoiced {
		t.Fatalf("comped = %v, invoiced = %v; want true/false", created.Comped, created.Invoiced)
	}

	var subscriptions, invoices int
	if err := e.pool.QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM subscriptions WHERE customer_id = $1),
		       (SELECT count(*) FROM invoices WHERE customer_id = $1)`, customerID).
		Scan(&subscriptions, &invoices); err != nil {
		t.Fatalf("count: %v", err)
	}
	if subscriptions != 0 || invoices != 0 {
		t.Fatalf("subscriptions = %d, invoices = %d; a comped service must have neither", subscriptions, invoices)
	}
}

// seedStaffProduct creates a product with one monthly price and the given stock, and returns both
// ids. `stock` of 0 is a sold-out product; the caller decides what that should mean.
func seedStaffProduct(t *testing.T, e *staffEnv, slug string, stock int) (productID, priceVersionID string) {
	t.Helper()
	ctx := context.Background()
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO products (slug, name, stock) VALUES ($1, $2, $3) RETURNING id`,
		slug+"-"+t.Name(), "Staff Created VPS", stock).Scan(&productID); err != nil {
		t.Fatalf("product: %v", err)
	}
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, interval,
			anchor_day, anchor_timezone)
		VALUES ($1, 1, 'USD', 2500, 'monthly', 1, 'UTC') RETURNING id`,
		productID).Scan(&priceVersionID); err != nil {
		t.Fatalf("price version: %v", err)
	}
	return productID, priceVersionID
}
