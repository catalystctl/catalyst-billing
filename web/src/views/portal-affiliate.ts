// Referrals, for the customer who is an affiliate.
//
// One question, answered in the first line: what is my code, and what has it earned. Everything
// below is the evidence for that number — every signup the code attributed, the commission each
// one produced, and whether it has been paid.
//
// Three properties of the server shape this screen, and each one is a sentence the page has to
// say rather than leave to be discovered:
//
//   - **The affiliate is resolved from the session, never from the URL.** `GET
//     /api/v1/portal/affiliate` reads the affiliate out of the caller's user id, so there is no
//     identifier here to substitute and no way to read somebody else's earnings. That is why the
//     page has no id in its route: an "affiliate id" in a URL would be an invitation to try one.
//
//   - **"Not an affiliate" is a 404, not a 403.** The server answers `not_an_affiliate` for a
//     customer who is not enrolled, and the page renders that as a state with a next step
//     (an operator enrols an affiliate and sets the rate), not as an error with a retry button
//     that cannot change the answer. A 403 here would confirm that the record exists, which is
//     information about somebody else.
//
//   - **A balance is owed, not paid.** `owed` is commission that has been earned and not yet
//     settled; `paid` is what a recorded payout already settled. The page shows both per
//     currency and never sums across currencies: this system has no exchange rate, so a total in
//     two currencies would be arithmetic on unrelated quantities.
//
// The referral rows are the ledger behind those figures. A row with a commission came from a
// paid order; a row without one is an attribution whose order has not settled yet, and it is
// shown as such instead of being hidden — "I referred three people and only see two" is exactly
// the support ticket this list exists to prevent.

import { ApiError } from "../lib/api";
import { h } from "../lib/dom";
import { dateOnly, relative, shortId, type Amount } from "../lib/format";
import type { Ctx } from "../lib/types";
import {
  badge,
  banner,
  btn,
  card,
  copyButton,
  emptyState,
  mono,
  moneyEl,
  pageHead,
  resource,
  skeletonPage,
  statusBadge,
} from "../ui/kit";
import { portalLoadError } from "./portal";

export const meta = { title: "Referrals", crumb: "Referrals" };

// ---------------------------------------------------------------- payloads --

/** One affiliate's balance in one currency, as `affiliateJSON` renders it. */
interface AffiliateBalance {
  currency: string;
  owed: Amount;
  paid: Amount;
}

/** One attribution or commission row, as `referralJSON` emits it.
 *
 *  `commission` and `rate` are present only when the referral came from an order: an attribution
 *  with no settled order has a status and a date and nothing to pay yet. Every field is
 *  normalised at the edge (rule 11) so the renderer never has to guess. */
interface Referral {
  id: string;
  affiliate_id: string;
  customer_id: string;
  status: string;
  attributed_at: string;
  order_id?: string;
  commission?: Amount;
  rate?: string;
  paid_at?: string;
}

/** `GET /api/v1/portal/affiliate` — `handlePortalAffiliate` in internal/httpapi/affiliates.go.
 *
 *  `balances` and `referrals` are built with `make` server-side, so they are always arrays; they
 *  are normalised here anyway because a payload that changes to `null` must not crash a screen
 *  that a customer opens once a month. */
interface PortalAffiliate {
  code: string;
  commission_rate: string;
  payout_method: string;
  enabled: boolean;
  /** 'active' | 'suspended' | 'deleted'. Derived server-side from the state that gates
   *  earning; branch on this, never on `enabled`. */
  status: string;
  referred_count: number;
  balances: AffiliateBalance[] | null;
  referrals: Referral[] | null;
}

/** What the page learned: the books, or the fact that this account is not enrolled. */
type AffiliateView = PortalAffiliate | "not_enrolled";

// -------------------------------------------------------------------- page --

export default async function portalAffiliatePage(ctx: Ctx): Promise<Node> {
  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Referrals",
      sub: "Your referral code, who it brought in, and what that has earned.",
      actions: btn("Overview", { href: "/portal", size: "sm", variant: "ghost", icon: "home" }),
    }),
    resource(
      ctx,
      (signal) => loadAffiliate(ctx, signal),
      (view) => render(ctx, view),
      { skeleton: skeletonPage },
    ),
  );
}

/** Reads the caller's own affiliate record.
 *
 *  A 404 with `not_an_affiliate` is the documented answer for a customer who is not enrolled, so
 *  it becomes a state rather than an exception: retrying produces the same answer, and offering
 *  a retry button would be a lie about what it can change. Any other 404/403 means "this login
 *  has no billing customer", which `portalLoadError` turns into the sentence the rest of the
 *  portal uses. */
async function loadAffiliate(ctx: Ctx, signal: AbortSignal): Promise<AffiliateView> {
  try {
    return await ctx.api.get<PortalAffiliate>("/api/v1/portal/affiliate", { cache: false, signal });
  } catch (err) {
    if (err instanceof ApiError && err.status === 404 && err.code === "not_an_affiliate") return "not_enrolled";
    throw portalLoadError(err);
  }
}

function render(ctx: Ctx, view: AffiliateView): Node {
  if (view === "not_enrolled") return notEnrolled();
  return h("div", { class: "stack" }, ...affiliateCards(ctx, view));
}

/** What a customer who is not an affiliate sees.
 *
 *  It says who enrols them and what will appear once somebody does, because "you are not an
 *  affiliate" on its own is a dead end: the customer cannot fix it from this page and has no way
 *  to know whether the programme exists at all. */
function notEnrolled(): Node {
  return card({
    title: "You are not in the referral programme",
    body: emptyState({
      title: "No referral code on this account",
      body: "An operator enrols an affiliate and sets their commission rate. Ask support to enrol you: once they do, your code, the people it brought in and the commission they earned all appear here.",
      icon: "percent",
      action: h(
        "div",
        { class: "row-wrap" },
        btn("Contact support", { href: "/tickets/new", size: "sm", variant: "primary", icon: "message" }),
        btn("Back to overview", { href: "/portal", size: "sm", variant: "ghost", icon: "home" }),
      ),
    }),
  });
}

// ------------------------------------------------------------------ render --

function affiliateCards(ctx: Ctx, view: PortalAffiliate): Node[] {
  const balances = view.balances ?? [];
  const referrals = view.referrals ?? [];
  const nodes: Node[] = [];

  // A suspended or deleted affiliate keeps their books. The state is stated above them rather
  // than replacing them: what is owed is still owed, and hiding it would look like a mistake.
  if (view.status === "suspended") {
    nodes.push(
      banner(
        "warning",
        "Your referral code is suspended",
        h(
          "span",
          null,
          "It no longer attributes new signups and no further commission accrues. What you have already earned is still yours and is still payable.",
        ),
      ),
    );
  } else if (view.status === "deleted") {
    nodes.push(
      banner(
        "info",
        "You have left the referral programme",
        h(
          "span",
          null,
          "The record is closed, so nothing new is attributed to your code. Your commission history and any unpaid balance are kept.",
        ),
      ),
    );
  }

  nodes.push(codeCard(ctx, view));

  const owed = balances.filter((balance) => (balance.owed?.minor ?? 0) > 0);
  const paid = balances.filter((balance) => (balance.paid?.minor ?? 0) > 0);

  nodes.push(
    card({
      title: "Commission",
      sub: "What has been earned, and what has been paid out",
      actions:
        owed.length === 0
          ? badge("Nothing outstanding", "neutral", { dot: true })
          : badge("Unpaid balance", "warning", { dot: true }),
      body:
        balances.length === 0
          ? emptyState({
              title: "Nothing earned yet",
              body: "Commission appears here once someone signs up with your code and their order is paid. Referrals are listed below as they arrive.",
              icon: "wallet",
            })
          : h(
              "div",
              { class: "stack" },
              h(
                "div",
                { class: "grid-3" },
                // One figure per currency, never a total: two currencies have no common unit
                // here, and a number that added them would be a fiction.
                ...owed.map((balance) => moneyFigure(`Owed · ${balance.currency}`, balance.owed, "Not yet paid out")),
                ...paid.map((balance) => moneyFigure(`Paid · ${balance.currency}`, balance.paid, "Settled by a payout")),
              ),
              h(
                "p",
                { class: "field-hint" },
                "A payout is recorded by the operator who runs the programme. It settles the whole outstanding balance for one currency, so a part-paid commission is not a thing this ledger has.",
              ),
            ),
    }),
  );

  nodes.push(referralsCard(referrals, view.referred_count));
  return nodes;
}

/** The code itself, with a copy button and the rate it pays.
 *
 *  The code is the one thing on this page the customer came for, so it is set in the monospace
 *  face and copyable rather than buried in a table of facts. */
function codeCard(ctx: Ctx, view: PortalAffiliate): Node {
  return card({
    title: "Your referral code",
    sub: "Give this to someone when they sign up",
    actions: statusBadge(view.status),
    body: h(
      "div",
      { class: "stack" },
      h(
        "div",
        { class: "row-wrap" },
        h("span", { class: "detail-title mono" }, view.code),
        copyButton(ctx, view.code, "Copy the code"),
      ),
      h(
        "div",
        { class: "grid-3" },
        textFigure("Commission rate", view.commission_rate, "of every paid order"),
        textFigure(
          "Referred",
          String(view.referred_count),
          `signup${view.referred_count === 1 ? "" : "s"} attributed`,
        ),
        textFigure("Payout method", payoutMethodLabel(view.payout_method), "set by the operator"),
      ),
      h(
        "p",
        { class: "field-hint" },
        "The code is what a new customer presents when they sign up. Commission is credited when their order is paid, at the rate shown — a later rate change applies to future commission only.",
      ),
    ),
  });
}

/** The attributions and commissions behind the balance. */
function referralsCard(referrals: Referral[], referredCount: number): Node {
  if (referrals.length === 0) {
    return card({
      title: "Your referrals",
      body: emptyState({
        title: "Nobody yet",
        body: "When someone signs up with your code they appear here, with the commission their first paid order earned.",
        icon: "users",
      }),
    });
  }

  const rows = referrals.map((referral) =>
    h(
      "tr",
      null,
      h(
        "td",
        { class: "cell-primary" },
        h(
          "div",
          { class: "cell-main" },
          h("div", { class: "primary mono" }, shortId(referral.customer_id)),
          h(
            "div",
            { class: "secondary" },
            referral.attributed_at ? `signed up ${relative(referral.attributed_at)}` : "signed up",
          ),
        ),
      ),
      h("td", { "data-label": "Status" }, statusBadge(referral.status)),
      h(
        "td",
        { "data-label": "Commission" },
        referral.commission
          ? h(
              "div",
              { class: "cell-main" },
              h("div", { class: "primary" }, moneyEl(referral.commission, { code: true })),
              referral.rate ? h("div", { class: "secondary" }, `at ${referral.rate}`) : null,
            )
          : h("span", { class: "faint" }, "awaiting a paid order"),
      ),
      h(
        "td",
        { "data-label": "Paid" },
        referral.paid_at
          ? h("span", { class: "dim nowrap", title: dateOnly(referral.paid_at) }, `paid ${relative(referral.paid_at)}`)
          : h("span", { class: "faint" }, "—"),
      ),
    ),
  );

  return card({
    title: "Your referrals",
    sub:
      referredCount > referrals.length
        ? `${referrals.length} most recent of ${referredCount}`
        : `${referrals.length} attribution${referrals.length === 1 ? "" : "s"}`,
    flush: true,
    body: h(
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
            h("th", { scope: "col" }, "Referred account"),
            h("th", { scope: "col" }, "Status"),
            h("th", { scope: "col" }, "Commission"),
            h("th", { scope: "col" }, "Paid"),
          ),
        ),
        h("tbody", null, ...rows),
      ),
    ),
  });
}

// ----------------------------------------------------------------- helpers --

/** A figure whose value is an amount. */
function moneyFigure(label: string, amount: Amount, hint: string): HTMLElement {
  return h(
    "div",
    { class: "stat" },
    h("span", { class: "stat-label" }, label),
    h("span", { class: "stat-value money" }, moneyEl(amount, { code: true })),
    h("span", { class: "stat-hint" }, hint),
  );
}

/** A figure whose value is a word or a count. */
function textFigure(label: string, value: string, hint: string): HTMLElement {
  return h(
    "div",
    { class: "stat" },
    h("span", { class: "stat-label" }, label),
    h("span", { class: "stat-value" }, value),
    h("span", { class: "stat-hint" }, hint),
  );
}

/** The payout methods the schema allows, in words. */
function payoutMethodLabel(method: string): string {
  switch (method) {
    case "credit":
      return "account credit";
    case "paypal":
      return "PayPal";
    case "bank_transfer":
      return "bank transfer";
    case "manual":
      return "paid out of band";
    default:
      return method || "not set";
  }
}
