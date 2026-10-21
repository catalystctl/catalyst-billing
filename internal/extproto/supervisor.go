package extproto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Supervisor defaults.
const (
	// DefaultMaxAttempts is how many consecutive failed starts or crashes the
	// supervisor tolerates before it disables the extension.
	DefaultMaxAttempts = 5
	// DefaultBaseDelay is the first backoff step.
	DefaultBaseDelay = 250 * time.Millisecond
	// DefaultMaxDelay caps the backoff.
	DefaultMaxDelay = 30 * time.Second
	// DefaultHealthTimeout bounds the post-restart health probe.
	DefaultHealthTimeout = 5 * time.Second
	// DefaultStableAfter is how long a restarted extension must stay up before a
	// later crash is treated as a fresh failure streak rather than a continuation
	// of the crash loop. Without this window, an extension that starts, passes
	// its health check and dies immediately would reset the counter forever and
	// never be disabled.
	DefaultStableAfter = 30 * time.Second
)

// State is the supervisor's persisted view of an extension. It is written to a
// Store so that a crash-looping extension stays disabled across core restarts:
// restarting the core must not re-arm a broken extension.
type State struct {
	// Disabled means the supervisor will not start the extension.
	Disabled bool `json:"disabled"`
	// Reason explains why it was disabled, for the operator.
	Reason string `json:"reason,omitempty"`
	// Attempts is the length of the current consecutive-failure streak.
	Attempts int `json:"attempts"`
	// DisabledAt is when the extension was disabled.
	DisabledAt time.Time `json:"disabled_at,omitzero"`
	// UpdatedAt is when the state was last written.
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// Store persists supervisor State.
//
// A Store must be safe for concurrent use and must fail loudly: silently losing
// the disabled flag means a crash-looping extension is restarted after the next
// core restart, which is exactly the behaviour the flag exists to prevent.
type Store interface {
	Load() (State, error)
	Save(State) error
}

// MemoryStore is an in-process Store. It keeps the disabled flag for the life of
// the process, which is enough for tests and for deployments that treat a core
// restart as an operator decision.
type MemoryStore struct {
	mu    sync.Mutex
	state State
}

// Load returns the stored state.
func (m *MemoryStore) Load() (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state, nil
}

// Save replaces the stored state.
func (m *MemoryStore) Save(s State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = s
	return nil
}

// FileStore persists State as a JSON file, replaced atomically so a crash during
// the write cannot leave a half-written file that disables (or re-enables) an
// extension by accident.
type FileStore struct {
	Path string
}

// Load reads the state file. A missing file is not an error: it means nothing has
// been recorded yet.
func (f FileStore) Load() (State, error) {
	data, err := os.ReadFile(f.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, nil
		}
		return State{}, fmt.Errorf("extproto: read supervisor state %q: %w", f.Path, err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("extproto: parse supervisor state %q: %w", f.Path, err)
	}
	return s, nil
}

// Save writes the state file atomically.
func (f FileStore) Save(s State) error {
	if f.Path == "" {
		return fmt.Errorf("%w: FileStore.Path is empty", ErrInvalidSpec)
	}
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("extproto: encode supervisor state: %w", err)
	}
	dir := filepath.Dir(f.Path)
	tmp, err := os.CreateTemp(dir, ".extproto-state-*")
	if err != nil {
		return fmt.Errorf("extproto: create supervisor state: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("extproto: write supervisor state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("extproto: sync supervisor state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("extproto: close supervisor state: %w", err)
	}
	if err := os.Rename(name, f.Path); err != nil {
		return fmt.Errorf("extproto: replace supervisor state: %w", err)
	}
	return nil
}

// SupervisorSpec configures a Supervisor. The hooks exist so that backoff
// behaviour can be tested deterministically instead of by sleeping.
type SupervisorSpec struct {
	// Launch starts one extension process and returns a client whose handshake
	// has completed. Required.
	Launch func(ctx context.Context) (*Client, error)
	// MaxAttempts is the consecutive-failure budget; zero means
	// DefaultMaxAttempts.
	MaxAttempts int
	// BaseDelay is the first backoff step; zero means DefaultBaseDelay.
	BaseDelay time.Duration
	// MaxDelay caps the backoff; zero means DefaultMaxDelay.
	MaxDelay time.Duration
	// Jitter maps a nominal backoff to the delay actually used. nil means full
	// jitter: uniformly random in [nominal/2, nominal].
	Jitter func(nominal time.Duration) time.Duration
	// Seed seeds the default jitter source.
	Seed int64
	// Sleep waits for d or until ctx is done. nil uses a timer. Tests inject a
	// recording no-op so backoff progression is asserted without wall time.
	Sleep func(ctx context.Context, d time.Duration) error
	// HealthTimeout bounds each health probe; zero means DefaultHealthTimeout.
	HealthTimeout time.Duration
	// StableAfter is the uptime after which a crash counts as a new failure
	// streak; zero means DefaultStableAfter. Set it to a large value in tests to
	// simulate an extension that never stays up.
	StableAfter time.Duration
	// Store persists the disabled state; nil means a MemoryStore.
	Store Store
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
}

// Supervisor keeps one long-lived extension running.
//
// It restarts the extension with exponential backoff and jitter after a crash,
// health-checks every restart, and gives up after MaxAttempts consecutive
// failures by writing a persisted disabled state. Giving up is deliberate: an
// extension that crashes on startup will crash on every restart, and a tight
// restart loop is worse than an obviously disabled integration, because it hides
// the failure behind a stream of identical log lines.
//
// Recovery is explicit. Enable clears the disabled state and the failure streak;
// Probe does the same but only after a launch and health check actually succeed,
// so an operator can re-arm a fixed extension without guessing.
type Supervisor struct {
	spec  SupervisorSpec
	store Store
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
	rnd   *rand.Rand

	mu        sync.Mutex
	state     State
	client    *Client
	startedAt time.Time
	attempts  int
	killed    bool
	closed    bool
	kill      chan struct{}
}

// NewSupervisor builds a Supervisor and loads any persisted disabled state.
func NewSupervisor(spec SupervisorSpec) (*Supervisor, error) {
	if spec.Launch == nil {
		return nil, fmt.Errorf("%w: SupervisorSpec.Launch is nil", ErrInvalidSpec)
	}
	if spec.MaxAttempts <= 0 {
		spec.MaxAttempts = DefaultMaxAttempts
	}
	if spec.BaseDelay <= 0 {
		spec.BaseDelay = DefaultBaseDelay
	}
	if spec.MaxDelay <= 0 {
		spec.MaxDelay = DefaultMaxDelay
	}
	if spec.MaxDelay < spec.BaseDelay {
		spec.MaxDelay = spec.BaseDelay
	}
	if spec.HealthTimeout <= 0 {
		spec.HealthTimeout = DefaultHealthTimeout
	}
	if spec.StableAfter <= 0 {
		spec.StableAfter = DefaultStableAfter
	}
	if spec.Store == nil {
		spec.Store = &MemoryStore{}
	}
	if spec.Now == nil {
		spec.Now = time.Now
	}
	if spec.Sleep == nil {
		spec.Sleep = sleepContext
	}
	s := &Supervisor{
		spec:  spec,
		store: spec.Store,
		now:   spec.Now,
		sleep: spec.Sleep,
		rnd:   rand.New(rand.NewSource(spec.Seed)),
		kill:  make(chan struct{}),
	}
	st, err := spec.Store.Load()
	if err != nil {
		return nil, err
	}
	s.state = st
	s.attempts = st.Attempts
	return s, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run supervises the extension until ctx is done, the extension is disabled, the
// kill switch is thrown, or Close is called.
//
// Run returns nil when ctx ends, and an error wrapping ErrDisabled when the
// extension is given up on. It is not safe to call Run concurrently with itself.
func (s *Supervisor) Run(ctx context.Context) error {
	for {
		if err := s.runnable(ctx); err != nil {
			return err
		}
		client, err := s.start(ctx)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return wrapContext(ctxErr)
			}
			delay, fatal := s.recordFailure(err)
			if fatal != nil {
				return fatal
			}
			if serr := s.sleep(ctx, delay); serr != nil {
				return wrapContext(serr)
			}
			continue
		}
		s.publish(client)
		kill := s.killCh()
		select {
		case <-ctx.Done():
			s.release(client)
			_ = client.Close()
			return wrapContext(ctx.Err())
		case <-kill:
			s.release(client)
			_ = client.Close()
			return s.disabledError()
		case <-client.Done():
			cause := client.Err()
			s.release(client)
			_ = client.Close()
			delay, fatal := s.recordCrash(cause)
			if fatal != nil {
				return fatal
			}
			if serr := s.sleep(ctx, delay); serr != nil {
				return wrapContext(serr)
			}
		}
	}
}

// Client returns the extension the supervisor is currently running.
func (s *Supervisor) Client() (*Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Disabled {
		return nil, s.disabledErrorLocked()
	}
	if s.client == nil {
		return nil, fmt.Errorf("%w: supervisor is not running the extension", ErrNotRunning)
	}
	return s.client, nil
}

// State returns a copy of the current supervisor state.
func (s *Supervisor) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Disable is the kill switch: it stops the supervisor from starting the
// extension again, closes the running client, and persists the decision so a
// core restart does not undo it.
func (s *Supervisor) Disable(reason string) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("%w: supervisor closed", ErrClosed)
	}
	client := s.client
	s.client = nil
	s.killed = true
	s.state.Disabled = true
	s.state.Reason = reason
	s.state.DisabledAt = s.now()
	s.state.UpdatedAt = s.state.DisabledAt
	st := s.state
	s.signalKillLocked()
	s.mu.Unlock()

	if err := s.store.Save(st); err != nil {
		return err
	}
	if client != nil {
		return client.Close()
	}
	return nil
}

// Enable clears the disabled state and the failure streak. It is the operator's
// "I fixed it, try again" action; it does not start anything, so call Run
// afterwards.
func (s *Supervisor) Enable() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("%w: supervisor closed", ErrClosed)
	}
	s.state.Disabled = false
	s.state.Reason = ""
	s.state.Attempts = 0
	s.state.DisabledAt = time.Time{}
	s.state.UpdatedAt = s.now()
	s.attempts = 0
	if s.killed {
		s.killed = false
		s.kill = make(chan struct{})
	}
	st := s.state
	s.mu.Unlock()
	return s.store.Save(st)
}

// Probe performs one launch plus health check and, if both succeed, clears the
// disabled state. Unlike Enable it proves the extension works before re-arming
// it, so an operator can retry a crash-looping extension safely.
func (s *Supervisor) Probe(ctx context.Context) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return fmt.Errorf("%w: supervisor closed", ErrClosed)
	}
	client, err := s.start(ctx)
	if err != nil {
		return err
	}
	_ = client.Close()

	s.mu.Lock()
	s.state.Disabled = false
	s.state.Reason = ""
	s.state.Attempts = 0
	s.state.DisabledAt = time.Time{}
	s.state.UpdatedAt = s.now()
	s.attempts = 0
	st := s.state
	s.mu.Unlock()
	return s.store.Save(st)
}

// Close stops the supervisor and the running extension.
func (s *Supervisor) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	client := s.client
	s.client = nil
	s.signalKillLocked()
	s.mu.Unlock()
	if client != nil {
		return client.Close()
	}
	return nil
}

// runnable reports why the supervisor must not start anything.
func (s *Supervisor) runnable(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return wrapContext(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return fmt.Errorf("%w: supervisor closed", ErrClosed)
	case s.state.Disabled:
		return s.disabledErrorLocked()
	}
	return nil
}

func (s *Supervisor) killCh() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kill
}

func (s *Supervisor) signalKillLocked() {
	select {
	case <-s.kill:
	default:
		close(s.kill)
	}
}

func (s *Supervisor) disabledError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.disabledErrorLocked()
}

func (s *Supervisor) disabledErrorLocked() error {
	if s.state.Reason != "" {
		return fmt.Errorf("%w: %s", ErrDisabled, s.state.Reason)
	}
	return ErrDisabled
}

// start launches one process and health-checks it. A restart that cannot pass a
// health check is a failed restart, not a running extension.
func (s *Supervisor) start(ctx context.Context) (*Client, error) {
	client, err := s.spec.Launch(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.health(ctx, client); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("health check after start: %w", err)
	}
	return client, nil
}

func (s *Supervisor) health(ctx context.Context, client *Client) error {
	hctx, cancel := context.WithTimeout(ctx, s.spec.HealthTimeout)
	defer cancel()
	var res HealthResult
	if err := client.Call(hctx, MethodHealth, &HealthParams{}, &res); err != nil {
		return err
	}
	if res.Status != StatusOK {
		return fmt.Errorf("%w: health status %q", ErrRejected, res.Status)
	}
	return nil
}

func (s *Supervisor) publish(client *Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.client = client
	s.startedAt = s.now()
}

func (s *Supervisor) release(client *Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == client {
		s.client = nil
	}
}

// recordFailure counts a failed start or health check and returns the backoff to
// wait before the next attempt. A non-nil error means the supervisor has given
// up and the extension is now disabled.
func (s *Supervisor) recordFailure(cause error) (time.Duration, error) {
	s.mu.Lock()
	s.attempts++
	attempts := s.attempts
	s.state.Attempts = attempts
	s.state.UpdatedAt = s.now()
	if attempts >= s.spec.MaxAttempts {
		s.state.Disabled = true
		s.state.Reason = fmt.Sprintf("disabled after %d consecutive failures, last: %v", attempts, cause)
		s.state.DisabledAt = s.now()
		st := s.state
		s.signalKillLocked()
		s.mu.Unlock()
		if err := s.store.Save(st); err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("%w: %s", ErrDisabled, st.Reason)
	}
	delay := s.backoffLocked(attempts)
	st := s.state
	s.mu.Unlock()
	if err := s.store.Save(st); err != nil {
		return 0, err
	}
	return delay, nil
}

// recordCrash counts a crash of a client that had passed its health check.
func (s *Supervisor) recordCrash(cause error) (time.Duration, error) {
	s.mu.Lock()
	uptime := s.now().Sub(s.startedAt)
	if uptime >= s.spec.StableAfter {
		// It ran long enough to count as a working extension, so this crash
		// starts a fresh streak instead of continuing an old one.
		s.attempts = 1
	} else {
		s.attempts++
	}
	attempts := s.attempts
	s.state.Attempts = attempts
	s.state.UpdatedAt = s.now()
	if attempts >= s.spec.MaxAttempts {
		s.state.Disabled = true
		s.state.Reason = fmt.Sprintf("disabled after %d consecutive crashes, last: %v", attempts, cause)
		s.state.DisabledAt = s.now()
		st := s.state
		s.signalKillLocked()
		s.mu.Unlock()
		if err := s.store.Save(st); err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("%w: %s", ErrDisabled, st.Reason)
	}
	delay := s.backoffLocked(attempts)
	st := s.state
	s.mu.Unlock()
	if err := s.store.Save(st); err != nil {
		return 0, err
	}
	return delay, nil
}

// backoffLocked computes base * 2^(attempts-1), capped, then jittered. The
// caller holds s.mu because the default jitter uses a non-thread-safe PRNG.
func (s *Supervisor) backoffLocked(attempts int) time.Duration {
	nominal := s.spec.BaseDelay
	for i := 1; i < attempts; i++ {
		nominal *= 2
		if nominal >= s.spec.MaxDelay {
			nominal = s.spec.MaxDelay
			break
		}
	}
	if nominal > s.spec.MaxDelay {
		nominal = s.spec.MaxDelay
	}
	if s.spec.Jitter != nil {
		return s.spec.Jitter(nominal)
	}
	if nominal <= 0 {
		return 0
	}
	half := nominal / 2
	span := int64(nominal - half)
	if span <= 0 {
		return nominal
	}
	return half + time.Duration(s.rnd.Int63n(span+1))
}

// wrapContext turns a context error into a protocol sentinel. Callers must never
// have to compare against context.Canceled to understand an extension failure.
func wrapContext(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %v", ErrTimeout, err)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%w: %v", ErrCanceled, err)
	default:
		return err
	}
}
