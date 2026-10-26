// Theme Studio — custom CSS.
//
// The escape hatch: a validated block of CSS that the server parses, re-validates at serve time
// and serves as a first-party stylesheet at a revision-addressed URL. It is not a `<style>` tag
// and not an inline style, because the panel's CSP is `style-src 'self'` with no
// 'unsafe-inline' — and widening that policy for an operator-authored stylesheet would be the
// wrong trade for a panel that renders customer-supplied text.
//
// # Why the screen is mostly explanation
//
// Custom CSS is the one control in the studio that can break the product. On the panel scope it
// can hide a button, cover a table or make a dialog unreachable, and an operator who pasted a
// snippet from the internet will not guess that. So the screen states three things before the
// box: that panel CSS is privileged, what the parser refuses and why, and that the only way to
// validate is to save — because the parser is the server's, not this screen's.
//
// # Why "save" is the validation
//
// A client-side check would be a second parser with a second opinion, and the two would
// disagree exactly where it matters (a `url()` the server considers external and the browser
// does not). So the screen does not pretend to validate: it saves, and if the server refuses, it
// shows the server's own sentence verbatim, above the box, with the code beside it. The operator
// fixes the CSS the parser actually reads.
//
// The byte counter is the one exception, and it is not a validation: it is the same bound the
// server applies, counted the same way (UTF-8 bytes, not characters), so a paste that is 300 KiB
// is visible before the round trip rather than after it.

import { ApiError } from "../lib/api";
import { fill, h, swap } from "../lib/dom";
import { problemFrom, tx } from "../lib/i18n";
import type { Ctx } from "../lib/types";
import {
  adoptSavedRevision,
  adoptScope,
  byteLength,
  cloneDocument,
  isDirty,
  isScopeName,
  loadScope,
  problemSentence,
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
  type ScopeResponse,
  type ThemeDocument,
} from "../lib/theme-studio";
import { badge, banner, btn, card, resource, skeletonPage, textarea, withBusy } from "../ui/kit";

export const meta = { title: "Theme custom CSS", crumb: "Custom CSS" };

export default async function themeCssPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("theme:read")) {
    return studioDenied("theme:read", tx("theme.css.what", "The custom CSS editor"));
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
                tx("theme.css.unavailable", "This installation does not serve GET /api/v1/staff/theme/{scope}, so there is no stylesheet to edit."),
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
      active: "css",
      title: tx("theme.css.title", "Custom CSS"),
      sub: tx("theme.css.sub", "A validated stylesheet served next to the generated tokens. It is parsed on write and again on serve, and it can only use first-party assets."),
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
  const maxBytes = state.limits.max_custom_css_bytes;

  // Where the server's own refusal is rendered. It is above the box rather than in a toast: the
  // sentence names a line, the operator has to read it while editing, and a toast disappears.
  const errorHost = h("div");
  const counter = h("span", { class: "theme-css-counter" });
  const dirtyNote = h("span", { class: "field-hint" });

  const area = textarea({
    id: "theme-custom-css",
    placeholder: tx("theme.css.placeholder", "/* Only what the token contract cannot express. */"),
    value: doc.customCss ?? "",
    maxlength: maxBytes,
  });
  area.rows = 22;
  area.classList.add("theme-css-input");
  area.spellcheck = false;

  const updateCounter = (): void => {
    const bytes = byteLength(area.value);
    const over = bytes > maxBytes;
    counter.dataset.over = over ? "true" : "false";
    counter.textContent = tx("theme.css.counter", "{{used}} of {{max}} bytes", {
      used: bytes.toLocaleString("en-US"),
      max: maxBytes.toLocaleString("en-US"),
    });
    dirtyNote.textContent = isDirty()
      ? tx("theme.css.unsaved", "Unsaved changes")
      : tx("theme.css.saved", "Everything here is saved");
  };

  area.addEventListener("input", () => {
    // An empty box means "no custom CSS", which is a real state rather than an empty string the
    // generator has to special-case: the key is removed and the document inherits.
    const value = area.value;
    if (value.trim() === "") delete doc.customCss;
    else doc.customCss = value;
    updateCounter();
  });

  const save = btn(tx("theme.css.save", "Save and validate"), {
    variant: "primary",
    icon: "check",
    disabled: !canWrite,
    onclick: (event: MouseEvent) =>
      void withBusy(ctx, event.currentTarget as HTMLButtonElement, async () => {
        fill(errorHost);
        try {
          const rev = await saveDraft(ctx.api, state, doc);
          adoptSavedRevision(rev);
          ctx.toast(tx("theme.css.saved_toast", "Draft {{number}} saved and the CSS parsed", { number: rev.number }), {
            tone: "success",
            detail: tx("theme.css.saved_detail", "The server re-validates it again when the stylesheet is served."),
          });
          reload();
        } catch (err) {
          // Verbatim, because the parser's message names the line and the rule it refused, and
          // any sentence this screen wrote would be a worse version of it.
          fill(
            errorHost,
            banner(
              "danger",
              tx("theme.css.refused", "The stylesheet was refused"),
              h(
                "div",
                { class: "stack-sm" },
                h("p", { class: "theme-css-error" }, problemSentence(err, tx("theme.css.refused_fallback", "The server did not say why."))),
                err instanceof ApiError ? h("code", { class: "inline-code" }, `${err.status} ${err.code}`) : null,
              ),
            ),
          );
          reportStudioError(ctx, err, tx("theme.css.refused", "The stylesheet was refused"));
        }
      }),
  });

  const discard = btn(tx("theme.css.discard", "Discard changes"), {
    variant: "ghost",
    icon: "undo",
    disabled: !canWrite,
    onclick: () => {
      setStudioDocument(cloneDocument(state.base ?? data.document));
      reload();
    },
  });

  const scopeWarning =
    data.scope === "panel"
      ? banner(
          "warning",
          tx("theme.css.panel_warning", "Panel CSS is privileged"),
          tx(
            "theme.css.panel_warning_body",
            "This stylesheet loads on every staff screen. A rule that hides a control, covers a dialog or changes a hit area can make the panel unusable for everybody, including the person who wrote it — and publishing it needs theme:publish for exactly that reason. Test it in the preview before publishing, and remember that History can roll it back.",
          ),
        )
      : banner(
          "info",
          tx("theme.css.scoped_warning", "This stylesheet is scoped to {{scope}}", { scope: data.scope }),
          tx(
            "theme.css.scoped_warning_body",
            "Custom CSS in the portal, storefront, email or invoice scope cannot leak into the staff panel. It is still validated by the same parser.",
          ),
        );

  const form = h(
    "div",
    { class: "stack" },
    errorHost,
    scopeWarning,
    card({
      title: tx("theme.css.card_title", "Stylesheet"),
      sub: tx("theme.css.card_sub", "Served as a first-party stylesheet at a revision-addressed URL, so a publish is visible on the next navigation with no cache purge."),
      actions: h(
        "div",
        { class: "row row-wrap" },
        badge(tx("theme.css.byte_badge", "{{max}} KiB maximum", { max: Math.round(maxBytes / 1024) }), "neutral"),
        counter,
      ),
      body: h(
        "div",
        { class: "stack-sm" },
        area,
        h(
          "div",
          { class: "row row-wrap theme-save-row" },
          canWrite ? save : null,
          canWrite ? discard : null,
          dirtyNote,
        ),
      ),
    }),
    policyCard(),
  );

  if (!canWrite) {
    area.disabled = true;
    for (const control of form.querySelectorAll<HTMLButtonElement>("button")) control.disabled = true;
  }

  updateCounter();

  return h(
    "div",
    { class: "stack" },
    canWrite
      ? null
      : banner(
          "info",
          tx("theme.css.read_only", "This session can read the stylesheet but not save it"),
          tx("theme.css.read_only_body", "Writing panel custom CSS needs theme:publish as well as theme:write, because it is the one control that can hide the interface."),
        ),
    form,
  );
}

/**
 * What the parser refuses.
 *
 * It is a list rather than a paragraph because each line is a rule an operator can check their
 * own CSS against. The reasons matter as much as the rules: "no external url()" reads as an
 * arbitrary restriction until it is explained as the thing that stops a stylesheet exfiltrating
 * a value through a background image.
 */
function policyCard(): HTMLElement {
  const rule = (title: string, body: string): HTMLElement =>
    h(
      "div",
      { class: "theme-policy-row" },
      h("span", { class: "theme-policy-mark" }, tx("theme.css.refused_mark", "refused")),
      h("div", { class: "theme-policy-text" }, h("strong", null, title), h("span", { class: "field-hint" }, body)),
    );

  return card({
    title: tx("theme.css.policy_title", "What the parser refuses"),
    sub: tx("theme.css.policy_sub", "It parses the CSS rather than filtering it with a regular expression, on write and again on serve."),
    body: h(
      "div",
      { class: "stack-sm" },
      rule(
        tx("theme.css.policy_import", "@import"),
        tx("theme.css.policy_import_body", "A stylesheet cannot pull in a second stylesheet. It would be a request to an origin this installation does not control, made by every visitor."),
      ),
      rule(
        tx("theme.css.policy_url", "url() outside the theme's own assets"),
        tx("theme.css.policy_url_body", "Only digest-addressed theme assets may be referenced. An arbitrary URL in an attribute selector is how a stylesheet exfiltrates a value: the request itself carries it."),
      ),
      rule(
        tx("theme.css.policy_script", "expression(), behavior: and -moz-binding:"),
        tx("theme.css.policy_script_body", "Legacy ways of running code from a stylesheet. They are refused by name, not by pattern."),
      ),
      rule(
        tx("theme.css.policy_image_set", "image-set() with an external URL, and content: url()"),
        tx("theme.css.policy_image_set_body", "Both fetch a resource the parser cannot account for, which is the same exfiltration path as url()."),
      ),
      rule(
        tx("theme.css.policy_bounds", "Size, selector count and nesting depth"),
        tx("theme.css.policy_bounds_body", "Bounded so one stylesheet cannot become a denial of service on every page load. The byte bound is the counter above."),
      ),
      h(
        "p",
        { class: "field-hint" },
        tx(
          "theme.css.policy_note",
          "The server re-validates on every serve, so a stylesheet written before a rule was added cannot bypass it.",
        ),
      ),
    ),
  });
}

/** The problem's code, for a caller that wants to branch on it. */
export function cssProblemCode(err: unknown): string {
  const problem = problemFrom(err);
  return problem?.code ?? "";
}
