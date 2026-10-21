package extproto

import (
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/money"
)

// Protocol is the protocol name sent in initialize. It is versioned separately
// from the individual method payloads so that a breaking envelope change does
// not silently look like a payload change.
const Protocol = "billing.ext/1"

// Version is the protocol version this core implements.
const Version = 1

// Method names. The gateway and provisioner method names double as capability
// names: an extension that lists "provisioner.suspend" in its handshake promises
// to implement exactly that method.
const (
	MethodInitialize = "initialize"
	MethodHealth     = "health"

	MethodGatewayDescribe       = "gateway.describe"
	MethodGatewayCreateCheckout = "gateway.create_checkout"
	MethodGatewayQueryPayment   = "gateway.query_payment"
	MethodGatewayRefund         = "gateway.refund"
	MethodGatewayVerifyWebhook  = "gateway.verify_webhook"

	MethodProvisionerDescribe = "provisioner.describe"
	MethodProvisionerValidate = "provisioner.validate"
	MethodProvisionerCreate   = "provisioner.create"
	MethodProvisionerInspect  = "provisioner.inspect"
	// MethodProvisionerLookup answers "does a resource already exist for this
	// operation key?". It is what makes reconciliation authoritative: replaying
	// create only works if the adapter still remembers the key, and after a
	// restart it may not.
	MethodProvisionerLookup    = "provisioner.lookup"
	MethodProvisionerSuspend   = "provisioner.suspend"
	MethodProvisionerResume    = "provisioner.resume"
	MethodProvisionerTerminate = "provisioner.terminate"
)

// CancelMethod is the notification the core sends when it abandons a call. The
// extension should stop working on the request with the matching id; a late
// response for that id is discarded by the core.
const CancelMethod = "$/cancelRequest"

// Capability names, for extensions that want to spell them out.
const (
	CapGatewayDescribe       = MethodGatewayDescribe
	CapGatewayCreateCheckout = MethodGatewayCreateCheckout
	CapGatewayQueryPayment   = MethodGatewayQueryPayment
	CapGatewayRefund         = MethodGatewayRefund
	CapGatewayVerifyWebhook  = MethodGatewayVerifyWebhook

	CapProvisionerDescribe  = MethodProvisionerDescribe
	CapProvisionerValidate  = MethodProvisionerValidate
	CapProvisionerCreate    = MethodProvisionerCreate
	CapProvisionerInspect   = MethodProvisionerInspect
	CapProvisionerLookup    = MethodProvisionerLookup
	CapProvisionerSuspend   = MethodProvisionerSuspend
	CapProvisionerResume    = MethodProvisionerResume
	CapProvisionerTerminate = MethodProvisionerTerminate
)

// Amount is the wire form of a monetary value: an exact count of minor units
// plus the ISO 4217 currency code. Binary floating point never appears in this
// protocol.
type Amount struct {
	// Minor is the value in the currency's minor unit (cents, yen, fils).
	Minor int64 `json:"minor"`
	// Currency is the upper-case ISO 4217 code, e.g. "USD".
	Currency string `json:"currency"`
}

// AmountOf converts a money.Amount into its wire form.
func AmountOf(a money.Amount) Amount {
	return Amount{Minor: a.Minor(), Currency: a.Currency().Code()}
}

// Money converts the wire form back into an exact money.Amount.
//
// The wire carries only the code, not the minor-unit exponent, so the exponent
// is taken from the ISO 4217 table. Codes outside that table are rejected rather
// than guessed: guessing a scale silently changes the value by orders of
// magnitude.
func (a Amount) Money() (money.Amount, error) {
	cur, ok := money.LookupCurrency(a.Currency)
	if !ok {
		return money.Amount{}, fmt.Errorf("%w: unknown ISO 4217 currency %q", ErrInvalidParams, a.Currency)
	}
	return money.New(a.Minor, cur), nil
}

// Validate reports whether the amount is representable: a known currency code
// written in canonical upper case.
func (a Amount) Validate() error {
	if a.Currency != strings.TrimSpace(a.Currency) || a.Currency != strings.ToUpper(a.Currency) {
		return fmt.Errorf("%w: currency %q is not canonical upper-case", ErrInvalidParams, a.Currency)
	}
	if _, ok := money.LookupCurrency(a.Currency); !ok {
		return fmt.Errorf("%w: unknown ISO 4217 currency %q", ErrInvalidParams, a.Currency)
	}
	return nil
}

// CallMeta is embedded in every request payload. It carries the per-call
// metadata the protocol requires: the deadline the extension should honour.
//
// The client fills Deadline in automatically when the payload is passed as a
// pointer, so callers normally leave it zero.
type CallMeta struct {
	Deadline time.Time `json:"deadline,omitzero"`
}

// SetDeadline implements the interface the client uses to stamp every outgoing
// payload with the effective deadline.
func (m *CallMeta) SetDeadline(t time.Time) { m.Deadline = t }

// MutatingMeta is embedded in every payload that changes remote state. The
// operation key is the caller's idempotency token: replaying the same key must
// return the result of the first execution, never perform a second one.
type MutatingMeta struct {
	CallMeta
	OperationKey string `json:"operation_key"`
}

// OperationKeyValue exposes the key to the client, which refuses to send a
// mutating call without one.
func (m *MutatingMeta) OperationKeyValue() string { return m.OperationKey }

// CancelParams is the payload of a $/cancelRequest notification. The extension
// should stop working on the request with the given id; the core discards any
// reply that arrives afterwards.
type CancelParams struct {
	// ID is the id of the abandoned request.
	ID uint64 `json:"id"`
	// Method is the abandoned method, for the extension's logs.
	Method string `json:"method,omitempty"`
	// Reason explains why the core gave up.
	Reason string `json:"reason,omitempty"`
}

// ValidateOperationKey rejects keys the protocol cannot use as idempotency
// tokens.
func ValidateOperationKey(key string) error {
	switch {
	case key == "":
		return fmt.Errorf("%w: operation_key is required", ErrInvalidParams)
	case len(key) > 200:
		return fmt.Errorf("%w: operation_key longer than 200 bytes", ErrInvalidParams)
	case strings.TrimSpace(key) != key:
		return fmt.Errorf("%w: operation_key has surrounding whitespace", ErrInvalidParams)
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: operation_key contains a control character", ErrInvalidParams)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// initialize / health
// ---------------------------------------------------------------------------

// InitializeParams is the handshake request. The core offers every version it
// speaks; the extension picks one.
type InitializeParams struct {
	CallMeta
	// Protocol is the protocol family name, always Protocol.
	Protocol string `json:"protocol"`
	// Versions are the protocol versions the core supports, newest first.
	Versions []int `json:"versions"`
	// CoreVersion is the billing core's build version, for diagnostics only.
	CoreVersion string `json:"core_version"`
}

// InitializeResult is the handshake reply.
type InitializeResult struct {
	// Version is the selected protocol version; it must be one the core offered.
	Version int `json:"version"`
	// Name identifies the extension, e.g. "stripe-gateway".
	Name string `json:"name"`
	// Publisher identifies who ships it.
	Publisher string `json:"publisher"`
	// Capabilities lists the method names the extension implements.
	Capabilities []string `json:"capabilities"`
}

// HasCapability reports whether the handshake advertised method.
func (r InitializeResult) HasCapability(method string) bool {
	for _, c := range r.Capabilities {
		if c == method {
			return true
		}
	}
	return false
}

// HealthParams is a liveness probe. It is the only call besides initialize that
// may be made before the handshake completes.
type HealthParams struct {
	CallMeta
}

// HealthResult is the liveness answer.
type HealthResult struct {
	// Status is "ok" for a usable extension; anything else is treated as
	// unhealthy by the supervisor.
	Status string `json:"status"`
	// Detail is optional human-readable context, e.g. "upstream unreachable".
	Detail string `json:"detail,omitempty"`
}

// StatusOK is the HealthResult.Status of a usable extension.
const StatusOK = "ok"

// ---------------------------------------------------------------------------
// gateway
// ---------------------------------------------------------------------------

// PaymentStatus is the lifecycle state of a payment.
type PaymentStatus string

// Payment lifecycle states.
const (
	PaymentPending   PaymentStatus = "pending"
	PaymentSucceeded PaymentStatus = "succeeded"
	PaymentFailed    PaymentStatus = "failed"
	PaymentCanceled  PaymentStatus = "canceled"
	PaymentRefunded  PaymentStatus = "refunded"
)

// PaymentMethod is one way of paying that the gateway offers.
type PaymentMethod struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// GatewayDescribeParams asks a gateway what it can do.
type GatewayDescribeParams struct {
	CallMeta
}

// GatewayDescribeResult is a gateway's self-description.
type GatewayDescribeResult struct {
	Name             string          `json:"name"`
	Currencies       []string        `json:"currencies"`
	Methods          []PaymentMethod `json:"methods"`
	SupportsRefunds  bool            `json:"supports_refunds"`
	SupportsWebhooks bool            `json:"supports_webhooks"`
	// MinAmount/MaxAmount bound a single checkout when the gateway has limits.
	MinAmount *Amount `json:"min_amount,omitempty"`
	MaxAmount *Amount `json:"max_amount,omitempty"`
}

// CreateCheckoutParams starts a payment. It is mutating: replaying the operation
// key returns the original checkout instead of creating a second one.
type CreateCheckoutParams struct {
	MutatingMeta
	// CustomerID is the core's customer identifier, echoed back in webhooks.
	CustomerID string `json:"customer_id"`
	// Amount is what the customer must pay.
	Amount Amount `json:"amount"`
	// Description is shown to the customer, e.g. "Pro plan, March".
	Description string `json:"description,omitempty"`
	// SuccessURL and CancelURL are where the gateway returns the customer.
	SuccessURL string `json:"success_url,omitempty"`
	CancelURL  string `json:"cancel_url,omitempty"`
	// Metadata is opaque to the gateway and returned with the payment.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// CreateCheckoutResult is the created checkout session.
type CreateCheckoutResult struct {
	PaymentID string `json:"payment_id"`
	// CheckoutURL is where the customer completes the payment.
	CheckoutURL string `json:"checkout_url"`
	// Status is the payment state right after creation, normally "pending".
	Status PaymentStatus `json:"status"`
	// ExpiresAt is when the checkout session stops being payable.
	ExpiresAt time.Time `json:"expires_at"`
}

// QueryPaymentParams asks for the current state of one payment. It is a pure
// read, so it carries no operation key.
type QueryPaymentParams struct {
	CallMeta
	PaymentID string `json:"payment_id"`
}

// QueryPaymentResult is the current state of a payment.
type QueryPaymentResult struct {
	PaymentID string        `json:"payment_id"`
	Status    PaymentStatus `json:"status"`
	// Amount is what was charged.
	Amount Amount `json:"amount"`
	// AmountRefunded is how much of it has been given back.
	AmountRefunded Amount            `json:"amount_refunded"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

// RefundParams gives money back. It is mutating and idempotent by operation key:
// a retried refund must not refund twice.
type RefundParams struct {
	MutatingMeta
	PaymentID string `json:"payment_id"`
	// Amount is the part to refund; nil means the full outstanding amount.
	Amount *Amount `json:"amount,omitempty"`
	Reason string  `json:"reason,omitempty"`
}

// RefundResult is the recorded refund.
type RefundResult struct {
	RefundID   string        `json:"refund_id"`
	PaymentID  string        `json:"payment_id"`
	Status     PaymentStatus `json:"status"`
	Amount     Amount        `json:"amount"`
	RefundedAt time.Time     `json:"refunded_at"`
}

// VerifyWebhookParams asks the extension to authenticate and decode an inbound
// webhook. It does not mutate remote state (the core applies the event), so it
// carries no operation key.
type VerifyWebhookParams struct {
	CallMeta
	// Headers are the request headers, canonicalised by the HTTP layer.
	Headers map[string]string `json:"headers"`
	// Body is the raw request body; it travels base64-encoded in JSON.
	Body []byte `json:"body"`
}

// VerifyWebhookResult is the verified event. Valid=false means the signature did
// not check out and Reason says why; that is not a transport error.
type VerifyWebhookResult struct {
	Valid     bool    `json:"valid"`
	Reason    string  `json:"reason,omitempty"`
	EventID   string  `json:"event_id,omitempty"`
	EventType string  `json:"event_type,omitempty"`
	PaymentID string  `json:"payment_id,omitempty"`
	Amount    *Amount `json:"amount,omitempty"`
	// OccurredAt is the gateway's timestamp for the event.
	OccurredAt time.Time `json:"occurred_at,omitzero"`
}

// ---------------------------------------------------------------------------
// provisioner
// ---------------------------------------------------------------------------

// ResourceStatus is the lifecycle state of a provisioned resource.
type ResourceStatus string

// Resource lifecycle states.
const (
	ResourceProvisioning ResourceStatus = "provisioning"
	ResourceActive       ResourceStatus = "active"
	ResourceSuspended    ResourceStatus = "suspended"
	ResourceTerminated   ResourceStatus = "terminated"
)

// ProvisionPlan is one sellable size of a provisioned resource.
type ProvisionPlan struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Amount Amount `json:"amount"`
	// Period is the billing period the price refers to: "monthly" or "annual".
	Period string `json:"period"`
}

// ProvisionerDescribeParams asks a provisioner what it can do.
type ProvisionerDescribeParams struct {
	CallMeta
}

// ProvisionerDescribeResult is a provisioner's self-description.
type ProvisionerDescribeResult struct {
	Name    string          `json:"name"`
	Plans   []ProvisionPlan `json:"plans"`
	Regions []string        `json:"regions"`
}

// ProvisionValidateParams checks a prospective purchase without creating
// anything. It is a pure read.
type ProvisionValidateParams struct {
	CallMeta
	PlanID   string `json:"plan_id"`
	Region   string `json:"region"`
	Quantity int32  `json:"quantity"`
}

// ProvisionValidateResult reports whether the purchase could be made. Valid=false
// with Problems filled in is a normal answer, not an error.
type ProvisionValidateResult struct {
	Valid    bool     `json:"valid"`
	Problems []string `json:"problems,omitempty"`
	// Amount is the price the provisioner would charge, when it can compute one.
	Amount *Amount `json:"amount,omitempty"`
}

// ProvisionCreateParams buys a resource. It is the call that makes idempotency
// non-negotiable: a retry after a timeout must return the server that already
// exists rather than buy a second one.
type ProvisionCreateParams struct {
	MutatingMeta
	// SubscriptionID is the core's subscription identifier.
	SubscriptionID string            `json:"subscription_id"`
	PlanID         string            `json:"plan_id"`
	Region         string            `json:"region"`
	Quantity       int32             `json:"quantity"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

// ProvisionCreateResult identifies the provisioned resource.
type ProvisionCreateResult struct {
	// ExternalID is the provisioner's stable identifier for the resource.
	ExternalID string         `json:"external_id"`
	Status     ResourceStatus `json:"status"`
	PlanID     string         `json:"plan_id"`
	Region     string         `json:"region"`
	CreatedAt  time.Time      `json:"created_at"`
}

// ProvisionLookupParams asks whether a resource already exists for an operation
// key.
//
// This is the authoritative reconciliation question. It is a read, not a create
// replay: replaying create only returns the existing resource while the adapter
// still remembers the key, and after a restart it may not, which is how a
// reconciliation can provision a second server.
type ProvisionLookupParams struct {
	OperationKey string `json:"operation_key"`
}

// ProvisionLookupResult is the answer.
//
// Found=false means the adapter is *certain* no resource exists for the key. An
// adapter that cannot answer must return an error instead, so the core never
// mistakes "I do not know" for "it does not exist".
type ProvisionLookupResult struct {
	Found      bool   `json:"found"`
	ExternalID string `json:"external_id,omitempty"`
	// Status is the adapter's own state name, when it has one.
	Status string `json:"status,omitempty"`
}

// ProvisionInspectParams looks up one resource. Pure read.
type ProvisionInspectParams struct {
	CallMeta
	ExternalID string `json:"external_id"`
}

// ProvisionInspectResult is the observed state of a resource.
type ProvisionInspectResult struct {
	ExternalID string         `json:"external_id"`
	Status     ResourceStatus `json:"status"`
	PlanID     string         `json:"plan_id"`
	Region     string         `json:"region"`
	CreatedAt  time.Time      `json:"created_at"`
	// SuspendedAt is set while Status is "suspended".
	SuspendedAt time.Time `json:"suspended_at,omitzero"`
}

// ProvisionSuspendParams stops a resource without destroying it. Mutating.
type ProvisionSuspendParams struct {
	MutatingMeta
	ExternalID string `json:"external_id"`
	Reason     string `json:"reason,omitempty"`
}

// ProvisionResumeParams restarts a suspended resource. Mutating.
type ProvisionResumeParams struct {
	MutatingMeta
	ExternalID string `json:"external_id"`
}

// ProvisionTerminateParams destroys a resource for good. Mutating and
// idempotent: terminating an already-terminated resource succeeds.
type ProvisionTerminateParams struct {
	MutatingMeta
	ExternalID string `json:"external_id"`
	Reason     string `json:"reason,omitempty"`
}

// ProvisionStateResult is the answer to suspend, resume and terminate.
type ProvisionStateResult struct {
	ExternalID string         `json:"external_id"`
	Status     ResourceStatus `json:"status"`
	// Changed is false when the call was a replay or the resource was already in
	// the requested state.
	Changed bool `json:"changed"`
}
