package marketplace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/store"
	"github.com/jackc/pgx/v5"
)

// Persistence for the marketplace: the configuration, the cached index and the installed
// extensions with their verified artifacts.
//
// The index is cached in the database for the same reason the update check is: a fetch on
// every page load is a denial of service against the index host, and a multi-process
// deployment would otherwise fetch once per process.

// Config is the marketplace configuration.
type Config struct {
	// IndexURL is where the listing is fetched from. Empty means the marketplace is not
	// configured, which the screen renders as a state rather than as an empty grid.
	IndexURL string
	// InstallDir is where a verified artifact is staged. Empty means the verified bytes live
	// in the database only, which is still a real install.
	InstallDir string
	UpdatedAt  time.Time
}

// Store is the database side of the marketplace.
type Store struct {
	pool *store.Pool
	// Now is injectable so a test can age a cache entry without sleeping.
	Now func() time.Time
}

// New constructs a Store.
func New(pool *store.Pool) *Store {
	return &Store{pool: pool, Now: func() time.Time { return time.Now().UTC() }}
}

// Config reads the singleton configuration row, creating it when it is absent.
//
// The same rule `internal/settings` applies to a missing setting applies here: the row is a
// default an operator edits, so an installation that has never been configured reads as "no
// index URL" rather than as a broken database.
func (s *Store) Config(ctx context.Context) (Config, error) {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO marketplace_config (id) VALUES (true) ON CONFLICT (id) DO NOTHING`); err != nil {
		return Config{}, fmt.Errorf("marketplace: ensure config: %w", err)
	}
	var out Config
	err := s.pool.QueryRow(ctx,
		`SELECT index_url, install_dir, updated_at FROM marketplace_config WHERE id`).
		Scan(&out.IndexURL, &out.InstallDir, &out.UpdatedAt)
	if err != nil {
		return Config{}, fmt.Errorf("marketplace: read config: %w", err)
	}
	out.UpdatedAt = out.UpdatedAt.UTC()
	return out, nil
}

// SetConfig writes the configuration inside a caller's transaction.
//
// It takes a transaction because the audit row for the change must commit with it: a
// rolled-back configuration change must not leave an audit row claiming it happened.
func (s *Store) SetConfig(ctx context.Context, tx pgx.Tx, indexURL, installDir, actorID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO marketplace_config (id, index_url, install_dir, updated_at, updated_by)
		VALUES (true, $1, $2, now(), $3)
		ON CONFLICT (id) DO UPDATE
			SET index_url = EXCLUDED.index_url,
			    install_dir = EXCLUDED.install_dir,
			    updated_at = now(),
			    updated_by = EXCLUDED.updated_by`,
		strings.TrimSpace(indexURL), strings.TrimSpace(installDir), nullableUUID(actorID))
	if err != nil {
		return fmt.Errorf("marketplace: write config: %w", err)
	}
	return nil
}

// CachedIndex is the cached listing.
type CachedIndex struct {
	Index     Index
	SourceURL string
	FetchedAt time.Time
	// FailureCode is set when the last fetch failed. It is non-empty instead of an Index.
	FailureCode string
	// Cached is always true: a CachedIndex came from the database.
	Cached bool
	// ExpiresAt is when this answer stops being believed.
	ExpiresAt *time.Time
}

// Fresh reports whether the cached listing may be used without a new fetch.
func (c CachedIndex) Fresh(now time.Time) bool {
	if c.ExpiresAt == nil {
		return false
	}
	return now.Before(*c.ExpiresAt)
}

// IndexTTL is how long a cached listing is believed: the failure TTL for a failed fetch.
func (c CachedIndex) IndexTTL() time.Duration {
	if c.FailureCode != "" {
		return IndexFailureTTL
	}
	return IndexCacheTTL
}

// LoadIndex answers "what is in the marketplace", using the cache unless force is set.
//
// A failed fetch is cached briefly and reported as a failure rather than an error, so a
// broken index makes the screen say "the index could not be read" instead of failing to
// render at all.
func (s *Store) LoadIndex(ctx context.Context, sourceURL string, force bool, fetcher *Fetcher) (CachedIndex, error) {
	sourceURL = strings.TrimSpace(sourceURL)
	if sourceURL == "" {
		return CachedIndex{}, &IndexError{
			Code:    CodeNotConfigured,
			Message: "no marketplace index URL is configured",
		}
	}
	now := s.Now()

	if !force {
		cached, ok, err := s.cachedIndex(ctx, sourceURL)
		if err != nil {
			return CachedIndex{}, err
		}
		if ok && cached.Fresh(now) {
			return cached, nil
		}
	}

	if fetcher == nil {
		fetcher = NewFetcher()
	}
	index, err := fetcher.Fetch(ctx, sourceURL)
	if err != nil {
		code, message := asIndexError(err)
		out := CachedIndex{
			SourceURL:   sourceURL,
			FetchedAt:   now,
			FailureCode: code,
			Cached:      true,
		}
		expires := now.Add(out.IndexTTL())
		out.ExpiresAt = &expires
		if saveErr := s.saveIndexFailure(ctx, sourceURL, code, now); saveErr != nil {
			return out, saveErr
		}
		_ = message
		return out, nil
	}

	out := CachedIndex{Index: index, SourceURL: sourceURL, FetchedAt: now, Cached: true}
	expires := now.Add(out.IndexTTL())
	out.ExpiresAt = &expires
	if err := s.saveIndex(ctx, sourceURL, index, now); err != nil {
		return CachedIndex{}, err
	}
	return out, nil
}

// cachedIndex reads the cached listing when it is about the same URL.
func (s *Store) cachedIndex(ctx context.Context, sourceURL string) (CachedIndex, bool, error) {
	var (
		out     CachedIndex
		payload []byte
	)
	err := s.pool.QueryRow(ctx,
		`SELECT source_url, fetched_at, payload, failure_code FROM marketplace_index WHERE id`).
		Scan(&out.SourceURL, &out.FetchedAt, &payload, &out.FailureCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return CachedIndex{}, false, nil
	}
	if err != nil {
		return CachedIndex{}, false, fmt.Errorf("marketplace: read index cache: %w", err)
	}
	if out.SourceURL != sourceURL {
		// A different URL means a different marketplace: the cached listing is about
		// somewhere else and must not be shown.
		return CachedIndex{}, false, nil
	}
	out.FetchedAt = out.FetchedAt.UTC()
	out.Cached = true
	expires := out.FetchedAt.Add(out.IndexTTL())
	out.ExpiresAt = &expires

	if out.FailureCode == "" && len(payload) > 0 {
		index, err := ParseIndex(payload)
		if err != nil {
			// A stored payload that no longer parses (a hand-edited row, a schema change) is
			// treated as no cache rather than as a failure: refetching is the recovery.
			return CachedIndex{}, false, nil
		}
		out.Index = index
	}
	return out, true, nil
}

// saveIndex stores a fetched listing.
func (s *Store) saveIndex(ctx context.Context, sourceURL string, index Index, at time.Time) error {
	payload, err := encodeIndex(index)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO marketplace_index (id, source_url, fetched_at, payload, failure_code)
		VALUES (true, $1, $2, $3::jsonb, '')
		ON CONFLICT (id) DO UPDATE
			SET source_url = EXCLUDED.source_url,
			    fetched_at = EXCLUDED.fetched_at,
			    payload = EXCLUDED.payload,
			    failure_code = ''`, sourceURL, at, string(payload))
	if err != nil {
		return fmt.Errorf("marketplace: write index cache: %w", err)
	}
	return nil
}

// saveIndexFailure records a failed fetch without discarding the last good listing's URL.
func (s *Store) saveIndexFailure(ctx context.Context, sourceURL, code string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO marketplace_index (id, source_url, fetched_at, payload, failure_code)
		VALUES (true, $1, $2, NULL, $3)
		ON CONFLICT (id) DO UPDATE
			SET source_url = EXCLUDED.source_url,
			    fetched_at = EXCLUDED.fetched_at,
			    payload = NULL,
			    failure_code = EXCLUDED.failure_code`, sourceURL, at, code)
	if err != nil {
		return fmt.Errorf("marketplace: write index failure: %w", err)
	}
	return nil
}

// Installed is one installed extension as a read path sees it.
type Installed struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Publisher   string    `json:"publisher"`
	Description string    `json:"description"`
	Kind        string    `json:"kind"`
	SHA256      string    `json:"sha256"`
	Pinned      bool      `json:"pinned"`
	SourceURL   string    `json:"source_url"`
	SizeBytes   int64     `json:"size_bytes"`
	StagedPath  string    `json:"staged_path"`
	InstalledAt time.Time `json:"installed_at"`
	// Status is always "verified". It is stated rather than implied so a client cannot
	// render "running": the core has no way to start an extension it was not configured for.
	Status string `json:"status"`
	// Enabled is the operator's answer to "should the deployment use this?". It is separate from
	// "verified" and from "the environment variable points at it": an installation may keep an
	// artifact installed (for rollback) while switched off, and a screen that could not show that
	// difference left a support engineer guessing which of four gateways was live (ADM-12).
	Enabled bool `json:"enabled"`
}

// ListInstalled returns the installed extensions, newest first.
func (s *Store) ListInstalled(ctx context.Context) ([]Installed, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, version, publisher, description, kind, sha256,
		       pinned_sha256 IS NOT NULL, source_url, size_bytes, staged_path, installed_at,
		       enabled
		FROM installed_extensions
		ORDER BY installed_at DESC, name`)
	if err != nil {
		return nil, fmt.Errorf("marketplace: list installed: %w", err)
	}
	defer rows.Close()

	out := []Installed{}
	for rows.Next() {
		var item Installed
		if err := rows.Scan(&item.Name, &item.Version, &item.Publisher, &item.Description,
			&item.Kind, &item.SHA256, &item.Pinned, &item.SourceURL, &item.SizeBytes,
			&item.StagedPath, &item.InstalledAt, &item.Enabled); err != nil {
			return nil, fmt.Errorf("marketplace: scan installed: %w", err)
		}
		item.InstalledAt = item.InstalledAt.UTC()
		item.Status = "verified"
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("marketplace: list installed: %w", err)
	}
	return out, nil
}

// installedDigest returns the digest of an installed name and version, or "" when it is not
// installed.
func (s *Store) installedDigest(ctx context.Context, name, version string) (string, error) {
	var digest string
	err := s.pool.QueryRow(ctx,
		`SELECT sha256 FROM installed_extensions WHERE name = $1 AND version = $2`,
		name, version).Scan(&digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("marketplace: read installed digest: %w", err)
	}
	return digest, nil
}

// recordInstall writes the installed row and its verified artifact.
//
// Both happen in one transaction so a row can never claim an install whose bytes were not
// stored: the artifact is what makes the digest check reproducible, and an "installed"
// extension whose artifact is missing is exactly the fake state this feature must not show.
func (s *Store) recordInstall(ctx context.Context, ext Extension, result InstallResult, body []byte, actorID string) error {
	return s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var id string
		err := tx.QueryRow(ctx, `
			INSERT INTO installed_extensions
			    (name, version, publisher, description, kind, sha256, pinned_sha256,
			     source_url, size_bytes, staged_path, installed_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (name, version) DO UPDATE
				SET publisher = EXCLUDED.publisher,
				    description = EXCLUDED.description,
				    kind = EXCLUDED.kind,
				    staged_path = EXCLUDED.staged_path,
				    installed_by = EXCLUDED.installed_by,
				    installed_at = now()
			RETURNING id`,
			ext.Name, ext.Version, ext.Publisher, ext.Description, ext.Kind, result.SHA256,
			pinnedValue(result), ext.DownloadURL, result.SizeBytes, result.StagedPath,
			nullableUUID(actorID)).Scan(&id)
		if err != nil {
			return fmt.Errorf("marketplace: record install: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO installed_extension_artifacts (extension_id, sha256, content)
			VALUES ($1, $2, $3)
			ON CONFLICT (extension_id) DO UPDATE
				SET sha256 = EXCLUDED.sha256, content = EXCLUDED.content`,
			id, result.SHA256, body); err != nil {
			return fmt.Errorf("marketplace: store artifact: %w", err)
		}
		return nil
	})
}

// pinnedValue renders the pin as SQL NULL when the install was not pinned.
func pinnedValue(result InstallResult) any {
	if !result.Pinned {
		return nil
	}
	return result.SHA256
}

// Artifact returns the stored bytes and digest for an installed extension.
//
// It exists so the operator can take the exact artifact that was verified and deploy it. The
// digest is returned alongside so a client can check what it received.
func (s *Store) Artifact(ctx context.Context, name, version string) ([]byte, string, error) {
	var (
		body   []byte
		digest string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT a.content, a.sha256
		FROM installed_extensions e
		JOIN installed_extension_artifacts a ON a.extension_id = e.id
		WHERE e.name = $1 AND e.version = $2`, name, version).Scan(&body, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("marketplace: read artifact: %w", err)
	}
	return body, digest, nil
}

// asIndexError returns the stable code and message of a fetch error.
func asIndexError(err error) (code, message string) {
	var ie *IndexError
	if errors.As(err, &ie) {
		return ie.Code, ie.Message
	}
	return CodeIndexUnreachable, "the marketplace index could not be read"
}

// encodeIndex renders an index to its stored JSON.
func encodeIndex(index Index) ([]byte, error) {
	body, err := json.Marshal(index)
	if err != nil {
		return nil, fmt.Errorf("marketplace: encode index: %w", err)
	}
	return body, nil
}

// nullableUUID renders an empty actor id as SQL NULL, because the columns are uuid
// references and "" is not a uuid.
func nullableUUID(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
