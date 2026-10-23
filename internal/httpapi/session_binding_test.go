package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Session binding over HTTP (IDN-16).
//
// The property that matters here is not "a mismatch is refused" — the authn package proves
// that — it is that the refusal reaches the client as its own problem code, on every guard,
// and that it does not quietly become "authentication required" or "CSRF token missing".

// getAs issues a GET with a chosen user agent and source address.
func (e *authEnv) getAs(t *testing.T, path string, cookie *http.Cookie, userAgent, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// loginAs signs in from a chosen user agent and source address.
func (e *authEnv) loginAs(t *testing.T, email, password, userAgent, remoteAddr string) *http.Cookie {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d, body %s", rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "billing_session" {
			return c
		}
	}
	t.Fatal("no session cookie was set")
	return nil
}

// bindingProblemCode reads the refusal and insists it carries an explanation, which is the
// difference between "refused with a code" and "silently redirected".
func bindingProblemCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content type = %q, want application/problem+json", ct)
	}
	var body struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("problem body: %v (%s)", err, rec.Body)
	}
	if body.Detail == "" {
		t.Fatal("the refusal has no detail a user can act on")
	}
	return body.Code
}

func setSessionBinding(t *testing.T, env *authEnv, mode, scope string) {
	t.Helper()
	for k, v := range map[string]string{"binding": mode, "binding_scope": scope} {
		if _, err := env.pool.Exec(context.Background(), `
			INSERT INTO settings (group_key, key, value) VALUES ('session', $1, $2)
			ON CONFLICT (group_key, key) DO UPDATE SET value = $2`, k, v); err != nil {
			t.Fatalf("set session.%s: %v", k, err)
		}
	}
}

// A session used from a different browser is refused with its own code, and the same session
// from where it was signed in keeps working. The second half matters: a refusal that also
// broke the legitimate device would be a lockout, not a control.
func TestSessionBindingRefusesForeignUserAgent(t *testing.T) {
	env := newAuthEnv(t)
	setSessionBinding(t, env, "user_agent", "all")
	env.createUser(t, "customer@example.test", "customer password 1", "")

	cookie := env.loginAs(t, "customer@example.test", "customer password 1",
		"browser-a", "203.0.113.10:5555")

	if rec := env.getAs(t, "/api/v1/auth/me", cookie, "browser-a", "198.51.100.9:1234"); rec.Code != http.StatusOK {
		t.Fatalf("the bound session was refused from a new address under user_agent mode: %d %s",
			rec.Code, rec.Body)
	}

	rec := env.getAs(t, "/api/v1/auth/me", cookie, "browser-b", "203.0.113.10:5555")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a foreign browser: status %d, want 403", rec.Code)
	}
	if code := bindingProblemCode(t, rec); code != "session_binding_mismatch" {
		t.Fatalf("problem code = %q, want session_binding_mismatch", code)
	}
	// The cookie must survive the refusal. Clearing it would sign the user out of the
	// device they are actually on because another device tried the cookie.
	for _, c := range rec.Result().Cookies() {
		if c.Name == "billing_session" && c.Value == "" {
			t.Fatal("the refusal cleared the session cookie, which turns a refusal into a sign-out")
		}
	}
}

// Under ip_and_user_agent mode an address change is the refusal, and the detail names the
// address rather than a browser.
func TestSessionBindingRefusesForeignAddress(t *testing.T) {
	env := newAuthEnv(t)
	setSessionBinding(t, env, "ip_and_user_agent", "all")
	env.createUser(t, "customer@example.test", "customer password 1", "")

	cookie := env.loginAs(t, "customer@example.test", "customer password 1",
		"browser-a", "203.0.113.10:5555")

	if rec := env.getAs(t, "/api/v1/auth/me", cookie, "browser-a", "203.0.113.10:9999"); rec.Code != http.StatusOK {
		t.Fatalf("the bound session was refused from the same address and browser: %d %s",
			rec.Code, rec.Body)
	}

	rec := env.getAs(t, "/api/v1/auth/me", cookie, "browser-a", "198.51.100.9:1234")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a foreign address: status %d, want 403", rec.Code)
	}
	if code := bindingProblemCode(t, rec); code != "session_binding_mismatch" {
		t.Fatalf("problem code = %q, want session_binding_mismatch", code)
	}
}

// A refused mutation says what is wrong with the session, not that a CSRF token is missing.
// The guards run in order and the outermost one is the CSRF check; a refusal that only exists
// in requireUser would be reported as the wrong problem here.
func TestSessionBindingRefusalBeatsTheCSRFGuard(t *testing.T) {
	env := newAuthEnv(t)
	setSessionBinding(t, env, "user_agent", "all")
	env.createUser(t, "customer@example.test", "customer password 1", "")

	cookie := env.loginAs(t, "customer@example.test", "customer password 1",
		"browser-a", "203.0.113.10:5555")

	body := bytes.NewReader([]byte(`{"current_password":"customer password 1","new_password":"another password 2"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/password/change", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "browser-b")
	req.RemoteAddr = "203.0.113.10:5555"
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	if code := bindingProblemCode(t, rec); code != "session_binding_mismatch" {
		t.Fatalf("problem code = %q, want session_binding_mismatch", code)
	}
}

// A public page must still render for a browser holding a refused session: the refusal is
// about the identity, not about the address being banned.
func TestSessionBindingDoesNotBlockPublicRoutes(t *testing.T) {
	env := newAuthEnv(t)
	setSessionBinding(t, env, "user_agent", "all")
	env.createUser(t, "customer@example.test", "customer password 1", "")

	cookie := env.loginAs(t, "customer@example.test", "customer password 1",
		"browser-a", "203.0.113.10:5555")

	if rec := env.getAs(t, "/healthz", cookie, "browser-b", "203.0.113.10:5555"); rec.Code != http.StatusOK {
		t.Fatalf("public health check: status %d, want 200", rec.Code)
	}
}
