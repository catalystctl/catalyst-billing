// The in-app inbox, and the per-event preference toggles beside it.
//
// Two lists, one screen, because they answer the same question from two directions: "what have I
// been told" and "what do I want to be told". A customer who opens a payment-failed notice and
// decides they never want another one should not have to find a different page to say so.
//
// The payloads are the handler's, exactly:
//
//   GET  /api/v1/portal/notifications            -> {data: [...], next_cursor}   (a Page)
//   POST /api/v1/portal/notifications/{id}/read  -> {status, unread}
//   POST /api/v1/portal/notifications/read-all   -> {status, marked, unread}
//   GET  /api/v1/portal/notifications/preferences -> {data: [{event_key, name, force, preference, stored, mail, in_app}]}
//   PUT  /api/v1/portal/notifications/preferences -> {data: [...]}  (the whole set, replaced)
//
// The preference model has four values — force, choice_on, choice_off, never — and the screen
// renders only two states, on and off, because that is what a customer can *choose*. A `force`
// event is not a toggle: it is a locked row with an explanation, and it is locked in the UI for
// the same reason the server refuses it with 422 `force_event` — opting out of a password-reset
// mail does not protect anyone, it just makes the account unrecoverable.
//
// The PUT replaces the whole set, so this screen always sends every row it is showing. Sending
// only the row that changed would reset every other event to its default, which is the classic
// "PATCH implemented as PUT" data loss.

import { ApiError, type Page } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateTime, relative } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  badge,
  banner,
  btn,
  card,
  emptyState,
  errorState,
  pageHead,
  resource,
  skeletonTable,
  withBusy,
} from "../ui/kit";

export const meta = { title: "Notifications", crumb: "Notifications" };

// ---------------------------------------------------------------- payloads --

interface InboxRow {
  id: string;
  event_key: string;
  title: string;
  body: string;
  url: string;
  unread: boolean;
  created_at: string;
  read_at?: string;
}

interface PreferenceRow {
  event_key: string;
  name: string;
  /** True when the event cannot be turned off. Derived from the catalogue, not from the row. */
  force: boolean;
  /** The effective preference: force, choice_on, choice_off or never. */
  preference: string;
  /** What the user actually stored, empty when they have never expressed one. */
  stored: string;
  mail: boolean;
  in_app: boolean;
}

export default async function portalNotificationsPage(ctx: Ctx): Promise<Node> {
  const inbox = inboxSection(ctx);

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Notifications",
      sub: "What we have told you, and which events you want to hear about.",
      actions: btn("Back to overview", { href: "/portal", size: "sm", icon: "arrowLeft" }),
    }),
    h(
      "div",
      { class: "stack" },
      inbox.el,
      card({
        title: "What you are told about",
        sub: "These choices apply to every channel. An event that is required cannot be switched off.",
        body: preferencesSection(ctx),
      }),
    ),
  );
}

// ------------------------------------------------------------------- inbox --

interface InboxState {
  rows: InboxRow[];
  next: string | null;
  unread: number;
}

/** Loaded once, then reloaded in place after a mutation, so a mark-read does not remount the
 *  screen and lose the scroll position of a long inbox. */
function inboxSection(ctx: Ctx): { el: HTMLElement; reload: () => void } {
  const host = h("div");
  const controller = new AbortController();
  ctx.onDispose(() => controller.abort());
  let generation = 0;
  let loaded = false;

  const render = (state: InboxState) => {
    if (state.rows.length === 0) {
      swap(
        host,
        card({
          flush: true,
          body: emptyState({
            title: "Nothing in your inbox",
            body: "Notices about invoices, payments and services arrive here as they happen. There is nothing to clear, and nothing is wrong.",
            icon: "inbox",
            action: btn("Back to overview", { href: "/portal", size: "sm", icon: "home" }),
          }),
        }),
      );
      return;
    }

    const markAll = btn("Mark all read", { size: "sm", variant: "ghost", icon: "check" }) as HTMLButtonElement;
    markAll.disabled = state.unread === 0;
    markAll.addEventListener("click", () => {
      void withBusy(ctx, markAll, async () => {
        try {
          await ctx.api.post("/api/v1/portal/notifications/read-all", {});
        } catch (err) {
          ctx.toast("The inbox could not be marked read", {
            tone: "danger",
            detail: err instanceof ApiError ? err.detail || err.code : undefined,
          });
          return;
        }
        ctx.toast("Inbox marked read", { tone: "success" });
        load();
      });
    });

    swap(
      host,
      card({
        title: "Inbox",
        sub:
          state.unread > 0
            ? `${state.unread} unread`
            : "Everything here has been read",
        actions: h("div", { class: "row" }, markAll),
        flush: true,
        body: h("div", { class: "notif-list" }, ...state.rows.map((row) => inboxRow(ctx, row, load))),
        foot: h(
          "span",
          { class: "row" },
          icon("info", 12),
          "A notice links to the record it is about. Reading one does not change anything else.",
        ),
      }),
    );
  };

  const load = async () => {
    const mine = ++generation;
    if (!loaded) swap(host, skeletonTable(5, 3));
    try {
      const [page, count] = await Promise.all([
        ctx.api.get<Page<InboxRow>>("/api/v1/portal/notifications", { query: { limit: 25 }, cache: false, signal: controller.signal }),
        // The badge number is its own endpoint because the shell polls it; asking here means the
        // header count and the list agree even when only one of them has been read.
        ctx.api
          .get<{ unread?: number }>("/api/v1/portal/notifications/unread-count", { cache: false, signal: controller.signal })
          .catch(() => ({ unread: undefined as number | undefined })),
      ]);
      if (mine !== generation) return;
      loaded = true;
      const rows = Array.isArray(page?.data) ? page.data : [];
      const unread = typeof count?.unread === "number" ? count.unread : rows.filter((row) => row.unread).length;
      render({ rows, next: page?.next_cursor ?? null, unread });
    } catch (err) {
      if (mine !== generation) return;
      if (controller.signal.aborted || (err instanceof DOMException && err.name === "AbortError")) return;
      loaded = true;
      swap(
        host,
        errorState(err, () => {
          void load();
        }),
      );
    }
  };

  void load();

  return { el: host, reload: () => void load() };
}

function inboxRow(ctx: Ctx, row: InboxRow, reload: () => void): HTMLElement {
  const mark = btn("Mark read", { size: "sm", variant: "ghost", icon: "check" }) as HTMLButtonElement;
  mark.addEventListener("click", () => {
    void withBusy(ctx, mark, async () => {
      try {
        await ctx.api.post(`/api/v1/portal/notifications/${encodeURIComponent(row.id)}/read`, {});
      } catch (err) {
        ctx.toast("That notice could not be marked read", {
          tone: "danger",
          detail: err instanceof ApiError ? err.detail || err.code : undefined,
        });
        return;
      }
      reload();
    });
  });

  // A notice's own `url` is the record it is about. It is rendered as a link only when it is an
  // internal path: a notification body is written by an operator, and an absolute URL there
  // would be a redirect off the panel wearing our chrome.
  const href = typeof row.url === "string" && row.url.startsWith("/") ? row.url : "";

  return h(
    "article",
    { class: `notif-row${row.unread ? " notif-row--unread" : ""}` },
    h(
      "div",
      { class: "notif-main" },
      h(
        "div",
        { class: "notif-title" },
        row.unread ? h("span", { class: "notif-dot", "aria-label": "Unread" }) : null,
        href ? h("a", { class: "row-title", href }, row.title || "Notification") : h("span", { class: "row-title" }, row.title || "Notification"),
        badge(row.event_key.replace(/[._]+/g, " "), "neutral"),
      ),
      row.body ? h("p", { class: "notif-body" }, row.body) : null,
      h(
        "div",
        { class: "notif-meta" },
        h("span", { title: dateTime(row.created_at) }, relative(row.created_at)),
        row.read_at ? h("span", { class: "dot-sep" }, "·") : null,
        row.read_at ? h("span", { title: dateTime(row.read_at) }, `read ${relative(row.read_at)}`) : null,
      ),
    ),
    h(
      "div",
      { class: "notif-side" },
      row.unread ? mark : h("span", { class: "faint nowrap" }, "Read"),
      href ? btn("Open", { href, size: "sm", variant: "ghost", iconAfter: "chevronRight" }) : null,
    ),
  );
}

// ------------------------------------------------------------- preferences --

function preferencesSection(ctx: Ctx): HTMLElement {
  const state: { rows: PreferenceRow[] } = { rows: [] };

  return resource(
    ctx,
    async (signal) => {
      const body = await ctx.api.get<{ data?: unknown }>("/api/v1/portal/notifications/preferences", { cache: false, signal });
      const rows = Array.isArray(body?.data) ? (body.data as PreferenceRow[]) : [];
      return { rows };
    },
    (data) => {
      state.rows = data.rows;
      if (data.rows.length === 0) {
        return emptyState({
          title: "No notification events are configured",
          body: "The server returned an empty catalogue, which means this installation sends no customer notices. Nothing here needs your attention.",
          icon: "radio",
        });
      }
      return preferenceList(ctx, state);
    },
    { skeleton: () => skeletonTable(6, 2) },
  );
}

function preferenceList(ctx: Ctx, state: { rows: PreferenceRow[] }): HTMLElement {
  const noticeHost = h("div");
  const listHost = h("div");
  const save = btn("Save preferences", { variant: "primary", size: "sm", type: "submit" }) as HTMLButtonElement;

  // The pending values, keyed by event. Seeded from the effective preference so a customer who
  // saves without touching anything writes back what they already had.
  const pending = new Map<string, boolean>();
  for (const row of state.rows) pending.set(row.event_key, isOn(row));

  const renderList = () => {
    const rows = state.rows.map((row) => {
        if (row.force) {
          // A force event is not a disabled checkbox: a greyed control invites a click and then
          // explains nothing. This is a statement with the reason next to it.
          return h(
            "div",
            { class: "pref-row pref-row--locked" },
            h(
              "div",
              { class: "pref-main" },
              h(
                "div",
                { class: "pref-title" },
                icon("lock", 12),
                h("span", null, row.name || row.event_key),
                badge("Required", "warning", { dot: true }),
              ),
              h(
                "p",
                { class: "pref-hint" },
                "This notice cannot be turned off. It is part of keeping the account usable — a password reset or an address verification you cannot receive is an account you cannot recover.",
              ),
            ),
            h("div", { class: "pref-side" }, h("span", { class: "faint nowrap" }, "Always on")),
          );
        }
        const box = h("input", {
          type: "checkbox",
          checked: pending.get(row.event_key) ?? true,
          onchange: () => pending.set(row.event_key, box.checked),
        });
        return h(
          "div",
          { class: "pref-row" },
          h(
            "div",
            { class: "pref-main" },
            h("div", { class: "pref-title" }, h("span", null, row.name || row.event_key)),
            h(
              "p",
              { class: "pref-hint" },
              row.stored === ""
                ? "On by default. You have not changed this."
                : isOn(row)
                  ? "You have this on."
                  : "You have this off.",
            ),
          ),
          h("div", { class: "pref-side" }, h("label", { class: "switch" }, box, h("span", { class: "sr-only" }, `Toggle ${row.name || row.event_key}`))),
        );
      });
    swap(listHost, h("div", { class: "pref-list" }, ...rows));
  };

  renderList();

  const form = h(
    "form",
    { class: "stack", novalidate: true },
    noticeHost,
    listHost,
    h(
      "div",
      { class: "row-wrap" },
      save,
      h(
        "span",
        { class: "field-hint" },
        "The whole set is saved at once. An event you leave alone keeps its current setting.",
      ),
    ),
  );

  form.addEventListener("submit", (event) => {
    event.preventDefault();
    void withBusy(ctx, save, async () => {
      noticeHost.replaceChildren();
      // A required event is **omitted**, not sent. `force` is derived from the catalogue and is
      // not an input: `handlePutNotificationPreferences` accepts only choice_on, choice_off and
      // never, and `notify.SetPreference` refuses `force` outright. An omitted event keeps its
      // default, and the default for a force event is "on and not optional" — which is exactly
      // what the locked row already shows.
      const preferences = state.rows
        .filter((row) => !row.force)
        .map((row) => ({
          event_key: row.event_key,
          preference: pending.get(row.event_key) ? "choice_on" : "choice_off",
        }));

      let saved: PreferenceRow[];
      try {
        const body = await ctx.api.put<{ data?: unknown }>("/api/v1/portal/notifications/preferences", { preferences });
        saved = Array.isArray(body?.data) ? (body.data as PreferenceRow[]) : [];
      } catch (err) {
        noticeHost.replaceChildren(preferenceFailure(err));
        return;
      }

      if (saved.length > 0) {
        state.rows = saved;
        for (const row of saved) pending.set(row.event_key, isOn(row));
        renderList();
      }
      noticeHost.replaceChildren(
        banner("success", "Preferences saved", "These choices apply to every device on the account, not just this browser."),
      );
      ctx.toast("Notification preferences saved", { tone: "success" });
    });
  });

  return form;
}

function preferenceFailure(err: unknown): HTMLElement {
  if (!(err instanceof ApiError)) {
    return banner("danger", "The preferences were not saved", "The server could not be reached. Nothing has changed.");
  }
  switch (err.code) {
    case "force_event":
      return banner(
        "danger",
        "A required notice cannot be turned off",
        "One of these events is required to keep the account usable, so the server refused the whole set. Reload the page to see which row is locked.",
      );
    case "unknown_event":
      return banner("danger", "One of these events no longer exists", "Reload the page to fetch the current catalogue.");
    case "invalid_preference":
      return banner("danger", "That preference is not one of the allowed values", err.detail || "Reload and try again.");
    default:
      return banner("danger", "The preferences were not saved", err.detail || err.code);
  }
}

/** A row is "on" when the effective preference is force or choice_on. `never` is an operator's
 *  decision and cannot be stored by a customer, so it renders as off if it ever arrives. */
function isOn(row: PreferenceRow): boolean {
  return row.preference === "force" || row.preference === "choice_on";
}
