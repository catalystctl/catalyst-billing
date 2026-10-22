// Creating an order: buy a catalog price for a billing account.
//
// Two decisions shape this screen:
//
//  - The price is chosen from the catalog, never typed. The API takes a price version id
//    and reads the amount from it, so the client cannot propose a price — the select below
//    is a picker, not an input.
//  - The customer is optional. Leaving it unset bills the acting staff member: a staff user
//    who is the billing account should not have to link an account to an account before
//    they can buy something. Choosing someone else is the "sell to this customer" case.

import { ApiError } from "../lib/api";
import { debounce, h } from "../lib/dom";
import { moneyWithCode, type Amount } from "../lib/format";
import type { Ctx } from "../lib/types";
import {
  banner,
  btn,
  card,
  emptyState,
  field,
  input,
  pageHead,
  searchInput,
  select,
} from "../ui/kit";

export const meta = { title: "New order", crumb: "New order" };

interface Price {
  id: string;
  version: number;
  name: string;
  currency: string;
  amount: Amount;
  setup: Amount;
  interval: string;
}

interface Product {
  id: string;
  name: string;
  slug: string;
  hidden: boolean;
  prices: Price[];
}

interface CustomerHit {
  id: string;
  email: string;
  name: string;
}

interface CreatedOrder {
  id: string;
  invoice_id: string;
  number: string;
  status: string;
}

const INTERVAL_LABEL: Record<string, string> = {
  monthly: "month",
  quarterly: "quarter",
  semiannual: "6 months",
  annual: "year",
};

export default async function orderNewPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("invoice:write:all")) return denied();

  let products: Product[] = [];
  let selectedCustomer: CustomerHit | null = null;
  let searchController: AbortController | null = null;

  const errorHost = h("div", { class: "stack-sm" });

  // ---------------------------------------------------------------- customer --
  const customerHost = h("div", { class: "stack-sm" });
  const resultsHost = h("div", { class: "search-results" });
  const resultsCard = h("div", { class: "card", hidden: true }, resultsHost);

  const renderCustomer = () => {
    if (selectedCustomer) {
      customerHost.replaceChildren(
        h(
          "div",
          { class: "inv-picked" },
          h(
            "div",
            { class: "cell-main" },
            h("div", { class: "primary" }, selectedCustomer.name || selectedCustomer.email),
            h("div", { class: "secondary" }, selectedCustomer.email),
          ),
          btn("Change", {
            size: "sm",
            variant: "ghost",
            onclick: () => {
              selectedCustomer = null;
              renderCustomer();
            },
          }),
        ),
      );
      return;
    }
    customerHost.replaceChildren(
      h(
        "div",
        { class: "stack-sm" },
        h(
          "p",
          { class: "field-hint" },
          "Leave this empty to bill yourself. Your own billing account is created from your login on the first order — no separate account to link.",
        ),
        searchInput({
          placeholder: "Search by email or name to bill someone else",
          oninput: (value) => void search(value),
        }),
        resultsCard,
      ),
    );
  };

  const search = debounce(async (raw: string) => {
    const q = raw.trim();
    if (q.length < 2) {
      resultsCard.hidden = true;
      return;
    }
    searchController?.abort();
    searchController = new AbortController();
    try {
      const page = await ctx.api.get<{ data: CustomerHit[] }>(
        `/api/v1/staff/customers?q=${encodeURIComponent(q)}&limit=8`,
        { cache: false, signal: searchController.signal },
      );
      const hits = page.data ?? [];
      resultsCard.hidden = false;
      if (hits.length === 0) {
        resultsHost.replaceChildren(h("p", { class: "field-hint" }, "No customer matches that search."));
        return;
      }
      resultsHost.replaceChildren(
        ...hits.map((hit) =>
          h(
            "button",
            {
              type: "button",
              class: "search-hit",
              onclick: () => {
                selectedCustomer = hit;
                resultsCard.hidden = true;
                renderCustomer();
              },
            },
            h(
              "div",
              { class: "search-hit-body" },
              h("div", { class: "search-hit-title" }, hit.name || hit.email),
              h("div", { class: "search-hit-sub" }, hit.name ? hit.email : "No display name"),
            ),
          ),
        ),
      );
    } catch {
      resultsCard.hidden = true;
    }
  }, 220);

  ctx.onDispose(() => searchController?.abort());

  // ---------------------------------------------------------------- product --
  const productSelect = select([], { id: "order-product" });
  const priceSelect = select([], { id: "order-price" });
  const quantity = input({ id: "order-quantity", type: "number", min: 1, max: 1000, value: 1 });

  const paintPrices = () => {
    const product = products.find((p) => p.id === productSelect.value);
    const prices = product?.prices ?? [];
    priceSelect.replaceChildren(
      ...prices.map((price) =>
        h(
          "option",
          { value: price.id },
          `${price.name || `Version ${price.version}`} — ${moneyWithCode(price.amount)} / ${
            INTERVAL_LABEL[price.interval] ?? price.interval
          }${price.setup.minor > 0 ? ` + ${moneyWithCode(price.setup)} setup` : ""}`,
        ),
      ),
    );
    priceSelect.disabled = prices.length === 0;
  };

  productSelect.addEventListener("change", paintPrices);

  // --------------------------------------------------------------- the page --
  const submit = btn("Create order", { type: "submit", variant: "primary", icon: "plus" });

  const submitOrder = async () => {
    const priceVersionID = priceSelect.value;
    if (!priceVersionID) {
      errorHost.replaceChildren(
        banner("danger", "No price selected", "Choose a product that has a price before creating the order."),
      );
      return;
    }
    const qty = Number.parseInt(quantity.value, 10);
    if (!Number.isFinite(qty) || qty < 1) {
      errorHost.replaceChildren(banner("danger", "Invalid quantity", "Quantity must be a whole number of at least 1."));
      return;
    }

    const body: Record<string, unknown> = { price_version_id: priceVersionID, quantity: qty };
    if (selectedCustomer) body.customer_id = selectedCustomer.id;

    if (submit instanceof HTMLButtonElement) submit.disabled = true;
    try {
      const created = await ctx.api.post<CreatedOrder>("/api/v1/staff/orders", body);
      ctx.toast("Order created", {
        tone: "success",
        detail: created.number ? `${created.number} · awaiting payment` : "Awaiting payment",
      });
      ctx.navigate(`/orders/${created.id}`);
    } catch (err) {
      const message =
        err instanceof ApiError
          ? err.detail ?? "The order could not be created."
          : "The server could not be reached. Nothing was created.";
      errorHost.replaceChildren(banner("danger", "Not created", message));
      if (submit instanceof HTMLButtonElement) submit.disabled = false;
    }
  };

  const form = h(
    "form",
    {
      class: "stack",
      novalidate: true,
      onsubmit: (event: Event) => {
        event.preventDefault();
        void submitOrder();
      },
    },
    errorHost,
    card({ title: "Billing account", sub: "Who is being billed", body: customerHost }),
    card({
      title: "Product",
      sub: "Priced from the catalog — the amount is never sent by this form",
      body: h(
        "div",
        { class: "stack" },
        field({ label: "Product", control: productSelect, id: "order-product" }),
        field({ label: "Price", control: priceSelect, id: "order-price", hint: "Prices are versioned; an issued invoice keeps the version it was sold at." }),
        field({ label: "Quantity", control: quantity, id: "order-quantity", hint: "1 to 1000" }),
      ),
    }),
    h("div", { class: "row" }, h("span", { class: "spacer" }), submit),
  );

  const page = h(
    "div",
    { class: "page" },
    h("div", { class: "row" }, btn("All orders", { href: "/orders", icon: "arrowLeft", size: "sm", variant: "ghost" })),
    pageHead({
      title: "New order",
      sub: "Buy a product for a billing account. The order issues an invoice; paying it provisions the service.",
    }),
    form,
  );

  // Load the catalog once. A product with no price is listed but cannot be ordered; the
  // server refuses it too, so the UI is a courtesy rather than the rule.
  try {
    const list = await ctx.api.get<{ data: Product[] }>("/api/v1/staff/catalog/products", { cache: false });
    products = (list.data ?? []).filter((p) => (p.prices ?? []).length > 0);
    if (products.length === 0) {
      form.replaceWith(
        emptyState({
          title: "Nothing is for sale yet",
          body: "Add a product and at least one price in the catalog, then come back.",
          icon: "box",
          action: btn("Open catalog", { href: "/catalog-management", icon: "box", size: "sm" }),
        }),
      );
      renderCustomer();
      return page;
    }
    productSelect.replaceChildren(
      ...products.map((p) => h("option", { value: p.id }, p.hidden ? `${p.name} (hidden)` : p.name)),
    );
    paintPrices();

    // Deep link from a product or price screen: /orders/new?price=<price_version_id>.
    const wanted = ctx.route.query.get("price");
    if (wanted) {
      for (const product of products) {
        if (product.prices.some((price) => price.id === wanted)) {
          productSelect.value = product.id;
          paintPrices();
          priceSelect.value = wanted;
          break;
        }
      }
    }
  } catch (err) {
    errorHost.replaceChildren(
      banner(
        "danger",
        "The catalog could not be loaded",
        err instanceof ApiError ? err.detail ?? "" : err instanceof Error ? err.message : "",
      ),
    );
  }

  // Deep link to bill a specific customer: /orders/new?customer=<id>.
  const customerParam = ctx.route.query.get("customer");
  if (customerParam) {
    try {
      const detail = await ctx.api.get<{ customer: CustomerHit }>(
        `/api/v1/staff/customers/${encodeURIComponent(customerParam)}`,
        { cache: false },
      );
      selectedCustomer = detail.customer;
    } catch {
      // Fall through to the picker: a stale link should not block the screen.
    }
  }

  renderCustomer();
  return page;
}

function denied(): HTMLElement {
  return h(
    "div",
    { class: "page" },
    pageHead({ title: "New order" }),
    emptyState({
      title: "Creating an order needs the invoice write permission",
      body: "An order issues an invoice, so it shares invoice:write:all with the documents it produces.",
      icon: "lock",
      action: btn("Open orders", { href: "/orders", icon: "box", size: "sm" }),
    }),
  );
}
