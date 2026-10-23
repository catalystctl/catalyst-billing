// Package provisioning: the live view of a provisioned resource, and its power switch.
//
// # Why this lives beside extension.go rather than on Adapter
//
// `Adapter` is the interface internal/catalog and the e2e harness implement against fakes, and
// adding a required method to it would break every one of them — and would make "this adapter
// cannot report a running state" a compile error instead of a fact an operator can see. A cPanel
// account and a game server do not agree on what "running" means, so this is an optional
// capability: the worker and the HTTP layer type-assert, and an adapter that does not advertise it
// is simply not asked.
//
// # Why a read launches a process
//
// The same reason `ReadProvisionerFields` does. The panel's HTTP server does not hold a long-lived
// extension client — cmd/billing's worker does — so a request-path read launches the configured
// binary, handshakes, makes one call and exits. That costs a process start per page load, which is
// tens of milliseconds, and it buys the property that matters more: one definition of "how this
// installation talks to its provisioner", shared with the queue, with the same environment and the
// same allowlist. The alternative — a second, separately-configured HTTP client in the core —
// would be a second place for credentials to live and drift.

package provisioning

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/gatewayconfig"
	"github.com/catalyst/billing/internal/store"
)

// ResourceReader reports the live state of one provisioned resource.
//
// It is implemented by an adapter that advertises `provisioner.resource`. The bool in the return
// is not used by this package: an adapter that cannot answer says so by not advertising the
// capability, which is checked before the call.
type ResourceReader interface {
	Resource(ctx context.Context, externalID string) (extproto.ProvisionResourceResult, error)
}

// PowerController changes the power state of one provisioned resource.
type PowerController interface {
	Power(ctx context.Context, req PowerRequest) (extproto.ProvisionPowerResult, error)
}

// PowerRequest is the adapter input for a power action.
//
// OperationKey is the queue-independent idempotency token for this press. The HTTP layer derives it
// from the request's idempotency key, so a retried click returns the first result instead of
// restarting a server that already restarted.
type PowerRequest struct {
	OperationKey string
	ExternalID   string
	Action       extproto.PowerAction
}

// ResourceView is what the configured provisioner said about one resource.
//
// `Supported` is false when the adapter does not implement `provisioner.resource`. That is not a
// failure: an adapter that only creates and destroys declares nothing here, and the page says so
// rather than showing an error.
type ResourceView struct {
	Provider     string
	ProviderName string
	// Extension is the adapter's own name from the handshake, which tells an operator that the
	// process answering is the binary they configured.
	Extension string
	// Capabilities is the whole advertised set, for diagnostics.
	Capabilities []string
	Supported    bool
	Result       extproto.ProvisionResourceResult
}

// PowerView is the outcome of a power call, with the same provenance as ResourceView.
type PowerView struct {
	Provider  string
	Extension string
	Supported bool
	Result    extproto.ProvisionPowerResult
}

// ReadResource launches the configured provisioner and asks for one resource's live state.
//
// A resource the provider does not know is `extproto.ErrNotFound`, which the HTTP layer turns into
// a 404. That distinction matters to a customer: "your server is gone" and "we could not reach the
// panel" are different sentences, and only one of them is worth retrying.
func ReadResource(ctx context.Context, pool *store.Pool, timeout time.Duration, externalID string) (ResourceView, error) {
	if timeout <= 0 {
		timeout = defaultCallTimeout
	}
	if externalID == "" {
		return ResourceView{}, fmt.Errorf("%w: the service has no provisioned resource", ErrNotConfigured)
	}
	_, client, out, err := launchFor(ctx, pool, timeout)
	if err != nil {
		return out, err
	}
	defer func() { _ = client.Close() }()

	if !client.Info().HasCapability(extproto.CapProvisionerResource) {
		return out, nil
	}
	var res extproto.ProvisionResourceResult
	if err := client.Call(ctx, extproto.MethodProvisionerResource,
		&extproto.ProvisionResourceParams{ExternalID: externalID}, &res); err != nil {
		return out, fmt.Errorf("provisioner.resource: %w", err)
	}
	out.Supported = true
	out.Result = res
	return out, nil
}

// ControlPower launches the configured provisioner and issues one power action.
//
// The capability check comes first, so an adapter that cannot do this produces
// ErrUnsupportedOperation — "this provider has no power control" — rather than a JSON-RPC
// "method not found" that reads like a transport fault.
func ControlPower(ctx context.Context, pool *store.Pool, timeout time.Duration, req PowerRequest) (PowerView, error) {
	if timeout <= 0 {
		timeout = defaultCallTimeout
	}
	if err := (extproto.ProvisionPowerParams{
		MutatingMeta: extproto.MutatingMeta{OperationKey: req.OperationKey},
		ExternalID:   req.ExternalID,
		Action:       req.Action,
	}).Validate(); err != nil {
		return PowerView{}, fmt.Errorf("%w: %w", ErrInvalidPowerRequest, err)
	}
	_, client, view, err := launchFor(ctx, pool, timeout)
	if err != nil {
		return PowerView{Provider: view.Provider, Extension: view.Extension}, err
	}
	defer func() { _ = client.Close() }()

	if !client.Info().HasCapability(extproto.CapProvisionerPower) {
		return PowerView{Provider: view.Provider, Extension: view.Extension},
			fmt.Errorf("%w: the %s adapter does not advertise %s", ErrUnsupportedOperation,
				view.Provider, extproto.CapProvisionerPower)
	}
	var res extproto.ProvisionPowerResult
	if err := client.Call(ctx, extproto.MethodProvisionerPower, &extproto.ProvisionPowerParams{
		MutatingMeta: extproto.MutatingMeta{OperationKey: req.OperationKey},
		ExternalID:   req.ExternalID,
		Action:       req.Action,
	}, &res); err != nil {
		return PowerView{Provider: view.Provider, Extension: view.Extension}, fmt.Errorf("provisioner.power: %w", err)
	}
	return PowerView{Provider: view.Provider, Extension: view.Extension, Supported: true, Result: res}, nil
}

// ErrInvalidPowerRequest means the caller built a power request the protocol forbids.
//
// It is separate from ErrUnsupportedOperation because the remedy is a different party's: the caller
// sent an action outside the closed set, and no amount of configuration fixes it.
var ErrInvalidPowerRequest = errors.New("provisioning: invalid power request")

// launchFor resolves the configured provisioner and starts it, returning the provenance the
// caller's response needs.
//
// It is shared by the read and the write so both answer with the same provider name, and so a
// change to how a provisioner is launched cannot apply to one and not the other.
func launchFor(ctx context.Context, pool *store.Pool, timeout time.Duration) (gatewayconfig.Config, *extproto.Client, ResourceView, error) {
	if pool == nil {
		return gatewayconfig.Config{}, nil, ResourceView{},
			fmt.Errorf("%w: the server has no database pool", ErrNotConfigured)
	}
	cfg, err := gatewayconfig.New(pool).Load(ctx, gatewayconfig.ServerKind)
	if err != nil {
		return gatewayconfig.Config{}, nil, ResourceView{},
			fmt.Errorf("provisioning: read servers configuration: %w", err)
	}
	out := ResourceView{Provider: cfg.Provider, ProviderName: cfg.ProviderName}
	if !cfg.Configured() {
		return cfg, nil, out, fmt.Errorf("%w: configure a provider on the servers screen", ErrNotConfigured)
	}
	client, err := gatewayconfig.Launch(ctx, cfg, timeout)
	if err != nil {
		// Nothing was called, so this is safe to retry once an operator fixes the path or the
		// credentials.
		return cfg, nil, out, fmt.Errorf("provisioning: launch %s: %w", cfg.Provider, err)
	}
	out.Extension = client.Info().Name
	out.Capabilities = client.Info().Capabilities
	return cfg, client, out, nil
}
