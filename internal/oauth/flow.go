package oauth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/authn"
	"github.com/jackc/pgx/v5"
)

// The authorization-code grant.
//
// The flow is the ordinary one, with the decisions that make it safe spelled out:
//
//	authorize  → validate client, exact redirect URI, response_type, scope and PKCE, then
//	             (if there is no consent covering the scopes) show a consent screen;
//	code       → a thirty-second, single-use, hashed value bound to client, user, redirect
//	             URI, scopes and PKCE challenge;
//	token      → authenticate the client, consume the code, verify the verifier, mint a
//	             grant (rotating refresh digest) and a one-hour access token.
//
// Scopes are resolved twice: once when the request is prepared (so the consent screen and the
// code carry what the user actually holds) and once when the token is minted (so a permission
// removed between the two is not granted). The third resolution — at use time — is the HTTP
// layer's, and it is the one that matters for a token that outlives a role change.

// AuthorizationRequest is a parsed /oauth/authorize query.
type AuthorizationRequest struct {
	ResponseType        string
	ClientID            string
	RedirectURI         string
	Scope               string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
}

// Authorization is a validated request, ready to consent to or to turn into a code.
type Authorization struct {
	Client      Client
	RedirectURI string
	Scopes      []string
	State       string
	// NeedsConsent is true when the user has not already agreed to these scopes for this
	// client. The caller shows a screen; the code is only issued after the POST.
	NeedsConsent        bool
	CodeChallenge       string
	CodeChallengeMethod string
}

// PrepareAuthorization validates an authorization request.
//
// The order is deliberate and is the security property: the client and the redirect URI are
// validated before anything else, because every later error is reported *by redirecting* to
// the redirect URI. Validating late would mean redirecting to an unvalidated URI, which
// hands the code to whoever supplied it.
func (s *Service) PrepareAuthorization(ctx context.Context, req AuthorizationRequest,
	user authn.User, authz *authn.Authorizer) (Authorization, error) {
	if authz == nil {
		return Authorization{}, fmt.Errorf("%w: authorization is not configured", ErrInvalidInput)
	}
	clientID := strings.TrimSpace(req.ClientID)
	if clientID == "" {
		return Authorization{}, fmt.Errorf("%w: client_id is required", ErrInvalidInput)
	}
	client, err := s.clientByPublicID(ctx, clientID)
	if err != nil {
		return Authorization{}, err
	}
	if !client.Enabled {
		// Reported as an unknown client: a disabled application is not owed an explanation,
		// and "disabled" tells a probe that the id exists.
		return Authorization{}, ErrUnknownClient
	}

	redirect, err := resolveRedirectURI(client, req.RedirectURI)
	if err != nil {
		return Authorization{}, err
	}

	if strings.TrimSpace(req.ResponseType) != "code" {
		return Authorization{}, ErrUnsupportedResponseType
	}

	if err := validatePKCE(client, req.CodeChallenge, req.CodeChallengeMethod); err != nil {
		return Authorization{}, err
	}

	scopes, err := ResolveScopes(ParseScopeList(req.Scope), client.AllowedScopes, authz.Permissions(user))
	if err != nil {
		return Authorization{}, err
	}

	needsConsent, err := s.needsConsent(ctx, client.ID, user.ID, scopes)
	if err != nil {
		return Authorization{}, err
	}

	return Authorization{
		Client:              client,
		RedirectURI:         redirect,
		Scopes:              scopes,
		State:               req.State,
		NeedsConsent:        needsConsent,
		CodeChallenge:       strings.TrimSpace(req.CodeChallenge),
		CodeChallengeMethod: strings.ToUpper(strings.TrimSpace(req.CodeChallengeMethod)),
	}, nil
}

// resolveRedirectURI matches the requested URI against the registered list.
//
// Exact string match. Not a prefix, not a normalised comparison: `https://app.example/cb` and
// `https://app.example/cb/../../evil` are different strings, and a normalisation step is a
// step an attacker gets to argue with. A request with no redirect_uri is allowed only when the
// client registered exactly one, which is the case RFC 6749 permits.
func resolveRedirectURI(client Client, requested string) (string, error) {
	value := strings.TrimSpace(requested)
	if value == "" {
		if len(client.RedirectURIs) == 1 {
			return client.RedirectURIs[0], nil
		}
		return "", fmt.Errorf("%w: redirect_uri is required", ErrInvalidRedirect)
	}
	for _, registered := range client.RedirectURIs {
		if registered == value {
			return value, nil
		}
	}
	return "", ErrInvalidRedirect
}

// validatePKCE enforces S256, and requires it for a client that has no secret.
//
// A public client has no way to prove it is itself; PKCE is that proof. Requiring it is what
// makes the authorization code useless to anyone who intercepts it.
func validatePKCE(client Client, challenge, method string) error {
	challenge = strings.TrimSpace(challenge)
	method = strings.ToUpper(strings.TrimSpace(method))
	if challenge == "" {
		if !client.Confidential {
			return ErrPKCERequired
		}
		return nil
	}
	if len(challenge) != 43 || strings.ContainsAny(challenge, "+/=") {
		return fmt.Errorf("%w: the challenge must be 43 base64url characters", ErrPKCEVerification)
	}
	if method != "S256" {
		return ErrPKCEMethod
	}
	return nil
}

// ClientRedirect validates a client id and a redirect URI, and returns both.
//
// It exists for one caller: the authorize endpoint has to decide whether a failed request may
// be reported *by redirecting* (the URI is registered, so the client is waiting there) or has
// to be a problem document (there is no verified destination, and inventing one is an open
// redirect). Doing that decision with this function keeps the exact-match rule in one place.
func (s *Service) ClientRedirect(ctx context.Context, clientID, redirectURI string) (Client, string, error) {
	client, err := s.clientByPublicID(ctx, strings.TrimSpace(clientID))
	if err != nil {
		return Client{}, "", err
	}
	if !client.Enabled {
		return Client{}, "", ErrUnknownClient
	}
	redirect, err := resolveRedirectURI(client, redirectURI)
	if err != nil {
		return Client{}, "", err
	}
	return client, redirect, nil
}

// needsConsent reports whether the user has already agreed to these scopes for this client.
func (s *Service) needsConsent(ctx context.Context, clientID, userID string, scopes []string) (bool, error) {
	var consented []string
	err := s.pool.QueryRow(ctx, `
		SELECT scopes FROM oauth_consents WHERE client_id = $1 AND user_id = $2`,
		clientID, userID).Scan(&consented)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("oauth: read consent: %w", err)
	}
	return !Subset(scopes, consented), nil
}

// IssueCode records consent and returns a fresh authorization code.
//
// The code is returned in the clear exactly once — to be placed in a redirect — and stored as
// a digest. Recording consent here rather than at the consent screen keeps the two facts
// (what was agreed, what was issued) in one transaction.
func (s *Service) IssueCode(ctx context.Context, authz Authorization, userID string) (string, error) {
	raw, err := randomSecret(32)
	if err != nil {
		return "", err
	}
	code := authCodePrefix + raw
	expires := s.now().Add(CodeLifetime)

	err = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		s.sweepExpired(ctx, tx)
		if _, err := tx.Exec(ctx, `
			INSERT INTO oauth_consents (client_id, user_id, scopes)
			VALUES ($1, $2, $3)
			ON CONFLICT (client_id, user_id) DO UPDATE
				SET scopes = EXCLUDED.scopes, updated_at = now()`,
			authz.Client.ID, userID, authz.Scopes); err != nil {
			return fmt.Errorf("oauth: record consent: %w", err)
		}
		var challenge, method any
		if authz.CodeChallenge != "" {
			challenge = authz.CodeChallenge
			method = authz.CodeChallengeMethod
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO oauth_auth_codes (code_hash, client_id, user_id, redirect_uri,
				scopes, code_challenge, code_challenge_method, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			digest(code), authz.Client.ID, userID, authz.RedirectURI, authz.Scopes,
			challenge, method, expires)
		if err != nil {
			return fmt.Errorf("oauth: store authorization code: %w", err)
		}
		return s.record(ctx, tx, "oauth.authorization_granted", authz.Client.ID, map[string]any{
			"client_id":    authz.Client.ClientID,
			"scopes":       authz.Scopes,
			"redirect_uri": authz.RedirectURI,
			"pkce":         authz.CodeChallenge != "",
			"granted_to":   userID,
		})
	})
	if err != nil {
		return "", err
	}
	return code, nil
}

// Token is a successful token response.
type Token struct {
	AccessToken  string
	TokenType    string
	ExpiresIn    int
	RefreshToken string
	Scope        string
	GrantID      string
	UserID       string
	ClientID     string
}

// CodeExchange is an authorization_code grant request.
type CodeExchange struct {
	Code         string
	ClientID     string
	ClientSecret string
	RedirectURI  string
	CodeVerifier string
}

// Permissions resolves a user's live permissions. The HTTP layer supplies one backed by
// authn.Authorizer; the service never decides permissions itself.
type Permissions func(ctx context.Context, userID string) (map[authn.Permission]struct{}, error)

// WithPermissions returns a copy of the service that resolves live permissions through fn.
//
// It is required for ExchangeCode and Refresh: the alternative — minting a token from the
// scopes stored on the code — would let a permission removed between the consent screen and
// the exchange survive into a token.
func (s *Service) WithPermissions(fn Permissions) *Service {
	clone := *s
	clone.permissions = fn
	return &clone
}

// ExchangeCode turns an authorization code into a token pair.
func (s *Service) ExchangeCode(ctx context.Context, in CodeExchange) (Token, error) {
	if s.permissions == nil {
		return Token{}, fmt.Errorf("%w: no permission source is configured", ErrInvalidInput)
	}
	client, err := s.authenticateClient(ctx, in.ClientID, in.ClientSecret)
	if err != nil {
		return Token{}, err
	}

	var out Token
	err = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			codeID      string
			codeClient  string
			userID      string
			redirectURI string
			scopes      []string
			challenge   *string
			method      *string
			expiresAt   time.Time
			consumedAt  *time.Time
		)
		err := tx.QueryRow(ctx, `
			SELECT id, client_id, user_id, redirect_uri, scopes, code_challenge,
			       code_challenge_method, expires_at, consumed_at
			FROM oauth_auth_codes
			WHERE code_hash = $1
			FOR UPDATE`, digest(strings.TrimSpace(in.Code))).
			Scan(&codeID, &codeClient, &userID, &redirectURI, &scopes, &challenge, &method,
				&expiresAt, &consumedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidCode
		}
		if err != nil {
			return fmt.Errorf("oauth: read authorization code: %w", err)
		}
		// A consumed code is refused rather than replayed, and refused with the same error
		// as an unknown one so a probe cannot tell the two apart.
		if consumedAt != nil || !s.now().Before(expiresAt) {
			return ErrInvalidCode
		}
		if codeClient != client.ID {
			return ErrInvalidCode
		}
		// Exact match, for the same reason the registered list is matched exactly: a
		// normalising comparison is a comparison an attacker gets to argue with.
		if strings.TrimSpace(in.RedirectURI) != redirectURI {
			return ErrInvalidRedirect
		}
		// PKCE: when a challenge was recorded, the verifier must match it. When none was
		// recorded, the client had a secret and that is what authenticated it above.
		if challenge != nil {
			if method == nil || *method != "S256" {
				return ErrPKCEMethod
			}
			if !validPKCEVerifier(in.CodeVerifier) || !verifyPKCE(in.CodeVerifier, *challenge) {
				return ErrPKCEVerification
			}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE oauth_auth_codes SET consumed_at = now() WHERE id = $1`, codeID); err != nil {
			return fmt.Errorf("oauth: consume authorization code: %w", err)
		}

		held, err := s.permissions(ctx, userID)
		if err != nil {
			return err
		}
		effective := narrowScopes(scopes, held)
		if len(effective) == 0 {
			return fmt.Errorf("%w: the user no longer holds any of the granted scopes", ErrInvalidScope)
		}

		token, err := s.issueToken(ctx, tx, client, userID, effective)
		if err != nil {
			return err
		}
		out = token
		return nil
	})
	if err != nil {
		return Token{}, err
	}
	return out, nil
}

// RefreshExchange is a refresh_token grant request.
type RefreshExchange struct {
	RefreshToken string
	ClientID     string
	ClientSecret string
	// Scope is optional. When present it must be a subset of what the grant already holds,
	// which is how RFC 6749 lets a client narrow a token it no longer needs in full.
	Scope string
}

// Refresh exchanges a refresh token for a new pair, rotating the refresh token.
//
// Rotation is the point: the old value stops working in the same transaction that issues the
// new one. A stolen refresh token is therefore usable at most once, and its use invalidates
// the legitimate holder's copy — a visible failure rather than a silent parallel session.
func (s *Service) Refresh(ctx context.Context, in RefreshExchange) (Token, error) {
	if s.permissions == nil {
		return Token{}, fmt.Errorf("%w: no permission source is configured", ErrInvalidInput)
	}
	client, err := s.authenticateClient(ctx, in.ClientID, in.ClientSecret)
	if err != nil {
		return Token{}, err
	}
	presented := strings.TrimSpace(in.RefreshToken)
	if !strings.HasPrefix(presented, refreshPrefix) {
		return Token{}, ErrInvalidGrant
	}

	var out Token
	err = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			grantID     string
			grantClient string
			userID      string
			scopes      []string
			expiresAt   time.Time
			revokedAt   *time.Time
		)
		err := tx.QueryRow(ctx, `
			SELECT id, client_id, user_id, scopes, expires_at, revoked_at
			FROM oauth_grants
			WHERE refresh_hash = $1
			FOR UPDATE`, digest(presented)).
			Scan(&grantID, &grantClient, &userID, &scopes, &expiresAt, &revokedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidGrant
		}
		if err != nil {
			return fmt.Errorf("oauth: read grant: %w", err)
		}
		if revokedAt != nil || !s.now().Before(expiresAt) || grantClient != client.ID {
			return ErrInvalidGrant
		}

		requested := scopes
		if narrowed := ParseScopeList(in.Scope); len(narrowed) > 0 {
			if !Subset(narrowed, scopes) {
				return fmt.Errorf("%w: a refresh cannot widen the grant", ErrInvalidScope)
			}
			requested = narrowed
		}

		held, err := s.permissions(ctx, userID)
		if err != nil {
			return err
		}
		effective := narrowScopes(requested, held)
		if len(effective) == 0 {
			return fmt.Errorf("%w: you no longer hold any of the granted scopes", ErrInvalidScope)
		}

		// Rotate: the digest is replaced, so the presented value can never be used again.
		rotated, err := randomSecret(32)
		if err != nil {
			return err
		}
		refresh := refreshPrefix + rotated
		if _, err := tx.Exec(ctx, `
			UPDATE oauth_grants SET refresh_hash = $2, rotated_at = now() WHERE id = $1`,
			grantID, digest(refresh)); err != nil {
			return fmt.Errorf("oauth: rotate refresh token: %w", err)
		}

		token, err := s.issueTokenWithRefresh(ctx, tx, client, userID, effective, grantID, refresh)
		if err != nil {
			return err
		}
		out = token
		return nil
	})
	if err != nil {
		return Token{}, err
	}
	return out, nil
}

// issueToken creates a grant and its first access token.
func (s *Service) issueToken(ctx context.Context, tx pgx.Tx, client Client, userID string,
	scopes []string) (Token, error) {
	rotated, err := randomSecret(32)
	if err != nil {
		return Token{}, err
	}
	refresh := refreshPrefix + rotated

	var grantID string
	err = tx.QueryRow(ctx, `
		INSERT INTO oauth_grants (client_id, user_id, scopes, refresh_hash, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		client.ID, userID, scopes, digest(refresh),
		s.now().Add(RefreshTokenLifetime)).Scan(&grantID)
	if err != nil {
		return Token{}, fmt.Errorf("oauth: create grant: %w", err)
	}
	return s.issueTokenWithRefresh(ctx, tx, client, userID, scopes, grantID, refresh)
}

// issueTokenWithRefresh mints the access token for an existing grant.
func (s *Service) issueTokenWithRefresh(ctx context.Context, tx pgx.Tx, client Client, userID string,
	scopes []string, grantID, refresh string) (Token, error) {
	raw, err := randomSecret(32)
	if err != nil {
		return Token{}, err
	}
	access := accessTokenPrefix + raw
	expires := s.now().Add(AccessTokenLifetime)

	if _, err := tx.Exec(ctx, `
		INSERT INTO oauth_access_tokens (grant_id, token_hash, scopes, expires_at)
		VALUES ($1, $2, $3, $4)`, grantID, digest(access), scopes, expires); err != nil {
		return Token{}, fmt.Errorf("oauth: create access token: %w", err)
	}

	if err := s.record(ctx, tx, "oauth.token_issued", client.ID, map[string]any{
		"client_id": client.ClientID,
		"scopes":    scopes,
		"grant_id":  grantID,
	}); err != nil {
		return Token{}, err
	}

	return Token{
		AccessToken:  access,
		TokenType:    "Bearer",
		ExpiresIn:    int(AccessTokenLifetime / time.Second),
		RefreshToken: refresh,
		Scope:        JoinScopeList(scopes),
		GrantID:      grantID,
		UserID:       userID,
		ClientID:     client.ClientID,
	}, nil
}

// AccessToken is a validated bearer credential.
type AccessToken struct {
	ID         string
	GrantID    string
	ClientID   string
	UserID     string
	Scopes     []string
	ExpiresAt  time.Time
	LastUsedAt *time.Time
}

// Authenticate resolves a presented access token.
//
// It returns the token so the caller can narrow the owner's permissions with its scopes. It
// never returns a token's owner's *permissions*: those are resolved by the caller from the
// user's roles on every request, which is what makes a role change take effect immediately.
func (s *Service) Authenticate(ctx context.Context, presented string) (AccessToken, error) {
	presented = strings.TrimSpace(presented)
	if !strings.HasPrefix(presented, accessTokenPrefix) {
		return AccessToken{}, ErrInvalidToken
	}

	var (
		token        AccessToken
		revoked      *time.Time
		grantRevoked *time.Time
		enabled      bool
	)
	err := s.pool.QueryRow(ctx, `
		SELECT t.id, t.grant_id, c.client_id, g.user_id, t.scopes, t.expires_at,
		       t.last_used_at, t.revoked_at, g.revoked_at, c.enabled
		FROM oauth_access_tokens t
		JOIN oauth_grants g ON g.id = t.grant_id
		JOIN oauth_clients c ON c.id = g.client_id
		WHERE t.token_hash = $1`, digest(presented)).
		Scan(&token.ID, &token.GrantID, &token.ClientID, &token.UserID, &token.Scopes,
			&token.ExpiresAt, &token.LastUsedAt, &revoked, &grantRevoked, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccessToken{}, ErrInvalidToken
	}
	if err != nil {
		return AccessToken{}, fmt.Errorf("oauth: read access token: %w", err)
	}
	if revoked != nil || grantRevoked != nil || !enabled || !s.now().Before(token.ExpiresAt) {
		return AccessToken{}, ErrInvalidToken
	}

	// Touch last_used_at at most once a minute, for the same reason an API key does: a read
	// endpoint that writes on every request turns every GET into contention on one row.
	if token.LastUsedAt == nil || s.now().Sub(*token.LastUsedAt) > time.Minute {
		_, _ = s.pool.Exec(ctx,
			`UPDATE oauth_access_tokens SET last_used_at = now() WHERE id = $1`, token.ID)
	}
	return token, nil
}

// RevokeGrant revokes a grant and every token it issued.
//
// It exists so "sign out of this application" is one statement rather than a sweep: the
// tokens are refused by the join on the grant, so revocation is immediate even for a token
// the caller is holding.
func (s *Service) RevokeGrant(ctx context.Context, userID, grantID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE oauth_grants SET revoked_at = now(), refresh_hash = NULL
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, grantID, userID)
	if err != nil {
		return fmt.Errorf("oauth: revoke grant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// authenticateClient verifies the client half of a token request.
//
// A confidential client must present the secret that matches its stored digest, compared in
// constant time. A public client must present no secret at all: accepting one would be
// pretending a public client can keep a secret, and refusing it loudly is better than
// quietly ignoring a credential the operator believes is protecting something.
func (s *Service) authenticateClient(ctx context.Context, clientID, secret string) (Client, error) {
	clientID = strings.TrimSpace(clientID)
	secret = strings.TrimSpace(secret)
	if clientID == "" {
		return Client{}, ErrUnknownClient
	}
	client, err := s.clientByPublicID(ctx, clientID)
	if err != nil {
		return Client{}, err
	}
	if !client.Enabled {
		return Client{}, ErrUnknownClient
	}
	if !client.Confidential {
		if secret != "" {
			return Client{}, ErrClientAuth
		}
		return client, nil
	}
	if secret == "" {
		return Client{}, ErrClientAuth
	}
	var stored []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT secret_hash FROM oauth_clients WHERE id = $1`, client.ID).Scan(&stored); err != nil {
		return Client{}, fmt.Errorf("oauth: read client secret: %w", err)
	}
	if len(stored) == 0 || subtle.ConstantTimeCompare(stored, digest(secret)) != 1 {
		return Client{}, ErrClientAuth
	}
	return client, nil
}

// narrowScopes keeps the scopes the user still holds, preserving order.
func narrowScopes(scopes []string, held map[authn.Permission]struct{}) []string {
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		if _, ok := held[authn.Permission(scope)]; ok {
			out = append(out, scope)
		}
	}
	return out
}
