// The operation queue.
//
// Every long action the panel starts — provision, inspect, suspend, resume, terminate —
// becomes a `service_operations` row and returns 202 immediately. This screen is that
// table, ordered newest first: what is waiting, what the worker is doing right now, and
// what has given up. The one thing it must never do is make a drained queue look broken.

import type { Page } from "../lib/api";
import { h } from "../lib/dom";
import { dateTime, humanStatus, relative, shortId, truncate } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { btn, card, emptyState, field, input, mono, pageHead, select, statusBadge } from "../ui/kit";
import { createTable, type Column } from "../ui/table";

export const meta = { title: "Queue", crumb: "Queue" };

/** The worker's lease, as `provisioning.DefaultLeaseDuration` defines it. The API also
 *  sends `lease_expires_at`; this is the fallback for a row that predates the field, and
 *  what the page documents when an operator asks how long "reaped in" means. */
const LEASE_MINUTES = 5;

/** The statuses the queue can hold, in lifecycle order. */
const STATUSES = ["queued", "running", "succeeded", "retry_wait", "unknown", "failed", "needs_review"] as const;

/** The kinds `service_operations.kind` allows. */
const KINDS = ["create", "inspect", "suspend", "resume", "terminate", "resize"] as const;

interface OperationRow {
  id: string;
  service: { id: string; customer: { id: string; email: string } };
  kind: string;
  operation_key: string;
  generation: number;
  status: string;
  attempts: number;
  last_error: string | null;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
  lease_expires_at?: string | null;
}

interface Filters {
  status: string;
  kind: string;
  service_id: string;
}

export default async function operationsPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("service:read:all")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Operation queue" }),
      card({
        body: emptyState({
          title: "Not available for your role",
          body: "Working the queue needs the service:read:all permission.",
          icon: "lock",
        }),
      }),
    );
  }

  let filters = filtersFrom(ctx.route.query);
  let counts: Record<string, number> = {};
  const controller = new AbortController();
  ctx.onDispose(() => controller.abort());

  const chipsHost = h("div", { class: "queue-chips" });
  const tableHost = h("div");

  // The empty state has to say two different things: "nothing is waiting, which is good"
  // and "your filters match nothing, which is not the same thing". The table reads this
  // object when it renders, so mutating it before a reload is enough.
  const empty: { title: string; body?: string; action?: Node } = {
    title: "",
    body: "",
  };

  const columns: Column<OperationRow>[] = [
    {
      key: "operation",
      label: "Operation",
      primary: true,
      render: (row) =>
        h(
          "div",
          { class: "cell-main" },
          h(
            "div",
            { class: "primary" },
            h("a", { class: "row-title", href: `/operations/${row.id}` }, humanStatus(row.kind)),
            row.status === "unknown" || row.status === "needs_review"
              ? h("span", { class: "queue-flag", title: "A human has to decide what happened" }, icon("alertTriangle", 11))
              : null,
          ),
          h(
            "div",
            { class: "secondary" },
            icon("server", 10),
            h("span", { class: "mono" }, shortId(row.service.id)),
            h("span", { class: "dot-sep" }, "·"),
            h("span", null, `generation ${row.generation}`),
          ),
        ),
    },
    {
      key: "customer",
      label: "Customer",
      hideSmall: true,
      render: (row) =>
        h(
          "a",
          { class: "linkish", href: `/customers/${row.service.customer.id}` },
          row.service.customer.email || "—",
        ),
    },
    { key: "status", label: "Status", render: (row) => statusBadge(row.status) },
    {
      key: "generation",
      label: "Gen",
      align: "right",
      render: (row) => mono(row.generation),
    },
    {
      key: "attempts",
      label: "Attempts",
      align: "right",
      render: (row) =>
        row.attempts > 1 ? h("span", { class: "queue-attempts" }, mono(row.attempts)) : mono(row.attempts),
    },
    {
      key: "last_error",
      label: "Last error",
      hideSmall: true,
      render: (row) =>
        row.last_error
          ? h("span", { class: "error-text", title: row.last_error }, truncate(row.last_error, 80))
          : h("span", { class: "faint" }, "—"),
    },
    {
      key: "created",
      label: "Created",
      align: "right",
      render: (row) => h("span", { class: "dim nowrap", title: dateTime(row.created_at) }, relative(row.created_at)),
    },
    { key: "lease", label: "Lease", align: "right", render: (row) => leaseCell(row) },
  ];

  const table = createTable<OperationRow>(ctx, {
    path: "/api/v1/staff/operations",
    columns,
    query: queryOf(filters),
    limit: 50,
    empty,
    rowHref: (row) => `/operations/${row.id}`,
    rowKey: (row) => row.id,
    // Column widths live in view-queue.css; the class is what scopes them to this table.
    class: "queue-table",
  });
  tableHost.appendChild(table.el);

  const kindSelect = select(kindOptions(filters.kind), {
    id: "queue-kind",
    value: filters.kind,
    onchange: (event) => setFilters({ ...filters, kind: (event.target as HTMLSelectElement).value }),
  });
  const serviceInput = input({
    id: "queue-service",
    type: "search",
    value: filters.service_id,
    placeholder: "Service id…",
    onchange: (event) => setFilters({ ...filters, service_id: (event.target as HTMLInputElement).value.trim() }),
  });
  const clearButton = btn("Clear filters", {
    size: "sm",
    variant: "ghost",
    icon: "x",
    onclick: () => setFilters({ status: "", kind: "", service_id: "" }),
  });

  function setFilters(next: Filters): void {
    filters = next;
    kindSelect.value = next.kind;
    serviceInput.value = next.service_id;
    clearButton.hidden = !hasFilters(next);
    ctx.replaceQuery({
      status: next.status || null,
      kind: next.kind || null,
      service_id: next.service_id || null,
    });
    describeEmpty();
    renderChips();
    void table.reload(queryOf(next));
    void loadCounts();
  }

  /** Status counts are not in the list payload (a page has no totals), so they come from
   *  one extra request with the status filter removed. The count is over the newest 100
   *  matching operations — the queue's working set — and the page says so. */
  async function loadCounts(): Promise<void> {
    try {
      const page = await ctx.api.get<Page<OperationRow>>("/api/v1/staff/operations", {
        query: {
          kind: filters.kind || undefined,
          service_id: filters.service_id || undefined,
          limit: 100,
        },
        cache: false,
        signal: controller.signal,
      });
      const next: Record<string, number> = {};
      for (const row of page.data) next[row.status] = (next[row.status] ?? 0) + 1;
      counts = next;
      renderChips();
    } catch (err) {
      // The chips are a convenience; the table reports the real failure.
      if (controller.signal.aborted || (err instanceof DOMException && err.name === "AbortError")) return;
    }
  }

  function renderChips(): void {
    const total = STATUSES.reduce((sum, status) => sum + (counts[status] ?? 0), 0);
    chipsHost.replaceChildren(
      h("span", { class: "queue-chips-label" }, "Status"),
      chip("", "All", total),
      ...STATUSES.map((status) => chip(status, humanStatus(status), counts[status] ?? 0)),
      h(
        "span",
        { class: "queue-note" },
        `Counts cover the newest 100 operations${filters.kind || filters.service_id ? " with these filters" : ""}.`,
      ),
    );
  }

  function chip(value: string, label: string, count: number): HTMLElement {
    const active = filters.status === value;
    return h(
      "button",
      {
        class: `chip${active ? " is-active" : ""}`,
        type: "button",
        "aria-pressed": active ? "true" : "false",
        onclick: () => setFilters({ ...filters, status: value }),
      },
      label,
      h("span", { class: "chip-count" }, String(count)),
    );
  }

  function describeEmpty(): void {
    if (hasFilters(filters)) {
      empty.title = "Nothing matches these filters";
      empty.body = "No operation in the queue has this status, kind and service. Widen the filters to see more.";
      empty.action = btn("Clear filters", {
        size: "sm",
        icon: "x",
        onclick: () => setFilters({ status: "", kind: "", service_id: "" }),
      });
      return;
    }
    empty.title = "The queue is drained";
    empty.body =
      "A drained queue is a good thing: every operation has finished and nothing is waiting on the worker. New actions appear here the moment they are enqueued.";
    empty.action = undefined;
  }

  describeEmpty();
  renderChips();
  clearButton.hidden = !hasFilters(filters);

  const page = h(
    "div",
    { class: "page" },
    pageHead({
      title: "Operation queue",
      sub: `Every long action the panel starts, newest first. A worker holds a lease of ${LEASE_MINUTES} minutes while an operation runs; if it dies, the reaper returns the operation to the queue under the same key.`,
      actions: btn("Refresh", {
        icon: "refresh",
        size: "sm",
        onclick: () => {
          void table.reload(queryOf(filters));
          void loadCounts();
        },
      }),
    }),
    card({
      body: h(
        "div",
        { class: "stack-sm" },
        chipsHost,
        h(
          "div",
          { class: "queue-filters" },
          field({ label: "Kind", control: kindSelect }),
          field({ label: "Service", control: serviceInput }),
          h("div", { class: "queue-filters-end" }, clearButton),
        ),
      ),
    }),
    tableHost,
  );

  void loadCounts();
  return page;
}

// ------------------------------------------------------------------ helpers --

function filtersFrom(query: URLSearchParams): Filters {
  return {
    status: query.get("status") ?? "",
    kind: query.get("kind") ?? "",
    service_id: query.get("service_id") ?? "",
  };
}

function queryOf(filters: Filters): Record<string, string | undefined> {
  return {
    status: filters.status || undefined,
    kind: filters.kind || undefined,
    service_id: filters.service_id || undefined,
  };
}

function hasFilters(filters: Filters): boolean {
  return Boolean(filters.status || filters.kind || filters.service_id);
}

function kindOptions(current: string): Array<{ value: string; label: string }> {
  const options = [{ value: "", label: "Any kind" }];
  for (const kind of KINDS) options.push({ value: kind, label: humanStatus(kind) });
  // A link can carry a kind this build does not know; keep it selectable rather than
  // silently showing "Any kind" while the table is filtered.
  if (current && !KINDS.some((kind) => kind === current)) {
    options.push({ value: current, label: humanStatus(current) });
  }
  return options;
}

/** When the worker's lease runs out. The server computes this too; the fallback exists so
 *  the column still answers when `lease_expires_at` is absent. */
function leaseExpiry(row: OperationRow): Date | null {
  if (row.lease_expires_at) {
    const at = new Date(row.lease_expires_at);
    if (!Number.isNaN(at.getTime())) return at;
  }
  if (!row.started_at) return null;
  const started = new Date(row.started_at);
  if (Number.isNaN(started.getTime())) return null;
  return new Date(started.getTime() + LEASE_MINUTES * 60_000);
}

/** "reaped in 3m" while running, "—" otherwise: a finished operation holds no lease, and
 *  saying so is more useful than a stale number. */
function leaseCell(row: OperationRow): Node {
  if (row.status !== "running") return h("span", { class: "faint" }, "—");
  const expiry = leaseExpiry(row);
  if (!expiry) return h("span", { class: "faint" }, "—");
  const minutes = Math.ceil((expiry.getTime() - Date.now()) / 60_000);
  if (minutes <= 0) {
    return h("span", { class: "lease lease--soon" }, icon("alertTriangle", 11), "reap due");
  }
  return h(
    "span",
    { class: `lease${minutes <= 1 ? " lease--soon" : ""}`, title: `Lease expires ${dateTime(expiry.toISOString())}` },
    icon("clock", 11),
    `reaped in ${minutes}m`,
  );
}
