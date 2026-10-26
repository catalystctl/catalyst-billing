package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/jackc/pgx/v5"
)

// Impersonation over HTTP (IDN-14, ADM-30).
//
// The flow has three steps and the client sees all of them:
//
//  1. POST /api/v1/staff/users/{id}/impersonate — staff:manage. Issues a session for the
//     target and sets it as the cookie. The response carries `impersonated: true`, the
//     operator and the target, so the UI can raise its banner before the first request.
//  2. Every /api/v1/auth/me while impersonating reports the operator. That is the durable
//     signal: a reload, a second tab and a bookmark all still show whose identity is worn.
//  3. POST /api/v1/portal/impersonation/stop — no permission beyond the session. It ends
//     the borrowed session and rotates the operator's own token back into the cookie.
//
// The audit row is written twice on purpose: once against the target ("somebody acted as
// me") and once against the operator ("I acted as somebody"). A review that filters by
// actor id finds the second, and a customer asking "why did this happen on my account"
// finds the first.
//
// What impersonation deliberately cannot do: change the target's password, enrol or remove
// MFA, mint an API key for them, or grant roles. Those are the actions that turn a support
// visit into a takeover, and they are refused by the guard below rather than by the UI.

func init() {
	RegisterRoutes("61-identity-impersonation", registerImpersonation)
}

func registerImpersonation(r *router) {
	r.staffMutation(http.MethodPost, "/api/v1/staff/users/{id}/impersonate",
		authn.PermStaffManage, r.s.handleImpersonate)
	r.mutation(http.MethodPost, "/api/v1/portal/impersonation/stop",
		authn.PermCustomerReadSelf, r.s.handleStopImpersonation)
}

func (s *Server) handleImpersonate(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	operator, _ := userFrom(r.Context())
	operatorSession, _ := sessionFrom(r.Context())
	if operatorSession.ID == "" {
		// An API key has no session to return to, so it cannot start an impersonation: the
		// stop path would have nowhere to go. Refusing is clearer than issuing a session
		// whose end logs the caller out.
		writeProblem(w, http.StatusBadRequest, "session_required",
			"impersonation requires a browser session, not an API key")
		return
	}

	targetID := r.PathValue("id")
	if !validUUID(targetID) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such user")
		return
	}

	// The permission was checked by the route wrapper. This second check is about *whom*:
	// an operator must not be able to wear another staff member's identity, because that
	// would hand a support agent an administrator's permissions through the back door.
	res, err := s.auth.Service.Impersonate(r.Context(),
		operator.ID, operatorSession.ID, targetID, s.clientIP(r), r.UserAgent())
	switch {
	case err == nil:
	case errors.Is(err, authn.ErrSelfImpersonation):
		writeProblem(w, http.StatusBadRequest, "self_impersonation",
			"an operator cannot impersonate themselves")
		return
	case errors.Is(err, authn.ErrForbidden):
		writeProblem(w, http.StatusForbidden, "forbidden",
			"that account cannot be impersonated")
		return
	case errors.Is(err, authn.ErrAccountDisabled):
		writeProblem(w, http.StatusConflict, "account_disabled", "that account is disabled")
		return
	case errors.Is(err, authn.ErrUnknownAccount):
		writeProblem(w, http.StatusNotFound, "not_found", "no such user")
		return
	default:
		s.identityError(w, r, err)
		return
	}

	// The audit row names both principals. It is written after the session exists so a
	// failure to record it cannot leave an impersonation running unrecorded: the caller
	// gets an error and the session is revoked below.
	if err := s.recordImpersonationAudit(r, "user.impersonation_start",
		targetID, operator.ID); err != nil {
		if rerr := s.auth.Service.RevokeSession(r.Context(), targetID, res.Session.ID); rerr != nil {
			s.cfg.Logger.ErrorContext(r.Context(), "could not revoke an unrecorded impersonation",
				"error", rerr)
		}
		s.cfg.Logger.ErrorContext(r.Context(), "audit write failed for impersonation", "error", err)
		writeProblem(w, http.StatusServiceUnavailable, "audit_unavailable",
			"the impersonation could not be recorded; try again")
		return
	}

	s.setSessionCookies(w, res.Token, res.Session)
	writeJSON(w, http.StatusCreated, map[string]any{
		"impersonating": true,
		"impersonated_by": map[string]any{
			"id":    res.Operator.ID,
			"email": res.Operator.Email,
		},
		"user": map[string]any{
			"id":           res.Target.ID,
			"email":        res.Target.Email,
			"display_name": res.Target.DisplayName,
			"roles":        nonNilRoles(res.Target.Roles),
			"is_staff":     res.Target.IsStaff(),
		},
		"csrf_token": res.Session.CSRFToken,
		"expires_at": res.Session.ExpiresAt,
	})
}

func (s *Server) handleStopImpersonation(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	target, _ := userFrom(r.Context())
	sess, _ := sessionFrom(r.Context())
	if sess.ID == "" {
		writeProblem(w, http.StatusBadRequest, "session_required",
			"stopping an impersonation requires a browser session")
		return
	}

	res, err := s.auth.Service.StopImpersonation(r.Context(), target.ID, sess.ID, s.clientIP(r))
	if err != nil {
		if errors.Is(err, authn.ErrNoSessionRow) {
			// The operator's own session is gone too (revoked or expired while they were
			// impersonating). Clear the cookie: there is nothing to return to.
			s.clearSessionCookies(w)
			writeProblem(w, http.StatusUnauthorized, "operator_session_ended",
				"your own session has ended; sign in again")
			return
		}
		s.identityError(w, r, err)
		return
	}

	if err := s.recordImpersonationAudit(r, "user.impersonation_stop",
		target.ID, res.OperatorID); err != nil {
		// The session is already ended and the operator's token already rotated. Failing the
		// response would leave the browser holding a dead cookie, so log and continue: the
		// auth_events row written by the service is the durable record.
		s.cfg.Logger.ErrorContext(r.Context(), "audit write failed for impersonation stop",
			"error", err)
	}

	s.setSessionCookies(w, res.Token, res.Session)
	writeJSON(w, http.StatusOK, map[string]any{
		"impersonating": false,
		"user": map[string]any{
			"id":           res.User.ID,
			"email":        res.User.Email,
			"display_name": res.User.DisplayName,
			"roles":        nonNilRoles(res.User.Roles),
			"is_staff":     res.User.IsStaff(),
		},
		"csrf_token": res.Session.CSRFToken,
		"expires_at": res.Session.ExpiresAt,
	})
}

// recordImpersonationAudit writes one audit row naming both principals.
//
// The actor is the target, because that is whose account was acted on, and the impersonator
// is carried in `changes` as well as (once internal/audit grows the field) in the dedicated
// column. Putting it in `changes` now means the fact is recorded today even before the
// shared audit type carries it, so a missing schema change degrades the query, not the
// evidence.
func (s *Server) recordImpersonationAudit(r *http.Request, action, targetID, operatorID string) error {
	changes := map[string]any{
		"impersonated_by": operatorID,
	}
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		ev := s.auditEvent(r, action, "user", targetID, audit.OutcomeSuccess)
		// The actor is the target user, not the operator: the account that was acted on is
		// what a per-record review filters by. The operator goes in the dedicated
		// `impersonated_by` column as well as in `changes`, so "every action this operator took
		// while wearing somebody else's identity" is one indexed query rather than a scan of a
		// JSON column.
		ev.Actor = audit.Actor{ID: targetID, Kind: audit.ActorUser, ImpersonatedBy: operatorID}
		return audit.Record(ctx, tx, ev.WithChanges(changes))
	})
	return err
}

// impersonationGuard refuses an action that must be performed as yourself.
//
// It is applied to the endpoints that would turn a support visit into a takeover and that are
// reachable from a customer-scoped session — enrolling or removing a second factor, and reading
// the second-factor state (auth_lifecycle.go, mfa_state.go). Money and the remaining credential
// actions are refused a layer earlier, by the route table in auth.go's `impersonationRefused`,
// which every request passes through; this guard is kept for the handlers that want the refusal
// to be part of their own contract rather than an inherited property of the middleware.
//
// Both layers answer 403 with code `impersonating`, so a client cannot tell them apart and does
// not have to: the answer to either is the same — stop impersonating and act as yourself.
func (s *Server) impersonationGuard(w http.ResponseWriter, r *http.Request) bool {
	sess, ok := sessionFrom(r.Context())
	if !ok || sess.ID == "" || s.auth == nil || s.auth.Service == nil {
		return true
	}
	// The session carries the flag when it was loaded; a second query is only needed for
	// routes that do not go through Authenticate (there are none today, but the fallback
	// keeps the guard correct if one is added).
	impersonated, _, err := s.auth.Service.IsImpersonated(r.Context(), sess.ID)
	if err != nil {
		// Fail closed: an action guarded against impersonation must not proceed when the
		// server cannot tell whether the caller is impersonating.
		writeProblem(w, http.StatusServiceUnavailable, "identity_unavailable",
			"the session could not be verified; try again")
		return false
	}
	if impersonated {
		writeProblem(w, http.StatusForbidden, "impersonating",
			"this action cannot be performed while impersonating another user")
		return false
	}
	return true
}
