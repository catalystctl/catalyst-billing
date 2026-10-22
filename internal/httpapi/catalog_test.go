package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The catalog's HTTP surface has three audiences with different access, and the boundary
// between them is the whole point of these tests:
//
//   - staff write needs catalog:write (not merely invoice:read:all),
//   - the public read must never return a hidden product, a hidden category, or an unpriced one,
//   - media is public but must not serve an image as a document that can run script.

// postRaw sends a raw body with an explicit content type, for the image upload.
func (e *authEnv) postRaw(t *testing.T, path string, cookie *http.Cookie, csrf, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.AddCookie(cookie)
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	req.Header.Set("Idempotency-Key", "test-raw-"+time.Now().Format("150405.000000000"))
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// publicGet issues an unauthenticated GET: no cookie, no token.
func (e *authEnv) publicGet(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return out
}

// seedCatalog creates a category and a priced product directly, for the read tests.
func (e *staffEnv) seedCatalog(t *testing.T, slug string, hidden bool) (categoryID, productID string) {
	t.Helper()
	ctx := context.Background()
	if err := e.pool.QueryRow(ctx,
		`INSERT INTO categories (slug, name) VALUES ($1, $1) RETURNING id`, "cat-"+slug).Scan(&categoryID); err != nil {
		t.Fatalf("category: %v", err)
	}
	if err := e.pool.QueryRow(ctx,
		`INSERT INTO products (slug, name, category_id, hidden) VALUES ($1, $1, $2, $3) RETURNING id`,
		slug, categoryID, hidden).Scan(&productID); err != nil {
		t.Fatalf("product: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO price_versions (product_id, version, name, currency, amount_minor, interval,
			anchor_day, anchor_timezone) VALUES ($1, 1, 'Monthly', 'USD', 1000, 'monthly', 1, 'UTC')`,
		productID); err != nil {
		t.Fatalf("price: %v", err)
	}
	return categoryID, productID
}

// ---------------------------------------------------------------- the write path --

func TestCatalogRequiresCatalogWrite(t *testing.T) {
	env := newStaffEnv(t)
	body := map[string]any{"slug": "vps", "name": "VPS"}

	// Billing holds invoice:write:all but not catalog:write: reading the catalog for invoicing
	// must not imply the right to change what is for sale.
	rec := env.postJSON(t, "/api/v1/staff/catalog/categories", env.billing, env.billingCSRF, body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("billing create category = %d, want 403", rec.Code)
	}

	// An administrator holds it.
	rec = env.postJSON(t, "/api/v1/staff/catalog/categories", env.admin, env.adminCSRF, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin create category = %d, want 201: %s", rec.Code, rec.Body)
	}
}

func TestCatalogWriteRequiresCSRF(t *testing.T) {
	env := newStaffEnv(t)
	rec := env.postJSON(t, "/api/v1/staff/catalog/categories", env.admin, "", map[string]any{"slug": "vps", "name": "VPS"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("create category without CSRF = %d, want 403", rec.Code)
	}
}

func TestCatalogAnonymousWriteIsRefused(t *testing.T) {
	env := newStaffEnv(t)
	// No session at all. The route is staff-gated, so this must not reach the handler.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/staff/catalog/products", strings.NewReader(`{"slug":"x","name":"X"}`))
	req.Header.Set("Content-Type", "application/json")
	// A valid key, so the request fails on authentication rather than on the key's shape:
	// the key is validated before auth by design, and a short key would test the wrong guard.
	req.Header.Set("Idempotency-Key", "anonymous-catalog-write-attempt")
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("anonymous create product = %d, want 401/403", rec.Code)
	}
}

func TestCatalogDuplicateSlugIsConflict(t *testing.T) {
	env := newStaffEnv(t)
	body := map[string]any{"slug": "vps", "name": "VPS"}

	if rec := env.postJSON(t, "/api/v1/staff/catalog/categories", env.admin, env.adminCSRF, body); rec.Code != http.StatusCreated {
		t.Fatalf("first create = %d: %s", rec.Code, rec.Body)
	}
	rec := env.postJSON(t, "/api/v1/staff/catalog/categories", env.admin, env.adminCSRF, body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate slug = %d, want 409", rec.Code)
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	// The client branches on the code, so the code is the contract.
	if problem.Code != "slug_taken" {
		t.Fatalf("code = %q, want slug_taken", problem.Code)
	}
}

func TestCatalogInvalidInputIs422(t *testing.T) {
	env := newStaffEnv(t)
	cases := []map[string]any{
		{"slug": "", "name": "No slug"},
		{"slug": "Has Spaces", "name": "Bad slug"},
		{"slug": "ok", "name": ""},
	}
	for _, body := range cases {
		rec := env.postJSON(t, "/api/v1/staff/catalog/categories", env.admin, env.adminCSRF, body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%v = %d, want 422: %s", body, rec.Code, rec.Body)
		}
	}
}

// ---------------------------------------------------------------- the public read --

func TestPublicCatalogExcludesHiddenAndUnpriced(t *testing.T) {
	env := newStaffEnv(t)
	env.seedCatalog(t, "shown", false)
	env.seedCatalog(t, "hidden", true)

	// A third product in a visible category with no price at all.
	ctx := context.Background()
	var catID string
	if err := env.pool.QueryRow(ctx, `SELECT id FROM categories WHERE slug = 'cat-shown'`).Scan(&catID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO products (slug, name, category_id) VALUES ('unpriced', 'Unpriced', $1)`, catID); err != nil {
		t.Fatal(err)
	}

	rec := env.publicGet(t, "/api/v1/catalog")
	if rec.Code != http.StatusOK {
		t.Fatalf("public catalog = %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Products []struct {
			Slug string `json:"slug"`
		} `json:"products"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Products) != 1 || body.Products[0].Slug != "shown" {
		t.Fatalf("public products = %v, want only 'shown'", body.Products)
	}
}

func TestPublicCatalogHidesProvisionTargets(t *testing.T) {
	env := newStaffEnv(t)
	_, productID := env.seedCatalog(t, "shown", false)
	ctx := context.Background()
	if _, err := env.pool.Exec(ctx,
		`UPDATE price_versions SET provision_plan = 'secret-plan', provision_region = 'dc-7' WHERE product_id = $1`,
		productID); err != nil {
		t.Fatal(err)
	}

	rec := env.publicGet(t, "/api/v1/catalog")
	body := rec.Body.String()
	for _, leak := range []string{"secret-plan", "dc-7", "provision_plan", "provision_region", "active_subscriptions"} {
		if strings.Contains(body, leak) {
			t.Fatalf("the public catalog leaked %q:\n%s", leak, body)
		}
	}

	// Staff still see them, because an operator needs to know where a service lands.
	staff := env.get(t, "/api/v1/staff/catalog/products/"+productID, env.admin)
	if !strings.Contains(staff.Body.String(), "secret-plan") {
		t.Fatalf("staff lost the provision plan:\n%s", staff.Body)
	}
}

func TestPublicCatalogIsCacheable(t *testing.T) {
	env := newStaffEnv(t)
	env.seedCatalog(t, "shown", false)
	rec := env.publicGet(t, "/api/v1/catalog")
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age") {
		t.Fatalf("Cache-Control = %q, want a max-age", cc)
	}
}

func TestPublicProductBySlug(t *testing.T) {
	env := newStaffEnv(t)
	env.seedCatalog(t, "shown", false)

	rec := env.publicGet(t, "/api/v1/catalog/shown")
	if rec.Code != http.StatusOK {
		t.Fatalf("public product = %d: %s", rec.Code, rec.Body)
	}
	if rec := env.publicGet(t, "/api/v1/catalog/nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing product = %d, want 404", rec.Code)
	}
}

// ---------------------------------------------------------------- the media --

func TestMediaServesBytesWithSniffingRefused(t *testing.T) {
	env := newStaffEnv(t)
	png := []byte("\x89PNG\r\n\x1a\nfake")
	rec := env.postRaw(t, "/api/v1/staff/catalog/images", env.admin, env.adminCSRF, "image/png", png)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload = %d: %s", rec.Code, rec.Body)
	}
	var uploaded struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}

	// Media is public: an <img> on the storefront has no session.
	media := env.publicGet(t, uploaded.URL)
	if media.Code != http.StatusOK {
		t.Fatalf("media = %d", media.Code)
	}
	if got := media.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := media.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q", got)
	}
	if !bytes.Equal(media.Body.Bytes(), png) {
		t.Fatal("the served bytes differ from the uploaded bytes")
	}
	// The id names one exact byte sequence, so the cache can be immutable.
	if cc := media.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("Cache-Control = %q, want immutable", cc)
	}
}

// An SVG is a document that can carry a script. It must never be served as a plain document
// from our origin, and it must not be an attachment either — a logo that downloads instead of
// rendering is not a logo. The resolution is a sandboxing CSP.
func TestMediaSVGIsSandboxed(t *testing.T) {
	env := newStaffEnv(t)
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	rec := env.postRaw(t, "/api/v1/staff/catalog/images", env.admin, env.adminCSRF, "image/svg+xml", svg)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload = %d: %s", rec.Code, rec.Body)
	}
	var uploaded struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}

	media := env.publicGet(t, uploaded.URL)
	csp := media.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "sandbox") || !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("SVG CSP = %q, want a sandbox with default-src 'none'", csp)
	}
	if got := media.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", got)
	}
}

func TestMediaRejectsNonImageTypes(t *testing.T) {
	env := newStaffEnv(t)
	rec := env.postRaw(t, "/api/v1/staff/catalog/images", env.admin, env.adminCSRF, "text/html", []byte("<script>"))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/html upload = %d, want 415: %s", rec.Code, rec.Body)
	}
}

func TestMediaRejectsOversize(t *testing.T) {
	env := newStaffEnv(t)
	big := bytes.Repeat([]byte("x"), 256*1024+1)
	rec := env.postRaw(t, "/api/v1/staff/catalog/images", env.admin, env.adminCSRF, "image/png", big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize upload = %d, want 413: %s", rec.Code, rec.Body)
	}
}

// ---------------------------------------------------------------- the price --

// A price is appended, never edited: the response to adding a price must be version 2 while
// version 1 is still readable, because an issued invoice points at it.
func TestAddPriceAppendsAVersion(t *testing.T) {
	env := newStaffEnv(t)
	_, productID := env.seedCatalog(t, "shown", false)

	body := map[string]any{
		"name": "Annual", "currency": "USD", "amount_minor": 12000, "setup_minor": 500,
		"interval": "annual", "anchor_day": 1, "anchor_timezone": "UTC",
	}
	rec := env.postJSON(t, "/api/v1/staff/catalog/products/"+productID+"/prices", env.admin, env.adminCSRF, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add price = %d: %s", rec.Code, rec.Body)
	}
	var price struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &price); err != nil {
		t.Fatal(err)
	}
	if price.Version != 2 {
		t.Fatalf("new price version = %d, want 2 (the seeded row is version 1)", price.Version)
	}

	read := env.get(t, "/api/v1/staff/catalog/products/"+productID, env.admin)
	var product struct {
		Prices []struct {
			Version int `json:"version"`
		} `json:"prices"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &product); err != nil {
		t.Fatal(err)
	}
	if len(product.Prices) != 2 {
		t.Fatalf("prices = %d, want 2: a price is appended, never replaced", len(product.Prices))
	}
}

// The list and the detail must return the same shape.
//
// The list used to return the raw struct, so `prices[].amount` existed only in the detail
// response. A client reading a price from the list therefore rendered "—" without an error —
// which is exactly how the catalog price column and the order form failed.
func TestStaffProductListMatchesTheDetailShape(t *testing.T) {
	env := newStaffEnv(t)
	_, productID := env.seedCatalog(t, "shown", false)

	rec := env.get(t, "/api/v1/staff/catalog/products", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body)
	}
	type amount struct {
		Minor    int64  `json:"minor"`
		Currency string `json:"currency"`
		Scale    int    `json:"scale"`
	}
	var list struct {
		Data []struct {
			ID     string `json:"id"`
			Prices []struct {
				Amount amount `json:"amount"`
				Setup  amount `json:"setup"`
			} `json:"prices"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, p := range list.Data {
		if p.ID != productID {
			continue
		}
		found = true
		if len(p.Prices) != 1 {
			t.Fatalf("prices = %d, want 1", len(p.Prices))
		}
		if p.Prices[0].Amount.Minor != 1000 || p.Prices[0].Amount.Currency != "USD" ||
			p.Prices[0].Amount.Scale != 2 {
			t.Fatalf("amount = %+v, want 1000 USD at scale 2", p.Prices[0].Amount)
		}
	}
	if !found {
		t.Fatal("the seeded product is missing from the list")
	}
}
