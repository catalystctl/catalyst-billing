#!/usr/bin/env python3
"""Prove that the brand audit actually rejects the mistakes it exists to catch.

An audit nobody has seen fail is not an audit — it is a green light with no wiring. This
script takes a known-good asset, reintroduces each mistake the first draft made, and
asserts the audit rejects it. If someone later loosens a threshold in audit.py, this fails.

The two mistakes, both of which the first draft shipped:
  1. saturated logo-orange used as a UI accent (a button where the site uses the tan);
  2. an orange radial wash on the ground (the site's ground is flat).

Usage: python3 scripts/brand/regress.py [assets/brand/png/og-default.png]
Requires Pillow and numpy.
"""

from __future__ import annotations

import subprocess
import sys
import tempfile
from pathlib import Path

import numpy as np
from PIL import Image, ImageDraw

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
AUDIT = HERE / "audit.py"
ORANGE = (0xF9, 0x73, 0x01)


def audit(path: Path, kind: str = "composite") -> tuple[bool, str]:
    r = subprocess.run(
        [sys.executable, str(AUDIT), f"--kind={kind}", str(path)],
        capture_output=True, text=True, cwd=ROOT,
    )
    lines = (r.stdout.strip() or r.stderr.strip()).splitlines()
    return r.returncode == 0, (lines[-1] if lines else "")


def main() -> int:
    src = Path(sys.argv[1]) if len(sys.argv) > 1 else ROOT / "assets/brand/png/og-default.png"
    if not src.exists():
        print(f"regress: {src} does not exist; run scripts/brand/render.sh first")
        return 1

    tmp = Path(tempfile.mkdtemp(prefix="brand-regress-"))
    ok = True

    # 0. the real asset must pass, or every rejection below proves nothing
    passed, line = audit(src)
    print(f"  {'PASS' if passed else 'FAIL'}  the shipped asset is accepted")
    print(f"        {line}")
    ok &= passed

    # 1. an orange button in place of the tan one
    im = Image.open(src).convert("RGB")
    d = ImageDraw.Draw(im)
    d.rounded_rectangle([72, 494, 250, 528], radius=4, fill=ORANGE)
    p1 = tmp / "orange-button.png"
    im.save(p1)
    passed, line = audit(p1)
    print(f"  {'PASS' if not passed else 'FAIL'}  an orange button is rejected")
    print(f"        {line}")
    ok &= not passed

    # 2. the orange radial wash the first draft painted on every banner
    a = np.array(Image.open(src).convert("RGB")).astype(float)
    h, w, _ = a.shape
    yy, xx = np.mgrid[0:h, 0:w]
    r = np.sqrt(((xx - w * 0.12) / (w * 0.92)) ** 2 + ((yy + h * 0.14) / (h * 0.92)) ** 2)
    fade = np.clip(1 - r, 0, 1)[:, :, None] * 0.11
    washed = a * (1 - fade) + np.array(ORANGE) * fade
    p2 = tmp / "wash.png"
    Image.fromarray(washed.astype(np.uint8)).save(p2)
    passed, line = audit(p2)
    print(f"  {'PASS' if not passed else 'FAIL'}  an orange ground wash is rejected")
    print(f"        {line}")
    ok &= not passed

    # 3. an asset with the accent removed entirely: the thing that made the first draft's
    #    architecture card read as a generic dark poster rather than as this family
    a = np.array(Image.open(src).convert("RGB"))
    tan = np.array([0xC4, 0x8D, 0x5A])
    a[np.all(np.abs(a.astype(int) - tan) <= 22, axis=-1)] = (0xF3, 0xF2, 0xF2)
    p3 = tmp / "no-accent.png"
    Image.fromarray(a).save(p3)
    passed, line = audit(p3)
    print(f"  {'PASS' if not passed else 'FAIL'}  an asset with no tan accent is rejected")
    print(f"        {line}")
    ok &= not passed

    print("\nbrand audit regression: " + ("ok" if ok else "FAILED"))
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
