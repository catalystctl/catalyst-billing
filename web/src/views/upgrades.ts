// The product upgrade graph.
//
// `product_upgrades` is one directed table and a downgrade is the same edge read backwards,
// so the graph is exactly the promise the storefront makes: from this product, a customer may
// move to that one. `internal/service` enforces it (`reachable`), and the customer's upgrade
// options are computed from it.
//
// The screen has to say what moving *costs*, in words a person can check, because the number a
// customer sees is not either price on this page. `internal/service/upgrades.go` computes:
//
//	proration  = time remaining in the current period / the whole period   (an exact fraction)
//	charge     = (target price − current price) × proration                (rounded half-up, once)
//
// So an upgrade issues an invoice for the difference scaled by the time left — not the new
// price — and the service keeps its **old** price until that invoice is paid
// (`ApplyPendingUpgrades` is what finally moves it). A downgrade is the same arithmetic with a
// negative sign: the row records a credit the customer is owed, and the service moves down only
// once that credit is posted. The page states both, because "the customer was charged 13.00 and
// their service still says 12.00" is a support ticket waiting to happen.
//
// Two facts the table makes visible rather than leaving to be discovered:
//
//  - An edge between products with no shared currency and interval is accepted and flagged.
//    Upgrade options are only offered in the subscription's own currency and interval (this
//    system has no exchange rate), so such an edge is invisible to every customer. Saying so is
//    the difference between staging the graph and believing it works.
//  - A self-edge is refused. The database refuses it (`product_upgrades_not_self`) and the API
//    turns that into `self_edge`; this screen shows the sentence next to the form instead of a
//    500.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { humanStatus, relative, type Amount } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  badge,
  banner,
  btn,
  card,
  emptyState,
  errorState,
  field,
  moneyEl,
  pageHead,
  select,
  skeletonPage,
  withBusy,
} from "../ui/kit";

export const meta = { title: "Upgrade graph", crumb: "Upgrades" };

// ---------------------------------------------------------------- payloads --

interface GraphPrice {
  price_version_id: string;
  version: number;
  name: string;
  currency: string;
  amount_minor: number;
  setup_minor: number;
  interval: string;
  amount?: Amount;
}

interface GraphProduct {
  id: string;
  name: string;
  slug: string;
  hidden: boolean;
  prices: GraphPrice[] | null;
}

interface GraphEdge {
  product_id: string;
  product_name?: string;
  product_slug?: string;
  product_hidden?: boolean;
  upgrade_to_id: string;
  upgrade_to_name?: string;
  upgrade_to_slug?: string;
  upgrade_to_hidden?: boolean;
  created_at?: string;
  compatible: boolean;
  compatible_prices: string[] | null;
  from_prices?: GraphPrice[] | null;
  to_prices?: GraphPrice[] | null;
  warning?: string;
}

interface GraphResponse {
  edges: GraphEdge[] | null;
  products: GraphProduct[] | null;
}

export default async function upgradesPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("catalog:write")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Upgrade graph" }),
      banner(
        "danger",
        "You cannot change the upgrade graph",
        "The graph is catalog configuration: adding or removing an edge needs the catalog:write permission.",
        btn("Back to the dashboard", { href: "/", size: "sm", variant: "ghost" }),
      ),
    );
  }

  const host = h("div", { class: "page" }, skeletonPage());

  const load = async (): Promise<void> => {
    try {
      const data = await ctx.api.get<GraphResponse>("/api/v1/staff/upgrade-graph", { cache: false });
      swap(host, render(data));
    } catch (err) {
      swap(host, errorState(err, () => void load()));
    }
  };

  const render = (data: GraphResponse): Node => {
    const products = data.products ?? [];
    const edges = data.edges ?? [];
    const invisible = edges.filter((e) => !e.compatible).length;

    return h(
      "div",
      { class: "page" },
      pageHead({
        title: "Upgrade graph",
        sub: "Which product a customer may move to from which. A downgrade is the same edge read backwards, so there is one table and one screen.",
        actions: btn("Catalog", { href: "/catalog", size: "sm", variant: "ghost", icon: "box" }),
      }),

      banner(
        "info",
        "What moving costs, exactly",
        // Prose, not a one-line explanation: each claim is its own paragraph so the two bold
        // phrases do not run into each other as a block of emphasis. `banner-prose` (pages.css)
        // gives the body normal leading and a gap between the paragraphs.
        h(
          "div",
          { class: "banner-prose" },
          h(
            "p",
            null,
            "A mid-period move charges the difference for the part of the period that is left: ",
            h("code", { class: "inline-code" }, "(new price − current price) × time remaining / period length"),
            ", rounded half-up once. The prices below are whole-period prices — the charge is the difference between them, scaled by the time left.",
          ),
          h(
            "p",
            null,
            h("strong", null, "An upgrade issues an invoice"),
            " for that difference, and the service keeps its old price until the invoice is paid.",
          ),
          h(
            "p",
            null,
            h("strong", null, "A downgrade records a credit"),
            " the customer is owed; the service moves down only once that credit is posted.",
          ),
          h("p", null, "Neither happens when the edge is created — an edge is permission to move, not a move."),
        ),
      ),

      invisible > 0
        ? banner(
            "warning",
            `${invisible} edge${invisible === 1 ? "" : "s"} will never be offered`,
            "Both products must be priced in the same currency and billing interval for the option to appear: this panel has no exchange rate, so a price in another currency is not a price a customer can move to.",
          )
        : null,

      h(
        "div",
        { class: "grid-4" },
        stat("Products", products.length, "in the catalog"),
        stat("Edges", edges.length, "permitted moves"),
        stat("Offerable", edges.length - invisible, "visible to customers"),
        stat("Invisible", invisible, "no shared currency and interval"),
      ),

      addEdgeCard(products),

      card({
        title: "Edges",
        sub: `${edges.length} in the graph`,
        flush: true,
        body:
          edges.length === 0
            ? emptyState({
                title: "No edges yet",
                body: "With no edges, no customer can move between products: every upgrade option list is empty. Add the pairs that are comparable.",
                icon: "layers",
              })
            : h("div", { class: "table-wrap table-wrap--cards" }, edgeTable(edges)),
      }),
    );
  };

  // ---------------------------------------------------------------- add edge --

  const addEdgeCard = (products: GraphProduct[]): HTMLElement => {
    if (products.length < 2) {
      return card({
        title: "Add an edge",
        body: h(
          "p",
          { class: "field-hint" },
          "The graph needs at least two products. Create them in the catalog first, with a price: an edge between unpriced products is invisible to every customer.",
        ),
      });
    }

    const from = select(
      products.map((p) => ({
        value: p.id,
        label: `${p.name}${p.hidden ? " (hidden)" : ""}${(p.prices ?? []).length === 0 ? " — no price" : ""}`,
      })),
      { id: "edge-from" },
    );
    const to = select(
      products.map((p) => ({
        value: p.id,
        label: `${p.name}${p.hidden ? " (hidden)" : ""}${(p.prices ?? []).length === 0 ? " — no price" : ""}`,
      })),
      { id: "edge-to" },
    );
    // Default the target to the second product so the first click is not "the same product",
    // which is the one answer the server refuses.
    const second = products[1];
    if (second) to.value = second.id;

    const errorHost = h("div");
    const submit = btn("Add edge", { variant: "primary", icon: "plus" }) as HTMLButtonElement;

    submit.addEventListener("click", () => {
      void withBusy(ctx, submit, async () => {
        errorHost.replaceChildren();
        if (from.value === to.value) {
          // Said here as well as on the server: the database refuses a self-edge, and waiting
          // for a round trip to learn that is a worse screen.
          swap(errorHost, inlineError("A product cannot be its own upgrade target. Pick two different products."));
          return;
        }
        try {
          const created = await ctx.api.post<GraphEdge>("/api/v1/staff/upgrade-graph", {
            product_id: from.value,
            upgrade_to_id: to.value,
          });
          if (created?.compatible === false) {
            ctx.toast("Edge added, but no customer will see it", {
              tone: "info",
              detail: created.warning ?? "The two products share no currency and billing interval.",
            });
          } else {
            ctx.toast("Edge added", {
              tone: "success",
              detail: "It permits a move; it does not move anybody or charge anything.",
            });
          }
          await load();
        } catch (err) {
          if (!(err instanceof ApiError)) {
            swap(errorHost, inlineError("The server could not be reached, so nothing changed."));
            return;
          }
          switch (err.code) {
            case "self_edge":
              swap(errorHost, inlineError("A product cannot be its own upgrade target."));
              break;
            case "edge_exists":
              swap(errorHost, inlineError("That edge is already in the graph."));
              break;
            case "not_found":
              swap(errorHost, inlineError("One of the products no longer exists. Reload the page."));
              break;
            case "forbidden":
              swap(errorHost, inlineError("Adding an edge needs the catalog:write permission."));
              break;
            default:
              swap(errorHost, inlineError(err.detail || `The edge could not be added (${err.code}).`));
          }
        }
      });
    });

    return card({
      title: "Add an edge",
      sub: "From one product to another",
      body: h(
        "div",
        { class: "stack" },
        errorHost,
        h(
          "div",
          { class: "filter-bar" },
          field({ label: "From", control: from, id: "edge-from", class: "filter-bar__grow filter-bar__grow--capped" }),
          h("span", { class: "faint filter-bar__arrow", "aria-hidden": "true" }, icon("arrowRight", 14)),
          field({ label: "May move to", control: to, id: "edge-to", class: "filter-bar__grow filter-bar__grow--capped" }),
          submit,
        ),
        h(
          "p",
          { class: "field-hint" },
          "The edge is a permission, not a price: it makes the target product's prices available as an upgrade option, and the proration is computed when a customer actually moves.",
        ),
      ),
    });
  };

  // ------------------------------------------------------------------ table --

  const edgeTable = (edges: GraphEdge[]): HTMLElement =>
    h(
      "table",
      { class: "table" },
      h(
        "thead",
        null,
        h(
          "tr",
          null,
          h("th", { scope: "col" }, "From"),
          h("th", { scope: "col" }, "May move to"),
          h("th", { scope: "col" }, "Difference per period"),
          h("th", { scope: "col", class: "hide-sm" }, "Offered"),
          h("th", { scope: "col", class: "num" }, ""),
        ),
      ),
      h("tbody", null, ...edges.map(edgeRow)),
    );

  const edgeRow = (edge: GraphEdge): HTMLElement => {
    const name = edge.product_name ?? edge.product_id;
    const target = edge.upgrade_to_name ?? edge.upgrade_to_id;
    return h(
      "tr",
      null,
      h(
        "td",
        { class: "cell-primary" },
        h(
          "div",
          { class: "cell-main" },
          h(
            "div",
            { class: "primary" },
            h("a", { class: "row-title", href: `/catalog-management/${encodeURIComponent(edge.product_id)}` }, name),
            edge.product_hidden ? h("span", { class: "faint" }, " hidden") : null,
          ),
          h(
            "div",
            { class: "secondary" },
            edge.created_at ? `added ${relative(edge.created_at)}` : "edge",
          ),
        ),
      ),
      h(
        "td",
        { "data-label": "May move to" },
        h(
          "div",
          { class: "cell-main" },
          h(
            "div",
            { class: "primary" },
            h(
              "a",
              { class: "row-title", href: `/catalog-management/${encodeURIComponent(edge.upgrade_to_id)}` },
              target,
            ),
            edge.upgrade_to_hidden ? h("span", { class: "faint" }, " hidden") : null,
          ),
          h("div", { class: "secondary" }, "a downgrade is this edge read backwards"),
        ),
      ),
      h("td", { "data-label": "Difference per period" }, difference(edge)),
      h(
        "td",
        { class: "hide-sm", "data-label": "Offered" },
        edge.compatible
          ? badge("Offered", "success", { dot: true })
          : h(
              "span",
              { class: "stack-sm", title: edge.warning ?? "no shared currency and interval" },
              badge("Invisible", "warning", { dot: true }),
              h("span", { class: "faint" }, "no shared currency/interval"),
            ),
      ),
      h(
        "td",
        { class: "num", "data-label": "Actions" },
        btn("Remove", {
          size: "sm",
          variant: "danger",
          icon: "trash",
          title: "Remove this edge",
          onclick: () => void remove(edge),
        }),
      ),
    );
  };

  /** The whole-period difference for each currency and interval the two products share.
   *
   *  This is integer subtraction of minor units, which is exact — the same arithmetic the
   *  server does before it applies the proration ratio. The screen never divides: dividing here
   *  would produce a second, possibly different, answer to "what will they be charged". */
  const difference = (edge: GraphEdge): Node => {
    const from = edge.from_prices ?? [];
    const to = edge.to_prices ?? [];
    const pairs: Array<{ currency: string; interval: string; delta: number; amount: Amount }> = [];
    for (const target of to) {
      const match = from.find((p) => p.currency === target.currency && p.interval === target.interval);
      if (!match) continue;
      if (pairs.some((p) => p.currency === target.currency && p.interval === target.interval)) continue;
      pairs.push({
        currency: target.currency,
        interval: target.interval,
        delta: target.amount_minor - match.amount_minor,
        amount: target.amount ?? { minor: target.amount_minor, currency: target.currency, scale: 2 },
      });
    }
    if (pairs.length === 0) return h("span", { class: "faint" }, "—");
    return h(
      "div",
      { class: "stack-sm" },
      ...pairs.map((pair) =>
        h(
          "div",
          { class: "row" },
          moneyEl(
            { minor: pair.delta, currency: pair.currency, scale: pair.amount.scale },
            { code: true, signed: true },
          ),
          h("span", { class: "faint nowrap" }, `per ${intervalWord(pair.interval)}`),
        ),
      ),
      h(
        "span",
        { class: "faint" },
        "whole period; the charge is this, prorated by the time left",
      ),
    );
  };

  const remove = async (edge: GraphEdge): Promise<void> => {
    const name = edge.product_name ?? edge.product_id;
    const target = edge.upgrade_to_name ?? edge.upgrade_to_id;
    const ok = await ctx.confirm({
      title: `Remove ${name} → ${target}?`,
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "p",
          null,
          "New upgrade options stop being offered immediately. Services that already moved keep the price they moved to: this removes a permission, it does not reprice anybody.",
        ),
        h(
          "p",
          { class: "field-hint" },
          "A pending upgrade whose invoice is unpaid is not affected either — that row is cancelled only by the stale-upgrade job.",
        ),
      ),
      confirmLabel: "Remove the edge",
      danger: true,
    });
    if (!ok) return;
    await withBusy(ctx, null, async () => {
      try {
        await ctx.api.del(
          `/api/v1/staff/upgrade-graph/${encodeURIComponent(edge.product_id)}/${encodeURIComponent(edge.upgrade_to_id)}`,
        );
        ctx.toast("Edge removed", { tone: "success", detail: "No service was repriced." });
        await load();
      } catch (err) {
        if (err instanceof ApiError) {
          ctx.toast(err.detail || "The edge could not be removed", { tone: "danger", detail: err.code });
          return;
        }
        ctx.toast("The edge could not be removed", {
          tone: "danger",
          detail: "The server could not be reached.",
        });
      }
    });
  };

  void load();
  return host;
}

// ------------------------------------------------------------------ helpers --

function inlineError(message: string): HTMLElement {
  return h("div", { class: "banner banner--danger" }, icon("alertCircle", 15), h("span", null, message));
}

function stat(label: string, value: number, hint: string): HTMLElement {
  return h(
    "section",
    { class: "card stat" },
    h("span", { class: "stat-label" }, label),
    h("span", { class: "stat-value" }, String(value)),
    h("span", { class: "stat-hint" }, hint),
  );
}

function intervalWord(interval: string): string {
  switch (interval) {
    case "monthly":
      return "month";
    case "quarterly":
      return "quarter";
    case "semiannual":
      return "six months";
    case "annual":
      return "year";
    default:
      return humanStatus(interval).toLowerCase();
  }
}
