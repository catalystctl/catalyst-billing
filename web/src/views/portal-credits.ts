// Credit, for the customer.
//
// Two questions, answered in that order: how much do I have, and how did it get there.
//
// The balance is **derived from the subledger**, never a stored column: `GET
// /api/v1/portal/credits` sums `journal_entries` on the `customer_credit` account, per currency.
// That is why the page can promise the number agrees with the books — and why it renders one
// line per currency rather than a total, because adding USD and EUR would be arithmetic on
// unrelated quantities.
//
// The deposit form is bounded by the *server's* settings, which the same response carries
// (`minimum_deposit_minor`, `maximum_deposit_minor`, `maximum_credit_minor`, `enabled`). The
// form shows those limits as hints and validates against them before sending, because a
// customer who is told "the amount must be at least 5.00" by a toast after a round trip has been
// made to do work the page could have done.
//
// A deposit is a *payment*, not a balance edit: the endpoint creates a deposit invoice, opens a
// gateway session and returns a redirect URL. The credit is granted when the settled webhook
// arrives, not when the call returns, and the page says so — a customer who expects the balance
// to move immediately and finds it unchanged will assume the top-up failed.
//
// The transaction history is not in this payload. `GET /api/v1/portal/credits` returns balances
// and settings only, so the history section reads the customer's invoices, filtered to the
// deposit documents, from `GET /api/v1/portal/invoices`. That is honest: a credit grant that did
// not come from a deposit (a goodwill note) has no row here, and the section says where to find
// it instead of inventing a list.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateOnly, formatCurrency, plural, type Amount } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  amountFromInput,
  badge,
  banner,
  btn,
  card,
  emptyState,
  field,
  input,
  moneyEl,
  pageHead,
  resource,
  skeletonPage,
  withBusy,
} from "../ui/kit";
import { formatsOf, loadCurrencies, type PublicCurrency } from "./cart";
import { portalLoadError } from "./portal";

export const meta = { title: "Credit", crumb: "Your credit" };

// ---------------------------------------------------------------- payloads --

/** `GET /api/v1/portal/credits` — `handlePortalCredits` in internal/httpapi/credits.go.
 *
 *  `balances` is one entry per currency with a positive balance; an empty array means "nothing
 *  on account", not "we could not read it". `settings` is the server's own deposit policy, which
 *  is what the form validates against. */
interface CreditResponse {
  customer_id: string;
  balances: Amount[] | null;
  settings: {
    enabled: boolean;
    minimum_deposit_minor: number;
    maximum_deposit_minor: number;
    maximum_credit_minor: number;
    auto_use: boolean;
  };
}

/** The 201 from `POST /api/v1/portal/credits/deposit`: a deposit invoice and the gateway
 *  redirect. `redirect_url` may be empty when the gateway could not be opened. */
interface DepositStarted {
  order_id: string;
  invoice_id: string;
  number: string;
  attempt_id: string;
  redirect_url: string;
  amount: Amount;
  status: string;
}

/** One credit movement. The endpoint is not part of the payload this page is specified
 *  against, so every field is optional and the renderer normalises: the ledger's own
 *  `journal_entries` are the only source, and a build that does not expose them yields `null`
 *  rather than a guess. */
interface CreditTransaction {
  id?: string;
  kind?: string;
  amount?: Amount | null;
  balance_after?: Amount | null;
  reference_type?: string;
  reference_id?: string;
  description?: string;
  created_at?: string;
}

interface TransactionsResponse {
  transactions?: CreditTransaction[] | null;
  data?: CreditTransaction[] | null;
}

interface CreditView {
  credit: CreditResponse;
  currencies: PublicCurrency[];
  /** The credit ledger, or `null` when this installation does not expose it. `null` is a state,
   *  not an error: the balance and the form still work. */
  transactions: CreditTransaction[] | null;
  transactionsNote: string;
}

export default async function portalCreditsPage(ctx: Ctx): Promise<Node> {
  const host = h("div");

  /** Loads the balance, or reports that this build does not serve it.
   *
   *  `GET /api/v1/portal/credits` is the credits package's route. Until it is merged the
   *  request is a 404, and a 404 is a fact about the installation rather than a failed read:
   *  retrying produces the same answer, so the screen explains instead of offering a retry
   *  button that cannot work. That distinction is the whole reason this returns a sentinel
   *  rather than throwing. */
  const load = async (signal: AbortSignal): Promise<CreditView | "unavailable"> => {
    const currencies = await loadCurrencies(ctx.api, signal);
    let credit: CreditResponse;
    try {
      credit = await ctx.api.get<CreditResponse>("/api/v1/portal/credits", { cache: false, signal });
    } catch (err) {
      if (err instanceof ApiError && (err.status === 404 || err.status === 501)) return "unavailable";
      throw portalLoadError(err);
    }
    // The transaction history is read from its own endpoint and normalised at the edge: the
    // wire has `null` where an array is expected, and a build that does not serve the route
    // answers 404. Both mean "no history to show", which is a state this page renders rather
    // than an error it throws.
    let transactions: CreditTransaction[] | null = null;
    let transactionsNote = "";
    try {
      const res = await ctx.api.get<TransactionsResponse>("/api/v1/portal/credits/transactions", {
        cache: false,
        signal,
      });
      transactions = res.transactions ?? res.data ?? [];
    } catch (err) {
      if (err instanceof DOMException && err.name === "AbortError") throw err;
      if (err instanceof ApiError && (err.status === 404 || err.status === 501)) {
        transactions = null;
        transactionsNote =
          "This installation does not expose the credit ledger as a list. The balance above is read from it, and each movement is visible on the invoice it came from.";
      } else {
        transactions = null;
        transactionsNote =
          err instanceof ApiError
            ? err.detail || "The credit history could not be read."
            : "The credit history could not be read. The server could not be reached.";
      }
    }
    return { credit, currencies, transactions, transactionsNote };
  };

  const paint = (): void => {
    swap(
      host,
      resource(ctx, load, (view) => (view === "unavailable" ? unavailableCard() : render(view)), {
        skeleton: skeletonPage,
      }),
    );
  };

  /** What a missing credit endpoint looks like. It says what is missing, what still works, and
   *  where a balance can still be seen — the portal overview reports credit alongside what is
   *  outstanding, so a customer is not left with no way to read it. */
  const unavailableCard = (): HTMLElement =>
    card({
      title: "Credit is not available in this build",
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "p",
          { class: "field-hint" },
          "This installation does not serve ",
          h("code", { class: "inline-code" }, "GET /api/v1/portal/credits"),
          ", so there is no balance to show and no top-up form. Reloading will give the same answer until a build that includes the credit surface is deployed.",
        ),
        h(
          "p",
          { class: "field-hint" },
          "Any credit on your account is still there and is still applied to invoices. The overview reports it beside what is outstanding, and each use of it appears on the invoice that spent it.",
        ),
        h(
          "div",
          { class: "row" },
          btn("Back to overview", { href: "/portal", size: "sm", icon: "home" }),
          btn("Your invoices", { href: "/portal/invoices", size: "sm", variant: "ghost", icon: "invoice" }),
        ),
      ),
    });

  // ---------------------------------------------------------------- render --

  const render = (view: CreditView): Node => {
    const balances = view.credit.balances ?? [];
    const settings = view.credit.settings;
    const formats = formatsOf(view.currencies);

    const fmt = (amount: Amount | null | undefined): string =>
      formatCurrency(amount, amount ? formats.get(amount.currency.toUpperCase()) : undefined, true);

    return h(
      "div",
      { class: "stack" },
      // The balance hero. One line per currency, and a sentence that says the number is the
      // ledger's rather than a copy of it.
      h(
        "section",
        { class: "credit-hero" },
        h(
          "div",
          { class: "credit-hero-main" },
          h("span", { class: "credit-hero-label" }, balances.length > 1 ? "Credit, per currency" : "Credit balance"),
          balances.length === 0
            ? h("p", { class: "credit-hero-none" }, "Nothing on account. Credit is only used when it is there.")
            : h(
                "div",
                { class: "credit-hero-amounts" },
                ...balances.map((amount) =>
                  h(
                    "span",
                    { class: "credit-hero-amount" },
                    h("span", { class: "credit-hero-code mono" }, amount.currency),
                    moneyEl(amount, { size: "xl", code: false }),
                  ),
                ),
              ),
          h(
            "p",
            { class: "credit-hero-note" },
            settings.auto_use
              ? "Credit is applied to a new invoice automatically when it covers the total."
              : "Credit is not applied automatically; it is used when an invoice is paid with it.",
          ),
        ),
        balances.length > 0
          ? h(
              "div",
              { class: "credit-hero-side" },
              badge(`${balances.length} currenc${balances.length === 1 ? "y" : "ies"}`, "info"),
              h("span", { class: "faint" }, "Derived from the ledger, not a stored number."),
            )
          : null,
      ),

      settings.enabled
        ? depositCard(view, fmt)
        : card({
            title: "Top-ups are closed",
            body: h(
              "div",
              { class: "stack-sm" },
              h(
                "p",
                { class: "field-hint" },
                "This installation does not accept credit deposits. Credit already on account can still be spent, and support can add a goodwill credit if one is owed.",
              ),
              btn("Contact support", { href: "/tickets", size: "sm", variant: "ghost", icon: "message" }),
            ),
          }),

      historyCard(view.transactions, view.transactionsNote, fmt),
    );
  };

  // --------------------------------------------------------------- deposit --

  const depositCard = (view: CreditView, fmt: (amount: Amount | null | undefined) => string): HTMLElement => {
    const settings = view.credit.settings;
    const balances = view.credit.balances ?? [];

    // The currency the form starts in: the one already on account, else the installation's
    // default, else the first configured one. A deposit needs a currency and guessing wrong is
    // a refused request.
    const preferred =
      balances[0]?.currency ??
      view.currencies.find((row) => row.is_default)?.code ??
      view.currencies[0]?.code ??
      "USD";

    const codes = [...new Set([...balances.map((b) => b.currency), ...view.currencies.map((c) => c.code), preferred])];
    const currency = h(
      "select",
      { class: "select", id: "credit-currency" },
      ...codes.map((code) => {
        const option = h("option", { value: code }, code);
        if (code === preferred) option.selected = true;
        return option;
      }),
    );
    const amount = input({
      id: "credit-amount",
      placeholder: "20.00",
      inputmode: "decimal",
      class: "input--mono",
      required: true,
    });
    const errorHost = h("div");
    const submit = btn("Top up", { variant: "primary", icon: "card" }) as HTMLButtonElement;

    const scaleOf = (code: string): number =>
      view.currencies.find((row) => row.code.toUpperCase() === code.toUpperCase())?.scale ?? 2;

    const minorAmount = (): { minor: number; currency: string; scale: number } | null => {
      const code = currency.value;
      const scale = scaleOf(code);
      const minor = amountFromInput(amount.value, scale);
      if (minor === null || minor <= 0) return null;
      return { minor, currency: code, scale };
    };

    const limitsHint = (): HTMLElement => {
      const code = currency.value;
      const scale = scaleOf(code);
      const min = { minor: settings.minimum_deposit_minor, currency: code, scale };
      const max = { minor: settings.maximum_deposit_minor, currency: code, scale };
      return h(
        "span",
        { class: "field-hint" },
        `Between ${fmt(min)} and ${fmt(max)} per top-up.`,
      );
    };

    let hint = limitsHint();
    currency.addEventListener("change", () => {
      swap(hint, limitsHint());
    });

    submit.addEventListener("click", () => void startDeposit());

    async function startDeposit(): Promise<void> {
      errorHost.replaceChildren();
      const parsed = minorAmount();
      if (!parsed) {
        swap(
          errorHost,
          h("div", { class: "banner banner--danger" }, "Enter an amount, e.g. 20.00."),
        );
        return;
      }
      // The server validates these too; checking here is about telling the customer before the
      // round trip, not about trusting the client.
      if (parsed.minor < settings.minimum_deposit_minor) {
        swap(
          errorHost,
          h(
            "div",
            { class: "banner banner--danger" },
            `The minimum top-up is ${fmt({ minor: settings.minimum_deposit_minor, currency: parsed.currency, scale: parsed.scale })}.`,
          ),
        );
        return;
      }
      if (parsed.minor > settings.maximum_deposit_minor) {
        swap(
          errorHost,
          h(
            "div",
            { class: "banner banner--danger" },
            `The maximum single top-up is ${fmt({ minor: settings.maximum_deposit_minor, currency: parsed.currency, scale: parsed.scale })}.`,
          ),
        );
        return;
      }

      await withBusy(ctx, submit, async () => {
        try {
          const started = await ctx.api.post<DepositStarted>("/api/v1/portal/credits/deposit", {
            amount_minor: parsed.minor,
            currency: parsed.currency,
          });
          // The redirect is the payment page. It is followed only after a safety check: a URL
          // that arrives from another system is data, not a command.
          const target = safeURL(started.redirect_url);
          if (!target) {
            ctx.toast("The payment page could not be opened", {
              tone: "danger",
              duration: 0,
              detail:
                "The gateway returned no usable link, so there is nowhere to send you. Nothing has been charged — try again, and contact support if it keeps happening.",
            });
            return;
          }
          ctx.toast("Opening the payment page", {
            tone: "info",
            detail: `${fmt(started.amount)} · deposit ${started.number || ""}`.trim(),
          });
          window.location.assign(target);
        } catch (err) {
          if (!(err instanceof ApiError)) {
            ctx.toast("The top-up could not be started", {
              tone: "danger",
              detail: "The server could not be reached. Nothing has been charged.",
            });
            return;
          }
          swap(errorHost, h("div", { class: "banner banner--danger" }, depositError(err, fmt)));
        }
      });
    }

    return card({
      title: "Top up",
      sub: "A deposit is a payment: it opens the gateway and the credit appears once the payment settles.",
      body: h(
        "div",
        { class: "stack" },
        errorHost,
        h(
          "div",
          { class: "credit-deposit-form" },
          field({ label: "Amount", control: amount, id: "credit-amount", required: true }),
          field({ label: "Currency", control: currency, id: "credit-currency" }),
          h("div", { class: "credit-deposit-action" }, submit),
        ),
        hint,
        h(
          "p",
          { class: "field-hint" },
          settings.auto_use
            ? "Credit is spent automatically on the next invoice when it covers the total. It is never taken for anything else."
            : "Credit stays on account until an invoice is paid with it.",
        ),
      ),
    });
  };

  // --------------------------------------------------------------- history --

  const historyCard = (
    transactions: CreditTransaction[] | null,
    note: string,
    fmt: (amount: Amount | null | undefined) => string,
  ): HTMLElement => {
    const rows = transactions ?? [];
    return card({
      title: "Credit history",
      sub: transactions === null ? undefined : `${plural(rows.length, "movement")}`,
      flush: true,
      body: h(
        "div",
        { class: "stack" },
        note
          ? h(
              "div",
              { class: "banner banner--info credit-history-note" },
              icon("info", 15),
              h("span", null, note),
            )
          : null,
        transactions === null
          ? emptyState({
              title: "The ledger is not listed here",
              body: "Every credit movement is a posting on the customer credit account, and this build does not expose that list to the portal. A deposit's invoice shows what was paid; a goodwill credit shows on the invoice it was issued against.",
              icon: "wallet",
              action: btn("Your invoices", { href: "/portal/invoices", size: "sm", variant: "ghost", icon: "invoice" }),
            })
          : rows.length === 0
            ? emptyState({
                title: "No movements yet",
                body: "A deposit or a goodwill credit appears here as soon as it settles, with the balance it left behind.",
                icon: "wallet",
              })
            : h("div", { class: "credit-history" }, ...rows.map((row) => historyRow(row, fmt))),
      ),
    });
  };

  const historyRow = (row: CreditTransaction, fmt: (amount: Amount | null | undefined) => string): HTMLElement => {
    const positive = (row.amount?.minor ?? 0) >= 0;
    const reference = row.reference_id
      ? row.reference_type === "invoice"
        ? `/portal/invoices/${encodeURIComponent(row.reference_id)}`
        : null
      : null;
    return h(
      "div",
      { class: "credit-history-row" },
      h(
        "div",
        { class: "credit-history-main" },
        h(
          "div",
          { class: "credit-history-title" },
          reference
            ? h("a", { class: "row-title", href: reference }, row.description || kindLabel(row.kind))
            : h("span", { class: "row-title" }, row.description || kindLabel(row.kind)),
          row.kind ? badge(kindLabel(row.kind), positive ? "success" : "neutral") : null,
        ),
        h(
          "div",
          { class: "credit-history-meta" },
          row.created_at ? h("span", null, dateOnly(row.created_at)) : null,
          row.balance_after ? h("span", { class: "credit-history-dot" }, "·") : null,
          row.balance_after ? h("span", null, `balance after ${fmt(row.balance_after)}`) : null,
        ),
      ),
      h(
        "div",
        { class: "credit-history-side" },
        h(
          "span",
          { class: `money${positive ? "" : " money--neg"}` },
          positive ? "+" : "",
          fmt(row.amount ?? null),
        ),
      ),
    );
  };

  paint();

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Credit",
      sub: "What is on account, and how to add to it. The balance is read from the ledger, so it always agrees with the books.",
      actions: btn("Back to overview", { href: "/portal", size: "sm", icon: "arrowLeft" }),
    }),
    host,
  );
}

// ----------------------------------------------------------------- helpers --

/** Only http(s) is followed: a redirect target from a gateway is data, not a command. */
function safeURL(raw: string | null | undefined): string | null {
  if (!raw) return null;
  try {
    const parsed = new URL(raw, window.location.origin);
    if (parsed.protocol !== "https:" && parsed.protocol !== "http:") return null;
    return parsed.toString();
  } catch {
    return null;
  }
}

function kindLabel(kind: string | undefined): string {
  switch (kind) {
    case "deposit":
      return "Top-up";
    case "grant":
      return "Goodwill credit";
    case "application":
      return "Applied to an invoice";
    case "refund":
      return "Refund";
    case "adjustment":
      return "Adjustment";
    default:
      return kind ? kind.replace(/[_-]+/g, " ") : "Movement";
  }
}

/** Plain copy for the documented deposit refusals from `handlePortalCreditDeposit`. */
function depositError(err: ApiError, fmt: (amount: Amount | null | undefined) => string): string {
  switch (err.code) {
    case "credit_disabled":
      return "Top-ups are closed on this installation. Credit already on account can still be spent.";
    case "deposit_below_minimum":
      return err.detail || "That amount is below the minimum top-up.";
    case "deposit_above_maximum":
      return err.detail || "That amount is above the maximum single top-up.";
    case "max_credit_balance":
      return err.detail || "That top-up would take your balance over the maximum this installation allows.";
    case "gateway_unavailable":
      return "The payment gateway is not available right now, so the top-up was not started. Nothing has been charged — try again in a moment.";
    case "invalid_amount":
      return "The amount must be positive.";
    case "invalid_currency":
      return "Choose a currency for the top-up.";
    case "not_found":
      return "This login has no billing account, so there is nothing to top up.";
    default:
      return err.detail || `The top-up could not be started (${err.code}).`;
  }
}
