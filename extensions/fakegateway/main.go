// Command fakegateway is a conformance fixture for the billing extension
// protocol: an in-memory payment gateway that speaks newline-delimited JSON-RPC
// 2.0 on stdin/stdout.
//
// It exists so that the core's extension client, the supervisor and the invoice
// flow can be tested against a real second process with real pipes, including
// every failure mode that is impossible to trigger against a real gateway on
// demand. It is deterministic: ids come from a counter, no randomness is
// involved, and the same input always produces the same output.
//
// # Programming the fixture
//
//	FAKE_GATEWAY_MODE=ok|fail|hang|oversized|crash|slow
//	FAKE_GATEWAY_SLOW_MS=<n>       delay for slow mode (default 500)
//	FAKE_GATEWAY_VERSION=<n>       reply with this protocol version instead of 1
//	FAKE_GATEWAY_NEGOTIATE=fail    refuse version negotiation
//	FAKE_GATEWAY_PIDFILE=<path>    write the process id at startup
//	FAKE_GATEWAY_STDERR=<text>     write this text to stderr at startup
//
// The failure mode applies to every method except "initialize", so the handshake
// always succeeds and the failure happens where the test wants it: on the call,
// or on the supervisor's post-restart health check.
//
// # Idempotency
//
// Mutating calls are idempotent by operation_key, in memory: replaying a key
// returns the original payment instead of creating a second one. That is what
// makes a retry-after-timeout safe, and it is the property the core relies on
// when it retries a create whose reply it never saw.
//
// # Fixture-only methods
//
//	health          liveness probe; the only call allowed before initialize
//	fixture.echo    returns the raw params it received, verbatim
//	fixture.env     returns the child's environment as {"vars": {...}}
//
// fixture.echo is not part of the protocol; it lets tests assert on the exact
// bytes the client put on the wire.
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

// maxInput bounds a request frame. The core never sends anything close to this;
// the limit only stops a broken peer from exhausting the fixture's memory.
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
	writePIDFile(os.Getenv("FAKE_GATEWAY_PIDFILE"))
	if msg := os.Getenv("FAKE_GATEWAY_STDERR"); msg != "" {
		fmt.Fprint(os.Stderr, msg)
	}
	s := &server{
		mode:      envOr("FAKE_GATEWAY_MODE", "ok"),
		slow:      envDuration("FAKE_GATEWAY_SLOW_MS", 500*time.Millisecond),
		version:   envInt("FAKE_GATEWAY_VERSION", protocolVersion),
		negotiate: os.Getenv("FAKE_GATEWAY_NEGOTIATE"),
		payments:  make(map[string]*payment),
		byKey:     make(map[string]string),
		refunds:   make(map[string]*refundResult),
	}
	if err := s.serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "fakegateway: %v\n", err)
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

// reply is what one handled request produces: a result, an error, or raw bytes
// for the oversized mode.
type reply struct {
	result any
	rpcErr *rpcError
	raw    []byte
}

// amount is the wire form of money: exact minor units plus the currency code.
type amount struct {
	Minor    int64  `json:"minor"`
	Currency string `json:"currency"`
}

func (a amount) validate() error {
	if a.Currency == "" || a.Currency != strings.TrimSpace(a.Currency) || a.Currency != strings.ToUpper(a.Currency) {
		return fmt.Errorf("currency %q is not canonical upper-case", a.Currency)
	}
	if a.Minor <= 0 {
		return fmt.Errorf("minor must be positive, got %d", a.Minor)
	}
	return nil
}

// payment is one gateway-side payment.
type payment struct {
	ID          string
	CustomerID  string
	Status      string
	Amount      amount
	Refunded    amount
	Description string
	Metadata    map[string]string
	CheckoutURL string
	ExpiresAt   time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// server is the fixture's whole state. It is driven by a single goroutine, so it
// needs no locking.
type server struct {
	mode      string
	slow      time.Duration
	version   int
	negotiate string

	seq      int
	payments map[string]*payment
	byKey    map[string]string
	refunds  map[string]*refundResult
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
			// A parse failure is fatal for a JSON-RPC stream: we cannot know
			// which id it belonged to.
			return fmt.Errorf("parse error: %w", err)
		}
		if req.ID == nil {
			// Notifications (such as $/cancelRequest) need no answer.
			continue
		}
		if req.JSONRPC != "2.0" {
			if err := enc.Encode(response{JSONRPC: "2.0", ID: *req.ID, Error: &rpcError{Code: codeInvalidRequest, Message: "jsonrpc must be 2.0"}}); err != nil {
				return err
			}
			continue
		}
		rep := s.handle(&req)
		if rep.raw != nil {
			if _, err := out.Write(rep.raw); err != nil {
				return err
			}
			continue
		}
		resp := response{JSONRPC: "2.0", ID: *req.ID, Result: rep.result, Error: rep.rpcErr}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
}

// handle dispatches one request, applying the programmed failure mode.
func (s *server) handle(req *request) reply {
	// The handshake always works, otherwise no test could reach the failure it
	// wants to provoke.
	if req.Method == "initialize" {
		return s.initialize(req)
	}
	switch s.mode {
	case "crash":
		// Exit mid-call: the caller's request has been read and will never be
		// answered.
		os.Exit(3)
	case "hang":
		hang()
	case "fail":
		return reply{rpcErr: &rpcError{
			Code:    -32000,
			Message: "fake gateway failure",
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
	case "gateway.describe":
		return reply{result: s.describe()}
	case "gateway.create_checkout":
		return s.createCheckout(req)
	case "gateway.query_payment":
		return s.queryPayment(req)
	case "gateway.refund":
		return s.refund(req)
	case "gateway.verify_webhook":
		return s.verifyWebhook(req)
	default:
		return reply{rpcErr: &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}}
	}
}

// capabilities is what the fixture advertises in the handshake.
var capabilities = []string{
	"gateway.describe",
	"gateway.create_checkout",
	"gateway.query_payment",
	"gateway.refund",
	"gateway.verify_webhook",
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
		// Simulate an extension that answers with a version the core never
		// offered; the core must reject it rather than guess.
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
		"name":         "fakegateway",
		"publisher":    "catalyst-fixtures",
		"capabilities": capabilities,
	}}
}

func (s *server) describe() map[string]any {
	return map[string]any{
		"name":              "fakegateway",
		"currencies":        []string{"USD", "EUR", "JPY"},
		"methods":           []map[string]string{{"id": "card", "name": "Card"}, {"id": "bank", "name": "Bank transfer"}},
		"supports_refunds":  true,
		"supports_webhooks": true,
		"min_amount":        amount{Minor: 50, Currency: "USD"},
	}
}

func (s *server) createCheckout(req *request) reply {
	var p struct {
		OperationKey string            `json:"operation_key"`
		CustomerID   string            `json:"customer_id"`
		Amount       amount            `json:"amount"`
		Description  string            `json:"description"`
		SuccessURL   string            `json:"success_url"`
		CancelURL    string            `json:"cancel_url"`
		Metadata     map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "create_checkout params: " + err.Error()}}
	}
	if p.OperationKey == "" {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "operation_key is required"}}
	}
	if id, ok := s.byKey[p.OperationKey]; ok {
		// Replay: return the original checkout, do not charge again.
		pay := s.payments[id]
		return reply{result: checkoutResult{
			PaymentID:   pay.ID,
			CheckoutURL: pay.CheckoutURL,
			Status:      pay.Status,
			ExpiresAt:   pay.ExpiresAt,
		}}
	}
	if err := p.Amount.validate(); err != nil {
		return reply{rpcErr: &rpcError{Code: codeRejected, Message: err.Error()}}
	}
	if p.CustomerID == "" {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "customer_id is required"}}
	}
	s.seq++
	now := time.Now().UTC()
	pay := &payment{
		ID:          fmt.Sprintf("pay_%d", s.seq),
		CustomerID:  p.CustomerID,
		Status:      "pending",
		Amount:      p.Amount,
		Refunded:    amount{Minor: 0, Currency: p.Amount.Currency},
		Description: p.Description,
		Metadata:    p.Metadata,
		CheckoutURL: fmt.Sprintf("https://fakegateway.invalid/checkout/pay_%d", s.seq),
		ExpiresAt:   now.Add(30 * time.Minute),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	s.payments[pay.ID] = pay
	s.byKey[p.OperationKey] = pay.ID
	return reply{result: checkoutResult{
		PaymentID:   pay.ID,
		CheckoutURL: pay.CheckoutURL,
		Status:      pay.Status,
		ExpiresAt:   pay.ExpiresAt,
	}}
}

type checkoutResult struct {
	PaymentID   string    `json:"payment_id"`
	CheckoutURL string    `json:"checkout_url"`
	Status      string    `json:"status"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func (s *server) queryPayment(req *request) reply {
	var p struct {
		PaymentID string `json:"payment_id"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "query_payment params: " + err.Error()}}
	}
	pay, ok := s.payments[p.PaymentID]
	if !ok {
		return reply{rpcErr: &rpcError{Code: codeNotFound, Message: "unknown payment " + p.PaymentID}}
	}
	return reply{result: map[string]any{
		"payment_id":      pay.ID,
		"status":          pay.Status,
		"amount":          pay.Amount,
		"amount_refunded": pay.Refunded,
		"created_at":      pay.CreatedAt,
		"updated_at":      pay.UpdatedAt,
		"metadata":        pay.Metadata,
	}}
}

type refundResult struct {
	RefundID   string    `json:"refund_id"`
	PaymentID  string    `json:"payment_id"`
	Status     string    `json:"status"`
	Amount     amount    `json:"amount"`
	RefundedAt time.Time `json:"refunded_at"`
}

func (s *server) refund(req *request) reply {
	var p struct {
		OperationKey string  `json:"operation_key"`
		PaymentID    string  `json:"payment_id"`
		Amount       *amount `json:"amount"`
		Reason       string  `json:"reason"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "refund params: " + err.Error()}}
	}
	if p.OperationKey == "" {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "operation_key is required"}}
	}
	if prev, ok := s.refunds[p.OperationKey]; ok {
		return reply{result: *prev}
	}
	pay, ok := s.payments[p.PaymentID]
	if !ok {
		return reply{rpcErr: &rpcError{Code: codeNotFound, Message: "unknown payment " + p.PaymentID}}
	}
	outstanding := amount{Minor: pay.Amount.Minor - pay.Refunded.Minor, Currency: pay.Amount.Currency}
	want := outstanding
	if p.Amount != nil {
		want = *p.Amount
		if want.Currency != pay.Amount.Currency {
			return reply{rpcErr: &rpcError{Code: codeRejected, Message: "refund currency does not match payment"}}
		}
		if want.Minor <= 0 || want.Minor > outstanding.Minor {
			return reply{rpcErr: &rpcError{
				Code:    codeRejected,
				Message: fmt.Sprintf("refund of %d exceeds outstanding %d", want.Minor, outstanding.Minor),
			}}
		}
	}
	s.seq++
	now := time.Now().UTC()
	pay.Refunded.Minor += want.Minor
	pay.UpdatedAt = now
	if pay.Refunded.Minor == pay.Amount.Minor {
		pay.Status = "refunded"
	}
	res := &refundResult{
		RefundID:   fmt.Sprintf("ref_%d", s.seq),
		PaymentID:  pay.ID,
		Status:     "succeeded",
		Amount:     want,
		RefundedAt: now,
	}
	s.refunds[p.OperationKey] = res
	return reply{result: *res}
}

func (s *server) verifyWebhook(req *request) reply {
	var p struct {
		Headers map[string]string `json:"headers"`
		Body    []byte            `json:"body"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "verify_webhook params: " + err.Error()}}
	}
	sig := ""
	for k, v := range p.Headers {
		if strings.EqualFold(k, "X-Signature") {
			sig = v
		}
	}
	if sig != "sha256=fake" {
		return reply{result: map[string]any{"valid": false, "reason": "signature mismatch"}}
	}
	var event struct {
		EventID   string `json:"event_id"`
		EventType string `json:"event_type"`
		PaymentID string `json:"payment_id"`
		Amount    amount `json:"amount"`
	}
	if err := json.Unmarshal(p.Body, &event); err != nil {
		return reply{result: map[string]any{"valid": false, "reason": "body is not JSON: " + err.Error()}}
	}
	if event.PaymentID != "" {
		if _, ok := s.payments[event.PaymentID]; !ok {
			return reply{rpcErr: &rpcError{Code: codeNotFound, Message: "unknown payment " + event.PaymentID}}
		}
	}
	return reply{result: map[string]any{
		"valid":       true,
		"event_id":    event.EventID,
		"event_type":  event.EventType,
		"payment_id":  event.PaymentID,
		"amount":      event.Amount,
		"occurred_at": time.Now().UTC(),
	}}
}

// writePIDFile records the fixture's pid so tests can check that the core
// killed the process (and, for the process-group test, its children).
func writePIDFile(path string) {
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "fakegateway: writing pid file: %v\n", err)
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

// hang blocks forever without replying. A plain select{} would deadlock the
// runtime, so sleep in a loop instead.
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
		fmt.Fprintf(os.Stderr, "fakegateway: %s=%q is not an integer, using %d\n", key, v, def)
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
		fmt.Fprintf(os.Stderr, "fakegateway: %s=%q is not a number of milliseconds, using %s\n", key, v, def)
		return def
	}
	return time.Duration(n) * time.Millisecond
}
