// Command pterodactyl implements the Pterodactyl game-server panel as a billing
// provisioner extension.
//
// It speaks the same newline-delimited JSON-RPC 2.0 protocol as
// extensions/fakeprov, but the calls go to Pterodactyl's *application* API over
// HTTPS. The core's queue, leases, fencing and reconciliation are unchanged: what
// this file adds is an adapter that can answer the two questions the core depends
// on, honestly.
//
// # Why the external_id is the operation key
//
// The protocol says a lookup must be authoritative "because a create replay only
// works while the adapter still remembers the key, and after a restart it may
// not". An in-memory map cannot satisfy that. Pterodactyl stores an `external_id`
// on every server and can filter by it, so the operation key is written there:
//
//	provisioner.lookup(key)  ->  GET /api/application/servers?filter[external_id]=key
//
// That makes reconciliation survive this process restarting, which is exactly the
// case that used to provision a second server.
//
// # Configuration
//
//	PTERODACTYL_PANEL_URL    required; e.g. https://panel.example.com
//	PTERODACTYL_API_KEY      required; an application API key (ptla_…), not a client key
//	PTERODACTYL_NEST_ID      required; the nest the egg lives in
//	PTERODACTYL_EGG_ID       required; the egg new servers are created from
//	PTERODACTYL_NODE_ID      optional; a default node when a plan names none
//	PTERODACTYL_DOCKER_IMAGE optional; overrides the egg's own image
//	PTERODACTYL_ALLOCATION_ID optional; a fixed allocation instead of the first free one
//	PTERODACTYL_TIMEOUT_MS   optional; per-call HTTP timeout, default 30000
//	PTERODACTYL_REGIONS      optional; comma-separated region labels to advertise
//
// # Plans
//
// Pterodactyl has no concept of a sellable plan, so a plan is a named bundle of
// limits and is configured here rather than discovered. PTERODACTYL_PLANS is a
// comma-separated list of `id:memory_mb:disk_mb:cpu_percent`, e.g.
//
//	PTERODACTYL_PLANS=small:2048:10240:100,medium:4096:20480:200
//
// An unknown plan id is refused by provisioner.validate rather than guessed at,
// because guessing a plan means creating a server of the wrong size and billing
// for it.
//
// # Environment variables (UPG-12)
//
// A product's config options reach the adapter as metadata entries prefixed
// `env.`, and are passed to the egg's startup environment verbatim. That is how
// "Minecraft version" or "max players" becomes a real environment variable on the
// server without the adapter knowing what any of them mean.
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

// protocolVersion is the only protocol version this extension speaks.
const protocolVersion = 1

// maxInput bounds a request frame.
const maxInput = 4 << 20

// JSON-RPC codes, matching the fixture's set so the core maps them identically.
const (
	codeInvalidRequest  = -32600
	codeMethodNotFound  = -32601
	codeInvalidParams   = -32602
	codeInternal        = -32000
	codeVersionMismatch = -32001
	codeNotFound        = -32004
	codeRejected        = -32005
)

func main() {
	cfg, err := configFromEnv(os.Environ())
	if err != nil {
		fmt.Fprintf(os.Stderr, "pterodactyl: %v\n", err)
		os.Exit(2)
	}
	s := &server{client: newPteroClient(cfg), cfg: cfg}
	if err := s.serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "pterodactyl: %v\n", err)
		os.Exit(1)
	}
}

// plan is one sellable size, configured rather than discovered.
type plan struct {
	ID       string
	MemoryMB int64
	DiskMB   int64
	// CPUPercent is Pterodactyl's own unit: 100 is one core.
	CPUPercent int64
}

// config is the extension's whole configuration, read once at startup.
type config struct {
	panelURL     string
	apiKey       string
	nestID       string
	eggID        string
	nodeID       string
	dockerImage  string
	allocationID string
	timeout      time.Duration
	regions      []string
	plans        []plan
	plansByID    map[string]plan
}

// configFromEnv reads and validates the configuration. Every missing required
// value is a startup failure: a provisioner that starts without them would fail
// every operation, and a queue full of failures looks like a broken worker rather
// than a missing setting.
func configFromEnv(environ []string) (config, error) {
	env := make(map[string]string, len(environ))
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	cfg := config{
		panelURL:     strings.TrimRight(strings.TrimSpace(env["PTERODACTYL_PANEL_URL"]), "/"),
		apiKey:       strings.TrimSpace(env["PTERODACTYL_API_KEY"]),
		nestID:       strings.TrimSpace(env["PTERODACTYL_NEST_ID"]),
		eggID:        strings.TrimSpace(env["PTERODACTYL_EGG_ID"]),
		nodeID:       strings.TrimSpace(env["PTERODACTYL_NODE_ID"]),
		dockerImage:  strings.TrimSpace(env["PTERODACTYL_DOCKER_IMAGE"]),
		allocationID: strings.TrimSpace(env["PTERODACTYL_ALLOCATION_ID"]),
		timeout:      30 * time.Second,
		plansByID:    map[string]plan{},
	}
	var missing []string
	for _, required := range []struct{ name, value string }{
		{"PTERODACTYL_PANEL_URL", cfg.panelURL},
		{"PTERODACTYL_API_KEY", cfg.apiKey},
		{"PTERODACTYL_NEST_ID", cfg.nestID},
		{"PTERODACTYL_EGG_ID", cfg.eggID},
	} {
		if required.value == "" {
			missing = append(missing, required.name)
		}
	}
	if len(missing) > 0 {
		return config{}, fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}
	// A client key (ptlc_) cannot create servers; catching it here turns a
	// confusing 403 on every operation into one clear startup error.
	if !strings.HasPrefix(cfg.apiKey, "ptla_") {
		return config{}, errors.New("PTERODACTYL_API_KEY must be an application key (ptla_…), not a client key")
	}

	if raw := strings.TrimSpace(env["PTERODACTYL_TIMEOUT_MS"]); raw != "" {
		ms, err := strconv.Atoi(raw)
		if err != nil || ms <= 0 {
			return config{}, fmt.Errorf("PTERODACTYL_TIMEOUT_MS=%q is not a positive integer", raw)
		}
		cfg.timeout = time.Duration(ms) * time.Millisecond
	}
	if raw := strings.TrimSpace(env["PTERODACTYL_REGIONS"]); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			if r := strings.TrimSpace(part); r != "" {
				cfg.regions = append(cfg.regions, r)
			}
		}
	}
	plans, err := parsePlans(env["PTERODACTYL_PLANS"])
	if err != nil {
		return config{}, err
	}
	cfg.plans = plans
	for _, p := range plans {
		cfg.plansByID[p.ID] = p
	}
	return cfg, nil
}

// parsePlans reads `id:memory_mb:disk_mb:cpu_percent,…`.
func parsePlans(raw string) ([]plan, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		// One default plan, so a single-size installation needs no configuration
		// and a multi-size one says so explicitly.
		return []plan{{ID: "default", MemoryMB: 2048, DiskMB: 10240, CPUPercent: 100}}, nil
	}
	var out []plan
	seen := map[string]struct{}{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) != 4 {
			return nil, fmt.Errorf("PTERODACTYL_PLANS entry %q is not id:memory_mb:disk_mb:cpu_percent", entry)
		}
		p := plan{ID: strings.TrimSpace(parts[0])}
		if p.ID == "" {
			return nil, fmt.Errorf("PTERODACTYL_PLANS entry %q has no id", entry)
		}
		var err error
		if p.MemoryMB, err = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64); err != nil || p.MemoryMB <= 0 {
			return nil, fmt.Errorf("PTERODACTYL_PLANS entry %q has an invalid memory", entry)
		}
		if p.DiskMB, err = strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64); err != nil || p.DiskMB <= 0 {
			return nil, fmt.Errorf("PTERODACTYL_PLANS entry %q has an invalid disk", entry)
		}
		if p.CPUPercent, err = strconv.ParseInt(strings.TrimSpace(parts[3]), 10, 64); err != nil || p.CPUPercent <= 0 {
			return nil, fmt.Errorf("PTERODACTYL_PLANS entry %q has an invalid cpu", entry)
		}
		if _, dup := seen[p.ID]; dup {
			return nil, fmt.Errorf("PTERODACTYL_PLANS declares %q twice", p.ID)
		}
		seen[p.ID] = struct{}{}
		out = append(out, p)
	}
	return out, nil
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

// reply is one handled request's outcome.
type reply struct {
	result any
	rpcErr *rpcError
}

// server is the extension's state. One goroutine drives it; the HTTP client is the
// only thing it shares.
type server struct {
	client *pteroClient
	cfg    config
}

func (s *server) serve(in io.Reader, out io.Writer) error {
	r := bufio.NewReaderSize(in, 64<<10)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	for {
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
			return fmt.Errorf("parse error: %w", err)
		}
		if req.ID == nil {
			continue // a notification
		}
		if req.JSONRPC != "2.0" {
			if err := enc.Encode(response{JSONRPC: "2.0", ID: *req.ID, Error: &rpcError{
				Code: codeInvalidRequest, Message: "jsonrpc must be 2.0",
			}}); err != nil {
				return err
			}
			continue
		}
		rep := s.handle(&req)
		if err := enc.Encode(response{JSONRPC: "2.0", ID: *req.ID, Result: rep.result, Error: rep.rpcErr}); err != nil {
			return err
		}
	}
}

// capabilities is exactly the methods this adapter implements.
var capabilities = []string{
	"provisioner.describe",
	"provisioner.validate",
	"provisioner.create",
	"provisioner.inspect",
	"provisioner.lookup",
	"provisioner.suspend",
	"provisioner.resume",
	"provisioner.terminate",
	// SVC-16 / SVC-22: a product change has to reach the panel, not just the ledger.
	"provisioner.upgrade",
	// UPG-12: the panel asks which per-purchase inputs this adapter needs.
	"provisioner.fields",
}

func (s *server) handle(req *request) reply {
	switch req.Method {
	case "initialize":
		return s.initialize(req)
	case "health":
		return reply{result: map[string]string{"status": "ok"}}
	case "provisioner.describe":
		return reply{result: s.describe()}
	case "provisioner.validate":
		return s.validate(req)
	case "provisioner.create":
		return s.create(req)
	case "provisioner.lookup":
		return s.lookup(req)
	case "provisioner.inspect":
		return s.inspect(req)
	case "provisioner.suspend":
		return s.setState(req, "suspend")
	case "provisioner.resume":
		return s.setState(req, "resume")
	case "provisioner.terminate":
		return s.terminate(req)
	case "provisioner.upgrade":
		return s.upgrade(req)
	case "provisioner.fields":
		return s.fields(req)
	default:
		return reply{rpcErr: &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}}
	}
}

func (s *server) initialize(req *request) reply {
	var p struct {
		Protocol string `json:"protocol"`
		Versions []int  `json:"versions"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "initialize params: " + err.Error()}}
	}
	for _, v := range p.Versions {
		if v == protocolVersion {
			return reply{result: map[string]any{
				"version":      protocolVersion,
				"name":         "pterodactyl",
				"publisher":    "catalyst",
				"capabilities": capabilities,
			}}
		}
	}
	return reply{rpcErr: &rpcError{
		Code:    codeVersionMismatch,
		Message: "no common protocol version",
		Data:    map[string]any{"supported": []int{protocolVersion}, "offered": p.Versions},
	}}
}

func (s *server) describe() map[string]any {
	plans := make([]map[string]any, 0, len(s.cfg.plans))
	for _, p := range s.cfg.plans {
		plans = append(plans, map[string]any{
			"id":     p.ID,
			"name":   p.ID,
			"amount": map[string]any{"minor": 0, "currency": "USD"},
			// The price is the billing panel's business; this adapter reports the
			// shape of what it can build, not what it costs.
			"period": "monthly",
			"limits": map[string]any{
				"memory_mb":   p.MemoryMB,
				"disk_mb":     p.DiskMB,
				"cpu_percent": p.CPUPercent,
			},
		})
	}
	regions := s.cfg.regions
	if len(regions) == 0 && s.cfg.nodeID != "" {
		regions = []string{s.cfg.nodeID}
	}
	return map[string]any{
		"name":    "pterodactyl",
		"plans":   plans,
		"regions": regions,
	}
}

func (s *server) validate(req *request) reply {
	var p struct {
		PlanID   string `json:"plan_id"`
		Region   string `json:"region"`
		Quantity int32  `json:"quantity"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "validate params: " + err.Error()}}
	}
	problems := s.problemsFor(p.PlanID, p.Region, p.Quantity)
	return reply{result: map[string]any{
		"valid":    len(problems) == 0,
		"problems": problems,
	}}
}

// problemsFor is the single place that decides whether a purchase is buildable.
// create calls it too, so validate and create cannot disagree about what is legal.
func (s *server) problemsFor(planID, region string, quantity int32) []string {
	var problems []string
	if _, ok := s.cfg.plansByID[planID]; !ok {
		problems = append(problems, fmt.Sprintf("unknown plan %q", planID))
	}
	if quantity < 0 {
		problems = append(problems, fmt.Sprintf("quantity %d is negative", quantity))
	}
	if region != "" && s.cfg.nodeID == "" && len(s.cfg.regions) == 0 {
		// A region was asked for but no node is configured to serve it, so the
		// request cannot be honoured and saying so is better than creating the
		// server somewhere else.
		problems = append(problems, fmt.Sprintf("region %q cannot be served: no node is configured", region))
	}
	return problems
}

func (s *server) create(req *request) reply {
	var p struct {
		OperationKey   string            `json:"operation_key"`
		SubscriptionID string            `json:"subscription_id"`
		PlanID         string            `json:"plan_id"`
		Region         string            `json:"region"`
		Quantity       int32             `json:"quantity"`
		Metadata       map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "create params: " + err.Error()}}
	}
	if strings.TrimSpace(p.OperationKey) == "" {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "operation_key is required"}}
	}
	if problems := s.problemsFor(p.PlanID, p.Region, p.Quantity); len(problems) > 0 {
		return reply{rpcErr: &rpcError{
			Code:    codeRejected,
			Message: strings.Join(problems, "; "),
			Data:    map[string]any{"problems": problems},
		}}
	}
	pl := s.cfg.plansByID[p.PlanID]

	// Local preconditions are checked before the lookup: a request that cannot be
	// honoured is refused without asking the panel anything, and the message names
	// what is missing rather than relaying a 422.
	if strings.TrimSpace(p.Metadata["pterodactyl_user_id"]) == "" {
		return reply{rpcErr: &rpcError{
			Code:    codeRejected,
			Message: "a server must be owned by a panel user; set metadata[pterodactyl_user_id]",
			Data:    map[string]any{"problems": []string{"metadata[pterodactyl_user_id] is required"}},
		}}
	}
	environment, err := s.environmentFor(p.Metadata)
	if err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: err.Error()}}
	}

	// A replay must return the server that already exists, not buy a second one.
	// The lookup goes to Pterodactyl rather than to an in-memory map, so it still
	// works after this process has restarted.
	existing, err := s.client.findByExternalID(p.OperationKey)
	if err != nil {
		return pteroErrorReply(err)
	}
	if existing != nil {
		return reply{result: map[string]any{
			"external_id": existing.identifier(),
			"status":      existing.protocolStatus(),
			"plan_id":     p.PlanID,
			"region":      p.Region,
			"created_at":  existing.createdAt(),
		}}
	}

	allocationID := s.cfg.allocationID
	if v := p.Metadata["allocation_id"]; v != "" {
		allocationID = v
	}
	if allocationID == "" {
		// Pick the first free allocation on the node. Pterodactyl requires one and
		// has no "any" option, so this is the adapter's job.
		nodeID := firstNonEmpty(p.Region, s.cfg.nodeID)
		if nodeID == "" {
			return reply{rpcErr: &rpcError{
				Code:    codeRejected,
				Message: "no allocation_id in metadata and no node configured to choose one from",
			}}
		}
		id, err := s.client.firstFreeAllocation(nodeID)
		if err != nil {
			return pteroErrorReply(err)
		}
		allocationID = id
	}

	created, err := s.client.createServer(createServerRequest{
		Name:         firstNonEmpty(p.Metadata["name"], "server-"+shortKey(p.OperationKey)),
		UserID:       p.Metadata["pterodactyl_user_id"],
		Description:  p.Metadata["description"],
		ExternalID:   p.OperationKey,
		EggID:        firstNonEmpty(p.Metadata["egg_id"], s.cfg.eggID),
		NestID:       s.cfg.nestID,
		DockerImage:  firstNonEmpty(p.Metadata["docker_image"], s.cfg.dockerImage),
		Startup:      p.Metadata["startup"],
		Environment:  environment,
		MemoryMB:     pl.MemoryMB,
		DiskMB:       pl.DiskMB,
		CPUPercent:   pl.CPUPercent,
		AllocationID: allocationID,
	})
	if err != nil {
		return pteroErrorReply(err)
	}
	return reply{result: map[string]any{
		"external_id": created.identifier(),
		"status":      created.protocolStatus(),
		"plan_id":     p.PlanID,
		"region":      p.Region,
		"created_at":  created.createdAt(),
	}}
}

// environmentFor turns a product's config options into the egg's environment.
//
// A metadata key prefixed `env.` is passed through verbatim, which is how a
// "Minecraft version" option becomes an environment variable without this adapter
// knowing what it means (UPG-12). Everything else is the panel's own bookkeeping
// and must not leak into the server's environment.
func (s *server) environmentFor(metadata map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for key, value := range metadata {
		if !strings.HasPrefix(key, "env.") {
			continue
		}
		name := strings.TrimPrefix(key, "env.")
		if name == "" {
			return nil, errors.New("a metadata key is `env.` with no variable name")
		}
		// Pterodactyl stores the environment as JSON and reads it back as a
		// string map, so a value that is not valid UTF-8 or that contains a NUL
		// would corrupt the record rather than fail loudly.
		if strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("environment variable %q contains a NUL byte", name)
		}
		out[name] = value
	}
	return out, nil
}

func (s *server) lookup(req *request) reply {
	var p struct {
		OperationKey string `json:"operation_key"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "lookup params: " + err.Error()}}
	}
	if strings.TrimSpace(p.OperationKey) == "" {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "operation_key is required"}}
	}
	found, err := s.client.findByExternalID(p.OperationKey)
	if err != nil {
		// An adapter that cannot answer must return an error, so the core never
		// mistakes "I do not know" for "it does not exist" and provisions again.
		return pteroErrorReply(err)
	}
	if found == nil {
		return reply{result: map[string]any{"found": false}}
	}
	return reply{result: map[string]any{
		"found":       true,
		"external_id": found.identifier(),
		"status":      string(found.protocolStatus()),
	}}
}

func (s *server) inspect(req *request) reply {
	var p struct {
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "inspect params: " + err.Error()}}
	}
	if p.ExternalID == "" {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "external_id is required"}}
	}
	srv, err := s.client.getServer(p.ExternalID)
	if err != nil {
		return pteroErrorReply(err)
	}
	out := map[string]any{
		"external_id": srv.identifier(),
		"status":      srv.protocolStatus(),
		"plan_id":     "",
		"region":      srv.NodeID,
		"created_at":  srv.createdAt(),
	}
	if srv.isSuspended() {
		out["suspended_at"] = time.Now().UTC()
	}
	return reply{result: out}
}

// setState handles suspend and resume, which differ only in the endpoint.
func (s *server) setState(req *request, kind string) reply {
	var p struct {
		OperationKey string `json:"operation_key"`
		ExternalID   string `json:"external_id"`
		Reason       string `json:"reason"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: kind + " params: " + err.Error()}}
	}
	if p.ExternalID == "" {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "external_id is required"}}
	}
	srv, err := s.client.getServer(p.ExternalID)
	if err != nil {
		return pteroErrorReply(err)
	}

	// Asking for a state the server is already in is a success with Changed=false,
	// not an error: the core's suspend is idempotent and a second call must not
	// look like a failure.
	switch kind {
	case "suspend":
		if srv.isSuspended() {
			return reply{result: map[string]any{
				"external_id": srv.identifier(), "status": "suspended", "changed": false,
			}}
		}
		if err := s.client.suspendServer(srv.ID); err != nil {
			return pteroErrorReply(err)
		}
		return reply{result: map[string]any{
			"external_id": srv.identifier(), "status": "suspended", "changed": true,
		}}
	default:
		if !srv.isSuspended() {
			return reply{result: map[string]any{
				"external_id": srv.identifier(), "status": srv.protocolStatus(), "changed": false,
			}}
		}
		if err := s.client.unsuspendServer(srv.ID); err != nil {
			return pteroErrorReply(err)
		}
		return reply{result: map[string]any{
			"external_id": srv.identifier(), "status": "active", "changed": true,
		}}
	}
}

// fields is what this adapter needs to create a server for a customer (UPG-12).
//
// Pterodactyl refuses a create with no `user`, and the panel user id is a per-customer
// fact the panel cannot invent: it is not the billing customer id, it is the id in the
// game panel. Declaring it here is what lets the purchase form collect it *before* the
// server is bought, instead of failing the provisioning call with a 422 an hour later.
func (s *server) fields(*request) reply {
	return reply{result: map[string]any{"fields": []map[string]any{
		{
			"key":      "pterodactyl_user_id",
			"label":    "Panel user ID",
			"type":     "text",
			"required": true,
			"hint":     "The numeric user id on the Pterodactyl panel that will own the server. It is not the billing customer id.",
		},
		{
			"key":   "name",
			"label": "Server name",
			"type":  "text",
			"hint":  "Optional. Defaults to a short suffix of the operation key.",
		},
		{
			"key":   "description",
			"label": "Description",
			"type":  "text",
		},
		{
			"key":   "allocation_id",
			"label": "Allocation ID",
			"type":  "text",
			"hint":  "Optional. A fixed allocation instead of the first free port on the node.",
		},
	}}}
}

// upgrade moves an existing server to another plan (SVC-16, SVC-22, UPG-05).
//
// It is the call that was missing: the ledger moved and the server did not, so a customer
// who paid to upgrade from 2 GB to 4 GB kept the 2 GB box until an operator edited the
// panel by hand.
//
// Two requests, because Pterodactyl splits the two things a plan consists of: the *build*
// (memory, disk, cpu) and the *startup environment* (the egg's variables, where a product's
// config options land). Both are sent, because a resize that resized the box and dropped the
// customer's "Minecraft version" would be a different kind of data loss.
func (s *server) upgrade(req *request) reply {
	var p struct {
		OperationKey string            `json:"operation_key"`
		ExternalID   string            `json:"external_id"`
		PlanID       string            `json:"plan_id"`
		Region       string            `json:"region"`
		Quantity     int32             `json:"quantity"`
		Metadata     map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "upgrade params: " + err.Error()}}
	}
	if strings.TrimSpace(p.ExternalID) == "" {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "external_id is required"}}
	}
	pl, ok := s.cfg.plansByID[p.PlanID]
	if !ok {
		// Guessing a size would resize a paid server to the wrong plan and report success.
		msg := fmt.Sprintf("unknown plan %q", p.PlanID)
		return reply{rpcErr: &rpcError{
			Code: codeRejected, Message: msg, Data: map[string]any{"problems": []string{msg}},
		}}
	}
	environment, err := s.environmentFor(p.Metadata)
	if err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: err.Error()}}
	}

	srv, err := s.client.getServer(p.ExternalID)
	if err != nil {
		return pteroErrorReply(err)
	}
	// A replay of a completed upgrade must report what is true now rather than claim another
	// change, so both halves are read back from the panel before anything is written.
	buildChanged := !srv.atPlan(pl)
	envChanged := !srv.hasEnvironment(environment)
	if buildChanged {
		if err := s.client.updateBuild(srv.ID, pl); err != nil {
			return pteroErrorReply(err)
		}
	}
	if envChanged {
		if err := s.client.updateStartup(srv.ID, environment, p.Metadata["startup"]); err != nil {
			return pteroErrorReply(err)
		}
	}
	return reply{result: map[string]any{
		"external_id": srv.identifier(),
		"status":      srv.protocolStatus(),
		"plan_id":     pl.ID,
		"region":      firstNonEmpty(p.Region, fmt.Sprintf("%d", srv.NodeID)),
		"changed":     buildChanged || envChanged,
	}}
}

func (s *server) terminate(req *request) reply {
	var p struct {
		OperationKey string `json:"operation_key"`
		ExternalID   string `json:"external_id"`
		Reason       string `json:"reason"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "terminate params: " + err.Error()}}
	}
	if p.ExternalID == "" {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "external_id is required"}}
	}
	srv, err := s.client.getServer(p.ExternalID)
	if err != nil {
		// Terminating a server that is already gone is the requested end state, so
		// a 404 is success with Changed=false rather than an error the core would
		// retry forever.
		var ae *apiError
		if errors.As(err, &ae) && ae.httpStatus == 404 {
			return reply{result: map[string]any{
				"external_id": p.ExternalID, "status": "terminated", "changed": false,
			}}
		}
		return pteroErrorReply(err)
	}
	if err := s.client.deleteServer(srv.ID); err != nil {
		return pteroErrorReply(err)
	}
	return reply{result: map[string]any{
		"external_id": srv.identifier(), "status": "terminated", "changed": true,
	}}
}

// pteroErrorReply maps an adapter error onto a JSON-RPC error, preserving the
// distinction the core branches on: not-found, rejected on the merits, or broken.
func pteroErrorReply(err error) reply {
	var ae *apiError
	if errors.As(err, &ae) {
		switch {
		case ae.httpStatus == 404:
			return reply{rpcErr: &rpcError{Code: codeNotFound, Message: ae.Error(), Data: ae.data()}}
		case ae.httpStatus == 422 || ae.httpStatus == 409:
			// A validation refusal or a conflict is a business answer, not a
			// transport failure.
			return reply{rpcErr: &rpcError{Code: codeRejected, Message: ae.Error(), Data: ae.data()}}
		case ae.httpStatus >= 400 && ae.httpStatus < 500 && ae.httpStatus != 401 && ae.httpStatus != 403:
			return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: ae.Error(), Data: ae.data()}}
		default:
			return reply{rpcErr: &rpcError{Code: codeInternal, Message: ae.Error(), Data: ae.data()}}
		}
	}
	return reply{rpcErr: &rpcError{Code: codeInternal, Message: err.Error()}}
}

// shortKey is a readable suffix for a server name.
func shortKey(key string) string {
	if len(key) <= 8 {
		return key
	}
	return key[len(key)-8:]
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// readLine reads one newline-terminated frame, bounded by max.
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > max {
			return nil, fmt.Errorf("request frame exceeds %d bytes", max)
		}
		buf = append(buf, chunk...)
		if err == nil {
			return trimEOL(buf), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return nil, err
	}
}

func trimEOL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
