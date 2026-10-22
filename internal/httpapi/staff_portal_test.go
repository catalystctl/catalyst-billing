package httpapi_test

// Tests for the staff read-only view of a customer's portal.
//
// The interesting cases are not "does it return 200". They are the ways a view into somebody
// else's account could go wrong: a customer reaching the staff route, a support role that is
// allowed to read a customer's details but should not be able to see their billing, a malformed
// id leaking a 500, and the audit entry that makes the access answerable after the fact.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/catalyst/billing/internal/portal"
)

// TestStaffPortalViewShowsTheCustomersAccount is the happy path: an administrator opens a
// customer's portal and sees that customer's balance, invoices and services.
func TestStaffPortalViewShowsTheCustomersAccount(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))

	tenant := env.createTenant(t, "viewed@example.test", "42.00")
	admin := env.createUser(t, "admin-viewer@example.test", "a long enough password", "administrator")
	_ = admin
	adminCookie, _ := env.login(t, "admin-viewer@example.test", "a long enough password")

	rec := env.do(t, http.MethodGet, "/api/v1/staff/customers/"+tenant.customerID+"/portal", adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("staff portal view: %d %s", rec.Code, rec.Body)
	}

	var body struct {
		Customer struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"customer"`
		Balance struct {
			Outstanding []struct {
				Minor int64 `json:"minor"`
			} `json:"outstanding"`
		} `json:"balance"`
		RecentInvoices []struct {
			ID string `json:"id"`
		} `json:"recent_invoices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body %s", err, rec.Body)
	}
	if body.Customer.ID != tenant.customerID {
		t.Fatalf("view returned customer %q, want %q", body.Customer.ID, tenant.customerID)
	}
	if body.Customer.Email != "viewed@example.test" {
		t.Fatalf("view returned email %q", body.Customer.Email)
	}
	// The fixture issues a 42.00 invoice, so the outstanding line must carry it in minor units.
	if len(body.Balance.Outstanding) != 1 || body.Balance.Outstanding[0].Minor != 4200 {
		t.Fatalf("outstanding: %+v, want one line of 4200", body.Balance.Outstanding)
	}
	if len(body.RecentInvoices) != 1 || body.RecentInvoices[0].ID != tenant.invoiceID {
		t.Fatalf("recent invoices: %+v", body.RecentInvoices)
	}
}

// TestStaffPortalViewIsRefusedToCustomers is the one that matters most: the staff route is a
// separate path, and a customer session must not reach it. If this ever passes with a 200, a
// customer can read any other customer's account by guessing an id.
func TestStaffPortalViewIsRefusedToCustomers(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))

	alice := env.createTenant(t, "alice-view@example.test", "12.00")
	bob := env.createTenant(t, "bob-view@example.test", "25.00")
	aliceCookie, _ := env.login(t, "alice-view@example.test", "a long enough password")
	_ = alice

	for _, path := range []string{
		"/portal",
		"/portal/invoices",
		"/portal/services",
		"/portal/billing-profile",
	} {
		rec := env.do(t, http.MethodGet, "/api/v1/staff/customers/"+bob.customerID+path, aliceCookie, "")
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
			t.Fatalf("customer reached %s: %d %s", path, rec.Code, rec.Body)
		}
	}
}

// TestStaffPortalViewRequiresAuthentication: no session, no view, and the 401 must come before
// any work that would reveal whether the customer exists.
func TestStaffPortalViewRequiresAuthentication(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	tenant := env.createTenant(t, "anon-view@example.test", "9.00")

	for _, path := range []string{"/portal", "/portal/invoices", "/portal/services"} {
		rec := env.do(t, http.MethodGet, "/api/v1/staff/customers/"+tenant.customerID+path, nil, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s: %d, want 401", path, rec.Code)
		}
	}
}

// TestStaffPortalViewUnknownCustomerIsNotFound: an id that is well formed but names nobody is a
// 404, not an empty page. An empty portal for a customer who does not exist reads as "this
// customer has nothing", which is a lie an operator would act on.
func TestStaffPortalViewUnknownCustomerIsNotFound(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	env.createUser(t, "admin-missing@example.test", "a long enough password", "administrator")
	adminCookie, _ := env.login(t, "admin-missing@example.test", "a long enough password")

	rec := env.do(t, http.MethodGet,
		"/api/v1/staff/customers/00000000-0000-0000-0000-000000000000/portal", adminCookie, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown customer: %d %s, want 404", rec.Code, rec.Body)
	}
}

// TestStaffPortalViewMalformedIDIsNotFound: a non-UUID path is a 404 rather than a 500. The id
// reaches SQL, so it is validated first; otherwise a typo in a URL becomes an error page.
func TestStaffPortalViewMalformedIDIsNotFound(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	env.createUser(t, "admin-bad-id@example.test", "a long enough password", "administrator")
	adminCookie, _ := env.login(t, "admin-bad-id@example.test", "a long enough password")

	rec := env.do(t, http.MethodGet, "/api/v1/staff/customers/not-a-uuid/portal", adminCookie, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("malformed id: %d %s, want 404", rec.Code, rec.Body)
	}
}

// TestStaffPortalViewIsAudited: opening somebody else's account is recorded. A support team has
// to be able to answer "who looked at this account, and when", and the answer has to survive
// the operator's session.
func TestStaffPortalViewIsAudited(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	tenant := env.createTenant(t, "audited-view@example.test", "7.00")
	env.createUser(t, "admin-audit-view@example.test", "a long enough password", "administrator")
	adminCookie, _ := env.login(t, "admin-audit-view@example.test", "a long enough password")

	if rec := env.do(t, http.MethodGet, "/api/v1/staff/customers/"+tenant.customerID+"/portal/invoices", adminCookie, ""); rec.Code != http.StatusOK {
		t.Fatalf("view: %d %s", rec.Code, rec.Body)
	}

	var n int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_events
		WHERE action = 'customer.portal_view' AND target_id = $1 AND changes->>'view' = 'invoices'`,
		tenant.customerID).Scan(&n); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if n != 1 {
		t.Fatalf("audit entries for the invoice view: %d, want 1", n)
	}
}

// TestStaffPortalViewMatchesTheCustomersOwnView: the staff view and the customer's own overview
// must describe the same account identically. They are built by the same function, and this
// test is what stops someone later "improving" one of them.
func TestStaffPortalViewMatchesTheCustomersOwnView(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	tenant := env.createTenant(t, "same-view@example.test", "33.00")
	customerCookie, _ := env.login(t, "same-view@example.test", "a long enough password")

	env.createUser(t, "admin-same-view@example.test", "a long enough password", "administrator")
	adminCookie, _ := env.login(t, "admin-same-view@example.test", "a long enough password")

	own := env.do(t, http.MethodGet, "/api/v1/portal/overview", customerCookie, "")
	if own.Code != http.StatusOK {
		t.Fatalf("customer overview: %d %s", own.Code, own.Body)
	}
	staff := env.do(t, http.MethodGet, "/api/v1/staff/customers/"+tenant.customerID+"/portal", adminCookie, "")
	if staff.Code != http.StatusOK {
		t.Fatalf("staff overview: %d %s", staff.Code, staff.Body)
	}

	var a, b map[string]any
	if err := json.Unmarshal(own.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(staff.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatalf("the two views have different shapes: %d vs %d keys", len(a), len(b))
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			t.Fatalf("the customer view has %q but the staff view does not", k)
		}
	}
}

// TestStaffPortalViewDoesNotWidenPermissions is the subtle one.
//
// Infrastructure holds customer:read:all but deliberately not invoice:read:all — the role
// exists to run servers, and an invoice is a financial record. A portal view gated only on the
// customer permission would have handed that role every invoice in the system through a screen
// that looked like a convenience. Each section carries its own permission, and the sections a
// caller may not read are absent rather than empty.
func TestStaffPortalViewDoesNotWidenPermissions(t *testing.T) {
	env := newAuthEnv(t)
	env.srv.WithPortal(portal.New(env.pool))
	tenant := env.createTenant(t, "infra-view@example.test", "55.00")
	env.createUser(t, "infra-viewer@example.test", "a long enough password", "infrastructure")
	infraCookie, _ := env.login(t, "infra-viewer@example.test", "a long enough password")

	// The invoice list is refused outright.
	rec := env.do(t, http.MethodGet, "/api/v1/staff/customers/"+tenant.customerID+"/portal/invoices", infraCookie, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("infrastructure reading invoices: %d %s, want 403", rec.Code, rec.Body)
	}

	// The overview still works, because the role may read the customer and their services,
	// but it must not carry the invoice or balance sections.
	rec = env.do(t, http.MethodGet, "/api/v1/staff/customers/"+tenant.customerID+"/portal", infraCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("infrastructure overview: %d %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"open_invoices", "recent_invoices", "balance"} {
		if _, present := body[forbidden]; present {
			t.Fatalf("the overview leaked %q to a role without invoice:read:all: %v", forbidden, body)
		}
	}
	if _, present := body["customer"]; !present {
		t.Fatalf("the overview dropped the customer, which the role may read: %v", body)
	}
	if _, present := body["services"]; !present {
		t.Fatalf("the overview dropped services, which the role may read: %v", body)
	}
}
