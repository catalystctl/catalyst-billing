// The tenant's brand, as the panel needs it.
//
// This module is deliberately free of DOM access so it can be exercised in Node: the two
// decisions it makes — whether to emit an accent override at all, and what to write when it
// does — are the decisions that can break the light and dark themes, and they are worth a
// test that does not need a browser.
//
// # What it is, and what it is not
//
// It is one brand per installation — a name, a logo and one accent per colour mode — layered
// on top of the two themes `lib/theme.ts` already switches. It is not a theme registry: the
// light/dark pair stays exactly as it is, and a brand adds three custom properties to
// whichever theme is active. A registry (many named themes, each a full token set, selectable
// per user) is a different, much larger feature and is not what this module does.
//
// # The rule that keeps the themes intact
//
// A brand override is written to `document.documentElement.style`, i.e. as an inline style on
// <html>, and it only ever writes the accent properties for the *current* mode. Two
// consequences follow, and both are the point:
//
//   - **A brand cannot break light or dark**, because it does not touch `--bg`, `--surface`,
//     `--text` or any other token. Switching modes re-resolves the whole token set from
//     `tokens.css`; the only thing a brand contributes is a value for `--accent` and the
//     three properties derived from it.
//   - **An unset brand writes nothing at all.** No inline property, no stylesheet, not one
//     attribute. That is what makes the fallback byte-identical rather than "a brand that
//     happens to equal the default": there is nothing to differ.
//
// # Why the accent is per mode
//
// The built-in accent is #3b5bdb on white and #7b95ff on near-black: the same intent at two
// luminances, because a mid-tone blue that reads on white is too dark on #0a0d13 and the pale
// version of it is invisible on white. One stored accent would be legible in one theme and
// washed out in the other, so a brand carries one per mode and either may be empty.

import { ApiError, type Api } from "./api";

/** The brand as `GET /api/v1/branding` returns it. */
export interface Brand {
  /** The tenant's display name, or "" when not configured. */
  name: string;
  /** Light-mode accent as `#rrggbb`, or "". */
  accent_light: string;
  /** Dark-mode accent as `#rrggbb`, or "". */
  accent_dark: string;
  /** The stored image id, or "". */
  logo_id: string;
  /** `/api/v1/media/{id}`, or "". */
  logo_url: string;
  /** When the group was last written, or null. */
  updated_at?: string | null;
  /** The derived colour facts, computed by the server with the same arithmetic the invoice
   *  renderer uses. Optional so a client that predates it still type-checks. */
  resolved?: {
    light: ModeDerived;
    dark: ModeDerived;
  };
}

/** One mode's derived colour facts. */
export interface ModeDerived {
  /** The colour text takes on top of the accent. */
  ink: string;
  /** The accent adjusted until it reads on this mode's background. Empty when unset. */
  text: string;
  /** The raw accent's WCAG contrast ratio against this mode's background. */
  contrast: number;
  /** Whether that ratio reaches 4.5:1. */
  legible: boolean;
  /** Whether an accent is configured for this mode. */
  set: boolean;
}

/** What the panel paints. `null` when nothing is configured. */
export interface BrandPaint {
  /** The accent to write, as `#rrggbb`. */
  accent: string;
  /** The colour text takes on top of it. */
  ink: string;
  /** The accent adjusted to read on the current background, for small text and links. */
  text: string;
  /** The accent with an alpha suffix, for tinted surfaces. */
  soft: string;
  /** The stronger tint, for a selected row or an active chip. */
  softStrong: string;
}

/** The empty brand: nothing configured, which must paint nothing. */
export const NO_BRAND: Brand = {
  name: "",
  accent_light: "",
  accent_dark: "",
  logo_id: "",
  logo_url: "",
  updated_at: null,
};

/**
 * The custom properties a brand may write.
 *
 * They are the accent family only. A brand that could set `--bg` or `--text` would be a theme
 * registry by another name, and the first tenant to pick a dark surface with dark text would
 * have a panel they cannot read — in one mode only, which is the worst kind of bug report.
 */
export const BRAND_PROPERTIES = [
  "--accent",
  "--accent-ink",
  "--accent-text",
  "--accent-soft",
  "--accent-soft-strong",
] as const;

/** True when nothing about this brand is configured. */
export function isBrandEmpty(brand: Brand | null | undefined): boolean {
  if (!brand) return true;
  return !brand.name && !brand.accent_light && !brand.accent_dark && !brand.logo_id;
}

/** The accent configured for a mode, or "" when that mode has none. */
export function accentFor(brand: Brand | null | undefined, mode: "light" | "dark"): string {
  if (!brand) return "";
  return (mode === "dark" ? brand.accent_dark : brand.accent_light) || "";
}

/**
 * The derived facts for a mode.
 *
 * When the server sent them they are used as they are — the panel and the invoice must agree
 * about whether a colour is legible, and two implementations of WCAG arithmetic are two
 * answers. The local fallback exists only so a build talking to an older server still paints
 * something sensible.
 */
export function derivedFor(brand: Brand | null | undefined, mode: "light" | "dark"): ModeDerived {
  const accent = accentFor(brand, mode);
  if (!accent) {
    return { ink: "", text: "", contrast: 0, legible: true, set: false };
  }
  const sent = brand?.resolved?.[mode];
  if (sent && sent.set) return sent;
  const rgb = parseHex(accent);
  const bg = mode === "dark" ? "#0a0d13" : "#f5f6f8";
  const ratio = rgb ? contrast(rgb, parseHex(bg)!) : 0;
  return {
    ink: rgb && contrast(rgb, { r: 255, g: 255, b: 255 }) >= contrast(rgb, { r: 10, g: 13, b: 19 }) ? "#ffffff" : "#0a0d13",
    text: rgb ? readableHex(rgb, parseHex(bg)!) : accent,
    contrast: Math.round(ratio * 100) / 100,
    legible: ratio >= 4.5,
    set: true,
  };
}

/**
 * What to paint for a brand in one mode, or null when there is nothing to paint.
 *
 * This is the function the "a theme cannot break light or dark" property rests on: it returns
 * null unless an accent is configured for the mode being rendered, so an unbranded
 * installation — and a brand that only set the dark accent, while the light theme is active —
 * writes nothing and the stylesheet's own tokens stand.
 */
export function paintFor(brand: Brand | null | undefined, mode: "light" | "dark"): BrandPaint | null {
  const accent = accentFor(brand, mode);
  if (!accent) return null;
  const rgb = parseHex(accent);
  if (!rgb) return null;
  const derived = derivedFor(brand, mode);
  const ink = derived.ink || "#0a0d13";
  return {
    accent: hexOf(rgb),
    ink,
    // The adjusted colour is what small text uses. It is empty only if the server sent
    // nothing and the fallback could not compute one, in which case the raw accent is the
    // best available answer.
    text: derived.text || hexOf(rgb),
    soft: `${hexOf(rgb)}24`,
    softStrong: `${hexOf(rgb)}3d`,
  };
}

/**
 * Applies a brand to the document root.
 *
 * `mode` is the theme that is active. Properties from the *other* mode are removed, so a
 * brand that configured only the light accent leaves no trace when the panel is dark — which
 * is what makes "the light and dark themes still work" a fact rather than a hope.
 *
 * It returns the properties it wrote, so a caller (and a test) can see exactly what changed.
 */
export function applyBrand(
  brand: Brand | null | undefined,
  mode: "light" | "dark",
  root: HTMLElement = document.documentElement,
): string[] {
  const paint = paintFor(brand, mode);
  // Clear first, always: switching from a branded installation to an unbranded one, or from
  // light to dark with only a light accent configured, has to remove the previous override.
  for (const property of BRAND_PROPERTIES) root.style.removeProperty(property);
  if (!paint) return [];

  const written: Array<[string, string]> = [
    ["--accent", paint.accent],
    ["--accent-ink", paint.ink],
    ["--accent-text", paint.text],
    ["--accent-soft", paint.soft],
    ["--accent-soft-strong", paint.softStrong],
  ];
  for (const [property, value] of written) root.style.setProperty(property, value);
  return written.map(([property]) => property);
}

/** Removes every brand property from the root. */
export function clearBrand(root: HTMLElement = document.documentElement): void {
  for (const property of BRAND_PROPERTIES) root.style.removeProperty(property);
}

/**
 * Reads the brand from the server.
 *
 * A failure is not a reason to fail to render: an installation whose branding endpoint is
 * unreachable is a panel in its built-in colours, which is exactly what an installation that
 * never configured a brand looks like. The call never throws.
 */
export async function loadBrand(api: Api, signal?: AbortSignal): Promise<Brand> {
  try {
    return await api.get<Brand>("/api/v1/branding", { cache: false, signal });
  } catch (err) {
    // A 404 means this build has no branding endpoint; anything else means it could not be
    // read right now. Both fall back, and both are logged once so the reason is visible.
    if (!(err instanceof ApiError) || err.status !== 404) {
      console.warn("branding: falling back to the built-in appearance", err);
    }
    return NO_BRAND;
  }
}

// ------------------------------------------------------------------- colour --

/** An sRGB triple, 0-255 per channel. */
export interface Rgb {
  r: number;
  g: number;
  b: number;
}

/** Parses `#rgb` or `#rrggbb` (the `#` optional). Returns null for anything else. */
export function parseHex(input: string): Rgb | null {
  const raw = (input || "").trim().replace(/^#/, "");
  if (raw.length === 3) {
    const [r, g, b] = [raw[0], raw[1], raw[2]].map((c) => parseInt(c + c, 16));
    if ([r, g, b].some((v) => Number.isNaN(v))) return null;
    return { r, g, b };
  }
  if (raw.length === 6) {
    const r = parseInt(raw.slice(0, 2), 16);
    const g = parseInt(raw.slice(2, 4), 16);
    const b = parseInt(raw.slice(4, 6), 16);
    if ([r, g, b].some((v) => Number.isNaN(v))) return null;
    return { r, g, b };
  }
  return null;
}

/** Renders a triple as `#rrggbb`. */
export function hexOf(c: Rgb): string {
  const part = (v: number) => Math.max(0, Math.min(255, Math.round(v))).toString(16).padStart(2, "0");
  return `#${part(c.r)}${part(c.g)}${part(c.b)}`;
}

function linear(channel: number): number {
  const c = channel / 255;
  return c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4);
}

/** WCAG 2.1 relative luminance. */
export function luminance(c: Rgb): number {
  return 0.2126 * linear(c.r) + 0.7152 * linear(c.g) + 0.0722 * linear(c.b);
}

/** WCAG contrast ratio, 1 to 21. */
export function contrast(a: Rgb, b: Rgb): number {
  const la = luminance(a);
  const lb = luminance(b);
  const [hi, lo] = la < lb ? [lb, la] : [la, lb];
  return (hi + 0.05) / (lo + 0.05);
}

/** Blends `a` towards `b` by `t` (0..1). */
export function mix(a: Rgb, b: Rgb, t: number): Rgb {
  const lerp = (x: number, y: number) => x + (y - x) * t;
  return { r: lerp(a.r, b.r), g: lerp(a.g, b.g), b: lerp(a.b, b.b) };
}

/**
 * Adjusts `c` towards white or black until it reaches AA against `bg`.
 *
 * The mirror of `brand.Readable` in Go. It is duplicated rather than served because the panel
 * needs it synchronously, before the first paint, for the case where the server's derivation
 * is absent — and because the two are pinned by tests against the same cases.
 */
export function readableHex(c: Rgb, bg: Rgb, min = 4.5): string {
  if (contrast(c, bg) >= min) return hexOf(c);
  const target: Rgb = luminance(bg) < 0.5 ? { r: 255, g: 255, b: 255 } : { r: 0, g: 0, b: 0 };
  let best = c;
  let bestRatio = contrast(c, bg);
  for (let step = 1; step <= 20; step++) {
    const candidate = mix(c, target, step * 0.05);
    const ratio = contrast(candidate, bg);
    if (ratio > bestRatio) {
      best = candidate;
      bestRatio = ratio;
      if (ratio >= min) break;
    }
  }
  return hexOf(best);
}
