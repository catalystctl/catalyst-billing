// One catalog product, for staff: its metadata, its logo, and its price history.
//
// The screen exists mostly to make one thing obvious: the price table is append-only. There is
// no edit button on a price row, the newest row is marked current, and every older row is shown
// with the date it was created — because that is the row an invoice from last month still
// points at. An operator who understands that will not ask for an "edit price" button; one who
// does not would otherwise assume the newest row overwrote the others.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateTime, money, relative, type Amount } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  badge,
  btn,
  card,
  emptyState,
  errorState,
  kv,
  metaGrid,
  pageHead,
  skeletonPage,
} from "../ui/kit";

export const meta = { title: "Product", crumb: "Product" };

interface Price {
  id: string;
  version: number;
  name: string;
  currency: string;
  amount: Amount;
  setup: Amount;
  interval: string;
  anchor_day: number;
  anchor_timezone: string;
  provision_plan: string;
  provision_region: string;
  created_at: string;
}

interface Product {
  id: string;
  slug: string;
  name: string;
  short_description: string;
  description: string;
  category_id: string | null;
  category_name: string;
  category_slug: string;
  image_id: string | null;
  image_url: string;
  hidden: boolean;
  sort_order: number;
  active_subscriptions: number;
  created_at: string;
  updated_at: string;
  prices: Price[];
}

const INTERVAL_LABEL: Record<string, string> = {
  monthly: "Monthly",
  quarterly: "Quarterly",
  semiannual: "Every six months",
  annual: "Annual",
};

export default async function catalogProductPage(ctx: Ctx): Promise<Node> {
  const id = ctx.route.params.id ?? "";
  const host = h("div", { class: "page" }, skeletonPage());

  const load = async () => {
    try {
      const product = await ctx.api.get<Product>(`/api/v1/staff/catalog/products/${id}`, { cache: false });
      swap(host, render(product));
    } catch (err) {
      swap(host, errorState(err, () => void load()));
    }
  };

  const render = (p: Product): Node => {
    const prices = p.prices ?? [];
    const current = prices[0];
    const canWrite = ctx.can("catalog:write");

    return h(
      "div",
      { class: "page" },
      pageHead({
        title: p.name,
        sub: p.short_description || "No short description.",
        badge: h(
          "span",
          { class: "row" },
          p.hidden ? badge("Hidden", "neutral", { dot: true }) : badge("Published", "success", { dot: true }),
          prices.length === 0 ? badge("No price", "warning", { dot: true }) : null,
        ),
        actions: h(
          "div",
          { class: "row" },
          h("a", { class: "btn", href: `/products/${p.slug}`, "data-native": "true" }, icon("external", 14), "View in storefront"),
          btn("Back to catalog", { href: "/catalog-management" }),
        ),
      }),

      h(
        "div",
        { class: "split" },
        h(
          "div",
          { class: "stack" },
          card({
            title: "Product",
            body: h(
              "div",
              { class: "stack" },
              h(
                "div",
                { class: "prod-head" },
                h(
                  "div",
                  { class: "prod-logo" },
                  p.image_url
                    ? h("img", { src: p.image_url, alt: "", decoding: "async" })
                    : h("span", { class: "store-card-initial" }, p.name.charAt(0).toUpperCase()),
                ),
                h(
                  "div",
                  { class: "stack-sm" },
                  h("div", { class: "store-card-cat" }, p.category_name || "No category"),
                  h("p", { class: "dim" }, p.description || "No long description."),
                ),
              ),
              metaGrid([
                { label: "Slug", value: h("span", { class: "mono" }, p.slug) },
                { label: "Category", value: p.category_name || "—" },
                { label: "Sort order", value: String(p.sort_order) },
                { label: "Active subscriptions", value: String(p.active_subscriptions) },
                { label: "Created", value: dateTime(p.created_at) },
                { label: "Updated", value: relative(p.updated_at) },
              ]),
            ),
          }),

          card({
            title: "Price versions",
            sub: prices.length > 0 ? `${prices.length} version${prices.length === 1 ? "" : "s"}` : undefined,
            actions: canWrite
              ? btn("Add price version", {
                  size: "sm",
                  icon: "plus",
                  onclick: () => ctx.navigate(`/catalog-management/${p.id}?addPrice=1`),
                })
              : null,
            flush: true,
            body:
              prices.length === 0
                ? emptyState({
                    title: "No price yet",
                    body: "A product with no price is not shown in the storefront, because nobody can buy it.",
                    icon: "dollar",
                  })
                : h(
                    "div",
                    { class: "table-wrap" },
                    h(
                      "table",
                      { class: "table" },
                      h(
                        "thead",
                        null,
                        h(
                          "tr",
                          null,
                          h("th", null, "Version"),
                          h("th", null, "Plan"),
                          h("th", { class: "num" }, "Amount"),
                          h("th", { class: "num" }, "Setup"),
                          h("th", null, "Interval"),
                          h("th", null, "Provision"),
                          h("th", null, "Created"),
                        ),
                      ),
                      h(
                        "tbody",
                        null,
                        ...prices.map((price, index) =>
                          h(
                            "tr",
                            null,
                            h(
                              "td",
                              null,
                              h(
                                "div",
                                { class: "row" },
                                h("span", { class: "mono" }, `v${price.version}`),
                                index === 0 ? badge("Current", "accent", { dot: true }) : null,
                              ),
                            ),
                            h("td", null, price.name || "—"),
                            h("td", { class: "num" }, h("span", { class: "money" }, h("span", { class: "money-code" }, price.currency), money(price.amount))),
                            h("td", { class: "num" }, price.setup.minor > 0 ? money(price.setup) : h("span", { class: "faint" }, "—")),
                            h("td", null, INTERVAL_LABEL[price.interval] ?? price.interval),
                            h(
                              "td",
                              null,
                              price.provision_plan
                                ? h(
                                    "span",
                                    { class: "mono faint" },
                                    price.provision_plan,
                                    price.provision_region ? ` / ${price.provision_region}` : "",
                                  )
                                : h("span", { class: "faint" }, "—"),
                            ),
                            h("td", { class: "dim" }, relative(price.created_at)),
                          ),
                        ),
                      ),
                    ),
                  ),
            foot: h(
              "span",
              null,
              "Prices are append-only. An issued invoice records the version it priced from.",
            ),
          }),
        ),

        h(
          "div",
          { class: "stack" },
          card({
            title: "Current price",
            body: current
              ? h(
                  "div",
                  { class: "stack" },
                  h(
                    "div",
                    { class: "stack-sm" },
                    h("span", { class: "field-label" }, current.name || `Version ${current.version}`),
                    h(
                      "span",
                      { class: "money money--lg" },
                      h("span", { class: "money-code" }, current.currency),
                      money(current.amount),
                    ),
                    h("span", { class: "faint" }, INTERVAL_LABEL[current.interval] ?? current.interval),
                  ),
                  h(
                    "div",
                    null,
                    kv("Setup fee", current.setup.minor > 0 ? money(current.setup) : "None"),
                    kv("Anchor day", String(current.anchor_day)),
                    kv("Timezone", h("span", { class: "mono" }, current.anchor_timezone)),
                    kv("Provision plan", current.provision_plan || "—"),
                    kv("Provision region", current.provision_region || "—"),
                  ),
                )
              : h("p", { class: "dim" }, "Add a price version to publish this product."),
          }),
          card({
            title: "Why a version, not an edit",
            body: h(
              "p",
              { class: "field-hint" },
              "An issued invoice stores the price version it was created from. Editing a price in place would change a document the customer already has, and a ledger that can be rewritten after the fact is not a ledger. So a price change is a new row, and the old row stays exactly as it was.",
            ),
          }),
        ),
      ),
    );
  };

  void load();
  return host;
}
