package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/catalyst/billing/web"
)

func newHandler(t *testing.T, secure bool) *web.Handler {
	t.Helper()
	h, err := web.New(secure)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return h
}

func get(t *testing.T, h *web.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// The shell is served at the root and is never cached: a cached index.html is how a
// deploy leaves every returning user on the previous bundle until they hard-refresh.
func TestIndexIsNotCached(t *testing.T) {
	h := newHandler(t, false)
	rec := get(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
}

// A client-side route must serve the shell so a reload works.
func TestUnknownPathFallsBackToShell(t *testing.T) {
	h := newHandler(t, false)
	rec := get(t, h, "/customers/abc/invoices")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<main") {
		t.Fatalf("fallback did not serve the shell: %s", rec.Body.String())
	}
}

// A hashed asset is immutable; an unhashed one is not.
func TestAssetCachePolicy(t *testing.T) {
	h := newHandler(t, false)

	rec := get(t, h, "/assets/app.4f2c1d9e.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Fatalf("hashed asset Cache-Control = %q, want immutable", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("Content-Type = %q, want javascript", ct)
	}

	// The same file reached through a normalised path is the same file.
	rec = get(t, h, "/assets/./app.4f2c1d9e.js")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("normalised path served differently: %d %q", rec.Code,
			rec.Header().Get("Cache-Control"))
	}
}

// Traversal cannot escape the embedded tree.
func TestTraversalDoesNotEscape(t *testing.T) {
	h := newHandler(t, false)
	for _, path := range []string{
		"/../../etc/passwd",
		"/assets/../../web.go",
		"/assets/%2e%2e/%2e%2e/web.go",
	} {
		rec := get(t, h, path)
		if strings.Contains(rec.Body.String(), "package web") {
			t.Fatalf("%s served a source file", path)
		}
		if rec.Code != http.StatusOK {
			// Either the shell or a 404 is acceptable; leaking a file is not.
			continue
		}
		if !strings.Contains(rec.Body.String(), "<main") {
			t.Fatalf("%s served something other than the shell: %s", path, rec.Body.String())
		}
	}
}

// The documented CSP must be present, and must not contain an exemption that would let
// an injected script run.
func TestSecurityHeaders(t *testing.T) {
	h := newHandler(t, false)
	rec := get(t, h, "/")

	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{
		"default-src 'self'",
		"script-src 'self'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		"object-src 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP missing %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Fatalf("CSP contains an unsafe exemption: %s", csp)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "same-origin" {
		t.Fatalf("Referrer-Policy = %q", got)
	}
	// HSTS must not be sent from a plain-HTTP server, or the browser refuses to reach it
	// again.
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("HSTS sent without TLS: %q", got)
	}
}

func TestHSTSOnlyWhenSecure(t *testing.T) {
	h := newHandler(t, true)
	rec := get(t, h, "/")
	if got := rec.Header().Get("Strict-Transport-Security"); !strings.Contains(got, "max-age=31536000") {
		t.Fatalf("HSTS = %q, want the documented policy", got)
	}
}

// The API prefix is recognised so the API layer can answer JSON instead of the shell.
func TestIsAPIPath(t *testing.T) {
	h := newHandler(t, false)
	for _, p := range []string{"/api/v1/auth/me", "/api/unknown", "/webhooks/stripe", "/healthz"} {
		if !h.IsAPIPath(p) {
			t.Fatalf("IsAPIPath(%q) = false", p)
		}
	}
	for _, p := range []string{"/", "/customers", "/assets/app.4f2c1d9e.js"} {
		if h.IsAPIPath(p) {
			t.Fatalf("IsAPIPath(%q) = true", p)
		}
	}
}

// Only GET and HEAD are served: a static handler that accepts POST is a static handler
// that can be used to write.
func TestNonReadMethodsRejected(t *testing.T) {
	h := newHandler(t, false)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s /: status %d, want 405", method, rec.Code)
		}
	}
}
