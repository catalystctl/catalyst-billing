// Package provisioning executes service operations against an infrastructure
// adapter.
//
// The hard problem is not calling the adapter; it is what to do when the answer is
// lost. A create call that times out may have created the server. Retrying blindly
// buys a second one. Not retrying leaves a paid customer without a server.
//
// The approach:
//
//   - the operation intent is persisted before the remote call, with a stable
//     operation key that the adapter uses for idempotency;
//   - a remote timeout moves the operation to "unknown" and the reconciler asks
//     the adapter what exists, instead of retrying;
//   - every destructive transition re-checks the service generation under a row
//     lock, so a worker that started before a payment or a cancellation cannot
//     apply a stale intent;
//   - an outcome that cannot be established is surfaced for review rather than
//     guessed.
//
// Remote APIs without server-side fencing cannot make this perfect. This package
// therefore reconciles to the observed state and refuses to act on uncertainty
// about destructive operations.
package provisioning

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

var (
	// ErrNoWork is returned when there is nothing to do.
	ErrNoWork = errors.New("provisioning: no queued operations")
	// ErrStaleGeneration is returned when the service changed after the operation
	// was queued, so its intent is obsolete.
	ErrStaleGeneration = errors.New("provisioning: operation generation is stale")
	// ErrUnknownOutcome is returned when the adapter call did not produce a
	// trustworthy result (timeout, crash, connection loss).
	ErrUnknownOutcome = errors.New("provisioning: remote outcome unknown")
	// ErrUnsupportedOperation is returned for an operation kind the adapter does
	// not implement.
	ErrUnsupportedOperation = errors.New("provisioning: unsupported operation")
)

// Adapter is the infrastructure side of an extension.
type Adapter interface {
	Name() string
	Capabilities() []string
	Create(ctx context.Context, req CreateRequest) (extproto.ProvisionCreateResult, error)
	// Lookup answers authoritatively whether a resource exists for an operation
	// key. Reconciliation depends on it, because a create replay only works while
	// the adapter still remembers the key.
	Lookup(ctx context.Context, operationKey string) (extproto.ProvisionLookupResult, error)
	Inspect(ctx context.Context, externalID string) (extproto.ProvisionInspectResult, error)
	Suspend(ctx context.Context, externalID, reason string) (extproto.ProvisionStateResult, error)
	Resume(ctx context.Context, externalID string) (extproto.ProvisionStateResult, error)
	Terminate(ctx context.Context, externalID, reason string) (extproto.ProvisionStateResult, error)
}

// CreateRequest is the adapter input for a create operation.
type CreateRequest struct {
	OperationKey   string
	ServiceID      string
	SubscriptionID string
	PlanID         string
	Region         string
	Quantity       int32
	Metadata       map[string]string
}

// Operation is a claimed unit of work.
type Operation struct {
	ID           string
	ServiceID    string
	Kind         string
	OperationKey string
	Generation   int64
	Status       string
	Attempts     int
	// Service state at claim time.
	ServiceStatus   string
	ServiceExternal string
	ProductID       string
	CustomerID      string
	Config          map[string]any
}

// Worker executes provisioning operations.
type Worker struct {
	pool    *store.Pool
	adapter Adapter
	// MaxAttempts bounds retries of a failing operation before it is parked for
	// review. A provisioning call that fails deterministically must not retry
	// forever and mask the problem.
	MaxAttempts int
	// CallTimeout bounds one adapter call. It is shorter than LeaseDuration so a
	// hung call is classified as unknown before the lease expires.
	CallTimeout time.Duration
	// LeaseDuration is how long a claim is honoured. A worker that dies mid-call
	// leaves the row 'running' with an expired lease, and Reap returns it to the
	// queue instead of leaving it wedged forever.
	LeaseDuration time.Duration
	// Now is injectable for tests.
	Now func() time.Time
}

// NewWorker constructs a Worker.
func NewWorker(pool *store.Pool, adapter Adapter) *Worker {
	return &Worker{
		pool:          pool,
		adapter:       adapter,
		MaxAttempts:   5,
		CallTimeout:   30 * time.Second,
		LeaseDuration: 5 * time.Minute,
		Now:           func() time.Time { return time.Now().UTC() },
	}
}

// Result reports what happened to one operation.
type Result struct {
	OperationID string
	ServiceID   string
	Kind        string
	// Outcome is one of: succeeded, retry_wait, unknown, needs_review, failed.
	Outcome    string
	ExternalID string
	Detail     string
}

// RunOnce claims and executes at most one queued or retryable operation.
func (w *Worker) RunOnce(ctx context.Context) (Result, error) {
	op, err := w.claim(ctx)
	if err != nil {
		return Result{}, err
	}
	return w.execute(ctx, op)
}

// Run processes up to limit operations, stopping when there is no work.
func (w *Worker) Run(ctx context.Context, limit int) ([]Result, error) {
	var results []Result
	for i := 0; i < limit; i++ {
		res, err := w.RunOnce(ctx)
		if errors.Is(err, ErrNoWork) {
			return results, nil
		}
		if err != nil {
			return results, err
		}
		results = append(results, res)
	}
	return results, nil
}

// claim selects one operation and marks it running, in one transaction.
//
// FOR UPDATE SKIP LOCKED is what lets several workers run without coordinating:
// each takes a different row, and a row already being worked on is invisible
// rather than blocking.
func (w *Worker) claim(ctx context.Context) (Operation, error) {
	var op Operation
	err := w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT o.id, o.service_id, o.kind, o.operation_key, o.generation, o.status, o.attempts,
			       s.status, COALESCE(s.external_id, ''), s.product_id, s.customer_id, s.config
			FROM service_operations o
			JOIN services s ON s.id = o.service_id
			WHERE o.status IN ('queued', 'retry_wait')
		   OR (o.status = 'running' AND o.started_at < $1)
			ORDER BY o.created_at
			FOR UPDATE OF o SKIP LOCKED
			LIMIT 1`, w.Now().Add(-w.LeaseDuration)).
			Scan(&op.ID, &op.ServiceID, &op.Kind, &op.OperationKey, &op.Generation, &op.Status,
				&op.Attempts, &op.ServiceStatus, &op.ServiceExternal, &op.ProductID,
				&op.CustomerID, &op.Config)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoWork
		}
		if err != nil {
			return fmt.Errorf("provisioning: claim operation: %w", err)
		}
		_, err = tx.Exec(ctx, `
			UPDATE service_operations
			SET status = 'running', started_at = $2, attempts = attempts + 1
			WHERE id = $1`, op.ID, w.Now())
		if err != nil {
			return fmt.Errorf("provisioning: mark operation running: %w", err)
		}
		return nil
	})
	if err != nil {
		return Operation{}, err
	}
	op.Attempts++
	return op, nil
}

// execute performs the remote call and records the outcome.
func (w *Worker) execute(ctx context.Context, op Operation) (Result, error) {
	res := Result{OperationID: op.ID, ServiceID: op.ServiceID, Kind: op.Kind}

	// Fencing: the operation's intent is only valid for the service generation it
	// was queued against. A stale worker must not suspend a service that has since
	// been resumed or terminated.
	current, err := w.serviceGeneration(ctx, op.ServiceID)
	if err != nil {
		return res, err
	}
	if current != op.Generation {
		res.Outcome = "needs_review"
		res.Detail = fmt.Sprintf("service generation moved from %d to %d while the operation was queued",
			op.Generation, current)
		return res, w.finish(ctx, op, res)
	}

	callCtx, cancel := context.WithTimeout(ctx, w.CallTimeout)
	defer cancel()

	switch op.Kind {
	case "create":
		if op.ServiceExternal != "" {
			// Already provisioned: reconcile instead of buying again.
			res.Outcome = "succeeded"
			res.ExternalID = op.ServiceExternal
			res.Detail = "service already has an external id; create treated as a replay"
			return res, w.finish(ctx, op, res)
		}
		out, err := w.adapter.Create(callCtx, CreateRequest{
			OperationKey: op.OperationKey,
			ServiceID:    op.ServiceID,
			PlanID:       op.planID(),
			Region:       op.region(),
			Quantity:     1,
			Metadata:     map[string]string{"service_id": op.ServiceID, "customer_id": op.CustomerID},
		})
		if err != nil {
			return w.recordFailure(ctx, op, res, err)
		}
		if out.ExternalID == "" {
			// "Provisioned" with no resource id is not provisioned: every later
			// suspend or terminate would call the adapter with an empty target.
			res.Outcome = "needs_review"
			res.Detail = "adapter reported success but returned no external id"
			return res, w.finish(ctx, op, res)
		}
		res.Outcome = "succeeded"
		res.ExternalID = out.ExternalID
		return res, w.finish(ctx, op, res)

	case "inspect":
		out, err := w.adapter.Inspect(callCtx, op.ServiceExternal)
		if err != nil {
			return w.recordFailure(ctx, op, res, err)
		}
		res.Outcome = "succeeded"
		res.ExternalID = out.ExternalID
		res.Detail = string(out.Status)
		return res, w.finish(ctx, op, res)

	case "suspend":
		out, err := w.adapter.Suspend(callCtx, op.ServiceExternal, "billing")
		if err != nil {
			return w.recordFailure(ctx, op, res, err)
		}
		res.Outcome = "succeeded"
		res.ExternalID = out.ExternalID
		return res, w.finish(ctx, op, res)

	case "resume":
		out, err := w.adapter.Resume(callCtx, op.ServiceExternal)
		if err != nil {
			return w.recordFailure(ctx, op, res, err)
		}
		res.Outcome = "succeeded"
		res.ExternalID = out.ExternalID
		return res, w.finish(ctx, op, res)

	case "terminate":
		out, err := w.adapter.Terminate(callCtx, op.ServiceExternal, "terminated")
		if err != nil {
			return w.recordFailure(ctx, op, res, err)
		}
		res.Outcome = "succeeded"
		res.ExternalID = out.ExternalID
		return res, w.finish(ctx, op, res)

	default:
		res.Outcome = "failed"
		res.Detail = fmt.Sprintf("unsupported operation kind %q", op.Kind)
		return res, w.finish(ctx, op, res)
	}
}

// recordFailure classifies an adapter error and returns the updated result.
//
// It returns the Result rather than mutating one in place: Go passes structs by
// value, so an earlier version of this function classified the error correctly
// and then handed the caller an empty outcome, which looked like "nothing
// happened" to every caller and to the audit trail.
//
// The distinction that matters: an error that means "we do not know whether it
// happened" must never be retried as if nothing happened. It becomes "unknown" and
// waits for reconciliation.
func (w *Worker) recordFailure(ctx context.Context, op Operation, res Result, cause error) (Result, error) {
	switch {
	case errors.Is(cause, context.DeadlineExceeded), errors.Is(cause, context.Canceled),
		errors.Is(cause, extproto.ErrTimeout), errors.Is(cause, extproto.ErrCanceled):
		res.Outcome = "unknown"
		res.Detail = "adapter call did not answer: " + cause.Error()
		return res, w.finish(ctx, op, res)
	case errors.Is(cause, extproto.ErrCrashed), errors.Is(cause, extproto.ErrProtocol):
		// The extension died mid-call: the remote effect may or may not exist.
		res.Outcome = "unknown"
		res.Detail = "adapter failed during the call: " + cause.Error()
		return res, w.finish(ctx, op, res)
	}

	if op.Attempts >= w.MaxAttempts {
		res.Outcome = "failed"
		res.Detail = fmt.Sprintf("gave up after %d attempts: %v", op.Attempts, cause)
		return res, w.finish(ctx, op, res)
	}
	res.Outcome = "retry_wait"
	res.Detail = cause.Error()
	return res, w.finish(ctx, op, res)
}

// planID and region read the adapter's target from the service configuration,
// which was captured when the service was created. The operation key is not a
// plan: conflating identity with configuration produced an unprovidable request.
func (o Operation) planID() string {
	if v, ok := o.Config["plan_id"].(string); ok {
		return v
	}
	return ""
}

func (o Operation) region() string {
	if v, ok := o.Config["region"].(string); ok {
		return v
	}
	return ""
}

// finish writes the operation outcome and the service transition atomically.
func (w *Worker) finish(ctx context.Context, op Operation, res Result) error {
	return w.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Re-check the generation inside the writing transaction: the remote call
		// took time, and the service may have changed while it was in flight.
		var generation int64
		if err := tx.QueryRow(ctx, `SELECT generation FROM services WHERE id = $1 FOR UPDATE`,
			op.ServiceID).Scan(&generation); err != nil {
			return fmt.Errorf("provisioning: lock service: %w", err)
		}
		if generation != op.Generation && res.Outcome == "succeeded" {
			// The effect happened, but the intent is obsolete. Record it and let
			// reconciliation decide; do not apply a state transition.
			res.Outcome = "needs_review"
			res.Detail = fmt.Sprintf("service generation moved from %d to %d during the call",
				op.Generation, generation)
		}

		status := map[string]string{
			"succeeded":    "succeeded",
			"retry_wait":   "retry_wait",
			"unknown":      "unknown",
			"needs_review": "needs_review",
			"failed":       "failed",
		}[res.Outcome]
		if status == "" {
			status = "failed"
		}

		_, err := tx.Exec(ctx, `
			UPDATE service_operations
			SET status = $2, last_error = NULLIF($3, ''), finished_at = CASE WHEN $2 IN ('succeeded','failed','needs_review') THEN now() ELSE NULL END
			WHERE id = $1`, op.ID, status, res.Detail)
		if err != nil {
			return fmt.Errorf("provisioning: update operation: %w", err)
		}

		if res.Outcome != "succeeded" {
			return nil
		}

		// Apply the service transition the operation implied.
		switch op.Kind {
		case "create":
			if _, err := tx.Exec(ctx, `
				UPDATE services
				SET status = 'active', external_id = $2, updated_at = now()
				WHERE id = $1`, op.ServiceID, res.ExternalID); err != nil {
				return fmt.Errorf("provisioning: activate service: %w", err)
			}
			if err := advanceOrder(ctx, tx, op.ServiceID); err != nil {
				return err
			}
		case "suspend":
			if _, err := tx.Exec(ctx, `
				UPDATE services SET status = 'suspended', updated_at = now() WHERE id = $1`,
				op.ServiceID); err != nil {
				return fmt.Errorf("provisioning: mark service suspended: %w", err)
			}
		case "resume":
			if _, err := tx.Exec(ctx, `
				UPDATE services SET status = 'active', updated_at = now() WHERE id = $1`,
				op.ServiceID); err != nil {
				return fmt.Errorf("provisioning: mark service active: %w", err)
			}
		case "terminate":
			if _, err := tx.Exec(ctx, `
				UPDATE services SET status = 'terminated', updated_at = now() WHERE id = $1`,
				op.ServiceID); err != nil {
				return fmt.Errorf("provisioning: mark service terminated: %w", err)
			}
		}
		return nil
	})
}

// advanceOrder moves the order behind a service along its own lifecycle.
//
// The order status is derived, not remembered: a paid order becomes `fulfilling` as
// soon as its first service is provisioned, and `completed` once no operation for any
// of its services is still open. Doing it here, in the same transaction as the service
// transition, means the operator's order screen cannot disagree with reality. The
// transitions are conditional so a late operation cannot resurrect a cancelled order.
func advanceOrder(ctx context.Context, tx pgx.Tx, serviceID string) error {
	// A service reaches its order through the invoice that paid for it: the invoice
	// carries both the subscription and the order.
	const ordersForService = `
		SELECT DISTINCT i.order_id
		FROM invoices i
		JOIN services s ON s.subscription_id = i.subscription_id
		WHERE s.id = $1 AND i.order_id IS NOT NULL`

	if _, err := tx.Exec(ctx, `
		UPDATE orders SET status = 'fulfilling', updated_at = now(), version = version + 1
		WHERE status = 'paid' AND id IN (`+ordersForService+`)`, serviceID); err != nil {
		return fmt.Errorf("provisioning: mark order fulfilling: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE orders SET status = 'completed', updated_at = now(), version = version + 1
		WHERE status = 'fulfilling' AND id IN (`+ordersForService+`)
		  AND NOT EXISTS (
			SELECT 1
			FROM service_operations so
			JOIN services s2 ON s2.id = so.service_id
			JOIN invoices i2 ON i2.subscription_id = s2.subscription_id
			WHERE i2.order_id = orders.id
			  AND so.status IN ('queued', 'running', 'retry_wait', 'unknown', 'needs_review')
		  )`, serviceID); err != nil {
		return fmt.Errorf("provisioning: mark order completed: %w", err)
	}
	return nil
}

// serviceGeneration reads the fencing token.
func (w *Worker) serviceGeneration(ctx context.Context, serviceID string) (int64, error) {
	var gen int64
	if err := w.pool.QueryRow(ctx, `SELECT generation FROM services WHERE id = $1`, serviceID).
		Scan(&gen); err != nil {
		return 0, fmt.Errorf("provisioning: read service generation: %w", err)
	}
	return gen, nil
}

// Reap returns operations whose lease expired to the queue.
//
// A worker that dies between claim and finish leaves a row 'running'. Without
// this, that row is invisible to Run (which claims queued/retry_wait) and to
// Reconcile (which reads unknown), so the customer is wedged forever and the
// dashboard shows nothing wrong. Reaping is bounded and records why it acted.
func (w *Worker) Reap(ctx context.Context) (int, error) {
	cutoff := w.Now().Add(-w.LeaseDuration)
	tag, err := w.pool.Exec(ctx, `
		UPDATE service_operations
		SET status = 'retry_wait',
		    last_error = 'lease expired: the worker stopped mid-call, so the outcome is unknown',
		    finished_at = NULL
		WHERE status = 'running' AND started_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("provisioning: reap expired leases: %w", err)
	}
	n := int(tag.RowsAffected())
	if n > 0 {
		// These operations are re-run with the same operation key, so the adapter's
		// idempotency (or its authoritative lookup) prevents a duplicate.
		return n, nil
	}
	return 0, nil
}

// Reconcile resolves operations whose remote outcome is unknown.
//
// For a create, the adapter is asked what exists for the operation key; if the
// resource is there the operation becomes succeeded, and if it is not the
// operation is requeued. If the adapter cannot answer, the operation stays in
// review: guessing either way risks a duplicate server or a lost customer.
func (w *Worker) Reconcile(ctx context.Context, limit int) ([]Result, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := w.pool.Query(ctx, `
		SELECT o.id, o.service_id, o.kind, o.operation_key, o.generation, o.status, o.attempts,
		       s.status, COALESCE(s.external_id, ''), s.product_id, s.customer_id, s.config
		FROM service_operations o
		JOIN services s ON s.id = o.service_id
		WHERE o.status = 'unknown'
		ORDER BY o.created_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("provisioning: list unknown operations: %w", err)
	}
	var ops []Operation
	for rows.Next() {
		var op Operation
		if err := rows.Scan(&op.ID, &op.ServiceID, &op.Kind, &op.OperationKey, &op.Generation,
			&op.Status, &op.Attempts, &op.ServiceStatus, &op.ServiceExternal, &op.ProductID,
			&op.CustomerID, &op.Config); err != nil {
			rows.Close()
			return nil, fmt.Errorf("provisioning: scan unknown operation: %w", err)
		}
		ops = append(ops, op)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("provisioning: list unknown operations: %w", err)
	}
	rows.Close()

	var results []Result
	for _, op := range ops {
		res, err := w.reconcileOne(ctx, op)
		if err != nil {
			results = append(results, Result{
				OperationID: op.ID, ServiceID: op.ServiceID, Kind: op.Kind,
				Outcome: "needs_review", Detail: err.Error(),
			})
			continue
		}
		results = append(results, res)
	}
	return results, nil
}

// reconcileOne re-derives the truth for a single operation.
func (w *Worker) reconcileOne(ctx context.Context, op Operation) (Result, error) {
	res := Result{OperationID: op.ID, ServiceID: op.ServiceID, Kind: op.Kind}

	// The service may already carry the external id: the create succeeded and only
	// the reply was lost.
	if op.ServiceExternal != "" {
		res.Outcome = "succeeded"
		res.ExternalID = op.ServiceExternal
		res.Detail = "reconciled from the service record"
		return res, w.finish(ctx, op, res)
	}
	if op.Kind != "create" {
		// Destructive and state operations cannot be reconciled without an
		// external id; a human must look.
		res.Outcome = "needs_review"
		res.Detail = "cannot reconcile without an external resource id"
		return res, w.finish(ctx, op, res)
	}

	// Ask the adapter whether a resource exists for this operation key. This is a
	// read, not a create: replaying create would provision a second server
	// whenever the adapter no longer remembers the key, which is exactly the
	// situation that produced the unknown outcome in the first place.
	callCtx, cancel := context.WithTimeout(ctx, w.CallTimeout)
	defer cancel()
	found, err := w.adapter.Lookup(callCtx, op.OperationKey)
	if err != nil {
		// The adapter cannot answer. That is not "it does not exist", so the
		// operation stays in review rather than being retried into a duplicate.
		res.Outcome = "needs_review"
		res.Detail = "lookup failed, so the remote state is still unknown: " + err.Error()
		return res, w.finish(ctx, op, res)
	}
	if !found.Found {
		// The adapter is certain nothing exists: the create never took effect, so
		// it is safe to try again with the same key.
		res.Outcome = "retry_wait"
		res.Detail = "adapter confirms no resource exists for this operation key"
		return res, w.finish(ctx, op, res)
	}
	if found.ExternalID == "" {
		// Exists but unidentifiable: a human must resolve it, because a later
		// suspend or terminate would have no target.
		res.Outcome = "needs_review"
		res.Detail = "adapter reports a resource exists but returned no external id"
		return res, w.finish(ctx, op, res)
	}
	res.Outcome = "succeeded"
	res.ExternalID = found.ExternalID
	res.Detail = "reconciled by authoritative lookup"
	return res, w.finish(ctx, op, res)
}
