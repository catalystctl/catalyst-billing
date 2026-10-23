package settings

// The invoice settings this round of parity work needs (INV-05, INV-10, INV-14, INV-16,
// INV-17, INV-18, INV-15).
//
// They live in their own file because the base table in settings.go is a merge hotspot: a
// group declared there is an edit to a region every other feature also edits, and an
// adjacency conflict in a settings table is resolved by hand, which is how a key quietly
// disappears. `invoices.due_days`, `invoice_number_prefix`, `invoice_number_padding`,
// `credit_note_prefix`, `credit_note_padding`, `proforma`, `bill_to_text` and
// `notes_client_visible` already exist in the base table and are not redeclared here.
//
// The read helpers at the bottom exist so a caller with a transaction (the manual invoice
// handler, the checkout path, the reminder job) reads the same fallback the settings screen
// shows. A second place that parses "7" out of a text column is a second place that gets the
// default wrong.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

func init() {
	Register(
		// INV-05: whether an issued document may still receive the soft edits that the
		// database trigger deliberately leaves writable (a client note, an extended due
		// date).
		//
		// Off by default, because "optional immutable invoices" is the feature: the trigger
		// already freezes every money fact of an issued document, and an operator adding a
		// note to a document a customer already has is an existing, useful behaviour that
		// must not stop working because a setting shipped. Turning it on is the deliberate
		// act, and it then refuses those edits with a stable error code.
		Definition{Group: "invoices", Key: "immutable", Kind: KindBool, Default: "false"},

		// INV-10/INV-18: the debit-note series. Separate from the credit-note series for the
		// same reason the credit series is separate from the invoice series: a debtor and a
		// creditor number are quoted in different places, and interleaving them makes both
		// harder to audit.
		Definition{Group: "invoices", Key: "debit_note_prefix", Kind: KindString, Default: "DN-"},
		Definition{Group: "invoices", Key: "debit_note_padding", Kind: KindInt, Default: "6"},

		// INV-16: the reminder schedule. Paymenter's two knobs — "X days before due" and "X
		// days overdue" — plus the interval and the step budget the engine already had, so
		// an operator can change the cadence without a deploy.
		//
		// Zero is a legitimate value for the two day counts: "0 days before" means no
		// pre-due reminder, which is a supported choice rather than a missing one.
		Definition{Group: "invoices", Key: "reminder_days_before", Kind: KindInt, Default: "3",
			Check: checkDayCount},
		Definition{Group: "invoices", Key: "reminder_days_overdue", Kind: KindInt, Default: "1",
			Check: checkDayCount},
		Definition{Group: "invoices", Key: "reminder_interval_days", Kind: KindInt, Default: "2"},
		Definition{Group: "invoices", Key: "reminder_max_steps", Kind: KindInt, Default: "3"},

		// INV-14/ADM-31: whether issuing a document mails the customer by default. The
		// per-request toggle overrides it in both directions; this is the default the "send
		// the invoice" checkbox starts from.
		Definition{Group: "invoices", Key: "email_on_issue", Kind: KindBool, Default: "true"},

		// INV-15: the theme's PDF presentation. JSON rather than a table because it is a
		// handful of strings and a theme ships it as a file; the shape is
		// invoicepdf.Template, which refuses anything it cannot render.
		Definition{Group: "invoices", Key: "pdf_template", Kind: KindJSON, Default: "{}"},
	)
}

// checkDayCount refuses a negative day count and an absurd one.
//
// A negative "days before" would schedule a reminder after the due date while claiming it is
// a pre-due notice, which is a support ticket waiting to happen; a count above a year is not
// a schedule, it is a typo.
func checkDayCount(d Definition, value string) error {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("%w: %s must be an integer", ErrInvalidValue, d.Name())
	}
	if n < 0 || n > 365 {
		return fmt.Errorf("%w: %s must be between 0 and 365 days", ErrInvalidValue, d.Name())
	}
	return nil
}

// RowQuerier is the read surface a settings helper needs.
//
// It is separate from the internal `querier` because these helpers run one row at a time,
// including from inside a transaction that is already open. It is exported so a package that
// holds a pgx.Tx (the checkout path, the reminder job) can call the same helper the panel
// does, instead of re-implementing the fallback.
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// InvoiceDueDays is the payment term in days (INV-17).
//
// Paymenter's default is 7 days. Every caller that used to hard-code 30 now comes through
// here, so "due in a week" is one number in one place.
func InvoiceDueDays(ctx context.Context, q RowQuerier) int {
	return intOr(ctx, q, "invoices", "due_days", 7, 1, 3650)
}

// InvoiceImmutable reports whether issued documents are frozen against soft edits (INV-05).
//
// A missing row means "not frozen", which matches the behaviour before the setting existed.
// An operator who wants the stricter rule turns it on and gets a stable refusal code.
func InvoiceImmutable(ctx context.Context, q RowQuerier) bool {
	def, ok := Lookup("invoices", "immutable")
	if !ok {
		return false
	}
	want, err := strconv.ParseBool(def.Default)
	if err != nil {
		return false
	}
	raw, ok := storedValue(ctx, q, "invoices", "immutable")
	if !ok {
		return want
	}
	got, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return want
	}
	return got
}

// EmailOnIssue reports whether issuing a document mails the customer by default.
func EmailOnIssue(ctx context.Context, q RowQuerier) bool {
	def, ok := Lookup("invoices", "email_on_issue")
	if !ok {
		return true
	}
	want, err := strconv.ParseBool(def.Default)
	if err != nil {
		return true
	}
	raw, ok := storedValue(ctx, q, "invoices", "email_on_issue")
	if !ok {
		return want
	}
	got, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return want
	}
	return got
}

// ReminderSchedule is the dunning/reminder cadence (INV-16).
type ReminderSchedule struct {
	// DaysBeforeDue is how many days ahead of the due date a reminder is sent. Zero disables
	// the pre-due reminder.
	DaysBeforeDue int
	// DaysOverdue is when the first overdue reminder is sent, measured from the due date.
	DaysOverdue int
	// IntervalDays is the gap between subsequent overdue reminders.
	IntervalDays int
	// MaxSteps is how many overdue reminders run before the invoice is exhausted.
	MaxSteps int
}

// InvoiceReminderSchedule reads the reminder cadence.
//
// Each value falls back independently, so an operator who sets only the pre-due day count
// keeps the seeded cadence for everything else instead of silently getting zeros.
func InvoiceReminderSchedule(ctx context.Context, q RowQuerier) ReminderSchedule {
	return ReminderSchedule{
		DaysBeforeDue: intOr(ctx, q, "invoices", "reminder_days_before", 3, 0, 365),
		DaysOverdue:   intOr(ctx, q, "invoices", "reminder_days_overdue", 1, 0, 365),
		IntervalDays:  intOr(ctx, q, "invoices", "reminder_interval_days", 2, 1, 365),
		MaxSteps:      intOr(ctx, q, "invoices", "reminder_max_steps", 3, 1, 20),
	}
}

// intOr reads one integer setting and clamps it, falling back to the code-side default for
// the key when the row is absent, unparseable or out of range.
//
// It reads the default out of the declaration rather than taking a second copy as an
// argument: two copies of "7" is how the panel and the checkout path end up disagreeing.
func intOr(ctx context.Context, q RowQuerier, group, key string, fallback, min, max int) int {
	def, ok := Lookup(group, key)
	if ok {
		if n, err := strconv.Atoi(strings.TrimSpace(def.Default)); err == nil {
			fallback = n
		}
	}
	raw, ok := storedValue(ctx, q, group, key)
	if !ok {
		return fallback
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < min || n > max {
		return fallback
	}
	return n
}

// storedValue reads one stored row, reporting whether it exists.
func storedValue(ctx context.Context, q RowQuerier, group, key string) (string, bool) {
	var raw string
	if q == nil {
		return "", false
	}
	err := q.QueryRow(ctx,
		`SELECT value FROM settings WHERE group_key = $1 AND key = $2`, group, key).Scan(&raw)
	if err != nil {
		return "", false
	}
	return raw, true
}
