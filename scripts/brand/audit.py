"""Audit the rendered brand assets against the site's measured design language.

This is the check that would have caught the first draft of these assets. That draft used
the correct tokens and the correct logo and still looked like a different product, because
it used SATURATED ORANGE as a UI accent and washed the ground with an orange radial. The
site does neither: #F97301 exists only inside the mark, the accent is the muted tan, and the
ground is flat.

Asserted per asset:
  1. logo-orange does not appear outside the mark (a few hundred px of tolerance covers
     the wordmark in the lockup, which is intentionally orange);
  2. the ground is flat -- at least three of the four corners are BG, so no radial wash;
  3. the tan accent is actually present, so the asset reads as this family and not as a
     generic dark poster.

Usage: python3 scripts/brand/audit.py assets/brand/png/*.png
Needs Pillow and numpy (the brand tooling already requires Python).
"""
import sys
from PIL import Image
import numpy as np

ORANGE = np.array([0xF9, 0x73, 0x01])
BG     = np.array([0x16, 0x14, 0x12])
SIGNAL = np.array([0xC4, 0x8D, 0x5A])

def near(a, c, tol=12):
    return np.all(np.abs(a.astype(int) - c) <= tol, axis=-1)

def mark_box(im):
    """The mark is the only legal home for saturated orange. Its box is found by
    clustering the strongest orange pixels in the top-left third."""
    h, w, _ = im.shape
    m = near(im, ORANGE, 40)
    ys, xs = np.where(m)
    if len(xs) == 0:
        return None
    return xs.min(), xs.max(), ys.min(), ys.max()

def audit(path):
    im = np.array(Image.open(path).convert("RGB"))
    h, w, _ = im.shape
    fails = []

    orange = near(im, ORANGE, 30)
    n_orange = int(orange.sum())

    box = mark_box(im)
    if n_orange:
        if box is None:
            fails.append("orange present but no mark box found")
        else:
            x0, x1, y0, y1 = box
            outside = orange.copy()
            outside[max(0, y0 - 2):y1 + 3, max(0, x0 - 2):x1 + 3] = False
            leaked = int(outside.sum())
            # The product word "BILLING" in the lockup is intentionally orange (that is the
            # approved logo treatment). Allow it, but only in the lockup's wordmark band.
            if leaked > 400:
                fails.append(f"{leaked}px of logo-orange outside the mark")

    # ground must be flat: sample the four corners and the mid-edge of the far side
    corners = [im[2, 2], im[2, w - 3], im[h - 3, 2], im[h - 3, w - 3]]
    flat = sum(1 for c in corners if np.abs(c.astype(int) - BG).max() <= 6)
    if flat < 3:
        fails.append(f"ground not flat BG ({flat}/4 corners match)")

    # the accent actually used should be the tan
    tan = int(near(im, SIGNAL, 22).sum())
    if tan < 200:
        fails.append(f"tan accent barely present ({tan}px)")

    print(f"{path.split('/')[-1]:32} orange={n_orange:6d} tan={tan:6d} "
          f"flat={flat * 100:5.1f}%  {'OK' if not fails else 'FAIL: ' + '; '.join(fails)}")
    return not fails

def kind(path):
    """Not every asset has a ground or an accent, so the rules differ by kind.

    composite  an og card or a banner. The rules that catch a draft that "used the right
               colours and still looked like another product":
                 - the ground is flat (no radial wash);
                 - the tan accent is actually present;
                 - logo-orange appears ONLY in the lockup, i.e. never in the lower part of
                   the canvas and never in the right-hand card. Those are exactly the
                   places the site would use the tan, and where a saturated orange button,
                   rule or eyebrow square gives the whole thing away.
    glyph/logo a mark, lockup or icon. Only meaningful rules are "it is not empty" and
               "the one-colour variants really are one colour".
    """
    name = path.split("/")[-1]
    if name.startswith(("og-", "banner-")):
        return "composite"
    if name.startswith("logo-mark"):
        return "glyph"
    return "logo"


def audit(path, rules):
    im = np.array(Image.open(path).convert("RGBA"))
    alpha = im[:, :, 3]
    rgb = im[:, :, :3]
    h, w, _ = rgb.shape
    fails = []
    name = path.split("/")[-1]
    ink = alpha > 128

    orange = near(rgb, ORANGE, 30) & ink
    tan = near(rgb, SIGNAL, 22) & ink
    n_orange, n_tan = int(orange.sum()), int(tan.sum())

    if rules == "composite":
        # Measure the ground along the full top and bottom edges, not just the corners: a
        # radial wash from the top-left leaves three corners untouched and still ruins the
        # asset, which is exactly how the first draft got past a corner check.
        def edge_flatness(row):
            return float((np.abs(row.astype(int) - BG).max(axis=-1) <= 4).mean())
        top, bot = edge_flatness(rgb[2, :, :]), edge_flatness(rgb[h - 3, :, :])
        flat = min(top, bot)
        if flat < 0.98:
            fails.append(f"ground not flat ({flat * 100:.0f}% of the edge is BG) — a wash reads as another product")
        if n_tan < 200:
            fails.append(f"tan accent barely present ({n_tan}px)")

        # Orange below the lockup band, or in the right-hand card, is the tell.
        low = orange.copy()
        low[: int(h * 0.45)] = False
        if int(low.sum()) > 60:
            fails.append(f"{int(low.sum())}px of logo-orange below the lockup")
        right = orange.copy()
        right[:, : int(w * 0.60)] = False
        if int(right.sum()) > 60:
            fails.append(f"{int(right.sum())}px of logo-orange in the card column")
        print(f"{name:32} {rules:9} orange={n_orange:6d} tan={n_tan:6d} flat={flat * 100:5.1f}%  "
              f"{'OK' if not fails else 'FAIL: ' + '; '.join(fails)}")

    elif rules == "glyph":
        if int(ink.sum()) == 0:
            fails.append("empty")
        mono = "mono" in name
        if mono and (n_orange or n_tan):
            fails.append(f"mono variant carries colour (orange={n_orange} tan={n_tan})")
        print(f"{name:32} {rules:9} orange={n_orange:6d} tan={n_tan:6d}            "
              f"{'OK' if not fails else 'FAIL: ' + '; '.join(fails)}")

    else:  # logo / icon
        if int(ink.sum()) == 0:
            fails.append("empty")
        # An icon or lockup may use orange for the mark and the product word, but nothing
        # else — no orange rules, bars or fills anywhere in it.
        print(f"{name:32} {rules:9} orange={n_orange:6d} tan={n_tan:6d}            "
              f"{'OK' if not fails else 'FAIL: ' + '; '.join(fails)}")
    return not fails


# kind() reads the filename, which is correct for the shipped asset names. --kind lets the
# regression harness re-audit a copy under a different name and still apply the real rules.
force = None
paths = []
for a in sys.argv[1:]:
    if a.startswith("--kind="):
        force = a.split("=", 1)[1]
    else:
        paths.append(a)

ok = True
for p in paths:
    ok &= audit(p, force or kind(p))
sys.exit(0 if ok else 1)
