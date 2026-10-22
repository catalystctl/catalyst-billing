package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/audit"
	"github.com/jackc/pgx/v5"
)

// OAuth2 clients.
//
// A client is a third-party application: it has a public identifier, an optional secret, the
// exact redirect URIs it may be sent to and a ceiling of scopes it may ask for. Registering
// one is an access decision, which is why the route that does it needs `oauth:manage` and
// why every change here is audited inside the transaction that made it.
//
// The secret follows the same rule as an API key: it is generated, hashed, returned exactly
// once and never stored in a readable form. There is no endpoint that reveals it again,
// because there is nothing to reveal.

// Client is a registered OAuth2 application.
type Client struct {
	ID       string
	ClientID string
	Name     string
	// Confidential is true when the client can keep a secret. A public client (a browser, a
	// CLI) cannot, and authenticates with PKCE instead.
	Confidential bool
	// HasSecret reports whether a secret is currently configured. It is what the screen
	// shows; the secret itself is never read back.
	HasSecret     bool
	RedirectURIs  []string
	AllowedScopes []string
	Enabled       bool
	CreatedAt     time.Time
	// SecretRotatedAt is nil until the secret has been rotated once.
	SecretRotatedAt *time.Time
}

// CreatedClient is the one-time result of creating or rotating a secret.
type CreatedClient struct {
	Client
	// Secret is the credential. It is returned once and never stored in this form.
	Secret string
	// ShownOnce is part of the contract, so a UI cannot quietly omit the warning.
	ShownOnce bool
}

// NewClient describes a client to register.
type NewClient struct {
	Name          string
	RedirectURIs  []string
	AllowedScopes []string
	Confidential  bool
	// CreatedBy is the operator's user id, for the audit trail. Empty means a system action.
	CreatedBy string
}

const clientColumns = `id, client_id, name, confidential, secret_hash IS NOT NULL,
	redirect_uris, allowed_scopes, enabled, created_at, secret_rotated_at`

// scanClient reads one row of clientColumns.
func scanClient(row pgx.Row) (Client, error) {
	var c Client
	err := row.Scan(&c.ID, &c.ClientID, &c.Name, &c.Confidential, &c.HasSecret,
		&c.RedirectURIs, &c.AllowedScopes, &c.Enabled, &c.CreatedAt, &c.SecretRotatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Client{}, ErrNotFound
	}
	if err != nil {
		return Client{}, fmt.Errorf("oauth: read client: %w", err)
	}
	return c, nil
}

// CreateClient registers a client and returns its secret exactly once.
func (s *Service) CreateClient(ctx context.Context, in NewClient) (CreatedClient, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return CreatedClient{}, fmt.Errorf("%w: a client needs a name", ErrInvalidInput)
	}
	if len(name) > 120 {
		return CreatedClient{}, fmt.Errorf("%w: the name is too long", ErrInvalidInput)
	}
	redirects, err := normalizeRedirectURIs(in.RedirectURIs)
	if err != nil {
		return CreatedClient{}, err
	}
	scopes, err := normalizeScopes(in.AllowedScopes)
	if err != nil {
		return CreatedClient{}, err
	}
	clientID, err := newClientID()
	if err != nil {
		return CreatedClient{}, err
	}

	// A public client has no secret at all rather than an empty one: the CHECK constraint
	// ties the column to the kind, so "confidential with no secret" cannot be stored.
	var secret string
	var hash []byte
	if in.Confidential {
		secret, err = randomSecret(32)
		if err != nil {
			return CreatedClient{}, err
		}
		secret = clientSecretPrefix + secret
		hash = digest(secret)
	}

	out := CreatedClient{Secret: secret, ShownOnce: in.Confidential}
	var createdBy any
	if in.CreatedBy != "" {
		createdBy = in.CreatedBy
	}

	err = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO oauth_clients (client_id, name, secret_hash, confidential,
				redirect_uris, allowed_scopes, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING `+clientColumns,
			clientID, name, hash, in.Confidential, redirects, scopes, createdBy)
		client, err := scanClient(row)
		if err != nil {
			return err
		}
		out.Client = client
		return s.record(ctx, tx, "oauth.client_created", client.ID, map[string]any{
			"name":          client.Name,
			"client_id":     client.ClientID,
			"confidential":  client.Confidential,
			"redirect_uris": client.RedirectURIs,
			"scopes":        client.AllowedScopes,
		})
	})
	if err != nil {
		return CreatedClient{}, err
	}
	return out, nil
}

// ListClients returns every registered client, newest first.
func (s *Service) ListClients(ctx context.Context) ([]Client, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+clientColumns+` FROM oauth_clients ORDER BY created_at DESC, name`)
	if err != nil {
		return nil, fmt.Errorf("oauth: list clients: %w", err)
	}
	defer rows.Close()

	out := []Client{}
	for rows.Next() {
		c, err := scanClient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ClientByID reads one client by its row id.
func (s *Service) ClientByID(ctx context.Context, id string) (Client, error) {
	return scanClient(s.pool.QueryRow(ctx,
		`SELECT `+clientColumns+` FROM oauth_clients WHERE id = $1`, id))
}

// clientByPublicID reads one client by the identifier that appears in redirects.
//
// It joins nothing else on purpose: a disabled client is returned (with Enabled false) so the
// caller can decide whether the reason is "does not exist" or "turned off", and the authorize
// endpoint reports both to the operator without telling the third party which it was.
func (s *Service) clientByPublicID(ctx context.Context, clientID string) (Client, error) {
	return scanClient(s.pool.QueryRow(ctx,
		`SELECT `+clientColumns+` FROM oauth_clients WHERE client_id = $1`, clientID))
}

// RotateSecret replaces a confidential client's secret and returns the new one once.
//
// Rotation does not revoke outstanding tokens: the secret authenticates the *client* at the
// token endpoint, while a token already issued is the user's grant. Revoking grants is a
// separate, deliberate action (and deleting the client does it), because rotating a leaked
// secret should not sign every customer out of the application.
func (s *Service) RotateSecret(ctx context.Context, id string) (CreatedClient, error) {
	raw, err := randomSecret(32)
	if err != nil {
		return CreatedClient{}, err
	}
	secret := clientSecretPrefix + raw

	out := CreatedClient{Secret: secret, ShownOnce: true}
	err = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE oauth_clients
			SET secret_hash = $2, confidential = true, updated_at = now(),
			    secret_rotated_at = now()
			WHERE id = $1
			RETURNING `+clientColumns, id, digest(secret))
		client, err := scanClient(row)
		if err != nil {
			return err
		}
		out.Client = client
		return s.record(ctx, tx, "oauth.client_secret_rotated", client.ID, map[string]any{
			"client_id": client.ClientID,
		})
	})
	if err != nil {
		return CreatedClient{}, err
	}
	return out, nil
}

// DisableSecret turns a confidential client into a public one.
//
// The secret is destroyed rather than kept-but-ignored: a credential that exists but is not
// checked is the worst of both worlds, because the operator believes it was revoked and a
// later code path could start honouring it again. A public client must use PKCE, which the
// authorize and token endpoints enforce for every client without a secret.
func (s *Service) DisableSecret(ctx context.Context, id string) (Client, error) {
	var out Client
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE oauth_clients
			SET secret_hash = NULL, confidential = false, updated_at = now()
			WHERE id = $1
			RETURNING `+clientColumns, id)
		client, err := scanClient(row)
		if err != nil {
			return err
		}
		out = client
		return s.record(ctx, tx, "oauth.client_secret_disabled", client.ID, map[string]any{
			"client_id": client.ClientID,
		})
	})
	if err != nil {
		return Client{}, err
	}
	return out, nil
}

// DeleteClient removes a client and everything that depends on it.
//
// The foreign keys cascade, so every code, grant and token it issued disappears with it. The
// audit row records the deletion, which is the only trace that survives — that is the
// intended shape: a deleted client is a client whose credentials are gone, not one whose
// credentials are quietly still valid.
func (s *Service) DeleteClient(ctx context.Context, id string) error {
	return s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var clientID string
		var name string
		err := tx.QueryRow(ctx,
			`DELETE FROM oauth_clients WHERE id = $1 RETURNING client_id, name`, id).
			Scan(&clientID, &name)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("oauth: delete client: %w", err)
		}
		return s.record(ctx, tx, "oauth.client_deleted", id, map[string]any{
			"client_id": clientID,
			"name":      name,
		})
	})
}

// record writes an audit row inside the caller's transaction.
//
// House rule: the audit row is written in the transaction that made the change, so a
// committed change always has one and a rolled-back change never does. Without an auditor
// (a CLI or a test constructing the service directly) the change still happens; that is the
// same contract every other service in this repository keeps.
func (s *Service) record(ctx context.Context, tx pgx.Tx, action, targetID string, changes map[string]any) error {
	if s.auditor == nil {
		return nil
	}
	return s.auditor.RecordAction(ctx, tx, action, "oauth_client", targetID, changes)
}

// WithAuditor returns a copy of the service that writes audit rows through a.
func (s *Service) WithAuditor(a audit.Auditor) *Service {
	clone := *s
	clone.auditor = a
	return &clone
}

// normalizeRedirectURIs validates the registered redirect list.
//
// The rules are the ones that keep an authorization code from being delivered to somebody
// else: absolute, http(s), no fragment, no wildcard, and no duplicate. A fragment is refused
// because a redirect URI with a fragment is where a token ends up in a place the referrer
// leaks; `javascript:` and `data:` are refused because they are not destinations, they are
// script.
func normalizeRedirectURIs(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("%w: at least one redirect URI is required", ErrInvalidInput)
	}
	if len(in) > 10 {
		return nil, fmt.Errorf("%w: at most ten redirect URIs are supported", ErrInvalidInput)
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		u, err := url.Parse(value)
		if err != nil || !u.IsAbs() || u.Host == "" {
			return nil, fmt.Errorf("%w: %q is not an absolute URL", ErrInvalidInput, value)
		}
		if u.Scheme != "https" && u.Scheme != "http" {
			return nil, fmt.Errorf("%w: %q must be http or https", ErrInvalidInput, value)
		}
		if u.Fragment != "" || strings.Contains(value, "#") {
			return nil, fmt.Errorf("%w: %q must not carry a fragment", ErrInvalidInput, value)
		}
		if strings.ContainsAny(value, "*") {
			return nil, fmt.Errorf("%w: %q must be an exact URI, not a pattern", ErrInvalidInput, value)
		}
		if _, dup := seen[value]; dup {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: at least one redirect URI is required", ErrInvalidInput)
	}
	return out, nil
}

// normalizeScopes validates a client's allowed-scope ceiling.
func normalizeScopes(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("%w: at least one allowed scope is required", ErrInvalidInput)
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		scope := strings.TrimSpace(raw)
		if scope == "" {
			continue
		}
		if !isKnownPermission(scope) {
			return nil, fmt.Errorf("%w: %q is not a permission this build knows", ErrInvalidInput, scope)
		}
		if _, dup := seen[scope]; dup {
			continue
		}
		seen[scope] = struct{}{}
		out = append(out, scope)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: at least one allowed scope is required", ErrInvalidInput)
	}
	return out, nil
}
