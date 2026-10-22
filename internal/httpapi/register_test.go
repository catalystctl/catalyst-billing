package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/register"
)

// Registration over HTTP. The properties that matter here are the ones the service
// cannot prove on its own: that the route is public, that the same-origin and credential
// budgets are applied, and that the response shape does not leak whether an address is
// registered.

// lowerRegisterCost makes the registration hasher cheap for the suite.
//
// The route hashes with the production parameters (64 MiB, three passes), which is the
// point: a registration must cost what a login costs, or it is a cheaper way to attack
// the server. A test does not need that cost, and paying it per test turns a
// two-second suite into a minute.
func lowerRegisterCost(t *testing.T) {
	t.Helper()
	previous := register.Argon2Params
	p := authn.DefaultArgon2Params()
	p.Memory = 19 * 1024
	p.Iterations = 1
	register.Argon2Params = func() authn.Argon2Params { return p }
	t.Cleanup(func() { register.Argon2Params = previous })
}

// register posts a registration with no session and no CSRF token, which is what a
// visitor's browser does.
func (e *authEnv) register(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestRegisterCreatesAnAccount(t *testing.T) {
	lowerRegisterCost(t)
	env := newAuthEnv(t)

	rec := env.register(t, map[string]any{
		"email":        "newcomer@example.test",
		"password":     "correct horse battery",
		"display_name": "New Comer",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}

	var body struct {
		Status               string `json:"status"`
		VerificationRequired bool   `json:"verification_required"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if body.Status != "accepted" {
		t.Fatalf("status = %q", body.Status)
	}
	// No session is issued: the client must sign in with the credentials it chose.
	if len(rec.Result().Cookies()) != 0 {
		t.Fatalf("registration set %d cookie(s); it must not sign the visitor in",
			len(rec.Result().Cookies()))
	}

	// The account is real and can sign in.
	env.login(t, "newcomer@example.test", "correct horse battery")

	// The three rows exist and are linked.
	var linked int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM customer_accounts ca
		JOIN users u ON u.id = ca.user_id
		JOIN customers c ON c.id = ca.customer_id
		WHERE lower(u.email) = 'newcomer@example.test'
		  AND lower(c.email) = 'newcomer@example.test'`).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked != 1 {
		t.Fatalf("customer_accounts rows = %d, want 1", linked)
	}

	// The account is immediately usable: it can sign in and identify itself.
	cookie, _ := env.login(t, "newcomer@example.test", "correct horse battery")
	if rec := env.get(t, "/api/v1/auth/me", cookie); rec.Code != http.StatusOK {
		t.Fatalf("identity after registration: %d %s", rec.Code, rec.Body)
	}
}

// A duplicate address must be answered identically to a fresh one, or the form is an
// account-discovery oracle.
func TestRegisterDuplicateLooksLikeSuccess(t *testing.T) {
	lowerRegisterCost(t)
	env := newAuthEnv(t)
	first := env.register(t, map[string]any{
		"email":        "taken@example.test",
		"password":     "correct horse battery",
		"display_name": "First",
	})
	if first.Code != http.StatusAccepted {
		t.Fatalf("first: %d %s", first.Code, first.Body)
	}

	// Same address, different case: the unique index is case-insensitive.
	second := env.register(t, map[string]any{
		"email":        "TAKEN@example.test",
		"password":     "a completely different one",
		"display_name": "Second",
	})
	if second.Code != first.Code {
		t.Fatalf("duplicate status = %d, fresh status = %d", second.Code, first.Code)
	}
	if second.Body.String() != first.Body.String() {
		t.Fatalf("duplicate body %q differs from fresh body %q", second.Body, first.Body)
	}

	// Only one account exists, and the original password still works.
	var users int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM users WHERE lower(email) = 'taken@example.test'`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if users != 1 {
		t.Fatalf("users = %d, want 1", users)
	}
	env.login(t, "taken@example.test", "correct horse battery")
}

// A weak password is refused, and the refusal names the reason.
func TestRegisterRefusesWeakPassword(t *testing.T) {
	lowerRegisterCost(t)
	env := newAuthEnv(t)
	rec := env.register(t, map[string]any{
		"email":        "weak@example.test",
		"password":     "short",
		"display_name": "Weak",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "weak_password") {
		t.Fatalf("body = %s", rec.Body)
	}
}

// Terms must be accepted when the installation configures them.
func TestRegisterRequiresTOSWhenConfigured(t *testing.T) {
	lowerRegisterCost(t)
	env := newAuthEnv(t)
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO settings (group_key, key, value) VALUES ('general', 'tos', 'Be kind.')
		ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`); err != nil {
		t.Fatal(err)
	}

	rec := env.register(t, map[string]any{
		"email":        "notos@example.test",
		"password":     "correct horse battery",
		"display_name": "No Terms",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "tos_required") {
		t.Fatalf("body = %s", rec.Body)
	}

	rec = env.register(t, map[string]any{
		"email":        "notos@example.test",
		"password":     "correct horse battery",
		"display_name": "No Terms",
		"accept_tos":   true,
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("with acceptance: %d %s", rec.Code, rec.Body)
	}
}

// A closed registration is refused with a code the UI can explain.
func TestRegisterRefusesWhenDisabled(t *testing.T) {
	lowerRegisterCost(t)
	env := newAuthEnv(t)
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO settings (group_key, key, value) VALUES ('general', 'registration_disabled', 'true')
		ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`); err != nil {
		t.Fatal(err)
	}

	rec := env.register(t, map[string]any{
		"email":        "closed@example.test",
		"password":     "correct horse battery",
		"display_name": "Closed",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "registration_disabled") {
		t.Fatalf("body = %s", rec.Body)
	}
}

// The route is public — a visitor has no session — but a cross-origin post is refused.
func TestRegisterIsPublicButSameOrigin(t *testing.T) {
	lowerRegisterCost(t)
	env := newAuthEnv(t)

	// No Origin header: a non-browser client, which cannot be used for CSRF.
	if rec := env.register(t, map[string]any{
		"email":        "cli@example.test",
		"password":     "correct horse battery",
		"display_name": "CLI",
	}); rec.Code != http.StatusAccepted {
		t.Fatalf("anonymous registration: %d %s", rec.Code, rec.Body)
	}

	// A cross-origin browser post is refused.
	body, _ := json.Marshal(map[string]any{
		"email":        "evil@example.test",
		"password":     "correct horse battery",
		"display_name": "Evil",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example.test")
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin registration: %d %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "bad_origin") {
		t.Fatalf("body = %s", rec.Body)
	}
}

// The credential budget bounds registration per source, so a bot cannot spray.
func TestRegisterIsRateLimited(t *testing.T) {
	lowerRegisterCost(t)
	env := newAuthEnv(t)
	// The budget is per minute and per address, and the route class is the credential
	// class; five attempts are allowed, the sixth is refused.
	var last *httptest.ResponseRecorder
	for i := 0; i < 6; i++ {
		last = env.register(t, map[string]any{
			"email":        "spray@example.test",
			"password":     "correct horse battery",
			"display_name": "Spray",
		})
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body %s", last.Code, last.Body)
	}
	if !containsCode(last.Body.Bytes(), "rate_limited") {
		t.Fatalf("body = %s", last.Body)
	}
}

// Changing a password over HTTP: the session survives, other sessions do not, and CSRF
// is required.
func TestPasswordChange(t *testing.T) {
	lowerRegisterCost(t)
	env := newAuthEnv(t)
	env.createUser(t, "rotate@example.test", "correct horse battery", "")
	cookie, csrf := env.login(t, "rotate@example.test", "correct horse battery")
	// A second session, which must be revoked.
	otherCookie, _ := env.login(t, "rotate@example.test", "correct horse battery")

	// Without a CSRF token the mutation is refused.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/password/change",
		bytes.NewReader([]byte(`{"current_password":"correct horse battery","new_password":"a whole new passphrase"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("without CSRF: %d %s", rec.Code, rec.Body)
	}

	rec = env.postJSON(t, "/api/v1/auth/password/change", cookie, csrf, map[string]any{
		"current_password": "correct horse battery",
		"new_password":     "a whole new passphrase",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		Status          string `json:"status"`
		SessionsRevoked int    `json:"sessions_revoked"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v\n%s", err, rec.Body)
	}
	if body.Status != "password_changed" {
		t.Fatalf("status = %q", body.Status)
	}
	if body.SessionsRevoked != 1 {
		t.Fatalf("sessions_revoked = %d, want 1", body.SessionsRevoked)
	}

	// The caller's session still works…
	if rec := env.get(t, "/api/v1/auth/me", cookie); rec.Code != http.StatusOK {
		t.Fatalf("the caller was signed out by their own password change: %d", rec.Code)
	}
	// …and the other one does not.
	if rec := env.get(t, "/api/v1/auth/me", otherCookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the other session survived: %d", rec.Code)
	}

	// The new password works.
	env.login(t, "rotate@example.test", "a whole new passphrase")
}

// A wrong current password is refused and changes nothing.
func TestPasswordChangeRefusesWrongCurrent(t *testing.T) {
	lowerRegisterCost(t)
	env := newAuthEnv(t)
	env.createUser(t, "wrong@example.test", "correct horse battery", "")
	cookie, csrf := env.login(t, "wrong@example.test", "correct horse battery")

	rec := env.postJSON(t, "/api/v1/auth/password/change", cookie, csrf, map[string]any{
		"current_password": "not it at all",
		"new_password":     "a whole new passphrase",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "invalid_credentials") {
		t.Fatalf("body = %s", rec.Body)
	}
	env.login(t, "wrong@example.test", "correct horse battery")
}

// The route is not reachable without a session.
//
// The refusal is a CSRF failure rather than a 401 because the CSRF wrapper is applied
// outside requireUser (a mutation with no session-bound token fails closed before the
// identity is consulted). Either way nothing happens, and this test pins the actual
// shape so a change to the wrapper order is visible.
func TestPasswordChangeRequiresASession(t *testing.T) {
	env := newAuthEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/password/change",
		bytes.NewReader([]byte(`{"current_password":"a","new_password":"b"}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body)
	}
	if !containsCode(rec.Body.Bytes(), "csrf_failed") {
		t.Fatalf("body = %s", rec.Body)
	}

	// With a session but no CSRF token the answer is the same, which is the property
	// that matters: a cross-site form post cannot change a password.
	env.createUser(t, "csrf@example.test", "correct horse battery", "")
	cookie, _ := env.login(t, "csrf@example.test", "correct horse battery")
	req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/password/change",
		bytes.NewReader([]byte(`{"current_password":"correct horse battery","new_password":"a whole new passphrase"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("signed in without CSRF: %d %s", rec.Code, rec.Body)
	}
}

// A short password change is a 422, not a 401: the request was well formed.
func TestPasswordChangeRefusesWeakNew(t *testing.T) {
	lowerRegisterCost(t)
	env := newAuthEnv(t)
	env.createUser(t, "weakchange@example.test", "correct horse battery", "")
	cookie, csrf := env.login(t, "weakchange@example.test", "correct horse battery")

	rec := env.postJSON(t, "/api/v1/auth/password/change", cookie, csrf, map[string]any{
		"current_password": "correct horse battery",
		"new_password":     "short",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
}

// Registration does not need the credential budget to be generous, but it must not
// accept a body it does not understand: an unknown field is a client bug.
func TestRegisterRejectsUnknownFields(t *testing.T) {
	lowerRegisterCost(t)
	env := newAuthEnv(t)
	rec := env.register(t, map[string]any{
		"email":        "strict@example.test",
		"password":     "correct horse battery",
		"display_name": "Strict",
		"is_staff":     true,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body)
	}
}

// A registration with a display name missing is refused, because the name appears on
// every document the account will ever be issued.
func TestRegisterRequiresDisplayName(t *testing.T) {
	lowerRegisterCost(t)
	env := newAuthEnv(t)
	rec := env.register(t, map[string]any{
		"email":    "nameless@example.test",
		"password": "correct horse battery",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body)
	}
}
