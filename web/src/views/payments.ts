// The payments list.
//
// A payment is a fact, not an editable record: the provider charged a card, the webhook
// recorded it, and the row is what the ledger points at. So this screen offers no edit and
// no delete. The two ways to correct money are a refund (back to the card) and a credit note
// (forward as credit), and both live on the payment and invoice screens.
//
// The row shape is exactly what handleStaffPayments writes. It carries `invoice_ids`, not a
// customer: a payment is allocated to invoices, and the customer is one hop further away
// through the invoice. The column below therefore links to the invoices the payment settled,
// which is the fact an operator can actually act on.

import { h } from "../lib/dom";
import type { Amount } from "../lib/format";
import { dateTime, humanStatus, isoDate, relative, shortId, titleCase, truncate } from "../lib/format";
import type { Ctx } from "../lib/types";
import { badge, btn, card, cellMain, emptyState, field, input, moneyEl, pageHead, select, statusBadge } from "../ui/kit";
import { createTable, type Column, type Table } from "../ui/table";

export const meta = { title: "Payments", crumb: "Payments" };

/** The payment list row, verbatim from internal/httpapi/staff_read.go. */
interface PaymentRow {
  id: string;
  provider: string;
  provider_account: string;
  provider_payment_id: string;
  amount: Amount;
  currency: string;
  status: string;
  received_at: string;
  invoice_ids: string[];
  /** Set only for a payment a staff member recorded outside a gateway; null for a gateway payment. */
  method: string | null;
  /** The operator's note on an offline payment, null otherwise. */
  note: string | null;
}

/** The CHECK constraint on payments.status, in the order an operator thinks about them. */
const STATUSES = ["succeeded", "partially_refunded", "refunded", "disputed"] as const;

interface Filters {
  provider: string;
  status: string;
  customer_id: string;
  method: string;
  from: string;
  to: string;
}

export default async function paymentsPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("payment:reconcile")) return denied();

  const query = ctx.route.query;
  const state: Filters = {
    provider: query.get("provider") ?? "",
    status: query.get("status") ?? "",
    customer_id: query.get("customer_id") ?? "",
    method: query.get("method") ?? "",
    from: dayValue(query.get("from")),
    to: dayValue(query.get("to")),
  };

  const providerInput = input({
    id: "payment-filter-provider",
    placeholder: "stripe",
    value: state.provider,
    autocomplete: "off",
  });
  const statusSelect = select(
    [{ value: "", label: "Any status" }, ...STATUSES.map((s) => ({ value: s, label: humanStatus(s) }))],
    { id: "payment-filter-status", value: state.status },
  );
  // The offline methods are fetched rather than hardcoded, so the filter offers exactly what the
  // server accepts. A failed fetch costs the filter, not the list: the page still works, which is
  // the right trade for a convenience control on a read-only screen.
  const methodOptions = await offlineMethods(ctx);
  const methodSelect = select(
    [{ value: "", label: "Any method" }, ...methodOptions.map((m) => ({ value: m.value, label: m.label }))],
    { id: "payment-filter-method", value: state.method },
  );
  const customerInput = input({
    id: "payment-filter-customer",
    placeholder: "customer UUID",
    value: state.customer_id,
    autocomplete: "off",
    class: "input--mono",
  });
  const fromInput = input({ id: "payment-filter-from", type: "date", value: state.from });
  const toInput = input({ id: "payment-filter-to", type: "date", value: state.to });

  const columns: Column<PaymentRow>[] = [
    {
      key: "payment",
      label: "Payment",
      primary: true,
      render: (row) =>
        cellMain(
          // A provider reference is 60+ characters of unbreakable text. Shown whole it either
          // sets the column's width (pushing the table past its card) or, with a looser wrap
          // rule, breaks into a vertical ribbon. It is an identifier, so it is shown short with
          // the full value in the title and on the payment's own page.
          h(
            "a",
            { class: "row-title mono", href: `/payments/${row.id}`, title: row.provider_payment_id },
            truncate(row.provider_payment_id ?? "—", 22),
          ),
          h("span", { class: "faint mono" }, shortId(row.id)),
        ),
    },
    {
      key: "invoices",
      label: "Invoice",
      render: (row) => {
        const ids = row.invoice_ids ?? [];
        const first = ids[0];
        if (!first) return h("span", { class: "faint" }, "Unallocated");
        return cellMain(
          h("a", { class: "row-title", href: `/invoices/${first}` }, `Invoice ${shortId(first)}`),
          ids.length > 1 ? h("span", { class: "faint" }, `+${ids.length - 1} more`) : null,
        );
      },
    },
    {
      key: "provider",
      label: "Provider",
      render: (row) =>
        cellMain(h("span", null, row.provider), h("span", { class: "faint mono" }, row.provider_account)),
    },
    {
      // How an offline payment arrived. A gateway payment has no method here — the provider owns
      // that fact — so the column says "Gateway" rather than guessing one.
      key: "method",
      label: "Method",
      render: (row) => (row.method ? badge(titleCase(row.method), "info") : h("span", { class: "faint" }, "Gateway")),
    },
    {
      key: "status",
      label: "Status",
      render: (row) => statusBadge(row.status),
    },
    {
      key: "received",
      label: "Received",
      render: (row) => h("span", { class: "dim", title: dateTime(row.received_at) }, relative(row.received_at)),
    },
    {
      key: "amount",
      label: "Amount",
      align: "right",
      render: (row) => moneyEl(row.amount),
    },
  ];

  const table: Table<PaymentRow> = createTable<PaymentRow>(ctx, {
    path: "/api/v1/staff/payments",
    columns,
    query: queryOf(state),
    limit: 25,
    empty: {
      title: "No payments match",
      body: "Payments arrive from the provider webhook, or from a staff member recording a bank transfer on an invoice. A narrow date range or a mistyped provider name is the usual reason this list is empty.",
      action: btn("Clear filters", {
        size: "sm",
        icon: "x",
        onclick: () => {
          providerInput.value = "";
          customerInput.value = "";
          methodSelect.value = "";
          fromInput.value = "";
          toInput.value = "";
          statusSelect.value = "";
          apply();
        },
      }),
    },
    rowHref: (row) => `/payments/${row.id}`,
    rowKey: (row) => row.id,
    footNote: h("span", { class: "faint" }, "Newest first"),
  });

  const apply = () => {
    state.provider = providerInput.value.trim();
    state.status = statusSelect.value;
    state.customer_id = customerInput.value.trim();
    state.method = methodSelect.value;
    state.from = fromInput.value;
    state.to = toInput.value;
    ctx.replaceQuery({
      provider: state.provider || null,
      status: state.status || null,
      customer_id: state.customer_id || null,
      method: state.method || null,
      from: state.from || null,
      to: state.to || null,
    });
    void table.reload(queryOf(state));
  };

  // A select and a date picker commit on change; the text fields wait for Enter or the
  // Apply button (Enter in a field submits the form), because reloading a table on every
  // keystroke of a UUID is noise.
  statusSelect.addEventListener("change", apply);
  methodSelect.addEventListener("change", apply);
  fromInput.addEventListener("change", apply);
  toInput.addEventListener("change", apply);

  const filters = h(
    "form",
    {
      class: "money-filters",
      novalidate: true,
      onsubmit: (event: Event) => {
        event.preventDefault();
        apply();
      },
    },
    field({ label: "Provider", control: providerInput, id: "payment-filter-provider", hint: "Exact provider name" }),
    field({ label: "Status", control: statusSelect, id: "payment-filter-status" }),
    field({ label: "Method", control: methodSelect, id: "payment-filter-method", hint: "How an offline payment arrived" }),
    field({ label: "Customer", control: customerInput, id: "payment-filter-customer", hint: "Exact customer id" }),
    field({ label: "From", control: fromInput, id: "payment-filter-from" }),
    field({ label: "To", control: toInput, id: "payment-filter-to" }),
    h("div", { class: "money-filters-actions" }, btn("Apply", { type: "submit", icon: "filter", size: "sm" })),
  );

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Payments",
      sub: "Money that was actually collected — by the provider, or recorded by staff for a bank transfer, cash or cheque. A payment is immutable: corrections are refunds or credit notes, never edits.",
      actions: btn("Invoices", { href: "/invoices", icon: "invoice", size: "sm" }),
    }),
    card({ flush: true, tight: true, body: filters }),
    table.el,
  );
}

function queryOf(state: Filters): Record<string, string | undefined> {
  return {
    provider: state.provider || undefined,
    status: state.status || undefined,
    customer_id: state.customer_id || undefined,
    method: state.method || undefined,
    from: startOfDay(state.from),
    to: endOfDay(state.to),
  };
}

/** The offline payment vocabulary, or an empty list when the endpoint cannot be read. */
async function offlineMethods(ctx: Ctx): Promise<{ value: string; label: string }[]> {
  try {
    const res = await ctx.api.get<{ methods?: { value: string; label: string }[] }>(
      "/api/v1/staff/payment-methods/offline",
    );
    return res.methods ?? [];
  } catch {
    return [];
  }
}

/** The date input value ("2024-05-01") back from an RFC3339 query parameter. */
function dayValue(value: string | null): string {
  return value ? isoDate(value) : "";
}

/** The API parses `from`/`to` as RFC3339, so a bare date has to be widened to a day. */
function startOfDay(day: string): string | undefined {
  return validDay(day) ? `${day}T00:00:00Z` : undefined;
}

function endOfDay(day: string): string | undefined {
  return validDay(day) ? `${day}T23:59:59Z` : undefined;
}

function validDay(day: string): boolean {
  return /^\d{4}-\d{2}-\d{2}$/.test(day) && !Number.isNaN(new Date(`${day}T00:00:00Z`).getTime());
}

function denied(): HTMLElement {
  return h(
    "div",
    { class: "page" },
    pageHead({ title: "Payments" }),
    emptyState({
      title: "Payments need the reconciliation permission",
      body: "Reading payments requires payment:reconcile. The invoice list shows the same money from the other side, and every payment is linked from the invoice it settled.",
      icon: "lock",
      action: btn("Open invoices", { href: "/invoices", icon: "invoice", size: "sm" }),
    }),
  );
}
