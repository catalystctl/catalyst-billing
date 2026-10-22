package notify

// Editable templates, one per event.
//
// The outbox in notify.go answers "was it sent, and if not why"; this file answers "what
// did it say". Those were hardcoded Go string literals, which meant changing the wording
// of a payment-failed notice was a deploy. A template row moves the wording to the panel
// without moving the delivery guarantee.
//
// # Plain text only, and not as a style preference
//
// A template renders attacker-influenced data: an invoice line description, a ticket
// subject, a customer's own display name. If the render step were to interpret markup,
// then whoever can type into an invoice line could put a link in the body of a mail that
// the billing panel sends from its own domain, over its own DKIM signature, to its own
// customer. That is not a template feature; it is a phishing kit with a billing UI.
//
// So there is no HTML here and no escaping either. Escaping would imply the output is
// parsed as markup; the output is text/plain and is sent as such by SMTPSender. What this
// file does defend against is the one place where "plain text" is not enough: a value
// substituted into the *subject* lands in an RFC 5322 header, where a newline is how one
// message becomes two. Subject values are therefore stripped of control characters.
//
// # Substitution is a single pass
//
// Render scans the template once and never re-examines what it wrote. A value containing
// "{{link}}" therefore renders literally instead of being expanded, so a customer who can
// type into a field cannot make a template pull in another variable's value.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the database surface this package needs.
//
// Both *store.Pool and pgx.Tx satisfy it, which is the point: every write here is called
// from inside the transaction that made the domain change, and the same functions are
// called from read paths with a pool. Taking a pgx.Tx specifically would force every
// caller to invent a transaction it does not need; taking *store.Pool would make the
// transactional case impossible.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ErrTemplateNotFound is returned when an event has no template row.
//
// It is an error rather than an empty render on purpose. An event whose template was
// deleted must fail where it happens — inside the transaction that was about to notify
// the customer — rather than send a blank email that nobody can explain later.
var ErrTemplateNotFound = errors.New("notify: no template for event")

// Template is one editable message, keyed by event.
type Template struct {
	EventKey string
	Name     string
	// Subject and Body are the mail channel, as plain text with {{variables}}.
	Subject string
	Body    string
	// InAppTitle, InAppBody and InAppURL are the inbox channel. They are separate from the
	// mail body because an inbox row is a line in a list with a link, not an email: the
	// same words would be both too long to scan and too short to be useful.
	InAppTitle string
	InAppBody  string
	InAppURL   string
	// Enabled is the master switch. A disabled template sends nothing on any channel, which
	// is how an operator silences an event without deleting its wording.
	Enabled      bool
	MailEnabled  bool
	InAppEnabled bool
	// CC and BCC are comma-separated address lists, as stored. Use CCList and BCCList to
	// read them.
	CC  string
	BCC string

	UpdatedAt time.Time
}

// CCList splits the stored CC field into addresses.
func (t Template) CCList() []string { return splitAddresses(t.CC) }

// BCCList splits the stored BCC field into addresses.
func (t Template) BCCList() []string { return splitAddresses(t.BCC) }

func splitAddresses(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Rendered is a template with its variables substituted.
type Rendered struct {
	EventKey   string
	Subject    string
	Body       string
	InAppTitle string
	InAppBody  string
	InAppURL   string
	CC         []string
	BCC        []string
}

// templateColumns is the one place the column list is written, so the scan order and the
// INSERT/SELECT cannot drift apart.
const templateColumns = `event_key, name, subject, body, in_app_title, in_app_body,
	in_app_url, enabled, mail_enabled, in_app_enabled, cc_addresses, bcc_addresses, updated_at`

func scanTemplate(row pgx.Row) (Template, error) {
	var t Template
	err := row.Scan(&t.EventKey, &t.Name, &t.Subject, &t.Body, &t.InAppTitle, &t.InAppBody,
		&t.InAppURL, &t.Enabled, &t.MailEnabled, &t.InAppEnabled, &t.CC, &t.BCC, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Template{}, ErrTemplateNotFound
	}
	if err != nil {
		return Template{}, fmt.Errorf("notify: read template: %w", err)
	}
	return t, nil
}

// TemplateFor loads one event's template.
//
// A row that does not exist is not an error when this build ships a default for the event: it
// means the installation has not been seeded yet, and the shipped wording is exactly what a
// fresh installation would send. There is no delete path for a template — the staff screen can
// reword one or reset it to the default, not remove it — so "no row" cannot mean "an operator
// took this away". Falling back rather than failing is what keeps the credential flows working
// in a database that has been migrated but not yet seeded, and a missing template must never be
// the reason a customer cannot reset a password.
//
// An event key this build knows nothing about is still ErrTemplateNotFound. A template for an
// event that does not exist looks like a working feature and is a permanently silent one, so it
// must be visible.
func TemplateFor(ctx context.Context, q Querier, eventKey string) (Template, error) {
	t, err := scanTemplate(q.QueryRow(ctx,
		`SELECT `+templateColumns+` FROM notification_templates WHERE event_key = $1`, eventKey))
	if errors.Is(err, ErrTemplateNotFound) {
		if def, ok := DefaultTemplateFor(eventKey); ok {
			return def, nil
		}
		return Template{}, fmt.Errorf("%w: %s", ErrTemplateNotFound, eventKey)
	}
	return t, err
}

// DefaultTemplateFor returns the shipped default for one event, and whether this build has one.
//
// It is exported because the staff template screen needs the same answer for its "reset to the
// shipped wording" action, and two copies of "which event has which default" is one copy too
// many.
func DefaultTemplateFor(eventKey string) (Template, bool) {
	for _, t := range DefaultTemplates() {
		if t.EventKey == eventKey {
			return t, true
		}
	}
	return Template{}, false
}

// Templates lists every template, ordered by event key so the staff screen is stable.
func Templates(ctx context.Context, q Querier) ([]Template, error) {
	rows, err := q.Query(ctx,
		`SELECT `+templateColumns+` FROM notification_templates ORDER BY event_key`)
	if err != nil {
		return nil, fmt.Errorf("notify: list templates: %w", err)
	}
	defer rows.Close()

	out := make([]Template, 0, len(DefaultTemplates()))
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notify: read templates: %w", err)
	}
	return out, nil
}

// SaveTemplate creates or replaces one event's template.
//
// It is an upsert keyed by event_key rather than by id because the event key *is* the
// identity of a template: "the invoice-paid notice" is a singleton, and a table that could
// hold two of them would only ever be a bug waiting to happen.
func SaveTemplate(ctx context.Context, q Querier, t Template) (Template, error) {
	if strings.TrimSpace(t.EventKey) == "" {
		return Template{}, errors.New("notify: template has no event key")
	}
	// A placeholder the event does not supply is refused here, at the one place wording is
	// stored, rather than at send time where the customer would read "{{invoice_number}}"
	// as literal text. See template_vars.go for the vocabulary and why it is per event.
	if err := ValidateTemplate(t); err != nil {
		return Template{}, err
	}
	if t.Name == "" {
		t.Name = t.EventKey
	}
	return scanTemplate(q.QueryRow(ctx, `
		INSERT INTO notification_templates
			(event_key, name, subject, body, in_app_title, in_app_body, in_app_url,
			 enabled, mail_enabled, in_app_enabled, cc_addresses, bcc_addresses, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, now())
		ON CONFLICT (event_key) DO UPDATE SET
			name = EXCLUDED.name,
			subject = EXCLUDED.subject,
			body = EXCLUDED.body,
			in_app_title = EXCLUDED.in_app_title,
			in_app_body = EXCLUDED.in_app_body,
			in_app_url = EXCLUDED.in_app_url,
			enabled = EXCLUDED.enabled,
			mail_enabled = EXCLUDED.mail_enabled,
			in_app_enabled = EXCLUDED.in_app_enabled,
			cc_addresses = EXCLUDED.cc_addresses,
			bcc_addresses = EXCLUDED.bcc_addresses,
			updated_at = now()
		RETURNING `+templateColumns,
		t.EventKey, t.Name, t.Subject, t.Body, t.InAppTitle, t.InAppBody, t.InAppURL,
		t.Enabled, t.MailEnabled, t.InAppEnabled, t.CC, t.BCC))
}

// Render substitutes variables into every field of a template.
//
// A placeholder with no matching variable is left exactly as written. That is deliberate:
// replacing it with the empty string produces a sentence with a hole in it that the
// customer sees and the operator never learns about, while leaving "{{invoice_number}}"
// visible makes the missing variable obvious in the first test mail.
func Render(t Template, vars map[string]string) Rendered {
	return Rendered{
		EventKey: t.EventKey,
		// Subject is a header: substituted values lose their control characters.
		Subject:    renderHeader(t.Subject, vars),
		Body:       renderText(t.Body, vars),
		InAppTitle: renderText(t.InAppTitle, vars),
		InAppBody:  renderText(t.InAppBody, vars),
		InAppURL:   strings.TrimSpace(renderText(t.InAppURL, vars)),
		CC:         t.CCList(),
		BCC:        t.BCCList(),
	}
}

// renderHeader substitutes and then removes control characters from what it wrote.
//
// Only the substituted values are stripped; the literal template text is the operator's
// and has already been reviewed. Doing it this way means a value cannot smuggle a CRLF
// into the Subject header even if the operator wrote the placeholder inline.
func renderHeader(tmpl string, vars map[string]string) string {
	return substitute(tmpl, func(key string) string {
		v, ok := vars[key]
		if !ok {
			return "{{" + key + "}}"
		}
		return strings.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || unicode.IsControl(r) {
				return ' '
			}
			return r
		}, v)
	})
}

// renderText substitutes into a body field.
//
// CRLF is normalised to LF so a stored template behaves the same on every platform and
// SMTPSender's own normalisation has one shape to deal with. Nothing else is touched: the
// text is sent as text/plain, so a "<" is a "<".
func renderText(tmpl string, vars map[string]string) string {
	out := substitute(tmpl, func(key string) string {
		if v, ok := vars[key]; ok {
			return v
		}
		return "{{" + key + "}}"
	})
	return strings.ReplaceAll(out, "\r\n", "\n")
}

// substitute walks the template once, calling value for each placeholder it finds.
//
// One pass, and the replacement text is never rescanned. A loop of strings.ReplaceAll
// calls would be both quadratic and, worse, order-dependent: a value that happened to
// contain another placeholder would be expanded, which lets the data choose the template.
func substitute(tmpl string, value func(key string) string) string {
	if !strings.Contains(tmpl, "{{") {
		return tmpl
	}
	var b strings.Builder
	b.Grow(len(tmpl))

	rest := tmpl
	for {
		open := strings.Index(rest, "{{")
		if open < 0 {
			b.WriteString(rest)
			return b.String()
		}
		closeRel := strings.Index(rest[open+2:], "}}")
		if closeRel < 0 {
			// An unclosed "{{" is literal text, not an error: a template is prose, and
			// refusing to render it because of one stray brace would be a worse outcome
			// than rendering the brace.
			b.WriteString(rest)
			return b.String()
		}
		end := open + 2 + closeRel
		b.WriteString(rest[:open])
		key := strings.TrimSpace(rest[open+2 : end])
		if key == "" || strings.ContainsAny(key, "{}") {
			// Not a placeholder after all (e.g. "{{}}" or nested braces).
			b.WriteString(rest[open : end+2])
		} else {
			b.WriteString(value(key))
		}
		rest = rest[end+2:]
	}
}

// SeedTemplates inserts the default template for every event that does not have one.
//
// It is INSERT ... ON CONFLICT DO NOTHING, so it is safe to call on every boot and safe to
// call from two processes at once, and an operator's edited wording is never overwritten by
// a deploy. That is why it is a function and not a migration: seed data that can be
// re-applied belongs in the code that knows the events, and a migration that inserts
// defaults would silently re-clobber edits if it were ever edited and re-run.
//
// A new event added to this package therefore appears in an existing installation on the
// next start, with no schema change.
func SeedTemplates(ctx context.Context, q Querier) (inserted int, err error) {
	defaults := DefaultTemplates()
	for _, t := range defaults {
		tag, err := q.Exec(ctx, `
			INSERT INTO notification_templates
				(event_key, name, subject, body, in_app_title, in_app_body, in_app_url,
				 enabled, mail_enabled, in_app_enabled, cc_addresses, bcc_addresses)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT (event_key) DO NOTHING`,
			t.EventKey, t.Name, t.Subject, t.Body, t.InAppTitle, t.InAppBody, t.InAppURL,
			t.Enabled, t.MailEnabled, t.InAppEnabled, t.CC, t.BCC)
		if err != nil {
			return inserted, fmt.Errorf("notify: seed template %s: %w", t.EventKey, err)
		}
		inserted += int(tag.RowsAffected())
	}
	return inserted, nil
}

// DefaultTemplates is the wording a fresh installation starts with.
//
// It is the Paymenter event list, written for this product. Every default is enabled on
// both channels; the two force events (password reset, email verification) cannot be turned
// off by a user, and the rest can — see preferences.go for where that is decided.
func DefaultTemplates() []Template {
	all := func(eventKey, name, subject, body, inAppTitle, inAppBody, inAppURL string) Template {
		return Template{
			EventKey: eventKey, Name: name, Subject: subject, Body: body,
			InAppTitle: inAppTitle, InAppBody: inAppBody, InAppURL: inAppURL,
			Enabled: true, MailEnabled: true, InAppEnabled: true,
		}
	}
	return []Template{
		all(EventPasswordReset, "Password reset",
			"Reset your password",
			"Someone asked to reset the password for the account {{email}}.\n"+
				"\n"+
				"Open this link to choose a new password:\n"+
				"{{link}}\n"+
				"\n"+
				"The link expires in {{expires_in}} and can be used once.\n"+
				"If you did not ask for this, ignore this message; the password is unchanged.\n"+
				"\n"+
				"{{company_name}}",
			"Reset your password",
			"A password reset was requested for this account.",
			"/reset"),

		all(EventEmailVerification, "Email verification",
			"Confirm your email address",
			"Confirm this address to finish setting up the account {{email}}.\n"+
				"\n"+
				"{{link}}\n"+
				"\n"+
				"The link expires in {{expires_in}} and can be used once.\n"+
				"\n"+
				"{{company_name}}",
			"Confirm your email address",
			"Confirm this address to finish setting up your account.",
			"/account"),

		all(EventNewInvoiceCreated, "New invoice",
			"Invoice {{invoice_number}}",
			"Invoice {{invoice_number}} has been issued to {{customer_name}}.\n"+
				"\n"+
				"Amount due: {{total}}\n"+
				"Due: {{due_at}}\n"+
				"\n"+
				"View and pay it here:\n"+
				"{{invoice_url}}\n"+
				"\n"+
				"{{company_name}}",
			"Invoice {{invoice_number}} is ready",
			"{{total}} due by {{due_at}}.",
			"{{invoice_url}}"),

		all(EventInvoicePaid, "Invoice paid",
			"Payment received for invoice {{invoice_number}}",
			"Thank you. Invoice {{invoice_number}} has been paid in full.\n"+
				"\n"+
				"Amount paid: {{total}}\n"+
				"Received: {{paid_at}}\n"+
				"\n"+
				"{{invoice_url}}\n"+
				"\n"+
				"{{company_name}}",
			"Payment received",
			"Invoice {{invoice_number}} — {{total}}.",
			"{{invoice_url}}"),

		all(EventInvoicePaymentFailed, "Invoice payment failed",
			"We could not take payment for invoice {{invoice_number}}",
			"A payment attempt for invoice {{invoice_number}} did not go through.\n"+
				"\n"+
				"Amount: {{total}}\n"+
				"Attempted: {{attempted_at}}\n"+
				"Reason: {{failure_reason}}\n"+
				"\n"+
				"Retry the payment here:\n"+
				"{{invoice_url}}\n"+
				"\n"+
				"{{company_name}}",
			"Payment failed",
			"We could not take payment for invoice {{invoice_number}}.",
			"{{invoice_url}}"),

		all(EventNewOrderCreated, "New order",
			"Order {{order_number}} received",
			"Thanks for your order {{order_number}}.\n"+
				"\n"+
				"Total: {{total}}\n"+
				"\n"+
				"Track it here:\n"+
				"{{order_url}}\n"+
				"\n"+
				"{{company_name}}",
			"Order {{order_number}} received",
			"{{total}} — we will let you know when it is ready.",
			"{{order_url}}"),

		all(EventNewServerCreated, "Server created",
			"{{service_name}} is ready",
			"{{service_name}} has been set up and is ready to use.\n"+
				"\n"+
				"{{product_instructions}}\n"+
				"\n"+
				"Server: {{server_name}}\n"+
				"\n"+
				"Manage it here:\n"+
				"{{service_url}}\n"+
				"\n"+
				"{{company_name}}",
			"{{service_name}} is ready",
			"Your server {{server_name}} has been set up.",
			"{{service_url}}"),

		all(EventServerSuspended, "Server suspended",
			"{{service_name}} has been suspended",
			"{{service_name}} has been suspended.\n"+
				"\n"+
				"Suspended: {{suspended_at}}\n"+
				"Reason: {{reason}}\n"+
				"\n"+
				"Resolve it here:\n"+
				"{{service_url}}\n"+
				"\n"+
				"{{company_name}}",
			"{{service_name}} suspended",
			"{{reason}}",
			"{{service_url}}"),

		all(EventServerTerminated, "Server terminated",
			"{{service_name}} has been terminated",
			"{{service_name}} has been terminated.\n"+
				"\n"+
				"Terminated: {{terminated_at}}\n"+
				"\n"+
				"If this was not expected, contact us and quote the service reference\n"+
				"in your account.\n"+
				"\n"+
				"{{company_name}}",
			"{{service_name}} terminated",
			"The service was terminated on {{terminated_at}}.",
			"{{service_url}}"),

		all(EventServiceCancellationReceived, "Cancellation received",
			"Cancellation received for {{service_name}}",
			"We have received your cancellation for {{service_name}}.\n"+
				"\n"+
				"It ends: {{cancels_at}}\n"+
				"\n"+
				"Nothing else is needed. If you did not ask for this, reply to this\n"+
				"message immediately.\n"+
				"\n"+
				"{{service_url}}\n"+
				"\n"+
				"{{company_name}}",
			"Cancellation received",
			"{{service_name}} ends on {{cancels_at}}.",
			"{{service_url}}"),

		all(EventNewTicketMessage, "New ticket message",
			"[{{ticket_reference}}] {{ticket_subject}}",
			"{{author}} replied to ticket {{ticket_reference}}.\n"+
				"\n"+
				"{{ticket_excerpt}}\n"+
				"\n"+
				"Read and reply here:\n"+
				"{{ticket_url}}\n"+
				"\n"+
				"{{company_name}}",
			"{{author}} replied to {{ticket_reference}}",
			"{{ticket_excerpt}}",
			"{{ticket_url}}"),

		all(EventNewLoginDetected, "New login",
			"New sign-in to your account",
			"A new sign-in to the account {{email}} was detected.\n"+
				"\n"+
				"When: {{login_at}}\n"+
				"Address: {{ip_address}}\n"+
				"Browser: {{user_agent}}\n"+
				"\n"+
				"If this was you, no action is needed.\n"+
				"If it was not, change your password now:\n"+
				"{{security_url}}\n"+
				"\n"+
				"{{company_name}}",
			"New sign-in detected",
			"{{login_at}} from {{ip_address}}.",
			"{{security_url}}"),
	}
}
