package tickets

import (
	"context"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// Auto-close (TKT-10 / OPS-06).
//
// Paymenter closes a ticket that nobody has replied to for N days. The rule is
// deliberately conservative: only a ticket that is *not already closed* and whose
// last activity is older than the threshold is touched, and the close is stamped
// with the same instant the scan used, so the audit trail explains itself.
//
// CloseStale is exported rather than hidden in the worker because the worker is not
// the only caller that needs it: a test drives it directly, and an operator
// re-running the job by hand should call the same function rather than a second
// implementation. The integrator wires it into cmd/billing/jobs.go; this package
// does not import the worker, so the worker can be restarted or replaced without
// this rule changing.
//
// The threshold comes from the `tickets.autoclose_days` setting. A non-positive
// `days` means "use the configured value"; the caller passes 0 to say "whatever the
// operator chose".

// CloseStale is the entry point the worker calls.
//
// It exists as a package-level function as well as a Store method because the job
// runner has a *store.Pool and nothing else; constructing a Store there would be a
// line of ceremony around one query. Wire it into cmd/billing/jobs.go as a jobSpec:
//
//	tickets.CloseStale(ctx, pool, 0)
//
// A days value of 0 means "use the tickets.autoclose_days setting", which is what the
// worker should pass so the operator's choice is the one that applies.
func CloseStale(ctx context.Context, pool *store.Pool, days int) (int, error) {
	closed, err := New(pool).CloseStale(ctx, days)
	if err != nil {
		return 0, err
	}
	return len(closed), nil
}

// StaleBatch bounds how many tickets one CloseStale run closes. A backlog drains over
// several ticks rather than in one long transaction.
const StaleBatch = 200

// CloseStale closes every open or replied ticket whose last reply is older than
// days, and returns the tickets it closed.
//
// It deliberately does **not** apply the close-time custom field requirement. That rule exists to
// make a *person* record what a resolved ticket needed (the root cause, the customer's
// confirmation); a stale ticket is one nobody is going to answer, and enforcing the rule here
// would mean that adding a required-to-close field silently switches auto-close off for the whole
// installation — a queue that never drains, which is the failure the rule was meant to prevent.
// The auto-close is therefore an "abandoned" close, not a "resolved" one, and it is the only path
// that closes a ticket without the answers.
//
// A ticket with no reply at all (last_reply_at IS NULL) falls back to its creation
// time, so a ticket opened and abandoned also ages out. That is the case Paymenter's
// own scan misses, and it is the one that fills a queue.
func (s *Store) CloseStale(ctx context.Context, days int) ([]Ticket, error) {
	if days <= 0 {
		settings, err := s.Settings(ctx)
		if err != nil {
			return nil, err
		}
		days = settings.AutoCloseDays
	}
	if days <= 0 {
		days = DefaultAutoCloseDays
	}

	now := s.clock()
	cutoff := StaleAfter(now, days)

	var closed []Ticket
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// One statement: pick a bounded batch with SKIP LOCKED, close it, and return the
		// closed rows in the API's shape.
		//
		// The bound matters. A cron that closes "everything stale" is a cron that one
		// day takes an hour and holds locks while it does; a batch of StaleBatch rows
		// per tick drains a backlog over a few runs and cannot stall the queue. SKIP
		// LOCKED means two workers running the same tick split the work instead of
		// blocking on each other.
		rows, err := tx.Query(ctx, `
			WITH due AS (
				SELECT id FROM tickets
				WHERE status <> 'closed'
				  AND COALESCE(last_reply_at, created_at) < $1
				ORDER BY COALESCE(last_reply_at, created_at)
				LIMIT $3
				FOR UPDATE SKIP LOCKED
			), stale AS (
				UPDATE tickets
				SET status = 'closed', closed_at = $2, updated_at = $2
				WHERE id IN (SELECT id FROM due)
				RETURNING *
			)
			SELECT `+ticketSelect+`
			FROM stale t`+ticketLateral+`
			ORDER BY t.created_at DESC, t.id DESC`, cutoff, now, StaleBatch)
		if err != nil {
			return fmt.Errorf("tickets: close stale: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			ticket, err := scanTicket(rows)
			if err != nil {
				return fmt.Errorf("tickets: scan stale ticket: %w", err)
			}
			closed = append(closed, ticket)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("tickets: close stale: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return closed, nil
}

// StaleAfter is the cutoff a scan at now would use. Exported for the worker's log
// line, so the operator sees the threshold the run actually applied.
func StaleAfter(now time.Time, days int) time.Time {
	if days <= 0 {
		days = DefaultAutoCloseDays
	}
	return now.UTC().AddDate(0, 0, -days)
}
