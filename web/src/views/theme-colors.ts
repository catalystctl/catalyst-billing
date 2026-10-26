// Theme Studio — colours.
//
// The colour screen is where a theme stops being a preset and becomes the operator's own. It
// edits three things, in the order the stylesheet resolves them:
//
//   1. `color.mode` — which mode a *first* visit opens in. It is not "the mode I am looking at":
//      an operator who has chosen dark in the sidebar keeps dark, and this value decides what a
//      stranger sees. The hint says so, because the two are easy to confuse.
//   2. `color.brand` and `color.semantic` — the mode-independent seed colours. The generator
//      derives hover/press/soft tints from the brand seed, which is why there is no hover field
//      here for the seed and there is one per mode in the palette below.
//   3. `color.dark` and `color.light` — the full surface set per mode. They are separate values
//      rather than one value with an opacity because a panel and a piece of paper are different
//      colour spaces: `#ff951e` reads on `#171413` and disappears on `#f5f2ee`.
//
// # The contrast meter
//
// A palette that looks fine on the operator's monitor can be unreadable on a laptop in a bright
// room, and the failure is always in the same six places: body text, secondary text, a hint, a
// control's border, the accent as a link, and the label inside an accent-filled button. The
// meter measures exactly those, in both modes, against the thresholds each one has to meet
// (4.5:1 for body text, 3:1 for large text and UI boundaries, 1.4:1 for a decorative rule) and
// says PASS or FAIL per pair.
//
// It is a meter, not a gate. The server validates contrast when a document is published, and
// its answer is the one that decides; this exists so an operator can see the problem while
// typing rather than after a failed save. It never refuses a colour — a brand's own hue is the
// brand's business, and an illegible one is a decision to make knowingly.
//
// # Why the form is built once and only the meter repaints
//
// Forty-five colour fields rebuilt on every keystroke would lose focus, the caret and the
// picker's drag state. So the inputs are created once, the working document is mutated in
// place, and only the two live regions — the meter and the preview — are re-rendered.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { titleCase } from "../lib/format";
import { tx } from "../lib/i18n";
import type { Ctx } from "../lib/types";
import {
  adoptSavedRevision,
  adoptScope,
  cloneDocument,
  colourInput,
  contrastLabel,
  contrastReport,
  currentMode,
  isDirty,
  isScopeName,
  loadScope,
  paintPreview,
  paletteFor,
  readPath,
  reportStudioError,
  saveDraft,
  scopeNote,
  setStudioDocument,
  scopePicker,
  setStudioScope,
  studioDenied,
  studioHead,
  studioState,
  swatchRow,
  unavailableCard,
  writePath,
  type ContrastResult,
  type ModePalette,
  type ScopeResponse,
  type ThemeDocument,
} from "../lib/theme-studio";
import { badge, banner, btn, card, field, resource, skeletonPage, select, withBusy } from "../ui/kit";

export const meta = { title: "Theme colours", crumb: "Colours" };

export default async function themeColorsPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("theme:read")) {
    return studioDenied("theme:read", tx("theme.colors.what", "The colour editor"));
  }

  let scope = studioState().scope;
  const requested = ctx.route.query.get("scope");
  if (isScopeName(requested)) {
    scope = requested;
    setStudioScope(requested);
  }

  const host = h("div");
  const paint = (): void => {
    swap(
      host,
      resource(
        ctx,
        async (signal) => {
          try {
            return await loadScope(ctx.api, scope, signal);
          } catch (err) {
            if (err instanceof ApiError && (err.status === 404 || err.status === 501)) {
              return "unavailable" as const;
            }
            throw err;
          }
        },
        (data) =>
          data === "unavailable"
            ? unavailableCard(
                tx("theme.colors.unavailable", "This installation does not serve GET /api/v1/staff/theme/{scope}, so there is no palette to edit."),
              )
            : renderEditor(ctx, data, paint),
        { skeleton: skeletonPage },
      ),
    );
  };
  paint();

  return h(
    "div",
    { class: "page" },
    studioHead({
      active: "colors",
      title: tx("theme.colors.title", "Colours"),
      sub: tx(
        "theme.colors.sub",
        "Both colour modes, the brand family and the four signals. The meter beside the form measures the pairs a person actually reads.",
      ),
    }),
    h(
      "div",
      { class: "theme-toolbar" },
      scopePicker(scope, (next) => {
        setStudioScope(next);
        scope = next;
        paint();
      }),
      scopeNote(scope),
    ),
    host,
  );
}

// --------------------------------------------------------------- the editor --

function renderEditor(ctx: Ctx, data: ScopeResponse, reload: () => void): Node {
  adoptScope(data);
  const state = studioState();
  const doc: ThemeDocument = state.document ?? cloneDocument(data.document);
  const limits = state.limits;
  const canWrite = state.permissions.can_write !== false;

  // The two live regions. Everything else in this screen is built once.
  const meterHost = h("div");
  const previewHost = h("div");
  const dirtyNote = h("span", { class: "field-hint" });

  const repaint = (): void => {
    swap(meterHost, meterCard(doc));
    swap(previewHost, previewCard(doc));
    dirtyNote.textContent = isDirty()
      ? tx("theme.colors.unsaved", "Unsaved changes")
      : tx("theme.colors.saved", "Everything here is saved");
  };

  /** One colour field. An emptied box means "inherit", so it removes the key rather than
   *  writing an empty string the generator would have to special-case. */
  const colourField = (path: string, label: string, hint?: string): HTMLElement => {
    const id = `theme-${path.replace(/[^a-zA-Z0-9]+/g, "-")}`;
    const input = colourInput(id, readPath(doc, path), (value) => {
      const clean = value.trim();
      writePath(doc, path, clean === "" ? undefined : clean);
      repaint();
    });
    return field({ label, hint, control: input, id });
  };

  const grid = (...children: HTMLElement[]): HTMLElement => h("div", { class: "grid-3 theme-colour-grid" }, ...children);

  const modeSelect = select(
    (limits.modes ?? ["dark", "light", "system"]).map((mode) => ({ value: mode, label: titleCase(mode) })),
    {
      id: "theme-color-mode",
      value: doc.color.mode ?? "dark",
      onchange: (event: Event) => {
        doc.color.mode = (event.target as HTMLSelectElement).value;
        repaint();
      },
    },
  );

  const save = btn(tx("theme.colors.save", "Save draft"), {
    variant: "primary",
    icon: "check",
    disabled: !canWrite,
    onclick: (event: MouseEvent) =>
      void withBusy(ctx, event.currentTarget as HTMLButtonElement, async () => {
        try {
          const rev = await saveDraft(ctx.api, state, doc);
          adoptSavedRevision(rev);
          ctx.toast(tx("theme.colors.saved_toast", "Draft {{number}} saved", { number: rev.number }), {
            tone: "success",
            detail: tx("theme.colors.saved_detail", "Publish it from History when you are happy with it."),
          });
          reload();
        } catch (err) {
          reportStudioError(ctx, err, tx("theme.colors.failed", "The palette could not be saved"));
        }
      }),
  });

  const discard = btn(tx("theme.colors.discard", "Discard changes"), {
    variant: "ghost",
    icon: "undo",
    disabled: !canWrite,
    onclick: () => {
      setStudioDocument(cloneDocument(state.base ?? data.document));
      reload();
    },
  });

  const form = h(
    "div",
    { class: "stack" },
    card({
      title: tx("theme.colors.mode_title", "Default mode"),
      sub: tx("theme.colors.mode_sub", "What a first visit opens in. An operator's own choice in the sidebar always wins over this."),
      body: h(
        "div",
        { class: "grid-2" },
        field({
          label: tx("theme.colors.mode", "Colour mode"),
          control: modeSelect,
          id: "theme-color-mode",
          hint: tx(
            "theme.colors.mode_hint",
            "System follows the visitor's operating system. The published theme's boot script applies this before the first paint, so there is no flash of the wrong mode.",
          ),
        }),
        h(
          "div",
          { class: "stack-sm" },
          h("span", { class: "field-label" }, tx("theme.colors.current", "Previewed in")),
          badge(titleCase(currentMode()), "accent"),
          h("span", { class: "field-hint" }, tx("theme.colors.current_hint", "The preview below is painted in the mode this browser is using now.")),
        ),
      ),
    }),
    card({
      title: tx("theme.colors.brand_title", "Brand family"),
      sub: tx("theme.colors.brand_sub", "Mode-independent seed colours. Hover, press and soft tints are derived from the accent."),
      body: grid(
        colourField("color.brand.primary", tx("theme.colors.brand_primary", "Primary")),
        colourField("color.brand.secondary", tx("theme.colors.brand_secondary", "Secondary")),
        colourField("color.brand.accent", tx("theme.colors.brand_accent", "Accent")),
      ),
    }),
    card({
      title: tx("theme.colors.semantic_title", "Signals"),
      sub: tx("theme.colors.semantic_sub", "Success, warning, danger and information. In a billing panel colour is a signal, so these are not decorative."),
      body: grid(
        colourField("color.semantic.success", tx("theme.colors.success", "Success")),
        colourField("color.semantic.warning", tx("theme.colors.warning", "Warning")),
        colourField("color.semantic.danger", tx("theme.colors.danger", "Danger")),
        colourField("color.semantic.info", tx("theme.colors.info", "Information")),
        colourField("color.semantic.focus", tx("theme.colors.focus", "Focus ring")),
      ),
    }),
    paletteCard("dark", doc, colourField),
    paletteCard("light", doc, colourField),
    h(
      "div",
      { class: "row row-wrap theme-save-row" },
      canWrite ? save : null,
      canWrite ? discard : null,
      dirtyNote,
    ),
  );

  if (!canWrite) {
    for (const control of form.querySelectorAll<HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement>("input, select, textarea")) {
      control.disabled = true;
    }
  }

  repaint();

  return h(
    "div",
    { class: "stack" },
    canWrite
      ? null
      : banner(
          "info",
          tx("theme.colors.read_only", "This session can read the palette but not save it"),
          tx("theme.colors.read_only_body", "Saving a draft needs theme:write. The values below are what is live or what the open draft holds."),
        ),
    data.drafts.length === 0 && !data.published
      ? banner(
          "neutral",
          tx("theme.colors.builtin", "This scope is serving the built-in default"),
          tx("theme.colors.builtin_body", "Saving here opens the first draft for this scope. Nothing changes on the live surface until it is published."),
        )
      : null,
    h("div", { class: "split" }, form, h("div", { class: "stack theme-side" }, previewHost, meterHost)),
  );
}

/** One mode's palette, grouped the way the stylesheet groups it. */
function paletteCard(
  mode: "dark" | "light",
  doc: ThemeDocument,
  colourField: (path: string, label: string, hint?: string) => HTMLElement,
): HTMLElement {
  const prefix = `color.${mode}`;
  const palette = mode === "dark" ? doc.color.dark : doc.color.light;
  const grid = (...children: HTMLElement[]): HTMLElement => h("div", { class: "grid-3 theme-colour-grid" }, ...children);
  const group = (title: string, ...children: HTMLElement[]): HTMLElement =>
    h("div", { class: "stack-sm" }, h("span", { class: "theme-group-title" }, title), grid(...children));

  return card({
    title: mode === "dark" ? tx("theme.colors.dark_title", "Dark mode") : tx("theme.colors.light_title", "Light mode"),
    sub:
      mode === "dark"
        ? tx("theme.colors.dark_sub", "The panel, the portal and the storefront while the dark mode is active.")
        : tx("theme.colors.light_sub", "The same surfaces for the light mode, and the palette an invoice is printed with."),
    actions: swatchRow(
      [palette.bg, palette.card, palette.text, palette.accent].filter((value): value is string => Boolean(value)),
      tx("theme.colors.swatches", "{{mode}}: background, card, text and accent", { mode: titleCase(mode) }),
    ),
    body: h(
      "div",
      { class: "stack" },
      group(
        tx("theme.colors.group_ground", "Ground and surfaces"),
        colourField(`${prefix}.bg`, tx("theme.colors.bg", "Background")),
        colourField(`${prefix}.popover`, tx("theme.colors.popover", "Popover")),
        colourField(`${prefix}.overlay`, tx("theme.colors.overlay", "Overlay")),
        colourField(`${prefix}.card`, tx("theme.colors.card", "Card")),
        colourField(`${prefix}.surface1`, tx("theme.colors.surface1", "Surface 1")),
        colourField(`${prefix}.surface2`, tx("theme.colors.surface2", "Surface 2")),
        colourField(`${prefix}.surface3`, tx("theme.colors.surface3", "Surface 3")),
      ),
      group(
        tx("theme.colors.group_lines", "Lines"),
        colourField(`${prefix}.border`, tx("theme.colors.border", "Border")),
        colourField(`${prefix}.borderStrong`, tx("theme.colors.border_strong", "Border, strong")),
        colourField(`${prefix}.input`, tx("theme.colors.input", "Input")),
      ),
      group(
        tx("theme.colors.group_text", "Text"),
        colourField(`${prefix}.text`, tx("theme.colors.text", "Text")),
        colourField(`${prefix}.textMuted`, tx("theme.colors.text_muted", "Text, muted")),
        colourField(`${prefix}.textFaint`, tx("theme.colors.text_faint", "Text, faint")),
      ),
      group(
        tx("theme.colors.group_accent", "Accent family"),
        colourField(`${prefix}.accent`, tx("theme.colors.accent", "Accent")),
        colourField(`${prefix}.accentHover`, tx("theme.colors.accent_hover", "Accent, hover")),
        colourField(`${prefix}.accentPress`, tx("theme.colors.accent_press", "Accent, pressed")),
        colourField(`${prefix}.accentInk`, tx("theme.colors.accent_ink", "Ink on accent")),
      ),
      group(
        tx("theme.colors.group_signals", "Signals"),
        colourField(`${prefix}.success`, tx("theme.colors.success", "Success")),
        colourField(`${prefix}.warning`, tx("theme.colors.warning", "Warning")),
        colourField(`${prefix}.danger`, tx("theme.colors.danger", "Danger")),
        colourField(`${prefix}.info`, tx("theme.colors.info", "Information")),
      ),
      h(
        "p",
        { class: "field-hint" },
        tx(
          "theme.colors.legacy_note",
          "The legacy names tokens.css already defines (side, top, surfaceHover, lineSoft and the rest) are derived from these values and preserved untouched.",
        ),
      ),
    ),
  });
}

// -------------------------------------------------------------- the meter --

/**
 * The contrast report for both modes.
 *
 * Both, not just the mode being previewed: a palette is edited in the mode the operator is
 * using, and the failure that reaches a customer is in the other one.
 */
function meterCard(doc: ThemeDocument): HTMLElement {
  const section = (mode: "dark" | "light", palette: ModePalette): HTMLElement => {
    const results = contrastReport(palette);
    return h(
      "div",
      { class: "stack-sm" },
      h(
        "div",
        { class: "row row-between" },
        h("span", { class: "theme-group-title" }, mode === "dark" ? tx("theme.colors.dark_title", "Dark mode") : tx("theme.colors.light_title", "Light mode")),
        results.some((result) => !result.pass)
          ? badge(tx("theme.colors.failing", "{{count}} failing", { count: results.filter((result) => !result.pass).length }), "warning")
          : badge(tx("theme.colors.passing", "All pass"), "success"),
      ),
      results.length === 0
        ? h("p", { class: "field-hint" }, tx("theme.colors.meter_empty", "No pair could be measured: the palette's colours are missing or are var() references."))
        : h("div", { class: "theme-contrast" }, ...results.map(contrastRow)),
    );
  };

  return card({
    title: tx("theme.colors.meter_title", "Contrast"),
    sub: tx(
      "theme.colors.meter_sub",
      "WCAG ratios for the pairs a person reads. Body text 4.5:1, large text and UI boundaries 3:1, decorative rules 1.4:1. The built-in default predates this gate, so it can report a failure that a preset would not be allowed to ship.",
    ),
    marker: "accent",
    body: h("div", { class: "stack" }, section("dark", doc.color.dark), section("light", doc.color.light)),
  });
}

/** One measured pair: the two colours, the ratio and the verdict. */
function contrastRow(result: ContrastResult): HTMLElement {
  const threshold =
    result.kind === "body"
      ? tx("theme.colors.kind_body", "body text")
      : result.kind === "ui"
        ? tx("theme.colors.kind_ui", "large text and UI")
        : tx("theme.colors.kind_line", "decorative rule");
  return h(
    "div",
    { class: "theme-contrast-row", dataset: { pass: result.pass ? "true" : "false" } },
    swatchRow([result.fg, result.bg], tx("theme.colors.pair", "{{fg}} on {{bg}}", { fg: result.fg, bg: result.bg })),
    h(
      "div",
      { class: "theme-contrast-text" },
      h("strong", null, contrastLabel(result.key)),
      h("span", { class: "field-hint" }, tx("theme.colors.threshold", "needs {{ratio}}:1 for {{kind}}", { ratio: String(result.threshold), kind: threshold })),
    ),
    h(
      "span",
      { class: "theme-contrast-ratio" },
      `${result.ratio}:1`,
      badge(result.pass ? tx("theme.colors.pass", "PASS") : tx("theme.colors.fail", "FAIL"), result.pass ? "success" : "danger"),
    ),
  );
}

// ------------------------------------------------------------ the preview --

/**
 * A card, a button, a link and a badge painted with the palette.
 *
 * The values are written as custom properties on this subtree by `paintPreview`, which is the
 * same narrow mechanism the branding screen's preview uses. It is not a live edit: nothing here
 * touches the document root, so an operator can type a colour and see it without saving it.
 */
function previewCard(doc: ThemeDocument): HTMLElement {
  const mode = currentMode();
  const palette = paletteFor(doc, mode);
  const body = h(
    "div",
    { class: "theme-preview", dataset: { mode } },
    h(
      "div",
      { class: "theme-preview-head" },
      h("strong", null, tx("theme.colors.preview_title_text", "A card, a button and a link")),
      h("span", { class: "theme-preview-badge" }, tx("theme.colors.preview_badge", "Badge")),
    ),
    h(
      "p",
      { class: "theme-preview-text" },
      tx("theme.colors.preview_body", "Body text keeps its own colour. Secondary text and hints are the two pairs that fail most often, which is why the meter checks them first."),
    ),
    h("p", { class: "theme-preview-hint" }, tx("theme.colors.preview_hint", "A hint or a caption, in the faint colour.")),
    h(
      "div",
      { class: "row row-wrap" },
      h("span", { class: "theme-preview-btn" }, tx("theme.colors.preview_action", "Primary action")),
      h("a", { class: "theme-preview-link", href: "#", onclick: (event: Event) => event.preventDefault() }, tx("theme.colors.preview_link", "A link")),
    ),
    h("div", { class: "theme-preview-rule" }),
  );
  paintPreview(body, palette);

  return card({
    title: tx("theme.colors.preview_title", "Preview"),
    sub: tx("theme.colors.preview_sub", "Painted with the values in the form, in the {{mode}} mode this browser is using.", {
      mode: mode === "dark" ? tx("theme.colors.dark", "dark") : tx("theme.colors.light", "light"),
    }),
    body,
  });
}
