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

// Billing profile tests (Gate B4).
//
// The property under test is not "the field round-trips". It is that a profile edit is an
// input to the *next* document: it must not rewrite an issued invoice's snapshot, and it must
// be visible to the next one. Those two are what a customer and an accountant respectively
// care about.

func TestBillingProfileRoundTrip(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "profile@example.test", "Profile Owner")

	rec := env.putJSON(t, "/api/v1/staff/customers/"+customerID+"/billing-profile",
		env.admin, env.adminCSRF, map[string]any{
			"legal_name":    "Profile Owner GmbH",
			"address_line1": "Hauptstrasse 1",
			"address_line2": "Hinterhaus",
			"city":          "Berlin",
			"postal_code":   "10115",
			"region":        "Berlin",
			"country_code":  "DE",
			"tax_id":        "DE123456789",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("save: status %d; body %s", rec.Code, rec.Body)
	}

	got := env.get(t, "/api/v1/staff/customers/"+customerID+"/billing-profile", env.admin)
	if got.Code != http.StatusOK {
		t.Fatalf("read: status %d; body %s", got.Code, got.Body)
	}
	var body struct {
		LegalName   string `json:"legal_name"`
		CountryCode string `json:"country_code"`
		TaxID       string `json:"tax_id"`
		City        string `json:"city"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v\n%s", err, got.Body)
	}
	if body.LegalName != "Profile Owner GmbH" || body.CountryCode != "DE" ||
		body.TaxID != "DE123456789" || body.City != "Berlin" {
		t.Fatalf("profile = %+v", body)
	}

	// Saving twice updates rather than duplicating: a customer either has a profile or does
	// not, and "save" must not require the client to know which.
	rec = env.putJSON(t, "/api/v1/staff/customers/"+customerID+"/billing-profile",
		env.admin, env.adminCSRF, map[string]any{
			"legal_name":   "Profile Owner AG",
			"country_code": "DE",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("second save: status %d; body %s", rec.Code, rec.Body)
	}
	var rows int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM billing_profiles WHERE customer_id = $1`, customerID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("profile rows = %d, want 1", rows)
	}
}

// An invalid country code is refused with a message that names the field.
func TestBillingProfileValidation(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "badprofile@example.test", "Bad Profile")

	for name, body := range map[string]map[string]any{
		"lower case country": {"country_code": "de"},
		"three letters":      {"country_code": "DEU"},
		"digits":             {"country_code": "12"},
	} {
		rec := env.putJSON(t, "/api/v1/staff/customers/"+customerID+"/billing-profile",
			env.admin, env.adminCSRF, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400; body %s", name, rec.Code, rec.Body)
		}
		if !containsCode(rec.Body.Bytes(), "invalid_profile") {
			t.Fatalf("%s: body = %s", name, rec.Body)
		}
	}
}

// A customer edits their own profile; another customer's is not reachable through the portal
// route at all, because the customer id comes from the session.
func TestPortalBillingProfile(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	customerID, _ := env.seedCustomer(t, "selfprofile@example.test", "Self Profile")
	cookie, csrf := env.login(t, "selfprofile@example.test", "customer password 1")

	rec := env.putJSON(t, "/api/v1/portal/billing-profile", cookie, csrf, map[string]any{
		"legal_name":   "Self Profile Ltd",
		"country_code": "GB",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("save: status %d; body %s", rec.Code, rec.Body)
	}

	got := env.get(t, "/api/v1/portal/billing-profile", cookie)
	if got.Code != http.StatusOK {
		t.Fatalf("read: status %d; body %s", got.Code, got.Body)
	}
	var body struct {
		CustomerID  string `json:"customer_id"`
		LegalName   string `json:"legal_name"`
		CountryCode string `json:"country_code"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.CustomerID != customerID {
		t.Fatalf("customer_id = %s, want the session's customer %s", body.CustomerID, customerID)
	}
	if body.LegalName != "Self Profile Ltd" || body.CountryCode != "GB" {
		t.Fatalf("profile = %+v", body)
	}
}

// Editing a profile must not rewrite an issued invoice, and the next invoice must carry the
// new details. Both halves matter: the first is what a tax authority relies on, the second is
// what the customer expects after correcting their address.
func TestProfileChangeDoesNotRewriteIssuedInvoice(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "snapshot@example.test", "Snapshot Owner")

	// Issue one invoice with the original details.
	rec := env.postJSON(t, "/api/v1/staff/invoices", env.billing, env.billingCSRF, map[string]any{
		"customer_id": customerID,
		"issue_now":   true,
		"lines": []map[string]any{
			{"description": "Hosting", "quantity": 1, "unit_amount_minor": 1000, "currency": "USD"},
		},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("first invoice: %d %s", rec.Code, rec.Body)
	}
	var first struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}

	// Change the profile.
	if put := env.putJSON(t, "/api/v1/staff/customers/"+customerID+"/billing-profile",
		env.admin, env.adminCSRF, map[string]any{
			"legal_name":    "Snapshot Owner GmbH",
			"address_line1": "Neue Strasse 9",
			"city":          "Munich",
			"country_code":  "DE",
			"tax_id":        "DE999999999",
		}); put.Code != http.StatusOK {
		t.Fatalf("profile save: %d %s", put.Code, put.Body)
	}

	// The issued invoice's snapshot is unchanged.
	var snapshot []byte
	if err := env.pool.QueryRow(context.Background(),
		`SELECT snapshot FROM invoices WHERE id = $1`, first.ID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Customer struct {
			Name    string `json:"name"`
			Address string `json:"billing_address"`
			TaxID   string `json:"tax_id"`
		} `json:"customer"`
	}
	if err := json.Unmarshal(snapshot, &decoded); err != nil {
		t.Fatalf("snapshot: %v\n%s", err, snapshot)
	}
	if decoded.Customer.TaxID == "DE999999999" || decoded.Customer.Name == "Snapshot Owner GmbH" {
		t.Fatalf("an issued invoice's snapshot was rewritten: %+v", decoded.Customer)
	}

	// The next invoice carries the new details.
	rec = env.postJSON(t, "/api/v1/staff/invoices", env.billing, env.billingCSRF, map[string]any{
		"customer_id": customerID,
		"issue_now":   true,
		"lines": []map[string]any{
			{"description": "Hosting", "quantity": 1, "unit_amount_minor": 1000, "currency": "USD"},
		},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("second invoice: %d %s", rec.Code, rec.Body)
	}
	var second struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if err := env.pool.QueryRow(context.Background(),
		`SELECT snapshot FROM invoices WHERE id = $1`, second.ID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(snapshot, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Customer.TaxID != "DE999999999" {
		t.Fatalf("the next invoice does not carry the new tax id: %+v", decoded.Customer)
	}
	if decoded.Customer.Name != "Snapshot Owner GmbH" {
		t.Fatalf("the next invoice does not carry the new legal name: %+v", decoded.Customer)
	}
}

// A customer without a profile still produces a usable document: the customer record is the
// fallback, and refusing to invoice them would be worse than an invoice with only a name.
func TestProfileFallsBackToCustomerRecord(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "noprofile@example.test", "No Profile")

	got := env.get(t, "/api/v1/staff/customers/"+customerID+"/billing-profile", env.admin)
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", got.Code, got.Body)
	}
	var body struct {
		LegalName string `json:"legal_name"`
		TaxID     string `json:"tax_id"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.LegalName != "No Profile" {
		t.Fatalf("legal_name = %q, want the customer's name as a fallback", body.LegalName)
	}
	if body.TaxID != "" {
		t.Fatalf("tax_id = %q, want empty", body.TaxID)
	}
}

// The profile save is audited with a diff, so an accountant can see what changed and when.
func TestProfileSaveIsAudited(t *testing.T) {
	env := newStaffEnv(t)
	customerID, _ := env.seedCustomer(t, "auditprofile@example.test", "Audit Profile")

	if rec := env.putJSON(t, "/api/v1/staff/customers/"+customerID+"/billing-profile",
		env.admin, env.adminCSRF, map[string]any{
			"legal_name":   "Before Ltd",
			"country_code": "FR",
		}); rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	if rec := env.putJSON(t, "/api/v1/staff/customers/"+customerID+"/billing-profile",
		env.admin, env.adminCSRF, map[string]any{
			"legal_name":   "After Ltd",
			"country_code": "FR",
		}); rec.Code != http.StatusOK {
		t.Fatalf("second save: %d %s", rec.Code, rec.Body)
	}

	var changes []byte
	if err := env.pool.QueryRow(context.Background(), `
		SELECT changes FROM audit_events
		WHERE target_id = $1 AND action = 'customer.billing_profile_update'
		ORDER BY created_at DESC LIMIT 1`, customerID).Scan(&changes); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	var diff struct {
		Before map[string]any `json:"before"`
		After  map[string]any `json:"after"`
	}
	if err := json.Unmarshal(changes, &diff); err != nil {
		t.Fatalf("changes: %v\n%s", err, changes)
	}
	if diff.Before["legal_name"] != "Before Ltd" || diff.After["legal_name"] != "After Ltd" {
		t.Fatalf("audit diff = %+v", diff)
	}
}

// putJSON performs a PUT with the CSRF token and an idempotency key.
func (e *authEnv) putJSON(t *testing.T, path string, cookie *http.Cookie, csrf string, body any) *httptest.ResponseRecorder {
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
	req.Header.Set("Idempotency-Key", "put-"+path+"-"+time.Now().Format("150405.000000000"))
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}
