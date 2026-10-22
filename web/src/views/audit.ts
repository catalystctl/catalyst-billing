// The audit log.
//
// Every state change the system makes is a row here: who did it, to which record, with what
// outcome, and — in `changes` — what actually moved. Two properties shape the screen:
//
//  - The table is append-only. The database rejects an update or a delete, so this is a
//    record and not a feed: nothing shown here can be edited, and the page says so rather
//    than leaving an operator to wonder why there is no "undo".
//  - A cursor, not a page number. `(created_at, id)` is the sort key, so a row written
//    between two pages cannot make a reviewer skip an entry. `createTable` already keeps the
//    cursor stack; this page only supplies the filters.
//
// The `changes` object is a redacted diff and its shape depends on the writer: some events
// carry a before/after pair, most carry flat context ("reason", "generation", "number").
// The row expands to whichever it is, and falls back to pretty-printed JSON so no field is
// ever hidden by the UI's guess about the shape.

import { h } from "../lib/dom";
import { dateTime, humanStatus, relative, shortId } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  badge,
  btn,
  card,
  copyButton,
  emptyState,
  field,
  input,
  mono,
  pageHead,
  sectionTitle,
  select,
} from "../ui/kit";
import { createTable, type Column } from "../ui/table";

export const meta = { title: "Audit log", crumb: "Audit log" };

/** The wire shape of the entry built in `handleAuditList`
 *  (internal/httpapi/operations.go). `target_id` and `ip` are empty strings when absent,
 *  and `changes` is the redacted diff, possibly `{}`. */
interface AuditRow {
  id: number;
  actor_id: string;
  actor_kind: string;
  action: string;
  target_type: string;
  target_id: string;
  outcome: string;
  changes: Record<string, unknown> | null;
  correlation_id: string;
  ip: string;
  created_at: string;
}

/** The outcome CHECK on `audit_events.outcome`. */
const OUTCOMES = ["success", "denied", "failure"] as const;

/** Target types the audit writers use today, offered as suggestions. The filter is exact
 *  match on a free-text column, so a type this build does not know can still be typed. */
const TARGET_TYPES = [
  "invoice",
  "payment",
  "refund",
  "credit_note",
  "service",
  "customer",
  "approval",
  "api_key",
  "user",
] as const;

/** Where a target can be opened. A type without a screen (a user, a credit note) is shown
 *  as text: a link to nowhere is worse than no link. */
const TARGET_HREF: Record<string, (id: string) => string | null> = {
  invoice: (id) => (id ? `/invoices/${id}` : null),
  payment: (id) => (id ? `/payments/${id}` : null),
  service: (id) => (id ? `/services/${id}` : null),
  customer: (id) => (id ? `/customers/${id}` : null),
  approval: () => "/approvals",
  api_key: () => "/api-keys",
};

interface Filters {
  actor_id: string;
  action: string;
  target_type: string;
  target_id: string;
  outcome: string;
  /** The raw `datetime-local` value, so the input can be restored from the URL. */
  from: string;
  to: string;
}

export default async function auditPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("audit:read")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Audit log" }),
      card({
        body: emptyState({
          title: "Not available for your role",
          body: "Reading the audit log needs the audit:read permission.",
          icon: "lock",
        }),
      }),
    );
  }

  let filters = filtersFrom(ctx.route.query);

  const tableHost = h("div");
  const empty: { title: string; body?: string; action?: Node } = { title: "", body: "" };

  const columns: Column<AuditRow>[] = [
    {
      key: "created",
      label: "Created",
      primary: true,
      render: (row) =>
        h(
          "button",
          {
            class: "trust-toggle",
            type: "button",
            "aria-expanded": "false",
            title: "Show the recorded changes",
            onclick: (event: MouseEvent) => toggleDetail(row, event.currentTarget as HTMLButtonElement),
          },
          icon("chevronRight", 12),
          h("span", { class: "dim nowrap", title: dateTime(row.created_at) }, relative(row.created_at)),
        ),
    },
    {
      key: "actor",
      label: "Actor",
      render: (row) =>
        row.actor_id
          ? h("span", { class: "mono", title: row.actor_id }, shortId(row.actor_id))
          : h("span", { class: "faint" }, "system"),
    },
    {
      key: "actor_kind",
      label: "Kind",
      hideSmall: true,
      render: (row) =>
        row.actor_kind === "api_key"
          ? badge("api key", "accent")
          : h("span", { class: "dim" }, row.actor_kind || "—"),
    },
    {
      key: "action",
      label: "Action",
      render: (row) => mono(row.action),
    },
    {
      key: "target",
      label: "Target",
      render: (row) => targetCell(row),
    },
    {
      key: "outcome",
      label: "Outcome",
      render: (row) => outcomeBadge(row.outcome),
    },
    {
      key: "ip",
      label: "IP",
      align: "right",
      hideSmall: true,
      render: (row) => (row.ip ? mono(row.ip) : h("span", { class: "faint" }, "—")),
    },
  ];

  const table = createTable<AuditRow>(ctx, {
    path: "/api/v1/staff/audit",
    columns,
    query: queryOf(filters),
    limit: 50,
    empty,
    rowKey: (row) => String(row.id),
  });
  tableHost.appendChild(table.el);

  const actorHint = h("span", { class: "field-hint" }, "A user id (uuid).");
  const actorError = h("span", { class: "field-error", hidden: true }, icon("alertCircle", 11), "A user id is a UUID.");
  const actorInput = input({
    id: "audit-actor",
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

  const actionInput = input({
    id: "audit-action",
    type: "search",
    value: filters.action,
    placeholder: "invoice.void",
    autocomplete: "off",
    class: "input--mono",
    onchange: (event) => setFilters({ ...filters, action: (event.target as HTMLInputElement).value.trim() }),
  });

  const targetTypeInput = input({
    id: "audit-target-type",
    type: "search",
    value: filters.target_type,
    placeholder: "invoice",
    autocomplete: "off",
    list: "audit-target-types",
    onchange: (event) => setFilters({ ...filters, target_type: (event.target as HTMLInputElement).value.trim() }),
  });

  const targetIdHint = h("span", { class: "field-hint" }, "A record id (uuid).");
  const targetIdError = h("span", { class: "field-error", hidden: true }, icon("alertCircle", 11), "A record id is a UUID.");
  const targetIdInput = input({
    id: "audit-target-id",
    type: "search",
    value: filters.target_id,
    placeholder: "Record id…",
    autocomplete: "off",
    onchange: (event) => {
      const value = (event.target as HTMLInputElement).value.trim();
      applyIdValidity(targetIdInput, targetIdHint, targetIdError, value);
      setFilters({ ...filters, target_id: value });
    },
  });
  applyIdValidity(targetIdInput, targetIdHint, targetIdError, filters.target_id);

  const outcomeSelect = select(
    [
      { value: "", label: "Any outcome" },
      ...OUTCOMES.map((outcome) => ({ value: outcome, label: humanStatus(outcome) })),
      // A URL can carry an outcome this build does not know; keep it selectable rather
      // than silently showing "Any outcome" while the table is filtered.
      ...(filters.outcome && !OUTCOMES.some((outcome) => outcome === filters.outcome)
        ? [{ value: filters.outcome, label: humanStatus(filters.outcome) }]
        : []),
    ],
    {
      id: "audit-outcome",
      value: filters.outcome,
      onchange: (event) => setFilters({ ...filters, outcome: (event.target as HTMLSelectElement).value }),
    },
  );

  const fromInput = input({
    id: "audit-from",
    type: "datetime-local",
    value: filters.from,
    onchange: (event) => setFilters({ ...filters, from: (event.target as HTMLInputElement).value }),
  });
  const toInput = input({
    id: "audit-to",
    type: "datetime-local",
    value: filters.to,
    onchange: (event) => setFilters({ ...filters, to: (event.target as HTMLInputElement).value }),
  });

  const clearButton = btn("Clear filters", {
    size: "sm",
    variant: "ghost",
    icon: "x",
    onclick: () =>
      setFilters({ actor_id: "", action: "", target_type: "", target_id: "", outcome: "", from: "", to: "" }),
  });

  function setFilters(next: Filters): void {
    filters = next;
    actorInput.value = next.actor_id;
    actionInput.value = next.action;
    targetTypeInput.value = next.target_type;
    targetIdInput.value = next.target_id;
    outcomeSelect.value = next.outcome;
    fromInput.value = next.from;
    toInput.value = next.to;
    clearButton.hidden = !hasFilters(next);
    ctx.replaceQuery({
      actor_id: next.actor_id || null,
      action: next.action || null,
      target_type: next.target_type || null,
      target_id: next.target_id || null,
      outcome: next.outcome || null,
      from: next.from || null,
      to: next.to || null,
    });
    describeEmpty();
    void table.reload(queryOf(next));
  }

  function describeEmpty(): void {
    if (hasFilters(filters)) {
      empty.title = "Nothing matches these filters";
      empty.body = "No audit event matches this actor, action, target and window. Widen the filters to see more.";
      empty.action = btn("Clear filters", {
        size: "sm",
        icon: "x",
        onclick: () =>
          setFilters({ actor_id: "", action: "", target_type: "", target_id: "", outcome: "", from: "", to: "" }),
      });
      return;
    }
    empty.title = "No audit events yet";
    empty.body =
      "Every staff change writes a row here as part of the transaction that made it. An empty log means nothing has changed yet, not that logging is off.";
    empty.action = undefined;
  }

  /** The expanded row. It is a real table row so it inherits the column count, but it reads
   *  as a panel: the diff, the identifiers that tie the row to a request, and the reminder
   *  that the record cannot be edited. */
  function detailRow(row: AuditRow): HTMLElement {
    return h(
      "tr",
      { class: "trust-detail" },
      h(
        "td",
        { colspan: String(columns.length) },
        h(
          "div",
          { class: "trust-detail-body" },
          sectionTitle("Recorded changes"),
          changesView(row.changes),
          h(
            "div",
            { class: "trust-detail-meta" },
            h("span", null, `event ${row.id}`),
            row.correlation_id
              ? h("span", { class: "row" }, mono(row.correlation_id, row.correlation_id), copyButton(ctx, row.correlation_id, "Copy"))
              : h("span", null, "no correlation id"),
            h("span", null, "append-only: this row cannot be edited or deleted"),
          ),
        ),
      ),
    );
  }

  function toggleDetail(row: AuditRow, button: HTMLButtonElement): void {
    const tr = button.closest("tr");
    if (!tr) return;
    const next = tr.nextElementSibling;
    if (next instanceof HTMLElement && next.classList.contains("trust-detail")) {
      next.remove();
      button.setAttribute("aria-expanded", "false");
      button.classList.remove("is-open");
      return;
    }
    button.setAttribute("aria-expanded", "true");
    button.classList.add("is-open");
    tr.after(detailRow(row));
  }

  /** One expanded diff, either as before/after columns or as pretty-printed JSON. */
  function changesView(changes: Record<string, unknown> | null): Node {
    if (!changes || Object.keys(changes).length === 0) {
      return h("p", { class: "faint" }, "No field changes were recorded for this event.");
    }
    const diff = diffRows(changes);
    if (!diff) {
      return h("pre", { class: "audit-json" }, pretty(changes));
    }
    return h(
      "div",
      { class: "stack-sm" },
      ...diff.rows.map((entry) =>
        h(
          "div",
          { class: "trust-diff" },
          h("span", { class: "trust-diff-key" }, entry.field),
          h(
            "div",
            { class: "audit-change" },
            h(
              "div",
              { class: "audit-before" },
              h("span", { class: "trust-diff-label" }, "before"),
              valueText(entry.before),
            ),
            h(
              "div",
              { class: "audit-after" },
              h("span", { class: "trust-diff-label" }, "after"),
              valueText(entry.after),
            ),
          ),
        ),
      ),
      diff.rest ? h("pre", { class: "audit-json" }, pretty(diff.rest)) : null,
    );
  }

  describeEmpty();
  clearButton.hidden = !hasFilters(filters);

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Audit log",
      sub: "Every state change, newest first, with the person or key behind it. Rows are append-only: the database refuses an update or a delete, so this log cannot be rewritten.",
      actions: btn("Refresh", {
        icon: "refresh",
        size: "sm",
        onclick: () => void table.reload(queryOf(filters)),
      }),
    }),
    card({
      body: h(
        "div",
        { class: "trust-filters" },
        h(
          "div",
          { class: "field" },
          h("label", { class: "field-label", for: "audit-actor" }, "Actor"),
          actorInput,
          actorHint,
          actorError,
        ),
        field({ label: "Action", control: actionInput }),
        field({ label: "Target type", control: targetTypeInput }),
        h(
          "div",
          { class: "field" },
          h("label", { class: "field-label", for: "audit-target-id" }, "Target id"),
          targetIdInput,
          targetIdHint,
          targetIdError,
        ),
        field({ label: "Outcome", control: outcomeSelect }),
        field({ label: "From", control: fromInput, class: "trust-field-wide" }),
        field({ label: "To", control: toInput, class: "trust-field-wide" }),
        // Suggestions for the free-text target type. A datalist keeps the filter exact-match
        // without freezing the set of types a future writer may use.
        h("datalist", { id: "audit-target-types" }, ...TARGET_TYPES.map((type) => h("option", { value: type }))),
        h("div", { class: "trust-filters-end" }, clearButton),
      ),
    }),
    tableHost,
  );
}

// ------------------------------------------------------------------ helpers --

function filtersFrom(query: URLSearchParams): Filters {
  return {
    actor_id: query.get("actor_id") ?? "",
    action: query.get("action") ?? "",
    target_type: query.get("target_type") ?? "",
    target_id: query.get("target_id") ?? "",
    outcome: query.get("outcome") ?? "",
    // A `datetime-local` input silently blanks a value it cannot parse, so the URL value is
    // checked first: the box and the request then always agree about the window.
    from: datetimeLocal(query.get("from")),
    to: datetimeLocal(query.get("to")),
  };
}

/** Accepts only a value a `datetime-local` input can display. */
function datetimeLocal(raw: string | null): string {
  if (!raw) return "";
  return /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?$/.test(raw) ? raw : "";
}

function queryOf(filters: Filters): Record<string, string | undefined> {
  return {
    // `actor_id` and `target_id` are uuid columns; a fragment would reach the database and
    // come back as an internal error, so an invalid value is simply not sent.
    actor_id: uuidLike(filters.actor_id) ? filters.actor_id : undefined,
    action: filters.action || undefined,
    target_type: filters.target_type || undefined,
    target_id: uuidLike(filters.target_id) ? filters.target_id : undefined,
    outcome: filters.outcome || undefined,
    from: rfc3339(filters.from),
    to: rfc3339(filters.to),
  };
}

function hasFilters(filters: Filters): boolean {
  return Boolean(
    filters.actor_id ||
      filters.action ||
      filters.target_type ||
      filters.target_id ||
      filters.outcome ||
      filters.from ||
      filters.to,
  );
}

/** A `datetime-local` value is local wall-clock time; the API wants RFC3339. An
 *  unparseable value is dropped rather than sent as "Invalid Date". */
function rfc3339(local: string): string | undefined {
  if (!local) return undefined;
  const when = new Date(local);
  if (Number.isNaN(when.getTime())) return undefined;
  return when.toISOString();
}

function uuidLike(value: string): boolean {
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value);
}

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

function targetCell(row: AuditRow): Node {
  const href = TARGET_HREF[row.target_type]?.(row.target_id) ?? null;
  const label = h(
    "span",
    { class: "trust-target" },
    h("span", { class: "mono" }, row.target_type || "—"),
    row.target_id ? h("span", { class: "faint mono" }, `· ${shortId(row.target_id)}`) : null,
  );
  if (!href) return label;
  return h(
    "a",
    { class: "linkish", href, title: `${row.target_type} ${row.target_id}` },
    label,
  );
}

function outcomeBadge(outcome: string): HTMLElement {
  switch (outcome) {
    case "success":
      return badge("Success", "success", { dot: true });
    case "denied":
      // A denied outcome is a policy refusal, not a malfunction: someone tried something
      // the permissions forbid, and the audit row is the evidence.
      return badge("Denied", "danger", { dot: true });
    case "failure":
      return badge("Failure", "warning", { dot: true });
    default:
      return badge(outcome || "—", "neutral", { dot: true });
  }
}

// --------------------------------------------------------------------- diff --

interface DiffEntry {
  field: string;
  before: unknown;
  after: unknown;
}

/** The key pairs that mean "before and after" in a changes map. The audit writer is not
 *  obliged to use one shape, so both the documented "field names and old/new values" and a
 *  plain before/after wrapper are recognised. */
const DIFF_PAIRS: Array<[string, string]> = [
  ["before", "after"],
  ["old", "new"],
  ["from", "to"],
  ["previous", "current"],
];

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** Returns the diff rows and any context keys that sat beside the pair, or null when the
 *  object is not a before/after shape (and should be shown as JSON). */
function diffRows(changes: Record<string, unknown>): { rows: DiffEntry[]; rest: Record<string, unknown> | null } | null {
  for (const [beforeKey, afterKey] of DIFF_PAIRS) {
    const before = changes[beforeKey];
    const after = changes[afterKey];
    if (!isRecord(before) || !isRecord(after)) continue;
    const fields = new Set([...Object.keys(before), ...Object.keys(after)]);
    const rest = Object.fromEntries(
      Object.entries(changes).filter(([key]) => key !== beforeKey && key !== afterKey),
    );
    return {
      rows: [...fields].map((field) => ({ field, before: before[field], after: after[field] })),
      rest: Object.keys(rest).length > 0 ? rest : null,
    };
  }

  // The other plausible shape: one entry per field, each carrying its own old/new pair.
  const entries = Object.entries(changes);
  const perField = entries.every(
    ([, value]) =>
      isRecord(value) &&
      (("old" in value && "new" in value) || ("before" in value && "after" in value)),
  );
  if (entries.length > 0 && perField) {
    return {
      rows: entries.map(([field, value]) => {
        const record = value as Record<string, unknown>;
        const usesOld = "old" in record;
        return {
          field,
          before: record[usesOld ? "old" : "before"],
          after: record[usesOld ? "new" : "after"],
        };
      }),
      rest: null,
    };
  }
  return null;
}

function valueText(value: unknown): string {
  if (value === null || value === undefined) return "—";
  if (typeof value === "string") return value === "" ? "—" : value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  return pretty(value);
}

function pretty(value: unknown): string {
  try {
    return JSON.stringify(value, null, 2) ?? String(value);
  } catch {
    return String(value);
  }
}
