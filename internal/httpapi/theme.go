// The theme registry over HTTP: the public manifest and revision-addressed resources, and the
// staff surface the Theme Studio edits (docs/theming-system.md §6).
//
// # The two halves, and why they are one file
//
// The public half is what a browser needs before it paints: a manifest, a stylesheet, a boot script
// and a handful of assets. The staff half is the editor: drafts, publishing, history, presets,
// export. They live together because they share one cache and one document model, and splitting them
// would mean two files that must be kept in step every time a token is added.
//
// # The rules that shape the handlers
//
//  1. **Theme resources are first-party files, not inline markup.** The CSP is `script-src 'self';
//     style-src 'self'` with no 'unsafe-inline', so the stylesheet is a `text/css` response and the
//     boot code is a `text/javascript` one. Nothing here writes a `<style>` tag or an inline script,
//     and nothing here needs a nonce.
//
//  2. **A revision in the URL is a promise.** `/theme/<revision>/theme.css` serves *that* revision
//     and is cached `immutable` for a year. A publish changes the shell (which is `no-store`), so a
//     returning browser picks up the new revision on its next navigation with no cache purge.
//
//  3. **A draft is invisible.** Only a published revision reaches a visitor. The one exception is
//     `/theme/preview`, which requires an HMAC-signed token bound to a user and a revision, is
//     `no-store` and `noindex`, and is the only response in the product that may be framed.
//
//  4. **The theme does not own tenant identity.** The display name, logo and accent stay in
//     `branding.*` and are read through the existing brand loader. This file composes them for the
//     shell; it never writes them. `/api/v1/staff/branding` keeps its own permission.
//
//  5. **Publishing is one transaction and one invalidation.** The store commits the revision, the
//     assignment and the publication row together; the cache is invalidated after the commit, never
//     before, or a concurrent request would reload the old revision and cache it for another TTL.
package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	// PNG and JPEG are imported for their decoders so an upload's dimensions can be read — and so a
	// file that claims to be an image and is not is refused at the edge rather than served.
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/theme"
	themecache "github.com/catalyst/billing/internal/theme/cache"
	"github.com/catalyst/billing/internal/theme/css"
	"github.com/catalyst/billing/internal/theme/presets"
	"github.com/catalyst/billing/internal/theme/store"
	"github.com/catalyst/billing/web"
	"github.com/jackc/pgx/v5"
)

func init() {
	RegisterRoutes("47-theme", registerTheme)
}

func registerTheme(r *router) {
	// ---- public ----
	//
	// The manifest is public because the storefront is: an anonymous visitor's first paint needs the
	// tenant's colours, and there is nothing in a resolved palette that is not already visible on the
	// page they are looking at.
	r.publicGet("/api/v1/theme", r.s.handleThemeManifest)
	r.publicGet("/api/v1/theme/{scope}", r.s.handleThemeManifest)

	r.publicGet("/theme/r/{revision}/theme.css", r.s.handleThemeCSS)
	r.publicGet("/theme/r/{revision}/boot.js", r.s.handleThemeBoot)
	r.publicGet("/theme/assets/{id}", r.s.handleThemeAsset)
	r.publicGet("/theme/manifest.webmanifest", r.s.handleThemeWebManifest)
	r.publicGet("/theme/favicon.ico", r.s.handleThemeFavicon)
	r.publicGet("/theme/og.png", r.s.handleThemeOGImage)
	// The one framed response in the product. The token is the authorisation; there is no session
	// requirement, because a preview is opened in an iframe that may be cross-navigation within the
	// same origin and the token already names the user who asked for it.
	r.publicGet("/theme/preview", r.s.handleThemePreview)

	// ---- staff ----
	r.staffGet("/api/v1/staff/theme", authn.PermThemeRead, r.s.handleThemeOverview)
	r.staffGet("/api/v1/staff/theme/presets", authn.PermThemeRead, r.s.handleThemePresets)
	r.staffGet("/api/v1/staff/theme/assets", authn.PermThemeRead, r.s.handleThemeAssetList)
	r.staffGet("/api/v1/staff/theme/{scope}", authn.PermThemeRead, r.s.handleThemeScope)
	r.staffGet("/api/v1/staff/theme/revisions/{id}/diff", authn.PermThemeRead, r.s.handleThemeDiff)
	r.staffExport("/api/v1/staff/theme/{scope}/export", authn.PermThemeRead, r.s.handleThemeExport)

	r.staffMutation(http.MethodPost, "/api/v1/staff/theme/{scope}/draft", authn.PermThemeWrite,
		r.s.handleThemeCreateDraft)
	r.staffMutation(http.MethodPut, "/api/v1/staff/theme/revisions/{id}", authn.PermThemeWrite,
		r.s.handleThemeUpdateDraft)
	r.staffMutation(http.MethodPost, "/api/v1/staff/theme/revisions/{id}/restore", authn.PermThemeWrite,
		r.s.handleThemeRestore)
	r.staffMutation(http.MethodPost, "/api/v1/staff/theme/revisions/{id}/publish", authn.PermThemePublish,
		r.s.handleThemePublish)
	r.staffMutation(http.MethodPost, "/api/v1/staff/theme/preview-token", authn.PermThemeWrite,
		r.s.handleThemePreviewToken)
	r.staffMutation(http.MethodPost, "/api/v1/staff/theme/import", authn.PermThemeWrite,
		r.s.handleThemeImport)
	r.staffMutation(http.MethodPost, "/api/v1/staff/theme/assets", authn.PermThemeWrite,
		r.s.handleThemeAssetUpload)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/theme/assets/{id}", authn.PermThemeWrite,
		r.s.handleThemeAssetDelete)
}

// ------------------------------------------------------------- the cache --

// themes returns the theme snapshot cache, building it on first use.
//
// Lazy construction rather than a field set in New: a Server built in a test without a pool must
// still serve a shell, and the cache's own contract is that it answers with the built-in default when
// it cannot read anything.
func (s *Server) themes() *themecache.Cache {
	s.themeCacheOnce.Do(func() {
		var st *store.Store
		if s.pool != nil {
			st = store.New(s.pool)
		}
		s.themeCache = themecache.New(st).WithLogger(s.cfg.Logger)
	})
	return s.themeCache
}

// themeStore returns the theme store, or nil when there is no database.
func (s *Server) themeStore() *store.Store {
	if s.pool == nil {
		return nil
	}
	return store.New(s.pool)
}

// ---------------------------------------------------------- scope lookup --

// shellScope decides which presentation scope a browser request belongs to.
//
// # The rule is about the screen, not about the caller
//
// `/` and `/products*` render the storefront for *everyone*, including a signed-in operator — the
// dashboard is `/dashboard`. So the storefront is a path, not a permission, and the session is
// deliberately not consulted: an operator who opens their own shop to check what a customer sees must
// see the shop's theme, because that is the thing they opened.
//
// An earlier version of this function returned the panel scope for a signed-in user on `/`, on the
// reasoning that a signed-in person is "working". It is wrong, and a browser was the only thing that
// caught it: the storefront page was served the panel's stylesheet and came out wearing the staff
// panel's published theme, which is the one outcome a storefront scope exists to prevent.
//
// The rest is the same rule read the same way:
//
//   - the customer prefixes are always the portal, signed in or not, because the portal is the
//     surface a customer is in whether or not their session has expired;
//   - everything else is the panel.
func shellScope(path string) string {
	switch {
	case path == "/" || path == "/products" || strings.HasPrefix(path, "/products/"):
		return "storefront"
	case path == "/portal" || strings.HasPrefix(path, "/portal/"),
		path == "/cart" || strings.HasPrefix(path, "/cart/"),
		path == "/checkout" || strings.HasPrefix(path, "/checkout/"),
		path == "/account" || strings.HasPrefix(path, "/account/"),
		path == "/tickets" || strings.HasPrefix(path, "/tickets/"),
		path == "/login", path == "/register", path == "/forgot-password",
		path == "/reset-password", path == "/verify":
		return "portal"
	default:
		return "panel"
	}
}

// ShellTheme implements web.ThemeProvider: the appearance of the shell for this request.
//
// It never returns the zero value. A provider that failed would make the shell's title blank and its
// social card absent, which is a worse outcome than a colour that is one publish out of date, so
// every failure path falls back to the built-in default.
func (s *Server) ShellTheme(ctx context.Context, r *http.Request) web.ShellTheme {
	scope := shellScope(r.URL.Path)
	snap := s.themes().Snapshot(ctx, scope)
	out := web.DefaultShellTheme()

	out.Revision = snap.Revision
	out.CSSHref = "/theme/r/" + snap.Revision + "/theme.css"
	out.BootSrc = "/theme/r/" + snap.Revision + "/boot.js"
	out.ThemeColor = css.ThemeColor(snap.Document)
	out.ColorScheme = css.ColorScheme(snap.Document)
	out.Nav = orString(snap.Document.Layout.Shell.Nav, "sidebar")
	out.Density = orString(snap.Document.Space.Density, "compact")
	out.CardStyle = orString(snap.Document.Layout.Shell.CardStyle, "outlined")
	out.TableStyle = orString(snap.Document.Layout.Shell.TableStyle, "compact")

	// The tenant's display name is identity, and the theme document's name is an administrative label
	// for the gallery and the history. A published preset called "Brutalist" must not become the
	// product's name in the browser tab, so the theme name is never used for the shell: the brand name
	// wins, and the built-in title is the fallback.
	if brand, err := s.loadBrand(ctx); err == nil && strings.TrimSpace(brand.Name) != "" {
		out.Title = brand.Name
		out.SiteName = brand.Name
		out.Description = brand.Name + " — billing and service automation."
	}
	return out
}

// ------------------------------------------------------------- the public --

// themeManifest is the resolved presentation a client needs.
type themeManifest struct {
	Scope     string `json:"scope"`
	Revision  string `json:"revision"`
	Digest    string `json:"digest"`
	Published bool   `json:"published"`
	// CSS and Boot are the revision-addressed resource URLs.
	CSS string `json:"css"`
	// Boot is empty only when no theme is published; the shell then has no pre-paint script and the
	// bundled theme module applies the stored mode, which is exactly the pre-theme behaviour.
	Boot     string            `json:"boot"`
	Document theme.Document    `json:"document"`
	Meta     themeManifestMeta `json:"meta"`
}

// themeManifestMeta is the document-level facts a client would otherwise derive.
type themeManifestMeta struct {
	Title       string `json:"title"`
	ThemeColor  string `json:"theme_color"`
	ColorScheme string `json:"color_scheme"`
	// Nav, Density, CardStyle and TableStyle are the shell shape, so a client that has the manifest
	// does not have to read them out of the nested document.
	Nav        string `json:"nav"`
	Density    string `json:"density"`
	CardStyle  string `json:"card_style"`
	TableStyle string `json:"table_style"`
}

// manifestFor builds the manifest for a snapshot.
func manifestFor(snap *themecache.Snapshot) themeManifest {
	return themeManifest{
		Scope:     snap.Scope,
		Revision:  snap.Revision,
		Digest:    snap.Digest,
		Published: snap.Published,
		CSS:       "/theme/r/" + snap.Revision + "/theme.css",
		Boot:      "/theme/r/" + snap.Revision + "/boot.js",
		Document:  snap.Document,
		Meta: themeManifestMeta{
			Title:       snap.Document.Name,
			ThemeColor:  css.ThemeColor(snap.Document),
			ColorScheme: css.ColorScheme(snap.Document),
			Nav:         orString(snap.Document.Layout.Shell.Nav, "sidebar"),
			Density:     orString(snap.Document.Space.Density, "compact"),
			CardStyle:   orString(snap.Document.Layout.Shell.CardStyle, "outlined"),
			TableStyle:  orString(snap.Document.Layout.Shell.TableStyle, "compact"),
		},
	}
}

// handleThemeManifest serves the manifest for a scope, or for the caller's own scope.
//
// `GET /api/v1/theme` with no scope uses the request path, the same rule the shell uses, so a client
// that fetches it from the page it is on gets the right answer without knowing the rule.
func (s *Server) handleThemeManifest(w http.ResponseWriter, r *http.Request) {
	scope := r.PathValue("scope")
	if scope == "" {
		scope = shellScope(r.URL.Path)
	}
	if !store.ValidScope(scope) {
		writeProblem(w, http.StatusNotFound, "unknown_scope",
			"no such theme scope; the scopes are "+strings.Join(store.Scopes, ", "))
		return
	}
	// The panel and portal manifests describe a signed-in surface. Serving them anonymously would
	// tell an unauthenticated caller what the staff panel's theme is, which is small but pointless.
	if _, signedIn := userFrom(r.Context()); (scope == "panel" || scope == "portal") && !signedIn {
		writeProblem(w, http.StatusUnauthorized, "unauthorized", "sign in to read this scope")
		return
	}
	snap := s.themes().Snapshot(r.Context(), scope)

	body := manifestFor(snap)
	// The manifest's title is what a client shows as the page name, so it follows the same rule the
	// shell does: the tenant's brand, never the theme document's administrative name.
	if brand, err := s.loadBrand(r.Context()); err == nil && strings.TrimSpace(brand.Name) != "" {
		body.Meta.Title = brand.Name
	}
	etag := snap.ETag()
	w.Header().Set("ETag", etag)
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	// `no-cache`, not a max-age, and the distinction is the whole reason this comment exists.
	//
	// The manifest is applied by the client *after* the shell has painted — lib/theme.ts reads it to
	// set the shell attributes — so a browser that serves it from its own cache re-applies the
	// previous theme for as long as the max-age lasts. A 60-second max-age here made a publish look
	// like it did nothing for up to a minute: the stylesheet link was the new revision and the nav
	// variant was the old one, on the same page. A browser found that; nothing else could have.
	//
	// `no-cache` still caches: the ETag is compared and an unchanged manifest is a 304 with no body,
	// which costs one round trip against an in-memory snapshot. Correctness for one conditional
	// request is the right trade for the one response that decides what the page looks like.
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, body)
}

// handleThemeCSS serves a revision's stylesheet.
//
// # Why the scope is looked up and not derived from the path
//
// The URL is `/theme/r/<revision>/theme.css`, and `<revision>` is the document digest. The scope
// cannot be read from the request path — every scope's stylesheet lives at the same shape of URL —
// and deriving it from `shellScope` was wrong in a way that took a browser to find: the storefront's
// shell asked for `/theme/r/default/theme.css` and the handler answered for the *panel*, which by
// then had a published revision, so the shop's stylesheet 404'd and the page rendered unstyled.
//
// So the segment is the key. The current snapshot of each scope is checked first (five map lookups,
// the hot path for every page load), and only then the database, for a revision that is no longer
// current — a browser holding a shell from before the last publish.
func (s *Server) handleThemeCSS(w http.ResponseWriter, r *http.Request) {
	snap := s.revisionSnapshot(r.Context(), r.PathValue("revision"))
	if snap == nil || len(snap.CSS) == 0 {
		writeProblem(w, http.StatusNotFound, "unknown_revision", "no such theme revision")
		return
	}
	serveImmutable(w, r, "text/css; charset=utf-8", snap.CSS)
}

// handleThemeBoot serves a revision's pre-paint script.
func (s *Server) handleThemeBoot(w http.ResponseWriter, r *http.Request) {
	snap := s.revisionSnapshot(r.Context(), r.PathValue("revision"))
	if snap == nil || len(snap.Boot) == 0 {
		writeProblem(w, http.StatusNotFound, "unknown_revision", "no such theme revision")
		return
	}
	serveImmutable(w, r, "text/javascript; charset=utf-8", snap.Boot)
}

// revisionSnapshot resolves a revision segment to a snapshot.
//
// The cache first: every scope's current revision is one map lookup, and that covers every request a
// live page makes. A miss means the revision is not current anywhere, which is the stale-tab case,
// and that is answered from the database.
func (s *Server) revisionSnapshot(ctx context.Context, segment string) *themecache.Snapshot {
	if strings.TrimSpace(segment) == "" {
		return nil
	}
	if snap := s.themes().LookupRevision(ctx, segment); snap != nil {
		return snap
	}
	return s.revisionSnapshotByDigest(ctx, segment)
}

// revisionSnapshotByDigest builds a snapshot for a revision that is not the current one.
//
// It is not cached: an old revision is requested once by a stale tab and never again, so caching it
// would only push the current revision out of the bounded map.
func (s *Server) revisionSnapshotByDigest(ctx context.Context, segment string) *themecache.Snapshot {
	st := s.themeStore()
	if st == nil {
		return nil
	}
	for _, scope := range store.Scopes {
		rev, err := st.ByDigest(ctx, scope, "sha256:"+segment)
		if err != nil || rev == nil {
			continue
		}
		sheet, err := css.Generate(rev.Document)
		if err != nil {
			s.cfg.Logger.ErrorContext(ctx, "generate a historical theme stylesheet",
				"revision", rev.ID, "error", err)
			return nil
		}
		return &themecache.Snapshot{
			Scope:     rev.Scope,
			Revision:  segment,
			Digest:    rev.Digest,
			Document:  rev.Document,
			CSS:       sheet,
			Boot:      css.BootScript(rev.Document),
			Published: rev.Status == store.StatusPublished,
			LoadedAt:  time.Now(),
		}
	}
	return nil
}

// handleThemeAsset serves a theme-only asset.
//
// `immutable` because the id is a uuid for a digest-deduped payload: the bytes behind an id never
// change. `nosniff` because the content type is a stored fact, not a guess.
func (s *Server) handleThemeAsset(w http.ResponseWriter, r *http.Request) {
	st := s.themeStore()
	if st == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	asset, err := st.Asset(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such asset")
		return
	}
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "read a theme asset", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if asset.ContentType == "image/svg+xml" {
		// An SVG is a document that can carry a script. Serving it from this origin with
		// `default-src 'none'; sandbox` means a direct navigation renders it as a picture and cannot
		// run anything; this build does not accept SVG uploads, and the header is here so that a row
		// written by hand cannot become an XSS primitive.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	}
	serveImmutable(w, r, asset.ContentType, asset.Bytes)
}

// handleThemeWebManifest serves the PWA manifest, themed from the current scope.
func (s *Server) handleThemeWebManifest(w http.ResponseWriter, r *http.Request) {
	snap := s.themes().Snapshot(r.Context(), shellScope(r.URL.Path))
	// The installed app's name is the tenant's brand, never the theme document's name.
	name := web.DefaultShellTheme().Title
	if brand, err := s.loadBrand(r.Context()); err == nil && strings.TrimSpace(brand.Name) != "" {
		name = brand.Name
	}
	themeColor := css.ThemeColor(snap.Document)
	manifest := map[string]any{
		"name":             name,
		"short_name":       shortName(name),
		"description":      "Billing and service automation for hosting providers.",
		"start_url":        "/",
		"display":          "standalone",
		"background_color": themeColor,
		"theme_color":      themeColor,
		"icons": []map[string]string{
			{"src": "/assets/icon-192.png", "sizes": "192x192", "type": "image/png"},
			{"src": "/assets/icon-512.png", "sizes": "512x512", "type": "image/png"},
			{"src": "/assets/mark.svg", "sizes": "any", "type": "image/svg+xml"},
		},
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal_error", "the manifest could not be built")
		return
	}
	etag := `W/"` + theme.DigestBytes(body) + `"`
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(append(body, '\n'))
}

// handleThemeFavicon redirects to the themed favicon.
//
// A redirect rather than a proxy: the bytes are already served from a stable URL, and re-serving them
// here would be a second path with a second cache policy. `/theme/favicon.ico` exists because a
// browser asks for it by name.
func (s *Server) handleThemeFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	http.Redirect(w, r, "/assets/favicon-32.png", http.StatusFound)
}

// handleThemeOGImage redirects to the themed social card.
func (s *Server) handleThemeOGImage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	http.Redirect(w, r, "/assets/og.png", http.StatusFound)
}

// ------------------------------------------------------------- the preview --

// previewTokenTTL bounds how long a preview link works. It is short because the token is a bearer
// credential for an unpublished document.
const previewTokenTTL = 15 * time.Minute

// previewClaims is the signed payload of a preview token.
type previewClaims struct {
	// User is the operator who asked. The token is bound to them so a leaked link cannot be used by
	// somebody else's session.
	User string `json:"u"`
	// Revision is the draft revision to render.
	Revision string `json:"r"`
	// Scope is the scope the draft belongs to.
	Scope string `json:"s"`
	// Expires is a unix timestamp.
	Expires int64 `json:"e"`
}

// previewKey returns the process-wide signing key, generating it on first use.
//
// # Why it is per process
//
// There is no installation-wide signing secret in this deployment (sessions are opaque tokens in the
// database, not signed cookies), and inventing a new environment variable for a fifteen-minute
// preview link would be a configuration knob whose absence silently breaks a feature. The cost of a
// per-process key is that a preview opened through a load balancer may hit an instance that did not
// mint the token and be refused. That is a refusal, not a bypass, and the remedy — sticky sessions or
// a shared secret — is a deployment decision rather than something to guess at here.
func (s *Server) previewKey() []byte {
	if len(s.themePreviewKey) == 0 {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			// A failed read of the system CSPRNG is not recoverable, and a predictable preview key
			// would let anyone read an unpublished draft. Refusing is the only safe answer, so the
			// key stays empty and every mint fails closed.
			s.cfg.Logger.Error("theme preview key could not be generated; previews are disabled",
				"error", err)
			return nil
		}
		s.themePreviewKey = key
	}
	return s.themePreviewKey
}

// signPreview renders a token for the claims.
func (s *Server) signPreview(claims previewClaims) (string, error) {
	key := s.previewKey()
	if len(key) == 0 {
		return "", errors.New("preview signing is unavailable")
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		hex.EncodeToString(mac.Sum(nil)), nil
}

// verifyPreview checks a token and returns its claims.
func (s *Server) verifyPreview(token string) (previewClaims, error) {
	key := s.previewKey()
	if len(key) == 0 {
		return previewClaims{}, errors.New("preview signing is unavailable")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return previewClaims{}, errors.New("malformed token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return previewClaims{}, errors.New("malformed token")
	}
	signature, err := hex.DecodeString(parts[1])
	if err != nil {
		return previewClaims{}, errors.New("malformed token")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return previewClaims{}, errors.New("bad signature")
	}
	var claims previewClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return previewClaims{}, errors.New("malformed token")
	}
	if time.Now().Unix() > claims.Expires {
		return previewClaims{}, errors.New("expired")
	}
	return claims, nil
}

// handleThemePreviewToken mints a preview token for a draft.
func (s *Server) handleThemePreviewToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RevisionID string `json:"revision_id"`
		Scope      string `json:"scope"`
	}
	if !s.decodeJSON(w, r, &body, 8<<10) {
		return
	}
	if strings.TrimSpace(body.RevisionID) == "" {
		writeProblem(w, http.StatusBadRequest, "revision_required", "revision_id is required")
		return
	}
	st := s.themeStore()
	if st == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	rev, err := st.Revision(r.Context(), body.RevisionID)
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such revision")
		return
	}
	if err != nil {
		s.themeError(w, r, err)
		return
	}
	scope := body.Scope
	if scope == "" {
		scope = rev.Scope
	}
	if !store.ValidScope(scope) {
		writeProblem(w, http.StatusUnprocessableEntity, "unknown_scope", "no such theme scope")
		return
	}
	user, _ := userFrom(r.Context())
	expires := time.Now().Add(previewTokenTTL)
	token, err := s.signPreview(previewClaims{
		User:     user.ID,
		Revision: rev.ID,
		Scope:    scope,
		Expires:  expires.Unix(),
	})
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "mint a preview token", "error", err)
		writeProblem(w, http.StatusServiceUnavailable, "preview_unavailable",
			"preview links are unavailable on this instance")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"url":        "/theme/preview?scope=" + scope + "&token=" + token,
		"expires_at": expires.UTC().Format(time.RFC3339),
	})
}

// handleThemePreview serves the draft shell for the preview iframe.
func (s *Server) handleThemePreview(w http.ResponseWriter, r *http.Request) {
	if s.web == nil {
		writeProblem(w, http.StatusNotFound, "not_found", "no frontend is embedded in this build")
		return
	}
	claims, err := s.verifyPreview(r.URL.Query().Get("token"))
	if err != nil {
		// No detail: a bad token is a bad token, and saying whether it was expired or forged tells an
		// attacker which half to work on.
		writeProblem(w, http.StatusForbidden, "invalid_preview_token",
			"this preview link is not valid; ask for a new one")
		return
	}
	st := s.themeStore()
	if st == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	rev, err := st.Revision(r.Context(), claims.Revision)
	if err != nil {
		writeProblem(w, http.StatusNotFound, "not_found", "no such revision")
		return
	}
	// Re-validate: the row may have been written by hand, and this is the one path that renders an
	// unpublished document.
	if err := theme.ValidateResolved(rev.Document); err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_document", err.Error())
		return
	}
	segment := themecache.RevisionSegment(rev.Digest)
	shell := web.DefaultShellTheme()
	shell.Revision = segment
	shell.CSSHref = "/theme/r/" + segment + "/theme.css"
	shell.BootSrc = "/theme/r/" + segment + "/boot.js"
	shell.ThemeColor = css.ThemeColor(rev.Document)
	shell.ColorScheme = css.ColorScheme(rev.Document)
	// The preview's title names the theme being previewed, because it is the one place where the
	// document's administrative name is the useful label — and it is never the live shell.
	shell.Title = rev.Document.Name + " — preview"
	shell.SiteName = rev.Document.Name
	shell.Nav = orString(rev.Document.Layout.Shell.Nav, "sidebar")
	shell.Density = orString(rev.Document.Space.Density, "compact")
	shell.CardStyle = orString(rev.Document.Layout.Shell.CardStyle, "outlined")
	shell.TableStyle = orString(rev.Document.Layout.Shell.TableStyle, "compact")
	s.web.WritePreviewShell(w, r, shell)
}

// -------------------------------------------------------------- the staff --

// handleThemeOverview answers "what is live, and what can I do".
func (s *Server) handleThemeOverview(w http.ResponseWriter, r *http.Request) {
	st := s.themeStore()
	scopes := make([]map[string]any, 0, len(store.Scopes))
	for _, scope := range store.Scopes {
		entry := map[string]any{"scope": scope, "label": store.ScopeDisplay[scope]}
		if st == nil {
			scopes = append(scopes, entry)
			continue
		}
		published, err := st.Published(r.Context(), scope)
		if err != nil {
			s.themeError(w, r, err)
			return
		}
		drafts, err := st.Drafts(r.Context(), scope)
		if err != nil {
			s.themeError(w, r, err)
			return
		}
		if published != nil {
			entry["published"] = published
		}
		if len(drafts) > 0 {
			entry["draft"] = drafts[0]
		}
		scopes = append(scopes, entry)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"scopes":      scopes,
		"presets":     presets.Catalogue(),
		"limits":      themeLimits(),
		"permissions": s.themePermissions(r),
		"presets_ok":  presets.Err() == nil,
	})
}

// handleThemePresets serves the gallery.
func (s *Server) handleThemePresets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"presets": presets.Catalogue(),
		"limits":  themeLimits(),
	})
}

// handleThemeScope serves one scope's draft, published revision and history.
func (s *Server) handleThemeScope(w http.ResponseWriter, r *http.Request) {
	scope := r.PathValue("scope")
	if !store.ValidScope(scope) {
		writeProblem(w, http.StatusNotFound, "unknown_scope", "no such theme scope")
		return
	}
	st := s.themeStore()
	if st == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	published, err := st.Published(r.Context(), scope)
	if err != nil {
		s.themeError(w, r, err)
		return
	}
	drafts, err := st.Drafts(r.Context(), scope)
	if err != nil {
		s.themeError(w, r, err)
		return
	}
	history, err := st.History(r.Context(), scope, 50)
	if err != nil {
		s.themeError(w, r, err)
		return
	}

	// The document the editor starts from: the draft if there is one, else the published revision,
	// else the built-in default. `source` is what the operator wrote; `document` is the resolved
	// form, which is what a preview shows.
	document := theme.Default()
	source := theme.Default()
	if published != nil {
		document, source = published.Document, published.Source
	}
	if len(drafts) > 0 {
		document, source = drafts[0].Document, drafts[0].Source
	}

	out := map[string]any{
		"scope":       scope,
		"label":       store.ScopeDisplay[scope],
		"document":    document,
		"source":      source,
		"drafts":      drafts,
		"history":     history,
		"presets":     presets.Catalogue(),
		"limits":      themeLimits(),
		"permissions": s.themePermissions(r),
	}
	if published != nil {
		out["published"] = published
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// themeDraftBody is the create/update payload.
type themeDraftBody struct {
	// Preset applies an embedded preset as the source document.
	Preset string `json:"preset"`
	// Document is an explicit source document, decoded by theme.Decode rather than by the request
	// decoder.
	//
	// # Why it is raw JSON
	//
	// `decodeJSON` sets `DisallowUnknownFields`, so an unknown key *inside* the document came back as
	// "the request body is not valid JSON" — a message that tells an operator nothing about the
	// `colour` they typed instead of `color`. theme.Decode exists to name the key, and it can only do
	// that if it sees the bytes. So the envelope is decoded here and the document is decoded there.
	Document json.RawMessage `json:"document"`
	Note     string          `json:"note"`
}

// document decodes the payload's document, if it carried one.
//
// It returns (nil, nil) when the body named no document, which is the "apply a preset" and "duplicate
// what is live" cases.
func (b themeDraftBody) document() (*theme.Document, error) {
	if len(b.Document) == 0 || string(b.Document) == "null" {
		return nil, nil
	}
	doc, err := theme.Decode(b.Document)
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

// handleThemeCreateDraft creates a draft from a preset, an explicit document, or the current state.
func (s *Server) handleThemeCreateDraft(w http.ResponseWriter, r *http.Request) {
	scope := r.PathValue("scope")
	if !store.ValidScope(scope) {
		writeProblem(w, http.StatusNotFound, "unknown_scope", "no such theme scope")
		return
	}
	st := s.themeStore()
	if st == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	var body themeDraftBody
	if !s.decodeJSON(w, r, &body, theme.MaxDocumentBytes+64<<10) {
		return
	}

	source, err := s.draftSource(r.Context(), scope, body)
	if err != nil {
		s.themeError(w, r, err)
		return
	}
	resolved, err := s.resolveForScope(r.Context(), scope, source)
	if err != nil {
		s.themeError(w, r, err)
		return
	}
	user, _ := userFrom(r.Context())
	rev, err := st.CreateDraft(r.Context(), store.DraftInput{
		Scope:    scope,
		Source:   source,
		Document: resolved,
		Note:     body.Note,
		UserID:   user.ID,
	})
	if err != nil {
		s.themeError(w, r, err)
		return
	}
	s.auditTheme(r, "theme.draft.create", scope, rev.ID, map[string]any{
		"revision": rev.Number, "digest": rev.Digest,
	})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"revision": rev})
}

// draftSource works out the source document a draft should start from.
func (s *Server) draftSource(ctx context.Context, scope string, body themeDraftBody) (theme.Document, error) {
	doc, err := body.document()
	if err != nil {
		return theme.Document{}, err
	}
	switch {
	case doc != nil:
		return *doc, nil
	case body.Preset != "":
		p, ok := presets.Get(body.Preset)
		if !ok {
			return theme.Document{}, fmt.Errorf("theme: no preset %q", body.Preset)
		}
		return p.Document, nil
	default:
		// "Duplicate what is live" — the seasonal-campaign case in §15.
		st := s.themeStore()
		if st != nil {
			if published, err := st.Published(ctx, scope); err == nil && published != nil {
				return published.Source, nil
			}
		}
		return theme.Default(), nil
	}
}

// resolveForScope resolves a source document against the parent scope and the default.
//
// Parent scopes are declared rather than inferred: the portal and the storefront inherit the panel's
// presentation, and email and invoice inherit nothing because their renderers are not CSS.
func (s *Server) resolveForScope(ctx context.Context, scope string, source theme.Document) (theme.Document, error) {
	layers := []theme.Document{theme.Default()}
	if theme.Inherits(source) {
		if parent := parentScope(scope); parent != "" {
			if st := s.themeStore(); st != nil {
				if parentRev, err := st.Published(ctx, parent); err == nil && parentRev != nil {
					layers = append(layers, parentRev.Document)
				}
			}
		}
	}
	// The preset's own `extends`, when it names one, sits between the parent and the document.
	if source.Extends != "" {
		if parent, ok := presets.Get(source.Extends); ok {
			layers = append(layers, parent.Document)
		}
	}
	layers = append(layers, source)
	resolved, err := theme.Resolve(layers...)
	if err != nil {
		return theme.Document{}, err
	}
	resolved = theme.WithoutInheritance(resolved)
	// The resolved document must be complete, because it is what the CSS generator reads.
	resolved.Inherit = nil
	if err := theme.ValidateResolved(resolved); err != nil {
		return theme.Document{}, err
	}
	return resolved, nil
}

// parentScope returns the scope a scope inherits from, or "".
func parentScope(scope string) string {
	switch scope {
	case "portal", "storefront":
		return "panel"
	default:
		return ""
	}
}

// handleThemeUpdateDraft replaces a draft's document.
func (s *Server) handleThemeUpdateDraft(w http.ResponseWriter, r *http.Request) {
	st := s.themeStore()
	if st == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	var body themeDraftBody
	if !s.decodeJSON(w, r, &body, theme.MaxDocumentBytes+64<<10) {
		return
	}
	doc, err := body.document()
	if err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_document", err.Error())
		return
	}
	if doc == nil {
		writeProblem(w, http.StatusBadRequest, "document_required",
			"the request must carry the document to save")
		return
	}
	existing, err := st.Revision(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such revision")
		return
	}
	if err != nil {
		s.themeError(w, r, err)
		return
	}
	resolved, err := s.resolveForScope(r.Context(), existing.Scope, *doc)
	if err != nil {
		s.themeError(w, r, err)
		return
	}
	user, _ := userFrom(r.Context())
	rev, err := st.UpdateDraft(r.Context(), existing.ID, store.DraftInput{
		Scope:    existing.Scope,
		Source:   *doc,
		Document: resolved,
		Note:     body.Note,
		UserID:   user.ID,
	})
	if err != nil {
		s.themeError(w, r, err)
		return
	}
	s.auditTheme(r, "theme.draft.update", existing.Scope, rev.ID, map[string]any{
		"revision": rev.Number, "digest": rev.Digest,
	})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"revision": rev})
}

// handleThemePublish makes a revision live.
func (s *Server) handleThemePublish(w http.ResponseWriter, r *http.Request) {
	st := s.themeStore()
	if st == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	// An empty body means "publish it with no note" (decodeOptionalJSON, customer_close.go): a client
	// that has nothing to say should not have to send `{}` to say it.
	var body struct {
		Note string `json:"note"`
	}
	if !s.decodeOptionalJSON(w, r, &body, 16<<10) {
		return
	}
	user, _ := userFrom(r.Context())
	result, err := st.Publish(r.Context(), r.PathValue("id"), user.ID, body.Note)
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such revision")
		return
	}
	if err != nil {
		s.themeError(w, r, err)
		return
	}
	// After the commit, never before: invalidating first would let a concurrent request reload the
	// old revision and cache it for another TTL.
	s.themes().Invalidate(result.Revision.Scope)

	s.auditTheme(r, "theme.publish", result.Revision.Scope, result.Revision.ID, map[string]any{
		"revision": result.Revision.Number, "digest": result.Revision.Digest,
		"previous": result.Publication.PreviousRevisionID, "note": body.Note,
	})
	s.publish(events.Event{
		Kind:       "theme.published",
		Permission: string(authn.PermThemeRead),
		Data: map[string]any{
			"scope": result.Revision.Scope, "revision": result.Revision.Number,
		},
	})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"revision":    result.Revision,
		"publication": result.Publication,
	})
}

// handleThemeRestore copies an old revision into a new draft.
func (s *Server) handleThemeRestore(w http.ResponseWriter, r *http.Request) {
	st := s.themeStore()
	if st == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	user, _ := userFrom(r.Context())
	rev, err := st.Restore(r.Context(), r.PathValue("id"), user.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such revision")
		return
	}
	if err != nil {
		s.themeError(w, r, err)
		return
	}
	s.auditTheme(r, "theme.revision.restore", rev.Scope, rev.ID, map[string]any{
		"revision": rev.Number,
	})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"revision": rev})
}

// handleThemeDiff renders the structured difference between a revision and what is live.
func (s *Server) handleThemeDiff(w http.ResponseWriter, r *http.Request) {
	st := s.themeStore()
	if st == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	rev, err := st.Revision(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such revision")
		return
	}
	if err != nil {
		s.themeError(w, r, err)
		return
	}
	before := theme.Default()
	if published, err := st.Published(r.Context(), rev.Scope); err == nil && published != nil {
		before = published.Document
	}
	changes, err := theme.Diff(before, rev.Document)
	if err != nil {
		s.themeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"revision_id": rev.ID,
		"changes":     changes,
		"summary":     theme.DiffSummary(changes),
	})
}

// themeEnvelope is the import/export format (docs/theming-system.md §4.3).
type themeEnvelope struct {
	Kind       string               `json:"kind"`
	Schema     int                  `json:"schema"`
	ExportedAt string               `json:"exported_at"`
	Theme      theme.Document       `json:"theme"`
	Assets     []themeEnvelopeAsset `json:"assets"`
}

// themeEnvelopeAsset is one asset carried by an envelope.
type themeEnvelopeAsset struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	ContentType string `json:"content_type"`
	Digest      string `json:"digest"`
	DataBase64  string `json:"data_base64"`
}

// handleThemeExport renders a deterministic envelope.
//
// Determinism matters because the envelope is meant to be committed to a repository and diffed: two
// exports of the same theme must produce the same bytes.
func (s *Server) handleThemeExport(w http.ResponseWriter, r *http.Request) {
	scope := r.PathValue("scope")
	if !store.ValidScope(scope) {
		writeProblem(w, http.StatusNotFound, "unknown_scope", "no such theme scope")
		return
	}
	st := s.themeStore()
	if st == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	doc := theme.Default()
	if published, err := st.Published(r.Context(), scope); err == nil && published != nil {
		doc = published.Source
	} else if drafts, derr := st.Drafts(r.Context(), scope); derr == nil && len(drafts) > 0 {
		doc = drafts[0].Source
	}
	envelope := themeEnvelope{
		Kind:       "catalyst.theme",
		Schema:     theme.SchemaVersion,
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Theme:      doc,
		Assets:     []themeEnvelopeAsset{},
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", scope+"-theme.json"))
	writeJSON(w, http.StatusOK, envelope)
}

// handleThemeImport creates a draft from an envelope.
//
// Idempotency is the digest: importing the same envelope twice against a scope whose draft already has
// that digest returns the existing revision instead of creating a second identical one.
func (s *Server) handleThemeImport(w http.ResponseWriter, r *http.Request) {
	st := s.themeStore()
	if st == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	var envelope themeEnvelope
	if !s.decodeJSON(w, r, &envelope, theme.MaxDocumentBytes+1<<20) {
		return
	}
	if envelope.Kind != "catalyst.theme" {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_envelope",
			`an envelope must declare "kind": "catalyst.theme"`)
		return
	}
	if envelope.Schema != theme.SchemaVersion {
		writeProblem(w, http.StatusUnprocessableEntity, "unsupported_schema",
			fmt.Sprintf("this build imports schema %d", theme.SchemaVersion))
		return
	}
	if err := theme.Validate(envelope.Theme); err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_document", err.Error())
		return
	}
	scope := envelope.Theme.ID
	_ = scope // the scope comes from the query, not the document: one document can serve several.

	targetScope := r.URL.Query().Get("scope")
	if !store.ValidScope(targetScope) {
		writeProblem(w, http.StatusBadRequest, "scope_required",
			"import needs ?scope= one of "+strings.Join(store.Scopes, ", "))
		return
	}
	resolved, err := s.resolveForScope(r.Context(), targetScope, envelope.Theme)
	if err != nil {
		s.themeError(w, r, err)
		return
	}
	digest, err := theme.Digest(resolved)
	if err != nil {
		s.themeError(w, r, err)
		return
	}
	if drafts, derr := st.Drafts(r.Context(), targetScope); derr == nil && len(drafts) > 0 &&
		drafts[0].Digest == digest {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"revision": drafts[0], "unchanged": true})
		return
	}
	user, _ := userFrom(r.Context())
	rev, err := st.CreateDraft(r.Context(), store.DraftInput{
		Scope:    targetScope,
		Source:   envelope.Theme,
		Document: resolved,
		Note:     "imported",
		UserID:   user.ID,
	})
	if err != nil {
		s.themeError(w, r, err)
		return
	}
	s.auditTheme(r, "theme.import", targetScope, rev.ID, map[string]any{"revision": rev.Number})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"revision": rev})
}

// ------------------------------------------------------------- the assets --

// maxThemeAssetBytes is the upload cap. It is larger than the catalog's 256 KiB because a full-width
// background or a social card is legitimately a megabyte, and smaller than a font subset needs to be
// because a browser font is at most a few hundred kilobytes.
const maxThemeAssetBytes = 4 << 20

// themeAssetKinds is the closed set of asset roles.
var themeAssetKinds = []string{"favicon", "og", "banner", "background", "font", "other"}

// themeAssetTypes is the closed set of content types this build accepts.
//
// SVG is deliberately absent. The design document asks for a parsed, allowlisted SVG (no <script>, no
// <foreignObject>, no event attributes, no external href), and a half-written sanitiser for a format
// whose whole purpose is to be a document is worse than refusing it: a theme's favicon can be a PNG.
// The serving route still applies a sandbox CSP to an SVG row, so a row written by hand cannot become
// an XSS primitive.
var themeAssetTypes = []string{
	"image/png", "image/jpeg", "image/webp", "font/woff2", "font/woff",
}

// handleThemeAssetUpload stores an uploaded asset.
//
// A raw-body upload with the content type in the header, matching the catalog's image upload: one
// fewer parser in the request path and the browser has a File in hand.
func (s *Server) handleThemeAssetUpload(w http.ResponseWriter, r *http.Request) {
	st := s.themeStore()
	if st == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	kind := r.URL.Query().Get("kind")
	if !containsString(themeAssetKinds, kind) {
		writeProblem(w, http.StatusBadRequest, "invalid_asset_kind",
			"kind must be one of "+strings.Join(themeAssetKinds, ", "))
		return
	}
	contentType := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])
	if !containsString(themeAssetTypes, contentType) {
		writeProblem(w, http.StatusUnsupportedMediaType, "unsupported_asset_type",
			"the asset must be one of "+strings.Join(themeAssetTypes, ", "))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxThemeAssetBytes+1))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_body", "the upload could not be read")
		return
	}
	if len(body) > maxThemeAssetBytes {
		writeProblem(w, http.StatusRequestEntityTooLarge, "asset_too_large",
			"the asset exceeds 4 MiB")
		return
	}
	if len(body) == 0 {
		writeProblem(w, http.StatusBadRequest, "invalid_body", "the upload is empty")
		return
	}

	var width, height *int
	switch contentType {
	case "image/png", "image/jpeg":
		cfg, format, derr := image.DecodeConfig(bytes.NewReader(body))
		if derr != nil {
			writeProblem(w, http.StatusUnprocessableEntity, "invalid_image",
				"the upload is not an image this server can read")
			return
		}
		// The declared type must match the bytes. A PNG declared as a JPEG would be served with the
		// wrong type, and a browser that sniffs would do something unexpected with it.
		if (contentType == "image/png" && format != "png") || (contentType == "image/jpeg" && format != "jpeg") {
			writeProblem(w, http.StatusUnprocessableEntity, "invalid_image",
				"the file's contents do not match its declared type")
			return
		}
		if cfg.Width > 8192 || cfg.Height > 8192 {
			writeProblem(w, http.StatusUnprocessableEntity, "image_too_large",
				"the image is larger than 8192 pixels in one dimension")
			return
		}
		width, height = &cfg.Width, &cfg.Height
	case "font/woff2":
		if !bytes.HasPrefix(body, []byte("wOF2")) {
			writeProblem(w, http.StatusUnprocessableEntity, "invalid_font",
				"the upload is not a WOFF2 font")
			return
		}
	case "font/woff":
		if !bytes.HasPrefix(body, []byte("wOFF")) {
			writeProblem(w, http.StatusUnprocessableEntity, "invalid_font",
				"the upload is not a WOFF font")
			return
		}
	}

	user, _ := userFrom(r.Context())
	asset, err := st.PutAsset(r.Context(), store.AssetInput{
		Kind:        kind,
		ContentType: contentType,
		Bytes:       body,
		Digest:      theme.DigestBytes(body),
		Width:       width,
		Height:      height,
		Alt:         r.URL.Query().Get("alt"),
		UserID:      user.ID,
	})
	if err != nil {
		s.themeError(w, r, err)
		return
	}
	s.auditTheme(r, "theme.asset.upload", kind, asset.ID, map[string]any{
		"content_type": asset.ContentType, "byte_size": asset.ByteSize,
	})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":           asset.ID,
		"kind":         asset.Kind,
		"content_type": asset.ContentType,
		"byte_size":    asset.ByteSize,
		"width":        asset.Width,
		"height":       asset.Height,
		"digest":       asset.Digest,
		"url":          "/theme/assets/" + asset.ID,
		"ref":          theme.AssetRef(asset.ID),
	})
}

// handleThemeAssetList lists the library.
func (s *Server) handleThemeAssetList(w http.ResponseWriter, r *http.Request) {
	st := s.themeStore()
	if st == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	assets, err := st.Assets(r.Context())
	if err != nil {
		s.themeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"assets": assets,
		"limits": map[string]any{
			"max_bytes": maxThemeAssetBytes,
			"kinds":     themeAssetKinds,
			"types":     themeAssetTypes,
		},
	})
}

// handleThemeAssetDelete removes an unused asset.
func (s *Server) handleThemeAssetDelete(w http.ResponseWriter, r *http.Request) {
	st := s.themeStore()
	if st == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id := r.PathValue("id")
	err := st.DeleteAsset(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrAssetInUse):
		writeProblem(w, http.StatusConflict, "asset_in_use",
			"a theme revision still references this asset; remove the reference and republish first")
		return
	case errors.Is(err, store.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such asset")
		return
	case err != nil:
		s.themeError(w, r, err)
		return
	}
	s.auditTheme(r, "theme.asset.delete", "asset", id, nil)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// --------------------------------------------------------------- helpers --

// themeLimits publishes the bounds the server enforces, so the studio can state them.
func themeLimits() map[string]any {
	return map[string]any{
		"max_document_bytes":   theme.MaxDocumentBytes,
		"max_custom_css_bytes": theme.MaxCustomCSSBytes,
		"max_name_length":      theme.MaxNameLength,
		"modes":                theme.AllowedModes,
		"densities":            theme.AllowedDensities,
		"navs":                 theme.AllowedNavs,
		"card_styles":          theme.AllowedCardStyles,
		"table_styles":         theme.AllowedTableStyles,
		"backgrounds":          theme.AllowedBackgrounds,
		"components":           theme.AllowedComponents,
		"families":             theme.InstalledFamilies,
		"scopes":               store.Scopes,
		"asset_max_bytes":      maxThemeAssetBytes,
		"asset_kinds":          themeAssetKinds,
		"asset_types":          themeAssetTypes,
		"preview_ttl_seconds":  int(previewTokenTTL.Seconds()),
	}
}

// themePermissions reports what the caller may do, so the studio can hide actions rather than let
// them fail.
func (s *Server) themePermissions(r *http.Request) map[string]bool {
	return map[string]bool{
		"can_read":    s.can(r, authn.PermThemeRead),
		"can_write":   s.can(r, authn.PermThemeWrite),
		"can_publish": s.can(r, authn.PermThemePublish),
	}
}

// auditTheme records a theme action.
//
// It is a best-effort audit on purpose: a theme publish that succeeded must not be reported as failed
// because the audit row could not be written, but a theme publish that leaves no trace is a gap the
// operator will notice at exactly the wrong time. The compromise is to log the failure loudly and
// return success, which is the same trade the request log makes.
//
// It runs in its own transaction because the theme store's writes have already committed by the time
// it is called: passing a nil transaction would be a nil-pointer panic inside the recorder rather
// than "record it outside a transaction".
func (s *Server) auditTheme(r *http.Request, action, targetType, targetID string, changes map[string]any) {
	if s.pool == nil {
		return
	}
	auditor := s.auditorFor(r, "")
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		return auditor.RecordAction(ctx, tx, action, targetType, targetID, changes)
	})
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "record a theme audit action",
			"action", action, "target", targetID, "error", err)
	}
}

// themeError maps a theme error to a stable problem code.
func (s *Server) themeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such revision")
	case errors.Is(err, store.ErrNotDraft):
		writeProblem(w, http.StatusConflict, "not_a_draft",
			"only a draft can be edited; restore the revision as a draft first")
	case errors.Is(err, store.ErrAssetInUse):
		writeProblem(w, http.StatusConflict, "asset_in_use",
			"a theme revision still references this asset")
	case isValidationError(err):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_document", err.Error())
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "theme request failed",
			"path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
	}
}

// isValidationError reports whether err came from the theme document validator.
//
// The validator's errors all start with the package prefix, which is the cheapest reliable signal
// available without exporting an error type for every rule.
func isValidationError(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "theme: ")
}

// serveImmutable writes a revision-addressed body with the immutable cache policy.
func serveImmutable(w http.ResponseWriter, r *http.Request, contentType string, body []byte) {
	etag := `"` + theme.DigestBytes(body) + `"`
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", etag)
	// Revision-addressed: the URL changes when the bytes change, so a year is safe and the second
	// navigation never pays for the stylesheet again.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(body)
}

// shortName derives a PWA short name from the display name.
func shortName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Billing"
	}
	if fields := strings.Fields(name); len(fields) > 1 && len(fields[len(fields)-1]) <= 12 {
		// "Acme Hosting" → "Hosting", which is what an operator expects on a home screen. A single
		// long word is truncated instead.
		return fields[len(fields)-1]
	}
	if len([]rune(name)) > 12 {
		return string([]rune(name)[:12])
	}
	return name
}

// containsString reports whether list holds value.
func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
