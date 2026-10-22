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
import { brandMark, icon } from "../lib/icons";
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
}

interface Product {
  slug: string;
  name: string;
  short_description: string;
  description: string;
  category_name: string;
  category_slug: string;
  image_url: string;
  prices: Price[];
}

const INTERVAL_LABEL: Record<string, string> = {
  monthly: "per month",
  quarterly: "per quarter",
  semiannual: "every six months",
  annual: "per year",
};

export default async function storefrontProductPage(ctx: AuthCtx): Promise<Node> {
  const slug = ctx.params.slug ?? window.location.pathname.split("/").pop() ?? "";
  const host = h("div", { class: "store" }, skeletonPage());
  const controller = new AbortController();

  const load = async () => {
    try {
      const product = await ctx.api.get<Product>(`/api/v1/catalog/${encodeURIComponent(slug)}`, {
        cache: false,
        signal: controller.signal,
      });
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
        h("h1", { class: "store-title" }, "That product is not available"),
        h(
          "p",
          { class: "store-sub" },
          "It may have been unpublished, or the link may be out of date. The catalog lists everything currently for sale.",
        ),
        h("div", { class: "row" }, btn("See all products", { href: "/products", variant: "primary" })),
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
        h("a", { href: "/products" }, "Products"),
        h("a", { href: "/login" }, "Sign in"),
        h("a", { class: "btn btn--primary btn--sm", href: "/login" }, "Client area"),
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
        h("a", { href: "/login" }, "Client area"),
        h("a", { href: "/api/v1/catalog", "data-native": "true" }, "Catalog API"),
      ),
    );

  const render = (product: Product): Node => {
    const prices = [...(product.prices ?? [])].sort((a, b) => a.amount.minor - b.amount.minor);
    const cheapest = prices[0];
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
            "All products",
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
              h("div", { class: "store-card-cat" }, product.category_name || "Product"),
              h("h1", { class: "store-title store-title--sm" }, product.name),
              product.short_description
                ? h("p", { class: "store-sub" }, product.short_description)
                : null,
            ),
          ),
          product.description
            ? h("div", { class: "store-prose" }, ...product.description.split(/\n{2,}/).map((para) => h("p", null, para)))
            : null,
          h(
            "div",
            { class: "store-includes" },
            include("checkCircle", "Provisioned automatically after payment"),
            include("checkCircle", "An invoice you can download and a ledger you can audit"),
            include("checkCircle", "Cancel at the end of any paid period"),
          ),
        ),
        h(
          "aside",
          { class: "store-product-buy" },
          h("div", { class: "store-buy-card" },
            h("div", { class: "store-buy-label" }, prices.length > 1 ? "Plans" : "Price"),
            prices.length === 0
              ? h("p", { class: "dim" }, "This product has no price yet.")
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
                        h("span", { class: "store-plan-interval" }, INTERVAL_LABEL[price.interval] ?? price.interval),
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
                              " setup",
                            )
                          : null,
                      ),
                    ),
                  ),
                ),
            h(
              "div",
              { class: "stack-sm" },
              btn(cheapest ? "Order this plan" : "Contact us", {
                href: buyHref,
                variant: "primary",
                size: "lg",
                block: true,
              }),
              h(
                "p",
                { class: "store-buy-note" },
                "You will sign in first — an order belongs to an account, so we can put the invoice and the server in the same place.",
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

  void load();
  return host;
}
