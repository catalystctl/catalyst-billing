// Package main implements the Stripe gateway as a billing extension.
//
// It is a real adapter, not a fixture: the same process shape as
// extensions/fakegateway (newline-delimited JSON-RPC 2.0 on stdin/stdout), but the
// calls go to Stripe's REST API over HTTPS.
//
// # Why a separate process
//
// The gateway is the one component that holds a provider secret and talks to a
// third party. Running it as an extension means a hung or panicking HTTP client
// cannot take the panel down, the secret lives in the extension's environment
// rather than the core's, and the core's own tests never need a live Stripe key.
//
// # Configuration
//
//	STRIPE_SECRET_KEY       required; the API key, sk_live_… or sk_test_…
//	STRIPE_WEBHOOK_SECRET   required to verify webhooks; whsec_…
//	STRIPE_API_BASE         optional; defaults to https://api.stripe.com. Tests point
//	                        it at a local server, which is what makes the adapter
//	                        testable without network access.
//	STRIPE_CURRENCIES       optional; comma-separated codes to advertise. Defaults to
//	                        the set Stripe settles in most jurisdictions.
//	STRIPE_PUBLISHABLE_KEY  optional; only echoed in describe for the panel's benefit.
//	STRIPE_TIMEOUT_MS       optional; per-call HTTP timeout, default 20000.
//
// # Idempotency
//
// Every mutating call sends the protocol's operation_key as Stripe's
// Idempotency-Key header. That is the whole reason the protocol has the field: a
// create whose reply the core never saw must be retryable, and Stripe's own
// idempotency store is a better one than an in-process map, because it survives
// this process restarting.
//
// # Payment identity
//
// The id the core records is the Checkout Session id (cs_…), because that is what
// the customer's browser is redirected with and what a webhook names. Refunds and
// status queries therefore accept either a session id or a PaymentIntent id, and
// resolve the former to the latter: a Stripe refund is against the PaymentIntent.
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

// protocolVersion is the only protocol version this extension speaks.
const protocolVersion = 1

// maxInput bounds a request frame. A gateway receives small JSON documents; the
// limit exists so a broken peer cannot exhaust this process's memory.
const maxInput = 4 << 20

// JSON-RPC error codes. They match the fixture's set, because the core maps them
// to behaviour: -32005 is "the provider refused this on its merits" (a declined
// card), which must leave the invoice open rather than look like a crash.
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
		fmt.Fprintf(os.Stderr, "stripe: %v\n", err)
		os.Exit(2)
	}
	s := &server{
		client:  newStripeClient(cfg),
		webhook: cfg.webhookSecret,
		cfg:     cfg,
	}
	if err := s.serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "stripe: %v\n", err)
		os.Exit(1)
	}
}

// config is the extension's whole configuration, read once at startup.
type config struct {
	secretKey     string
	webhookSecret string
	apiBase       string
	currencies    []string
	publishable   string
	timeout       time.Duration
	// tolerance is how old a webhook signature may be. Stripe's own default is
	// five minutes; a replayed delivery outside that window is refused.
	tolerance time.Duration
}

// configFromEnv reads the configuration, refusing a missing key rather than
// starting a gateway that cannot charge anyone.
func configFromEnv(environ []string) (config, error) {
	env := make(map[string]string, len(environ))
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	cfg := config{
		secretKey:     strings.TrimSpace(env["STRIPE_SECRET_KEY"]),
		webhookSecret: strings.TrimSpace(env["STRIPE_WEBHOOK_SECRET"]),
		apiBase:       strings.TrimSpace(env["STRIPE_API_BASE"]),
		publishable:   strings.TrimSpace(env["STRIPE_PUBLISHABLE_KEY"]),
		timeout:       20 * time.Second,
		tolerance:     5 * time.Minute,
	}
	if cfg.apiBase == "" {
		cfg.apiBase = "https://api.stripe.com"
	}
	cfg.apiBase = strings.TrimRight(cfg.apiBase, "/")
	if cfg.secretKey == "" {
		return config{}, errors.New("STRIPE_SECRET_KEY is required")
	}
	if raw := strings.TrimSpace(env["STRIPE_CURRENCIES"]); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			if code := strings.ToUpper(strings.TrimSpace(part)); code != "" {
				cfg.currencies = append(cfg.currencies, code)
			}
		}
	}
	if len(cfg.currencies) == 0 {
		cfg.currencies = []string{"USD", "EUR", "GBP", "CAD", "AUD", "JPY", "CHF", "SEK", "NOK", "DKK"}
	}
	if raw := strings.TrimSpace(env["STRIPE_TIMEOUT_MS"]); raw != "" {
		ms, err := strconv.Atoi(raw)
		if err != nil || ms <= 0 {
			return config{}, fmt.Errorf("STRIPE_TIMEOUT_MS=%q is not a positive integer", raw)
		}
		cfg.timeout = time.Duration(ms) * time.Millisecond
	}
	if raw := strings.TrimSpace(env["STRIPE_WEBHOOK_TOLERANCE_S"]); raw != "" {
		secs, err := strconv.Atoi(raw)
		if err != nil || secs <= 0 {
			return config{}, fmt.Errorf("STRIPE_WEBHOOK_TOLERANCE_S=%q is not a positive integer", raw)
		}
		cfg.tolerance = time.Duration(secs) * time.Second
	}
	return cfg, nil
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

// amount is the wire form of money: exact minor units plus the currency code.
// Stripe's own API uses the same convention, which is why no scaling happens
// anywhere in this adapter.
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

// server is the extension's state. It is driven by one goroutine, so the only
// shared state is the HTTP client.
type server struct {
	client  *stripeClient
	webhook string
	cfg     config
	now     func() time.Time
}

func (s *server) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
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
			// A parse failure is fatal: the id it belonged to is unknown, so no
			// error can be attributed to a caller.
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
}

// capabilities is what the extension advertises in the handshake. It is exactly
// the methods it implements, because the core refuses to call one that was not
// advertised.
var capabilities = []string{
	"gateway.describe",
	"gateway.create_checkout",
	"gateway.query_payment",
	"gateway.refund",
	"gateway.verify_webhook",
}

func (s *server) handle(req *request) reply {
	switch req.Method {
	case "initialize":
		return s.initialize(req)
	case "health":
		return reply{result: map[string]string{"status": "ok"}}
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
				"name":         "stripe-gateway",
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
	return map[string]any{
		"name":              "stripe",
		"currencies":        s.cfg.currencies,
		"methods":           []map[string]string{{"id": "card", "name": "Card"}},
		"supports_refunds":  true,
		"supports_webhooks": s.webhook != "",
		"publishable_key":   s.cfg.publishable,
		// Stripe's minimum charge is 50 minor units in most currencies and 1 in
		// JPY; the panel only uses this to disable a button, so the general
		// figure is the honest one to publish.
		"min_amount": amount{Minor: 50, Currency: "USD"},
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
	if strings.TrimSpace(p.OperationKey) == "" {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "operation_key is required"}}
	}
	if err := p.Amount.validate(); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: err.Error()}}
	}
	if p.SuccessURL == "" || p.CancelURL == "" {
		// Stripe requires both, and a checkout without a return path strands the
		// customer on a page that cannot send them back.
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "success_url and cancel_url are required"}}
	}
	if p.CustomerID == "" {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "customer_id is required"}}
	}

	form := newForm()
	form.Set("mode", "payment")
	form.Set("success_url", p.SuccessURL)
	form.Set("cancel_url", p.CancelURL)
	form.Set("client_reference_id", p.CustomerID)
	form.Set("line_items[0][quantity]", "1")
	form.Set("line_items[0][price_data][currency]", strings.ToLower(p.Amount.Currency))
	form.Set("line_items[0][price_data][unit_amount]", strconv.FormatInt(p.Amount.Minor, 10))
	form.Set("line_items[0][price_data][product_data][name]", firstNonEmpty(p.Description, "Invoice"))
	for k, v := range p.Metadata {
		form.Set("metadata["+k+"]", v)
		// The same metadata on the PaymentIntent, because a refund or a dispute
		// investigation reads the intent, not the session.
		form.Set("payment_intent_data[metadata]["+k+"]", v)
	}

	var out struct {
		ID        string `json:"id"`
		URL       string `json:"url"`
		Status    string `json:"status"`
		ExpiresAt int64  `json:"expires_at"`
		AmountTot int64  `json:"amount_total"`
		Currency  string `json:"currency"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.timeout)
	defer cancel()
	if err := s.client.post(ctx, "/v1/checkout/sessions", p.OperationKey, form, &out); err != nil {
		return stripeErrorReply(err)
	}
	if out.ID == "" || out.URL == "" {
		return reply{rpcErr: &rpcError{Code: codeInternal, Message: "stripe returned a session with no id or url"}}
	}
	expires := time.Time{}
	if out.ExpiresAt > 0 {
		expires = time.Unix(out.ExpiresAt, 0).UTC()
	}
	return reply{result: map[string]any{
		"payment_id":   out.ID,
		"checkout_url": out.URL,
		"status":       sessionStatus(out.Status, ""),
		"expires_at":   expires,
	}}
}

func (s *server) queryPayment(req *request) reply {
	var p struct {
		PaymentID string `json:"payment_id"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "query_payment params: " + err.Error()}}
	}
	if p.PaymentID == "" {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "payment_id is required"}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.timeout)
	defer cancel()

	sess, err := s.client.getSession(ctx, p.PaymentID)
	if err != nil {
		return stripeErrorReply(err)
	}
	return reply{result: map[string]any{
		"payment_id":      sess.ID,
		"status":          sess.protocolStatus(),
		"amount":          amount{Minor: sess.Amount, Currency: strings.ToUpper(sess.Currency)},
		"amount_refunded": amount{Minor: sess.AmountRefunded, Currency: strings.ToUpper(sess.Currency)},
		"created_at":      time.Unix(sess.Created, 0).UTC(),
		"updated_at":      time.Unix(sess.Created, 0).UTC(),
		"metadata":        sess.Metadata,
	}}
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
	if strings.TrimSpace(p.OperationKey) == "" {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "operation_key is required"}}
	}
	if p.PaymentID == "" {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "payment_id is required"}}
	}
	if p.Amount != nil {
		if err := p.Amount.validate(); err != nil {
			return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: err.Error()}}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.timeout)
	defer cancel()

	// A refund is against the PaymentIntent, so a session id is resolved first.
	intent, err := s.client.resolveIntent(ctx, p.PaymentID)
	if err != nil {
		return stripeErrorReply(err)
	}
	// Refuse an over-refund here rather than letting the provider answer: the bound
	// is a fact the adapter already holds, and a 400 from Stripe would be reported as
	// invalid params instead of the rejection it is.
	if p.Amount != nil && intent.Amount-intent.AmountRefunded < p.Amount.Minor {
		return reply{rpcErr: &rpcError{
			Code:    codeRejected,
			Message: fmt.Sprintf("refund of %d exceeds the %d still refundable", p.Amount.Minor, intent.Amount-intent.AmountRefunded),
		}}
	}

	form := newForm()
	form.Set("payment_intent", intent.ID)
	if p.Amount != nil {
		form.Set("amount", strconv.FormatInt(p.Amount.Minor, 10))
	}
	if reason := stripeRefundReason(p.Reason); reason != "" {
		form.Set("reason", reason)
	}
	form.Set("metadata[catalyst_reason]", p.Reason)

	var out struct {
		ID       string `json:"id"`
		Status   string `json:"status"`
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
		Created  int64  `json:"created"`
	}
	if err := s.client.post(ctx, "/v1/refunds", p.OperationKey, form, &out); err != nil {
		return stripeErrorReply(err)
	}
	return reply{result: map[string]any{
		"refund_id":   out.ID,
		"payment_id":  p.PaymentID,
		"status":      refundStatus(out.Status),
		"amount":      amount{Minor: out.Amount, Currency: strings.ToUpper(out.Currency)},
		"refunded_at": time.Unix(out.Created, 0).UTC(),
	}}
}

// stripeRefundReason maps free text to the closed set Stripe accepts. Sending an
// arbitrary string is a 400, so an unmappable reason is simply omitted and the
// text travels in metadata instead, where it is preserved.
func stripeRefundReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "duplicate":
		return "duplicate"
	case "fraud", "fraudulent":
		return "fraudulent"
	case "requested_by_customer", "customer", "request":
		return "requested_by_customer"
	default:
		return ""
	}
}

// refundStatus maps Stripe's refund status onto the protocol's payment status.
func refundStatus(status string) string {
	switch status {
	case "succeeded":
		return "succeeded"
	case "pending":
		return "pending"
	case "failed", "canceled":
		return "failed"
	default:
		return "pending"
	}
}

// sessionStatus maps a Checkout Session's state onto the protocol's statuses.
// paymentStatus is the authoritative field: a session can be "complete" and still
// unpaid when the payment method is asynchronous.
func sessionStatus(sessionStatus, paymentStatus string) string {
	switch paymentStatus {
	case "paid", "no_payment_required":
		return "succeeded"
	}
	switch sessionStatus {
	case "expired":
		return "canceled"
	case "complete":
		return "pending"
	default:
		return "pending"
	}
}

// stripeErrorReply maps an adapter error onto a JSON-RPC error. The distinction
// that matters to the core is declined-versus-broken: a declined card is a
// refusal on the merits (-32005), which leaves the invoice open and tells the
// customer something useful, while an unreachable provider is a failure (-32000).
func stripeErrorReply(err error) reply {
	var ae *apiError
	if errors.As(err, &ae) {
		switch {
		case ae.httpStatus == 404:
			return reply{rpcErr: &rpcError{Code: codeNotFound, Message: ae.Error(), Data: ae.data()}}
		case ae.isDecline():
			return reply{rpcErr: &rpcError{Code: codeRejected, Message: ae.Error(), Data: ae.data()}}
		case ae.httpStatus >= 400 && ae.httpStatus < 500 && ae.httpStatus != 401 && ae.httpStatus != 403:
			return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: ae.Error(), Data: ae.data()}}
		default:
			return reply{rpcErr: &rpcError{Code: codeInternal, Message: ae.Error(), Data: ae.data()}}
		}
	}
	return reply{rpcErr: &rpcError{Code: codeInternal, Message: err.Error()}}
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
