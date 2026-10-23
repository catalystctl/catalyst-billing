// Package payments connects the gateway to the subledger.
//
// The shape of this package is dictated by one rule: a browser redirect is never
// proof of payment, and a webhook delivery is never proof of a *single* payment.
//
// The flow is therefore:
//
//  1. Verify: authenticate the raw body and normalise it into an event. The
//     signature is checked over the exact bytes received, before any parsing, so
//     a body that is re-serialised cannot be smuggled past the check.
//  2. Persist: store the event in the inbox under a unique (provider, account,
//     event id). Nothing else happens before the row is durable. If persistence
//     fails the request fails, so the provider retries instead of the event being
//     lost.
//  3. Process: a worker asks the gateway for the *authoritative* payment state.
//     A "checkout completed" webhook is a hint to go and look, not a payment.
//  4. Apply: the payment row, its allocation, the invoice transition and the
//     provisioning job commit in one transaction.
//
// Duplicate delivery is stopped twice, on purpose. The unique event id absorbs a
// redelivery of the same event; the unique provider payment id absorbs two
// *different* events that describe the same payment. Neither check is sufficient
// alone, and the provider documents that both situations occur.
package payments

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

var (
	// ErrInvalidSignature is returned when a webhook body does not authenticate.
	ErrInvalidSignature = errors.New("payments: invalid webhook signature")
	// ErrUnsupportedEvent is returned for an event type the core does not act on.
	ErrUnsupportedEvent = errors.New("payments: unsupported event type")
	// ErrProviderMismatch is returned when an event's account does not match the
	// configured gateway account.
	ErrProviderMismatch = errors.New("payments: provider account mismatch")
	// ErrNotPaid is returned when the provider's authoritative state is not a
	// successful payment.
	ErrNotPaid = errors.New("payments: provider reports the payment is not settled")
	// ErrMissingInvoice is returned when a settled payment cannot be tied to an
	// invoice. It is not an error to retry blindly: it means the checkout was
	// created without an invoice reference.
	ErrMissingInvoice = errors.New("payments: settled payment has no invoice reference")
	// ErrAmountMismatch is returned when the provider settled an amount that does
	// not match what the customer was asked to pay. This is deliberately fatal
	// rather than treated as a partial payment: a mismatch is an anomaly, and
	// applying it would launder a wrong charge into legitimate revenue.
	ErrAmountMismatch = errors.New("payments: settled amount does not match the checkout")
	// ErrMissingPaymentID is returned when neither the verifier nor the stored body
	// names the provider payment an event is about. Nothing can be queried or applied
	// without it, so the event stays pending for an operator to look at.
	ErrMissingPaymentID = errors.New("payments: event carries no payment id")
)

// The core's own vocabulary for "this event settled a payment".
//
// It is deliberately provider-free: an extension normalises its provider's names into one of
// these, because the core has no provider knowledge and must not grow any. A delivery reported
// under any other kind is recorded and not acted on.
const (
	// EventPaymentSucceeded means a payment settled. It is the kind every gateway uses.
	EventPaymentSucceeded = "payment.succeeded"
	// EventPaymentIntentSucceeded is the older spelling some gateways report. It is
	// accepted because it is already stored in inbox rows and in the fixture gateway's
	// contract; new adapters should report EventPaymentSucceeded.
	EventPaymentIntentSucceeded = "payment_intent.succeeded"
)

// IsSettlementKind reports whether an event type means "a payment settled".
func IsSettlementKind(kind string) bool {
	return kind == EventPaymentSucceeded || kind == EventPaymentIntentSucceeded
}

// VerifiedEvent is a normalised, authenticated webhook.
type VerifiedEvent struct {
	EventID   string
	EventType string
	PaymentID string
	// Amount is what the provider says was charged, when it reports one. It is
	// informational: the authoritative amount comes from QueryPayment.
	Amount *money.Amount
}

// Verifier authenticates and normalises a webhook.
//
// Two implementations exist. ExtensionVerifier delegates to the gateway's own
// verify_webhook method, which is the only component that knows a provider's
// payload shape. HMACSigner verifies a Stripe-style signature in the core, for
// gateways whose payloads the core already understands.
type Verifier interface {
	Verify(ctx context.Context, raw []byte, headers map[string]string) (VerifiedEvent, error)
}

// WebhookVerifier is the extension method used for normalisation.
type WebhookVerifier interface {
	VerifyWebhook(ctx context.Context, raw []byte, headers map[string]string) (extproto.VerifyWebhookResult, error)
}

// ExtensionVerifier verifies through the gateway extension.
type ExtensionVerifier struct {
	Gateway WebhookVerifier
}

// Verify implements Verifier.
func (v ExtensionVerifier) Verify(ctx context.Context, raw []byte, headers map[string]string) (VerifiedEvent, error) {
	if len(raw) == 0 {
		return VerifiedEvent{}, fmt.Errorf("%w: empty body", ErrInvalidSignature)
	}
	res, err := v.Gateway.VerifyWebhook(ctx, raw, headers)
	if err != nil {
		// A transport failure is not a verdict on the signature; the caller must
		// not treat it as a rejection that gets acknowledged.
		return VerifiedEvent{}, fmt.Errorf("payments: verify webhook: %w", err)
	}
	if !res.Valid {
		reason := res.Reason
		if reason == "" {
			reason = "unspecified"
		}
		return VerifiedEvent{}, fmt.Errorf("%w: %s", ErrInvalidSignature, reason)
	}
	if res.EventID == "" {
		// Without an id there is no way to deduplicate, so the event cannot be
		// accepted: accepting it would mean processing it once per delivery.
		return VerifiedEvent{}, fmt.Errorf("%w: provider returned no event id", ErrInvalidSignature)
	}

	out := VerifiedEvent{
		EventID:   res.EventID,
		EventType: res.EventType,
		PaymentID: res.PaymentID,
	}
	if res.Amount != nil {
		amt, err := res.Amount.Money()
		if err != nil {
			return VerifiedEvent{}, fmt.Errorf("payments: webhook amount: %w", err)
		}
		out.Amount = &amt
	}
	return out, nil
}

// HMACSigner verifies a Stripe-style signature in the core and reads the event
// identity from the JSON body.
type HMACSigner struct {
	// Secret is the endpoint signing secret.
	Secret []byte
	// Header is the header carrying the signature, e.g. "Stripe-Signature".
	Header string
	// TimestampHeader is the header carrying the signed timestamp.
	TimestampHeader string
	// Tolerance bounds clock skew so a replayed old signature is refused.
	Tolerance time.Duration
}

// hmacPayload is the body shape HMACSigner understands.
type hmacPayload struct {
	EventID   string      `json:"event_id"`
	EventType string      `json:"event_type"`
	PaymentID string      `json:"payment_id"`
	Amount    *wireAmount `json:"amount,omitempty"`
}

type wireAmount struct {
	Minor    int64  `json:"minor"`
	Currency string `json:"currency"`
}

// Verify implements Verifier.
//
// The comparison uses hmac.Equal so a timing side channel cannot be used to forge
// a signature one byte at a time. The timestamp is checked first, because an
// obviously stale request should cost no cryptographic work.
func (s HMACSigner) Verify(_ context.Context, raw []byte, headers map[string]string) (VerifiedEvent, error) {
	if len(s.Secret) == 0 {
		return VerifiedEvent{}, fmt.Errorf("%w: no signing secret configured", ErrInvalidSignature)
	}
	if len(raw) == 0 {
		return VerifiedEvent{}, fmt.Errorf("%w: empty body", ErrInvalidSignature)
	}
	tolerance := s.Tolerance
	if tolerance <= 0 {
		tolerance = 5 * time.Minute
	}

	sig := headerValue(headers, s.Header)
	ts := headerValue(headers, s.TimestampHeader)
	if sig == "" || ts == "" {
		return VerifiedEvent{}, fmt.Errorf("%w: missing signature headers", ErrInvalidSignature)
	}
	tsTime, err := parseUnix(ts)
	if err != nil {
		return VerifiedEvent{}, fmt.Errorf("%w: bad timestamp %q", ErrInvalidSignature, ts)
	}
	skew := time.Since(tsTime)
	if skew < 0 {
		skew = -skew
	}
	if skew > tolerance {
		return VerifiedEvent{}, fmt.Errorf("%w: timestamp outside %s tolerance", ErrInvalidSignature, tolerance)
	}

	mac := hmac.New(sha256.New, s.Secret)
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(raw)
	want := hex.EncodeToString(mac.Sum(nil))

	matched := false
	for _, candidate := range strings.Split(sig, ",") {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "v1=")
		if hmac.Equal([]byte(candidate), []byte(want)) {
			matched = true
		}
	}
	if !matched {
		return VerifiedEvent{}, fmt.Errorf("%w: no matching signature", ErrInvalidSignature)
	}

	var body hmacPayload
	if err := json.Unmarshal(raw, &body); err != nil {
		return VerifiedEvent{}, fmt.Errorf("payments: webhook body is not JSON: %w", err)
	}
	if body.EventID == "" {
		return VerifiedEvent{}, fmt.Errorf("%w: body has no event_id", ErrInvalidSignature)
	}
	out := VerifiedEvent{
		EventID:   body.EventID,
		EventType: body.EventType,
		PaymentID: body.PaymentID,
	}
	if body.Amount != nil {
		cur, ok := money.LookupCurrency(body.Amount.Currency)
		if !ok {
			return VerifiedEvent{}, fmt.Errorf("payments: webhook amount has unknown currency %q",
				body.Amount.Currency)
		}
		amt := money.New(body.Amount.Minor, cur)
		out.Amount = &amt
	}
	return out, nil
}

func headerValue(headers map[string]string, name string) string {
	if name == "" {
		return ""
	}
	if v, ok := headers[name]; ok {
		return v
	}
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func parseUnix(s string) (time.Time, error) {
	var sec int64
	if _, err := fmt.Sscanf(s, "%d", &sec); err != nil {
		return time.Time{}, err
	}
	return time.Unix(sec, 0).UTC(), nil
}

// Inbox stores webhook events.
type Inbox struct {
	pool *store.Pool
}

// NewInbox constructs an Inbox.
func NewInbox(pool *store.Pool) *Inbox { return &Inbox{pool: pool} }

// Receive persists a verified event and reports whether it is new.
//
// A duplicate is not an error from the provider's point of view: it is
// acknowledged so the provider stops retrying.
//
// Two things are persisted, and they are not the same thing: the verifier's normalisation
// (event type and provider payment id) and the provider's raw body. The normalisation is what
// the processor acts on — the core cannot parse a provider's payload shape, and by the time the
// processor runs the signature headers are long gone — while the raw body stays as the audit
// record of what the provider actually sent.
func (i *Inbox) Receive(ctx context.Context, provider, account string, ev VerifiedEvent, raw []byte) (newEvent bool, err error) {
	if provider == "" || account == "" || ev.EventID == "" {
		return false, fmt.Errorf("payments: incomplete event identity")
	}
	if len(raw) == 0 {
		return false, fmt.Errorf("payments: empty webhook body")
	}

	var id string
	err = i.pool.QueryRow(ctx, `
		INSERT INTO gateway_events
			(provider, provider_account, event_id, event_type, payload, signature_valid, payment_id)
		VALUES ($1, $2, $3, $4, $5, true, NULLIF($6, ''))
		ON CONFLICT (provider, provider_account, event_id) DO NOTHING
		RETURNING id`,
		provider, account, ev.EventID, ev.EventType, raw, ev.PaymentID,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already ingested. A redelivery still carries the normalisation the first delivery
		// may have lacked: rows written before gateway_events.payment_id existed, or by a
		// verifier that reported a provider-shaped event type. Filling it in is what makes a
		// provider's "resend" a repair rather than a no-op — the row is still unprocessed, so
		// the next processor run applies it. Nothing is re-processed here, and a processed
		// row is never rewritten: it is the audit record of what was applied.
		if _, rerr := i.pool.Exec(ctx, `
			UPDATE gateway_events
			   SET payment_id = COALESCE(NULLIF($4, ''), payment_id),
			       event_type = COALESCE(NULLIF($5, ''), event_type)
			 WHERE provider = $1 AND provider_account = $2 AND event_id = $3
			   AND processed_at IS NULL`,
			provider, account, ev.EventID, ev.PaymentID, ev.EventType,
		); rerr != nil {
			return false, fmt.Errorf("payments: repair webhook event: %w", rerr)
		}
		return false, nil
	}
	if err != nil {
		// Durable persistence failed: the caller must NOT acknowledge, so the
		// provider retries and the event is not lost.
		return false, fmt.Errorf("payments: persist webhook event: %w", err)
	}
	return true, nil
}

// PendingEvent is an unprocessed inbox row.
type PendingEvent struct {
	ID              string
	Provider        string
	ProviderAccount string
	EventID         string
	EventType       string
	// PaymentID is the provider payment id the verifier normalised at ingest time. It is
	// empty for rows written before it was persisted, which is why the processor still has a
	// body fallback.
	PaymentID string
	Raw       []byte
	Attempts  int
}

// ClaimPending selects unprocessed events, oldest first.
//
// It does not lock: processing is idempotent, so two workers racing on the same
// event is safe and cheaper than coordinating. The guard against double effects is
// the unique provider payment id inside the apply step.
func (i *Inbox) ClaimPending(ctx context.Context, limit int) ([]PendingEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := i.pool.Query(ctx, `
		SELECT id, provider, provider_account, event_id, event_type,
		       COALESCE(payment_id, ''), payload, attempts
		FROM gateway_events
		WHERE processed_at IS NULL
		ORDER BY received_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("payments: claim pending events: %w", err)
	}
	defer rows.Close()

	var out []PendingEvent
	for rows.Next() {
		var e PendingEvent
		if err := rows.Scan(&e.ID, &e.Provider, &e.ProviderAccount, &e.EventID,
			&e.EventType, &e.PaymentID, &e.Raw, &e.Attempts); err != nil {
			return nil, fmt.Errorf("payments: scan pending event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkProcessed records a successful handling.
func (i *Inbox) MarkProcessed(ctx context.Context, id string) error {
	_, err := i.pool.Exec(ctx, `
		UPDATE gateway_events SET processed_at = now(), attempts = attempts + 1, last_error = NULL
		WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("payments: mark event processed: %w", err)
	}
	return nil
}

// MarkFailed records a failed handling so the event stays pending and the cause
// is visible to an operator.
func (i *Inbox) MarkFailed(ctx context.Context, id string, cause error) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	_, err := i.pool.Exec(ctx, `
		UPDATE gateway_events SET attempts = attempts + 1, last_error = $2 WHERE id = $1`, id, msg)
	if err != nil {
		return fmt.Errorf("payments: mark event failed: %w", err)
	}
	return nil
}

// Gateway is the payment side of an extension.
type Gateway interface {
	// QueryPayment asks the provider for the authoritative state of a payment.
	QueryPayment(ctx context.Context, paymentID string) (extproto.QueryPaymentResult, error)
	// Name identifies the gateway for diagnostics.
	Name() string
}

// Applier records a settled payment against an invoice and returns the outcome.
type Applier interface {
	ApplySettledPayment(ctx context.Context, tx pgx.Tx, p SettledPayment) (ApplyResult, error)
}

// SettledPayment is an authoritative, provider-confirmed payment.
type SettledPayment struct {
	Provider          string
	ProviderAccount   string
	ProviderPaymentID string
	InvoiceID         string
	// AmountMinor and Currency come from the provider, never from the browser.
	AmountMinor int64
	Currency    string
}

// ApplyResult reports what the application did.
type ApplyResult struct {
	Duplicate   bool
	InvoicePaid bool
	PaymentID   string
}

// AttemptLookup resolves the recorded attempt behind a provider payment.
type AttemptLookup func(ctx context.Context, provider, providerRef string) (invoiceID string, amountMinor int64, currency string, found bool, err error)

// ProcessResult is the outcome for one inbox event.
type ProcessResult struct {
	EventID   string
	Skipped   bool
	Duplicate bool
	InvoiceID string
}

// Processor turns inbox events into applied payments.
type Processor struct {
	pool    *store.Pool
	inbox   *Inbox
	gateway Gateway
	// Account is the configured gateway account. An event from another account
	// is refused rather than applied: a shared endpoint serving two accounts must
	// not cross-credit.
	Account string
	Applier Applier
	// Attempts resolves the recorded checkout attempt for a provider payment, so
	// the settled amount can be verified against what was asked for. When nil the
	// check is skipped, which is only appropriate for gateways that cannot report
	// an amount.
	Attempts AttemptLookup
}

// NewProcessor constructs a Processor.
func NewProcessor(pool *store.Pool, inbox *Inbox, gateway Gateway, account string, applier Applier) *Processor {
	return &Processor{pool: pool, inbox: inbox, gateway: gateway, Account: account, Applier: applier}
}

// WithAttempts attaches the attempt lookup used for amount verification.
func (p *Processor) WithAttempts(lookup AttemptLookup) *Processor {
	p.Attempts = lookup
	return p
}

// Process handles pending events until none are left or the context is done.
func (p *Processor) Process(ctx context.Context, limit int) ([]ProcessResult, error) {
	events, err := p.inbox.ClaimPending(ctx, limit)
	if err != nil {
		return nil, err
	}

	results := make([]ProcessResult, 0, len(events))
	var processErr error
	for _, ev := range events {
		res, err := p.processOne(ctx, ev)
		if err != nil {
			// The event stays pending so it is retried and stays visible.
			if merr := p.inbox.MarkFailed(ctx, ev.ID, err); merr != nil {
				processErr = errors.Join(processErr, merr)
			}
			processErr = errors.Join(processErr, fmt.Errorf("event %s: %w", ev.EventID, err))
			continue
		}
		if err := p.inbox.MarkProcessed(ctx, ev.ID); err != nil {
			processErr = errors.Join(processErr, err)
			continue
		}
		results = append(results, res)
	}
	return results, processErr
}

// processOne verifies and applies a single event.
func (p *Processor) processOne(ctx context.Context, ev PendingEvent) (ProcessResult, error) {
	out := ProcessResult{EventID: ev.EventID}

	if p.Account != "" && ev.ProviderAccount != p.Account {
		return out, fmt.Errorf("%w: event account %q, configured %q",
			ErrProviderMismatch, ev.ProviderAccount, p.Account)
	}

	// Only payment-settled events require action. A provider emits many other
	// events; treating them as errors would fill the retry queue with noise.
	//
	// The kinds are the core's own vocabulary, not a provider's: an extension normalises
	// "checkout.session.completed" into EventPaymentSucceeded, because the core has no
	// provider knowledge and must not grow any.
	if !IsSettlementKind(ev.EventType) {
		if ev.PaymentID != "" {
			// The verifier named a settled payment under a kind the core does not recognise.
			// Skipping would mark the event processed and leave the invoice unpaid with no
			// signal anywhere; keeping it pending makes the mismatch visible in the inbox,
			// where an operator (or the extension's own fix) can act on it.
			return out, fmt.Errorf("%w: event %s names payment %s but type %q is not a settlement kind",
				ErrUnsupportedEvent, ev.EventID, ev.PaymentID, ev.EventType)
		}
		out.Skipped = true
		return out, nil
	}

	paymentID, err := p.paymentIDOf(ctx, ev)
	if err != nil {
		return out, err
	}

	// The provider's state is authoritative: an event saying "succeeded" is
	// re-checked, so a forged or mis-ordered delivery cannot settle an invoice.
	payment, err := p.gateway.QueryPayment(ctx, paymentID)
	if err != nil {
		return out, fmt.Errorf("query payment %s: %w", paymentID, err)
	}
	if payment.Status != extproto.PaymentSucceeded {
		return out, fmt.Errorf("%w: %s is %q", ErrNotPaid, paymentID, payment.Status)
	}
	amount, err := payment.Amount.Money()
	if err != nil {
		return out, fmt.Errorf("payment %s amount: %w", paymentID, err)
	}

	// The invoice is carried in the checkout metadata, which the core set when it
	// created the session. It is not taken from the webhook body: that would let
	// a provider payload (or a forged one) choose which invoice to settle.
	invoiceID := payment.Metadata["invoice_id"]
	if invoiceID == "" {
		return out, fmt.Errorf("%w: payment %s", ErrMissingInvoice, paymentID)
	}
	out.InvoiceID = invoiceID

	// Verify the settled amount against what the customer was asked to pay. The
	// provider's own numbers are used for the comparison, not the webhook body's.
	if p.Attempts != nil {
		attemptInvoice, wantMinor, wantCurrency, found, err := p.Attempts(ctx, ev.Provider, paymentID)
		if err != nil {
			return out, err
		}
		if !found {
			// A payment with no recorded attempt was not started by this panel.
			// Applying it would credit an invoice on an unverifiable basis.
			return out, fmt.Errorf("%w: payment %s has no recorded checkout", ErrAmountMismatch, paymentID)
		}
		if attemptInvoice != invoiceID {
			return out, fmt.Errorf("%w: payment %s was opened for invoice %s but reports %s",
				ErrAmountMismatch, paymentID, attemptInvoice, invoiceID)
		}
		if amount.Minor() != wantMinor || !strings.EqualFold(amount.Currency().Code(), wantCurrency) {
			return out, fmt.Errorf("%w: payment %s settled %s, expected %d %s",
				ErrAmountMismatch, paymentID, amount.Format(), wantMinor, wantCurrency)
		}
	}

	var applied ApplyResult
	err = p.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		applied, err = p.Applier.ApplySettledPayment(ctx, tx, SettledPayment{
			Provider:          ev.Provider,
			ProviderAccount:   ev.ProviderAccount,
			ProviderPaymentID: paymentID,
			InvoiceID:         invoiceID,
			AmountMinor:       amount.Minor(),
			Currency:          amount.Currency().Code(),
		})
		return err
	})
	if err != nil {
		return out, err
	}
	out.Duplicate = applied.Duplicate
	return out, nil
}

// paymentIDOf resolves the provider payment id an event is about.
//
// The normalised id stored at ingest time is authoritative. The body is only a fallback, for
// rows written before that column existed (and for verifiers whose normalisation is the body
// itself). Parsing the body is not a substitute: a real provider nests its payment several
// levels deep under its own key names, and re-deriving it here would put provider knowledge
// back in the core — the thing the extension boundary exists to prevent. That mistake is what
// made the first real checkout fail with "event … carries no payment id" after the delivery
// had already been acknowledged.
func (p *Processor) paymentIDOf(_ context.Context, ev PendingEvent) (string, error) {
	if ev.PaymentID != "" {
		return ev.PaymentID, nil
	}
	var body struct {
		PaymentID string `json:"payment_id"`
	}
	if err := json.Unmarshal(ev.Raw, &body); err != nil {
		return "", fmt.Errorf("payments: event %s body is not JSON: %w", ev.EventID, err)
	}
	if body.PaymentID == "" {
		return "", fmt.Errorf("%w: event %s (neither the verifier nor the stored body named one; a provider resend repairs the row)",
			ErrMissingPaymentID, ev.EventID)
	}
	return body.PaymentID, nil
}
