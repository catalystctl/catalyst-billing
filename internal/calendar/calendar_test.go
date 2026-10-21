package calendar

import (
	"math/rand"
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return loc
}

func mustSchedule(t *testing.T, iv Interval, day, hour, minute int, loc *time.Location) *Schedule {
	t.Helper()
	a, err := NewAnchor(day, hour, minute)
	if err != nil {
		t.Fatalf("anchor: %v", err)
	}
	s, err := NewSchedule(iv, a, loc)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	return s
}

func TestMonthlyAnniversary(t *testing.T) {
	t.Parallel()
	s := mustSchedule(t, Monthly, 15, 0, 0, time.UTC)
	p := s.PeriodStartingIn(2026, time.January)

	if got, want := p.Start.UTC(), time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("start = %s, want %s", got, want)
	}
	if got, want := p.End.UTC(), time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("end = %s, want %s", got, want)
	}
	if got, want := p.Key(), "2026-01-15/2026-02-15"; got != want {
		t.Fatalf("key = %q, want %q", got, want)
	}
	if d := p.Duration(); d != 31*24*time.Hour {
		t.Fatalf("duration = %s, want 744h", d)
	}
}

// Acceptance scenario 7, part 1: month-end anchors clamp to the last valid day
// while retaining the original anchor rule.
func TestMonthEndAnchorRetainsRule(t *testing.T) {
	t.Parallel()
	s := mustSchedule(t, Monthly, 31, 0, 0, time.UTC)

	want := []string{
		"2026-01-31/2026-02-28",
		"2026-02-28/2026-03-31",
		"2026-03-31/2026-04-30",
		"2026-04-30/2026-05-31",
		"2026-05-31/2026-06-30",
		"2026-06-30/2026-07-31",
		"2026-07-31/2026-08-31",
		"2026-08-31/2026-09-30",
	}
	p := s.PeriodStartingIn(2026, time.January)
	for i, w := range want {
		if got := p.Key(); got != w {
			t.Fatalf("period %d key = %q, want %q", i, got, w)
		}
		// The nominal anchor day must never drift.
		if p.NominalStart().Day != 31 {
			t.Fatalf("period %d nominal day = %d, want 31", i, p.NominalStart().Day)
		}
		p = p.Next()
	}
}

func TestMonthEndAnchorBackwards(t *testing.T) {
	t.Parallel()
	s := mustSchedule(t, Monthly, 31, 0, 0, time.UTC)
	p := s.PeriodStartingIn(2026, time.March) // Mar 31 -> Apr 30
	prev := p.Prev()
	if got, want := prev.Key(), "2026-02-28/2026-03-31"; got != want {
		t.Fatalf("prev key = %q, want %q", got, want)
	}
	back := prev.Prev()
	if got, want := back.Key(), "2026-01-31/2026-02-28"; got != want {
		t.Fatalf("prev-prev key = %q, want %q", got, want)
	}
}

// Acceptance scenario 7, part 2: leap-day annual renewal is explicit.
func TestLeapDayAnnual(t *testing.T) {
	t.Parallel()
	s := mustSchedule(t, Annual, 29, 0, 0, time.UTC)
	// Anchor month is fixed by the period we start from; start at Feb 2028.
	p := s.PeriodStartingIn(2028, time.February)

	want := []string{
		"2028-02-29/2029-02-28",
		"2029-02-28/2030-02-28",
		"2030-02-28/2031-02-28",
		"2031-02-28/2032-02-29",
		"2032-02-29/2033-02-28",
	}
	for i, w := range want {
		if got := p.Key(); got != w {
			t.Fatalf("year %d key = %q, want %q", i, got, w)
		}
		if p.NominalStart().Day != 29 {
			t.Fatalf("nominal day = %d, want 29", p.NominalStart().Day)
		}
		p = p.Next()
	}
}

// Acceptance scenario 7, part 3: a wall-clock boundary that does not exist is
// shifted forward by the gap.
func TestDSTSpringForwardGap(t *testing.T) {
	t.Parallel()
	ny := mustLoc(t, "America/New_York")
	s := mustSchedule(t, Monthly, 8, 2, 30, ny) // 02:30 on the 8th
	p := s.PeriodStartingIn(2026, time.March)

	// 2026-03-08 02:30 EST does not exist; 02:00 -> 03:00.
	if got, want := p.Start.UTC(), time.Date(2026, 3, 8, 7, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("start = %s, want %s (03:30 EDT)", got.UTC(), want)
	}
	if got, want := p.StartCivil.String(), "2026-03-08 03:30:00"; got != want {
		t.Fatalf("start civil = %q, want %q", got, want)
	}
	// The period is one hour shorter than 31 days.
	if d := p.Duration(); d != 31*24*time.Hour-time.Hour {
		t.Fatalf("duration = %s, want 743h", d)
	}
}

// Acceptance scenario 7, part 4: an ambiguous wall clock resolves to its first
// occurrence.
func TestDSTFallBackAmbiguity(t *testing.T) {
	t.Parallel()
	ny := mustLoc(t, "America/New_York")
	s := mustSchedule(t, Monthly, 1, 1, 30, ny) // 01:30 on the 1st
	p := s.PeriodStartingIn(2026, time.November)

	// 2026-11-01 01:30 occurs twice (EDT then EST); the first occurrence is EDT.
	if got, want := p.Start.UTC(), time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("start = %s, want %s (01:30 EDT)", got.UTC(), want)
	}
	_, offset := p.Start.In(ny).Zone()
	if offset != -4*3600 {
		t.Fatalf("offset = %d, want EDT (-14400)", offset)
	}
	if d := p.Duration(); d != 30*24*time.Hour+time.Hour {
		t.Fatalf("duration = %s, want 721h", d)
	}
}

// Acceptance scenario 7, part 5: the business timezone, not the server's, fixes
// the UTC instant.
func TestBusinessTimezoneTokyo(t *testing.T) {
	t.Parallel()
	tokyo := mustLoc(t, "Asia/Tokyo")
	s := mustSchedule(t, Monthly, 1, 0, 0, tokyo)
	p := s.PeriodStartingIn(2026, time.February)

	if got, want := p.Start.UTC(), time.Date(2026, 1, 31, 15, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("start = %s, want %s", got.UTC(), want)
	}
	if got, want := p.StartCivil.String(), "2026-02-01 00:00:00"; got != want {
		t.Fatalf("start civil = %q, want %q", got, want)
	}
}

func TestQuarterlyAndSemiAnnual(t *testing.T) {
	t.Parallel()
	q := mustSchedule(t, Quarterly, 31, 0, 0, time.UTC)
	p := q.PeriodStartingIn(2026, time.January)
	wantQ := []string{
		"2026-01-31/2026-04-30",
		"2026-04-30/2026-07-31",
		"2026-07-31/2026-10-31",
		"2026-10-31/2027-01-31",
	}
	for i, w := range wantQ {
		if got := p.Key(); got != w {
			t.Fatalf("quarter %d = %q, want %q", i, got, w)
		}
		p = p.Next()
	}

	h := mustSchedule(t, SemiAnnual, 30, 0, 0, time.UTC)
	hp := h.PeriodStartingIn(2026, time.August)
	if got, want := hp.Key(), "2026-08-30/2027-02-28"; got != want {
		t.Fatalf("semiannual = %q, want %q", got, want)
	}
}

func TestPeriodAtAndBoundaries(t *testing.T) {
	t.Parallel()
	s := mustSchedule(t, Monthly, 15, 0, 0, time.UTC)
	start := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)

	// Half-open: the start instant belongs to the period, the end does not.
	if got := s.PeriodAt(start).Key(); got != "2026-03-15/2026-04-15" {
		t.Fatalf("at start = %q", got)
	}
	if got := s.PeriodAt(end.Add(-time.Nanosecond)).Key(); got != "2026-03-15/2026-04-15" {
		t.Fatalf("just before end = %q", got)
	}
	if got := s.PeriodAt(end).Key(); got != "2026-04-15/2026-05-15" {
		t.Fatalf("at end = %q", got)
	}
	if got := s.PeriodAt(start.Add(-time.Nanosecond)).Key(); got != "2026-02-15/2026-03-15" {
		t.Fatalf("just before start = %q", got)
	}

	if got, want := s.NextStartAfter(start), end; !got.Equal(want) {
		t.Fatalf("next start after start = %s, want %s", got, want)
	}
	if got, want := s.NextStartAfter(start.Add(-time.Hour)), start; !got.Equal(want) {
		t.Fatalf("next start before start = %s, want %s", got, want)
	}
}

// A catch-up scan after downtime must enumerate contiguous periods.
func TestPeriodsInCatchUp(t *testing.T) {
	t.Parallel()
	s := mustSchedule(t, Monthly, 31, 0, 0, time.UTC)
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	// Intersecting periods include the one that straddles `from`.
	periods := s.PeriodsIn(from, to)
	if len(periods) != 7 {
		t.Fatalf("got %d periods, want 7: %v", len(periods), periods)
	}
	for i := 1; i < len(periods); i++ {
		if !periods[i].Start.Equal(periods[i-1].End) {
			t.Fatalf("gap between %v and %v", periods[i-1], periods[i])
		}
	}
	if got, want := periods[0].Key(), "2025-12-31/2026-01-31"; got != want {
		t.Fatalf("first intersecting period = %q, want %q", got, want)
	}
	if !periods[len(periods)-1].End.Equal(time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("last period ends %s", periods[len(periods)-1].End)
	}

	// The renewal scanner sees only billing dates that actually came due.
	starts := s.PeriodStartsIn(from, to)
	if len(starts) != 6 {
		t.Fatalf("got %d period starts, want 6: %v", len(starts), starts)
	}
	if got, want := starts[0].Key(), "2026-01-31/2026-02-28"; got != want {
		t.Fatalf("first due period = %q, want %q", got, want)
	}
	if got, want := starts[len(starts)-1].Key(), "2026-06-30/2026-07-31"; got != want {
		t.Fatalf("last due period = %q, want %q", got, want)
	}
	// A range that starts exactly on a boundary must not repeat the prior period.
	exact := s.PeriodStartsIn(time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC), to)
	if len(exact) != 6 || exact[0].Key() != "2026-01-31/2026-02-28" {
		t.Fatalf("boundary-aligned scan = %v", exact)
	}

	if got := s.PeriodsIn(to, from); got != nil {
		t.Fatalf("reversed range should be empty, got %v", got)
	}
	if got := s.PeriodsIn(from, from); got != nil {
		t.Fatalf("empty range should be empty, got %v", got)
	}
}

func TestScheduleValidation(t *testing.T) {
	t.Parallel()
	if _, err := NewAnchor(0, 0, 0); err == nil {
		t.Fatal("day 0 should fail")
	}
	if _, err := NewAnchor(32, 0, 0); err == nil {
		t.Fatal("day 32 should fail")
	}
	if _, err := NewAnchor(1, 24, 0); err == nil {
		t.Fatal("hour 24 should fail")
	}
	if _, err := NewAnchor(1, 0, 60); err == nil {
		t.Fatal("minute 60 should fail")
	}
	a, err := NewAnchor(1, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSchedule(Interval(99), a, time.UTC); err == nil {
		t.Fatal("invalid interval should fail")
	}
	if _, err := NewSchedule(Monthly, a, nil); err == nil {
		t.Fatal("nil location should fail")
	}
	for _, name := range []string{"monthly", "quarterly", "semiannual", "annual"} {
		iv, err := ParseInterval(name)
		if err != nil {
			t.Fatalf("parse %q: %v", name, err)
		}
		if iv.String() != name {
			t.Fatalf("%q round trip = %q", name, iv.String())
		}
	}
	if _, err := ParseInterval("weekly"); err == nil {
		t.Fatal("weekly is out of scope and must not parse")
	}
}

// Property: over long runs and awkward timezones, periods tile the timeline
// exactly, never regress, and keep unique keys.
func TestPeriodTilingProperty(t *testing.T) {
	t.Parallel()
	zones := []string{"UTC", "America/New_York", "Europe/Berlin", "Asia/Tokyo", "Australia/Lord_Howe", "Pacific/Auckland"}
	locs := make([]*time.Location, len(zones))
	for i, z := range zones {
		locs[i] = mustLoc(t, z)
	}
	intervals := []Interval{Monthly, Quarterly, SemiAnnual, Annual}
	rng := rand.New(rand.NewSource(20261021))

	for iter := 0; iter < 400; iter++ {
		loc := locs[rng.Intn(len(locs))]
		iv := intervals[rng.Intn(len(intervals))]
		day := 1 + rng.Intn(31)
		hour := rng.Intn(24)
		minute := rng.Intn(60)
		anchor := Anchor{Day: day, Hour: hour, Minute: minute}
		s, err := NewSchedule(iv, anchor, loc)
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		year := 2024 + rng.Intn(6)
		p := s.PeriodStartingIn(year, time.Month(1+rng.Intn(12)))

		seen := make(map[string]struct{}, 40)
		for i := 0; i < 40; i++ {
			if !p.Start.Before(p.End) {
				t.Fatalf("iter %d step %d: start %s not before end %s", iter, i, p.Start, p.End)
			}
			if _, dup := seen[p.Key()]; dup {
				t.Fatalf("iter %d step %d: duplicate period key %q", iter, i, p.Key())
			}
			seen[p.Key()] = struct{}{}
			// Wall clock must agree with the resolved instant.
			if got := CivilOf(p.Start, loc); got != p.StartCivil {
				t.Fatalf("iter %d step %d: start civil %v != %v", iter, i, got, p.StartCivil)
			}
			if got := CivilOf(p.End, loc); got != p.EndCivil {
				t.Fatalf("iter %d step %d: end civil %v != %v", iter, i, got, p.EndCivil)
			}
			// The nominal anchor day never drifts.
			if p.NominalStart().Day != day {
				t.Fatalf("iter %d step %d: nominal day %d != %d", iter, i, p.NominalStart().Day, day)
			}
			next := p.Next()
			if !next.Start.Equal(p.End) {
				t.Fatalf("iter %d step %d: gap %s -> %s", iter, i, p.End, next.Start)
			}
			// Walking back must return the same period.
			if back := next.Prev(); back.Key() != p.Key() || !back.Start.Equal(p.Start) {
				t.Fatalf("iter %d step %d: Prev not inverse (%q vs %q)", iter, i, back.Key(), p.Key())
			}
			p = next
		}
	}
}

// Property: PeriodAt agrees with linear scanning for arbitrary instants.
func TestPeriodAtProperty(t *testing.T) {
	t.Parallel()
	loc := mustLoc(t, "Europe/Berlin")
	s := mustSchedule(t, Monthly, 31, 3, 0, loc)
	rng := rand.New(rand.NewSource(99))
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	periods := s.PeriodsIn(base, base.AddDate(2, 0, 0))
	for iter := 0; iter < 2000; iter++ {
		t0 := base.Add(time.Duration(rng.Int63n(int64(2 * 365 * 24 * time.Hour))))
		want := -1
		for i, p := range periods {
			if p.Contains(t0) {
				want = i
				break
			}
		}
		if want < 0 {
			t.Fatalf("no period contains %s", t0)
		}
		if got := s.PeriodAt(t0); got.Key() != periods[want].Key() {
			t.Fatalf("PeriodAt(%s) = %q, want %q", t0, got.Key(), periods[want].Key())
		}
	}
}
