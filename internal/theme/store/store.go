// Package store is the database side of the theme registry.
//
// # The shape of the data
//
// One theme row per (scope, slug); one immutable revision per save; one assignment row per scope
// saying which revision it serves; one publication row per publish, for the history list. The
// reasoning is in the migration's own header (0073_themes.sql) — this package is the code that
// honours it.
//
// # The two invariants this package is responsible for
//
//  1. **At most one open draft per theme.** The studio shows "the draft"; if two existed, which
//     one "Save" wrote to would depend on load order. Creating a draft archives the previous one,
//     inside the same transaction, so the invariant is a state the database is in rather than a
//     rule a handler remembers.
//
//  2. **At most one published revision per theme, and the assignment points at it.** Publishing
//     archives the previous published revision, marks this one published, and updates the
//     assignment, in one transaction. A publish that half-succeeded would leave a scope serving a
//     revision marked archived, which is the state that makes "why is my panel the wrong colour"
//     impossible to answer.
//
// # Why the resolved document is stored, not derived on read
//
// `document` is what the CSS generator reads; `source` is what the operator saved. Storing the
// resolved form is what makes a published theme stable: a parent scope republishing cannot change
// a child that nobody touched. See internal/theme/merge.go.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/catalyst/billing/internal/store"
	"github.com/catalyst/billing/internal/theme"
	"github.com/jackc/pgx/v5"
)

// Scopes is the closed set of presentation scopes, in the order the studio lists them.
//
// It mirrors the CHECK constraint in 0073. A scope outside this set is refused here as well as in
// the schema so that a typo produces a named error rather than a constraint violation.
var Scopes = []string{"panel", "portal", "storefront", "email", "invoice"}

// ScopeDisplay names each scope for the UI. It is here rather than in the frontend so the API and
// the audit log use the same words.
var ScopeDisplay = map[string]string{
	"panel":      "Staff panel",
	"portal":     "Customer portal",
	"storefront": "Storefront",
	"email":      "Transactional email",
	"invoice":    "Invoice PDF",
}

// ValidScope reports whether scope is one of the declared scopes.
func ValidScope(scope string) bool {
	for _, s := range Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Status is a revision's lifecycle state.
type Status string

const (
	StatusDraft     Status = "draft"
	StatusPublished Status = "published"
	StatusArchived  Status = "archived"
)

// Revision is one immutable theme document.
type Revision struct {
	ID       string         `json:"id"`
	ThemeID  string         `json:"theme_id"`
	Scope    string         `json:"scope"`
	Slug     string         `json:"slug"`
	Name     string         `json:"name"`
	Number   int            `json:"number"`
	Status   Status         `json:"status"`
	Document theme.Document `json:"document"`
	Source   theme.Document `json:"source"`
	Digest   string         `json:"digest"`
	Note     string         `json:"note"`

	CreatedAt   time.Time  `json:"created_at"`
	PublishedAt *time.Time `json:"published_at"`
	CreatedBy   *string    `json:"created_by"`
	PublishedBy *string    `json:"published_by"`
}

// Publication is one entry in the history list.
type Publication struct {
	ID                 string    `json:"id"`
	Scope              string    `json:"scope"`
	ThemeID            string    `json:"theme_id"`
	RevisionID         string    `json:"revision_id"`
	RevisionNumber     int       `json:"revision_number"`
	PreviousRevisionID *string   `json:"previous_revision_id"`
	PublishedBy        *string   `json:"published_by"`
	PublishedAt        time.Time `json:"published_at"`
	Note               string    `json:"note"`
}

// Asset is a theme-only image or font.
type Asset struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	ContentType string    `json:"content_type"`
	ByteSize    int       `json:"byte_size"`
	Digest      string    `json:"digest"`
	Width       *int      `json:"width"`
	Height      *int      `json:"height"`
	Alt         string    `json:"alt"`
	CreatedAt   time.Time `json:"created_at"`
	// Usage is how many revisions reference the asset. A delete is refused while it is non-zero,
	// because a published revision's stylesheet would start returning 404 for its own background.
	Usage int `json:"usage"`
	// Bytes is only populated by Asset(), which is what the serving route uses.
	Bytes []byte `json:"-"`
}

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("theme/store: not found")

// ErrNotDraft is returned when an operation needs a draft and the revision is not one.
var ErrNotDraft = errors.New("theme/store: the revision is not a draft")

// ErrAssetInUse is returned when a delete would break a revision that references the asset.
var ErrAssetInUse = errors.New("theme/store: the asset is referenced by a theme revision")

// Store is the database access layer.
type Store struct {
	pool *store.Pool
}

// New constructs a Store.
func New(pool *store.Pool) *Store { return &Store{pool: pool} }

// ------------------------------------------------------------------ reads --

// Published returns the revision a scope currently serves.
//
// A missing assignment is not an error: it means the scope serves the built-in default document,
// which is the state of a fresh installation. Returning (nil, nil) rather than ErrNotFound is what
// lets the caller say "no theme published" without treating it as a failure.
func (s *Store) Published(ctx context.Context, scope string) (*Revision, error) {
	if s.pool == nil {
		return nil, nil
	}
	const q = `
		SELECT r.id, r.theme_id, t.scope, t.slug, t.name, r.revision, r.status,
		       r.document, r.source, r.digest, r.note,
		       r.created_at, r.published_at, r.created_by, r.published_by
		FROM theme_assignments a
		JOIN theme_revisions r ON r.id = a.revision_id
		JOIN themes t ON t.id = r.theme_id
		WHERE a.scope = $1`
	row, err := s.pool.Query(ctx, q, scope)
	if err != nil {
		return nil, fmt.Errorf("theme/store: read the published revision: %w", err)
	}
	defer row.Close()
	if !row.Next() {
		return nil, row.Err()
	}
	rev, err := scanRevision(row)
	if err != nil {
		return nil, err
	}
	return &rev, nil
}

// Revision loads one revision by id.
func (s *Store) Revision(ctx context.Context, id string) (*Revision, error) {
	if s.pool == nil {
		return nil, ErrNotFound
	}
	const q = `
		SELECT r.id, r.theme_id, t.scope, t.slug, t.name, r.revision, r.status,
		       r.document, r.source, r.digest, r.note,
		       r.created_at, r.published_at, r.created_by, r.published_by
		FROM theme_revisions r
		JOIN themes t ON t.id = r.theme_id
		WHERE r.id = $1`
	row, err := s.pool.Query(ctx, q, id)
	if err != nil {
		return nil, fmt.Errorf("theme/store: read a revision: %w", err)
	}
	defer row.Close()
	if !row.Next() {
		return nil, ErrNotFound
	}
	rev, err := scanRevision(row)
	if err != nil {
		return nil, err
	}
	return &rev, nil
}

// ByDigest finds a revision in a scope by its document digest.
//
// It exists for the revision-addressed stylesheet route: a browser holding a shell from before the
// last publish asks for `/theme/<old digest>/theme.css`, and the honest answer is that revision's
// bytes rather than a 404 that would leave the page unstyled. Any status qualifies — an archived
// revision is exactly the case this is for.
func (s *Store) ByDigest(ctx context.Context, scope, digest string) (*Revision, error) {
	if s.pool == nil {
		return nil, nil
	}
	const q = `
		SELECT r.id, r.theme_id, t.scope, t.slug, t.name, r.revision, r.status,
		       r.document, r.source, r.digest, r.note,
		       r.created_at, r.published_at, r.created_by, r.published_by
		FROM theme_revisions r
		JOIN themes t ON t.id = r.theme_id
		WHERE t.scope = $1 AND r.digest = $2
		ORDER BY r.revision DESC
		LIMIT 1`
	row, err := s.pool.Query(ctx, q, scope, digest)
	if err != nil {
		return nil, fmt.Errorf("theme/store: read a revision by digest: %w", err)
	}
	defer row.Close()
	if !row.Next() {
		return nil, row.Err()
	}
	rev, err := scanRevision(row)
	if err != nil {
		return nil, err
	}
	return &rev, nil
}

// Drafts returns the open draft(s) for a scope, newest first.
func (s *Store) Drafts(ctx context.Context, scope string) ([]Revision, error) {
	if s.pool == nil {
		return nil, nil
	}
	const q = `
		SELECT r.id, r.theme_id, t.scope, t.slug, t.name, r.revision, r.status,
		       r.document, r.source, r.digest, r.note,
		       r.created_at, r.published_at, r.created_by, r.published_by
		FROM theme_revisions r
		JOIN themes t ON t.id = r.theme_id
		WHERE t.scope = $1 AND r.status = 'draft'
		ORDER BY r.revision DESC`
	return s.listRevisions(ctx, q, scope)
}

// Revisions returns every revision for a scope, newest first.
func (s *Store) Revisions(ctx context.Context, scope string, limit int) ([]Revision, error) {
	if s.pool == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	const q = `
		SELECT r.id, r.theme_id, t.scope, t.slug, t.name, r.revision, r.status,
		       r.document, r.source, r.digest, r.note,
		       r.created_at, r.published_at, r.created_by, r.published_by
		FROM theme_revisions r
		JOIN themes t ON t.id = r.theme_id
		WHERE t.scope = $1
		ORDER BY r.revision DESC
		LIMIT $2`
	return s.listRevisions(ctx, q, scope, limit)
}

// History returns the publication log for a scope, newest first.
func (s *Store) History(ctx context.Context, scope string, limit int) ([]Publication, error) {
	if s.pool == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	const q = `
		SELECT p.id, p.scope, p.theme_id, p.revision_id, r.revision, p.previous_revision_id,
		       p.published_by, p.published_at, p.note
		FROM theme_publications p
		JOIN theme_revisions r ON r.id = p.revision_id
		WHERE p.scope = $1
		ORDER BY p.published_at DESC, r.revision DESC
		LIMIT $2`
	rows, err := s.pool.Query(ctx, q, scope, limit)
	if err != nil {
		return nil, fmt.Errorf("theme/store: read the publication history: %w", err)
	}
	defer rows.Close()
	var out []Publication
	for rows.Next() {
		var p Publication
		if err := rows.Scan(&p.ID, &p.Scope, &p.ThemeID, &p.RevisionID, &p.RevisionNumber,
			&p.PreviousRevisionID, &p.PublishedBy, &p.PublishedAt, &p.Note); err != nil {
			return nil, fmt.Errorf("theme/store: scan a publication: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// listRevisions runs a revision query and scans every row.
func (s *Store) listRevisions(ctx context.Context, q string, args ...any) ([]Revision, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("theme/store: read revisions: %w", err)
	}
	defer rows.Close()
	var out []Revision
	for rows.Next() {
		rev, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rev)
	}
	return out, rows.Err()
}

// scanRevision reads one row of the revision select.
//
// The two JSONB columns are decoded here rather than by the caller because a document that does not
// decode is a corrupted row, and the error should name the revision that carries it.
func scanRevision(row pgx.Rows) (Revision, error) {
	var (
		rev      Revision
		document []byte
		source   []byte
	)
	if err := row.Scan(&rev.ID, &rev.ThemeID, &rev.Scope, &rev.Slug, &rev.Name, &rev.Number,
		&rev.Status, &document, &source, &rev.Digest, &rev.Note,
		&rev.CreatedAt, &rev.PublishedAt, &rev.CreatedBy, &rev.PublishedBy); err != nil {
		return Revision{}, fmt.Errorf("theme/store: scan a revision: %w", err)
	}
	if err := json.Unmarshal(document, &rev.Document); err != nil {
		return Revision{}, fmt.Errorf("theme/store: revision %s has an unreadable document: %w", rev.ID, err)
	}
	if err := json.Unmarshal(source, &rev.Source); err != nil {
		return Revision{}, fmt.Errorf("theme/store: revision %s has an unreadable source: %w", rev.ID, err)
	}
	return rev, nil
}

// ----------------------------------------------------------------- writes --

// DraftInput is what creating a draft needs.
type DraftInput struct {
	Scope string
	// Source is what the operator saved. It is resolved over the parent scope and the default by
	// the caller (internal/theme.Resolve), which is where the inheritance rule lives.
	Source theme.Document
	// Document is the resolved document. It must already have passed theme.ValidateResolved.
	Document theme.Document
	Note     string
	UserID   string
}

// CreateDraft writes a new draft revision for a scope.
//
// It archives any open draft first: one open draft per theme is the invariant that makes "Save"
// unambiguous. It runs in a transaction so a failure cannot leave a scope with two drafts and no
// way to tell which is current.
func (s *Store) CreateDraft(ctx context.Context, in DraftInput) (Revision, error) {
	if !ValidScope(in.Scope) {
		return Revision{}, fmt.Errorf("theme/store: %q is not a theme scope", in.Scope)
	}
	if err := theme.ValidateResolved(in.Document); err != nil {
		return Revision{}, err
	}
	digest, err := theme.Digest(in.Document)
	if err != nil {
		return Revision{}, err
	}
	sourceJSON, err := json.Marshal(in.Source)
	if err != nil {
		return Revision{}, fmt.Errorf("theme/store: encode the source document: %w", err)
	}
	documentJSON, err := json.Marshal(in.Document)
	if err != nil {
		return Revision{}, fmt.Errorf("theme/store: encode the resolved document: %w", err)
	}

	var out Revision
	err = s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		themeID, err := upsertTheme(ctx, tx, in.Scope, in.Document.ID, in.Document.Name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE theme_revisions SET status = 'archived'
			 WHERE theme_id = $1 AND status = 'draft'`, themeID); err != nil {
			return fmt.Errorf("theme/store: archive the previous draft: %w", err)
		}
		const insert = `
			INSERT INTO theme_revisions
				(theme_id, revision, status, document, source, digest, note, created_by)
			VALUES ($1,
			        COALESCE((SELECT max(revision) FROM theme_revisions WHERE theme_id = $1), 0) + 1,
			        'draft', $2, $3, $4, $5, $6)
			RETURNING id, revision, created_at`
		if err := tx.QueryRow(ctx, insert, themeID, documentJSON, sourceJSON, digest,
			strings.TrimSpace(in.Note), nullString(in.UserID)).Scan(
			&out.ID, &out.Number, &out.CreatedAt); err != nil {
			return fmt.Errorf("theme/store: insert the draft: %w", err)
		}
		out.ThemeID = themeID
		out.Scope = in.Scope
		out.Slug = in.Document.ID
		out.Name = in.Document.Name
		out.Status = StatusDraft
		out.Document = in.Document
		out.Source = in.Source
		out.Digest = digest
		out.Note = strings.TrimSpace(in.Note)
		if in.UserID != "" {
			user := in.UserID
			out.CreatedBy = &user
		}
		return nil
	})
	if err != nil {
		return Revision{}, err
	}
	return out, nil
}

// UpdateDraft replaces a draft's document.
//
// Only a draft may be edited: a published revision is a promise about what a live site serves, and
// editing it would change the site with no publish and nothing to roll back to.
func (s *Store) UpdateDraft(ctx context.Context, id string, in DraftInput) (Revision, error) {
	if err := theme.ValidateResolved(in.Document); err != nil {
		return Revision{}, err
	}
	digest, err := theme.Digest(in.Document)
	if err != nil {
		return Revision{}, err
	}
	sourceJSON, err := json.Marshal(in.Source)
	if err != nil {
		return Revision{}, fmt.Errorf("theme/store: encode the source document: %w", err)
	}
	documentJSON, err := json.Marshal(in.Document)
	if err != nil {
		return Revision{}, fmt.Errorf("theme/store: encode the resolved document: %w", err)
	}

	const q = `
		UPDATE theme_revisions
		SET document = $2, source = $3, digest = $4, note = $5
		WHERE id = $1 AND status = 'draft'
		RETURNING revision`
	var number int
	err = s.pool.QueryRow(ctx, q, id, documentJSON, sourceJSON, digest,
		strings.TrimSpace(in.Note)).Scan(&number)
	if errors.Is(err, pgx.ErrNoRows) {
		// Distinguish "no such revision" from "not a draft": the operator's next action differs.
		if _, findErr := s.Revision(ctx, id); findErr != nil {
			return Revision{}, ErrNotFound
		}
		return Revision{}, ErrNotDraft
	}
	if err != nil {
		return Revision{}, fmt.Errorf("theme/store: update the draft: %w", err)
	}
	// The theme's display name and slug follow the document: applying a preset renames the theme.
	if _, err := s.pool.Exec(ctx,
		`UPDATE themes SET slug = $2, name = $3, updated_at = now()
		 WHERE id = (SELECT theme_id FROM theme_revisions WHERE id = $1)`,
		id, in.Document.ID, in.Document.Name); err != nil {
		return Revision{}, fmt.Errorf("theme/store: rename the theme: %w", err)
	}
	return s.mustRevision(ctx, id)
}

// PublishResult is what a publish produced.
type PublishResult struct {
	Revision    Revision
	Publication Publication
}

// Publish makes a draft live for its scope.
//
// The three writes — archive the previous published revision, mark this one published, point the
// assignment at it — happen in one transaction. Publishing is also idempotent in the sense that
// matters: publishing an already-published revision re-points the assignment and writes a
// publication row, which is what "roll back to revision 3" is.
func (s *Store) Publish(ctx context.Context, id, userID, note string) (PublishResult, error) {
	var out PublishResult
	if s.pool == nil {
		return out, ErrNotFound
	}
	err := s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var (
			themeID  string
			scope    string
			status   Status
			previous *string
		)
		const load = `
			SELECT r.theme_id, t.scope, r.status,
			       (SELECT a.revision_id::text FROM theme_assignments a WHERE a.scope = t.scope)
			FROM theme_revisions r
			JOIN themes t ON t.id = r.theme_id
			WHERE r.id = $1
			FOR UPDATE OF r`
		if err := tx.QueryRow(ctx, load, id).Scan(&themeID, &scope, &status, &previous); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("theme/store: read the revision to publish: %w", err)
		}
		if status == StatusArchived {
			// An archived revision is history. Publishing it is "restore then publish", which is a
			// deliberate two-step: it keeps the history linear and makes the new revision visible.
			return fmt.Errorf("theme/store: an archived revision cannot be published; restore it as a draft first")
		}
		if _, err := tx.Exec(ctx,
			`UPDATE theme_revisions SET status = 'archived'
			 WHERE theme_id = $1 AND status = 'published' AND id <> $2`, themeID, id); err != nil {
			return fmt.Errorf("theme/store: archive the previous published revision: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE theme_revisions
			 SET status = 'published', published_at = now(), published_by = $2
			 WHERE id = $1`, id, nullString(userID)); err != nil {
			return fmt.Errorf("theme/store: mark the revision published: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO theme_assignments (scope, theme_id, revision_id, updated_by)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (scope) DO UPDATE
			SET theme_id = EXCLUDED.theme_id, revision_id = EXCLUDED.revision_id,
			    updated_by = EXCLUDED.updated_by, updated_at = now()`,
			scope, themeID, id, nullString(userID)); err != nil {
			return fmt.Errorf("theme/store: point the scope at the revision: %w", err)
		}
		const record = `
			INSERT INTO theme_publications
				(scope, theme_id, revision_id, previous_revision_id, published_by, note)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING id, published_at`
		var pub Publication
		if err := tx.QueryRow(ctx, record, scope, themeID, id, previous,
			nullString(userID), strings.TrimSpace(note)).Scan(&pub.ID, &pub.PublishedAt); err != nil {
			return fmt.Errorf("theme/store: record the publication: %w", err)
		}
		pub.Scope = scope
		pub.ThemeID = themeID
		pub.RevisionID = id
		pub.PreviousRevisionID = previous
		pub.Note = strings.TrimSpace(note)
		if userID != "" {
			user := userID
			pub.PublishedBy = &user
		}
		out.Publication = pub
		return nil
	})
	if err != nil {
		return out, err
	}
	rev, err := s.Revision(ctx, id)
	if err != nil {
		return out, err
	}
	out.Revision = *rev
	return out, nil
}

// Restore copies an old revision's source into a new draft.
//
// It copies the *source*, not the resolved document: restoring revision 3 of a storefront theme
// should start from what the operator wrote then and re-resolve against today's parent, not freeze
// the parent as it was.
func (s *Store) Restore(ctx context.Context, id, userID string) (Revision, error) {
	old, err := s.Revision(ctx, id)
	if err != nil {
		return Revision{}, err
	}
	resolved, err := theme.Resolve(theme.Default(), old.Source)
	if err != nil {
		return Revision{}, err
	}
	return s.CreateDraft(ctx, DraftInput{
		Scope:    old.Scope,
		Source:   old.Source,
		Document: resolved,
		Note:     fmt.Sprintf("restored from revision %d", old.Number),
		UserID:   userID,
	})
}

// ---------------------------------------------------------------- assets --

// AssetInput is an uploaded theme asset.
type AssetInput struct {
	Kind        string
	ContentType string
	Bytes       []byte
	Digest      string
	Width       *int
	Height      *int
	Alt         string
	UserID      string
}

// PutAsset stores an asset, deduping on (digest, kind).
//
// Deduping means uploading the same social card twice returns the existing row rather than a second
// copy — and it is why the digest index includes the kind: the same PNG used as a favicon and as a
// banner is two roles, and deleting one must not break the other.
func (s *Store) PutAsset(ctx context.Context, in AssetInput) (Asset, error) {
	if s.pool == nil {
		return Asset{}, errors.New("theme/store: no database is configured")
	}
	const q = `
		INSERT INTO theme_assets (kind, content_type, bytes, byte_size, digest, width, height, alt, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (digest, kind) DO UPDATE SET alt = COALESCE(NULLIF(EXCLUDED.alt, ''), theme_assets.alt)
		RETURNING id, kind, content_type, byte_size, digest, width, height, alt, created_at`
	var a Asset
	if err := s.pool.QueryRow(ctx, q, in.Kind, in.ContentType, in.Bytes, len(in.Bytes), in.Digest,
		in.Width, in.Height, strings.TrimSpace(in.Alt), nullString(in.UserID)).Scan(
		&a.ID, &a.Kind, &a.ContentType, &a.ByteSize, &a.Digest, &a.Width, &a.Height, &a.Alt,
		&a.CreatedAt); err != nil {
		return Asset{}, fmt.Errorf("theme/store: store the asset: %w", err)
	}
	return a, nil
}

// Asset loads one asset including its bytes.
func (s *Store) Asset(ctx context.Context, id string) (Asset, error) {
	if s.pool == nil {
		return Asset{}, ErrNotFound
	}
	const q = `
		SELECT id, kind, content_type, byte_size, digest, width, height, alt, created_at, bytes
		FROM theme_assets WHERE id = $1`
	var a Asset
	if err := s.pool.QueryRow(ctx, q, id).Scan(&a.ID, &a.Kind, &a.ContentType, &a.ByteSize,
		&a.Digest, &a.Width, &a.Height, &a.Alt, &a.CreatedAt, &a.Bytes); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Asset{}, ErrNotFound
		}
		return Asset{}, fmt.Errorf("theme/store: read the asset: %w", err)
	}
	return a, nil
}

// Assets lists the library, with a usage count per asset.
//
// The count is computed by searching the revisions' JSON for the asset id, which is exact because
// the only way an asset is referenced is as `theme-asset:<uuid>` inside a document.
func (s *Store) Assets(ctx context.Context) ([]Asset, error) {
	if s.pool == nil {
		return nil, nil
	}
	const q = `
		SELECT a.id, a.kind, a.content_type, a.byte_size, a.digest, a.width, a.height, a.alt,
		       a.created_at,
		       (SELECT count(*) FROM theme_revisions r
		        WHERE r.document::text LIKE '%' || a.id::text || '%') AS usage
		FROM theme_assets a
		ORDER BY a.created_at DESC`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("theme/store: list assets: %w", err)
	}
	defer rows.Close()
	var out []Asset
	for rows.Next() {
		var a Asset
		if err := rows.Scan(&a.ID, &a.Kind, &a.ContentType, &a.ByteSize, &a.Digest, &a.Width,
			&a.Height, &a.Alt, &a.CreatedAt, &a.Usage); err != nil {
			return nil, fmt.Errorf("theme/store: scan an asset: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAsset removes an asset that no revision references.
func (s *Store) DeleteAsset(ctx context.Context, id string) error {
	if s.pool == nil {
		return ErrNotFound
	}
	return s.pool.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var usage int
		const count = `
			SELECT (SELECT count(*) FROM theme_revisions r
			        WHERE r.document::text LIKE '%' || $1 || '%')`
		if err := tx.QueryRow(ctx, count, id).Scan(&usage); err != nil {
			return fmt.Errorf("theme/store: count asset usage: %w", err)
		}
		if usage > 0 {
			return ErrAssetInUse
		}
		tag, err := tx.Exec(ctx, `DELETE FROM theme_assets WHERE id = $1`, id)
		if err != nil {
			return fmt.Errorf("theme/store: delete the asset: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ---------------------------------------------------------------- helpers --

// upsertTheme finds or creates the one theme row for a scope.
//
// The conflict target is the scope, not the slug: a scope serves one presentation, and applying a
// different preset is a new revision of that presentation rather than a second theme. Keying on the
// slug instead left two open drafts in one scope the moment an operator tried a second preset, and the
// studio then had to guess which one "Save" meant.
//
// The slug and name are updated from the incoming document because they are labels for the gallery and
// the history, and they should say what the theme currently is.
func upsertTheme(ctx context.Context, tx pgx.Tx, scope, slug, name string) (string, error) {
	const q = `
		INSERT INTO themes (slug, name, scope)
		VALUES ($1, $2, $3)
		ON CONFLICT (scope) WHERE deleted_at IS NULL
		DO UPDATE SET slug = EXCLUDED.slug, name = EXCLUDED.name, updated_at = now()
		RETURNING id`
	var id string
	if err := tx.QueryRow(ctx, q, slug, name, scope).Scan(&id); err != nil {
		return "", fmt.Errorf("theme/store: upsert the theme: %w", err)
	}
	return id, nil
}

// mustRevision loads a revision that was just written.
func (s *Store) mustRevision(ctx context.Context, id string) (Revision, error) {
	rev, err := s.Revision(ctx, id)
	if err != nil {
		return Revision{}, err
	}
	return *rev, nil
}

// nullString turns "" into SQL NULL.
//
// It matters for the `created_by`/`published_by` columns: they are foreign keys into users, and a
// system-initiated publish (a migration, a seed) has no user. An empty string would be a foreign
// key violation rather than "no author".
func nullString(v string) *string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return &v
}
