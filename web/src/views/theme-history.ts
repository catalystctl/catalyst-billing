// Theme Studio — history.
//
// Every publish writes an immutable revision and a publication row, so this screen is the
// record of what changed, when, and by whom — and the place an operator goes when a theme turns
// out to be wrong. Three properties come from the backend and shape it:
//
//  - **A revision is immutable.** Publishing never edits the row it publishes; it writes a new
//    one. So "restore" is not a rollback of history, it is a new draft copied from an old
//    revision — which is why the button says "Restore as draft" and not "Revert".
//  - **A publication is the assignment of a revision to a scope.** The history list is
//    publications, and each one carries the revision number, the author and the note. That is
//    what makes "who put this live" answerable without joining two tables in the browser.
//  - **Drafts are invisible to visitors.** They appear at the top of this list, marked as
//    drafts, because they are the work in progress and the only rows a visitor cannot see.
//
// The diff is loaded on demand, per revision, from `GET …/revisions/{id}/diff`. It is not
// fetched for the whole list: a diff is a flattened document pair, and rendering fifty of them
// to show one table would make the screen slow to answer a question nobody asked.
//
// Publishing is a separate, explicit action with a confirmation that names the scope and the
// revision. That is the one place in the studio where a click changes what customers see, so it
// is the one place that asks.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateTime } from "../lib/format";
import { icon } from "../lib/icons";
import { tx } from "../lib/i18n";
import type { Ctx } from "../lib/types";
import {
  adoptScope,
  diffTable,
  exportHref,
  isScopeName,
  loadDiff,
  loadScope,
  openPreviewDialog,
  publishRevision,
  reportStudioError,
  restoreRevision,
  revisionBadge,
  scopeLabel,
  scopeNote,
  scopePicker,
  setStudioScope,
  shortDigest,
  studioDenied,
  studioHead,
  studioState,
  unavailableCard,
  whenText,
  type DiffChange,
  type Publication,
  type Revision,
  type ScopeResponse,
} from "../lib/theme-studio";
import { badge, banner, btn, card, emptyState, resource, skeletonPage, withBusy } from "../ui/kit";
import { openDialog } from "../ui/dialog";

export const meta = { title: "Theme history", crumb: "History" };

export default async function themeHistoryPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("theme:read")) {
    return studioDenied("theme:read", tx("theme.history.what", "The theme history"));
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
                tx("theme.history.unavailable", "This installation does not serve GET /api/v1/staff/theme/{scope}, so there is no history to show."),
              )
            : renderHistory(ctx, data, paint),
        { skeleton: skeletonPage },
      ),
    );
  };
  paint();

  return h(
    "div",
    { class: "page" },
    studioHead({
      active: "history",
      title: tx("theme.history.title", "History"),
      sub: tx("theme.history.sub", "Revisions and publications for this scope, newest first. Nothing is ever deleted, so a theme can always be put back."),
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

// -------------------------------------------------------------- the rows --

/** One line of the table: a draft, or a revision that went live at some point. */
interface HistoryRow {
  key: string;
  revisionId: string;
  /** The revision's own number, or null when only the publication row is known. */
  number: number | null;
  status: "draft" | "published" | "archived";
  digest: string;
  author: string | null;
  at: string | null;
  note: string;
}

function rowsFor(data: ScopeResponse): HistoryRow[] {
  const drafts = data.drafts ?? [];
  const publications = data.history ?? [];
  const published = data.published ?? null;

  // Revisions the response actually carries, so a publication row can borrow its digest.
  const known = new Map<string, Revision>();
  if (published) known.set(published.id, published);
  for (const draft of drafts) known.set(draft.id, draft);

  const rows: HistoryRow[] = [];

  for (const draft of drafts) {
    rows.push({
      key: `draft-${draft.id}`,
      revisionId: draft.id,
      number: draft.number,
      status: "draft",
      digest: draft.digest,
      author: draft.created_by,
      at: draft.created_at,
      note: draft.note,
    });
  }

  for (const publication of publications) {
    const revision = known.get(publication.revision_id) ?? null;
    rows.push({
      key: `pub-${publication.id}`,
      revisionId: publication.revision_id,
      number: publication.revision_number,
      status: "published",
      digest: revision?.digest ?? "",
      author: publication.published_by,
      at: publication.published_at,
      note: publication.note,
    });
  }

  // A published revision with no publication row is a fact worth showing rather than hiding:
  // it means the history was truncated or the row was written by hand.
  if (published && !publications.some((entry: Publication) => entry.revision_id === published.id)) {
    rows.push({
      key: `pub-rev-${published.id}`,
      revisionId: published.id,
      number: published.number,
      status: "published",
      digest: published.digest,
      author: published.published_by ?? published.created_by,
      at: published.published_at ?? published.created_at,
      note: published.note,
    });
  }

  const stamp = (row: HistoryRow): number => {
    const value = row.at ? Date.parse(row.at) : Number.NaN;
    return Number.isFinite(value) ? value : 0;
  };
  // Drafts first, then the publications newest first: the work in progress is what the operator
  // came here to act on, and it has no publication date to sort by.
  return rows.sort((a, b) => {
    if (a.status === "draft" && b.status !== "draft") return -1;
    if (b.status === "draft" && a.status !== "draft") return 1;
    return stamp(b) - stamp(a);
  });
}

function renderHistory(ctx: Ctx, data: ScopeResponse, reload: () => void): Node {
  adoptScope(data);
  const state = studioState();
  const canWrite = state.permissions.can_write !== false;
  const canPublish = state.permissions.can_publish !== false;
  const rows = rowsFor(data);
  const label = scopeLabel(data.scope, data.label);

  if (rows.length === 0) {
    return card({
      title: tx("theme.history.card", "Revisions"),
      body: emptyState({
        icon: "clock",
        title: tx("theme.history.empty", "Nothing has been published for {{scope}}", { scope: label }),
        body: tx(
          "theme.history.empty_body",
          "This scope is serving the built-in default. Applying a preset or saving a colour opens the first draft, and publishing it writes the first row here.",
        ),
        action: h(
          "div",
          { class: "row row-wrap" },
          btn(tx("theme.history.empty_presets", "Browse presets"), { href: `/settings/theme/presets?scope=${data.scope}`, size: "sm", icon: "sparkles" }),
          btn(tx("theme.history.empty_colors", "Open colours"), { href: `/settings/theme/colors?scope=${data.scope}`, size: "sm", variant: "ghost", icon: "edit" }),
        ),
      }),
    });
  }

  const table = h(
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
          h("th", null, tx("theme.history.col_revision", "Revision")),
          h("th", null, tx("theme.history.col_status", "Status")),
          h("th", null, tx("theme.history.col_digest", "Digest")),
          h("th", null, tx("theme.history.col_author", "Author")),
          h("th", null, tx("theme.history.col_when", "When")),
          h("th", null, tx("theme.history.col_note", "Note")),
          h("th", null, tx("theme.history.col_actions", "Actions")),
        ),
      ),
      h("tbody", null, ...rows.map((row) => rowElement(ctx, row, data, canWrite, canPublish, reload))),
    ),
  );

  return h(
    "div",
    { class: "stack" },
    canWrite
      ? null
      : banner(
          "info",
          tx("theme.history.read_only", "This session can read the history but not act on it"),
          tx(
            "theme.history.read_only_body",
            "Previewing and restoring need theme:write; publishing needs theme:publish. The rows below are the record of what happened, whichever grants you hold.",
          ),
        ),
    card({
      title: tx("theme.history.card", "Revisions"),
      sub: tx(
        "theme.history.card_sub",
        "A revision is immutable: publishing writes a new row rather than editing one, so restoring an old theme is a new draft copied from it. Retention keeps every revision.",
      ),
      actions: h(
        "div",
        { class: "row row-wrap" },
        badge(tx("theme.history.count", "{{count}} rows", { count: rows.length }), "neutral"),
        // The export is a plain link to a GET, so the browser saves the envelope and the studio
        // needs no download code. It sits inside the loaded card rather than the page head
        // because the head is rendered before the scope is known: a link built there would keep
        // exporting the scope the operator arrived with.
        btn(tx("theme.history.export", "Export JSON"), {
          href: exportHref(data.scope),
          icon: "download",
          size: "sm",
          variant: "ghost",
        }),
      ),
      flush: true,
      body: table,
    }),
    h(
      "p",
      { class: "field-hint" },
      tx(
        "theme.history.export_note",
        "Export writes the envelope a matching installation can import: the theme document and the theme's own assets, and nothing that belongs to the tenant's identity, mail branding or invoice template.",
      ),
    ),
  );
}

function rowElement(
  ctx: Ctx,
  row: HistoryRow,
  data: ScopeResponse,
  canWrite: boolean,
  canPublish: boolean,
  reload: () => void,
): HTMLElement {
  const label = scopeLabel(data.scope, data.label);
  const actions = h(
    "div",
    { class: "row row-wrap" },
    canWrite
      ? btn(tx("theme.history.preview", "Preview"), {
          size: "sm",
          variant: "ghost",
          icon: "eye",
          onclick: () =>
            void openPreviewDialog(ctx, {
              revisionId: row.revisionId,
              scope: data.scope,
              label: `${label} · ${tx("theme.history.revision", "revision")} ${row.number ?? shortDigest(row.revisionId)}`,
            }),
        })
      : null,
    btn(tx("theme.history.diff", "Diff"), {
      size: "sm",
      variant: "ghost",
      icon: "layers",
      onclick: (event: MouseEvent) =>
        void withBusy(ctx, event.currentTarget as HTMLButtonElement, async () => {
          try {
            const diff = await loadDiff(ctx.api, row.revisionId);
            openDiffDialog(row, diff.changes, diff.summary ?? "");
          } catch (err) {
            reportStudioError(ctx, err, tx("theme.history.diff_failed", "The difference could not be read"));
          }
        }),
    }),
    canWrite && row.status !== "draft"
      ? btn(tx("theme.history.restore", "Restore as draft"), {
          size: "sm",
          variant: "ghost",
          icon: "undo",
          onclick: (event: MouseEvent) =>
            void withBusy(ctx, event.currentTarget as HTMLButtonElement, async () => {
              const ok = await ctx.confirm({
                title: tx("theme.history.restore_title", "Copy revision {{number}} into a new draft?", {
                  number: row.number ?? shortDigest(row.revisionId),
                }),
                body: tx(
                  "theme.history.restore_body",
                  "This does not publish anything. It opens a draft for {{scope}} with the values from that revision, which you can edit and publish — or leave open.",
                  { scope: label },
                ),
                confirmLabel: tx("theme.history.restore_confirm", "Create the draft"),
              });
              if (!ok) return;
              try {
                const rev = await restoreRevision(ctx.api, row.revisionId);
                ctx.toast(tx("theme.history.restored", "Draft {{number}} created", { number: rev.number }), {
                  tone: "success",
                  detail: tx("theme.history.restored_detail", "Nothing is live until it is published."),
                });
                reload();
              } catch (err) {
                reportStudioError(ctx, err, tx("theme.history.restore_failed", "The revision could not be restored"));
              }
            }),
        })
      : null,
    canPublish && row.status === "draft"
      ? btn(tx("theme.history.publish", "Publish"), {
          size: "sm",
          variant: "primary",
          icon: "checkCircle",
          onclick: (event: MouseEvent) =>
            void withBusy(ctx, event.currentTarget as HTMLButtonElement, async () => {
              // The one click that changes what customers see, so it names the scope and the
              // revision before it does anything.
              const ok = await ctx.confirm({
                title: tx("theme.history.publish_title", "Publish revision {{number}} to {{scope}}?", {
                  number: row.number ?? shortDigest(row.revisionId),
                  scope: label,
                }),
                body: tx(
                  "theme.history.publish_body",
                  "Every visitor to {{scope}} will see this revision from their next navigation. The revision that is live now stays in the history and can be restored.",
                  { scope: label },
                ),
                confirmLabel: tx("theme.history.publish_confirm", "Publish it"),
              });
              if (!ok) return;
              try {
                const result = await publishRevision(ctx.api, row.revisionId, row.note);
                ctx.toast(
                  tx("theme.history.published", "Revision {{number}} is live on {{scope}}", {
                    number: result.revision.number,
                    scope: label,
                  }),
                  { tone: "success", detail: tx("theme.history.published_detail", "The cache was invalidated after the commit.") },
                );
                reload();
              } catch (err) {
                reportStudioError(ctx, err, tx("theme.history.publish_failed", "The revision could not be published"));
              }
            }),
        })
      : null,
  );

  return h(
    "tr",
    null,
    h("td", null, h("strong", null, row.number === null ? shortDigest(row.revisionId) : `#${row.number}`)),
    h("td", null, revisionBadge(row.status)),
    h("td", null, row.digest ? h("code", { class: "inline-code", title: row.digest }, shortDigest(row.digest)) : h("span", { class: "field-hint" }, "—")),
    h("td", null, row.author ?? h("span", { class: "field-hint" }, tx("theme.history.author_unknown", "unknown"))),
    h("td", null, row.at ? h("span", { title: dateTime(row.at) }, whenText(row.at)) : h("span", { class: "field-hint" }, "—")),
    h("td", null, row.note ? h("span", { class: "theme-note" }, row.note) : h("span", { class: "field-hint" }, "—")),
    h("td", null, actions),
  );
}

/**
 * The diff, in a dialog.
 *
 * The table is `theme-studio.diffTable` so the history screen and any future comparison use one
 * renderer: a diff that showed one set of fields here and another there would be two answers to
 * "what changed".
 */
function openDiffDialog(row: HistoryRow, changes: DiffChange[], summary: string): void {
  openDialog({
    title: tx("theme.history.diff_title", "Revision {{number}} against what is live", {
      number: row.number ?? shortDigest(row.revisionId),
    }),
    subtitle: summary || tx("theme.history.diff_sub", "Every field that differs from the published document."),
    wide: true,
    body: h(
      "div",
      { class: "stack" },
      changes.length === 0
        ? h(
            "div",
            { class: "row" },
            icon("checkCircle", 14),
            h("span", { class: "field-hint" }, tx("theme.history.diff_same", "This revision is identical to what is published.")),
          )
        : diffTable(changes),
      h(
        "p",
        { class: "field-hint" },
        tx(
          "theme.history.diff_note",
          "The comparison is against the revision that is live now, flattened the same way the merge flattens it — so a difference the merge would not see is a difference this table does not show either.",
        ),
      ),
    ),
  });
}
