package calendar

import (
	"math/rand"
	"testing"
	"time"
)

// The tests in this file cover the two things that must both be true at once:
//
//  1. the calendar intervals are *unchanged* — the golden sequences below are the
//     exact dates the package produced before a day-count period existed, and any
//     edit that moves a month-end, a leap day or a DST boundary breaks them; and
//  2. a day-count period is exact — it lands on the right date across a month end
//     and a leap day, and it keeps its wall-clock time across a DST transition.

// mustDaySchedule builds a day-count schedule whose grid starts at origin.
func mustDaySchedule(t *testing.T, days, anchorDay, hour int, origin time.Time, loc *time.Location) *Schedule {
	t.Helper()
	anchor, err := NewAnchor(anchorDay, hour, 0)
	if err != nil {
		t.Fatalf("anchor: %v", err)
	}
	s, err := NewDaySchedule(days, anchor, origin, loc)
	if err != nil {
		t.Fatalf("day schedule: %v", err)
	}
	return s
}

// goldenPeriods walks a schedule and returns the period keys in order.
func goldenPeriods(s *Schedule, from Civil, n int) []string {
	p := s.PeriodStartingIn(from.Year, from.Month)
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, p.Key())
		p = p.Next()
	}
	return out
}

func assertKeys(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d keys, want %d (%v)", label, len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: key %d = %q, want %q", label, i, got[i], want[i])
		}
	}
}

// TestCalendarIntervalsAreUnchanged is the regression proof for the existing
// intervals. Every string below is what the calendar produced before the day-count
// model was added; the day-count model must not have touched any of it.
func TestCalendarIntervalsAreUnchanged(t *testing.T) {
	t.Parallel()
	utc := time.UTC

	monthly31 := mustSchedule(t, Monthly, 31, 0, 0, utc)
	assertKeys(t, "monthly, anchor 31, from 2024-01", goldenPeriods(monthly31, Civil{Year: 2024, Month: time.January}, 12), []string{
		"2024-01-31/2024-02-29",
		"2024-02-29/2024-03-31",
		"2024-03-31/2024-04-30",
		"2024-04-30/2024-05-31",
		"2024-05-31/2024-06-30",
		"2024-06-30/2024-07-31",
		"2024-07-31/2024-08-31",
		"2024-08-31/2024-09-30",
		"2024-09-30/2024-10-31",
		"2024-10-31/2024-11-30",
		"2024-11-30/2024-12-31",
		"2024-12-31/2025-01-31",
	})

	// The same anchor in a common year, so the February clamp is proven for both
	// lengths of February rather than only the leap one.
	monthly31Common := mustSchedule(t, Monthly, 31, 0, 0, utc)
	assertKeys(t, "monthly, anchor 31, from 2023-01", goldenPeriods(monthly31Common, Civil{Year: 2023, Month: time.January}, 4), []string{
		"2023-01-31/2023-02-28",
		"2023-02-28/2023-03-31",
		"2023-03-31/2023-04-30",
		"2023-04-30/2023-05-31",
	})

	quarterly := mustSchedule(t, Quarterly, 30, 0, 0, utc)
	assertKeys(t, "quarterly, anchor 30", goldenPeriods(quarterly, Civil{Year: 2024, Month: time.January}, 4), []string{
		"2024-01-30/2024-04-30",
		"2024-04-30/2024-07-30",
		"2024-07-30/2024-10-30",
		"2024-10-30/2025-01-30",
	})

	semi := mustSchedule(t, SemiAnnual, 31, 0, 0, utc)
	assertKeys(t, "semiannual, anchor 31", goldenPeriods(semi, Civil{Year: 2024, Month: time.January}, 4), []string{
		"2024-01-31/2024-07-31",
		"2024-07-31/2025-01-31",
		"2025-01-31/2025-07-31",
		"2025-07-31/2026-01-31",
	})

	// A leap-day annual anchor: Feb 29 in a leap year, Feb 28 otherwise, and back to
	// Feb 29 at the next leap year.
	annual := mustSchedule(t, Annual, 29, 0, 0, utc)
	assertKeys(t, "annual, anchor Feb 29", goldenPeriods(annual, Civil{Year: 2024, Month: time.February}, 4), []string{
		"2024-02-29/2025-02-28",
		"2025-02-28/2026-02-28",
		"2026-02-28/2027-02-28",
		"2027-02-28/2028-02-29",
	})
}

// TestCalendarInstantsAreUnchanged pins the resolved instants, not just the dates,
// for a DST timezone: the boundary keeps its wall-clock time and therefore its
// offset changes with the season.
func TestCalendarInstantsAreUnchanged(t *testing.T) {
	t.Parallel()
	loc := mustLoc(t, "Europe/Berlin")
	s := mustSchedule(t, Monthly, 15, 2, 30, loc)
	p := s.PeriodStartingIn(2024, time.March)

	// 02:30 CET is 01:30Z; 02:30 CEST is 00:30Z. The period is 31 days minus the
	// hour the spring transition removes.
	if got, want := p.Start.UTC(), time.Date(2024, 3, 15, 1, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("start = %s, want %s", got, want)
	}
	if got, want := p.End.UTC(), time.Date(2024, 4, 15, 0, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("end = %s, want %s", got, want)
	}
	if got, want := p.Duration(), 31*24*time.Hour-time.Hour; got != want {
		t.Fatalf("duration = %s, want %s", got, want)
	}
}

// TestDayPeriodAcrossMonthEnd: 45 days from Jan 20 crosses the end of January and
// must land on Mar 5, not on "Jan 20 plus one and a half months" and not on a
// 30.44-day month.
func TestDayPeriodAcrossMonthEnd(t *testing.T) {
	t.Parallel()
	loc := time.UTC
	origin := time.Date(2024, 1, 20, 0, 0, 0, 0, time.UTC)
	s := mustDaySchedule(t, 45, 20, 0, origin, loc)

	p := s.PeriodStartingAt(origin)
	if got, want := p.Key(), "2024-01-20/2024-03-05"; got != want {
		t.Fatalf("key = %q, want %q", got, want)
	}
	if got, want := p.Duration(), 45*24*time.Hour; got != want {
		t.Fatalf("duration = %s, want %s", got, want)
	}

	next := p.Next()
	if got, want := next.Key(), "2024-03-05/2024-04-19"; got != want {
		t.Fatalf("next key = %q, want %q", got, want)
	}
	if !next.Start.Equal(p.End) {
		t.Fatalf("next start %s != previous end %s", next.Start, p.End)
	}
	if back := next.Prev(); back.Key() != p.Key() {
		t.Fatalf("Prev(Next(p)) = %q, want %q", back.Key(), p.Key())
	}
}

// TestDayPeriodAcrossLeapDay: a 45-day period that spans February 29 lands one day
// later in a leap year than it does in a common year. That difference is the proof
// the arithmetic counts real days rather than assuming a month length.
func TestDayPeriodAcrossLeapDay(t *testing.T) {
	t.Parallel()
	leapOrigin := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	leap := mustDaySchedule(t, 45, 1, 0, leapOrigin, time.UTC)
	if got, want := leap.PeriodStartingAt(leapOrigin).Key(), "2024-02-01/2024-03-17"; got != want {
		t.Fatalf("leap-year key = %q, want %q", got, want)
	}

	commonOrigin := time.Date(2023, 2, 1, 0, 0, 0, 0, time.UTC)
	common := mustDaySchedule(t, 45, 1, 0, commonOrigin, time.UTC)
	if got, want := common.PeriodStartingAt(commonOrigin).Key(), "2023-02-01/2023-03-18"; got != want {
		t.Fatalf("common-year key = %q, want %q", got, want)
	}

	// A single-day period on Feb 29 is exactly one day, and the next one is Mar 1.
	daily := mustDaySchedule(t, 1, 29, 0, leapOrigin, time.UTC)
	p := daily.PeriodAt(time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC))
	if got, want := p.Key(), "2024-02-29/2024-03-01"; got != want {
		t.Fatalf("leap day key = %q, want %q", got, want)
	}
}

// TestDayPeriodKeepsWallClockAcrossDST: a day-count period is counted in civil days,
// so the boundary stays at the anchor's wall-clock time and the elapsed duration
// changes by the hour the transition adds or removes. Counting 24-hour multiples
// instead would move every later renewal by an hour.
func TestDayPeriodKeepsWallClockAcrossDST(t *testing.T) {
	t.Parallel()
	loc := mustLoc(t, "Europe/Berlin")

	// Spring forward in Berlin is 2024-03-31 02:00 -> 03:00, so that civil day is 23
	// hours long.
	daily := mustDaySchedule(t, 1, 31, 0, time.Date(2024, 3, 30, 0, 0, 0, 0, loc), loc)
	before := daily.PeriodAt(time.Date(2024, 3, 30, 12, 0, 0, 0, loc))
	short := before.Next()
	if got, want := short.StartCivil, (Civil{Year: 2024, Month: time.March, Day: 31}); got != want {
		t.Fatalf("start civil = %v, want %v", got, want)
	}
	if got, want := short.EndCivil, (Civil{Year: 2024, Month: time.April, Day: 1}); got != want {
		t.Fatalf("end civil = %v, want %v", got, want)
	}
	if got, want := short.Duration(), 23*time.Hour; got != want {
		t.Fatalf("duration across spring forward = %s, want %s", got, want)
	}

	// A 45-day period that spans the transition is 45 days minus one hour, and still
	// starts and ends at midnight wall-clock.
	fortyFive := mustDaySchedule(t, 45, 1, 0, time.Date(2024, 3, 1, 0, 0, 0, 0, loc), loc)
	p := fortyFive.PeriodAt(time.Date(2024, 3, 2, 0, 0, 0, 0, loc))
	if got, want := p.Key(), "2024-03-01/2024-04-15"; got != want {
		t.Fatalf("key = %q, want %q", got, want)
	}
	if got, want := p.Duration(), 45*24*time.Hour-time.Hour; got != want {
		t.Fatalf("duration = %s, want %s", got, want)
	}
	if p.StartCivil.Hour != 0 || p.EndCivil.Hour != 0 {
		t.Fatalf("wall clock drifted: start %v end %v", p.StartCivil, p.EndCivil)
	}
}

// TestDayPeriodIsStableFromItsOrigin is the property the renewal engine depends on:
// the persisted boundary is the grid's origin, so the same boundary is reproduced on
// every run and a period in the middle of the grid resolves to the same dates.
func TestDayPeriodIsStableFromItsOrigin(t *testing.T) {
	t.Parallel()
	loc := mustLoc(t, "Europe/Berlin")
	origin := time.Date(2024, 1, 20, 0, 0, 0, 0, loc)
	s := mustDaySchedule(t, 45, 20, 0, origin, loc)

	first := s.PeriodStartingAt(origin)
	if !first.Start.Equal(origin) {
		t.Fatalf("PeriodStartingAt(origin).Start = %s, want %s", first.Start, origin)
	}

	// Every grid boundary reproduces itself exactly, and a run started from the
	// second boundary produces the same grid as a run started from the first.
	second := first.Next()
	fromSecond, err := NewDaySchedule(45, Anchor{Day: 20}, second.Start, loc)
	if err != nil {
		t.Fatalf("second grid: %v", err)
	}
	if got := fromSecond.PeriodStartingAt(second.Start); got.Key() != second.Key() {
		t.Fatalf("second grid key = %q, want %q", got.Key(), second.Key())
	}
	if got := fromSecond.PeriodAt(second.Start.Add(24 * time.Hour)); got.Key() != second.Key() {
		t.Fatalf("PeriodAt inside the second period = %q, want %q", got.Key(), second.Key())
	}
	// And it advances to the same next boundary as the original grid.
	if got, want := fromSecond.PeriodStartingAt(second.Start).Next().Key(), first.Next().Next().Key(); got != want {
		t.Fatalf("third key = %q, want %q", got, want)
	}
}

// TestFloatingDayScheduleStartsOnTheAnchorDay: a catalog price has no stored
// boundary, so its first period starts on the anchor day of the month it is bought
// in — the same rule a calendar interval already uses. The clamp applies, so an
// anchor on the 31st still produces a period in February.
func TestFloatingDayScheduleStartsOnTheAnchorDay(t *testing.T) {
	t.Parallel()
	loc := time.UTC

	// Anchor day 1, ten-day periods, bought on Jan 20: the period containing the
	// purchase is Jan 11 - Jan 21.
	tenDay, err := newFloatingDaySchedule(10, Anchor{Day: 1}, loc)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if got, want := tenDay.PeriodAt(time.Date(2024, 1, 20, 12, 0, 0, 0, time.UTC)).Key(),
		"2024-01-11/2024-01-21"; got != want {
		t.Fatalf("ten-day key = %q, want %q", got, want)
	}

	// Anchor day 31 in February clamps to Feb 29 (2024), which is *after* the
	// purchase instant, so the period containing it is the one that ends there.
	fortyFive, err := newFloatingDaySchedule(45, Anchor{Day: 31}, loc)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	p := fortyFive.PeriodAt(time.Date(2024, 2, 10, 0, 0, 0, 0, time.UTC))
	if got, want := p.Key(), "2024-01-15/2024-02-29"; got != want {
		t.Fatalf("clamped-origin key = %q, want %q", got, want)
	}
	if !p.Contains(time.Date(2024, 2, 10, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("the period does not contain the instant it was derived from")
	}
}

// TestFirstPeriodMatchesTheCalendarModel ties the shared purchase-path helper to the
// behaviour it replaced: for a calendar interval it is the period containing the
// purchase instant, unchanged.
func TestFirstPeriodMatchesTheCalendarModel(t *testing.T) {
	t.Parallel()
	now := time.Date(2024, 1, 20, 13, 45, 0, 0, time.UTC)

	for _, tc := range []struct {
		name string
		iv   Interval
	}{
		{"monthly", Monthly},
		{"quarterly", Quarterly},
		{"semiannual", SemiAnnual},
		{"annual", Annual},
	} {
		for _, day := range []int{1, 15, 29, 31} {
			got, err := FirstPeriod(tc.name, day, "Europe/Berlin", now)
			if err != nil {
				t.Fatalf("%s anchor %d: %v", tc.name, day, err)
			}
			loc := mustLoc(t, "Europe/Berlin")
			anchor, err := NewAnchor(day, 0, 0)
			if err != nil {
				t.Fatalf("anchor: %v", err)
			}
			s, err := NewSchedule(tc.iv, anchor, loc)
			if err != nil {
				t.Fatalf("schedule: %v", err)
			}
			want := s.PeriodAt(now)
			if got.Key() != want.Key() || !got.Start.Equal(want.Start) || !got.End.Equal(want.End) {
				t.Fatalf("%s anchor %d: FirstPeriod = %q, want %q", tc.name, day, got.Key(), want.Key())
			}
			if got.IsDayCount() {
				t.Fatalf("%s anchor %d: reported as a day-count period", tc.name, day)
			}
		}
	}

	// A day-count interval through the same helper is a day-count period.
	got, err := FirstPeriod("every_45_days", 20, "UTC", now)
	if err != nil {
		t.Fatalf("day period: %v", err)
	}
	if !got.IsDayCount() || got.Days() != 45 {
		t.Fatalf("FirstPeriod(every_45_days) = %+v, want a 45-day period", got)
	}
	if got.Key() != "2024-01-20/2024-03-05" {
		t.Fatalf("FirstPeriod(every_45_days) = %q, want 2024-01-20/2024-03-05", got.Key())
	}

	// An unknown interval is still refused.
	if _, err := FirstPeriod("weekly", 1, "UTC", now); err == nil {
		t.Fatal("weekly must not parse")
	}
}

// TestParseDayInterval covers the stored spelling: one period, one string.
func TestParseDayInterval(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		days int
		ok   bool
	}{
		{"every_1_days", 1, true},
		{"every_45_days", 45, true},
		{"every_366_days", 366, true},
		{"every_0_days", 0, false},
		{"every_367_days", 0, false},
		{"every_045_days", 0, false},
		{"every_-5_days", 0, false},
		{"every_45_day", 0, false},
		{"every_days", 0, false},
		{"monthly", 0, false},
		{"45 days", 0, false},
	} {
		days, ok := ParseDayInterval(tc.in)
		if ok != tc.ok || days != tc.days {
			t.Fatalf("ParseDayInterval(%q) = (%d, %v), want (%d, %v)", tc.in, days, ok, tc.days, tc.ok)
		}
	}
	if got, want := DayInterval(45), "every_45_days"; got != want {
		t.Fatalf("DayInterval(45) = %q, want %q", got, want)
	}
	if days, ok := ParseDayInterval(DayInterval(45)); !ok || days != 45 {
		t.Fatalf("round trip of DayInterval(45) = (%d, %v)", days, ok)
	}
}

// TestScheduleForDispatch: the dispatcher picks the model from the stored name, and
// a calendar name never becomes a day-count schedule.
func TestScheduleForDispatch(t *testing.T) {
	t.Parallel()
	loc := time.UTC
	anchor, err := NewAnchor(15, 0, 0)
	if err != nil {
		t.Fatalf("anchor: %v", err)
	}
	origin := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)

	for _, name := range []string{"monthly", "quarterly", "semiannual", "annual"} {
		s, err := ScheduleFor(name, anchor, origin, loc)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if s.IsDayCount() || s.Interval().String() != name {
			t.Fatalf("%s dispatched to %v", name, s)
		}
	}
	s, err := ScheduleFor("every_45_days", anchor, origin, loc)
	if err != nil {
		t.Fatalf("every_45_days: %v", err)
	}
	if !s.IsDayCount() || s.Days() != 45 {
		t.Fatalf("every_45_days dispatched to %v", s)
	}
	if _, err := ScheduleFor("every_0_days", anchor, origin, loc); err == nil {
		t.Fatal("every_0_days must be refused")
	}
	if _, err := ScheduleFor("weekly", anchor, origin, loc); err == nil {
		t.Fatal("weekly must be refused")
	}
}

// TestDayPeriodTilingProperty is the day-count counterpart of the calendar tiling
// property: over long runs, arbitrary day counts and awkward timezones, periods tile
// the timeline exactly, keep unique keys and walk back to themselves.
func TestDayPeriodTilingProperty(t *testing.T) {
	t.Parallel()
	zones := []string{"UTC", "America/New_York", "Europe/Berlin", "Asia/Tokyo", "Australia/Lord_Howe", "Pacific/Auckland"}
	locs := make([]*time.Location, len(zones))
	for i, z := range zones {
		locs[i] = mustLoc(t, z)
	}
	rng := rand.New(rand.NewSource(20261022))

	for iter := 0; iter < 300; iter++ {
		loc := locs[rng.Intn(len(locs))]
		days := 1 + rng.Intn(MaxDayInterval)
		hour := rng.Intn(24)
		origin := time.Date(2024+rng.Intn(4), time.Month(1+rng.Intn(12)), 1+rng.Intn(28), hour, 0, 0, 0, loc)
		s := mustDaySchedule(t, days, 1+rng.Intn(31), hour, origin, loc)

		p := s.PeriodStartingAt(origin)
		seen := make(map[string]struct{}, 20)
		for i := 0; i < 20; i++ {
			if !p.Start.Before(p.End) {
				t.Fatalf("iter %d step %d: start %s not before end %s", iter, i, p.Start, p.End)
			}
			if _, dup := seen[p.Key()]; dup {
				t.Fatalf("iter %d step %d: duplicate key %q", iter, i, p.Key())
			}
			seen[p.Key()] = struct{}{}
			if p.Days() != days || !p.IsDayCount() {
				t.Fatalf("iter %d step %d: period reports %d days", iter, i, p.Days())
			}
			// The wall clock agrees with the instant, and the period is exactly
			// `days` civil days long: an elapsed duration may differ by a DST hour,
			// but never by a day.
			if got := CivilOf(p.Start, loc); got != p.StartCivil {
				t.Fatalf("iter %d step %d: start civil %v != %v", iter, i, got, p.StartCivil)
			}
			if got := CivilOf(p.End, loc); got != p.EndCivil {
				t.Fatalf("iter %d step %d: end civil %v != %v", iter, i, got, p.EndCivil)
			}
			if got := civilDaysBetween(p.StartCivil, p.EndCivil); got != days {
				t.Fatalf("iter %d step %d: %d civil days, want %d", iter, i, got, days)
			}
			if d := p.Duration(); d < time.Duration(days)*24*time.Hour-2*time.Hour ||
				d > time.Duration(days)*24*time.Hour+2*time.Hour {
				t.Fatalf("iter %d step %d: duration %s for %d days", iter, i, d, days)
			}
			// PeriodAt inside the period agrees with the walk.
			mid := p.Start.Add(p.Duration() / 2)
			if got := s.PeriodAt(mid); got.Key() != p.Key() {
				t.Fatalf("iter %d step %d: PeriodAt(%s) = %q, want %q", iter, i, mid, got.Key(), p.Key())
			}
			next := p.Next()
			if !next.Start.Equal(p.End) {
				t.Fatalf("iter %d step %d: gap %s -> %s", iter, i, p.End, next.Start)
			}
			if back := next.Prev(); back.Key() != p.Key() || !back.Start.Equal(p.Start) {
				t.Fatalf("iter %d step %d: Prev not inverse (%q vs %q)", iter, i, back.Key(), p.Key())
			}
			p = next
		}
	}
}
