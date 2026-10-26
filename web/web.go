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
//
// # The shell is rendered, not static
//
// The built index.html carries `{{THEME_*}}` placeholders for the document head (title,
// description, social card, favicon, manifest, theme colour) and for the theme's stylesheet and
// boot script. This package substitutes them from a ShellTheme and caches the result per
// revision, so the hot path is a map lookup rather than a template execution.
//
// The substitution is deliberately *not* html/template over the whole document: the shell is a
// build artefact with `{{STYLE}}`/`{{SCRIPT}}` already resolved by web/build.mjs, and running the
// entire file through a contextual escaper would need every existing byte to be template-safe.
// Instead each placeholder is filled with a value escaped for the context it lands in, and the one
// unescaped value (the inline brand mark) is markup this package controls or has already
// sanitised at upload.
package web

import (
	"context"
	"embed"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// dist holds the built frontend. The build writes index.html and hashed assets here.
//
//go:embed dist
var dist embed.FS

// hashedAsset matches a build-tool content hash in a filename, e.g. app.4f2c1d9e.js.
// Only such a file is safe to cache immutably: the name changes when the content does.
var hashedAsset = regexp.MustCompile(`\.[0-9a-f]{8,}\.(js|css|woff2?|png|svg|jpg|webp)$`)

// ShellTheme is everything a theme contributes to the document.
//
// It is a plain value rather than an interface so the handler can cache a rendered shell keyed on
// it, and so a test can construct one without a database.
type ShellTheme struct {
	// Revision identifies the theme revision the shell was rendered for. It is part of the cache
	// key and it is what makes the linked stylesheet addressable and immutable.
	Revision string
	// Title is the document title; Description the meta description.
	Title       string
	Description string
	// SiteName is og:site_name.
	SiteName string
	// ThemeColor is the browser chrome colour.
	ThemeColor string
	// ColorScheme is "dark" or "light": the mode a cold visit gets.
	ColorScheme string
	// CSSHref is the revision-addressed stylesheet, or "" when no theme is published (the bundled
	// stylesheet is then the only one, which is the correct fallback).
	CSSHref string
	// BootSrc is the revision-addressed pre-paint script, or "".
	BootSrc string
	// FaviconSVG, FaviconPNG and AppleTouchIcon are URLs.
	FaviconSVG     string
	FaviconPNG     string
	AppleTouchIcon string
	// ManifestHref is the PWA manifest URL.
	ManifestHref string
	// OGImage is the social card URL, with its pixel size.
	OGImage       string
	OGImageWidth  int
	OGImageHeight int
	// Mark is inline SVG markup for the boot screen. It is inserted unescaped, so it must be
	// markup this package controls or an SVG asset that was sanitised at upload.
	Mark string
	// Nav, Density, CardStyle and TableStyle become data attributes on <html>, which is what the
	// generated stylesheet's shell rules key off.
	Nav        string
	Density    string
	CardStyle  string
	TableStyle string
}

// DefaultShellTheme is the appearance of an installation with no theme.
//
// It reproduces what the built shell shipped before the theme registry existed, so `go build` with
// no Node toolchain, no database and no published theme serves exactly the page it served before.
// The brand assets it names are the ones scripts/brand/verify.mjs asserts on.
func DefaultShellTheme() ShellTheme {
	return ShellTheme{
		Revision:       "default",
		Title:          "Catalyst Billing",
		Description:    "Catalyst Billing — revenue operations for the people who keep the ledger honest.",
		SiteName:       "Catalyst Billing",
		ThemeColor:     "#161412",
		ColorScheme:    "dark",
		FaviconSVG:     "/assets/mark.svg",
		FaviconPNG:     "/assets/favicon-32.png",
		AppleTouchIcon: "/assets/apple-touch-icon.png",
		ManifestHref:   "/theme/manifest.webmanifest",
		OGImage:        "/assets/og.png",
		OGImageWidth:   1200,
		OGImageHeight:  630,
		Mark:           defaultMark,
		Nav:            "sidebar",
		Density:        "compact",
		CardStyle:      "outlined",
		TableStyle:     "compact",
	}
}

// ThemeProvider supplies the shell appearance for a request.
//
// It is a function of the request because the scope does: an anonymous visitor to `/` gets the
// storefront theme and a signed-in operator gets the panel theme, and only the caller knows which
// is which. A provider that cannot answer returns DefaultShellTheme() rather than an error: the
// shell must always render, because a theme problem that produced a blank page would take the whole
// product down to protect a colour.
type ThemeProvider interface {
	ShellTheme(ctx context.Context, r *http.Request) ShellTheme
}

// ThemeProviderFunc adapts a function.
type ThemeProviderFunc func(ctx context.Context, r *http.Request) ShellTheme

// ShellTheme implements ThemeProvider.
func (f ThemeProviderFunc) ShellTheme(ctx context.Context, r *http.Request) ShellTheme {
	return f(ctx, r)
}

// Handler serves the SPA.
type Handler struct {
	fsys fs.FS
	// index is the fallback document for a client-side route, with the theme placeholders intact.
	index []byte
	// secure enables HSTS, which must only be sent over TLS.
	secure bool
	// apiPrefixes are the path prefixes that must never fall back to the SPA.
	apiPrefixes []string
	// challengeOrigins are the extra script and frame origins a configured captcha provider
	// needs. Empty — the default, and the case with no provider — leaves the policy exactly as
	// strict as it was.
	challengeOrigins []string
	// theme supplies the shell appearance; nil means the built-in default.
	theme ThemeProvider

	// shellMu guards shellCache. The cache is small and bounded: one entry per revision in play,
	// which is at most one per scope plus whatever a preview is showing.
	shellMu    sync.RWMutex
	shellCache map[string][]byte
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
		apiPrefixes: []string{"/api/", "/webhooks/", "/theme/"},
		shellCache:  map[string][]byte{},
	}, nil
}

// WithChallengeOrigins permits a captcha provider's script and frame origins.
//
// The policy below is deliberately strict: `script-src 'self'` with no 'unsafe-inline' is what
// stops an injected <script> running inside an authenticated origin. A challenge widget cannot
// work under it — it loads a script from the provider's own host and renders their iframe — so
// the policy is widened by exactly the origins of the *configured* provider, and by nothing
// at all when none is configured.
//
// That is the smallest widening that makes the feature work. It is applied at startup from the
// same setting the verifier reads, so a provider that is switched off does not leave a hole open,
// and it is never derived from a request.
func (h *Handler) WithChallengeOrigins(origins []string) *Handler {
	h.challengeOrigins = origins
	return h
}

// WithTheme attaches the theme provider.
//
// It is optional. Without it the handler serves the built-in default shell, which is what an
// API-only deployment and every unit test get.
func (h *Handler) WithTheme(p ThemeProvider) *Handler {
	h.theme = p
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
//
// The document itself is `no-store` because a cached shell is how a deploy leaves every returning
// user on the previous bundle. The *rendered bytes* are cached in memory per revision, which is
// the opposite trade: same response, no template work on the hot path.
func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	theme := h.themeFor(r)
	document := h.Shell(theme)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(document)
}

// WritePreviewShell serves a rendered shell for the Theme Studio's preview iframe.
//
// It is the one response in the product that may be framed by this origin, and the widening is
// scoped to this response: the rest of the app keeps `frame-ancestors 'none'`. The response is
// `no-store` and `noindex` because it shows an unpublished draft, and it is reachable only through
// a signed, short-lived token that the API layer verifies before calling this.
func (h *Handler) WritePreviewShell(w http.ResponseWriter, r *http.Request, theme ShellTheme) {
	document := h.Shell(theme)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	h.securityHeadersFor(w, true)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(document)
}

// Shell renders the shell for a theme, caching the result per revision.
//
// The cache key includes the revision and every field that can differ between two shells at the
// same revision (a preview passes a draft document whose revision id has not changed yet), so a
// stale entry cannot be served for a different appearance.
func (h *Handler) Shell(theme ShellTheme) []byte {
	key := shellCacheKey(theme)

	h.shellMu.RLock()
	if cached, ok := h.shellCache[key]; ok {
		h.shellMu.RUnlock()
		return cached
	}
	h.shellMu.RUnlock()

	document := h.render(theme)

	h.shellMu.Lock()
	// Bound the cache. Two scopes plus a preview is three; anything beyond a handful means
	// revisions are being published faster than they are being read, and an unbounded map on the
	// document path is a memory leak with a nice name.
	if len(h.shellCache) > 16 {
		h.shellCache = map[string][]byte{}
	}
	h.shellCache[key] = document
	h.shellMu.Unlock()
	return document
}

// shellCacheKey renders the fields that change the document.
func shellCacheKey(t ShellTheme) string {
	var b strings.Builder
	b.WriteString(t.Revision)
	for _, field := range []string{
		t.Title, t.Description, t.SiteName, t.ThemeColor, t.ColorScheme,
		t.CSSHref, t.BootSrc, t.FaviconSVG, t.FaviconPNG, t.AppleTouchIcon,
		t.ManifestHref, t.OGImage, t.Nav, t.Density, t.CardStyle, t.TableStyle,
		strconv.Itoa(t.OGImageWidth), strconv.Itoa(t.OGImageHeight), t.Mark,
	} {
		b.WriteByte(0)
		b.WriteString(field)
	}
	return b.String()
}

// render substitutes the theme placeholders.
//
// Each value is escaped for the context it lands in: text and attribute values through html.Escape,
// and the one raw slot (the boot mark) is markup. A theme title containing `<script>` therefore
// becomes text, which is the whole point of rendering the shell on the server rather than
// interpolating a string in the browser.
func (h *Handler) render(t ShellTheme) []byte {
	// A missing field must not produce an empty attribute; the default is the fallback for every
	// slot, so a partial theme degrades to the built-in appearance rather than to a blank one.
	def := DefaultShellTheme()

	replacements := []struct {
		marker string
		value  string
		raw    bool
	}{
		{"{{THEME_TITLE}}", orString(t.Title, def.Title), false},
		{"{{THEME_DESCRIPTION}}", orString(t.Description, def.Description), false},
		{"{{THEME_SITE_NAME}}", orString(t.SiteName, def.SiteName), false},
		{"{{THEME_COLOR}}", orString(t.ThemeColor, def.ThemeColor), false},
		{"{{THEME_COLOR_SCHEME}}", orString(t.ColorScheme, def.ColorScheme), false},
		{"{{THEME_CSS}}", t.CSSHref, false},
		{"{{THEME_BOOT}}", t.BootSrc, false},
		{"{{THEME_FAVICON_SVG}}", orString(t.FaviconSVG, def.FaviconSVG), false},
		{"{{THEME_FAVICON_PNG}}", orString(t.FaviconPNG, def.FaviconPNG), false},
		{"{{THEME_APPLE_ICON}}", orString(t.AppleTouchIcon, def.AppleTouchIcon), false},
		{"{{THEME_MANIFEST}}", orString(t.ManifestHref, def.ManifestHref), false},
		{"{{THEME_OG_IMAGE}}", orString(t.OGImage, def.OGImage), false},
		{"{{THEME_OG_W}}", strconv.Itoa(orInt(t.OGImageWidth, def.OGImageWidth)), false},
		{"{{THEME_OG_H}}", strconv.Itoa(orInt(t.OGImageHeight, def.OGImageHeight)), false},
		{"{{THEME_NAV}}", orString(t.Nav, def.Nav), false},
		{"{{THEME_DENSITY}}", orString(t.Density, def.Density), false},
		{"{{THEME_CARD_STYLE}}", orString(t.CardStyle, def.CardStyle), false},
		{"{{THEME_TABLE_STYLE}}", orString(t.TableStyle, def.TableStyle), false},
		{"{{THEME_MARK}}", orString(t.Mark, def.Mark), true},
		// The theme stylesheet is a second <link>, after the bundled one, so its declarations win
		// on source order at equal specificity. Empty when no theme is published, which leaves the
		// bundled stylesheet as the only one — the byte-identical fallback.
		{"{{THEME_CSS_LINK}}", cssLink(t.CSSHref), true},
		{"{{THEME_BOOT_SCRIPT}}", bootScript(t.BootSrc), true},
	}

	out := string(h.index)
	for _, r := range replacements {
		value := r.value
		if !r.raw {
			value = html.EscapeString(value)
		}
		out = strings.ReplaceAll(out, r.marker, value)
	}
	return []byte(out)
}

// cssLink renders the theme stylesheet link, or nothing.
func cssLink(href string) string {
	if strings.TrimSpace(href) == "" {
		return ""
	}
	return `<link rel="stylesheet" href="` + html.EscapeString(href) + `" />`
}

// bootScript renders the pre-paint script tag, or nothing.
//
// It is a classic script (no `type="module"`) in <head> so it runs before the first paint; a module
// would be deferred and the default mode would flash for one frame.
func bootScript(src string) string {
	if strings.TrimSpace(src) == "" {
		return ""
	}
	return `<script src="` + html.EscapeString(src) + `"></script>`
}

// themeFor asks the provider, falling back to the default on any problem.
func (h *Handler) themeFor(r *http.Request) ShellTheme {
	if h.theme == nil {
		return DefaultShellTheme()
	}
	theme := h.theme.ShellTheme(r.Context(), r)
	if strings.TrimSpace(theme.Revision) == "" {
		// A provider that returned the zero value is a provider with nothing to say.
		return DefaultShellTheme()
	}
	return theme
}

// securityHeaders applies the documented policy (G2).
//
// The CSP is strict because the panel renders customer-supplied text (invoice line
// descriptions, service names) inside an authenticated origin. `script-src 'self'` with
// no 'unsafe-inline' means an injected <script> cannot run; `frame-ancestors 'none'`
// means the panel cannot be framed for clickjacking; `object-src 'none'` removes a whole
// class of plugin bypasses. There is no nonce or hash because there is no inline script
// to allow: the frontend is a separate file.
func (h *Handler) securityHeaders(w http.ResponseWriter) { h.securityHeadersFor(w, false) }

// securityHeadersFor applies the policy, optionally allowing this origin to frame the response.
//
// Only the draft-preview shell passes preview=true. Keeping it a parameter rather than a second
// policy means the two cannot drift: everything except `frame-ancestors` and `X-Frame-Options` is
// byte-identical, which is what web_test.go asserts.
func (h *Handler) securityHeadersFor(w http.ResponseWriter, preview bool) {
	// The challenge provider's origins are appended to the three directives a widget needs, and
	// only those three: it may load a script, show an iframe, and call back to its own verify
	// endpoint. Nothing else in the policy moves, so the strictness that protects the panel from
	// customer-supplied text is unchanged.
	scriptSrc := "script-src 'self'"
	// `frame-src 'self'` rather than 'none': the Theme Studio's preview is a same-origin iframe
	// inside the panel, and frame-src governs what *this* document may frame while frame-ancestors
	// on the preview response governs who may frame *that* one. Both halves are required, and the
	// per-response frame-ancestors is what keeps the widening from letting any other page be framed.
	frameSrc := "frame-src 'self'"
	connectSrc := "connect-src 'self'"
	if len(h.challengeOrigins) > 0 {
		extra := " " + strings.Join(h.challengeOrigins, " ")
		scriptSrc += extra
		frameSrc = "frame-src" + extra
		connectSrc += extra
	}
	frameAncestors := "frame-ancestors 'none'"
	if preview {
		frameAncestors = "frame-ancestors 'self'"
	}
	w.Header().Set("Content-Security-Policy", strings.Join([]string{
		"default-src 'self'",
		scriptSrc,
		"style-src 'self'",
		"img-src 'self' data:",
		"font-src 'self'",
		connectSrc,
		frameSrc,
		frameAncestors,
		"base-uri 'none'",
		"form-action 'self'",
		"object-src 'none'",
	}, "; "))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	if preview {
		// `DENY` is the header browsers without CSP support honour; for a preview it must allow the
		// same origin, which `SAMEORIGIN` does. It is set here rather than omitted so the response
		// is still framed-only-by-us on an old browser.
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	} else {
		w.Header().Set("X-Frame-Options", "DENY")
	}
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
	case ".json", ".webmanifest":
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

// Index returns the SPA document with its placeholders intact, for tests.
func (h *Handler) Index() []byte { return h.index }

// orString returns value, or fallback when value is blank.
func orString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// orInt returns value, or fallback when it is not positive.
func orInt(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

// defaultMark is the parent's mark, inlined so the first paint needs no request.
//
// It is kept in sync with assets/brand/logo-mark.svg by scripts/brand/build.py. It is a constant
// here rather than read from the embedded tree because the boot screen must paint from the shell
// document alone.
const defaultMark = `<svg viewBox="63 45 374 428" width="34" height="39" fill="none">` +
	`<path d="M240.5 469.1C236.1 466.5 216.3 455.0 196.5 443.5C176.7 432.1 143.6 412.8 123.0 400.7C102.4 388.5 81.0 376.0 75.6 372.9C70.1 369.7 65.3 366.7 64.8 366.2C63.9 365.3 63.6 292.1 64.5 291.2C64.7 290.9 65.5 291.2 66.2 291.7C66.9 292.3 87.5 304.2 112.0 318.2C136.5 332.3 175.2 354.5 198.0 367.7C220.8 380.8 242.0 393.0 245.1 394.8L250.6 397.9L264.1 390.4C271.5 386.2 289.5 376.0 304.2 367.8L331.0 352.8L348.7 360.6C391.7 379.3 402.6 384.2 403.3 384.9C404.4 385.9 403.6 386.4 344.5 420.3C261.3 468.0 250.6 474.0 249.5 473.9C249.0 473.9 244.9 471.7 240.5 469.1ZM415.5 367.0C408.9 363.9 387.3 353.5 367.6 344.1L331.7 326.9L327.6 328.9C322.9 331.1 277.3 353.3 263.0 360.4C257.8 362.9 252.9 365.0 252.0 365.0C250.0 364.9 217.4 342.3 203.7 331.4C191.3 321.6 184.4 313.7 179.4 303.9C175.9 296.9 172.4 285.3 173.4 284.3C173.7 284.0 188.0 292.2 205.2 302.5C222.4 312.9 236.9 321.3 237.5 321.3C238.1 321.3 254.7 312.7 274.5 302.3C330.5 272.8 344.4 265.5 345.5 265.2C346.1 265.1 355.8 270.0 367.0 276.3C378.3 282.5 398.9 293.9 412.8 301.6L438.0 315.5L437.8 341.3L437.5 367.1L432.5 369.9L427.5 372.8L415.5 367.0ZM109.0 289.4C90.6 279.3 72.9 269.5 69.7 267.7L63.8 264.5L63.9 209.0L63.9 153.5L102.2 131.1C137.8 110.3 151.2 102.5 223.4 60.6C238.2 52.0 250.7 45.0 251.2 45.0C251.7 45.0 270.1 55.6 292.3 68.6C314.4 81.7 345.6 99.9 361.5 109.3C377.4 118.7 397.0 130.1 404.8 134.8C412.7 139.4 418.8 143.6 418.3 144.0C417.9 144.5 401.2 152.2 381.3 161.3L345.0 177.7L335.3 172.1C329.9 168.9 308.7 156.8 288.2 145.1L251.0 123.7L230.2 135.7C218.8 142.2 194.8 156.1 176.8 166.6L144.0 185.5L144.0 211.8L144.0 238.2L148.2 235.8C150.6 234.4 158.0 230.3 164.7 226.6L176.9 219.8L177.2 213.7L177.5 207.6L213.5 187.3C233.3 176.2 250.1 167.0 250.9 167.0C251.6 167.0 263.1 173.3 276.4 180.9C289.6 188.6 301.7 195.4 303.2 196.1C304.8 196.8 306.0 197.6 306.0 198.0C306.0 198.3 304.0 200.6 301.5 203.0C299.0 205.4 297.0 207.7 297.0 208.0C297.0 208.8 307.7 214.0 309.2 214.0C310.4 214.0 354.3 193.5 411.4 166.3C425.7 159.5 437.5 154.0 437.7 154.0C437.9 154.0 438.0 167.7 438.0 184.3L438.0 214.7L404.2 231.0C385.7 240.0 366.1 249.5 360.6 252.2C355.2 254.8 350.3 257.0 349.9 257.0C349.4 257.0 326.9 244.4 299.9 229.0L250.7 201.0L225.6 215.3C211.8 223.2 187.8 236.8 172.3 245.6L144.1 261.5L144.0 284.8C144.0 297.5 143.7 308.0 143.2 307.9C142.8 307.9 127.4 299.6 109.0 289.4Z" fill="#F97301" fill-rule="evenodd" />` +
	`</svg>`

// String makes a ShellTheme printable in a log line without dumping the mark.
func (t ShellTheme) String() string {
	return fmt.Sprintf("theme{rev=%s title=%q mode=%s}", t.Revision, t.Title, t.ColorScheme)
}
