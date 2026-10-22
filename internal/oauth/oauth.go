// Package oauth is the panel's OAuth2 authorization server and its social login.
//
// The two halves are one package because they are one question — who is this third party,
// and whose account is it acting for — and because the interesting failure mode of both is
// the same one: a credential that grants more than the person behind it currently holds.
//
// # Scopes are permissions, not a second vocabulary
//
// `authn.Permission` is a closed set that the rest of the panel already checks. A token's
// scope is a permission name, so an OAuth-authenticated request goes through exactly the
// same `requirePermission` check a session does; there is no second authorization path to
// keep in step. The scope a token carries is intersected with its owner's live permissions
// twice:
//
//   - at issue time (ResolveScopes), so a client cannot be handed a capability its user does
//     not hold, and the token's stored scopes are already the narrow set;
//   - at use time, because the HTTP layer resolves the bearer token to a *user* plus a
//     narrowing scope list and then runs the ordinary permission check against the user's
//     roles as they are now. Removing a permission from a role therefore narrows a token
//     that was minted before the change — the case a token carrying a copied permission list
//     would get wrong, silently and forever.
//
// # Every credential is a digest
//
// Client secrets, authorization codes, access tokens and refresh tokens are 256 bits of
// server randomness; only SHA-256 digests are stored. A database leak is then not a
// credential leak, and no endpoint can reveal a secret a second time — which is what makes
// "shown once" a property of the system rather than a promise in a UI.
//
// # Refresh tokens rotate
//
// A grant owns one refresh digest. Exchanging it replaces the digest in the same transaction
// that issues the new access token, so a replay of the old value is a mismatch and is
// refused rather than minting a second token. Revoking the grant kills the family.
//
// # Social login never merges accounts
//
// A provider identity is bound to an account only by a signed-in user linking it, and only
// when the provider's verified email matches that account's email. A callback whose email
// matches an existing account is refused with a message telling the person to sign in and
// link — never absorbed into the account. Treating a third party's email assertion as proof
// of ownership of a Catalyst account is the account-takeover bug, so it is not a policy this
// package can be configured out of.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// Lifetimes.
//
// They are constants rather than settings because each one is a security parameter, not a
// preference: an access token lives an hour because that is the window a leaked token is
// useful for, and a refresh token lives thirty days because that is how long a person
// expects "remember me" to last.
const (
	// AccessTokenLifetime is how long a bearer token is accepted.
	AccessTokenLifetime = time.Hour
	// RefreshTokenLifetime is how long a grant may be renewed.
	RefreshTokenLifetime = 30 * 24 * time.Hour
	// CodeLifetime is how long an authorization code may be exchanged. RFC 6749 permits up
	// to ten minutes; a minute is plenty for a redirect the browser is already making.
	CodeLifetime = time.Minute
	// StateLifetime is how long a social-login state is valid.
	StateLifetime = 10 * time.Minute
)

// Token prefixes.
//
// A prefix is not security, it is legibility: an operator who finds a string in a log or a
// bug report can tell what kind of credential it is, and a scanner can tell what to look
// for. `at_` is checked before anything else, so a token that is not ours is refused
// without a database round trip.
const (
	clientIDPrefix     = "oc_"
	clientSecretPrefix = "ocs_"
	authCodePrefix     = "ac_"
	accessTokenPrefix  = "at_"
	refreshPrefix      = "rt_"
)

// Errors are sentinels so the HTTP layer maps them to a stable problem code without
// matching on message text. The names double as the OAuth2 `error` values where RFC 6749
// defines one, so a third-party client sees the code it expects.
var (
	// ErrNotFound is returned when a record does not exist.
	ErrNotFound = errors.New("oauth: not found")
	// ErrInvalidInput is a malformed request that is not one of the protocol errors below.
	ErrInvalidInput = errors.New("oauth: invalid input")
	// ErrUnknownClient is returned for a client id this build does not know, or a disabled
	// client. It maps to OAuth2 `invalid_client` at the token endpoint.
	ErrUnknownClient = errors.New("oauth: unknown client")
	// ErrClientAuth is returned when a confidential client's secret is wrong or missing.
	ErrClientAuth = errors.New("oauth: client authentication failed")
	// ErrInvalidRedirect is returned when redirect_uri is not one of the client's registered
	// URIs. It is deliberately not a redirect to the supplied value: an unvalidated
	// redirect is how a code is delivered to an attacker.
	ErrInvalidRedirect = errors.New("oauth: redirect_uri is not registered for this client")
	// ErrUnsupportedGrant is returned for a grant_type this server does not implement.
	ErrUnsupportedGrant = errors.New("oauth: unsupported grant_type")
	// ErrUnsupportedResponseType is returned when response_type is not `code`.
	ErrUnsupportedResponseType = errors.New("oauth: unsupported response_type")
	// ErrInvalidScope is returned when a requested scope is unknown, outside the client's
	// allowed scopes, or not held by the user.
	ErrInvalidScope = errors.New("oauth: invalid scope")
	// ErrPKCERequired is returned when a public client omits the PKCE challenge.
	ErrPKCERequired = errors.New("oauth: a public client must use PKCE")
	// ErrPKCEMethod is returned for a challenge method other than S256.
	ErrPKCEMethod = errors.New("oauth: only the S256 PKCE method is supported")
	// ErrPKCEVerification is returned when the verifier does not match the challenge.
	ErrPKCEVerification = errors.New("oauth: the code verifier does not match the challenge")
	// ErrInvalidCode is returned for an unknown, expired or already-consumed code.
	ErrInvalidCode = errors.New("oauth: invalid authorization code")
	// ErrInvalidGrant is returned for an unknown, expired, revoked or rotated refresh token.
	ErrInvalidGrant = errors.New("oauth: invalid refresh token")
	// ErrInvalidToken is returned for an unknown, expired or revoked access token.
	ErrInvalidToken = errors.New("oauth: invalid access token")
	// ErrProviderDisabled is returned when a social provider is not enabled.
	ErrProviderDisabled = errors.New("oauth: the provider is not enabled")
	// ErrInvalidState is returned for a state that does not verify, has expired, was already
	// used, or belongs to another provider.
	ErrInvalidState = errors.New("oauth: the state parameter is invalid or has expired")
	// ErrIdentityTaken is returned when a provider subject is already bound to another user.
	ErrIdentityTaken = errors.New("oauth: this provider account is already linked to another user")
	// ErrEmailMismatch is returned when a link attempt's provider email differs from the
	// signed-in account's email.
	ErrEmailMismatch = errors.New("oauth: the provider email does not match your account")
	// ErrEmailUnverified is returned when the provider did not assert a verified email.
	ErrEmailUnverified = errors.New("oauth: the provider did not confirm the email address")
	// ErrAccountExists is returned when a sign-in would have to merge into an existing
	// account. It is the refusal that prevents an account takeover.
	ErrAccountExists = errors.New("oauth: an account with this email already exists")
	// ErrNoAccount is returned when there is nothing to sign in to.
	ErrNoAccount = errors.New("oauth: no account is linked to this identity")
	// ErrProviderExchange is returned when the provider's token or userinfo endpoint fails.
	ErrProviderExchange = errors.New("oauth: the provider rejected the exchange")
)

// Service is the OAuth2 server and social-login surface.
type Service struct {
	pool *store.Pool
	now  func() time.Time
	// httpClient is used for the provider calls. It is a field so a test can point the
	// provider rows at an httptest server instead of the internet, which is what makes the
	// social flow testable at all.
	httpClient *http.Client
	// auditor, when set, writes the audit row inside the transaction that made the change.
	// It is nil for a CLI or a test that constructs the service directly.
	auditor audit.Auditor
	// keys caches the social-login state signing key.
	keys *keyCache
	// permissions resolves a user's live permissions when a token is minted. The HTTP layer
	// supplies one backed by authn.Authorizer; nil is refused at exchange time rather than
	// treated as "no narrowing", because a silent fail-open here is a token that outlives a
	// revoked role.
	permissions Permissions
}

// New constructs the service.
func New(pool *store.Pool) *Service {
	return &Service{
		pool: pool,
		keys: &keyCache{},
		now:  func() time.Time { return time.Now().UTC() },
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// WithClock replaces the clock. Tests use it to expire a code without sleeping.
func (s *Service) WithClock(f func() time.Time) *Service {
	if f != nil {
		s.now = f
	}
	return s
}

// WithHTTPClient replaces the client used to talk to social providers.
func (s *Service) WithHTTPClient(c *http.Client) *Service {
	if c != nil {
		s.httpClient = c
	}
	return s
}

// Now is the service's clock, so a caller can compare expiries consistently.
func (s *Service) Now() time.Time { return s.now() }

// randomSecret returns n bytes of CSPRNG output as unpadded base64url.
//
// Every credential in this package comes from here. There is no fallback: a failure to read
// the system's CSPRNG is a failure to issue a credential, and issuing a predictable one is
// strictly worse than returning an error.
func randomSecret(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("oauth: read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// digest is the stored form of a credential.
//
// SHA-256 without a salt is correct here and would be wrong for a password: these values are
// 256 bits of server randomness, so there is nothing to guess and a per-row salt would only
// make lookup a scan.
func digest(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

// newClientID returns a public client identifier.
func newClientID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("oauth: read random bytes: %w", err)
	}
	return clientIDPrefix + hex.EncodeToString(buf), nil
}

// ResolveScopes intersects what was asked for with what the client may ask for and what the
// user actually holds.
//
// The three-way intersection is the whole point of the function: a client's allowed scopes
// are a ceiling set by an operator, the request is what the application says it needs, and
// the user's permissions are the truth. The result is what the token carries, and it is
// resolved again at use time — this function decides what the token *says*, the HTTP layer
// decides what it *does*.
//
// An unknown or out-of-ceiling scope is an error rather than a silent drop, because a client
// that believes it was granted `invoice:read:all` and quietly was not is a client that fails
// in production for a reason nobody can see. A scope the *user* does not hold is narrowed
// silently: the client asked for a capability the person does not have, and the honest answer
// is a token that does less.
func ResolveScopes(requested, allowed []string, held map[authn.Permission]struct{}) ([]string, error) {
	if len(requested) == 0 {
		return nil, fmt.Errorf("%w: a scope is required", ErrInvalidScope)
	}
	ceiling := make(map[string]struct{}, len(allowed))
	for _, scope := range allowed {
		ceiling[scope] = struct{}{}
	}
	seen := make(map[string]struct{}, len(requested))
	out := make([]string, 0, len(requested))
	for _, raw := range requested {
		scope := strings.TrimSpace(raw)
		if scope == "" {
			continue
		}
		if !authn.IsKnownPermission(scope) {
			return nil, fmt.Errorf("%w: %q is not a permission this build knows", ErrInvalidScope, scope)
		}
		if _, ok := ceiling[scope]; !ok {
			return nil, fmt.Errorf("%w: %q is not in this client's allowed scopes", ErrInvalidScope, scope)
		}
		if _, dup := seen[scope]; dup {
			continue
		}
		seen[scope] = struct{}{}
		if _, ok := held[authn.Permission(scope)]; !ok {
			continue
		}
		out = append(out, scope)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: you do not hold any of the requested scopes", ErrInvalidScope)
	}
	return out, nil
}

// ParseScopeList splits a space-delimited scope parameter.
func ParseScopeList(raw string) []string {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return nil
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if s := strings.TrimSpace(f); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// JoinScopeList renders scopes the way RFC 6749 wants them.
func JoinScopeList(scopes []string) string { return strings.Join(scopes, " ") }

// Subset reports whether every scope in want is in have.
func Subset(want, have []string) bool {
	set := make(map[string]struct{}, len(have))
	for _, s := range have {
		set[s] = struct{}{}
	}
	for _, s := range want {
		if _, ok := set[s]; !ok {
			return false
		}
	}
	return true
}

// verifyPKCE checks an S256 code verifier against the stored challenge.
//
// The comparison is constant time even though a challenge is not a secret in the usual
// sense: it is compared against an attacker-supplied value, and a timing signal would leak
// how much of a guess was right.
func verifyPKCE(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

// validPKCEVerifier reports whether a verifier has the length RFC 7636 requires.
func validPKCEVerifier(v string) bool {
	if len(v) < 43 || len(v) > 128 {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-', r == '.', r == '_', r == '~':
		default:
			return false
		}
	}
	return true
}

// isKnownPermission reports whether a string names a permission this build declares.
//
// It delegates to authn so the closed set has exactly one definition. A scope this build does
// not know cannot be stored, which is what stops a client from being registered with a
// capability the panel would silently ignore.
func isKnownPermission(name string) bool { return authn.IsKnownPermission(name) }

// sweepExpired deletes rows that can no longer be used.
//
// It runs opportunistically, on the write paths that already take a lock on these tables,
// rather than as a cron job: the tables are small, the delete is bounded, and a token table
// that grows forever is a token table that eventually makes the index lookup that guards
// every request slower. A failure is ignored — a stale row is refused by its expiry anyway.
func (s *Service) sweepExpired(ctx context.Context, tx pgx.Tx) {
	now := s.now()
	_, _ = tx.Exec(ctx, `DELETE FROM oauth_auth_codes WHERE expires_at < $1`, now)
	_, _ = tx.Exec(ctx, `DELETE FROM oauth_access_tokens WHERE expires_at < $1`, now)
	_, _ = tx.Exec(ctx, `DELETE FROM oauth_grants WHERE expires_at < $1`, now)
	_, _ = tx.Exec(ctx, `DELETE FROM social_login_states WHERE expires_at < $1`, now)
}
