package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/httpapi"
	"github.com/catalyst/billing/internal/payments"
	"github.com/catalyst/billing/internal/portal"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
)

// The OAuth2 server and social login over HTTP.
//
// These are the acceptance tests, driven the way a third party drives them: a real session
// consenting, a real form-encoded token request, and a bearer token calling a documented
// endpoint. The interesting assertion is not that the token works — it is that a token minted
// before a permission was revoked stops working, because that is the difference between a
// scope list that is a copy and one that is a ceiling.

type oauthEnv struct {
	t      *testing.T
	pool   *store.Pool
	srv    *httpapi.Server
	authz  *authn.Authorizer
	hasher *authn.Hasher
	now    time.Time
	seq    atomic.Int64
}

func newOAuthEnv(t *testing.T) *oauthEnv {
	t.Helper()
	pool := storetest.DB(t)
	params := authn.DefaultArgon2Params()
	params.Memory = 19 * 1024
	params.Iterations = 1
	hasher, err := authn.NewHasher(params)
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	env := &oauthEnv{t: t, pool: pool, authz: authn.NewAuthorizer(), hasher: hasher,
		now: time.Now().UTC()}

	cfg := authn.DefaultConfig()
	cfg.Now = func() time.Time { return env.now }
	svc, err := authn.New(pool, hasher, cfg)
	if err != nil {
		t.Fatalf("authn.New: %v", err)
	}
	env.srv = httpapi.New(httpapi.Config{
		Version: "test",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, pool, payments.NewInbox(pool), "stripe", "acct_test", nil).
		WithAuth(&httpapi.Auth{
			Service:    svc,
			Authz:      env.authz,
			Cookies:    httpapi.CookieConfig{Secure: false},
			SessionTTL: cfg.SessionLifetime,
		}).
		WithPortal(portal.New(pool))
	return env
}

// createUser inserts an account with the given staff role.
func (e *oauthEnv) createUser(email, password, role string) string {
	e.t.Helper()
	hash, err := e.hasher.Hash(password)
	if err != nil {
		e.t.Fatalf("hash: %v", err)
	}
	var id string
	if err := e.pool.QueryRow(context.Background(), `
		INSERT INTO users (email, password_hash, display_name, email_verified_at)
		VALUES ($1, $2, 'User', now()) RETURNING id`, email, hash).Scan(&id); err != nil {
		e.t.Fatalf("create user: %v", err)
	}
	if role != "" {
		if _, err := e.pool.Exec(context.Background(),
			`INSERT INTO staff_roles (user_id, role) VALUES ($1, $2)`, id, role); err != nil {
			e.t.Fatalf("grant role: %v", err)
		}
	}
	return id
}

// login signs in and returns the session cookie and CSRF token.
func (e *oauthEnv) login(email, password string) (*http.Cookie, string) {
	e.t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("login: status %d, body %s", rec.Code, rec.Body)
	}
	var resp struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	for _, c := range rec.Result().Cookies() {
		if c.Name == "billing_session" {
			return c, resp.CSRFToken
		}
	}
	e.t.Fatal("no session cookie was set")
	return nil, ""
}

func (e *oauthEnv) get(path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// postJSON sends a mutation the way the panel's own client does.
func (e *oauthEnv) postJSON(method, path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", fmt.Sprintf("test-key-%06d", e.seq.Add(1)))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// postForm sends an RFC 6749 form-encoded request, with a session cookie when one is given.
func (e *oauthEnv) postForm(path string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// registerClient creates a client through the staff API and returns its id, public id and secret.
func (e *oauthEnv) registerClient(cookie *http.Cookie, csrf, name string, confidential bool, scopes []string) (id, clientID, secret string) {
	e.t.Helper()
	body, _ := json.Marshal(map[string]any{
		"name":           name,
		"redirect_uris":  []string{"https://app.example.test/callback"},
		"allowed_scopes": scopes,
		"confidential":   confidential,
	})
	rec := e.postJSON(http.MethodPost, "/api/v1/staff/oauth-clients", string(body), cookie, csrf)
	if rec.Code != http.StatusCreated {
		e.t.Fatalf("create client: status %d, body %s", rec.Code, rec.Body)
	}
	var created struct {
		ID       string `json:"id"`
		ClientID string `json:"client_id"`
		Secret   string `json:"secret"`
		ShownOne bool   `json:"shown_once"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		e.t.Fatalf("create body: %v", err)
	}
	return created.ID, created.ClientID, created.Secret
}

// ------------------------------------------------------------------- tests --

// The acceptance test: a third party obtains a token with a scope, calls the documented API,
// and the token narrows the moment the permission behind it is revoked.
func TestOAuthTokenNarrowsWhenTheRoleLosesThePermission(t *testing.T) {
	env := newOAuthEnv(t)
	// A custom role, so a permission can be taken away without a deploy. The four system roles
	// are compiled in; a table role is the case where "revoke a permission from the role" is
	// something an operator actually does.
	// The role has to exist as a row too: staff_roles references it, and the whole point of a
	// table role is that it is data an operator can change.
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO roles (name, description) VALUES ('analyst', 'Read-only reporting')
		ON CONFLICT (name) DO NOTHING`); err != nil {
		t.Fatalf("create role: %v", err)
	}
	env.authz.SetTableRoles(map[authn.Role][]authn.Permission{
		"analyst": {authn.PermReportRead},
	})
	env.createUser("analyst@example.test", "correct horse battery", "analyst")
	env.createUser("admin@example.test", "correct horse battery", "administrator")

	adminCookie, adminCSRF := env.login("admin@example.test", "correct horse battery")
	clientID, publicID, secret := env.registerClient(adminCookie, adminCSRF, "Reporting App", true,
		[]string{"report:read"})
	if clientID == "" || secret == "" {
		t.Fatal("the client was created without an id or a secret")
	}

	userCookie, userCSRF := env.login("analyst@example.test", "correct horse battery")

	// 1. The analyst authorizes the client. The first authorization shows a consent screen.
	authorizeURL := "/oauth/authorize?" + url.Values{
		"response_type": {"code"},
		"client_id":     {publicID},
		"redirect_uri":  {"https://app.example.test/callback"},
		"scope":         {"report:read"},
		"state":         {"xyz"},
	}.Encode()
	rec := env.get(authorizeURL, userCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("consent screen: status %d, body %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("consent screen content-type = %q, want HTML", ct)
	}
	if !strings.Contains(rec.Body.String(), "Reporting App") {
		t.Fatalf("the consent screen does not name the client: %s", rec.Body)
	}

	// 2. Approving it redirects back with a code.
	decision := url.Values{
		"response_type": {"code"},
		"client_id":     {publicID},
		"redirect_uri":  {"https://app.example.test/callback"},
		"scope":         {"report:read"},
		"state":         {"xyz"},
		"decision":      {"approve"},
		"csrf_token":    {userCSRF},
	}
	rec = env.postForm("/oauth/authorize", decision, userCookie)
	if rec.Code != http.StatusFound {
		t.Fatalf("consent decision: status %d, body %s", rec.Code, rec.Body)
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("location: %v", err)
	}
	if location.Host != "app.example.test" {
		t.Fatalf("redirected to %q, want the registered redirect URI", location.String())
	}
	code := location.Query().Get("code")
	if code == "" || location.Query().Get("state") != "xyz" {
		t.Fatalf("redirect = %q, want a code and the state back", location.String())
	}

	// 3. The client exchanges it at the token endpoint.
	rec = env.postForm("/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"https://app.example.test/callback"},
		"client_id":     {publicID},
		"client_secret": {secret},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("token: status %d, body %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("token response is cacheable: %q", rec.Header().Get("Cache-Control"))
	}
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &token); err != nil {
		t.Fatalf("token body: %v", err)
	}
	if token.AccessToken == "" || token.RefreshToken == "" || token.TokenType != "Bearer" {
		t.Fatalf("token = %+v, want a bearer pair", token)
	}
	if token.Scope != "report:read" {
		t.Fatalf("scope = %q, want report:read", token.Scope)
	}

	// 4. The token reaches a documented endpoint that needs that permission.
	bearer := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token.AccessToken)
		rec := httptest.NewRecorder()
		env.srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := bearer("/api/v1/ops/overview"); rec.Code != http.StatusOK {
		t.Fatalf("overview with token: status %d, body %s", rec.Code, rec.Body)
	}

	// /api/v1/me says who the token is for and what it carries.
	rec = bearer("/api/v1/me")
	if rec.Code != http.StatusOK {
		t.Fatalf("me: status %d, body %s", rec.Code, rec.Body)
	}
	var me struct {
		User struct {
			Email string `json:"email"`
		} `json:"user"`
		Auth struct {
			Kind     string   `json:"kind"`
			ClientID string   `json:"client_id"`
			Scopes   []string `json:"scopes"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatalf("me body: %v", err)
	}
	if me.Auth.Kind != "oauth" || me.Auth.ClientID != publicID {
		t.Fatalf("auth = %+v, want an oauth token for %s", me.Auth, publicID)
	}
	if me.User.Email != "analyst@example.test" {
		t.Fatalf("me user = %q, want the token owner", me.User.Email)
	}

	// 5. The permission is revoked from the role. The *same* token must stop working: the
	// check is against the owner's permissions as they are now, not a copy taken at issue time.
	env.authz.SetTableRoles(map[authn.Role][]authn.Permission{"analyst": {}})
	if rec := bearer("/api/v1/ops/overview"); rec.Code != http.StatusForbidden {
		t.Fatalf("overview after revocation: status %d, want 403; body %s", rec.Code, rec.Body)
	}
	// The token still authenticates — it is a valid credential — but it can do less.
	if rec := bearer("/api/v1/me"); rec.Code != http.StatusOK {
		t.Fatalf("me after revocation: status %d, want 200; body %s", rec.Code, rec.Body)
	}
}

// A client secret is in the create response and in no other response body.
func TestOAuthClientSecretIsShownOnce(t *testing.T) {
	env := newOAuthEnv(t)
	env.createUser("admin@example.test", "correct horse battery", "administrator")
	cookie, csrf := env.login("admin@example.test", "correct horse battery")

	body, _ := json.Marshal(map[string]any{
		"name":           "Shown Once",
		"redirect_uris":  []string{"https://app.example.test/callback"},
		"allowed_scopes": []string{"invoice:read:self"},
		"confidential":   true,
	})
	rec := env.postJSON(http.MethodPost, "/api/v1/staff/oauth-clients", string(body), cookie, csrf)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", rec.Code, rec.Body)
	}
	var created struct {
		ID        string `json:"id"`
		Secret    string `json:"secret"`
		ShownOne  bool   `json:"shown_once"`
		HasSecret bool   `json:"has_secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create body: %v", err)
	}
	if !strings.HasPrefix(created.Secret, "ocs_") || !created.ShownOne || !created.HasSecret {
		t.Fatalf("create = %+v, want an ocs_ secret marked shown_once", created)
	}

	// Every other read path: no secret anywhere in the body.
	for _, path := range []string{"/api/v1/staff/oauth-clients", "/api/v1/staff/oauth-clients/" + created.ID} {
		rec := env.get(path, cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), created.Secret) {
			t.Fatalf("%s returned the secret: %s", path, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), `"has_secret":true`) {
			t.Fatalf("%s does not report that a secret is configured: %s", path, rec.Body)
		}
	}

	// Rotation is the only way to get another one, and it is shown once too.
	rec = env.postJSON(http.MethodPost,
		"/api/v1/staff/oauth-clients/"+created.ID+"/rotate-secret", "", cookie, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: status %d, body %s", rec.Code, rec.Body)
	}
	var rotated struct {
		Secret   string `json:"secret"`
		ShownOne bool   `json:"shown_once"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rotated); err != nil {
		t.Fatalf("rotate body: %v", err)
	}
	if rotated.Secret == "" || rotated.Secret == created.Secret || !rotated.ShownOne {
		t.Fatalf("rotate = %+v, want a different shown-once secret", rotated)
	}
}

// The token endpoint answers a problem document that also carries RFC 6749's error member.
func TestOAuthTokenErrorsAreProblemJSONWithAnOAuthCode(t *testing.T) {
	env := newOAuthEnv(t)
	env.createUser("admin@example.test", "correct horse battery", "administrator")
	cookie, csrf := env.login("admin@example.test", "correct horse battery")
	_, publicID, _ := env.registerClient(cookie, csrf, "Errors", true, []string{"invoice:read:self"})

	rec := env.postForm("/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"ac_nope"},
		"client_id":     {publicID},
		"client_secret": {"ocs_wrong"},
	}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad secret: status %d, want 401; body %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/problem+json") {
		t.Fatalf("content-type = %q, want application/problem+json", ct)
	}
	var problem struct {
		Code   string `json:"code"`
		Error  string `json:"error"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("problem body: %v", err)
	}
	if problem.Code != "invalid_client" || problem.Error != "invalid_client" || problem.Status != 401 {
		t.Fatalf("problem = %+v, want invalid_client under both members", problem)
	}

	// An unknown grant type is a 400 with the RFC code.
	rec = env.postForm("/oauth/token", url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {publicID},
		"client_secret": {"ocs_wrong"},
	}, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unsupported_grant_type") {
		t.Fatalf("unknown grant: status %d, body %s", rec.Code, rec.Body)
	}
}

// The authorization endpoint sends a browser to sign in rather than answering JSON.
func TestOAuthAuthorizeRedirectsToSignIn(t *testing.T) {
	env := newOAuthEnv(t)
	env.createUser("admin@example.test", "correct horse battery", "administrator")
	cookie, csrf := env.login("admin@example.test", "correct horse battery")
	_, publicID, _ := env.registerClient(cookie, csrf, "Redirects", true, []string{"invoice:read:self"})

	rec := env.get("/oauth/authorize?response_type=code&client_id="+publicID+
		"&redirect_uri="+url.QueryEscape("https://app.example.test/callback")+
		"&scope=invoice%3Aread%3Aself", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("anonymous authorize: status %d, want 302", rec.Code)
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "/login?next=") {
		t.Fatalf("location = %q, want a sign-in redirect", location)
	}
	if !strings.Contains(location, "client_id") {
		t.Fatalf("location = %q, want the original request preserved as next", location)
	}
}

// An unregistered redirect URI is a problem document, never a redirect.
func TestOAuthAuthorizeRefusesAnUnregisteredRedirect(t *testing.T) {
	env := newOAuthEnv(t)
	env.createUser("admin@example.test", "correct horse battery", "administrator")
	cookie, csrf := env.login("admin@example.test", "correct horse battery")
	_, publicID, _ := env.registerClient(cookie, csrf, "Strict", true, []string{"invoice:read:self"})

	rec := env.get("/oauth/authorize?response_type=code&client_id="+publicID+
		"&redirect_uri="+url.QueryEscape("https://attacker.example.test/callback")+
		"&scope=invoice%3Aread%3Aself", cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unregistered redirect: status %d, want 400; body %s", rec.Code, rec.Body)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("redirected to %q, want no redirect", loc)
	}
	if !strings.Contains(rec.Body.String(), "invalid_redirect") {
		t.Fatalf("body = %s, want invalid_redirect", rec.Body)
	}
}

// The sign-in screen's provider list: empty with none configured, one entry when enabled.
func TestAuthProvidersListForTheSignInScreen(t *testing.T) {
	env := newOAuthEnv(t)

	rec := env.get("/api/v1/auth/providers", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("providers: status %d", rec.Code)
	}
	var empty struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil {
		t.Fatalf("providers body: %v", err)
	}
	if len(empty.Data) != 0 {
		t.Fatalf("providers = %v, want none configured", empty.Data)
	}

	env.createUser("admin@example.test", "correct horse battery", "administrator")
	cookie, csrf := env.login("admin@example.test", "correct horse battery")
	provider := `{"slug":"google","name":"Google","issuer_url":"https://accounts.example.test",
		"authorization_endpoint":"https://accounts.example.test/authorize",
		"token_endpoint":"https://accounts.example.test/token",
		"userinfo_endpoint":"https://accounts.example.test/userinfo",
		"client_id":"id","client_secret":"secret","enabled":true}`
	rec = env.postJSON(http.MethodPost, "/api/v1/staff/oauth-providers", provider, cookie, csrf)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create provider: status %d, body %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "secret") && strings.Contains(rec.Body.String(), `"client_secret"`) {
		t.Fatalf("the provider response carries the secret: %s", rec.Body)
	}

	rec = env.get("/api/v1/auth/providers", nil)
	var listed struct {
		Data []struct {
			Slug     string `json:"slug"`
			Name     string `json:"name"`
			StartURL string `json:"start_url"`
			LinkURL  string `json:"link_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("providers body: %v", err)
	}
	if len(listed.Data) != 1 || listed.Data[0].Slug != "google" {
		t.Fatalf("providers = %+v, want the enabled google provider", listed.Data)
	}
	if listed.Data[0].StartURL != "/oauth/social/google/start" {
		t.Fatalf("start_url = %q", listed.Data[0].StartURL)
	}

	// A disabled provider is not offered at all.
	disabled := `{"slug":"github","name":"GitHub","issuer_url":"https://github.example.test",
		"authorization_endpoint":"https://github.example.test/authorize",
		"token_endpoint":"https://github.example.test/token",
		"userinfo_endpoint":"https://github.example.test/userinfo",
		"client_id":"id","client_secret":"secret","enabled":false}`
	rec = env.postJSON(http.MethodPost, "/api/v1/staff/oauth-providers", disabled, cookie, csrf)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create disabled provider: status %d, body %s", rec.Code, rec.Body)
	}
	rec = env.get("/api/v1/auth/providers", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &listed)
	if len(listed.Data) != 1 {
		t.Fatalf("providers = %+v, want only the enabled one", listed.Data)
	}
}

// A social sign-in for an email that already has an account refuses, and sets no session.
func TestSocialSignInWithExistingEmailRefusesOverHTTP(t *testing.T) {
	env := newOAuthEnv(t)
	env.createUser("taken@example.test", "correct horse battery", "")
	env.createUser("admin@example.test", "correct horse battery", "administrator")

	// A provider that asserts the existing account's address.
	var providerURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"provider-token","token_type":"Bearer"}`)
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"sub":"subject-1","email":"taken@example.test","email_verified":true}`)
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	providerURL = server.URL

	cookie, csrf := env.login("admin@example.test", "correct horse battery")
	body := fmt.Sprintf(`{"slug":"google","name":"Google","issuer_url":%q,
		"authorization_endpoint":%q,"token_endpoint":%q,"userinfo_endpoint":%q,
		"client_id":"id","client_secret":"secret","scopes":["openid","email"],"enabled":true}`,
		providerURL, providerURL+"/authorize", providerURL+"/token", providerURL+"/userinfo")
	rec := env.postJSON(http.MethodPost, "/api/v1/staff/oauth-providers", body, cookie, csrf)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create provider: status %d, body %s", rec.Code, rec.Body)
	}

	// The start endpoint redirects to the provider with a signed state.
	rec = env.get("/oauth/social/google/start", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("start: status %d, body %s", rec.Code, rec.Body)
	}
	start, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("start location: %v", err)
	}
	state := start.Query().Get("state")
	if state == "" || start.Query().Get("code_challenge") != "" {
		t.Fatalf("start location = %q, want a state", start.String())
	}
	if start.Query().Get("redirect_uri") == "" {
		t.Fatalf("start location = %q, want a redirect_uri", start.String())
	}

	// The callback refuses: an account with that email exists and is not linked.
	rec = env.get("/oauth/social/google/callback?code=provider-code&state="+url.QueryEscape(state), nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback: status %d, body %s", rec.Code, rec.Body)
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "/login?social_error=account_exists") {
		t.Fatalf("callback location = %q, want social_error=account_exists", location)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "billing_session" && c.Value != "" {
			t.Fatal("a session was issued for a refused sign-in")
		}
	}

	// And nothing was written: no link.
	var links int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM social_identities`).Scan(&links); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if links != 0 {
		t.Fatalf("links = %d, want 0", links)
	}

	// The same state cannot be replayed.
	rec = env.get("/oauth/social/google/callback?code=provider-code&state="+url.QueryEscape(state), nil)
	if location := rec.Header().Get("Location"); !strings.Contains(location, "state_invalid") {
		t.Fatalf("replayed state location = %q, want state_invalid", location)
	}
}

// The documented read a third party actually makes: a token with invoice:read:self returns the
// owner's invoices and nothing else. The customer is resolved from the credential, so another
// customer's invoice is not addressable — there is no id in the request to substitute.
func TestOAuthTokenReadsOnlyItsOwnersInvoices(t *testing.T) {
	env := newOAuthEnv(t)
	ownerID := env.createUser("owner@example.test", "correct horse battery", "")
	otherID := env.createUser("other@example.test", "correct horse battery", "")

	// Two billing customers, one invoice each, linked to the two logins.
	seed := func(userID, email, number string) {
		var customerID string
		if err := env.pool.QueryRow(context.Background(), `
			INSERT INTO customers (email, name) VALUES ($1, 'Customer') RETURNING id`, email).
			Scan(&customerID); err != nil {
			t.Fatalf("customer: %v", err)
		}
		if _, err := env.pool.Exec(context.Background(), `
			INSERT INTO customer_accounts (user_id, customer_id) VALUES ($1, $2)`,
			userID, customerID); err != nil {
			t.Fatalf("link: %v", err)
		}
		if _, err := env.pool.Exec(context.Background(), `
			INSERT INTO invoices (number, customer_id, status, currency, subtotal_minor,
				tax_minor, total_minor, amount_paid_minor, issued_at, due_at, snapshot)
			VALUES ($1, $2, 'open', 'USD', 1200, 0, 1200, 0, now(), now() + interval '7 days',
			        '{"schema_version":1}'::jsonb)`, number, customerID); err != nil {
			t.Fatalf("invoice: %v", err)
		}
	}
	seed(ownerID, "owner@example.test", "INV-OWNER")
	seed(otherID, "other@example.test", "INV-OTHER")

	env.createUser("admin@example.test", "correct horse battery", "administrator")
	adminCookie, adminCSRF := env.login("admin@example.test", "correct horse battery")
	_, publicID, secret := env.registerClient(adminCookie, adminCSRF, "Portal App", true,
		[]string{"invoice:read:self"})

	ownerCookie, ownerCSRF := env.login("owner@example.test", "correct horse battery")
	rec := env.postForm("/oauth/authorize", url.Values{
		"response_type": {"code"},
		"client_id":     {publicID},
		"redirect_uri":  {"https://app.example.test/callback"},
		"scope":         {"invoice:read:self"},
		"decision":      {"approve"},
		"csrf_token":    {ownerCSRF},
	}, ownerCookie)
	if rec.Code != http.StatusFound {
		t.Fatalf("consent: status %d, body %s", rec.Code, rec.Body)
	}
	location, _ := url.Parse(rec.Header().Get("Location"))
	code := location.Query().Get("code")

	rec = env.postForm("/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"https://app.example.test/callback"},
		"client_id":     {publicID},
		"client_secret": {secret},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("token: status %d, body %s", rec.Code, rec.Body)
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &token)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/invoices", nil)
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	rec = httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("invoices: status %d, body %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "INV-OWNER") {
		t.Fatalf("the owner's invoice is missing: %s", body)
	}
	if strings.Contains(body, "INV-OTHER") {
		t.Fatalf("another customer's invoice was returned: %s", body)
	}
}
