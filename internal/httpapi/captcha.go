package httpapi

import (
	"errors"
	"net/http"

	"github.com/catalyst/billing/internal/captcha"
	"github.com/catalyst/billing/internal/store"
)

// The human-challenge gate (IDN-18).
//
// Registration and the password-reset request are the two anonymous endpoints that spend
// real money: one creates an account and sends a verification mail, the other sends a reset
// mail to whatever address is typed. Both are already bounded per source address, and that
// budget stays — a challenge is a *second* control, not a replacement for the first. A
// captcha that replaced the rate limit would make one solved challenge worth unlimited
// registrations, which is exactly what a solver farm sells.
//
// # Three codes, three meanings
//
//	captcha_required     400  a provider is configured and the request carried no token
//	captcha_failed       403  the provider said the token is wrong, expired or reused
//	captcha_unavailable  503  the provider could not be asked (outage, timeout, misconfig)
//
// The split is the point: an operator watching codes can tell a solver farm (a burst of
// captcha_failed) from a provider outage (a burst of captcha_unavailable), and the client
// knows whether to re-run the widget or ask the visitor to come back later.
//
// # Non-enumeration
//
// None of the three depends on the submitted address: the gate runs before any lookup, so
// an existing and an unknown address get byte-identical answers on both the registration
// and the reset path. The reset flow's "always 202" promise is kept for every request that
// passes the challenge.
//
// # When nothing is configured
//
// With `captcha.provider = none` — the default — requireCaptcha makes no outbound call and
// returns true. Registration and reset behave exactly as they did before this file existed;
// enabling a challenge is a configuration change, never a deploy.

// Codes returned by the gate.
const (
	codeCaptchaRequired    = "captcha_required"
	codeCaptchaFailed      = "captcha_failed"
	codeCaptchaUnavailable = "captcha_unavailable"
)

func init() {
	RegisterRoutes("25-captcha", registerCaptcha)
}

func registerCaptcha(r *router) {
	// Public: a sign-in, registration or reset screen needs to know which widget to render
	// before it has a session, and the answer is public by construction (a site key is in
	// the page). It never contains the secret key.
	r.publicGet("/api/v1/auth/captcha", r.s.handleCaptchaConfig)
}

// newCaptchaService builds the gate for one request.
//
// It is a variable so a test can substitute a service pointed at a fake siteverify server
// instead of Cloudflare, Google or hCaptcha. Production never reassigns it, and nothing else
// in this package reads it.
var newCaptchaService = func(pool *store.Pool) *captcha.Service {
	return captcha.NewService(pool)
}

// captchaService returns the gate for this server, or nil when there is no database to read
// the configuration from.
func (s *Server) captchaService() *captcha.Service {
	if s.pool == nil {
		return nil
	}
	return newCaptchaService(s.pool)
}

// requireCaptcha enforces the challenge for one request.
//
// It returns true when the request may proceed — including when no provider is configured,
// which is the default and must change nothing. On refusal it has already written the
// problem document and the caller must return.
//
// The caller passes the token it decoded from the request body, because the body can only be
// read once and the handler that owns the body is the only place that knows the field name.
// Wiring is one line, immediately after the body is decoded and before anything is looked
// up:
//
//	if !s.requireCaptcha(w, r, req.CaptchaToken) {
//		return
//	}
func (s *Server) requireCaptcha(w http.ResponseWriter, r *http.Request, token string) bool {
	svc := s.captchaService()
	if svc == nil {
		// No database means no settings, and the endpoints that call this already refuse
		// without storage. Nothing is configured, so nothing is enforced.
		return true
	}

	err := svc.Check(r.Context(), token, s.clientIP(r))
	switch {
	case err == nil:
		return true
	case errors.Is(err, captcha.ErrMissingToken):
		writeProblem(w, http.StatusBadRequest, codeCaptchaRequired,
			"complete the challenge before continuing")
	case errors.Is(err, captcha.ErrChallengeFailed):
		// The provider's own reason codes are in the error. They are the provider's
		// vocabulary, not the visitor's, and they never reach the response.
		s.cfg.Logger.WarnContext(r.Context(), "captcha challenge refused", "error", err)
		writeProblem(w, http.StatusForbidden, codeCaptchaFailed,
			"the challenge was not solved; try again")
	default:
		// ErrUnavailable, ErrNotConfigured, ErrUnknownProvider and anything else that is
		// not a verdict: the challenge could not be checked, so the request is refused
		// rather than let through unchecked. Failing open here would turn a provider
		// outage into an open registration form.
		s.cfg.Logger.ErrorContext(r.Context(), "captcha could not be verified", "error", err)
		writeProblem(w, http.StatusServiceUnavailable, codeCaptchaUnavailable,
			"the challenge could not be checked; try again in a moment")
	}
	return false
}

// handleCaptchaConfig serves the public half of the challenge configuration.
//
// It answers `{"provider": "none", "site_key": "", "enabled": false}` when nothing is
// configured, so a page can render "no widget" without a second request or a guess. The
// secret key is not in the shape at all: a response that echoed it would put the credential
// in every proxy log and browser cache between here and the operator.
func (s *Server) handleCaptchaConfig(w http.ResponseWriter, r *http.Request) {
	svc := s.captchaService()
	if svc == nil {
		writeJSON(w, http.StatusOK, captcha.PublicConfig{Provider: captcha.ProviderNone})
		return
	}

	public, err := svc.Public(r.Context())
	if err != nil {
		// A settings read failure is reported, not guessed at. A page that cannot read
		// this renders no widget and sends no token, and the gate refuses at the endpoint
		// — which is the safe direction for a client-side failure.
		s.cfg.Logger.ErrorContext(r.Context(), "could not read captcha configuration", "error", err)
		writeProblem(w, http.StatusServiceUnavailable, codeCaptchaUnavailable,
			"the challenge configuration could not be read")
		return
	}

	// Not cached: an operator who switches provider or rotates a site key expects the next
	// page load to use it, and this response is a few hundred bytes.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, public)
}
