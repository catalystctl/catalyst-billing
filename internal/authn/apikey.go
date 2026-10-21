package authn

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// API keys.
//
// A key is a bearer credential for automation: a CI job that reads invoices, a script that
// reconciles payments. The design constraints are different from a session's:
//
//   - It is long-lived, so it must be revocable and it must be visible. The list shows the
//     prefix, the scopes, when it was last used and when it expires, because "which key is this
//     and is anything still using it" is the question an operator asks before revoking one.
//   - It is shown exactly once. Storing the key would make a database leak a credential leak, so
//     only a SHA-256 digest and a short prefix are kept. SHA-256 is right here for the same
//     reason it is right for a session token: the key is 256 bits of server randomness.
//   - It can only narrow its owner's permissions. The key resolves to the owning user, and the
//     scopes are intersected with that user's roles at request time. Revoking a role therefore
//     immediately narrows every key that user owns, which a separate permission table could not
//     promise.
//   - `last_used_at` is updated at most once a minute. Updating it on every request would turn a
//     read endpoint into a write endpoint and make every GET contend on one row.

// APIKey errors.
var (
	// ErrAPIKeyNotFound is returned for an unknown, revoked or expired key.
	ErrAPIKeyNotFound = errors.New("authn: no such api key")
)

// APIKey is a stored key, without its secret.
type APIKey struct {
	ID         string
	UserID     string
	Name       string
	Prefix     string
	Scopes     []string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
}

// Revoked reports whether the key can no longer authenticate.
func (k APIKey) Revoked() bool { return k.RevokedAt != nil }

// Expired reports whether the key's expiry has passed.
func (k APIKey) Expired(now time.Time) bool {
	return k.ExpiresAt != nil && !now.Before(*k.ExpiresAt)
}

// NewAPIKeyRequest describes a key to create.
type NewAPIKeyRequest struct {
	UserID string
	Name   string
	Scopes []string
	// ExpiresIn is optional. Zero means the key does not expire, which an operator may
	// legitimately want for a long-running integration but must choose explicitly.
	ExpiresIn time.Duration
}

// CreatedAPIKey is the one-time result: the key itself is here and nowhere else.
type CreatedAPIKey struct {
	APIKey
	// Key is the credential. It is returned once and never stored.
	Key string
}

// apiKeyPrefixLen is how many characters of the key are stored for display. Eight is enough to
// distinguish keys in a list and not enough to help guess one.
const apiKeyPrefixLen = 8

// CreateAPIKey issues a key.
//
// The format is `bk_` followed by 43 base64url characters, which is 256 bits. The prefix is
// stored separately so the list can show something recognisable without showing the key.
func (s *Service) CreateAPIKey(ctx context.Context, req NewAPIKeyRequest) (CreatedAPIKey, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return CreatedAPIKey{}, errors.New("authn: an api key needs a name")
	}
	if len(name) > 120 {
		return CreatedAPIKey{}, errors.New("authn: the api key name is too long")
	}
	for _, scope := range req.Scopes {
		if !IsKnownPermission(scope) {
			return CreatedAPIKey{}, fmt.Errorf("authn: %q is not a permission this build knows", scope)
		}
	}

	secret, err := newToken(32)
	if err != nil {
		return CreatedAPIKey{}, err
	}
	key := "bk_" + secret
	digest := sha256.Sum256([]byte(key))

	// A nil slice is stored as NULL, and the column is NOT NULL. An empty scope set means "no
	// narrowing", so the empty slice is the correct representation of "the caller did not narrow".
	scopes := req.Scopes
	if scopes == nil {
		scopes = []string{}
	}

	var expiresAt *time.Time
	if req.ExpiresIn > 0 {
		when := s.cfg.Now().Add(req.ExpiresIn)
		expiresAt = &when
	}

	out := CreatedAPIKey{Key: key}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO api_keys (user_id, name, prefix, key_hash, scopes, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, user_id, name, prefix, scopes, created_at, expires_at`,
		req.UserID, name, key[:apiKeyPrefixLen], digest[:], scopes, expiresAt).
		Scan(&out.ID, &out.UserID, &out.Name, &out.Prefix, &out.Scopes, &out.CreatedAt,
			&out.ExpiresAt)
	if err != nil {
		return CreatedAPIKey{}, fmt.Errorf("authn: create api key: %w", err)
	}
	return out, nil
}

// ListAPIKeys returns a user's keys, newest first. It never returns a key.
func (s *Service) ListAPIKeys(ctx context.Context, userID string) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, name, prefix, scopes, created_at, last_used_at, expires_at, revoked_at
		FROM api_keys WHERE user_id = $1
		ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("authn: list api keys: %w", err)
	}
	defer rows.Close()

	out := []APIKey{}
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.Prefix, &k.Scopes, &k.CreatedAt,
			&k.LastUsedAt, &k.ExpiresAt, &k.RevokedAt); err != nil {
			return nil, fmt.Errorf("authn: scan api key: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKey revokes a key the caller owns.
//
// Ownership is part of the WHERE clause rather than a separate check: a key belonging to someone
// else must be indistinguishable from a nonexistent one.
func (s *Service) RevokeAPIKey(ctx context.Context, userID, keyID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE api_keys SET revoked_at = now()
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, keyID, userID)
	if err != nil {
		return fmt.Errorf("authn: revoke api key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAPIKeyNotFound
	}
	return nil
}

// AuthenticateAPIKey resolves a bearer key to its owner.
//
// It returns the key so the caller can intersect the scopes with the owner's permissions. A
// revoked or expired key is ErrAPIKeyNotFound: the caller must not be told which, because the
// distinction only helps someone probing.
func (s *Service) AuthenticateAPIKey(ctx context.Context, presented string) (APIKey, error) {
	presented = strings.TrimSpace(presented)
	if !strings.HasPrefix(presented, "bk_") || len(presented) < apiKeyPrefixLen {
		return APIKey{}, ErrAPIKeyNotFound
	}
	digest := sha256.Sum256([]byte(presented))

	var k APIKey
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, name, prefix, scopes, created_at, last_used_at, expires_at, revoked_at
		FROM api_keys WHERE key_hash = $1`, digest[:]).
		Scan(&k.ID, &k.UserID, &k.Name, &k.Prefix, &k.Scopes, &k.CreatedAt, &k.LastUsedAt,
			&k.ExpiresAt, &k.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIKey{}, ErrAPIKeyNotFound
	}
	if err != nil {
		return APIKey{}, fmt.Errorf("authn: read api key: %w", err)
	}
	if k.Revoked() || k.Expired(s.cfg.Now()) {
		return APIKey{}, ErrAPIKeyNotFound
	}

	// Touch last_used_at at most once a minute. A read endpoint that writes on every request
	// makes every GET contend on one row, which is how a monitoring script takes the panel down.
	if k.LastUsedAt == nil || s.cfg.Now().Sub(*k.LastUsedAt) > time.Minute {
		if _, err := s.pool.Exec(ctx,
			`UPDATE api_keys SET last_used_at = now() WHERE id = $1`, k.ID); err != nil {
			// A failed touch must not fail the request: the key is valid, and the metadata is
			// not worth rejecting a legitimate call over.
			_ = err
		}
	}
	return k, nil
}

// IsKnownPermission reports whether a string names a permission this build declares.
//
// An unknown scope is refused at creation time rather than stored and ignored: a key that claims
// a scope nobody honours is a key whose owner believes it can do something it cannot.
func IsKnownPermission(name string) bool {
	for _, p := range AllPermissions() {
		if string(p) == name {
			return true
		}
	}
	return false
}
