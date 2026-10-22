// The orders list.
//
// An order is a checkout intent: the customer picked something from the catalog, the total
// was priced, and an invoice is what eventually carries the money. That is why the row leads
// with the order and carries the invoice beside it — an order without an invoice is a
// checkout that never got far enough, and that is exactly what an operator scans for.

import { h } from "../lib/dom";
import type { Amount } from "../lib/format";
import { dateTime, humanStatus, relative, shortId } from "../lib/format";
import type { Ctx } from "../lib/types";
import { btn, card, cellMain, emptyState, field, input, moneyEl, pageHead, select, statusBadge } from "../ui/kit";
import { createTable, type Column, type Table } from "../ui/table";

export const meta = { title: "Orders", crumb: "Orders" };

/** The order list row, verbatim from handleStaffOrders. */
interface OrderRow {
  id: string;
  customer: { id: string; email: string };
  status: string;
  currency: string;
  total: Amount;
  created_at: string;
  invoice_id: string | null;
}

/** The CHECK constraint on orders.status. */
const STATUSES = ["pending_payment", "paid", "fulfilling", "completed", "cancelled", "needs_review"] as const;

interface Filters {
  status: string;
  customer_id: string;
}

export default async function ordersPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("invoice:read:all")) return denied();

  const query = ctx.route.query;
  const state: Filters = {
    status: query.get("status") ?? "",
    customer_id: query.get("customer_id") ?? "",
  };

  const statusSelect = select(
    [{ value: "", label: "Any status" }, ...STATUSES.map((s) => ({ value: s, label: humanStatus(s) }))],
    { id: "order-filter-status", value: state.status },
  );
  const customerInput = input({
    id: "order-filter-customer",
    placeholder: "customer UUID",
    value: state.customer_id,
    autocomplete: "off",
    class: "input--mono",
  });

  const columns: Column<OrderRow>[] = [
    {
      key: "order",
      label: "Order",
      primary: true,
      render: (row) =>
        cellMain(
          h("a", { class: "row-title mono", href: `/orders/${row.id}` }, shortId(row.id, 12)),
          h("span", { class: "faint", title: dateTime(row.created_at) }, relative(row.created_at)),
        ),
    },
    {
      key: "customer",
      label: "Customer",
      render: (row) =>
        row.customer?.id
          ? h("a", { class: "linkish", href: `/customers/${row.customer.id}` }, row.customer.email || shortId(row.customer.id))
          : h("span", { class: "faint" }, "—"),
    },
    {
      key: "status",
      label: "Status",
      render: (row) => statusBadge(row.status),
    },
    {
      key: "total",
      label: "Total",
      align: "right",
      render: (row) => moneyEl(row.total),
    },
    {
      key: "created",
      label: "Created",
      render: (row) => h("span", { class: "dim", title: dateTime(row.created_at) }, relative(row.created_at)),
    },
    {
      key: "invoice",
      label: "Invoice",
      render: (row) =>
        row.invoice_id
          ? h("a", { class: "row-title", href: `/invoices/${row.invoice_id}` }, `Invoice ${shortId(row.invoice_id)}`)
          : h("span", { class: "faint" }, "Not invoiced"),
    },
  ];

  const table: Table<OrderRow> = createTable<OrderRow>(ctx, {
    path: "/api/v1/staff/orders",
    columns,
    query: queryOf(state),
    limit: 25,
    empty: {
      title: "No orders match",
      body: "Orders are created when a customer checks out from the catalog. Clear the filters to see all of them.",
      action: btn("Clear filters", {
        size: "sm",
        icon: "x",
        onclick: () => {
          statusSelect.value = "";
          customerInput.value = "";
          apply();
        },
      }),
    },
    rowHref: (row) => `/orders/${row.id}`,
    rowKey: (row) => row.id,
    footNote: h("span", { class: "faint" }, "Newest first"),
  });

  const apply = () => {
    state.status = statusSelect.value;
    state.customer_id = customerInput.value.trim();
    ctx.replaceQuery({
      status: state.status || null,
      customer_id: state.customer_id || null,
    });
    void table.reload(queryOf(state));
  };

  statusSelect.addEventListener("change", apply);

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
    field({ label: "Status", control: statusSelect, id: "order-filter-status" }),
    field({ label: "Customer", control: customerInput, id: "order-filter-customer", hint: "Exact customer id" }),
    h("div", { class: "money-filters-actions" }, btn("Apply", { type: "submit", icon: "filter", size: "sm" })),
  );

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Orders",
      sub: "Checkout intents. An order becomes an invoice once the customer pays; an order stuck in awaiting payment is a checkout that never finished.",
      actions: h(
        "div",
        { class: "row" },
        ctx.can("invoice:write:all")
          ? btn("New order", { href: "/orders/new", icon: "plus", size: "sm", variant: "primary" })
          : null,
        btn("Catalog", { href: "/catalog-management", icon: "box", size: "sm" }),
      ),
    }),
    card({ flush: true, tight: true, body: filters }),
    table.el,
  );
}

function queryOf(state: Filters): Record<string, string | undefined> {
  return {
    status: state.status || undefined,
    customer_id: state.customer_id || undefined,
  };
}

function denied(): HTMLElement {
  return h(
    "div",
    { class: "page" },
    pageHead({ title: "Orders" }),
    emptyState({
      title: "Orders need the invoice read permission",
      body: "The order list shares invoice:read:all with the invoices it produces, because an order is only interesting for the invoice behind it.",
      icon: "lock",
      action: btn("Open invoices", { href: "/invoices", icon: "invoice", size: "sm" }),
    }),
  );
}
