package main

// A strict fake of the Catalyst panel API.
//
// It is strict in the way the other fixtures in this repository are strict: an unmodelled path is a
// 404 in the panel's own error shape, a missing bearer token is a 401, and every mutating call is
// recorded so a test can assert what the adapter *sent* rather than only what it returned. The point
// is that a test which passes here proves something about the adapter's HTTP behaviour, not just
// about its own branch logic.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakePanel is one panel instance for the duration of a test.
type fakePanel struct {
	t *testing.T

	mu sync.Mutex
	// servers is keyed by id.
	servers map[string]panelServer
	// order preserves creation order so a list scan is deterministic.
	order []string

	locations []panelLocation
	nodes     []panelNode
	// nodeStats is keyed by node id.
	nodeStats map[string]panelNodeStats
	// allocations is keyed by node id.
	allocations map[string][]panelAllocation
	templates   []panelTemplate

	// calls records every mutating request: "POST /servers", "POST /servers/x/start", …
	calls []recordedCall
	// created holds the decoded bodies of every POST /servers.
	created []createRequest
	// nextID is the id the next created server gets.
	nextID int
	// failCreateStatus, when non-zero, makes POST /servers fail with that status.
	failCreateStatus int
	// failCreateCode is the panel error code for that failure.
	failCreateCode string
	// noMetrics makes the stats endpoint answer the panel's "no sample yet" shape.
	noMetrics bool
	// listPages, when non-zero, caps every list page to that many servers, which is how a panel
	// that enforces its own page size is simulated.
	listPages int
	// repeatsPage makes every list request return the first page again, ignoring the offset. It
	// simulates a panel whose pagination the adapter cannot walk, which is the case a scan must
	// refuse to answer rather than mistake for the end of the list.
	repeatsPage bool
	// unauthorized makes every request answer 401, for the credential-failure path.
	unauthorized bool
}

// recordedCall is one mutating request the adapter made.
type recordedCall struct {
	Method string
	Path   string
	Body   map[string]any
}

// newFakePanel builds a panel with one location, one online node and one template.
func newFakePanel(t *testing.T) *fakePanel {
	t.Helper()
	return &fakePanel{
		t:         t,
		servers:   map[string]panelServer{},
		locations: []panelLocation{{ID: "loc-1", Name: "US East 1"}},
		nodes: []panelNode{{
			ID: "node-1", Name: "node one", LocationID: "loc-1",
			PublicAddress: "10.0.0.1", IsOnline: true, MaxMemoryMb: 16384, MaxCpuCores: 8,
		}},
		nodeStats: map[string]panelNodeStats{
			"node-1": {NodeID: "node-1", IsOnline: true, Resources: struct {
				MaxMemoryMb       int64 `json:"maxMemoryMb"`
				MaxCpuCores       int64 `json:"maxCpuCores"`
				AllocatedMemoryMb int64 `json:"allocatedMemoryMb"`
				AllocatedCpuCores int64 `json:"allocatedCpuCores"`
			}{MaxMemoryMb: 16384, MaxCpuCores: 8, AllocatedMemoryMb: 1024, AllocatedCpuCores: 1}},
		},
		allocations: map[string][]panelAllocation{
			"node-1": {
				{ID: "alloc-1", NodeID: "node-1", IP: "10.0.0.1", Port: 25565},
				{ID: "alloc-2", NodeID: "node-1", IP: "10.0.0.1", Port: 25566},
			},
		},
		templates: []panelTemplate{{ID: "tpl-paper", Name: "Paper"}},
		nextID:    1,
	}
}

// start runs the fake and returns its URL.
func (f *fakePanel) start() string {
	f.t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	f.t.Cleanup(srv.Close)
	return srv.URL
}

// addServer registers an existing server.
func (f *fakePanel) addServer(srv panelServer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.servers[srv.ID] = srv
	f.order = append(f.order, srv.ID)
}

// lastCall returns the most recent mutating call, failing when there is none.
func (f *fakePanel) lastCall() recordedCall {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		f.t.Fatal("the adapter made no mutating call")
	}
	return f.calls[len(f.calls)-1]
}

// callCount counts the mutating calls matching method and path.
func (f *fakePanel) callCount(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, call := range f.calls {
		if call.Method == method && call.Path == path {
			n++
		}
	}
	return n
}

func (f *fakePanel) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.unauthorized {
		writePanelError(w, http.StatusUnauthorized, "", "Invalid API key")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		writePanelError(w, http.StatusUnauthorized, "", "Invalid API key")
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api")
	switch {
	case r.Method == http.MethodGet && path == "/locations":
		writePanelOK(w, f.locations)
	case r.Method == http.MethodGet && path == "/templates":
		writePanelOK(w, f.templates)
	case r.Method == http.MethodGet && path == "/nodes":
		writePanelOK(w, f.nodes)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/nodes/") && strings.HasSuffix(path, "/stats"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/nodes/"), "/stats")
		stats, ok := f.nodeStats[id]
		if !ok {
			writePanelError(w, http.StatusNotFound, "NODE_NOT_FOUND", "Node not found")
			return
		}
		writePanelOK(w, stats)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/nodes/") && strings.HasSuffix(path, "/allocations"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/nodes/"), "/allocations")
		writePanelOK(w, f.allocations[id])
	case r.Method == http.MethodPost && path == "/servers":
		f.handleCreate(w, r)
	case r.Method == http.MethodGet && path == "/servers":
		f.handleList(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/servers/"):
		f.handleGet(w, path)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/servers/"):
		f.handleAction(w, r, path)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/servers/"):
		f.handleDelete(w, path)
	default:
		writePanelError(w, http.StatusNotFound, "NOT_FOUND", "no such endpoint "+path)
	}
}

func (f *fakePanel) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writePanelError(w, http.StatusBadRequest, "VALIDATION_ERROR", "bad body")
		return
	}
	f.created = append(f.created, req)
	f.calls = append(f.calls, recordedCall{Method: http.MethodPost, Path: "/servers"})

	if f.failCreateStatus != 0 {
		writePanelError(w, f.failCreateStatus, f.failCreateCode, "refused")
		return
	}
	id := "srv-" + itoa(f.nextID)
	f.nextID++
	description := req.Description
	networkMode := ""
	if req.NetworkMode != nil {
		networkMode = *req.NetworkMode
	}
	primaryIP := req.PrimaryIP
	srv := panelServer{
		ID: id, Name: req.Name, Description: &description, Status: "installing",
		NodeID: req.NodeID, LocationID: req.LocationID, TemplateID: req.TemplateID,
		NetworkMode: networkMode, AllocatedMemoryMb: req.AllocatedMemoryMb,
		AllocatedCpuCores: req.AllocatedCpuCores, AllocatedDiskMb: req.AllocatedDiskMb,
		PrimaryPort: req.PrimaryPort, PrimaryIP: &primaryIP,
	}
	srv.Node = &panelNode{ID: req.NodeID, Name: "node one", PublicAddress: "10.0.0.1", IsOnline: true}
	srv.Connection = &panelConnection{Host: "10.0.0.1", Port: req.PrimaryPort}
	f.servers[id] = srv
	f.order = append(f.order, id)
	writePanelOK(w, srv)
}

func (f *fakePanel) handleList(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		if n, err := atoi(raw); err == nil && n > 0 {
			offset = n
		}
	}
	if f.listPages > 0 {
		limit = f.listPages
	}
	if f.repeatsPage {
		offset = 0
	}
	out := []panelServer{}
	for i := offset; i < len(f.order) && len(out) < limit; i++ {
		out = append(out, f.servers[f.order[i]])
	}
	writePanelOK(w, out)
}

func (f *fakePanel) handleGet(w http.ResponseWriter, path string) {
	id := strings.TrimPrefix(path, "/servers/")
	if strings.HasSuffix(id, "/stats") {
		id = strings.TrimSuffix(id, "/stats")
		srv, ok := f.servers[id]
		if !ok {
			writePanelError(w, http.StatusNotFound, "SERVER_NOT_FOUND", "Server not found")
			return
		}
		if f.noMetrics || srv.Status == "stopped" {
			writePanelOK(w, map[string]any{
				"message": "No metrics available yet",
				"server":  map[string]any{"id": srv.ID, "name": srv.Name, "status": srv.Status},
			})
			return
		}
		writePanelOK(w, map[string]any{
			"cpuPercent": 12.5, "memoryUsageMb": 512, "memoryAllocatedMb": 1024,
			"diskUsageMb": 200, "networkRxBytes": "1234", "networkTxBytes": "5678",
			"timestamp": "2026-10-16T02:15:59.851Z",
			"server":    map[string]any{"id": srv.ID, "name": srv.Name, "status": srv.Status},
		})
		return
	}
	srv, ok := f.servers[id]
	if !ok {
		writePanelError(w, http.StatusNotFound, "SERVER_NOT_FOUND", "Server not found")
		return
	}
	writePanelOK(w, srv)
}

func (f *fakePanel) handleAction(w http.ResponseWriter, r *http.Request, path string) {
	rest := strings.TrimPrefix(path, "/servers/")
	slash := strings.LastIndex(rest, "/")
	if slash < 0 {
		writePanelError(w, http.StatusNotFound, "NOT_FOUND", "no such endpoint")
		return
	}
	id, action := rest[:slash], rest[slash+1:]
	srv, ok := f.servers[id]
	if !ok {
		writePanelError(w, http.StatusNotFound, "SERVER_NOT_FOUND", "Server not found")
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.calls = append(f.calls, recordedCall{Method: http.MethodPost, Path: "/servers/" + id + "/" + action, Body: body})

	switch action {
	case "start":
		if srv.SuspendedAt != nil {
			writePanelError(w, http.StatusLocked, "SERVER_SUSPENDED", "Server is suspended")
			return
		}
		srv.Status = "starting"
	case "stop":
		srv.Status = "stopping"
	case "restart":
		srv.Status = "restarting"
	case "kill":
		srv.Status = "stopped"
	case "suspend":
		stamp := "2026-10-16T00:00:00Z"
		srv.SuspendedAt = &stamp
		srv.Status = "stopped"
	case "unsuspend":
		srv.SuspendedAt = nil
		srv.Status = "stopped"
	default:
		writePanelError(w, http.StatusNotFound, "NOT_FOUND", "no such action "+action)
		return
	}
	f.servers[id] = srv
	writePanelOK(w, srv)
}

func (f *fakePanel) handleDelete(w http.ResponseWriter, path string) {
	id := strings.TrimPrefix(path, "/servers/")
	f.calls = append(f.calls, recordedCall{Method: http.MethodDelete, Path: "/servers/" + id})
	if _, ok := f.servers[id]; !ok {
		writePanelError(w, http.StatusNotFound, "SERVER_NOT_FOUND", "Server not found")
		return
	}
	delete(f.servers, id)
	w.WriteHeader(http.StatusNoContent)
}

// writePanelOK writes the panel's success envelope.
func writePanelOK(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
}

// writePanelError writes the panel's flat error shape, which is *not* the success envelope.
func writePanelError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	out := map[string]any{"error": message}
	if code != "" {
		out["code"] = code
	}
	_ = json.NewEncoder(w).Encode(out)
}

// drain closes a request body, ignoring the error: tests do not care.
func drain(body io.ReadCloser) { _ = body.Close() }

// itoa and atoi keep the fixture free of strconv noise in the middle of HTTP handling.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func atoi(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errNotANumber
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

// errNotANumber is returned by atoi.
var errNotANumber = &panelError{status: http.StatusBadRequest, message: "not a number"}
