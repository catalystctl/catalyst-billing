// Package provisioning: the calls a queued operation needs beyond create/suspend/resume/
// terminate, and how they reach an extension.
//
// # Why these are not methods on Adapter
//
// `Adapter` is the interface internal/catalog and the e2e harness implement against fakes, and
// the production implementation is built in cmd/billing around one long-lived extension client.
// Adding a required method to Adapter would break every one of those, and — worse — would make
// "this adapter cannot resize" a compile error instead of a runtime fact an operator can see.
//
// An adapter that cannot move a plan is not a broken adapter: a cPanel account and a
// Pterodactyl server have different notions of size, and the capability a protocol advertises
// is what the core should dispatch on. So `Upgrader`, `SubscriptionCanceller` and `FieldReader`
// are *optional* interfaces the worker type-asserts, and the fallback is an extension client
// launched from the panel's own configuration.
//
// # Why the fallback launches its own client
//
// The queue must be able to complete a resize even though the client cmd/billing holds predates
// the upgrade method. Launching the same binary the connection test proves reachable — through
// gatewayconfig.Launch, so the environment and the allowlist are the same — keeps one
// definition of "how this installation talks to its provisioner". It costs one process start
// per resize, and a resize is a rare, human- or invoice-triggered event.
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

// ErrNotConfigured is returned when the queue needs an extension the panel has not configured.
//
// It is separate from ErrUnsupportedOperation because the remedy is different: nothing is
// configured versus the configured adapter cannot do this. Both park the operation for review
// rather than retrying, because neither improves by being attempted again five seconds later.
var ErrNotConfigured = errors.New("provisioning: no integration is configured for this operation")

// Upgrader moves a live resource to another plan.
type Upgrader interface {
	Upgrade(ctx context.Context, req UpgradeRequest) (extproto.ProvisionUpgradeResult, error)
}

// UpgradeRequest is the adapter input for a resize.
type UpgradeRequest struct {
	// OperationKey is the queue row's identity, so a replay after a lost reply is the same
	// call rather than a second resize.
	OperationKey string
	ExternalID   string
	ServiceID    string
	// PlanID is the target. Empty means "the size this service already names", which is what
	// a plain price change (no plan change) sends.
	PlanID   string
	Region   string
	Quantity int32
	Metadata map[string]string
}

// SubscriptionCanceller ends a subscription the payment provider holds the record of.
type SubscriptionCanceller interface {
	CancelSubscription(ctx context.Context, req CancelSubscriptionRequest) (extproto.GatewayCancelSubscriptionResult, error)
}

// CancelSubscriptionRequest is the adapter input for a subscription cancellation.
type CancelSubscriptionRequest struct {
	OperationKey   string
	SubscriptionID string
	Reason         string
}

// FieldReader reports the inputs an adapter needs at purchase time.
type FieldReader interface {
	Fields(ctx context.Context) ([]extproto.ProvisionField, error)
}

// extensionCaller issues one call to a freshly launched, configured extension.
//
// It is the fallback for every optional adapter interface: it launches the extension the panel
// names for the given kind, checks the capability the call needs, and makes exactly that call.
type extensionCaller struct {
	pool    *store.Pool
	kind    gatewayconfig.Kind
	timeout time.Duration
}

// call launches the configured extension and invokes one method.
//
// The capability check is not ceremony: the protocol's promise is that a method name it did
// not advertise is not implemented, and calling it anyway would surface as a JSON-RPC
// "method not found" that reads like a transport fault instead of "this adapter cannot do
// this".
func (e extensionCaller) call(ctx context.Context, capability, method string, params, out any) error {
	cfg, err := gatewayconfig.New(e.pool).Load(ctx, e.kind)
	if err != nil {
		return fmt.Errorf("provisioning: read %s configuration: %w", e.kind, err)
	}
	if !cfg.Configured() {
		return fmt.Errorf("%w: configure a provider on the %s screen", ErrNotConfigured, e.kind)
	}
	client, err := gatewayconfig.Launch(ctx, cfg, e.timeout)
	if err != nil {
		// A launch failure is a deployment problem, not a remote effect: nothing was called,
		// so it is safe to retry once an operator fixes the path or the credentials.
		return fmt.Errorf("provisioning: launch %s: %w", cfg.Provider, err)
	}
	defer func() { _ = client.Close() }()

	if !client.Info().HasCapability(capability) {
		return fmt.Errorf("%w: the %s adapter does not advertise %s", ErrUnsupportedOperation,
			cfg.Provider, capability)
	}
	return client.Call(ctx, method, params, out)
}

// Upgrade implements Upgrader.
func (e extensionCaller) Upgrade(ctx context.Context, req UpgradeRequest) (extproto.ProvisionUpgradeResult, error) {
	var out extproto.ProvisionUpgradeResult
	err := e.call(ctx, extproto.CapProvisionerUpgrade, extproto.MethodProvisionerUpgrade,
		&extproto.ProvisionUpgradeParams{
			MutatingMeta: extproto.MutatingMeta{OperationKey: req.OperationKey},
			ExternalID:   req.ExternalID,
			PlanID:       req.PlanID,
			Region:       req.Region,
			Quantity:     req.Quantity,
			Metadata:     req.Metadata,
		}, &out)
	return out, err
}

// CancelSubscription implements SubscriptionCanceller.
func (e extensionCaller) CancelSubscription(ctx context.Context, req CancelSubscriptionRequest) (extproto.GatewayCancelSubscriptionResult, error) {
	var out extproto.GatewayCancelSubscriptionResult
	err := e.call(ctx, extproto.CapGatewayCancelSubscription, extproto.MethodGatewayCancelSubscription,
		&extproto.GatewayCancelSubscriptionParams{
			MutatingMeta:   extproto.MutatingMeta{OperationKey: req.OperationKey},
			SubscriptionID: req.SubscriptionID,
			Reason:         req.Reason,
		}, &out)
	return out, err
}

// Fields implements FieldReader.
func (e extensionCaller) Fields(ctx context.Context) ([]extproto.ProvisionField, error) {
	var out extproto.ProvisionFieldsResult
	err := e.call(ctx, extproto.CapProvisionerFields, extproto.MethodProvisionerFields,
		&extproto.ProvisionFieldsParams{}, &out)
	return out.Fields, err
}

// defaultCallTimeout bounds one launched-call round trip, including the process start and the
// handshake. It is shorter than the lease so a hung call is classified as unknown before the
// row can be reaped and retried underneath it.
const defaultCallTimeout = 25 * time.Second

// ProvisionerFields is what the configured provisioner says it needs at purchase time.
//
// `Supported` is false when the adapter does not implement `provisioner.fields`. That is not a
// failure: an adapter that needs nothing at checkout declares nothing, and an adapter that
// predates the method needs nothing either.
type ProvisionerFields struct {
	Provider     string
	ProviderName string
	// Extension is the adapter's own name from the handshake, which is what tells an operator
	// that the process answering is the binary they configured.
	Extension    string
	Capabilities []string
	Supported    bool
	Fields       []extproto.ProvisionField
}

// ReadProvisionerFields launches the configured provisioner and asks it which inputs it needs.
//
// It is a read: no settings row, no audit row, no queue row. The filter drops a malformed
// declaration rather than failing the screen, because one bad field must not hide the fields an
// operator can still fill in.
func ReadProvisionerFields(ctx context.Context, pool *store.Pool, timeout time.Duration) (ProvisionerFields, error) {
	if timeout <= 0 {
		timeout = defaultCallTimeout
	}
	cfg, err := gatewayconfig.New(pool).Load(ctx, gatewayconfig.ServerKind)
	if err != nil {
		return ProvisionerFields{}, fmt.Errorf("provisioning: read servers configuration: %w", err)
	}
	out := ProvisionerFields{Provider: cfg.Provider, ProviderName: cfg.ProviderName}
	if !cfg.Configured() {
		return out, fmt.Errorf("%w: configure a provider on the servers screen", ErrNotConfigured)
	}
	client, err := gatewayconfig.Launch(ctx, cfg, timeout)
	if err != nil {
		return out, fmt.Errorf("provisioning: launch %s: %w", cfg.Provider, err)
	}
	defer func() { _ = client.Close() }()

	handshake := client.Info()
	out.Extension = handshake.Name
	out.Capabilities = handshake.Capabilities
	if !handshake.HasCapability(extproto.CapProvisionerFields) {
		// The adapter answered the handshake and simply has no purchase-time inputs.
		return out, nil
	}

	var res extproto.ProvisionFieldsResult
	if err := client.Call(ctx, extproto.MethodProvisionerFields, &extproto.ProvisionFieldsParams{}, &res); err != nil {
		return out, fmt.Errorf("provisioner.fields: %w", err)
	}
	out.Supported = true
	for _, f := range res.Fields {
		if !f.Valid() {
			continue
		}
		out.Fields = append(out.Fields, f)
	}
	return out, nil
}

// upgrader resolves what a resize operation calls.
//
// The adapter comes first: when an Adapter also implements Upgrader, that is the same process,
// the same credentials and the same cache the rest of its operations use, and nothing should
// launch a second client behind its back. The injected seam is next (a test, or a deployment with
// its own host), and the launched extension is the fallback for the production Adapter, which is
// built around a client interface that predates provisioner.upgrade.
func (w *Worker) upgrader() (Upgrader, error) {
	if u, ok := w.adapter.(Upgrader); ok {
		return u, nil
	}
	if w.Upgrader != nil {
		return w.Upgrader, nil
	}
	if w.pool == nil {
		return nil, fmt.Errorf("%w: the worker has no pool to launch a provisioner from", ErrNotConfigured)
	}
	return extensionCaller{pool: w.pool, kind: gatewayconfig.ServerKind, timeout: w.callTimeout()}, nil
}

// canceller resolves what a subscription-cancellation operation calls. It is the *gateway*,
// not the provisioner: a hosting panel does not own a card subscription.
func (w *Worker) canceller() (SubscriptionCanceller, error) {
	if w.Canceller != nil {
		return w.Canceller, nil
	}
	if w.pool == nil {
		return nil, fmt.Errorf("%w: the worker has no pool to launch a gateway from", ErrNotConfigured)
	}
	return extensionCaller{pool: w.pool, kind: gatewayconfig.GatewayKind, timeout: w.callTimeout()}, nil
}

// callTimeout is the per-call budget, defaulted like NewWorker's.
func (w *Worker) callTimeout() time.Duration {
	if w.CallTimeout > 0 {
		return w.CallTimeout
	}
	return defaultCallTimeout
}
