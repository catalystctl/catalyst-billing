package main

// The email piping job.
//
// Piping has two ingest paths and this file wires the pull one. The push one — a provider posting
// to /webhooks/tickets/inbound — is a route registered by internal/httpapi and needs no job.
//
// The poller is harmless on an installation that has not configured a mailbox: Poll returns a
// zero result rather than an error, because "no mailbox" is a fresh install and not a fault, and a
// job that logged a failure every minute on every fresh install would train an operator to ignore
// the log. A real failure — a refused login, an unreachable host — is returned and logged by the
// worker loop.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/tickets"
)

// pipingInterval is how often the mailbox is read.
//
// A minute is chosen against the two things that matter: a customer who replies expects the
// thread to show it, and an IMAP poll is a login plus a fetch, so polling every few seconds would
// be rude to the mail host for no gain. It is a constant rather than a setting because the right
// value does not vary by installation, and a knob nobody turns is a knob that rots.
const pipingInterval = time.Minute

// pipingJob builds the poller for this process.
//
// It is constructed once and reused, because the poller keeps the UID watermark of what it has
// already read. A poller rebuilt on every tick would re-read the mailbox from the start, which is
// the same as having no dedupe at all until the message ids catch it.
func pipingJob(pool *store.Pool, logger *slog.Logger) (jobSpec, error) {
	if pool == nil {
		return jobSpec{}, errors.New("piping: no database")
	}
	cfg, err := tickets.LoadPipingConfig(context.Background(), pool)
	if err != nil {
		return jobSpec{}, fmt.Errorf("piping: read configuration: %w", err)
	}
	poller := tickets.NewPoller(pool, cfg.IMAP).WithLogger(logger)

	return jobSpec{
		name:     "ticket-piping",
		interval: pipingInterval,
		run: func(ctx context.Context) error {
			res, err := poller.Poll(ctx)
			if err != nil {
				return err
			}
			// Only speak when something happened. A job that logs every minute is a job whose
			// log nobody reads, and the interesting line — a refusal — would be lost in it.
			if res.Fetched > 0 {
				logger.Info("ticket piping polled the mailbox",
					"fetched", res.Fetched,
					"attached", res.Attached,
					"created", res.Created,
					"duplicates", res.Duplicates,
					"ignored", res.Ignored,
					"unattributed", res.Unattributed,
					"too_large", res.TooLarge,
					"failed", res.Failed)
			}
			return nil
		},
	}, nil
}
