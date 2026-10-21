package httpapi

// In-package contract tests.
//
// These are internal because they assert properties of the route table and of the
// cross-cutting wrappers, which are unexported by design. They are the tests that keep
// the surface uniform as it grows: permission coverage (F3), the error envelope (E4),
// idempotency (E3), the request budget (F2), streaming (E6) and static serving (G1/G2).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/approvals"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/catalyst/billing/web"
)

// testServer builds a fully wired server against the test database.
func testServer(t *testing.T) *Server {
	t.Helper()
	pool := storetest.DB(t)
	s := New(Config{Version: "test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		pool, payments.NewInbox(pool), "stripe", "acct_test", nil)
	svc, err := authn.New(pool, mustHasher(t), authn.DefaultConfig())
	if err != nil {
		t.Fatalf("authn: %v", err)
	}
	s.WithAuth(&Auth{Service: svc, Authz: authn.NewAuthorizer()})
	h, err := web.New(false)
	if err != nil {
		t.Fatalf("web: %v", err)
	}
	s.WithWeb(h)
	ap, err := approvals.New(pool)
	if err != nil {
		t.Fatalf("approvals: %v", err)
	}
	s.WithApprovals(ap).WithPortal(portal.New(pool))
	return s
}

func mustHasher(t *testing.T) *authn.Hasher {
	t.Helper()
	p := authn.DefaultArgon2Params()
	p.Memory = 19 * 1024
	p.Iterations = 1
	h, err := authn.NewHasher(p)
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	return h
}

// adminUser is a staff identity used to exercise guarded routes.
func adminUser() authn.User {
	return authn.User{
		ID:     "11111111-1111-1111-1111-111111111111",
		Email:  "admin@example.test",
		Status: "active",
		Roles:  []string{"administrator"},
	}
}

// withUser attaches an identity the way loadSession does.
func withUser(r *http.Request, u authn.User) *http.Request {
	ctx := context.WithValue(r.Context(), ctxUser, u)
	ctx = context.WithValue(ctx, ctxSession, authn.Session{UserID: u.ID, CSRFToken: "tok"})
	return r.WithContext(ctx)
}

// substitutePath fills path parameters with plausible values.
func substitutePath(pattern string) string {
	parts := strings.Split(pattern, "/")
	for i, p := range parts {
		if !strings.HasPrefix(p, "{") {
			continue
		}
		switch strings.Trim(p, "{}") {
		case "provider":
			parts[i] = "stripe"
		default:
			parts[i] = "0f8fad5b-d9cb-469f-a165-70867728950e"
		}
	}
	return strings.Join(parts, "/")
}

// ------------------------------------------------------------------ F3 --

// Every route under the staff or portal prefix must declare a permission. This is the
// mechanism that stops a future route shipping unguarded.
func TestPermissionCoverage(t *testing.T) {
	s := testServer(t)
	s.Handler()

	routes := s.Routes()
	if len(routes) == 0 {
		t.Fatal("the route table is empty")
	}

	seen := make(map[string]string, len(routes))
	for _, rt := range routes {
		if prev, dup := seen[rt.Path()]; dup {
			t.Fatalf("route %s is registered twice (%s and another)", rt.Path(), prev)
		}
		seen[rt.Path()] = rt.Method

		if rt.Access == AccessPermission && rt.Permission == "" {
			t.Fatalf("%s requires a permission but declares none", rt.Path())
		}
		if rt.Access != AccessPermission && rt.Permission != "" {
			t.Fatalf("%s declares a permission but does not require it", rt.Path())
		}
		// A staff-guarded route outside the /api/v1/staff prefix is a deliberate exemption, and the
		// set is pinned so a new one cannot appear without a decision. Both entries are pre-existing
		// role-based guards, not permission-based ones:
		//
		//   GET /metrics          — staff-only by role; support is allowed to read counters, which
		//                           TestMetricsRequiresStaff pins.
		//   GET /api/v1/ops/overview — staff-only *and* report:read (it is under /api/v1/, not
		//                           /api/v1/staff, so the prefix rule below does not see it).
		if rt.StaffOnly && rt.Permission == "" && !rt.DynamicPermission {
			switch rt.Path() {
			case "GET /metrics":
			default:
				t.Fatalf("%s is staff-only but declares no permission, and is not a pinned "+
					"exemption", rt.Path())
			}
		}
		if rt.DynamicPermission && !rt.StaffOnly {
			// A dynamic permission is resolved in the handler; if the route is not also
			// staff-only there is nothing stopping a customer from reaching it.
			t.Fatalf("%s resolves its permission dynamically but is not staff-only", rt.Path())
		}
		if !knownPermission(rt.Permission) && rt.Permission != "" {
			t.Fatalf("%s declares an undeclared permission %q", rt.Path(), rt.Permission)
		}
		if strings.HasPrefix(rt.Pattern, "/api/v1/staff") ||
			strings.HasPrefix(rt.Pattern, "/api/v1/portal") {
			if rt.Access != AccessPermission && !rt.DynamicPermission {
				t.Fatalf("%s is a customer/staff route without a permission check", rt.Path())
			}
			if rt.Permission == "" && !rt.DynamicPermission {
				t.Fatalf("%s is a customer/staff route without a declared permission", rt.Path())
			}
		}
		if strings.HasPrefix(rt.Pattern, "/api/v1/staff") && !rt.StaffOnly {
			t.Fatalf("%s is a staff route without the staff-only guard", rt.Path())
		}
		if rt.Method != http.MethodGet && rt.Method != http.MethodHead &&
			rt.Access != AccessPublic && !rt.CSRF {
			t.Fatalf("%s is a session mutation without CSRF protection", rt.Path())
		}
	}
}

// knownPermission reports whether p is a declared permission.
func knownPermission(p authn.Permission) bool {
	if p == "" {
		return true
	}
	for _, candidate := range authn.AllPermissions() {
		if candidate == p {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------ E4 --

// Every error path on every route must use the same envelope with a stable code. A UI
// needs one shape to render; a client branches on `code`.
func TestEveryErrorIsProblemJSON(t *testing.T) {
	s := testServer(t)
	handler := s.Handler()

	type problemBody struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
		Code   string `json:"code"`
	}

	check := func(t *testing.T, rec *httptest.ResponseRecorder, route string) {
		t.Helper()
		if rec.Code < 400 {
			return
		}
		ct := rec.Header().Get("Content-Type")
		if !strings.Contains(ct, "application/problem+json") {
			t.Fatalf("%s: status %d with Content-Type %q, want application/problem+json\n%s",
				route, rec.Code, ct, rec.Body)
		}
		var body problemBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: error body is not JSON: %v\n%s", route, err, rec.Body)
		}
		if body.Code == "" {
			t.Fatalf("%s: error body has no code: %s", route, rec.Body)
		}
		if body.Status != rec.Code {
			t.Fatalf("%s: body status %d != HTTP status %d", route, body.Status, rec.Code)
		}
		if body.Title == "" || body.Type == "" {
			t.Fatalf("%s: incomplete problem document: %s", route, rec.Body)
		}
	}

	for _, rt := range s.Routes() {
		path := substitutePath(rt.Pattern)
		route := rt.Path()

		// Anonymous pass: a guarded route must refuse with the envelope.
		var body io.Reader
		if rt.Method != http.MethodGet && rt.Method != http.MethodHead {
			body = strings.NewReader("{")
		}
		ctx := context.Background()
		if rt.Pattern == "/api/v1/events" {
			// The SSE handler holds the connection open until the context ends.
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			ctx = cancelled
		}
		req := httptest.NewRequest(rt.Method, path, body).WithContext(ctx)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		check(t, rec, route)

		// Authenticated pass with a deliberately invalid body: validation failures must
		// use the same envelope.
		if rt.Method == http.MethodGet || rt.Method == http.MethodHead {
			continue
		}
		req = httptest.NewRequest(rt.Method, path, strings.NewReader("{")).WithContext(ctx)
		req = withUser(req, adminUser())
		req.Header.Set("X-CSRF-Token", "tok")
		req.Header.Set("Idempotency-Key", "test-key-0001")
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		check(t, rec, route)
	}
}

// No route may answer with an HTML body: a client that expected JSON must never have to
// parse a page.
func TestNoRouteServesHTMLForAPI(t *testing.T) {
	s := testServer(t)
	handler := s.Handler()

	for _, path := range []string{"/api/v1/nope", "/api/v1/staff/nope", "/webhooks/nope"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "<html") || strings.Contains(rec.Body.String(), "<main") {
			t.Fatalf("%s returned HTML: %s", path, rec.Body)
		}
		var body struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code != "not_found" {
			t.Fatalf("%s: body = %s", path, rec.Body)
		}
	}
}

// ------------------------------------------------------------------ E3 --

// idempotentRoute builds a mutation with the idempotency wrapper, counting how often the
// handler actually ran.
func idempotentRoute(s *Server, runs *int, requireKey bool) http.HandlerFunc {
	rt := Route{
		Method: http.MethodPost, Pattern: "/api/v1/portal/services/{id}/cancel",
		Access: AccessUser, CSRF: true, Idempotent: true, IdempotencyRequired: requireKey,
	}
	return s.withIdempotency(rt, func(w http.ResponseWriter, r *http.Request) {
		*runs++
		writeJSON(w, http.StatusOK, map[string]any{"runs": *runs})
	})
}

func TestIdempotencyReplaysStoredResponse(t *testing.T) {
	s := testServer(t)
	runs := 0
	h := idempotentRoute(s, &runs, true)

	body := []byte(`{"reason":"moving house"}`)
	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/portal/services/x/cancel",
			bytes.NewReader(body))
		req = withUser(req, adminUser())
		req.Header.Set("Idempotency-Key", "cancel-0001")
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}

	first := do()
	if first.Code != http.StatusOK {
		t.Fatalf("first: status %d, body %s", first.Code, first.Body)
	}
	second := do()
	if second.Code != http.StatusOK {
		t.Fatalf("replay: status %d, body %s", second.Code, second.Body)
	}
	if runs != 1 {
		t.Fatalf("handler ran %d times, want 1", runs)
	}
	// The stored body is jsonb, so the replay is semantically identical rather than
	// byte-identical: PostgreSQL normalises whitespace. A client parses it either way.
	var firstBody, secondBody map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatalf("original body: %v", err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondBody); err != nil {
		t.Fatalf("replay body: %v", err)
	}
	if firstBody["runs"] != secondBody["runs"] {
		t.Fatalf("replay body %v != original %v", secondBody, firstBody)
	}
	if second.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("the replay was not marked as a replay")
	}
}

func TestIdempotencyRejectsDifferentBody(t *testing.T) {
	s := testServer(t)
	runs := 0
	h := idempotentRoute(s, &runs, true)

	send := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/portal/services/x/cancel",
			strings.NewReader(body))
		req = withUser(req, adminUser())
		req.Header.Set("Idempotency-Key", "cancel-0002")
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}

	if rec := send(`{"reason":"first"}`); rec.Code != http.StatusOK {
		t.Fatalf("first: %d %s", rec.Code, rec.Body)
	}
	rec := send(`{"reason":"second"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("reuse with a different body: status %d, want 422; body %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "idempotency_key_reuse") {
		t.Fatalf("body = %s", rec.Body)
	}
	if runs != 1 {
		t.Fatalf("handler ran %d times, want 1", runs)
	}
}

func TestIdempotencyIsPerPrincipal(t *testing.T) {
	s := testServer(t)
	runs := 0
	h := idempotentRoute(s, &runs, true)

	send := func(user authn.User) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/portal/services/x/cancel",
			strings.NewReader(`{}`))
		req = withUser(req, user)
		req.Header.Set("Idempotency-Key", "shared-key-1")
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}

	other := adminUser()
	other.ID = "22222222-2222-2222-2222-222222222222"
	if rec := send(adminUser()); rec.Code != http.StatusOK {
		t.Fatalf("first principal: %d %s", rec.Code, rec.Body)
	}
	if rec := send(other); rec.Code != http.StatusOK {
		t.Fatalf("second principal: %d %s", rec.Code, rec.Body)
	}
	if runs != 2 {
		t.Fatalf("handler ran %d times, want 2 (keys are per principal)", runs)
	}
}

// A money action without a key is refused, because a retry without one is a second
// refund.
func TestIdempotencyKeyRequired(t *testing.T) {
	s := testServer(t)
	runs := 0
	h := idempotentRoute(s, &runs, true)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/portal/services/x/cancel",
		strings.NewReader(`{}`))
	req = withUser(req, adminUser())
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "idempotency_key_required") {
		t.Fatalf("body = %s", rec.Body)
	}
	if runs != 0 {
		t.Fatal("the handler ran without an idempotency key")
	}
}

// A failing handler must not have its failure stored: a retry has to be able to try
// again.
func TestIdempotencyDoesNotStoreServerErrors(t *testing.T) {
	s := testServer(t)
	rt := Route{Method: http.MethodPost, Pattern: "/api/v1/test/fail",
		Access: AccessUser, CSRF: true, Idempotent: true}
	runs := 0
	h := s.withIdempotency(rt, func(w http.ResponseWriter, r *http.Request) {
		runs++
		writeProblem(w, http.StatusServiceUnavailable, "transient", "try again")
	})

	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/test/fail", strings.NewReader(`{}`))
		req = withUser(req, adminUser())
		req.Header.Set("Idempotency-Key", "fail-key-1")
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}

	if rec := send(); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("first: %d", rec.Code)
	}
	if rec := send(); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("retry: %d", rec.Code)
	}
	if runs != 2 {
		t.Fatalf("handler ran %d times, want 2: a 5xx must not be replayed", runs)
	}
}

// ------------------------------------------------------------------ F2 --

func TestRateLimitReturns429WithRetryAfter(t *testing.T) {
	s := testServer(t)
	s.WithRateLimits(map[RateClass]int{RateRead: 2})

	rt := Route{Method: http.MethodGet, Pattern: "/api/v1/test/read", Access: AccessUser,
		Rate: RateRead}
	h := s.withRateLimit(rt, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	send := func(user authn.User) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/test/read", nil)
		req = withUser(req, user)
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}

	for i := 0; i < 2; i++ {
		if rec := send(adminUser()); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status %d", i, rec.Code)
		}
	}
	rec := send(adminUser())
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third request: status %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
	if !strings.Contains(rec.Body.String(), "rate_limited") {
		t.Fatalf("body = %s", rec.Body)
	}

	// A different principal has its own bucket.
	other := adminUser()
	other.ID = "33333333-3333-3333-3333-333333333333"
	if rec := send(other); rec.Code != http.StatusOK {
		t.Fatalf("other principal: status %d, want 200", rec.Code)
	}
}

func TestRateLimitWindowResets(t *testing.T) {
	s := testServer(t)
	lim := s.rateLimiter
	base := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	now := base
	lim.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	lim.limits[RateExport] = 1

	if ok, _, _ := lim.allow("p1", RateExport); !ok {
		t.Fatal("the first request was refused")
	}
	if ok, _, _ := lim.allow("p1", RateExport); ok {
		t.Fatal("the second request in the window was allowed")
	}

	mu.Lock()
	now = base.Add(2 * time.Minute)
	mu.Unlock()
	if ok, _, _ := lim.allow("p1", RateExport); !ok {
		t.Fatal("the window did not reset")
	}
}

func TestRateLimiterSweepDropsExpiredWindows(t *testing.T) {
	lim := newRateLimiter()
	base := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	now := base
	lim.now = func() time.Time { return now }
	lim.allow("p1", RateRead)
	lim.allow("p2", RateRead)

	now = base.Add(5 * time.Minute)
	lim.sweep()
	lim.mu.Lock()
	n := len(lim.windows)
	lim.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d windows survived the sweep", n)
	}
}

// ------------------------------------------------------------------ E6 --

// sseRecorder is a concurrency-safe ResponseWriter for a streaming handler.
type sseRecorder struct {
	mu      sync.Mutex
	header  http.Header
	status  int
	body    bytes.Buffer
	flushed chan struct{}
}

func newSSERecorder() *sseRecorder {
	return &sseRecorder{header: make(http.Header), flushed: make(chan struct{}, 64)}
}

func (r *sseRecorder) Header() http.Header { return r.header }

func (r *sseRecorder) WriteHeader(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status == 0 {
		r.status = status
	}
}

func (r *sseRecorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(b)
}

func (r *sseRecorder) Flush() {
	select {
	case r.flushed <- struct{}{}:
	default:
	}
}

func (r *sseRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}

// waitFor polls the recorder until the body contains want.
func (r *sseRecorder) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(r.String(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("stream never contained %q; got:\n%s", want, r.String())
}

func startStream(t *testing.T, s *Server, u authn.User, lastEventID string) (*sseRecorder, func()) {
	t.Helper()
	rec := newSSERecorder()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	req = withUser(req, u)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	go s.handleEvents(rec, req)
	// Wait for the opening frame so the subscription exists before anything is published.
	rec.waitFor(t, ": connected")
	return rec, cancel
}

func TestSSEDeliversStaffEventAfterPublish(t *testing.T) {
	s := testServer(t)
	rec, cancel := startStream(t, s, adminUser(), "")
	defer cancel()

	// Nothing is delivered until something is published: the feed reports committed
	// changes, it does not invent them.
	if strings.Contains(rec.String(), "invoice.paid") {
		t.Fatal("an event appeared before anything was published")
	}

	s.publish(events.Event{
		Kind: "invoice.paid", Permission: string(authn.PermInvoiceReadAll),
		Data: map[string]any{"invoice_id": "inv_1"},
	})
	rec.waitFor(t, "event: invoice.paid")
	if !strings.Contains(rec.String(), `"invoice_id":"inv_1"`) {
		t.Fatalf("payload missing: %s", rec.String())
	}
	if !strings.Contains(rec.String(), "id: 1") {
		t.Fatalf("event has no id: %s", rec.String())
	}
}

// A staff member must not receive an event whose permission they do not hold.
func TestSSEFiltersStaffByPermission(t *testing.T) {
	s := testServer(t)
	support := authn.User{ID: "44444444-4444-4444-4444-444444444444",
		Email: "support@example.test", Status: "active", Roles: []string{"support"}}

	rec, cancel := startStream(t, s, support, "")
	defer cancel()

	s.publish(events.Event{Kind: "refund.created", Permission: string(authn.PermPaymentRefund)})
	s.publish(events.Event{Kind: "invoice.issued", Permission: string(authn.PermInvoiceReadAll)})
	rec.waitFor(t, "event: invoice.issued")

	if strings.Contains(rec.String(), "refund.created") {
		t.Fatalf("support received a refund event: %s", rec.String())
	}
}

// A customer only ever sees their own events, even when they are staff-permission-free.
func TestSSECustomerIsolation(t *testing.T) {
	s := testServer(t)
	pool := s.pool
	ctx := context.Background()

	newCustomer := func(email string) (customerID, userID string) {
		if err := pool.QueryRow(ctx,
			`INSERT INTO customers (email) VALUES ($1) RETURNING id`, email).Scan(&customerID); err != nil {
			t.Fatalf("customer: %v", err)
		}
		if err := pool.QueryRow(ctx, `
			INSERT INTO users (email, password_hash, email_verified_at)
			VALUES ($1, 'x', now()) RETURNING id`, email).Scan(&userID); err != nil {
			t.Fatalf("user: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO customer_accounts (user_id, customer_id) VALUES ($1, $2)`,
			userID, customerID); err != nil {
			t.Fatalf("account: %v", err)
		}
		return customerID, userID
	}

	customerA, userA := newCustomer("a@example.test")
	customerB, _ := newCustomer("b@example.test")

	rec, cancel := startStream(t, s, authn.User{
		ID: userA, Email: "a@example.test", Status: "active",
	}, "")
	defer cancel()

	s.publish(events.Event{Kind: "invoice.paid", CustomerID: customerB,
		Data: map[string]any{"invoice_id": "b_invoice"}})
	s.publish(events.Event{Kind: "service.updated", CustomerID: customerA,
		Data: map[string]any{"service_id": "a_service"}})

	rec.waitFor(t, "event: service.updated")
	if strings.Contains(rec.String(), "b_invoice") {
		t.Fatalf("customer A received customer B's event: %s", rec.String())
	}
}

// Reconnecting with Last-Event-ID resumes after that id and replays nothing older.
func TestSSEResumesAfterLastEventID(t *testing.T) {
	s := testServer(t)
	s.publish(events.Event{Kind: "old.event", Permission: string(authn.PermInvoiceReadAll)})
	s.publish(events.Event{Kind: "old.event", Permission: string(authn.PermInvoiceReadAll)})

	rec, cancel := startStream(t, s, adminUser(), "2")
	defer cancel()

	// Publish an event that is newer than the resume point.
	s.publish(events.Event{Kind: "new.event", Permission: string(authn.PermInvoiceReadAll)})
	rec.waitFor(t, "event: new.event")

	if strings.Contains(rec.String(), "event: old.event") {
		t.Fatalf("an event at or before Last-Event-ID was replayed: %s", rec.String())
	}
}

// Without a broker the endpoint says so instead of holding a dead connection open.
func TestSSEUnavailableWithoutBroker(t *testing.T) {
	s := testServer(t)
	s.events = nil
	rec := httptest.NewRecorder()
	req := withUser(httptest.NewRequest(http.MethodGet, "/api/v1/events", nil), adminUser())
	s.handleEvents(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// ------------------------------------------------------------------ G1 --

// The SPA is served by the real mux, and an API path stays JSON.
func TestStaticServingThroughMux(t *testing.T) {
	s := testServer(t)
	handler := s.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	// The assertion is "an HTML document was served", not "a particular element exists": pinning a
	// tag makes this test fail on a frontend change it has no opinion about, and the contract under
	// test is the routing, not the markup.
	root := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.HasPrefix(strings.TrimSpace(root), "<!DOCTYPE html") {
		t.Fatalf("root: %d %.200s", rec.Code, root)
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("the shell was served without a CSP")
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil))
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "<!DOCTYPE html") {
		t.Fatalf("unknown API path: %d %s", rec.Code, rec.Body)
	}
}

// The request-log wrapper must not hide http.Flusher.
//
// This is a regression test for a defect found by driving the real UI: the SSE handler
// refuses to stream when its ResponseWriter is not a Flusher ("this server cannot stream
// responses"), and statusRecorder — the wrapper withRequestLog installs — implemented only
// Header/Write/WriteHeader. Every unit test called handleEvents directly, so the chain was
// never exercised and the live feed was a 500 in a browser while the suite stayed green.
//
// The assertion is made through the same middleware stack the server builds, not against
// the wrapper alone, because the defect was in the composition.
func TestMiddlewareChainPreservesFlusher(t *testing.T) {
	s := testServer(t)

	var sawFlusher bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		sawFlusher = true
		flusher.Flush()
	})

	handler := s.withRecovery(s.withRequestLog(s.withCorrelation(inner)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))

	if !sawFlusher {
		t.Fatal("http.Flusher is not visible through the middleware chain; SSE cannot stream")
	}
	if !rec.Flushed {
		t.Fatal("Flush did not reach the underlying ResponseWriter")
	}
}

// An anonymous caller must not be able to open the event stream.
//
// The route declares AccessUser. Before the router applied that declaration, the handler
// streamed to anyone: the filter delivered nothing, so it was not a data leak, but it was an
// unauthenticated long-lived connection per client and a route that did not do what its
// declaration said.
func TestEventsRouteRequiresAuthentication(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Get(srv.URL + "/api/v1/events")
	if err != nil {
		t.Fatalf("GET /api/v1/events: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 256))
		t.Fatalf("status = %d, want 401 for an unauthenticated stream: %s", res.StatusCode, body)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Fatalf("Content-Type = %q, want problem+json", ct)
	}
}
