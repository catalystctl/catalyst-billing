package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/catalyst/billing/internal/captcha"
	"github.com/catalyst/billing/internal/settings"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/httpapi"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/web"
)

// serveConfig is the runtime configuration, read from the environment.
type serveConfig struct {
	addr            string
	databaseURL     string
	gatewayProvider string
	gatewayAccount  string
	gatewayBin      string
	signingSecret   string
	secureCookies   bool
	trustedProxies  string
	logFormat       string
	logLevel        string
	embeddedWorker  bool
}

func serveConfigFromEnv() serveConfig {
	return serveConfig{
		addr:            envOr("BILLING_HTTP_ADDR", "127.0.0.1:8080"),
		databaseURL:     os.Getenv("BILLING_DATABASE_URL"),
		gatewayProvider: envOr("BILLING_GATEWAY_PROVIDER", ""),
		gatewayAccount:  envOr("BILLING_GATEWAY_ACCOUNT", ""),
		gatewayBin:      os.Getenv("BILLING_GATEWAY_EXTENSION"),
		signingSecret:   os.Getenv("BILLING_WEBHOOK_SECRET"),
		secureCookies:   !killSwitch("BILLING_INSECURE_COOKIES"),
		trustedProxies:  envOr("BILLING_TRUSTED_PROXIES", ""),
		logFormat:       envOr("BILLING_LOG_FORMAT", "text"),
		logLevel:        envOr("BILLING_LOG_LEVEL", "info"),
		// The scripts (dev-ui.sh, browser-e2e.sh, browser-smoke.sh) export this to keep the
		// background jobs out of the server process, so it has to be read here rather than only
		// from the flag. Reading it only from the flag made those exports dead: harmless while
		// they pass false, and silently wrong the day someone passes true.
		embeddedWorker: killSwitch("BILLING_EMBEDDED_WORKER"),
	}
}

// parseCIDRs parses a comma-separated list of networks.
//
// A malformed entry is an error rather than being skipped: silently ignoring a typo
// would leave the operator believing their proxy is trusted when it is not, or worse,
// that no header is trusted when they intended one to be.
func parseCIDRs(list string) ([]*net.IPNet, error) {
	list = strings.TrimSpace(list)
	if list == "" {
		return nil, nil
	}
	var out []*net.IPNet
	for _, part := range strings.Split(list, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			// Accept a bare address as a single-host network.
			ip := net.ParseIP(part)
			if ip == nil {
				return nil, fmt.Errorf("BILLING_TRUSTED_PROXIES: %q is not an address or CIDR", part)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, n, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("BILLING_TRUSTED_PROXIES: %q is not a valid CIDR: %w", part, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// envIntOr reads an integer setting, falling back when it is absent or malformed.
//
// A malformed value falls back rather than failing startup: these are operational knobs, and a
// typo in a port should not take the process down. The value is logged by the caller when it
// matters.
// publicBaseURL is the origin a redirect or a link must point at.
//
// It is configuration rather than a request-derived value: a redirect built from the Host header is a
// redirect an attacker can point at their own domain by sending the request with a forged Host.
func publicBaseURL() string {
	return strings.TrimRight(envOr("BILLING_PUBLIC_URL", "http://127.0.0.1:8080"), "/")
}

func envIntOr(key string, fallback int) int {
	raw := envOr(key, "")
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// newLogger builds the process logger. Structured logs are the only way to
// correlate a request, a payment and a job after the fact.
func newLogger(format, level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if strings.EqualFold(format, "json") {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfg := serveConfigFromEnv()
	fs.StringVar(&cfg.addr, "addr", cfg.addr, "listen address (host:port)")
	fs.BoolVar(&cfg.embeddedWorker, "embedded-worker", cfg.embeddedWorker, "also run background jobs in this process (default: $BILLING_EMBEDDED_WORKER)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := httpapi.ParseAddr(cfg.addr); err != nil {
		return err
	}

	logger := newLogger(cfg.logFormat, cfg.logLevel)
	slog.SetDefault(logger)

	ctx, cancel := signalContext()
	defer cancel()

	pool, err := store.Open(ctx, store.ConfigFromEnv())
	if err != nil {
		return err
	}
	defer pool.Close()

	// Refuse to serve traffic against an outdated schema: every handler assumes
	// the current schema, and a half-migrated database produces confusing errors
	// far from the cause.
	pending, err := pool.Migrator().Pending(ctx)
	if err != nil {
		return fmt.Errorf("check schema: %w", err)
	}
	if len(pending) > 0 {
		names := make([]string, len(pending))
		for i, m := range pending {
			names[i] = m.Name
		}
		return fmt.Errorf("schema is not current; run `billing migrate` (pending: %s)",
			strings.Join(names, ", "))
	}

	inbox := payments.NewInbox(pool)

	// The gateway adapter owns the lazily-launched extension process. It is built here, before the
	// verifier, because the verifier needs it: verification is a provider-specific operation and the
	// only component that knows a provider's payload shape is that provider's extension.
	gateway := configuredGateway(ctx, pool, logger, cfg.gatewayProvider)

	// The webhook endpoint needs a verifier. Without a configured gateway it refuses deliveries
	// rather than accepting unverified events.
	//
	// It delegates to the extension's `gateway.verify_webhook`, which is the only implementation that
	// can be correct for the gateway actually configured: Stripe signs `t=<ts>,v1=<sig>` over
	// "{timestamp}.{body}" and nests the payment under `data.object`, while the fixture gateway signs
	// a flat body with its own header. The core-side HMACSigner this replaced understood a third,
	// fictional scheme — a separate `Stripe-Timestamp` header — so a real Stripe delivery failed with
	// "missing signature headers" and nothing could ever settle.
	var verifier payments.Verifier
	if cfg.gatewayProvider != "" {
		verifier = payments.ExtensionVerifier{Gateway: gateway}
	} else {
		logger.Warn("no webhook verifier configured; POST /webhooks/* will refuse deliveries",
			"hint", "set BILLING_GATEWAY_PROVIDER (or configure a gateway in the panel)")
	}
	if cfg.signingSecret == "" && cfg.gatewayProvider != "" {
		// The core no longer verifies the signature itself, but the extension still needs its
		// signing secret, and it is resolved from the same configuration the extension is launched
		// with. Saying so here turns a rejected delivery into a diagnosable one.
		logger.Warn("no webhook signing secret is configured; deliveries will be rejected",
			"hint", "set STRIPE_WEBHOOK_SECRET (or save the webhook secret on the integrations screen)")
	}

	// Authentication. A panel served over plain HTTP outside loopback would send
	// the session cookie in the clear, so Secure is on unless the operator
	// explicitly opted out for a local development server.
	hasher, err := authn.NewHasher(authn.DefaultArgon2Params())
	if err != nil {
		return err
	}
	authCfg := authn.DefaultConfig()
	authCfg.Now = func() time.Time { return time.Now().UTC() }
	authSvc, err := authn.New(pool, hasher, authCfg)
	if err != nil {
		return err
	}
	if !cfg.secureCookies {
		// This fires only when BILLING_INSECURE_COOKIES is set, so the hint must say how to get
		// *back* to Secure cookies. It used to advise setting the variable that was already set,
		// which sent an operator in a circle.
		logger.Warn("session cookies are not marked Secure; use this only on loopback",
			"hint", "unset BILLING_INSECURE_COOKIES once the panel is served over TLS")
	}

	// Forwarding headers are only believed from these networks. Empty means the panel
	// is directly exposed and no header is trusted, which is the safe default.
	nets, err := parseCIDRs(cfg.trustedProxies)
	if err != nil {
		return err
	}
	if len(nets) == 0 {
		logger.Info("no trusted proxies configured; X-Forwarded-* headers are ignored",
			"hint", "set BILLING_TRUSTED_PROXIES to your proxy's CIDR, e.g. 10.0.0.0/8")
	}

	// The authorizer resolves the four system roles from the compiled-in map and any
	// operator-defined role from the database. Loading the custom roles is best-effort: a broken
	// read must not stop the panel booting, because the system roles are what the code's separation
	// of duties depends on, and a custom role being inert until the next boot is a far smaller
	// failure than every guarded request erroring. A role edit refreshes the table at the point of
	// the edit (internal/httpapi/roles_ext.go), so this is the boot-time snapshot only.
	authz := authn.NewAuthorizer()
	if table, err := authSvc.AllRolePermissions(ctx); err != nil {
		logger.Warn("custom role permissions could not be loaded; only the four system roles are active",
			"error", err)
	} else {
		authz.SetTableRoles(table)
	}

	// The snapshot above is the boot state, and a role edit through the API refreshes it at the
	// point of the edit. Neither covers a row inserted directly in the database (psql, a restore,
	// another instance's process), which used to stay inert until the next boot. This poll closes
	// that gap from a background goroutine, so a guarded request still costs no query — the
	// reason the table is cached at all. internal/authn/role_refresh.go justifies the poll over
	// LISTEN/NOTIFY.
	roleRefresher := authn.NewRoleRefresher(authz, authSvc, authn.DefaultRoleRefreshInterval).
		WithLogger(func(msg string, args ...any) { logger.Warn(msg, args...) })
	roleRefresher.Start(ctx)
	defer roleRefresher.Stop()

	srv := httpapi.New(httpapi.Config{
		Version: version,
		Logger:  logger,
	}, pool, inbox, cfg.gatewayProvider, cfg.gatewayAccount, verifier).WithAuth(&httpapi.Auth{
		Service:        authSvc,
		Authz:          authz,
		Cookies:        httpapi.CookieConfig{Secure: cfg.secureCookies},
		SessionTTL:     authCfg.SessionLifetime,
		TrustedProxies: nets,
	}).WithPortal(portal.New(pool)).WithGateway(gateway).
		WithCheckoutURLs(
			envOr("BILLING_CHECKOUT_SUCCESS_URL", publicBaseURL()+"/checkout/complete"),
			envOr("BILLING_CHECKOUT_CANCEL_URL", publicBaseURL()+"/checkout/cancelled"))

	// The embedded frontend. A failure here means the binary was built without its assets, which is
	// a broken build rather than a runtime condition, so it stops startup instead of serving 404 for
	// every page.
	webHandler, err := web.New(strings.HasPrefix(publicBaseURL(), "https://"))
	if err != nil {
		return fmt.Errorf("embedded frontend is missing from this build: %w", err)
	}
	// A captcha widget loads a script from the provider's host and renders their iframe, which the
	// panel's CSP forbids by default. The policy is widened by exactly the configured provider's
	// origins and by nothing when no provider is set, so a shop that has not configured a
	// challenge keeps the strictest policy available.
	if provider := captchaProvider(ctx, pool); provider != captcha.ProviderNone {
		webHandler.WithChallengeOrigins(captcha.ChallengeOrigins(provider))
	}
	srv.WithWeb(webHandler)

	errCh := make(chan error, 2)
	go func() { errCh <- srv.Serve(ctx, cfg.addr) }()

	if cfg.embeddedWorker {
		// The worker needs the server for the refund reconciler, which owns the gateway
		// assertion: a gateway that cannot refund is a configuration choice, not a startup error.
		go func() {
			errCh <- runWorkerLoop(ctx, pool, logger, srv)
		}()
	}

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	// Wait for the server to finish draining. Returning immediately would exit
	// the process while in-flight requests (including a webhook being written)
	// are still running, which is how a payment gets lost on a deploy.
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	case <-time.After(30 * time.Second):
		return errors.New("timed out waiting for the server to shut down")
	}
	return nil
}

func cmdWorker(args []string) error {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	format := fs.String("log-format", envOr("BILLING_LOG_FORMAT", "text"), "log format: text or json")
	level := fs.String("log-level", envOr("BILLING_LOG_LEVEL", "info"), "log level")
	if err := fs.Parse(args); err != nil {
		return err
	}
	logger := newLogger(*format, *level)
	slog.SetDefault(logger)

	ctx, cancel := signalContext()
	defer cancel()

	pool, err := store.Open(ctx, store.ConfigFromEnv())
	if err != nil {
		return err
	}
	defer pool.Close()

	return runWorkerLoop(ctx, pool, logger, nil)
}

// captchaProvider reads the configured provider, defaulting to none.
//
// A failure to read the setting is treated as none rather than stopping startup: the panel must
// come up so an operator can fix the setting, and a challenge that silently does not appear is a
// smaller problem than a panel that will not start. The server-side gate reads the same setting
// and refuses a submission without a token, so the failure mode is "registration is refused until
// the setting is fixed" rather than "registration is unprotected".
func captchaProvider(ctx context.Context, pool *store.Pool) captcha.Provider {
	if pool == nil {
		return captcha.ProviderNone
	}
	raw, err := settings.New(pool).String(ctx, "captcha", "provider")
	if err != nil {
		return captcha.ProviderNone
	}
	provider, err := captcha.ParseProvider(raw)
	if err != nil {
		return captcha.ProviderNone
	}
	return provider
}
