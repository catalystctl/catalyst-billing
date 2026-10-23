package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/calendar"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/service"
)

// The proration of a day-count period ("every 45 days") is the fraction of *that* period
// that remains, computed as an exact rational of elapsed time. There is no 30-day month and
// no float anywhere in the answer.

// A 45-day period that spans the leap day of 2024 is exactly 45 days long, so a change 15
// days in leaves exactly two thirds of it, and 30.00 of a 45.00 difference is charged.
func TestProrationOnADayCountPeriodIsExact(t *testing.T) {
	loc, err := time.LoadLocation("UTC")
	if err != nil {
		t.Fatalf("timezone: %v", err)
	}
	schedule, err := calendar.ScheduleFor("every_45_days", calendar.Anchor{Day: 20}, time.Date(2024, 1, 20, 0, 0, 0, 0, loc), loc)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	period := schedule.PeriodAt(time.Date(2024, 1, 20, 0, 0, 0, 0, loc))
	if got, want := period.Key(), "2024-01-20/2024-03-05"; got != want {
		t.Fatalf("period = %q, want %q", got, want)
	}

	p, err := service.ComputeProration(period.Start, period.End, period.Start.Add(15*24*time.Hour))
	if err != nil {
		t.Fatalf("compute proration: %v", err)
	}
	if got, want := p.Total, 45*24*time.Hour; got != want {
		t.Fatalf("total = %s, want %s", got, want)
	}
	if got, want := p.Ratio.String(), "2/3"; got != want {
		t.Fatalf("ratio = %s, want %s", got, want)
	}

	got, err := service.ProratedDifference(money.New(4500, usd), money.New(9000, usd), p, money.RoundHalfUp)
	if err != nil {
		t.Fatalf("prorated difference: %v", err)
	}
	if got.Minor() != 3000 {
		t.Fatalf("prorated = %d minor, want 3000 (30.00 exactly)", got.Minor())
	}

	// Two thirds of the way through, exactly one third remains.
	late, err := service.ComputeProration(period.Start, period.End, period.Start.Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("compute proration: %v", err)
	}
	if got, want := late.Ratio.String(), "1/3"; got != want {
		t.Fatalf("late ratio = %s, want %s", got, want)
	}
	amount, err := service.ProratedDifference(money.New(4500, usd), money.New(9000, usd), late, money.RoundHalfUp)
	if err != nil {
		t.Fatalf("prorated difference: %v", err)
	}
	if amount.Minor() != 1500 {
		t.Fatalf("late prorated = %d minor, want 1500 (15.00 exactly)", amount.Minor())
	}
}

// The divisor is the period's own length. The same elapsed time in a 45-day period, a 30-day
// month and a 31-day month gives three different fractions — which a fixed 30-day (or 30.44
// day) divisor could not do.
func TestProrationUsesTheDayCountPeriodsOwnLength(t *testing.T) {
	oneDay := 24 * time.Hour
	fortyFive := time.Date(2024, 1, 20, 0, 0, 0, 0, time.UTC)
	month := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)

	dayCount, err := service.ComputeProration(fortyFive, fortyFive.Add(45*oneDay), fortyFive.Add(oneDay))
	if err != nil {
		t.Fatalf("day-count proration: %v", err)
	}
	may, err := service.ComputeProration(month, month.Add(31*oneDay), month.Add(oneDay))
	if err != nil {
		t.Fatalf("may proration: %v", err)
	}
	if got, want := dayCount.Ratio.String(), "44/45"; got != want {
		t.Fatalf("day-count ratio = %s, want %s", got, want)
	}
	if got, want := may.Ratio.String(), "30/31"; got != want {
		t.Fatalf("may ratio = %s, want %s", got, want)
	}
	if dayCount.Ratio.String() == may.Ratio.String() {
		t.Fatal("a 45-day period and a 31-day month produced the same ratio")
	}
}

// A day-count period that spans a daylight-saving transition is 45 days minus an hour, and
// the ratio stays exact: no float creeps into the arithmetic when the period is not a whole
// number of 24-hour days.
func TestProrationOnADayCountPeriodAcrossDSTIsExact(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("timezone: %v", err)
	}
	schedule, err := calendar.ScheduleFor("every_45_days", calendar.Anchor{Day: 1}, time.Date(2024, 3, 1, 0, 0, 0, 0, loc), loc)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	period := schedule.PeriodAt(time.Date(2024, 3, 1, 0, 0, 0, 0, loc))
	if got, want := period.Key(), "2024-03-01/2024-04-15"; got != want {
		t.Fatalf("period = %q, want %q", got, want)
	}

	p, err := service.ComputeProration(period.Start, period.End, period.Start.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("compute proration: %v", err)
	}
	// 45 days minus the hour the spring transition removes: 1079 hours in total, 1055
	// remaining after one day. The rational is exact, which is the point.
	if got, want := p.Total, time.Duration(45*24-1)*time.Hour; got != want {
		t.Fatalf("total = %s, want %s", got, want)
	}
	if got, want := p.Ratio.String(), "1055/1079"; got != want {
		t.Fatalf("ratio = %s, want %s", got, want)
	}
}

// An upgrade mid-way through a day-count period prorates against the 45 days the customer
// actually paid for, and issues an invoice for the exact difference.
func TestUpgradeOnADayCountSubscriptionProratesTheRealPeriod(t *testing.T) {
	loc, err := time.LoadLocation("UTC")
	if err != nil {
		t.Fatalf("timezone: %v", err)
	}
	periodStart := time.Date(2024, 1, 20, 0, 0, 0, 0, loc)
	schedule, err := calendar.ScheduleFor("every_45_days", calendar.Anchor{Day: 20}, periodStart, loc)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	period := schedule.PeriodAt(periodStart)

	// Fifteen days into the 45-day period, so exactly two thirds remain.
	now := period.Start.Add(15 * 24 * time.Hour)
	f := newFixtureWithPeriod(t, 4500, "every_45_days", now, period.Start, period.End)
	_, targetPrice := f.addUpgradeTarget(t, 9000, "every_45_days")

	res, err := f.store.Upgrade(context.Background(), service.UpgradeRequest{
		ServiceID: f.serviceID, PriceVersionID: targetPrice, UserID: f.userID, Reason: "more",
	})
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if got, want := res.Proration.Ratio.String(), "2/3"; got != want {
		t.Fatalf("proration ratio = %s, want %s", got, want)
	}
	if got, want := res.Proration.Total, 45*24*time.Hour; got != want {
		t.Fatalf("prorated period = %s, want %s", got, want)
	}
	// 45.00 difference over two thirds of the period is 30.00 exactly.
	if res.ProratedMinor != 3000 {
		t.Fatalf("prorated = %d minor, want 3000", res.ProratedMinor)
	}

	var total int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT total_minor FROM invoices WHERE id = $1`, res.InvoiceID).Scan(&total); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	if total != 3000 {
		t.Fatalf("invoice total = %d, want 3000", total)
	}
}
