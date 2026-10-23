package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePtero is a stand-in for Pterodactyl's application API, strict enough to
// catch a wrong request.
//
// It enforces the things the real panel enforces and that this adapter has to get
// right: the application key prefix, the `external_id` filter used for
// reconciliation, a default allocation being required at create, and the fact that
// a create without a user is a 422.
type fakePtero struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	seq      int
	servers  map[int64]*fakeServer
	allocs   map[string][]fakeAlloc
	requests []pteroRequest
	failNext *failMode
	// nextAllocation decides what the node reports; empty means no free ports.
	freeAllocations []int64
}

type pteroRequest struct {
	Method string
	Path   string
	Query  string
	Body   map[string]any
	Auth   string
}

type failMode struct {
	status int
	body   string
}

type fakeServer struct {
	ID         int64
	ExternalID string
	UUID       string
	Identifier string
	Name       string
	Suspended  bool
	Node       int64
	CreatedAt  string
	Env        map[string]string
	Limits     map[string]any
}

type fakeAlloc struct {
	ID       int64
	Assigned bool
}

func newFakePtero(t *testing.T) *fakePtero {
	t.Helper()
	f := &fakePtero{
		t:               t,
		servers:         map[int64]*fakeServer{},
		allocs:          map[string][]fakeAlloc{},
		freeAllocations: []int64{101, 102, 103},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakePtero) baseURL() string { return f.server.URL }

func (f *fakePtero) config() config {
	plans := []plan{
		{ID: "small", MemoryMB: 2048, DiskMB: 10240, CPUPercent: 100},
		{ID: "medium", MemoryMB: 4096, DiskMB: 20480, CPUPercent: 200},
	}
	return config{
		panelURL:  f.server.URL,
		apiKey:    "ptla_test_key",
		nestID:    "1",
		eggID:     "15",
		nodeID:    "3",
		timeout:   5 * time.Second,
		plans:     plans,
		plansByID: map[string]plan{"small": plans[0], "medium": plans[1]},
	}
}

func (f *fakePtero) lastRequest() pteroRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		f.t.Fatal("no request recorded")
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakePtero) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakePtero) failWith(status int, body string) {
	f.mu.Lock()
	f.failNext = &failMode{status: status, body: body}
	f.mu.Unlock()
}

// suspend marks a server suspended, as staff doing it in the panel would.
func (f *fakePtero) suspend(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if srv, ok := f.servers[id]; ok {
		srv.Suspended = true
	}
}

func (f *fakePtero) handle(w http.ResponseWriter, r *http.Request) {
	rec := pteroRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization")}
	if r.Body != nil {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			rec.Body = body
		}
	}

	f.mu.Lock()
	f.requests = append(f.requests, rec)
	fail := f.failNext
	f.failNext = nil
	f.mu.Unlock()

	if fail != nil {
		w.WriteHeader(fail.status)
		fmt.Fprint(w, fail.body)
		return
	}
	if !strings.HasPrefix(rec.Auth, "Bearer ptla_") {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"errors":{"auth":["this key does not have permission"]}}`)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/application")
	switch {
	case r.Method == http.MethodGet && path == "/servers":
		f.listServers(w, r)
	case r.Method == http.MethodPost && path == "/servers":
		f.createServer(w, rec)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/servers/"):
		f.getServer(w, strings.TrimPrefix(path, "/servers/"))
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/suspend"):
		f.setSuspended(w, strings.TrimSuffix(strings.TrimPrefix(path, "/servers/"), "/suspend"), true)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/unsuspend"):
		f.setSuspended(w, strings.TrimSuffix(strings.TrimPrefix(path, "/servers/"), "/unsuspend"), false)
	case r.Method == http.MethodPatch && strings.HasSuffix(path, "/build"):
		f.updateBuild(w, rec, strings.TrimSuffix(strings.TrimPrefix(path, "/servers/"), "/build"))
	case r.Method == http.MethodPatch && strings.HasSuffix(path, "/startup"):
		f.updateStartup(w, rec, strings.TrimSuffix(strings.TrimPrefix(path, "/servers/"), "/startup"))
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/servers/"):
		f.deleteServer(w, strings.TrimPrefix(path, "/servers/"))
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/allocations"):
		f.listAllocations(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/nodes/"), "/allocations"))
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"errors":{"route":["not found"]}}`)
	}
}

func (f *fakePtero) listServers(w http.ResponseWriter, r *http.Request) {
	// Pterodactyl's filter syntax arrives as filter[external_id].
	filter := ""
	for key, values := range r.URL.Query() {
		if key == "filter[external_id]" && len(values) > 0 {
			filter = values[0]
		}
	}
	f.mu.Lock()
	var out []map[string]any
	for _, srv := range f.servers {
		if filter != "" && srv.ExternalID != filter {
			continue
		}
		out = append(out, serverAttrs(srv))
	}
	f.mu.Unlock()
	writeList(w, out)
}

func (f *fakePtero) createServer(w http.ResponseWriter, rec pteroRequest) {
	body := rec.Body
	if body == nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprint(w, `{"errors":{"body":["a body is required"]}}`)
		return
	}
	user, _ := body["user"].(string)
	if user == "" {
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprint(w, `{"errors":{"user":["The user field is required."]}}`)
		return
	}
	egg, _ := body["egg"].(string)
	if egg == "" {
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprint(w, `{"errors":{"egg":["The egg field is required."]}}`)
		return
	}
	allocation, _ := body["allocation"].(map[string]any)
	if allocation == nil || allocation["default"] == nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprint(w, `{"errors":{"allocation.default":["The allocation.default field is required."]}}`)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	srv := &fakeServer{
		ID:         int64(f.seq),
		ExternalID: stringField(body, "external_id"),
		UUID:       fmt.Sprintf("uuid-%d", f.seq),
		Identifier: fmt.Sprintf("id%d", f.seq),
		Name:       stringField(body, "name"),
		Node:       3,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		Env:        map[string]string{},
		Limits:     map[string]any{},
	}
	if env, ok := body["environment"].(map[string]any); ok {
		for k, v := range env {
			srv.Env[k] = fmt.Sprint(v)
		}
	}
	if limits, ok := body["limits"].(map[string]any); ok {
		srv.Limits = limits
	}
	f.servers[srv.ID] = srv
	w.WriteHeader(http.StatusCreated)
	writeObject(w, serverAttrs(srv))
}

func stringField(body map[string]any, key string) string {
	if v, ok := body[key].(string); ok {
		return v
	}
	return ""
}

func (f *fakePtero) getServer(w http.ResponseWriter, id string) {
	f.mu.Lock()
	var found *fakeServer
	for _, srv := range f.servers {
		if fmt.Sprint(srv.ID) == id || srv.Identifier == id || srv.UUID == id {
			found = srv
			break
		}
	}
	f.mu.Unlock()
	if found == nil {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"errors":{"server":["The requested server was not found."]}}`)
		return
	}
	writeObject(w, serverAttrs(found))
}

func (f *fakePtero) setSuspended(w http.ResponseWriter, id string, suspended bool) {
	f.mu.Lock()
	var found *fakeServer
	for _, srv := range f.servers {
		if fmt.Sprint(srv.ID) == id {
			found = srv
			break
		}
	}
	if found != nil {
		found.Suspended = suspended
	}
	f.mu.Unlock()
	if found == nil {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"errors":{"server":["The requested server was not found."]}}`)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// updateBuild applies a PATCH /servers/{id}/build the way the real panel does: the body
// replaces the limits it names.
func (f *fakePtero) updateBuild(w http.ResponseWriter, rec pteroRequest, id string) {
	f.mu.Lock()
	found := f.serverByID(id)
	if found != nil && rec.Body != nil {
		limits, _ := found.Limits, rec.Body
		_ = limits
		memory, _ := rec.Body["memory"].(float64)
		disk, _ := rec.Body["disk"].(float64)
		cpu, _ := rec.Body["cpu"].(float64)
		found.Limits = map[string]any{"memory": memory, "disk": disk, "cpu": cpu}
	}
	f.mu.Unlock()
	if found == nil {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"errors":{"server":["The requested server was not found."]}}`)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// updateStartup replaces the environment, matching the panel's own semantics.
func (f *fakePtero) updateStartup(w http.ResponseWriter, rec pteroRequest, id string) {
	f.mu.Lock()
	found := f.serverByID(id)
	if found != nil && rec.Body != nil {
		if env, ok := rec.Body["environment"].(map[string]any); ok {
			replaced := map[string]string{}
			for k, v := range env {
				replaced[k] = fmt.Sprint(v)
			}
			found.Env = replaced
		}
	}
	f.mu.Unlock()
	if found == nil {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"errors":{"server":["The requested server was not found."]}}`)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// serverByID finds a server without taking the lock (callers hold it).
func (f *fakePtero) serverByID(id string) *fakeServer {
	for _, srv := range f.servers {
		if fmt.Sprint(srv.ID) == id {
			return srv
		}
	}
	return nil
}

func (f *fakePtero) deleteServer(w http.ResponseWriter, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key, srv := range f.servers {
		if fmt.Sprint(srv.ID) == id {
			delete(f.servers, key)
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprint(w, `{"errors":{"server":["The requested server was not found."]}}`)
}

func (f *fakePtero) listAllocations(w http.ResponseWriter, r *http.Request, nodeID string) {
	f.mu.Lock()
	var out []map[string]any
	for _, id := range f.freeAllocations {
		out = append(out, map[string]any{"id": id, "assigned": false})
	}
	f.mu.Unlock()
	_ = nodeID
	writeList(w, out)
}

func serverAttrs(srv *fakeServer) map[string]any {
	return map[string]any{
		"id":          srv.ID,
		"external_id": srv.ExternalID,
		"uuid":        srv.UUID,
		"identifier":  srv.Identifier,
		"name":        srv.Name,
		"suspended":   srv.Suspended,
		"node":        srv.Node,
		"created_at":  srv.CreatedAt,
		"limits":      srv.Limits,
		// The application API reports the egg's environment under container; the adapter reads
		// it so a replayed resize can answer "already like this".
		"container": map[string]any{"environment": srv.Env},
	}
}

func writeObject(w http.ResponseWriter, attrs map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "server", "attributes": attrs,
	})
}

func writeList(w http.ResponseWriter, items []map[string]any) {
	data := make([]map[string]any, 0, len(items))
	for _, attrs := range items {
		data = append(data, map[string]any{"object": "server", "attributes": attrs})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   data,
		"meta":   map[string]any{"pagination": map[string]any{"total_pages": 1, "current_page": 1}},
	})
}

// signNothing is a placeholder so an unused-import check does not fire when a
// test is temporarily disabled; it has no behaviour.
func signNothing() {}

var _ = signNothing
