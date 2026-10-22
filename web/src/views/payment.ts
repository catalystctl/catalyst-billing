// One payment, and everything that hangs off it.
//
// The header answers "what money is this" in one glance: the amount, the provider, the
// provider's own payment id (copyable, because the first thing an operator does with it is
// paste it into the provider's dashboard), the status and when it landed.
//
// The three sections are the three records a payment owns: the allocations that decided
// which invoices it settled, the refunds that moved money back, and the double-entry journal
// the ledger wrote. The journal is read-only here and everywhere — a correction is a new
// transaction, never an edit of an old one.

import { h } from "../lib/dom";
import type { Child } from "../lib/dom";
import type { Amount } from "../lib/format";
import { dateTime, formatMinor, humanStatus, relative, shortId } from "../lib/format";
import type { Ctx } from "../lib/types";
import {
  banner,
  btn,
  card,
  code,
  copyButton,
  emptyState,
  errorState,
  moneyEl,
  pageHead,
  skeletonPage,
  statusBadge,
} from "../ui/kit";
import { openRefundDialog } from "./refund";

export const meta = { title: "Payment", crumb: "Payment" };

interface Payment {
  id: string;
  provider: string;
  provider_account: string;
  provider_payment_id: string;
  amount: Amount;
  currency: string;
  status: string;
  received_at: string;
}

interface Allocation {
  id: string;
  payment_id: string;
  invoice_id: string;
  amount: Amount;
  created_at: string;
}

interface Refund {
  id: string;
  payment_id: string;
  provider_refund_id: string | null;
  operation_key: string;
  amount: Amount;
  status: string;
  reason: string;
  created_at: string;
  finished_at: string | null;
}

interface JournalEntry {
  account: string;
  debit: number;
  credit: number;
  memo: string;
}

interface JournalTransaction {
  id: string;
  kind: string;
  reference_type: string;
  reference_id: string;
  currency: string;
  created_at: string;
  entries: JournalEntry[];
}

interface PaymentDetail {
  payment: Payment;
  allocations: Allocation[];
  refunds: Refund[];
  journal: JournalTransaction[];
}

export default async function paymentPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("payment:reconcile")) return denied();

  const id = ctx.route.params.id ?? "";
  const host = h("div", { class: "page" }, skeletonPage());
  const controller = new AbortController();
  ctx.onDispose(() => controller.abort());

  const load = async (): Promise<void> => {
    try {
      const detail = await ctx.api.get<PaymentDetail>(`/api/v1/staff/payments/${id}`, {
        cache: false,
        signal: controller.signal,
      });
      if (controller.signal.aborted) return;
      host.replaceChildren(...render(detail));
    } catch (err) {
      if (controller.signal.aborted || (err instanceof DOMException && err.name === "AbortError")) return;
      host.replaceChildren(errorState(err, () => void load()));
    }
  };

  const render = (detail: PaymentDetail): Node[] => {
    const { payment } = detail;
    const fullyRefunded = payment.status === "refunded";

    const refundAction = ctx.can("payment:refund")
      ? btn("Refund", {
          variant: "danger",
          icon: "undo",
          disabled: fullyRefunded,
          title: fullyRefunded ? "This payment is fully refunded: nothing remains refundable." : "Refund this payment",
          onclick: () =>
            openRefundDialog(ctx, {
              paymentId: payment.id,
              paymentAmount: payment.amount,
              onDone: () => void load(),
            }),
        })
      : null;

    return [
      pageHead({
        title: "Payment",
        sub: h(
          "span",
          { class: "row" },
          h("a", { class: "linkish", href: "/payments" }, "All payments"),
          h("span", { class: "faint" }, "·"),
          h("span", null, "the provider's record of money it collected"),
        ),
        badge: statusBadge(payment.status),
        actions: refundAction,
      }),
      h(
        "section",
        { class: "card" },
        h(
          "div",
          { class: "card-body pay-head" },
          h(
            "div",
            { class: "pay-hero" },
            h("span", { class: "pay-label" }, "Amount received"),
            moneyEl(payment.amount, { size: "xl" }),
            h(
              "span",
              { class: "pay-sub" },
              h("strong", null, payment.provider),
              h("span", { class: "faint" }, "·"),
              h("span", { class: "mono" }, payment.provider_account),
            ),
          ),
          h(
            "div",
            { class: "pay-side" },
            h("span", { class: "pay-label" }, "Provider payment id"),
            h("div", { class: "pay-id" }, code(payment.provider_payment_id), copyButton(ctx, payment.provider_payment_id, "payment id")),
            h(
              "span",
              { class: "pay-when" },
              h("span", { class: "faint" }, "Received "),
              h("span", { title: dateTime(payment.received_at) }, `${dateTime(payment.received_at)} (${relative(payment.received_at)})`),
            ),
          ),
        ),
      ),
      banner(
        "info",
        "A refund never edits the invoice, and never creates customer credit",
        "The invoice keeps the payment it received, because the customer did pay it. A refund is recorded against this payment and the ledger gets a reversing posting. If the customer should end up holding credit they can spend, issue a credit note instead.",
      ),
      card({
        title: "Allocations",
        sub: `${detail.allocations.length} invoice${detail.allocations.length === 1 ? "" : "s"}`,
        flush: true,
        body:
          detail.allocations.length === 0
            ? note("This payment is not allocated to any invoice yet. It exists in the ledger; reconciliation or a manual allocation will decide where it goes.")
            : dataTable<Allocation>(
                [
                  {
                    label: "Invoice",
                    primary: true,
                    render: (row) =>
                      h("a", { class: "row-title", href: `/invoices/${row.invoice_id}` }, `Invoice ${shortId(row.invoice_id)}`),
                  },
                  { label: "Amount", align: "right", render: (row) => moneyEl(row.amount) },
                  {
                    label: "Allocated",
                    render: (row) => h("span", { class: "dim", title: dateTime(row.created_at) }, relative(row.created_at)),
                  },
                ],
                detail.allocations,
              ),
      }),
      card({
        title: "Refunds",
        sub: `${detail.refunds.length} recorded`,
        flush: true,
        body:
          detail.refunds.length === 0
            ? note("Nothing has been refunded against this payment.")
            : dataTable<Refund>(
                [
                  { label: "Status", primary: true, render: (row) => statusBadge(row.status) },
                  { label: "Amount", align: "right", render: (row) => moneyEl(row.amount) },
                  {
                    label: "Reason",
                    render: (row) =>
                      h("span", { class: "truncate refund-reason", title: row.reason || undefined }, row.reason || "—"),
                  },
                  {
                    label: "Created",
                    render: (row) => h("span", { class: "dim", title: dateTime(row.created_at) }, relative(row.created_at)),
                  },
                  {
                    label: "Finished",
                    render: (row) =>
                      row.finished_at
                        ? h("span", { class: "dim", title: dateTime(row.finished_at) }, relative(row.finished_at))
                        : h("span", { class: "faint" }, "—"),
                  },
                ],
                detail.refunds,
              ),
      }),
      card({
        title: "Journal",
        sub: "Double-entry postings this payment produced",
        flush: true,
        body:
          detail.journal.length === 0
            ? note("No journal transaction references this payment.")
            : h("div", null, ...detail.journal.map((tx) => journalBlock(payment, tx))),
      }),
    ];
  };

  ctx.onLive((event) => {
    if (event.kind === "refund.succeeded") void load();
  });

  void load();
  return host;
}

function journalBlock(payment: Payment, tx: JournalTransaction): HTMLElement {
  // The journal entry carries minor units and a currency, but no scale: it is written in the
  // payment's currency, so the payment's own scale is the right one. A different currency
  // here would be a data problem worth seeing, not something to paper over.
  const scale = tx.currency === payment.amount.currency ? payment.amount.scale : 2;
  const amountOf = (minor: number): Amount => ({
    minor,
    currency: tx.currency,
    scale,
    display: `${tx.currency} ${formatMinor(minor, scale)}`,
  });

  return h(
    "div",
    { class: "journal-tx" },
    h(
      "div",
      { class: "journal-head" },
      h("span", { class: "journal-kind" }, humanStatus(tx.kind)),
      h(
        "span",
        { class: "faint mono" },
        `${tx.reference_type} ${shortId(tx.reference_id)}`,
      ),
      h("span", { class: "spacer" }),
      h("span", { class: "faint mono" }, tx.currency),
      h("span", { class: "dim", title: dateTime(tx.created_at) }, dateTime(tx.created_at)),
    ),
    dataTable<JournalEntry>(
      [
        { label: "Account", primary: true, render: (row) => h("span", { class: "mono" }, row.account) },
        {
          label: "Debit",
          align: "right",
          render: (row) =>
            row.debit === 0 ? h("span", { class: "faint" }, "—") : moneyEl(amountOf(row.debit), { code: false }),
        },
        {
          label: "Credit",
          align: "right",
          render: (row) =>
            row.credit === 0 ? h("span", { class: "faint" }, "—") : moneyEl(amountOf(row.credit), { code: false }),
        },
        { label: "Memo", render: (row) => h("span", { class: "dim" }, row.memo || "—") },
      ],
      tx.entries,
    ),
  );
}

interface SimpleColumn<T> {
  label: string;
  align?: "right";
  primary?: boolean;
  render: (row: T) => Child;
}

/** A plain table for data that arrived inside a detail payload.
 *
 *  It mirrors the classes createTable uses — including `data-label` on every cell — so the
 *  mobile card layout applies without a second stylesheet. */
function dataTable<T>(columns: Array<SimpleColumn<T>>, rows: T[]): HTMLElement {
  const thead = h(
    "thead",
    null,
    h(
      "tr",
      null,
      ...columns.map((col) =>
        h("th", { class: col.align === "right" ? "num" : undefined, scope: "col" }, col.label),
      ),
    ),
  );
  const tbody = h("tbody");
  for (const row of rows) {
    tbody.appendChild(
      h(
        "tr",
        null,
        ...columns.map((col) =>
          h(
            "td",
            {
              class: [col.align === "right" ? "num" : "", col.primary ? "cell-primary" : ""].filter(Boolean).join(" "),
              "data-label": col.primary ? undefined : col.label,
            },
            col.render(row),
          ),
        ),
      ),
    );
  }
  return h("div", { class: "table-wrap table-wrap--cards" }, h("table", { class: "table" }, thead, tbody));
}

function note(text: string): HTMLElement {
  return h("div", { class: "card-body" }, h("p", { class: "empty-body" }, text));
}

function denied(): HTMLElement {
  return h(
    "div",
    { class: "page" },
    pageHead({ title: "Payment" }),
    emptyState({
      title: "Payments need the reconciliation permission",
      body: "Reading a payment requires payment:reconcile. The invoice it settled is reachable from the invoice list.",
      icon: "lock",
      action: btn("Open invoices", { href: "/invoices", icon: "invoice", size: "sm" }),
    }),
  );
}
