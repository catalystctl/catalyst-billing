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
import { ApiError } from "../lib/api";
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
  input,
  kv,
  metaGrid,
  pageHead,
  select,
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
  /** The per-product activation snippet, rendered into the activation mail as
   *  {{product_instructions}}. Staff-only: the public shape omits it. */
  email_template?: string;
  featured?: boolean;
  stock?: number | null;
  per_user_limit?: number | null;
  allow_quantity?: string;
  server_id?: string | null;
  server_name?: string;
  prices: Price[];
}

interface Server {
  id: string;
  slug: string;
  name: string;
  provider: string;
  region: string;
  node: string;
  enabled: boolean;
  sort_order: number;
  product_count: number;
}

// ProductPlacement is where a product sits: the provisioner target it is sold onto and its place
// in the category tree. It is a separate read from the product because the product shape is also
// served to the storefront, which must never be handed a node name.
interface ProductPlacement {
  product_id: string;
  server_id: string | null;
  server: Server | null;
  category_id: string | null;
  category_parent_id: string | null;
  category_path: string;
  category_ancestor_slugs: string[];
}

interface ServersResponse {
  data: Server[];
}

const INTERVAL_LABEL: Record<string, string> = {
  monthly: "Monthly",
  quarterly: "Quarterly",
  semiannual: "Every six months",
  annual: "Annual",
  one_time: "One-time",
  free: "Free",
};

// dayIntervalDays reads the stored day-count interval ("every_45_days") and returns the
// number of days, or 0 when the interval is a calendar one. It mirrors
// calendar.ParseDayInterval on the server — one canonical spelling, 1..366 days — so the
// screen cannot render a period the server would refuse.
function dayIntervalDays(interval: string): number {
  const match = /^every_([1-9][0-9]{0,2})_days$/.exec(interval);
  if (!match) return 0;
  const days = Number(match[1]);
  return days >= 1 && days <= 366 ? days : 0;
}

const intervalText = (price: Price): string => {
  if (price.interval_label) return price.interval_label;
  const days = dayIntervalDays(price.interval);
  if (days > 0) return `Every ${days} days`;
  return INTERVAL_LABEL[price.interval] || price.interval;
};

// The options an interval picker offers, in one place, because two screens offer them: this one
// and the catalog screen's "Add a price" dialog. Exported rather than copied so the two cannot
// drift, and so the day-count form is not a value one screen knows about and the other does not.
//
// `every_days` is a UI-only sentinel: it is never sent to the API. The stored value is always
// the canonical `every_<N>_days` produced by dayIntervalValue, which is the single spelling the
// server and the database CHECK accept.
export interface IntervalOption {
  value: string;
  label: string;
}

export const INTERVAL_OPTIONS: IntervalOption[] = [
  { value: "monthly", label: "Monthly" },
  { value: "quarterly", label: "Quarterly" },
  { value: "semiannual", label: "Every six months" },
  { value: "annual", label: "Annual" },
  // The two plan types that never renew (SF-13, CAT-03). The server has always accepted them;
  // neither picker offered them, so a one-time or free plan could not be created from the panel.
  { value: "one_time", label: "One-time (charged once)" },
  { value: "free", label: "Free (no invoice)" },
  // A fixed number of days is a second kind of period, not a calendar one: it is counted in
  // civil days from the first period, so it lands on the right date across a month end, a leap
  // day and a daylight-saving change.
  { value: "every_days", label: "Every N days" },
];

/** dayIntervalValue composes the stored interval for a day count.
 *
 *  It returns null rather than a string when the count is not a whole number in 1..366, so a
 *  caller cannot accidentally turn a typo into a billing period. That is not defensive
 *  padding: an earlier version of this helper truncated and fell back, which meant a cleared
 *  field or `abc` became "every_45_days" — a 45-day schedule nobody chose — and `45.9` became
 *  45. Refusing is the only behaviour that cannot invent a period. */
export function dayIntervalValue(days: number): string | null {
  if (!Number.isInteger(days) || days < 1 || days > 366) return null;
  return `every_${days}_days`;
}

/** intervalValueFor turns a picker selection into the value to POST: the day-count sentinel is
 *  composed from the number field, and every other choice is sent as itself. It returns null
 *  when the sentinel is selected with an unusable count, which the caller must refuse. */
export function intervalValueFor(selection: string, days: number): string | null {
  return selection === "every_days" ? dayIntervalValue(days) : selection;
}

/** parseMinorUnits reads a major-unit amount ("12.00") into minor units without ever passing
 *  through a float, which is the rule this codebase holds everywhere money is parsed. Returns
 *  null when the text is not a number, so the caller can refuse the submit rather than send a
 *  silently wrong amount. */
function parseMinorUnits(raw: string): number | null {
  const cleaned = raw.trim().replace(/[,\s]/g, "");
  if (!/^\d*(\.\d{0,})?$/.test(cleaned) || cleaned === "" || cleaned === ".") return null;
  const [whole = "0", fraction = ""] = cleaned.split(".");
  const padded = (fraction + "00").slice(0, 2);
  return Number(whole) * 100 + Number(padded);
}

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
  // The last product read, so a header action (delete) outside the render closure can name the
  // record without a second request.
  let latestProduct: Product | null = null;

  const load = async () => {
    try {
      const [product, placement, servers] = await Promise.all([
        ctx.api.get<Product>(`/api/v1/staff/catalog/products/${id}`, { cache: false }),
        // The placement is optional in the same sense the server is: a product with no server
        // has nothing to show, and a failed read must not blank the whole page.
        ctx.api
          .get<ProductPlacement>(`/api/v1/staff/catalog/products/${id}/placement`, { cache: false })
          .catch(() => null),
        ctx.api
          .get<ServersResponse>("/api/v1/staff/catalog/servers", { cache: false })
          .catch(() => ({ data: [] as Server[] })),
      ]);
      latestProduct = product;
      swap(host, render(product, placement, servers.data ?? []));
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

  /** deleteProduct removes a product that was never sold, and explains the refusal when it was. */
  const deleteProduct = async () => {
    const product = latestProduct;
    if (!product) return;
    const ok = await ctx.confirm({
      title: `Delete “${product.name}”?`,
      body: "This removes the product and its price versions. It is refused while a service or an order still references it — hide it instead, which keeps everything that was sold exactly as it is.",
      confirmLabel: "Delete product",
      danger: true,
    });
    if (!ok) return;
    try {
      await ctx.api.del(`/api/v1/staff/catalog/products/${product.id}`);
      ctx.toast("Product deleted", { tone: "success", detail: product.name });
      ctx.navigate("/catalog-management");
    } catch (err) {
      if (err instanceof ApiError && err.code === "in_use") {
        ctx.toast("This product has been sold", {
          tone: "danger",
          detail: err.detail || "Hide it instead: a delete would orphan the services and orders that reference it.",
          duration: 0,
        });
        return;
      }
      ctx.toast(err instanceof ApiError ? err.detail || "The product could not be deleted." : "The server could not be reached.", {
        tone: "danger",
        detail: err instanceof ApiError ? err.code : undefined,
      });
    }
  };

  const render = (p: Product, placement: ProductPlacement | null, servers: Server[]): Node => {
    const prices = p.prices ?? [];
    const current = prices[0];
    const ordered = plansInOrder(prices);
    const canWrite = ctx.can("catalog:write");

    // addPrice appends a price version without leaving the product.
  //
  // The "Add price version" button used to navigate to `?addPrice=1`, which nothing consumed:
  // the price dialog lives on the catalog list, so the button re-rendered this page and did
  // nothing. It is a dead end, and a dead end on the one screen where a price is entered is how
  // a product ends up unpriced. The form is here instead, and it shares the interval list with
  // the catalog screen's dialog (INTERVAL_OPTIONS), so the two cannot offer different periods.
  const addPrice = (p: Product, onDone: () => void) => {
    const name = input({ id: "price-name", placeholder: "Monthly", required: true });
    const currency = input({ id: "price-currency", value: "USD", class: "input--mono", maxlength: 12 });
    const amount = input({ id: "price-amount", placeholder: "12.00", inputmode: "decimal", class: "input--mono" });
    const setup = input({ id: "price-setup", placeholder: "0.00", inputmode: "decimal", class: "input--mono" });
    const interval = select(INTERVAL_OPTIONS, { id: "price-interval", value: "monthly" });
    const days = input({ id: "price-interval-days", type: "number", value: "45", min: "1", max: "366" });
    const anchorDay = input({ id: "price-anchor-day", type: "number", value: "1", min: "1", max: "31" });
    const timezone = input({ id: "price-tz", value: "UTC", class: "input--mono" });

    const daysField = field({
      label: "Days between renewals",
      control: days,
      id: "price-interval-days",
      hint: "1-366. Each period is exactly this many days, counted from the first one.",
    });
    const syncDays = () => {
      daysField.hidden = interval.value !== "every_days";
    };
    interval.addEventListener("change", syncDays);
    syncDays();

    void ctx.formDialog({
      title: "Add a price version",
      subtitle: `${p.name} — a new version is appended; the old one is never edited.`,
      submitLabel: "Add price version",
      body: h(
        "div",
        { class: "form-grid" },
        field({ label: "Plan name", control: name, id: "price-name", hint: "What the customer sees: “Monthly”, “8 GB”." }),
        field({ label: "Currency", control: currency, id: "price-currency", hint: "ISO code, e.g. USD." }),
        field({ label: "Amount", control: amount, id: "price-amount", hint: "In major units, e.g. 12.00" }),
        field({ label: "Setup fee", control: setup, id: "price-setup", hint: "Optional. Charged once." }),
        field({ label: "Billing interval", control: interval, id: "price-interval" }),
        daysField,
        field({ label: "Anchor day", control: anchorDay, id: "price-anchor-day", hint: "Day of the month the first period starts." }),
        field({ label: "Anchor timezone", control: timezone, id: "price-tz", hint: "IANA name, e.g. Europe/Berlin." }),
      ),
      onSubmit: async () => {
        const minor = parseMinorUnits(amount.value);
        const setupMinor = setup.value.trim() === "" ? 0 : parseMinorUnits(setup.value);
        if (minor === null || setupMinor === null) {
          return "Amounts must be numbers, e.g. 12.00.";
        }
        // The wire value is composed once, and a null means the sentinel was selected with a
        // count the helper refused. Validation lives in the helper and nowhere else: a second
        // copy here would be two rules that drift, and the looser one would win.
        const wireInterval = intervalValueFor(interval.value, Number(days.value));
        if (wireInterval === null) {
          return "Days between renewals must be a whole number from 1 to 366.";
        }
        try {
          await ctx.api.post(`/api/v1/staff/catalog/products/${p.id}/prices`, {
            name: name.value.trim(),
            currency: currency.value.trim().toUpperCase(),
            // A free plan is zero by definition and the server refuses one that costs anything;
            // sending zero is the honest reading of the operator's choice rather than an error
            // about a field they did not think applied.
            amount_minor: wireInterval === "free" ? 0 : minor,
            setup_minor: wireInterval === "free" ? 0 : setupMinor,
            interval: wireInterval,
            anchor_day: Number(anchorDay.value) || 1,
            anchor_timezone: timezone.value.trim() || "UTC",
          });
        } catch (err) {
          // A refusal is shown in the dialog rather than closing it, so the operator keeps what
          // they typed and can correct the one field the server objected to.
          return err instanceof ApiError
            ? err.detail || "The price could not be added."
            : "The server could not be reached.";
        }
        ctx.toast("Price version added", { tone: "success" });
        onDone();
      },
    });
  };

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

    // placementCard is where the product sits: the provisioner target it is sold onto and the
    // category branch it is filed in. It is its own card because both are structural facts rather
    // than product prose, and because the server is the one control on this page that changes
    // *where a future purchase is provisioned* rather than what the page says.
    const placementCard = (): HTMLElement => {
      const current = placement?.server ?? null;
      const selector = select(
        [
          { value: "", label: "Inherit from the plan" },
          ...servers.map((srv) => ({
            value: srv.id,
            label: serverLabel(srv),
            disabled: !srv.enabled,
          })),
        ],
        { id: "prod-server", value: placement?.server_id ?? "" },
      );
      const save = btn("Save server", { size: "sm" });
      save.addEventListener("click", () => {
        void withBusy(ctx, save, async () => {
          await ctx.api.put(`/api/v1/staff/catalog/products/${p.id}/server`, {
            server_id: selector.value || null,
          });
          ctx.toast("Server saved.", { tone: "success" });
          await load();
        });
      });

      return card({
        title: "Placement",
        sub: "where it is sold from",
        body: h(
          "div",
          { class: "stack-sm" },
          kv("Category", placement?.category_path ? pathText(placement.category_path) : p.category_name || "—"),
          kv("Server", current ? serverLabel(current) : "Inherit from the plan"),
          canWrite
            ? field({
                label: "Server",
                control: selector,
                id: "prod-server",
                hint: "The node this product provisions onto. A plan that names its own region wins.",
              })
            : null,
          canWrite ? h("div", { class: "row" }, save) : null,
        ),
        foot: h(
          "span",
          null,
          "A change here affects the next purchase. Services that already exist keep the region they were created with.",
        ),
      });
    };

    // merchandisingCard is the presentation half of the product: how many there are, how many one
    // customer may hold, whether a quantity is offered, and whether the shop pushes it. None of
    // these existed as a control before, which meant `stock` could only be set through psql and a
    // sold-out product could not be marked sold out.
    //
    // It is its own request rather than part of the product PUT because that PUT replaces the whole
    // metadata record: the description editor and the hide toggle send it, and neither knows what
    // stock is, so folding these fields in would mean every description save restocked the shop.
    const merchandisingCard = (): HTMLElement => {
      const stock = input({
        id: "prod-stock",
        type: "number",
        min: 0,
        value: p.stock ?? "",
        placeholder: "Unlimited",
        class: "input--mono",
      });
      const limit = input({
        id: "prod-limit",
        type: "number",
        min: 1,
        value: p.per_user_limit ?? "",
        placeholder: "No limit",
        class: "input--mono",
      });
      const policy = select(
        [
          { value: "disabled", label: "One per order" },
          { value: "separated", label: "Quantity: each is its own service" },
          { value: "combined", label: "Quantity: one service, more units" },
        ],
        { id: "prod-quantity", value: p.allow_quantity ?? "disabled" },
      );
      const featured = h("input", { type: "checkbox", checked: p.featured ?? false });
      const errorHost = h("div");

      const save = btn("Save merchandising", { size: "sm" });
      save.addEventListener("click", () => {
        void withBusy(ctx, save, async () => {
          errorHost.replaceChildren();
          const parsedStock = parseOptionalCount(stock.value);
          const parsedLimit = parseOptionalCount(limit.value);
          if (parsedStock === undefined || parsedLimit === undefined) {
            swap(errorHost, h("div", { class: "banner banner--danger" }, "Stock and the limit must be whole numbers, or empty for no limit."));
            return;
          }
          try {
            await ctx.api.put(`/api/v1/staff/catalog/products/${p.id}/merchandising`, {
              stock: parsedStock,
              per_user_limit: parsedLimit,
              allow_quantity: policy.value,
              featured: featured.checked,
            });
            ctx.toast("Merchandising saved.", { tone: "success" });
            await load();
          } catch (err) {
            const message =
              err instanceof ApiError ? err.detail || "The change was refused." : "The server could not be reached.";
            swap(errorHost, h("div", { class: "banner banner--danger" }, message));
          }
        });
      });

      return card({
        title: "Merchandising",
        sub: p.featured ? "featured" : "not featured",
        body: h(
          "div",
          { class: "stack-sm" },
          errorHost,
          kv("Stock", p.stock === null || p.stock === undefined ? "Unlimited" : String(p.stock)),
          kv("Per-customer limit", p.per_user_limit === null || p.per_user_limit === undefined ? "None" : String(p.per_user_limit)),
          kv("Quantity", quantityPolicyLabel(p.allow_quantity ?? "disabled")),
          kv("Featured", p.featured ? "Yes — shown first on the storefront" : "No"),
          canWrite
            ? h(
                "div",
                { class: "form-grid" },
                field({ label: "Stock", control: stock, id: "prod-stock", hint: "Empty is unlimited; 0 marks it sold out." }),
                field({ label: "Per-customer limit", control: limit, id: "prod-limit", hint: "Empty is no limit." }),
                field({ label: "Quantity policy", control: policy, id: "prod-quantity", class: "span-2" }),
                h("div", { class: "span-2" }, h("label", { class: "switch" }, featured, "Featured on the storefront")),
              )
            : null,
          canWrite ? h("div", { class: "row" }, save) : null,
        ),
        foot: h(
          "span",
          null,
          "Stock is decremented at checkout and restored when an unpaid order is cancelled. A sold-out product stays listed and cannot be ordered.",
        ),
      });
    };

    // activationCard edits the per-product activation snippet (CAT-08). It is staff-only prose —
    // what the customer is told about how to use what they bought — rendered into the activation
    // mail as {{product_instructions}}. The column and the mail existed; the editor did not, so the
    // only way to set it was SQL.
    const activationCard = (): HTMLElement => {
      const snippet = h("textarea", {
        class: "textarea",
        id: "prod-instructions",
        rows: 5,
        placeholder: "Where the panel is, what to do first, which port to open…",
      }) as HTMLTextAreaElement;
      snippet.value = p.email_template ?? "";
      const errorHost = h("div");
      const save = btn("Save instructions", { size: "sm" });
      save.addEventListener("click", () => {
        void withBusy(ctx, save, async () => {
          errorHost.replaceChildren();
          try {
            await ctx.api.put(`/api/v1/staff/catalog/products/${p.id}`, {
              slug: p.slug,
              name: p.name,
              short_description: p.short_description,
              description: p.description,
              description_format: p.description_format ?? "plain",
              category_id: p.category_id,
              image_id: p.image_id,
              hidden: p.hidden,
              sort_order: p.sort_order,
              email_template: snippet.value,
            });
            ctx.toast("Activation instructions saved.", { tone: "success" });
            await load();
          } catch (err) {
            const message =
              err instanceof ApiError ? err.detail || "The instructions were refused." : "The server could not be reached.";
            swap(errorHost, h("div", { class: "banner banner--danger" }, message));
          }
        });
      });

      return card({
        title: "Activation instructions",
        sub: "what the customer is told after paying",
        body: h(
          "div",
          { class: "stack-sm" },
          errorHost,
          canWrite
            ? field({
                label: "Instructions",
                control: snippet,
                id: "prod-instructions",
                hint: "Plain text, up to 4000 bytes. Empty sends the default sentence.",
              })
            : h("p", { class: "field-hint" }, p.email_template || "None set: the default sentence is sent."),
          canWrite ? h("div", { class: "row" }, save) : null,
        ),
        foot: h("span", null, "Rendered into the activation mail as {{product_instructions}}."),
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
          // Deleting is the other half of "stop selling this" (ADM-03): hiding keeps a product
          // sellable to the people who already bought it, deleting is for a draft that was never
          // sold. The server refuses it while a service or an order references the product.
          canWrite
            ? btn("Delete", {
                size: "sm",
                variant: "danger",
                icon: "trash",
                onclick: () => void deleteProduct(),
              })
            : null,
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
                {
                  label: "Category path",
                  value: placement?.category_path ? pathText(placement.category_path) : "—",
                },
                { label: "Server", value: placement?.server ? serverLabel(placement.server) : "Inherit from the plan" },
                { label: "Sort order", value: String(p.sort_order) },
                {
                  label: "Stock",
                  value: p.stock === null || p.stock === undefined ? "Unlimited" : String(p.stock),
                },
                { label: "Quantity", value: quantityPolicyLabel(p.allow_quantity ?? "disabled") },
                { label: "Featured", value: p.featured ? "Yes" : "No" },
                { label: "Active subscriptions", value: String(p.active_subscriptions) },
                { label: "Created", value: dateTime(p.created_at) },
                { label: "Updated", value: relative(p.updated_at) },
              ]),
            ),
          }),

          placementCard(),

          merchandisingCard(),

          descriptionCard(),

          activationCard(),

          card({
            title: "Price versions",
            sub: prices.length > 0 ? `${prices.length} version${prices.length === 1 ? "" : "s"}` : undefined,
            actions: canWrite
              ? btn("Add price version", {
                  size: "sm",
                  icon: "plus",
                  onclick: () => addPrice(p, () => void load()),
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
                    kv("Billing period", billingPeriodDetail(current)),
                    kv(
                      "Anchor day",
                      // A day-count period is counted from this day for its first
                      // period only; after that the period is a fixed number of days
                      // and the day-of-month drifts by design.
                      dayIntervalDays(current.interval) > 0
                        ? `${current.anchor_day} (first period only)`
                        : String(current.anchor_day),
                    ),
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

/** billingPeriodDetail shows what the price is actually sold as: the readable label and, for a
 *  day-count plan, the stored interval beside it, because "every 45 days" and the row an invoice
 *  priced from are not the same string and an operator checking a mistake needs both. */
function billingPeriodDetail(price: Price): string {
  const label = intervalText(price);
  const days = dayIntervalDays(price.interval);
  if (days === 0) return label;
  return `${label} — ${days} civil days, stored as ${price.interval}`;
}

/** serverLabel renders a provisioner target the way an operator recognises it: the name, then
 *  the region and node the adapter will actually be told about. */
function serverLabel(server: Server): string {
  const detail = [server.region, server.node].filter(Boolean).join(" / ");
  return detail ? `${server.name} (${detail})` : server.name;
}

/** pathText renders a slash-joined category path as a breadcrumb. */
function pathText(path: string): string {
  return path.split("/").filter(Boolean).join(" › ");
}

/** parseOptionalCount reads a whole number that may be left empty.
 *
 *  `undefined` is "the text is not a number", `null` is "empty means no limit", and a number is
 *  the number. The distinction is the whole point: a helper that returned null for both would
 *  turn a typo into "unlimited stock", which is the one direction a stock field must never fail
 *  in. */
function parseOptionalCount(raw: string): number | null | undefined {
  const text = raw.trim();
  if (text === "") return null;
  if (!/^\d+$/.test(text)) return undefined;
  return Number(text);
}

/** quantityPolicyLabel says what the storefront does with a quantity, in the customer's terms. */
function quantityPolicyLabel(policy: string): string {
  switch (policy) {
    case "separated":
      return "Quantity allowed — each unit is its own service";
    case "combined":
      return "Quantity allowed — one service with more units";
    default:
      return "One per order";
  }
}
