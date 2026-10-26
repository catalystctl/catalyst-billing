// Command example-hooks is a conformance fixture for the two halves of the extension
// surface the panel adds beyond the gateway and provisioner: extension-contributed UI
// (protocol v2 `ui.describe`) and lifecycle event delivery (protocol v2
// `events.subscribe` / `events.deliver`).
//
// It is a real extension binary, not a mock: it speaks newline-delimited JSON-RPC 2.0 on
// stdin/stdout, negotiates a protocol version, advertises capabilities and answers
// exactly the methods it advertised. The core's own client drives it over real pipes in
// extensions/example-hooks/conformance_test.go and in internal/httpapi's hook tests, so a
// change to the envelope, the handshake or the notification path fails there rather than
// in production.
//
// # What it declares
//
//	nav item      "Example hooks", path /staff/extensions/example-hooks, icon "plug",
//	              permission extension:manage
//	service action "Reboot service", method service.reboot, confirmation prompt,
//	              permission service:write:self
//
// Both are data. The extension ships no code to the browser: the panel renders a label,
// an icon name it already has and a link to a path it already routes, and the only thing
// this binary is ever asked to do is run `service.reboot`, which it advertised.
//
// # Programming the fixture
//
//	EXAMPLE_HOOKS_MODE=ok|reject|stall|exit   (default ok)
//	EXAMPLE_HOOKS_VERSIONS=2,1                (default "2,1"; set "1" for a v1-only extension)
//	EXAMPLE_HOOKS_ACTION_PERMISSION=...        (default service:write:self)
//	EXAMPLE_HOOKS_NAV_PERMISSION=...           (default extension:manage)
//	EXAMPLE_HOOKS_ACTION_METHOD=...            (default service.reboot)
//	EXAMPLE_HOOKS_EVENT_KINDS=a,b              (default: every kind the core offers)
//	EXAMPLE_HOOKS_PIDFILE=<path>               write the process id at startup
//
// The modes exist because the properties under test are failures:
//
//	stall  after registration, stop reading stdin. The core's writes fill the pipe and
//	       then its write queue, which is how "a dead extension must not delay a
//	       checkout" is tested: the checkout must complete anyway, and the events that
//	       could not be delivered must be counted as dropped.
//	exit   after registration, exit. The core's next notification fails immediately.
//	reject the service action answers with a business rejection (-32005), which the panel
//	       must surface as "the extension refused" rather than "the extension is broken".
//
// # Fixture-only methods
//
//	fixture.received   what this process has received: the delivered events and the
//	                   service-action calls, in order. It is how a test asserts that an
//	                   order event actually arrived at the extension rather than at a
//	                   counter inside the core.
//
// fixture.received is not part of the protocol. It exists so the notification path can be
// observed end to end without a real extension having to expose an API for it.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// maxInput bounds a request frame. The core never sends anything close to this.
const maxInput = 4 << 20

// JSON-RPC codes used by the fixture.
const (
	codeParseError      = -32700
	codeInvalidRequest  = -32600
	codeMethodNotFound  = -32601
	codeInvalidParams   = -32602
	codeVersionMismatch = -32001
	codeRejected        = -32005
)

func main() {
	writePIDFile(os.Getenv("EXAMPLE_HOOKS_PIDFILE"))
	s := &server{
		mode:             envOr("EXAMPLE_HOOKS_MODE", "ok"),
		versions:         envVersions("EXAMPLE_HOOKS_VERSIONS", []int{2, 1}),
		navPermission:    envOr("EXAMPLE_HOOKS_NAV_PERMISSION", "extension:manage"),
		actionPermission: envOr("EXAMPLE_HOOKS_ACTION_PERMISSION", "service:write:self"),
		actionMethod:     envOr("EXAMPLE_HOOKS_ACTION_METHOD", "service.reboot"),
		wantKinds:        envOr("EXAMPLE_HOOKS_EVENT_KINDS", "*"),
		events:           nil,
		calls:            nil,
		byKey:            make(map[string]actionResult),
	}
	if err := s.serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "example-hooks: %v\n", err)
		os.Exit(1)
	}
}

// request is the incoming JSON-RPC envelope.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *uint64         `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// response is the outgoing JSON-RPC envelope.
type response struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      uint64    `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

// rpcError is the outgoing JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// reply is what one handled request produces.
type reply struct {
	result any
	rpcErr *rpcError
}

// navItem mirrors extproto.NavItem. The fixture declares its own copies so that a change
// to the core's struct is caught as a wire mismatch here instead of being compiled away.
type navItem struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Path       string `json:"path"`
	Icon       string `json:"icon"`
	Permission string `json:"permission"`
}

// serviceAction mirrors extproto.ServiceAction.
type serviceAction struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Method     string `json:"method"`
	Confirm    string `json:"confirm,omitempty"`
	Permission string `json:"permission"`
}

// recordsMethod is the method the declared admin resource and page read from. It is a fixture
// answer: what matters is that a declared surface names a method the extension advertised.
const recordsMethod = "example.records"

// actionResult is the answer to one service-action call, kept so a replay of the same
// operation key returns the first result instead of acting twice.
type actionResult struct {
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

// receivedEvent is one lifecycle event the core delivered.
type receivedEvent struct {
	Kind       string         `json:"kind"`
	ID         int64          `json:"id"`
	CustomerID string         `json:"customer_id"`
	Data       map[string]any `json:"data"`
	At         time.Time      `json:"at"`
}

// server is the fixture's whole state. It is driven by a single goroutine, so it needs
// no locking.
type server struct {
	mode             string
	versions         []int
	navPermission    string
	actionPermission string
	actionMethod     string
	wantKinds        string

	// version is the version negotiated by initialize; 0 before the handshake.
	version int
	// registered is set once the core has finished describing and subscribing, which is
	// the point where "stall" and "exit" start misbehaving.
	registered bool

	events []receivedEvent
	calls  []actionResult
	byKey  map[string]actionResult
	seq    int
}

func (s *server) serve(in io.Reader, out io.Writer) error {
	r := bufio.NewReaderSize(in, 64<<10)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)

	for {
		if s.registered && s.mode == "stall" {
			// Stop reading stdin. The core keeps writing into the pipe until it fills,
			// which is exactly the dead-extension case the drop counter exists for.
			select {}
		}
		line, err := readLine(r, maxInput)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			// A parse failure is fatal for a JSON-RPC stream: we cannot know which id it
			// belonged to.
			return fmt.Errorf("parse error: %w", err)
		}
		if req.ID == nil {
			// A notification from the core. In protocol version 2 that is a delivered
			// lifecycle event; nothing is answered.
			s.notify(req)
			continue
		}
		if req.JSONRPC != "2.0" {
			if err := enc.Encode(response{JSONRPC: "2.0", ID: *req.ID, Error: &rpcError{
				Code: codeInvalidRequest, Message: "jsonrpc must be 2.0"}}); err != nil {
				return err
			}
			continue
		}
		rep := s.handle(&req)
		if err := enc.Encode(response{JSONRPC: "2.0", ID: *req.ID, Result: rep.result, Error: rep.rpcErr}); err != nil {
			return err
		}
		if s.registered && s.mode == "exit" {
			// Registration is complete and the mode says the extension is gone. Exiting
			// here models a crash after a healthy start.
			return nil
		}
	}
}

// handle dispatches one request.
func (s *server) handle(req *request) reply {
	switch req.Method {
	case "initialize":
		return s.initialize(req)
	case "health":
		return reply{result: map[string]string{"status": "ok"}}
	case "ui.describe":
		return s.describe()
	case "events.subscribe":
		return s.subscribe(req)
	case "fixture.received":
		return reply{result: map[string]any{
			"version": s.version,
			"events":  s.events,
			"calls":   s.calls,
		}}
	}
	if req.Method == s.actionMethod {
		return s.action(req)
	}
	if req.Method == recordsMethod {
		// A fixture answer: the point of the conformance fixture is the *declaration* the panel
		// accepts and the shape it can render, not a database round trip.
		return reply{result: map[string]any{"records": []map[string]any{
			{"name": "web-1", "status": "active"},
			{"name": "web-2", "status": "suspended"},
		}}}
	}
	return reply{rpcErr: &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}}
}

// initialize negotiates the protocol version.
//
// The extension picks the newest version it knows that the core offered. A v1-only
// fixture picks 1 and is accepted: that is the compatibility promise, and it is why the
// capability list is computed after the version is known rather than being a constant.
func (s *server) initialize(req *request) reply {
	var p struct {
		Protocol    string `json:"protocol"`
		Versions    []int  `json:"versions"`
		CoreVersion string `json:"core_version"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "initialize params: " + err.Error()}}
	}
	if p.Protocol != "billing.ext/1" {
		return reply{rpcErr: &rpcError{Code: codeVersionMismatch, Message: "unknown protocol " + p.Protocol}}
	}
	chosen := 0
	for _, mine := range s.versions {
		for _, theirs := range p.Versions {
			if mine == theirs && mine > chosen {
				chosen = mine
			}
		}
	}
	if chosen == 0 {
		return reply{rpcErr: &rpcError{
			Code:    codeVersionMismatch,
			Message: "no common protocol version",
			Data:    map[string]any{"supported": s.versions, "offered": p.Versions},
		}}
	}
	s.version = chosen
	return reply{result: map[string]any{
		"version":      chosen,
		"name":         "example-hooks",
		"publisher":    "catalyst",
		"capabilities": s.capabilities(),
	}}
}

// capabilities is what the extension promises to implement at the negotiated version.
//
// The UI and event methods are only advertised at version 2, because at version 1 the
// core will not call them and advertising a method it will not call would be a lie about
// the extension's own surface.
func (s *server) capabilities() []string {
	caps := []string{"health", "fixture.received", s.actionMethod}
	if s.version >= 2 {
		caps = append(caps, "ui.describe", "events.subscribe", recordsMethod)
	}
	return caps
}

// describe answers ui.describe: one navigation item and one service action.
func (s *server) describe() reply {
	if s.version < 2 {
		return reply{rpcErr: &rpcError{Code: codeMethodNotFound, Message: "ui.describe needs protocol version 2"}}
	}
	return reply{result: map[string]any{
		"nav_items": []navItem{{
			ID:         "example-hooks",
			Label:      "Example hooks",
			Path:       "/staff/extensions/example-hooks",
			Icon:       "plug",
			Permission: s.navPermission,
		}},
		"service_actions": []serviceAction{{
			ID:         "reboot",
			Label:      "Reboot service",
			Method:     s.actionMethod,
			Confirm:    "Reboot this service? It will be unavailable for about a minute.",
			Permission: s.actionPermission,
		}},
		// The operator surface (GRW-05). A resource is a record type the panel draws a table
		// for, a page is a place in the menu, and a checkout field is an input the customer is
		// asked for. None of them carries markup or SQL: they are declarations the panel renders
		// with its own components.
		"admin_resources": []map[string]any{{
			"id":         "records",
			"label":      "Example record",
			"plural":     "Example records",
			"path":       "/staff/extensions/example-hooks/records",
			"icon":       "plug",
			"permission": s.navPermission,
			"method":     recordsMethod,
			"columns": []map[string]any{
				{"key": "name", "label": "Name", "type": "text", "primary": true},
				{"key": "status", "label": "Status", "type": "status"},
			},
		}},
		"pages": []map[string]any{{
			"id":         "overview",
			"label":      "Example overview",
			"path":       "/staff/extensions/example-hooks/overview",
			"icon":       "plug",
			"permission": s.navPermission,
			"method":     recordsMethod,
		}},
		"checkout_fields": []map[string]any{{
			"key":      "example.hostname",
			"label":    "Hostname",
			"type":     "text",
			"required": true,
			"help":     "The name the server should answer to.",
		}},
	}}
}

// subscribe answers events.subscribe with the kinds it wants.
func (s *server) subscribe(req *request) reply {
	if s.version < 2 {
		return reply{rpcErr: &rpcError{Code: codeMethodNotFound, Message: "events.subscribe needs protocol version 2"}}
	}
	var p struct {
		Kinds []string `json:"kinds"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "subscribe params: " + err.Error()}}
	}
	accepted := make([]string, 0, len(p.Kinds))
	for _, kind := range p.Kinds {
		if s.wants(kind) {
			accepted = append(accepted, kind)
		}
	}
	// Registration is over once the subscription is answered: from here on the only
	// frames the core sends are event notifications, which is the point where the
	// "stall" and "exit" modes start misbehaving.
	s.registered = s.mode == "stall" || s.mode == "exit"
	return reply{result: map[string]any{"accepted": accepted}}
}

// wants reports whether the fixture subscribed to a kind.
func (s *server) wants(kind string) bool {
	if s.wantKinds == "*" || s.wantKinds == "" {
		return true
	}
	for _, want := range strings.Split(s.wantKinds, ",") {
		if strings.TrimSpace(want) == kind {
			return true
		}
	}
	return false
}

// notify records a delivered lifecycle event.
func (s *server) notify(req request) {
	if req.Method != "events.deliver" {
		return
	}
	var ev receivedEvent
	if err := json.Unmarshal(req.Params, &ev); err != nil {
		return
	}
	s.events = append(s.events, ev)
}

// action runs the declared service action. It is idempotent by operation key: a replayed
// click returns the first result, which is what makes a retried request safe.
func (s *server) action(req *request) reply {
	if s.mode == "reject" {
		return reply{rpcErr: &rpcError{
			Code:    codeRejected,
			Message: "this service cannot be rebooted right now",
			Data:    map[string]any{"reason": "maintenance_window"},
		}}
	}
	var p struct {
		OperationKey string `json:"operation_key"`
		Action       string `json:"action"`
		ServiceID    string `json:"service_id"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "action params: " + err.Error()}}
	}
	if p.OperationKey == "" || p.ServiceID == "" {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "operation_key and service_id are required"}}
	}
	if prior, ok := s.byKey[p.OperationKey]; ok {
		// Replay: the same answer, no second reboot.
		return reply{result: prior}
	}
	s.seq++
	out := actionResult{
		Message: "Reboot requested",
		Data: map[string]any{
			"call_id":    strconv.Itoa(s.seq),
			"service_id": p.ServiceID,
			"action":     p.Action,
		},
	}
	s.byKey[p.OperationKey] = out
	s.calls = append(s.calls, out)
	return reply{result: out}
}

// writePIDFile records the process id, so a test can prove the core killed it.
func writePIDFile(path string) {
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "example-hooks: write pid file: %v\n", err)
	}
}

// readLine reads one newline-terminated frame, bounded by max.
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > max {
			return nil, fmt.Errorf("frame exceeds %d bytes", max)
		}
		buf = append(buf, chunk...)
		if err == nil {
			return trimNewline(buf), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return nil, err
	}
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// envVersions parses a comma-separated version list, falling back to the default when
// the value is absent or unparseable.
func envVersions(key string, fallback []int) []int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	out := make([]int, 0, len(fallback))
	for _, part := range strings.Split(raw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n <= 0 {
			return fallback
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}
