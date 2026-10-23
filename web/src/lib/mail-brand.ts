// The mail-brand overlay, and the accent presets both surfaces share.
//
// # Why this is a separate module
//
// Two decisions here are testable without a browser, and both are the ones that go wrong quietly:
// which of the five `mail.brand_*` settings mean what (with the defaults the Go registry declares),
// and what a preset is allowed to touch. The screen is the easy part; this is the part that has to
// agree with `internal/settings/group_mail_brand.go`.
//
// # What a preset may change
//
// A preset is a *pair of accents*, not a token set. The panel's tokens are a fixed vocabulary and
// the brand only ever contributes the accent family (see lib/brand.ts), so a preset that set a
// surface or a text colour would be a theme registry by another name — and the first tenant to
// pick a dark surface with dark text would have a panel they cannot read in one mode only. Naming
// a pair is the useful middle: an operator gets a designed combination in one click, and the rule
// that keeps the themes intact is untouched.
//
// Presets are shared with the mail accent, because a shop that picked "Slate & lime" for its panel
// should not have to guess the hex again for its invoices.

/** The mail-brand settings as the settings API returns them, keyed by the bare key. */
export interface MailBrandSettings {
  /** Generate the branded HTML part at all. */
  html: boolean;
  /** The line above the body. Empty means the company name. */
  header: string;
  /** The small print below the body. Empty means nothing but the company name. */
  footer: string;
  /** `#rrggbb`, or "" to fall back to the panel's light accent. */
  accent: string;
  /** An absolute http(s) URL, or "" for no logo in the mail. */
  logo_url: string;
}

/** What a fresh installation sends: branding on, the company name, no logo, the panel accent. */
export const MAIL_BRAND_DEFAULTS: MailBrandSettings = {
  html: true,
  header: "",
  footer: "",
  accent: "",
  logo_url: "",
};

/** The setting keys, so the screen and the tests cannot spell one differently. */
export const MAIL_BRAND_KEYS = {
  html: "brand_html",
  header: "brand_header",
  footer: "brand_footer",
  accent: "brand_accent",
  logo_url: "brand_logo_url",
} as const;

/** One named accent pair an operator can apply in a click. */
export interface AccentPreset {
  name: string;
  /** The light-mode accent, `#rrggbb`. */
  light: string;
  /** The dark-mode accent, `#rrggbb`. A mid-tone that reads on white is too dark on near-black. */
  dark: string;
}

/**
 * The preset catalogue.
 *
 * Four pairs, chosen so each is legible in both modes rather than pretty in one: the light value is
 * dark enough for white, and the dark value is pale enough for near-black. A preset that failed
 * that would be a preset that ships an unreadable link, so the legibility warning on the screen
 * would fire for a combination the product itself suggested.
 */
export const ACCENT_PRESETS: readonly AccentPreset[] = [
  { name: "Indigo", light: "#3b5bdb", dark: "#7b95ff" },
  { name: "Slate & lime", light: "#3f6212", dark: "#a3e635" },
  { name: "Teal", light: "#0f766e", dark: "#5eead4" },
  { name: "Plum", light: "#7e22ce", dark: "#d8b4fe" },
  { name: "Amber", light: "#b45309", dark: "#fbbf24" },
  { name: "Ink", light: "#1f2937", dark: "#cbd5e1" },
];

/** The accent pair for a preset, lower-cased so a comparison against a stored value is exact. */
export function presetAccents(preset: AccentPreset): { light: string; dark: string } {
  return { light: preset.light.toLowerCase(), dark: preset.dark.toLowerCase() };
}

/** True when the stored pair is exactly this preset, which is what marks it as active. */
export function presetMatches(
  preset: AccentPreset,
  brand: { accent_light: string; accent_dark: string },
): boolean {
  const accents = presetAccents(preset);
  return (
    (brand.accent_light || "").toLowerCase() === accents.light &&
    (brand.accent_dark || "").toLowerCase() === accents.dark
  );
}

/** A settings key, as one group read returns it. */
export interface SettingsKey {
  key: string;
  value: string;
  kind?: string;
  is_secret?: boolean;
}

/** Reads the mail brand out of one group read, applying the registry's defaults. */
export function mailBrandFromSettings(keys: readonly SettingsKey[] | null | undefined): MailBrandSettings {
  const byKey = new Map<string, string>();
  for (const key of keys ?? []) {
    if (key && typeof key.key === "string") byKey.set(key.key, typeof key.value === "string" ? key.value : "");
  }
  const bool = (raw: string | undefined, fallback: boolean): boolean => {
    const value = (raw ?? "").trim().toLowerCase();
    if (["true", "t", "1", "yes", "on"].includes(value)) return true;
    if (["false", "f", "0", "no", "off"].includes(value)) return false;
    return fallback;
  };
  return {
    html: bool(byKey.get(MAIL_BRAND_KEYS.html), MAIL_BRAND_DEFAULTS.html),
    header: (byKey.get(MAIL_BRAND_KEYS.header) ?? MAIL_BRAND_DEFAULTS.header).trim(),
    footer: (byKey.get(MAIL_BRAND_KEYS.footer) ?? MAIL_BRAND_DEFAULTS.footer).trim(),
    accent: (byKey.get(MAIL_BRAND_KEYS.accent) ?? MAIL_BRAND_DEFAULTS.accent).trim(),
    logo_url: (byKey.get(MAIL_BRAND_KEYS.logo_url) ?? MAIL_BRAND_DEFAULTS.logo_url).trim(),
  };
}

/**
 * The values to send back, as strings.
 *
 * The server validates; this only shapes a boolean back into the text the settings table stores, so
 * the screen does not have to remember that every settings value is a string.
 */
export function mailBrandToSettings(brand: MailBrandSettings): Record<string, string> {
  return {
    [MAIL_BRAND_KEYS.html]: brand.html ? "true" : "false",
    [MAIL_BRAND_KEYS.header]: brand.header.trim(),
    [MAIL_BRAND_KEYS.footer]: brand.footer.trim(),
    [MAIL_BRAND_KEYS.accent]: brand.accent.trim().toLowerCase(),
    [MAIL_BRAND_KEYS.logo_url]: brand.logo_url.trim(),
  };
}

/**
 * A mail accent that is safe to write into an inline style.
 *
 * The server refuses anything that is not a hex colour, and the renderer normalises again; this is
 * the client's third gate, because a value that reached `style=` some other way (a restored backup,
 * a hand-edited row) must not be able to inject a declaration into a preview.
 */
export function safeAccent(raw: string): string {
  return /^#[0-9a-f]{6}$/i.test((raw || "").trim()) ? raw.trim().toLowerCase() : "";
}
