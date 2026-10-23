// One storefront address, two pages: a product, or a category.
//
// `/products/{slug}` is one URL shape for both because a category has to be linkable (SF-22):
// the server resolves the slug and says which record it named (`kind`), and this view renders the
// page that record deserves. Doing it in one view also means the two share a header, a footer, a
// language control and a price card, so a shop cannot end up with two visual languages.
//
// The product page answers the three questions a buyer has — what is it, what does it cost, how do
// I buy it — and it answers the third one by *selling*: "Order this plan" puts that price version
// in the cart. It used to be a link to `/login?next=/portal`, which forgot the product on the
// way, so a customer who clicked it landed on an empty portal with nothing in the cart. The funnel
// had no first step.
//
// What a buyer now also sees is what would stop the purchase being possible (SF-03, SF-06):
//
//  - **Stock.** "Only 3 left" when the operator publishes the count, "Sold out" and a disabled
//    button when there are none. The refusal is the server's too; this is the explanation.
//  - **Options.** A product whose plan has a required option used to be unbuyable from here,
//    because the page had no way to choose one. The picker sends `config`, which is what the cart
//    and the checkout validate — the page never computes a price from an option, it only shows
//    the amount the server attached to the choice.
//  - **Quantity.** Only for a product whose `allow_quantity` policy takes more than one, and
//    never above the policy's ceiling: a field that the cart refuses is a trap.
//
// The page serves two audiences and says the same thing to both:
//
//  - **A visitor** is sent to sign in with the chosen plan in `next`, and the page adds it when
//    they come back. An order belongs to an account, so a guest checkout would be a second,
//    unowned path into the ledger; carrying the choice through the sign-in page is the honest
//    way to keep the intent without inventing that path.
//  - **A signed-in customer** adds it and goes to the cart, which is where the price, the
//    period and the total are shown from the server's own pricing.

import { ApiError } from "../lib/api";
import { appliedBrand, applyBrandChrome } from "../lib/brand";
import { h, swap } from "../lib/dom";
import { money, type Amount } from "../lib/format";
import { currentLocale, enabledLocales, languageSwitcher, onLocaleChange, setLocale, tx } from "../lib/i18n";
import { brandMark, icon } from "../lib/icons";
import { descriptionBody } from "../lib/richtext";
import { loadSession } from "../lib/session";
import type { AuthCtx, Session } from "../lib/types";
import { badge, btn, emptyState, errorState, field, select, skeletonPage } from "../ui/kit";
import { loadCurrencies, type PublicCurrency } from "./cart";
import {
  intervalLabel,
  productGrid,
  storefrontCurrencySelect,
  type PublicCategory,
  type PublicProduct,
} from "./storefront";

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

/** One configurable choice on a product, with the amount attached to each plan. */
interface ConfigOption {
  id: string;
  name: string;
  description?: string;
  type: "dropdown" | "radio" | "checkbox" | "text" | "number" | string;
  choices: string[];
  required: boolean;
  parent_id?: string | null;
  prices: Array<{ price_id: string; choice: string; amount: Amount; setup: Amount }>;
}

interface Product extends PublicProduct {
  description_format?: string;
  prices: Price[];
  config_options?: ConfigOption[];
  allow_quantity?: string;
  low_stock_threshold?: number;
}

/** The response for `/api/v1/catalog/{slug}`: a product, or the category of that name. */
interface CatalogResponse {
  kind?: "product" | "category";
  currency?: string;
  currency_switcher?: boolean;
  // Product page.
  slug?: string;
  name?: string;
  prices?: Price[];
  config_options?: ConfigOption[];
  // Category page.
  category?: PublicCategory;
  products?: PublicProduct[];
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

function priceIntervalLabel(price: Price): string {
  if (price.interval_label) return price.interval_label;
  const fallback = INTERVAL_LABEL[price.interval];
  return fallback ? tx(`storefront.interval.${price.interval}`, fallback) : price.interval;
}

/** setPageSEO gives a category page an address worth sharing: a title, and the description a
 *  crawler or a chat client shows under the link. The SPA cannot set a server-rendered tag, but
 *  the two that matter for a pasted link are in the document. */
function setPageSEO(title: string, description: string): void {
  document.title = `${title} · Catalyst Billing`;
  const meta = document.querySelector('meta[name="description"]');
  if (meta && description) meta.setAttribute("content", description);
}

export default async function storefrontProductPage(ctx: AuthCtx): Promise<Node> {
  const slug = ctx.params.slug ?? window.location.pathname.split("/").pop() ?? "";
  const currency = (ctx.query.get("currency") ?? "").toUpperCase();
  const host = h("div", { class: "store" }, skeletonPage());
  const controller = new AbortController();
  // Kept so a language switch re-renders the page from the response already in hand.
  let latest: CatalogResponse | null = null;
  // The signed-in customer, or null for a visitor. This is what the buy button branches on:
  // the page is public, so it cannot assume either audience.
  let session: Session | null = null;
  // Which plan the button orders. `null` means "not chosen yet", resolved to the cheapest one
  // when the page is first built so the button is never a question without an answer.
  let selected: string | null = null;
  // The chosen option values, keyed by option id. It survives a re-render, so switching the
  // language does not silently drop what the customer picked.
  const chosenOptions: Record<string, string> = {};
  // A plan carried back from the sign-in page (`?add=<price version>`). Consumed once, after
  // the page has rendered, so a failure is reported on the page the customer was using.
  let pendingAdd: string | null = ctx.query.get("add");
  // The offered currencies and whether the operator wants the switcher. A visitor who landed on a
  // product page is one click from the rest of the shop, so the control has to be here too.
  let currencies: PublicCurrency[] = [];
  let switcherEnabled = true;
  let servedCurrency = currency;

  const load = async () => {
    try {
      // The record and the identity, in parallel. `loadSession` also installs the CSRF token a
      // mutation needs, and it answers null for a visitor rather than throwing.
      const [data, me, rows] = await Promise.all([
        ctx.api.get<CatalogResponse>(`/api/v1/catalog/${encodeURIComponent(slug)}`, {
          cache: false,
          signal: controller.signal,
          query: { currency },
        }),
        loadSession(ctx.api).catch(() => null),
        loadCurrencies(ctx.api, controller.signal).catch(() => [] as PublicCurrency[]),
      ]);
      latest = data;
      session = me;
      currencies = rows;
      servedCurrency = (data.currency ?? currency).toUpperCase();
      switcherEnabled = data.currency_switcher !== false;
      swap(host, render(data));
      if (data.kind === "product" && pendingAdd) {
        const wanted = pendingAdd;
        pendingAdd = null;
        const price = (data.prices ?? []).find((candidate) => candidate.id === wanted);
        if (price) await addToCart(asProduct(data), price);
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

  /** asProduct narrows a product-shaped response for the functions that read it. */
  const asProduct = (data: CatalogResponse): Product =>
    ({
      ...data,
      slug: data.slug ?? slug,
      name: data.name ?? "",
      short_description: "",
      description: "",
      category_name: "",
      category_slug: "",
      image_url: "",
      prices: data.prices ?? [],
      config_options: data.config_options ?? [],
    }) as Product;

  const notFound = (): Node =>
    h(
      "div",
      { class: "store-page" },
      header(),
      h(
        "div",
        { class: "store-section" },
        h("h1", { class: "store-title" }, tx("storefront.product.missing_title", "That page is not available")),
        h(
          "p",
          { class: "store-sub" },
          tx(
            "storefront.product.missing_body",
            "It may have been unpublished, or the link may be out of date. The catalog lists everything currently for sale.",
          ),
        ),
        h("div", { class: "row" }, btn(tx("storefront.product.see_all", "See all products"), { href: "/", variant: "primary" })),
      ),
      footer(),
    );

  const header = (): HTMLElement => {
    // Built branded, for the reason given in storefront.ts: this page re-renders on a language
    // change, and a decoration on the previous markup would be lost. A no-op when unset.
    const brand = h(
      "a",
      { class: "store-brand", href: "/" },
      h("span", { class: "brand-mark" }, brandMark(18)),
      h("span", { class: "brand-name" }, "Catalyst", h("small", null, "Hosting")),
    );
    applyBrandChrome(brand, appliedBrand());
    return h(
      "header",
      { class: "store-head" },
      brand,
      h(
        "nav",
        { class: "store-nav" },
        h("a", { href: withCurrency("/products") }, tx("storefront.nav.products", "Products")),
        // The cart is in the storefront's own navigation because it is the second step of the
        // funnel. A visitor who clicks it is asked to sign in and brought back to it.
        h("a", { href: "/cart" }, tx("storefront.nav.cart", "Cart")),
        session ? null : h("a", { href: "/login" }, tx("storefront.nav.signin", "Sign in")),
        storefrontCurrencySelect(currencies, servedCurrency, switcherEnabled),
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
      ),
    );

  /** withCurrency keeps the visitor in the currency they chose across storefront links. */
  const withCurrency = (path: string): string => {
    const code = servedCurrency || currency;
    return code ? `${path}${path.includes("?") ? "&" : "?"}currency=${encodeURIComponent(code)}` : path;
  };

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

  // ------------------------------------------------------------- category page --

  /** The category page: the branch's own address, its children, and everything sold below it. */
  const renderCategory = (data: CatalogResponse): Node => {
    const category = data.category;
    const products = data.products ?? [];
    if (!category) return notFound();
    setPageSEO(
      category.name,
      category.description || tx("storefront.category.meta", "Products in this category."),
    );
    // The path is the slug chain, so a visitor can walk back up the tree rather than only
    // sideways — the reason nesting has an address at all.
    const slugs = (category.path || category.slug).split("/").filter(Boolean);
    const crumbs: Node[] = [h("a", { class: "store-back", href: withCurrency("/products") }, tx("storefront.nav.products", "Products"))];
    slugs.forEach((part, index) => {
      const upTo = slugs.slice(0, index + 1).join("/");
      const label = index === slugs.length - 1 ? category.name : part.replace(/-/g, " ");
      crumbs.push(h("span", { class: "dot-sep" }, "›"));
      crumbs.push(
        index === slugs.length - 1
          ? h("span", { class: "dim" }, label)
          : h("a", { class: "store-back", href: withCurrency(`/products/${encodeURIComponent(upTo)}`) }, label),
      );
    });

    const children = category.children ?? [];
    const childGrid =
      children.length === 0
        ? null
        : h(
            "div",
            { class: "store-grid" },
            ...children.map((child) =>
              h(
                "a",
                { class: "store-card", href: withCurrency(`/products/${encodeURIComponent(child.slug)}`), dataset: { category: child.slug } },
                h(
                  "div",
                  { class: "store-card-logo" },
                  child.image_url
                    ? h("img", { src: child.image_url, alt: "", loading: "lazy", decoding: "async" })
                    : h("span", { class: "store-card-initial" }, child.name.charAt(0).toUpperCase()),
                ),
                h(
                  "div",
                  { class: "store-card-body" },
                  h("div", { class: "store-card-cat" }, tx("storefront.category.subcategory", "Sub-category")),
                  h("h3", { class: "store-card-name" }, child.name),
                  h(
                    "p",
                    { class: "store-card-desc" },
                    child.description || tx("storefront.category.count", "Products in this branch."),
                  ),
                ),
                h(
                  "div",
                  { class: "store-card-foot" },
                  h(
                    "span",
                    { class: "dim" },
                    `${child.product_count ?? 0} ${tx("storefront.category.products", "products")}`,
                  ),
                  h("span", { class: "store-card-go" }, icon("arrowRight", 14)),
                ),
              ),
            ),
          );

    return h(
      "div",
      { class: "store-page" },
      header(),
      h(
        "section",
        { class: "store-section", id: "plans" },
        h("div", { class: "row" }, ...crumbs),
        // The category's own logo (SF-06). It is the same stored image the panel uploads, served
        // by the same media endpoint, so a category looks like the thing it sells rather than like
        // a heading.
        category.image_url
          ? h(
              "div",
              { class: "store-product-head" },
              h("div", { class: "store-product-logo" }, h("img", { src: category.image_url, alt: "", decoding: "async" })),
              h(
                "div",
                { class: "store-section-head" },
                h("h1", { class: "store-title store-title--sm" }, category.name),
                category.description ? h("p", { class: "dim" }, category.description) : null,
              ),
            )
          : h(
              "div",
              { class: "store-section-head" },
              h("h1", { class: "store-title store-title--sm" }, category.name),
              category.description ? h("p", { class: "dim" }, category.description) : null,
            ),
        childGrid,
        products.length === 0
          ? emptyState({
              title: tx("storefront.empty.category_title", "Nothing in this category yet"),
              body: tx(
                "storefront.empty.category_body",
                "Every product in this branch is either unpublished or has no price in this currency. Try another category.",
              ),
              icon: "box",
              action: btn(tx("storefront.empty.show_all", "Show everything"), { href: withCurrency("/products") }),
            })
          : productGrid(products, currency),
      ),
      footer(),
    );
  };

  // -------------------------------------------------------------- product page --

  /** The amount an option choice adds to one plan, or null when the choice is free. */
  const optionAmount = (option: ConfigOption, priceID: string, choice: string): Amount | null => {
    const hit = (option.prices ?? []).find((p) => p.price_id === priceID && p.choice === choice);
    return hit && hit.amount.minor > 0 ? hit.amount : null;
  };

  /** One option's control. The control is the option's type, because a dropdown that renders as
   *  free text is a value the checkout will refuse. */
  const optionControl = (product: Product, option: ConfigOption, planID: string): HTMLElement => {
    const name = `option-${option.id}`;
    const current = chosenOptions[option.id] ?? (option.prices?.find((p) => p.price_id === planID)?.choice ?? "");
    const insertNone = (option.required ? [] : [{ value: "", label: tx("storefront.product.no_thanks", "None") }]);

    if (option.type === "dropdown") {
      const options = [
        ...(option.required ? [] : [{ value: "", label: tx("storefront.product.choose", "Choose…") }]),
        ...option.choices.map((choice) => {
          const amount = optionAmount(option, planID, choice);
          return {
            value: choice,
            label: amount ? `${choice} (+ ${money(amount)})` : choice,
          };
        }),
      ];
      const el = select(options, { id: name, value: current });
      el.addEventListener("change", () => {
        chosenOptions[option.id] = el.value;
      });
      return el;
    }
    if (option.type === "radio") {
      const group = h("div", { class: "stack-sm" });
      for (const choice of option.choices) {
        const amount = optionAmount(option, planID, choice);
        const input = h("input", { type: "radio", name, value: choice, checked: current === choice });
        input.addEventListener("change", () => {
          chosenOptions[option.id] = choice;
        });
        group.append(
          h(
            "label",
            { class: "switch" },
            input,
            h("span", null, amount ? `${choice} (+ ${money(amount)})` : choice),
          ),
        );
      }
      return group;
    }
    if (option.type === "checkbox") {
      const input = h("input", { type: "checkbox", checked: current === option.choices[0] });
      input.addEventListener("change", () => {
        chosenOptions[option.id] = input.checked ? (option.choices[0] ?? "on") : "";
      });
      return h("label", { class: "switch" }, input, h("span", null, option.name));
    }
    // text and number: the value is whatever was typed, and the checkout validates it.
    const input = h("input", {
      class: "input",
      type: option.type === "number" ? "number" : "text",
      id: name,
      value: current,
    });
    input.addEventListener("input", () => {
      chosenOptions[option.id] = input.value;
    });
    return input;
  };

  const optionsCard = (product: Product, planID: string): HTMLElement | null => {
    const options = (product.config_options ?? []).filter((o) => !o.parent_id);
    if (options.length === 0) return null;
    return h(
      "div",
      { class: "stack-sm" },
      ...options.map((option) =>
        field({
          label: option.name,
          id: `option-${option.id}`,
          required: option.required,
          hint: option.description || undefined,
          control: optionControl(product, option, planID),
        }),
      ),
    );
  };

  /** The quantity field, when the product's policy takes more than one. */
  const quantityField = (product: Product): HTMLInputElement | null => {
    if ((product.allow_quantity ?? "disabled") === "disabled") return null;
    return h("input", {
      class: "input",
      type: "number",
      id: "product-quantity",
      value: 1,
      min: 1,
      max: 1000,
      "aria-label": tx("storefront.product.quantity", "Quantity"),
    });
  };

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
        // A plan change re-prices the options, so the card is rebuilt from the response we
        // already have rather than left showing amounts that belong to another plan.
        if (latest) swap(host, render(latest));
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
        h("span", { class: "store-plan-interval" }, priceIntervalLabel(price)),
      ),
      h(
        "div",
        { class: "store-plan-price" },
        h("span", { class: "money" }, h("span", { class: "money-code" }, price.currency), money(price.amount)),
        price.setup.minor > 0
          ? h("span", { class: "store-plan-setup" }, "+ ", money(price.setup), ` ${tx("storefront.product.setup", "setup")}`)
          : null,
      ),
    );
  };

  /** The call to action. A button, not a link: it adds a line to the cart. */
  const orderButton = (product: Product, fallback: Price | undefined): HTMLButtonElement => {
    const soldOut = product.in_stock === false;
    const el = btn(
      soldOut ? tx("storefront.product.sold_out", "Sold out") : tx("storefront.product.order", "Order this plan"),
      {
        id: "product-order",
        variant: "primary",
        size: "lg",
        block: true,
        iconAfter: "arrowRight",
      },
    ) as HTMLButtonElement;
    if (soldOut) {
      // The server would refuse this too; disabling is the explanation rather than the rule.
      el.disabled = true;
      return el;
    }
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
   *  The request names a price version, a quantity and the chosen options; the customer comes
   *  from the session and the amount from the catalog, inside the transaction that writes the
   *  cart. A refusal is shown where the button is rather than as a toast over a page already
   *  left. */
  const addToCart = async (product: Product, price: Price, button?: HTMLButtonElement): Promise<void> => {
    const signIn = (): void =>
      ctx.navigate(
        `/login?next=${encodeURIComponent(`${window.location.pathname}?add=${encodeURIComponent(price.id)}`)}`,
      );
    if (!session) {
      // A visitor: nothing is lost. The plan rides in `next`, and this page adds it on the way
      // back. That is the whole difference between this button and the login link it replaced.
      signIn();
      return;
    }
    const quantityEl = document.getElementById("product-quantity") as HTMLInputElement | null;
    const quantity = quantityEl ? Math.max(1, Math.min(1000, Number(quantityEl.value) || 1)) : 1;
    // Only the options that were actually answered are sent: an empty value is "no choice",
    // which the checkout treats as absent rather than as a choice named "".
    const config: Record<string, string> = {};
    for (const [key, value] of Object.entries(chosenOptions)) {
      if (value !== "") config[key] = value;
    }
    if (button) {
      button.disabled = true;
      button.classList.add("is-busy");
    }
    orderError.replaceChildren();
    try {
      await ctx.api.post("/api/v1/portal/cart/items", {
        price_version_id: price.id,
        quantity,
        config,
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

  /** The stock sentence. Silence when the product is unlimited, because "unlimited" is not a
   *  count and a visitor does not need to be told the shop is not running out. */
  const stockNotice = (product: Product): HTMLElement | null => {
    if (product.in_stock === false) {
      return h(
        "div",
        { class: "banner banner--warning" },
        icon("alertCircle", 15),
        h("span", null, tx("storefront.product.stock_out", "Sold out — this plan cannot be ordered right now.")),
      );
    }
    if (product.low_stock && typeof product.stock === "number") {
      return h(
        "p",
        { class: "store-buy-note" },
        `${tx("storefront.product.stock_left", "Only")} ${product.stock} ${tx("storefront.product.stock_left_unit", "left")}`,
      );
    }
    return null;
  };

  const renderProduct = (product: Product): Node => {
    setPageSEO(
      product.name,
      product.short_description || tx("storefront.product.meta", "A plan you can buy and a server that appears."),
    );
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
    const quantity = quantityField(product);

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
            { class: "store-back", href: withCurrency("/products") },
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
              h(
                "div",
                { class: "store-card-cat" },
                product.category_slug
                  ? h(
                      "a",
                      { href: withCurrency(`/products/${encodeURIComponent(product.category_slug)}`) },
                      product.category_name || tx("storefront.card.product", "Product"),
                    )
                  : product.category_name || tx("storefront.card.product", "Product"),
                product.featured ? h("span", { class: "dot-sep" }, "·") : null,
                product.featured ? badge(tx("storefront.card.featured", "Featured"), "accent") : null,
              ),
              h("h1", { class: "store-title store-title--sm" }, product.name),
              product.short_description ? h("p", { class: "store-sub" }, product.short_description) : null,
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
          h(
            "div",
            { class: "store-buy-card" },
            h(
              "div",
              { class: "store-buy-label" },
              prices.length > 1 ? tx("storefront.product.plans", "Plans") : tx("storefront.product.price", "Price"),
            ),
            prices.length === 0
              ? h("p", { class: "dim" }, tx("storefront.product.no_price", "This product has no price yet."))
              : h("div", { class: "store-plans" }, ...prices.map((price) => planRow(product, price, prices.length > 1))),
            stockNotice(product),
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
                  chosen ? optionsCard(product, chosen.id) : null,
                  quantity
                    ? field({
                        label: tx("storefront.product.quantity", "Quantity"),
                        id: "product-quantity",
                        control: quantity,
                      })
                    : null,
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

  const render = (data: CatalogResponse): Node =>
    data.kind === "category" ? renderCategory(data) : renderProduct(asProduct(data));

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
