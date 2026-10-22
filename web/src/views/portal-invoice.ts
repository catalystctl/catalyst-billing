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

import { ApiError } from "../lib/api";
import { h } from "../lib/dom";
import { dateOnly, dateTime, type Amount } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { banner, btn, moneyEl, pageHead, resource, skeletonPage, statusBadge } from "../ui/kit";
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

export default async function portalInvoicePage(ctx: Ctx): Promise<Node> {
  const id = ctx.route.params.id ?? "";
  return resource(
    ctx,
    async (signal) => {
      try {
        return await ctx.api.get<InvoicePayload>(`/api/v1/portal/invoices/${encodeURIComponent(id)}`, {
          cache: false,
          signal,
        });
      } catch (err) {
        throw invoiceLoadError(err);
      }
    },
    (invoice) => render(ctx, invoice),
    { skeleton: skeletonPage },
  );
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

function render(ctx: Ctx, inv: InvoicePayload): Node {
  const invoice: PortalInvoice = inv;
  const user = ctx.session.user;
  const account = [user.display_name || "Your account", user.email].filter(Boolean).join("\n");

  const notice = noticeFor(inv);

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
        btn("Back to invoices", { href: "/portal/invoices", size: "md", icon: "arrowLeft" }),
      ),
    }),
    notice,
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
