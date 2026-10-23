package catalog_test

import (
	"context"
	"testing"

	"github.com/catalyst/billing/internal/catalog"
)

// The storefront's merchandising surface: a category that has an image, a product that can be
// featured, sold out or sold two at a time, the currency a visitor is shopping in, the options a
// product offers, and the category strip's rule.
//
// Every test here is written so it fails if the behaviour is *removed* rather than merely
// reworded: deleting the column from the query, the filter from the read, or the row from the
// copy breaks one of these assertions.

// ---------------------------------------------------------------- imagery --

// TestCategoryImageRoundTrips is SF-06 / CAT-01: a category has an image, the panel can replace
// or clear it, and the storefront read carries it.
func TestCategoryImageRoundTrips(t *testing.T) {
	s, pool := newStoreWithPool(t)
	ctx := context.Background()

	img, err := s.PutImage(ctx, "image/png", []byte("\x89PNG\r\n\x1a\ncategory-icon"))
	if err != nil {
		t.Fatalf("store image: %v", err)
	}

	created, err := s.CreateCategory(ctx, catalog.Category{
		Slug: "vps", Name: "VPS", Description: "Fast servers", ImageID: &img.ID,
	})
	if err != nil {
		t.Fatalf("create category: %v", err)
	}
	if created.ImageID == nil || *created.ImageID != img.ID {
		t.Fatalf("created category image = %v, want %s", created.ImageID, img.ID)
	}
	if got, want := created.ImageURL(), "/api/v1/media/"+img.ID; got != want {
		t.Fatalf("image URL = %q, want %q", got, want)
	}

	// The tree is what the storefront strip renders, so the image has to be on the node.
	tree, err := s.CategoryTree(ctx)
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	var found bool
	for _, node := range tree {
		if node.Slug != "vps" {
			continue
		}
		found = true
		if node.ImageID == nil || *node.ImageID != img.ID {
			t.Fatalf("tree node image = %v, want %s", node.ImageID, img.ID)
		}
		if node.Description != "Fast servers" {
			t.Fatalf("tree node description = %q", node.Description)
		}
	}
	if !found {
		t.Fatalf("the category is missing from its own tree: %+v", tree)
	}

	// Clearing it is an explicit nil, and it must be stored as NULL rather than an empty uuid.
	cleared, err := s.UpdateCategory(ctx, created.ID, catalog.Category{
		Slug: "vps", Name: "VPS", Description: "Fast servers",
	})
	if err != nil {
		t.Fatalf("update category: %v", err)
	}
	if cleared.ImageID != nil {
		t.Fatalf("cleared category still has image %v", cleared.ImageID)
	}
	var stored *string
	if err := pool.QueryRow(ctx, `SELECT image_id::text FROM categories WHERE id = $1`, created.ID).
		Scan(&stored); err != nil {
		t.Fatalf("read stored image: %v", err)
	}
	if stored != nil {
		t.Fatalf("stored image = %q, want NULL", *stored)
	}
}

// TestCategoryImageIsOnThePublicBranchPage is the same column read through the storefront path:
// a category page's node carries the image URL, because that page is where a shopper lands.
func TestCategoryImageIsOnThePublicBranchPage(t *testing.T) {
	s, pool := newStoreWithPool(t)
	ctx := context.Background()
	img, err := s.PutImage(ctx, "image/webp", []byte("RIFF-webp-category"))
	if err != nil {
		t.Fatalf("store image: %v", err)
	}
	parent := mustChildCategory(t, s, pool, ctx, "hardware", nil)
	if _, err := s.UpdateCategoryTx(ctx, pool, parent.ID, catalog.Category{
		Slug: "hardware", Name: "Hardware", ImageID: &img.ID,
	}); err != nil {
		t.Fatalf("set category image: %v", err)
	}
	child := mustChildCategory(t, s, pool, ctx, "gpu", &parent.ID)
	mustProductIn(t, s, ctx, child.ID, "gpu-a")

	node, products, err := s.PublicCategoryProducts(ctx, "hardware")
	if err != nil {
		t.Fatalf("public branch: %v", err)
	}
	if node.ImageID == nil || *node.ImageID != img.ID {
		t.Fatalf("branch node image = %v, want %s", node.ImageID, img.ID)
	}
	if len(products) != 1 || products[0].Slug != "gpu-a" {
		t.Fatalf("branch products = %+v", products)
	}
}

// mustProductIn creates a priced product in a category, for the tests that do not care whether it
// is hidden.
func mustProductIn(t *testing.T, s *catalog.Store, ctx context.Context, catID, slug string) catalog.Product {
	t.Helper()
	p, err := s.CreateProduct(ctx, catalog.ProductInput{Slug: slug, Name: slug, CategoryID: &catID})
	if err != nil {
		t.Fatalf("create product %q: %v", slug, err)
	}
	if _, err := s.AddPrice(ctx, p.ID, catalog.Price{
		Name: "Monthly", Currency: "USD", AmountMinor: 500, Interval: "monthly",
		AnchorDay: 1, AnchorTimezone: "UTC",
	}); err != nil {
		t.Fatalf("price %q: %v", slug, err)
	}
	return p
}

// ------------------------------------------------------------ merchandising --

// TestSetMerchandisingRoundTrips is SF-09's write path: stock, the per-customer limit, the
// quantity policy and the featured flag are editable and are what the storefront reads.
func TestSetMerchandisingRoundTrips(t *testing.T) {
	s, pool := newStoreWithPool(t)
	ctx := context.Background()
	cat := mustCategory(t, s, ctx, "vps")
	product := mustProduct(t, s, ctx, cat.ID, "vps-small", false)

	stock, limit := 3, 2
	updated, err := s.SetMerchandisingTx(ctx, pool, product.ID, catalog.Merchandising{
		Stock: &stock, PerUserLimit: &limit,
		AllowQuantity: catalog.QuantitySeparated, Featured: true,
	})
	if err != nil {
		t.Fatalf("set merchandising: %v", err)
	}
	if updated.Stock == nil || *updated.Stock != 3 {
		t.Fatalf("stock = %v, want 3", updated.Stock)
	}
	if updated.PerUserLimit == nil || *updated.PerUserLimit != 2 {
		t.Fatalf("per-user limit = %v, want 2", updated.PerUserLimit)
	}
	if updated.AllowQuantity != catalog.QuantitySeparated {
		t.Fatalf("allow_quantity = %q", updated.AllowQuantity)
	}
	if !updated.Featured {
		t.Fatal("featured was not stored")
	}
	if !updated.InStock() {
		t.Fatal("a product with 3 units is in stock")
	}

	// Sold out and cleared back to unlimited, and the policy is untouched when it is omitted —
	// that is what lets the stock field be saved on its own.
	soldOut := 0
	updated, err = s.SetMerchandisingTx(ctx, pool, product.ID, catalog.Merchandising{
		Stock: &soldOut, PerUserLimit: &limit, Featured: false,
	})
	if err != nil {
		t.Fatalf("sell out: %v", err)
	}
	if updated.InStock() {
		t.Fatal("a product with 0 units is not in stock")
	}
	if updated.AllowQuantity != catalog.QuantitySeparated {
		t.Fatalf("an omitted policy was rewritten to %q", updated.AllowQuantity)
	}

	updated, err = s.SetMerchandisingTx(ctx, pool, product.ID, catalog.Merchandising{
		Stock: nil, PerUserLimit: nil, AllowQuantity: catalog.QuantityDisabled,
	})
	if err != nil {
		t.Fatalf("clear stock: %v", err)
	}
	if updated.Stock != nil {
		t.Fatalf("stock = %v, want unlimited", updated.Stock)
	}
	if !updated.InStock() {
		t.Fatal("unlimited stock must read as in stock")
	}
}

// TestMerchandisingRefusesWhatTheDatabaseRefuses keeps the error a sentence rather than a
// SQLSTATE: the API's code is only stable if the check happens before the write.
func TestMerchandisingRefusesWhatTheDatabaseRefuses(t *testing.T) {
	s, pool := newStoreWithPool(t)
	ctx := context.Background()
	cat := mustCategory(t, s, ctx, "vps")
	product := mustProduct(t, s, ctx, cat.ID, "vps-small", false)

	negative := -1
	if _, err := s.SetMerchandisingTx(ctx, pool, product.ID, catalog.Merchandising{Stock: &negative}); err == nil {
		t.Fatal("a negative stock was accepted")
	}
	if _, err := s.SetMerchandisingTx(ctx, pool, product.ID, catalog.Merchandising{PerUserLimit: &negative}); err == nil {
		t.Fatal("a zero per-customer limit was accepted")
	}
	if _, err := s.SetMerchandisingTx(ctx, pool, product.ID,
		catalog.Merchandising{AllowQuantity: "unlimited"}); err == nil {
		t.Fatal("an unknown quantity policy was accepted")
	}
}

// TestLowStockOnlyAppliesBelowTheThreshold is the display rule that stops "only 0 left".
func TestLowStockOnlyAppliesBelowTheThreshold(t *testing.T) {
	five, zero := 5, 0
	unlimited := catalog.Product{}
	if unlimited.LowStock(5) {
		t.Fatal("an unlimited product is never low on stock")
	}
	if !(&catalog.Product{Stock: &five}).LowStock(5) {
		t.Fatal("five units at a threshold of five is low")
	}
	if (&catalog.Product{Stock: &five}).LowStock(4) {
		t.Fatal("five units at a threshold of four is not low")
	}
	if (&catalog.Product{Stock: &zero}).LowStock(5) {
		t.Fatal("a sold-out product is out of stock, not low on stock")
	}
}

// --------------------------------------------------------------- the strip --

// TestHomeCategoriesListsTopLevelBranches is SF-01: the strip lists top-level categories that
// have children or visible products, ordered by sort, and never an empty branch.
func TestHomeCategoriesListsTopLevelBranches(t *testing.T) {
	s, pool := newStoreWithPool(t)
	ctx := context.Background()

	// A branch with a product two levels down, a root with one product, an empty root, and a
	// branch whose only child is empty.
	games := mustChildCategory(t, s, pool, ctx, "game-servers", nil)
	minecraft := mustChildCategory(t, s, pool, ctx, "minecraft", &games.ID)
	modded := mustChildCategory(t, s, pool, ctx, "modded", &minecraft.ID)
	mustProductIn(t, s, ctx, modded.ID, "modded-a")

	hosting := mustChildCategory(t, s, pool, ctx, "hosting", nil)
	mustProductIn(t, s, ctx, hosting.ID, "shared-a")

	mustChildCategory(t, s, pool, ctx, "empty-root", nil)
	ghost := mustChildCategory(t, s, pool, ctx, "ghost", nil)
	mustChildCategory(t, s, pool, ctx, "ghost-child", &ghost.ID)

	nodes, err := s.CategoryTree(ctx)
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	visible, err := s.Public(ctx)
	if err != nil {
		t.Fatalf("public products: %v", err)
	}
	strip := catalog.HomeCategories(nodes, visible, "top_level")

	names := flattenNames(strip)
	for _, want := range []string{"game-servers", "minecraft", "modded", "hosting"} {
		if !names[want] {
			t.Fatalf("the strip is missing %q: %v", want, names)
		}
	}
	for _, unwanted := range []string{"empty-root", "ghost", "ghost-child"} {
		if names[unwanted] {
			t.Fatalf("the strip offered the empty branch %q: %v", unwanted, names)
		}
	}
	// Only the roots are top level: the strip starts at the root and nests below it.
	if len(strip) != 2 || strip[0].Slug != "game-servers" || strip[1].Slug != "hosting" {
		t.Fatalf("strip roots = %+v, want game-servers then hosting", strip)
	}
	if strip[0].SubtreeProductCount != 1 {
		t.Fatalf("game-servers branch count = %d, want 1", strip[0].SubtreeProductCount)
	}

	// `all` is the escape hatch for a nested shop whose operator wants the children in the strip:
	// every visible category, depth first, flat, in sort order.
	everything := catalog.HomeCategories(nodes, visible, "all")
	if len(everything) != 7 {
		t.Fatalf("mode=all returned %d entries, want all seven: %v", len(everything), flattenNames(everything))
	}
	if everything[1].Slug != "minecraft" || everything[2].Slug != "modded" {
		t.Fatalf("mode=all order = %v, want the tree's depth-first order", flattenNames(everything))
	}
	for _, node := range everything {
		if len(node.Children) != 0 {
			t.Fatalf("mode=all left %q nested, so the strip would draw it twice", node.Slug)
		}
	}

	// A hidden category and its subtree never reach the strip.
	if _, err := s.UpdateCategory(ctx, ghost.ID, catalog.Category{Slug: "ghost", Name: "Ghost", Hidden: true}); err != nil {
		t.Fatalf("hide: %v", err)
	}
	// Give the hidden branch a product: it must still not appear.
	mustProductIn(t, s, ctx, ghost.ID, "ghost-product")
	nodes, err = s.CategoryTree(ctx)
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	visible, err = s.Public(ctx)
	if err != nil {
		t.Fatalf("public products: %v", err)
	}
	if flattenNames(catalog.HomeCategories(nodes, visible, "top_level"))["ghost"] {
		t.Fatal("a hidden category reached the storefront strip")
	}
}

// flattenNames collects every slug in a tree, for the assertions above.
func flattenNames(nodes []catalog.CategoryNode) map[string]bool {
	out := map[string]bool{}
	for _, n := range nodes {
		out[n.Slug] = true
		for k := range flattenNames(n.Children) {
			out[k] = true
		}
	}
	return out
}

// --------------------------------------------------------------- currency --

// TestPublicInCurrencyIsWhatTheSwitcherChanges is SF-18 / CUR-03: the currency the visitor picked
// decides which products are on the shelf and which prices are quoted.
func TestPublicInCurrencyIsWhatTheSwitcherChanges(t *testing.T) {
	s, _ := newStoreWithPool(t)
	ctx := context.Background()
	cat := mustCategory(t, s, ctx, "vps")

	dollarsOnly := mustProduct(t, s, ctx, cat.ID, "usd-only", false)
	both := mustProduct(t, s, ctx, cat.ID, "both", false)
	if _, err := s.AddPrice(ctx, both.ID, catalog.Price{
		Name: "Monthly EUR", Currency: "EUR", AmountMinor: 900, Interval: "monthly",
		AnchorDay: 1, AnchorTimezone: "UTC",
	}); err != nil {
		t.Fatalf("add EUR price: %v", err)
	}

	all, err := s.Public(ctx)
	if err != nil {
		t.Fatalf("public: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("unfiltered public = %d products, want 2", len(all))
	}

	euro, err := s.PublicIn(ctx, "eur")
	if err != nil {
		t.Fatalf("public in EUR: %v", err)
	}
	if len(euro) != 1 || euro[0].Slug != "both" {
		t.Fatalf("EUR catalogue = %+v, want only %q", euro, "both")
	}
	if len(euro[0].Prices) != 1 || euro[0].Prices[0].Currency != "EUR" {
		t.Fatalf("EUR prices = %+v, want only the EUR row", euro[0].Prices)
	}
	if euro[0].Prices[0].AmountMinor != 900 {
		t.Fatalf("EUR amount = %d, want 900", euro[0].Prices[0].AmountMinor)
	}

	// A product with no price in the selected currency is not found by slug either, which is
	// what makes a shared product link honest rather than a page with no price on it.
	if _, err := s.PublicProductIn(ctx, "usd-only", "EUR"); err == nil {
		t.Fatal("a USD-only product resolved for a EUR visitor")
	}
	if _, err := s.PublicProductIn(ctx, dollarsOnly.Slug, "USD"); err != nil {
		t.Fatalf("the same product in USD: %v", err)
	}

	// The category page obeys the same filter, so the branch is not a second shop.
	_, branch, err := s.PublicCategoryProductsIn(ctx, "vps", "EUR")
	if err != nil {
		t.Fatalf("EUR branch: %v", err)
	}
	if len(branch) != 1 || branch[0].Slug != "both" {
		t.Fatalf("EUR branch products = %+v", branch)
	}
}

// ---------------------------------------------------------------- options --

// TestConfigOptionsReadCarriesPerPlanAmounts is SF-03's data half: the option a customer must
// choose, with the amount that belongs to the plan they picked.
func TestConfigOptionsReadCarriesPerPlanAmounts(t *testing.T) {
	s, pool := newStoreWithPool(t)
	ctx := context.Background()
	cat := mustCategory(t, s, ctx, "vps")
	product := mustProduct(t, s, ctx, cat.ID, "vps-small", false)
	// Re-read: the create response carries no prices, and the option amounts hang off the
	// price version ids.
	product, err := s.Product(ctx, product.ID)
	if err != nil {
		t.Fatalf("read product: %v", err)
	}
	monthly := product.Prices[0]
	annual, err := s.AddPrice(ctx, product.ID, catalog.Price{
		Name: "Annual", Currency: "USD", AmountMinor: 10000, Interval: "annual",
		AnchorDay: 1, AnchorTimezone: "UTC",
	})
	if err != nil {
		t.Fatalf("annual price: %v", err)
	}

	var optionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO config_options (product_id, name, description, option_type, choices,
		                            required, hidden, sort_order)
		VALUES ($1, 'Memory', 'How much RAM', 'dropdown', '["8 GB","16 GB"]'::jsonb, true, false, 1)
		RETURNING id`, product.ID).Scan(&optionID); err != nil {
		t.Fatalf("config option: %v", err)
	}
	// The same choice costs one thing monthly and another annually.
	for _, row := range []struct {
		version string
		choice  string
		amount  int64
	}{
		{monthly.ID, "16 GB", 500},
		{annual.ID, "16 GB", 4000},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO config_option_prices (config_option_id, price_version_id, choice, amount_minor)
			VALUES ($1, $2, $3, $4)`, optionID, row.version, row.choice, row.amount); err != nil {
			t.Fatalf("option price: %v", err)
		}
	}

	options, err := s.ConfigOptions(ctx, product.ID)
	if err != nil {
		t.Fatalf("read options: %v", err)
	}
	if len(options) != 1 {
		t.Fatalf("options = %+v, want one", options)
	}
	o := options[0]
	if o.Name != "Memory" || !o.Required || o.OptionType != "dropdown" {
		t.Fatalf("option = %+v", o)
	}
	if len(o.Choices) != 2 || o.Choices[0] != "8 GB" {
		t.Fatalf("choices = %+v", o.Choices)
	}
	amounts := map[string]int64{}
	for _, p := range o.OptionPrices {
		amounts[p.PriceVersionID] = p.AmountMinor
	}
	if amounts[monthly.ID] != 500 || amounts[annual.ID] != 4000 {
		t.Fatalf("per-plan amounts = %+v, want 500 monthly and 4000 annual", amounts)
	}
}

// -------------------------------------------------------------- duplicating --

// TestDuplicateCopiesTheWholeProduct is CAT-08's breadth: a copy carries the presentation and the
// structural facts, not just the prose.
func TestDuplicateCopiesTheWholeProduct(t *testing.T) {
	s, pool := newStoreWithPool(t)
	ctx := context.Background()
	cat := mustCategory(t, s, ctx, "vps")

	var serverID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO servers (slug, name, provider, region) VALUES ('eu', 'EU', 'pterodactyl', 'eu-west')
		RETURNING id`).Scan(&serverID); err != nil {
		t.Fatalf("server: %v", err)
	}

	stock, limit := 4, 1
	source := mustProduct(t, s, ctx, cat.ID, "vps-8gb", false)
	if _, err := s.SetMerchandisingTx(ctx, pool, source.ID, catalog.Merchandising{
		Stock: &stock, PerUserLimit: &limit,
		AllowQuantity: catalog.QuantityCombined, Featured: true,
	}); err != nil {
		t.Fatalf("merchandising: %v", err)
	}
	if _, err := s.SetProductServerTx(ctx, pool, source.ID, &serverID); err != nil {
		t.Fatalf("assign server: %v", err)
	}
	upgradeTarget := mustProduct(t, s, ctx, cat.ID, "vps-16gb", false)
	if _, err := pool.Exec(ctx,
		`INSERT INTO product_upgrades (product_id, upgrade_to_id) VALUES ($1, $2)`,
		source.ID, upgradeTarget.ID); err != nil {
		t.Fatalf("upgrade edge: %v", err)
	}

	copied, err := s.DuplicateProductTx(ctx, pool, source.ID)
	if err != nil {
		t.Fatalf("duplicate: %v", err)
	}

	if !copied.Hidden {
		t.Fatal("a copy must be a draft")
	}
	if copied.ServerID == nil || *copied.ServerID != serverID {
		t.Fatalf("copy server = %v, want the source's %s", copied.ServerID, serverID)
	}
	if copied.Stock == nil || *copied.Stock != 4 || copied.PerUserLimit == nil || *copied.PerUserLimit != 1 {
		t.Fatalf("copy merchandising = %+v", copied)
	}
	if copied.AllowQuantity != catalog.QuantityCombined || !copied.Featured {
		t.Fatalf("copy presentation = %q featured=%v", copied.AllowQuantity, copied.Featured)
	}
	if len(copied.Prices) != 1 {
		t.Fatalf("copy has %d plans, want 1", len(copied.Prices))
	}

	var edges int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM product_upgrades WHERE product_id = $1 AND upgrade_to_id = $2`,
		copied.ID, upgradeTarget.ID).Scan(&edges); err != nil {
		t.Fatalf("count upgrade edges: %v", err)
	}
	if edges != 1 {
		t.Fatal("the copy lost its upgrade graph: a variant nobody can move to")
	}
}
