#!/usr/bin/env python3
"""Build the Catalyst Billing brand assets.

The product is part of the Catalyst ecosystem, so this generator does not invent a
look: it reuses the parent's mark, the parent's palette and the parent's typefaces,
read from catalystctl.com. What makes Billing a separate product is composition and
subject matter — a ledger surface, tabular money, status dots — not a second colour.

Provenance of every value used here:
  mark      traced from https://catalystctl.com/logo-512.png (see mark.path)
  palette   the site's own design tokens (--ink/--background/--panel/--edge/--chalk/
            --dust/--signal plus its semantic hues), converted from HSL to hex
  type      Oxanium (display), DM Sans (body), JetBrains Mono (labels and figures) —
            the three families the site itself loads
  orange    #F97301, sampled from the logo

Every text run is outlined to path data, so no asset depends on a font being installed
where it is opened. Requires fontTools (`pip install fonttools brotli`) and the woff2
files named in FONT_DIR; see assets/brand/README.md for the two-line download.

    ./scripts/brand/build.py      # writes assets/brand/*.svg
    ./scripts/brand/render.sh     # rasterises them to PNG with Chromium
"""

from __future__ import annotations

import math
import os
import sys
from pathlib import Path

from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen
from fontTools.ttLib import TTFont
from fontTools.varLib.instancer import instantiateVariableFont

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
OUT = ROOT / "assets" / "brand"
FONT_DIR = Path(os.environ.get("BRAND_FONT_DIR", Path.home() / ".cache/catbrand/fonts"))

# --------------------------------------------------------------------- tokens --
# Every value below was measured from the rendered site (a headless screenshot of
# catalystctl.com, then colour-clustered), not inferred from its stylesheet. The two do
# differ: the stylesheet says --edge is 44 10% 21% (#3B3830), but the border that actually
# draws on the page is #1E1D19, because --edge is used at partial opacity. Measurement wins.
#
# THE RULE THIS FILE EXISTS TO ENFORCE:
#   #F97301 (saturated orange) is LOGO-ONLY. It appears nowhere else on the site — not as
#   an accent word, not as an eyebrow square, not as a button, not as a rule. The accent is
#   the muted tan SIGNAL below. An asset that paints orange anywhere but inside the mark is
#   off-brand, and that is what the first draft of these banners got wrong.

INK = "#110F0E"        # the deepest ground
BG = "#161412"         # --background  30 10% 8%   — page ground, FLAT (no wash)
PANEL = "#1C1A17"      # --panel       36 10% 10%  — card surface
PANEL_2 = "#23211C"    # raised surface / hover
EDGE = "#1E1D19"       # the hairline that actually draws (measured)
EDGE_STRONG = "#312E28"  # a border that should read as a border
EDGE_TRACK = "#3B3830"   # progress-bar tracks, --edge at full strength
CHALK = "#F3F2F2"      # headings — measured; near-neutral, not warm
MUTED = "#B6B3AF"      # body copy — --muted-foreground 37 5% 70%
DUST = "#A6A096"       # labels — --dust 38 8% 62%
FAINT = "#6D6A67"      # mono meta lines, the dimmest legible text
SIGNAL = "#C48D5A"     # --signal 29 47% 56% — THE accent: eyebrows, CTAs, accent words
SUCCESS = "#23B370"    # --success 152 67% 42%
WARNING = "#B77B16"    # --warning, as it renders inside a badge (dimmed)
DANGER = "#D93F3F"     # --danger 0 67% 55%
INFO = "#3F7AD9"       # --info 217 67% 55%

ORANGE = "#F97301"     # the mark ONLY. See the rule above.

RADIUS = 4             # the site's --deck-radius / --radius: 4px, not 8

SANS, MONO, DISPLAY = "DMSans", "JetBrainsMono", "Oxanium"


# ----------------------------------------------------------------------- type --

_fonts: dict[tuple[str, int], TTFont] = {}


def font(family: str, weight: int) -> TTFont:
    key = (family, weight)
    if key in _fonts:
        return _fonts[key]
    path = FONT_DIR / f"{family}-latin.woff2"
    if not path.exists():
        sys.exit(
            f"missing {path}\n"
            f"Download the latin subset of Oxanium, DM Sans and JetBrains Mono into\n"
            f"{FONT_DIR} (see assets/brand/README.md), or set BRAND_FONT_DIR."
        )
    f = TTFont(path)
    if "fvar" in f:
        f = instantiateVariableFont(f, {"wght": weight}, updateFontNames=False)
    _fonts[key] = f
    return f


def text_width(s: str, size: float, family: str = SANS, weight: int = 400, tracking: float = 0) -> float:
    f = font(family, weight)
    upm = f["head"].unitsPerEm
    cmap, hmtx = f.getBestCmap(), f["hmtx"]
    # A glyph the subset lacks contributes nothing — in measuring and in drawing — so a
    # stray character cannot shift a right-aligned money column by a phantom advance.
    adv = sum(hmtx[cmap[ord(c)]][0] for c in s if ord(c) in cmap)
    adv += tracking * upm * max(0, len(s) - 1)
    return adv * size / upm


def text(
    s: str,
    x: float,
    y: float,
    size: float,
    *,
    family: str = SANS,
    weight: int = 400,
    color: str = CHALK,
    tracking: float = 0.0,
    anchor: str = "start",
    opacity: float | None = None,
) -> str:
    """One text run as outlined path data, baseline at (x, y).

    anchor is start | middle | end, measured on the advance width including tracking —
    which is what makes right-aligned columns actually line up."""
    f = font(family, weight)
    upm = f["head"].unitsPerEm
    scale = size / upm
    cmap, hmtx = f.getBestCmap(), f["hmtx"]
    glyphs = f.getGlyphSet()

    if anchor != "start":
        w = text_width(s, size, family, weight, tracking)
        x -= w if anchor == "end" else w / 2

    parts: list[str] = []
    pen_x = 0.0
    for ch in s:
        gname = cmap.get(ord(ch))
        if gname is None:
            continue
        pen = SVGPathPen(glyphs, ntos=lambda v: f"{v:.2f}")
        glyphs[gname].draw(TransformPen(pen, (scale, 0, 0, -scale, x + pen_x * scale, y)))
        d = pen.getCommands()
        if d:
            parts.append(d)
        pen_x += hmtx[gname][0] + tracking * upm

    attrs = f'fill="{color}"'
    if opacity is not None:
        attrs += f' fill-opacity="{opacity}"'
    return f'<path {attrs} d="{" ".join(parts)}"/>'


# ----------------------------------------------------------------------- mark --

MARK_PATH = (HERE / "mark.path").read_text().strip()

# The traced artwork does not fill its 512 box: it is inset, so every placement is
# computed from the artwork's own bounding box rather than the file's viewBox.
ART_X0, ART_Y0, ART_X1, ART_Y1 = 63.0, 45.0, 437.0, 473.0
ART_W, ART_H = ART_X1 - ART_X0, ART_Y1 - ART_Y0


def mark(cx: float, cy: float, height: float, color: str = ORANGE) -> str:
    """The parent's mark, centred on (cx, cy) at the given rendered height.

    Same path, same orange, unscaled relative to itself — a sub-brand that redrew its
    parent's logo would be claiming to be a different company."""
    s = height / ART_H
    tx = cx - (ART_X0 + ART_W / 2) * s
    ty = cy - (ART_Y0 + ART_H / 2) * s
    return (
        f'<g transform="translate({tx:.2f} {ty:.2f}) scale({s:.5f})">'
        f'<path d="{MARK_PATH}" fill="{color}" fill-rule="evenodd"/></g>'
    )


def mark_width(height: float) -> float:
    return ART_W * height / ART_H


# ------------------------------------------------------------------ primitives --

def doc(w: float, h: float, body: str, *, bg: str | None = None, radius: int = 0) -> str:
    ground = ""
    if bg:
        ground = f'<rect width="{w:.0f}" height="{h:.0f}"' + (f' rx="{radius}"' if radius else "") + f' fill="{bg}"/>'
    return (
        f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {w:.0f} {h:.0f}" '
        f'width="{w:.0f}" height="{h:.0f}" fill="none" role="img">\n{ground}\n{body}\n</svg>\n'
    )


def eyebrow(x: float, y: float, label: str, *, dot: str = SIGNAL, color: str = DUST,
            size: float = 11.5, tracking: float = 0.17) -> str:
    """The site's eyebrow: a small filled square, then uppercase mono with wide tracking.

    The square is TAN, not orange — measured from the site's own section eyebrows. The one
    exception is a genuine status eyebrow ("early testing"), which uses a green dot."""
    side = size * 0.62
    return (
        f'<rect x="{x:.2f}" y="{y - size * 0.74:.2f}" width="{side:.2f}" height="{side:.2f}" '
        f'rx="{side * 0.16:.2f}" fill="{dot}"/>'
        + text(label.upper(), x + size * 1.45, y, size, family=MONO, weight=500, color=color, tracking=tracking)
    )


def rule(x1: float, y: float, x2: float, color: str = EDGE, opacity: float = 1.0) -> str:
    return f'<line x1="{x1:.2f}" y1="{y:.2f}" x2="{x2:.2f}" y2="{y:.2f}" stroke="{color}" stroke-opacity="{opacity}"/>'


def arrow(x: float, y: float, size: float, color: str = SIGNAL) -> str:
    """A right arrow as geometry, not a character: the latin subsets omit U+2192, and two
    strokes stay crisp at 11px where a subset glyph would not."""
    h = size * 0.40
    return (
        f'<path d="M{x:.2f} {y:.2f} h{size:.2f} M{x + size - h:.2f} {y - h:.2f} l{h:.2f} {h:.2f} '
        f'l{-h:.2f} {h:.2f}" stroke="{color}" stroke-width="{max(1.0, size * 0.14):.2f}" '
        f'stroke-linecap="round" stroke-linejoin="round"/>'
    )


def vrule(x: float, y1: float, y2: float, color: str = EDGE) -> str:
    """The site divides a strip into columns with a vertical hairline rather than drawing
    separate cards. That is the single most recognisable thing about its section layout."""
    return f'<line x1="{x:.2f}" y1="{y1:.2f}" x2="{x:.2f}" y2="{y2:.2f}" stroke="{color}"/>'


def card(x: float, y: float, w: float, h: float, *, fill: str = PANEL, stroke: str = EDGE) -> str:
    return (f'<rect x="{x:.2f}" y="{y:.2f}" width="{w:.2f}" height="{h:.2f}" '
            f'rx="{RADIUS}" fill="{fill}" stroke="{stroke}"/>')


def meter(x: float, y: float, w: float, frac: float, color: str, *, h: float = 4.0) -> str:
    """The site's resource meter: a full-width track at EDGE_TRACK with a coloured fill.
    Not a segmented bar and not a coloured block — a thin measure of how much."""
    return (
        f'<rect x="{x:.2f}" y="{y:.2f}" width="{w:.2f}" height="{h}" rx="{h / 2}" fill="{EDGE_TRACK}"/>'
        f'<rect x="{x:.2f}" y="{y:.2f}" width="{max(3.0, w * frac):.2f}" height="{h}" rx="{h / 2}" fill="{color}"/>'
    )


def wrap(s: str, width: float, size: float, family: str = SANS, weight: int = 400) -> list[str]:
    """Greedy wrap measured with the real font, so a paragraph cannot overrun its column."""
    words, line, out = s.split(), "", []
    for word in words:
        trial = f"{line} {word}".strip()
        if line and text_width(trial, size, family, weight) > width:
            out.append(line)
            line = word
        else:
            line = trial
    if line:
        out.append(line)
    return out


def button(x: float, y: float, label: str, *, primary: bool = True,
           size: float = 13.0, pad_x: float = 16.0, h: float = 34.0) -> tuple[str, float]:
    """The site's primary CTA: a tan fill, near-black label, 4px radius, an arrow after.

    Measured off the live site: 148x34 for "Install Catalyst ->", radius 4, label ~13px in
    DM Sans at 500. The ghost variant is the same box with a tan hairline and tan label.
    Returns (markup, total width) so a caller can lay out two buttons side by side."""
    tw = text_width(label, size, SANS, 500)
    arrow_w = size * 0.9
    w = tw + arrow_w + pad_x * 2
    if primary:
        out = (f'<rect x="{x:.2f}" y="{y:.2f}" width="{w:.2f}" height="{h:.2f}" rx="{RADIUS}" fill="{SIGNAL}"/>'
               + text(label, x + pad_x, y + h / 2 + size * 0.36, size, weight=500, color=BG))
    else:
        out = (f'<rect x="{x:.2f}" y="{y:.2f}" width="{w:.2f}" height="{h:.2f}" rx="{RADIUS}" '
               f'fill="none" stroke="{SIGNAL}"/>'
               + text(label, x + pad_x, y + h / 2 + size * 0.36, size, weight=500, color=SIGNAL))
    out += arrow(x + pad_x + tw + size * 0.28, y + h / 2, arrow_w * 0.62,
                 BG if primary else SIGNAL)
    return out, w


def assert_fits(name: str, left_end: float, right_start: float) -> None:
    """A layout collision is a build failure, not something a reviewer has to notice."""
    if left_end > right_start:
        sys.exit(f"layout error in {name}: content ends at {left_end:.0f}px but the next column starts at {right_start:.0f}px")


# ---------------------------------------------------------------------- logos --

def build_logos() -> dict[str, str]:
    out: dict[str, str] = {}

    # Glyph alone, transparent ground, tight to the artwork. Padding is a quarter of the
    # artwork height so the file drops into a container without further adjustment.
    for suffix, color in (("", ORANGE), ("-mono-black", "#000000"), ("-mono-white", "#FFFFFF")):
        h = 432.0
        w = round(mark_width(h) + h * 0.5)
        out[f"logo-mark{suffix}.svg"] = doc(w, 512, mark(w / 2, 256, h, color))

    # App icon: the mark on the site's own ground in a squircle. This is the file that
    # becomes the favicon, so it is drawn to survive 32px.
    s = 512
    out["logo-badge.svg"] = doc(
        s, s,
        f'<rect width="{s}" height="{s}" rx="112" fill="{BG}"/>' + mark(s / 2, s / 2, 356),
    )

    # Horizontal lockup: mark, then "Catalyst" in Oxanium with "Billing" set in mono caps.
    # The two words differ in family and weight, not in colour — a two-colour wordmark
    # reads as two companies, and this is one.
    for theme, fg, dim in (("dark", CHALK, DUST), ("light", INK, "#6B655C")):
        size = 96.0
        mh = 116.0
        gap = 34.0
        w1 = text_width("Catalyst", size, DISPLAY, 600, -0.015)
        sub = 19.0
        sub_track = 0.30
        w2 = text_width("BILLING", sub, MONO, 500, sub_track)
        x0 = 14.0
        baseline = mh / 2 + size * 0.345
        left_end = x0 + mark_width(mh) + gap + w1
        right_start = left_end + 22
        assert_fits(f"lockup-horizontal-{theme}", right_start + w2, 10_000)
        width = right_start + w2 + 14
        body = (
            mark(x0 + mark_width(mh) / 2, mh / 2, mh)
            + text("Catalyst", x0 + mark_width(mh) + gap, baseline, size, family=DISPLAY, weight=600, color=fg, tracking=-0.015)
            + text("BILLING", right_start, baseline - size * 0.02, sub, family=MONO, weight=500, color=ORANGE, tracking=sub_track)
        )
        out[f"logo-lockup-horizontal-{theme}.svg"] = doc(round(width), round(mh), body)

    # Stacked lockup: the mark, the wordmark, then the product line. Used where the
    # horizontal form would be too wide to be legible (avatars, print, square slots).
    for theme, fg in (("dark", CHALK), ("light", INK)):
        size = 104.0
        mh = 150.0
        sub = 17.0
        sub_track = 0.34
        w1 = text_width("Catalyst", size, DISPLAY, 600, -0.015)
        w2 = text_width("BILLING", sub, MONO, 500, sub_track)
        width = max(w1, w2, mark_width(mh)) + 28
        cx = width / 2
        # The baseline has to clear the mark's own box, not a guessed fraction of the type
        # size: cap height is ~0.70em, so baseline = mark + cap + a quarter-em of air.
        cap = size * 0.70
        baseline = mh + cap + size * 0.26
        sub_y = baseline + sub * 2.1
        body = (
            mark(cx, mh / 2, mh)
            + text("Catalyst", cx, baseline, size, family=DISPLAY, weight=600, color=fg, tracking=-0.015, anchor="middle")
            + text("BILLING", cx, sub_y, sub, family=MONO, weight=500, color=ORANGE, tracking=sub_track, anchor="middle")
        )
        out[f"logo-lockup-stacked-{theme}.svg"] = doc(round(width), round(sub_y + sub * 0.9), body)

    return out


# --------------------------------------------------------------------- banners --

def build_readme_banner() -> str:
    """1280x320 — the README hero.

    Composed from the site's own parts, in its own order: an eyebrow, the lockup, a body
    line, a mono meta line, and one card on the right. Flat ground, tan accents, orange
    confined to the mark."""
    w, h = 1280, 320
    body = ""

    # --- right: a ledger card, the same anatomy as the site's vitals panel -----------
    cx0, cy0, cw, ch = 704, 48, 520, 232
    body += card(cx0, cy0, cw, ch)
    body += eyebrow(cx0 + 22, cy0 + 30, "ledger", size=10.5, tracking=0.22)
    body += text("Open", cx0 + cw - 40, cy0 + 30, 10.5, family=MONO, weight=500, color=SIGNAL, anchor="end", tracking=0.12)
    body += arrow(cx0 + cw - 34, cy0 + 26, 11, SIGNAL)
    body += rule(cx0, cy0 + 48, cx0 + cw)

    rows = (
        ("Northwind Hosting", "INV-2026-0141", "1,248.00", "PAID", SUCCESS),
        ("Kestrel Systems", "INV-2026-0142", "312.40", "OPEN", INFO),
        ("Greyline Labs", "INV-2026-0143", "96.00", "OVERDUE", DANGER),
    )
    row_h = 42.0
    for i, (who, ref, amount, state, dot) in enumerate(rows):
        ry = cy0 + 48 + i * row_h
        body += text(who, cx0 + 22, ry + 20, 12.5, weight=500, color=CHALK, tracking=-0.008)
        body += text(ref, cx0 + 22, ry + 34, 9.5, family=MONO, weight=400, color=FAINT, tracking=0.02)
        body += text(amount, cx0 + cw - 22, ry + 20, 13, family=MONO, weight=500, color=CHALK, anchor="end", tracking=-0.01)
        sw = text_width(state, 9, MONO, 500, 0.12)
        body += f'<circle cx="{cx0 + cw - 22 - sw - 8:.2f}" cy="{ry + 31.5}" r="2.3" fill="{dot}"/>'
        body += text(state, cx0 + cw - 22, ry + 34, 9, family=MONO, weight=500, color=dot, anchor="end", tracking=0.12)
        body += rule(cx0 + 22, ry + row_h, cx0 + cw - 22, EDGE)

    fy = cy0 + 48 + len(rows) * row_h
    body += text("Balance", cx0 + 22, fy + 26, 10, family=MONO, weight=500, color=DUST, tracking=0.16)
    body += text("3,516.40", cx0 + cw - 22, fy + 27, 17, family=MONO, weight=500, color=SIGNAL, anchor="end", tracking=-0.02)
    body += text("USD · per currency, never summed", cx0 + 22, fy + 44, 9.5, family=MONO, weight=400, color=FAINT, tracking=0.02)

    # --- left: the lockup, then the line that says what it is ------------------------
    mh = 86.0
    body += mark(56 + mark_width(mh) / 2, 96, mh)
    tx = 56 + mark_width(mh) + 26
    body += text("Catalyst", tx, 120, 58, family=DISPLAY, weight=600, color=CHALK, tracking=-0.02)
    body += text("BILLING", tx + text_width("Catalyst", 58, DISPLAY, 600, -0.02) + 16, 118,
                 15, family=MONO, weight=500, color=SIGNAL, tracking=0.30)

    body += text("Billing and service automation for hosting providers.", 56, 178, 18, color=MUTED, tracking=-0.004)
    body += text("Exact money · immutable invoices · a subledger that always balances",
                 56, 206, 11.5, family=MONO, weight=400, color=FAINT, tracking=0.015)
    body += rule(56, 244, 500, EDGE)
    body += eyebrow(56, 274, "self-hosted · go + postgresql · extensions", size=10, tracking=0.17)
    assert_fits("banner-readme", tx + text_width("Catalyst", 58, DISPLAY, 600, -0.02) + 200, cx0 - 30)

    return doc(w, h, body, bg=BG)


def build_github_banner() -> str:
    """1280x640 — GitHub's social preview.

    The site's section anatomy at poster scale: lockup, eyebrow, a headline with one tan
    phrase, a body line, a primary CTA, then a single stat strip divided by vertical
    hairlines. No floating cards and no coloured bars — the site draws neither."""
    w, h = 1280, 640
    body = ""

    mh = 54.0
    body += mark(72 + mark_width(mh) / 2, 82, mh)
    body += text("Catalyst", 72 + mark_width(mh) + 18, 96, 31, family=DISPLAY, weight=600, color=CHALK, tracking=-0.018)
    body += text("BILLING", 72 + mark_width(mh) + 18 + text_width("Catalyst", 31, DISPLAY, 600, -0.018) + 13, 94,
                 11, family=MONO, weight=500, color=ORANGE, tracking=0.28)
    body += rule(72, 134, w - 72, EDGE)

    body += eyebrow(72, 186, "revenue operations", size=11, tracking=0.18)

    lines = (("Revenue operations", CHALK), ("for the people who keep", CHALK), ("the ledger honest.", SIGNAL))
    size = 46.0
    for i, (line, col) in enumerate(lines):
        while text_width(line, size, DISPLAY, 600, -0.028) > w - 144:
            size -= 1
        body += text(line, 72, 250 + i * 54, size, family=DISPLAY, weight=600, color=col, tracking=-0.028)

    body += text("One Go binary. PostgreSQL. No framework in the frontend.", 72, 404, 15, color=MUTED)

    b1, _ = button(72, 428, "Clone the repo")
    body += b1

    sx, sy, sw_, sh = 72, 496, w - 144, 92
    body += card(sx, sy, sw_, sh)
    stats = (
        ("TABLES", "79", "one schema, 26 migrations", None),
        ("API OPERATIONS", "205", "generated, parity tested", None),
        ("FLOATS", "0", "integer minor units", None),
        ("BROWSER TESTS", "244", "every screen, headless", SUCCESS),
    )
    col_w = sw_ / len(stats)
    for i, (label, figure, desc, dot) in enumerate(stats):
        x = sx + i * col_w
        if i:
            body += vrule(x, sy, sy + sh, EDGE)
        px = x + 22
        if dot:
            body += f'<circle cx="{px + 3:.2f}" cy="{sy + 24.5}" r="3" fill="{dot}"/>'
            body += text(label, px + 13, sy + 28, 9.5, family=MONO, weight=500, color=DUST, tracking=0.14)
        else:
            body += text(label, px, sy + 28, 9.5, family=MONO, weight=500, color=DUST, tracking=0.14)
        body += text(figure, px, sy + 62, 24, family=DISPLAY, weight=600, color=CHALK, tracking=-0.01)
        body += text(desc, px, sy + 80, 9.5, family=MONO, weight=400, color=FAINT, tracking=0.01)

    body += text("catalystctl.com", 72, 620, 11, family=MONO, weight=400, color=FAINT, tracking=0.08)
    body += text("GPL-3.0 · self-hosted", w - 72, 620, 11, family=MONO, weight=400, color=FAINT, tracking=0.08, anchor="end")
    return doc(w, h, body, bg=BG)


def ledger_card(x: float, y: float, w: float, h: float) -> str:
    """The OG card's right panel, built from the site's table anatomy: a mono caps header,
    hairline rows, tabular figures, and a state that is a dot plus a word."""
    pad = 22.0
    s = [card(x, y, w, h)]
    s.append(eyebrow(x + pad, y + 30, "ledger", size=10.5, tracking=0.22))
    s.append(text("Open", x + w - pad - 18, y + 30, 10.5, family=MONO, weight=500, color=SIGNAL, anchor="end", tracking=0.12))
    s.append(arrow(x + w - pad - 13, y + 26, 11, SIGNAL))
    s.append(rule(x, y + 48, x + w))

    rows = (
        ("Northwind Hosting", "INV-2026-0141", "1,248.00", "PAID", SUCCESS),
        ("Kestrel Systems", "INV-2026-0142", "312.40", "OPEN", INFO),
        ("Greyline Labs", "INV-2026-0143", "96.00", "OVERDUE", DANGER),
        ("Vantage Cloud", "INV-2026-0144", "1,860.00", "DRAFT", DUST),
    )
    row_h = 44.0
    for i, (who, ref, amount, state, dot) in enumerate(rows):
        ry = y + 48 + i * row_h
        s.append(text(who, x + pad, ry + 21, 12.5, weight=500, color=CHALK, tracking=-0.008))
        s.append(text(ref, x + pad, ry + 35, 9.5, family=MONO, weight=400, color=FAINT, tracking=0.02))
        s.append(text(amount, x + w - pad, ry + 21, 13, family=MONO, weight=500, color=CHALK, anchor="end", tracking=-0.01))
        sw = text_width(state, 9, MONO, 500, 0.12)
        s.append(f'<circle cx="{x + w - pad - sw - 8:.2f}" cy="{ry + 32.5}" r="2.3" fill="{dot}"/>')
        s.append(text(state, x + w - pad, ry + 35, 9, family=MONO, weight=500, color=dot, anchor="end", tracking=0.12))
        s.append(rule(x + pad, ry + row_h, x + w - pad, EDGE))

    by = y + 48 + len(rows) * row_h
    s.append(text("Balance", x + pad, by + 24, 10, family=MONO, weight=500, color=DUST, tracking=0.16))
    s.append(text("3,516.40", x + w - pad, by + 25, 18, family=MONO, weight=500, color=SIGNAL, anchor="end", tracking=-0.02))
    # one honest measure: how much of the open book is collected, as the site meters CPU.
    s.append(meter(x + pad, by + 40, w - 2 * pad, 0.52, SUCCESS))
    s.append(text("USD · per currency, never summed", x + pad, by + 62, 9.5, family=MONO, weight=400, color=FAINT, tracking=0.02))
    return "".join(s)


def build_og_default() -> str:
    """1200x630 — the link card.

    Retells the site's hero for this product, in its order: lockup, eyebrow, headline with
    one tan phrase, body line, a primary CTA beside a ghost one, then the mono meta line
    that names the stack. The CTA matters: the site's hero is anchored by a tan button, and
    a card without one reads as a poster rather than as the same product."""
    w, h = 1200, 630
    body = ledger_card(636, 132, 500, 300)

    x = 72.0
    mh = 64.0
    body += mark(x + mark_width(mh) / 2, 148, mh)
    body += text("Catalyst", x + mark_width(mh) + 18, 162, 35, family=DISPLAY, weight=600, color=CHALK, tracking=-0.018)
    body += text("BILLING", x + mark_width(mh) + 18 + text_width("Catalyst", 35, DISPLAY, 600, -0.018) + 13, 160,
                 11.5, family=MONO, weight=500, color=ORANGE, tracking=0.28)

    body += eyebrow(x, 246, "self-hosted · go + postgresql · extensions", size=11, tracking=0.17)

    assert_fits("og-default headline", x + 520, 636 - 24)
    body += text("Billing and service", x, 320, 44, family=DISPLAY, weight=600, color=CHALK, tracking=-0.028)
    body += text("automation for", x, 370, 44, family=DISPLAY, weight=600, color=CHALK, tracking=-0.028)
    body += text("hosting providers.", x, 420, 44, family=DISPLAY, weight=600, color=SIGNAL, tracking=-0.028)

    body += text("Exact money · immutable invoices · a ledger that balances", x, 462, 12.5, family=MONO, weight=400, color=FAINT, tracking=0.015)

    b1, w1 = button(x, 494, "Get started")
    b2, _ = button(x + w1 + 10, 494, "Read the docs", primary=False)
    body += b1 + b2

    body += rule(x, 560, 500, EDGE)
    body += text("catalystctl.com", x, 590, 12, family=MONO, weight=400, color=MUTED, tracking=0.08)
    body += text("self-hosted · GPL-3.0", x + 190, 590, 11, family=MONO, weight=400, color=FAINT, tracking=0.06)
    return doc(w, h, body, bg=BG)


def build_og_architecture() -> str:
    """The second card: the site's three-column numbered strip, which is how it explains a
    process. One bordered box, divided by hairlines — not three cards with coloured edges."""
    w, h = 1200, 630
    body = ""

    mh = 54.0
    body += mark(72 + mark_width(mh) / 2, 90, mh)
    body += text("Catalyst", 72 + mark_width(mh) + 18, 106, 31, family=DISPLAY, weight=600, color=CHALK, tracking=-0.018)
    body += text("BILLING", 72 + mark_width(mh) + 18 + text_width("Catalyst", 31, DISPLAY, 600, -0.018) + 13, 104,
                 11, family=MONO, weight=500, color=SIGNAL, tracking=0.28)
    body += rule(72, 140, w - 72, EDGE)

    body += eyebrow(72, 192, "architecture", size=11, tracking=0.18)
    body += text("One binary, three trust boundaries.", 72, 246, 38, family=DISPLAY, weight=600, color=CHALK, tracking=-0.026)
    body += text("The core is the authority. A provider is never the last word on what happened.",
                 72, 278, 15.5, color=MUTED)

    cols = (
        ("01", "panel", "TypeScript, no framework, embedded",
         "Everything it shows comes from the API, and every action it hides is refused by the server.",
         "reads /api/v1 · refuses nothing it is shown"),
        ("02", "core", "Go · exact money · the subledger",
         "Constraints and triggers hold what must never be false; a stale worker cannot act.",
         "postgresql · one schema · 26 migrations"),
        ("03", "provider", "out-of-process extension",
         "Crash-isolated JSON-RPC. An unverifiable outcome becomes unknown and waits for reconciliation.",
         "NDJSON · deadlines · crash isolation"),
    )
    sx, sy, sw_, sh = 72, 316, w - 144, 240
    body += card(sx, sy, sw_, sh)
    col_w = sw_ / 3
    for i, (num, name, sub, blurb, foot) in enumerate(cols):
        x = sx + i * col_w
        if i:
            body += vrule(x, sy, sy + sh, EDGE)
        px = x + 24
        body += text(num, px, sy + 34, 10.5, family=MONO, weight=500, color=FAINT, tracking=0.18)
        body += text(name, px, sy + 70, 22, family=DISPLAY, weight=600, color=CHALK, tracking=-0.015)
        body += text(sub, px, sy + 92, 10.5, family=MONO, weight=400, color=DUST, tracking=0.01)
        for j, ln in enumerate(wrap(blurb, col_w - 48, 12.5)):
            body += text(ln, px, sy + 122 + j * 19, 12.5, color=MUTED, tracking=-0.004)
        body += rule(px, sy + sh - 44, x + col_w - 24, EDGE)
        body += text(foot, px, sy + sh - 24, 9.5, family=MONO, weight=400, color=FAINT, tracking=0.02)

    body += rule(72, 590, w - 72, EDGE)
    body += text("catalystctl.com", 72, 620, 11.5, family=MONO, weight=400, color=FAINT, tracking=0.08)
    body += text("self-hosted · GPL-3.0 · one postgresql database", w - 72, 620, 11, family=MONO, weight=400, color=FAINT, tracking=0.06, anchor="end")
    return doc(w, h, body, bg=BG)


def main() -> None:
    OUT.mkdir(parents=True, exist_ok=True)
    files: dict[str, str] = {}
    files.update(build_logos())
    files["banner-readme.svg"] = build_readme_banner()
    files["banner-github.svg"] = build_github_banner()
    files["og-default.svg"] = build_og_default()
    files["og-architecture.svg"] = build_og_architecture()

    for name, svg in files.items():
        path = OUT / name
        path.write_text(svg)
        print(f"  {path.relative_to(ROOT)}  {len(svg) / 1024:.1f} KiB")
    print(f"{len(files)} SVG files written to {OUT.relative_to(ROOT)}")


if __name__ == "__main__":
    main()
