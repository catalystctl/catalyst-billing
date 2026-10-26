package calendar

import (
	"testing"
	"time"
)

// The explicit week and month units extend the period vocabulary without changing
// either existing model: a week is the day-count model with a length of 7*N days, and
// a month is the calendar model with a month count of N. These tests pin the parts a
// wrong implementation would get wrong: the parse bounds, the dispatch, the month-end
// clamp for an N-month period, and the fact that a week never drifts into a month.

func mustAnchor(t *testing.T, day, hour int) Anchor {
	t.Helper()
	a, err := NewAnchor(day, hour, 0)
	if err != nil {
		t.Fatalf("anchor: %v", err)
	}
	return a
}

func TestParseUnitIntervals(t *testing.T) {
	t.Parallel()
	weeks := []struct {
		in string
		n  int
		ok bool
	}{
		{"every_1_weeks", 1, true},
		{"every_2_weeks", 2, true},
		{"every_52_weeks", 52, true},
		{"every_0_weeks", 0, false},
		{"every_53_weeks", 0, false},
		{"every_02_weeks", 0, false},
		{"every_2_week", 0, false},
		{"every_2_days", 0, false},
		{"every_2_months", 0, false},
	}
	for _, tc := range weeks {
		if n, ok := ParseWeekInterval(tc.in); ok != tc.ok || n != tc.n {
			t.Fatalf("ParseWeekInterval(%q) = (%d, %v), want (%d, %v)", tc.in, n, ok, tc.n, tc.ok)
		}
	}
	months := []struct {
		in string
		n  int
		ok bool
	}{
		{"every_1_months", 1, true},
		{"every_3_months", 3, true},
		{"every_12_months", 12, true},
		{"every_0_months", 0, false},
		{"every_13_months", 0, false},
		{"every_03_months", 0, false},
		{"every_3_month", 0, false},
		{"every_3_weeks", 0, false},
	}
	for _, tc := range months {
		if n, ok := ParseMonthInterval(tc.in); ok != tc.ok || n != tc.n {
			t.Fatalf("ParseMonthInterval(%q) = (%d, %v), want (%d, %v)", tc.in, n, ok, tc.n, tc.ok)
		}
	}
	if got, want := WeekInterval(2), "every_2_weeks"; got != want {
		t.Fatalf("WeekInterval(2) = %q, want %q", got, want)
	}
	if got, want := MonthInterval(3), "every_3_months"; got != want {
		t.Fatalf("MonthInterval(3) = %q, want %q", got, want)
	}
	// A calendar name is not an explicit unit and vice versa.
	if IsCalendarInterval("every_2_weeks") || IsCalendarInterval("every_3_months") {
		t.Fatal("an explicit unit must not parse as a calendar interval")
	}
	for _, name := range []string{"monthly", "quarterly", "semiannual", "annual"} {
		if !IsCalendarInterval(name) {
			t.Fatalf("%q must be a calendar interval", name)
		}
	}
}

func TestScheduleForUnitsDispatch(t *testing.T) {
	t.Parallel()
	loc := time.UTC
	anchor := mustAnchor(t, 15, 0)
	origin := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)

	// A week is the day-count model with length 7*N: it is a day count, not a month.
	ws, err := ScheduleFor("every_2_weeks", anchor, origin, loc)
	if err != nil {
		t.Fatalf("every_2_weeks: %v", err)
	}
	if !ws.IsDayCount() || ws.IsMonthCount() || ws.Days() != 14 {
		t.Fatalf("every_2_weeks dispatched to %+v", ws)
	}

	// A month is the calendar model with an explicit month count.
	ms, err := ScheduleFor("every_3_months", anchor, time.Time{}, loc)
	if err != nil {
		t.Fatalf("every_3_months: %v", err)
	}
	if ms.IsDayCount() || !ms.IsMonthCount() || ms.Months() != 3 {
		t.Fatalf("every_3_months dispatched to %+v", ms)
	}
}

// TestMonthCountClampsAtMonthEnd proves an explicit N-month period uses the calendar
// model: a Jan 31 anchor clamps to Feb 29 and returns to Mar 31, exactly as
// "quarterly" would — it does not become 90 days.
func TestMonthCountClampsAtMonthEnd(t *testing.T) {
	t.Parallel()
	s, err := NewMonthSchedule(1, mustAnchor(t, 31, 0), time.UTC)
	if err != nil {
		t.Fatalf("month schedule: %v", err)
	}
	assertKeys(t, "every 1 month, anchor 31, from 2024-01", goldenPeriods(s, Civil{Year: 2024, Month: time.January}, 4), []string{
		"2024-01-31/2024-02-29",
		"2024-02-29/2024-03-31",
		"2024-03-31/2024-04-30",
		"2024-04-30/2024-05-31",
	})
}

// TestWeekPeriodTiling: an N-week period is exact civil days from its origin, so two
// weeks from a date is exactly fourteen days later, across a month end too.
func TestWeekPeriodTiling(t *testing.T) {
	t.Parallel()
	s, err := NewWeekSchedule(2, mustAnchor(t, 20, 0), time.Date(2024, 1, 20, 0, 0, 0, 0, time.UTC), time.UTC)
	if err != nil {
		t.Fatalf("week schedule: %v", err)
	}
	p := s.PeriodAt(time.Date(2024, 1, 20, 12, 0, 0, 0, time.UTC))
	got := []string{p.Key()}
	for i := 0; i < 2; i++ {
		p = p.Next()
		got = append(got, p.Key())
	}
	assertKeys(t, "every 2 weeks from 2024-01-20", got, []string{
		"2024-01-20/2024-02-03",
		"2024-02-03/2024-02-17",
		"2024-02-17/2024-03-02",
	})
}
