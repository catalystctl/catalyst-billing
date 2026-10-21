package extproto

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Sentinels callers branch on. A client never returns a bare context error or a
// bare I/O error: every failure is wrapped around one of these, so callers can
// decide between "retry", "give up" and "the extension is broken" without
// inspecting strings.
var (
	// ErrTimeout means the call exceeded the caller's deadline or Spec.CallTimeout.
	ErrTimeout = errors.New("extproto: call timed out")
	// ErrCanceled means the caller's context was canceled.
	ErrCanceled = errors.New("extproto: call canceled")
	// ErrCrashed means the extension process is not running: it failed to start,
	// died, or closed stdout. The client is unusable afterwards.
	ErrCrashed = errors.New("extproto: extension crashed")
	// ErrOversized means the extension wrote a frame larger than Spec.MaxFrame.
	// The client is unusable afterwards.
	ErrOversized = errors.New("extproto: frame exceeds limit")
	// ErrProtocol means the extension violated the wire protocol (bad JSON-RPC
	// envelope, unknown method id, handshake before initialize, ...).
	ErrProtocol = errors.New("extproto: protocol violation")
	// ErrVersionMismatch means the core and the extension share no protocol
	// version.
	ErrVersionMismatch = errors.New("extproto: no common protocol version")
	// ErrRemote means the extension answered with a JSON-RPC error object. The
	// concrete failure is an *Error; use errors.As to read Code and Data.
	ErrRemote = errors.New("extproto: remote error")
	// ErrNotFound means the extension answered CodeNotFound: the payment,
	// resource or webhook it was asked about does not exist.
	ErrNotFound = errors.New("extproto: not found")
	// ErrRejected means the extension refused a business operation (validation
	// failure, unsupported currency, ...). Retrying unchanged will not help.
	ErrRejected = errors.New("extproto: operation rejected")
	// ErrInvalidParams means the caller built a payload the protocol forbids
	// (missing operation key, float money, ...). It is a local programming error.
	ErrInvalidParams = errors.New("extproto: invalid params")
	// ErrInvalidSpec means Launch was given a Spec it cannot use.
	ErrInvalidSpec = errors.New("extproto: invalid spec")
	// ErrClosed means the client was closed, or a previous fatal error left it
	// unusable, so the call was refused without touching the process.
	ErrClosed = errors.New("extproto: client closed")
	// ErrNotInitialized means a call other than initialize/health was attempted
	// before a successful handshake.
	ErrNotInitialized = errors.New("extproto: handshake not completed")
	// ErrDisabled means the supervisor will not start the extension: it is
	// crash-looping, an operator disabled it, or the kill switch was thrown.
	ErrDisabled = errors.New("extproto: extension disabled")
	// ErrNotRunning means the supervisor has no live client to hand out.
	ErrNotRunning = errors.New("extproto: extension not running")
)

// JSON-RPC 2.0 reserved codes, plus the extension-specific range this protocol
// defines. Extensions must use the -32000..-32099 range for their own codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603

	// CodeVersionMismatch is returned by initialize when the extension supports
	// none of the offered protocol versions.
	CodeVersionMismatch = -32001
	// CodeNotInitialized is returned when a method is called before initialize.
	CodeNotInitialized = -32002
	// CodeNotFound is returned for an unknown payment, resource or event.
	CodeNotFound = -32004
	// CodeRejected is returned when a business rule refuses the operation.
	CodeRejected = -32005
	// CodeDeadlineExceeded is returned when the extension gave up because the
	// call's deadline passed.
	CodeDeadlineExceeded = -32006
)

// Error is a JSON-RPC error object returned by an extension.
//
// It matches ErrRemote, and also the more specific sentinel implied by its code
// (ErrVersionMismatch, ErrNotFound, ErrRejected, ErrTimeout), so callers can use
// errors.Is for the common cases and errors.As when they need Code or Data.
type Error struct {
	// Code is the JSON-RPC error code.
	Code int `json:"code"`
	// Message is the human-readable message, already redacted by the extension.
	Message string `json:"message"`
	// Data is extension-specific detail. It is never interpreted by the core.
	Data json.RawMessage `json:"data,omitempty"`
	// Method is the method that failed. It is filled in by the client and is not
	// part of the wire format.
	Method string `json:"-"`
}

// Error renders "gateway.refund: -32005: currency not supported".
func (e *Error) Error() string {
	if e.Method != "" {
		return fmt.Sprintf("%s: %d: %s", e.Method, e.Code, e.Message)
	}
	return fmt.Sprintf("%d: %s", e.Code, e.Message)
}

// sentinel maps a wire code onto the sentinel a caller would branch on.
func (e *Error) sentinel() error {
	switch e.Code {
	case CodeVersionMismatch:
		return ErrVersionMismatch
	case CodeNotFound:
		return ErrNotFound
	case CodeRejected, CodeInvalidParams:
		return ErrRejected
	case CodeDeadlineExceeded:
		return ErrTimeout
	default:
		return ErrRemote
	}
}

// Is reports whether the error matches target. A remote error always matches
// ErrRemote; it matches a more specific sentinel when its code says so.
func (e *Error) Is(target error) bool {
	if target == ErrRemote {
		return true
	}
	return errors.Is(e.sentinel(), target)
}

// Unwrap exposes the sentinel so wrapped remote errors compose with errors.Is
// and errors.Join.
func (e *Error) Unwrap() error { return e.sentinel() }

// CodeOf returns the JSON-RPC code of err if it is (or wraps) an *Error.
func CodeOf(err error) (int, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Code, true
	}
	return 0, false
}
