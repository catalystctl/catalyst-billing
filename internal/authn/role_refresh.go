package authn

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Live role refresh.
//
// authz.go keeps the custom-role permission table in memory and says why: a permission check
// runs on every guarded request, so a query per request would put the database on the hot
// path of a page load. That decision is not revisited here — this file supplies the half of
// the trade the panel was missing.
//
// A cached table is a *snapshot*, and something has to take a new snapshot. Two paths did:
// the boot (cmd/billing/serve.go) and a role edit made through HTTP
// (internal/httpapi/roles_ext.go). Everything else did not. A row inserted with psql, by a
// restore, by a migration, or by another instance's process stayed inert until the next
// boot — and "I granted the permission and it still says 403" is a support ticket that looks
// like a bug in the guard.
//
// # Why a poll rather than LISTEN/NOTIFY
//
// Both were on the table. The choice is a poll over two small tables, and it is deliberate:
//
//  1. **It cannot miss a change.** NOTIFY is fire-and-forget: a listener that is reconnecting
//     (or that started after the commit) never hears about the payload, so a correct
//     implementation still needs a periodic resync behind it. Having two mechanisms where one
//     is the fallback for the other means the fallback is what actually guarantees freshness,
//     and the notification only makes the common case faster. Polling alone has one failure
//     mode — latency, bounded by the interval — and no way to silently lose an update.
//  2. **It costs one query per interval per process, not one per request.** The snapshot is
//     two small tables (`roles`, `role_permissions`), read by `AllRolePermissions` with a
//     single statement. At DefaultRoleRefreshInterval that is 0.5 queries a second on an idle
//     panel, independent of traffic — the property authz.go is protecting.
//  3. **It needs no migration and no trigger**, so the exact case that was broken (a row
//     inserted by hand) goes through the same code path as an edit made through the API. A
//     trigger would also have to be granted to every role that may write those tables, which
//     is a privilege change in a schema this package does not own.
//
// The trade is latency: a change reaches a running process within one interval rather than
// instantly. That is imperceptible for an access change — which is already a human-paced,
// audited action — and the HTTP edit path still refreshes at the point of the edit
// (internal/httpapi/roles_ext.go calls Authorizer.SetTableRoles directly), so the screen that
// made the change never shows a role it cannot use. Every other reader converges within the
// interval.
//
// The loop is started by the process that owns the Authorizer (`billing serve`), not by this
// package's init: a library that starts goroutines at import time is a library that cannot be
// embedded, and the tests here need to drive the clock by hand.

// RolePermissionSource loads the custom-role permission snapshot.
//
// *Service implements it. It is an interface so the refresher can be tested without a
// database, and so a caller can supply a source that is not the authn service (a cluster
// reader, a cached mirror) without touching this file.
type RolePermissionSource interface {
	AllRolePermissions(ctx context.Context) (map[Role][]Permission, error)
}

// DefaultRoleRefreshInterval is how often a running process re-reads the custom-role table.
//
// Two seconds is short enough that an operator who has just run an INSERT does not conclude
// the panel is broken, and long enough that the query is noise: the statement is two
// sequential scans of tables with one row per role and a handful per role.
const DefaultRoleRefreshInterval = 2 * time.Second

// RoleRefresher keeps an Authorizer's custom-role table in step with the database.
//
// It is safe for concurrent use. The refresh runs on its own goroutine and never on the
// request path: a request that arrives while a refresh is in flight reads the previous
// snapshot, which is the whole point of caching it.
type RoleRefresher struct {
	authz  *Authorizer
	source RolePermissionSource
	// interval is the poll period. Zero or negative disables the loop (Refresh still works),
	// which is what a test that wants to drive the refresh by hand uses.
	interval time.Duration
	// logf reports a failed load. It is a function rather than a *slog.Logger so this
	// package does not grow a logging dependency for one call site.
	logf func(msg string, args ...any)

	// loads counts load *attempts*, successful or not. It is the number a test asserts on to
	// prove that handling a request did not add a query.
	loads atomic.Int64
	// applied counts successful loads, so a caller can tell "polling" from "polling and
	// failing", which are very different states during an incident.
	applied atomic.Int64

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	stopped bool
}

// NewRoleRefresher builds a refresher for an authorizer and a source.
func NewRoleRefresher(authz *Authorizer, source RolePermissionSource, interval time.Duration) *RoleRefresher {
	return &RoleRefresher{authz: authz, source: source, interval: interval}
}

// WithLogger sets the reporter for failed loads. A nil function is ignored, so a caller can
// pass a logger unconditionally.
func (r *RoleRefresher) WithLogger(logf func(msg string, args ...any)) *RoleRefresher {
	if logf != nil {
		r.logf = logf
	}
	return r
}

// Refresh loads the current snapshot and installs it.
//
// It is the explicit hook: the loop calls it, a test calls it, and a caller that has just
// changed a role by some path that does not go through the API (a CLI, an importer) can call
// it instead of waiting for the next tick. A failure leaves the previous snapshot in place —
// an unreachable database must not strip every custom role of its permissions.
func (r *RoleRefresher) Refresh(ctx context.Context) error {
	r.loads.Add(1)
	table, err := r.source.AllRolePermissions(ctx)
	if err != nil {
		r.report("role permission refresh failed; keeping the previous snapshot", "error", err)
		return err
	}
	r.authz.SetTableRoles(table)
	r.applied.Add(1)
	return nil
}

// Start begins polling until Stop, or until ctx is cancelled.
//
// Calling it twice is a no-op: the second call returns the same loop. That makes it safe to
// call from a path that may run more than once (a test helper, a supervisor that re-executes
// setup) without spawning a second poller against the same authorizer.
func (r *RoleRefresher) Start(ctx context.Context) {
	if r == nil || r.interval <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil || r.stopped {
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	done := make(chan struct{})
	r.done = done

	go func() {
		defer close(done)
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				// The load gets its own deadline: a poll that hangs on a locked table must
				// not stop the next tick from being considered, and it must not outlive the
				// process's shutdown by much.
				loadCtx, loadCancel := context.WithTimeout(loopCtx, 3*r.interval+time.Second)
				_ = r.Refresh(loadCtx)
				loadCancel()
			}
		}
	}()
}

// Stop ends the loop and waits for it.
//
// Waiting matters: a shutdown that leaves a goroutine mid-query against a pool the caller is
// about to close is a panic in the log at exactly the moment an operator is reading it.
func (r *RoleRefresher) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.cancel = nil
	r.stopped = true
	r.mu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	if done != nil {
		<-done
	}
}

// Loads reports how many load attempts have been made. It is exported for the test that
// proves a guarded request does not query, and for an operator wondering whether the poll is
// running at all.
func (r *RoleRefresher) Loads() int64 { return r.loads.Load() }

// Applied reports how many loads succeeded and were installed.
func (r *RoleRefresher) Applied() int64 { return r.applied.Load() }

// report logs a failure, if a logger was supplied.
func (r *RoleRefresher) report(msg string, args ...any) {
	if r.logf != nil {
		r.logf(msg, args...)
	}
}
