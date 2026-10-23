package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/oauth"
)

// Social login (IDN-07, IDN-08, IDN-09) and the provider registry it reads (IDN-10).
//
// The shape is deliberately generic. Google, GitHub and Discord are three rows in
// `social_providers`, not three code paths: the endpoints, the scopes and the claim names are
// configuration, so a fourth provider is an insert and a fix here fixes all of them.
//
// Three endpoints, and what each one is for:
//
//	GET /api/v1/auth/providers     the sign-in screen's button list. With nothing configured it
//	                               returns an empty list, which renders as no buttons rather
//	                               than a broken one.
//	GET /oauth/social/{slug}/start the redirect to the provider, with a signed single-use state.
//	GET /oauth/social/{slug}/callback the provider's return, which either signs an existing link
//	                               in or refuses and says why.
//
// The refusal is the point. An existing account with the same email is never signed in and
// never merged: the callback sends the person to the sign-in screen with `account_exists`, and
// the screen tells them to sign in and link. That is the one decision in this file that stops
// an account takeover, and it is not configurable.

func init() {
	RegisterRoutes("96-social", registerSocial)
}

func registerSocial(r *router) {
	// Public: the sign-in screen and the two legs of the redirect dance.
	r.publicGet("/api/v1/auth/providers", r.s.handleAuthProviders)
	r.add(Route{Method: http.MethodGet, Pattern: "/oauth/social/{slug}/start",
		Access: AccessPublic, Rate: RateRead, Handler: r.s.handleSocialStart})
	r.add(Route{Method: http.MethodGet, Pattern: "/oauth/social/{slug}/callback",
		Access: AccessPublic, Rate: RateRead, Handler: r.s.handleSocialCallback})

	// The provider registry. It carries provider credentials, so it is oauth:manage.
	r.staffGet("/api/v1/staff/oauth-providers", authn.PermOAuthManage, r.s.handleProviderList)
	r.staffGet("/api/v1/staff/oauth-providers/{id}", authn.PermOAuthManage, r.s.handleProviderGet)
	r.staffMutation(http.MethodPost, "/api/v1/staff/oauth-providers",
		authn.PermOAuthManage, r.s.handleProviderCreate)
	r.staffMutation(http.MethodPut, "/api/v1/staff/oauth-providers/{id}",
		authn.PermOAuthManage, r.s.handleProviderUpdate)
	r.staffMutation(http.MethodDelete, "/api/v1/staff/oauth-providers/{id}",
		authn.PermOAuthManage, r.s.handleProviderDelete)

	// A customer's own links, so "sign in and link" has somewhere to happen.
	r.get("/api/v1/portal/social-links", authn.PermCustomerReadSelf, r.s.handleSocialLinks)
	r.mutation(http.MethodDelete, "/api/v1/portal/social-links/{id}",
		authn.PermCustomerWriteSelf, r.s.handleSocialUnlink)
}

// ------------------------------------------------------------- sign-in list --

// handleAuthProviders lists the providers the sign-in screen may offer.
//
// It is public because the sign-in screen is public, and it carries nothing but a name and a
// URL. A disabled provider is omitted: a button that leads to a refusal is worse than no
// button, and "no providers configured" is the same shape as "none enabled".
func (s *Server) handleAuthProviders(w http.ResponseWriter, r *http.Request) {
	svc := s.oauthService()
	if svc == nil {
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{}})
		return
	}
	providers, err := svc.ListProviders(r.Context(), true)
	if err != nil {
		s.oauthInternalError(w, r, "list social providers", err)
		return
	}
	out := make([]map[string]any, 0, len(providers))
	for _, p := range providers {
		out = append(out, map[string]any{
			"slug": p.Slug,
			"name": p.Name,
			// The screen renders one link; the start endpoint decides whether it is a sign-in
			// or a link from the session and the query string.
			"start_url": "/oauth/social/" + url.PathEscape(p.Slug) + "/start",
			"link_url":  "/oauth/social/" + url.PathEscape(p.Slug) + "/start?link=1",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// ------------------------------------------------------------------- start --

// handleSocialStart redirects to the provider with a signed, single-use state.
//
// `?link=1` switches the flow from "sign me in" to "attach this provider to the account I am
// already signed in to". The two modes are recorded inside the signed state rather than
// trusted from the callback's query string, so a callback cannot claim to be a link flow when
// it was started as a sign-in.
func (s *Server) handleSocialStart(w http.ResponseWriter, r *http.Request) {
	svc := s.oauthFor(r)
	if svc == nil {
		writeProblem(w, http.StatusServiceUnavailable, "oauth_not_configured",
			"social login is not configured")
		return
	}
	provider, ok := s.socialProvider(w, r, svc)
	if !ok {
		return
	}

	mode := oauth.StateSignIn
	userID := ""
	if r.URL.Query().Get("link") == "1" {
		user, signedIn := userFrom(r.Context())
		if !signedIn {
			// Linking needs an account to link to. Sending the person to sign in first is the
			// only honest answer; a link state with no user cannot be signed.
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}
		mode = oauth.StateLink
		userID = user.ID
	}

	state, err := svc.SignState(r.Context(), provider, mode, userID)
	if err != nil {
		s.oauthInternalError(w, r, "sign social state", err)
		return
	}

	target, err := url.Parse(provider.AuthorizationEndpoint)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "provider_misconfigured",
			"the provider's authorization endpoint is not a URL")
		return
	}
	q := target.Query()
	q.Set("response_type", "code")
	q.Set("client_id", provider.ClientID)
	q.Set("redirect_uri", s.socialRedirectURI(provider))
	q.Set("scope", strings.Join(provider.Scopes, " "))
	q.Set("state", state)
	target.RawQuery = q.Encode()

	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// ---------------------------------------------------------------- callback --

// handleSocialCallback completes the flow.
//
// Every user-facing outcome is a redirect to the sign-in screen with `social_error=<code>`,
// because the person is in a browser and a JSON document would be a dead end. A malformed
// request (unknown provider, no code) is a problem document, because there is no screen that
// could explain it.
func (s *Server) handleSocialCallback(w http.ResponseWriter, r *http.Request) {
	svc := s.oauthFor(r)
	if svc == nil {
		writeProblem(w, http.StatusServiceUnavailable, "oauth_not_configured",
			"social login is not configured")
		return
	}
	provider, ok := s.socialProvider(w, r, svc)
	if !ok {
		return
	}

	query := r.URL.Query()
	if providerErr := query.Get("error"); providerErr != "" {
		// The provider refused (usually the person pressed cancel). Nothing to do but say so.
		s.socialRefuse(w, r, "provider_denied")
		return
	}
	code := query.Get("code")
	if code == "" {
		writeProblem(w, http.StatusBadRequest, "missing_code", "the provider returned no authorization code")
		return
	}

	claims, err := svc.ConsumeState(r.Context(), provider, query.Get("state"))
	if err != nil {
		if errors.Is(err, oauth.ErrInvalidState) {
			s.socialRefuse(w, r, "state_invalid")
			return
		}
		s.oauthInternalError(w, r, "consume social state", err)
		return
	}

	identity, err := svc.ExchangeIdentity(r.Context(), provider, code, s.socialRedirectURI(provider))
	if err != nil {
		if errors.Is(err, oauth.ErrProviderExchange) {
			s.cfg.Logger.WarnContext(r.Context(), "social provider exchange failed",
				"provider", provider.Slug, "error", err)
			s.socialRefuse(w, r, "provider_failed")
			return
		}
		s.oauthInternalError(w, r, "exchange social identity", err)
		return
	}

	if claims.Mode == oauth.StateLink {
		s.completeSocialLink(w, r, svc, provider, identity, claims.UserID)
		return
	}
	s.completeSocialSignIn(w, r, svc, provider, identity)
}

// completeSocialSignIn signs in an account that is already linked, and refuses otherwise.
func (s *Server) completeSocialSignIn(w http.ResponseWriter, r *http.Request, svc *oauth.Service,
	provider oauth.Provider, identity oauth.Identity) {
	result, err := svc.CompleteSignIn(r.Context(), provider, identity)
	if err != nil {
		s.oauthInternalError(w, r, "complete social sign-in", err)
		return
	}
	switch result.Outcome {
	case oauth.OutcomeAccountExists:
		// The refusal that prevents an account takeover. No session, no link, no merge.
		s.socialRefuse(w, r, "account_exists")
		return
	case oauth.OutcomeNoAccount:
		s.socialRefuse(w, r, "no_account")
		return
	case oauth.OutcomeSignedIn:
	default:
		s.oauthInternalError(w, r, "complete social sign-in",
			errors.New("unexpected outcome "+string(result.Outcome)))
		return
	}

	if s.auth == nil || s.auth.Service == nil {
		writeProblem(w, http.StatusServiceUnavailable, "auth_not_configured", "authentication is not configured")
		return
	}
	// A second factor that a social sign-in skipped is not a second factor. An account with
	// MFA enrolled is told to sign in with its password instead, which is the only way the
	// code is actually checked.
	hasMFA, err := s.auth.Service.HasMFA(r.Context(), result.User.ID)
	if err != nil {
		s.oauthInternalError(w, r, "read mfa state", err)
		return
	}
	if hasMFA {
		s.socialRefuse(w, r, "mfa_required")
		return
	}

	session, err := s.auth.Service.LoginWithID(r.Context(), result.User.ID, s.clientIP(r), r.UserAgent())
	if err != nil {
		if errors.Is(err, authn.ErrAccountDisabled) {
			s.socialRefuse(w, r, "account_disabled")
			return
		}
		s.oauthInternalError(w, r, "social session", err)
		return
	}
	s.setSessionCookies(w, session.Token, session.Session)
	w.Header().Set("Cache-Control", "no-store")
	// A social sign-in lands on the account's own home, not on the storefront. `/` used to be
	// "the panel for staff and the portal for a customer", decided in the browser; it is the
	// shop now, so redirecting there would drop a freshly signed-in user on the product list with
	// no sign that they had signed in at all. `landingPath` holds the rule (and is where it is
	// tested), because this is the one redirect the client-side guard never sees.
	http.Redirect(w, r, landingPath(session.User), http.StatusFound)
}

// completeSocialLink binds a provider identity to the signed-in account.
func (s *Server) completeSocialLink(w http.ResponseWriter, r *http.Request, svc *oauth.Service,
	provider oauth.Provider, identity oauth.Identity, userID string) {
	_, err := svc.CompleteLink(r.Context(), provider, identity, userID)
	switch {
	case err == nil:
		http.Redirect(w, r, "/portal/security?linked="+url.QueryEscape(provider.Slug), http.StatusFound)
	case errors.Is(err, oauth.ErrEmailMismatch):
		s.socialRefuse(w, r, "email_mismatch")
	case errors.Is(err, oauth.ErrEmailUnverified):
		s.socialRefuse(w, r, "email_unverified")
	case errors.Is(err, oauth.ErrIdentityTaken):
		s.socialRefuse(w, r, "identity_taken")
	case errors.Is(err, oauth.ErrNotFound):
		s.socialRefuse(w, r, "account_missing")
	default:
		s.oauthInternalError(w, r, "link social identity", err)
	}
}

// socialRefuse sends the browser back to the sign-in screen with a stable reason code.
//
// A code rather than a sentence: the screen owns the wording, and the same code means the same
// thing on every provider. `account_exists` in particular has to be able to say "sign in with
// your password, then link this provider", which is a message only the screen can render well.
func (s *Server) socialRefuse(w http.ResponseWriter, r *http.Request, code string) {
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/login?social_error="+url.QueryEscape(code), http.StatusFound)
}

// socialProvider resolves the {slug} path parameter.
func (s *Server) socialProvider(w http.ResponseWriter, r *http.Request, svc *oauth.Service) (oauth.Provider, bool) {
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		writeProblem(w, http.StatusNotFound, "not_found", "no such provider")
		return oauth.Provider{}, false
	}
	provider, err := svc.ProviderBySlug(r.Context(), slug)
	if errors.Is(err, oauth.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such provider")
		return oauth.Provider{}, false
	}
	if err != nil {
		s.oauthInternalError(w, r, "read social provider", err)
		return oauth.Provider{}, false
	}
	if !provider.Enabled {
		writeProblem(w, http.StatusNotFound, "not_found", "no such provider")
		return oauth.Provider{}, false
	}
	return provider, true
}

// socialRedirectURI is the callback URL registered with the provider.
//
// It is built from configuration rather than from the request's Host header: a callback URL
// derived from Host is a callback URL an attacker chooses by sending a request with a forged
// header, and the provider's registered redirect URI would then match the attacker's.
func (s *Server) socialRedirectURI(provider oauth.Provider) string {
	return publicBaseURL() + "/oauth/social/" + url.PathEscape(provider.Slug) + "/callback"
}

// ------------------------------------------------------------ provider registry --

// providerJSON is the shape of a provider everywhere it is returned: never the secret.
func providerJSON(p oauth.Provider) map[string]any {
	return map[string]any{
		"id":                     p.ID,
		"slug":                   p.Slug,
		"name":                   p.Name,
		"issuer_url":             p.IssuerURL,
		"authorization_endpoint": p.AuthorizationEndpoint,
		"token_endpoint":         p.TokenEndpoint,
		"userinfo_endpoint":      p.UserinfoEndpoint,
		"client_id":              p.ClientID,
		"has_secret":             p.HasSecret(),
		"scopes":                 p.Scopes,
		"subject_claim":          p.SubjectClaim,
		"email_claim":            p.EmailClaim,
		"email_verified_claim":   p.EmailVerifiedClaim,
		"enabled":                p.Enabled,
		"created_at":             p.CreatedAt.UTC(),
		"updated_at":             p.UpdatedAt.UTC(),
	}
}

// providerRequest is the create/update body.
type providerRequest struct {
	Slug                  string   `json:"slug"`
	Name                  string   `json:"name"`
	IssuerURL             string   `json:"issuer_url"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	UserinfoEndpoint      string   `json:"userinfo_endpoint"`
	ClientID              string   `json:"client_id"`
	ClientSecret          string   `json:"client_secret"`
	Scopes                []string `json:"scopes"`
	SubjectClaim          string   `json:"subject_claim"`
	EmailClaim            string   `json:"email_claim"`
	EmailVerifiedClaim    string   `json:"email_verified_claim"`
	Enabled               bool     `json:"enabled"`
}

func (req providerRequest) newProvider() oauth.NewProvider {
	return oauth.NewProvider{
		Slug:                  req.Slug,
		Name:                  req.Name,
		IssuerURL:             req.IssuerURL,
		AuthorizationEndpoint: req.AuthorizationEndpoint,
		TokenEndpoint:         req.TokenEndpoint,
		UserinfoEndpoint:      req.UserinfoEndpoint,
		ClientID:              req.ClientID,
		ClientSecret:          req.ClientSecret,
		Scopes:                req.Scopes,
		SubjectClaim:          req.SubjectClaim,
		EmailClaim:            req.EmailClaim,
		EmailVerifiedClaim:    req.EmailVerifiedClaim,
		Enabled:               req.Enabled,
	}
}

func (s *Server) handleProviderList(w http.ResponseWriter, r *http.Request) {
	svc := s.oauthFor(r)
	if svc == nil {
		writeProblem(w, http.StatusServiceUnavailable, "oauth_not_configured",
			"social login is not configured")
		return
	}
	providers, err := svc.ListProviders(r.Context(), false)
	if err != nil {
		s.oauthInternalError(w, r, "list social providers", err)
		return
	}
	out := make([]map[string]any, 0, len(providers))
	for _, p := range providers {
		out = append(out, providerJSON(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) handleProviderGet(w http.ResponseWriter, r *http.Request) {
	svc := s.oauthFor(r)
	if svc == nil {
		writeProblem(w, http.StatusServiceUnavailable, "oauth_not_configured",
			"social login is not configured")
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such provider")
		return
	}
	provider, err := svc.ProviderByID(r.Context(), id)
	if errors.Is(err, oauth.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such provider")
		return
	}
	if err != nil {
		s.oauthInternalError(w, r, "read social provider", err)
		return
	}
	writeJSON(w, http.StatusOK, providerJSON(provider))
}

func (s *Server) handleProviderCreate(w http.ResponseWriter, r *http.Request) {
	svc := s.oauthFor(r)
	if svc == nil {
		writeProblem(w, http.StatusServiceUnavailable, "oauth_not_configured",
			"social login is not configured")
		return
	}
	var req providerRequest
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	provider, err := svc.CreateProvider(r.Context(), req.newProvider())
	if err != nil {
		s.providerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, providerJSON(provider))
}

func (s *Server) handleProviderUpdate(w http.ResponseWriter, r *http.Request) {
	svc := s.oauthFor(r)
	if svc == nil {
		writeProblem(w, http.StatusServiceUnavailable, "oauth_not_configured",
			"social login is not configured")
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such provider")
		return
	}
	var req providerRequest
	if !s.decodeJSON(w, r, &req, 16<<10) {
		return
	}
	provider, err := svc.UpdateProvider(r.Context(), id, req.newProvider())
	if err != nil {
		s.providerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, providerJSON(provider))
}

func (s *Server) handleProviderDelete(w http.ResponseWriter, r *http.Request) {
	svc := s.oauthFor(r)
	if svc == nil {
		writeProblem(w, http.StatusServiceUnavailable, "oauth_not_configured",
			"social login is not configured")
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such provider")
		return
	}
	err := svc.DeleteProvider(r.Context(), id)
	if errors.Is(err, oauth.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such provider")
		return
	}
	if err != nil {
		s.oauthInternalError(w, r, "delete social provider", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// providerError maps a provider failure to a stable problem code.
func (s *Server) providerError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, oauth.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "no such provider")
	case errors.Is(err, oauth.ErrInvalidInput):
		writeProblem(w, http.StatusBadRequest, "invalid_provider", err.Error())
	case errors.Is(err, oauth.ErrProviderExchange):
		// Discovery failed, which is a configuration problem the operator has to see.
		writeProblem(w, http.StatusBadGateway, "provider_unreachable", err.Error())
	default:
		s.oauthInternalError(w, r, "social provider", err)
	}
}

// ------------------------------------------------------------- customer links --

// handleSocialLinks lists the provider identities bound to the caller's account.
func (s *Server) handleSocialLinks(w http.ResponseWriter, r *http.Request) {
	svc := s.oauthService()
	if svc == nil {
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{}})
		return
	}
	user, _ := userFrom(r.Context())
	links, err := svc.LinksForUser(r.Context(), user.ID)
	if err != nil {
		s.oauthInternalError(w, r, "list social links", err)
		return
	}
	out := make([]map[string]any, 0, len(links))
	for _, link := range links {
		out = append(out, map[string]any{
			"id":            link.ID,
			"provider_slug": link.ProviderSlug,
			"provider_name": link.ProviderName,
			"email":         link.Email,
			"created_at":    link.CreatedAt.UTC(),
			// The subject is an opaque provider id. It is included so support can match a
			// report ("my Discord is not linked") to the row, and it reveals nothing.
			"subject": link.Subject,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// handleSocialUnlink removes one of the caller's links.
func (s *Server) handleSocialUnlink(w http.ResponseWriter, r *http.Request) {
	svc := s.oauthFor(r)
	if svc == nil {
		writeProblem(w, http.StatusServiceUnavailable, "oauth_not_configured",
			"social login is not configured")
		return
	}
	user, _ := userFrom(r.Context())
	id := r.PathValue("id")
	if !validUUID(id) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such link")
		return
	}
	err := svc.Unlink(r.Context(), user.ID, id)
	if errors.Is(err, oauth.ErrNotFound) {
		// Somebody else's link is indistinguishable from one that does not exist.
		writeProblem(w, http.StatusNotFound, "not_found", "no such link")
		return
	}
	if err != nil {
		s.oauthInternalError(w, r, "unlink social identity", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
