// The public storefront: what we sell, grouped by category.
//
// This is the only page in the app a stranger can see, so it is built to three rules:
//
//  - It reads one public endpoint and nothing else. No session, no staff data, no id that
//    isn't already public. The server decides what is visible; this page renders what it is
//    given and never guesses at a hidden product.
//  - A product with no logo is not a broken image. The card falls back to a generated mark
//    from the product's initial, so a half-filled catalog still looks deliberate.
//  - It is a storefront, not a CMS. Category and sort order come from the catalog; the layout
//    is fixed, because a page builder here would be a second product to maintain.

import type { Api } from "../lib/api";
import { h, swap } from "../lib/dom";
import { money, type Amount } from "../lib/format";
import { currentLocale, enabledLocales, languageSwitcher, onLocaleChange, setLocale, tx } from "../lib/i18n";
import { brandMark, icon } from "../lib/icons";
import type { AuthCtx } from "../lib/types";
import { badge, btn, emptyState, errorState, skeletonPage } from "../ui/kit";

export const meta = { title: "Products", crumb: "Products" };

interface PublicPrice {
  id: string;
  version: number;
  name: string;
  currency: string;
  amount: Amount;
  setup: Amount;
  interval: string;
}

interface PublicProduct {
  slug: string;
  name: string;
  short_description: string;
  description: string;
  category_name: string;
  category_slug: string;
  image_url: string;
  prices: PublicPrice[];
}

interface PublicCategory {
  id: string;
  slug: string;
  name: string;
  description: string;
  product_count: number;
}

interface CatalogResponse {
  categories: PublicCategory[];
  products: PublicProduct[];
}

/** The cheapest price, which is what a card advertises. */
function fromPrice(p: PublicProduct): PublicPrice | null {
  if (p.prices.length === 0) return null;
  return [...p.prices].sort((a, b) => a.amount.minor - b.amount.minor)[0] ?? null;
}

export default async function storefrontPage(ctx: AuthCtx): Promise<Node> {
  const host = h("div", { class: "store" }, skeletonPage());
  const controller = new AbortController();
  let activeCategory = ctx.query.get("category") ?? "";
  // The last response, kept so a language switch is a re-render rather than a refetch: the
  // catalogue is the operator's data, not the visitor's language, so nothing about it changes
  // when the language does.
  let latest: CatalogResponse | null = null;

  const load = async () => {
    try {
      const data = await ctx.api.get<CatalogResponse>("/api/v1/catalog", {
        cache: false,
        signal: controller.signal,
      });
      latest = data;
      swap(host, render(data));
    } catch (err) {
      if (controller.signal.aborted) return;
      swap(
        host,
        errorState(err, () => {
          swap(host, skeletonPage());
          void load();
        }),
      );
    }
  };

  const render = (data: CatalogResponse): Node => {
    const categories = data.categories ?? [];
    const products = data.products ?? [];
    const shown = activeCategory ? products.filter((p) => p.category_slug === activeCategory) : products;

    const header = h(
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

    const hero = h(
      "section",
      { class: "store-hero" },
      h("p", { class: "store-kicker" }, tx("storefront.hero.kicker", "Infrastructure, billed honestly")),
      h("h1", { class: "store-title" }, tx("storefront.hero.title", "Servers that come with a ledger.")),
      h(
        "p",
        { class: "store-sub" },
        tx(
          "storefront.hero.body",
          "Pick a plan, pay in a currency you recognise, and watch the invoice and the server agree. No surprise renewals, no drifted totals.",
        ),
      ),
      h(
        "div",
        { class: "store-hero-actions" },
        btn(tx("storefront.hero.plans", "See the plans"), { href: "#plans", variant: "primary", size: "lg" }),
        btn(tx("storefront.nav.client_area", "Client area"), { href: "/login", size: "lg" }),
      ),
    );

    const filterRow =
      categories.length > 0
        ? h(
            "div",
            {
              class: "store-filters",
              role: "tablist",
              "aria-label": tx("storefront.categories.label", "Categories"),
            },
            h(
              "button",
              {
                class: `chip${activeCategory === "" ? " is-active" : ""}`,
                type: "button",
                role: "tab",
                "aria-selected": activeCategory === "" ? "true" : "false",
                onclick: () => selectCategory(""),
              },
              tx("storefront.categories.all", "All products"),
              h("span", { class: "chip-count" }, String(products.length)),
            ),
            ...categories.map((c) =>
              h(
                "button",
                {
                  class: `chip${activeCategory === c.slug ? " is-active" : ""}`,
                  type: "button",
                  role: "tab",
                  "aria-selected": activeCategory === c.slug ? "true" : "false",
                  onclick: () => selectCategory(c.slug),
                },
                c.name,
                h("span", { class: "chip-count" }, String(c.product_count ?? 0)),
              ),
            ),
          )
        : null;

    const grid =
      shown.length === 0
        ? emptyState({
            title: activeCategory
              ? tx("storefront.empty.category_title", "Nothing in this category yet")
              : tx("storefront.empty.catalog_title", "The catalog is empty"),
            body: activeCategory
              ? tx(
                  "storefront.empty.category_body",
                  "Every product in this category is either unpublished or has no price. Try another category.",
                )
              : tx(
                  "storefront.empty.catalog_body",
                  "No product is published yet. An operator publishes one from the catalog screen.",
                ),
            icon: "box",
            action: activeCategory
              ? btn(tx("storefront.empty.show_all", "Show everything"), {
                  onclick: () => selectCategory(""),
                })
              : null,
          })
        : h("div", { class: "store-grid" }, ...shown.map(productCard));

    return h(
      "div",
      { class: "store-page" },
      header,
      hero,
      h(
        "section",
        { class: "store-section", id: "plans" },
        h(
          "div",
          { class: "store-section-head" },
          h(
            "h2",
            null,
            activeCategory
              ? (categories.find((c) => c.slug === activeCategory)?.name ?? tx("storefront.nav.products", "Products"))
              : tx("storefront.section.all", "All products"),
          ),
          h(
            "p",
            { class: "dim" },
            activeCategory
              ? (categories.find((c) => c.slug === activeCategory)?.description ?? "")
              : tx("storefront.section.sub", "Every plan we sell, with its starting price."),
          ),
        ),
        filterRow,
        grid,
      ),
      footer(),
    );
  };

  const selectCategory = (slug: string) => {
    activeCategory = slug;
    const url = new URL(window.location.href);
    if (slug) url.searchParams.set("category", slug);
    else url.searchParams.delete("category");
    window.history.replaceState({}, "", url.pathname + url.search);
    // Re-render from the data already in hand: a category filter is a view over one response,
    // not another round trip.
    void load();
  };

  const productCard = (p: PublicProduct): HTMLElement => {
    const price = fromPrice(p);
    return h(
      "a",
      { class: "store-card", href: `/products/${encodeURIComponent(p.slug)}` },
      h(
        "div",
        { class: "store-card-logo" },
        p.image_url
          ? h("img", { src: p.image_url, alt: "", loading: "lazy", decoding: "async" })
          : h("span", { class: "store-card-initial" }, p.name.charAt(0).toUpperCase()),
      ),
      h(
        "div",
        { class: "store-card-body" },
        h("div", { class: "store-card-cat" }, p.category_name || tx("storefront.card.product", "Product")),
        h("h3", { class: "store-card-name" }, p.name),
        h(
          "p",
          { class: "store-card-desc" },
          p.short_description || p.description || tx("storefront.card.no_description", "No description yet."),
        ),
      ),
      h(
        "div",
        { class: "store-card-foot" },
        price
          ? h(
              "span",
              { class: "store-price" },
              h("span", { class: "store-price-from" }, tx("storefront.card.from", "from")),
              h("span", { class: "money" }, h("span", { class: "money-code" }, price.currency), money(price.amount)),
              h("span", { class: "store-price-interval" }, `/ ${price.interval}`),
            )
          : h("span", { class: "faint" }, tx("storefront.card.pricing_on_request", "Pricing on request")),
        h("span", { class: "store-card-go" }, icon("arrowRight", 14)),
      ),
    );
  };

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
        h("a", { href: "/healthz", "data-native": "true" }, tx("storefront.footer.status", "Status")),
      ),
    );

  /**
   * The storefront's own language control (SF-19).
   *
   * It is here rather than only in the panel's shell because a visitor reading the storefront
   * has no shell and no account: the header is the only place they can choose, and the choice
   * has to survive a reload. `setLocale` persists it and notifies; the subscription below
   * re-renders this page in place, so the visitor keeps their scroll position and the category
   * they had filtered to.
   */
  const languageSelect = (): HTMLElement | null =>
    languageSwitcher({
      locales: enabledLocales(),
      selected: currentLocale(),
      label: tx("storefront.language.label", "Language"),
      id: "storefront-locale",
      onSelect: (code) => void setLocale(code),
    });

  const unsubscribe = onLocaleChange(() => {
    // A page that has been replaced must stop listening: the listener set is module-level and
    // would otherwise keep a detached tree alive and re-render it for nobody.
    if (!host.isConnected) {
      unsubscribe();
      return;
    }
    if (latest) swap(host, render(latest));
  });

  void load();
  return host;
}

/** Shared with the product page so the two cannot drift. */
export type { PublicProduct, PublicPrice, PublicCategory, CatalogResponse };
export { fromPrice };
export type { Api };
