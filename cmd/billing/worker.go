package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/catalyst/billing/internal/httpapi"
	"github.com/catalyst/billing/internal/operations"
	"github.com/catalyst/billing/internal/store"
)

// workerHeartbeatInterval is how often the loop reports that it is alive.
//
// It is a property of the loop rather than of the installation, and it is deliberately several
// times shorter than httpapi.HeartbeatStaleAfter: the reader calls a worker stale after two
// minutes of silence, so a beat every fifteen seconds still survives a slow query or a paused
// container before the panel raises an alarm.
const workerHeartbeatInterval = 15 * time.Second

// workerDrainTimeout bounds how long shutdown waits for in-flight batches.
//
// A batch that is mid-transaction gets the chance to commit, because abandoning one between a
// remote call and its commit is how a payment is applied twice. A batch blocked on a gateway
// that will never answer is abandoned: the process is going down regardless, and the `running`
// row it leaves behind is the honest record that the run did not finish.
const workerDrainTimeout = 5 * time.Second

// workerRunCloseTimeout bounds the write that closes a `cron_runs` row.
//
// Closing a run is the one write whose failure is permanent: a row left `running` reads as a hung
// job forever, and nothing later revisits it. So it gets a deadline of its own — long enough to
// outlast a busy pool, short enough that a job's shutdown cannot block on it indefinitely.
const workerRunCloseTimeout = 5 * time.Second

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
func runWorkerLoop(ctx context.Context, pool *store.Pool, logger *slog.Logger, apiServer *httpapi.Server) error {
	// The concrete jobs are wired in jobs.go; keeping the loop generic means a
	// new periodic task is one entry, not a new control flow.
	cache, jobs := buildJobs(pool, logger, apiServer)
	defer cache.Close()
	return runJobs(ctx, pool, logger, jobs, workerHeartbeatInterval)
}

// runJobs is the loop itself: the tickers, the run history and the heartbeat.
//
// The job list is a parameter rather than something this builds, so a test can drive the real
// recording with jobs that fail and panic; buildJobs needs a gateway, a provisioner and a mail
// server before it returns anything, and a test that cannot reach the recording is exactly how
// `cron_runs` stayed empty in production while every test passed. heartbeatEvery is a parameter
// for the same reason: a test that waited fifteen seconds for its second beat would not be
// written, and an untested heartbeat is the one an operator cannot trust.
func runJobs(ctx context.Context, pool *store.Pool, logger *slog.Logger, jobs []jobSpec, heartbeatEvery time.Duration) error {
	var running sync.WaitGroup

	// The heartbeat belongs to the loop, not to a job. A beat written only when work happens
	// cannot tell an idle worker from a dead one, and a job stuck inside a gateway call would
	// silence it exactly when an operator needs it most.
	running.Add(1)
	go func() {
		defer running.Done()
		beatHeartbeat(ctx, pool, logger, len(jobs), heartbeatEvery)
	}()

	for _, j := range jobs {
		j := j
		running.Add(1)
		go func() {
			defer running.Done()
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
				runJob(ctx, pool, logger, j)
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
	drained := make(chan struct{})
	go func() {
		running.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(workerDrainTimeout):
	}
	logger.Info("worker stopped")
	return nil
}

// runJob runs one job and records it in `cron_runs`.
//
// The row is opened before the work and closed after it, because the question this answers is
// "did the renewals run, and is dunning stuck right now" — a row written only on success can
// answer neither. The deferred close is what makes a panicking job visible as `failed` instead
// of as a `running` row that an operator reads as a hang.
//
// Items stays zero: jobSpec.run returns only an error, so the loop has nothing to count. The
// count belongs to the job that did the work, and inventing one here would be a lie in a table
// an operator makes decisions from.
func runJob(ctx context.Context, pool *store.Pool, logger *slog.Logger, spec jobSpec) {
	runID, err := httpapi.StartRun(ctx, pool, spec.name)
	if err != nil {
		// Recording is bookkeeping; the work is the product. A database that refuses the
		// insert is a line in the log, not a reason to stop invoicing.
		logger.Error("record cron run", "job", spec.name, "error", err)
	}

	closed := false
	closeRun := func(outcome, detail string) {
		if closed {
			return
		}
		closed = true
		if runID == "" {
			return
		}
		// The run is closed on a context that outlives cancellation: during shutdown the
		// job's own context is already done, and a cancelled write would leave the row
		// `running` forever — the abandoned row this whole path exists to avoid.
		//
		// It is also BOUNDED. `WithoutCancel` alone inherits no deadline, so a saturated pool
		// makes this write block indefinitely and leaves exactly the abandoned `running` row the
		// comment above promises it cannot. That is not hypothetical: it is how a run stays
		// open for the life of the process while every other connection is in use, and the
		// symptom — a job that reads as hung while it is doing nothing — is the one this row
		// exists to distinguish from a real hang.
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workerRunCloseTimeout)
		defer cancel()
		if err := httpapi.FinishRun(closeCtx, pool, runID, outcome, detail, 0); err != nil {
			logger.Error("close cron run", "job", spec.name, "run", runID, "error", err)
		}
	}

	defer func() {
		r := recover()
		if r == nil {
			return
		}
		// A panic is a failed run, not a dead worker: recovering here keeps the other jobs
		// ticking, and the stack below is the same trace a crash would have printed.
		closeRun("failed", fmt.Sprintf("panic: %v", r))
		logger.Error("job panicked", "job", spec.name, "panic", r, "stack", string(debug.Stack()))
	}()

	err = spec.run(ctx)
	switch {
	case isSkip(err):
		// Checked before the generic failure branch: a skip is a decision, not a defect, and the
		// reason is the whole value of the row.
		reason, _ := operations.SkipReason(err)
		closeRun("skipped", reason)
	case errors.Is(err, context.Canceled):
		// Not a failure: the operator stopped the worker, and the row says so rather than
		// claiming the work is still going.
		closeRun("skipped", "worker shutting down")
	case err != nil:
		closeRun("failed", err.Error())
		logger.Error("job failed", "job", spec.name, "error", err)
	default:
		closeRun("ok", "")
	}
}

// newInstanceID identifies this worker process in the heartbeat table.
//
// Random rather than host-and-pid: two workers can share a host, a container can restart with the
// same pid, and a reused id would make two processes overwrite one row — which is the singleton
// behaviour this replaced. Six bytes is enough to be unique per process and short enough to read in
// a list.
func newInstanceID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A worker with no readable entropy still has to beat, or the panel reports a dead fleet.
		// Falling back to the clock is unique enough for a row key and never silently reuses an id.
		return fmt.Sprintf("worker-%d", time.Now().UnixNano())
	}
	return "worker-" + hex.EncodeToString(b[:])
}

// isSkip reports whether the job stood down deliberately.
//
// A named helper rather than a two-value call inside the switch, because Go cannot use a
// comma-ok result as a case guard and the alternative is allocating the message twice.
func isSkip(err error) bool {
	_, ok := operations.SkipReason(err)
	return ok
}

// beatHeartbeat upserts the heartbeat immediately, then on every tick until the loop stops.
//
// It runs in its own goroutine so a job that hangs cannot take the heartbeat with it: the panel
// must be able to say "the worker is alive, the dunning job is stuck" rather than only "the
// worker is gone", which is the same sentence whether the process died or is merely busy.
func beatHeartbeat(ctx context.Context, pool *store.Pool, logger *slog.Logger, jobs int, every time.Duration) {
	// One instance id for this worker loop, so every beat updates the same row and two workers
	// produce two rows rather than overwriting one.
	instanceID := newInstanceID()
	beat := func() {
		if err := httpapi.Beat(ctx, pool, instanceID, jobs, version); err != nil && ctx.Err() == nil {
			logger.Error("worker heartbeat failed", "error", err, "instance", instanceID)
		}
	}
	// Beat before the first tick so a worker that has just started is already visible.
	beat()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			beat()
		}
	}
}

// stagger derives a stable offset per job name so first runs are spread out.
func stagger(name string) time.Duration {
	var sum int
	for _, r := range name {
		sum += int(r)
	}
	return time.Duration(sum%20) * 100 * time.Millisecond
}
