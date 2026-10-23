// The writers in logs.go are actually called.
//
// This is the regression test for a silent defect: WriteError and WriteHTTP were correct,
// exported and had no caller, so /logs offered an error tab and an HTTP tab that could never
// show a row. An operator opening the screen to find out why a customer saw a 500 found an
// empty table and concluded nothing was wrong, which is worse than an error page.
//
// Every assertion below goes through the handler the server really builds. A writer called from
// a handler the router does not reach would be the same bug with more code behind it.

package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5/pgxpool"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// observabilityServer is a server with a real database and no authentication, so a request
// reaches the handler without a session and the row is attributable to nothing but the request.
func observabilityServer(t *testing.T) (*Server, *store.Pool) {
	t.Helper()
	pool := storetest.DB(t)
	s := New(Config{Version: "test", Logger: quietLogger()},
		pool, payments.NewInbox(pool), "stripe", "acct_test", nil)
	return s, pool
}

// A request that goes through the real chain leaves a row: method, path, status and duration,
// which is the whole point of the HTTP tab.
func TestHTTPLogRecordsServedRequests(t *testing.T) {
	s, pool := observabilityServer(t)
	ctx := context.Background()

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz: status %d; body %s", rec.Code, rec.Body)
	}

	var (
		method, path string
		status       int
		durationMS   int
		anonymous    bool
	)
	if err := pool.QueryRow(ctx, `
		SELECT method, path, status, duration_ms, actor_id IS NULL
		FROM http_logs WHERE path = '/healthz' ORDER BY id DESC LIMIT 1`).
		Scan(&method, &path, &status, &durationMS, &anonymous); err != nil {
		t.Fatalf("no http_logs row for a served request: %v", err)
	}
	if method != http.MethodGet || path != "/healthz" || status != http.StatusOK {
		t.Fatalf("row = %s %s %d, want GET /healthz 200", method, path, status)
	}
	if durationMS < 0 {
		t.Fatalf("duration_ms = %d", durationMS)
	}
	// An anonymous request has no actor, and the column must say so rather than carrying a
	// placeholder that an operator would read as a user id.
	if !anonymous {
		t.Fatal("an unauthenticated request was recorded with an actor")
	}

	// A request the mux refuses still reached this server, so it is still a row. This is the
	// half that makes the tab useful when the answer is "the URL was wrong".
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil))
	if err := pool.QueryRow(ctx, `
		SELECT status FROM http_logs WHERE path = '/api/v1/nope' ORDER BY id DESC LIMIT 1`).
		Scan(&status); err != nil {
		t.Fatalf("no row for a 404: %v", err)
	}
	if status != http.StatusNotFound {
		t.Fatalf("404 row recorded status %d", status)
	}
}

// A 5xx is recorded as an error, with the problem code, from the one place the problem is
// rendered — no handler opts in.
func TestErrorLogRecordsServerErrors(t *testing.T) {
	s, pool := observabilityServer(t)
	ctx := context.Background()

	// No authentication is configured, so login answers 503. That is the shape of failure an
	// operator opens the error tab for: a working route refusing for a reason on this side.
	body := `{"email":"someone@example.test","password":"` + secretBodyPassword + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://example.test")
	req.Host = "example.test"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("login: status %d; body %s", rec.Code, rec.Body)
	}

	var (
		level, source, message, detail string
	)
	if err := pool.QueryRow(ctx, `
		SELECT level, source, message, detail FROM error_logs
		WHERE message = 'auth_not_configured' ORDER BY id DESC LIMIT 1`).
		Scan(&level, &source, &message, &detail); err != nil {
		t.Fatalf("no error_logs row for a 503: %v", err)
	}
	if level != "error" {
		t.Fatalf("level = %q, want error for a 5xx", level)
	}
	if source != "http.request" {
		t.Fatalf("source = %q", source)
	}
	// The row has to stand on its own: an operator who found it by searching the code should not
	// have to guess which endpoint returned it.
	if !strings.Contains(detail, "POST /api/v1/auth/login") || !strings.Contains(detail, "503") {
		t.Fatalf("detail = %q, want the method, path and status", detail)
	}
	// The HTTP tab records the same request, so the two views agree about what happened.
	var status int
	if err := pool.QueryRow(ctx, `
		SELECT status FROM http_logs WHERE path = '/api/v1/auth/login' ORDER BY id DESC LIMIT 1`).
		Scan(&status); err != nil {
		t.Fatalf("no http_logs row for the 503: %v", err)
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("http row status = %d, want 503", status)
	}
}

// A 4xx is recorded as a warning when it says the caller's request was understood and rejected,
// and not at all when it is the surface working as intended.
func TestErrorLogRecordsRejectedRequestsButNotNoise(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	s := New(Config{Version: "test", Logger: quietLogger()}, pool, payments.NewInbox(pool),
		"stripe", "acct_test", payments.HMACSigner{
			Secret:          []byte("whsec_test"),
			Header:          "Stripe-Signature",
			TimestampHeader: "Stripe-Timestamp",
			Tolerance:       time.Minute,
		})

	// An unsigned delivery is a client that is broken in a way an operator can act on.
	req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", strings.NewReader(`{"id":"evt_1"}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unsigned webhook: status %d; body %s", rec.Code, rec.Body)
	}

	// An authentication refusal is the guard doing its job, on a route every anonymous scanner
	// can reach. Recording it would bury the row that matters.
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/staff/logs/errors", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous staff read: status %d; body %s", rec.Code, rec.Body)
	}

	var level string
	if err := pool.QueryRow(ctx, `
		SELECT level FROM error_logs WHERE message = 'invalid_signature' ORDER BY id DESC LIMIT 1`).
		Scan(&level); err != nil {
		t.Fatalf("no error_logs row for a rejected request: %v", err)
	}
	if level != "warn" {
		t.Fatalf("level = %q, want warn for a 4xx", level)
	}
	var noise int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM error_logs
		WHERE message IN ('authentication_required', 'forbidden')`).Scan(&noise); err != nil {
		t.Fatalf("count: %v", err)
	}
	if noise != 0 {
		t.Fatalf("%d authentication or permission refusals were recorded as errors", noise)
	}
	// It is still in the HTTP log: "who was refused" is a different question from "what failed",
	// and the request was served.
	var status int
	if err := pool.QueryRow(ctx, `
		SELECT status FROM http_logs WHERE path = '/api/v1/staff/logs/errors' ORDER BY id DESC LIMIT 1`).
		Scan(&status); err != nil {
		t.Fatalf("no http_logs row for the 401: %v", err)
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("http row status = %d, want 401", status)
	}
}

// A logging failure is a lost row, never a failed request.
//
// The write happens on the request goroutine, so this is the property that matters: the database
// is unreachable, every row is lost, and the response is exactly the one the handler produced.
func TestLogWriteFailureDoesNotChangeTheResponse(t *testing.T) {
	ctx := context.Background()

	// A pool that cannot connect. Every row the writer attempts fails.
	brokenPool, err := pgxpool.New(ctx,
		"postgres://billing:devonly@127.0.0.1:1/billing?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("build broken pool: %v", err)
	}
	t.Cleanup(brokenPool.Close)

	s := New(Config{Version: "test", Logger: quietLogger()}, nil, nil, "", "", nil)
	s.pool = &store.Pool{Pool: brokenPool}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a failed log write changed the response", rec.Code)
	}
	if got := s.logWriteFailures.Load(); got == 0 {
		t.Fatal("the writer never attempted a row, so this proves nothing")
	}

	// The same for a 5xx: the handler's own status survives the failed error-log write, and the
	// problem body is still the problem body.
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the handler's 503", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "problem+json") {
		t.Fatalf("Content-Type = %q: the response was damaged by the failed log write", ct)
	}
	if got := s.logWriteFailures.Load(); got < 2 {
		t.Fatalf("logWriteFailures = %d, want both rows attempted", got)
	}
}

const (
	secretQueryToken   = "reset-token-9f3c1d"
	secretCookie       = "session-cookie-77bd"
	secretBearer       = "sk_live_bearer_51aa"
	secretBodyPassword = "hunter2-body-4c1e"
)

// Nothing that is a credential reaches a row.
//
// The table has no header column and no body column, and the path loses its query string; this
// test is what keeps that true, because the natural way to make the log "more useful" is to add
// exactly one of those things.
func TestLogsNeverStoreSecrets(t *testing.T) {
	s, pool := observabilityServer(t)
	ctx := context.Background()

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/announcements?token="+secretQueryToken+"&password="+secretBodyPassword, nil)
	req.AddCookie(&http.Cookie{Name: "billing_session", Value: secretCookie})
	req.Header.Set("Authorization", "Bearer "+secretBearer)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("announcements: status %d; body %s", rec.Code, rec.Body)
	}

	// A credential in a request body: the login endpoint reads a password, and the log must not.
	body := `{"email":"someone@example.test","password":"` + secretBodyPassword + `"}`
	login := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
	login.Header.Set("Content-Type", "application/json")
	login.Host = "example.test"
	login.Header.Set("Origin", "http://example.test")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, login)

	// A rendered problem whose detail carries a credential. The redaction rule the audit log
	// already applies is applied by the writer, not by the handler.
	recordProblem(&statusRecorder{
		ResponseWriter: httptest.NewRecorder(),
		method:         http.MethodPost,
		path:           "/api/v1/example",
		srv:            s,
	}, http.StatusInternalServerError, "internal_error", "Authorization: Bearer "+secretBearer)

	var path string
	if err := pool.QueryRow(ctx, `
		SELECT path FROM http_logs WHERE path LIKE '/api/v1/announcements%' ORDER BY id DESC LIMIT 1`).
		Scan(&path); err != nil {
		t.Fatalf("no http_logs row: %v", err)
	}
	if path != "/api/v1/announcements" {
		t.Fatalf("path = %q, want the query string stripped", path)
	}

	var detail string
	if err := pool.QueryRow(ctx, `
		SELECT detail FROM error_logs WHERE message = 'internal_error' ORDER BY id DESC LIMIT 1`).
		Scan(&detail); err != nil {
		t.Fatalf("no error_logs row: %v", err)
	}
	if !strings.Contains(detail, "[redacted]") {
		t.Fatalf("the credential was not replaced by the marker: %q", detail)
	}

	for _, secret := range []string{secretQueryToken, secretCookie, secretBearer, secretBodyPassword} {
		var hits int
		if err := pool.QueryRow(ctx, `
			SELECT
				(SELECT count(*) FROM http_logs
				 WHERE method || ' ' || path || ' ' || COALESCE(actor_id::text, '') || ' ' ||
				       COALESCE(host(ip), '') ILIKE '%' || $1 || '%')
				+ (SELECT count(*) FROM error_logs
				   WHERE level || ' ' || source || ' ' || message || ' ' || detail
				         ILIKE '%' || $1 || '%')`, secret).Scan(&hits); err != nil {
			t.Fatalf("scan for %q: %v", secret, err)
		}
		if hits != 0 {
			t.Fatalf("a credential reached %d log rows: %q", hits, secret)
		}
	}
}

// A problem rendered behind the idempotency wrapper is still recorded.
//
// withIdempotency substitutes its own ResponseWriter, so the observer the handler is holding is
// not the one withRequestLog installed. That is the one seam where this log could go dark again
// without a compile error, and the walk in problemRecorder is what closes it.
func TestErrorLogReachesThroughTheIdempotencyWrapper(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()

	req := withUser(httptest.NewRequest(http.MethodPost, "/api/v1/staff/customers",
		strings.NewReader("")), adminUser())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", "tok")
	req.Header.Set("Idempotency-Key", "observability-idempotent-problem")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}

	var message, detail string
	if err := s.pool.QueryRow(ctx, `
		SELECT message, detail FROM error_logs ORDER BY id DESC LIMIT 1`).
		Scan(&message, &detail); err != nil {
		t.Fatalf("no error_logs row through the idempotency wrapper: %v", err)
	}
	if message != "invalid_body" {
		t.Fatalf("message = %q, want the rendered problem code", message)
	}
	if !strings.Contains(detail, "POST /api/v1/staff/customers") {
		t.Fatalf("detail = %q, want the request the row describes", detail)
	}
}

// A panic is still a row.
//
// withRequestLog sits outside withRecovery for this reason: the request that crashed a handler is
// the first one an operator opens /logs to find, and it would otherwise be the one request the log
// never mentions.
func TestPanicIsRecorded(t *testing.T) {
	s, pool := observabilityServer(t)
	ctx := context.Background()

	handler := s.withRequestLog(s.withRecovery(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) { panic("boom") })))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/panics", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	var status int
	if err := pool.QueryRow(ctx, `
		SELECT status FROM http_logs WHERE path = '/api/v1/panics' ORDER BY id DESC LIMIT 1`).
		Scan(&status); err != nil {
		t.Fatalf("no http_logs row for a panic: %v", err)
	}
	if status != http.StatusInternalServerError {
		t.Fatalf("http row status = %d, want 500", status)
	}
	var level, message string
	if err := pool.QueryRow(ctx, `
		SELECT level, message FROM error_logs WHERE message = 'internal_error'
		ORDER BY id DESC LIMIT 1`).Scan(&level, &message); err != nil {
		t.Fatalf("no error_logs row for a panic: %v", err)
	}
	if level != "error" {
		t.Fatalf("level = %q", level)
	}
}
