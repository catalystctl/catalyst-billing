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

// assetFromIndex reads the hashed bundle name out of the served shell, so the test does not
// have to be edited every time the bundle changes. A test that pins a content hash is a test
// that fails on an unrelated copy edit and teaches the team to update it without reading it.
func assetFromIndex(t *testing.T, h *web.Handler) string {
	t.Helper()
	index := string(h.Index())
	for _, marker := range []string{`src="`, `href="`} {
		rest := index
		for {
			i := strings.Index(rest, marker)
			if i < 0 {
				break
			}
			rest = rest[i+len(marker):]
			j := strings.Index(rest, `"`)
			if j < 0 {
				break
			}
			candidate := rest[:j]
			if strings.HasPrefix(candidate, "/assets/") && strings.HasSuffix(candidate, ".js") {
				return candidate
			}
		}
	}
	t.Fatalf("index.html references no /assets/*.js bundle:\n%s", index)
	return ""
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
	if !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("fallback did not serve the shell: %s", rec.Body.String())
	}
}

// A hashed asset is immutable; an unhashed one is not.
func TestAssetCachePolicy(t *testing.T) {
	h := newHandler(t, false)

	asset := assetFromIndex(t, h)
	rec := get(t, h, asset)
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
	rec = get(t, h, strings.Replace(asset, "/assets/", "/assets/./", 1))
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
		if !strings.Contains(rec.Body.String(), `id="root"`) {
			t.Fatalf("%s served something other than the shell: %s", path, rec.Body.String())
		}
	}
}

// A captcha provider's origins are the only thing that may widen the policy, and only the three
// directives a widget needs.
//
// This is the test that keeps the widening honest. The panel renders customer-supplied text
// inside an authenticated origin, so `script-src 'self'` with no 'unsafe-inline' is load-bearing;
// a feature that needed a looser policy would otherwise quietly loosen all of it. Here the
// widening is bounded: the provider's hosts are added to script-src, frame-src and connect-src,
// `frame-ancestors` stays 'none', and every other directive is byte-identical.
func TestChallengeOriginsWidenOnlyWhatAWidgetNeeds(t *testing.T) {
	h := newHandler(t, false).WithChallengeOrigins([]string{"https://challenges.cloudflare.com"})
	csp := get(t, h, "/").Header().Get("Content-Security-Policy")

	for _, want := range []string{
		"script-src 'self' https://challenges.cloudflare.com",
		"frame-src https://challenges.cloudflare.com",
		"connect-src 'self' https://challenges.cloudflare.com",
		// Untouched, and these are the ones that matter.
		"frame-ancestors 'none'",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'self'",
	} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP missing %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Fatalf("CSP contains an unsafe exemption: %s", csp)
	}
	// style-src and img-src must not have moved: a challenge widget needs neither.
	if strings.Contains(csp, "style-src 'self' https://challenges") ||
		strings.Contains(csp, "img-src 'self' data: https://challenges") {
		t.Fatalf("the policy was widened further than the widget needs: %s", csp)
	}
}

// With no provider configured the policy is exactly as strict as it was, which is the common case:
// most shops do not run a challenge, and they must not pay for the feature's policy.
func TestNoChallengeKeepsTheStrictestPolicy(t *testing.T) {
	csp := get(t, newHandler(t, false), "/").Header().Get("Content-Security-Policy")
	for _, want := range []string{"script-src 'self'", "frame-src 'none'", "connect-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP missing %q: %s", want, csp)
		}
	}
}

// The service worker is served from a stable URL with no-cache.
//
// Both halves are load-bearing. A hashed URL would make every deploy a different registration
// URL, so the browser would install a second worker beside the first rather than replacing it;
// and a long-cached worker is one the browser never checks for an update, so a fix to the push
// handler would never reach an installed device. This test exists because the build silently
// stopped producing the file — esbuild reports `write: false` output as `<stdout>`, so the
// selection by extension found nothing and the build failed only when someone ran it.
func TestServiceWorkerIsServedAtAStableUncachedURL(t *testing.T) {
	h := newHandler(t, false)
	rec := get(t, h, "/sw.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /sw.js = %d, want 200 (the build must emit dist/sw.js)", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache: a cached worker never updates", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("Content-Type = %q, want javascript: a browser refuses a worker served as text", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "addEventListener") {
		t.Fatalf("the worker body does not look like a service worker: %s", body[:min(len(body), 120)])
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
	for _, p := range []string{"/", "/customers", assetFromIndex(t, h)} {
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

// Every asset the shell references must exist in the embedded tree.
//
// The dist directory is committed, so it can be half-built: a build that wrote the bundle
// and then failed before rewriting index.html leaves the previous hash in the HTML and a
// 404 for the script. The page then renders nothing, which is exactly the kind of failure a
// unit test on the handler cannot see.
func TestShellReferencesExistingAssets(t *testing.T) {
	h := newHandler(t, false)
	index := string(h.Index())

	for _, ref := range assetRefs(index) {
		rec := get(t, h, ref)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s is referenced by index.html but served %d", ref, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Fatalf("%s is empty", ref)
		}
	}
}

// The bundle must not contain an inline script or style attribute in the shell: the CSP has
// no 'unsafe-inline', so either would be refused by the browser rather than reported.
func TestShellHasNoInlineScriptOrStyle(t *testing.T) {
	h := newHandler(t, false)
	index := string(h.Index())

	if strings.Contains(index, "style=\"") || strings.Contains(index, "style='") {
		t.Fatal("the shell contains an inline style attribute; the CSP refuses it")
	}
	if strings.Contains(index, "<script>") {
		t.Fatal("the shell contains an inline script; the CSP refuses it")
	}
	for _, ref := range assetRefs(index) {
		if strings.HasSuffix(ref, ".js") {
			body := get(t, h, ref).Body.String()
			if strings.Contains(body, "<script") {
				t.Fatalf("%s appears to contain HTML", ref)
			}
		}
	}
}

// assetRefs extracts every /assets/... reference from a document.
func assetRefs(document string) []string {
	var out []string
	seen := map[string]bool{}
	for _, marker := range []string{`src="`, `href="`} {
		rest := document
		for {
			i := strings.Index(rest, marker)
			if i < 0 {
				break
			}
			rest = rest[i+len(marker):]
			j := strings.Index(rest, `"`)
			if j < 0 {
				break
			}
			ref := rest[:j]
			if strings.HasPrefix(ref, "/assets/") && !seen[ref] {
				seen[ref] = true
				out = append(out, ref)
			}
		}
	}
	return out
}
