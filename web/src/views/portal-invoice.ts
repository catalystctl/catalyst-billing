// One invoice, as the customer sees it.
//
// The portal payload is `invoiceJSON` from internal/httpapi/portal.go: id, number, status,
// currency, total, paid, due, overdue, payable, issued_at, due_at. It carries **no line
// items**, no issuer address and no customer snapshot, so this document shows the totals that
// exist and says plainly that the itemised lines are not part of the portal. Inventing a
// "Description / Qty / Unit price" table from the total would be a document that disagrees
// with the one that was issued.
//
// The documents themselves are a second read (`GET /portal/invoices/{id}/document`): the PDF
// the customer was sent, the credit notes issued against it, the operator's note, and whether
// the document is a proforma. That read is what makes the download reachable — the PDF route
// always existed, but nothing on the page named the document it serves.
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
import { banner, btn, card, moneyEl, pageHead, resource, skeletonPage, statusBadge, withBusy } from "../ui/kit";
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

/** One credit note issued against this invoice, from
 *  `GET /api/v1/portal/invoices/{id}/document` (`handlePortalInvoiceDocument` in
 *  internal/httpapi/invoice_documents.go). The id is what the PDF route is keyed on. */
interface CreditNoteDoc {
  id: string;
  number: string;
  total: Amount;
  tax: Amount;
  reason: string;
  status: string;
  issued_at: string | null;
}

/** The document facts a customer may read. The note is already filtered by
 *  `invoices.notes_client_visible` on the server, so an empty `client_note` means "nothing to
 *  print", never "you may not see it". */
interface DocumentState {
  id: string;
  status: string;
  proforma: boolean;
  client_note: string;
  notes_visible: boolean;
  void_reason: string;
  credit_notes: CreditNoteDoc[];
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
  /** The document facts, or null when they could not be read: an invoice that cannot see its
   *  credit notes is still an invoice, it just offers one fewer document. */
  doc: DocumentState | null;
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

  // A third read, and the same rule: the document state is additive. A failure here means the
  // customer sees the invoice without its PDF and credit-note links, not an error page.
  let doc: DocumentState | null = null;
  try {
    doc = await ctx.api.get<DocumentState>(`/api/v1/portal/invoices/${encodeURIComponent(id)}/document`, {
      cache: false,
      signal,
    });
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") throw err;
    doc = null;
  }
  return { invoice, credit, doc };
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
  const doc = view.doc;
  const creditNotes = doc?.credit_notes ?? [];
  const clientNote = (doc?.client_note ?? "").trim();
  const proforma = doc?.proforma === true;

  const notice = noticeFor(inv, doc);
  // Credit is offered only when it can actually settle part of this invoice: a payable invoice,
  // and a balance in the invoice's own currency. Offering it otherwise would be a button whose
  // only outcome is a refusal.
  const usableCredit = inv.payable && view.credit && view.credit.minor > 0 ? view.credit : null;
  const creditButton = usableCredit ? creditPayButton(ctx, invoice, usableCredit, repaint) : null;

  return h(
    "div",
    { class: "stack" },
    pageHead({
      title: inv.number ? `Invoice ${inv.number}` : proforma ? "Proforma" : "Invoice",
      badge: statusBadge(inv.overdue ? "overdue" : inv.status),
      sub: inv.issued_at ? `Issued ${dateTime(inv.issued_at)}` : "This invoice has not been issued yet.",
      actions: h(
        "div",
        { class: "row-wrap" },
        inv.payable ? payButton(ctx, invoice, { size: "md" }) : null,
        creditButton,
        // The document the invoice is about. It is only offered when the server confirms there
        // is one: a draft has no number and no PDF, and a button that leads to a 404 is worse
        // than no button.
        doc
          ? h(
              "a",
              {
                class: "btn",
                href: `/api/v1/portal/invoices/${encodeURIComponent(inv.id)}/pdf`,
                download: true,
                "data-native": "true",
                title: "Download this invoice as a PDF",
              },
              icon("download", 14),
              "Download invoice",
            )
          : null,
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
          h("strong", null, proforma ? "Proforma" : "Invoice"),
          h("span", { class: "mono" }, inv.number || (proforma ? "No number yet" : "No number")),
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
        // The operator's note, reprinted where the customer reads it. The server has already
        // applied the installation's note policy, so a note that arrives here is one the
        // document prints.
        clientNote
          ? h(
              "div",
              null,
              h("div", { class: "doc-party-label" }, "Note from us"),
              h("div", { class: "doc-party" }, clientNote),
            )
          : null,
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
    creditNotes.length > 0 ? creditNotesCard(creditNotes) : null,
  );
}

/** The credit notes issued against this invoice.
 *
 *  They are separate documents, not negative lines on this one: each has its own number, its
 *  own date and its own PDF. The download is the reason the section exists — a customer who is
 *  told they have been credited should be able to hold the note that says so. */
function creditNotesCard(notes: CreditNoteDoc[]): HTMLElement {
  return card({
    title: "Credit notes",
    sub: notes.length === 1 ? "1 credit note against this invoice" : `${notes.length} credit notes against this invoice`,
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
            h("th", { scope: "col" }, "Credit note"),
            h("th", { class: "num", scope: "col" }, "Total"),
            h("th", { scope: "col" }, "Issued"),
            h("th", { scope: "col" }, "Document"),
          ),
        ),
        h(
          "tbody",
          null,
          ...notes.map((note) =>
            h(
              "tr",
              null,
              h(
                "td",
                { class: "cell-primary" },
                h(
                  "div",
                  { class: "cell-main" },
                  h("div", { class: "primary mono" }, note.number || "Credit note"),
                  note.reason ? h("div", { class: "secondary" }, note.reason) : null,
                ),
              ),
              h("td", { class: "num", "data-label": "Total" }, moneyEl(note.total)),
              h(
                "td",
                { "data-label": "Issued" },
                h("span", { class: "dim" }, note.issued_at ? dateOnly(note.issued_at) : "Not issued"),
              ),
              h(
                "td",
                { "data-label": "Document" },
                h(
                  "a",
                  {
                    class: "btn btn--sm",
                    href: `/api/v1/portal/credit-notes/${encodeURIComponent(note.id)}/pdf`,
                    download: true,
                    "data-native": "true",
                    title: `Download credit note ${note.number || "as a PDF"}`,
                  },
                  icon("download", 14),
                  "PDF",
                ),
              ),
            ),
          ),
        ),
      ),
    ),
  });
}

function totalRow(label: string, amount: Amount, grand = false): HTMLElement {
  return h(
    "div",
    { class: `doc-total-row${grand ? " doc-total-row--grand" : ""}` },
    h("span", null, label),
    moneyEl(amount),
  );
}

function noticeFor(inv: InvoicePayload, doc: DocumentState | null): Node | null {
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
    const reason = (doc?.void_reason ?? "").trim();
    return banner(
      "neutral",
      "This invoice was voided",
      reason
        ? `Nothing is owed on it, and it cannot be paid. The reason recorded on the document: ${reason}`
        : "Nothing is owed on it, and it cannot be paid.",
    );
  }
  if (doc?.proforma) {
    return banner(
      "info",
      "This is a proforma",
      "It asks for payment but carries no invoice number: the number is assigned once, when it is paid. A proforma is not a tax invoice.",
    );
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
