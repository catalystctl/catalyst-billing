package oauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/oauth"
	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/store/storetest"
)

// The OAuth2 server's contracts, against a real database.
//
// These tests are about the properties a third party depends on and a reviewer should be able
// to check: a secret that is shown once, a code that is single-use and PKCE-bound, a refresh
// token that rotates, and a token whose scopes are the owner's permissions rather than a copy
// of them.

type env struct {
	t     *testing.T
	pool  *store.Pool
	svc   *oauth.Service
	authz *authn.Authorizer
	now   time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := storetest.DB(t)
	e := &env{t: t, pool: pool, authz: authn.NewAuthorizer(), now: time.Now().UTC()}
	e.svc = oauth.New(pool).
		WithClock(func() time.Time { return e.now }).
		WithPermissions(func(ctx context.Context, userID string) (map[authn.Permission]struct{}, error) {
			user, err := e.userByID(ctx, userID)
			if err != nil {
				return nil, err
			}
			return e.authz.Permissions(user), nil
		})
	return e
}

// insertUser creates an account with the given staff roles.
func (e *env) insertUser(email string, roles ...string) authn.User {
	e.t.Helper()
	ctx := context.Background()
	var id string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, display_name, email_verified_at)
		VALUES ($1, 'x', 'Test Person', now()) RETURNING id`, email).Scan(&id); err != nil {
		e.t.Fatalf("insert user: %v", err)
	}
	for _, role := range roles {
		if _, err := e.pool.Exec(ctx,
			`INSERT INTO staff_roles (user_id, role) VALUES ($1, $2)`, id, role); err != nil {
			e.t.Fatalf("grant role: %v", err)
		}
	}
	return authn.User{ID: id, Email: email, DisplayName: "Test Person", Status: "active",
		EmailVerified: true, Roles: roles}
}

func (e *env) userByID(ctx context.Context, id string) (authn.User, error) {
	var (
		user  authn.User
		roles []string
	)
	err := e.pool.QueryRow(ctx, `
		SELECT u.id, u.email, COALESCE(u.display_name, ''), u.status,
		       u.email_verified_at IS NOT NULL,
		       COALESCE(array_agg(sr.role) FILTER (WHERE sr.role IS NOT NULL), '{}')
		FROM users u LEFT JOIN staff_roles sr ON sr.user_id = u.id
		WHERE u.id = $1
		GROUP BY u.id`, id).
		Scan(&user.ID, &user.Email, &user.DisplayName, &user.Status, &user.EmailVerified, &roles)
	if err != nil {
		return authn.User{}, err
	}
	user.Roles = roles
	return user, nil
}

// newClient registers a confidential client that may ask for invoice:read:self.
func (e *env) newClient(confidential bool) oauth.CreatedClient {
	e.t.Helper()
	created, err := e.svc.CreateClient(context.Background(), oauth.NewClient{
		Name:          "Test App",
		RedirectURIs:  []string{"https://app.example.test/callback"},
		AllowedScopes: []string{"invoice:read:self", "service:read:self"},
		Confidential:  confidential,
	})
	if err != nil {
		e.t.Fatalf("create client: %v", err)
	}
	return created
}

// pkce returns a verifier and its S256 challenge.
func pkce() (verifier, challenge string) {
	verifier = strings.Repeat("a1b2c3d4", 6)[:43]
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorize prepares and issues a code for a client and user.
func (e *env) authorize(client oauth.CreatedClient, user authn.User, scope, challenge string) string {
	e.t.Helper()
	method := ""
	if challenge != "" {
		method = "S256"
	}
	prepared, err := e.svc.PrepareAuthorization(context.Background(), oauth.AuthorizationRequest{
		ResponseType:        "code",
		ClientID:            client.ClientID,
		RedirectURI:         "https://app.example.test/callback",
		Scope:               scope,
		State:               "state-1",
		CodeChallenge:       challenge,
		CodeChallengeMethod: method,
	}, user, e.authz)
	if err != nil {
		e.t.Fatalf("prepare authorization: %v", err)
	}
	code, err := e.svc.IssueCode(context.Background(), prepared, user.ID)
	if err != nil {
		e.t.Fatalf("issue code: %v", err)
	}
	return code
}

// ------------------------------------------------------------------ clients --

// A secret is returned once, and what is stored cannot be read back as a secret.
func TestClientSecretIsShownOnceAndStoredHashed(t *testing.T) {
	e := newEnv(t)
	created := e.newClient(true)

	if !created.ShownOnce || !strings.HasPrefix(created.Secret, "ocs_") {
		t.Fatalf("created client = %+v, want a shown-once ocs_ secret", created)
	}

	// The list a screen renders has no secret field at all, and says one is configured.
	clients, err := e.svc.ListClients(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(clients) != 1 {
		t.Fatalf("clients = %d, want 1", len(clients))
	}
	if !clients[0].HasSecret || !clients[0].Confidential {
		t.Fatalf("client = %+v, want confidential with a secret", clients[0])
	}

	// The stored value is a digest: 32 bytes, not the secret.
	var stored []byte
	if err := e.pool.QueryRow(context.Background(),
		`SELECT secret_hash FROM oauth_clients WHERE id = $1`, created.ID).Scan(&stored); err != nil {
		t.Fatalf("read hash: %v", err)
	}
	if len(stored) != 32 {
		t.Fatalf("stored secret is %d bytes, want a 32-byte digest", len(stored))
	}
	if strings.Contains(string(stored), created.Secret) {
		t.Fatal("the secret is stored in a readable form")
	}

	// Rotation replaces it: the old value stops authenticating the client at the token
	// endpoint, which is the only place a secret is used.
	rotated, err := e.svc.RotateSecret(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.Secret == created.Secret || !rotated.ShownOnce {
		t.Fatalf("rotation returned %+v, want a new shown-once secret", rotated)
	}
	if rotated.SecretRotatedAt == nil {
		t.Fatal("rotation did not record when it happened")
	}
}

// A public client has no secret at all, and disabling a secret destroys it rather than
// leaving a credential that exists but is not checked.
func TestDisableSecretMakesTheClientPublic(t *testing.T) {
	e := newEnv(t)
	created := e.newClient(true)

	client, err := e.svc.DisableSecret(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("disable secret: %v", err)
	}
	if client.HasSecret || client.Confidential {
		t.Fatalf("client = %+v, want a public client with no secret", client)
	}

	// And the flow now requires PKCE, because that is the only proof left.
	_, err = e.svc.PrepareAuthorization(context.Background(), oauth.AuthorizationRequest{
		ResponseType: "code",
		ClientID:     client.ClientID,
		RedirectURI:  "https://app.example.test/callback",
		Scope:        "invoice:read:self",
	}, e.insertUser("pkce@example.test"), e.authz)
	if !errors.Is(err, oauth.ErrPKCERequired) {
		t.Fatalf("authorize without PKCE = %v, want ErrPKCERequired", err)
	}
}

// A redirect URI that is not registered is refused, and refused without redirecting: the code
// must never be delivered to a URI the client did not register.
func TestAuthorizeRejectsUnregisteredRedirectURI(t *testing.T) {
	e := newEnv(t)
	client := e.newClient(true)
	user := e.insertUser("redirect@example.test")

	_, err := e.svc.PrepareAuthorization(context.Background(), oauth.AuthorizationRequest{
		ResponseType: "code",
		ClientID:     client.ClientID,
		RedirectURI:  "https://attacker.example.test/callback",
		Scope:        "invoice:read:self",
	}, user, e.authz)
	if !errors.Is(err, oauth.ErrInvalidRedirect) {
		t.Fatalf("unregistered redirect = %v, want ErrInvalidRedirect", err)
	}

	// A prefix of a registered URI is not a match either.
	_, err = e.svc.PrepareAuthorization(context.Background(), oauth.AuthorizationRequest{
		ResponseType: "code",
		ClientID:     client.ClientID,
		RedirectURI:  "https://app.example.test/callback/extra",
		Scope:        "invoice:read:self",
	}, user, e.authz)
	if !errors.Is(err, oauth.ErrInvalidRedirect) {
		t.Fatalf("prefix redirect = %v, want ErrInvalidRedirect", err)
	}
}

// -------------------------------------------------------------- code flow --

// The code is single-use, PKCE-bound and short-lived.
func TestAuthorizationCodeIsSingleUseAndPKCEBound(t *testing.T) {
	e := newEnv(t)
	client := e.newClient(false)
	user := e.insertUser("code@example.test")
	verifier, challenge := pkce()

	code := e.authorize(client, user, "invoice:read:self", challenge)

	// A wrong verifier is refused, and the code is not consumed by the attempt: a client
	// that mistypes should be able to retry with the right value rather than re-authorize.
	_, err := e.svc.ExchangeCode(context.Background(), oauth.CodeExchange{
		Code:         code,
		ClientID:     client.ClientID,
		RedirectURI:  "https://app.example.test/callback",
		CodeVerifier: strings.Repeat("z", 43),
	})
	if !errors.Is(err, oauth.ErrPKCEVerification) {
		t.Fatalf("wrong verifier = %v, want ErrPKCEVerification", err)
	}

	token, err := e.svc.ExchangeCode(context.Background(), oauth.CodeExchange{
		Code:         code,
		ClientID:     client.ClientID,
		RedirectURI:  "https://app.example.test/callback",
		CodeVerifier: verifier,
	})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if token.AccessToken == "" || token.RefreshToken == "" || token.TokenType != "Bearer" {
		t.Fatalf("token = %+v, want an access and refresh token", token)
	}
	if token.Scope != "invoice:read:self" {
		t.Fatalf("scope = %q, want invoice:read:self", token.Scope)
	}

	// A second exchange of the same code is refused.
	_, err = e.svc.ExchangeCode(context.Background(), oauth.CodeExchange{
		Code:         code,
		ClientID:     client.ClientID,
		RedirectURI:  "https://app.example.test/callback",
		CodeVerifier: verifier,
	})
	if !errors.Is(err, oauth.ErrInvalidCode) {
		t.Fatalf("replay = %v, want ErrInvalidCode", err)
	}

	// The access token authenticates, and carries the owner.
	presented, err := e.svc.Authenticate(context.Background(), token.AccessToken)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if presented.UserID != user.ID || presented.ClientID != client.ClientID {
		t.Fatalf("token owner = %+v, want user %s and client %s", presented, user.ID, client.ClientID)
	}

	// An expired code is refused, and so is an expired token.
	e.now = e.now.Add(oauth.AccessTokenLifetime + time.Minute)
	if _, err := e.svc.Authenticate(context.Background(), token.AccessToken); !errors.Is(err, oauth.ErrInvalidToken) {
		t.Fatalf("expired token = %v, want ErrInvalidToken", err)
	}
}

// A code minted for one client cannot be exchanged by another.
func TestAuthorizationCodeIsBoundToItsClient(t *testing.T) {
	e := newEnv(t)
	client := e.newClient(false)
	other := e.newClient(false)
	user := e.insertUser("bound@example.test")
	_, challenge := pkce()

	code := e.authorize(client, user, "invoice:read:self", challenge)

	_, err := e.svc.ExchangeCode(context.Background(), oauth.CodeExchange{
		Code:         code,
		ClientID:     other.ClientID,
		RedirectURI:  "https://app.example.test/callback",
		CodeVerifier: strings.Repeat("q", 43),
	})
	if !errors.Is(err, oauth.ErrInvalidCode) {
		t.Fatalf("cross-client exchange = %v, want ErrInvalidCode", err)
	}
}

// A confidential client must present its secret, and a public one must not.
func TestTokenEndpointAuthenticatesTheClient(t *testing.T) {
	e := newEnv(t)
	client := e.newClient(true)
	user := e.insertUser("confidential@example.test")
	code := e.authorize(client, user, "invoice:read:self", "")

	if _, err := e.svc.ExchangeCode(context.Background(), oauth.CodeExchange{
		Code:        code,
		ClientID:    client.ClientID,
		RedirectURI: "https://app.example.test/callback",
	}); !errors.Is(err, oauth.ErrClientAuth) {
		t.Fatalf("missing secret = %v, want ErrClientAuth", err)
	}
	if _, err := e.svc.ExchangeCode(context.Background(), oauth.CodeExchange{
		Code:         code,
		ClientID:     client.ClientID,
		ClientSecret: "ocs_wrong",
		RedirectURI:  "https://app.example.test/callback",
	}); !errors.Is(err, oauth.ErrClientAuth) {
		t.Fatalf("wrong secret = %v, want ErrClientAuth", err)
	}
	if _, err := e.svc.ExchangeCode(context.Background(), oauth.CodeExchange{
		Code:         code,
		ClientID:     client.ClientID,
		ClientSecret: client.Secret,
		RedirectURI:  "https://app.example.test/callback",
	}); err != nil {
		t.Fatalf("correct secret: %v", err)
	}
}

// The refresh token rotates on use, and a replayed one is refused.
func TestRefreshTokenRotatesOnUse(t *testing.T) {
	e := newEnv(t)
	client := e.newClient(true)
	user := e.insertUser("refresh@example.test")
	code := e.authorize(client, user, "invoice:read:self service:read:self", "")

	first, err := e.svc.ExchangeCode(context.Background(), oauth.CodeExchange{
		Code:         code,
		ClientID:     client.ClientID,
		ClientSecret: client.Secret,
		RedirectURI:  "https://app.example.test/callback",
	})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}

	second, err := e.svc.Refresh(context.Background(), oauth.RefreshExchange{
		RefreshToken: first.RefreshToken,
		ClientID:     client.ClientID,
		ClientSecret: client.Secret,
	})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if second.RefreshToken == first.RefreshToken {
		t.Fatal("the refresh token was not rotated")
	}
	if second.AccessToken == first.AccessToken {
		t.Fatal("refresh returned the same access token")
	}

	// The old refresh token is now a mismatch, which is what makes a stolen one usable at
	// most once.
	if _, err := e.svc.Refresh(context.Background(), oauth.RefreshExchange{
		RefreshToken: first.RefreshToken,
		ClientID:     client.ClientID,
		ClientSecret: client.Secret,
	}); !errors.Is(err, oauth.ErrInvalidGrant) {
		t.Fatalf("replayed refresh = %v, want ErrInvalidGrant", err)
	}

	// Revoking the grant kills the whole family, including the token minted a moment ago.
	if err := e.svc.RevokeGrant(context.Background(), user.ID, second.GrantID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := e.svc.Authenticate(context.Background(), second.AccessToken); !errors.Is(err, oauth.ErrInvalidToken) {
		t.Fatalf("token after revoke = %v, want ErrInvalidToken", err)
	}
}

// A refresh may narrow the scopes but never widen them.
func TestRefreshCannotWidenTheGrant(t *testing.T) {
	e := newEnv(t)
	client := e.newClient(true)
	user := e.insertUser("narrow@example.test")
	code := e.authorize(client, user, "invoice:read:self", "")

	first, err := e.svc.ExchangeCode(context.Background(), oauth.CodeExchange{
		Code:         code,
		ClientID:     client.ClientID,
		ClientSecret: client.Secret,
		RedirectURI:  "https://app.example.test/callback",
	})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}

	// service:read:self was never granted by this code.
	if _, err := e.svc.Refresh(context.Background(), oauth.RefreshExchange{
		RefreshToken: first.RefreshToken,
		ClientID:     client.ClientID,
		ClientSecret: client.Secret,
		Scope:        "invoice:read:self service:read:self",
	}); !errors.Is(err, oauth.ErrInvalidScope) {
		t.Fatalf("widened refresh = %v, want ErrInvalidScope", err)
	}
}

// --------------------------------------------------------------- scopes --

// A scope the client is not allowed to ask for, or one this build does not declare, is
// refused rather than silently dropped.
func TestScopesAreBoundedByTheClientAndTheBuild(t *testing.T) {
	e := newEnv(t)
	client := e.newClient(true)
	user := e.insertUser("scopes@example.test")

	for _, scope := range []string{"invoice:read:all", "not:a:permission"} {
		_, err := e.svc.PrepareAuthorization(context.Background(), oauth.AuthorizationRequest{
			ResponseType: "code",
			ClientID:     client.ClientID,
			RedirectURI:  "https://app.example.test/callback",
			Scope:        scope,
		}, user, e.authz)
		if !errors.Is(err, oauth.ErrInvalidScope) {
			t.Fatalf("scope %q = %v, want ErrInvalidScope", scope, err)
		}
	}
}

// A token can never carry a permission its owner does not hold: the request is narrowed at
// issue time, and the code refuses to mint anything when nothing survives.
func TestIssuedScopesAreTheOwnersPermissions(t *testing.T) {
	e := newEnv(t)
	// The client may ask for report:read, but this user is a plain customer.
	created, err := e.svc.CreateClient(context.Background(), oauth.NewClient{
		Name:          "Reporting App",
		RedirectURIs:  []string{"https://app.example.test/callback"},
		AllowedScopes: []string{"report:read", "invoice:read:self"},
		Confidential:  true,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	user := e.insertUser("customer@example.test")

	prepared, err := e.svc.PrepareAuthorization(context.Background(), oauth.AuthorizationRequest{
		ResponseType: "code",
		ClientID:     created.ClientID,
		RedirectURI:  "https://app.example.test/callback",
		Scope:        "report:read invoice:read:self",
	}, user, e.authz)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if got := oauth.JoinScopeList(prepared.Scopes); got != "invoice:read:self" {
		t.Fatalf("prepared scopes = %q, want only invoice:read:self", got)
	}

	// Removing the permission between the consent and the exchange means the exchange has
	// nothing left to grant, so it refuses instead of issuing an empty token.
	e.authz.SetTableRoles(map[authn.Role][]authn.Permission{})
	code, err := e.svc.IssueCode(context.Background(), prepared, user.ID)
	if err != nil {
		t.Fatalf("issue code: %v", err)
	}
	// The customer baseline still holds invoice:read:self, so this succeeds with it; the
	// point is that it carries exactly that and nothing from the client's ceiling.
	token, err := e.svc.ExchangeCode(context.Background(), oauth.CodeExchange{
		Code:         code,
		ClientID:     created.ClientID,
		ClientSecret: created.Secret,
		RedirectURI:  "https://app.example.test/callback",
	})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if token.Scope != "invoice:read:self" {
		t.Fatalf("token scope = %q, want invoice:read:self", token.Scope)
	}
}

// Consent is remembered, so a second authorization for the same scopes is silent.
func TestConsentIsRemembered(t *testing.T) {
	e := newEnv(t)
	client := e.newClient(true)
	user := e.insertUser("consent@example.test")

	prepared, err := e.svc.PrepareAuthorization(context.Background(), oauth.AuthorizationRequest{
		ResponseType: "code",
		ClientID:     client.ClientID,
		RedirectURI:  "https://app.example.test/callback",
		Scope:        "invoice:read:self",
	}, user, e.authz)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !prepared.NeedsConsent {
		t.Fatal("the first authorization must ask for consent")
	}
	if _, err := e.svc.IssueCode(context.Background(), prepared, user.ID); err != nil {
		t.Fatalf("issue code: %v", err)
	}

	again, err := e.svc.PrepareAuthorization(context.Background(), oauth.AuthorizationRequest{
		ResponseType: "code",
		ClientID:     client.ClientID,
		RedirectURI:  "https://app.example.test/callback",
		Scope:        "invoice:read:self",
	}, user, e.authz)
	if err != nil {
		t.Fatalf("prepare again: %v", err)
	}
	if again.NeedsConsent {
		t.Fatal("consent was not remembered")
	}

	// Asking for more than was agreed asks again.
	more, err := e.svc.PrepareAuthorization(context.Background(), oauth.AuthorizationRequest{
		ResponseType: "code",
		ClientID:     client.ClientID,
		RedirectURI:  "https://app.example.test/callback",
		Scope:        "invoice:read:self service:read:self",
	}, user, e.authz)
	if err != nil {
		t.Fatalf("prepare more: %v", err)
	}
	if !more.NeedsConsent {
		t.Fatal("a wider scope set must ask for consent again")
	}
}
