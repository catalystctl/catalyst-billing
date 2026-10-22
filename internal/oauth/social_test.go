package oauth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/catalyst/billing/internal/oauth"
)

// Social login, against a real database and a real (local) provider.
//
// The headline test is the one that matters: a provider asserting an email that already has an
// account must not produce a session, a link or a merged account. Everything else here exists
// to make that refusal trustworthy — the state is signed and single-use, and a link requires
// the provider's email to be the account's own.

// fakeProvider is an httptest server that speaks the two endpoints a provider needs.
type fakeProvider struct {
	server *httptest.Server
	// claims is what the userinfo endpoint returns.
	claims map[string]any
	// tokenStatus lets a test make the token endpoint fail.
	tokenStatus int
}

func newFakeProvider(t *testing.T, claims map[string]any) *fakeProvider {
	t.Helper()
	f := &fakeProvider{claims: claims, tokenStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if f.tokenStatus != http.StatusOK {
			w.WriteHeader(f.tokenStatus)
			return
		}
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// The exchange must present the code and the client credentials the row holds.
		if r.Form.Get("code") == "" || r.Form.Get("client_secret") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "provider-access-token",
			"token_type":   "Bearer",
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer provider-access-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.claims)
	})
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authorization_endpoint": f.server.URL + "/authorize",
			"token_endpoint":         f.server.URL + "/token",
			"userinfo_endpoint":      f.server.URL + "/userinfo",
		})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// newProvider registers a provider row pointing at the fake server.
func (e *env) newProvider(f *fakeProvider, slug string) oauth.Provider {
	e.t.Helper()
	provider, err := e.svc.CreateProvider(context.Background(), oauth.NewProvider{
		Slug:                  slug,
		Name:                  "Test Provider",
		IssuerURL:             f.server.URL,
		AuthorizationEndpoint: f.server.URL + "/authorize",
		TokenEndpoint:         f.server.URL + "/token",
		UserinfoEndpoint:      f.server.URL + "/userinfo",
		ClientID:              "client-abc",
		ClientSecret:          "provider-secret",
		Scopes:                []string{"openid", "email"},
		Enabled:               true,
	})
	if err != nil {
		e.t.Fatalf("create provider: %v", err)
	}
	return provider
}

// socialIdentity runs the state and the provider exchange, returning the identity.
func (e *env) socialIdentity(provider oauth.Provider, mode, userID string) oauth.Identity {
	e.t.Helper()
	state, err := e.svc.SignState(context.Background(), provider, mode, userID)
	if err != nil {
		e.t.Fatalf("sign state: %v", err)
	}
	claims, err := e.svc.ConsumeState(context.Background(), provider, state)
	if err != nil {
		e.t.Fatalf("consume state: %v", err)
	}
	if claims.Mode != mode || claims.ProviderID != provider.ID {
		e.t.Fatalf("state claims = %+v, want mode %q for provider %s", claims, mode, provider.ID)
	}
	identity, err := e.svc.ExchangeIdentity(context.Background(), provider, "provider-code",
		"https://panel.example.test/oauth/social/"+provider.Slug+"/callback")
	if err != nil {
		e.t.Fatalf("exchange identity: %v", err)
	}
	return identity
}

// The one decision that prevents an account takeover: an existing email is never merged.
func TestSocialSignInWithExistingEmailDoesNotMerge(t *testing.T) {
	e := newEnv(t)
	user := e.insertUser("taken@example.test")
	provider := e.newProvider(newFakeProvider(t, map[string]any{
		"sub":            "subject-123",
		"email":          "taken@example.test",
		"email_verified": true,
	}), "google")

	identity := e.socialIdentity(provider, oauth.StateSignIn, "")
	result, err := e.svc.CompleteSignIn(context.Background(), provider, identity)
	if err != nil {
		t.Fatalf("complete sign-in: %v", err)
	}
	if result.Outcome != oauth.OutcomeAccountExists {
		t.Fatalf("outcome = %q, want account_exists", result.Outcome)
	}
	// It names the existing account so the message can say "sign in and link", but it does
	// not sign it in: there is no session to issue from an outcome.
	if result.User.ID != user.ID {
		t.Fatalf("result user = %q, want the existing account %q", result.User.ID, user.ID)
	}

	// Nothing was written: no link, no new account.
	var links int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM social_identities`).Scan(&links); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if links != 0 {
		t.Fatalf("links = %d, want 0: a sign-in must not create a link", links)
	}
	var users int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM users WHERE lower(email) = lower($1)`, "taken@example.test").
		Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 1 {
		t.Fatalf("users with that email = %d, want 1", users)
	}
}

// An unknown email is not an invitation to create an account either: registration is its own
// flow with its own rules, and this one only signs people in.
func TestSocialSignInWithUnknownEmailCreatesNothing(t *testing.T) {
	e := newEnv(t)
	provider := e.newProvider(newFakeProvider(t, map[string]any{
		"sub":   "subject-456",
		"email": "stranger@example.test",
	}), "github")

	identity := e.socialIdentity(provider, oauth.StateSignIn, "")
	result, err := e.svc.CompleteSignIn(context.Background(), provider, identity)
	if err != nil {
		t.Fatalf("complete sign-in: %v", err)
	}
	if result.Outcome != oauth.OutcomeNoAccount {
		t.Fatalf("outcome = %q, want no_account", result.Outcome)
	}
	var users int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM users`).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 0 {
		t.Fatalf("users = %d, want 0: a social sign-in must not create an account", users)
	}
}

// Linking is the only way a link is created, and it requires the provider's email to be the
// account's own.
func TestSocialLinkRequiresTheAccountsEmail(t *testing.T) {
	e := newEnv(t)
	user := e.insertUser("mine@example.test")
	provider := e.newProvider(newFakeProvider(t, map[string]any{
		"sub":            "subject-789",
		"email":          "someone-else@example.test",
		"email_verified": true,
	}), "discord")

	identity := e.socialIdentity(provider, oauth.StateLink, user.ID)
	if _, err := e.svc.CompleteLink(context.Background(), provider, identity, user.ID); !errors.Is(err, oauth.ErrEmailMismatch) {
		t.Fatalf("link with a different email = %v, want ErrEmailMismatch", err)
	}

	// The same provider identity, now asserting the account's own address, links and then
	// signs in.
	provider2 := e.newProvider(newFakeProvider(t, map[string]any{
		"sub":            "subject-789",
		"email":          "mine@example.test",
		"email_verified": true,
	}), "discord2")
	identity2 := e.socialIdentity(provider2, oauth.StateLink, user.ID)
	result, err := e.svc.CompleteLink(context.Background(), provider2, identity2, user.ID)
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if result.Outcome != oauth.OutcomeLinked {
		t.Fatalf("outcome = %q, want linked", result.Outcome)
	}

	signIn := e.socialIdentity(provider2, oauth.StateSignIn, "")
	signedIn, err := e.svc.CompleteSignIn(context.Background(), provider2, signIn)
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	if signedIn.Outcome != oauth.OutcomeSignedIn || signedIn.User.ID != user.ID {
		t.Fatalf("sign in = %+v, want the linked account %s", signedIn, user.ID)
	}
}

// A provider that did not confirm the email cannot be used to link, even if the address looks
// right: the whole point of the check is that somebody else cannot assert it.
func TestSocialLinkRefusesAnUnverifiedProviderEmail(t *testing.T) {
	e := newEnv(t)
	user := e.insertUser("verify@example.test")
	provider, err := e.svc.CreateProvider(context.Background(), oauth.NewProvider{
		Slug:                  "strict",
		Name:                  "Strict Provider",
		IssuerURL:             "https://strict.example.test",
		AuthorizationEndpoint: "https://strict.example.test/authorize",
		TokenEndpoint:         "https://strict.example.test/token",
		UserinfoEndpoint:      "https://strict.example.test/userinfo",
		ClientID:              "id",
		ClientSecret:          "secret",
		Scopes:                []string{"openid"},
		EmailVerifiedClaim:    "email_verified",
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	identity := oauth.Identity{Subject: "s-1", Email: "verify@example.test", EmailVerified: false}
	if _, err := e.svc.CompleteLink(context.Background(), provider, identity, user.ID); !errors.Is(err, oauth.ErrEmailUnverified) {
		t.Fatalf("unverified link = %v, want ErrEmailUnverified", err)
	}
}

// A state is signed and single-use: a replay, a tamper and another provider's state are all
// refused.
func TestSocialStateIsSignedAndSingleUse(t *testing.T) {
	e := newEnv(t)
	f := newFakeProvider(t, map[string]any{"sub": "s", "email": "a@example.test"})
	provider := e.newProvider(f, "stateful")
	other := e.newProvider(f, "other")

	state, err := e.svc.SignState(context.Background(), provider, oauth.StateSignIn, "")
	if err != nil {
		t.Fatalf("sign state: %v", err)
	}

	// A tampered payload does not verify.
	parts := strings.Split(state, ".")
	tampered := parts[0] + "." + parts[1][:len(parts[1])-2] + "AA." + parts[2]
	if _, err := e.svc.ConsumeState(context.Background(), provider, tampered); !errors.Is(err, oauth.ErrInvalidState) {
		t.Fatalf("tampered state = %v, want ErrInvalidState", err)
	}

	// Another provider cannot consume it.
	if _, err := e.svc.ConsumeState(context.Background(), other, state); !errors.Is(err, oauth.ErrInvalidState) {
		t.Fatalf("cross-provider state = %v, want ErrInvalidState", err)
	}

	// The real one is accepted once.
	if _, err := e.svc.ConsumeState(context.Background(), provider, state); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if _, err := e.svc.ConsumeState(context.Background(), provider, state); !errors.Is(err, oauth.ErrInvalidState) {
		t.Fatalf("replayed state = %v, want ErrInvalidState", err)
	}
}

// A provider whose issuer publishes discovery needs only the issuer URL.
func TestProviderDiscoveryFillsTheEndpoints(t *testing.T) {
	e := newEnv(t)
	f := newFakeProvider(t, map[string]any{"sub": "s"})

	provider, err := e.svc.CreateProvider(context.Background(), oauth.NewProvider{
		Slug:         "discovered",
		Name:         "Discovered",
		IssuerURL:    f.server.URL,
		ClientID:     "id",
		ClientSecret: "secret",
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if provider.AuthorizationEndpoint != f.server.URL+"/authorize" ||
		provider.TokenEndpoint != f.server.URL+"/token" ||
		provider.UserinfoEndpoint != f.server.URL+"/userinfo" {
		t.Fatalf("discovered provider = %+v, want the endpoints from the discovery document", provider)
	}
	if provider.SubjectClaim != "sub" || provider.EmailClaim != "email" {
		t.Fatalf("claim mapping = %q/%q, want the defaults", provider.SubjectClaim, provider.EmailClaim)
	}
}

// The provider's secret is never part of what a read path hands out.
func TestProviderSecretIsNotReadable(t *testing.T) {
	e := newEnv(t)
	f := newFakeProvider(t, map[string]any{"sub": "s"})
	provider := e.newProvider(f, "secretive")

	providers, err := e.svc.ListProviders(context.Background(), false)
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	for _, p := range providers {
		public := p.Public()
		if public.ClientSecret != "" {
			t.Fatal("the public view of a provider carries its secret")
		}
	}
	// The internal view keeps it, because the token exchange needs it.
	if provider.ClientSecret != "provider-secret" {
		t.Fatalf("provider secret = %q, want the configured value", provider.ClientSecret)
	}
}

// A provider that rejects the exchange is an error, not a half-completed sign-in.
func TestSocialExchangeFailureIsReported(t *testing.T) {
	e := newEnv(t)
	f := newFakeProvider(t, map[string]any{"sub": "s"})
	f.tokenStatus = http.StatusUnauthorized
	provider := e.newProvider(f, "broken")

	_, err := e.svc.ExchangeIdentity(context.Background(), provider, "code", "https://panel.example.test/cb")
	if !errors.Is(err, oauth.ErrProviderExchange) {
		t.Fatalf("failed exchange = %v, want ErrProviderExchange", err)
	}
}

// A userinfo document without the configured subject claim is a configuration error, not an
// identity with an empty subject.
func TestSocialExchangeRequiresTheSubjectClaim(t *testing.T) {
	e := newEnv(t)
	provider := e.newProvider(newFakeProvider(t, map[string]any{
		"email": "no-subject@example.test",
	}), "subjectless")

	_, err := e.svc.ExchangeIdentity(context.Background(), provider, "code", "https://panel.example.test/cb")
	if !errors.Is(err, oauth.ErrProviderExchange) {
		t.Fatalf("missing subject = %v, want ErrProviderExchange", err)
	}
}
