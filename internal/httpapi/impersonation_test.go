package httpapi_test

// Impersonation, end to end over HTTP: start it, act inside it, stop it.
//
// The feature is only worth having if three things are true at once, and each one is asserted
// here rather than assumed:
//
//  1. Starting one is recorded against *both* accounts. A row naming only the customer makes an
//     operator's action indistinguishable from the customer's own; a row naming only the operator
//     cannot answer "why did this happen on my account". The audit row therefore carries the
//     target as the actor and the operator in `impersonated_by`, and the customer's own auth log
//     carries a second row. Both are checked.
//  2. An impersonated session cannot move money or change credentials. This is a server-side
//     refusal (internal/httpapi/auth.go, `impersonationRefused`), not a hidden button: the tests
//     below call the endpoints directly with the impersonated cookie and require 403
//     `impersonating`. Hiding a control and hoping is not a restriction.
//  3. Stopping returns the operator to their own identity, ends the borrowed session, and is
//     recorded too. A "stop" that logs the operator out, or one that leaves the borrowed token
//     alive, would make the feature unsafe to use.
//
// What must keep working while impersonating is asserted as well — reading the customer's data,
// reading the identity, and stopping — because a refusal table that also refuses those would pass
// every negative test above and still make the feature useless.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/httpapi"
)

// impersonationSession is a started impersonation plus the operator it belongs to.
type impersonationSession struct {
	OperatorID string
	TargetID   string
	Cookie     *http.Cookie
	CSRF       string
}

// startImpersonation logs an administrator in and impersonates targetID, returning the borrowed
// session.
func startImpersonation(t *testing.T, env *authEnv, operatorEmail, operatorPassword, targetID string) impersonationSession {
	t.Helper()
	operatorCookie, operatorCSRF := env.login(t, operatorEmail, operatorPassword)

	start := env.doIdentity(t, http.MethodPost,
		"/api/v1/staff/users/"+targetID+"/impersonate", operatorCookie, operatorCSRF, nil)
	if start.Code != http.StatusCreated {
		t.Fatalf("impersonate: status %d, want 201; body %s", start.Code, start.Body)
	}

	var body struct {
		Impersonating  bool `json:"impersonating"`
		ImpersonatedBy struct {
			ID string `json:"id"`
		} `json:"impersonated_by"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &body); err != nil {
		t.Fatalf("impersonate body: %v", err)
	}
	if !body.Impersonating {
		t.Fatalf("the response does not say it is impersonating: %s", start.Body)
	}
	if body.User.ID != targetID {
		t.Fatalf("the session is for %q, want the target %q", body.User.ID, targetID)
	}

	var cookie *http.Cookie
	for _, c := range start.Result().Cookies() {
		if c.Name == "billing_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no impersonated session cookie was set")
	}
	return impersonationSession{
		OperatorID: body.ImpersonatedBy.ID,
		TargetID:   targetID,
		Cookie:     cookie,
		CSRF:       body.CSRFToken,
	}
}

// ---------------------------------------------------------------- the audit --

// Starting an impersonation is recorded against the customer and the operator, by name.
func TestImpersonationStartIsAuditedAgainstBothAccounts(t *testing.T) {
	env := newAuthEnv(t)
	targetID := env.createUser(t, "audited-target@example.test", "customer password 1", "")
	operatorID := env.createUser(t, "audited-operator@example.test", "operator password 1", "administrator")

	imp := startImpersonation(t, env, "audited-operator@example.test", "operator password 1", targetID)
	if imp.OperatorID != operatorID {
		t.Fatalf("the response names operator %q, want %q", imp.OperatorID, operatorID)
	}

	ctx := context.Background()

	// The audit row: the customer is the actor (whose account was acted on), the operator is in
	// `impersonated_by` (who did it), and the pair is also in `changes` so the evidence survives
	// even a reader that does not know about the column.
	var (
		actorID        string
		impersonatedBy string
		changesBy      string
	)
	if err := env.pool.QueryRow(ctx, `
		SELECT COALESCE(actor_id::text, ''), COALESCE(impersonated_by::text, ''),
		       COALESCE(changes->>'impersonated_by', '')
		FROM audit_events
		WHERE action = 'user.impersonation_start'
		ORDER BY id DESC LIMIT 1`).Scan(&actorID, &impersonatedBy, &changesBy); err != nil {
		t.Fatalf("read the impersonation audit row: %v", err)
	}
	if actorID != targetID {
		t.Fatalf("the audit row's actor is %q, want the customer %q", actorID, targetID)
	}
	if impersonatedBy != operatorID {
		t.Fatalf("audit impersonated_by = %q, want the operator %q", impersonatedBy, operatorID)
	}
	if changesBy != operatorID {
		t.Fatalf("audit changes.impersonated_by = %q, want the operator %q", changesBy, operatorID)
	}

	// The customer's own authentication log answers "why did this happen on my account" without
	// anybody having to read the audit table.
	var detail string
	if err := env.pool.QueryRow(ctx, `
		SELECT detail FROM auth_events
		WHERE user_id = $1 AND event = 'impersonation_started'
		ORDER BY id DESC LIMIT 1`, targetID).Scan(&detail); err != nil {
		t.Fatalf("read the customer's auth event: %v", err)
	}
	if !strings.Contains(detail, operatorID) {
		t.Fatalf("the customer's auth event says %q; it must name the operator %q", detail, operatorID)
	}

	// And the operator's log says whom they wore, so a review filtered by operator finds it.
	if err := env.pool.QueryRow(ctx, `
		SELECT detail FROM auth_events
		WHERE user_id = $1 AND event = 'impersonation_started'
		ORDER BY id DESC LIMIT 1`, operatorID).Scan(&detail); err != nil {
		t.Fatalf("read the operator's auth event: %v", err)
	}
	if !strings.Contains(detail, targetID) {
		t.Fatalf("the operator's auth event says %q; it must name the target %q", detail, targetID)
	}
}

// ------------------------------------------------------------------ the stop --

// Stopping ends the borrowed session, hands the operator their own back, and is audited.
func TestImpersonationStopReturnsToTheOperatorAndIsAudited(t *testing.T) {
	env := newAuthEnv(t)
	targetID := env.createUser(t, "stop-target@example.test", "customer password 1", "")
	operatorID := env.createUser(t, "stop-operator@example.test", "operator password 1", "administrator")

	imp := startImpersonation(t, env, "stop-operator@example.test", "operator password 1", targetID)

	stop := env.doIdentity(t, http.MethodPost, "/api/v1/portal/impersonation/stop",
		imp.Cookie, imp.CSRF, nil)
	if stop.Code != http.StatusOK {
		t.Fatalf("stop: status %d, want 200; body %s", stop.Code, stop.Body)
	}
	var stopped struct {
		Impersonating bool `json:"impersonating"`
		User          struct {
			ID      string `json:"id"`
			IsStaff bool   `json:"is_staff"`
			Roles   []string
		} `json:"user"`
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(stop.Body.Bytes(), &stopped); err != nil {
		t.Fatalf("stop body: %v", err)
	}
	if stopped.Impersonating {
		t.Fatal("the stop response still reports impersonating")
	}
	if stopped.User.ID != operatorID || !stopped.User.IsStaff {
		t.Fatalf("stop returned user %q (staff %v), want the operator %q",
			stopped.User.ID, stopped.User.IsStaff, operatorID)
	}

	// The borrowed token is dead: it must not outlive the impersonation in a browser that may
	// have been shared.
	if got := env.get(t, "/api/v1/auth/me", imp.Cookie); got.Code != http.StatusUnauthorized {
		t.Fatalf("the impersonated session survived the stop: %d %s", got.Code, got.Body)
	}

	// The operator's own session is live again, from the cookie the stop set.
	var operatorCookie *http.Cookie
	for _, c := range stop.Result().Cookies() {
		if c.Name == "billing_session" {
			operatorCookie = c
		}
	}
	if operatorCookie == nil {
		t.Fatal("stop did not set the operator's session cookie")
	}
	me := env.get(t, "/api/v1/auth/me", operatorCookie)
	if me.Code != http.StatusOK {
		t.Fatalf("the operator is not signed in after the stop: %d %s", me.Code, me.Body)
	}
	var identity struct {
		Impersonating bool `json:"impersonating"`
		User          struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(me.Body.Bytes(), &identity); err != nil {
		t.Fatalf("me body: %v", err)
	}
	if identity.Impersonating || identity.User.ID != operatorID {
		t.Fatalf("after the stop /auth/me reports impersonating=%v user=%q, want the operator %q",
			identity.Impersonating, identity.User.ID, operatorID)
	}

	// The stop is recorded, naming both accounts, exactly as the start is.
	var (
		actorID        string
		impersonatedBy string
	)
	if err := env.pool.QueryRow(context.Background(), `
		SELECT COALESCE(actor_id::text, ''), COALESCE(impersonated_by::text, '')
		FROM audit_events
		WHERE action = 'user.impersonation_stop'
		ORDER BY id DESC LIMIT 1`).Scan(&actorID, &impersonatedBy); err != nil {
		t.Fatalf("read the stop audit row: %v", err)
	}
	if actorID != targetID || impersonatedBy != operatorID {
		t.Fatalf("stop audit row: actor %q impersonated_by %q, want %q and %q",
			actorID, impersonatedBy, targetID, operatorID)
	}
}

// -------------------------------------------------------- the cannot-act rule --

// An impersonated session cannot move money or change credentials, and the refusal is the
// server's rather than the screen's.
//
// Every path below is called directly with the borrowed cookie. A test that clicked a button
// would prove the button is hidden; this proves the endpoint is closed.
func TestImpersonatedSessionCannotMoveMoneyOrChangeCredentials(t *testing.T) {
	env := newAuthEnv(t)
	targetID := env.createUser(t, "readonly-target@example.test", "customer password 1", "")
	env.createUser(t, "readonly-operator@example.test", "operator password 1", "administrator")

	imp := startImpersonation(t, env, "readonly-operator@example.test", "operator password 1", targetID)

	const someID = "00000000-0000-4000-8000-000000000001"
	cases := []struct {
		name string
		verb string
		path string
		body any
	}{
		// Money. None of these should ever reach a handler: the refusal is in the middleware.
		{"pay an invoice", http.MethodPost, "/api/v1/portal/invoices/" + someID + "/checkout", nil},
		{"pay an invoice with credit", http.MethodPost, "/api/v1/portal/invoices/" + someID + "/pay-with-credit", nil},
		{"deposit credit", http.MethodPost, "/api/v1/portal/credits/deposit", map[string]any{"amount_minor": 1000, "currency": "USD"}},
		{"place an order", http.MethodPost, "/api/v1/portal/orders", map[string]any{}},
		{"refund a payment", http.MethodPost, "/api/v1/staff/payments/" + someID + "/refunds", map[string]any{}},
		{"record a payment", http.MethodPost, "/api/v1/staff/invoices/" + someID + "/payments", map[string]any{}},
		{"grant credit", http.MethodPost, "/api/v1/staff/customers/" + targetID + "/credit", map[string]any{}},

		// Credentials.
		{"change the password", http.MethodPost, "/api/v1/auth/password/change", map[string]any{
			"current_password": "customer password 1", "new_password": "a brand new password 1",
		}},
		{"read the second factor", http.MethodGet, "/api/v1/auth/mfa", nil},
		{"enrol a second factor", http.MethodPost, "/api/v1/auth/mfa/enrol", map[string]any{}},
		{"confirm a second factor", http.MethodPost, "/api/v1/auth/mfa/confirm", map[string]any{"code": "000000"}},
		{"disable a second factor", http.MethodPost, "/api/v1/auth/mfa/disable", map[string]any{
			"password": "customer password 1", "code": "000000",
		}},
		{"set the password as staff", http.MethodPost, "/api/v1/staff/users/" + targetID + "/password", map[string]any{
			"password": "a brand new password 1",
		}},
		{"reset the second factor as staff", http.MethodPost, "/api/v1/staff/users/" + targetID + "/mfa/reset", map[string]any{}},
		{"mint an API key", http.MethodPost, "/api/v1/staff/api-keys", map[string]any{"name": "backdoor", "scopes": []string{}}},
		{"grant a role", http.MethodPost, "/api/v1/staff/roles", map[string]any{"name": "backdoor-role", "permissions": []string{}}},

		// The record itself.
		{"close the account", http.MethodPost, "/api/v1/portal/account-closure", map[string]any{"reason": "support said so"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.doIdentity(t, tc.verb, tc.path, imp.Cookie, imp.CSRF, tc.body)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s %s while impersonating: status %d, want 403; body %s",
					tc.verb, tc.path, rec.Code, rec.Body)
			}
			if code := decodeProblemCode(t, rec); code != "impersonating" {
				t.Fatalf("code = %q, want impersonating", code)
			}
			// The sentence names the family, so the caller knows what they tried to do and what
			// to do instead rather than reading a bare "forbidden".
			if !strings.Contains(rec.Body.String(), "stop impersonating") {
				t.Fatalf("the refusal does not say how to proceed: %s", rec.Body)
			}
		})
	}

	// Nothing was written. The refusal must happen before the handler, so a payment attempt
	// leaves no row behind to reconcile later.
	var payments int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM payments`).Scan(&payments); err != nil {
		t.Fatalf("count payments: %v", err)
	}
	if payments != 0 {
		t.Fatalf("%d payment rows exist after refused attempts", payments)
	}
}

// The refusal is about the action, not about the spelling of the URL: a variant path is judged
// by the same rule rather than sliding past it.
func TestImpersonationRefusalSurvivesAPathVariant(t *testing.T) {
	env := newAuthEnv(t)
	targetID := env.createUser(t, "variant-target@example.test", "customer password 1", "")
	env.createUser(t, "variant-operator@example.test", "operator password 1", "administrator")
	imp := startImpersonation(t, env, "variant-operator@example.test", "operator password 1", targetID)

	for _, path := range []string{
		"/api/v1/portal/credits/deposit/",
		"/api/v1/portal/credits/./deposit",
		"//api/v1/portal/credits/deposit",
	} {
		rec := env.doIdentity(t, http.MethodPost, path, imp.Cookie, imp.CSRF, map[string]any{})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("POST %s while impersonating: status %d, want 403; body %s", path, rec.Code, rec.Body)
		}
		if code := decodeProblemCode(t, rec); code != "impersonating" {
			t.Fatalf("POST %s: code = %q, want impersonating", path, code)
		}
	}
}

// Impersonation that cannot read, or cannot end, is a trap. What must stay reachable is asserted
// here so a future entry in the refusal table cannot quietly close it.
func TestImpersonatedSessionCanStillLookAndStillStop(t *testing.T) {
	env := newAuthEnv(t)
	targetID := env.createUser(t, "look-target@example.test", "customer password 1", "")
	env.createUser(t, "look-operator@example.test", "operator password 1", "administrator")
	imp := startImpersonation(t, env, "look-operator@example.test", "operator password 1", targetID)

	// The identity: this is what renders the banner, on every screen and after every reload.
	me := env.get(t, "/api/v1/auth/me", imp.Cookie)
	if me.Code != http.StatusOK {
		t.Fatalf("me while impersonating: %d %s", me.Code, me.Body)
	}
	var identity struct {
		Impersonating  bool   `json:"impersonating"`
		ImpersonatedBy string `json:"impersonated_by"`
		User           struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(me.Body.Bytes(), &identity); err != nil {
		t.Fatalf("me body: %v", err)
	}
	if !identity.Impersonating || identity.ImpersonatedBy == "" {
		t.Fatalf("the impersonated session does not identify the operator: %s", me.Body)
	}
	if identity.User.ID != targetID {
		t.Fatalf("the session identifies as %q, want the target %q", identity.User.ID, targetID)
	}

	// The customer's own data: looking is the whole point of the feature.
	for _, path := range []string{"/api/v1/portal/sessions", "/api/v1/portal/social-links"} {
		if rec := env.get(t, path, imp.Cookie); rec.Code == http.StatusForbidden {
			t.Fatalf("GET %s while impersonating: 403; an impersonated session must be able to look: %s",
				path, rec.Body)
		}
	}

	// And the way out.
	stop := env.doIdentity(t, http.MethodPost, "/api/v1/portal/impersonation/stop", imp.Cookie, imp.CSRF, nil)
	if stop.Code != http.StatusOK {
		t.Fatalf("stop while impersonating: %d %s", stop.Code, stop.Body)
	}
}

// The refusal table names routes that exist.
//
// A refusal list is a security control only while it points at real endpoints. If a route is
// renamed and the entry is not, the protection silently disappears while the table still reads
// as if it were there — the same failure mode as a guard that has stopped looking.
func TestImpersonationRefusedRoutesExist(t *testing.T) {
	env := newAuthEnv(t)
	// Handler() is what builds the route table (and records it for Routes()), so it has to be
	// built once before the table can be read.
	_ = env.srv.Handler()

	declared := make(map[string]bool, 256)
	for _, route := range env.srv.Routes() {
		declared[route.Path()] = true
	}
	if len(declared) == 0 {
		t.Fatal("the route table is empty; the server did not build its routes")
	}

	refused := httpapi.ImpersonationRefusedRoutes()
	if len(refused) == 0 {
		t.Fatal("the impersonation refusal table is empty")
	}
	for _, entry := range refused {
		if !declared[entry] {
			t.Errorf("the impersonation refusal names %q, which is not a route in this build: "+
				"the refusal protects nothing while still reading as if it did", entry)
		}
	}

	// The three things an impersonated session must always be able to do. A refusal entry that
	// closed one of these would turn the feature into a trap, so the absence is asserted rather
	// than assumed.
	for _, entry := range []string{
		"POST /api/v1/portal/impersonation/stop",
		"GET /api/v1/auth/me",
		"GET /api/v1/staff/customers/{id}",
		"GET /api/v1/portal/sessions",
	} {
		for _, refusedEntry := range refused {
			if refusedEntry == entry {
				t.Fatalf("%q is refused while impersonating; that is the way out, not an action", entry)
			}
		}
	}

	// A refusal is declared for every family, so a table edited down to one kind is noticed.
	var money, credentials int
	for _, entry := range refused {
		switch {
		case strings.HasPrefix(entry, "POST /api/v1/portal/invoices/") ||
			strings.HasPrefix(entry, "POST /api/v1/staff/payments"):
			money++
		case strings.Contains(entry, "/mfa/") || strings.Contains(entry, "/password") ||
			strings.Contains(entry, "api-keys"):
			credentials++
		}
	}
	if money == 0 {
		t.Error("no money route is refused while impersonating")
	}
	if credentials == 0 {
		t.Error("no credential route is refused while impersonating")
	}
}

// The rule belongs to the session, not to the person: the operator's own session can still do
// what the borrowed one cannot.
func TestTheOperatorKeepsTheirOwnPowers(t *testing.T) {
	env := newAuthEnv(t)
	targetID := env.createUser(t, "powers-target@example.test", "customer password 1", "")
	env.createUser(t, "powers-operator@example.test", "operator password 1", "administrator")

	operatorCookie, operatorCSRF := env.login(t, "powers-operator@example.test", "operator password 1")
	if operatorCookie == nil || operatorCSRF == "" {
		t.Fatal("the operator could not sign in")
	}

	// Start and stop, so the borrowed session has existed and ended.
	imp := startImpersonation(t, env, "powers-operator@example.test", "operator password 1", targetID)
	if rec := env.doIdentity(t, http.MethodPost, "/api/v1/portal/impersonation/stop",
		imp.Cookie, imp.CSRF, nil); rec.Code != http.StatusOK {
		t.Fatalf("stop: %d %s", rec.Code, rec.Body)
	}

	// The original operator cookie was rotated by the stop, so the operator signs in again to
	// prove the refusal never attached to the person. What must not happen is a 403 whose code
	// is `impersonating` on their own session.
	cookie, csrf := env.login(t, "powers-operator@example.test", "operator password 1")
	rec := env.doIdentity(t, http.MethodPost, "/api/v1/staff/users/"+targetID+"/password",
		cookie, csrf, map[string]any{"password": "an operator chosen password 1"})
	if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "impersonating") {
		t.Fatalf("the operator's own session was refused as impersonating: %s", rec.Body)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("the operator cannot set a password after impersonating: %d %s", rec.Code, rec.Body)
	}

	// And the credential the operator set is the one that works, which proves the write landed
	// rather than being swallowed by the refusal path. login() fails the test itself if the
	// credential is refused.
	env.login(t, "powers-target@example.test", "an operator chosen password 1")
}
