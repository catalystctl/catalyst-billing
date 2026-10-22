package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/authn"
)

// The four operator screens that had an API and no route to a human: holds, the upgrade graph,
// the sign-in provider registry and security.
//
// These tests assert the properties a screen actually depends on, and the ones a wrong
// implementation would break silently:
//
//   - A hold is visible with its reason and its author, and it changes no money: placing one
//     issues no invoice, and lifting one does not bill the period it covered.
//   - An upgrade edge round-trips, and a self-edge is a sentence (`self_edge`) rather than a
//     constraint violation surfacing as a 500.
//   - A provider's client secret is never in a response body — asserted on the bytes, not on a
//     struct, because a struct assertion passes while the field is still serialised.
//   - An operator can end somebody else's session, and cannot end the one they are holding the
//     screen with.

// admSession signs in a fresh administrator and returns the cookie and CSRF token.
func (e *authEnv) admSession(t *testing.T, email string) (*http.Cookie, string) {
	t.Helper()
	const password = "an administrator password"
	e.createUser(t, email, password, string(authn.RoleAdministrator))
	return e.login(t, email, password)
}

// admDelete sends an authenticated DELETE with the CSRF token and an idempotency key.
func (e *authEnv) admDelete(t *testing.T, path string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	req.AddCookie(cookie)
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	req.Header.Set("Idempotency-Key", "adm-"+path+"-"+time.Now().Format("150405.000000000"))
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// admCountInvoices counts every invoice, which is how "this action billed nobody" is proven.
func (e *authEnv) admCountInvoices(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM invoices`).Scan(&n); err != nil {
		t.Fatalf("count invoices: %v", err)
	}
	return n
}

// ---------------------------------------------------------------- holds --

// A hold is placed, appears in the register with who placed it and why, and bills nothing.
//
// "A hold stops automation, not the calendar" is the fact an operator most often gets wrong,
// so the register also carries the subscription's next billing date beside the hold.
func TestHoldRoundTripsAndDoesNotBill(t *testing.T) {
	env := newAuthEnv(t)
	tenant := env.createTenant(t, "held@example.test", "12.00")
	cookie, _ := env.admSession(t, "holds-admin@example.test")
	adminID := env.createUser(t, "holds-author@example.test", "an author password",
		string(authn.RoleSupport))
	authorCookie, authorCSRF := env.login(t, "holds-author@example.test", "an author password")

	before := env.admCountInvoices(t)

	// Placed by support, who hold service:hold and not service:suspend: the two are separate
	// instructions on purpose.
	rec := env.postServiceJSON(t, "/api/v1/staff/services/"+tenant.serviceID+"/holds",
		authorCookie, authorCSRF, map[string]any{
			"reason": "dispute over the June invoice; do not touch this box",
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("place hold: status %d; body %s", rec.Code, rec.Body)
	}
	var placed struct {
		ID        string `json:"id"`
		ServiceID string `json:"service_id"`
		CreatedBy string `json:"created_by"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &placed); err != nil {
		t.Fatalf("place hold body: %v\n%s", err, rec.Body)
	}
	if placed.CreatedBy != adminID {
		t.Fatalf("created_by = %q, want the session's user %q", placed.CreatedBy, adminID)
	}

	// The register is the screen's data source.
	list := env.get(t, "/api/v1/staff/holds", cookie)
	if list.Code != http.StatusOK {
		t.Fatalf("holds register: status %d; body %s", list.Code, list.Body)
	}
	var body struct {
		Data []struct {
			ID           string  `json:"id"`
			ServiceID    string  `json:"service_id"`
			Reason       string  `json:"reason"`
			ExpiresAt    *string `json:"expires_at"`
			Expired      bool    `json:"expired"`
			OpenInvoices int     `json:"open_invoices"`
			Customer     struct {
				ID    string `json:"id"`
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"customer"`
			PlacedBy *struct {
				UserID      string `json:"user_id"`
				Email       string `json:"email"`
				DisplayName string `json:"display_name"`
			} `json:"placed_by"`
			Subscription *struct {
				Status        string  `json:"status"`
				NextBillingAt *string `json:"next_billing_at"`
			} `json:"subscription"`
			ServiceStatus string `json:"service_status"`
		} `json:"data"`
		Counts map[string]int `json:"counts"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &body); err != nil {
		t.Fatalf("holds body: %v\n%s", err, list.Body)
	}
	var found bool
	for _, row := range body.Data {
		if row.ID != placed.ID {
			continue
		}
		found = true
		if row.Reason != "dispute over the June invoice; do not touch this box" {
			t.Fatalf("reason = %q", row.Reason)
		}
		if row.Customer.Email != "held@example.test" {
			t.Fatalf("customer email = %q", row.Customer.Email)
		}
		if row.PlacedBy == nil || row.PlacedBy.UserID != adminID || row.PlacedBy.Email != "holds-author@example.test" {
			t.Fatalf("placed_by = %+v, want the support agent who placed it", row.PlacedBy)
		}
		if row.Expired {
			t.Fatal("a hold with no expiry is reported as expired")
		}
		if row.ExpiresAt != nil {
			t.Fatalf("expires_at = %v, want null for a hold that lasts until cleared", *row.ExpiresAt)
		}
		// The schedule keeps running: this is the fact the screen states, and the response has
		// to be able to support the statement.
		if row.Subscription == nil || row.Subscription.Status != "active" ||
			row.Subscription.NextBillingAt == nil {
			t.Fatalf("subscription = %+v, want an active schedule with a next billing date", row.Subscription)
		}
		if row.ServiceStatus != "active" {
			t.Fatalf("service status = %q, want active: a hold does not suspend the service", row.ServiceStatus)
		}
	}
	if !found {
		t.Fatalf("the hold is not in the register: %s", list.Body)
	}
	if body.Counts["active"] < 1 {
		t.Fatalf("counts.active = %d, want at least the hold just placed", body.Counts["active"])
	}

	// Nothing billed. A hold is an instruction to automation, not a billing event.
	if after := env.admCountInvoices(t); after != before {
		t.Fatalf("placing a hold changed the invoice count: %d -> %d", before, after)
	}

	// The placement is audited in the same transaction as the row.
	var audited int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_events WHERE action = 'service.hold_add' AND target_id = $1`,
		tenant.serviceID).Scan(&audited); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if audited == 0 {
		t.Fatal("placing a hold wrote no audit row")
	}
}

// Lifting a hold bills nothing either: no catch-up invoice for the period the hold covered.
func TestLiftingHoldDoesNotRetroactivelyInvoice(t *testing.T) {
	env := newAuthEnv(t)
	tenant := env.createTenant(t, "lifted@example.test", "12.00")
	cookie, csrf := env.admSession(t, "lift-admin@example.test")

	rec := env.postServiceJSON(t, "/api/v1/staff/services/"+tenant.serviceID+"/holds",
		cookie, csrf, map[string]any{"reason": "waiting on the customer", "expires_in_seconds": 3600})
	if rec.Code != http.StatusCreated {
		t.Fatalf("place hold: status %d; body %s", rec.Code, rec.Body)
	}
	var placed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &placed); err != nil {
		t.Fatalf("place body: %v\n%s", err, rec.Body)
	}

	before := env.admCountInvoices(t)
	var nextBefore time.Time
	if err := env.pool.QueryRow(context.Background(), `
		SELECT next_billing_at FROM subscriptions WHERE id =
			(SELECT subscription_id FROM services WHERE id = $1)`, tenant.serviceID).
		Scan(&nextBefore); err != nil {
		t.Fatalf("read schedule: %v", err)
	}

	del := env.admDelete(t, "/api/v1/staff/holds/"+placed.ID, cookie, csrf)
	if del.Code != http.StatusNoContent {
		t.Fatalf("lift hold: status %d; body %s", del.Code, del.Body)
	}

	if after := env.admCountInvoices(t); after != before {
		t.Fatalf("lifting a hold issued %d invoice(s): %d -> %d", after-before, before, after)
	}
	var nextAfter time.Time
	if err := env.pool.QueryRow(context.Background(), `
		SELECT next_billing_at FROM subscriptions WHERE id =
			(SELECT subscription_id FROM services WHERE id = $1)`, tenant.serviceID).
		Scan(&nextAfter); err != nil {
		t.Fatalf("read schedule: %v", err)
	}
	if !nextAfter.Equal(nextBefore) {
		t.Fatalf("lifting a hold moved the billing date: %s -> %s", nextBefore, nextAfter)
	}

	// The register no longer shows it, and says so through the counts.
	list := env.get(t, "/api/v1/staff/holds", cookie)
	if list.Code != http.StatusOK {
		t.Fatalf("holds register: status %d; body %s", list.Code, list.Body)
	}
	if bytes.Contains(list.Body.Bytes(), []byte(placed.ID)) {
		t.Fatalf("the lifted hold is still in the active register: %s", list.Body)
	}

	var audited int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_events WHERE action = 'service.hold_clear' AND target_id = $1`,
		tenant.serviceID).Scan(&audited); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if audited == 0 {
		t.Fatal("lifting a hold wrote no audit row")
	}
}

// The register is a staff read: a customer must not see other people's holds.
func TestHoldsRegisterRequiresStaffReadAll(t *testing.T) {
	env := newAuthEnv(t)
	env.createTenant(t, "holds-customer@example.test", "12.00")
	cookie, _ := env.login(t, "holds-customer@example.test", "a long enough password")
	if rec := env.get(t, "/api/v1/staff/holds", cookie); rec.Code != http.StatusForbidden {
		t.Fatalf("customer reading the holds register: status %d, want 403", rec.Code)
	}
}

// ------------------------------------------------------------ upgrade graph --

// admProduct creates a priced product and returns its id.
func (e *authEnv) admProduct(t *testing.T, slug, name string, amountMinor int64) string {
	t.Helper()
	ctx := context.Background()
	var productID string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO products (slug, name) VALUES ($1, $2) RETURNING id`, slug, name).
		Scan(&productID); err != nil {
		t.Fatalf("create product: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO price_versions (product_id, version, name, currency, amount_minor, interval,
			anchor_day, anchor_timezone)
		VALUES ($1, 1, $2, 'USD', $3, 'monthly', 1, 'UTC')`, productID, name, amountMinor); err != nil {
		t.Fatalf("create price: %v", err)
	}
	return productID
}

// An edge round-trips, and a self-edge is a clean refusal rather than a 500.
func TestUpgradeEdgeRoundTripsAndSelfEdgeIsRefused(t *testing.T) {
	env := newAuthEnv(t)
	cookie, csrf := env.admSession(t, "graph-admin@example.test")
	small := env.admProduct(t, "graph-small", "Graph Small", 1200)
	large := env.admProduct(t, "graph-large", "Graph Large", 2500)

	rec := env.postServiceJSON(t, "/api/v1/staff/upgrade-graph", cookie, csrf,
		map[string]any{"product_id": small, "upgrade_to_id": large})
	if rec.Code != http.StatusCreated {
		t.Fatalf("add edge: status %d; body %s", rec.Code, rec.Body)
	}
	var edge struct {
		ProductID      string   `json:"product_id"`
		UpgradeToID    string   `json:"upgrade_to_id"`
		ProductName    string   `json:"product_name"`
		UpgradeToName  string   `json:"upgrade_to_name"`
		Compatible     bool     `json:"compatible"`
		CompatibleWith []string `json:"compatible_prices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &edge); err != nil {
		t.Fatalf("add edge body: %v\n%s", err, rec.Body)
	}
	if edge.ProductID != small || edge.UpgradeToID != large {
		t.Fatalf("edge = %+v", edge)
	}
	if !edge.Compatible || len(edge.CompatibleWith) == 0 {
		t.Fatalf("edge is not compatible although both products are priced monthly in USD: %+v", edge)
	}

	graph := env.get(t, "/api/v1/staff/upgrade-graph", cookie)
	if graph.Code != http.StatusOK {
		t.Fatalf("graph: status %d; body %s", graph.Code, graph.Body)
	}
	var listed struct {
		Edges []struct {
			ProductID     string `json:"product_id"`
			UpgradeToID   string `json:"upgrade_to_id"`
			ProductName   string `json:"product_name"`
			UpgradeToName string `json:"upgrade_to_name"`
			FromPrices    []struct {
				AmountMinor int64  `json:"amount_minor"`
				Currency    string `json:"currency"`
			} `json:"from_prices"`
			ToPrices []struct {
				AmountMinor int64  `json:"amount_minor"`
				Currency    string `json:"currency"`
			} `json:"to_prices"`
		} `json:"edges"`
		Products []struct {
			ID string `json:"id"`
		} `json:"products"`
	}
	if err := json.Unmarshal(graph.Body.Bytes(), &listed); err != nil {
		t.Fatalf("graph body: %v\n%s", err, graph.Body)
	}
	var found bool
	for _, e := range listed.Edges {
		if e.ProductID != small || e.UpgradeToID != large {
			continue
		}
		found = true
		if e.ProductName != "Graph Small" || e.UpgradeToName != "Graph Large" {
			t.Fatalf("edge names = %q -> %q", e.ProductName, e.UpgradeToName)
		}
		// The two prices are what the screen turns into "moving costs the prorated
		// difference"; without them it cannot say anything true about money.
		if len(e.FromPrices) != 1 || e.FromPrices[0].AmountMinor != 1200 ||
			len(e.ToPrices) != 1 || e.ToPrices[0].AmountMinor != 2500 {
			t.Fatalf("edge prices = %+v -> %+v", e.FromPrices, e.ToPrices)
		}
	}
	if !found {
		t.Fatalf("the edge is not in the graph: %s", graph.Body)
	}
	if len(listed.Products) < 2 {
		t.Fatalf("the graph did not list the products: %s", graph.Body)
	}

	// A self-edge is refused by the database; the answer has to be a sentence, not a 500.
	self := env.postServiceJSON(t, "/api/v1/staff/upgrade-graph", cookie, csrf,
		map[string]any{"product_id": small, "upgrade_to_id": small})
	if self.Code != http.StatusBadRequest {
		t.Fatalf("self-edge: status %d, want 400; body %s", self.Code, self.Body)
	}
	if code := problemCode(t, self); code != "self_edge" {
		t.Fatalf("self-edge code = %q, want self_edge", code)
	}

	// The same edge twice is a conflict, not a second row.
	dup := env.postServiceJSON(t, "/api/v1/staff/upgrade-graph", cookie, csrf,
		map[string]any{"product_id": small, "upgrade_to_id": large})
	if dup.Code != http.StatusConflict {
		t.Fatalf("duplicate edge: status %d, want 409; body %s", dup.Code, dup.Body)
	}
	if code := problemCode(t, dup); code != "edge_exists" {
		t.Fatalf("duplicate edge code = %q, want edge_exists", code)
	}

	// And it can be removed again.
	del := env.admDelete(t, "/api/v1/staff/upgrade-graph/"+small+"/"+large, cookie, csrf)
	if del.Code != http.StatusNoContent {
		t.Fatalf("remove edge: status %d; body %s", del.Code, del.Body)
	}
	after := env.get(t, "/api/v1/staff/upgrade-graph", cookie)
	var afterGraph struct {
		Edges []struct {
			ProductID   string `json:"product_id"`
			UpgradeToID string `json:"upgrade_to_id"`
		} `json:"edges"`
	}
	if err := json.Unmarshal(after.Body.Bytes(), &afterGraph); err != nil {
		t.Fatalf("graph body after removal: %v\n%s", err, after.Body)
	}
	for _, e := range afterGraph.Edges {
		if e.ProductID == small && e.UpgradeToID == large {
			t.Fatalf("the removed edge is still in the graph: %s", after.Body)
		}
	}

	var audited int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_events
		WHERE action IN ('catalog.upgrade_edge_added', 'catalog.upgrade_edge_removed')`).Scan(&audited); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if audited < 2 {
		t.Fatalf("audit rows for the graph = %d, want the add and the remove", audited)
	}
}

// ------------------------------------------------------------- providers --

// A provider's client secret is never in a response body.
//
// The assertion is on the bytes. A test that unmarshalled into a struct without a secret field
// would pass while the handler still wrote the value, which is the mistake worth catching.
func TestProviderSecretIsNeverInAResponseBody(t *testing.T) {
	env := newAuthEnv(t)
	cookie, csrf := env.admSession(t, "providers-admin@example.test")
	const secret = "s3cr3t-provider-value-do-not-echo"

	create := env.postServiceJSON(t, "/api/v1/staff/oauth-providers", cookie, csrf,
		map[string]any{
			"slug":                   "example-idp",
			"name":                   "Example IdP",
			"issuer_url":             "https://idp.example.test",
			"authorization_endpoint": "https://idp.example.test/authorize",
			"token_endpoint":         "https://idp.example.test/token",
			"userinfo_endpoint":      "https://idp.example.test/userinfo",
			"client_id":              "billing-panel",
			"client_secret":          secret,
			"scopes":                 []string{"openid", "email"},
			"subject_claim":          "sub",
			"email_claim":            "email",
			"enabled":                true,
		})
	if create.Code != http.StatusCreated {
		t.Fatalf("create provider: status %d; body %s", create.Code, create.Body)
	}
	if bytes.Contains(create.Body.Bytes(), []byte(secret)) {
		t.Fatalf("the create response contains the client secret: %s", create.Body)
	}
	var created struct {
		ID           string `json:"id"`
		HasSecret    bool   `json:"has_secret"`
		Enabled      bool   `json:"enabled"`
		SubjectClaim string `json:"subject_claim"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatalf("create body: %v\n%s", err, create.Body)
	}
	if !created.HasSecret {
		t.Fatal("has_secret = false after a secret was supplied")
	}
	if created.SubjectClaim != "sub" {
		t.Fatalf("subject_claim = %q, want sub", created.SubjectClaim)
	}

	list := env.get(t, "/api/v1/staff/oauth-providers", cookie)
	if list.Code != http.StatusOK {
		t.Fatalf("list providers: status %d; body %s", list.Code, list.Body)
	}
	if bytes.Contains(list.Body.Bytes(), []byte(secret)) {
		t.Fatalf("the list response contains the client secret: %s", list.Body)
	}
	if !bytes.Contains(list.Body.Bytes(), []byte(`"has_secret":true`)) {
		t.Fatalf("the list does not report has_secret: %s", list.Body)
	}

	one := env.get(t, "/api/v1/staff/oauth-providers/"+created.ID, cookie)
	if one.Code != http.StatusOK {
		t.Fatalf("read provider: status %d; body %s", one.Code, one.Body)
	}
	if bytes.Contains(one.Body.Bytes(), []byte(secret)) {
		t.Fatalf("the read response contains the client secret: %s", one.Body)
	}

	// An empty secret means "keep the stored one": saving a form that cannot show the value
	// must not wipe the credential.
	update := env.putServiceJSON(t, "/api/v1/staff/oauth-providers/"+created.ID, cookie, csrf,
		map[string]any{
			"slug":                   "example-idp",
			"name":                   "Example IdP renamed",
			"issuer_url":             "https://idp.example.test",
			"authorization_endpoint": "https://idp.example.test/authorize",
			"token_endpoint":         "https://idp.example.test/token",
			"userinfo_endpoint":      "https://idp.example.test/userinfo",
			"client_id":              "billing-panel",
			"client_secret":          "",
			"scopes":                 []string{"openid", "email", "profile"},
			"subject_claim":          "sub",
			"email_claim":            "email",
			"enabled":                false,
		})
	if update.Code != http.StatusOK {
		t.Fatalf("update provider: status %d; body %s", update.Code, update.Body)
	}
	if bytes.Contains(update.Body.Bytes(), []byte(secret)) {
		t.Fatalf("the update response contains the client secret: %s", update.Body)
	}
	var updated struct {
		HasSecret bool `json:"has_secret"`
		Enabled   bool `json:"enabled"`
	}
	if err := json.Unmarshal(update.Body.Bytes(), &updated); err != nil {
		t.Fatalf("update body: %v\n%s", err, update.Body)
	}
	if !updated.HasSecret {
		t.Fatal("an empty client_secret cleared the stored one")
	}
	if updated.Enabled {
		t.Fatal("enabled = true after it was set to false")
	}
}

// -------------------------------------------------------------- security --

// Revoking another account's session works, is audited, and does not touch the operator's own.
func TestSecurityRevokesAnotherSessionAndRefusesItsOwn(t *testing.T) {
	env := newAuthEnv(t)
	cookie, csrf := env.admSession(t, "security-admin@example.test")

	env.createUser(t, "attacked@example.test", "a long enough password", "")
	targetCookie, _ := env.login(t, "attacked@example.test", "a long enough password")

	// The target's session is visible in the cross-account list.
	list := env.get(t, "/api/v1/staff/security/sessions?q=attacked@example.test", cookie)
	if list.Code != http.StatusOK {
		t.Fatalf("list sessions: status %d; body %s", list.Code, list.Body)
	}
	var sessions struct {
		Data []struct {
			ID      string `json:"id"`
			Current bool   `json:"current"`
			User    struct {
				ID    string `json:"id"`
				Email string `json:"email"`
			} `json:"user"`
		} `json:"data"`
		Summary struct {
			Sessions int `json:"sessions"`
			Users    int `json:"users"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("sessions body: %v\n%s", err, list.Body)
	}
	if len(sessions.Data) == 0 {
		t.Fatalf("the target's session is not listed: %s", list.Body)
	}
	targetSession := sessions.Data[0]
	if targetSession.User.Email != "attacked@example.test" || targetSession.Current {
		t.Fatalf("session = %+v", targetSession)
	}
	if sessions.Summary.Sessions < 2 || sessions.Summary.Users < 2 {
		t.Fatalf("summary = %+v, want at least two sessions across two users", sessions.Summary)
	}

	rec := env.admDelete(t, "/api/v1/staff/security/sessions/"+targetSession.ID, cookie, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke another session: status %d; body %s", rec.Code, rec.Body)
	}
	// The session is dead server-side, not merely un-cookied.
	if got := env.get(t, "/api/v1/auth/me", targetCookie); got.Code != http.StatusUnauthorized {
		t.Fatalf("the revoked session still authenticates: %d", got.Code)
	}
	// The operator's own session is untouched.
	if got := env.get(t, "/api/v1/auth/me", cookie); got.Code != http.StatusOK {
		t.Fatalf("the operator was signed out by revoking somebody else's session: %d", got.Code)
	}

	// The revocation is audited against the account it ended.
	var audited int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_events
		WHERE action = 'session.revoke' AND target_id = $1`, targetSession.User.ID).Scan(&audited); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if audited == 0 {
		t.Fatal("revoking another account's session wrote no audit row")
	}
	// And the account's own sign-in trail says an operator did it, not a compromise.
	var trail int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM auth_events
		WHERE user_id = $1 AND event = 'session_revoked' AND detail LIKE 'by operator%'`,
		targetSession.User.ID).Scan(&trail); err != nil {
		t.Fatalf("count auth events: %v", err)
	}
	if trail == 0 {
		t.Fatal("the revoked account has no auth event recording the operator")
	}

	// The operator's own current session cannot be revoked through this screen: that is a
	// sign-out, and doing it by mis-click throws them out mid-incident.
	own := env.get(t, "/api/v1/staff/security/sessions?q=security-admin@example.test", cookie)
	if own.Code != http.StatusOK {
		t.Fatalf("list own sessions: status %d; body %s", own.Code, own.Body)
	}
	var ownBody struct {
		Data []struct {
			ID      string `json:"id"`
			Current bool   `json:"current"`
		} `json:"data"`
	}
	if err := json.Unmarshal(own.Body.Bytes(), &ownBody); err != nil {
		t.Fatalf("own sessions body: %v\n%s", err, own.Body)
	}
	var currentID string
	for _, row := range ownBody.Data {
		if row.Current {
			currentID = row.ID
		}
	}
	if currentID == "" {
		t.Fatalf("no session is marked current: %s", own.Body)
	}
	self := env.admDelete(t, "/api/v1/staff/security/sessions/"+currentID, cookie, csrf)
	if self.Code != http.StatusConflict {
		t.Fatalf("revoking the caller's own session: status %d, want 409; body %s", self.Code, self.Body)
	}
	if code := problemCode(t, self); code != "own_session" {
		t.Fatalf("own-session code = %q, want own_session", code)
	}
	if got := env.get(t, "/api/v1/auth/me", cookie); got.Code != http.StatusOK {
		t.Fatalf("the refusal still signed the operator out: %d", got.Code)
	}
}

// The sign-in history shows both a success and a failure, so "is this account being attacked"
// is answerable from the screen.
func TestSignInHistoryShowsSuccessAndFailure(t *testing.T) {
	env := newAuthEnv(t)
	cookie, _ := env.admSession(t, "history-admin@example.test")
	env.createUser(t, "watched@example.test", "a long enough password", "")

	// One success.
	env.login(t, "watched@example.test", "a long enough password")
	// One failure, with the wrong password: the account is named in the event, which is what
	// makes "which account is being tried" answerable.
	body, _ := json.Marshal(map[string]string{
		"email": "watched@example.test", "password": "definitely not the password",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("failed login: status %d, want 401; body %s", rec.Code, rec.Body)
	}

	history := env.get(t, "/api/v1/staff/security/sign-ins?q=watched@example.test", cookie)
	if history.Code != http.StatusOK {
		t.Fatalf("sign-in history: status %d; body %s", history.Code, history.Body)
	}
	var page struct {
		Data []struct {
			Event   string `json:"event"`
			Outcome string `json:"outcome"`
			IP      string `json:"ip"`
			User    *struct {
				Email string `json:"email"`
			} `json:"user"`
		} `json:"data"`
		Summary struct {
			Successes          int `json:"successes"`
			Failures           int `json:"failures"`
			TopFailingAccounts []struct {
				Label string `json:"label"`
				Count int    `json:"count"`
			} `json:"top_failing_accounts"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(history.Body.Bytes(), &page); err != nil {
		t.Fatalf("history body: %v\n%s", err, history.Body)
	}

	var sawSuccess, sawFailure bool
	for _, row := range page.Data {
		if row.User == nil || row.User.Email != "watched@example.test" {
			continue
		}
		switch row.Outcome {
		case "success":
			sawSuccess = sawSuccess || row.Event == "login_succeeded"
		case "failure":
			sawFailure = true
		}
	}
	if !sawSuccess || !sawFailure {
		t.Fatalf("history shows success=%v failure=%v; body %s", sawSuccess, sawFailure, history.Body)
	}
	if page.Summary.Successes < 1 || page.Summary.Failures < 1 {
		t.Fatalf("summary = %+v, want at least one of each in the window", page.Summary)
	}
	var named bool
	for _, row := range page.Summary.TopFailingAccounts {
		if row.Label == "watched@example.test" && row.Count >= 1 {
			named = true
		}
	}
	if !named {
		t.Fatalf("the failing account is not in top_failing_accounts: %+v", page.Summary.TopFailingAccounts)
	}

	// The outcome filter narrows the list, which is how an operator reads "only the failures".
	failures := env.get(t, "/api/v1/staff/security/sign-ins?outcome=failure&q=watched@example.test", cookie)
	if failures.Code != http.StatusOK {
		t.Fatalf("failure filter: status %d; body %s", failures.Code, failures.Body)
	}
	var failuresBody struct {
		Data []struct {
			Outcome string `json:"outcome"`
		} `json:"data"`
	}
	if err := json.Unmarshal(failures.Body.Bytes(), &failuresBody); err != nil {
		t.Fatalf("failure body: %v\n%s", err, failures.Body)
	}
	if len(failuresBody.Data) == 0 {
		t.Fatalf("the failure filter returned nothing: %s", failures.Body)
	}
	for _, row := range failuresBody.Data {
		if row.Outcome != "failure" {
			t.Fatalf("the failure filter returned a %s row", row.Outcome)
		}
	}

	if bad := env.get(t, "/api/v1/staff/security/sign-ins?outcome=maybe", cookie); bad.Code != http.StatusBadRequest {
		t.Fatalf("an invalid outcome: status %d, want 400", bad.Code)
	}
}

// The security screen is staff:manage, not merely "any staff": it is other people's sessions.
func TestSecurityScreenRequiresStaffManage(t *testing.T) {
	env := newAuthEnv(t)
	env.createUser(t, "support-security@example.test", "a long enough password", "support")
	cookie, _ := env.login(t, "support-security@example.test", "a long enough password")
	for _, path := range []string{
		"/api/v1/staff/security/sessions",
		"/api/v1/staff/security/sign-ins",
	} {
		if rec := env.get(t, path, cookie); rec.Code != http.StatusForbidden {
			t.Fatalf("%s as support: status %d, want 403", path, rec.Code)
		}
	}
}

// A guard against the response shape drifting: the problem documents this file produces carry
// a stable code and the problem content type. It is asserted once here because every screen
// branches on the code rather than on the sentence.
func TestAdminScreensProblemShape(t *testing.T) {
	env := newAuthEnv(t)
	cookie, _ := env.admSession(t, "problem-admin@example.test")
	rec := env.get(t, "/api/v1/staff/holds?status=whenever", cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid hold status: status %d, want 400", rec.Code)
	}
	if code := problemCode(t, rec); code != "invalid_status" {
		t.Fatalf("code = %q, want invalid_status", code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q", ct)
	}
}
