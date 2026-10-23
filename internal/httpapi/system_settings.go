// System settings (ADM-02, OPS-09, NTF-10, I18N-07, GRW-01).
//
// The settings surface is deliberately a *bulk* surface. An operator edits a form and
// saves it; a request per keystroke would write a partial configuration and an audit row
// per field. So a read is one group (or all of them) and a write is one group, validated
// as a whole before anything is stored, with one audit row per changed key.
//
// Two rules make it safe to expose credentials through an HTTP page:
//
//   - A secret is never returned. The settings package redacts it at the source, so no
//     handler has to remember, and the response carries `is_set` instead. A client renders
//     an empty password field and submits nothing to leave it unchanged.
//   - A write is audited inside the same transaction that stores it, through the same
//     request auditor the money paths use, so the audit row carries the operator, the
//     correlation id and the source address.
//
// The announcement routes are registered here — they are part of the same operator surface —
// but their handlers live in announcements.go, together with the public read that is the only
// unauthenticated route either file serves.

package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/settings"
)

func init() {
	RegisterRoutes("44-operator-settings", registerOperatorSettings)
}

func registerOperatorSettings(r *router) {
	// Reading settings requires settings:write as well. A settings page is one screen:
	// separating read from write would let a support agent read the SMTP host and the
	// gateway configuration, which is reconnaissance rather than support.
	r.staffGet("/api/v1/staff/settings", authn.PermSettingsWrite, r.s.handleGetSettings)
	r.staffMutation(http.MethodPut, "/api/v1/staff/settings", authn.PermSettingsWrite,
		r.s.handlePutSettings)

	// Announcements: staff CRUD, public feed.
	r.staffGet("/api/v1/staff/announcements", authn.PermAnnouncementWrite,
		r.s.handleListAnnouncements)
	r.staffGet("/api/v1/staff/announcements/{id}", authn.PermAnnouncementWrite,
		r.s.handleGetAnnouncement)
	r.staffMutation(http.MethodPost, "/api/v1/staff/announcements",
		authn.PermAnnouncementWrite, r.s.handleCreateAnnouncement)
	r.staffMutation(http.MethodPut, "/api/v1/staff/announcements/{id}",
		authn.PermAnnouncementWrite, r.s.handleUpdateAnnouncement)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/announcements/{id}",
		authn.PermAnnouncementWrite, r.s.handleDeleteAnnouncement)

	// The storefront and the customer dashboard read this. It is public because the
	// storefront is public: a visitor who cannot see "we are closed for maintenance" has
	// no way to find out before registering.
	r.publicGet("/api/v1/announcements", r.s.handlePublicAnnouncements)
}

// settingsStore returns the settings accessor.
//
// It is constructed per call rather than cached on Server: it is a pool handle and a
// clock, and this package deliberately avoids adding shared mutable state to Server for
// something this cheap.
func (s *Server) settingsStore() *settings.Store { return settings.New(s.pool) }

// settingsError maps a settings error to a problem document.
func (s *Server) settingsError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, settings.ErrUnknownGroup):
		writeProblem(w, http.StatusNotFound, "unknown_group", "no such settings group")
	case errors.Is(err, settings.ErrUnknownKey):
		// The message names the key the caller sent, never a stored value, so it is safe
		// to return: a client that sent `company_nmae` needs to be told which field was
		// wrong, and it already knows the value it sent.
		writeProblem(w, http.StatusBadRequest, "unknown_setting", err.Error())
	case errors.Is(err, settings.ErrInvalidValue):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_value", err.Error())
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "settings operation failed",
			"path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
	}
}

// settingsGroupJSON renders one group for the wire.
//
// `keys` is always an array, never null: a client iterating it should not have to
// distinguish "no keys" from "field absent".
func settingsGroupJSON(g settings.GroupValues) map[string]any {
	keys := make([]map[string]any, 0, len(g.Keys))
	for _, v := range g.Keys {
		entry := map[string]any{
			"group":      v.Group,
			"key":        v.Key,
			"kind":       v.Kind,
			"value":      v.Value,
			"is_secret":  v.IsSecret,
			"is_set":     v.IsSet,
			"updated_at": nil,
		}
		if v.UpdatedAt != nil {
			entry["updated_at"] = v.UpdatedAt.UTC()
		}
		keys = append(keys, entry)
	}
	return map[string]any{"group": g.Group, "keys": keys}
}

// handleGetSettings returns every group, or one group when `?group=` names it.
//
// Secrets are redacted by the settings package, so this handler cannot leak one even by
// accident: there is no branch here that reads the stored text.
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	store := s.settingsStore()

	if group := strings.TrimSpace(r.URL.Query().Get("group")); group != "" {
		values, err := store.Group(r.Context(), group)
		if err != nil {
			s.settingsError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, settingsGroupJSON(settings.GroupValues{Group: group, Keys: values}))
		return
	}

	groups, err := store.All(r.Context())
	if err != nil {
		s.settingsError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		out = append(out, settingsGroupJSON(g))
	}
	// A settings page must never be served from a cache: the whole point is that it shows
	// the current configuration, and a proxy that caches it would show one operator
	// another operator's last save.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"data":   out,
		"groups": settings.Groups(),
	})
}

// settingsPutBody is the bulk upsert body.
type settingsPutBody struct {
	// Group is the group being written. One request writes one group, so a form that
	// fails cannot half-apply across groups.
	Group string `json:"group"`
	// Values is key -> text value. A secret sent as "[redacted]" is skipped, which is
	// what makes "load the page, change the company name, save" safe.
	Values map[string]string `json:"values"`
}

// handlePutSettings replaces a group's values atomically.
func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}

	var body settingsPutBody
	if !s.decodeJSON(w, r, &body, 256<<10) {
		return
	}
	body.Group = strings.TrimSpace(body.Group)
	if body.Group == "" {
		writeProblem(w, http.StatusBadRequest, "group_required",
			"the request must name a settings group")
		return
	}
	if body.Values == nil {
		writeProblem(w, http.StatusBadRequest, "values_required",
			"the request must carry the values to write")
		return
	}

	user, _ := userFrom(r.Context())
	values, err := s.settingsStore().Update(r.Context(), body.Group, body.Values, user.ID,
		s.auditorFor(r, ""))
	if err != nil {
		s.settingsError(w, r, err)
		return
	}

	// The audit rows are written by the store inside the same transaction, so there is
	// nothing to record here. Publishing an event after commit is what lets another
	// operator's open settings page know it is stale.
	s.publish(events.Event{
		Kind:       "settings.updated",
		Permission: string(authn.PermSettingsWrite),
		Data:       map[string]any{"group": body.Group},
	})

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK,
		settingsGroupJSON(settings.GroupValues{Group: body.Group, Keys: values}))
}
