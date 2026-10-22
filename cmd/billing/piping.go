package main

// Email piping: the IMAP poller as a worker job (E3.8 / TKT-11).
//
// The webhook path is a route and needs no wiring; the poller is periodic work and does.
// It is exported as a function rather than appended inside buildJobs so the piping feature
// owns its own file, the way the extension adapters do: the integrator adds one line to
// cmd/billing/jobs.go and this file is the whole of the rest.
//
// The job is harmless on an installation that has never configured a mailbox: with no
// mail.imap_host it returns without connecting. That matters because the job list is built
// unconditionally — a poller that fails every minute on a fresh install is noise that hides
// the failures that are real.

import (
	"context"
	"log/slog"
	"time"

	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/tickets"
)

// PipingJobs returns the email-piping job specs.
//
// Wire it into buildJobs with:
//
//	jobs = append(jobs, PipingJobs(pool, logger)...)
func PipingJobs(pool *store.Pool, logger *slog.Logger) []jobSpec {
	if killSwitch("BILLING_DISABLE_PIPING") {
		logger.Warn("email piping is disabled by kill switch")
		return nil
	}
	return []jobSpec{{
		name: "ticket-piping",
		// A minute is the interval a customer expects between sending a reply and seeing it in
		// the thread. The poll is bounded by MaxMessages, so a backlog drains over several ticks
		// rather than in one long IMAP session.
		interval: 60 * time.Second,
		run: func(ctx context.Context) error {
			cfg, err := tickets.LoadPipingConfig(ctx, pool)
			if err != nil {
				return err
			}
			if !cfg.IMAP.Configured() {
				// No mailbox configured: nothing to do, and not an error.
				return nil
			}
			result, err := tickets.NewPoller(pool, cfg.IMAP).WithLogger(logger).Poll(ctx)
			if result.Fetched > 0 {
				logger.Info("piped mail",
					"fetched", result.Fetched,
					"attached", result.Attached,
					"created", result.Created,
					"duplicates", result.Duplicates,
					"ignored", result.Ignored,
					"unattributed", result.Unattributed,
					"too_large", result.TooLarge,
					"failed", result.Failed)
			}
			return err
		},
	}}
}
