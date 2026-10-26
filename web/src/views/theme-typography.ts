// Theme Studio — typography.
//
// Three families and one scale. The families are display, body and mono, and each is a *stack*,
// not a name: the server accepts either an installed family or a complete CSS stack, because the
// built-in theme names the system UI font rather than a self-hosted one. So each row is a
// select of the installed families beside a free-text stack — the select is what most operators
// want, and the text box is what makes `ui-sans-serif, -apple-system, …` expressible.
//
// The eight-step scale is the part that decides whether a screen reads as designed or as
// default. It is edited as eight numbers with their units (`rem` by default), and the preview
// beside it renders the same eight steps, so a scale that collapses at `md` is visible before it
// is saved rather than on a customer's invoice list.
//
// # Why the weights are a text field
//
// A theme's weights are the set the generator reads when it decides which `@font-face`
// declarations a self-hosted family needs. A checkbox per weight would be nine controls per
// family for a value most themes set once; a comma-separated list is one field and it is what
// the document stores. The field parses and refuses anything that is not a weight, so a typo
// cannot become a silent 0-weight font.
//
// # What the preview is painted with
//
// Custom properties on the preview subtree only — never the document root. The same rule the
// colour screen's preview follows: an unsaved value is shown in the preview and nowhere else.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { tx } from "../lib/i18n";
import type { Ctx } from "../lib/types";
import {
  adoptSavedRevision,
  adoptScope,
  cloneDocument,
  isDirty,
  isScopeName,
  loadScope,
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
  unavailableCard,
  writePath,
  type FontSpec,
  type ScopeResponse,
  type ThemeDocument,
} from "../lib/theme-studio";
import { badge, banner, btn, card, field, input, resource, skeletonPage, select, withBusy } from "../ui/kit";

export const meta = { title: "Theme typography", crumb: "Typography" };

/** The eight steps, in the order the stylesheet defines them. */
const SCALE_STEPS: ReadonlyArray<{ key: string; label: string; sample: string }> = [
  { key: "xs", label: "xs", sample: "0.6875rem" },
  { key: "sm", label: "sm", sample: "0.75rem" },
  { key: "base", label: "base", sample: "0.8125rem" },
  { key: "md", label: "md", sample: "0.875rem" },
  { key: "lg", label: "lg", sample: "1rem" },
  { key: "xl", label: "xl", sample: "1.125rem" },
  { key: "2xl", label: "2xl", sample: "1.5rem" },
  { key: "3xl", label: "3xl", sample: "2.25rem" },
];

/** The three family roles. */
const FAMILY_ROLES: ReadonlyArray<{ key: "display" | "body" | "mono"; label: string; hint: string }> = [
  {
    key: "display",
    label: "Display",
    hint: "Headings and page titles. A serif here is what makes Editorial Paper and Midnight Luxe read as different products.",
  },
  {
    key: "body",
    label: "Body",
    hint: "Every sentence in the panel. It is the family most operators should leave alone: the system UI font is the fastest and the most familiar.",
  },
  {
    key: "mono",
    label: "Monospace",
    hint: "Amounts, invoice numbers, API keys and digests. A proportional font here makes a column of figures impossible to scan.",
  },
];

export default async function themeTypographyPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("theme:read")) {
    return studioDenied("theme:read", tx("theme.type.what", "The typography editor"));
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
                tx("theme.type.unavailable", "This installation does not serve GET /api/v1/staff/theme/{scope}, so there is no type scale to edit."),
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
      active: "typography",
      title: tx("theme.type.title", "Typography"),
      sub: tx("theme.type.sub", "Three families, eight steps, three line heights and two letter-spacing values. The preview shows the scale as a screen would."),
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
  const canWrite = state.permissions.can_write !== false;

  const previewHost = h("div");
  const dirtyNote = h("span", { class: "field-hint" });

  const repaint = (): void => {
    swap(previewHost, previewCard(doc));
    dirtyNote.textContent = isDirty()
      ? tx("theme.type.unsaved", "Unsaved changes")
      : tx("theme.type.saved", "Everything here is saved");
  };

  /** A text field bound to a dotted path. An empty value removes the key, which means inherit. */
  const textField = (path: string, label: string, hint?: string, placeholder?: string): HTMLElement => {
    const id = `theme-${path.replace(/[^a-zA-Z0-9]+/g, "-")}`;
    const control = input({
      id,
      value: readPath(doc, path),
      placeholder,
      spellcheck: false,
      oninput: (event: Event) => {
        const clean = (event.target as HTMLInputElement).value.trim();
        writePath(doc, path, clean === "" ? undefined : clean);
        repaint();
      },
    });
    return field({ label, hint, control, id });
  };

  /**
   * The weights field.
   *
   * It is not a plain text path field because the document stores an array of numbers, and a
   * field that wrote the typed string back would be refused by the server on every save. It
   * parses what is typed and only writes when the whole list is valid, so a half-typed "40"
   * leaves the stored list alone rather than replacing it with a weight the generator rejects.
   */
  const weightsField = (role: { key: "display" | "body" | "mono" }): HTMLElement => {
    const path = `typography.${role.key}.weights`;
    const id = `theme-${role.key}-weights`;
    const current = doc.typography[role.key]?.weights ?? [];
    const control = input({
      id,
      value: current.join(", "),
      placeholder: "400, 500",
      spellcheck: false,
      oninput: (event: Event) => {
        const parsed = parseWeights((event.target as HTMLInputElement).value);
        if (parsed) writePath(doc, path, parsed);
        repaint();
      },
    });
    return field({
      label: tx("theme.type.weights", "Weights"),
      control,
      id,
      hint: tx("theme.type.weights_hint", "Comma-separated, in steps of 100 from 100 to 900. It decides which weights a self-hosted family needs."),
    });
  };

  const familyRow = (role: { key: "display" | "body" | "mono"; label: string; hint: string }): HTMLElement => {
    const path = `typography.${role.key}.family`;
    const current = readPath(doc, path);
    const families = state.limits.families ?? [];
    // A document may name a complete stack rather than an installed family. The select then has
    // to carry that value as an option, or it would silently show the first family in the list
    // and the next save would replace the operator's stack with it.
    const options = families.map((family) => ({ value: family, label: family }));
    if (current && !families.includes(current)) {
      options.unshift({ value: current, label: tx("theme.type.custom", "Custom stack") });
    }
    const picker = select(options, {
      id: `theme-${role.key}-family`,
      value: current,
      onchange: (event: Event) => {
        writePath(doc, path, (event.target as HTMLSelectElement).value);
        stack.value = readPath(doc, path);
        repaint();
      },
    });
    const stack = input({
      id: `theme-${role.key}-stack`,
      value: current,
      placeholder: "ui-sans-serif, -apple-system, …",
      spellcheck: false,
      oninput: (event: Event) => {
        const clean = (event.target as HTMLInputElement).value.trim();
        writePath(doc, path, clean === "" ? undefined : clean);
        repaint();
      },
    });
    return h(
      "div",
      { class: "stack-sm theme-family" },
      h("span", { class: "theme-group-title" }, tx(`theme.type.${role.key}`, role.label)),
      h(
        "div",
        { class: "grid-2" },
        field({ label: tx("theme.type.family", "Installed family"), control: picker, id: `theme-${role.key}-family` }),
        field({
          label: tx("theme.type.stack", "Family stack"),
          control: stack,
          id: `theme-${role.key}-stack`,
          hint: tx("theme.type.stack_hint", "What the browser actually uses. Edit it to name a font this installation serves itself."),
        }),
      ),
      h(
        "div",
        { class: "grid-2" },
        textField(`typography.${role.key}.fallback`, tx("theme.type.fallback", "Fallback"), tx("theme.type.fallback_hint", "Appended after the family, so a missing font degrades to something close."), "system-ui, sans-serif"),
        weightsField(role),
      ),
      h("p", { class: "field-hint" }, role.hint),
    );
  };

  const save = btn(tx("theme.type.save", "Save draft"), {
    variant: "primary",
    icon: "check",
    disabled: !canWrite,
    onclick: (event: MouseEvent) =>
      void withBusy(ctx, event.currentTarget as HTMLButtonElement, async () => {
        try {
          const rev = await saveDraft(ctx.api, state, doc);
          adoptSavedRevision(rev);
          ctx.toast(tx("theme.type.saved_toast", "Draft {{number}} saved", { number: rev.number }), {
            tone: "success",
            detail: tx("theme.type.saved_detail", "Publish it from History when you are happy with it."),
          });
          reload();
        } catch (err) {
          reportStudioError(ctx, err, tx("theme.type.failed", "The type scale could not be saved"));
        }
      }),
  });

  const discard = btn(tx("theme.type.discard", "Discard changes"), {
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
      title: tx("theme.type.families", "Families"),
      sub: tx("theme.type.families_sub", "The installed set is closed: a family outside it is refused, because the browser would silently fall back to its default serif and the theme would look broken in a way nobody can diagnose."),
      body: h("div", { class: "stack" }, ...FAMILY_ROLES.map(familyRow)),
    }),
    card({
      title: tx("theme.type.scale", "Scale"),
      sub: tx("theme.type.scale_sub", "Eight steps. A unit is required — rem is the default, px is what the built-in theme uses for its smallest steps."),
      body: h(
        "div",
        { class: "grid-4 theme-scale" },
        ...SCALE_STEPS.map((step) => textField(`typography.scale.${step.key}`, step.label, undefined, step.sample)),
      ),
    }),
    card({
      title: tx("theme.type.leading", "Leading and tracking"),
      sub: tx("theme.type.leading_sub", "Line heights are unitless multipliers; tracking is an em value. Both are applied by the stylesheet, not by a component."),
      body: h(
        "div",
        { class: "grid-3" },
        textField("typography.leading.tight", tx("theme.type.leading_tight", "Leading, tight"), tx("theme.type.leading_tight_hint", "Headings."), "1.15"),
        textField("typography.leading.snug", tx("theme.type.leading_snug", "Leading, snug"), tx("theme.type.leading_snug_hint", "Table rows and dense lists."), "1.35"),
        textField("typography.leading.normal", tx("theme.type.leading_normal", "Leading, normal"), tx("theme.type.leading_normal_hint", "Body copy."), "1.45"),
        textField("typography.tracking.tight", tx("theme.type.tracking_tight", "Tracking, tight"), tx("theme.type.tracking_tight_hint", "Large headings, which need negative tracking to look set."), "-0.015em"),
        textField("typography.tracking.wide", tx("theme.type.tracking_wide", "Tracking, wide"), tx("theme.type.tracking_wide_hint", "Eyebrows and small capitals."), "0.06em"),
      ),
    }),
    h("div", { class: "row row-wrap theme-save-row" }, canWrite ? save : null, canWrite ? discard : null, dirtyNote),
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
          tx("theme.type.read_only", "This session can read the type system but not save it"),
          tx("theme.type.read_only_body", "Saving a draft needs theme:write. The values below are what is live or what the open draft holds."),
        ),
    h("div", { class: "split" }, form, h("div", { class: "stack theme-side" }, previewHost)),
  );
}

// ------------------------------------------------------------ the preview --

/**
 * The scale, rendered.
 *
 * Every value comes from the document being edited, so a step that is smaller than the one
 * below it is visible here as a heading that is quieter than its own body text — which is the
 * failure a column of eight `rem` values hides.
 */
function previewCard(doc: ThemeDocument): HTMLElement {
  const scale = doc.typography.scale;
  const body = h(
    "div",
    { class: "theme-type-preview" },
    h("span", { class: "theme-type-preview-label" }, tx("theme.type.preview_display", "Display")),
    h("p", { class: "theme-type-preview-display" }, tx("theme.type.preview_heading", "Invoices, services and the ledger")),
    h("span", { class: "theme-type-preview-label" }, tx("theme.type.preview_body", "Body")),
    h(
      "p",
      { class: "theme-type-preview-body" },
      tx(
        "theme.type.preview_sentence",
        "An invoice line is read at a glance: a description, a quantity, a price. The body family is the one a person reads for eight hours, which is why the built-in theme names the system UI font rather than shipping one.",
      ),
    ),
    h("span", { class: "theme-type-preview-label" }, tx("theme.type.preview_mono", "Monospace")),
    h(
      "p",
      { class: "theme-type-preview-mono" },
      tx("theme.type.preview_mono_sample", "INV-000042 · 1,240.00 USD · sha256:9f2c…"),
    ),
    h(
      "div",
      { class: "theme-type-scale" },
      ...SCALE_STEPS.map((step) =>
        h(
          "div",
          { class: "theme-type-step" },
          h("span", { class: "theme-type-step-label" }, step.label),
          h(
            "span",
            { class: `theme-type-step-sample theme-type-step-sample--${step.key}` },
            tx("theme.type.preview_sample", "Aa 0123"),
          ),
        ),
      ),
    ),
  );

  // The three stacks, composed the way the generator composes them: family, then fallback.
  body.style.setProperty("--preview-font-display", stackOf(doc.typography.display));
  body.style.setProperty("--preview-font-body", stackOf(doc.typography.body));
  body.style.setProperty("--preview-font-mono", stackOf(doc.typography.mono));
  for (const step of SCALE_STEPS) {
    const size = scale[step.key as keyof typeof scale];
    if (typeof size === "string" && size.trim()) {
      body.style.setProperty(`--preview-text-${step.key}`, size.trim());
    }
  }
  const leading = doc.typography.leading;
  if (leading?.normal) body.style.setProperty("--preview-leading", leading.normal);

  return card({
    title: tx("theme.type.preview_title", "Preview"),
    sub: tx("theme.type.preview_sub", "The three families and the eight steps, painted with the values in the form."),
    actions: badge(tx("theme.type.preview_badge", "Unsaved values"), "neutral"),
    body,
  });
}

/** A family plus its fallback, as one CSS value. */
function stackOf(spec: FontSpec | undefined): string {
  const family = (spec?.family ?? "").trim();
  const fallback = (spec?.fallback ?? "").trim();
  if (family && fallback) return `${family}, ${fallback}`;
  return family || fallback || "inherit";
}

/**
 * Parses a typed weight list, or null when any entry is not a weight.
 *
 * All-or-nothing on purpose: writing the valid prefix of "400, 500, 7" would save a theme whose
 * weights are not what the operator is looking at, and the server would refuse the whole save
 * anyway with a message about the third entry.
 */
function parseWeights(raw: string): number[] | null {
  const parts = raw
    .split(/[,\s]+/)
    .map((part) => part.trim())
    .filter(Boolean);
  if (parts.length === 0) return null;
  const weights: number[] = [];
  for (const part of parts) {
    const value = Number(part);
    if (!Number.isInteger(value) || value < 100 || value > 900 || value % 100 !== 0) return null;
    weights.push(value);
  }
  return weights;
}
