package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The catalogue's structural routes: the category tree, moving a category, and the product →
// server assignment.
//
// What these prove is the part a store test cannot: that the routes are registered and guarded,
// that the two structural refusals arrive as problem+json with their own stable codes, and that
// the shapes the panel reads carry the fields the tree and the server selector need.

// seedTreeCategory inserts a category, optionally under a parent, and returns its id.
func (e *authEnv) seedTreeCategory(t *testing.T, slug string, parentID *string) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `
		INSERT INTO categories (slug, name, parent_id) VALUES ($1, $1, $2) RETURNING id`,
		slug, parentID).Scan(&id); err != nil {
		t.Fatalf("seed category %q: %v", slug, err)
	}
	return id
}

// seedTreeProduct inserts a priced product in a category and returns its id.
func (e *authEnv) seedTreeProduct(t *testing.T, slug, categoryID string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := e.pool.QueryRow(ctx,
		`INSERT INTO products (slug, name, category_id) VALUES ($1, $1, $2) RETURNING id`,
		slug, categoryID).Scan(&id); err != nil {
		t.Fatalf("seed product %q: %v", slug, err)
	}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO price_versions (product_id, version, name, currency, amount_minor, interval,
		                            anchor_day, anchor_timezone)
		VALUES ($1, 1, 'Monthly', 'USD', 1000, 'monthly', 1, 'UTC')`, id); err != nil {
		t.Fatalf("seed price for %q: %v", slug, err)
	}
	return id
}

func TestCategoryTreeRouteRequiresAStaffRead(t *testing.T) {
	env := newStaffEnv(t)
	env.seedTreeCategory(t, "tree-perm", nil)

	if rec := env.get(t, "/api/v1/staff/catalog/categories/tree", env.billing); rec.Code != http.StatusOK {
		t.Fatalf("billing tree read = %d, want 200 (it holds invoice:read:all)", rec.Code)
	}
	// No session at all is unauthenticated, not forbidden: the route is staff-only.
	if rec := env.get(t, "/api/v1/staff/catalog/categories/tree", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous tree read = %d, want 401", rec.Code)
	}
}

func TestMovingACategoryIsRefusedForACycleWithItsOwnCode(t *testing.T) {
	env := newStaffEnv(t)
	parent := env.seedTreeCategory(t, "move-parent", nil)
	child := env.seedTreeCategory(t, "move-child", &parent)

	// Billing may read the catalogue and still may not restructure it.
	rec := env.putJSON(t, "/api/v1/staff/catalog/categories/"+parent+"/parent",
		env.billing, env.billingCSRF, map[string]any{"parent_id": child})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("billing move = %d, want 403", rec.Code)
	}

	// The cycle is refused with a code a client branches on, not a 500 from the database.
	rec = env.putJSON(t, "/api/v1/staff/catalog/categories/"+parent+"/parent",
		env.admin, env.adminCSRF, map[string]any{"parent_id": child})
	if rec.Code != http.StatusConflict {
		t.Fatalf("cycle move = %d; body %s", rec.Code, rec.Body)
	}
	assertProblemCode(t, rec, "category_cycle")

	// A legal move answers with the tree, so the panel re-renders from the truth.
	third := env.seedTreeCategory(t, "move-third", nil)
	rec = env.putJSON(t, "/api/v1/staff/catalog/categories/"+third+"/parent",
		env.admin, env.adminCSRF, map[string]any{"parent_id": parent})
	if rec.Code != http.StatusOK {
		t.Fatalf("legal move = %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		Data []struct {
			Slug     string `json:"slug"`
			Depth    int    `json:"depth"`
			Path     string `json:"path"`
			Children []struct {
				Slug     string `json:"slug"`
				Children []struct {
					Slug string `json:"slug"`
				} `json:"children"`
			} `json:"children"`
		} `json:"data"`
		MaxDepth int `json:"max_depth"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode tree: %v (%s)", err, rec.Body)
	}
	if body.MaxDepth != 4 {
		t.Fatalf("max_depth = %d, want 4", body.MaxDepth)
	}
	if len(body.Data) != 1 || body.Data[0].Slug != "move-parent" {
		t.Fatalf("tree roots = %+v, want the one root", body.Data)
	}
	if len(body.Data[0].Children) != 2 {
		t.Fatalf("root children = %+v, want the moved category and the child", body.Data[0].Children)
	}
}

func TestMovingACategoryPastTheDepthBoundHasItsOwnCode(t *testing.T) {
	env := newStaffEnv(t)
	level1 := env.seedTreeCategory(t, "deep-1", nil)
	level2 := env.seedTreeCategory(t, "deep-2", &level1)
	level3 := env.seedTreeCategory(t, "deep-3", &level2)
	level4 := env.seedTreeCategory(t, "deep-4", &level3)
	orphan := env.seedTreeCategory(t, "deep-orphan", nil)

	// A move to the deepest allowed level (4) succeeds.
	rec := env.putJSON(t, "/api/v1/staff/catalog/categories/"+orphan+"/parent",
		env.admin, env.adminCSRF, map[string]any{"parent_id": level3})
	if rec.Code != http.StatusOK {
		t.Fatalf("move to depth 4 = %d; body %s", rec.Code, rec.Body)
	}

	// A fifth level does not, and says so with its own code rather than a constraint violation.
	rec = env.putJSON(t, "/api/v1/staff/catalog/categories/"+orphan+"/parent",
		env.admin, env.adminCSRF, map[string]any{"parent_id": level4})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("move past the bound = %d; body %s", rec.Code, rec.Body)
	}
	assertProblemCode(t, rec, "category_too_deep")
}

func TestDeletingACategoryWithChildrenIsRefusedWithInUse(t *testing.T) {
	env := newStaffEnv(t)
	parent := env.seedTreeCategory(t, "del-parent", nil)
	env.seedTreeCategory(t, "del-child", &parent)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/staff/catalog/categories/"+parent, nil)
	req.AddCookie(env.admin)
	req.Header.Set("X-CSRF-Token", env.adminCSRF)
	req.Header.Set("Idempotency-Key", "del-parent-key")
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete of a parent = %d; body %s", rec.Code, rec.Body)
	}
	assertProblemCode(t, rec, "in_use")
}

func TestServerAssignmentRoundTripsThroughTheAPIRoutes(t *testing.T) {
	env := newStaffEnv(t)

	// Create a target. The panel's server selector reads this list.
	rec := env.postJSON(t, "/api/v1/staff/catalog/servers", env.admin, env.adminCSRF, map[string]any{
		"slug": "eu-west", "name": "EU West", "region": "eu-west", "node": "node-7", "enabled": true,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create server = %d; body %s", rec.Code, rec.Body)
	}
	var created struct {
		ID     string `json:"id"`
		Region string `json:"region"`
		Node   string `json:"node"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode server: %v", err)
	}
	if created.Region != "eu-west" || created.Node != "node-7" {
		t.Fatalf("created server = %+v", created)
	}

	// Billing may not create one: a target is infrastructure, and the catalogue is not theirs.
	if rec := env.postJSON(t, "/api/v1/staff/catalog/servers", env.billing, env.billingCSRF,
		map[string]any{"slug": "nope", "name": "Nope"}); rec.Code != http.StatusForbidden {
		t.Fatalf("billing create server = %d, want 403", rec.Code)
	}

	rec = env.get(t, "/api/v1/staff/catalog/servers", env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("list servers = %d", rec.Code)
	}

	category := env.seedTreeCategory(t, "srv-cat", nil)
	product := env.seedTreeProduct(t, "srv-product", category)

	// Assign it, then read the placement back.
	rec = env.putJSON(t, "/api/v1/staff/catalog/products/"+product+"/server",
		env.admin, env.adminCSRF, map[string]any{"server_id": created.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("assign server = %d; body %s", rec.Code, rec.Body)
	}
	var placement struct {
		ProductID    string  `json:"product_id"`
		ServerID     *string `json:"server_id"`
		CategoryPath string  `json:"category_path"`
		Server       *struct {
			Region string `json:"region"`
		} `json:"server"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &placement); err != nil {
		t.Fatalf("decode placement: %v", err)
	}
	if placement.ServerID == nil || *placement.ServerID != created.ID {
		t.Fatalf("placement = %+v", placement)
	}
	if placement.Server == nil || placement.Server.Region != "eu-west" {
		t.Fatalf("placement did not resolve the server: %+v", placement.Server)
	}
	if placement.CategoryPath != "srv-cat" {
		t.Fatalf("category path = %q", placement.CategoryPath)
	}

	rec = env.get(t, "/api/v1/staff/catalog/products/"+product+"/placement", env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("read placement = %d", rec.Code)
	}

	// The target cannot be deleted while the product is sold on it.
	del := httptest.NewRequest(http.MethodDelete, "/api/v1/staff/catalog/servers/"+created.ID, nil)
	del.AddCookie(env.admin)
	del.Header.Set("X-CSRF-Token", env.adminCSRF)
	del.Header.Set("Idempotency-Key", "del-server-key")
	rec = httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, del)
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete of an in-use server = %d; body %s", rec.Code, rec.Body)
	}
	assertProblemCode(t, rec, "in_use")

	// Clearing the assignment is what makes the delete possible.
	if rec := env.putJSON(t, "/api/v1/staff/catalog/products/"+product+"/server",
		env.admin, env.adminCSRF, map[string]any{"server_id": nil}); rec.Code != http.StatusOK {
		t.Fatalf("clear server = %d; body %s", rec.Code, rec.Body)
	}
	del2 := httptest.NewRequest(http.MethodDelete, "/api/v1/staff/catalog/servers/"+created.ID, nil)
	del2.AddCookie(env.admin)
	del2.Header.Set("X-CSRF-Token", env.adminCSRF)
	del2.Header.Set("Idempotency-Key", "del-server-key-2")
	rec = httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, del2)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete after clearing = %d; body %s", rec.Code, rec.Body)
	}
}

// TestPublicCategoryPageFindsAChildsProduct is the storefront's half of SF-02: a product filed
// under a child category must be listed on the parent's page.
func TestPublicCategoryPageFindsAChildsProduct(t *testing.T) {
	env := newStaffEnv(t)
	parent := env.seedTreeCategory(t, "sf-parent", nil)
	child := env.seedTreeCategory(t, "sf-child", &parent)
	env.seedTreeProduct(t, "sf-child-product", child)

	// The tree renders the nesting and reports the branch count.
	rec := env.get(t, "/api/v1/catalog/categories", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public tree = %d; body %s", rec.Code, rec.Body)
	}
	var tree struct {
		Categories []struct {
			Slug            string `json:"slug"`
			SubtreeProducts int    `json:"product_count"`
			Children        []struct {
				Slug string `json:"slug"`
			} `json:"children"`
		} `json:"categories"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tree); err != nil {
		t.Fatalf("decode public tree: %v", err)
	}
	var found bool
	for _, node := range tree.Categories {
		if node.Slug != "sf-parent" {
			continue
		}
		found = true
		if node.SubtreeProducts != 1 {
			t.Fatalf("branch product count = %d, want 1 (the child's product)", node.SubtreeProducts)
		}
		if len(node.Children) != 1 || node.Children[0].Slug != "sf-child" {
			t.Fatalf("children = %+v", node.Children)
		}
	}
	if !found {
		t.Fatalf("the parent category is missing from the public tree: %+v", tree.Categories)
	}

	// And the parent's page lists the child's product.
	rec = env.get(t, "/api/v1/catalog/category/sf-parent", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("category page = %d; body %s", rec.Code, rec.Body)
	}
	var page struct {
		Category struct {
			Slug string `json:"slug"`
		} `json:"category"`
		Products []struct {
			Slug string `json:"slug"`
		} `json:"products"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode category page: %v", err)
	}
	if page.Category.Slug != "sf-parent" {
		t.Fatalf("category = %+v", page.Category)
	}
	if len(page.Products) != 1 || page.Products[0].Slug != "sf-child-product" {
		t.Fatalf("products on the parent page = %+v, want the child's product", page.Products)
	}

	// A hidden category is 404 to a visitor, not an empty page that confirms it exists.
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE categories SET hidden = true WHERE id = $1`, child); err != nil {
		t.Fatalf("hide child: %v", err)
	}
	if rec := env.get(t, "/api/v1/catalog/category/sf-child", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("hidden category page = %d, want 404", rec.Code)
	}
}
