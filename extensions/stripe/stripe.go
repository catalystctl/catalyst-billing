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
	AmountRefunded int64  `json:"-"`
	Fee            int64  `json:"-"`
	FeeCurrency    string `json:"-"`
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
	Customer       string            `json:"customer"`
	Metadata       map[string]string `json:"metadata"`
	// LatestCharge is an id string by default and an object when expanded. The
	// balance transaction under it is where Stripe reports its own fee.
	LatestCharge json.RawMessage `json:"latest_charge"`
	// LastPaymentError is Stripe's decline object on a failed confirmation.
	LastPaymentError json.RawMessage `json:"last_payment_error"`

	// Filled from the expanded latest charge.
	Fee         int64  `json:"-"`
	FeeCurrency string `json:"-"`
	DeclineCode string `json:"-"`
	FailCode    string `json:"-"`
	FailMessage string `json:"-"`
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
			Fee:            in.Fee,
			FeeCurrency:    in.FeeCurrency,
		}, nil
	}
	var out session
	path := "/v1/checkout/sessions/" + url.PathEscape(id) +
		"?expand%5B%5D=payment_intent&expand%5B%5D=payment_intent.latest_charge.balance_transaction"
	if err := c.get(ctx, path, &out); err != nil {
		return session{}, err
	}
	var expanded intent
	if len(out.PaymentIntent) > 0 {
		if err := json.Unmarshal(out.PaymentIntent, &expanded); err == nil {
			expanded.readLatestCharge()
			out.intentID = expanded.ID
			out.AmountRefunded = expanded.AmountRefunded
			out.Fee = expanded.Fee
			out.FeeCurrency = expanded.FeeCurrency
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
	path := "/v1/payment_intents/" + url.PathEscape(id) +
		"?expand%5B%5D=latest_charge.balance_transaction"
	if err := c.get(ctx, path, &out); err != nil {
		return intent{}, err
	}
	out.readLatestCharge()
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

// minorAmount formats an amount the way Stripe expects it: a bare integer of
// minor units, never a decimal. Money is never converted to float anywhere in
// this adapter.
func minorAmount(minor int64) string { return strconv.FormatInt(minor, 10) }

var errNoWebhookSecret = errors.New("no STRIPE_WEBHOOK_SECRET is configured, so a delivery cannot be authenticated")

// ---------------------------------------------------------- billing agreements --

// setupSession is the part of a Checkout Session in setup mode the adapter reads.
type setupSession struct {
	ID          string            `json:"id"`
	Status      string            `json:"status"`
	URL         string            `json:"url"`
	Customer    string            `json:"customer"`
	Metadata    map[string]string `json:"metadata"`
	Created     int64             `json:"created"`
	ExpiresAt   int64             `json:"expires_at"`
	SetupIntent json.RawMessage   `json:"setup_intent"`
}

// setupIntent is the mandate underneath a setup session.
type setupIntent struct {
	ID            string          `json:"id"`
	Status        string          `json:"status"`
	Customer      string          `json:"customer"`
	PaymentMethod json.RawMessage `json:"payment_method"`
}

// paymentMethod is the display metadata and provider id of a stored method. It is the
// only place this adapter reads a card's brand and last four, and it never reads a PAN.
type paymentMethod struct {
	ID       string `json:"id"`
	Customer string `json:"customer"`
	Card     struct {
		Brand    string `json:"brand"`
		Last4    string `json:"last4"`
		ExpMonth int    `json:"exp_month"`
		ExpYear  int    `json:"exp_year"`
	} `json:"card"`
}

// createBillingAgreement starts a hosted setup session: the customer enters the card on
// Stripe's page, and Stripe returns a reusable PaymentMethod id.
func (c *stripeClient) createBillingAgreement(ctx context.Context, key, customerID, successURL, cancelURL string, metadata map[string]string) (setupSession, error) {
	form := newForm()
	form.Set("mode", "setup")
	form.Set("success_url", successURL)
	form.Set("cancel_url", cancelURL)
	form.Set("payment_method_types[0]", "card")
	form.Set("client_reference_id", customerID)
	for k, v := range metadata {
		form.Set("metadata["+k+"]", v)
		form.Set("setup_intent_data[metadata]["+k+"]", v)
	}
	var out setupSession
	if err := c.post(ctx, "/v1/checkout/sessions", key, form, &out); err != nil {
		return setupSession{}, err
	}
	return out, nil
}

// getBillingAgreement reads a setup session and expands the method it produced.
func (c *stripeClient) getBillingAgreement(ctx context.Context, id string) (setupSession, paymentMethod, error) {
	var out setupSession
	path := "/v1/checkout/sessions/" + url.PathEscape(id) +
		"?expand%5B%5D=setup_intent.payment_method"
	if err := c.get(ctx, path, &out); err != nil {
		return setupSession{}, paymentMethod{}, err
	}
	var si setupIntent
	if len(out.SetupIntent) > 0 {
		if err := json.Unmarshal(out.SetupIntent, &si); err == nil && len(si.PaymentMethod) > 0 {
			var pm paymentMethod
			if err := json.Unmarshal(si.PaymentMethod, &pm); err == nil {
				return out, pm, nil
			}
		}
	}
	return out, paymentMethod{}, nil
}

// chargeSavedMethod confirms a PaymentIntent against a stored method with off_session=true.
// The amount and currency are the core's, in minor units; this adapter never scales them.
func (c *stripeClient) chargeSavedMethod(ctx context.Context, key string, p chargeRequest) (intent, error) {
	form := newForm()
	form.Set("amount", minorAmount(p.amountMinor))
	form.Set("currency", strings.ToLower(p.currency))
	form.Set("payment_method", p.methodToken)
	form.Set("confirm", "true")
	form.Set("off_session", "true")
	form.Set("description", p.description)
	if p.providerCustomerID != "" {
		form.Set("customer", p.providerCustomerID)
	}
	for k, v := range p.metadata {
		form.Set("metadata["+k+"]", v)
	}
	var out intent
	path := "/v1/payment_intents?expand%5B%5D=latest_charge.balance_transaction"
	if err := c.post(ctx, path, key, form, &out); err != nil {
		return intent{}, err
	}
	out.readLatestCharge()
	return out, nil
}

type chargeRequest struct {
	amountMinor        int64
	currency           string
	methodToken        string
	providerCustomerID string
	description        string
	metadata           map[string]string
}

// readLatestCharge pulls the provider's fee out of the expanded latest charge.
func (i *intent) readLatestCharge() {
	if len(i.LastPaymentError) > 0 {
		var pe struct {
			Code        string `json:"code"`
			DeclineCode string `json:"decline_code"`
			Message     string `json:"message"`
		}
		if err := json.Unmarshal(i.LastPaymentError, &pe); err == nil {
			i.FailCode = pe.DeclineCode
			if i.FailCode == "" {
				i.FailCode = pe.Code
			}
			i.FailMessage = pe.Message
		}
	}
	if len(i.LatestCharge) == 0 {
		return
	}
	var charge struct {
		BalanceTransaction json.RawMessage `json:"balance_transaction"`
	}
	if err := json.Unmarshal(i.LatestCharge, &charge); err != nil || len(charge.BalanceTransaction) == 0 {
		return
	}
	var bt struct {
		Fee      int64  `json:"fee"`
		Currency string `json:"currency"`
	}
	if err := json.Unmarshal(charge.BalanceTransaction, &bt); err == nil {
		i.Fee = bt.Fee
		i.FeeCurrency = strings.ToUpper(bt.Currency)
	}
}

// detachPaymentMethod removes a stored method from the provider. A method that is already
// gone is a success: the local row is what ultimately decides whether it can be charged.
func (c *stripeClient) detachPaymentMethod(ctx context.Context, key, methodToken string) error {
	if !strings.HasPrefix(methodToken, "pm_") {
		return nil
	}
	form := newForm()
	err := c.post(ctx, "/v1/payment_methods/"+url.PathEscape(methodToken)+"/detach", key, form, nil)
	var ae *apiError
	if errors.As(err, &ae) && ae.httpStatus == http.StatusNotFound {
		return nil
	}
	return err
}
