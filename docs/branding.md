# Branding

A tenant's brand: a display name, a logo and one accent colour per colour mode. It reaches the
panel chrome and the invoice PDF, and it falls back to the built-in appearance byte-identically
when it is unset.

**Status:** built (E6.10 / INV-15, the invoice half).
**Scope boundary:** this is a brand overlay, **not** a theme registry. See §5.

---

## 1. What exists

| Piece | Where |
|---|---|
| The brand value type, colour parsing, WCAG contrast, the readable-relative derivation | `internal/brand/brand.go` |
| Storage: the `branding` settings group (`name`, `logo_id`, `accent_light`, `accent_dark`) | `internal/settings/settings.go`, migration `0060_branding.sql` |
| HTTP: public read, settings read/write, logo upload/delete | `internal/httpapi/branding.go` |
| The invoice: a colour band, a branded letterhead rule, a darkened title, the logo | `internal/invoicepdf/brand.go`, `internal/invoicepdf/document.go` |
| The panel: the accent override, the chrome decoration, the colour arithmetic mirror | `web/src/lib/brand.ts`, `web/src/app.ts` |
| The screen: form, live preview, legibility report, logo upload | `web/src/views/branding.ts`, `web/src/styles/view-branding.css` |

## 2. Why two accents

The panel's built-in accent is `#3b5bdb` on white and `#7b95ff` on near-black: the same intent at
two luminances, because a mid-tone blue that reads on white is too dark on `#0a0d13` and the pale
version of it is invisible on white. One stored accent would therefore be legible in one theme and
washed out in the other. So there is one per mode, and **either may be empty**.

The invoice prints on white paper, so it uses the light accent (falling back to the dark one when
only that is configured).

## 3. Why a brand cannot break light or dark

Two rules, and both are enforced by tests rather than by convention:

1. **A brand writes only the accent family.** `BRAND_PROPERTIES` in `web/src/lib/brand.ts` is
   `--accent`, `--accent-ink`, `--accent-text`, `--accent-soft`, `--accent-soft-strong`. It cannot
   write `--bg`, `--surface`, `--text` or any other token, so it cannot make a surface and its text
   the same colour — the failure mode that would only show up in one mode.
2. **A brand writes only the current mode's accent.** `applyBrand(brand, mode)` clears the family
   and then sets it only if that mode has an accent. A brand with only a light accent writes
   nothing while the dark theme is active, and the stylesheet's own tokens stand.

`web/test/brand.test.mjs` asserts both, and `scripts/e2e-ui.mjs` asserts them again in a real
browser: it saves two distinct accents, toggles the theme, and checks the accent followed the
theme.

## 4. Why unset is byte-identical, and how that is proven

An unset brand is the empty string for every key, and **empty means "not configured"** — never
"the default value". That distinction is the whole design:

* In the browser, `paintFor` returns `null` and `applyBrand` writes no custom property at all.
  There is no inline style, so the rendering is the stylesheet's.
* In the PDF, every brand drawing call is conditional. A zero `Brand` emits no colour operator, no
  image object and no extra resource, so the bytes are unchanged.

Both are pinned by tests rather than asserted in prose:

* `TestUnsetBrandIsByteIdentical` (`internal/invoicepdf/document_test.go`) hashes the whole PDF for
  an invoice and a credit note and compares it against a recorded value, then additionally asserts
  that no `rg`/`RG` colour operator, no `/XObject` and no `Do` appears.
* `test/brand.test.mjs` asserts `applyBrand(NO_BRAND, …)` returns `[]` and writes nothing, and that
  `isBrandEmpty` is true for the empty brand.

> **Note on the recorded hashes.** The first recording was taken from the renderer as it stood
> before branding existed. Rendering branded and unbranded documents side by side then showed they
> disagreed for a reason unrelated to branding: `canvas.fillRect` set the fill colour and never
> restored it, so every glyph after the table header — line items, totals, terms — was painted at
> 92% grey. On screen that reads as "a bit faint"; printed, the amount a customer is asked to pay
> is nearly invisible. That is a defect in the *unbranded* document, so it was fixed
> (`fillRect` now wraps the fill in `q`/`Q`) and the hashes re-recorded.
> `TestFillColourDoesNotLeakIntoText` pins the fix.

## 5. The boundary: brand overlay, not theme registry

`docs/theming-system.md` designs a much larger system: versioned theme documents, presets,
per-scope inheritance (panel / portal / storefront / email / invoice), a full token contract
(typography, density, elevation, motion, layout composition), uploaded fonts, validated custom CSS
served as a first-party stylesheet, and revision history with publish/rollback.

**None of that is built, and none of it is this.** What is built is the part a tenant needs in
order to look like themselves: their name, their logo and their colour, in both modes, on the
panel and on their invoices. A registry is a different feature with its own schema (every token
becomes a row or a column), its own migration story (a new token is a migration per theme) and its
own failure mode (a theme missing a token renders an unstyled control).

Concretely, the following are deliberately **not** implemented:

* no `themes` table, no theme documents, no revisions;
* no presets, no per-user theme selection, no preview-and-publish;
* no custom CSS (the CSP is `style-src 'self'` with no `'unsafe-inline'`, and that is a feature);
* no per-scope themes: one brand, for the whole installation;
* no typography, spacing, radius or layout tokens;
* no branding of outgoing **email** (it still substitutes `{{company_name}}` from
  `general.company_name`, which is a separate setting);
* no favicon, social card or OG image.

## 6. The logo

Uploaded, not linked. A URL field would be an SSRF primitive and a privacy leak (the panel fetching
a third-party origin on every render), so the bytes are stored through the catalog's image store
(deduped by SHA-256, capped at 256 KiB) and served from `/api/v1/media/{id}` with an immutable
cache and `X-Content-Type-Options: nosniff`.

**PNG and JPEG only.** The catalog also accepts WebP and SVG for product logos, and both are wrong
here: the invoice renderer can draw neither, so a WebP logo would render in the panel and vanish
from every document, and an SVG is a document that can carry a script. A brand that is half-applied
is worse than one refused with a reason.

A reference to an image that no longer exists reads as "no logo" — the panel shows its built-in
mark, the invoice prints the merchant name alone, and the brand's name and colour still apply. One
broken field is not a broken brand.

## 7. Legibility

A pale accent is a legitimate brand colour and an illegible link. The server therefore reports, per
mode, the raw accent's WCAG contrast against that mode's background, whether it reaches 4.5:1, and
`--accent-text`: the accent adjusted towards white or black until it reads, with the hue preserved.

* The panel uses `--accent-text` for links and small accent-coloured text; `--accent` stays exactly
  the colour the tenant picked, so buttons and the focus ring are unaffected.
* The invoice darkens the accent for the document *title* and keeps the stored colour for the band
  and the rule, because a shape needs no contrast.
* The screen shows the report and offers no refusal.

The arithmetic exists twice — `brand.Readable` in Go and `readableHex` in TypeScript — because the
panel needs it synchronously for the pre-paint path and cannot call Go. Both are pinned against the
same cases.

## 8. Tests

| What | Where |
|---|---|
| Colour parsing, contrast, the derivation, the zero brand | `internal/brand/brand_test.go` |
| Unset brand byte-identical; brand reaches the document; pale accent darkened; corrupt/SVG logo ignored | `internal/invoicepdf/document_test.go` |
| The fill-colour leak | `internal/invoicepdf/document_test.go` |
| Group declared, colours/uuid/name validated, schema constraints and normalisation | `internal/settings/settings_test.go` |
| Public read, round-trip, normalisation, validation as a whole, permission, logo upload/serve/clear, refusals, branded invoice PDF, dangling reference | `internal/httpapi/branding_test.go` |
| Paint rules, theme independence, unset writes nothing, derivation mirror | `web/test/brand.test.mjs` |
| Branded panel renders; accent follows the theme; reset removes the property; wordmark | `scripts/e2e-ui.mjs` |
