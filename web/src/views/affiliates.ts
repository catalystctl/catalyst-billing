// Affiliates: who we owe, what for, and the payout that settles it.
//
// This is a reconciliation screen, not a leaderboard. An operator arrives with one question —
// "what do we owe, and what have we already paid" — and the answer has to be checkable, so the
// screen shows the three figures the server reconciles *and* the rows that make them up:
// every commission with the order or renewal invoice it came from and the payout that settled
// it, and every payout with its method and reference.
//
// Two properties of the server shape this page:
//
//   - A payout is an append-only record. It links the commissions it settled; it never restates
//     what a commission was worth. So the action here is "record a payout", never "edit".
//   - `reconciles` is computed server-side from `earned - paid == outstanding`. When it is
//     false the page says so plainly instead of showing a confident total: that is a defect in
//     the data, and the rows below are how to find it.
//
// Reading needs report:read. Recording a payout needs credit:read:all, which is a borrow —
// every staff role holds it, support included. A dedicated `affiliate:payout` is the right
// long-term answer; the banner says what the action currently requires rather than pretending
// the guard is narrower than it is.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateTime, money, relative, shortId } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  badge,
  banner,
  btn,
  card,
  emptyState,
  field,
  input,
  mono,
  pageHead,
  sectionTitle,
  select,
  skeletonLines,
} from "../ui/kit";
import { createTable, type Column } from "../ui/table";
import type { Amount } from "../lib/format";

export const meta = { title: "Affiliates", crumb: "Affiliates" };

/** One affiliate, as `affiliateJSON` renders it. */
interface Affiliate {
  id: string;
  user_id: string;
  email: string;
  code: string;
  commission_rate: string;
  payout_method: string;
  enabled: boolean;
  /** 'active' | 'suspended' | 'deleted'. Derived server-side; branch on this, not on `enabled`. */
  status: AffiliateStatus;
  suspended_at: string | null;
  suspended_reason: string;
  deleted_at: string | null;
  deleted_reason: string;
  referred_count: number;
  balances: Array<{ currency: string; owed: Amount; paid: Amount }>;
  created_at: string;
}

type AffiliateStatus = "active" | "suspended" | "deleted";

/** One row of a reconciliation: a commission or a payout. */
interface ReconRow {
  kind: "commission" | "renewal" | "payout";
  id: string;
  invoice_id?: string;
  order_id?: string;
  amount: Amount;
  status: string;
  rate?: string;
  occurred_at: string;
  paid_at?: string;
  payout_id?: string;
  renewal_index?: number;
}

/** One affiliate's books in one currency. */
interface Reconciliation {
  affiliate_id: string;
  code: string;
  email: string;
  currency: string;
  earned: Amount;
  paid: Amount;
  outstanding: Amount;
  payouts: number;
  reconciles: boolean;
  rows: ReconRow[];
}

/** A recorded payout. */
interface PayoutHistory {
  id: string;
  affiliate_id: string;
  currency: string;
  amount: Amount;
  method: string;
  reference: string;
  note: string;
  created_by?: string;
  period_from?: string;
  period_to?: string;
  created_at: string;
}

interface ReportResponse {
  data: Reconciliation[];
  history: PayoutHistory[];
  period: { from: string; to: string };
}

/** The windows the server accepts. A custom range needs both bounds. */
const PERIODS: ReadonlyArray<{ key: string; label: string }> = [
  { key: "30d", label: "30 days" },
  { key: "90d", label: "90 days" },
  { key: "custom", label: "Custom" },
];

/** The methods the schema's CHECK allows. */
const METHODS: ReadonlyArray<{ value: string; label: string }> = [
  { value: "credit", label: "Account credit" },
  { value: "paypal", label: "PayPal" },
  { value: "bank_transfer", label: "Bank transfer" },
  { value: "manual", label: "Manual (out of band)" },
];

/** The permission the payout action requires today. */
const PAYOUT_PERMISSION = "credit:read:all";

/**
 * The permission the four controls require: setting the rate, suspending, reinstating and
 * deleting. It is credit:write — not report:read — because each of them changes how much
 * money will later leave the business. Support holds neither, so the screen offers the
 * actions only to a role that can complete them rather than letting a click fail with 403.
 */
const CONTROL_PERMISSION = "credit:write";

export default async function affiliatesPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("report:read")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Affiliates" }),
      card({
        body: emptyState({
          title: "Not available for your role",
          body: "The affiliate programme is a report: it needs report:read. Recording a payout additionally needs credit:read:all.",
          icon: "lock",
        }),
      }),
    );
  }

  const canPay = ctx.can(PAYOUT_PERMISSION);
  const canControl = ctx.can(CONTROL_PERMISSION);

  let period = periodFromQuery(ctx.route.query.get("period"));
  let customFrom = ctx.route.query.get("from") ?? "";
  let customTo = ctx.route.query.get("to") ?? "";
  let selected = ctx.route.query.get("affiliate") ?? "";
  // The archive is off by default: a deleted affiliate is not somebody to enrol, suspend or
  // reason about. It stays reachable because a deleted affiliate can still be owed money, and
  // the screen that settles a debt has to be able to name the payee.
  let showDeleted = ctx.route.query.get("deleted") === "1";
  let loaded: Affiliate[] = [];

  const reportHost = h("div", { class: "stack" }, skeletonLines(3));
  // `.filter-bar` aligns on the bottom edge, so the preset run, the custom dates and Apply
  // share one line whatever their heights (see the same bar in dashboard.ts).
  const periodHost = h("div", { class: "filter-bar period-bar" });
  const noticeHost = h("div", { hidden: true });

  // ---------------------------------------------------------------- the list --

  const columns: Column<Affiliate>[] = [
    {
      key: "code",
      label: "Affiliate",
      primary: true,
      render: (row) =>
        h(
          "div",
          { class: "cell-main" },
          h("div", { class: "primary" }, mono(row.code)),
          h("div", { class: "secondary" }, row.email || shortId(row.user_id)),
        ),
    },
    {
      key: "rate",
      label: "Rate",
      render: (row) => h("span", { class: "mono" }, row.commission_rate),
    },
    {
      key: "referred",
      label: "Referred",
      align: "right",
      render: (row) => h("span", { class: "mono" }, String(row.referred_count)),
    },
    {
      key: "balances",
      label: "Owed",
      align: "right",
      render: (row) => balancesCell(row, "owed"),
    },
    {
      key: "paid",
      label: "Paid out",
      align: "right",
      hideSmall: true,
      render: (row) => balancesCell(row, "paid"),
    },
    {
      key: "method",
      label: "Method",
      hideSmall: true,
      render: (row) => h("span", { class: "dim" }, methodLabel(row.payout_method)),
    },
    {
      key: "status",
      label: "Status",
      render: (row) => statusCell(row),
    },
    {
      key: "actions",
      label: "",
      align: "right",
      width: "1%",
      render: (row) =>
        h(
          "div",
          { class: "row-wrap" },
          btn(row.id === selected ? "Selected" : "Reconcile", {
            size: "sm",
            variant: row.id === selected ? "primary" : "ghost",
            icon: "eye",
            title: "Show what this affiliate earned, what was paid and the rows that make up the difference",
            onclick: () => void selectAffiliate(row.id),
          }),
          // The three controls. A deleted affiliate offers none of them: the record is closed
          // and the server would refuse the request anyway, so the screen does not pretend.
          row.status !== "deleted" && canControl
            ? btn("Set rate", {
                size: "sm",
                variant: "ghost",
                icon: "percent",
                title: `Set a custom commission rate for ${row.code}. It applies to future commission only.`,
                onclick: () => void openRate(row),
              })
            : null,
          row.status === "active" && canControl
            ? btn("Suspend", {
                size: "sm",
                variant: "ghost",
                icon: "pauseCircle",
                title: `Stop ${row.code} earning: their link stops attributing and no further commission accrues`,
                onclick: () => void suspendAffiliate(row),
              })
            : null,
          row.status === "suspended" && canControl
            ? btn("Reinstate", {
                size: "sm",
                variant: "ghost",
                icon: "playCircle",
                title: `Put ${row.code} back into the programme with the rate they had`,
                onclick: () => void reinstateAffiliate(row),
              })
            : null,
          row.status !== "deleted" && canControl
            ? btn("Delete", {
                size: "sm",
                variant: "ghost",
                icon: "trash",
                title: `Archive ${row.code}. Their commission and any balance stay: a deleted affiliate can still be owed money`,
                onclick: () => void deleteAffiliate(row),
              })
            : null,
        ),
    },
  ];

  const empty = {
    title: "No affiliates yet",
    body: "An affiliate is a customer who may refer others. Enrol one with a referral code and a commission rate, and their referrals, commissions and payouts appear here.",
  };

  const table = createTable<Affiliate>(ctx, {
    path: "/api/v1/staff/affiliates",
    columns,
    query: showDeleted ? { include_deleted: "1" } : {},
    limit: 100,
    empty,
    rowKey: (row) => row.id,
    onLoaded: (rows) => {
      loaded = rows;
      // Land on the first affiliate when nothing is selected, so the screen answers a
      // question instead of showing an empty report beside a list.
      if (!selected && rows.length > 0 && rows[0]) {
        selected = rows[0].id;
        void loadReport();
      }
    },
  });

  // ------------------------------------------------------------- the report --

  /** Selects an affiliate and loads its reconciliation. */
  const selectAffiliate = async (affiliateID: string): Promise<void> => {
    selected = affiliateID;
    ctx.replaceQuery({ affiliate: affiliateID });
    await table.reload();
    await loadReport();
  };

  /** Reloads the list, and the report when the changed payee is the selected one. */
  const refresh = async (affiliateID: string): Promise<void> => {
    await table.reload();
    if (affiliateID === selected) await loadReport();
  };

  /**
   * What this affiliate is still owed, in words, for the dialog that has to say it plainly.
   * A suspension or a deletion does not forgive a balance, and the operator should read that
   * before they click rather than discover it afterwards.
   */
  const owedSummary = (row: Affiliate): string => {
    const owed = row.balances.filter((balance) => balance.owed.minor > 0).map((balance) => money(balance.owed));
    return owed.length === 0 ? "nothing" : owed.join(", ");
  };

  // --------------------------------------------------------------- controls --

  /** Sets a custom commission rate for one affiliate. */
  const openRate = async (row: Affiliate): Promise<void> => {
    const rate = input({
      id: "affiliate-rate",
      value: row.commission_rate,
      placeholder: "10%, 0.1 or 1/10",
      autocomplete: "off",
    });
    await ctx.formDialog({
      title: `Commission rate for ${row.code}`,
      subtitle: `Currently ${row.commission_rate}. The rate is an exact rational, never a float.`,
      submitLabel: "Save the rate",
      body: h(
        "div",
        { class: "stack" },
        field({
          label: "Rate",
          control: rate,
          required: true,
          hint: "10%, 0.1 or 1/10. At most 100%, and 0 is a legitimate \"this affiliate earns nothing\".",
        }),
        banner(
          "info",
          "Future commission only",
          h(
            "span",
            null,
            "Every commission row keeps the rate that produced it, so changing the rate cannot restate what a past order earned.",
          ),
        ),
      ),
      onSubmit: async () => {
        const value = rate.value.trim();
        if (!value) return "Enter a rate: 10%, 0.1 or 1/10.";
        try {
          const saved = await ctx.api.patch<Affiliate>(`/api/v1/staff/affiliates/${row.id}`, {
            commission_rate: value,
          });
          ctx.toast("Commission rate saved", {
            tone: "success",
            detail: `${row.code} · ${row.commission_rate} → ${saved.commission_rate}`,
          });
          await refresh(row.id);
          return undefined;
        } catch (err) {
          return describeError(err);
        }
      },
    });
  };

  /** Suspends (bans) an affiliate, with the reason the next operator will read. */
  const suspendAffiliate = async (row: Affiliate): Promise<void> => {
    const reason = input({
      id: "suspend-reason",
      placeholder: "Chargebacks on every referral",
      autocomplete: "off",
    });
    await ctx.formDialog({
      title: `Suspend ${row.code}?`,
      subtitle: "Their referral link stops attributing signups and no further commission accrues.",
      submitLabel: "Suspend",
      danger: true,
      body: h(
        "div",
        { class: "stack" },
        field({
          label: "Reason",
          control: reason,
          required: true,
          hint: "Recorded with your name. It is what the next operator reviews.",
        }),
        banner(
          "warning",
          "What a suspension does not do",
          h(
            "span",
            null,
            `It does not touch what ${row.code} has already earned: ${owedSummary(row)} stays owed and stays payable.`,
          ),
        ),
      ),
      onSubmit: async () => {
        const value = reason.value.trim();
        if (!value) return "A reason is required: it is what the next operator reviews.";
        try {
          await ctx.api.post(`/api/v1/staff/affiliates/${row.id}/suspend`, { reason: value });
          ctx.toast("Affiliate suspended", {
            tone: "success",
            detail: `${row.code} can no longer earn`,
          });
          await refresh(row.id);
          return undefined;
        } catch (err) {
          return describeError(err);
        }
      },
    });
  };

  /** Puts a suspended affiliate back to work with the rate they had. */
  const reinstateAffiliate = async (row: Affiliate): Promise<void> => {
    const ok = await ctx.confirm({
      title: `Reinstate ${row.code}?`,
      body: `Their link attributes again and commission accrues at ${row.commission_rate}.`,
      confirmLabel: "Reinstate",
    });
    if (!ok) return;
    try {
      await ctx.api.post(`/api/v1/staff/affiliates/${row.id}/reinstate`, {});
      ctx.toast("Affiliate reinstated", {
        tone: "success",
        detail: `${row.code} earns again at ${row.commission_rate}`,
      });
      await refresh(row.id);
    } catch (err) {
      ctx.toast(err instanceof ApiError ? err.detail || describeError(err) : "The affiliate could not be reinstated", {
        tone: "danger",
        detail: err instanceof ApiError ? err.code : undefined,
      });
    }
  };

  /**
   * Archives an affiliate. The dialog states what deletion does *not* do, because the whole
   * point of an archive rather than a removal is that the debt survives it.
   */
  const deleteAffiliate = async (row: Affiliate): Promise<void> => {
    const reason = input({
      id: "delete-reason",
      placeholder: "Left the programme",
      autocomplete: "off",
    });
    await ctx.formDialog({
      title: `Delete ${row.code}?`,
      subtitle: "The affiliate leaves the programme and the default list. Their ledger stays.",
      submitLabel: "Delete the affiliate",
      danger: true,
      body: h(
        "div",
        { class: "stack" },
        field({
          label: "Reason",
          control: reason,
          required: true,
          hint: "Recorded with your name. It is what the next operator reviews.",
        }),
        banner(
          "warning",
          "The debt is not deleted",
          h(
            "span",
            null,
            `A deleted affiliate with unpaid commission is still a debt. ${row.code} keeps ${owedSummary(row)}: it stays on the payout report and can still be paid.`,
          ),
        ),
      ),
      onSubmit: async () => {
        const value = reason.value.trim();
        if (!value) return "A reason is required: it is what the next operator reviews.";
        try {
          await ctx.api.del(`/api/v1/staff/affiliates/${row.id}`, { reason: value });
          ctx.toast("Affiliate deleted", {
            tone: "success",
            detail: `${row.code} archived; their commission and balance are kept`,
          });
          // A deleted affiliate leaves the default list, so a selection that pointed at it
          // would show a report for a payee the operator can no longer see. Clear it unless
          // the archive is being shown.
          if (!showDeleted && selected === row.id) {
            selected = "";
            ctx.replaceQuery({ affiliate: null });
          }
          await table.reload(showDeleted ? { include_deleted: "1" } : {});
          await loadReport();
          return undefined;
        } catch (err) {
          return describeError(err);
        }
      },
    });
  };

  /** Loads the reconciliation for the selected affiliate. */
  const loadReport = async (): Promise<void> => {
    if (!selected) {
      reportHost.replaceChildren(
        emptyState({
          title: "Pick an affiliate",
          body: "Choose a payee from the list to see what they have earned, what has been paid and the rows that make up the difference.",
          icon: "wallet",
        }),
      );
      return;
    }
    swap(reportHost, h("div", { class: "stack" }, skeletonLines(4)));
    const query: Record<string, string> = { period };
    if (period === "custom") {
      query.from = customFrom;
      query.to = customTo;
    }
    try {
      const report = await ctx.api.get<ReportResponse>(`/api/v1/staff/affiliates/${selected}/payouts`, {
        query,
        cache: false,
      });
      reportHost.replaceChildren(...renderReport(report));
    } catch (err) {
      reportHost.replaceChildren(
        banner(
          "danger",
          "The reconciliation could not be read",
          h("span", null, describeError(err)),
          btn("Try again", { size: "sm", variant: "ghost", icon: "refresh", onclick: () => void loadReport() }),
        ),
      );
    }
  };

  /** Renders the report: one card per currency, then the payout history. */
  const renderReport = (report: ReportResponse): Node[] => {
    const nodes: Node[] = [];

    // A payee who has left the programme still has books, and the state is stated above them:
    // a suspended affiliate earns nothing more, a deleted one is archived, and in both cases
    // what is below is still owed and still payable.
    const payee = loaded.find((row) => row.id === selected);
    if (payee?.status === "suspended") {
      nodes.push(
        banner(
          "warning",
          `${payee.code} is suspended`,
          h(
            "span",
            null,
            `They accrue no further commission${payee.suspended_reason ? ` (${payee.suspended_reason})` : ""}, but what is below is still owed and can still be paid.`,
          ),
        ),
      );
    }
    if (payee?.status === "deleted") {
      nodes.push(
        banner(
          "info",
          `${payee.code} is deleted`,
          h(
            "span",
            null,
            `The affiliate has left the programme${payee.deleted_reason ? ` (${payee.deleted_reason})` : ""}. Their ledger is kept, so what is still owed remains payable and auditable.`,
          ),
        ),
      );
    }

    const mismatch = report.data.filter((line) => !line.reconciles);
    if (mismatch.length > 0) {
      nodes.push(
        banner(
          "danger",
          "These books do not reconcile",
          h(
            "span",
            null,
            `${mismatch.map((line) => line.currency).join(", ")}: earned minus paid does not equal outstanding. The rows below are the difference; a payout is refused until they agree.`,
          ),
        ),
      );
    }

    if (report.data.length === 0) {
      nodes.push(
        card({
          title: "Reconciliation",
          body: emptyState({
            title: "Nothing earned in this period",
            body: "This affiliate has no commission and no payout in this window. Widen the period, or check that their referral code has been used.",
            icon: "wallet",
          }),
        }),
      );
    }

    for (const line of report.data) {
      nodes.push(
        card({
          title: `Reconciliation · ${line.currency}`,
          sub: `${dateTime(report.period.from)} → ${dateTime(report.period.to)}`,
          actions: line.reconciles ? badge("Reconciles", "success", { dot: true }) : badge("Out of balance", "danger", { dot: true }),
          body: h(
            "div",
            { class: "stack" },
            h(
              "div",
              { class: "grid-3" },
              figure("Earned", line.earned, "Every commission, all time"),
              figure("Paid", line.paid, `${line.payouts} payout${line.payouts === 1 ? "" : "s"} recorded`),
              figure("Outstanding", line.outstanding, "What a payout would settle now"),
            ),
            rowsTable(line),
            canPay
              ? h(
                  "div",
                  { class: "row-wrap" },
                  btn("Record a payout", {
                    variant: "primary",
                    icon: "wallet",
                    disabled: line.outstanding.minor <= 0,
                    title:
                      line.outstanding.minor > 0
                        ? "Settle the outstanding balance and record it"
                        : "There is nothing outstanding in this currency",
                    onclick: () => void openPayout(line),
                  }),
                  h(
                    "span",
                    { class: "field-hint" },
                    `Settles the whole outstanding balance in ${line.currency}. A commission is not divisible, so a partial payout is not offered.`,
                  ),
                )
              : h(
                  "span",
                  { class: "field-hint" },
                  `Recording a payout needs ${PAYOUT_PERMISSION}. You can read the report but not settle it.`,
                ),
          ),
        }),
      );
    }

    nodes.push(historyCard(report.history));
    return nodes;
  };

  /** The rows that make up the difference. */
  const rowsTable = (line: Reconciliation): Node => {
    if (line.rows.length === 0) {
      return h("span", { class: "field-hint" }, "No commission or payout falls inside this window.");
    }
    const head = h(
      "tr",
      null,
      h("th", null, "Kind"),
      h("th", null, "Source"),
      h("th", { class: "num" }, "Amount"),
      h("th", null, "Status"),
      h("th", null, "When"),
      h("th", null, "Settled by"),
    );
    const body = line.rows.map((row) =>
      h(
        "tr",
        null,
        h("td", null, badge(kindLabel(row), row.kind === "payout" ? "info" : row.kind === "renewal" ? "accent" : "neutral")),
        h("td", null, sourceCell(row)),
        h("td", { class: "num" }, h("span", { class: "money" }, money(row.amount))),
        h("td", null, h("span", { class: "dim" }, row.status)),
        h(
          "td",
          null,
          h("span", { class: "dim nowrap", title: dateTime(row.occurred_at) }, relative(row.occurred_at)),
        ),
        h("td", null, row.payout_id ? h("span", { class: "mono" }, shortId(row.payout_id)) : h("span", { class: "faint" }, "owed")),
      ),
    );
    return h(
      "div",
      { class: "table-wrap" },
      h("table", { class: "table" }, h("thead", null, head), h("tbody", null, ...body)),
    );
  };

  /** Where a row came from: an order, a renewal invoice, or a payout. */
  const sourceCell = (row: ReconRow): Node => {
    if (row.kind === "payout") {
      return h("span", { class: "dim" }, "Payout");
    }
    if (row.kind === "renewal" && row.invoice_id) {
      return h(
        "div",
        { class: "cell-main" },
        h("a", { class: "row-title", href: `/invoices/${row.invoice_id}` }, "Renewal invoice"),
        h(
          "div",
          { class: "secondary" },
          row.renewal_index ? `renewal ${row.renewal_index} · ${row.rate ?? ""}` : (row.rate ?? ""),
        ),
      );
    }
    if (row.order_id) {
      return h(
        "div",
        { class: "cell-main" },
        h("a", { class: "row-title", href: `/orders/${row.order_id}` }, "Order"),
        h("div", { class: "secondary" }, row.rate ?? ""),
      );
    }
    return h("span", { class: "faint" }, "—");
  };

  /** The payouts already recorded. */
  const historyCard = (history: PayoutHistory[]): Node =>
    card({
      title: "Recorded payouts",
      sub: history.length > 0 ? `${history.length} most recent` : undefined,
      flush: true,
      body:
        history.length === 0
          ? emptyState({
              title: "No payout has been recorded",
              body: "A payout is a row here, not an edit of the commissions it settles. Nothing has left yet.",
              icon: "wallet",
            })
          : h(
              "div",
              { class: "table-wrap" },
              h(
                "table",
                { class: "table" },
                h(
                  "thead",
                  null,
                  h(
                    "tr",
                    null,
                    h("th", null, "Payout"),
                    h("th", null, "Amount"),
                    h("th", null, "Method"),
                    h("th", null, "Reference"),
                    h("th", null, "When"),
                  ),
                ),
                h(
                  "tbody",
                  null,
                  ...history.map((p) =>
                    h(
                      "tr",
                      null,
                      h("td", null, mono(shortId(p.id))),
                      h("td", null, h("span", { class: "money" }, money(p.amount))),
                      h("td", null, methodLabel(p.method)),
                      h("td", null, p.reference ? mono(p.reference) : h("span", { class: "faint" }, "—")),
                      h(
                        "td",
                        null,
                        h("span", { class: "dim nowrap", title: dateTime(p.created_at) }, relative(p.created_at)),
                      ),
                    ),
                  ),
                ),
              ),
            ),
    });

  // ------------------------------------------------------------- the action --

  /** Records a payout for one currency. */
  const openPayout = async (line: Reconciliation): Promise<void> => {
    const method = select(
      METHODS.map((m) => ({ value: m.value, label: m.label })),
      { id: "payout-method", value: "bank_transfer" },
    );
    const reference = input({ id: "payout-reference", placeholder: "SEPA-1234, PayPal txn id…", autocomplete: "off" });
    const note = input({ id: "payout-note", placeholder: "March run", autocomplete: "off" });

    await ctx.formDialog({
      title: `Record a payout of ${money(line.outstanding)}`,
      subtitle: `Settles every outstanding commission in ${line.currency} and records one row that links them.`,
      submitLabel: "Record the payout",
      body: h(
        "div",
        { class: "stack" },
        field({ label: "Method", control: method, required: true }),
        field({
          label: "Reference",
          control: reference,
          hint: "What you will quote when you reconcile the bank statement.",
        }),
        field({ label: "Note", control: note, hint: "Optional. Context for the next person to read this." }),
        h(
          "span",
          { class: "field-hint" },
          "The payout row and its audit event commit together. If the audit write fails, the money does not move.",
        ),
      ),
      onSubmit: async () => {
        try {
          const recorded = await ctx.api.post<{ id: string; amount: Amount }>(
            `/api/v1/staff/affiliates/${line.affiliate_id}/payouts`,
            {
              currency: line.currency,
              method: method.value,
              reference: reference.value.trim(),
              note: note.value.trim(),
              from: period === "custom" ? customFrom : undefined,
              to: period === "custom" ? customTo : undefined,
            },
          );
          ctx.toast("Payout recorded", {
            tone: "success",
            detail: `${money(recorded.amount)} · ${shortId(recorded.id)}`,
          });
          await table.reload();
          await loadReport();
          return undefined;
        } catch (err) {
          const apiErr = err instanceof ApiError ? err : null;
          showNotice(apiErr);
          return describeError(err);
        }
      },
    });
  };

  /** A refused payout is a fact about the books, so it stays on the page, not only in a toast. */
  const showNotice = (apiErr: ApiError | null): void => {
    noticeHost.hidden = false;
    const code = apiErr?.code ?? "";
    const title =
      code === "balance_mismatch"
        ? "The balance disagrees with the commission rows"
        : code === "nothing_to_pay"
          ? "There is nothing outstanding to pay"
          : "The payout was not recorded";
    swap(
      noticeHost,
      banner(
        "danger",
        title,
        h("span", null, apiErr ? `${apiErr.detail} (${apiErr.status} ${apiErr.code})` : "The request failed."),
        btn("Dismiss", { size: "sm", variant: "ghost", icon: "x", onclick: () => (noticeHost.hidden = true) }),
      ),
    );
  };

  // ------------------------------------------------------------ period bar --

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
            onclick: () => {
              period = option.key;
              if (option.key !== "custom") {
                customFrom = "";
                customTo = "";
                ctx.replaceQuery({ period: option.key === "30d" ? null : option.key, from: null, to: null });
                void loadReport();
              }
              renderPeriodBar();
            },
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

    // The range is only in the tree when Custom is picked: `hidden` does nothing on a
    // `display: flex` element, so the empty From/To row used to sit under the presets always.
    const children: HTMLElement[] = [presets];
    if (period === "custom") {
      // The range is one group, so it either sits beside the presets or moves under them as a
      // whole — never half a range on each line, with Apply left behind.
      children.push(
        h(
          "div",
          { class: "period-range" },
          dateField("From", "affiliate-period-from", customFrom, (next) => {
            customFrom = next;
          }),
          dateField("To", "affiliate-period-to", customTo, (next) => {
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
              void loadReport();
            },
          }),
        ),
      );
    }

    periodHost.replaceChildren(...children);
  };

  renderPeriodBar();

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Affiliates",
      sub: "Who we owe, what for, and the payouts that settled it.",
      actions: canPay
        ? badge("Can record payouts", "success", { dot: true })
        : badge("Read only", "neutral", { dot: true }),
    }),
    banner(
      "info",
      "Reconcile, then pay",
      h(
        "span",
        null,
        "Earned minus paid must equal outstanding. The report states all three and lists the rows behind them, so a payout is a record you can check rather than a number you have to trust.",
      ),
    ),
    noticeHost,
    h(
      "div",
      { class: "split" },
      h(
        "div",
        { class: "stack" },
        sectionTitle(
          "Payees",
          btn(showDeleted ? "Hide deleted" : "Show deleted", {
            size: "sm",
            variant: "ghost",
            icon: showDeleted ? "eye" : "trash",
            title:
              "A deleted affiliate is archived, not removed: their commission and any balance are kept, and can still be paid",
            onclick: () => {
              showDeleted = !showDeleted;
              ctx.replaceQuery({ deleted: showDeleted ? "1" : null });
              void table.reload(showDeleted ? { include_deleted: "1" } : {});
            },
          }),
        ),
        table.el,
      ),
      h("div", { class: "stack" }, periodHost, reportHost),
    ),
  );

  // The table loads itself; the report loads when a selection exists.
  void loadReport();
}

// ------------------------------------------------------------------ helpers --

/** Reads the period out of the query string, falling back to the default window. */
function periodFromQuery(raw: string | null): string {
  const value = (raw ?? "").trim();
  for (const option of PERIODS) {
    if (option.key === value) return value;
  }
  return "30d";
}

/**
 * The status badge, with the reason underneath when there is one.
 *
 * It renders the server's derived status and never re-derives it from `enabled`: the server
 * is the authority on whether an affiliate can earn, and a screen that computed its own
 * answer would eventually disagree with the request it just sent.
 */
function statusCell(row: Affiliate): Node {
  switch (row.status) {
    case "suspended":
      return h(
        "div",
        { class: "cell-main" },
        badge("Suspended", "danger", { dot: true }),
        row.suspended_reason ? h("div", { class: "secondary" }, row.suspended_reason) : null,
      );
    case "deleted":
      return h(
        "div",
        { class: "cell-main" },
        badge("Deleted", "neutral"),
        row.deleted_reason ? h("div", { class: "secondary" }, row.deleted_reason) : null,
      );
    default:
      return badge("Active", "success", { dot: true });
  }
}

/** The label for a payout method. */
function methodLabel(method: string): string {
  for (const m of METHODS) {
    if (m.value === method) return m.label;
  }
  return method || "—";
}

/** The label for a reconciliation row's kind. */
function kindLabel(row: ReconRow): string {
  switch (row.kind) {
    case "commission":
      return "Signup commission";
    case "renewal":
      return "Renewal commission";
    case "payout":
      return "Payout";
    default:
      return row.kind;
  }
}

/** One of the three figures the report reconciles. */
function figure(label: string, amount: Amount, hint: string): HTMLElement {
  return h(
    "div",
    { class: "stat" },
    h("span", { class: "stat-label" }, label),
    h("span", { class: "stat-value money" }, money(amount)),
    h("span", { class: "stat-hint" }, hint),
  );
}

/** One currency's owed or paid total, or a dash when there is none. */
function balancesCell(row: Affiliate, field: "owed" | "paid"): Node {
  if (row.balances.length === 0) return h("span", { class: "faint" }, "—");
  return h(
    "div",
    { class: "cell-main num" },
    ...row.balances.map((balance) =>
      h(
        "div",
        { class: field === "owed" && balance.owed.minor > 0 ? "primary money" : "secondary money" },
        money(balance[field]),
      ),
    ),
  );
}

/** Turns an API failure into a sentence. */
function describeError(err: unknown): string {
  if (err instanceof ApiError) {
    switch (err.code) {
      case "nothing_to_pay":
        return "There is no outstanding balance in that currency. A second click pays nothing twice.";
      case "balance_mismatch":
        return "The materialised balance disagrees with the commission rows. The report shows both figures; fix the difference before paying.";
      case "not_found":
        return "That affiliate does not exist.";
      case "affiliate_deleted":
        return "That affiliate has been deleted. Their ledger is kept, but the record itself is closed.";
      case "invalid_input":
        return err.detail || "That value is not accepted.";
      case "invalid_period":
        return err.detail || "That is not a period this panel accepts.";
      default:
        return `${err.status} ${err.code}: ${err.detail}`;
    }
  }
  return "The request failed. Try again.";
}
