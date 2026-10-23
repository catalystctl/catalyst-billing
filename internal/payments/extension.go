package payments

// The extension-protocol side of an off-session charge.
//
// OffSessionCharger is the domain's seam; this file is the production implementation over
// the protocol's generic Call. The method names and payloads live here rather than in
// internal/extproto because the protocol's central spec is a separate, shared surface: this
// feature is additive and is advertised through the handshake capability list, so an
// extension that does not implement it is simply never called.
//
// The names below are the contract with extensions/stripe. They are strings on the wire, so
// a typo here is a method-not-found at the adapter rather than a compile error in the core;
// the adapter's own tests assert the same names.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/extproto"
)

// Gateway method names used by the billing-agreement feature.
const (
	MethodGatewayCreateBillingAgreement = "gateway.create_billing_agreement"
	MethodGatewayQueryBillingAgreement  = "gateway.query_billing_agreement"
	MethodGatewayCancelBillingAgreement = "gateway.cancel_billing_agreement"
	MethodGatewayCharge                 = "gateway.charge"
	// CapGatewayCharge is the handshake capability an adapter advertises when it can charge
	// a stored method. It is the same string as the method, which is the protocol's rule.
	CapGatewayCharge = MethodGatewayCharge
)

// Metadata keys an adapter may add to a query_payment result to report its fee without a new
// protocol field. They are namespaced so they cannot collide with a provider's own metadata.
const (
	MetadataFeeMinor    = "catalyst_fee_minor"
	MetadataFeeCurrency = "catalyst_fee_currency"
)

// CreateBillingAgreementParams starts a hosted flow that collects a reusable method.
type CreateBillingAgreementParams struct {
	OperationKey string            `json:"operation_key"`
	CustomerID   string            `json:"customer_id"`
	SuccessURL   string            `json:"success_url,omitempty"`
	CancelURL    string            `json:"cancel_url,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	Deadline     time.Time         `json:"deadline,omitzero"`
}

// SetDeadline implements the interface the extproto client uses to stamp every outgoing
// payload with the effective deadline.
func (p *CreateBillingAgreementParams) SetDeadline(t time.Time) { p.Deadline = t }

// OperationKeyValue exposes the idempotency token to the client, which refuses a mutating
// call without one.
func (p *CreateBillingAgreementParams) OperationKeyValue() string { return p.OperationKey }

// CreateBillingAgreementResult is the hosted mandate the provider created.
type CreateBillingAgreementResult struct {
	AgreementID string    `json:"agreement_id"`
	CheckoutURL string    `json:"checkout_url,omitempty"`
	Status      string    `json:"status"`
	ExpiresAt   time.Time `json:"expires_at,omitzero"`
}

// QueryBillingAgreementParams reads the state of one hosted mandate.
type QueryBillingAgreementParams struct {
	AgreementID string    `json:"agreement_id"`
	Deadline    time.Time `json:"deadline,omitzero"`
}

// SetDeadline implements the protocol's call metadata.
func (p *QueryBillingAgreementParams) SetDeadline(t time.Time) { p.Deadline = t }

// QueryBillingAgreementResult is the observed state. MethodToken is a credential and never
// leaves the core's storage layer.
type QueryBillingAgreementResult struct {
	AgreementID        string    `json:"agreement_id"`
	Status             string    `json:"status"`
	MethodToken        string    `json:"method_token,omitempty"`
	ProviderCustomerID string    `json:"provider_customer_id,omitempty"`
	Brand              string    `json:"brand,omitempty"`
	Last4              string    `json:"last4,omitempty"`
	ExpiryMonth        int       `json:"expiry_month,omitempty"`
	ExpiryYear         int       `json:"expiry_year,omitempty"`
	CreatedAt          time.Time `json:"created_at,omitzero"`
}

// CancelBillingAgreementParams revokes a mandate.
type CancelBillingAgreementParams struct {
	OperationKey string    `json:"operation_key"`
	AgreementID  string    `json:"agreement_id,omitempty"`
	MethodToken  string    `json:"method_token,omitempty"`
	Deadline     time.Time `json:"deadline,omitzero"`
}

// SetDeadline implements the protocol's call metadata.
func (p *CancelBillingAgreementParams) SetDeadline(t time.Time) { p.Deadline = t }

// OperationKeyValue exposes the idempotency token to the client.
func (p *CancelBillingAgreementParams) OperationKeyValue() string { return p.OperationKey }

// CancelBillingAgreementResult reports whether anything changed.
type CancelBillingAgreementResult struct {
	AgreementID string `json:"agreement_id"`
	Changed     bool   `json:"changed"`
}

// ChargeParams charges a stored method with no customer present.
type ChargeParams struct {
	OperationKey       string            `json:"operation_key"`
	CustomerID         string            `json:"customer_id"`
	MethodToken        string            `json:"method_token"`
	ProviderCustomerID string            `json:"provider_customer_id,omitempty"`
	Amount             WireAmount        `json:"amount"`
	Description        string            `json:"description,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	Deadline           time.Time         `json:"deadline,omitzero"`
}

// WireAmount is the protocol's money shape: integer minor units plus a canonical code.
type WireAmount struct {
	Minor    int64  `json:"minor"`
	Currency string `json:"currency"`
}

// SetDeadline implements the protocol's call metadata.
func (p *ChargeParams) SetDeadline(t time.Time) { p.Deadline = t }

// OperationKeyValue exposes the idempotency token to the client.
func (p *ChargeParams) OperationKeyValue() string { return p.OperationKey }

// ChargeResult is the provider's answer to an off-session charge.
type ChargeResult struct {
	PaymentID      string      `json:"payment_id"`
	Status         string      `json:"status"`
	Amount         WireAmount  `json:"amount"`
	Fee            *WireAmount `json:"fee,omitempty"`
	FailureCode    string      `json:"failure_code,omitempty"`
	FailureMessage string      `json:"failure_message,omitempty"`
}

// GatewayCaller is the one method of extproto.Client this package needs.
type GatewayCaller interface {
	Call(ctx context.Context, method string, params, result any) error
}

// ExtensionCharger charges a saved method through a gateway extension.
type ExtensionCharger struct {
	Caller   GatewayCaller
	Provider string
}

// Name implements OffSessionCharger.
func (c ExtensionCharger) Name() string { return c.Provider }

// ChargeOffSession implements OffSessionCharger.
//
// The amount, currency and idempotency key pass through unchanged: the adapter is not
// allowed to choose either the amount or an identity, because both decide what the customer
// is charged and whether a retry is a second charge.
func (c ExtensionCharger) ChargeOffSession(ctx context.Context, req OffSessionCharge) (OffSessionResult, error) {
	if c.Caller == nil {
		return OffSessionResult{}, ErrOffSessionUnsupported
	}
	var out ChargeResult
	err := c.Caller.Call(ctx, MethodGatewayCharge, &ChargeParams{
		OperationKey:       req.OperationKey,
		CustomerID:         req.CustomerID,
		MethodToken:        req.ProviderToken,
		ProviderCustomerID: req.ProviderCustomerID,
		Amount:             WireAmount{Minor: req.AmountMinor, Currency: req.Currency},
		Description:        req.Description,
		Metadata: map[string]string{
			"invoice_id":   req.InvoiceID,
			"agreement_id": req.AgreementID,
		},
	}, &out)
	if err != nil {
		if errors.Is(err, extproto.ErrRejected) {
			// A refusal on the merits is a decline, not a transport failure. Turning it
			// into a result here is what lets the invoice stay open with the provider's
			// own explanation instead of looking like a charge whose outcome is unknown.
			return declineResult(err), nil
		}
		return OffSessionResult{}, fmt.Errorf("payments: charge saved method: %w", err)
	}
	res := OffSessionResult{
		ProviderPaymentID: out.PaymentID,
		Status:            out.Status,
		AmountMinor:       out.Amount.Minor,
		FailureCode:       out.FailureCode,
		FailureMessage:    out.FailureMessage,
	}
	if out.Fee != nil {
		res.FeeMinor = out.Fee.Minor
	}
	return res, nil
}

// declineResult translates an extension's rejected call into the domain's decline shape.
//
// The code and message are the provider's: a customer told "card_declined" can act on it,
// while one told "stripe: -32005" cannot.
func declineResult(err error) OffSessionResult {
	res := OffSessionResult{Status: "failed", FailureCode: "declined"}
	var remote *extproto.Error
	if errors.As(err, &remote) {
		res.FailureMessage = remote.Message
		var data struct {
			Code        string `json:"code"`
			DeclineCode string `json:"decline_code"`
		}
		if len(remote.Data) > 0 && json.Unmarshal(remote.Data, &data) == nil {
			res.FailureCode = data.DeclineCode
			if res.FailureCode == "" {
				res.FailureCode = data.Code
			}
		}
	}
	if res.FailureMessage == "" {
		res.FailureMessage = err.Error()
	}
	if res.FailureCode == "" {
		res.FailureCode = "declined"
	}
	return res
}

// metadataFee reads a fee an adapter reported through query_payment metadata. The protocol
// has no fee field, so the adapter puts the provider's own numbers under namespaced keys; a
// value that does not parse is ignored rather than turned into a zero-cost payment.
func metadataFee(metadata map[string]string) (int64, string, bool) {
	raw := strings.TrimSpace(metadata[MetadataFeeMinor])
	if raw == "" {
		return 0, "", false
	}
	minor, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || minor <= 0 {
		return 0, "", false
	}
	return minor, strings.TrimSpace(metadata[MetadataFeeCurrency]), true
}
