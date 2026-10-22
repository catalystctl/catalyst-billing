// Creating an order: buy a catalog price for a billing account.
//
// Three decisions shape this screen:
//
//  - The price is chosen from the catalog, never typed — except in the one place where typing
//    it is the point. A price version id is what the API takes for the *product*, so the select
//    below is a picker, not an input; the amount is read from the version.
//  - A negotiated price is a separate, explicit field: an amount plus a reason. The API stores
//    it as an override on the line and never writes the catalog, so one customer's deal cannot
//    become everybody's price. The amount is sent as integer minor units, parsed from the
//    currency's own scale — never `Number(x) * 100`.
//  - The customer is optional. Leaving it unset bills the acting staff member: a staff user
//    who is the billing account should not have to link an account to an account before
//    they can buy something. Choosing someone else is the "sell to this customer" case.

import { ApiError } from "../lib/api";
import { debounce, h } from "../lib/dom";
import { moneyWithCode, type Amount } from "../lib/format";
import type { Ctx } from "../lib/types";
import {
  amountFromInput,
  banner,
  btn,
  card,
  emptyState,
  field,
  input,
  pageHead,
  searchInput,
  select,
  textarea,
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
  manual_price?: boolean;
  custom_price_minor?: number;
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

  // ----------------------------------------------------------- custom price --
  //
  // Both fields are always on the page rather than behind a disclosure: a staff member looking
  // for "sell this at 5.00" must find it, and a browser test must be able to reach it without
  // first discovering a toggle. Leaving the amount empty means "catalog price".
  const customPrice = input({
    id: "order-custom-price",
    type: "text",
    inputmode: "decimal",
    placeholder: "e.g. 5.00",
    autocomplete: "off",
  });
  const customReason = textarea({
    id: "order-custom-reason",
    maxlength: 500,
    placeholder: "Why this customer pays a different price — recorded on the order and the invoice",
  });
  const customPreview = h("p", { class: "field-hint" });

  /** The catalog price currently selected, if any. */
  const chosenPrice = (): Price | undefined =>
    products.find((p) => p.id === productSelect.value)?.prices.find((price) => price.id === priceSelect.value);

  /**
   * The typed amount in minor units, or null when it is empty or not a number.
   *
   * The scale comes from the chosen price's currency, so 5.00 is 500 in USD and 5 in JPY.
   */
  const typedCustomMinor = (): number | null => {
    const raw = customPrice.value.trim();
    if (raw === "") return null;
    return amountFromInput(raw, chosenPrice()?.amount.scale ?? 2);
  };

  const paintCustomPreview = () => {
    const price = chosenPrice();
    const raw = customPrice.value.trim();
    if (raw === "") {
      customPreview.textContent = price
        ? `Charging the catalog price: ${moneyWithCode(price.amount)} per unit.`
        : "";
      return;
    }
    const minor = typedCustomMinor();
    if (minor === null) {
      customPreview.textContent = "That is not an amount this currency can express.";
      return;
    }
    const charged = moneyWithCode({
      minor,
      currency: price?.currency ?? "USD",
      scale: price?.amount.scale ?? 2,
    });
    // A negotiated price is this invoice's price, not the subscription's: a renewal bills the
    // catalog amount. Saying so here is cheaper than explaining it on the next invoice.
    const renewalNote =
      price && price.interval !== "one_time" && price.interval !== "free"
        ? " Renewals will bill the catalog price."
        : "";
    customPreview.textContent = price
      ? `This order charges ${charged} per unit instead of the catalog ${moneyWithCode(
          price.amount,
        )}, with no setup fee. The catalog is not changed.${renewalNote}`
      : `This order charges ${charged} per unit. The catalog is not changed.${renewalNote}`;
  };

  customPrice.addEventListener("input", paintCustomPreview);

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
    paintCustomPreview();
  };

  productSelect.addEventListener("change", paintPrices);
  // The preview quotes the amount in the chosen price's currency and scale, so a currency
  // switch has to repaint it.
  priceSelect.addEventListener("change", paintCustomPreview);

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

    // A custom price is an amount and a reason, or neither. The three ways to get it wrong are
    // checked here so the operator sees the problem before an order exists; the server refuses
    // all three as well, because the screen is a courtesy and the handler is the rule.
    const reason = customReason.value.trim();
    const typedAmount = customPrice.value.trim() !== "";
    const customMinor = typedCustomMinor();
    if (typedAmount && customMinor === null) {
      errorHost.replaceChildren(
        banner("danger", "Invalid custom price", "Enter the amount as a number, e.g. 5.00."),
      );
      return;
    }
    if (typedAmount && customMinor !== null && customMinor <= 0) {
      errorHost.replaceChildren(
        banner("danger", "Invalid custom price", "A custom price must be greater than zero."),
      );
      return;
    }
    if (typedAmount && reason === "") {
      errorHost.replaceChildren(
        banner(
          "danger",
          "A custom price needs a reason",
          "Say why this customer pays a different price. It is recorded on the order and the invoice, so a later reader can tell it apart from a catalog price.",
        ),
      );
      return;
    }
    if (!typedAmount && reason !== "") {
      errorHost.replaceChildren(
        banner("danger", "Missing custom price", "A reason was entered but no amount: enter the price, or clear the reason."),
      );
      return;
    }

    const body: Record<string, unknown> = { price_version_id: priceVersionID, quantity: qty };
    if (selectedCustomer) body.customer_id = selectedCustomer.id;
    if (customMinor !== null) {
      // Integer minor units, already scaled by the currency: the server never sees a float.
      body.custom_price_minor = customMinor;
      body.custom_price_reason = reason;
    }

    if (submit instanceof HTMLButtonElement) submit.disabled = true;
    try {
      const created = await ctx.api.post<CreatedOrder>("/api/v1/staff/orders", body);
      ctx.toast(created.manual_price ? "Order created at a custom price" : "Order created", {
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
      sub: "Priced from the catalog, or at a negotiated price for this customer",
      body: h(
        "div",
        { class: "stack" },
        field({ label: "Product", control: productSelect, id: "order-product" }),
        field({ label: "Price", control: priceSelect, id: "order-price", hint: "Prices are versioned; an issued invoice keeps the version it was sold at." }),
        field({ label: "Quantity", control: quantity, id: "order-quantity", hint: "1 to 1000" }),
      ),
    }),
    card({
      title: "Custom price",
      sub: "For this order only — the catalog price is never changed",
      body: h(
        "div",
        { class: "stack" },
        field({
          label: "Price per unit",
          control: customPrice,
          id: "order-custom-price",
          hint: "Leave empty to charge the catalog price. A custom price replaces the catalog price and its setup fee for this order, and is recorded on the invoice as manually priced.",
        }),
        field({
          label: "Reason",
          control: customReason,
          id: "order-custom-reason",
          required: true,
          hint: "Required with a custom price. Written to the order and the invoice, so the amount can be explained later.",
        }),
        customPreview,
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
          paintCustomPreview();
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
