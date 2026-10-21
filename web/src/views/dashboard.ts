// The dashboard.
//
// This is not a wall of statistics. It is the operator's answer to "what needs me today":
// the exception list from ops.Overview, ordered by what happens if it is ignored, plus the
// four counts that belong in a header. A number nobody can act on is not on this page.

import type { Amount } from "../lib/format";
import { h } from "../lib/dom";
import { age, humanStatus, relative } from "../lib/format";
import { icon, type IconName } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  badge,
  btn,
  card,
  emptyState,
  errorState,
  moneyEl,
  pageHead,
  skeletonPage,
  statusBadge,
} from "../ui/kit";
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

  const load = async () => {
    try {
      const overview = await ctx.api.get<Overview>("/api/v1/ops/overview", { cache: false, signal: controller.signal });
      if (controller.signal.aborted) return;
      host.replaceChildren(render(overview));
    } catch (err) {
      if (controller.signal.aborted) return;
      host.replaceChildren(errorState(err, () => void load()));
    }
  };

  const render = (overview: Overview): Node => {
    const exceptions = overview.exceptions ?? [];
    const counts = overview.counts;
    const critical = exceptions.filter((e) => e.severity === "critical").length;
    const greeting = greetingFor(new Date());

    const hero = h(
      "section",
      { class: "hero" },
      h(
        "div",
        null,
        h("div", { class: "hero-greeting" }, greeting),
        h(
          "h1",
          { class: "hero-title" },
          exceptions.length === 0
            ? "The ledger is quiet."
            : critical > 0
              ? `${critical} thing${critical === 1 ? "" : "s"} need${critical === 1 ? "s" : ""} you now.`
              : "A few things want a look.",
        ),
        h(
          "p",
          { class: "hero-sub" },
          exceptions.length === 0
            ? "No exception is open. The queue is drained, every webhook is processed and the subledger balances."
            : "These are the open exceptions the billing engine knows about, worst first. Each one names the action that clears it.",
        ),
        h(
          "div",
          { class: "row-wrap" },
          btn("Open the queue", { href: "/operations", icon: "activity", size: "sm" }),
          btn("Review invoices", { href: "/invoices", icon: "invoice", size: "sm" }),
          btn("Customers", { href: "/customers", icon: "users", size: "sm" }),
        ),
      ),
      h(
        "div",
        { class: "hero-figure" },
        h("span", { class: "hero-figure-label" }, "Open invoices"),
        h("span", { class: "money money--xl" }, counts.available ? String(counts.open_invoices) : "—"),
        h(
          "span",
          { class: "hero-figure-sub" },
          counts.available
            ? `${counts.due_subscriptions} subscription${counts.due_subscriptions === 1 ? "" : "s"} due to renew`
            : "Counts unavailable — the database did not answer",
        ),
        h("span", { class: "hero-figure-sub faint" }, `Snapshot ${relative(overview.generated_at)}`),
      ),
    );

    const stats = h(
      "div",
      { class: "grid-4" },
      stat("Customers", counts.available ? counts.customers : null, "Total on the platform", "users"),
      stat("Services", counts.available ? counts.services : null, "Provisioned and billable", "server"),
      stat("Open invoices", counts.available ? counts.open_invoices : null, "Awaiting payment", "invoice"),
      stat("Renewals due", counts.available ? counts.due_subscriptions : null, "Next billing run", "refresh"),
    );

    const exceptionCard = card({
      title: "Needs attention",
      sub: exceptions.length > 0 ? `${exceptions.length} open` : undefined,
      actions: exceptions.length > 0 ? badge("Live", "accent", { dot: true }) : null,
      flush: true,
      body:
        exceptions.length === 0
          ? emptyState({
              title: "Nothing is on fire",
              body: "No exception is open. This list fills itself from the queue, the webhook inbox and the ledger.",
              icon: "checkCircle",
            })
          : h("div", null, ...exceptions.map((e) => exceptionRow(ctx, e))),
    });

    const side = h(
      "div",
      { class: "stack" },
      card({
        title: "At a glance",
        body: h(
          "div",
          { class: "stack-sm" },
          kvRow("Subledger", counts.available ? badge("Balanced", "success", { dot: true }) : badge("Unknown", "warning", { dot: true })),
          kvRow("Queue", exceptionCount(exceptions, "unknown_operations", "failed_operations")),
          kvRow("Webhook inbox", exceptionCount(exceptions, "stalled_webhooks")),
          kvRow("Overdue", exceptionCount(exceptions, "overdue_invoices")),
          kvRow("Renewals", exceptionCount(exceptions, "renewals_needing_review")),
        ),
      }),
      card({
        title: "Shortcuts",
        flush: true,
        body: h(
          "div",
          null,
          shortcut(ctx, "/invoices/new", "plus", "Create an invoice", "Draft and issue in one step"),
          shortcut(ctx, "/customers", "users", "Find a customer", "Search by email or name"),
          shortcut(ctx, "/operations", "activity", "Work the queue", "Retry or reconcile an operation"),
          shortcut(ctx, "/audit", "shield", "Read the audit log", "Who did what, and when"),
        ),
      }),
    );

    const recent = ctx.can("invoice:read:all") ? recentInvoices(ctx) : null;

    return h(
      "div",
      { class: "page" },
      pageHead({
        title: "Overview",
        sub: "The state of the billing engine, in the order it should be read.",
        actions: btn("Refresh", {
          icon: "refresh",
          size: "sm",
          onclick: () => {
            host.replaceChildren(skeletonPage());
            void load();
          },
        }),
      }),
      hero,
      stats,
      h("div", { class: "split" }, exceptionCard, side),
      recent ?? h("div"),
    );
  };

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

function greetingFor(now: Date): string {
  const hour = now.getHours();
  if (hour < 5) return "Working late";
  if (hour < 12) return "Good morning";
  if (hour < 18) return "Good afternoon";
  return "Good evening";
}

function stat(label: string, value: number | null, hint: string, iconName: IconName): HTMLElement {
  // No sparkline here on purpose: there is no time series in the overview payload, and a
  // chart drawn from invented numbers is worse than no chart at all.
  return h(
    "section",
    { class: "card stat" },
    h("span", { class: "stat-label" }, icon(iconName, 12), label),
    h("span", { class: "stat-value" }, value === null ? "—" : value.toLocaleString("en-US")),
    h("span", { class: "stat-hint" }, hint),
  );
}

function kvRow(label: string, value: Node): HTMLElement {
  return h("div", { class: "kv" }, h("span", { class: "kv-key" }, label), h("span", { class: "kv-value" }, value));
}

function exceptionCount(exceptions: Exception[], ...codes: string[]): Node {
  const total = exceptions.filter((e) => codes.includes(e.code)).reduce((sum, e) => sum + e.count, 0);
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
    { class: "card-link", href },
    h("span", { class: "search-hit-icon" }, icon(iconName, 13)),
    h("div", { class: "cell-main" }, h("div", { class: "primary" }, title), h("div", { class: "secondary" }, sub)),
    icon("chevronRight", 13),
  );
}
