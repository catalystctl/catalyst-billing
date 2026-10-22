package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/register"
)

// Customer self-registration and the signed-in password change (IDN-01, IDN-06,
// IDN-19, IDN-21).
//
// Registration is the single highest-leverage gap in the revenue path: without it an
// anonymous visitor cannot become a customer, so the storefront's Buy button can only
// be a login link. The handler is deliberately thin — the transaction, the password
// hashing and the settings are in internal/register, where a test can prove them
// without an HTTP round trip.
//
// # Why the response never tells you whether an address is registered
//
// A registration form is the easiest account-discovery oracle there is: submit an
// address, read the answer, learn whether that person is a customer. The password
// reset flow already refuses to be that oracle (always 202, fixed body, no token), and
// this endpoint follows the same rule for the same reason:
//
//   - the answer is 202 with a fixed body whether the account was created or the
//     address already existed;
//   - the internal/register package hashes the password before it attempts the insert,
//     so the duplicate path costs the same Argon2 work as the success path and the
//     *timing* does not leak either;
//   - a duplicate still queues a verification message, addressed to the account that
//     already exists, which is useful (it verifies an unverified address) and
//     indistinguishable from the fresh case.
//
// The consequence for the frontend is deliberate and documented: registration does not
// sign you in. It answers "accepted" and the client sends the visitor to sign in with
// the credentials they just chose (or to check their mail when verification is
// required). The alternative — returning a session for a new account and an accepted
// body for a duplicate — is a two-request oracle (`POST /auth/register` then
// `GET /auth/me`), so it is not offered.
//
// The `verification_required` flag in the body is policy, not a fact about the
// address: it is read from `mail.must_verify` and is the same for every caller, so it
// reveals nothing about any account.

func init() {
	RegisterRoutes("20-register", registerCustomerAccounts)
}

func registerCustomerAccounts(r *router) {
	// Public: this is how a session-less visitor gets an account. It enforces
	// same-origin itself and applies the credential budget, exactly like login and
	// the password-reset request. CSRF is not applied because there is no session to
	// bind a token to.
	r.publicPost("/api/v1/auth/register", r.s.handleRegister)

	// A signed-in user changing their own password. AccessUser (not a permission)
	// because the action is scoped to the caller by construction: the user id comes
	// from the session and there is no identifier in the path or body to substitute.
	r.userMutation(http.MethodPost, "/api/v1/auth/password/change", false, r.s.handlePasswordChange)
}

// registerService builds the registration service for this request.
//
// The hasher is constructed per call rather than stored on the Server because the
// Server's authn.Service does not expose its own: hashes are self-describing, so a
// registration hashed with the current parameters still verifies against a service
// configured with different ones, and the opportunistic re-hash on next login upgrades
// it. `register.Argon2Params` is a variable so a test can lower the cost.
func (s *Server) registerService() (*register.Service, error) {
	hasher, err := register.NewHasher()
	if err != nil {
		return nil, err
	}
	svc := register.New(s.pool, hasher)
	if s.auth != nil && s.auth.Service != nil {
		// The verification token and the outbox are attached only when the
		// installation has them: an account is still creatable without a mail path,
		// it simply has no message queued.
		svc.WithVerification(s.auth.Service, s.notify, publicBaseURL())
	}
	return svc, nil
}

// handleRegister creates a customer account.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable",
			"registration is not available")
		return
	}
	if !s.sameOrigin(r) {
		writeProblem(w, http.StatusForbidden, "bad_origin", "the request did not come from this site")
		return
	}
	// The credential budget, per source address. It is the same machinery login and
	// the reset request use: a bot hitting register is bounded by the surface, not by
	// the (nonexistent) session.
	if !s.allowCredentialAttempt(w, r, "register:"+s.clientIP(r), 5) {
		return
	}

	var req struct {
		Email       string `json:"email"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
		AcceptTOS   bool   `json:"accept_tos"`
		// CaptchaToken is the challenge's answer. It is empty when no provider is configured,
		// and requireCaptcha then makes no outbound call at all.
		CaptchaToken string `json:"captcha_token"`
	}
	if !s.decodeJSON(w, r, &req, 8<<10) {
		return
	}

	// The challenge is checked after the body is read (the token is in it) and before the account
	// is touched, so a bot never reaches the user table. With no provider configured this is a
	// no-op: no challenge, no outbound call, no behaviour change.
	if !s.requireCaptcha(w, r, req.CaptchaToken) {
		return
	}

	svc, err := s.registerService()
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "registration is misconfigured", "error", err)
		writeProblem(w, http.StatusServiceUnavailable, "registration_unavailable",
			"registration is not available")
		return
	}

	// A second budget, per submitted address. One address cannot be used to spray, and
	// one source cannot be used to flood a mailbox. It is applied after the body is
	// decoded because the address is the subject.
	if !s.allowCredentialAttempt(w, r, "register-email:"+normaliseEmail(req.Email), 3) {
		return
	}

	_, err = svc.Register(r.Context(), register.Request{
		Email:       req.Email,
		Password:    req.Password,
		DisplayName: req.DisplayName,
		AcceptedTOS: req.AcceptTOS,
		IP:          s.clientIP(r),
	})
	if err != nil {
		switch {
		case errors.Is(err, register.ErrRegistrationDisabled):
			// Not a secret: the setting is a public property of the installation, and a
			// visitor who is told "registration is closed" can act on it.
			writeProblem(w, http.StatusForbidden, "registration_disabled",
				"registration is closed; contact the operator for an account")
		case errors.Is(err, register.ErrInvalidEmail):
			writeProblem(w, http.StatusBadRequest, "invalid_email", "a valid email address is required")
		case errors.Is(err, register.ErrInvalidDisplayName):
			writeProblem(w, http.StatusBadRequest, "invalid_display_name",
				"a display name is required (at most 200 characters)")
		case errors.Is(err, register.ErrTOSRequired):
			writeProblem(w, http.StatusUnprocessableEntity, "tos_required",
				"the terms of service must be accepted")
		case errors.Is(err, authn.ErrWeakPassword):
			// The caller's mistake, and a distinct 422 for the same reason the reset
			// flow uses one: "your password is too short" is actionable.
			writeProblem(w, http.StatusUnprocessableEntity, "weak_password", err.Error())
		case errors.Is(err, register.ErrEmailTaken):
			// The duplicate path never reaches here for the caller: Register reports it
			// as a successful-looking result. This branch exists so a future change
			// that surfaces the error cannot accidentally turn it into a 409.
			writeAccepted(w, false)
		default:
			s.cfg.Logger.ErrorContext(r.Context(), "registration failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, "internal_error",
				"the request could not be completed")
		}
		return
	}

	// A duplicate is answered exactly like a creation. res.Duplicate is deliberately
	// not consulted for the body: that is the whole point.
	// The body carries policy, never a fact about the address: `verification_required`
	// is the same for every caller, so it reveals nothing about any account. The
	// service's Result is deliberately not consulted — its Duplicate field is exactly
	// what must not reach the wire.
	policy, perr := svc.Policy(r.Context())
	if perr != nil {
		// The account is already committed; a settings read failure must not turn a
		// successful registration into a 500 the customer would retry.
		s.cfg.Logger.ErrorContext(r.Context(), "could not read registration policy", "error", perr)
		writeAccepted(w, false)
		return
	}
	writeAccepted(w, policy.MustVerifyEmail)
}

// writeAccepted is the one response shape registration ever returns.
func writeAccepted(w http.ResponseWriter, verificationRequired bool) {
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":                "accepted",
		"verification_required": verificationRequired,
	})
}

// handlePasswordChange changes the signed-in user's password.
func (s *Server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeProblem(w, http.StatusServiceUnavailable, "storage_unavailable",
			"this action is not available")
		return
	}
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication_required", "sign in to continue")
		return
	}
	sess, _ := sessionFrom(r.Context())

	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !s.decodeJSON(w, r, &req, 8<<10) {
		return
	}
	if req.CurrentPassword == "" || req.NewPassword == "" {
		writeProblem(w, http.StatusBadRequest, "credentials_required",
			"both the current and the new password are required")
		return
	}

	svc, err := s.registerService()
	if err != nil {
		s.cfg.Logger.ErrorContext(r.Context(), "password change is misconfigured", "error", err)
		writeProblem(w, http.StatusServiceUnavailable, "registration_unavailable",
			"this action is not available")
		return
	}

	revoked, err := svc.ChangePassword(r.Context(), user.ID, sess.ID,
		req.CurrentPassword, req.NewPassword)
	if err != nil {
		switch {
		case errors.Is(err, register.ErrInvalidCredentials):
			// Reached only after the caller presented a wrong current password, so
			// saying so is safe: it cannot be used to discover which accounts exist,
			// because the request already required a session.
			writeProblem(w, http.StatusUnauthorized, "invalid_credentials",
				"the current password is incorrect")
		case errors.Is(err, authn.ErrWeakPassword):
			writeProblem(w, http.StatusUnprocessableEntity, "weak_password", err.Error())
		case errors.Is(err, register.ErrNoSuchUser):
			writeProblem(w, http.StatusUnauthorized, "authentication_required", "sign in to continue")
		default:
			s.cfg.Logger.ErrorContext(r.Context(), "password change failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, "internal_error",
				"the request could not be completed")
		}
		return
	}

	// The audit row is written by the account itself, exactly as the reset flow does:
	// the caller proved the current password, and attributing the change to "system"
	// would make an incident review ask a question the row can answer.
	s.auditAuthChange(r, "user.password_changed", user.ID)

	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "password_changed",
		"sessions_revoked": revoked,
	})
}

// normaliseEmail lower-cases and trims an address for use as a rate-limit subject.
//
// It is not validation: a malformed address still gets a budget, so a caller cannot
// escape the limit by sending junk. It only makes "A@x.test" and "a@x.test" the same
// bucket, which is the same equivalence the unique index enforces.
func normaliseEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
