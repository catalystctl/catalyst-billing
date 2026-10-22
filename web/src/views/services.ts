// The services list.
//
// One row per provisioned service: what it is, whose it is, what state it is in, and
// whether the queue is holding something for it. The filters are the three questions an
// operator actually asks ("what is suspended", "what is stuck", "what belongs to this
// customer"), not a generic query builder — the server accepts exactly these parameters and
// nothing else, so the UI offers exactly these.
//
// `open_operations` is the column that earns the screen: a service with something in flight
// is the one that needs a person, and it is coloured accordingly.

import { h } from "../lib/dom";
import { dateTime, relative, shortId, truncate } from "../lib/format";
import type { Ctx } from "../lib/types";
import { badge, btn, cellMain, errorState, field, input, mono, pageHead, select, statusBadge } from "../ui/kit";
import { createTable, type Column, type Table } from "../ui/table";

export const meta = { title: "Services", crumb: "Services" };

interface Service {
  id: string;
  customer: { id: string; email: string };
  product: { id: string; name: string };
  status: string;
  external_id: string;
  generation: number;
  next_bill_at: string | null;
  created_at: string;
  open_operations: number;
  last_error: string | null;
}

const STATUS_OPTIONS = [
  { value: "", label: "Any status" },
  { value: "active", label: "Active" },
  { value: "suspended", label: "Suspended" },
  { value: "pending", label: "Pending" },
  { value: "provisioning", label: "Provisioning" },
  { value: "terminating", label: "Terminating" },
  { value: "terminated", label: "Terminated" },
  { value: "error", label: "Error" },
];

const OPEN_OPTIONS = [
  { value: "", label: "Any queue state" },
  { value: "true", label: "Something in flight" },
  { value: "false", label: "Nothing in flight" },
];

/** "error" is missing from the shared tone map, and it is the one status an operator must
 *  not miss, so it is coloured here instead of rendering neutral. */
function serviceStatusBadge(status: string): HTMLElement {
  if (status === "error") return badge("Error", "danger", { dot: true });
  return statusBadge(status, { dot: true });
}

export default async function servicesPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("service:read:all")) {
    return h(
      "div",
      { class: "page" },
      errorState(
        new Error("Your account cannot read services. Ask an administrator for the service:read:all permission."),
      ),
    );
  }

  // Filters live in the URL, so a filtered list is a link somebody can send a colleague.
  const params = ctx.route.query;
  const filters = {
    status: params.get("status") ?? "",
    product_id: params.get("product_id") ?? "",
    customer_id: params.get("customer_id") ?? "",
    has_open_operation: params.get("has_open_operation") ?? "",
  };

  const buildQuery = (): Record<string, string | undefined> => ({
    status: filters.status || undefined,
    product_id: filters.product_id || undefined,
    customer_id: filters.customer_id || undefined,
    has_open_operation: filters.has_open_operation || undefined,
  });

  const hasFilters = (): boolean =>
    Boolean(filters.status || filters.product_id || filters.customer_id || filters.has_open_operation);

  let table: Table<Service>;
  let statusSelect: HTMLSelectElement;
  let openSelect: HTMLSelectElement;
  let productInput: HTMLInputElement;
  let customerInput: HTMLInputElement;
  const clearButton = btn("Clear", {
    size: "sm",
    variant: "ghost",
    icon: "x",
    onclick: () => clearFilters(),
  });
  clearButton.hidden = !hasFilters();

  const apply = () => {
    ctx.replaceQuery({
      status: filters.status || null,
      product_id: filters.product_id || null,
      customer_id: filters.customer_id || null,
      has_open_operation: filters.has_open_operation || null,
    });
    clearButton.hidden = !hasFilters();
    void table.reload(buildQuery());
  };

  const clearFilters = () => {
    filters.status = "";
    filters.product_id = "";
    filters.customer_id = "";
    filters.has_open_operation = "";
    statusSelect.value = "";
    openSelect.value = "";
    productInput.value = "";
    customerInput.value = "";
    apply();
  };

  const columns: Column<Service>[] = [
    {
      key: "service",
      label: "Service",
      primary: true,
      render: (row) =>
        cellMain(
          h("a", { class: "row-title", href: `/services/${row.id}` }, row.external_id || row.id),
          row.external_id
            ? mono(shortId(row.id, 12), row.id)
            : h("span", { class: "faint" }, "no external id"),
        ),
    },
    {
      key: "customer",
      label: "Customer",
      render: (row) => h("a", { class: "linkish", href: `/customers/${row.customer.id}` }, row.customer.email),
    },
    {
      key: "product",
      label: "Product",
      render: (row) => h("a", { class: "linkish", href: `/catalog-management/${row.product.id}` }, row.product.name),
    },
    {
      key: "status",
      label: "Status",
      render: (row) => serviceStatusBadge(row.status),
    },
    {
      key: "generation",
      label: "Generation",
      align: "right",
      className: "mono",
      render: (row) => mono(row.generation),
    },
    {
      key: "next_bill_at",
      label: "Next bill",
      render: (row) =>
        row.next_bill_at
          ? h("span", { class: "dim", title: dateTime(row.next_bill_at) }, relative(row.next_bill_at))
          : h("span", { class: "faint" }, "—"),
    },
    {
      key: "open_operations",
      label: "Open ops",
      align: "right",
      render: (row) =>
        badge(String(row.open_operations), row.open_operations > 0 ? "danger" : "neutral", {
          dot: row.open_operations > 0,
        }),
    },
    {
      key: "last_error",
      label: "Last error",
      hideSmall: true,
      render: (row) =>
        row.last_error
          ? h("span", { class: "mono svc-truncate", title: row.last_error }, truncate(row.last_error, 44))
          : h("span", { class: "faint" }, "—"),
    },
  ];

  table = createTable<Service>(ctx, {
    path: "/api/v1/staff/services",
    columns,
    query: buildQuery(),
    rowKey: (row) => row.id,
    rowHref: (row) => `/services/${row.id}`,
    empty: {
      title: hasFilters() ? "No services match those filters" : "No services yet",
      body: hasFilters()
        ? "Nothing in the book matches this combination. Widen the status, or clear the id filters."
        : "A service appears here once an order is provisioned or a subscription is created for a customer.",
      action: hasFilters()
        ? btn("Clear filters", { size: "sm", icon: "x", onclick: () => clearFilters() })
        : btn("Browse the catalog", { size: "sm", icon: "box", href: "/catalog-management" }),
    },
  });

  statusSelect = select(STATUS_OPTIONS, {
    id: "filter-status",
    value: filters.status,
    onchange: (event) => {
      filters.status = (event.target as HTMLSelectElement).value;
      apply();
    },
  });

  openSelect = select(OPEN_OPTIONS, {
    id: "filter-open",
    value: filters.has_open_operation,
    onchange: (event) => {
      filters.has_open_operation = (event.target as HTMLSelectElement).value;
      apply();
    },
  });

  productInput = input({
    id: "filter-product",
    class: "input--mono input--sm",
    placeholder: "product uuid",
    value: filters.product_id,
    autocomplete: "off",
    onchange: (event) => {
      filters.product_id = (event.target as HTMLInputElement).value.trim();
      apply();
    },
  });

  customerInput = input({
    id: "filter-customer",
    class: "input--mono input--sm",
    placeholder: "customer uuid",
    value: filters.customer_id,
    autocomplete: "off",
    onchange: (event) => {
      filters.customer_id = (event.target as HTMLInputElement).value.trim();
      apply();
    },
  });

  const toolbar = h(
    "div",
    { class: "svc-filters" },
    field({ label: "Status", control: statusSelect }),
    field({ label: "Queue", control: openSelect }),
    field({ label: "Product", control: productInput }),
    field({ label: "Customer", control: customerInput }),
    h(
      "div",
      { class: "toolbar-end" },
      clearButton,
      btn("Refresh", { size: "sm", icon: "refresh", onclick: () => void table.reload() }),
    ),
  );

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Services",
      sub: "Every provisioned service. A row with an open operation is one the queue is still working on.",
      actions: btn("Operations queue", { size: "sm", icon: "activity", href: "/operations" }),
    }),
    toolbar,
    table.el,
  );
}
