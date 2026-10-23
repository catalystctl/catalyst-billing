package httpapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Service metadata over HTTP (E2.6: SVC-02, SVC-03, SVC-04, SVC-07, SVC-08).
//
// These assert the properties a screen depends on rather than the happy path: that a customer's
// rename is scoped in SQL so another customer's identifier is a 404, that the operator's property
// record is not writable by a customer, that every refusal is application/problem+json with a
// stable code, and that the guards (permission, CSRF, Idempotency-Key) are actually applied.

// putServiceMetadata sends a PUT with the CSRF token and an idempotency key.
func (e *authEnv) putServiceMetadata(t *testing.T, path string, cookie *http.Cookie, csrf string,
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
	req.Header.Set("Idempotency-Key", "meta-"+path+"-"+time.Now().Format("150405.000000000"))
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// deleteServiceMetadata sends a DELETE with the CSRF token and an idempotency key.
func (e *authEnv) deleteServiceMetadata(t *testing.T, path string, cookie *http.Cookie,
	csrf string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	req.AddCookie(cookie)
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	req.Header.Set("Idempotency-Key", "meta-del-"+path+"-"+time.Now().Format("150405.000000000"))
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// metadataBody is the part of the response these tests read.
type metadataBody struct {
	ServiceID            string            `json:"service_id"`
	Label                string            `json:"label"`
	Description          string            `json:"description"`
	EffectiveLabel       string            `json:"effective_label"`
	EffectiveDescription string            `json:"effective_description"`
	DerivedDescription   string            `json:"derived_description"`
	CustomProperties     map[string]string `json:"custom_properties"`
	Quantity             int               `json:"quantity"`
	OrderID              *string           `json:"order_id"`
	Plan                 struct {
		PriceVersionID string `json:"price_version_id"`
		Name           string `json:"name"`
		Currency       string `json:"currency"`
		AmountMinor    int64  `json:"amount_minor"`
		Amount         struct {
			Minor    int64  `json:"minor"`
			Currency string `json:"currency"`
		} `json:"amount"`
	} `json:"plan"`
	Properties []struct {
		Key   string `json:"key"`
		Label string `json:"label"`
		Value string `json:"value"`
	} `json:"properties"`
}

func decodeMetadata(t *testing.T, rec *httptest.ResponseRecorder) metadataBody {
	t.Helper()
	var body metadataBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("metadata body: %v\n%s", err, rec.Body)
	}
	return body
}

// A customer renames their own service and the page keeps a description: the effective values are
// never empty, and the price travels as integer minor units.
func TestPortalServiceMetadataOverHTTP(t *testing.T) {
	env := newAuthEnv(t)
	tenant := env.createTenant(t, "meta-alice@example.test", "12.00")
	cookie, csrf := env.login(t, "meta-alice@example.test", "a long enough password")

	rec := env.get(t, "/api/v1/portal/services/"+tenant.serviceID+"/metadata", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("read: status %d; body %s", rec.Code, rec.Body)
	}
	body := decodeMetadata(t, rec)
	if body.EffectiveLabel != "VPS Small" || body.EffectiveDescription == "" {
		t.Fatalf("a fresh service is blank: %+v", body)
	}
	if body.Plan.AmountMinor != 1200 || body.Plan.Currency != "USD" || body.Plan.Amount.Minor != 1200 {
		t.Fatalf("plan price = %+v", body.Plan)
	}
	// Staff-only facts are not in the customer's view.
	if body.OrderID != nil {
		t.Fatalf("the customer's view carries the internal order id: %v", *body.OrderID)
	}

	rec = env.putServiceMetadata(t, "/api/v1/portal/services/"+tenant.serviceID+"/metadata",
		cookie, csrf, map[string]any{
			"label":       "web-01",
			"description": "The box that serves example.test.",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("rename: status %d; body %s", rec.Code, rec.Body)
	}
	body = decodeMetadata(t, rec)
	if body.Label != "web-01" || body.EffectiveLabel != "web-01" {
		t.Fatalf("label = %q / %q", body.Label, body.EffectiveLabel)
	}
	if body.EffectiveDescription != "The box that serves example.test." {
		t.Fatalf("description = %q", body.EffectiveDescription)
	}
}

// Another customer's service identifier answers 404 on both the read and the write, and nothing is
// written. That is the never-regress rule: the answer is the same as an id that does not exist.
func TestPortalServiceMetadataIsScopedToTheCustomer(t *testing.T) {
	env := newAuthEnv(t)
	alice := env.createTenant(t, "meta-scope-alice@example.test", "12.00")
	env.createTenant(t, "meta-scope-bob@example.test", "12.00")

	bobCookie, bobCSRF := env.login(t, "meta-scope-bob@example.test", "a long enough password")

	rec := env.get(t, "/api/v1/portal/services/"+alice.serviceID+"/metadata", bobCookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("read as another customer: status %d, want 404; body %s", rec.Code, rec.Body)
	}
	problemCode(t, rec)

	rec = env.putServiceMetadata(t, "/api/v1/portal/services/"+alice.serviceID+"/metadata",
		bobCookie, bobCSRF, map[string]any{"label": "stolen"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("write as another customer: status %d, want 404; body %s", rec.Code, rec.Body)
	}
	problemCode(t, rec)

	var label string
	if err := env.pool.QueryRow(t.Context(), `SELECT label FROM services WHERE id = $1`,
		alice.serviceID).Scan(&label); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if label != "" {
		t.Fatalf("a refused request wrote a label: %q", label)
	}
}

// A customer edits the properties the operator made visible, and naming an internal one is a 403
// with a stable code rather than a silent no-op.
//
// The rule is the definition's: `rack` is customer_visible so the customer may write it, `asset_tag`
// is not, so the same body that sets one is refused for the other — and the whole edit is refused,
// because a half-applied edit is a state the customer never asked for.
func TestPortalEditsOnlyCustomerVisibleProperties(t *testing.T) {
	env := newStaffEnv(t)
	tenant := env.createTenant(t, "meta-props@example.test", "12.00")
	cookie, csrf := env.login(t, "meta-props@example.test", "a long enough password")

	// Two definitions: one the customer may edit, one the operator kept internal.
	rec := env.putServiceJSON(t, "/api/v1/staff/service-properties/rack", env.support, env.supportCSRF,
		map[string]any{"label": "Rack", "customer_visible": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("define rack: status %d; body %s", rec.Code, rec.Body)
	}
	rec = env.putServiceJSON(t, "/api/v1/staff/service-properties/asset_tag", env.support, env.supportCSRF,
		map[string]any{"label": "Asset tag", "customer_visible": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("define asset_tag: status %d; body %s", rec.Code, rec.Body)
	}

	rec = env.putServiceMetadata(t, "/api/v1/portal/services/"+tenant.serviceID+"/metadata",
		cookie, csrf, map[string]any{"custom_properties": map[string]string{"rack": "B2"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("customer sets a visible property: status %d; body %s", rec.Code, rec.Body)
	}
	if body := decodeMetadata(t, rec); body.CustomProperties["rack"] != "B2" {
		t.Fatalf("stored properties = %v", body.CustomProperties)
	}

	rec = env.putServiceMetadata(t, "/api/v1/portal/services/"+tenant.serviceID+"/metadata",
		cookie, csrf, map[string]any{
			"custom_properties": map[string]string{"rack": "B3", "asset_tag": "CT-0042"},
		})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("customer sets an internal property: status %d, want 403; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "property_not_visible" {
		t.Fatalf("code = %q, want property_not_visible", code)
	}

	rec = env.get(t, "/api/v1/portal/services/"+tenant.serviceID+"/metadata", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-read: status %d; body %s", rec.Code, rec.Body)
	}
	body := decodeMetadata(t, rec)
	if body.CustomProperties["rack"] != "B2" {
		t.Fatalf("a refused edit wrote its visible key anyway: %v", body.CustomProperties)
	}
	if _, ok := body.CustomProperties["asset_tag"]; ok {
		t.Fatalf("the customer's view carries an internal property: %v", body.CustomProperties)
	}

	// A key with no definition at all is still the older, narrower refusal.
	rec = env.putServiceMetadata(t, "/api/v1/portal/services/"+tenant.serviceID+"/metadata",
		cookie, csrf, map[string]any{"custom_properties": map[string]string{"serial": "nope"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("undefined key: status %d, want 400; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "unknown_property" {
		t.Fatalf("code = %q, want unknown_property", code)
	}
}

// Staff read the whole record and edit the properties; a key with no definition is a 400 with a
// stable code rather than a constraint name.
func TestStaffServiceMetadataOverHTTP(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "meta-staff@example.test", "Meta Staff")
	serviceID, _ := env.seedService(t, customerID, "active")

	rec := env.putServiceJSON(t, "/api/v1/staff/service-properties/rack", env.support, env.supportCSRF,
		map[string]any{"label": "Rack", "show_on_invoice": true, "sort_order": 1})
	if rec.Code != http.StatusOK {
		t.Fatalf("define: status %d; body %s", rec.Code, rec.Body)
	}

	rec = env.putServiceJSON(t, "/api/v1/staff/services/"+serviceID+"/metadata",
		env.support, env.supportCSRF, map[string]any{
			"label":             "web-01",
			"description":       "Serves example.test.",
			"custom_properties": map[string]string{"rack": "B2"},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("edit: status %d; body %s", rec.Code, rec.Body)
	}
	body := decodeMetadata(t, rec)
	if body.CustomProperties["rack"] != "B2" {
		t.Fatalf("properties = %v", body.CustomProperties)
	}
	if len(body.Properties) != 1 || body.Properties[0].Label != "Rack" {
		t.Fatalf("properties = %+v", body.Properties)
	}

	// An undefined key is refused with a code the UI can point at.
	rec = env.putServiceJSON(t, "/api/v1/staff/services/"+serviceID+"/metadata",
		env.support, env.supportCSRF, map[string]any{
			"custom_properties": map[string]string{"serial": "nope"},
		})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown key: status %d, want 400; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "unknown_property" {
		t.Fatalf("code = %q, want unknown_property", code)
	}

	// A definition in use cannot be deleted: the values would have no label and no way to print.
	rec = env.deleteServiceMetadata(t, "/api/v1/staff/service-properties/rack",
		env.support, env.supportCSRF)
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete in use: status %d, want 409; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "property_in_use" {
		t.Fatalf("code = %q, want property_in_use", code)
	}
}

// A label longer than the column allows is a 400 with its own code, not a 500 from the CHECK.
func TestMetadataValidationOverHTTP(t *testing.T) {
	env := newAuthEnv(t)
	tenant := env.createTenant(t, "meta-invalid@example.test", "12.00")
	cookie, csrf := env.login(t, "meta-invalid@example.test", "a long enough password")

	long := bytes.Repeat([]byte("x"), 200)
	rec := env.putServiceMetadata(t, "/api/v1/portal/services/"+tenant.serviceID+"/metadata",
		cookie, csrf, map[string]any{"label": string(long)})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("long label: status %d, want 400; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "invalid_label" {
		t.Fatalf("code = %q, want invalid_label", code)
	}

	// A description that carries a bell is refused; one with a newline is a paragraph and is kept.
	rec = env.putServiceMetadata(t, "/api/v1/portal/services/"+tenant.serviceID+"/metadata",
		cookie, csrf, map[string]any{"description": "line one\x07line two"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("control character: status %d, want 400; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "invalid_description" {
		t.Fatalf("code = %q, want invalid_description", code)
	}
	rec = env.putServiceMetadata(t, "/api/v1/portal/services/"+tenant.serviceID+"/metadata",
		cookie, csrf, map[string]any{"description": "line one\nline two"})
	if rec.Code != http.StatusOK {
		t.Fatalf("paragraph: status %d; body %s", rec.Code, rec.Body)
	}
}

// Every route this package added is guarded by a declared permission, and the staff routes carry
// the staff-only flag. The whole-surface test does this too; this pins the set so a route cannot be
// added to this domain without a decision.
func TestServiceMetadataRoutesAreGuarded(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.Handler()

	want := map[string]string{
		"GET /api/v1/portal/services/{id}/metadata":     "service:read:self",
		"PUT /api/v1/portal/services/{id}/metadata":     "service:write:self",
		"GET /api/v1/staff/services/{id}/metadata":      "service:read:all",
		"PUT /api/v1/staff/services/{id}/metadata":      "service:write:all",
		"GET /api/v1/staff/service-properties":          "service:read:all",
		"PUT /api/v1/staff/service-properties/{key}":    "service:write:all",
		"DELETE /api/v1/staff/service-properties/{key}": "service:write:all",
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
		if rt.Method != http.MethodGet && !rt.Idempotent {
			t.Fatalf("%s is a mutation without an idempotency key", rt.Path())
		}
	}
	for path := range want {
		if !seen[path] {
			t.Fatalf("%s is not in the route table", path)
		}
	}
}

// A staff metadata edit without an idempotency key is refused: a retried edit is how a support
// agent's change lands twice.
func TestStaffMetadataEditRequiresAKey(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "meta-key@example.test", "Meta Key")
	serviceID, _ := env.seedService(t, customerID, "active")

	req := httptest.NewRequest(http.MethodPut, "/api/v1/staff/services/"+serviceID+"/metadata",
		bytes.NewReader([]byte(`{"label":"web-01"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(env.support)
	req.Header.Set("X-CSRF-Token", env.supportCSRF)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no idempotency key: status %d, want 400; body %s", rec.Code, rec.Body)
	}
	problemCode(t, rec)
}

// The support role holds service:write:all, so the properties screen is reachable; a customer
// session is not, and the refusal is a problem document.
func TestMetadataPermissionRefusals(t *testing.T) {
	env := newStaffEnv(t)
	tenant := env.createTenant(t, "meta-perm@example.test", "12.00")
	cookie, _ := env.login(t, "meta-perm@example.test", "a long enough password")

	if rec := env.get(t, "/api/v1/staff/service-properties", cookie); rec.Code != http.StatusForbidden {
		t.Fatalf("customer reading definitions: status %d, want 403; body %s", rec.Code, rec.Body)
	} else {
		problemCode(t, rec)
	}
	if rec := env.get(t, "/api/v1/staff/services/"+tenant.serviceID+"/metadata", cookie); rec.Code != http.StatusForbidden {
		t.Fatalf("customer reading staff metadata: status %d, want 403; body %s", rec.Code, rec.Body)
	}
	// A malformed id is a 404 with the problem envelope, not a 500.
	if rec := env.get(t, "/api/v1/staff/services/not-a-uuid/metadata", env.support); rec.Code != http.StatusNotFound {
		t.Fatalf("bad id: status %d, want 404; body %s", rec.Code, rec.Body)
	} else {
		problemCode(t, rec)
	}
}
