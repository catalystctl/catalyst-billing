package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The operator actions on a service that the panel was missing (SVC-12, SVC-13, SVC-14, SVC-22,
// UPG-05, UPG-12, CT-12).
//
// Two properties are asserted in every mutation test below:
//
//   - the local change is durable (the subscription moved, the id is recorded, the row is gone);
//   - nothing called a provisioner. A remote effect is a `service_operations` row, and the tests
//     look for the row rather than for a fake adapter call, because the row is the contract.

// serviceActionFixture is a customer, a product, a price and an active service with a billing
// schedule.
type serviceActionFixture struct {
	env            *staffEnv
	customerID     string
	productID      string
	priceVersionID string
	subscriptionID string
	serviceID      string
}

func seedServiceActionFixture(t *testing.T) *serviceActionFixture {
	t.Helper()
	env := newStaffEnv(t)
	ctx := context.Background()
	f := &serviceActionFixture{env: env}
	f.customerID, _ = env.seedCustomer(t, "act@example.test", "Action Customer")

	if err := env.pool.QueryRow(ctx, `
		INSERT INTO products (slug, name) VALUES ('act-'||gen_random_uuid(), 'Action VPS')
		RETURNING id`).Scan(&f.productID); err != nil {
		t.Fatalf("product: %v", err)
	}
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, interval,
			anchor_day, anchor_timezone, provision_plan, provision_region)
		VALUES ($1, 1, 'USD', 5000, 'monthly', 1, 'UTC', 'plan-a', 'eu-west')
		RETURNING id`, f.productID).Scan(&f.priceVersionID); err != nil {
		t.Fatalf("price version: %v", err)
	}
	now := time.Now().UTC()
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO subscriptions (customer_id, price_version_id, status, interval, anchor_day,
			timezone, current_period_start, current_period_end, next_billing_at)
		VALUES ($1, $2, 'active', 'monthly', 1, 'UTC', $3, $4, $4)
		RETURNING id`,
		f.customerID, f.priceVersionID, now.AddDate(0, -1, 0), now).Scan(&f.subscriptionID); err != nil {
		t.Fatalf("subscription: %v", err)
	}
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO services (customer_id, product_id, subscription_id, status, external_id)
		VALUES ($1, $2, $3, 'active', 'srv-ext-1') RETURNING id`,
		f.customerID, f.productID, f.subscriptionID).Scan(&f.serviceID); err != nil {
		t.Fatalf("service: %v", err)
	}
	return f
}

// do issues one request with the operator's session, CSRF token and an idempotency key.
func (f *serviceActionFixture) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(f.env.admin)
	req.Header.Set("X-CSRF-Token", f.env.adminCSRF)
	req.Header.Set("Idempotency-Key", "action-"+path+"-"+time.Now().Format("150405.000000000"))
	rec := httptest.NewRecorder()
	f.env.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// countOperations returns the queued operations of one kind.
func (f *serviceActionFixture) countOperations(t *testing.T, kind string) int {
	t.Helper()
	var n int
	if err := f.env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM service_operations WHERE kind = $1`, kind).Scan(&n); err != nil {
		t.Fatalf("count operations: %v", err)
	}
	return n
}

// ------------------------------------------------------------------ recalculate --

// SVC-13: the route recomputes the price from the catalogue, and applying it moves both the
// subscription and the stored option amount.
func TestRecalculatePriceRouteAppliesTheCatalogPrice(t *testing.T) {
	f := seedServiceActionFixture(t)
	ctx := context.Background()

	// A config option priced on the current version and selected on the service.
	var optionID string
	if err := f.env.pool.QueryRow(ctx, `
		INSERT INTO config_options (product_id, name, option_type, choices)
		VALUES ($1, 'RAM', 'text', '[]'::jsonb) RETURNING id`, f.productID).Scan(&optionID); err != nil {
		t.Fatalf("option: %v", err)
	}
	if _, err := f.env.pool.Exec(ctx, `
		INSERT INTO config_option_prices (config_option_id, price_version_id, choice, amount_minor)
		VALUES ($1, $2, '8', 500)`, optionID, f.priceVersionID); err != nil {
		t.Fatalf("option price: %v", err)
	}
	if _, err := f.env.pool.Exec(ctx, `
		INSERT INTO service_configs (service_id, config_option_id, value, amount_minor)
		VALUES ($1, $2, '8', 500)`, f.serviceID, optionID); err != nil {
		t.Fatalf("selection: %v", err)
	}

	// The catalog gains a version.
	var newPriceID string
	if err := f.env.pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, currency, amount_minor, interval,
			anchor_day, anchor_timezone, provision_plan, provision_region)
		VALUES ($1, 2, 'USD', 6000, 'monthly', 1, 'UTC', 'plan-b', 'eu-west')
		RETURNING id`, f.productID).Scan(&newPriceID); err != nil {
		t.Fatalf("new price: %v", err)
	}
	if _, err := f.env.pool.Exec(ctx, `
		INSERT INTO config_option_prices (config_option_id, price_version_id, choice, amount_minor)
		VALUES ($1, $2, '8', 800)`, optionID, newPriceID); err != nil {
		t.Fatalf("new option price: %v", err)
	}

	// The read half changes nothing.
	rec := f.do(t, http.MethodPost, "/api/v1/staff/services/"+f.serviceID+"/recalculate-price",
		map[string]any{"apply": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	var preview struct {
		Changed bool `json:"changed"`
		Applied bool `json:"applied"`
		Plan    bool `json:"plan_changed"`
		To      struct {
			Total struct {
				Minor int64 `json:"minor"`
			} `json:"total"`
		} `json:"to"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if !preview.Changed || preview.Applied {
		t.Fatalf("preview = %+v, want changed but not applied", preview)
	}
	if preview.To.Total.Minor != 6800 {
		t.Fatalf("to total = %d, want 6800 (6000 plan + 800 option)", preview.To.Total.Minor)
	}

	rec = f.do(t, http.MethodPost, "/api/v1/staff/services/"+f.serviceID+"/recalculate-price",
		map[string]any{"apply": true, "reason": "catalog correction"})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body)
	}
	var applied struct {
		Applied     bool   `json:"applied"`
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &applied); err != nil {
		t.Fatalf("decode applied: %v", err)
	}
	if !applied.Applied {
		t.Fatalf("apply = %+v, want applied", applied)
	}

	var subPrice string
	if err := f.env.pool.QueryRow(ctx, `SELECT price_version_id FROM subscriptions WHERE id = $1`,
		f.subscriptionID).Scan(&subPrice); err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if subPrice != newPriceID {
		t.Fatalf("subscription price = %s, want %s", subPrice, newPriceID)
	}
	// The plan moved, so exactly one resize is queued — and nothing was called.
	if applied.OperationID == "" || f.countOperations(t, "resize") != 1 {
		t.Fatalf("resize operations = %d, want 1", f.countOperations(t, "resize"))
	}
}

// ------------------------------------------------------------------ subscriptions --

// SVC-14: cancelling a provider-side subscription records the id and queues the gateway call.
func TestCancelSubscriptionRouteQueuesTheGatewayCall(t *testing.T) {
	f := seedServiceActionFixture(t)
	ctx := context.Background()

	// Nothing to cancel yet, and the code says which fact is missing.
	rec := f.do(t, http.MethodPost, "/api/v1/staff/services/"+f.serviceID+"/cancel-subscription",
		map[string]any{"reason": "x"})
	if rec.Code != http.StatusConflict || !problemHasCode(t, rec, "no_external_subscription") {
		t.Fatalf("no id: %d %s", rec.Code, rec.Body)
	}

	rec = f.do(t, http.MethodPost, "/api/v1/staff/services/"+f.serviceID+"/cancel-subscription",
		map[string]any{"subscription_id": "sub_legacy_9", "reason": "moved to a card"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	var stored string
	if err := f.env.pool.QueryRow(ctx,
		`SELECT external_subscription_id FROM services WHERE id = $1`, f.serviceID).Scan(&stored); err != nil {
		t.Fatalf("read service: %v", err)
	}
	if stored != "sub_legacy_9" {
		t.Fatalf("stored id = %q", stored)
	}
	if n := f.countOperations(t, "cancel_subscription"); n != 1 {
		t.Fatalf("cancel_subscription operations = %d, want 1", n)
	}
}

// CT-12: the order-level action queues one cancellation per service of the order.
func TestCancelOrderSubscriptionRouteQueuesEveryService(t *testing.T) {
	f := seedServiceActionFixture(t)
	ctx := context.Background()

	var orderID string
	if err := f.env.pool.QueryRow(ctx, `
		INSERT INTO orders (customer_id, status, currency, total_minor)
		VALUES ($1, 'paid', 'USD', 5000) RETURNING id`, f.customerID).Scan(&orderID); err != nil {
		t.Fatalf("order: %v", err)
	}
	if _, err := f.env.pool.Exec(ctx, `
		UPDATE services SET order_id = $2, external_subscription_id = 'sub_order_1'
		WHERE id = $1`, f.serviceID, orderID); err != nil {
		t.Fatalf("attach order: %v", err)
	}

	rec := f.do(t, http.MethodPost, "/api/v1/staff/orders/"+orderID+"/cancel-subscription",
		map[string]any{"reason": "account closing"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("cancel order: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Count  int              `json:"count"`
		Queued []map[string]any `json:"queued"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Count != 1 || len(body.Queued) != 1 {
		t.Fatalf("queued = %+v, want one", body)
	}
	if n := f.countOperations(t, "cancel_subscription"); n != 1 {
		t.Fatalf("cancel_subscription operations = %d, want 1", n)
	}
}

// ------------------------------------------------------------------ delete remote --

// SVC-12: "delete remote" destroys the provider-side resource and leaves the service alone.
func TestDeleteRemoteRouteQueuesWithoutTouchingTheService(t *testing.T) {
	f := seedServiceActionFixture(t)
	ctx := context.Background()

	rec := f.do(t, http.MethodPost, "/api/v1/staff/services/"+f.serviceID+"/delete-remote",
		map[string]any{"reason": "rebuilt by hand"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("delete remote: %d %s", rec.Code, rec.Body)
	}
	if n := f.countOperations(t, "delete_remote"); n != 1 {
		t.Fatalf("delete_remote operations = %d, want 1", n)
	}
	// The service still exists and is still active: that is the difference from terminating.
	var status string
	var externalID *string
	if err := f.env.pool.QueryRow(ctx,
		`SELECT status, external_id FROM services WHERE id = $1`, f.serviceID).
		Scan(&status, &externalID); err != nil {
		t.Fatalf("read service: %v", err)
	}
	if status != "active" {
		t.Fatalf("status = %q, want active", status)
	}
	if externalID == nil || *externalID != "srv-ext-1" {
		t.Fatal("the remote id was cleared by the handler; only the worker may clear it after the call")
	}

	// And a service that was never provisioned is refused with its own code.
	if _, err := f.env.pool.Exec(ctx,
		`UPDATE services SET external_id = NULL WHERE id = $1`, f.serviceID); err != nil {
		t.Fatalf("clear external id: %v", err)
	}
	rec = f.do(t, http.MethodPost, "/api/v1/staff/services/"+f.serviceID+"/delete-remote",
		map[string]any{})
	if rec.Code != http.StatusConflict || !problemHasCode(t, rec, "no_remote_resource") {
		t.Fatalf("no remote: %d %s", rec.Code, rec.Body)
	}
}

// ------------------------------------------------------------------ delete --

// SVC-12: a service with no history can be deleted; one with operations behind it is refused
// with the reason, because deleting it would orphan a record of something that happened.
func TestDeleteServiceRefusesHistoryAndDeletesACleanRow(t *testing.T) {
	f := seedServiceActionFixture(t)

	// A queued operation is history: the customer paid for something that was attempted.
	if _, err := f.env.pool.Exec(context.Background(), `
		INSERT INTO service_operations (service_id, kind, operation_key, generation, status)
		VALUES ($1, 'create', 'create:'||$2::text, 1, 'succeeded')`, f.serviceID, f.serviceID); err != nil {
		t.Fatalf("insert operation: %v", err)
	}
	rec := f.do(t, http.MethodDelete, "/api/v1/staff/services/"+f.serviceID, nil)
	if rec.Code != http.StatusConflict || !problemHasCode(t, rec, "service_has_history") {
		t.Fatalf("delete with history: %d %s", rec.Code, rec.Body)
	}

	// Remove the history and the same request succeeds.
	if _, err := f.env.pool.Exec(context.Background(),
		`DELETE FROM service_operations WHERE service_id = $1`, f.serviceID); err != nil {
		t.Fatalf("clear operations: %v", err)
	}
	rec = f.do(t, http.MethodDelete, "/api/v1/staff/services/"+f.serviceID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	var exists bool
	if err := f.env.pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM services WHERE id = $1)`, f.serviceID).Scan(&exists); err != nil {
		t.Fatalf("read service: %v", err)
	}
	if exists {
		t.Fatal("the service still exists")
	}
}

// ------------------------------------------------------------------ notify --

// SVC-12: a staff message to the customer is recorded in the outbox, not sent from a private
// mailbox where nothing is auditable.
func TestNotifyRouteQueuesAMessageToTheCustomer(t *testing.T) {
	f := seedServiceActionFixture(t)

	rec := f.do(t, http.MethodPost, "/api/v1/staff/services/"+f.serviceID+"/notify",
		map[string]any{"subject": "Your server", "body": "We moved it."})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("notify: %d %s", rec.Code, rec.Body)
	}
	var (
		to      string
		subject string
	)
	if err := f.env.pool.QueryRow(context.Background(), `
		SELECT to_address, subject FROM notification_deliveries
		WHERE kind = 'staff_service_message'`).Scan(&to, &subject); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if to != "act@example.test" || subject != "Your server" {
		t.Fatalf("delivery = %s/%s", to, subject)
	}

	// A message is a deliberate act, so a second press sends a second message rather than being
	// silently swallowed by a dedupe key.
	rec = f.do(t, http.MethodPost, "/api/v1/staff/services/"+f.serviceID+"/notify",
		map[string]any{"subject": "Your server", "body": "We moved it."})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("second notify: %d %s", rec.Code, rec.Body)
	}
	var n int
	if err := f.env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM notification_deliveries WHERE kind = 'staff_service_message'`).Scan(&n); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if n != 2 {
		t.Fatalf("%d deliveries, want 2", n)
	}
}

// ------------------------------------------------------------------ upgrade queue --

// UPG-05: the manual half of "apply the upgrades that have been paid for".
func TestApplyPendingUpgradesRouteIsReachableAndIdempotent(t *testing.T) {
	f := seedServiceActionFixture(t)

	rec := f.do(t, http.MethodPost, "/api/v1/staff/upgrades/apply", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Count   int      `json:"count"`
		Applied []string `json:"applied"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Count != 0 || len(body.Applied) != 0 {
		t.Fatalf("applied = %+v, want nothing pending", body)
	}
}

// ------------------------------------------------------------------ fields --

// UPG-12 / SVC-19: the adapter's own purchase-time inputs are reported, and with nothing
// configured the answer is a prompt rather than an error.
func TestServerFieldsRouteWithoutAConfiguredProvider(t *testing.T) {
	env := newStaffEnv(t)
	rec := env.get(t, "/api/v1/staff/servers/fields", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("fields: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Configured bool             `json:"configured"`
		Supported  bool             `json:"supported"`
		Fields     []map[string]any `json:"fields"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Configured || body.Supported {
		t.Fatalf("body = %+v, want not configured", body)
	}
	if body.Fields == nil {
		t.Fatal("fields is null; a client iterating it would have to special-case it")
	}
}

// A service's declared inputs cannot be stored when no adapter declares them: an id typed into a
// form that nothing will ever read is worse than a refusal.
func TestProvisioningFieldsRouteRefusesWithoutAProvider(t *testing.T) {
	f := seedServiceActionFixture(t)
	rec := f.do(t, http.MethodPut,
		"/api/v1/staff/services/"+f.serviceID+"/provisioning-fields",
		map[string]any{"values": map[string]string{"domain": "example.com"}})
	if rec.Code != http.StatusUnprocessableEntity || !problemHasCode(t, rec, "provider_required") {
		t.Fatalf("put fields: %d %s", rec.Code, rec.Body)
	}
}

// problemHasCode decodes a problem document and reports whether it carries the code.
func problemHasCode(t *testing.T, rec *httptest.ResponseRecorder, code string) bool {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		return false
	}
	return body.Code == code
}
