package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/catalyst/billing/internal/billing"
	"github.com/catalyst/billing/internal/checkout"
	"github.com/catalyst/billing/internal/dunning"
	"github.com/catalyst/billing/internal/extproto"
	"github.com/catalyst/billing/internal/fulfilment"
	"github.com/catalyst/billing/internal/money"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/provisioning"
	"github.com/catalyst/billing/internal/renewal"
	"github.com/catalyst/billing/internal/store"
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
func buildJobs(pool *store.Pool, logger *slog.Logger) []jobSpec {
	var jobs []jobSpec

	// ---- payments: turn inbox events into applied payments -------------------
	if !killSwitch("BILLING_DISABLE_PAYMENTS") {
		jobs = append(jobs, jobSpec{
			name:     "payments",
			interval: 5 * time.Second,
			run: func(ctx context.Context) error {
				gw, closeFn, err := launchGateway(ctx)
				if err != nil {
					return err
				}
				defer closeFn()

				inbox := payments.NewInbox(pool)
				// Amount verification is always attached in production: a settled
				// payment is only applied when it matches the recorded checkout.
				processor := payments.NewProcessor(pool, inbox, gw,
					envOr("BILLING_GATEWAY_ACCOUNT", ""), fulfilment.NewApplier(pool)).
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
				prov, closeFn, err := launchProvisioner(ctx)
				if err != nil {
					return err
				}
				defer closeFn()

				worker := provisioning.NewWorker(pool, prov)
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

	return jobs
}

// killSwitch reports whether a subsystem is disabled by the environment.
func killSwitch(key string) bool {
	v := envOr(key, "")
	return v == "1" || v == "true" || v == "yes"
}

// launchGateway starts the configured payment extension.
func launchGateway(ctx context.Context) (payments.Gateway, func(), error) {
	path := envOr("BILLING_GATEWAY_EXTENSION", "")
	if path == "" {
		return nil, nil, errMissingConfig("BILLING_GATEWAY_EXTENSION")
	}
	client, err := extproto.Launch(ctx, extproto.Spec{
		Path:        path,
		CoreVersion: version,
		CallTimeout: 30 * time.Second,
	})
	if err != nil {
		return nil, nil, err
	}
	return gatewayExtension{client: client}, func() { _ = client.Close() }, nil
}

// launchProvisioner starts the configured infrastructure extension.
func launchProvisioner(ctx context.Context) (provisioning.Adapter, func(), error) {
	path := envOr("BILLING_PROVISIONER_EXTENSION", "")
	if path == "" {
		return nil, nil, errMissingConfig("BILLING_PROVISIONER_EXTENSION")
	}
	client, err := extproto.Launch(ctx, extproto.Spec{
		Path:        path,
		CoreVersion: version,
		CallTimeout: 60 * time.Second,
	})
	if err != nil {
		return nil, nil, err
	}
	return provExtension{client: client}, func() { _ = client.Close() }, nil
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
