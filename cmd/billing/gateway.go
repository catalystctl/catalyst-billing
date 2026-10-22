package main

import (
	"context"
	"github.com/catalyst/billing/internal/gatewayconfig"
	"github.com/catalyst/billing/internal/store"
	"log/slog"
	"sync"
	"time"

	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/refunds"
)

// gatewayAdapter is the payment gateway as the server sees it.
//
// It owns a lazily-launched extension process, for the same reason the worker's cache does: the
// gateway is only needed when someone pays or refunds, and launching it at startup would make a
// missing extension a startup failure for a panel that can still serve invoices.
//
// A client whose process died stays dead (Err() is non-nil forever), so a dead client is discarded
// and a fresh process launched. Returning the dead one would turn a single crash into "payments are
// broken until restart".
type gatewayAdapter struct {
	provider string
	// path is the extension binary and env is the environment it starts with. They are resolved
	// once, at startup, so the checkout path never reads settings: a configuration change takes
	// effect on the next boot, which is the honest behaviour for the component that takes money.
	path string
	env  []string

	mu     sync.Mutex
	client *extproto.Client
}

// newGatewayAdapter constructs the adapter for a provider name.
func newGatewayAdapter(provider string) *gatewayAdapter {
	return &gatewayAdapter{provider: provider}
}

// withLaunch tells the adapter which binary to launch and what to start it with.
//
// Without this the adapter launches whatever BILLING_GATEWAY_EXTENSION names and nothing else,
// which is a gateway with no credentials: every charge fails with "SECRET_KEY is required" while
// the panel's own configuration screen reports a working gateway, because that screen resolves
// the configuration through internal/gatewayconfig and this path did not.
func (g *gatewayAdapter) withLaunch(path string, env []string) *gatewayAdapter {
	g.path = path
	g.env = env
	return g
}

// Name implements the checkout gateway interface.
func (g *gatewayAdapter) Name() string { return g.provider }

// clientOrLaunch returns a live client, launching one if needed.
func (g *gatewayAdapter) clientOrLaunch(ctx context.Context) (*extproto.Client, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.client != nil {
		if err := g.client.Err(); err == nil {
			return g.client, nil
		}
		_ = g.client.Close()
		g.client = nil
	}

	path := g.path
	if path == "" {
		path = envOr("BILLING_GATEWAY_EXTENSION", "")
	}
	if path == "" {
		return nil, errMissingConfig("BILLING_GATEWAY_EXTENSION")
	}
	client, err := extproto.Launch(ctx, extproto.Spec{
		Path: path,
		// The child inherits PATH and exactly these entries. This is the only channel an
		// extension has for its credentials.
		Env:         g.env,
		CoreVersion: version,
		CallTimeout: 30 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	g.client = client
	return client, nil
}

// CreateCheckout implements httpapi.CheckoutGateway.
func (g *gatewayAdapter) CreateCheckout(ctx context.Context, p extproto.CreateCheckoutParams) (extproto.CreateCheckoutResult, error) {
	client, err := g.clientOrLaunch(ctx)
	if err != nil {
		return extproto.CreateCheckoutResult{}, err
	}
	var out extproto.CreateCheckoutResult
	if err := client.Call(ctx, extproto.MethodGatewayCreateCheckout, &p, &out); err != nil {
		return extproto.CreateCheckoutResult{}, err
	}
	return out, nil
}

// Refund implements refunds.Gateway.
//
// It exists as a separate method rather than a generic Call so the refund service can be handed an
// interface it can fake: the branch that matters most in a refund is the one where the call times
// out, and that branch is untestable through a real process.
func (g *gatewayAdapter) Refund(ctx context.Context, p extproto.RefundParams) (extproto.RefundResult, error) {
	client, err := g.clientOrLaunch(ctx)
	if err != nil {
		return extproto.RefundResult{}, err
	}
	var out extproto.RefundResult
	if err := client.Call(ctx, extproto.MethodGatewayRefund, &p, &out); err != nil {
		return extproto.RefundResult{}, err
	}
	return out, nil
}

// The assertions are the contract: if either interface changes, this file fails to compile rather
// than silently serving a gateway that cannot refund.
var (
	_ refunds.Gateway = (*gatewayAdapter)(nil)
)

// configuredGateway builds the adapter with the launch configuration the operator saved.
//
// Resolution failure is logged and the adapter falls back to the environment alone rather than
// stopping the server: a panel that will not start because its gateway settings are half-filled
// cannot be used to fix them. The fallback is visible — the log line names what could not be
// resolved — and a charge attempted in that state fails with a clear message rather than a
// mysterious provider error.
func configuredGateway(ctx context.Context, pool *store.Pool, logger *slog.Logger, provider string) *gatewayAdapter {
	adapter := newGatewayAdapter(provider)
	if pool == nil {
		return adapter
	}
	cfg, err := gatewayconfig.New(pool).Load(ctx, gatewayconfig.GatewayKind)
	if err != nil {
		logger.Warn("gateway configuration could not be resolved; falling back to the environment",
			"error", err)
		return adapter
	}
	if cfg.Provider != "" {
		adapter.provider = cfg.Provider
	}
	return adapter.withLaunch(cfg.ExtensionPath, cfg.Env())
}
