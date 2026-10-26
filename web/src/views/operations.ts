// The operation queue.
//
// Every long action the panel starts — provision, inspect, suspend, resume, terminate —
// becomes a `service_operations` row and returns 202 immediately. This screen is that
// table, ordered newest first: what is waiting, what the worker is doing right now, and
// what has given up. The one thing it must never do is make a drained queue look broken.

import { ApiError, type Page } from "../lib/api";
import { h } from "../lib/dom";
import { dateTime, humanStatus, relative, shortId, truncate } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { banner, btn, card, emptyState, field, input, mono, pageHead, select, statusBadge, textarea } from "../ui/kit";
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

// ----------------------------------------------------------------- ADM-18 --
//
// Three controls turn a row the worker gave up on back into a decision: retry it, resolve it
// by hand, or delete the record. They live here rather than in each screen because the queue
// and the detail page must make exactly the same offer, and the server refuses the request
// for exactly the same reasons (internal/httpapi/operations.go).
//
// The permissions are the server's, not a second opinion:
//   retry   — re-runs the provisioning call, so it needs service:provision;
//   resolve — records that a human settled the remote state, so it needs service:write:all;
//   delete  — removes the row, same permission as resolve.
// A role that cannot complete an action is not shown it, because a button that always answers
// 403 is worse than no button.

/** The permission a retry requires. */
export const RETRY_PERMISSION = "service:provision";

/** The permission resolve and delete require. */
export const RESOLVE_PERMISSION = "service:write:all";

/** The statuses retry and resolve accept. A queued or running operation is already in the
 *  worker's hands, and a succeeded one has nothing left to do; the server refuses all three, so
 *  the screen does not offer them. */
const ACTIONABLE = ["failed", "unknown", "needs_review"] as const;

/** The minimum a row must carry for the controls to decide what to offer. Both the list row and
 *  the detail payload satisfy it, so one predicate serves both screens. */
export interface OperationSubject {
  id: string;
  kind: string;
  status: string;
  attempts?: number;
  generation?: number;
}

export interface OperationCapabilities {
  retry: boolean;
  resolve: boolean;
  delete: boolean;
}

/** What this session may do to this operation, decided by status and permission together.
 *
 *  `delete` is deliberately narrower than the other two: the server deletes only a `failed`
 *  row. An `unknown` or `needs_review` row still has an unresolved remote outcome, and deleting
 *  it would erase the question instead of answering it, so it must be resolved first. */
export function operationCapabilities(ctx: Ctx, op: OperationSubject): OperationCapabilities {
  const actionable = (ACTIONABLE as readonly string[]).includes(op.status);
  const mayDecide = ctx.can(RESOLVE_PERMISSION);
  return {
    retry: actionable && ctx.can(RETRY_PERMISSION),
    resolve: actionable && mayDecide,
    delete: op.status === "failed" && mayDecide,
  };
}

/** True when at least one control is on offer, so a caller can hide an empty card or column. */
export function hasCapabilities(caps: OperationCapabilities): boolean {
  return caps.retry || caps.resolve || caps.delete;
}

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

  const canRetry = ctx.can(RETRY_PERMISSION);
  const canDecide = ctx.can(RESOLVE_PERMISSION);

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

  // A session that can neither retry nor decide gets no action column at all: an empty column
  // of dashes would only say "you are not allowed", which the permission guard already said.
  if (canRetry || canDecide) {
    columns.push({
      key: "actions",
      label: "",
      align: "right",
      width: "1%",
      render: (row) => {
        const caps = operationCapabilities(ctx, row);
        if (!hasCapabilities(caps)) return h("span", { class: "faint" }, "—");
        return h(
          "div",
          { class: "row-wrap" },
          caps.retry
            ? btn("Retry", {
                size: "sm",
                variant: "ghost",
                icon: "rotate",
                title: `Requeue this ${row.kind} operation with a fresh attempt budget`,
                onclick: () => void runAction(row, "retry"),
              })
            : null,
          caps.resolve
            ? btn("Resolve", {
                size: "sm",
                variant: "ghost",
                icon: "checkCircle",
                title: `Record that you settled this ${row.kind} operation's remote state by hand`,
                onclick: () => void runAction(row, "resolve"),
              })
            : null,
          caps.delete
            ? btn("Delete", {
                size: "sm",
                variant: "ghost",
                icon: "trash",
                title: `Remove this failed ${row.kind} operation from the queue`,
                onclick: () => void runAction(row, "delete"),
              })
            : null,
        );
      },
    });
  }

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

  /** Opens the dialog for one control and, when the server accepted it, re-reads the page.
   *  The dialog owns the in-flight state and the refusal sentence; the list only has to reload. */
  const runAction = async (row: OperationRow, action: "retry" | "resolve" | "delete"): Promise<void> => {
    const changed =
      action === "retry"
        ? await retryOperation(ctx, row)
        : action === "resolve"
          ? await resolveOperation(ctx, row)
          : await deleteOperation(ctx, row);
    if (!changed) return;
    await table.reload(queryOf(filters));
    void loadCounts();
  };

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
      sub: `Every long action the panel starts, newest first. A worker holds a lease of ${LEASE_MINUTES} minutes while an operation runs; if it dies, the reaper returns the operation to the queue under the same key. A failed or needs-review row carries Retry and Resolve; only a failed row can be deleted.`,
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

// ---------------------------------------------------------------- actions --

/**
 * Opens the retry dialog.
 *
 * The reason is optional because the API makes it optional: a retry is an ordinary operation
 * that happens again, not a decision about what already happened. The audit row records the
 * previous status and attempt count either way, and the sentence, when given, says why the
 * operator believed the cause was fixed.
 *
 * Returns true only when the server accepted the retry (202), so the caller knows to re-read.
 */
export async function retryOperation(ctx: Ctx, op: OperationSubject): Promise<boolean> {
  const reason = textarea({
    id: `operation-retry-reason-${op.id}`,
    placeholder: "Provider was down; it is reachable again",
  });
  let accepted = false;
  await ctx.formDialog({
    title: `Retry this ${humanStatus(op.kind).toLowerCase()} operation?`,
    subtitle: "It goes back to the queue under the same operation key, with a fresh attempt budget.",
    submitLabel: "Retry operation",
    body: h(
      "div",
      { class: "stack" },
      field({
        label: "Reason (optional)",
        control: reason,
        hint: "Recorded with your name in the audit log. It is what the next operator reads.",
      }),
      banner(
        "warning",
        "A retry runs the provider call again",
        "If the service has changed since this operation was queued, the worker's fencing token refuses the retry. Review the service's current state instead of retrying blindly.",
      ),
    ),
    onSubmit: async () => {
      const value = reason.value.trim();
      try {
        await ctx.api.post(
          `/api/v1/staff/operations/${encodeURIComponent(op.id)}/retry`,
          value ? { reason: value } : undefined,
        );
        accepted = true;
        ctx.toast("Operation queued again", {
          tone: "success",
          detail: `${humanStatus(op.kind)} · attempts reset to 0`,
        });
        return undefined;
      } catch (err) {
        return describeOperationActionError(err);
      }
    },
  });
  return accepted;
}

/**
 * Opens the resolve dialog.
 *
 * The reason is required, and the dialog refuses to submit without one, because the API does:
 * `needs_review` means the panel does not know the remote state, and a resolution without a
 * sentence saying how it was established is indistinguishable from clearing an alert.
 */
export async function resolveOperation(ctx: Ctx, op: OperationSubject): Promise<boolean> {
  const reason = textarea({
    id: `operation-resolve-reason-${op.id}`,
    placeholder: "The server exists in the provider panel; billing was applied by hand",
  });
  let accepted = false;
  await ctx.formDialog({
    title: `Resolve this ${humanStatus(op.kind).toLowerCase()} operation?`,
    subtitle: "It is closed as succeeded and your sentence is kept on the row and in the audit log.",
    submitLabel: "Resolve operation",
    body: h(
      "div",
      { class: "stack" },
      field({
        label: "Reason",
        control: reason,
        required: true,
        hint: "Say how the remote state was established. Without it the next operator cannot tell a decision from a cleared alert.",
      }),
      banner(
        "warning",
        "Resolving runs nothing",
        "It records a human decision. Only resolve once you have checked the provider and the service actually reflect the intended state.",
      ),
    ),
    onSubmit: async () => {
      const value = reason.value.trim();
      if (!value) return "A reason is required: say how the remote state was established.";
      try {
        await ctx.api.post(`/api/v1/staff/operations/${encodeURIComponent(op.id)}/resolve`, { reason: value });
        accepted = true;
        ctx.toast("Operation resolved", {
          tone: "success",
          detail: `${humanStatus(op.kind)} · closed as succeeded by hand`,
        });
        return undefined;
      } catch (err) {
        return describeOperationActionError(err);
      }
    },
  });
  return accepted;
}

/**
 * Opens the delete dialog.
 *
 * Deletion is the one control that destroys the row, so it asks for the operation's short id to
 * be typed: the dialog is reachable from a list where one failed row looks like the next, and a
 * mis-click on the wrong one is not recoverable. The reason is optional — the API's is — but the
 * audit row records the deletion either way.
 */
export async function deleteOperation(ctx: Ctx, op: OperationSubject): Promise<boolean> {
  const confirm = input({
    id: `operation-delete-confirm-${op.id}`,
    placeholder: shortId(op.id),
    autocomplete: "off",
    class: "input--mono",
  });
  const reason = textarea({
    id: `operation-delete-reason-${op.id}`,
    placeholder: "Duplicate of a newer operation",
  });
  let accepted = false;
  await ctx.formDialog({
    title: `Delete this failed ${humanStatus(op.kind).toLowerCase()} operation?`,
    subtitle: "The row leaves the queue. The audit log keeps the record of the deletion.",
    submitLabel: "Delete operation",
    danger: true,
    body: h(
      "div",
      { class: "stack" },
      banner(
        "danger",
        "This cannot be undone",
        "The operation row is removed. Deleting it does not undo anything the worker already did to the service, and it does not retry it.",
      ),
      field({
        label: `Type “${shortId(op.id)}” to confirm`,
        control: confirm,
        required: true,
        hint: "The operation id is on the row and in this page's address.",
      }),
      field({
        label: "Reason (optional)",
        control: reason,
        hint: "Recorded with your name in the audit log.",
      }),
    ),
    onSubmit: async () => {
      if (confirm.value.trim() !== shortId(op.id)) {
        return `Type ${shortId(op.id)} exactly to confirm the deletion.`;
      }
      const value = reason.value.trim();
      try {
        await ctx.api.del(
          `/api/v1/staff/operations/${encodeURIComponent(op.id)}`,
          value ? { reason: value } : undefined,
        );
        accepted = true;
        ctx.toast("Operation deleted", {
          tone: "success",
          detail: `${humanStatus(op.kind)} · removed from the queue`,
        });
        return undefined;
      } catch (err) {
        return describeOperationActionError(err);
      }
    },
  });
  return accepted;
}

/**
 * Turns the API's refusal into a sentence the operator can act on.
 *
 * Each code is a different fix, so a single "the request failed" would be a worse answer than
 * the raw status: `operation_fenced` means review the service, `operation_outcome_unknown` means
 * resolve first, and `operation_already_queued` means wait.
 */
export function describeOperationActionError(err: unknown): string {
  if (err instanceof ApiError) {
    switch (err.code) {
      case "operation_already_queued":
        return "The worker already has this operation queued or running. Wait for it to finish rather than queueing it twice.";
      case "operation_already_succeeded":
        return "This operation already succeeded, so there is nothing left to retry or resolve.";
      case "operation_fenced":
        return "The service changed after this operation was queued, so the worker would refuse a re-run. Review the service's current state instead.";
      case "operation_outcome_unknown":
        return "The remote outcome is still unknown, so this operation cannot be deleted. Resolve it first.";
      case "operation_not_deletable":
        return "Only a failed operation can be deleted. Resolve this one instead.";
      case "invalid_reason":
        return err.detail || "A reason is required: say how the remote state was established.";
      case "not_found":
        return "That operation no longer exists. It may have been deleted or resolved in another tab.";
      case "forbidden":
        return "Your role is not allowed to do that to an operation.";
      default:
        return err.detail || `${err.status} ${err.code}`;
    }
  }
  return "The request failed. Try again.";
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
