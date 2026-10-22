#!/usr/bin/env bash
# Rasterise the brand SVGs to PNG with headless Chromium.
#
# Chromium rather than a converter because it is the renderer the product itself is
# viewed in, it is already required by scripts/browser-smoke.sh, and it is the only
# SVG rasteriser on a stock machine that handles the outlined paths, gradients and
# sub-pixel hairlines here without artifacts.
#
# Two rules this script exists to enforce:
#
#   1. The window size always comes from the SVG's own width/height. Passing a fixed
#      viewport is how a logo ends up as a small mark in a large empty canvas.
#   2. A transparent export is unreadable on the wrong background — a chalk-on-dark
#      lockup is invisible on white. Each asset therefore declares the ground it is
#      meant to sit on, and the preview is rendered on that ground. The SVG stays
#      transparent so the PNG is a preview, not a second source of truth.
#
#   ./scripts/brand/render.sh            # everything
#   ./scripts/brand/render.sh og logo    # selected groups

set -euo pipefail

cd "$(dirname "$0")/../.."

SRC=assets/brand
PNG=assets/brand/png
CHROME="${CHROME:-chromium}"

if ! command -v "$CHROME" >/dev/null; then
  echo "render: $CHROME not found; set CHROME=/path/to/chromium" >&2
  exit 1
fi

mkdir -p "$PNG"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# The two grounds, from the site's own tokens (--background and --chalk).
DARK=161412
LIGHT=F5F6F8

# svg_size <file> — prints "W H" from the SVG's width/height attributes.
svg_size() {
  local wh
  wh=$(sed -n 's/.*width="\([0-9]*\)" height="\([0-9]*\)".*/\1 \2/p' "$1" | head -1)
  if [[ -z $wh ]]; then
    echo "render: $1 has no integer width/height attributes" >&2
    exit 1
  fi
  echo "$wh"
}

# render <source.svg> <out.png> <ground|none> [scale] [force_width]
# force_width is for platform-fixed sizes (favicon 32px); everything else uses the SVG.
render() {
  local src="$1" out="$2" ground="$3" scale="${4:-1}" force_w="${5:-}"
  local w h
  read -r w h < <(svg_size "$src")
  [[ -n $force_w ]] && w=$force_w
  h=$(( w * h / $(svg_size "$src" | cut -d' ' -f1) ))

  local bg="00000000"
  # The ground is named (DARK/LIGHT) at the call site and resolved here, so a caller cannot
  # pass a colour Chromium will reject — it validated the literal "DARK" as hex.
  if [[ $ground != none ]]; then
    bg="${!ground}"
    [[ $bg =~ ^[0-9A-Fa-f]{6}$ ]] || { echo "render: unknown ground '$ground'" >&2; return 1; }
  fi

  # Chromium resolves --screenshot against its own notion of the working directory, which
  # is not ours once --user-data-dir is set, so every path handed to it is absolute.
  local abs_out="$out"
  [[ $abs_out != /* ]] && abs_out="$PWD/$out"

  # Chromium clamps a short viewport: it draws only the top ~30px of the page while still
  # emitting a canvas of the requested size, which silently crops a 116px-tall lockup. So
  # always give it a viewport large enough to lay the document out at 1:1 from the top-left
  # corner (it does not centre a standalone SVG), then crop the canvas to exactly the region
  # the asset occupies. Deterministic, and independent of the minimum this build enforces.
  local win_w=$(( w > 400 ? w : 400 )) win_h=$(( h > 400 ? h : 400 ))

  # A unique profile per run: concurrent headless Chromium invocations share a default
  # profile and fail with a lock error, and each one leaves ~90MB under /tmp.
  local profile
  profile="$(mktemp -d "$WORK/profile.XXXXXX")"
  if ! "$CHROME" --headless --disable-gpu --no-sandbox --hide-scrollbars \
      --user-data-dir="$profile" \
      --force-device-scale-factor="$scale" --window-size="$win_w,$win_h" \
      --default-background-color="$bg" \
      --screenshot="$abs_out" "file://$PWD/$src" >"$WORK/chrome.log" 2>&1; then
    echo "render: FAILED $src" >&2
    tail -5 "$WORK/chrome.log" >&2
    rm -rf "$profile"
    return 1
  fi
  rm -rf "$profile"
  if [[ ! -s $abs_out ]]; then
    echo "render: $src produced no output" >&2
    tail -5 "$WORK/chrome.log" >&2
    return 1
  fi

  python3 - "$abs_out" "$(( w * scale ))" "$(( h * scale ))" <<'PY'
import sys
from PIL import Image
path, cw, ch = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
im = Image.open(path)
if im.size != (cw, ch):
    im.crop((0, 0, cw, ch)).save(path)
PY
  printf '  %-48s %sx%s @%sx\n' "${out#"$PNG"/}" "$w" "$h" "$scale"
}

# want <group> [selectors...] — true when nothing was named, or the group was.
want() {
  local key="$1"
  shift
  [[ $# -eq 0 ]] || [[ " $* " == *" $key "* ]]
}
sel=("$@")

# --- marks: transparent, they sit on whatever ground the reader uses ----------
if want mark "${sel[@]}"; then
  render "$SRC/logo-mark.svg"            "$PNG/logo-mark-1024.png"            none 2
  render "$SRC/logo-mark.svg"            "$PNG/logo-mark-512.png"             none 1
  render "$SRC/logo-mark-mono-black.svg" "$PNG/logo-mark-mono-black-512.png"  none 1
  render "$SRC/logo-mark-mono-white.svg" "$PNG/logo-mark-mono-white-512.png"  DARK 1
fi

# --- lockups: each previewed on the ground it is drawn for -------------------
if want logo "${sel[@]}"; then
  render "$SRC/logo-badge.svg"                   "$PNG/logo-badge-512.png"                  DARK  1
  render "$SRC/logo-lockup-horizontal-dark.svg"  "$PNG/logo-lockup-horizontal-dark.png"     DARK  2
  render "$SRC/logo-lockup-horizontal-light.svg" "$PNG/logo-lockup-horizontal-light.png"    LIGHT 2
  render "$SRC/logo-lockup-stacked-dark.svg"     "$PNG/logo-lockup-stacked-dark.png"        DARK  2
  render "$SRC/logo-lockup-stacked-light.svg"    "$PNG/logo-lockup-stacked-light.png"       LIGHT 2
fi

# --- app icons: sizes fixed by platform convention --------------------------
if want icons "${sel[@]}"; then
  render "$SRC/logo-badge.svg" "$PNG/app-icon-512.png"         DARK 1
  render "$SRC/logo-badge.svg" "$PNG/app-icon-192.png"         DARK 1 192
  render "$SRC/logo-badge.svg" "$PNG/apple-touch-icon-180.png" DARK 1 180
  render "$SRC/logo-badge.svg" "$PNG/favicon-48.png"           DARK 1 48
  render "$SRC/logo-badge.svg" "$PNG/favicon-32.png"           DARK 1 32
fi

# --- banners and OG: exact published dimensions -----------------------------
if want banner "${sel[@]}"; then
  render "$SRC/banner-readme.svg" "$PNG/banner-readme.png" DARK 1
  render "$SRC/banner-github.svg" "$PNG/banner-github.png" DARK 1
fi

if want og "${sel[@]}"; then
  render "$SRC/og-default.svg"      "$PNG/og-default.png"      DARK 1
  render "$SRC/og-architecture.svg" "$PNG/og-architecture.png" DARK 1
fi

echo "png exports in $PNG"
