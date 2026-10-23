package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Renaming and removing a catalogue server.
//
// A server is a provisioner target: the node a product is sold onto. Creating one and listing
// the list were served and tested; renaming and deleting were served and unreachable, so a
// target created with the wrong node id could not be corrected and a target created by mistake
// could not be taken away.
//
// The delete is the interesting half. It refuses while a product still points at the server
// (`409 in_use`), because deleting would silently move those products back to their plan's
// region — a provisioning change nobody asked for. Disabling is the reversible alternative, and
// the refusal says so.

// createServer creates a target through the API and returns its id.
func createServer(t *testing.T, env *staffEnv, body map[string]any) string {
	t.Helper()
	rec := env.postJSON(t, "/api/v1/staff/catalog/servers", env.admin, env.adminCSRF, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create server: %d %s", rec.Code, rec.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode server: %v", err)
	}
	return created.ID
}

// A rename changes what the next purchase is provisioned onto and keeps the identity of the
// target, so the id, the products pointing at it and the services already created are untouched.
func TestServerRenameKeepsItsIdentity(t *testing.T) {
	env := newStaffEnv(t)
	id := createServer(t, env, map[string]any{
		"slug": "eu-west", "name": "EU West", "provider": "hetzner", "region": "eu-west",
		"node": "node-7", "enabled": true,
	})

	rec := env.putJSON(t, "/api/v1/staff/catalog/servers/"+id, env.admin, env.adminCSRF, map[string]any{
		"slug": "eu-central", "name": "EU Central", "provider": "hetzner", "region": "eu-central",
		"node": "node-9", "enabled": true, "sort_order": 3,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("rename server: %d %s", rec.Code, rec.Body)
	}
	var renamed struct {
		ID       string `json:"id"`
		Slug     string `json:"slug"`
		Name     string `json:"name"`
		Region   string `json:"region"`
		Node     string `json:"node"`
		Sort     int    `json:"sort_order"`
		Products int    `json:"product_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &renamed); err != nil {
		t.Fatalf("decode renamed: %v", err)
	}
	if renamed.ID != id {
		t.Fatalf("id changed on rename: %q → %q", id, renamed.ID)
	}
	if renamed.Name != "EU Central" || renamed.Slug != "eu-central" ||
		renamed.Region != "eu-central" || renamed.Node != "node-9" || renamed.Sort != 3 {
		t.Fatalf("renamed server = %+v", renamed)
	}

	// The list the product dialog reads reports the new values, so a rename is not a change that
	// only the response knows about.
	rec = env.get(t, "/api/v1/staff/catalog/servers", env.billing)
	if rec.Code != http.StatusOK {
		t.Fatalf("list servers: %d", rec.Code)
	}
	var listed struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Node string `json:"node"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	found := false
	for _, srv := range listed.Data {
		if srv.ID == id {
			found = true
			if srv.Name != "EU Central" || srv.Node != "node-9" {
				t.Fatalf("listed server = %+v, want the renamed values", srv)
			}
		}
	}
	if !found {
		t.Fatal("the renamed server is missing from the list")
	}
}

// A rename onto another server's slug is a 409 rather than a duplicate row, and renaming a
// server that does not exist is a 404.
func TestServerRenameRefusesATakenSlug(t *testing.T) {
	env := newStaffEnv(t)
	first := createServer(t, env, map[string]any{"slug": "first", "name": "First"})
	createServer(t, env, map[string]any{"slug": "second", "name": "Second"})

	rec := env.putJSON(t, "/api/v1/staff/catalog/servers/"+first, env.admin, env.adminCSRF,
		map[string]any{"slug": "second", "name": "First"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("rename onto a taken slug: %d %s", rec.Code, rec.Body)
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil || problem.Code != "slug_taken" {
		t.Fatalf("problem = %s", rec.Body)
	}

	rec = env.putJSON(t, "/api/v1/staff/catalog/servers/00000000-0000-0000-0000-000000000000",
		env.admin, env.adminCSRF, map[string]any{"slug": "ghost", "name": "Ghost"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("renaming a missing server: %d %s", rec.Code, rec.Body)
	}
}

// Disabling is a state, not a deletion: the row stays, the products keep pointing at it, and it
// stops being offered for a new assignment. That is the choice the delete refusal points to.
func TestServerCanBeDisabledInsteadOfDeleted(t *testing.T) {
	env := newStaffEnv(t)
	id := createServer(t, env, map[string]any{"slug": "off", "name": "Off", "enabled": true})

	rec := env.putJSON(t, "/api/v1/staff/catalog/servers/"+id, env.admin, env.adminCSRF,
		map[string]any{"slug": "off", "name": "Off", "enabled": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("disable server: %d %s", rec.Code, rec.Body)
	}
	var disabled struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &disabled); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if disabled.Enabled {
		t.Fatal("the server is still enabled")
	}

	// It is still deletable afterwards, which is what makes disabling reversible rather than a
	// one-way door.
	rec = env.deleteServer(t, id)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("deleting a disabled server: %d %s", rec.Code, rec.Body)
	}
}

// deleteServer performs the DELETE the panel's own client performs.
func (e *staffEnv) deleteServer(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()
	rec := e.deleteJSON(t, "/api/v1/staff/catalog/servers/"+id, e.admin, e.adminCSRF)
	return rec
}

// The removal is real: the row is gone and the list no longer offers it.
func TestServerDeleteRemovesTheRow(t *testing.T) {
	env := newStaffEnv(t)
	id := createServer(t, env, map[string]any{"slug": "gone", "name": "Gone"})

	if rec := env.deleteServer(t, id); rec.Code != http.StatusNoContent {
		t.Fatalf("delete server: %d %s", rec.Code, rec.Body)
	}

	var remaining int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM servers WHERE id = $1`, id).Scan(&remaining); err != nil {
		t.Fatalf("count server: %v", err)
	}
	if remaining != 0 {
		t.Fatal("the server row survived its delete")
	}

	// Deleting it again is a 404: the target is gone, and the second request is not a second
	// delete of something that still exists.
	if rec := env.deleteServer(t, id); rec.Code != http.StatusNotFound {
		t.Fatalf("deleting twice: %d %s", rec.Code, rec.Body)
	}
}
