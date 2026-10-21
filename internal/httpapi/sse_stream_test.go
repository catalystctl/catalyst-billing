package httpapi_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Streaming through the full middleware chain.
//
// The in-package SSE tests call handleEvents directly, which is the right way to test the filtering
// and resume logic — but it skips every wrapper. That hid a real defect: the request logger wraps
// the ResponseWriter in a statusRecorder, and a wrapper that does not forward Flush hides
// http.Flusher. The handler's type assertion then correctly refuses to stream, and the endpoint
// answers 500 to every client while the direct-call test stays green.
//
// These tests therefore drive Handler() — the whole chain — with a ResponseWriter that implements
// http.Flusher, and assert that frames are actually delivered. Reverting the Flush forwarding makes
// them fail.

// flushRecorder is a ResponseWriter that implements http.Flusher and records what was written.
type flushRecorder struct {
	mu     sync.Mutex
	header http.Header
	status int
	body   bytes.Buffer
	// flushes counts Flush calls, which is what proves the wrapper forwarded them.
	flushes int
	flushed chan struct{}
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{header: make(http.Header), flushed: make(chan struct{}, 64)}
}

func (r *flushRecorder) Header() http.Header { return r.header }

func (r *flushRecorder) WriteHeader(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status == 0 {
		r.status = status
	}
}

func (r *flushRecorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(b)
}

func (r *flushRecorder) Flush() {
	r.mu.Lock()
	r.flushes++
	r.mu.Unlock()
	select {
	case r.flushed <- struct{}{}:
	default:
	}
}

func (r *flushRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}

func (r *flushRecorder) flushCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flushes
}

// waitFor polls until the recorded body contains want.
func (r *flushRecorder) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(r.String(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the stream never delivered %q; received %q", want, r.String())
}

// openStream drives the full handler chain for the events endpoint.
func openStream(t *testing.T, env *staffEnv, cookie *http.Cookie) (*flushRecorder, func()) {
	t.Helper()
	rec := newFlushRecorder()
	ctx, cancel := context.WithCancel(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	if cookie != nil {
		req.AddCookie(cookie)
	}

	go env.srv.Handler().ServeHTTP(rec, req)

	// Wait for the opening frame, which proves the handler got past its Flusher assertion and the
	// wrapper chain forwarded at least one Flush.
	rec.waitFor(t, ": connected")
	return rec, cancel
}

// A staff subscriber receives an event through the whole chain.
func TestSSEThroughFullChainDeliversFrames(t *testing.T) {
	env := newStaffEnv(t)
	rec, cancel := openStream(t, env, env.admin)
	defer cancel()

	if got := rec.flushCount(); got == 0 {
		t.Fatal("the response was never flushed: a wrapper is hiding http.Flusher")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no (a proxy must not buffer the stream)", got)
	}
}

// A customer-scoped stream opens through the same chain: the customer is the caller most likely to
// sit behind a buffering proxy.
func TestSSEThroughFullChainForACustomer(t *testing.T) {
	env := newStaffEnv(t)
	env.seedCustomer(t, "sse-customer@example.test", "SSE Customer")
	cookie, _ := env.login(t, "sse-customer@example.test", "customer password 1")

	rec, cancel := openStream(t, env, cookie)
	defer cancel()

	if rec.flushCount() == 0 {
		t.Fatal("a customer stream was never flushed")
	}
}

// An anonymous caller is refused before the stream opens, and the refusal is a JSON problem rather
// than a half-open stream.
func TestSSEThroughFullChainRefusesAnonymous(t *testing.T) {
	env := newStaffEnv(t)
	rec := httptest.NewRecorder()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	env.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "problem+json") {
		t.Fatalf("Content-Type = %q, want problem+json", ct)
	}
	if strings.Contains(rec.Body.String(), ": connected") {
		t.Fatal("an anonymous caller received a stream")
	}
}
