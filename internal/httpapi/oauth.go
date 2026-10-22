package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/oauth"
)

// The OAuth2 authorization server (IDN-11) and the client registry that feeds it (ADM-15).
//
// # There is no second authorization path
//
// A bearer access token resolves to exactly the identity shape an API key produces: the owning
// *user* plus a set of scopes that narrow what that user may do. It is attached with the same
// `withAPIKey` helper the API-key path uses, so every existing guard — `requirePermission`,
// the CSRF exemption for a bearer credential, the audit actor — applies unchanged. The scope
// list is checked against the user's roles as they are *now*, on every request, which is what
// makes a revoked permission narrow a token that was minted before the revocation.
//
// # The token endpoint speaks OAuth2 and problem+json
//
// RFC 6749 defines the token response and its error member; the rest of this API answers
// `application/problem+json` with a stable `code`. This endpoint does both: the body is a
// problem document *and* carries `error`, set to the same stable string, so a standard OAuth2
// client reads the member it expects and the panel's own client reads the code it expects.
//
// # The consent screen is HTML, and that is the only HTML here
//
// /oauth/authorize is a browser destination: a third party redirects a person to it. A JSON
// error there would be a dead end for the person, so the screen is a small server-rendered
// page with no scripts, `Referrer-Policy: no-referrer` and `Cache-Control: no-store`. Every
// error that happens *before* the client and its redirect URI are validated is a problem
// document, because redirecting to an unvalidated URI is how a code is delivered to an
// attacker.

func init() {
	RegisterRoutes("95-oauth", registerOAuth)
}

func registerOAuth(r *router) {
	// The authorization endpoint: a browser gets redirected here, so it is public and it
	// redirects to the sign-in screen when there is no session.
	r.add(Route{Method: http.MethodGet, Pattern: "/oauth/authorize", Access: AccessPublic,
		Rate: RateRead, Handler: r.s.handleAuthorize})
	r.add(Route{Method: http.MethodPost, Pattern: "/oauth/authorize", Access: AccessPublic,
		Rate: RateMutation, Handler: r.s.handleAuthorizeDecision})
	// The token endpoint: client-authenticated, never session-authenticated.
	r.add(Route{Method: http.MethodPost, Pattern: "/oauth/token", Access: AccessPublic,
		Rate: RateMutation, Handler: r.s.handleToken})

	// The documented API a third party calls with the token it was given.
	r.userGet("/api/v1/me", r.s.handleAPIMe)
	r.get("/api/v1/me/invoices", authn.PermInvoiceReadSelf, r.s.handleAPIMeInvoices)

	// The client registry. Registering a client is an access decision, hence oauth:manage.
	r.staffGet("/api/v1/staff/oauth-clients", authn.PermOAuthManage, r.s.handleOAuthClientList)
	r.staffGet("/api/v1/staff/oauth-clients/{id}", authn.PermOAuthManage, r.s.handleOAuthClientGet)
	r.staffMutation(http.MethodPost, "/api/v1/staff/oauth-clients",
		authn.PermOAuthManage, r.s.handleOAuthClientCreate)
	r.staffMutation(http.MethodPost, "/api/v1/staff/oauth-clients/{id}/rotate-secret",
		authn.PermOAuthManage, r.s.handleOAuthClientRotate)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/oauth-clients/{id}/secret",
		authn.PermOAuthManage, r.s.handleOAuthClientDisableSecret)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/oauth-clients/{id}",
		authn.PermOAuthManage, r.s.handleOAuthClientDelete)
}

// oauthServices caches one service per pool.
//
// The service carries the process-wide state-signing key cache, so a new one per request would
// re-read the key on every callback. The key is the pool pointer because that is what
// identifies an installation; the auditor is attached per request, because it names the person
// and the address.
var oauthServices sync.Map

// oauthService returns the OAuth2 service for this server.
//
// The cached value carries only the pool and the process-wide state-signing key cache; the
// permission source and the auditor are attached per request. Caching the permission source
// would be a bug waiting to happen: it closes over one Server's Authorizer, and a second Server
// built on the same pool (which is what every test in this package does) would silently check
// permissions against the first one's.
func (s *Server) oauthService() *oauth.Service {
	if s.pool == nil {
		return nil
	}
	if cached, ok := oauthServices.Load(s.pool); ok {
		return cached.(*oauth.Service)
	}
	actual, _ := oauthServices.LoadOrStore(s.pool, oauth.New(s.pool))
	return actual.(*oauth.Service)
}

// oauthFor returns the service with this request's permission source and auditor attached.
func (s *Server) oauthFor(r *http.Request) *oauth.Service {
	svc := s.oauthService()
	if svc == nil {
		return nil
	}
	return svc.WithPermissions(s.oauthPermissions).WithAuditor(s.auditorFor(r, ""))
}

// oauthPermissions resolves a user's live permissions for the token-minting path.
//
// It goes through authn.Authorizer — the same table every route uses — rather than through a
// copy stored on the code, so a token minted after a role change carries the new set.
func (s *Server) oauthPermissions(ctx context.Context, userID string) (map[authn.Permission]struct{}, error) {
	if s.auth == nil || s.auth.Service == nil || s.auth.Authz == nil {
		return nil, errors.New("httpapi: authentication is not configured")
	}
	user, err := s.auth.Service.UserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.auth.Authz.Permissions(user), nil
}

// ------------------------------------------------------------------ identity --

// oauthTokenCtxKey is the context key for the OAuth token behind a request.
type oauthTokenCtxKey struct{}

// withOAuthToken attaches the token metadata to a request context.
func withOAuthToken(ctx context.Context, token oauth.AccessToken) context.Context {
	return context.WithValue(ctx, oauthTokenCtxKey{}, token)
}

// oauthTokenFrom returns the OAuth token, if the request used one.
func oauthTokenFrom(ctx context.Context) (oauth.AccessToken, bool) {
	token, ok := ctx.Value(oauthTokenCtxKey{}).(oauth.AccessToken)
	return token, ok
}

// oauthIdentity resolves a bearer access token into a user plus narrowing scopes.
//
// The identity it returns is the *API key* identity shape on purpose: an OAuth token narrows
// its owner exactly the way a key does, so it rides the same permission check instead of
// getting one of its own. A token that is unknown, expired or revoked is not this function's
// error to report — it returns false, and the caller decides between "no credential" and "bad
// credential".
func (s *Server) oauthIdentity(r *http.Request, presented string) (APIKeyIdentity, authn.User, oauth.AccessToken, bool) {
	if s.auth == nil || s.auth.Service == nil || s.pool == nil {
		return APIKeyIdentity{}, authn.User{}, oauth.AccessToken{}, false
	}
	// A token that is not ours costs no database round trip.
	if !strings.HasPrefix(presented, "at_") {
		return APIKeyIdentity{}, authn.User{}, oauth.AccessToken{}, false
	}
	token, err := s.oauthService().Authenticate(r.Context(), presented)
	if err != nil {
		return APIKeyIdentity{}, authn.User{}, oauth.AccessToken{}, false
	}
	user, err := s.auth.Service.UserByID(r.Context(), token.UserID)
	if err != nil || user.Status != "active" {
		// A token outliving its owner's disabled account would be a permanent way back in.
		return APIKeyIdentity{}, authn.User{}, oauth.AccessToken{}, false
	}
	ident := APIKeyIdentity{KeyID: token.ID, UserID: token.UserID, Scopes: token.Scopes}
	return ident, user, token, true
}

// -------------------------------------------------------------- token endpoint --

// oauthErrorCode maps a domain error to the OAuth2 error member and the HTTP status.
//
// The names are RFC 6749 §5.2's, not invented ones, because a third-party client branches on
// them. An unrecognised error is a 500 with a code that says so rather than a 400 that blames
// the caller.
func oauthErrorCode(err error) (int, string) {
	switch {
	case errors.Is(err, oauth.ErrUnknownClient), errors.Is(err, oauth.ErrClientAuth):
		return http.StatusUnauthorized, "invalid_client"
	case errors.Is(err, oauth.ErrInvalidGrant), errors.Is(err, oauth.ErrInvalidCode):
		return http.StatusBadRequest, "invalid_grant"
	case errors.Is(err, oauth.ErrUnsupportedGrant):
		return http.StatusBadRequest, "unsupported_grant_type"
	case errors.Is(err, oauth.ErrInvalidScope):
		return http.StatusBadRequest, "invalid_scope"
	case errors.Is(err, oauth.ErrInvalidRedirect), errors.Is(err, oauth.ErrInvalidInput),
		errors.Is(err, oauth.ErrUnsupportedResponseType):
		return http.StatusBadRequest, "invalid_request"
	case errors.Is(err, oauth.ErrPKCERequired), errors.Is(err, oauth.ErrPKCEMethod),
		errors.Is(err, oauth.ErrPKCEVerification):
		return http.StatusBadRequest, "invalid_grant"
	default:
		return http.StatusInternalServerError, "internal_error"
	}
}

// writeOAuthProblem writes a problem document that also carries the RFC 6749 `error` member.
func writeOAuthProblem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="oauth/token"`)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   "about:blank",
		"title":  http.StatusText(status),
		"status": status,
		"detail": detail,
		"code":   code,
		// The same value under the member a standard OAuth2 client reads.
		"error": code,
	})
}

// handleToken serves POST /oauth/token.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	svc := s.oauthFor(r)
	if svc == nil {
		writeOAuthProblem(w, http.StatusServiceUnavailable, "oauth_not_configured",
			"the authorization server is not configured")
		return
	}
	// A bound on credential guessing. The shared limiter keys on a principal and an
	// unauthenticated request has none, so the token endpoint meters by address instead: it is
	// the one public endpoint where a secret is checked.
	if s.rateLimiter != nil {
		if ok, _, reset := s.rateLimiter.allow("oauth-token:"+s.clientIP(r), RateMutation); !ok {
			seconds := int(time.Until(reset).Seconds())
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			writeOAuthProblem(w, http.StatusTooManyRequests, "rate_limited",
				"too many token requests; retry after the window resets")
			return
		}
	}

	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/x-www-form-urlencoded") {
		writeOAuthProblem(w, http.StatusBadRequest, "invalid_request",
			"the token endpoint takes application/x-www-form-urlencoded")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if err := r.ParseForm(); err != nil {
		writeOAuthProblem(w, http.StatusBadRequest, "invalid_request", "the form body could not be parsed")
		return
	}

	clientID, clientSecret := tokenClientCredentials(r)
	ctx := r.Context()

	var (
		token oauth.Token
		err   error
	)
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		token, err = svc.ExchangeCode(ctx, oauth.CodeExchange{
			Code:         r.PostForm.Get("code"),
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURI:  r.PostForm.Get("redirect_uri"),
			CodeVerifier: r.PostForm.Get("code_verifier"),
		})
	case "refresh_token":
		token, err = svc.Refresh(ctx, oauth.RefreshExchange{
			RefreshToken: r.PostForm.Get("refresh_token"),
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Scope:        r.PostForm.Get("scope"),
		})
	case "":
		err = fmt.Errorf("%w: grant_type is required", oauth.ErrUnsupportedGrant)
	default:
		err = fmt.Errorf("%w: %q", oauth.ErrUnsupportedGrant, r.PostForm.Get("grant_type"))
	}
	if err != nil {
		status, code := oauthErrorCode(err)
		if status >= 500 {
			s.cfg.Logger.ErrorContext(ctx, "token request failed", "error", err)
			writeOAuthProblem(w, status, code, "the request could not be completed")
			return
		}
		writeOAuthProblem(w, status, code, err.Error())
		return
	}

	// Cache-Control: no-store on a token response is not politeness: a shared cache holding an
	// access token hands it to the next caller.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  token.AccessToken,
		"token_type":    token.TokenType,
		"expires_in":    token.ExpiresIn,
		"refresh_token": token.RefreshToken,
		"scope":         token.Scope,
	})
}

// tokenClientCredentials reads client authentication from the Basic header or the form.
//
// Both are in RFC 6749; the header is preferred because a secret in a form body ends up in
// access logs, and a client that sends both is read from the header.
func tokenClientCredentials(r *http.Request) (clientID, clientSecret string) {
	if header := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(header), "basic ") {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header[6:]))
		if err == nil {
			id, secret, ok := strings.Cut(string(raw), ":")
			if ok {
				// RFC 6749 §2.3.1: both halves are form-urlencoded before base64.
				id, _ = url.QueryUnescape(id)
				secret, _ = url.QueryUnescape(secret)
				return id, secret
			}
		}
	}
	return r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
}

// ---------------------------------------------------------- authorize endpoint --

// handleAuthorize serves GET /oauth/authorize.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	svc := s.oauthFor(r)
	if svc == nil || s.auth == nil || s.auth.Authz == nil {
		writeProblem(w, http.StatusServiceUnavailable, "oauth_not_configured",
			"the authorization server is not configured")
		return
	}
	query := r.URL.Query()
	req := oauth.AuthorizationRequest{
		ResponseType:        query.Get("response_type"),
		ClientID:            query.Get("client_id"),
		RedirectURI:         query.Get("redirect_uri"),
		Scope:               query.Get("scope"),
		State:               query.Get("state"),
		CodeChallenge:       query.Get("code_challenge"),
		CodeChallengeMethod: query.Get("code_challenge_method"),
	}

	user, signedIn := userFrom(r.Context())
	if !signedIn {
		// The sign-in screen brings the person back to exactly this URL, query string and all,
		// so nothing the client sent is lost on the way.
		next := r.URL.RequestURI()
		http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusFound)
		return
	}

	prepared, err := svc.PrepareAuthorization(r.Context(), req, user, s.auth.Authz)
	if err != nil {
		s.authorizeError(w, r, svc, req, err)
		return
	}
	if !prepared.NeedsConsent {
		s.completeAuthorize(w, r, svc, prepared, user)
		return
	}
	s.renderConsent(w, r, prepared, user)
}

// handleAuthorizeDecision serves POST /oauth/authorize: the consent form's answer.
func (s *Server) handleAuthorizeDecision(w http.ResponseWriter, r *http.Request) {
	svc := s.oauthFor(r)
	if svc == nil || s.auth == nil || s.auth.Authz == nil {
		writeProblem(w, http.StatusServiceUnavailable, "oauth_not_configured",
			"the authorization server is not configured")
		return
	}
	user, signedIn := userFrom(r.Context())
	if !signedIn {
		writeProblem(w, http.StatusUnauthorized, "authentication_required", "sign in to continue")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_body", "the form body could not be parsed")
		return
	}

	// The consent form carries the session's CSRF token as a hidden field rather than in a
	// header, because it is a plain HTML form. The comparison is the same one requireCSRF
	// makes; the route is public because the third party's redirect arrives without one.
	sess, _ := sessionFrom(r.Context())
	if err := authn.VerifyCSRF(sess, r.PostForm.Get("csrf_token")); err != nil {
		writeProblem(w, http.StatusForbidden, "csrf_failed", "the CSRF token is missing or invalid")
		return
	}

	req := oauth.AuthorizationRequest{
		ResponseType:        r.PostForm.Get("response_type"),
		ClientID:            r.PostForm.Get("client_id"),
		RedirectURI:         r.PostForm.Get("redirect_uri"),
		Scope:               r.PostForm.Get("scope"),
		State:               r.PostForm.Get("state"),
		CodeChallenge:       r.PostForm.Get("code_challenge"),
		CodeChallengeMethod: r.PostForm.Get("code_challenge_method"),
	}
	prepared, err := svc.PrepareAuthorization(r.Context(), req, user, s.auth.Authz)
	if err != nil {
		s.authorizeError(w, r, svc, req, err)
		return
	}
	if r.PostForm.Get("decision") != "approve" {
		// A refusal is a redirect with `error=access_denied`, which is what the client's
		// library expects and what tells the person the application was not authorized.
		redirectWithError(w, r, prepared.RedirectURI, "access_denied", prepared.State)
		return
	}
	s.completeAuthorize(w, r, svc, prepared, user)
}

// completeAuthorize issues the code and redirects back to the client.
func (s *Server) completeAuthorize(w http.ResponseWriter, r *http.Request, svc *oauth.Service,
	prepared oauth.Authorization, user authn.User) {
	code, err := svc.IssueCode(r.Context(), prepared, user.ID)
	if err != nil {
		status, problemCode := oauthErrorCode(err)
		if status >= 500 {
			s.cfg.Logger.ErrorContext(r.Context(), "issue authorization code failed", "error", err)
		}
		writeProblem(w, status, problemCode, "the authorization could not be completed")
		return
	}
	target, err := url.Parse(prepared.RedirectURI)
	if err != nil {
		// Unreachable: the URI was matched against the registered list, which is validated.
		writeProblem(w, http.StatusBadRequest, "invalid_redirect", "the redirect URI is not a URL")
		return
	}
	q := target.Query()
	q.Set("code", code)
	if prepared.State != "" {
		q.Set("state", prepared.State)
	}
	target.RawQuery = q.Encode()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// authorizeError reports a failed authorization.
//
// Errors raised before the client and redirect URI are validated are problem documents: there
// is no verified place to send the person, and inventing one is the open redirect this whole
// endpoint has to avoid. Errors raised after are redirects with `error`, because the client is
// waiting at a URI it registered.
func (s *Server) authorizeError(w http.ResponseWriter, r *http.Request, svc *oauth.Service,
	req oauth.AuthorizationRequest, err error) {
	_, redirect, resolveErr := svc.ClientRedirect(r.Context(), req.ClientID, req.RedirectURI)
	if resolveErr != nil {
		// There is no verified destination, so the failure has to be readable here. The code
		// names which half was wrong: an unknown client and an unregistered redirect are
		// different operator mistakes.
		if errors.Is(resolveErr, oauth.ErrUnknownClient) {
			writeProblem(w, http.StatusBadRequest, "unknown_client", "no such client, or it is disabled")
			return
		}
		writeProblem(w, http.StatusBadRequest, "invalid_redirect",
			"the redirect_uri is not registered for this client")
		return
	}
	code := "invalid_request"
	switch {
	case errors.Is(err, oauth.ErrInvalidScope):
		code = "invalid_scope"
	case errors.Is(err, oauth.ErrUnsupportedResponseType):
		code = "unsupported_response_type"
	case errors.Is(err, oauth.ErrPKCERequired), errors.Is(err, oauth.ErrPKCEMethod),
		errors.Is(err, oauth.ErrPKCEVerification):
		code = "invalid_request"
	}
	redirectWithError(w, r, redirect, code, req.State)
}

// redirectWithError sends the person back to the client with an OAuth2 error.
func redirectWithError(w http.ResponseWriter, r *http.Request, redirectURI, code, state string) {
	target, err := url.Parse(redirectURI)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_redirect", "the redirect URI is not a URL")
		return
	}
	q := target.Query()
	q.Set("error", code)
	if state != "" {
		q.Set("state", state)
	}
	target.RawQuery = q.Encode()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// renderConsent writes the consent screen.
//
// It is deliberately plain: no scripts, no stylesheet, no image, `Referrer-Policy:
// no-referrer` so the query string (which contains the client id and the state) cannot leak to
// a third party through a referrer, and `no-store` so a back button does not re-submit a stale
// decision.
func (s *Server) renderConsent(w http.ResponseWriter, r *http.Request, prepared oauth.Authorization,
	user authn.User) {
	sess, _ := sessionFrom(r.Context())

	var body strings.Builder
	body.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8">`)
	body.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	body.WriteString(`<title>Authorize ` + html.EscapeString(prepared.Client.Name) + `</title>`)
	body.WriteString(`<style>body{font:16px/1.5 system-ui,sans-serif;max-width:34rem;margin:4rem auto;padding:0 1.5rem;color:#16181d}`)
	body.WriteString(`h1{font-size:1.35rem;margin:0 0 .25rem}p{color:#4a4f57}ul{padding-left:1.1rem;color:#4a4f57}`)
	body.WriteString(`code{background:#f2f3f5;padding:.1rem .3rem;border-radius:3px;font-size:.9em}`)
	body.WriteString(`form{display:inline-block;margin:1.5rem .5rem 0 0}button{font:inherit;padding:.6rem 1.1rem;border-radius:6px;border:1px solid #c8ccd2;background:#fff;cursor:pointer}`)
	body.WriteString(`button.primary{background:#16181d;color:#fff;border-color:#16181d}</style></head><body>`)
	body.WriteString(`<h1>Authorize <strong>` + html.EscapeString(prepared.Client.Name) + `</strong>?</h1>`)
	body.WriteString(`<p>` + html.EscapeString(prepared.Client.Name) +
		` is asking to act as <strong>` + html.EscapeString(user.Email) + `</strong>. ` +
		`It will be able to:</p><ul>`)
	for _, scope := range prepared.Scopes {
		body.WriteString(`<li><code>` + html.EscapeString(scope) + `</code> — ` +
			html.EscapeString(describeScope(scope)) + `</li>`)
	}
	body.WriteString(`</ul><p>You can revoke this at any time. If you do not recognise this application, refuse.</p>`)

	form := func(decision, label, class string) {
		body.WriteString(`<form method="post" action="/oauth/authorize">`)
		for name, value := range map[string]string{
			"response_type":         "code",
			"client_id":             prepared.Client.ClientID,
			"redirect_uri":          prepared.RedirectURI,
			"scope":                 oauth.JoinScopeList(prepared.Scopes),
			"state":                 prepared.State,
			"code_challenge":        prepared.CodeChallenge,
			"code_challenge_method": prepared.CodeChallengeMethod,
			"csrf_token":            sess.CSRFToken,
			"decision":              decision,
		} {
			if value == "" {
				continue
			}
			body.WriteString(`<input type="hidden" name="` + html.EscapeString(name) +
				`" value="` + html.EscapeString(value) + `">`)
		}
		body.WriteString(`<button type="submit" class="` + class + `">` + label + `</button></form>`)
	}
	form("approve", "Authorize", "primary")
	form("deny", "Refuse", "")
	body.WriteString(`</body></html>`)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body.String()))
}

// describeScope turns a permission name into a sentence a person can judge.
//
// It reads the permission rather than a second table of descriptions, so a new permission is
// described by the same naming convention every existing one follows.
func describeScope(scope string) string {
	action, rest, _ := strings.Cut(scope, ":")
	object, target, _ := strings.Cut(rest, ":")
	verb := map[string]string{
		"read": "Read", "write": "Change", "create": "Create", "manage": "Manage",
		"void": "Void", "refund": "Refund", "reconcile": "Reconcile", "suspend": "Suspend",
		"terminate": "Terminate", "provision": "Provision", "upgrade": "Upgrade",
		"hold": "Place and clear holds on", "pay": "Pay", "cancel": "Cancel",
		"import": "Import", "update": "Update",
	}[action]
	if verb == "" {
		verb = capitalize(action)
	}
	noun := map[string]string{
		"customer": "customers", "invoice": "invoices", "service": "services",
		"ticket": "tickets", "payment": "payments", "paymentmethod": "payment methods",
		"order": "orders", "credit": "credits", "coupon": "coupons", "tax": "tax rates",
		"currency": "currencies", "config": "configurable options", "catalog": "the catalog",
		"extension": "extensions", "settings": "settings", "staff": "staff accounts",
		"audit": "the audit log", "report": "reports", "apikey": "API keys",
		"notification": "message templates", "gateway": "payment gateways",
		"server": "provisioning servers", "log": "logs", "announcement": "announcements",
		"i18n": "translations", "import": "imports", "oauth": "OAuth clients",
		"update": "panel updates",
	}[object]
	if noun == "" {
		noun = object
	}
	whose := "everything"
	switch target {
	case "self":
		whose = "your own"
	case "all":
		whose = "every customer's"
	}
	return verb + " " + whose + " " + noun
}

// --------------------------------------------------------------- the API --

// handleAPIMe serves GET /api/v1/me: who the credential is for, and what it may do.
//
// It is the endpoint a third party calls first, because it answers both questions in one
// request: the account behind the token and the scopes the token was narrowed to. The
// permission list is resolved from the user's roles *now*, so a client can see its token has
// been narrowed rather than discovering it through a 403.
func (s *Server) handleAPIMe(w http.ResponseWriter, r *http.Request) {
	user, ok := userFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication_required", "sign in to continue")
		return
	}

	auth := map[string]any{"kind": "session"}
	if token, ok := oauthTokenFrom(r.Context()); ok {
		auth = map[string]any{
			"kind":       "oauth",
			"client_id":  token.ClientID,
			"grant_id":   token.GrantID,
			"scopes":     token.Scopes,
			"expires_at": token.ExpiresAt.UTC(),
		}
	} else if key, ok := apiKeyFrom(r.Context()); ok {
		auth = map[string]any{"kind": "api_key", "key_id": key.KeyID, "scopes": key.Scopes}
	}

	out := map[string]any{
		"user": map[string]any{
			"id":             user.ID,
			"email":          user.Email,
			"display_name":   user.DisplayName,
			"roles":          user.Roles,
			"is_staff":       user.IsStaff(),
			"email_verified": user.EmailVerified,
		},
		"auth": auth,
	}
	if s.auth != nil && s.auth.Authz != nil {
		held := s.auth.Authz.Permissions(user)
		permissions := make([]string, 0, len(held))
		for permission := range held {
			permissions = append(permissions, string(permission))
		}
		// Sorted so the response is stable for a client that diffs it.
		sort.Strings(permissions)
		out["permissions"] = permissions
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAPIMeInvoices serves GET /api/v1/me/invoices.
//
// It is the documented read a third party actually makes, and it exists to prove the point:
// the same portal query a session uses, behind the same permission, reached with a token. The
// customer id is resolved from the caller's identity, never from the request.
func (s *Server) handleAPIMeInvoices(w http.ResponseWriter, r *http.Request) {
	if s.portal == nil {
		writeProblem(w, http.StatusServiceUnavailable, "portal_unavailable", "the portal is not configured")
		return
	}
	user, _ := userFrom(r.Context())
	invoices, err := s.portal.Invoices(r.Context(), user.ID)
	if err != nil {
		s.writePortalError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(invoices))
	for _, inv := range invoices {
		out = append(out, invoiceJSON(inv))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// ------------------------------------------------------------ client registry --

// oauthClientJSON is the shape of a client everywhere it is returned.
//
// There is no secret field. The create and rotate responses add `secret` and `shown_once`;
// nothing else does, because nothing else can.
func oauthClientJSON(c oauth.Client) map[string]any {
	out := map[string]any{
		"id":                c.ID,
		"client_id":         c.ClientID,
		"name":              c.Name,
		"confidential":      c.Confidential,
		"has_secret":        c.HasSecret,
		"redirect_uris":     c.RedirectURIs,
		"allowed_scopes":    c.AllowedScopes,
		"enabled":           c.Enabled,
		"created_at":        c.CreatedAt.UTC(),
		"secret_rotated_at": nil,
	}
	if c.SecretRotatedAt != nil {
		out["secret_rotated_at"] = c.SecretRotatedAt.UTC()
	}
	return out
}

func (s *Server) handleOAuthClientList(w http.ResponseWriter, r *http.Request) {
	svc := s.oauthFor(r)
	if svc == nil {
		writeProblem(w, http.StatusServiceUnavailable, "oauth_not_configured",
			"the authorization server is not configured")
		return
	}
	clients, err := svc.ListClients(r.Context())
	if err != nil {
		s.oauthInternalError(w, r, "list oauth clients", err)
		return
	}
	out := make([]map[string]any, 0, len(clients))
	for _, c := range clients {
		out = append(out, oauthClientJSON(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) handleOAuthClientGet(w http.ResponseWriter, r *http.Request) {
	svc := s.oauthFor(r)
	if svc == nil {
		writeProblem(w, http.StatusServiceUnavailable, "oauth_not_configured",
			"the authorization server is not configured")
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such client")
		return
	}
	client, err := svc.ClientByID(r.Context(), id)
	if errors.Is(err, oauth.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such client")
		return
	}
	if err != nil {
		s.oauthInternalError(w, r, "read oauth client", err)
		return
	}
	writeJSON(w, http.StatusOK, oauthClientJSON(client))
}

func (s *Server) handleOAuthClientCreate(w http.ResponseWriter, r *http.Request) {
	svc := s.oauthFor(r)
	if svc == nil {
		writeProblem(w, http.StatusServiceUnavailable, "oauth_not_configured",
			"the authorization server is not configured")
		return
	}
	user, _ := userFrom(r.Context())

	var req struct {
		Name          string   `json:"name"`
		RedirectURIs  []string `json:"redirect_uris"`
		AllowedScopes []string `json:"allowed_scopes"`
		// Confidential defaults to false: a client that can keep a secret has to say so,
		// because a public client with a secret it cannot keep is the more dangerous default.
		Confidential bool `json:"confidential"`
	}
	if !s.decodeJSON(w, r, &req, 32<<10) {
		return
	}

	created, err := svc.CreateClient(r.Context(), oauth.NewClient{
		Name:          req.Name,
		RedirectURIs:  req.RedirectURIs,
		AllowedScopes: req.AllowedScopes,
		Confidential:  req.Confidential,
		CreatedBy:     user.ID,
	})
	if err != nil {
		if errors.Is(err, oauth.ErrInvalidInput) {
			writeProblem(w, http.StatusBadRequest, "invalid_client_request", err.Error())
			return
		}
		s.oauthInternalError(w, r, "create oauth client", err)
		return
	}

	out := oauthClientJSON(created.Client)
	if created.Secret != "" {
		out["secret"] = created.Secret
		out["shown_once"] = created.ShownOnce
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleOAuthClientRotate(w http.ResponseWriter, r *http.Request) {
	svc := s.oauthFor(r)
	if svc == nil {
		writeProblem(w, http.StatusServiceUnavailable, "oauth_not_configured",
			"the authorization server is not configured")
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such client")
		return
	}
	rotated, err := svc.RotateSecret(r.Context(), id)
	if errors.Is(err, oauth.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such client")
		return
	}
	if err != nil {
		s.oauthInternalError(w, r, "rotate oauth client secret", err)
		return
	}
	out := oauthClientJSON(rotated.Client)
	out["secret"] = rotated.Secret
	out["shown_once"] = rotated.ShownOnce
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleOAuthClientDisableSecret(w http.ResponseWriter, r *http.Request) {
	svc := s.oauthFor(r)
	if svc == nil {
		writeProblem(w, http.StatusServiceUnavailable, "oauth_not_configured",
			"the authorization server is not configured")
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such client")
		return
	}
	client, err := svc.DisableSecret(r.Context(), id)
	if errors.Is(err, oauth.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such client")
		return
	}
	if err != nil {
		s.oauthInternalError(w, r, "disable oauth client secret", err)
		return
	}
	writeJSON(w, http.StatusOK, oauthClientJSON(client))
}

func (s *Server) handleOAuthClientDelete(w http.ResponseWriter, r *http.Request) {
	svc := s.oauthFor(r)
	if svc == nil {
		writeProblem(w, http.StatusServiceUnavailable, "oauth_not_configured",
			"the authorization server is not configured")
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such client")
		return
	}
	err := svc.DeleteClient(r.Context(), id)
	if errors.Is(err, oauth.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such client")
		return
	}
	if err != nil {
		s.oauthInternalError(w, r, "delete oauth client", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// oauthInternalError logs and reports a failure without leaking its detail.
func (s *Server) oauthInternalError(w http.ResponseWriter, r *http.Request, action string, err error) {
	s.cfg.Logger.ErrorContext(r.Context(), "oauth request failed", "action", action, "error", err)
	writeProblem(w, http.StatusInternalServerError, "internal_error", "the request could not be completed")
}

// capitalize upper-cases the first letter, for a label built from a permission name.
func capitalize(value string) string {
	if value == "" {
		return value
	}
	return strings.ToUpper(value[:1]) + value[1:]
}
