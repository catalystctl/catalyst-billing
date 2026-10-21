// Package calendar owns the billing calendar: anniversary anchors, period
// arithmetic, month-end clamping, leap-day behaviour and daylight-saving
// resolution in a persisted business timezone.
//
// The rules are deliberately explicit rather than convenient, because a
// recurring-billing mistake silently charges the wrong people on the wrong day.
//
// # Anchor semantics
//
// An anchor is a nominal day-of-month (1..31) plus a wall-clock time-of-day in
// the business timezone. The nominal day is never rewritten by clamping: an
// anchor on the 31st produces Jan 31, Feb 28 (or 29), Mar 31, Apr 30, and so on.
// Clamping happens only when a period boundary is resolved, so the anchor rule
// survives short months instead of drifting permanently to the 28th.
//
// # Leap day
//
// An annual anchor on Feb 29 yields Feb 29 in leap years and Feb 28 otherwise,
// returning to Feb 29 at the next leap year.
//
// # Daylight saving
//
// Boundaries are stored as UTC instants derived from the business timezone.
// When a requested wall-clock time does not exist (spring forward) it is shifted
// forward by the gap, matching java.time and iCalendar. When it occurs twice
// (fall back) the earlier instant, i.e. the first occurrence, is used.
package calendar

import (
	"fmt"
	"time"

	// Embed the IANA database so billing boundaries cannot change because a
	// host's tzdata package was updated or is missing.
	_ "time/tzdata"
)

// Interval is the recurring period length.
type Interval int

const (
	Monthly Interval = iota
	Quarterly
	SemiAnnual
	Annual
)

// Months returns the number of calendar months per interval.
func (i Interval) Months() int {
	switch i {
	case Monthly:
		return 1
	case Quarterly:
		return 3
	case SemiAnnual:
		return 6
	case Annual:
		return 12
	default:
		return 0
	}
}

// Valid reports whether the interval is known.
func (i Interval) Valid() bool { return i.Months() > 0 }

func (i Interval) String() string {
	switch i {
	case Monthly:
		return "monthly"
	case Quarterly:
		return "quarterly"
	case SemiAnnual:
		return "semiannual"
	case Annual:
		return "annual"
	default:
		return fmt.Sprintf("interval(%d)", int(i))
	}
}

// ParseInterval parses a stored interval name.
func ParseInterval(s string) (Interval, error) {
	switch s {
	case "monthly":
		return Monthly, nil
	case "quarterly":
		return Quarterly, nil
	case "semiannual":
		return SemiAnnual, nil
	case "annual":
		return Annual, nil
	default:
		return 0, fmt.Errorf("calendar: unknown interval %q", s)
	}
}

// Civil is a wall-clock date-time with no offset: what a customer in the
// business timezone would read on the invoice.
type Civil struct {
	Year   int
	Month  time.Month
	Day    int
	Hour   int
	Minute int
	Second int
}

// DateString renders the date part as YYYY-MM-DD.
func (c Civil) DateString() string {
	return fmt.Sprintf("%04d-%02d-%02d", c.Year, int(c.Month), c.Day)
}

func (c Civil) String() string {
	return fmt.Sprintf("%s %02d:%02d:%02d", c.DateString(), c.Hour, c.Minute, c.Second)
}

// CivilOf returns the wall clock a given instant shows in loc.
func CivilOf(t time.Time, loc *time.Location) Civil {
	l := t.In(loc)
	return Civil{
		Year:   l.Year(),
		Month:  l.Month(),
		Day:    l.Day(),
		Hour:   l.Hour(),
		Minute: l.Minute(),
		Second: l.Second(),
	}
}

// DaysIn returns the number of days in the given month.
func DaysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// Anchor is the persisted billing anchor.
type Anchor struct {
	// Day is the nominal day of month, 1..31. Values above the length of a
	// month are clamped when a boundary is resolved.
	Day int
	// Hour, Minute, Second are the wall-clock time of day in the business
	// timezone.
	Hour, Minute, Second int
}

// NewAnchor validates an anchor.
func NewAnchor(day, hour, minute int) (Anchor, error) {
	a := Anchor{Day: day, Hour: hour, Minute: minute}
	if err := a.Validate(); err != nil {
		return Anchor{}, err
	}
	return a, nil
}

// Validate checks the anchor ranges.
func (a Anchor) Validate() error {
	if a.Day < 1 || a.Day > 31 {
		return fmt.Errorf("calendar: anchor day %d out of range 1..31", a.Day)
	}
	if a.Hour < 0 || a.Hour > 23 {
		return fmt.Errorf("calendar: anchor hour %d out of range 0..23", a.Hour)
	}
	if a.Minute < 0 || a.Minute > 59 {
		return fmt.Errorf("calendar: anchor minute %d out of range 0..59", a.Minute)
	}
	if a.Second < 0 || a.Second > 59 {
		return fmt.Errorf("calendar: anchor second %d out of range 0..59", a.Second)
	}
	return nil
}

func (a Anchor) String() string {
	return fmt.Sprintf("day %d at %02d:%02d:%02d", a.Day, a.Hour, a.Minute, a.Second)
}

// Schedule is an interval plus an anchor evaluated in one business timezone.
type Schedule struct {
	interval Interval
	anchor   Anchor
	loc      *time.Location
}

// NewSchedule builds a schedule. loc must be non-nil.
func NewSchedule(interval Interval, anchor Anchor, loc *time.Location) (*Schedule, error) {
	if !interval.Valid() {
		return nil, fmt.Errorf("calendar: invalid interval %s", interval)
	}
	if err := anchor.Validate(); err != nil {
		return nil, err
	}
	if loc == nil {
		return nil, fmt.Errorf("calendar: nil location")
	}
	return &Schedule{interval: interval, anchor: anchor, loc: loc}, nil
}

// Interval returns the schedule interval.
func (s *Schedule) Interval() Interval { return s.interval }

// Anchor returns the schedule anchor.
func (s *Schedule) Anchor() Anchor { return s.anchor }

// Location returns the business timezone.
func (s *Schedule) Location() *time.Location { return s.loc }

// Period is one half-open billing period [Start, End) identified by its
// nominal (unclamped) anchor civil time.
type Period struct {
	interval Interval
	loc      *time.Location
	// anchor is the full schedule anchor (nominal day plus time of day). It is
	// carried by every period so that period arithmetic never has to consult
	// the schedule again.
	anchor Anchor
	// nominalStart is the unclamped anchor date for this period: Day is always
	// the anchor day, which is what makes month-end periods re-expand.
	nominalStart Civil

	// Start and End are the resolved instants (UTC-based; compare with .Equal).
	Start time.Time
	End   time.Time
	// StartCivil and EndCivil are the wall clocks those instants show in the
	// business timezone, after clamping and DST resolution.
	StartCivil Civil
	EndCivil   Civil
}

// Interval returns the period interval.
func (p Period) Interval() Interval { return p.interval }

// Location returns the business timezone.
func (p Period) Location() *time.Location { return p.loc }

// NominalStart returns the unclamped anchor date that identifies the period.
func (p Period) NominalStart() Civil { return p.nominalStart }

// Anchor returns the anchor this period was generated from.
func (p Period) Anchor() Anchor { return p.anchor }

// Duration is the length of the period as elapsed time. It is not always a
// whole number of days: a period containing a DST transition is an hour longer
// or shorter.
func (p Period) Duration() time.Duration { return p.End.Sub(p.Start) }

// Contains reports whether t falls in [Start, End).
func (p Period) Contains(t time.Time) bool {
	return !t.Before(p.Start) && t.Before(p.End)
}

// Key is a stable identifier for the period within one schedule, suitable for
// a database uniqueness constraint (one renewal invoice per subscription per
// period).
func (p Period) Key() string {
	return p.StartCivil.DateString() + "/" + p.EndCivil.DateString()
}

func (p Period) String() string {
	return fmt.Sprintf("%s [%s, %s)", p.interval, p.StartCivil, p.EndCivil)
}

// Next returns the following period. The nominal anchor day is preserved, so a
// Jan 31 period is followed by a Feb 28 period which is followed by Mar 31.
func (p Period) Next() Period { return p.shift(1) }

// Prev returns the preceding period.
func (p Period) Prev() Period { return p.shift(-1) }

func (p Period) shift(n int) Period {
	start := p.nominalStart
	months := p.interval.Months() * n
	year, month := addMonths(start.Year, start.Month, months)
	return p.build(year, month)
}

// build resolves a period from a nominal (year, month) using the period's
// anchor day and time of day.
func (p Period) build(year int, month time.Month) Period {
	nominal := func(y int, m time.Month) Civil {
		return Civil{
			Year:   y,
			Month:  m,
			Day:    p.anchor.Day,
			Hour:   p.anchor.Hour,
			Minute: p.anchor.Minute,
			Second: p.anchor.Second,
		}
	}
	startNom := nominal(year, month)
	endYear, endMonth := addMonths(year, month, p.interval.Months())
	endNom := nominal(endYear, endMonth)

	start := resolve(p.loc, clamp(startNom))
	end := resolve(p.loc, clamp(endNom))

	return Period{
		interval:     p.interval,
		loc:          p.loc,
		anchor:       p.anchor,
		nominalStart: startNom,
		Start:        start,
		End:          end,
		StartCivil:   CivilOf(start, p.loc),
		EndCivil:     CivilOf(end, p.loc),
	}
}

// PeriodStartingIn returns the period whose nominal start falls in year/month.
// A month-end anchor is clamped for resolution but retained nominally.
func (s *Schedule) PeriodStartingIn(year int, month time.Month) Period {
	seed := Period{interval: s.interval, loc: s.loc, anchor: s.anchor}
	return seed.build(year, month)
}

// PeriodAt returns the period containing t.
func (s *Schedule) PeriodAt(t time.Time) Period {
	c := CivilOf(t, s.loc)
	p := s.PeriodStartingIn(c.Year, c.Month)
	for t.Before(p.Start) {
		p = p.Prev()
	}
	for !t.Before(p.End) {
		p = p.Next()
	}
	return p
}

// NextStartAfter returns the first period boundary strictly after t, which is
// what a renewal scanner needs to decide whether work is due.
func (s *Schedule) NextStartAfter(t time.Time) time.Time {
	p := s.PeriodAt(t)
	if t.Before(p.Start) {
		return p.Start
	}
	return p.End
}

// PeriodsIn returns every period that intersects [from, to), including a
// period that began before from. It is used to check coverage of a time range.
func (s *Schedule) PeriodsIn(from, to time.Time) []Period {
	if !from.Before(to) {
		return nil
	}
	p := s.PeriodAt(from)
	var out []Period
	for p.Start.Before(to) {
		if p.End.After(from) {
			out = append(out, p)
		}
		p = p.Next()
	}
	return out
}

// PeriodStartsIn returns the periods whose start falls in [from, to). This is
// the renewal-scanner view: after downtime it lists exactly the billing dates
// that came due, and it never includes a period that was already handled before
// the outage.
func (s *Schedule) PeriodStartsIn(from, to time.Time) []Period {
	if !from.Before(to) {
		return nil
	}
	p := s.PeriodAt(from)
	if p.Start.Before(from) {
		p = p.Next()
	}
	var out []Period
	for p.Start.Before(to) {
		out = append(out, p)
		p = p.Next()
	}
	return out
}

// resolve converts a wall clock in the business timezone to an instant,
// applying the documented gap and ambiguity policies.
func resolve(loc *time.Location, c Civil) time.Time {
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
		if CivilOf(cand, loc) == c {
			if !found || cand.Before(best) {
				best = cand
				found = true
			}
		}
	}
	if found {
		return best
	}
	// The wall clock does not exist: shift forward by the gap by interpreting
	// it with the offset in effect before the transition.
	_, before := probe.Add(-36 * time.Hour).In(loc).Zone()
	return probe.Add(-time.Duration(before) * time.Second)
}

func clamp(c Civil) Civil {
	if d := DaysIn(c.Year, c.Month); c.Day > d {
		c.Day = d
	}
	return c
}

// addMonths adds a (possibly negative) month count using floor division so that
// month arithmetic is symmetric around year boundaries.
func addMonths(year int, month time.Month, months int) (int, time.Month) {
	total := year*12 + int(month) - 1 + months
	y := floorDiv(total, 12)
	m := total - y*12 + 1
	return y, time.Month(m)
}

func floorDiv(a, b int) int {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}
