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

// Every section is optional. The endpoint removes the ones the caller may not read — a role can
// hold customer:read:all without invoice:read:all — so a section is absent rather than empty,
// and "absent" has to render as "you cannot see this" rather than as "there is nothing here".
interface Overview {
  customer: PortalCustomer;
  balance?: { outstanding: Amount[]; credit: Amount[] };
  services?: PortalService[];
  open_invoices?: PortalInvoice[];
  recent_invoices?: PortalInvoice[];
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

    // Whether the caller may read each section at all. Absent means "not permitted", which is
    // a different sentence from "this customer has none", and an operator who is told the
    // wrong one will draw the wrong conclusion about the account.
    const canSeeBilling = data.balance !== undefined;
    const canSeeServices = data.services !== undefined;

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
            !canSeeBilling
              ? "Their billing is not visible with your permissions."
              : open.length === 0
                ? "No invoice is waiting for payment."
                : `${open.length} invoice${open.length === 1 ? "" : "s"} awaiting payment.`,
          ),
        ),
        canSeeBilling
          ? h(
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
            )
          : h(
              "div",
              { class: "portal-balance" },
              h("span", { class: "portal-balance-label" }, "Balance"),
              h("span", { class: "faint" }, "Not visible with your permissions"),
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
            body: !canSeeBilling
              ? emptyState({
                  title: "Not visible",
                  body: "Your role can see this customer but not their invoices.",
                  icon: "lock",
                })
              : open.length === 0
                ? emptyState({
                    title: "Nothing to pay",
                    body: "Every issued invoice is settled. This is what the customer sees.",
                    icon: "checkCircle",
                  })
                : h("div", null, ...open.map((inv) => invoiceRow(inv, { payable: true }))),
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
            body: !canSeeBilling
              ? emptyState({
                  title: "Not visible",
                  body: "Your role can see this customer but not their invoices.",
                  icon: "lock",
                })
              : recent.length === 0
                ? emptyState({ title: "No invoices", body: "This customer has no issued invoice.", icon: "invoice" })
                : h("div", null, ...recent.map((inv) => invoiceRow(inv))),
          }),
        ),
        h(
          "div",
          { class: "stack" },
          card({
            title: "Services",
            sub: services.length > 0 ? `${services.length}` : undefined,
            flush: true,
            body: !canSeeServices
              ? emptyState({
                  title: "Not visible",
                  body: "Your role can see this customer but not their services.",
                  icon: "lock",
                })
              : services.length === 0
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

  // The row shapes are the portal's own (.portal-row and friends). Reusing them is the whole
  // point: if a row is laid out differently here, the operator is not really seeing what the
  // customer sees. The only difference is the destination — a staff member lands on the staff
  // invoice, not on the customer's checkout page.
  const invoiceRow = (inv: PortalInvoice, opts?: { payable?: boolean }): HTMLElement =>
    h(
      "div",
      { class: "portal-row" },
      h(
        "div",
        { class: "portal-row-main" },
        h(
          "div",
          { class: "portal-row-title" },
          h(
            "a",
            { class: "row-title", href: `/invoices/${encodeURIComponent(inv.id)}` },
            inv.number || "Invoice",
          ),
          inv.overdue ? badge("Overdue", "warning", { dot: true }) : null,
        ),
        h(
          "div",
          { class: "portal-row-meta" },
          h("span", null, inv.issued_at ? `Issued ${dateOnly(inv.issued_at)}` : "Not issued"),
          inv.due_at ? h("span", { class: "portal-row-dot" }, "·") : null,
          inv.due_at ? h("span", null, `due ${dateOnly(inv.due_at)}`) : null,
        ),
      ),
      h(
        "div",
        { class: "portal-row-side" },
        opts?.payable
          ? h(
              "div",
              { class: "portal-row-amount" },
              h("span", { class: "portal-row-label" }, "Outstanding"),
              h("span", { class: "money" }, money(inv.due)),
            )
          : h("span", { class: "money" }, money(inv.total)),
        statusBadge(inv.overdue ? "overdue" : inv.status),
      ),
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
          h(
            "a",
            { class: "row-title", href: `/services/${encodeURIComponent(svc.id)}` },
            svc.name || "Service",
          ),
          svc.cancel_at_period_end ? badge("Cancels at period end", "warning", { dot: true }) : null,
        ),
        h(
          "div",
          { class: "portal-service-meta" },
          svc.next_bill_at ? `Renews ${dateOnly(svc.next_bill_at)}` : "No renewal scheduled",
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
