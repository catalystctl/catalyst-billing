// Command catalystctl implements the CatalystCTL game panel as a billing provisioner
// extension.
//
// It speaks the same newline-delimited JSON-RPC 2.0 protocol as extensions/fakeprov, and the
// calls go to the Catalyst panel's own API over HTTPS. The core's queue, leases, fencing and
// reconciliation are unchanged: what this file adds is an adapter that answers honestly.
//
// # What it does beyond create/suspend/resume/terminate
//
// The panel's customers expect to see their server without leaving billing, so this adapter also
// implements the two calls that make that possible:
//
//	provisioner.resource  a pure read: name, live state, address, panel deep link, and metrics
//	provisioner.power     a mutating, idempotent call: start, stop, restart, kill
//
// Both are advertised as capabilities, so a core that predates them simply never asks, and an
// adapter that cannot measure a resource simply omits the numbers.
//
// # Why the operation key is stored twice
//
// `provisioner.lookup` must be authoritative, and Catalyst has no `external_id` field on a server
// (Pterodactyl does, which is why the Pterodactyl adapter needs no ledger). So the key is written
// to a local ledger before the create is sent, and the server's description carries a
// `billing:<key>` marker as a second, recoverable record. See state.go for the argument.
//
// # Configuration
//
//	CCTL_PANEL_URL      required; e.g. https://panel.example.com
//	CCTL_API_KEY        required; a panel API key (catalyst_…)
//	CCTL_TEMPLATE_ID    required; the template new servers are created from
//	CCTL_NODE_ID        required; the node new servers are placed on
//	CCTL_LOCATION_ID    required; the location the node belongs to
//	CCTL_OWNER_ID       optional; the panel user that owns new servers
//	CCTL_PLANS          optional; id:memory_mb:disk_mb:cpu_cores bundles
//	CCTL_NETWORK_MODE   optional; bridge, macvlan, host, mc-lan-static, mc-lan-dynamic
//	CCTL_PANEL_PATH     optional; the customer-facing path of a server, default /servers/{id}
//	CCTL_TIMEOUT_MS     optional; per-call HTTP timeout, default 30000
//	CCTL_STATE_FILE     optional; the operation ledger, default /var/lib/catalyst/catalystctl/operations.json
//	CCTL_VERIFY_TLS     optional; default on. Off prints a warning at startup
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Protocol versions this extension speaks. Version 2 is what advertises ui.describe and event
// delivery; this adapter wants neither, so it speaks both and picks what the core offers.
const (
	protocolFamily = "billing.ext/1"
	maxInput       = 4 << 20
)

// JSON-RPC codes, matching the core's set so it maps them identically.
const (
	codeParseError      = -32700
	codeInvalidRequest  = -32600
	codeMethodNotFound  = -32601
	codeInvalidParams   = -32602
	codeInternal        = -32000
	codeVersionMismatch = -32001
	codeNotFound        = -32004
	codeRejected        = -32005
)

// errScanExhausted is returned when a lookup ran out of pages before it could answer. It is
// deliberately not a "not found": the core treats that as authoritative.
var errScanExhausted = errors.New("lookup could not be answered")

func main() {
	cfg, err := configFromEnv(os.Environ())
	if err != nil {
		fmt.Fprintf(os.Stderr, "catalystctl: %v\n", err)
		os.Exit(2)
	}
	if !cfg.verifyTLS {
		fmt.Fprintln(os.Stderr, "catalystctl: CCTL_VERIFY_TLS is off; the panel's certificate is not verified")
	}
	s := &server{
		client: newPanelClient(cfg),
		cfg:    cfg,
		ledger: newLedger(cfg.stateFile),
		byKey:  map[string]any{},
	}
	if err := s.serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "catalystctl: %v\n", err)
		os.Exit(1)
	}
}

// plan is one sellable size, configured rather than discovered: a panel has no concept of a
// sellable plan, so a plan is a named bundle of limits.
type plan struct {
	ID       string
	MemoryMB int64
	DiskMB   int64
	// CPUCores is whole cores, which is the panel's own unit for a server.
	CPUCores int64
}

// config is the adapter's whole configuration, read once at startup.
type config struct {
	panelURL   string
	apiKey     string
	templateID string
	nodeID     string
	locationID string
	ownerID    string
	// userMode and userRoleID govern the panel account a customer's server belongs to. They are
	// EnvOnly: a role id is panel-specific, and an installation that sets it once should not have to
	// repeat it on every service.
	userMode    panelUserMode
	userRoleID  string
	networkMode string
	panelPath   string
	timeout     time.Duration
	stateFile   string
	verifyTLS   bool
	plans       []plan
	plansByID   map[string]plan
}

// configFromEnv reads and validates the configuration.
//
// Only the credentials are required. A provisioner that starts without its panel address would fail
// every operation, and a queue full of failures looks like a broken worker rather than a missing
// setting — which is the exact confusion the cPanel adapter's first production run produced. The
// placement settings that follow are optional *defaults*, because where a server goes belongs to the
// service being sold, not to the adapter.
func configFromEnv(environ []string) (config, error) {
	env := make(map[string]string, len(environ))
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	cfg := config{
		panelURL:    strings.TrimRight(strings.TrimSpace(env["CCTL_PANEL_URL"]), "/"),
		apiKey:      strings.TrimSpace(env["CCTL_API_KEY"]),
		templateID:  strings.TrimSpace(env["CCTL_TEMPLATE_ID"]),
		nodeID:      strings.TrimSpace(env["CCTL_NODE_ID"]),
		locationID:  strings.TrimSpace(env["CCTL_LOCATION_ID"]),
		ownerID:     strings.TrimSpace(env["CCTL_OWNER_ID"]),
		userMode:    panelUserMode(strings.TrimSpace(env["CCTL_USER_MODE"])),
		userRoleID:  strings.TrimSpace(env["CCTL_USER_ROLE_ID"]),
		networkMode: strings.TrimSpace(env["CCTL_NETWORK_MODE"]),
		panelPath:   strings.TrimSpace(env["CCTL_PANEL_PATH"]),
		timeout:     30 * time.Second,
		stateFile:   strings.TrimSpace(env["CCTL_STATE_FILE"]),
		verifyTLS:   true,
		plansByID:   map[string]plan{},
	}
	if cfg.panelPath == "" {
		cfg.panelPath = "/servers/{id}"
	}
	if cfg.stateFile == "" {
		cfg.stateFile = "/var/lib/catalyst/catalystctl/operations.json"
	}

	// Only the panel's address and key are required. Everything else — template, location, node,
	// size — is a property of the *service* being sold, resolved per purchase from the adapter's own
	// provisioning fields or from a product's config option. Requiring them here would mean one
	// adapter deployment per product, which is the opposite of a catalogue.
	var missing []string
	for _, required := range []struct{ name, value string }{
		{"CCTL_PANEL_URL", cfg.panelURL},
		{"CCTL_API_KEY", cfg.apiKey},
	} {
		if required.value == "" {
			missing = append(missing, required.name)
		}
	}
	if len(missing) > 0 {
		return config{}, fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}
	// Catching an obviously wrong key here turns a confusing 401 on every operation into one clear
	// startup error.
	if !strings.HasPrefix(cfg.apiKey, "catalyst_") {
		return config{}, errors.New("CCTL_API_KEY must be a panel API key (catalyst_…)")
	}

	// Creating a panel account per customer is the default: a server owned by the API key's own user is
	// a server the customer cannot open, and one panel login would expose every customer's server.
	if cfg.userMode == "" {
		cfg.userMode = userModeCreate
	}
	if !validUserMode(string(cfg.userMode)) {
		return config{}, fmt.Errorf("CCTL_USER_MODE=%q is not create or none", string(cfg.userMode))
	}

	if raw := strings.TrimSpace(env["CCTL_TIMEOUT_MS"]); raw != "" {
		ms, err := strconv.Atoi(raw)
		if err != nil || ms <= 0 {
			return config{}, fmt.Errorf("CCTL_TIMEOUT_MS=%q is not a positive integer", raw)
		}
		cfg.timeout = time.Duration(ms) * time.Millisecond
	}
	if raw := strings.TrimSpace(env["CCTL_VERIFY_TLS"]); raw != "" {
		switch strings.ToLower(raw) {
		case "0", "false", "no", "off":
			cfg.verifyTLS = false
		case "1", "true", "yes", "on":
			cfg.verifyTLS = true
		default:
			return config{}, fmt.Errorf("CCTL_VERIFY_TLS=%q is not a boolean", raw)
		}
	}
	switch cfg.networkMode {
	case "", "bridge", "macvlan", "host", "mc-lan-static", "mc-lan-dynamic":
	default:
		return config{}, fmt.Errorf("CCTL_NETWORK_MODE=%q is not one of bridge, macvlan, host, mc-lan-static, mc-lan-dynamic",
			cfg.networkMode)
	}
	// Bridge is the default, and it is a deliberate choice rather than a fallback.
	//
	// Bridge is the only mode where the panel reserves a host port through an allocation, which is what
	// makes two concurrent creates unable to collide on the same port and what gives the customer a stable
	// address to connect to. Left unset, the panel picks its own default (mc-lan-static on the reference
	// installation), which needs a static IP pool per node and hands out an address the operator has to
	// manage. Sending bridge explicitly means one code path, one pool requirement, and no dependence on
	// what a particular panel happens to default to.
	if cfg.networkMode == "" {
		cfg.networkMode = "bridge"
	}

	plans, err := parsePlans(env["CCTL_PLANS"])
	if err != nil {
		return config{}, err
	}
	cfg.plans = plans
	for _, p := range plans {
		cfg.plansByID[p.ID] = p
	}
	return cfg, nil
}

// parsePlans reads `id:memory_mb:disk_mb:cpu_cores,…`.
func parsePlans(raw string) ([]plan, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		// One default plan, so a single-size installation needs no configuration and a multi-size
		// one says so explicitly.
		return []plan{{ID: "default", MemoryMB: 2048, DiskMB: 10240, CPUCores: 1}}, nil
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
			return nil, fmt.Errorf("CCTL_PLANS entry %q is not id:memory_mb:disk_mb:cpu_cores", entry)
		}
		id := strings.TrimSpace(parts[0])
		if id == "" {
			return nil, fmt.Errorf("CCTL_PLANS entry %q has an empty id", entry)
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("CCTL_PLANS declares %q twice", id)
		}
		mem, err1 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		disk, err2 := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64)
		cpu, err3 := strconv.ParseInt(strings.TrimSpace(parts[3]), 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			return nil, fmt.Errorf("CCTL_PLANS entry %q has a non-numeric limit", entry)
		}
		// The panel's own minimums. Catching them here means an operator reads one startup error
		// instead of every create failing with VALIDATION_ERROR.
		if mem < 512 {
			return nil, fmt.Errorf("CCTL_PLANS entry %q: memory must be at least 512 MB", entry)
		}
		if disk < 1024 {
			return nil, fmt.Errorf("CCTL_PLANS entry %q: disk must be at least 1024 MB", entry)
		}
		if cpu < 1 {
			return nil, fmt.Errorf("CCTL_PLANS entry %q: cpu must be at least 1 core", entry)
		}
		seen[id] = struct{}{}
		out = append(out, plan{ID: id, MemoryMB: mem, DiskMB: disk, CPUCores: cpu})
	}
	if len(out) == 0 {
		return nil, errors.New("CCTL_PLANS declares no plans")
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

// rpcError is the outgoing error object.
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

// server is the adapter's state. It is driven by a single goroutine.
type server struct {
	client *panelClient
	cfg    config
	ledger *ledger

	// version is the negotiated protocol version; 0 before the handshake.
	version int
	// byKey replays a mutating call within one process lifetime. The ledger is the durable
	// record; this is the cheap in-memory answer for a double-tap.
	byKey map[string]any
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
			// A parse failure is fatal for a JSON-RPC stream: we cannot know which id it belonged to.
			return fmt.Errorf("parse error: %w", err)
		}
		if req.ID == nil {
			// A notification. This adapter subscribes to no events, so there is nothing to do and
			// nothing is answered.
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
	}
}

// handle dispatches one request.
func (s *server) handle(req *request) reply {
	switch req.Method {
	case "initialize":
		return s.initialize(req)
	case "health":
		return reply{result: map[string]string{"status": "ok"}}
	case "provisioner.describe":
		return s.describe()
	case "provisioner.validate":
		return s.validate(req)
	case "provisioner.create":
		return s.create(req)
	case "provisioner.inspect":
		return s.inspect(req)
	case "provisioner.lookup":
		return s.lookup(req)
	case "provisioner.suspend":
		return s.suspend(req)
	case "provisioner.resume":
		return s.resume(req)
	case "provisioner.terminate":
		return s.terminate(req)
	case "provisioner.fields":
		return s.fields(req)
	case "provisioner.resource":
		return s.resource(req)
	case "provisioner.power":
		return s.power(req)
	}
	return reply{rpcErr: &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}}
}

// initialize negotiates the protocol version.
func (s *server) initialize(req *request) reply {
	var p struct {
		Protocol    string `json:"protocol"`
		Versions    []int  `json:"versions"`
		CoreVersion string `json:"core_version"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "initialize params: " + err.Error()}}
	}
	if p.Protocol != protocolFamily {
		return reply{rpcErr: &rpcError{Code: codeVersionMismatch, Message: "unknown protocol " + p.Protocol}}
	}
	// Newest version both sides speak. This adapter uses nothing that version 2 added, but it
	// accepts it so a core that only offers 2 is not refused.
	chosen := 0
	for _, theirs := range p.Versions {
		if (theirs == 1 || theirs == 2) && theirs > chosen {
			chosen = theirs
		}
	}
	if chosen == 0 {
		return reply{rpcErr: &rpcError{
			Code:    codeVersionMismatch,
			Message: "no common protocol version",
			Data:    map[string]any{"supported": []int{2, 1}, "offered": p.Versions},
		}}
	}
	s.version = chosen
	return reply{result: map[string]any{
		"version":      chosen,
		"name":         "catalystctl",
		"publisher":    "catalyst",
		"capabilities": s.capabilities(),
	}}
}

// capabilities is what this adapter promises to implement.
func (s *server) capabilities() []string {
	return []string{
		"health",
		"provisioner.describe",
		"provisioner.validate",
		"provisioner.create",
		"provisioner.inspect",
		"provisioner.lookup",
		"provisioner.suspend",
		"provisioner.resume",
		"provisioner.terminate",
		"provisioner.fields",
		"provisioner.resource",
		"provisioner.power",
	}
}

// describe answers what the adapter can sell.
func (s *server) describe() reply {
	plans := make([]map[string]any, 0, len(s.cfg.plans))
	for _, p := range s.cfg.plans {
		plans = append(plans, map[string]any{
			"id":   p.ID,
			"name": p.ID,
			// The adapter is not the price list: the billing panel prices a product and the plan is
			// only a size. Reporting a zero amount is the honest answer to "what would you charge".
			"amount": map[string]any{"minor": 0, "currency": "USD"},
			"period": "monthly",
		})
	}
	return reply{result: map[string]any{"name": "catalystctl", "plans": plans}}
}

// validate is a pre-flight, not a plan lookup.
//
// `plan_id` is the *billing* catalogue's identifier, which this adapter has no opinion about: sizes
// come from the service's own provisioning fields. What can be checked without a purchase is whether
// the panel is reachable and has anything sellable — which is the question that actually prevents a
// checkout that fails five minutes later. A panel that is down reports it as a problem rather than an
// error so the checkout can decide whether to block.
func (s *server) validate(req *request) reply {
	var p struct {
		PlanID   string `json:"plan_id"`
		Region   string `json:"region"`
		Quantity int32  `json:"quantity"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "validate params: " + err.Error()}}
	}
	if p.Quantity < 0 {
		return reply{result: map[string]any{"valid": false, "problems": []string{"quantity cannot be negative"}}}
	}

	ctx, done := contextFor(req)
	defer done()
	locations, err := s.client.locations(ctx)
	if err != nil {
		return reply{result: map[string]any{
			"valid":    false,
			"problems": []string{"the server panel could not be reached: " + err.Error()},
		}}
	}
	if len(locations) == 0 {
		return reply{result: map[string]any{
			"valid":    false,
			"problems": []string{"the server panel has no locations; add one before selling a server"},
		}}
	}
	return reply{result: map[string]any{"valid": true}}
}

// planFor resolves a size bundle.
//
// An unknown id is *not* a refusal: the core sends the billing catalogue's plan id here, and this
// adapter's bundles are an optional shorthand. A caller that names nothing, or names something this
// adapter has never heard of, falls back to the built-in size and the service's own fields — the
// purchase is still explicit about what it wants.
func (s *server) planFor(id string) (plan, bool) {
	if id = strings.TrimSpace(id); id != "" {
		if p, ok := s.cfg.plansByID[id]; ok {
			return p, true
		}
	}
	if len(s.cfg.plans) == 1 {
		return s.cfg.plans[0], true
	}
	return plan{}, false
}

// sizeDefaults is the size a purchase gets when neither a bundle nor a field names one.
func (s *server) sizeDefaults() plan {
	if p, ok := s.planFor(""); ok {
		return p
	}
	return plan{ID: "default", MemoryMB: 2048, DiskMB: 10240, CPUCores: 1}
}

// create buys a server.
//
// The order of operations is the whole idempotency argument:
//
//  1. read the ledger; a key already recorded as created returns that server, so a retry after a
//     lost reply never creates a second one;
//  2. write the intent to the ledger *before* the request, so a crash mid-call leaves a record a
//     lookup can act on;
//  3. create;
//  4. record the id.
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
	if err := validateKey(p.OperationKey); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: err.Error()}}
	}
	// The billing catalogue's plan id is recorded for the ledger and otherwise advisory: the size
	// a server actually gets comes from the service's placement fields, which are explicit about it.
	// A plan id this adapter has never heard of is therefore normal, not an error.
	pl, _ := s.planFor(p.PlanID)
	if pl.ID == "" {
		pl = s.sizeDefaults()
	}

	ctx, done := contextFor(req)
	defer done()

	// 1. A recorded create is the answer, with no second call.
	if rec, found, err := s.ledger.get(p.OperationKey); err != nil {
		return reply{rpcErr: internalError(err)}
	} else if found && rec.State == recordCreated && rec.ExternalID != "" {
		srv, err := s.client.getServer(ctx, rec.ExternalID)
		if err == nil {
			return reply{result: createResult(srv, pl.ID, p.Region)}
		}
		if !isNotFound(err) {
			return reply{rpcErr: internalError(err)}
		}
		// The panel says it is gone. Fall through and create, which is the correct repair for a
		// resource somebody deleted by hand.
	}

	name := serverName(p.Metadata, p.SubscriptionID)

	// 2. Resolve placement *before* recording intent. A create that cannot say where the server goes
	// must not be written down as in flight: the ledger's job is to record an act that may have
	// happened, and nothing has happened yet.
	//
	// Placement comes from the service, not the adapter: which location, node and template a server
	// needs is a fact about the thing being sold. This adapter holds the panel's address and key and
	// nothing else about the catalogue.
	place, err := s.resolvePlacement(ctx, p.Metadata)
	if err != nil {
		return reply{rpcErr: &rpcError{Code: codeRejected, Message: err.Error(),
			Data: map[string]any{"problems": []string{err.Error()}}}}
	}
	if place.Name != "" {
		name = place.Name
	}

	// 3. Intent first: an adapter that cannot record what it is about to do must not do it.
	if err := s.ledger.put(operationRecord{
		OperationKey: p.OperationKey,
		Name:         name,
		PlanID:       pl.ID,
		Region:       place.LocationName,
		State:        recordCreating,
	}); err != nil {
		return reply{rpcErr: internalError(err)}
	}

	// The account the server belongs to. A panel is multi-tenant, so this is what makes the console link
	// we show the customer open *their* server rather than one in a shared account.
	ownerID, err := s.ensurePanelUser(ctx, p.Metadata)
	if err != nil {
		if isRejection(err) {
			return reply{rpcErr: &rpcError{Code: codeRejected, Message: "could not create the customer's panel account: " + err.Error()}}
		}
		return reply{rpcErr: internalError(err)}
	}

	allocation, err := s.claimAllocation(ctx, place)
	if err != nil {
		return reply{rpcErr: &rpcError{Code: codeRejected, Message: err.Error()}}
	}

	createReq := createRequest{
		Name:              name,
		Description:       markerFor(p.OperationKey),
		TemplateID:        place.TemplateID,
		NodeID:            place.NodeID,
		LocationID:        place.LocationID,
		OwnerID:           ownerID,
		PrimaryPort:       allocation.Port,
		AllocatedMemoryMb: place.MemoryMB,
		AllocatedCpuCores: place.CPUCores,
		AllocatedDiskMb:   place.DiskMB,
		Environment:       environmentFor(p.Metadata),
	}
	// How the server gets its address depends on the network mode, and the panel is strict about it:
	// an allocation is only accepted for bridge networking, where it reserves a host port. Every other
	// mode binds an address directly, so the address is sent instead.
	//
	// The free allocation is read either way, because its *port* is the port to use and reserving it is
	// what stops two concurrent creates from racing for the same one.
	if usesBridgeNetworking(place.NetworkMode) {
		createReq.AllocationID = allocation.ID
	} else if place.PrimaryIP != "" {
		// A static address is only sent when the service asked for one. Otherwise the panel's own IPAM
		// allocates from the pool configured for the network, and that is authoritative: the reference
		// panel refused a create with "No IP pool configured for this network" until a pool existed, and
		// once one did it assigned the address itself. Sending the node's address instead would take an
		// address outside the pool and collide with the node.
		createReq.PrimaryIP = place.PrimaryIP
	}
	if place.NetworkMode != "" {
		mode := place.NetworkMode
		createReq.NetworkMode = &mode
	}

	srv, err := s.client.createServer(ctx, createReq)
	// A port that looked free and was not: another create took it between the list and this request, or the
	// panel's allocation table disagrees with a running server. Trying the next free port is the correct
	// repair — the operation key already recorded intent, so the alternative is a failed purchase for a
	// reason the customer cannot act on.
	for attempt := 0; err != nil && portConflict(err) && attempt < 2; attempt++ {
		used, usedErr := s.client.portsInUse(ctx, place.NodeID)
		if usedErr != nil {
			break
		}
		used[allocation.Port] = struct{}{}
		allocation, err = s.client.freeAllocation(ctx, place.NodeID, used)
		if err != nil {
			break
		}
		createReq.PrimaryPort = allocation.Port
		createReq.AllocationID = ""
		if usesBridgeNetworking(place.NetworkMode) {
			createReq.AllocationID = allocation.ID
		}
		srv, err = s.client.createServer(ctx, createReq)
	}
	if err != nil {
		// A create that failed before the panel acted is a clean failure: the ledger keeps the
		// intent, and the core may retry with the same key. A create the panel *refused* is a
		// business rejection, which retrying will not fix.
		if isRejection(err) {
			return reply{rpcErr: &rpcError{Code: codeRejected, Message: err.Error()}}
		}
		return reply{rpcErr: internalError(err)}
	}

	// 4. Record the outcome. A failure here is reported but not fatal: the server exists, and the
	// description marker is the fallback that keeps lookup honest.
	if err := s.ledger.put(operationRecord{
		OperationKey: p.OperationKey,
		ExternalID:   srv.ID,
		Name:         srv.Name,
		PlanID:       pl.ID,
		Region:       place.LocationName,
		State:        recordCreated,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "catalystctl: server %s was created but the ledger could not be updated: %v\n", srv.ID, err)
	}
	return reply{result: createResult(srv, pl.ID, place.LocationName)}
}

// claimAllocation takes the port this server will listen on.
//
// If the service asked for a specific port it gets that port or a refusal; otherwise the first free
// allocation on the node is taken. Either way the allocation id is passed to create, which is what
// stops two concurrent creates from racing for the same port: the panel reserves it.
func (s *server) claimAllocation(ctx context.Context, place placement) (panelAllocation, error) {
	if place.Port > 0 {
		return s.client.allocationOnPort(ctx, place.NodeID, place.Port)
	}
	used, err := s.client.portsInUse(ctx, place.NodeID)
	if err != nil {
		// Not being able to read the node's servers is not a reason to refuse a purchase: fall back to the
		// allocation table alone, which is what a panel with a broken list endpoint still answers.
		used = nil
	}
	return s.client.freeAllocation(ctx, place.NodeID, used)
}

// createResult renders the create reply.
func createResult(srv panelServer, planID, region string) map[string]any {
	return map[string]any{
		"external_id": srv.ID,
		"status":      statusFor(srv),
		"plan_id":     planID,
		"region":      region,
		"created_at":  srv.CreatedAt,
	}
}

// fields answers `provisioner.fields`: what this service must say about where its server goes.
//
// The answer is taken from the panel on every call, so the dropdowns an operator sees are the
// templates, locations and nodes that exist right now. That is the whole point of the method: a
// placement chosen from a list cannot be a typo, and a list kept in the billing panel would be stale
// the first time somebody added a node.
//
// It is a pure read: no panel state changes, and a panel that cannot be reached yields text fields
// rather than a failure, so an operator with a known id is never blocked by a listing outage.
func (s *server) fields(req *request) reply {
	ctx, done := contextFor(req)
	defer done()

	fields := s.declaredFields(ctx)
	out := make([]map[string]any, 0, len(fields))
	for _, f := range fields {
		entry := map[string]any{
			"key":      f.Key,
			"label":    f.Label,
			"type":     string(f.Type),
			"required": f.Required,
		}
		if len(f.Choices) > 0 {
			entry["choices"] = f.Choices
		}
		if f.Secret {
			entry["secret"] = true
		}
		if f.Hint != "" {
			entry["hint"] = f.Hint
		}
		out = append(out, entry)
	}
	return reply{result: map[string]any{"fields": out}}
}

// inspect reports one resource's protocol-level state.
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
	ctx, done := contextFor(req)
	defer done()
	srv, err := s.client.getServer(ctx, p.ExternalID)
	if err != nil {
		return reply{rpcErr: mapPanelError(err)}
	}
	rec, _, _ := s.ledger.byExternalID(p.ExternalID)
	out := map[string]any{
		"external_id": srv.ID,
		"status":      statusFor(srv),
		"created_at":  srv.CreatedAt,
	}
	if rec.PlanID != "" {
		out["plan_id"] = rec.PlanID
	}
	if rec.Region != "" {
		out["region"] = rec.Region
	}
	if srv.SuspendedAt != nil {
		out["suspended_at"] = *srv.SuspendedAt
	}
	return reply{result: out}
}

// lookup answers "does a resource already exist for this operation key?".
//
// This is the call reconciliation depends on, so the rules are strict:
//
//   - a ledger record with an id is confirmed against the panel before claiming found, because a
//     server somebody deleted by hand is not found;
//   - a record without an id (a create in flight) forces a scan;
//   - no record forces a scan by description marker;
//   - running out of pages is an error, never "not found".
func (s *server) lookup(req *request) reply {
	var p struct {
		OperationKey string `json:"operation_key"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "lookup params: " + err.Error()}}
	}
	if p.OperationKey == "" {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "operation_key is required"}}
	}
	ctx, done := contextFor(req)
	defer done()

	rec, found, err := s.ledger.get(p.OperationKey)
	if err != nil {
		return reply{rpcErr: internalError(err)}
	}
	if found && rec.ExternalID != "" {
		srv, err := s.client.getServer(ctx, rec.ExternalID)
		if err == nil {
			return reply{result: map[string]any{
				"found":       true,
				"external_id": srv.ID,
				"status":      string(statusFor(srv)),
			}}
		}
		if !isNotFound(err) {
			// "I could not ask" must never be reported as "it does not exist".
			return reply{rpcErr: internalError(err)}
		}
		// The panel is sure it is gone: the record is stale, and a create with this key is the
		// right repair rather than a second server.
	}

	// Scan by description marker.
	var match *panelServer
	err = s.client.listServers(ctx, func(srv panelServer) bool {
		if keyInDescription(srv.descriptionText(), p.OperationKey) {
			matched := srv
			match = &matched
			return true
		}
		return false
	})
	if err != nil {
		return reply{rpcErr: internalError(err)}
	}
	if match == nil {
		return reply{result: map[string]any{"found": false}}
	}
	// Re-record, so the next lookup takes the fast path.
	_ = s.ledger.put(operationRecord{
		OperationKey: p.OperationKey,
		ExternalID:   match.ID,
		Name:         match.Name,
		State:        recordCreated,
	})
	return reply{result: map[string]any{
		"found":       true,
		"external_id": match.ID,
		"status":      string(statusFor(*match)),
	}}
}

// suspend stops a server and marks it suspended.
func (s *server) suspend(req *request) reply {
	var p struct {
		OperationKey string `json:"operation_key"`
		ExternalID   string `json:"external_id"`
		Reason       string `json:"reason"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "suspend params: " + err.Error()}}
	}
	if p.ExternalID == "" {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "external_id is required"}}
	}
	ctx, done := contextFor(req)
	defer done()
	srv, err := s.client.getServer(ctx, p.ExternalID)
	if err != nil {
		return reply{rpcErr: mapPanelError(err)}
	}
	// Suspending an already-suspended server is success with changed=false: the requested end state
	// is what the caller asked for.
	if srv.SuspendedAt != nil {
		return reply{result: stateResult(srv.ID, string(statusFor(srv)), false)}
	}
	if err := s.client.suspend(ctx, p.ExternalID, p.Reason); err != nil {
		return reply{rpcErr: mapPanelError(err)}
	}
	return reply{result: stateResult(srv.ID, string(resourceSuspended), true)}
}

// resume restores a suspended server.
func (s *server) resume(req *request) reply {
	var p struct {
		OperationKey string `json:"operation_key"`
		ExternalID   string `json:"external_id"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "resume params: " + err.Error()}}
	}
	if p.ExternalID == "" {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "external_id is required"}}
	}
	ctx, done := contextFor(req)
	defer done()
	srv, err := s.client.getServer(ctx, p.ExternalID)
	if err != nil {
		return reply{rpcErr: mapPanelError(err)}
	}
	if srv.SuspendedAt == nil {
		return reply{result: stateResult(srv.ID, string(statusFor(srv)), false)}
	}
	if err := s.client.unsuspend(ctx, p.ExternalID); err != nil {
		return reply{rpcErr: mapPanelError(err)}
	}
	return reply{result: stateResult(srv.ID, string(resourceActive), true)}
}

// terminate destroys a server.
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
	ctx, done := contextFor(req)
	defer done()
	if err := s.client.deleteServer(ctx, p.ExternalID); err != nil {
		if isNotFound(err) {
			// Terminating an already-terminated resource succeeds: the end state is what was asked.
			s.markTerminated(p.ExternalID)
			return reply{result: stateResult(p.ExternalID, string(resourceTerminated), false)}
		}
		return reply{rpcErr: mapPanelError(err)}
	}
	s.markTerminated(p.ExternalID)
	return reply{result: stateResult(p.ExternalID, string(resourceTerminated), true)}
}

// markTerminated records a termination against every operation key that named the resource.
func (s *server) markTerminated(externalID string) {
	rec, ok, err := s.ledger.byExternalID(externalID)
	if err != nil || !ok {
		return
	}
	rec.State = recordTerminated
	rec.UpdatedAt = time.Now().UTC()
	_ = s.ledger.put(rec)
}

// resource answers the customer-facing read: what is my server doing right now?
//
// Every field is taken from the panel on this call. Nothing is cached and nothing is copied into
// the billing database, because the panel is the authority on a server's name, state and address,
// and a stale copy would be wrong the moment an operator renamed something.
func (s *server) resource(req *request) reply {
	var p struct {
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "resource params: " + err.Error()}}
	}
	if p.ExternalID == "" {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "external_id is required"}}
	}
	ctx, done := contextFor(req)
	defer done()

	srv, err := s.client.getServer(ctx, p.ExternalID)
	if err != nil {
		return reply{rpcErr: mapPanelError(err)}
	}
	host, port := srv.address()
	out := map[string]any{
		"external_id": srv.ID,
		"name":        srv.Name,
		"state":       srv.Status,
		"status":      string(statusFor(srv)),
		"observed_at": time.Now().UTC(),
		"can_power":   powerActionsFor(srv),
	}
	if host != "" {
		out["host"] = host
	}
	if port > 0 {
		out["port"] = port
	}
	if link := s.panelLink(srv.ID); link != "" {
		out["panel_url"] = link
	}
	if rec, ok, _ := s.ledger.byExternalID(srv.ID); ok {
		if rec.PlanID != "" {
			out["plan_id"] = rec.PlanID
		}
		if rec.Region != "" {
			out["region"] = rec.Region
		}
	}

	// Metrics are advisory: a stopped server has no sample and that is not a failure of the read.
	stats, ok, _ := s.client.stats(ctx, srv.ID)
	if ok {
		m := map[string]any{
			"cpu_percent":      stats.CPUPercent,
			"memory_used_mb":   stats.MemoryUsageMb.Int64(),
			"memory_limit_mb":  stats.MemoryAllocatedMb.Int64(),
			"disk_used_mb":     stats.DiskUsageMb.Int64(),
			"network_rx_bytes": stats.NetworkRxBytes.Int64(),
			"network_tx_bytes": stats.NetworkTxBytes.Int64(),
		}
		if srv.AllocatedDiskMb > 0 {
			m["disk_limit_mb"] = srv.AllocatedDiskMb
		}
		if stats.Timestamp != nil && *stats.Timestamp != "" {
			if at, err := time.Parse(time.RFC3339, *stats.Timestamp); err == nil {
				m["observed_at"] = at
			}
		}
		out["stats"] = m
	}
	return reply{result: out}
}

// power changes a server's power state.
//
// It is idempotent by operation key in two layers: the in-memory replay map answers a double-tap
// within one process, and the state check answers a replay after a restart. The state check is
// the one that matters, because the core may retry this call in a fresh process minutes later.
func (s *server) power(req *request) reply {
	var p struct {
		OperationKey string `json:"operation_key"`
		ExternalID   string `json:"external_id"`
		Action       string `json:"action"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "power params: " + err.Error()}}
	}
	if err := validateKey(p.OperationKey); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: err.Error()}}
	}
	if p.ExternalID == "" {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "external_id is required"}}
	}
	if !validPowerAction(p.Action) {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams,
			Message: fmt.Sprintf("unknown power action %q; the actions are start, stop, restart, kill", p.Action)}}
	}
	if prior, ok := s.byKey[p.OperationKey]; ok {
		return reply{result: prior}
	}

	ctx, done := contextFor(req)
	defer done()
	srv, err := s.client.getServer(ctx, p.ExternalID)
	if err != nil {
		return reply{rpcErr: mapPanelError(err)}
	}
	if srv.SuspendedAt != nil {
		// A suspended server is stopped by policy, not by the customer. Saying so is the honest
		// answer, and it is a rejection rather than a retryable failure.
		return reply{rpcErr: &rpcError{Code: codeRejected,
			Message: "this server is suspended, so its power state cannot be changed"}}
	}

	// Already in the requested state: report success without a second call. This is what makes a
	// replayed request safe.
	running := srv.Status == "running" || srv.Status == "starting"
	switch p.Action {
	case "start", "restart":
		if p.Action == "start" && running {
			return s.replayPower(p.OperationKey, srv.ID, p.Action, srv.Status, false)
		}
	case "stop", "kill":
		if !running {
			return s.replayPower(p.OperationKey, srv.ID, p.Action, srv.Status, false)
		}
	}

	if err := s.client.power(ctx, srv.ID, p.Action); err != nil {
		return reply{rpcErr: mapPanelError(err)}
	}

	// The panel accepts the action and changes state asynchronously, so the state reported back is
	// the one the action is heading towards rather than one that was observed.
	next := nextState(p.Action, srv.Status)
	out := map[string]any{
		"external_id": srv.ID,
		"action":      p.Action,
		"state":       next,
		"status":      string(statusForState(next)),
		"changed":     true,
	}
	s.byKey[p.OperationKey] = out
	return reply{result: out}
}

// replayPower answers a call that needed no work, recording it so a further replay is identical.
func (s *server) replayPower(key, externalID, action, state string, changed bool) reply {
	out := map[string]any{
		"external_id": externalID,
		"action":      action,
		"state":       state,
		"status":      string(statusForState(state)),
		"changed":     changed,
	}
	s.byKey[key] = out
	return reply{result: out}
}

// panelLink renders the deep link to a server in the panel's own UI.
//
// CCTL_PANEL_PATH makes it configurable because a panel may mount its UI under a prefix, and a
// button that 404s is worse than no button. `{id}` is substituted; a template without it is used
// verbatim, which is how an operator turns the link off entirely by pointing it at the panel root.
func (s *server) panelLink(serverID string) string {
	if s.cfg.panelURL == "" {
		return ""
	}
	path := s.cfg.panelPath
	if path == "" {
		path = "/servers/{id}"
	}
	if !strings.Contains(path, "{id}") {
		return s.cfg.panelURL + "/" + strings.TrimLeft(path, "/")
	}
	return s.cfg.panelURL + "/" + strings.TrimLeft(strings.ReplaceAll(path, "{id}", serverID), "/")
}

// serverName derives the panel's display name from the metadata a purchase carries.
func serverName(metadata map[string]string, subscriptionID string) string {
	for _, key := range []string{"name", "server_name", "label"} {
		if v := strings.TrimSpace(metadata[key]); v != "" {
			return truncate(v, 100)
		}
	}
	if subscriptionID != "" {
		return "Server " + truncate(subscriptionID, 8)
	}
	return "Game server"
}

// environmentFor turns a purchase's `env.`-prefixed metadata into the server's environment.
//
// This is the UPG-12 contract: a product's config options reach the adapter as metadata, and the
// prefix is how they are distinguished from the panel bookkeeping. Passing the bookkeeping through
// as environment variables would put a subscription id in a game server's process environment.
//
// Placement options are *consumed*, not forwarded. A product that declares a LOCATION config option
// is telling this adapter where to build the server, not asking for a `LOCATION` variable in the game
// process — and a game process full of `LOCATION`, `TEMPLATE` and `CPU_CORES` is both noise and, for
// a game that reads a variable of the same name, a genuine conflict.
func environmentFor(metadata map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range metadata {
		name, ok := strings.CutPrefix(key, "env.")
		if !ok || name == "" {
			continue
		}
		if isPlacementOption(name) {
			continue
		}
		out[name] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// portConflict reports whether the panel refused a create because the requested port is taken.
//
// It is checked by code because the alternative — a string match on the message — would break the moment the
// panel's wording changed, and this retry is the difference between a purchase that succeeds on the next port
// and one that fails for a reason nobody can act on.
func portConflict(err error) bool {
	var pe *panelError
	if !errors.As(err, &pe) {
		return false
	}
	switch pe.code {
	case "PORT_ALREADY_IN_USE", "SERVER_PORT_CONFLICT", "ALLOCATION_TAKEN":
		return true
	}
	return false
}
