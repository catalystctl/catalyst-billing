package service_test

// The proration proof, in the same shape as the calendar proof: the pre-change proration
// arithmetic is frozen here as an independent implementation, and the live one is compared
// against it over a randomised space of periods and change instants — including a month end, a
// leap day and a DST transition. A day-count period is then measured by the *same* arithmetic,
// which is the property that makes it correct rather than approximately correct.

import (
	"math/rand"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/service"
)

// legacyProration is the pre-change ComputeProration, copied verbatim: the remaining fraction
// of [start, end) at asOf, clamped into the period, as an exact rational of elapsed time.
func legacyProration(start, end, asOf time.Time) (remaining, total time.Duration, ratio string) {
	if !end.After(start) {
		return 0, 0, "invalid"
	}
	total = end.Sub(start)
	remaining = end.Sub(asOf)
	if remaining > total {
		remaining = total
	}
	if remaining < 0 {
		remaining = 0
	}
	r, err := money.NewRatio(int64(remaining), int64(total))
	if err != nil {
		return remaining, total, "invalid"
	}
	return remaining, total, r.String()
}

// TestProrationIsByteIdenticalToThePreChangeAlgorithm compares the live proration with the
// frozen one over 3000 random periods in six timezones, including change instants before,
// inside and after each period.
func TestProrationIsByteIdenticalToThePreChangeAlgorithm(t *testing.T) {
	t.Parallel()
	zones := []string{
		"UTC", "Europe/Berlin", "America/New_York", "Asia/Kolkata",
		"Australia/Lord_Howe", "Pacific/Auckland",
	}
	locs := make([]*time.Location, len(zones))
	for i, z := range zones {
		loc, err := time.LoadLocation(z)
		if err != nil {
			t.Fatalf("load %s: %v", z, err)
		}
		locs[i] = loc
	}
	rng := rand.New(rand.NewSource(20261024))

	for iter := 0; iter < 3000; iter++ {
		loc := locs[rng.Intn(len(locs))]
		// Periods of the awkward lengths on purpose: a 28-day February, a 29-day leap
		// February, a 30- and a 31-day month, a 45-day day-count period, and one that spans
		// the spring transition.
		start := time.Date(2024+rng.Intn(6), time.Month(1+rng.Intn(12)), 1+rng.Intn(28),
			rng.Intn(24), rng.Intn(60), 0, 0, loc)
		lengths := []time.Duration{
			24 * time.Hour,
			7 * 24 * time.Hour,
			28 * 24 * time.Hour,
			29 * 24 * time.Hour,
			30 * 24 * time.Hour,
			31 * 24 * time.Hour,
			45 * 24 * time.Hour,
			365 * 24 * time.Hour,
		}
		end := start.Add(lengths[rng.Intn(len(lengths))])

		// An instant before the period, inside it at a random offset, exactly on the
		// boundary, and after it.
		asOf := []time.Time{
			start.Add(-time.Duration(rng.Intn(72)) * time.Hour),
			start.Add(time.Duration(rng.Int63n(int64(end.Sub(start) + 1)))),
			start,
			end,
			end.Add(time.Duration(rng.Intn(72)) * time.Hour),
		}[rng.Intn(5)]

		live, err := service.ComputeProration(start, end, asOf)
		if err != nil {
			t.Fatalf("iter %d: ComputeProration(%s, %s, %s): %v", iter, start, end, asOf, err)
		}
		wantRemaining, wantTotal, wantRatio := legacyProration(start, end, asOf)

		if live.Total != wantTotal {
			t.Fatalf("iter %d: total = %s, pre-change = %s (period %s..%s)",
				iter, live.Total, wantTotal, start, end)
		}
		if live.Remaining != wantRemaining {
			t.Fatalf("iter %d: remaining = %s, pre-change = %s (as of %s in %s..%s)",
				iter, live.Remaining, wantRemaining, asOf, start, end)
		}
		if live.Ratio.String() != wantRatio {
			t.Fatalf("iter %d: ratio = %s, pre-change = %s (remaining %s of %s)",
				iter, live.Ratio.String(), wantRatio, live.Remaining, live.Total)
		}
		if live.AsOf != asOf && !live.AsOf.Equal(asOf) {
			t.Fatalf("iter %d: asOf = %s, want %s", iter, live.AsOf, asOf)
		}
	}
}

// TestProratedDifferenceIsByteIdenticalToThePreChangeAlgorithm does the same for the amount:
// (to - from) * ratio, rounded once with an explicit mode, in integer minor units.
func TestProratedDifferenceIsByteIdenticalToThePreChangeAlgorithm(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(20261025))
	cur := money.MustLookupCurrency("USD")

	// A fixed set of amounts including 0, 1 minor unit, a round number and one chosen so the
	// product lands exactly on a half unit (the case a rounding mode has to decide).
	amounts := []int64{0, 1, 7, 999, 1000, 4500, 12345, 999999, 1 << 40}

	for iter := 0; iter < 2000; iter++ {
		start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		end := start.Add(time.Duration(1+rng.Intn(400)) * 24 * time.Hour)
		asOf := start.Add(time.Duration(rng.Int63n(int64(end.Sub(start) + 1))))

		p, err := service.ComputeProration(start, end, asOf)
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		from := money.New(amounts[rng.Intn(len(amounts))], cur)
		to := money.New(amounts[rng.Intn(len(amounts))], cur)
		mode := []money.Rounding{money.RoundHalfUp, money.RoundHalfEven, money.RoundTowardZero}[rng.Intn(3)]

		live, err := service.ProratedDifference(from, to, p, mode)
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}

		// The frozen arithmetic: subtract, then apply the exact rational once.
		delta := to.Minor() - from.Minor()
		want, err := money.ApplyRate(money.New(delta, cur), p.Ratio, mode)
		if err != nil {
			t.Fatalf("iter %d: frozen: %v", iter, err)
		}
		if live.Minor() != want.Minor() {
			t.Fatalf("iter %d: prorated = %d, pre-change = %d (delta %d, ratio %s, mode %v)",
				iter, live.Minor(), want.Minor(), delta, p.Ratio.String(), mode)
		}
		if live.Currency() != want.Currency() {
			t.Fatalf("iter %d: currency = %s, want %s", iter, live.Currency(), want.Currency())
		}
	}
}

// TestProrationOnDayCountPeriodIsTheSameArithmetic: a day-count period is not prorated by a
// different rule. The 45-day period is measured by its real length and the *existing*
// rational arithmetic is applied to it, so a mid-cycle change costs exactly what the same
// change would cost in a 45-day calendar period.
func TestProrationOnDayCountPeriodIsTheSameArithmetic(t *testing.T) {
	t.Parallel()
	start := time.Date(2028, 1, 20, 0, 0, 0, 0, time.UTC) // crosses 2028-02-29
	end := start.Add(45 * 24 * time.Hour)                 // 2028-03-05
	asOf := start.Add(18 * 24 * time.Hour)

	live, err := service.ComputeProration(start, end, asOf)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	wantRemaining, wantTotal, wantRatio := legacyProration(start, end, asOf)
	if live.Total != wantTotal || live.Remaining != wantRemaining || live.Ratio.String() != wantRatio {
		t.Fatalf("day-count proration = %s (%s of %s), pre-change arithmetic = %s (%s of %s)",
			live.Ratio.String(), live.Remaining, live.Total, wantRatio, wantRemaining, wantTotal)
	}
	// 27 of 45 days remain: 3/5 exactly.
	if got, want := live.Ratio.String(), "3/5"; got != want {
		t.Fatalf("ratio = %s, want %s", got, want)
	}
	cur := money.MustLookupCurrency("USD")
	amount, err := service.ProratedDifference(money.New(0, cur), money.New(4500, cur), live, money.RoundHalfUp)
	if err != nil {
		t.Fatalf("prorated: %v", err)
	}
	if amount.Minor() != 2700 {
		t.Fatalf("prorated = %d, want 2700 (45.00 * 3/5, exactly)", amount.Minor())
	}
}

// TestNoThirtyPointFourFourDayMonth is the specific mistake this feature must not make: a
// proration divisor averaged over a month. A 45-day period and a 30.44-day "month" would give
// the same fraction for every change instant; the real arithmetic does not.
func TestNoThirtyPointFourFourDayMonth(t *testing.T) {
	t.Parallel()
	start := time.Date(2028, 1, 20, 0, 0, 0, 0, time.UTC)
	end := start.Add(45 * 24 * time.Hour)

	// One day in, the exact fraction is 44/45. An averaged month would say 30.44/45.
	p, err := service.ComputeProration(start, end, start.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if got, want := p.Ratio.String(), "44/45"; got != want {
		t.Fatalf("ratio = %s, want %s", got, want)
	}
	// 44/45 is not 30.44/45: the denominator is the real period, not an average.
	if p.Total != 45*24*time.Hour {
		t.Fatalf("total = %s, want exactly 45 days", p.Total)
	}
}
