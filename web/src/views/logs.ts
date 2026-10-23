// Logs.
//
// Three tables, three questions, one screen:
//
//   - **Email** — what was sent, to whom, and whether it worked. The durable outbox is the
//     source, so this viewer cannot disagree with it.
//   - **Errors** — what failed in the process.
//   - **HTTP** — what the process served, and how long it took.
//
// All three are keyset-paginated (`{data, next_cursor}`), so each tab is a `createTable` and the
// cursor handling — including Back, which needs a stack because a cursor is "the row after this
// one" — is the shared component's, not this page's.
//
// Two details are the server's, and the filters follow them rather than inventing a friendlier
// shape:
//
//  - The email log's recipient filter is `to`, which collides with the *time* filter `to_time`.
//    The handler distinguishes them; this page labels the address one "Recipient" and sends it
//    as `to`, and offers no time range at all because a half-implemented range is worse than
//    none. A date range is a follow-up, not a guess.
//  - A status filter on the HTTP tab is an exact code, and `min_status` is the "only errors"
//    shortcut. The form offers both, labelled as what they are.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateTime, relative, shortId, titleCase } from "../lib/format";
import type { Ctx } from "../lib/types";
import { badge, banner, btn, card, field, input, pageHead, select, skeletonLines, tabs } from "../ui/kit";
import { createTable, type Column, type Table } from "../ui/table";

export const meta = { title: "Logs", crumb: "Logs" };

// ---------------------------------------------------------------- payloads --

/** One delivery, from `emailLogRow` in internal/httpapi/logs.go. */
interface EmailLogRow {
  id: string;
  kind: string;
  to: string;
  subject: string;
  status: string;
  attempts: number;
  last_error: string;
  created_at: string;
  sent_at: string | null;
}

/** One error row, from `errorLogRow`. */
interface ErrorLogRow {
  id: number;
  level: string;
  source: string;
  message: string;
  detail: string;
  created_at: string;
}

/** One served request, from `httpLogRow`. */
interface HTTPLogRow {
  id: number;
  method: string;
  path: string;
  status: number;
  duration_ms: number;
  actor_id: string;
  ip: string;
  created_at: string;
}

type TabKey = "email" | "errors" | "http";

const TABS: Array<{ key: TabKey; label: string }> = [
  { key: "email", label: "Email" },
  { key: "errors", label: "Errors" },
  { key: "http", label: "HTTP" },
];

/** The delivery states the outbox uses. A closed list, because the column is a status. */
const EMAIL_STATUSES = ["queued", "sending", "sent", "failed", "suppressed"];

export default async function logsPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("log:read")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Logs" }),
      banner(
        "danger",
        "You cannot read the logs",
        "The log viewers need the log:read permission. They carry customer addresses and request paths, so they are not part of the default operator surface.",
        btn("Back to the dashboard", { href: "/dashboard", size: "sm", variant: "ghost" }),
      ),
    );
  }

  // The tab lives in the URL so a link to "the error log" is a real link and a refresh keeps
  // the tab. `replaceQuery` is used so switching tabs does not push a history entry per click.
  const initial = (ctx.route.query.get("tab") ?? "email") as TabKey;
  let active: TabKey = TABS.some((tab) => tab.key === initial) ? initial : "email";

  const bodyHost = h("div", { class: "stack" });
  const tabHost = h("div");

  /** Whether each tab's endpoint exists on this build, probed once and remembered.
   *
   *  `GET /api/v1/staff/logs/*` belongs to the operator-surface package. Until it is merged the
   *  request is a 404, and a 404 is a fact about the installation rather than a failed read: a
   *  table that rendered "That did not work" would be inviting a retry that cannot succeed. So
   *  the screen asks once per tab with a one-row read and, when the answer is 404, draws an
   *  explanation instead of the table. The probe is one cheap request per tab per visit. */
  const availability = new Map<TabKey, boolean>();
  const probe = async (key: TabKey, path: string): Promise<boolean> => {
    const known = availability.get(key);
    if (known !== undefined) return known;
    try {
      await ctx.api.get<{ data: unknown[] }>(path, { query: { limit: 1 }, cache: false });
      availability.set(key, true);
      return true;
    } catch (err) {
      const missing = err instanceof ApiError && (err.status === 404 || err.status === 501);
      if (missing) {
        availability.set(key, false);
        return false;
      }
      // Any other failure is the table's to report: it has the retry button that is right for a
      // transient error, and duplicating it here would be two error blocks for one failure.
      availability.set(key, true);
      return true;
    }
  };

  const renderTabs = (): void => {
    swap(
      tabHost,
      tabs(
        TABS.map((tab) => ({ key: tab.key, label: tab.label })),
        active,
        (key) => {
          if (key === active) return;
          active = key as TabKey;
          ctx.replaceQuery({ tab: active });
          renderTabs();
          renderBody();
        },
      ),
    );
  };

  const renderBody = (): void => {
    const key = active;
    const path = `/api/v1/staff/logs/${key}`;
    swap(bodyHost, h("div", { class: "stack" }, skeletonLines()));
    void (async () => {
      const available = await probe(key, path);
      // A tab switch while the probe was in flight must not paint over the newer tab.
      if (key !== active) return;
      swap(bodyHost, available ? (key === "email" ? emailTab() : key === "errors" ? errorsTab() : httpTab()) : unavailableCard(key));
    })();
  };

  /** What a missing log endpoint looks like: which table is missing, and what still works. */
  const unavailableCard = (key: TabKey): HTMLElement => {
    const label = TABS.find((tab) => tab.key === key)?.label ?? key;
    return card({
      title: `The ${label.toLowerCase()} log is not available in this build`,
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "p",
          { class: "field-hint" },
          "This installation does not serve ",
          h("code", { class: "inline-code" }, `GET /api/v1/staff/logs/${key}`),
          ", so there is nothing to read. Reloading will give the same answer until a build that includes the log viewers is deployed.",
        ),
        h(
          "p",
          { class: "field-hint" },
          "The tables themselves are still written: the records accumulate in the database and appear here as soon as a build that can read them is running. Nothing is lost by the gap.",
        ),
        h(
          "div",
          { class: "row" },
          btn("Back to the dashboard", { href: "/dashboard", size: "sm", icon: "home" }),
          btn("Audit log", { href: "/audit", size: "sm", variant: "ghost", icon: "shield" }),
        ),
      ),
    });
  };

  // ------------------------------------------------------------------ email --

  const emailTab = (): HTMLElement => {
    const filters = {
      status: ctx.route.query.get("status") ?? "",
      kind: ctx.route.query.get("kind") ?? "",
      to: ctx.route.query.get("to") ?? "",
    };
    const statusSelect = select(
      [{ value: "", label: "Any status" }, ...EMAIL_STATUSES.map((s) => ({ value: s, label: titleCase(s) }))],
      { id: "log-email-status", value: filters.status },
    );
    const kindInput = input({
      id: "log-email-kind",
      value: filters.kind,
      placeholder: "invoice_paid",
      class: "input--mono",
      autocomplete: "off",
    });
    const toInput = input({
      id: "log-email-to",
      value: filters.to,
      placeholder: "ada@example.com",
      autocomplete: "off",
    });

    let table: Table<EmailLogRow> | null = null;
    const apply = btn("Apply", { size: "sm", icon: "filter" });
    const clear = btn("Clear", { size: "sm", variant: "ghost" });
    const query = () => ({
      status: statusSelect.value || undefined,
      kind: kindInput.value.trim() || undefined,
      to: toInput.value.trim() || undefined,
    });
    apply.addEventListener("click", () => void table?.reload(query()));
    clear.addEventListener("click", () => {
      statusSelect.value = "";
      kindInput.value = "";
      toInput.value = "";
      void table?.reload({});
    });
    for (const control of [kindInput, toInput]) {
      onEnter(control, () => void table?.reload(query()));
    }
    statusSelect.addEventListener("change", () => void table?.reload(query()));

    const columns: Column<EmailLogRow>[] = [
      {
        key: "subject",
        label: "Email",
        primary: true,
        render: (row) =>
          h(
            "div",
            { class: "cell-main" },
            h("div", { class: "primary" }, h("span", { class: "row-title" }, row.subject || row.kind || "Message")),
            h(
              "div",
              { class: "secondary" },
              h("span", { class: "mono" }, row.to || "—"),
              row.kind ? ` · ${row.kind}` : "",
            ),
          ),
      },
      {
        key: "status",
        label: "Status",
        render: (row) =>
          h(
            "div",
            { class: "stack-sm" },
            badge(titleCase(row.status || "unknown"), toneFor(row.status), { dot: true }),
            row.attempts > 1 ? h("span", { class: "faint" }, `${row.attempts} attempts`) : null,
          ),
      },
      {
        key: "error",
        label: "Last error",
        hideSmall: true,
        render: (row) =>
          row.last_error
            ? h("span", { class: "log-error", title: row.last_error }, row.last_error)
            : h("span", { class: "faint" }, "—"),
      },
      {
        key: "created",
        label: "Queued",
        align: "right",
        render: (row) => h("span", { class: "dim nowrap", title: dateTime(row.created_at) }, relative(row.created_at)),
      },
      {
        key: "sent",
        label: "Sent",
        align: "right",
        hideSmall: true,
        render: (row) =>
          row.sent_at
            ? h("span", { class: "dim nowrap", title: dateTime(row.sent_at) }, relative(row.sent_at))
            : h("span", { class: "faint nowrap" }, "Not yet"),
      },
    ];

    table = createTable<EmailLogRow>(ctx, {
      path: "/api/v1/staff/logs/email",
      columns,
      query: query(),
      limit: 50,
      empty: {
        title: "No deliveries match",
        body: "The outbox records every message the system queued. If nothing matches, no message of that kind was queued for that recipient.",
      },
      rowKey: (row) => row.id,
      footNote: "Newest first. The table grows on every request, so it is keyset-paginated rather than offset.",
    });

    return h(
      "div",
      { class: "stack" },
      card({
        body: h(
          "div",
          { class: "log-filters" },
          field({ label: "Status", control: statusSelect, id: "log-email-status" }),
          field({ label: "Kind", control: kindInput, id: "log-email-kind", hint: "The event key, e.g. invoice_paid." }),
          field({ label: "Recipient", control: toInput, id: "log-email-to", hint: "A fragment of the address." }),
          h("div", { class: "log-filter-actions" }, apply, clear),
        ),
      }),
      table.el,
    );
  };

  // ----------------------------------------------------------------- errors --

  const errorsTab = (): HTMLElement => {
    const levelInput = input({
      id: "log-err-level",
      value: ctx.route.query.get("level") ?? "",
      placeholder: "error",
      class: "input--mono",
      autocomplete: "off",
    });
    const sourceInput = input({
      id: "log-err-source",
      value: ctx.route.query.get("source") ?? "",
      placeholder: "worker.renewals",
      class: "input--mono",
      autocomplete: "off",
    });
    const searchInput = input({
      id: "log-err-q",
      value: ctx.route.query.get("q") ?? "",
      placeholder: "a fragment of the message",
      autocomplete: "off",
    });

    let table: Table<ErrorLogRow> | null = null;
    const query = () => ({
      level: levelInput.value.trim() || undefined,
      source: sourceInput.value.trim() || undefined,
      q: searchInput.value.trim() || undefined,
    });
    const apply = btn("Apply", { size: "sm", icon: "filter" });
    apply.addEventListener("click", () => void table?.reload(query()));
    const clear = btn("Clear", { size: "sm", variant: "ghost" });
    clear.addEventListener("click", () => {
      levelInput.value = "";
      sourceInput.value = "";
      searchInput.value = "";
      void table?.reload({});
    });
    for (const control of [levelInput, sourceInput, searchInput]) {
      onEnter(control, () => void table?.reload(query()));
    }

    const columns: Column<ErrorLogRow>[] = [
      {
        key: "message",
        label: "Error",
        primary: true,
        render: (row) =>
          h(
            "div",
            { class: "cell-main" },
            h(
              "div",
              { class: "primary" },
              h("span", { class: "row-title" }, row.message || "Unspecified error"),
              badge(row.level || "error", toneForLevel(row.level), { dot: true }),
            ),
            h("div", { class: "secondary mono" }, row.source || "unknown source"),
          ),
      },
      {
        key: "detail",
        label: "Detail",
        hideSmall: true,
        // The row keeps one ellipsised line (a stack trace in a cell makes every other row
        // unreadable) but the rest is a real element behind a disclosure rather than a `title`
        // tooltip: a tooltip is invisible to a screenshot, a screen reader and a touch device, and
        // the diagnostic text is the whole reason an operator opened this tab.
        render: (row) =>
          row.detail
            ? h(
                "details",
                { class: "log-detail-disclosure" },
                h("summary", null, `diagnostic text (${row.detail.length} bytes)`),
                h("pre", { class: "log-detail" }, row.detail),
              )
            : h("span", { class: "faint" }, "—"),
      },
      {
        key: "when",
        label: "When",
        align: "right",
        render: (row) => h("span", { class: "dim nowrap", title: dateTime(row.created_at) }, relative(row.created_at)),
      },
    ];

    table = createTable<ErrorLogRow>(ctx, {
      path: "/api/v1/staff/logs/errors",
      columns,
      query: query(),
      limit: 50,
      empty: {
        title: "No errors recorded",
        body: "Nothing has failed in the process since the table was last pruned. An empty error log is the state you want; a stale one is the state you should check the worker for.",
      },
      rowKey: (row) => String(row.id),
      footNote: "Detail is redacted at write time: a credential never reaches this table.",
    });

    return h(
      "div",
      { class: "stack" },
      card({
        body: h(
          "div",
          { class: "log-filters" },
          field({ label: "Level", control: levelInput, id: "log-err-level", hint: "error, warn or critical." }),
          field({ label: "Source", control: sourceInput, id: "log-err-source", hint: "The component that failed." }),
          field({ label: "Message contains", control: searchInput, id: "log-err-q" }),
          h("div", { class: "log-filter-actions" }, apply, clear),
        ),
      }),
      table.el,
    );
  };

  // ------------------------------------------------------------------- http --

  const httpTab = (): HTMLElement => {
    const method = select(
      [
        { value: "", label: "Any method" },
        ...["GET", "POST", "PUT", "DELETE", "PATCH"].map((value) => ({ value, label: value })),
      ],
      { id: "log-http-method", value: ctx.route.query.get("method") ?? "" },
    );
    const statusInput = input({
      id: "log-http-status",
      value: ctx.route.query.get("status") ?? "",
      placeholder: "500",
      inputmode: "numeric",
      class: "input--mono",
      autocomplete: "off",
      maxlength: 3,
    });
    const minStatus = select(
      [
        { value: "", label: "Any status" },
        { value: "400", label: "400 and above (client errors)" },
        { value: "500", label: "500 and above (server errors)" },
      ],
      { id: "log-http-min-status", value: ctx.route.query.get("min_status") ?? "" },
    );
    const pathInput = input({
      id: "log-http-path",
      value: ctx.route.query.get("path") ?? "",
      placeholder: "/api/v1/staff/invoices",
      class: "input--mono",
      autocomplete: "off",
    });

    let table: Table<HTTPLogRow> | null = null;
    const query = () => ({
      method: method.value || undefined,
      status: statusInput.value.trim() || undefined,
      min_status: minStatus.value || undefined,
      path: pathInput.value.trim() || undefined,
    });
    const apply = btn("Apply", { size: "sm", icon: "filter" });
    apply.addEventListener("click", () => void table?.reload(query()));
    const clear = btn("Clear", { size: "sm", variant: "ghost" });
    clear.addEventListener("click", () => {
      method.value = "";
      statusInput.value = "";
      minStatus.value = "";
      pathInput.value = "";
      void table?.reload({});
    });
    for (const control of [statusInput, pathInput]) {
      onEnter(control, () => void table?.reload(query()));
    }
    method.addEventListener("change", () => void table?.reload(query()));
    minStatus.addEventListener("change", () => void table?.reload(query()));

    const columns: Column<HTTPLogRow>[] = [
      {
        key: "path",
        label: "Request",
        primary: true,
        render: (row) =>
          h(
            "div",
            { class: "cell-main" },
            h(
              "div",
              { class: "primary" },
              h("span", { class: "chip mono" }, row.method),
              h("span", { class: "mono" }, row.path),
            ),
            h(
              "div",
              { class: "secondary" },
              row.actor_id ? `actor ${shortId(row.actor_id)}` : "anonymous",
              row.ip ? ` · ${row.ip}` : "",
            ),
          ),
      },
      {
        key: "status",
        label: "Status",
        align: "right",
        render: (row) => badge(String(row.status), toneForStatus(row.status), { dot: false }),
      },
      {
        key: "duration",
        label: "Duration",
        align: "right",
        hideSmall: true,
        render: (row) =>
          h(
            "span",
            { class: row.duration_ms > 1000 ? "mono log-slow" : "mono" },
            `${row.duration_ms.toLocaleString("en-US")} ms`,
          ),
      },
      {
        key: "when",
        label: "When",
        align: "right",
        render: (row) => h("span", { class: "dim nowrap", title: dateTime(row.created_at) }, relative(row.created_at)),
      },
    ];

    table = createTable<HTTPLogRow>(ctx, {
      path: "/api/v1/staff/logs/http",
      columns,
      query: query(),
      limit: 50,
      empty: {
        title: "No requests match",
        body: "The HTTP log records what this process served. The query string is stripped before it is stored, so searching for a token will not find anything — by design.",
      },
      rowKey: (row) => String(row.id),
      footNote: "Headers are never stored: a header map is where a session cookie would end up in a backup.",
    });

    return h(
      "div",
      { class: "stack" },
      card({
        body: h(
          "div",
          { class: "log-filters" },
          field({ label: "Method", control: method, id: "log-http-method" }),
          field({ label: "Exact status", control: statusInput, id: "log-http-status", hint: "e.g. 404" }),
          field({ label: "At least", control: minStatus, id: "log-http-min-status" }),
          field({ label: "Path starts with", control: pathInput, id: "log-http-path" }),
          h("div", { class: "log-filter-actions" }, apply, clear),
        ),
      }),
      table.el,
    );
  };

  renderTabs();
  renderBody();

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Logs",
      sub: "What the system sent, what failed, and what it served. Three tables, keyset-paginated, so a row is never skipped while you scroll.",
      actions: btn("Refresh", {
        variant: "ghost",
        size: "sm",
        icon: "refresh",
        onclick: () => renderBody(),
      }),
    }),
    banner(
      "info",
      "Redaction happens on the way in",
      h(
        "span",
        null,
        "A body or a header that is stored is stored in a backup and a replica, so a credential must not reach the table in the first place. The HTTP log keeps the method, path, status, duration and actor — never a header, and never a query string.",
      ),
    ),
    tabHost,
    bodyHost,
  );
}

// ----------------------------------------------------------------- helpers --

/** Applies a filter on Enter, which is what an operator expects from a search box. */
function onEnter(control: HTMLElement, run: () => void): void {
  control.addEventListener("keydown", (event) => {
    if ((event as KeyboardEvent).key !== "Enter") return;
    event.preventDefault();
    run();
  });
}

function toneFor(status: string): "success" | "warning" | "danger" | "info" | "neutral" {
  switch (status) {
    case "sent":
      return "success";
    case "queued":
    case "sending":
      return "info";
    case "failed":
      return "danger";
    case "suppressed":
      return "neutral";
    default:
      return "neutral";
  }
}

function toneForLevel(level: string): "danger" | "warning" | "neutral" {
  switch (level) {
    case "critical":
    case "error":
      return "danger";
    case "warn":
    case "warning":
      return "warning";
    default:
      return "neutral";
  }
}

function toneForStatus(status: number): "success" | "info" | "warning" | "danger" | "neutral" {
  if (status >= 500) return "danger";
  if (status >= 400) return "warning";
  if (status >= 300) return "info";
  if (status >= 200) return "success";
  return "neutral";
}
