package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// stripeClient is the adapter's whole view of Stripe: four endpoints, one HTTP
// client, form encoding. It deliberately does not depend on Stripe's SDK — the
// SDK is a large dependency for four calls, and a version bump of it would be a
// change to the money path that nothing here needs.
type stripeClient struct {
	apiBase string
	key     string
	http    *http.Client
}

func newStripeClient(cfg config) *stripeClient {
	return &stripeClient{
		apiBase: cfg.apiBase,
		key:     cfg.secretKey,
		http:    &http.Client{Timeout: cfg.timeout},
	}
}

// form is url.Values under a local name. It is not a new type: net/url already
// encodes deterministically (keys sorted), which is what makes a test able to
// assert on the exact bytes sent to the provider.
type form = url.Values

func newForm() form { return make(url.Values) }

// apiError is a refusal or failure from Stripe, carrying the fields that decide
// what the core should do with it.
type apiError struct {
	httpStatus  int
	Type        string `json:"type"`
	Code        string `json:"code"`
	DeclineCode string `json:"decline_code"`
	Message     string `json:"message"`
	Param       string `json:"param"`
	// RequestID is Stripe's request id, which is what support asks for.
	RequestID string `json:"-"`
}

func (e *apiError) Error() string {
	if e == nil {
		return "stripe: unknown error"
	}
	msg := e.Message
	if msg == "" {
		msg = fmt.Sprintf("stripe returned HTTP %d", e.httpStatus)
	}
	if e.RequestID != "" {
		return fmt.Sprintf("%s (request %s)", msg, e.RequestID)
	}
	return msg
}

func (e *apiError) data() map[string]any {
	out := map[string]any{"http_status": e.httpStatus}
	if e.Type != "" {
		out["type"] = e.Type
	}
	if e.Code != "" {
		out["code"] = e.Code
	}
	if e.DeclineCode != "" {
		out["decline_code"] = e.DeclineCode
	}
	if e.Param != "" {
		out["param"] = e.Param
	}
	return out
}

// isDecline reports whether the payment was refused on its merits — a card
// decline, an expired card, insufficient funds. The distinction matters: a
// decline is not a broken integration, and the invoice must stay open with a
// message the customer can act on.
func (e *apiError) isDecline() bool {
	if e.Type == "card_error" {
		return true
	}
	switch e.Code {
	case "card_declined", "expired_card", "incorrect_cvc", "insufficient_funds",
		"processing_error", "incorrect_number", "authentication_required":
		return true
	}
	switch e.DeclineCode {
	case "generic_decline", "insufficient_funds", "lost_card", "stolen_card",
		"expired_card", "incorrect_cvc", "do_not_honor", "card_not_supported":
		return true
	}
	return false
}

// post sends one form-encoded request and decodes the reply.
//
// idempotencyKey is Stripe's Idempotency-Key. Sending the protocol's operation
// key here is what makes a create-whose-reply-was-lost safe to retry: Stripe
// remembers the first result for 24 hours, which outlives this process.
func (c *stripeClient) post(ctx context.Context, path, idempotencyKey string, body form, out any) error {
	return c.do(ctx, http.MethodPost, path, idempotencyKey, body, out)
}

func (c *stripeClient) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, "", nil, out)
}

func (c *stripeClient) do(ctx context.Context, method, path, idempotencyKey string, body form, out any) error {
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(body.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase+path, reader)
	if err != nil {
		return fmt.Errorf("stripe: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("stripe: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	// Bound the reply: a provider that streams an error page forever must not be
	// able to exhaust this process's memory.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("stripe: read reply: %w", err)
	}

	if resp.StatusCode >= 400 {
		ae := &apiError{httpStatus: resp.StatusCode, RequestID: resp.Header.Get("Request-Id")}
		var envelope struct {
			Error struct {
				Type        string `json:"type"`
				Code        string `json:"code"`
				DeclineCode string `json:"decline_code"`
				Message     string `json:"message"`
				Param       string `json:"param"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &envelope) == nil {
			ae.Type = envelope.Error.Type
			ae.Code = envelope.Error.Code
			ae.DeclineCode = envelope.Error.DeclineCode
			ae.Message = envelope.Error.Message
			ae.Param = envelope.Error.Param
		}
		if ae.Message == "" {
			// A non-JSON error body (a proxy's HTML page, say) is still an error;
			// keep a bounded excerpt so the log says what actually came back.
			ae.Message = fmt.Sprintf("stripe returned HTTP %d: %s", resp.StatusCode, excerpt(raw))
		}
		return ae
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("stripe: decode reply: %w", err)
	}
	return nil
}

// excerpt shortens an unexpected body for a log line.
func excerpt(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// session is the part of a Checkout Session the adapter reads.
type session struct {
	ID            string            `json:"id"`
	Status        string            `json:"status"`
	PaymentStatus string            `json:"payment_status"`
	Amount        int64             `json:"amount_total"`
	Currency      string            `json:"currency"`
	Created       int64             `json:"created"`
	Metadata      map[string]string `json:"metadata"`
	// PaymentIntent is an id string by default and an object when the request
	// expands it. Both shapes are legal, so both are handled.
	PaymentIntent json.RawMessage `json:"payment_intent"`

	// Filled from the expanded intent.
	AmountRefunded int64 `json:"-"`
	intentID       string
}

// protocolStatus maps a session onto the protocol's payment status.
func (s session) protocolStatus() string {
	if s.AmountRefunded > 0 && s.AmountRefunded >= s.Amount && s.Amount > 0 {
		return "refunded"
	}
	if s.PaymentStatus == "paid" || s.PaymentStatus == "no_payment_required" {
		return "succeeded"
	}
	return sessionStatus(s.Status, s.PaymentStatus)
}

// intent is the part of a PaymentIntent the adapter reads.
type intent struct {
	ID             string            `json:"id"`
	Status         string            `json:"status"`
	Amount         int64             `json:"amount"`
	AmountReceived int64             `json:"amount_received"`
	AmountRefunded int64             `json:"amount_refunded"`
	Currency       string            `json:"currency"`
	Metadata       map[string]string `json:"metadata"`
}

// protocolStatus maps an intent's state onto the protocol's statuses.
func (i intent) protocolStatus() string {
	switch i.Status {
	case "succeeded":
		if i.AmountRefunded > 0 && i.AmountRefunded >= i.Amount {
			return "refunded"
		}
		return "succeeded"
	case "canceled":
		return "canceled"
	case "requires_payment_method":
		// The first attempt failed and Stripe is asking for another method.
		return "failed"
	default:
		return "pending"
	}
}

// getSession reads a session, expanding its PaymentIntent so the refunded total
// is available without a second call.
func (c *stripeClient) getSession(ctx context.Context, id string) (session, error) {
	if strings.HasPrefix(id, "pi_") {
		// The core normally stores the session id, but a deployment that changed
		// its mind mid-flight may have recorded the intent. Both work.
		in, err := c.getIntent(ctx, id)
		if err != nil {
			return session{}, err
		}
		return session{
			ID:             in.ID,
			PaymentStatus:  intentPaymentStatus(in),
			Amount:         in.Amount,
			Currency:       in.Currency,
			Metadata:       in.Metadata,
			AmountRefunded: in.AmountRefunded,
		}, nil
	}
	var out session
	path := "/v1/checkout/sessions/" + url.PathEscape(id) + "?expand%5B%5D=payment_intent"
	if err := c.get(ctx, path, &out); err != nil {
		return session{}, err
	}
	var expanded intent
	if len(out.PaymentIntent) > 0 {
		if err := json.Unmarshal(out.PaymentIntent, &expanded); err == nil {
			out.intentID = expanded.ID
			out.AmountRefunded = expanded.AmountRefunded
		} else {
			// It is a bare id string.
			var raw string
			if json.Unmarshal(out.PaymentIntent, &raw) == nil {
				out.intentID = raw
			}
		}
	}
	if out.Amount == 0 {
		out.Amount = expanded.Amount
	}
	if out.Currency == "" {
		out.Currency = expanded.Currency
	}
	if out.Created == 0 {
		out.Created = time.Now().Unix()
	}
	return out, nil
}

func intentPaymentStatus(in intent) string {
	switch in.Status {
	case "succeeded":
		return "paid"
	case "canceled":
		return "unpaid"
	default:
		return "unpaid"
	}
}

func (c *stripeClient) getIntent(ctx context.Context, id string) (intent, error) {
	var out intent
	if err := c.get(ctx, "/v1/payment_intents/"+url.PathEscape(id), &out); err != nil {
		return intent{}, err
	}
	return out, nil
}

// resolveIntent turns whatever the core recorded into the PaymentIntent a refund
// must name.
func (c *stripeClient) resolveIntent(ctx context.Context, paymentID string) (intent, error) {
	if strings.HasPrefix(paymentID, "pi_") {
		return c.getIntent(ctx, paymentID)
	}
	sess, err := c.getSession(ctx, paymentID)
	if err != nil {
		return intent{}, err
	}
	if sess.intentID == "" {
		return intent{}, &apiError{
			httpStatus: http.StatusConflict,
			Type:       "invalid_request_error",
			Message:    fmt.Sprintf("session %s has no payment intent yet, so it cannot be refunded", paymentID),
		}
	}
	return c.getIntent(ctx, sess.intentID)
}

// idempotencyKeyFor builds the key the adapter sends. The protocol's operation key
// is already unique per logical operation, so it is used verbatim — rewriting it
// would break the core's assumption that a replay maps to the same provider call.
func idempotencyKeyFor(operationKey string) string {
	if len(operationKey) <= 255 {
		return operationKey
	}
	return operationKey[:255]
}

// minorAmount formats an amount the way Stripe expects it: a bare integer of
// minor units, never a decimal. Money is never converted to float anywhere in
// this adapter.
func minorAmount(minor int64) string { return strconv.FormatInt(minor, 10) }

var errNoWebhookSecret = errors.New("no STRIPE_WEBHOOK_SECRET is configured, so a delivery cannot be authenticated")
