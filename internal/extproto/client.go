package extproto

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults applied by Launch when a Spec leaves a field zero.
const (
	// DefaultMaxFrame is the largest frame the core will read. It is generous
	// for a protocol whose payloads are a few kilobytes and small enough that a
	// runaway extension cannot exhaust memory.
	DefaultMaxFrame = 1 << 20
	// DefaultHandshakeTimeout bounds process start plus initialize.
	DefaultHandshakeTimeout = 10 * time.Second
	// DefaultCallTimeout bounds a single call when the caller's context has no
	// deadline of its own.
	DefaultCallTimeout = 30 * time.Second

	// stderrLimit is how much of the extension's stderr is retained.
	stderrLimit = 8 << 10
	// minFrame is the smallest MaxFrame Launch accepts: an envelope, an id and
	// a result must fit with room to spare.
	minFrame = 1024
	// writeQueue is how many frames may be queued before a caller blocks.
	writeQueue = 64
)

// Spec describes how to start and talk to one extension process.
type Spec struct {
	// Path is the executable to start. Required.
	Path string
	// Args are passed as-is.
	Args []string
	// Env are "KEY=value" entries added to the child's environment. The child
	// inherits PATH and nothing else: the core's own environment never leaks into
	// an extension, because it contains database URLs and API keys.
	Env []string
	// Dir is the working directory; empty means the core's.
	Dir string
	// MaxFrame is the largest accepted frame in bytes; zero means
	// DefaultMaxFrame. A larger frame is a protocol violation and kills the
	// connection.
	MaxFrame int
	// HandshakeTimeout bounds process start plus initialize; zero means
	// DefaultHandshakeTimeout.
	HandshakeTimeout time.Duration
	// CallTimeout bounds each call; zero means DefaultCallTimeout. The effective
	// deadline is min(ctx deadline, CallTimeout).
	CallTimeout time.Duration
	// CoreVersion is reported to the extension in initialize, for diagnostics.
	CoreVersion string
	// Redact is applied to captured stderr before it is stored, so secrets the
	// extension prints by accident never reach the core's logs. nil means no
	// redaction.
	Redact func(string) string
}

func (s Spec) withDefaults() (Spec, error) {
	if s.Path == "" {
		return Spec{}, fmt.Errorf("%w: Path is empty", ErrInvalidSpec)
	}
	if s.MaxFrame == 0 {
		s.MaxFrame = DefaultMaxFrame
	}
	if s.MaxFrame < minFrame {
		return Spec{}, fmt.Errorf("%w: MaxFrame %d is below the %d byte minimum", ErrInvalidSpec, s.MaxFrame, minFrame)
	}
	if s.HandshakeTimeout <= 0 {
		s.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if s.CallTimeout <= 0 {
		s.CallTimeout = DefaultCallTimeout
	}
	if s.Redact == nil {
		s.Redact = func(v string) string { return v }
	}
	return s, nil
}

// result is what the reader goroutine hands to a waiting caller.
type result struct {
	resp *wireResponse
	err  error
}

// Client is a live connection to one extension process.
//
// A Client is safe for concurrent use: ids are allocated atomically and every
// call owns its own response channel. It is single-use — once it fails (crash,
// protocol violation, oversized frame, or Close) it stays failed and every later
// call returns the recorded error instead of hanging.
type Client struct {
	spec Spec
	cmd  *exec.Cmd

	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	maxFrame    int
	callTimeout time.Duration

	writeCh chan []byte
	quit    chan struct{}
	done    chan struct{}

	failOnce  sync.Once
	closeOnce sync.Once
	closeErr  error

	mu      sync.Mutex
	pending map[uint64]chan result
	fatal   error
	init    InitializeResult
	ready   bool

	nextID    atomic.Uint64
	stderrBuf *ringBuffer
	wg        sync.WaitGroup
}

// Launch starts an extension and performs the handshake.
//
// ctx governs the launch: if it is canceled or exceeds Spec.HandshakeTimeout the
// process is killed and Launch fails. It does not govern the client's lifetime —
// use Close for that.
//
// Launch always performs the handshake, because a client that has not negotiated
// a version cannot safely interpret any payload. On any failure the process
// group is killed before the error is returned, so a rejected extension never
// outlives its Launch call.
func Launch(ctx context.Context, spec Spec) (*Client, error) {
	return launch(ctx, spec, true)
}

func launch(ctx context.Context, spec Spec, handshake bool) (*Client, error) {
	spec, err := spec.withDefaults()
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = cleanEnv(spec.Env)
	setProcessGroup(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("%w: stdin pipe: %v", ErrCrashed, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("%w: stdout pipe: %v", ErrCrashed, err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("%w: stderr pipe: %v", ErrCrashed, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%w: start %q: %v", ErrCrashed, spec.Path, err)
	}

	c := &Client{
		spec:        spec,
		cmd:         cmd,
		stdin:       stdin,
		stdout:      stdout,
		stderr:      stderrPipe,
		maxFrame:    spec.MaxFrame,
		callTimeout: spec.CallTimeout,
		writeCh:     make(chan []byte, writeQueue),
		quit:        make(chan struct{}),
		done:        make(chan struct{}),
		pending:     make(map[uint64]chan result),
		stderrBuf:   newRingBuffer(stderrLimit, spec.Redact),
	}
	c.wg.Add(4)
	go c.writeLoop()
	go c.readLoop()
	go c.stderrLoop()
	go c.waitLoop()

	if handshake {
		if err := c.handshake(ctx); err != nil {
			_ = c.Close()
			return nil, err
		}
	}
	return c, nil
}

// cleanEnv builds the child's environment: PATH plus the explicitly passed
// entries, and nothing else. The core's environment carries credentials, and an
// extension is a third-party program.
func cleanEnv(extra []string) []string {
	env := make([]string, 0, len(extra)+1)
	if p, ok := os.LookupEnv("PATH"); ok {
		env = append(env, "PATH="+p)
	}
	for _, kv := range extra {
		if kv == "" {
			continue
		}
		env = append(env, kv)
	}
	return env
}

// handshake performs version negotiation.
func (c *Client) handshake(ctx context.Context) error {
	if c.spec.HandshakeTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.spec.HandshakeTimeout)
		defer cancel()
	}
	// Every version the core speaks is offered, newest first. An extension that only
	// knows version 1 picks it and is accepted; it simply never sees the version-2
	// surface (event emission), because the core only offers what it can deliver.
	params := &InitializeParams{
		Protocol:    Protocol,
		Versions:    SupportedVersions(),
		CoreVersion: c.spec.CoreVersion,
	}
	return c.initialize(ctx, params, nil)
}

// Call invokes method on the extension and decodes the result into result.
//
// params must be a pointer to one of the payload types in this package (or nil
// for a method that takes no parameters); result may be nil when the caller does
// not care about the answer. The effective deadline is min(ctx deadline,
// Spec.CallTimeout) and is also stamped into the payload's CallMeta so the
// extension can honour it.
//
// Call never returns a bare context error: a deadline produces ErrTimeout, a
// cancellation produces ErrCanceled, and both are wrapped so errors.Is works.
//
// Calling MethodInitialize performs the handshake: the selected version is
// validated and the client becomes usable for every other method.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if c.callTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.callTimeout)
		defer cancel()
	}
	if method == MethodInitialize {
		return c.initialize(ctx, params, result)
	}
	return c.call(ctx, method, params, result)
}

// initialize performs the handshake and records its outcome.
func (c *Client) initialize(ctx context.Context, params, out any) error {
	p, ok := params.(*InitializeParams)
	if !ok || p == nil {
		return fmt.Errorf("%w: initialize needs *InitializeParams", ErrInvalidParams)
	}
	var res InitializeResult
	if err := c.call(ctx, MethodInitialize, p, &res); err != nil {
		return fmt.Errorf("extproto: handshake failed: %w", err)
	}
	if !SupportsVersion(res.Version) {
		// A version the core never offered is a protocol violation, not a
		// negotiation failure the core can paper over: the payloads of that version
		// are unknown, so every later call would be a guess.
		return fmt.Errorf("%w: extension selected version %d, core offers %v",
			ErrVersionMismatch, res.Version, SupportedVersions())
	}
	if res.Name == "" {
		return fmt.Errorf("%w: initialize result has no name", ErrProtocol)
	}
	c.mu.Lock()
	c.init = res
	c.ready = true
	c.mu.Unlock()
	if out == nil {
		return nil
	}
	if dst, ok := out.(*InitializeResult); ok {
		*dst = res
		return nil
	}
	encoded, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("%w: re-encode initialize result: %v", ErrProtocol, err)
	}
	if err := json.Unmarshal(encoded, out); err != nil {
		return fmt.Errorf("%w: decode initialize result: %v", ErrProtocol, err)
	}
	return nil
}

func (c *Client) call(ctx context.Context, method string, params, out any) error {
	if method == "" {
		return fmt.Errorf("%w: empty method", ErrInvalidParams)
	}
	c.mu.Lock()
	ready := c.ready
	c.mu.Unlock()
	// Nothing but the handshake and a liveness probe may run before the
	// extension has agreed on a protocol version: the payloads of any other
	// method are only meaningful once the version is known.
	if !ready && method != MethodInitialize && method != MethodHealth {
		return fmt.Errorf("%w: %s", ErrNotInitialized, method)
	}
	if err := c.fatalErr(); err != nil {
		return err
	}
	if keyed, ok := params.(interface{ OperationKeyValue() string }); ok {
		if err := ValidateOperationKey(keyed.OperationKeyValue()); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrInvalidParams, method, err)
		}
	}
	if d, ok := ctx.Deadline(); ok {
		if setter, ok := params.(interface{ SetDeadline(time.Time) }); ok {
			setter.SetDeadline(d.UTC())
		}
	}

	id := c.nextID.Add(1)
	frame, err := json.Marshal(wireRequest{JSONRPC: jsonrpcVersion, ID: id, Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("%w: marshal %s params: %v", ErrInvalidParams, method, err)
	}
	frame = append(frame, '\n')

	ch := make(chan result, 1)
	c.mu.Lock()
	if c.fatal != nil {
		fatal := c.fatal
		c.mu.Unlock()
		return fatal
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.send(frame); err != nil {
		return err
	}

	select {
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		return decodeResult(method, r.resp, out)
	case <-ctx.Done():
		// Withdraw the call and drop its reply: the id is never reused, so a late
		// response cannot be delivered to another caller.
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		c.notifyCancel(id, method)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%w: %s", ErrTimeout, method)
		}
		return fmt.Errorf("%w: %s", ErrCanceled, method)
	}
}

// Notify sends a notification: a call with no id and therefore no answer.
//
// Notifications are fire-and-forget. The error reports only whether the frame
// could be handed to the extension; there is no remote acknowledgement.
func (c *Client) Notify(method string, params any) error {
	if method == "" {
		return fmt.Errorf("%w: empty method", ErrInvalidParams)
	}
	if err := c.fatalErr(); err != nil {
		return err
	}
	frame, err := json.Marshal(wireNotification{JSONRPC: jsonrpcVersion, Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("%w: marshal %s params: %v", ErrInvalidParams, method, err)
	}
	return c.send(append(frame, '\n'))
}

// notifyCancel tells the extension to abandon a request. It is best effort: the
// caller has already given up, and a dead extension cannot be told anything.
func (c *Client) notifyCancel(id uint64, method string) {
	_ = c.Notify(CancelMethod, &CancelParams{ID: id, Method: method, Reason: "core deadline or cancellation"})
}

// decodeResult turns a response into the caller's result value.
func decodeResult(method string, resp *wireResponse, out any) error {
	if resp.Error != nil {
		return &Error{
			Code:    resp.Error.Code,
			Message: resp.Error.Message,
			Data:    resp.Error.Data,
			Method:  method,
		}
	}
	if len(resp.Result) == 0 {
		return fmt.Errorf("%w: %s response carries neither result nor error", ErrProtocol, method)
	}
	if out == nil || string(resp.Result) == "null" {
		return nil
	}
	if err := json.Unmarshal(resp.Result, out); err != nil {
		return fmt.Errorf("%w: decode %s result: %v", ErrProtocol, method, err)
	}
	return nil
}

// send queues one frame for the writer goroutine.
func (c *Client) send(frame []byte) error {
	select {
	case <-c.quit:
		return c.fatalOr(ErrClosed)
	case <-c.done:
		return c.fatalOr(ErrCrashed)
	default:
	}
	select {
	case c.writeCh <- frame:
		return nil
	case <-c.quit:
		return c.fatalOr(ErrClosed)
	case <-c.done:
		return c.fatalOr(ErrCrashed)
	}
}

func (c *Client) fatalErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fatal
}

func (c *Client) fatalOr(fallback error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fatal != nil {
		return c.fatal
	}
	return fallback
}

// fail records the first fatal error, closes Done and fails every pending call.
// Only the first error is kept: later symptoms of the same failure are noise.
func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.fatal == nil {
		c.fatal = err
	}
	fatal := c.fatal
	pending := c.pending
	c.pending = make(map[uint64]chan result)
	c.mu.Unlock()

	c.failOnce.Do(func() { close(c.done) })
	for _, ch := range pending {
		select {
		case ch <- result{err: fatal}:
		default:
		}
	}
}

// kill terminates the whole process group, so grandchildren die too.
func (c *Client) kill() {
	if c.cmd != nil && c.cmd.Process != nil {
		_ = killProcessGroup(c.cmd.Process)
	}
}

func (c *Client) writeLoop() {
	defer c.wg.Done()
	for {
		select {
		case <-c.quit:
			return
		case frame := <-c.writeCh:
			if _, err := c.stdin.Write(frame); err != nil {
				c.fail(fmt.Errorf("%w: write to extension: %v", ErrCrashed, err))
				return
			}
		}
	}
}

func (c *Client) readLoop() {
	defer c.wg.Done()
	r := bufio.NewReaderSize(c.stdout, 64<<10)
	for {
		frame, err := readFrame(r, c.maxFrame)
		if err != nil {
			switch {
			case errors.Is(err, errFrameTooLarge):
				c.fail(fmt.Errorf("%w: limit is %d bytes", ErrOversized, c.maxFrame))
				c.kill()
			case errors.Is(err, io.EOF):
				c.fail(fmt.Errorf("%w: extension closed stdout", ErrCrashed))
			default:
				c.fail(fmt.Errorf("%w: read stdout: %v", ErrCrashed, err))
			}
			return
		}
		if len(frame) == 0 {
			c.fail(fmt.Errorf("%w: empty frame", ErrProtocol))
			c.kill()
			return
		}
		var resp wireResponse
		if err := json.Unmarshal(frame, &resp); err != nil {
			c.fail(fmt.Errorf("%w: unparseable frame: %v", ErrCrashed, err))
			c.kill()
			return
		}
		if resp.JSONRPC != jsonrpcVersion {
			c.fail(fmt.Errorf("%w: frame has jsonrpc %q", ErrProtocol, resp.JSONRPC))
			c.kill()
			return
		}
		if resp.ID == nil {
			// Server-initiated notifications are not part of protocol version 1.
			// Ignore them rather than kill: a newer extension may stream progress
			// to an older core, and dropping it is harmless.
			continue
		}
		c.mu.Lock()
		ch := c.pending[*resp.ID]
		delete(c.pending, *resp.ID)
		c.mu.Unlock()
		if ch == nil {
			// Late reply for a call the caller already abandoned.
			continue
		}
		select {
		case ch <- result{resp: &resp}:
		default:
		}
	}
}

func (c *Client) stderrLoop() {
	defer c.wg.Done()
	buf := make([]byte, 4096)
	for {
		n, err := c.stderr.Read(buf)
		if n > 0 {
			c.stderrBuf.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (c *Client) waitLoop() {
	defer c.wg.Done()
	err := c.cmd.Wait()
	// An extension that exits while the core still holds the client is broken,
	// whatever its exit code: its half of the conversation is gone.
	c.fail(fmt.Errorf("%w: process exited: %v", ErrCrashed, err))
}

// Capabilities returns the capabilities advertised during the handshake.
func (c *Client) Capabilities() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.init.Capabilities))
	copy(out, c.init.Capabilities)
	return out
}

// Info returns the handshake result: version, identity and capabilities.
func (c *Client) Info() InitializeResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.init
	out.Capabilities = make([]string, len(c.init.Capabilities))
	copy(out.Capabilities, c.init.Capabilities)
	return out
}

// Stderr returns the retained tail of the extension's stderr, already redacted.
// It stays readable after Close, which is what makes it useful in an error path.
func (c *Client) Stderr() string { return c.stderrBuf.String() }

// Done is closed when the client becomes unusable: the process exited, the
// protocol was violated, or Close was called.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns the fatal error that made the client unusable, or nil while it is
// still healthy.
func (c *Client) Err() error { return c.fatalErr() }

// Close stops the extension and releases the client.
//
// It is idempotent and safe to call concurrently with in-flight calls: those
// fail with ErrClosed rather than hanging. Close kills the whole process group,
// so helper processes the extension started die with it.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		// Refuse new work and fail pending calls before touching the process, so
		// no caller is left waiting on a channel nobody will write to.
		c.fail(fmt.Errorf("%w: client closed", ErrClosed))
		close(c.quit)
		c.kill()
		// Unblock the reader goroutines even if the process ignored SIGKILL.
		_ = c.stdin.Close()
		_ = c.stdout.Close()
		_ = c.stderr.Close()
		c.wg.Wait()
	})
	return c.closeErr
}
