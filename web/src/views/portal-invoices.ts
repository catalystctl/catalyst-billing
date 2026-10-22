// The customer invoice list.
//
// `GET /api/v1/portal/invoices` answers `{invoices: [...]}` — not the `{data, next_cursor}`
// page shape the staff tables use — so this screen builds its own table out of the same
// `.table` markup. The column set is exactly the payload: number, status, issued_at, due_at,
// total, paid, due (the outstanding amount), overdue, payable. Nothing here is derived.
//
// The "Due" column is the date and "Outstanding" is the amount, because `due` in the payload
// is what remains payable and `due_at` is when it is due. Showing one as the other would be a
// quiet lie about what the customer owes.

import { h } from "../lib/dom";
import { dateOnly, plural } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { badge, btn, card, cellMain, emptyState, moneyEl, pageHead, resource, skeletonTable, statusBadge } from "../ui/kit";
import { payButton, portalLoadError, type PortalInvoice } from "./portal";

export const meta = { title: "Invoices", crumb: "Invoices" };

interface InvoiceList {
  invoices: PortalInvoice[];
}

export default async function portalInvoicesPage(ctx: Ctx): Promise<Node> {
  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Invoices",
      sub: "Everything issued to your account, newest first. Paying opens the payment gateway and returns you here.",
      actions: btn("Back to overview", { href: "/portal", size: "sm", icon: "arrowLeft" }),
    }),
    resource(
      ctx,
      async (signal) => {
        try {
          return await ctx.api.get<InvoiceList>("/api/v1/portal/invoices", { cache: false, signal });
        } catch (err) {
          throw portalLoadError(err);
        }
      },
      (data) => {
        const invoices = data.invoices ?? [];
        if (invoices.length === 0) {
          return card({
            flush: true,
            body: emptyState({
              title: "No invoices yet",
              body: "An invoice appears here as soon as one is issued. Nothing is owed until then.",
              icon: "invoice",
              action: btn("Back to overview", { href: "/portal", size: "sm" }),
            }),
          });
        }
        const payable = invoices.filter((inv) => inv.payable).length;
        return card({
          title: "All invoices",
          sub: payable > 0 ? `${plural(payable, "invoice")} can be paid now` : "Nothing is waiting for payment",
          flush: true,
          body: invoicesTable(ctx, invoices),
          foot: h(
            "span",
            { class: "row" },
            icon("info", 12),
            "Payments are handled by the payment gateway. The amount is set by the invoice, never by this page.",
          ),
        });
      },
      { skeleton: () => skeletonTable(8, 8) },
    ),
  );
}

function invoicesTable(ctx: Ctx, invoices: PortalInvoice[]): HTMLElement {
  const thead = h(
    "thead",
    null,
    h(
      "tr",
      null,
      h("th", { scope: "col" }, "Invoice"),
      h("th", { scope: "col" }, "Status"),
      h("th", { scope: "col", class: "hide-sm" }, "Issued"),
      h("th", { scope: "col", class: "hide-sm" }, "Due"),
      h("th", { scope: "col", class: "num" }, "Total"),
      h("th", { scope: "col", class: "num hide-sm" }, "Paid"),
      h("th", { scope: "col", class: "num" }, "Outstanding"),
      h("th", { scope: "col", class: "hide-sm" }, "Overdue"),
      h("th", { scope: "col", class: "cell-actions" }, ""),
    ),
  );

  const tbody = h("tbody");
  for (const inv of invoices) tbody.appendChild(invoiceRow(ctx, inv));

  return h("div", { class: "table-wrap table-wrap--cards" }, h("table", { class: "table" }, thead, tbody));
}

function invoiceRow(ctx: Ctx, inv: PortalInvoice): HTMLElement {
  const href = `/portal/invoices/${encodeURIComponent(inv.id)}`;
  const tr = h("tr", { class: "is-clickable", dataset: { href } });
  tr.addEventListener("click", (event) => {
    const target = event.target as Element | null;
    // A control inside the row keeps its own behaviour: the Pay button must not navigate.
    if (target?.closest("a,button")) return;
    ctx.navigate(href);
  });

  tr.appendChild(
    h(
      "td",
      { class: "cell-primary" },
      cellMain(
        h("a", { class: "row-title", href }, inv.number || "Invoice"),
        h("span", { class: "mono" }, inv.currency),
      ),
    ),
  );
  tr.appendChild(h("td", { "data-label": "Status" }, statusBadge(inv.overdue ? "overdue" : inv.status)));
  tr.appendChild(
    h("td", { class: "hide-sm", "data-label": "Issued" }, h("span", { class: "dim" }, dateOnly(inv.issued_at))),
  );
  tr.appendChild(
    h(
      "td",
      { class: "hide-sm", "data-label": "Due" },
      inv.due_at
        ? h("span", { class: inv.overdue ? "portal-due--overdue" : "dim" }, dateOnly(inv.due_at))
        : h("span", { class: "faint" }, "—"),
    ),
  );
  tr.appendChild(h("td", { class: "num", "data-label": "Total" }, moneyEl(inv.total)));
  tr.appendChild(h("td", { class: "num hide-sm", "data-label": "Paid" }, moneyEl(inv.paid)));
  tr.appendChild(
    h(
      "td",
      { class: "num", "data-label": "Outstanding" },
      inv.due.minor > 0 ? moneyEl(inv.due) : h("span", { class: "faint" }, "Settled"),
    ),
  );
  tr.appendChild(
    h(
      "td",
      { class: "hide-sm", "data-label": "Overdue" },
      inv.overdue ? badge("Overdue", "warning", { dot: true }) : h("span", { class: "faint" }, "—"),
    ),
  );
  tr.appendChild(
    h(
      "td",
      { class: "cell-actions", "data-label": "Pay" },
      inv.payable
        ? payButton(ctx, inv)
        : h("a", { class: "linkish", href }, "View", icon("chevronRight", 12)),
    ),
  );

  return tr;
}
