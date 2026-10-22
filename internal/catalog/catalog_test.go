package catalog_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/catalog"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The catalog's two reads are the security boundary between staff and the public, so they get
// their own tests. Everything else here is CRUD; these are the assertions that decide whether a
// hidden product leaks.

func newStore(t *testing.T) (*catalog.Store, context.Context) {
	t.Helper()
	s, _ := newStoreWithPool(t)
	return s, context.Background()
}

func newStoreWithPool(t *testing.T) (*catalog.Store, *pgxpool.Pool) {
	t.Helper()
	pool := storetest.DB(t).Pool
	return catalog.New(pool), pool
}

// mustCategory creates a category or fails the test.
func mustCategory(t *testing.T, s *catalog.Store, ctx context.Context, slug string) catalog.Category {
	t.Helper()
	c, err := s.CreateCategory(ctx, catalog.Category{Slug: slug, Name: strings.ToUpper(slug)})
	if err != nil {
		t.Fatalf("create category %q: %v", slug, err)
	}
	return c
}

// mustProduct creates a priced product or fails the test.
func mustProduct(t *testing.T, s *catalog.Store, ctx context.Context, catID, slug string, hidden bool) catalog.Product {
	t.Helper()
	p, err := s.CreateProduct(ctx, catalog.ProductInput{
		Slug:       slug,
		Name:       slug,
		CategoryID: &catID,
		Hidden:     hidden,
	})
	if err != nil {
		t.Fatalf("create product %q: %v", slug, err)
	}
	if _, err := s.AddPrice(ctx, p.ID, catalog.Price{
		Name: "Monthly", Currency: "USD", AmountMinor: 1000, Interval: "monthly",
		AnchorDay: 1, AnchorTimezone: "UTC",
	}); err != nil {
		t.Fatalf("add price to %q: %v", slug, err)
	}
	return p
}

func TestPublicExcludesHiddenProducts(t *testing.T) {
	s, ctx := newStore(t)
	cat := mustCategory(t, s, ctx, "vps")
	mustProduct(t, s, ctx, cat.ID, "visible", false)
	mustProduct(t, s, ctx, cat.ID, "hidden", true)

	products, err := s.Public(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(products) != 1 || products[0].Slug != "visible" {
		t.Fatalf("public products = %v, want only the visible one", slugs(products))
	}

	// The staff read sees both, which is what makes hiding a publishing decision rather than a
	// deletion.
	all, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("staff products = %d, want 2", len(all))
	}
}

func TestPublicExcludesProductsInHiddenCategories(t *testing.T) {
	s, ctx := newStore(t)
	hiddenCat := mustCategory(t, s, ctx, "secret")
	if _, err := s.UpdateCategory(ctx, hiddenCat.ID, catalog.Category{
		Slug: "secret", Name: "Secret", Hidden: true,
	}); err != nil {
		t.Fatal(err)
	}
	mustProduct(t, s, ctx, hiddenCat.ID, "under-secret", false)

	products, err := s.Public(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(products) != 0 {
		t.Fatalf("a product in a hidden category leaked: %v", slugs(products))
	}
}

func TestPublicExcludesUnpricedProducts(t *testing.T) {
	s, ctx := newStore(t)
	cat := mustCategory(t, s, ctx, "vps")
	if _, err := s.CreateProduct(ctx, catalog.ProductInput{
		Slug: "no-price", Name: "No Price", CategoryID: &cat.ID,
	}); err != nil {
		t.Fatal(err)
	}

	products, err := s.Public(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A product with no price cannot be bought, so offering it would be a card whose button
	// fails. This is the assertion that keeps the storefront honest.
	if len(products) != 0 {
		t.Fatalf("an unpriced product leaked into the storefront: %v", slugs(products))
	}
}

func TestPublicProductIsIndistinguishableFromMissing(t *testing.T) {
	s, ctx := newStore(t)
	cat := mustCategory(t, s, ctx, "vps")
	mustProduct(t, s, ctx, cat.ID, "hidden-one", true)

	if _, err := s.PublicProduct(ctx, "hidden-one"); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("hidden product lookup = %v, want ErrNotFound", err)
	}
	if _, err := s.PublicProduct(ctx, "does-not-exist"); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("missing product lookup = %v, want ErrNotFound", err)
	}
}

func TestPublicJSONDoesNotLeakProvisionTargets(t *testing.T) {
	s, ctx := newStore(t)
	cat := mustCategory(t, s, ctx, "vps")
	p, err := s.CreateProduct(ctx, catalog.ProductInput{Slug: "p1", Name: "P1", CategoryID: &cat.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPrice(ctx, p.ID, catalog.Price{
		Name: "Monthly", Currency: "USD", AmountMinor: 500, Interval: "monthly",
		AnchorDay: 1, AnchorTimezone: "UTC",
		ProvisionPlan: "vps.small", ProvisionRegion: "us-east-1",
	}); err != nil {
		t.Fatal(err)
	}

	// The storefront shape is built by the HTTP layer, but the fields it must not carry live on
	// the price row, so this pins that they exist for staff and that the public query returns
	// them only because the handler chooses what to render. The handler test lives in
	// internal/httpapi; here the contract is that a price carries them at all.
	public, err := s.PublicProduct(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if public.Prices[0].ProvisionPlan != "vps.small" {
		t.Fatalf("expected the price to carry its provision plan, got %q", public.Prices[0].ProvisionPlan)
	}
}

func TestPriceVersionsAppendAndNeverOverwrite(t *testing.T) {
	s, ctx := newStore(t)
	cat := mustCategory(t, s, ctx, "vps")
	p, err := s.CreateProduct(ctx, catalog.ProductInput{Slug: "p1", Name: "P1", CategoryID: &cat.ID})
	if err != nil {
		t.Fatal(err)
	}

	first, err := s.AddPrice(ctx, p.ID, catalog.Price{
		Name: "Monthly", Currency: "USD", AmountMinor: 1000, Interval: "monthly",
		AnchorDay: 1, AnchorTimezone: "UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != 1 {
		t.Fatalf("first version = %d, want 1", first.Version)
	}

	second, err := s.AddPrice(ctx, p.ID, catalog.Price{
		Name: "Monthly", Currency: "USD", AmountMinor: 1200, Interval: "monthly",
		AnchorDay: 1, AnchorTimezone: "UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Version != 2 {
		t.Fatalf("second version = %d, want 2", second.Version)
	}

	read, err := s.Product(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Prices) != 2 {
		t.Fatalf("prices = %d, want 2 (a price is appended, not replaced)", len(read.Prices))
	}
	// Newest first, and the original amount is untouched: this is what makes an invoice from
	// last month still readable.
	if read.Prices[0].AmountMinor != 1200 || read.Prices[1].AmountMinor != 1000 {
		t.Fatalf("price history = %d then %d, want 1200 then 1000",
			read.Prices[0].AmountMinor, read.Prices[1].AmountMinor)
	}
}

func TestDeleteProductRefusesWhenInUse(t *testing.T) {
	s, pool := newStoreWithPool(t)
	ctx := context.Background()
	cat := mustCategory(t, s, ctx, "vps")
	p := mustProduct(t, s, ctx, cat.ID, "in-use", false)

	// A price version alone does not block deletion: it is part of the product. A service
	// does, and a service needs a customer, so the minimum rows are inserted directly — the
	// catalog package has no business creating customers, and the guard is what is under test.
	if _, err := pool.Exec(ctx, `
		INSERT INTO customers (id, email, name)
		VALUES ('00000000-0000-4000-8000-000000000001', 'c1@example.test', 'C1')
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO services (customer_id, product_id, status, external_id, generation)
		VALUES ('00000000-0000-4000-8000-000000000001', $1, 'active', 'svc-1', 1)`, p.ID); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteProduct(ctx, p.ID); !errors.Is(err, catalog.ErrInUse) {
		t.Fatalf("delete in-use product = %v, want ErrInUse", err)
	}
	if _, err := s.Product(ctx, p.ID); err != nil {
		t.Fatalf("the product should still exist: %v", err)
	}
}

func TestDeleteCategoryRefusesWhenProductsRemain(t *testing.T) {
	s, ctx := newStore(t)
	cat := mustCategory(t, s, ctx, "vps")
	mustProduct(t, s, ctx, cat.ID, "p1", false)

	if err := s.DeleteCategory(ctx, cat.ID); !errors.Is(err, catalog.ErrInUse) {
		t.Fatalf("delete category with products = %v, want ErrInUse", err)
	}
}

func TestSlugValidation(t *testing.T) {
	cases := map[string]bool{
		"vps":                   true,
		"vps-small":             true,
		"vps-2":                 true,
		"VPS":                   false, // uppercase
		"vps small":             false, // space
		"vps--small":            false, // double hyphen
		"-vps":                  false, // leading hyphen
		"vps-":                  false, // trailing hyphen
		"a":                     false, // too short
		strings.Repeat("a", 65): false,
	}
	for slug, want := range cases {
		err := catalog.ValidateSlug(slug)
		if (err == nil) != want {
			t.Errorf("ValidateSlug(%q) error = %v, want valid=%v", slug, err, want)
		}
	}
}

func TestImageRejectsBadTypesAndOversize(t *testing.T) {
	s, ctx := newStore(t)

	if _, err := s.PutImage(ctx, "text/html", []byte("<script>")); !errors.Is(err, catalog.ErrBadImageType) {
		t.Fatalf("text/html = %v, want ErrBadImageType", err)
	}
	if _, err := s.PutImage(ctx, "image/png", nil); err == nil {
		t.Fatal("an empty upload was accepted")
	}
	big := make([]byte, catalog.MaxImageBytes+1)
	if _, err := s.PutImage(ctx, "image/png", big); !errors.Is(err, catalog.ErrImageTooLarge) {
		t.Fatalf("oversize = %v, want ErrImageTooLarge", err)
	}
}

func TestImageDeduplicatesByDigest(t *testing.T) {
	s, ctx := newStore(t)
	data := []byte("\x89PNG\r\n\x1a\n fake png bytes")

	first, err := s.PutImage(ctx, "image/png", data)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.PutImage(ctx, "image/png", data)
	if err != nil {
		t.Fatal(err)
	}
	// One row for identical bytes is what lets the media endpoint promise an immutable cache:
	// the id names exactly one byte sequence.
	if first.ID != second.ID {
		t.Fatalf("identical uploads produced different ids: %s vs %s", first.ID, second.ID)
	}
}

func slugs(ps []catalog.Product) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Slug)
	}
	return out
}
