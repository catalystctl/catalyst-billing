// The operations jobs, registered through the seam the worker already iterates.
//
// # Why they are registered here
//
// `cmd/billing/jobs.go` is the worker's own `package main`: it cannot be imported, and it is the
// one place in this repository that lists the periodic jobs. It calls `lifecycle.Jobs(pool,
// logger)` for the rules that live in a package, which makes this function the registration seam
// a library can use without editing the worker.
//
// The two jobs themselves are not lifecycle rules — nothing about them terminates a service or
// voids a document — so they live in `internal/operations` and are converted here. The conversion
// exists rather than the rules being written into this package because the rules are then
// testable without a worker, and because "what runs on a timer" and "what a rule does" are
// different questions.
//
// # What they are
//
//   - `charge-agreements`: charge the saved payment method of every open, past-due invoice that
//     has one, obeying `cronjob.charge_retry_hours` so a declined card is not retried on every
//     tick (OPS-01).
//   - `telemetry-send`: deliver the anonymous payload once a day when `updates.telemetry` is on
//     (OPS-13).
//
// Both skip rather than fail when the installation has not configured them, which is the normal
// state of a fresh install: no gateway, no telemetry endpoint.
package lifecycle

import (
	"log/slog"

	"github.com/catalyst/billing/internal/operations"
	"github.com/catalyst/billing/internal/store"
)

// OperationsJobs returns the maintenance jobs from internal/operations in the lifecycle shape.
//
// The version is read from internal/updates rather than passed in: it is stamped into the process
// at startup (see updates.SetBuildVersion), and threading it through this function would mean
// changing the signature `cmd/billing/jobs.go` already calls.
func OperationsJobs(pool *store.Pool, logger *slog.Logger) []Job {
	if pool == nil {
		return nil
	}
	var out []Job
	for _, job := range operations.Jobs(pool, operations.Options{Logger: logger}) {
		out = append(out, Job{Name: job.Name, Interval: job.Interval, Run: job.Run})
	}
	return out
}
