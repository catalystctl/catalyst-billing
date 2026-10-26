// Theme Studio — overview.
//
// The first screen of the studio answers three questions and nothing else: what is live, where
// do I go next, and what does this installation actually allow. It is a status page, not an
// editor — an operator who has just been handed a new installation needs to see, in one place,
// that the panel is on the built-in default while the storefront is published, and what the
// limits are before they design something the server will refuse.
//
// Three decisions shape it:
//
//  - **"No published theme" is a state, not an error.** An installation that has never opened
//    the studio serves the built-in default, which is a complete, tested appearance. The row
//    says so plainly, and the setup card explains the path from here. A screen that rendered an
//    empty revision table instead would read as broken.
//  - **The existing identity screens are linked, not rebuilt.** Brand name, logo and accents
//    live on `/branding`; mail branding lives with the mail settings; the invoice template lives
//    with the invoice settings. A second editor for any of them would be a second writer for one
//    value, which is the failure this whole system is designed to avoid.
//  - **The limits are shown.** A document over the size bound is refused by the server with a
//    message about bytes; an operator who saw the bound beforehand writes a smaller document.
//
// Every fetch path here has four states — loading, empty, error and permission-denied — because
// this is the screen a reviewer opens first.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { titleCase } from "../lib/format";
import { icon } from "../lib/icons";
import { tx } from "../lib/i18n";import type { Ctx } from "../lib/types";
import {
  DEFAULT_LIMITS,
  adoptOverview,
  loadOverview,
  openPreviewDialog,
  revisionBadge,
  scopeLabel,
  shortDigest,
  studioDenied,
  studioHead,
  unavailableCard,
  whenText,
  type Limits,
  type OverviewResponse,
  type ScopeSummary,
} from "../lib/theme-studio";
import { badge, banner, btn, card, emptyState, resource, skeletonPage } from "../ui/kit";

export const meta = { title: "Theme Studio", crumb: "Theme" };

export default async function themeOverviewPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("theme:read")) {
    return studioDenied("theme:read", tx("theme.overview.what", "The Theme Studio overview"));
  }

  const host = h("div");
  const paint = (): void => {
    swap(
      host,
      resource(
        ctx,
        async (signal) => {
          try {
            return await loadOverview(ctx.api, signal);
          } catch (err) {
            // A 404 or 501 is a fact about the build rather than a failed request: the same
            // request answers the same way, so the screen says what is missing instead of
            // offering a retry button that cannot work.
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
                  "theme.overview.unavailable",
                  "This installation does not serve GET /api/v1/staff/theme, so there is no theme registry to manage.",
                ),
              )
            : renderOverview(ctx, data, paint),
        { skeleton: skeletonPage },
      ),
    );
  };
  paint();
  return h("div", { class: "page" }, host);
}

// ---------------------------------------------------------------- the page --

function renderOverview(ctx: Ctx, data: OverviewResponse, reload: () => void): Node {
  // The overview carries the limits and the permissions every other screen relies on, so it
  // adopts them here: an operator who opens the studio on this screen and then moves to Colours
  // has the same bounds in hand without a second read.
  adoptOverview(data);
  const limits: Limits = data.limits ?? DEFAULT_LIMITS;
  const scopes = Array.isArray(data.scopes) ? data.scopes : [];
  const presets = Array.isArray(data.presets) ? data.presets : [];
  const canWrite = data.permissions?.can_write !== false;
  const anyPublished = scopes.some((entry) => Boolean(entry.published));

  return h(
    "div",
    { class: "page" },
    studioHead({
      active: "overview",
      title: tx("theme.overview.title", "Theme Studio"),
      sub: tx(
        "theme.overview.sub",
        "The appearance of the panel, the customer portal, the storefront, transactional mail and invoice PDFs.",
      ),
      actions: h(
        "div",
        { class: "row row-wrap" },
        btn(tx("theme.overview.open_presets", "Browse presets"), {
          href: "/settings/theme/presets",
          icon: "sparkles",
          variant: "primary",
        }),
        btn(tx("theme.overview.open_history", "History"), { href: "/settings/theme/history", icon: "clock" }),
      ),
    }),

    data.presets_ok === false
      ? banner(
          "warning",
          tx("theme.overview.presets_broken", "The preset catalogue could not be loaded"),
          tx(
            "theme.overview.presets_broken_body",
            "The embedded presets failed validation in this build, so the gallery is empty. Publishing a document you already have still works; the preset gallery needs a fixed build.",
          ),
        )
      : null,

    canWrite
      ? null
      : banner(
          "info",
          tx("theme.overview.read_only", "You can look, but not change"),
          tx(
            "theme.overview.read_only_body",
            "Reading the studio needs theme:read. Creating a draft needs theme:write and publishing needs theme:publish, so this session shows what is live without offering an action that would be refused.",
          ),
        ),

    anyPublished ? null : setupCard(),

    scopesCard(ctx, scopes, canWrite, reload),

    h(
      "div",
      { class: "split" },
      healthCard(limits, presets.length),
      existingScreensCard(),
    ),
  );
}

/**
 * The onboarding card.
 *
 * It disappears the moment anything is published, which is the only definition of "set up" that
 * does not need a second fact stored somewhere: a theme that is live is a theme somebody
 * finished.
 */
function setupCard(): HTMLElement {
  const step = (index: number, title: string, body: string, href: string, label: string): HTMLElement =>
    h(
      "div",
      { class: "theme-step" },
      h("span", { class: "theme-step-number" }, String(index)),
      h(
        "div",
        { class: "theme-step-text" },
        h("strong", null, title),
        h("span", { class: "field-hint" }, body),
      ),
      btn(label, { href, size: "sm", variant: "ghost", icon: "arrowRight" }),
    );

  return card({
    title: tx("theme.overview.setup_title", "Finish setting up your theme"),
    sub: tx("theme.overview.setup_sub", "Nothing is published yet, so every scope serves the built-in appearance."),
    marker: "accent",
    body: h(
      "div",
      { class: "stack-sm" },
      step(
        1,
        tx("theme.overview.setup_1", "Pick a starting point"),
        tx("theme.overview.setup_1_body", "A preset is a whole design — colours, shape, density and type — not a hue swap. Applying one opens a draft."),
        "/settings/theme/presets",
        tx("theme.overview.setup_1_action", "Presets"),
      ),
      step(
        2,
        tx("theme.overview.setup_2", "Adjust the colours"),
        tx("theme.overview.setup_2_body", "Both modes are edited side by side, with a contrast meter that says which pairs fail WCAG."),
        "/settings/theme/colors",
        tx("theme.overview.setup_2_action", "Colours"),
      ),
      step(
        3,
        tx("theme.overview.setup_3", "Add your name and logo"),
        tx("theme.overview.setup_3_body", "Identity stays on the branding screen, because it also reaches invoices and mail — it is not a theme token."),
        "/branding",
        tx("theme.overview.setup_3_action", "Branding"),
      ),
      step(
        4,
        tx("theme.overview.setup_4", "Check the shell"),
        tx("theme.overview.setup_4_body", "Navigation, density, card and table style, radius and motion. The preview shows the result in an iframe."),
        "/settings/theme/layout",
        tx("theme.overview.setup_4_action", "Layout"),
      ),
      step(
        5,
        tx("theme.overview.setup_5", "Publish"),
        tx("theme.overview.setup_5_body", "Publishing is a separate step from saving. It writes an immutable revision and a publication row, so it can always be rolled back."),
        "/settings/theme/history",
        tx("theme.overview.setup_5_action", "History"),
      ),
    ),
  });
}

/** One row per scope: what is live, and how to get to it. */
function scopesCard(ctx: Ctx, scopes: ScopeSummary[], canWrite: boolean, reload: () => void): HTMLElement {
  if (scopes.length === 0) {
    return card({
      title: tx("theme.overview.scopes", "Scopes"),
      body: emptyState({
        icon: "layers",
        title: tx("theme.overview.empty", "No scopes were reported"),
        body: tx(
          "theme.overview.empty_body",
          "The overview answered without a scope list. That is a server problem rather than an empty installation: every installation has the five declared scopes.",
        ),
        action: btn(tx("theme.overview.retry", "Try again"), { size: "sm", icon: "refresh", onclick: reload }),
      }),
    });
  }

  const rows = scopes.map((entry) => {
    const published = entry.published ?? null;
    const draft = entry.draft ?? null;
    const scope = entry.scope;
    const actions = h(
      "div",
      { class: "row row-wrap" },
      btn(tx("theme.overview.edit", "Edit"), {
        href: `/settings/theme/colors?scope=${scope}`,
        size: "sm",
        variant: "ghost",
        icon: "edit",
      }),
      btn(tx("theme.overview.history", "History"), {
        href: `/settings/theme/history?scope=${scope}`,
        size: "sm",
        variant: "ghost",
        icon: "clock",
      }),
      published && canWrite
        ? btn(tx("theme.overview.preview", "Preview"), {
            size: "sm",
            variant: "ghost",
            icon: "eye",
            onclick: () =>
              void openPreviewDialog(ctx, {
                revisionId: published.id,
                scope,
                label: `${scopeLabel(scope, entry.label)} · ${tx("theme.overview.revision", "revision")} ${published.number}`,
              }),
          })
        : null,
    );

    return h(
      "tr",
      null,
      h(
        "td",
        null,
        h("div", { class: "cell-main" }, h("div", { class: "primary" }, scopeLabel(scope, entry.label)), h("div", { class: "secondary" }, scope)),
      ),
      h(
        "td",
        null,
        published
          ? h(
              "div",
              { class: "stack-sm" },
              revisionBadge("published"),
              h("span", { class: "field-hint" }, tx("theme.overview.revision", "Revision {{number}}", { number: published.number })),
            )
          : h(
              "div",
              { class: "stack-sm" },
              badge(tx("theme.overview.builtin", "Built-in default"), "neutral", { dot: true }),
              h("span", { class: "field-hint" }, tx("theme.overview.builtin_hint", "No theme is published for this scope.")),
            ),
      ),
      h(
        "td",
        null,
        published
          ? h("code", { class: "inline-code", title: published.digest }, shortDigest(published.digest))
          : h("span", { class: "field-hint" }, "—"),
      ),
      h(
        "td",
        null,
        published
          ? h(
              "div",
              { class: "stack-sm" },
              h("span", null, whenText(published.published_at)),
              h(
                "span",
                { class: "field-hint" },
                published.published_by
                  ? tx("theme.overview.by", "by {{who}}", { who: published.published_by })
                  : tx("theme.overview.by_unknown", "by an operator who is no longer listed"),
              ),
            )
          : h("span", { class: "field-hint" }, "—"),
      ),
      h(
        "td",
        null,
        draft
          ? h(
              "div",
              { class: "stack-sm" },
              badge(tx("theme.overview.draft_open", "Draft {{number}}", { number: draft.number }), "warning", { dot: true }),
              h("span", { class: "field-hint" }, whenText(draft.created_at)),
            )
          : h("span", { class: "field-hint" }, tx("theme.overview.no_draft", "None")),
      ),
      h("td", null, actions),
    );
  });

  return card({
    title: tx("theme.overview.scopes", "Scopes"),
    sub: tx(
      "theme.overview.scopes_sub",
      "A scope is a surface. Portal and storefront inherit the panel's presentation until they override a token; mail and invoice carry presentation only, because their renderers read their own settings.",
    ),
    flush: true,
    body: h(
      "div",
      { class: "table-wrap" },
      h(
        "table",
        { class: "table" },
        h(
          "thead",
          null,
          h(
            "tr",
            null,
            h("th", null, tx("theme.overview.col_scope", "Scope")),
            h("th", null, tx("theme.overview.col_status", "Status")),
            h("th", null, tx("theme.overview.col_digest", "Digest")),
            h("th", null, tx("theme.overview.col_published", "Published")),
            h("th", null, tx("theme.overview.col_draft", "Open draft")),
            h("th", null, tx("theme.overview.col_actions", "Actions")),
          ),
        ),
        h("tbody", null, ...rows),
      ),
    ),
  });
}

/**
 * The health card: what the server will accept.
 *
 * The numbers are the server's own (`themeLimits`), not a copy: a build talking to an older
 * server shows that server's bounds rather than this build's opinion of them.
 */
function healthCard(limits: Limits, presetCount: number): HTMLElement {
  const kib = (bytes: number): string => `${Math.round(bytes / 1024)} KiB`;
  const row = (label: string, value: string): HTMLElement =>
    h("div", { class: "theme-health-row" }, h("span", { class: "theme-health-label" }, label), h("span", { class: "theme-health-value" }, value));

  return card({
    title: tx("theme.overview.health", "What this installation accepts"),
    sub: tx("theme.overview.health_sub", "The bounds the server enforces when a draft is saved or published."),
    body: h(
      "div",
      { class: "stack-sm" },
      row(tx("theme.overview.limit_document", "Document size"), kib(limits.max_document_bytes)),
      row(tx("theme.overview.limit_css", "Custom CSS"), kib(limits.max_custom_css_bytes)),
      row(tx("theme.overview.limit_name", "Theme name"), tx("theme.overview.limit_chars", "{{count}} characters", { count: limits.max_name_length })),
      row(tx("theme.overview.limit_modes", "Colour modes"), (limits.modes ?? []).map((mode) => titleCase(mode)).join(", ")),
      row(tx("theme.overview.limit_densities", "Densities"), (limits.densities ?? []).map((value) => titleCase(value)).join(", ")),
      row(tx("theme.overview.limit_navs", "Navigation"), (limits.navs ?? []).map((value) => titleCase(value)).join(", ")),
      row(tx("theme.overview.limit_cards", "Card styles"), (limits.card_styles ?? []).map((value) => titleCase(value)).join(", ")),
      row(tx("theme.overview.limit_tables", "Table styles"), (limits.table_styles ?? []).map((value) => titleCase(value)).join(", ")),
      row(tx("theme.overview.limit_backgrounds", "Backgrounds"), (limits.backgrounds ?? []).map((value) => titleCase(value)).join(", ")),
      row(tx("theme.overview.limit_families", "Font families"), String((limits.families ?? []).length)),
      row(tx("theme.overview.limit_components", "Component overrides"), String((limits.components ?? []).length)),
      row(tx("theme.overview.limit_presets", "Presets"), String(presetCount)),
    ),
  });
}

/**
 * The existing screens, linked.
 *
 * These three are the compatibility contract: identity, mail branding and the invoice template
 * each already have a tested editor and a validated store. The studio aggregates the workflow
 * and links to them, and this card is where it says so out loud — so an operator looking for
 * the logo does not conclude the studio is missing a feature.
 */
function existingScreensCard(): HTMLElement {
  const link = (href: string, title: string, body: string): HTMLElement =>
    h(
      "a",
      { class: "theme-link-row", href },
      icon("arrowUpRight", 14),
      h(
        "span",
        { class: "theme-link-text" },
        h("strong", null, title),
        h("span", { class: "field-hint" }, body),
      ),
    );

  return card({
    title: tx("theme.overview.existing", "Identity, mail and invoices"),
    sub: tx("theme.overview.existing_sub", "These are existing screens with their own validated settings. The studio does not duplicate them."),
    body: h(
      "div",
      { class: "stack-sm" },
      link(
        "/branding",
        tx("theme.overview.existing_brand", "Branding"),
        tx("theme.overview.existing_brand_body", "The display name, the uploaded logo and one accent per colour mode. It reaches the panel chrome, mail and the invoice letterhead."),
      ),
      link(
        "/notification-templates",
        tx("theme.overview.existing_mail", "Mail templates"),
        tx("theme.overview.existing_mail_body", "The words of every transactional message. The email scope carries presentation only; mail.brand_* stays the source of the header, accent and footer."),
      ),
      link(
        "/settings/system",
        tx("theme.overview.existing_invoice", "Invoice and mail settings"),
        tx("theme.overview.existing_invoice_body", "Numbering, payment terms, the PDF template's headings and the SMTP configuration — the values a theme deliberately does not own."),
      ),
    ),
  });
}
