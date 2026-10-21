package money

import (
	"errors"
	"math"
	"testing"
)

func mustUSD(t *testing.T) Currency {
	t.Helper()
	c, ok := LookupCurrency("USD")
	if !ok {
		t.Fatal("USD missing from ISO table")
	}
	return c
}

func TestCurrencyValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		code    string
		scale   uint8
		wantErr bool
	}{
		{"usd", "USD", 2, false},
		{"lowercase normalised", "usd", 2, false},
		{"jpy", "JPY", 0, false},
		{"kwd", "KWD", 3, false},
		{"max scale", "CLF", 4, false},
		{"scale too large", "USD", 5, true},
		{"too short", "U", 2, true},
		{"too long", "ABCDEFGHIJKLM", 2, true},
		{"invalid rune", "US$", 2, true},
		{"empty", "", 2, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewCurrency(tc.code, tc.scale)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidCurrency) {
					t.Fatalf("want ErrInvalidCurrency, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.Scale() != tc.scale {
				t.Fatalf("scale = %d, want %d", c.Scale(), tc.scale)
			}
		})
	}
}

func TestLookupCurrencyScales(t *testing.T) {
	t.Parallel()
	cases := map[string]uint8{"USD": 2, "EUR": 2, "JPY": 0, "KWD": 3, "CLF": 4, "VND": 0}
	for code, scale := range cases {
		c, ok := LookupCurrency(code)
		if !ok {
			t.Fatalf("%s not found", code)
		}
		if c.Scale() != scale {
			t.Fatalf("%s scale = %d, want %d", code, c.Scale(), scale)
		}
	}
	if _, ok := LookupCurrency("XYZ"); ok {
		t.Fatal("unknown currency should not resolve")
	}
}

func TestAmountArithmetic(t *testing.T) {
	t.Parallel()
	usd := mustUSD(t)
	eur := MustLookupCurrency("EUR")

	a := New(1234, usd)
	b := New(66, usd)

	sum, err := a.Add(b)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if sum.Minor() != 1300 {
		t.Fatalf("sum = %d, want 1300", sum.Minor())
	}

	diff, err := a.Sub(b)
	if err != nil {
		t.Fatalf("sub: %v", err)
	}
	if diff.Minor() != 1168 {
		t.Fatalf("diff = %d, want 1168", diff.Minor())
	}

	if _, err := a.Add(New(1, eur)); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("cross-currency add: got %v, want ErrCurrencyMismatch", err)
	}
	if _, err := a.Cmp(New(1, eur)); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("cross-currency cmp: got %v, want ErrCurrencyMismatch", err)
	}

	// The zero value carries no currency and must be rejected, not treated as 0.
	var invalid Amount
	if invalid.IsValid() {
		t.Fatal("zero value should be invalid")
	}
	if _, err := invalid.Add(a); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("invalid add: got %v, want ErrInvalidAmount", err)
	}
	if _, err := New(1, Currency{}).Add(New(1, Currency{})); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("empty-currency add: got %v, want ErrInvalidAmount", err)
	}
	if New(0, Currency{}).IsZero() {
		t.Fatal("invalid amount must not report IsZero")
	}
	if a.Equal(New(1234, eur)) {
		t.Fatal("different currencies must not be equal")
	}
	if invalid.Equal(invalid) {
		t.Fatal("invalid amounts must never be equal")
	}
}

func TestAmountOverflow(t *testing.T) {
	t.Parallel()
	usd := mustUSD(t)
	max := New(math.MaxInt64, usd)
	min := New(math.MinInt64, usd)

	if _, err := max.Add(New(1, usd)); !errors.Is(err, ErrOverflow) {
		t.Fatalf("max+1: got %v, want ErrOverflow", err)
	}
	if _, err := min.Sub(New(1, usd)); !errors.Is(err, ErrOverflow) {
		t.Fatalf("min-1: got %v, want ErrOverflow", err)
	}
	if _, err := min.Neg(); !errors.Is(err, ErrOverflow) {
		t.Fatalf("neg(min): got %v, want ErrOverflow", err)
	}
	if _, err := max.MulInt(2); !errors.Is(err, ErrOverflow) {
		t.Fatalf("max*2: got %v, want ErrOverflow", err)
	}
	// MinInt64 must format without wrapping through int64 negation.
	if got, want := min.String(), "-92233720368547758.08"; got != want {
		t.Fatalf("min.String() = %q, want %q", got, want)
	}
}

func TestParseAndFormat(t *testing.T) {
	t.Parallel()
	usd := mustUSD(t)
	jpy := MustLookupCurrency("JPY")
	kwd := MustLookupCurrency("KWD")

	ok := []struct {
		in   string
		cur  Currency
		want int64
	}{
		{"0", usd, 0},
		{"12.34", usd, 1234},
		{"-12.34", usd, -1234},
		{".5", usd, 50},
		{"0.05", usd, 5},
		{"1.2", usd, 120},
		{"  12.34  ", usd, 1234},
		{"007.10", usd, 710},
		{"1234", jpy, 1234},
		{"12.345", kwd, 12345},
		{"92233720368547758.07", usd, math.MaxInt64},
	}
	for _, tc := range ok {
		a, err := Parse(tc.in, tc.cur)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.in, err)
		}
		if a.Minor() != tc.want {
			t.Fatalf("Parse(%q) = %d, want %d", tc.in, a.Minor(), tc.want)
		}
		if a.Currency() != tc.cur {
			t.Fatalf("Parse(%q) currency = %s, want %s", tc.in, a.Currency(), tc.cur)
		}
		// Round trip through String must preserve the value exactly.
		reparsed, err := Parse(a.String(), tc.cur)
		if err != nil {
			t.Fatalf("re-Parse(%q): %v", a.String(), err)
		}
		if !reparsed.Equal(a) {
			t.Fatalf("round trip changed %q -> %q", a.String(), reparsed.String())
		}
	}

	bad := []struct {
		in  string
		cur Currency
	}{
		{"1.234", usd},                // would require rounding
		{"", usd},                     // empty
		{"-", usd},                    // sign only
		{"1.", usd},                   // trailing point
		{"+1", usd},                   // explicit plus
		{"1,000", usd},                // grouping separator
		{"1e3", usd},                  // exponent
		{"$1", usd},                   // symbol
		{"12.5", jpy},                 // JPY has no minor units
		{"92233720368547758.08", usd}, // overflow
		{"abc", usd},
	}
	for _, tc := range bad {
		if _, err := Parse(tc.in, tc.cur); err == nil {
			t.Fatalf("Parse(%q) should have failed", tc.in)
		}
	}
}

func TestStringScales(t *testing.T) {
	t.Parallel()
	usd := mustUSD(t)
	jpy := MustLookupCurrency("JPY")
	kwd := MustLookupCurrency("KWD")

	cases := []struct {
		minor int64
		cur   Currency
		want  string
	}{
		{0, usd, "0.00"},
		{5, usd, "0.05"},
		{-5, usd, "-0.05"},
		{1234, usd, "12.34"},
		{-1234, usd, "-12.34"},
		{100, usd, "1.00"},
		{7, jpy, "7"},
		{-7, jpy, "-7"},
		{12345, kwd, "12.345"},
	}
	for _, tc := range cases {
		if got := New(tc.minor, tc.cur).String(); got != tc.want {
			t.Fatalf("New(%d, %s).String() = %q, want %q", tc.minor, tc.cur, got, tc.want)
		}
	}
}

func FuzzParse(f *testing.F) {
	usd := MustLookupCurrency("USD")
	for _, seed := range []string{"0", "12.34", "-1.05", ".5", "", "1.234", "999999999999999999999"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		a, err := Parse(s, usd)
		if err != nil {
			return
		}
		if a.Minor() > 0 && s[0] == '-' {
			t.Fatalf("negative input parsed positive: %q", s)
		}
		back, err := Parse(a.String(), usd)
		if err != nil {
			t.Fatalf("String() not re-parseable: %q -> %q: %v", s, a.String(), err)
		}
		if !back.Equal(a) {
			t.Fatalf("round trip mismatch: %q -> %q -> %d vs %d", s, a.String(), back.Minor(), a.Minor())
		}
	})
}
