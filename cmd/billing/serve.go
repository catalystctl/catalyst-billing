package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/httpapi"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/store"
)

// serveConfig is the runtime configuration, read from the environment.
type serveConfig struct {
	addr            string
	databaseURL     string
	gatewayProvider string
	gatewayAccount  string
	gatewayBin      string
	signingSecret   string
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
		logFormat:       envOr("BILLING_LOG_FORMAT", "text"),
		logLevel:        envOr("BILLING_LOG_LEVEL", "info"),
	}
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
	fs.BoolVar(&cfg.embeddedWorker, "embedded-worker", false, "also run background jobs in this process")
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

	// The webhook endpoint needs a verifier. Without a configured gateway it
	// refuses deliveries rather than accepting unverified events.
	var verifier payments.Verifier
	if cfg.gatewayProvider != "" && cfg.signingSecret != "" {
		verifier = payments.HMACSigner{
			Secret:          []byte(cfg.signingSecret),
			Header:          "Stripe-Signature",
			TimestampHeader: "Stripe-Timestamp",
			Tolerance:       5 * time.Minute,
		}
	} else {
		logger.Warn("no webhook verifier configured; POST /webhooks/* will refuse deliveries",
			"hint", "set BILLING_GATEWAY_PROVIDER and BILLING_WEBHOOK_SECRET")
	}

	srv := httpapi.New(httpapi.Config{
		Version: version,
		Logger:  logger,
	}, pool, inbox, cfg.gatewayProvider, cfg.gatewayAccount, verifier)

	errCh := make(chan error, 2)
	go func() { errCh <- srv.Serve(ctx, cfg.addr) }()

	if cfg.embeddedWorker {
		go func() {
			errCh <- runWorkerLoop(ctx, pool, logger)
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

	return runWorkerLoop(ctx, pool, logger)
}
