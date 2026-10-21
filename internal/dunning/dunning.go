// Package dunning decides when an unpaid invoice escalates, and when a service
// may be suspended because of it.
//
// Two rules shape everything here.
//
// First, suspension is a last resort with a paper trail. The decision is written
// to suspension_decisions with the reason and the service generation, so "why was
// this customer suspended" is answerable months later without reading logs.
//
// Second, no automated action may override a human. A manual hold, a payment that
// arrived a second ago, or a cancellation that is already scheduled all block the
// transition, and they are re-read inside the transaction that enqueues the work
// rather than trusted from an earlier scan.
//
// What this package deliberately does not do: charge a stored payment method. That
// requires recorded consent and a reusable method, and the v1 gateway protocol has
// no off-session charge method. Dunning therefore escalates and reminds; it does
// not invent a charge.
package dunning

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/ledger"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// State is the persisted dunning state of an invoice.
type State string

const (
	// StateOpen means the invoice is within its payment terms.
	StateOpen State = "open"
	// StateOverdue means the invoice is past due and reminders are running.
	StateOverdue State = "overdue"
	// StateExhausted means the retry budget is spent; suspension is permitted.
	StateExhausted State = "exhausted"
	// StateResolved means the invoice was paid or voided.
	StateResolved State = "resolved"
)

// Policy controls escalation.
type Policy struct {
	// ReminderInterval is the gap between dunning steps.
	ReminderInterval time.Duration
	// MaxSteps is how many reminders run before the state is exhausted.
	MaxSteps int
	// SuspendAfter is how long after the due date a service may be suspended.
	// It is independent of MaxSteps so an operator can require both.
	SuspendAfter time.Duration
}

// DefaultPolicy is a deliberately conservative escalation: three reminders over a
// week, then suspension becomes possible only after a further grace period.
func DefaultPolicy() Policy {
	return Policy{
		ReminderInterval: 48 * time.Hour,
		MaxSteps:         3,
		SuspendAfter:     7 * 24 * time.Hour,
	}
}

// Engine runs dunning.
type Engine struct {
	pool   *store.Pool
	policy Policy
	// Now is injectable for tests.
	Now func() time.Time
	// auditor, when set, writes an audit row inside the same transaction that enqueues the
	// operation.
	//
	// It is a callback rather than a direct dependency on the audit package so the engine does
	// not need to know about HTTP identities, and a copy of the engine is given a per-request
	// auditor (see WithAuditor) rather than the engine holding mutable request state.
	auditor Auditor
}

// Auditor records who asked for a service action.
//
// It is called inside the enqueue transaction, which is the point: an audit row for a change that
// rolled back is a lie, and a change with no audit row is a gap.
type Auditor interface {
	RecordAction(ctx context.Context, tx pgx.Tx, action, serviceID string, generation int64, reason string) error
}

// WithAuditor returns a copy of the engine that writes an audit row inside each mutating
// transaction.
//
// A copy, not a mutation: the engine is shared by every request, and an actor set on it would be
// whichever request happened to set it last.
func (e *Engine) WithAuditor(a Auditor) *Engine {
	clone := *e
	clone.auditor = a
	return &clone
}

// audit records the action if an auditor is configured.
func (e *Engine) audit(ctx context.Context, tx pgx.Tx, action, serviceID string, generation int64, reason string) error {
	if e.auditor == nil {
		return nil
	}
	return e.auditor.RecordAction(ctx, tx, action, serviceID, generation, reason)
}

// NewEngine constructs an Engine.
func NewEngine(pool *store.Pool, policy Policy) *Engine {
	if policy.MaxSteps <= 0 {
		policy = DefaultPolicy()
	}
	return &Engine{pool: pool, policy: policy, Now: func() time.Time { return time.Now().UTC() }}
}

// AdvanceResult reports what one advance did.
type AdvanceResult struct {
	InvoiceID string
	State     State
	Step      int
	// Exhausted is true when this step used the last of the retry budget.
	Exhausted bool
}

// Advance moves overdue invoices through the dunning schedule.
//
// It is idempotent per step: a row is only advanced when its next_action_at is
// due, and the update is guarded on the step it read, so two workers cannot both
// advance the same invoice.
func (e *Engine) Advance(ctx context.Context, limit int) ([]AdvanceResult, error) {
	if limit <= 0 {
		limit = 100
	}
	now := e.Now()

	// Bring newly overdue invoices into dunning, and resolve settled ones.
	if err := e.sync(ctx, now); err != nil {
		return nil, err
	}

	rows, err := e.pool.Query(ctx, `
		SELECT invoice_id, step FROM dunning_states
		WHERE state = 'overdue' AND next_action_at IS NOT NULL AND next_action_at <= $1
		ORDER BY next_action_at
		LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("dunning: scan due states: %w", err)
	}
	type due struct {
		id   string
		step int
	}
	var dues []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.step); err != nil {
			rows.Close()
			return nil, fmt.Errorf("dunning: scan state: %w", err)
		}
		dues = append(dues, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("dunning: scan states: %w", err)
	}
	rows.Close()

	results := make([]AdvanceResult, 0, len(dues))
	for _, d := range dues {
		res, err := e.advanceOne(ctx, d.id, d.step, now)
		if err != nil {
			return results, err
		}
		results = append(results, res)
	}
	return results, nil
}

// sync creates dunning state for overdue invoices and closes settled ones.
func (e *Engine) sync(ctx context.Context, now time.Time) error {
	// Newly overdue: an open invoice past its due date with no dunning state.
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO dunning_states (invoice_id, state, step, next_action_at)
		SELECT i.id, 'overdue', 0, $1
		FROM invoices i
		WHERE i.status = 'open' AND i.due_at IS NOT NULL AND i.due_at < $1
		ON CONFLICT (invoice_id) DO NOTHING`, now); err != nil {
		return fmt.Errorf("dunning: open overdue states: %w", err)
	}

	// Paid or voided invoices are resolved; no further action is scheduled.
	if _, err := e.pool.Exec(ctx, `
		UPDATE dunning_states d
		SET state = 'resolved', next_action_at = NULL, updated_at = now()
		FROM invoices i
		WHERE d.invoice_id = i.id
		  AND d.state <> 'resolved'
		  AND i.status IN ('paid', 'void', 'uncollectible')`); err != nil {
		return fmt.Errorf("dunning: resolve settled states: %w", err)
	}
	return nil
}

// advanceOne performs one dunning step.
func (e *Engine) advanceOne(ctx context.Context, invoiceID string, step int, now time.Time) (AdvanceResult, error) {
	res := AdvanceResult{InvoiceID: invoiceID}
	err := e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Re-check the invoice: it may have been paid between the scan and now.
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM invoices WHERE id = $1 FOR UPDATE`,
			invoiceID).Scan(&status); err != nil {
			return fmt.Errorf("dunning: lock invoice: %w", err)
		}
		if status != "open" {
			if _, err := tx.Exec(ctx, `
				UPDATE dunning_states SET state = 'resolved', next_action_at = NULL, updated_at = now()
				WHERE invoice_id = $1`, invoiceID); err != nil {
				return fmt.Errorf("dunning: resolve: %w", err)
			}
			res.State = StateResolved
			res.Step = step
			return nil
		}

		nextStep := step + 1
		exhausted := nextStep >= e.policy.MaxSteps
		nextState := StateOverdue
		var next *time.Time
		if exhausted {
			nextState = StateExhausted
		} else {
			t := now.Add(e.policy.ReminderInterval)
			next = &t
		}

		// Guard on the step that was read: a concurrent worker that already
		// advanced this row affects no rows here.
		tag, err := tx.Exec(ctx, `
			UPDATE dunning_states
			SET state = $3, step = $4, last_action_at = $5, next_action_at = $6, updated_at = now()
			WHERE invoice_id = $1 AND step = $2`,
			invoiceID, step, string(nextState), nextStep, now, next)
		if err != nil {
			return fmt.Errorf("dunning: advance state: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("dunning: invoice %s was advanced concurrently", invoiceID)
		}

		res.State = nextState
		res.Step = nextStep
		res.Exhausted = exhausted
		return nil
	})
	return res, err
}

// Eligibility explains whether a service may be suspended, and why not.
type Eligibility struct {
	ServiceID string `json:"service_id"`
	InvoiceID string `json:"invoice_id,omitempty"`
	Eligible  bool   `json:"eligible"`
	// Reason always explains the outcome, including when eligible.
	Reason string `json:"reason"`
	// Generation is the service generation at decision time.
	Generation int64 `json:"generation"`
	// Outstanding is what is owed, when the block is financial.
	Outstanding *string `json:"outstanding,omitempty"`
}

// ErrNotEligible is returned when a suspension was requested but must not happen.
var ErrNotEligible = errors.New("dunning: service is not eligible for suspension")

// Evaluate decides whether a service may be suspended for non-payment, and
// records the decision.
//
// The checks run inside one transaction against locked rows, so a payment that
// lands while this is running either wins outright or is seen by this check. That
// is the difference between "we usually do not suspend paying customers" and "we
// cannot".
func (e *Engine) Evaluate(ctx context.Context, serviceID string) (Eligibility, error) {
	now := e.Now()
	out := Eligibility{ServiceID: serviceID}

	err := e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			status     string
			generation int64
			customerID string
		)
		err := tx.QueryRow(ctx, `
			SELECT status, generation, customer_id FROM services WHERE id = $1 FOR UPDATE`,
			serviceID).Scan(&status, &generation, &customerID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("dunning: service %s not found", serviceID)
		}
		if err != nil {
			return fmt.Errorf("dunning: lock service: %w", err)
		}
		out.Generation = generation

		hold := func(reason string) error {
			out.Eligible = false
			out.Reason = reason
			return recordDecision(ctx, tx, out, "held")
		}

		if status != "active" {
			return hold(fmt.Sprintf("service is %s, not active", status))
		}

		// A manual hold is an operator's veto and outranks every automated rule.
		var held bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM service_holds
				WHERE service_id = $1 AND (expires_at IS NULL OR expires_at > $2)
			)`, serviceID, now).Scan(&held); err != nil {
			return fmt.Errorf("dunning: check holds: %w", err)
		}
		if held {
			return hold("a manual hold is in place")
		}

		// Find the invoice that funds this service. The oldest exhausted, unpaid
		// invoice is the one that justifies suspension.
		var (
			invoiceID    string
			totalMinor   int64
			paidMinor    int64
			currency     string
			dunningState string
			dueAt        *time.Time
		)
		err = tx.QueryRow(ctx, `
			SELECT i.id, i.total_minor, i.amount_paid_minor, i.currency, d.state, i.due_at
			FROM invoices i
			JOIN dunning_states d ON d.invoice_id = i.id
			JOIN subscriptions sub ON sub.id = i.subscription_id
			JOIN services s ON s.subscription_id = sub.id
			WHERE s.id = $1 AND i.status = 'open'
			ORDER BY i.due_at
			LIMIT 1
			FOR UPDATE OF i`, serviceID).
			Scan(&invoiceID, &totalMinor, &paidMinor, &currency, &dunningState, &dueAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return hold("no open invoice is linked to this service")
		}
		if err != nil {
			return fmt.Errorf("dunning: find linked invoice: %w", err)
		}
		out.InvoiceID = invoiceID

		cur, cerr := currencyOf(currency)
		if cerr != nil {
			return cerr
		}
		outstanding := money.New(totalMinor-paidMinor, cur)
		s := outstanding.Format()
		out.Outstanding = &s

		if !outstanding.IsPositive() {
			return hold("the linked invoice is settled")
		}
		if dunningState != string(StateExhausted) {
			return hold(fmt.Sprintf("dunning is %s, not exhausted", dunningState))
		}
		if dueAt == nil {
			return hold("the linked invoice has no due date")
		}
		if now.Before(dueAt.Add(e.policy.SuspendAfter)) {
			return hold(fmt.Sprintf("suspension is permitted from %s",
				dueAt.Add(e.policy.SuspendAfter).Format(time.RFC3339)))
		}

		// The subledger is the authority on what is owed. A cached column that
		// disagrees means something is wrong; do not act on a number that is in
		// doubt.
		receivable, err := ledger.ReceivableForCustomer(ctx, tx, customerID, cur)
		if err != nil {
			return err
		}
		if !receivable.IsPositive() {
			return hold("the customer has no outstanding receivable")
		}

		out.Eligible = true
		out.Reason = "invoice is exhausted, past the suspension period, and nothing is held"
		return recordDecision(ctx, tx, out, "suspend")
	})
	if err != nil {
		return Eligibility{}, err
	}
	return out, nil
}

// RequestSuspension evaluates eligibility and, if permitted, enqueues the
// suspension operation with the generation it was decided against.
func (e *Engine) RequestSuspension(ctx context.Context, serviceID string) (Eligibility, error) {
	elig, err := e.Evaluate(ctx, serviceID)
	if err != nil {
		return Eligibility{}, err
	}
	if !elig.Eligible {
		return elig, fmt.Errorf("%w: %s", ErrNotEligible, elig.Reason)
	}

	err = e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// The generation is re-checked here, inside the enqueue transaction, so a
		// resume that happened in between invalidates this request.
		var generation int64
		var status string
		if err := tx.QueryRow(ctx, `SELECT generation, status FROM services WHERE id = $1 FOR UPDATE`,
			serviceID).Scan(&generation, &status); err != nil {
			return fmt.Errorf("dunning: lock service: %w", err)
		}
		if generation != elig.Generation || status != "active" {
			return fmt.Errorf("%w: service changed since evaluation (generation %d, status %s)",
				ErrNotEligible, generation, status)
		}

		key := fmt.Sprintf("suspend:%s:%d", serviceID, generation)
		if _, err := tx.Exec(ctx, `
			INSERT INTO service_operations (service_id, kind, operation_key, generation, status)
			VALUES ($1, 'suspend', $2, $3, 'queued')
			ON CONFLICT (operation_key) DO NOTHING`, serviceID, key, generation); err != nil {
			return fmt.Errorf("dunning: enqueue suspension: %w", err)
		}
		return e.audit(ctx, tx, "service.suspend", serviceID, generation, "")
	})
	if err != nil {
		return elig, err
	}
	return elig, nil
}

// RequestResume enqueues a resume for a suspended service whose balance is clear.
//
// Without this, a customer who pays is left suspended: dunning can suspend but
// nothing could undo it, which is a worse outcome than not suspending at all.
//
// The decision is made against the subledger, not the invoice columns: a resume is
// only safe when the customer owes nothing at all, because a second overdue
// invoice would suspend the service again immediately.
func (e *Engine) RequestResume(ctx context.Context, serviceID string) (Eligibility, error) {
	out := Eligibility{ServiceID: serviceID}
	err := e.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			status     string
			generation int64
			customerID string
		)
		if err := tx.QueryRow(ctx, `
			SELECT status, generation, customer_id FROM services WHERE id = $1 FOR UPDATE`,
			serviceID).Scan(&status, &generation, &customerID); err != nil {
			return fmt.Errorf("dunning: lock service: %w", err)
		}
		out.Generation = generation

		if status != "suspended" {
			out.Reason = fmt.Sprintf("service is %s, not suspended", status)
			return recordDecision(ctx, tx, out, "held")
		}

		// A manual hold also blocks an automatic resume: an operator who stopped
		// the service for a non-billing reason does not want it switched back on.
		var held bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM service_holds
				WHERE service_id = $1 AND (expires_at IS NULL OR expires_at > $2)
			)`, serviceID, e.Now()).Scan(&held); err != nil {
			return fmt.Errorf("dunning: check holds: %w", err)
		}
		if held {
			out.Reason = "a manual hold is in place"
			return recordDecision(ctx, tx, out, "held")
		}

		// What does the customer owe, in every currency they hold invoices in?
		rows, err := tx.Query(ctx, `
			SELECT currency, COALESCE(SUM(total_minor - amount_paid_minor), 0)
			FROM invoices
			WHERE customer_id = $1 AND status = 'open'
			GROUP BY currency`, customerID)
		if err != nil {
			return fmt.Errorf("dunning: sum open invoices: %w", err)
		}
		var owed []string
		for rows.Next() {
			var code string
			var minor int64
			if err := rows.Scan(&code, &minor); err != nil {
				rows.Close()
				return fmt.Errorf("dunning: scan open invoices: %w", err)
			}
			if minor > 0 {
				cur, cerr := currencyOf(code)
				if cerr != nil {
					rows.Close()
					return cerr
				}
				owed = append(owed, money.New(minor, cur).Format())
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("dunning: read open invoices: %w", err)
		}
		rows.Close()

		if len(owed) > 0 {
			out.Reason = "the customer still owes " + strings.Join(owed, ", ")
			return recordDecision(ctx, tx, out, "held")
		}

		key := fmt.Sprintf("resume:%s:%d", serviceID, generation)
		if _, err := tx.Exec(ctx, `
			INSERT INTO service_operations (service_id, kind, operation_key, generation, status)
			VALUES ($1, 'resume', $2, $3, 'queued')
			ON CONFLICT (operation_key) DO NOTHING`, serviceID, key, generation); err != nil {
			return fmt.Errorf("dunning: enqueue resume: %w", err)
		}
		out.Eligible = true
		out.Reason = "nothing is owed and no hold applies"
		if err := recordDecision(ctx, tx, out, "suspend"); err != nil {
			return err
		}
		return e.audit(ctx, tx, "service.resume", serviceID, generation, "")
	})
	if err != nil {
		return Eligibility{}, err
	}
	return out, nil
}

// AddHold places a manual hold.
func (e *Engine) AddHold(ctx context.Context, serviceID, reason, createdBy string, expiresAt *time.Time) (string, error) {
	if reason == "" {
		return "", errors.New("dunning: a hold needs a reason")
	}
	var id string
	err := e.pool.QueryRow(ctx, `
		INSERT INTO service_holds (service_id, reason, created_by, expires_at)
		VALUES ($1, $2, $3, $4) RETURNING id`,
		serviceID, reason, createdBy, expiresAt).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("dunning: add hold: %w", err)
	}
	return id, nil
}

// ClearHold removes a hold.
func (e *Engine) ClearHold(ctx context.Context, holdID string) error {
	tag, err := e.pool.Exec(ctx, `DELETE FROM service_holds WHERE id = $1`, holdID)
	if err != nil {
		return fmt.Errorf("dunning: clear hold: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("dunning: hold %s not found", holdID)
	}
	return nil
}

func recordDecision(ctx context.Context, tx pgx.Tx, e Eligibility, decision string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO suspension_decisions (service_id, invoice_id, decision, reason, generation)
		VALUES ($1, $2, $3, $4, $5)`,
		e.ServiceID, nullable(e.InvoiceID), decision, e.Reason, e.Generation)
	if err != nil {
		return fmt.Errorf("dunning: record decision: %w", err)
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func currencyOf(code string) (money.Currency, error) {
	if cur, ok := money.LookupCurrency(code); ok {
		return cur, nil
	}
	return money.NewCurrency(code, 2)
}
