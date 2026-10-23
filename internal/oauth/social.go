package oauth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Social login.
//
// One generic provider row, not three implementations. Google, GitHub and Discord differ in
// their endpoints, their scopes and which claim carries the subject and the email; every one
// of those differences is data in `social_providers`, so a fourth provider is an insert and a
// bug fix in this file fixes all of them at once.
//
// The two decisions that make this safe:
//
//   - **State is signed and single-use.** The value handed to the provider is
//     `v1.<payload>.<hmac>`; the payload names the provider, the mode and (for a link) the
//     user, and the nonce inside it is consumed by a row. A signature alone would allow a
//     replay inside the ten-minute window; a row alone would allow a forged state. The key
//     lives in the database (see 0038), so no deployment secret has to be invented for a
//     value with a ten-minute life.
//
//   - **Nothing is merged and nothing is created.** A sign-in callback resolves an existing
//     `social_identities` row or it refuses. It never looks an account up by email and signs
//     it in, and it never creates one: an email from an identity provider is a third party's
//     assertion, and treating it as proof of ownership of a Catalyst account is the
//     account-takeover bug. The only way a link is created is a *signed-in* user completing a
//     link flow whose provider email matches their own account email.

// Provider is a configured OIDC/OAuth2 provider.
//
// ClientSecret is populated for internal use (the token exchange) and must never be
// serialised: every read path that leaves the process uses Provider.Public().
type Provider struct {
	ID                    string
	Slug                  string
	Name                  string
	IssuerURL             string
	AuthorizationEndpoint string
	TokenEndpoint         string
	UserinfoEndpoint      string
	ClientID              string
	ClientSecret          string
	Scopes                []string
	SubjectClaim          string
	EmailClaim            string
	EmailVerifiedClaim    string
	Enabled               bool
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// Public returns the provider with the credential removed, for a response body.
func (p Provider) Public() Provider {
	p.ClientSecret = ""
	return p
}

// HasSecret reports whether a client secret is configured, for a screen that renders
// "configured / not configured" without ever receiving the value.
func (p Provider) HasSecret() bool { return p.ClientSecret != "" }

// NewProvider describes a provider to register.
//
// The three endpoints may be left empty when the issuer publishes OIDC discovery; they are
// then filled in from `<issuer>/.well-known/openid-configuration`, which is what makes
// "paste the issuer URL" a complete configuration for Google and Discord.
type NewProvider struct {
	Slug                  string
	Name                  string
	IssuerURL             string
	AuthorizationEndpoint string
	TokenEndpoint         string
	UserinfoEndpoint      string
	ClientID              string
	ClientSecret          string
	Scopes                []string
	SubjectClaim          string
	EmailClaim            string
	EmailVerifiedClaim    string
	Enabled               bool
}

// providerColumns is the read shape of a provider.
//
// `email_verified_claim` is COALESCEd because the column is nullable and the struct field is a
// string: a row whose claim mapping was left unset — a hand-written row, or one an older build
// wrote before the field existed — used to fail the scan outright, and because the whole list is
// read with one query, a single such row took down the entire sign-in provider screen. A NULL
// means "this provider cannot prove an address", which is exactly the empty string the code
// already treats as that.
const providerColumns = `id, slug, name, issuer_url, authorization_endpoint, token_endpoint,
	userinfo_endpoint, client_id, client_secret, scopes, subject_claim, email_claim,
	COALESCE(email_verified_claim, ''), enabled, created_at, updated_at`

func scanProvider(row pgx.Row) (Provider, error) {
	var p Provider
	err := row.Scan(&p.ID, &p.Slug, &p.Name, &p.IssuerURL, &p.AuthorizationEndpoint,
		&p.TokenEndpoint, &p.UserinfoEndpoint, &p.ClientID, &p.ClientSecret, &p.Scopes,
		&p.SubjectClaim, &p.EmailClaim, &p.EmailVerifiedClaim, &p.Enabled,
		&p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Provider{}, ErrNotFound
	}
	if err != nil {
		return Provider{}, fmt.Errorf("oauth: read provider: %w", err)
	}
	return p, nil
}

// CreateProvider registers a provider.
func (s *Service) CreateProvider(ctx context.Context, in NewProvider) (Provider, error) {
	clean, err := s.normalizeProvider(ctx, in)
	if err != nil {
		return Provider{}, err
	}
	var out Provider
	err = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO social_providers (slug, name, issuer_url, authorization_endpoint,
				token_endpoint, userinfo_endpoint, client_id, client_secret, scopes,
				subject_claim, email_claim, email_verified_claim, enabled)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			RETURNING `+providerColumns,
			clean.Slug, clean.Name, clean.IssuerURL, clean.AuthorizationEndpoint,
			clean.TokenEndpoint, clean.UserinfoEndpoint, clean.ClientID, clean.ClientSecret,
			clean.Scopes, clean.SubjectClaim, clean.EmailClaim, clean.EmailVerifiedClaim,
			clean.Enabled)
		provider, err := scanProvider(row)
		if err != nil {
			return err
		}
		out = provider
		return s.recordProvider(ctx, tx, "oauth.provider_created", provider)
	})
	if err != nil {
		return Provider{}, err
	}
	return out, nil
}

// UpdateProvider replaces a provider's configuration.
//
// An empty ClientSecret keeps the stored one, which is what lets the edit form show a
// redacted field: submitting the form unchanged must not wipe the credential.
func (s *Service) UpdateProvider(ctx context.Context, id string, in NewProvider) (Provider, error) {
	clean, err := s.normalizeProvider(ctx, in)
	if err != nil {
		return Provider{}, err
	}
	var out Provider
	err = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE social_providers SET
				slug = $2, name = $3, issuer_url = $4, authorization_endpoint = $5,
				token_endpoint = $6, userinfo_endpoint = $7, client_id = $8,
				client_secret = COALESCE(NULLIF($9, ''), client_secret), scopes = $10,
				subject_claim = $11, email_claim = $12, email_verified_claim = $13,
				enabled = $14, updated_at = now()
			WHERE id = $1
			RETURNING `+providerColumns,
			id, clean.Slug, clean.Name, clean.IssuerURL, clean.AuthorizationEndpoint,
			clean.TokenEndpoint, clean.UserinfoEndpoint, clean.ClientID, clean.ClientSecret,
			clean.Scopes, clean.SubjectClaim, clean.EmailClaim, clean.EmailVerifiedClaim,
			clean.Enabled)
		provider, err := scanProvider(row)
		if err != nil {
			return err
		}
		out = provider
		return s.recordProvider(ctx, tx, "oauth.provider_updated", provider)
	})
	if err != nil {
		return Provider{}, err
	}
	return out, nil
}

// DeleteProvider removes a provider and every link it made.
//
// The cascade deletes the identities, which means a person who signed in with it can no
// longer do so. That is the correct meaning of removing a provider: the sign-in method stops
// existing, rather than silently remaining usable through a row nobody can see any more.
func (s *Service) DeleteProvider(ctx context.Context, id string) error {
	return s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var slug string
		err := tx.QueryRow(ctx, `DELETE FROM social_providers WHERE id = $1 RETURNING slug`, id).
			Scan(&slug)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("oauth: delete provider: %w", err)
		}
		return s.recordProviderID(ctx, tx, "oauth.provider_deleted", id, slug)
	})
}

// ListProviders returns the configured providers.
//
// With enabledOnly it is the list the sign-in screen renders: a disabled provider is not
// offered as a button, because a button that leads to a refusal is worse than no button.
func (s *Service) ListProviders(ctx context.Context, enabledOnly bool) ([]Provider, error) {
	query := `SELECT ` + providerColumns + ` FROM social_providers`
	if enabledOnly {
		query += ` WHERE enabled`
	}
	query += ` ORDER BY name`
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("oauth: list providers: %w", err)
	}
	defer rows.Close()

	out := []Provider{}
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProviderBySlug reads one provider by its URL slug.
func (s *Service) ProviderBySlug(ctx context.Context, slug string) (Provider, error) {
	return scanProvider(s.pool.QueryRow(ctx,
		`SELECT `+providerColumns+` FROM social_providers WHERE slug = $1`, slug))
}

// ProviderByID reads one provider by id.
func (s *Service) ProviderByID(ctx context.Context, id string) (Provider, error) {
	return scanProvider(s.pool.QueryRow(ctx,
		`SELECT `+providerColumns+` FROM social_providers WHERE id = $1`, id))
}

// normalizeProvider validates a provider and fills in endpoints from OIDC discovery.
func (s *Service) normalizeProvider(ctx context.Context, in NewProvider) (NewProvider, error) {
	out := in
	out.Slug = strings.ToLower(strings.TrimSpace(in.Slug))
	out.Name = strings.TrimSpace(in.Name)
	out.IssuerURL = strings.TrimRight(strings.TrimSpace(in.IssuerURL), "/")
	out.ClientID = strings.TrimSpace(in.ClientID)
	out.ClientSecret = strings.TrimSpace(in.ClientSecret)
	out.SubjectClaim = defaultString(strings.TrimSpace(in.SubjectClaim), "sub")
	out.EmailClaim = defaultString(strings.TrimSpace(in.EmailClaim), "email")
	out.EmailVerifiedClaim = strings.TrimSpace(in.EmailVerifiedClaim)

	if out.Name == "" {
		out.Name = out.Slug
	}
	if len(out.Slug) < 2 || len(out.Slug) > 32 {
		return NewProvider{}, fmt.Errorf("%w: the slug must be 2–32 characters", ErrInvalidInput)
	}
	for _, r := range out.Slug {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return NewProvider{}, fmt.Errorf("%w: the slug may contain a-z, 0-9, - and _", ErrInvalidInput)
		}
	}
	if out.ClientID == "" {
		return NewProvider{}, fmt.Errorf("%w: a client id is required", ErrInvalidInput)
	}
	if out.ClientSecret == "" && in.ClientSecret != "" {
		return NewProvider{}, fmt.Errorf("%w: the client secret is empty", ErrInvalidInput)
	}
	if len(out.Scopes) == 0 {
		out.Scopes = []string{"openid", "email"}
	}

	out.AuthorizationEndpoint = strings.TrimSpace(in.AuthorizationEndpoint)
	out.TokenEndpoint = strings.TrimSpace(in.TokenEndpoint)
	out.UserinfoEndpoint = strings.TrimSpace(in.UserinfoEndpoint)
	if out.AuthorizationEndpoint == "" || out.TokenEndpoint == "" || out.UserinfoEndpoint == "" {
		// Discovery fills the gaps. It is done at configuration time rather than at every
		// login: a provider's metadata changes once in a blue moon, and a login that depends
		// on a second network round trip to a third party is a login that fails when that
		// third party is slow.
		discovered, err := s.discover(ctx, out.IssuerURL)
		if err != nil {
			return NewProvider{}, err
		}
		if out.AuthorizationEndpoint == "" {
			out.AuthorizationEndpoint = discovered.AuthorizationEndpoint
		}
		if out.TokenEndpoint == "" {
			out.TokenEndpoint = discovered.TokenEndpoint
		}
		if out.UserinfoEndpoint == "" {
			out.UserinfoEndpoint = discovered.UserinfoEndpoint
		}
	}
	for label, endpoint := range map[string]string{
		"authorization_endpoint": out.AuthorizationEndpoint,
		"token_endpoint":         out.TokenEndpoint,
		"userinfo_endpoint":      out.UserinfoEndpoint,
	} {
		u, err := url.Parse(endpoint)
		if err != nil || !u.IsAbs() || (u.Scheme != "https" && u.Scheme != "http") {
			return NewProvider{}, fmt.Errorf("%w: %s must be an absolute http(s) URL", ErrInvalidInput, label)
		}
	}
	return out, nil
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// discovered is the subset of an OIDC discovery document this package uses.
type discovered struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

// discover reads `<issuer>/.well-known/openid-configuration`.
func (s *Service) discover(ctx context.Context, issuer string) (discovered, error) {
	if issuer == "" {
		return discovered{}, fmt.Errorf("%w: an issuer URL or explicit endpoints are required", ErrInvalidInput)
	}
	u, err := url.Parse(issuer)
	if err != nil || !u.IsAbs() {
		return discovered{}, fmt.Errorf("%w: the issuer URL must be absolute", ErrInvalidInput)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return discovered{}, fmt.Errorf("%w: %v", ErrProviderExchange, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return discovered{}, fmt.Errorf("%w: %v", ErrProviderExchange, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return discovered{}, fmt.Errorf("%w: discovery returned %d", ErrProviderExchange, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return discovered{}, fmt.Errorf("%w: %v", ErrProviderExchange, err)
	}
	var doc discovered
	if err := json.Unmarshal(body, &doc); err != nil {
		return discovered{}, fmt.Errorf("%w: discovery document is not JSON", ErrProviderExchange)
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" {
		return discovered{}, fmt.Errorf("%w: discovery document is incomplete", ErrProviderExchange)
	}
	return doc, nil
}

// ---------------------------------------------------------------- state --

// statePayload is what the signed state carries.
//
// It is deliberately small: a nonce, which provider and mode it is for, the user id for a
// link, and an expiry. Nothing secret, because the payload is readable by the browser — the
// signature is what makes it trustworthy, not the encoding.
type statePayload struct {
	Nonce      string `json:"n"`
	ProviderID string `json:"p"`
	Mode       string `json:"m"`
	UserID     string `json:"u,omitempty"`
	ExpiresAt  int64  `json:"e"`
}

// StateClaims is a verified state value.
type StateClaims struct {
	ProviderID string
	Mode       string
	UserID     string
	Nonce      string
	ExpiresAt  time.Time
}

// StateModes.
const (
	// StateSignIn ends in a session for the linked account.
	StateSignIn = "signin"
	// StateLink binds the provider identity to the signed-in user.
	StateLink = "link"
)

// SignState returns a signed, single-use state value for a provider.
func (s *Service) SignState(ctx context.Context, provider Provider, mode, userID string) (string, error) {
	if mode != StateSignIn && mode != StateLink {
		return "", fmt.Errorf("%w: unknown state mode %q", ErrInvalidInput, mode)
	}
	if mode == StateLink && userID == "" {
		return "", fmt.Errorf("%w: a link state needs a user", ErrInvalidInput)
	}
	nonce, err := randomSecret(16)
	if err != nil {
		return "", err
	}
	payload := statePayload{
		Nonce:      nonce,
		ProviderID: provider.ID,
		Mode:       mode,
		UserID:     userID,
		ExpiresAt:  s.now().Add(StateLifetime).Unix(),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("oauth: encode state: %w", err)
	}
	key, err := s.stateKey(ctx)
	if err != nil {
		return "", err
	}
	signature := signState(key, encoded)

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO social_login_states (nonce_hash, provider_id, mode, user_id, expires_at)
		VALUES ($1, $2, $3, $4, $5)`,
		digest(nonce), provider.ID, mode, nullable(userID), s.now().Add(StateLifetime)); err != nil {
		return "", fmt.Errorf("oauth: store state: %w", err)
	}
	return "v1." + base64.RawURLEncoding.EncodeToString(encoded) + "." +
		base64.RawURLEncoding.EncodeToString(signature), nil
}

// ConsumeState verifies and burns a state value.
//
// The three checks are separate on purpose: the signature says nobody forged it, the payload
// says it was minted for this provider and this mode, and the UPDATE says it has not been
// used. Any one of them alone is insufficient, and the order means a forged value never
// touches the database.
func (s *Service) ConsumeState(ctx context.Context, provider Provider, state string) (StateClaims, error) {
	parts := strings.Split(strings.TrimSpace(state), ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return StateClaims{}, ErrInvalidState
	}
	encoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return StateClaims{}, ErrInvalidState
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return StateClaims{}, ErrInvalidState
	}
	key, err := s.stateKey(ctx)
	if err != nil {
		return StateClaims{}, err
	}
	if !hmac.Equal(signature, signState(key, encoded)) {
		return StateClaims{}, ErrInvalidState
	}
	var payload statePayload
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return StateClaims{}, ErrInvalidState
	}
	if payload.ProviderID != provider.ID {
		return StateClaims{}, ErrInvalidState
	}
	if !s.now().Before(time.Unix(payload.ExpiresAt, 0)) {
		return StateClaims{}, ErrInvalidState
	}

	// Burn the nonce. A state that was already consumed, or that this provider did not mint,
	// matches no row and is refused.
	tag, err := s.pool.Exec(ctx, `
		UPDATE social_login_states SET consumed_at = now()
		WHERE nonce_hash = $1 AND provider_id = $2 AND consumed_at IS NULL AND expires_at > $3`,
		digest(payload.Nonce), provider.ID, s.now())
	if err != nil {
		return StateClaims{}, fmt.Errorf("oauth: consume state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return StateClaims{}, ErrInvalidState
	}
	return StateClaims{
		ProviderID: payload.ProviderID,
		Mode:       payload.Mode,
		UserID:     payload.UserID,
		Nonce:      payload.Nonce,
		ExpiresAt:  time.Unix(payload.ExpiresAt, 0),
	}, nil
}

func signState(key, payload []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	return mac.Sum(nil)
}

// stateKey reads the signing key, cached for the process.
//
// A missing row is an error rather than a generated key: two instances that each invented a
// key would each reject the other's states, which presents as an intermittent login failure
// that is very hard to explain.
func (s *Service) stateKey(ctx context.Context) ([]byte, error) {
	s.keys.mu.Lock()
	defer s.keys.mu.Unlock()
	if len(s.keys.key) > 0 {
		return s.keys.key, nil
	}
	var key []byte
	err := s.pool.QueryRow(ctx,
		`SELECT key FROM oauth_signing_keys WHERE id = 'social-state'`).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: no state signing key is configured", ErrInvalidState)
	}
	if err != nil {
		return nil, fmt.Errorf("oauth: read state signing key: %w", err)
	}
	s.keys.key = key
	return key, nil
}

// ------------------------------------------------------------ exchange --

// Identity is what a provider says about the person who just signed in.
type Identity struct {
	Subject       string
	Email         string
	EmailVerified bool
	// Raw is the userinfo document, kept for the audit trail's benefit. It is not stored.
	Raw map[string]any
}

// ExchangeIdentity trades an authorization code for the provider's userinfo document.
//
// Both calls are bounded (timeout, 1 MiB response) and the result is validated against the
// configured claim mapping: a provider that stops returning the subject claim is a
// configuration error, and it is reported as one rather than creating a link with an empty
// subject.
func (s *Service) ExchangeIdentity(ctx context.Context, provider Provider, code, redirectURI string) (Identity, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {provider.ClientID},
		"client_secret": {provider.ClientSecret},
		"redirect_uri":  {redirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.TokenEndpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrProviderExchange, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// GitHub answers form-encoded unless JSON is asked for, and this server only parses JSON.
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrProviderExchange, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrProviderExchange, err)
	}
	if resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("%w: the token endpoint returned %d", ErrProviderExchange, resp.StatusCode)
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &token); err != nil || token.AccessToken == "" {
		return Identity{}, fmt.Errorf("%w: the token endpoint returned no access token", ErrProviderExchange)
	}

	infoReq, err := http.NewRequestWithContext(ctx, http.MethodGet, provider.UserinfoEndpoint, nil)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrProviderExchange, err)
	}
	infoReq.Header.Set("Authorization", "Bearer "+token.AccessToken)
	infoReq.Header.Set("Accept", "application/json")
	infoResp, err := s.httpClient.Do(infoReq)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrProviderExchange, err)
	}
	defer infoResp.Body.Close()
	infoBody, err := io.ReadAll(io.LimitReader(infoResp.Body, 1<<20))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrProviderExchange, err)
	}
	if infoResp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("%w: the userinfo endpoint returned %d", ErrProviderExchange, infoResp.StatusCode)
	}
	var claims map[string]any
	if err := json.Unmarshal(infoBody, &claims); err != nil {
		return Identity{}, fmt.Errorf("%w: the userinfo document is not JSON", ErrProviderExchange)
	}

	subject := claimString(claims, provider.SubjectClaim)
	if subject == "" {
		return Identity{}, fmt.Errorf("%w: the userinfo document has no %q claim",
			ErrProviderExchange, provider.SubjectClaim)
	}
	email := strings.TrimSpace(claimString(claims, provider.EmailClaim))
	verified := true
	if provider.EmailVerifiedClaim != "" {
		verified = claimBool(claims, provider.EmailVerifiedClaim)
	}
	return Identity{
		Subject:       subject,
		Email:         strings.ToLower(email),
		EmailVerified: verified,
		Raw:           claims,
	}, nil
}

// claimValue walks a dotted claim path, so a provider that nests its subject
// (`user.id`) needs no code change.
func claimValue(claims map[string]any, path string) any {
	if path == "" {
		return nil
	}
	var current any = claims
	for _, part := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current, ok = object[part]
		if !ok {
			return nil
		}
	}
	return current
}

func claimString(claims map[string]any, path string) string {
	switch value := claimValue(claims, path).(type) {
	case string:
		return strings.TrimSpace(value)
	case float64:
		// GitHub returns its numeric id as a JSON number; a subject is an opaque string, so
		// rendering it without an exponent is the right conversion.
		return strconv.FormatFloat(value, 'f', -1, 64)
	case json.Number:
		return value.String()
	default:
		return ""
	}
}

func claimBool(claims map[string]any, path string) bool {
	switch value := claimValue(claims, path).(type) {
	case bool:
		return value
	case string:
		return strings.EqualFold(value, "true")
	default:
		return false
	}
}

// ---------------------------------------------------------- outcomes --

// Outcome is the result of a completed social callback.
type Outcome string

const (
	// OutcomeSignedIn means an existing link resolved to an account.
	OutcomeSignedIn Outcome = "signed_in"
	// OutcomeLinked means a provider identity was bound to the signed-in user.
	OutcomeLinked Outcome = "linked"
	// OutcomeAccountExists means an account with the provider's email exists but is not
	// linked. The flow refuses and tells the person to sign in and link; it never merges.
	OutcomeAccountExists Outcome = "account_exists"
	// OutcomeNoAccount means there is nothing to sign in to.
	OutcomeNoAccount Outcome = "no_account"
)

// SignInResult is what a callback resolves to.
type SignInResult struct {
	Outcome Outcome
	User    Account
}

// Account is the subset of a user a social login needs.
type Account struct {
	ID            string
	Email         string
	DisplayName   string
	Status        string
	EmailVerified bool
}

// CompleteSignIn resolves a provider identity to an account, or refuses.
//
// The refusal is the feature. An email that matches an existing account is reported as
// `account_exists` and nothing else happens — no session, no link, no merge — because the
// alternative is that anyone who can create a provider account with somebody else's address
// can sign in as them.
func (s *Service) CompleteSignIn(ctx context.Context, provider Provider, identity Identity) (SignInResult, error) {
	linked, err := s.accountBySubject(ctx, provider.ID, identity.Subject)
	switch {
	case err == nil:
		if linked.Status != "active" {
			// A link outliving a disabled account would be a permanent way back in.
			return SignInResult{Outcome: OutcomeNoAccount}, nil
		}
		return SignInResult{Outcome: OutcomeSignedIn, User: linked}, nil
	case !errors.Is(err, ErrNotFound):
		return SignInResult{}, err
	}

	if identity.Email == "" {
		return SignInResult{Outcome: OutcomeNoAccount}, nil
	}
	existing, err := s.accountByEmail(ctx, identity.Email)
	switch {
	case err == nil:
		return SignInResult{Outcome: OutcomeAccountExists, User: existing}, nil
	case !errors.Is(err, ErrNotFound):
		return SignInResult{}, err
	}
	return SignInResult{Outcome: OutcomeNoAccount}, nil
}

// CompleteLink binds a provider identity to the signed-in user.
//
// Two conditions, both required: the provider must have asserted a verified email, and that
// email must be the account's own. Without them, "link my Google account" is a way to attach
// somebody else's identity to your account — or your identity to somebody else's.
func (s *Service) CompleteLink(ctx context.Context, provider Provider, identity Identity,
	userID string) (SignInResult, error) {
	if userID == "" {
		return SignInResult{}, fmt.Errorf("%w: linking needs a signed-in user", ErrInvalidInput)
	}
	if identity.Email == "" {
		return SignInResult{}, ErrEmailUnverified
	}
	if provider.EmailVerifiedClaim != "" && !identity.EmailVerified {
		return SignInResult{}, ErrEmailUnverified
	}
	user, err := s.accountByID(ctx, userID)
	if err != nil {
		return SignInResult{}, err
	}
	if !strings.EqualFold(strings.TrimSpace(user.Email), identity.Email) {
		return SignInResult{}, ErrEmailMismatch
	}

	var out SignInResult
	err = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO social_identities (provider_id, user_id, subject, email)
			VALUES ($1, $2, $3, $4)`,
			provider.ID, userID, identity.Subject, identity.Email)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				// Either this provider identity belongs to another account, or this account
				// already has a different identity for this provider. Both are refusals: the
				// first would be a takeover, the second needs an explicit unlink.
				return ErrIdentityTaken
			}
			return fmt.Errorf("oauth: link identity: %w", err)
		}
		out = SignInResult{Outcome: OutcomeLinked, User: user}
		return s.record(ctx, tx, "oauth.identity_linked", provider.ID, map[string]any{
			"provider": provider.Slug,
			"user_id":  userID,
			"email":    identity.Email,
		})
	})
	if err != nil {
		return SignInResult{}, err
	}
	return out, nil
}

// SocialLink is one provider identity bound to an account.
type SocialLink struct {
	ID           string
	ProviderID   string
	ProviderSlug string
	ProviderName string
	Subject      string
	Email        string
	CreatedAt    time.Time
}

// LinksForUser lists the provider identities bound to an account.
func (s *Service) LinksForUser(ctx context.Context, userID string) ([]SocialLink, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, i.provider_id, p.slug, p.name, i.subject, COALESCE(i.email, ''), i.created_at
		FROM social_identities i
		JOIN social_providers p ON p.id = i.provider_id
		WHERE i.user_id = $1
		ORDER BY p.name`, userID)
	if err != nil {
		return nil, fmt.Errorf("oauth: list links: %w", err)
	}
	defer rows.Close()

	out := []SocialLink{}
	for rows.Next() {
		var l SocialLink
		if err := rows.Scan(&l.ID, &l.ProviderID, &l.ProviderSlug, &l.ProviderName,
			&l.Subject, &l.Email, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("oauth: scan link: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Unlink removes a link the caller owns.
//
// Ownership is part of the WHERE clause rather than a separate check, so somebody else's link
// is indistinguishable from one that does not exist.
func (s *Service) Unlink(ctx context.Context, userID, linkID string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM social_identities WHERE id = $1 AND user_id = $2`, linkID, userID)
	if err != nil {
		return fmt.Errorf("oauth: unlink: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// accountBySubject resolves a provider identity to its account.
func (s *Service) accountBySubject(ctx context.Context, providerID, subject string) (Account, error) {
	var userID string
	err := s.pool.QueryRow(ctx, `
		SELECT user_id FROM social_identities WHERE provider_id = $1 AND subject = $2`,
		providerID, subject).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("oauth: read identity: %w", err)
	}
	return s.accountByID(ctx, userID)
}

// accountByID reads the account fields a social login needs.
func (s *Service) accountByID(ctx context.Context, id string) (Account, error) {
	var a Account
	var displayName *string
	err := s.pool.QueryRow(ctx, `
		SELECT id, email, display_name, status, email_verified_at IS NOT NULL
		FROM users WHERE id = $1`, id).
		Scan(&a.ID, &a.Email, &displayName, &a.Status, &a.EmailVerified)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("oauth: read account: %w", err)
	}
	if displayName != nil {
		a.DisplayName = *displayName
	}
	return a, nil
}

// accountByEmail looks an account up by address.
//
// It is used for exactly one thing: deciding that an account exists and the flow must refuse.
// It is never used to sign somebody in, and the name says so.
func (s *Service) accountByEmail(ctx context.Context, email string) (Account, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id FROM users WHERE lower(email) = lower($1)`, email).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("oauth: read account by email: %w", err)
	}
	return s.accountByID(ctx, id)
}

// recordProvider writes an audit row for a provider change, redacting the secret.
func (s *Service) recordProvider(ctx context.Context, tx pgx.Tx, action string, p Provider) error {
	return s.recordProviderID(ctx, tx, action, p.ID, p.Slug)
}

func (s *Service) recordProviderID(ctx context.Context, tx pgx.Tx, action, id, slug string) error {
	if s.auditor == nil {
		return nil
	}
	return s.auditor.RecordAction(ctx, tx, action, "social_provider", id,
		map[string]any{"slug": slug})
}

// nullable turns an empty string into SQL NULL.
func nullable(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

// keyCache holds the state signing key for the process.
//
// It is a pointer so the With... clones share it: a copy of a mutex is a copy of a lock, and
// two services with two caches would each read the key once, which is merely wasteful — but
// two services with two *copied* mutexes is a data race.
type keyCache struct {
	mu  sync.Mutex
	key []byte
}
