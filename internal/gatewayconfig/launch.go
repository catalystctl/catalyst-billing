package gatewayconfig

import (
	"context"
	"fmt"
	"time"

	"github.com/catalyst/billing/internal/extproto"
)

// Launch starts the configured extension and performs the handshake.
//
// It is the one function the worker, the server and the connection test share, so the
// binary a test proves reachable is the binary a payment actually runs. A test that launched
// the extension a different way would be a test of the test.
//
// It refuses when there is nothing to launch, and it never silently falls back to the
// fixture: launching the wrong gateway is worse than refusing to take a payment.
func Launch(ctx context.Context, cfg Config, callTimeout time.Duration) (*extproto.Client, error) {
	if !cfg.Configured() {
		return nil, ErrNotConfigured
	}
	if cfg.ExtensionPath == "" {
		return nil, fmt.Errorf("%w: no extension binary is configured for %q", ErrNotConfigured, cfg.Provider)
	}
	if callTimeout <= 0 {
		callTimeout = 30 * time.Second
	}
	client, err := extproto.Launch(ctx, extproto.Spec{
		Path:        cfg.ExtensionPath,
		Env:         cfg.Env(),
		CallTimeout: callTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("gatewayconfig: launch %s: %w", cfg.Provider, err)
	}
	return client, nil
}

// Amount is the wire shape of a money value in a description: minor units plus the code,
// with a display string the panel can show without doing arithmetic of its own.
type Amount struct {
	Minor    int64  `json:"minor"`
	Currency string `json:"currency"`
	Display  string `json:"display"`
}

// amountOf renders a protocol amount. A code outside ISO 4217 is left undisplayed rather
// than guessed at: guessing a scale changes the value by orders of magnitude.
func amountOf(a extproto.Amount) *Amount {
	out := &Amount{Minor: a.Minor, Currency: a.Currency}
	if m, err := a.Money(); err == nil {
		out.Display = m.String()
	}
	return out
}

// Method is one way of paying the gateway offers.
type Method struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// GatewayDescription is what a connection test reports about a payment gateway.
type GatewayDescription struct {
	// Provider is the id the panel stores; ProviderName is the human name.
	Provider     string `json:"provider"`
	ProviderName string `json:"provider_name"`
	// Extension, Publisher and ProtocolVersion come from the protocol handshake, not from
	// describe: they are what proves the process that answered is an extension at all.
	Extension       string   `json:"extension"`
	Publisher       string   `json:"publisher"`
	ProtocolVersion int      `json:"protocol_version"`
	Capabilities    []string `json:"capabilities"`
	// Currencies are the codes the gateway will settle in. It is the list an operator
	// checks against the catalog's currencies.
	Currencies       []string `json:"currencies"`
	Methods          []Method `json:"methods"`
	SupportsRefunds  bool     `json:"supports_refunds"`
	SupportsWebhooks bool     `json:"supports_webhooks"`
	MinAmount        *Amount  `json:"min_amount,omitempty"`
	MaxAmount        *Amount  `json:"max_amount,omitempty"`
	// WebhookURL is where the provider must deliver events, repeated here so the test result
	// is self-contained.
	WebhookURL string    `json:"webhook_url,omitempty"`
	TestedAt   time.Time `json:"tested_at"`
	// DurationMS is how long the launch plus the handshake plus describe took. A gateway
	// that takes four seconds to describe will time out under load.
	DurationMS int64 `json:"duration_ms"`
}

// Plan is one sellable size the provisioner advertises.
type Plan struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Amount *Amount `json:"amount,omitempty"`
	Period string  `json:"period,omitempty"`
}

// ServerDescription is what a connection test reports about a provisioner.
type ServerDescription struct {
	Provider        string    `json:"provider"`
	ProviderName    string    `json:"provider_name"`
	Extension       string    `json:"extension"`
	Publisher       string    `json:"publisher"`
	ProtocolVersion int       `json:"protocol_version"`
	Capabilities    []string  `json:"capabilities"`
	Plans           []Plan    `json:"plans"`
	Regions         []string  `json:"regions"`
	TestedAt        time.Time `json:"tested_at"`
	DurationMS      int64     `json:"duration_ms"`
}

// TestGateway launches the configured gateway, calls gateway.describe and reports what it
// found.
//
// It is a read: it writes no settings row, no audit row and no queue entry. It deliberately
// works while the gateway is switched off, because testing before enabling is the point —
// an operator who has to turn payments on to find out whether the key is right has been
// asked to take a risk to answer a question.
func (r *Resolver) TestGateway(ctx context.Context, cfg Config, timeout time.Duration) (GatewayDescription, error) {
	if cfg.Kind != GatewayKind {
		return GatewayDescription{}, fmt.Errorf("gatewayconfig: %s is not a gateway", cfg.Provider)
	}
	started := time.Now()
	client, handshake, err := launchForTest(ctx, cfg, timeout, extproto.CapGatewayDescribe)
	if err != nil {
		return GatewayDescription{}, err
	}
	defer func() { _ = client.Close() }()

	var res extproto.GatewayDescribeResult
	if err := client.Call(ctx, extproto.MethodGatewayDescribe, &extproto.GatewayDescribeParams{}, &res); err != nil {
		return GatewayDescription{}, fmt.Errorf("gateway.describe: %w", err)
	}

	out := GatewayDescription{
		Provider:         cfg.Provider,
		ProviderName:     cfg.ProviderName,
		Extension:        handshake.Name,
		Publisher:        handshake.Publisher,
		ProtocolVersion:  handshake.Version,
		Capabilities:     nonNil(handshake.Capabilities),
		Currencies:       nonNil(res.Currencies),
		SupportsRefunds:  res.SupportsRefunds,
		SupportsWebhooks: res.SupportsWebhooks,
		WebhookURL:       cfg.WebhookURL,
		TestedAt:         time.Now().UTC(),
		DurationMS:       time.Since(started).Milliseconds(),
	}
	for _, m := range res.Methods {
		out.Methods = append(out.Methods, Method{ID: m.ID, Name: m.Name})
	}
	if out.Methods == nil {
		out.Methods = []Method{}
	}
	if res.MinAmount != nil {
		out.MinAmount = amountOf(*res.MinAmount)
	}
	if res.MaxAmount != nil {
		out.MaxAmount = amountOf(*res.MaxAmount)
	}
	// describe is the extension's own claim; the handshake is its identity. A describe that
	// names nothing still leaves the panel with a name to show.
	if out.Extension == "" {
		out.Extension = res.Name
	}
	return out, nil
}

// TestServer launches the configured provisioner, calls provisioner.describe and reports
// what it found.
func (r *Resolver) TestServer(ctx context.Context, cfg Config, timeout time.Duration) (ServerDescription, error) {
	if cfg.Kind != ServerKind {
		return ServerDescription{}, fmt.Errorf("gatewayconfig: %s is not a provisioner", cfg.Provider)
	}
	started := time.Now()
	client, handshake, err := launchForTest(ctx, cfg, timeout, extproto.CapProvisionerDescribe)
	if err != nil {
		return ServerDescription{}, err
	}
	defer func() { _ = client.Close() }()

	var res extproto.ProvisionerDescribeResult
	if err := client.Call(ctx, extproto.MethodProvisionerDescribe, &extproto.ProvisionerDescribeParams{}, &res); err != nil {
		return ServerDescription{}, fmt.Errorf("provisioner.describe: %w", err)
	}

	out := ServerDescription{
		Provider:        cfg.Provider,
		ProviderName:    cfg.ProviderName,
		Extension:       handshake.Name,
		Publisher:       handshake.Publisher,
		ProtocolVersion: handshake.Version,
		Capabilities:    nonNil(handshake.Capabilities),
		Regions:         nonNil(res.Regions),
		TestedAt:        time.Now().UTC(),
		DurationMS:      time.Since(started).Milliseconds(),
	}
	for _, p := range res.Plans {
		plan := Plan{ID: p.ID, Name: p.Name, Period: p.Period}
		// A plan's amount is informational here; an adapter that does not price (Pterodactyl
		// does not) sends zero and the panel shows the plan without a price rather than 0.00.
		if p.Amount.Minor != 0 || p.Amount.Currency != "" {
			plan.Amount = amountOf(p.Amount)
		}
		out.Plans = append(out.Plans, plan)
	}
	if out.Plans == nil {
		out.Plans = []Plan{}
	}
	return out, nil
}

// launchForTest is the shared half of both tests: launch and handshake, then check that the
// method the test is about to call was advertised.
//
// The capability check is not ceremony. The core refuses to call a method an extension did
// not advertise, so an extension that launches and then fails every call would look
// "connected" on a screen that only launched it.
func launchForTest(ctx context.Context, cfg Config, timeout time.Duration,
	capability string) (*extproto.Client, extproto.InitializeResult, error) {
	client, err := Launch(ctx, cfg, timeout)
	if err != nil {
		return nil, extproto.InitializeResult{}, err
	}
	handshake := client.Info()
	if !handshake.HasCapability(capability) {
		// The child's stderr is the only place the reason can be, and it is already redacted
		// by the client, so it is safe to put in an error.
		stderr := client.Stderr()
		_ = client.Close()
		if stderr != "" {
			return nil, extproto.InitializeResult{}, fmt.Errorf(
				"%w: the extension did not advertise %s: %s", extproto.ErrProtocol, capability, stderr)
		}
		return nil, extproto.InitializeResult{}, fmt.Errorf(
			"%w: the extension did not advertise %s", extproto.ErrProtocol, capability)
	}
	return client, handshake, nil
}

// nonNil renders an absent list as an empty one, so a client iterating it never has to
// distinguish "none" from "field absent".
func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
