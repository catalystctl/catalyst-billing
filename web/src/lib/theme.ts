// Theme.
//
// Applied on <html data-theme> so every token switches at once and no component needs to
// know which theme it is in. The stored choice wins over everything else; a first visit gets
// the published theme's own default mode, and only a document that carries no mode at all
// falls back to the OS. The attribute is set before the first paint (main.ts runs as a module
// and the shell's background is on <body>), so there is no flash of the wrong theme.
//
// # The three answers, in order
//
// Since the theme registry landed there are three sources for "which mode am I in", and the
// order between them is the whole contract:
//
//   1. **The operator's own choice** — `localStorage["catalyst.theme"]`. It is the most recent
//      deliberate action on this device, and it is the key the shell's pre-paint boot script
//      reads, so a returning user never sees the theme's default mode flash for a frame.
//   2. **The mode the shell already resolved** — the `data-theme` the server wrote into the
//      document, and the value the boot script's own script confirms before the first paint. It
//      is the published theme's default mode, and it is already on screen: reading it is how
//      this module agrees with the server instead of overruling it and repainting.
//   3. **The OS preference** — the fallback for a document that carries no mode at all, which is
//      what a page rendered without the theme shell looks like.
//
// The boot script the shell injects implements the same order in one file (see
// `internal/theme/css/generate.go`), which is why the stored key is byte-identical here: two
// readers of one key must not disagree about its name.
//
// # Why the manifest is read here rather than in the shell
//
// The manifest also carries the *shape* of the shell — `data-shell`, `data-density`,
// `data-card-variant`, `data-table-variant` — which the server writes into the shell document
// when it renders it. This module reads the same manifest over the API and writes the same
// four attributes, so a page that was not rendered by that shell (a stale build, a dev server,
// a navigation that changed scope) still lands on the published shape instead of the built-in
// one. It is a fallback for a shell that could not do it, not a second source of truth: when
// the server already wrote the attribute, this writes the same value.
//
// The read is kicked off by `initTheme` and never awaited. The first paint must not wait on a
// network round trip to know its background, and a manifest that fails to load is an
// installation whose shell already told us everything it needs.

import { Api } from "./api";

export type Theme = "dark" | "light";

/** The mode a theme asks for: one of the two, or "follow the OS". */
export type ThemeMode = Theme | "system";

const KEY = "catalyst.theme";

/**
 * The parts of the public manifest this module reads.
 *
 * It is a structural subset rather than an import of the studio's document type: this module
 * runs before the shell exists and is on the critical path for the first paint, so it must not
 * pull the editor, the component kit or the dialog code into the boot chunk.
 */
export interface ThemeManifest {
  scope?: string;
  revision?: string;
  digest?: string;
  /** False when nothing is published and the manifest is the built-in default. */
  published?: boolean;
  css?: string;
  boot?: string;
  document?: ManifestDocument;
  meta?: ManifestMeta;
}

interface ManifestDocument {
  color?: { mode?: string };
  space?: { density?: string };
  layout?: { shell?: { nav?: string; cardStyle?: string; tableStyle?: string } };
}

interface ManifestMeta {
  title?: string;
  theme_color?: string;
  color_scheme?: string;
  nav?: string;
  density?: string;
  card_style?: string;
  table_style?: string;
}

/** The mode stored in this browser, or null when the operator has never chosen one. */
function storedTheme(): Theme | null {
  try {
    const stored = localStorage.getItem(KEY);
    if (stored === "dark" || stored === "light") return stored;
  } catch {
    // A blocked storage API is not a reason to fail to render.
  }
  return null;
}

/** The OS preference, which is the last resort. */
function osTheme(): Theme {
  try {
    return window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
  } catch {
    // A browser without matchMedia gets the product's own default rather than a throw.
    return "dark";
  }
}

/**
 * The mode the document already carries.
 *
 * The server writes the published theme's default into `<html data-theme>` and the boot script
 * confirms it before the first paint. Reading it is what keeps this module from overruling a
 * decision that is already on screen — the failure that produced a visible repaint on every
 * first visit to a dark-first installation.
 */
function shellTheme(): Theme | null {
  const value = document.documentElement.dataset.theme;
  return value === "dark" || value === "light" ? value : null;
}

export function initTheme(): Theme {
  const theme: Theme = storedTheme() ?? shellTheme() ?? osTheme();
  document.documentElement.dataset.theme = theme;
  // Deliberately not awaited: see the header. `hydrate` swallows its own failures, so a build
  // whose theme endpoint does not exist yet boots exactly as it did before.
  void hydrateThemeManifest();
  return theme;
}

export function applyTheme(theme: Theme): void {
  document.documentElement.dataset.theme = theme;
  try {
    localStorage.setItem(KEY, theme);
  } catch {
    /* ignore */
  }
}

export function toggleTheme(current: Theme): Theme {
  const next: Theme = current === "dark" ? "light" : "dark";
  applyTheme(next);
  return next;
}

/** The manifest's default mode, or null when it does not name one. */
export function manifestMode(manifest: ThemeManifest | null | undefined): ThemeMode | null {
  const raw = (manifest?.meta?.color_scheme || manifest?.document?.color?.mode || "").trim();
  if (raw === "dark" || raw === "light" || raw === "system") return raw;
  return null;
}

/**
 * Applies a manifest to the document root.
 *
 * The mode is written only when this browser has no stored choice: a manifest is a default, and
 * a default that overwrites a person's choice is the bug the stored key exists to prevent. The
 * four shape attributes are always written, because they are not a preference — they are what
 * the published theme looks like, and a page that had none is on the built-in shape.
 *
 * It returns the attributes it wrote, so a caller (and a test) can see exactly what changed.
 */
export function applyThemeManifest(
  manifest: ThemeManifest | null | undefined,
  root: HTMLElement = document.documentElement,
): string[] {
  if (!manifest) return [];
  const written: string[] = [];

  const mode = manifestMode(manifest);
  if (!storedTheme() && mode && manifest.published !== false) {
    const next: Theme = mode === "system" ? osTheme() : mode;
    if (root.dataset.theme !== next) {
      root.dataset.theme = next;
      written.push("data-theme");
    }
  }

  const shell = manifest.document?.layout?.shell ?? {};
  const shape: Array<[string, string | undefined]> = [
    ["shell", manifest.meta?.nav || shell.nav],
    ["density", manifest.meta?.density || manifest.document?.space?.density],
    ["cardVariant", manifest.meta?.card_style || shell.cardStyle],
    ["tableVariant", manifest.meta?.table_style || shell.tableStyle],
  ];
  for (const [name, value] of shape) {
    const clean = (value ?? "").trim();
    if (clean) {
      if (root.dataset[name] !== clean) {
        root.dataset[name] = clean;
        written.push(`data-${name.replace(/[A-Z]/g, (c) => `-${c.toLowerCase()}`)}`);
      }
    } else if (root.dataset[name] !== undefined) {
      // No value means "the stylesheet's own default", which is what the built-in theme is. An
      // attribute left behind from a previous manifest would pin the page to a shape nothing
      // published any more.
      delete root.dataset[name];
      written.push(`data-${name.replace(/[A-Z]/g, (c) => `-${c.toLowerCase()}`)}`);
    }
  }
  return written;
}

/**
 * Reads the public manifest.
 *
 * It never throws: a manifest that cannot be read is an installation whose appearance is already
 * on screen, and a boot that failed because a colour could not be fetched would be a worse
 * outcome than a colour that is one publish out of date.
 */
export async function loadThemeManifest(api: Api, signal?: AbortSignal): Promise<ThemeManifest | null> {
  try {
    return await api.get<ThemeManifest>("/api/v1/theme", { cache: false, signal });
  } catch (err) {
    console.warn("theme: the manifest could not be read; keeping the shell's own appearance", err);
    return null;
  }
}

/**
 * Fetches the manifest and applies it.
 *
 * It builds its own client because it runs from `initTheme`, which the shell calls before it
 * has constructed the one it hands to pages. A GET of a public manifest needs none of what that
 * client carries — no CSRF token, no idempotency key, no session-expiry handling — so the
 * second instance is a short-lived reader, not a second HTTP client.
 */
async function hydrateThemeManifest(): Promise<void> {
  const manifest = await loadThemeManifest(new Api());
  if (manifest) applyThemeManifest(manifest);
}
