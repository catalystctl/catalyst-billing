# Catalyst Billing brand assets

Catalyst Billing is part of the Catalyst ecosystem, so these assets reuse the parent's
mark, palette and typefaces rather than inventing a second identity. What makes Billing
distinct is **composition and subject matter** — a ledger surface, money in tabular
figures, status dots — not a different colour.

Everything here is generated. Do not hand-edit an SVG; edit `scripts/brand/build.py`.

## Provenance

| Element | Source |
|---|---|
| Mark | Traced from `https://catalystctl.com/logo-512.png`, stored as path data in `scripts/brand/mark.path` |
| Orange | `#F97301`, sampled from that logo |
| Palette | The site's own tokens: `--ink 30 10% 6%`, `--background 30 10% 8%`, `--panel 36 10% 10%`, `--panel-2 34 10% 13%`, `--edge 44 10% 21%`, `--chalk 40 12% 93%`, `--dust 38 8% 62%`, `--signal 29 47% 56%`, plus `--success 152 67% 42%`, `--warning 38 82% 48%`, `--danger 0 67% 55%`, `--info 217 67% 55%` |
| Display type | **Oxanium** — identified as the site's `font-display` by glyph-shape matching against its own OG image (distance 0.156, next best 0.380) |
| Body type | **DM Sans** |
| Labels and figures | **JetBrains Mono** |

Every text run is **outlined to path data**, so no asset depends on a font being installed
where it is opened. That is why the SVGs are tens of kilobytes: they are drawings, not
text with a `font-family` that will fall back to DejaVu on someone else's machine.

## What ships

```
logo-mark.svg                     the mark alone, transparent ground
logo-mark-mono-black.svg          one-colour, for print and single-colour embeds
logo-mark-mono-white.svg          one-colour, for dark photography
logo-badge.svg                    the mark on the site ground, in a squircle (app icon)
logo-lockup-horizontal-{dark,light}.svg   mark + "Catalyst" + BILLING, one baseline
logo-lockup-stacked-{dark,light}.svg      mark over the wordmark, for square slots
banner-readme.svg                 1280x320 — the README hero
banner-github.svg                 1280x640 — GitHub social preview
og-default.svg                    1200x630 — the link card
og-architecture.svg               1200x630 — the technical card, for docs and posts
png/                              rendered exports (see render.sh)
```

`logo-badge.svg`, `favicon-32.png`, `apple-touch-icon.png`, `icon-192.png`, `icon-512.png`
and `og.png` are **copied into `web/src/`** and served by the panel, so the tab icon and the
social card cannot drift from the brand. `web/build.mjs` copies them into `dist/` and emits
`site.webmanifest`.

## Rebuilding

The generator needs Python with `fontTools` and `brotli`, and three woff2 files. It is not
part of `make verify`: it runs when the brand changes, not on every commit.

```bash
# once
python3 -m venv ~/.cache/catbrand/venv
~/.cache/catbrand/venv/bin/pip install fonttools brotli
mkdir -p ~/.cache/catbrand/fonts && cd ~/.cache/catbrand/fonts
UA='Mozilla/5.0'; for f in "Oxanium:wght@600;700" "DM+Sans:wght@400;500;600;700" "JetBrains+Mono:wght@400;500;700"; do
  curl -sS -A "$UA" "https://fonts.googleapis.com/css2?family=$f&display=swap"
done   # then download each latin woff2 as {Oxanium,DMSans,JetBrainsMono}-latin.woff2

# every time the brand changes
~/.cache/catbrand/venv/bin/python scripts/brand/build.py   # SVG sources
./scripts/brand/render.sh                                  # PNG exports
cd web && npm run build                                    # copy icons into the app
```

`BRAND_FONT_DIR` overrides the font location. `render.sh` takes group names:
`mark`, `logo`, `icons`, `banner`, `og`.

## Verifying

```bash
make dev                                    # or any running panel
node scripts/brand/verify.mjs               # BASE=http://127.0.0.1:8791 by default
```

This drives a real headless Chromium against a real server and asserts what only a browser
can: every brand asset responds 2xx, the favicon **decodes as an image**, the manifest names
the 192 and 512 icons, `og:image` is 1200x630, `mark.svg` still carries the traced path in
`#F97301`, the shell carries an inline mark for first paint, and the module boots without an
uncaught exception. A valid SVG that a browser refuses is still a broken asset, and reading
the file cannot prove otherwise.

## Rules

1. **The mark is the parent's.** Same path, same orange, never redrawn or recoloured. A
   sub-brand that restyles its parent's logo is claiming to be a different company.
2. **`Billing` is a word, not a colour.** It is set in mono caps beside or beneath the
   wordmark. Two-colour wordmarks read as two products.
3. **One accent.** Orange carries the brand. Cobalt, green, amber and red are only ever the
   semantic hues — settled, due, failed, informational — never decoration.
4. **No gradients on the logo.** The page wash is a soft radial on the *ground*; the mark
   itself is flat.
5. **Figures are mono and right-aligned.** Money in a proportional face does not line up,
   which is the one thing a billing brand cannot get wrong.
