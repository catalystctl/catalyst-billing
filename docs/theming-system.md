# Centralized theming system — design and implementation plan

**Status:** proposed, reconciled against HEAD `7b9a705` and the current worktree. The palette and
layout values called out below include an uncommitted restyle; treat those as in-progress input
until that work is merged.
**Scope:** `catalyst-billing` (Go core + embedded TypeScript frontend)
**Goal:** one theming system that an operator can use to (a) re-skin the entire panel and
storefront from a preset, (b) upload their own logo, favicon, social card, banners and fonts,
(c) remove every trace of Catalyst branding, (d) write custom CSS, and (e) recompose the
storefront layout — without a fork, a rebuild, or a CSP exemption.

**Decisions requested before implementation:**

1. Confirm the default for new installations: `Catalyst Console` (game-panel match, rose
   `#ff3d7f` on `#0e1116`) or `Catalyst Billing` (the current warm rust/orange restyle,
   still uncommitted). This plan recommends `Catalyst Console` for new installs and preserving
   the installed token set on upgrades.
2. Confirm whether per-product SEO/OG (server rendering of the storefront) is in scope.
3. Confirm whether existing email-brand and invoice-template controls should be surfaced inside
   Theme Studio in its first release or linked to their current settings screens.
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

Billing has also moved since the first draft of this plan. It now has a tested tenant-brand
overlay (name, uploaded logo, light/dark accents) reaching the panel and PDF; an invoice PDF
presentation setting; HTML multipart email branding; and merchandising controls for featured
products, category images, stock and announcements. These are separate existing capabilities,
not a theme registry. The plan below treats them as the compatibility foundation and focuses
the new work on a complete token system, presets, page composition and a unified editing
experience. It must not rebuild or create a second source of truth for those features.

| Capability | Game panel | Billing (this plan) |
|---|---|---|
| Brand name / logo / accent | URL fields | Keep billing's built name/logo/accent API; expose it inside Theme Studio without duplicating storage |
| Social card (OG) | — | Still missing; make configurable/generated |
| Banners / backgrounds | — | Still missing as page-level assets; compose with existing announcement feed |
| Colours | 3 brand + 12 surfaces × 2 modes | Full token contract; preserve the existing per-mode brand accent overlay |
| Typography | — | Font families, scale, weights, tracking; self-hosted fonts |
| Density / shape / elevation / motion | Radius only | Full knobs, with presets that differ structurally, not just by hue |
| Presets | 9 hue variations | 10 genuinely different designs, including one that matches the game panel |
| Scopes | One | Panel, portal, storefront, email, invoice — themes layer over existing identity/mail/PDF settings |
| Layout | — | Section-based page composition + shell variants; retain existing merchandising and announcement data |
| Custom CSS | Regex blacklist, `<style>` tag | Parsed and validated, served as a first-party stylesheet |
| History | — | Immutable revisions, diff, preview, publish, rollback |
| Branding removal | Name/logo/accent and mail/PDF settings exist separately | Unify workflow, add missing favicon/OG/assets, and verify all surfaces |

The plan’s original CSP decision remains sound: **theme CSS and theme boot code are first-party
HTTP resources, not inline markup.** `web/web.go` enforces `script-src 'self'; style-src 'self'`
with no `'unsafe-inline'`. Existing brand CSS is applied through a small, tested set of inline
custom properties on the root (accent family only); that narrowly-scoped mechanism is distinct
from injecting admin-authored CSS. Do not widen it into a theme mechanism.

* `GET /theme/<revision>/theme.css` — generated tokens + preset CSS + validated custom CSS.
* `GET /theme/<revision>/boot.js` — a tiny pre-paint script for personal preferences.
* `/api/v1/media/{id}` — keep using the existing media route for the tenant logo and catalog
  images; introduce new theme-asset storage only for asset classes the current store cannot
  represent (such as larger backgrounds or self-hosted fonts).

---

## 2. What exists today

### 2.1 Billing frontend

* Framework-free TypeScript DOM app (`web/src`), bundled by esbuild into committed `web/dist`
  and embedded by `web/web.go`.
* `tokens.css` already provides a shared variable vocabulary for both light and dark themes;
  `web/src/lib/theme.ts` stores the mode in `localStorage`. Recent dashboard, shell and
  storefront work changes token values and layout dimensions, not the underlying two-mode
  architecture.
* **Current worktree palette (in progress, not yet merged):** dark ground `#171413`, side
  `#201c1a`, surfaces `#1e1a18/#201c1a/#29231f`, orange accent `#ff951e`; light ground
  `#f5f2ee`, side `#ebe6e0`, surfaces `#fdfaf6/#f9f7f3/#f1eae3`, accent `#d36c09`. The
  worktree also tightens radii to 4px for most controls, changes the sidebar to 242px, topbar
  to 55px and content max to 1900px, and adds dashboard-specific components. Recheck the
  merged token file before freezing a preset; do not treat these in-flight values as the final
  Catalyst Billing design.
* Tenant identity is no longer hardcoded: `GET /api/v1/branding` supplies the public name,
  logo and resolved light/dark accent; `web/src/lib/brand.ts` applies the current mode's
  accent family, and `/branding` edits it. The existing theme toggle and brand overlay are
  independently tested. This is a constrained brand overlay, not the theme registry proposed
  here.
* Storefront composition is still fixed in code (`web/src/views/storefront.ts`), but the data
  is more configurable than the first plan implied: `products.featured`, category images,
  stock/quantity rules, the storefront settings group, and scheduled announcements already
  influence what is shown. The future layout renderer should consume these existing APIs and
  rows, not create replacement merchandising data.
* Existing browser tests include `web/test/brand.test.mjs`, `web/test/styles.test.mjs`, and
  `scripts/e2e-ui.mjs`; extend those rather than inventing parallel guards.

### 2.2 Billing backend

* `internal/settings` is a typed registry over `(group, key, value)`: absent rows use code
  defaults; each value is capped at 64 KiB; secrets are redacted; writes are audited.
* Routes are table-driven (`internal/httpapi/routes.go`) with access class, permission, CSRF
  and idempotency. Permission-coverage and OpenAPI-parity tests are gates.
* **Tenant brand is built** (`0060_branding.sql`, `internal/brand`, `internal/httpapi/branding.go`):
  `branding.name`, `logo_id`, `accent_light`, `accent_dark`; public `GET /api/v1/branding`,
  staff read/write, and logo upload/delete. The name/logo reach panel chrome, storefront and
  sign-in views; the brand and logo also reach invoice PDFs. Logo bytes reuse `product_images`,
  capped at 256 KiB. The no-brand fallback is pinned byte-identical in CSSOM behaviour and PDF
  output. Keep this API and data contract backward-compatible.
* **Invoice presentation is built:** `invoices.pdf_template` is validated JSON
  (`invoicepdf.Template`) for document headings, bill-to/footer/total labels and accent. Brand
  logo/name/accent are applied separately. Extend this contract rather than creating a second
  PDF template store.
* **Email branding is built** (`0068_mail_brand_and_problems.sql`): `mail.brand_html`, header,
  footer, accent and logo URL; multipart/alternative HTML is rendered at event time and stored
  in `notification_deliveries.body_html`, while the plain-text body remains. The HTML mail logo
  is intentionally an absolute URL because recipient mail clients fetch it. Preserve that
  compatibility, and keep it distinct from same-origin browser assets.
* **Storefront merchandising is partly built** (`0061_storefront_merchandising.sql`): category
  images, featured products, stock and quantity policy; announcements have placement/schedule
  and appear on storefront/customer surfaces. These are content/data controls, not page-layout
  controls.
* The current migration head is `0072_worker_instances.sql`; a future theme-registry migration
  must take the next free number at implementation time, not assume `0059`/`0073` remains free.
* Migrations are transactional/checksummed; `billing doctor` checks schema health.

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
* **Gaps:** no versioned theme registry, presets, per-scope overrides, layout composition,
  revision history or per-page templates. Billing already has identity/logo/accent and email/
  PDF branding, so those are not feature gaps or fresh work in billing.

---

## 3. Design principles

1. **A theme is data, not code.** One versioned JSON document per scope. No theme requires a
   deploy; no deploy changes a theme.
2. **A preset is a starting point, not a mode.** Applying a preset creates a draft revision
   the operator can edit. Presets are embedded JSON in the binary, importable and exportable.
3. **The token contract is the API.** Components only ever read CSS variables. A theme that
   does not define a token inherits it from its parent scope or the built-in default.
4. **The strict CSP does not move.** No `'unsafe-inline'`, no nonce for admin-authored CSS,
   and no arbitrary external origins for browser theme resources. Theme CSS/JS/assets are
   same-origin, revision-addressed resources (configured CAPTCHA origins remain a separate,
   existing exception).
5. **Compatibility first.** Preserve the existing brand overlay, per-user light/dark choice,
   invoice PDF template, mail branding settings and storefront merchandising APIs. A Theme
   Studio may present them together, but it must not introduce duplicate settings or silently
   change an existing installation's rendering.
6. **A scope is a trust boundary.** Storefront custom CSS can never style the staff panel.
   Panel custom CSS is a privileged capability and is audited.
7. **Every claim is testable.** Contrast, CSP, sanitizer bypasses, cache headers, preset
   rendering and branding coverage all get automated gates.

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
   │  theme.css         │   │  (manifest: tokens,  │   │ existing brand/PDF  │
   │  boot.js           │   │   layouts, refs)      │   │ mail adapters        │
   │ /theme/assets/<id> │   └──────────┬───────────┘   └────────────────────┘
   └──────────┬─────────┘              │
              │                        │
   ┌──────────▼────────────────────────▼───────────────────────────────────┐
   │ Server-rendered HTML shell (title, meta/OG, favicon, manifest, mark)  │
   │ + existing SPA bundle                                                 │
   └───────────────────────────────────────────────────────────────────────┘
```

### 4.1 The theme document (schema v2)

The theme document stores **presentation tokens, shell settings, page layouts and validated
custom CSS only**. It deliberately does not duplicate tenant identity, mail branding or the
PDF template. Those values remain in the existing `branding.*`, `mail.brand_*` and
`invoices.pdf_template` settings contracts; Theme Studio can aggregate-edit those controls,
but revisions must not create a second source of truth. A public theme manifest may compose
the safe values for rendering.

Every theme field is optional; missing fields inherit. A complete example is in §7.

```jsonc
{
  "schema": 2,
  "id": "catalyst-console",
  "name": "Catalyst Console",
  "description": "Dense, dark-first console matching the game panel.",
  "extends": null,                    // preset id or parent theme id
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
* assets: theme document refs use `theme-asset:<uuid>` for new favicon/OG/banner/background/font
  assets. Existing tenant logo stays as `branding.logo_id` referencing `product_images`; it is
  never imported into or copied by a visual theme revision;
* layout: every section type exists in the block registry and every prop validates against
  that block's schema;
* total document size ≤ 1 MiB (excluding asset bytes); `customCss` ≤ 256 KiB;
* unknown top-level keys are rejected (a typo must not be silently ignored).

### 4.2 Scopes and inheritance

| Scope | Used by | Default | Can override |
|---|---|---|---|
| `panel` | Staff admin SPA | `Catalyst Console` on new installs; migrations preserve installed tokens | tokens, shell layout, custom CSS |
| `portal` | Customer area | inherits panel presentation | tokens, customer shell layout, custom CSS |
| `storefront` | Public catalogue, cart, checkout, auth pages | inherits panel presentation | tokens, full page composition, custom CSS |
| `email` | Transactional mail | existing `mail.brand_*` settings stay authoritative | email-safe presentation, rendered through `notify.MailBrandFor` |
| `invoice` | PDF invoices/credit notes | existing `branding.*` plus `invoices.pdf_template` stay authoritative | PDF-safe presentation, rendered through `invoicepdf` |

Brand identity is currently installation-wide (name, logo, per-mode accents). Email has
separate header/footer/accent/logo URL settings, and PDF wording/accent has a validated
settings document. Scope-specific brand overrides are a future product decision, not an
implicit `brand` subtree in every theme. The initial theme resolver can share/inherit visual
tokens while composing the appropriate existing identity configuration for each renderer.

Resolution is a deep merge: `built-in default ← parent scope ← scope document`. A scope can
set `"inherit": false` to start from the built-in default instead. The merge is performed
once at publish time and the resolved document is stored alongside the draft (so a later
change to the parent cannot silently mutate a published storefront). Publishing a parent
offers to re-resolve children; the UI shows a diff.

### 4.3 Presets

Presets are embedded JSON files (`internal/theme/presets/*.json`, `go:embed`) validated by
the same code path as an imported theme. Applying a preset creates a **draft revision**; it
never publishes directly. The catalogue is in §8.

Import/export uses an envelope for presentation documents and theme-only assets:

```jsonc
{
  "kind": "catalyst.theme",
  "schema": 2,
  "exported_at": "2025-01-01T00:00:00Z",
  "theme": { /* tokens/layout document */ },
  "assets": [ { "id": "...", "kind": "background", "content_type": "image/webp", "digest": "sha256:...", "data_base64": "..." } ]
}
```

The existing tenant logo, mail logo URL and legal/company settings are not silently exported
with a visual theme. A separate full-brand export can be added with explicit authorization and
warning. The export is deterministic (sorted keys, fixed asset order); a future marketplace
can add a detached signature over the canonical JSON.

---

## 5. Data model

Migration `<next-free-number>_themes.sql` (check `internal/store/migrations` when
implementation begins; the current head is `0072`). All tables are owned by the migration
role; the runtime role gets `SELECT/INSERT/UPDATE/DELETE` only, matching the existing
privilege model.

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

-- Uploaded theme-only assets such as fonts and full-width backgrounds. Existing tenant logos
-- and catalog/category images continue to use product_images; their bytes are not copied here.
-- The digest index includes kind because one byte payload may serve multiple theme roles;
-- existing product_images stay the canonical store for tenant and catalog logos.
CREATE TABLE theme_assets (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text NOT NULL CHECK (kind IN ('favicon','og','banner','background','font','other')),
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

A seed row creates a published baseline theme for each scope. On upgrade it must preserve the
merged current token set (including a tenant's brand overlay); fresh-install bootstrap may
select `Catalyst Console` after installing its preset document. Do not make the migration alone
overwrite existing `tokens.css`/brand settings with a new palette.

### 5.1 Go types

```go
package theme

type Document struct {
    Schema     int               `json:"schema"`
    ID         string            `json:"id"`
    Name       string            `json:"name"`
    Extends    string            `json:"extends,omitempty"`
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
| `internal/theme/email` | adapter to existing `notify.MailBrandFor` / `HTMLPart`; only new email-safe token conversion |
| `internal/theme/pdf` | adapter to existing `invoicepdf.Template` + brand; no second template renderer |

### 5.2 Notes that constrain the implementation

These are the repo facts that most often surprise a first implementation:

* `web/web.go` sets one global CSP for every response. A preview iframe needs
  `frame-src 'self'` and `frame-ancestors 'self'`; do **not** relax the global policy. Give the
  preview path its own header branch (and keep `frame-ancestors 'none'` on `/`), otherwise
  `scripts/browser-smoke.sh` (which asserts `frame-ancestors 'none'`) and `web/web_test.go`
  fail — correctly.
* `web/web_test.go` asserts that the shell contains no inline `<script>` and no literal inline
  `style=` attribute. The existing brand overlay does use CSSOM `style.setProperty` for a
  restricted accent family after the stylesheet loads; preserve that explicitly-tested
  exception. Theme CSS itself must remain a first-party stylesheet, not an injected `<style>`.
* `scripts/brand/verify.mjs` asserts `og:image` is 1200×630, the manifest names the 192/512
  icons, `mark.svg` carries the traced path, and the favicon decodes as an image. The default
  assets must keep these contracts until themed replacements exist.
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
* Two different concepts must stay distinct: `general.company_name` is a legal/merchant
  setting; `branding.name` is the displayed tenant name. Email uses `MailBrandFor` and its
  own `mail.brand_*` keys; PDF output uses `invoicepdf.Template`. The old `docs/branding.md`
  §5 and the NTF-11/I18N-06 prose in `docs/paymenter-parity.md` still describe mail as
  unbranded, although `0068_mail_brand_and_problems.sql` and `internal/notify/brand.go` now
  implement it. Reconcile those docs in the documentation follow-up; do not base this design
  on their stale description.

---

## 6. API surface

The existing endpoints remain in service and are not replaced by `/api/v1/theme`:

| Existing interface | Compatibility contract |
|---|---|
| `GET /api/v1/branding` | Public, pre-paint resolved tenant identity; keep the response shape stable |
| Staff branding/settings routes | Continue writing `branding.*` and uploaded logo references with existing validation/audit |
| `mail.brand_*` settings | Keep current HTML/plain-text fallback, URL logo semantics and event-time body snapshot |
| `invoices.pdf_template` | Keep `invoicepdf.Template` validation and built-in fallback; expose its controls through Theme Studio, not a duplicate document |
| Storefront merchandising/announcements APIs | Keep the existing product/category/announcement rows and visibility rules as block data sources |

New public routes then deliver the theme token/layout manifest and revision-addressed CSS/JS;
the manifest can compose the public brand response, but should not introduce a second writer.

### 6.1 Public (no session)

| Method | Path | Purpose | Cache |
|---|---|---|---|
| `GET` | `/api/v1/theme` | Resolved manifest for the caller's scope (storefront for anonymous, panel/portal for a session) | `public, max-age=60, stale-while-revalidate=300`; ETag |
| `GET` | `/api/v1/theme/{scope}` | Manifest for a named scope (storefront/email/invoice only; panel/portal require a session) | same |
| `GET` | `/theme/{revision}/theme.css` | Generated tokens + preset CSS + custom CSS | `immutable, max-age=31536000` (revision-addressed) |
| `GET` | `/theme/{revision}/boot.js` | Pre-paint personal-preference script | immutable |
| `GET` | `/theme/assets/{id}` | Theme-only image/font bytes; existing logo/catalog media stays on `/api/v1/media/{id}` | immutable, `nosniff`; sandbox CSP for SVG |
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
| `POST` | `/api/v1/staff/theme/branding/remove-catalyst` | `theme:publish` | Updates supported display/mail/browser fields through their existing validated stores; must not rewrite legal entity data |

Mutations use the existing `staffMutation` wrapper: CSRF, `Idempotency-Key` required, rate
class `mutation`, audit row in the same transaction.

New permissions apply to the new theme document and publishing flow only. Existing branding
and settings endpoints continue to use their current `settings:write` policy until an explicit
compatibility migration says otherwise. The new `theme:read`, `theme:write`, and
`theme:publish` permissions must not silently grant write access to mail or invoice settings.

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

`tokens.css` currently defines one existing billing vocabulary, with names such as
`--bg`, `--surface`, `--side`, `--top`, `--line`, `--text`, `--accent`, `--success`,
`--warning`, `--danger`, `--info`, `--s-1…8`, `--r-xs…full`, `--shadow-*`, `--ease` and
`--dur-*`. The in-progress restyle adds/changes values including `--surface-active`,
`--line-soft`, `--text-subtle` and `--pink`. The actual list is intentionally not frozen in
this plan; generate the compatibility map from the merged stylesheet during implementation.

The generator emits current aliases from `tokens.css`, keeping existing CSS working while
canonical names are introduced file by file. Extend `web/test/styles.test.mjs` with a narrow
token-reference check so a variable missed by the generator cannot silently stop theming.

The tenant brand remains a separate overlay over generated theme tokens. Runtime precedence is
`preset/theme token → selected light/dark mode → optional branding accent-family override`;
`brand.applyBrand` may set only `--accent`, `--accent-ink`, `--accent-text`, `--accent-soft`
and `--accent-soft-strong` for the current mode. The theme's base `color.brand` values are
preset defaults, not a replacement for `branding.accent_light/accent_dark`.

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

Only variables. A new rule that hardcodes a colour, font stack, radius or shadow should fail
a token check. Build the check by extending `web/test/styles.test.mjs`, which already verifies
that rendered classes have styling; don't quote the old literal counts because they move with
the active dashboard restyle. Keep a measured baseline in the test output.

---

## 8. Preset catalogue

Ten presets, each a full document (not a hue swap). All must pass WCAG AA for body text and
3:1 for UI borders/focus rings; the contrast report is generated by a test, not by hand.

| # | Preset | Character | Default mode | Radius | Density | Signature |
|---|---|---|---|---|---|---|
| 1 | **Catalyst Console** | The game panel's dashboard: near-black `#0e1116`, compact, 4px radius, pink/rose primary `#ff3d7f` | dark | 4px | compact | Dense tables, flat cards, thin borders |
| 2 | **Catalyst Billing** | Current billing identity with the warm rust/orange palette in the in-progress restyle; finalize from the merged `tokens.css` | dark | 4px controls / 8px large | compact/comfortable | Warm, dense operator UI; keep the tenant's accent overlay separate |
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

The default for **new** installations is recommended to be `Catalyst Console`, as requested
for a suite-wide game-panel match. Existing installations preserve their currently merged
light/dark tokens and brand settings on upgrade; never replace their palette merely by adding
theme support. `Catalyst Billing` is the preset name for the current warm restyle once it is
merged, not a claim that its in-progress values are final.

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
| control radius | 4px |

The shell dimensions are billing choices, not values verified from the game panel. For
reference, the game panel's *shipped* `Catalyst Bronze` preset is primary `#c48d5a`,
secondary `#5ac4c2`, accent `#5a5cc4` on the same dark ground. Include it as preset 3; it is
the conservative choice if an operator wants the console palette with the warmer Catalyst
accent.

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
        "media": "theme-asset:<id>",
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
* `product_grid` | source (all/featured/category), category, columns, limit, showPrice, showCategory | existing catalog APIs; reuse `featured`, stock, images and visibility rules |
| `announcements` | placement, visual variant | existing scheduled announcement feed; do not duplicate announcement rows |
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

The section layout renders the existing announcement feed rather than creating duplicate
announcement content. `announcement_bar` is a visual treatment of a published storefront
announcement; a separate `announcements` section may render the existing scheduled feed.

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
The renderer must consume existing storefront data as-is: `featured` is only a promotion flag,
visibility and prices remain server-enforced, stock/quantity comes from existing catalogue
rows, and announcement placement/schedule stays authoritative. Layout documents control
arrangement and styling; they cannot make a hidden product visible or fabricate price, tax or
stock.

* SEO: the shell's title/description/OG come from the server. Per-product OG/structured data
  requires SSR or a pre-render pass and remains a future phase, pending the scope decision in §17.

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
/settings/theme/brand           Brand Studio entry that links to existing `/branding`: name, logo, mode accents; add new favicon/OG controls
/settings/theme/colors          Theme tokens, semantic colours, surfaces, contrast meter, harmony and auto-fix
/settings/theme/typography      Families, scale, weights, tracking, font uploads
/settings/theme/layout          Shell: nav, density, width, card/table style, radius, motion
/settings/theme/storefront      Page/section composer (§9.4)
/settings/theme/portal          Portal + auth page overrides
/settings/theme/email           Existing mail-brand settings and preview/test send
/settings/theme/invoice         Existing PDF template settings and sample PDF
/settings/theme/assets          Asset library: upload, crop/focal point, variants, alt text
/settings/theme/css             Custom CSS editor with validation, scope warning, diff
/settings/theme/history         Revisions, who/when, diff, preview, restore, publish
/settings/theme/import-export   JSON import/export, preset envelope
```

A left sub-nav mirrors these sections; keep `/branding` as a stable deep link during
migration. The first implementation should reuse its existing identity form/API instead of
creating a second writer for `branding.*`. Existing mail and invoice settings are surfaced
through adapters to their current validated settings and renderers.

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

A dedicated panel with a checklist that distinguishes **already configurable** values from
new work:

| Touchpoint | Current state | Theme-system action |
|---|---|---|
| Panel name, logo, light/dark accent | `/branding` and `branding.*` already work | reuse existing API; preserve no-brand fallback |
| Email HTML, header, footer, accent, logo URL | `mail.brand_*` already works | include existing controls/preview; keep plain-text alternative |
| PDF headings/footer/accent | `invoices.pdf_template` + brand overlay already work | expose existing template + PDF preview |
| Browser title | fixed shell default | make theme-driven |
| Meta description / OG / Twitter | default OG asset exists; no tenant editor | configurable text + OG image |
| Favicon + Apple touch icon | default assets only | upload/asset reference |
| PWA manifest name/short name/icons | default manifest exists | theme-driven fields |
| Boot-screen mark | default mark exists | replaceable asset |
| Storefront header/footer | brand partly reaches chrome; section order fixed | use existing brand + layout document |
| MFA recovery-code export header | static product text | decide whether security artifacts should be white-labelled |
| Service worker fallback title | default string | theme-driven title |
| API docs / developer errors | product-level surface | define what is customer-facing before promising total white-labeling |

**“Remove Catalyst branding”** updates existing tenant/mail brand settings and the new
browser-only fields. It must not silently rewrite `general.company_name` or statutory invoice
data: the screen explains that the legal merchant name may remain on invoices. Its report
checks `/`, `/products`, `/login`, manifest, OG, a test email and a sample PDF, distinguishing
presentation strings from legal identity, source code and developer-facing docs.

The existing `branding.name` is the display name; `general.company_name` remains the legal/
merchant value. Existing mail and PDF configuration retain their own validated settings; no
`theme.brand.name` field is added to the persisted theme document.

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
5. **No browser remote URLs in v1.** Browser logo/OG import is upload-only. The existing
   `mail.brand_logo_url` is a separate compatibility exception: the recipient's mail client
   fetches it and the billing app does not proxy/fetch it. Preserve that contract or migrate it
   in a separately reviewed change.
6. **Panel custom CSS is privileged.** It can hide staff UI. Require `theme:publish`, show an
   explicit risk acknowledgement the first time, and audit every publish. Storefront/portal
   custom CSS is scoped to those surfaces and cannot leak into the panel.
7. **Every sink escapes.** Display name into `<title>`/meta/JSON-LD, alt text, email HTML,
   PDF text and manifest JSON. The shell renderer uses `html/template` (contextual escaping);
   existing mail/PDF renderers keep their own tested escaping paths.
8. **Preview tokens** are signed, short-lived, audience-bound, single-scope, and grant no
   session. Preview responses are `no-store` and `noindex`.
9. **Uploads and publishes are rate-limited and audited.** A double-submitted publish is
   idempotent; a replayed import is idempotent on the document digest.
10. **Keep browser CSP origins tight.** Do not widen `img-src` for custom external images; use
    same-origin digest assets.

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

### Phase 0 — Reconcile and preserve existing appearance (short verification pass)

**Outcome:** the theme work starts from the actual current product, not stale assumptions.

* Confirm the in-progress restyle is merged or rejected. Freeze the built-in `Catalyst Billing`
  preset from merged `tokens.css`, and reconcile the base accent against `brand.ts` and
  `brand.test.mjs`. An unset brand must remain byte-identical to that merged baseline.
* Treat `/branding`, `mail.brand_*`, `invoices.pdf_template`, merchandising fields and
  announcements as existing capabilities. Do not duplicate their settings, copy current logo
  bytes into a new asset table, or rewrite their renderers.
* Reconcile `docs/branding.md` with the newer behavior: it understates storefront/sign-in brand
  reach and still says mail is unbranded despite migration `0068`. Reconcile the affected
  Paymenter parity rows at the same time.
* Run `brand.test.mjs`, `mail-brand.test.mjs`, invoice PDF fallback tests,
  `web/test/styles.test.mjs`, and the existing real-browser brand assertions before changing
  CSS/runtime ordering.

**Acceptance:** no new theme is selected and the brand/mail/PDF output remains unchanged;
`make verify` passes.

### Phase 1 — Theme document and runtime (2–3 weeks)

**Outcome:** presets and scope-specific presentation can be previewed/published without
changing existing brand, mail or PDF settings contracts.

* Define the versioned appearance/layout schema, validator, inheritance, revision store and
  digest-addressed generated CSS/boot resources.
* Compose public brand data from `GET /api/v1/branding`; apply the existing accent overlay
  after theme tokens, preserving its tested current-mode-only precedence. Preserve the
  `localStorage` light/dark choice.
* Render the HTML shell with theme title/OG/favicon/manifest values; keep global CSP strict
  and use route-specific frame headers only for preview.
* Add the theme permissions/routes and adapters for existing identity, email and PDF settings;
  do not silently widen existing `/branding` or settings permissions.
* Tests: schema/default resolution, generated CSS, brand/no-brand precedence, CSP, ETag/cache,
  preview token and the existing end-to-end suite.

### Phase 2 — Theme Studio and presets (2–3 weeks)

**Outcome:** one operator workflow for theme controls, with ten distinct presentation presets.

* Build the preset gallery, token/surface editor, typography, density/layout controls, contrast
  report, live preview, import/export, immutable revisions and rollback.
* Link the existing Branding, Mail and Invoice controls rather than creating parallel editors;
  move them into one visual studio only through adapters that preserve validation/audit.
* Extend `web/test/styles.test.mjs`, `brand.test.mjs`, `mail-brand.test.mjs` and real-browser
  assertions; add preset contrast and screenshot coverage.

### Phase 3 — Storefront designer (2–3 weeks)

**Outcome:** the storefront can be recomposed, not just recoloured.

* Layout schema + block registry + server-side validation.
* Renderer + built-in `classic` layout (current storefront, byte-comparable DOM).
* Storefront page/section editor with reorder, props, visibility, preview.
* Preset layouts: Classic, Showcase, Marketplace, Minimal, Single-product landing,
  Portal-first.
* Tests: layout parity, every block renders with empty/error data, e2e reorder/publish,
  accessibility (keyboard drag-reorder, focus management).

### Phase 4 — White-label assets and renderer integration (1–2 weeks)

**Outcome:** operators can replace the remaining Catalyst browser assets and preview existing
mail/PDF branding in the same workflow.

* Add favicon, OG/social-card, banners/backgrounds and self-hosted font support, distinct from
  the existing logo/media store and email-client logo URL.
* Integrate previews for current `mail.brand_*` and `invoices.pdf_template`; change
  `notify`/`invoicepdf` only for concrete new rendering requirements.
* Add a branding-removal checklist and report; never rewrite legal merchant identity
  implicitly.
* Verify existing unconfigured mail/PDF fallbacks plus configured output in browser, email
  and PDF tests.

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
| `make theme-check` | extend existing stylesheet checks with token-reference validation; validate presets and generated CSS |
| `make smoke` additions | `/theme/<rev>/theme.css` is 200 with the right type; CSP has no `unsafe-inline`; boot script runs; no console errors |
| `make e2e-ui` additions | apply preset → preview → publish → rollback; brand overlay still applies only its current accent; upload theme media; compose/publish a storefront page |
| Existing brand/mail/PDF tests | keep `web/test/brand.test.mjs`, `web/test/mail-brand.test.mjs`, invoice PDF fallback tests and mail outbox snapshot coverage green |
| Visual regression | screenshots of representative screens × presets at desktop/tablet/mobile; compare to the merged restyle baseline |
| Branding assertion | verify configured name/logo/mail/PDF settings reach their existing surfaces; verify new favicon/OG fields; distinguish legal company data from removable presentation |

Add `theme-check` and the theme tests to `make verify`, next to `openapi-check` and
`client-check`.

### 14.1 What this plan deliberately does not do (v1)

* No free-form HTML block, no arbitrary JavaScript, no plugin-supplied CSS on the storefront.
  The section registry covers the legitimate layout needs; arbitrary markup is the largest
  XSS and brand-safety risk in the feature.
* No new external asset URLs for browser theme rendering. Existing `mail.brand_logo_url` is
  a separate mail-client reference and remains supported until deliberately migrated.
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
2. **Presets** → choose `Catalyst Console` or another preset → preview updates instantly;
   “Apply as draft”.
3. **Brand** → reuse the existing `/branding` name/logo/light-dark accent form; add favicon and
   OG assets when available. The mail and invoice controls remain accessible in the studio.
4. **Colors** → the contrast meter shows every pair; “Auto-fix” adjusts the two that fail.
5. **Storefront** → reorder the hero, add a “Featured products” section, hide the FAQ on
   mobile, set the hero image. The editor uses existing catalog visibility, featured and
   announcement data. Preview at 390px.
6. **Publish** → choose scopes (storefront + portal + email), confirm the diff summary.
7. **Remove Catalyst branding** → the checklist updates existing branding/mail settings and
   browser assets, while warning that statutory merchant identity is not automatically changed.
8. **Done.** No rebuild, no restart, no ticket to the vendor. A second operator with
   `theme:read` can see the history; a `theme:write` operator can draft; only
   `theme:publish` can go live.

A returning operator who wants a seasonal campaign: duplicate the published revision, change
three colours and the hero, preview, publish, and roll back after the campaign — all without
touching the base theme.

### 15.1 Definition of done

* A fresh install defaults to the agreed game-panel-matching design; existing installations
  preserve their current theme tokens and the working tenant/mail/PDF brand settings.
* An operator can change the visible presentation, upload the missing browser assets, and
  remove removable Catalyst product branding without editing files or opening a ticket; legal
  merchant identity remains explicit and separate.
* The storefront can be recomposed into at least five materially different layouts.
* Custom CSS is accepted, validated, versioned and served without weakening the CSP.
* Every preset passes the contrast gate; representative screens render at 390/834/1440 without
  overflow; `make verify` is green.
* A publish is visible within one navigation on every instance; a rollback is one click.
* The audit log answers “who published what, when, and what changed”.

---

## 16. Risks and mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| Server-rendered shell breaks the SPA boot/CSP | High | Phase 1 only; keep the static shell as a fallback; smoke + e2e assert first paint, routing, CSP |
| Custom CSS exfiltration | High | `url()` restricted to digest assets; parser-based validation; scope isolation; audit |
| Panel CSS hides staff-critical UI | High | `theme:publish` + risk acknowledgement + audit; optional policy that disallows custom CSS on `panel` scope |
| Storefront layout regression | High | Built-in `classic` fallback; server validates layout; e2e renders every block |
| Restyle and brand overlay disagree | High | Reconcile `tokens.css`, `lib/brand.ts` and existing brand tests before selecting default tokens; preserve empty-brand fallback |
| Duplicate brand sources | Medium | Existing `branding.*`, `mail.brand_*`, `invoices.pdf_template` remain canonical; theme docs exclude identity values |
| Storefront feature duplication | Medium | Layout blocks consume existing featured/category-image/stock/announcement APIs; no replacement content tables |
| Font licensing | Medium | Only open-licence families ship; uploaded fonts require an acknowledgement; no external browser font CDNs |
| Theme publish cache stampede | Low | Snapshot cache + NOTIFY + revision URLs; assets immutable |
| Marketplace supply chain | Low (future) | Signed envelopes, schema version, preview, no executable content |
| Preview token abuse | Low | HMAC, short TTL, user+revision bound, no API privilege, `noindex` |

---

## 17. Open questions

1. **Default preset.** Should a fresh install default to `Catalyst Console` (matching the game
   panel) or the existing billing token set? The plan recommends `Catalyst Console` for new
   installs, with migrations preserving installed token values. Confirm the game panel's
   intended primary: the demo stores `#ff3d7f` (rose), while its shipped preset is `#c48d5a`
   (bronze).
2. **Storefront SSR.** Per-product OG tags and real SEO need server rendering or a
   pre-render pass. Is that in scope for this project, or phase 5+?
3. **Custom HTML block.** Deliberately excluded from v1. Is there a must-have use case that
   the block registry cannot cover?
4. **Email/PDF integration.** Their identity/logo and basic presentation already exist. Should
   the first Theme Studio surface link to the current mail and invoice settings, or move those
   editors into the studio immediately through adapters?
5. **PDF fonts.** Embedding a brand font in every invoice increases file size and needs the
   font's licence to allow embedding. Is the built-in Helvetica with themed colours enough for
   v1?
6. **Reseller/multi-tenant.** Should the schema carry a nullable `tenant_id` now, even if the
   feature is later? Adding it later is a migration but not a redesign.
7. **Theme marketplace.** Is a marketplace in scope, or only import/export? The envelope is
   ready for signatures either way.

---

## Appendix A — Game panel comparison (what “better” means concretely)

| Tenant identity | Game panel URL fields | Billing name/logo/per-mode accent already reach panel, storefront, sign-in and invoice; preserve APIs |
| Social card | None | Still missing; server-generated or uploaded, theme-driven |
| Banners/backgrounds | None | Theme-only asset library + background tokens; announcements remain existing content |
| Colour editing | 3 brand + 12 surfaces; harmony generator | Full token contract; preserve `branding.accent_*` overlay and its legibility rules |
| Typography | None | Families, scale, weights, tracking, self-hosted |
| Density/shape | Radius slider | Density, radius scale, border, elevation, motion, shell variant |
| Presets | 9 hue variations | 10 structurally different designs |
| Scopes | One | 5 presentation scopes; existing identity/mail/PDF settings remain canonical |
| Layout | None | Section composer + shell variants; existing merchandising data is reused |
| Custom CSS | Regex blacklist + `<style>` | Parser + policy + first-party stylesheet + digest-restricted `url()` |
| History | None | Immutable revisions, diff, preview, rollback |
| Branding removal | Manual fields | Unified checklist over existing brand/mail/PDF config plus new browser assets |
| Email/PDF | None | Billing already has HTML mail branding and PDF template/brand; Theme Studio integrates them |
| Accessibility | No contrast gate | AA gate per preset and per custom theme |
| Security | Inline CSS approach | Strict billing CSP preserved |

## Appendix B — Branding touchpoint checklist

Symbols are authoritative; line numbers drift. Verified against the current tree.

- [x] Tenant name, uploaded PNG/JPEG logo and per-mode accent are configurable via
      `/api/v1/branding` and already reach panel chrome, storefront/sign-in and invoice; preserve
      the endpoint and its unset fallback.
- [x] Email HTML branding is configurable through `mail.brand_*`; retain the plain-text part,
      event-time HTML snapshot and existing mail-logo URL behavior.
- [x] Invoice PDF identity and presentation use the tenant brand plus
      `invoices.pdf_template`; retain the built-in byte-identical fallback.
- [x] `web/src/index.html` — make title, description, OG/Twitter, favicon, manifest,
      `theme-color` and boot mark dynamic while retaining default asset checks.
- [ ] `web/src/sw.ts` — `FALLBACK_TITLE` and branding-related cache behavior.
- [ ] `web/src/views/storefront.ts`, `storefront-product.ts` — connect existing brand and
      merchandising data to configurable section layouts.
- [ ] `web/src/lib/mfa.ts` — decide if the recovery-codes export header is in the white-label
      scope; do not alter recovery-code handling.
- [ ] `web/src/lib/icons.ts` — replace hardcoded product mark only if fully white-labelled UI
      is in scope.
- [x] `web/build.mjs` / `web/web.go` — generate favicon/manifest/OG shell values while keeping
      the current `brand/verify.mjs` contracts.
- [ ] `internal/settings/group_mail_brand.go`, `internal/notify/brand.go` and
      `internal/invoicepdf/template.go` — renderer adapters; update `docs/branding.md` because
      its mail-brand scope statement is stale relative to migration `0068`.
- [ ] `general.company_name` is legal/merchant identity; never overwrite it in a generic
      “remove branding” action.
- [ ] Product-level OG and developer-facing docs are separate scope decisions.

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
  /* shell dimensions come from the active billing layout preset */
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
| WP1 theme core | `internal/theme/**`, next-free migration after `0072` | backend |
| WP1 shell/CSP | `web/web.go`, `web/build.mjs`, `web/src/index.html` | frontend + security |
| WP1 runtime | `web/src/lib/theme.ts`, `web/src/styles/tokens.css` | frontend |
| WP2 studio API | `internal/httpapi/theme.go`, `internal/theme/store/**` | backend |
| WP2 studio UI | `web/src/views/theme-*.ts`, `web/src/ui/theme-*.ts` | frontend + design |
| WP2 presets | `internal/theme/presets/*.json`, `assets/fonts/**` | design + frontend |
| WP3 layout | `internal/theme/layout/**`, `web/src/lib/{layout,blocks}.ts` | frontend |
| Existing brand adapters | `internal/brand/**`, `internal/notify/brand.go`, `internal/invoicepdf/template.go` | backend |
| WP5 hardening | tests, `Makefile`, docs | security |

## Appendix E — Phase 1 implementation file checklist

A concrete, reviewable starting point for the new theme registry. It does not repeat the
already-built brand/mail/PDF work.

**Go core**

- [x] `internal/theme/document.go` — `Document`, `Color`, `Typography`, `Space`, `Shape`,
      `Motion`, `Background`, `Layout`, `Components` (no duplicated tenant `Brand`).
- [x] `internal/theme/validate.go` — closed validation for every field (§4.1).
- [x] `internal/theme/merge.go` — deep merge + `inherit: false`.
- [x] `internal/theme/canonical.go` — sorted-key JSON + SHA-256 digest.
- [x] `internal/theme/css/generate.go` — canonical vars, legacy aliases, game-panel HSL
      aliases, preset structural rules.
- [ ] `internal/theme/css/sanitize.go` — parser-based custom-CSS validation.
- [x] `internal/theme/presets/embed.go` + `internal/theme/presets/catalyst-billing.json`.
- [x] `internal/theme/store/store.go` — themes, revisions, assignments, assets, publications.
- [x] `internal/theme/cache/cache.go` — atomic snapshot + `LISTEN/NOTIFY`.
- [ ] `internal/theme/assets/upload.go` — theme-only fonts/backgrounds/favicon/OG; existing
      tenant/catalog images keep using `product_images` and `/api/v1/media/{id}`.
- [x] `internal/httpapi/theme.go` — public + staff routes (§6).
- [x] `internal/store/migrations/<next-free>_themes.sql` — §5 DDL and permission grants;
      check the live migration head at implementation time.
- [x] `internal/authn/authz.go` — `PermThemeRead`, `PermThemeWrite`, `PermThemePublish`;
      update `AllPermissions` and the role test.

**Web shell**

- [x] `web/web.go` — `ThemeProvider` interface, server-rendered shell, theme routes, preview
      header branch, cache of rendered shells.
- [x] `web/src/index.html` — replace hardcoded brand/meta with `{{THEME_*}}` placeholders;
      keep `{{STYLE}}`/`{{SCRIPT}}`.
- [x] `web/build.mjs` — leave theme placeholders intact; keep the default manifest/icons for
      the no-theme case.
- [x] `web/src/lib/theme.ts` — add theme document boot/runtime while preserving the current
      `localStorage` light/dark preference and `lib/brand.ts` overlay.
- [ ] `web/src/styles/tokens.css` — generate defaults only after reconciling the merged restyle
      with brand overlay tests; keep compatibility aliases.
- [ ] `web/src/ui/layout.ts`, `app.ts`, auth views, storefront views, `icons.ts`, `sw.ts`,
      `mfa.ts` — replace hardcoded brand strings and the `#F97301` mark fill.
- [x] `web/src/views/theme-*.ts` — route stubs for the studio (Phase 1 fills them).

**Tests**

- [ ] Extend `web/web_test.go`, `web/test/brand.test.mjs`, `web/test/mail-brand.test.mjs`,
      invoice PDF tests and `scripts/e2e-ui.mjs`; cover theme render/publish without regressing
      the existing branding fallbacks.

---

*This document is the design contract. When implementation departs from it, record the
departure and the reason here, the way `docs/ui-gate.md` does — the reasoning is the
expensive part.*

---

## Appendix F — Implementation status (theme registry, phases 1–2)

This appendix is the departure log the note above asks for: what landed, what differs from this
document and why, and what is deliberately still open. It is written to be read by whoever picks up
phase 3.

### F.1 What landed

**Go core**

| File | What it is |
| --- | --- |
| `internal/theme/document.go` | The schema v2 document (§4.1). Every field optional, because omission *is* the inheritance mechanism. |
| `internal/theme/defaults.go` | The built-in default document. Deliberately a reproduction of the shipped `tokens.css`, which is the whole upgrade story (§F.3.6). |
| `internal/theme/validate.go` | Closed validation: grammars for colours, lengths, radii, font sizes, font stacks, shadows, durations, easings, gradients; closed enums for every mode/density/nav/card/table/background/component name; the layer-vs-resolved distinction. |
| `internal/theme/customcss.go` | The custom-CSS allowlist scanner (§F.3.1). |
| `internal/theme/merge.go` | Layer resolution: `built-in ← parent scope ← source`, plus the two rules that make the accent follow the brand (§F.3.12). |
| `internal/theme/canonical.go` | Canonical JSON and the `sha256:` digest. |
| `internal/theme/diff.go` | The structured `{path, from, to}` diff the history screen renders. |
| `internal/theme/css/generate.go` | The three token vocabularies, per-mode blocks, shell-shape rules, the boot script, and the operator's CSS last. |
| `internal/theme/presets/*.json` | Ten presets (§8) plus `presets.go`, validated at load. |
| `internal/theme/store/store.go` | Themes, immutable revisions, assignments, publications, assets. |
| `internal/theme/cache/cache.go` | Per-scope snapshot, atomic replacement, explicit invalidation, TTL fallback (§F.3.2). |
| `internal/httpapi/theme.go` | The public resources and the staff surface (§6). |
| `internal/store/migrations/0073_themes.sql` | The DDL, the partial unique indexes that make "the published revision" a fact, and the three permission grants. |
| `internal/authn/authz.go` | `theme:read`, `theme:write`, `theme:publish` and their role sets. |
| `internal/openapi/descriptions_theme.go` | The API contract, regenerated into `api/openapi.yaml` and `api/client/client.go`. |

**Web shell**

`web/web.go` renders the shell per request from a `ThemeProvider` and caches the rendered bytes per
revision; `web/src/index.html` carries `{{THEME_*}}` placeholders; `web/build.mjs` refuses to build if a
placeholder is dropped; `web/src/lib/theme.ts` applies the manifest at runtime on top of the preserved
`catalyst.theme` preference; `web/src/views/theme-*.ts` are the seven studio screens;
`web/src/styles/view-theme.css` is the studio's own styles.

**Verification**

- `go test ./...` and `go test -race ./...` pass. The theme packages add 250+ cases: the document
  grammars, a 40-case custom-CSS security corpus, the merge properties, the digest, the store's
  database invariants, the cache's fallbacks, and the HTTP surface's permission and publish flow.
- `web/test/theme-studio.test.mjs` covers the studio's routes, endpoints, contrast helper and the
  preserved `lib/theme.ts` contract; the whole frontend suite is 118 tests.
- `make e2e-theme` drives the system in a real Chromium: it publishes a theme, then reads what the
  browser *computed* — the painted background, the generated radius, the border width, the nav
  variant, whether the sidebar overflows its own column — and fails on any console error. It passes for
  all ten presets and rolls back what it published.
- `make e2e-ui` (the existing authenticated browser test) passes.

### F.2 Two bugs a browser found and no test would have

Recorded because they are the argument for the browser gate in §14, and because both were invisible to
a passing Go suite.

1. **The storefront wore the panel's theme.** `/` is the storefront for everyone — including a signed-in
   operator, whose dashboard is `/dashboard`. The scope resolver originally returned the panel scope
   for a signed-in user on `/`, on the plausible reasoning that a signed-in person is "working". The
   shop was then served the panel's stylesheet. No unit test noticed; a screenshot did.
2. **The manifest was cached for 60 seconds, so a publish looked like it did nothing.** The client
   applies the manifest *after* the shell has painted, so a browser serving it from its own cache
   re-applied the previous theme: the stylesheet link was the new revision and the nav variant was the
   old one, on the same page. The manifest is now `no-cache` (still cached; revalidated by ETag).

A third was a CSS bug in the same family: the `rail` nav variant widened the sidebar to 64px but left
the wordmark, the account name and every nav label visible, so they overflowed their own column. The
generated stylesheet now hides them and the browser test measures the overflow.

### F.3 Departures from this document

**F.3.1 Custom CSS: an allowlist scanner, not `tdewolff/parse`.** §5.2 recommends the parser library.
This build keeps the module list at three and implements the allowlist directly: CSS escapes are
refused outright (which removes the `@\69mport` class of bypass rather than enumerating it), comments
are stripped before inspection, only nine at-rules are permitted, every `url()` must point at a
same-origin digest asset or a base64 *raster* data URI, braces are balanced and nesting is bounded. The
behaviour a parser would have to preserve is pinned by the corpus in `customcss_test.go`; a swap is a
refactor against a green suite, not a redesign.

**F.3.2 The cache has no `LISTEN/NOTIFY`.** §12 asks for it. What landed is explicit invalidation on
publish (immediate on the instance that published) plus a 30-second TTL (bounded everywhere else). That
is the smaller thing to get right first, and the TTL is what keeps the gap bounded when a notification
would have been dropped. Adding the listener is additive and does not change the interface.

**F.3.3 No `internal/theme/assets` package.** Uploads live in `store.PutAsset`/`store.Asset` and
`httpapi.handleThemeAssetUpload`, because the validation is HTTP-shaped (a content type, a size cap, a
decoder) and the storage is one table. The tenant logo and catalog images are untouched: they stay in
`product_images` and `/api/v1/media/{id}`.

**F.3.4 SVG uploads are refused, not sanitised.** §11 asks for a parsed SVG with `<script>`,
`<foreignObject>`, event attributes and external `href` removed. A half-written sanitiser for a format
whose purpose is to be a document is worse than a refusal, and a favicon can be a PNG. The serving
route still applies `default-src 'none'; sandbox` to an SVG row, so a row written by hand cannot become
an XSS primitive. Adding a sanitiser later is a new accepted content type, not a change to the route.

**F.3.5 The shell is substituted, not templated.** §10 does not specify; this build substitutes
`{{THEME_*}}` per context rather than running the whole document through `html/template`. The shell is a
build artefact whose `{{STYLE}}`/`{{SCRIPT}}` are already resolved by `web/build.mjs`, and running it
through a contextual escaper would need every existing byte to be template-safe. Every value is escaped
for the context it lands in, and the one raw slot (the inline boot mark) is markup this build controls.

**F.3.6 The default document is today's warm palette, not `Catalyst Console`.** §8 recommends Console
as the default for *new* installations and preserving installed tokens on upgrade. Those conflict here,
because there is no row to migrate: the default document *is* the fallback. Making it Console would
silently restyle every existing installation on upgrade, which the same section forbids. So the default
is a byte-for-byte reproduction of the shipped `tokens.css`, Console ships as a preset one click away,
and `TestDefaultReproducesTheShippedTokens` is what keeps the two in step. Changing the default is a
change to `defaults.go` and to this paragraph.

**F.3.7 One theme per scope, not one per slug.** The obvious design keys `themes` on `(scope, slug)`, so
applying a preset makes a new theme. It is wrong: with two theme rows in one scope, "the open draft"
stops being a single thing — applying Brutalist and then Warm SaaS left two drafts open and "Save" had
no single target. A scope serves one presentation; applying a preset is a new revision of that
presentation. The row is keyed on the scope and `slug`/`name` follow the current revision. A store test
pins it.

**F.3.8 `published_by` is nullable.** A publish can be initiated by the system (a CLI, a migration, a
seed). Inventing a user row to satisfy a NOT NULL would put a fictional person in the history; the
authoritative actor is the audit row. `published_at` *is* required, because "when did this go live" has
no system-initiated exception.

**F.3.9 The theme resources live under `/theme/r/{revision}/`.** §6 sketches
`/theme/{revision}/theme.css`. Go's `ServeMux` refuses a wildcard segment beside a literal one in the
same position (`/theme/assets/{id}`), so the revision routes are nested under `/r/`. The shapes and the
caching are otherwise as specified.

**F.3.10 `frame-src 'self'` in the CSP.** §11 asks that the preview be framable by its own origin only.
`frame-ancestors` on the preview response is one half; `frame-src` on the *panel* is the other, and
without it the browser refuses to load the iframe at all. The widening is one directive, and every
other response keeps `frame-ancestors 'none'`.

**F.3.11 Elevation, the accent family and the signal colours are per colour mode.** §7.1 lists them
once. A shadow and an accent are not tints: a 28%-black drop shadow that reads on `#171413` is a bruise
on `#f5f2ee`, and `#ff951e` disappears on paper. The schema therefore carries `elevation.dark`/
`elevation.light` and per-mode `accent`/`accentHover`/`accentPress`/`accentInk` and signal colours. A
preset that states only `color.brand.primary` gets them derived, so the extra fields cost an author
nothing.

**F.3.12 The accent follows the brand, and `secondary` does not.** Two rules in `merge.go` that are not
plain merges. A layer that names a brand accent (or a primary with no accent) drops the accent family it
would have inherited, per mode — otherwise a preset that says "my colour is rose" would inherit the
default's orange in both modes, and every preset author would have to remember four values per mode. And
`color.brand.secondary` is *mode-independent*, which is a trap: a value chosen for a dark ground is also
used on paper, so a preset that wants a mode-correct companion leaves it absent and lets the generator
derive it from the mode's own border.

**F.3.13 The built-in default is `system`, not `dark`.** `tokens.css` puts the dark palette under
`:root`, so with no attribute the product paints dark — that is the CSS fallback. But what a *first
visit* does has always been "follow the OS" (`lib/theme.ts` reads `prefers-color-scheme` when nothing is
stored), and a theme default of `dark` silently overrode it. The existing browser test caught the
regression, in the branding block, as a preview painting the dark accent.

**F.3.14 The preview token's key is per process.** There is no installation-wide signing secret in this
deployment (sessions are opaque rows, not signed cookies), and a new environment variable whose absence
silently breaks a feature is worse than the limitation. The cost is that a preview opened through a load
balancer may reach an instance that did not mint the token and be refused — a refusal, not a bypass.
Sticky sessions or a shared secret are a deployment decision.

### F.4 Deliberately still open

- **Phase 3, storefront composition (§9).** `layout.pages` is reserved in the schema and a non-empty
  value is *refused* with an error naming the missing feature, rather than accepted and ignored. The
  block registry, renderer and composer editor are not built.
- **Full white-label asset swap (§13, Phase 4).** The shell's icons, the manifest and the social card
  are themed by URL but the default assets are still the product's own. The favicon and OG routes
  redirect to the built-in files; an uploaded asset is reachable and referenceable from a document but
  the shell does not yet pick it up.
- **`web/src/sw.ts`, `lib/mfa.ts`, `lib/icons.ts`** still carry the product's own strings and mark.
  They are on the phase-4 list and are not touched here.
- **`tokens.css` is unchanged.** It stays as the no-JavaScript fallback and as the byte-for-byte
  reference the golden test compares against. Generating it is a phase-4 step that requires the
  reconciliation §Appendix E describes.
- **`general.company_name`** remains legal/merchant identity and is never written by the theme, as
  §Appendix B requires.
