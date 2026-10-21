package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/catalyst/billing/internal/store"
)

// runWorkerLoop is the periodic job runner.
//
// It is deliberately a simple ticker over idempotent batches rather than an
// in-memory scheduler with per-item timers: due work is derived from persisted
// state, so a restart, an outage or a duplicate worker cannot lose or double up
// on it. When River is introduced for the heavier queues, this loop stays as the
// scanner that enqueues.
//
// Jobs are separated by interval so a slow or failing subsystem (a gateway that
// hangs, a provisioner that is down) cannot starve the others.
func runWorkerLoop(ctx context.Context, pool *store.Pool, logger *slog.Logger) error {
	type job struct {
		name     string
		interval time.Duration
		run      func(context.Context) error
	}

	// The concrete jobs are wired in jobs.go; keeping the loop generic means a
	// new periodic task is one entry, not a new control flow.
	jobs := buildJobs(pool, logger)
	for _, j := range jobs {
		j := j
		go func() {
			// Stagger the first run so a fleet of workers restarted together does
			// not hit the database in lockstep.
			select {
			case <-ctx.Done():
				return
			case <-time.After(stagger(j.name)):
			}
			ticker := time.NewTicker(j.interval)
			defer ticker.Stop()
			for {
				if err := j.run(ctx); err != nil && !errors.Is(err, context.Canceled) {
					logger.Error("job failed", "job", j.name, "error", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}

	logger.Info("worker started", "jobs", len(jobs))
	<-ctx.Done()

	// Graceful shutdown: give in-flight batches a moment to finish so a job is
	// not abandoned between a remote call and its commit.
	logger.Info("worker draining")
	select {
	case <-time.After(5 * time.Second):
	case <-context.Background().Done():
	}
	logger.Info("worker stopped")
	return nil
}

// stagger derives a stable offset per job name so first runs are spread out.
func stagger(name string) time.Duration {
	var sum int
	for _, r := range name {
		sum += int(r)
	}
	return time.Duration(sum%20) * 100 * time.Millisecond
}
