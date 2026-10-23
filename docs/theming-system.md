# Centralized theming system — design and implementation plan

**Status:** proposed
**Scope:** `catalyst-billing` (Go core + embedded TypeScript frontend)
**Goal:** one theming system that an operator can use to (a) re-skin the entire panel and
storefront from a preset, (b) upload their own logo, favicon, social card, banners and fonts,
(c) remove every trace of Catalyst branding, (d) write custom CSS, and (e) recompose the
storefront layout — without a fork, a rebuild, or a CSP exemption.

**Decisions requested before implementation:**

1. Confirm the default for new installations: `Catalyst Console` (game-panel match, rose
   `#ff3d7f` on `#0e1116`) or `Catalyst Billing` (current tan/orange brand). This plan
   recommends `Catalyst Console` for new installs and `Catalyst Billing` for upgrades.
2. Confirm whether per-product SEO/OG (server rendering of the storefront) is in scope.
3. Confirm whether HTML email is in scope, or only a themed logo/footer on plain text.
4. Confirm whether a free-form custom HTML block is required (deliberately excluded in v1).

**How to read the file references.** Paths and symbol names are authoritative; line numbers
are indicative and drift as the code changes. When a line number and a symbol disagree,
trust the symbol.

---

## 1. Executive summary

The game panel already ships a theme settings screen: a name, a logo URL, a favicon URL,
three brand colours, a dozen surface colours per mode, a radius, a custom-CSS box and nine
presets. It is a good first version, but it is a *recolouring* system: it cannot upload an
asset, cannot brand email or invoices, cannot theme the storefront differently from the admin
panel, cannot change layout, and its custom CSS is filtered by a regex blacklist.

Billing should not copy that design. It should implement the same mental model — **a theme is
a versioned document; a preset is a document you can start from** — and then extend it along
the axes the game panel left out:

| Capability | Game panel | Billing (this plan) |
|---|---|---|
| Brand name / logo / favicon | URL fields | Uploaded assets with digests, light/dark variants, alt text |
| Social card (OG) | — | Generated and themed, per scope |
| Banners / backgrounds | — | Asset library + background tokens |
| Colours | 3 brand + 12 surfaces × 2 modes | Full token contract: brand, semantic, surfaces, text, borders, overlays, focus |
| Typography | — | Font families, scale, weights, tracking; self-hosted fonts |
| Density / shape / elevation / motion | Radius only | Full knobs, with presets that differ structurally, not just by hue |
| Presets | 9 hue variations | 10 genuinely different designs, including one that matches the game panel |
| Scopes | One | Panel, portal, storefront, email, invoice — with inheritance |
| Layout | — | Section-based page composition + shell variants |
| Custom CSS | Regex blacklist, `<style>` tag | Parsed and validated, served as a first-party stylesheet |
| History | — | Immutable revisions, diff, preview, publish, rollback |
| Branding removal | Manual, incomplete | One action + a checklist that proves nothing remains |
| Security | `'unsafe-inline'`-class exposure risk | Strict CSP preserved end to end |

The single most important architectural decision: **theme CSS and theme boot code are
first-party HTTP resources, not inline markup.** `web/web.go` enforces
`script-src 'self'; style-src 'self'` with no `'unsafe-inline'`. That policy is a feature —
the panel renders customer-supplied text — and this plan keeps it. A `<style>` tag full of
admin CSS, or an inline pre-paint script, would force the policy open. Instead:

* `GET /theme/<revision>/theme.css` — generated tokens + preset CSS + validated custom CSS.
* `GET /theme/<revision>/boot.js` — a tiny pre-paint script for personal preferences.
* `GET /theme/assets/<digest>` — uploaded images and fonts, immutable.

Both are same-origin, both are versioned by revision, and both are cacheable forever because
the URL changes when the bytes change. The HTML shell is rendered by Go (not static) so the
title, meta/OG tags, favicon and manifest links are theme-driven on the first byte.

---

## 2. What exists today

### 2.1 Billing frontend

* Framework-free TypeScript DOM app (`web/src`), ~50k lines, bundled by esbuild into a
  committed `web/dist`, embedded in the binary by `web/web.go`.
* One design-token file, `web/src/styles/tokens.css`, with `:root` / `[data-theme="dark"]` /
  `[data-theme="light"]`. Everything else is already remarkably tokenized: outside
  `tokens.css` there are **10 colour literals across 18 CSS files, plus exactly one in
  TypeScript**. That is the good news — the colour layer is nearly ready for a theme document.
* `web/src/lib/theme.ts` supports exactly two themes (`dark`, `light`) and stores the choice
  in `localStorage["catalyst.theme"]`.
* Branding is hardcoded in a small, known set of places (full inventory in Appendix B).
* Storefront layout is fixed in code: header, hero, category chips, product grid, footer
  (`web/src/views/storefront.ts`). Hero copy and section headings are literal.
* `web/src/styles/layout.css` owns the shell (`--sidebar-w`, `--topbar-h`, grid).

### 2.2 Billing backend

* `internal/settings` is a typed registry over a `(group, key, value)` table: a missing row
  reads a code-side default; values are bounded at 64 KiB (`maxValueBytes = 64 << 10`);
  secrets are redacted; writes are audited in the same transaction.
* Routes are data (`internal/httpapi/routes.go`) and every route declares its access class,
  permission, CSRF and idempotency. Permission coverage and OpenAPI parity are tests.
* `internal/authn/authz.go` holds the closed permission list; system roles are immutable by
  trigger and seeded through migrations. The newest permission is `i18n:write` /
  `import:write` / `update:manage` (`0027_permission_expansion.sql`), which is the template a
  new `theme:*` grant must follow.
* Uploads already have a precedent: `product_images` (`0018_catalog.sql:58`) stores bytes in
  the database, deduped by SHA-256 digest, capped at 256 KiB, served from `/api/v1/media/{id}`
  with `Cache-Control: immutable`, `X-Content-Type-Options: nosniff`, and a sandboxing CSP for
  SVG.
* Email templates (`internal/notify`) substitute `{{company_name}}` and fall back to the
  literal `"Catalyst Billing"` in three places.
* Invoice PDFs (`internal/invoicepdf`) use a `billing.Merchant` snapshot and built-in
  Helvetica; they do not read the web theme at all.
* Migrations live in `internal/store/migrations`, are applied in one transaction and
  checksummed; `billing doctor` verifies the schema is current. **The highest-numbered
  migration today is `0058`**, so the theme migration takes the next free number (`0059`) and
  must be renumbered again if it lands after another migration.

### 2.3 The game panel (reference implementation)

Live at `https://demo.catalystctl.com/admin/theme-settings`; its SPA bundle and
`ThemeSettingsPage` chunk were downloaded and read to derive the facts below.

* **Settings shape:** `panelName`, `logoUrl`, `faviconUrl`, `defaultTheme`
  (`system|light|dark`), `enabledThemes[]`, `primaryColor`, `secondaryColor`,
  `accentColor`, `metadata.themeColors` (success/warning/danger/info/ring/borderRadius plus
  12 dark and 12 light surface colours), `customCss` (100 KiB).
* **APIs:** `GET`/`PATCH /api/admin/theme-settings`, `GET /api/theme-settings/public`.
* **UI:** eight tabs — Presets, Brand, Palette, Colors, Surfaces, Layout, Manage, Advanced.
  Colour-harmony generator, random seed, live preview, personal colour overrides, JSON
  import/export (schema v1), reset, save.
* **Runtime:** localStorage cache `catalyst-theme-cache-v1`, a pre-paint inline script, CSS
  variables in HSL, and a `<style id="catalyst-custom-css">` element.
* **Sanitizer:** `customCss.trim().slice(0, 100000)` then five regex replacements
  (`expression(`, `behavior:`, `-moz-binding:`, `javascript:`, `@import`). This is a
  blacklist, not a parser; it is the first thing to improve on.
* **Presets:** Catalyst Bronze, Ocean, Sunset Ember, Forest, Royal, Crimson, Slate Mono,
  Neon Nights, Brutalist.
* **Gaps:** no asset upload, no OG/banner/email/PDF branding, no scoped themes, no layout
  composition (the “Layout” tab is only a radius slider), no history/rollback, no fonts, no
  contrast validation, no per-page templates.

---

## 3. Design principles

1. **A theme is data, not code.** One versioned JSON document per scope. No theme requires a
   deploy; no deploy changes a theme.
2. **A preset is a starting point, not a mode.** Applying a preset creates a draft revision
   the operator can edit. Presets are embedded JSON in the binary, importable and exportable.
3. **The token contract is the API.** Components only ever read CSS variables. A theme that
   does not define a token inherits it from its parent scope or the built-in default.
4. **The strict CSP does not move.** No `'unsafe-inline'`, no nonce for admin content, no
   external origins. Theme CSS/JS/assets are same-origin, revision-addressed resources.
5. **The default is the current look.** Shipping the system must not change a pixel until an
   operator chooses a preset. `Catalyst Console` (the game-panel match) is the recommended
   default for new installations, but existing installations keep `Catalyst Billing`.
6. **A scope is a trust boundary.** Storefront custom CSS can never style the staff panel.
   Panel custom CSS is a privileged capability and is audited.
7. **Every claim is testable.** Contrast, CSP, sanitizer bypasses, cache headers, preset
   rendering and branding removal all get automated gates.

---

## 4. Architecture

```
                       ┌──────────────────────────────────────────────┐
                       │  theme_revisions (immutable JSONB document)  │
                       │  scope: panel | portal | storefront | email  │
                       │         | invoice                            │
                       └───────────────┬──────────────────────────────┘
                                       │ publish
                       ┌───────────────▼──────────────┐
                       │ theme_assignments (scope →    │
                       │ revision) + in-process cache  │
                       └───────────────┬──────────────┘
              ┌────────────────────────┼─────────────────────────┐
              │                        │                         │
   ┌──────────▼─────────┐   ┌──────────▼──────────┐   ┌──────────▼─────────┐
   │ GET /theme/<rev>/  │   │ GET /api/v1/theme    │   │ Email / PDF         │
   │  theme.css         │   │  (manifest: brand,   │   │ renderers consume   │
   │  boot.js           │   │  layout, assets)     │   │ the same document   │
   │ /theme/assets/<id> │   └──────────┬───────────┘   └────────────────────┘
   └──────────┬─────────┘              │
              │                        │
   ┌──────────▼────────────────────────▼───────────────────────────────────┐
   │ Server-rendered HTML shell (title, meta/OG, favicon, manifest, mark)  │
   │ + existing SPA bundle                                                 │
   └───────────────────────────────────────────────────────────────────────┘
```

### 4.1 The theme document (schema v2)

The document is intentionally broader than the game panel's settings row. Every field is
optional; missing fields inherit. A complete example is in §7.

```jsonc
{
  "schema": 2,
  "id": "catalyst-console",
  "name": "Catalyst Console",
  "description": "Dense, dark-first console matching the game panel.",
  "extends": null,                    // preset id or parent theme id
  "brand": {
    "name": "Catalyst",
    "product": "Billing",
    "legal_name": "Catalyst Hosting Ltd",
    "tagline": "Infrastructure, billed honestly",
    "logo_light": "asset:<id>",
    "logo_dark": "asset:<id>",
    "mark": "asset:<id>",
    "favicon": "asset:<id>",
    "og_image": "asset:<id>",
    "email_logo": "asset:<id>",
    "support_url": "https://example.com/support",
    "footer_text": "© 2025 Catalyst Hosting",
    "hide_credit": true
  },
  "color": {
    "mode": "dark",                   // dark | light | system
    "brand":  { "primary": "#ff3d7f", "secondary": "#252b35", "accent": "#ff3d7f" },
    "semantic": { "success": "#3fcf8e", "warning": "#ffb020", "danger": "#ff5a5a", "info": "#6fa8ff", "focus": "#ff3d7f" },
    "dark":  { "bg": "#0e1116", "card": "#161a21", "surface1": "#1d222b", "surface2": "#232a35", "surface3": "#2b3442", "border": "#2b323d", "borderStrong": "#3a4350", "text": "#e9edf2", "textMuted": "#939dac", "textFaint": "#6b7585", "input": "#2b323d", "popover": "#161a21", "overlay": "rgba(5,7,11,.72)" },
    "light": { "bg": "#f0f3f7", "card": "#fcfdff", "surface1": "#fcfdff", "surface2": "#e9edf3", "surface3": "#d5dae2", "border": "#d5dae2", "borderStrong": "#b9c1cd", "text": "#161c24", "textMuted": "#5f6a78", "textFaint": "#8792a5", "input": "#d5dae2", "popover": "#fcfdff", "overlay": "rgba(15,21,35,.55)" }
  },
  "typography": {
    "display": { "family": "Inter", "fallback": "system-ui, sans-serif", "weights": [600, 700], "tracking": "-0.015em" },
    "body":    { "family": "Inter", "fallback": "system-ui, sans-serif", "weights": [400, 500], "tracking": "0" },
    "mono":    { "family": "JetBrains Mono", "fallback": "ui-monospace, monospace", "weights": [400, 500], "tracking": "0" },
    "scale":   { "xs": "0.6875rem", "sm": "0.75rem", "base": "0.8125rem", "md": "0.875rem", "lg": "1rem", "xl": "1.25rem", "2xl": "1.625rem", "3xl": "2.25rem" }
  },
  "space":   { "unit": 4, "density": "compact", "scale": [0.25, 0.5, 0.75, 1, 1.5, 2, 3, 4] },
  "shape":   { "radius": { "xs": "4px", "sm": "6px", "md": "10px", "lg": "14px", "xl": "20px", "full": "999px" }, "borderWidth": "1px" },
  "elevation": { "1": "0 1px 0 rgba(255,255,255,.03) inset", "2": "0 10px 30px -12px rgba(0,0,0,.7)", "3": "0 32px 80px -24px rgba(0,0,0,.85)" },
  "motion":  { "fast": "90ms", "normal": "160ms", "slow": "260ms", "ease": "cubic-bezier(.2,.8,.2,1)" },
  "background": { "style": "flat", "image": null, "gradient": null, "noise": 0 },
  "layout": {
    "shell": { "nav": "sidebar", "sidebarWidth": "232px", "topbarHeight": "56px", "contentMax": "1440px", "cardStyle": "outlined", "tableStyle": "compact" },
    "pages": { "storefront.home": { "sections": [ /* §9 */ ] } }
  },
  "components": {
    "button": { "height": "32px", "radius": "6px", "uppercase": false },
    "input":  { "height": "32px", "radius": "6px" },
    "card":   { "radius": "10px", "padding": "16px" }
  },
  "customCss": "/* advanced, validated, ≤ 256 KiB */"
}
```

Validation is server-side and closed:

* colours: `#rgb`, `#rrggbb`, `#rrggbbaa`, `rgb()/rgba()`, `hsl()/hsla()`, or a token
  reference `var(--…)`; no `url()`, no `expression`, no unknown functions;
* lengths: a number plus `px|rem|em|%|vh|vw|ch`; bounded (e.g. radius ≤ 64px, font scale
  between 0.5× and 3×);
* enums: nav, density, cardStyle, tableStyle, background style, font families from the
  installed set;
* assets: an `asset:<uuid>` reference that exists and has an allowed content type;
* layout: every section type exists in the block registry and every prop validates against
  that block's schema;
* total document size ≤ 1 MiB (excluding asset bytes); `customCss` ≤ 256 KiB;
* unknown top-level keys are rejected (a typo must not be silently ignored).

### 4.2 Scopes and inheritance

| Scope | Used by | Default | Can override |
|---|---|---|---|
| `panel` | Staff admin SPA | `Catalyst Billing` (existing look) | everything |
| `portal` | Customer area | inherits `panel` | brand, colours, layout shell, custom CSS |
| `storefront` | Public catalogue, cart, checkout, auth pages | inherits `panel` | brand, colours, layout (full page composition), custom CSS |
| `email` | Transactional mail | inherits `panel` (colours/logo only) | logo, colours, footer text |
| `invoice` | PDF invoices/credit notes | inherits `panel` (logo/colours) | logo, colours, fonts |

Resolution is a deep merge: `built-in default ← parent scope ← scope document`. A scope can
set `"inherit": false` to start from the built-in default instead. The merge is performed
once at publish time and the resolved document is stored alongside the draft (so a later
change to the parent cannot silently mutate a published storefront). Publishing a parent
offers to re-resolve children; the UI shows a diff.

The storefront can therefore be a completely different design from the admin panel while
still sharing the semantic contract (success is still green, danger is still red).

### 4.3 Presets

Presets are embedded JSON files (`internal/theme/presets/*.json`, `go:embed`) validated by
the same code path as an imported theme. Applying a preset creates a **draft revision**; it
never publishes directly. The catalogue is in §8.

Import/export uses an envelope so a theme can carry its assets:

```jsonc
{
  "kind": "catalyst.theme",
  "schema": 2,
  "exported_at": "2025-01-01T00:00:00Z",
  "theme": { /* document */ },
  "assets": [ { "id": "...", "kind": "logo", "content_type": "image/png", "digest": "sha256:...", "data_base64": "..." } ]
}
```

The export is deterministic (sorted keys, fixed asset order) so two exports of the same
revision are byte-identical. A future marketplace can add a detached signature over the
canonical JSON; the envelope has a `signature` field reserved for it.

---

## 5. Data model

Migration `0059_themes.sql` (next free number; `0058` is the current highest — renumber on
merge). All tables are owned by the migration role; the runtime role gets `SELECT/INSERT/
UPDATE/DELETE` only, matching the existing privilege model.

```sql
-- A theme is a named container. Revisions are immutable; the container is the stable identity.
CREATE TABLE themes (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        text NOT NULL,
    name        text NOT NULL,
    scope       text NOT NULL CHECK (scope IN ('panel','portal','storefront','email','invoice')),
    created_by  uuid REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    deleted_at  timestamptz
);
CREATE UNIQUE INDEX themes_slug_unique ON themes (lower(slug)) WHERE deleted_at IS NULL;
CREATE INDEX themes_scope_idx ON themes (scope) WHERE deleted_at IS NULL;

-- One immutable document per revision. `document` is the resolved theme (after inheritance);
-- `source` is what the operator actually saved (before inheritance), so a child can be edited.
CREATE TABLE theme_revisions (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    theme_id      uuid NOT NULL REFERENCES themes (id) ON DELETE CASCADE,
    revision      int  NOT NULL CHECK (revision > 0),
    status        text NOT NULL CHECK (status IN ('draft','published','archived')),
    document      jsonb NOT NULL,
    source        jsonb NOT NULL,
    digest        text NOT NULL,              -- sha256 of canonical document
    note          text NOT NULL DEFAULT '',
    created_by    uuid REFERENCES users (id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    published_by  uuid REFERENCES users (id),
    published_at  timestamptz,
    UNIQUE (theme_id, revision),
    -- Lets theme_assignments prove the revision belongs to the theme it names.
    UNIQUE (theme_id, id)
);
CREATE UNIQUE INDEX theme_revisions_one_published
    ON theme_revisions (theme_id) WHERE status = 'published';
CREATE INDEX theme_revisions_theme_idx ON theme_revisions (theme_id, revision DESC);

-- Which revision each scope currently serves. One row per scope.
CREATE TABLE theme_assignments (
    scope        text PRIMARY KEY CHECK (scope IN ('panel','portal','storefront','email','invoice')),
    theme_id     uuid NOT NULL,
    revision_id  uuid NOT NULL,
    updated_by   uuid REFERENCES users (id),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (theme_id, revision_id) REFERENCES theme_revisions (theme_id, id)
);

-- Uploaded bytes, deduped by digest. Mirrors product_images, with a kind and alt text.
-- The digest index includes kind because the same bytes can legitimately be both a logo
-- and a favicon, and the asset row carries the role it plays.
CREATE TABLE theme_assets (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text NOT NULL CHECK (kind IN ('logo','mark','favicon','og','banner','background','font','other')),
    content_type text NOT NULL CHECK (content_type IN (
                    'image/png','image/jpeg','image/webp','image/svg+xml',
                    'font/woff2','font/woff','application/octet-stream')),
    bytes        bytea NOT NULL,
    byte_size    int NOT NULL CHECK (byte_size > 0 AND byte_size <= 4194304),
    digest       text NOT NULL,
    width        int,
    height       int,
    alt          text NOT NULL DEFAULT '',
    created_by   uuid REFERENCES users (id),
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX theme_assets_digest_unique ON theme_assets (digest, kind);

-- Audit-friendly publication log (the audit table already records the action; this is the
-- queryable history the UI lists, including the note and the diff summary).
CREATE TABLE theme_publications (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    scope        text NOT NULL,
    theme_id     uuid NOT NULL REFERENCES themes (id),
    revision_id  uuid NOT NULL REFERENCES theme_revisions (id),
    previous_revision_id uuid REFERENCES theme_revisions (id),
    published_by uuid REFERENCES users (id),
    published_at timestamptz NOT NULL DEFAULT now(),
    note         text NOT NULL DEFAULT ''
);
CREATE INDEX theme_publications_scope_idx ON theme_publications (scope, published_at DESC);
```

A seed row creates the default `panel` theme and assignment from the built-in
`catalyst-billing` preset, so a fresh install and an upgraded install both have a published
revision before the first request.

### 5.1 Go types

```go
package theme

type Document struct {
    Schema     int               `json:"schema"`
    ID         string            `json:"id"`
    Name       string            `json:"name"`
    Extends    string            `json:"extends,omitempty"`
    Brand      Brand             `json:"brand"`
    Color      Color             `json:"color"`
    Typography Typography        `json:"typography"`
    Space      Space             `json:"space"`
    Shape      Shape             `json:"shape"`
    Elevation  map[string]string `json:"elevation"`
    Motion     Motion            `json:"motion"`
    Background Background        `json:"background"`
    Layout     Layout            `json:"layout"`
    Components map[string]any    `json:"components"`
    CustomCSS  string            `json:"customCss,omitempty"`
}

type Revision struct {
    ID        string
    ThemeID   string
    Number    int
    Status    Status
    Document  Document   // resolved
    Source    Document   // as saved
    Digest    string
    Note      string
    CreatedAt time.Time
}
```

Packages:

| Package | Responsibility |
|---|---|
| `internal/theme` | document types, validation, merge/inheritance, canonical JSON, digest |
| `internal/theme/presets` | embedded preset JSON + catalogue metadata |
| `internal/theme/css` | token → CSS generator, custom-CSS parser/validator, aliases |
| `internal/theme/layout` | layout schema, block registry, validation |
| `internal/theme/store` | DB access: themes, revisions, assignments, assets, publications |
| `internal/theme/cache` | in-process snapshot, revision lookup, `LISTEN/NOTIFY` invalidation |
| `internal/theme/assets` | upload decode/validate/re-encode, digest, dimensions |
| `internal/httpapi/theme.go` | public + staff routes |
| `internal/theme/email` | email-safe inline CSS from tokens |
| `internal/theme/pdf` | PDF palette/logo/font helpers (consumed by `invoicepdf`) |

### 5.2 Notes that constrain the implementation

These are the repo facts that most often surprise a first implementation:

* `web/web.go` sets one global CSP for every response. A preview iframe needs
  `frame-src 'self'` and `frame-ancestors 'self'`; do **not** relax the global policy. Give the
  preview path its own header branch (and keep `frame-ancestors 'none'` on `/`), otherwise
  `scripts/browser-smoke.sh` (which asserts `frame-ancestors 'none'`) and `web/web_test.go`
  fail — correctly.
* `web/web_test.go` asserts that the shell contains no inline `<script>` and no inline
  `style=` attribute. The server-rendered shell must keep that property: theme values go into
  `<meta>`, `<link>`, `<title>` and text, never into inline CSS/JS.
* `scripts/brand/verify.mjs` asserts `og:image` is 1200×630, the manifest names the 192/512
  icons, `mark.svg` carries the traced path, and the favicon decodes as an image. The
  **default** theme must continue to satisfy every one of those; a custom theme changes the
  values, not the contract.
* `go.mod` is deliberately dependency-light (pgx, x/crypto, yaml only). A real CSS parser is
  the one new Go dependency this plan recommends (`github.com/tdewolff/parse/v2/css`, small
  and pure Go). The alternative is a hand-written tokenizer/validator, which is more code and
  more risk than a reviewed parser.
* `web/build.mjs` currently substitutes `{{STYLE}}` and `{{SCRIPT}}` at build time and strips
  an empty stylesheet link. Server-side shell rendering adds theme placeholders; the build
  must leave unknown placeholders intact and the Go renderer must have a default for each
  one, so `go build` still works with no Node toolchain.
* The shell is served for every SPA route. Cache the rendered shell per `(scope, revision)`
  in memory; rendering it per request is unnecessary work on the hot path.
* Two different Catalyst fallback strings already exist and disagree: the settings default for
  `general.company_name` is `"Catalyst Hosting"` (`internal/settings/settings.go`), while
  `internal/notify` falls back to `"Catalyst Billing"` in three places
  (`events.go`, `manual_create.go`). A theme must not add a third; it should own the display
  name and leave the merchant name to `general.company_name`.

---

## 6. API surface

### 6.1 Public (no session)

| Method | Path | Purpose | Cache |
|---|---|---|---|
| `GET` | `/api/v1/theme` | Resolved manifest for the caller's scope (storefront for anonymous, panel/portal for a session) | `public, max-age=60, stale-while-revalidate=300`; ETag |
| `GET` | `/api/v1/theme/{scope}` | Manifest for a named scope (storefront/email/invoice only; panel/portal require a session) | same |
| `GET` | `/theme/{revision}/theme.css` | Generated tokens + preset CSS + custom CSS | `immutable, max-age=31536000` (revision-addressed) |
| `GET` | `/theme/{revision}/boot.js` | Pre-paint personal-preference script | immutable |
| `GET` | `/theme/assets/{id}` | Uploaded image/font bytes | immutable, `nosniff`, sandbox CSP for SVG |
| `GET` | `/theme/manifest.webmanifest` | PWA manifest from brand assets | `max-age=0, must-revalidate`; ETag |
| `GET` | `/theme/favicon.ico` | Themed favicon (redirect to asset or generated) | `max-age=0, must-revalidate` |
| `GET` | `/theme/og.png` | Themed social card | `max-age=0, must-revalidate` |
| `GET` | `/theme/preview` | Draft preview shell, requires a signed token | `no-store`, `X-Robots-Tag: noindex` |

The shell (`GET /`) is rendered with the active scope's revision. The HTML references the
revision-addressed CSS/JS, so a publish is visible on the next navigation without a cache
purge.

### 6.2 Staff

| Method | Path | Permission | Notes |
|---|---|---|---|
| `GET` | `/api/v1/staff/theme` | `theme:read` | Themes, active revisions, scopes |
| `GET` | `/api/v1/staff/theme/presets` | `theme:read` | Catalogue + preview thumbnails |
| `GET` | `/api/v1/staff/theme/{scope}` | `theme:read` | Draft + published + history |
| `POST` | `/api/v1/staff/theme/{scope}/draft` | `theme:write` | Create draft from preset / current / imported doc |
| `PUT` | `/api/v1/staff/theme/revisions/{id}` | `theme:write` | Update a draft only |
| `POST` | `/api/v1/staff/theme/revisions/{id}/publish` | `theme:publish` | Publish (optionally re-resolve children) |
| `POST` | `/api/v1/staff/theme/revisions/{id}/restore` | `theme:write` | New draft from an old revision |
| `GET` | `/api/v1/staff/theme/revisions/{id}/diff` | `theme:read` | Structured diff vs published/parent |
| `POST` | `/api/v1/staff/theme/preview-token` | `theme:write` | Signed, short-lived preview token |
| `POST` | `/api/v1/staff/theme/import` | `theme:write` | Import envelope (theme + assets) |
| `GET` | `/api/v1/staff/theme/{scope}/export` | `theme:read` | Deterministic export envelope |
| `POST` | `/api/v1/staff/theme/assets` | `theme:write` | Raw body + `Content-Type`, ≤ 4 MiB |
| `GET` | `/api/v1/staff/theme/assets` | `theme:read` | List, usage count |
| `DELETE` | `/api/v1/staff/theme/assets/{id}` | `theme:write` | Refuse if referenced by a published revision |
| `POST` | `/api/v1/staff/theme/{scope}/branding/remove-catalyst` | `theme:publish` | One-click brand removal + verification report |

Mutations use the existing `staffMutation` wrapper: CSRF, `Idempotency-Key` required, rate
class `mutation`, audit row in the same transaction.

New permissions (closed list in `internal/authn/authz.go`, mirrored by a migration grant):

* `theme:read` — view the studio, presets, history.
* `theme:write` — create/edit drafts, upload assets, import.
* `theme:publish` — publish, rollback, edit custom CSS, remove branding.

Grant `theme:read`/`theme:write`/`theme:publish` to `administrator`; `theme:read`/`theme:write`
to `billing` (storefront merchandising); `theme:read` to `support`. The `authn` role test must
be updated in the same commit, as with `0027_permission_expansion.sql`.

---

## 7. Token contract

The generated CSS defines **three vocabularies** over the same values, so nothing has to be
rewritten at once and presets can be authored in either the billing or the game-panel format.

### 7.1 Canonical billing variables

| Group | Variables |
|---|---|
| Ground | `--color-bg`, `--color-bg-elevated`, `--color-overlay`, `--color-scrim` |
| Surface | `--color-surface-1`, `--color-surface-2`, `--color-surface-3`, `--color-surface-hover`, `--color-surface-inset` |
| Text | `--color-text`, `--color-text-muted`, `--color-text-faint`, `--color-text-inverse` |
| Line | `--color-border`, `--color-border-strong`, `--color-focus-ring` |
| Brand | `--color-primary`, `--color-primary-hover`, `--color-primary-press`, `--color-on-primary`, `--color-primary-soft`, `--color-secondary`, `--color-accent` |
| Semantic | `--color-success`, `--color-warning`, `--color-danger`, `--color-info` (+ `-soft` variants) |
| Type | `--font-display`, `--font-body`, `--font-mono`, `--text-xs…3xl`, `--leading-*`, `--tracking-*` |
| Space | `--space-1…8`, `--density` |
| Shape | `--radius-xs…full`, `--border-width` |
| Depth | `--shadow-1…3`, `--shadow-focus` |
| Motion | `--duration-fast/normal/slow`, `--ease` |
| Layout | `--sidebar-w`, `--topbar-h`, `--content-max`, `--gutter` |
| Components | `--button-h`, `--input-h`, `--card-radius`, `--card-padding`, `--table-row-h` |

### 7.2 Legacy aliases (zero-risk migration)

`tokens.css` today uses `--bg`, `--surface`, `--surface-2`, `--surface-3`,
`--surface-hover`, `--surface-inset`, `--line`, `--line-strong`, `--line-accent`, `--text`,
`--text-dim`, `--text-faint`, `--text-inverse`, `--accent`, `--accent-hover`,
`--accent-press`, `--accent-ink`, `--accent-soft`, `--accent-soft-strong`, `--success`,
`--warning`, `--danger`, `--info` (+ `-soft`), `--shadow-1…3`, `--shadow-focus`,
`--skeleton`, `--overlay`, `--scrollbar`, `--s-1…8`, `--r-xs…full`, `--ease`, `--dur-1…3`.

The generator emits these as aliases of the canonical variables. Existing CSS keeps working
unchanged; the migration to canonical names can happen file by file, or never. There are
**72 distinct `var(--…)` names across 18 CSS files** today, which is the exact set the
aliases must cover — a name that is missed is a token that silently stops theming.

### 7.3 Game-panel compatibility

For presets authored in the game panel's format, emit the HSL triplets it expects:
`--background`, `--foreground`, `--card`, `--card-foreground`, `--popover`, `--primary`,
`--primary-foreground`, `--secondary`, `--muted`, `--muted-foreground`, `--accent`,
`--border`, `--input`, `--ring`, `--surface-1…3`, `--radius`, plus `--success`, `--warning`,
`--danger`, `--info`. This makes the two products' theme JSON interchangeable and lets an
operator copy a game-panel theme into billing.

The generator converts any accepted colour to the formats each vocabulary needs. It computes
derived values (`primary-hover`, `primary-press`, `on-primary`, soft tints) from the seed
colour, and it validates contrast **before** publishing.

### 7.4 What a component may use

Only variables. A new rule that hardcodes a colour, font stack, radius or shadow fails
`make theme-check` (a small script that greps CSS/TS for colour literals outside the token
and asset files, with an allowlist for the generator and `assets/`). This is the gate that
stops the system rotting the way most theming systems do.

The current exceptions are known and finite: 10 colour literals in CSS
(`components.css` ×3, `view-portal.css` ×2, `layout.css` ×2, `view-storefront.css`,
`pages.css` ×2) and one in TypeScript (`lib/icons.ts`, the `#F97301` mark fill). They should
be reduced to zero or added to the allowlist with a reason.

---

## 8. Preset catalogue

Ten presets, each a full document (not a hue swap). All must pass WCAG AA for body text and
3:1 for UI borders/focus rings; the contrast report is generated by a test, not by hand.

| # | Preset | Character | Default mode | Radius | Density | Signature |
|---|---|---|---|---|---|---|
| 1 | **Catalyst Console** | The game panel's dashboard: near-black `#0e1116`, compact, 4px radius, pink/rose primary `#ff3d7f` | dark | 4px | compact | Dense tables, flat cards, thin borders |
| 2 | **Catalyst Billing** | Today's billing panel: graphite + cobalt, tan brand accents | dark | 10px | comfortable | The safe default for existing installs |
| 3 | **Catalyst Bronze** | The game panel's shipped tan/bronze palette (`#c48d5a`) | dark | 4px | compact | Warm bronze on ink |
| 4 | **Brutalist** | Hard edges, 2px borders, offset black shadows, mono labels, yellow/red | light | 0 | compact | No radius, no blur, high contrast |
| 5 | **Editorial Paper** | Warm paper, serif display, magazine grid, generous whitespace | light | 2px | roomy | Serif headlines, rules instead of cards |
| 6 | **Neon Terminal** | Near-black, phosphor green/amber, mono, scanline texture, glow | dark | 2px | compact | Monospace everywhere, glowing focus |
| 7 | **Glass Aurora** | Translucent surfaces, gradient mesh, blur, light-first | light | 14px | comfortable | Frosted panels, soft shadows |
| 8 | **Warm SaaS** | Friendly rounded, pastel, soft shadows, humanist sans | light | 14px | comfortable | Rounded pills, gentle elevation |
| 9 | **Monochrome Studio** | Grayscale + one signal colour, Swiss grid, strong type | light | 4px | comfortable | Type-led, no decoration |
| 10 | **Midnight Luxe** | Deep navy/ink, gold accent, serif display, low density | dark | 8px | roomy | Premium, spacious, gold hairlines |

Fonts are self-hosted (open licences only): Inter, DM Sans, JetBrains Mono, Source Serif 4,
Space Grotesk, IBM Plex Mono, Fraunces. Each preset lists its families and fallbacks; the
generator emits `@font-face` for the uploaded/embedded subsets only when the font is actually
used.

The default for **new** installations is `Catalyst Console`; the default for an **existing**
installation after upgrade is `Catalyst Billing` (its current look), with a banner offering
"Match the game panel" as a one-click draft.

### 8.1 `Catalyst Console` — exact reference values

Taken from the game panel's theme store and its shipped default preset. These are the values
an implementer should use for the game-panel match; the contrast test may adjust one or two
for AA.

| Token | Dark | Light |
|---|---|---|
| background | `#0e1116` | `#f0f3f7` |
| foreground | `#e9edf2` | `#161c24` |
| card / popover | `#161a21` | `#fcfdff` |
| surface-1 | `#1d222b` | `#fcfdff` |
| surface-2 | `#232a35` | `#e9edf3` |
| surface-3 | `#2b3442` | `#d5dae2` |
| border / input | `#2b323d` | `#d5dae2` |
| muted-foreground | `#939dac` | `#5f6a78` |
| muted-background | `#1d222b` | `#e9edf3` |

| Brand / semantic | Value |
|---|---|
| primary / accent / ring | `#ff3d7f` |
| secondary | `#252b35` |
| success | `#3fcf8e` |
| warning | `#ffb020` |
| danger | `#ff5a5a` |
| info | `#6fa8ff` |
| radius | `0.25rem` (4px) |
| density | compact |
| shell | sidebar, 232px, 56px topbar, 1440px content |

For reference, the game panel's *shipped* `Catalyst Bronze` preset is primary `#c48d5a`,
secondary `#5ac4c2`, accent `#5a5cc4` on the same dark ground. Include it as preset 3; it is
the conservative choice if an operator wants the console layout with the warmer Catalyst
palette.

---

## 9. Storefront layout composition

The goal is not a generic website builder. It is a **section-based storefront** with enough
range to look like a different product, bounded enough to stay accessible, fast and safe.

### 9.1 Layout document

```jsonc
{
  "schema": 1,
  "page": "home",
  "template": "showcase",
  "sections": [
    { "id": "hero", "type": "hero", "props": {
        "eyebrow": "Infrastructure, billed honestly",
        "title": "Servers that come with a ledger.",
        "body": "Pick a plan…",
        "actions": [
          { "label": "See the plans", "href": "#plans", "variant": "primary" },
          { "label": "Client area", "href": "/login" }
        ],
        "media": "asset:<id>",
        "align": "left",
        "height": "lg"
    }},
    { "id": "featured", "type": "product_grid", "props": {
        "source": "featured", "columns": 3, "limit": 6, "showPrice": true
    }},
    { "id": "trust", "type": "trust_badges", "props": { "items": ["99.99% uptime", "24/7 support", "No setup fee"] }},
    { "id": "faq", "type": "faq", "props": { "items": [ { "q": "…", "a": "…" } ] }},
    { "id": "footer", "type": "footer", "props": { "columns": 3 } }
  ],
  "visible": { "auth": "any", "devices": ["desktop", "tablet", "mobile"] }
}
```

Layouts are stored per page: `storefront.home`, `storefront.product`, `storefront.category`,
`storefront.cart`, `storefront.checkout`, `auth.login`, `auth.register`, `portal.home`,
`portal.invoices`, `portal.services`, `portal.support`.

### 9.2 Block registry v1

| Block | Props (summary) | Data source |
|---|---|---|
| `announcement_bar` | text, link, tone, dismissible | settings/announcements |
| `hero` | eyebrow, title, body, actions[], media, align, height, overlay | static + asset |
| `product_grid` | source (all/featured/category), category, columns, limit, showPrice, showCategory | catalog |
| `product_list` | same, layout (rows/cards), density | catalog |
| `category_grid` | columns, showCount, showDescription | catalog |
| `pricing_table` | product, highlight price id, showSetup | catalog |
| `product_detail` | gallery, description, plan selector, buy action | catalog |
| `features` | items[] (icon, title, body), columns | static |
| `image_text` | media, title, body, align, cta | static + asset |
| `stats` | items[] (value, label) | static |
| `testimonials` | items[] (quote, author, role, avatar) | static |
| `logo_cloud` | assets[], grayscale | static + assets |
| `faq` | items[] (q, a) | static |
| `rich_text` | sanitized HTML | static |
| `trust_badges` | items[] | static |
| `newsletter` | title, body, action | static (no provider v1) |
| `footer` | columns[], legal, social[], language switcher | static + settings |
| `auth_card` | title, body, providers | auth config |
| `portal_nav` | items[] | permissions |
| `cart_summary` | showCoupon, showTax | cart |
| `checkout_summary` | showTerms | checkout |

Every block declares: prop schema, data source, fallbacks, empty state, error state,
accessibility notes, and whether it is allowed on customer-facing pages only. Blocks render
through the existing `ui/kit.ts` primitives so they inherit tokens and focus behaviour.

### 9.3 Renderer

* `web/src/lib/blocks.ts` — registry: `type -> { schema, render(props, ctx) }`.
* `web/src/lib/layout.ts` — fetch the layout document (part of the theme manifest), validate
  client-side, render sections in order, mount into the page host.
* The storefront page becomes: load theme + catalog in parallel, render `sections` with a
  shared data context. A missing/invalid layout falls back to the built-in `classic` layout
  so a bad publish cannot take the shop down.
* Each block fetches only what it needs; the catalog response is shared where possible.
* SEO: the shell's title/description/OG come from the server. Per-product OG/structured data
  is phase 4 (requires SSR or a pre-render pass; note it explicitly rather than pretending
  client-rendered meta is enough).

### 9.4 Admin editor

`/settings/theme/storefront`:

* page selector (home, product, category, cart, checkout, auth, portal);
* section list with drag-reorder, enable/disable, duplicate, delete;
* “Add section” drawer grouped by purpose, with a preview thumbnail;
* property panel generated from the block schema (text, asset picker, colour token, number,
  toggle, repeater);
* responsive preview (desktop/tablet/mobile) in an iframe;
* “Reset section”, “Reset page”, “Start from preset”;
* draft/publish with a diff summary;
* JSON view for power users (read-only in v1; import/export).

A free-form “custom HTML” block is deliberately **not** in v1. It is the single largest XSS
and brand-safety risk, and the section registry covers the legitimate use cases. If it is
added later, it is gated on `theme:publish`, sanitized with the existing HTML allowlist
(`internal/catalog/sanitize.go`), and rendered in a sandboxed iframe.

### 9.5 Shell variants

The admin/portal shell becomes token- and layout-driven:

* `nav`: `sidebar` | `rail` | `topbar`;
* `density`: compact | comfortable | roomy;
* `cardStyle`: flat | outlined | elevated;
* `tableStyle`: compact | comfortable;
* `contentMax`, `sidebarWidth`, `topbarHeight`;
* optional `commandPalette` and `breadcrumbs` toggles.

These are CSS variables plus a `data-shell` attribute, not a second stylesheet. A preset can
therefore change the *shape* of the panel, not only its colours.

---

## 10. Admin UX — the Theme Studio

### 10.1 Information architecture

A dedicated area, not one long settings page. Routes (lazy-loaded like every other screen):

```
/settings/theme                 Overview: active theme per scope, quick actions, health
/settings/theme/presets         Preset gallery, apply as draft, compare
/settings/theme/brand           Name, product, legal name, tagline, logo/mark/favicon/OG,
                                email logo, support URL, footer, “Remove Catalyst branding”
/settings/theme/colors          Brand, semantic, surfaces (light/dark), contrast meter,
                                harmony generator, auto-fix
/settings/theme/typography      Families, scale, weights, tracking, font uploads
/settings/theme/layout          Shell: nav, density, width, card/table style, radius, motion
/settings/theme/storefront      Page/section composer (§9.4)
/settings/theme/portal          Portal + auth page overrides
/settings/theme/email           Email logo, colours, footer, test send
/settings/theme/invoice         PDF logo, colours, font, sample download
/settings/theme/assets          Asset library: upload, crop/focal point, variants, alt text
/settings/theme/css             Custom CSS editor with validation, scope warning, diff
/settings/theme/history         Revisions, who/when, diff, preview, restore, publish
/settings/theme/import-export   JSON import/export, preset envelope
```

A left sub-nav mirrors these sections; the existing sticky-index pattern in
`web/src/views/settings.ts` is reused. Every route deep-links, so a support agent can send an
operator a URL.

### 10.2 Live preview

* A same-origin iframe at `/theme/preview?scope=…&page=…&token=…`.
* The token is HMAC-signed, expires in 15 minutes, is bound to the user and the draft
  revision, and grants no API access beyond reading that draft.
* The preview shell uses `frame-ancestors 'self'` (the rest of the app keeps `'none'`).
* Device frames: desktop 1440, tablet 834, mobile 390; zoom 50/75/100%.
* Scope switcher: panel, portal, storefront, login, email, invoice.
* “Preview draft” vs “Published”; a side-by-side diff mode for revisions.
* Preview never writes; publish is a separate, explicit action with a confirmation that
  names the scope and the revision.

### 10.3 Branding removal

A dedicated panel with a checklist of every Catalyst touchpoint:

| Touchpoint | State | Action |
|---|---|---|
| Browser title | overridden / default | text field |
| Meta description / OG / Twitter | overridden / default | text + asset |
| Favicon + Apple touch icon | overridden / default | asset |
| PWA manifest name/short name/icons | overridden / default | text + assets |
| Boot-screen mark | overridden / default | asset |
| Sidebar wordmark | overridden / default | logo light/dark |
| Auth pages wordmark | overridden / default | logo |
| Storefront header/footer | overridden / default | logo + footer text |
| Email logo + footer | overridden / default | asset + text |
| Invoice PDF letterhead | overridden / default | asset + legal name |
| MFA recovery-code export header | overridden / default | text |
| Service worker fallback title | overridden / default | text |
| API docs / error pages | overridden / default | text |

**“Remove all Catalyst branding”** sets every field to the operator's own values (or clears
them), then runs a verification pass: fetch `/`, `/products`, `/login`, the manifest, the OG
image headers, a test email and a sample PDF; assert the string “Catalyst” and the mark
digest do not appear. The result is shown as a pass/fail list, not a toast.

`general.company_name` remains the legal/merchant name for invoices and tax; display names
come from `theme.brand.name`. The UI explains the distinction so an operator does not change
their legal entity by accident.

### 10.4 Onboarding

A five-step wizard for a new installation: Brand → Preset → Colours → Storefront → Publish.
It uses sample data in the preview, can be skipped and resumed, and ends with a “what changed”
summary. The overview page keeps a “Finish setting up your theme” card until every step is
done.

### 10.5 History and rollback

Every publish writes an immutable revision and a `theme_publications` row. The history screen
lists scope, revision, author, time, note, and a structured diff (brand, colours, layout,
custom CSS). Actions: preview, restore as draft, publish. Retention: keep all revisions;
assets referenced by a revision are never garbage-collected.

---

## 11. Security model

1. **CSP stays strict.** `script-src 'self'; style-src 'self'; img-src 'self' data:;
   font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'`. The only change
   is `frame-ancestors 'self'` on the preview path (or a separate preview host later).
   Custom CSS is a stylesheet, not a `<style>` tag. Boot code is a script file, not inline.
2. **Custom CSS is parsed, not regex-filtered.** Use a real CSS parser
   (`github.com/tdewolff/parse/v2/css`), validate at write time and re-validate at serve
   time. Deny: `@import`, external `@font-face` sources, `expression()`, `behavior:`,
   `-moz-binding:`, `javascript:`, `url()` outside `/theme/assets/<digest>`, `image-set()`
   with external URLs, and `content: url()`. Bound size (256 KiB), selector count and
   nesting depth.
3. **`url()` is restricted to digest-addressed theme assets.** This is what stops CSS
   exfiltration: an attribute selector can only request an asset that already exists, not an
   arbitrary path with a secret in the query string.
4. **Assets are decoded and re-encoded.** PNG/JPEG are decoded (dimension caps, no
   decompression bombs), re-encoded to strip EXIF/metadata, and stored under a digest. SVG is
   parsed against an allowlist (no `<script>`, `<foreignObject>`, event attributes, external
   `href`, or `xlink:href`), and served with `Content-Security-Policy: default-src 'none';
   sandbox` plus `nosniff`. Fonts are validated by signature and served as `font/woff2`.
5. **No remote URLs in v1.** Logo/OG import is upload-only. If remote import is added, it
   needs SSRF defenses (scheme allowlist, DNS pinning, private/link-local/metadata IP block,
   redirect and size limits) — a separate, reviewed feature.
6. **Panel custom CSS is privileged.** It can hide staff UI. Require `theme:publish`, show an
   explicit risk acknowledgement the first time, and audit every publish. Storefront/portal
   custom CSS is scoped to those surfaces and cannot leak into the panel.
7. **Every sink escapes.** `panelName` into `<title>`/meta/JSON-LD, alt text, email HTML,
   PDF text, manifest JSON. The shell renderer uses `html/template` (contextual escaping);
   email uses `html/template` with inline styles; PDF uses the existing text writer.
8. **Preview tokens** are signed, short-lived, audience-bound, single-scope, and grant no
   session. Preview responses are `no-store` and `noindex`.
9. **Uploads and publishes are rate-limited and audited.** A double-submitted publish is
   idempotent; a replayed import is idempotent on the document digest.
10. **Do not widen `img-src` for external images.** If an operator wants a CDN image, upload
    it or proxy it; the CSP is a product feature.

---

## 12. Caching, performance and operations

* **Snapshot cache:** each server holds `atomic.Pointer[Snapshot]` per scope (resolved
  document, revision digest, generated CSS bytes, manifest JSON, ETag). Reads never hit the
  database on the hot path.
* **Invalidation:** publish writes the revision, updates `theme_assignments`, then
  `NOTIFY billing_theme, '<scope>:<revision>'`. Every instance listens and reloads. A 30 s TTL
  is the fallback for a dropped notification.
* **Revision-addressed URLs:** the shell links `/theme/<revision>/theme.css` and
  `/theme/<revision>/boot.js` with `Cache-Control: immutable, max-age=31536000`. A publish
  changes the shell (which is `no-cache`), so returning browsers pick up the new revision on
  the next navigation with no purge.
* **Assets:** digest URLs, immutable for a year, `Content-Length`, `nosniff`. Orphan GC runs
  daily and deletes unreferenced assets older than 30 days.
* **Generated CSS:** gzip/brotli on the fly (the existing server already compresses); the
  generated CSS for a preset is typically 20–40 KiB before compression. Custom CSS is
  included; a 256 KiB cap keeps the worst case bounded.
* **Metrics:** `theme_revision`, `theme_cache_hit`, `theme_publish_total`,
  `theme_css_bytes`, `theme_asset_bytes`. A `billing doctor` check verifies every scope has a
  published revision and its assets exist.
* **Backups:** themes are rows, assets are `bytea`; the existing database backup covers them.
  No filesystem volume is introduced.

---

## 13. Rollout plan and work packages

The plan is sequenced so that each phase ships something usable and `make verify` stays
green. Estimates assume one full-time engineer; parallelise where noted.

### Phase 0 — Foundations (1–2 weeks)

**Outcome:** the current look is served by the new system; nothing visibly changes.

* `internal/theme`: document types, validation, merge, canonical JSON, digest.
* `internal/theme/presets/catalyst-billing.json`: the current `tokens.css` values.
* `internal/theme/css`: generator (canonical + legacy aliases + game-panel HSL aliases).
* Migration: `themes`, `theme_revisions`, `theme_assignments`, `theme_assets`,
  `theme_publications`; seed the default theme and assignment.
* `internal/authn`: `theme:read/write/publish` + migration grants + role test update.
* `web/web.go`: render `index.html` from a Go template with theme values; add
  `/theme/<rev>/theme.css`, `/theme/<rev>/boot.js`, `/theme/assets/<id>`,
  `/theme/manifest.webmanifest`, `/theme/favicon.ico`, `/theme/og.png`.
* `web/src/lib/theme.ts`: read `data-theme`/`data-density`, personal preference, boot
  contract; keep the old localStorage key as a fallback.
* Replace hardcoded brand strings in `layout.ts`, `app.ts`, auth views, storefront,
  `icons.ts`, `sw.ts`, `mfa.ts`, `index.html`.
* `tokens.css` becomes the generated default (or is generated at build time from the
  built-in preset; the build must remain reproducible without a database).

**Acceptance:** a screenshot diff of every screen before/after is empty; `make verify` green;
a CSP test asserts no inline style/script and the theme routes are same-origin.

### Phase 1 — Theme Studio (2–3 weeks)

**Outcome:** an operator can pick a preset, edit brand/colours/typography/surfaces/layout,
preview, publish, and roll back.

* Staff API: list/get/draft/update/publish/restore/diff/import/export/presets/assets.
* Admin screens: overview, presets, brand, colors, typography, layout, assets, history,
  import/export.
* Colour harmony + contrast meter + auto-fix.
* Live preview iframe + signed tokens.
* Asset upload (raster + SVG + fonts) and library.
* Revision diff and rollback.
* Tests: unit (validation/merge/generator), API (permissions, idempotency, ETag), e2e
  (preset → edit → preview → publish → rollback).

### Phase 2 — Presets and token coverage (1–2 weeks)

**Outcome:** ten genuinely different designs, all accessible, all rendering every screen.

* Author the 10 preset documents (§8) and self-host the fonts.
* Density/shell variants; portal and auth page overrides.
* Contrast gate (AA/3:1) and visual regression screenshots per preset.
* `make theme-check`: no colour literals outside tokens; preset schema validation; generated
  CSS golden files.

### Phase 3 — Storefront designer (2–3 weeks)

**Outcome:** the storefront can be recomposed, not just recoloured.

* Layout schema + block registry + server-side validation.
* Renderer + built-in `classic` layout (current storefront, byte-comparable DOM).
* Storefront page/section editor with reorder, props, visibility, preview.
* Preset layouts: Classic, Showcase, Marketplace, Minimal, Single-product landing,
  Portal-first.
* Tests: layout parity, every block renders with empty/error data, e2e reorder/publish,
  accessibility (keyboard drag-reorder, focus management).

### Phase 4 — Branding removal, email and PDF (1–2 weeks)

**Outcome:** no Catalyst trace remains, and mail/invoices match the brand.

* Brand fields + one-click removal + verification report.
* Email HTML wrapper from tokens (inline styles, no `<style>`), logo, footer, test send.
* PDF logo/colours/font embedding (with the existing Helvetica fallback).
* Tests: grep + browser + email + PDF assertions for “Catalyst”; snapshot of the email and
  PDF.

### Phase 5 — Hardening and launch (1 week)

* Sanitizer bypass corpus, upload fuzz, SSRF tests (if remote import exists).
* Cache/ETag/multi-instance invalidation tests.
* Performance budget (theme.css size, first paint, LCP on the storefront).
* Documentation: operator guide, preset authoring, custom CSS policy, migration notes.
* Marketplace readiness: signed envelopes, version compatibility, preview thumbnails.

**Total:** roughly 8–12 engineer-weeks. Phases 2 and 3 can run in parallel with Phase 1 once
the document schema and the API contract are frozen.

---

## 14. Testing and gates

| Gate | What it proves |
|---|---|
| `go test ./internal/theme/...` | schema validation, merge/inheritance, canonical JSON, digest stability, CSS generator golden files |
| `go test ./internal/httpapi -run Theme` | permissions, CSRF/idempotency, ETag/304, cache headers, scope isolation, preview tokens |
| Sanitizer corpus | a growing list of bypass attempts (escaped `@import`, `url()` with query strings, `image-set`, `@font-face` external, `expression`, nested comments) is rejected |
| Contrast test | every preset's text/background and UI/border pairs meet AA / 3:1 |
| `make theme-check` | no hardcoded colours/radii/fonts outside the token and asset files; every preset validates; generated CSS is current |
| `make smoke` additions | `/theme/<rev>/theme.css` is 200 with the right type; CSP has no `unsafe-inline`; boot script runs; no console errors |
| `make e2e-ui` additions | apply preset → preview → publish → rollback; upload logo; remove branding; reorder a storefront section; every screen still renders |
| Visual regression | screenshots of every screen × every preset at desktop/tablet/mobile; diff threshold |
| Branding assertion | after “remove all branding”, `/`, `/products`, `/login`, manifest, OG, email and PDF contain no “Catalyst” and no Catalyst mark digest |
| `billing doctor` | each scope has a published revision; every referenced asset exists; custom CSS re-validates |

Add `theme-check` and the theme tests to `make verify`, next to `openapi-check` and
`client-check`.

### 14.1 What this plan deliberately does not do (v1)

* No free-form HTML block, no arbitrary JavaScript, no plugin-supplied CSS on the storefront.
  The section registry covers the legitimate layout needs; arbitrary markup is the largest
  XSS and brand-safety risk in the feature.
* No external asset URLs. Uploads only. A CDN-hosted logo is a request to widen `img-src` and
  to depend on a third party; both are avoidable.
* No per-tenant themes yet. The schema can grow a nullable `tenant_id`; the permission model
  and the assignment table are already scope-shaped.
* No theme marketplace in v1. The export envelope is signature-ready, but operating a
  marketplace (review, signing keys, abuse) is a separate product.
* No storefront SSR in v1. The shell's OG tags are themed; per-product OG/structured data
  waits for a server-rendering pass.
* No visual page-builder canvas (free positioning, absolute layout). Sections are ordered
  blocks with schemas, not a canvas. A canvas produces inaccessible, unmaintainable pages and
  is the wrong tool for a billing storefront.

---

## 15. Operator walkthrough (the experience this plan is buying)

A new operator, from a fresh install to a live branded storefront:

1. **Sign in** → a “Finish setting up your theme” card on the dashboard.
2. **Presets** → click `Catalyst Console` (or any of the ten) → the preview iframe updates
   instantly; “Apply as draft”.
3. **Brand** → upload a logo, mark, favicon, OG image; type the company name and tagline.
   The preview updates without a reload.
4. **Colors** → the contrast meter shows every pair; “Auto-fix” adjusts the two that fail.
5. **Storefront** → reorder the hero, add a “Featured products” section, hide the FAQ on
   mobile, set the hero image. Preview at 390px.
6. **Publish** → choose scopes (storefront + portal + email), confirm the diff summary.
7. **Remove Catalyst branding** → the checklist turns green; the verification pass fetches
   `/`, `/products`, `/login`, the manifest, a test email and a sample PDF.
8. **Done.** No rebuild, no restart, no ticket to the vendor. A second operator with
   `theme:read` can see the history; a `theme:write` operator can draft; only
   `theme:publish` can go live.

A returning operator who wants a seasonal campaign: duplicate the published revision, change
three colours and the hero, preview, publish, and roll back after the campaign — all without
touching the base theme.

### 15.1 Definition of done

* A fresh install renders the current look by default, and `Catalyst Console` matches the game
  panel's dashboard closely enough that the two products feel like one suite.
* An operator can change every visible brand element, upload every asset, and remove every
  Catalyst reference without editing a file or opening a ticket.
* The storefront can be recomposed into at least five materially different layouts.
* Custom CSS is accepted, validated, versioned and served without weakening the CSP.
* Every preset passes the contrast gate; every screen renders at 390/834/1440 without
  overflow; `make verify` is green.
* A publish is visible within one navigation on every instance; a rollback is one click.
* The audit log answers “who published what, when, and what changed”.

---

## 16. Risks and mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| Server-rendered shell breaks the SPA boot/CSP | High | Phase 0 only; keep the static shell as a fallback; smoke + e2e assert first paint, routing, CSP |
| Custom CSS exfiltration | High | `url()` restricted to digest assets; parser-based validation; scope isolation; audit |
| Panel CSS hides staff-critical UI | High | `theme:publish` + risk acknowledgement + audit; optional policy that disallows custom CSS on `panel` scope |
| Storefront layout regression | High | Built-in `classic` fallback; server validates layout; e2e renders every block |
| Migration of 50k LOC of CSS | Medium | Legacy aliases keep old CSS working; `theme-check` prevents new hardcoding; migrate file by file |
| Two sources of truth for the brand name | Medium | `theme.brand.name` is display; `general.company_name` is legal; UI explains; one-click removal sets both |
| Font licensing | Medium | Only open-licence families ship; uploaded fonts require an acknowledgement; no external font CDNs |
| Theme publish cache stampede | Low | Snapshot cache + NOTIFY + revision URLs; assets immutable |
| Marketplace supply chain | Low (future) | Signed envelopes, schema version, preview, no executable content |
| Preview token abuse | Low | HMAC, short TTL, user+revision bound, no API privilege, `noindex` |

---

## 17. Open questions

1. **Default preset.** Should a fresh install default to `Catalyst Console` (matching the game
   panel) or `Catalyst Billing` (the current tan/orange brand)? This plan recommends
   `Catalyst Console` for new installs and `Catalyst Billing` for upgrades. Confirm the
   game panel's intended primary: the demo stores `#ff3d7f` (rose), while its shipped preset
   is `#c48d5a` (bronze).
2. **Storefront SSR.** Per-product OG tags and real SEO need server rendering or a
   pre-render pass. Is that in scope for this project, or phase 5+?
3. **Custom HTML block.** Deliberately excluded from v1. Is there a must-have use case that
   the block registry cannot cover?
4. **Email HTML.** Today's mail is plain text. Do we want HTML email at all, or only a
   themed logo/footer on the existing plain-text bodies?
5. **PDF fonts.** Embedding a brand font in every invoice increases file size and needs the
   font's licence to allow embedding. Is the built-in Helvetica with themed colours enough
   for v1?
6. **Reseller/multi-tenant.** Should the schema carry a nullable `tenant_id` now, even if the
   feature is later? Adding it later is a migration but not a redesign.
7. **Theme marketplace.** Is a marketplace in scope, or only import/export? The envelope is
   ready for signatures either way.

---

## Appendix A — Game panel comparison (what “better” means concretely)

| Dimension | Game panel today | Billing plan |
|---|---|---|
| Brand assets | URL text fields; remote URLs may be blocked by CSP | Upload + digest + light/dark variants + alt text |
| Social card | None | Server-generated, themed, per scope |
| Banners/backgrounds | None | Asset library + background tokens |
| Colour editing | 3 brand + 12 surfaces; harmony generator | Full token contract; harmony; contrast meter with auto-fix; derived states |
| Typography | None | Families, scale, weights, tracking, self-hosted |
| Density/shape | Radius slider | Density, radius scale, border, elevation, motion, shell variant |
| Presets | 9 hue variations | 10 structurally different designs |
| Scopes | One | 5 with inheritance |
| Layout | None | Section composer + shell variants |
| Custom CSS | Regex blacklist + `<style>` | Parser + allowlist + first-party stylesheet + digest-restricted `url()` |
| History | None | Immutable revisions, diff, preview, rollback |
| Branding removal | Manual fields | One action + verification checklist |
| Email/PDF | None | Themed logo/colours/footer/letterhead |
| Accessibility | No contrast gate | AA gate per preset and per custom theme |
| Security | CSP likely widened for inline CSS | Strict CSP preserved |

## Appendix B — Branding touchpoint checklist

Symbols are authoritative; line numbers drift. Verified against the tree at the time of
writing.

- [ ] `web/src/index.html` — `<title>`, description, OG/Twitter, favicon, Apple icon,
      manifest, `theme-color`, inline boot mark.
- [ ] `web/src/app.ts` — three `document.title` templates and the fatal-screen heading
      (`app.fatal.title` fallback "Catalyst Billing is unreachable").
- [ ] `web/src/ui/layout.ts` — sidebar wordmark (`brand-name`) and the user-menu brand.
- [ ] `web/src/views/login.ts`, `register.ts`, `forgot-password.ts`, `reset-password.ts`,
      `verify.ts` — the auth brand lockup.
- [ ] `web/src/views/storefront.ts`, `storefront-product.ts` — header wordmark and footer.
- [ ] `web/src/views/invoice.ts` — merchant-name fallback in the print view.
- [ ] `web/src/lib/mfa.ts` — the recovery-codes export header ("Catalyst Billing — two-factor
      recovery codes").
- [ ] `web/src/lib/icons.ts` — the mark path's `#F97301` fill (`brandMark`).
- [ ] `web/src/sw.ts` — `FALLBACK_TITLE` and the cache names.
- [ ] `web/build.mjs` — manifest name/short name/description/colours/icons.
- [ ] `web/web.go` — headers and static asset names that mention the product.
- [ ] `internal/notify` — `orDefault(..., "Catalyst Billing")` in `events.go` (×2) and
      `manual_create.go`, plus the mail template defaults.
- [ ] `internal/settings/settings.go` — `general.company_name` default `"Catalyst Hosting"`
      (note: disagrees with the notify fallback).
- [ ] `internal/invoicepdf` — letterhead fallback and any hardcoded colours.
- [ ] `README.md`, `assets/brand/`, `scripts/brand/` — developer-facing only; not shipped to
      customers, but the “remove branding” report should say so explicitly.
- [ ] API docs/OpenAPI title and descriptions.
- [ ] Error pages (`not-found.ts`, the fatal screen) and the service-worker offline page.
- [ ] `localStorage` keys (`catalyst.theme`, `catalyst.*`): keep reading the old key for
      migration, write the new namespaced key.

## Appendix C — Example generated CSS (abridged)

```css
/* /theme/42/theme.css — revision 42, scope storefront, preset catalyst-console */
:root {
  color-scheme: dark;
  --font-display: "Inter", system-ui, sans-serif;
  --font-body: "Inter", system-ui, sans-serif;
  --font-mono: "JetBrains Mono", ui-monospace, monospace;
  --space-1: .25rem; --space-2: .5rem; --space-3: .75rem; --space-4: 1rem;
  --radius-sm: 4px; --radius-md: 6px; --radius-lg: 10px;
  --sidebar-w: 232px; --topbar-h: 56px; --content-max: 1440px;
  /* canonical */
  --color-bg: #0e1116;
  --color-surface-1: #161a21;
  --color-surface-2: #1d222b;
  --color-surface-3: #232a35;
  --color-border: #2b323d;
  --color-text: #e9edf2;
  --color-text-muted: #939dac;
  --color-primary: #ff3d7f;
  --color-primary-hover: #ff5a92;
  --color-on-primary: #0e1116;
  --color-success: #3fcf8e;
  --color-warning: #ffb020;
  --color-danger: #ff5a5a;
  --color-info: #6fa8ff;
  /* legacy aliases */
  --bg: var(--color-bg);
  --surface: var(--color-surface-1);
  --surface-2: var(--color-surface-2);
  --surface-3: var(--color-surface-3);
  --line: var(--color-border);
  --text: var(--color-text);
  --text-dim: var(--color-text-muted);
  --accent: var(--color-primary);
  --accent-ink: var(--color-on-primary);
  /* game-panel HSL aliases */
  --background: 220 22% 7%;
  --foreground: 218 30% 93%;
  --primary: 340 100% 62%;
  --border: 218 15% 20%;
  --radius: 0.25rem;
}
[data-theme="light"] { color-scheme: light; /* light tokens */ }
/* preset structural rules */
[data-shell="sidebar"] .sidebar { width: var(--sidebar-w); }
/* validated admin CSS */
```

## Appendix D — Work-package ownership map

| Package | Primary files | Reviewer |
|---|---|---|
| WP0 theme core | `internal/theme/**`, `0059_themes.sql` | backend |
| WP0 shell/CSP | `web/web.go`, `web/build.mjs`, `web/src/index.html` | frontend + security |
| WP0 runtime | `web/src/lib/theme.ts`, `web/src/styles/tokens.css` | frontend |
| WP1 studio API | `internal/httpapi/theme.go`, `internal/theme/store/**` | backend |
| WP1 studio UI | `web/src/views/theme-*.ts`, `web/src/ui/theme-*.ts` | frontend + design |
| WP2 presets | `internal/theme/presets/*.json`, `assets/fonts/**` | design + frontend |
| WP3 layout | `internal/theme/layout/**`, `web/src/lib/{layout,blocks}.ts` | frontend |
| WP4 brand/email/PDF | `internal/notify/**`, `internal/invoicepdf/**` | backend |
| WP5 hardening | tests, `Makefile`, docs | security |

## Appendix E — Phase 0 file checklist

A concrete, reviewable starting point. Every item is either a new file or a named change.

**Go core**

- [ ] `internal/theme/document.go` — `Document`, `Brand`, `Color`, `Typography`, `Space`,
      `Shape`, `Motion`, `Background`, `Layout`, `Components`.
- [ ] `internal/theme/validate.go` — closed validation for every field (§4.1).
- [ ] `internal/theme/merge.go` — deep merge + `inherit: false`.
- [ ] `internal/theme/canonical.go` — sorted-key JSON + SHA-256 digest.
- [ ] `internal/theme/css/generate.go` — canonical vars, legacy aliases, game-panel HSL
      aliases, preset structural rules.
- [ ] `internal/theme/css/sanitize.go` — parser-based custom-CSS validation.
- [ ] `internal/theme/presets/embed.go` + `internal/theme/presets/catalyst-billing.json`.
- [ ] `internal/theme/store/store.go` — themes, revisions, assignments, assets, publications.
- [ ] `internal/theme/cache/cache.go` — atomic snapshot + `LISTEN/NOTIFY`.
- [ ] `internal/theme/assets/upload.go` — decode, re-encode, digest, dimensions.
- [ ] `internal/httpapi/theme.go` — public + staff routes (§6).
- [ ] `internal/store/migrations/0059_themes.sql` — §5 DDL, permission grants, default seed.
- [ ] `internal/authn/authz.go` — `PermThemeRead`, `PermThemeWrite`, `PermThemePublish`;
      update `AllPermissions` and the role test.

**Web shell**

- [ ] `web/web.go` — `ThemeProvider` interface, server-rendered shell, theme routes, preview
      header branch, cache of rendered shells.
- [ ] `web/src/index.html` — replace hardcoded brand/meta with `{{THEME_*}}` placeholders;
      keep `{{STYLE}}`/`{{SCRIPT}}`.
- [ ] `web/build.mjs` — leave theme placeholders intact; keep the default manifest/icons for
      the no-theme case.
- [ ] `web/src/lib/theme.ts` — read the boot contract, personal preference, density, preset;
      keep `catalyst.theme` as a read-only fallback.
- [ ] `web/src/lib/brand.ts` — `brandName()`, `productName()`, `logoSrc()`, `markSrc()`,
      `titleFor(page)`; every hardcoded string in Appendix B calls this.
- [ ] `web/src/styles/tokens.css` — generated defaults or generated at build time; legacy
      aliases preserved.
- [ ] `web/src/ui/layout.ts`, `app.ts`, auth views, storefront views, `icons.ts`, `sw.ts`,
      `mfa.ts` — replace hardcoded brand strings and the `#F97301` mark fill.
- [ ] `web/src/views/theme-*.ts` — route stubs for the studio (Phase 1 fills them).

**Tests**

- [ ] `web/web_test.go` — shell renders with a theme provider; CSP unchanged; no inline.
- [ ] `internal/theme/..._test.go` — validation, merge, digest, CSS golden, sanitizer corpus.
- [ ] `internal/httpapi/theme_test.go` — permissions, ETag/304, scope isolation, assets.
- [ ] `scripts/theme-check.mjs` — no colour literals outside tokens; presets valid.
- [ ] `Makefile` — `theme-check` in `verify`.
- [ ] `scripts/browser-smoke.sh` — theme CSS/JS routes return 200 and the right types; CSP
      still has no `unsafe-inline`; `frame-ancestors 'none'` on `/`.
- [ ] `scripts/e2e-ui.mjs` — a theme screen renders, apply/publish/rollback flow.

---

*This document is the design contract. When implementation departs from it, record the
departure and the reason here, the way `docs/ui-gate.md` does — the reasoning is the
expensive part.*
