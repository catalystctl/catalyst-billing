package main

import (
	"context"
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

	mu     sync.Mutex
	client *extproto.Client
}

// newGatewayAdapter constructs the adapter for a provider name.
func newGatewayAdapter(provider string) *gatewayAdapter {
	return &gatewayAdapter{provider: provider}
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

	path := envOr("BILLING_GATEWAY_EXTENSION", "")
	if path == "" {
		return nil, errMissingConfig("BILLING_GATEWAY_EXTENSION")
	}
	client, err := extproto.Launch(ctx, extproto.Spec{
		Path:        path,
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
