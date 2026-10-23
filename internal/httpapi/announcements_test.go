package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// Announcements: the feed a customer sees, and the setting that governs it.
//
// The defect these tests exist for is an *inert* feature: `announcements.enabled` was documented
// as "whether the announcement feed is shown to customers", the settings page stored it, and no
// Go code read it. An operator switched the feed off, every published notice still showed, and
// nothing in the system said why. A setting nobody reads is worse than no setting, because the
// operator believes it worked.
//
// So the central test below does not assert a column: it toggles the setting through the staff
// settings API — the path an operator actually uses — and asserts that what a customer's request
// receives changes. The others pin the clock: a row whose `published_at` has not arrived and a
// row whose `expires_at` has passed are both invisible to a customer, and both are *named* on
// the staff list rather than left as a mystery `live: false`.

// announcementWire is the wire shape the tests read, including the derived state.
type announcementWire struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Placement   string  `json:"placement"`
	Published   bool    `json:"published"`
	PublishedAt *string `json:"published_at"`
	ExpiresAt   *string `json:"expires_at"`
	Live        bool    `json:"live"`
	State       string  `json:"state"`
}

// announcementFeed is the public feed's envelope. `enabled` is the setting as the server read it.
type announcementFeed struct {
	Data    []announcementWire `json:"data"`
	Enabled bool               `json:"enabled"`
}

// readFeed fetches the public feed as a signed-out visitor would.
func readFeed(t *testing.T, env *staffEnv, placement string) announcementFeed {
	t.Helper()
	path := "/api/v1/announcements"
	if placement != "" {
		path += "?placement=" + placement
	}
	rec := env.get(t, path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public feed: status %d; body %s", rec.Code, rec.Body)
	}
	var body announcementFeed
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode feed: %v\n%s", err, rec.Body)
	}
	return body
}

// setAnnouncementsEnabled writes the setting through the staff settings API, which is the path an
// operator takes. Writing the row with SQL instead would test the column, not the contract.
func setAnnouncementsEnabled(t *testing.T, env *staffEnv, enabled bool) {
	t.Helper()
	value := "false"
	if enabled {
		value = "true"
	}
	rec := env.doJSON(t, http.MethodPut, "/api/v1/staff/settings", env.admin, env.adminCSRF,
		map[string]any{"group": "announcements", "values": map[string]string{"enabled": value}})
	if rec.Code != http.StatusOK {
		t.Fatalf("write announcements.enabled=%s: status %d; body %s", value, rec.Code, rec.Body)
	}
}

// publishAnnouncement creates a published announcement through the staff API and returns its id.
func publishAnnouncement(t *testing.T, env *staffEnv, title string) string {
	t.Helper()
	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/announcements", env.admin, env.adminCSRF,
		map[string]any{
			"title":     title,
			"body":      "The panel is read-only from 02:00 to 03:00 UTC.",
			"placement": "both",
			"published": true,
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create announcement: status %d; body %s", rec.Code, rec.Body)
	}
	var created announcementWire
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	return created.ID
}

// staffAnnouncement reads one row from the staff surface.
func staffAnnouncement(t *testing.T, env *staffEnv, id string) announcementWire {
	t.Helper()
	rec := env.get(t, "/api/v1/staff/announcements/"+id, env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("staff read: status %d; body %s", rec.Code, rec.Body)
	}
	var row announcementWire
	if err := json.Unmarshal(rec.Body.Bytes(), &row); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	return row
}

// staffList reads the staff list and the `enabled` flag beside it.
func staffList(t *testing.T, env *staffEnv) (rows []announcementWire, enabled bool) {
	t.Helper()
	rec := env.get(t, "/api/v1/staff/announcements", env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("staff list: status %d; body %s", rec.Code, rec.Body)
	}
	var body struct {
		Data    []announcementWire `json:"data"`
		Enabled bool               `json:"enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	return body.Data, body.Enabled
}

// The setting an operator was promised actually governs the feed.
//
// This is the whole point of the task: the toggle is written through the settings API and the
// public feed — the exact request a storefront makes — changes. Before this test, the write
// succeeded, the row changed, and the feed was identical.
func TestAnnouncementFeedFollowsEnabledSetting(t *testing.T) {
	env := newStaffEnv(t)
	// The suite truncates installation-wide rows, so the setting starts at its default. State
	// it rather than inherit it from another test's leftovers.
	setAnnouncementsEnabled(t, env, true)

	publishAnnouncement(t, env, "Scheduled maintenance on Sunday")

	feed := readFeed(t, env, "")
	if !feed.Enabled {
		t.Fatal("the feed reports enabled=false before anything switched it off")
	}
	if len(feed.Data) != 1 || feed.Data[0].Title != "Scheduled maintenance on Sunday" {
		t.Fatalf("feed with the setting on = %+v, want the published announcement", feed.Data)
	}

	// The operator switches the feed off. Nothing about the announcement row changes.
	setAnnouncementsEnabled(t, env, false)

	feed = readFeed(t, env, "")
	if feed.Enabled {
		t.Fatal("the feed still reports enabled=true after announcements.enabled=false")
	}
	if len(feed.Data) != 0 {
		t.Fatalf("a switched-off feed returned rows: %+v", feed.Data)
	}
	// The placement-narrowed feed is gated the same way: the storefront and the dashboard are
	// two readers of one setting, not two settings.
	if got := readFeed(t, env, "storefront"); len(got.Data) != 0 || got.Enabled {
		t.Fatalf("storefront feed with the setting off = %+v", got)
	}
	if got := readFeed(t, env, "dashboard"); len(got.Data) != 0 || got.Enabled {
		t.Fatalf("dashboard feed with the setting off = %+v", got)
	}

	// The announcement itself is untouched: it is still live, and the staff list says the feed
	// is switched off. That distinction is what tells an operator "publish" was not the problem.
	rows, enabled := staffList(t, env)
	if enabled {
		t.Fatal("the staff list reports the feed as enabled while it is switched off")
	}
	if len(rows) != 1 || !rows[0].Live || rows[0].State != "live" {
		t.Fatalf("the announcement changed when the feed was switched off: %+v", rows)
	}

	// Switching it back on restores the feed without republishing anything.
	setAnnouncementsEnabled(t, env, true)
	if feed := readFeed(t, env, ""); len(feed.Data) != 1 || !feed.Enabled {
		t.Fatalf("the feed did not come back after re-enabling: %+v", feed)
	}
}

// A published announcement whose publish instant has not arrived is not live, and the staff list
// names it "scheduled" rather than leaving a `live: false` the operator has to decode.
func TestAnnouncementScheduledIsNotLive(t *testing.T) {
	env := newStaffEnv(t)
	future := time.Now().UTC().Add(2 * time.Hour)

	var id string
	if err := env.pool.QueryRow(context.Background(), `
		INSERT INTO announcements (title, body, placement, published, published_at)
		VALUES ($1, $2, 'both', true, $3) RETURNING id`,
		"Next week's migration", "Read-only from 02:00.", future).Scan(&id); err != nil {
		t.Fatalf("insert scheduled announcement: %v", err)
	}

	if got := readFeed(t, env, ""); len(got.Data) != 0 {
		t.Fatalf("a scheduled announcement is in the public feed: %+v", got.Data)
	}

	row := staffAnnouncement(t, env, id)
	if row.State != "scheduled" {
		t.Fatalf("state = %q, want scheduled", row.State)
	}
	if row.Live {
		t.Fatal("a scheduled announcement reports live=true")
	}
	if !row.Published || row.PublishedAt == nil {
		t.Fatalf("the row lost its publish state: %+v", row)
	}
}

// An expired announcement is "expired", not "published, not showing": the two are different
// answers to "why does nobody see this".
func TestAnnouncementExpiredIsNamed(t *testing.T) {
	env := newStaffEnv(t)
	past := time.Now().UTC().Add(-time.Hour)

	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/announcements", env.admin, env.adminCSRF,
		map[string]any{
			"title":      "Yesterday's outage",
			"body":       "Resolved at 03:10 UTC.",
			"placement":  "both",
			"published":  true,
			"expires_at": past.Format(time.RFC3339),
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d; body %s", rec.Code, rec.Body)
	}
	var created announcementWire
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	if created.State != "expired" {
		t.Fatalf("state = %q, want expired", created.State)
	}
	if created.Live {
		t.Fatal("an expired announcement reports live=true")
	}
	if got := readFeed(t, env, ""); len(got.Data) != 0 {
		t.Fatalf("an expired announcement is in the public feed: %+v", got.Data)
	}
}

// A draft is a draft, and a live row is live: the four states are the four answers, and the
// staff screen renders them rather than guessing.
func TestAnnouncementStatesAreDistinct(t *testing.T) {
	env := newStaffEnv(t)

	// A draft.
	rec := env.doJSON(t, http.MethodPost, "/api/v1/staff/announcements", env.admin, env.adminCSRF,
		map[string]any{"title": "Not yet", "placement": "storefront"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create draft: status %d; body %s", rec.Code, rec.Body)
	}
	var draft announcementWire
	if err := json.Unmarshal(rec.Body.Bytes(), &draft); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	if draft.State != "draft" || draft.Live {
		t.Fatalf("draft = %+v, want state=draft and live=false", draft)
	}

	// A live row.
	liveID := publishAnnouncement(t, env, "Live right now")
	live := staffAnnouncement(t, env, liveID)
	if live.State != "live" || !live.Live {
		t.Fatalf("live = %+v, want state=live and live=true", live)
	}

	// Both states are on the same list, which is what makes the badge meaningful: the operator
	// is comparing two rows at one instant, not reading two screens a minute apart.
	rows, enabled := staffList(t, env)
	if !enabled {
		t.Fatal("the staff list reports the feed as switched off on a fresh installation")
	}
	states := make(map[string]int, len(rows))
	for _, row := range rows {
		states[row.State]++
	}
	if states["draft"] != 1 || states["live"] != 1 {
		t.Fatalf("states = %v, want one draft and one live", states)
	}

	// Only the live one reaches a customer.
	feed := readFeed(t, env, "")
	if len(feed.Data) != 1 || feed.Data[0].ID != liveID {
		t.Fatalf("feed = %+v, want only the live row", feed.Data)
	}
}

// The feed is not cacheable.
//
// A browser harness found this the only way it can be found: the operator switched the feed off
// through the settings page, the customer reloaded, and the notice was still there — answered from
// the browser's own copy of a response the server had marked `public, max-age=60`. The setting was
// read correctly on every request; no request was made. A cache that outlives the switch is the
// same defect as a setting nobody reads, one layer further out.
func TestPublicAnnouncementsAreNotCached(t *testing.T) {
	env := newStaffEnv(t)
	publishAnnouncement(t, env, "Maintenance tonight")

	for _, tc := range []struct {
		name    string
		enabled bool
	}{
		{"with the feed on", true},
		{"with the feed off", false},
	} {
		setAnnouncementsEnabled(t, env, tc.enabled)
		rec := env.get(t, "/api/v1/announcements", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", tc.name, rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("%s: Cache-Control = %q, want no-store", tc.name, cc)
		}
	}
}
