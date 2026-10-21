package extproto

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// JSON-RPC envelope types. They are unexported because the payload types in
// spec.go are the public surface: nothing outside this package should have to
// think about ids or envelope fields.

const jsonrpcVersion = "2.0"

// wireRequest is an outgoing call.
type wireRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      uint64 `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// wireNotification is an outgoing call that expects no answer.
type wireNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// wireResponse is an incoming answer to a wireRequest.
type wireResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *uint64         `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *wireError      `json:"error"`
}

// wireError is the JSON-RPC error object.
type wireError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// errFrameTooLarge is the internal signal that a frame exceeded Spec.MaxFrame.
// It never escapes the package: readLoop turns it into ErrOversized.
var errFrameTooLarge = errors.New("extproto: frame exceeds limit")

// readFrame reads exactly one newline-terminated frame and returns it without
// the terminator. It enforces max while reading, so an extension that never
// sends a newline cannot make the core allocate without bound.
func readFrame(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > max {
			return nil, errFrameTooLarge
		}
		// ReadSlice's chunk is only valid until the next read, so it must be
		// copied before looping.
		buf = append(buf, chunk...)
		if err == nil {
			return bytes.TrimRight(buf, "\r\n"), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return nil, err
	}
}

// ringBuffer is a fixed-size, line-aligned tail of the extension's stderr.
//
// stderr is captured rather than inherited so that a chatty or hostile extension
// cannot flood the core's logs, and so the text can be passed through Spec.Redact
// before anyone stores it.
type ringBuffer struct {
	mu     sync.Mutex
	buf    []byte
	max    int
	redact func(string) string
}

func newRingBuffer(max int, redact func(string) string) *ringBuffer {
	return &ringBuffer{max: max, redact: redact}
}

// Write appends p, redacted, keeping only the last max bytes. It never fails;
// the error return only satisfies io.Writer.
func (r *ringBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	if r.redact != nil {
		p = []byte(r.redact(string(p)))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) <= r.max {
		return n, nil
	}
	keep := r.buf[len(r.buf)-r.max:]
	// Drop the first partial line so the buffer starts at a line boundary.
	if i := bytes.IndexByte(keep, '\n'); i >= 0 && i+1 < len(keep) {
		keep = keep[i+1:]
	}
	r.buf = append(r.buf[:0], keep...)
	return n, nil
}

// String returns the retained tail.
func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}

var _ io.Writer = (*ringBuffer)(nil)
