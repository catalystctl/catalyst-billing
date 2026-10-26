-- 0073_themes.sql
--
-- The theme registry: a versioned presentation document per scope, the immutable revisions an
-- operator publishes, the assignment that says which revision each scope serves, and the
-- theme-only asset store (docs/theming-system.md §5).
--
-- # What this is, and what it is deliberately not
--
-- It is the storage for `internal/theme`: one JSON document per revision, resolved once at
-- publish time. It is *not* a second home for tenant identity. `branding.*` (0060) keeps the
-- display name, the logo and the per-mode accent; `mail.brand_*` (0068) keeps the mail header,
-- footer, accent and logo URL; `invoices.pdf_template` keeps the PDF wording. Those are existing,
-- tested contracts with renderers that read them directly, and a theme document that repeated
-- them would drift the first time an operator renamed themselves from /branding.
--
-- # Why revisions are immutable
--
-- A published theme is a promise: the page an operator approved is the page visitors get. If a
-- revision were a mutable row, editing it would silently change what a live site serves, and there
-- would be nothing to roll back to. So a revision is written once; an edit is a new revision, and
-- "rollback" is republishing an old one. `theme_revisions_one_published` enforces at most one
-- published revision per theme at the schema level, which is the constraint that makes
-- "the published revision" a fact rather than a query convention.
--
-- # Why `document` and `source` are both stored
--
-- `source` is what the operator saved. `document` is that, resolved over the parent scope and the
-- built-in default (internal/theme/merge.go). Storing both means:
--
--   * the rendered theme cannot change when a parent scope is republished — a storefront nobody
--     touched keeps serving the colours it served yesterday;
--   * editing a child starts from the child's own values, not from the resolved snapshot, so the
--     editor does not show the parent's tokens as if the operator had typed them.
--
-- # Why the assignment table has one row per scope
--
-- A scope serves exactly one revision. A table keyed on scope makes that a primary key rather than
-- an ordering rule ("latest wins") that every reader has to remember. The composite foreign key
-- into (theme_id, id) is what proves the named revision belongs to the named theme: without it, a
-- bug could point the storefront at the panel's revision and the only symptom would be a wrong
-- palette on one page.
--
-- # Why a separate asset table, and why the tenant logo is not in it
--
-- The existing logo and catalog images live in `product_images` and are served by
-- `/api/v1/media/{id}` with a 256 KiB cap, digest dedupe and an SVG sandbox. That is the right
-- store for a tenant logo, and copying those bytes here would create two answers to "what is the
-- logo". This table exists for the asset classes the catalog cannot represent: a full-width
-- background, a social card, a favicon, a self-hosted font. They are larger (4 MiB), they are
-- served from `/theme/assets/{id}`, and they are referenced from the theme document as
-- `theme-asset:<uuid>` — a reference a CSS `url()` is only allowed to make (see
-- internal/theme/customcss.go), which is what stops a custom stylesheet becoming an
-- exfiltration channel.
--
-- # Why no seed rows
--
-- An installation with no published revision serves the built-in default document
-- (internal/theme/defaults.go), which reproduces the current tokens.css byte for byte. Seeding a
-- baseline revision would mean writing a resolved document into SQL, where it could not be
-- re-derived when a token is added, and it would make "no theme configured" and "the default theme
-- is configured" indistinguishable — so a future change to the default could not reach an
-- installation that had never opened the Theme Studio.
--
-- The consequence is that `theme_assignments` is legitimately empty on a fresh install, and
-- `billing doctor` reports that as information rather than an error.

BEGIN;

-- A theme is the container for one scope's presentation. Revisions hang off it; the container is the
-- stable identity an operator sees in the history list.
--
-- # One row per scope, and why
--
-- The obvious design is one theme row per *document id*, so applying a different preset makes a
-- different theme. It is wrong, and a test caught it: with two theme rows in one scope, "the open
-- draft" stops being a single thing, because each theme has its own draft. Applying Brutalist and
-- then Warm SaaS left two drafts open in the panel scope and the studio had to guess which one "Save"
-- meant.
--
-- A scope serves one presentation. Applying a preset replaces what that presentation *is*, which is a
-- new revision of the same theme, not a new theme. So the row is keyed on the scope, and `slug` and
-- `name` are labels that follow the current revision's document.
CREATE TABLE themes (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        text NOT NULL,
    name        text NOT NULL,
    scope       text NOT NULL CHECK (scope IN ('panel', 'portal', 'storefront', 'email', 'invoice')),
    created_by  uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    deleted_at  timestamptz,
    -- A slug is what a document's `id` is: lower-case letters, digits and hyphens. The constraint
    -- is here as well as in Go because a row written by hand through psql must not be able to
    -- create a theme whose id a document could never reference.
    CONSTRAINT themes_slug_shape CHECK (slug ~ '^[a-z0-9][a-z0-9-]*$' AND length(slug) <= 64),
    CONSTRAINT themes_name_not_blank CHECK (btrim(name) <> '' AND length(name) <= 80)
);
-- One live theme per scope. This is the constraint that makes "the open draft" and "the published
-- revision" single things rather than a query convention.
CREATE UNIQUE INDEX themes_scope_unique ON themes (scope) WHERE deleted_at IS NULL;

COMMENT ON TABLE themes IS
    'One theme container per scope (0073). Revisions are immutable; this row is the stable identity '
    'the history list groups by, and its slug and name follow the current revision. Presentation '
    'only: tenant identity stays in branding.*.';

-- One immutable document per revision.
CREATE TABLE theme_revisions (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    theme_id      uuid NOT NULL REFERENCES themes (id) ON DELETE CASCADE,
    revision      int  NOT NULL CHECK (revision > 0),
    status        text NOT NULL CHECK (status IN ('draft', 'published', 'archived')),
    -- document is the resolved theme (built-in default ← parent scope ← source).
    document      jsonb NOT NULL,
    -- source is what the operator actually saved, so a child can be edited without showing the
    -- parent's tokens as its own.
    source        jsonb NOT NULL,
    digest        text NOT NULL,
    note          text NOT NULL DEFAULT '',
    created_by    uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    published_by  uuid REFERENCES users (id) ON DELETE SET NULL,
    published_at  timestamptz,
    UNIQUE (theme_id, revision),
    -- Lets theme_assignments prove the revision belongs to the theme it names.
    UNIQUE (theme_id, id),
    -- A published revision must record *when* it went live.
    --
    -- It does not require `published_by`, and the asymmetry with `published_at` is deliberate: a
    -- publish can be initiated by the system — a CLI, a migration, a seed — and inventing a user row
    -- to satisfy a NOT NULL would put a fictional person in the history. The authoritative actor is
    -- the audit row (which records `ActorSystem` for those cases); this column is the convenient
    -- copy for the history screen, and it is nullable for exactly the same reason `created_by` is.
    CONSTRAINT theme_revisions_published_complete CHECK (
        status <> 'published' OR published_at IS NOT NULL
    ),
    CONSTRAINT theme_revisions_digest_shape CHECK (digest ~ '^sha256:[0-9a-f]{64}$')
);
CREATE UNIQUE INDEX theme_revisions_one_published
    ON theme_revisions (theme_id) WHERE status = 'published';
CREATE INDEX theme_revisions_theme_idx ON theme_revisions (theme_id, revision DESC);

COMMENT ON TABLE theme_revisions IS
    'One immutable theme document per revision (0073). document is the resolved theme; source is '
    'what the operator saved. At most one published revision per theme, enforced by a partial '
    'unique index.';

-- Which revision each scope currently serves. One row per scope.
CREATE TABLE theme_assignments (
    scope        text PRIMARY KEY CHECK (scope IN ('panel', 'portal', 'storefront', 'email', 'invoice')),
    theme_id     uuid NOT NULL,
    revision_id  uuid NOT NULL,
    updated_by   uuid REFERENCES users (id) ON DELETE SET NULL,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (theme_id, revision_id) REFERENCES theme_revisions (theme_id, id) ON DELETE CASCADE
);

COMMENT ON TABLE theme_assignments IS
    'The revision each scope serves (0073). One row per scope; an absent row means the scope '
    'serves the built-in default document, which is what makes a fresh install look like the '
    'product it replaced.';

-- Uploaded theme-only assets: fonts, full-width backgrounds, a social card, a favicon.
CREATE TABLE theme_assets (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text NOT NULL CHECK (kind IN ('favicon', 'og', 'banner', 'background', 'font', 'other')),
    content_type text NOT NULL CHECK (content_type IN (
                    'image/png', 'image/jpeg', 'image/webp', 'image/svg+xml',
                    'font/woff2', 'font/woff', 'application/octet-stream')),
    bytes        bytea NOT NULL,
    byte_size    int NOT NULL CHECK (byte_size > 0 AND byte_size <= 4194304),
    digest       text NOT NULL,
    width        int,
    height       int,
    alt          text NOT NULL DEFAULT '',
    created_by   uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT theme_assets_size_matches CHECK (byte_size = octet_length(bytes)),
    CONSTRAINT theme_assets_alt_is_short CHECK (length(alt) <= 300)
);
-- The digest index includes kind because one byte payload may serve two roles (the same PNG as a
-- favicon and as a social card), and deduping across roles would make deleting one break the
-- other.
CREATE UNIQUE INDEX theme_assets_digest_unique ON theme_assets (digest, kind);

COMMENT ON TABLE theme_assets IS
    'Theme-only assets (0073): backgrounds, social cards, favicons and self-hosted fonts. The '
    'tenant logo and catalog images stay in product_images and are served by /api/v1/media/{id}; '
    'their bytes are never copied here.';

-- The queryable publication history the UI lists.
--
-- The audit table already records the action; this is the list an operator reads, including the
-- note they wrote and the revision it replaced, which the audit row does not carry in a form a
-- screen can show.
CREATE TABLE theme_publications (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    scope                text NOT NULL CHECK (scope IN ('panel', 'portal', 'storefront', 'email', 'invoice')),
    theme_id             uuid NOT NULL REFERENCES themes (id) ON DELETE CASCADE,
    revision_id          uuid NOT NULL REFERENCES theme_revisions (id) ON DELETE CASCADE,
    previous_revision_id uuid REFERENCES theme_revisions (id) ON DELETE SET NULL,
    published_by         uuid REFERENCES users (id) ON DELETE SET NULL,
    published_at         timestamptz NOT NULL DEFAULT now(),
    note                 text NOT NULL DEFAULT '',
    CONSTRAINT theme_publications_note_is_short CHECK (length(note) <= 500)
);
CREATE INDEX theme_publications_scope_idx ON theme_publications (scope, published_at DESC);

COMMENT ON TABLE theme_publications IS
    'Publication history per scope (0073): who published which revision, when, what it replaced '
    'and why. The audit table has the action; this has the list.';

-- ---------------------------------------------------------------- permissions --
--
-- Three permissions, split by what an operator decides rather than by which screen they appear on:
--
--   theme:read     view the studio, the presets and the history.
--   theme:write    create and edit drafts, upload assets, import.
--   theme:publish  publish, roll back, edit custom CSS, remove branding.
--
-- The split exists because custom CSS can hide staff-critical UI and a publish is what makes a
-- theme live. A merchandiser needs to draft a storefront theme; they should not be able to hide the
-- refund button from every colleague, which is what panel custom CSS can do.
--
-- These are new permissions for the new theme document and publishing flow only. They do NOT widen
-- the existing branding or settings endpoints: `/api/v1/staff/branding` keeps its
-- `settings:write` gate, and mail/PDF settings keep theirs. A theme grant must not silently become
-- the right to rewrite an invoice template.
--
-- The four system roles are immutable by trigger (0026), so this INSERT is the only way their sets
-- can grow — and it is reviewed in a diff, which is the point.
INSERT INTO role_permissions (role_id, permission)
SELECT r.id, seed.permission
FROM roles r
JOIN (VALUES
    ('administrator',  'theme:read'),
    ('administrator',  'theme:write'),
    ('administrator',  'theme:publish'),
    ('billing',        'theme:read'),
    ('billing',        'theme:write'),
    ('support',        'theme:read')
) AS seed(role_name, permission) ON seed.role_name = r.name
WHERE r.is_system
ON CONFLICT DO NOTHING;

COMMIT;
