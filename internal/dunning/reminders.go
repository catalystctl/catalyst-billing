package dunning

// Reminders (INV-16).
//
// Dunning already escalated an overdue invoice — it advanced a step counter and made the
// service suspension-eligible — but it never told the customer anything. Paymenter's reminder
// feature is the part a customer experiences: a mail *before* the due date, and mails at a
// configured distance *after* it. The gap the parity inventory records is exactly that:
// "only reminds after the due date ... with no pre-due reminder and no settings key for the
// schedule".
//
// Three decisions shape the implementation:
//
//  1. **The schedule is settings, read on every run.** `invoices.reminder_days_before`,
//     `reminder_days_overdue`, `reminder_interval_days` and `reminder_max_steps` are read by
//     the engine rather than baked into `Policy`, so an operator can change the cadence
//     without a deploy and the cron entry that calls `Advance` does not have to be told about
//     it. `Policy` remains the fallback for a caller that supplies its own cadence (the
//     tests do).
//  2. **A reminder is a document, so it is idempotent by construction.** The outbox's
//     `dedupe_key` is unique, and every reminder derives its key from the invoice and the
//     step. A retried job, a second worker and a replayed scan all find the existing row
//     instead of sending a second mail — the same property that makes invoice mail safe.
//  3. **"We did not send it" is recorded.** A customer with no verified address gets a
//     suppressed row, not silence: the answer to "why did nobody tell me" is then a row with
//     a reason on it rather than a gap in the log.
//
// What a reminder may not do: it may not itself suspend, void or change an invoice. It is a
// message about a fact, and the fact is read in the same transaction that queues the message.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/settings"
	"github.com/jackc/pgx/v5"
)

// Reminder kinds, used in dedupe keys and in the audit trail a support agent reads.
const (
	reminderDueSoon = "due_soon"
	reminderOverdue = "overdue"
)

// schedule is the cadence for one run.
//
// Settings win when the engine was constructed with the default policy — that is the
// production entry point, and the whole point of the knobs. A caller that supplied its own
// policy (a test, a replay tool, an operator running a one-off with a tighter cadence) keeps
// what it asked for, because silently overriding an explicit argument is worse than not
// having the setting.
func (e *Engine) schedule(ctx context.Context) settings.ReminderSchedule {
	out := settings.ReminderSchedule{
		DaysOverdue:  int(e.policy.ReminderInterval.Hours() / 24),
		IntervalDays: int(e.policy.ReminderInterval.Hours() / 24),
		MaxSteps:     e.policy.MaxSteps,
	}
	if e.pool == nil {
		return out
	}
	if e.policy != DefaultPolicy() {
		// Still honour the pre-due reminder: Policy has no field for it, so there is nothing
		// a caller could have meant instead.
		out.DaysBeforeDue = settings.InvoiceReminderSchedule(ctx, e.pool).DaysBeforeDue
		return out
	}
	read := settings.InvoiceReminderSchedule(ctx, e.pool)
	// DaysBeforeDue has no Policy equivalent; zero means "no pre-due reminder", which is a
	// supported configuration rather than a missing value, so it is taken as read.
	out.DaysBeforeDue = read.DaysBeforeDue
	if read.DaysOverdue > 0 {
		out.DaysOverdue = read.DaysOverdue
	}
	if read.IntervalDays > 0 {
		out.IntervalDays = read.IntervalDays
	}
	if read.MaxSteps > 0 {
		out.MaxSteps = read.MaxSteps
	}
	return out
}

// remindDueSoon queues the pre-due reminder for every open invoice whose due date is within
// the configured window.
//
// "Within the window" is inclusive of the boundary and excludes the due date itself: a
// reminder that arrives on the day payment is due is an overdue notice wearing the wrong
// subject line. The unique dedupe key per invoice means the scan can run as often as the job
// likes.
func (e *Engine) remindDueSoon(ctx context.Context, now time.Time, sch settings.ReminderSchedule) (int, error) {
	if sch.DaysBeforeDue <= 0 {
		return 0, nil
	}
	rows, err := e.pool.Query(ctx, `
		SELECT i.id, COALESCE(i.number, ''), i.total_minor, i.amount_paid_minor, i.currency,
		       i.due_at, c.email,
		       EXISTS (
		           SELECT 1 FROM customer_accounts ca
		           JOIN users u ON u.id = ca.user_id
		           WHERE ca.customer_id = c.id AND u.email_verified_at IS NOT NULL)
		FROM invoices i
		JOIN customers c ON c.id = i.customer_id
		WHERE i.status = 'open'
		  AND i.due_at IS NOT NULL
		  AND i.due_at > $1
		  AND i.due_at <= $1 + make_interval(days => $2)
		ORDER BY i.due_at
		LIMIT 500`, now, sch.DaysBeforeDue)
	if err != nil {
		return 0, fmt.Errorf("dunning: scan due-soon invoices: %w", err)
	}
	type candidate struct {
		id, number, currency, email string
		total, paid                 int64
		dueAt                       time.Time
		verified                    bool
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.number, &c.total, &c.paid, &c.currency,
			&c.dueAt, &c.email, &c.verified); err != nil {
			rows.Close()
			return 0, fmt.Errorf("dunning: scan due-soon invoice: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("dunning: scan due-soon invoices: %w", err)
	}
	rows.Close()

	sent := 0
	for _, c := range candidates {
		created, err := e.queueReminder(ctx, reminderInput{
			InvoiceID: c.id, Number: c.number, Currency: c.currency,
			TotalMinor: c.total, PaidMinor: c.paid, DueAt: c.dueAt,
			Email: c.email, Verified: c.verified,
			Kind: reminderDueSoon, Step: 0,
		})
		if err != nil {
			return sent, err
		}
		if created {
			sent++
		}
	}
	return sent, nil
}

// reminderInput is one reminder to queue.
type reminderInput struct {
	InvoiceID  string
	Number     string
	Currency   string
	TotalMinor int64
	PaidMinor  int64
	DueAt      time.Time
	Email      string
	Verified   bool
	Kind       string
	Step       int
}

// queueReminder writes one reminder into the outbox inside its own transaction.
func (e *Engine) queueReminder(ctx context.Context, in reminderInput) (bool, error) {
	message, err := reminderMessage(in)
	if err != nil {
		return false, err
	}
	var created bool
	err = e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		queue := notify.NewQueue(e.pool)
		if !in.Verified {
			// A suppressed row, not silence: "not sent because there is no verified address"
			// is an answer a support agent can give, and a gap in the log is not.
			return queue.Suppress(ctx, tx, message, "the customer has no verified address")
		}
		var qerr error
		_, created, qerr = queue.Enqueue(ctx, tx, message)
		return qerr
	})
	if err != nil {
		return false, fmt.Errorf("dunning: queue reminder: %w", err)
	}
	return created, nil
}

// reminderMessage renders the mail for one reminder.
//
// The body is plain text with the facts a customer needs to act: which document, how much is
// left, and when it is or was due. It deliberately does not threaten suspension — dunning's
// escalation is the operator's policy, and a mail that promises a consequence the product may
// not apply is a promise the product then breaks.
func reminderMessage(in reminderInput) (notify.Message, error) {
	cur, ok := money.LookupCurrency(in.Currency)
	if !ok {
		var err error
		cur, err = money.NewCurrency(in.Currency, 2)
		if err != nil {
			return notify.Message{}, fmt.Errorf("dunning: reminder currency %q: %w", in.Currency, err)
		}
	}
	outstanding := in.TotalMinor - in.PaidMinor
	if outstanding < 0 {
		outstanding = 0
	}
	amount := money.New(outstanding, cur).Format()
	due := in.DueAt.UTC().Format("2006-01-02")
	number := in.Number
	if number == "" {
		number = "your invoice"
	}

	msg := notify.Message{
		Kind: notify.KindDunningReminder,
		To:   in.Email,
		// The dedupe key is what makes a retried job harmless: one reminder per invoice per
		// kind per step, however often the scan runs.
		DedupeKey: fmt.Sprintf("invoice_reminder:%s:%s:%d", in.InvoiceID, in.Kind, in.Step),
	}
	switch in.Kind {
	case reminderDueSoon:
		msg.Subject = "Invoice " + number + " is due on " + due
		msg.Body = strings.Join([]string{
			"Invoice " + number + " is due on " + due + ".",
			"",
			"Amount due: " + amount,
			"",
			"Invoice reference: " + in.InvoiceID,
		}, "\n")
	case reminderOverdue:
		msg.Subject = "Invoice " + number + " is overdue"
		msg.Body = strings.Join([]string{
			"Invoice " + number + " was due on " + due + " and has not been paid.",
			"",
			"Amount due: " + amount,
			"",
			"Paying it now stops further reminders.",
			"",
			"Invoice reference: " + in.InvoiceID,
		}, "\n")
	default:
		return notify.Message{}, fmt.Errorf("dunning: unknown reminder kind %q", in.Kind)
	}
	return msg, nil
}
