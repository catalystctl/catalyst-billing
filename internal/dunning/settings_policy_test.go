package dunning_test

import (
	"context"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/dunning"
	"github.com/catalyst/billing/internal/settings"
)

// The dunning ladder is configuration (OPS-09).
//
// Before this, `dunning.DefaultPolicy()` was the only answer: an operator could set "cancel an
// unpaid order after 30 days" on the settings page — a rule that can take a customer's service
// away — while the reminders that lead up to it ran on numbers compiled into the binary. These
// tests fail if the stored thresholds stop governing the ladder, if the switch stops stopping it,
// or if a zero day count is silently read as "act now".

// setSetting writes one operations setting through the same accessor the panel uses.
func setSetting(t *testing.T, f *fixture, key, value string) {
	t.Helper()
	if _, err := settings.New(f.pool).Set(context.Background(), settings.GroupCronjob, key, value,
		"", nil); err != nil {
		t.Fatalf("set %s.%s = %q: %v", settings.GroupCronjob, key, value, err)
	}
}

// The gap between reminders comes from `cronjob.reminder_days`.
//
// The fixture's engine is built exactly like the worker's — NewEngine(pool, DefaultPolicy()) — so
// this is the production path. With the stored value at one day, the next action must be one day
// out; the default is two, and the difference is the whole point of the test.
func TestReminderIntervalComesFromSettings(t *testing.T) {
	f := newFixture(t, baseTime.AddDate(0, 0, -1))
	setSetting(t, f, settings.KeyReminderDays, "1")

	if _, err := f.engine.Advance(context.Background(), 100); err != nil {
		t.Fatalf("advance: %v", err)
	}

	var next *time.Time
	if err := f.pool.QueryRow(context.Background(),
		`SELECT next_action_at FROM dunning_states WHERE invoice_id = $1`, f.invoiceID).
		Scan(&next); err != nil {
		t.Fatalf("read next action: %v", err)
	}
	if next == nil {
		t.Fatal("the ladder scheduled nothing; a reminder step must schedule the next one")
	}
	want := f.now.Add(24 * time.Hour)
	if !next.Equal(want) {
		t.Fatalf("next action = %s, want %s (the stored reminder_days, not the compiled 48h)",
			next.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// The number of reminders comes from `cronjob.max_reminders`.
//
// With the budget at one, the first step is already the last: the invoice is exhausted after a
// single reminder rather than after three. An operator who lowers the budget must see the effect,
// because "the ladder is shorter than I configured" is how a customer gets suspended a fortnight
// early.
func TestMaxRemindersComesFromSettings(t *testing.T) {
	f := newFixture(t, baseTime.AddDate(0, 0, -1))
	setSetting(t, f, settings.KeyMaxReminders, "1")

	results, err := f.engine.Advance(context.Background(), 100)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("%d advances, want 1", len(results))
	}
	if !results[0].Exhausted || results[0].State != dunning.StateExhausted {
		t.Fatalf("first advance = %+v, want exhausted after one reminder", results[0])
	}
}

// `cronjob.dunning_enabled = false` stops the ladder without losing the state.
//
// The invoice is still recorded as overdue — the overdue widget counts it, and an operator's
// decision to stop reminding does not make the debt disappear — but nothing escalates and no
// suspension becomes possible.
func TestDunningSwitchStopsEscalation(t *testing.T) {
	f := newFixture(t, baseTime.AddDate(0, 0, -1))
	setSetting(t, f, settings.KeyDunningEnabled, "false")

	for i := 0; i < 3; i++ {
		results, err := f.engine.Advance(context.Background(), 100)
		if err != nil {
			t.Fatalf("advance: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("advance %d escalated %+v while the ladder was switched off", i, results)
		}
		f.now = f.now.Add(48 * time.Hour)
	}

	state, step := f.state(t)
	if state != "overdue" || step != 0 {
		t.Fatalf("state = %s step = %d, want overdue/0: the switch must not escalate", state, step)
	}

	// A switched-off ladder also permits no suspension, whatever the invoice's age.
	f.now = baseTime.AddDate(0, 0, 90)
	if _, err := f.engine.Evaluate(context.Background(), f.serviceID); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	elig, err := f.engine.Evaluate(context.Background(), f.serviceID)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if elig.Eligible {
		t.Fatal("a service was eligible for suspension while the dunning ladder was switched off")
	}
}

// The suspension period comes from `cronjob.suspend_days`.
//
// The invoice in this test is 30 days overdue and the dunning state is exhausted, so the only
// thing standing between the service and a suspension is the stored number: with 40 days it is
// too early, with 7 it is permitted.
func TestSuspendAfterComesFromSettings(t *testing.T) {
	f := newFixture(t, baseTime.AddDate(0, 0, -30))
	setSetting(t, f, settings.KeySuspendDays, "40")

	// Drive the ladder to exhausted on the stored interval (one day, so the test does not have
	// to wait two).
	setSetting(t, f, settings.KeyReminderDays, "1")
	for i := 0; i < 4; i++ {
		if _, err := f.engine.Advance(context.Background(), 100); err != nil {
			t.Fatalf("advance: %v", err)
		}
		f.now = f.now.Add(48 * time.Hour)
	}
	if state, _ := f.state(t); state != "exhausted" {
		t.Fatalf("state = %q, want exhausted before the suspension question", state)
	}

	// 30 days overdue, 40 required: not yet.
	elig, err := f.engine.Evaluate(context.Background(), f.serviceID)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if elig.Eligible {
		t.Fatalf("suspended after 30 days although suspend_days is 40: %s", elig.Reason)
	}

	// The operator lowers it to a week: now the same invoice permits a suspension.
	setSetting(t, f, settings.KeySuspendDays, "7")
	elig, err = f.engine.Evaluate(context.Background(), f.serviceID)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !elig.Eligible {
		t.Fatalf("not eligible with suspend_days = 7 and a 30-day-overdue exhausted invoice: %s",
			elig.Reason)
	}
}

// A zero day count is refused by the settings table rather than read as "act immediately".
//
// Two readers would read `0` in opposite directions — "never" and "now" — and the wrong one
// suspends a paying customer. The value cannot be stored at all; the ladder's switch is the way
// to stop it.
func TestZeroThresholdIsRefused(t *testing.T) {
	f := newFixture(t, baseTime.AddDate(0, 0, -1))
	for _, key := range []string{settings.KeyReminderDays, settings.KeySuspendDays} {
		if _, err := settings.New(f.pool).Set(context.Background(), settings.GroupCronjob, key, "0",
			"", nil); err == nil {
			t.Fatalf("%s = 0 was accepted; a zero day count has two readings and both are wrong", key)
		}
	}
}
