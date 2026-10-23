package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Webhook verification.
//
// Stripe signs a delivery with HMAC-SHA256 over "{timestamp}.{body}", keyed by the
// endpoint's signing secret, and puts the result in the Stripe-Signature header:
//
//	Stripe-Signature: t=1492774577,v1=5257a869…,v1=…
//
// Three things are checked, and each of them is a real attack if it is skipped:
//
//  1. the HMAC matches, compared in constant time, so a forged body cannot settle
//     an invoice;
//  2. the timestamp is within the tolerance window, so a captured delivery cannot
//     be replayed a week later;
//  3. the event is one this adapter understands, so an unrelated event type is
//     reported as valid-but-not-payment rather than guessed at.
//
// A failure of (1) or (2) returns Valid=false with a reason, not a transport
// error: the core records a rejected delivery and moves on, which is what should
// happen when someone is probing the endpoint.

// verifyResult is the protocol's answer for a delivery.
type verifyResult struct {
	Valid      bool      `json:"valid"`
	Reason     string    `json:"reason,omitempty"`
	EventID    string    `json:"event_id,omitempty"`
	EventType  string    `json:"event_type,omitempty"`
	PaymentID  string    `json:"payment_id,omitempty"`
	Amount     *amount   `json:"amount,omitempty"`
	OccurredAt time.Time `json:"occurred_at,omitzero"`
}

// signatureHeader parses a Stripe-Signature header into its timestamp and the
// v1 signatures it carries. Stripe may send several during a secret rotation, and
// any of them matching is correct.
type signatureHeader struct {
	timestamp  int64
	signatures []string
	valid      bool
	reason     string
}

func parseSignatureHeader(header string) signatureHeader {
	var out signatureHeader
	if strings.TrimSpace(header) == "" {
		out.reason = "missing Stripe-Signature header"
		return out
	}
	for _, part := range strings.Split(header, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch key {
		case "t":
			ts, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				out.reason = "timestamp is not an integer"
				return out
			}
			out.timestamp = ts
		case "v1":
			out.signatures = append(out.signatures, value)
		}
	}
	if out.timestamp == 0 {
		out.reason = "no timestamp in Stripe-Signature"
		return out
	}
	if len(out.signatures) == 0 {
		out.reason = "no v1 signature in Stripe-Signature"
		return out
	}
	out.valid = true
	return out
}

// signPayload computes the signature Stripe would send for a body at a time. It
// is used by the adapter to verify, and by the tests to construct a delivery the
// adapter must accept — the two sharing one function is what keeps the test
// honest.
func signPayload(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// verifySignature reports whether the header authenticates the body, and why not
// when it does not.
func verifySignature(secret, header string, body []byte, now time.Time, tolerance time.Duration) (bool, string) {
	if secret == "" {
		return false, errNoWebhookSecret.Error()
	}
	parsed := parseSignatureHeader(header)
	if !parsed.valid {
		return false, parsed.reason
	}
	// The tolerance cuts both ways: a timestamp far in the future is as suspicious
	// as one far in the past, and a clock-skewed sender must not be able to sign a
	// delivery that stays valid indefinitely.
	skew := now.Sub(time.Unix(parsed.timestamp, 0))
	if skew > tolerance {
		return false, fmt.Sprintf("timestamp is %s old, outside the %s tolerance", skew.Round(time.Second), tolerance)
	}
	if skew < -tolerance {
		return false, fmt.Sprintf("timestamp is %s in the future, outside the %s tolerance", (-skew).Round(time.Second), tolerance)
	}
	expected := signPayload(secret, parsed.timestamp, body)
	for _, candidate := range parsed.signatures {
		if hmac.Equal([]byte(candidate), []byte(expected)) {
			return true, ""
		}
	}
	return false, "signature mismatch"
}

// stripeEvent is the part of a Stripe event the adapter reads.
type stripeEvent struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Created int64  `json:"created"`
	Data    struct {
		Object struct {
			// A Checkout Session.
			ID            string            `json:"id"`
			PaymentStatus string            `json:"payment_status"`
			AmountTotal   int64             `json:"amount_total"`
			PaymentIntent json.RawMessage   `json:"payment_intent"`
			Metadata      map[string]string `json:"metadata"`
			// A PaymentIntent.
			Amount         int64  `json:"amount"`
			AmountReceived int64  `json:"amount_received"`
			Currency       string `json:"currency"`
			// Either object may carry a status.
			Status string `json:"status"`
		} `json:"object"`
	} `json:"data"`
}

// paymentSucceededEvents is the closed set of Stripe event types that mean "this
// payment settled". A list rather than a prefix test, because "succeeded" appears
// in event names that mean something else.
var paymentSucceededEvents = map[string]bool{
	"checkout.session.completed":               true,
	"checkout.session.async_payment_succeeded": true,
	"payment_intent.succeeded":                 true,
}

// verifyWebhook authenticates and decodes one delivery.
func (s *server) verifyWebhook(req *request) reply {
	var p struct {
		Headers map[string]string `json:"headers"`
		Body    []byte            `json:"body"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply{rpcErr: &rpcError{Code: codeInvalidParams, Message: "verify_webhook params: " + err.Error()}}
	}

	header := ""
	for k, v := range p.Headers {
		if strings.EqualFold(k, "Stripe-Signature") {
			header = v
		}
	}
	now := s.clock()
	ok, reason := verifySignature(s.webhook, header, p.Body, now, s.cfg.tolerance)
	if !ok {
		return reply{result: verifyResult{Valid: false, Reason: reason}}
	}

	var ev stripeEvent
	if err := json.Unmarshal(p.Body, &ev); err != nil {
		return reply{result: verifyResult{Valid: false, Reason: "body is not a JSON event: " + err.Error()}}
	}
	if ev.ID == "" {
		return reply{result: verifyResult{Valid: false, Reason: "event has no id, so it cannot be deduplicated"}}
	}

	out := verifyResult{
		Valid:     true,
		EventID:   ev.ID,
		EventType: ev.Type,
	}
	if ev.Created > 0 {
		out.OccurredAt = time.Unix(ev.Created, 0).UTC()
	} else {
		out.OccurredAt = now.UTC()
	}

	if !paymentSucceededEvents[ev.Type] {
		// Valid, authentic, and not about a settled payment. Reporting it as
		// valid lets the core record it and skip; reporting an error would fill
		// the retry queue with deliveries that can never succeed.
		out.Reason = "not a payment event"
		return reply{result: out}
	}

	obj := ev.Data.Object
	switch {
	case ev.Type == "payment_intent.succeeded":
		// The core keys a checkout on the session id, so a delivery that names
		// only the intent is resolved back to its session. Without this the
		// attempt lookup has nothing to compare the amount against and the core
		// refuses the payment — correctly, but the customer's money is sitting at
		// the provider.
		sessionID, err := s.sessionForIntent(req, obj.ID)
		if err != nil {
			// Fall back to the intent id: the core will report a payment with no
			// recorded checkout, which is the honest outcome for a charge this
			// panel did not start.
			sessionID = obj.ID
		}
		out.PaymentID = sessionID
		amt := obj.AmountReceived
		if amt == 0 {
			amt = obj.Amount
		}
		if amt > 0 && obj.Currency != "" {
			out.Amount = &amount{Minor: amt, Currency: strings.ToUpper(obj.Currency)}
		}
	default:
		if obj.PaymentStatus != "paid" && obj.PaymentStatus != "no_payment_required" {
			// checkout.session.completed with an unpaid status is an asynchronous
			// method still in flight, not a settlement. It must not name a payment:
			// the core would try to apply it.
			out.Reason = "session is not paid yet"
			return reply{result: out}
		}
		out.PaymentID = obj.ID
		if obj.AmountTotal > 0 && obj.Currency != "" {
			out.Amount = &amount{Minor: obj.AmountTotal, Currency: strings.ToUpper(obj.Currency)}
		}
	}
	if out.PaymentID == "" {
		return reply{result: verifyResult{
			Valid: false, Reason: "event carries no payment identifier",
		}}
	}
	// The core acts on its own settlement vocabulary, not on provider event names: it has no
	// provider knowledge and must not grow any, so mapping "checkout.session.completed" is
	// this adapter's job. Reporting Stripe's own type here left the core with an event it did
	// not recognise, which it skipped — the invoice stayed unpaid and nothing said so. The raw
	// delivery is stored alongside the normalised event, so Stripe's own type is still there
	// for an operator reading the inbox.
	out.EventType = "payment.succeeded"
	return reply{result: out}
}

// sessionForIntent finds the Checkout Session a PaymentIntent belongs to. Stripe
// exposes the relation as a filter on the sessions list, so this is one call.
func (s *server) sessionForIntent(req *request, intentID string) (string, error) {
	if intentID == "" {
		return "", errors.New("empty payment intent id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.timeout)
	defer cancel()
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	path := "/v1/checkout/sessions?limit=1&payment_intent=" + intentID
	if err := s.client.get(ctx, path, &list); err != nil {
		return "", err
	}
	if len(list.Data) == 0 {
		return "", errors.New("no checkout session for payment intent")
	}
	return list.Data[0].ID, nil
}
