// System settings, announcements and the public announcement feed (ADM-02, OPS-09,
// NTF-10, I18N-07, GRW-01).
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
// Announcements are here rather than in a growth package because they are four columns and
// a screen: a title, a body, where it shows and when it stops showing. The public read is
// the only unauthenticated route in this file, and it returns nothing that an operator has
// not published.

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/settings"
	"github.com/jackc/pgx/v5"
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

// ------------------------------------------------------------- announcements --

// announcement is one row's wire shape.
type announcement struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	Placement   string     `json:"placement"`
	Published   bool       `json:"published"`
	PublishedAt *time.Time `json:"published_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	// Live says whether this announcement is currently visible to a customer. It is
	// derived rather than stored so it cannot drift from the clock.
	Live bool `json:"live"`
}

// announcementInput is the create/update body.
type announcementInput struct {
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	Placement string     `json:"placement"`
	Published *bool      `json:"published"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// announcementPlacements is the closed set the table accepts.
var announcementPlacements = []string{"storefront", "dashboard", "both"}

// validPlacement reports whether p is a placement the schema accepts.
func validPlacement(p string) bool {
	for _, candidate := range announcementPlacements {
		if p == candidate {
			return true
		}
	}
	return false
}

// maxAnnouncementTitle bounds the title, matching a heading rather than an essay.
const maxAnnouncementTitle = 200

// maxAnnouncementBody bounds the body. It is rendered in a card, not a page.
const maxAnnouncementBody = 20 << 10

// scanAnnouncement reads one row.
func scanAnnouncement(row interface{ Scan(...any) error }, now time.Time) (announcement, error) {
	var a announcement
	if err := row.Scan(&a.ID, &a.Title, &a.Body, &a.Placement, &a.Published,
		&a.PublishedAt, &a.ExpiresAt, &a.CreatedBy, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return announcement{}, err
	}
	a.Live = a.isLive(now)
	return a, nil
}

// isLive reports whether a customer would see this row now.
func (a announcement) isLive(now time.Time) bool {
	if !a.Published {
		return false
	}
	if a.ExpiresAt != nil && !a.ExpiresAt.After(now) {
		return false
	}
	return true
}

// handleListAnnouncements lists every announcement, published or not.
func (s *Server) handleListAnnouncements(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	rows, err := s.pool.Query(r.Context(), `
		SELECT id, title, body, placement, published, published_at, expires_at,
		       COALESCE(created_by::text, ''), created_at, updated_at
		FROM announcements
		ORDER BY created_at DESC, id DESC`)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "list announcements failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	defer rows.Close()

	now := time.Now().UTC()
	out := make([]announcement, 0, 16)
	for rows.Next() {
		a, err := scanAnnouncement(rows, now)
		if err != nil {
			s.cfg.Logger.ErrorContext(r.Context(), "scan announcement failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, "internal_error",
				"the request could not be completed")
			return
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// handleGetAnnouncement reads one announcement.
func (s *Server) handleGetAnnouncement(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such announcement")
		return
	}
	row := s.pool.QueryRow(r.Context(), `
		SELECT id, title, body, placement, published, published_at, expires_at,
		       COALESCE(created_by::text, ''), created_at, updated_at
		FROM announcements WHERE id = $1`, id)
	a, err := scanAnnouncement(row, time.Now().UTC())
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such announcement")
		return
	}
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "get announcement failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// validateAnnouncement checks the body and returns a normalized copy.
func validateAnnouncement(in announcementInput) (announcementInput, error) {
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		return in, errors.New("a title is required")
	}
	if len(in.Title) > maxAnnouncementTitle {
		return in, errors.New("the title is too long")
	}
	if len(in.Body) > maxAnnouncementBody {
		return in, errors.New("the body is too long")
	}
	in.Placement = strings.TrimSpace(in.Placement)
	if in.Placement == "" {
		in.Placement = "both"
	}
	if !validPlacement(in.Placement) {
		return in, errors.New("placement must be storefront, dashboard or both")
	}
	return in, nil
}

// handleCreateAnnouncement publishes or drafts an announcement.
func (s *Server) handleCreateAnnouncement(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	var in announcementInput
	if !s.decodeJSON(w, r, &in, 64<<10) {
		return
	}
	clean, verr := validateAnnouncement(in)
	if verr != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_announcement", verr.Error())
		return
	}
	published := false
	if clean.Published != nil {
		published = *clean.Published
	}
	user, _ := userFrom(r.Context())

	var id string
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		// published_at is set once, when it is first published. A draft that is later
		// published keeps the instant it went live, which is what the ordering and the
		// "new since you last looked" badge need.
		var publishedAt any
		if published {
			publishedAt = time.Now().UTC()
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO announcements
				(title, body, placement, published, published_at, expires_at, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING id`,
			clean.Title, clean.Body, clean.Placement, published, publishedAt,
			clean.ExpiresAt, nullableUUID(user.ID)).Scan(&id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "announcement.create", "announcement", id,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"title":     clean.Title,
			"placement": clean.Placement,
			"published": published,
		}))
	})
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "create announcement failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	created, err := s.loadAnnouncement(r.Context(), id)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "reload announcement failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// handleUpdateAnnouncement replaces an announcement's fields.
func (s *Server) handleUpdateAnnouncement(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such announcement")
		return
	}
	var in announcementInput
	if !s.decodeJSON(w, r, &in, 64<<10) {
		return
	}
	clean, verr := validateAnnouncement(in)
	if verr != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_announcement", verr.Error())
		return
	}

	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var (
			wasPublished bool
			publishedAt  *time.Time
		)
		if err := tx.QueryRow(ctx, `
			SELECT published, published_at FROM announcements WHERE id = $1 FOR UPDATE`, id).
			Scan(&wasPublished, &publishedAt); err != nil {
			return err
		}
		published := wasPublished
		if clean.Published != nil {
			published = *clean.Published
		}
		// Publish stamps the instant; unpublish keeps the original so a re-publish does
		// not pretend the announcement is brand new. A row that was never published gets
		// the current instant the first time it goes live.
		stamp := publishedAt
		if published && stamp == nil {
			now := time.Now().UTC()
			stamp = &now
		}
		if _, err := tx.Exec(ctx, `
			UPDATE announcements
			SET title = $2, body = $3, placement = $4, published = $5,
			    published_at = $6, expires_at = $7, updated_at = now()
			WHERE id = $1`,
			id, clean.Title, clean.Body, clean.Placement, published, stamp,
			clean.ExpiresAt); err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "announcement.update", "announcement", id,
			audit.OutcomeSuccess).WithChanges(map[string]any{
			"title":         clean.Title,
			"placement":     clean.Placement,
			"published":     published,
			"was_published": wasPublished,
		}))
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such announcement")
		return
	}
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "update announcement failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	updated, err := s.loadAnnouncement(r.Context(), id)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// handleDeleteAnnouncement removes an announcement.
//
// Delete is allowed here, unlike an invoice: an announcement has no legal weight and no
// record references it, so a typo published for a minute should be removable rather than
// left on the storefront forever. The audit row keeps the fact that it existed.
func (s *Server) handleDeleteAnnouncement(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	id := r.PathValue("id")
	if !portal.ValidID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such announcement")
		return
	}

	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM announcements WHERE id = $1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		return audit.Record(ctx, tx, s.auditEvent(r, "announcement.delete", "announcement", id,
			audit.OutcomeSuccess))
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such announcement")
		return
	}
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "delete announcement failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

// loadAnnouncement reads one row after a write, so the response is the committed state.
func (s *Server) loadAnnouncement(ctx context.Context, id string) (announcement, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, title, body, placement, published, published_at, expires_at,
		       COALESCE(created_by::text, ''), created_at, updated_at
		FROM announcements WHERE id = $1`, id)
	return scanAnnouncement(row, time.Now().UTC())
}

// handlePublicAnnouncements is the storefront and dashboard feed.
//
// It returns only published, unexpired rows, and only the fields a customer may see: the
// creator and the draft state are operator information. `?placement=storefront|dashboard`
// narrows it; `both` matches either.
func (s *Server) handlePublicAnnouncements(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		// The storefront must still render without a database rather than fail its whole
		// render on an announcement fetch.
		writeJSON(w, http.StatusOK, map[string]any{"data": []announcement{}})
		return
	}

	placement := strings.TrimSpace(r.URL.Query().Get("placement"))
	if placement != "" && !validPlacement(placement) {
		writeProblem(w, http.StatusBadRequest, "invalid_placement",
			"placement must be storefront, dashboard or both")
		return
	}

	now := time.Now().UTC()
	// The predicate is written so the partial index on (published, published_at) is usable:
	// an announcement feed is read on every storefront render.
	rows, err := s.pool.Query(r.Context(), `
		SELECT id, title, body, placement, published, published_at, expires_at,
		       ''::text, created_at, updated_at
		FROM announcements
		WHERE published
		  AND published_at IS NOT NULL
		  AND published_at <= $1
		  AND (expires_at IS NULL OR expires_at > $1)
		  AND ($2 = '' OR placement = $2 OR placement = 'both')
		ORDER BY published_at DESC, id DESC
		LIMIT 20`, now, placement)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "public announcements failed", "error", err)
		// An announcement feed is decoration: a failure here must not take down the
		// storefront, so the response is an empty feed rather than a 500.
		writeJSON(w, http.StatusOK, map[string]any{"data": []announcement{}})
		return
	}
	defer rows.Close()

	out := make([]announcement, 0, 8)
	for rows.Next() {
		a, err := scanAnnouncement(rows, now)
		if err != nil {
			s.cfg.Logger.ErrorContext(r.Context(), "scan public announcement failed", "error", err)
			writeJSON(w, http.StatusOK, map[string]any{"data": []announcement{}})
			return
		}
		// The public feed does not expose who wrote it.
		a.CreatedBy = ""
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"data": []announcement{}})
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// nullableUUID renders an empty id as SQL NULL.
func nullableUUID(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
