package notify

// System mail: the messages an installation sends about *itself* rather than about a
// customer's record (NTF-09).
//
// Two of them, and they share a property that makes them worth separating from the event
// catalogue: neither has a customer's template behind it. A test mail is an operator proving the
// SMTP settings work, and a job-failure alert is the panel telling whoever runs it that something
// broke. Those are operator-facing sentences, and putting them in the same editable catalogue as
// "your invoice is paid" would offer an operator the ability to word them into something the code
// no longer checks.
//
// They still go through the outbox, and that is the point: a test mail sent straight through an
// SMTP client would prove the credentials and nothing else. Queued, it exercises the same claim,
// render, deliver and record path as every real message — which is the path an operator is
// actually trying to test — and its result is visible in the email log (NTF-06).

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestEmailInput is what SendTestEmail needs.
type TestEmailInput struct {
	// To is the recipient. An empty value means the caller's own address, which is the case the
	// screen offers: proving the relay works by mailing yourself.
	To string
	// RequestedBy is the staff user's address, recorded in the message so a mail arriving in a
	// shared inbox can be traced to the person who asked for it.
	RequestedBy string
}

// SendTestEmail enqueues a branded test message inside the caller's transaction.
//
// It returns the delivery row's id so the screen can say "queued" and the operator can find the
// outcome in the email log. It is not deduplicated: an operator who sends three test mails in a
// minute is doing exactly what the button is for, and an empty dedupe key is distinct from itself
// in the unique index.
func SendTestEmail(ctx context.Context, tx pgx.Tx, queue *Queue, in TestEmailInput) (string, error) {
	if queue == nil {
		return "", errors.New("notify: no queue configured for the test email")
	}
	to := strings.TrimSpace(in.To)
	if to == "" {
		return "", errors.New("notify: test email has no recipient")
	}
	if strings.ContainsAny(to, "\r\n") {
		// The address lands in a header; a newline there is how one message becomes two.
		return "", errors.New("notify: test email recipient contains a newline")
	}

	brand := MailBrandFor(ctx, tx)
	company := brand.Company
	subject := "Test email from " + company
	body := strings.Join([]string{
		"This is a test message from " + company + ".",
		"",
		"Mail delivery is working: this message was rendered, queued and delivered by the same",
		"path every invoice and password-reset notice takes. Nothing else was changed.",
		"",
		"Requested by " + orDefault(in.RequestedBy, "a staff account") + ".",
		"",
		company,
	}, "\n")

	msg := Message{
		Kind:    KindTestEmail,
		To:      to,
		Subject: subject,
		Body:    body,
		HTML:    brand.HTMLPart(Rendered{Subject: subject, Body: body}, publicURL()),
	}
	id, created, err := queue.Enqueue(ctx, tx, msg)
	if err != nil {
		return "", err
	}
	if !created {
		// An empty dedupe key cannot collide, so this is an impossible state rather than a
		// deduplicated send; saying so is better than returning "" to a handler that would
		// report a delivery id that does not exist.
		return "", errors.New("notify: the test email was deduplicated")
	}
	return id, nil
}

// CronFailureInput is what SendCronFailure needs.
type CronFailureInput struct {
	// Job is the job's stable name, e.g. "renewals".
	Job string
	// RunID is the cron_runs row, used as the dedupe component: a retried alert for the same
	// failed run is a no-op, while the next failure is a new message.
	RunID string
	// Detail is the job's own error text, already truncated by the runner.
	Detail   string
	FailedAt time.Time
	// To overrides the configured system address. Empty reads general.system_email_address.
	To string
}

// SendCronFailure alerts the operator that a scheduled job failed, inside the caller's
// transaction.
//
// A cron failure that is only visible in a table is a failure nobody reads: the whole class of
// "renewals stopped running three weeks ago" bugs is invisible until a customer notices. The
// alert is deduplicated by the run id, so the same failed run cannot mail twice.
func SendCronFailure(ctx context.Context, tx pgx.Tx, queue *Queue, in CronFailureInput) (Outcome, error) {
	res := Outcome{EventKey: KindSystemAlert}
	if queue == nil {
		return res, errors.New("notify: no queue configured for the cron alert")
	}
	to := strings.TrimSpace(in.To)
	if to == "" {
		to = systemAddress(ctx, tx)
	}
	if to == "" {
		res.Reason = "no_recipient"
		return res, nil
	}

	brand := MailBrandFor(ctx, tx)
	subject := "Scheduled job failed: " + orDefault(in.Job, "unknown job")
	body := strings.Join([]string{
		"The scheduled job " + orDefault(in.Job, "(unnamed)") + " failed.",
		"",
		"Failed at: " + formatWhen(in.FailedAt),
		"Run: " + orDefault(in.RunID, "(unrecorded)"),
		"",
		orDefault(in.Detail, "The job did not report a reason."),
		"",
		"Nothing was changed by the failed run that this message knows about. Check the cron run",
		"history before retrying it by hand.",
		"",
		brand.Company,
	}, "\n")

	msg := Message{
		Kind:      KindSystemAlert,
		To:        to,
		Subject:   subject,
		Body:      body,
		HTML:      brand.HTMLPart(Rendered{Subject: subject, Body: body}, publicURL()),
		DedupeKey: systemMailDedupeKey(KindSystemAlert, in.RunID),
	}
	if mailDisabledBySetting(ctx, tx) {
		// `mail.disable` is the operator's emergency brake; an alert that ignored it would be the
		// one message that cannot be stopped during a relay misconfiguration, which is exactly
		// when it would be sent most often.
		res.Reason = "mail_disabled_by_setting"
		return res, queue.Suppress(ctx, tx, msg, "mail.disable is set")
	}
	_, created, err := queue.Enqueue(ctx, tx, msg)
	if err != nil {
		return res, err
	}
	res.Mailed = created
	return res, nil
}

// systemAddress reads the address the installation sends its own mail to.
//
// It is `general.system_email_address`, the setting the panel already seeds for exactly this
// purpose, and a missing value is "" rather than a guess: a cron alert mailed to a made-up
// address is worse than one that was never sent, and Outcome records the reason.
func systemAddress(ctx context.Context, q Querier) string {
	var value string
	if err := q.QueryRow(ctx,
		`SELECT value FROM settings WHERE group_key = 'general' AND key = 'system_email_address'`).
		Scan(&value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}
