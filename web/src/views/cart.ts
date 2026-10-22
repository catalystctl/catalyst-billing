// The customer's cart.
//
// A cart holds a *selection*, never a price: every number on this screen is re-read from the
// catalog by `GET /api/v1/portal/cart/totals` at the moment the screen loads. That is why the
// page refetches after every mutation instead of patching a number in place — a cart that
// showed a total it had computed itself would be a second pricer, and the two would disagree
// the first time an operator changed a price.
//
// Two rules shape the layout:
//
//  - **Money never leaves its currency.** A cart has exactly one currency, adopted from the
//    first price that lands in it, because a price is denominated and this installation has no
//    exchange rate. So the selector is disabled with an explanation once the cart holds an
//    item, and every amount is rendered through `formatCurrency` with that currency's own
//    symbol, position and grouping.
//  - **A coupon refusal is shown, not swallowed.** When an attached code stops applying (it
//    expired, or the cart changed) the server returns the cart *and* the reason. The customer
//    can remove the code and still check out, which is better than a silently missing discount.
//
// The payloads below are the wire shapes in internal/httpapi/commerce_cart.go: `cartJSON` and
// `totalsJSON`. Nothing here is derived from a guess: a field that is not in those functions is
// not on this page.

import { ApiError, type Api } from "../lib/api";
import { h, swap } from "../lib/dom";
import { formatCurrency, plural, plainCurrency, type Amount, type CurrencyFormat } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { btn, card, emptyState, errorState, field, input, pageHead, resource, skeletonPage, withBusy } from "../ui/kit";
import { portalLoadError } from "./portal";

export const meta = { title: "Cart", crumb: "Your cart" };

// ---------------------------------------------------------------- payloads --

/** One line, verbatim from `cartJSON`. `config` maps a config-option id to the chosen value;
 *  the page shows the values because those are what the customer recognises. */
export interface CartItem {
  id: string;
  price_version_id: string;
  product_id: string;
  product_name: string;
  price_name: string;
  quantity: number;
  config: Record<string, string> | null;
  label: string;
  created_at: string;
}

export interface Cart {
  id: string;
  currency: string;
  coupon_id: string | null;
  items: CartItem[] | null;
  item_count: number;
  tos_accepted_at: string | null;
}

/** One priced line from `totalsJSON`. Every amount is the `{minor, currency, scale}` triple. */
export interface CartLineTotal {
  item_id: string;
  price_version_id: string;
  product_id: string;
  description: string;
  quantity: number;
  unit: Amount;
  setup: Amount;
  line: Amount;
  setup_line: Amount;
  discount: Amount;
  net: Amount;
  tax: Amount;
  total: Amount;
  tax_rate: string;
  tax_evidence?: { jurisdiction: string; rate: string; inclusive: boolean; exemption_reason?: string };
}

export interface AppliedCoupon {
  coupon_id: string;
  code: string;
  applies_to: string;
  discount: Amount;
  recurring_cycles: number | null;
}

export interface CartTotals {
  currency: string;
  lines: CartLineTotal[] | null;
  subtotal: Amount;
  discount: Amount;
  tax: Amount;
  total: Amount;
  item_count: number;
  coupon?: AppliedCoupon;
  coupon_problem?: string;
}

/** The public currency row (`GET /api/v1/currencies`). Presentation only: symbol, position,
 *  scale and a pre-rendered example. The arithmetic never comes from here. */
export interface PublicCurrency {
  code: string;
  name: string;
  scale: number;
  symbol: string;
  symbol_position: string;
  is_default: boolean;
  example: string;
}

/** Loads the presentation rules. A failure is not fatal: every amount still renders through
 *  `formatCurrency`'s plain fallback, so the cart keeps working with no symbol. */
export async function loadCurrencies(api: Api, signal?: AbortSignal): Promise<PublicCurrency[]> {
  try {
    const res = await api.get<{ data: PublicCurrency[] }>("/api/v1/currencies", { cache: false, signal });
    return res.data ?? [];
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") throw err;
    return [];
  }
}

/** Turns the server's currency rows into the format map `formatCurrency` expects. The public
 *  shape omits the separators and the group size, so those keep the plain defaults. */
export function formatsOf(rows: PublicCurrency[]): Map<string, CurrencyFormat> {
  const map = new Map<string, CurrencyFormat>();
  for (const row of rows) {
    const code = row.code.toUpperCase();
    map.set(code, {
      ...plainCurrency(code, row.scale ?? 2),
      symbol: row.symbol ?? "",
      symbol_position: row.symbol_position === "suffix" ? "suffix" : "prefix",
    });
  }
  return map;
}

/** The minor-unit exponent for a code, from the server's currency rows. */
export function scaleOf(rows: PublicCurrency[], currency: string): number {
  const hit = rows.find((row) => row.code.toUpperCase() === currency.toUpperCase());
  return hit?.scale ?? 2;
}

/** An amount from a raw minor value and a currency code, for the endpoints that send
 *  `amount_minor` + `currency` rather than the triple (a coupon, a config-option price). */
export function amountOf(rows: PublicCurrency[], minor: number, currency: string): Amount {
  return { minor, currency, scale: scaleOf(rows, currency) };
}

/** An amount rendered with the currency's own symbol, position and grouping. The code is
 *  appended by default: a cart is the one place where a bare "$" is ambiguous. */
export function fmtMoney(
  amount: Amount | null | undefined,
  formats: Map<string, CurrencyFormat>,
  opts: { code?: boolean; class?: string } = {},
): HTMLElement {
  const text = formatCurrency(amount, amount ? formats.get(amount.currency.toUpperCase()) : undefined, opts.code ?? true);
  return h("span", { class: ["money", opts.class ?? ""].filter(Boolean).join(" ") }, text);
}

/** "0.19" reads as "19%" on a line. A rate the server canonicalised as a fraction ("1/5") is
 *  shown as it is rather than rounded into a lie. */
export function taxRateLabel(rate: string): string {
  const trimmed = (rate ?? "").trim();
  if (trimmed === "" || trimmed === "0") return "";
  if (/^\d+(\.\d+)?$/.test(trimmed)) {
    const [whole = "0", fraction = ""] = trimmed.split(".");
    const shifted = `${whole}${(fraction + "00").slice(0, 2)}`;
    const cut = shifted.length - 2;
    const major = shifted.slice(0, cut).replace(/^0+(?=\d)/, "");
    const minor = shifted.slice(cut).replace(/0+$/, "");
    return `${major}${minor ? `.${minor}` : ""}%`;
  }
  return trimmed;
}

// -------------------------------------------------------------------- page --

interface CartView {
  cart: Cart;
  totals: CartTotals | null;
  totalsProblem: string;
  currencies: PublicCurrency[];
}

export default async function cartPage(ctx: Ctx): Promise<Node> {
  const host = h("div");

  const load = async (signal: AbortSignal): Promise<CartView> => {
    const currencies = await loadCurrencies(ctx.api, signal);
    let cart: Cart;
    try {
      cart = await ctx.api.get<Cart>("/api/v1/portal/cart", { cache: false, signal });
    } catch (err) {
      throw cartLoadError(err);
    }
    // The totals are a second request on purpose: the cart is what was chosen, the totals are
    // what it costs. A pricing failure is reported inside the loaded screen (the lines are
    // still useful) rather than replacing the page with an error about a total.
    let totals: CartTotals | null = null;
    let totalsProblem = "";
    try {
      totals = await ctx.api.get<CartTotals>("/api/v1/portal/cart/totals", { cache: false, signal });
    } catch (err) {
      if (err instanceof DOMException && err.name === "AbortError") throw err;
      totalsProblem =
        err instanceof ApiError
          ? err.detail || "The cart could not be priced."
          : "The cart could not be priced. The server could not be reached.";
    }
    return { cart, totals, totalsProblem, currencies };
  };

  const paint = (): void => {
    swap(host, resource(ctx, load, (view) => render(view), { skeleton: skeletonPage }));
  };

  /** Re-renders from a fresh read. Used after every mutation, because the price is the
   *  server's and the client is not allowed to guess it. */
  const reload = async (): Promise<void> => {
    paint();
  };

  // ------------------------------------------------------------- rendering --

  const render = (view: CartView): Node => {
    const formats = formatsOf(view.currencies);
    const items = view.cart.items ?? [];
    const totals = view.totals;

    if (items.length === 0) {
      return h(
        "div",
        { class: "stack" },
        card({
          flush: true,
          body: emptyState({
            title: "Your cart is empty",
            body: "Pick a plan from the storefront and it appears here with the price and the period it renews on. Nothing is charged until you place the order.",
            icon: "bag",
            action: btn("Browse the catalog", { href: "/products", variant: "primary", icon: "box" }),
          }),
        }),
        currencyCard(view),
      );
    }

    return h(
      "div",
      { class: "split" },
      h(
        "div",
        { class: "stack" },
        card({
          title: "Items",
          sub: plural(items.length, "line"),
          actions: btn("Empty cart", { variant: "ghost", size: "sm", icon: "trash", onclick: () => void clearCart() }),
          flush: true,
          body: h("div", { class: "cart-lines" }, ...items.map((item) => lineRow(item))),
        }),
        couponCard(totals),
        view.totalsProblem
          ? h(
              "div",
              { class: "banner banner--warning" },
              icon("alertTriangle", 15),
              h(
                "div",
                { class: "stack-sm" },
                h("strong", null, "The cart could not be priced"),
                h("span", null, view.totalsProblem),
              ),
              btn("Try again", { variant: "ghost", size: "sm", onclick: () => reload() }),
            )
          : null,
      ),
      h("div", { class: "stack" }, summaryCard(view), currencyCard(view)),
    );

    // ------------------------------------------------------------- pieces --

    function lineRow(item: CartItem): HTMLElement {
      const pricedLine = (totals?.lines ?? []).find((line) => line.item_id === item.id);
      const configValues = Object.values(item.config ?? {}).filter((value) => String(value).trim() !== "");

      const minus = h(
        "button",
        {
          class: "cart-step",
          type: "button",
          title: "One fewer",
          "aria-label": `Decrease the quantity of ${item.product_name}`,
          disabled: item.quantity <= 1,
          onclick: () => void setQuantity(item, item.quantity - 1, minus),
        },
        icon("minus", 12),
      );
      const plus = h(
        "button",
        {
          class: "cart-step",
          type: "button",
          title: "One more",
          "aria-label": `Increase the quantity of ${item.product_name}`,
          onclick: () => void setQuantity(item, item.quantity + 1, plus),
        },
        icon("plus", 12),
      );
      const remove = h(
        "button",
        {
          class: "btn btn--ghost btn--icon btn--sm",
          type: "button",
          title: "Remove this line",
          "aria-label": `Remove ${item.product_name}`,
          onclick: () => void removeItem(item, remove),
        },
        icon("x", 13),
      );

      const rate = pricedLine ? taxRateLabel(pricedLine.tax_rate) : "";

      return h(
        "div",
        { class: "cart-line" },
        h(
          "div",
          { class: "cart-line-main" },
          h(
            "div",
            { class: "cart-line-title" },
            h("span", { class: "row-title" }, item.product_name || "Product"),
            item.label ? h("span", { class: "chip" }, item.label) : null,
          ),
          h(
            "div",
            { class: "cart-line-meta" },
            item.price_name ? h("span", null, item.price_name) : h("span", { class: "faint" }, "Standard price"),
            configValues.length > 0 ? h("span", { class: "cart-line-dot" }, "·") : null,
            configValues.length > 0 ? h("span", null, configValues.join(" · ")) : null,
          ),
          rate
            ? h(
                "div",
                { class: "cart-line-tax" },
                `Includes ${rate} tax`,
                pricedLine?.tax_evidence?.jurisdiction ? ` (${pricedLine.tax_evidence.jurisdiction})` : "",
              )
            : null,
        ),
        h("div", { class: "cart-qty" }, minus, h("span", { class: "cart-qty-value" }, String(item.quantity)), plus),
        h(
          "div",
          { class: "cart-line-side" },
          pricedLine
            ? h(
                "div",
                { class: "cart-line-amount" },
                fmtMoney(pricedLine.total, formats),
                h(
                  "span",
                  { class: "cart-line-unit" },
                  `${pricedLine.quantity} × ${formatCurrency(pricedLine.unit, formats.get(pricedLine.unit.currency.toUpperCase()), false)}`,
                ),
              )
            : h("span", { class: "faint" }, "—"),
          remove,
        ),
      );
    }

    function couponCard(priced: CartTotals | null): HTMLElement {
      const applied = priced?.coupon;
      const problem = priced?.coupon_problem ?? "";

      if (applied) {
        return card({
          title: "Coupon",
          sub: applied.code,
          body: h(
            "div",
            { class: "stack-sm" },
            h(
              "div",
              { class: "cart-coupon-applied" },
              h("span", { class: "chip is-active" }, icon("tag", 12), applied.code),
              h(
                "span",
                { class: "dim" },
                "−",
                formatCurrency(applied.discount, formats.get(applied.discount.currency.toUpperCase()), true),
              ),
              h("span", { class: "spacer" }),
              btn("Remove", { variant: "ghost", size: "sm", icon: "x", onclick: () => void removeCoupon() }),
            ),
            applied.recurring_cycles !== null && applied.recurring_cycles !== undefined
              ? h(
                  "p",
                  { class: "field-hint" },
                  applied.recurring_cycles === 1
                    ? "Applies to the first billing cycle only."
                    : `Applies to the first ${applied.recurring_cycles} billing cycles.`,
                )
              : null,
            problem
              ? h(
                  "div",
                  { class: "banner banner--warning" },
                  icon("alertTriangle", 15),
                  h(
                    "div",
                    { class: "stack-sm" },
                    h("strong", null, "This code is not reducing anything right now"),
                    h("span", null, problem),
                  ),
                )
              : null,
          ),
        });
      }

      const code = input({
        id: "cart-coupon-code",
        class: "input--mono",
        placeholder: "SAVE10",
        maxlength: 64,
        autocomplete: "off",
      });
      code.addEventListener("keydown", (event: KeyboardEvent) => {
        if (event.key === "Enter") {
          event.preventDefault();
          void applyCoupon(code);
        }
      });

      return card({
        title: "Coupon",
        body: h(
          "div",
          { class: "stack-sm" },
          h(
            "div",
            { class: "cart-coupon-form" },
            field({ label: "Code", control: code, id: "cart-coupon-code", class: "cart-coupon-field" }),
            btn("Apply", { variant: "primary", onclick: () => void applyCoupon(code) }),
          ),
          problem
            ? h("div", { class: "banner banner--warning" }, icon("alertTriangle", 15), h("span", null, problem))
            : h("p", { class: "field-hint" }, "One code per cart. It is validated against the items in it."),
        ),
      });
    }

    function summaryCard(view: CartView): HTMLElement {
      const priced = view.totals;
      const itemCount = (view.cart.items ?? []).length;
      const checkout = itemCount > 0
        ? btn("Checkout", { href: "/checkout", variant: "primary", size: "lg", block: true, iconAfter: "arrowRight" })
        : btn("Checkout", { variant: "primary", size: "lg", block: true, disabled: true, iconAfter: "arrowRight" });

      return card({
        title: "Summary",
        body: h(
          "div",
          { class: "stack" },
          h(
            "div",
            { class: "cart-totals" },
            totalRow("Subtotal", priced ? fmtMoney(priced.subtotal, formats) : h("span", { class: "faint" }, "—")),
            priced && priced.discount.minor > 0
              ? totalRow(
                  "Discount",
                  fmtMoney({ ...priced.discount, minor: -priced.discount.minor }, formats, { class: "money--neg" }),
                )
              : null,
            priced && priced.tax.minor > 0 ? totalRow("Tax", fmtMoney(priced.tax, formats)) : null,
            h("div", { class: "cart-total-divider" }),
            totalRow("Total", priced ? fmtMoney(priced.total, formats) : h("span", { class: "faint" }, "—"), true),
          ),
          h("p", { class: "field-hint" }, "Tax comes from your billing country when this installation has tax configured."),
          h("div", { class: "stack-sm" }, checkout),
          h(
            "p",
            { class: "field-hint" },
            "Checkout asks you to confirm the order and, when terms are configured, to accept them.",
          ),
        ),
      });
    }

    function totalRow(label: string, value: Node, strong = false): HTMLElement {
      return h(
        "div",
        { class: `cart-total-row${strong ? " cart-total-row--strong" : ""}` },
        h("span", { class: "cart-total-label" }, label),
        h("span", { class: "cart-total-value" }, value),
      );
    }

    /** The currency block: the selector plus the reason it is locked once an item exists. */
    function currencyCard(view: CartView): HTMLElement {
      const cart = view.cart;
      const locked = (cart.items ?? []).length > 0;
      const rows =
        view.currencies.length > 0
          ? view.currencies
          : [
              {
                code: cart.currency,
                name: cart.currency,
                scale: 2,
                symbol: "",
                symbol_position: "prefix",
                is_default: true,
                example: "",
              },
            ];

      const picker = h(
        "select",
        {
          class: "select",
          id: "cart-currency",
          "aria-label": "Cart currency",
          disabled: locked,
          onchange: (event: Event) => {
            const code = (event.target as HTMLSelectElement).value;
            // The cart adopts the currency of the first price that lands in it, so an empty
            // cart carries the choice to the storefront rather than pretending to convert.
            window.location.assign(`/products?currency=${encodeURIComponent(code)}`);
          },
        },
        ...rows.map((row) => {
          const option = h("option", { value: row.code }, `${row.code} — ${row.name}`);
          if (row.code.toUpperCase() === cart.currency.toUpperCase()) option.selected = true;
          return option;
        }),
      );

      return card({
        title: "Currency",
        body: h(
          "div",
          { class: "stack-sm" },
          h(
            "div",
            { class: "cart-currency-row" },
            h("span", { class: "cart-currency-code mono" }, cart.currency),
            picker,
          ),
          locked
            ? h(
                "p",
                { class: "field-hint" },
                "The currency is fixed once the cart holds an item. Every price is set per currency and this installation has no exchange rate, so switching now would silently re-denominate your order. Remove the items to change it.",
              )
            : h(
                "p",
                { class: "field-hint" },
                "The first item you add decides the currency, because a price is denominated and there is no exchange rate. Empty the cart to change it.",
              ),
        ),
      });
    }

    // ---------------------------------------------------------- mutations --

    async function setQuantity(item: CartItem, quantity: number, button: HTMLButtonElement): Promise<void> {
      if (quantity < 1 || quantity > 1000) return;
      await withBusy(ctx, button, async () => {
        try {
          await ctx.api.put(`/api/v1/portal/cart/items/${encodeURIComponent(item.id)}`, { quantity });
          await reload();
        } catch (err) {
          reportCartError(err, "The quantity could not be changed");
        }
      });
    }

    async function removeItem(item: CartItem, button: HTMLButtonElement): Promise<void> {
      await withBusy(ctx, button, async () => {
        try {
          await ctx.api.del(`/api/v1/portal/cart/items/${encodeURIComponent(item.id)}`);
          ctx.toast("Removed from your cart", { tone: "success", detail: item.product_name });
          await reload();
        } catch (err) {
          reportCartError(err, "That line could not be removed");
        }
      });
    }

    async function clearCart(): Promise<void> {
      const ok = await ctx.confirm({
        title: "Empty the cart?",
        body: "Every line is removed, and the coupon with them. The currency stays as it is.",
        confirmLabel: "Empty cart",
        danger: true,
      });
      if (!ok) return;
      try {
        await ctx.api.del("/api/v1/portal/cart");
        ctx.toast("Cart emptied", { tone: "success" });
        await reload();
      } catch (err) {
        reportCartError(err, "The cart could not be emptied");
      }
    }

    async function applyCoupon(box: HTMLInputElement): Promise<void> {
      const raw = box.value.trim();
      if (!raw) {
        ctx.toast("Enter a code first", {
          tone: "info",
          detail: "Codes are 2–64 letters, digits, hyphens or underscores.",
        });
        return;
      }
      try {
        await ctx.api.post("/api/v1/portal/cart/coupon", { code: raw });
        ctx.toast("Coupon applied", { tone: "success", detail: raw.toUpperCase() });
        await reload();
      } catch (err) {
        if (!(err instanceof ApiError)) {
          ctx.toast("The coupon could not be applied", { tone: "danger", detail: "The server could not be reached." });
          return;
        }
        // The documented refusals from cartError() and commerceError(): each names something
        // the customer can act on, so each gets its own sentence.
        const messages: Record<string, string> = {
          not_found: "That code does not exist.",
          coupon_expired: "That code has expired.",
          coupon_exhausted: "That code has been fully redeemed.",
          coupon_not_applicable: "That code does not apply to anything in your cart.",
          not_applicable: "That code does not apply to anything in your cart.",
          coupon_disabled: "That code is no longer available.",
          coupon_not_started: "That code is not active yet.",
          coupon_already_used: "You have already used that code.",
          coupon_currency_mismatch: "That code is issued in a different currency.",
          coupon_no_discount: "That code gives no discount on this cart.",
          invalid_input: err.detail || "That code is not valid.",
        };
        ctx.toast(messages[err.code] ?? (err.detail || "The coupon could not be applied"), {
          tone: "danger",
          detail: err.code,
        });
      }
    }

    async function removeCoupon(): Promise<void> {
      try {
        await ctx.api.del("/api/v1/portal/cart/coupon");
        ctx.toast("Coupon removed", { tone: "success" });
        await reload();
      } catch (err) {
        reportCartError(err, "The coupon could not be removed");
      }
    }

    function reportCartError(err: unknown, fallback: string): void {
      if (!(err instanceof ApiError)) {
        ctx.toast(fallback, { tone: "danger", detail: "The server could not be reached." });
        return;
      }
      switch (err.code) {
        case "not_applicable":
          // The price's `allow_quantity` is off, or the cart currency and the price disagree.
          ctx.toast("That change is not possible", { tone: "danger", detail: err.detail });
          return;
        case "not_found":
          ctx.toast("That line is no longer in your cart", { tone: "danger", detail: "Reloading the cart." });
          void reload();
          return;
        case "portal_unavailable":
          ctx.toast("The portal is not available", { tone: "danger", detail: "Contact the operator." });
          return;
        default:
          ctx.toast(err.detail || fallback, { tone: "danger", detail: err.code });
      }
    }
  };

  paint();

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Your cart",
      sub: "What you have chosen, priced from the catalog as it stands right now. Nothing is charged until you place the order.",
      actions: btn("Keep shopping", { href: "/products", icon: "arrowLeft", size: "sm" }),
    }),
    host,
  );
}

/** A 404/503 here is a state, not a crash: the login has no billing account, or the portal is
 *  not configured on this deployment. */
function cartLoadError(err: unknown): unknown {
  if (err instanceof ApiError && err.code === "portal_unavailable") {
    return new Error(
      "The customer portal is not configured on this installation, so there is no cart to show. Contact the operator if you expected to buy something.",
    );
  }
  return portalLoadError(err);
}
