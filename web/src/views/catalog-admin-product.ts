// One catalog product, for staff: its metadata, its description, its logo, and its plans.
//
// The screen exists mostly to make one thing obvious: the price table is append-only. There is
// no edit button on a price row, the newest row is marked current, and every older row is shown
// with the date it was created — because that is the row an invoice from last month still
// points at. An operator who understands that will not ask for an "edit price" button; one who
// does not would otherwise assume the newest row overwrote the others.
//
// Two things on this page *are* editable, and the difference is deliberate:
//
//   - the **description**, as rich text, because it is prose and nobody should be typing HTML by
//     hand; and
//   - the **plan order**, because where a plan sits in a list changes no amount and no issued
//     document. It is the one column of a price version the API will UPDATE, and this page is
//     the only place that writes it.
//
// "Duplicate" copies the whole product — prices, options and all — into a hidden draft. It is
// how an operator makes a variant without retyping a price, which is how prices get mistyped.

import { h, swap } from "../lib/dom";
import { dateTime, money, relative, type Amount } from "../lib/format";
import { icon } from "../lib/icons";
import { descriptionBody, richTextEditor } from "../lib/richtext";
import type { Ctx } from "../lib/types";
import {
  badge,
  btn,
  card,
  emptyState,
  errorState,
  field,
  kv,
  metaGrid,
  pageHead,
  skeletonPage,
  withBusy,
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
  interval_label?: string;
  recurring?: boolean;
  sort_order?: number;
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
  description_format?: string;
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
  one_time: "One-time",
  free: "Free",
};

const intervalText = (price: Price): string =>
  price.interval_label || INTERVAL_LABEL[price.interval] || price.interval;

// plansInOrder is the display order: the operator's plan order, then the newest version within
// one position. It mirrors catalog.Plans on the server, so the preview here and the storefront
// cannot disagree.
const plansInOrder = (prices: Price[]): Price[] =>
  [...prices].sort((a, b) => (a.sort_order ?? 0) - (b.sort_order ?? 0) || b.version - a.version);

// plainToHTML turns a plain description into paragraphs, so opening a plain description in the
// rich editor does not collapse its blank lines into one run-on paragraph.
function plainToHTML(text: string): string {
  const escape = (value: string): string =>
    value.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  return text
    .split(/\n{2,}/)
    .map((para) => para.trim())
    .filter(Boolean)
    .map((para) => `<p>${escape(para).replace(/\n/g, "<br>")}</p>`)
    .join("");
}

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

  // duplicate copies the product and navigates to the copy, which is where the operator wants
  // to be: the copy is a draft that needs a name and a price review.
  const duplicate = async (button: HTMLButtonElement | HTMLAnchorElement | null) => {
    await withBusy(ctx, button, async () => {
      const copy = await ctx.api.post<Product>(`/api/v1/staff/catalog/products/${id}/duplicate`, {});
      ctx.toast("Copied. The copy is hidden until you publish it.", { tone: "success" });
      ctx.navigate(`/catalog-management/${copy.id}`);
    });
  };

  const render = (p: Product): Node => {
    const prices = p.prices ?? [];
    const current = prices[0];
    const ordered = plansInOrder(prices);
    const canWrite = ctx.can("catalog:write");

    // saveDescription writes the description and its format in one request. The format travels
    // with the text because the renderer has to know whether to parse it or escape it.
    const saveDescription = async (
      text: string,
      format: "html" | "plain",
      button: HTMLButtonElement | HTMLAnchorElement | null,
    ) => {
      await withBusy(ctx, button, async () => {
        await ctx.api.put(`/api/v1/staff/catalog/products/${p.id}`, {
          slug: p.slug,
          name: p.name,
          short_description: p.short_description,
          description: text,
          description_format: format,
          category_id: p.category_id,
          image_id: p.image_id,
          hidden: p.hidden,
          sort_order: p.sort_order,
        });
        ctx.toast("Description saved.", { tone: "success" });
        await load();
      });
    };

    const descriptionCard = (): HTMLElement => {
      const body = h("div", { class: "stack-sm" });
      const showView = () => {
        swap(body, descriptionBody(p.description, p.description_format ?? "plain"));
      };
      showView();

      const editButton = canWrite
        ? btn("Edit description", {
            size: "sm",
            icon: "sliders",
            onclick: () => showEditor(),
          })
        : null;

      const showEditor = () => {
        const editor = richTextEditor({
          value: p.description_format === "html" ? p.description : plainToHTML(p.description),
          id: "prod-description",
        });
        const save = btn("Save description", {
          size: "sm",
          variant: "primary",
          onclick: () => void saveDescription(editor.value(), "html", save),
        });
        const cancel = btn("Cancel", {
          size: "sm",
          onclick: () => showView(),
        });
        swap(
          body,
          h(
            "div",
            { class: "stack-sm" },
            field({
              label: "Description",
              id: "prod-description",
              hint: "Rich text. Formatting is limited to what a product page needs; anything a browser could execute is removed when you save.",
              control: editor.el,
            }),
            h("div", { class: "row" }, save, cancel),
          ),
        );
      };

      return card({
        title: "Description",
        sub: (p.description_format ?? "plain") === "html" ? "rich text" : "plain text",
        actions: editButton,
        body,
        foot: h(
          "span",
          null,
          "The format is stored with the text, so the storefront knows whether to render it or show it as written.",
        ),
      });
    };

    const planOrderCard = (): HTMLElement | null => {
      // One plan has no order to arrange, so the card is absent rather than empty.
      if (prices.length < 2) return null;
      const move = async (from: number, to: number, button: HTMLButtonElement | HTMLAnchorElement) => {
        const ids = ordered.map((price) => price.id);
        const [moved] = ids.splice(from, 1);
        if (moved === undefined) return;
        ids.splice(to, 0, moved);
        await withBusy(ctx, button, async () => {
          await ctx.api.put(`/api/v1/staff/catalog/products/${p.id}/plans/order`, { plan_ids: ids });
          await load();
        });
      };

      return card({
        title: "Plan order",
        sub: "how the storefront lists them",
        flush: true,
        body: h(
          "div",
          { class: "portal-list" },
          ...ordered.map((price, index) =>
            h(
              "div",
              { class: "portal-row" },
              h(
                "div",
                { class: "portal-row-main" },
                h("span", { class: "portal-row-title" }, price.name || `Version ${price.version}`),
                h(
                  "span",
                  { class: "portal-row-meta" },
                  h("span", null, intervalText(price)),
                  h("span", { class: "portal-row-dot" }, "·"),
                  h("span", { class: "mono" }, `v${price.version}`),
                ),
              ),
              canWrite
                ? h(
                    "div",
                    { class: "row" },
                    btn("", {
                      size: "sm",
                      icon: "chevronUp",
                      title: "Move up",
                      disabled: index === 0,
                      onclick: (event) => {
                        const target = event.currentTarget as HTMLButtonElement;
                        void move(index, index - 1, target);
                      },
                    }),
                    btn("", {
                      size: "sm",
                      icon: "chevronDown",
                      title: "Move down",
                      disabled: index === ordered.length - 1,
                      onclick: (event) => {
                        const target = event.currentTarget as HTMLButtonElement;
                        void move(index, index + 1, target);
                      },
                    }),
                  )
                : null,
            ),
          ),
        ),
        foot: h(
          "span",
          null,
          "Order is presentation only: it changes no amount and no issued invoice. Changing a price still appends a version.",
        ),
      });
    };

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
          canWrite
            ? btn("Duplicate", {
                size: "sm",
                icon: "copy",
                title: "Copy this product, its plans and its options into a hidden draft",
                onclick: (event) => void duplicate(event.currentTarget as HTMLButtonElement),
              })
            : null,
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
                  h("p", { class: "dim" }, p.short_description || "No short description."),
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

          descriptionCard(),

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
                            h("td", null, intervalText(price)),
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
                    h(
                      "span",
                      { class: "faint" },
                      intervalText(current),
                      current.recurring === false ? " — charged once, never renewed" : "",
                    ),
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
          planOrderCard(),
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
