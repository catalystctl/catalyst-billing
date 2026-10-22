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
# The site's tokens, converted from HSL. Kept as literals rather than parsed from the
# site's stylesheet: a build that scraped a live URL would fail offline, and one that
# parsed our own CSS would silently half-brand itself on a rename.

INK = "#110F0E"        # --ink            30 10% 6%
BG = "#161412"         # --background     30 10% 8%
PANEL = "#1C1A17"      # --panel          36 10% 10%
PANEL_2 = "#24221E"    # --panel-2        34 10% 13%
EDGE = "#3B3830"       # --edge           44 10% 21%
CHALK = "#EFEEEB"      # --chalk          40 12% 93%
DUST = "#A6A096"       # --dust           38 8% 62%
MUTED = "#B6B3AF"      # --muted-foreground 37 5% 70%
SIGNAL = "#C48D5A"     # --signal         29 47% 56%
SUCCESS = "#23B370"    # --success       152 67% 42%
WARNING = "#DF9516"    # --warning        38 82% 48%
DANGER = "#D93F3F"     # --danger          0 67% 55%
INFO = "#3F7AD9"       # --info          217 67% 55%

ORANGE = "#F97301"     # the mark, sampled from the parent logo
ORANGE_DEEP = "#D95F00"  # one step down, for hairlines and rules

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


def eyebrow(x: float, y: float, label: str, *, color: str = DUST, size: float = 11.5, tracking: float = 0.17) -> str:
    """The site's eyebrow: a small filled square, then uppercase mono with wide tracking.
    It is the element that most obviously says 'same family as catalystctl.com'."""
    side = size * 0.62
    return (
        f'<rect x="{x:.2f}" y="{y - size * 0.74:.2f}" width="{side:.2f}" height="{side:.2f}" '
        f'rx="{side * 0.16:.2f}" fill="{ORANGE}"/>'
        + text(label.upper(), x + size * 1.45, y, size, family=MONO, weight=500, color=color, tracking=tracking)
    )


def rule(x1: float, y: float, x2: float, color: str = EDGE, opacity: float = 1.0) -> str:
    return f'<line x1="{x1:.2f}" y1="{y:.2f}" x2="{x2:.2f}" y2="{y:.2f}" stroke="{color}" stroke-opacity="{opacity}"/>'


def arrow(x: float, y: float, size: float, color: str = ORANGE) -> str:
    """A right arrow as geometry, not a character: the latin subsets omit U+2192, and two
    strokes stay crisp at 11px where a subset glyph would not."""
    h = size * 0.40
    return (
        f'<path d="M{x:.2f} {y:.2f} h{size:.2f} M{x + size - h:.2f} {y - h:.2f} l{h:.2f} {h:.2f} '
        f'l{-h:.2f} {h:.2f}" stroke="{color}" stroke-width="{max(1.0, size * 0.14):.2f}" '
        f'stroke-linecap="round" stroke-linejoin="round"/>'
    )


def wash(w: float, h: float, color: str = ORANGE, opacity: float = 0.10) -> str:
    """The site's own page wash: one soft radial from the top-left, so the ground is not a
    flat rectangle. Kept faint — at more than ~0.12 it reads as a gradient, not a light."""
    return (
        f'<defs><radialGradient id="wash" cx="12%" cy="-14%" r="92%">'
        f'<stop offset="0" stop-color="{color}" stop-opacity="{opacity}"/>'
        f'<stop offset="1" stop-color="{color}" stop-opacity="0"/>'
        f"</radialGradient></defs>"
        f'<rect width="{w:.0f}" height="{h:.0f}" fill="url(#wash)"/>'
    )


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
        f'<rect width="{s}" height="{s}" rx="112" fill="{BG}"/>' + wash(s, s, ORANGE, 0.16) + mark(s / 2, s / 2, 356),
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
    """1280x320 — the README hero. Everything a reader needs in one screen: the family,
    the product, what it is, and what it is built on."""
    w, h = 1280, 320
    body = wash(w, h, ORANGE, 0.11)

    # A ledger sheet on the right: ruled rows with money in tabular figures and a settled
    # marker per row. The only decoration, and it is the product's own shape.
    gx0, gx1 = 744, w - 56
    rows = (
        ("INV-2026-0141", "1,248.00", SUCCESS),
        ("INV-2026-0142", "312.40", INFO),
        ("INV-2026-0143", "96.00", DANGER),
        ("INV-2026-0144", "1,860.00", DUST),
    )
    top, row_h = 78.0, 40.0
    # The panel has to be tall enough for the header rule, the rows, the closing rule and
    # the balance line — computed from those parts, so adding a row cannot clip the total.
    sheet_h = 26 + 8 + len(rows) * row_h + 4 + 44
    sheet = [f'<rect x="{gx0}" y="{top - 26}" width="{gx1 - gx0}" height="{sheet_h}" rx="6" fill="{PANEL}" stroke="{EDGE}"/>']
    sheet.append(text("LEDGER", gx0 + 18, top - 4, 10, family=MONO, weight=500, color=DUST, tracking=0.22))
    sheet.append(rule(gx0, top + 8, gx1, EDGE))
    for i, (ref, amount, col) in enumerate(rows):
        ry = top + 8 + i * row_h
        sheet.append(f'<circle cx="{gx0 + 24}" cy="{ry + 20}" r="3" fill="{col}"/>')
        sheet.append(text(ref, gx0 + 38, ry + 24, 12, family=MONO, weight=400, color=MUTED, tracking=0.02))
        sheet.append(text(amount, gx1 - 20, ry + 24, 13, family=MONO, weight=500, color=CHALK, anchor="end"))
        if i < len(rows) - 1:
            sheet.append(rule(gx0 + 18, ry + row_h, gx1 - 18, EDGE, 0.55))
    sheet.append(rule(gx0, top + 8 + len(rows) * row_h + 4, gx1, EDGE))
    sheet.append(text("Balance", gx0 + 18, top + 8 + len(rows) * row_h + 32, 11, family=MONO, weight=500, color=DUST, tracking=0.16))
    sheet.append(text("3,516.40", gx1 - 20, top + 8 + len(rows) * row_h + 33, 16, family=MONO, weight=500, color=ORANGE, anchor="end"))
    body += "".join(sheet)

    mh = 132.0
    x = 56.0
    body += mark(x + mark_width(mh) / 2, 132, mh)
    tx = x + mark_width(mh) + 40
    assert_fits("banner-readme", tx + 460, gx0 - 30)

    body += eyebrow(tx, 84, "catalyst · self-hosted · go + postgresql", size=10.5, tracking=0.18)
    body += text("Catalyst", tx, 178, 66, family=DISPLAY, weight=600, color=CHALK, tracking=-0.02)
    body += text("Billing", tx + text_width("Catalyst", 66, DISPLAY, 600, -0.02) + 18, 178, 30, family=MONO, weight=500, color=ORANGE, tracking=0.24)
    body += text("Billing and service automation for hosting providers.", tx, 218, 19, color=MUTED, tracking=-0.004)
    body += text("Exact money · immutable invoices · a subledger that always balances", tx, 250, 12.5, family=MONO, weight=400, color=DUST, tracking=0.015)
    return doc(w, h, body, bg=BG)


def build_github_banner() -> str:
    """1280x640 — GitHub's social preview. Its own idea rather than a bigger OG card:
    the claim on the left, the three numbers that back it on the right, and the ledger
    rule motif tying it to the product."""
    w, h = 1280, 640
    body = wash(w, h, ORANGE, 0.11)

    # header lockup
    mh = 66.0
    body += mark(72 + mark_width(mh) / 2, 96, mh)
    body += text("Catalyst", 72 + mark_width(mh) + 22, 116, 38, family=DISPLAY, weight=600, color=CHALK, tracking=-0.018)
    body += text("BILLING", 72 + mark_width(mh) + 22 + text_width("Catalyst", 38, DISPLAY, 600, -0.018) + 16, 114,
                 12.5, family=MONO, weight=500, color=ORANGE, tracking=0.28)

    # headline — three lines, each measured, so it cannot reach the right column
    col_split = 660.0
    lines = (("Revenue operations", CHALK), ("for the people who keep", CHALK), ("the ledger honest.", ORANGE))
    for i, (line, col) in enumerate(lines):
        size = 58.0
        while text_width(line, size, DISPLAY, 600, -0.028) > col_split - 72 - 40:
            size -= 1
        body += text(line, 72, 292 + i * 68, size, family=DISPLAY, weight=600, color=col, tracking=-0.028)
        assert_fits("banner-github headline", 72 + text_width(line, size, DISPLAY, 600, -0.028), col_split - 40)

    body += text("One Go binary. PostgreSQL. No framework in the frontend.", 72, 512, 17, color=MUTED)

    # footer rule + wordmark
    body += rule(72, 560, w - 72, EDGE)
    body += text("catalystctl.com", 72, 596, 12.5, family=MONO, weight=400, color=DUST, tracking=0.08)
    body += text("GPL-3.0 · self-hosted", w - 72, 596, 12.5, family=MONO, weight=400, color=DUST, tracking=0.08, anchor="end")

    # right column: three facts, each with a rule and a figure
    facts = (
        ("79", "tables", "one schema, 26 migrations", ORANGE),
        ("205", "API operations", "generated document, parity tested", INFO),
        ("0", "floats", "integer minor units end to end", SUCCESS),
    )
    fx = col_split
    for i, (num, unit, claim, col) in enumerate(facts):
        fy = 300 + i * 96
        body += f'<rect x="{fx}" y="{fy - 34}" width="3" height="62" rx="1.5" fill="{col}"/>'
        body += text(num, fx + 26, fy, 42, family=MONO, weight=500, color=CHALK, tracking=-0.02)
        body += text(unit, fx + 26 + text_width(num, 42, MONO, 500) + 14, fy, 15, color=MUTED)
        body += text(claim, fx + 26, fy + 24, 12, family=MONO, weight=400, color=DUST, tracking=0.01)
        assert_fits("banner-github facts", fx + 26 + text_width(claim, 12, MONO, 400), w - 60)
    return doc(w, h, body, bg=BG)


# ------------------------------------------------------------------------- og --

def ledger_card(x: float, y: float, w: float, h: float) -> str:
    """The OG card's right panel. Same anatomy as the parent site's vitals panel — an
    eyebrow header, hairline rules, mono labels, a footer of status dots — filled with
    the product's own subject: invoices and money in tabular figures."""
    pad = 22.0
    s = [f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="8" fill="{PANEL}" stroke="{EDGE}"/>']
    s.append(eyebrow(x + pad, y + 30, "ledger", size=10.5, tracking=0.22))
    s.append(text("Open", x + w - pad - 18, y + 30, 10.5, family=MONO, weight=500, color=ORANGE, anchor="end", tracking=0.12))
    s.append(arrow(x + w - pad - 13, y + 26, 12, ORANGE))
    s.append(rule(x, y + 50, x + w))

    rows = (
        ("Northwind Hosting", "INV-2026-0141", "1,248.00", "PAID", SUCCESS),
        ("Kestrel Systems", "INV-2026-0142", "312.40", "OPEN", INFO),
        ("Greyline Labs", "INV-2026-0143", "96.00", "OVERDUE", DANGER),
        ("Vantage Cloud", "INV-2026-0144", "1,860.00", "DRAFT", DUST),
    )
    row_h = 48.0
    for i, (who, ref, amount, status, col) in enumerate(rows):
        ry = y + 50 + i * row_h
        s.append(text(who, x + pad, ry + 23, 13.5, weight=500, color=CHALK, tracking=-0.008))
        s.append(text(ref, x + pad, ry + 39, 10.5, family=MONO, weight=400, color=DUST, tracking=0.02))
        s.append(text(amount, x + w - pad, ry + 23, 14, family=MONO, weight=500, color=CHALK, anchor="end", tracking=-0.01))
        sw = text_width(status, 9.5, MONO, 500, 0.12)
        s.append(f'<circle cx="{x + w - pad - sw - 9:.2f}" cy="{ry + 35.5}" r="2.6" fill="{col}"/>')
        s.append(text(status, x + w - pad, ry + 39, 9.5, family=MONO, weight=500, color=col, anchor="end", tracking=0.12))
        if i < len(rows) - 1:
            s.append(rule(x + pad, ry + row_h, x + w - pad, EDGE, 0.6))

    by = y + 50 + len(rows) * row_h
    s.append(rule(x, by, x + w))
    s.append(text("Balance", x + pad, by + 30, 10.5, family=MONO, weight=500, color=DUST, tracking=0.16))
    s.append(text("3,516.40", x + w - pad, by + 32, 20, family=MONO, weight=500, color=ORANGE, anchor="end", tracking=-0.02))

    bar_y, bar_h, bar_w = by + 46, 5.0, w - 2 * pad
    cx = x + pad
    for frac, col in ((0.52, SUCCESS), (0.30, INFO), (0.18, DANGER)):
        seg = bar_w * frac
        s.append(f'<rect x="{cx:.2f}" y="{bar_y}" width="{seg - 3:.2f}" height="{bar_h}" rx="{bar_h / 2:.2f}" fill="{col}"/>')
        cx += seg

    lx = x + pad
    for label, col in (("12 paid", SUCCESS), ("3 open", INFO), ("1 overdue", DANGER)):
        s.append(f'<circle cx="{lx + 3:.2f}" cy="{bar_y + 26}" r="3" fill="{col}"/>')
        s.append(text(label, lx + 13, bar_y + 30, 10.5, family=MONO, weight=400, color=MUTED, tracking=0.04))
        lx += 13 + text_width(label, 10.5, MONO, 400, 0.04) + 22
    return "".join(s)


def build_og_default() -> str:
    """1200x630 — the link card. The parent's OG grammar (eyebrow, big wordmark, one
    subline, a panel on the right) with the product's own panel content."""
    w, h = 1200, 630
    body = wash(w, h, ORANGE, 0.12)
    body += ledger_card(628, 150, 508, 356)

    x = 72.0
    mh = 84.0
    body += mark(x + mark_width(mh) / 2, 168, mh)
    body += text("Catalyst", x + mark_width(mh) + 24, 186, 44, family=DISPLAY, weight=600, color=CHALK, tracking=-0.018)
    body += text("BILLING", x + mark_width(mh) + 24 + text_width("Catalyst", 44, DISPLAY, 600, -0.018) + 16, 184,
                 13, family=MONO, weight=500, color=ORANGE, tracking=0.28)

    assert_fits("og-default headline", x + 520, 628 - 24)
    body += text("Billing and service", x, 306, 52, family=DISPLAY, weight=600, color=CHALK, tracking=-0.028)
    body += text("automation for hosting", x, 362, 52, family=DISPLAY, weight=600, color=CHALK, tracking=-0.028)
    body += text("providers.", x, 418, 52, family=DISPLAY, weight=600, color=ORANGE, tracking=-0.028)

    body += text("Exact money · immutable invoices · a ledger that balances", x, 468, 14, family=MONO, weight=400, color=DUST, tracking=0.015)
    body += rule(x, 512, 420, EDGE)
    body += text("catalystctl.com", x, 548, 13, family=MONO, weight=400, color=MUTED, tracking=0.08)
    body += text("self-hosted · GPL-3.0", x, 574, 11.5, family=MONO, weight=400, color=DUST, tracking=0.06)
    return doc(w, h, body, bg=BG)


def build_og_architecture() -> str:
    """The second card: what it is made of, for docs and technical posts. Three columns,
    one per trust boundary — the only architecture diagram this system actually needs."""
    w, h = 1200, 630
    body = wash(w, h, ORANGE, 0.11)

    mh = 58.0
    body += mark(72 + mark_width(mh) / 2, 96, mh)
    body += text("Catalyst", 72 + mark_width(mh) + 20, 114, 34, family=DISPLAY, weight=600, color=CHALK, tracking=-0.018)
    body += text("BILLING", 72 + mark_width(mh) + 20 + text_width("Catalyst", 34, DISPLAY, 600, -0.018) + 14, 112,
                 12, family=MONO, weight=500, color=ORANGE, tracking=0.28)

    body += text("One binary, three trust boundaries.", 72, 214, 40, family=DISPLAY, weight=600, color=CHALK, tracking=-0.026)
    body += text("The core is the authority. A provider is never the last word on what happened.", 72, 248, 16.5, color=MUTED)

    cols = (
        ("01", "panel", "TypeScript, no framework, embedded",
         "Everything it shows comes from the API, and every action it hides is refused by the server.", ORANGE),
        ("02", "core", "Go · exact money · the subledger",
         "Constraints and triggers hold what must never be false; a stale worker cannot act.", INFO),
        ("03", "provider", "out-of-process extension",
         "Crash-isolated JSON-RPC. An unverifiable outcome becomes unknown and waits for reconciliation.", SUCCESS),
    )
    gap = 26.0
    cw = (w - 72 * 2 - 2 * gap) / 3
    for i, (num, name, sub, blurb, col) in enumerate(cols):
        cx = 72 + i * (cw + gap)
        body += f'<rect x="{cx:.2f}" y="300" width="{cw:.2f}" height="228" rx="8" fill="{PANEL}" stroke="{EDGE}"/>'
        body += f'<rect x="{cx:.2f}" y="300" width="{cw:.2f}" height="3" rx="1.5" fill="{col}"/>'
        body += text(num, cx + 22, 334, 11, family=MONO, weight=500, color=col, tracking=0.18)
        body += text(name, cx + 22, 374, 25, family=DISPLAY, weight=600, color=CHALK, tracking=-0.015)
        body += text(sub, cx + 22, 398, 11, family=MONO, weight=400, color=DUST, tracking=0.01)

        words, line, lines = blurb.split(), "", []
        for word in words:
            trial = f"{line} {word}".strip()
            if text_width(trial, 13, SANS) > cw - 44 and line:
                lines.append(line)
                line = word
            else:
                line = trial
        lines.append(line)
        for j, ln in enumerate(lines):
            body += text(ln, cx + 22, 428 + j * 20, 13, color=MUTED, tracking=-0.004)

    body += rule(72, 566, w - 72, EDGE)
    body += text("catalystctl.com", 72, 600, 12.5, family=MONO, weight=400, color=MUTED, tracking=0.08)
    body += text("self-hosted · GPL-3.0 · one postgresql database", w - 72, 600, 11.5, family=MONO, weight=400, color=DUST, tracking=0.06, anchor="end")
    return doc(w, h, body, bg=BG)


# ----------------------------------------------------------------------- main --

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
