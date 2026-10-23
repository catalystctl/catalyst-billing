// One product, for a visitor.
//
// The page answers the three questions a buyer has — what is it, what does it cost, how do I
// buy it — and it answers the third one by *selling*: "Order this plan" puts that price version
// in the cart. It used to be a link to `/login?next=/portal`, which forgot the product on the
// way, so a customer who clicked it landed on an empty portal with nothing in the cart and no
// way to add one from anywhere in the UI. The funnel had no first step.
//
// The page serves two audiences and says the same thing to both:
//
//  - **A visitor** is sent to sign in with the chosen plan in `next`, and the page adds it when
//    they come back. An order belongs to an account, so a guest checkout would be a second,
//    unowned path into the ledger; carrying the choice through the sign-in page is the honest
//    way to keep the intent without inventing that path.
//  - **A signed-in customer** adds it and goes to the cart, which is where the price, the
//    period and the total are shown from the server's own pricing.
//
// Adding the same plan twice merges into one line with a larger quantity: `cart_items` is
// unique on (cart, price version, config) and `AddItem` upserts with `quantity + EXCLUDED`,
// so a double click is two of the same thing rather than two lines of one.

import { ApiError } from "../lib/api";
import { appliedBrand, applyBrandChrome } from "../lib/brand";
import { h, swap } from "../lib/dom";
import { money, type Amount } from "../lib/format";
import { currentLocale, enabledLocales, languageSwitcher, onLocaleChange, setLocale, tx } from "../lib/i18n";
import { brandMark, icon } from "../lib/icons";
import { descriptionBody } from "../lib/richtext";
import { loadSession } from "../lib/session";
import type { AuthCtx, Session } from "../lib/types";
import { badge, btn, errorState, skeletonPage } from "../ui/kit";

export const meta = { title: "Product" };

interface Price {
  id: string;
  version: number;
  name: string;
  currency: string;
  amount: Amount;
  setup: Amount;
  interval: string;
  // The server decides both: the wording, so a new interval is not a client release, and
  // whether the plan bills again, so a one-time product is not described as a subscription.
  interval_label?: string;
  recurring?: boolean;
  sort_order?: number;
}

interface Product {
  slug: string;
  name: string;
  short_description: string;
  description: string;
  description_format?: string;
  category_name: string;
  category_slug: string;
  image_url: string;
  prices: Price[];
}

// A fallback for an interval the server did not label. The server's `interval_label` is what is
// normally shown, because it knows about intervals this file has never heard of. The labels
// here are translated because this is the path a visitor reads when the server sent none.
const INTERVAL_LABEL: Record<string, string> = {
  monthly: "per month",
  quarterly: "per quarter",
  semiannual: "every six months",
  annual: "per year",
  one_time: "one-time",
  free: "free",
};

function intervalLabel(price: Price): string {
  if (price.interval_label) return price.interval_label;
  const fallback = INTERVAL_LABEL[price.interval];
  return fallback ? tx(`storefront.interval.${price.interval}`, fallback) : price.interval;
}

export default async function storefrontProductPage(ctx: AuthCtx): Promise<Node> {
  const slug = ctx.params.slug ?? window.location.pathname.split("/").pop() ?? "";
  const host = h("div", { class: "store" }, skeletonPage());
  const controller = new AbortController();
  // Kept so a language switch re-renders the page from the response already in hand.
  let latest: Product | null = null;
  // The signed-in customer, or null for a visitor. This is what the buy button branches on:
  // the page is public, so it cannot assume either audience.
  let session: Session | null = null;
  // Which plan the button orders. `null` means "not chosen yet", resolved to the cheapest one
  // when the page is first built so the button is never a question without an answer.
  let selected: string | null = null;
  // A plan carried back from the sign-in page (`?add=<price version>`). Consumed once, after
  // the page has rendered, so a failure is reported on the page the customer was using.
  let pendingAdd: string | null = ctx.query.get("add");

  const load = async () => {
    try {
      // The product and the identity, in parallel. `loadSession` also installs the CSRF token a
      // mutation needs, and it answers null for a visitor rather than throwing.
      const [product, me] = await Promise.all([
        ctx.api.get<Product>(`/api/v1/catalog/${encodeURIComponent(slug)}`, {
          cache: false,
          signal: controller.signal,
        }),
        loadSession(ctx.api).catch(() => null),
      ]);
      latest = product;
      session = me;
      swap(host, render(product));
      // The header is part of this page, so it is decorated with the tenant's brand here: the
      // shell applied the brand before this view fetched its data, and this swap replaced the
      // markup it had decorated.
      applyBrandChrome(host, appliedBrand());
      if (pendingAdd) {
        const wanted = pendingAdd;
        pendingAdd = null;
        const price = (product.prices ?? []).find((candidate) => candidate.id === wanted);
        if (price) await addToCart(product, price);
      }
    } catch (err) {
      if (controller.signal.aborted) return;
      if (err instanceof ApiError && err.status === 404) {
        swap(host, notFound());
        return;
      }
      swap(host, errorState(err, () => void load()));
    }
  };

  const notFound = (): Node =>
    h(
      "div",
      { class: "store-page" },
      header(),
      h(
        "div",
        { class: "store-section" },
        h("h1", { class: "store-title" }, tx("storefront.product.missing_title", "That product is not available")),
        h(
          "p",
          { class: "store-sub" },
          tx(
            "storefront.product.missing_body",
            "It may have been unpublished, or the link may be out of date. The catalog lists everything currently for sale.",
          ),
        ),
        h(
          "div",
          { class: "row" },
          btn(tx("storefront.product.see_all", "See all products"), { href: "/", variant: "primary" }),
        ),
      ),
      footer(),
    );

  const header = (): HTMLElement =>
    h(
      "header",
      { class: "store-head" },
      h(
        "a",
        { class: "store-brand", href: "/" },
        h("span", { class: "brand-mark" }, brandMark(18)),
        h("span", { class: "brand-name" }, "Catalyst", h("small", null, "Hosting")),
      ),
      h(
        "nav",
        { class: "store-nav" },
        h("a", { href: "/products" }, tx("storefront.nav.products", "Products")),
        // The cart is in the storefront's own navigation because it is the second step of the
        // funnel. A visitor who clicks it is asked to sign in and brought back to it.
        h("a", { href: "/cart" }, tx("storefront.nav.cart", "Cart")),
        session ? null : h("a", { href: "/login" }, tx("storefront.nav.signin", "Sign in")),
        languageSelect(),
        // The way back out. An operator gets the panel, a customer the portal, a visitor the
        // sign-in page — the storefront is public for all three, so it has to know which one it
        // is looking at.
        h(
          "a",
          { class: "btn btn--primary btn--sm", href: session && session.user.is_staff ? "/dashboard" : "/portal" },
          session && session.user.is_staff
            ? tx("storefront.nav.dashboard", "Dashboard")
            : tx("storefront.nav.client_area", "Client area"),
        ),
      ),
    );

  const footer = (): HTMLElement =>
    h(
      "footer",
      { class: "store-foot" },
      h("span", null, "Catalyst Billing"),
      h(
        "span",
        { class: "store-foot-links" },
        h("a", { href: "/login" }, tx("storefront.footer.client_area", "Client area")),
        h("a", { href: "/api/v1/catalog", "data-native": "true" }, tx("storefront.footer.catalog_api", "Catalog API")),
      ),
    );

  /** The same control the index has, built by the same helper. */
  const languageSelect = (): HTMLElement | null =>
    languageSwitcher({
      locales: enabledLocales(),
      selected: currentLocale(),
      label: tx("storefront.language.label", "Language"),
      id: "storefront-locale",
      onSelect: (code) => void setLocale(code),
    });

  /** Where a refused add is reported: inside the buy card, next to the button that was pressed.
   *  Created once and re-attached by each render, so a message survives a language switch. */
  const orderError = h("div");

  /** One plan, as a row the customer can pick.
   *
   *  A radio per row rather than a separate selector: the list is the merchandising (name,
   *  period, price, setup fee) and it is what the customer reads, so the control that picks a
   *  plan belongs on the row it picks. A single-plan product needs no control at all. */
  const planRow = (product: Product, price: Price, pickable: boolean): HTMLElement => {
    const radio = pickable
      ? h("input", {
          type: "radio",
          name: "plan",
          class: "store-plan-radio",
          value: price.id,
          checked: selected === price.id,
          "aria-label": `${product.name} — ${price.name || "standard price"}`,
        })
      : null;
    if (radio) {
      radio.addEventListener("change", () => {
        selected = price.id;
      });
    }
    return h(
      "label",
      { class: "store-plan", dataset: { priceId: price.id } },
      radio,
      h(
        "div",
        { class: "store-plan-main" },
        h("span", { class: "store-plan-name" }, price.name || `Version ${price.version}`),
        h("span", { class: "store-plan-interval" }, intervalLabel(price)),
      ),
      h(
        "div",
        { class: "store-plan-price" },
        h("span", { class: "money" }, h("span", { class: "money-code" }, price.currency), money(price.amount)),
        price.setup.minor > 0
          ? h(
              "span",
              { class: "store-plan-setup" },
              "+ ",
              money(price.setup),
              ` ${tx("storefront.product.setup", "setup")}`,
            )
          : null,
      ),
    );
  };

  /** The call to action. A button, not a link: it adds a line to the cart. */
  const orderButton = (product: Product, fallback: Price | undefined): HTMLButtonElement => {
    const el = btn(tx("storefront.product.order", "Order this plan"), {
      id: "product-order",
      variant: "primary",
      size: "lg",
      block: true,
      iconAfter: "arrowRight",
    }) as HTMLButtonElement;
    el.addEventListener("click", () => {
      const prices = product.prices ?? [];
      // Resolved at click time, not at render time: the customer may have picked another plan
      // after this button was built.
      const price = prices.find((candidate) => candidate.id === selected) ?? fallback;
      if (price) void addToCart(product, price, el);
    });
    return el;
  };

  /** Puts one plan in the cart — or carries it through sign-in when there is no session.
   *
   *  The request names a price version and a quantity and nothing else: the customer comes from
   *  the session and the amount from the catalog, inside the transaction that writes the cart.
   *  A refusal is shown where the button is rather than as a toast over a page already left. */
  const addToCart = async (product: Product, price: Price, button?: HTMLButtonElement): Promise<void> => {
    const signIn = (): void =>
      ctx.navigate(
        `/login?next=${encodeURIComponent(`/products/${encodeURIComponent(slug)}?add=${encodeURIComponent(price.id)}`)}`,
      );
    if (!session) {
      // A visitor: nothing is lost. The plan rides in `next`, and this page adds it on the way
      // back. That is the whole difference between this button and the login link it replaced.
      signIn();
      return;
    }
    if (button) {
      button.disabled = true;
      button.classList.add("is-busy");
    }
    orderError.replaceChildren();
    try {
      await ctx.api.post("/api/v1/portal/cart/items", {
        price_version_id: price.id,
        quantity: 1,
      });
      // The cart page reads the cart and its totals from the server, so the cached copy is
      // dropped rather than patched: a patched total would be a second pricer.
      ctx.api.invalidate("/api/v1/portal/cart");
      ctx.toast(tx("storefront.product.added", "Added to your cart"), {
        tone: "success",
        detail: `${product.name} · ${price.name || tx("storefront.product.standard_price", "Standard price")}`,
      });
      ctx.navigate("/cart");
    } catch (err) {
      if (button) {
        button.disabled = false;
        button.classList.remove("is-busy");
      }
      // The session ended between the render and the click. The answer is the same as for a
      // visitor: sign in, and come back with the plan still chosen.
      if (err instanceof ApiError && (err.status === 401 || err.status === 403)) {
        signIn();
        return;
      }
      reportAddError(err);
    }
  };

  /** One sentence per documented refusal, because each names something the customer can do. */
  const reportAddError = (err: unknown): void => {
    if (!(err instanceof ApiError)) {
      swap(
        orderError,
        h(
          "div",
          { class: "banner banner--danger" },
          icon("alertCircle", 15),
          h("span", null, tx("storefront.product.add_failed", "That plan could not be added. The server could not be reached.")),
        ),
      );
      return;
    }
    const messages: Record<string, string> = {
      not_applicable: tx(
        "storefront.product.add_not_applicable",
        "This plan cannot join your cart: it is priced in a different currency from the items already in it. Empty the cart and try again.",
      ),
      // A product sold one at a time. The cart refuses the second add rather than merging it
      // into a quantity the checkout would reject, so this is the message that keeps a double
      // click from producing a cart that cannot be paid for.
      already_in_cart: tx(
        "storefront.product.add_already",
        "This plan is sold one at a time, and it is already in your cart.",
      ),
      not_found: tx("storefront.product.add_gone", "That plan is no longer for sale."),
      portal_unavailable: tx(
        "storefront.product.add_no_portal",
        "This installation has no customer portal, so nothing can be bought here.",
      ),
    };
    const message =
      messages[err.code] ?? (err.detail || tx("storefront.product.add_failed", "That plan could not be added."));
    swap(
      orderError,
      h(
        "div",
        { class: "banner banner--danger" },
        icon("alertCircle", 15),
        h(
          "div",
          { class: "stack-sm" },
          h("strong", null, tx("storefront.product.add_failed_title", "Not added")),
          h("span", null, message),
          // The useful next step for the one refusal that means "you already did this": the
          // cart. A dead-end message would leave the customer clicking the same button again.
          err.code === "already_in_cart"
            ? h("a", { class: "linkish", href: "/cart" }, tx("storefront.product.go_to_cart", "Go to your cart"))
            : null,
        ),
      ),
    );
  };

  const render = (product: Product): Node => {
    // The plans arrive in the operator's order (`price_versions.sort_order`), and that order is
    // the merchandising decision: "Basic, Pro, Enterprise" is not a sort by price. The "cheapest"
    // is the default selection so the button is never a question without an answer; choosing a
    // row overrides it and does not disturb the list.
    const prices = product.prices ?? [];
    const cheapest = prices.reduce<Price | undefined>(
      (best, price) => (best === undefined || price.amount.minor < best.amount.minor ? price : best),
      undefined,
    );
    if (selected === null || !prices.some((price) => price.id === selected)) {
      selected = cheapest?.id ?? null;
    }
    const chosen = prices.find((price) => price.id === selected) ?? cheapest;
    const anyRecurring = prices.some((price) => price.recurring !== false);

    return h(
      "div",
      { class: "store-page" },
      header(),
      h(
        "div",
        { class: "store-product" },
        h(
          "div",
          { class: "store-product-main" },
          h(
            "a",
            { class: "store-back", href: "/products" },
            icon("chevronLeft", 13),
            tx("storefront.product.all_products", "All products"),
          ),
          h(
            "div",
            { class: "store-product-head" },
            h(
              "div",
              { class: "store-product-logo" },
              product.image_url
                ? h("img", { src: product.image_url, alt: "", decoding: "async" })
                : h("span", { class: "store-card-initial" }, product.name.charAt(0).toUpperCase()),
            ),
            h(
              "div",
              null,
              h("div", { class: "store-card-cat" }, product.category_name || tx("storefront.card.product", "Product")),
              h("h1", { class: "store-title store-title--sm" }, product.name),
              product.short_description
                ? h("p", { class: "store-sub" }, product.short_description)
                : null,
            ),
          ),
          product.description
            ? descriptionBody(product.description, product.description_format ?? "plain")
            : null,
          h(
            "div",
            { class: "store-includes" },
            include("checkCircle", tx("storefront.product.include_provisioned", "Provisioned automatically after payment")),
            include(
              "checkCircle",
              tx("storefront.product.include_invoice", "An invoice you can download and a ledger you can audit"),
            ),
            // A one-time product has no period to cancel at the end of, so promising one would
            // be a claim the checkout cannot honour.
            anyRecurring
              ? include("checkCircle", tx("storefront.product.include_cancel", "Cancel at the end of any paid period"))
              : null,
          ),
        ),
        h(
          "aside",
          { class: "store-product-buy" },
          h("div", { class: "store-buy-card" },
            h(
              "div",
              { class: "store-buy-label" },
              prices.length > 1
                ? tx("storefront.product.plans", "Plans")
                : tx("storefront.product.price", "Price"),
            ),
            prices.length === 0
              ? h("p", { class: "dim" }, tx("storefront.product.no_price", "This product has no price yet."))
              : h("div", { class: "store-plans" }, ...prices.map((price) => planRow(product, price, prices.length > 1))),
            prices.length === 0
              ? h(
                  "div",
                  { class: "stack-sm" },
                  btn(tx("storefront.product.contact", "Contact us"), {
                    href: `/login?next=${encodeURIComponent("/tickets/new")}`,
                    variant: "primary",
                    size: "lg",
                    block: true,
                  }),
                )
              : h(
                  "div",
                  { class: "stack-sm" },
                  orderError,
                  orderButton(product, chosen),
                  h(
                    "p",
                    { class: "store-buy-note" },
                    session
                      ? tx(
                          "storefront.product.buy_note_signed_in",
                          "It goes into your cart with the price and the period. Nothing is charged until you place the order.",
                        )
                      : tx(
                          "storefront.product.buy_note",
                          "You will sign in first — an order belongs to an account, so we can put the invoice and the server in the same place. The plan you pick here is kept.",
                        ),
                  ),
                ),
          ),
        ),
      ),
      footer(),
    );
  };

  const include = (iconName: "checkCircle", text: string) =>
    h("div", { class: "store-include" }, icon(iconName, 15), h("span", null, text));

  const unsubscribe = onLocaleChange(() => {
    if (!host.isConnected) {
      unsubscribe();
      return;
    }
    if (latest) swap(host, render(latest));
  });

  void load();
  return host;
}
