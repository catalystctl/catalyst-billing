// Theme Studio — presets.
//
// A preset is a whole design, not a hue swap: `Brutalist` has no radius and 2px borders,
// `Editorial Paper` is a serif on warm paper, `Neon Terminal` is monospace with a glow. That is
// why this screen is a gallery and not a dropdown — the differences that matter are the ones a
// name cannot describe, and a row of swatches plus the chips for radius, density and navigation
// shows them before the operator commits.
//
// Three rules come from the backend and shape the screen:
//
//  - **Applying a preset creates a draft; it never publishes.** The button says "Apply as
//    draft" because that is what happens, and a gallery that quietly restyled the live panel
//    would be the single most dangerous control in the product.
//  - **A draft is a thing you can lose.** Applying a preset replaces the open draft's document,
//    so if one exists the screen asks first, names the revision it is about to replace, and
//    offers History instead. That confirmation is the whole reason this file has a dialog.
//  - **`extends` is resolved at draft time.** A later change to the preset cannot mutate a
//    theme somebody already published, so the card can honestly say the draft is a copy.
//
// The gallery renders from the server's catalogue (`presets.Meta`), not from a list in this
// file: the character line, the radius and the swatches are derived from the embedded JSON by
// the same code that validates it, so a preset cannot be described on screen as something the
// generator does not produce.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { titleCase } from "../lib/format";
import { tx } from "../lib/i18n";
import type { Ctx } from "../lib/types";
import {
  adoptScope,
  applyPreset,
  isScopeName,
  loadScope,
  reportStudioError,
  scopeNote,
  scopePicker,
  setStudioScope,
  studioDenied,
  studioHead,
  studioState,
  swatchRow,
  unavailableCard,
  type PresetMeta,
  type ScopeResponse,
} from "../lib/theme-studio";
import { badge, banner, btn, card, emptyState, resource, skeletonPage, withBusy } from "../ui/kit";

export const meta = { title: "Theme presets", crumb: "Presets" };

export default async function themePresetsPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("theme:read")) {
    return studioDenied("theme:read", tx("theme.presets.what", "The preset gallery"));
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
                tx(
                  "theme.presets.unavailable",
                  "This installation does not serve GET /api/v1/staff/theme/{scope}, so there is no theme to apply a preset to.",
                ),
              )
            : renderGallery(ctx, data, () => paint()),
        { skeleton: skeletonPage },
      ),
    );
  };
  paint();

  return h(
    "div",
    { class: "page" },
    studioHead({
      active: "presets",
      title: tx("theme.presets.title", "Presets"),
      sub: tx(
        "theme.presets.sub",
        "Ten starting points. Each one is a complete document — colour, type, shape, density and shell — so applying one is a design decision, not a recolour.",
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

// -------------------------------------------------------------- the gallery --

function renderGallery(ctx: Ctx, data: ScopeResponse, reload: () => void): Node {
  adoptScope(data);
  const state = studioState();
  const presets = data.presets ?? [];
  const canWrite = state.permissions.can_write !== false;
  const openDraft = state.revision && state.revision.status === "draft" ? state.revision : null;
  // The preset the current document came from. A resolved document keeps the preset's `id`, so
  // this survives inheritance; `annotations.preset` is the fallback an import may carry.
  const activePreset = data.document?.annotations?.preset || data.document?.id || "";

  if (presets.length === 0) {
    return card({
      title: tx("theme.presets.card", "The catalogue"),
      body: emptyState({
        icon: "sparkles",
        title: tx("theme.presets.empty", "No presets were reported"),
        body: tx(
          "theme.presets.empty_body",
          "The embedded preset catalogue failed to load in this build. A document you already have can still be edited and published; the gallery needs a fixed build.",
        ),
        action: btn(tx("theme.presets.history", "Open history"), { href: `/settings/theme/history?scope=${data.scope}`, size: "sm", icon: "clock" }),
      }),
    });
  }

  const grid = h(
    "div",
    { class: "theme-preset-grid" },
    ...presets.map((preset) => presetCard(ctx, preset, data, openDraft, activePreset, canWrite, reload)),
  );

  return h(
    "div",
    { class: "stack" },
    canWrite
      ? null
      : banner(
          "info",
          tx("theme.presets.read_only", "Applying a preset needs theme:write"),
          tx(
            "theme.presets.read_only_body",
            "This session can read the catalogue and the active revision, but not open a draft. That is a separate grant from reading the studio.",
          ),
        ),
    openDraft
      ? banner(
          "warning",
          tx("theme.presets.draft_open", "Revision {{number}} is an open draft", { number: openDraft.number }),
          tx(
            "theme.presets.draft_open_body",
            "Applying a preset replaces that draft's document. Nothing published changes until somebody publishes, and History can restore the old revision afterwards — but the unsaved work in the draft is gone.",
          ),
          btn(tx("theme.presets.open_history", "Open history"), {
            href: `/settings/theme/history?scope=${data.scope}`,
            size: "sm",
            variant: "ghost",
          }),
        )
      : null,
    grid,
    h(
      "p",
      { class: "field-hint" },
      tx(
        "theme.presets.note",
        "A preset is copied into the draft, not referenced. Editing the draft afterwards cannot change the preset, and a later change to the preset cannot change a published theme.",
      ),
    ),
  );
}

function presetCard(
  ctx: Ctx,
  preset: PresetMeta,
  data: ScopeResponse,
  openDraft: { id: string; number: number } | null,
  activePreset: string,
  canWrite: boolean,
  reload: () => void,
): HTMLElement {
  const active = preset.id === activePreset;

  const chips = h(
    "div",
    { class: "row row-wrap theme-chips" },
    badge(titleCase(preset.mode), "neutral"),
    badge(tx("theme.presets.chip_radius", "radius {{value}}", { value: preset.radius || "—" }), "neutral"),
    badge(titleCase(preset.density), "neutral"),
    badge(tx("theme.presets.chip_nav", "{{value}} nav", { value: titleCase(preset.nav) }), "neutral"),
  );

  const apply = btn(tx("theme.presets.apply", "Apply as draft"), {
    size: "sm",
    variant: active ? "ghost" : "primary",
    icon: "plus",
    disabled: !canWrite,
    onclick: (event: MouseEvent) =>
      void withBusy(ctx, event.currentTarget as HTMLButtonElement, async () => {
        // A draft is unsaved work, so replacing one is a decision the operator makes with the
        // revision number in front of them rather than a side effect of a gallery click.
        if (openDraft) {
          const ok = await ctx.confirm({
            title: tx("theme.presets.replace_title", "Replace draft revision {{number}}?", {
              number: openDraft.number,
            }),
            body: tx(
              "theme.presets.replace_body",
              "This scope already has an open draft. Applying {{preset}} replaces its document, and the values in it that are not part of the preset are lost. Nothing published changes.",
              { preset: preset.name },
            ),
            confirmLabel: tx("theme.presets.replace_confirm", "Replace the draft"),
            danger: true,
          });
          if (!ok) return;
        }
        try {
          const rev = await applyPreset(
            ctx.api,
            data.scope,
            preset.id,
            tx("theme.presets.note_applied", "Applied the {{preset}} preset", { preset: preset.name }),
          );
          ctx.toast(tx("theme.presets.applied", "Draft {{number}} created from {{preset}}", { number: rev.number, preset: preset.name }), {
            tone: "success",
            detail: tx("theme.presets.applied_detail", "Nothing is live until it is published."),
          });
          reload();
        } catch (err) {
          reportStudioError(ctx, err, tx("theme.presets.failed", "The preset could not be applied"));
        }
      }),
  });

  return card({
    title: preset.name,
    sub: preset.character,
    actions: active ? badge(tx("theme.presets.active", "In use"), "success", { dot: true }) : null,
    body: h(
      "div",
      { class: "stack-sm" },
      swatchRow(preset.swatch ?? [], tx("theme.presets.swatches", "{{name}}: background, card, text and accent", { name: preset.name })),
      h("p", { class: "field-hint" }, preset.description),
      chips,
    ),
    foot: h(
      "div",
      { class: "row row-wrap" },
      apply,
      btn(tx("theme.presets.edit", "Open colours"), {
        href: `/settings/theme/colors?scope=${data.scope}`,
        size: "sm",
        variant: "ghost",
        icon: "edit",
      }),
    ),
  });
}
