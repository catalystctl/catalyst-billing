package commerce_test

import (
	"context"
	"errors"
	"testing"

	"github.com/catalyst/billing/internal/commerce"
	"github.com/catalyst/billing/internal/store"
)

// resetCurrencies restores the seeded currency rows.
//
// `currencies` is reference data that the test harness deliberately does not truncate, because
// a fresh installation must be able to take a payment without an operator configuring anything.
// That means a test which edits it leaks into the next test, so the editing tests restore it
// first. This is the same reason the harness re-seeds `settings` and `billing_policies`.
func resetCurrencies(t *testing.T, pool *store.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM currencies`); err != nil {
		t.Fatalf("clear currencies: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO currencies (code, name, scale, symbol, symbol_position, is_default, sort_order) VALUES
			('USD', 'US Dollar',        2, '$',  'prefix', true,  10),
			('EUR', 'Euro',             2, '€',  'prefix', false, 20),
			('GBP', 'Pound Sterling',   2, '£',  'prefix', false, 30),
			('CAD', 'Canadian Dollar',  2, 'CA$','prefix', false, 40),
			('AUD', 'Australian Dollar',2, 'A$', 'prefix', false, 50),
			('JPY', 'Japanese Yen',     0, '¥',  'prefix', false, 60)`); err != nil {
		t.Fatalf("restore currencies: %v", err)
	}
}

// A currency supplies presentation and nothing else: no exchange rate, no arithmetic. So the
// properties worth asserting are "the format is a string insertion over integer minor units"
// and "exactly one default exists at every commit".

func TestCurrencyFormattingIsExact(t *testing.T) {
	s, ctx := newStore(t)
	cur, err := s.Currency(ctx, "USD")
	if err != nil {
		t.Fatalf("read USD: %v", err)
	}
	cases := []struct {
		minor int64
		want  string
	}{
		{0, "$0.00"},
		{5, "$0.05"},
		{123456, "$1,234.56"},
		{-123456, "-$1,234.56"},
		{100000000, "$1,000,000.00"},
	}
	for _, tc := range cases {
		if got := cur.Format(tc.minor); got != tc.want {
			t.Errorf("USD %d = %q, want %q", tc.minor, got, tc.want)
		}
	}

	// A zero-decimal currency must never render cents: JPY 1000 is ¥1,000, not ¥10.00.
	jpy, err := s.Currency(ctx, "JPY")
	if err != nil {
		t.Fatalf("read JPY: %v", err)
	}
	if got := jpy.Format(1000); got != "¥1,000" {
		t.Fatalf("JPY 1000 = %q, want ¥1,000", got)
	}
}

// A suffix currency and a comma decimal mark render the other way round, which is what makes
// the configuration worth having.
func TestCurrencyFormattingHonoursSuffixAndDecimalMark(t *testing.T) {
	s, ctx := newStore(t)
	group := "."
	mark := ","
	scale := uint8(2)
	if _, err := s.CreateCurrency(ctx, commerce.CurrencyInput{
		Code: "TEST", Name: "Test", Scale: &scale,
		Symbol: "T", SymbolPosition: "suffix",
		GroupSeparator: &group, DecimalMark: &mark,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	cur, err := s.Currency(ctx, "TEST")
	if err != nil {
		t.Fatal(err)
	}
	if got := cur.Format(123456); got != "1.234,56 T" {
		t.Fatalf("format = %q, want \"1.234,56 T\"", got)
	}
	// Grouping can be turned off entirely.
	none := ""
	size := 0
	if _, err := s.UpdateCurrency(ctx, "TEST", commerce.CurrencyInput{
		GroupSeparator: &none, GroupSize: &size,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	cur, err = s.Currency(ctx, "TEST")
	if err != nil {
		t.Fatal(err)
	}
	if got := cur.Format(123456); got != "1234,56 T" {
		t.Fatalf("ungrouped format = %q, want \"1234,56 T\"", got)
	}
}

// Exactly one currency is the default, and making another one the default demotes the
// incumbent in the same transaction. The migration's partial unique index is the backstop; this
// asserts the application never relies on it.
func TestCurrencyExactlyOneDefault(t *testing.T) {
	s, ctx := newStore(t)
	def, err := s.DefaultCurrency(ctx)
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if def.Code != "USD" {
		t.Fatalf("seeded default = %s, want USD", def.Code)
	}

	yes := true
	if _, err := s.UpdateCurrency(ctx, "EUR", commerce.CurrencyInput{IsDefault: &yes}); err != nil {
		t.Fatalf("promote EUR: %v", err)
	}
	def, err = s.DefaultCurrency(ctx)
	if err != nil {
		t.Fatalf("default after promote: %v", err)
	}
	if def.Code != "EUR" {
		t.Fatalf("default after promote = %s, want EUR", def.Code)
	}
	var defaults int
	if err := s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM currencies WHERE is_default`).Scan(&defaults); err != nil {
		t.Fatal(err)
	}
	if defaults != 1 {
		t.Fatalf("defaults = %d, want exactly 1", defaults)
	}

	// Demoting the default promotes a replacement rather than leaving none.
	no := false
	if _, err := s.UpdateCurrency(ctx, "EUR", commerce.CurrencyInput{IsDefault: &no}); err != nil {
		t.Fatalf("demote EUR: %v", err)
	}
	if _, err := s.DefaultCurrency(ctx); err != nil {
		t.Fatalf("a default must still exist after a demotion: %v", err)
	}
}

// The default currency cannot be disabled: a storefront with no default has no currency to
// create a price in.
func TestDefaultCurrencyCannotBeDisabled(t *testing.T) {
	s, ctx := newStore(t)
	no := false
	if _, err := s.UpdateCurrency(ctx, "USD", commerce.CurrencyInput{Enabled: &no}); !errors.Is(err, commerce.ErrInUse) {
		t.Fatalf("disable the default: err = %v, want ErrInUse", err)
	}
	// A non-default currency can be disabled, and disappears from the public list.
	if _, err := s.UpdateCurrency(ctx, "GBP", commerce.CurrencyInput{Enabled: &no}); err != nil {
		t.Fatalf("disable GBP: %v", err)
	}
	enabled, err := s.EnabledCurrencies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range enabled {
		if c.Code == "GBP" {
			t.Fatal("a disabled currency must not be offered at checkout")
		}
	}
	// It is still readable for staff, because its history references it.
	if _, err := s.Currency(ctx, "GBP"); err != nil {
		t.Fatalf("a disabled currency must still be readable: %v", err)
	}
}

// A currency referenced by a price version cannot be deleted: the price row would point at
// nothing. Disabling is the supported way to stop offering it.
func TestCurrencyInUseCannotBeDeleted(t *testing.T) {
	s, ctx := newStore(t)
	pool := s.Pool()
	mustCustomer(t, pool, "currency-inuse@example.test")
	mustProduct(t, pool, "vps-currency", 1000, 0, "GBP")

	if err := s.DeleteCurrency(ctx, "GBP"); !errors.Is(err, commerce.ErrInUse) {
		t.Fatalf("delete a currency with prices: err = %v, want ErrInUse", err)
	}
	// The default is refused even with no prices.
	if err := s.DeleteCurrency(ctx, "USD"); !errors.Is(err, commerce.ErrInUse) {
		t.Fatalf("delete the default: err = %v, want ErrInUse", err)
	}
	// An unused currency can be removed.
	if err := s.DeleteCurrency(ctx, "AUD"); err != nil {
		t.Fatalf("delete an unused currency: %v", err)
	}
	if _, err := s.Currency(ctx, "AUD"); !errors.Is(err, commerce.ErrNotFound) {
		t.Fatalf("deleted currency: err = %v, want ErrNotFound", err)
	}
}

// Changing a currency's scale would reinterpret every stored minor amount, so it is refused.
func TestCurrencyScaleIsImmutable(t *testing.T) {
	s, ctx := newStore(t)
	scale := uint8(3)
	if _, err := s.UpdateCurrency(ctx, "USD", commerce.CurrencyInput{Scale: &scale}); !errors.Is(err, commerce.ErrInUse) {
		t.Fatalf("change scale: err = %v, want ErrInUse", err)
	}
}

// A duplicate code is a conflict, not a 500.
func TestCurrencyCodeIsUnique(t *testing.T) {
	s, ctx := newStore(t)
	_, err := s.CreateCurrency(ctx, commerce.CurrencyInput{Code: "USD", Name: "Another dollar"})
	if !errors.Is(err, commerce.ErrCodeTaken) {
		t.Fatalf("duplicate code: err = %v, want ErrCodeTaken", err)
	}
}
