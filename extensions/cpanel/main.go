// Command cpanel implements a cPanel/WHM hosting account as a billing
// provisioner extension.
//
// It speaks the same newline-delimited JSON-RPC 2.0 protocol as
// extensions/fakeprov and extensions/pterodactyl, and answers the same eight
// provisioner methods. The point of a second real adapter is to find out which
// parts of that protocol are provider-neutral and which parts are Pterodactyl's
// shape wearing a costume; every place where cPanel does not fit is marked
// "NOT PTERODACTYL-SHAPED" in a comment, and they are the real deliverable of this
// package.
//
// # The identifier: WHM has no external_id, and what we do instead
//
// Pterodactyl has an `external_id` column and can filter on it, so that adapter
// writes the operation key there and provisioner.lookup is one indexed query.
// WHM has no equivalent: json-api createacct takes no notes or metadata
// parameter, listaccts can only search on fields cPanel owns (user, domain, plan,
// ip), and an unknown parameter is silently ignored rather than rejected. The one
// field an account has that this adapter can write at creation and read back
// afterwards is the account *username*.
//
// So the operation key is recorded in the username:
//
//	username(key) = "c" + base36(sha256(key)[:8])
//
// which is lower case, starts with a letter and is at most 14 characters —
// cPanel's own rules for a name. The external_id this adapter returns is that
// username, and provisioner.lookup(key) is
//
//	listaccts?api.version=1&searchtype=user&search=<username(key)>
//
// Because the mapping is a pure function of the key, the lookup is authoritative
// with *no local state at all*: it still answers after this process, its machine
// or its disk is gone, as long as WHM is reachable. That is stronger than
// Pterodactyl's arrangement, not merely different, and it is the one design
// decision here that Pterodactyl cannot copy.
//
// # When the answer is genuinely unknown
//
// A lookup must never answer found=false unless it is certain, because the core
// provisions a second account when it believes none exists. This adapter therefore
// returns found=false only when WHM answered listaccts successfully and the
// account is not in the reply. Every other outcome is a JSON-RPC error (-32000),
// which the core reads as "unknown, ask again later":
//
//   - WHM unreachable, timing out, or answering 5xx;
//   - WHM rejecting the API token (401/403) — a configuration error, not absence;
//   - a reply that is not the json-api envelope (a proxy error page, a wrong
//     CPANEL_WHM_URL);
//   - metadata.result == 0 from listaccts, i.e. WHM refusing to answer at all.
//
// The one thing that would make the answer unknowable is a name the adapter
// cannot recompute from the key. metadata[cpanel_username] is therefore refused
// with an explanation instead of being accepted and quietly made unsafe: see
// pinnedUsername in names.go.
//
// # The operation ledger
//
// CPANEL_STATE_FILE holds a small JSON ledger of what this adapter has done:
// key -> username, domain, plan id, created_at, state. It is written *before*
// createacct is called, so a crash between the request and the reply still leaves
// a record of intent, and it is the only place the billing panel's plan id
// survives, because WHM knows quota and bandwidth limits rather than plan names.
//
// It is not the authority for lookup (WHM is), so losing it costs history rather
// than correctness. A create whose ledger write fails is refused before any
// request is sent, on the principle that an adapter which cannot write down what
// it is about to do must not do it.
//
// # Configuration
//
//	CPANEL_WHM_URL      required; e.g. https://whm.example.com:2087
//	CPANEL_API_TOKEN    required; a WHM API token, sent as `Authorization: whm root:TOKEN`
//	CPANEL_PLANS        optional; comma-separated `id:quota_mb:bandwidth_mb`
//	CPANEL_DEFAULT_PLAN optional; the plan to use when a call names none
//	CPANEL_TIMEOUT_MS   optional; per-call HTTP timeout, default 30000
//	CPANEL_VERIFY_TLS   optional; default on. Setting it off prints a warning
//	CPANEL_STATE_FILE   optional; the operation ledger, default
//	                    /var/lib/catalyst/cpanel/operations.json
//
// Every missing required value is named in one startup error, because a
// provisioner that starts without them fails every operation, and a queue full of
// failures looks like a broken worker rather than a missing setting.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// protocolVersion is the only protocol version this extension speaks.
const protocolVersion = 1

// maxInput bounds a request frame.
const maxInput = 4 << 20

// defaultStatePath is where the operation ledger lives when CPANEL_STATE_FILE is
// unset. It is a package-install location; a container deployment sets the
// variable to a mounted volume instead.
const defaultStatePath = "/var/lib/catalyst/cpanel/operations.json"

// JSON-RPC codes, matching the core's set so it maps them identically.
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
		fmt.Fprintf(os.Stderr, "cpanel: %v\n", err)
		os.Exit(2)
	}
	for _, warning := range cfg.warnings {
		fmt.Fprintf(os.Stderr, "cpanel: warning: %s\n", warning)
	}
	s := &server{client: newWHMClient(cfg), cfg: cfg, ops: newLedger(cfg.statePath)}
	if err := s.serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "cpanel: %v\n", err)
		os.Exit(1)
	}
}

// plan is one sellable account size, configured rather than discovered.
//
// NOT PTERODACTYL-SHAPED: Pterodactyl's plans had to be a bundle of memory, disk
// and CPU because those are the limits its egg model applies. cPanel's limits are
// a disk quota and a monthly bandwidth cap, and both are plain createacct
// parameters, so a plan here is nothing but those two numbers. There is also no
// nest/egg/node equivalent to discover or keep in sync with the panel.
type plan struct {
	ID string
	// QuotaMB is the account's disk quota in megabytes.
	QuotaMB int64
	// BandwidthMB is the monthly transfer cap in megabytes.
	BandwidthMB int64
}

// config is the extension's whole configuration, read once at startup.
type config struct {
	whmURL    string
	apiToken  string
	timeout   time.Duration
	verifyTLS bool
	statePath string
	plans     []plan
	plansByID map[string]plan
	// defaultPlan is used when a call names no plan. Empty means "only a call
	// that names a plan can be served".
	defaultPlan string
	// host is the WHM server's host name. cPanel has no region concept: an
	// account lives on exactly one server, so this is the only placement that can
	// be offered and the only one that can be honoured.
	host string
	// warnings are printed to stderr at startup. They are things the operator
	// should know that are not fatal.
	warnings []string
}

// configFromEnv reads and validates the configuration. Every missing required
// value is collected so one startup message names them all.
func configFromEnv(environ []string) (config, error) {
	env := make(map[string]string, len(environ))
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	cfg := config{
		whmURL:      strings.TrimRight(strings.TrimSpace(env["CPANEL_WHM_URL"]), "/"),
		apiToken:    strings.TrimSpace(env["CPANEL_API_TOKEN"]),
		timeout:     30 * time.Second,
		verifyTLS:   true,
		statePath:   strings.TrimSpace(env["CPANEL_STATE_FILE"]),
		plansByID:   map[string]plan{},
		defaultPlan: strings.TrimSpace(env["CPANEL_DEFAULT_PLAN"]),
	}
	if cfg.statePath == "" {
		cfg.statePath = defaultStatePath
	}

	var missing []string
	if cfg.whmURL == "" {
		missing = append(missing, "CPANEL_WHM_URL")
	}
	if cfg.apiToken == "" {
		missing = append(missing, "CPANEL_API_TOKEN")
	}
	if len(missing) > 0 {
		return config{}, fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}

	parsed, err := url.Parse(cfg.whmURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return config{}, fmt.Errorf("CPANEL_WHM_URL=%q is not an http(s) URL such as https://whm.example.com:2087", cfg.whmURL)
	}
	cfg.host = parsed.Hostname()

	if raw := strings.TrimSpace(env["CPANEL_TIMEOUT_MS"]); raw != "" {
		ms, err := strconv.Atoi(raw)
		if err != nil || ms <= 0 {
			return config{}, fmt.Errorf("CPANEL_TIMEOUT_MS=%q is not a positive integer", raw)
		}
		cfg.timeout = time.Duration(ms) * time.Millisecond
	}

	// CPANEL_VERIFY_TLS defaults to on. The only accepted "off" spellings are
	// explicit ones: a typo must not silently disable certificate checking.
	if raw, ok := env["CPANEL_VERIFY_TLS"]; ok && strings.TrimSpace(raw) != "" {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "0", "false", "no", "off":
			cfg.verifyTLS = false
			cfg.warnings = append(cfg.warnings, "CPANEL_VERIFY_TLS is off: WHM's TLS certificate is not being verified, "+
				"so the API token can be intercepted by anyone on the network path. Fix the certificate and unset it.")
		case "1", "true", "yes", "on":
			cfg.verifyTLS = true
		default:
			return config{}, fmt.Errorf("CPANEL_VERIFY_TLS=%q is not a boolean (use 1/0, true/false, on/off)", raw)
		}
	}

	plans, err := parsePlans(env["CPANEL_PLANS"])
	if err != nil {
		return config{}, err
	}
	cfg.plans = plans
	for _, p := range plans {
		cfg.plansByID[p.ID] = p
	}

	switch {
	case cfg.defaultPlan != "":
		if _, ok := cfg.plansByID[cfg.defaultPlan]; !ok {
			return config{}, fmt.Errorf("CPANEL_DEFAULT_PLAN=%q is not one of CPANEL_PLANS (%s)",
				cfg.defaultPlan, planIDs(plans))
		}
	case len(plans) == 1:
		// A single-plan installation needs no configuration to say which one it
		// means.
		cfg.defaultPlan = plans[0].ID
	}
	return cfg, nil
}

func planIDs(plans []plan) string {
	ids := make([]string, 0, len(plans))
	for _, p := range plans {
		ids = append(ids, p.ID)
	}
	return strings.Join(ids, ", ")
}

// parsePlans reads `id:quota_mb:bandwidth_mb,…`.
func parsePlans(raw string) ([]plan, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		// One default so a single-size installation needs no configuration, and a
		// multi-size one says so explicitly. 1 GB of disk and 10 GB of transfer is
		// a small hosting account, which is the honest default for an adapter that
		// was told nothing.
		return []plan{{ID: "default", QuotaMB: 1024, BandwidthMB: 10240}}, nil
	}
	var out []plan
	seen := map[string]struct{}{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) != 3 {
			return nil, fmt.Errorf("CPANEL_PLANS entry %q is not id:quota_mb:bandwidth_mb", entry)
		}
		p := plan{ID: strings.TrimSpace(parts[0])}
		if p.ID == "" {
			return nil, fmt.Errorf("CPANEL_PLANS entry %q has no id", entry)
		}
		if strings.ContainsAny(p.ID, " ,:") {
			return nil, fmt.Errorf("CPANEL_PLANS entry %q has an id containing a separator", entry)
		}
		var err error
		if p.QuotaMB, err = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64); err != nil || p.QuotaMB <= 0 {
			// A zero quota is cPanel's "unlimited", which is never what a billing
			// panel means by a plan, so it is refused rather than silently sold.
			return nil, fmt.Errorf("CPANEL_PLANS entry %q has an invalid quota (a positive number of megabytes is required)", entry)
		}
		if p.BandwidthMB, err = strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64); err != nil || p.BandwidthMB <= 0 {
			return nil, fmt.Errorf("CPANEL_PLANS entry %q has an invalid bandwidth (a positive number of megabytes is required)", entry)
		}
		if _, dup := seen[p.ID]; dup {
			return nil, fmt.Errorf("CPANEL_PLANS declares %q twice", p.ID)
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

// server is the extension's state. One goroutine drives it; the HTTP client and
// the ledger are the only things it shares.
type server struct {
	client *whmClient
	cfg    config
	ops    *ledger
}

// serve reads frames until stdin closes.
//
// NOT PTERODACTYL-SHAPED: this loop is synchronous, like Pterodactyl's, because
// WHM's json-api is a blocking call per account. A $/cancelRequest therefore
// cannot interrupt a call already in flight; it is dropped and the reply is
// written when the call returns, which the core tolerates by discarding replies
// for abandoned ids. What the loop does honour is the *deadline* the core puts in
// every payload: see ctxFor.
func (s *server) serve(in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64<<10), maxInput)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			return fmt.Errorf("parse error: %w", err)
		}
		if req.ID == nil {
			continue // a notification, e.g. $/cancelRequest
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
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return fmt.Errorf("request frame exceeds %d bytes", maxInput)
		}
		return err
	}
	return nil
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
	// SVC-16 / SVC-22: a plan change has to reach WHM, not just the ledger.
	"provisioner.upgrade",
	// UPG-12: the panel asks which per-purchase inputs this adapter needs.
	"provisioner.fields",
}

func (s *server) handle(req *request) reply {
	switch req.Method {
	case "initialize":
		return s.initialize(req)
	case "health":
		// A liveness probe, not a WHM probe: the supervisor calls this on every
		// start and a WHM outage must not stop the extension from being started,
		// or a temporary network problem would look like a broken install.
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
				"name":         "cpanel",
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

// ctxFor bounds one WHM call.
//
// The protocol stamps every payload with a deadline; honouring it means the
// adapter stops working at the same moment the core stops waiting, instead of
// finishing a createacct whose reply nobody will read. The configured timeout is
// the outer bound for a core that sends no deadline.
func (s *server) ctxFor(params json.RawMessage) (context.Context, context.CancelFunc) {
	var meta struct {
		Deadline time.Time `json:"deadline"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &meta)
	}
	if !meta.Deadline.IsZero() {
		if until := time.Until(meta.Deadline); until > 0 && until < s.cfg.timeout {
			return context.WithTimeout(context.Background(), until)
		}
	}
	return context.WithTimeout(context.Background(), s.cfg.timeout)
}

func (s *server) describe() map[string]any {
	plans := make([]map[string]any, 0, len(s.cfg.plans))
	for _, p := range s.cfg.plans {
		plans = append(plans, map[string]any{
			"id":     p.ID,
			"name":   p.ID,
			"amount": map[string]any{"minor": 0, "currency": "USD"},
			// What the plan costs is the billing panel's business; this adapter
			// reports the shape of what it can build.
			"period": "monthly",
			"limits": map[string]any{
				"quota_mb":     p.QuotaMB,
				"bandwidth_mb": p.BandwidthMB,
			},
		})
	}
	// NOT PTERODACTYL-SHAPED: Pterodactyl has nodes, and a node is effectively a
	// region. A WHM server is one machine with no placement to choose, so the
	// region list is the server itself — a region naming anything else cannot be
	// honoured and is refused rather than silently ignored.
	regions := []string{}
	if s.cfg.host != "" {
		regions = append(regions, s.cfg.host)
	}
	return map[string]any{
		"name":    "cpanel",
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

// resolvePlan applies CPANEL_DEFAULT_PLAN.
func (s *server) resolvePlan(id string) (plan, bool) {
	if strings.TrimSpace(id) == "" {
		id = s.cfg.defaultPlan
	}
	p, ok := s.cfg.plansByID[id]
	return p, ok
}

// problemsFor is the single place that decides whether a purchase is buildable.
// create calls it too, so validate and create cannot disagree about what is legal.
func (s *server) problemsFor(planID, region string, quantity int32) []string {
	var problems []string
	if _, ok := s.resolvePlan(planID); !ok {
		if strings.TrimSpace(planID) == "" {
			problems = append(problems, fmt.Sprintf("no plan named and no CPANEL_DEFAULT_PLAN is configured (known plans: %s)", planIDs(s.cfg.plans)))
		} else {
			problems = append(problems, fmt.Sprintf("unknown plan %q (known plans: %s)", planID, planIDs(s.cfg.plans)))
		}
	}
	switch {
	case quantity < 0:
		problems = append(problems, fmt.Sprintf("quantity %d is negative", quantity))
	case quantity > 1:
		// NOT PTERODACTYL-SHAPED: the protocol's create result is a single
		// external_id, so a quantity above one has nowhere to go. Pterodactyl read
		// quantity as a size multiplier; a cPanel account is one account per
		// service, and selling "two of them" would need the core to create two
		// services. Refusing is honest; silently creating one would under-deliver.
		problems = append(problems, fmt.Sprintf("quantity %d cannot be served: one cPanel account holds one service", quantity))
	}
	if region := strings.TrimSpace(region); region != "" && region != s.cfg.host {
		problems = append(problems, fmt.Sprintf("region %q cannot be served: this adapter manages the single WHM server %q", region, s.cfg.host))
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
	pl, _ := s.resolvePlan(p.PlanID)

	// A replay must return the account that already exists. The ledger is what
	// remembers which name and plan the first attempt used, so a replay with
	// different metadata still resolves to the same account instead of asking WHM
	// to create a second one.
	rec, recorded, err := s.ops.get(p.OperationKey)
	if err != nil {
		return reply{rpcErr: &rpcError{Code: codeInternal, Message: err.Error()}}
	}
	username, domain := rec.Username, rec.Domain
	if !recorded {
		username, err = accountName(p.OperationKey, p.Metadata)
		if err != nil {
			// Refused locally, before any request: WHM's own answer to a bad
			// username is a refusal sentence the core would retry forever.
			return reply{rpcErr: &rpcError{
				Code:    codeRejected,
				Message: err.Error(),
				Data:    map[string]any{"problems": []string{err.Error()}},
			}}
		}
		if domain, err = domainFor(p.Metadata); err != nil {
			return reply{rpcErr: &rpcError{
				Code:    codeRejected,
				Message: err.Error(),
				Data:    map[string]any{"problems": []string{err.Error()}},
			}}
		}
	}

	ctx, cancel := s.ctxFor(req.Params)
	defer cancel()

	// The authoritative question: does this account exist? It is answered from WHM
	// by name, so it works for an account this process never created.
	existing, err := s.client.findAccount(ctx, username)
	if err != nil {
		return whmErrorReply(err)
	}
	if existing != nil {
		if err := s.ops.put(operationRecord{
			OperationKey: p.OperationKey, Username: existing.User, Domain: existing.Domain,
			PlanID: firstNonEmpty(rec.PlanID, pl.ID), State: ledgerCreated,
		}); err != nil {
			return reply{rpcErr: &rpcError{Code: codeInternal, Message: err.Error()}}
		}
		return reply{result: s.createResult(existing, p.OperationKey, firstNonEmpty(rec.PlanID, pl.ID), p.Region)}
	}

	// Write-ahead: the intent is recorded before the account is created, so a
	// crash between the request and the reply leaves a record rather than an
	// orphan. If this fails, nothing is sent.
	if err := s.ops.put(operationRecord{
		OperationKey: p.OperationKey, Username: username, Domain: domain,
		PlanID: pl.ID, State: ledgerPending,
	}); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInternal, Message: err.Error()}}
	}

	err = s.client.createAccount(ctx, createAccountRequest{
		Username:     username,
		Domain:       domain,
		QuotaMB:      pl.QuotaMB,
		BandwidthMB:  pl.BandwidthMB,
		ContactEmail: contactEmailFor(p.Metadata),
	})
	if err != nil {
		// Two workers can race on the same key: the other one created the account
		// between our lookup and our createacct, and WHM refused because the name is
		// taken. Adopting the account is correct — the name is a function of this
		// key, so the account is this operation's account.
		if again, ferr := s.client.findAccount(ctx, username); ferr == nil && again != nil {
			if perr := s.ops.put(operationRecord{
				OperationKey: p.OperationKey, Username: again.User, Domain: again.Domain,
				PlanID: pl.ID, State: ledgerCreated,
			}); perr != nil {
				return reply{rpcErr: &rpcError{Code: codeInternal, Message: perr.Error()}}
			}
			return reply{result: s.createResult(again, p.OperationKey, pl.ID, p.Region)}
		}
		return whmErrorReply(err)
	}

	// Read the account back rather than trusting the createacct reply: it carries
	// no state, and a later suspend between our create and our answer must be
	// reported as it is.
	created, err := s.client.findAccount(ctx, username)
	if err != nil {
		return whmErrorReply(err)
	}
	if created == nil {
		return reply{rpcErr: &rpcError{
			Code:    codeInternal,
			Message: fmt.Sprintf("cPanel reported that it created %s but listaccts does not show it; refusing to report success for an account that cannot be found", username),
		}}
	}
	if err := s.ops.put(operationRecord{
		OperationKey: p.OperationKey, Username: created.User, Domain: created.Domain,
		PlanID: pl.ID, State: ledgerCreated,
	}); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInternal, Message: err.Error()}}
	}
	return reply{result: s.createResult(created, p.OperationKey, pl.ID, p.Region)}
}

func (s *server) createResult(acct *whmAccount, operationKey, planID, region string) map[string]any {
	out := map[string]any{
		"external_id": acct.User,
		"status":      protocolStatus(acct),
		"plan_id":     planID,
		// The account lives on the one WHM server this adapter manages, so an
		// unnamed region is reported as that server rather than as "".
		"region": firstNonEmpty(region, s.cfg.host),
	}
	if created := s.createdAtFor(acct, operationKey); !created.IsZero() {
		out["created_at"] = created
	}
	return out
}

// createdAtFor prefers the ledger's timestamp (to the second, and written by us)
// over WHM's startdate (a date, and the server's clock).
func (s *server) createdAtFor(acct *whmAccount, operationKey string) time.Time {
	if rec, ok, err := s.ops.get(operationKey); err == nil && ok && !rec.CreatedAt.IsZero() {
		return rec.CreatedAt
	}
	return acct.createdAt()
}

// protocolStatus maps a cPanel account onto the protocol's lifecycle states.
//
// cPanel has exactly two states for an account that exists. "provisioning" never
// appears because createacct is synchronous: by the time it answers, the account
// is there. "terminated" is only ever reported by terminate, never observed,
// because a removed account cannot be inspected.
func protocolStatus(acct *whmAccount) string {
	if acct.isSuspended() {
		return "suspended"
	}
	return "active"
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

	username := usernameForKey(p.OperationKey)
	if rec, ok, err := s.ops.get(p.OperationKey); err == nil && ok && rec.Username != "" {
		if rec.Username != username {
			// Only reachable if the ledger was written by a different derivation.
			// Answering with either name would be a guess, and a guess here is a
			// duplicate account.
			return reply{rpcErr: &rpcError{Code: codeInternal, Message: fmt.Sprintf(
				"the operation ledger records account %q for this key but the key derives %q; refusing to guess which account is authoritative",
				rec.Username, username)}}
		}
	}
	// A ledger that cannot be read does not stop this answer: WHM plus the
	// derivation is the authority, and the ledger is only history.

	ctx, cancel := s.ctxFor(req.Params)
	defer cancel()

	acct, err := s.client.findAccount(ctx, username)
	if err != nil {
		// An adapter that cannot answer must return an error. found=false here is
		// the single mistake this whole package exists to avoid: the core would
		// provision a second account.
		return whmErrorReply(err)
	}
	if acct == nil {
		return reply{result: map[string]any{"found": false}}
	}
	return reply{result: map[string]any{
		"found":       true,
		"external_id": acct.User,
		"status":      protocolStatus(acct),
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
	ctx, cancel := s.ctxFor(req.Params)
	defer cancel()

	acct, err := s.client.findAccount(ctx, p.ExternalID)
	if err != nil {
		return whmErrorReply(err)
	}
	if acct == nil {
		return reply{rpcErr: &rpcError{Code: codeNotFound,
			Message: "cPanel has no account named " + p.ExternalID,
			Data:    map[string]any{"external_id": p.ExternalID}}}
	}

	out := map[string]any{
		"external_id": acct.User,
		"status":      protocolStatus(acct),
		// WHM cannot tell us the plan id, so it comes from the ledger; an
		// unreadable ledger reports "" rather than failing the inspect, because a
		// suspension of an abusive customer must not wait for an operator to
		// repair a file.
		"plan_id": s.planIDFor(acct.User),
		// NOT PTERODACTYL-SHAPED: WHM has no node or region per account, so the
		// region is the server.
		"region": s.cfg.host,
	}
	if created := acct.createdAt(); !created.IsZero() {
		out["created_at"] = created
	}
	if acct.isSuspended() {
		// WHM does not record when an account was suspended, so the timestamp is
		// the observation time. The core only uses it to display "suspended since",
		// and an observation is the honest value it can have.
		out["suspended_at"] = time.Now().UTC()
	}
	return reply{result: out}
}

func (s *server) planIDFor(username string) string {
	rec, ok, err := s.ops.byUsername(username)
	if err != nil || !ok {
		return ""
	}
	return rec.PlanID
}

// setState handles suspend and resume, which differ only in the WHM function.
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
	ctx, cancel := s.ctxFor(req.Params)
	defer cancel()

	acct, err := s.client.findAccount(ctx, p.ExternalID)
	if err != nil {
		return whmErrorReply(err)
	}
	if acct == nil {
		// A suspended or resumed account that does not exist is not something the
		// adapter can decide: the core has to learn it is gone and reconcile. A
		// "success" here would hide a deleted account behind a green queue.
		return reply{rpcErr: &rpcError{Code: codeNotFound,
			Message: "cPanel has no account named " + p.ExternalID,
			Data:    map[string]any{"external_id": p.ExternalID}}}
	}

	// Asking for the state the account is already in is a success with
	// changed=false. The core's suspend is idempotent, and a second call must not
	// look like a failure — WHM's own answer to suspending a suspended account is
	// result:0, which would otherwise become a -32005 rejection.
	if kind == "suspend" {
		if acct.isSuspended() {
			return reply{result: map[string]any{
				"external_id": acct.User, "status": "suspended", "changed": false,
			}}
		}
		if err := s.client.suspendAccount(ctx, acct.User, p.Reason); err != nil {
			return whmErrorReply(err)
		}
		return reply{result: map[string]any{
			"external_id": acct.User, "status": "suspended", "changed": true,
		}}
	}
	if !acct.isSuspended() {
		return reply{result: map[string]any{
			"external_id": acct.User, "status": "active", "changed": false,
		}}
	}
	if err := s.client.unsuspendAccount(ctx, acct.User); err != nil {
		return whmErrorReply(err)
	}
	return reply{result: map[string]any{
		"external_id": acct.User, "status": "active", "changed": true,
	}}
}

// fields is what this adapter needs to create an account (UPG-12).
//
// A cPanel account is a domain plus a username. The adapter derived the username from the
// operation key long before the protocol could declare fields (it is the only way lookup can
// survive a restart), but the *domain* is a fact only the customer has, and it arrived through a
// config option purely by convention. Declaring it makes the requirement visible to the purchase
// form instead of depending on an operator remembering to add the option.
func (s *server) fields(*request) reply {
	return reply{result: map[string]any{"fields": []map[string]any{
		{
			"key":      "domain",
			"label":    "Domain",
			"type":     "text",
			"required": true,
			"hint":     "The bare hostname this account hosts, e.g. example.com. No scheme or path.",
		},
		{
			"key":   "cpanel_contact_email",
			"label": "Contact email",
			"type":  "text",
			"hint":  "Optional. WHM stores it on the account for its own notices.",
		},
	}}}
}

// upgrade moves an account to another plan (SVC-16, SVC-22, UPG-05).
//
// NOT PTERODACTYL-SHAPED: there is nothing to rebuild. WHM's modifyacct sets the quota and the
// bandwidth limit on an existing account, in place, and the files, databases and mailboxes are
// untouched — which is what a hosting customer expects from an upgrade.
//
// It reports changed=true after a successful call. WHM answers modifyacct with a result flag and
// nothing about the previous limits, so this adapter cannot distinguish a first resize from a
// replay; the call itself is idempotent (it sets the values it is given), so a replay writes the
// same numbers rather than compounding anything.
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
	pl, ok := s.resolvePlan(p.PlanID)
	if !ok {
		msg := fmt.Sprintf("unknown plan %q (known plans: %s)", p.PlanID, planIDs(s.cfg.plans))
		return reply{rpcErr: &rpcError{
			Code: codeRejected, Message: msg, Data: map[string]any{"problems": []string{msg}},
		}}
	}
	ctx, cancel := s.ctxFor(req.Params)
	defer cancel()

	acct, err := s.client.findAccount(ctx, p.ExternalID)
	if err != nil {
		return whmErrorReply(err)
	}
	if acct == nil {
		// Resizing an account that is not there is not something this adapter can decide: the
		// core has to learn it is gone. Reporting success would leave a paid customer with a
		// service the panel believes is provisioned.
		return reply{rpcErr: &rpcError{Code: codeNotFound,
			Message: "cPanel has no account named " + p.ExternalID,
			Data:    map[string]any{"external_id": p.ExternalID}}}
	}
	if err := s.client.modifyAccount(ctx, acct.User, pl); err != nil {
		return whmErrorReply(err)
	}
	// The ledger is the only place the billing panel's plan id survives (WHM knows quota, not
	// plan names), so it is updated here rather than left describing the size the account used
	// to be.
	if rec, ok, err := s.ops.get(p.OperationKey); err == nil && ok {
		rec.PlanID = pl.ID
		_ = s.ops.put(rec)
	}
	return reply{result: map[string]any{
		"external_id": acct.User,
		"status":      protocolStatus(acct),
		"plan_id":     pl.ID,
		"region":      firstNonEmpty(p.Region, s.cfg.host),
		"changed":     true,
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
	ctx, cancel := s.ctxFor(req.Params)
	defer cancel()

	acct, err := s.client.findAccount(ctx, p.ExternalID)
	if err != nil {
		return whmErrorReply(err)
	}
	if acct == nil {
		// Terminating an account WHM does not have is the requested end state.
		// Answering with an error would put the operation back in the queue forever.
		s.recordTerminated(p.ExternalID)
		return reply{result: map[string]any{
			"external_id": p.ExternalID, "status": "terminated", "changed": false,
		}}
	}
	if err := s.client.removeAccount(ctx, acct.User); err != nil {
		return whmErrorReply(err)
	}
	s.recordTerminated(acct.User)
	return reply{result: map[string]any{
		"external_id": acct.User, "status": "terminated", "changed": true,
	}}
}

// recordTerminated marks the ledger record for an account as terminated. It is
// best effort: the account is already gone from WHM, and failing the call because
// history could not be updated would make the core retry a termination that has
// already happened.
func (s *server) recordTerminated(username string) {
	rec, ok, err := s.ops.byUsername(username)
	if err != nil || !ok {
		return
	}
	rec.State = ledgerTerminated
	_ = s.ops.put(rec)
}

// whmErrorReply maps an adapter error onto a JSON-RPC error, preserving the
// distinction the core branches on: rejected on the merits, or broken.
func whmErrorReply(err error) reply {
	var we *whmError
	if errors.As(err, &we) {
		if we.refused {
			// metadata.result == 0 is WHM understanding the request and saying no.
			// It is a business answer, not a transport failure, and retrying it
			// unchanged will not help.
			return reply{rpcErr: &rpcError{Code: codeRejected, Message: we.Error(), Data: we.data()}}
		}
		// Everything else is "we could not get an answer": a transport failure, a
		// 5xx, an unusable body, a rejected token. None of these is not-found.
		return reply{rpcErr: &rpcError{Code: codeInternal, Message: we.Error(), Data: we.data()}}
	}
	return reply{rpcErr: &rpcError{Code: codeInternal, Message: err.Error()}}
}
