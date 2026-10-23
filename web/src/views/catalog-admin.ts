// Catalog management: categories, products, logos and price versions.
//
// This is the screen the storefront is built from, and it is deliberately shaped around the one
// rule the API enforces: a product's *description* is editable, a product's *price* is not.
//
// So there is no "edit price" control anywhere. A price is added as a new version, and the
// version list shows the history with the newest marked current. An operator who wants to raise
// a price clicks "Add price version" and sees version 2 appear above version 1 — which is what
// actually happened, and what an invoice issued last month still points at.
//
// A hidden product is shown here with a badge, because hiding is a publishing decision, not a
// deletion: the row stays, the storefront stops showing it, and existing services keep billing.

import type { Api } from "../lib/api";
import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { money, relative, type Amount } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  badge,
  btn,
  card,
  checkboxField,
  emptyState,
  errorState,
  field,
  formValues,
  input,
  pageHead,
  sectionTitle,
  select,
  skeletonPage,
  textarea,
} from "../ui/kit";
import { bindForm, openDialog } from "../ui/dialog";

export const meta = { title: "Catalog", crumb: "Catalog" };

interface Price {
  id: string;
  version: number;
  name: string;
  currency: string;
  amount: Amount;
  setup: Amount;
  interval: string;
  /** The customer-facing name of the interval, from `catalog.IntervalLabel`. Present on every
   *  staff payload; optional here so a stale response cannot break the row. */
  interval_label?: string;
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
  featured?: boolean;
  /** Units available: null is unlimited, 0 is sold out. */
  stock?: number | null;
  per_user_limit?: number | null;
  allow_quantity?: string;
  prices: Price[];
}

interface Category {
  id: string;
  slug: string;
  name: string;
  description: string;
  /** The category's logo, in the same deduped store a product logo lives in (SF-06). NULL means
   *  no image, which the storefront draws as a generated mark. */
  image_id: string | null;
  sort_order: number;
  hidden: boolean;
  product_count?: number;
}

/** mediaURL is the path an uploaded image is served from: the id names one byte sequence, so the
 *  URL is a function of it and there is nothing to keep in sync. */
function mediaURL(id: string | null | undefined): string {
  return id ? `/api/v1/media/${id}` : "";
}

// CategoryNode is a category plus its place in the tree. The panel renders the tree rather than
// the flat list because that is the point of nesting: an operator has to be able to see that
// "Minecraft" sits under "Game servers" before filing a product in it.
interface CategoryNode extends Category {
  parent_id: string | null;
  depth: number;
  path: string;
  subtree_product_count: number;
  children?: CategoryNode[];
}

// Server is a provisioner target: a node on the provider the installation is configured with.
// It is not the provider configuration (that is the Servers settings screen); it is which node
// on it a product is sold onto.
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

// ProductPlacement is where a product sits: its server and its category branch. It is its own
// read because the product shape is also served to the storefront, which must never see a node
// name.
interface ProductPlacement {
  product_id: string;
  server_id: string | null;
  server: Server | null;
  category_id: string | null;
  category_parent_id: string | null;
  category_path: string;
  category_ancestor_slugs: string[];
}

interface ProductsResponse {
  data: Product[];
}

interface TreeResponse {
  data: CategoryNode[];
  max_depth: number;
}

interface ServersResponse {
  data: Server[];
}

const INTERVALS = [
  { value: "monthly", label: "Monthly" },
  { value: "quarterly", label: "Quarterly" },
  { value: "semiannual", label: "Every six months" },
  { value: "annual", label: "Annual" },
  // The two plan types that do not renew (SF-13, CAT-03). They were accepted by the server and
  // offered by the product screen but missing here, so a one-time or free plan could only be
  // created from one of the two price dialogs.
  { value: "one_time", label: "One-time (charged once)" },
  { value: "free", label: "Free (no invoice)" },
  // A sentinel, never sent to the API: the wire value of an arbitrary period is
  // `every_<N>_days`, which the number input beside this select supplies. The select cannot
  // offer 366 options, and a free-text interval field would let an operator type a value the
  // server rejects without saying what it accepts.
  { value: "every_days", label: "Every N days" },
];

export default async function catalogAdminPage(ctx: Ctx): Promise<Node> {
  const host = h("div", { class: "page" }, skeletonPage());
  let products: Product[] = [];
  let categories: CategoryNode[] = [];
  let categoryTree: CategoryNode[] = [];
  let servers: Server[] = [];

  const load = async () => {
    try {
      const [p, t, s] = await Promise.all([
        ctx.api.get<ProductsResponse>("/api/v1/staff/catalog/products", { cache: false }),
        ctx.api.get<TreeResponse>("/api/v1/staff/catalog/categories/tree", { cache: false }),
        ctx.api.get<ServersResponse>("/api/v1/staff/catalog/servers", { cache: false }),
      ]);
      products = p.data ?? [];
      categoryTree = t.data ?? [];
      // The flat list feeds the product dialog's category selector and the counts; the tree is
      // what the categories card renders.
      categories = flattenTree(categoryTree).map((entry) => entry.node);
      servers = s.data ?? [];
      swap(host, render());
    } catch (err) {
      swap(host, errorState(err, () => void load()));
    }
  };

  const render = (): Node => {
    const canWrite = ctx.can("catalog:write");
    const visible = products.filter((p) => !p.hidden).length;
    const priced = products.filter((p) => p.prices.length > 0).length;

    return h(
      "div",
      { class: "page" },
      pageHead({
        title: "Catalog",
        sub: "What is for sale, and every price it has ever had. Products are editable; a price is immutable, so changing one adds a version.",
        actions: canWrite
          ? h(
              "div",
              { class: "row" },
              btn("New category", { icon: "layers", onclick: () => categoryDialog(null) }),
              btn("New product", { variant: "primary", icon: "plus", onclick: () => productDialog(null) }),
            )
          : null,
      }),

      !canWrite
        ? h(
            "div",
            { class: "banner banner--info" },
            icon("info", 15),
            h(
              "span",
              null,
              h("strong", null, "Read only."),
              " Changing the catalog requires the ",
              h("code", { class: "inline-code" }, "catalog:write"),
              " permission.",
            ),
          )
        : null,

      h(
        "div",
        { class: "grid-4" },
        stat("Products", products.length, "in the catalog"),
        stat("Published", visible, "visible in the storefront"),
        stat("Priced", priced, priced === products.length ? "all priced" : `${products.length - priced} without a price`),
        stat("Categories", categories.length, "for grouping"),
      ),

      h(
        "div",
        { class: "split" },
        card({
          title: "Products",
          sub: `${products.length} total`,
          actions: h(
            "a",
            // A real navigation, not a router hop: the storefront is a whole-page layout with its
            // own header, and `data-native` keeps the SPA router out of it so the catalog screen's
            // state is not carried into a page that has no shell. `target` opens it beside the
            // catalog, which is what "view the storefront" means while you are editing a product.
            { class: "linkish", href: "/", target: "_blank", rel: "noopener", "data-native": "true" },
            "View storefront",
            icon("external", 12),
          ),
          flush: true,
          body:
            products.length === 0
              ? emptyState({
                  title: "No products yet",
                  body: "Create a category, then a product, then add a price. A product without a price is not shown in the storefront.",
                  icon: "box",
                  action: canWrite
                    ? btn("Create the first product", { variant: "primary", onclick: () => productDialog(null) })
                    : null,
                })
              : h("div", null, ...products.map(productRow)),
        }),
        h(
          "div",
          { class: "stack" },
          card({
            title: "Categories",
            sub: `${categories.length}`,
            flush: true,
            body:
              categories.length === 0
                ? emptyState({
                    title: "No categories",
                    body: "A product belongs to a category so the storefront can group it.",
                    icon: "layers",
                    action: canWrite ? btn("New category", { onclick: () => categoryDialog(null) }) : null,
                  })
                : h(
                    "div",
                    null,
                    ...flattenTree(categoryTree).map((entry) => categoryRow(entry.node, entry.depth)),
                  ),
          }),
          card({
            title: "Servers",
            sub: servers.length === 0 ? "None" : `${servers.length}`,
            actions: canWrite
              ? btn("New server", { size: "sm", icon: "plus", onclick: () => serverDialog(null) })
              : null,
            flush: true,
            body:
              servers.length === 0
                ? emptyState({
                    title: "No servers",
                    body: "A server is a provisioner target: the node a product is sold onto. With none, every product provisions onto the region its plan names.",
                    icon: "server",
                    action: canWrite ? btn("Add a server", { onclick: () => serverDialog(null) }) : null,
                  })
                : h(
                    "div",
                    null,
                    ...servers.map((server) => serverRow(server, canWrite)),
                  ),
          }),
          card({
            title: "How pricing works",
            body: h(
              "div",
              { class: "stack-sm" },
              h(
                "p",
                { class: "field-hint" },
                "A price is a version, not a field. An issued invoice records the version it priced from, so editing a price in place would rewrite a document a customer already has.",
              ),
              h(
                "p",
                { class: "field-hint" },
                "To change a price, add a new version. Existing subscriptions stay on the version they started on until they are migrated.",
              ),
              h(
                "p",
                { class: "field-hint" },
                "A product with no price is not shown in the storefront: nobody can buy it, so offering it would be a broken card.",
              ),
            ),
          }),
        ),
      ),
    );
  };

  const stat = (label: string, value: number, hint: string): HTMLElement =>
    h(
      "section",
      { class: "card stat" },
      h("span", { class: "stat-label" }, label),
      h("span", { class: "stat-value" }, String(value)),
      h("span", { class: "stat-hint" }, hint),
    );

  const productRow = (p: Product): HTMLElement => {
    const current = p.prices[0];
    return h(
      "div",
      { class: "cat-row" },
      h(
        "div",
        { class: "cat-logo" },
        p.image_url
          ? h("img", { src: p.image_url, alt: "", loading: "lazy", decoding: "async" })
          : h("span", { class: "store-card-initial" }, p.name.charAt(0).toUpperCase()),
      ),
      h(
        "div",
        { class: "cat-main" },
        h(
          "div",
          { class: "cat-name" },
          h("a", { href: `/catalog-management/${p.id}`, class: "row-title" }, p.name),
          p.hidden ? badge("Hidden", "neutral", { dot: true }) : badge("Published", "success", { dot: true }),
          p.prices.length === 0 ? badge("No price", "warning", { dot: true }) : null,
          p.featured ? badge("Featured", "accent") : null,
          // Stock is a merchandising fact an operator has to be able to see from the list: it is
          // the one field that decides whether a published product can actually be bought.
          p.stock === null || p.stock === undefined
            ? null
            : p.stock === 0
              ? badge("Sold out", "danger", { dot: true })
              : badge(`${p.stock} in stock`, "neutral"),
        ),
        h(
          "div",
          { class: "cat-meta" },
          h("span", { class: "mono" }, p.slug),
          p.category_name ? h("span", { class: "dot-sep" }, "·") : null,
          p.category_name ? h("span", null, p.category_name) : null,
          p.active_subscriptions > 0
            ? h("span", { class: "dot-sep" }, "·")
            : null,
          p.active_subscriptions > 0
            ? h("span", null, `${p.active_subscriptions} active subscription${p.active_subscriptions === 1 ? "" : "s"}`)
            : null,
        ),
        p.short_description
          ? h("div", { class: "cat-desc" }, p.short_description)
          : null,
      ),
      h(
        "div",
        { class: "cat-price" },
        current
          ? h(
              "span",
              { class: "money" },
              h("span", { class: "money-code" }, current.currency),
              money(current.amount),
              h("span", { class: "cat-interval" }, ` / ${current.interval_label || current.interval}`),
            )
          : h("span", { class: "faint" }, "—"),
      ),
      h(
        "div",
        { class: "cat-actions" },
        btn("Open", { href: `/catalog-management/${p.id}`, size: "sm" }),
        ctx.can("catalog:write")
          ? btn("Add price", {
              size: "sm",
              icon: "plus",
              title: "Add a price version to this product",
              onclick: () => priceDialog(p),
            })
          : null,
        ctx.can("catalog:write")
          ? h(
              "button",
              {
                class: "btn btn--ghost btn--icon btn--sm",
                type: "button",
                title: p.hidden ? "Publish" : "Hide from storefront",
                "aria-label": p.hidden ? "Publish" : "Hide",
                onclick: () => void toggleHidden(p),
              },
              icon(p.hidden ? "eye" : "eye", 14),
            )
          : null,
        // Deleting is not hiding (ADM-03). Hiding keeps the product sellable to anyone who
        // already bought it; deleting is for a draft that was never sold. The server refuses a
        // delete while a service or an order references it, and that refusal is the honest
        // answer rather than a rule hidden in the UI.
        ctx.can("catalog:write")
          ? h(
              "button",
              {
                class: "btn btn--ghost btn--icon btn--sm",
                type: "button",
                title: "Delete product",
                "aria-label": "Delete product",
                onclick: () => void deleteProduct(p),
              },
              icon("trash", 13),
            )
          : null,
      ),
    );
  };

  const categoryRow = (c: CategoryNode, depth: number): HTMLElement =>
    h(
      "div",
      { class: "cat-row cat-row--compact", dataset: { depth: String(depth) } },
      h(
        "div",
        { class: "cat-main" },
        h(
          "div",
          { class: "cat-name" },
          // The depth is drawn as a repeated mark rather than as padding: an inline `style`
          // attribute is refused by the CSP (`style-src 'self'`, no `unsafe-inline`), so the old
          // padding silently did nothing and every category looked like a root.
          depth > 1 ? h("span", { class: "faint mono" }, `${"— ".repeat(depth - 1)}`) : null,
          h("span", { class: "row-title" }, c.name),
          c.hidden ? badge("Hidden", "neutral", { dot: true }) : null,
          c.image_id ? badge("Image", "info") : null,
          c.children && c.children.length > 0 ? badge(`${c.children.length} sub`, "neutral") : null,
        ),
        h(
          "div",
          { class: "cat-meta" },
          h("span", { class: "mono" }, c.slug),
          h("span", { class: "dot-sep" }, "·"),
          h("span", null, `${c.product_count ?? 0} product${(c.product_count ?? 0) === 1 ? "" : "s"}`),
          c.subtree_product_count > (c.product_count ?? 0)
            ? h("span", { class: "faint" }, ` (${c.subtree_product_count} in the branch)`)
            : null,
        ),
      ),
      ctx.can("catalog:write")
        ? h(
            "div",
            { class: "cat-actions" },
            h(
              "button",
              {
                class: "btn btn--ghost btn--icon btn--sm",
                type: "button",
                title: "Edit category",
                "aria-label": "Edit category",
                onclick: () => categoryDialog(c),
              },
              icon("edit", 13),
            ),
            h(
              "button",
              {
                class: "btn btn--ghost btn--icon btn--sm",
                type: "button",
                title: "Delete category",
                "aria-label": "Delete category",
                onclick: () => void deleteCategory(c),
              },
              icon("trash", 13),
            ),
          )
        : null,
    );

  // ------------------------------------------------------------- mutations --

  const toggleHidden = async (p: Product) => {
    try {
      await ctx.api.put(`/api/v1/staff/catalog/products/${p.id}`, {
        slug: p.slug,
        name: p.name,
        short_description: p.short_description,
        description: p.description,
        category_id: p.category_id,
        image_id: p.image_id,
        hidden: !p.hidden,
        sort_order: p.sort_order,
      });
      ctx.toast(p.hidden ? "Product published" : "Product hidden", { tone: "success" });
      await load();
    } catch (err) {
      report(err, "Could not change visibility");
    }
  };

  const deleteCategory = async (c: Category) => {
    const ok = await ctx.confirm({
      title: `Delete “${c.name}”?`,
      body: "A category can only be deleted when no product uses it and no sub-category hangs from it. Its products and its sub-categories are not deleted with it.",
      confirmLabel: "Delete category",
      danger: true,
    });
    if (!ok) return;
    try {
      await ctx.api.del(`/api/v1/staff/catalog/categories/${c.id}`);
      ctx.toast("Category deleted", { tone: "success" });
      await load();
    } catch (err) {
      report(err, "Could not delete the category");
    }
  };

  /** deleteProduct is the other half of "stop selling this".
   *
   *  Hiding is the right answer for a product anyone has already bought; deleting is for a draft
   *  that was never sold. The server refuses the delete while a service or an order references
   *  it, so the only thing the dialog has to do is say what the two words mean. */
  const deleteProduct = async (p: Product) => {
    const ok = await ctx.confirm({
      title: `Delete “${p.name}”?`,
      body: "This removes the product and its price versions. It is refused while a service or an order still references it — hide it instead, which keeps everything that was sold exactly as it is.",
      confirmLabel: "Delete product",
      danger: true,
    });
    if (!ok) return;
    try {
      await ctx.api.del(`/api/v1/staff/catalog/products/${p.id}`);
      ctx.toast("Product deleted", { tone: "success", detail: p.name });
      await load();
    } catch (err) {
      if (err instanceof ApiError && err.code === "in_use") {
        ctx.toast("This product has been sold", {
          tone: "danger",
          detail: err.detail || "Hide it instead: a delete would orphan the services and orders that reference it.",
          duration: 0,
        });
        return;
      }
      report(err, "Could not delete the product");
    }
  };

  const report = (err: unknown, fallback: string) => {
    if (err instanceof ApiError) {
      ctx.toast(err.detail || fallback, { tone: "danger", detail: err.code });
      return;
    }
    ctx.toast(fallback, { tone: "danger" });
  };

  // --------------------------------------------------------------- servers --

  // A server is a provisioner target: the node a product is sold onto. Creating one is the
  // only way a product can name a target other than its plan's region, and until now the
  // catalogue could list them and nothing else — a server created once could not be renamed
  // and could not be removed.
  const removeServer = async (server: Server) => {
    const ok = await ctx.confirm({
      title: `Delete “${server.name}”?`,
      body:
        server.product_count > 0
          ? `${server.product_count} product${server.product_count === 1 ? " is" : "s are"} sold on this server. The server refuses the delete while that is true: those products would silently fall back to their plan's region, which is a provisioning change nobody asked for. Unassign them first, or disable the server instead.`
          : "Nothing is sold on this server, so removing it changes no existing service. Products are assigned to a target when they are bought; a service already created keeps the target it was created with.",
      confirmLabel: "Delete server",
      danger: true,
    });
    if (!ok) return;
    try {
      await ctx.api.del(`/api/v1/staff/catalog/servers/${server.id}`);
      ctx.toast("Server deleted", { tone: "success", detail: server.name });
      await load();
    } catch (err) {
      if (err instanceof ApiError && err.code === "in_use") {
        ctx.toast("This server is still in use", {
          tone: "danger",
          detail: err.detail || "Move its products to another target, or disable it instead.",
          duration: 0,
        });
        return;
      }
      report(err, "Could not delete the server");
    }
  };

  const serverDialog = (existing: Server | null) => {
    const name = input({ id: "srv-name", value: existing?.name ?? "", placeholder: "Hetzner eu-west", required: true });
    const slug = input({
      id: "srv-slug",
      value: existing?.slug ?? "",
      placeholder: "hetzner-eu-west",
      class: "input--mono",
      required: true,
    });
    const provider = input({
      id: "srv-provider",
      value: existing?.provider ?? "",
      placeholder: "hetzner",
      class: "input--mono",
      autocomplete: "off",
    });
    const region = input({
      id: "srv-region",
      value: existing?.region ?? "",
      placeholder: "eu-west",
      class: "input--mono",
      autocomplete: "off",
    });
    const node = input({
      id: "srv-node",
      value: existing?.node ?? "",
      placeholder: "nbg1",
      class: "input--mono",
      autocomplete: "off",
    });
    const sortOrder = input({ id: "srv-sort", type: "number", value: existing?.sort_order ?? 0, min: 0 });
    const enabled = h("input", { type: "checkbox", checked: existing?.enabled ?? true });

    // The slug is derived from the name until it is edited, then left alone: it is stable and
    // appears in reports, so silently rewriting it a second time is a rename nobody asked for.
    let slugTouched = Boolean(existing);
    name.addEventListener("input", () => {
      if (slugTouched) return;
      slug.value = name.value
        .toLowerCase()
        .replace(/[^a-z0-9]+/g, "-")
        .replace(/^-+|-+$/g, "")
        .slice(0, 64);
    });
    slug.addEventListener("input", () => {
      slugTouched = true;
    });

    const body = h(
      "div",
      { class: "form-grid" },
      field({ label: "Name", control: name, id: "srv-name", required: true, class: "span-2" }),
      field({ label: "Slug", control: slug, id: "srv-slug", required: true, hint: "Stable identifier, used in reports." }),
      field({ label: "Sort order", control: sortOrder, id: "srv-sort", hint: "Lower shows first." }),
      field({
        label: "Provider",
        control: provider,
        id: "srv-provider",
        hint: "The provisioning extension this target belongs to. Empty means the configured one.",
      }),
      field({
        label: "Region",
        control: region,
        id: "srv-region",
        hint: "What the adapter receives as the region for a new service.",
      }),
      field({
        label: "Node",
        control: node,
        id: "srv-node",
        hint: "The provider's own node or group label, when it addresses targets by node id.",
      }),
      h("div", { class: "span-2" }, h("label", { class: "switch" }, enabled, "Enabled — offer it for new products")),
    );

    const submit = btn(existing ? "Save server" : "Create server", { variant: "primary", type: "submit" });
    const cancel = btn("Cancel", { variant: "ghost" });
    const errorHost = h("div");
    const form = h("form", { class: "stack", novalidate: true }, errorHost, body);

    bindForm(form, submit);
    const handle = openDialog({
      title: existing ? "Edit server" : "New server",
      subtitle: "A provisioner target: which node on the provider a product is sold onto.",
      body: h("div", { class: "stack" }, form),
      footer: h("div", { class: "row spacer" }, cancel, submit),
      locked: () => submit instanceof HTMLButtonElement && submit.disabled,
    });

    cancel.addEventListener("click", () => handle.close());

    form.addEventListener("submit", (event) => {
      event.preventDefault();
      void (async () => {
        errorHost.replaceChildren();
        if (!name.value.trim() || !slug.value.trim()) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "A name and a slug are required."));
          return;
        }
        if (submit instanceof HTMLButtonElement) submit.disabled = true;
        const payload = {
          slug: slug.value.trim(),
          name: name.value.trim(),
          provider: provider.value.trim(),
          region: region.value.trim(),
          node: node.value.trim(),
          enabled: enabled.checked,
          sort_order: Number(sortOrder.value) || 0,
        };
        try {
          if (existing) {
            await ctx.api.put(`/api/v1/staff/catalog/servers/${existing.id}`, payload);
          } else {
            await ctx.api.post("/api/v1/staff/catalog/servers", payload);
          }
          ctx.toast(existing ? "Server saved" : "Server created", { tone: "success", detail: payload.name });
          handle.close();
          await load();
        } catch (err) {
          const message =
            err instanceof ApiError
              ? err.code === "slug_taken"
                ? "That slug is already used by another server."
                : err.code === "invalid_input"
                  ? err.detail || "The server is not valid: check the slug and the name."
                  : err.detail || "The server could not be saved."
              : "The server could not be reached.";
          swap(errorHost, h("div", { class: "banner banner--danger" }, message));
        } finally {
          if (submit instanceof HTMLButtonElement) submit.disabled = false;
        }
      })();
    });
  };

  const serverRow = (server: Server, canWrite: boolean): HTMLElement =>
    h(
      "div",
      { class: "cat-row" },
      h(
        "div",
        { class: "cat-main" },
        h(
          "div",
          { class: "cat-name" },
          h("span", { class: "row-title" }, server.name),
          server.enabled ? badge("Enabled", "success", { dot: true }) : badge("Disabled", "neutral", { dot: true }),
          server.product_count > 0
            ? badge(`${server.product_count} product${server.product_count === 1 ? "" : "s"}`, "info")
            : null,
        ),
        h(
          "div",
          { class: "cat-meta" },
          h("span", { class: "mono" }, server.slug),
          server.provider ? h("span", { class: "dot-sep" }, "·") : null,
          server.provider ? h("span", null, server.provider) : null,
          server.region ? h("span", { class: "dot-sep" }, "·") : null,
          server.region ? h("span", { class: "mono" }, server.region) : null,
          server.node ? h("span", { class: "dot-sep" }, "·") : null,
          server.node ? h("span", { class: "mono" }, server.node) : null,
        ),
      ),
      canWrite
        ? h(
            "div",
            { class: "row" },
            btn("Edit", { size: "sm", icon: "edit", onclick: () => serverDialog(server) }),
            btn("Delete", {
              size: "sm",
              variant: "danger",
              icon: "trash",
              // Not disabled when products reference it: the refusal is the server's, and a
              // disabled button would hide the reason. The dialog says what will happen.
              onclick: () => void removeServer(server),
            }),
          )
        : null,
    );

  // --------------------------------------------------------------- dialogs --

  const categoryDialog = (existing: CategoryNode | null) => {
    const name = input({ id: "cat-name", value: existing?.name ?? "", placeholder: "VPS Hosting", required: true });
    const slug = input({
      id: "cat-slug",
      value: existing?.slug ?? "",
      placeholder: "vps-hosting",
      class: "input--mono",
      required: true,
    });
    const description = textarea({
      id: "cat-desc",
      value: existing?.description ?? "",
      placeholder: "Fast virtual servers for people who outgrew shared hosting.",
    });
    const sortOrder = input({ id: "cat-sort", type: "number", value: existing?.sort_order ?? 0, min: 0 });
    const hidden = h("input", { type: "checkbox", checked: existing?.hidden ?? false });

    // The category's logo (CAT-01 / SF-06). It uses the same upload route and the same picker as
    // a product logo, because it is the same deduped image store and the same 256 KiB cap — a
    // second uploader would be a second allowlist to keep in step.
    const imageHost = h("div", { class: "logo-picker" });
    let imageId: string | null = existing?.image_id ?? null;
    renderLogoPicker(imageHost, mediaURL(existing?.image_id), (next) => {
      imageId = next;
    });

    // The parent control. A category cannot be moved under itself or under one of its own
    // descendants, and the database refuses both; offering them in a dropdown would be an
    // invitation to a 409, so they are not offered. Depth is capped at four by the database too,
    // which is why the option label shows the level.
    const excluded = existing ? subtreeIDs(existing.id, categoryTree) : new Set<string>();
    const parentOptions = [
      { value: "", label: "No parent (top level)" },
      ...flattenTree(categoryTree)
        .filter((entry) => !excluded.has(entry.node.id))
        .map((entry) => ({
          value: entry.node.id,
          label: `${"\u00a0\u00a0".repeat(entry.depth - 1)}${entry.node.name}`,
        })),
    ];
    const parent = select(parentOptions, { id: "cat-parent", value: existing?.parent_id ?? "" });

    // A slug is derived from the name until the operator edits it, then left alone: silently
    // rewriting a slug a second time would change a URL that may already be linked.
    let slugTouched = Boolean(existing);
    name.addEventListener("input", () => {
      if (slugTouched) return;
      slug.value = name.value
        .toLowerCase()
        .replace(/[^a-z0-9]+/g, "-")
        .replace(/^-+|-+$/g, "")
        .slice(0, 64);
    });
    slug.addEventListener("input", () => {
      slugTouched = true;
    });

    const body = h(
      "div",
      { class: "form-grid" },
      field({ label: "Name", control: name, id: "cat-name", required: true, class: "span-2" }),
      field({ label: "Slug", control: slug, id: "cat-slug", hint: "Used in the storefront URL.", required: true }),
      field({ label: "Sort order", control: sortOrder, id: "cat-sort", hint: "Lower shows first." }),
      field({
        label: "Parent category",
        control: parent,
        id: "cat-parent",
        hint: "Four levels at most. A parent's page lists the products of everything under it.",
        class: "span-2",
      }),
      field({ label: "Description", control: description, id: "cat-desc", class: "span-2" }),
      field({
        label: "Image",
        control: imageHost,
        id: "cat-image",
        class: "span-2",
        hint: "Shown in the storefront's category strip. PNG, JPEG, WebP or SVG, up to 256 KiB.",
      }),
      h("div", { class: "span-2" }, h("label", { class: "switch" }, hidden, "Hide from the storefront")),
    );

    const submit = btn(existing ? "Save category" : "Create category", { variant: "primary", type: "submit" });
    const cancel = btn("Cancel", { variant: "ghost" });
    const errorHost = h("div");
    const formId = "category-form";

    const form = h(
      "form",
      { class: "stack", id: formId, novalidate: true },
      errorHost,
      body,
    );

    bindForm(form, submit);
    const handle = openDialog({
      title: existing ? "Edit category" : "New category",
      subtitle: "A category groups products in the storefront.",
      body: h("div", { class: "stack" }, form),
      footer: h("div", { class: "row spacer" }, cancel, submit),
      locked: () => submit instanceof HTMLButtonElement && submit.disabled,
    });

    cancel.addEventListener("click", () => handle.close());

    form.addEventListener("submit", (event) => {
      event.preventDefault();
      void (async () => {
        errorHost.replaceChildren();
        if (!name.value.trim() || !slug.value.trim()) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "A name and a slug are required."));
          return;
        }
        if (submit instanceof HTMLButtonElement) submit.disabled = true;
        const payload = {
          slug: slug.value.trim(),
          name: name.value.trim(),
          description: description.value.trim(),
          image_id: imageId,
          sort_order: Number(sortOrder.value) || 0,
          hidden: hidden.checked,
        };
        try {
          if (existing) {
            await ctx.api.put(`/api/v1/staff/catalog/categories/${existing.id}`, payload);
            // The edit endpoint predates the tree and has no parent field, so a move is its own
            // request — sent only when it changed, because moving a branch is a structural edit
            // worth its own audit row rather than part of a rename.
            if ((existing.parent_id ?? "") !== parent.value) {
              await ctx.api.put(`/api/v1/staff/catalog/categories/${existing.id}/parent`, {
                parent_id: parent.value || null,
              });
            }
          } else {
            const created = await ctx.api.post<Category>("/api/v1/staff/catalog/categories", payload);
            if (parent.value && created?.id) {
              await ctx.api.put(`/api/v1/staff/catalog/categories/${created.id}/parent`, {
                parent_id: parent.value,
              });
            }
          }
          ctx.toast(existing ? "Category saved" : "Category created", { tone: "success" });
          handle.close();
          await load();
        } catch (err) {
          const message =
            err instanceof ApiError
              ? err.code === "slug_taken"
                ? "That slug is already used by another category."
                : err.code === "category_cycle"
                  ? "That would put the category inside itself."
                  : err.code === "category_too_deep"
                    ? "Categories can only nest four levels deep."
                    : err.detail || "The category could not be saved."
              : "The server could not be reached.";
          swap(errorHost, h("div", { class: "banner banner--danger" }, message));
        } finally {
          if (submit instanceof HTMLButtonElement) submit.disabled = false;
        }
      })();
    });
  };

  const productDialog = (existing: Product | null) => {
    const name = input({ id: "prod-name", value: existing?.name ?? "", placeholder: "VPS Small", required: true });
    const slug = input({
      id: "prod-slug",
      value: existing?.slug ?? "",
      placeholder: "vps-small",
      class: "input--mono",
      required: true,
    });
    const shortDesc = input({
      id: "prod-short",
      value: existing?.short_description ?? "",
      placeholder: "2 vCPU, 4 GB RAM, 80 GB NVMe",
      maxlength: 160,
    });
    const description = textarea({
      id: "prod-desc",
      value: existing?.description ?? "",
      placeholder: "Describe what the customer gets. Two or three sentences is plenty.",
    });
    const category = select(
      [{ value: "", label: "No category" }, ...categories.map((c) => ({ value: c.id, label: c.path.replace(/\//g, " \u203a ") }))],
      { id: "prod-category", value: existing?.category_id ?? "" },
    );
    // Which provisioner target the product is sold onto. Empty means "whatever the plan says",
    // which is what every product did before this control existed.
    const serverOptions = [
      { value: "", label: "Inherit from the plan" },
      ...servers.map((s) => ({
        value: s.id,
        label: s.region ? `${s.name} (${s.region})` : s.name,
        disabled: !s.enabled,
      })),
    ];
    const serverSelect = select(serverOptions, { id: "prod-server", value: "" });
    const sortOrder = input({ id: "prod-sort", type: "number", value: existing?.sort_order ?? 0, min: 0 });
    const hidden = h("input", { type: "checkbox", checked: existing?.hidden ?? false });

    let slugTouched = Boolean(existing);
    name.addEventListener("input", () => {
      if (slugTouched) return;
      slug.value = name.value
        .toLowerCase()
        .replace(/[^a-z0-9]+/g, "-")
        .replace(/^-+|-+$/g, "")
        .slice(0, 64);
    });
    slug.addEventListener("input", () => {
      slugTouched = true;
    });

    const imageHost = h("div", { class: "logo-picker" });
    let imageId: string | null = existing?.image_id ?? null;
    renderLogoPicker(imageHost, existing?.image_url ?? "", (next) => {
      imageId = next;
    });

    const body = h(
      "div",
      { class: "form-grid" },
      field({ label: "Name", control: name, id: "prod-name", required: true, class: "span-2" }),
      field({ label: "Slug", control: slug, id: "prod-slug", hint: "Used in the storefront URL.", required: true }),
      field({ label: "Category", control: category, id: "prod-category" }),
      field({
        label: "Server",
        control: serverSelect,
        id: "prod-server",
        hint: "The node this product provisions onto. A plan that names its own region wins.",
      }),
      field({
        label: "Short description",
        control: shortDesc,
        id: "prod-short",
        hint: "One line, shown on the card.",
        class: "span-2",
      }),
      field({ label: "Description", control: description, id: "prod-desc", class: "span-2" }),
      field({ label: "Sort order", control: sortOrder, id: "prod-sort", hint: "Lower shows first in its category." }),
      h("div", { class: "span-2" }, h("label", { class: "switch" }, hidden, "Hide from the storefront")),
      field({ label: "Logo", control: imageHost, id: "prod-logo", class: "span-2", hint: "PNG, JPEG, WebP or SVG, up to 256 KiB." }),
    );

    const submit = btn(existing ? "Save product" : "Create product", { variant: "primary", type: "submit" });
    const cancel = btn("Cancel", { variant: "ghost" });
    const errorHost = h("div");
    const form = h("form", { class: "stack", novalidate: true }, errorHost, body);

    // The product read is the storefront's shape as well, so it carries no server: the placement
    // is its own read. It is loaded after the dialog opens rather than before, because a failed
    // placement read must not stop an operator from renaming a product.
    let initialServerID: string | null = null;
    if (existing) {
      void (async () => {
        try {
          const placement = await ctx.api.get<ProductPlacement>(
            `/api/v1/staff/catalog/products/${existing.id}/placement`,
            { cache: false },
          );
          initialServerID = placement.server_id;
          serverSelect.value = placement.server_id ?? "";
        } catch {
          // Leaving the selector on "inherit" is the honest fallback: it matches what the
          // product does when the read fails.
        }
      })();
    }

    bindForm(form, submit);
    const handle = openDialog({
      title: existing ? "Edit product" : "New product",
      subtitle: "A product is what the storefront sells. Its price is added separately, as a version.",
      body: h("div", { class: "stack" }, form),
      footer: h("div", { class: "row spacer" }, cancel, submit),
      wide: true,
      locked: () => submit instanceof HTMLButtonElement && submit.disabled,
    });
    cancel.addEventListener("click", () => handle.close());

    form.addEventListener("submit", (event) => {
      event.preventDefault();
      void (async () => {
        errorHost.replaceChildren();
        if (!name.value.trim() || !slug.value.trim()) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "A name and a slug are required."));
          return;
        }
        if (submit instanceof HTMLButtonElement) submit.disabled = true;
        const payload = {
          slug: slug.value.trim(),
          name: name.value.trim(),
          short_description: shortDesc.value.trim(),
          description: description.value.trim(),
          category_id: category.value || null,
          image_id: imageId,
          hidden: hidden.checked,
          sort_order: Number(sortOrder.value) || 0,
        };
        try {
          const chosenServer = serverSelect.value || null;
          if (existing) {
            await ctx.api.put(`/api/v1/staff/catalog/products/${existing.id}`, payload);
            // The product edit endpoint carries catalogue metadata and predates this column, so
            // the assignment is its own request — and only when it changed, because moving a
            // product onto another node is a provisioning decision, not part of a rename.
            if (chosenServer !== initialServerID) {
              await ctx.api.put(`/api/v1/staff/catalog/products/${existing.id}/server`, {
                server_id: chosenServer,
              });
            }
          } else {
            const created = await ctx.api.post<Product>("/api/v1/staff/catalog/products", payload);
            if (chosenServer && created?.id) {
              await ctx.api.put(`/api/v1/staff/catalog/products/${created.id}/server`, {
                server_id: chosenServer,
              });
            }
            ctx.toast("Product created", { tone: "success", detail: "Add a price to publish it." });
            handle.close();
            await load();
            // A product with no price is invisible in the storefront, so the next step is
            // offered rather than left to be discovered.
            priceDialog(created);
            return;
          }
          ctx.toast("Product saved", { tone: "success" });
          handle.close();
          await load();
        } catch (err) {
          const message =
            err instanceof ApiError
              ? err.code === "slug_taken"
                ? "That slug is already used by another product."
                : err.detail || "The product could not be saved."
              : "The server could not be reached.";
          swap(errorHost, h("div", { class: "banner banner--danger" }, message));
        } finally {
          if (submit instanceof HTMLButtonElement) submit.disabled = false;
        }
      })();
    });
  };

  /** The logo picker: preview, upload, remove. Upload happens on choose, not on submit, so a
   *  failure is reported while the dialog is still open and the operator can retry. */
  const renderLogoPicker = (host: HTMLElement, url: string, onChange: (id: string | null) => void) => {
    const preview = h("div", { class: "logo-preview" });
    const setPreview = (src: string, name: string) => {
      swap(
        preview,
        src
          ? h("img", { src, alt: "Current logo", decoding: "async" })
          : h("span", { class: "store-card-initial" }, (name.charAt(0) || "?").toUpperCase()),
      );
    };
    setPreview(url, "");

    const uploadLogo = async (file: File) => {
      try {
        // A raw-body upload: the content type is the file's own type, and the response carries
        // the id to store on the product.
        const res = await fetch("/api/v1/staff/catalog/images", {
          method: "POST",
          credentials: "same-origin",
          headers: {
            "Content-Type": file.type,
            "X-CSRF-Token": ctx.session.csrfToken,
            "Idempotency-Key": crypto.randomUUID(),
          },
          body: file,
        });
        if (!res.ok) {
          const problem = await res.json().catch(() => ({}));
          throw new ApiError(res.status, problem);
        }
        const uploaded = (await res.json()) as { id: string; url: string };
        onChange(uploaded.id);
        setPreview(uploaded.url, "");
        ctx.toast("Logo uploaded", { tone: "success" });
      } catch (err) {
        report(err, "The logo could not be uploaded");
      }
    };

    const fileInput = h("input", {
      type: "file",
      accept: "image/png,image/jpeg,image/webp,image/svg+xml",
      class: "sr-only",
      id: "logo-file",
      onchange: (event: Event) => {
        const file = (event.target as HTMLInputElement).files?.[0];
        if (!file) return;
        void (async () => {
          if (file.size > 256 * 1024) {
            ctx.toast("That image is too large", { tone: "danger", detail: "The limit is 256 KiB." });
            return;
          }
          await uploadLogo(file);
        })();
      },
    });

    const pick = btn("Choose file", {
      size: "sm",
      icon: "download",
      onclick: () => fileInput.click(),
    });
    const remove = btn("Remove", {
      size: "sm",
      variant: "ghost",
      onclick: () => {
        onChange(null);
        setPreview("", "");
      },
    });

    swap(
      host,
      h(
        "div",
        { class: "row" },
        preview,
        h("div", { class: "stack-sm" }, h("div", { class: "row" }, pick, remove), fileInput),
      ),
    );
  };

  const priceDialog = (product: Product) => {
    const name = input({ id: "price-name", placeholder: "Monthly", required: true });
    const currency = input({ id: "price-currency", value: "USD", class: "input--mono", maxlength: 12, required: true });
    const amount = input({ id: "price-amount", placeholder: "12.00", inputmode: "decimal", class: "input--mono", required: true });
    const setup = input({ id: "price-setup", placeholder: "0.00", inputmode: "decimal", class: "input--mono" });
    const interval = select(INTERVALS, { id: "price-interval", value: "monthly" });
    // The day count for "Every N days". It is shown only when that interval is selected, and
    // the field is hidden with the `hidden` attribute rather than left visible-but-ignored: a
    // number that does nothing is a field an operator fills in and then wonders about.
    const intervalDays = input({ id: "price-interval-days", type: "number", value: 30, min: 1, max: 366 });
    const intervalDaysField = field({
      label: "Days between bills",
      control: intervalDays,
      id: "price-interval-days",
      hint: "1 to 366. A 45-day period bills on the 45th day, not on a calendar month.",
    });
    intervalDaysField.hidden = true;
    const syncIntervalDays = () => {
      intervalDaysField.hidden = interval.value !== "every_days";
    };
    interval.addEventListener("change", syncIntervalDays);
    syncIntervalDays();
    const anchorDay = input({ id: "price-anchor-day", type: "number", value: 1, min: 1, max: 31 });
    const timezone = input({ id: "price-tz", value: "UTC", class: "input--mono", placeholder: "UTC" });
    const plan = input({ id: "price-plan", placeholder: "vps.small", class: "input--mono" });
    const region = input({ id: "price-region", placeholder: "us-east", class: "input--mono" });

    const body = h(
      "div",
      { class: "form-grid" },
      field({
        label: "Plan name",
        control: name,
        id: "price-name",
        required: true,
        hint: "What the customer sees: “Monthly”, “8 GB”.",
        class: "span-2",
      }),
      field({ label: "Currency", control: currency, id: "price-currency", required: true }),
      field({ label: "Amount", control: amount, id: "price-amount", required: true, hint: "In major units, e.g. 12.00" }),
      field({ label: "Setup fee", control: setup, id: "price-setup", hint: "Optional." }),
      field({
        label: "Billing interval",
        control: interval,
        id: "price-interval",
        hint: "One-time and free plans never renew; the server refuses a free plan that costs anything.",
      }),
      intervalDaysField,
      field({ label: "Anchor day", control: anchorDay, id: "price-anchor-day", hint: "Day of the month the period starts." }),
      field({ label: "Anchor timezone", control: timezone, id: "price-tz", hint: "IANA name, e.g. Europe/Berlin." }),
      field({ label: "Provision plan", control: plan, id: "price-plan", hint: "Passed to the provisioning extension." }),
      field({ label: "Provision region", control: region, id: "price-region" }),
    );

    const submit = btn("Add price version", { variant: "primary", type: "submit" });
    const cancel = btn("Cancel", { variant: "ghost" });
    const errorHost = h("div");
    const form = h("form", { class: "stack", novalidate: true }, errorHost, body);

    bindForm(form, submit);
    const handle = openDialog({
      title: "Add a price version",
      subtitle: `${product.name} — a new version is appended; the old one is never edited.`,
      body: h("div", { class: "stack" }, form),
      footer: h("div", { class: "row spacer" }, cancel, submit),
      wide: true,
      locked: () => submit instanceof HTMLButtonElement && submit.disabled,
    });
    cancel.addEventListener("click", () => handle.close());

    form.addEventListener("submit", (event) => {
      event.preventDefault();
      void (async () => {
        errorHost.replaceChildren();
        const minor = toMinor(amount.value);
        const setupMinor = setup.value.trim() === "" ? 0 : toMinor(setup.value);
        if (minor === null || setupMinor === null) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "Amounts must be numbers, e.g. 12.00."));
          return;
        }
        // An arbitrary period is `every_<N>_days` on the wire. The sentinel is never sent: a
        // server that received it would have to guess the number, and guessing is how a price
        // ends up billing on a schedule nobody chose.
        let wireInterval = interval.value;
        if (wireInterval === "every_days") {
          const days = Number(intervalDays.value);
          if (!Number.isInteger(days) || days < 1 || days > 366) {
            swap(
              errorHost,
              h("div", { class: "banner banner--danger" }, "The number of days must be a whole number between 1 and 366."),
            );
            return;
          }
          wireInterval = `every_${days}_days`;
        }
        // A free plan is zero by definition, and the server refuses one that costs anything. Sending
        // zero rather than failing is the honest reading of the operator's choice: they asked for a
        // free plan, not for a free plan with a price they forgot to clear.
        const finalMinor = wireInterval === "free" ? 0 : minor;
        const finalSetup = wireInterval === "free" ? 0 : setupMinor;
        if (submit instanceof HTMLButtonElement) submit.disabled = true;
        try {
          await ctx.api.post(`/api/v1/staff/catalog/products/${product.id}/prices`, {
            name: name.value.trim(),
            currency: currency.value.trim().toUpperCase(),
            amount_minor: finalMinor,
            setup_minor: finalSetup,
            interval: wireInterval,
            anchor_day: Number(anchorDay.value) || 1,
            anchor_timezone: timezone.value.trim() || "UTC",
            provision_plan: plan.value.trim(),
            provision_region: region.value.trim(),
          });
          ctx.toast("Price version added", { tone: "success" });
          handle.close();
          await load();
        } catch (err) {
          const message = err instanceof ApiError ? err.detail || "The price could not be added." : "The server could not be reached.";
          swap(errorHost, h("div", { class: "banner banner--danger" }, message));
        } finally {
          if (submit instanceof HTMLButtonElement) submit.disabled = false;
        }
      })();
    });
  };

  void load();
  return host;
}

/** Parses a major-unit amount into minor units without floating point. */
function toMinor(raw: string): number | null {
  const cleaned = raw.trim().replace(/[,\s]/g, "");
  if (!/^\d*(\.\d{0,})?$/.test(cleaned) || cleaned === "" || cleaned === ".") return null;
  const [whole = "0", fraction = ""] = cleaned.split(".");
  const padded = (fraction + "00").slice(0, 2);
  return Number(whole) * 100 + Number(padded);
}

/** flattenTree walks the category tree in display order, carrying each node's depth. */
function flattenTree(
  nodes: CategoryNode[],
  depth = 1,
  out: Array<{ node: CategoryNode; depth: number }> = [],
): Array<{ node: CategoryNode; depth: number }> {
  for (const node of nodes) {
    out.push({ node, depth });
    if (node.children && node.children.length > 0) flattenTree(node.children, depth + 1, out);
  }
  return out;
}

/** subtreeIDs is a node's id and every id below it. A category cannot be moved into that set,
 *  because the move would make it its own ancestor — the database refuses it, so the parent
 *  dropdown does not offer it. */
function subtreeIDs(rootID: string, tree: CategoryNode[]): Set<string> {
  const ids = new Set<string>([rootID]);
  const entries = flattenTree(tree);
  let grew = true;
  while (grew) {
    grew = false;
    for (const entry of entries) {
      const parentID = entry.node.parent_id;
      if (parentID && ids.has(parentID) && !ids.has(entry.node.id)) {
        ids.add(entry.node.id);
        grew = true;
      }
    }
  }
  return ids;
}
