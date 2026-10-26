package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/paginate"
	"github.com/catalyst/billing/internal/provisioning"
	"github.com/jackc/pgx/v5"
)

// The operation resource (E5) and the audit read (F1).
//
// A long action must not hold a connection while a remote call runs. It returns 202 with
// an operation id and a status URL, and the client polls that URL. This file is that URL:
// the one place that answers "what happened to the thing I asked for".
//
// The audit read is here for the same reason: it is the operator's answer to "who did
// this", and it is a read over a table that only ever grows.

func init() {
	RegisterRoutes("20-operations", registerOperations)
}

func registerOperations(r *router) {
	r.staffGet("/api/v1/staff/operations/{id}", authn.PermServiceReadAll, r.s.handleOperation)
	r.staffExport("/api/v1/staff/audit", authn.PermAuditRead, r.s.handleAuditList)

	// ADM-18: the three things an operator does to a failed operation. They are separate
	// routes because they are separate decisions, and each needs a different permission:
	//
	//   retry   — re-runs the provisioning call, so it needs service:provision;
	//   resolve — records a human decision that the remote state is settled, so it needs
	//             the same service:write:all that edits a service's operator-facing facts;
	//   delete  — removes a terminal failure from the queue, same permission as resolve.
	r.staffMutation(http.MethodPost, "/api/v1/staff/operations/{id}/retry",
		authn.PermServiceProvision, r.s.handleOperationRetry)
	r.staffMutation(http.MethodPost, "/api/v1/staff/operations/{id}/resolve",
		authn.PermServiceWriteAll, r.s.handleOperationResolve)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/operations/{id}",
		authn.PermServiceWriteAll, r.s.handleOperationDelete)
}

// handleOperation returns one operation's state.
//
// `lease_expires_at` is derived (started_at + the worker's lease) rather than stored,
// because the lease is a property of the worker configuration, not of the operation. The
// UI shows "will be reaped in N minutes" so an operator knows whether to wait or
// intervene; a stored column would drift the moment the lease duration changed.
func (s *Server) handleOperation(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such operation")
		return
	}

	var (
		op           operationRow
		serviceID    string
		customerID   string
		customerMail string
		startedAt    *time.Time
	)
	err := s.pool.QueryRow(r.Context(), `
		SELECT so.id, so.service_id, s.customer_id, c.email,
		       so.kind, so.operation_key, so.generation, so.status, so.attempts,
		       COALESCE(so.last_error, ''), so.created_at, so.started_at, so.finished_at
		FROM service_operations so
		JOIN services s ON s.id = so.service_id
		JOIN customers c ON c.id = s.customer_id
		WHERE so.id = $1`, id).
		Scan(&op.ID, &serviceID, &customerID, &customerMail, &op.Kind, &op.OperationKey,
			&op.Generation, &op.Status, &op.Attempts, &op.LastError, &op.CreatedAt,
			&startedAt, &op.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such operation")
		return
	}
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "load operation failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error", "the request could not be completed")
		return
	}
	op.ServiceID = serviceID
	op.Customer = map[string]string{"id": customerID, "email": customerMail}
	op.StartedAt = startedAt
	if startedAt != nil {
		lease := startedAt.Add(provisioning.DefaultLeaseDuration).UTC()
		op.LeaseExpiresAt = &lease
	}

	writeJSON(w, http.StatusOK, op)
}

// operationRow is the operation resource's wire shape.
type operationRow struct {
	ID             string            `json:"id"`
	ServiceID      string            `json:"service_id"`
	Customer       map[string]string `json:"customer"`
	Kind           string            `json:"kind"`
	OperationKey   string            `json:"operation_key"`
	Generation     int64             `json:"generation"`
	Status         string            `json:"status"`
	Attempts       int               `json:"attempts"`
	LastError      string            `json:"last_error"`
	CreatedAt      time.Time         `json:"created_at"`
	StartedAt      *time.Time        `json:"started_at"`
	FinishedAt     *time.Time        `json:"finished_at"`
	LeaseExpiresAt *time.Time        `json:"lease_expires_at"`
}

// handleAuditList returns the application audit log, newest first.
func (s *Server) handleAuditList(w http.ResponseWriter, r *http.Request) {
	limit, cursor, err := ParsePage(r)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the pagination parameters are invalid")
		return
	}

	q := r.URL.Query()
	filter := audit.Filter{
		ActorID:    q.Get("actor_id"),
		Action:     q.Get("action"),
		TargetType: q.Get("target_type"),
		TargetID:   q.Get("target_id"),
		Outcome:    q.Get("outcome"),
	}
	for name, dst := range map[string]**time.Time{"from": &filter.From, "to": &filter.To} {
		raw := q.Get(name)
		if raw == "" {
			continue
		}
		when, perr := time.Parse(time.RFC3339, raw)
		if perr != nil {
			writeProblem(w, http.StatusBadRequest, "invalid_time",
				name+" must be an RFC3339 timestamp")
			return
		}
		*dst = &when
	}
	if filter.TargetID != "" && !validUUID(filter.TargetID) {
		writeProblem(w, http.StatusBadRequest, "invalid_target", "target_id must be a uuid")
		return
	}

	res, err := audit.List(r.Context(), s.pool, filter, limit, cursor)
	if err != nil {
		if errors.Is(err, paginate.ErrInvalidCursor) {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
			return
		}
		s.cfg.Logger.ErrorContext(r.Context(), "audit list failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error", "the request could not be completed")
		return
	}

	out := make([]map[string]any, 0, len(res.Rows))
	for _, row := range res.Rows {
		entry := map[string]any{
			"id":             row.ID,
			"actor_id":       row.ActorID,
			"actor_kind":     row.ActorKind,
			"action":         row.Action,
			"target_type":    row.TargetType,
			"target_id":      row.TargetID,
			"outcome":        row.Outcome,
			"changes":        row.Changes,
			"correlation_id": row.CorrelationID,
			"ip":             row.IP,
			"created_at":     row.CreatedAt.UTC(),
		}
		out = append(out, entry)
	}

	var next *string
	if res.NextCursor != "" {
		next = &res.NextCursor
	}
	writeJSON(w, http.StatusOK, Page[map[string]any]{Data: out, NextCursor: next})
}

// ------------------------------------------------------------------ actions (ADM-18) --

// The failed-operation actions. The queue had one reader and no way to act on what it showed:
// an operator could see that a provisioning call gave up and could do nothing about it from the
// panel. The three actions below are the three honest answers to a failed row, and they are
// deliberately not one "fix it" button:
//
//   - **retry** puts the same operation back in the queue. It is safe because the operation key
//     is unchanged, so the adapter's idempotency (or its authoritative lookup) prevents a second
//     server. It is refused when the service's generation has moved, because the worker would
//     fence the retried call straight back into `needs_review` — a retry that cannot succeed is
//     a lie about what the button does.
//   - **resolve** records that a human established the remote state by hand (the provider's
//     panel, a support call). It closes the row as `succeeded` and keeps the operator's sentence
//     in `last_error`, which is the only free-text column the operation has. The audit row is
//     the authoritative record of who decided it and why.
//   - **delete** removes a `failed` row from the queue. Only `failed` is deletable: `unknown` and
//     `needs_review` are open questions, and deleting one would hide exactly the exception the
//     dashboard raises. Deleting a failure does not touch the service, the invoice or the ledger.
//
// Every action is audited inside the same transaction as the change, so a row cannot move
// without a record of who moved it.

// operationAction is the subset of a row an action needs, locked for the transaction.
type operationAction struct {
	ID                string
	ServiceID         string
	Kind              string
	Status            string
	Generation        int64
	ServiceGeneration int64
	Attempts          int
}

// Action refusals. They are sentinels rather than problem responses inside the transaction so
// the message and the status code are decided in one place, after the rollback.
var (
	errOperationGone      = errors.New("no such operation")
	errOperationQueued    = errors.New("the operation is already queued or running")
	errOperationSucceeded = errors.New("the operation already succeeded")
	errOperationFenced    = errors.New("the service has changed since the operation was queued")
	errOperationOpen      = errors.New("the operation's remote outcome is still unknown")
	errOperationNotFailed = errors.New("only a failed operation can be deleted")
)

// loadOperationForAction reads and locks one operation with its service's current generation.
func loadOperationForAction(ctx context.Context, tx pgx.Tx, id string) (operationAction, error) {
	var a operationAction
	err := tx.QueryRow(ctx, `
		SELECT so.id, so.service_id, so.kind, so.status, so.generation, s.generation, so.attempts
		FROM service_operations so
		JOIN services s ON s.id = so.service_id
		WHERE so.id = $1
		FOR UPDATE OF so`, id).
		Scan(&a.ID, &a.ServiceID, &a.Kind, &a.Status, &a.Generation, &a.ServiceGeneration,
			&a.Attempts)
	return a, err
}

// writeOperationActionError maps a refusal to problem+json.
func (s *Server) writeOperationActionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errOperationGone), errors.Is(err, pgx.ErrNoRows):
		writeProblem(w, http.StatusNotFound, "not_found", "no such operation")
	case errors.Is(err, errOperationQueued):
		writeProblem(w, http.StatusConflict, "operation_already_queued",
			"the operation is already queued or running; wait for the worker to finish it")
	case errors.Is(err, errOperationSucceeded):
		writeProblem(w, http.StatusConflict, "operation_already_succeeded",
			"the operation already succeeded, so there is nothing to retry or resolve")
	case errors.Is(err, errOperationFenced):
		writeProblem(w, http.StatusConflict, "operation_fenced",
			"the service changed after this operation was queued, so re-running it would be "+
				"rejected by the worker's fencing token; review the service's current state instead")
	case errors.Is(err, errOperationOpen):
		writeProblem(w, http.StatusConflict, "operation_outcome_unknown",
			"the remote outcome is not known, so the operation cannot be deleted; resolve it first")
	case errors.Is(err, errOperationNotFailed):
		writeProblem(w, http.StatusConflict, "operation_not_deletable",
			"only a failed operation can be deleted")
	default:
		s.cfg.Logger.Error("operation action failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
	}
}

// operationActionID validates the path id before any query sees it.
func operationActionID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such operation")
		return "", false
	}
	return id, true
}

// handleOperationRetry requeues one failed or uncertain operation.
//
// The attempts counter is reset because the operator has asserted that the cause is fixed; the
// previous count and error stay in the audit row. Leaving it at the exhausted value would give
// the retry no budget, so it would fail again on the first unrelated hiccup.
func (s *Server) handleOperationRetry(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id, ok := operationActionID(w, r)
	if !ok {
		return
	}

	var reason string
	if r.ContentLength != 0 {
		var req struct {
			Reason string `json:"reason"`
		}
		if !s.decodeJSON(w, r, &req, 16<<10) {
			return
		}
		reason = strings.TrimSpace(req.Reason)
	}

	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		op, err := loadOperationForAction(ctx, tx, id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errOperationGone
			}
			return fmt.Errorf("operation: lock: %w", err)
		}
		switch op.Status {
		case "queued", "running", "retry_wait":
			return errOperationQueued
		case "succeeded":
			return errOperationSucceeded
		case "failed", "unknown", "needs_review":
			// Retryable.
		default:
			return errOperationQueued
		}
		if op.Generation != op.ServiceGeneration {
			return errOperationFenced
		}

		if _, err := tx.Exec(ctx, `
			UPDATE service_operations
			SET status = 'queued', attempts = 0, started_at = NULL, finished_at = NULL
			WHERE id = $1`, id); err != nil {
			return fmt.Errorf("operation: requeue: %w", err)
		}
		return s.auditorFor(r, reason).RecordAction(ctx, tx, "operation.retry",
			"service_operation", id, map[string]any{
				"service_id":        op.ServiceID,
				"kind":              op.Kind,
				"previous_status":   op.Status,
				"previous_attempts": op.Attempts,
			})
	})
	if err != nil {
		s.writeOperationActionError(w, err)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id":         id,
		"status":     "queued",
		"queued":     true,
		"status_url": "/api/v1/staff/operations/" + id,
	})
}

// handleOperationResolve closes an operation a human has settled by hand.
//
// The reason is required: `needs_review` means the panel does not know the remote state, and a
// resolution without a sentence saying how the operator established it is indistinguishable
// from somebody clearing the alert.
func (s *Server) handleOperationResolve(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id, ok := operationActionID(w, r)
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		writeProblem(w, http.StatusBadRequest, "invalid_reason",
			"a reason is required: say how the remote state was established")
		return
	}

	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		op, err := loadOperationForAction(ctx, tx, id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errOperationGone
			}
			return fmt.Errorf("operation: lock: %w", err)
		}
		switch op.Status {
		case "succeeded":
			return errOperationSucceeded
		case "queued", "running", "retry_wait":
			return errOperationQueued
		case "failed", "unknown", "needs_review":
			// Resolvable.
		default:
			return errOperationNotFailed
		}

		// `last_error` is the only free-text column the row has, so the operator's sentence is
		// kept there rather than dropped: the audit row is authoritative, but an operator looking
		// at the queue should not have to open the audit log to learn why a row turned green.
		note := truncate("resolved by operator: "+reason, 4000)
		if _, err := tx.Exec(ctx, `
			UPDATE service_operations
			SET status = 'succeeded', finished_at = now(), last_error = $2
			WHERE id = $1`, id, note); err != nil {
			return fmt.Errorf("operation: resolve: %w", err)
		}
		return s.auditorFor(r, reason).RecordAction(ctx, tx, "operation.resolve",
			"service_operation", id, map[string]any{
				"service_id":      op.ServiceID,
				"kind":            op.Kind,
				"previous_status": op.Status,
			})
	})
	if err != nil {
		s.writeOperationActionError(w, err)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       id,
		"status":   "succeeded",
		"resolved": true,
	})
}

// handleOperationDelete removes a failed operation from the queue.
func (s *Server) handleOperationDelete(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id, ok := operationActionID(w, r)
	if !ok {
		return
	}
	var reason string
	if r.ContentLength != 0 {
		var req struct {
			Reason string `json:"reason"`
		}
		if !s.decodeJSON(w, r, &req, 16<<10) {
			return
		}
		reason = strings.TrimSpace(req.Reason)
	}

	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		op, err := loadOperationForAction(ctx, tx, id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errOperationGone
			}
			return fmt.Errorf("operation: lock: %w", err)
		}
		switch op.Status {
		case "failed":
			// Deletable.
		case "unknown", "needs_review":
			return errOperationOpen
		case "queued", "running", "retry_wait":
			return errOperationQueued
		default:
			return errOperationNotFailed
		}

		if _, err := tx.Exec(ctx, `DELETE FROM service_operations WHERE id = $1`, id); err != nil {
			return fmt.Errorf("operation: delete: %w", err)
		}
		return s.auditorFor(r, reason).RecordAction(ctx, tx, "operation.delete",
			"service_operation", id, map[string]any{
				"service_id": op.ServiceID,
				"kind":       op.Kind,
				"status":     op.Status,
			})
	})
	if err != nil {
		s.writeOperationActionError(w, err)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

// validUUID reports whether s looks like a UUID.
//
// It is a shape check, not a lookup: a malformed id must be a 404 that never reaches the
// database, so the response cannot differ between "malformed" and "someone else's".
func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}
