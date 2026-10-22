// One product, for a visitor.
//
// The page answers the three questions a buyer has — what is it, what does it cost, how do I
// buy it — and refuses to invent an answer it does not have. "Buy" leads to the client area,
// because checkout requires a session: an order belongs to a customer, and a guest order would
// be a second, unowned path into the ledger. Saying so plainly is better than a button that
// fails at the payment step.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { money, type Amount } from "../lib/format";
import { currentLocale, enabledLocales, languageSwitcher, onLocaleChange, setLocale, tx } from "../lib/i18n";
import { brandMark, icon } from "../lib/icons";
import { descriptionBody } from "../lib/richtext";
import type { AuthCtx } from "../lib/types";
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

  const load = async () => {
    try {
      const product = await ctx.api.get<Product>(`/api/v1/catalog/${encodeURIComponent(slug)}`, {
        cache: false,
        signal: controller.signal,
      });
      latest = product;
      swap(host, render(product));
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
          btn(tx("storefront.product.see_all", "See all products"), { href: "/products", variant: "primary" }),
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
        h("a", { href: "/login" }, tx("storefront.nav.signin", "Sign in")),
        languageSelect(),
        h("a", { class: "btn btn--primary btn--sm", href: "/login" }, tx("storefront.nav.client_area", "Client area")),
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

  const render = (product: Product): Node => {
    // The plans arrive in the operator's order (`price_versions.sort_order`), and that order is
    // the merchandising decision: "Basic, Pro, Enterprise" is not a sort by price. The "cheapest"
    // used for the call to action is computed separately so it cannot disturb the list.
    const prices = product.prices ?? [];
    const cheapest = prices.reduce<Price | undefined>(
      (best, price) => (best === undefined || price.amount.minor < best.amount.minor ? price : best),
      undefined,
    );
    const anyRecurring = prices.some((price) => price.recurring !== false);
    const buyHref = `/login?next=${encodeURIComponent("/portal")}`;

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
              : h(
                  "div",
                  { class: "store-plans" },
                  ...prices.map((price) =>
                    h(
                      "div",
                      { class: "store-plan" },
                      h(
                        "div",
                        { class: "store-plan-main" },
                        h("span", { class: "store-plan-name" }, price.name || `Version ${price.version}`),
                        h("span", { class: "store-plan-interval" }, intervalLabel(price)),
                      ),
                      h(
                        "div",
                        { class: "store-plan-price" },
                        h(
                          "span",
                          { class: "money" },
                          h("span", { class: "money-code" }, price.currency),
                          money(price.amount),
                        ),
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
                    ),
                  ),
                ),
            h(
              "div",
              { class: "stack-sm" },
              btn(cheapest ? tx("storefront.product.order", "Order this plan") : tx("storefront.product.contact", "Contact us"), {
                href: buyHref,
                variant: "primary",
                size: "lg",
                block: true,
              }),
              h(
                "p",
                { class: "store-buy-note" },
                tx(
                  "storefront.product.buy_note",
                  "You will sign in first — an order belongs to an account, so we can put the invoice and the server in the same place.",
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
