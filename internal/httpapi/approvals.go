package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/catalyst/billing/internal/approvals"
	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/paginate"
	"github.com/jackc/pgx/v5"
)

// Dual approval over HTTP (Gate D4).
//
// The interesting part is not the CRUD. It is that the *permission to approve* depends on the
// action being approved: a refund approval needs payment:refund, a termination approval needs
// service:terminate. That is why the check is done here rather than declared on the route: a
// single permission on the endpoint would let a support agent approve a refund.
//
// The second property is that the requester cannot approve their own request. The database
// enforces it (approvals_distinct_actors); this handler checks it first so the error names the
// problem instead of surfacing a constraint violation.

func init() {
	RegisterRoutes("70-approvals", registerApprovals)
}

func registerApprovals(r *router) {
	r.staffGet("/api/v1/staff/approvals", authn.PermAuditRead, r.s.handleApprovalList)
	// The permission these require depends on the action being decided, so it is resolved
	// inside the handler against the record. The route is still staff-only, still CSRF
	// protected and still requires an idempotency key.
	r.staffMutationDynamic(http.MethodPost, "/api/v1/staff/approvals/{id}/approve",
		r.s.handleApprovalApprove)
	r.staffMutationDynamic(http.MethodPost, "/api/v1/staff/approvals/{id}/reject",
		r.s.handleApprovalReject)
}

// actionPermission maps an approval action to the permission its approver must hold.
//
// The mapping is the security model: without it, holding audit:read would be enough to
// approve any action, which would make the second signature a formality.
func actionPermission(action string) (authn.Permission, bool) {
	switch action {
	case approvals.ActionRefund:
		return authn.PermPaymentRefund, true
	case approvals.ActionTerminateBatch:
		return authn.PermServiceTerminate, true
	case approvals.ActionHoldOverride:
		// The action an override performs is a suspension, so the permission is the one that
		// governs suspending. Requiring service:terminate instead would let the two be
		// confused, and a hold override does not terminate anything.
		return authn.PermServiceSuspend, true
	default:
		return "", false
	}
}

func (s *Server) handleApprovalList(w http.ResponseWriter, r *http.Request) {
	if s.approvals == nil {
		writeProblem(w, http.StatusServiceUnavailable, "approvals_unavailable",
			"approvals are not configured")
		return
	}
	limit, cursor, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	filter := approvals.Filter{
		Status: q.Get("status"),
		Action: q.Get("action"),
		Actor:  q.Get("actor_id"),
	}

	rows, next, err := s.approvals.List(r.Context(), filter, limit, cursor)
	if err != nil {
		if errors.Is(err, paginate.ErrInvalidCursor) {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
			return
		}
		s.staffError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		out = append(out, approvalMap(a))
	}
	writePage(w, Page[map[string]any]{Data: out, NextCursor: nextCursor(next)})
}

func approvalMap(a approvals.Approval) map[string]any {
	out := map[string]any{
		"id":           a.ID,
		"action":       a.Action,
		"reference_id": a.ReferenceID,
		"requested_by": a.RequestedBy,
		"status":       a.Status,
		"reason":       a.Reason,
		"created_at":   a.CreatedAt.UTC(),
		"decided_at":   timeOrNil(a.DecidedAt),
	}
	if a.ApprovedBy != "" {
		out["approved_by"] = a.ApprovedBy
	}
	return out
}

func (s *Server) handleApprovalApprove(w http.ResponseWriter, r *http.Request) {
	s.decideApproval(w, r, true)
}

func (s *Server) handleApprovalReject(w http.ResponseWriter, r *http.Request) {
	s.decideApproval(w, r, false)
}

func (s *Server) decideApproval(w http.ResponseWriter, r *http.Request, approve bool) {
	if s.approvals == nil {
		writeProblem(w, http.StatusServiceUnavailable, "approvals_unavailable",
			"approvals are not configured")
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such approval")
		return
	}

	var req struct {
		Reason string `json:"reason"`
	}
	if !s.decodeJSON(w, r, &req, 4<<10) {
		return
	}

	user, _ := userFrom(r.Context())

	// Read the approval first to learn which permission its approval requires.
	pending, err := s.approvals.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, approvals.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "not_found", "no such approval")
			return
		}
		s.staffError(w, r, err)
		return
	}

	required, known := actionPermission(pending.Action)
	if !known {
		writeProblem(w, http.StatusConflict, "unknown_action",
			"this approval's action is not one this build can decide")
		return
	}
	if !s.userHolds(r, user, required) {
		// The audit row is the point: an attempt to approve something you cannot approve is
		// exactly the event an incident review looks for.
		s.recordDenied(r, "approval.approve", "approval", id)
		writeProblem(w, http.StatusForbidden, "forbidden",
			"you do not hold the permission this action requires")
		return
	}

	var (
		decided approvals.Approval
	)
	if approve {
		decided, err = s.approvals.Approve(r.Context(), id, user.ID)
	} else {
		decided, err = s.approvals.Reject(r.Context(), id, user.ID, req.Reason)
	}
	switch {
	case err == nil:
	case errors.Is(err, approvals.ErrSelfApproval):
		// A refused self-approval is a denied outcome, not a failure: nothing went wrong with
		// the system, someone tried something the policy forbids.
		s.recordDenied(r, "approval.approve", "approval", id)
		writeProblem(w, http.StatusConflict, "self_approval_refused",
			"the requester cannot approve their own action")
		return
	case errors.Is(err, approvals.ErrNotPending):
		writeProblem(w, http.StatusConflict, "already_decided", err.Error())
		return
	case errors.Is(err, approvals.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such approval")
		return
	default:
		s.staffError(w, r, err)
		return
	}

	action := "approval.reject"
	if approve {
		action = "approval.approve"
	}
	s.recordApprovalAudit(r, action, id, decided)

	writeJSON(w, http.StatusOK, approvalMap(decided))
}

// userHolds reports whether a user holds a permission.
func (s *Server) userHolds(r *http.Request, user authn.User, permission authn.Permission) bool {
	if s.auth == nil || s.auth.Authz == nil {
		return false
	}
	_, ok := s.auth.Authz.Permissions(user)[permission]
	return ok
}

// recordDenied writes a denied audit event.
//
// It is best-effort by design: failing the response because the audit write failed would turn
// a policy refusal into a 500, which is a worse answer.
func (s *Server) recordDenied(r *http.Request, action, targetType, targetID string) {
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, s.auditEvent(r, action, targetType, targetID,
			audit.OutcomeDenied))
	})
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "audit write failed for a denied action",
			"action", action, "error", err)
	}
}

// recordApprovalAudit writes the audit row for a decision.
func (s *Server) recordApprovalAudit(r *http.Request, action, id string, a approvals.Approval) {
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, s.auditEvent(r, action, "approval", id,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"target_action": a.Action,
			"reference_id":  a.ReferenceID,
			"status":        a.Status,
		}))
	})
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "audit write failed for an approval decision",
			"approval_id", id, "error", err)
	}
}
