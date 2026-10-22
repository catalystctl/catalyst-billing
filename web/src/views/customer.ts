// The customer detail screen.
//
// Four questions, four tabs, in the order support asks them: what does this customer owe and
// what do they own (Overview), what have we billed them (Invoices), what address goes on the
// next document (Billing details), and can they get in at all (Activity).
//
// Two things on this screen are deliberate and should stay:
//
//  - Money is a list, never a total. `balance.outstanding` and `balance.credit` arrive one
//    entry per currency, and each is rendered on its own line. A customer can owe USD and
//    hold EUR credit; adding them needs an exchange rate this product does not have.
//  - `has_login: false` is not an error, it is the most common support question. The banner
//    explains it, and nothing here pretends to be able to fix it: portal accounts are created
//    by staff through the account flow, not by a button on a read screen.

import { h } from "../lib/dom";
import type { Amount } from "../lib/format";
import { dateTime, humanStatus, plural, relative, shortId, truncate } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  avatar,
  badge,
  banner,
  btn,
  card,
  emptyState,
  moneyEl,
  resource,
  statusBadge,
  tabs,
} from "../ui/kit";
import { profileForm } from "./billing-profile";

export const meta = { title: "Customer" };

// ------------------------------------------------------------------- shapes --

interface CustomerSummary {
  id: string;
  email: string;
  name: string;
  created_at: string;
  open_invoices: number;
  services: number;
  has_login: boolean;
}

interface Account {
  user_id: string;
  email: string;
  role: string;
  last_seen_at: string | null;
}

interface Subscription {
  id: string;
  status: string;
  interval: string;
  product: string;
  next_billing_at: string;
  current_period_start: string;
  current_period_end: string;
  cancel_at_period_end: boolean;
}

interface Service {
  id: string;
  customer: { id: string; email: string };
  product: { id: string; name: string };
  status: string;
  external_id: string;
  generation: number;
  next_bill_at: string | null;
  created_at: string;
  open_operations: number;
  last_error: string | null;
}

interface Invoice {
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
  customer?: { id: string; email: string };
}

interface Hold {
  id: string;
  service_id: string;
  reason: string;
  expires_at: string | null;
  created_by: string;
  created_at: string;
}

interface AuthEvent {
  event: string;
  detail: string;
  ip: string;
  created_at: string;
}

interface CustomerDetail {
  customer: CustomerSummary;
  accounts: Account[] | null;
  subscriptions: Subscription[] | null;
  services: Service[] | null;
  invoices: Invoice[] | null;
  balance: { outstanding: Amount[] | null; credit: Amount[] | null };
  holds: Hold[] | null;
  recent_auth_events: AuthEvent[] | null;
}

// --------------------------------------------------------------------- tabs --

const TAB_ITEMS = [
  { key: "overview", label: "Overview" },
  { key: "invoices", label: "Invoices" },
  { key: "billing", label: "Billing details" },
  { key: "activity", label: "Activity" },
] as const;

type TabKey = (typeof TAB_ITEMS)[number]["key"];

function tabFrom(value: string | null): TabKey {
  const match = TAB_ITEMS.find((item) => item.key === value);
  return match?.key ?? "overview";
}

// --------------------------------------------------------------------- page --

export default async function customerPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("customer:read:all")) {
    ctx.navigate("/", { replace: true });
    return h("div");
  }

  const id = ctx.route.params.id ?? "";
  let activeTab = tabFrom(ctx.route.query.get("tab"));

  const shell = h("div");

  const render = (detail: CustomerDetail): Node => {
    const customer = detail.customer;
    const displayName = customer.name || customer.email;
    ctx.setTitle(displayName);
    ctx.setCrumbs([{ label: "Customers", href: "/customers" }, { label: displayName }]);

    const tabHost = h("div", { class: "stack" });

    const paint = () => {
      tabHost.replaceChildren(
        tabs(
          TAB_ITEMS.map((item) => ({
            key: item.key,
            label: item.label,
            count:
              item.key === "invoices"
                ? (detail.invoices ?? []).length
                : item.key === "activity"
                  ? (detail.recent_auth_events ?? []).length
                  : undefined,
          })),
          activeTab,
          (key) => {
            activeTab = tabFrom(key);
            ctx.replaceQuery({ tab: activeTab });
            paint();
          },
        ),
        tabBody(ctx, detail, id, activeTab),
      );
    };

    const loginBadge = customer.has_login
      ? badge("Portal access", "success", { dot: true })
      : badge("No login", "neutral", { dot: true });

    const header = h(
      "header",
      { class: "detail-head" },
      h(
        "div",
        { class: "detail-ident" },
        avatar(displayName, { size: "lg" }),
        h(
          "div",
          null,
          h("div", { class: "detail-title" }, displayName, loginBadge),
          h(
            "div",
            { class: "detail-sub" },
            icon("mail", 13),
            h("a", { class: "linkish", href: `mailto:${customer.email}` }, customer.email),
            h("span", { class: "dot-sep" }, "·"),
            h("span", null, `Customer since ${relative(customer.created_at)}`),
          ),
          h(
            "div",
            { class: "detail-sub" },
            h("span", null, plural(customer.open_invoices, "open invoice")),
            h("span", { class: "dot-sep" }, "·"),
            h("span", null, plural(customer.services, "service")),
            h("span", { class: "dot-sep" }, "·"),
            h("span", { class: "mono faint", title: customer.id }, shortId(customer.id, 8)),
          ),
        ),
      ),
      h("div", { class: "spacer" }),
      h(
        "div",
        { class: "page-actions" },
        ctx.can("invoice:write:all")
          ? btn("New order", { href: `/orders/new?customer=${encodeURIComponent(customer.id)}`, icon: "box", size: "sm" })
          : null,
        ctx.can("invoice:write:all")
          ? btn("New invoice", { href: "/invoices/new", icon: "plus", size: "sm" })
          : null,
        // What the customer sees, read-only. Support answers most questions by looking at this
        // page rather than by asking the customer to read their own screen aloud.
        ctx.can("customer:read:all")
          ? btn("View their portal", {
              href: `/customers/${customer.id}/portal`,
              icon: "eye",
              size: "sm",
            })
          : null,
        btn("Refresh", { icon: "refresh", size: "sm", onclick: () => draw() }),
      ),
    );

    // A customer without a `customer_accounts` row cannot sign in. Say what that means and
    // who fixes it; do not offer a button that cannot exist.
    const noLogin = customer.has_login
      ? null
      : banner(
          "info",
          "This customer cannot sign in to the portal",
          "There is no portal account for this email, so invoices, services and billing " +
            "details are visible only to staff. A portal account is created by staff — never " +
            "by the customer — and this screen does not create one.",
        );

    paint();

    return h(
      "div",
      { class: "page" },
      h(
        "div",
        { class: "row" },
        btn("All customers", { href: "/customers", icon: "arrowLeft", size: "sm", variant: "ghost" }),
      ),
      header,
      noLogin,
      tabHost,
    );
  };

  const draw = () => {
    shell.replaceChildren(
      resource<CustomerDetail>(
        ctx,
        (signal) =>
          ctx.api.get<CustomerDetail>(`/api/v1/staff/customers/${id}`, { cache: false, signal }),
        render,
      ),
    );
  };

  draw();
  return shell;
}

// --------------------------------------------------------------------- tabs --

function tabBody(ctx: Ctx, detail: CustomerDetail, id: string, tab: TabKey): Node {
  switch (tab) {
    case "invoices":
      return invoicesTab(detail);
    case "billing":
      return billingTab(ctx, id);
    case "activity":
      return activityTab(detail);
    default:
      return overviewTab(detail);
  }
}

// ----------------------------------------------------------------- overview --

function overviewTab(detail: CustomerDetail): Node {
  const accounts = detail.accounts ?? [];
  const subscriptions = detail.subscriptions ?? [];
  const services = detail.services ?? [];
  const holds = detail.holds ?? [];

  const balanceCard = card({
    title: "Balance",
    sub: "One line per currency — never added together.",
    body: h(
      "div",
      { class: "cust-balance" },
      balanceGroup("Outstanding", detail.balance.outstanding ?? [], "Nothing outstanding"),
      balanceGroup("Credit", detail.balance.credit ?? [], "No credit on account"),
    ),
  });

  const servicesCard = card({
    title: "Services",
    sub: services.length > 0 ? `${plural(services.length, "service")} provisioned` : undefined,
    flush: true,
    body:
      services.length === 0
        ? emptyState({
            title: "No services",
            body: "Nothing is provisioned for this customer. A service is created by checkout or by the provisioning pipeline.",
            icon: "server",
          })
        : h("div", null, ...services.map(serviceRow)),
  });

  const subscriptionsCard = card({
    title: "Subscriptions",
    sub: subscriptions.length > 0 ? `${plural(subscriptions.length, "subscription")}` : undefined,
    flush: true,
    body:
      subscriptions.length === 0
        ? emptyState({
            title: "No subscriptions",
            body: "This customer is billed by invoice only, so there is no renewal schedule to show.",
            icon: "refresh",
          })
        : h("div", null, ...subscriptions.map(subscriptionRow)),
  });

  const accountsCard = card({
    title: "Portal accounts",
    sub: accounts.length > 0 ? `${plural(accounts.length, "login")} linked` : undefined,
    flush: true,
    body:
      accounts.length === 0
        ? emptyState({
            title: "No login",
            body: "No account is linked to this customer, so the portal is not available to them.",
            icon: "lock",
          })
        : h("div", null, ...accounts.map(accountRow)),
  });

  const holdsCard = card({
    title: "Holds",
    sub: holds.length > 0 ? `${plural(holds.length, "hold")} in place` : undefined,
    flush: true,
    body:
      holds.length === 0
        ? emptyState({
            title: "No holds",
            body: "Automation may suspend or terminate this customer's services as the rules say. A hold is the manual stop that overrides it.",
            icon: "pauseCircle",
          })
        : h("div", null, ...holds.map(holdRow)),
  });

  return h(
    "div",
    { class: "split" },
    h("div", { class: "stack" }, servicesCard, subscriptionsCard),
    h("div", { class: "stack" }, balanceCard, accountsCard, holdsCard),
  );
}

/** A labelled group of amounts, one line per currency. */
function balanceGroup(label: string, amounts: Amount[], emptyText: string): HTMLElement {
  return h(
    "div",
    { class: "cust-balance-group" },
    h("div", { class: "cust-balance-label" }, label),
    amounts.length === 0
      ? h("div", { class: "faint" }, emptyText)
      : h("div", { class: "cust-amounts cust-amounts--start" }, ...amounts.map((amount) => moneyEl(amount))),
  );
}

function serviceRow(svc: Service): HTMLElement {
  return h(
    "a",
    { class: "card-link", href: `/services/${svc.id}` },
    h(
      "div",
      { class: "cell-main cust-grow" },
      h("div", { class: "primary" }, svc.product?.name || "Service"),
      h(
        "div",
        { class: "secondary" },
        svc.external_id ? `External ${svc.external_id}` : "No external id",
        ` · generation ${svc.generation}`,
      ),
      svc.last_error ? h("div", { class: "secondary cust-error" }, truncate(svc.last_error, 90)) : null,
    ),
    svc.open_operations > 0 ? badge(plural(svc.open_operations, "open op"), "warning", { dot: true }) : null,
    statusBadge(svc.status),
    h(
      "div",
      { class: "cust-row-side" },
      h("span", null, svc.next_bill_at ? `Bills ${relative(svc.next_bill_at)}` : "No next bill"),
    ),
    icon("chevronRight", 13),
  );
}

function subscriptionRow(sub: Subscription): HTMLElement {
  return h(
    "div",
    { class: "cust-row" },
    h(
      "div",
      { class: "cell-main cust-grow" },
      h(
        "div",
        { class: "primary" },
        sub.product || "Subscription",
        sub.cancel_at_period_end ? badge("Cancels at period end", "warning", { dot: true }) : null,
      ),
      h(
        "div",
        { class: "secondary" },
        `Every ${sub.interval} · ${dateTime(sub.current_period_start)} → ${dateTime(sub.current_period_end)}`,
      ),
    ),
    statusBadge(sub.status),
    h("div", { class: "cust-row-side" }, h("span", null, `Renews ${relative(sub.next_billing_at)}`)),
  );
}

function accountRow(account: Account): HTMLElement {
  return h(
    "div",
    { class: "cust-row" },
    h(
      "div",
      { class: "cell-main cust-grow" },
      h("div", { class: "primary" }, account.email),
      h("div", { class: "secondary mono" }, shortId(account.user_id, 12)),
    ),
    badge(humanStatus(account.role), "neutral"),
    h(
      "div",
      { class: "cust-row-side" },
      h(
        "span",
        { class: account.last_seen_at ? undefined : "faint" },
        account.last_seen_at ? `Seen ${relative(account.last_seen_at)}` : "Never signed in",
      ),
    ),
  );
}

function holdRow(hold: Hold): HTMLElement {
  return h(
    "div",
    { class: "cust-row" },
    h(
      "div",
      { class: "cell-main cust-grow" },
      h("div", { class: "primary" }, hold.reason || "Manual hold"),
      h(
        "div",
        { class: "secondary" },
        `Placed by ${hold.created_by || "unknown"} ${relative(hold.created_at)}`,
      ),
    ),
    h(
      "div",
      { class: "cust-row-side" },
      h("a", { class: "linkish", href: `/services/${hold.service_id}` }, "Service", icon("chevronRight", 11)),
      hold.expires_at
        ? h("span", { class: "faint" }, `Expires ${relative(hold.expires_at)}`)
        : h("span", { class: "cust-permanent" }, "Permanent"),
    ),
  );
}

// ----------------------------------------------------------------- invoices --

function invoicesTab(detail: CustomerDetail): Node {
  const invoices = detail.invoices ?? [];
  return card({
    title: "Recent invoices",
    sub: "The most recent documents on this customer's ledger.",
    actions: h("a", { class: "linkish", href: "/invoices" }, "All invoices", icon("chevronRight", 12)),
    flush: true,
    body:
      invoices.length === 0
        ? emptyState({
            title: "No invoices",
            body: "Nothing has been issued to this customer yet. A renewal or a manual invoice will appear here.",
            icon: "invoice",
          })
        : h("div", null, ...invoices.map(invoiceRow)),
  });
}

function invoiceRow(inv: Invoice): HTMLElement {
  return h(
    "a",
    { class: "card-link", href: `/invoices/${inv.id}` },
    h(
      "div",
      { class: "cell-main cust-grow" },
      h("div", { class: "primary" }, inv.number || "Draft"),
      h(
        "div",
        { class: "secondary" },
        inv.issued_at ? `Issued ${relative(inv.issued_at)}` : "Not issued",
        inv.due_at ? ` · due ${relative(inv.due_at)}` : "",
      ),
    ),
    statusBadge(inv.overdue ? "overdue" : inv.status),
    h(
      "div",
      { class: "cust-row-side" },
      moneyEl(inv.total),
      inv.due.minor > 0
        ? h("span", { class: "faint cust-due" }, "Due ", moneyEl(inv.due))
        : h("span", { class: "faint" }, "Settled"),
    ),
    icon("chevronRight", 13),
  );
}

// ----------------------------------------------------------------- activity --

function activityTab(detail: CustomerDetail): Node {
  const events = detail.recent_auth_events ?? [];
  return card({
    title: "Authentication events",
    sub: "The most recent sign-in and password events on this customer's accounts.",
    body:
      events.length === 0
        ? emptyState({
            title: "No authentication events",
            body: "No account has signed in, so there is nothing to show. A customer without a portal account never appears here.",
            icon: "key",
          })
        : h(
            "div",
            { class: "timeline" },
            ...events.map((event) =>
              h(
                "div",
                { class: "tl-item" },
                h(
                  "div",
                  { class: `tl-dot tl-dot--${authTone(event.event)}` },
                  icon(authIcon(event.event), 9),
                ),
                h(
                  "div",
                  { class: "tl-body" },
                  h(
                    "div",
                    { class: "tl-title" },
                    humanStatus(event.event),
                    h("span", { class: "tl-time" }, relative(event.created_at)),
                  ),
                  h(
                    "div",
                    { class: "tl-detail" },
                    [event.detail, event.ip ? `IP ${event.ip}` : ""].filter(Boolean).join(" · ") || "No detail recorded",
                  ),
                ),
              ),
            ),
          ),
  });
}

type DotTone = "success" | "danger" | "warning" | "accent";

function authTone(event: string): DotTone {
  const name = event.toLowerCase();
  if (/(fail|lock|denied|blocked|invalid)/.test(name)) return "danger";
  if (/(success|login|sign_in|signed_in|verified|reset)/.test(name)) return "success";
  if (/(attempt|challenge|throttl)/.test(name)) return "warning";
  return "accent";
}

function authIcon(event: string): "checkCircle" | "alertCircle" | "clock" | "key" {
  const tone = authTone(event);
  if (tone === "danger") return "alertCircle";
  if (tone === "success") return "checkCircle";
  if (tone === "warning") return "clock";
  return "key";
}

// ------------------------------------------------------------------ billing --

function billingTab(ctx: Ctx, id: string): Node {
  const writable = ctx.can("customer:write:all");
  return h(
    "div",
    { class: "stack" },
    profileForm(ctx, {
      path: `/api/v1/staff/customers/${id}/billing-profile`,
      readOnly: !writable,
    }),
    writable
      ? null
      : h(
          "p",
          { class: "faint" },
          "Your role can read billing details but not change them: saving requires customer:write:all.",
        ),
  );
}
