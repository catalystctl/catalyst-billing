// The support queue: every ticket, filtered.
//
// This is the staff screen, and it is the one place in the panel where a list is not scoped to
// anybody: a queue that showed one customer's tickets would not be a queue. The boundary is the
// permission (`ticket:read:all`) and the route (`/api/v1/staff/tickets` is `StaffOnly`), not a
// filter this page applies.
//
// Two consequences shape the screen:
//
//   - **A refusal is a state, not a crash.** A customer who reaches `/support` — by typing the
//     URL, or because a role was revoked while the page was open — gets 403 from the handler.
//     The screen answers "this screen is for the support team" and offers the way back to the
//     portal, because a red error block with a Retry button would be a lie about a permanent
//     answer.
//   - **The empty state names the filter.** "No tickets" and "no tickets matching these filters"
//     are different facts, and confusing them sends an agent hunting for a bug in the queue.
//
// The filters are the four the handler accepts — status, priority, department, assignee — plus
// the two-character search over the subject and the customer. Each one is a query parameter, so a
// filtered queue is a URL an agent can paste to a colleague.

import { ApiError } from "../lib/api";
import { h } from "../lib/dom";
import { dateTime, relative, titleCase } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { btn, card, emptyState, field, input, pageHead, searchInput, select } from "../ui/kit";
import type { Column } from "../ui/table";
import {
  humanTicketStatus,
  normaliseTickets,
  NotAvailable,
  ticketHref,
  ticketPriorityBadge,
  ticketStatusBadge,
  ticketTable,
  type TicketMeta,
  type TicketRow,
} from "./tickets";

export const meta = { title: "Support queue", crumb: "Support" };

/** The search needs two characters: the handler refuses one with `invalid_input` rather than
 *  scanning the table for a single letter. */
const MIN_SEARCH = 2;

interface Filters {
  status: string;
  priority: string;
  department: string;
  assignee: string;
  q: string;
}

const PRIORITIES = ["low", "normal", "high"];
const STATUSES = ["open", "replied", "closed"];

export default async function supportQueuePage(ctx: Ctx): Promise<Node> {
  // The navigation already hides this link from a customer and the router bounces them, so this
  // is the belt to that braces: a page that rendered a queue would be claiming an access it does
  // not have.
  if (!ctx.can("ticket:read:all")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Support queue" }),
      card({
        flush: true,
        body: emptyState({
          title: "This screen is for the support team",
          body: "Reading the whole queue needs the ticket:read:all permission. Your own tickets are in your account, under Tickets.",
          icon: "lock",
          action: h(
            "div",
            { class: "row-wrap" },
            btn("Your tickets", { href: "/tickets", size: "sm", icon: "message" }),
            btn("Back to overview", { href: "/", size: "sm", icon: "home" }),
          ),
        }),
      }),
    );
  }

  let filters = filtersFrom(ctx.route.query);
  let meta: TicketMeta | null = null;

  // The meta request supplies the configured departments. It is optional: without it the
  // department filter is a free-text field rather than a select, and the table still works.
  try {
    meta = await ctx.api.get<TicketMeta>("/api/v1/staff/tickets/meta", { cache: false });
  } catch {
    meta = null;
  }

  const queryOf = (f: Filters) => ({
    status: f.status || undefined,
    priority: f.priority || undefined,
    department: f.department || undefined,
    assignee: f.assignee || undefined,
    q: f.q.length >= MIN_SEARCH ? f.q : undefined,
  });

  let table: ReturnType<typeof ticketTable<TicketRow>>;

  const apply = (next: Filters) => {
    filters = next;
    ctx.replaceQuery({
      status: next.status || null,
      priority: next.priority || null,
      department: next.department || null,
      assignee: next.assignee || null,
      q: next.q || null,
    });
    syncControls();
    table.reload();
  };

  // ------------------------------------------------------------- controls --
  const search = searchInput({
    value: filters.q,
    placeholder: "Search subject or customer",
    oninput: (value: string) => {
      // The search is debounced by the caller (250ms) rather than per keystroke: one request per
      // letter is a queue that never settles while someone types.
      filters = { ...filters, q: value.trim() };
      if (timer !== undefined) window.clearTimeout(timer);
      timer = window.setTimeout(() => apply(filters), 250);
    },
  });
  search.classList.add("support-search");

  let timer: number | undefined;
  ctx.onDispose(() => {
    if (timer !== undefined) window.clearTimeout(timer);
  });

  const statusSelect = select(
    [{ value: "", label: "Any status" }, ...STATUSES.map((value) => ({ value, label: humanTicketStatus(value) }))],
    {
      id: "support-status",
      value: filters.status,
      onchange: (event: Event) => apply({ ...filters, status: (event.target as HTMLSelectElement).value }),
    },
  );

  const prioritySelect = select(
    [{ value: "", label: "Any priority" }, ...PRIORITIES.map((value) => ({ value, label: titleCase(value) }))],
    {
      id: "support-priority",
      value: filters.priority,
      onchange: (event: Event) => apply({ ...filters, priority: (event.target as HTMLSelectElement).value }),
    },
  );

  const departments = Array.isArray(meta?.departments) ? meta.departments.filter(Boolean) : [];
  const departmentSelect = select(
    [
      { value: "", label: departments.length > 0 ? "Any department" : "Any department" },
      ...departments.map((value) => ({ value, label: titleCase(value) })),
      // A URL can carry a department the current configuration no longer offers. Keeping it
      // selectable means the filter the table is actually using is visible rather than silently
      // showing "Any department" over filtered rows.
      ...(filters.department && !departments.includes(filters.department)
        ? [{ value: filters.department, label: titleCase(filters.department) }]
        : []),
    ],
    {
      id: "support-department",
      value: filters.department,
      onchange: (event: Event) => apply({ ...filters, department: (event.target as HTMLSelectElement).value }),
    },
  );

  const assigneeSelect = select(
    [
      { value: "", label: "Anyone" },
      { value: "me", label: "Assigned to me" },
      { value: "unassigned", label: "Unassigned" },
    ],
    {
      id: "support-assignee",
      value: ["", "me", "unassigned"].includes(filters.assignee) ? filters.assignee : "",
      onchange: (event: Event) => apply({ ...filters, assignee: (event.target as HTMLSelectElement).value }),
    },
  );

  // A specific agent's tickets are a uuid the select cannot offer (no staff-list endpoint exists),
  // so it is a separate field that wins when it has a value.
  const assigneeID = input({
    id: "support-assignee-id",
    placeholder: "Agent id (uuid)",
    value: ["", "me", "unassigned"].includes(filters.assignee) ? "" : filters.assignee,
    onchange: (event: Event) => {
      const value = (event.target as HTMLInputElement).value.trim();
      apply({ ...filters, assignee: value });
    },
  });

  const clear = btn("Clear filters", { size: "sm", variant: "ghost", icon: "x" }) as HTMLButtonElement;
  // `.btn` sets `display`, which defeats the `hidden` attribute; view-support.css restores it for
  // this class. Setting the attribute here is still the intent — the rule is what makes it show.
  clear.classList.add("support-clear");
  clear.addEventListener("click", () => {
    (search.querySelector("input") as HTMLInputElement | null)?.value &&
      ((search.querySelector("input") as HTMLInputElement).value = "");
    apply({ status: "", priority: "", department: "", assignee: "", q: "" });
  });

  function syncControls(): void {
    statusSelect.value = filters.status;
    prioritySelect.value = filters.priority;
    departmentSelect.value = filters.department;
    assigneeSelect.value = ["", "me", "unassigned"].includes(filters.assignee) ? filters.assignee : "";
    assigneeID.value = ["", "me", "unassigned"].includes(filters.assignee) ? "" : filters.assignee;
    clear.hidden = !hasFilters(filters);
  }

  // ---------------------------------------------------------------- table --
  const columns: Column<TicketRow>[] = [
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
            row.last_message?.body
              ? h("span", { class: "support-preview", title: row.last_message.body }, row.last_message.body)
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
      render: (row) =>
        row.assigned_to
          ? h("span", { class: "nowrap" }, row.assignee_name || "Assigned")
          : h("span", { class: "faint nowrap" }, "Unassigned"),
    },
    {
      key: "messages",
      label: "Msgs",
      align: "right",
      hideSmall: true,
      render: (row) => h("span", { class: row.message_count > 1 ? "mono" : "mono faint" }, String(row.message_count)),
    },
    {
      key: "updated",
      label: "Updated",
      align: "right",
      hideSmall: true,
      render: (row) => h("span", { class: "dim nowrap", title: dateTime(row.updated_at) }, relative(row.updated_at)),
    },
  ];

  table = ticketTable<TicketRow>(ctx, {
    path: "/api/v1/staff/tickets",
    query: () => queryOf(filters),
    columns,
    rowHref: (row) => ticketHref(row.id),
    limit: 25,
    load: normaliseTickets,
    // A 403 here is "your role cannot read the queue", which is an answer, not a failure. The
    // list's own error path would otherwise offer a retry that can never succeed.
    mapError: (err) => {
      if (err instanceof ApiError && (err.status === 403 || err.code === "forbidden")) {
        return new NotAvailable(
          "This screen is for the support team",
          "Reading the whole queue needs the ticket:read:all permission. Your own tickets are in your account, under Tickets.",
        );
      }
      return err;
    },
    empty: () => ({
      title: hasFilters(filters) ? "No ticket matches these filters" : "The queue is empty",
      body: hasFilters(filters)
        ? "Nothing in the queue has this status, priority, department and assignee. Widen the filters to see more."
        : "Every ticket has been answered and closed. A new one appears here the moment a customer opens it.",
      icon: "message",
      action: hasFilters(filters)
        ? btn("Clear filters", {
            size: "sm",
            icon: "x",
            onclick: () => apply({ status: "", priority: "", department: "", assignee: "", q: "" }),
          })
        : btn("Back to overview", { href: "/", size: "sm", icon: "home" }),
    }),
  });

  syncControls();

  const filtersCard = card({
    body: h(
      "div",
      { class: "stack-sm" },
      h(
        "div",
        { class: "support-filters" },
        h("div", { class: "support-filters-search" }, search),
        field({ label: "Status", control: statusSelect }),
        field({ label: "Priority", control: prioritySelect }),
        field({ label: "Department", control: departmentSelect }),
        field({ label: "Assignee", control: assigneeSelect }),
        field({ label: "Agent id", control: assigneeID, hint: "Overrides the assignee choice when set." }),
        h("div", { class: "support-filters-end" }, clear),
      ),
      h(
        "p",
        { class: "field-hint" },
        icon("info", 12),
        ` Search needs at least ${MIN_SEARCH} characters and matches the subject and the customer's name or address.`,
      ),
    ),
  });

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Support queue",
      sub: "Every ticket on the platform, newest first. Opening one shows the thread and the controls to move it.",
      actions: h(
        "div",
        { class: "row" },
        btn("Refresh", { icon: "refresh", size: "sm", onclick: () => table.reload() }),
      ),
    }),
    h("div", { class: "stack" }, filtersCard, table.el),
  );
}

// ------------------------------------------------------------------ helpers --

function filtersFrom(query: URLSearchParams): Filters {
  return {
    status: (query.get("status") ?? "").trim(),
    priority: (query.get("priority") ?? "").trim(),
    department: (query.get("department") ?? "").trim(),
    assignee: (query.get("assignee") ?? "").trim(),
    q: (query.get("q") ?? "").trim(),
  };
}

function hasFilters(filters: Filters): boolean {
  return Boolean(filters.status || filters.priority || filters.department || filters.assignee || filters.q);
}
