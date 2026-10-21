package money

import (
	"errors"
	"math/rand"
	"testing"
)

func TestAllocateBasics(t *testing.T) {
	t.Parallel()
	usd := mustUSD(t)

	// A cent split three ways cannot be equal: the residual must land somewhere
	// deterministic and the sum must still be exact.
	parts, err := Allocate(New(100, usd), []int64{1, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	assertParts(t, parts, []int64{34, 33, 33})

	parts, err = Allocate(New(5, usd), []int64{1, 1})
	if err != nil {
		t.Fatal(err)
	}
	assertParts(t, parts, []int64{3, 2})

	parts, err = Allocate(New(-5, usd), []int64{1, 1})
	if err != nil {
		t.Fatal(err)
	}
	assertParts(t, parts, []int64{-3, -2})

	parts, err = Allocate(New(100, usd), []int64{1, 3})
	if err != nil {
		t.Fatal(err)
	}
	assertParts(t, parts, []int64{25, 75})

	// A zero weight receives nothing when the remainder ordering is decisive.
	parts, err = Allocate(New(100, usd), []int64{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	assertParts(t, parts, []int64{0, 100})

	parts, err = Allocate(New(0, usd), []int64{5, 5})
	if err != nil {
		t.Fatal(err)
	}
	assertParts(t, parts, []int64{0, 0})
}

func TestAllocateErrors(t *testing.T) {
	t.Parallel()
	usd := mustUSD(t)
	if _, err := Allocate(New(1, usd), nil); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("nil weights: %v", err)
	}
	if _, err := Allocate(New(1, usd), []int64{0, 0}); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("zero weights: %v", err)
	}
	if _, err := Allocate(New(1, usd), []int64{1, -1}); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("negative weight: %v", err)
	}
	if _, err := Allocate(Amount{}, []int64{1}); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("invalid total: %v", err)
	}
	if _, err := Split(New(1, usd), 0); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("zero split: %v", err)
	}
}

func TestSum(t *testing.T) {
	t.Parallel()
	usd := mustUSD(t)
	eur := MustLookupCurrency("EUR")
	total, err := Sum(usd, New(100, usd), New(250, usd))
	if err != nil {
		t.Fatal(err)
	}
	if total.Minor() != 350 {
		t.Fatalf("sum = %d, want 350", total.Minor())
	}
	empty, err := Sum(usd)
	if err != nil {
		t.Fatal(err)
	}
	if !empty.IsZero() || empty.Currency() != usd {
		t.Fatalf("empty sum = %s, want 0.00 USD", empty.Format())
	}
	if _, err := Sum(usd, New(1, usd), New(1, eur)); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("mixed sum: %v", err)
	}
}

func assertParts(t *testing.T, got []Amount, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len(parts) = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Minor() != want[i] {
			t.Fatalf("parts[%d] = %d, want %d (all: %v)", i, got[i].Minor(), want[i], got)
		}
	}
}

// TestAllocateProperties is the property test required by the plan: for random
// totals and weights the parts always sum exactly to the total, and no part is
// more than one minor unit from its exact proportional share.
func TestAllocateProperties(t *testing.T) {
	t.Parallel()
	usd := mustUSD(t)
	rng := rand.New(rand.NewSource(20261021))

	for iter := 0; iter < 5000; iter++ {
		n := 1 + rng.Intn(12)
		weights := make([]int64, n)
		var weightSum int64
		for i := range weights {
			weights[i] = int64(rng.Intn(1000))
			weightSum += weights[i]
		}
		if weightSum == 0 {
			weights[0] = 1
			weightSum = 1
		}
		totalMinor := rng.Int63n(2_000_000_001) - 1_000_000_000
		total := New(totalMinor, usd)

		parts, err := Allocate(total, weights)
		if err != nil {
			t.Fatalf("iter %d: allocate(%d, %v): %v", iter, totalMinor, weights, err)
		}

		var sum int64
		for _, p := range parts {
			sum += p.Minor()
		}
		if sum != totalMinor {
			t.Fatalf("iter %d: sum %d != total %d (weights %v, parts %v)", iter, sum, totalMinor, weights, parts)
		}

		// |part - total*w/W| <= 1, evaluated in exact integer arithmetic.
		for i, p := range parts {
			exactNum := totalMinor * weights[i]
			diff := int64(p.Minor())*weightSum - exactNum
			if diff < 0 {
				diff = -diff
			}
			if diff > weightSum {
				t.Fatalf("iter %d: part %d (%d) deviates from exact share (total %d, weight %d/%d)",
					iter, i, p.Minor(), totalMinor, weights[i], weightSum)
			}
		}
		for _, p := range parts {
			if p.Currency() != usd {
				t.Fatalf("iter %d: part lost currency: %s", iter, p.Currency())
			}
		}
	}
}

func TestSplitProperties(t *testing.T) {
	t.Parallel()
	usd := mustUSD(t)
	rng := rand.New(rand.NewSource(7))
	for iter := 0; iter < 2000; iter++ {
		n := 1 + rng.Intn(20)
		totalMinor := rng.Int63n(1_000_001) - 500_000
		parts, err := Split(New(totalMinor, usd), n)
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		if len(parts) != n {
			t.Fatalf("len = %d, want %d", len(parts), n)
		}
		var sum int64
		min, max := parts[0].Minor(), parts[0].Minor()
		for _, p := range parts {
			sum += p.Minor()
			if p.Minor() < min {
				min = p.Minor()
			}
			if p.Minor() > max {
				max = p.Minor()
			}
		}
		if sum != totalMinor {
			t.Fatalf("iter %d: sum %d != %d", iter, sum, totalMinor)
		}
		if max-min > 1 {
			t.Fatalf("iter %d: split uneven by %d (total %d, n %d)", iter, max-min, totalMinor, n)
		}
	}
}

// TestTaxThenDiscountOrderMatters documents that the rounding stage is part of
// the contract: applying tax to a rounded subtotal differs from applying it to
// the exact amount, so the pipeline must be fixed and snapshot.
func TestRoundingStageIsExplicit(t *testing.T) {
	t.Parallel()
	usd := mustUSD(t)
	vat, err := ParseRatio("7.5%")
	if err != nil {
		t.Fatal(err)
	}
	line := New(333, usd) // 3.33

	// Tax on the line total.
	tax, err := ApplyRate(line, vat, RoundHalfUp)
	if err != nil {
		t.Fatal(err)
	}
	if tax.Minor() != 25 { // 24.975 -> 25
		t.Fatalf("tax = %d, want 25", tax.Minor())
	}
	gross, err := line.Add(tax)
	if err != nil {
		t.Fatal(err)
	}
	if gross.Minor() != 358 {
		t.Fatalf("gross = %d, want 358", gross.Minor())
	}
}
