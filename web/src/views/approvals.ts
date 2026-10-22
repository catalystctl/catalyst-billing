// Approvals: the second signature.
//
// A high-value action — a refund above the threshold, a termination batch, an override of a
// hold — is requested on another screen and decided here, by a different person. Two
// properties of the server shape this page:
//
//  - The permission to *decide* depends on the action, not on the route. `audit:read` gets
//    you the list; approving a refund additionally needs `payment:refund`, a termination
//    `service:terminate`, a hold override `service:suspend`. The banner names that mapping
//    and says which of them you hold, so an operator can see why a decision will be refused
//    before making it.
//  - The requester cannot decide their own request. The database enforces it and the handler
//    answers `self_approval_refused`. The buttons therefore stay visible on your own
//    requests: a hidden button teaches nobody, while the server's refusal names the policy
//    and leaves a `denied` row in the audit log. This page surfaces that answer, with its
//    code, instead of hiding the control.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateTime, relative, shortId, titleCase, truncate } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  badge,
  banner,
  btn,
  card,
  emptyState,
  field,
  input,
  mono,
  pageHead,
  select,
  statusBadge,
  textarea,
} from "../ui/kit";
import { createTable, type Column } from "../ui/table";

export const meta = { title: "Approvals", crumb: "Approvals" };

/** The wire shape of `approvalMap` in internal/httpapi/approvals.go. `approved_by` is only
 *  present once a decision exists, and `decided_at` is null until then. */
interface ApprovalRow {
  id: string;
  action: string;
  reference_id: string;
  requested_by: string;
  approved_by?: string;
  status: string;
  reason: string;
  created_at: string;
  decided_at: string | null;
}

/** The actions the CHECK on `approvals.action` allows. */
const ACTIONS = ["refund", "terminate_batch", "hold_override"] as const;

/** The statuses the CHECK on `approvals.status` allows. */
const STATUSES = ["pending", "approved", "rejected", "expired"] as const;

/** Mirrors `actionPermission` in internal/httpapi/approvals.go. The server resolves the
 *  permission from the record; this map is only used to explain the policy and to say
 *  whether the signed-in operator holds it. */
const ACTION_PERMISSION: Record<string, string> = {
  refund: "payment:refund",
  terminate_batch: "service:terminate",
  hold_override: "service:suspend",
};

/** Where a reference points. A refund's reference is the payment it would refund; a
 *  termination batch and a hold override are decided against the services screen. */
const ACTION_REFERENCE_HREF: Record<string, (referenceId: string) => string | null> = {
  refund: (id) => (id ? `/payments/${id}` : null),
  terminate_batch: () => "/services",
  hold_override: () => "/services",
};

interface Filters {
  status: string;
  action: string;
  actor_id: string;
}

export default async function approvalsPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("audit:read")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Approvals" }),
      card({
        body: emptyState({
          title: "Not available for your role",
          body: "The approval queue needs the audit:read permission. Deciding a request additionally needs the permission its action requires.",
          icon: "lock",
        }),
      }),
    );
  }

  let filters = filtersFrom(ctx.route.query);

  // The last decision's failure, if any. It lives above the table rather than in a toast:
  // a refusal is a fact about the policy, and it should still be on screen when the dialog
  // that triggered it is gone.
  const decisionHost = h("div", { hidden: true });
  const tableHost = h("div");

  // The table reads this object when it renders, so mutating it before a reload is enough.
  const empty: { title: string; body?: string; action?: Node } = { title: "", body: "" };

  const columns: Column<ApprovalRow>[] = [
    {
      key: "action",
      label: "Action",
      primary: true,
      render: (row) =>
        h(
          "div",
          { class: "cell-main" },
          h("div", { class: "primary" }, actionLabel(row.action)),
          h("div", { class: "secondary" }, mono(row.action)),
        ),
    },
    {
      key: "reference",
      label: "Reference",
      render: (row) => referenceCell(row),
    },
    {
      key: "requested_by",
      label: "Requested by",
      render: (row) => h("span", { class: "mono", title: row.requested_by }, shortId(row.requested_by)),
    },
    {
      key: "status",
      label: "Status",
      render: (row) => statusBadge(row.status),
    },
    {
      key: "reason",
      label: "Reason",
      hideSmall: true,
      render: (row) =>
        row.reason
          ? h("span", { class: "trust-truncate", title: row.reason }, truncate(row.reason, 90))
          : h("span", { class: "faint" }, "—"),
    },
    {
      key: "created",
      label: "Requested",
      align: "right",
      render: (row) => h("span", { class: "dim nowrap", title: dateTime(row.created_at) }, relative(row.created_at)),
    },
    {
      key: "decided",
      label: "Decided",
      align: "right",
      render: (row) =>
        row.decided_at
          ? h(
              "div",
              { class: "cell-main" },
              h("div", { class: "primary nowrap", title: dateTime(row.decided_at) }, relative(row.decided_at)),
              row.approved_by ? h("div", { class: "secondary mono", title: row.approved_by }, shortId(row.approved_by)) : null,
            )
          : h("span", { class: "faint nowrap" }, "—"),
    },
    {
      key: "actions",
      label: "",
      align: "right",
      width: "1%",
      render: (row) =>
        row.status === "pending"
          ? h(
              "div",
              { class: "row" },
              btn("Approve", {
                size: "sm",
                variant: "primary",
                icon: "check",
                title: "Approve this request",
                onclick: () => void openApprove(row),
              }),
              btn("Reject", {
                size: "sm",
                variant: "danger",
                icon: "x",
                title: "Reject this request",
                onclick: () => openReject(row),
              }),
            )
          : h("span", { class: "faint" }, "—"),
    },
  ];

  const table = createTable<ApprovalRow>(ctx, {
    path: "/api/v1/staff/approvals",
    columns,
    query: queryOf(filters),
    limit: 50,
    empty,
    rowKey: (row) => row.id,
  });
  tableHost.appendChild(table.el);

  const statusSelect = select(statusOptions(filters.status), {
    id: "approvals-status",
    value: filters.status,
    onchange: (event) => setFilters({ ...filters, status: (event.target as HTMLSelectElement).value }),
  });
  const actionSelect = select(actionOptions(filters.action), {
    id: "approvals-action",
    value: filters.action,
    onchange: (event) => setFilters({ ...filters, action: (event.target as HTMLSelectElement).value }),
  });

  const actorHint = h("span", { class: "field-hint" }, "A user id. Matches the requester or the approver.");
  const actorError = h("span", { class: "field-error", hidden: true }, icon("alertCircle", 11), "A user id is a UUID.");
  const actorInput = input({
    id: "approvals-actor",
    type: "search",
    value: filters.actor_id,
    placeholder: "User id…",
    autocomplete: "off",
    onchange: (event) => {
      const value = (event.target as HTMLInputElement).value.trim();
      applyIdValidity(actorInput, actorHint, actorError, value);
      setFilters({ ...filters, actor_id: value });
    },
  });
  applyIdValidity(actorInput, actorHint, actorError, filters.actor_id);

  const clearButton = btn("Clear filters", {
    size: "sm",
    variant: "ghost",
    icon: "x",
    onclick: () => setFilters({ status: "", action: "", actor_id: "" }),
  });

  function setFilters(next: Filters): void {
    filters = next;
    statusSelect.value = next.status;
    actionSelect.value = next.action;
    actorInput.value = next.actor_id;
    clearButton.hidden = !hasFilters(next);
    ctx.replaceQuery({
      status: next.status || null,
      action: next.action || null,
      actor_id: next.actor_id || null,
    });
    describeEmpty();
    void table.reload(queryOf(next));
  }

  function describeEmpty(): void {
    if (hasFilters(filters)) {
      empty.title = "Nothing matches these filters";
      empty.body = "No approval has this status, action and actor. Widen the filters to see the rest of the queue.";
      empty.action = btn("Clear filters", {
        size: "sm",
        icon: "x",
        onclick: () => setFilters({ status: "", action: "", actor_id: "" }),
      });
      return;
    }
    empty.title = "No approvals requested";
    empty.body =
      "A refund above the threshold, a termination batch or a hold override creates a request here. Nothing is waiting for a second signature right now.";
    empty.action = undefined;
  }

  /** A refused decision is shown with its code, because the code is the stable fact: the
   *  detail is a copy edit away from saying something else. */
  function showDecisionError(err: unknown, message: string): void {
    const apiErr = err instanceof ApiError ? err : null;
    decisionHost.hidden = false;
    swap(
      decisionHost,
      banner(
        "danger",
        apiErr?.code === "self_approval_refused" ? "You cannot decide your own request" : "The decision was not recorded",
        h(
          "span",
          null,
          message,
          apiErr ? h("code", { class: "inline-code" }, `${apiErr.status} ${apiErr.code}`) : null,
        ),
        btn("Refresh", {
          size: "sm",
          variant: "ghost",
          icon: "refresh",
          onclick: () => {
            decisionHost.hidden = true;
            void table.reload(queryOf(filters));
          },
        }),
      ),
    );
  }

  /** Runs the decision. Returns an error message for the dialog that is still open, or
   *  undefined when the row was decided and the table refreshed. */
  async function decide(row: ApprovalRow, verb: "approve" | "reject", reason: string): Promise<string | undefined> {
    try {
      await ctx.api.post(`/api/v1/staff/approvals/${row.id}/${verb}`, { reason });
      decisionHost.hidden = true;
      ctx.toast(verb === "approve" ? "Approval recorded" : "Request rejected", {
        tone: "success",
        detail: `${actionLabel(row.action)} · ${shortId(row.reference_id)}`,
      });
      await table.reload(queryOf(filters));
      return undefined;
    } catch (err) {
      const message = describeDecisionError(err, row);
      showDecisionError(err, message);
      return message;
    }
  }

  async function openApprove(row: ApprovalRow): Promise<void> {
    const permission = ACTION_PERMISSION[row.action] ?? "the action's permission";
    const ok = await ctx.confirm({
      title: "Approve this request?",
      body: h(
        "span",
        null,
        "This records your approval of the ",
        mono(row.action),
        ` request against ${shortId(row.reference_id)}. Approving lets the action run. You must hold ${permission}, and you cannot approve a request you made yourself.`,
      ),
      confirmLabel: "Approve",
      note: "The decision is written to the audit log with your name and cannot be undone.",
    });
    if (!ok) return;
    await decide(row, "approve", "");
  }

  function openReject(row: ApprovalRow): void {
    const reason = textarea({
      id: "approvals-reject-reason",
      name: "reason",
      placeholder: "Why is this being rejected?",
    });
    void ctx.formDialog({
      title: "Reject this request?",
      subtitle: `${actionLabel(row.action)} · ${shortId(row.reference_id)} · requested by ${shortId(row.requested_by)}`,
      submitLabel: "Reject",
      danger: true,
      body: field({
        label: "Reason",
        control: reason,
        hint: "Appended to the request's reason and kept with the decision. The requester sees it.",
      }),
      onSubmit: async () => decide(row, "reject", reason.value.trim()),
    });
  }

  /** The policy, stated where it is applied. It does not disable a button: the server is
   *  the gate, and its refusal is what the operator sees. */
  function permissionBanner(pageCtx: Ctx): HTMLElement {
    return banner(
      "info",
      "A second signature, by a different person",
      h(
        "div",
        { class: "stack-sm" },
        h(
          "span",
          null,
          "The approver must be someone other than the requester, holding the permission the action needs. The server refuses a self-approval with self_approval_refused, so the button is never hidden — the refusal is recorded and explained.",
        ),
        h(
          "div",
          { class: "trust-perms" },
          ...ACTIONS.map((action) => {
            const permission = ACTION_PERMISSION[action] ?? "";
            return h(
              "div",
              { class: "trust-perm" },
              h("span", { class: "trust-perm-action" }, actionLabel(action)),
              h("span", { class: "mono" }, permission),
              pageCtx.can(permission) ? badge("You hold it", "success") : badge("Not held", "neutral"),
            );
          }),
        ),
      ),
    );
  }

  describeEmpty();
  clearButton.hidden = !hasFilters(filters);

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Approvals",
      sub: "Requests for a second signature on a high-value action. Deciding one needs the permission its action requires, and never the person who asked for it.",
      actions: btn("Refresh", {
        icon: "refresh",
        size: "sm",
        onclick: () => {
          decisionHost.hidden = true;
          void table.reload(queryOf(filters));
        },
      }),
    }),
    permissionBanner(ctx),
    decisionHost,
    card({
      body: h(
        "div",
        { class: "trust-filters" },
        field({ label: "Status", control: statusSelect }),
        field({ label: "Action", control: actionSelect }),
        h(
          "div",
          { class: "field" },
          h("label", { class: "field-label", for: "approvals-actor" }, "Actor"),
          actorInput,
          actorHint,
          actorError,
        ),
        h("div", { class: "trust-filters-end" }, clearButton),
      ),
    }),
    tableHost,
  );
}

// ------------------------------------------------------------------ helpers --

function filtersFrom(query: URLSearchParams): Filters {
  return {
    status: query.get("status") ?? "",
    action: query.get("action") ?? "",
    actor_id: query.get("actor_id") ?? "",
  };
}

function queryOf(filters: Filters): Record<string, string | undefined> {
  return {
    status: filters.status || undefined,
    action: filters.action || undefined,
    // `requested_by`/`approved_by` are uuid columns; sending a fragment would make the
    // server answer with a database error rather than an empty page.
    actor_id: uuidLike(filters.actor_id) ? filters.actor_id : undefined,
  };
}

function hasFilters(filters: Filters): boolean {
  return Boolean(filters.status || filters.action || filters.actor_id);
}

function actionLabel(action: string): string {
  return titleCase(action);
}

function referenceCell(row: ApprovalRow): Node {
  const href = ACTION_REFERENCE_HREF[row.action]?.(row.reference_id) ?? null;
  const id = row.reference_id ? shortId(row.reference_id) : "—";
  if (!href) return h("span", { class: "mono", title: row.reference_id }, id);
  return h(
    "a",
    {
      class: "linkish mono",
      href,
      title: `${actionLabel(row.action)} reference ${row.reference_id}`,
    },
    id,
  );
}

function statusOptions(current: string): Array<{ value: string; label: string }> {
  const options = [{ value: "", label: "Any status" }];
  for (const status of STATUSES) options.push({ value: status, label: titleCase(status) });
  // A URL can carry a status this build does not know; keep it selectable rather than
  // silently showing "Any status" while the table is filtered.
  if (current && !STATUSES.some((status) => status === current)) {
    options.push({ value: current, label: titleCase(current) });
  }
  return options;
}

function actionOptions(current: string): Array<{ value: string; label: string }> {
  const options = [{ value: "", label: "Any action" }];
  for (const action of ACTIONS) options.push({ value: action, label: actionLabel(action) });
  if (current && !ACTIONS.some((action) => action === current)) {
    options.push({ value: current, label: actionLabel(current) });
  }
  return options;
}

function uuidLike(value: string): boolean {
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value);
}

/** Marks the input invalid without blocking the rest of the filter bar: an empty box is
 *  "no filter", a malformed id is a typo the operator can see and fix. */
function applyIdValidity(
  inputEl: HTMLInputElement,
  hint: HTMLElement,
  error: HTMLElement,
  value: string,
): void {
  const invalid = value !== "" && !uuidLike(value);
  if (invalid) inputEl.setAttribute("aria-invalid", "true");
  else inputEl.removeAttribute("aria-invalid");
  hint.hidden = invalid;
  error.hidden = !invalid;
}

/** Explains a failed decision in terms of the policy, not in terms of HTTP. */
function describeDecisionError(err: unknown, row: ApprovalRow): string {
  if (!(err instanceof ApiError)) return "The server could not be reached, so nothing was decided.";
  const permission = ACTION_PERMISSION[row.action] ?? "the action's permission";
  switch (err.code) {
    case "self_approval_refused":
      return `You requested this approval, so you cannot decide it. A different staff member holding ${permission} has to approve or reject it.`;
    case "forbidden":
      return `You do not hold ${permission}, which deciding this action requires.`;
    case "already_decided":
      return "This approval has already been decided. Refresh to see who decided it and when.";
    case "unknown_action":
      return "This build cannot decide this approval's action.";
    case "approvals_unavailable":
      return "Approvals are not configured on this server, so nothing can be decided.";
    case "not_found":
      return "This approval no longer exists.";
    default:
      return err.detail || "The decision could not be recorded.";
  }
}
