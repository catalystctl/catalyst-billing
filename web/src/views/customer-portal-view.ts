// A customer's portal, as a staff member sees it.
//
// The point of this screen is that it is *the same* screen the customer sees, with the actions
// removed. Support questions are almost always "what does my account look like right now", and
// the answer has to be the customer's view rather than a staff-shaped summary of it — a summary
// is what produces "but I see something different" replies.
//
// It is read-only on purpose. Paying, cancelling and editing billing details stay on the
// customer's own endpoints, where the identity is theirs. An operator who needs to change
// something has the customer's own staff screens for that, each with its own audit trail; this
// page exists so they can look before they act.
//
// The banner is not decoration. An operator who forgets they are looking at somebody else's
// account is how a "why is this empty" ticket gets answered about the wrong customer.

import { h } from "../lib/dom";
import { dateOnly, money, relative, statusTone, type Amount } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  badge,
  btn,
  card,
  emptyState,
  errorState,
  pageHead,
  skeletonPage,
  statusBadge,
} from "../ui/kit";

export const meta = { title: "Customer portal", crumb: "Portal view" };

interface PortalCustomer {
  id: string;
  name: string;
  email: string;
}

interface PortalInvoice {
  id: string;
  number: string;
  status: string;
  currency: string;
  total: Amount;
  paid: Amount;
  due: Amount;
  overdue: boolean;
  issued_at: string | null;
  due_at: string | null;
  payable: boolean;
}

interface PortalService {
  id: string;
  name: string;
  status: string;
  next_bill_at: string | null;
  cancel_at_period_end: boolean;
  cancellable: boolean;
}

interface Overview {
  customer: PortalCustomer;
  balance: { outstanding: Amount[]; credit: Amount[] };
  services: PortalService[];
  open_invoices: PortalInvoice[];
  recent_invoices: PortalInvoice[];
}

export default async function staffPortalViewPage(ctx: Ctx): Promise<Node> {
  const customerID = ctx.route.params.id ?? "";
  const host = h("div", { class: "page" }, skeletonPage());

  const load = async () => {
    try {
      const data = await ctx.api.get<Overview>(
        `/api/v1/staff/customers/${encodeURIComponent(customerID)}/portal`,
        { cache: false },
      );
      host.replaceChildren(render(data));
    } catch (err) {
      host.replaceChildren(errorState(err, () => void load()));
    }
  };

  const render = (data: Overview): Node => {
    const outstanding = data.balance?.outstanding ?? [];
    const credit = data.balance?.credit ?? [];
    const services = data.services ?? [];
    const open = data.open_invoices ?? [];
    const recent = data.recent_invoices ?? [];

    return h(
      "div",
      { class: "page" },
      pageHead({
        title: data.customer.name || data.customer.email,
        sub: `The customer portal for ${data.customer.email}, read-only.`,
        actions: h(
          "div",
          { class: "row" },
          btn("Open the customer record", {
            href: `/customers/${data.customer.id}`,
            size: "sm",
          }),
          btn("Refresh", { icon: "refresh", size: "sm", onclick: () => void load() }),
        ),
      }),

      h(
        "div",
        { class: "banner banner--info" },
        icon("eye", 15),
        h(
          "span",
          null,
          h("strong", null, "You are viewing this as staff."),
          " Nothing here can pay, cancel or change anything — those stay with the customer. Opening this page is recorded in the audit log.",
        ),
      ),

      // The same hero the customer sees, without the Pay buttons.
      h(
        "section",
        { class: "portal-hero" },
        h(
          "div",
          null,
          h("h2", { class: "detail-title" }, `Hello, ${firstName(data.customer.name || data.customer.email)}`),
          h(
            "p",
            { class: "dim" },
            open.length === 0
              ? "No invoice is waiting for payment."
              : `${open.length} invoice${open.length === 1 ? "" : "s"} awaiting payment.`,
          ),
        ),
        h(
          "div",
          { class: "portal-balance" },
          h(
            "span",
            { class: "portal-balance-label" },
            outstanding.length > 1 ? "Outstanding, per currency" : "Outstanding",
          ),
          outstanding.length === 0
            ? h("span", { class: "money money--xl faint" }, "Nothing due")
            : h("div", { class: "portal-balance-amounts" }, ...outstanding.map((a) => moneyElBlock(a))),
          ...credit.map((a) =>
            h("span", { class: "portal-balance-sub" }, "Credit ", h("span", { class: "money" }, money(a))),
          ),
        ),
      ),

      h(
        "div",
        { class: "split" },
        h(
          "div",
          { class: "stack" },
          card({
            title: "Invoices awaiting payment",
            sub: open.length > 0 ? `${open.length}` : undefined,
            flush: true,
            body:
              open.length === 0
                ? emptyState({
                    title: "Nothing to pay",
                    body: "Every issued invoice is settled. This is what the customer sees.",
                    icon: "checkCircle",
                  })
                : h("div", null, ...open.map(invoiceRow)),
          }),
          card({
            title: "Recent invoices",
            actions: h(
              "a",
              { class: "linkish", href: `/customers/${data.customer.id}` },
              "All invoices",
              icon("chevronRight", 12),
            ),
            flush: true,
            body:
              recent.length === 0
                ? emptyState({ title: "No invoices", body: "This customer has no issued invoice.", icon: "invoice" })
                : h("div", null, ...recent.map(invoiceRow)),
          }),
        ),
        h(
          "div",
          { class: "stack" },
          card({
            title: "Services",
            sub: services.length > 0 ? `${services.length}` : undefined,
            flush: true,
            body:
              services.length === 0
                ? emptyState({
                    title: "No services",
                    body: "Nothing is provisioned for this customer.",
                    icon: "server",
                  })
                : h("div", null, ...services.map(serviceRow)),
          }),
          card({
            title: "What this page does not do",
            body: h(
              "p",
              { class: "field-hint" },
              "There are no Pay, Cancel or Edit buttons here, and that is deliberate: an impersonated session that can move money is a much larger decision than one that can look. To act on this account, use the customer record, the invoice, or the service — each of those writes its own audit entry.",
            ),
          }),
        ),
      ),
    );
  };

  const moneyElBlock = (a: Amount): HTMLElement =>
    h("span", { class: "money money--xl" }, h("span", { class: "money-code" }, a.currency), money(a));

  const invoiceRow = (inv: PortalInvoice): HTMLElement =>
    h(
      "a",
      { class: "portal-invoice-row", href: `/customers/${ctx.route.params.id}?tab=invoices` },
      h(
        "div",
        { class: "cell-main" },
        h(
          "div",
          { class: "primary" },
          inv.number || "Draft",
          inv.overdue ? badge("Overdue", "warning", { dot: true }) : null,
        ),
        h(
          "div",
          { class: "secondary" },
          inv.issued_at ? `issued ${dateOnly(inv.issued_at)}` : "not issued",
          inv.due_at ? ` · due ${relative(inv.due_at)}` : "",
        ),
      ),
      h(
        "div",
        { class: "portal-invoice-amounts" },
        h("span", { class: "money" }, money(inv.total)),
        inv.due.minor > 0
          ? h("span", { class: "portal-invoice-due" }, `${money(inv.due)} due`)
          : h("span", { class: "faint" }, "settled"),
      ),
      statusBadge(inv.status),
    );

  const serviceRow = (svc: PortalService): HTMLElement =>
    h(
      "div",
      { class: "portal-service" },
      h(
        "div",
        { class: "portal-service-main" },
        h(
          "div",
          { class: "portal-service-name" },
          svc.name || "Service",
          svc.cancel_at_period_end ? badge("Cancels at period end", "warning", { dot: true }) : null,
        ),
        h(
          "div",
          { class: "portal-service-meta" },
          svc.next_bill_at ? `renews ${relative(svc.next_bill_at)}` : "no renewal scheduled",
        ),
      ),
      statusBadge(svc.status),
    );

  void load();
  return host;
}

function firstName(name: string): string {
  return name.trim().split(/\s+/)[0] || name;
}
