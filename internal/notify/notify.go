// Package notify is the durable outbox behind every message the product sends.
//
// The rule that shapes it: a message is a *side effect of a committed change*, so it is
// written in the same transaction as that change and sent afterwards by a worker. A
// crash between "the reset token was created" and "the mail went out" therefore cannot
// lose the mail, and a retry cannot send it twice, because dedupe_key is unique.
//
// The alternative — sending mail inside the request — has two failure modes that both
// matter: a slow SMTP server makes the request slow (or times out and the customer is
// told the reset failed while the mail arrives), and a rolled-back transaction sends a
// mail about a change that did not happen.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/growth"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// Message kinds. They are a closed set, matching the CHECK on the table, so a typo
// fails at compile time or at insert rather than silently sending nothing.
const (
	KindPasswordReset     = "password_reset"
	KindEmailVerification = "email_verification"
	KindInvoiceIssued     = "invoice_issued"
	KindDunningReminder   = "dunning_reminder"
)

// Sender delivers one message.
//
// It is an interface so the tests can assert what would have been sent, and so an
// installation without SMTP can use the log sender and still have a delivery record.
//
// The contract deliberately stays "one plain-text body". A sender that can also carry a branded
// HTML alternative opts in by implementing HTMLSender, and one that does not keeps working
// unchanged: the queue falls back to the text body rather than handing a sender markup it would
// have to interpret. That is what makes branding additive instead of a new requirement on every
// implementation and every test double.
type Sender interface {
	Send(ctx context.Context, to, subject, body string) error
}

// HTMLSender is a Sender that can deliver a multipart/alternative message.
//
// SMTPSender and LogSender implement it. The plain-text argument is always the message's real
// body, so an implementation is free to ignore the html argument — it must never be the only part
// delivered.
type HTMLSender interface {
	SendHTML(ctx context.Context, to, subject, text, htmlPart string) error
}

// Message is one queued delivery.
type Message struct {
	Kind    string
	To      string
	Subject string
	Body    string
	// HTML is the branded multipart alternative, or "" for a plain-text message. It is stored
	// with the row because the drain happens later, in another process: the message that is
	// claimed must be the message that was rendered inside the transaction.
	HTML string
	// DedupeKey makes enqueueing idempotent, e.g. "invoice_issued:<invoice id>".
	// Empty means "always enqueue", which is only correct for a message a human
	// explicitly asked for.
	DedupeKey string
}

// Queue writes and drains the outbox.
type Queue struct {
	pool *store.Pool
	// MaxAttempts bounds retries. A message that cannot be delivered five times is a
	// configuration problem, not a transient one, and retrying forever turns it into an
	// unbounded queue.
	MaxAttempts int
	// Now is injectable for tests.
	Now func() time.Time
	// Mirror posts delivered events to the operator's Discord channel. It is nil by default in a
	// zero Queue and set by NewQueue; a test replaces it with a double to assert what was posted
	// without an HTTP client.
	Mirror Mirror
}

// NewQueue constructs a Queue.
func NewQueue(pool *store.Pool) *Queue {
	q := &Queue{pool: pool, MaxAttempts: 5, Now: func() time.Time { return time.Now().UTC() }}
	if pool != nil {
		// The mirror is wired here rather than by every caller because "the operator selected this
		// event" is a property of the installation, not of which subsystem emitted the event.
		q.Mirror = discordMirror{notifier: growth.NewDiscordNotifier(pool)}
	}
	return q
}

// Enqueue writes a delivery row inside the caller's transaction.
//
// It returns the row id and whether the row was created; a duplicate dedupe_key is not
// an error, because "already queued" and "queued" are the same outcome for the caller.
func (q *Queue) Enqueue(ctx context.Context, tx pgx.Tx, m Message) (id string, created bool, err error) {
	if m.To == "" {
		return "", false, errors.New("notify: message has no recipient")
	}
	if m.Kind == "" || m.Subject == "" || m.Body == "" {
		return "", false, errors.New("notify: message is incomplete")
	}

	var key any
	if m.DedupeKey != "" {
		key = m.DedupeKey
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO notification_deliveries
			(kind, to_address, subject, body, body_html, status, dedupe_key)
		VALUES ($1, $2, $3, $4, $5, 'queued', $6)
		ON CONFLICT (dedupe_key) DO NOTHING
		RETURNING id`,
		m.Kind, m.To, m.Subject, m.Body, m.HTML, key).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already queued under this dedupe key.
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("notify: enqueue: %w", err)
	}
	return id, true, nil
}

// SuppressKind records a deliberate non-send against an event key.
//
// It exists because the templated path names its deliveries by EVENT KEY. Building a Message to
// suppress from the hardcoded constructor would write the retired `invoice_issued` kind while the
// send writes `new_invoice_created`, and the two rows would be invisible to each other: a support
// agent asking "did we mail them?" would see nothing at all for the suppressed case.
//
// The dedupe key matches the one a real send writes for the same event and record, so the row is
// still a no-op if the mail already went out.
func (q *Queue) SuppressKind(ctx context.Context, tx pgx.Tx, kind, to, subject, dedupeKey, reason string) error {
	var key any
	if dedupeKey != "" {
		key = dedupeKey
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO notification_deliveries
			(kind, to_address, subject, body, body_html, status, dedupe_key, last_error)
		VALUES ($1, $2, $3, '', '', 'suppressed', $4, $5)
		ON CONFLICT (dedupe_key) DO NOTHING`,
		kind, to, subject, key, reason)
	if err != nil {
		return fmt.Errorf("notify: record suppression: %w", err)
	}
	return nil
}

// Suppress records that a message was deliberately not sent.
//
// "We did not send it" and "we failed to send it" are different answers to a support
// ticket, and only one of them is a bug, so the decision is recorded rather than the
// message being silently skipped.
func (q *Queue) Suppress(ctx context.Context, tx pgx.Tx, m Message, reason string) error {
	var key any
	if m.DedupeKey != "" {
		key = m.DedupeKey
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO notification_deliveries
			(kind, to_address, subject, body, body_html, status, dedupe_key, last_error)
		VALUES ($1, $2, $3, $4, $5, 'suppressed', $6, $7)
		ON CONFLICT (dedupe_key) DO NOTHING`,
		m.Kind, m.To, m.Subject, m.Body, m.HTML, key, reason)
	if err != nil {
		return fmt.Errorf("notify: record suppression: %w", err)
	}
	return nil
}

// Delivery is one claimed row.
type Delivery struct {
	ID       string
	Kind     string
	To       string
	Subject  string
	Body     string
	HTML     string
	Attempts int
}

// Result summarises one drain pass.
type Result struct {
	Sent     int
	Failed   int
	Attempts int
}

// RunOnce claims due deliveries and sends them.
//
// Claiming uses SKIP LOCKED so two workers can drain the same outbox without sending
// the same message twice. The row is marked 'sending' before the network call, so a
// worker that dies mid-send leaves a row that a later pass can reclaim.
func (q *Queue) RunOnce(ctx context.Context, sender Sender, limit int) (Result, error) {
	if sender == nil {
		return Result{}, errors.New("notify: no sender configured")
	}
	if limit <= 0 {
		limit = 25
	}
	now := q.Now()

	rows, err := q.pool.Query(ctx, `
		WITH due AS (
			SELECT id FROM notification_deliveries
			WHERE status IN ('queued', 'failed', 'sending')
			  AND attempts < $1
			  AND next_attempt_at <= $2
			ORDER BY created_at
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		UPDATE notification_deliveries d
		SET status = 'sending'
		FROM due
		WHERE d.id = due.id
		RETURNING d.id, d.kind, d.to_address, d.subject, d.body, d.body_html, d.attempts`,
		q.MaxAttempts, now, limit)
	if err != nil {
		return Result{}, fmt.Errorf("notify: claim deliveries: %w", err)
	}
	var claimed []Delivery
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.ID, &d.Kind, &d.To, &d.Subject, &d.Body, &d.HTML, &d.Attempts); err != nil {
			rows.Close()
			return Result{}, fmt.Errorf("notify: scan delivery: %w", err)
		}
		claimed = append(claimed, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Result{}, fmt.Errorf("notify: read deliveries: %w", err)
	}
	rows.Close()

	var res Result
	for _, d := range claimed {
		res.Attempts++
		if err := sendOne(ctx, sender, d); err != nil {
			res.Failed++
			// Exponential backoff in minutes, capped so a permanently failing address
			// stops consuming a slot on every pass.
			delay := time.Duration(1<<min(d.Attempts, 6)) * time.Minute
			if _, uerr := q.pool.Exec(ctx, `
				UPDATE notification_deliveries
				SET status = 'failed', attempts = attempts + 1, last_error = $2,
				    next_attempt_at = $3
				WHERE id = $1`,
				d.ID, truncate(err.Error(), 500), q.Now().Add(delay)); uerr != nil {
				return res, fmt.Errorf("notify: mark failed: %w", uerr)
			}
			continue
		}
		res.Sent++
		if _, err := q.pool.Exec(ctx, `
			UPDATE notification_deliveries
			SET status = 'sent', attempts = attempts + 1, sent_at = $2, last_error = NULL
			WHERE id = $1`, d.ID, q.Now()); err != nil {
			return res, fmt.Errorf("notify: mark sent: %w", err)
		}
	}
	// The mirror runs after the mails are settled and never changes the pass's result: a chat
	// webhook that is down must not make a delivered message look failed.
	q.mirrorDelivered(ctx, limit)
	return res, nil
}

// sendOne delivers one claimed row, using the HTML alternative only when there is one and the
// sender understands it.
//
// The fallback is the point: a Sender that predates this feature (a test double, an out-of-tree
// integration) receives the plain body, never the markup. A failure to deliver must not be caused
// by a decoration the recipient's client did not ask for.
func sendOne(ctx context.Context, sender Sender, d Delivery) error {
	if d.HTML != "" {
		if hs, ok := sender.(HTMLSender); ok {
			return hs.SendHTML(ctx, d.To, d.Subject, d.Body, d.HTML)
		}
	}
	return sender.Send(ctx, d.To, d.Subject, d.Body)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// LogSender writes messages to the log instead of sending them.
//
// It exists so a development installation has a working reset flow with no mail server,
// and so the delivery record is still written. It is never the right choice in
// production, which is why the sender is chosen explicitly at startup.
type LogSender struct {
	Logger *slog.Logger
}

// Send implements Sender.
func (s LogSender) Send(_ context.Context, to, subject, body string) error {
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("notification (log sender)",
		"to", to, "subject", subject, "body_bytes", len(body))
	return nil
}

// SendHTML implements HTMLSender.
//
// It logs the size of the HTML part rather than its content: a development installation's log is
// not the place to read a customer's invoice, and the fact that both parts were rendered is what
// a developer wants to know.
func (s LogSender) SendHTML(ctx context.Context, to, subject, text, htmlPart string) error {
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("notification (log sender)",
		"to", to, "subject", subject, "body_bytes", len(text), "html_bytes", len(htmlPart))
	return nil
}

// Templates. Plain text only: an invoice line description is attacker-controlled, and a
// template that renders it as markup turns a billing panel into a phishing page.

// PasswordResetMessage renders the reset mail.
//
// Deprecated: the wording is now the editable `password_reset` template, dispatched by
// notify.PasswordReset, which also writes the inbox row and honours `mail.disable`. This
// function is kept only so an out-of-tree caller still compiles; it is not used by this panel
// and it should be deleted once nothing references it, because a second renderer for a security
// mail is how the two get out of step.
func PasswordResetMessage(to, link string, expires time.Duration) Message {
	return Message{
		Kind:    KindPasswordReset,
		To:      to,
		Subject: "Reset your password",
		Body: strings.Join([]string{
			"Someone asked to reset the password for this account.",
			"",
			"Open this link to choose a new password:",
			link,
			"",
			fmt.Sprintf("The link expires in %s and can be used once.", expires),
			"If you did not ask for this, ignore this message; the password is unchanged.",
		}, "\n"),
	}
}

// EmailVerificationMessage renders the verification mail.
//
// Deprecated: the wording is now the editable `email_verification` template, dispatched by
// notify.EmailVerification (used by both registration and the signed-in resend endpoint). Kept
// for the same reason and with the same intent as PasswordResetMessage.
func EmailVerificationMessage(to, link string, expires time.Duration) Message {
	return Message{
		Kind:    KindEmailVerification,
		To:      to,
		Subject: "Confirm your email address",
		Body: strings.Join([]string{
			"Confirm this address to finish setting up the account.",
			"",
			link,
			"",
			fmt.Sprintf("The link expires in %s and can be used once.", expires),
		}, "\n"),
	}
}

// There is deliberately no InvoiceIssuedMessage constructor.
//
// It rendered a hardcoded "your invoice is ready" body with the retired `invoice_issued` kind, and
// when invoice mail moved onto the editable `new_invoice_created` template both remaining callers
// (checkout and staff issuance) moved with it — leaving the constructor called by nothing but its
// own test. It was deleted rather than deprecated because the two paths must agree on the kind: a
// suppression written under one name and a send under another are invisible to each other, which
// is precisely the bug that made "did we mail this customer?" unanswerable.
//
// Invoice mail is built by notify.InvoiceIssued, which takes InvoiceIssuedInput and derives its
// kind and dedupe key from the event key.
