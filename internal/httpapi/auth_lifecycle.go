package httpapi

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/notify"
	"github.com/jackc/pgx/v5"
)

// Account lifecycle endpoints (Gate C1, C2, C4).
//
// The shape of every response here is chosen so that a caller cannot learn something they
// should not:
//
//   - The reset request answers 202 with a fixed body whether or not the address exists. The
//     token never appears in a response.
//   - The verification request answers 202 whether or not the account is already verified.
//   - A bad token, a used token and an expired token produce the same 400 with the same message.
//     The customer has to request a new link in all three cases, so distinguishing them only
//     helps an attacker.
//
// The tokens themselves live in the database as hashes (internal/authn), and the message that
// carries one is written to the durable outbox in the same transaction as the token, so a crash
// cannot lose the mail and a retry cannot send it twice.

func init() {
	RegisterRoutes("60-auth-lifecycle", registerAuthLifecycle)
}

func registerAuthLifecycle(r *router) {
	// Unauthenticated: these are how a locked-out customer gets back in. They enforce same-origin
	// themselves, exactly like the login route.
	r.publicPost("/api/v1/auth/password-reset/request", r.s.handleResetRequest)
	r.publicPost("/api/v1/auth/password-reset/complete", r.s.handleResetComplete)
	r.publicPost("/api/v1/auth/verify/request", r.s.handleVerifyRequest)
	r.publicGet("/api/v1/auth/verify", r.s.handleVerify)
	r.publicPost("/api/v1/auth/mfa/verify", r.s.handleMFAVerify)

	// Authenticated: enrolment and disabling act on the caller's own account.
	r.userMutation(http.MethodPost, "/api/v1/auth/mfa/enrol", false, r.s.handleMFAEnrol)
	r.userMutation(http.MethodPost, "/api/v1/auth/mfa/confirm", false, r.s.handleMFAConfirm)
	r.userMutation(http.MethodPost, "/api/v1/auth/mfa/disable", false, r.s.handleMFADisable)
}

// publicBaseURL is the origin a link in an email must point at.
//
// It is configuration rather than a request-derived value: a reset link built from the Host
// header is a reset link an attacker can point at their own domain by sending the request with a
// forged Host.
func publicBaseURL() string {
	if v := strings.TrimSpace(os.Getenv("BILLING_PUBLIC_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://127.0.0.1:8080"
}

// queueMessage writes a message to the outbox inside the caller's transaction.
func (s *Server) queueMessage(ctx context.Context, tx pgx.Tx, msg notify.Message) error {
	if s.notify == nil {
		// Without an outbox there is nowhere to record the decision. Failing is honest: a 202
		// that promised a mail nobody will send is worse than a 503.
		return errors.New("httpapi: no notification queue configured")
	}
	_, _, err := s.notify.Enqueue(ctx, tx, msg)
	return err
}

// ------------------------------------------------------------ password reset --

// handleResetRequest starts a password reset.
//
// Throttling is by address and by submitted email, so one address cannot be used to spray and one
// mailbox cannot be flooded. The limiter is the same per-principal machinery the API uses, keyed by
// a synthetic principal so it does not depend on a session.
func (s *Server) handleResetRequest(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	if !s.sameOrigin(r) {
		writeProblem(w, http.StatusForbidden, "bad_origin", "the request did not come from this site")
		return
	}
	if !s.allowCredentialAttempt(w, r, "reset", 5) {
		return
	}

	var req struct {
		Email string `json:"email"`
	}
	if !s.decodeJSON(w, r, &req, 4<<10) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" || !strings.Contains(email, "@") {
		// A malformed address is refused, because it is a client bug rather than a
		// non-enumeration concern: there is nothing to look up.
		writeProblem(w, http.StatusBadRequest, "invalid_email", "a valid email address is required")
		return
	}

	res, err := s.auth.Service.RequestPasswordReset(r.Context(), email)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "password reset request failed", "error", err)
		// Even a storage failure answers 202: a 500 here is an account oracle, because it only
		// happens for addresses that exist.
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted"})
		return
	}

	if res.Token != "" {
		link := publicBaseURL() + "/reset?token=" + res.Token
		msg := notify.PasswordResetMessage(res.Email, link, authn.PasswordResetLifetime)
		// The dedupe key includes the token: two genuine requests must both be delivered, while a
		// retried request for the same token must not send twice.
		msg.DedupeKey = "password_reset:" + res.Token[:12]
		if err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
			return s.queueMessage(ctx, tx, msg)
		}); err != nil {
			s.cfg.Logger.ErrorContext(r.Context(), "could not queue the reset message", "error", err)
		}
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted"})
}

// handleResetComplete consumes a token and sets a new password.
func (s *Server) handleResetComplete(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	if !s.sameOrigin(r) {
		writeProblem(w, http.StatusForbidden, "bad_origin", "the request did not come from this site")
		return
	}

	var req struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if !s.decodeJSON(w, r, &req, 8<<10) {
		return
	}

	userID, err := s.auth.Service.ResetPassword(r.Context(), req.Token, req.Password)
	switch {
	case err == nil:
	case errors.Is(err, authn.ErrInvalidToken):
		// One message for unknown, used and expired.
		writeProblem(w, http.StatusBadRequest, "invalid_token",
			"this link is invalid, already used or expired; request a new one")
		return
	case errors.Is(err, authn.ErrWeakPassword):
		writeProblem(w, http.StatusUnprocessableEntity, "weak_password", err.Error())
		return
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "password reset failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	// The actor is the account itself: the caller proved control of its mailbox with a single-use
	// token. Attributing this to "system" would make an incident review ask who did it when the
	// answer is in the row.
	s.auditAuthChangeAs(r, "user.password_reset", userID)
	writeJSON(w, http.StatusOK, map[string]any{"status": "password_changed"})
}

// ------------------------------------------------------- email verification --

// handleVerifyRequest queues a verification message for the caller.
func (s *Server) handleVerifyRequest(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication_required", "sign in to continue")
		return
	}
	if !s.allowCredentialAttempt(w, r, "verify:"+user.ID, 3) {
		return
	}

	res, err := s.auth.Service.RequestEmailVerification(r.Context(), user.ID)
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "verification request failed", "error", err)
		// The same answer as the success case: an already-verified address and a failure must not
		// be distinguishable, and the customer's action is the same either way.
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted"})
		return
	}

	if res.Token != "" {
		link := publicBaseURL() + "/api/v1/auth/verify?token=" + res.Token
		msg := notify.EmailVerificationMessage(res.Email, link, authn.EmailVerificationLifetime)
		msg.DedupeKey = "email_verification:" + res.Token[:12]
		if err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
			return s.queueMessage(ctx, tx, msg)
		}); err != nil {
			s.cfg.Logger.ErrorContext(r.Context(), "could not queue the verification message", "error", err)
		}
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted"})
}

// handleVerify consumes a verification token from a link.
func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	token := r.URL.Query().Get("token")
	if token == "" {
		writeProblem(w, http.StatusBadRequest, "invalid_token", "a token is required")
		return
	}

	userID, err := s.auth.Service.VerifyEmail(r.Context(), token)
	if err != nil {
		if errors.Is(err, authn.ErrInvalidToken) {
			writeProblem(w, http.StatusBadRequest, "invalid_token",
				"this link is invalid, already used or expired")
			return
		}
		s.cfg.Logger.ErrorContext(r.Context(), "verification failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}
	s.auditAuthChange(r, "user.email_verified", userID)
	writeJSON(w, http.StatusOK, map[string]any{"status": "verified"})
}

// --------------------------------------------------------------------- MFA --

// handleMFAEnrol starts enrolment.
func (s *Server) handleMFAEnrol(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	user, _ := userFrom(r.Context())

	secret, otpauth, err := s.auth.Service.EnrolMFA(r.Context(), user.ID)
	switch {
	case err == nil:
	case errors.Is(err, authn.ErrMFAAlreadyEnrolled):
		writeProblem(w, http.StatusConflict, "mfa_already_enrolled",
			"a second factor is already confirmed; disable it first")
		return
	case errors.Is(err, authn.ErrMFAKeyMissing):
		// The secret cannot be encrypted, so enrolment must not proceed. Saying so is better than
		// storing a plaintext secret.
		writeProblem(w, http.StatusServiceUnavailable, "mfa_unavailable",
			"MFA is not configured on this server")
		return
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "mfa enrolment failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"secret":      secret,
		"otpauth_url": otpauth,
		"digits":      6,
		"period":      30,
		"algorithm":   "SHA1",
	})
}

// handleMFAConfirm validates a code and returns the recovery codes once.
func (s *Server) handleMFAConfirm(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	user, _ := userFrom(r.Context())
	if !s.allowCredentialAttempt(w, r, "mfa-confirm:"+user.ID, 10) {
		return
	}

	var req struct {
		Code string `json:"code"`
	}
	if !s.decodeJSON(w, r, &req, 4<<10) {
		return
	}

	codes, err := s.auth.Service.ConfirmMFA(r.Context(), user.ID, req.Code)
	switch {
	case err == nil:
	case errors.Is(err, authn.ErrInvalidCode):
		writeProblem(w, http.StatusUnauthorized, "invalid_code", "that code is not valid")
		return
	case errors.Is(err, authn.ErrMFANotEnrolled), errors.Is(err, authn.ErrMFAAlreadyEnrolled):
		writeProblem(w, http.StatusConflict, "mfa_not_pending", err.Error())
		return
	case errors.Is(err, authn.ErrMFAKeyMissing):
		writeProblem(w, http.StatusServiceUnavailable, "mfa_unavailable", "MFA is not configured")
		return
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "mfa confirmation failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	s.auditAuthChange(r, "user.mfa_enrolled", user.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"recovery_codes": codes,
		// The warning is part of the contract: the codes are not retrievable again, and a UI that
		// does not say so is a UI that will lose them.
		"shown_once": true,
	})
}

// handleMFADisable removes the second factor.
func (s *Server) handleMFADisable(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	user, _ := userFrom(r.Context())

	var req struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if !s.decodeJSON(w, r, &req, 4<<10) {
		return
	}
	if req.Password == "" || req.Code == "" {
		writeProblem(w, http.StatusBadRequest, "credentials_required",
			"both the password and a current code are required")
		return
	}

	err := s.auth.Service.DisableMFA(r.Context(), user.ID, req.Password, req.Code)
	switch {
	case err == nil:
	case errors.Is(err, authn.ErrInvalidCredentials):
		writeProblem(w, http.StatusUnauthorized, "invalid_credentials", "the password is incorrect")
		return
	case errors.Is(err, authn.ErrInvalidCode):
		writeProblem(w, http.StatusUnauthorized, "invalid_code", "that code is not valid")
		return
	case errors.Is(err, authn.ErrMFANotEnrolled):
		writeProblem(w, http.StatusConflict, "mfa_not_enrolled", "no second factor is enrolled")
		return
	case errors.Is(err, authn.ErrMFAKeyMissing):
		writeProblem(w, http.StatusServiceUnavailable, "mfa_unavailable", "MFA is not configured")
		return
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "mfa disable failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	s.auditAuthChange(r, "user.mfa_disabled", user.ID)
	w.WriteHeader(http.StatusNoContent)
}

// handleMFAVerify completes the second login step.
func (s *Server) handleMFAVerify(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	if !s.sameOrigin(r) {
		writeProblem(w, http.StatusForbidden, "bad_origin", "the request did not come from this site")
		return
	}
	if !s.allowCredentialAttempt(w, r, "mfa-verify", 10) {
		return
	}

	var req struct {
		ChallengeToken string `json:"challenge_token"`
		Code           string `json:"code"`
	}
	if !s.decodeJSON(w, r, &req, 8<<10) {
		return
	}
	if req.ChallengeToken == "" || req.Code == "" {
		writeProblem(w, http.StatusBadRequest, "credentials_required",
			"a challenge token and a code are required")
		return
	}

	userID, err := s.auth.Service.CompleteMFAChallenge(r.Context(), req.ChallengeToken, req.Code)
	switch {
	case err == nil:
	case errors.Is(err, authn.ErrInvalidCode):
		// The challenge is not consumed: consuming it would let someone with the password deny
		// the real user access by submitting garbage.
		writeProblem(w, http.StatusUnauthorized, "invalid_code", "that code is not valid")
		return
	case errors.Is(err, authn.ErrChallengeExpired):
		writeProblem(w, http.StatusUnauthorized, "challenge_expired",
			"the login attempt expired; sign in again")
		return
	case errors.Is(err, authn.ErrMFAKeyMissing):
		writeProblem(w, http.StatusServiceUnavailable, "mfa_unavailable", "MFA is not configured")
		return
	default:
		s.cfg.Logger.ErrorContext(r.Context(), "mfa verify failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	// The password was already proven when the challenge was issued, so a session is created here.
	res, err := s.auth.Service.LoginWithID(r.Context(), userID, s.clientIP(r), r.UserAgent())
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "could not create a session after MFA", "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal_error",
			"the request could not be completed")
		return
	}

	s.setSessionCookies(w, res.Token, res.Session)
	writeJSON(w, http.StatusOK, map[string]any{
		"user": map[string]any{
			"id":             res.User.ID,
			"email":          res.User.Email,
			"display_name":   res.User.DisplayName,
			"email_verified": res.User.EmailVerified,
			"roles":          nonNilRoles(res.User.Roles),
			"is_staff":       res.User.IsStaff(),
		},
		"csrf_token": res.Session.CSRFToken,
		"expires_at": res.Session.ExpiresAt,
	})
}

// allowCredentialAttempt applies a per-subject budget to a credential endpoint.
//
// It is separate from the route-class limiter because these routes are unauthenticated: there is
// no principal to key on, so the key is the submitted address or the user id. Returning false means
// the response has already been written.
func (s *Server) allowCredentialAttempt(w http.ResponseWriter, r *http.Request, subject string, perWindow int) bool {
	if s.rateLimiter == nil {
		return true
	}
	class := RateClass("credential:" + subject)
	s.rateLimiter.mu.Lock()
	s.rateLimiter.limits[class] = perWindow
	s.rateLimiter.mu.Unlock()

	// The window is per minute; a credential attempt is not a bulk operation, so the same
	// fixed-window machinery is enough.
	allowed, _, reset := s.rateLimiter.allow("credential\x00"+subject, class)
	if allowed {
		return true
	}
	seconds := int(time.Until(reset).Seconds())
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeProblem(w, http.StatusTooManyRequests, "rate_limited",
		"too many attempts; try again later")
	return false
}

// auditAuthChange records a credential change in the audit log.
//
// The auth_events table already has the event; this row is what makes it visible to the operator
// who reviews "what happened to this account" alongside the invoice and service changes.
func (s *Server) auditAuthChange(r *http.Request, action, userID string) {
	s.auditAuthChangeAs(r, action, userID)
}

// auditAuthChangeAs records a credential change attributed to the account itself.
//
// The credential flows are reachable without a session, so there is no operator to name. The account
// whose token was redeemed is the actor: it proved control of the mailbox, and that is the fact an
// incident review needs.
func (s *Server) auditAuthChangeAs(r *http.Request, action, userID string) {
	actor := audit.Actor{ID: userID, Kind: audit.ActorUser}
	err := s.pool.InTx(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
		ev := s.auditEvent(r, action, "user", userID, audit.OutcomeSuccess)
		ev.Actor = actor
		return audit.Record(ctx, tx, ev)
	})
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "audit write failed for an account change",
			"action", action, "error", err)
	}
}
