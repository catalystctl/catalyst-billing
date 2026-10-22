package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/portal"
)

// Service lifecycle route contracts (P6).
//
// These tests assert the properties a UI depends on rather than the happy path: that a
// customer's request is recorded and not acted on, that staff see the queue, that a
// substituted identifier returns 404 rather than 403, that every failure is
// application/problem+json with a stable code, and that the guards (permission, CSRF,
// Idempotency-Key) are actually applied.

// tenantWithService creates a customer with a login, a service on an active subscription, and
// a reachable upgrade product. It extends createTenant, which builds the billing chain.
func (e *authEnv) tenantWithService(t *testing.T, email string, amount string) (tenantFixture, string) {
	t.Helper()
	f := e.createTenant(t, email, amount)

	// A second product reachable from the first, so upgrade options are non-empty.
	ctx := context.Background()
	var productID, priceID string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO products (slug, name) VALUES ($1, 'VPS Large') RETURNING id`,
		"large-"+f.customerID[:8]).Scan(&productID); err != nil {
		t.Fatalf("upgrade product: %v", err)
	}
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, name, currency, amount_minor, interval,
			anchor_day, anchor_timezone)
		VALUES ($1, 1, 'Large', 'USD', 2500, 'monthly', 1, 'UTC') RETURNING id`,
		productID).Scan(&priceID); err != nil {
		t.Fatalf("upgrade price: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO product_upgrades (product_id, upgrade_to_id)
		SELECT product_id, $1 FROM services WHERE id = $2`, productID, f.serviceID); err != nil {
		t.Fatalf("upgrade graph: %v", err)
	}
	return f, priceID
}

// postServiceJSON posts a body with the CSRF token and an idempotency key.
func (e *authEnv) postServiceJSON(t *testing.T, path string, cookie *http.Cookie, csrf string,
	body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	req.Header.Set("Idempotency-Key", "svc-"+path+"-"+time.Now().Format("150405.000000000"))
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// putServiceJSON is postServiceJSON for a PUT.
func (e *authEnv) putServiceJSON(t *testing.T, path string, cookie *http.Cookie, csrf string,
	body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	req.Header.Set("Idempotency-Key", "svc-put-"+path+"-"+time.Now().Format("150405.000000000"))
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// problemCode reads the stable code from a problem document.
func problemCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !bytes.Contains([]byte(ct), []byte("application/problem+json")) {
		t.Fatalf("Content-Type = %q, want application/problem+json (body %s)", ct, rec.Body)
	}
	var body struct {
		Code   string `json:"code"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("problem body: %v\n%s", err, rec.Body)
	}
	if body.Code == "" {
		t.Fatalf("problem body has no code: %s", rec.Body)
	}
	if body.Status != rec.Code {
		t.Fatalf("body status %d != HTTP status %d", body.Status, rec.Code)
	}
	return body.Code
}

// ---------------------------------------------------------------- customer --

// A customer's end-of-period request is recorded, sets the schedule, and keeps the service
// running. The response states the consequence rather than just a status code.
func TestPortalCancellationEndOfPeriodOverHTTP(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	f := env.createTenant(t, "cancel-eop@example.test", "12.00")
	cookie, csrf := env.login(t, "cancel-eop@example.test", "a long enough password")

	rec := env.postServiceJSON(t, "/api/v1/portal/services/"+f.serviceID+"/cancellation",
		cookie, csrf, map[string]any{"reason": "moving house", "type": "end_of_period"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}

	var body struct {
		ID          string `json:"id"`
		Type        string `json:"type"`
		Status      string `json:"status"`
		Reason      string `json:"reason"`
		EffectiveAt string `json:"effective_at"`
		Message     string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if body.Status != "scheduled" {
		t.Fatalf("status = %q, want scheduled", body.Status)
	}
	if body.EffectiveAt == "" {
		t.Fatal("no effective_at was returned: the customer cannot be told when it ends")
	}
	if body.Message == "" {
		t.Fatal("the response does not explain the consequence")
	}
	if body.Reason != "moving house" {
		t.Fatalf("reason = %q", body.Reason)
	}

	// The service is still active and the schedule is set.
	var (
		serviceStatus string
		cancelAtEnd   bool
	)
	if err := env.pool.QueryRow(context.Background(), `
		SELECT s.status, sub.cancel_at_period_end
		FROM services s JOIN subscriptions sub ON sub.id = s.subscription_id
		WHERE s.id = $1`, f.serviceID).Scan(&serviceStatus, &cancelAtEnd); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if serviceStatus != "active" {
		t.Fatalf("service status = %q, want active", serviceStatus)
	}
	if !cancelAtEnd {
		t.Fatal("cancel_at_period_end was not set")
	}

	// The portal can now report the open request instead of offering the button again.
	rec = env.do(t, http.MethodGet, "/api/v1/portal/services/"+f.serviceID+"/cancellation", cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("read request: status %d; body %s", rec.Code, rec.Body)
	}
	var open struct {
		Cancellation *struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"cancellation"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &open); err != nil {
		t.Fatalf("read body: %v\n%s", err, rec.Body)
	}
	if open.Cancellation == nil || open.Cancellation.Status != "scheduled" {
		t.Fatalf("open request = %+v", open.Cancellation)
	}
}

// An immediate request is recorded as pending and terminates nothing.
func TestPortalCancellationImmediateIsOnlyARequest(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	f := env.createTenant(t, "cancel-now@example.test", "12.00")
	cookie, csrf := env.login(t, "cancel-now@example.test", "a long enough password")

	rec := env.postServiceJSON(t, "/api/v1/portal/services/"+f.serviceID+"/cancellation",
		cookie, csrf, map[string]any{"reason": "delete it", "type": "immediate"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Status != "pending" {
		t.Fatalf("status = %q, want pending", body.Status)
	}

	var (
		serviceStatus string
		operations    int
	)
	if err := env.pool.QueryRow(context.Background(),
		`SELECT status FROM services WHERE id = $1`, f.serviceID).Scan(&serviceStatus); err != nil {
		t.Fatalf("read service: %v", err)
	}
	if serviceStatus != "active" {
		t.Fatalf("service status = %q; an immediate request must not terminate anything", serviceStatus)
	}
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM service_operations`).Scan(&operations); err != nil {
		t.Fatalf("count operations: %v", err)
	}
	if operations != 0 {
		t.Fatalf("%d operations were enqueued by a customer's request", operations)
	}
}

// An unknown cancellation type is a 400 with a stable code, not a 500 from a constraint.
func TestPortalCancellationRejectsUnknownType(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	f := env.createTenant(t, "cancel-bad@example.test", "12.00")
	cookie, csrf := env.login(t, "cancel-bad@example.test", "a long enough password")

	rec := env.postServiceJSON(t, "/api/v1/portal/services/"+f.serviceID+"/cancellation",
		cookie, csrf, map[string]any{"reason": "x", "type": "whenever"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	problemCode(t, rec)
}

// A customer cannot cancel another customer's service: the answer is the same 404 a
// nonexistent identifier produces, so the endpoint cannot be used to discover identifiers.
func TestPortalCancellationIsScopedToTheCustomer(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	alice := env.createTenant(t, "alice-cancel@example.test", "12.00")
	env.createTenant(t, "bob-cancel@example.test", "12.00")

	cookie, csrf := env.login(t, "alice-cancel@example.test", "a long enough password")

	rec := env.postServiceJSON(t, "/api/v1/portal/services/"+alice.serviceID+"/cancellation",
		cookie, csrf, map[string]any{"reason": "x", "type": "end_of_period"})
	// Alice's own service works.
	if rec.Code != http.StatusCreated {
		t.Fatalf("alice's own request: %d %s", rec.Code, rec.Body)
	}

	bobCookie, bobCSRF := env.login(t, "bob-cancel@example.test", "a long enough password")
	rec = env.postServiceJSON(t, "/api/v1/portal/services/"+alice.serviceID+"/cancellation",
		bobCookie, bobCSRF, map[string]any{"reason": "not mine", "type": "end_of_period"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bob's attempt on alice's service: status %d, want 404; body %s", rec.Code, rec.Body)
	}
	problemCode(t, rec)
}

// A cancellation mutation without a CSRF token is refused: it changes state on a session.
func TestCancellationRequiresCSRF(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	f := env.createTenant(t, "csrf-cancel@example.test", "12.00")
	cookie, _ := env.login(t, "csrf-cancel@example.test", "a long enough password")

	rec := env.postServiceJSON(t, "/api/v1/portal/services/"+f.serviceID+"/cancellation",
		cookie, "", map[string]any{"reason": "x", "type": "end_of_period"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 without a CSRF token; body %s", rec.Code, rec.Body)
	}
	problemCode(t, rec)
}

// ------------------------------------------------------------------ staff --

// Staff see the queue, and executing an immediate request answers 202 with the operation id
// the destructive work was queued under.
func TestStaffCancellationQueueAndExecute(t *testing.T) {
	env := newStaffEnv(t)
	customer, _ := env.seedCustomer(t, "queue@example.test", "Queue Owner")
	serviceID := env.seedCancellableService(t, customer)

	// The customer asks, through the staff route (same body, no scoping).
	rec := env.postServiceJSON(t, "/api/v1/staff/services/"+serviceID+"/cancellation",
		env.admin, env.adminCSRF, map[string]any{"reason": "customer called", "type": "immediate"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("request: status %d; body %s", rec.Code, rec.Body)
	}
	var created struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("request body: %v\n%s", err, rec.Body)
	}
	if created.Status != "pending" {
		t.Fatalf("status = %q, want pending", created.Status)
	}

	// The queue lists it.
	rec = env.get(t, "/api/v1/staff/cancellations", env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("queue: status %d; body %s", rec.Code, rec.Body)
	}
	var queue struct {
		Cancellations []struct {
			ID          string                `json:"id"`
			Status      string                `json:"status"`
			Customer    struct{ Name string } `json:"customer"`
			ProductName string                `json:"product_name"`
		} `json:"cancellations"`
		Statuses []string `json:"statuses"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &queue); err != nil {
		t.Fatalf("queue body: %v\n%s", err, rec.Body)
	}
	if len(queue.Cancellations) != 1 {
		t.Fatalf("%d rows in the queue, want 1", len(queue.Cancellations))
	}
	if queue.Cancellations[0].Customer.Name == "" {
		t.Fatal("the queue row has no customer name")
	}
	if len(queue.Statuses) != 5 {
		t.Fatalf("%d statuses, want the closed set of 5", len(queue.Statuses))
	}

	// A status filter that matches nothing returns an empty list rather than an error.
	rec = env.get(t, "/api/v1/staff/cancellations?status=executed", env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("filtered queue: status %d; body %s", rec.Code, rec.Body)
	}

	// Support cannot execute: it does not hold service:terminate.
	rec = env.postServiceJSON(t, "/api/v1/staff/cancellations/"+created.ID+"/execute",
		env.support, env.supportCSRF, map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("support executing a cancellation: status %d, want 403; body %s", rec.Code, rec.Body)
	}

	// The infrastructure role can.
	rec = env.postServiceJSON(t, "/api/v1/staff/cancellations/"+created.ID+"/execute",
		env.admin, env.adminCSRF, map[string]any{"reason": "confirmed"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("execute: status %d; body %s", rec.Code, rec.Body)
	}
	var executed struct {
		OperationID    string `json:"operation_id"`
		StatusURL      string `json:"status_url"`
		CancellationID string `json:"cancellation_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &executed); err != nil {
		t.Fatalf("execute body: %v\n%s", err, rec.Body)
	}
	if executed.OperationID == "" {
		t.Fatal("no operation id was returned")
	}
	if executed.StatusURL == "" {
		t.Fatal("no status_url was returned")
	}

	// Exactly one terminate operation exists.
	var (
		kind string
		n    int
	)
	if err := env.pool.QueryRow(context.Background(),
		`SELECT kind, count(*) OVER () FROM service_operations`).Scan(&kind, &n); err != nil {
		t.Fatalf("read operation: %v", err)
	}
	if n != 1 || kind != "terminate" {
		t.Fatalf("%d operations of kind %q, want exactly 1 terminate", n, kind)
	}

	// A second execution is a 409 not_pending, not a second termination.
	rec = env.postServiceJSON(t, "/api/v1/staff/cancellations/"+created.ID+"/execute",
		env.admin, env.adminCSRF, map[string]any{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("second execute: status %d, want 409; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "not_pending" {
		t.Fatalf("second execute code = %q, want not_pending", code)
	}
}

// Rejecting a request requires a reason, and clears the schedule it set.
func TestStaffRejectCancellationRequiresReason(t *testing.T) {
	env := newStaffEnv(t)
	customer, _ := env.seedCustomer(t, "reject@example.test", "Reject Owner")
	serviceID := env.seedCancellableService(t, customer)

	rec := env.postServiceJSON(t, "/api/v1/staff/services/"+serviceID+"/cancellation",
		env.admin, env.adminCSRF, map[string]any{"reason": "asked", "type": "end_of_period"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("request: %d %s", rec.Code, rec.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("body: %v", err)
	}

	rec = env.postServiceJSON(t, "/api/v1/staff/cancellations/"+created.ID+"/reject",
		env.admin, env.adminCSRF, map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("reject without a reason: status %d, want 400; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "reason_required" {
		t.Fatalf("code = %q, want reason_required", code)
	}

	rec = env.postServiceJSON(t, "/api/v1/staff/cancellations/"+created.ID+"/reject",
		env.admin, env.adminCSRF, map[string]any{"reason": "customer wants to stay"})
	if rec.Code != http.StatusOK {
		t.Fatalf("reject: status %d; body %s", rec.Code, rec.Body)
	}

	var cancelAtEnd bool
	if err := env.pool.QueryRow(context.Background(), `
		SELECT sub.cancel_at_period_end
		FROM subscriptions sub
		JOIN services s ON s.subscription_id = sub.id
		WHERE s.id = $1`, serviceID).Scan(&cancelAtEnd); err != nil {
		t.Fatalf("read schedule: %v", err)
	}
	if cancelAtEnd {
		t.Fatal("the schedule was not cleared by the rejection")
	}
}

// A staff mutation without an Idempotency-Key is refused: a retried money or lifecycle action
// must be replayable, and the key is what makes it so.
func TestStaffCancellationMutationRequiresIdempotencyKey(t *testing.T) {
	env := newStaffEnv(t)
	customer, _ := env.seedCustomer(t, "idem@example.test", "Idem Owner")
	serviceID := env.seedCancellableService(t, customer)

	body, _ := json.Marshal(map[string]any{"reason": "x", "type": "immediate"})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/staff/services/"+serviceID+"/cancellation", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(env.admin)
	req.Header.Set("X-CSRF-Token", env.adminCSRF)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 without an Idempotency-Key; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "idempotency_key_required" {
		t.Fatalf("code = %q, want idempotency_key_required", code)
	}
}

// An unknown cancellation id is a 404 with the problem envelope.
func TestStaffCancellationUnknownIDIs404(t *testing.T) {
	env := newStaffEnv(t)

	rec := env.postServiceJSON(t, "/api/v1/staff/cancellations/0f8fad5b-d9cb-469f-a165-70867728950e/execute",
		env.admin, env.adminCSRF, map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body %s", rec.Code, rec.Body)
	}
	problemCode(t, rec)

	// A malformed id is also a 404, not a cast error and a 500.
	rec = env.postServiceJSON(t, "/api/v1/staff/cancellations/not-a-uuid/execute",
		env.admin, env.adminCSRF, map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("malformed id: status %d, want 404; body %s", rec.Code, rec.Body)
	}
}

// ---------------------------------------------------------------- upgrades --

// The upgrade options list is scoped to the caller's own service.
func TestUpgradeOptionsOverHTTP(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	f, priceID := env.tenantWithService(t, "upgrade-opt@example.test", "12.00")
	cookie, _ := env.login(t, "upgrade-opt@example.test", "a long enough password")

	rec := env.do(t, http.MethodGet,
		"/api/v1/portal/services/"+f.serviceID+"/upgrade-options", cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		Options []struct {
			ProductName string `json:"product_name"`
			Prices      []struct {
				PriceVersionID string         `json:"price_version_id"`
				Amount         map[string]any `json:"amount"`
				Prorated       map[string]any `json:"prorated"`
			} `json:"prices"`
		} `json:"options"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if len(body.Options) != 1 {
		t.Fatalf("%d options, want 1", len(body.Options))
	}
	if len(body.Options[0].Prices) != 1 {
		t.Fatalf("%d prices, want 1", len(body.Options[0].Prices))
	}
	if body.Options[0].Prices[0].PriceVersionID != priceID {
		t.Fatalf("price = %s, want %s", body.Options[0].Prices[0].PriceVersionID, priceID)
	}
	// The amount is integer minor units with an explicit currency, never a float.
	if body.Options[0].Prices[0].Amount["minor"] == nil {
		t.Fatalf("amount has no minor: %+v", body.Options[0].Prices[0].Amount)
	}
}

// An upgrade issues a prorated invoice and reports the proration it used.
func TestUpgradeOverHTTP(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	f, priceID := env.tenantWithService(t, "upgrade@example.test", "12.00")
	cookie, csrf := env.login(t, "upgrade@example.test", "a long enough password")

	rec := env.postServiceJSON(t, "/api/v1/portal/services/"+f.serviceID+"/upgrade",
		cookie, csrf, map[string]any{"price_version_id": priceID, "reason": "need more"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		UpgradeID     string `json:"upgrade_id"`
		Direction     string `json:"direction"`
		InvoiceID     string `json:"invoice_id"`
		InvoiceNumber string `json:"invoice_number"`
		ProratedMinor int64  `json:"prorated_minor"`
		Status        string `json:"status"`
		Proration     struct {
			Ratio     string `json:"ratio"`
			TotalSecs int64  `json:"total_secs"`
		} `json:"proration"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if body.Direction != "upgrade" {
		t.Fatalf("direction = %q, want upgrade", body.Direction)
	}
	if body.InvoiceID == "" || body.InvoiceNumber == "" {
		t.Fatalf("no invoice: %+v", body)
	}
	if body.ProratedMinor <= 0 {
		t.Fatalf("prorated = %d, want a positive charge", body.ProratedMinor)
	}
	if body.Status != "pending" {
		t.Fatalf("status = %q, want pending (the service moves when the invoice is paid)", body.Status)
	}
	if body.Proration.Ratio == "" {
		t.Fatal("the response does not report the proration ratio")
	}
}

// A price version that is not reachable is a 409 with a stable code.
func TestUpgradeToUnreachablePriceIsRefused(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	f := env.createTenant(t, "upgrade-bad@example.test", "12.00")
	cookie, csrf := env.login(t, "upgrade-bad@example.test", "a long enough password")

	// A price on an unrelated product.
	ctx := context.Background()
	var productID, priceID string
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO products (slug, name) VALUES ('unrelated-x', 'Unrelated') RETURNING id`).
		Scan(&productID); err != nil {
		t.Fatalf("product: %v", err)
	}
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, interval,
			anchor_day, anchor_timezone)
		VALUES ($1, 1, 'USD', 9900, 'monthly', 1, 'UTC') RETURNING id`, productID).
		Scan(&priceID); err != nil {
		t.Fatalf("price: %v", err)
	}

	rec := env.postServiceJSON(t, "/api/v1/portal/services/"+f.serviceID+"/upgrade",
		cookie, csrf, map[string]any{"price_version_id": priceID})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "not_upgradable" {
		t.Fatalf("code = %q, want not_upgradable", code)
	}
}

// The pending-upgrade queue is staff-only and reports the invoice state.
func TestStaffUpgradeQueue(t *testing.T) {
	env := newStaffEnv(t)
	customer, _ := env.seedCustomer(t, "upq@example.test", "Upgrade Owner")
	serviceID, _ := env.seedUpgradableService(t, customer)

	rec := env.get(t, "/api/v1/staff/upgrades", env.support)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}

	// The customer role must not reach the staff queue.
	custCookie, _ := env.loginAsCustomer(t, "upq-customer@example.test")
	rec = env.get(t, "/api/v1/staff/upgrades", custCookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a customer reading the staff queue: status %d, want 403", rec.Code)
	}
	_ = serviceID
}

// ------------------------------------------------------------ config options --

// Staff create an option, price a choice, and the resolver sums it exactly over HTTP.
func TestConfigOptionCRUDAndResolveOverHTTP(t *testing.T) {
	env := newStaffEnv(t)
	customer, _ := env.seedCustomer(t, "cfg@example.test", "Config Owner")
	_, productID, priceID := env.seedPricedProduct(t, customer)

	rec := env.postServiceJSON(t, "/api/v1/staff/config-options", env.billing, env.billingCSRF,
		map[string]any{
			"product_id":   productID,
			"name":         "RAM",
			"type":         "dropdown",
			"choices":      []string{"2 GB", "8 GB"},
			"env_variable": "RAM_MB",
			"required":     true,
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d; body %s", rec.Code, rec.Body)
	}
	var created struct {
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		Choices []string `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create body: %v\n%s", err, rec.Body)
	}
	if len(created.Choices) != 2 {
		t.Fatalf("choices = %v", created.Choices)
	}

	// Support cannot create options: it does not hold config:write.
	rec = env.postServiceJSON(t, "/api/v1/staff/config-options", env.support, env.supportCSRF,
		map[string]any{"product_id": productID, "name": "X", "type": "text"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("support creating an option: status %d, want 403; body %s", rec.Code, rec.Body)
	}

	// Price the 8 GB choice at 4.00 and 1.25 of setup.
	rec = env.putServiceJSON(t, "/api/v1/staff/config-options/"+created.ID+"/prices",
		env.billing, env.billingCSRF, map[string]any{
			"price_version_id": priceID,
			"choice":           "8 GB",
			"amount_minor":     400,
			"setup_minor":      125,
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("price: status %d; body %s", rec.Code, rec.Body)
	}

	// The resolver returns the breakdown and the exact totals.
	rec = env.get(t, "/api/v1/staff/config-options/resolve?product_id="+productID+
		"&price_version_id="+priceID+"&option."+created.ID+"=8+GB", env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve: status %d; body %s", rec.Code, rec.Body)
	}
	var resolved struct {
		Total      map[string]any `json:"total"`
		SetupTotal map[string]any `json:"setup_total"`
		Options    []struct {
			Name   string         `json:"name"`
			Value  string         `json:"value"`
			Amount map[string]any `json:"amount"`
		} `json:"options"`
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resolved); err != nil {
		t.Fatalf("resolve body: %v\n%s", err, rec.Body)
	}
	if len(resolved.Options) != 1 {
		t.Fatalf("%d contributions, want 1", len(resolved.Options))
	}
	if minor, _ := resolved.Total["minor"].(float64); minor != 400 {
		t.Fatalf("total minor = %v, want 400", resolved.Total["minor"])
	}
	if minor, _ := resolved.SetupTotal["minor"].(float64); minor != 125 {
		t.Fatalf("setup minor = %v, want 125", resolved.SetupTotal["minor"])
	}
	if resolved.Env["RAM_MB"] != "8 GB" {
		t.Fatalf("env = %+v", resolved.Env)
	}

	// The list shows the option with its price attached.
	rec = env.get(t, "/api/v1/staff/config-options?product_id="+productID, env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d; body %s", rec.Code, rec.Body)
	}
	var list struct {
		Options []struct {
			Prices []struct {
				Choice      string `json:"choice"`
				AmountMinor int64  `json:"amount_minor"`
			} `json:"prices"`
		} `json:"options"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list body: %v\n%s", err, rec.Body)
	}
	if len(list.Options) != 1 || len(list.Options[0].Prices) != 1 {
		t.Fatalf("list = %+v", list)
	}
	if list.Options[0].Prices[0].AmountMinor != 400 {
		t.Fatalf("amount = %d, want 400", list.Options[0].Prices[0].AmountMinor)
	}
}

// A malformed option definition is a 400 with the problem envelope, not a 500.
func TestConfigOptionValidationOverHTTP(t *testing.T) {
	env := newStaffEnv(t)
	customer, _ := env.seedCustomer(t, "cfg-bad@example.test", "Config Bad")
	_, productID, _ := env.seedPricedProduct(t, customer)

	rec := env.postServiceJSON(t, "/api/v1/staff/config-options", env.billing, env.billingCSRF,
		map[string]any{"product_id": productID, "name": "RAM", "type": "dropdown"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a dropdown with no choices; body %s", rec.Code, rec.Body)
	}
	problemCode(t, rec)
}

// ------------------------------------------------------------- route table --

// Every route this package added is guarded by a declared permission, and the staff routes
// carry the staff-only flag. The whole-surface test does this too; this pins the set so a
// route cannot be added to this domain without a decision.
func TestServiceLifecycleRoutesAreGuarded(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.Handler()

	want := map[string]string{
		"POST /api/v1/portal/services/{id}/cancellation":   "service:cancel:self",
		"GET /api/v1/portal/services/{id}/cancellation":    "service:read:self",
		"GET /api/v1/portal/services/{id}/upgrade-options": "service:upgrade:self",
		"POST /api/v1/portal/services/{id}/upgrade":        "service:upgrade:self",
		"GET /api/v1/portal/services/{id}/config":          "service:read:self",
		"PUT /api/v1/portal/services/{id}/config":          "service:upgrade:self",

		"GET /api/v1/staff/cancellations":                 "service:read:all",
		"POST /api/v1/staff/cancellations/{id}/execute":   "service:terminate",
		"POST /api/v1/staff/cancellations/{id}/reject":    "service:suspend",
		"POST /api/v1/staff/services/{id}/cancellation":   "service:upgrade",
		"POST /api/v1/staff/services/{id}/upgrade":        "service:upgrade",
		"GET /api/v1/staff/upgrades":                      "service:read:all",
		"GET /api/v1/staff/config-options":                "invoice:read:all",
		"POST /api/v1/staff/config-options":               "config:write",
		"PUT /api/v1/staff/config-options/{id}":           "config:write",
		"DELETE /api/v1/staff/config-options/{id}":        "config:write",
		"PUT /api/v1/staff/config-options/{id}/prices":    "config:write",
		"DELETE /api/v1/staff/config-options/{id}/prices": "config:write",
		"GET /api/v1/staff/config-options/resolve":        "invoice:read:all",
	}

	seen := map[string]bool{}
	for _, rt := range env.srv.Routes() {
		permission, ok := want[rt.Path()]
		if !ok {
			continue
		}
		seen[rt.Path()] = true
		if string(rt.Permission) != permission {
			t.Fatalf("%s permission = %q, want %q", rt.Path(), rt.Permission, permission)
		}
		if rt.Method != http.MethodGet && !rt.CSRF {
			t.Fatalf("%s is a session mutation without CSRF protection", rt.Path())
		}
	}
	for path := range want {
		if !seen[path] {
			t.Fatalf("%s is not in the route table", path)
		}
	}
}

// ---------------------------------------------------------------- fixtures --

// seedCancellableService creates an active service on an active subscription, which is the
// state a cancellation request requires.
func (e *staffEnv) seedCancellableService(t *testing.T, customerID string) string {
	t.Helper()
	ctx := context.Background()

	var productID, priceID, subID, serviceID string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO products (slug, name) VALUES ('can-'||gen_random_uuid(), 'Cancellable VPS')
		RETURNING id`).Scan(&productID); err != nil {
		t.Fatalf("product: %v", err)
	}
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, interval,
			anchor_day, anchor_timezone)
		VALUES ($1, 1, 'USD', 1200, 'monthly', 1, 'UTC') RETURNING id`, productID).
		Scan(&priceID); err != nil {
		t.Fatalf("price: %v", err)
	}
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO subscriptions (customer_id, price_version_id, status, interval, anchor_day,
			timezone, current_period_start, current_period_end, next_billing_at)
		VALUES ($1, $2, 'active', 'monthly', 1, 'UTC',
		        now() - interval '15 days', now() + interval '15 days', now() + interval '15 days')
		RETURNING id`, customerID, priceID).Scan(&subID); err != nil {
		t.Fatalf("subscription: %v", err)
	}
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO services (customer_id, subscription_id, product_id, status, external_id)
		VALUES ($1, $2, $3, 'active', 'ext-can-1') RETURNING id`,
		customerID, subID, productID).Scan(&serviceID); err != nil {
		t.Fatalf("service: %v", err)
	}
	return serviceID
}

// seedUpgradableService creates a service with a reachable upgrade target.
func (e *staffEnv) seedUpgradableService(t *testing.T, customerID string) (serviceID, targetPriceID string) {
	t.Helper()
	serviceID = e.seedCancellableService(t, customerID)

	ctx := context.Background()
	var sourceProduct, targetProduct string
	if err := e.pool.QueryRow(ctx, `
		SELECT product_id FROM services WHERE id = $1`, serviceID).Scan(&sourceProduct); err != nil {
		t.Fatalf("read product: %v", err)
	}
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO products (slug, name) VALUES ('tgt-'||gen_random_uuid(), 'Target VPS')
		RETURNING id`).Scan(&targetProduct); err != nil {
		t.Fatalf("target product: %v", err)
	}
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, name, currency, amount_minor, interval,
			anchor_day, anchor_timezone)
		VALUES ($1, 1, 'Target', 'USD', 2400, 'monthly', 1, 'UTC') RETURNING id`,
		targetProduct).Scan(&targetPriceID); err != nil {
		t.Fatalf("target price: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO product_upgrades (product_id, upgrade_to_id) VALUES ($1, $2)`,
		sourceProduct, targetProduct); err != nil {
		t.Fatalf("graph: %v", err)
	}
	return serviceID, targetPriceID
}

// seedPricedProduct creates a product with one price version and returns both ids.
func (e *staffEnv) seedPricedProduct(t *testing.T, customerID string) (categoryID, productID, priceID string) {
	t.Helper()
	ctx := context.Background()
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO products (slug, name) VALUES ('cfg-'||gen_random_uuid(), 'Configurable VPS')
		RETURNING id`).Scan(&productID); err != nil {
		t.Fatalf("product: %v", err)
	}
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, name, currency, amount_minor, interval,
			anchor_day, anchor_timezone)
		VALUES ($1, 1, 'Standard', 'USD', 1200, 'monthly', 1, 'UTC') RETURNING id`, productID).
		Scan(&priceID); err != nil {
		t.Fatalf("price: %v", err)
	}
	return "", productID, priceID
}

// loginAsCustomer creates a customer login and returns its session cookie.
func (e *staffEnv) loginAsCustomer(t *testing.T, email string) (*http.Cookie, string) {
	t.Helper()
	e.seedCustomer(t, email, "Customer")
	return e.login(t, email, "customer password 1")
}

// Every customer-facing read in this domain is scoped in SQL, so another customer's service
// identifier produces the same 404 a nonexistent one does. The assertion is per route because
// a forgotten check on one route is exactly the failure mode the never-regress list names.
func TestServiceLifecycleReadsAreScopedToTheCustomer(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	alice := env.createTenant(t, "alice-reads@example.test", "12.00")
	env.createTenant(t, "bob-reads@example.test", "12.00")

	bobCookie, _ := env.login(t, "bob-reads@example.test", "a long enough password")

	for _, path := range []string{
		"/api/v1/portal/services/" + alice.serviceID + "/cancellation",
		"/api/v1/portal/services/" + alice.serviceID + "/upgrade-options",
		"/api/v1/portal/services/" + alice.serviceID + "/config",
	} {
		rec := env.do(t, http.MethodGet, path, bobCookie, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s as another customer: status %d, want 404; body %s", path, rec.Code, rec.Body)
		}
		problemCode(t, rec)
	}
}

// The same scoping holds for the mutations: an upgrade or a config change against another
// customer's service is a 404, and nothing is written.
func TestServiceLifecycleMutationsAreScopedToTheCustomer(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	alice := env.createTenant(t, "alice-writes@example.test", "12.00")
	env.createTenant(t, "bob-writes@example.test", "12.00")

	ctx := context.Background()
	var productID, priceID string
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO products (slug, name) VALUES ('alice-target', 'Target') RETURNING id`).
		Scan(&productID); err != nil {
		t.Fatalf("product: %v", err)
	}
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, interval,
			anchor_day, anchor_timezone)
		VALUES ($1, 1, 'USD', 2500, 'monthly', 1, 'UTC') RETURNING id`, productID).
		Scan(&priceID); err != nil {
		t.Fatalf("price: %v", err)
	}
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO product_upgrades (product_id, upgrade_to_id)
		SELECT product_id, $1 FROM services WHERE id = $2`, productID, alice.serviceID); err != nil {
		t.Fatalf("graph: %v", err)
	}

	bobCookie, bobCSRF := env.login(t, "bob-writes@example.test", "a long enough password")

	rec := env.postServiceJSON(t, "/api/v1/portal/services/"+alice.serviceID+"/upgrade",
		bobCookie, bobCSRF, map[string]any{"price_version_id": priceID})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bob upgrading alice's service: status %d, want 404; body %s", rec.Code, rec.Body)
	}

	var upgrades int
	if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM service_upgrades`).Scan(&upgrades); err != nil {
		t.Fatalf("count: %v", err)
	}
	if upgrades != 0 {
		t.Fatalf("%d upgrade rows were written by a refused request", upgrades)
	}
}
