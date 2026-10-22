// Package web serves the embedded frontend.
//
// The frontend is compiled into the binary. That is a deployment decision with a
// security consequence: there is no static directory to misconfigure, no separate web
// server with its own defaults, and no way for the running binary and the assets to
// drift apart.
//
// Three rules shape this package:
//
//   - An unknown path that looks like an API path must stay a JSON 404. The SPA
//     fallback exists so a client-side route survives a reload; it must never turn a
//     typo in an endpoint into an HTML page, because a client parsing that as JSON gets a
//     confusing error instead of "no such endpoint".
//   - Only files that exist are served, and only from the embedded tree. There is no
//     directory listing, no path traversal and no filesystem access at runtime.
//   - A hashed asset is immutable and cached for a year; index.html is never cached.
//     Caching index.html is how a deploy leaves every returning user on the old bundle
//     until they hard-refresh.
package web

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
)

// dist holds the built frontend. The build writes index.html and hashed assets here.
//
//go:embed dist
var dist embed.FS

// hashedAsset matches a build-tool content hash in a filename, e.g. app.4f2c1d9e.js.
// Only such a file is safe to cache immutably: the name changes when the content does.
var hashedAsset = regexp.MustCompile(`\.[0-9a-f]{8,}\.(js|css|woff2?|png|svg|jpg|webp)$`)

// Handler serves the SPA.
type Handler struct {
	fsys fs.FS
	// index is the fallback document for a client-side route.
	index []byte
	// secure enables HSTS, which must only be sent over TLS.
	secure bool
	// apiPrefixes are the path prefixes that must never fall back to the SPA.
	apiPrefixes []string
	// challengeOrigins are the extra script and frame origins a configured captcha provider
	// needs. Empty — the default, and the case with no provider — leaves the policy exactly as
	// strict as it was.
	challengeOrigins []string
}

// New constructs a Handler over the embedded assets.
func New(secure bool) (*Handler, error) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, err
	}
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return nil, err
	}
	return &Handler{
		fsys:        sub,
		index:       index,
		secure:      secure,
		apiPrefixes: []string{"/api/", "/webhooks/"},
	}, nil
}

// WithChallengeOrigins permits a captcha provider's script and frame origins.
//
// The policy below is deliberately strict: `script-src 'self'` with no 'unsafe-inline' is what
// stops an injected <script> running inside an authenticated origin. A challenge widget cannot
// work under it — it loads a script from the provider's own host and renders their iframe — so
// the policy is widened by exactly the origins of the *configured* provider, and by nothing at
// all when none is configured.
//
// That is the smallest widening that makes the feature work. It is applied at startup from the
// same setting the verifier reads, so a provider that is switched off does not leave a hole open,
// and it is never derived from a request.
func (h *Handler) WithChallengeOrigins(origins []string) *Handler {
	h.challengeOrigins = origins
	return h
}

// IsAPIPath reports whether a path belongs to the API surface rather than the SPA.
//
// The API handler uses this to answer a JSON 404 instead of the SPA document.
func (h *Handler) IsAPIPath(p string) bool {
	for _, prefix := range h.apiPrefixes {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	switch p {
	case "/healthz", "/readyz", "/metrics":
		return true
	}
	return false
}

// ServeHTTP serves an asset, or the SPA document for a client-side route.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	h.securityHeaders(w)

	// Clean the path so "//assets//app.js" and "/assets/../index.html" cannot reach a
	// different file than the name suggests.
	cleaned := path.Clean("/" + r.URL.Path)
	if cleaned == "/" {
		h.serveIndex(w, r)
		return
	}

	name := strings.TrimPrefix(cleaned, "/")
	info, err := fs.Stat(h.fsys, name)
	if err != nil || info.IsDir() {
		// A client-side route: serve the shell so a reload works.
		h.serveIndex(w, r)
		return
	}

	file, err := h.fsys.Open(name)
	if err != nil {
		h.serveIndex(w, r)
		return
	}
	defer file.Close()

	if hashedAsset.MatchString(name) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if ct := contentType(name); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = io.Copy(w, file)
}

// serveIndex writes the SPA document, never cached.
func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(h.index)
}

// securityHeaders applies the documented policy (G2).
//
// The CSP is strict because the panel renders customer-supplied text (invoice line
// descriptions, service names) inside an authenticated origin. `script-src 'self'` with
// no 'unsafe-inline' means an injected <script> cannot run; `frame-ancestors 'none'`
// means the panel cannot be framed for clickjacking; `object-src 'none'` removes a whole
// class of plugin bypasses. There is no nonce or hash because there is no inline script
// to allow: the frontend is a separate file.
func (h *Handler) securityHeaders(w http.ResponseWriter) {
	// The challenge provider's origins are appended to the three directives a widget needs, and
	// only those three: it may load a script, show an iframe, and call back to its own verify
	// endpoint. Nothing else in the policy moves, so the strictness that protects the panel from
	// customer-supplied text is unchanged.
	scriptSrc := "script-src 'self'"
	frameSrc := "frame-src 'none'"
	connectSrc := "connect-src 'self'"
	if len(h.challengeOrigins) > 0 {
		extra := " " + strings.Join(h.challengeOrigins, " ")
		scriptSrc += extra
		frameSrc = "frame-src" + extra
		connectSrc += extra
	}
	w.Header().Set("Content-Security-Policy", strings.Join([]string{
		"default-src 'self'",
		scriptSrc,
		"style-src 'self'",
		"img-src 'self' data:",
		"font-src 'self'",
		connectSrc,
		frameSrc,
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		"object-src 'none'",
	}, "; "))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")
	if h.secure {
		// Only over TLS: sending HSTS from a plain-HTTP development server would make the
		// browser refuse to reach it again.
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
}

// contentType maps an extension to a type.
//
// It is an explicit table rather than a sniff: a served file must not be interpreted as
// a type the build did not intend, and sniffing is how an uploaded-looking asset becomes
// executable.
func contentType(name string) string {
	switch path.Ext(name) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".woff2":
		return "font/woff2"
	case ".woff":
		return "font/woff"
	case ".ico":
		return "image/x-icon"
	case ".txt", ".map":
		return "text/plain; charset=utf-8"
	default:
		return ""
	}
}

// Index returns the SPA document, for tests.
func (h *Handler) Index() []byte { return h.index }
