package notify

// The Discord mirror (GRW-04, NTF-13).
//
// `internal/growth` implements the webhook: the payload, the embed colours, the event
// selection and the credential handling. What was missing was a *call site*: no production
// path ever invoked Notify, so an operator could select an event and see nothing arrive.
//
// # Why the outbox is the call site
//
// A Discord post must happen after the transaction that produced the event has committed, and
// growth's own documentation says why: a slow or failing webhook must never roll back a
// payment. The event functions in this package run *inside* that transaction, so they cannot
// post. The drain loop is the first place that runs afterwards, in another process, with a
// durable record of what happened — which is exactly the position a mirror should post from.
//
// # Why a marker table rather than the delivery row
//
// A delivery row is per recipient: one event CC'd to two addresses is three rows, and posting
// each would put three copies of the same event in the channel. The marker is keyed by the
// outbox dedupe key, which is per event *instance*, so a mirrored event is mirrored once — and a
// replayed webhook, which writes no new row, cannot re-post either.
//
// The mirror is best-effort and does not retry, matching growth's contract: the durable channel
// is the mail outbox, and Discord is the convenience mirror. A failed post is recorded on the
// marker so "why is this event missing from the channel" has an answer, and is not retried,
// because a Discord outage that retries is a queue that outlives the outage.

import (
	"context"
	"errors"
	"strings"

	"github.com/catalyst/billing/internal/growth"
	"github.com/jackc/pgx/v5"
)

// Mirror posts one event instance to the operator's Discord channel.
//
// It is an interface rather than a concrete notifier so the drain loop can be tested without an
// HTTP client, and so an installation that removes the growth package still compiles.
type Mirror interface {
	MirrorEvent(ctx context.Context, eventKey string, p MirrorPayload) error
}

// MirrorPayload is the subset of an event that is useful in a chat channel.
//
// It is not the mail: a channel post is scanned, not read, so the subject becomes the title and
// the first lines of the body become the description. The email address is deliberately absent —
// a webhook URL is a credential the operator pasted, and it may point at a channel with more
// readers than the notification preference screen implies.
type MirrorPayload struct {
	Title       string
	Description string
}

// discordMirror adapts growth.DiscordNotifier to Mirror.
type discordMirror struct {
	notifier *growth.DiscordNotifier
}

// MirrorEvent implements Mirror.
func (d discordMirror) MirrorEvent(ctx context.Context, eventKey string, p MirrorPayload) error {
	target, ok := discordEventFor(eventKey)
	if !ok {
		// Not every notification event is one an operator can select for Discord, and inventing
		// a mapping would post an event under a name the settings screen never offered.
		return nil
	}
	return d.notifier.Notify(ctx, target, growth.Payload{
		Title:       p.Title,
		Description: p.Description,
	})
}

// discordShadow maps a notification event key onto the Discord event key an operator selects.
//
// The two vocabularies are close but not identical, and they are deliberately not merged: the
// notification event is what happened to a customer, the Discord event is what an operator asked
// to hear about. Events with no Discord equivalent are absent rather than mapped to the nearest
// neighbour, because a "ticket reply" posted as "ticket opened" is a lie in the channel.
func discordEventFor(eventKey string) (string, bool) {
	switch eventKey {
	case EventNewInvoiceCreated:
		return growth.EventInvoiceIssued, true
	case EventInvoicePaid:
		return growth.EventInvoicePaid, true
	case EventNewServerCreated:
		return growth.EventServiceCreated, true
	case EventServerSuspended:
		return growth.EventServiceSuspended, true
	case EventServerTerminated:
		return growth.EventServiceTerminated, true
	}
	return "", false
}

// mirroredEvents lists the notification events that have a Discord counterpart.
//
// The drain filters on it in SQL rather than fetching every event and skipping the unmapped ones
// in Go: an unmapped event writes no marker, so it would be re-examined on every pass forever.
func mirroredEvents() []string {
	return []string{
		EventNewInvoiceCreated,
		EventInvoicePaid,
		EventNewServerCreated,
		EventServerSuspended,
		EventServerTerminated,
	}
}

// mirrorDelivered posts events whose outbox rows have settled and that have not been mirrored.
//
// It runs at the end of a drain pass, outside any transaction, and never fails the pass: a mail
// was delivered, and a chat mirror that cannot be posted must not turn that into a retry. The
// marker row is claimed before the post so two workers draining in parallel cannot post twice;
// a claimed-but-failed marker keeps its error and is not retried.
func (q *Queue) mirrorDelivered(ctx context.Context, limit int) {
	if q.Mirror == nil || q.pool == nil {
		return
	}
	if limit <= 0 {
		limit = 25
	}
	rows, err := q.pool.Query(ctx, `
		SELECT DISTINCT ON (d.dedupe_key) d.dedupe_key, d.kind, d.subject, d.body
		FROM notification_deliveries d
		WHERE d.dedupe_key IS NOT NULL
		  AND d.status IN ('sent', 'suppressed')
		  AND d.kind = ANY($2::text[])
		  AND NOT EXISTS (
		      SELECT 1 FROM notification_mirrors m WHERE m.dedupe_key = d.dedupe_key)
		ORDER BY d.dedupe_key, d.created_at
		LIMIT $1`, limit, mirroredEvents())
	if err != nil {
		return
	}
	type candidate struct {
		key     string
		kind    string
		subject string
		body    string
	}
	var pending []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.key, &c.kind, &c.subject, &c.body); err != nil {
			rows.Close()
			return
		}
		pending = append(pending, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return
	}

	for _, c := range pending {
		// Claim first: the marker is the mutual exclusion between two drain passes.
		var claimed string
		err := q.pool.QueryRow(ctx, `
			INSERT INTO notification_mirrors (dedupe_key, event_key)
			VALUES ($1, $2)
			ON CONFLICT (dedupe_key) DO NOTHING
			RETURNING dedupe_key`, c.key, c.kind).Scan(&claimed)
		if err == pgx.ErrNoRows {
			continue
		}
		if err != nil {
			continue
		}
		err = q.Mirror.MirrorEvent(ctx, c.kind, MirrorPayload{
			Title:       c.subject,
			Description: excerpt(c.body, 500),
		})
		if err == nil {
			continue
		}
		// "The operator has not configured this event" is a decision, not a failure; it is still
		// recorded, because "why is this event missing from the channel" deserves an answer.
		reason := err.Error()
		if errors.Is(err, growth.ErrDiscordDisabled) {
			reason = "discord mirror is disabled or this event is not selected"
		}
		_, _ = q.pool.Exec(ctx, `
			UPDATE notification_mirrors SET last_error = $2 WHERE dedupe_key = $1`,
			c.key, truncate(reason, 500))
	}
}

// SystemMailKind names a message the installation sends about itself rather than about a
// customer's record: the test mail an operator triggers, and the alert a failed job sends.
//
// They are outbox rows like any other, because the guarantee that matters — "it was sent, or the
// log says why" — is the same one, and a test mail that bypassed the outbox would prove the SMTP
// settings without proving the path that actually delivers anything.
const (
	KindTestEmail   = "test_email"
	KindSystemAlert = "system_alert"
)

// systemMailDedupeKey scopes a system mail that may be sent repeatedly.
//
// A test mail has no natural instance identity — an operator may send three in a minute — so it
// is not deduplicated at all (an empty key is distinct from itself in the unique index). A cron
// alert does have one: the run it is about, which makes a retried alert a no-op.
func systemMailDedupeKey(kind, instance string) string {
	instance = strings.TrimSpace(instance)
	if instance == "" {
		return ""
	}
	return kind + ":" + instance
}
