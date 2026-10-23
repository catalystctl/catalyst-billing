package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The storefront's merchandising surface over HTTP: the category strip, a category page served
// from the product URL, stock and options on a product, the currency the visitor picked, and the
// one write route that edits them.
//
// These prove the parts a store test cannot: that the routes are registered and guarded, that the
// public shapes carry the fields the storefront renders and no field it must not see, and that a
// refusal arrives as application/problem+json with a stable code.

// seedCategoryImage uploads a small image through the staff route and returns its id.
func seedCategoryImage(t *testing.T, env *staffEnv) string {
	t.Helper()
	rec := env.postRaw(t, "/api/v1/staff/catalog/images", env.admin, env.adminCSRF,
		"image/png", []byte("\x89PNG\r\n\x1a\ncategory-logo"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload image = %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode image: %v", err)
	}
	return body.ID
}

// TestPublicCatalogListsTopLevelBranchesWithProducts is SF-01: the strip is the roots that have
// something under them, ordered by sort, and an empty branch is not offered.
func TestPublicCatalogListsTopLevelBranchesWithProducts(t *testing.T) {
	env := newStaffEnv(t)
	ctx := context.Background()

	// A root with a product two levels down.
	games := env.seedTreeCategory(t, "sf-games", nil)
	minecraft := env.seedTreeCategory(t, "sf-minecraft", &games)
	env.seedTreeProduct(t, "sf-minecraft-product", minecraft)
	// A root with a product of its own, sorted after the games branch.
	hosting := env.seedTreeCategory(t, "sf-hosting", nil)
	if _, err := env.pool.Exec(ctx, `UPDATE categories SET sort_order = 10 WHERE id = $1`, hosting); err != nil {
		t.Fatal(err)
	}
	env.seedTreeProduct(t, "sf-hosting-product", hosting)
	// A root that exists only to group an empty child: clicking it would show nothing.
	empty := env.seedTreeCategory(t, "sf-empty", nil)
	env.seedTreeCategory(t, "sf-empty-child", &empty)

	rec := env.publicGet(t, "/api/v1/catalog")
	if rec.Code != http.StatusOK {
		t.Fatalf("public catalog = %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Categories []struct {
			Slug        string `json:"slug"`
			Depth       int    `json:"depth"`
			ProductCnt  int    `json:"product_count"`
			Description string `json:"description"`
			Children    []struct {
				Slug string `json:"slug"`
			} `json:"children"`
		} `json:"categories"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var slugs []string
	for _, c := range body.Categories {
		slugs = append(slugs, c.Slug)
	}
	want := []string{"sf-games", "sf-hosting"}
	if strings.Join(slugs, ",") != strings.Join(want, ",") {
		t.Fatalf("strip = %v, want %v (top level, non-empty, ordered by sort)", slugs, want)
	}
	if body.Categories[0].ProductCnt != 1 {
		t.Fatalf("the games branch count = %d, want 1 (the child's product)",
			body.Categories[0].ProductCnt)
	}
	if len(body.Categories[0].Children) != 1 || body.Categories[0].Children[0].Slug != "sf-minecraft" {
		t.Fatalf("children = %+v", body.Categories[0].Children)
	}

	// The setting is the escape hatch: a flat shop asks for every visible category.
	if rec := env.putJSON(t, "/api/v1/staff/settings", env.admin, env.adminCSRF,
		map[string]any{"group": "storefront", "values": map[string]any{"home_categories": "all"}}); rec.Code != http.StatusOK {
		t.Fatalf("set home_categories = %d: %s", rec.Code, rec.Body)
	}
	rec = env.publicGet(t, "/api/v1/catalog")
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Categories) != 3 {
		t.Fatalf("mode=all strip = %d entries, want 3 (both roots and the child): %s",
			len(body.Categories), rec.Body)
	}
}

// TestPublicSlugResolvesACategoryPage is SF-02 and SF-22: a category has a shareable URL, and the
// page lists its children and the products of its whole branch.
func TestPublicSlugResolvesACategoryPage(t *testing.T) {
	env := newStaffEnv(t)
	ctx := context.Background()
	image := seedCategoryImage(t, env)

	parent := env.seedTreeCategory(t, "sf-gpu", nil)
	if _, err := env.pool.Exec(ctx, `
		UPDATE categories SET description = $2, image_id = $3, sort_order = 3 WHERE id = $1`,
		parent, "Accelerators for training", image); err != nil {
		t.Fatal(err)
	}
	child := env.seedTreeCategory(t, "sf-gpu-4090", &parent)
	env.seedTreeProduct(t, "sf-gpu-4090-product", child)

	rec := env.publicGet(t, "/api/v1/catalog/sf-gpu")
	if rec.Code != http.StatusOK {
		t.Fatalf("category page = %d: %s", rec.Code, rec.Body)
	}
	var page struct {
		Kind     string `json:"kind"`
		Category struct {
			Slug        string `json:"slug"`
			Description string `json:"description"`
			ImageURL    string `json:"image_url"`
			Path        string `json:"path"`
			Children    []struct {
				Slug string `json:"slug"`
			} `json:"children"`
		} `json:"category"`
		Products []struct {
			Slug string `json:"slug"`
		} `json:"products"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Kind != "category" {
		t.Fatalf("kind = %q, want \"category\" so the storefront renders the right page", page.Kind)
	}
	if page.Category.Slug != "sf-gpu" || page.Category.Description != "Accelerators for training" {
		t.Fatalf("category = %+v", page.Category)
	}
	if page.Category.ImageURL != "/api/v1/media/"+image {
		t.Fatalf("category image_url = %q, want the uploaded image", page.Category.ImageURL)
	}
	if page.Category.Path != "sf-gpu" {
		t.Fatalf("category path = %q", page.Category.Path)
	}
	if len(page.Category.Children) != 1 || page.Category.Children[0].Slug != "sf-gpu-4090" {
		t.Fatalf("children = %+v", page.Category.Children)
	}
	if len(page.Products) != 1 || page.Products[0].Slug != "sf-gpu-4090-product" {
		t.Fatalf("products = %+v, want the child's product", page.Products)
	}

	// A product slug still resolves to a product, and says so.
	rec = env.publicGet(t, "/api/v1/catalog/sf-gpu-4090-product")
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode product: %v", err)
	}
	if page.Kind != "product" {
		t.Fatalf("product kind = %q", page.Kind)
	}
	// A slug that is neither is still a 404, not an empty category page.
	if rec := env.publicGet(t, "/api/v1/catalog/not-a-thing"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown slug = %d, want 404", rec.Code)
	}
}

// TestPublicProductCarriesStockAndOptions is SF-03 and SF-06: a buyer can see what is left, and a
// product whose plan has a required option is actually buyable.
func TestPublicProductCarriesStockAndOptions(t *testing.T) {
	env := newStaffEnv(t)
	ctx := context.Background()
	cat := env.seedTreeCategory(t, "sf-vps", nil)
	product := env.seedTreeProduct(t, "sf-vps-small", cat)

	var priceID string
	if err := env.pool.QueryRow(ctx,
		`SELECT id FROM price_versions WHERE product_id = $1`, product).Scan(&priceID); err != nil {
		t.Fatal(err)
	}
	var optionID string
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO config_options (product_id, name, option_type, choices, required)
		VALUES ($1, 'Memory', 'dropdown', '["8 GB","16 GB"]'::jsonb, true) RETURNING id`,
		product).Scan(&optionID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO config_option_prices (config_option_id, price_version_id, choice, amount_minor)
		VALUES ($1, $2, '16 GB', 700)`, optionID, priceID); err != nil {
		t.Fatal(err)
	}
	// Three left, featured, and a server assigned: the last one must never reach a visitor.
	stock := 3
	if rec := env.putJSON(t, "/api/v1/staff/catalog/products/"+product+"/merchandising",
		env.admin, env.adminCSRF, map[string]any{
			"stock": stock, "per_user_limit": 2,
			"allow_quantity": "separated", "featured": true,
		}); rec.Code != http.StatusOK {
		t.Fatalf("set merchandising = %d: %s", rec.Code, rec.Body)
	}
	var serverID string
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO servers (slug, name, provider, region) VALUES ('sf-node', 'SF node', 'pterodactyl', 'us-west')
		RETURNING id`).Scan(&serverID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx, `UPDATE products SET server_id = $2 WHERE id = $1`, product, serverID); err != nil {
		t.Fatal(err)
	}

	rec := env.publicGet(t, "/api/v1/catalog/sf-vps-small")
	if rec.Code != http.StatusOK {
		t.Fatalf("product page = %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Kind         string `json:"kind"`
		Featured     bool   `json:"featured"`
		Stock        *int   `json:"stock"`
		InStock      bool   `json:"in_stock"`
		LowStock     bool   `json:"low_stock"`
		AllowQty     string `json:"allow_quantity"`
		ConfigOption []struct {
			Name     string   `json:"name"`
			Type     string   `json:"type"`
			Required bool     `json:"required"`
			Choices  []string `json:"choices"`
			Prices   []struct {
				PriceID string `json:"price_id"`
				Choice  string `json:"choice"`
				Amount  struct {
					Minor int64 `json:"minor"`
				} `json:"amount"`
			} `json:"prices"`
		} `json:"config_options"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Stock == nil || *body.Stock != 3 || !body.InStock || !body.LowStock {
		t.Fatalf("stock = %+v in_stock=%v low_stock=%v", body.Stock, body.InStock, body.LowStock)
	}
	if !body.Featured {
		t.Fatal("the featured flag did not reach the storefront")
	}
	if body.AllowQty != "separated" {
		t.Fatalf("allow_quantity = %q", body.AllowQty)
	}
	if len(body.ConfigOption) != 1 {
		t.Fatalf("config options = %+v", body.ConfigOption)
	}
	option := body.ConfigOption[0]
	if option.Name != "Memory" || !option.Required || option.Type != "dropdown" {
		t.Fatalf("option = %+v", option)
	}
	if len(option.Prices) != 1 || option.Prices[0].Choice != "16 GB" ||
		option.Prices[0].Amount.Minor != 700 || option.Prices[0].PriceID != priceID {
		t.Fatalf("option prices = %+v", option.Prices)
	}

	// A node name is infrastructure: it tells a visitor which host a customer lands on.
	for _, leak := range []string{"sf-node", "SF node", "us-west", "server_name"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Fatalf("the public product leaked %q:\n%s", leak, rec.Body)
		}
	}

	// Sold out: still listed, still honest, and never in stock.
	if rec := env.putJSON(t, "/api/v1/staff/catalog/products/"+product+"/merchandising",
		env.admin, env.adminCSRF, map[string]any{"stock": 0, "featured": true}); rec.Code != http.StatusOK {
		t.Fatalf("sell out = %d: %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(env.publicGet(t, "/api/v1/catalog/sf-vps-small").Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.InStock {
		t.Fatal("a sold-out product reported in_stock")
	}
	if body.Stock == nil || *body.Stock != 0 {
		t.Fatalf("stock = %v, want 0", body.Stock)
	}
	if body.LowStock {
		t.Fatal("a sold-out product is out of stock, not low on stock")
	}
}

// TestPublicCatalogFiltersByCurrency is SF-18 / CUR-03: the currency the visitor picked decides
// what is offered, and a product sold in another currency is not quietly converted.
func TestPublicCatalogFiltersByCurrency(t *testing.T) {
	env := newStaffEnv(t)
	ctx := context.Background()
	cat := env.seedTreeCategory(t, "sf-money", nil)
	dollars := env.seedTreeProduct(t, "sf-usd-only", cat)
	both := env.seedTreeProduct(t, "sf-both", cat)
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO price_versions (product_id, version, name, currency, amount_minor, interval,
		                            anchor_day, anchor_timezone)
		VALUES ($1, 2, 'Monthly EUR', 'EUR', 900, 'monthly', 1, 'UTC')`, both); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx,
		`UPDATE price_versions SET currency = 'USD' WHERE product_id = $1`, dollars); err != nil {
		t.Fatal(err)
	}

	rec := env.publicGet(t, "/api/v1/catalog?currency=EUR")
	if rec.Code != http.StatusOK {
		t.Fatalf("EUR catalog = %d: %s", rec.Code, rec.Body)
	}
	var all struct {
		Currency   string `json:"currency"`
		Categories []struct {
			Slug       string `json:"slug"`
			ProductCnt int    `json:"product_count"`
		} `json:"categories"`
		Products []struct {
			Slug   string `json:"slug"`
			Prices []struct {
				Currency string `json:"currency"`
				Amount   struct {
					Minor int64 `json:"minor"`
				} `json:"amount"`
			} `json:"prices"`
		} `json:"products"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if all.Currency != "EUR" {
		t.Fatalf("currency = %q", all.Currency)
	}
	if len(all.Products) != 1 || all.Products[0].Slug != "sf-both" {
		t.Fatalf("EUR products = %+v, want only sf-both", all.Products)
	}
	if len(all.Products[0].Prices) != 1 || all.Products[0].Prices[0].Currency != "EUR" ||
		all.Products[0].Prices[0].Amount.Minor != 900 {
		t.Fatalf("EUR prices = %+v, want only the 900 EUR row", all.Products[0].Prices)
	}
	if len(all.Categories) != 1 || all.Categories[0].ProductCnt != 1 {
		t.Fatalf("the strip was not pruned to the currency: %+v", all.Categories)
	}
	// The category page obeys the same choice, so it is not a second shop.
	rec = env.publicGet(t, "/api/v1/catalog/sf-money?currency=EUR")
	var page struct {
		Kind     string `json:"kind"`
		Products []struct {
			Slug string `json:"slug"`
		} `json:"products"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode category page: %v", err)
	}
	if page.Kind != "category" || len(page.Products) != 1 || page.Products[0].Slug != "sf-both" {
		t.Fatalf("EUR category page = %+v", page)
	}
	// And a product sold only in the other currency is not at a EUR URL.
	if rec := env.publicGet(t, "/api/v1/catalog/sf-usd-only?currency=EUR"); rec.Code != http.StatusNotFound {
		t.Fatalf("USD-only product at a EUR URL = %d, want 404", rec.Code)
	}
}

// TestSetMerchandisingIsGuardedAndAudited covers the one write route: catalog:write, a validating
// refusal with a stable code, and an audit row in the same transaction as the change.
func TestSetMerchandisingIsGuardedAndAudited(t *testing.T) {
	env := newStaffEnv(t)
	cat := env.seedTreeCategory(t, "sf-guard", nil)
	product := env.seedTreeProduct(t, "sf-guard-product", cat)
	path := "/api/v1/staff/catalog/products/" + product + "/merchandising"

	// Billing holds invoice:read:all and not catalog:write, so the catalog is read-only to it.
	if rec := env.putJSON(t, path, env.billing, env.billingCSRF, map[string]any{"stock": 1}); rec.Code != http.StatusForbidden {
		t.Fatalf("billing merchandising write = %d, want 403", rec.Code)
	}
	// A mutation without an Idempotency-Key is refused before the handler runs.
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(`{"stock":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(env.admin)
	req.Header.Set("X-CSRF-Token", env.adminCSRF)
	if rec := env.doRequest(t, req); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing idempotency key = %d, want 400", rec.Code)
	}
	// A refusal names a stable code and is problem+json.
	rec := env.putJSON(t, path, env.admin, env.adminCSRF,
		map[string]any{"stock": 1, "allow_quantity": "unlimited"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad quantity policy = %d, want 422: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Code != "invalid_input" {
		t.Fatalf("code = %q, want invalid_input", problem.Code)
	}
	// A negative stock is refused too, and never reaches the database.
	if rec := env.putJSON(t, path, env.admin, env.adminCSRF, map[string]any{"stock": -2}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("negative stock = %d, want 422", rec.Code)
	}

	rec = env.putJSON(t, path, env.admin, env.adminCSRF, map[string]any{
		"stock": 7, "per_user_limit": 2, "allow_quantity": "combined", "featured": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("merchandising write = %d: %s", rec.Code, rec.Body)
	}
	var stored struct {
		Stock         *int   `json:"stock"`
		PerUserLimit  *int   `json:"per_user_limit"`
		AllowQuantity string `json:"allow_quantity"`
		Featured      bool   `json:"featured"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stored.Stock == nil || *stored.Stock != 7 || stored.AllowQuantity != "combined" || !stored.Featured {
		t.Fatalf("stored = %+v", stored)
	}

	var actions int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_events
		 WHERE action = 'catalog.product.merchandising' AND target_id = $1`, product).Scan(&actions); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if actions == 0 {
		t.Fatal("no audit row was written for the merchandising change")
	}
}

// TestCategoryImageViaTheCatalogAPI is CAT-01 / SF-06 through the routes the panel uses: a
// category is created with an image and the storefront read carries it.
func TestCategoryImageViaTheCatalogAPI(t *testing.T) {
	env := newStaffEnv(t)
	image := seedCategoryImage(t, env)

	rec := env.postJSON(t, "/api/v1/staff/catalog/categories", env.admin, env.adminCSRF, map[string]any{
		"slug": "sf-imaged", "name": "Imaged", "description": "With a logo",
		"image_id": image, "sort_order": 4, "hidden": false,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create category = %d: %s", rec.Code, rec.Body)
	}
	var created struct {
		ID       string  `json:"id"`
		ImageID  *string `json:"image_id"`
		SortOrd  int     `json:"sort_order"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.ImageID == nil || *created.ImageID != image {
		t.Fatalf("created image = %v, want %s", created.ImageID, image)
	}

	rec = env.publicGet(t, "/api/v1/catalog/categories")
	if rec.Code != http.StatusOK {
		t.Fatalf("public tree = %d: %s", rec.Code, rec.Body)
	}
	var tree struct {
		Categories []struct {
			Slug     string `json:"slug"`
			ImageURL string `json:"image_url"`
		} `json:"categories"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tree); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, c := range tree.Categories {
		if c.Slug == "sf-imaged" {
			if c.ImageURL != "/api/v1/media/"+image {
				t.Fatalf("category image_url = %q", c.ImageURL)
			}
			return
		}
	}
	t.Fatalf("the category is missing from the public tree: %+v", tree.Categories)
}
