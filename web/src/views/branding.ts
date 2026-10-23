// Branding.
//
// The tenant's brand: a display name, a logo and an accent colour per colour mode. It reaches
// the panel chrome (the sidebar wordmark, the accent every action uses) and the invoice PDF
// (the letterhead, a colour band and the document title), and it is deliberately *not* a theme
// registry: light and dark stay exactly as they are, and a brand only ever contributes an
// accent. That is what keeps this screen from being able to break a theme — see
// `lib/brand.ts`, where the override is written and where the rule lives.
//
// Three things shape the screen:
//
//  - **A preview, not a promise.** The card on the right is painted with the values in the
//    form, using the same `paintFor` the shell uses, so an operator sees the accent on a
//    button, a link and a badge before saving. A colour picker with no preview is a colour
//    picker that produces a surprise.
//  - **Legibility is a fact the server reports.** A pale accent is a legitimate brand colour
//    and an illegible link, so the preview says so and shows the adjusted colour that will
//    actually be used for small text. The screen does not refuse the colour.
//  - **Clearing is a first-class action.** An empty field means "not configured" and restores
//    the built-in appearance, which is the fallback the invoice and the panel both rely on.
//    So "Reset to the built-in appearance" clears all three values rather than writing the
//    defaults back — writing them back would make "unset" indistinguishable from "set to the
//    default".
//
// The logo is uploaded rather than linked. A URL field would be an SSRF primitive and a
// privacy leak (the panel fetching a third-party origin on every render), so the bytes are
// stored, deduped and served from `/api/v1/media/{id}` with an immutable cache.

import { ApiError } from "../lib/api";
import { derivedFor, NO_BRAND, paintFor, type Brand } from "../lib/brand";
import {
  ACCENT_PRESETS,
  mailBrandFromSettings,
  mailBrandToSettings,
  presetAccents,
  presetMatches,
  safeAccent,
  type MailBrandSettings,
  type SettingsKey,
} from "../lib/mail-brand";
import { problemFieldText, problemFrom, problemText } from "../lib/i18n";
import { fill, h, swap } from "../lib/dom";
import { icon } from "../lib/icons";
import { tx } from "../lib/i18n";
import type { Ctx } from "../lib/types";
import { badge, banner, btn, card, field, input, pageHead, resource, skeletonPage, spinner, withBusy } from "../ui/kit";

export const meta = { title: "Branding", crumb: "Branding" };

// ---------------------------------------------------------------- payloads --

interface BrandingResponse {
  brand: Brand;
  group?: unknown;
  limits?: {
    name_max: number;
    logo_max_bytes: number;
    logo_types: string[];
    min_text_contrast: number;
  };
}

const ENDPOINT = "/api/v1/staff/branding";

/** The limits the server enforces, mirrored here so the form can state them before a save is
 *  refused. A refusal is still authoritative; this only saves the round trip. */
const DEFAULT_LIMITS = {
  name_max: 80,
  logo_max_bytes: 256 << 10,
  logo_types: ["image/png", "image/jpeg"],
  min_text_contrast: 4.5,
};

// ------------------------------------------------------------------ screen --

export default async function brandingPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("settings:write")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Branding" }),
      banner(
        "danger",
        "You cannot change the brand",
        h(
          "span",
          null,
          "This page needs the ",
          h("code", { class: "inline-code" }, "settings:write"),
          " permission. Branding is gated with the rest of the settings surface: the name and the logo are what a customer sees on every document, so it is not a support-desk edit.",
        ),
        btn("Back to the dashboard", { href: "/", size: "sm", variant: "ghost" }),
      ),
    );
  }

  const host = h("div");
  const paint = (): void => {
    swap(
      host,
      resource(
        ctx,
        async (signal) => {
          try {
            return await ctx.api.get<BrandingResponse>(ENDPOINT, { cache: false, signal });
          } catch (err) {
            // A 404 is a fact about the build, not a failed request: retrying produces the
            // same answer, so the screen says what is missing instead of offering a button
            // that cannot work.
            if (err instanceof ApiError && (err.status === 404 || err.status === 501)) return "unavailable" as const;
            throw err;
          }
        },
        (data) => (data === "unavailable" ? unavailableCard() : renderEditor(ctx, data as BrandingResponse, paint)),
        { skeleton: skeletonPage },
      ),
    );
  };
  paint();
  return h("div", { class: "page" }, host);
}

/** What a build without the branding API looks like. */
function unavailableCard(): HTMLElement {
  return card({
    title: "Branding is not available in this build",
    body: h(
      "div",
      { class: "stack-sm" },
      h(
        "p",
        { class: "field-hint" },
        "This installation does not serve ",
        h("code", { class: "inline-code" }, "GET /api/v1/staff/branding"),
        ", so there is no brand to edit. The panel keeps its built-in name and colour, and invoices keep the layout they had before branding existed — which is the same appearance an installation with no brand configured has.",
      ),
      h("div", { class: "row" }, btn("Back to the dashboard", { href: "/", size: "sm", icon: "home" })),
    ),
  });
}

// ------------------------------------------------------------------ editor --

function renderEditor(ctx: Ctx, data: BrandingResponse, reload: () => void): Node {
  const brand: Brand = data.brand ?? NO_BRAND;
  const limits = data.limits ?? DEFAULT_LIMITS;

  // The form's working copy. It is a plain object rather than reading the DOM on every
  // keystroke: the preview has to repaint on input, and a preview that reads the DOM is a
  // preview that disagrees with the field it is beside during an IME composition.
  const draft = {
    name: brand.name,
    accent_light: brand.accent_light,
    accent_dark: brand.accent_dark,
  };

  const previewHost = h("div", { class: "brand-preview-host" });
  // The preset row is rebuilt after a click so "active" follows the draft, not the stored brand.
  const presetRow = h("div");
  const statusHost = h("div", { class: "stack-sm" });

  const repaint = (): void => {
    fill(previewHost, renderPreview(draft, brand, limits));
    fill(statusHost, renderLegibility(draft, brand, limits));
  };

  const nameInput = input({
    id: "brand-name",
    name: "name",
    value: draft.name,
    maxlength: limits.name_max,
    placeholder: tx("branding.name_placeholder", "Catalyst Hosting"),
    autocomplete: "organization",
    oninput: (event: Event) => {
      draft.name = (event.target as HTMLInputElement).value;
      repaint();
    },
  });

  const lightInput = colourInput("brand-accent-light", draft.accent_light, (value) => {
    draft.accent_light = value;
    repaint();
  });
  const darkInput = colourInput("brand-accent-dark", draft.accent_dark, (value) => {
    draft.accent_dark = value;
    repaint();
  });

  /** The preset buttons, rebuilt after a click so "active" follows the draft. */
  const presetButtons = (): HTMLElement =>
    h(
      "div",
      { class: "row row-wrap" },
      ...ACCENT_PRESETS.map((preset) => {
        const accents = presetAccents(preset);
        return btn(preset.name, {
          variant: presetMatches(preset, draft) ? "primary" : "ghost",
          size: "sm",
          onclick: () => {
            // A preset fills the two accent fields and nothing else: it is a designed pair, not a
            // theme, so surfaces, text colours and layout stay the theme's own.
            draft.accent_light = accents.light;
            draft.accent_dark = accents.dark;
            lightInput.set(accents.light);
            darkInput.set(accents.dark);
            repaint();
            swap(presetRow, presetButtons());
          },
        });
      }),
    );
  fill(presetRow, presetButtons());

  const save = btn(tx("branding.save", "Save brand"), {
    variant: "primary",
    icon: "check",
    onclick: (event: MouseEvent) =>
      void withBusy(ctx, event.currentTarget as HTMLButtonElement, async () => {
        try {
          await ctx.api.put(ENDPOINT, { values: { ...draft } });
          ctx.toast(tx("branding.saved", "Brand saved"), {
            tone: "success",
            detail: tx("branding.saved_detail", "The panel and the next invoice will use it."),
          });
          // Apply it immediately rather than waiting for a reload: an operator who saved a
          // colour wants to see the chrome change, and a screen that only changes after a
          // refresh looks like the save did not work. The shell owns the brand, so it is told
          // through the same event the shell listens for; this screen never writes the panel's
          // own custom properties directly.
          publishBrand({ ...brand, ...draft });
          reload();
        } catch (err) {
          reportError(ctx, err);
        }
      }),
  });

  const reset = btn(tx("branding.reset", "Reset to the built-in appearance"), {
    variant: "ghost",
    icon: "refresh",
    onclick: (event: MouseEvent) =>
      void withBusy(ctx, event.currentTarget as HTMLButtonElement, async () => {
        try {
          // Clear, do not write the defaults back: an empty value is what "not configured"
          // means, and writing the built-in colour would make the brand a lookalike that can
          // never fall back.
          await ctx.api.put(ENDPOINT, { values: { name: "", accent_light: "", accent_dark: "" } });
          ctx.toast(tx("branding.reset_done", "Back to the built-in appearance"), { tone: "success" });
          publishBrand(NO_BRAND);
          reload();
        } catch (err) {
          reportError(ctx, err);
        }
      }),
  });

  repaint();

  const form = h(
    "form",
    {
      class: "stack",
      onsubmit: (event: Event) => {
        event.preventDefault();
        (save as HTMLButtonElement).click();
      },
    },
    field({
      label: tx("branding.name", "Display name"),
      hint: tx(
        "branding.name_hint",
        "Shown in the sidebar and above the merchant address on every invoice. Leave it empty to keep the built-in wordmark.",
      ),
      control: nameInput,
      id: "brand-name",
    }),
    h(
      "div",
      { class: "grid-2" },
      colourField(
        "brand-accent-light",
        tx("branding.accent_light", "Accent — light mode"),
        tx("branding.accent_hint", "Used for buttons, links and the focus ring while the light theme is active."),
        lightInput.element,
      ),
      colourField(
        "brand-accent-dark",
        tx("branding.accent_dark", "Accent — dark mode"),
        tx("branding.accent_hint_dark", "Used while the dark theme is active. A mid-tone that reads on white is too dark on near-black, which is why there are two."),
        darkInput.element,
      ),
    ),
    h(
      "div",
      { class: "stack-sm" },
      h("span", { class: "field-label" }, tx("branding.presets", "Presets")),
      presetRow,
      h(
        "p",
        { class: "field-hint" },
        tx(
          "branding.presets_hint",
          "A preset is a pair of accents chosen to read in both themes. It changes no token other than the accent family, so it cannot make the panel unreadable in one mode.",
        ),
      ),
    ),
    statusHost,
    h("div", { class: "row" }, save, reset),
  );

  return h(
    "div",
    { class: "stack" },
    pageHead({
      title: tx("branding.title", "Branding"),
      sub: tx(
        "branding.sub",
        "The name, logo and colour a customer sees on the panel and on their invoices.",
      ),
    }),
    banner(
      "info",
      tx("branding.scope_title", "This is a brand, not a theme"),
      h(
        "span",
        null,
        tx(
          "branding.scope_body",
          "Light and dark are unchanged and remain the operator's own choice. A brand adds an accent on top of whichever theme is active; it cannot change a surface, a text colour or a layout, so a brand cannot make the panel unreadable in one mode only.",
        ),
      ),
    ),
    h(
      "div",
      { class: "split" },
      h(
        "div",
        { class: "stack" },
        card({ title: tx("branding.card_brand", "Brand"), body: form }),
        logoCard(ctx, brand, limits, reload),
        mailBrandCard(ctx, draft.name || brand.name),
      ),
      h("div", { class: "stack" }, previewHost),
    ),
  );
}

/** One colour field: a native picker plus a text box, kept in step.
 *
 *  Both are needed. The picker is what most operators want; the text box is what makes a value
 *  copied from a brand guide usable, and what makes "empty" expressible — a colour input can
 *  never be cleared. */
function colourInput(id: string, value: string, onChange: (value: string) => void): {
  element: HTMLElement;
  set: (next: string) => void;
} {
  const picker = h("input", {
    class: "input input--color",
    type: "color",
    id: `${id}-picker`,
    value: value || "#7b95ff",
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
    placeholder: "#7b95ff",
    spellcheck: false,
    oninput: (event: Event) => {
      const next = (event.target as HTMLInputElement).value.trim();
      const parsed = /^#?[0-9a-fA-F]{3}([0-9a-fA-F]{3})?$/.test(next);
      // The picker follows only a value it can represent; a half-typed "#" must not reset the
      // picker to black while the operator is still typing.
      if (parsed) picker.value = normalise(next);
      onChange(next);
    },
  });
  return {
    element: h("div", { class: "colour-field" }, picker, text),
    // A preset writes both fields at once; the operator must see the value they just applied,
    // including in the picker, or the form and the preview disagree.
    set: (next: string) => {
      text.value = next;
      if (/^#[0-9a-fA-F]{6}$/.test(next)) picker.value = next;
    },
  };
}

function colourField(id: string, label: string, hint: string, control: HTMLElement): HTMLElement {
  return field({ label, hint, control, id });
}

/** Normalises a typed colour to `#rrggbb` for the native picker. */
function normalise(value: string): string {
  const raw = value.trim().replace(/^#/, "");
  if (raw.length === 3) return `#${raw[0]}${raw[0]}${raw[1]}${raw[1]}${raw[2]}${raw[2]}`;
  return `#${raw}`;
}

/** The legibility report: what the server says about each mode's accent.
 *
 *  This is where "a pale colour is allowed but its text is adjusted" becomes visible. It is a
 *  report rather than a refusal: a tenant's brand colour is theirs to choose. */
function renderLegibility(
  draft: { name: string; accent_light: string; accent_dark: string },
  brand: Brand,
  limits: BrandingResponse["limits"] & object,
): Node {
  const rows: Node[] = [];
  for (const mode of ["light", "dark"] as const) {
    const accent = mode === "dark" ? draft.accent_dark : draft.accent_light;
    if (!accent.trim()) continue;
    const derived = derivedFor({ ...brand, ...draft }, mode);
    const min = (limits as { min_text_contrast?: number }).min_text_contrast ?? 4.5;
    if (!derived.set) continue;
    rows.push(
      h(
        "div",
        { class: "brand-legibility-row" },
        badge(mode === "dark" ? tx("branding.mode_dark", "Dark") : tx("branding.mode_light", "Light"), "neutral"),
        derived.legible
          ? h(
              "span",
              { class: "brand-legibility-ok" },
              icon("checkCircle", 12),
              tx("branding.legible", "Readable on this theme's background ({{ratio}}:1).", {
                ratio: String(derived.contrast),
              }),
            )
          : h(
              "span",
              { class: "brand-legibility-warn" },
              icon("alertCircle", 12),
              tx(
                "branding.not_legible",
                "Only {{ratio}}:1 on this background, under the {{min}}:1 minimum. Small text and links will use {{text}} instead — the same hue, darkened until it reads.",
                { ratio: String(derived.contrast), min: String(min), text: derived.text },
              ),
            ),
      ),
    );
  }
  if (rows.length === 0) {
    return h(
      "p",
      { class: "field-hint" },
      tx(
        "branding.no_accents",
        "No accent is configured, so the panel keeps its built-in colour in both themes.",
      ),
    );
  }
  return h("div", { class: "brand-legibility" }, ...rows);
}

/** The preview: the same paint the shell applies, drawn on a mock of the chrome.
 *
 *  It calls `paintFor`, which is the function `applyBrand` writes from, so the preview cannot
 *  drift from what the panel will actually do. */
function renderPreview(
  draft: { name: string; accent_light: string; accent_dark: string },
  brand: Brand,
  limits: BrandingResponse["limits"] & object,
): Node {
  const merged: Brand = { ...brand, ...draft };
  const mode = currentMode();
  const paint = paintFor(merged, mode);
  const name = draft.name.trim() || tx("branding.preview_builtin", "Catalyst");

  const swatch = (mode: "light" | "dark"): HTMLElement => {
    const p = paintFor(merged, mode);
    return h(
      "div",
      { class: "brand-swatch" },
      h("span", { class: "brand-swatch-label" }, mode === "dark" ? tx("branding.mode_dark", "Dark") : tx("branding.mode_light", "Light")),
      h(
        "span",
        {
          class: "brand-swatch-chip",
          style: p ? { background: p.accent, color: p.ink } : undefined,
          dataset: { unset: p ? "false" : "true" },
        },
        p ? p.accent : tx("branding.builtin", "built-in"),
      ),
    );
  };

  const body = h(
    "div",
    { class: "brand-preview", dataset: { mode } },
    h(
      "div",
      { class: "brand-preview-rail" },
      h(
        "div",
        { class: "brand" },
        brand.logo_url
          ? h("img", { class: "brand-logo", src: brand.logo_url, alt: name, height: 20 })
          : h("span", { class: "brand-mark", style: paint ? { color: paint.accent } : undefined }, icon("sparkles", 17)),
        h("span", { class: "brand-name" }, name),
      ),
      h("div", { class: "brand-preview-nav" }, h("span", { class: "brand-preview-item brand-preview-item--active" }, tx("branding.preview_nav_dashboard", "Dashboard"))),
      h("div", { class: "brand-preview-nav" }, h("span", { class: "brand-preview-item" }, tx("branding.preview_nav_invoices", "Invoices"))),
    ),
    h(
      "div",
      { class: "brand-preview-main" },
      h("h3", { class: "brand-preview-title" }, tx("branding.preview_card", "A card, a button and a link")),
      h(
        "p",
        { class: "brand-preview-text" },
        tx("branding.preview_body", "Body text keeps its own colour. Only the accent changes, which is what keeps the two themes intact."),
      ),
      h(
        "div",
        { class: "row" },
        h("span", { class: "btn btn--primary brand-preview-btn" }, tx("branding.preview_action", "Primary action")),
        h("a", { class: "brand-preview-link", href: "#", onclick: (e: Event) => e.preventDefault() }, tx("branding.preview_link", "A link")),
        badge(tx("branding.preview_badge", "Badge"), "accent"),
      ),
      h(
        "div",
        { class: "brand-preview-doc" },
        h("span", { class: "brand-preview-doc-rule" }),
        h("span", { class: "brand-preview-doc-label" }, tx("branding.preview_doc", "On an invoice: the letterhead rule and the title take the same accent.")),
      ),
    ),
  );

  // The paint is applied to the preview subtree only, never to the document: a preview that
  // changed the panel's own accent while the operator typed would be a live edit with no save.
  if (paint) {
    body.style.setProperty("--accent", paint.accent);
    body.style.setProperty("--accent-ink", paint.ink);
    body.style.setProperty("--accent-text", paint.text);
    body.style.setProperty("--accent-soft", paint.soft);
    body.style.setProperty("--accent-soft-strong", paint.softStrong);
  }

  const current = currentMode();
  return card({
    title: tx("branding.preview_title", "Preview"),
    sub: tx("branding.preview_sub", "Painted with the values above, in the theme you are using now."),
    body: h(
      "div",
      { class: "stack" },
      body,
      h("div", { class: "row brand-swatches" }, swatch("light"), swatch("dark")),
      h(
        "p",
        { class: "field-hint" },
        tx("branding.preview_note", "The panel you are looking at is {{mode}}. Saving applies the brand immediately; the invoice uses the light accent, because it is printed on white paper.", {
          mode: current === "dark" ? tx("branding.mode_dark", "Dark") : tx("branding.mode_light", "Light"),
        }),
      ),
      paint
        ? null
        : h(
            "p",
            { class: "field-hint" },
            tx("branding.preview_unset", "Nothing is configured for this theme, so the built-in appearance is shown."),
          ),
    ),
  });
}

/** The logo card: upload, preview, remove. */
function logoCard(ctx: Ctx, brand: Brand, limits: BrandingResponse["limits"] & object, reload: () => void): HTMLElement {
  const accept = (limits as { logo_types?: string[] }).logo_types ?? DEFAULT_LIMITS.logo_types;
  const maxBytes = (limits as { logo_max_bytes?: number }).logo_max_bytes ?? DEFAULT_LIMITS.logo_max_bytes;

  const file = h("input", {
    class: "input",
    type: "file",
    id: "brand-logo",
    accept: accept.join(","),
  });

  const upload = btn(tx("branding.logo_upload", "Upload logo"), {
    icon: "arrowUpRight",
    onclick: (event: MouseEvent) =>
      void withBusy(ctx, event.currentTarget as HTMLButtonElement, async () => {
        const chosen = file.files?.[0];
        if (!chosen) {
          ctx.toast(tx("branding.logo_none", "Choose a file first"), { tone: "danger" });
          return;
        }
        if (chosen.size > maxBytes) {
          ctx.toast(tx("branding.logo_too_big", "That file is too large"), {
            tone: "danger",
            detail: tx("branding.logo_too_big_detail", "The limit is {{kb}} KiB.", { kb: String(Math.floor(maxBytes / 1024)) }),
          });
          return;
        }
        try {
          // The content type comes from the File, not from its extension: the server decodes
          // the bytes anyway, and this is the same value the browser would send for a form.
          await ctx.api.postRaw("/api/v1/staff/branding/logo", chosen, { contentType: chosen.type || "image/png" });
          ctx.toast(tx("branding.logo_saved", "Logo uploaded"), { tone: "success" });
          reload();
        } catch (err) {
          reportError(ctx, err);
        }
      }),
  });

  const remove = btn(tx("branding.logo_remove", "Remove logo"), {
    variant: "ghost",
    icon: "trash",
    onclick: (event: MouseEvent) =>
      void withBusy(ctx, event.currentTarget as HTMLButtonElement, async () => {
        try {
          await ctx.api.del("/api/v1/staff/branding/logo");
          ctx.toast(tx("branding.logo_removed", "Logo removed"), { tone: "success" });
          reload();
        } catch (err) {
          reportError(ctx, err);
        }
      }),
  });

  return card({
    title: tx("branding.logo_title", "Logo"),
    sub: tx("branding.logo_sub", "PNG or JPEG, up to {{kb}} KiB. It appears above the merchant name on every invoice.", {
      kb: String(Math.floor(maxBytes / 1024)),
    }),
    body: h(
      "div",
      { class: "stack-sm" },
      brand.logo_url
        ? h(
            "div",
            { class: "brand-logo-preview" },
            h("img", { src: brand.logo_url, alt: tx("branding.logo_alt", "The current brand logo"), height: 48 }),
          )
        : h(
            "p",
            { class: "field-hint" },
            tx("branding.logo_none_set", "No logo is set. The panel shows its built-in mark and the invoice prints the merchant name alone."),
          ),
      field({
        label: tx("branding.logo_file", "Choose a file"),
        hint: tx("branding.logo_hint", "PNG or JPEG only: those are the formats an invoice can print. An SVG is refused rather than accepted into a brand that would vanish from every document."),
        control: file,
        id: "brand-logo",
      }),
      h("div", { class: "row" }, upload, brand.logo_url ? remove : null),
    ),
  });
}

// ------------------------------------------------------------- mail branding --

/**
 * mailBrandCard is the overlay applied to transactional mail (NTF-11, I18N-06).
 *
 * It lives on this screen because it is the same brand: the panel, the invoice PDF and the mail are
 * three surfaces of one identity, and an operator who has just set a name and an accent should not
 * have to find a second page to make their email match. The values are the `mail.brand_*` settings
 * group, so the mail the outbox renders and the mail this card previews are the same thing.
 *
 * The body here is a sample sentence, not a real message: the point of the preview is the frame —
 * header, accent and footer — because that is what branding adds. The full render, with the
 * operator's own words, is on the notification templates screen.
 */
function mailBrandCard(ctx: Ctx, companyName: string): HTMLElement {
  const host = h("div", { class: "stack-sm" }, h("div", { class: "row" }, spinner(), h("span", { class: "field-hint" }, tx("branding.mail_loading", "Reading the mail settings…"))));

  const load = async (signal?: AbortSignal): Promise<void> => {
    try {
      const res = await ctx.api.get<{ group: string; keys: SettingsKey[] }>("/api/v1/staff/settings", {
        query: { group: "mail" },
        cache: false,
        signal,
      });
      swap(host, mailBrandForm(ctx, mailBrandFromSettings(res?.keys), companyName, () => void load()));
    } catch (err) {
      if (err instanceof DOMException && err.name === "AbortError") return;
      swap(host, h("p", { class: "field-hint" }, problemText(problemFrom(err), "The mail settings could not be read.")));
    }
  };
  void load();

  return card({
    title: tx("branding.mail_card", "Email"),
    sub: tx(
      "branding.mail_card_sub",
      "The header, accent and footer every transactional mail carries. The plain-text part is always sent unchanged.",
    ),
    body: host,
  });
}

/** mailBrandForm renders the five mail keys with a live frame. */
function mailBrandForm(
  ctx: Ctx,
  stored: MailBrandSettings,
  companyName: string,
  reload: () => void,
): HTMLElement {
  const draft = { ...stored };
  const preview = h("div");

  const repaint = (): void => {
    const accent = safeAccent(draft.accent) || "#3b5bdb";
    const header = draft.header || companyName || tx("branding.mail_header_fallback", "Your company");
    swap(
      preview,
      h(
        "div",
        {
          class: "stack-sm",
          style: `border:1px solid var(--line);border-top:3px solid ${accent};border-radius:8px;padding:12px;background:var(--surface);`,
        },
        draft.logo_url
          ? h("img", { src: draft.logo_url, alt: header, height: "20", style: "display:block;height:20px" })
          : null,
        h("strong", { style: `color:${accent}` }, header),
        h("p", { class: "field-hint" }, tx("branding.mail_sample", "The message body is the template's text, escaped and unchanged.")),
        h(
          "div",
          { class: "field-hint", style: "border-top:1px solid var(--line);padding-top:6px" },
          draft.footer ? `${draft.footer} · ` : "",
          companyName || header,
        ),
      ),
    );
  };

  const htmlToggle = h("input", {
    type: "checkbox",
    id: "mail-brand-html",
    checked: draft.html,
    onchange: (event: Event) => {
      draft.html = (event.target as HTMLInputElement).checked;
      repaint();
    },
  }) as HTMLInputElement;

  const headerInput = input({
    id: "mail-brand-header",
    value: draft.header,
    maxlength: 80,
    placeholder: companyName,
    oninput: (event: Event) => {
      draft.header = (event.target as HTMLInputElement).value;
      repaint();
    },
  });

  const footerInput = input({
    id: "mail-brand-footer",
    value: draft.footer,
    placeholder: tx("branding.mail_footer_placeholder", "Northwind Hosting · 12 Harbour Road"),
    oninput: (event: Event) => {
      draft.footer = (event.target as HTMLInputElement).value;
      repaint();
    },
  });

  const accentInput = colourInput("mail-brand-accent", draft.accent, (value) => {
    draft.accent = value;
    repaint();
  });

  const logoInput = input({
    id: "mail-brand-logo",
    type: "url",
    value: draft.logo_url,
    placeholder: "https://cdn.example.test/logo.png",
    spellcheck: false,
    oninput: (event: Event) => {
      draft.logo_url = (event.target as HTMLInputElement).value;
      repaint();
    },
  });

  const save = btn(tx("branding.mail_save", "Save email branding"), {
    variant: "primary",
    icon: "check",
    onclick: (event: MouseEvent) =>
      void withBusy(ctx, event.currentTarget as HTMLButtonElement, async () => {
        try {
          await ctx.api.put("/api/v1/staff/settings", {
            group: "mail",
            values: mailBrandToSettings(draft),
          });
          ctx.toast(tx("branding.mail_saved", "Email branding saved"), {
            tone: "success",
            detail: tx("branding.mail_saved_detail", "The next message the outbox renders will use it."),
          });
          reload();
        } catch (err) {
          reportError(ctx, err);
        }
      }),
  });

  repaint();

  return h(
    "form",
    {
      class: "stack-sm",
      onsubmit: (event: Event) => {
        event.preventDefault();
        (save as HTMLButtonElement).click();
      },
    },
    h("label", { class: "switch-row" }, htmlToggle, h("span", null, tx("branding.mail_html", "Send a branded HTML part alongside the plain text"))),
    h(
      "p",
      { class: "field-hint" },
      tx(
        "branding.mail_html_hint",
        "The plain-text part is always sent and is never changed by this. Turning this off removes the decoration only — some operators prefer a mail that renders identically in every client.",
      ),
    ),
    field({ label: tx("branding.mail_header", "Header line"), control: headerInput, id: "mail-brand-header", hint: tx("branding.mail_header_hint", "An email has no sidebar to show the wordmark, so this line is the brand. Empty uses the company name.") }),
    field({ label: tx("branding.mail_footer", "Footer"), control: footerInput, id: "mail-brand-footer", hint: tx("branding.mail_footer_hint", "A postal address, a registration number, a sentence. The company name is always shown beneath it.") }),
    colourField(
      "mail-brand-accent",
      tx("branding.mail_accent", "Accent"),
      tx("branding.mail_accent_hint", "The rule above the header and the colour of the shop's own links. Empty uses the panel's light-mode accent."),
      accentInput.element,
    ),
    field({ label: tx("branding.mail_logo", "Logo URL"), control: logoInput, id: "mail-brand-logo", hint: tx("branding.mail_logo_hint", "An absolute https URL: a mail client fetches the image from the recipient's machine, so a path on this server would arrive broken.") }),
    h("div", { class: "stack-sm" }, h("span", { class: "field-label" }, tx("branding.mail_preview", "Preview")), preview),
    h("div", { class: "row" }, save),
  );
}

// ----------------------------------------------------------------- helpers --

/**
 * Tells the shell the brand changed.
 *
 * The shell owns the brand — it fetched it, it paints the accent and it decorates the wordmark
 * — so a screen does not write the panel's custom properties itself. An event is the handoff
 * because the alternative is a view importing `app.ts`, which is the dependency the whole
 * `Ctx` contract exists to avoid.
 */
function publishBrand(brand: Brand): void {
  window.dispatchEvent(new CustomEvent<Brand>("catalyst:brand", { detail: brand }));
}

/** The theme currently on <html>, read from the attribute the shell sets. */
function currentMode(): "light" | "dark" {
  return document.documentElement.dataset.theme === "light" ? "light" : "dark";
}

/** Reports a failed save as the server described it. */
function reportError(ctx: Ctx, err: unknown): void {
  if (err instanceof ApiError) {
    // The catalogue's `error.<code>` sentence when it exists, the server's English detail
    // otherwise, and the field the refusal names — a settings save that failed on one key should
    // say which key, in whatever language the operator is reading.
    const problem = problemFrom(err) ?? { code: err.code, detail: err.detail };
    const fields = problem.fields?.length
      ? " " + problem.fields.map((field) => problemFieldText(field)).join(" ")
      : "";
    ctx.toast(problemText(problem, tx("branding.failed", "That did not work")) + fields, {
      tone: "danger",
      detail: problem.code,
    });
    return;
  }
  ctx.toast(tx("branding.failed", "That did not work"), { tone: "danger", detail: String(err) });
}
