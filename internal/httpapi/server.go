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
	"sync"
	"sync/atomic"
	"time"

	"github.com/catalyst/billing/internal/approvals"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/catalog"
	"github.com/catalyst/billing/internal/dunning"
	"github.com/catalyst/billing/internal/events"
	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/ops"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/staff"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/web"
	"github.com/jackc/pgx/v5"
)

// Config configures the server.
type Config struct {
	// MaxWebhookBytes bounds a webhook body. Providers sign what they send, so a
	// body larger than this is either a misconfiguration or an attack.
	MaxWebhookBytes int64
	// ReadHeaderTimeout bounds how long a client may take to send headers.
	ReadHeaderTimeout time.Duration
	// ReadTimeout bounds reading the whole request, so a client that sends a
	// Content-Length and then trickles bytes cannot hold a connection forever.
	ReadTimeout time.Duration
	// WriteTimeout bounds writing the response.
	WriteTimeout time.Duration
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
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = 30 * time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 30 * time.Second
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
	// auth is nil when authentication is not configured; guarded routes then
	// refuse rather than allowing anonymous access.
	auth *Auth
	// portal is nil when the customer portal is not configured.
	portal *portal.Manager
	// gateway is nil when no payment gateway is configured; checkout then refuses
	// with 503 instead of failing halfway through.
	gateway CheckoutGateway
	// events is the in-process broker behind GET /api/v1/events. It carries only
	// events published after their transaction committed.
	events *events.Broker
	// rateLimiter is the per-principal fixed-window limiter.
	rateLimiter *rateLimiter
	// notify is the durable outbox used by the account lifecycle flows.
	notify *notify.Queue
	// approvals handles the second signature on high-value actions.
	approvals *approvals.Service
	// web serves the embedded frontend; nil for an API-only deployment.
	web *web.Handler
	// staff is the operator read surface, constructed on first use.
	staff   *staff.Service
	staffMu sync.Mutex
	// productCatalog is the product catalog, constructed on first use like staff.
	productCatalog *catalog.Store
	catalogMu      sync.Mutex
	// dunning evaluates suspension eligibility; nil when not configured.
	dunning *dunning.Engine
	// checkoutSuccessURL and checkoutCancelURL are where the gateway returns the customer.
	checkoutSuccessURL string
	checkoutCancelURL  string
	// issuer creates and issues invoices; nil when not configured.
	issuer *billing.Issuer
	// merchant, terms and series are the document defaults a manual invoice uses.
	merchant billing.Merchant
	terms    string
	series   billing.NumberSeries
	// now is the clock, injectable for tests.
	now func() time.Time

	// routes is the table built by the most recent Handler() call.
	routesMu sync.Mutex
	routes   []Route

	// logWriteFailures counts the error_logs and http_logs rows the database refused (see
	// middleware.go). It is on /metrics because an empty /logs is otherwise ambiguous: it could
	// mean nothing failed, or that the writer is losing everything.
	logWriteFailures atomic.Int64

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
	s := &Server{
		cfg:         cfg.withDefaults(),
		pool:        pool,
		inbox:       inbox,
		provider:    provider,
		account:     account,
		verifier:    verifier,
		events:      events.NewBroker(),
		rateLimiter: newRateLimiter(),
		startedAt:   time.Now().UTC(),
		issuer:      billing.NewIssuer(billing.FixedTaxPricer{Policy: "no-tax", Jurisdiction: "none"}),
		series:      billing.DefaultSeries,
		now:         func() time.Time { return time.Now().UTC() },
	}
	if pool != nil {
		s.notify = notify.NewQueue(pool)
		// The dunning engine is needed by the staff surface (eligibility) and the staff
		// mutations (suspend, resume). Constructing it here means one instance with one
		// clock and one policy, rather than one per handler.
		s.dunning = dunning.NewEngine(pool, dunning.DefaultPolicy())
		if svc, err := approvals.New(pool); err == nil {
			s.approvals = svc
		} else {
			// A malformed threshold is a configuration error, not a reason to refuse to
			// start: the endpoints that need approvals report that it is unavailable, and
			// every other screen keeps working.
			s.cfg.Logger.Error("approval thresholds are invalid; approvals are unavailable",
				"error", err)
		}
	}
	return s
}

// WithRateLimits overrides the per-class budgets. It exists for tests and for an
// installation that wants a different ceiling; zero values keep the defaults.
func (s *Server) WithRateLimits(limits map[RateClass]int) *Server {
	if s.rateLimiter == nil {
		s.rateLimiter = newRateLimiter()
	}
	for class, limit := range limits {
		s.rateLimiter.limits[class] = limit
	}
	return s
}

// WithPortal attaches the customer portal.
func (s *Server) WithPortal(m *portal.Manager) *Server {
	s.portal = m
	return s
}

// WithAuth attaches authentication. Without it, guarded routes refuse every
// request rather than falling open.
func (s *Server) WithAuth(a *Auth) *Server {
	s.auth = a
	// Tell a customer about a login from an address their account has not used before. The hook
	// lives here rather than inside authn because authn must not import the outbox: the login
	// path has to work when mail is broken.
	//
	// It writes on its own connection and swallows a failure on purpose. A notification is not a
	// security control that may refuse a correct password — turning a mail outage into a lockout
	// would be a worse bug than the one the notification exists to report.
	if a != nil && a.Service != nil && s.notify != nil {
		a.Service.OnNewDevice(func(ctx context.Context, ev authn.NewDeviceLogin) {
			queue := s.notify
			if queue == nil {
				return
			}
			if err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				_, err := notify.NewLogin(ctx, tx, queue, notify.NewLoginInput{
					UserID:      ev.UserID,
					Email:       ev.Email,
					AuthEventID: ev.AuthEventID,
					At:          ev.At,
					IP:          ev.IP,
					UserAgent:   ev.UserAgent,
					SecurityURL: "/portal/security",
				})
				return err
			}); err != nil {
				// Best effort, and deliberately silent: a failed notification must not fail a
				// login, there is no caller left to return the error to, and the login itself is
				// already durable. A mail outage shows up in notification_deliveries, which is
				// where an operator looks for it.
				_ = err
			}
		})
	}
	return s
}

// Handler returns the root handler.
//
// The route table is built by the registrars (see routes.go), so this function does
// not enumerate endpoints: it applies the outermost middleware and hands the mux to
// the server. loadSession is deliberately global rather than per-route: public
// endpoints must still work when a stale cookie is present, and a route that needs an
// identity is guarded by its own declaration.
//
// Order matters, and two positions are load-bearing. withRequestLog is outermost so the row it
// writes describes the request that actually happened: a panic recovered further in, and a
// request refused by loadSession itself, are both requests this server served, and the log is
// read precisely when something went wrong. noteRequestContext is innermost so it runs after
// loadSession has resolved an identity and withCorrelation has minted an id — both facts exist
// only in the context derived between the two.
func (s *Server) Handler() http.Handler {
	mux, _ := s.buildRoutes()
	return s.withRequestLog(s.withRecovery(s.withCorrelation(s.loadSession(s.noteRequestContext(mux)))))
}

// withCorrelation attaches a request id to the context and echoes it in the response,
// so a support report ("it failed at 12:04") can be tied to one audit row.
func (s *Server) withCorrelation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if id == "" || len(id) > 128 {
			id = newCorrelationID()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(withCorrelationID(r.Context(), id)))
	})
}

// handleHealth is liveness only: it must not touch the database, because a
// database blip should not cause an orchestrator to restart every instance.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	// Liveness only. The version and uptime are not reported here: this endpoint is
	// anonymous and reachable from the internet, and the version alone tells an
	// attacker which vulnerabilities to try.
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
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
		// Readiness is anonymous, so it must not enumerate migration names: that
		// tells an unauthenticated caller which version the deployment is on and what
		// changed recently. The count is enough to act on.
		s.cfg.Logger.WarnContext(ctx, "schema is outdated", "pending", len(pending))
		writeProblem(w, http.StatusServiceUnavailable, "schema_outdated",
			fmt.Sprintf("%d pending migration(s); run billing migrate", len(pending)))
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

	// Log rows the database refused. Without this an empty /logs is ambiguous: it could mean
	// nothing failed, or that the writer is losing everything.
	fmt.Fprintf(w, "# HELP billing_log_rows_failed_total Log rows the database refused.\n")
	fmt.Fprintf(w, "# TYPE billing_log_rows_failed_total counter\n")
	fmt.Fprintf(w, "billing_log_rows_failed_total %d\n", s.logWriteFailures.Load())
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
	// After the response, never before: a log write must not be able to change the answer the
	// caller receives.
	recordProblem(w, status, code, detail)
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
		ReadTimeout:       s.cfg.ReadTimeout,
		// WriteTimeout is generous but finite: responses are small, and a connection
		// held open by a client that stopped reading is a resource leak, not a
		// feature.
		WriteTimeout: s.cfg.WriteTimeout,
		IdleTimeout:  60 * time.Second,
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
