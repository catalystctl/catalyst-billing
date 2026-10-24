package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// These tests drive the loop, not the recorder.
//
// internal/httpapi already proves that RecordRun, FinishRun and Beat write the right rows. What
// it cannot prove is that anything calls them, and that is precisely how `cron_runs` and
// `worker_heartbeat` stayed empty in a real deployment while every test passed. So the
// assertions below are made against the tables after running the worker's own loop with jobs
// that succeed, fail and panic; delete the recording from worker.go and these go red.

// workerTestLogger keeps the loop's own logging out of the test output.
func workerTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startWorker runs the loop in the background until stop is called or the test ends.
//
// The returned stop is what lets a test assert on a quiet table: while the loop is ticking, a
// `running` row can always be one that is legitimately in flight, so the interesting assertions
// are made after it has drained.
func startWorker(t *testing.T, pool *store.Pool, jobs []jobSpec, heartbeatEvery time.Duration) (stop func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := runJobs(ctx, pool, workerTestLogger(), jobs, heartbeatEvery); err != nil {
			t.Errorf("runJobs: %v", err)
		}
	}()

	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Error("the worker loop did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// waitFor polls cond until it holds, so a test never depends on a sleep being long enough.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	waitForWithin(t, 15*time.Second, what, cond)
}

// waitForWithin is waitFor with an explicit budget, for the checks whose deadline is a property of
// the system rather than of a test's convenience. The shutdown check below is one: the drain is
// bounded at five seconds, so on an unloaded machine every run is closed long before fifteen — but a
// loaded machine can leave a goroutine descheduled past it, and a test that fails on scheduling is a
// test people learn to re-run rather than read.
func waitForWithin(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// runSummary is the part of a cron_runs row these tests assert on.
type runSummary struct {
	outcome  string
	detail   string
	finished bool
}

// openRuns counts runs of THESE jobs that were started and never closed.
//
// Two scoping decisions, both learned from getting them wrong:
//
//   - Every run of every named job, not the newest row per job. A job that abandons a run and then
//     closes the next one looks perfectly healthy from the newest row alone, and abandoning runs is
//     the failure being guarded — a mutation removing `closeRun` from the success branch passed the
//     newest-row version of this check.
//   - Restricted to the jobs under test, because the package's other tests deliberately leave runs
//     open. `TestTheHeartbeatBeatsWhileAJobIsStuck` starts a job that blocks until the test releases
//     it, and asserts the heartbeat keeps beating while it does; that run is `running` on purpose.
//     Counting the whole table made this check wait out its full budget whenever that test ran
//     first, which reads as a shutdown bug and is a fixture bug.
func openRuns(t *testing.T, pool *store.Pool, jobs []jobSpec) int {
	t.Helper()
	names := make([]string, 0, len(jobs))
	for _, j := range jobs {
		names = append(names, j.name)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM cron_runs WHERE finished_at IS NULL AND job = ANY($1)`,
		names).Scan(&n); err != nil {
		t.Fatalf("count open runs: %v", err)
	}
	return n
}

// latestRun reads the newest run recorded for a job.
func latestRun(t *testing.T, pool *store.Pool, job string) (runSummary, bool) {
	t.Helper()
	var row runSummary
	var finished *time.Time
	err := pool.QueryRow(context.Background(), `
		SELECT outcome, detail, finished_at FROM cron_runs
		WHERE job = $1
		ORDER BY started_at DESC, id DESC
		LIMIT 1`, job).Scan(&row.outcome, &row.detail, &finished)
	if errors.Is(err, pgx.ErrNoRows) {
		return runSummary{}, false
	}
	if err != nil {
		t.Fatalf("read run for %s: %v", job, err)
	}
	row.finished = finished != nil
	return row, true
}

// readHeartbeat reads the worker's heartbeat row.
func readHeartbeat(t *testing.T, pool *store.Pool) (beatAt time.Time, jobs int, version string, ok bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `
		SELECT beat_at, jobs, version FROM worker_heartbeat WHERE id = 'worker'`).
		Scan(&beatAt, &jobs, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, 0, "", false
	}
	if err != nil {
		t.Fatalf("read heartbeat: %v", err)
	}
	return beatAt, jobs, version, true
}

// Every job invocation leaves a closed row with the outcome it actually had.
//
// A job that reports success, a job that returns an error and a job that panics all have to be
// distinguishable in the table, because that table is how an operator decides whether last
// night's dunning ran.
func TestTheWorkerRecordsEveryRunWithItsOutcome(t *testing.T) {
	pool := storetest.DB(t)

	jobs := []jobSpec{
		{name: "test-ok", interval: 50 * time.Millisecond,
			run: func(context.Context) error { return nil }},
		{name: "test-failed", interval: 50 * time.Millisecond,
			run: func(context.Context) error { return errors.New("gateway refused the charge") }},
		{name: "test-panic", interval: 50 * time.Millisecond,
			run: func(context.Context) error { panic("boom") }},
	}
	stop := startWorker(t, pool, jobs, 25*time.Millisecond)

	waitFor(t, "a closed run for every job", func() bool {
		for _, j := range jobs {
			row, ok := latestRun(t, pool, j.name)
			if !ok || !row.finished {
				return false
			}
		}
		return true
	})
	// Stop first: a `running` row is only evidence of a defect once nothing is in flight.
	stop()

	// And then wait for every run to be closed — not just the newest one.
	//
	// `stop()` cancels and waits, but the drain is *bounded*: it gives up after
	// `workerDrainTimeout` rather than blocking shutdown forever, so a run opened by the tick that
	// fired just before cancellation can still be closing when this returns. Asserting immediately
	// that nothing is open tested the scheduler's timing instead of the property that matters.
	//
	// The count is over ALL runs, deliberately. An earlier version of this wait looked only at the
	// newest row per job, and a mutation check proved that vacuous: leaving an older run open
	// forever still passed, because the next tick opened and closed a newer one and "latest" looked
	// healthy. A job that abandons a run and carries on is precisely the failure this guards.
	waitForWithin(t, 15*time.Second, "every run to close after shutdown", func() bool { return openRuns(t, pool, jobs) == 0 })

	for _, want := range []struct{ job, outcome, detail string }{
		{"test-ok", "ok", ""},
		{"test-failed", "failed", "gateway refused the charge"},
		{"test-panic", "failed", "panic: boom"},
	} {
		row, ok := latestRun(t, pool, want.job)
		if !ok {
			t.Fatalf("%s recorded no run at all: the loop is not writing cron_runs", want.job)
		}
		if row.outcome != want.outcome {
			t.Fatalf("%s recorded outcome %q, want %q (detail %q)",
				want.job, row.outcome, want.outcome, row.detail)
		}
		if !row.finished {
			t.Fatalf("%s left a run open: an abandoned `running` row reads as a hang", want.job)
		}
		if want.detail != "" && !strings.Contains(row.detail, want.detail) {
			t.Fatalf("%s recorded detail %q, want it to contain %q",
				want.job, row.detail, want.detail)
		}
	}

	// The defect this whole file exists for: a panicking job that never closes its row looks
	// like a job that hung, which is worse than no row at all.
	var abandoned int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM cron_runs WHERE outcome = 'running'`).Scan(&abandoned); err != nil {
		t.Fatalf("count running rows: %v", err)
	}
	if abandoned != 0 {
		t.Fatalf("%d run(s) were left `running` after the worker stopped", abandoned)
	}

	beatAt, recordedJobs, recordedVersion, ok := readHeartbeat(t, pool)
	if !ok {
		t.Fatal("the worker never recorded a heartbeat: /logs cannot tell a live worker from a dead one")
	}
	if recordedJobs != len(jobs) {
		t.Fatalf("the heartbeat reports %d jobs, want %d", recordedJobs, len(jobs))
	}
	if recordedVersion != version {
		t.Fatalf("the heartbeat reports version %q, want %q", recordedVersion, version)
	}
	if time.Since(beatAt) > time.Minute {
		t.Fatalf("the last heartbeat is %s old", time.Since(beatAt))
	}
}

// The heartbeat advances while a job is still stuck inside its work.
//
// This is the property a per-job beat cannot have: if the heartbeat only moves when a job
// finishes, a worker whose dunning job is blocked on a gateway is indistinguishable from a dead
// process, and the panel says nothing at the exact moment it matters.
func TestTheHeartbeatBeatsWhileAJobIsStuck(t *testing.T) {
	pool := storetest.DB(t)

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	jobs := []jobSpec{{
		name:     "test-stuck",
		interval: time.Hour,
		run: func(ctx context.Context) error {
			select {
			case started <- struct{}{}:
			default:
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}}
	startWorker(t, pool, jobs, 25*time.Millisecond)

	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("the job never started")
	}

	// The run is opened before the work and is still open while the work is in progress, which
	// is what makes "stuck" visible as a run that has not finished rather than as nothing.
	row, ok := latestRun(t, pool, "test-stuck")
	if !ok {
		t.Fatal("the job started but no run row was written: the loop is not recording runs")
	}
	if row.outcome != "running" || row.finished {
		t.Fatalf("while the job was in progress the run was %q (finished=%v), want an open run",
			row.outcome, row.finished)
	}

	before, _, _, ok := readHeartbeat(t, pool)
	if !ok {
		t.Fatal("no heartbeat while a job was running")
	}
	waitFor(t, "the heartbeat to advance while the job is still stuck", func() bool {
		after, _, _, ok := readHeartbeat(t, pool)
		return ok && after.After(before)
	})

	// Let the job finish and wait for its row before stopping, so the outcome is the job's
	// rather than the shutdown's.
	close(release)
	waitFor(t, "the stuck run to be closed", func() bool {
		row, ok := latestRun(t, pool, "test-stuck")
		return ok && row.finished
	})
	row, ok = latestRun(t, pool, "test-stuck")
	if !ok || row.outcome != "ok" {
		t.Fatalf("the finished run recorded %q, want ok", row.outcome)
	}
}

// An idle worker beats too.
//
// A worker with nothing to do must still look alive: the heartbeat answers "is the process
// running", not "did anything happen", and a loop that only beat when it had work would report
// every quiet night as an outage.
func TestAnIdleWorkerStillBeats(t *testing.T) {
	pool := storetest.DB(t)
	stop := startWorker(t, pool, nil, 25*time.Millisecond)

	waitFor(t, "a heartbeat from a worker with no jobs", func() bool {
		_, _, _, ok := readHeartbeat(t, pool)
		return ok
	})
	before, recordedJobs, _, _ := readHeartbeat(t, pool)
	if recordedJobs != 0 {
		t.Fatalf("the heartbeat reports %d jobs, want 0", recordedJobs)
	}
	waitFor(t, "the idle heartbeat to advance", func() bool {
		after, _, _, ok := readHeartbeat(t, pool)
		return ok && after.After(before)
	})
	stop()

	var runs int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM cron_runs`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 0 {
		t.Fatalf("a worker with no jobs recorded %d run(s)", runs)
	}
}
