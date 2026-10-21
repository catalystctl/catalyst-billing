package httpapi

import (
	"errors"
	"net/http"
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
}

// handleOperation returns one operation's state.
//
// `lease_expires_at` is derived (started_at + the worker's lease) rather than stored,
// because the lease is a property of the worker configuration, not of the operation. The
// UI shows "will be reaped in N minutes" so an operator knows whether to wait or
// intervene; a stored column would drift the moment the lease duration changed.
func (s *Server) handleOperation(w http.ResponseWriter, r *http.Request) {
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
