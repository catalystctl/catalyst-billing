package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Idempotency.
//
// The problem this solves is not "the client sent the request twice". It is "the
// response was lost, so the client does not know whether the money moved". A UI that
// retries a refund on a flaky network is behaving correctly; the server has to make the
// retry safe.
//
// The contract:
//
//   - The key is scoped to (principal, key), so two customers using "retry-1" cannot
//     collide and cannot read each other's stored response.
//   - The key is bound to the route and to a digest of the request body. Replaying with
//     the same digest returns the stored response verbatim. Replaying with a *different*
//     digest is 422: answering a different question with a stale answer would be worse
//     than failing.
//   - The row is claimed with response_code 0 before the handler runs, so a concurrent
//     duplicate is distinguishable from a completed one and gets 409 rather than
//     racing.
//   - A 5xx response is not stored: the effect did not happen (or is unknown), so a
//     retry must be allowed to try again. Storing it would make one transient failure
//     permanent for the lifetime of the key.

const (
	// maxIdempotentBody bounds the body that is buffered to compute the digest. A
	// mutation body larger than this is a bug or an attack; both are rejected.
	maxIdempotentBody = 1 << 20
	// idempotencyTTL is how long a stored response is replayed. It matches the
	// retention policy on the table.
	idempotencyTTL = 24 * time.Hour
	// minIdempotencyKeyLen rejects a key too short to be unique.
	minIdempotencyKeyLen = 8
	// maxIdempotencyKeyLen bounds the stored key.
	maxIdempotencyKeyLen = 200
)

// idempotencyHeader is the request header a client sends.
const idempotencyHeader = "Idempotency-Key"

// withIdempotency wraps a mutation.
func (s *Server) withIdempotency(rt Route, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(r.Header.Get(idempotencyHeader))
		if key == "" {
			if rt.IdempotencyRequired {
				writeProblem(w, http.StatusBadRequest, "idempotency_key_required",
					"this action requires an "+idempotencyHeader+" header")
				return
			}
			next(w, r)
			return
		}
		if len(key) < minIdempotencyKeyLen || len(key) > maxIdempotencyKeyLen {
			writeProblem(w, http.StatusBadRequest, "invalid_idempotency_key",
				"the idempotency key must be between 8 and 200 characters")
			return
		}
		if s.pool == nil {
			// Without a database there is nowhere to store the response, and pretending
			// the request is idempotent would be a lie. The handler will fail anyway.
			next(w, r)
			return
		}

		principal := principalID(r)
		if principal == "" {
			writeProblem(w, http.StatusUnauthorized, "authentication_required",
				"an idempotency key needs an identified principal")
			return
		}

		// Buffer the body so it can be digested and then replayed to the handler.
		body, err := io.ReadAll(io.LimitReader(r.Body, maxIdempotentBody+1))
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "unreadable_body", "could not read the request body")
			return
		}
		if len(body) > maxIdempotentBody {
			writeProblem(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				"the request body is too large")
			return
		}
		digest := requestDigest(rt, body)
		r.Body = io.NopCloser(bytes.NewReader(body))

		ctx := r.Context()

		// Claim the key. A row inserted here is ours; an existing row is either a replay
		// or a concurrent duplicate.
		var claimed bool
		err = s.pool.QueryRow(ctx, `
			INSERT INTO idempotency_keys
				(key, principal_id, route, request_hash, response_code, response_body, expires_at)
			VALUES ($1, $2, $3, $4, 0, '{}'::jsonb, $5)
			ON CONFLICT (principal_id, key) DO NOTHING
			RETURNING true`,
			key, principal, rt.Path(), digest, time.Now().UTC().Add(idempotencyTTL)).Scan(&claimed)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			s.cfg.Logger.ErrorContext(ctx, "idempotency claim failed", "error", err)
			writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable",
				"the request could not be recorded; retry")
			return
		}

		if !claimed {
			s.replayIdempotent(w, r, principal, key, rt, digest)
			return
		}

		rec := &idempotentRecorder{ResponseWriter: w}
		next(rec, r)

		// Only a definitive, JSON response is stored. A 5xx means the effect is unknown,
		// so a retry must be allowed to run the handler again.
		if rec.status >= 200 && rec.status < 500 && rec.wroteJSON() {
			var payload json.RawMessage = rec.body.Bytes()
			if len(payload) == 0 || !json.Valid(payload) {
				payload = json.RawMessage(`{}`)
			}
			if _, uerr := s.pool.Exec(ctx, `
				UPDATE idempotency_keys
				SET response_code = $3, response_body = $4
				WHERE principal_id = $1 AND key = $2`,
				principal, key, rec.status, payload); uerr != nil {
				// The action succeeded; failing the response now would be worse than
				// losing the replay. Log it so the gap is visible.
				s.cfg.Logger.ErrorContext(ctx, "idempotency store failed",
					"route", rt.Path(), "error", uerr)
			}
		} else {
			// Release the claim so a retry can proceed.
			if _, derr := s.pool.Exec(ctx, `
				DELETE FROM idempotency_keys WHERE principal_id = $1 AND key = $2`,
				principal, key); derr != nil {
				s.cfg.Logger.ErrorContext(ctx, "idempotency release failed", "error", derr)
			}
		}
	}
}

// replayIdempotent answers a duplicate request from the stored response.
func (s *Server) replayIdempotent(w http.ResponseWriter, r *http.Request,
	principal, key string, rt Route, digest []byte) {
	var (
		route        string
		storedHash   []byte
		responseCode int
		responseBody []byte
	)
	err := s.pool.QueryRow(r.Context(), `
		SELECT route, request_hash, response_code, response_body
		FROM idempotency_keys
		WHERE principal_id = $1 AND key = $2`, principal, key).
		Scan(&route, &storedHash, &responseCode, &responseBody)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "idempotency lookup failed", "error", err)
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable",
			"the request could not be checked; retry")
		return
	}

	if route != rt.Path() || !bytes.Equal(storedHash, digest) {
		// Same key, different question. Answering with the stored response would answer
		// the wrong question convincingly.
		writeProblem(w, http.StatusUnprocessableEntity, "idempotency_key_reuse",
			"this idempotency key was already used for a different request")
		return
	}
	if responseCode == 0 {
		writeProblem(w, http.StatusConflict, "idempotency_in_progress",
			"an identical request is still being processed")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Idempotent-Replayed", "true")
	w.WriteHeader(responseCode)
	_, _ = w.Write(responseBody)
}

// requestDigest binds the key to the route and the exact body.
func requestDigest(rt Route, body []byte) []byte {
	h := sha256.New()
	h.Write([]byte(rt.Path()))
	h.Write([]byte{0})
	h.Write(body)
	return h.Sum(nil)
}

// principalID is the identity an idempotency key is scoped to.
func principalID(r *http.Request) string {
	if key, ok := apiKeyFrom(r.Context()); ok {
		return key.UserID
	}
	if user, ok := userFrom(r.Context()); ok {
		return user.ID
	}
	return ""
}

// idempotentRecorder captures the response so it can be stored.
type idempotentRecorder struct {
	http.ResponseWriter
	status      int
	body        bytes.Buffer
	contentType string
}

func (r *idempotentRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
		r.contentType = r.Header().Get("Content-Type")
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *idempotentRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
		r.contentType = r.Header().Get("Content-Type")
	}
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

// wroteJSON reports whether the captured response is JSON, which is the only shape that
// can be replayed byte-for-byte without re-deriving headers.
func (r *idempotentRecorder) wroteJSON() bool {
	return strings.Contains(r.contentType, "json")
}
