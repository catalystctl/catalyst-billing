package money

import (
	"errors"
	"math"
	"testing"
)

func TestParseRatio(t *testing.T) {
	t.Parallel()
	ok := []struct {
		in   string
		want string
	}{
		{"0.2", "1/5"},
		{"20%", "1/5"},
		{"7.5%", "3/40"},
		{"1/12", "1/12"},
		{"0", "0"},
		{"-5%", "-1/20"},
		{"1", "1"},
	}
	for _, tc := range ok {
		r, err := ParseRatio(tc.in)
		if err != nil {
			t.Fatalf("ParseRatio(%q): %v", tc.in, err)
		}
		if got := r.String(); got != tc.want {
			t.Fatalf("ParseRatio(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "abc", "1/", "%", "1.2.3"} {
		if _, err := ParseRatio(bad); err == nil {
			t.Fatalf("ParseRatio(%q) should have failed", bad)
		}
	}
	if _, err := NewRatio(1, 0); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("zero denominator: got %v", err)
	}
}

func TestRatioComplement(t *testing.T) {
	t.Parallel()
	r, err := ParseRatio("15%")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Complement().String(); got != "17/20" {
		t.Fatalf("complement = %s, want 17/20", got)
	}
}

func TestApplyRateRoundingModes(t *testing.T) {
	t.Parallel()
	usd := mustUSD(t)
	half, err := ParseRatio("50%")
	if err != nil {
		t.Fatal(err)
	}

	// 0.05 * 50% = 2.5 minor units: the canonical half case.
	pos := New(5, usd)
	neg := New(-5, usd)

	cases := []struct {
		mode Rounding
		in   Amount
		want int64
	}{
		{RoundHalfUp, pos, 3},
		{RoundHalfEven, pos, 2},
		{RoundTowardZero, pos, 2},
		{RoundAwayFromZero, pos, 3},
		{RoundFloor, pos, 2},
		{RoundCeil, pos, 3},
		{RoundHalfUp, neg, -3},
		{RoundHalfEven, neg, -2},
		{RoundTowardZero, neg, -2},
		{RoundAwayFromZero, neg, -3},
		{RoundFloor, neg, -3},
		{RoundCeil, neg, -2},
	}
	for _, tc := range cases {
		got, err := ApplyRate(tc.in, half, tc.mode)
		if err != nil {
			t.Fatalf("%s on %s: %v", tc.mode, tc.in, err)
		}
		if got.Minor() != tc.want {
			t.Fatalf("%s on %s = %d, want %d", tc.mode, tc.in, got.Minor(), tc.want)
		}
	}
}

func TestApplyRateRealistic(t *testing.T) {
	t.Parallel()
	usd := mustUSD(t)

	vat, err := ParseRatio("20%")
	if err != nil {
		t.Fatal(err)
	}
	net := New(1999, usd) // 19.99
	got, err := ApplyRate(net, vat, RoundHalfUp)
	if err != nil {
		t.Fatal(err)
	}
	// 19.99 * 0.20 = 3.998 -> 4.00
	if got.Minor() != 400 {
		t.Fatalf("vat = %d, want 400", got.Minor())
	}

	// 1/12 proration of an annual 120.00 = 10.00 exactly.

	twelfth, err := ParseRatio("1/12")
	if err != nil {
		t.Fatal(err)
	}
	annual := New(12000, usd)
	prorated, err := ApplyRate(annual, twelfth, RoundHalfUp)
	if err != nil {
		t.Fatal(err)
	}
	if prorated.Minor() != 1000 {
		t.Fatalf("prorated = %d, want 1000", prorated.Minor())
	}

	// Rounding must not lose the currency or change it.
	if prorated.Currency() != usd {
		t.Fatalf("currency = %s, want USD", prorated.Currency())
	}
}

func TestApplyRateOverflow(t *testing.T) {
	t.Parallel()
	usd := mustUSD(t)
	two := MustRatio(2, 1)
	if _, err := ApplyRate(New(math.MaxInt64, usd), two, RoundHalfUp); !errors.Is(err, ErrOverflow) {
		t.Fatalf("got %v, want ErrOverflow", err)
	}
	if _, err := ApplyRate(Amount{}, two, RoundHalfUp); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("invalid amount: got %v, want ErrInvalidAmount", err)
	}
	if _, err := ApplyRate(New(1, usd), two, Rounding(99)); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("invalid mode: got %v, want ErrInvalidAmount", err)
	}
}

func TestRoundingStrings(t *testing.T) {
	t.Parallel()
	for _, m := range []Rounding{RoundHalfUp, RoundHalfEven, RoundTowardZero, RoundAwayFromZero, RoundFloor, RoundCeil} {
		if !m.Valid() {
			t.Fatalf("%d should be valid", m)
		}
		if m.String() == "" {
			t.Fatalf("%d has no name", m)
		}
	}
}
