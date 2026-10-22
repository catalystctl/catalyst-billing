package catalog_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/catalog"
)

// Plan types, plan order, duplication and the rich description. Each test here is written so
// that it fails if the behaviour is merely *present* rather than correct: a free price with an
// amount, a duplicate that misses the options, an order that is ignored, and a description that
// is stored as typed.

func TestFreePriceMustHaveNoAmount(t *testing.T) {
	s, pool := newStoreWithPool(t)
	ctx := context.Background()
	cat := mustCategory(t, s, ctx, "free-cat")
	p, err := s.CreateProduct(ctx, catalog.ProductInput{Slug: "freebie", Name: "Freebie", CategoryID: &cat.ID})
	if err != nil {
		t.Fatalf("product: %v", err)
	}

	if _, err := s.AddPrice(ctx, p.ID, catalog.Price{
		Name: "Free", Currency: "USD", AmountMinor: 500, Interval: catalog.IntervalFree,
		AnchorDay: 1, AnchorTimezone: "UTC",
	}); !errors.Is(err, catalog.ErrInvalidInput) {
		t.Fatalf("a free price with an amount = %v, want ErrInvalidInput", err)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO price_versions (product_id, version, name, currency, amount_minor, interval,
		                            anchor_day, anchor_timezone)
		VALUES ($1, 99, 'Free', 'USD', 100, 'free', 1, 'UTC')`, p.ID)
	if err == nil {
		t.Fatal("the database accepted a free price with a non-zero amount")
	}

	// A free price with no amount is accepted, and a one-time price may cost anything.
	if _, err := s.AddPrice(ctx, p.ID, catalog.Price{
		Name: "Free", Currency: "USD", Interval: catalog.IntervalFree,
		AnchorDay: 1, AnchorTimezone: "UTC",
	}); err != nil {
		t.Fatalf("free price: %v", err)
	}
	if _, err := s.AddPrice(ctx, p.ID, catalog.Price{
		Name: "Setup package", Currency: "USD", AmountMinor: 25000, Interval: catalog.IntervalOneTime,
		AnchorDay: 1, AnchorTimezone: "UTC",
	}); err != nil {
		t.Fatalf("one-time price: %v", err)
	}
}

func TestIntervalVocabulary(t *testing.T) {
	for _, iv := range catalog.Intervals {
		if err := catalog.ValidateInterval(iv); err != nil {
			t.Fatalf("%q is in Intervals but is not valid: %v", iv, err)
		}
	}
	if err := catalog.ValidateInterval("weekly"); err == nil {
		t.Fatal("an interval outside the closed set must be refused")
	}
	// The predicate the rest of the system asks. A one-time or free price is not a schedule.
	if catalog.Recurring(catalog.IntervalOneTime) || catalog.Recurring(catalog.IntervalFree) {
		t.Fatal("one_time and free must not be recurring")
	}
	if !catalog.Recurring(catalog.IntervalAnnual) {
		t.Fatal("annual must be recurring")
	}
}

func TestPlanOrderIsHonoured(t *testing.T) {
	s, pool := newStoreWithPool(t)
	ctx := context.Background()
	cat := mustCategory(t, s, ctx, "plans")
	p, err := s.CreateProduct(ctx, catalog.ProductInput{Slug: "tiers", Name: "Tiers", CategoryID: &cat.ID})
	if err != nil {
		t.Fatalf("product: %v", err)
	}
	var ids []string
	for _, tier := range []struct {
		name   string
		amount int64
	}{{"Basic", 1000}, {"Pro", 2000}, {"Enterprise", 5000}} {
		price, err := s.AddPrice(ctx, p.ID, catalog.Price{
			Name: tier.name, Currency: "USD", AmountMinor: tier.amount,
			Interval: catalog.IntervalMonthly, AnchorDay: 1, AnchorTimezone: "UTC",
		})
		if err != nil {
			t.Fatalf("add %s: %v", tier.name, err)
		}
		ids = append(ids, price.ID)
	}

	// Appending assigns an increasing order, so a new plan lands last rather than at an
	// arbitrary position.
	before, err := s.Product(ctx, p.ID)
	if err != nil {
		t.Fatalf("read product: %v", err)
	}
	if got := planNames(catalog.Plans(before.Prices)); strings.Join(got, ",") != "Basic,Pro,Enterprise" {
		t.Fatalf("plan order = %v, want Basic,Pro,Enterprise", got)
	}

	// Reverse it.
	reordered, err := s.ReorderPlansTx(ctx, pool, p.ID, []string{ids[2], ids[0], ids[1]})
	if err != nil {
		t.Fatalf("reorder: %v", err)
	}
	if got := planNames(catalog.Plans(reordered)); strings.Join(got, ",") != "Enterprise,Basic,Pro" {
		t.Fatalf("after reorder = %v, want Enterprise,Basic,Pro", got)
	}

	// A partial order is refused rather than leaving the unnamed plans where they were.
	if _, err := s.ReorderPlansTx(ctx, pool, p.ID, []string{ids[2]}); !errors.Is(err, catalog.ErrInvalidInput) {
		t.Fatalf("partial order = %v, want ErrInvalidInput", err)
	}
	// So is a plan that belongs to a different product.
	if _, err := s.ReorderPlansTx(ctx, pool, p.ID, []string{ids[0], ids[1], ids[2], p.ID}); !errors.Is(err, catalog.ErrInvalidInput) {
		t.Fatalf("foreign plan in order = %v, want ErrInvalidInput", err)
	}

	// The storefront reads the same order, which is what makes the column worth having.
	public, err := s.PublicProduct(ctx, "tiers")
	if err != nil {
		t.Fatalf("public product: %v", err)
	}
	if got := planNames(catalog.Plans(public.Prices)); strings.Join(got, ",") != "Enterprise,Basic,Pro" {
		t.Fatalf("public plan order = %v, want the operator's order", got)
	}
}

func TestDuplicateProductCopiesPlansOptionsAndHidesTheCopy(t *testing.T) {
	s, pool := newStoreWithPool(t)
	ctx := context.Background()
	cat := mustCategory(t, s, ctx, "dup-cat")
	src, err := s.CreateProduct(ctx, catalog.ProductInput{
		Slug: "vps-8gb", Name: "VPS 8 GB", CategoryID: &cat.ID,
		ShortDescription: "Eight gigabytes", Description: "<p>Fast</p>", DescriptionFormat: "html",
	})
	if err != nil {
		t.Fatalf("source product: %v", err)
	}
	var versionIDs []string
	for _, p := range []catalog.Price{
		{Name: "Monthly", Currency: "USD", AmountMinor: 1000, Interval: catalog.IntervalMonthly, AnchorDay: 1, AnchorTimezone: "UTC"},
		{Name: "Annual", Currency: "USD", AmountMinor: 10000, Interval: catalog.IntervalAnnual, AnchorDay: 1, AnchorTimezone: "UTC"},
	} {
		price, err := s.AddPrice(ctx, src.ID, p)
		if err != nil {
			t.Fatalf("add price: %v", err)
		}
		versionIDs = append(versionIDs, price.ID)
	}
	// A config option tree with a price per version: a duplicate without these is not a copy of
	// the product, it is a copy of its name.
	var ramID, eccID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO config_options (product_id, name, option_type, choices, required)
		VALUES ($1, 'RAM', 'dropdown', '["8 GB","16 GB"]'::jsonb, true) RETURNING id`,
		src.ID).Scan(&ramID); err != nil {
		t.Fatalf("option: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO config_options (product_id, parent_id, name, option_type, choices)
		VALUES ($1, $2, 'ECC', 'checkbox', '["on"]'::jsonb) RETURNING id`,
		src.ID, ramID).Scan(&eccID); err != nil {
		t.Fatalf("child option: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO config_option_prices (config_option_id, price_version_id, choice, amount_minor)
		VALUES ($1, $2, '16 GB', 500)`, ramID, versionIDs[0]); err != nil {
		t.Fatalf("option price: %v", err)
	}

	copyOf, err := s.DuplicateProductTx(ctx, pool, src.ID)
	if err != nil {
		t.Fatalf("duplicate: %v", err)
	}

	if copyOf.Slug != "vps-8gb-copy" || copyOf.Name != "VPS 8 GB (copy)" {
		t.Fatalf("copy = %q / %q, want a disambiguated slug and name", copyOf.Slug, copyOf.Name)
	}
	if !copyOf.Hidden {
		t.Fatal("the copy must be hidden: duplicating a product produces a draft, not a second live listing")
	}
	if copyOf.ShortDescription != "Eight gigabytes" || copyOf.Description != "<p>Fast</p>" ||
		copyOf.DescriptionFormat != "html" {
		t.Fatalf("copy lost the description: %+v", copyOf)
	}
	if len(copyOf.Prices) != 2 {
		t.Fatalf("copy has %d plans, want 2", len(copyOf.Prices))
	}
	for i, want := range []struct {
		name   string
		amount int64
		iv     string
	}{{"Monthly", 1000, "monthly"}, {"Annual", 10000, "annual"}} {
		got := copyOf.Prices[len(copyOf.Prices)-1-i] // prices arrive newest version first
		if got.Name != want.name || got.AmountMinor != want.amount || got.Interval != want.iv {
			t.Fatalf("plan %d = %+v, want %v", i, got, want)
		}
		if got.ID == versionIDs[i] {
			t.Fatal("the copy reuses the source's price version id, so an invoice would point at the wrong product")
		}
	}

	// Config options, including the parent link and the per-version price.
	var options int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM config_options WHERE product_id = $1`, copyOf.ID).
		Scan(&options); err != nil {
		t.Fatalf("count options: %v", err)
	}
	if options != 2 {
		t.Fatalf("copy has %d config options, want 2", options)
	}
	var childParent string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(parent_id::text, '') FROM config_options
		 WHERE product_id = $1 AND name = 'ECC'`, copyOf.ID).Scan(&childParent); err != nil {
		t.Fatalf("read copied child: %v", err)
	}
	if childParent == "" || childParent == ramID {
		t.Fatalf("copied child's parent = %q, want the copied RAM option", childParent)
	}
	var copiedOptionPrices int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM config_option_prices cop
		  JOIN config_options co ON co.id = cop.config_option_id
		 WHERE co.product_id = $1 AND cop.choice = '16 GB' AND cop.amount_minor = 500`,
		copyOf.ID).Scan(&copiedOptionPrices); err != nil {
		t.Fatalf("count option prices: %v", err)
	}
	if copiedOptionPrices != 1 {
		t.Fatalf("copied option prices = %d, want 1", copiedOptionPrices)
	}

	// The source is untouched.
	original, err := s.Product(ctx, src.ID)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	if original.Hidden || original.Slug != "vps-8gb" || len(original.Prices) != 2 {
		t.Fatalf("the source was modified: %+v", original)
	}

	// A second duplicate of the same product must not collide with the first.
	second, err := s.DuplicateProductTx(ctx, pool, src.ID)
	if err != nil {
		t.Fatalf("second duplicate: %v", err)
	}
	if second.Slug != "vps-8gb-copy-2" {
		t.Fatalf("second copy slug = %q, want vps-8gb-copy-2", second.Slug)
	}
}

func TestDuplicateRefusesAMissingProduct(t *testing.T) {
	s, pool := newStoreWithPool(t)
	_, err := s.DuplicateProductTx(context.Background(), pool,
		"00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("duplicate of a missing product = %v, want ErrNotFound", err)
	}
}

func TestRichDescriptionIsSanitisedOnWriteAndItsFormatIsKept(t *testing.T) {
	s, ctx := newStore(t)
	cat := mustCategory(t, s, ctx, "rich-cat")
	p, err := s.CreateProduct(ctx, catalog.ProductInput{
		Slug: "rich", Name: "Rich", CategoryID: &cat.ID,
		Description:       `<h2>Hi</h2><img src=x onerror=alert(1)><script>alert(2)</script><a href="javascript:alert(3)">x</a>`,
		DescriptionFormat: "html",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// What is *stored* must be safe, not merely what is rendered: the value in the database is
	// what a browser will one day parse.
	stored, err := s.Product(ctx, p.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if stored.DescriptionFormat != "html" {
		t.Fatalf("description_format = %q, want html", stored.DescriptionFormat)
	}
	lower := strings.ToLower(stored.Description)
	for _, bad := range []string{"onerror", "alert(1)", "alert(2)", "alert(3)", "<script", "javascript:"} {
		if strings.Contains(lower, bad) {
			t.Fatalf("stored description contains %q: %s", bad, stored.Description)
		}
	}
	if !strings.Contains(stored.Description, "<h2>Hi</h2>") {
		t.Fatalf("stored description lost the good markup: %s", stored.Description)
	}

	// The plain metadata form re-sends the description unchanged and says nothing about the
	// format. That must not silently downgrade a rich description to escaped markup.
	updated, err := s.UpdateProduct(ctx, p.ID, catalog.ProductInput{
		Slug: "rich", Name: "Rich renamed", CategoryID: &cat.ID,
		Description: stored.Description,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.DescriptionFormat != "html" || updated.Description != stored.Description {
		t.Fatalf("a metadata edit changed the description format: %+v", updated)
	}

	// Changing the text without declaring a format means it was written as plain text, and the
	// format follows the text.
	plain, err := s.UpdateProduct(ctx, p.ID, catalog.ProductInput{
		Slug: "rich", Name: "Rich renamed", CategoryID: &cat.ID,
		Description: "Now plain & <safe>",
	})
	if err != nil {
		t.Fatalf("update plain: %v", err)
	}
	if plain.DescriptionFormat != "plain" || plain.Description != "Now plain & <safe>" {
		t.Fatalf("plain update = (%q, %q)", plain.Description, plain.DescriptionFormat)
	}
}

func planNames(prices []catalog.Price) []string {
	out := make([]string, 0, len(prices))
	for _, p := range prices {
		out = append(out, p.Name)
	}
	return out
}
