package httpapi

// The challenge gate on the two anonymous endpoints that spend money: registration and the
// password-reset request (IDN-18).
//
// These tests are in-package because the gate, the seam that points verification at a fake
// siteverify server, and the handlers it wraps are all unexported. They drive the real
// handlers against the real database, so the properties under test are the ones production
// has:
//
//   - with no provider configured, registration makes no outbound call and behaves exactly
//     as it did before this feature existed;
//   - a failed challenge is refused with its own stable code, before anything is looked up,
//     so it cannot become an account oracle;
//   - an outage is a different code from a refusal, so an operator can tell them apart;
//   - the reset flow answers identically for a known and an unknown address whether the
//     challenge passes or fails.
//
// The one thing these tests cannot do is edit internal/httpapi/register.go, which another
// change owns. postWithChallenge below is the wiring the integrator adds there and in
// auth_lifecycle.go, written out so the composition is proven now rather than after the
// merge: decode the body, run the gate on the token it carries, then hand the same body to
// the handler.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/captcha"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/register"
	"github.com/catalyst/billing/internal/settings"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
)

// fakeSiteverify is a stand-in for Cloudflare, Google or hCaptcha: it answers whatever the
// test sets and records the form it was sent.
type fakeSiteverify struct {
	server *httptest.Server

	mu       sync.Mutex
	calls    int
	lastForm url.Values
	status   int
	body     string
}

func newFakeSiteverify(t *testing.T) *fakeSiteverify {
	t.Helper()
	f := &fakeSiteverify{status: http.StatusOK, body: `{"success":true}`}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("siteverify: parse form: %v", err)
		}
		f.mu.Lock()
		f.calls++
		f.lastForm = r.PostForm
		status, body := f.status, f.body
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeSiteverify) respond(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

func (f *fakeSiteverify) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeSiteverify) form() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastForm
}

// captchaEnv is a server with authentication wired and verification pointed at a fake
// provider.
type captchaEnv struct {
	srv  *Server
	pool *store.Pool
	fake *fakeSiteverify
}

func newCaptchaEnv(t *testing.T) *captchaEnv {
	t.Helper()

	// Registration hashes a password; the real parameters are deliberately expensive.
	previous := register.Argon2Params
	params := authn.DefaultArgon2Params()
	params.Memory = 19 * 1024
	params.Iterations = 1
	register.Argon2Params = func() authn.Argon2Params { return params }
	t.Cleanup(func() { register.Argon2Params = previous })

	pool := storetest.DB(t)
	srv := New(Config{Version: "test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		pool, payments.NewInbox(pool), "stripe", "acct_test", nil)

	cfg := authn.DefaultConfig()
	svc, err := authn.New(pool, mustHasher(t), cfg)
	if err != nil {
		t.Fatalf("authn: %v", err)
	}
	srv.WithAuth(&Auth{
		Service:    svc,
		Authz:      authn.NewAuthorizer(),
		Cookies:    CookieConfig{Secure: false},
		SessionTTL: cfg.SessionLifetime,
	})

	fake := newFakeSiteverify(t)
	previousService := newCaptchaService
	newCaptchaService = func(p *store.Pool) *captcha.Service {
		return captcha.NewService(p).WithEndpoint(fake.server.URL)
	}
	t.Cleanup(func() { newCaptchaService = previousService })

	return &captchaEnv{srv: srv, pool: pool, fake: fake}
}

// configure writes the 'captcha' settings group the way the settings screen does.
func (e *captchaEnv) configure(t *testing.T, values map[string]string) {
	t.Helper()
	settingsStore := settings.New(e.pool)
	for key, value := range values {
		if _, err := settingsStore.Set(context.Background(), "captcha", key, value, "", nil); err != nil {
			t.Fatalf("set captcha.%s: %v", key, err)
		}
	}
}

// provider configures a complete challenge.
func (e *captchaEnv) provider(t *testing.T, name, siteKey, secret string) {
	t.Helper()
	e.configure(t, map[string]string{"provider": name, "site_key": siteKey, "secret_key": secret})
}

// postWithChallenge runs one request the way the handlers will once the gate is wired in:
// the body is decoded once for the token, the gate decides, and the handler then reads the
// same body.
//
// This mirrors the one-line addition in register.go and auth_lifecycle.go exactly — the
// gate runs after the body is decoded and before anything is looked up — because the tests
// below are the only place that composition can be exercised while those files belong to
// another change.
func (e *captchaEnv) postWithChallenge(t *testing.T, handler http.HandlerFunc, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	var probe struct {
		CaptchaToken string `json:"captcha_token"`
	}
	// A body the handler itself will reject is left to the handler: the gate is not a JSON
	// validator, and refusing a malformed body with captcha_required would be a worse
	// answer than invalid_json.
	if err := json.Unmarshal(raw, &probe); err == nil {
		if !e.srv.requireCaptcha(rec, req, probe.CaptchaToken) {
			return rec
		}
	}

	// The request structs in register.go and auth_lifecycle.go do not declare a
	// captcha_token field yet, and their decoder refuses unknown fields, so the mirror
	// strips the token from the body it hands on. Declaring that field is the integrator's
	// other half of the wiring; what these tests are about is the gate in front of the
	// handler, not the handler's JSON guard.
	handled := make(map[string]any, len(body))
	for key, value := range body {
		if key == "captcha_token" {
			continue
		}
		handled[key] = value
	}
	handlerBody, err := json.Marshal(handled)
	if err != nil {
		t.Fatalf("marshal handler body: %v", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(handlerBody))
	req.ContentLength = int64(len(handlerBody))
	handler(rec, req)
	return rec
}

// register posts to the registration handler.
func (e *captchaEnv) register(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	if _, ok := body["display_name"]; !ok {
		body["display_name"] = "New Customer"
	}
	return e.postWithChallenge(t, e.srv.handleRegister, "/api/v1/auth/register", body)
}

// reset posts to the password-reset request handler.
func (e *captchaEnv) reset(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return e.postWithChallenge(t, e.srv.handleResetRequest, "/api/v1/auth/password-reset/request", body)
}

// userCount is how many accounts exist for an address.
func (e *captchaEnv) userCount(t *testing.T, email string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM users WHERE email = $1`, email).Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	return n
}

// resetMessages is how many reset mails were queued for an address.
func (e *captchaEnv) resetMessages(t *testing.T, email string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM notification_deliveries
		WHERE kind = 'password_reset' AND to_address = $1`, email).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	return n
}

// problemCode reads the stable code out of a problem+json body.
func problemCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem %q: %v", rec.Body.String(), err)
	}
	return problem.Code
}

// ---------------------------------------------------------------- registration --

// With no provider configured the registration path makes no outbound call at all, and the
// account is created exactly as before. This is the assertion that makes the feature safe to
// leave off — and it is not vacuous: the gate runs and is expected to do nothing.
func TestRegisterWithNoProviderMakesNoOutboundCall(t *testing.T) {
	env := newCaptchaEnv(t)

	rec := env.register(t, map[string]any{
		"email":    "new@example.test",
		"password": "a good password 1",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", rec.Code, rec.Body)
	}
	if env.fake.count() != 0 {
		t.Fatalf("provider none produced %d outbound call(s)", env.fake.count())
	}
	if n := env.userCount(t, "new@example.test"); n != 1 {
		t.Fatalf("the account was not created (%d rows)", n)
	}
}

// A provider is configured and the request carries no token: a distinct refusal, before any
// account work, so nothing was created.
func TestRegisterRequiresAChallengeWhenConfigured(t *testing.T) {
	env := newCaptchaEnv(t)
	env.provider(t, "turnstile", "0x4AAAAAAA-public", "0x4AAAAAAA-private")

	rec := env.register(t, map[string]any{
		"email":    "new@example.test",
		"password": "a good password 1",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != codeCaptchaRequired {
		t.Fatalf("code = %q, want %q", code, codeCaptchaRequired)
	}
	if env.fake.count() != 0 {
		t.Fatalf("an empty token produced %d outbound call(s)", env.fake.count())
	}
	if n := env.userCount(t, "new@example.test"); n != 0 {
		t.Fatalf("a refused challenge created %d account(s)", n)
	}
}

// The provider says the token is wrong: 403 captcha_failed, and no account.
func TestRegisterRefusesAFailedChallenge(t *testing.T) {
	env := newCaptchaEnv(t)
	env.provider(t, "recaptcha", "public-site-key", "server-secret-value")
	env.fake.respond(http.StatusOK, `{"success":false,"error-codes":["invalid-input-response"]}`)

	rec := env.register(t, map[string]any{
		"email":         "new@example.test",
		"password":      "a good password 1",
		"captcha_token": "a-bad-token",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != codeCaptchaFailed {
		t.Fatalf("code = %q, want %q", code, codeCaptchaFailed)
	}
	if env.fake.count() != 1 {
		t.Fatalf("the provider was asked %d times, want once", env.fake.count())
	}
	if got := env.fake.form().Get("secret"); got != "server-secret-value" {
		t.Fatalf("the provider was given secret %q", got)
	}
	if n := env.userCount(t, "new@example.test"); n != 0 {
		t.Fatalf("a refused challenge created %d account(s)", n)
	}
}

// A solved challenge lets the registration through.
func TestRegisterAcceptsASolvedChallenge(t *testing.T) {
	env := newCaptchaEnv(t)
	env.provider(t, "hcaptcha", "10000000-ffff-ffff-ffff-000000000001", "0x0000000000000000000000000000000000000000")
	env.fake.respond(http.StatusOK, `{"success":true}`)

	rec := env.register(t, map[string]any{
		"email":         "new@example.test",
		"password":      "a good password 1",
		"captcha_token": "a-good-token",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", rec.Code, rec.Body)
	}
	if env.fake.count() != 1 {
		t.Fatalf("the provider was asked %d times, want once", env.fake.count())
	}
	if n := env.userCount(t, "new@example.test"); n != 1 {
		t.Fatalf("the account was not created (%d rows)", n)
	}
}

// A provider outage is refused with the outage code, not the refusal code: the visitor did
// nothing wrong and the operator has an outage to look at.
func TestRegisterRefusesWhenTheProviderIsDown(t *testing.T) {
	env := newCaptchaEnv(t)
	env.provider(t, "turnstile", "0x4AAAAAAA-public", "0x4AAAAAAA-private")
	env.fake.respond(http.StatusBadGateway, `{"success":false}`)

	rec := env.register(t, map[string]any{
		"email":         "new@example.test",
		"password":      "a good password 1",
		"captcha_token": "a-token",
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != codeCaptchaUnavailable {
		t.Fatalf("code = %q, want %q", code, codeCaptchaUnavailable)
	}
	if n := env.userCount(t, "new@example.test"); n != 0 {
		t.Fatalf("an unverifiable request created %d account(s)", n)
	}
}

// A provider selected without its secret refuses rather than falling open.
func TestRegisterRefusesWhenTheProviderIsMisconfigured(t *testing.T) {
	env := newCaptchaEnv(t)
	env.configure(t, map[string]string{"provider": "turnstile", "site_key": "0x4AAAAAAA-public"})

	rec := env.register(t, map[string]any{
		"email":         "new@example.test",
		"password":      "a good password 1",
		"captcha_token": "a-token",
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != codeCaptchaUnavailable {
		t.Fatalf("code = %q, want %q", code, codeCaptchaUnavailable)
	}
	if env.fake.count() != 0 {
		t.Fatalf("an incomplete configuration produced %d outbound call(s)", env.fake.count())
	}
}

// -------------------------------------------------------------- password reset --

// The reset request must stay non-enumerating with a challenge in front of it: a failed
// challenge answers identically for a known and an unknown address, and so does a solved
// one.
func TestResetDoesNotEnumerateUnderAChallenge(t *testing.T) {
	env := newCaptchaEnv(t)
	env.provider(t, "turnstile", "0x4AAAAAAA-public", "0x4AAAAAAA-private")

	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO users (email, password_hash, display_name, email_verified_at)
		VALUES ('known@example.test', 'not-a-real-hash', 'Known', now())`); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// A failed challenge, for an address that exists and one that does not.
	env.fake.respond(http.StatusOK, `{"success":false,"error-codes":["invalid-input-response"]}`)
	known := env.reset(t, map[string]any{"email": "known@example.test", "captcha_token": "bad"})
	unknown := env.reset(t, map[string]any{"email": "nobody@example.test", "captcha_token": "bad"})

	if known.Code != http.StatusForbidden || unknown.Code != http.StatusForbidden {
		t.Fatalf("statuses = %d, %d; want 403 for both", known.Code, unknown.Code)
	}
	if known.Body.String() != unknown.Body.String() {
		t.Fatalf("a failed challenge distinguishes addresses:\n known:   %s\n unknown: %s",
			known.Body, unknown.Body)
	}
	if code := problemCode(t, known); code != codeCaptchaFailed {
		t.Fatalf("code = %q, want %q", code, codeCaptchaFailed)
	}
	if n := env.resetMessages(t, "known@example.test"); n != 0 {
		t.Fatalf("a refused challenge queued %d message(s)", n)
	}

	// A solved challenge, for the same two addresses: identical 202 bodies.
	env.fake.respond(http.StatusOK, `{"success":true}`)
	known = env.reset(t, map[string]any{"email": "known@example.test", "captcha_token": "good"})
	unknown = env.reset(t, map[string]any{"email": "nobody@example.test", "captcha_token": "good"})

	if known.Code != http.StatusAccepted || unknown.Code != http.StatusAccepted {
		t.Fatalf("statuses = %d, %d; want 202 for both", known.Code, unknown.Code)
	}
	if known.Body.String() != unknown.Body.String() {
		t.Fatalf("the accepted bodies differ:\n known:   %s\n unknown: %s", known.Body, unknown.Body)
	}
	if strings.Contains(known.Body.String(), "token") {
		t.Fatalf("the response contains a token: %s", known.Body)
	}
	// Only the address that exists produced a message, which is the whole point: the
	// answer is uniform, the effect is not.
	if n := env.resetMessages(t, "known@example.test"); n != 1 {
		t.Fatalf("the known address produced %d message(s), want 1", n)
	}
	if n := env.resetMessages(t, "nobody@example.test"); n != 0 {
		t.Fatalf("the unknown address produced %d message(s)", n)
	}
}

// The rate limit is still the backstop: the challenge does not buy a caller extra attempts.
func TestCaptchaDoesNotReplaceTheRateLimit(t *testing.T) {
	env := newCaptchaEnv(t)
	env.provider(t, "turnstile", "0x4AAAAAAA-public", "0x4AAAAAAA-private")
	env.fake.respond(http.StatusOK, `{"success":true}`)

	// The registration budget is five per source address per window.
	var last *httptest.ResponseRecorder
	for i := 0; i < 6; i++ {
		last = env.register(t, map[string]any{
			"email":         "spray" + string(rune('a'+i)) + "@example.test",
			"password":      "a good password 1",
			"captcha_token": "a-good-token",
		})
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d after six registrations, want 429; body %s", last.Code, last.Body)
	}
	if code := problemCode(t, last); code != "rate_limited" {
		t.Fatalf("code = %q, want rate_limited", code)
	}
}

// ------------------------------------------------------------ public config --

// The browser needs to know which widget to render, and must never be told the secret.
func TestCaptchaConfigEndpoint(t *testing.T) {
	env := newCaptchaEnv(t)

	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		env.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/captcha", nil))
		return rec
	}

	rec := get()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var unconfigured captcha.PublicConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &unconfigured); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if unconfigured.Provider != captcha.ProviderNone || unconfigured.Enabled || unconfigured.SiteKey != "" {
		t.Fatalf("unconfigured response = %+v", unconfigured)
	}

	env.provider(t, "turnstile", "0x4AAAAAAA-public", "0x4AAAAAAA-private")
	rec = get()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var configured captcha.PublicConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &configured); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !configured.Enabled || configured.Provider != captcha.ProviderTurnstile ||
		configured.SiteKey != "0x4AAAAAAA-public" {
		t.Fatalf("configured response = %+v", configured)
	}
	if strings.Contains(rec.Body.String(), "0x4AAAAAAA-private") {
		t.Fatalf("the response echoes the secret key: %s", rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}
}

// A challenge is never required for a request that does not carry a body to check: the gate
// is only called by the handlers that own one. This documents the seam's contract.
func TestRequireCaptchaWithNoStorageIsOpen(t *testing.T) {
	s := &Server{cfg: Config{}.withDefaults()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", nil)
	if !s.requireCaptcha(rec, req, "") {
		t.Fatalf("a server with no storage refused the request: %d %s", rec.Code, rec.Body)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("a no-op gate wrote a body: %s", rec.Body)
	}
}
