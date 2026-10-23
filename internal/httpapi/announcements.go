// Announcements: the staff CRUD, the public feed and the setting that governs both.
//
// An announcement is four columns and a screen: a title, a body, where it shows and when it
// stops showing. What earns it its own file is the clock. `published` alone does not mean a
// customer sees it — the row also needs a `published_at` that has arrived and an `expires_at`
// that has not. The server derives that as `state` on every row, so the staff screen and the
// customer feed cannot disagree about the same instant. Before `state` existed the staff screen
// had to infer it from three columns, and a row whose `published_at` was in the future rendered
// as "Live" while the public feed — which does check the clock — showed nothing.
//
// The feed is governed by `announcements.enabled`, which is a setting an operator was promised:
// "whether the announcement feed is shown to customers". It is READ here, on the public feed and
// on the staff list, and a switched-off feed answers `enabled: false` with no rows rather than
// pretending the announcements are not live. A setting that only writes is worse than no setting
// at all: the operator switches the feed off, sees every published notice still on the storefront,
// and stops trusting the settings page.
//
// The public read is unauthenticated because the storefront is: a visitor who cannot see "we are
// closed for maintenance" has no way to find out before registering.
package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/portal"
	"github.com/jackc/pgx/v5"
)

// The states a row can be in, as the server's clock sees it.
//
// They travel on the wire as a string rather than as three timestamps for the client to compare,
// because the comparison that matters is against the server's clock, not the browser's. A screen
// that re-derives "is this live" in a browser with a skewed clock tells the operator something
// different from what a customer's request is told.
const (
	// announcementDraft was never published. Only staff see it.
	announcementDraft = "draft"
	// announcementScheduled is published, but its publish instant has not arrived or is
	// missing, so the feed's predicate excludes it. This is the state that looks like a bug
	// and is not one.
	announcementScheduled = "scheduled"
	// announcementExpired is published, but its expiry has passed.
	announcementExpired = "expired"
	// announcementLive is what a customer sees right now.
	announcementLive = "live"
)

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
	// State names *why* Live is what it is: draft, scheduled, expired or live. `live: false`
	// alone cannot distinguish a draft from a notice that stopped showing an hour ago, and
	// those are different problems for the operator reading the screen.
	State string `json:"state"`
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

// announcementsEnabled reports whether an operator has left the announcement feed switched on.
//
// This is the read that makes `announcements.enabled` a setting rather than a stored string. It
// is the contract the settings page states: "whether the announcement feed is shown to customers".
// The default in internal/settings is `true`, so a fresh installation shows announcements until
// an operator says otherwise.
func (s *Server) announcementsEnabled(ctx context.Context) (bool, error) {
	return s.settingsStore().Bool(ctx, "announcements", "enabled")
}

// scanAnnouncement reads one row.
func scanAnnouncement(row interface{ Scan(...any) error }, now time.Time) (announcement, error) {
	var a announcement
	if err := row.Scan(&a.ID, &a.Title, &a.Body, &a.Placement, &a.Published,
		&a.PublishedAt, &a.ExpiresAt, &a.CreatedBy, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return announcement{}, err
	}
	a.State = a.state(now)
	a.Live = a.State == announcementLive
	return a, nil
}

// state reports which of the four states a row is in at `now`.
//
// The order matters. Expiry is checked before the publish instant, because a row whose window is
// empty is expired whatever else is true of it: an announcement scheduled for next week that also
// expired an hour ago will never show, and "expired" is the label that says so.
func (a announcement) state(now time.Time) string {
	switch {
	case !a.Published:
		return announcementDraft
	case a.ExpiresAt != nil && !a.ExpiresAt.After(now):
		return announcementExpired
	case a.PublishedAt == nil || a.PublishedAt.After(now):
		// Published with no instant, or with one the clock has not reached. The public feed's
		// predicate requires `published_at <= now`, so both are "not yet", not "live".
		return announcementScheduled
	default:
		return announcementLive
	}
}

// handleListAnnouncements lists every announcement, published or not.
//
// The response carries `enabled` alongside the rows: with the feed switched off every row may be
// live and still be invisible, and a staff screen that lists them without saying so is the exact
// trap this setting used to be.
func (s *Server) handleListAnnouncements(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database is configured")
		return
	}
	// A settings read failure must not hide the announcements themselves: the operator
	// can still publish and unpublish. The flag is then *omitted* rather than sent as false —
	// "nobody could read the setting" is not "the feed is switched off", and a screen that
	// claimed the latter would send the operator to a settings page that already says on.
	enabled, enabledErr := s.announcementsEnabled(r.Context())
	if enabledErr != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "read announcements.enabled failed", "error", enabledErr)
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
	body := map[string]any{"data": out}
	if enabledErr == nil {
		body["enabled"] = enabled
	}
	writeJSON(w, http.StatusOK, body)
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
		s.cfg.Logger.ErrorContext(r.Context(), "reload announcement failed", "error", err)
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
// It returns only live rows, and only the fields a customer may see: the creator and the draft
// state are operator information. `?placement=storefront|dashboard` narrows it; `both` matches
// either. `enabled` in the response is the setting, not a guess: the client renders a feed when
// it is true and nothing at all when it is false.
func (s *Server) handlePublicAnnouncements(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		// The storefront must still render without a database rather than fail its whole
		// render on an announcement fetch. Without a database there is neither a feed nor a
		// setting to read, so the answer is "nothing to show".
		writeJSON(w, http.StatusOK, map[string]any{"data": []announcement{}, "enabled": false})
		return
	}

	placement := strings.TrimSpace(r.URL.Query().Get("placement"))
	if placement != "" && !validPlacement(placement) {
		writeProblem(w, http.StatusBadRequest, "invalid_placement",
			"placement must be storefront, dashboard or both")
		return
	}

	enabled, err := s.announcementsEnabled(r.Context())
	if err != nil {
		// Fail closed. Showing a notice against the operator's explicit switch is a contract
		// violation; showing nothing is what a switched-off feed does anyway, and the log line
		// is where the difference between "off" and "unreadable" is recorded.
		s.cfg.Logger.ErrorContext(r.Context(), "read announcements.enabled failed", "error", err)
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"data": []announcement{}, "enabled": false})
		return
	}
	if !enabled {
		// The feed is switched off in system settings. Nothing is returned, and nothing is
		// cached: the operator who switches it back on must not be behind a 60-second cache.
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"data": []announcement{}, "enabled": false})
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
		writeJSON(w, http.StatusOK, map[string]any{"data": []announcement{}, "enabled": true})
		return
	}
	defer rows.Close()

	out := make([]announcement, 0, 8)
	for rows.Next() {
		a, err := scanAnnouncement(rows, now)
		if err != nil {
			s.cfg.Logger.ErrorContext(r.Context(), "scan public announcement failed", "error", err)
			writeJSON(w, http.StatusOK, map[string]any{"data": []announcement{}, "enabled": true})
			return
		}
		// The public feed does not expose who wrote it.
		a.CreatedBy = ""
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"data": []announcement{}, "enabled": true})
		return
	}
	// No-store, deliberately, and not the `public, max-age=60` this route used to send. A cached
	// feed is a feed that survives the switch that was supposed to hide it: an operator switches
	// `announcements.enabled` off, a customer reloads within the minute, and the browser answers
	// from its own cache with the notice the operator just hid. That is the same failure as a
	// setting nobody reads — the control exists and does not control — one layer further out. The
	// cost is one indexed query per storefront render, which is what the partial index on
	// (published, published_at) exists for.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "enabled": true})
}

// nullableUUID renders an empty id as SQL NULL.
func nullableUUID(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
