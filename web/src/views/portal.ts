// The customer landing page.
//
// One question, answered in the first line: what do I owe, and what happens next. Everything
// below it is the detail behind that answer — the invoices waiting for payment, the services
// that will renew, and the invoices that are already settled.
//
// The Pay button is one flow shared by three screens (this one, the invoice list and the
// invoice document). It lives here, in the portal's entry module, so the two rules that make
// checkout safe are written once: the request body carries no amount (the server reads it from
// the invoice under a row lock), and the gateway's redirect URL is only followed when
// safeExternalURL() accepts it — a URL that arrives from another system is data, not a command.

import { ApiError } from "../lib/api";
import { h } from "../lib/dom";
import { dateOnly, moneyWithCode, safeExternalURL, shortId, type Amount } from "../lib/format";
import { tx } from "../lib/i18n";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { badge, btn, card, emptyState, moneyEl, resource, statusBadge, withBusy } from "../ui/kit";

export const meta = { title: "Overview", crumb: "Overview" };

// ---------------------------------------------------------------- payloads --

/** One invoice, exactly as `invoiceJSON` in internal/httpapi/portal.go emits it.
 *  `due` is what remains payable; `due_at` is when it is due. There are no line items in the
 *  portal payload, so no screen may render any. */
export interface PortalInvoice {
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

/** One service, exactly as `serviceJSON` emits it. `actions` is computed by the server from
 *  the same rules the action handlers enforce, so the UI offers only what would be accepted. */
export interface PortalService {
  id: string;
  name: string;
  status: string;
  actions: string[];
  next_bill_at?: string;
}

export interface PortalOverview {
  customer: { id: string; name: string; email: string };
  balance: { outstanding: Amount[]; credit: Amount[] };
  services: PortalService[];
  open_invoices: PortalInvoice[];
  recent_invoices: PortalInvoice[];
}

export interface CheckoutSession {
  attempt_id: string;
  invoice_id: string;
  redirect_url: string;
  amount: Amount;
  reused: boolean;
}

// ---------------------------------------------------------------- checkout --

/** A Pay button for one invoice. Disabled while the request is in flight. */
export function payButton(
  ctx: Ctx,
  invoice: PortalInvoice,
  opts: { size?: "sm" | "md"; block?: boolean } = {},
): HTMLButtonElement {
  const label = h("span", null, `${tx("portal.pay.action", "Pay")} `, moneyEl(invoice.due));
  const el = btn(label, {
    variant: "primary",
    size: opts.size ?? "sm",
    icon: "card",
    block: opts.block,
    title: invoice.number
      ? tx("portal.pay.title", "Pay invoice {{number}}", { number: invoice.number })
      : tx("portal.pay.title_plain", "Pay this invoice"),
  }) as HTMLButtonElement;
  el.addEventListener("click", () => void startCheckout(ctx, invoice, el));
  return el;
}

/** Starts a checkout and leaves for the gateway.
 *
 *  The body is an empty object on purpose: `handlePortalCheckout` reads no field from it and
 *  the amount is never client-supplied. Sending one would be a lie about where the amount
 *  comes from even if the server ignored it. */
export async function startCheckout(ctx: Ctx, invoice: PortalInvoice, button: HTMLButtonElement): Promise<void> {
  await withBusy(ctx, button, async () => {
    let session: CheckoutSession;
    try {
      session = await ctx.api.post<CheckoutSession>(
        `/api/v1/portal/invoices/${encodeURIComponent(invoice.id)}/checkout`,
        {},
      );
    } catch (err) {
      reportCheckoutError(ctx, err);
      return;
    }

    const target = safeExternalURL(session.redirect_url);
    if (!target) {
      // A gateway that answers with no link is a gateway problem, not a customer problem:
      // say what happened, say that nothing was charged, and leave the invoice payable.
      ctx.toast(tx("portal.checkout.no_link_title", "The payment page could not be opened"), {
        tone: "danger",
        duration: 0,
        detail: tx(
          "portal.checkout.no_link_detail",
          "The payment gateway returned no usable link, so there is nowhere to send you. Nothing has been charged — try again, and contact support if it keeps happening.",
        ),
      });
      return;
    }

    ctx.toast(
      session.reused
        ? tx("portal.checkout.resuming", "Resuming your payment")
        : tx("portal.checkout.redirecting", "Taking you to the payment page"),
      {
        tone: "info",
        detail: `${moneyWithCode(session.amount)} · ${tx("portal.checkout.attempt", "attempt")} ${shortId(session.attempt_id)}`,
      },
    );
    window.location.assign(target);
  });
}

/** Plain copy for each documented checkout failure. Branch on the code, never the detail. */
function reportCheckoutError(ctx: Ctx, err: unknown): void {
  if (!(err instanceof ApiError)) {
    ctx.toast(tx("portal.checkout.failed_title", "The payment could not be started"), {
      tone: "danger",
      detail: tx("portal.checkout.failed_detail", "The server could not be reached. Nothing has been charged."),
    });
    return;
  }
  switch (err.code) {
    case "not_payable":
      ctx.toast(tx("portal.checkout.not_payable_title", "This invoice cannot be paid right now"), {
        tone: "danger",
        detail: tx(
          "portal.checkout.not_payable_detail",
          "It may be a draft, already void, or have nothing left to pay. Reload the page to see its current state.",
        ),
      });
      return;
    case "already_settled":
      ctx.toast(tx("portal.checkout.settled_title", "This invoice is already paid"), {
        tone: "success",
        detail: tx("portal.checkout.settled_detail", "There is nothing left to pay on it. Reload the page to see the payment."),
      });
      return;
    case "gateway_unavailable":
      ctx.toast(tx("portal.checkout.gateway_title", "The payment gateway is not available"), {
        tone: "danger",
        detail: tx("portal.checkout.gateway_detail", "Nothing has been charged. Try again in a moment."),
      });
      return;
    case "not_found":
      ctx.toast(tx("portal.checkout.not_found_title", "That invoice is not available"), {
        tone: "danger",
        detail: tx("portal.checkout.not_found_detail", "It may belong to another account, or no longer exist."),
      });
      return;
    case "portal_unavailable":
      ctx.toast(tx("portal.checkout.portal_title", "The customer portal is not available"), {
        tone: "danger",
        detail: tx("portal.checkout.portal_detail", "This deployment has no portal configured. Contact support to pay an invoice."),
      });
      return;
    case "rate_limited":
      ctx.toast(tx("portal.checkout.rate_title", "Too many attempts"), {
        tone: "danger",
        detail: tx("portal.checkout.rate_detail", "Wait a minute and try again."),
      });
      return;
    default:
      ctx.toast(err.detail || tx("portal.checkout.failed_title", "The payment could not be started"), {
        tone: "danger",
        detail: err.code,
      });
  }
}

// -------------------------------------------------------------------- page --

export default async function portalPage(ctx: Ctx): Promise<Node> {
  return h(
    "div",
    { class: "page" },
    resource(ctx, (signal) => loadOverview(ctx, signal), (data) => renderOverview(ctx, data)),
  );
}

async function loadOverview(ctx: Ctx, signal: AbortSignal): Promise<PortalOverview> {
  try {
    return await ctx.api.get<PortalOverview>("/api/v1/portal/overview", { cache: false, signal });
  } catch (err) {
    throw portalLoadError(err);
  }
}

/** A 403/404 here means "this login has no billing customer", which is a state, not a crash. */
export function portalLoadError(err: unknown): unknown {
  if (err instanceof ApiError && (err.status === 403 || err.status === 404)) {
    return new Error(
      tx(
        "portal.error.no_customer",
        "This account is not linked to a billing customer, so there is nothing to show here. If you expect invoices or services, contact support.",
      ),
    );
  }
  return err;
}

function renderOverview(ctx: Ctx, data: PortalOverview): Node {
  const outstanding = data.balance?.outstanding ?? [];
  const credit = data.balance?.credit ?? [];
  const open = data.open_invoices ?? [];
  const recent = data.recent_invoices ?? [];
  const services = data.services ?? [];

  // The overview lists only payable invoices, but sort anyway: "the oldest one" is the one
  // that has been waiting longest, and that is the one the hero button pays.
  const oldest = [...open].filter((inv) => inv.payable).sort(oldestFirst)[0];

  const hero = h(
    "section",
    { class: "portal-hero" },
    h(
      "div",
      null,
      h("h1", null, `${greeting()}, ${firstName(data.customer?.name || data.customer?.email || "")}.`),
      h(
        "p",
        null,
        open.length === 0
          ? tx("portal.hero.nothing_open", "Nothing is outstanding. Your services and recent invoices are below.")
          : open.length === 1
            ? tx(
                "portal.hero.one_open",
                "One invoice is waiting for payment. Paying takes a minute and returns you here.",
              )
            : tx(
                "portal.hero.many_open",
                "{{count}} invoices are waiting for payment. Paying takes a minute and returns you here.",
                { count: open.length },
              ),
      ),
      h(
        "div",
        { class: "row-wrap" },
        oldest ? payButton(ctx, oldest, { size: "md" }) : null,
        btn(tx("portal.hero.all_invoices", "All invoices"), { href: "/portal/invoices", icon: "invoice", size: "md" }),
        btn(tx("portal.hero.your_services", "Your services"), { href: "/portal/services", icon: "server", size: "md" }),
      ),
    ),
    h(
      "div",
      { class: "portal-balance" },
      h(
        "span",
        { class: "portal-balance-label" },
        outstanding.length > 1
          ? tx("portal.balance.per_currency", "Outstanding, per currency")
          : tx("portal.balance.outstanding", "Outstanding"),
      ),
      outstanding.length === 0
        ? h("span", { class: "portal-balance-none" }, tx("portal.balance.nothing_due", "Nothing due"))
        // One line per currency: summing two currencies would be meaningless without a rate.
        : h("div", { class: "portal-balance-amounts" }, ...outstanding.map((amount) => moneyEl(amount, { size: "xl" }))),
      ...credit.map((amount) =>
        h("span", { class: "portal-balance-sub" }, `${tx("portal.balance.credit", "Credit")} `, moneyEl(amount)),
      ),
    ),
  );

  const openCard = card({
    title: tx("portal.open.title", "Open invoices"),
    sub:
      open.length > 0
        ? open.length === 1
          ? tx("portal.open.sub_one", "One invoice awaiting payment")
          : tx("portal.open.sub_many", "{{count}} invoices awaiting payment", { count: open.length })
        : undefined,
    actions: h(
      "a",
      { class: "linkish", href: "/portal/invoices" },
      tx("portal.hero.all_invoices", "All invoices"),
      icon("chevronRight", 12),
    ),
    flush: true,
    body:
      open.length === 0
        ? emptyState({
            title: tx("portal.open.empty_title", "Nothing to pay"),
            body: tx(
              "portal.open.empty_body",
              "Every invoice sent to you is settled. New ones appear here when they are issued.",
            ),
            icon: "checkCircle",
          })
        : h("div", { class: "portal-list" }, ...open.map((inv) => openInvoiceRow(ctx, inv))),
  });

  const servicesCard = card({
    title: tx("portal.services.title", "Your services"),
    sub:
      services.length > 0
        ? services.length === 1
          ? tx("portal.services.sub_one", "One service")
          : tx("portal.services.sub_many", "{{count}} services", { count: services.length })
        : undefined,
    actions: h(
      "a",
      { class: "linkish", href: "/portal/services" },
      tx("portal.services.all", "All services"),
      icon("chevronRight", 12),
    ),
    flush: true,
    body:
      services.length === 0
        ? emptyState({
            title: tx("portal.services.empty_title", "No services yet"),
            body: tx("portal.services.empty_body", "When you buy something, it appears here with its renewal date."),
            icon: "server",
          })
        : h("div", null, ...services.map((svc) => serviceRow(svc))),
  });

  const recentCard = card({
    title: tx("portal.recent.title", "Recent invoices"),
    sub: recent.length > 0 ? tx("portal.recent.sub", "Newest first") : undefined,
    actions: h("a", { class: "linkish", href: "/portal/invoices" }, tx("portal.recent.all", "See all"), icon("chevronRight", 12)),
    flush: true,
    body:
      recent.length === 0
        ? emptyState({
            title: tx("portal.recent.empty_title", "No invoices yet"),
            body: tx(
              "portal.recent.empty_body",
              "An invoice will appear here as soon as one is issued to your account.",
            ),
            icon: "invoice",
          })
        : h("div", { class: "portal-list" }, ...recent.map((inv) => recentInvoiceRow(inv))),
  });

  return h("div", { class: "stack" }, hero, openCard, servicesCard, recentCard);
}

function openInvoiceRow(ctx: Ctx, inv: PortalInvoice): HTMLElement {
  const href = `/portal/invoices/${encodeURIComponent(inv.id)}`;
  return h(
    "div",
    { class: "portal-row" },
    h(
      "div",
      { class: "portal-row-main" },
      h(
        "div",
        { class: "portal-row-title" },
        h("a", { class: "row-title", href }, inv.number || tx("portal.invoice.fallback", "Invoice")),
        inv.overdue ? badge(tx("portal.invoice.overdue", "Overdue"), "warning", { dot: true }) : null,
      ),
      h(
        "div",
        { class: "portal-row-meta" },
        h("span", null, tx("portal.row.issued", "Issued {{date}}", { date: dateOnly(inv.issued_at) })),
        inv.due_at ? h("span", { class: "portal-row-dot" }, "·") : null,
        inv.due_at
          ? h("span", null, tx("portal.row.due", "due {{date}}", { date: dateOnly(inv.due_at) }))
          : null,
      ),
    ),
    h(
      "div",
      { class: "portal-row-side" },
      h(
        "div",
        { class: "portal-row-amount" },
        h("span", { class: "portal-row-label" }, tx("portal.balance.outstanding", "Outstanding")),
        moneyEl(inv.due),
      ),
      inv.payable ? payButton(ctx, inv) : null,
    ),
  );
}

function recentInvoiceRow(inv: PortalInvoice): HTMLElement {
  const href = `/portal/invoices/${encodeURIComponent(inv.id)}`;
  return h(
    "div",
    { class: "portal-row" },
    h(
      "div",
      { class: "portal-row-main" },
      h("div", { class: "portal-row-title" }, h("a", { class: "row-title", href }, inv.number || tx("portal.invoice.fallback", "Invoice"))),
      h(
        "div",
        { class: "portal-row-meta" },
        h(
          "span",
          null,
          inv.issued_at
            ? dateOnly(inv.issued_at)
            : tx("portal.invoice.not_issued", "Not issued"),
        ),
      ),
    ),
    h(
      "div",
      { class: "portal-row-side" },
      statusBadge(inv.overdue ? "overdue" : inv.status),
      moneyEl(inv.total),
    ),
  );
}

function serviceRow(svc: PortalService): HTMLElement {
  const href = `/portal/services/${encodeURIComponent(svc.id)}`;
  const hint = svc.actions.includes("cancel")
    ? tx("portal.service.cancellable", "Cancellation available")
    : svc.actions.includes("pay_to_restore")
      ? tx("portal.service.suspended", "Suspended — pay to restore")
      : null;
  return h(
    "div",
    { class: "portal-service" },
    h(
      "div",
      { class: "portal-service-main" },
      h(
        "div",
        { class: "portal-service-name" },
        h("a", { href }, svc.name || tx("portal.service.fallback", "Service")),
        statusBadge(svc.status),
      ),
      h(
        "div",
        { class: "portal-service-meta" },
        svc.next_bill_at
          ? tx("portal.service.next_bill", "Next bill {{date}}", { date: dateOnly(svc.next_bill_at) })
          : tx("portal.service.no_renewal", "No renewal scheduled"),
        hint ? " · " : null,
        hint ? h("span", null, hint) : null,
      ),
    ),
    btn(tx("portal.service.manage", "Manage"), { href, size: "sm", variant: "ghost", iconAfter: "chevronRight" }),
  );
}

// ----------------------------------------------------------------- helpers --

function oldestFirst(a: PortalInvoice, b: PortalInvoice): number {
  return issuedAt(a) - issuedAt(b);
}

function issuedAt(inv: PortalInvoice): number {
  const raw = inv.issued_at ?? inv.due_at;
  if (!raw) return Number.MAX_SAFE_INTEGER;
  const parsed = Date.parse(raw);
  return Number.isNaN(parsed) ? Number.MAX_SAFE_INTEGER : parsed;
}

function greeting(now = new Date()): string {
  const hour = now.getHours();
  if (hour < 5) return tx("portal.greeting.late", "Working late");
  if (hour < 12) return tx("portal.greeting.morning", "Good morning");
  if (hour < 18) return tx("portal.greeting.afternoon", "Good afternoon");
  return tx("portal.greeting.evening", "Good evening");
}

function firstName(name: string): string {
  const trimmed = name.trim();
  if (!trimmed) return tx("portal.greeting.there", "there");
  const local = trimmed.includes("@") ? (trimmed.split("@")[0] ?? trimmed) : trimmed;
  return (local.split(/\s+/)[0] ?? local) || tx("portal.greeting.there", "there");
}
