package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/notify"
	"github.com/jackc/pgx/v5"
)

// Termination of long-suspended services (OPS-05).
//
// Dunning can suspend a service and can resume it. Nothing could end it, so a service
// suspended for non-payment stayed suspended forever: the server kept running at the
// provider, the panel kept paying for it, and the customer's invoice stayed open in a state
// nobody would ever resolve. This job is the end of that road, after
// `cronjob.terminate_days`.
//
// # Termination goes through the queue, always
//
// The job does not touch `services.status`. It enqueues a `terminate` operation with the
// service's current generation — the same operation key the staff terminate handler and the
// executed cancellation already use — and the provisioning worker applies the transition
// when the adapter call succeeds. That is not ceremony: the adapter call can fail, can time
// out, or can succeed while the worker dies before recording it, and only the queue has the
// leases, the fencing token and the reconciler to survive that. A cron that wrote
// `status = 'terminated'` would claim a server was destroyed when nobody had asked the
// provider.
//
// # The vetoes
//
// The job is deliberately hard to trigger. A service is terminated only when *all* of these
// hold, and each is re-checked inside the transaction that enqueues the work:
//
//   - it is still `suspended` (a resume that landed a second ago wins);
//   - no active manual hold exists. A hold is an operator saying "do not touch this", and it
//     outranks every automated rule — that is the whole point of `service_holds`. The check
//     is dunning's own definition of an active hold, asked through `dunning.Engine.ActiveHold`
//     rather than re-derived here;
//   - it has been suspended for longer than the threshold, measured from the *last successful
//     suspend operation* (persisted state, not a guess from `updated_at`);
//   - no terminate operation has ever been enqueued for it, so a second run is a no-op;
//   - the customer still owes money. This is dunning's resume rule asked in reverse: resume
//     fires when nothing is owed, so termination must not fire in the same situation. Without
//     it, a customer who paid yesterday could have their service destroyed today by a job
//     that only looked at how long the service had been suspended.
//
// What it does on the way: the service's open, untouched invoices are voided through the same
// helper the order-cancellation job uses, billing stops on the subscription, and the customer
// is told through internal/notify with the `server_terminated` template.

// TerminatedService is one service the run enqueued termination for.
type TerminatedService struct {
	ServiceID   string
	CustomerID  string
	OperationID string
	// InvoiceIDs are the invoices this run voided.
	InvoiceIDs []string
	// Notified reports whether a notification row was written.
	Notified bool
	// SuspendedFor is how long the service had been suspended when the decision was made.
	SuspendedFor time.Duration
}

// TerminationRun reports what one run did.
type TerminationRun struct {
	Days   int
	Cutoff time.Time
	// Scanned is how many candidate services the scan returned.
	Scanned int
	// Terminated are the services this run enqueued termination for.
	Terminated []TerminatedService
	// Failures are per-service errors; the batch continues past them.
	Failures []string
	// Reason is set when the run deliberately did nothing.
	Reason string
}

// TerminateSuspended enqueues termination for services suspended longer than
// `cronjob.terminate_days`.
//
// It is idempotent: the terminate operation key is unique per service and generation, and the
// enqueue is the claim (a worker that loses the race gets zero rows affected and does
// nothing else). Running it twice therefore enqueues one operation, voids one set of
// invoices, and sends one message.
func (r *Runner) TerminateSuspended(ctx context.Context) (TerminationRun, error) {
	run := TerminationRun{}

	if disabled(TerminationKillSwitch) {
		run.Reason = ReasonKillSwitch
		return run, nil
	}

	days, reason, err := r.threshold(ctx, terminateDaysSetting)
	if err != nil {
		return run, err
	}
	if reason != "" {
		run.Reason = reason
		return run, nil
	}
	run.Days = days
	run.Cutoff = cutoff(r.now(), days)

	ids, err := r.scanSuspendedServices(ctx, run.Cutoff, r.batch())
	if err != nil {
		return run, err
	}
	run.Scanned = len(ids)

	now := r.now()
	for _, id := range ids {
		// Ask dunning, which owns the definition of a manual hold, before spending a
		// transaction on the candidate. The answer is a hint — the transaction re-reads the
		// holds under its own lock — but asking here means the rule is expressed in terms of
		// the engine that owns it rather than in terms of a query this package invented.
		if _, held, err := r.holds.ActiveHold(ctx, id); err != nil {
			run.Failures = append(run.Failures, fmt.Sprintf("%s: %v", id, err))
			continue
		} else if held {
			continue
		}

		terminated, skipped, err := r.terminateService(ctx, id, run.Cutoff, days, now)
		if err != nil {
			run.Failures = append(run.Failures, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		if skipped != "" {
			continue
		}
		run.Terminated = append(run.Terminated, terminated)
	}
	return run, nil
}

// scanSuspendedServices returns the ids of services that look terminable.
//
// The suspension date is the last *succeeded* suspend operation's finish time. A service
// suspended twice (suspend, resume, suspend) must age from the second suspension, which is
// why this is MAX(...) and not "exists a suspend older than the cutoff". A service with no
// successful suspend operation has no suspension date and is not selected: nothing here
// invents one from `updated_at`, which changes for reasons that have nothing to do with
// suspension.
func (r *Runner) scanSuspendedServices(ctx context.Context, before time.Time, limit int) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT s.id
		FROM services s
		WHERE s.status = 'suspended'
		  AND (
		      SELECT MAX(so.finished_at)
		      FROM service_operations so
		      WHERE so.service_id = s.id
		        AND so.kind = 'suspend'
		        AND so.status = 'succeeded'
		  ) < $1
		  -- One terminate intent per service, whatever became of it. A failed operation is
		  -- for an operator to look at, not for this job to re-enqueue every half hour.
		  AND NOT EXISTS (
		      SELECT 1 FROM service_operations t
		      WHERE t.service_id = s.id AND t.kind = 'terminate')
		ORDER BY s.id
		LIMIT $2`, before, limit)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: scan suspended services: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("lifecycle: scan suspended service: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lifecycle: scan suspended services: %w", err)
	}
	return ids, nil
}

// terminateService enqueues termination for one service, or explains why it did not.
func (r *Runner) terminateService(ctx context.Context, serviceID string, before time.Time, days int,
	now time.Time) (TerminatedService, string, error) {

	out := TerminatedService{ServiceID: serviceID}
	skip := ""

	err := r.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			status         string
			generation     int64
			customerID     string
			subscriptionID *string
			productName    string
		)
		err := tx.QueryRow(ctx, `
			SELECT s.status, s.generation, s.customer_id, s.subscription_id, p.name
			FROM services s
			JOIN products p ON p.id = s.product_id
			WHERE s.id = $1
			FOR UPDATE OF s`, serviceID).
			Scan(&status, &generation, &customerID, &subscriptionID, &productName)
		if errors.Is(err, pgx.ErrNoRows) {
			skip = "the service no longer exists"
			return nil
		}
		if err != nil {
			return fmt.Errorf("lifecycle: lock service: %w", err)
		}
		out.CustomerID = customerID

		if status != "suspended" {
			skip = fmt.Sprintf("its status is %s", status)
			return nil
		}

		// A manual hold outranks the rule. Ask dunning, which owns the definition of an
		// active hold, and then re-read it under this transaction's snapshot: a hold placed
		// while the scan was running must still win.
		held, err := holdInPlace(ctx, tx, serviceID, now)
		if err != nil {
			return err
		}
		if held {
			skip = "a manual hold is in place"
			return nil
		}

		// Age the suspension from the last successful suspend operation.
		var suspendedAt *time.Time
		if err := tx.QueryRow(ctx, `
			SELECT MAX(finished_at) FROM service_operations
			WHERE service_id = $1 AND kind = 'suspend' AND status = 'succeeded'`,
			serviceID).Scan(&suspendedAt); err != nil {
			return fmt.Errorf("lifecycle: read suspension date: %w", err)
		}
		if suspendedAt == nil {
			skip = "it has no recorded suspension"
			return nil
		}
		if !suspendedAt.Before(before) {
			skip = "it has not been suspended long enough"
			return nil
		}
		out.SuspendedFor = now.Sub(*suspendedAt)

		// Is there still a debt? This is dunning's resume rule read backwards: resume
		// enqueues when the customer owes nothing, so termination must not fire in that
		// state, or the two rules would fight over the same service every tick.
		var owes bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM invoices
				WHERE customer_id = $1
				  AND status = 'open'
				  AND total_minor > amount_paid_minor)`, customerID).Scan(&owes); err != nil {
			return fmt.Errorf("lifecycle: read outstanding balance: %w", err)
		}
		if !owes {
			skip = "nothing is owed, so the resume rule applies instead"
			return nil
		}

		// The enqueue is the claim. The operation key is the one the staff terminate handler
		// and the executed cancellation already use, so all three paths are the same
		// operation rather than three calls to the adapter. If another worker (or a staff
		// member) got here first, this inserts nothing and this run must not void invoices or
		// send a second message.
		key := fmt.Sprintf("terminate:%s:%d", serviceID, generation)
		tag, err := tx.Exec(ctx, `
			INSERT INTO service_operations (service_id, kind, operation_key, generation, status)
			VALUES ($1, 'terminate', $2, $3, 'queued')
			ON CONFLICT (operation_key) DO NOTHING`, serviceID, key, generation)
		if err != nil {
			return fmt.Errorf("lifecycle: enqueue termination: %w", err)
		}
		if tag.RowsAffected() == 0 {
			skip = "a termination is already queued"
			return nil
		}
		if err := tx.QueryRow(ctx, `
			SELECT id FROM service_operations WHERE operation_key = $1`, key).
			Scan(&out.OperationID); err != nil {
			return fmt.Errorf("lifecycle: read enqueued termination: %w", err)
		}

		// Void the invoices that fund this service, through the same helper the order
		// cancellation uses. A partly paid invoice is left alone: the customer owes it, and
		// the correction for a partly paid document is a credit note.
		if subscriptionID != nil {
			invoiceRows, err := tx.Query(ctx, `
				SELECT id FROM invoices
				WHERE subscription_id = $1 AND status = 'open' AND amount_paid_minor = 0
				ORDER BY created_at
				FOR UPDATE`, *subscriptionID)
			if err != nil {
				return fmt.Errorf("lifecycle: read service invoices: %w", err)
			}
			var invoiceIDs []string
			for invoiceRows.Next() {
				var id string
				if err := invoiceRows.Scan(&id); err != nil {
					invoiceRows.Close()
					return fmt.Errorf("lifecycle: scan service invoice: %w", err)
				}
				invoiceIDs = append(invoiceIDs, id)
			}
			if err := invoiceRows.Err(); err != nil {
				invoiceRows.Close()
				return fmt.Errorf("lifecycle: read service invoices: %w", err)
			}
			invoiceRows.Close()

			reason := fmt.Sprintf("This invoice was cancelled because the service was terminated after being suspended for more than %d days.", days)
			for _, id := range invoiceIDs {
				voided, err := voidInvoice(ctx, tx, id, reason)
				if err != nil {
					return err
				}
				if voided {
					out.InvoiceIDs = append(out.InvoiceIDs, id)
				}
			}

			// Billing stops now. The termination itself is applied by the worker, but a
			// subscription that keeps renewing after the service is gone invoices a customer
			// for nothing. This mirrors what the executed cancellation does.
			if _, err := tx.Exec(ctx, `
				UPDATE subscriptions
				SET cancel_at_period_end = true,
				    cancelled_at = COALESCE(cancelled_at, $2),
				    version = version + 1
				WHERE id = $1`, *subscriptionID, now); err != nil {
				return fmt.Errorf("lifecycle: stop billing: %w", err)
			}
		}

		// Tell the customer. It is enqueued inside this transaction, so the message exists if
		// and only if the termination intent committed. A missing template fails the
		// transaction on purpose: destroying a service without telling its owner is worse than
		// leaving it suspended for another tick.
		var (
			userID string
			email  string
		)
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE((
			           SELECT ca.user_id::text FROM customer_accounts ca
			           WHERE ca.customer_id = c.id
			           ORDER BY (ca.role = 'owner') DESC, ca.created_at
			           LIMIT 1), ''),
			       c.email
			FROM customers c WHERE c.id = $1`, customerID).Scan(&userID, &email); err != nil {
			return fmt.Errorf("lifecycle: read customer contact: %w", err)
		}
		res, err := notify.ServerTerminated(ctx, tx, r.queue, notify.ServerTerminatedInput{
			UserID:       userID,
			Email:        email,
			ServiceID:    serviceID,
			ServiceName:  productName,
			TerminatedAt: now,
		})
		if err != nil {
			return err
		}
		out.Notified = res.Mailed || res.Inboxed

		return audit.Record(ctx, tx, audit.Event{
			Actor:      audit.Actor{Kind: audit.ActorSystem},
			Action:     "service.terminate",
			TargetType: "service",
			TargetID:   serviceID,
			Outcome:    audit.OutcomeSuccess,
			Changes: map[string]any{
				"customer_id":      customerID,
				"generation":       generation,
				"operation_id":     out.OperationID,
				"days":             days,
				"cutoff":           before.Format(time.RFC3339),
				"suspended_since":  suspendedAt.Format(time.RFC3339),
				"invoices_voided":  out.InvoiceIDs,
				"notified":         out.Notified,
				"reason":           "suspended longer than terminate_days",
				"automated_action": true,
			},
		})
	})
	if err != nil {
		return TerminatedService{}, "", err
	}
	if skip != "" {
		return TerminatedService{}, skip, nil
	}
	return out, "", nil
}

// holdInPlace reports whether an unexpired manual hold applies to a service.
//
// This is dunning's definition of an active hold, repeated here because the engine's own
// accessor reads through the pool and this decision must be made against the rows this
// transaction has locked. The two cannot drift silently: TerminateSuspended asks
// `dunning.Engine.ActiveHold` first, this re-reads the same three clauses under the lock, and
// TestTerminationRespectsAManualHold exercises both against the same hold.
func holdInPlace(ctx context.Context, tx pgx.Tx, serviceID string, now time.Time) (bool, error) {
	var held bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM service_holds
			WHERE service_id = $1 AND (expires_at IS NULL OR expires_at > $2)
		)`, serviceID, now).Scan(&held)
	if err != nil {
		return false, fmt.Errorf("lifecycle: check holds: %w", err)
	}
	return held, nil
}
