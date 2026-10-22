// Checkout.
//
// The screen has one job: turn the cart into an order without ever letting the browser decide
// what something costs. The request body names *price versions* and quantities; the server
// re-prices from the catalog inside the transaction that writes the order, the subscription
// and the invoice. So the summary below is a reading aid built from `GET
// /api/v1/portal/cart/totals`, labelled as such, and it is never sent back.
//
// Three states get their own words rather than a stack trace:
//
//  - **The endpoint does not exist.** `POST /api/v1/portal/orders` is being built by another
//    package. A 404 or a 405 means this installation cannot take orders from the portal, and
//    the honest answer is to say so and leave the cart intact — not to render "something went
//    wrong" over a form the customer cannot use.
//  - **The cart is empty.** There is nothing to order, so the page says that and points at the
//    storefront rather than offering a button that would fail with `no_lines`.
//  - **The order was placed.** The response carries `invoice_id` and `invoice_number`; a free
//    order carries neither and is already activated, which is a different, better outcome and
//    is reported as such.
//
// The request shape is the one the customer checkout handler documents as canonical:
// `{items: [{price_version_id, quantity, config}], coupon_code, tos_accepted}`.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { formatCurrency, plural, type Amount, type CurrencyFormat } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { btn, card, emptyState, pageHead, resource, skeletonPage } from "../ui/kit";
import { formatsOf, loadCurrencies, type Cart, type CartTotals, type PublicCurrency } from "./cart";
import { portalLoadError } from "./portal";

export const meta = { title: "Checkout", crumb: "Checkout" };

// ---------------------------------------------------------------- payloads --

/** `POST /api/v1/portal/orders` — `checkoutJSON` in internal/httpapi/checkout_customer.go.
 *
 *  `invoice_id` is empty exactly when `activated` is true: a zero-total order has nothing to
 *  pay, so it has no document and the service is live immediately. */
export interface PlacedOrder {
  id: string;
  status: string;
  currency: string;
  subtotal: Amount;
  discount: Amount;
  tax: Amount;
  total: Amount;
  invoice_id: string;
  invoice_number: string;
  subscription_ids: string[];
  service_ids: string[];
  activated: boolean;
  coupon_code: string;
  lines: Array<{ description: string; quantity: number; unit: Amount; net: Amount; tax: Amount; total: Amount }>;
}

interface CheckoutView {
  cart: Cart;
  totals: CartTotals | null;
  totalsProblem: string;
  currencies: PublicCurrency[];
  /** The configured terms of service, from the settings page. Empty means no checkbox is
   *  required, which is what the server decides on too (`register.Policy.TOSRequired`). */
  terms: string;
}

export default async function checkoutPage(ctx: Ctx): Promise<Node> {
  const host = h("div");
  const load = async (signal: AbortSignal): Promise<CheckoutView> => {
    const currencies = await loadCurrencies(ctx.api, signal);
    let cart: Cart;
    try {
      cart = await ctx.api.get<Cart>("/api/v1/portal/cart", { cache: false, signal });
    } catch (err) {
      throw portalLoadError(err);
    }
    let totals: CartTotals | null = null;
    let totalsProblem = "";
    try {
      totals = await ctx.api.get<CartTotals>("/api/v1/portal/cart/totals", { cache: false, signal });
    } catch (err) {
      if (err instanceof DOMException && err.name === "AbortError") throw err;
      totalsProblem = err instanceof ApiError ? err.detail || "The cart could not be priced." : "The cart could not be priced.";
    }
    // The terms text is configuration, and `GET /api/v1/staff/settings` needs settings:write.
    // A customer therefore cannot read it, and asking would be a guaranteed 403 on every
    // checkout. So the read only happens for staff previewing the screen; a customer gets the
    // checkbox without the prose and the server enforces its own rule, which is why a
    // `tos_required` refusal is surfaced as an inline instruction rather than a toast.
    let terms = "";
    try {
      if (!ctx.can("settings:write")) throw new Error("not readable by this session");
      const settings = await ctx.api.get<{ data: Array<{ group: string; keys: Array<{ key: string; value: string; is_secret: boolean }> }> }>(
        "/api/v1/staff/settings",
        { query: { group: "general" }, cache: false, signal },
      );
      const group = (settings.data ?? []).find((entry) => entry.group === "general");
      const key = (group?.keys ?? []).find((entry) => entry.key === "tos");
      if (key && !key.is_secret) terms = key.value ?? "";
    } catch {
      terms = "";
    }
    return { cart, totals, totalsProblem, currencies, terms };
  };

  const paint = (): void => {
    swap(host, resource(ctx, load, (view) => render(ctx, view, paint), { skeleton: skeletonPage }));
  };
  paint();

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Checkout",
      sub: "Confirm the order. The total is priced by the server from the catalog; nothing here is charged until you place it.",
      actions: btn("Back to cart", { href: "/cart", icon: "arrowLeft", size: "sm" }),
    }),
    host,
  );
}

// ------------------------------------------------------------------ render --

function render(ctx: Ctx, view: CheckoutView, repaint: () => void): Node {
  const items = view.cart.items ?? [];
  const formats = formatsOf(view.currencies);
  const totals = view.totals;

  if (items.length === 0) {
    return card({
      flush: true,
      body: emptyState({
        title: "There is nothing to check out",
        body: "Your cart is empty. Pick a plan from the storefront first; the order is written from what is in the cart.",
        icon: "bag",
        action: btn("Browse the catalog", { href: "/products", variant: "primary", icon: "box" }),
      }),
    });
  }

  /** True when this session could read the configured terms, which makes the checkbox a
   *  hard requirement in the UI as well as on the server. A customer cannot read the setting,
   *  so for them the box is present, unchecked by default, and the server decides. */
  const termsKnown = view.terms.trim() !== "";
  const tos = h("input", { type: "checkbox", id: "checkout-tos" });
  const place = btn("Place order", {
    variant: "primary",
    size: "lg",
    block: true,
    iconAfter: "arrowRight",
    disabled: termsKnown,
  }) as HTMLButtonElement;
  const errorHost = h("div");

  tos.addEventListener("change", () => {
    if (termsKnown) place.disabled = !tos.checked;
  });

  place.addEventListener("click", () => void placeOrder());

  const summary = card({
    title: "Order summary",
    sub: `${plural(items.length, "line")} · ${view.cart.currency}`,
    body: h(
      "div",
      { class: "stack" },
      h(
        "div",
        { class: "cart-lines cart-lines--compact" },
        ...items.map((item) => {
          const line = (totals?.lines ?? []).find((candidate) => candidate.item_id === item.id);
          const configValues = Object.values(item.config ?? {}).filter((value) => String(value).trim() !== "");
          return h(
            "div",
            { class: "cart-line" },
            h(
              "div",
              { class: "cart-line-main" },
              h("div", { class: "cart-line-title" }, h("span", { class: "row-title" }, item.product_name || "Product")),
              h(
                "div",
                { class: "cart-line-meta" },
                item.price_name || "Standard price",
                configValues.length > 0 ? h("span", { class: "cart-line-dot" }, "·") : null,
                configValues.length > 0 ? h("span", null, configValues.join(" · ")) : null,
              ),
            ),
            h("div", { class: "cart-qty cart-qty--static" }, h("span", { class: "cart-qty-value" }, String(item.quantity))),
            h("div", { class: "cart-line-side" }, line ? fmt(line.total, formats) : h("span", { class: "faint" }, "—")),
          );
        }),
      ),
      h(
        "div",
        { class: "cart-totals" },
        totalRow("Subtotal", totals ? fmt(totals.subtotal, formats) : h("span", { class: "faint" }, "—")),
        totals && totals.discount.minor > 0
          ? totalRow(
              totals.coupon ? `Discount (${totals.coupon.code})` : "Discount",
              fmt({ ...totals.discount, minor: -totals.discount.minor }, formats, "money--neg"),
            )
          : null,
        totals && totals.tax.minor > 0 ? totalRow("Tax", fmt(totals.tax, formats)) : null,
        h("div", { class: "cart-total-divider" }),
        totalRow("Total", totals ? fmt(totals.total, formats) : h("span", { class: "faint" }, "—"), true),
      ),
      view.totalsProblem
        ? h(
            "div",
            { class: "banner banner--warning" },
            icon("alertTriangle", 15),
            h(
              "div",
              { class: "stack-sm" },
              h("strong", null, "The total could not be confirmed"),
              h("span", null, view.totalsProblem),
            ),
          )
        : null,
      h(
        "p",
        { class: "field-hint" },
        "The server prices this order again when it is placed, from the same catalog. If a price changed while the cart sat here, the order is written at the new price and the invoice shows it.",
      ),
    ),
  });

  const confirm = card({
    title: "Confirm",
    body: h(
      "div",
      { class: "stack" },
      errorHost,
      termsKnown
        ? h(
            "div",
            { class: "checkout-terms" },
            h("div", { class: "checkout-terms-text" }, ...view.terms.split(/\n{2,}/).map((para) => h("p", null, para))),
            h(
              "label",
              { class: "checkbox", for: "checkout-tos" },
              tos,
              h("span", null, "I accept the terms of service above."),
            ),
          )
        : h(
            "div",
            { class: "stack-sm" },
            h(
              "label",
              { class: "checkbox", for: "checkout-tos" },
              tos,
              h("span", null, "I accept this installation's terms of service."),
            ),
            h(
              "p",
              { class: "field-hint" },
              "The terms are set by the operator. If this installation requires them, the order is refused until the box is ticked.",
            ),
          ),
      place,
      h(
        "p",
        { class: "field-hint" },
        "Placing the order issues an invoice and starts the subscription. Payment is a separate step on the invoice.",
      ),
    ),
  });

  return h(
    "div",
    { class: "split" },
    h("div", { class: "stack" }, summary, confirm),
    h("div", { class: "stack" }, accountCard(ctx), helpCard()),
  );

  // ------------------------------------------------------------- mutations --

  async function placeOrder(): Promise<void> {
    errorHost.replaceChildren();
    if (termsKnown && !tos.checked) {
      swap(errorHost, h("div", { class: "banner banner--danger" }, "Accept the terms of service to place the order."));
      return;
    }
    place.disabled = true;
    place.classList.add("is-busy");
    try {
      const order = await ctx.api.post<PlacedOrder>("/api/v1/portal/orders", {
        // Canonical names, per the handler's documentation: `items` and `tos_accepted`. The
        // body carries no customer, no amount and no currency — all three are derived.
        items: items.map((item) => ({
          price_version_id: item.price_version_id,
          quantity: item.quantity,
          config: item.config ?? {},
        })),
        coupon_code: view.totals?.coupon?.code ?? "",
        // The real checkbox state, never a constant: a client that asserted acceptance on the
        // customer's behalf would make the acceptance meaningless.
        tos_accepted: tos.checked,
      });
      ctx.api.invalidate("/api/v1/portal");
      // A free order activates with no invoice, so there is no document to open. Saying so is
      // better than routing to an empty invoice id.
      if (!order.invoice_id) {
        ctx.toast(order.activated ? "Order placed and activated" : "Order placed", {
          tone: "success",
          detail: order.activated
            ? "Nothing was due, so the service is live already."
            : "The order was recorded.",
          duration: 0,
        });
        ctx.navigate("/portal/services");
        return;
      }
      ctx.toast("Order placed", {
        tone: "success",
        detail: order.invoice_number ? `Invoice ${order.invoice_number}` : "The invoice is ready.",
      });
      ctx.navigate(`/portal/invoices/${encodeURIComponent(order.invoice_id)}`);
    } catch (err) {
      if (err instanceof ApiError && err.code === "tos_required") {
        // The installation requires terms this session could not read. Show the requirement
        // where the checkbox is, and leave the order unplaced.
        swap(
          errorHost,
          h(
            "div",
            { class: "banner banner--danger" },
            icon("alertCircle", 15),
            h(
              "div",
              { class: "stack-sm" },
              h("strong", null, "The terms of service must be accepted"),
              h("span", null, "Tick the box below, then place the order again."),
            ),
          ),
        );
        place.disabled = false;
        return;
      }
      reportCheckoutError(ctx, err);
      // A 404/405 means this installation has no customer checkout route at all. That is a
      // permanent state, so the form is replaced with an explanation rather than left to be
      // clicked again.
      if (err instanceof ApiError && (err.status === 404 || err.status === 405)) {
        repaint();
      }
    } finally {
      place.disabled = termsKnown ? !tos.checked : false;
      place.classList.remove("is-busy");
    }
  }
}

/** Plain copy for every documented checkout refusal. Branch on the code, never the detail. */
function reportCheckoutError(ctx: Ctx, err: unknown): void {
  if (!(err instanceof ApiError)) {
    ctx.toast("The order could not be placed", {
      tone: "danger",
      detail: "The server could not be reached. Nothing has been ordered.",
    });
    return;
  }
  const messages: Record<string, { title: string; detail: string }> = {
    not_found: {
      title: "Checkout is not available on this installation",
      detail: "The customer ordering endpoint is not served by this build. Nothing has been ordered; your cart is unchanged.",
    },
    method_not_allowed: {
      title: "Checkout is not available on this installation",
      detail: "The customer ordering endpoint is not served by this build. Nothing has been ordered; your cart is unchanged.",
    },
    orders_unavailable: {
      title: "Ordering is not configured",
      detail: "This installation has no order writer configured, so nothing can be bought from the portal yet.",
    },
    no_lines: { title: "There is nothing to order", detail: "The cart is empty. Add something first." },
    too_many_lines: { title: "That is too many lines", detail: "An order can carry at most 50 lines." },
    no_billing_account: {
      title: "This login has no billing account",
      detail: "Finish registration or contact the operator, then try again.",
    },
    invalid_quantity: { title: "A quantity is out of range", detail: "Quantities are between 1 and 1000." },
    quantity_not_allowed: { title: "This product is sold one at a time", detail: "Set the quantity to 1." },
    mixed_currency: { title: "The lines use different currencies", detail: "One order must be in one currency." },
    out_of_stock: {
      title: "Not enough stock",
      detail: "Someone else took the last one while you were checking out. Reduce the quantity or choose another plan.",
    },
    user_limit_reached: {
      title: "You already hold the maximum of this product",
      detail: "This product has a per-customer limit that you have reached.",
    },
    invalid_price: {
      title: "This price cannot be billed",
      detail: "The price has no valid schedule. Contact the operator.",
    },
    invalid_config: { title: "A configuration choice is not valid", detail: "Reload the cart and choose again." },
    tos_required: { title: "The terms must be accepted", detail: "Accept the terms of service and place the order again." },
    email_verification_required: {
      title: "Confirm your email address first",
      detail: "This installation requires a verified address before buying. Check your inbox for the link.",
    },
    coupon_not_found: { title: "That coupon code is not valid", detail: "Remove it from the cart and try again." },
    coupon_disabled: { title: "That coupon is no longer available", detail: "Remove it from the cart and try again." },
    coupon_not_started: { title: "That coupon is not active yet", detail: "Remove it and try again later." },
    coupon_expired: { title: "That coupon has expired", detail: "Remove it from the cart and try again." },
    coupon_exhausted: { title: "That coupon has been fully redeemed", detail: "Remove it from the cart." },
    coupon_already_used: { title: "You have already used that coupon", detail: "Remove it from the cart." },
    coupon_not_applicable: { title: "That coupon does not apply", detail: "Remove it from the cart and try again." },
    coupon_currency_mismatch: { title: "That coupon is in another currency", detail: "Remove it from the cart." },
    coupon_no_discount: { title: "That coupon gives no discount here", detail: "Remove it from the cart." },
    ambiguous_cart: { title: "The request was ambiguous", detail: "Reload the page and try again." },
    portal_unavailable: {
      title: "The customer portal is not configured",
      detail: "This deployment has no portal, so orders cannot be placed from it.",
    },
  };
  const hit = messages[err.code];
  if (hit) {
    ctx.toast(hit.title, { tone: "danger", detail: hit.detail, duration: 0 });
    return;
  }
  ctx.toast(err.detail || "The order could not be placed", { tone: "danger", detail: err.code, duration: 0 });
}

// ------------------------------------------------------------------ pieces --

function fmt(amount: Amount, formats: Map<string, CurrencyFormat>, extraClass = ""): HTMLElement {
  return h(
    "span",
    { class: ["money", extraClass].filter(Boolean).join(" ") },
    formatCurrency(amount, formats.get(amount.currency.toUpperCase()), true),
  );
}

function totalRow(label: string, value: Node, strong = false): HTMLElement {
  return h(
    "div",
    { class: `cart-total-row${strong ? " cart-total-row--strong" : ""}` },
    h("span", { class: "cart-total-label" }, label),
    h("span", { class: "cart-total-value" }, value),
  );
}

function accountCard(ctx: Ctx): HTMLElement {
  const user = ctx.session.user;
  return card({
    title: "Billing account",
    body: h(
      "div",
      { class: "stack-sm" },
      h("div", { class: "kv" }, h("span", { class: "kv-key" }, "Signed in as"), h("span", { class: "kv-value" }, user.email)),
      h(
        "div",
        { class: "kv" },
        h("span", { class: "kv-key" }, "Email"),
        h("span", { class: "kv-value" }, user.email_verified ? "Verified" : "Not verified"),
      ),
      h(
        "p",
        { class: "field-hint" },
        "The order is placed against this account: the invoice and the service appear in your portal.",
      ),
      btn("Billing details", { href: "/portal/profile", size: "sm", variant: "ghost", icon: "building" }),
    ),
  });
}

function helpCard(): HTMLElement {
  return card({
    title: "What happens next",
    body: h(
      "div",
      { class: "stack-sm" },
      step("1", "An invoice is issued", "It appears in your portal with the total and the due date."),
      step("2", "You pay it", "The payment page opens from the invoice. Nothing is charged before that."),
      step("3", "The service is provisioned", "Provisioning runs after the payment settles, and the service appears under Services."),
      h(
        "p",
        { class: "field-hint" },
        "A zero-total order skips the invoice entirely and activates immediately.",
      ),
    ),
  });
}

function step(number: string, title: string, body: string): HTMLElement {
  return h(
    "div",
    { class: "checkout-step" },
    h("span", { class: "checkout-step-num" }, number),
    h("div", { class: "stack-sm" }, h("strong", null, title), h("span", { class: "dim" }, body)),
  );
}
