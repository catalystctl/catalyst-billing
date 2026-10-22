// One order.
//
// An order has a small state machine and the page shows it, because "why is this customer
// still not provisioned" is answered by the step the order stopped at. Items are the priced
// snapshot (they name the price version they came from), invoices are the money, and the
// attempts are what the gateway said.

import { h } from "../lib/dom";
import type { Child } from "../lib/dom";
import type { Amount } from "../lib/format";
import { dateTime, formatMinor, humanStatus, relative, shortId } from "../lib/format";
import type { Ctx } from "../lib/types";
import {
  badge,
  btn,
  card,
  code,
  copyButton,
  emptyState,
  moneyEl,
  pageHead,
  resource,
  skeletonPage,
  statusBadge,
} from "../ui/kit";

export const meta = { title: "Order", crumb: "Order" };

interface OrderCustomer {
  id: string;
  email: string;
}

interface Order {
  id: string;
  customer: OrderCustomer;
  status: string;
  currency: string;
  total: Amount;
  created_at: string;
  updated_at: string;
  invoice_id: string | null;
}

interface OrderItem {
  id: string;
  description: string;
  quantity: number;
  unit_minor: number;
  line_minor: number;
  setup_minor: number;
  price_version_id: string;
  provision_plan: string;
  provision_region: string;
}

interface Invoice {
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
  customer?: OrderCustomer;
}

interface Attempt {
  id: string;
  gateway: string;
  status: string;
  provider_ref: string | null;
  amount: Amount;
  created_at: string;
}

interface OrderDetail {
  order: Order;
  items: OrderItem[];
  invoices: Invoice[];
  attempts: Attempt[];
}

/** The happy path. Cancelled and needs_review sit off it. */
const FLOW = ["pending_payment", "paid", "fulfilling", "completed"] as const;

export default async function orderPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("invoice:read:all")) return denied();

  const id = ctx.route.params.id ?? "";

  return resource(
    ctx,
    (signal) => ctx.api.get<OrderDetail>(`/api/v1/staff/orders/${id}`, { cache: false, signal }),
    (detail) => render(ctx, detail),
    { skeleton: skeletonPage },
  );
}

function render(ctx: Ctx, detail: OrderDetail): Node {
  const { order } = detail;
  const customer = order.customer;

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: h("span", { class: "mono" }, `Order ${shortId(order.id, 12)}`),
      sub: h(
        "span",
        { class: "row" },
        customer?.id
          ? h("a", { class: "linkish", href: `/customers/${customer.id}` }, customer.email || shortId(customer.id))
          : h("span", { class: "faint" }, "unknown customer"),
        h("span", { class: "faint" }, "·"),
        h("span", { class: "dim", title: dateTime(order.created_at) }, `created ${relative(order.created_at)}`),
      ),
      badge: statusBadge(order.status),
      actions: order.invoice_id
        ? btn("Invoice", { href: `/invoices/${order.invoice_id}`, icon: "invoice", size: "sm" })
        : null,
    }),
    h(
      "section",
      { class: "card" },
      h(
        "div",
        { class: "card-body pay-head" },
        h(
          "div",
          { class: "pay-hero" },
          h("span", { class: "pay-label" }, "Order total"),
          moneyEl(order.total, { size: "xl" }),
          h(
            "span",
            { class: "pay-sub" },
            h("span", { class: "faint" }, "Currency "),
            h("span", { class: "mono" }, order.currency),
          ),
        ),
        h(
          "div",
          { class: "pay-side" },
          h("span", { class: "pay-label" }, "Order id"),
          h("div", { class: "pay-id" }, code(order.id), copyButton(ctx, order.id, "order id")),
          h(
            "span",
            { class: "pay-when" },
            h("span", { class: "faint" }, "Updated "),
            h("span", { title: dateTime(order.updated_at) }, `${dateTime(order.updated_at)} (${relative(order.updated_at)})`),
          ),
        ),
      ),
    ),
    card({
      title: "Status",
      sub: "Where this order sits on the checkout-to-provision path",
      body: statusFlow(order.status),
    }),
    card({
      title: "Items",
      sub: `${detail.items.length} line${detail.items.length === 1 ? "" : "s"}, priced from the catalog`,
      flush: true,
      body:
        detail.items.length === 0
          ? note("This order has no items. That is a data problem, not a UI state — the total above cannot be explained by anything.")
          : dataTable<OrderItem>(
              [
                {
                  label: "Item",
                  primary: true,
                  render: (row) =>
                    h(
                      "div",
                      { class: "cell-main" },
                      h("div", { class: "primary" }, row.description || "—"),
                      h(
                        "div",
                        { class: "secondary" },
                        [
                          row.provision_plan || "no plan",
                          row.provision_region || "no region",
                          `price ${shortId(row.price_version_id)}`,
                        ].join(" · "),
                      ),
                    ),
                },
                { label: "Qty", align: "right", render: (row) => h("span", { class: "mono" }, String(row.quantity)) },
                { label: "Unit", align: "right", render: (row) => moneyEl(amountOf(row.unit_minor, order.total)) },
                { label: "Line", align: "right", render: (row) => moneyEl(amountOf(row.line_minor, order.total)) },
                {
                  label: "Setup",
                  align: "right",
                  render: (row) =>
                    row.setup_minor === 0 ? h("span", { class: "faint" }, "—") : moneyEl(amountOf(row.setup_minor, order.total)),
                },
              ],
              detail.items,
            ),
    }),
    h(
      "div",
      { class: "money-halves" },
      card({
        title: "Invoices",
        sub: `${detail.invoices.length} raised`,
        flush: true,
        body:
          detail.invoices.length === 0
            ? note("No invoice has been raised for this order yet. A paid order without an invoice is worth chasing: nothing will collect the money.")
            : dataTable<Invoice>(
                [
                  {
                    label: "Invoice",
                    primary: true,
                    render: (row) =>
                      h("a", { class: "row-title", href: `/invoices/${row.id}` }, row.number || shortId(row.id)),
                  },
                  { label: "Status", render: (row) => statusBadge(row.overdue ? "overdue" : row.status) },
                  { label: "Total", align: "right", render: (row) => moneyEl(row.total) },
                  {
                    label: "Outstanding",
                    align: "right",
                    render: (row) => (row.due.minor > 0 ? moneyEl(row.due) : h("span", { class: "faint" }, "Settled")),
                  },
                  {
                    label: "Due",
                    render: (row) =>
                      row.due_at ? h("span", { class: "dim", title: dateTime(row.due_at) }, relative(row.due_at)) : h("span", { class: "faint" }, "—"),
                  },
                ],
                detail.invoices,
              ),
      }),
      card({
        title: "Payment attempts",
        sub: `${detail.attempts.length} recorded`,
        flush: true,
        body:
          detail.attempts.length === 0
            ? note("The gateway has not been asked to collect this order. A checkout that never started leaves no attempt behind.")
            : dataTable<Attempt>(
                [
                  { label: "Gateway", primary: true, render: (row) => h("span", { class: "mono" }, row.gateway) },
                  { label: "Status", render: (row) => statusBadge(row.status) },
                  {
                    label: "Provider ref",
                    render: (row) =>
                      row.provider_ref ? h("span", { class: "mono truncate" }, row.provider_ref) : h("span", { class: "faint" }, "—"),
                  },
                  { label: "Amount", align: "right", render: (row) => moneyEl(row.amount) },
                  {
                    label: "Created",
                    render: (row) => h("span", { class: "dim", title: dateTime(row.created_at) }, relative(row.created_at)),
                  },
                ],
                detail.attempts,
              ),
      }),
    ),
  );
}

/** The order state machine, drawn. The current step is the one that explains the delay. */
function statusFlow(status: string): HTMLElement {
  const index = (FLOW as readonly string[]).indexOf(status);
  const offPath = index === -1;

  const parts: Child[] = [];
  FLOW.forEach((step, i) => {
    const state = offPath ? "todo" : i < index ? "done" : i === index ? "current" : "todo";
    if (i > 0) parts.push(h("span", { class: "flow-arrow", "aria-hidden": "true" }, "→"));
    parts.push(
      h(
        "div",
        { class: `flow-step is-${state}` },
        h("span", { class: "flow-dot" }),
        h("span", { class: "flow-name" }, humanStatus(step)),
      ),
    );
  });

  return h(
    "div",
    { class: "stack-sm" },
    h("div", { class: "flow" }, ...parts),
    offPath
      ? h(
          "div",
          { class: "row" },
          badge(humanStatus(status), status === "cancelled" ? "neutral" : "warning", { dot: true }),
          h("span", { class: "faint" }, "This order left the happy path before completing."),
        )
      : null,
  );
}

interface SimpleColumn<T> {
  label: string;
  align?: "right";
  primary?: boolean;
  render: (row: T) => Child;
}

function dataTable<T>(columns: Array<SimpleColumn<T>>, rows: T[]): HTMLElement {
  const thead = h(
    "thead",
    null,
    h(
      "tr",
      null,
      ...columns.map((col) => h("th", { class: col.align === "right" ? "num" : undefined, scope: "col" }, col.label)),
    ),
  );
  const tbody = h("tbody");
  for (const row of rows) {
    tbody.appendChild(
      h(
        "tr",
        null,
        ...columns.map((col) =>
          h(
            "td",
            {
              class: [col.align === "right" ? "num" : "", col.primary ? "cell-primary" : ""].filter(Boolean).join(" "),
              "data-label": col.primary ? undefined : col.label,
            },
            col.render(row),
          ),
        ),
      ),
    );
  }
  return h("div", { class: "table-wrap table-wrap--cards" }, h("table", { class: "table" }, thead, tbody));
}

/** The order item amounts are bare minor units; the order total carries the currency and
 *  the scale, so it is the reference that keeps the rendering exact. */
function amountOf(minor: number, ref: Amount): Amount {
  return {
    minor,
    currency: ref.currency,
    scale: ref.scale,
    display: `${ref.currency} ${formatMinor(minor, ref.scale)}`,
  };
}

function note(text: string): HTMLElement {
  return h("div", { class: "card-body" }, h("p", { class: "empty-body" }, text));
}

function denied(): HTMLElement {
  return h(
    "div",
    { class: "page" },
    pageHead({ title: "Order" }),
    emptyState({
      title: "Orders need the invoice read permission",
      body: "Reading an order requires invoice:read:all, because the order is only meaningful next to the invoice it produces.",
      icon: "lock",
      action: btn("Open invoices", { href: "/invoices", icon: "invoice", size: "sm" }),
    }),
  );
}
