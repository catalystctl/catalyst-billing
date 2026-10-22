package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/checkout"
	"github.com/catalyst/billing/internal/credit"
	"github.com/catalyst/billing/internal/dunning"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/fulfilment"
	"github.com/catalyst/billing/internal/gatewayconfig"
	"github.com/catalyst/billing/internal/httpapi"
	"github.com/catalyst/billing/internal/lifecycle"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/notify"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/provisioning"
	"github.com/catalyst/billing/internal/renewal"
	"github.com/catalyst/billing/internal/retention"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/tickets"
)

// jobSpec is one periodic task.
type jobSpec struct {
	name     string
	interval time.Duration
	run      func(context.Context) error
}

// buildJobs wires the periodic work.
//
// Every job is a bounded batch that derives its work from persisted state, so
// running it twice, or running two workers, is safe. The kill switches below are
// the operator's emergency brake: a misbehaving gateway or provisioner can be
// stopped without a deploy.
// newSettlementApplier builds the chain that applies a settled payment.
//
// There are two kinds of settled payment and only one of them is an invoice. A credit deposit is
// a payment with no invoice behind it: the money is not revenue, it is a liability the shop owes
// back, and it must be posted to the customer's balance instead of paying for anything. The
// deposit applier recognises that case and falls through to fulfilment for everything else.
//
// It is a named function rather than an inline expression because the failure it prevents is
// silent: with the plain applier a deposit settles, the invoice is marked paid, and the customer's
// balance stays zero — the customer has paid and received nothing, and no error was logged.
// TestSettlementApplierRoutesDeposits calls this directly, so removing the wrapper is a red test
// rather than a support ticket three weeks later.
func newSettlementApplier(pool *store.Pool) payments.Applier {
	return credit.NewDepositApplier(pool,
		fulfilment.NewApplier(pool).WithNotifier(notify.NewQueue(pool)))
}

func buildJobs(pool *store.Pool, logger *slog.Logger, apiServer *httpapi.Server) (*extensionCache, []jobSpec) {
	var jobs []jobSpec

	// One long-lived client per extension, launched on first use and reused.
	//
	// Relaunching the extension on every tick would discard its in-process state,
	// including the idempotency records that make a retried create safe. The
	// process is supervised (see client.go): a crash is detected, the pending call
	// fails with ErrCrashed, and the next use launches a fresh process.
	clients := &extensionCache{ctx: context.Background(), pool: pool}

	// ---- payments: turn inbox events into applied payments -------------------
	if !killSwitch("BILLING_DISABLE_PAYMENTS") {
		jobs = append(jobs, jobSpec{
			name:     "payments",
			interval: 5 * time.Second,
			run: func(ctx context.Context) error {
				gw, err := clients.gateway()
				if err != nil {
					return err
				}

				inbox := payments.NewInbox(pool)
				// Amount verification is always attached in production: a settled
				// payment is only applied when it matches the recorded checkout.
				processor := payments.NewProcessor(pool, inbox, gw,
					envOr("BILLING_GATEWAY_ACCOUNT", ""), newSettlementApplier(pool)).
					WithAttempts(checkout.LookupFor(pool))
				results, err := processor.Process(ctx, 50)
				for _, r := range results {
					logger.Info("payment applied", "event_id", r.EventID,
						"invoice_id", r.InvoiceID, "duplicate", r.Duplicate, "skipped", r.Skipped)
				}
				return err
			},
		})
	} else {
		logger.Warn("payments are disabled by kill switch")
	}

	// ---- provisioning: execute queued service operations ---------------------
	if !killSwitch("BILLING_DISABLE_PROVISIONING") {
		jobs = append(jobs, jobSpec{
			name:     "provisioning",
			interval: 5 * time.Second,
			run: func(ctx context.Context) error {
				prov, err := clients.provisioner()
				if err != nil {
					return err
				}

				worker := provisioning.NewWorker(pool, prov)
				// Reap first: an operation stranded in 'running' by a worker that
				// died mid-call is otherwise invisible to both Run and Reconcile.
				if n, rerr := worker.Reap(ctx); rerr != nil {
					logger.Error("reap expired leases", "error", rerr)
				} else if n > 0 {
					logger.Warn("reaped expired operation leases", "count", n)
				}
				results, err := worker.Run(ctx, 20)
				for _, r := range results {
					logger.Info("provisioning operation", "operation_id", r.OperationID,
						"kind", r.Kind, "outcome", r.Outcome, "detail", r.Detail)
				}
				// Reconcile unknown outcomes in the same tick: they are the ones
				// that block a customer.
				reconciled, rerr := worker.Reconcile(ctx, 20)
				for _, r := range reconciled {
					logger.Info("reconciled operation", "operation_id", r.OperationID,
						"outcome", r.Outcome, "detail", r.Detail)
				}
				if err != nil {
					return err
				}
				return rerr
			},
		})
	} else {
		logger.Warn("provisioning is disabled by kill switch")
	}

	// ---- retention: delete bookkeeping rows past their policy ----------------
	jobs = append(jobs, jobSpec{
		name:     "retention",
		interval: 6 * time.Hour,
		run: func(ctx context.Context) error {
			results, err := retention.New(pool).Run(ctx)
			for _, r := range results {
				if r.Deleted > 0 {
					logger.Info("retention swept", "table", r.Table,
						"deleted", r.Deleted, "keep_for", r.KeepFor.String())
				}
			}
			return err
		},
	})

	// ---- tickets: close the ones nobody replied to --------------------------
	//
	// A ticket that has been open for a fortnight with no reply is not a queue item, it is a
	// liability: it looks like outstanding work and it is not. Closing it is a policy decision
	// with a configured threshold (`tickets.autoclose_days`, 14 by default), so this job reads
	// the setting rather than hard-coding it, and it is idempotent — the scan selects only
	// tickets that are not already closed.
	jobs = append(jobs, jobSpec{
		name:     "ticket-autoclose",
		interval: 6 * time.Hour,
		run: func(ctx context.Context) error {
			closed, err := tickets.CloseStale(ctx, pool, 0)
			if err != nil {
				return err
			}
			if closed > 0 {
				logger.Info("stale tickets closed", "count", closed)
			}
			return nil
		},
	})

	// ---- dunning: escalate overdue invoices, and decide suspensions ----------
	jobs = append(jobs, jobSpec{
		name:     "dunning",
		interval: 60 * time.Second,
		run: func(ctx context.Context) error {
			engine := dunning.NewEngine(pool, dunning.DefaultPolicy())
			results, err := engine.Advance(ctx, 200)
			for _, r := range results {
				logger.Info("dunning advanced", "invoice_id", r.InvoiceID,
					"state", string(r.State), "step", r.Step)
			}
			if err != nil {
				return err
			}
			if killSwitch("BILLING_DISABLE_SUSPENSION") {
				return nil
			}
			if err := resumeCleared(ctx, pool, engine, logger); err != nil {
				return err
			}
			return suspendEligible(ctx, pool, engine, logger)
		},
	})

	// ---- renewals: generate invoices for due subscriptions -------------------
	jobs = append(jobs, jobSpec{
		name:     "renewals",
		interval: 60 * time.Second,
		run: func(ctx context.Context) error {
			issuer := billing.NewIssuer(billing.FixedTaxPricer{
				Rate:     money.MustParseRatio(envOr("BILLING_TAX_RATE", "0%")),
				Rounding: money.RoundHalfUp,
				Policy:   envOr("BILLING_TAX_POLICY", "none"),
			})
			engine := renewal.NewEngine(pool, issuer, billing.Merchant{
				Name:        envOr("BILLING_MERCHANT_NAME", ""),
				CountryCode: envOr("BILLING_MERCHANT_COUNTRY", ""),
			}, envOr("BILLING_RENEWAL_TERMS", "Renews automatically until cancelled."))

			results, err := engine.Run(ctx, 200)
			invoiced := 0
			for _, r := range results {
				if r.Outcome == renewal.OutcomeInvoiced {
					invoiced++
					logger.Info("renewal invoiced", "subscription_id", r.SubscriptionID,
						"invoice", r.InvoiceNumber, "period", r.PeriodKey)
				} else if r.Outcome == renewal.OutcomeNeedsReview {
					logger.Warn("renewal needs review", "subscription_id", r.SubscriptionID,
						"detail", r.Detail)
				}
			}
			if invoiced > 0 {
				logger.Info("renewal batch complete", "scanned", len(results), "invoiced", invoiced)
			}
			return err
		},
	})

	// ---- refunds: reconcile the ones whose outcome is unknown ----------------
	//
	// A refund whose provider call timed out is neither done nor failed, and leaving it that way is
	// the one outcome a customer will never forgive. The reconciler asks the provider again with the
	// same operation key rather than retrying the refund, so asking is safe.
	if apiServer != nil && !killSwitch("BILLING_DISABLE_REFUND_RECONCILE") {
		jobs = append(jobs, jobSpec{
			name:     "refund-reconcile",
			interval: 2 * time.Minute,
			run: func(ctx context.Context) error {
				settled, unknown, err := apiServer.ReconcileRefunds(ctx, 25)
				if settled > 0 || unknown > 0 {
					logger.Info("refund reconciliation", "settled", settled, "still_unknown", unknown)
				}
				return err
			},
		})
	}

	// ---- notifications: drain the durable outbox ----------------------------
	//
	// This job is last for a reason: every other job in this list can enqueue a message, and a
	// message that is queued and never drained is worse than no message at all, because the
	// delivery record says it was accepted.
	if !killSwitch("BILLING_DISABLE_NOTIFICATIONS") {
		sender, err := notifySender(logger)
		if err != nil {
			logger.Error("notifications are disabled: the sender is misconfigured", "error", err)
		} else {
			queue := notify.NewQueue(pool)
			jobs = append(jobs, jobSpec{
				name:     "notifications",
				interval: 15 * time.Second,
				run: func(ctx context.Context) error {
					res, err := queue.RunOnce(ctx, sender, 50)
					if res.Sent > 0 || res.Failed > 0 {
						logger.Info("notifications drained", "sent", res.Sent,
							"failed", res.Failed, "attempted", res.Attempts)
					}
					return err
				},
			})
		}
	} else {
		logger.Warn("notifications are disabled by kill switch")
	}

	// ---- lifecycle: the two rules nothing used to read ------------------------
	//
	// cronjob.order_cancel_days and cronjob.terminate_days are settings, not code, so they were
	// reported by the panel while nothing acted on them: an unpaid order stayed pending forever
	// and a suspension never became a termination. These two jobs are what make those settings
	// real. Each is independently kill-switchable inside internal/lifecycle, because terminating
	// a customer's server is the one action an operator must be able to stop instantly without a
	// redeploy.
	for _, lj := range lifecycle.Jobs(pool, logger) {
		lj := lj
		jobs = append(jobs, jobSpec{
			name:     lj.Name,
			interval: lj.Interval,
			run:      lj.Run,
		})
	}

	return clients, jobs
}

// notifySender builds the configured sender.
//
// A missing SMTP host is not an error that stops the worker: it selects the log sender, which
// writes the message to the log and still records the delivery. That makes a development
// installation's reset flow observable end to end, and it makes the absence of a mail server a
// visible choice rather than a silent one.
func notifySender(logger *slog.Logger) (notify.Sender, error) {
	host := envOr("BILLING_SMTP_HOST", "")
	if host == "" {
		logger.Warn("no BILLING_SMTP_HOST is set; notifications are written to the log")
		return notify.LogSender{Logger: logger}, nil
	}
	return notify.SMTPSender{
		Host:     host,
		Port:     envIntOr("BILLING_SMTP_PORT", 587),
		Username: envOr("BILLING_SMTP_USERNAME", ""),
		Password: envOr("BILLING_SMTP_PASSWORD", ""),
		From:     envOr("BILLING_SMTP_FROM", ""),
	}, nil
}

// extensionCache keeps one client per configured extension for the life of the
// worker process.
type extensionCache struct {
	ctx  context.Context
	pool *store.Pool
	// launchOverride, when set, replaces the configuration resolution. It exists so a test can
	// assert on the launch without standing up a settings table, which is the only way to test
	// "the environment reaches the child" as a property of this type rather than of the resolver.
	launchOverride *gatewayconfig.Config
	mu             sync.Mutex
	gw             *gatewayExtension
	gwC            *extproto.Client
	pr             *provExtension
	prC            *extproto.Client
}

func (c *extensionCache) gateway() (*gatewayExtension, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// A client whose process died stays dead: Err() is non-nil forever. Returning it
	// would make one crash stop payments until the worker restarts, so a dead client
	// is discarded and a fresh process is launched.
	if c.gw != nil {
		if err := c.gwC.Err(); err == nil {
			return c.gw, nil
		}
		_ = c.gwC.Close()
		c.gw, c.gwC = nil, nil
	}
	cfg, err := c.integration(gatewayconfig.GatewayKind)
	if err != nil {
		return nil, err
	}
	client, err := extproto.Launch(c.ctx, extproto.Spec{
		Path: cfg.ExtensionPath,
		// The child inherits PATH and exactly these entries, nothing else. Without this the
		// adapter starts with no credentials at all: a gateway that is configured in the panel
		// and correct in every other respect would fail every charge with "STRIPE_SECRET_KEY is
		// required", and the panel would be reporting a configuration the worker never sees.
		Env:         cfg.Env(),
		CoreVersion: version,
		CallTimeout: 30 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	c.gwC = client
	c.gw = &gatewayExtension{client: client}
	return c.gw, nil
}

func (c *extensionCache) provisioner() (*provExtension, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pr != nil {
		if err := c.prC.Err(); err == nil {
			return c.pr, nil
		}
		_ = c.prC.Close()
		c.pr, c.prC = nil, nil
	}
	cfg, err := c.integration(gatewayconfig.ServerKind)
	if err != nil {
		return nil, err
	}
	client, err := extproto.Launch(c.ctx, extproto.Spec{
		Path: cfg.ExtensionPath,
		// See gateway(): the environment is the only channel an extension has for its
		// credentials, and a provisioner without them cannot build anything.
		Env:         cfg.Env(),
		CoreVersion: version,
		CallTimeout: 60 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	c.prC = client
	c.pr = &provExtension{client: client}
	return c.pr, nil
}

// integration resolves the configuration the worker must launch.
//
// The panel's Test button resolves the same thing through the same package, which is the point:
// a test that passes and a worker that fails would mean two answers to one question. The
// resolver reads the environment first and the settings table second, so a container deployment
// can still override what an operator saved.
//
// A configuration that names a provider but no binary is a hard error rather than a silent
// fall-through to the fixture. Launching the wrong gateway is worse than refusing to take a
// payment, and the fixture would happily accept one.
func (c *extensionCache) integration(kind gatewayconfig.Kind) (gatewayconfig.Config, error) {
	if c.launchOverride != nil {
		cfg := *c.launchOverride
		if cfg.ExtensionPath == "" {
			return gatewayconfig.Config{}, fmt.Errorf("%w: %s provider %q has no extension binary",
				gatewayconfig.ErrNotConfigured, kind, cfg.Provider)
		}
		return cfg, nil
	}
	cfg, err := gatewayconfig.New(c.pool).Load(c.ctx, kind)
	if err != nil {
		return gatewayconfig.Config{}, fmt.Errorf("resolve %s configuration: %w", kind, err)
	}
	if !cfg.Configured() {
		return gatewayconfig.Config{}, errMissingConfig(envNameFor(kind))
	}
	if cfg.ExtensionPath == "" {
		return gatewayconfig.Config{}, fmt.Errorf("%w: %s provider %q has no extension binary",
			gatewayconfig.ErrNotConfigured, kind, cfg.Provider)
	}
	return cfg, nil
}

// envNameFor names the variable an operator sets when nothing is configured at all, so the error
// tells them what to do rather than only what is missing.
func envNameFor(kind gatewayconfig.Kind) string {
	switch kind {
	case gatewayconfig.GatewayKind:
		return "BILLING_GATEWAY_EXTENSION"
	default:
		return "BILLING_PROVISIONER_EXTENSION"
	}
}

// Close shuts down every extension client this process started.
func (c *extensionCache) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gwC != nil {
		_ = c.gwC.Close()
		c.gw, c.gwC = nil, nil
	}
	if c.prC != nil {
		_ = c.prC.Close()
		c.pr, c.prC = nil, nil
	}
}

// killSwitch reports whether a subsystem is disabled by the environment.
func killSwitch(key string) bool {
	v := envOr(key, "")
	return v == "1" || v == "true" || v == "yes"
}

// suspendEligible enqueues suspension for services whose invoice is exhausted and
// past the policy's suspension period.
//
// It goes through dunning.RequestSuspension rather than inserting operations
// directly, so every suspension has an eligibility decision behind it and a
// generation to fence against.
func suspendEligible(ctx context.Context, pool *store.Pool, engine *dunning.Engine, logger *slog.Logger) error {
	rows, err := pool.Query(ctx, `
		SELECT s.id
		FROM services s
		JOIN subscriptions sub ON sub.id = s.subscription_id
		JOIN invoices i ON i.subscription_id = sub.id
		JOIN dunning_states d ON d.invoice_id = i.id
		WHERE s.status = 'active'
		  AND i.status = 'open'
		  AND d.state = 'exhausted'
		ORDER BY i.due_at
		LIMIT 50`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, id := range ids {
		elig, err := engine.RequestSuspension(ctx, id)
		switch {
		case errors.Is(err, dunning.ErrNotEligible):
			logger.Info("suspension withheld", "service_id", id, "reason", elig.Reason)
		case err != nil:
			logger.Error("suspension request failed", "service_id", id, "error", err)
		default:
			logger.Warn("suspension enqueued", "service_id", id,
				"outstanding", elig.Outstanding, "reason", elig.Reason)
		}
	}
	return nil
}

// resumeCleared re-enables services that were suspended for non-payment and whose
// balance is now clear.
//
// Resume runs before suspend in the same tick so a customer who pays is restored
// without waiting for the next cycle, and so a service cannot be suspended and
// resumed in the same pass.
func resumeCleared(ctx context.Context, pool *store.Pool, engine *dunning.Engine, logger *slog.Logger) error {
	rows, err := pool.Query(ctx, `
		SELECT s.id
		FROM services s
		WHERE s.status = 'suspended'
		  AND NOT EXISTS (
		      SELECT 1 FROM invoices i
		      WHERE i.customer_id = s.customer_id
		        AND i.status = 'open'
		        AND i.total_minor > i.amount_paid_minor
		  )
		LIMIT 50`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, id := range ids {
		res, err := engine.RequestResume(ctx, id)
		switch {
		case err != nil:
			logger.Error("resume request failed", "service_id", id, "error", err)
		case res.Eligible:
			logger.Info("resume enqueued", "service_id", id)
		default:
			logger.Debug("resume withheld", "service_id", id, "reason", res.Reason)
		}
	}
	return nil
}
