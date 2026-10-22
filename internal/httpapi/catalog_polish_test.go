package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// assertProblemCode checks that a response is problem+json carrying the code a client branches
// on. Every error in this API is one of these, so the shape is asserted rather than the prose.
func assertProblemCode(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v (%s)", err, rec.Body)
	}
	if problem.Code != want {
		t.Fatalf("code = %q, want %q", problem.Code, want)
	}
}

// The catalogue-polish endpoints: duplicating a product, ordering its plans, and the rich
// description. What these tests prove is the part a unit test cannot: that the routes are
// registered, guarded by `catalog:write`, that their errors are problem+json with a stable
// code, and that the shapes the panel reads carry the new fields.

// seedCatalogProduct creates a category, a product with one price and one config option, and
// returns the product and price ids. (The service-lifecycle tests have their own
// seedPricedProduct, which is about a customer's configurable service.)
func (e *staffEnv) seedCatalogProduct(t *testing.T, slug string) (productID, priceID string) {
	t.Helper()
	ctx := context.Background()
	var categoryID string
	if err := e.pool.QueryRow(ctx,
		`INSERT INTO categories (slug, name) VALUES ($1, $1) RETURNING id`, "cat-"+slug).
		Scan(&categoryID); err != nil {
		t.Fatalf("category: %v", err)
	}
	if err := e.pool.QueryRow(ctx,
		`INSERT INTO products (slug, name, category_id) VALUES ($1, $1, $2) RETURNING id`,
		slug, categoryID).Scan(&productID); err != nil {
		t.Fatalf("product: %v", err)
	}
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO price_versions (product_id, version, name, currency, amount_minor, interval,
			anchor_day, anchor_timezone) VALUES ($1, 1, 'Monthly', 'USD', 1000, 'monthly', 1, 'UTC')
		RETURNING id`, productID).Scan(&priceID); err != nil {
		t.Fatalf("price: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO config_options (product_id, name, option_type, choices)
		VALUES ($1, 'RAM', 'dropdown', '["8 GB"]'::jsonb)`, productID); err != nil {
		t.Fatalf("option: %v", err)
	}
	return productID, priceID
}

func TestDuplicateProductRequiresCatalogWrite(t *testing.T) {
	env := newStaffEnv(t)
	productID, _ := env.seedCatalogProduct(t, "dup-perm")

	// Billing may read the catalog and issue invoices, and still may not copy a product: a copy
	// is a new thing for sale, which is a catalogue decision.
	rec := env.postJSON(t, "/api/v1/staff/catalog/products/"+productID+"/duplicate",
		env.billing, env.billingCSRF, map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("billing duplicate = %d, want 403", rec.Code)
	}
}

func TestDuplicateProductReturnsTheCopy(t *testing.T) {
	env := newStaffEnv(t)
	productID, _ := env.seedCatalogProduct(t, "dup-ok")

	rec := env.postJSON(t, "/api/v1/staff/catalog/products/"+productID+"/duplicate",
		env.admin, env.adminCSRF, map[string]any{})
	if rec.Code != http.StatusCreated {
		t.Fatalf("duplicate = %d, want 201: %s", rec.Code, rec.Body)
	}
	var body struct {
		ID     string `json:"id"`
		Slug   string `json:"slug"`
		Name   string `json:"name"`
		Hidden bool   `json:"hidden"`
		Prices []struct {
			ID        string `json:"id"`
			Interval  string `json:"interval"`
			Recurring bool   `json:"recurring"`
			SortOrder int    `json:"sort_order"`
		} `json:"prices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if body.ID == "" || body.ID == productID {
		t.Fatalf("copy id = %q, want a new product", body.ID)
	}
	if body.Slug != "dup-ok-copy" || body.Name != "dup-ok (copy)" {
		t.Fatalf("copy = %q / %q, want a disambiguated slug and name", body.Slug, body.Name)
	}
	if !body.Hidden {
		t.Fatal("the copy must be hidden: duplicating produces a draft")
	}
	if len(body.Prices) != 1 || !body.Prices[0].Recurring {
		t.Fatalf("copy plans = %+v, want one recurring plan", body.Prices)
	}

	// The audit row names the source, inside the same transaction that made the copy.
	var target string
	var changes []byte
	if err := env.pool.QueryRow(context.Background(), `
		SELECT target_id, changes FROM audit_events
		 WHERE action = 'catalog.product.duplicate'
		 ORDER BY created_at DESC LIMIT 1`).Scan(&target, &changes); err != nil {
		t.Fatalf("audit: %v", err)
	}
	if target != productID {
		t.Fatalf("audit target = %q, want the source product %q", target, productID)
	}
	if !strings.Contains(string(changes), productID) {
		t.Fatalf("audit changes do not name the source: %s", changes)
	}
}

func TestDuplicateMissingProductIs404Problem(t *testing.T) {
	env := newStaffEnv(t)
	rec := env.postJSON(t, "/api/v1/staff/catalog/products/00000000-0000-0000-0000-000000000000/duplicate",
		env.admin, env.adminCSRF, map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("duplicate missing = %d, want 404", rec.Code)
	}
	assertProblemCode(t, rec, "not_found")
}

func TestPlanOrderIsWrittenAndValidated(t *testing.T) {
	env := newStaffEnv(t)
	productID, firstPriceID := env.seedCatalogProduct(t, "order-plans")
	ctx := context.Background()

	// A second plan, appended.
	rec := env.postJSON(t, "/api/v1/staff/catalog/products/"+productID+"/prices",
		env.admin, env.adminCSRF, map[string]any{
			"name": "Annual", "currency": "USD", "amount_minor": 10000,
			"interval": "annual", "anchor_day": 1, "anchor_timezone": "UTC",
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("add price = %d: %s", rec.Code, rec.Body)
	}
	var second struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode price: %v", err)
	}

	// Reverse them.
	rec = env.putJSON(t, "/api/v1/staff/catalog/products/"+productID+"/plans/order",
		env.admin, env.adminCSRF, map[string]any{"plan_ids": []string{second.ID, firstPriceID}})
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder = %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Data []struct {
			ID        string `json:"id"`
			SortOrder int    `json:"sort_order"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data) != 2 || body.Data[0].ID != second.ID || body.Data[0].SortOrder != 0 {
		t.Fatalf("plan order = %+v, want the annual plan first", body.Data)
	}

	// A partial order is refused with a stable code rather than half-applied.
	rec = env.putJSON(t, "/api/v1/staff/catalog/products/"+productID+"/plans/order",
		env.admin, env.adminCSRF, map[string]any{"plan_ids": []string{firstPriceID}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("partial order = %d, want 422: %s", rec.Code, rec.Body)
	}
	assertProblemCode(t, rec, "invalid_input")

	// And the storefront reads the operator's order.
	var sortOrders []int
	rows, err := env.pool.Query(ctx, `
		SELECT sort_order FROM price_versions WHERE product_id = $1 ORDER BY sort_order`, productID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		sortOrders = append(sortOrders, n)
	}
	if len(sortOrders) != 2 || sortOrders[0] != 0 || sortOrders[1] != 1 {
		t.Fatalf("sort orders = %v, want 0 and 1", sortOrders)
	}
}

// A rich description is stored sanitised, and the public read says which format it is in, so a
// renderer knows whether to parse it or to escape it.
func TestRichDescriptionRoundTripsThroughTheAPI(t *testing.T) {
	env := newStaffEnv(t)
	productID, _ := env.seedCatalogProduct(t, "rich-api")

	// The PUT replaces the product, so the category has to be sent back: omitting it would
	// ungroup the product and the storefront read would (correctly) answer 404.
	var categoryID string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT category_id::text FROM products WHERE id = $1`, productID).Scan(&categoryID); err != nil {
		t.Fatalf("category: %v", err)
	}

	hostile := `<h2>Specs</h2><p onclick="steal()">Fast</p><script>alert(1)</script>` +
		`<img src=x onerror=alert(2)><a href="javascript:alert(3)">click</a>`
	rec := env.putJSON(t, "/api/v1/staff/catalog/products/"+productID,
		env.admin, env.adminCSRF, map[string]any{
			"slug": "rich-api", "name": "rich-api", "category_id": categoryID,
			"description": hostile, "description_format": "html",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", rec.Code, rec.Body)
	}

	// The staff read returns the sanitised value, not the payload.
	rec = env.get(t, "/api/v1/staff/catalog/products/"+productID, env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d", rec.Code)
	}
	lower := strings.ToLower(rec.Body.String())
	for _, bad := range []string{"onclick", "onerror", "<script", "javascript:", "alert("} {
		if strings.Contains(lower, bad) {
			t.Fatalf("the staff read returned %q: %s", bad, rec.Body)
		}
	}
	var staffBody struct {
		Description       string `json:"description"`
		DescriptionFormat string `json:"description_format"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &staffBody); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if staffBody.DescriptionFormat != "html" || !strings.Contains(staffBody.Description, "<h2>Specs</h2>") {
		t.Fatalf("description = (%q, %q)", staffBody.Description, staffBody.DescriptionFormat)
	}

	// The public read carries the format too: a visitor's renderer has to know whether to
	// parse the text as markup or to show it as text.
	rec = env.publicGet(t, "/api/v1/catalog/rich-api")
	if rec.Code != http.StatusOK {
		t.Fatalf("public get = %d: %s", rec.Code, rec.Body)
	}
	var publicBody struct {
		DescriptionFormat string `json:"description_format"`
		Prices            []struct {
			Recurring bool `json:"recurring"`
		} `json:"prices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &publicBody); err != nil {
		t.Fatalf("decode public: %v", err)
	}
	if publicBody.DescriptionFormat != "html" {
		t.Fatalf("public description_format = %q, want html", publicBody.DescriptionFormat)
	}
	if len(publicBody.Prices) != 1 || !publicBody.Prices[0].Recurring {
		t.Fatalf("public prices = %+v, want a recurring plan", publicBody.Prices)
	}
}

// A one-time price is accepted by the API and a free price with an amount is refused with a
// stable code, so the panel can say why.
func TestPlanTypesOverTheAPI(t *testing.T) {
	env := newStaffEnv(t)
	productID, _ := env.seedCatalogProduct(t, "types-api")

	rec := env.postJSON(t, "/api/v1/staff/catalog/products/"+productID+"/prices",
		env.admin, env.adminCSRF, map[string]any{
			"name": "Setup", "currency": "USD", "amount_minor": 5000,
			"interval": "one_time", "anchor_day": 1, "anchor_timezone": "UTC",
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("one-time price = %d: %s", rec.Code, rec.Body)
	}

	rec = env.postJSON(t, "/api/v1/staff/catalog/products/"+productID+"/prices",
		env.admin, env.adminCSRF, map[string]any{
			"name": "Free but not", "currency": "USD", "amount_minor": 100,
			"interval": "free", "anchor_day": 1, "anchor_timezone": "UTC",
		})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("free price with an amount = %d, want 422: %s", rec.Code, rec.Body)
	}
	assertProblemCode(t, rec, "invalid_input")

	rec = env.postJSON(t, "/api/v1/staff/catalog/products/"+productID+"/prices",
		env.admin, env.adminCSRF, map[string]any{
			"name": "Weekly", "currency": "USD", "amount_minor": 100,
			"interval": "weekly", "anchor_day": 1, "anchor_timezone": "UTC",
		})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown interval = %d, want 422: %s", rec.Code, rec.Body)
	}
	assertProblemCode(t, rec, "invalid_input")
}
