package dunning_test

import (
	"context"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/dunning"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
)

// reminderFixture is one customer with (optionally) a verified login and one open invoice.
type reminderFixture struct {
	pool       *store.Pool
	customerID string
	invoiceID  string
	dueAt      time.Time
}

func newReminderFixture(t *testing.T, dueAt time.Time, verified bool, totalMinor int64) *reminderFixture {
	t.Helper()
	pool := storetest.DB(t)
	ctx := context.Background()
	f := &reminderFixture{pool: pool, dueAt: dueAt}
	if err := pool.QueryRow(ctx, `
		INSERT INTO customers (email, name) VALUES ('remind@example.test', 'Remind Me')
		RETURNING id`).Scan(&f.customerID); err != nil {
		t.Fatalf("customer: %v", err)
	}
	if verified {
		var userID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO users (email, password_hash, email_verified_at)
			VALUES ('remind@example.test', 'x', now()) RETURNING id`).Scan(&userID); err != nil {
			t.Fatalf("user: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO customer_accounts (user_id, customer_id) VALUES ($1, $2)`,
			userID, f.customerID); err != nil {
			t.Fatalf("link account: %v", err)
		}
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO invoices (customer_id, number, status, currency, subtotal_minor, tax_minor,
			total_minor, issued_at, due_at, snapshot)
		VALUES ($1, 'INV-REMIND-1', 'open', 'USD', $2, 0, $2, now(), $3,
		        '{"schema_version":1}'::jsonb)
		RETURNING id`, f.customerID, totalMinor, dueAt).Scan(&f.invoiceID); err != nil {
		t.Fatalf("invoice: %v", err)
	}
	return f
}

// setSetting stores one invoice setting.
func (f *reminderFixture) setSetting(t *testing.T, key, value string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO settings (group_key, key, value) VALUES ('invoices', $1, $2)
		ON CONFLICT (group_key, key) DO UPDATE SET value = EXCLUDED.value`, key, value); err != nil {
		t.Fatalf("set %s: %v", key, err)
	}
}

// reminders reads the reminder rows this fixture's invoice produced.
func (f *reminderFixture) reminders(t *testing.T) []struct {
	Subject string
	Status  string
} {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT subject, status FROM notification_deliveries
		WHERE kind = 'dunning_reminder'
		ORDER BY created_at, id`)
	if err != nil {
		t.Fatalf("read reminders: %v", err)
	}
	defer rows.Close()
	var out []struct {
		Subject string
		Status  string
	}
	for rows.Next() {
		var r struct {
			Subject string
			Status  string
		}
		if err := rows.Scan(&r.Subject, &r.Status); err != nil {
			t.Fatalf("scan reminder: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// A customer is reminded before the money is late, not only after. That is the pre-due half of
// INV-16 and it did not exist at all: the engine escalated silently.
func TestPreDueReminderIsQueuedOnce(t *testing.T) {
	now := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)
	f := newReminderFixture(t, now.AddDate(0, 0, 2), true, 1200)
	engine := dunning.NewEngine(f.pool, dunning.DefaultPolicy())
	engine.Now = func() time.Time { return now }

	ctx := context.Background()
	if _, err := engine.Advance(ctx, 100); err != nil {
		t.Fatalf("advance: %v", err)
	}
	got := f.reminders(t)
	if len(got) != 1 {
		t.Fatalf("reminders = %+v, want one pre-due reminder", got)
	}
	if got[0].Status != "queued" {
		t.Fatalf("status = %q, want queued for a verified address", got[0].Status)
	}

	// A second run of the job, minutes later, is the normal case. It must not send twice: the
	// dedupe key is what makes a frequent scan safe.
	if _, err := engine.Advance(ctx, 100); err != nil {
		t.Fatalf("second advance: %v", err)
	}
	if got := f.reminders(t); len(got) != 1 {
		t.Fatalf("a re-run produced %d reminders, want the one already queued", len(got))
	}
}

// The window is the setting, not a constant (INV-16). With "remind 1 day before", an invoice
// due in two days is not reminded yet.
func TestPreDueWindowComesFromSettings(t *testing.T) {
	now := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)
	f := newReminderFixture(t, now.AddDate(0, 0, 2), true, 1200)
	f.setSetting(t, "reminder_days_before", "1")

	engine := dunning.NewEngine(f.pool, dunning.DefaultPolicy())
	engine.Now = func() time.Time { return now }
	ctx := context.Background()
	if _, err := engine.Advance(ctx, 100); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got := f.reminders(t); len(got) != 0 {
		t.Fatalf("reminders = %+v, want none until one day before the due date", got)
	}

	// A day later it is inside the window.
	engine.Now = func() time.Time { return now.AddDate(0, 0, 1) }
	if _, err := engine.Advance(ctx, 100); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got := f.reminders(t); len(got) != 1 {
		t.Fatalf("reminders = %+v, want the pre-due reminder inside the window", got)
	}
}

// "X days overdue" delays the first overdue mail. The escalation state machine is not moved by
// it: suspension timing is its own policy.
func TestOverdueReminderWaitsForTheConfiguredDays(t *testing.T) {
	due := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)
	f := newReminderFixture(t, due, true, 1200)
	f.setSetting(t, "reminder_days_before", "0")
	f.setSetting(t, "reminder_days_overdue", "3")

	now := due.Add(24 * time.Hour) // one day overdue
	engine := dunning.NewEngine(f.pool, dunning.DefaultPolicy())
	engine.Now = func() time.Time { return now }
	ctx := context.Background()
	if _, err := engine.Advance(ctx, 100); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got := f.reminders(t); len(got) != 0 {
		t.Fatalf("reminders = %+v, want none before three days overdue", got)
	}

	now = due.AddDate(0, 0, 4)
	engine.Now = func() time.Time { return now }
	if _, err := engine.Advance(ctx, 100); err != nil {
		t.Fatalf("advance: %v", err)
	}
	got := f.reminders(t)
	if len(got) != 1 {
		t.Fatalf("reminders = %+v, want one once three days overdue", got)
	}
	if want := "Invoice INV-REMIND-1 is overdue"; got[0].Subject != want {
		t.Fatalf("subject = %q, want %q", got[0].Subject, want)
	}
}

// A customer with no verified address is recorded as suppressed, not silently skipped: "why
// did nobody tell me" has to have an answer.
func TestReminderToAnUnverifiedAddressIsRecordedAsSuppressed(t *testing.T) {
	now := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)
	f := newReminderFixture(t, now.AddDate(0, 0, 1), false, 1200)
	engine := dunning.NewEngine(f.pool, dunning.DefaultPolicy())
	engine.Now = func() time.Time { return now }

	if _, err := engine.Advance(context.Background(), 100); err != nil {
		t.Fatalf("advance: %v", err)
	}
	got := f.reminders(t)
	if len(got) != 1 {
		t.Fatalf("reminders = %+v, want one suppressed row", got)
	}
	if got[0].Status != "suppressed" {
		t.Fatalf("status = %q, want suppressed", got[0].Status)
	}
}

// The reminder states the amount that is left, not the invoice total: a partly paid invoice is
// reminded for the balance, which is the number the customer has to act on.
func TestReminderStatesTheOutstandingAmount(t *testing.T) {
	now := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)
	f := newReminderFixture(t, now.AddDate(0, 0, 1), true, 1200)
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE invoices SET amount_paid_minor = 500 WHERE id = $1`, f.invoiceID); err != nil {
		t.Fatalf("part-pay: %v", err)
	}
	engine := dunning.NewEngine(f.pool, dunning.DefaultPolicy())
	engine.Now = func() time.Time { return now }
	if _, err := engine.Advance(context.Background(), 100); err != nil {
		t.Fatalf("advance: %v", err)
	}

	var body string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT body FROM notification_deliveries WHERE kind = 'dunning_reminder'`).Scan(&body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !contains(body, "7.00 USD") {
		t.Fatalf("body = %q, want the outstanding 7.00 USD", body)
	}
}
