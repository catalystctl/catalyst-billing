package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/authn"
)

// Session cookie names.
const (
	sessionCookieName = "billing_session"
	csrfCookieName    = "billing_csrf"
	csrfHeaderName    = "X-CSRF-Token"
)

// CookieConfig controls cookie attributes.
type CookieConfig struct {
	// Secure must be true whenever the panel is served over HTTPS. It defaults to
	// true and is only disabled for a loopback development server, because a
	// session cookie sent over plain HTTP is readable by anyone on the path.
	Secure bool
	// Domain is empty for a host-only cookie, which is the safe default.
	Domain string
	// Path scopes the cookie to the panel.
	Path string
}

func (c CookieConfig) withDefaults() CookieConfig {
	if c.Path == "" {
		c.Path = "/"
	}
	return c
}

// contextKey is a private type so no other package can collide with these keys.
type contextKey int

const (
	ctxSession contextKey = iota
	ctxUser
	// ctxBindingMismatch marks a session that authenticated but was used from outside
	// what it was bound to (IDN-16). The identity is deliberately not attached in that
	// case: every guard in this server answers for the caller, and a caller whose session
	// is being refused must not be able to reach a handler through a guard that forgot to
	// ask.
	ctxBindingMismatch
)

// sessionFrom returns the authenticated session, if any.
func sessionFrom(ctx context.Context) (authn.Session, bool) {
	s, ok := ctx.Value(ctxSession).(authn.Session)
	return s, ok
}

// userFrom returns the authenticated user, if any.
func userFrom(ctx context.Context) (authn.User, bool) {
	u, ok := ctx.Value(ctxUser).(authn.User)
	return u, ok
}

// bindingMismatchFrom returns the refusal detail when the request carried a session that was
// used from outside its binding, if any.
func bindingMismatchFrom(ctx context.Context) (error, bool) {
	err, ok := ctx.Value(ctxBindingMismatch).(error)
	return err, ok
}

// refuseSessionBinding answers a request whose session was used from outside its binding.
//
// It writes the refusal itself and reports whether it did, so every guard calls it in one
// line and the problem code is written once. The code is its own — not
// authentication_required and not forbidden — because the session *is* valid and the caller
// needs to know that signing in again, rather than retrying, is the fix.
func (s *Server) refuseSessionBinding(w http.ResponseWriter, r *http.Request) bool {
	err, mismatch := bindingMismatchFrom(r.Context())
	if !mismatch {
		return false
	}
	var detail *authn.BindingMismatch
	what := "this session was signed in from a different address or browser"
	if errors.As(err, &detail) {
		switch detail.Field {
		case "ip":
			what = "this session was signed in from a different network address"
		case "user_agent":
			what = "this session was signed in from a different browser"
		}
	}
	writeProblem(w, http.StatusForbidden, "session_binding_mismatch",
		what+"; sign in again to continue from here")
	return true
}

// Auth bundles the authentication dependencies the HTTP layer needs.
type Auth struct {
	Service *authn.Service
	Authz   *authn.Authorizer
	Cookies CookieConfig
	// TrustedProxies lists the networks whose forwarding headers are believed.
	//
	// This is a security boundary, not a convenience. When it is empty, no
	// forwarding header is honoured at all and the client address is taken from the
	// connection, which is correct for a directly exposed panel. When the panel sits
	// behind a proxy, the proxy's networks must be listed here explicitly; otherwise
	// any client could choose its own throttling bucket by sending X-Forwarded-For,
	// lock out another address, poison the audit log, and defeat the Origin check by
	// choosing X-Forwarded-Host.
	TrustedProxies []*net.IPNet
	// SessionTTL is the cookie lifetime, kept in step with the session's own
	// expiry so the browser and the server agree about when to stop sending it.
	SessionTTL time.Duration
}

// loadSession resolves the session cookie and attaches the identity to the request
// context.
//
// It never rejects the request: public routes (health, readiness, webhooks) must
// still work, and a route that needs an identity is guarded by requireUser or
// requireStaff. That separation is deliberate — a single middleware that rejects
// everything is how a webhook endpoint accidentally starts demanding a cookie.
func (s *Server) loadSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auth == nil || s.auth.Service == nil {
			next.ServeHTTP(w, r)
			return
		}

		// A bearer key first: an API client that sends both is identified by the key it
		// configured, and a cookie it did not intend must not override it.
		if presented := bearerToken(r); presented != "" {
			if key, user, ok := s.apiKeyIdentity(r, presented); ok {
				ctx := context.WithValue(r.Context(), ctxUser, user)
				ctx = withAPIKey(ctx, key)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			// An OAuth2 access token is the other bearer credential this server issues. It
			// resolves to the same identity shape an API key does — the owner plus scopes that
			// only narrow — so it rides the same guards instead of getting a second
			// authorization path of its own (see internal/httpapi/oauth.go).
			if ident, user, token, ok := s.oauthIdentity(r, presented); ok {
				ctx := context.WithValue(r.Context(), ctxUser, user)
				ctx = withAPIKey(ctx, ident)
				ctx = withOAuthToken(ctx, token)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			// An invalid key is a hard 401, not an anonymous pass: a client that presented a
			// credential must be told the credential is wrong, not treated as a browser with a
			// stale cookie.
			writeProblem(w, http.StatusUnauthorized, "invalid_api_key",
				"the API key is invalid, revoked or expired")
			return
		}

		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || cookie.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		sess, user, err := s.auth.Service.Authenticate(r.Context(), cookie.Value)
		if err != nil {
			// An invalid session is not an error for the caller: clear the cookie so
			// the browser stops sending a dead token, then continue as anonymous.
			s.clearSessionCookies(w)
			next.ServeHTTP(w, r)
			return
		}
		// The session authenticated; now ask whether it is being used from where it was
		// signed in (IDN-16). A mismatch does not clear the cookie and does not continue as
		// anonymous: clearing would sign the user out on a mobile network change, and
		// continuing as anonymous would turn the refusal into a silent 401. The marker is
		// carried instead, and the guards below refuse with a code that names the reason.
		// Public pages keep rendering, because they never ask for an identity.
		if berr := s.auth.Service.EnforceSessionBinding(r.Context(), sess, s.clientIP(r), r.UserAgent()); berr != nil {
			ctx := context.WithValue(r.Context(), ctxBindingMismatch, berr)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		// An impersonated session is a real session for the customer whose identity is worn, so
		// every other guard in this server answers for them — which is exactly why the actions
		// that would turn looking into acting have to be refused here, before any of them run.
		// See impersonationRefused for what is refused and why it is refused in one place.
		if sess.ImpersonatedBy != "" {
			if rule, refused := impersonationRefusal(r.Method, r.URL.Path); refused {
				writeProblem(w, http.StatusForbidden, "impersonating", rule.refusal())
				return
			}
		}
		ctx := context.WithValue(r.Context(), ctxSession, sess)
		ctx = context.WithValue(ctx, ctxUser, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ------------------------------------------------------- impersonation limits --

// impersonationRule is one request an impersonated session may not make.
type impersonationRule struct {
	Method string
	// Pattern is the router's own pattern, `{id}` wildcards included, so this table and the
	// route table are written in the same language and a test can compare them.
	Pattern string
	// Noun names the family of action in the refusal: "money", "credentials", "account".
	Noun string
}

// refusal is the sentence the caller reads. It names the family rather than the endpoint so the
// refusal explains itself without describing the permission model to whoever is holding the
// borrowed session.
func (r impersonationRule) refusal() string {
	return "an impersonated session cannot change " + r.Noun +
		"; stop impersonating and act as yourself"
}

// impersonationRefused is the list of actions an impersonated session cannot take.
//
// Impersonation exists so an operator can *see* what a customer sees. Every route below turns
// looking into acting as somebody else, and the families are refused for different reasons:
//
//   - money: paying an invoice, spending credit, checking out an order, refunding a payment.
//     A transaction the customer never authorised is attributed to the customer, and the ledger
//     is then wrong in a way no audit row can undo.
//   - credentials: changing a password, enrolling or removing a second factor, unlinking a
//     sign-in identity, minting an API key, granting a role. Each one hands the operator a way
//     back into the account after the impersonation ends. That is a takeover, not support.
//   - account: closing the record. It ends the login and its sessions, and there is no undo.
//
// The refusal lives in the middleware every request passes through (loadSession), not in the
// handlers. A handler-by-handler guard is a list that grows a hole the first time somebody adds
// a payment endpoint without knowing this file exists, and its completeness cannot be proved.
// Here the rule is one table, and impersonation_test.go checks every entry against the live route
// table — so an entry cannot go stale, and the routes that must stay reachable (reading the
// customer, /auth/me, stopping the impersonation) are proven reachable by the same test.
//
// Staff routes are listed even though an impersonated session cannot hold a staff permission
// today. The rule is a property of the session, not of the current permission model: the day
// somebody allows impersonating a colleague, the money and credential actions are already shut.
var impersonationRefused = []impersonationRule{
	// -- money --
	{http.MethodPost, "/api/v1/portal/invoices/{id}/checkout", "money"},
	{http.MethodPost, "/api/v1/portal/invoices/{id}/pay-with-credit", "money"},
	{http.MethodPost, "/api/v1/portal/credits/deposit", "money"},
	{http.MethodPost, "/api/v1/portal/orders", "money"},
	{http.MethodPost, "/api/v1/staff/payments", "money"},
	{http.MethodPost, "/api/v1/staff/payments/{id}/refunds", "money"},
	{http.MethodPost, "/api/v1/staff/invoices/{id}/payments", "money"},
	{http.MethodPost, "/api/v1/staff/invoices/{id}/credit-notes", "money"},
	{http.MethodPost, "/api/v1/staff/invoices/{id}/void", "money"},
	{http.MethodPost, "/api/v1/staff/customers/{id}/credit", "money"},
	{http.MethodPost, "/api/v1/staff/customers/{id}/credit/apply", "money"},
	{http.MethodPost, "/api/v1/staff/customers/{id}/credit/debit", "money"},
	{http.MethodPost, "/api/v1/staff/affiliates/{id}/payout", "money"},
	{http.MethodPost, "/api/v1/staff/affiliates/{id}/payouts", "money"},

	// -- credentials --
	{http.MethodPost, "/api/v1/auth/password/change", "credentials"},
	{http.MethodGet, "/api/v1/auth/mfa", "credentials"},
	{http.MethodPost, "/api/v1/auth/mfa/enrol", "credentials"},
	{http.MethodPost, "/api/v1/auth/mfa/confirm", "credentials"},
	{http.MethodPost, "/api/v1/auth/mfa/disable", "credentials"},
	{http.MethodDelete, "/api/v1/portal/social-links/{id}", "credentials"},
	{http.MethodPost, "/api/v1/staff/users/{id}/password", "credentials"},
	{http.MethodPost, "/api/v1/staff/users/{id}/mfa/reset", "credentials"},
	{http.MethodPost, "/api/v1/staff/api-keys", "credentials"},
	{http.MethodDelete, "/api/v1/staff/api-keys/{id}", "credentials"},
	{http.MethodPut, "/api/v1/staff/api-keys/{id}/ip-allowlist", "credentials"},
	{http.MethodPost, "/api/v1/staff/roles", "credentials"},
	{http.MethodPut, "/api/v1/staff/roles/{id}/permissions", "credentials"},

	// -- account --
	{http.MethodPost, "/api/v1/portal/account-closure", "account"},
}

// impersonationRefusal reports which rule refuses this request, if any.
//
// The path is cleaned first. `path.Clean` collapses `//`, `.` and a trailing slash, so
// `/api/v1/portal/credits/deposit/` is judged by the same rule as the canonical spelling instead
// of sliding past it into whatever the mux decides to do with the variant.
func impersonationRefusal(method, rawPath string) (impersonationRule, bool) {
	segments := splitSegments(path.Clean("/" + rawPath))
	for _, rule := range impersonationRefused {
		if rule.Method != method {
			continue
		}
		if matchSegments(splitSegments(rule.Pattern), segments) {
			return rule, true
		}
	}
	return impersonationRule{}, false
}

// ImpersonationRefusedRoutes lists the `METHOD /pattern` pairs an impersonated session may not
// call.
//
// It is exported for the same reason Routes() is: a property of the whole surface should be
// assertable from outside the package. impersonation_test.go uses it to prove every entry names a
// route that actually exists, so the table cannot rot into a list of paths that stopped being
// routed — a refusal that protects nothing while looking thorough.
func ImpersonationRefusedRoutes() []string {
	out := make([]string, 0, len(impersonationRefused))
	for _, rule := range impersonationRefused {
		out = append(out, rule.Method+" "+rule.Pattern)
	}
	return out
}

func splitSegments(p string) []string {
	trimmed := strings.Trim(p, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

// matchSegments compares a route pattern to a request path, treating `{...}` as one wildcard
// segment. Segment counts must agree: a wildcard never swallows a `/`.
func matchSegments(pattern, segments []string) bool {
	if len(pattern) != len(segments) {
		return false
	}
	for i, want := range pattern {
		if strings.HasPrefix(want, "{") && strings.HasSuffix(want, "}") {
			if segments[i] == "" {
				return false
			}
			continue
		}
		if want != segments[i] {
			return false
		}
	}
	return true
}

// bearerToken returns the Authorization bearer value, if any.
func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// apiKeyIdentity resolves a bearer key into a user plus its scopes.
//
// The user is loaded and their roles resolved exactly as a session login would: the key never
// carries permissions of its own. The scopes only narrow.
func (s *Server) apiKeyIdentity(r *http.Request, presented string) (APIKeyIdentity, authn.User, bool) {
	key, err := s.auth.Service.AuthenticateAPIKey(r.Context(), presented)
	if err != nil {
		return APIKeyIdentity{}, authn.User{}, false
	}
	// A key may be restricted to a set of networks. The check fails closed: a key whose allowlist
	// cannot be read is refused, because a key that is momentarily unrestricted is a key that can
	// be used from anywhere, and the moment it takes to notice is the whole attack.
	if !s.apiKeyAllowedFromRequest(r, key.ID) {
		return APIKeyIdentity{}, authn.User{}, false
	}
	user, err := s.auth.Service.UserByID(r.Context(), key.UserID)
	if err != nil || user.Status != "active" {
		// A key outliving its owner's disabled account would be a permanent backdoor.
		return APIKeyIdentity{}, authn.User{}, false
	}
	return APIKeyIdentity{KeyID: key.ID, UserID: key.UserID, Scopes: key.Scopes}, user, true
}

// requireUser rejects an anonymous request.
func (s *Server) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.refuseSessionBinding(w, r) {
			return
		}
		if _, ok := userFrom(r.Context()); !ok {
			writeProblem(w, http.StatusUnauthorized, "authentication_required", "sign in to continue")
			return
		}
		next(w, r)
	}
}

// requireStaff rejects a request from anyone without a staff role.
//
// Being staff is not the same as holding a permission: the permission check is
// separate, so a support agent cannot reach a billing action just because they are
// staff.
func (s *Server) requireStaff(next http.HandlerFunc) http.HandlerFunc {
	return s.requireUser(func(w http.ResponseWriter, r *http.Request) {
		user, _ := userFrom(r.Context())
		if !user.IsStaff() {
			writeProblem(w, http.StatusForbidden, "staff_required", "this area is for staff")
			return
		}
		next(w, r)
	})
}

// requirePermission rejects a request whose user lacks the permission.
func (s *Server) requirePermission(p authn.Permission, next http.HandlerFunc) http.HandlerFunc {
	return s.requireUser(func(w http.ResponseWriter, r *http.Request) {
		user, _ := userFrom(r.Context())
		if s.auth == nil || s.auth.Authz == nil {
			writeProblem(w, http.StatusForbidden, "forbidden", "authorization is not configured")
			return
		}
		if err := s.auth.Authz.Require(user, p); err != nil {
			// The response says nothing about which permission was missing, so it
			// cannot be used to map the permission model.
			writeProblem(w, http.StatusForbidden, "forbidden", "you do not have access to this action")
			return
		}
		// A key can only narrow its owner's permissions, never widen them. The owner's roles
		// were checked above; this is the second half, and doing it here rather than in each
		// handler is what makes it true for every route at once.
		if key, ok := apiKeyFrom(r.Context()); ok && !key.Allows(string(p)) {
			writeProblem(w, http.StatusForbidden, "forbidden", "you do not have access to this action")
			return
		}
		next(w, r)
	})
}

// requireCSRF rejects a state-changing request whose CSRF token does not match the
// session.
//
// The token is checked from the header only. Accepting it from the body would let a
// cross-site form post supply it, which defeats the point; a form-based client must
// send it as a header from script.
func (s *Server) requireCSRF(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Before the token check, because a refused session attaches no session and would
		// otherwise be reported as a missing CSRF token — a true statement that names the
		// wrong problem.
		if s.refuseSessionBinding(w, r) {
			return
		}
		// A request authenticated by an API key is exempt. CSRF exists because a browser attaches
		// cookies automatically; a bearer header is not attached by any browser, so there is
		// nothing for a cross-site form to forge.
		if _, ok := apiKeyFrom(r.Context()); ok {
			next(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next(w, r)
			return
		}
		sess, ok := sessionFrom(r.Context())
		if !ok {
			// No session means no session-bound secret. A mutation that requires a
			// session is already rejected by requireUser; reaching here without one
			// means the route is misconfigured, so fail closed.
			writeProblem(w, http.StatusForbidden, "csrf_failed", "a CSRF token is required")
			return
		}
		if err := authn.VerifyCSRF(sess, r.Header.Get(csrfHeaderName)); err != nil {
			writeProblem(w, http.StatusForbidden, "csrf_failed", "the CSRF token is missing or invalid")
			return
		}
		next(w, r)
	}
}

// setSessionCookies writes the session and CSRF cookies.
//
// The session cookie is HttpOnly so script cannot read it. The CSRF cookie is
// deliberately *not* HttpOnly: the browser client reads it to echo in the header,
// which is the double-submit pattern, and it is useless without the session cookie
// because the token is validated against the session's own secret.
func (s *Server) setSessionCookies(w http.ResponseWriter, token string, sess authn.Session) {
	cfg := s.auth.Cookies.withDefaults()
	ttl := s.auth.SessionTTL
	if ttl <= 0 {
		ttl = time.Until(sess.ExpiresAt)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     cfg.Path,
		Domain:   cfg.Domain,
		Expires:  sess.ExpiresAt,
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   cfg.Secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    sess.CSRFToken,
		Path:     cfg.Path,
		Domain:   cfg.Domain,
		Expires:  sess.ExpiresAt,
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: false,
		Secure:   cfg.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearSessionCookies expires both cookies.
func (s *Server) clearSessionCookies(w http.ResponseWriter) {
	cfg := CookieConfig{Path: "/"}
	if s.auth != nil {
		cfg = s.auth.Cookies.withDefaults()
	}
	for _, name := range []string{sessionCookieName, csrfCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     cfg.Path,
			Domain:   cfg.Domain,
			MaxAge:   -1,
			HttpOnly: name == sessionCookieName,
			Secure:   cfg.Secure,
			SameSite: http.SameSiteLaxMode,
		})
	}
}

// trustedProxy reports whether the request arrived from a configured proxy.
func (s *Server) trustedProxy(r *http.Request) bool {
	if s.auth == nil || len(s.auth.TrustedProxies) == 0 {
		return false
	}
	ip := remoteIP(r)
	if ip == nil {
		return false
	}
	for _, n := range s.auth.TrustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// remoteIP parses the address of the immediate peer.
func remoteIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(host)
}

// sameOrigin enforces that a state-changing request came from this origin.
//
// SameSite=Lax already blocks most cross-site posts, but a defence in depth check
// costs nothing and closes the cases SameSite does not cover (older clients,
// subdomain origins, and a misconfigured proxy that rewrites headers).
func (s *Server) sameOrigin(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Non-browser clients do not send Origin. They also cannot be used for a
		// CSRF attack, and they must still pass the CSRF token check.
		return true
	}

	host := r.Host
	// X-Forwarded-Host is only believed when the request came from a trusted proxy.
	// Honouring it unconditionally let a client choose the value it is compared
	// against, which defeats the whole check.
	if s.trustedProxy(r) {
		if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" {
			host = forwarded
		}
	}

	// Compare the host exactly, including the scheme, rather than by suffix: a
	// suffix test accepts "https://evil.example.attacker.test" for host
	// "attacker.test" and accepts "https://notpanel.example" for "panel.example".
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Host != host {
		return false
	}
	// An origin carrying a scheme we did not serve over is a different origin.
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return true
}

// loginRequest is the credential submission.
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// handleLogin authenticates and sets the session cookies.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	if !s.sameOrigin(r) {
		writeProblem(w, http.StatusForbidden, "bad_origin", "the request did not come from this site")
		return
	}

	var req loginRequest
	if !s.decodeJSON(w, r, &req, 4<<10) {
		return
	}

	res, err := s.auth.Service.Login(r.Context(), authn.LoginRequest{
		Email:     req.Email,
		Password:  req.Password,
		IP:        s.clientIP(r),
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		switch {
		case errors.Is(err, authn.ErrTooManyAttempts):
			w.Header().Set("Retry-After", "900")
			writeProblem(w, http.StatusTooManyRequests, "too_many_attempts",
				"too many attempts; try again later")
		case errors.Is(err, authn.ErrOverloaded):
			// The server is saturated, not the credentials. Saying so lets a client
			// back off instead of concluding the password is wrong.
			w.Header().Set("Retry-After", "5")
			writeProblem(w, http.StatusServiceUnavailable, "overloaded",
				"the server is busy; try again shortly")
		case errors.Is(err, authn.ErrAccountDisabled):
			// Reached only after a correct password, so it is safe to be specific.
			writeProblem(w, http.StatusForbidden, "account_disabled", "this account is disabled")
		default:
			// One message for a wrong password and an unknown account alike.
			writeProblem(w, http.StatusUnauthorized, "invalid_credentials",
				"email or password is incorrect")
		}
		return
	}

	// A confirmed second factor turns this into the first half of a login: the password is
	// right, but no session is issued until the code is presented. The challenge carries no
	// authority beyond that.
	if s.auth.Service != nil {
		if hasMFA, merr := s.auth.Service.HasMFA(r.Context(), res.User.ID); merr == nil && hasMFA {
			// The session the service just created is discarded: issuing it would defeat the
			// second factor entirely.
			if res.Session.ID != "" {
				if derr := s.auth.Service.Revoke(r.Context(), res.Token); derr != nil {
					s.cfg.Logger.ErrorContext(r.Context(), "could not discard the session for an MFA login",
						"error", derr)
				}
			}
			challenge, expires, cerr := s.auth.Service.StartMFAChallenge(r.Context(), res.User.ID)
			if cerr != nil {
				s.cfg.Logger.ErrorContext(r.Context(), "could not start an MFA challenge", "error", cerr)
				writeProblem(w, http.StatusServiceUnavailable, "mfa_unavailable",
					"the second factor could not be started; try again")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"mfa_required":    true,
				"challenge_token": challenge,
				"expires_at":      expires,
			})
			return
		}
	}

	s.setSessionCookies(w, res.Token, res.Session)
	// A sign-in that lands on an account owing a second factor says so. An operator's MFA reset
	// removes the factor and leaves `mfa_enrolment_required` behind; the session is issued (the
	// customer has to be able to reach the enrolment screen) but the response carries the demand,
	// and the shell sends them straight to it. Without this the reset would silently downgrade the
	// account to password-only and nobody would be asked to fix it.
	writeJSON(w, http.StatusOK, map[string]any{
		"user": map[string]any{
			"id":             res.User.ID,
			"email":          res.User.Email,
			"display_name":   res.User.DisplayName,
			"email_verified": res.User.EmailVerified,
			"roles":          res.User.Roles,
			"is_staff":       res.User.IsStaff(),
		},
		// `mfa_enrolment_required`, not `enrolment_required`: this is a flat identity document, and
		// the same name is returned by GET /auth/me. A field that means one thing carries one name
		// across the two documents a client reads to learn who it is signed in as. The bare
		// `enrolment_required` is used inside objects that are already scoped to a second factor —
		// the `logins[]` entries of the staff account-state payload and GET /portal/mfa — where the
		// prefix would only repeat the context.
		"mfa_enrolment_required": s.enrolmentRequired(r, res.User.ID),
		"csrf_token":             res.Session.CSRFToken,
		"expires_at":             res.Session.ExpiresAt,
	})
}

// handleLogout revokes the session and clears the cookies.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.auth != nil && s.auth.Service != nil {
		if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
			if err := s.auth.Service.Revoke(r.Context(), cookie.Value); err != nil {
				// Logout must still clear the cookie: leaving the browser signed in
				// because the server call failed is the worse outcome.
				s.cfg.Logger.WarnContext(r.Context(), "revoke session failed", "error", err)
			}
		}
	}
	s.clearSessionCookies(w)
	writeJSON(w, http.StatusOK, map[string]any{"status": "signed_out"})
}

// handleMe returns the current identity.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication_required", "not signed in")
		return
	}
	sess, _ := sessionFrom(r.Context())
	perms := []string{}
	if s.auth != nil && s.auth.Authz != nil {
		for p := range s.auth.Authz.Permissions(user) {
			perms = append(perms, string(p))
		}
	}
	// Both lists are arrays on the wire even when empty. A nil slice marshals to JSON null,
	// and a client that iterates the list — `roles.length`, `roles.join(", ")` — throws before
	// it renders anything. The client is fixed to tolerate null as well, but the contract is
	// "a list", and a contract that is sometimes null is one every caller has to special-case.
	// An impersonated session is reported as such on every identity read, not just once at the
	// start. The banner that warns an operator they are acting as a customer is rendered from
	// this flag, and a page reload, a second tab or a bookmarked URL all take this path — so the
	// warning cannot be missed by navigating.
	//
	// It is reported as both a flag and the operator's id: the flag is what the UI branches on,
	// and the id is what makes the audit trail explainable to the person reading it.
	writeJSON(w, http.StatusOK, map[string]any{
		"user": map[string]any{
			"id":             user.ID,
			"email":          user.Email,
			"display_name":   user.DisplayName,
			"email_verified": user.EmailVerified,
			"roles":          nonNilRoles(user.Roles),
			"is_staff":       user.IsStaff(),
		},
		"impersonating":   sess.ImpersonatedBy != "",
		"impersonated_by": sess.ImpersonatedBy,
		// The operator's address, because the banner has to name a person. "Impersonated by
		// 6f2a…" is a fact nobody can act on: the point of the warning is that the operator
		// recognises the colleague behind it. Best effort — a failed read leaves the field out
		// and the banner falls back to the id rather than failing the identity request.
		"impersonated_by_email": s.impersonatorEmail(r, sess.ImpersonatedBy),
		"permissions":           perms,
		// The same demand the sign-in response carries, so a page load, a second tab or a
		// bookmarked URL is told about it too. Cleared by a confirmed enrolment.
		"mfa_enrolment_required": s.enrolmentRequired(r, user.ID),
		// The rest of the caller's own second-factor state: whether a factor is confirmed, when,
		// how many recovery codes are left, and whether enrolment is even possible here. The shell
		// and the account screens read this to say "on"/"off" instead of guessing; the field names
		// match `GET /api/v1/portal/mfa`, and both come from `authn.MFAStatusFor`.
		"mfa":        s.mfaStateFor(r.Context(), user.ID),
		"csrf_token": sess.CSRFToken,
		"expires_at": sess.ExpiresAt,
	})
}

// impersonatorEmail resolves the operator behind an impersonated session, or "" when there is
// none or it cannot be read.
//
// Best effort on purpose: the banner names a person when the name is available and falls back to
// the id when it is not. A failed lookup must not fail the identity request — the customer's
// `/auth/me` is the request every screen makes, and a 500 there signs them out over a cosmetic
// detail.
func (s *Server) impersonatorEmail(r *http.Request, operatorID string) string {
	if operatorID == "" || s.auth == nil || s.auth.Service == nil {
		return ""
	}
	operator, err := s.auth.Service.UserByID(r.Context(), operatorID)
	if err != nil {
		return ""
	}
	return operator.Email
}

// enrolmentRequired reports whether the account owes the panel a second factor.
//
// It is a best-effort read: a failure here must not fail a sign-in or an identity request, because
// the answer is a prompt rather than an authority — the enrolment screen re-reads it, and a wrong
// `false` costs a customer one extra click while a 500 costs them their session.
func (s *Server) enrolmentRequired(r *http.Request, userID string) bool {
	if s.auth == nil || s.auth.Service == nil || userID == "" {
		return false
	}
	status, err := s.auth.Service.MFAStatusFor(r.Context(), userID)
	if err != nil {
		return false
	}
	return status.EnrolmentRequired
}

// nonNilRoles makes "no staff role" encode as [] rather than null.
//
// One helper, used by every response that carries a role list, so the two endpoints that
// report an identity cannot drift into disagreeing about the shape of an empty list.
func nonNilRoles(roles []string) []string {
	if roles == nil {
		return []string{}
	}
	return roles
}

// clientIP extracts the caller's address for throttling and audit.
//
// The returned value is always a valid IP or the empty string. A header value that
// is not an address is ignored rather than stored: an attacker-chosen string would
// otherwise poison the audit log, and a value that fails the inet cast would make a
// *correct* login fail.
//
// X-Forwarded-For is honoured only when the immediate peer is a trusted proxy, and
// then only the right-most entry that is not itself a trusted proxy. Taking the
// left-most entry (the common shortcut) is wrong: with a proxy that appends rather
// than replaces, the left-most value is whatever the client sent.
func (s *Server) clientIP(r *http.Request) string {
	peer := remoteIP(r)
	if peer == nil {
		return ""
	}
	if !s.trustedProxy(r) {
		return peer.String()
	}

	// Walk right to left, skipping trusted proxies, and take the first untrusted
	// address: that is the client as the outermost proxy saw it.
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := net.ParseIP(strings.TrimSpace(parts[i]))
		if candidate == nil {
			// A malformed entry invalidates the chain: fall back to the peer rather
			// than trusting anything to its left.
			return peer.String()
		}
		trusted := false
		for _, n := range s.auth.TrustedProxies {
			if n.Contains(candidate) {
				trusted = true
				break
			}
		}
		if !trusted {
			return candidate.String()
		}
	}
	return peer.String()
}

// decodeJSON reads a bounded JSON body.
//
// It returns false after writing a response, so handlers read as a sequence of
// guards. DisallowUnknownFields is on: a client sending a field the server does not
// understand is usually a bug or an attempt to smuggle one.
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, dst any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_body", "the request body is not valid JSON")
		return false
	}
	return true
}
