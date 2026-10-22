// The client half of localization: hold the catalogue, resolve a locale, render a sentence.
//
// The backend is already the whole feature. `locales` and `message_catalog` are tables, the
// resolution chain (preference, then Accept-Language, then the default) lives in
// internal/i18n, and `GET /api/v1/i18n/messages?locale=…` answers with one flat map whose
// fallback has *already been applied* — the locale's own text with the default's text merged
// underneath it. This module is the missing half: it keeps that map for the session, decides
// which locale the page is in, and gives a screen a function to call instead of a literal.
//
// ## The convention: `t()` at build time, not `data-i18n` attributes
//
// There are two ways to render translated text into a framework-free DOM, and this file picks
// one: **every view calls `t()`/`tx()` where it builds the node**, and there is no
// `translateTree(root)` pass over the finished document. The reasons are specific to how this
// frontend is built:
//
//  1. A `data-i18n` attribute carries a key *and* leaves the English text in the markup, so the
//     same sentence has two sources — the attribute and the text node — and the pre-translation
//     DOM is a second, silently divergent copy of the catalogue. That is exactly the failure
//     the migration header warns about: untranslated starts to look translated.
//  2. `h()` builds real text nodes, often beside real elements (a chip's count, a button's
//     icon). A tree pass translates by assigning `textContent`, which deletes those children.
//     `t()` at build time never touches a node that is already correct.
//  3. Attribute values are strings a customer reads too — `placeholder`, `aria-label`, `title`,
//     `alt`. With `t()` they are the same call as a text node; with a tree pass they need a
//     second and third attribute (`data-i18n-attr`, `data-i18n-params`) invented for them.
//  4. Re-rendering is already solved: the shell rebuilds the current route on a locale change
//     (see `onLocaleChange`), so the "translate the DOM in place" job a tree pass would do is a
//     route render that already exists.
//
// ## A missing translation is never blank
//
// Three tiers, and every one of them produces words:
//
//  1. the current locale's text (the server merged the default's text underneath it, so a key
//     that exists anywhere is here),
//  2. the default locale's own catalogue, when it has been loaded,
//  3. the English sentence the call site passed (`tx`), or the key itself for a bare `t()`.
//
// The catalogue has 88 seeded keys — the storefront, the locales screen and transactional mail.
// Screens whose keys are not seeded yet still pass their English sentence, which is why `tx()`
// exists and why the storefront's own seeded text and its inline fallback are the same words:
// an operator can add `login.title` in /locales and it takes effect without a code change, and
// until then the screen reads correctly in English rather than reading `login.title`.
//
// ## What this module deliberately does not do
//
// Pluralization and date/number formatting. The catalogue is (locale, key, text) with
// `{{name}}` placeholders and no plural rules, so a counted sentence picks between two keys at
// the call site — see the `count === 1 ? … : …` in views/portal.ts. Dates and money come from
// lib/format.ts, which is a separate and larger job.

import type { Api } from "./api";
import { h } from "./dom";

/** One offered language, exactly as `GET /api/v1/i18n/locales` emits it. */
export interface LocaleOption {
  code: string;
  /** The English name, for an operator's list: "German". */
  name: string;
  /** The name in the language itself, for the customer's switcher: "Deutsch". */
  native_name: string;
  is_default: boolean;
}

export interface LocalesResponse {
  locales: LocaleOption[];
  default: string;
  /** What this request resolved to, and which step of the chain answered. */
  resolved?: { locale: string; source: string };
}

export interface MessagesResponse {
  locale: string;
  messages: Record<string, string>;
}

/** Values substituted into `{{name}}` placeholders. Strings and numbers only: a placeholder is
 *  prose, and an object rendered into a sentence is a `[object Object]` on a customer's screen. */
export type MessageParams = Record<string, string | number>;

/** One entry in a language switcher. `label` is the native name, which is the one a person who
 *  does not read the current language can still recognise. */
export interface LanguageOption {
  code: string;
  label: string;
  selected: boolean;
}

export interface LanguageSwitcherOptions {
  /** The offered locales. Defaults to the ones `initI18n` loaded. */
  locales?: LocaleOption[];
  /** The currently selected code. Defaults to the current locale. */
  selected?: string;
  /** Called with the chosen code. `setLocale` persists and notifies; a caller that wants the
   *  choice to also re-render itself passes its own handler. */
  onSelect: (code: string) => void;
  id?: string;
  /** The accessible name. A select with no visible label needs one, and "Language" is not a
   *  word to guess at: it is the key `storefront.language.label` / `shell.language.label`. */
  label?: string;
  class?: string;
}

/**
 * A language `<select>`, or `null` when there is only one language to offer.
 *
 * Returning `null` rather than a disabled control is the point: one language is not a choice,
 * and a dropdown with a single option in it is a control that does nothing while looking like
 * it might. A fresh installation offers only `en`, so this is the common case and it must
 * render nothing at all.
 *
 * It lives here rather than in ui/ because both shells need it — the signed-in shell and the
 * public storefront — and they share no other module that could own it. `h` is the DOM
 * primitive, not a framework, so this stays a two-line builder.
 */
export function languageSwitcher(options: LanguageSwitcherOptions): HTMLSelectElement | null {
  const items = languageOptions(options.locales ?? enabled, options.selected ?? currentLocale());
  if (items.length === 0) return null;

  const el = h("select", {
    class: ["select", "select--sm", "select--auto", options.class ?? ""].filter(Boolean).join(" "),
    id: options.id,
    // A select cannot be labelled by its options; without this a screen reader announces
    // "combo box" and the person has to guess what it changes.
    "aria-label": options.label ?? tx("shell.language.label", "Language"),
    title: options.label ?? tx("shell.language.label", "Language"),
    onchange: (event: Event) => {
      const target = event.target as HTMLSelectElement | null;
      if (target && target.value) options.onSelect(target.value);
    },
  });
  for (const item of items) {
    const option = h("option", { value: item.code, lang: item.code }, item.label);
    option.selected = item.selected;
    el.appendChild(option);
  }
  return el;
}

/** The locale a fresh installation ships, and the last resort when the API cannot be reached. */
const DEFAULT_LOCALE = "en";

/** Where an explicit choice is kept. localStorage rather than the session, because a language
 *  chosen on the storefront should still be in force tomorrow — and it is the idiom the theme
 *  already uses (lib/theme.ts), so there is one place a "remember this browser" fact lives. */
const STORAGE_KEY = "catalyst.locale";

/** locale code -> merged catalogue. One fetch per locale per session. */
const catalogues = new Map<string, Record<string, string>>();
/** In-flight fetches, so two screens asking for the same locale share one request. */
const inflight = new Map<string, Promise<Record<string, string>>>();
/** Locale-change subscribers: the shell re-renders the current route, a public page itself. */
const listeners = new Set<(locale: string) => void>();

let enabled: LocaleOption[] = [];
let current = "";
let fallback = DEFAULT_LOCALE;
let client: Api | null = null;

// ------------------------------------------------------------------ locale --

/** The locale the page is rendering in. Never empty: an unresolved page is in the default. */
export function currentLocale(): string {
  return current || fallback || DEFAULT_LOCALE;
}

/** The default locale, which is what a missing key falls back to. */
export function defaultLocale(): string {
  return fallback || DEFAULT_LOCALE;
}

/** The languages this installation offers, in the order the switcher shows them. Empty until
 *  `initI18n` has answered, and empty on an installation that offers no locale at all. */
export function enabledLocales(): LocaleOption[] {
  return enabled.slice();
}

/**
 * The switcher's options, or an empty list when there is nothing to switch to.
 *
 * The empty list is the whole contract: one language is not a choice, so a caller that gets
 * `[]` renders nothing at all rather than a control with a single option in it. That is the
 * common case — a fresh installation offers only `en` — and a dead dropdown is a worse lie
 * than no dropdown.
 */
export function languageOptions(locales: LocaleOption[] = enabled, selected: string = currentLocale()): LanguageOption[] {
  if (!Array.isArray(locales) || locales.length < 2) return [];
  const out: LanguageOption[] = [];
  for (const locale of locales) {
    if (!locale || !locale.code) continue;
    out.push({
      code: locale.code,
      // The native name is what the person looking for their own language recognises; the
      // English name is a fallback for a locale row that has not been given one yet.
      label: locale.native_name || locale.name || locale.code,
      selected: locale.code === selected,
    });
  }
  // A list that lost all but one usable row is still not a choice.
  return out.length > 1 ? out : [];
}

/**
 * Loads the catalogue for one locale and keeps it for the session.
 *
 * The response is already merged, so this is one request and no client-side fallback work. A
 * failed fetch resolves with an empty map and caches nothing: a storefront that cannot reach
 * the API still renders its own English sentences instead of replacing the whole page with an
 * error, and a transient failure is retried by the next screen rather than poisoning the
 * session.
 */
export async function loadMessages(api: Api, locale: string): Promise<Record<string, string>> {
  const code = (locale || "").trim() || fallback || DEFAULT_LOCALE;
  const cached = catalogues.get(code);
  if (cached) return cached;
  const pending = inflight.get(code);
  if (pending) return pending;

  const request = (async (): Promise<Record<string, string>> => {
    try {
      const res = await api.get<MessagesResponse>("/api/v1/i18n/messages", {
        query: { locale: code },
        // The endpoint is cacheable but must be revalidated, because an operator who edits a
        // translation expects the next page to show it. The API client's in-memory cache would
        // answer from a copy taken before the edit, so it is bypassed here and the browser's
        // HTTP cache does the work.
        cache: false,
      });
      const messages = res && typeof res.messages === "object" && res.messages ? res.messages : {};
      catalogues.set(code, messages);
      return messages;
    } catch {
      // Not cached: a failed fetch is a moment, not a fact about this locale.
      return {};
    }
  })();

  inflight.set(code, request);
  void request.then(() => {
    if (inflight.get(code) === request) inflight.delete(code);
  });
  return request;
}

/** Reads the enabled locales, the default, and what this request resolved to. */
export async function loadLocales(api: Api): Promise<LocalesResponse> {
  const res = await api.get<LocalesResponse>("/api/v1/i18n/locales", { cache: false });
  return {
    locales: Array.isArray(res?.locales) ? res.locales : [],
    default: res?.default || DEFAULT_LOCALE,
    resolved: res?.resolved,
  };
}

/**
 * Boots the client catalogue: which languages exist, which one this page is in, and the text.
 *
 * Returns the offered locales, so the shell can build the switcher from the same answer it
 * rendered with. It never throws: a panel whose locale endpoint is down is a panel in English,
 * not a panel that fails to start.
 */
export async function initI18n(api: Api): Promise<LocaleOption[]> {
  client = api;
  let response: LocalesResponse;
  try {
    response = await loadLocales(api);
  } catch {
    enabled = [];
    return enabled;
  }

  enabled = response.locales.filter((locale) => Boolean(locale && locale.code));
  fallback = response.default || enabled.find((locale) => locale.is_default)?.code || DEFAULT_LOCALE;

  // An explicit choice in this browser beats everything else. It is the most recent deliberate
  // action by this person on this device, and the alternative — letting the account's stored
  // preference flip a language the customer just picked — is a switcher that undoes itself on
  // the next page load. (When a route for `SetUserLocale` lands, `setLocale` should also write
  // the choice to the account so it follows the person to another browser.)
  const offered = new Set(enabled.map((locale) => locale.code));
  const stored = readStored();
  const resolved = response.resolved?.locale ?? "";
  current =
    (stored && offered.has(stored) ? stored : "") ||
    (resolved && offered.has(resolved) ? resolved : "") ||
    fallback;

  applyDocumentLang();
  await loadMessages(api, current);
  // The default's own catalogue is the second tier of the chain. The server merges it into
  // every response, so this is one small request that is normally redundant — and it is what
  // makes a switch back to the default instant, and what makes `t()`'s chain a fact rather
  // than a claim when the merged response is missing something.
  if (current !== fallback) await loadMessages(api, fallback);
  return enabled;
}

/**
 * Switches language, persists the choice, and tells every subscriber to re-render.
 *
 * The catalogue is fetched *before* the switch is announced, so the re-render that follows is
 * in the new language rather than flashing the old one. A fetch that fails still switches: the
 * fallback chain and each call site's own sentence keep the page readable, and a switcher that
 * silently refused to move would be the bug this whole module exists to fix.
 */
export async function setLocale(code: string): Promise<void> {
  const next = (code || "").trim();
  if (!next) return;
  if (enabled.length > 0 && !enabled.some((locale) => locale.code === next)) return;
  if (next === current) return;

  if (client) await loadMessages(client, next);
  current = next;
  persist(next);
  applyDocumentLang();
  for (const listener of [...listeners]) {
    try {
      listener(next);
    } catch {
      // One screen's failed re-render must not stop the others from translating.
    }
  }
}

/** Subscribes to locale changes. Returns the unsubscribe, so a page can clean up. */
export function onLocaleChange(fn: (locale: string) => void): () => void {
  listeners.add(fn);
  return () => {
    listeners.delete(fn);
  };
}

/** Re-reads the offered locales and the current one. Used by the shell after a sign-in, when
 *  the account's own preference may differ from the anonymous visitor's. */
export async function refreshLocales(api: Api): Promise<LocaleOption[]> {
  return initI18n(api);
}

/** Drops every cached catalogue and subscriber. For tests, and for a hard reload of the
 *  catalogue after an operator edits a translation. */
export function resetI18n(): void {
  catalogues.clear();
  inflight.clear();
  listeners.clear();
  enabled = [];
  current = "";
  fallback = DEFAULT_LOCALE;
  client = null;
}

// ------------------------------------------------------------------- lookup --

/**
 * The text for `key` in the current locale.
 *
 * `params` fills `{{name}}` placeholders. `fallback` is the English sentence this call site is
 * a translation of; it is used when the catalogue has nothing for the key, which is the normal
 * state for a screen whose keys an operator has not created yet. With neither, the key itself
 * is returned — visible, greppable, and never a blank label.
 */
export function t(key: string, params?: MessageParams, fallbackText?: string): string {
  const own = catalogues.get(currentLocale())?.[key];
  if (own) return interpolate(own, params);

  const def = catalogues.get(defaultLocale())?.[key];
  if (def) return interpolate(def, params);

  if (fallbackText) return interpolate(fallbackText, params);
  return key;
}

/**
 * `t()` with the English source sentence: `tx("storefront.hero.title", "Servers that come with
 * a ledger.")`.
 *
 * Every translated call site in the app uses this, because the catalogue is deliberately
 * smaller than the app: a key that does not exist yet must still render words. The sentence
 * passed here is the same one seeded in 0037_i18n.sql, so an installation that has never opened
 * /locales reads exactly like one that has.
 */
export function tx(key: string, sourceText: string, params?: MessageParams): string {
  return t(key, params, sourceText);
}

/**
 * Replaces `{{name}}` placeholders in one pass.
 *
 * Single pass is the security-relevant half: a value that itself contains `{{link}}` is written
 * literally rather than expanded, so a customer who can type into a field cannot make a message
 * pull in another variable's value. This mirrors `i18n.Substitute` in Go, including its other
 * rule — an unknown placeholder is left as written (`{{missing}}`), which is visible in a
 * rendered sentence and therefore fixable, rather than silently blank.
 */
export function interpolate(text: string, params?: MessageParams): string {
  if (!params || !text.includes("{{")) return text;
  let out = "";
  for (let i = 0; i < text.length; ) {
    if (text.startsWith("{{", i)) {
      const end = text.indexOf("}}", i + 2);
      if (end >= 0) {
        const name = text.slice(i + 2, end);
        // hasOwnProperty, not `params[name]`: a placeholder named `constructor` or `toString`
        // would otherwise resolve to a function on the prototype and render its source.
        if (Object.prototype.hasOwnProperty.call(params, name)) {
          out += String(params[name]);
          i = end + 2;
          continue;
        }
      }
    }
    out += text[i];
    i += 1;
  }
  return out;
}

// ---------------------------------------------------------------- plumbing --

function readStored(): string {
  try {
    return globalThis.localStorage?.getItem(STORAGE_KEY) ?? "";
  } catch {
    // A browser with storage disabled (or a test runner without it) is a browser with no
    // remembered choice, not a broken page.
    return "";
  }
}

function persist(code: string): void {
  try {
    globalThis.localStorage?.setItem(STORAGE_KEY, code);
  } catch {
    /* the choice still applies to this page */
  }
}

function applyDocumentLang(): void {
  if (typeof document === "undefined") return;
  // The `lang` attribute is what a screen reader picks its voice from and what the browser
  // offers to translate. Leaving it at "en" on a German page is a translation nobody hears.
  document.documentElement.lang = currentLocale();
}
