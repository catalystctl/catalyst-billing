package httpapi

import (
	"errors"
	"net/http"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/paginate"
)

// Session management over HTTP (IDN-15, IDN-17).
//
// Two surfaces, one rule: the subject of the query comes from the session, never from a
// path or a query parameter, except on the staff view where a second, explicit permission
// check authorises naming somebody else. A caller who names a session that is not theirs
// gets 404 — the same answer as a session that never existed — so the endpoints cannot be
// used to discover identifiers or to learn whether a user is currently signed in.
//
// The portal routes exist for every authenticated user, staff included: "where am I signed
// in" is a question about *your* account, not about your role.

func init() {
	RegisterRoutes("60-identity-sessions", registerIdentitySessions)
}

func registerIdentitySessions(r *router) {
	// The caller's own sessions. The permission is the customer baseline (every signed-in
	// user holds it), which is what makes "where am I signed in" available to staff and
	// customers through the same route rather than two that could drift.
	r.get("/api/v1/portal/sessions", authn.PermCustomerReadSelf, r.s.handlePortalSessions)
	r.mutation(http.MethodDelete, "/api/v1/portal/sessions/{id}",
		authn.PermCustomerWriteSelf, r.s.handlePortalSessionRevoke)
	r.mutation(http.MethodPost, "/api/v1/portal/sessions/revoke-others",
		authn.PermCustomerWriteSelf, r.s.handlePortalSessionRevokeOthers)
	r.get("/api/v1/portal/login-history", authn.PermCustomerReadSelf, r.s.handlePortalLoginHistory)

	// The staff view. It lists the caller's own sessions by default; naming another user
	// requires staff:manage, which is checked in the handler because the same route has to
	// serve both cases. The declared permission is customer:read:all so a support agent can
	// see their own sessions here without being an administrator.
	r.staffGet("/api/v1/staff/sessions", authn.PermCustomerReadAll, r.s.handleStaffSessions)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/sessions/{id}",
		authn.PermStaffManage, r.s.handleStaffSessionRevoke)
}

// sessionJSON is the wire shape of one session.
//
// It never includes the token or the CSRF secret: the list describes sessions, it does not
// hand out credentials.
func sessionJSON(info authn.SessionInfo) map[string]any {
	out := map[string]any{
		"id":           info.ID,
		"user_id":      info.UserID,
		"created_at":   info.CreatedAt.UTC(),
		"last_seen_at": info.LastSeenAt.UTC(),
		"expires_at":   info.ExpiresAt.UTC(),
		"ip":           info.IP,
		"user_agent":   info.UserAgent,
		"current":      info.Current,
		// impersonated_by is present on every row so a client can render the banner without
		// special-casing "the field is missing".
		"impersonated_by": nil,
	}
	if info.ImpersonatedBy != "" {
		out["impersonated_by"] = info.ImpersonatedBy
	}
	return out
}

func (s *Server) handlePortalSessions(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	user, _ := userFrom(r.Context())
	sess, _ := sessionFrom(r.Context())

	sessions, err := s.auth.Service.Sessions(r.Context(), user.ID, sess.ID)
	if err != nil {
		s.identityError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(sessions))
	for _, info := range sessions {
		out = append(out, sessionJSON(info))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) handlePortalSessionRevoke(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	user, _ := userFrom(r.Context())
	sess, _ := sessionFrom(r.Context())

	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such session")
		return
	}

	if err := s.auth.Service.RevokeSession(r.Context(), user.ID, id); err != nil {
		s.identityError(w, r, err)
		return
	}

	// Revoking the session that made the request is a logout, and the browser must be told:
	// leaving a cookie for a dead session produces one failed request per click until the
	// client notices.
	if sess.ID != "" && id == sess.ID {
		s.clearSessionCookies(w)
		writeJSON(w, http.StatusOK, map[string]any{"status": "signed_out", "current": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "revoked", "current": false})
}

func (s *Server) handlePortalSessionRevokeOthers(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	user, _ := userFrom(r.Context())
	sess, _ := sessionFrom(r.Context())
	if sess.ID == "" {
		// An API key has no session to keep. "Sign out everywhere" for a key is "revoke the
		// key", which is a different endpoint with a different consequence.
		writeProblem(w, http.StatusBadRequest, "no_session",
			"this request is not authenticated by a session")
		return
	}

	n, err := s.auth.Service.RevokeOtherSessions(r.Context(), user.ID, sess.ID)
	if err != nil {
		s.identityError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "revoked",
		"count":  n,
	})
}

func (s *Server) handlePortalLoginHistory(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	user, _ := userFrom(r.Context())

	limit, cursor, err := ParsePage(r)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor or limit is invalid")
		return
	}

	history, err := s.auth.Service.LoginHistory(r.Context(), user.ID, limit, cursor)
	if err != nil {
		if errors.Is(err, paginate.ErrInvalidCursor) {
			writeProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is invalid")
			return
		}
		s.identityError(w, r, err)
		return
	}

	out := make([]map[string]any, 0, len(history.Events))
	for _, ev := range history.Events {
		out = append(out, map[string]any{
			"id":         ev.ID,
			"event":      ev.Event,
			"detail":     ev.Detail,
			"ip":         ev.IP,
			"created_at": ev.CreatedAt.UTC(),
		})
	}
	page := Page[map[string]any]{Data: out}
	if history.NextCursor != "" {
		next := history.NextCursor
		page.NextCursor = &next
	}
	writePage(w, page)
}

func (s *Server) handleStaffSessions(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	caller, _ := userFrom(r.Context())
	sess, _ := sessionFrom(r.Context())

	target, ok := s.sessionSubject(w, r, caller)
	if !ok {
		return
	}

	sessions, err := s.auth.Service.Sessions(r.Context(), target, sess.ID)
	if err != nil {
		s.identityError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(sessions))
	for _, info := range sessions {
		out = append(out, sessionJSON(info))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":    out,
		"user_id": target,
	})
}

func (s *Server) handleStaffSessionRevoke(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	caller, _ := userFrom(r.Context())
	sess, _ := sessionFrom(r.Context())

	target, ok := s.sessionSubject(w, r, caller)
	if !ok {
		return
	}

	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such session")
		return
	}

	if err := s.auth.Service.RevokeSession(r.Context(), target, id); err != nil {
		s.identityError(w, r, err)
		return
	}
	// An administrator who revokes their own current session is logged out, exactly as on
	// the portal route: the consequence of revoking a session must not depend on which URL
	// was used to do it.
	if sess.ID != "" && id == sess.ID && target == caller.ID {
		s.clearSessionCookies(w)
		writeJSON(w, http.StatusOK, map[string]any{"status": "signed_out", "current": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "revoked", "current": false})
}

// sessionSubject resolves whose sessions a staff request is about.
//
// The caller's own sessions need no extra permission — the route is already staff-only and
// already requires customer:read:all. Naming somebody else is an administrative act and
// needs staff:manage, the same permission that grants roles and starts an impersonation.
// The check is here rather than on the route because one route serves both cases.
func (s *Server) sessionSubject(w http.ResponseWriter, r *http.Request, caller authn.User) (string, bool) {
	target := r.URL.Query().Get("user_id")
	if target == "" || target == caller.ID {
		return caller.ID, true
	}
	if !validUUID(target) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such user")
		return "", false
	}
	if s.auth == nil || s.auth.Authz == nil || !s.auth.Authz.Can(caller, authn.PermStaffManage) {
		writeProblem(w, http.StatusForbidden, "forbidden",
			"you do not have access to this action")
		return "", false
	}
	return target, true
}

// identityError maps an identity error to a response.
//
// "No such row for you" and "no such row at all" are the same answer on purpose: a
// distinguishable 403 would confirm that a session id exists and belongs to someone else.
func (s *Server) identityError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, authn.ErrNoSessionRow), errors.Is(err, authn.ErrUnknownAccount):
		writeProblem(w, http.StatusNotFound, "not_found", "no such record")
	case errors.Is(err, authn.ErrImpersonationNotAllowed):
		writeProblem(w, http.StatusBadRequest, "not_impersonating",
			"this session is not an impersonation")
	case errors.Is(err, authn.ErrSelfImpersonation):
		writeProblem(w, http.StatusBadRequest, "self_impersonation",
			"an operator cannot impersonate themselves")
	case errors.Is(err, authn.ErrForbidden):
		writeProblem(w, http.StatusForbidden, "forbidden", "you do not have access to this action")
	case errors.Is(err, authn.ErrAccountDisabled):
		writeProblem(w, http.StatusConflict, "account_disabled", "that account is disabled")
	case errors.Is(err, authn.ErrRoleNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such role")
	case errors.Is(err, authn.ErrSystemRole):
		writeProblem(w, http.StatusConflict, "system_role",
			"a system role cannot be changed: the separation of duties depends on it")
	case errors.Is(err, authn.ErrRoleInUse):
		writeProblem(w, http.StatusConflict, "role_in_use", err.Error())
	case errors.Is(err, authn.ErrInvalidRole):
		writeProblem(w, http.StatusBadRequest, "invalid_role", err.Error())
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "identity request failed",
			"path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
	}
}
