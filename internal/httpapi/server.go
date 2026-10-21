// Package httpapi is the HTTP surface of the panel.
//
// Design rules that shape it:
//
//   - the request body is bounded before it is read, so a large payload cannot
//     exhaust memory before a handler gets a chance to reject it;
//   - a webhook is acknowledged only after its event is durably stored, so a
//     failure results in a provider retry rather than a lost payment;
//   - a duplicate delivery is acknowledged with success, because "already
//     handled" and "handled" are the same outcome to the provider;
//   - health and readiness are different endpoints: liveness must not depend on
//     the database, or a database blip restarts every instance.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/catalyst/billing/internal/ops"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/store"
)

// Config configures the server.
type Config struct {
	// MaxWebhookBytes bounds a webhook body. Providers sign what they send, so a
	// body larger than this is either a misconfiguration or an attack.
	MaxWebhookBytes int64
	// ReadHeaderTimeout bounds how long a client may take to send headers.
	ReadHeaderTimeout time.Duration
	// ShutdownGrace bounds in-flight request completion on shutdown.
	ShutdownGrace time.Duration
	// Version is reported by the health endpoint.
	Version string
	// Logger receives structured request and error logs.
	Logger *slog.Logger
}

func (c Config) withDefaults() Config {
	if c.MaxWebhookBytes <= 0 {
		c.MaxWebhookBytes = 1 << 20 // 1 MiB
	}
	if c.ReadHeaderTimeout <= 0 {
		c.ReadHeaderTimeout = 10 * time.Second
	}
	if c.ShutdownGrace <= 0 {
		c.ShutdownGrace = 20 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Server is the HTTP handler set.
type Server struct {
	cfg      Config
	pool     *store.Pool
	inbox    *payments.Inbox
	verifier payments.Verifier
	account  string
	provider string

	// Counters are exported by the metrics endpoint. They are process-local on
	// purpose: they describe what this instance saw, not global state.
	webhooksReceived  atomic.Int64
	webhooksDuplicate atomic.Int64
	webhooksRejected  atomic.Int64
	webhooksFailed    atomic.Int64
	startedAt         time.Time
}

// New constructs a Server.
//
// verifier may be nil when no gateway is configured; the webhook endpoint then
// refuses requests instead of silently accepting unsigned events.
func New(cfg Config, pool *store.Pool, inbox *payments.Inbox, provider, account string, verifier payments.Verifier) *Server {
	return &Server{
		cfg:       cfg.withDefaults(),
		pool:      pool,
		inbox:     inbox,
		provider:  provider,
		account:   account,
		verifier:  verifier,
		startedAt: time.Now().UTC(),
	}
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("POST /webhooks/{provider}", s.handleWebhook)
	mux.HandleFunc("GET /api/v1/ops/overview", s.handleOverview)
	return s.withRecovery(s.withRequestLog(mux))
}

// handleHealth is liveness only: it must not touch the database, because a
// database blip should not cause an orchestrator to restart every instance.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.cfg.Version,
		"uptime":  time.Since(s.startedAt).Round(time.Second).String(),
	})
}

// handleReady reports whether this instance can serve traffic: the database must
// be reachable and the schema must be current.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured",
			"no database connection is configured")
		return
	}
	if err := s.pool.Ping(ctx); err != nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_unreachable", "database is not reachable")
		return
	}
	pending, err := s.pool.Migrator().Pending(ctx)
	if err != nil {
		writeProblem(w, http.StatusServiceUnavailable, "schema_unknown", "cannot read migration state")
		return
	}
	if len(pending) > 0 {
		names := make([]string, len(pending))
		for i, m := range pending {
			names[i] = m.Name
		}
		writeProblem(w, http.StatusServiceUnavailable, "schema_outdated",
			"pending migrations: "+strings.Join(names, ", "))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

// handleMetrics exposes a minimal Prometheus text exposition.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# HELP billing_webhooks_received_total Webhook deliveries received.\n")
	fmt.Fprintf(w, "# TYPE billing_webhooks_received_total counter\n")
	fmt.Fprintf(w, "billing_webhooks_received_total %d\n", s.webhooksReceived.Load())
	fmt.Fprintf(w, "# HELP billing_webhooks_duplicate_total Webhook deliveries already ingested.\n")
	fmt.Fprintf(w, "# TYPE billing_webhooks_duplicate_total counter\n")
	fmt.Fprintf(w, "billing_webhooks_duplicate_total %d\n", s.webhooksDuplicate.Load())
	fmt.Fprintf(w, "# HELP billing_webhooks_rejected_total Webhook deliveries that failed verification.\n")
	fmt.Fprintf(w, "# TYPE billing_webhooks_rejected_total counter\n")
	fmt.Fprintf(w, "billing_webhooks_rejected_total %d\n", s.webhooksRejected.Load())
	fmt.Fprintf(w, "# HELP billing_webhooks_failed_total Webhook deliveries that could not be stored.\n")
	fmt.Fprintf(w, "# TYPE billing_webhooks_failed_total counter\n")
	fmt.Fprintf(w, "billing_webhooks_failed_total %d\n", s.webhooksFailed.Load())
	fmt.Fprintf(w, "# HELP billing_uptime_seconds Seconds since this instance started.\n")
	fmt.Fprintf(w, "# TYPE billing_uptime_seconds gauge\n")
	fmt.Fprintf(w, "billing_uptime_seconds %.0f\n", time.Since(s.startedAt).Seconds())
}

// handleWebhook ingests one delivery.
//
// Order matters: bound the body, verify the signature over the exact bytes,
// persist, then acknowledge. Anything that fails before persistence returns a
// non-2xx so the provider retries.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if provider != s.provider || s.verifier == nil {
		// Unknown provider: 404 rather than 400, so a scanner cannot tell an
		// unconfigured provider from a configured one with a bad signature.
		writeProblem(w, http.StatusNotFound, "unknown_provider", "no gateway is configured for this path")
		return
	}

	s.webhooksReceived.Add(1)

	if r.ContentLength > s.cfg.MaxWebhookBytes {
		s.webhooksRejected.Add(1)
		writeProblem(w, http.StatusRequestEntityTooLarge, "payload_too_large",
			fmt.Sprintf("body exceeds %d bytes", s.cfg.MaxWebhookBytes))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, s.cfg.MaxWebhookBytes+1))
	if err != nil {
		s.webhooksFailed.Add(1)
		writeProblem(w, http.StatusBadRequest, "unreadable_body", "could not read the request body")
		return
	}
	if int64(len(body)) > s.cfg.MaxWebhookBytes {
		s.webhooksRejected.Add(1)
		writeProblem(w, http.StatusRequestEntityTooLarge, "payload_too_large",
			fmt.Sprintf("body exceeds %d bytes", s.cfg.MaxWebhookBytes))
		return
	}

	headers := make(map[string]string, len(r.Header))
	for k := range r.Header {
		headers[k] = r.Header.Get(k)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	ev, err := s.verifier.Verify(ctx, body, headers)
	if err != nil {
		s.webhooksRejected.Add(1)
		status := http.StatusBadRequest
		code := "invalid_signature"
		if !errors.Is(err, payments.ErrInvalidSignature) {
			// A transport failure while asking the gateway is our problem, not the
			// sender's: ask for a retry.
			status = http.StatusBadGateway
			code = "verification_unavailable"
		}
		s.cfg.Logger.WarnContext(ctx, "webhook rejected", "provider", provider, "error", err, "code", code)
		writeProblem(w, status, code, "the webhook could not be verified")
		return
	}

	isNew, err := s.inbox.Receive(ctx, provider, s.account, ev, body)
	if err != nil {
		// Durability failed: never acknowledge, or the event is lost.
		s.webhooksFailed.Add(1)
		s.cfg.Logger.ErrorContext(ctx, "webhook persistence failed",
			"provider", provider, "event_id", ev.EventID, "error", err)
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable",
			"the event could not be stored; retry")
		return
	}
	if !isNew {
		// Already ingested. Acknowledge: the provider's retry has done its job.
		s.webhooksDuplicate.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{"status": "duplicate", "event_id": ev.EventID})
		return
	}

	s.cfg.Logger.InfoContext(ctx, "webhook accepted",
		"provider", provider, "event_id", ev.EventID, "event_type", ev.EventType)
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "event_id": ev.EventID})
}

// handleOverview serves the operator dashboard.
//
// It is a GET because it changes nothing, and it is one request because an
// operator opening the panel should see the state of the system, not a page of
// spinners. A slow database therefore fails this endpoint rather than degrading
// it silently.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "database_not_configured", "no database is configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	overview := ops.New(s.pool).Overview(ctx)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, overview)
}

// withRequestLog logs one line per request with a duration, without the body.
func (s *Server) withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.cfg.Logger.InfoContext(r.Context(), "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"bytes", rec.written,
		)
	})
}

// withRecovery turns a panic into a 500 instead of dropping the connection, so a
// single bad request cannot take the process down.
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.cfg.Logger.ErrorContext(r.Context(), "panic in handler",
					"path", r.URL.Path, "panic", fmt.Sprint(rec))
				writeProblem(w, http.StatusInternalServerError, "internal_error", "an internal error occurred")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.written += n
	return n, err
}

// problem is an RFC 9457-style error body: a stable machine code plus a human
// message that never leaks internals.
type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	Code   string `json:"code"`
}

func writeProblem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem{
		Type:   "about:blank",
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
		Code:   code,
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Serve runs the HTTP server until ctx is cancelled, then drains in-flight
// requests within the configured grace period.
func (s *Server) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,
		// No write timeout: responses are small, but a slow database read should
		// surface as a client timeout rather than a truncated response.
		IdleTimeout: 60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.cfg.Logger.Info("http server listening", "addr", addr, "version", s.cfg.Version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	s.cfg.Logger.Info("http server stopped")
	return nil
}

// ParseAddr validates a listen address of the form "host:port".
func ParseAddr(s string) (string, error) {
	if s == "" {
		return "", errors.New("empty listen address")
	}
	idx := strings.LastIndexByte(s, ':')
	if idx < 0 {
		return "", fmt.Errorf("listen address %q must be host:port", s)
	}
	port, err := strconv.Atoi(s[idx+1:])
	if err != nil || port <= 0 || port > 65535 {
		return "", fmt.Errorf("listen address %q has an invalid port", s)
	}
	return s, nil
}
