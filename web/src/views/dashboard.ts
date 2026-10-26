// The dashboard.
//
// This is not a wall of statistics. It is the operator's answer to "what needs me today":
// the exception list from ops.Overview, ordered by what happens if it is ignored, plus the
// four counts that belong in a header. A number nobody can act on is not on this page.

import { ApiError } from "../lib/api";
import type { Amount } from "../lib/format";
import { h } from "../lib/dom";
import { age, dateOnly, humanStatus, money, relative } from "../lib/format";
import { icon, type IconName } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  badge,
  banner,
  btn,
  card,
  emptyState,
  errorState,
  moneyEl,
  pageHead,
  sectionTitle,
  skeletonLines,
  skeletonPage,
  statusBadge,
} from "../ui/kit";
import { sparkline } from "../ui/charts";
import { createTable, type Column } from "../ui/table";

export const meta = { title: "Dashboard", crumb: "Dashboard" };

interface Exception {
  code: string;
  title: string;
  detail: string;
  count: number;
  severity: "critical" | "warning" | "info";
  money?: string;
  oldest_at?: string;
  action: string;
}

interface Overview {
  generated_at: string;
  exceptions: Exception[] | null;
  counts: {
    customers: number;
    services: number;
    open_invoices: number;
    due_subscriptions: number;
    available: boolean;
  };
}

interface InvoiceRow {
  id: string;
  number: string;
  status: string;
  currency: string;
  total: Amount;
  due: Amount;
  overdue: boolean;
  issued_at: string | null;
  due_at: string | null;
  customer?: { id: string; email: string };
}

/** The wire shape of `GET /api/v1/staff/dashboard/widgets`.
 *
 *  Every figure is nullable on purpose: `null` means the query failed and the widget is named
 *  in `unavailable`, while `0` means the period was empty. The two must render differently —
 *  a dash and a zero — because a dashboard that reports an outage as a quiet month is worse
 *  than one that reports nothing. */
interface DashboardWidgets {
  period: string;
  from: string;
  to: string;
  days: number;
  generated_at: string;
  available: boolean;
  unavailable: string[];
  revenue: Amount[];
  active_customers: number | null;
  new_customers: number | null;
  open_tickets: number | null;
  tickets_opened: number | null;
  overdue_invoices: {
    count: number;
    oldest_due_at: string | null;
    money: Amount[];
  } | null;
  /** The revenue chart: one dense series per currency, from the same ledger as `revenue`. */
  revenue_series: Array<{
    currency: string;
    points: Array<{ at: string; minor: number }>;
  }>;
  /** The window immediately before this one, so a figure can be read as up or down. */
  previous_period: {
    from: string;
    to: string;
    revenue: Amount[];
    active_customers: number | null;
    new_customers: number | null;
    tickets_opened: number | null;
  } | null;
  /** The support queue itself, oldest waiting first. */
  support_tickets: Array<{
    id: string;
    number: string;
    subject: string;
    department: string;
    priority: "low" | "normal" | "high";
    status: string;
    customer_id: string;
    email: string;
    assigned_to: string;
    created_at: string;
    waiting_since: string;
  }>;
  paid_not_provisioned: {
    code: string;
    title: string;
    detail: string;
    action: string;
    severity: "critical" | "warning" | "info";
    count: number;
    oldest_at: string | null;
    money: Amount[];
    items: Array<{
      invoice_id: string;
      number: string;
      customer_id: string;
      email: string;
      order_id: string;
      total: Amount;
      issued_at: string | null;
    }>;
    truncated: boolean;
  } | null;
}

/** The periods the server accepts. `custom` needs `from` and `to`. */
const PERIODS: ReadonlyArray<{ key: string; label: string }> = [
  { key: "7d", label: "7 days" },
  { key: "30d", label: "30 days" },
  { key: "90d", label: "90 days" },
  { key: "custom", label: "Custom" },
];

const EXCEPTION_LINKS: Record<string, string> = {
  paid_not_provisioned: "/services?has_open_operation=true",
  unknown_operations: "/operations?status=unknown",
  overdue_invoices: "/invoices?overdue=true",
  stalled_webhooks: "/webhooks?processed=false",
  failed_operations: "/operations?status=failed",
  renewals_needing_review: "/services",
  suspensions_withheld: "/services?status=active",
  ledger_imbalance: "/audit",
};

const EXCEPTION_ICONS: Record<Exception["severity"], IconName> = {
  critical: "alertCircle",
  warning: "alertTriangle",
  info: "info",
};

export default async function dashboardPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("report:read")) {
    ctx.navigate(ctx.can("service:read:all") ? "/services" : "/settings", { replace: true });
    return h("div");
  }

  const host = h("div", { class: "page" }, skeletonPage());
  const controller = new AbortController();
  ctx.onDispose(() => controller.abort());

  // The widget period. It lives in the query string so a refresh, a bookmark or a link to a
  // colleague lands on the same window: a dashboard whose range resets on every reload makes
  // two people comparing screens disagree without knowing it.
  let period = periodFromQuery(ctx.route.query.get("period"));
  let customFrom = ctx.route.query.get("from") ?? "";
  let customTo = ctx.route.query.get("to") ?? "";

  let widgets: DashboardWidgets | null = null;
  let widgetsError: unknown = null;
  let widgetsLoading = false;

  // The two hosts are created once and moved into whatever tree `render` builds, so a period
  // change replaces the widgets and leaves the exception list — which is a current state, not
  // a period figure — alone.
  const widgetHost = h("div", { class: "stack" });
  // The bar is the `.filter-bar` primitive: it aligns its children on the bottom edge, which is
  // the only line a preset button and a labelled date input can share.
  const periodHost = h("div", { class: "filter-bar period-bar" });

  const load = async () => {
    try {
      const overview = await ctx.api.get<Overview>("/api/v1/ops/overview", { cache: false, signal: controller.signal });
      if (controller.signal.aborted) return;
      host.replaceChildren(render(overview));
      // The widgets are a second request on purpose: they are period-scoped and reloadable on
      // their own, and a failure there must not blank the exception list.
      void loadWidgets();
    } catch (err) {
      if (controller.signal.aborted) return;
      host.replaceChildren(errorState(err, () => void load()));
    }
  };

  /** Re-reads the widget set for the current period. */
  const loadWidgets = async () => {
    widgetsLoading = true;
    widgetsError = null;
    renderWidgets();
    const query: Record<string, string> = { period };
    if (period === "custom") {
      query.from = customFrom;
      query.to = customTo;
    }
    try {
      const next = await ctx.api.get<DashboardWidgets>("/api/v1/staff/dashboard/widgets", {
        query,
        cache: false,
        signal: controller.signal,
      });
      if (controller.signal.aborted) return;
      widgets = next;
    } catch (err) {
      if (controller.signal.aborted) return;
      widgetsError = err;
    }
    widgetsLoading = false;
    renderWidgets();
  };

  /** Switches the window and reloads only the widgets. */
  const choosePeriod = (next: string): void => {
    if (next === period && next !== "custom") return;
    period = next;
    if (next !== "custom") {
      customFrom = "";
      customTo = "";
    }
    ctx.replaceQuery({
      period: next === "30d" ? null : next,
      from: next === "custom" ? customFrom || null : null,
      to: next === "custom" ? customTo || null : null,
    });
    renderPeriodBar();
    if (next === "custom" && (!customFrom || !customTo)) {
      // Wait for the operator to pick a range: asking for a period with no bounds is a 400
      // and a dialog telling them so, which is not a useful first answer.
      widgetHost.replaceChildren(
        emptyState({
          title: "Choose a range",
          body: "Pick a start and an end date to see the figures for that window.",
          icon: "calendar",
        }),
      );
      return;
    }
    void loadWidgets();
  };

  /** Renders the period selector.
   *
   *  Two shapes share this bar: the preset run (one line of buttons under one label) and the
   *  custom range (two labelled date inputs and the button that applies them). A labelled
   *  control is a column, a button is a line, so the bar aligns on the bottom edge — labels
   *  above, every control's bottom edge on one line, Apply on that same line. The date fields
   *  are only in the tree when Custom is picked: `hidden` on a `display: flex` element does
   *  nothing, which is why the empty range used to sit under the presets all the time. */
  const renderPeriodBar = (): void => {
    const presets = h(
      "div",
      { class: "field period-presets" },
      h("span", { class: "field-label" }, icon("calendar", 12), "Period"),
      h(
        "div",
        { class: "preset-group" },
        ...PERIODS.map((option) =>
          btn(option.label, {
            size: "sm",
            variant: option.key === period ? "primary" : "ghost",
            onclick: () => choosePeriod(option.key),
          }),
        ),
      ),
    );

    const dateField = (label: string, id: string, value: string, assign: (next: string) => void): HTMLElement =>
      h(
        "div",
        { class: "field" },
        h("label", { class: "field-label", for: id }, label),
        h("input", {
          class: "input input--sm",
          id,
          type: "date",
          value,
          onchange: (event: Event) => assign((event.target as HTMLInputElement).value),
        }),
      );

    const children: HTMLElement[] = [presets];
    if (period === "custom") {
      // The range is one group, so it either sits beside the presets or moves under them as a
      // whole — never half a range on each line, with Apply left behind.
      children.push(
        h(
          "div",
          { class: "period-range" },
          dateField("From", "period-from", customFrom, (next) => {
            customFrom = next;
          }),
          dateField("To", "period-to", customTo, (next) => {
            customTo = next;
          }),
          btn("Apply", {
            size: "sm",
            variant: "primary",
            icon: "check",
            onclick: () => {
              if (!customFrom || !customTo) {
                ctx.toast("Pick both dates", { tone: "danger", detail: "A range needs a start and an end." });
                return;
              }
              ctx.replaceQuery({ period: "custom", from: customFrom, to: customTo });
              void loadWidgets();
            },
          }),
        ),
      );
    }

    periodHost.replaceChildren(...children);
  };

  /** Renders the widget set: revenue, the counts, the overdue position and the queue. */
  const renderWidgets = (): void => {
    if (widgetsLoading && widgets === null) {
      widgetHost.replaceChildren(card({ title: "This period", body: skeletonLines(3) }));
      return;
    }
    if (widgetsError !== null) {
      widgetHost.replaceChildren(
        banner(
          "warning",
          "The period figures could not be read",
          h("span", null, describeWidgetError(widgetsError)),
          btn("Try again", { size: "sm", variant: "ghost", icon: "refresh", onclick: () => void loadWidgets() }),
        ),
      );
      return;
    }
    if (widgets === null) return;

    const unavailable = new Set(widgets.unavailable ?? []);
    const window = `${dateOnly(widgets.from)} → ${dateOnly(widgets.to)}`;

    const revenue = card({
      title: "Revenue",
      sub: `${window} · from the ledger`,
      actions: widgets.available ? null : badge("Partial", "warning", { dot: true }),
      body:
        widgets.revenue.length === 0
          ? emptyState({
              title: "No revenue in this period",
              body: "Nothing was earned between these dates. A voided invoice and a credit note both land here as zero, because the figure is the ledger and not a sum of invoices.",
              icon: "dollar",
            })
          : h(
              "div",
              { class: "stack-sm" },
              ...widgets.revenue.map((amount) =>
                h(
                  "div",
                  { class: "kv" },
                  h("span", { class: "kv-key" }, amount.currency),
                  h("span", { class: "kv-value money money--lg" }, moneyEl(amount)),
                ),
              ),
              h(
                "span",
                { class: "field-hint" },
                "Per currency and never summed: there is no exchange rate in this system, so a combined total would be arithmetic on unrelated quantities.",
              ),
            ),
    });

    const counts = h(
      "div",
      { class: "grid-4" },
      widgetStat(
        "Active customers",
        widgets.active_customers,
        `Customers with a live service, an invoice or a payment in the ${widgets.days} days`,
        "users",
        unavailable.has("active_customers"),
        trendOf(widgets.active_customers, widgets.previous_period?.active_customers),
      ),
      widgetStat(
        "New customers",
        widgets.new_customers,
        "Signed up in the period",
        "plus",
        unavailable.has("new_customers"),
        trendOf(widgets.new_customers, widgets.previous_period?.new_customers),
      ),
      widgetStat(
        "Open tickets",
        widgets.open_tickets,
        // The queue is a current state, so it has no previous-period figure; what *does* compare
        // is how many were opened, and that is the sentence the hint carries.
        widgets.tickets_opened === null
          ? "The queue right now"
          : `${widgets.tickets_opened} opened in the period` +
            trendHint(trendOf(widgets.tickets_opened, widgets.previous_period?.tickets_opened)),
        "message",
        unavailable.has("open_tickets"),
        null,
      ),
      widgetStat(
        "Overdue invoices",
        widgets.overdue_invoices?.count ?? null,
        widgets.overdue_invoices && widgets.overdue_invoices.money.length > 0
          ? `${widgets.overdue_invoices.money.map((m) => money(m)).join(" · ")} still owed`
          : "Nothing is past due",
        "clock",
        unavailable.has("overdue_invoices"),
      ),
    );

    // The chart and the support table are optional: a period with no revenue has no series, and
    // an empty queue has no table. They are filtered rather than rendered empty, because a card
    // that says "no tickets" twice (once here, once in the exception list) is noise.
    widgetHost.replaceChildren(
      ...[revenueChartCard(widgets), revenue, counts, supportQueueCard(ctx, widgets), exceptionQueueCard(widgets)]
        .filter((node): node is HTMLElement => node !== null),
    );
  };

  const render = (overview: Overview): Node => {
    const exceptions = overview.exceptions ?? [];
    const counts = overview.counts;
    const ledgerBalanced = !exceptions.some((e) => e.code === "ledger_imbalance");
    const queueClear = exceptionTotal(exceptions, "unknown_operations", "failed_operations") === 0;
    const quiet = exceptions.length === 0;

    const vitals = card({
      title: "Billing vitals",
      class: "vitals",
      actions: h("a", { class: "panel-link", href: "/invoices" }, "View invoices", icon("arrowRight", 12)),
      flush: true,
      body: h(
        "div",
        null,
        vitalRow("Customers", counts.available ? counts.customers : null, "Total on platform"),
        vitalRow("Services", counts.available ? counts.services : null, "Provisioned and billable"),
        vitalRow("Open invoices", counts.available ? counts.open_invoices : null, counts.available ? "Awaiting payment" : "Counts unavailable", true),
        vitalRow("Renewals due", counts.available ? counts.due_subscriptions : null, "Next billing run"),
        vitalStatusRow("Job queue", queueClear ? "Clear" : "Review", queueClear ? "All jobs processed" : "Operations need attention", queueClear),
        vitalStatusRow("Subledger", counts.available && ledgerBalanced ? "Balanced" : "Review", counts.available && ledgerBalanced ? "No exceptions detected" : "Check the audit trail", counts.available && ledgerBalanced),
      ),
      foot: h(
        "div",
        { class: "vital-footer-left" },
        h("span", { class: `pulse-icon${quiet ? "" : " pulse-icon--warning"}` }, icon(quiet ? "checkCircle" : "alertTriangle", 18)),
        h(
          "div",
          null,
          h("strong", null, quiet ? "The ledger is quiet." : "The engine needs a look."),
          h(
            "p",
            null,
            quiet
              ? "No exception is open. The queue is drained, every webhook is processed, and the subledger balances."
              : "Open exceptions are listed below, worst first. Each one names the action that clears it.",
          ),
        ),
      ),
    });
    vitals.querySelector(".card-foot")?.appendChild(btn("Open the queue", { href: "/operations", iconAfter: "arrowRight", size: "sm" }));

    const exceptionCard = card({
      title: "Needs attention",
      marker: "accent",
      sub: exceptions.length > 0 ? `${exceptions.length} open` : undefined,
      actions: exceptions.length > 0 ? h("a", { class: "panel-link", href: "/operations" }, "View all", icon("arrowRight", 12)) : null,
      flush: true,
      body:
        exceptions.length === 0
          ? h(
              "div",
              { class: "attention-content" },
              h(
                "div",
                { class: "healthy" },
                h("span", { class: "healthy-icon" }, icon("checkCircle", 18)),
                h(
                  "div",
                  null,
                  h("h3", null, "Nothing is on fire"),
                  h("p", null, "No exception is open. This list fills itself from the queue, webhook inbox, and ledger when something needs attention."),
                ),
              ),
              h("div", { class: "thin-rule" }),
              h("div", { class: "micro-row" }, h("span", { class: "mini-dot" }), "All operational checks are passing", h("span", { class: "micro-right" }, `Snapshot ${relative(overview.generated_at)}`)),
            )
          : h("div", null, ...exceptions.map((e) => exceptionRow(ctx, e))),
    });

    const glance = card({
      title: "At a glance",
      actions: h("a", { class: "panel-link", href: "/settings/system" }, "System settings", icon("arrowRight", 12)),
      flush: true,
      body: h(
        "div",
        null,
        glanceRow("checkCircle", "Subledger", counts.available && ledgerBalanced ? badge("Balanced", "success", { dot: true }) : badge("Review", "warning", { dot: true })),
        glanceRow("activity", "Queue", exceptionCount(exceptions, "unknown_operations", "failed_operations")),
        glanceRow("radio", "Webhook inbox", exceptionCount(exceptions, "stalled_webhooks")),
        glanceRow("clock", "Overdue", exceptionCount(exceptions, "overdue_invoices")),
        glanceRow("refresh", "Renewals", exceptionCount(exceptions, "renewals_needing_review")),
      ),
    });

    const shortcuts = card({
      title: "Quick actions",
      marker: "accent",
      class: "shortcuts",
      flush: true,
      body: h(
        "div",
        { class: "shortcut-grid" },
        shortcut(ctx, "/invoices/new", "plus", "Create an invoice", "Draft and issue in one step"),
        shortcut(ctx, "/customers", "users", "Customers", "Manage customer accounts"),
        shortcut(ctx, "/services", "server", "Services", "View provisioned services"),
      ),
    });

    const recent = ctx.can("invoice:read:all") ? recentInvoices(ctx) : null;

    return h(
      "div",
      { class: "page dashboard-page" },
      pageHead({
        title: "Dashboard",
        eyebrow: "Overview",
        sub: "The state of the billing engine, in the order it should be read.",
        actions: h(
          "div",
          { class: "row-wrap" },
          h(
            "a",
            { class: "alert-pill", href: "/invoices" },
            h("span", { class: "live-dot" }),
            `${counts.available ? counts.open_invoices : "—"} open invoice${counts.open_invoices === 1 ? "" : "s"}`,
            icon("arrowRight", 13),
          ),
          btn("Refresh", {
            icon: "refresh",
            size: "sm",
            onclick: () => {
              host.replaceChildren(skeletonPage());
              void load();
            },
          }),
        ),
      }),
      vitals,
      h("div", { class: "dashboard-grid" }, exceptionCard, glance),
      shortcuts,
      sectionTitle("Period performance"),
      periodHost,
      widgetHost,
      recent ?? h("div"),
    );
  };

  renderPeriodBar();

  const recentInvoices = (c: Ctx): Node => {
    const columns: Column<InvoiceRow>[] = [
      {
        key: "number",
        label: "Invoice",
        primary: true,
        render: (row) =>
          h(
            "div",
            { class: "cell-main" },
            h("div", { class: "primary" }, h("a", { class: "row-title", href: `/invoices/${row.id}` }, row.number || "Draft")),
            h("div", { class: "secondary" }, row.customer?.email ?? "—"),
          ),
      },
      {
        key: "status",
        label: "Status",
        render: (row) => statusBadge(row.overdue ? "overdue" : row.status),
      },
      { key: "issued", label: "Issued", render: (row) => h("span", { class: "dim" }, row.issued_at ? relative(row.issued_at) : "—") },
      { key: "due", label: "Due", render: (row) => (row.due_at ? h("span", { class: "dim" }, relative(row.due_at)) : "—") },
      { key: "total", label: "Total", align: "right", render: (row) => moneyEl(row.total) },
      {
        key: "outstanding",
        label: "Outstanding",
        align: "right",
        render: (row) => (row.due.minor > 0 ? moneyEl(row.due) : h("span", { class: "faint" }, "Settled")),
      },
    ];
    const table = createTable(c, {
      path: "/api/v1/staff/invoices",
      columns,
      limit: 6,
      query: { sort: "issued_at" },
      empty: { title: "No invoices yet", body: "A renewal or a manual invoice will appear here." },
      rowHref: (row) => `/invoices/${row.id}`,
    });
    return card({
      title: "Latest invoices",
      actions: h("a", { class: "linkish", href: "/invoices" }, "All invoices", icon("chevronRight", 12)),
      flush: true,
      body: table.el,
    });
  };

  void load();
  return host;
}

function vitalRow(label: string, value: number | null, hint: string, meter = false): HTMLElement {
  return h(
    "div",
    { class: "vital-row" },
    h("span", { class: "vital-label" }, label),
    h(
      "span",
      { class: "vital-main" },
      h("strong", null, value === null ? "—" : value.toLocaleString("en-US")),
      meter && value !== null && value > 0 ? h("span", { class: "meter", "aria-hidden": "true" }, h("span", { class: "meter-fill" })) : null,
    ),
    h("span", { class: "vital-side" }, hint),
  );
}

function vitalStatusRow(label: string, value: string, hint: string, healthy: boolean): HTMLElement {
  return h(
    "div",
    { class: "vital-row" },
    h("span", { class: "vital-label" }, label),
    h("span", { class: `vital-main vital-status ${healthy ? "is-healthy" : "is-warning"}` }, value),
    h("span", { class: "vital-side" }, hint),
  );
}

function glanceRow(iconName: IconName, label: string, value: Node): HTMLElement {
  return h(
    "div",
    { class: "glance-row" },
    h("span", { class: "glance-label" }, icon(iconName, 13), label),
    h("span", { class: "glance-result" }, value),
  );
}

function exceptionTotal(exceptions: Exception[], ...codes: string[]): number {
  return exceptions.filter((e) => codes.includes(e.code)).reduce((sum, e) => sum + e.count, 0);
}

function exceptionCount(exceptions: Exception[], ...codes: string[]): Node {
  const total = exceptionTotal(exceptions, ...codes);
  if (total === 0) return h("span", { class: "faint" }, "Clear");
  return h("span", { class: "mono" }, String(total));
}

function exceptionRow(ctx: Ctx, e: Exception): HTMLElement {
  const href = EXCEPTION_LINKS[e.code];
  const row = h(
    "div",
    { class: "exception" },
    h("span", { class: `exception-icon exception-icon--${e.severity}` }, icon(EXCEPTION_ICONS[e.severity], 12)),
    h(
      "div",
      null,
      h(
        "div",
        { class: "exception-title" },
        href ? h("a", { href, class: "row-title" }, e.title) : h("span", null, e.title),
        badge(humanStatus(e.severity), e.severity === "critical" ? "danger" : e.severity === "warning" ? "warning" : "info"),
      ),
      h("div", { class: "exception-detail" }, e.detail),
      h("div", { class: "exception-action" }, icon("arrowRight", 11), e.action),
    ),
    h(
      "div",
      { class: "exception-side" },
      h("span", { class: "exception-count" }, String(e.count)),
      e.money ? h("span", { class: "exception-money" }, e.money) : null,
      e.oldest_at ? h("span", { class: "exception-age" }, `oldest ${age(e.oldest_at)}`) : null,
    ),
  );
  if (href) {
    row.addEventListener("click", (event) => {
      if ((event.target as Element).closest("a")) return;
      ctx.navigate(href);
    });
  }
  return row;
}

function shortcut(ctx: Ctx, href: string, iconName: IconName, title: string, sub: string): HTMLElement {
  return h(
    "a",
    { class: "shortcut", href },
    h("span", { class: "shortcut-icon" }, icon(iconName, 15)),
    h("div", { class: "cell-main" }, h("div", { class: "primary" }, title), h("div", { class: "secondary" }, sub)),
    icon("chevronRight", 13),
  );
}

/** Reads the period out of the query string, falling back to the default window. */
function periodFromQuery(raw: string | null): string {
  const value = (raw ?? "").trim();
  for (const option of PERIODS) {
    if (option.key === value) return value;
  }
  return "30d";
}

/** Turns a failed widget request into a sentence an operator can act on. */
function describeWidgetError(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.code === "invalid_period") {
      return err.detail || "That is not a period this panel accepts. Choose 7, 30 or 90 days, or a range of at most a year.";
    }
    return `${err.status} ${err.code}: ${err.detail}`;
  }
  return "The database did not answer. The exception list above is unaffected.";
}

/** One widget figure.
 *
 *  `value === null` means the server could not read it, and it renders as a dash with a note
 *  saying so — never as zero. An empty period is a zero; an outage is not. */
function widgetStat(
  label: string,
  value: number | null,
  hint: string,
  iconName: IconName,
  unavailable: boolean,
  trend: Trend | null = null,
): HTMLElement {
  const missing = unavailable || value === null;
  return h(
    "section",
    { class: "card stat" },
    h("span", { class: "stat-label" }, icon(iconName, 12), label),
    h(
      "span",
      { class: "stat-value" },
      missing ? "—" : value.toLocaleString("en-US"),
      ...(missing || !trend ? [] : [trendBadge(trend)].filter((node): node is HTMLElement => node !== null)),
    ),
    h("span", { class: "stat-hint" }, missing ? "Could not be read" : hint),
  );
}

/** `Trend` is a change against the previous window.
 *
 *  `previous === null` means the previous figure could not be read, and `previous === 0` means
 *  there was nothing to compare with — both of which must render as nothing rather than as
 *  "+1000%", which is what dividing by a missing or zero baseline produces. */
interface Trend {
  current: number;
  previous: number;
  delta: number;
  percent: number | null;
}

function trendOf(current: number | null, previous: number | null | undefined): Trend | null {
  if (current === null || previous === null || previous === undefined) return null;
  const delta = current - previous;
  // A percentage of zero is not infinity, it is undefined: "up 3 from nothing" is the honest
  // sentence, and it is the one `trendBadge` renders when percent is null.
  const percent = previous === 0 ? null : (delta / previous) * 100;
  return { current, previous, delta, percent };
}

function trendHint(trend: Trend | null): string {
  if (!trend || trend.delta === 0) return "";
  if (trend.percent === null) return ` (${trend.delta > 0 ? "+" : ""}${trend.delta} on a period with none)`;
  const sign = trend.delta > 0 ? "+" : "";
  return ` (${sign}${Math.round(trend.percent)}% vs the previous period)`;
}

/** Renders a delta as a badge: up is good for customers, and never coloured for revenue, where
 *  direction is not the same as health. */
function trendBadge(trend: Trend): HTMLElement | null {
  if (trend.delta === 0) return null;
  const up = trend.delta > 0;
  const label = trend.percent === null
    ? `${up ? "+" : ""}${trend.delta}`
    : `${up ? "+" : ""}${Math.round(trend.percent)}%`;
  // `.stat-hint` and its is-up/is-down modifiers are the panel's existing vocabulary for a
  // change against a baseline, so this reuses them rather than introducing a second name for
  // the same thing. The colour is the health reading, which is why it is applied here and not
  // on the revenue chart's label.
  return h(
    "span",
    {
      class: `stat-hint ${up ? "is-up" : "is-down"}`,
      title: `Previous period: ${trend.previous.toLocaleString("en-US")}`,
    },
    icon(up ? "chevronUp" : "chevronDown", 11),
    label,
  );
}

/** The revenue chart.
 *
 *  One sparkline per currency from the ledger series the server returns, with the change against
 *  the previous period. There is no combined line: adding dollars to euros is arithmetic on
 *  unrelated quantities, which is the same reason the revenue card has no total. */
function revenueChartCard(widgets: DashboardWidgets): HTMLElement | null {
  const series = widgets.revenue_series ?? [];
  if (series.length === 0) return null;
  const previous = new Map((widgets.previous_period?.revenue ?? []).map((amount) => [amount.currency, amount]));

  return card({
    title: "Revenue over the period",
    sub: `${widgets.days} days · ${dateOnly(widgets.from)} → ${dateOnly(widgets.to)}`,
    body: h(
      "div",
      { class: "stack-sm" },
      ...series.map((line) => {
        const before = previous.get(line.currency);
        const trend = trendOf(
          line.points.reduce((sum, point) => sum + point.minor, 0),
          before === undefined ? null : before.minor,
        );
        return h(
          "div",
          { class: "stack-xs" },
          h(
            "div",
            { class: "row-between" },
            h("span", { class: "field-hint" }, line.currency),
            h(
              "span",
              { class: "row" },
              trend ? trendBadge(trend) : null,
              h("span", { class: "field-hint faint" }, `previous ${money(before)}`),
            ),
          ),
          // The sparkline is scaled to its own min and max, so an all-zero series draws nothing
          // rather than a flat line at the bottom, which would read as "revenue collapsed".
          // No wrapper: sparkline() already puts `.stat-spark` on the SVG itself, and a second
          // element around it only gave the chart two places to be sized from.
          sparkline(line.points.map((point) => point.minor), { width: 320, height: 44 }),
        );
      }),
      h(
        "span",
        { class: "field-hint" },
        "Per currency and never summed. Each bucket is a day (a week for a 90-day window), and a day with no revenue is drawn as zero rather than skipped.",
      ),
    ),
  });
}

/** The support queue: the rows behind the open-tickets count, oldest waiting first. */
function supportQueueCard(ctx: Ctx, widgets: DashboardWidgets): HTMLElement | null {
  const tickets = widgets.support_tickets ?? [];
  if (tickets.length === 0) return null;
  return card({
    title: "Waiting on support",
    sub: widgets.open_tickets === null
      ? undefined
      : `${widgets.open_tickets} open${widgets.open_tickets > tickets.length ? `, the ${tickets.length} oldest below` : ""}`,
    flush: true,
    body: h(
      "div",
      null,
      ...tickets.map((ticket) =>
        h(
          "a",
          { class: "card-link", href: `/tickets/${ticket.id}` },
          h("span", { class: "search-hit-icon" }, icon("message", 13)),
          h(
            "div",
            { class: "cell-main" },
            h("div", { class: "primary" }, ticket.subject),
            h(
              "div",
              { class: "secondary" },
              ticket.email,
              " · ",
              ticket.number || "no number",
              " · ",
              ticket.department,
            ),
          ),
          h(
            "div",
            { class: "cell-main" },
            badge(humanStatus(ticket.priority), ticket.priority === "high" ? "danger" : ticket.priority === "low" ? "info" : "neutral"),
            h("div", { class: "secondary nowrap" }, `waiting ${age(ticket.waiting_since)}`),
          ),
        ),
      ),
    ),
    foot: h(
      "div",
      { class: "row-wrap" },
      h("span", { class: "field-hint" }, "Oldest first: the ticket nobody has touched is the one that needs a reply."),
      btn("All tickets", { href: "/tickets", size: "sm", variant: "ghost" }),
    ),
  });
}

/** The paid-not-provisioned queue: settled invoices whose order bought nothing that runs.
 *
 *  It is the same exception the ops overview alerts on, rendered with the rows behind it so
 *  the card is a queue an operator can work rather than a number to worry about. */
function exceptionQueueCard(widgets: DashboardWidgets): HTMLElement {
  const queue = widgets.paid_not_provisioned;
  if (queue === null) {
    return card({
      title: "Paid, but not running",
      body: emptyState({
        title: "The queue could not be read",
        body: "The exception list above still shows what the engine knows. Reload the period to try again.",
        icon: "alertTriangle",
      }),
    });
  }

  const moneyAtStake = queue.money.length > 0 ? queue.money.map((amount) => money(amount)).join(" · ") : null;

  if (queue.count === 0) {
    return card({
      title: "Paid, but not running",
      actions: badge("Clear", "success", { dot: true }),
      // A short card for a short answer. The card fills itself from an invoice list, so its
      // empty state is the common case; a 335px card of whitespace for one sentence pushed the
      // two-column split below the fold. `.dash-empty` is the compact shape of `.empty`.
      tight: true,
      body: h(
        "div",
        { class: "dash-empty" },
        emptyState({
          title: "Nothing is paid for nothing",
          body: "Every settled order has a service that is running. This card fills itself from the invoices whose order bought nothing that is up.",
          icon: "checkCircle",
        }),
      ),
    });
  }

  return card({
    title: "Paid, but not running",
    sub: queue.count === 1 ? "1 invoice" : `${queue.count} invoices`,
    actions: badge("Critical", "danger", { dot: true }),
    flush: true,
    body: h(
      "div",
      null,
      ...queue.items.map((item) =>
        h(
          "a",
          { class: "card-link", href: `/invoices/${item.invoice_id}` },
          h("span", { class: "search-hit-icon" }, icon("alertCircle", 13)),
          h(
            "div",
            { class: "cell-main" },
            h("div", { class: "primary" }, item.number || "Invoice"),
            // A node interpolated into a template string renders as "[object HTMLSpanElement]",
            // so the amount is a child of the row rather than part of the string. The browser
            // harness caught this the moment the "paid, but not running" queue had a row in it.
            h("div", { class: "secondary" }, item.email, " · ", moneyEl(item.total)),
          ),
          h(
            "div",
            { class: "cell-main" },
            h("div", { class: "secondary nowrap" }, item.issued_at ? `issued ${relative(item.issued_at)}` : "—"),
          ),
        ),
      ),
    ),
    foot: h(
      "div",
      { class: "row-wrap" },
      h("span", { class: "field-hint" }, queue.action),
      queue.truncated
        ? h("span", { class: "field-hint faint" }, `Showing the oldest ${queue.items.length} of ${queue.count}.`)
        : null,
      moneyAtStake ? h("span", { class: "field-hint" }, `${moneyAtStake} paid for nothing.`) : null,
    ),
  });
}
