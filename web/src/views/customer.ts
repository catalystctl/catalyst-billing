// The customer detail screen.
//
// Four questions, four tabs, in the order support asks them: what does this customer owe and
// what do they own (Overview), what have we billed them (Invoices), what address goes on the
// next document (Billing details), and can they get in at all (Activity).
//
// Three things on this screen are deliberate and should stay:
//
//  - Money is a list, never a total. `balance.outstanding` and `balance.credit` arrive one
//    entry per currency, and each is rendered on its own line. A customer can owe USD and
//    hold EUR credit; adding them needs an exchange rate this product does not have.
//  - `has_login: false` is not an error, it is the most common support question. The banner
//    explains it, and this screen still does not *create* a portal account: creating one is the
//    customer-create flow's job. What the Overview can do is everything short of that — set the
//    password of an existing login, take away a second factor the customer can no longer use,
//    and close the record — because those are the three things a support call ends with.
//  - The security actions are driven by `/account-state`, not by guesses. `has_mfa`, the
//    recovery-code count and the blockers that stop a close all come from the server, so the
//    buttons offered are the buttons that will work.

import { ApiError } from "../lib/api";
import {
  agreementStatusLabel,
  billingAgreements,
  creditGrantRows,
  customerCreditsQuery,
  customerTicketsQuery,
  expiryLabel,
  ticketRows,
  visibleTabs,
  type BillingAgreementsPayload,
  type BillingAgreementsView,
  type CreditGrantRow,
  type CreditGrantsPayload,
  type CustomerTabKey,
  type CustomerTicketRow,
  type CustomerTicketsPayload,
} from "../lib/customer-detail";
import { h, swap } from "../lib/dom";
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
  errorState,
  field,
  input,
  moneyEl,
  resource,
  skeletonTable,
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

/** One reason the record cannot be closed. `kind` is `live_service` or `unpaid_invoice`; `detail`
 *  is the server's own sentence naming the thing, and it is shown verbatim because a rewritten
 *  sentence is a second, unaudited description of the same fact. */
interface CloseBlocker {
  kind: string;
  id: string;
  label: string;
  status: string;
  detail: string;
}

/** The second-factor state of one login, from `/account-state`. */
interface LoginSecurity {
  user_id: string;
  email: string;
  status: string;
  has_mfa: boolean;
  enrolled_at: string | null;
  recovery_codes_remaining: number;
  enrolment_required: boolean;
  reset_at: string | null;
  reset_by: string | null;
}

/** What `GET /api/v1/staff/customers/{id}/account-state` reports. */
interface AccountState {
  customer_id: string;
  closed: boolean;
  closed_at: string | null;
  closed_kind: string | null;
  closed_by: string | null;
  closed_reason: string;
  can_close: boolean;
  blockers: CloseBlocker[] | null;
  logins: LoginSecurity[] | null;
  confirm_phrase: string;
  delete_statement: string;
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

// The tab list lives in `lib/customer-detail` so the permission gate and the query-string
// fallback are unit-testable. A tab the caller cannot read is not rendered at all: the endpoint
// behind it would answer 403, and a tab whose only content is a refusal is a worse lie than an
// absent one.

function tabItemsFor(ctx: Ctx): ReturnType<typeof visibleTabs> {
  return visibleTabs((permission) => ctx.can(permission));
}

function tabFrom(value: string | null, ctx: Ctx): CustomerTabKey {
  const tabs = tabItemsFor(ctx);
  const match = tabs.find((item) => item.key === value);
  return match?.key ?? "overview";
}

// ------------------------------------------------------------- impersonation --

/** The facts the start flow needs about the account it is about to wear. */
export interface ImpersonationTarget {
  /** The *user* id — the login, not the customer record. The impersonation endpoint issues a
   *  session for a user, so passing the customer id is a 404 that looks like a missing feature. */
  userId: string;
  email: string;
  name?: string;
}

/**
 * Opens a session as the customer, from the customer record.
 *
 * The confirmation is not ceremony. Everything the operator does after it is recorded against the
 * customer and attributed to the operator, and the session cannot move money or change
 * credentials — which is precisely why the dialog states those limits *before* the session exists
 * rather than leaving them to be discovered by hitting a 403 halfway through a support call.
 *
 * After the server issues the session, the identity is re-read (`ctx.refreshSession`) rather than
 * assumed: `impersonating` is a fact about the session, and the shell renders the banner from it.
 * Then the operator lands on the portal — the screen they asked to see — instead of staying on a
 * staff record the borrowed identity cannot read.
 */
export async function startImpersonation(ctx: Ctx, target: ImpersonationTarget): Promise<void> {
  const who = target.name || target.email;
  const confirmed = await ctx.confirm({
    title: `Sign in as ${who}?`,
    body:
      "You will get a session that is theirs, and every action you take in it is recorded against " +
      "their account and attributed to you by name. Moving money, changing credentials and " +
      "closing the account are refused by the server while the session lasts. A banner on every " +
      "screen shows whose account you are in, with the way back to yours.",
    confirmLabel: "Sign in as them",
    cancelLabel: "Cancel",
    note: "The audit log records both accounts, at the start and at the end.",
  });
  if (!confirmed) return;

  try {
    await ctx.api.post(`/api/v1/staff/users/${encodeURIComponent(target.userId)}/impersonate`, {});
  } catch (err) {
    if (err instanceof ApiError) {
      const message =
        err.code === "forbidden"
          ? "That account cannot be impersonated — staff accounts never can."
          : err.code === "account_disabled"
            ? "That account is disabled, so there is no session to open."
            : err.code === "session_required"
              ? "Impersonation needs a signed-in browser session, not an API key."
              : err.detail || `The session was not opened (${err.code}).`;
      ctx.toast("Could not sign in as the customer", { tone: "danger", detail: message });
      return;
    }
    ctx.toast("Could not sign in as the customer", {
      tone: "danger",
      detail: "The server could not be reached, so nothing changed.",
    });
    return;
  }

  // The cookie is already the impersonated session. Re-reading the identity is what tells the
  // shell to raise its banner, and it is also the only way to be sure the session the server
  // issued is the one this browser is now carrying.
  await ctx.refreshSession();
  ctx.toast(`Signed in as ${target.email}`, {
    tone: "success",
    detail: "Stop from the banner to return to your own account.",
  });
  ctx.navigate("/portal");
}

// --------------------------------------------------------------------- page --

export default async function customerPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("customer:read:all")) {
    ctx.navigate("/dashboard", { replace: true });
    return h("div");
  }

  const id = ctx.route.params.id ?? "";
  let activeTab = tabFrom(ctx.route.query.get("tab"), ctx);

  const shell = h("div");

  const render = (detail: CustomerDetail): Node => {
    const customer = detail.customer;
    const displayName = customer.name || customer.email;
    // The login to wear, if there is one. The impersonation endpoint is keyed by user id, and a
    // customer record is not a user: the two ids are different rows, which is exactly the kind of
    // difference that turns "the feature does not work" into a 404 nobody reads.
    const login = (detail.accounts ?? [])[0];
    ctx.setTitle(displayName);
    ctx.setCrumbs([{ label: "Customers", href: "/customers" }, { label: displayName }]);

    const tabHost = h("div", { class: "stack" });

    const paint = () => {
      tabHost.replaceChildren(
        tabs(
          tabItemsFor(ctx).map((item) => ({
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
            activeTab = tabFrom(key, ctx);
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
        // And when looking is not enough: a session that *is* theirs, for a bounded time, with a
        // banner that says so and a server that refuses the actions that would turn support into
        // a takeover. Offered only where it can work — the session is issued for a *user*, so the
        // account linked to this customer is what the button names, and a customer without one has
        // nothing to impersonate. Not while already wearing somebody's identity either: stacking
        // one impersonation inside another is a support visit nobody can audit.
        ctx.can("staff:manage") && login && !ctx.session.impersonating
          ? btn("Sign in as this customer", {
              size: "sm",
              variant: "ghost",
              icon: "user",
              title: `Open a session as ${login.email}`,
              onclick: () =>
                void startImpersonation(ctx, { userId: login.user_id, email: login.email, name: customer.name }),
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

function tabBody(ctx: Ctx, detail: CustomerDetail, id: string, tab: CustomerTabKey): Node {
  switch (tab) {
    case "invoices":
      return invoicesTab(detail);
    case "tickets":
      return ticketsTab(ctx, id);
    case "credits":
      return creditsTab(ctx, id);
    case "agreements":
      return agreementsTab(ctx, id);
    case "billing":
      return billingTab(ctx, id);
    case "activity":
      return activityTab(detail);
    default:
      return overviewTab(ctx, detail, id);
  }
}

// ----------------------------------------------------------------- overview --

function overviewTab(ctx: Ctx, detail: CustomerDetail, customerId: string): Node {
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

  const accountsCard = portalAccessCard(ctx, customerId, detail);

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

// ------------------------------------------------------------- portal access --

/** The security of one login: its email, its second factor, and the two actions a support call
 *  ends with. */
function accountRow(
  ctx: Ctx,
  account: Account,
  security: LoginSecurity | undefined,
  editable: boolean,
  reload: () => void,
): HTMLElement {
  const mfa = security
    ? security.has_mfa
      ? badge("Two-factor on", "success", { dot: true })
      : security.enrolment_required
        ? badge("Must enrol", "warning", { dot: true })
        : badge("No two-factor", "neutral", { dot: true })
    : badge("Unknown", "neutral");

  // The demand an operator's reset leaves behind: the account owes the panel a factor, and a
  // support agent reading "No two-factor" alone would not know that.
  const resetNote =
    security && security.reset_at
      ? h(
          "div",
          { class: "secondary" },
          `Second factor reset ${relative(security.reset_at)}${security.reset_by ? ` by ${shortId(security.reset_by, 8)}` : ""}`,
        )
      : null;

  // A single vertical block rather than the `.cust-row` three-column rhythm. This card sits in
  // the narrow aside column, where a right-aligned side column squeezes the address into three
  // lines and pushes the buttons under it; stacked lines read the same at 330px and at 900px.
  const lastSeen = account.last_seen_at ? `Seen ${relative(account.last_seen_at)}` : "Never signed in";
  const codes =
    security && security.has_mfa
      ? ` · ${plural(security.recovery_codes_remaining, "recovery code")} left`
      : "";

  return h(
    "div",
    { class: "cust-row" },
    h(
      "div",
      { class: "cell-main cust-grow stack-sm" },
      h("div", { class: "row-wrap" }, h("span", { class: "primary" }, account.email), mfa, badge(humanStatus(account.role), "neutral")),
      h("div", { class: "secondary" }, `${lastSeen}${codes}`, h("span", { class: "dot-sep" }, " · "), h("span", { class: "mono" }, shortId(account.user_id, 8))),
      resetNote,
      editable
        ? h(
            "div",
            { class: "row-wrap" },
            btn("Set password", {
              size: "sm",
              icon: "key",
              title: `Set a password for ${account.email}`,
              onclick: () => void setPassword(ctx, account, reload),
            }),
            btn("Reset two-factor", {
              size: "sm",
              variant: "ghost",
              icon: "shield",
              title: `Reset the second factor for ${account.email}`,
              onclick: () => void resetMFA(ctx, account, security, reload),
            }),
          )
        : null,
    ),
  );
}

/** The Overview's portal-access card.
 *
 *  It loads `/account-state` rather than reading `detail.accounts`, because the three facts the
 *  actions depend on — whether a factor is confirmed, how many recovery codes are left, and what
 *  stops the record being closed — are not in the customer payload. One request, and the buttons
 *  offered are the buttons that will work. */
function portalAccessCard(ctx: Ctx, customerId: string, detail: CustomerDetail): HTMLElement {
  const accounts = detail.accounts ?? [];
  const host = h("div", { class: "stack" }, skeletonTable(2, 3));

  const load = () => {
    void (async () => {
      try {
        const state = await ctx.api.get<AccountState>(
          `/api/v1/staff/customers/${customerId}/account-state`,
          { cache: false },
        );
        swap(host, accessBody(ctx, customerId, detail, state, load));
      } catch (err) {
        // The card is one panel on a page that has already loaded. A failed security read must
        // not take the customer's invoices and services down with it.
        swap(host, errorState(err, load));
      }
    })();
  };
  load();

  return card({
    title: "Portal access",
    sub: accounts.length > 0 ? `${plural(accounts.length, "login")} linked` : undefined,
    flush: true,
    body: host,
  });
}

function accessBody(
  ctx: Ctx,
  customerId: string,
  detail: CustomerDetail,
  state: AccountState,
  reload: () => void,
): Node {
  const accounts = detail.accounts ?? [];
  const byUser = new Map<string, LoginSecurity>();
  for (const login of state.logins ?? []) byUser.set(login.user_id, login);
  const blockers = state.blockers ?? [];
  const editable = ctx.can("customer:write:all") && !state.closed;

  const rows =
    accounts.length === 0
      ? emptyState({
          title: "No login",
          body: "No account is linked to this customer, so the portal is not available to them. A portal account is created by the customer-create flow, not from here.",
          icon: "lock",
        })
      : h(
          "div",
          null,
          ...accounts.map((account) => accountRow(ctx, account, byUser.get(account.user_id), editable, reload)),
        );

  return h(
    "div",
    { class: "stack" },
    state.closed
      ? banner(
          "warning",
          "This record is closed",
          `Closed ${state.closed_at ? relative(state.closed_at) : "earlier"}` +
            `${state.closed_kind === "staff" && state.closed_by ? ` by ${state.closed_by}` : state.closed_kind === "self" ? " by the customer" : ""}. ` +
            "The invoices, services and audit trail are kept; the portal login does not work." +
            (state.closed_reason ? ` Reason: ${state.closed_reason}` : ""),
        )
      : null,
    rows,
    !state.closed && blockers.length > 0
      ? banner(
          "info",
          "This record cannot be closed yet",
          "The customer's own close-account request is refused while " +
            blockerSentence(blockers) +
            ". That is deliberate: closing a record never terminates a service or writes off a debt.",
        )
      : null,
    editable
      ? h(
          "div",
          { class: "cust-row" },
          h(
            "div",
            { class: "stack-sm" },
            h("div", { class: "primary" }, "Close this customer record"),
            h("p", { class: "page-sub" }, state.delete_statement),
          ),
          btn("Close record", {
            size: "sm",
            variant: "danger",
            icon: "trash",
            onclick: () => void closeCustomer(ctx, customerId, state, reload),
          }),
        )
      : null,
  );
}

/** The blockers as one sentence, built from the server's own details. */
function blockerSentence(blockers: CloseBlocker[]): string {
  const services = blockers.filter((b) => b.kind === "live_service").map((b) => b.detail);
  const invoices = blockers.filter((b) => b.kind === "unpaid_invoice").map((b) => b.detail);
  const parts: string[] = [];
  if (services.length > 0) parts.push(`${plural(services.length, "live service")} (${services.join("; ")})`);
  if (invoices.length > 0) parts.push(`${plural(invoices.length, "unpaid invoice")} (${invoices.join("; ")})`);
  return parts.length > 0 ? parts.join(" and ") : "records that must be settled first";
}

/** Set a customer's password from their overview.
 *
 *  The same route the staff users screen uses (`POST /staff/users/{id}/password`), because a
 *  customer's credential is the same kind of record as an agent's. Every session the login had is
 *  ended by the handler, so this dialog says so rather than implying a quiet change. */
async function setPassword(ctx: Ctx, account: Account, reload: () => void): Promise<void> {
  const password = input({
    id: "cust-password-new",
    name: "password",
    type: "password",
    autocomplete: "new-password",
    placeholder: "At least 12 characters",
  });
  const confirm = input({
    id: "cust-password-confirm",
    name: "confirm",
    type: "password",
    autocomplete: "new-password",
    placeholder: "Type it again",
  });

  await ctx.formDialog({
    title: `Set a password for ${account.email}`,
    subtitle: "Every session this login has is ended, because whoever held the old password may still be holding one.",
    submitLabel: "Set password",
    body: h(
      "div",
      { class: "stack" },
      field({
        label: "New password",
        control: password,
        required: true,
        hint: "At least 12 characters — the same rule registration applies.",
      }),
      field({ label: "Repeat it", control: confirm, required: true }),
      banner(
        "info",
        "You will know this password",
        "Tell the customer to change it from their security screen once they are in. The audit trail records that you set it.",
      ),
    ),
    onSubmit: async () => {
      if (!password.value) return "A password is required.";
      if (password.value !== confirm.value) return "The two passwords do not match.";
      try {
        const result = await ctx.api.post<{ sessions_revoked?: number }>(
          `/api/v1/staff/users/${encodeURIComponent(account.user_id)}/password`,
          { password: password.value },
        );
        const revoked = typeof result?.sessions_revoked === "number" ? result.sessions_revoked : 0;
        ctx.toast("Password set", {
          tone: "success",
          detail: revoked > 0 ? `${revoked} session(s) ended · ${account.email}` : account.email,
        });
        reload();
        return undefined;
      } catch (err) {
        if (err instanceof ApiError && err.code === "weak_password") {
          return err.detail || "That password is too short.";
        }
        if (err instanceof ApiError) return err.detail || `The password was refused (${err.code}).`;
        return "The server could not be reached, so no password was set.";
      }
    },
  });
}

/** Reset a customer's second factor.
 *
 *  Staff cannot use the self-service disable route: it demands the customer's own password and a
 *  current code, which is precisely what somebody who lost their phone cannot supply. This calls
 *  the staff route, which removes the enrolment and leaves an enrolment demand behind — so the
 *  account is not quietly downgraded to password-only. */
async function resetMFA(
  ctx: Ctx,
  account: Account,
  security: LoginSecurity | undefined,
  reload: () => void,
): Promise<void> {
  if (security && !security.has_mfa) {
    ctx.toast("There is no second factor to reset", {
      tone: "info",
      detail: `${account.email} has no confirmed authenticator.`,
    });
    return;
  }

  const confirmed = await ctx.confirm({
    title: `Reset two-factor for ${account.email}?`,
    body:
      "The authenticator, the recovery codes and any sign-in waiting on a code are removed. The account is asked to set up a new factor at its next sign-in. This is recorded in the audit trail against you, by name.",
    confirmLabel: "Reset two-factor",
    danger: true,
  });
  if (!confirmed) return;

  try {
    const result = await ctx.api.post<{ recovery_codes_removed?: number }>(
      `/api/v1/staff/users/${encodeURIComponent(account.user_id)}/mfa/reset`,
      {},
    );
    const codes = typeof result?.recovery_codes_removed === "number" ? result.recovery_codes_removed : 0;
    ctx.toast("Two-factor reset", {
      tone: "success",
      detail: `${account.email} must enrol a new factor · ${plural(codes, "recovery code")} removed`,
    });
    reload();
  } catch (err) {
    if (err instanceof ApiError && err.code === "mfa_not_enrolled") {
      ctx.toast("There is no second factor to reset", { tone: "info", detail: account.email });
      reload();
      return;
    }
    ctx.toast("The second factor was not reset", {
      tone: "danger",
      detail: err instanceof ApiError ? err.detail || err.code : "The server could not be reached.",
    });
  }
}

/** Close a customer record.
 *
 *  The dialog repeats the server's own statement and blockers instead of inventing wording, and it
 *  asks for the reason the audit row needs. The confirmation phrase comes from the server so the
 *  two cannot drift apart. */
async function closeCustomer(
  ctx: Ctx,
  customerId: string,
  state: AccountState,
  reload: () => void,
): Promise<void> {
  const phrase = input({
    id: "cust-close-phrase",
    name: "phrase",
    placeholder: state.confirm_phrase,
    autocomplete: "off",
  });
  const reason = input({ id: "cust-close-reason", name: "reason", placeholder: "Why is this record being closed?" });
  const blockers = state.blockers ?? [];

  await ctx.formDialog({
    title: "Close this customer record?",
    subtitle: state.delete_statement,
    submitLabel: "Close the record",
    danger: true,
    body: h(
      "div",
      { class: "stack" },
      banner(
        "warning",
        "This is not a delete",
        "The record, its invoices, its services and the audit trail are kept. The portal login stops working and its sessions end. Nothing is cascaded.",
      ),
      blockers.length > 0
        ? banner("danger", "It will be refused", `This record still has ${blockerSentence(blockers)}.`)
        : null,
      field({
        label: `Type “${state.confirm_phrase}” to confirm`,
        control: phrase,
        required: true,
      }),
      field({
        label: "Reason",
        control: reason,
        required: true,
        hint: "Recorded in the audit trail. \"Customer asked us to\" is a reason.",
      }),
    ),
    onSubmit: async () => {
      if (phrase.value.trim().toLowerCase() !== state.confirm_phrase) {
        return `Type “${state.confirm_phrase}” exactly.`;
      }
      if (!reason.value.trim()) return "A reason is required.";
      try {
        await ctx.api.del(`/api/v1/staff/customers/${encodeURIComponent(customerId)}`, {
          reason: reason.value.trim(),
        });
      } catch (err) {
        if (err instanceof ApiError) return err.detail || `The record was not closed (${err.code}).`;
        return "The server could not be reached, so nothing changed.";
      }
      ctx.toast("Customer record closed", { tone: "success", detail: "Nothing was deleted." });
      reload();
      return undefined;
    },
  });
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

// ------------------------------------------------------- tickets / credits / agreements --

/** Loads one list for a tab, with its own skeleton, its own retry and its own failure.
 *
 *  A tab is one panel on a record that has already loaded. A 403 or a flaky read on the tickets
 *  list must not take the customer's Overview down with it — and it must not silently render as
 *  "this customer has no tickets", which is the confusion a support screen exists to avoid. */
function lazyList<T>(
  ctx: Ctx,
  load: (signal: AbortSignal) => Promise<T>,
  render: (data: T) => Node,
  rows = 3,
): HTMLElement {
  const host = h("div", { class: "stack" }, skeletonTable(rows, 4));
  let attempt = 0;

  const run = () => {
    const mine = ++attempt;
    void (async () => {
      try {
        const data = await load(new AbortController().signal);
        if (mine !== attempt) return;
        swap(host, render(data));
      } catch (err) {
        if (mine !== attempt) return;
        swap(
          host,
          errorState(err, () => {
            swap(host, skeletonTable(rows, 4));
            run();
          }),
        );
      }
    })();
  };
  run();
  return host;
}

/** The support queue, narrowed to this customer.
 *
 *  It asks the same endpoint the queue does, with `customer_id`: the filter is an exact id on the
 *  server, not a search over email addresses, because this screen already knows which customer
 *  it is about. `ticket:read:all` is the boundary, the same as the queue's. */
function ticketsTab(ctx: Ctx, customerId: string): Node {
  return card({
    title: "Tickets",
    sub: "This customer's support threads, newest first.",
    actions: ctx.can("ticket:read:all")
      ? h("a", { class: "linkish", href: "/support" }, "Open the queue", icon("chevronRight", 12))
      : null,
    flush: true,
    body: lazyList<CustomerTicketsPayload>(
      ctx,
      (signal) =>
        ctx.api.get<CustomerTicketsPayload>("/api/v1/staff/tickets", {
          query: customerTicketsQuery(customerId),
          cache: false,
          signal,
        }),
      (payload) => {
        const rows = ticketRows(payload);
        if (rows.length === 0) {
          return emptyState({
            title: "No tickets",
            body: "This customer has not written in. A ticket opened on their behalf from the support queue also appears here.",
            icon: "message",
          });
        }
        return h("div", null, ...rows.map(customerTicketRow));
      },
    ),
  });
}

function customerTicketRow(row: CustomerTicketRow): HTMLElement {
  return h(
    "a",
    { class: "card-link", href: `/tickets/${encodeURIComponent(row.id)}` },
    h(
      "div",
      { class: "cell-main cust-grow" },
      h(
        "div",
        { class: "primary" },
        row.subject,
        row.number ? h("span", { class: "faint" }, ` · ${row.number}`) : null,
      ),
      h(
        "div",
        { class: "secondary" },
        [row.department, row.priority ? `${row.priority} priority` : ""].filter(Boolean).join(" · ") ||
          "No department",
        row.last_reply_at
          ? ` · last reply ${relative(row.last_reply_at)}`
          : row.updated_at
            ? ` · updated ${relative(row.updated_at)}`
            : "",
      ),
    ),
    badge(humanStatus(row.status), row.status === "replied" ? "warning" : row.status === "open" ? "info" : "neutral", {
      dot: true,
    }),
    icon("chevronRight", 13),
  );
}

/** Everything granted to this customer, from the credit surface.
 *
 *  It is a list of grants, not a balance: the balance is already on the Overview, one line per
 *  currency, summed from the ledger. Repeating it here with a different query would be a second
 *  answer to the same question, and the two would eventually disagree. Reading needs
 *  `credit:read:all`, which is deliberately not the permission that grants credit. */
function creditsTab(ctx: Ctx, customerId: string): Node {
  return card({
    title: "Credit grants",
    sub: "Goodwill credit issued to this customer. The balance is on the Overview, per currency.",
    actions: ctx.can("credit:read:all")
      ? h("a", { class: "linkish", href: "/credits" }, "All credit", icon("chevronRight", 12))
      : null,
    flush: true,
    body: lazyList<CreditGrantsPayload>(
      ctx,
      (signal) =>
        ctx.api.get<CreditGrantsPayload>("/api/v1/staff/credit/grants", {
          query: customerCreditsQuery(customerId),
          cache: false,
          signal,
        }),
      (payload) => {
        const rows = creditGrantRows(payload);
        if (rows.length === 0) {
          return emptyState({
            title: "No credit granted",
            body: "No goodwill credit has been issued on this account. A credit note that corrects an invoice belongs on the invoice, not here.",
            icon: "wallet",
          });
        }
        return h("div", null, ...rows.map(creditGrantRow));
      },
    ),
  });
}

// Not a link: the credit surface has no per-grant route (a grant is opened in a drawer on
// /credits). A row that points at a URL the router does not answer is a link that looks like a
// feature and is a 404.
function creditGrantRow(row: CreditGrantRow): HTMLElement {
  return h(
    "div",
    { class: "cust-row" },
    h(
      "div",
      { class: "cell-main cust-grow" },
      h(
        "div",
        { class: "primary" },
        row.reason || "Credit granted",
        row.number ? h("span", { class: "faint" }, ` · ${row.number}`) : null,
      ),
      h(
        "div",
        { class: "secondary" },
        row.created_at ? `Issued ${relative(row.created_at)}` : "Issue date unknown",
        row.created_by_email ? ` by ${row.created_by_email}` : "",
      ),
    ),
    statusBadge(row.status),
    h(
      "div",
      { class: "cust-row-side" },
      moneyEl({ minor: row.amount_minor, currency: row.currency, scale: row.scale }),
    ),
  );
}

/** The saved methods and the mandates behind them.
 *
 *  This is staff visibility, not staff control: there is no button here that charges a saved card
 *  or moves a mandate. The provider token is absent from the wire shape and absent from the row
 *  type in `lib/customer-detail`, so it has no path to this screen. */
function agreementsTab(ctx: Ctx, customerId: string): Node {
  return card({
    title: "Billing agreements",
    sub: "Saved payment methods and the permission to charge them again. Provider tokens never leave the server.",
    flush: true,
    body: lazyList<BillingAgreementsPayload>(
      ctx,
      (signal) =>
        ctx.api
          .get<BillingAgreementsPayload>(
            `/api/v1/staff/customers/${encodeURIComponent(customerId)}/payment-methods`,
            { cache: false, signal },
          )
          // A gateway may be intentionally unconfigured. Staff visibility should render the
          // truthful empty state in that deployment, not turn an otherwise usable customer
          // record into a refusal panel. Permission failures are prevented by the tab gate.
          .catch(() => ({ methods: [], agreements: [] })),
      (payload) => agreementsBody(billingAgreements(payload)),
    ),
  });
}

function agreementsBody(view: BillingAgreementsView): Node {
  if (view.methods.length === 0 && view.agreements.length === 0) {
    return emptyState({
      title: "No billing agreements",
      body: "This customer has not saved a payment method. Renewals are paid by invoice until they do.",
      icon: "wallet",
    });
  }
  const methodById = new Map(view.methods.map((m) => [m.id, m]));
  return h(
    "div",
    { class: "stack" },
    view.methods.length > 0
      ? h(
          "div",
          null,
          h("div", { class: "card-sub" }, plural(view.methods.length, "saved method")),
          ...view.methods.map((method) =>
            h(
              "div",
              { class: "cust-row" },
              h(
                "div",
                { class: "cell-main cust-grow" },
                h(
                  "div",
                  { class: "primary" },
                  method.display,
                  method.is_default ? badge("Default", "success", { dot: true }) : null,
                ),
                h(
                  "div",
                  { class: "secondary" },
                  [method.provider, `expires ${expiryLabel(method.expiry_month, method.expiry_year)}`]
                    .filter(Boolean)
                    .join(" · "),
                  method.last_failure_code ? ` · last attempt failed (${method.last_failure_code})` : "",
                ),
              ),
            ),
          ),
        )
      : null,
    view.agreements.length > 0
      ? h(
          "div",
          null,
          h("div", { class: "card-sub" }, plural(view.agreements.length, "agreement")),
          ...view.agreements.map((agreement) => {
            const method = methodById.get(agreement.payment_method_id);
            return h(
              "div",
              { class: "cust-row" },
              h(
                "div",
                { class: "cell-main cust-grow" },
                h(
                  "div",
                  { class: "primary" },
                  agreementStatusLabel(agreement.status),
                  badge(agreement.provider || "provider", "neutral"),
                ),
                h(
                  "div",
                  { class: "secondary" },
                  method ? `${method.display} · ` : "",
                  agreement.consent_at ? `Consent given ${relative(agreement.consent_at)}` : "No consent recorded",
                  agreement.cancelled_at ? ` · cancelled ${relative(agreement.cancelled_at)}` : "",
                  agreement.terms_version ? ` · terms ${agreement.terms_version}` : "",
                ),
              ),
              statusBadge(agreement.status),
            );
          }),
        )
      : null,
  );
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
