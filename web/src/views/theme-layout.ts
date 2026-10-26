// Theme Studio — layout.
//
// This is the screen that makes a preset a *design* rather than a recolour. Two themes with the
// same palette and a different navigation, density and corner radius read as different products:
// a rail with a 14px radius and roomy spacing is a different claim about the software than a
// 232px sidebar with 4px corners and compact rows.
//
// It edits four groups, in the order the stylesheet consumes them:
//
//   1. `layout.shell` — navigation, the three widths, card style, table style and the two
//      optional chrome features. These become CSS variables plus one `data-` attribute, which is
//      why a preset can change the *shape* of the panel without a second stylesheet.
//   2. `shape` — the six-step radius scale and the border width. `full` is the pill radius and
//      is deliberately not subject to the 64px bound the others have.
//   3. `motion` — three durations and one easing curve. Bounded, because a 2s transition is a
//      theme that makes the panel feel broken.
//   4. `background` — the page treatment and its optional gradient. An image is an asset
//      reference (`theme-asset:`), never a URL, and this build has no asset library yet, so the
//      image field is shown read-only rather than as a box that would be refused.
//
// The diagram is not decoration. `sidebar`, `rail` and `topbar` are three genuinely different
// shells, and the difference is the one thing an operator cannot infer from a select's label —
// so the picture changes with the choice, and the rail's width changes with the width field.
//
// Nothing here is a colour: a layout value that also had a colour would be two decisions in one
// field, and the colour screen owns those.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { titleCase } from "../lib/format";
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
  type ScopeResponse,
  type ThemeDocument,
} from "../lib/theme-studio";
import { badge, banner, btn, card, field, input, resource, select, skeletonPage, switchField, withBusy } from "../ui/kit";

export const meta = { title: "Theme layout", crumb: "Layout" };

export default async function themeLayoutPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("theme:read")) {
    return studioDenied("theme:read", tx("theme.layout.what", "The layout editor"));
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
                tx("theme.layout.unavailable", "This installation does not serve GET /api/v1/staff/theme/{scope}, so there is no shell to edit."),
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
      active: "layout",
      title: tx("theme.layout.title", "Layout"),
      sub: tx("theme.layout.sub", "The shell: navigation, widths, density, card and table style, radius, motion and the page background."),
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

  const diagramHost = h("div");
  const dirtyNote = h("span", { class: "field-hint" });

  const repaint = (): void => {
    swap(diagramHost, shellDiagram(doc));
    dirtyNote.textContent = isDirty()
      ? tx("theme.layout.unsaved", "Unsaved changes")
      : tx("theme.layout.saved", "Everything here is saved");
  };

  /** A text field bound to a dotted path; empty removes the key, which means inherit. */
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

  /** A select of a closed set, bound to a dotted path. */
  const choiceField = (path: string, label: string, hint: string, choices: string[]): HTMLElement => {
    const id = `theme-${path.replace(/[^a-zA-Z0-9]+/g, "-")}`;
    const control = select(
      choices.map((choice) => ({ value: choice, label: titleCase(choice) })),
      {
        id,
        value: readPath(doc, path),
        onchange: (event: Event) => {
          writePath(doc, path, (event.target as HTMLSelectElement).value);
          repaint();
        },
      },
    );
    return field({ label, hint, control, id });
  };

  /**
   * A whole-number field bound to a dotted path.
   *
   * The document stores `space.unit` as a number, so writing the typed string back would be a
   * type the server refuses. It parses, and only writes when what was typed is a number — an
   * empty box clears the key, which means inherit.
   */
  const numberField = (path: string, label: string, hint: string, placeholder: string): HTMLElement => {
    const id = `theme-${path.replace(/[^a-zA-Z0-9]+/g, "-")}`;
    const control = input({
      id,
      type: "number",
      min: 1,
      max: 16,
      step: 1,
      value: readPath(doc, path),
      placeholder,
      oninput: (event: Event) => {
        const raw = (event.target as HTMLInputElement).value.trim();
        if (raw === "") {
          writePath(doc, path, undefined);
          repaint();
          return;
        }
        const parsed = Number(raw);
        if (Number.isFinite(parsed)) writePath(doc, path, parsed);
        repaint();
      },
    });
    return field({ label, hint, control, id });
  };

  /** A boolean switch bound to a dotted path. */
  const switchRow = (path: string, label: string, hint: string): HTMLElement => {
    const control = switchField(label, {
      checked: readPath(doc, path) === "true",
      onchange: (event: Event) => {
        writePath(doc, path, (event.target as HTMLInputElement).checked);
        repaint();
      },
    });
    return h("div", { class: "stack-sm" }, control, h("span", { class: "field-hint" }, hint));
  };

  const save = btn(tx("theme.layout.save", "Save draft"), {
    variant: "primary",
    icon: "check",
    disabled: !canWrite,
    onclick: (event: MouseEvent) =>
      void withBusy(ctx, event.currentTarget as HTMLButtonElement, async () => {
        try {
          const rev = await saveDraft(ctx.api, state, doc);
          adoptSavedRevision(rev);
          ctx.toast(tx("theme.layout.saved_toast", "Draft {{number}} saved", { number: rev.number }), {
            tone: "success",
            detail: tx("theme.layout.saved_detail", "Publish it from History when you are happy with it."),
          });
          reload();
        } catch (err) {
          reportStudioError(ctx, err, tx("theme.layout.failed", "The layout could not be saved"));
        }
      }),
  });

  const discard = btn(tx("theme.layout.discard", "Discard changes"), {
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
      title: tx("theme.layout.shell_title", "Shell"),
      sub: tx("theme.layout.shell_sub", "How the panel is arranged. These become CSS variables and one data attribute, so a preset can change the shape of the panel without a second stylesheet."),
      body: h(
        "div",
        { class: "stack" },
        h(
          "div",
          { class: "grid-3" },
          choiceField("layout.shell.nav", tx("theme.layout.nav", "Navigation"), tx("theme.layout.nav_hint", "A sidebar has room for labels; a rail is icons only; a topbar moves the whole navigation above the content."), limits.navs ?? []),
          choiceField("space.density", tx("theme.layout.density", "Density"), tx("theme.layout.density_hint", "The multiplier the shell applies to its spacing ladder. Compact is the operator default."), limits.densities ?? []),
          numberField("space.unit", tx("theme.layout.unit", "Base grid"), tx("theme.layout.unit_hint", "The spacing unit in pixels. Everything in the ladder is a multiple of it."), "4"),
        ),
        h(
          "div",
          { class: "grid-2" },
          choiceField("layout.shell.cardStyle", tx("theme.layout.card_style", "Card style"), tx("theme.layout.card_style_hint", "Outlined draws a border, elevated draws a shadow, flat draws neither and relies on the surface colour."), limits.card_styles ?? []),
          choiceField("layout.shell.tableStyle", tx("theme.layout.table_style", "Table style"), tx("theme.layout.table_style_hint", "Compact fits more rows on a screen; comfortable gives each row room to be read."), limits.table_styles ?? []),
        ),
        h(
          "div",
          { class: "grid-3" },
          textField("layout.shell.sidebarWidth", tx("theme.layout.sidebar", "Sidebar width"), undefined, "232px"),
          textField("layout.shell.topbarHeight", tx("theme.layout.topbar", "Topbar height"), undefined, "56px"),
          textField("layout.shell.contentMax", tx("theme.layout.content_max", "Content width"), tx("theme.layout.content_max_hint", "The widest the page content grows on a large monitor. Above it the content is centred."), "1440px"),
        ),
        h(
          "div",
          { class: "grid-2" },
          switchRow("layout.shell.commandPalette", tx("theme.layout.palette", "Command palette"), tx("theme.layout.palette_hint", "The keyboard-first search over records and screens.")),
          switchRow("layout.shell.breadcrumbs", tx("theme.layout.breadcrumbs", "Breadcrumbs"), tx("theme.layout.breadcrumbs_hint", "The trail above the page title.")),
        ),
        h(
          "p",
          { class: "field-hint" },
          tx(
            "theme.layout.spacing_note",
            "The eight-step spacing ladder itself (space.scale) is preserved from the document and scaled by the density; it is not edited here.",
          ),
        ),
      ),
    }),
    card({
      title: tx("theme.layout.shape_title", "Shape"),
      sub: tx("theme.layout.shape_sub", "The corner scale and the border width. Full is the pill radius and is not bounded like the others."),
      body: h(
        "div",
        { class: "stack" },
        h(
          "div",
          { class: "grid-4 theme-radius" },
          textField("shape.radius.xs", "xs", undefined, "4px"),
          textField("shape.radius.sm", "sm", undefined, "6px"),
          textField("shape.radius.md", "md", undefined, "10px"),
          textField("shape.radius.lg", "lg", undefined, "14px"),
          textField("shape.radius.xl", "xl", undefined, "20px"),
          textField("shape.radius.full", "full", tx("theme.layout.full_hint", "The pill radius, used by badges and avatars."), "999px"),
          textField("shape.borderWidth", tx("theme.layout.border_width", "Border width"), tx("theme.layout.border_width_hint", "1px is a hairline; 2px is what Brutalist uses to make every box a decision."), "1px"),
        ),
      ),
    }),
    card({
      title: tx("theme.layout.motion_title", "Motion"),
      sub: tx("theme.layout.motion_sub", "Three durations and one easing curve. They are bounded, because a two-second transition is a theme that makes the panel feel broken."),
      body: h(
        "div",
        { class: "grid-4" },
        textField("motion.fast", tx("theme.layout.motion_fast", "Fast"), tx("theme.layout.motion_fast_hint", "Hover and press."), "90ms"),
        textField("motion.normal", tx("theme.layout.motion_normal", "Normal"), tx("theme.layout.motion_normal_hint", "A dialog opening, a panel sliding."), "160ms"),
        textField("motion.slow", tx("theme.layout.motion_slow", "Slow"), tx("theme.layout.motion_slow_hint", "A page transition."), "260ms"),
        textField("motion.ease", tx("theme.layout.motion_ease", "Easing"), tx("theme.layout.motion_ease_hint", "A cubic-bezier() or a keyword."), "cubic-bezier(0.2, 0.8, 0.2, 1)"),
      ),
    }),
    card({
      title: tx("theme.layout.background_title", "Background"),
      sub: tx("theme.layout.background_sub", "The page treatment behind every surface. An image would be an asset reference, never a remote URL, and this build has no asset library yet."),
      body: h(
        "div",
        { class: "stack" },
        h(
          "div",
          { class: "grid-2" },
          choiceField("background.style", tx("theme.layout.background_style", "Style"), tx("theme.layout.background_style_hint", "Flat is the built-in treatment; gradient and mesh paint a soft field; paper adds a fine grain."), limits.backgrounds ?? []),
          textField("background.gradient", tx("theme.layout.background_gradient", "Gradient"), tx("theme.layout.background_gradient_hint", "A CSS gradient value, used when the style is gradient or mesh."), "linear-gradient(160deg, #171413, #241d18)"),
        ),
        h(
          "p",
          { class: "field-hint" },
          tx(
            "theme.layout.background_image_note",
            "A background image is preserved from the document but cannot be chosen here: uploading assets is a later phase, and a field that accepted a URL would be a theme that leaks a request to a third party.",
          ),
        ),
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
          tx("theme.layout.read_only", "This session can read the shell but not save it"),
          tx("theme.layout.read_only_body", "Saving a draft needs theme:write. The values below are what is live or what the open draft holds."),
        ),
    h("div", { class: "split" }, form, h("div", { class: "stack theme-side" }, h("div", { class: "theme-side-sticky" }, diagramHost))),
  );
}

// ------------------------------------------------------------- the diagram --

/**
 * A small picture of the chosen shell.
 *
 * The three navigation variants are the one layout choice whose label does not describe it: a
 * "rail" and a "sidebar" are the same word to somebody who has not seen both. So the diagram is
 * drawn from the document, the rail's width follows `sidebarWidth`, and the top bar's height
 * follows `topbarHeight` — which also makes a 600px sidebar visibly wrong before it is saved.
 */
function shellDiagram(doc: ThemeDocument): HTMLElement {
  const shell = doc.layout?.shell ?? {};
  const nav = (shell.nav ?? "sidebar").trim() || "sidebar";
  const diagram = h(
    "div",
    { class: "theme-shell-diagram", dataset: { nav } },
    h(
      "div",
      { class: "theme-shell-rail" },
      h("span", { class: "theme-shell-mark" }),
      h("span", { class: "theme-shell-item is-active" }),
      h("span", { class: "theme-shell-item" }),
      h("span", { class: "theme-shell-item" }),
      h("span", { class: "theme-shell-item" }),
    ),
    h(
      "div",
      { class: "theme-shell-main" },
      h("div", { class: "theme-shell-top" }, h("span", { class: "theme-shell-top-item" }), h("span", { class: "theme-shell-top-item theme-shell-top-item--short" })),
      h(
        "div",
        { class: "theme-shell-body" },
        h("div", { class: "theme-shell-card" }),
        h("div", { class: "theme-shell-card" }),
      ),
    ),
  );

  // The two dimensions are written as custom properties on the diagram only. They are arbitrary
  // lengths the operator is typing, so no class can carry them; the diagram is a preview of a
  // value, not a token any other rule reads.
  const railWidth = (shell.sidebarWidth ?? "").trim();
  const topHeight = (shell.topbarHeight ?? "").trim();
  if (railWidth) diagram.style.setProperty("--diagram-rail", railWidth);
  if (topHeight) diagram.style.setProperty("--diagram-top", topHeight);

  return card({
    title: tx("theme.layout.diagram_title", "The shell"),
    sub: tx("theme.layout.diagram_sub", "Drawn from the values in the form. The rail is {{nav}} at {{width}}.", {
      nav: titleCase(nav),
      width: railWidth || tx("theme.layout.diagram_default", "the default width"),
    }),
    actions: badge(titleCase(shell.cardStyle ?? "outlined"), "neutral"),
    body: h(
      "div",
      { class: "stack-sm" },
      diagram,
      h(
        "div",
        { class: "row row-wrap theme-chips" },
        badge(titleCase(doc.space?.density ?? "compact"), "neutral"),
        badge(titleCase(shell.tableStyle ?? "compact"), "neutral"),
        badge(doc.shape?.radius?.md ?? "—", "neutral"),
        badge(doc.motion?.normal ?? "—", "neutral"),
      ),
    ),
  });
}
