// The invoice list.
//
// Every filter here is a query parameter the server actually parses
// (handleStaffInvoices reads status, overdue, currency, customer_id, from and to), and the
// URL is kept in step with them so a filtered view can be linked, bookmarked and reloaded.
// Nothing is filtered in the browser: a filter that silently drops rows from the page that
// happens to be loaded would be a lie about the ledger.
//
// The summary strip is computed from the loaded page and says so in as many words. The list
// paginates by keyset cursor and there is no aggregate endpoint; a "total outstanding"
// figure invented from one page would disagree with the customer screen the moment anybody
// compared them.

import type { Amount } from "../lib/format";
import { dateOnly, daysUntil, relative, shortId } from "../lib/format";
import { debounce, h } from "../lib/dom";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  btn,
  card,
  errorState,
  field,
  input,
  isAbort,
  moneyEl,
  pageHead,
  select,
  statusBadge,
  switchField,
} from "../ui/kit";
import { createTable, type Column } from "../ui/table";

export const meta = { title: "Invoices", crumb: "Invoices" };

/** One row of GET /api/v1/staff/invoices. */
interface InvoiceRow {
  id: string;
  number: string;
  status: string;
  currency: string;
  total: Amount;
  paid: Amount;
  due: Amount;
  overdue: boolean;
  issued_at: string | null;
  due_at: string | null;
  customer?: { id: string; email: string };
}

/** A row of GET /api/v1/staff/customers?q=… — only what the filter needs. */
interface CustomerHit {
  id: string;
  email: string;
  name: string;
}

interface Filters {
  status: string;
  overdue: boolean;
  currency: string;
  customerId: string;
  /** yyyy-mm-dd, as typed into the date input; the URL carries an RFC3339 timestamp. */
  from: string;
  to: string;
}

const STATUS_OPTIONS = [
  { value: "", label: "Any status" },
  { value: "draft", label: "Draft" },
  { value: "open", label: "Open" },
  { value: "paid", label: "Paid" },
  { value: "void", label: "Void" },
];

export default async function invoicesPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("invoice:read:all")) {
    ctx.navigate("/", { replace: true });
    return h("div");
  }

  const filters = readFilters(ctx.route.query);
  const summaryHost = h("div", { class: "inv-summary" });
  const table = createTable<InvoiceRow>(ctx, {
    path: "/api/v1/staff/invoices",
    columns: columns(),
    query: tableQuery(filters),
    empty: {
      title: "No invoice matches these filters",
      body: "Clear a filter, or create an invoice for a customer who should be billed.",
      action: ctx.can("invoice:write:all")
        ? btn("New invoice", { href: "/invoices/new", icon: "plus", size: "sm" })
        : undefined,
    },
    rowHref: (row) => `/invoices/${row.id}`,
    footNote: "Filters apply on the server; the summary above covers this page.",
    onLoaded: (rows) => renderSummary(summaryHost, rows),
  });

  /** Writes the filters to the URL, reloads the first page and re-syncs the chrome. */
  const apply = (patch: Partial<Filters>) => {
    Object.assign(filters, patch);
    ctx.replaceQuery({
      status: filters.status || null,
      overdue: filters.overdue ? "true" : null,
      currency: filters.currency || null,
      customer_id: filters.customerId || null,
      from: timestamp(filters.from, "start") ?? null,
      to: timestamp(filters.to, "end") ?? null,
    });
    exportLink.href = exportHref(filters);
    void table.reload(tableQuery(filters));
    syncChrome();
  };

  const statusSelect = select(STATUS_OPTIONS, {
    id: "invoice-filter-status",
    value: filters.status,
    onchange: (event) => apply({ status: (event.target as HTMLSelectElement).value }),
  });

  const currencyInput = input({
    id: "invoice-filter-currency",
    value: filters.currency,
    placeholder: "USD",
    maxlength: 12,
    class: "input--mono",
    onchange: (event) => apply({ currency: normalizeCode((event.target as HTMLInputElement).value) }),
  });

  const fromInput = h("input", {
    class: "input",
    type: "date",
    id: "invoice-filter-from",
    value: filters.from,
    onchange: (event: Event) => apply({ from: (event.target as HTMLInputElement).value }),
  });

  const toInput = h("input", {
    class: "input",
    type: "date",
    id: "invoice-filter-to",
    value: filters.to,
    onchange: (event: Event) => apply({ to: (event.target as HTMLInputElement).value }),
  });

  const overdueSwitch = switchField("Overdue only", {
    checked: filters.overdue,
    onchange: (event) => apply({ overdue: (event.target as HTMLInputElement).checked }),
  });

  // The customer filter is a search, not a uuid box. `customer_id` is the only parameter the
  // server accepts, and the reliable way to obtain one is to pick the customer meant.
  let customerLabel = "";
  let customerSearch: AbortController | null = null;
  const customerFilterInput = input({
    id: "invoice-filter-customer",
    placeholder: "Email or name",
    autocomplete: "off",
    oninput: (event: Event) => void searchCustomer((event.target as HTMLInputElement).value),
  });
  const customerResults = h("div", { class: "search-results" });
  const customerResultsCard = h("div", { class: "card inv-picker-results", hidden: true }, customerResults);
  const customerFilterHost = h(
    "div",
    { class: "inv-picker" },
    field({ label: "Customer", control: customerFilterInput, id: "invoice-filter-customer" }),
    customerResultsCard,
  );

  const searchCustomer = debounce(async (raw: string) => {
    const q = raw.trim();
    customerSearch?.abort();
    if (q === "") {
      customerResultsCard.hidden = true;
      return;
    }
    const controller = new AbortController();
    customerSearch = controller;
    customerResultsCard.hidden = false;
    customerResults.replaceChildren(h("div", { class: "inv-picker-state" }, h("span", { class: "spinner" }), "Searching…"));
    try {
      const page = await ctx.api.get<{ data: CustomerHit[] }>("/api/v1/staff/customers", {
        query: { q, limit: 6 },
        cache: false,
        signal: controller.signal,
      });
      if (controller.signal.aborted) return;
      if (page.data.length === 0) {
        customerResults.replaceChildren(h("div", { class: "inv-picker-state" }, `No customer matches “${q}”.`));
        return;
      }
      customerResults.replaceChildren(
        ...page.data.map((hit) =>
          h(
            "button",
            {
              class: "search-hit",
              type: "button",
              onclick: () => {
                customerResultsCard.hidden = true;
                customerFilterInput.value = "";
                customerLabel = hit.name || hit.email;
                apply({ customerId: hit.id });
              },
            },
            h("span", { class: "search-hit-icon" }, icon("user", 13)),
            h(
              "div",
              { class: "search-hit-body" },
              h("div", { class: "search-hit-title" }, hit.name || hit.email),
              h("div", { class: "search-hit-sub" }, hit.name ? hit.email : "No display name"),
            ),
          ),
        ),
      );
    } catch (err) {
      if (controller.signal.aborted || isAbort(err)) return;
      customerResults.replaceChildren(errorState(err, () => void searchCustomer(q)));
    }
  }, 250);
  ctx.onDispose(() => customerSearch?.abort());

  // The export is a link, not a fetch. The response is text/csv with Content-Disposition,
  // so the browser saves it directly and a hundred thousand invoices never pass through
  // JavaScript. The href carries the same filters as the list, so the file is the list the
  // operator is looking at; `download=""` keeps the server's timestamped filename rather
  // than inventing one here.
  const exportLink = h(
    "a",
    {
      class: "btn btn--sm",
      href: exportHref(filters),
      download: "",
      title: "Download every invoice matching these filters as CSV",
    },
    icon("download", 14),
    "Export CSV",
  );

  const chipHost = h("div", { class: "toolbar" });
  const clearButton = btn("Clear filters", { variant: "ghost", size: "sm", icon: "x", onclick: () => clearAll() });

  const hasFilters = (): boolean =>
    Boolean(filters.status || filters.overdue || filters.currency || filters.customerId || filters.from || filters.to);

  const clearAll = () => {
    customerLabel = "";
    statusSelect.value = "";
    currencyInput.value = "";
    fromInput.value = "";
    toInput.value = "";
    const box = overdueSwitch.querySelector("input");
    if (box instanceof HTMLInputElement) box.checked = false;
    apply({ status: "", overdue: false, currency: "", customerId: "", from: "", to: "" });
  };

  /** Keeps the chip and the clear button honest after any filter change. */
  const syncChrome = () => {
    chipHost.replaceChildren(
      filters.customerId
        ? h(
            "span",
            { class: "chip", title: filters.customerId },
            icon("user", 11),
            h("span", null, customerLabel || shortId(filters.customerId, 8)),
            h(
              "button",
              {
                class: "chip-x",
                type: "button",
                title: "Clear the customer filter",
                "aria-label": "Clear the customer filter",
                onclick: () => {
                  customerLabel = "";
                  apply({ customerId: "" });
                },
              },
              icon("x", 10),
            ),
          )
        : h("span"),
    );
    if (clearButton instanceof HTMLButtonElement) clearButton.disabled = !hasFilters();
  };

  const filterBar = card({
    class: "inv-filters",
    body: h(
      "div",
      { class: "stack-sm" },
      h(
        "div",
        { class: "inv-filter-grid" },
        field({ label: "Status", control: statusSelect, id: "invoice-filter-status" }),
        customerFilterHost,
        field({
          label: "Currency",
          control: currencyInput,
          id: "invoice-filter-currency",
          hint: "ISO 4217 code",
        }),
        field({
          label: "Issued from",
          control: fromInput,
          id: "invoice-filter-from",
          hint: "UTC. A draft has no issue date and drops out.",
        }),
        field({ label: "Issued to", control: toInput, id: "invoice-filter-to" }),
      ),
      h("div", { class: "toolbar" }, overdueSwitch, chipHost, h("div", { class: "toolbar-end" }, clearButton)),
    ),
  });
  syncChrome();

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Invoices",
      sub: "Every document the engine has issued, in number order per page. Overdue is a state, not a status: it is computed from the due date.",
      actions: h(
        "div",
        { class: "row" },
        exportLink,
        ctx.can("invoice:write:all")
          ? btn("New invoice", { href: "/invoices/new", variant: "primary", size: "sm", icon: "plus" })
          : null,
      ),
    }),
    summaryHost,
    filterBar,
    table.el,
  );
}

// ------------------------------------------------------------------ columns --

function columns(): Column<InvoiceRow>[] {
  return [
    {
      key: "number",
      label: "Invoice",
      primary: true,
      render: (row) =>
        h(
          "div",
          { class: "cell-main" },
          h(
            "div",
            { class: "primary" },
            h("a", { class: "row-title", href: `/invoices/${row.id}` }, row.number || "Draft"),
          ),
          h("div", { class: "secondary" }, row.customer?.email ?? "No customer"),
        ),
    },
    {
      key: "status",
      label: "Status",
      render: (row) => statusBadge(row.overdue ? "overdue" : row.status),
    },
    {
      key: "issued",
      label: "Issued",
      hideSmall: true,
      render: (row) =>
        row.issued_at
          ? h("span", { class: "dim", title: dateOnly(row.issued_at) }, relative(row.issued_at))
          : h("span", { class: "faint" }, "Not issued"),
    },
    {
      key: "due",
      label: "Due",
      render: (row) => {
        if (!row.due_at) return h("span", { class: "faint" }, "—");
        const days = daysUntil(row.due_at);
        return h(
          "div",
          { class: "cell-main" },
          h("div", { class: "primary" }, dateOnly(row.due_at)),
          row.status === "open" || row.overdue
            ? h(
                "div",
                { class: "secondary" },
                row.overdue && days !== null ? `${Math.abs(days)}d overdue` : relative(row.due_at),
              )
            : null,
        );
      },
    },
    { key: "total", label: "Total", align: "right", render: (row) => moneyEl(row.total) },
    {
      key: "paid",
      label: "Paid",
      align: "right",
      hideSmall: true,
      render: (row) => (row.paid.minor > 0 ? moneyEl(row.paid) : h("span", { class: "faint" }, "—")),
    },
    {
      key: "outstanding",
      label: "Outstanding",
      align: "right",
      render: (row) => (row.due.minor > 0 ? moneyEl(row.due) : h("span", { class: "faint" }, "Settled")),
    },
  ];
}

// ------------------------------------------------------------------ summary --

/** Rebuilt from each loaded page. Deliberately not called a total: it is this page. */
function renderSummary(host: HTMLElement, rows: InvoiceRow[]): void {
  const outstanding = new Map<string, { minor: number; scale: number }>();
  let overdue = 0;
  for (const row of rows) {
    if (row.overdue) overdue += 1;
    if (row.due.minor <= 0) continue;
    const bucket = outstanding.get(row.due.currency) ?? { minor: 0, scale: row.due.scale };
    bucket.minor += row.due.minor;
    outstanding.set(row.due.currency, bucket);
  }

  const items: Node[] = [
    summaryItem("Invoices on this page", String(rows.length)),
    summaryItem("Overdue on this page", String(overdue)),
  ];
  if (outstanding.size === 0) {
    items.push(summaryItem("Outstanding on this page", h("span", { class: "faint" }, "Nothing due")));
  }
  for (const [currency, bucket] of [...outstanding.entries()].sort((a, b) => a[0].localeCompare(b[0]))) {
    items.push(
      summaryItem(
        `Outstanding ${currency}`,
        moneyEl({ minor: bucket.minor, currency, scale: bucket.scale }),
      ),
    );
  }

  host.replaceChildren(
    h("div", { class: "inv-summary-items" }, ...items),
    h(
      "p",
      { class: "inv-summary-note" },
      icon("info", 12),
      "Computed from the loaded page only. The list pages by cursor, so this is not a book total.",
    ),
  );
}

function summaryItem(label: string, value: Node | string): HTMLElement {
  return h(
    "div",
    { class: "inv-summary-item" },
    h("span", { class: "inv-summary-label" }, label),
    h("span", { class: "inv-summary-value" }, value),
  );
}

// ------------------------------------------------------------------ filters --

function readFilters(query: URLSearchParams): Filters {
  return {
    status: query.get("status") ?? "",
    overdue: query.get("overdue") === "true",
    currency: normalizeCode(query.get("currency") ?? ""),
    customerId: query.get("customer_id") ?? "",
    from: dateValue(query.get("from")),
    to: dateValue(query.get("to")),
  };
}

/** The parameters the server reads, with empty values omitted. */
function tableQuery(filters: Filters): Record<string, string | undefined> {
  return {
    status: filters.status || undefined,
    overdue: filters.overdue ? "true" : undefined,
    currency: filters.currency || undefined,
    customer_id: filters.customerId || undefined,
    from: timestamp(filters.from, "start"),
    to: timestamp(filters.to, "end"),
  };
}

/** The CSV export path for the current filters.
 *
 *  It is built from the same `tableQuery` the list sends, so the file cannot contain a
 *  different set of rows than the screen: one filter vocabulary, two renderings of it. */
function exportHref(filters: Filters): string {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(tableQuery(filters))) {
    if (value !== undefined && value !== "") params.set(key, value);
  }
  const query = params.toString();
  return `/api/v1/staff/exports/invoices.csv${query ? `?${query}` : ""}`;
}

/** A date input's value as the RFC3339 instant handleStaffInvoices parses. */
function timestamp(date: string, edge: "start" | "end"): string | undefined {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(date)) return undefined;
  return edge === "start" ? `${date}T00:00:00Z` : `${date}T23:59:59Z`;
}

/** An RFC3339 query parameter (or a plain date) as the yyyy-mm-dd a date input wants.
 *
 *  The literal date part is used rather than the browser's local date: the parameter is an
 *  instant, and converting it to a local day would show "the day before" west of UTC for the
 *  `T00:00:00Z` values this page writes. */
function dateValue(raw: string | null): string {
  if (!raw) return "";
  const match = /^(\d{4}-\d{2}-\d{2})/.exec(raw);
  return match?.[1] ?? "";
}

function normalizeCode(raw: string): string {
  return raw.trim().toUpperCase();
}
