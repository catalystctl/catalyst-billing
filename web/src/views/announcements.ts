// Announcements.
//
// An announcement is four columns and a screen: a title, a body, where it shows and when it stops
// showing. The screen's job is to make "is this live for a customer right now" obvious, because
// the answer is not one column — it is `published` **and** a `published_at` in the past **and**
// an `expires_at` in the future. The server computes that as `state` (and the older `live`) on
// every row from its own clock, and this page renders the server's answer rather than re-deriving
// it: a browser whose clock is a minute fast would otherwise label a notice live that a customer's
// request is being told is not.
//
// The four states are named, not inferred. `live: false` alone cannot tell a draft from a notice
// that stopped showing an hour ago from one scheduled for Sunday, and those are three different
// things for the operator to do next.
//
// The list also carries `enabled`, which is the `announcements.enabled` setting. With it off every
// row may be live and still be invisible to customers, so the page says so at the top: an operator
// who publishes a maintenance notice and sees it listed as Live, while the storefront shows
// nothing, has found the one bug this screen exists to prevent.
//
// The publish toggle is therefore a first-class action, not a form field buried in a dialog: a
// typo published for a minute is the exact case the API allows a delete for, and a draft that
// should be live is one click. Delete is offered here (unlike an invoice) because an announcement
// has no legal weight and no record references it.
//
// A `published_at` is stamped once, when the row first goes live, so unpublishing and
// republishing does not pretend the announcement is brand new. The page says so, because an
// operator who expects the timestamp to move will otherwise file a bug.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateTime, relative } from "../lib/format";
import type { Ctx } from "../lib/types";
import {
  badge,
  banner,
  btn,
  card,
  emptyState,
  field,
  input,
  pageHead,
  resource,
  select,
  skeletonPage,
  textarea,
  withBusy,
} from "../ui/kit";
import { bindForm, openDialog } from "../ui/dialog";

export const meta = { title: "Announcements", crumb: "Announcements" };

// ---------------------------------------------------------------- payloads --

/** One announcement, verbatim from the `announcement` struct in
 *  internal/httpapi/announcements.go. `state` and `live` are derived server-side from the
 *  server's clock. */
interface Announcement {
  id: string;
  title: string;
  body: string;
  placement: string;
  published: boolean;
  published_at: string | null;
  expires_at: string | null;
  created_by: string;
  created_at: string;
  updated_at: string;
  live: boolean;
  /** draft | scheduled | expired | live */
  state: string;
}

interface AnnouncementList {
  data: Announcement[];
  /** The `announcements.enabled` setting as the server read it. Absent means the server did not
   *  say, which is treated as on: the rows are the stronger signal. */
  enabled?: boolean;
}

/** The closed set the table's CHECK accepts, from `announcementPlacements`. */
const PLACEMENTS = [
  { value: "both", label: "Storefront and customer dashboard" },
  { value: "storefront", label: "Storefront only" },
  { value: "dashboard", label: "Customer dashboard only" },
];

export default async function announcementsPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("announcement:write")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Announcements" }),
      banner(
        "danger",
        "You cannot publish announcements",
        "This screen needs the announcement:write permission: a published announcement is visible to every visitor, including signed-out ones.",
        btn("Back to the dashboard", { href: "/", size: "sm", variant: "ghost" }),
      ),
    );
  }

  const host = h("div");

  /** Loads the list, or reports that this build does not serve it. A 404 is the
   *  operator-surface package not being merged yet, which is a fact about the installation
   *  rather than a failed request: the screen says so instead of offering a retry. */
  const load = async (signal: AbortSignal): Promise<AnnouncementList | "unavailable"> => {
    try {
      return await ctx.api.get<AnnouncementList>("/api/v1/staff/announcements", { cache: false, signal });
    } catch (err) {
      if (err instanceof ApiError && (err.status === 404 || err.status === 501)) return "unavailable";
      throw err;
    }
  };

  const paint = (): void => {
    swap(
      host,
      resource(
        ctx,
        load,
        (data) => (data === "unavailable" ? unavailableCard() : render(data)),
        { skeleton: skeletonPage },
      ),
    );
  };

  // ------------------------------------------------------------- mutations --

  const togglePublish = async (row: Announcement, button: HTMLButtonElement): Promise<void> => {
    await withBusy(ctx, button, async () => {
      try {
        // A full update, because `announcementInput` is a whole row: an omitted body would be
        // stored as empty. Sending what the row already holds is the honest way to change one
        // field on an endpoint that replaces the row.
        await ctx.api.put(`/api/v1/staff/announcements/${encodeURIComponent(row.id)}`, {
          title: row.title,
          body: row.body,
          placement: row.placement,
          published: !row.published,
          expires_at: row.expires_at,
        });
        ctx.toast(row.published ? "Unpublished" : "Published", {
          tone: "success",
          detail: row.published ? "It is hidden from customers immediately." : "It is visible where it was placed.",
        });
        paint();
      } catch (err) {
        report(err, row.published ? "The announcement could not be unpublished" : "The announcement could not be published");
      }
    });
  };

  const remove = async (row: Announcement): Promise<void> => {
    const ok = await ctx.confirm({
      title: `Delete “${row.title}”?`,
      body: "An announcement has no legal weight and nothing references it, so it can be removed. The audit log keeps the record that it existed and who published it.",
      confirmLabel: "Delete announcement",
      danger: true,
    });
    if (!ok) return;
    try {
      await ctx.api.del(`/api/v1/staff/announcements/${encodeURIComponent(row.id)}`);
      ctx.toast("Announcement deleted", { tone: "success" });
      paint();
    } catch (err) {
      report(err, "The announcement could not be deleted");
    }
  };

  const report = (err: unknown, fallback: string): void => {
    if (err instanceof ApiError) {
      ctx.toast(err.detail || fallback, { tone: "danger", detail: err.code });
      return;
    }
    ctx.toast(fallback, { tone: "danger", detail: "The server could not be reached." });
  };

  // ---------------------------------------------------------------- dialog --

  const editDialog = (existing: Announcement | null): void => {
    const title = input({
      id: "ann-title",
      value: existing?.title ?? "",
      placeholder: "Scheduled maintenance on Sunday",
      maxlength: 200,
      required: true,
    });
    const body = textarea({
      id: "ann-body",
      value: existing?.body ?? "",
      placeholder: "The control panel will be unavailable between 02:00 and 03:00 UTC while we upgrade the database.",
      maxlength: 20 * 1024,
    });
    const placement = select(PLACEMENTS, { id: "ann-placement", value: existing?.placement ?? "both" });
    const expires = input({
      id: "ann-expires",
      type: "datetime-local",
      value: existing?.expires_at ? localInput(existing.expires_at) : "",
    });
    const published = h("input", { type: "checkbox", checked: existing?.published ?? false });

    const errorHost = h("div");
    const form = h(
      "form",
      { class: "stack", novalidate: true },
      errorHost,
      h(
        "div",
        { class: "form-grid" },
        field({ label: "Title", control: title, id: "ann-title", required: true, class: "span-2" }),
        field({
          label: "Body",
          control: body,
          id: "ann-body",
          class: "span-2",
          hint: "Plain text. A blank line starts a new paragraph.",
        }),
        field({ label: "Where it shows", control: placement, id: "ann-placement" }),
        field({ label: "Expires", control: expires, id: "ann-expires", hint: "Empty means it never expires." }),
        h("div", { class: "span-2" }, h("label", { class: "switch" }, published, "Published")),
      ),
    );

    const submit = btn(existing ? "Save announcement" : "Create announcement", { variant: "primary", type: "submit" });
    const cancel = btn("Cancel", { variant: "ghost" });
    bindForm(form, submit);

    const handle = openDialog({
      title: existing ? "Edit announcement" : "New announcement",
      subtitle: "A draft is invisible until it is published. Publishing stamps the moment it first went live, and republishing does not move that stamp.",
      body: h("div", { class: "stack" }, form),
      footer: h("div", { class: "row spacer" }, cancel, submit),
      wide: true,
      locked: () => submit instanceof HTMLButtonElement && submit.disabled,
    });
    cancel.addEventListener("click", () => handle.close());

    form.addEventListener("submit", (event) => {
      event.preventDefault();
      void (async () => {
        errorHost.replaceChildren();
        if (!title.value.trim()) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "A title is required."));
          return;
        }
        const expiresIso = isoFromLocal(expires.value);
        if (expiresIso === null) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "The expiry is not a valid date and time."));
          return;
        }
        if (expiresIso && Date.parse(expiresIso) <= Date.now()) {
          swap(
            errorHost,
            h("div", { class: "banner banner--danger" }, "That expiry is in the past, so the announcement would never be live. Clear it or choose a later date."),
          );
          return;
        }
        const payload: Record<string, unknown> = {
          title: title.value.trim(),
          body: body.value,
          placement: placement.value,
          published: published.checked,
        };
        if (expiresIso) payload.expires_at = expiresIso;

        if (submit instanceof HTMLButtonElement) submit.disabled = true;
        try {
          if (existing) await ctx.api.put(`/api/v1/staff/announcements/${encodeURIComponent(existing.id)}`, payload);
          else await ctx.api.post("/api/v1/staff/announcements", payload);
          ctx.toast(existing ? "Announcement saved" : "Announcement created", { tone: "success" });
          handle.close();
          paint();
        } catch (err) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, announcementError(err)));
        } finally {
          if (submit instanceof HTMLButtonElement) submit.disabled = false;
        }
      })();
    });
  };

  const unavailableCard = (): HTMLElement =>
    card({
      title: "Announcements are not available in this build",
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "p",
          { class: "field-hint" },
          "This installation does not serve ",
          h("code", { class: "inline-code" }, "GET /api/v1/staff/announcements"),
          ", so there is nothing to list or publish. Reloading will give the same answer until a build that includes the announcement API is deployed.",
        ),
        h(
          "p",
          { class: "field-hint" },
          "The public feed the storefront reads is a separate route and is unaffected: any announcement already published continues to show.",
        ),
        h("div", { class: "row" }, btn("Back to the dashboard", { href: "/", size: "sm", icon: "home" })),
      ),
    });

  // ---------------------------------------------------------------- render --

  const render = (list: AnnouncementList): Node => {
    const rows = list.data ?? [];
    // `!== false` rather than `=== true`: a server that does not send the field is an older
    // build, and the rows it did send are the stronger signal.
    const enabled = list.enabled !== false;
    const count = (state: string): number => rows.filter((row) => row.state === state).length;
    const live = count("live");
    const scheduled = count("scheduled");
    const expired = count("expired");
    const drafts = count("draft");

    return h(
      "div",
      { class: "stack" },
      // The setting, stated where the operator will read it. With the feed switched off every
      // live row below is invisible to customers, and the list alone would look healthy.
      enabled
        ? null
        : banner(
            "warning",
            "The announcement feed is switched off",
            h(
              "span",
              null,
              "Every announcement below is hidden from customers, including the ones marked Live. Publishing changes nothing until the feed is switched back on in ",
              h("a", { class: "linkish", href: "/settings/system" }, "system settings"),
              ".",
            ),
          ),
      h(
        "div",
        { class: "row-wrap ann-summary" },
        badge(`${live} live`, live > 0 ? "success" : "neutral", { dot: true }),
        scheduled > 0 ? badge(`${scheduled} scheduled`, "info", { dot: true }) : null,
        expired > 0 ? badge(`${expired} expired`, "neutral") : null,
        h("span", { class: "dim" }, `${rows.length} total`),
        drafts > 0 ? badge(`${drafts} draft${drafts === 1 ? "" : "s"}`, "neutral") : null,
      ),
      rows.length === 0
        ? card({
            flush: true,
            body: emptyState({
              title: "Nothing announced",
              body: "An announcement appears on the storefront, in the customer dashboard, or both. It is the way to say “we are upgrading the database on Sunday” without an email.",
              icon: "radio",
              action: btn("Write the first announcement", { variant: "primary", icon: "plus", onclick: () => editDialog(null) }),
            }),
          })
        : h("div", { class: "ann-list" }, ...rows.map((row) => rowCard(row, enabled))),
    );
  };

  const rowCard = (row: Announcement, feedEnabled: boolean): HTMLElement => {
    const toggle = btn(row.published ? "Unpublish" : "Publish", {
      size: "sm",
      variant: row.published ? "ghost" : "primary",
      icon: row.published ? "eye" : "send",
    }) as HTMLButtonElement;
    toggle.addEventListener("click", () => void togglePublish(row, toggle));

    return h(
      "article",
      { class: `card ann-card${row.state === "live" ? " ann-card--live" : ""}` },
      h(
        "header",
        { class: "ann-head" },
        h(
          "div",
          { class: "ann-head-text" },
          h(
            "div",
            { class: "ann-title-row" },
            h("h2", { class: "ann-title" }, row.title || "Untitled"),
            stateBadge(row.state),
          ),
          h(
            "div",
            { class: "ann-meta" },
            h("span", null, placementLabel(row.placement)),
            row.published_at ? h("span", { class: "dot-sep" }, "·") : null,
            row.published_at
              ? h("span", { title: dateTime(row.published_at) }, `published ${relative(row.published_at)}`)
              : h("span", { class: "faint" }, "never published"),
            row.expires_at ? h("span", { class: "dot-sep" }, "·") : null,
            row.expires_at
              ? h(
                  "span",
                  { class: row.state === "expired" ? "portal-due--overdue" : "", title: dateTime(row.expires_at) },
                  `expires ${relative(row.expires_at)}`,
                )
              : h("span", { class: "faint" }, "no expiry"),
          ),
        ),
        h("div", { class: "ann-actions" }, toggle),
      ),
      row.body
        ? h(
            "div",
            { class: "ann-body" },
            ...row.body.split(/\n{2,}/).map((para) => h("p", null, para)),
          )
        : h("div", { class: "ann-body ann-body--empty" }, "No body — only the title is shown."),
      h(
        "footer",
        { class: "ann-foot" },
        h("span", { class: feedEnabled ? "faint" : "faint portal-due--overdue" }, stateReason(row.state, feedEnabled)),
        h("span", { class: "spacer" }),
        btn("Edit", { size: "sm", variant: "ghost", icon: "edit", onclick: () => editDialog(row) }),
        btn("Delete", { size: "sm", variant: "ghost", icon: "trash", onclick: () => void remove(row) }),
      ),
    );
  };

  paint();

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Announcements",
      sub: "What customers are told on the storefront and in their dashboard. A published announcement is visible until it expires; a draft is visible to nobody.",
      actions: btn("New announcement", { variant: "primary", icon: "plus", onclick: () => editDialog(null) }),
    }),
    host,
  );
}

// ----------------------------------------------------------------- helpers --

/** The badge for one of the four states.
 *
 *  The server names the state; this only picks a tone. A screen that re-derived the answer from
 *  `published`, `published_at` and `expires_at` would be comparing them against the *browser's*
 *  clock, which is how "Live" and "not showing" end up on the same row in two different tabs. */
function stateBadge(state: string): HTMLElement {
  switch (state) {
    case "live":
      return badge("Live", "success", { dot: true });
    case "scheduled":
      return badge("Scheduled", "info", { dot: true });
    case "expired":
      return badge("Expired", "neutral", { dot: true });
    case "draft":
      return badge("Draft", "neutral", { dot: true });
    default:
      // A state this build does not know is reported rather than guessed at: an unknown label is
      // a fact about the deployment, and inventing "Live" for it would be a lie with a badge on.
      return badge(state || "Unknown", "neutral");
  }
}

/** Why a customer does or does not see this row, in the operator's words.
 *
 *  With the feed switched off, every row is invisible however live it is — and that is the one
 *  answer the operator cannot get anywhere else on this page. */
function stateReason(state: string, feedEnabled: boolean): string {
  if (!feedEnabled) {
    return "The feed is switched off, so no customer sees this whatever its state.";
  }
  switch (state) {
    case "live":
      return "Customers can see this now.";
    case "scheduled":
      return "Published, but the publish time has not arrived, so no customer sees it yet.";
    case "expired":
      return "Published, but its expiry has passed, so no customer sees it.";
    case "draft":
      return "Only staff can see this.";
    default:
      return "This build does not recognise the row's state; reload to ask the server again.";
  }
}

function placementLabel(value: string): string {
  return PLACEMENTS.find((entry) => entry.value === value)?.label ?? value;
}

/** A `datetime-local` value from an RFC3339 timestamp, in the browser's own zone. */
function localInput(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

/** An RFC3339 timestamp from a `datetime-local` value. Empty is "not set"; `null` is "not a
 *  date", which the form reports instead of sending. */
function isoFromLocal(value: string): string | null {
  const trimmed = value.trim();
  if (!trimmed) return "";
  const date = new Date(trimmed);
  if (Number.isNaN(date.getTime())) return null;
  return date.toISOString();
}

function announcementError(err: unknown): string {
  if (!(err instanceof ApiError)) return "The server could not be reached, so nothing was saved.";
  switch (err.code) {
    case "invalid_announcement":
      return err.detail || "The announcement was refused. Check the title, the body and the placement.";
    case "not_found":
      return "That announcement no longer exists. Reload the page.";
    case "forbidden":
      return "You do not hold announcement:write, so nothing was saved.";
    default:
      return err.detail || `The announcement could not be saved (${err.code}).`;
  }
}
