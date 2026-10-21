package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/catalyst/billing/internal/billing"
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
				processor := payments.NewProcessor(pool, inbox, gw,
					envOr("BILLING_GATEWAY_ACCOUNT", ""), fulfilment.NewApplier(pool))
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
