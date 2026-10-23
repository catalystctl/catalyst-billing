// The public storefront: what we sell, grouped by category.
//
// This is the only page in the app a stranger can see, so it is built to four rules:
//
//  - It reads public endpoints and nothing else. No session, no staff data, no id that
//    isn't already public. The server decides what is visible; this page renders what it is
//    given and never guesses at a hidden product.
//  - A product with no logo is not a broken image. The card falls back to a generated mark
//    from the product's initial, so a half-filled catalog still looks deliberate.
//  - It is a storefront, not a CMS. Category and sort order come from the catalog; the layout
//    is fixed, because a page builder here would be a second product to maintain.
//  - **A category has an address.** The strip is a set of links to `/products/{slug}`, not a
//    client-side filter on this page, because a filtered view has no URL to paste into a
//    support reply and no URL for a crawler to index. The server prunes the strip to the
//    top-level branches that actually have something in them (SF-01).
//
// The second read is the announcement feed: an operator's maintenance notice belongs where a
// visitor will see it before choosing a plan, which is here and not only behind the login. It is
// loaded separately and renders nothing when `announcements.enabled` is off, so the storefront
// is unchanged on an installation that never publishes one.
//
// The currency control is the third (SF-18). A price is denominated per currency and this
// installation has no exchange rate, so choosing EUR is not a conversion: it asks the server for
// the products that are *sold* in EUR, at their EUR prices. That is why the control changes the
// URL rather than patching the amounts in place — the list itself changes.

import type { Api } from "../lib/api";
import { appliedBrand, applyBrandChrome } from "../lib/brand";
import { h, swap } from "../lib/dom";
import { money, type Amount } from "../lib/format";
import { currentLocale, enabledLocales, languageSwitcher, onLocaleChange, setLocale, tx } from "../lib/i18n";
import { brandMark, icon } from "../lib/icons";
import type { AuthCtx } from "../lib/types";
import { loadAnnouncementFeed, storefrontAnnouncements } from "../ui/announcement-feed";
import { btn, emptyState, errorState, skeletonPage } from "../ui/kit";
import { loadCurrencies, type PublicCurrency } from "./cart";

export const meta = { title: "Products", crumb: "Products" };

interface PublicPrice {
  id: string;
  version: number;
  name: string;
  currency: string;
  amount: Amount;
  setup: Amount;
  interval: string;
  /** The server's wording for the interval. Rendering `interval` instead is how "every 45
   *  days" reaches a visitor as "every_45_days" (SF-14). */
  interval_label?: string;
  recurring?: boolean;
}

interface PublicProduct {
  slug: string;
  name: string;
  short_description: string;
  description: string;
  category_name: string;
  category_slug: string;
  image_url: string;
  featured?: boolean;
  stock?: number | null;
  in_stock?: boolean;
  low_stock?: boolean;
  prices: PublicPrice[];
}

export interface PublicCategory {
  id: string;
  slug: string;
  name: string;
  description: string;
  image_url?: string;
  parent_id?: string | null;
  depth?: number;
  path?: string;
  product_count: number;
  children?: PublicCategory[];
}

interface CatalogResponse {
  currency?: string;
  currency_switcher?: boolean;
  categories: PublicCategory[];
  products: PublicProduct[];
}

/** The cheapest price, which is what a card advertises. */
function fromPrice(p: PublicProduct): PublicPrice | null {
  if (!p.prices || p.prices.length === 0) return null;
  return [...p.prices].sort((a, b) => a.amount.minor - b.amount.minor)[0] ?? null;
}

/** intervalLabel renders the server's wording, falling back to the raw interval.
 *
 *  The server is the only thing that knows every period the system can store (a day count, a
 *  one-time charge, a free plan), so the label is its answer rather than a table here. The
 *  fallback is for an older payload, not a normal path. */
export function intervalLabel(price: PublicPrice): string {
  if (price.interval_label) return price.interval_label;
  return price.interval;
}

/** A product card, shared with the category page so the two cannot look different. */
export function productCard(p: PublicProduct, currency: string): HTMLElement {
  const price = fromPrice(p);
  const href = currency
    ? `/products/${encodeURIComponent(p.slug)}?currency=${encodeURIComponent(currency)}`
    : `/products/${encodeURIComponent(p.slug)}`;
  const soldOut = p.in_stock === false;
  return h(
    "a",
    { class: "store-card", href, dataset: { slug: p.slug } },
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
      h(
        "div",
        { class: "store-card-cat" },
        p.category_name || tx("storefront.card.product", "Product"),
        p.featured ? h("span", { class: "dot-sep" }, "·") : null,
        p.featured ? h("span", null, tx("storefront.card.featured", "Featured")) : null,
        soldOut ? h("span", { class: "dot-sep" }, "·") : null,
        soldOut ? h("span", null, tx("storefront.card.sold_out", "Sold out")) : null,
      ),
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
            h("span", { class: "store-price-interval" }, `/ ${intervalLabel(price)}`),
          )
        : h("span", { class: "faint" }, tx("storefront.card.pricing_on_request", "Pricing on request")),
      h("span", { class: "store-card-go" }, icon("arrowRight", 14)),
    ),
  );
}

/** The grid of product cards. */
export function productGrid(products: PublicProduct[], currency: string): HTMLElement {
  return h("div", { class: "store-grid" }, ...products.map((p) => productCard(p, currency)));
}

export default async function storefrontPage(ctx: AuthCtx): Promise<Node> {
  const host = h("div", { class: "store" }, skeletonPage());
  // The identity, if there is one. The storefront is public for everyone — that is the whole
  // point of moving it to `/` — so the header must offer the right door out of it: the panel for
  // an operator, the portal for a customer, sign-in for a stranger. Captured once, at render
  // time, because a session does not change under a page that has already painted.
  const viewer = ctx.viewer;
  const areaHref = viewer ? (viewer.user.is_staff ? "/dashboard" : "/portal") : "/login";
  const areaLabel = viewer
    ? viewer.user.is_staff
      ? tx("storefront.nav.dashboard", "Dashboard")
      : tx("storefront.nav.account", "My account")
    : tx("storefront.nav.client_area", "Client area");
  const controller = new AbortController();
  // The last response, kept so a language switch is a re-render rather than a refetch: the
  // catalogue is the operator's data, not the visitor's language, so nothing about it changes
  // when the language does.
  let latest: CatalogResponse | null = null;
  // The currency the visitor is shopping in, from the URL. It is a property of the address
  // rather than of the page, so the cart's switcher can link straight to it.
  let currency = (ctx.query.get("currency") ?? "").toUpperCase();
  // The currencies the operator offers, for the switcher. Loaded in parallel with the catalogue
  // and allowed to fail: a shop still sells when the switcher cannot render.
  let currencies: PublicCurrency[] = [];
  // The announcement feed, once loaded. It is a separate request from the catalogue because a
  // notice must not be able to delay or break what the storefront is for, and it is kept as a
  // node rather than as data because the page re-renders from the response already in hand.
  let feed: HTMLElement | null = null;

  const loadFeed = async () => {
    try {
      // `?placement=storefront` asks for what a visitor may see: an announcement placed on the
      // customer dashboard only is not a storefront notice, and the server decides that.
      const rows = await loadAnnouncementFeed(ctx.api, "storefront");
      feed = storefrontAnnouncements(rows);
      if (feed !== null && latest !== null) swap(host, render(latest));
    } catch {
      // A notice is decoration. The storefront renders without it, exactly as it does when the
      // operator has switched the feed off.
    }
  };

  const load = async () => {
    try {
      const [data, rows] = await Promise.all([
        ctx.api.get<CatalogResponse>("/api/v1/catalog", {
          cache: false,
          signal: controller.signal,
          // The query is a parameter rather than a concatenated URL so the request is the same
          // one the API documents; an empty currency is dropped by the client.
          query: { currency },
        }),
        loadCurrencies(ctx.api, controller.signal).catch(() => [] as PublicCurrency[]),
      ]);
      latest = data;
      currencies = rows;
      // The server echoes the currency it actually used, which is the honest value: a code the
      // price table does not know is ignored there rather than guessed at here.
      currency = data.currency ?? currency;
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

    // The header is built branded rather than decorated afterwards. The shop re-renders itself
    // when the catalogue arrives and when the language changes, so a decoration applied to the
    // previous markup would be lost on every one of those swaps; `applyBrandChrome` is idempotent
    // and is called on the freshly built header instead. It is a no-op when no brand is set, so
    // an unbranded shop renders the built-in wordmark exactly as before.
    const brandHeader = h(
      "a",
      { class: "store-brand", href: "/" },
      h("span", { class: "brand-mark" }, brandMark(18)),
      h("span", { class: "brand-name" }, "Catalyst", h("small", null, "Hosting")),
    );
    applyBrandChrome(brandHeader, appliedBrand());

    const header = h(
      "header",
      { class: "store-head" },
      brandHeader,
      h(
        "nav",
        { class: "store-nav" },
        h("a", { href: "/products" }, tx("storefront.nav.products", "Products")),
        h("a", { href: "/cart" }, tx("storefront.nav.cart", "Cart")),
        // Sign-in is offered to visitors only. A signed-in visitor gets the way back to their own
        // screens instead, because a shop with no exit is a shop an operator stops opening.
        viewer ? null : h("a", { href: "/login" }, tx("storefront.nav.signin", "Sign in")),
        currencySelect(data),
        languageSelect(),
        h("a", { class: "btn btn--primary btn--sm", href: areaHref }, areaLabel),
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
        btn(areaLabel, { href: areaHref, size: "lg" }),
      ),
    );

    // The strip. "All products" stays a filter of this page (it is the page), and every category
    // is its own address so it can be linked, bookmarked and crawled.
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
              "a",
              {
                class: "chip is-active",
                href: currency ? `/products?currency=${encodeURIComponent(currency)}` : "/products",
                role: "tab",
                "aria-selected": "true",
                dataset: { category: "" },
              },
              tx("storefront.categories.all", "All products"),
              h("span", { class: "chip-count" }, String(products.length)),
            ),
            ...categories.map((c) =>
              h(
                "a",
                {
                  class: "chip",
                  href: currency
                    ? `/products/${encodeURIComponent(c.slug)}?currency=${encodeURIComponent(currency)}`
                    : `/products/${encodeURIComponent(c.slug)}`,
                  role: "tab",
                  "aria-selected": "false",
                  dataset: { category: c.slug },
                },
                c.name,
                h("span", { class: "chip-count" }, String(c.product_count ?? 0)),
              ),
            ),
          )
        : null;

    // The promotion row (SF-03). It is a separate row rather than a re-sort, so `sort_order` stays
    // the one authority on what comes first in a category: an operator who said "this one is
    // promoted" gets it at the top of the page, and an operator who said "this order" still does.
    const featured = products.filter((p) => p.featured === true);
    const featuredRow =
      featured.length === 0
        ? null
        : h(
            "div",
            { class: "store-section" },
            h(
              "div",
              { class: "store-section-head" },
              h("h2", null, tx("storefront.section.featured", "Featured")),
              h("p", { class: "dim" }, tx("storefront.section.featured_sub", "What we are pushing this month.")),
            ),
            productGrid(featured, currency),
          );

    const grid =
      products.length === 0
        ? emptyState({
            title: currency
              ? tx("storefront.empty.currency_title", "Nothing is sold in this currency yet")
              : tx("storefront.empty.catalog_title", "The catalog is empty"),
            body: currency
              ? tx(
                  "storefront.empty.currency_body",
                  "No published product has a price in this currency. A price is set per currency, so try another one.",
                )
              : tx(
                  "storefront.empty.catalog_body",
                  "No product is published yet. An operator publishes one from the catalog screen.",
                ),
            icon: "box",
            action: currency
              ? btn(tx("storefront.empty.show_default", "Show every currency"), { href: "/products" })
              : null,
          })
        : productGrid(products, currency);

    return h(
      "div",
      { class: "store-page" },
      header,
      hero,
      // Between the pitch and the catalogue: a visitor reads "we are closed on Sunday" before
      // deciding on a plan, and a returning customer sees it without signing in.
      feed,
      featuredRow,
      h(
        "section",
        { class: "store-section", id: "plans" },
        h(
          "div",
          { class: "store-section-head" },
          h("h2", null, tx("storefront.section.all", "All products")),
          h(
            "p",
            { class: "dim" },
            currency
              ? tx("storefront.section.sub_currency", "Every plan we sell in this currency.")
              : tx("storefront.section.sub", "Every plan we sell, with its starting price."),
          ),
        ),
        filterRow,
        grid,
      ),
      footer(),
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
        h("a", { href: areaHref }, tx("storefront.footer.client_area", "Client area")),
        h("a", { href: "/api/v1/catalog", "data-native": "true" }, tx("storefront.footer.catalog_api", "Catalog API")),
        h("a", { href: "/healthz", "data-native": "true" }, tx("storefront.footer.status", "Status")),
      ),
    );

  /**
   * The currency control (SF-18).
   *
   * It changes the URL rather than the rendered amounts, because there is no exchange rate:
   * choosing EUR asks the server for the products that are sold in EUR. The control is absent
   * when the operator switched it off or when there is nothing to switch between — a picker with
   * one option is a control that cannot do anything.
   */
  const currencySelect = (data: CatalogResponse): HTMLElement | null => {
    if (data.currency_switcher === false || currencies.length < 2) return null;
    const selected = (currency || data.currency || "").toUpperCase();
    return h(
      "select",
      {
        class: "select",
        id: "storefront-currency",
        "aria-label": tx("storefront.currency.label", "Currency"),
        onchange: (event: Event) => {
          const next = (event.target as HTMLSelectElement).value;
          if (!next) return;
          const url = new URL(window.location.href);
          url.searchParams.set("currency", next);
          // A full navigation, not a query patch: the catalogue itself changes, and the server
          // is what decides which products this visitor may see.
          window.location.assign(url.pathname + url.search);
        },
      },
      ...currencies.map((row) => {
        const option = h("option", { value: row.code }, `${row.code} — ${row.name}`);
        if (row.code.toUpperCase() === selected) option.selected = true;
        return option;
      }),
    );
  };

  /**
   * The storefront's own language control (SF-19).
   *
   * It is here rather than only in the panel's shell because a visitor reading the storefront
   * has no shell and no account: the header is the only place they can choose, and the choice
   * has to survive a reload. `setLocale` persists it and notifies; the subscription below
   * re-renders this page in place, so the visitor keeps their scroll position and the currency
   * they picked.
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
  void loadFeed();
  return host;
}

/** Shared with the product page so the two cannot drift. */
export type { PublicProduct, PublicPrice, CatalogResponse };
export type { Api };
