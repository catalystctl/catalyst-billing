package extproto

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingSleep records the backoff delays instead of waiting, so backoff
// progression is asserted without wall-clock time.
type recordingSleep struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (r *recordingSleep) sleep(ctx context.Context, d time.Duration) error {
	r.mu.Lock()
	r.delays = append(r.delays, d)
	r.mu.Unlock()
	return ctx.Err()
}

func (r *recordingSleep) recorded() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]time.Duration, len(r.delays))
	copy(out, r.delays)
	return out
}

// TestSupervisorBackoffDisablesAfterMaxAttempts proves a failing extension is
// retried with exponential backoff and then disabled, and that the decision is
// persisted.
func TestSupervisorBackoffDisablesAfterMaxAttempts(t *testing.T) {
	rec := &recordingSleep{}
	store := &MemoryStore{}
	var launches int32
	sup, err := NewSupervisor(SupervisorSpec{
		Launch: func(ctx context.Context) (*Client, error) {
			atomic.AddInt32(&launches, 1)
			return nil, errors.New("exec: extension vanished")
		},
		MaxAttempts: 4,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    time.Second,
		Jitter:      func(d time.Duration) time.Duration { return d },
		Sleep:       rec.sleep,
		Store:       store,
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}

	err = sup.Run(context.Background())
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("Run = %v, want ErrDisabled", err)
	}
	if got := atomic.LoadInt32(&launches); got != 4 {
		t.Fatalf("launched %d times, want 4 (MaxAttempts)", got)
	}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}
	got := rec.recorded()
	if len(got) != len(want) {
		t.Fatalf("recorded %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backoff %v, want %v", got, want)
		}
	}

	st, err := store.Load()
	if err != nil {
		t.Fatalf("Store.Load: %v", err)
	}
	if !st.Disabled || st.Attempts != 4 {
		t.Fatalf("persisted state = %+v, want disabled after 4 attempts", st)
	}
	if !strings.Contains(st.Reason, "consecutive failures") {
		t.Fatalf("reason = %q", st.Reason)
	}
	if _, err := sup.Client(); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Client() = %v, want ErrDisabled", err)
	}
	// A disabled supervisor must not launch anything else, however often it runs.
	if err := sup.Run(context.Background()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("second Run = %v, want ErrDisabled", err)
	}
	if got := atomic.LoadInt32(&launches); got != 4 {
		t.Fatalf("launched %d times after the second Run, want 4", got)
	}
}

// TestSupervisorBackoffCap proves the backoff stops growing at MaxDelay.
func TestSupervisorBackoffCap(t *testing.T) {
	rec := &recordingSleep{}
	sup, err := NewSupervisor(SupervisorSpec{
		Launch: func(ctx context.Context) (*Client, error) {
			return nil, errors.New("boom")
		},
		MaxAttempts: 6,
		BaseDelay:   10 * time.Millisecond,
		MaxDelay:    25 * time.Millisecond,
		Jitter:      func(d time.Duration) time.Duration { return d },
		Sleep:       rec.sleep,
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	if err := sup.Run(context.Background()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Run = %v, want ErrDisabled", err)
	}
	want := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		25 * time.Millisecond,
		25 * time.Millisecond,
		25 * time.Millisecond,
	}
	got := rec.recorded()
	if len(got) != len(want) {
		t.Fatalf("recorded %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backoff %v, want %v", got, want)
		}
	}
}

// TestSupervisorJitterBounds proves the default jitter stays inside [d/2, d] and
// still follows the exponential progression, so a fleet of cores does not restart
// in lockstep.
func TestSupervisorJitterBounds(t *testing.T) {
	rec := &recordingSleep{}
	sup, err := NewSupervisor(SupervisorSpec{
		Launch: func(ctx context.Context) (*Client, error) {
			return nil, errors.New("boom")
		},
		MaxAttempts: 7,
		BaseDelay:   8 * time.Millisecond,
		MaxDelay:    32 * time.Millisecond,
		Seed:        42,
		Sleep:       rec.sleep,
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	if err := sup.Run(context.Background()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Run = %v, want ErrDisabled", err)
	}
	nominal := []time.Duration{
		8 * time.Millisecond,
		16 * time.Millisecond,
		32 * time.Millisecond,
		32 * time.Millisecond,
		32 * time.Millisecond,
		32 * time.Millisecond,
	}
	got := rec.recorded()
	if len(got) != len(nominal) {
		t.Fatalf("recorded %d delays, want %d: %v", len(got), len(nominal), got)
	}
	for i, n := range nominal {
		if got[i] < n/2 || got[i] > n {
			t.Fatalf("delay %d = %s, want within [%s, %s]", i, got[i], n/2, n)
		}
	}
	// Jitter must actually jitter, or it is not protecting anything.
	distinct := map[time.Duration]bool{}
	for _, d := range got[2:] {
		distinct[d] = true
	}
	if len(distinct) < 2 {
		t.Fatalf("jitter produced identical delays %v", got)
	}
}

// TestSupervisorPersistsDisableAcrossRestart proves the disabled flag survives a
// core restart: a crash-looping extension must stay off.
func TestSupervisorPersistsDisableAcrossRestart(t *testing.T) {
	store := &FileStore{Path: filepath.Join(t.TempDir(), "extproto-state.json")}
	var launches int32
	spec := SupervisorSpec{
		Launch: func(ctx context.Context) (*Client, error) {
			atomic.AddInt32(&launches, 1)
			return nil, errors.New("boom")
		},
		MaxAttempts: 2,
		BaseDelay:   time.Millisecond,
		MaxDelay:    time.Millisecond,
		Jitter:      func(d time.Duration) time.Duration { return d },
		Sleep:       func(ctx context.Context, d time.Duration) error { return nil },
		Store:       store,
	}
	first, err := NewSupervisor(spec)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	if err := first.Run(context.Background()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("first Run = %v, want ErrDisabled", err)
	}

	// A fresh supervisor, as after a core restart, loads the persisted state.
	second, err := NewSupervisor(spec)
	if err != nil {
		t.Fatalf("NewSupervisor after restart: %v", err)
	}
	if !second.State().Disabled {
		t.Fatal("the disabled state did not survive the restart")
	}
	before := atomic.LoadInt32(&launches)
	if err := second.Run(context.Background()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Run after restart = %v, want ErrDisabled", err)
	}
	if after := atomic.LoadInt32(&launches); after != before {
		t.Fatalf("the restarted supervisor launched the extension %d more times", after-before)
	}
}

// TestSupervisorEnableReArms proves Enable clears the disabled state and that a
// later Run supervises a working extension again.
func TestSupervisorEnableReArms(t *testing.T) {
	store := &MemoryStore{}
	healthy := func(ctx context.Context) (*Client, error) {
		return Launch(ctx, gatewaySpec())
	}
	var launches int32
	spec := SupervisorSpec{
		Launch: func(ctx context.Context) (*Client, error) {
			atomic.AddInt32(&launches, 1)
			return nil, errors.New("boom")
		},
		MaxAttempts: 1,
		BaseDelay:   time.Millisecond,
		MaxDelay:    time.Millisecond,
		Jitter:      func(d time.Duration) time.Duration { return d },
		Sleep:       func(ctx context.Context, d time.Duration) error { return nil },
		Store:       store,
	}
	sup, err := NewSupervisor(spec)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	if err := sup.Run(context.Background()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Run = %v, want ErrDisabled", err)
	}

	// The operator fixes the extension and re-arms it.
	spec.Launch = healthy
	sup2, err := NewSupervisor(spec)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	if err := sup2.Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	st := sup2.State()
	if st.Disabled || st.Attempts != 0 || st.Reason != "" {
		t.Fatalf("state after Enable = %+v, want a clean state", st)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup2.Run(ctx) }()
	waitFor(t, 10*time.Second, "the supervisor to publish a client", func() bool {
		_, err := sup2.Client()
		return err == nil
	})
	client, err := sup2.Client()
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	var health HealthResult
	if err := client.Call(testCtx(t), MethodHealth, &HealthParams{}, &health); err != nil {
		t.Fatalf("health through the supervised client: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrCanceled) {
			t.Fatalf("Run = %v, want ErrCanceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context was canceled")
	}
	if err := sup2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestSupervisorProbeReEnables proves Probe only re-arms a disabled extension
// after a real launch and health check succeed.
func TestSupervisorProbeReEnables(t *testing.T) {
	store := &MemoryStore{}
	spec := SupervisorSpec{
		Launch: func(ctx context.Context) (*Client, error) {
			return Launch(ctx, gatewaySpec())
		},
		MaxAttempts: 1,
		BaseDelay:   time.Millisecond,
		MaxDelay:    time.Millisecond,
		Jitter:      func(d time.Duration) time.Duration { return d },
		Sleep:       func(ctx context.Context, d time.Duration) error { return nil },
		Store:       store,
	}
	sup, err := NewSupervisor(spec)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	if err := sup.Disable("operator disabled it"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if !sup.State().Disabled {
		t.Fatal("Disable did not set the disabled state")
	}

	// A probe against a broken extension must leave it disabled.
	spec.Launch = func(ctx context.Context) (*Client, error) {
		return nil, errors.New("still broken")
	}
	broken, err := NewSupervisor(spec)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	if err := broken.Disable("still broken"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := broken.Probe(context.Background()); err == nil {
		t.Fatal("Probe succeeded against a broken extension")
	}
	if !broken.State().Disabled {
		t.Fatal("a failed Probe re-enabled the extension")
	}

	// A probe against a fixed extension re-arms it.
	spec.Launch = func(ctx context.Context) (*Client, error) {
		return Launch(ctx, gatewaySpec())
	}
	fixed, err := NewSupervisor(spec)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	if err := fixed.Disable("was broken"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := fixed.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if st := fixed.State(); st.Disabled || st.Attempts != 0 {
		t.Fatalf("state after a successful Probe = %+v", st)
	}
	// Probe must not leave the probed process running.
	if _, err := fixed.Client(); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Client() after Probe = %v, want ErrNotRunning", err)
	}
}

// TestSupervisorKillSwitch proves Disable stops the supervisor immediately and
// prevents every future launch.
func TestSupervisorKillSwitch(t *testing.T) {
	var launches int32
	sup, err := NewSupervisor(SupervisorSpec{
		Launch: func(ctx context.Context) (*Client, error) {
			atomic.AddInt32(&launches, 1)
			return Launch(ctx, gatewaySpec())
		},
		MaxAttempts: 5,
		BaseDelay:   time.Millisecond,
		MaxDelay:    time.Millisecond,
		Sleep:       func(ctx context.Context, d time.Duration) error { return nil },
		Store:       &MemoryStore{},
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- sup.Run(context.Background()) }()
	waitFor(t, 10*time.Second, "the supervisor to publish a client", func() bool {
		_, err := sup.Client()
		return err == nil
	})
	if err := sup.Disable("kill switch"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrDisabled) {
			t.Fatalf("Run = %v, want ErrDisabled", err)
		}
		if !strings.Contains(err.Error(), "kill switch") {
			t.Fatalf("Run error %q does not explain why", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the kill switch")
	}
	if got := atomic.LoadInt32(&launches); got != 1 {
		t.Fatalf("launched %d times, want 1", got)
	}
	// The kill switch stops future execution too.
	if err := sup.Run(context.Background()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Run after the kill switch = %v, want ErrDisabled", err)
	}
	if got := atomic.LoadInt32(&launches); got != 1 {
		t.Fatalf("launched %d times after the kill switch, want 1", got)
	}
	if err := sup.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestSupervisorCrashLoopDisables proves repeated crashes of an extension that
// starts successfully still end in the disabled state, because a crash before the
// stability window is a continuation of the same streak.
func TestSupervisorCrashLoopDisables(t *testing.T) {
	rec := &recordingSleep{}
	var launches int32
	sup, err := NewSupervisor(SupervisorSpec{
		Launch: func(ctx context.Context) (*Client, error) {
			atomic.AddInt32(&launches, 1)
			c, err := Launch(ctx, gatewaySpec())
			if err != nil {
				return nil, err
			}
			// Simulate a process that dies moments after passing its health check.
			go func() {
				time.Sleep(5 * time.Millisecond)
				_ = c.Close()
			}()
			return c, nil
		},
		MaxAttempts: 3,
		BaseDelay:   time.Millisecond,
		MaxDelay:    2 * time.Millisecond,
		Jitter:      func(d time.Duration) time.Duration { return d },
		Sleep:       rec.sleep,
		// Never stable: every crash continues the streak.
		StableAfter: time.Hour,
		Store:       &MemoryStore{},
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	if err := sup.Run(context.Background()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Run = %v, want ErrDisabled", err)
	}
	if got := atomic.LoadInt32(&launches); got != 3 {
		t.Fatalf("launched %d times, want 3", got)
	}
	if st := sup.State(); !st.Disabled || !strings.Contains(st.Reason, "consecutive crashes") {
		t.Fatalf("state = %+v, want a crash-loop disable", st)
	}
}

// TestSupervisorHealthCheckFailureCounts proves a restart that comes up but fails
// its health check is treated as a failed restart, not a running extension.
func TestSupervisorHealthCheckFailureCounts(t *testing.T) {
	var launches int32
	sup, err := NewSupervisor(SupervisorSpec{
		Launch: func(ctx context.Context) (*Client, error) {
			atomic.AddInt32(&launches, 1)
			// fail mode answers every call except initialize with an error.
			return Launch(ctx, gatewaySpec("FAKE_GATEWAY_MODE=fail"))
		},
		MaxAttempts:   2,
		BaseDelay:     time.Millisecond,
		MaxDelay:      time.Millisecond,
		Jitter:        func(d time.Duration) time.Duration { return d },
		Sleep:         func(ctx context.Context, d time.Duration) error { return nil },
		HealthTimeout: 2 * time.Second,
		Store:         &MemoryStore{},
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	if err := sup.Run(context.Background()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Run = %v, want ErrDisabled", err)
	}
	if got := atomic.LoadInt32(&launches); got != 2 {
		t.Fatalf("launched %d times, want 2", got)
	}
	if _, err := sup.Client(); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Client() = %v, want ErrDisabled: a client that failed its health check must not be published", err)
	}
}

// TestSupervisorStableCrashResetsStreak proves an extension that ran long enough
// before crashing starts a new failure streak instead of being disabled on its
// first post-restart crash.
func TestSupervisorStableCrashResetsStreak(t *testing.T) {
	var launches int32
	sup, err := NewSupervisor(SupervisorSpec{
		Launch: func(ctx context.Context) (*Client, error) {
			atomic.AddInt32(&launches, 1)
			c, err := Launch(ctx, gatewaySpec())
			if err != nil {
				return nil, err
			}
			go func() {
				time.Sleep(5 * time.Millisecond)
				_ = c.Close()
			}()
			return c, nil
		},
		MaxAttempts: 2,
		BaseDelay:   time.Millisecond,
		MaxDelay:    time.Millisecond,
		Jitter:      func(d time.Duration) time.Duration { return d },
		// Every crash looks like it came after a long, healthy run.
		StableAfter: time.Nanosecond,
		Store:       &MemoryStore{},
		Sleep: func(ctx context.Context, d time.Duration) error {
			if atomic.LoadInt32(&launches) >= 3 {
				return errors.New("enough")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	err = sup.Run(context.Background())
	if err == nil || errors.Is(err, ErrDisabled) {
		t.Fatalf("Run = %v, want a non-disable failure: stable crashes must not disable", err)
	}
	if st := sup.State(); st.Disabled {
		t.Fatalf("a stable extension was disabled after %d launches", atomic.LoadInt32(&launches))
	}
	if got := atomic.LoadInt32(&launches); got < 3 {
		t.Fatalf("launched %d times, want at least 3", got)
	}
}

// TestSupervisorCloseStopsRun proves Close unblocks Run and reports ErrClosed.
func TestSupervisorCloseStopsRun(t *testing.T) {
	sup, err := NewSupervisor(SupervisorSpec{
		Launch: func(ctx context.Context) (*Client, error) {
			return Launch(ctx, gatewaySpec())
		},
		Store: &MemoryStore{},
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- sup.Run(context.Background()) }()
	waitFor(t, 10*time.Second, "the supervisor to publish a client", func() bool {
		_, err := sup.Client()
		return err == nil
	})
	if err := sup.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Run = %v, want ErrClosed", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after Close")
	}
	if err := sup.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestSupervisorSpecValidation covers the local refusals.
func TestSupervisorSpecValidation(t *testing.T) {
	if _, err := NewSupervisor(SupervisorSpec{}); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("NewSupervisor without Launch = %v, want ErrInvalidSpec", err)
	}
	if _, err := NewSupervisor(SupervisorSpec{
		Launch: func(ctx context.Context) (*Client, error) { return nil, errors.New("x") },
		Store:  FileStore{},
	}); err == nil {
		t.Fatal("a FileStore without a Path should fail loudly")
	}
}

// TestFileStoreRoundTrip covers the persisted state format.
func TestFileStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := FileStore{Path: path}
	if st, err := store.Load(); err != nil || st.Disabled {
		t.Fatalf("Load on a missing file = %+v, %v", st, err)
	}
	want := State{Disabled: true, Reason: "crash loop", Attempts: 5, DisabledAt: time.Unix(1_800_000_000, 0).UTC()}
	if err := store.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Disabled != want.Disabled || got.Reason != want.Reason || got.Attempts != want.Attempts {
		t.Fatalf("Load = %+v, want %+v", got, want)
	}
	if !got.DisabledAt.Equal(want.DisabledAt) {
		t.Fatalf("DisabledAt = %s, want %s", got.DisabledAt, want.DisabledAt)
	}
}
