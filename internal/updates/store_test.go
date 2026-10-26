package updates_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
	"github.com/catalyst/billing/internal/updates"
	"github.com/jackc/pgx/v5"
)

// The update check against a real server and a real database.
//
// The properties under test are the ones a broken implementation would still pass a
// hand-written unit test on: that the answer is cached (so a page load does not hit the
// update server), that a *failure* is cached too (so a broken server is not hammered), that
// the body is bounded, that the timeout is bounded, and that a cache written by a different
// build is not reused.

func newStore(t *testing.T) *store.Pool {
	t.Helper()
	return storetest.DB(t)
}

// manifestServer serves a version manifest and counts how many times it was asked.
func manifestServer(t *testing.T, body string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// The first check fetches; the second is served from the cache and does not touch the server.
// This is the whole reason the cache exists: a check on every page load is a denial of service
// against the update server.
func TestCheckIsCached(t *testing.T) {
	pool := newStore(t)
	srv, hits := manifestServer(t, `{"version":"1.5.0","download_url":"https://example.test/billing-1.5.0","notes":"fixes"}`)

	store := updates.New(pool)
	first, err := store.Check(context.Background(), "1.4.0", "stable", srv.URL, false, nil)
	if err != nil {
		t.Fatalf("first check: %v", err)
	}
	if first.Outcome != updates.OutcomeAvailable {
		t.Fatalf("outcome = %q, want available", first.Outcome)
	}
	if first.Cached {
		t.Fatal("the first check reported itself as cached")
	}
	if first.DownloadURL != "https://example.test/billing-1.5.0" {
		t.Fatalf("download url = %q", first.DownloadURL)
	}
	if hits.Load() != 1 {
		t.Fatalf("the update server was asked %d times, want 1", hits.Load())
	}

	second, err := store.Check(context.Background(), "1.4.0", "stable", srv.URL, false, nil)
	if err != nil {
		t.Fatalf("second check: %v", err)
	}
	if !second.Cached {
		t.Fatal("the second check was not served from the cache")
	}
	if second.Outcome != updates.OutcomeAvailable {
		t.Fatalf("cached outcome = %q, want available", second.Outcome)
	}
	if hits.Load() != 1 {
		t.Fatalf("a cached check asked the update server again (%d hits)", hits.Load())
	}

	// The on-demand check ignores the cache. That is the only route that may.
	forced, err := store.Check(context.Background(), "1.4.0", "stable", srv.URL, true, nil)
	if err != nil {
		t.Fatalf("forced check: %v", err)
	}
	if forced.Cached {
		t.Fatal("a forced check reported itself as cached")
	}
	if hits.Load() != 2 {
		t.Fatalf("a forced check did not ask the update server (%d hits)", hits.Load())
	}
}

// A check that failed is cached too, with a shorter TTL. A broken update server must not be
// asked on every page load, and must not be remembered for an hour as if it had answered.
func TestFailedCheckIsCachedBriefly(t *testing.T) {
	pool := newStore(t)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	store := updates.New(pool)
	first, err := store.Check(context.Background(), "1.4.0", "stable", srv.URL, false, nil)
	if err != nil {
		t.Fatalf("first check: %v", err)
	}
	if first.Outcome != updates.OutcomeUnreachable {
		t.Fatalf("outcome = %q, want unreachable", first.Outcome)
	}
	if first.FailureCode != updates.CodeUnreachable {
		t.Fatalf("failure code = %q, want %q", first.FailureCode, updates.CodeUnreachable)
	}

	second, err := store.Check(context.Background(), "1.4.0", "stable", srv.URL, false, nil)
	if err != nil {
		t.Fatalf("second check: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("a cached failure asked the update server again (%d hits)", hits.Load())
	}
	if second.FailureCode != updates.CodeUnreachable {
		t.Fatalf("cached failure code = %q", second.FailureCode)
	}

	// The failure TTL is shorter than the success TTL: the point of caching a failure is to
	// stop a retry storm, not to hide a fixed server for an hour.
	if updates.FailureCacheTTL >= updates.DefaultCacheTTL {
		t.Fatalf("the failure TTL (%v) is not shorter than the success TTL (%v)",
			updates.FailureCacheTTL, updates.DefaultCacheTTL)
	}

	// Age the cache past the failure TTL: the next check must try again.
	if _, err := pool.Exec(context.Background(),
		`UPDATE update_check SET checked_at = now() - interval '10 minutes'`); err != nil {
		t.Fatalf("age the cache: %v", err)
	}
	if _, err := store.Check(context.Background(), "1.4.0", "stable", srv.URL, false, nil); err != nil {
		t.Fatalf("third check: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("an expired failure cache was not retried (%d hits)", hits.Load())
	}
}

// A cache entry written by a different build is not reused: "1.5.0 available" is only an
// answer relative to the version that asked, and after an in-place upgrade the old answer
// would be a lie that also suppressed the new check.
func TestCacheIsNotReusedAcrossVersionsOrChannels(t *testing.T) {
	pool := newStore(t)
	srv, hits := manifestServer(t, `{"version":"1.5.0"}`)
	store := updates.New(pool)

	if _, err := store.Check(context.Background(), "1.4.0", "stable", srv.URL, false, nil); err != nil {
		t.Fatalf("check: %v", err)
	}
	if _, err := store.Check(context.Background(), "1.5.0", "stable", srv.URL, false, nil); err != nil {
		t.Fatalf("check after upgrade: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("a cache written by another build was reused (%d hits, want 2)", hits.Load())
	}
	// Same versions, different channel: a beta channel and a stable channel are different
	// questions and must not share an answer.
	if _, err := store.Check(context.Background(), "1.5.0", "beta", srv.URL, false, nil); err != nil {
		t.Fatalf("check on beta: %v", err)
	}
	if hits.Load() != 3 {
		t.Fatalf("a cache written for another channel was reused (%d hits, want 3)", hits.Load())
	}
}

// The body is bounded. An update server that answers with a hundred megabytes must not be
// able to make the panel allocate a hundred megabytes.
func TestCheckRefusesAnOversizedManifest(t *testing.T) {
	pool := newStore(t)
	big := `{"version":"1.5.0","notes":"` + strings.Repeat("x", updates.MaxManifestBytes) + `"}`
	srv, _ := manifestServer(t, big)

	store := updates.New(pool)
	result, err := store.Check(context.Background(), "1.4.0", "stable", srv.URL, false, nil)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if result.FailureCode != updates.CodeTooLarge {
		t.Fatalf("failure code = %q, want %q", result.FailureCode, updates.CodeTooLarge)
	}
	if result.Outcome != updates.OutcomeUnreachable {
		t.Fatalf("outcome = %q, want unreachable", result.Outcome)
	}
}

// A manifest whose version is not a version is a broken server, not an update. Comparing
// against it would have to guess, and guessing is how a panel says "1.5.0 available" for
// "latest".
func TestCheckRefusesAnInvalidManifest(t *testing.T) {
	pool := newStore(t)
	cases := map[string]string{
		"not json":         `<html>maintenance</html>`,
		"no version":       `{"download_url":"https://example.test/x"}`,
		"unparseable":      `{"version":"latest"}`,
		"bad download_url": `{"version":"1.5.0","download_url":"ftp://example.test/x"}`,
		"unknown field":    `{"version":"1.5.0","install_command":"rm -rf /"}`,
		"wrong json type":  `{"version":150}`,
	}
	store := updates.New(pool)
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := manifestServer(t, body)
			result, err := store.Check(context.Background(), "1.4.0", "stable", srv.URL, true, nil)
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if result.FailureCode != updates.CodeInvalidManifest {
				t.Fatalf("failure code = %q, want %q (outcome %q)",
					result.FailureCode, updates.CodeInvalidManifest, result.Outcome)
			}
		})
	}
}

// The fetch is bounded in time as well as in size: a server that accepts the connection and
// then says nothing must not hold the check open.
func TestCheckTimesOut(t *testing.T) {
	if testing.Short() {
		t.Skip("the timeout test waits for a real timeout")
	}
	pool := newStore(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(updates.DefaultTimeout + 5*time.Second):
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	store := updates.New(pool)
	started := time.Now()
	result, err := store.Check(context.Background(), "1.4.0", "stable", srv.URL, true, nil)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	elapsed := time.Since(started)
	if result.FailureCode != updates.CodeUnreachable {
		t.Fatalf("failure code = %q, want %q", result.FailureCode, updates.CodeUnreachable)
	}
	// The bound plus generous slack: the assertion is that it returned, not that it was fast.
	if elapsed > updates.DefaultTimeout+3*time.Second {
		t.Fatalf("the check took %v, which is beyond the %v bound", elapsed, updates.DefaultTimeout)
	}
}

// A URL that is not http(s) is refused before any request is made. A file:// URL here would
// make the panel read local files on behalf of whoever can set the setting.
func TestCheckRefusesNonHTTPURL(t *testing.T) {
	pool := newStore(t)
	store := updates.New(pool)
	for _, raw := range []string{"file:///etc/passwd", "ftp://example.test/u.json", "not a url"} {
		result, err := store.Check(context.Background(), "1.4.0", "stable", raw, true, nil)
		if err != nil {
			t.Fatalf("check(%q): %v", raw, err)
		}
		if result.FailureCode != updates.CodeURLInvalid {
			t.Fatalf("check(%q) failure code = %q, want %q", raw, result.FailureCode, updates.CodeURLInvalid)
		}
	}
}

// With no check URL the panel says so, and does not treat it as a failure.
func TestCheckWithoutURLIsNotConfigured(t *testing.T) {
	pool := newStore(t)
	result, err := updates.New(pool).Check(context.Background(), "1.4.0", "stable", "", false, nil)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if result.Outcome != updates.OutcomeNotConfigured {
		t.Fatalf("outcome = %q, want not_configured", result.Outcome)
	}
	if result.Configured {
		t.Fatal("an unconfigured check reported itself as configured")
	}
}

// The approval is one row, replaced, and clearing it is a real delete.
func TestApprovalRoundTrip(t *testing.T) {
	pool := newStore(t)
	ctx := context.Background()
	store := updates.New(pool)

	if _, err := store.Approval(ctx); err == nil {
		t.Fatal("a fresh installation reported an approval")
	} else if err != updates.ErrNoApproval {
		t.Fatalf("approval error = %v, want ErrNoApproval", err)
	}

	// An approval names a user, so one has to exist for the reference to hold.
	var userID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash) VALUES ('approver@example.test', 'x')
		RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return store.SetApproval(ctx, tx, "1.5.0", "security release", userID)
	}); err != nil {
		t.Fatalf("set approval: %v", err)
	}
	approval, err := store.Approval(ctx)
	if err != nil {
		t.Fatalf("read approval: %v", err)
	}
	if approval.Version != "1.5.0" || approval.Note != "security release" || approval.ApprovedBy != userID {
		t.Fatalf("approval = %+v", approval)
	}

	// Replacing rather than accumulating: the history is the audit log's job.
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return store.SetApproval(ctx, tx, "1.6.0", "", userID)
	}); err != nil {
		t.Fatalf("replace approval: %v", err)
	}
	approval, err = store.Approval(ctx)
	if err != nil {
		t.Fatalf("read approval: %v", err)
	}
	if approval.Version != "1.6.0" {
		t.Fatalf("approval version = %q, want 1.6.0", approval.Version)
	}

	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return store.ClearApproval(ctx, tx)
	}); err != nil {
		t.Fatalf("clear approval: %v", err)
	}
	if _, err := store.Approval(ctx); err != updates.ErrNoApproval {
		t.Fatalf("approval after clear = %v, want ErrNoApproval", err)
	}
}

// The state is one read, and it reports the two facts separately: what is available, and what
// has been approved. "Available and approved" is derived, not assumed.
func TestStateReportsApprovalSeparatelyFromAvailability(t *testing.T) {
	pool := newStore(t)
	ctx := context.Background()
	srv, _ := manifestServer(t, `{"version":"1.5.0"}`)
	store := updates.New(pool)

	state, err := store.State(ctx, "1.4.0", "stable", srv.URL, false, nil)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if state.Approval != nil {
		t.Fatal("a fresh installation reported an approval")
	}
	if state.ApprovalMatchesCheck {
		t.Fatal("an unapproved update reported itself as approved")
	}
	if state.Check.Outcome != updates.OutcomeAvailable {
		t.Fatalf("check outcome = %q", state.Check.Outcome)
	}
	// The apply plan is the honest answer to "can you apply it": a procedure, never an
	// automated mode.
	if state.Apply.Mode != "operator" {
		t.Fatalf("apply mode = %q, want operator", state.Apply.Mode)
	}
	if len(state.Apply.Steps) == 0 || state.Apply.Note == "" {
		t.Fatal("the apply plan has no steps or no note")
	}

	var userID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash) VALUES ('approver2@example.test', 'x')
		RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return store.SetApproval(ctx, tx, "1.5.0", "go", userID)
	}); err != nil {
		t.Fatalf("set approval: %v", err)
	}

	state, err = store.State(ctx, "1.4.0", "stable", srv.URL, false, nil)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if !state.ApprovalMatchesCheck {
		t.Fatalf("an approval of the available version did not match: %+v", state)
	}

	// An approval of a different version than the one available is not "available and
	// approved", and the screen must be able to tell.
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return store.SetApproval(ctx, tx, "1.9.9", "", userID)
	}); err != nil {
		t.Fatalf("set approval: %v", err)
	}
	state, err = store.State(ctx, "1.4.0", "stable", srv.URL, false, nil)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if state.ApprovalMatchesCheck {
		t.Fatal("an approval of a version that is not available reported itself as matching")
	}
}

// The telemetry endpoint is a credential: it commonly carries an ingest token in the path, so
// a read path must never return it.
func TestConfigReportsTheEndpointHostOnly(t *testing.T) {
	pool := newStore(t)
	ctx := context.Background()
	store := updates.New(pool)

	cfg, err := store.Config(ctx)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.InstanceID == "" {
		t.Fatal("the installation has no instance id")
	}

	const endpoint = "https://telemetry.example.test/ingest/secret-token-value"
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return store.SetTelemetryEndpoint(ctx, tx, endpoint, "")
	}); err != nil {
		t.Fatalf("set endpoint: %v", err)
	}
	cfg, err = store.Config(ctx)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.EndpointHost() != "telemetry.example.test" {
		t.Fatalf("endpoint host = %q, want telemetry.example.test", cfg.EndpointHost())
	}
	if strings.Contains(cfg.EndpointHost(), "secret-token-value") {
		t.Fatalf("the endpoint host leaked the path: %q", cfg.EndpointHost())
	}
}

// A delivery attempt is recorded whether or not it succeeded, and what is recorded is the
// exact bytes that were sent.
func TestSendRecordsTheAttempt(t *testing.T) {
	pool := newStore(t)
	ctx := context.Background()
	var received atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type = %q", r.Header.Get("Content-Type"))
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	store := updates.New(pool)
	if err := pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return store.SetTelemetryEndpoint(ctx, tx, srv.URL+"/ingest", "")
	}); err != nil {
		t.Fatalf("set endpoint: %v", err)
	}
	payload, err := store.BuildPayload(ctx, "1.4.0", "stable", true)
	if err != nil {
		t.Fatalf("build payload: %v", err)
	}
	result, err := store.Send(ctx, srv.URL+"/ingest", *payload, "", nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if result.Status != "sent" {
		t.Fatalf("status = %q, want sent", result.Status)
	}
	if result.HTTPStatus != http.StatusAccepted {
		t.Fatalf("http status = %d, want 202", result.HTTPStatus)
	}
	if received.Load() != 1 {
		t.Fatalf("the receiver saw %d requests, want 1", received.Load())
	}
	if len(result.PayloadSHA) != 64 {
		t.Fatalf("payload digest = %q, want 64 hex characters", result.PayloadSHA)
	}

	last, err := store.LastTelemetrySend(ctx)
	if err != nil {
		t.Fatalf("last send: %v", err)
	}
	if last == nil || last.Status != "sent" {
		t.Fatalf("last send = %+v", last)
	}

	// A rejected delivery is recorded as a failure, not as a send. An operator who is told
	// "sent" for a 500 would reasonably conclude it arrived.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(bad.Close)
	failed, err := store.Send(ctx, bad.URL+"/ingest", *payload, "", nil)
	if err != nil {
		t.Fatalf("send to a failing endpoint: %v", err)
	}
	if failed.Status != "failed" {
		t.Fatalf("status = %q, want failed", failed.Status)
	}
	if failed.FailureCode != "telemetry_rejected" {
		t.Fatalf("failure code = %q", failed.FailureCode)
	}
}

// A brief description of what the package promises, asserted so a change to it is deliberate:
// the running version is never guessed, and the check never trusts an unparseable answer.
func TestOutcomeStrings(t *testing.T) {
	want := map[updates.Outcome]string{
		updates.OutcomeNotConfigured: "not_configured",
		updates.OutcomeUpToDate:      "up_to_date",
		updates.OutcomeAvailable:     "available",
		updates.OutcomeAhead:         "ahead",
		updates.OutcomeUnknown:       "unknown",
		updates.OutcomeUnreachable:   "unreachable",
	}
	for outcome, text := range want {
		if string(outcome) != text {
			t.Errorf("outcome %q != %q", outcome, text)
		}
	}
}
