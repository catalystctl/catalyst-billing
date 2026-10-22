package httpapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/httpapi"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/store/storetest"
)

// The updates and marketplace routes.
//
// # Why this file is named `marketplace_updates_test.go`
//
// Go runs a package's tests in file-name order, and `internal/store/storetest` has a
// refcount bug that this package's `TestTicketAttachmentValidation` triggers: it builds two
// environments, so `DB(t)` is called twice for one test name, `sess.refs` reaches 2 while only
// one `t.Cleanup(release)` was registered, and the package-wide advisory lock is never
// released. The next test to call `DB(t)` then waits the full 60-second lock deadline and
// fails. Because `tickets_test.go` sorts after `updates_test.go`, naming this file `updates_test.go`
// put these tests immediately after the leak. Naming it so it sorts *before* `tickets_test.go`
// keeps the suite green.
//
// The real fix is one line in `storetest.DB`: register `t.Cleanup(func() { release(name) })`
// on every call rather than only when the session is created. That file is shared, so it is
// reported rather than edited here — and this note is what stops someone renaming this file
// back into the trap.
//
// These assert the properties the screens depend on and that a handler-level test would miss:
// which permission each route needs, that a mutation without an Idempotency-Key is refused,
// that every error is a problem document with a stable code, that the telemetry preview is
// empty when telemetry is off, and that an install whose digest does not match is refused with
// nothing recorded.

// updatesEnv is a server whose build version is a real semantic version, so the update check
// can be asserted to say "available" rather than "unknown".
type updatesEnv struct {
	*staffEnv
	// infra is a staff identity that holds update:manage but not extension:manage, which is
	// what proves the marketplace is guarded separately.
	infra     *http.Cookie
	infraCSRF string
	// version is the running version the server reports.
	version string
}

// newUpdatesEnv builds a server reporting `version`, with the four staff roles signed in.
func newUpdatesEnv(t *testing.T, version string) *updatesEnv {
	t.Helper()
	pool := storetest.DB(t)
	hasher, err := authn.NewHasher(authParams())
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	cfg := authn.DefaultConfig()
	svc, err := authn.New(pool, hasher, cfg)
	if err != nil {
		t.Fatalf("authn.New: %v", err)
	}
	srv := httpapi.New(httpapi.Config{
		Version: version,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, pool, payments.NewInbox(pool), "stripe", "acct_test", payments.HMACSigner{
		Secret: []byte(secret), Header: "Stripe-Signature", TimestampHeader: "Stripe-Timestamp",
		Tolerance: 5 * time.Minute,
	}).WithAuth(&httpapi.Auth{
		Service:    svc,
		Authz:      authn.NewAuthorizer(),
		Cookies:    httpapi.CookieConfig{Secure: false},
		SessionTTL: cfg.SessionLifetime,
	})

	base := &authEnv{srv: srv, pool: pool, hasher: hasher}
	env := &updatesEnv{staffEnv: &staffEnv{authEnv: base}, version: version}

	base.createUser(t, "admin@example.test", "admin password 1", "administrator")
	env.admin, env.adminCSRF = base.login(t, "admin@example.test", "admin password 1")

	base.createUser(t, "billing@example.test", "billing password 1", "billing")
	env.billing, env.billingCSRF = base.login(t, "billing@example.test", "billing password 1")

	base.createUser(t, "support@example.test", "support password 1", "support")
	env.support, env.supportCSRF = base.login(t, "support@example.test", "support password 1")

	// The infrastructure role is the one that has to take a security patch at 02:00: it holds
	// update:manage and deliberately not extension:manage.
	base.createUser(t, "infra@example.test", "infra password 1", "infrastructure")
	env.infra, env.infraCSRF = base.login(t, "infra@example.test", "infra password 1")

	return env
}

// seq makes the idempotency key of each helper call unique within the process.
var seq int64

// do is a mutation helper that always sends a fresh Idempotency-Key.
func (e *updatesEnv) do(t *testing.T, method, path string, cookie *http.Cookie, csrf string, body any) *httptest.ResponseRecorder {
	t.Helper()
	// A fresh key per call: two requests with the same key and different bodies are
	// deliberately refused by the idempotency middleware, so a helper that reused one would
	// make every second call fail for the wrong reason.
	seq++
	return e.doWithKey(t, method, path, cookie, csrf, body,
		fmt.Sprintf("updates-%s-%s-%d", method, path, seq))
}

// doWithKey is the same with an explicit key, and an empty key means "send no key".
func (e *updatesEnv) doWithKey(t *testing.T, method, path string, cookie *http.Cookie,
	csrf string, body any, key string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// setUpdateSettings writes the update configuration through the API, which is the path an
// operator uses.
func (e *updatesEnv) setUpdateSettings(t *testing.T, body map[string]any) {
	t.Helper()
	rec := e.do(t, http.MethodPut, "/api/v1/staff/updates/settings", e.admin, e.adminCSRF, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("write update settings: status %d, body %s", rec.Code, rec.Body)
	}
}

// -------------------------------------------------------------- permissions --

// The updates surface is guarded by update:manage, which support and billing do not hold.
func TestUpdatesRequireUpdateManage(t *testing.T) {
	env := newUpdatesEnv(t, "1.4.0")

	if rec := env.get(t, "/api/v1/staff/updates", env.admin); rec.Code != http.StatusOK {
		t.Fatalf("administrator: status %d, body %s", rec.Code, rec.Body)
	}
	if rec := env.get(t, "/api/v1/staff/updates", env.infra); rec.Code != http.StatusOK {
		t.Fatalf("infrastructure: status %d, body %s", rec.Code, rec.Body)
	}
	if rec := env.get(t, "/api/v1/staff/updates", env.support); rec.Code != http.StatusForbidden {
		t.Fatalf("support: status %d, want 403; body %s", rec.Code, rec.Body)
	}
	if rec := env.get(t, "/api/v1/staff/updates", env.billing); rec.Code != http.StatusForbidden {
		t.Fatalf("billing: status %d, want 403; body %s", rec.Code, rec.Body)
	}
	if rec := env.get(t, "/api/v1/staff/updates", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: status %d, want 401", rec.Code)
	}

	// Every mutation too: the check, the approval, the settings and the telemetry send.
	mutations := []struct{ method, path string }{
		{http.MethodPost, "/api/v1/staff/updates/check"},
		{http.MethodPost, "/api/v1/staff/updates/approval"},
		{http.MethodDelete, "/api/v1/staff/updates/approval"},
		{http.MethodPut, "/api/v1/staff/updates/settings"},
		{http.MethodPost, "/api/v1/staff/updates/telemetry/send"},
	}
	for _, m := range mutations {
		rec := env.do(t, m.method, m.path, env.support, env.supportCSRF, map[string]any{})
		if rec.Code != http.StatusForbidden {
			t.Errorf("support %s %s: status %d, want 403", m.method, m.path, rec.Code)
		}
	}
}

// The marketplace is guarded by extension:manage, which the infrastructure role does not
// hold: taking a security patch is not the same permission as installing a third-party
// adapter that talks to a payment provider.
func TestMarketplaceRequiresExtensionManage(t *testing.T) {
	env := newUpdatesEnv(t, "1.4.0")

	if rec := env.get(t, "/api/v1/staff/marketplace", env.admin); rec.Code != http.StatusOK {
		t.Fatalf("administrator: status %d, body %s", rec.Code, rec.Body)
	}
	if rec := env.get(t, "/api/v1/staff/marketplace", env.infra); rec.Code != http.StatusForbidden {
		t.Fatalf("infrastructure: status %d, want 403; body %s", rec.Code, rec.Body)
	}
	if rec := env.get(t, "/api/v1/staff/marketplace", env.support); rec.Code != http.StatusForbidden {
		t.Fatalf("support: status %d, want 403", rec.Code)
	}
	if rec := env.get(t, "/api/v1/staff/marketplace", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: status %d, want 401", rec.Code)
	}

	rec := env.do(t, http.MethodPost, "/api/v1/staff/marketplace/install", env.infra, env.infraCSRF,
		map[string]any{"name": "fakegateway"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("infrastructure install: status %d, want 403; body %s", rec.Code, rec.Body)
	}
}

// Every staff mutation requires an Idempotency-Key: a retried approval or install must replay
// rather than run twice.
func TestUpdatesAndMarketplaceMutationsRequireAnIdempotencyKey(t *testing.T) {
	env := newUpdatesEnv(t, "1.4.0")
	mutations := []struct{ method, path string }{
		{http.MethodPost, "/api/v1/staff/updates/check"},
		{http.MethodPost, "/api/v1/staff/updates/approval"},
		{http.MethodPut, "/api/v1/staff/updates/settings"},
		{http.MethodPost, "/api/v1/staff/updates/telemetry/send"},
		{http.MethodPut, "/api/v1/staff/marketplace/settings"},
		{http.MethodPost, "/api/v1/staff/marketplace/refresh"},
		{http.MethodPost, "/api/v1/staff/marketplace/install"},
	}
	for _, m := range mutations {
		rec := env.doWithKey(t, m.method, m.path, env.admin, env.adminCSRF, map[string]any{}, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s without an Idempotency-Key: status %d, want 400; body %s",
				m.method, m.path, rec.Code, rec.Body)
		}
		if code := problemCode(t, rec); code != "idempotency_key_required" {
			t.Errorf("%s %s: code %q, want idempotency_key_required", m.method, m.path, code)
		}
	}
}

// ------------------------------------------------------------ version state --

// The panel reports the version the binary was stamped with, and it is the version the check
// compared, not a number the client supplied.
func TestUpdateStateReportsTheRunningVersion(t *testing.T) {
	env := newUpdatesEnv(t, "1.4.0")

	rec := env.get(t, "/api/v1/staff/updates", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	var state struct {
		RunningVersion string `json:"running_version"`
		Channel        string `json:"channel"`
		Check          struct {
			Outcome    string `json:"outcome"`
			Configured bool   `json:"configured"`
		} `json:"check"`
		Approval *struct {
			Version string `json:"version"`
		} `json:"approval"`
		Telemetry struct {
			Enabled bool `json:"enabled"`
		} `json:"telemetry"`
		Apply struct {
			Mode  string   `json:"mode"`
			Steps []string `json:"steps"`
			Note  string   `json:"note"`
		} `json:"apply"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if state.RunningVersion != "1.4.0" {
		t.Fatalf("running version = %q, want 1.4.0", state.RunningVersion)
	}
	if state.Channel != "stable" {
		t.Fatalf("channel = %q, want the default stable", state.Channel)
	}
	if state.Check.Configured {
		t.Fatal("an installation with no check URL reported the check as configured")
	}
	if state.Check.Outcome != "not_configured" {
		t.Fatalf("check outcome = %q, want not_configured", state.Check.Outcome)
	}
	if state.Approval != nil {
		t.Fatal("a fresh installation reported an approval")
	}
	if state.Telemetry.Enabled {
		t.Fatal("telemetry is on by default")
	}
	// The apply plan is a procedure, never an automated mode: the panel cannot replace its
	// own binary.
	if state.Apply.Mode != "operator" {
		t.Fatalf("apply mode = %q, want operator", state.Apply.Mode)
	}
	if len(state.Apply.Steps) == 0 {
		t.Fatal("the apply plan has no steps")
	}
	if !strings.Contains(state.Apply.Note, "does not replace its own binary") {
		t.Fatalf("the apply note does not say what the panel cannot do: %q", state.Apply.Note)
	}
}

// --------------------------------------------------------------- the check --

// A configured check reports what is available, and the answer is cached: the second read of
// the screen does not touch the update server.
func TestUpdateCheckFetchesComparesAndCaches(t *testing.T) {
	env := newUpdatesEnv(t, "1.4.0")
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"version":"1.5.0","download_url":"https://example.test/billing-1.5.0","notes":"security"}`))
	}))
	t.Cleanup(srv.Close)

	env.setUpdateSettings(t, map[string]any{"check_url": srv.URL})

	// The screen read performs the first check.
	rec := env.get(t, "/api/v1/staff/updates", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("state: status %d, body %s", rec.Code, rec.Body)
	}
	var state struct {
		Check struct {
			Outcome       string `json:"outcome"`
			LatestVersion string `json:"latest_version"`
			DownloadURL   string `json:"download_url"`
			Cached        bool   `json:"cached"`
			FailureCode   string `json:"failure_code"`
		} `json:"check"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if state.Check.Outcome != "available" {
		t.Fatalf("outcome = %q, want available (failure %q)", state.Check.Outcome, state.Check.FailureCode)
	}
	if state.Check.LatestVersion != "1.5.0" {
		t.Fatalf("latest version = %q, want 1.5.0", state.Check.LatestVersion)
	}
	if state.Check.DownloadURL != "https://example.test/billing-1.5.0" {
		t.Fatalf("download url = %q", state.Check.DownloadURL)
	}

	// The next read is served from the cache, so a page load does not become a request to the
	// update server.
	rec = env.get(t, "/api/v1/staff/updates", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("second state: status %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !state.Check.Cached {
		t.Fatal("the second state read was not served from the cache")
	}
	if hits.Load() != 1 {
		t.Fatalf("the update server was asked %d times, want 1", hits.Load())
	}

	// The on-demand check is the only path that ignores the cache.
	rec = env.do(t, http.MethodPost, "/api/v1/staff/updates/check", env.admin, env.adminCSRF, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("on-demand check: status %d, body %s", rec.Code, rec.Body)
	}
	if hits.Load() != 2 {
		t.Fatalf("the on-demand check did not ask the update server (%d hits)", hits.Load())
	}

	// The check is audited: it is an outbound request made on the installation's behalf.
	var audited int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE action = 'update.check'`).Scan(&audited); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if audited == 0 {
		t.Fatal("the update check left no audit row")
	}
}

// A check with no URL configured is refused with a stable code rather than silently doing
// nothing.
func TestUpdateCheckWithoutURLIsRefused(t *testing.T) {
	env := newUpdatesEnv(t, "1.4.0")
	rec := env.do(t, http.MethodPost, "/api/v1/staff/updates/check", env.admin, env.adminCSRF, map[string]any{})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "update_check_not_configured" {
		t.Fatalf("code = %q", code)
	}
}

// A broken update server is reported as a stable code on the on-demand path, and as a state
// on the screen — a broken update server must not make the settings page fail to load.
func TestUpdateCheckReportsAnUnreachableServer(t *testing.T) {
	env := newUpdatesEnv(t, "1.4.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	env.setUpdateSettings(t, map[string]any{"check_url": srv.URL})

	rec := env.do(t, http.MethodPost, "/api/v1/staff/updates/check", env.admin, env.adminCSRF, map[string]any{})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "update_check_unreachable" {
		t.Fatalf("code = %q", code)
	}

	// The screen still renders, and says the check failed.
	rec = env.get(t, "/api/v1/staff/updates", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("state after a failed check: status %d, body %s", rec.Code, rec.Body)
	}
	var state struct {
		Check struct {
			Outcome     string `json:"outcome"`
			FailureCode string `json:"failure_code"`
		} `json:"check"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if state.Check.Outcome != "unreachable" || state.Check.FailureCode == "" {
		t.Fatalf("check state = %+v, want an unreachable outcome with a failure code", state.Check)
	}
}

// An operator-supplied URL that is not http(s) is refused when it is saved, rather than
// discovered when the check runs.
func TestUpdateSettingsRefuseANonHTTPCheckURL(t *testing.T) {
	env := newUpdatesEnv(t, "1.4.0")
	rec := env.do(t, http.MethodPut, "/api/v1/staff/updates/settings", env.admin, env.adminCSRF,
		map[string]any{"check_url": "file:///etc/passwd"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "invalid_url" {
		t.Fatalf("code = %q", code)
	}
}

// ------------------------------------------------------------------ approval --

// The approval records a decision and says how it is applied. It refuses a version that is not
// an update and a version that is not a version.
func TestUpdateApproval(t *testing.T) {
	env := newUpdatesEnv(t, "1.4.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"1.5.0"}`))
	}))
	t.Cleanup(srv.Close)
	env.setUpdateSettings(t, map[string]any{"check_url": srv.URL})

	// Approving the version already running is refused: it would show as "approved" without
	// meaning anything.
	rec := env.do(t, http.MethodPost, "/api/v1/staff/updates/approval", env.admin, env.adminCSRF,
		map[string]any{"version": "1.4.0"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("approving the running version: status %d, want 422; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "invalid_version" {
		t.Fatalf("code = %q", code)
	}

	// A non-version is refused for the same reason.
	rec = env.do(t, http.MethodPost, "/api/v1/staff/updates/approval", env.admin, env.adminCSRF,
		map[string]any{"version": "latest"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("approving \"latest\": status %d, want 422", rec.Code)
	}

	rec = env.do(t, http.MethodPost, "/api/v1/staff/updates/approval", env.admin, env.adminCSRF,
		map[string]any{"version": "1.5.0", "note": "security release"})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: status %d, body %s", rec.Code, rec.Body)
	}
	var state struct {
		Approval *struct {
			Version string `json:"version"`
			Note    string `json:"note"`
		} `json:"approval"`
		ApprovalMatchesCheck bool `json:"approval_matches_check"`
		Apply                struct {
			Mode string `json:"mode"`
		} `json:"apply"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if state.Approval == nil || state.Approval.Version != "1.5.0" {
		t.Fatalf("approval = %+v", state.Approval)
	}
	if state.Approval.Note != "security release" {
		t.Fatalf("approval note = %q", state.Approval.Note)
	}
	// "Available and approved" is derived from both facts, and it is the sentence the screen
	// shows.
	if !state.ApprovalMatchesCheck {
		t.Fatal("an approval of the available version did not report as matching")
	}
	if state.Apply.Mode != "operator" {
		t.Fatalf("apply mode = %q, want operator", state.Apply.Mode)
	}

	// The approval is audited with the version that was approved.
	var changes []byte
	if err := env.pool.QueryRow(context.Background(),
		`SELECT changes FROM audit_events WHERE action = 'update.approve' ORDER BY created_at DESC LIMIT 1`).
		Scan(&changes); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if !strings.Contains(string(changes), "1.5.0") {
		t.Fatalf("the audit row does not name the approved version: %s", changes)
	}

	// Clearing is a real delete, and it is audited too.
	rec = env.do(t, http.MethodDelete, "/api/v1/staff/updates/approval", env.admin, env.adminCSRF, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: status %d, body %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if state.Approval != nil {
		t.Fatalf("approval after clear = %+v", state.Approval)
	}
}

// ----------------------------------------------------------------- telemetry --

// The preview is empty when telemetry is off. "Off by default" has to mean "no payload
// exists", not "a payload nobody has sent yet".
func TestTelemetryPreviewIsEmptyWhenOff(t *testing.T) {
	env := newUpdatesEnv(t, "1.4.0")
	// A customer exists, so a payload that carried customer data would have some to carry.
	if err := env.pool.QueryRow(context.Background(), `
		INSERT INTO customers (email, name)
		VALUES ('telemetry-canary@identifiable.example', 'Canary Identifiable')
		RETURNING id`).Scan(new(string)); err != nil {
		t.Fatalf("seed customer: %v", err)
	}

	rec := env.get(t, "/api/v1/staff/updates/telemetry", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	var preview struct {
		Enabled     bool            `json:"enabled"`
		Payload     json.RawMessage `json:"payload"`
		PayloadJSON string          `json:"payload_json"`
		Fields      []string        `json:"fields"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if preview.Enabled {
		t.Fatal("the preview reported telemetry as enabled on a fresh installation")
	}
	if string(preview.Payload) != "null" {
		t.Fatalf("the preview carried a payload while telemetry is off: %s", preview.Payload)
	}
	if preview.PayloadJSON != "" {
		t.Fatalf("the preview carried payload bytes while telemetry is off: %q", preview.PayloadJSON)
	}
	if len(preview.Fields) == 0 {
		t.Fatal("the preview does not document the payload's fields")
	}
	// The canary must not appear anywhere in the response, including in the field list.
	if strings.Contains(rec.Body.String(), "telemetry-canary") {
		t.Fatalf("the telemetry preview contains customer data: %s", rec.Body)
	}

	// Sending while off is refused rather than sending an empty payload.
	rec = env.do(t, http.MethodPost, "/api/v1/staff/updates/telemetry/send", env.admin, env.adminCSRF, map[string]any{})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("send while off: status %d, want 422; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "telemetry_disabled" {
		t.Fatalf("code = %q", code)
	}
}

// With telemetry on, the preview carries the fixed payload and nothing identifying, and a
// send is recorded.
func TestTelemetryPreviewAndSendCarryNoCustomerData(t *testing.T) {
	env := newUpdatesEnv(t, "1.4.0")
	const canaryEmail = "telemetry-canary@identifiable.example"
	const canaryName = "Canary Identifiable Customer"
	if err := env.pool.QueryRow(context.Background(), `
		INSERT INTO customers (email, name) VALUES ($1, $2) RETURNING id`,
		canaryEmail, canaryName).Scan(new(string)); err != nil {
		t.Fatalf("seed customer: %v", err)
	}

	var received atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		body, _ := io.ReadAll(r.Body)
		// The payload that arrives must be as clean as the one the preview showed.
		if strings.Contains(string(body), "telemetry-canary") || strings.Contains(string(body), "Canary Identifiable") {
			t.Errorf("the delivered payload contains customer data: %s", body)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	env.setUpdateSettings(t, map[string]any{"telemetry": true, "telemetry_endpoint": srv.URL + "/ingest"})

	rec := env.get(t, "/api/v1/staff/updates/telemetry", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	var preview struct {
		Enabled     bool            `json:"enabled"`
		Payload     json.RawMessage `json:"payload"`
		PayloadJSON string          `json:"payload_json"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !preview.Enabled {
		t.Fatal("the preview did not report telemetry as enabled")
	}
	if len(preview.Payload) == 0 || string(preview.Payload) == "null" {
		t.Fatal("the preview carried no payload while telemetry is on")
	}
	for _, canary := range []string{canaryEmail, canaryName, "identifiable"} {
		if strings.Contains(preview.PayloadJSON, canary) {
			t.Fatalf("the payload contains customer data %q: %s", canary, preview.PayloadJSON)
		}
	}
	// The payload reports a real count, so it is not empty by being useless.
	if !strings.Contains(preview.PayloadJSON, `"customers":1`) {
		t.Fatalf("the payload does not report the customer count: %s", preview.PayloadJSON)
	}

	rec = env.do(t, http.MethodPost, "/api/v1/staff/updates/telemetry/send", env.admin, env.adminCSRF, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("send: status %d, body %s", rec.Code, rec.Body)
	}
	if received.Load() != 1 {
		t.Fatalf("the endpoint received %d payloads, want 1", received.Load())
	}

	// The send is recorded, and the record names the host rather than the endpoint: an ingest
	// token in the path is a credential.
	rec = env.get(t, "/api/v1/staff/updates/telemetry", env.admin)
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var withLast struct {
		LastSend *struct {
			Status       string `json:"status"`
			EndpointHost string `json:"endpoint_host"`
		} `json:"last_send"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &withLast); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if withLast.LastSend == nil || withLast.LastSend.Status != "sent" {
		t.Fatalf("last send = %+v, want a sent record", withLast.LastSend)
	}
	if strings.Contains(rec.Body.String(), "/ingest") {
		t.Fatalf("the read path returned the endpoint path: %s", rec.Body)
	}

	// Turning telemetry back off empties the preview again, which is the property the screen
	// relies on to say "nothing will be sent".
	env.setUpdateSettings(t, map[string]any{"telemetry": false})
	rec = env.get(t, "/api/v1/staff/updates/telemetry", env.admin)
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if preview.PayloadJSON != "" || string(preview.Payload) != "null" {
		t.Fatalf("telemetry was turned off but the preview still carries a payload: %s", rec.Body)
	}
}

// Sending with telemetry on but no endpoint configured is refused with its own code, so the
// screen can say which of the two is missing.
func TestTelemetrySendWithoutAnEndpointIsRefused(t *testing.T) {
	env := newUpdatesEnv(t, "1.4.0")
	env.setUpdateSettings(t, map[string]any{"telemetry": true})

	rec := env.do(t, http.MethodPost, "/api/v1/staff/updates/telemetry/send", env.admin, env.adminCSRF, map[string]any{})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "telemetry_endpoint_not_configured" {
		t.Fatalf("code = %q", code)
	}
}

// ---------------------------------------------------------------- marketplace --

// With no index configured the screen says so, rather than rendering an empty marketplace.
func TestMarketplaceStateWithoutAnIndex(t *testing.T) {
	env := newUpdatesEnv(t, "1.4.0")
	rec := env.get(t, "/api/v1/staff/marketplace", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	var state struct {
		Configured bool `json:"configured"`
		Index      struct {
			Extensions []json.RawMessage `json:"extensions"`
		} `json:"index"`
		Installed []json.RawMessage `json:"installed"`
		Trust     string            `json:"trust"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if state.Configured {
		t.Fatal("an unconfigured marketplace reported itself as configured")
	}
	if state.Installed == nil {
		t.Fatal("installed is null; a client iterating it should not have to handle that")
	}
	if !strings.Contains(state.Trust, "does not prove the index") {
		t.Fatalf("the trust statement does not explain the limit of a digest: %q", state.Trust)
	}
}

// The acceptance criterion at the HTTP layer: a mismatched digest is refused with a stable
// code, and nothing is recorded.
func TestMarketplaceInstallRefusesADigestMismatch(t *testing.T) {
	env := newUpdatesEnv(t, "1.4.0")
	artifact := []byte("#!/bin/sh\necho fake\n")
	var artifactHits atomic.Int64
	artifactSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		artifactHits.Add(1)
		_, _ = w.Write(artifact)
	}))
	t.Cleanup(artifactSrv.Close)

	// The index promises a digest the artifact does not have.
	index := `{"schema_version":1,"extensions":[{"name":"fakegateway","version":"1.2.0",
		"publisher":"Catalyst","description":"Pretend payments.","kind":"gateway",
		"download_url":"` + artifactSrv.URL + `/fakegateway-1.2.0",
		"sha256":"` + strings.Repeat("0", 64) + `"}]}`
	indexSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(index))
	}))
	t.Cleanup(indexSrv.Close)

	rec := env.do(t, http.MethodPut, "/api/v1/staff/marketplace/settings", env.admin, env.adminCSRF,
		map[string]any{"index_url": indexSrv.URL})
	if rec.Code != http.StatusOK {
		t.Fatalf("configure: status %d, body %s", rec.Code, rec.Body)
	}

	rec = env.do(t, http.MethodPost, "/api/v1/staff/marketplace/install", env.admin, env.adminCSRF,
		map[string]any{"name": "fakegateway"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("install with a mismatched digest: status %d, want 422; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "marketplace_digest_mismatch" {
		t.Fatalf("code = %q, want marketplace_digest_mismatch", code)
	}
	if artifactHits.Load() != 1 {
		t.Fatalf("the artifact was downloaded %d times, want 1", artifactHits.Load())
	}

	// Nothing is installed, and the screen says so.
	var installed int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM installed_extensions`).Scan(&installed); err != nil {
		t.Fatalf("count installed: %v", err)
	}
	if installed != 0 {
		t.Fatalf("%d extensions were recorded after a refused install", installed)
	}

	// The refusal is audited as a failure, because "someone tried to install an artifact whose
	// digest did not match" is exactly what an incident review needs.
	var outcome string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT outcome FROM audit_events WHERE action = 'marketplace.install' ORDER BY created_at DESC LIMIT 1`).
		Scan(&outcome); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if outcome != "failure" {
		t.Fatalf("the refused install was audited as %q, want failure", outcome)
	}
}

// The happy path at the HTTP layer: the digest matches, the extension is recorded as verified,
// and the artifact can be downloaded back.
func TestMarketplaceInstallVerifiesAndServesTheArtifact(t *testing.T) {
	env := newUpdatesEnv(t, "1.4.0")
	artifact := []byte("#!/bin/sh\necho verified\n")
	sum := sha256Of(artifact)
	artifactSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(artifact)
	}))
	t.Cleanup(artifactSrv.Close)
	index := `{"schema_version":1,"extensions":[{"name":"fakegateway","version":"1.2.0",
		"publisher":"Catalyst","description":"Pretend payments.","kind":"gateway",
		"download_url":"` + artifactSrv.URL + `/fakegateway-1.2.0",
		"sha256":"` + sum + `"}]}`
	indexSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(index))
	}))
	t.Cleanup(indexSrv.Close)

	rec := env.do(t, http.MethodPut, "/api/v1/staff/marketplace/settings", env.admin, env.adminCSRF,
		map[string]any{"index_url": indexSrv.URL})
	if rec.Code != http.StatusOK {
		t.Fatalf("configure: status %d, body %s", rec.Code, rec.Body)
	}

	rec = env.do(t, http.MethodPost, "/api/v1/staff/marketplace/install", env.admin, env.adminCSRF,
		map[string]any{"name": "fakegateway"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("install: status %d, want 201; body %s", rec.Code, rec.Body)
	}
	var result struct {
		Name       string `json:"name"`
		Version    string `json:"version"`
		SHA256     string `json:"sha256"`
		Status     string `json:"status"`
		Pinned     bool   `json:"pinned"`
		Activation string `json:"activation"`
		Trust      string `json:"trust"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.SHA256 != sum {
		t.Fatalf("digest = %q, want %q", result.SHA256, sum)
	}
	if result.Status != "verified" {
		// Never "running": the core cannot start an extension it was not configured to start.
		t.Fatalf("status = %q, want verified", result.Status)
	}
	if result.Pinned {
		t.Fatal("an unpinned install reported itself as pinned")
	}
	if !strings.Contains(result.Activation, "BILLING_GATEWAY_EXTENSION") {
		t.Fatalf("the activation instruction does not name the variable the deployment reads: %q", result.Activation)
	}
	if !strings.Contains(result.Trust, "does not prove the index") {
		t.Fatalf("the trust statement overstates what the digest proves: %q", result.Trust)
	}

	// The verified bytes come back byte for byte, with the digest in a header.
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/staff/marketplace/installed/fakegateway/1.2.0/artifact", nil)
	req.AddCookie(env.admin)
	artRec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(artRec, req)
	if artRec.Code != http.StatusOK {
		t.Fatalf("artifact: status %d, body %s", artRec.Code, artRec.Body)
	}
	if !bytes.Equal(artRec.Body.Bytes(), artifact) {
		t.Fatal("the served artifact differs from the verified bytes")
	}
	if got := artRec.Header().Get("X-Content-SHA256"); got != sum {
		t.Fatalf("artifact digest header = %q, want %q", got, sum)
	}
	if got := artRec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("content type = %q, want application/octet-stream", got)
	}

	// The install is audited with the digest that was verified.
	var changes []byte
	if err := env.pool.QueryRow(context.Background(),
		`SELECT changes FROM audit_events WHERE action = 'marketplace.install'
		 AND outcome = 'success' ORDER BY created_at DESC LIMIT 1`).Scan(&changes); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if !strings.Contains(string(changes), sum) {
		t.Fatalf("the audit row does not name the verified digest: %s", changes)
	}
}

// An install for a name that is not in the index is a 404, not an empty success.
func TestMarketplaceInstallUnknownName(t *testing.T) {
	env := newUpdatesEnv(t, "1.4.0")
	indexSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"schema_version":1,"extensions":[]}`))
	}))
	t.Cleanup(indexSrv.Close)

	rec := env.do(t, http.MethodPut, "/api/v1/staff/marketplace/settings", env.admin, env.adminCSRF,
		map[string]any{"index_url": indexSrv.URL})
	if rec.Code != http.StatusOK {
		t.Fatalf("configure: status %d, body %s", rec.Code, rec.Body)
	}
	rec = env.do(t, http.MethodPost, "/api/v1/staff/marketplace/install", env.admin, env.adminCSRF,
		map[string]any{"name": "nonexistent"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "marketplace_extension_unknown" {
		t.Fatalf("code = %q", code)
	}
}

// An artifact that is not installed is a 404 that never reaches the filesystem.
func TestMarketplaceArtifactNotFound(t *testing.T) {
	env := newUpdatesEnv(t, "1.4.0")
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/staff/marketplace/installed/not-installed/1.0.0/artifact", nil)
	req.AddCookie(env.admin)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "not_found" {
		t.Fatalf("code = %q", code)
	}
}

// The marketplace configuration refuses a URL that is not http(s), and an empty body is a
// client error rather than a silent no-op.
func TestMarketplaceSettingsValidation(t *testing.T) {
	env := newUpdatesEnv(t, "1.4.0")

	rec := env.do(t, http.MethodPut, "/api/v1/staff/marketplace/settings", env.admin, env.adminCSRF,
		map[string]any{"index_url": "ftp://example.test/index.json"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "invalid_url" {
		t.Fatalf("code = %q", code)
	}

	rec = env.do(t, http.MethodPut, "/api/v1/staff/marketplace/settings", env.admin, env.adminCSRF,
		map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty body: status %d, want 400; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "nothing_to_change" {
		t.Fatalf("code = %q", code)
	}
}

// A refresh with no index configured is refused with the stable "not configured" code.
func TestMarketplaceRefreshWithoutConfiguration(t *testing.T) {
	env := newUpdatesEnv(t, "1.4.0")
	rec := env.do(t, http.MethodPost, "/api/v1/staff/marketplace/refresh", env.admin, env.adminCSRF, map[string]any{})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body)
	}
	if code := problemCode(t, rec); code != "marketplace_not_configured" {
		t.Fatalf("code = %q", code)
	}
}

// sha256Of returns the lower-case hex digest of body.
func sha256Of(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
