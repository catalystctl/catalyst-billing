// Upgrades: the pending queue, and the graph of permitted moves.
//
// Two audiences, two permissions, one screen, and the split is the reason this file is not
// called "graph" any more:
//
//  - **The pending-upgrade queue** (`GET /api/v1/staff/upgrades`, `service:read:all`) is an
//    operations read. It answers "what have customers asked to change, and is the invoice that
//    pays for it settled yet" — the question an operator chasing a stuck upgrade opens. It is
//    deliberately not gated on catalog:write: holding `service:read:all` is what a support or
//    operations role has, and those are the people who receive the ticket.
//
//  - **The upgrade graph** (`/api/v1/staff/upgrade-graph`, `catalog:write`) is catalog
//    configuration. It edits the table of permitted moves.
//
// They live on one screen because they are one subject read two ways: an edge is permission to
// move, and a queue row is somebody using that permission. A role holding only one of the two
// permissions gets the half it can use and a sentence saying what the other half needs, rather
// than a refusal page that hides the half it could have read.
//
// The graph half of this file:
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
import { h, swap, type Child } from "../lib/dom";
import { formatCurrency, humanStatus, idempotencyKey, relative, shortId, type Amount } from "../lib/format";
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
  sectionTitle,
  select,
  skeletonLines,
  skeletonPage,
  statusBadge,
  withBusy,
} from "../ui/kit";
import { amountOf, loadCurrencies, type PublicCurrency } from "./cart";

export const meta = { title: "Upgrades", crumb: "Upgrades" };

// ---------------------------------------------------------------- payloads --

/** One row of the pending-upgrade queue, as `handleStaffUpgrades` renders it.
 *
 *  A row is a *request*: `service_id` is the service that would move, `to_price_version_id` the
 *  price it would move to, and `prorated_minor` what the customer would be charged (or, for a
 *  downgrade, credited) for the part of the period that is left. The service keeps its old price
 *  until the invoice is paid — `invoice_status` is the fact that says whether that has happened,
 *  which is why the queue shows it and the graph does not. */
interface PendingUpgrade {
  id: string;
  service_id: string;
  customer_id: string;
  from_price_version_id: string;
  to_price_version_id: string;
  prorated_minor: number;
  currency: string;
  status: string;
  created_at: string;
  invoice_id?: string;
  invoice_status?: string;
}

interface PendingUpgradesResponse {
  upgrades: PendingUpgrade[] | null;
}

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
  // Two independent reads, two permissions. A role that holds one gets the half it can use.
  const canQueue = ctx.can("service:read:all");
  const canGraph = ctx.can("catalog:write");
  // Posting a downgrade's credit moves money (a numbered note and a ledger entry), so the action
  // needs credit:write — separately from reading the queue, which is what support holds.
  const canPostCredit = ctx.can("credit:write");

  if (!canQueue && !canGraph) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Upgrades" }),
      banner(
        "danger",
        "You cannot see upgrades",
        "The pending queue needs service:read:all, and editing the graph needs catalog:write. This screen has two halves and your role holds neither.",
        btn("Back to the dashboard", { href: "/dashboard", size: "sm", variant: "ghost" }),
      ),
    );
  }

  // Each half is its own element with its own id, so a test can open the half it is about
  // without depending on the order of two cards inside one page.
  const queueHost = h("div", { class: "stack", id: "upgrade-queue" }, skeletonLines(4));
  const graphHost = h("div", { class: "stack", id: "upgrade-graph" }, skeletonPage());

  // ------------------------------------------------------------ the queue --

  // `paid=1` lists only the rows whose invoice has settled — the ones the worker will apply.
  // The default lists every pending row, which is what an operator chasing a stuck upgrade wants.
  let paidOnly = ctx.route.query.get("paid") === "1";
  // The currencies the last queue render used, so an action can format an amount it gets back
  // without re-reading the catalogue.
  let currenciesForQueue: PublicCurrency[] = [];

  /** Posts the credit a pending downgrade owes, and reports the note it produced.
   *
   *  The worker posts these on its own schedule; this button is for the case the queue shows:
   *  a downgrade that has been sitting here because nobody noticed. It is one request to the same
   *  domain method the job calls, so it cannot post a second note for a row that is already
   *  credited. */
  const postCredit = async (row: PendingUpgrade, button: HTMLButtonElement): Promise<void> => {
    const confirmed = await ctx.confirm({
      title: "Post the credit this downgrade owes?",
      body:
        "This issues a credit note for the prorated difference and applies the change: the service " +
        "moves to the cheaper price once the credit is posted. It cannot be undone from this screen.",
      confirmLabel: "Post the credit",
      cancelLabel: "Leave it to the worker",
    });
    if (!confirmed) return;
    await withBusy(ctx, button, async () => {
      try {
        const res = await ctx.api.post<{ number: string; amount_minor: number; applied: boolean; already_posted: boolean }>(
          `/api/v1/staff/upgrades/${encodeURIComponent(row.id)}/credit`,
          {},
          { idempotencyKey: idempotencyKey() },
        );
        const amount = amountOf(currenciesForQueue, res.amount_minor, row.currency);
        ctx.toast(res.already_posted ? "Already credited" : "Downgrade credited", {
          tone: "success",
          detail:
            `${res.number} · ${formatCurrency(amount, undefined, true)}` +
            (res.applied ? " · the service has moved to the cheaper plan" : " · the worker will apply it"),
        });
        await loadQueue();
      } catch (err) {
        if (err instanceof ApiError) {
          ctx.toast("The credit could not be posted", { tone: "danger", detail: err.detail || err.code });
          return;
        }
        ctx.toast("The credit could not be posted", {
          tone: "danger",
          detail: "The server could not be reached. Nothing has changed.",
        });
      }
    });
  };

  const loadQueue = async (): Promise<void> => {
    try {
      // The currency rows carry each code's scale, which the queue needs: it sends
      // `prorated_minor` + `currency` rather than the `{minor, currency, scale}` triple, and a
      // page that assumed two decimals would misprice a JPY or KWD installation (rule 11).
      const currencies = await loadCurrencies(ctx.api);
      const data = await ctx.api.get<PendingUpgradesResponse>("/api/v1/staff/upgrades", {
        query: { paid: paidOnly ? "true" : undefined, limit: "100" },
        cache: false,
      });
      currenciesForQueue = currencies;
      swap(queueHost, renderQueue(data, currencies));
    } catch (err) {
      swap(queueHost, errorState(err, () => void loadQueue()));
    }
  };

  const renderQueue = (data: PendingUpgradesResponse, currencies: PublicCurrency[]): Node => {
    const rows = data.upgrades ?? [];
    const amountOfRow = (row: PendingUpgrade): Amount => amountOf(currencies, row.prorated_minor, row.currency);

    // One total per currency, never a sum across them: this panel has no exchange rate, so
    // adding USD to EUR would be arithmetic on unrelated quantities.
    const byCurrency = new Map<string, number>();
    for (const row of rows) {
      byCurrency.set(row.currency, (byCurrency.get(row.currency) ?? 0) + row.prorated_minor);
    }
    const awaitingPayment = rows.filter((row) => (row.invoice_status ?? "") !== "paid").length;
    const creditButton = (row: PendingUpgrade): HTMLElement => {
      const button = btn("Post credit", {
        size: "sm",
        variant: "ghost",
        icon: "wallet",
        title: "Issue the credit note this downgrade owes and apply the change",
      }) as HTMLButtonElement;
      button.addEventListener("click", () => void postCredit(row, button));
      return button;
    };

    return h(
      "div",
      { class: "stack" },
      card({
        title: "Pending upgrades",
        sub: paidOnly
          ? "Requests whose invoice is paid, so the service is due to move"
          : "Every request that has not been applied yet, and whether its invoice is settled",
        actions: h(
          "div",
          { class: "row-wrap" },
          btn("All pending", {
            size: "sm",
            variant: paidOnly ? "ghost" : "primary",
            onclick: () => {
              paidOnly = false;
              ctx.replaceQuery({ paid: null });
              void loadQueue();
            },
          }),
          btn("Invoice paid", {
            size: "sm",
            variant: paidOnly ? "primary" : "ghost",
            icon: "check",
            title: "Only the rows whose invoice has settled — the ones the worker will apply",
            onclick: () => {
              paidOnly = true;
              ctx.replaceQuery({ paid: "1" });
              void loadQueue();
            },
          }),
          btn("Refresh", { size: "sm", variant: "ghost", icon: "refresh", onclick: () => void loadQueue() }),
        ),
        body:
          rows.length === 0
            ? emptyState({
                title: paidOnly ? "Nothing is waiting to be applied" : "No pending upgrades",
                body: paidOnly
                  ? "No request has a settled invoice right now. A row appears here as soon as the invoice that pays for a move is paid, and the worker applies it on its next run."
                  : "Nobody has asked to change a plan. A request appears here as soon as a customer moves from one product to another, or an operator does it on their behalf.",
                icon: "trendUp",
              })
            : h(
                "div",
                { class: "stack" },
                h(
                  "div",
                  { class: "grid-4" },
                  stat("Waiting", rows.length, "requests not yet applied"),
                  stat("Awaiting payment", awaitingPayment, "invoice not settled"),
                  ...Array.from(byCurrency.entries()).map(([currency, minor]) =>
                    stat(
                      `Per ${currency}`,
                      moneyEl(amountOf(currencies, minor, currency), { code: true }),
                      "prorated, whole queue",
                    ),
                  ),
                ),
                h(
                  "div",
                  { class: "table-wrap table-wrap--cards" },
                  h(
                    "table",
                    { class: "table" },
                    h(
                      "thead",
                      null,
                      h(
                        "tr",
                        null,
                        h("th", { scope: "col" }, "Service"),
                        h("th", { scope: "col" }, "Customer"),
                        h("th", { scope: "col" }, "Prorated charge"),
                        h("th", { scope: "col" }, "Status"),
                        h("th", { scope: "col" }, "Invoice"),
                        h("th", { scope: "col", class: "hide-sm" }, "Requested"),
                        canPostCredit ? h("th", { scope: "col", class: "num" }, "") : null,
                      ),
                    ),
                    h(
                      "tbody",
                      null,
                      ...rows.map((row) =>
                        h(
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
                                h(
                                  "a",
                                  { class: "row-title", href: `/services/${encodeURIComponent(row.service_id)}` },
                                  shortId(row.service_id),
                                ),
                              ),
                              h("div", { class: "secondary mono" }, shortId(row.to_price_version_id)),
                            ),
                          ),
                          h(
                            "td",
                            { "data-label": "Customer" },
                            h(
                              "a",
                              { class: "linkish", href: `/customers/${encodeURIComponent(row.customer_id)}` },
                              shortId(row.customer_id),
                            ),
                          ),
                          h("td", { "data-label": "Prorated charge" }, moneyEl(amountOfRow(row), { code: true })),
                          h("td", { "data-label": "Status" }, statusBadge(row.status)),
                          h(
                            "td",
                            { "data-label": "Invoice" },
                            row.invoice_id
                              ? h(
                                  "div",
                                  { class: "cell-main" },
                                  h(
                                    "div",
                                    { class: "primary" },
                                    h(
                                      "a",
                                      { class: "row-title", href: `/invoices/${encodeURIComponent(row.invoice_id)}` },
                                      shortId(row.invoice_id),
                                    ),
                                  ),
                                  h("div", { class: "secondary" }, statusBadge(row.invoice_status ?? "draft")),
                                )
                              : h("span", { class: "faint" }, "—"),
                          ),
                          h(
                            "td",
                            { class: "hide-sm", "data-label": "Requested" },
                            h("span", { class: "dim nowrap", title: row.created_at }, relative(row.created_at)),
                          ),
                          canPostCredit
                            ? h(
                                "td",
                                { class: "num", "data-label": "Actions" },
                                // Only a downgrade with no invoice owes a credit. An upgrade waits
                                // for its invoice instead, and posting a "credit" for it would be a
                                // refund for a charge that has not happened.
                                row.prorated_minor < 0 && !row.invoice_id
                                  ? creditButton(row)
                                  : h("span", { class: "faint" }, "—"),
                              )
                            : null,
                        ),
                      ),
                    ),
                  ),
                ),
              ),
      }),
      banner(
        "info",
        "A request is not a move",
        h(
          "div",
          { class: "banner-prose" },
          h(
            "p",
            null,
            h("strong", null, "An upgrade issues an invoice"),
            " for the difference, and the service keeps its old price until that invoice is paid. A paid invoice is what moves a row into the worker's hands.",
          ),
          h(
            "p",
            null,
            h("strong", null, "A downgrade records a credit"),
            " the customer is owed; the service moves down only once that credit is posted. A row that sits here for weeks is usually a downgrade whose credit was never posted.",
          ),
        ),
      ),
    );
  };

  // ------------------------------------------------------------ the graph --

  const loadGraph = async (): Promise<void> => {
    try {
      const data = await ctx.api.get<GraphResponse>("/api/v1/staff/upgrade-graph", { cache: false });
      swap(graphHost, render(data));
    } catch (err) {
      swap(graphHost, errorState(err, () => void loadGraph()));
    }
  };

  const render = (data: GraphResponse): Node => {
    const products = data.products ?? [];
    const edges = data.edges ?? [];
    const invisible = edges.filter((e) => !e.compatible).length;

    return h(
      "div",
      { class: "stack" },

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
          await loadGraph();
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
        await loadGraph();
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

  // The queue is the half an operations role holds, so it is read first and on every visit.
  // The graph is only read when this session may edit it: fetching a table the screen will not
  // render would be a request whose answer is a 403 the operator never sees.
  if (canQueue) void loadQueue();
  else swap(queueHost, emptyState({
    title: "You cannot see the pending queue",
    body: "The queue is an operations read: it needs service:read:all. You can still see and edit the graph below.",
    icon: "lock",
  }));

  if (canGraph) void loadGraph();
  else swap(graphHost, card({
    title: "Upgrade graph",
    body: emptyState({
      title: "You cannot change the graph",
      body: "The graph is catalog configuration: adding or removing an edge needs catalog:write. You can still see the queue above.",
      icon: "lock",
    }),
  }));

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Upgrades",
      sub: "What customers have asked to change, and which product a customer may move to from which.",
      actions: canGraph
        ? btn("Catalog", { href: "/catalog", size: "sm", variant: "ghost", icon: "box" })
        : null,
    }),
    h(
      "div",
      { class: "stack" },
      // Two headings, because the screen answers two questions and an operator who arrived from
      // a ticket should see which half is theirs without reading the page subtitle.
      sectionTitle("Pending queue"),
      h(
        "p",
        { class: "field-hint" },
        "What customers have asked to change, and whether the invoice that pays for it has settled.",
      ),
      queueHost,
      canGraph
        ? h(
            "div",
            { class: "stack" },
            sectionTitle("Upgrade graph"),
            h(
              "p",
              { class: "field-hint" },
              "Which product a customer may move to from which. A downgrade is the same edge read backwards, so there is one table and one screen.",
            ),
            graphHost,
          )
        : graphHost,
    ),
  );
}

// ------------------------------------------------------------------ helpers --

function inlineError(message: string): HTMLElement {
  return h("div", { class: "banner banner--danger" }, icon("alertCircle", 15), h("span", null, message));
}

/** One figure. The value may be an amount (`moneyEl`) or a count, so a queue total and a row
 *  count can sit in the same grid without two components. */
function stat(label: string, value: Child, hint: string): HTMLElement {
  return h(
    "section",
    { class: "card stat" },
    h("span", { class: "stat-label" }, label),
    h("span", { class: "stat-value" }, value),
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
