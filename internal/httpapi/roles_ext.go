package httpapi

import (
	"context"
	"net/http"
	"sort"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/jackc/pgx/v5"
)

// Editable roles over HTTP (IDN-13, ADM-13).
//
// The permission matrix is a screen, so the API is shaped like one: list every role with its
// permissions, create a role from a set, replace a role's set, delete a role nobody holds.
// The four system roles appear in the list — an operator needs to see what administrator
// means — but every write path refuses them with 409 `system_role`, because the separation
// of duties the approval flow depends on is written against those four names.
//
// A permission this build does not declare is refused at write time. Storing one would
// create a role that looks wider than it is, and the operator would have no way to notice.

func init() {
	RegisterRoutes("62-identity-roles", registerRoles)
}

func registerRoles(r *router) {
	r.staffGet("/api/v1/staff/roles", authn.PermStaffManage, r.s.handleRoleList)
	r.staffMutation(http.MethodPost, "/api/v1/staff/roles",
		authn.PermStaffManage, r.s.handleRoleCreate)
	r.staffMutation(http.MethodPut, "/api/v1/staff/roles/{id}/permissions",
		authn.PermStaffManage, r.s.handleRoleUpdatePermissions)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/roles/{id}",
		authn.PermStaffManage, r.s.handleRoleDelete)
	// The declared permission set is what the matrix offers. It is served rather than
	// embedded in the client so the client cannot offer a permission the server refuses.
	r.staffGet("/api/v1/staff/permissions", authn.PermStaffManage, r.s.handlePermissionList)
}

// roleJSON is the wire shape of a role.
func roleJSON(rec authn.RoleRecord) map[string]any {
	perms := make([]string, 0, len(rec.Permissions))
	for _, p := range rec.Permissions {
		perms = append(perms, string(p))
	}
	sort.Strings(perms)
	return map[string]any{
		"id":          rec.ID,
		"name":        rec.Name,
		"description": rec.Description,
		"is_system":   rec.System,
		"created_at":  rec.CreatedAt.UTC(),
		"permissions": perms,
	}
}

func (s *Server) handleRoleList(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	roles, err := s.auth.Service.ListRoles(r.Context())
	if err != nil {
		s.identityError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(roles))
	for _, rec := range roles {
		out = append(out, roleJSON(rec))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) handlePermissionList(w http.ResponseWriter, r *http.Request) {
	perms := make([]string, 0, len(authn.AllPermissions()))
	for _, p := range authn.AllPermissions() {
		perms = append(perms, string(p))
	}
	// Every role's current set is included so the matrix renders in one request instead of
	// one per role.
	byRole := map[string][]string{}
	if s.auth != nil && s.auth.Service != nil {
		if roles, err := s.auth.Service.ListRoles(r.Context()); err == nil {
			for _, rec := range roles {
				names := make([]string, 0, len(rec.Permissions))
				for _, p := range rec.Permissions {
					names = append(names, string(p))
				}
				sort.Strings(names)
				byRole[rec.Name] = names
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":    perms,
		"by_role": byRole,
		"system":  systemRoleNames(),
	})
}

// systemRoleNames returns the four declared role names, sorted.
func systemRoleNames() []string {
	names := make([]string, 0, 4)
	for _, r := range authn.AllRoles() {
		names = append(names, string(r))
	}
	sort.Strings(names)
	return names
}

func (s *Server) handleRoleCreate(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}

	var req struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Permissions []string `json:"permissions"`
	}
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	perms, ok := permissionList(w, req.Permissions)
	if !ok {
		return
	}

	rec, err := s.auth.Service.CreateRole(r.Context(), req.Name, req.Description, perms)
	if err != nil {
		s.identityError(w, r, err)
		return
	}

	s.auditIdentity(r, "role.create", "role", rec.ID, map[string]any{
		"name":        rec.Name,
		"permissions": req.Permissions,
	})
	s.refreshAuthorizerTable(r)
	writeJSON(w, http.StatusCreated, roleJSON(rec))
}

func (s *Server) handleRoleUpdatePermissions(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such role")
		return
	}

	var req struct {
		Permissions []string `json:"permissions"`
	}
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	perms, ok := permissionList(w, req.Permissions)
	if !ok {
		return
	}

	rec, err := s.auth.Service.UpdateRolePermissions(r.Context(), id, perms)
	if err != nil {
		s.identityError(w, r, err)
		return
	}

	s.auditIdentity(r, "role.permissions_update", "role", rec.ID, map[string]any{
		"name":        rec.Name,
		"permissions": req.Permissions,
	})
	s.refreshAuthorizerTable(r)
	writeJSON(w, http.StatusOK, roleJSON(rec))
}

func (s *Server) handleRoleDelete(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such role")
		return
	}

	if err := s.auth.Service.DeleteRole(r.Context(), id); err != nil {
		s.identityError(w, r, err)
		return
	}

	s.auditIdentity(r, "role.delete", "role", id, nil)
	s.refreshAuthorizerTable(r)
	w.WriteHeader(http.StatusNoContent)
}

// permissionList converts a submitted string list into declared permissions.
//
// The conversion is a whitelist, not a cast: a value that is not a declared permission is
// refused here with a 400 that names it, rather than stored and silently ignored.
func permissionList(w http.ResponseWriter, names []string) ([]authn.Permission, bool) {
	out := make([]authn.Permission, 0, len(names))
	for _, name := range names {
		if !authn.IsKnownPermission(name) {
			writeProblem(w, http.StatusBadRequest, "unknown_permission",
				"not a permission this build declares: "+name)
			return nil, false
		}
		out = append(out, authn.Permission(name))
	}
	return out, true
}

// refreshAuthorizerTable makes a committed role change take effect without a restart.
//
// The Authorizer holds a snapshot of the custom-role permissions, so a newly created role would
// otherwise stay inert until the next boot. This reloads the snapshot after every write.
//
// A failure to reload is logged, never returned: the role change has already committed, and
// answering 500 for a successful write is worse than a window in which the new role is inert
// (the four system roles still resolve from the compiled-in map, so nothing loses access).
func (s *Server) refreshAuthorizerTable(r *http.Request) {
	if s.auth == nil || s.auth.Service == nil || s.auth.Authz == nil {
		return
	}
	table, err := s.auth.Service.AllRolePermissions(r.Context())
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(),
			"role change committed but the authorizer could not be refreshed; "+
				"the new permissions take effect on next boot",
			"error", err)
		return
	}
	s.auth.Authz.SetTableRoles(table)
}

// auditIdentity records a role or identity change.
//
// A failure is logged rather than returned: the change has already committed, and reporting
// a 500 for a successful change is worse than a gap that the log names.
func (s *Server) auditIdentity(r *http.Request, action, targetType, targetID string, changes map[string]any) {
	if s.pool == nil {
		return
	}
	if err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, s.auditEvent(r, action, targetType, targetID,
			audit.OutcomeSuccess).WithChanges(changes))
	}); err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "audit write failed for an identity change",
			"action", action, "error", err)
	}
}
