// The Theme Studio's shared half.
//
// Seven screens edit one thing. A revision is a single JSON document, and the operator moves
// between the colour screen, the type screen and the shell screen while it is open — so the
// document has to live somewhere that outlives one screen's module, and every screen has to
// agree about which scope it is editing, what the server last stored, and whether the values
// on screen are still unsaved. That is what this module is: one draft, one scope, one set of
// fetch helpers, and the three renderers all seven screens share (the sub-nav, the scope
// picker and the diff table).
//
// # Why the draft is not re-read on every screen
//
// A screen mounts, fetches `GET /api/v1/staff/theme/{scope}`, and *adopts* the answer — except
// when the operator has unsaved edits, in which case the local document wins and the screen
// shows what they typed. Re-reading on mount is what keeps two tabs from silently diverging
// when nothing is dirty; keeping a dirty draft is what stops "I clicked Typography and lost my
// colour" — a navigation, not a save, is not a decision to discard work.
//
// # Why the contrast arithmetic is here and not on the server
//
// The server validates contrast before publishing (docs/theming-system.md §7.3) and its answer
// is authoritative. But the meter on the colour screen has to update on every keystroke, and a
// round trip per character is not a meter. So the WCAG formula is implemented here as well —
// the same one `internal/brand` uses, with the same reference points asserted in the tests, so
// a disagreement between the two would be a red test rather than a colour that passes on screen
// and fails at publish.
//
// # What is deliberately not here
//
// Storefront page composition (`layout.pages`) and asset upload. The first is a later phase the
// server refuses today, and the second needs an editor this studio does not have yet; a screen
// that rendered either would be a control that cannot work.

import { ApiError, type Api } from "./api";
import { h, type Child } from "./dom";
import { dateTime, relative, type Tone } from "./format";
import { icon } from "./icons";
import { problemFieldText, problemFrom, problemText, tx } from "./i18n";
import type { Ctx } from "./types";
import { badge, banner, btn, card, pageHead, select } from "../ui/kit";
import { openDialog } from "../ui/dialog";

// ------------------------------------------------------------------ scopes --

/** The five surfaces a theme can be published for. The order is the order the studio lists them. */
export type ScopeName = "panel" | "portal" | "storefront" | "email" | "invoice";

export interface ScopeInfo {
  scope: ScopeName;
  /** The server's own word for the scope (`store.ScopeDisplay`), used when a response omits it. */
  label: string;
  /** One sentence: what the scope paints, and what it inherits. */
  description: string;
}

/**
 * The scopes, with the sentence that says what each one is.
 *
 * The inheritance is stated rather than implied because it decides what an operator is looking
 * at: a portal theme that states only its accent still shows the panel's palette, and a screen
 * that did not say so would make every inherited value look like a value nobody chose.
 */
export const SCOPES: readonly ScopeInfo[] = [
  {
    scope: "panel",
    label: "Staff panel",
    description: "The signed-in operator screens. Every other scope starts from this one.",
  },
  {
    scope: "portal",
    label: "Customer portal",
    description: "The customer's account, invoices and services. Inherits the panel until it overrides something.",
  },
  {
    scope: "storefront",
    label: "Storefront",
    description: "The public catalogue, cart, checkout and sign-in pages. Inherits the panel.",
  },
  {
    scope: "email",
    label: "Transactional email",
    description: "The HTML part of every message. Mail-safe presentation only; `mail.brand_*` stays authoritative.",
  },
  {
    scope: "invoice",
    label: "Invoice PDF",
    description: "Printed invoices and credit notes. PDF-safe presentation only; the invoice template stays authoritative.",
  },
];

/** True when `value` names a scope this build knows. */
export function isScopeName(value: string | null | undefined): value is ScopeName {
  return SCOPES.some((entry) => entry.scope === value);
}

/** The scope's display name, from the server's answer when it sent one. */
export function scopeLabel(scope: string, fromServer?: string | null): string {
  if (fromServer) return fromServer;
  return SCOPES.find((entry) => entry.scope === scope)?.label ?? scope;
}

/** The scope's one-line description. */
export function scopeDescription(scope: string): string {
  return SCOPES.find((entry) => entry.scope === scope)?.description ?? "";
}

/**
 * The scope the studio is editing.
 *
 * A query parameter wins over the remembered value so a link can open a specific scope — the
 * overview's "open the storefront theme" row uses that — and the remembered value means moving
 * from Colours to Typography does not silently send the operator back to the panel.
 */
let currentScope: ScopeName = "panel";

export function studioScope(ctx?: Ctx): ScopeName {
  const requested = ctx?.route.query.get("scope") ?? null;
  if (isScopeName(requested)) currentScope = requested;
  return currentScope;
}

export function setStudioScope(next: ScopeName): void {
  currentScope = next;
}

// ------------------------------------------------------------- the payloads --

/** One immutable revision, as `store.Revision` marshals it. */
export interface Revision {
  id: string;
  theme_id: string;
  scope: string;
  slug: string;
  name: string;
  number: number;
  status: "draft" | "published" | "archived";
  digest: string;
  note: string;
  created_at: string;
  published_at: string | null;
  created_by: string | null;
  published_by: string | null;
  /** The resolved document: what a preview renders and what the generator reads. */
  document: ThemeDocument;
  /** What the operator wrote. A layer: fields it omits inherit. */
  source: ThemeDocument;
}

/** One publish, as `store.Publication` marshals it. */
export interface Publication {
  id: string;
  scope: string;
  theme_id: string;
  revision_id: string;
  revision_number: number;
  previous_revision_id: string | null;
  published_at: string;
  published_by: string | null;
  note: string;
}

/** One catalogue entry, as `presets.Meta` marshals it. */
export interface PresetMeta {
  id: string;
  name: string;
  description: string;
  /** The one line that says what makes this preset structurally different, not what colour it is. */
  character: string;
  mode: string;
  radius: string;
  density: string;
  nav: string;
  /** Ground, card, text, accent — the four colours the gallery card paints. */
  swatch: string[];
}

/** The closed sets the server accepts, so a screen can render a select instead of failing on save. */
export interface Limits {
  max_document_bytes: number;
  max_custom_css_bytes: number;
  max_name_length: number;
  modes: string[];
  densities: string[];
  navs: string[];
  card_styles: string[];
  table_styles: string[];
  backgrounds: string[];
  components: string[];
  families: string[];
  scopes?: string[];
  asset_max_bytes?: number;
  preview_ttl_seconds?: number;
}

/** What the caller may do, so the studio hides an action rather than letting it fail. */
export interface Permissions {
  can_read?: boolean;
  can_write?: boolean;
  can_publish?: boolean;
}

/** One entry of `GET /api/v1/staff/theme`. */
export interface ScopeSummary {
  scope: string;
  label?: string;
  published?: Revision | null;
  draft?: Revision | null;
}

/** `GET /api/v1/staff/theme`. */
export interface OverviewResponse {
  scopes: ScopeSummary[];
  presets: PresetMeta[];
  limits: Limits;
  permissions: Permissions;
  /** False when the embedded preset catalogue failed to load — a broken build, not a bad request. */
  presets_ok?: boolean;
}

/** `GET /api/v1/staff/theme/{scope}`. */
export interface ScopeResponse {
  scope: string;
  label?: string;
  published?: Revision | null;
  drafts: Revision[];
  history: Publication[];
  document: ThemeDocument;
  source: ThemeDocument;
  presets: PresetMeta[];
  limits: Limits;
  permissions: Permissions;
}

/** `GET /api/v1/staff/theme/revisions/{id}/diff`. */
export interface DiffResponse {
  revision_id: string;
  changes: DiffChange[];
  summary?: string;
}

/** One field-level difference. `from` and `to` are JSON text, empty when the field is absent. */
export interface DiffChange {
  path: string;
  from: string;
  to: string;
}

/** `POST /api/v1/staff/theme/preview-token`. */
export interface PreviewToken {
  token: string;
  url: string;
  expires_at: string;
}

// ------------------------------------------------------------- the document --

/**
 * One colour mode's palette.
 *
 * The first block is the canonical contract (docs/theming-system.md §7.1); the block after it
 * is the legacy vocabulary `tokens.css` already defines. The legacy names are optional because
 * the generator derives them — the studio does not edit them, and it preserves them by
 * round-tripping the document it was given rather than rebuilding one from the fields it knows.
 */
export interface ModePalette {
  bg?: string;
  popover?: string;
  overlay?: string;
  card?: string;
  surface1?: string;
  surface2?: string;
  surface3?: string;
  border?: string;
  borderStrong?: string;
  text?: string;
  textMuted?: string;
  textFaint?: string;
  input?: string;
  accent?: string;
  accentHover?: string;
  accentPress?: string;
  accentInk?: string;
  success?: string;
  warning?: string;
  danger?: string;
  info?: string;
  side?: string;
  top?: string;
  surfaceHover?: string;
  surfaceInset?: string;
  surfaceActive?: string;
  lineSoft?: string;
  textSubtle?: string;
  textInverse?: string;
  scrollbar?: string;
  pink?: string;
}

export interface BrandTokens {
  primary?: string;
  secondary?: string;
  accent?: string;
}

export interface SemanticTokens {
  success?: string;
  warning?: string;
  danger?: string;
  info?: string;
  focus?: string;
}

export interface ColorTokens {
  mode?: string;
  brand: BrandTokens;
  semantic: SemanticTokens;
  dark: ModePalette;
  light: ModePalette;
}

export interface FontSpec {
  /** A family from `limits.families`, or a complete CSS stack. */
  family?: string;
  fallback?: string;
  weights?: number[];
}

export interface TextScale {
  xs?: string;
  sm?: string;
  base?: string;
  md?: string;
  lg?: string;
  xl?: string;
  "2xl"?: string;
  "3xl"?: string;
}

export interface TypographyTokens {
  display: FontSpec;
  body: FontSpec;
  mono: FontSpec;
  scale: TextScale;
  leading: { tight?: string; snug?: string; normal?: string };
  tracking: { tight?: string; wide?: string };
}

export interface RadiusTokens {
  xs?: string;
  sm?: string;
  md?: string;
  lg?: string;
  xl?: string;
  full?: string;
}

export interface ShellTokens {
  nav?: string;
  sidebarWidth?: string;
  topbarHeight?: string;
  contentMax?: string;
  cardStyle?: string;
  tableStyle?: string;
  commandPalette?: boolean;
  breadcrumbs?: boolean;
}

/**
 * The theme document.
 *
 * The index signature is not laziness: the server rejects unknown keys, so the studio has to
 * hand back the document it was given with only the fields the operator touched changed. A
 * document rebuilt from a TypeScript type would drop `annotations`, a component override the
 * studio does not edit, or whatever field the next schema adds — and the operator would publish
 * a theme that lost their work.
 */
export interface ThemeDocument {
  schema: number;
  id: string;
  name: string;
  description?: string;
  extends?: string;
  inherit?: boolean;
  color: ColorTokens;
  typography: TypographyTokens;
  space: { unit?: number; density?: string; scale?: number[] };
  shape: { radius: RadiusTokens; borderWidth?: string };
  elevation: {
    dark: { "1"?: string; "2"?: string; "3"?: string; focus?: string };
    light: { "1"?: string; "2"?: string; "3"?: string; focus?: string };
  };
  motion: { fast?: string; normal?: string; slow?: string; ease?: string };
  background: { style?: string; image?: string; gradient?: string; noise?: number };
  layout: { shell: ShellTokens; pages?: Record<string, unknown> };
  components?: Record<string, { height?: string; radius?: string; padding?: string; uppercase?: boolean }>;
  customCss?: string;
  annotations?: Record<string, string>;
  [key: string]: unknown;
}

/**
 * The limits `internal/theme` enforces, mirrored so a form can state them before a save is
 * refused. The server's answer always wins; these are what a build talking to an older server
 * shows instead of an empty select.
 */
export const DEFAULT_LIMITS: Limits = {
  max_document_bytes: 1 << 20,
  max_custom_css_bytes: 256 << 10,
  max_name_length: 80,
  modes: ["dark", "light", "system"],
  densities: ["compact", "comfortable", "roomy"],
  navs: ["sidebar", "rail", "topbar"],
  card_styles: ["flat", "outlined", "elevated"],
  table_styles: ["compact", "comfortable"],
  backgrounds: ["flat", "gradient", "image", "mesh", "paper"],
  components: ["button", "input", "card", "table", "badge", "tabs", "modal", "nav"],
  families: [
    "Inter",
    "DM Sans",
    "JetBrains Mono",
    "Source Serif 4",
    "Space Grotesk",
    "IBM Plex Mono",
    "Fraunces",
    "system-ui",
    "ui-sans-serif",
    "ui-serif",
    "ui-monospace",
    "Georgia",
    "Helvetica Neue",
    "Arial",
  ],
};

/** The default mode a first visit uses when the document says nothing. */
export const DEFAULT_MODE = "dark";

// ----------------------------------------------------------------- the API --

/**
 * Every path the studio requests, in one place.
 *
 * A screen does not build a URL: it calls a helper below. That is what makes "the studio calls
 * only documented endpoints" a property of one file rather than of seven, and it is why a typo
 * in a path is caught by reading one list instead of grepping thirty call sites.
 */
export const THEME_API = {
  overview: "/api/v1/staff/theme",
  scope: (scope: string) => `/api/v1/staff/theme/${scope}`,
  draft: (scope: string) => `/api/v1/staff/theme/${scope}/draft`,
  revision: (id: string) => `/api/v1/staff/theme/revisions/${id}`,
  publish: (id: string) => `/api/v1/staff/theme/revisions/${id}/publish`,
  restore: (id: string) => `/api/v1/staff/theme/revisions/${id}/restore`,
  diff: (id: string) => `/api/v1/staff/theme/revisions/${id}/diff`,
  previewToken: "/api/v1/staff/theme/preview-token",
  export: (scope: string) => `/api/v1/staff/theme/${scope}/export`,
} as const;

/** Reads the overview: what is live in every scope, the presets and the limits. */
export async function loadOverview(api: Api, signal?: AbortSignal): Promise<OverviewResponse> {
  return api.get<OverviewResponse>(THEME_API.overview, { cache: false, signal });
}

/** Reads one scope: its document, drafts, history and presets. */
export async function loadScope(api: Api, scope: string, signal?: AbortSignal): Promise<ScopeResponse> {
  const res = await api.get<ScopeResponse>(THEME_API.scope(scope), { cache: false, signal });
  return {
    ...res,
    drafts: Array.isArray(res?.drafts) ? res.drafts : [],
    history: Array.isArray(res?.history) ? res.history : [],
    presets: Array.isArray(res?.presets) ? res.presets : [],
  };
}

/**
 * Saves a document, creating a draft when there is none.
 *
 * The server distinguishes the two operations for a reason worth respecting: a `PUT` on a
 * revision can only ever edit a draft, so a studio that always `PUT` would fail the first time
 * an operator changed anything on a scope that has never been edited. `POST …/draft` is what
 * opens the draft, and it carries the document so the first save and the edit are one request.
 */
export async function saveDraft(
  api: Api,
  state: StudioState,
  document: ThemeDocument,
  note?: string,
): Promise<Revision> {
  const body = note === undefined ? { document } : { document, note };
  if (state.revision && state.revision.status === "draft") {
    const res = await api.put<{ revision: Revision }>(THEME_API.revision(state.revision.id), body);
    return res.revision;
  }
  const res = await api.post<{ revision: Revision }>(THEME_API.draft(state.scope), body);
  return res.revision;
}

/** Applies a preset by opening a draft from it. The preset is a starting point, never a publish. */
export async function applyPreset(api: Api, scope: string, preset: string, note?: string): Promise<Revision> {
  const body = note === undefined ? { preset } : { preset, note };
  const res = await api.post<{ revision: Revision }>(THEME_API.draft(scope), body);
  return res.revision;
}

/** Publishes a revision. The caller confirms first; this only does the write. */
export async function publishRevision(
  api: Api,
  id: string,
  note?: string,
): Promise<{ revision: Revision; publication: Publication }> {
  return api.post<{ revision: Revision; publication: Publication }>(
    THEME_API.publish(id),
    note === undefined ? {} : { note },
  );
}

/** Copies an old revision into a new draft. */
export async function restoreRevision(api: Api, id: string): Promise<Revision> {
  const res = await api.post<{ revision: Revision }>(THEME_API.restore(id), {});
  return res.revision;
}

/** The structured difference between a revision and what is live. */
export async function loadDiff(api: Api, id: string): Promise<DiffResponse> {
  return api.get<DiffResponse>(THEME_API.diff(id), { cache: false });
}

/** Mints the short-lived token the preview iframe needs. */
export async function requestPreviewToken(
  api: Api,
  revisionId: string,
  scope: string,
): Promise<PreviewToken> {
  return api.post<PreviewToken>(THEME_API.previewToken, { revision_id: revisionId, scope });
}

/** The URL the export envelope downloads from. A link, not a fetch: the browser saves the file. */
export function exportHref(scope: string): string {
  return THEME_API.export(scope);
}

// ------------------------------------------------------------ the draft state --

/**
 * The studio's working copy.
 *
 * `document` is what the screens edit and `base` is what the server last returned; the two
 * differing is the definition of unsaved work. There is one of these per session, which is what
 * lets the colour screen's edits survive a click on Typography without a save.
 */
export interface StudioState {
  scope: ScopeName;
  /** The document being edited, or null before the first scope response arrives. */
  document: ThemeDocument | null;
  /** The document as the server last returned it. */
  base: ThemeDocument | null;
  /** The open draft revision, when there is one. */
  revision: Revision | null;
  published: Revision | null;
  limits: Limits;
  presets: PresetMeta[];
  permissions: Permissions;
}

const state: StudioState = {
  scope: "panel",
  document: null,
  base: null,
  revision: null,
  published: null,
  limits: DEFAULT_LIMITS,
  presets: [],
  permissions: { can_read: true, can_write: true, can_publish: true },
};

/** The working copy. Screens mutate `document` in place; see `markEdited`. */
export function studioState(): StudioState {
  return state;
}

/**
 * Adopts a scope response.
 *
 * `force` is what a save passes: the server's answer is then the truth and replaces whatever
 * was on screen. Without it, a dirty document survives — the operator's unsaved colour is not
 * discarded because they looked at the type scale.
 */
export function adoptScope(res: ScopeResponse, opts: { force?: boolean } = {}): void {
  const scope = isScopeName(res.scope) ? res.scope : state.scope;
  const switching = scope !== state.scope;
  state.scope = scope;
  currentScope = scope;
  state.limits = res.limits ?? DEFAULT_LIMITS;
  state.presets = res.presets ?? [];
  state.permissions = res.permissions ?? state.permissions;
  state.revision = res.drafts?.[0] ?? null;
  state.published = res.published ?? null;
  if (opts.force || switching || !isDirty()) {
    state.base = cloneDocument(res.document);
    state.document = cloneDocument(res.document);
  }
}

/** Adopts the overview, which carries the limits and the presets but no document. */
export function adoptOverview(res: OverviewResponse): void {
  state.limits = res.limits ?? DEFAULT_LIMITS;
  state.presets = res.presets ?? [];
  state.permissions = res.permissions ?? state.permissions;
}

/** Replaces the working document — used by "apply this preset to the form" and by discard. */
export function setStudioDocument(document: ThemeDocument | null): void {
  state.document = document;
}

/** True when the working document differs from what the server last returned. */
export function isDirty(): boolean {
  if (!state.document || !state.base) return false;
  return stableStringify(state.document) !== stableStringify(state.base);
}

/**
 * Records that a save succeeded.
 *
 * The saved revision becomes both the base and the working copy: what is on screen after a save
 * is what the server stored, including any value it derived — a form that kept the operator's
 * version would disagree with the stylesheet the preview renders.
 */
export function adoptSavedRevision(rev: Revision): void {
  state.revision = rev;
  state.base = cloneDocument(rev.document);
  state.document = cloneDocument(rev.document);
}

/** Deep-copies a document, so a form's working copy cannot alias the last response. */
export function cloneDocument(document: ThemeDocument): ThemeDocument {
  return JSON.parse(JSON.stringify(document)) as ThemeDocument;
}

/** JSON with sorted keys, for a comparison that does not depend on insertion order. */
export function stableStringify(value: unknown): string {
  if (value === null || typeof value !== "object") return JSON.stringify(value) ?? "null";
  if (Array.isArray(value)) return `[${value.map((item) => stableStringify(item)).join(",")}]`;
  const entries = Object.entries(value as Record<string, unknown>)
    .filter(([, item]) => item !== undefined)
    .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
    .map(([key, item]) => `${JSON.stringify(key)}:${stableStringify(item)}`);
  return `{${entries.join(",")}}`;
}

/**
 * Reads a value out of the document by dotted path, as text.
 *
 * The path is the same one `internal/theme.Diff` reports, so a field the studio edits and a
 * field the history screen shows a difference for are named the same way.
 */
export function readPath(document: ThemeDocument, path: string): string {
  let cursor: unknown = document;
  for (const part of path.split(".")) {
    if (!cursor || typeof cursor !== "object") return "";
    cursor = (cursor as Record<string, unknown>)[part];
  }
  if (cursor === undefined || cursor === null) return "";
  return typeof cursor === "string" ? cursor : JSON.stringify(cursor);
}

/** Writes a value by dotted path, creating the intermediate objects a sparse document needs. */
export function writePath(document: ThemeDocument, path: string, value: unknown): void {
  const parts = path.split(".");
  let cursor: Record<string, unknown> = document as unknown as Record<string, unknown>;
  for (const part of parts.slice(0, -1)) {
    const next = cursor[part];
    if (!next || typeof next !== "object") {
      const created: Record<string, unknown> = {};
      cursor[part] = created;
      cursor = created;
      continue;
    }
    cursor = next as Record<string, unknown>;
  }
  const leaf = parts[parts.length - 1];
  if (leaf) cursor[leaf] = value;
}

// ----------------------------------------------------------------- contrast --

/** An sRGB triple, 0-255 per channel. */
export interface Rgb {
  r: number;
  g: number;
  b: number;
}

/**
 * Parses the colour forms a theme may carry: `#rgb`, `#rgba`, `#rrggbb`, `#rrggbbaa`,
 * `rgb()`/`rgba()` and `hsl()`/`hsla()`.
 *
 * A `var(--token)` reference parses to null on purpose. A reference is a legitimate theme value
 * and an unmeasurable one: the studio cannot know what the token resolves to at the point of
 * use, and inventing an answer would put a PASS beside a pair nobody checked.
 */
export function parseColour(input: string | null | undefined): Rgb | null {
  const raw = (input ?? "").trim().toLowerCase();
  if (!raw) return null;

  const hex = /^#([0-9a-f]{3,8})$/.exec(raw);
  if (hex && hex[1]) {
    const digits = hex[1];
    if (digits.length === 3 || digits.length === 4) {
      const channels = [digits.charAt(0), digits.charAt(1), digits.charAt(2)].map((c) => parseInt(c + c, 16));
      const [r, g, b] = channels;
      if (r === undefined || g === undefined || b === undefined) return null;
      return { r, g, b };
    }
    if (digits.length === 6 || digits.length === 8) {
      return {
        r: parseInt(digits.slice(0, 2), 16),
        g: parseInt(digits.slice(2, 4), 16),
        b: parseInt(digits.slice(4, 6), 16),
      };
    }
    return null;
  }

  const functional = /^(rgba?|hsla?)\(([^)]+)\)$/.exec(raw);
  if (!functional || !functional[2]) return null;
  const parts = functional[2]
    .split(/[\s,/]+/)
    .filter(Boolean)
    .map((part) => part.trim());
  const numbers = parts.slice(0, 4).map((part) => {
    const percent = part.endsWith("%");
    const value = Number.parseFloat(part);
    if (!Number.isFinite(value)) return Number.NaN;
    return percent ? (value / 100) * 255 : value;
  });
  const [first, second, third] = numbers;
  if (first === undefined || second === undefined || third === undefined) return null;
  if (!Number.isFinite(first) || !Number.isFinite(second) || !Number.isFinite(third)) return null;

  const kind = functional[1] ?? "";
  if (kind.startsWith("hsl")) {
    const angle = Number.parseFloat(parts[0] ?? "");
    const saturation = Number.parseFloat((parts[1] ?? "").replace("%", ""));
    const lightness = Number.parseFloat((parts[2] ?? "").replace("%", ""));
    if (![angle, saturation, lightness].every((value) => Number.isFinite(value))) return null;
    return hslToRgb(angle, saturation / 100, lightness / 100);
  }
  return { r: clampChannel(first), g: clampChannel(second), b: clampChannel(third) };
}

function clampChannel(value: number): number {
  return Math.max(0, Math.min(255, value));
}

function hslToRgb(hue: number, saturation: number, lightness: number): Rgb {
  const h = ((hue % 360) + 360) % 360;
  const c = (1 - Math.abs(2 * lightness - 1)) * saturation;
  const x = c * (1 - Math.abs(((h / 60) % 2) - 1));
  const m = lightness - c / 2;
  const [r, g, b] =
    h < 60
      ? [c, x, 0]
      : h < 120
        ? [x, c, 0]
        : h < 180
          ? [0, c, x]
          : h < 240
            ? [0, x, c]
            : h < 300
              ? [x, 0, c]
              : [c, 0, x];
  return {
    r: Math.round((r + m) * 255),
    g: Math.round((g + m) * 255),
    b: Math.round((b + m) * 255),
  };
}

function linear(channel: number): number {
  const c = channel / 255;
  return c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4);
}

/** WCAG 2.1 relative luminance. */
export function luminance(colour: Rgb): number {
  return 0.2126 * linear(colour.r) + 0.7152 * linear(colour.g) + 0.0722 * linear(colour.b);
}

/** WCAG 2.1 contrast ratio, 1 to 21. Order does not matter. */
export function contrastRatio(a: Rgb, b: Rgb): number {
  const la = luminance(a);
  const lb = luminance(b);
  const [hi, lo] = la < lb ? [lb, la] : [la, lb];
  return (hi + 0.05) / (lo + 0.05);
}

/** What a pair of colours is used for, which is what decides the ratio it has to reach. */
export type ContrastKind = "body" | "ui" | "line";

/**
 * The three thresholds the meter applies.
 *
 * 4.5:1 is WCAG AA for body text, 3:1 is AA for large text and for the boundary of a control,
 * and 1.4:1 is the floor for a decorative rule — a line that is meant to separate and not to be
 * read. A single threshold for all three would either refuse a legitimate hairline or wave
 * through a label nobody can read.
 */
export const CONTRAST_THRESHOLDS: Record<ContrastKind, number> = {
  body: 4.5,
  ui: 3,
  line: 1.4,
};

/** One pair the meter checks, named by the two paths it compares. */
export interface ContrastPair {
  key: string;
  fg: keyof ModePalette;
  bg: keyof ModePalette;
  kind: ContrastKind;
}

/**
 * The six pairs worth measuring.
 *
 * They are the pairs a person actually reads or aims at: body text, secondary text, a hint, a
 * control's boundary, the accent as a link, and the label inside an accent-filled button. Every
 * other combination in the palette is decoration or a surface, and measuring those would bury
 * the six that matter under twenty that do not.
 */
export const CONTRAST_PAIRS: readonly ContrastPair[] = [
  { key: "text", fg: "text", bg: "bg", kind: "body" },
  { key: "textMuted", fg: "textMuted", bg: "bg", kind: "body" },
  { key: "textFaint", fg: "textFaint", bg: "bg", kind: "ui" },
  { key: "borderStrong", fg: "borderStrong", bg: "bg", kind: "ui" },
  { key: "border", fg: "border", bg: "bg", kind: "line" },
  { key: "accent", fg: "accent", bg: "bg", kind: "ui" },
  { key: "accentInk", fg: "accentInk", bg: "accent", kind: "body" },
];

/**
 * The name a person reads for a measured pair.
 *
 * The document's field name (`textMuted`) is precise and meaningless to somebody who is not
 * reading the JSON, and the meter exists to be read by that person.
 */
export function contrastLabel(key: string): string {
  switch (key) {
    case "text":
      return tx("theme.colors.pair_text", "Body text on the background");
    case "textMuted":
      return tx("theme.colors.pair_text_muted", "Secondary text on the background");
    case "textFaint":
      return tx("theme.colors.pair_text_faint", "Hint text on the background");
    case "borderStrong":
      return tx("theme.colors.pair_border_strong", "A control's border on the background");
    case "border":
      return tx("theme.colors.pair_border", "A separating rule on the background");
    case "accent":
      return tx("theme.colors.pair_accent", "The accent as a link on the background");
    case "accentInk":
      return tx("theme.colors.pair_accent_ink", "The label inside an accent-filled button");
    default:
      return key;
  }
}

/** One measured pair. */
export interface ContrastResult {
  key: string;
  fg: string;
  bg: string;
  ratio: number;
  threshold: number;
  kind: ContrastKind;
  pass: boolean;
}

/**
 * Measures every pair in a palette.
 *
 * A pair whose colours are missing or unparseable is omitted rather than reported as a failure:
 * "this token is not a colour" is a different problem, and the server refuses it at save time
 * with a message that names the field.
 */
export function contrastReport(palette: ModePalette | null | undefined): ContrastResult[] {
  if (!palette) return [];
  const out: ContrastResult[] = [];
  for (const pair of CONTRAST_PAIRS) {
    const fg = palette[pair.fg];
    const bg = palette[pair.bg];
    const fgRgb = parseColour(fg);
    const bgRgb = parseColour(bg);
    if (!fgRgb || !bgRgb) continue;
    const ratio = contrastRatio(fgRgb, bgRgb);
    const threshold = CONTRAST_THRESHOLDS[pair.kind];
    out.push({
      key: pair.key,
      fg: String(fg),
      bg: String(bg),
      // Two decimals, rounded rather than truncated, so 4.4999 reads as 4.5 and not as a pass
      // that is really a fail.
      ratio: Math.round(ratio * 100) / 100,
      threshold,
      kind: pair.kind,
      pass: ratio >= threshold,
    });
  }
  return out;
}

/** The palette a mode name selects, resolving `system` to the mode the panel is in now. */
export function paletteFor(document: ThemeDocument, mode: string): ModePalette {
  const resolved = mode === "system" ? currentMode() : mode;
  return resolved === "light" ? document.color.light : document.color.dark;
}

/** The theme currently on <html>, read from the attribute the shell sets. */
export function currentMode(): "light" | "dark" {
  return document.documentElement.dataset.theme === "light" ? "light" : "dark";
}

/** A byte count, counted as UTF-8 so a `max_custom_css_bytes` bound means what it says. */
export function byteLength(value: string): number {
  return new TextEncoder().encode(value).length;
}

// -------------------------------------------------------------------- diff --

/** Shortens a digest to the twelve characters a table column can hold. */
export function shortDigest(digest: string | null | undefined): string {
  const raw = (digest ?? "").trim();
  if (!raw) return "—";
  const value = raw.startsWith("sha256:") ? raw.slice("sha256:".length) : raw;
  return value.slice(0, 12);
}

/** When a revision was published, in the two registers a table and a card need. */
export function whenText(value: string | null | undefined): string {
  if (!value) return "—";
  return `${relative(value)} · ${dateTime(value)}`;
}

/** The tone for a revision status, so a draft and a live revision cannot look alike. */
export function statusTone(status: string | null | undefined): Tone {
  switch (status) {
    case "published":
      return "success";
    case "draft":
      return "warning";
    default:
      return "neutral";
  }
}

/**
 * The diff, as a table.
 *
 * `from` and `to` arrive as JSON text, which for an object is a whole subtree on one line. The
 * cell keeps it on one line and puts the full value in `title`, so a difference in a nested
 * object is readable without turning the dialog into a wall of wrapped JSON.
 */
export function diffTable(changes: readonly DiffChange[]): HTMLElement {
  if (changes.length === 0) {
    return h(
      "p",
      { class: "field-hint" },
      tx("theme.diff.empty", "No field differs from what is published."),
    );
  }
  const rows = changes.map((change) =>
    h(
      "tr",
      null,
      h("td", null, h("code", { class: "inline-code" }, change.path)),
      h("td", { class: "theme-diff-value", title: change.from || "—" }, change.from || "—"),
      h("td", { class: "theme-diff-value", title: change.to || "—" }, change.to || "—"),
    ),
  );
  return h(
    "div",
    { class: "table-wrap" },
    h(
      "table",
      { class: "table" },
      h(
        "thead",
        null,
        h("tr", null, h("th", null, tx("theme.diff.path", "Field")), h("th", null, tx("theme.diff.from", "Published")), h("th", null, tx("theme.diff.to", "This revision"))),
      ),
      h("tbody", null, ...rows),
    ),
  );
}

// ------------------------------------------------------------- the chrome --

export type StudioTab = "overview" | "presets" | "colors" | "typography" | "layout" | "css" | "history";

interface StudioNavItem {
  key: StudioTab;
  href: string;
  labelKey: string;
  label: string;
}

/**
 * The studio's sub-navigation.
 *
 * It mirrors the left sub-nav in docs/theming-system.md §10.1 for the sections this build has.
 * The entries are links rather than buttons because each section is a route: an operator can
 * bookmark the colour screen and send it to a colleague, and the browser's Back button walks
 * the sections they visited.
 */
export const STUDIO_NAV: readonly StudioNavItem[] = [
  { key: "overview", href: "/settings/theme", labelKey: "theme.nav.overview", label: "Overview" },
  { key: "presets", href: "/settings/theme/presets", labelKey: "theme.nav.presets", label: "Presets" },
  { key: "colors", href: "/settings/theme/colors", labelKey: "theme.nav.colors", label: "Colours" },
  { key: "typography", href: "/settings/theme/typography", labelKey: "theme.nav.typography", label: "Typography" },
  { key: "layout", href: "/settings/theme/layout", labelKey: "theme.nav.layout", label: "Layout" },
  { key: "css", href: "/settings/theme/css", labelKey: "theme.nav.css", label: "Custom CSS" },
  { key: "history", href: "/settings/theme/history", labelKey: "theme.nav.history", label: "History" },
];

/** The sub-nav, with the current section marked. */
export function studioSubnav(active: StudioTab): HTMLElement {
  return h(
    "nav",
    { class: "tabs theme-subnav", "aria-label": tx("theme.nav.label", "Theme Studio sections") },
    ...STUDIO_NAV.map((item) =>
      h(
        "a",
        {
          class: `tab${item.key === active ? " is-active" : ""}`,
          href: item.href,
          "aria-current": item.key === active ? "page" : undefined,
        },
        tx(item.labelKey, item.label),
      ),
    ),
  );
}

export interface StudioHeadOptions {
  active: StudioTab;
  title: string;
  sub: string;
  actions?: Child;
}

/** The head every studio screen shares: the page title and the sub-nav beneath it. */
export function studioHead(opts: StudioHeadOptions): HTMLElement {
  return h(
    "div",
    { class: "stack-sm" },
    pageHead({
      eyebrow: tx("theme.eyebrow", "Appearance"),
      title: opts.title,
      sub: opts.sub,
      actions: opts.actions,
    }),
    studioSubnav(opts.active),
  );
}

/**
 * The "you cannot open this" state.
 *
 * It is a card rather than a redirect because a 403 that silently moved the operator somewhere
 * else is a permission problem they cannot report. Naming the permission and the reason is what
 * turns it into a support request with an answer.
 */
export function studioDenied(permission: string, what: string): HTMLElement {
  return h(
    "div",
    { class: "page" },
    studioHead({ active: "overview", title: tx("theme.title", "Theme Studio"), sub: tx("theme.sub", "The appearance of the panel, the portal, the storefront, mail and invoices.") }),
    banner(
      "danger",
      tx("theme.denied.title", "You cannot open the theme studio"),
      h(
        "span",
        null,
        tx("theme.denied.body", "{{what}} needs the {{permission}} permission. Theme editing is a separate grant from settings: a theme can restyle every screen, so it is not implied by being able to change a setting.", {
          what,
          permission,
        }),
      ),
      btn(tx("theme.denied.back", "Back to the dashboard"), { href: "/dashboard", size: "sm", variant: "ghost" }),
    ),
  );
}

/** The scope picker shared by the six editing screens. */
export function scopePicker(current: ScopeName, onChange: (next: ScopeName) => void): HTMLElement {
  const control = select(
    SCOPES.map((entry) => ({ value: entry.scope, label: tx(`theme.scope.${entry.scope}`, entry.label) })),
    {
      id: "theme-scope",
      value: current,
      onchange: (event: Event) => {
        const next = (event.target as HTMLSelectElement).value;
        if (isScopeName(next)) onChange(next);
      },
    },
  );
  return h(
    "div",
    { class: "field theme-scope" },
    h("label", { class: "field-label", for: "theme-scope" }, tx("theme.scope.label", "Scope")),
    control,
    h("span", { class: "field-hint" }, tx("theme.scope.hint", "Which surface this theme paints. Portal and storefront inherit the panel until they override a token.")),
  );
}

/** A one-line description of what the selected scope is, shown under the scope picker. */
export function scopeNote(scope: string): HTMLElement {
  return h("p", { class: "field-hint theme-scope-note" }, scopeDescription(scope));
}

// ----------------------------------------------------------------- helpers --

/**
 * A colour field: a native picker beside a text box, kept in step.
 *
 * Both are needed, for the reason the branding screen states: the picker is what most operators
 * want, and the text box is what makes a value copied from a brand guide usable — and what makes
 * `var(--accent)` expressible, which a colour input cannot represent.
 */
export function colourInput(id: string, value: string, onChange: (value: string) => void): HTMLElement {
  const picker = h("input", {
    class: "input input--color",
    type: "color",
    id: `${id}-picker`,
    value: normaliseColour(value) || "#000000",
    oninput: (event: Event) => {
      const next = (event.target as HTMLInputElement).value;
      text.value = next;
      onChange(next);
    },
  });
  const text = h("input", {
    class: "input",
    type: "text",
    id,
    value,
    placeholder: "#000000",
    spellcheck: false,
    oninput: (event: Event) => {
      const next = (event.target as HTMLInputElement).value.trim();
      const parsed = parseColour(next);
      // The picker follows only a value it can represent. A half-typed "#" or a `var(--x)` must
      // not reset the picker to black while the operator is still typing.
      if (parsed) picker.value = normaliseColour(next);
      onChange(next);
    },
  });
  return h("div", { class: "colour-field" }, picker, text);
}

/** Normalises a typed colour to `#rrggbb` for the native picker, or "" when it is not one. */
function normaliseColour(value: string): string {
  const rgb = parseColour(value);
  if (!rgb) return "";
  const part = (channel: number) => clampChannel(Math.round(channel)).toString(16).padStart(2, "0");
  return `#${part(rgb.r)}${part(rgb.g)}${part(rgb.b)}`;
}

/**
 * Paints an element's preview with a palette.
 *
 * The values are unsaved and arbitrary, so a class cannot carry them: this is the one place in
 * the studio that writes a custom property, and it writes it on the preview subtree only —
 * never on the document root. That is the same rule `views/branding.ts` follows and the reason
 * `lib/brand.ts` is the only module allowed to touch the panel's own accent.
 */
export function paintPreview(element: HTMLElement, palette: ModePalette): void {
  const properties: Array<[string, string | undefined]> = [
    ["--preview-bg", palette.bg],
    ["--preview-surface", palette.card ?? palette.surface1],
    ["--preview-surface-2", palette.surface2],
    ["--preview-border", palette.border],
    ["--preview-border-strong", palette.borderStrong],
    ["--preview-text", palette.text],
    ["--preview-text-muted", palette.textMuted],
    ["--preview-text-faint", palette.textFaint],
    ["--preview-accent", palette.accent],
    ["--preview-accent-ink", palette.accentInk],
  ];
  for (const [property, value] of properties) {
    if (value) element.style.setProperty(property, value);
    else element.style.removeProperty(property);
  }
}

/**
 * A row of colour chips.
 *
 * The colours are arbitrary theme values, so a class cannot carry them: each chip gets one
 * custom property, written on the chip itself. It is the same narrow exception `paintPreview`
 * uses, and it is scoped to the chip — nothing here touches a token the rest of the page reads.
 */
export function swatchRow(colours: readonly string[], label?: string): HTMLElement {
  const chips = colours.map((colour) => {
    const chip = h("span", { class: "theme-swatch", title: colour, "aria-hidden": "true" });
    chip.style.setProperty("--swatch", colour);
    return chip;
  });
  return h(
    "div",
    {
      class: "theme-swatches",
      role: "img",
      "aria-label": label ?? tx("theme.swatch.label", "Theme colours: {{list}}", { list: colours.join(", ") }),
    },
    ...chips,
  );
}

/** Reports a failed studio request in the same shape the rest of the panel reports one. */
export function reportStudioError(ctx: Ctx, err: unknown, fallback: string): void {
  if (err instanceof ApiError) {
    const problem = problemFrom(err) ?? { code: err.code, detail: err.detail };
    const fields = problem.fields?.length
      ? " " + problem.fields.map((field) => problemFieldText(field)).join(" ")
      : "";
    ctx.toast(problemText(problem, fallback) + fields, { tone: "danger", detail: problem.code });
    return;
  }
  ctx.toast(fallback, { tone: "danger", detail: String(err) });
}

/** The server's own sentence for a refusal, which a validation screen shows verbatim. */
export function problemSentence(err: unknown, fallback: string): string {
  if (err instanceof ApiError) return err.detail || problemText(problemFrom(err), fallback);
  return err instanceof Error ? err.message : fallback;
}

// ---------------------------------------------------------------- preview --

/**
 * The widths the preview can be shown at (docs/theming-system.md §10.2).
 *
 * The labels are keys rather than English so the three buttons translate like every other
 * control; the widths are the three the plan names and are not a free-form size.
 */
const PREVIEW_DEVICES: ReadonlyArray<{ key: string; labelKey: string; label: string; width: number }> = [
  { key: "desktop", labelKey: "theme.preview.device_desktop", label: "Desktop", width: 1440 },
  { key: "tablet", labelKey: "theme.preview.device_tablet", label: "Tablet", width: 834 },
  { key: "mobile", labelKey: "theme.preview.device_mobile", label: "Mobile", width: 390 },
];

/**
 * Opens a draft in the preview iframe.
 *
 * The token is minted first and the iframe is only created once it exists, because the preview
 * URL *is* the token: an iframe pointed at the endpoint without one renders a 403 page that
 * looks like a broken theme. The token expires in fifteen minutes and is bound to the operator,
 * which is why this mints a new one per open rather than caching.
 */
export async function openPreviewDialog(
  ctx: Ctx,
  opts: { revisionId: string; scope: string; label: string },
): Promise<void> {
  let token: PreviewToken;
  try {
    token = await requestPreviewToken(ctx.api, opts.revisionId, opts.scope);
  } catch (err) {
    reportStudioError(ctx, err, tx("theme.preview.failed", "The preview could not be opened"));
    return;
  }

  const frame = h("iframe", {
    class: "theme-preview-frame",
    src: token.url,
    title: tx("theme.preview.frame", "Preview of {{label}}", { label: opts.label }),
    // The preview is a same-origin first-party document; it needs no sandbox of its own because
    // its CSP already forbids scripts it did not ship and the response is `no-store, noindex`.
    referrerpolicy: "same-origin",
  });
  const host = h("div", { class: "theme-preview-host", dataset: { device: "desktop" } }, frame);

  // One button per device, kept in a list so choosing a width can mark the chosen one without
  // looking the buttons up again in the DOM: a query would depend on the order of the row, and
  // the row is built from the same list.
  const buttons: HTMLButtonElement[] = [];
  const choose = (key: string): void => {
    host.dataset.device = key;
    for (const [index, device] of PREVIEW_DEVICES.entries()) {
      const button = buttons[index];
      if (!button) continue;
      button.classList.toggle("btn--primary", device.key === key);
      button.classList.toggle("btn--ghost", device.key !== key);
    }
  };
  for (const device of PREVIEW_DEVICES) {
    const button = btn(`${tx(device.labelKey, device.label)} · ${device.width}`, {
      size: "sm",
      variant: device.key === "desktop" ? "primary" : "ghost",
      onclick: () => choose(device.key),
    });
    if (button instanceof HTMLButtonElement) buttons.push(button);
  }

  openDialog({
    title: tx("theme.preview.title", "Preview"),
    subtitle: tx("theme.preview.sub", "{{label}} as this revision would render it. Nothing is published by looking.", {
      label: opts.label,
    }),
    wide: true,
    body: h(
      "div",
      { class: "stack" },
      h("div", { class: "row row-wrap" }, ...buttons),
      host,
      h(
        "p",
        { class: "field-hint" },
        tx("theme.preview.expiry", "The link stops working at {{when}}, and it only renders this draft to you.", {
          when: dateTime(token.expires_at),
        }),
      ),
    ),
  });
}

/** A revision's status, as a badge. */
export function revisionBadge(status: string | null | undefined): HTMLElement {
  const value = status ?? "draft";
  return badge(
    value === "published"
      ? tx("theme.status.published", "Published")
      : value === "draft"
        ? tx("theme.status.draft", "Draft")
        : tx("theme.status.archived", "Archived"),
    statusTone(value),
    { dot: true },
  );
}

/** The card that says a build has no theme store, rather than offering a button that cannot work. */
export function unavailableCard(detail: string): HTMLElement {
  return card({
    title: tx("theme.unavailable.title", "The theme registry is not available in this build"),
    body: h(
      "div",
      { class: "stack-sm" },
      h("p", { class: "field-hint" }, detail),
      h(
        "p",
        { class: "field-hint" },
        tx("theme.unavailable.fallback", "The installation keeps the appearance it had before the theme registry existed, which is exactly what an installation with no theme published serves."),
      ),
      h("div", { class: "row" }, btn(tx("theme.unavailable.dashboard", "Back to the dashboard"), { href: "/dashboard", size: "sm", icon: "home" })),
    ),
  });
}

/** The empty state a scope with no draft and no publication shows instead of an editor. */
export function noRevisionNote(): HTMLElement {
  return h(
    "div",
    { class: "theme-empty" },
    icon("sparkles", 18),
    h(
      "p",
      { class: "field-hint" },
      tx(
        "theme.no_revision",
        "Nothing has been published for this scope, so it is served the built-in default. Applying a preset creates a draft; publishing is a separate, deliberate step.",
      ),
    ),
  );
}
