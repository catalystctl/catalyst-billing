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
//
// # Period models
//
// A price is sold either in calendar months (monthly, quarterly, semiannual,
// annual) or as an explicit number of days, weeks or months ("every 45 days",
// "every 2 weeks", "every 3 months"). The persisted interval name says which
// model applies, and this package keeps the models apart:
//
//   - The **calendar model** hangs every period off an anchor day-of-month, which
//     is what makes month ends and leap days correct. The four named intervals and
//     an explicit N-month count both use it; N months is the same arithmetic with
//     a month count of N instead of the interval's fixed 1, 3, 6 or 12.
//   - The **day-count model** counts a fixed number of civil days from an explicit
//     origin, because a day count has no month to hang from. Counting civil days
//     (not 24-hour multiples) is what keeps a boundary on the same wall-clock time
//     across a daylight-saving transition. A week is exactly seven civil days, so
//     an N-week period is the day-count model with a length of 7*N days; it keeps
//     the week unit in its name for display and range validation, but nothing in
//     the arithmetic treats it as anything other than days.
//
// They are separate on purpose. Blending a day count into the month arithmetic
// would silently move the renewal date of every existing subscription, and that is
// the one thing this package must never do. An explicit N-month count deliberately
// uses the calendar model rather than 30*N days: a month is a calendar fact, not a
// day count, and month-end clamping is what customers expect from it.
package calendar

import (
	"fmt"
	"strconv"
	"strings"
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

// ParseInterval parses a stored calendar interval name. It refuses the explicit
// day/week/month forms; ParseDayInterval, ParseWeekInterval and ParseMonthInterval
// parse those, and ScheduleFor accepts all of them.
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

// IsCalendarInterval reports whether name is one of the four named calendar
// intervals. It is the predicate a caller asks before ParseInterval when the
// alternative forms are handled separately.
func IsCalendarInterval(name string) bool {
	_, err := ParseInterval(name)
	return err == nil
}

// --------------------------------------------------- explicit period units --

// The stored form of an explicit period is self-describing: "every_45_days",
// "every_2_weeks", "every_3_months". A self-describing string rather than a
// number and a unit column because it is one value — a period — and two columns
// would let a row say "monthly" with a day count beside it.
const (
	intervalPrefix      = "every_"
	dayIntervalSuffix   = "_days"
	weekIntervalSuffix  = "_weeks"
	monthIntervalSuffix = "_months"
)

// The longest explicit period accepted. A day count is bounded by one leap year,
// and a week or month count by the same one-year horizon: 52 weeks are 364 days,
// and 12 months are the annual calendar interval. A longer count is almost always
// a typo for a named interval, and the database CHECK enforces the same bounds so
// a row written by hand cannot exceed them.
const (
	MaxDayInterval   = 366
	MaxWeekInterval  = 52
	MaxMonthInterval = 12
)

// DayInterval renders n days as the stored interval name: "every_45_days".
func DayInterval(days int) string {
	return intervalPrefix + strconv.Itoa(days) + dayIntervalSuffix
}

// WeekInterval renders n weeks as the stored interval name: "every_2_weeks".
func WeekInterval(weeks int) string {
	return intervalPrefix + strconv.Itoa(weeks) + weekIntervalSuffix
}

// MonthInterval renders n months as the stored interval name: "every_3_months".
func MonthInterval(months int) string {
	return intervalPrefix + strconv.Itoa(months) + monthIntervalSuffix
}

// ParseDayInterval parses the day-count interval form and reports whether s was
// one. It accepts only the canonical spelling, so one period has one string in the
// database and the uniqueness of a period key cannot be defeated by "every_045_days".
func ParseDayInterval(s string) (int, bool) {
	return parseUnitInterval(s, dayIntervalSuffix, MaxDayInterval)
}

// ParseWeekInterval parses the explicit week form: "every_2_weeks".
func ParseWeekInterval(s string) (int, bool) {
	return parseUnitInterval(s, weekIntervalSuffix, MaxWeekInterval)
}

// ParseMonthInterval parses the explicit month form: "every_3_months".
func ParseMonthInterval(s string) (int, bool) {
	return parseUnitInterval(s, monthIntervalSuffix, MaxMonthInterval)
}

// parseUnitInterval parses one canonical explicit-period spelling. The leading-zero
// check is what makes one period have one name: "every_02_weeks" is refused even
// though it would parse, so a period key cannot be duplicated by a second spelling.
func parseUnitInterval(s, suffix string, max int) (int, bool) {
	if !strings.HasPrefix(s, intervalPrefix) || !strings.HasSuffix(s, suffix) {
		return 0, false
	}
	digits := strings.TrimSuffix(strings.TrimPrefix(s, intervalPrefix), suffix)
	count, err := strconv.Atoi(digits)
	if err != nil || count < 1 || count > max {
		return 0, false
	}
	if strconv.Itoa(count) != digits {
		return 0, false
	}
	return count, true
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

// Schedule is a period model plus an anchor evaluated in one business timezone.
//
// Exactly one of the models is in use: a named calendar interval (monthly,
// quarterly, semiannual, annual), an explicit month count ("every 3 months"), or a
// day count ("every 45 days", "every 2 weeks"). See the package comment for why
// they are not one blended model.
type Schedule struct {
	interval Interval
	// months is the explicit month count, zero for a named interval and for a
	// day-count schedule.
	months int
	// days is the day-count length, zero for a calendar interval. A week count is
	// converted to days here, so the arithmetic is the same civil-day model.
	days   int
	anchor Anchor
	loc    *time.Location
	// origin is the first period start of a day-count grid, with the wall-clock
	// time of day that boundary actually has. It is used only when hasOrigin is
	// set; otherwise the grid floats and is derived from the instant being asked
	// about (see periodOrigin).
	origin    Civil
	hasOrigin bool
}

// NewSchedule builds a calendar schedule. loc must be non-nil.
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

// NewMonthSchedule builds a calendar schedule with an explicit month count:
// every period is exactly months calendar months from the anchor, so month-end
// clamping and leap days behave exactly as they do for the named intervals.
func NewMonthSchedule(months int, anchor Anchor, loc *time.Location) (*Schedule, error) {
	if months < 1 || months > MaxMonthInterval {
		return nil, fmt.Errorf("calendar: month count %d out of range 1..%d", months, MaxMonthInterval)
	}
	if err := anchor.Validate(); err != nil {
		return nil, err
	}
	if loc == nil {
		return nil, fmt.Errorf("calendar: nil location")
	}
	return &Schedule{interval: Monthly, months: months, anchor: anchor, loc: loc}, nil
}

// NewWeekSchedule builds a week-count schedule: every period is exactly weeks
// weeks, which is 7*weeks civil days counted from origin. A week has no month to
// hang from, so it uses the day-count model rather than a 30-day approximation.
func NewWeekSchedule(weeks int, anchor Anchor, origin time.Time, loc *time.Location) (*Schedule, error) {
	if weeks < 1 || weeks > MaxWeekInterval {
		return nil, fmt.Errorf("calendar: week count %d out of range 1..%d", weeks, MaxWeekInterval)
	}
	return NewDaySchedule(weeks*7, anchor, origin, loc)
}

// NewDaySchedule builds a day-count schedule: every period is exactly days civil
// days, counted from origin.
//
// origin must be the first period start of the grid. A subscription passes its
// persisted period start, which is what makes "every 45 days" stable across
// restarts: the grid is the same grid on every run, so the boundary the customer
// was told about is the boundary that is billed. Its wall-clock time of day is kept
// rather than replaced by the anchor's, so the boundary is reproduced exactly.
func NewDaySchedule(days int, anchor Anchor, origin time.Time, loc *time.Location) (*Schedule, error) {
	if days < 1 || days > MaxDayInterval {
		return nil, fmt.Errorf("calendar: day count %d out of range 1..%d", days, MaxDayInterval)
	}
	if err := anchor.Validate(); err != nil {
		return nil, err
	}
	if loc == nil {
		return nil, fmt.Errorf("calendar: nil location")
	}
	if origin.IsZero() {
		return nil, fmt.Errorf("calendar: a day-count schedule needs an origin instant")
	}
	c := CivilOf(origin, loc)
	return &Schedule{
		days:      days,
		anchor:    anchor,
		loc:       loc,
		origin:    c,
		hasOrigin: true,
	}, nil
}

// newFloatingDaySchedule builds a day-count schedule whose grid is derived from the
// instant being asked about rather than fixed in advance. It is the price-to-first-
// period case: a catalog price has an anchor day and no stored boundary, so its first
// period starts on the anchor day of the month it is bought in — the same rule a
// calendar interval already uses.
func newFloatingDaySchedule(days int, anchor Anchor, loc *time.Location) (*Schedule, error) {
	if days < 1 || days > MaxDayInterval {
		return nil, fmt.Errorf("calendar: day count %d out of range 1..%d", days, MaxDayInterval)
	}
	if err := anchor.Validate(); err != nil {
		return nil, err
	}
	if loc == nil {
		return nil, fmt.Errorf("calendar: nil location")
	}
	return &Schedule{days: days, anchor: anchor, loc: loc}, nil
}

// ScheduleFor builds the schedule a stored interval name describes.
//
// origin is the first period start of a day-count grid and is ignored by a calendar
// interval, which hangs from a month instead. A subscription passes its stored next
// billing instant; a derivation with no stored boundary passes the zero Time, which
// leaves the grid floating (see newFloatingDaySchedule).
func ScheduleFor(intervalName string, anchor Anchor, origin time.Time, loc *time.Location) (*Schedule, error) {
	if days, ok := ParseDayInterval(intervalName); ok {
		if origin.IsZero() {
			return newFloatingDaySchedule(days, anchor, loc)
		}
		return NewDaySchedule(days, anchor, origin, loc)
	}
	// A week is a fixed number of civil days, so it is the day-count model with a
	// length of 7*N. It is parsed before months so "every_2_weeks" can never be
	// mistaken for a month count.
	if weeks, ok := ParseWeekInterval(intervalName); ok {
		return newDayScheduleForCount(weeks*7, anchor, origin, loc)
	}
	// A month is a calendar fact, not a day count: an explicit N-month period uses
	// the calendar model so the anchor clamps at month ends exactly as "quarterly"
	// does. It deliberately does not become 30*N days.
	if months, ok := ParseMonthInterval(intervalName); ok {
		return NewMonthSchedule(months, anchor, loc)
	}
	interval, err := ParseInterval(intervalName)
	if err != nil {
		return nil, err
	}
	return NewSchedule(interval, anchor, loc)
}

// newDayScheduleForCount builds a day-count schedule from an already-validated day
// length, choosing the floating grid when no origin was supplied.
func newDayScheduleForCount(days int, anchor Anchor, origin time.Time, loc *time.Location) (*Schedule, error) {
	if origin.IsZero() {
		return newFloatingDaySchedule(days, anchor, loc)
	}
	return NewDaySchedule(days, anchor, origin, loc)
}

// FirstPeriod derives the first billing period of a price bought at now, from the
// price's stored interval, anchor day and timezone.
//
// It is the one derivation every purchase path uses — orders, checkout, the staff
// create screen and the customer portal — so a new period model cannot be added to
// one path and forgotten in another. For a calendar interval it returns the period
// containing now, which is what those paths have always done.
func FirstPeriod(intervalName string, anchorDay int, timezone string, now time.Time) (Period, error) {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return Period{}, fmt.Errorf("calendar: unknown timezone %q", timezone)
	}
	anchor, err := NewAnchor(anchorDay, 0, 0)
	if err != nil {
		return Period{}, err
	}
	schedule, err := ScheduleFor(intervalName, anchor, time.Time{}, loc)
	if err != nil {
		return Period{}, err
	}
	return schedule.PeriodAt(now), nil
}

// Interval returns the named calendar interval of the schedule. It is the zero
// Interval for a day-count schedule and for an explicit month count (which has no
// named interval): ask IsDayCount or IsMonthCount first.
func (s *Schedule) Interval() Interval { return s.interval }

// IsDayCount reports whether this schedule counts days rather than calendar months.
// A week-count schedule is a day-count schedule; its length is 7*N days.
func (s *Schedule) IsDayCount() bool { return s.days > 0 }

// IsMonthCount reports whether this schedule uses an explicit month count.
func (s *Schedule) IsMonthCount() bool { return s.months > 0 }

// Days returns the day-count length, or zero for a calendar schedule.
func (s *Schedule) Days() int { return s.days }

// Months returns the number of calendar months per period. It is zero for a
// day-count schedule.
func (s *Schedule) Months() int {
	if s.months > 0 {
		return s.months
	}
	return s.interval.Months()
}

// Anchor returns the schedule anchor.
func (s *Schedule) Anchor() Anchor { return s.anchor }

// Location returns the business timezone.
func (s *Schedule) Location() *time.Location { return s.loc }

// Period is one half-open billing period [Start, End) identified by its
// nominal (unclamped) anchor civil time.
type Period struct {
	interval Interval
	// months is the explicit month count of the period, zero for a named interval
	// and for a day-count period.
	months int
	// days is the day-count length of the period, zero for a calendar period. A
	// week-count period carries its length in days here.
	days int
	loc  *time.Location
	// anchor is the full schedule anchor (nominal day plus time of day). It is
	// carried by every period so that period arithmetic never has to consult
	// the schedule again.
	anchor Anchor
	// nominalStart is the unclamped anchor date for this period: Day is always
	// the anchor day, which is what makes month-end periods re-expand. For a
	// day-count period it is the period's real start date, which no clamp ever
	// rewrites because it is produced by day arithmetic.
	nominalStart Civil

	// Start and End are the resolved instants (UTC-based; compare with .Equal).
	Start time.Time
	End   time.Time
	// StartCivil and EndCivil are the wall clocks those instants show in the
	// business timezone, after clamping and DST resolution.
	StartCivil Civil
	EndCivil   Civil
}

// Interval returns the period's named calendar interval. A day-count period and an
// explicit month count have none, so this is the zero Interval for them: ask
// IsDayCount or IsMonthCount first rather than reading it as Monthly.
func (p Period) Interval() Interval { return p.interval }

// IsDayCount reports whether the period counts days rather than calendar months.
func (p Period) IsDayCount() bool { return p.days > 0 }

// IsMonthCount reports whether the period uses an explicit month count.
func (p Period) IsMonthCount() bool { return p.months > 0 }

// Days returns the day-count length of the period, or zero for a calendar period.
func (p Period) Days() int { return p.days }

// Months returns the number of calendar months in the period, or zero for a
// day-count period.
func (p Period) Months() int {
	if p.months > 0 {
		return p.months
	}
	return p.interval.Months()
}

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
	if p.days > 0 {
		return fmt.Sprintf("every %d days [%s, %s)", p.days, p.StartCivil, p.EndCivil)
	}
	if p.months > 0 {
		return fmt.Sprintf("every %d months [%s, %s)", p.months, p.StartCivil, p.EndCivil)
	}
	return fmt.Sprintf("%s [%s, %s)", p.interval, p.StartCivil, p.EndCivil)
}

// Next returns the following period. The nominal anchor day is preserved, so a
// Jan 31 period is followed by a Feb 28 period which is followed by Mar 31. A
// day-count period is followed by one that starts exactly days later, which is what
// makes it land on the right date across a month end and a leap day.
func (p Period) Next() Period { return p.shift(1) }

// Prev returns the preceding period.
func (p Period) Prev() Period { return p.shift(-1) }

func (p Period) shift(n int) Period {
	if p.days > 0 {
		return p.buildDay(addDays(p.nominalStart, p.days*n))
	}
	start := p.nominalStart
	months := p.Months() * n
	year, month := addMonths(start.Year, start.Month, months)
	return p.build(year, month)
}

// buildDay resolves a day-count period from its start civil date. Day arithmetic
// never produces an impossible date, so there is nothing to clamp: the start is a
// real date and the end is the start plus a whole number of days.
func (p Period) buildDay(start Civil) Period {
	end := addDays(start, p.days)
	startI := resolve(p.loc, start)
	endI := resolve(p.loc, end)
	return Period{
		days:         p.days,
		loc:          p.loc,
		anchor:       p.anchor,
		nominalStart: start,
		Start:        startI,
		End:          endI,
		StartCivil:   CivilOf(startI, p.loc),
		EndCivil:     CivilOf(endI, p.loc),
	}
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
	endYear, endMonth := addMonths(year, month, p.Months())
	endNom := nominal(endYear, endMonth)

	start := resolve(p.loc, clamp(startNom))
	end := resolve(p.loc, clamp(endNom))

	return Period{
		interval:     p.interval,
		months:       p.months,
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
	seed := Period{interval: s.interval, months: s.months, loc: s.loc, anchor: s.anchor}
	return seed.build(year, month)
}

// PeriodAt returns the period containing t.
//
// A calendar period is the one anchored in t's month, walked backwards or forwards
// until it contains t. A day-count period is the chunk of the grid that contains t:
// its start is a whole number of periods after the grid's origin, which is what makes
// the grid stable rather than "now minus N days".
func (s *Schedule) PeriodAt(t time.Time) Period {
	if s.days > 0 {
		return s.dayPeriodContaining(t)
	}
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

// dayPeriodContaining returns the day-count period containing t.
//
// The candidate is found by whole-day arithmetic from the grid's origin, and then
// nudged by whole periods until it contains t. The nudge is not redundant: a period
// boundary carries a time of day, so two instants one day apart can sit on opposite
// sides of it, and the date difference alone cannot see that.
func (s *Schedule) dayPeriodContaining(t time.Time) Period {
	c := CivilOf(t, s.loc)
	origin := s.periodOrigin(t)
	k := floorDiv(civilDaysBetween(origin, c), s.days)
	p := s.dayPeriod(addDays(origin, k*s.days))
	for t.Before(p.Start) {
		p = p.Prev()
	}
	for !t.Before(p.End) {
		p = p.Next()
	}
	return p
}

// PeriodStartingAt returns the period that begins at t.
//
// It is the renewal scanner's view: the persisted boundary *is* the start of the
// period being billed, not an instant that has to be found in a grid. For a calendar
// interval the boundary must be on the calendar, and when it is not (an anchor or
// timezone edit moved the grid) the period whose nominal start falls in t's month is
// used — the re-anchoring rule the renewal engine has always applied. For a day-count
// interval the stored boundary is the grid's origin, so it is reproduced exactly.
func (s *Schedule) PeriodStartingAt(t time.Time) Period {
	if s.days > 0 {
		return s.dayPeriodContaining(t)
	}
	p := s.PeriodAt(t)
	if p.Start.Equal(t) {
		return p
	}
	c := CivilOf(t, s.loc)
	return s.PeriodStartingIn(c.Year, c.Month)
}

// periodOrigin returns the first period start of the day-count grid that applies to
// t: the grid's fixed origin when it has one, otherwise the anchor day of the month
// containing t, clamped by exactly the rule a calendar period clamps by. That clamp
// is what lets a price anchored on the 31st still produce a period in February.
func (s *Schedule) periodOrigin(t time.Time) Civil {
	if s.hasOrigin {
		return s.origin
	}
	c := CivilOf(t, s.loc)
	return clamp(Civil{
		Year:   c.Year,
		Month:  c.Month,
		Day:    s.anchor.Day,
		Hour:   s.anchor.Hour,
		Minute: s.anchor.Minute,
		Second: s.anchor.Second,
	})
}

// dayPeriod builds the period of exactly s.days civil days that starts on start.
func (s *Schedule) dayPeriod(start Civil) Period {
	seed := Period{days: s.days, loc: s.loc, anchor: s.anchor}
	return seed.buildDay(start)
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

// addDays returns the civil date n days later (or earlier, for negative n), keeping
// the wall-clock time of day. The arithmetic runs on a UTC date so a leap day and a
// month end are exactly what the calendar says they are, and the result is resolved in
// the business timezone afterwards by the caller. That two-step — civil days first,
// instant second — is what keeps an "every 45 days" boundary on the same wall-clock
// time across a daylight-saving transition instead of drifting by an hour.
func addDays(c Civil, n int) Civil {
	t := time.Date(c.Year, c.Month, c.Day, c.Hour, c.Minute, c.Second, 0, time.UTC).AddDate(0, 0, n)
	return Civil{
		Year:   t.Year(),
		Month:  t.Month(),
		Day:    t.Day(),
		Hour:   c.Hour,
		Minute: c.Minute,
		Second: c.Second,
	}
}

// civilDaysBetween returns the number of whole days from one civil date to another,
// ignoring the time of day. Both are read as UTC dates, so the answer is exact: no
// daylight-saving hour can make a day 23 or 25 hours long here.
func civilDaysBetween(from, to Civil) int {
	a := time.Date(from.Year, from.Month, from.Day, 0, 0, 0, 0, time.UTC)
	b := time.Date(to.Year, to.Month, to.Day, 0, 0, 0, 0, time.UTC)
	return int(b.Sub(a) / (24 * time.Hour))
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
