package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/paginate"
)

// Staff user administration over HTTP (ADM-12, IDN-01).
//
// This is the route set that closes the panel's headline gap: before it, the only way to
// create a login was `billing admin create-user`, which needs shell access to the host. An
// administrator could not add a support agent, and nobody could add a second administrator,
// without ssh.
//
// The shape is deliberately small and matches the other staff CRUD surfaces:
//
//	GET    /api/v1/staff/users                list, with search and filters
//	POST   /api/v1/staff/users                create a login and assign its role in one act
//	PATCH  /api/v1/staff/users/{id}           display name, email, role, enable/disable
//	DELETE /api/v1/staff/users/{id}           only when nothing depends on the account
//	POST   /api/v1/staff/users/{id}/password  set a password (never returned)
//
// Everything is guarded by staff:manage, the permission that already governs roles and
// impersonation: creating a login is the act of granting somebody access to the panel.
//
// Two refusals matter more than the rest and are answered with their own codes so the screen
// can say what happened rather than "that did not work":
//
//   - `last_administrator` — the change would leave no active administrator. The check runs
//     in the transaction *and* as a database trigger (0045), because two operators demoting
//     two administrators at the same moment would each see the other still present.
//   - `self_lockout` — an operator disabling or deleting the account they are signed in as.
//
// A password set here is never in a response body, is never in the audit row, and is hashed
// through the same validator registration uses, so a weak one is the same 422
// `weak_password`.

func init() {
	RegisterRoutes("63-identity-users", registerUsers)
}

func registerUsers(r *router) {
	r.staffGet("/api/v1/staff/users", authn.PermStaffManage, r.s.handleStaffUserList)
	r.staffMutation(http.MethodPost, "/api/v1/staff/users",
		authn.PermStaffManage, r.s.handleStaffUserCreate)
	r.staffMutation(http.MethodPatch, "/api/v1/staff/users/{id}",
		authn.PermStaffManage, r.s.handleStaffUserUpdate)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/users/{id}",
		authn.PermStaffManage, r.s.handleStaffUserDelete)
	r.staffMutation(http.MethodPost, "/api/v1/staff/users/{id}/password",
		authn.PermStaffManage, r.s.handleStaffUserSetPassword)
}

// userJSON is the wire shape of an account.
//
// `role` is the name the roles screen shows, and is empty for a customer account: the client
// renders "Customer" from that, not from a separate boolean, so there is one source of truth
// for "is this a staff account".
func userJSON(rec authn.UserRecord) map[string]any {
	var lastSignIn any
	if rec.LastSignInAt != nil {
		lastSignIn = rec.LastSignInAt.UTC()
	}
	var customerID any
	if rec.CustomerID != "" {
		customerID = rec.CustomerID
	}
	return map[string]any{
		"id":              rec.ID,
		"email":           rec.Email,
		"display_name":    rec.DisplayName,
		"status":          rec.Status,
		"email_verified":  rec.EmailVerified,
		"role":            rec.Role,
		"is_staff":        rec.IsStaff(),
		"customer_id":     customerID,
		"created_at":      rec.CreatedAt.UTC(),
		"last_sign_in_at": lastSignIn,
	}
}

func (s *Server) handleStaffUserList(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	limit, cursor, ok := s.pageParams(w, r)
	if !ok {
		return
	}

	query := r.URL.Query()
	filter := authn.UserFilter{
		Query:  strings.TrimSpace(query.Get("q")),
		Role:   strings.TrimSpace(query.Get("role")),
		Status: strings.TrimSpace(query.Get("status")),
	}
	// `staff=true` is the agents list, `staff=false` the customers list. An absent
	// parameter is both, which is what the screen shows by default.
	switch strings.ToLower(strings.TrimSpace(query.Get("staff"))) {
	case "":
	case "true", "1":
		yes := true
		filter.Staff = &yes
	case "false", "0":
		no := false
		filter.Staff = &no
	default:
		writeProblem(w, http.StatusBadRequest, "invalid_query", "staff must be true or false")
		return
	}

	rows, next, err := s.auth.Service.ListUsers(r.Context(), filter, limit, cursor)
	if err != nil {
		s.userAdminError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, rec := range rows {
		out = append(out, userJSON(rec))
	}
	body := map[string]any{"data": out}
	if next != "" {
		body["next_cursor"] = next
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleStaffUserCreate(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}

	var req struct {
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
		Role        string `json:"role"`
		// EmailVerified defaults to true: the operator was given the address by the person
		// on the other end of the conversation, which is the same assurance a verification
		// message gives. It is explicit rather than implicit so the screen can offer it as
		// a choice for a customer whose address nobody has confirmed.
		EmailVerified *bool `json:"email_verified"`
	}
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	operator, _ := userFrom(r.Context())

	verified := true
	if req.EmailVerified != nil {
		verified = *req.EmailVerified
	}

	rec, err := s.auth.Service.CreateUser(r.Context(), authn.CreateUserRequest{
		Email:         req.Email,
		DisplayName:   req.DisplayName,
		Password:      req.Password,
		Role:          req.Role,
		EmailVerified: verified,
		ActorID:       operator.ID,
	}, s.auditorFor(r, ""))
	if err != nil {
		s.userAdminError(w, r, err)
		return
	}

	// A new role means the custom-role table the authorizer holds may be stale for the new
	// holder. The role itself did not change, so this is belt and braces — but a role that
	// resolves to nothing until the next boot is exactly the bug that is invisible in
	// review.
	s.refreshAuthorizerTable(r)
	writeJSON(w, http.StatusCreated, userJSON(rec))
}

func (s *Server) handleStaffUserUpdate(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such user")
		return
	}

	var req struct {
		DisplayName *string `json:"display_name"`
		Email       *string `json:"email"`
		// `role` absent leaves the role alone; `"role": ""` removes it and makes the account
		// a customer. The distinction is why this is PATCH: the screen sends what changed.
		Role   *string `json:"role"`
		Status *string `json:"status"`
	}
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	operator, _ := userFrom(r.Context())

	rec, err := s.auth.Service.UpdateUser(r.Context(), id, authn.UpdateUserRequest{
		DisplayName: req.DisplayName,
		Email:       req.Email,
		Role:        req.Role,
		Status:      req.Status,
		ActorID:     operator.ID,
	}, s.auditorFor(r, ""))
	if err != nil {
		s.userAdminError(w, r, err)
		return
	}

	// A role change takes effect for the *next* request the affected user makes, because the
	// session reloads roles on every authentication. The authorizer's custom-role snapshot is
	// refreshed here for the same reason the roles screen refreshes it.
	if req.Role != nil {
		s.refreshAuthorizerTable(r)
	}
	writeJSON(w, http.StatusOK, userJSON(rec))
}

func (s *Server) handleStaffUserDelete(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such user")
		return
	}
	operator, _ := userFrom(r.Context())

	if err := s.auth.Service.DeleteUser(r.Context(), id, operator.ID, s.auditorFor(r, "")); err != nil {
		s.userAdminError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStaffUserSetPassword(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such user")
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if !s.decodeJSON(w, r, &req, 8<<10) {
		return
	}
	operator, _ := userFrom(r.Context())

	revoked, err := s.auth.Service.SetPasswordByOperator(r.Context(), id, req.Password,
		operator.ID, s.auditorFor(r, ""))
	if err != nil {
		s.userAdminError(w, r, err)
		return
	}

	// The response says what happened to the sessions and nothing about the credential. There
	// is no path in this API that returns a password, and this is the one place somebody
	// might expect one.
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "password_set",
		"sessions_revoked": revoked,
	})
}

// userAdminError maps a user-administration error to a response.
//
// The two refusals that matter are `last_administrator` and `self_lockout`: they are the
// difference between a screen that is safe to hand an administrator and one that can end a
// deployment's access in two clicks.
func (s *Server) userAdminError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, authn.ErrUserNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such user")
	case errors.Is(err, authn.ErrEmailTaken):
		writeProblem(w, http.StatusConflict, "email_taken",
			"that address already has an account")
	case errors.Is(err, authn.ErrLastAdministrator):
		writeProblem(w, http.StatusConflict, "last_administrator",
			"the last active administrator cannot be demoted, disabled or deleted: "+
				"grant administrator to another active account first")
	case errors.Is(err, authn.ErrSelfLockout):
		writeProblem(w, http.StatusConflict, "self_lockout", err.Error())
	case errors.Is(err, authn.ErrUserHasHistory):
		writeProblem(w, http.StatusConflict, "user_has_history", err.Error())
	case errors.Is(err, authn.ErrUnknownRole), errors.Is(err, authn.ErrRoleNotFound):
		writeProblem(w, http.StatusBadRequest, "unknown_role", err.Error())
	case errors.Is(err, authn.ErrInvalidEmail):
		writeProblem(w, http.StatusBadRequest, "invalid_email", "a valid email address is required")
	case errors.Is(err, authn.ErrInvalidUser):
		writeProblem(w, http.StatusBadRequest, "invalid_user", err.Error())
	case errors.Is(err, authn.ErrWeakPassword):
		// The same code, status and message the registration and reset flows answer with: a
		// password rule that differed by screen would be a rule nobody can state.
		writeProblem(w, http.StatusUnprocessableEntity, "weak_password", err.Error())
	case errors.Is(err, paginate.ErrInvalidCursor):
		writeProblem(w, http.StatusBadRequest, "invalid_cursor",
			"the pagination parameters are invalid")
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "user administration request failed",
			"path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
	}
}
