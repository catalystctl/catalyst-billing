// Request observability: what this process served, and what it refused with.
//
// The two writers in logs.go had no caller. The /logs screen has an error tab and an HTTP tab,
// and in a real deployment both would stay empty forever — an operator opening the screen to
// find out why a customer saw a 500 would find an empty table and conclude nothing was wrong.
// This file is the wiring.
//
// The seam is the middleware chain in Handler(), not the handlers. A call per handler is a list
// that grows a hole the first time somebody adds an endpoint without knowing this file exists,
// and its completeness cannot be proved. Every request already passes through withRequestLog,
// and every refusal is already rendered by writeProblem, which is the one place in the server
// where a status and its machine code exist together.
//
// The write is in-request, after the response, bounded by logWriteTimeout, with the error
// discarded. A bounded background queue was the alternative and was rejected: a queue drops rows
// when it is full and loses whatever it is holding at exit, which makes the log least reliable
// exactly when the database is struggling and the operator needs it most. In-request, the row is
// written before the caller's connection is done with, so the log cannot be stale — and the two
// properties that matter still hold, because the response has already been written when the write
// happens (so it cannot change the answer) and the error is dropped (so a failed write is a lost
// row, never a failed request). The timeout is what keeps a struggling database from holding a
// request open: past it the row is lost and the counter on /metrics says so.
package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// logWriteTimeout bounds one insert.
//
// It is a ceiling, not a target: a single-row INSERT against a healthy database is three orders
// of magnitude faster. What it buys is a guarantee that logging can never be the reason a request
// takes seconds — when the database is slow enough to hit this, the row is lost instead.
const logWriteTimeout = 250 * time.Millisecond

// statusRecorder observes one response as it is written.
//
// It is also where the two facts the outer wrapper cannot see are noted — the identity
// loadSession resolved and the correlation id withCorrelation minted — because both live only in
// the derived request context, which exists between the wrappers and nowhere the outermost one
// can read back.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int
	// method and path are what the request was, kept here because writeProblem receives only
	// the writer: the error row has to be actionable without a second lookup.
	method    string
	path      string
	srv       *Server
	actorID   string
	requestID string
}

// Flush forwards to the underlying writer.
//
// Without this, wrapping the ResponseWriter hides http.Flusher, and the SSE handler refuses
// to stream ("this server cannot stream responses") even though the real writer can. The
// type assertion in that handler is a deliberate guard against a wrapper that silently
// buffers; the wrapper's job is therefore to forward, not to hide.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer to http.ResponseController, which is how a handler
// reaches SetWriteDeadline or Hijack through a wrapper.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.written += n
	return n, err
}

// withRequestLog records one row per served request, and logs one line per request with a
// duration, without the body.
//
// It is the outermost wrapper so a panic recovered inside it is still a row: the request that
// crashed a handler is the first one an operator opens /logs to find. It sits outside loadSession
// for the same reason — a request refused before the mux (an invalid API key, an impersonated
// session reaching for money) still reached this server and belongs in the record.
func (s *Server) withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{
			ResponseWriter: w,
			status:         http.StatusOK,
			method:         r.Method,
			path:           r.URL.Path,
			srv:            s,
		}
		next.ServeHTTP(rec, r)
		elapsed := time.Since(start)

		s.cfg.Logger.InfoContext(r.Context(), "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", elapsed.Milliseconds(),
			"bytes", rec.written,
		)
		// Only what the table has columns for. No header map and no body: an Authorization
		// token or a session cookie copied into this row is copied into a backup, a replica and
		// a pg_dump as well, and the viewer is readable by any support agent holding log:read.
		// WriteHTTP strips the query string for the same reason.
		s.recordHTTP(r, rec, elapsed)
	})
}

// recordHTTP writes the row for a served request.
func (s *Server) recordHTTP(r *http.Request, rec *statusRecorder, elapsed time.Duration) {
	if s == nil || s.pool == nil {
		return
	}
	// The request context is carried for its values only: a client that hung up is not a reason
	// to lose the row describing the request it hung up on.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), logWriteTimeout)
	defer cancel()
	err := WriteHTTP(ctx, s.pool, HTTPEntry{
		Method:     r.Method,
		Path:       r.URL.Path,
		Status:     rec.status,
		DurationMS: int(elapsed.Milliseconds()),
		ActorID:    rec.actorID,
		IP:         s.clientIP(r),
	})
	if err != nil {
		s.noteLogFailure(err)
	}
}

// noteRequestContext notes the facts only the inner chain can see.
//
// It is inside loadSession because that is the first point at which an identity exists, and the
// outer observer cannot make the same observation for itself: the context loadSession derives is
// passed down, never back up.
func (s *Server) noteRequestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec, ok := w.(*statusRecorder); ok {
			if user, ok := userFrom(r.Context()); ok {
				rec.actorID = user.ID
			}
			rec.requestID = correlationID(r.Context())
		}
		next.ServeHTTP(w, r)
	})
}

// recordProblem mirrors a rendered problem into error_logs.
//
// It is called from writeProblem and nowhere else, so the error log cannot be incomplete by
// accident: every refusal in this surface goes through that one function, including the ones the
// router's own guards produce and the one withRecovery produces for a panic.
func recordProblem(w http.ResponseWriter, status int, code, detail string) {
	rec := problemRecorder(w)
	if rec == nil || rec.srv == nil || !worthRecording(status) {
		return
	}
	rec.srv.recordError(rec, status, code, detail)
}

// recordError writes the row for a rendered problem.
func (s *Server) recordError(rec *statusRecorder, status int, code, detail string) {
	level := "warn"
	if status >= 500 {
		level = "error"
	}
	// The method and path are already on the request's HTTP row; repeating them here is what
	// makes an error row actionable on its own, when the operator found it by searching rather
	// than by scrolling. The request id ties the two together, and the detail is redacted by
	// WriteError before it is stored.
	detail = fmt.Sprintf("%s %s returned %d: %s", rec.method, rec.path, status, detail)
	if rec.requestID != "" {
		detail += " (request " + rec.requestID + ")"
	}
	ctx, cancel := context.WithTimeout(context.Background(), logWriteTimeout)
	defer cancel()
	err := WriteError(ctx, s.pool, ErrorEntry{
		Level:   level,
		Source:  "http.request",
		Message: code,
		Detail:  detail,
	})
	if err != nil {
		s.noteLogFailure(err)
	}
}

// noteLogFailure counts a row the database refused.
//
// The failure cannot be returned to the caller — the response is already written — and it must
// not be a warning per request, because a database outage would turn a log-table problem into a
// log flood. The counter on /metrics is the signal; the debug line is for chasing one row.
func (s *Server) noteLogFailure(err error) {
	s.logWriteFailures.Add(1)
	s.cfg.Logger.Debug("a log row was lost", "error", err)
}

// worthRecording decides which rendered problems become rows.
//
// Every 5xx is a defect on this side and is recorded. A 4xx is recorded as a warning only when it
// says the caller's request was understood and rejected — a client broken in a way an operator can
// act on. The four statuses below are excluded because they are the surface working as intended and
// they are the bulk of the traffic: 401 and 403 are the guards turning away anonymous callers and
// permission probes, 404 is a scanner or a stale link, and 429 is the rate limiter doing its job. A
// row for each of those would bury the row that matters, which is the failure this log exists to
// prevent.
func worthRecording(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests:
		return false
	}
	return status >= 400
}

// problemRecorder finds the observer behind a response writer.
//
// A handler does not always receive the observer directly: withIdempotency substitutes its own
// recorder, and a future wrapper would too. The walk follows Unwrap where a wrapper offers one and
// knows the idempotency recorder by type, because a problem rendered behind it is still a problem
// this server produced.
func problemRecorder(w http.ResponseWriter) *statusRecorder {
	for w != nil {
		switch rec := w.(type) {
		case *statusRecorder:
			return rec
		case *idempotentRecorder:
			w = rec.ResponseWriter
		case interface{ Unwrap() http.ResponseWriter }:
			w = rec.Unwrap()
		default:
			return nil
		}
	}
	return nil
}
