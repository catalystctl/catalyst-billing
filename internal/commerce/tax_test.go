package commerce_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/commerce"
	"github.com/catalyst/billing/internal/money"
)

// Tax resolution is the whole reason the tax table exists, so these tests assert the
// precedence order rather than "a rate was found": region beats country, country beats the
// wildcard, and the wildcard catches everything else. Getting that order wrong is how a
// US state rate silently replaces the national one.

// mustRate creates a tax rate or fails the test.
func mustRate(t *testing.T, s *commerce.Store, ctx context.Context, in commerce.TaxRateInput) commerce.TaxRate {
	t.Helper()
	r, err := s.CreateTaxRate(ctx, in)
	if err != nil {
		t.Fatalf("create tax rate %+v: %v", in, err)
	}
	return r
}

func TestTaxRateResolutionPrecedence(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)

	mustRate(t, s, ctx, commerce.TaxRateInput{
		Name: "Wildcard", CountryCode: "all", Rate: "0.05",
	})
	mustRate(t, s, ctx, commerce.TaxRateInput{
		Name: "Germany", CountryCode: "DE", Rate: "0.19",
	})
	mustRate(t, s, ctx, commerce.TaxRateInput{
		Name: "California", CountryCode: "US", RegionCode: "CA", Rate: "0.0725",
	})
	mustRate(t, s, ctx, commerce.TaxRateInput{
		Name: "United States", CountryCode: "US", Rate: "0.08",
	})

	cases := []struct {
		name    string
		country string
		region  string
		want    string
	}{
		{"region beats country", "US", "CA", "0.0725"},
		{"country when no region row", "US", "NY", "0.08"},
		{"country row", "DE", "", "0.19"},
		{"wildcard fallback", "FR", "", "0.05"},
		{"wildcard for an unknown code", "ZZ", "", "0.05"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rate, err := s.ResolveRate(ctx, tc.country, tc.region, "standard", t0)
			if err != nil {
				t.Fatalf("resolve %s/%s: %v", tc.country, tc.region, err)
			}
			if rate.Rate != tc.want {
				t.Fatalf("%s/%s rate = %s, want %s", tc.country, tc.region, rate.Rate, tc.want)
			}
		})
	}
}

// A rate change is a dated row: the old rate still explains an invoice issued before the
// change, and the new rate applies after it. The wildcard must not be needed for this — the
// lookup picks the latest effective row for the same key.
func TestTaxRateEffectiveWindow(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)

	old := t0.Add(-365 * 24 * time.Hour)
	change := t0
	if _, err := s.CreateTaxRate(ctx, commerce.TaxRateInput{
		Name: "VAT 19", CountryCode: "DE", Rate: "0.19", EffectiveFrom: &old,
	}); err != nil {
		t.Fatalf("create old rate: %v", err)
	}
	// The new rate is dated from the change; the migration's unique index requires the old
	// one to be closed first, which is exactly the operational rule this surface documents.
	if _, err := s.CreateTaxRate(ctx, commerce.TaxRateInput{
		Name: "VAT 21", CountryCode: "DE", Rate: "0.21", EffectiveFrom: &change,
	}); !errors.Is(err, commerce.ErrInUse) {
		t.Fatalf("second open-ended rate: err = %v, want ErrInUse", err)
	}

	// Close the old rate the day before the change, then append the new one.
	closeAt := change
	oldRates, err := s.TaxRates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(oldRates) != 1 {
		t.Fatalf("rates = %d, want 1", len(oldRates))
	}
	if _, err := s.UpdateTaxRate(ctx, oldRates[0].ID, commerce.TaxRateInput{
		EffectiveTo: &closeAt,
	}); err != nil {
		t.Fatalf("close old rate: %v", err)
	}
	if _, err := s.CreateTaxRate(ctx, commerce.TaxRateInput{
		Name: "VAT 21", CountryCode: "DE", Rate: "0.21", EffectiveFrom: &change,
	}); err != nil {
		t.Fatalf("append new rate: %v", err)
	}

	// Before the change the old rate applies.
	before, err := s.ResolveRate(ctx, "DE", "", "standard", t0.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("resolve before: %v", err)
	}
	if before.Rate != "0.19" {
		t.Fatalf("rate before the change = %s, want 0.19", before.Rate)
	}
	// After the change the new one applies.
	after, err := s.ResolveRate(ctx, "DE", "", "standard", t0.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("resolve after: %v", err)
	}
	if after.Rate != "0.21" {
		t.Fatalf("rate after the change = %s, want 0.21", after.Rate)
	}
}

// A rate that has taken effect cannot have its arithmetic rewritten: that would change what
// an already-issued invoice can be explained by.
func TestTaxRateArithmeticIsImmutableOnceEffective(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	from := t0.Add(-24 * time.Hour)
	rate := mustRate(t, s, ctx, commerce.TaxRateInput{
		Name: "VAT", CountryCode: "DE", Rate: "0.19", EffectiveFrom: &from,
	})
	if _, err := s.UpdateTaxRate(ctx, rate.ID, commerce.TaxRateInput{Rate: "0.21"}); !errors.Is(err, commerce.ErrInUse) {
		t.Fatalf("rewrite a live rate: err = %v, want ErrInUse", err)
	}
	// A rate that has not taken effect yet may still be corrected.
	future := t0.Add(30 * 24 * time.Hour)
	pending := mustRate(t, s, ctx, commerce.TaxRateInput{
		Name: "Future", CountryCode: "FR", Rate: "0.20", EffectiveFrom: &future,
	})
	if _, err := s.UpdateTaxRate(ctx, pending.ID, commerce.TaxRateInput{Rate: "0.21"}); err != nil {
		t.Fatalf("correct a future rate: %v", err)
	}
}

// No matching rate is ErrNotFound, not a silent zero. A tax table that invented a zero rate
// for an unconfigured jurisdiction would under-collect without anyone noticing.
func TestTaxRateUnknownJurisdictionIsNotFound(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	if _, err := s.ResolveRate(ctx, "DE", "", "standard", t0); !errors.Is(err, commerce.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------- pricer --

// The pricer charges the resolved rate, records the jurisdiction, and produces totals whose
// identity (net + tax == total) holds exactly.
func TestTaxPricerChargesResolvedRate(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	mustRate(t, s, ctx, commerce.TaxRateInput{
		Name: "Germany", CountryCode: "DE", Rate: "0.19",
	})

	pricer := commerce.TaxPricer{Store: s, CountryCode: "DE", At: t0}
	lines := []billing.Line{{
		Description:   "VPS",
		QuantityMilli: 1000,
		UnitPrice:     money.New(1000, usd),
	}}
	results, totals, evidence, err := pricer.Price(ctx, lines, usd)
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Net.Minor() != 1000 || results[0].Tax.Minor() != 190 {
		t.Fatalf("net/tax = %d/%d, want 1000/190", results[0].Net.Minor(), results[0].Tax.Minor())
	}
	if totals.Total.Minor() != 1190 {
		t.Fatalf("total = %d, want 1190", totals.Total.Minor())
	}
	if evidence.Jurisdiction != "DE/standard" {
		t.Fatalf("jurisdiction = %q, want DE/standard", evidence.Jurisdiction)
	}
	if evidence.Rate != "19/100" && evidence.Rate != "0.19" {
		t.Fatalf("evidence rate = %q", evidence.Rate)
	}
}

// An inclusive rate extracts tax from the quoted price rather than adding to it, and the
// customer is charged exactly the quoted total.
func TestTaxPricerInclusiveMode(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	inclusive := true
	mustRate(t, s, ctx, commerce.TaxRateInput{
		Name: "UK VAT", CountryCode: "GB", Rate: "0.2", Inclusive: &inclusive,
	})

	pricer := commerce.TaxPricer{Store: s, CountryCode: "GB", At: t0}
	lines := []billing.Line{{
		Description: "VPS", QuantityMilli: 1000, UnitPrice: money.New(1200, usd),
	}}
	results, totals, evidence, err := pricer.Price(ctx, lines, usd)
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	if totals.Total.Minor() != 1200 {
		t.Fatalf("inclusive total = %d, want 1200 (the quoted price)", totals.Total.Minor())
	}
	if results[0].Tax.Minor() != 200 {
		t.Fatalf("extracted tax = %d, want 200", results[0].Tax.Minor())
	}
	if results[0].Net.Minor() != 1000 {
		t.Fatalf("net = %d, want 1000", results[0].Net.Minor())
	}
	if evidence.Mode != "inclusive" {
		t.Fatalf("mode = %q, want inclusive", evidence.Mode)
	}
}

// A tax-exempt customer is charged zero with a reason, and the reason is what makes the zero
// defensible.
func TestTaxPricerExemptionRecordsReason(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	mustRate(t, s, ctx, commerce.TaxRateInput{
		Name: "Germany", CountryCode: "DE", Rate: "0.19",
	})
	pricer := commerce.TaxPricer{
		Store: s, CountryCode: "DE", At: t0,
		Exempt: true, ExemptionReason: "reverse charge, VAT ID IE1234567",
	}
	lines := []billing.Line{{
		Description: "VPS", QuantityMilli: 1000, UnitPrice: money.New(1000, usd),
	}}
	_, totals, evidence, err := pricer.Price(ctx, lines, usd)
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	if totals.Tax.Minor() != 0 {
		t.Fatalf("exempt tax = %d, want 0", totals.Tax.Minor())
	}
	if evidence.ExemptionReason == "" {
		t.Fatal("an exempt charge must record why")
	}
}

// A jurisdiction with no configured rate charges nothing and says so. The alternative —
// charging a guessed rate, or silently recording "0" as a resolved rate — is the failure mode
// a tax table exists to prevent.
func TestTaxPricerNoRateRecordsReason(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	pricer := commerce.TaxPricer{Store: s, CountryCode: "DE", At: t0}
	lines := []billing.Line{{
		Description: "VPS", QuantityMilli: 1000, UnitPrice: money.New(1000, usd),
	}}
	_, totals, evidence, err := pricer.Price(ctx, lines, usd)
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	if totals.Tax.Minor() != 0 {
		t.Fatalf("tax = %d, want 0", totals.Tax.Minor())
	}
	if evidence.ExemptionReason == "" {
		t.Fatal("an unconfigured jurisdiction must record why no tax was charged")
	}
	if evidence.Policy != "no-rate" {
		t.Fatalf("policy = %q, want no-rate", evidence.Policy)
	}
}

// A wildcard rate applies when the country has no row of its own, which is what makes a
// single "rest of world" rate expressible without listing every country.
func TestTaxPricerWildcardFallback(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	mustRate(t, s, ctx, commerce.TaxRateInput{
		Name: "Rest of world", CountryCode: "all", Rate: "0.1",
	})
	pricer := commerce.TaxPricer{Store: s, CountryCode: "JP", At: t0}
	lines := []billing.Line{{
		Description: "VPS", QuantityMilli: 1000, UnitPrice: money.New(1000, usd),
	}}
	_, totals, evidence, err := pricer.Price(ctx, lines, usd)
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	if totals.Tax.Minor() != 100 {
		t.Fatalf("tax = %d, want 100", totals.Tax.Minor())
	}
	if evidence.Jurisdiction != "all/standard" {
		t.Fatalf("jurisdiction = %q, want all/standard", evidence.Jurisdiction)
	}
}

// With tax disabled, a configured rate must not start charging: the setting is the master
// switch.
func TestTaxEnabledReadsSetting(t *testing.T) {
	s, ctx := newStore(t)
	at(t, s, t0)
	enabled, err := s.TaxEnabled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("tax should be disabled by default")
	}
	if _, err := s.Pool().Exec(ctx,
		`UPDATE settings SET value = 'true' WHERE group_key = 'tax' AND key = 'enabled'`); err != nil {
		t.Fatal(err)
	}
	enabled, err = s.TaxEnabled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatal("tax should be enabled after the setting change")
	}
}
