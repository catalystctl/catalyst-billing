package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/httpapi"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
)

// authEnv is a server with authentication wired, plus a helper to sign in.
type authEnv struct {
	srv    *httpapi.Server
	pool   *store.Pool
	hasher *authn.Hasher
	now    time.Time
}

func authParams() authn.Argon2Params {
	p := authn.DefaultArgon2Params()
	p.Memory = 19 * 1024
	p.Iterations = 1
	return p
}

func newAuthEnv(t *testing.T) *authEnv {
	t.Helper()
	pool := storetest.DB(t)
	hasher, err := authn.NewHasher(authParams())
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	env := &authEnv{pool: pool, hasher: hasher, now: time.Now().UTC()}

	cfg := authn.DefaultConfig()
	cfg.Now = func() time.Time { return env.now }
	svc, err := authn.New(pool, hasher, cfg)
	if err != nil {
		t.Fatalf("authn.New: %v", err)
	}

	verifier := payments.HMACSigner{
		Secret:          []byte(secret),
		Header:          "Stripe-Signature",
		TimestampHeader: "Stripe-Timestamp",
		Tolerance:       5 * time.Minute,
	}
	env.srv = httpapi.New(httpapi.Config{
		Version: "test",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, pool, payments.NewInbox(pool), "stripe", "acct_test", verifier).WithAuth(&httpapi.Auth{
		Service:    svc,
		Authz:      authn.NewAuthorizer(),
		Cookies:    httpapi.CookieConfig{Secure: false},
		SessionTTL: cfg.SessionLifetime,
	})
	return env
}

// createUser inserts a user with the given staff role (empty for a customer).
func (e *authEnv) createUser(t *testing.T, email, password, role string) string {
	t.Helper()
	hash, err := e.hasher.Hash(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	var id string
	if err := e.pool.QueryRow(context.Background(), `
		INSERT INTO users (email, password_hash, display_name, email_verified_at)
		VALUES ($1, $2, 'User', now()) RETURNING id`, email, hash).Scan(&id); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if role != "" {
		if _, err := e.pool.Exec(context.Background(),
			`INSERT INTO staff_roles (user_id, role) VALUES ($1, $2)`, id, role); err != nil {
			t.Fatalf("grant role: %v", err)
		}
	}
	return id
}

// login performs a real login and returns the session cookie and CSRF token.
func (e *authEnv) login(t *testing.T, email, password string) (*http.Cookie, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d, body %s", rec.Code, rec.Body)
	}

	var resp struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("login body: %v", err)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "billing_session" {
			return c, resp.CSRFToken
		}
	}
	t.Fatal("no session cookie was set")
	return nil, ""
}

func (e *authEnv) get(t *testing.T, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// ------------------------------------------------------------------- tests --

// The operator overview exposes money and customer counts, so it must not be
// reachable without a session. It used to be completely public.
func TestOverviewRequiresAuthentication(t *testing.T) {
	env := newAuthEnv(t)

	rec := env.get(t, "/api/v1/ops/overview", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous overview: status %d, want 401", rec.Code)
	}

	// A plain customer must not see it either: it is staff reporting data.
	env.createUser(t, "customer@example.test", "customer password 1", "")
	cookie, _ := env.login(t, "customer@example.test", "customer password 1")
	rec = env.get(t, "/api/v1/ops/overview", cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("customer overview: status %d, want 403", rec.Code)
	}

	// Support staff lack the reporting permission.
	env.createUser(t, "support@example.test", "support password 1", "support")
	cookie, _ = env.login(t, "support@example.test", "support password 1")
	rec = env.get(t, "/api/v1/ops/overview", cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("support overview: status %d, want 403", rec.Code)
	}

	// Billing staff hold report:read.
	env.createUser(t, "billing@example.test", "billing password 1", "billing")
	cookie, _ = env.login(t, "billing@example.test", "billing password 1")
	rec = env.get(t, "/api/v1/ops/overview", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("billing overview: status %d, want 200; body %s", rec.Code, rec.Body)
	}
}

func TestMetricsRequiresStaff(t *testing.T) {
	env := newAuthEnv(t)

	if rec := env.get(t, "/metrics", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous metrics: status %d, want 401", rec.Code)
	}
	env.createUser(t, "customer@example.test", "customer password 1", "")
	cookie, _ := env.login(t, "customer@example.test", "customer password 1")
	if rec := env.get(t, "/metrics", cookie); rec.Code != http.StatusForbidden {
		t.Fatalf("customer metrics: status %d, want 403", rec.Code)
	}
	env.createUser(t, "support@example.test", "support password 1", "support")
	cookie, _ = env.login(t, "support@example.test", "support password 1")
	if rec := env.get(t, "/metrics", cookie); rec.Code != http.StatusOK {
		t.Fatalf("support metrics: status %d, want 200", rec.Code)
	}
}

// Public endpoints must stay public: a webhook has no cookie, and an orchestrator
// health check has no session.
func TestPublicEndpointsStayPublic(t *testing.T) {
	env := newAuthEnv(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		if rec := env.get(t, path, nil); rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", path, rec.Code)
		}
	}
	// A webhook with no signature is rejected on its own merits (400), not with a
	// demand for a cookie (401).
	req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("the webhook endpoint demanded authentication")
	}
}

func TestLoginRejectsBadCredentials(t *testing.T) {
	env := newAuthEnv(t)
	env.createUser(t, "u@example.test", "a good password 1", "")

	body, _ := json.Marshal(map[string]string{"email": "u@example.test", "password": "wrong"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	// The message must not reveal whether the account exists.
	if bytes.Contains(rec.Body.Bytes(), []byte("unknown")) ||
		bytes.Contains(rec.Body.Bytes(), []byte("not found")) {
		t.Fatalf("response leaks account existence: %s", rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "billing_session" && c.Value != "" {
			t.Fatal("a failed login set a session cookie")
		}
	}
}

// The session cookie must be HttpOnly and the CSRF cookie must not be, which is
// what makes the double-submit pattern work without exposing the session.
func TestCookieAttributes(t *testing.T) {
	env := newAuthEnv(t)
	env.createUser(t, "u@example.test", "a good password 1", "")

	body, _ := json.Marshal(map[string]string{"email": "u@example.test", "password": "a good password 1"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)

	var sawSession, sawCSRF bool
	for _, c := range rec.Result().Cookies() {
		switch c.Name {
		case "billing_session":
			sawSession = true
			if !c.HttpOnly {
				t.Fatal("the session cookie is readable by script")
			}
			if c.SameSite != http.SameSiteLaxMode {
				t.Fatalf("session SameSite = %v, want Lax", c.SameSite)
			}
		case "billing_csrf":
			sawCSRF = true
			if c.HttpOnly {
				t.Fatal("the CSRF cookie is HttpOnly, so the browser client cannot read it")
			}
		}
	}
	if !sawSession || !sawCSRF {
		t.Fatalf("cookies set: session=%v csrf=%v", sawSession, sawCSRF)
	}
}

// A state-changing request without the CSRF token must be refused.
func TestLogoutRequiresCSRF(t *testing.T) {
	env := newAuthEnv(t)
	env.createUser(t, "u@example.test", "a good password 1", "")
	cookie, csrf := env.login(t, "u@example.test", "a good password 1")

	// No token.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("logout without CSRF: status %d, want 403", rec.Code)
	}
	// The session must still be alive.
	if got := env.get(t, "/api/v1/auth/me", cookie); got.Code != http.StatusOK {
		t.Fatalf("session died on a rejected logout: %d", got.Code)
	}

	// Wrong token.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", "not-the-token")
	rec = httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("logout with a wrong CSRF token: status %d, want 403", rec.Code)
	}

	// Correct token.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	rec = httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout with the correct token: status %d, want 200", rec.Code)
	}
	// The session is now dead server-side, not merely un-cookied.
	if got := env.get(t, "/api/v1/auth/me", cookie); got.Code != http.StatusUnauthorized {
		t.Fatalf("session still valid after logout: %d", got.Code)
	}
}

func TestMeReturnsIdentityAndPermissions(t *testing.T) {
	env := newAuthEnv(t)
	env.createUser(t, "billing@example.test", "billing password 1", "billing")
	cookie, csrf := env.login(t, "billing@example.test", "billing password 1")

	rec := env.get(t, "/api/v1/auth/me", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("me: status %d", rec.Code)
	}
	var resp struct {
		User struct {
			Email   string   `json:"email"`
			IsStaff bool     `json:"is_staff"`
			Roles   []string `json:"roles"`
		} `json:"user"`
		Permissions []string `json:"permissions"`
		CSRFToken   string   `json:"csrf_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.User.Email != "billing@example.test" || !resp.User.IsStaff {
		t.Fatalf("user = %+v", resp.User)
	}
	if resp.CSRFToken != csrf {
		t.Fatal("the session's CSRF token changed")
	}
	// Billing holds refund but not terminate: the permission list must reflect it.
	var hasRefund, hasTerminate bool
	for _, p := range resp.Permissions {
		switch p {
		case "payment:refund":
			hasRefund = true
		case "service:terminate":
			hasTerminate = true
		}
	}
	if !hasRefund || hasTerminate {
		t.Fatalf("permissions = %v, want refund without terminate", resp.Permissions)
	}
}

// A revoked or expired session must be treated as anonymous, and the stale cookie
// cleared, so the browser stops sending it.
func TestDeadSessionTreatedAsAnonymous(t *testing.T) {
	env := newAuthEnv(t)
	env.createUser(t, "u@example.test", "a good password 1", "")
	cookie, _ := env.login(t, "u@example.test", "a good password 1")

	// Revoke it out from under the browser.
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE sessions SET revoked_at = now()`); err != nil {
		t.Fatal(err)
	}

	rec := env.get(t, "/api/v1/auth/me", cookie)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session: status %d, want 401", rec.Code)
	}
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == "billing_session" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("the dead session cookie was not cleared")
	}
}

// A cross-site login attempt is refused, which closes login CSRF.
func TestLoginRejectsForeignOrigin(t *testing.T) {
	env := newAuthEnv(t)
	env.createUser(t, "u@example.test", "a good password 1", "")

	body, _ := json.Marshal(map[string]string{"email": "u@example.test", "password": "a good password 1"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Origin", "https://evil.example")
	req.Host = "panel.example"
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign-origin login: status %d, want 403", rec.Code)
	}
}

// An unknown field in the login body is rejected rather than ignored.
func TestLoginRejectsUnknownFields(t *testing.T) {
	env := newAuthEnv(t)
	body := []byte(`{"email":"u@example.test","password":"x","role":"administrator"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// Without authentication configured, guarded routes must refuse rather than fall
// open. This is the failure mode that matters: a missing dependency must not
// remove the lock.
func TestGuardedRoutesFailClosedWithoutAuth(t *testing.T) {
	pool := storetest.DB(t)
	srv := httpapi.New(httpapi.Config{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, pool, payments.NewInbox(pool), "stripe", "acct_test", nil)

	for _, path := range []string{"/api/v1/ops/overview", "/metrics", "/api/v1/auth/me"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden &&
			rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s without auth config: status %d, want a refusal", path, rec.Code)
		}
	}
}

// A customer with no staff role must get an empty roles list, not JSON null.
//
// The wire shape matters because it is what a client iterates. `roles: null` in Go's encoder
// means a nil slice, and a UI that does `user.roles.length` or `roles.join(", ")` throws
// before the first screen renders. The bug this pins was exactly that: the shell crashed on
// the portal for every non-staff user, and no Go test noticed because none of them looked at
// the encoding of an empty list.
func TestMeEncodesEmptyRolesAsAList(t *testing.T) {
	env := newAuthEnv(t)
	env.createUser(t, "customer@example.test", "customer password 1", "")
	cookie, _ := env.login(t, "customer@example.test", "customer password 1")

	rec := env.get(t, "/api/v1/auth/me", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("me: status %d", rec.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	var user map[string]json.RawMessage
	if err := json.Unmarshal(raw["user"], &user); err != nil {
		t.Fatal(err)
	}

	roles, ok := user["roles"]
	if !ok {
		t.Fatal("the user object has no roles key")
	}
	if string(roles) == "null" {
		t.Fatal("roles is null; a client cannot iterate it")
	}
	var list []string
	if err := json.Unmarshal(roles, &list); err != nil {
		t.Fatalf("roles is not a JSON array: %s", roles)
	}
	if len(list) != 0 {
		t.Fatalf("roles = %v, want empty", list)
	}

	perms, ok := raw["permissions"]
	if !ok {
		t.Fatal("the response has no permissions key")
	}
	if string(perms) == "null" {
		t.Fatal("permissions is null; a client cannot iterate it")
	}
}
