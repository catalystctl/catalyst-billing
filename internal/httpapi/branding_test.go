package httpapi_test

// The tenant's brand overlay over HTTP (E6.10).
//
// The properties these tests assert are the ones a screenshot review cannot see:
//
//   - an installation that has never opened the branding screen serves an *empty* brand, and
//     the derived ink falls back to the built-in one — that is what makes "unset" a fallback
//     rather than a lookalike;
//   - a write is normalised, so there is exactly one spelling of a colour in the store and the
//     panel's comparison against its own token cannot be fooled by case;
//   - the logo is stored, served from the media endpoint the catalog already has, and refuses
//     an SVG with a reason instead of accepting a logo that no invoice could print;
//   - the invoice PDF is branded when a brand is configured and unbranded when it is not.
//
// The last one is the reason this feature exists: a brand that reaches the panel and not the
// document is a brand the customer never sees.

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// brandResponse is the shape the public read returns.
type brandResponse struct {
	Name        string  `json:"name"`
	AccentLight string  `json:"accent_light"`
	AccentDark  string  `json:"accent_dark"`
	LogoID      string  `json:"logo_id"`
	LogoURL     string  `json:"logo_url"`
	UpdatedAt   *string `json:"updated_at"`
	Resolved    struct {
		Light struct {
			Ink      string  `json:"ink"`
			Text     string  `json:"text"`
			Contrast float64 `json:"contrast"`
			Legible  bool    `json:"legible"`
			Set      bool    `json:"set"`
		} `json:"light"`
		Dark struct {
			Ink      string  `json:"ink"`
			Text     string  `json:"text"`
			Contrast float64 `json:"contrast"`
			Legible  bool    `json:"legible"`
			Set      bool    `json:"set"`
		} `json:"dark"`
	} `json:"resolved"`
}

// getBrand reads the public brand.
func (e *authEnv) getBrand(t *testing.T) brandResponse {
	t.Helper()
	rec := e.getDoc(t, "/api/v1/branding", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/branding: status %d; body %s", rec.Code, rec.Body)
	}
	var out brandResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("brand body: %v (%s)", err, rec.Body)
	}
	return out
}

// putBrand writes the branding group.
func (e *authEnv) putBrand(t *testing.T, cookie *http.Cookie, csrf string, values map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return e.doJSON(t, http.MethodPut, "/api/v1/staff/branding", cookie, csrf,
		map[string]any{"values": values})
}

// uploadLogo posts raw image bytes.
func (e *authEnv) uploadLogo(t *testing.T, cookie *http.Cookie, csrf, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/staff/branding/logo", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	req.Header.Set("Idempotency-Key", "brand-logo-"+time.Now().Format("150405.000000000"))
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// brandPNG builds a small PNG so the test carries no binary fixture.
func brandPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 0x1e, G: 0x88, B: 0xe5, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// An installation that never configured a brand serves nothing, and says so.
//
// "Nothing" has to be an empty string rather than the built-in colour: the panel decides
// whether to emit an override by comparing against "", so a response that repeated the
// default would make an unbranded installation take the branded code path with a lookalike
// value — and the byte-identical fallback could never be proven.
func TestUnsetBrandServesNothingAndFallsBack(t *testing.T) {
	env := newStaffEnv(t)

	brand := env.getBrand(t)
	if brand.Name != "" || brand.AccentLight != "" || brand.AccentDark != "" || brand.LogoID != "" {
		t.Fatalf("an unconfigured brand returned values: %+v", brand)
	}
	if brand.LogoURL != "" {
		t.Fatalf("an unconfigured brand returned a logo url: %q", brand.LogoURL)
	}
	if brand.Resolved.Light.Set || brand.Resolved.Dark.Set {
		t.Fatal("an unconfigured brand reports an accent as set")
	}
	// The derived ink is the built-in one, so a client can use it unconditionally without
	// knowing whether a brand exists.
	if brand.Resolved.Light.Ink != "#ffffff" {
		t.Fatalf("light ink = %q, want the built-in #ffffff", brand.Resolved.Light.Ink)
	}
	if brand.Resolved.Dark.Ink != "#0a0d13" {
		t.Fatalf("dark ink = %q, want the built-in #0a0d13", brand.Resolved.Dark.Ink)
	}
	// And the public read is cacheable: it is on the storefront's critical path.
	rec := env.getDoc(t, "/api/v1/branding", nil)
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "public") {
		t.Fatalf("Cache-Control = %q, want a public cache", got)
	}
}

// A brand write round-trips, is normalised, and reaches the public read.
func TestBrandWriteRoundTripsAndIsNormalised(t *testing.T) {
	env := newStaffEnv(t)

	rec := env.putBrand(t, env.admin, env.adminCSRF, map[string]string{
		"name":         "  Acme Hosting  ",
		"accent_light": "#ABC",
		"accent_dark":  "7B95FF",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("save: status %d; body %s", rec.Code, rec.Body)
	}

	brand := env.getBrand(t)
	if brand.Name != "Acme Hosting" {
		t.Fatalf("name = %q, want the trimmed value", brand.Name)
	}
	// Both spellings are stored in the one canonical form, so the panel's comparison against
	// its own token is a string equality.
	if brand.AccentLight != "#aabbcc" {
		t.Fatalf("accent_light = %q, want #aabbcc", brand.AccentLight)
	}
	if brand.AccentDark != "#7b95ff" {
		t.Fatalf("accent_dark = %q, want #7b95ff", brand.AccentDark)
	}
	if !brand.Resolved.Light.Set || !brand.Resolved.Dark.Set {
		t.Fatal("a configured accent is not reported as set")
	}
	// The derivation is done on the server so the panel and the invoice cannot disagree.
	if brand.Resolved.Light.Contrast <= 0 || brand.Resolved.Light.Text == "" {
		t.Fatalf("the light accent has no derivation: %+v", brand.Resolved.Light)
	}
	if brand.UpdatedAt == nil {
		t.Fatal("a written brand has no timestamp")
	}
}

// A pale accent is a legitimate brand colour. The server reports that it is not legible on
// the panel's background and offers the adjusted relative, rather than refusing the colour.
func TestPaleAccentIsReportedAsIllegibleWithAnAdjustment(t *testing.T) {
	env := newStaffEnv(t)

	rec := env.putBrand(t, env.admin, env.adminCSRF, map[string]string{
		"accent_light": "#f1e05a",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("save: status %d; body %s", rec.Code, rec.Body)
	}
	brand := env.getBrand(t)
	if brand.Resolved.Light.Legible {
		t.Fatalf("a pale yellow is reported as legible: %+v", brand.Resolved.Light)
	}
	if brand.Resolved.Light.Text == "" || brand.Resolved.Light.Text == brand.AccentLight {
		t.Fatalf("no adjusted colour was offered: %+v", brand.Resolved.Light)
	}
	// The stored colour is the one the tenant chose: the adjustment is a rendering decision,
	// not a rewrite of their brand.
	if brand.AccentLight != "#f1e05a" {
		t.Fatalf("accent_light = %q, want the colour as chosen", brand.AccentLight)
	}
}

// An invalid value is refused before anything is written: a form that sends a good name and
// a bad colour must not leave the name applied.
func TestBrandWriteIsValidatedAsAWhole(t *testing.T) {
	env := newStaffEnv(t)

	rec := env.putBrand(t, env.admin, env.adminCSRF, map[string]string{
		"name":         "Acme Hosting",
		"accent_light": "chartreuse",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body)
	}
	if brand := env.getBrand(t); brand.Name != "" {
		t.Fatalf("the name was stored despite the invalid colour: %q", brand.Name)
	}

	// The logo is set by uploading it, not by naming an image: a form must not be able to
	// point the brand at an arbitrary row.
	rec = env.putBrand(t, env.admin, env.adminCSRF, map[string]string{
		"logo_id": "8c8d0f5a-0000-0000-0000-000000000001",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("writing logo_id: status %d, want 400; body %s", rec.Code, rec.Body)
	}
}

// The brand is configuration, so it needs the settings permission.
func TestBrandWriteNeedsSettingsPermission(t *testing.T) {
	env := newStaffEnv(t)

	// Support has no settings:write.
	rec := env.putBrand(t, env.support, env.supportCSRF, map[string]string{"name": "Acme"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("support write: status %d, want 403; body %s", rec.Code, rec.Body)
	}

	// A read is gated with the write, like the rest of the settings surface: the brand is not
	// reconnaissance, but the screen that shows it is the screen that changes it.
	rec = env.getDoc(t, "/api/v1/staff/branding", env.support)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("support read: status %d, want 403", rec.Code)
	}
	if brand := env.getBrand(t); brand.Name != "" {
		t.Fatalf("a refused write changed the brand: %q", brand.Name)
	}
}

// A logo is stored, served from the media endpoint, and cleared without destroying the image.
func TestBrandLogoUploadsServesAndClears(t *testing.T) {
	env := newStaffEnv(t)
	body := brandPNG(t, 64, 24)

	rec := env.uploadLogo(t, env.admin, env.adminCSRF, "image/png", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: status %d; body %s", rec.Code, rec.Body)
	}
	var uploaded struct {
		ID          string `json:"id"`
		ContentType string `json:"content_type"`
		ByteSize    int    `json:"byte_size"`
		URL         string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &uploaded); err != nil {
		t.Fatalf("upload body: %v (%s)", err, rec.Body)
	}
	if uploaded.ContentType != "image/png" || uploaded.ByteSize != len(body) {
		t.Fatalf("upload metadata = %+v", uploaded)
	}

	// The reference is stored, so the public read carries it.
	brand := env.getBrand(t)
	if brand.LogoID != uploaded.ID {
		t.Fatalf("logo_id = %q, want %q", brand.LogoID, uploaded.ID)
	}
	if brand.LogoURL != "/api/v1/media/"+uploaded.ID {
		t.Fatalf("logo_url = %q", brand.LogoURL)
	}

	// The media endpoint serves the exact bytes: a logo that is re-encoded on the way out is
	// a different file from the one the operator uploaded.
	media := env.getDoc(t, brand.LogoURL, nil)
	if media.Code != http.StatusOK {
		t.Fatalf("media: status %d", media.Code)
	}
	if got := media.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("media Content-Type = %q", got)
	}
	if !bytes.Equal(media.Body.Bytes(), body) {
		t.Fatal("the served logo is not the uploaded file")
	}

	// Clearing the reference leaves the image row alone: it is deduped by digest and shared
	// with the catalog, so deleting it could break a product logo made of the same file.
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/staff/branding/logo", nil)
	req.AddCookie(env.admin)
	req.Header.Set("X-CSRF-Token", env.adminCSRF)
	req.Header.Set("Idempotency-Key", "brand-logo-delete-"+time.Now().Format("150405.000000000"))
	del := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(del, req)
	if del.Code != http.StatusOK {
		t.Fatalf("delete: status %d; body %s", del.Code, del.Body)
	}
	if brand := env.getBrand(t); brand.LogoID != "" || brand.LogoURL != "" {
		t.Fatalf("the logo survived the delete: %+v", brand)
	}
	var exists bool
	if err := env.pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM product_images WHERE id = $1)`, uploaded.ID).Scan(&exists); err != nil {
		t.Fatalf("check image: %v", err)
	}
	if !exists {
		t.Fatal("clearing the brand logo deleted the stored image")
	}
}

// A logo must be a format an invoice can print. An SVG is a document that can carry a script
// and cannot be drawn by the PDF writer, so it is refused with a reason rather than accepted
// into a brand that only works in the panel.
func TestBrandLogoRefusesWhatTheInvoiceCannotPrint(t *testing.T) {
	env := newStaffEnv(t)

	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	rec := env.uploadLogo(t, env.admin, env.adminCSRF, "image/svg+xml", svg)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("svg upload: status %d, want 415; body %s", rec.Code, rec.Body)
	}

	// A file that claims to be a PNG and is not is refused too: the alternative is a brand
	// whose logo is invisible on every document with no explanation.
	rec = env.uploadLogo(t, env.admin, env.adminCSRF, "image/png", []byte("not a png at all"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("fake png: status %d, want 422; body %s", rec.Code, rec.Body)
	}

	// And an oversized upload is refused before it is decoded.
	big := make([]byte, (256<<10)+1)
	rec = env.uploadLogo(t, env.admin, env.adminCSRF, "image/png", big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload: status %d, want 413; body %s", rec.Code, rec.Body)
	}

	// Nothing was stored by any of those.
	if brand := env.getBrand(t); brand.LogoID != "" {
		t.Fatalf("a refused upload stored a logo: %q", brand.LogoID)
	}
}

// The invoice PDF uses the tenant's brand. This is the whole point of the feature: a brand
// that reaches the panel and not the document is a brand the customer never sees.
func TestInvoicePDFUsesTheTenantBrand(t *testing.T) {
	env := newDocumentEnv(t)
	env.createUser(t, "settings@example.test", "settings password 1", "administrator")
	settingsCookie, settingsCSRF := env.login(t, "settings@example.test", "settings password 1")

	// Unbranded first: the document is the one the snapshot describes, and it embeds no image.
	before := env.getDoc(t, "/api/v1/staff/invoices/"+env.alice.invoiceID+"/pdf", env.adminCookie)
	if before.Code != http.StatusOK {
		t.Fatalf("unbranded pdf: status %d; body %s", before.Code, before.Body)
	}
	if bytes.Contains(before.Body.Bytes(), []byte("/Subtype /Image")) {
		t.Fatal("an unbranded document embeds an image")
	}

	logo := brandPNG(t, 48, 16)
	up := env.uploadLogo(t, settingsCookie, settingsCSRF, "image/png", logo)
	if up.Code != http.StatusCreated {
		t.Fatalf("upload logo: status %d; body %s", up.Code, up.Body)
	}
	saved := env.putBrand(t, settingsCookie, settingsCSRF, map[string]string{
		"name":         "Acme Hosting Ltd",
		"accent_light": "#c0392b",
	})
	if saved.Code != http.StatusOK {
		t.Fatalf("save brand: status %d; body %s", saved.Code, saved.Body)
	}

	after := env.getDoc(t, "/api/v1/staff/invoices/"+env.alice.invoiceID+"/pdf", env.adminCookie)
	if after.Code != http.StatusOK {
		t.Fatalf("branded pdf: status %d; body %s", after.Code, after.Body)
	}
	body := after.Body.Bytes()
	if !bytes.HasPrefix(body, []byte("%PDF-")) {
		t.Fatal("the branded document is not a PDF")
	}
	if !bytes.Contains(body, []byte("Acme Hosting Ltd")) {
		t.Fatal("the brand name is not on the invoice")
	}
	if !bytes.Contains(body, []byte("/Subtype /Image")) {
		t.Fatal("the brand logo is not embedded in the invoice")
	}
	// The accent reaches the paper as well, not just the name.
	if !bytes.Contains(body, []byte("0.753 0.224 0.169")) {
		t.Fatal("the brand accent is not drawn on the invoice")
	}
	// The facts are untouched: branding must not move money or a document identity.
	var number string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT COALESCE(number, '') FROM invoices WHERE id = $1`, env.alice.invoiceID).
		Scan(&number); err != nil {
		t.Fatalf("read number: %v", err)
	}
	if number != "" && !bytes.Contains(body, []byte(number)) {
		t.Fatalf("the invoice number %q disappeared from the branded document", number)
	}
}

// A brand whose logo cannot be read must not break the document. The panel shows the logo
// from the media endpoint, so a missing image is a broken brand, not a broken invoice.
func TestInvoicePDFSurvivesADanglingLogoReference(t *testing.T) {
	env := newDocumentEnv(t)
	// A reference to an image that does not exist, written the way a hand edit would.
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO settings (group_key, key, value) VALUES ('branding', 'logo_id', $1)
		ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`,
		"8c8d0f5a-0000-0000-0000-0000000000ff"); err != nil {
		t.Fatalf("seed dangling logo: %v", err)
	}
	rec := env.getDoc(t, "/api/v1/staff/invoices/"+env.alice.invoiceID+"/pdf", env.adminCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	if !bytes.HasPrefix(rec.Body.Bytes(), []byte("%PDF-")) {
		t.Fatal("the document is not a PDF")
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("/Subtype /Image")) {
		t.Fatal("a logo that cannot be read was embedded anyway")
	}
}
