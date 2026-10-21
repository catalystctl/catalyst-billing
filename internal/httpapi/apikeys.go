package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/jackc/pgx/v5"
)

// API keys over HTTP (Gate C5).
//
// The list never contains a key. The create response contains one, once, and the UI has to show
// it immediately: there is no endpoint that reveals it again, because the server does not have it.
//
// Keys are per-user, not per-tenant: an operator creates a key for their own automation, and it
// carries exactly their permissions narrowed by the scopes they chose. Every route still acts on
// the caller's own keys (the user id comes from the session, never the URL), so a key can never be
// minted for, or revoked from, somebody else.
//
// They live under /api/v1/staff because the permission that gates them (apikey:manage) is a staff
// permission and no customer role holds it. The staff-only guard is applied as well, so that stays
// true even if a customer role is ever given the permission by mistake.

func init() {
	RegisterRoutes("90-api-keys", registerAPIKeys)
}

func registerAPIKeys(r *router) {
	r.staffGet("/api/v1/staff/api-keys", authn.PermAPIKeyManage, r.s.handleAPIKeyList)
	r.staffMutation(http.MethodPost, "/api/v1/staff/api-keys",
		authn.PermAPIKeyManage, r.s.handleAPIKeyCreate)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/api-keys/{id}",
		authn.PermAPIKeyManage, r.s.handleAPIKeyRevoke)
}

// apiKeyResponse is the list shape: no secret.
func apiKeyResponse(k authn.APIKey) map[string]any {
	out := map[string]any{
		"id":           k.ID,
		"name":         k.Name,
		"prefix":       k.Prefix,
		"scopes":       k.Scopes,
		"created_at":   k.CreatedAt.UTC(),
		"last_used_at": nil,
		"expires_at":   nil,
		"revoked_at":   nil,
		"revoked":      k.Revoked(),
		"expired":      k.Expired(time.Now().UTC()),
	}
	if k.LastUsedAt != nil {
		out["last_used_at"] = k.LastUsedAt.UTC()
	}
	if k.ExpiresAt != nil {
		out["expires_at"] = k.ExpiresAt.UTC()
	}
	if k.RevokedAt != nil {
		out["revoked_at"] = k.RevokedAt.UTC()
	}
	return out
}

func (s *Server) handleAPIKeyList(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	user, _ := userFrom(r.Context())

	keys, err := s.auth.Service.ListAPIKeys(r.Context(), user.ID)
	if err != nil {
		s.staffError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, apiKeyResponse(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) handleAPIKeyCreate(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	user, _ := userFrom(r.Context())

	var req struct {
		Name string `json:"name"`
		// Scopes narrow the owner's permissions. An empty list means "everything the owner holds",
		// which is what an operator creating a key for their own automation expects.
		Scopes []string `json:"scopes"`
		// ExpiresInDays is optional; zero means the key does not expire.
		ExpiresInDays int `json:"expires_in_days"`
	}
	if !s.decodeJSON(w, r, &req, 8<<10) {
		return
	}
	if req.Name == "" {
		writeProblem(w, http.StatusBadRequest, "name_required", "a key needs a name")
		return
	}
	if req.ExpiresInDays < 0 || req.ExpiresInDays > 3650 {
		writeProblem(w, http.StatusBadRequest, "invalid_expiry",
			"expires_in_days must be between 1 and 3650, or absent")
		return
	}

	// A scope the caller does not hold must be refused: a key that claims a permission its owner
	// lacks is a key that silently does less than its owner believes.
	if s.auth.Authz != nil {
		held := s.auth.Authz.Permissions(user)
		for _, scope := range req.Scopes {
			if _, ok := held[authn.Permission(scope)]; !ok {
				writeProblem(w, http.StatusForbidden, "scope_not_held",
					"you cannot grant a scope you do not hold: "+scope)
				return
			}
		}
	}

	created, err := s.auth.Service.CreateAPIKey(r.Context(), authn.NewAPIKeyRequest{
		UserID:    user.ID,
		Name:      req.Name,
		Scopes:    req.Scopes,
		ExpiresIn: time.Duration(req.ExpiresInDays) * 24 * time.Hour,
	})
	switch {
	case err == nil:
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "api key creation failed", "error", err)
		writeProblem(w, http.StatusBadRequest, "invalid_key_request", err.Error())
		return
	}

	s.auditKeyEvent(r, "user.api_key_created", created.ID, map[string]any{
		"name":   created.Name,
		"scopes": created.Scopes,
	})

	out := apiKeyResponse(created.APIKey)
	// The key itself, once. `shown_once` is part of the contract so a UI cannot quietly omit the
	// warning.
	out["key"] = created.Key
	out["shown_once"] = true
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleAPIKeyRevoke(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	user, _ := userFrom(r.Context())
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such key")
		return
	}

	err := s.auth.Service.RevokeAPIKey(r.Context(), user.ID, id)
	switch {
	case err == nil:
	case errors.Is(err, authn.ErrAPIKeyNotFound):
		// Someone else's key is indistinguishable from a nonexistent one.
		writeProblem(w, http.StatusNotFound, "not_found", "no such key")
		return
	default:
		s.staffError(w, r, err)
		return
	}

	s.auditKeyEvent(r, "user.api_key_revoked", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

// auditKeyEvent records a key lifecycle change.
func (s *Server) auditKeyEvent(r *http.Request, action, keyID string, changes map[string]any) {
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, s.auditEvent(r, action, "api_key", keyID,
			audit.OutcomeSuccess).WithChanges(changes))
	})
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "audit write failed for an api key change",
			"action", action, "error", err)
	}
}
