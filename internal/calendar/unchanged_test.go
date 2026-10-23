package calendar

// The proof that adding a second period model did not disturb the first one.
//
// A golden list of dates proves only that the dates I happened to write down are still
// produced. This file instead freezes the *algorithm* the package used before day-count
// periods existed — the month-only period arithmetic, copied verbatim into
// legacyPeriodStartingIn / legacyShift / legacyBuild below — and compares it against the
// live Schedule over a randomised space of intervals, anchors, times of day, timezones and
// years. If the two ever disagree on a single instant, one of them is the bug, and the test
// says which case and which boundary.
//
// The frozen copy is deliberately *not* refactored to share code with the live one. Sharing
// would make the comparison tautological: the point is two independent implementations that
// must agree.

import (
	"math/rand"
	"testing"
	"time"
)

// legacyPeriod is a period as the package computed it before day-count periods: an
// interval in months, an anchor day-of-month, and a nominal start that is never clamped.
type legacyPeriod struct {
	interval     Interval
	loc          *time.Location
	anchor       Anchor
	nominalStart Civil
	Start        time.Time
	End          time.Time
	StartCivil   Civil
	EndCivil     Civil
}

// legacyBuild is the pre-change `Period.build`: resolve a nominal (year, month) from the
// anchor day and the interval's month count.
//
// It calls the frozen helpers below — not the live `resolve`, `clamp`, `addMonths`,
// `floorDiv` or `DaysIn`. That is deliberate and it is the whole value of this file: an
// earlier draft shared those helpers, and deleting the month-end clamp from the live code
// then left this test passing, because the same deletion applied to both sides. The frozen
// model has to be able to disagree with the live one, including about the leap-year rule.
func legacyBuild(interval Interval, anchor Anchor, loc *time.Location, year int, month time.Month) legacyPeriod {
	nominal := func(y int, m time.Month) Civil {
		return Civil{
			Year: y, Month: m, Day: anchor.Day,
			Hour: anchor.Hour, Minute: anchor.Minute, Second: anchor.Second,
		}
	}
	startNom := nominal(year, month)
	endYear, endMonth := legacyAddMonths(year, month, interval.Months())
	endNom := nominal(endYear, endMonth)

	start := legacyResolve(loc, legacyClamp(startNom))
	end := legacyResolve(loc, legacyClamp(endNom))
	return legacyPeriod{
		interval:     interval,
		loc:          loc,
		anchor:       anchor,
		nominalStart: startNom,
		Start:        start,
		End:          end,
		StartCivil:   legacyCivilOf(start, loc),
		EndCivil:     legacyCivilOf(end, loc),
	}
}

// legacyShift is the pre-change `Period.shift`: month arithmetic on the nominal start.
func legacyShift(p legacyPeriod, n int) legacyPeriod {
	year, month := legacyAddMonths(p.nominalStart.Year, p.nominalStart.Month, p.interval.Months()*n)
	return legacyBuild(p.interval, p.anchor, p.loc, year, month)
}

// legacyPeriodStartingIn is the pre-change `Schedule.PeriodStartingIn`.
func legacyPeriodStartingIn(interval Interval, anchor Anchor, loc *time.Location, year int, month time.Month) legacyPeriod {
	return legacyBuild(interval, anchor, loc, year, month)
}

// legacyPeriodAt is the pre-change `Schedule.PeriodAt`: seed from t's month, then walk.
func legacyPeriodAt(interval Interval, anchor Anchor, loc *time.Location, t time.Time) legacyPeriod {
	c := legacyCivilOf(t, loc)
	p := legacyPeriodStartingIn(interval, anchor, loc, c.Year, c.Month)
	for t.Before(p.Start) {
		p = legacyShift(p, -1)
	}
	for !t.Before(p.End) {
		p = legacyShift(p, 1)
	}
	return p
}

// legacyKey is the pre-change `Period.Key`.
func legacyKey(p legacyPeriod) string {
	return p.StartCivil.DateString() + "/" + p.EndCivil.DateString()
}

// ------------------------------------------------------- the frozen helpers --

// legacyCivilOf is the pre-change `CivilOf`.
func legacyCivilOf(t time.Time, loc *time.Location) Civil {
	l := t.In(loc)
	return Civil{
		Year: l.Year(), Month: l.Month(), Day: l.Day(),
		Hour: l.Hour(), Minute: l.Minute(), Second: l.Second(),
	}
}

// legacyDaysIn is the pre-change `DaysIn`: the length of a month, leap year included.
func legacyDaysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// legacyClamp is the pre-change month-end rule: a nominal day above the length of the month
// is pulled back to the last real day for resolution only.
func legacyClamp(c Civil) Civil {
	if d := legacyDaysIn(c.Year, c.Month); c.Day > d {
		c.Day = d
	}
	return c
}

// legacyResolve is the pre-change DST resolution: the documented gap and ambiguity policy.
func legacyResolve(loc *time.Location, c Civil) time.Time {
	probe := time.Date(c.Year, c.Month, c.Day, c.Hour, c.Minute, c.Second, 0, time.UTC)

	var best time.Time
	found := false
	seen := make(map[int]struct{}, 3)
	for _, ref := range []time.Time{probe.Add(-36 * time.Hour), probe, probe.Add(36 * time.Hour)} {
		_, offset := ref.In(loc).Zone()
		if _, dup := seen[offset]; dup {
			continue
		}
		seen[offset] = struct{}{}
		cand := probe.Add(-time.Duration(offset) * time.Second)
		if legacyCivilOf(cand, loc) == c {
			if !found || cand.Before(best) {
				best = cand
				found = true
			}
		}
	}
	if found {
		return best
	}
	_, before := probe.Add(-36 * time.Hour).In(loc).Zone()
	return probe.Add(-time.Duration(before) * time.Second)
}

// legacyAddMonths is the pre-change month arithmetic, floor division included.
func legacyAddMonths(year int, month time.Month, months int) (int, time.Month) {
	total := year*12 + int(month) - 1 + months
	y := legacyFloorDiv(total, 12)
	m := total - y*12 + 1
	return y, time.Month(m)
}

func legacyFloorDiv(a, b int) int {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// calendarIntervals are the four intervals that existed before day-count periods. `one_time`
// and `free` are absent because they were never calendar intervals: they have no period at
// all, and TestNonRecurringIntervalsHaveNoPeriod below proves that is still true.
var calendarIntervals = []Interval{Monthly, Quarterly, SemiAnnual, Annual}

// TestCalendarModelIsByteIdenticalToThePreChangeAlgorithm is the differential proof.
//
// Every field a caller can observe is compared — the two instants, the two wall clocks, the
// nominal start and the period key — for 4000 randomly chosen schedules over ten years,
// including zones with a 30-minute DST shift (Lord Howe), a southern-hemisphere transition
// (Auckland, Santiago) and a zone whose transition happens at midnight (Cairo), because
// those are where a "helpful" refactor of the resolve/clamp path would show up.
func TestCalendarModelIsByteIdenticalToThePreChangeAlgorithm(t *testing.T) {
	t.Parallel()
	zones := []string{
		"UTC", "Europe/Berlin", "America/New_York", "Asia/Tokyo", "Asia/Kolkata",
		"Australia/Lord_Howe", "Pacific/Auckland", "America/Santiago", "Africa/Cairo",
	}
	locs := make([]*time.Location, len(zones))
	for i, z := range zones {
		locs[i] = mustLoc(t, z)
	}
	rng := rand.New(rand.NewSource(20261023))

	for iter := 0; iter < 4000; iter++ {
		loc := locs[rng.Intn(len(locs))]
		iv := calendarIntervals[rng.Intn(len(calendarIntervals))]
		anchor := Anchor{
			Day:    1 + rng.Intn(31),
			Hour:   rng.Intn(24),
			Minute: rng.Intn(60),
			Second: rng.Intn(60),
		}
		year := 2020 + rng.Intn(11)
		month := time.Month(1 + rng.Intn(12))

		s, err := NewSchedule(iv, anchor, loc)
		if err != nil {
			t.Fatalf("iter %d: schedule: %v", iter, err)
		}
		live := s.PeriodStartingIn(year, month)
		frozen := legacyPeriodStartingIn(iv, anchor, loc, year, month)

		comparePeriods(t, iter, "PeriodStartingIn", live, frozen, iv, anchor, loc, year, month)

		// Walk five periods in both directions and compare every step.
		liveP, frozenP := live, frozen
		for step := 0; step < 5; step++ {
			liveP, frozenP = liveP.Next(), legacyShift(frozenP, 1)
			comparePeriods(t, iter, "Next", liveP, frozenP, iv, anchor, loc, year, month)
		}
		liveP, frozenP = live, frozen
		for step := 0; step < 5; step++ {
			liveP, frozenP = liveP.Prev(), legacyShift(frozenP, -1)
			comparePeriods(t, iter, "Prev", liveP, frozenP, iv, anchor, loc, year, month)
		}

		// And the containing period for an arbitrary instant in that window.
		probe := time.Date(year, month, 1+rng.Intn(28), rng.Intn(24), rng.Intn(60), 0, 0, time.UTC).
			Add(time.Duration(rng.Intn(400)) * 24 * time.Hour)
		liveAt := s.PeriodAt(probe)
		frozenAt := legacyPeriodAt(iv, anchor, loc, probe)
		if liveAt.Key() != legacyKey(frozenAt) ||
			!liveAt.Start.Equal(frozenAt.Start) || !liveAt.End.Equal(frozenAt.End) {
			t.Fatalf("iter %d: PeriodAt(%s) with %s anchor %v in %s = %q [%s,%s), "+
				"pre-change algorithm = %q [%s,%s)",
				iter, probe, iv, anchor, loc, liveAt.Key(), liveAt.Start, liveAt.End,
				legacyKey(frozenAt), frozenAt.Start, frozenAt.End)
		}
	}
}

// comparePeriods fails with the full case on any observable difference.
func comparePeriods(t *testing.T, iter int, step string, live Period, frozen legacyPeriod,
	iv Interval, anchor Anchor, loc *time.Location, year int, month time.Month) {
	t.Helper()
	if !live.Start.Equal(frozen.Start) {
		t.Fatalf("iter %d %s: %s anchor %v in %s from %04d-%02d: start = %s, pre-change = %s",
			iter, step, iv, anchor, loc, year, int(month), live.Start, frozen.Start)
	}
	if !live.End.Equal(frozen.End) {
		t.Fatalf("iter %d %s: %s anchor %v in %s from %04d-%02d: end = %s, pre-change = %s",
			iter, step, iv, anchor, loc, year, int(month), live.End, frozen.End)
	}
	if live.StartCivil != frozen.StartCivil {
		t.Fatalf("iter %d %s: start civil = %v, pre-change = %v", iter, step, live.StartCivil, frozen.StartCivil)
	}
	if live.EndCivil != frozen.EndCivil {
		t.Fatalf("iter %d %s: end civil = %v, pre-change = %v", iter, step, live.EndCivil, frozen.EndCivil)
	}
	if live.NominalStart() != frozen.nominalStart {
		t.Fatalf("iter %d %s: nominal start = %v, pre-change = %v",
			iter, step, live.NominalStart(), frozen.nominalStart)
	}
	if live.Key() != legacyKey(frozen) {
		t.Fatalf("iter %d %s: key = %q, pre-change = %q", iter, step, live.Key(), legacyKey(frozen))
	}
	// The fields the two models must still disagree about: a calendar period knows its
	// interval and has no day count.
	if live.Interval() != iv {
		t.Fatalf("iter %d %s: interval = %s, want %s", iter, step, live.Interval(), iv)
	}
	if live.IsDayCount() || live.Days() != 0 {
		t.Fatalf("iter %d %s: a calendar period reported a day count (%d)", iter, step, live.Days())
	}
}

// TestNonRecurringIntervalsHaveNoPeriod pins the other half of "unchanged": `one_time` and
// `free` never had a period and still have none. They are not a fifth and sixth model — they
// are the absence of a schedule, which is why a one-time purchase creates no subscription.
func TestNonRecurringIntervalsHaveNoPeriod(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, name := range []string{"one_time", "free"} {
		if _, err := FirstPeriod(name, 1, "UTC", now); err == nil {
			t.Fatalf("FirstPeriod(%q) produced a period; a non-recurring price has no schedule", name)
		}
		if _, err := ScheduleFor(name, Anchor{Day: 1}, time.Time{}, time.UTC); err == nil {
			t.Fatalf("ScheduleFor(%q) produced a schedule; a non-recurring price has none", name)
		}
		if _, err := ParseInterval(name); err == nil {
			t.Fatalf("ParseInterval(%q) accepted a non-recurring kind as a calendar interval", name)
		}
		if _, ok := ParseDayInterval(name); ok {
			t.Fatalf("ParseDayInterval(%q) accepted a non-recurring kind as a day count", name)
		}
	}
}

// TestLeapDay2028ExactRenewalDates is the leap-day case Jack asked for by date.
//
// A day-count period that spans 2028-02-29 is one day longer than the same period in a
// common year, and the exact boundary is asserted rather than "about a month later". The
// calendar intervals anchored on 29 and 31 are asserted on the same dates, because a leap
// day is where a month model and a day model could diverge.
func TestLeapDay2028ExactRenewalDates(t *testing.T) {
	t.Parallel()
	utc := time.UTC

	// A 45-day period starting 2028-01-20 crosses the leap day: 2028-01-20 + 45 days =
	// 2028-03-05. In 2027 the same start would end 2027-03-06.
	leapOrigin := time.Date(2028, 1, 20, 0, 0, 0, 0, utc)
	leap := mustDaySchedule(t, 45, 20, 0, leapOrigin, utc)
	if got, want := leap.PeriodStartingAt(leapOrigin).Key(), "2028-01-20/2028-03-05"; got != want {
		t.Fatalf("2028 45-day period = %q, want %q", got, want)
	}
	commonOrigin := time.Date(2027, 1, 20, 0, 0, 0, 0, utc)
	common := mustDaySchedule(t, 45, 20, 0, commonOrigin, utc)
	if got, want := common.PeriodStartingAt(commonOrigin).Key(), "2027-01-20/2027-03-06"; got != want {
		t.Fatalf("2027 45-day period = %q, want %q", got, want)
	}

	// A 1-day period whose start *is* the leap day, and the one after it.
	daily := mustDaySchedule(t, 1, 29, 0, time.Date(2028, 2, 1, 0, 0, 0, 0, utc), utc)
	leapDay := daily.PeriodAt(time.Date(2028, 2, 29, 12, 0, 0, 0, utc))
	if got, want := leapDay.Key(), "2028-02-29/2028-03-01"; got != want {
		t.Fatalf("leap day period = %q, want %q", got, want)
	}

	// An N-day period that starts exactly on the leap day.
	fromLeapDay := mustDaySchedule(t, 45, 29, 0, leapDay.Start, utc)
	if got, want := fromLeapDay.PeriodStartingAt(leapDay.Start).Key(), "2028-02-29/2028-04-14"; got != want {
		t.Fatalf("45 days from the leap day = %q, want %q", got, want)
	}

	// The calendar model on the same dates: monthly anchored on the 31st across the leap
	// February, and annual anchored on Feb 29.
	monthly := mustSchedule(t, Monthly, 31, 0, 0, utc)
	assertKeys(t, "monthly anchor 31 around 2028-02", goldenPeriods(monthly, Civil{Year: 2028, Month: time.January}, 3), []string{
		"2028-01-31/2028-02-29",
		"2028-02-29/2028-03-31",
		"2028-03-31/2028-04-30",
	})
	annual := mustSchedule(t, Annual, 29, 0, 0, utc)
	assertKeys(t, "annual anchor Feb 29 around 2028", goldenPeriods(annual, Civil{Year: 2028, Month: time.February}, 2), []string{
		"2028-02-29/2029-02-28",
		"2029-02-28/2030-02-28",
	})
}

// TestThirtyFirstToThirtiethBoundary is the month-end case: a period that starts on the 31st
// and ends in a 30-day month must land on the 30th, not on the 31st and not on the 1st.
func TestThirtyFirstToThirtiethBoundary(t *testing.T) {
	t.Parallel()
	utc := time.UTC

	// Calendar: an anchor on the 31st through April, June, September and November.
	monthly := mustSchedule(t, Monthly, 31, 0, 0, utc)
	assertKeys(t, "monthly anchor 31 across 30-day months", goldenPeriods(monthly, Civil{Year: 2026, Month: time.March}, 4), []string{
		"2026-03-31/2026-04-30",
		"2026-04-30/2026-05-31",
		"2026-05-31/2026-06-30",
		"2026-06-30/2026-07-31",
	})

	// Day count: 30 days from the 31st of January. February 2026 has 28 days, so the
	// boundary is 2026-03-02 — the 31st plus 30 real days, not "one month later".
	origin := time.Date(2026, 1, 31, 0, 0, 0, 0, utc)
	thirty := mustDaySchedule(t, 30, 31, 0, origin, utc)
	p := thirty.PeriodStartingAt(origin)
	if got, want := p.Key(), "2026-01-31/2026-03-02"; got != want {
		t.Fatalf("30 days from Jan 31 = %q, want %q", got, want)
	}
	if got, want := p.Duration(), 30*24*time.Hour; got != want {
		t.Fatalf("duration = %s, want %s", got, want)
	}
	// And 31 days from Jan 31 lands on 2026-03-03, which a "one month" model could not
	// distinguish from the 30-day one.
	thirtyOne := mustDaySchedule(t, 31, 31, 0, origin, utc)
	if got, want := thirtyOne.PeriodStartingAt(origin).Key(), "2026-01-31/2026-03-03"; got != want {
		t.Fatalf("31 days from Jan 31 = %q, want %q", got, want)
	}

	// A 30-day period starting on the 31st of a 31-day month, ending in a 30-day month,
	// is exactly one day shorter than the month model's period over the same span.
	if got, want := p.EndCivil.Day, 2; got != want {
		t.Fatalf("end day = %d, want %d (2026-03-02)", got, want)
	}
}
