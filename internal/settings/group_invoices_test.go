package settings_test

import (
	"context"
	"testing"

	"github.com/catalyst/billing/internal/settings"
	"github.com/catalyst/billing/internal/store/storetest"
)

// The invoice helpers exist so every path — manual create, checkout, reminders — reads the
// same fallback. If `due_days` stopped being read here, the manual invoice would go back to
// the hard-coded 30 days INV-17 records.
func TestInvoiceDueDaysDefaultsAndOverrides(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	if got := settings.InvoiceDueDays(ctx, pool); got != 7 {
		t.Fatalf("default due days = %d, want 7", got)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO settings (group_key, key, value) VALUES ('invoices', 'due_days', '3')
		ON CONFLICT (group_key, key) DO UPDATE SET value = '3'`); err != nil {
		t.Fatalf("set due days: %v", err)
	}
	if got := settings.InvoiceDueDays(ctx, pool); got != 3 {
		t.Fatalf("due days = %d, want 3", got)
	}
	// A malformed value falls back rather than refusing to issue: a typo in a settings field
	// must not take the revenue path down.
	if _, err := pool.Exec(ctx, `
		INSERT INTO settings (group_key, key, value) VALUES ('invoices', 'due_days', 'soon')
		ON CONFLICT (group_key, key) DO UPDATE SET value = 'soon'`); err != nil {
		t.Fatalf("set malformed due days: %v", err)
	}
	if got := settings.InvoiceDueDays(ctx, pool); got != 7 {
		t.Fatalf("malformed due days = %d, want the default 7", got)
	}
}

// The immutability switch is off until an operator turns it on, and then it is on (INV-05).
func TestInvoiceImmutableReadsTheSetting(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	if settings.InvoiceImmutable(ctx, pool) {
		t.Fatal("issued invoices are immutable by default; that would refuse an existing behaviour")
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO settings (group_key, key, value) VALUES ('invoices', 'immutable', 'true')
		ON CONFLICT (group_key, key) DO UPDATE SET value = 'true'`); err != nil {
		t.Fatalf("set immutable: %v", err)
	}
	if !settings.InvoiceImmutable(ctx, pool) {
		t.Fatal("the immutable setting was written and not read")
	}
	// An unparseable value keeps the safe default rather than the permissive one.
	if _, err := pool.Exec(ctx, `
		UPDATE settings SET value = 'perhaps' WHERE group_key = 'invoices' AND key = 'immutable'`); err != nil {
		t.Fatalf("set malformed immutable: %v", err)
	}
	if settings.InvoiceImmutable(ctx, pool) {
		t.Fatal("a malformed value was read as on")
	}
}

// The reminder cadence is four independent values (INV-16), and each falls back on its own so
// setting only the pre-due day count does not zero the rest.
func TestInvoiceReminderScheduleFallsBackPerKey(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()

	got := settings.InvoiceReminderSchedule(ctx, pool)
	if got.DaysBeforeDue != 3 || got.DaysOverdue != 1 || got.IntervalDays != 2 || got.MaxSteps != 3 {
		t.Fatalf("default schedule = %+v", got)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO settings (group_key, key, value) VALUES ('invoices', 'reminder_days_before', '5')
		ON CONFLICT (group_key, key) DO UPDATE SET value = '5'`); err != nil {
		t.Fatalf("set reminder days: %v", err)
	}
	got = settings.InvoiceReminderSchedule(ctx, pool)
	if got.DaysBeforeDue != 5 {
		t.Fatalf("days before = %d, want 5", got.DaysBeforeDue)
	}
	if got.DaysOverdue != 1 || got.IntervalDays != 2 || got.MaxSteps != 3 {
		t.Fatalf("an unrelated key changed the rest of the schedule: %+v", got)
	}

	// A negative day count is refused at the settings edge rather than scheduling a reminder
	// after the due date under a pre-due label.
	def, ok := settings.Lookup("invoices", "reminder_days_before")
	if !ok {
		t.Fatal("reminder_days_before is not declared")
	}
	if err := def.Validate("-1"); err == nil {
		t.Fatal("a negative reminder day count was accepted")
	}
	if err := def.Validate("3"); err != nil {
		t.Fatalf("a valid day count was refused: %v", err)
	}
}

// Email-on-issue defaults on and is overridable (INV-14).
func TestEmailOnIssueDefaultsOn(t *testing.T) {
	pool := storetest.DB(t)
	ctx := context.Background()
	if !settings.EmailOnIssue(ctx, pool) {
		t.Fatal("issuing a document does not mail the customer by default")
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO settings (group_key, key, value) VALUES ('invoices', 'email_on_issue', 'false')
		ON CONFLICT (group_key, key) DO UPDATE SET value = 'false'`); err != nil {
		t.Fatalf("set email_on_issue: %v", err)
	}
	if settings.EmailOnIssue(ctx, pool) {
		t.Fatal("the email_on_issue setting was written and not read")
	}
}

// The new keys are declared, so the settings screen renders them and a write is validated
// rather than refused as an unknown key.
func TestInvoiceGroupIsDeclared(t *testing.T) {
	for _, key := range []string{
		"immutable", "debit_note_prefix", "debit_note_padding",
		"reminder_days_before", "reminder_days_overdue", "reminder_interval_days",
		"reminder_max_steps", "email_on_issue", "pdf_template",
	} {
		if _, ok := settings.Lookup("invoices", key); !ok {
			t.Errorf("invoices.%s is not declared", key)
		}
	}
}
