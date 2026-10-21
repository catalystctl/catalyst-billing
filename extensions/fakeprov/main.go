// Command fakeprov is a conformance fixture for the billing extension protocol:
// an in-memory provisioner that speaks newline-delimited JSON-RPC 2.0 on
// stdin/stdout.
//
// It models the resource lifecycle (create, inspect, suspend, resume, terminate)
// and, more importantly, the two failure modes that decide whether a billing
// integration is safe: a create whose reply is lost after the resource already
// exists, and an extension that ignores the operation key and provisions a
// second resource on retry.
//
// # Programming the fixture
//
//	FAKE_PROV_MODE=ok|fail|hang|oversized|crash|slow|create_then_timeout|duplicate
//	FAKE_PROV_SLOW_MS=<n>       delay for slow mode (default 500)
//	FAKE_PROV_VERSION=<n>       reply with this protocol version instead of 1
//	FAKE_PROV_NEGOTIATE=fail    refuse version negotiation
//	FAKE_PROV_PIDFILE=<path>    write the process id at startup
//	FAKE_PROV_STDERR=<text>     write this text to stderr at startup
//
// create_then_timeout provisions the resource on the first create and then never
// answers, so a retry with the same operation key must discover the existing
// resource rather than create a second one. duplicate is the negative fixture: it
// ignores the operation key entirely, so a retry produces a second external id.
//
// The failure mode applies to every method except "initialize", so the handshake
// always succeeds and the failure happens on the call under test.
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

// protocolVersion is the only protocol version this fixture speaks.
const protocolVersion = 1

// maxInput bounds a request frame.
const maxInput = 4 << 20

// JSON-RPC codes used by the fixture.
const (
	codeParseError      = -32700
	codeInvalidRequest  = -32600
	codeMethodNotFound  = -32601
	codeInvalidParams   = -32602
	codeVersionMismatch = -32001
	codeNotFound        = -32004
	codeRejected        = -32005
)

func main() {
	writePIDFile(os.Getenv("FAKE_PROV_PIDFILE"))
	if msg := os.Getenv("FAKE_PROV_STDERR"); msg != "" {
		fmt.Fprint(os.Stderr, msg)
	}
	s := &server{
		mode:      envOr("FAKE_PROV_MODE", "ok"),
		slow:      envDuration("FAKE_PROV_SLOW_MS", 500*time.Millisecond),
		version:   envInt("FAKE_PROV_VERSION", protocolVersion),
		negotiate: os.Getenv("FAKE_PROV_NEGOTIATE"),
		resources: make(map[string]*resource),
		byKey:     make(map[string]string),
		ops:       make(map[string]stateResult),
	}
	if err := s.serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "fakeprov: %v\n", err)
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

// reply is what one handled request produces. silent drops the response while
// keeping the process alive and responsive, which is how a lost reply looks from
// the core's side.
type reply struct {
	result any
	rpcErr *rpcError
	raw    []byte
	silent bool
}

// amount is the wire form of money: exact minor units plus the currency code.
type amount struct {
	Minor    int64  `json:"minor"`
	Currency string `json:"currency"`
}

// resource is one provisioned server.
type resource struct {
	ExternalID     string    `json:"external_id"`
	Status         string    `json:"status"`
	PlanID         string    `json:"plan_id"`
	Region         string    `json:"region"`
	Quantity       int32     `json:"quantity"`
	SubscriptionID string    `json:"subscription_id"`
	CreatedAt      time.Time `json:"created_at"`
	SuspendedAt    time.Time `json:"suspended_at,omitzero"`
	// OperationKey is recorded on the resource itself, so the key survives a
	// process restart and lookup can answer authoritatively.
	OperationKey string `json:"operation_key"`
}

// stateResult is the answer to suspend, resume and terminate.
type stateResult struct {
	ExternalID string `json:"external_id"`
	Status     string `json:"status"`
	Changed    bool   `json:"changed"`
}

// server is the fixture's whole state. It is driven by a single goroutine.
type server struct {
	mode      string
	slow      time.Duration
	version   int
	negotiate string

	seq       int
	resources map[string]*resource
	byKey     map[string]string
	ops       map[string]stateResult
	creates   int
}

var capabilities = []string{
	"provisioner.describe",
	"provisioner.validate",
	"provisioner.create",
	"provisioner.inspect",
	"provisioner.suspend",
	"provisioner.resume",
	"provisioner.terminate",
}

// plans and regions are the fixture's catalogue.
var (
	plans = []struct {
		ID     string
		Name   string
		Amount amount
		Period string
	}{
		{"small", "Small", amount{Minor: 500, Currency: "USD"}, "monthly"},
		{"medium", "Medium", amount{Minor: 2000, Currency: "USD"}, "monthly"},
	}
	regions = []string{"eu-west", "us-east"}
)

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
			continue
		}
		if req.JSONRPC != "2.0" {
			if err := enc.Encode(response{JSONRPC: "2.0", ID: *req.ID, Error: &rpcError{Code: codeInvalidRequest, Message: "jsonrpc must be 2.0"}}); err != nil {
				return err
			}
			continue
		}
		rep := s.handle(&req)
		if rep.silent {
			continue
		}
		if rep.raw != nil {
			if _, err := out.Write(rep.raw); err != nil {
				return err
			}
			continue
		}
		if err := enc.Encode(response{JSONRPC: "2.0", ID: *req.ID, Result: rep.result, Error: rep.rpcErr}); err != nil {
			return err
		}
	}
}

func (s *server) handle(req *request) reply {
	if req.Method == "initialize" {
		return s.initialize(req)
	}
	switch s.mode {
	case "crash":
		os.Exit(3)
	case "hang":
		hang()
	case "fail":
		return reply{rpcErr: &rpcError{
			Code:    -32000,
			Message: "fake provisioner failure",
			Data:    map[string]string{"mode": s.mode},
		}}
	case "oversized":
		return reply{raw: oversizedFrame(*req.ID)}
	case "slow":
		time.Sleep(s.slow)
	}

	switch req.Method {
	case "health":
		return reply{result: map[string]string{"status": "ok"}}
	case "fixture.echo":
		if len(req.Params) == 0 {
			return reply{result: json.RawMessage("null")}
		}
		return reply{result: json.RawMessage(req.Params)}
	case "fixture.env":
		vars := make(map[string]string)
		for _, kv := range os.Environ() {
			if k, v, ok := strings.Cut(kv, "="); ok {
				vars[k] = v
			}
		}
		return reply{result: map[string]any{"vars": vars}}
	case "provisioner.describe":
		return reply{result: s.describe()}
	case "provisioner.validate":
		return s.validate(req)
	case "provisioner.create":
		return s.create(req)
	case "provisioner.inspect":
		return s.inspect(req)
	case "provisioner.lookup":
		return s.lookup(req)
	case "provisioner.suspend":
		return s.transition(req, "suspended")
	case "provisioner.resume":
		return s.transition(req, "active")
	case "provisioner.terminate":
		return s.transition(req, "terminated")
	default:
		return reply{rpcErr: &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}}
	}
}

func (s *server) initialize(req *request) reply {
	var p struct {
		Protocol    string `json:"protocol"`
		Versions    []int  `json:"versions"`
		CoreVersion string `json:"core_version"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "initialize params: " + err.Error()}}
	}
	if s.negotiate == "fail" {
		return reply{rpcErr: &rpcError{
			Code:    codeVersionMismatch,
			Message: "no common protocol version",
			Data:    map[string]any{"supported": []int{protocolVersion}, "offered": p.Versions},
		}}
	}
	chosen := 0
	if s.version != protocolVersion {
		chosen = s.version
	} else {
		for _, v := range p.Versions {
			if v == protocolVersion {
				chosen = v
				break
			}
		}
	}
	if chosen == 0 {
		return reply{rpcErr: &rpcError{
			Code:    codeVersionMismatch,
			Message: "no common protocol version",
			Data:    map[string]any{"supported": []int{protocolVersion}, "offered": p.Versions},
		}}
	}
	return reply{result: map[string]any{
		"version":      chosen,
		"name":         "fakeprov",
		"publisher":    "catalyst-fixtures",
		"capabilities": capabilities,
	}}
}

func (s *server) describe() map[string]any {
	out := make([]map[string]any, 0, len(plans))
	for _, p := range plans {
		out = append(out, map[string]any{
			"id":     p.ID,
			"name":   p.Name,
			"amount": p.Amount,
			"period": p.Period,
		})
	}
	return map[string]any{
		"name":    "fakeprov",
		"plans":   out,
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
	var problems []string
	plan, ok := planByID(p.PlanID)
	if !ok {
		problems = append(problems, "unknown plan "+p.PlanID)
	}
	if !regionKnown(p.Region) {
		problems = append(problems, "unknown region "+p.Region)
	}
	if p.Quantity < 1 || p.Quantity > 100 {
		problems = append(problems, fmt.Sprintf("quantity %d is outside 1..100", p.Quantity))
	}
	res := map[string]any{"valid": len(problems) == 0}
	if len(problems) > 0 {
		res["problems"] = problems
	}
	if ok {
		// Quantity is priced in whole units of the plan amount, in integers.
		res["amount"] = amount{Minor: plan.Amount.Minor * int64(p.Quantity), Currency: plan.Amount.Currency}
	}
	return reply{result: res}
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
	if p.OperationKey == "" {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "operation_key is required"}}
	}
	if _, ok := planByID(p.PlanID); !ok {
		return reply{rpcErr: &rpcError{Code: codeRejected, Message: "unknown plan " + p.PlanID}}
	}
	if !regionKnown(p.Region) {
		return reply{rpcErr: &rpcError{Code: codeRejected, Message: "unknown region " + p.Region}}
	}
	if p.Quantity < 1 {
		return reply{rpcErr: &rpcError{Code: codeRejected, Message: "quantity must be at least 1"}}
	}

	// duplicate is the negative fixture: it deliberately ignores the operation
	// key, so a retry provisions a second server. The core's idempotency tests
	// assert that the honest fixture does *not* do this.
	if s.mode == "duplicate" {
		res := s.newResource(p.OperationKey, p.PlanID, p.Region, p.Quantity, p.SubscriptionID)
		return reply{result: res}
	}
	if id, ok := s.byKey[p.OperationKey]; ok {
		return reply{result: s.resources[id]}
	}

	// create_then_timeout provisions the resource and then swallows the reply, so
	// the caller's retry has to discover the resource it never saw confirmed. The
	// process stays alive and responsive, because a real gateway that lost a reply
	// does not stop serving.
	if s.mode == "create_then_timeout" && s.creates == 0 {
		s.creates++
		_ = s.newResource(p.OperationKey, p.PlanID, p.Region, p.Quantity, p.SubscriptionID)
		return reply{silent: true}
	}
	return reply{result: s.newResource(p.OperationKey, p.PlanID, p.Region, p.Quantity, p.SubscriptionID)}
}

// newResource allocates a resource and records it under the operation key.
// lookupResult is the answer to provisioner.lookup. The fixture does not import
// the core module: it speaks the wire format only, which is what keeps the
// protocol the contract rather than the Go types.
type lookupResult struct {
	Found      bool   `json:"found"`
	ExternalID string `json:"external_id,omitempty"`
	Status     string `json:"status,omitempty"`
}

// lookup answers "does a resource already exist for this operation key?".
//
// This is the authoritative reconciliation path. It searches the *backend* (every
// resource this fixture has ever created in this process), not the in-memory
// idempotency map, because the whole point of reconciliation is to survive a
// restart that cleared that map.
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

	// Fast path: this process still remembers the key.
	if id, ok := s.byKey[p.OperationKey]; ok {
		if res, ok := s.resources[id]; ok {
			s.byKey[p.OperationKey] = id
			return reply{result: lookupResult{Found: true, ExternalID: res.ExternalID, Status: res.Status}}
		}
	}
	// Authoritative path: the backend records the operation key on the resource
	// itself, so a restart cannot lose it.
	for _, res := range s.resources {
		if res.OperationKey == p.OperationKey {
			s.byKey[p.OperationKey] = res.ExternalID
			return reply{result: lookupResult{Found: true, ExternalID: res.ExternalID, Status: res.Status}}
		}
	}
	return reply{result: lookupResult{Found: false}}
}

func (s *server) newResource(key, planID, region string, quantity int32, subscriptionID string) *resource {
	s.seq++
	res := &resource{
		ExternalID:     fmt.Sprintf("srv_%d", s.seq),
		Status:         "active",
		PlanID:         planID,
		Region:         region,
		Quantity:       quantity,
		SubscriptionID: subscriptionID,
		CreatedAt:      time.Now().UTC(),
		OperationKey:   key,
	}
	s.resources[res.ExternalID] = res
	s.byKey[key] = res.ExternalID
	return res
}

func (s *server) inspect(req *request) reply {
	var p struct {
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "inspect params: " + err.Error()}}
	}
	res, ok := s.resources[p.ExternalID]
	if !ok {
		return reply{rpcErr: &rpcError{Code: codeNotFound, Message: "unknown resource " + p.ExternalID}}
	}
	return reply{result: res}
}

// transition applies suspend/resume/terminate. It is idempotent by operation key
// and by target state, so replaying a lost call is safe.
func (s *server) transition(req *request, target string) reply {
	var p struct {
		OperationKey string `json:"operation_key"`
		ExternalID   string `json:"external_id"`
		Reason       string `json:"reason"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: req.Method + " params: " + err.Error()}}
	}
	if p.OperationKey == "" {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "operation_key is required"}}
	}
	opKey := req.Method + "|" + p.OperationKey
	if prev, ok := s.ops[opKey]; ok {
		prev.Changed = false
		return reply{result: prev}
	}
	res, ok := s.resources[p.ExternalID]
	if !ok {
		return reply{rpcErr: &rpcError{Code: codeNotFound, Message: "unknown resource " + p.ExternalID}}
	}
	out := stateResult{ExternalID: res.ExternalID, Status: target}
	if res.Status == target {
		out.Changed = false
		s.ops[opKey] = out
		return reply{result: out}
	}
	if res.Status == "terminated" && target != "terminated" {
		return reply{rpcErr: &rpcError{Code: codeRejected, Message: "resource is terminated"}}
	}
	res.Status = target
	if target == "suspended" {
		res.SuspendedAt = time.Now().UTC()
	} else {
		res.SuspendedAt = time.Time{}
	}
	out.Changed = true
	s.ops[opKey] = out
	return reply{result: out}
}

func planByID(id string) (struct {
	ID     string
	Name   string
	Amount amount
	Period string
}, bool) {
	for _, p := range plans {
		if p.ID == id {
			return p, true
		}
	}
	return struct {
		ID     string
		Name   string
		Amount amount
		Period string
	}{}, false
}

func regionKnown(r string) bool {
	for _, known := range regions {
		if known == r {
			return true
		}
	}
	return false
}

// writePIDFile records the fixture's pid for tests that assert on process
// cleanup.
func writePIDFile(path string) {
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "fakeprov: writing pid file: %v\n", err)
	}
}

// oversizedFrame builds a frame larger than the core's default 1 MiB limit.
func oversizedFrame(id uint64) []byte {
	const padding = 2 << 20
	var b strings.Builder
	b.Grow(padding + 128)
	b.WriteString(`{"jsonrpc":"2.0","id":`)
	b.WriteString(strconv.FormatUint(id, 10))
	b.WriteString(`,"result":{"padding":"`)
	b.WriteString(strings.Repeat("x", padding))
	b.WriteString("\"}}\n")
	return []byte(b.String())
}

// hang blocks forever without replying.
func hang() {
	for {
		time.Sleep(time.Hour)
	}
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

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeprov: %s=%q is not an integer, using %d\n", key, v, def)
		return def
	}
	return n
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeprov: %s=%q is not a number of milliseconds, using %s\n", key, v, def)
		return def
	}
	return time.Duration(n) * time.Millisecond
}
