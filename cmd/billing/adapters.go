package main

import (
	"context"
	"fmt"

	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/provisioning"
)

// errMissingConfig is a configuration error that names the variable to set.
type errMissingConfig string

func (e errMissingConfig) Error() string {
	return fmt.Sprintf("missing required configuration: %s", string(e))
}

// gatewayExtension adapts the extension client to payments.Gateway.
type gatewayExtension struct{ client *extproto.Client }

func (g gatewayExtension) Name() string { return g.client.Info().Name }

func (g gatewayExtension) QueryPayment(ctx context.Context, paymentID string) (extproto.QueryPaymentResult, error) {
	var out extproto.QueryPaymentResult
	err := g.client.Call(ctx, "gateway.query_payment",
		&extproto.QueryPaymentParams{PaymentID: paymentID}, &out)
	return out, err
}

// provExtension adapts the extension client to provisioning.Adapter.
type provExtension struct{ client *extproto.Client }

func (p provExtension) Name() string { return p.client.Info().Name }

func (p provExtension) Capabilities() []string { return p.client.Capabilities() }

func (p provExtension) Create(ctx context.Context, req provisioning.CreateRequest) (extproto.ProvisionCreateResult, error) {
	var out extproto.ProvisionCreateResult
	err := p.client.Call(ctx, "provisioner.create", &extproto.ProvisionCreateParams{
		MutatingMeta:   extproto.MutatingMeta{OperationKey: req.OperationKey},
		SubscriptionID: req.SubscriptionID,
		PlanID:         req.PlanID,
		Region:         req.Region,
		Quantity:       req.Quantity,
		Metadata:       req.Metadata,
	}, &out)
	return out, err
}

func (p provExtension) Lookup(ctx context.Context, operationKey string) (extproto.ProvisionLookupResult, error) {
	var out extproto.ProvisionLookupResult
	err := p.client.Call(ctx, extproto.MethodProvisionerLookup,
		&extproto.ProvisionLookupParams{OperationKey: operationKey}, &out)
	return out, err
}

func (p provExtension) Inspect(ctx context.Context, externalID string) (extproto.ProvisionInspectResult, error) {
	var out extproto.ProvisionInspectResult
	err := p.client.Call(ctx, "provisioner.inspect",
		&extproto.ProvisionInspectParams{ExternalID: externalID}, &out)
	return out, err
}

func (p provExtension) Suspend(ctx context.Context, externalID, reason string) (extproto.ProvisionStateResult, error) {
	var out extproto.ProvisionStateResult
	err := p.client.Call(ctx, "provisioner.suspend", &extproto.ProvisionSuspendParams{
		MutatingMeta: extproto.MutatingMeta{OperationKey: "suspend:" + externalID},
		ExternalID:   externalID,
		Reason:       reason,
	}, &out)
	return out, err
}

func (p provExtension) Resume(ctx context.Context, externalID string) (extproto.ProvisionStateResult, error) {
	var out extproto.ProvisionStateResult
	err := p.client.Call(ctx, "provisioner.resume", &extproto.ProvisionResumeParams{
		MutatingMeta: extproto.MutatingMeta{OperationKey: "resume:" + externalID},
		ExternalID:   externalID,
	}, &out)
	return out, err
}

func (p provExtension) Terminate(ctx context.Context, externalID, reason string) (extproto.ProvisionStateResult, error) {
	var out extproto.ProvisionStateResult
	err := p.client.Call(ctx, "provisioner.terminate", &extproto.ProvisionTerminateParams{
		MutatingMeta: extproto.MutatingMeta{OperationKey: "terminate:" + externalID},
		ExternalID:   externalID,
		Reason:       reason,
	}, &out)
	return out, err
}
