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
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
	"strings"
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
	// CustomerEmail and CustomerName identify the person behind the service to a provider that
	// needs an account of its own for them. They are read at claim time rather than stored on the
	// service, because a customer who changes their email address must not keep being provisioned
	// under the old one.
	CustomerEmail string
	CustomerName  string
	Config        map[string]any
	// ServerID, ServerRegion and ServerNode are the product's provisioner target as it
	// stood when the operation was claimed. The region is a *fallback*: a price version
	// that names its own region still wins, so an existing plan keeps provisioning
	// exactly where it did. See CreateRequest.Region below.
	ServerID     string
	ServerRegion string
	ServerNode   string
	// ExternalSubscription is the provider-side subscription id the service carries, read at
	// claim time so a cancellation queued before the id was recorded still finds it.
	ExternalSubscription string
	// Payload is the instruction this operation was queued with, e.g. the plan a resize
	// must reach. It is read from the row rather than derived from the service, because a
	// second upgrade between the enqueue and the claim would otherwise be applied under the
	// first operation's identity.
	Payload map[string]any
	// ConfigEnv is the service's config-option selection as one environment variable per
	// option that names one. It is read at claim time so an option changed after the
	// operation was queued is still delivered at the next call, and it is what turns
	// "8 GB" from a line on an invoice into a real environment variable on the server
	// (SVC-21, UPG-10).
	ConfigEnv map[string]string
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
	// Upgrader, when set, is what a resize operation calls. Nil means "launch the provisioner
	// this panel has configured", which is what production does: the Adapter cmd/billing builds
	// is one long-lived client whose interface predates provisioner.upgrade, and a queue row
	// must still be able to resize.
	Upgrader Upgrader
	// Canceller, when set, is what a subscription-cancellation operation calls. Nil means
	// "launch the configured gateway", because a card subscription belongs to the gateway and
	// not to the hosting panel.
	Canceller SubscriptionCanceller
	// notify is the outbox the activation message is written to. It is optional: a worker with
	// no queue still provisions, it just does not tell the customer. Nil is also what a test
	// gets, so the provisioning tests are unaffected by the notification feature.
	notify *notify.Queue
}

// WithNotifier attaches the notification outbox.
//
// It is a builder rather than a constructor argument so the existing call sites keep working and
// so "no notifications" stays the default: a missing notifier must not stop a paid customer's
// server from being created, which is the failure this package exists to avoid.
func (w *Worker) WithNotifier(q *notify.Queue) *Worker {
	w.notify = q
	return w
}

// DefaultLeaseDuration is how long a claim is honoured when a Worker is constructed with
// NewWorker.
//
// It is exported because other packages need to explain a claim to an operator: the
// operation resource reports when a lease expires, and computing that from a
// per-instance field would make the answer depend on which process served the request.
const DefaultLeaseDuration = 5 * time.Minute

// NewWorker constructs a Worker.
func NewWorker(pool *store.Pool, adapter Adapter) *Worker {
	return &Worker{
		pool:          pool,
		adapter:       adapter,
		MaxAttempts:   5,
		CallTimeout:   30 * time.Second,
		LeaseDuration: DefaultLeaseDuration,
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
			       o.payload,
			       s.status, COALESCE(s.external_id, ''), s.product_id, s.customer_id, s.config,
			       COALESCE(srv.id::text, ''), COALESCE(srv.region, ''), COALESCE(srv.node, ''),
			       COALESCE(s.external_subscription_id, ''),
			       COALESCE(c.email, ''), COALESCE(c.name, '')
			FROM service_operations o
			JOIN services s ON s.id = o.service_id
			LEFT JOIN customers c ON c.id = s.customer_id
			LEFT JOIN products p ON p.id = s.product_id
			LEFT JOIN servers srv ON srv.id = p.server_id
			WHERE o.status IN ('queued', 'retry_wait')
		   OR (o.status = 'running' AND o.started_at < $1)
			ORDER BY o.created_at
			FOR UPDATE OF o SKIP LOCKED
			LIMIT 1`, w.Now().Add(-w.LeaseDuration)).
			Scan(&op.ID, &op.ServiceID, &op.Kind, &op.OperationKey, &op.Generation, &op.Status,
				&op.Attempts, &op.Payload, &op.ServiceStatus, &op.ServiceExternal, &op.ProductID,
				&op.CustomerID, &op.Config, &op.ServerID, &op.ServerRegion, &op.ServerNode,
				&op.ExternalSubscription, &op.CustomerEmail, &op.CustomerName)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoWork
		}
		if err != nil {
			return fmt.Errorf("provisioning: claim operation: %w", err)
		}
		if op.Payload == nil {
			// A row written before 0063, or by a caller with no instruction beyond the kind.
			op.Payload = map[string]any{}
		}
		// The selection is read here rather than joined into the claim query so a duplicate
		// env_variable across two options cannot make the aggregate fail and stall the queue.
		if op.ConfigEnv, err = contractEnv(ctx, tx, op.ServiceID); err != nil {
			return err
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
			Metadata:     op.metadata(),
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

	case "resize":
		// A service that was never provisioned has no remote resource to resize, and saying so
		// is the truthful outcome: the ledger moved and there was nothing to tell.
		if op.ServiceExternal == "" {
			res.Outcome = "succeeded"
			res.Detail = "the service has no remote resource, so there was nothing to resize"
			return res, w.finish(ctx, op, res)
		}
		upg, err := w.upgrader()
		if err != nil {
			// "No adapter advertises provisioner.upgrade" and "no provisioner is configured"
			// do not improve by being attempted again in five seconds, and five quick
			// failures would park the row with a message about attempts instead of the one
			// fact an operator can act on.
			res.Outcome = "needs_review"
			res.Detail = err.Error()
			return res, w.finish(ctx, op, res)
		}
		out, err := upg.Upgrade(callCtx, UpgradeRequest{
			OperationKey: op.OperationKey,
			ExternalID:   op.ServiceExternal,
			ServiceID:    op.ServiceID,
			PlanID:       op.targetPlan(),
			Region:       op.targetRegion(),
			Quantity:     op.quantity(),
			Metadata:     op.metadata(),
		})
		if err != nil {
			if unsupported(err) {
				res.Outcome = "needs_review"
				res.Detail = err.Error()
				return res, w.finish(ctx, op, res)
			}
			return w.recordFailure(ctx, op, res, err)
		}
		res.Outcome = "succeeded"
		res.ExternalID = out.ExternalID
		if res.ExternalID == "" {
			res.ExternalID = op.ServiceExternal
		}
		res.Detail = fmt.Sprintf("resized to plan %q (changed=%t)", op.targetPlan(), out.Changed)
		return res, w.finish(ctx, op, res)

	case "delete_remote":
		// The operator's "the server is gone from the panel, the customer keeps the service".
		// It is a terminate at the provider and nothing else: the service row, its billing and
		// its status are untouched, which is what makes it different from terminating.
		if op.ServiceExternal == "" {
			res.Outcome = "succeeded"
			res.Detail = "the service has no remote resource, so there was nothing to delete"
			return res, w.finish(ctx, op, res)
		}
		out, err := w.adapter.Terminate(callCtx, op.ServiceExternal, "delete_remote")
		if err != nil {
			return w.recordFailure(ctx, op, res, err)
		}
		res.Outcome = "succeeded"
		res.ExternalID = op.ServiceExternal
		res.Detail = "remote resource deleted: " + string(out.Status)
		return res, w.finish(ctx, op, res)

	case "cancel_subscription":
		subscriptionID := op.targetSubscription()
		if subscriptionID == "" {
			res.Outcome = "needs_review"
			res.Detail = "the service has no external subscription id to cancel"
			return res, w.finish(ctx, op, res)
		}
		cancel, err := w.canceller()
		if err != nil {
			res.Outcome = "needs_review"
			res.Detail = err.Error()
			return res, w.finish(ctx, op, res)
		}
		out, err := cancel.CancelSubscription(callCtx, CancelSubscriptionRequest{
			OperationKey:   op.OperationKey,
			SubscriptionID: subscriptionID,
			Reason:         op.reason(),
		})
		if err != nil {
			if unsupported(err) {
				res.Outcome = "needs_review"
				res.Detail = err.Error()
				return res, w.finish(ctx, op, res)
			}
			return w.recordFailure(ctx, op, res, err)
		}
		res.Outcome = "succeeded"
		res.ExternalID = out.SubscriptionID
		res.Detail = fmt.Sprintf("subscription %s: status %q, cancelled=%t", subscriptionID, out.Status, out.Canceled)
		if out.AlreadyGone {
			// The provider has no such subscription. The panel's record is wrong rather than
			// the call failed, and an operator should see that instead of a green tick.
			res.Outcome = "needs_review"
			res.Detail = fmt.Sprintf("the provider does not know subscription %s", subscriptionID)
		}
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

// region is where the adapter is told to provision.
//
// The order is deliberate and it is the whole of the product → server feature on this side:
// a price version that names a region wins, because that is what it did before 0049 and a plan
// must keep provisioning exactly where it did. When the plan names none, the product's server
// supplies one. A product with no server — every product before 0049 — therefore produces an
// empty region, exactly as before.
//
// There is no second lookup and no second field: this is the same CreateRequest.Region the
// price version has always filled, and the panel's server selector is the only new way to fill
// it. It is read at claim time rather than at queue time so a product's server is honoured for
// an operation queued before the assignment; the value is metadata about *where*, never
// evidence about *whether*, so re-reading it cannot break fencing.
func (o Operation) region() string {
	if v, ok := o.Config["region"].(string); ok && v != "" {
		return v
	}
	return o.ServerRegion
}

// metadata is what the adapter is told about the resource, beyond its target. The product's
// server is named here as well as in Region because the providers that address a target by node
// id read the node from the environment rather than from the region label.
//
// Two families of entry ride along, and both exist because the adapter is the only component
// that can act on them:
//
//   - `env.NAME=value` is the service's config-option selection. Pterodactyl passes it to the
//     egg's startup environment verbatim, so a "Minecraft version" option becomes a real
//     variable on the server without the core knowing what it means (SVC-21, UPG-10).
//   - an entry supplied by the service extension's own `provisioner.fields` (a panel user id, a
//     domain name) is passed under its own key, because that is the key the adapter declared
//     and the key its create call reads (UPG-12).
func (o Operation) metadata() map[string]string {
	out := map[string]string{"service_id": o.ServiceID, "customer_id": o.CustomerID}
	// The identity of the person the resource belongs to. A provider that hosts servers per user —
	// a game panel, a VPS platform — has to create an account for them, and an account needs an
	// address. An id is not enough, and asking the adapter to look it up would mean the adapter
	// reading this database.
	if o.CustomerEmail != "" {
		out["customer_email"] = o.CustomerEmail
	}
	if o.CustomerName != "" {
		out["customer_name"] = o.CustomerName
	}
	if o.ServerID != "" {
		out["server_id"] = o.ServerID
	}
	if o.ServerNode != "" {
		out["server_node"] = o.ServerNode
	}
	for name, value := range o.ConfigEnv {
		if name == "" {
			continue
		}
		out["env."+name] = value
	}
	for key, value := range o.fields() {
		// The bookkeeping keys above win: an extension field may not rewrite which service
		// this operation is about.
		if _, taken := out[key]; taken {
			continue
		}
		out[key] = value
	}
	return out
}

// fields is the extension-supplied provisioning input stored on the service.
//
// It lives in `services.config` rather than in `custom_properties` because these values are the
// provisioner's own inputs, not operator-defined labels: they are sent to the adapter verbatim
// and must not be rendered as service metadata or edited by a customer.
func (o Operation) fields() map[string]string {
	raw, ok := o.Config["fields"].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for key, value := range raw {
		if text, ok := value.(string); ok && key != "" {
			out[key] = text
		}
	}
	return out
}

// contractEnv reads the service's config-option selection as environment entries.
//
// An option with no env_variable is a priced choice with nothing to tell the server, so it is
// skipped here and still priced by the renewal engine.
func contractEnv(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, serviceID string) (map[string]string, error) {
	rows, err := q.Query(ctx, `
		SELECT o.env_variable, sc.value
		FROM service_configs sc
		JOIN config_options o ON o.id = sc.config_option_id
		WHERE sc.service_id = $1 AND o.env_variable <> ''
		ORDER BY o.sort_order, o.name`, serviceID)
	if err != nil {
		return nil, fmt.Errorf("provisioning: read service config options: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return nil, fmt.Errorf("provisioning: scan service config option: %w", err)
		}
		out[name] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("provisioning: read service config options: %w", err)
	}
	return out, nil
}

// targetPlan is the plan a resize must reach.
//
// The payload wins, because it is the upgrade's own record of what was paid for. The service
// configuration is the fallback for a resize queued by a caller that had no plan to name (a
// plain price change), where the plan the service already has is exactly right.
func (o Operation) targetPlan() string {
	if v, ok := o.Payload["plan_id"].(string); ok && v != "" {
		return v
	}
	return o.planID()
}

// targetRegion is where a resize must land.
func (o Operation) targetRegion() string {
	if v, ok := o.Payload["region"].(string); ok && v != "" {
		return v
	}
	return o.region()
}

// targetSubscription is the provider-side subscription a cancellation must end.
func (o Operation) targetSubscription() string {
	if v, ok := o.Payload["subscription_id"].(string); ok && v != "" {
		return v
	}
	return o.ExternalSubscription
}

// suspensionReason is what the customer is told about a suspension.
//
// The operator's stated reason when there is one, and a sentence that names the usual cause
// otherwise: "the service was suspended" with no explanation is the message that produces a
// support ticket, and an empty reason renders as a hole in the template.
func suspensionReason(o Operation) string {
	if r := strings.TrimSpace(o.reason()); r != "" {
		return r
	}
	return "the service was suspended, usually because an invoice is overdue"
}

// reason is the operator's stated reason, for the adapter and the audit trail.
func (o Operation) reason() string {
	if v, ok := o.Payload["reason"].(string); ok {
		return v
	}
	return ""
}

// quantity is how many units the resource covers. It defaults to one because every row
// written before the payload existed meant one.
func (o Operation) quantity() int32 {
	switch v := o.Payload["quantity"].(type) {
	case float64:
		if v > 0 && v <= 1000 {
			return int32(v)
		}
	case int32:
		if v > 0 {
			return v
		}
	case int64:
		if v > 0 && v <= 1000 {
			return int32(v)
		}
	}
	return 1
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
			// Tell the customer, with the product's own instructions (SF-11). It runs inside this
			// transaction so the activation mail exists if and only if the service really became
			// active, and its dedupe key is the service id so a retried operation does not send a
			// second copy. A notifier that is not configured is a no-op; a notification that fails
			// rolls the transition back and the operation is retried, which is the same treatment
			// the invoice-paid message gets in internal/fulfilment.
			if w.notify != nil {
				if _, err := notify.ServiceActivated(ctx, tx, w.notify, op.ServiceID); err != nil {
					return fmt.Errorf("provisioning: notify activation: %w", err)
				}
			}
		case "suspend":
			if _, err := tx.Exec(ctx, `
				UPDATE services SET status = 'suspended', updated_at = now() WHERE id = $1`,
				op.ServiceID); err != nil {
				return fmt.Errorf("provisioning: mark service suspended: %w", err)
			}
			// Tell the customer, in the same transaction as the status change: a suspension that
			// committed without its notice is a customer who finds out when their site goes
			// down. The dedupe key includes the suspension time, so a suspension, a resume and a
			// second suspension are two messages rather than one suppressed as a duplicate.
			if w.notify != nil {
				if _, err := notify.ServiceSuspended(ctx, tx, w.notify, op.ServiceID,
					suspensionReason(op), w.Now()); err != nil {
					return fmt.Errorf("provisioning: notify suspension: %w", err)
				}
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
		case "resize":
			// The service now *is* the plan the operation was queued for, so the row must say
			// so: a later create, inspect or second resize reads the plan from here, and a
			// service whose configuration still named the old plan would be re-created at the
			// size the customer already upgraded away from.
			patch, err := resizePatch(op)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE services SET config = config || $2::jsonb, updated_at = now() WHERE id = $1`,
				op.ServiceID, patch); err != nil {
				return fmt.Errorf("provisioning: record resized service config: %w", err)
			}
		case "delete_remote":
			// The remote resource is gone, so the id that named it is gone too. The status is
			// deliberately untouched: the customer still holds the service.
			if _, err := tx.Exec(ctx, `
				UPDATE services SET external_id = NULL, updated_at = now() WHERE id = $1`,
				op.ServiceID); err != nil {
				return fmt.Errorf("provisioning: clear remote service id: %w", err)
			}
		case "cancel_subscription":
			// The provider-side record is cancelled, so the panel must stop pointing at it.
			// The id itself is kept in the audit row rather than in the column: a column that
			// still names a cancelled subscription is an invitation to cancel it twice.
			if _, err := tx.Exec(ctx, `
				UPDATE services SET external_subscription_id = '', updated_at = now() WHERE id = $1`,
				op.ServiceID); err != nil {
				return fmt.Errorf("provisioning: clear external subscription id: %w", err)
			}
		}

		// The three operator-initiated kinds are audited here, in the transaction that applied
		// them, because each one changes something a customer paid for and each one is a call
		// an operator made rather than a schedule. The row is written with the system actor: a
		// worker has no session, and recording it as a user would make the trail lie about who
		// acted.
		if action := auditedKind(op.Kind); action != "" {
			changes := map[string]any{
				"operation_id": op.ID,
				"operation":    op.Kind,
			}
			switch op.Kind {
			case "resize":
				changes["plan_id"] = op.targetPlan()
				changes["region"] = op.targetRegion()
			case "delete_remote":
				changes["external_id"] = op.ServiceExternal
			case "cancel_subscription":
				changes["external_subscription_id"] = op.targetSubscription()
				changes["reason"] = op.reason()
			}
			if err := audit.Record(ctx, tx, audit.Event{
				Actor:      audit.Actor{Kind: audit.ActorSystem},
				Action:     action,
				TargetType: "service",
				TargetID:   op.ServiceID,
				Outcome:    audit.OutcomeSuccess,
				Changes:    changes,
			}); err != nil {
				return fmt.Errorf("provisioning: audit %s: %w", op.Kind, err)
			}
		}
		return nil
	})
}

// unsupported reports whether an error means "this installation cannot do this".
//
// Such an operation is parked for review instead of retried: an adapter that does not implement
// provisioner.upgrade and an installation with no gateway configured both stay that way five
// seconds later, and five quick attempts would replace the actionable message with a note about
// attempt counts.
func unsupported(err error) bool {
	return errors.Is(err, ErrUnsupportedOperation) || errors.Is(err, ErrNotConfigured)
}

// auditedKind names the audit action an operation kind applies, or "" for the kinds that are
// part of the ordinary lifecycle and are audited by whatever queued them.
func auditedKind(kind string) string {
	switch kind {
	case "resize":
		return "service.resized"
	case "delete_remote":
		return "service.delete_remote"
	case "cancel_subscription":
		return "service.subscription_cancelled"
	}
	return ""
}

// resizePatch is the jsonb patch that records the plan and region a resize reached.
func resizePatch(op Operation) ([]byte, error) {
	patch := map[string]string{}
	if plan := op.targetPlan(); plan != "" {
		patch["plan_id"] = plan
	}
	if region := op.targetRegion(); region != "" {
		patch["region"] = region
	}
	encoded, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("provisioning: encode resize patch: %w", err)
	}
	return encoded, nil
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
