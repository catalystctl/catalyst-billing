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
//
// "New service" is the operator's own path to adding one: a phone order, a migration, a service
// added to an account that already exists. It is deliberately not a checkout — it raises no
// invoice — and the dialog says what it does and does not do, because an operator who assumes a
// bill went out is the failure mode this button has to avoid. What it does create is the
// subscription the renewal engine will bill from the next period, so the response reports the
// first bill date, or says that nothing will ever bill the service.

import { h } from "../lib/dom";
import type { Page } from "../lib/api";
import { dateTime, money, relative, shortId, truncate } from "../lib/format";
import type { Amount } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { bindForm, openDialog } from "../ui/dialog";
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

/** The CSV export path for a set of list filters.
 *
 *  It is built from the same filter object `buildQuery` sends, so the file cannot contain a
 *  different set of services than the screen: one filter vocabulary, two renderings of it. */
function exportHref(query: Record<string, string | undefined>): string {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value !== undefined && value !== "") params.set(key, value);
  }
  const search = params.toString();
  return `/api/v1/staff/exports/services.csv${search ? `?${search}` : ""}`;
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

  // Adding a service is service:write:all. A caller without it sees the list and no button, rather
  // than a button that answers 403.
  const canCreate = ctx.can("service:write:all");

  /** Reports a failure with the server's own sentence when it sent one. */
  const reportError = (err: unknown, fallback: string): void => {
    const detail = (err as { detail?: string }).detail || (err instanceof Error ? err.message : String(err));
    ctx.toast(fallback, { tone: "danger", detail });
  };

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

  // The export is a link with `download`, not a fetch: the server streams text/csv with
  // Content-Disposition, so the browser saves it and no page of the table passes through
  // JavaScript. The href carries the same filters the list sends.
  const exportLink = h(
    "a",
    {
      class: "btn btn--sm",
      href: exportHref(filters),
      download: "",
      title: "Download every service matching these filters as CSV",
    },
    icon("download", 14),
    "Export CSV",
  );

  const apply = () => {
    ctx.replaceQuery({
      status: filters.status || null,
      product_id: filters.product_id || null,
      customer_id: filters.customer_id || null,
      has_open_operation: filters.has_open_operation || null,
    });
    exportLink.href = exportHref(filters);
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
        : h(
            "div",
            { class: "row-wrap" },
            canCreate
              ? btn("New service", { size: "sm", variant: "primary", icon: "plus", onclick: () => void newServiceDialog() })
              : null,
            btn("Browse the catalog", { size: "sm", icon: "box", href: "/catalog-management" }),
          ),
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

  // ---------------------------------------------------------------- dialog --

  /** Creates a service for an existing customer.
   *
   *  The dialog loads its choices instead of asking for identifiers: a customer is picked from the
   *  customer list (prefilled from the filter, because an operator usually arrives having found the
   *  person first), a price from the product's plans, and the config options from the product. The
   *  reason is required by the server and by the form, because a service with no order behind it is
   *  the record an auditor asks about.
   */
  const newServiceDialog = async (): Promise<void> => {
    const customer = select([{ value: "", label: "Loading customers…" }], { id: "svc-customer" });
    const product = select([{ value: "", label: "Loading products…" }], { id: "svc-product" });
    const price = select([{ value: "", label: "Choose a product first" }], { id: "svc-price" });
    const quantity = input({ id: "svc-quantity", type: "number", value: "1", min: 1, max: 1000, step: 1 });
    const reason = input({
      id: "svc-reason",
      placeholder: "Phone order, paid by bank transfer",
      maxlength: 500,
      required: true,
    });
    const compBox = h("input", { type: "checkbox" });
    const configHost = h("div", { class: "stack-sm" });
    const errorHost = h("div");

    let products: CatalogProduct[] = [];

    const renderConfig = (options: ConfigOptionRow[]): void => {
      configHost.replaceChildren();
      const visible = options.filter((option) => !option.hidden);
      if (visible.length === 0) {
        configHost.append(h("p", { class: "field-hint" }, "This product has no configurable options."));
        return;
      }
      for (const option of visible) {
        let control: HTMLElement;
        if (option.type === "dropdown" || option.type === "radio") {
          control = select(
            [{ value: "", label: "— choose —" }, ...(option.choices ?? []).map((choice) => ({ value: choice, label: choice }))],
            { id: `cfg-${option.id}` },
          );
          control.dataset.option = option.id;
        } else if (option.type === "checkbox") {
          const box = h("input", { type: "checkbox" });
          box.dataset.option = option.id;
          control = h("label", { class: "checkbox" }, box, h("span", null, "Enabled"));
        } else {
          control = input({
            id: `cfg-${option.id}`,
            placeholder: option.type === "number" ? "0" : "value",
          });
          control.dataset.option = option.id;
        }
        configHost.append(
          field({
            label: option.name,
            control,
            id: `cfg-${option.id}`,
            required: option.required,
            hint: option.description || (option.required ? "Required by this product." : undefined),
          }),
        );
      }
    };

    const loadOptions = async (productID: string): Promise<void> => {
      configHost.replaceChildren(h("p", { class: "field-hint" }, "Loading options…"));
      try {
        const res = await ctx.api.get<{ data: ConfigOptionRow[] }>("/api/v1/staff/config-options", {
          query: { product_id: productID },
        });
        renderConfig(res.data ?? []);
      } catch (err) {
        configHost.replaceChildren(
          h("p", { class: "field-hint" }, "The product's options could not be loaded, so the service cannot be created from here right now."),
        );
        reportError(err, "The config options could not be loaded");
      }
    };

    const fillPrices = (productID: string): void => {
      const chosen = products.find((item) => item.id === productID);
      const plans = chosen?.prices ?? chosen?.price_versions ?? [];
      price.replaceChildren();
      if (plans.length === 0) {
        price.append(h("option", { value: "" }, "This product has no prices"));
        return;
      }
      for (const plan of plans) {
        const label = `${plan.name || `version ${plan.version}`} — ${money(plan.amount)} / ${plan.interval_label || plan.interval}`;
        price.append(h("option", { value: plan.id }, label));
      }
      void loadOptions(productID);
    };

    product.addEventListener("change", () => fillPrices(product.value));

    const form = h(
      "form",
      { class: "stack", novalidate: true },
      errorHost,
      h(
        "div",
        { class: "form-grid" },
        field({
          label: "Customer",
          control: customer,
          id: "svc-customer",
          required: true,
          class: "span-2",
          hint: "The 100 most recent customers. Filter the list first to find someone else.",
        }),
        field({ label: "Product", control: product, id: "svc-product", required: true }),
        field({
          label: "Plan",
          control: price,
          id: "svc-price",
          required: true,
          hint: "The price version the service is on. A later catalog change adds a version; it never edits this one.",
        }),
        field({ label: "Quantity", control: quantity, id: "svc-quantity" }),
        field({
          label: "Reason",
          control: reason,
          id: "svc-reason",
          required: true,
          class: "span-2",
          hint: "Recorded on the service and in the audit log. Required, because nothing else explains where this service came from.",
        }),
        h(
          "div",
          { class: "span-2" },
          h(
            "label",
            { class: "switch" },
            compBox,
            "Comped — create it with no subscription, so nothing ever bills it",
          ),
        ),
      ),
      configHost,
    );

    const submit = btn("Create service", { variant: "primary", type: "submit" });
    const cancel = btn("Cancel", { variant: "ghost" });
    bindForm(form, submit);

    const handle = openDialog({
      title: "New service",
      subtitle:
        "Adds the service, queues it for provisioning, and starts the billing schedule. It raises no invoice — to bill for it, issue an invoice with this product on it.",
      body: h("div", { class: "stack" }, form),
      footer: h("div", { class: "row spacer" }, cancel, submit),
      wide: true,
      locked: () => submit instanceof HTMLButtonElement && submit.disabled,
    });
    cancel.addEventListener("click", () => handle.close());

    // Load the choices. The dialog is already open, so a failure is reported inside it rather than
    // as a toast over a form the operator cannot use.
    void (async () => {
      try {
        const [customers, catalog] = await Promise.all([
          ctx.api.get<Page<CustomerOption>>("/api/v1/staff/customers", { query: { limit: 100 } }),
          ctx.api.get<{ data: CatalogProduct[] }>("/api/v1/staff/catalog/products"),
        ]);
        customer.replaceChildren(h("option", { value: "" }, "— choose a customer —"));
        for (const row of customers.data ?? []) {
          customer.append(h("option", { value: row.id }, row.name ? `${row.email} — ${row.name}` : row.email));
        }
        if (filters.customer_id) customer.value = filters.customer_id;

        products = catalog.data ?? [];
        product.replaceChildren(h("option", { value: "" }, "— choose a product —"));
        for (const item of products) {
          product.append(h("option", { value: item.id }, item.name));
        }
        if (filters.product_id && products.some((item) => item.id === filters.product_id)) {
          product.value = filters.product_id;
          fillPrices(product.value);
        }
      } catch (err) {
        errorHost.replaceChildren(
          h(
            "div",
            { class: "banner banner--danger" },
            "The customer and product lists could not be loaded, so the service cannot be created from here right now.",
          ),
        );
        reportError(err, "The lists could not be loaded");
      }
    })();

    const collectConfig = (): Record<string, string> => {
      const out: Record<string, string> = {};
      configHost.querySelectorAll<HTMLElement>("[data-option]").forEach((el) => {
        const id = el.dataset.option ?? "";
        const value =
          el instanceof HTMLInputElement && el.type === "checkbox"
            ? el.checked
              ? "on"
              : ""
            : (el as HTMLInputElement | HTMLSelectElement).value;
        if (id && value.trim() !== "") out[id] = value.trim();
      });
      return out;
    };

    form.addEventListener("submit", (event) => {
      event.preventDefault();
      void (async () => {
        errorHost.replaceChildren();
        if (!customer.value) {
          errorHost.replaceChildren(h("div", { class: "banner banner--danger" }, "Choose the customer this service belongs to."));
          return;
        }
        if (!price.value) {
          errorHost.replaceChildren(h("div", { class: "banner banner--danger" }, "Choose the plan this service is on."));
          return;
        }
        if (!reason.value.trim()) {
          errorHost.replaceChildren(
            h("div", { class: "banner banner--danger" }, "Say why this service is being added. It is the only record of where it came from."),
          );
          return;
        }

        const payload = {
          customer_id: customer.value,
          price_version_id: price.value,
          quantity: Number(quantity.value) || 1,
          config: collectConfig(),
          reason: reason.value.trim(),
          comp: compBox.checked,
        };

        if (submit instanceof HTMLButtonElement) submit.disabled = true;
        try {
          const created = await ctx.api.post<ServiceCreateResult>("/api/v1/staff/services", payload);
          const detail = created.comped
            ? "Comped: no subscription was created, so nothing will ever invoice it."
            : created.subscription
              ? `First bill on ${dateTime(created.subscription.next_billing_at)}. No invoice was raised now.`
              : "A one-time price: nothing renews it and no invoice was raised.";
          ctx.toast("Service created", { tone: "success", detail });
          handle.close();
          void table.reload(buildQuery());
        } catch (err) {
          errorHost.replaceChildren(h("div", { class: "banner banner--danger" }, serviceError(err)));
        } finally {
          if (submit instanceof HTMLButtonElement) submit.disabled = false;
        }
      })();
    });
  };

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Services",
      sub: "Every provisioned service. A row with an open operation is one the queue is still working on.",
      actions: h(
        "div",
        { class: "row" },
        exportLink,
        btn("Operations queue", { size: "sm", icon: "activity", href: "/operations" }),
        canCreate
          ? btn("New service", { size: "sm", variant: "primary", icon: "plus", onclick: () => void newServiceDialog() })
          : null,
      ),
    }),
    toolbar,
    table.el,
  );
}

// --------------------------------------------------------------- the dialog --

/** A row of the staff customer list, as the picker needs it. */
interface CustomerOption {
  id: string;
  email: string;
  name: string;
}

/** One price version, as the catalog endpoints render it. `amount` is the money shape: integer
 *  minor units plus an explicit currency, never a float. */
interface CatalogPrice {
  id: string;
  name: string;
  version: number;
  currency: string;
  amount: Amount;
  interval: string;
  interval_label: string;
  recurring: boolean;
}

interface CatalogProduct {
  id: string;
  name: string;
  /** The API names the plan list twice; `prices` is the documented one. */
  prices?: CatalogPrice[];
  price_versions?: CatalogPrice[];
}

/** A config option as the staff config-option endpoint returns it. */
interface ConfigOptionRow {
  id: string;
  name: string;
  description: string;
  type: string;
  choices: string[] | null;
  required: boolean;
  hidden: boolean;
}

/** What POST /api/v1/staff/services answers. */
interface ServiceCreateResult {
  service: { id: string; status: string; origin: string };
  subscription: { id: string; next_billing_at: string } | null;
  invoiced: boolean;
  comped: boolean;
}

/** Maps the server's stable codes to something an operator can act on. The code is what a client
 *  branches on; the sentence is what a person reads. */
function serviceError(err: unknown): string {
  const code = (err as { code?: string }).code ?? "";
  switch (code) {
    case "not_found":
      return "That customer or plan no longer exists. Reload the list and try again.";
    case "reason_required":
      return "Say why this service is being added.";
    case "out_of_stock":
      return "This product has no stock left. Restock it in the catalog, or the service cannot be given.";
    case "per_user_limit":
      return "The customer already holds the maximum number of this product.";
    case "quantity_not_allowed":
      return "This product is sold one per customer, so the quantity must be 1.";
    case "invalid_quantity":
      return "The quantity must be between 1 and 1000.";
    case "invalid_config":
      return (err as { detail?: string }).detail || "One of the selected options is not available for this plan.";
    case "invalid_schedule":
      return "This price cannot be turned into a billing period, so it needs a different plan.";
    default:
      return (err as { detail?: string }).detail || "The service could not be created.";
  }
}
