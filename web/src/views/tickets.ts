// Support tickets: the list, plus the vocabulary the three ticket screens share.
//
// One screen, two audiences. `/tickets` is the same route for a customer and for staff, and the
// difference is the *endpoint*, not the component: `/api/v1/portal/tickets` is scoped to the
// caller's customer in SQL, `/api/v1/staff/tickets` is the queue. The server decides which rows
// exist for the caller; this module only decides which columns describe them. Building two
// screens would have meant two empty states, two pagers and two answers to "what does replied
// mean" that drift the first time one of them is edited.
//
// The payload is `{tickets: [...], next_cursor}` — not the `{data, next_cursor}` page shape the
// staff tables use — so the list is built here rather than with `createTable`. It keeps the same
// four states (skeleton, empty with a next step, error with retry, rows), the same keyset pager,
// and the same card-per-row layout below 780px, because the markup is the same `.table` markup
// `createTable` emits.
//
// Everything about a ticket on this page is read from the handler: status is one of open,
// replied, closed; priority is low, normal, high; the department is whatever the operator
// configured, which is why it is rendered with titleCase and never compared to a literal.

import { ApiError, type RequestOptions } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateTime, relative, titleCase } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  badge,
  btn,
  emptyState,
  errorState,
  field,
  input,
  pageHead,
  select,
  skeletonTable,
  type EmptyOptions,
} from "../ui/kit";
import type { Column } from "../ui/table";

export const meta = { title: "Tickets", crumb: "Tickets" };

// ---------------------------------------------------------------- payloads --

/** One stored file, as `attachmentJSON` emits it. `url` is the API path that serves the bytes;
 *  the screen links to it and never fetches it, so the download is the browser's own and the
 *  response's `Content-Disposition: attachment` is what names the file. */
export interface TicketAttachment {
  id: string;
  filename: string;
  content_type: string;
  byte_size: number;
  digest?: string;
  created_at?: string;
  url: string;
}

/** One message in a thread, as `messageJSON` emits it. */
export interface TicketMessage {
  id: string;
  ticket_id: string;
  from_staff: boolean;
  body: string;
  created_at: string;
  message_id?: string;
  author?: { id: string; name: string };
  attachments?: TicketAttachment[];
}

/** One ticket row. The staff payload adds `customer` and `assignee_name`; a customer's own
 *  payload cannot, because every field it carries is the ticket's own or their own. */
export interface TicketRow {
  id: string;
  number: string;
  subject: string;
  status: string;
  priority: string;
  department: string;
  message_count: number;
  created_at: string;
  updated_at: string;
  service_id?: string;
  assigned_to?: string;
  last_reply_at?: string;
  closed_at?: string;
  last_message?: { from_staff: boolean; body: string };
  customer?: { id: string; email: string; name: string };
  assignee_name?: string;
  /** True when a member of the team opened this ticket, rather than the customer. `opened_by` is
   *  then the staff user's id. Both are absent on a ticket the customer wrote. */
  opened_by_staff?: boolean;
  opened_by?: string;
}

/** A ticket and its messages. */
export interface TicketThread {
  ticket: TicketRow;
  messages: TicketMessage[];
  /** The operator's custom fields, each with this ticket's answer or the fact that it has none.
   *  Present for both audiences: a customer needs to see which field a refused close is about. */
  fields: TicketFieldValue[];
}

/** One configured field, as `ticketFieldJSON` emits it. */
export interface TicketField {
  id: string;
  key: string;
  label: string;
  type: "text" | "select" | "checkbox" | string;
  options: string[];
  required_on_create: boolean;
  required_to_close: boolean;
  sort_order: number;
  archived: boolean;
  /** How many tickets carry an answer. What tells an operator that archiving keeps the history. */
  answer_count: number;
  created_at: string;
  updated_at: string;
}

/** A field plus one ticket's answer to it.
 *
 *  `value` is a string for text and select and a boolean for checkbox, and `null` when the field is
 *  unanswered — which is not the same as "" or false, and the reason the server stores the answer
 *  in typed columns rather than a JSON blob. */
export interface TicketFieldValue extends TicketField {
  answered: boolean;
  value: string | boolean | null;
  answered_at?: string;
}

/** What `GET .../tickets/meta` answers: the form's configuration, not a ticket. */
export interface TicketMeta {
  enabled: boolean;
  client_closing_disabled: boolean;
  departments: string[];
  priorities: string[];
  statuses: string[];
  max_attachment_bytes: number;
  max_subject_length: number;
  autoclose_days?: number;
  /** The types an upload may carry. The server enforces it; the form states it before a 10 MB
   *  file is sent, because a 415 after the upload is a worse answer than a sentence before it. */
  allowed_attachment_types?: string[];
  /** The fields a new ticket asks for. */
  custom_fields?: TicketField[];
}

/** A row that is not a ticket and not an error: "there is nothing here for you".
 *
 *  A 403 `tickets_disabled` and a 404 "no customer record" are answers, not failures — the same
 *  request will produce the same answer — so they render as an empty state with an explanation
 *  rather than as an error with a retry button that cannot work. */
export class NotAvailable extends Error {
  readonly title: string;
  readonly body: string;

  constructor(title: string, body: string) {
    super(title);
    this.name = "NotAvailable";
    this.title = title;
    this.body = body;
  }
}

// ------------------------------------------------------------- endpoints --

/** True when the caller is staff, which is what selects the queue endpoint. */
export function isStaffTickets(ctx: Ctx): boolean {
  return ctx.session.user.is_staff;
}

/** The list endpoint for the caller. */
export function ticketsBase(ctx: Ctx): string {
  return isStaffTickets(ctx) ? "/api/v1/staff/tickets" : "/api/v1/portal/tickets";
}

/** The meta endpoint for the caller. */
export function ticketsMetaPath(ctx: Ctx): string {
  return `${ticketsBase(ctx)}/meta`;
}

/** One thread. */
export function ticketPath(ctx: Ctx, id: string): string {
  return `${ticketsBase(ctx)}/${encodeURIComponent(id)}`;
}

/** Where a ticket screen sends the browser. One route, two audiences. */
export function ticketHref(id: string): string {
  return `/tickets/${encodeURIComponent(id)}`;
}

// ------------------------------------------------------------- presentation --

/** The status colours, decided once. `replied` is the one that needs attention: the customer
 *  has written and is waiting, so it is the warning tone rather than the same blue as `open`. */
export function ticketStatusBadge(status: string): HTMLElement {
  const tone = status === "replied" ? "warning" : status === "open" ? "info" : "neutral";
  return badge(humanTicketStatus(status), tone, { dot: true });
}

export function humanTicketStatus(status: string | null | undefined): string {
  switch (status) {
    case "open":
      return "Open";
    case "replied":
      return "Replied";
    case "closed":
      return "Closed";
    case "":
    case null:
    case undefined:
      return "—";
    default:
      return titleCase(status);
  }
}

/** Priorities are a closed set of three, so the tone is a lookup rather than a guess. */
export function ticketPriorityBadge(priority: string): HTMLElement {
  const tone = priority === "high" ? "warning" : priority === "normal" ? "neutral" : "neutral";
  return badge(titleCase(priority), tone);
}

/** "10 MB" from a byte count. Integer arithmetic only: this is a size, not money, but the
 *  habit of dividing by a power of two and rounding is the same one that keeps money exact. */
export function bytesLabel(bytes: number | null | undefined): string {
  if (typeof bytes !== "number" || !Number.isFinite(bytes) || bytes <= 0) return "—";
  if (bytes >= 1024 * 1024) {
    const mb = bytes / (1024 * 1024);
    return `${Number.isInteger(mb) ? mb : mb.toFixed(1)} MB`;
  }
  if (bytes >= 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${bytes} bytes`;
}

/** One attachment, as a link to the API path that serves it.
 *
 *  The href is the endpoint the server put in the payload, not a blob URL: the response sets
 *  `Content-Disposition: attachment`, `nosniff` and a sandboxing CSP, and none of that survives
 *  a `URL.createObjectURL` detour. `download` also tells the router to leave the navigation to
 *  the browser (see lib/router.ts), so the file does not become a page. */
export function attachmentLink(a: TicketAttachment): HTMLElement {
  const url = typeof a.url === "string" && a.url.startsWith("/") ? a.url : "";
  const name = a.filename || "attachment";
  const meta = [bytesLabel(a.byte_size), a.content_type].filter(Boolean).join(" · ");
  if (!url) {
    return h(
      "span",
      { class: "ticket-attachment ticket-attachment--missing", title: "This file has no download link" },
      icon("alertTriangle", 12),
      h("span", null, name),
    );
  }
  return h(
    "a",
    { class: "ticket-attachment", href: url, download: name, title: `${name} (${meta})` },
    icon("download", 12),
    h("span", { class: "ticket-attachment-name" }, name),
    h("span", { class: "ticket-attachment-size" }, bytesLabel(a.byte_size)),
  );
}

// -------------------------------------------------------------- the list --

interface ListPayload<T> {
  rows: T[];
  next: string | null;
}

export interface TicketTableOptions<T> {
  /** The API path, without the query. */
  path: string;
  /** The current filters, read at request time so a change is one call. */
  query: () => Record<string, string | undefined>;
  columns: Column<T>[];
  rowHref: (row: T) => string;
  /** The empty state, computed when it is needed so it can name the active filter. */
  empty: () => EmptyOptions;
  /** Normalises the handler's payload. `null` where a list is expected becomes `[]` here, once,
   *  so no cell renderer has to remember that the wire does that. */
  load: (payload: unknown) => ListPayload<T>;
  /** Maps a failure to something else before it is rendered: the two "nothing here for you"
   *  answers become a `NotAvailable` and render as an empty state, not as an error. */
  mapError?: (err: unknown) => unknown;
  limit?: number;
  /** A page keeps a count or a summary from the last page it saw. */
  onLoaded?: (rows: T[], next: string | null) => void;
}

export interface TicketTable<T> {
  el: HTMLElement;
  reload(): void;
  rows(): T[];
}

/** The list, built by hand because the payload is `{tickets: [...]}` rather than `{data: [...]}`.
 *
 *  It is deliberately shaped like `createTable` — same `.table` markup, same keyset cursor
 *  stack, same card layout on a phone — so the two look and behave identically. */
export function ticketTable<T extends { id: string }>(ctx: Ctx, opts: TicketTableOptions<T>): TicketTable<T> {
  const wrap = h("div", { class: "table-wrap table-wrap--cards" });
  const foot = h("div");
  const el = h("section", { class: "card" }, wrap, foot);

  let cursorStack: Array<string | null> = [null];
  let index = 0;
  let generation = 0;
  let current: T[] = [];
  let next: string | null = null;
  let loaded = false;

  const controller = new AbortController();
  ctx.onDispose(() => controller.abort());

  const renderRows = (rows: T[]) => {
    const thead = h("thead", null, h("tr", null, ...opts.columns.map(headerCell)));
    const tbody = h("tbody");
    for (const row of rows) tbody.appendChild(bodyRow(ctx, row, opts));
    swap(wrap, h("div", { class: "table-wrap table-wrap--cards" }, h("table", { class: "table" }, thead, tbody)));
  };

  const renderFoot = () => {
    const hasPrev = index > 0;
    const hasNext = Boolean(next);
    if (!hasPrev && !hasNext) {
      swap(foot, h("div"));
      return;
    }
    swap(
      foot,
      h(
        "div",
        { class: "table-foot" },
        h("span", null, `${current.length} row${current.length === 1 ? "" : "s"}`),
        h(
          "div",
          { class: "pager" },
          h(
            "button",
            {
              class: "pager-btn",
              type: "button",
              disabled: !hasPrev,
              onclick: () => {
                if (index === 0) return;
                index -= 1;
                void load();
              },
            },
            icon("chevronLeft", 12),
            "Previous",
          ),
          h(
            "button",
            {
              class: "pager-btn",
              type: "button",
              disabled: !hasNext,
              onclick: () => {
                if (!next) return;
                cursorStack = cursorStack.slice(0, index + 1);
                cursorStack.push(next);
                index += 1;
                void load();
              },
            },
            "Next",
            icon("chevronRight", 12),
          ),
        ),
      ),
    );
  };

  const load = async (): Promise<void> => {
    const mine = ++generation;
    el.classList.add("table-loading");
    if (!loaded) swap(wrap, skeletonTable(7, Math.min(opts.columns.length, 6)));
    try {
      const payload = await ctx.api.get<unknown>(opts.path, {
        query: { ...opts.query(), limit: opts.limit ?? 25, cursor: cursorStack[index] ?? undefined },
        cache: false,
        signal: controller.signal,
      });
      if (mine !== generation) return;
      const page = opts.load(payload);
      current = page.rows;
      next = page.next;
      loaded = true;
      if (current.length === 0) {
        swap(wrap, emptyState(opts.empty()));
        swap(foot, h("div"));
      } else {
        renderRows(current);
        renderFoot();
      }
      opts.onLoaded?.(current, next);
    } catch (rawErr) {
      if (mine !== generation) return;
      if (controller.signal.aborted || (rawErr instanceof DOMException && rawErr.name === "AbortError")) return;
      loaded = true;
      swap(foot, h("div"));
      const err = opts.mapError ? opts.mapError(rawErr) : rawErr;
      if (err instanceof NotAvailable) {
        swap(wrap, emptyState({ title: err.title, body: err.body, icon: "lock" }));
        return;
      }
      swap(
        wrap,
        errorState(err, () => {
          swap(wrap, skeletonTable(7, Math.min(opts.columns.length, 6)));
          void load();
        }),
      );
    } finally {
      if (mine === generation) el.classList.remove("table-loading");
    }
  };

  void load();

  return {
    el,
    reload() {
      // A filter change resets the cursor: a cursor is only meaningful for the query that
      // produced it.
      cursorStack = [null];
      index = 0;
      void load();
    },
    rows: () => current,
  };
}

function headerCell<T>(col: Column<T>): HTMLElement {
  return h(
    "th",
    {
      scope: "col",
      class: [col.align === "right" ? "num" : "", col.hideSmall ? "hide-sm" : ""].filter(Boolean).join(" "),
      width: col.width,
    },
    col.label,
  );
}

function bodyRow<T extends { id: string }>(ctx: Ctx, row: T, opts: TicketTableOptions<T>): HTMLElement {
  const href = opts.rowHref(row);
  const tr = h("tr", { class: "is-clickable", dataset: { href } });
  tr.addEventListener("click", (event) => {
    const target = event.target as Element | null;
    // A real control inside the row keeps its own behaviour.
    if (target?.closest("a,button,input,select,textarea,label")) return;
    ctx.navigate(href);
  });
  for (const col of opts.columns) {
    tr.appendChild(
      h(
        "td",
        {
          class: [
            col.align === "right" ? "num" : "",
            col.primary ? "cell-primary" : "",
            col.hideSmall ? "hide-sm" : "",
            col.className ?? "",
          ]
            .filter(Boolean)
            .join(" "),
          "data-label": col.primary ? undefined : col.label,
        },
        col.render(row),
      ),
    );
  }
  return tr;
}

// -------------------------------------------------------------- the page --

export default async function ticketsPage(ctx: Ctx): Promise<Node> {
  const staff = isStaffTickets(ctx);
  const canWrite = ctx.can(staff ? "ticket:write:all" : "ticket:write:self");
  let status = (ctx.route.query.get("status") ?? "").trim();
  if (status && !["open", "replied", "closed"].includes(status)) status = "";

  const queryOf = () => ({ status: status || undefined });

  // The filter and the table refer to each other: the filter's change handler reloads the
  // table, and the empty state's "clear the filter" button does too. Both are declared before
  // either runs, so neither is a closure over a `let` that has not been assigned yet.
  let table: TicketTable<TicketRow>;

  const clearFilter = () => {
    status = "";
    statusSelect.value = "";
    ctx.replaceQuery({ status: null });
    table.reload();
  };

  const statusSelect = select(
    [
      { value: "", label: "All statuses" },
      { value: "open", label: "Open" },
      { value: "replied", label: "Replied" },
      { value: "closed", label: "Closed" },
    ],
    {
      id: "ticket-status",
      value: status,
      onchange: (event: Event) => {
        status = (event.target as HTMLSelectElement).value;
        ctx.replaceQuery({ status: status || null });
        table.reload();
      },
    },
  );

  const columns: Column<TicketRow>[] = staff ? staffColumns() : customerColumns();

  table = ticketTable<TicketRow>(ctx, {
    path: ticketsBase(ctx),
    query: queryOf,
    columns,
    rowHref: (row) => ticketHref(row.id),
    limit: 25,
    load: normaliseTickets,
    mapError: (err) => unavailableReason(err) ?? err,
    empty: () => {
      // The empty state has to say two different things — "nothing is waiting, which is good"
      // and "your filter matches nothing, which is not the same thing" — and it has to offer
      // the next step rather than a dead end.
      if (status) {
        return {
          title: `No ${humanTicketStatus(status).toLowerCase()} tickets`,
          body: staff
            ? "Every ticket in this view has moved on. Clear the filter to see the whole queue."
            : "You have no ticket in this state. Clearing the filter shows the rest of your history.",
          icon: "message",
          action: btn("Show every ticket", { size: "sm", icon: "x", onclick: clearFilter }),
        };
      }
      if (staff) {
        return {
          title: "The support queue is empty",
          body: "Nothing is waiting for an answer. A ticket appears here the moment a customer opens one, and it stays until it is closed.",
          icon: "message",
          action: btn("Back to overview", { href: "/", size: "sm", icon: "home" }),
        };
      }
      return {
        title: "No tickets yet",
        body: "A ticket is how you reach support. Opening one sends it to the team with the details you write, and their reply arrives here.",
        icon: "message",
        action: canWrite
          ? btn("Open a ticket", { href: "/tickets/new", size: "sm", variant: "primary", icon: "plus" })
          : btn("Back to overview", { href: "/portal", size: "sm", icon: "home" }),
      };
    },
  });

  const actions = h(
    "div",
    { class: "row" },
    !staff && canWrite
      ? btn("New ticket", { href: "/tickets/new", variant: "primary", icon: "plus", size: "sm" })
      : null,
    btn("Refresh", { icon: "refresh", size: "sm", onclick: () => table.reload() }),
  );

  return h(
    "div",
    { class: "page" },
    pageHead({
      // Staff have two entries into the same table: /tickets is the plain list and /support is
      // the filtered queue. Two screens called "Support queue" would be indistinguishable in the
      // breadcrumb, so the plain one says what it is.
      title: staff ? "All tickets" : "Support tickets",
      sub: staff
        ? "Every ticket on the platform, newest first. Each row links to the thread and to the customer it belongs to."
        : "Every conversation you have had with support. A reply from the team marks the ticket replied; closing it ends the conversation.",
      actions,
    }),
    h(
      "div",
      { class: "stack" },
      h(
        "div",
        { class: "toolbar" },
        h("label", { class: "support-filter", for: "ticket-status" }, h("span", { class: "faint" }, "Status"), statusSelect),
        h(
          "div",
          { class: "toolbar-end" },
          staff ? btn("Queue filters", { href: "/support", size: "sm", icon: "filter" }) : null,
        ),
      ),
      table.el,
    ),
  );
}

// ------------------------------------------------------------------ columns --

function customerColumns(): Column<TicketRow>[] {
  return [
    {
      key: "subject",
      label: "Ticket",
      primary: true,
      render: (row) =>
        h(
          "div",
          { class: "cell-main" },
          h(
            "div",
            { class: "primary" },
            h("a", { class: "row-title", href: ticketHref(row.id) }, row.subject || "(no subject)"),
          ),
          h(
            "div",
            { class: "secondary" },
            h("span", { class: "mono" }, row.number || "—"),
            h("span", { class: "dot-sep" }, "·"),
            h("span", null, titleCase(row.department)),
            h("span", { class: "dot-sep" }, "·"),
            h("span", null, `${row.message_count} message${row.message_count === 1 ? "" : "s"}`),
          ),
        ),
    },
    { key: "status", label: "Status", render: (row) => ticketStatusBadge(row.status) },
    { key: "priority", label: "Priority", render: (row) => ticketPriorityBadge(row.priority) },
    {
      key: "last_reply",
      label: "Last reply",
      hideSmall: true,
      render: (row) =>
        row.last_reply_at
          ? h("span", { class: "dim", title: dateTime(row.last_reply_at) }, relative(row.last_reply_at))
          : h("span", { class: "faint" }, "No reply yet"),
    },
    {
      key: "updated",
      label: "Updated",
      align: "right",
      hideSmall: true,
      render: (row) => h("span", { class: "dim nowrap", title: dateTime(row.updated_at) }, relative(row.updated_at)),
    },
  ];
}

function staffColumns(): Column<TicketRow>[] {
  return [
    {
      key: "subject",
      label: "Ticket",
      primary: true,
      render: (row) =>
        h(
          "div",
          { class: "cell-main" },
          h(
            "div",
            { class: "primary" },
            h("a", { class: "row-title", href: ticketHref(row.id) }, row.subject || "(no subject)"),
          ),
          h(
            "div",
            { class: "secondary" },
            h("span", { class: "mono" }, row.number || "—"),
            row.customer ? h("span", { class: "dot-sep" }, "·") : null,
            row.customer
              ? h(
                  "a",
                  { class: "linkish", href: `/customers/${encodeURIComponent(row.customer.id)}` },
                  row.customer.name || row.customer.email,
                )
              : null,
          ),
        ),
    },
    { key: "status", label: "Status", render: (row) => ticketStatusBadge(row.status) },
    { key: "priority", label: "Priority", render: (row) => ticketPriorityBadge(row.priority) },
    {
      key: "department",
      label: "Department",
      hideSmall: true,
      render: (row) => h("span", { class: "dim" }, titleCase(row.department)),
    },
    {
      key: "assignee",
      label: "Assignee",
      hideSmall: true,
      render: (row) =>
        row.assigned_to
          ? h("span", null, row.assignee_name || "Assigned")
          : h("span", { class: "faint" }, "Unassigned"),
    },
    {
      key: "updated",
      label: "Updated",
      align: "right",
      render: (row) => h("span", { class: "dim nowrap", title: dateTime(row.updated_at) }, relative(row.updated_at)),
    },
  ];
}

// --------------------------------------------------------------- normalise --

/** Reads `{tickets: [...], next_cursor}` defensively: the wire uses `null` for an empty list in
 *  places, and a missing array must render as "no rows" rather than as a crash. */
export function normaliseTickets(payload: unknown): { rows: TicketRow[]; next: string | null } {
  const body = (payload ?? {}) as { tickets?: unknown; next_cursor?: unknown };
  const rows = Array.isArray(body.tickets) ? (body.tickets as TicketRow[]) : [];
  const next = typeof body.next_cursor === "string" && body.next_cursor !== "" ? body.next_cursor : null;
  return { rows, next };
}

/** Reads a thread defensively. A missing `messages` is an empty thread, not an error. */
export function normaliseThread(payload: unknown): TicketThread {
  const body = (payload ?? {}) as { ticket?: unknown; messages?: unknown; fields?: unknown };
  const ticket = body.ticket as TicketRow | undefined;
  if (!ticket || typeof ticket.id !== "string") {
    // A 200 with no ticket is not an `ApiError`: it is a response this build cannot read, and
    // the honest state is "that did not work, try again" rather than a blank thread.
    throw new Error("The server answered without a ticket. Reloading may fix it.");
  }
  return {
    ticket,
    messages: Array.isArray(body.messages) ? (body.messages as TicketMessage[]) : [],
    // `null` where an array is expected is a wire habit, not a fact about this ticket: a ticket
    // with no configured fields has no fields, and that is an empty list.
    fields: Array.isArray(body.fields) ? (body.fields as TicketFieldValue[]) : [],
  };
}

/** Turns the two "there is nothing here for you" answers into an empty state.
 *
 *  A 403 `tickets_disabled` means the operator switched support off; a 404 on the list means
 *  this login has no billing customer behind it (a staff account using the portal, or an
 *  account that has never bought anything). Neither is a bug, neither will change on a retry,
 *  so neither renders as an error. Everything else is returned unchanged and becomes the
 *  error state. */
export function unavailableReason(err: unknown): NotAvailable | null {
  if (!(err instanceof ApiError)) return null;
  if (err.code === "tickets_disabled") {
    return new NotAvailable(
      "Support tickets are switched off",
      "This installation has support tickets disabled, so there is no desk to open one at. Your invoices, services and billing details still work.",
    );
  }
  if (err.status === 404 && err.code === "not_found") {
    return new NotAvailable(
      "No support history for this account",
      "This login is not linked to a billing customer, so there are no tickets to show. If you expect a conversation here, contact the team by email.",
    );
  }
  return null;
}

// ------------------------------------------------------- requests the client
//                                                         cannot yet express --
//
// One thing these screens must do is still not expressible through every build of `Api`:
//
//   The attachment upload is a *raw body*: the file's own bytes, with the name in `?filename=`
//   and the type in Content-Type. `Api.post` JSON-encodes its body, so a File would arrive as
//   `{}` — a 9 MB PDF stored as two bytes of JSON. `Api.postRaw` is that method; it was added to
//   lib/api.ts, so this is a capability check for a client that predates it rather than a
//   permanent gap.
//
// It is deliberately **not** worked around with a local `fetch`. A second HTTP client here would
// re-implement CSRF, the idempotency key, the problem+json envelope and the 401 → sign-in
// redirect, and the ticket screens would then be the ones that are silently wrong the next time
// one of those four changes. So the capability is detected instead: `ctx.api.postRaw` is called
// when the client has it, and when it does not the screen says so plainly and offers the action
// it *can* do. Nothing here is dead UI, and nothing bypasses `ctx.api`.
//
// (The attachment *download* needs neither: it is an `<a href>` to the API path, which is what
// `attachmentLink` renders. The handler sets `Content-Disposition: attachment`, so the browser
// saves it and the router leaves the navigation alone because of `download`.)

/** The methods this build may or may not have. Structural, so `Api` satisfies it the moment it
 *  grows them, and nothing is cast at a call site. */
interface ExtendableApi {
  patch?<T>(path: string, body?: unknown, options?: RequestOptions): Promise<T>;
  postRaw?<T>(path: string, body: BodyInit, options?: RequestOptions & { contentType?: string }): Promise<T>;
}

/** What this build's API client can and cannot do, asked once per screen. */
export interface ClientGaps {
  /** `false` when there is no `patch`, so a staff edit cannot be sent. */
  canPatch: boolean;
  /** `false` when there is no raw-body POST, so a file cannot be uploaded. */
  canUpload: boolean;
}

export function clientGaps(ctx: Ctx): ClientGaps {
  const api = ctx.api as unknown as ExtendableApi;
  return {
    canPatch: typeof api.patch === "function",
    canUpload: typeof api.postRaw === "function",
  };
}

/** One sentence for the screen to show when a control is absent, naming the *specific* thing
 *  that is missing rather than pretending the feature does not exist.
 *
 *  `which` matters: a customer's reply box has no business explaining that a staff status edit
 *  cannot be sent, and a support agent does not need to be told about file uploads when the
 *  uploads are working. Each caller names its own gap. */
export function gapExplanation(which: "patch" | "upload"): string {
  if (which === "patch") {
    return "Changing a ticket is a PATCH request, and this build's API client cannot send one, so the change cannot be sent. Nothing has changed.";
  }
  return "Attaching a file is a raw-body POST, and this build's API client cannot send one, so a file would arrive as an empty object. Nothing is uploaded and nothing is lost — the ticket or reply is created without the file.";
}

/** Changes status, priority or department on a staff ticket. Only the fields that moved are
 *  sent: the handler treats an omitted field as "leave it alone". */
export async function patchTicket(
  ctx: Ctx,
  id: string,
  changes: { status?: string; priority?: string; department?: string },
): Promise<TicketRow> {
  const api = ctx.api as unknown as ExtendableApi;
  if (typeof api.patch !== "function") {
    throw new ApiError(0, {
      title: "Not available in this build",
      detail: "The API client has no PATCH method, so this change cannot be sent. Nothing has changed.",
      code: "client_unsupported",
    });
  }
  const body: Record<string, string> = {};
  if (changes.status) body.status = changes.status;
  if (changes.priority) body.priority = changes.priority;
  if (changes.department) body.department = changes.department;
  return api.patch<TicketRow>(`/api/v1/staff/tickets/${encodeURIComponent(id)}`, body);
}

/** Assigns (or, with an empty assignee, unassigns) a staff ticket. This one is a plain POST with
 *  a JSON body, so it needs nothing beyond `ctx.api`. */
export async function assignTicket(ctx: Ctx, id: string, assignee: string): Promise<TicketRow> {
  return ctx.api.post<TicketRow>(`/api/v1/staff/tickets/${encodeURIComponent(id)}/assign`, { assignee });
}

/** Uploads one file to a ticket.
 *
 *  The cap is checked by the caller before this runs (that is what the client-side refusal in
 *  ticket-new.ts is for) and again by the server, which answers 413 `attachment_too_large`. The
 *  body is the file and nothing else, so `filename` and the message it belongs to travel in the
 *  query string — the shape `internal/httpapi/ticket_attachments.go` reads.
 *
 *  The endpoint is chosen from the session, exactly as the list and thread are: a staff member's
 *  upload goes to `/api/v1/staff/tickets/{id}/attachments` (ticket:write:all, any ticket), a
 *  customer's to the portal route (ticket:write:self, their own). Posting a staff upload to the
 *  portal route is the defect this fixes: the portal handler resolves the caller's *customer*, a
 *  staff login has none, and the upload answered 404 — a working endpoint with no working
 *  button. */
export async function uploadTicketAttachment(
  ctx: Ctx,
  ticketID: string,
  file: File,
  messageID?: string,
): Promise<TicketAttachment> {
  const api = ctx.api as unknown as ExtendableApi;
  if (typeof api.postRaw !== "function") {
    throw new ApiError(0, {
      title: "Not available in this build",
      detail: "The API client cannot send a raw body, so this file cannot be uploaded. Nothing was stored.",
      code: "client_unsupported",
    });
  }
  const path = `${ticketsBase(ctx)}/${encodeURIComponent(ticketID)}/attachments`;
  return api.postRaw<TicketAttachment>(path, file, {
    query: { filename: file.name, message_id: messageID },
    contentType: file.type || "application/octet-stream",
  });
}

/** The custom-field endpoints for the caller. */
export function ticketFieldsPath(ctx: Ctx, id: string): string {
  return `${ticketsBase(ctx)}/${encodeURIComponent(id)}/fields`;
}

/** Answers one or more custom fields on a ticket.
 *
 *  `values` is keyed by field key. An omitted key is left alone and `null` clears an answer, which
 *  is the server's rule and the reason this is a merge rather than a replace: a screen that shows
 *  one field must not blank the other eight. */
export async function saveTicketFields(
  ctx: Ctx,
  ticketID: string,
  values: Record<string, string | boolean | null>,
): Promise<TicketFieldValue[]> {
  const body = await ctx.api.put<{ fields?: unknown }>(ticketFieldsPath(ctx, ticketID), { values });
  return Array.isArray(body.fields) ? (body.fields as TicketFieldValue[]) : [];
}

// ------------------------------------------------------- custom field controls --

/** The controls for a set of custom fields, plus the two questions a caller needs answered about
 *  them: what the answers are, and which required ones are still blank.
 *
 *  One implementation for three screens — the customer's new-ticket form, the staff one, and the
 *  thread's field editor — because three copies of "how is a checkbox required" would be three
 *  answers that disagree. The rule the server applies is the rule here: a required text or select
 *  must be non-empty, and a *required checkbox must be ticked* (an unticked required checkbox is
 *  sent as `null`, not `false`, so the server refuses it rather than recording a "no" nobody
 *  asked for). An optional checkbox sends its real value, because `false` is an answer. */
export interface CustomFieldControls {
  el: HTMLElement;
  /** The answers keyed by field key, ready for the API. */
  values(): Record<string, string | boolean | null>;
  /** The labels of the required fields that are unanswered, in display order. */
  missingRequired(): string[];
  /** Moves focus to the first control whose label is in `labels`, and returns whether it found one. */
  focusFirst(labels: string[]): boolean;
}

export function customFieldControls(
  fields: TicketField[] | TicketFieldValue[] | undefined,
): CustomFieldControls | null {
  const list = Array.isArray(fields) ? fields : [];
  if (list.length === 0) return null;

  const controls: Array<{
    def: TicketField | TicketFieldValue;
    el: HTMLInputElement | HTMLSelectElement;
    checkbox: boolean;
  }> = [];

  const rows = list.map((def) => {
    const id = `ticket-field-${def.key}`;
    const current = "value" in def ? def.value : null;
    const required = def.required_on_create || def.required_to_close;

    if (def.type === "checkbox") {
      const box = input({ type: "checkbox", id, name: def.key, checked: current === true }) as HTMLInputElement;
      controls.push({ def, el: box, checkbox: true });
      return h(
        "div",
        { class: "field" },
        h("label", { class: "checkbox", for: id }, box, h("span", null, def.label)),
        requirementHint(def) ? h("span", { class: "field-hint" }, requirementHint(def)) : null,
      );
    }

    if (def.type === "select") {
      const options = Array.isArray(def.options) ? def.options.filter((value) => typeof value === "string") : [];
      const chosen = select(
        [
          { value: "", label: options.length > 0 ? "Not answered" : "No options configured" },
          ...options.map((value) => ({ value, label: value })),
        ],
        { id, name: def.key, value: typeof current === "string" ? current : "" },
      );
      if (options.length === 0) chosen.disabled = true;
      controls.push({ def, el: chosen, checkbox: false });
      return field({ label: def.label, control: chosen, id, required, hint: requirementHint(def) });
    }

    const text = input({
      id,
      name: def.key,
      value: typeof current === "string" ? current : "",
      maxlength: 2000,
      autocomplete: "off",
    });
    controls.push({ def, el: text, checkbox: false });
    return field({ label: def.label, control: text, id, required, hint: requirementHint(def) });
  });

  const el = h("div", { class: "stack-sm ticket-fields-form" }, ...rows);

  return {
    el,
    values() {
      const out: Record<string, string | boolean | null> = {};
      for (const control of controls) {
        const required = control.def.required_on_create || control.def.required_to_close;
        if (control.checkbox) {
          const checked = (control.el as HTMLInputElement).checked;
          // An unticked required checkbox is "unanswered", not "no".
          out[control.def.key] = checked ? true : required ? null : false;
          continue;
        }
        const raw = control.el.value.trim();
        out[control.def.key] = raw === "" ? null : raw;
      }
      return out;
    },
    missingRequired() {
      const missing: string[] = [];
      for (const control of controls) {
        const required = control.def.required_on_create || control.def.required_to_close;
        if (!required) continue;
        if (control.checkbox) {
          if (!(control.el as HTMLInputElement).checked) missing.push(control.def.label);
          continue;
        }
        if (control.el.value.trim() === "") missing.push(control.def.label);
      }
      return missing;
    },
    focusFirst(labels: string[]) {
      for (const control of controls) {
        if (!labels.includes(control.def.label)) continue;
        control.el.focus();
        return true;
      }
      return false;
    },
  };
}

/** The sentence under a field, naming which requirement it carries. */
function requirementHint(field: TicketField): string | undefined {
  if (field.required_on_create && field.required_to_close) {
    return "Required to open a ticket, and again before it can be closed.";
  }
  if (field.required_on_create) return "Required to open a ticket.";
  if (field.required_to_close) return "Required before the ticket can be closed.";
  return undefined;
}

/** Renders one answered field as a read-only row: the label and the answer, or a marker that it is
 *  still blank. Used by the thread, where the value is the record rather than a form. */
export function fieldAnswerRow(answer: TicketFieldValue): HTMLElement {
  const value =
    answer.value === true
      ? "Yes"
      : answer.value === false
        ? "No"
        : typeof answer.value === "string" && answer.value !== ""
          ? answer.value
          : null;
  return h(
    "div",
    { class: `ticket-field-row${value === null ? " ticket-field-row--empty" : ""}` },
    h("span", { class: "ticket-field-label" }, answer.label),
    h("span", { class: "ticket-field-value" }, value ?? "Not answered"),
    answer.required_to_close && value === null
      ? badge("required to close", "warning")
      : null,
  );
}
