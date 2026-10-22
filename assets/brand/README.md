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
| Palette | **Measured off a rendered screenshot of the site**, not read from its stylesheet — the two differ, because `--edge` is used at partial opacity, so the border that actually draws is `#1E1D19` rather than `#3B3830` |
| Accent | `#C48D5A` (tan). `#F97301` is **logo-only** — see the rule below |
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

Two gates, because either one alone is a false green.

```bash
make brand-audit    # offline: colour + composition, straight off the PNGs
make brand-verify   # both: the audit, then a real browser against a served panel
```

`scripts/brand/audit.py` asserts, per asset: orange never below the lockup band and never in
the card column; the ground is flat across ≥98% of the top and bottom edges; and the tan
accent is actually present. It classifies assets by kind, because a transparent glyph has no
ground and no accent by design.

`scripts/brand/regress.py` then **proves the audit works**: it takes a known-good asset,
reintroduces each mistake above (an orange button, an orange wash, the accent removed) and
asserts the audit rejects it. An audit nobody has seen fail is a green light with no wiring.

This drives a real headless Chromium against a real server and asserts what only a browser
can: every brand asset responds 2xx, the favicon **decodes as an image**, the manifest names
the 192 and 512 icons, `og:image` is 1200x630, `mark.svg` still carries the traced path in
`#F97301`, the shell carries an inline mark for first paint, and the module boots without an
uncaught exception. A valid SVG that a browser refuses is still a broken asset, and reading
the file cannot prove otherwise.

## The measured language

Every value here was taken off the site, because the first draft of these assets used the
correct logo and the correct tokens and still looked like a different product. What was
wrong, and what the site actually does:

| First draft | The site |
|---|---|
| `#F97301` as the accent — eyebrow squares, CTA, accent words, rules | Orange appears **nowhere but inside the mark**. The accent is tan `#C48D5A` |
| An orange radial wash on every ground | The ground is **flat** `#161412`, edge to edge |
| 8px radii, coloured top edges, segmented bars | **4px** radii, hairline dividers, thin meters on a track |
| Three separate cards with coloured borders | **One** bordered box divided by **vertical hairlines** |
| An all-white headline | One **tan phrase** anchors every section heading |
| No CTA on the cards | A tan primary button, 34px tall, near-black label, arrow after |
| Free-form composition | Eyebrow → headline → body → mono meta → panel, in that order |

## Rules

1. **The mark is the parent's.** Same path, same orange, never redrawn or recoloured. A
   sub-brand that restyles its parent's logo is claiming to be a different company.
2. **Orange is logo-only.** `#F97301` may appear inside the mark and in the `BILLING`
   product word of a lockup — nowhere else. Not a button, not a rule, not an eyebrow, not an
   accent word, not a bar. This is the single rule that decides whether an asset looks like
   Catalyst or like a generic dark poster.
3. **One accent: the tan.** `#C48D5A` for eyebrows, CTAs, accent phrases and emphasis.
   Cobalt, green, amber and red are only ever the semantic hues — settled, due, failed,
   informational — never decoration.
4. **The ground is flat.** No gradient, no wash, no glow. Depth comes from the `#1C1A17`
   card surface and a `#1E1D19` hairline, nothing else.
5. **`Billing` is a word, not a colour.** Mono caps, wide tracking, beside or beneath the
   wordmark. Two-colour wordmarks read as two products.
6. **Figures are mono and right-aligned.** Money in a proportional face does not line up,
   which is the one thing a billing brand cannot get wrong.
