// One invoice, as the customer sees it.
//
// The portal payload is `invoiceJSON` from internal/httpapi/portal.go: id, number, status,
// currency, total, paid, due, overdue, payable, issued_at, due_at. It carries **no line
// items**, no issuer address and no customer snapshot, so this document shows the totals that
// exist and says plainly that the itemised lines are not part of the portal. Inventing a
// "Description / Qty / Unit price" table from the total would be a document that disagrees
// with the one that was issued.
//
// There is no Download button: no endpoint serves a PDF to the customer, and a button that
// does nothing is worse than no button.
//
// Two ways to pay, and the second one is the reason this page owns its own reload:
//
//  - **The gateway.** `POST /portal/invoices/{id}/checkout` returns a redirect URL and the
//    amount comes from the invoice, never from the browser.
//  - **Credit on account.** `POST /portal/invoices/{id}/pay-with-credit` spends the customer's
//    balance on this invoice. The endpoint has existed since the pay flow was built and no
//    screen ever called it, so an installation whose gateway is unconfigured (or whose
//    `credits.auto_use` is off) left a customer holding credit with no way to use it and no
//    way to pay at all. It is offered only when the invoice is payable and the balance is in
//    the invoice's own currency, because there is no exchange rate to convert one into the
//    other.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateOnly, dateTime, type Amount } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { banner, btn, moneyEl, pageHead, resource, skeletonPage, statusBadge, withBusy } from "../ui/kit";
import { payButton, type PortalInvoice } from "./portal";

export const meta = { title: "Invoice", crumb: "Invoice" };

interface InvoicePayload {
  id: string;
  number: string;
  status: string;
  currency: string;
  total: Amount;
  paid: Amount;
  due: Amount;
  overdue: boolean;
  payable: boolean;
  issued_at?: string;
  due_at?: string;
}

/** `POST /portal/invoices/{id}/pay-with-credit` — `handlePortalPayWithCredit` in
 *  internal/httpapi/payflow.go. `invoice_paid` is derived from the allocation, not from the
 *  request: a partial balance leaves the invoice payable and the page says so. */
interface CreditApplication {
  application_id: string;
  amount: Amount;
  total: Amount;
  paid: Amount;
  remaining: Amount;
  invoice_paid: boolean;
  already_applied: boolean;
  credit_balance: Amount[] | null;
}

interface InvoiceView {
  invoice: InvoicePayload;
  /** The balance in the invoice's own currency, or null when there is none to spend. */
  credit: Amount | null;
}

export default async function portalInvoicePage(ctx: Ctx): Promise<Node> {
  const id = ctx.route.params.id ?? "";
  const host = h("div", { class: "stack" });
  // The page repaints itself after a payment, because the document is the server's answer and a
  // patched copy of it would be a second source of truth for money.
  const paint = (): void => {
    swap(
      host,
      resource(ctx, (signal) => load(ctx, id, signal), (view) => render(ctx, view, paint), { skeleton: skeletonPage }),
    );
  };
  paint();
  return host;
}

async function load(ctx: Ctx, id: string, signal: AbortSignal): Promise<InvoiceView> {
  let invoice: InvoicePayload;
  try {
    invoice = await ctx.api.get<InvoicePayload>(`/api/v1/portal/invoices/${encodeURIComponent(id)}`, {
      cache: false,
      signal,
    });
  } catch (err) {
    throw invoiceLoadError(err);
  }
  // A second read, and a failure here is not fatal: an invoice that cannot see the credit
  // balance is still an invoice, it just offers one fewer way to pay it.
  let credit: Amount | null = null;
  try {
    const res = await ctx.api.get<{ balances: Amount[] | null }>("/api/v1/portal/credits", {
      cache: false,
      signal,
    });
    credit =
      (res.balances ?? []).find((row) => row.currency.toUpperCase() === invoice.currency.toUpperCase()) ?? null;
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") throw err;
    credit = null;
  }
  return { invoice, credit };
}

/** A 403 and a 404 both mean "not yours or not there" — the API deliberately makes them
 *  indistinguishable, so the page says one plain sentence for both. */
function invoiceLoadError(err: unknown): unknown {
  if (err instanceof ApiError && (err.status === 403 || err.status === 404)) {
    return new Error(
      "This invoice is not available. It may belong to another account, or it may no longer exist.",
    );
  }
  return err;
}

function render(ctx: Ctx, view: InvoiceView, repaint: () => void): Node {
  const inv = view.invoice;
  const invoice: PortalInvoice = inv;
  const user = ctx.session.user;
  const account = [user.display_name || "Your account", user.email].filter(Boolean).join("\n");

  const notice = noticeFor(inv);
  // Credit is offered only when it can actually settle part of this invoice: a payable invoice,
  // and a balance in the invoice's own currency. Offering it otherwise would be a button whose
  // only outcome is a refusal.
  const usableCredit = inv.payable && view.credit && view.credit.minor > 0 ? view.credit : null;
  const creditButton = usableCredit ? creditPayButton(ctx, invoice, usableCredit, repaint) : null;

  return h(
    "div",
    { class: "stack" },
    pageHead({
      title: inv.number ? `Invoice ${inv.number}` : "Invoice",
      badge: statusBadge(inv.overdue ? "overdue" : inv.status),
      sub: inv.issued_at ? `Issued ${dateTime(inv.issued_at)}` : "This invoice has not been issued yet.",
      actions: h(
        "div",
        { class: "row-wrap" },
        inv.payable ? payButton(ctx, invoice, { size: "md" }) : null,
        creditButton,
        btn("Back to invoices", { href: "/portal/invoices", size: "md", icon: "arrowLeft" }),
      ),
    }),
    notice,
    usableCredit
      ? h(
          "div",
          { class: "banner banner--info" },
          icon("wallet", 15),
          h(
            "span",
            null,
            h("strong", null, "You have credit on account."),
            ` Paying with it spends up to ${usableCredit.display || "your balance"} on this invoice; the ledger entry and the invoice status change together.`,
          ),
        )
      : null,
    h(
      "section",
      { class: "invoice-doc" },
      h(
        "header",
        { class: "doc-head" },
        h(
          "div",
          { class: "doc-issuer" },
          h("strong", null, "Invoice"),
          h("span", { class: "mono" }, inv.number || "No number"),
          h("span", null, statusBadge(inv.overdue ? "overdue" : inv.status)),
        ),
        h(
          "dl",
          { class: "doc-meta" },
          h("dt", null, "Issued"),
          h("dd", null, inv.issued_at ? dateOnly(inv.issued_at) : "—"),
          h("dt", null, "Due"),
          h("dd", null, inv.due_at ? dateOnly(inv.due_at) : "—"),
          h("dt", null, "Currency"),
          h("dd", null, inv.currency),
        ),
      ),
      h(
        "div",
        { class: "doc-parties" },
        h(
          "div",
          null,
          h("div", { class: "doc-party-label" }, "Your account"),
          h("div", { class: "doc-party" }, account),
        ),
      ),
      h(
        "div",
        { class: "doc-totals" },
        totalRow("Total", inv.total),
        totalRow("Paid", inv.paid),
        totalRow("Outstanding", inv.due, true),
      ),
      h(
        "footer",
        { class: "doc-foot" },
        h("span", null, "Totals as issued. The portal does not show itemised lines."),
        h("span", { class: "mono" }, inv.id),
      ),
    ),
  );
}

function totalRow(label: string, amount: Amount, grand = false): HTMLElement {
  return h(
    "div",
    { class: `doc-total-row${grand ? " doc-total-row--grand" : ""}` },
    h("span", null, label),
    moneyEl(amount),
  );
}

function noticeFor(inv: InvoicePayload): Node | null {
  if (inv.overdue && inv.due_at) {
    return banner(
      "warning",
      "This invoice is overdue",
      `It was due on ${dateOnly(inv.due_at)}. Paying now clears the outstanding amount.`,
    );
  }
  if (inv.status === "paid") {
    return banner("success", "Paid in full", "Nothing further is owed on this invoice.");
  }
  if (inv.status === "void") {
    return banner("neutral", "This invoice was voided", "Nothing is owed on it, and it cannot be paid.");
  }
  if (!inv.payable && inv.due.minor <= 0) {
    return banner("success", "Settled", "There is nothing left to pay on this invoice.");
  }
  if (!inv.payable) {
    return banner(
      "neutral",
      "This invoice cannot be paid right now",
      "It may not have been issued yet. If this looks wrong, contact support.",
    );
  }
  return h(
    "div",
    { class: "banner banner--info" },
    icon("info", 15),
    h(
      "span",
      null,
      h("strong", null, "Paying opens the payment gateway."),
      " The amount is taken from this invoice, and you are returned here afterwards.",
    ),
  );
}

/** Pays this invoice from the customer's credit balance.
 *
 *  The body is empty on purpose: the amount is the invoice's outstanding total and the balance
 *  is the subledger's, and the server reads both itself under a lock. A balance that does not
 *  cover the invoice leaves it payable, and the toast says what is left rather than claiming a
 *  payment that did not happen. */
function creditPayButton(
  ctx: Ctx,
  invoice: PortalInvoice,
  credit: Amount,
  repaint: () => void,
): HTMLButtonElement {
  const el = btn(h("span", null, "Pay with credit"), {
    variant: "primary",
    size: "md",
    icon: "wallet",
    title: `Spend up to ${credit.display || `${credit.minor / 10 ** credit.scale} ${credit.currency}`} of your credit on this invoice`,
  }) as HTMLButtonElement;
  el.id = "invoice-pay-with-credit";
  el.addEventListener("click", () =>
    void withBusy(ctx, el, async () => {
      let application: CreditApplication;
      try {
        application = await ctx.api.post<CreditApplication>(
          `/api/v1/portal/invoices/${encodeURIComponent(invoice.id)}/pay-with-credit`,
          {},
        );
      } catch (err) {
        reportCreditError(ctx, err);
        return;
      }
      ctx.api.invalidate("/api/v1/portal");
      const remaining = application.remaining?.display ?? "";
      ctx.toast(application.invoice_paid ? "Invoice paid" : "Credit applied", {
        tone: "success",
        detail: application.invoice_paid
          ? `Nothing is left to pay on ${invoice.number || "this invoice"}.`
          : `${remaining} is still outstanding on this invoice.`,
      });
      // The document is the server's answer, so it is re-read rather than patched: a paid
      // invoice shows a different badge, a different outstanding total and a different notice.
      repaint();
    }),
  );
  return el;
}

/** Plain copy for every documented refusal from the credit endpoint. */
function reportCreditError(ctx: Ctx, err: unknown): void {
  if (!(err instanceof ApiError)) {
    ctx.toast("The credit could not be applied", {
      tone: "danger",
      detail: "The server could not be reached. Nothing has been charged.",
    });
    return;
  }
  const messages: Record<string, { title: string; detail: string }> = {
    insufficient_credit: {
      title: "There is no credit to apply",
      detail: "Your balance is empty, or it is held in another currency. Pay with the gateway instead.",
    },
    nothing_to_apply: {
      title: "There is nothing to apply credit to",
      detail: "This invoice owes nothing, so no credit was spent.",
    },
    credit_disabled: {
      title: "Credit is switched off on this installation",
      detail: "An operator has disabled customer credit. Pay with the gateway instead.",
    },
    not_found: {
      title: "This invoice is not available",
      detail: "It may belong to another account, or it may no longer exist.",
    },
    portal_unavailable: {
      title: "The customer portal is not configured",
      detail: "This deployment cannot take payments from the portal.",
    },
  };
  const hit = messages[err.code];
  if (hit) {
    ctx.toast(hit.title, { tone: "danger", detail: hit.detail, duration: 0 });
    return;
  }
  ctx.toast(err.detail || "The credit could not be applied", { tone: "danger", detail: err.code, duration: 0 });
}
