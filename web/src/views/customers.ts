// The customer list.
//
// One row per customer, with the three numbers a support conversation starts from: how many
// invoices are open, what is owed, and whether the customer can sign in at all. "Can they see
// the portal?" is the question this screen exists to answer quickly, which is why `has_login`
// gets a badge of its own instead of being implied by an empty accounts list three clicks
// away.
//
// `outstanding` is a list of amounts, one per currency, and this page renders it as a list.
// A customer can hold a USD invoice and a EUR invoice at the same time; adding them would
// produce a number that no ledger contains and no exchange rate supports.
//
// There is no "create customer" button because there is no create endpoint. The empty state
// says so and points at the places that can explain a record: the audit log and the overview.
// Inventing a route here would be a dead link with a form behind it.

import { h } from "../lib/dom";
import type { Amount } from "../lib/format";
import { dateTime, relative } from "../lib/format";
import type { Ctx } from "../lib/types";
import { badge, btn, moneyEl, pageHead, searchInput, select } from "../ui/kit";
import { createTable, type Column } from "../ui/table";

export const meta = { title: "Customers", crumb: "Customers" };

interface CustomerRow {
  id: string;
  email: string;
  name: string;
  created_at: string;
  open_invoices: number;
  outstanding: Amount[] | null;
  services: number;
  has_login: boolean;
}

type SortKey = "created_at" | "email" | "name";

interface SortChoice {
  key: SortKey;
  label: string;
  /** created_at descends (recent signups first); the name sorts ascend, which is how a
   *  directory is read. The server defaults to descending, so asc has to be asked for. */
  direction: "asc" | "desc";
}

const SORTS: SortChoice[] = [
  { key: "created_at", label: "Newest first", direction: "desc" },
  { key: "email", label: "Email A–Z", direction: "asc" },
  { key: "name", label: "Name A–Z", direction: "asc" },
];

const DEFAULT_SORT = SORTS[0] as SortChoice;

/** The server refuses a one-character search with `invalid_query` rather than scanning the
 *  table, so a one-character term is not sent at all. */
const MIN_SEARCH = 2;

export default async function customersPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("customer:read:all")) {
    ctx.navigate("/", { replace: true });
    return h("div");
  }

  let term = (ctx.route.query.get("q") ?? "").trim();
  let sort = SORTS.find((choice) => choice.key === ctx.route.query.get("sort")) ?? DEFAULT_SORT;

  const queryOf = (search: string, order: SortChoice) => ({
    q: search.length >= MIN_SEARCH ? search : undefined,
    sort: order.key,
    direction: order.direction,
  });

  const hint = h(
    "span",
    { class: "faint cust-hint", hidden: term.length !== 1 },
    `Keep typing — the search needs ${MIN_SEARCH} characters.`,
  );

  const columns: Column<CustomerRow>[] = [
    {
      key: "customer",
      label: "Customer",
      primary: true,
      render: (row) =>
        h(
          "div",
          { class: "cell-main" },
          h(
            "div",
            { class: "primary" },
            h("a", { class: "row-title", href: `/customers/${row.id}` }, row.name || row.email),
          ),
          h("div", { class: "secondary" }, row.name ? row.email : "No name on file"),
        ),
    },
    {
      key: "open_invoices",
      label: "Open invoices",
      align: "right",
      hideSmall: true,
      render: (row) =>
        // A count column reads as numbers: "0" in a dim tone scans; the word "None" in a
        // column of integers is a different kind of value in the same place.
        h("span", { class: row.open_invoices > 0 ? "mono" : "mono faint" }, String(row.open_invoices)),
    },
    {
      key: "outstanding",
      label: "Outstanding",
      align: "right",
      render: (row) => outstandingCell(row.outstanding ?? []),
    },
    {
      key: "services",
      label: "Services",
      align: "right",
      hideSmall: true,
      render: (row) =>
        h("span", { class: row.services > 0 ? "mono" : "mono faint" }, String(row.services)),
    },
    {
      key: "has_login",
      label: "Login",
      render: (row) =>
        row.has_login
          ? badge("Portal access", "success", { dot: true })
          : badge("No login", "neutral", { dot: true }),
    },
    {
      key: "created",
      label: "Created",
      align: "right",
      hideSmall: true,
      render: (row) => h("span", { class: "faint", title: dateTime(row.created_at) }, relative(row.created_at)),
    },
  ];

  const table = createTable<CustomerRow>(ctx, {
    path: "/api/v1/staff/customers",
    columns,
    query: queryOf(term, sort),
    limit: 25,
    rowHref: (row) => `/customers/${row.id}`,
    empty: {
      title: term ? `No customer matches “${term}”` : "No customer records yet",
      body: term
        ? "Search matches the email and the name. Widen it, or clear it to see everyone."
        : "A customer record is created outside this panel — by the CLI or the provisioning " +
          "pipeline — so there is no create screen here and no route to send you to. Once the " +
          "record exists it appears in this list.",
      action: h(
        "div",
        { class: "row-wrap" },
        btn("Read the audit log", { href: "/audit", icon: "shield", size: "sm" }),
        btn("Back to overview", { href: "/", icon: "home", size: "sm" }),
      ),
    },
  });

  let timer: number | undefined;
  ctx.onDispose(() => {
    if (timer !== undefined) window.clearTimeout(timer);
  });

  const applySearch = (value: string) => {
    term = value.trim();
    hint.hidden = term.length !== 1;
    if (timer !== undefined) window.clearTimeout(timer);
    timer = window.setTimeout(() => {
      // The query string keeps the filter across a refresh and makes the URL shareable.
      ctx.replaceQuery({ q: term || null, sort: sort.key });
      void table.reload(queryOf(term, sort));
    }, 250);
  };

  const search = searchInput({
    value: term,
    placeholder: "Search by name or email",
    oninput: applySearch,
  });
  // The kit's search box is a fixed 200px; in this toolbar it should take the slack.
  search.classList.add("cust-search");

  const sortSelect = select(
    SORTS.map((choice) => ({ value: choice.key, label: choice.label })),
    {
      id: "customer-sort",
      value: sort.key,
      onchange: (event: Event) => {
        const key = (event.target as HTMLSelectElement).value as SortKey;
        sort = SORTS.find((choice) => choice.key === key) ?? DEFAULT_SORT;
        ctx.replaceQuery({ sort: sort.key, q: term || null });
        void table.reload(queryOf(term, sort));
      },
    },
  );

  const toolbar = h(
    "div",
    { class: "toolbar" },
    search,
    h("label", { class: "cust-sort", for: "customer-sort" }, h("span", { class: "faint" }, "Sort"), sortSelect),
    hint,
    h(
      "div",
      { class: "toolbar-end" },
      btn("Refresh", {
        icon: "refresh",
        size: "sm",
        onclick: () => void table.reload(queryOf(term, sort)),
      }),
    ),
  );

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Customers",
      sub: "Every customer record on the platform, with what each one owes per currency.",
      actions: btn("Audit log", { href: "/audit", icon: "shield", size: "sm" }),
    }),
    h("div", { class: "stack" }, toolbar, table.el),
  );
}

/** One line per currency. Never a total: the panel has no exchange rate and the ledger has
 *  no cross-currency sum. */
function outstandingCell(amounts: Amount[]): HTMLElement {
  if (amounts.length === 0) return h("span", { class: "faint" }, "Settled");
  return h("div", { class: "cust-amounts" }, ...amounts.map((amount) => moneyEl(amount)));
}
