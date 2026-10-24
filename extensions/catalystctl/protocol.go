package main

// Small shared helpers: the protocol's vocabulary, the mapping from a panel's words to it, and the
// error translation the core depends on.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/extproto"
)

// The protocol's own lifecycle words, taken from the package that defines the wire form.
const (
	resourceProvisioning = extproto.ResourceProvisioning
	resourceActive       = extproto.ResourceActive
	resourceSuspended    = extproto.ResourceSuspended
	resourceTerminated   = extproto.ResourceTerminated
)

// statusFor translates a panel server into the protocol's lifecycle vocabulary.
//
// The panel's own word is preserved in the `state` field of every reply, so this mapping is for
// callers that need to branch — the core's reconcile loop — and not for display. A suspended
// server is `suspended` even though the panel also calls it `stopped`, because that distinction is
// the one that decides whether a resume will work.
func statusFor(srv panelServer) extproto.ResourceStatus {
	if srv.SuspendedAt != nil {
		return resourceSuspended
	}
	return statusForState(srv.Status)
}

// statusForState translates a bare state word.
//
// An unrecognised state is `provisioning` rather than `active`: a panel inventing a state
// ("reinstalling", "migrating") means the resource is not ready to be treated as up, and claiming
// it is active would let a renewal proceed against a server that is not serving anyone.
//
// A *stopped* server is still `active` in the billing vocabulary, and that is not a mistake: the
// customer is paying for a server that exists and they can start. Billing lifecycle and power
// state are different questions, which is why the reply carries both `status` and `state`.
func statusForState(state string) extproto.ResourceStatus {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "running", "started", "stopped", "offline", "exited":
		return resourceActive
	case "suspended":
		return resourceSuspended
	case "deleted", "terminated":
		return resourceTerminated
	default:
		// starting, stopping, restarting, installing, pending, provisioning, created, and anything
		// a future panel invents.
		return resourceProvisioning
	}
}

// powerActionsFor reports which actions the panel would accept right now.
//
// A suspended server offers nothing: the panel refuses every power call on it, and a button that
// exists to produce a refusal is worse than a sentence explaining why. A transitional state offers
// nothing either, for the same reason — except kill, which is the only thing that works on a server
// wedged mid-transition.
func powerActionsFor(srv panelServer) []string {
	if srv.SuspendedAt != nil {
		return nil
	}
	switch strings.ToLower(srv.Status) {
	case "running", "started":
		return []string{"stop", "restart", "kill"}
	case "stopped", "offline", "exited":
		return []string{"start"}
	case "starting", "stopping", "restarting", "installing":
		return []string{"kill"}
	default:
		return nil
	}
}

// nextState is the state an action is heading towards.
//
// The panel applies power changes asynchronously, so the reply states the intent rather than a fact
// it has not observed. The next read confirms it.
func nextState(action, current string) string {
	switch action {
	case "start", "restart":
		return "starting"
	case "stop":
		return "stopping"
	case "kill":
		return "stopped"
	default:
		return current
	}
}

// validPowerAction reports whether the action is one of the four the protocol defines.
func validPowerAction(action string) bool {
	switch action {
	case "start", "stop", "restart", "kill":
		return true
	}
	return false
}

// validateKey rejects an operation key the protocol cannot use as an idempotency token.
func validateKey(key string) error { return extproto.ValidateOperationKey(key) }

// contextFor gives one call its own deadline.
//
// The protocol's CallMeta carries the caller's deadline, but it is advisory: an adapter that
// trusted it and nothing else would hang forever against a core that did not set one. This bounds
// the call from the adapter's own side, which is the only side it controls.
func contextFor(req *request) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(60 * time.Second)
	var p struct {
		Deadline time.Time `json:"deadline"`
	}
	if len(req.Params) > 0 && json.Unmarshal(req.Params, &p) == nil && !p.Deadline.IsZero() {
		// Honour the caller's deadline when it is sooner, so the core's timeout is not exceeded by
		// an adapter that kept working.
		if p.Deadline.Before(deadline) {
			deadline = p.Deadline
		}
	}
	return context.WithDeadline(context.Background(), deadline)
}

// internalError renders a retryable failure.
//
// -32000 is the core's "retryable" code: the queue tries again with the same operation key, which
// is safe because every mutating call here is idempotent.
func internalError(err error) *rpcError {
	return &rpcError{Code: codeInternal, Message: err.Error()}
}

// mapPanelError translates a panel failure into the protocol's error vocabulary.
//
// The distinction is load-bearing: -32004 ("not found") is authoritative and lets the core conclude
// a resource is gone; -32005 ("rejected") means retrying unchanged will not help; -32000 means try
// again. Flattening them is how an adapter tells the core to provision a second server.
func mapPanelError(err error) *rpcError {
	if err == nil {
		return nil
	}
	var pe *panelError
	if errors.As(err, &pe) {
		switch {
		case pe.notFound():
			return &rpcError{Code: codeNotFound, Message: pe.Error()}
		case pe.status == http.StatusUnauthorized || pe.status == http.StatusForbidden:
			// A credential or permission problem is a deployment fault, not a business refusal: it
			// is retryable once an operator fixes the key, and it must not read as "the customer may
			// not do this".
			return &rpcError{Code: codeInternal, Message: pe.Error()}
		case pe.status == http.StatusConflict || pe.status == http.StatusBadRequest ||
			pe.status == http.StatusUnprocessableEntity || pe.locked():
			return &rpcError{Code: codeRejected, Message: pe.Error()}
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, context.DeadlineExceeded) {
		return &rpcError{Code: codeInternal, Message: err.Error()}
	}
	return &rpcError{Code: codeInternal, Message: err.Error()}
}

// isNotFound reports whether the panel said the resource does not exist.
func isNotFound(err error) bool {
	var pe *panelError
	return errors.As(err, &pe) && pe.notFound()
}

// isRejection reports whether the panel refused the request on its merits, so a retry would fail
// the same way.
func isRejection(err error) bool {
	var pe *panelError
	if !errors.As(err, &pe) {
		return false
	}
	return pe.status == http.StatusBadRequest || pe.status == http.StatusConflict ||
		pe.status == http.StatusUnprocessableEntity || pe.status == http.StatusForbidden
}

// stateResult renders a suspend/resume/terminate reply.
func stateResult(externalID, status string, changed bool) map[string]any {
	return map[string]any{"external_id": externalID, "status": status, "changed": changed}
}

// readLine reads one newline-terminated frame, bounded by max.
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > max {
			return nil, fmt.Errorf("frame exceeds %d bytes", max)
		}
		buf = append(buf, chunk...)
		if err == nil {
			return trimNewline(buf), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return nil, err
	}
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// defaultTestTimeout bounds a fake-panel call in tests.
const defaultTestTimeout = 5 * time.Second
