// One invoice, as a document rather than a form.
//
// The screen has two jobs. The first is to reprint what the customer received: an invoice is
// frozen evidence (the snapshot), and a panel that showed today's customer address on last
// year's invoice would be showing a document that never existed. The second is to show the
// money around it — payments, allocations, refunds, credit notes and the postings — without
// ever offering to edit one. There is no endpoint that updates a posting or a payment, and
// there is no button here that pretends otherwise: a correction is a reversal or a credit
// note, and both are visible as their own rows.
//
// The snapshot is free-form JSON (`invoice.snapshot` is whatever billing.Snapshot was at
// issue time). Every block below is rendered only when the field is present and a string, so
// an older or newer snapshot renders the part it has instead of throwing.

import { ApiError } from "../lib/api";
import type { Amount } from "../lib/format";
import { dateOnly, dateTime, humanStatus, money, moneyWithCode, relative, shortId, titleCase } from "../lib/format";
import { h, type Child } from "../lib/dom";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  amountFromInput,
  badge,
  banner,
  btn,
  card,
  emptyState,
  errorState,
  field,
  input,
  kv,
  moneyEl,
  mutate,
  pageHead,
  balanceBar,
  skeletonPage,
  statusBadge,
  textarea,
} from "../ui/kit";
import { openDialog } from "../ui/dialog";
import { openRefundDialog } from "./refund";

export const meta = { title: "Invoice", crumb: "Invoice" };

// --------------------------------------------------------------- payloads --

/** Line amounts are raw minor-unit integers; the currency and scale come from the invoice. */
interface InvoiceLine {
  position: number;
  description: string;
  /** numeric::text from Postgres, e.g. "1.000". */
  quantity: string;
  unit_minor: number;
  net_minor: number;
  tax_rate: string;
  tax_minor: number;
  total_minor: number;
}

interface InvoiceHead {
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
  customer: { id: string; email: string; name: string };
  snapshot: unknown;
  lines: InvoiceLine[];
}

interface Payment {
  id: string;
  provider: string;
  provider_account: string;
  provider_payment_id: string;
  amount: Amount;
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

interface RefundRow {
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

interface CreditNote {
  id: string;
  number: string;
  currency: string;
  total: Amount;
  tax: Amount;
  reason: string;
  status: string;
  issued_at: string | null;
  created_at: string;
  invoice_id?: string;
}

interface JournalEntry {
  account: string;
  debit: number;
  credit: number;
  memo: string;
}

interface JournalTx {
  id: string;
  kind: string;
  reference_type: string;
  reference_id: string;
  currency: string;
  created_at: string;
  entries: JournalEntry[];
}

interface Detail {
  invoice: InvoiceHead;
  payments: Payment[];
  allocations: Allocation[];
  refunds: RefundRow[];
  credit_notes: CreditNote[];
  journal: JournalTx[];
}

// ------------------------------------------------------------------- page --

export default async function invoicePage(ctx: Ctx): Promise<Node> {
  const id = ctx.route.params.id ?? "";
  if (!ctx.can("invoice:read:all")) {
    ctx.navigate("/invoices", { replace: true });
    return h("div");
  }

  const host = h("div", { class: "page" }, skeletonPage());
  const controller = new AbortController();
  ctx.onDispose(() => controller.abort());
  let current: Detail | null = null;
  let loading = false;

  const load = async () => {
    if (loading) return;
    loading = true;
    try {
      const detail = await ctx.api.get<Detail>(`/api/v1/staff/invoices/${encodeURIComponent(id)}`, {
        cache: false,
        signal: controller.signal,
      });
      if (controller.signal.aborted) return;
      current = detail;
      host.replaceChildren(...render(ctx, detail, () => void load()));
    } catch (err) {
      if (controller.signal.aborted || (err instanceof DOMException && err.name === "AbortError")) return;
      host.replaceChildren(
        errorState(err, () => {
          host.replaceChildren(skeletonPage());
          void load();
        }),
      );
    } finally {
      loading = false;
    }
  };

  // The ledger moved: reprint the document and its sections. A refund event carries the
  // payment, not the invoice, so it is matched against the payments already loaded.
  ctx.onLive((event) => {
    const kind = typeof event.kind === "string" ? event.kind : "";
    if (!/^(invoice|credit_note|refund)\./.test(kind)) return;
    const invoiceId = typeof event.invoice_id === "string" ? event.invoice_id : "";
    if (invoiceId && invoiceId !== id) return;
    if (!invoiceId) {
      const paymentId = typeof event.payment_id === "string" ? event.payment_id : "";
      if (paymentId && current && !current.payments.some((p) => p.id === paymentId)) return;
    }
    void load();
  });

  void load();
  return host;
}

// ----------------------------------------------------------------- render --

function render(ctx: Ctx, detail: Detail, reload: () => void): Node[] {
  const inv = detail.invoice;
  const payments = detail.payments ?? [];
  const allocations = detail.allocations ?? [];
  const refunds = detail.refunds ?? [];
  const creditNotes = detail.credit_notes ?? [];
  const journal = detail.journal ?? [];

  const currency = inv.total.currency;
  const scale = inv.total.scale;
  /** Wraps a raw minor-unit integer in the invoice's own currency and scale. */
  const amt = (minor: number, code = currency): Amount => ({ minor, currency: code, scale });

  ctx.setTitle(inv.number ? `Invoice ${inv.number}` : "Draft invoice");
  ctx.setCrumbs([{ label: "Invoices", href: "/invoices" }, { label: inv.number || "Draft" }]);

  const canWrite = ctx.can("invoice:write:all");
  const canVoid = ctx.can("invoice:void");
  const canRefund = ctx.can("payment:refund");

  // ------------------------------------------------------------- actions --
  const openRefund = (payment: Payment) => {
    openRefundDialog(ctx, {
      paymentId: payment.id,
      paymentAmount: payment.amount,
      onDone: () => reload(),
    });
  };

  const refundAction = () => {
    const first = payments[0];
    if (!first) return;
    if (payments.length === 1) {
      openRefund(first);
      return;
    }
    let handle: { close(): void } | null = null;
    handle = openDialog({
      title: "Refund a payment",
      subtitle: "A refund is tied to the payment that was taken. Choose which one is being refunded.",
      body: h(
        "div",
        { class: "search-results" },
        ...payments.map((payment) =>
          h(
            "button",
            {
              class: "search-hit",
              type: "button",
              onclick: () => {
                handle?.close();
                openRefund(payment);
              },
            },
            h("span", { class: "search-hit-icon" }, icon("rotate", 13)),
            h(
              "div",
              { class: "search-hit-body" },
              h("div", { class: "search-hit-title" }, moneyWithCode(payment.amount)),
              h("div", { class: "search-hit-sub" }, `${payment.provider} · ${payment.provider_payment_id}`),
            ),
            h("span", { class: "search-hit-side" }, humanStatus(payment.status)),
          ),
        ),
      ),
      footer: h("div", { class: "row spacer" }, btn("Cancel", { variant: "ghost", onclick: () => handle?.close() })),
    });
  };

  const issue = async () => {
    const confirmed = await ctx.confirm({
      title: "Issue this invoice?",
      body: "Issuing allocates the next number in the series, posts to the subledger and queues the customer's notification in the same transaction. An issued invoice cannot be un-issued: a mistake is corrected with a credit note.",
      confirmLabel: "Issue invoice",
    });
    if (!confirmed) return;
    await mutate(ctx, {
      run: () => ctx.api.post(`/api/v1/staff/invoices/${inv.id}/issue`, {}),
      success: "Invoice issued",
      after: reload,
    });
  };

  const voidInvoice = async () => {
    const reason = textarea({
      id: "void-reason",
      placeholder: "Why is this invoice being voided?",
      maxlength: 500,
    });
    await ctx.formDialog({
      title: `Void ${inv.number || "this invoice"}?`,
      subtitle: "Voiding keeps the document and its history; it does not reverse a posting.",
      danger: true,
      submitLabel: "Void invoice",
      body: h(
        "div",
        { class: "stack" },
        banner(
          "warning",
          "This cannot be undone",
          "A voided invoice cannot be restored or reissued. If money was received, the correction is a credit note.",
        ),
        field({
          label: "Reason",
          control: reason,
          id: "void-reason",
          required: true,
          hint: "Recorded in the audit log with your name.",
        }),
      ),
      onSubmit: async () => {
        const text = reason.value.trim();
        if (text.length < 3) return "A reason is required: it is what an incident review reads.";
        try {
          await ctx.api.post(`/api/v1/staff/invoices/${inv.id}/void`, { reason: text });
          ctx.toast("Invoice voided", { tone: "success", detail: inv.number || undefined });
          reload();
        } catch (err) {
          if (err instanceof ApiError) {
            if (err.code === "not_voidable") {
              return err.detail || "A paid invoice cannot be voided. Issue a credit note instead.";
            }
            return err.detail || humanStatus(err.code);
          }
          return "The invoice could not be voided.";
        }
      },
    });
  };

  const applyCredit = async () => {
    const amountInput = input({
      id: "apply-credit-amount",
      class: "input--mono",
      inputmode: "decimal",
      value: money(inv.due),
    });
    await ctx.formDialog({
      title: "Apply customer credit",
      subtitle: "Spends the customer's credit balance on this invoice. It is a journal transaction, not a payment.",
      submitLabel: "Apply credit",
      body: h(
        "div",
        { class: "stack" },
        banner(
          "info",
          "Credit is not a payment",
          "The customer's credit balance is debited and receivables are credited. When the balance reaches zero the invoice becomes paid.",
        ),
        field({
          label: "Amount",
          control: amountInput,
          id: "apply-credit-amount",
          hint: `At most ${moneyWithCode(inv.due)} — the amount still outstanding.`,
        }),
      ),
      onSubmit: async () => {
        const minor = amountFromInput(amountInput.value, inv.due.scale);
        if (minor === null) return "That amount is not a number. Use digits and a decimal point, e.g. 120.00.";
        if (minor <= 0) return "The amount must be greater than zero.";
        try {
          await ctx.api.post(`/api/v1/staff/customers/${encodeURIComponent(inv.customer.id)}/credit/apply`, {
            invoice_id: inv.id,
            amount_minor: minor,
          });
          ctx.toast("Credit applied", { tone: "success", detail: moneyWithCode(amt(minor)) });
          reload();
        } catch (err) {
          if (err instanceof ApiError) {
            if (err.code === "insufficient_credit") {
              return err.detail || "The customer does not have that much credit.";
            }
            if (err.code === "exceeds_invoice") return err.detail || "That is more than the invoice still owes.";
            return err.detail || humanStatus(err.code);
          }
          return "The credit could not be applied.";
        }
      },
    });
  };

  const createCreditNote = async () => {
    const suggested = inv.due.minor > 0 ? inv.due : inv.total;
    const amountInput = input({
      id: "credit-note-amount",
      class: "input--mono",
      inputmode: "decimal",
      value: money(suggested),
    });
    const taxInput = input({ id: "credit-note-tax", class: "input--mono", inputmode: "decimal", value: money(amt(0)) });
    const reasonInput = textarea({
      id: "credit-note-reason",
      placeholder: "Why is this being credited?",
      maxlength: 500,
    });
    await ctx.formDialog({
      title: "Issue a credit note",
      subtitle: "A credit note is an immutable counter-document. It reduces what the customer owes; it does not edit the invoice.",
      submitLabel: "Issue credit note",
      body: h(
        "div",
        { class: "stack" },
        banner(
          "info",
          "The total of all credit notes cannot exceed the invoice",
          "The server checks this under a lock and answers 409 exceeds_invoice if it would.",
        ),
        field({
          label: "Amount",
          control: amountInput,
          id: "credit-note-amount",
          hint: "The total credit, including the tax share. In the invoice's currency.",
        }),
        field({
          label: "Tax",
          control: taxInput,
          id: "credit-note-tax",
          hint: "The part of the credit that reverses tax. Zero when nothing was taxed; never more than the amount.",
        }),
        field({
          label: "Reason",
          control: reasonInput,
          id: "credit-note-reason",
          required: true,
          hint: "Printed on the credit note and recorded in the audit log.",
        }),
      ),
      onSubmit: async () => {
        const minor = amountFromInput(amountInput.value, scale);
        const taxMinor = amountFromInput(taxInput.value, scale) ?? 0;
        if (minor === null) return "That amount is not a number. Use digits and a decimal point, e.g. 120.00.";
        if (minor <= 0) return "The amount must be greater than zero.";
        if (taxMinor < 0) return "The tax share cannot be negative.";
        if (taxMinor > minor) return "The tax share cannot be larger than the credit.";
        const reason = reasonInput.value.trim();
        if (reason.length < 3) return "A credit note needs a reason.";
        try {
          await ctx.api.post(`/api/v1/staff/invoices/${inv.id}/credit-notes`, {
            amount_minor: minor,
            tax_minor: taxMinor,
            reason,
          });
          ctx.toast("Credit note issued", { tone: "success", detail: moneyWithCode(amt(minor)) });
          reload();
        } catch (err) {
          if (err instanceof ApiError) {
            if (err.code === "exceeds_invoice") {
              return err.detail || "The credit notes against this invoice would exceed its total.";
            }
            return err.detail || humanStatus(err.code);
          }
          return "The credit note could not be issued.";
        }
      },
    });
  };

  const actions: Child[] = [];
  if (canWrite && inv.status === "draft") {
    actions.push(btn("Issue", { variant: "primary", size: "sm", icon: "send", onclick: () => void issue() }));
  }
  if (canWrite && (inv.status === "open" || inv.status === "paid")) {
    actions.push(btn("Credit note", { size: "sm", icon: "undo", onclick: () => void createCreditNote() }));
  }
  if (canWrite && inv.status === "open" && inv.due.minor > 0) {
    actions.push(btn("Apply credit", { size: "sm", icon: "dollar", onclick: () => void applyCredit() }));
  }
  if (canRefund && payments.length > 0) {
    actions.push(btn("Refund", { size: "sm", icon: "rotate", onclick: refundAction }));
  }
  if (canVoid && inv.status !== "void" && inv.paid.minor === 0) {
    actions.push(btn("Void", { variant: "danger", size: "sm", icon: "x", onclick: () => void voidInvoice() }));
  }

  // ------------------------------------------------------------ document --
  const document = invoiceDocument(detail, amt);

  // ------------------------------------------------------------ payments --
  const paymentsCard = card({
    title: "Payments & allocations",
    sub: `${payments.length} payment${payments.length === 1 ? "" : "s"} · ${allocations.length} allocation${allocations.length === 1 ? "" : "s"}`,
    flush: true,
    body:
      payments.length === 0
        ? emptyState({
            title: "Nothing has been received",
            body: "A payment appears here once the provider confirms it and it is allocated to this invoice. A payment is never edited — a refund is its own record.",
            icon: "dollar",
          })
        : h(
            "div",
            null,
            dataTable<Payment>(
              [
                {
                  label: "Payment",
                  primary: true,
                  render: (p) =>
                    h(
                      "div",
                      { class: "cell-main" },
                      h("div", { class: "primary" }, h("a", { class: "row-title", href: `/payments/${p.id}` }, `${p.provider} · ${p.provider_account}`)),
                      h("div", { class: "secondary mono" }, p.provider_payment_id),
                    ),
                },
                { label: "Received", render: (p) => h("span", { class: "dim", title: dateTime(p.received_at) }, relative(p.received_at)) },
                { label: "Amount", align: "right", render: (p) => moneyEl(p.amount) },
                { label: "Status", render: (p) => statusBadge(p.status) },
                {
                  label: "Refund",
                  align: "right",
                  render: (p) =>
                    canRefund
                      ? btn("Refund", { size: "sm", variant: "ghost", icon: "rotate", onclick: () => openRefund(p) })
                      : h("span"),
                },
              ],
              payments,
            ),
            allocations.length > 0
              ? h(
                  "div",
                  { class: "inv-subsection" },
                  h(
                    "div",
                    { class: "card-head" },
                    h("h3", { class: "card-title" }, "Allocations"),
                    h("span", { class: "card-sub" }, "Which payment paid which part of this invoice"),
                  ),
                  dataTable<Allocation>(
                    [
                      {
                        label: "Payment",
                        primary: true,
                        render: (a) =>
                          h(
                            "a",
                            { class: "row-title mono", href: `/payments/${a.payment_id}` },
                            shortId(a.payment_id, 12),
                          ),
                      },
                      { label: "Allocated", render: (a) => h("span", { class: "dim", title: dateTime(a.created_at) }, relative(a.created_at)) },
                      { label: "Amount", align: "right", render: (a) => moneyEl(a.amount) },
                    ],
                    allocations,
                  ),
                )
              : null,
          ),
  });

  // ------------------------------------------------------------- refunds --
  const refundsCard = card({
    title: "Refunds",
    sub: refunds.length === 0 ? "None" : `${refunds.length}`,
    flush: true,
    body:
      refunds.length === 0
        ? emptyState({
            title: "No refunds",
            body: "A refund reverses a payment, not this invoice: the customer did pay. Any refund appears here and as a reversing posting in the journal.",
            icon: "rotate",
          })
        : dataTable<RefundRow>(
            [
              {
                label: "Refund",
                primary: true,
                render: (r) =>
                  h(
                    "div",
                    { class: "cell-main" },
                    h("div", { class: "primary mono" }, shortId(r.id, 12)),
                    h("div", { class: "secondary" }, r.provider_refund_id ?? "No provider reference"),
                  ),
              },
              {
                label: "Payment",
                render: (r) => h("a", { class: "mono", href: `/payments/${r.payment_id}` }, shortId(r.payment_id, 12)),
              },
              { label: "Amount", align: "right", render: (r) => moneyEl(r.amount) },
              { label: "Status", render: (r) => statusBadge(r.status) },
              { label: "Reason", render: (r) => h("span", { class: "dim" }, r.reason || "—") },
              {
                label: "When",
                render: (r) =>
                  h(
                    "div",
                    { class: "cell-main" },
                    h("div", { class: "primary", title: dateTime(r.created_at) }, relative(r.created_at)),
                    h("div", { class: "secondary" }, r.finished_at ? `settled ${relative(r.finished_at)}` : "not settled"),
                  ),
              },
            ],
            refunds,
          ),
  });

  // -------------------------------------------------------- credit notes --
  const creditNotesCard = card({
    title: "Credit notes",
    sub: creditNotes.length === 0 ? "None" : `${creditNotes.length}`,
    actions:
      canWrite && (inv.status === "open" || inv.status === "paid")
        ? btn("Create credit note", { size: "sm", icon: "plus", onclick: () => void createCreditNote() })
        : null,
    flush: true,
    body:
      creditNotes.length === 0
        ? emptyState({
            title: "No credit notes",
            body: "Crediting this invoice issues a separate numbered document. It is how a mistake is corrected without rewriting the invoice.",
            icon: "undo",
          })
        : dataTable<CreditNote>(
            [
              {
                label: "Number",
                primary: true,
                render: (cn) => h("div", { class: "cell-main" }, h("div", { class: "primary mono" }, cn.number), h("div", { class: "secondary" }, cn.reason || "No reason recorded")),
              },
              { label: "Total", align: "right", render: (cn) => moneyEl(cn.total) },
              { label: "Tax", align: "right", render: (cn) => moneyEl(cn.tax) },
              { label: "Status", render: (cn) => statusBadge(cn.status) },
              {
                label: "Issued",
                render: (cn) => (cn.issued_at ? h("span", { class: "dim", title: dateTime(cn.issued_at) }, dateOnly(cn.issued_at)) : h("span", { class: "faint" }, "Not issued")),
              },
            ],
            creditNotes,
          ),
  });

  // ------------------------------------------------------------- journal --
  const journalCard = card({
    title: "Journal",
    sub: journal.length === 0 ? "No postings" : `${journal.length} transaction${journal.length === 1 ? "" : "s"}`,
    body:
      journal.length === 0
        ? emptyState({
            title: "Nothing has been posted",
            body: "A draft invoice has no posting. Issuing it writes one, and every later movement writes another.",
            icon: "book",
          })
        : h(
            "div",
            { class: "stack" },
            banner(
              "info",
              "Postings are append-only",
              "A correction is a reversing transaction, so a mistake appears as a second posting with the opposite sign — never as an edit.",
            ),
            ...journal.map((tx) => journalBlock(tx, amt)),
          ),
  });

  // ------------------------------------------------------------ snapshot --
  const snapshot = asRecord(inv.snapshot);
  const snapshotCard =
    snapshot && Object.keys(snapshot).length > 0
      ? card({
          title: "Snapshot",
          sub: "Frozen evidence, captured at issue",
          body: h(
            "details",
            { class: "inv-snapshot" },
            h(
              "summary",
              null,
              icon("database", 13),
              `Show the frozen snapshot (schema v${numberField(snapshot, "schema_version") ?? "?"})`,
            ),
            h("pre", { class: "code-block inv-json" }, jsonText(inv.snapshot)),
            h(
              "p",
              { class: "field-hint" },
              "This is what the customer received. Catalog, tax and profile edits cannot change it.",
            ),
          ),
        })
      : null;

  // --------------------------------------------------------------- aside --
  const settlementCard = card({
    title: "Settlement",
    body: h(
      "div",
      { class: "stack" },
      balanceBar(inv.paid.minor, inv.total.minor, { dueTone: inv.overdue ? "danger" : "warning" }),
      h(
        "div",
        { class: "legend" },
        h("span", { class: "legend-item" }, h("span", { class: "legend-swatch legend-swatch--paid" }), "Paid", moneyEl(inv.paid)),
        h("span", { class: "legend-item" }, h("span", { class: "legend-swatch legend-swatch--due" }), "Outstanding", moneyEl(inv.due)),
      ),
      h(
        "div",
        { class: "stack-sm" },
        kv("Total", moneyEl(inv.total)),
        kv("Paid", moneyEl(inv.paid)),
        kv("Outstanding", moneyEl(inv.due)),
        kv("Due", inv.due_at ? dateOnly(inv.due_at) : "—"),
        inv.overdue ? kv("Overdue", badge("Overdue", "warning", { dot: true })) : null,
      ),
      inv.status === "void"
        ? h("p", { class: "field-hint" }, "This invoice is void. It keeps its history and its postings.")
        : null,
    ),
  });

  const customerCard = card({
    title: "Customer",
    body: h(
      "div",
      { class: "stack-sm" },
      kv("Name", inv.customer.name || "—"),
      kv("Email", inv.customer.email),
      h(
        "div",
        { class: "row-wrap" },
        btn("Open customer", { href: `/customers/${inv.customer.id}`, size: "sm", icon: "users" }),
        btn("Their invoices", { href: `/invoices?customer_id=${encodeURIComponent(inv.customer.id)}`, size: "sm", icon: "invoice" }),
      ),
    ),
  });

  const notices: Node[] = [];
  if (inv.overdue) {
    notices.push(
      banner(
        "warning",
        "This invoice is overdue",
        inv.due_at ? `It was due ${dateOnly(inv.due_at)} (${relative(inv.due_at)}). Dunning follows the configured policy.` : "The due date has passed.",
      ),
    );
  }
  if (inv.status === "draft") {
    notices.push(
      banner(
        "info",
        "This is a draft",
        "A draft has no number and no posting. Issuing it allocates a number, posts to the ledger and queues the customer's notification.",
      ),
    );
  }
  if (inv.status === "void") {
    notices.push(banner("neutral", "This invoice is void", "Voiding does not delete the document and does not reverse a posting. A correction is a credit note."));
  }

  return [
    pageHead({
      title: inv.number || "Draft invoice",
      badge: statusBadge(inv.overdue ? "overdue" : inv.status),
      sub: h(
        "span",
        null,
        h("a", { href: `/customers/${inv.customer.id}` }, inv.customer.name || inv.customer.email),
        inv.issued_at ? ` · issued ${dateOnly(inv.issued_at)}` : " · not issued",
        inv.due_at ? ` · due ${dateOnly(inv.due_at)}` : "",
      ),
      actions: actions.length > 0 ? h("div", { class: "row-wrap" }, ...actions) : null,
    }),
    ...notices,
    h(
      "div",
      { class: "split" },
      h("div", { class: "stack" }, document, paymentsCard, refundsCard, creditNotesCard, journalCard, snapshotCard),
      h("div", { class: "stack" }, settlementCard, customerCard),
    ),
  ];
}

// --------------------------------------------------------------- document --

/** The document itself: what the customer received, reprinted from the snapshot. */
function invoiceDocument(detail: Detail, amt: (minor: number, code?: string) => Amount): HTMLElement {
  const inv = detail.invoice;
  const snap = asRecord(inv.snapshot);
  const merchant = asRecord(snap?.merchant);
  const customer = asRecord(snap?.customer);
  const period = asRecord(snap?.period);
  const tax = asRecord(snap?.tax_evidence);

  const issuer = h(
    "div",
    { class: "doc-issuer" },
    h("strong", null, textField(merchant, "name") || "Catalyst Billing"),
    textField(merchant, "address") ? h("span", null, textField(merchant, "address")) : null,
    textField(merchant, "tax_id") ? h("span", null, `Tax ID ${textField(merchant, "tax_id")}`) : null,
    textField(merchant, "country_code") ? h("span", null, textField(merchant, "country_code")) : null,
  );

  const meta = h(
    "dl",
    { class: "doc-meta" },
    h("dt", null, "Number"),
    h("dd", null, inv.number || "Draft"),
    h("dt", null, "Status"),
    h("dd", null, statusBadge(inv.overdue ? "overdue" : inv.status)),
    h("dt", null, "Issued"),
    h("dd", null, inv.issued_at ? dateOnly(inv.issued_at) : "—"),
    h("dt", null, "Due"),
    h("dd", null, inv.due_at ? dateOnly(inv.due_at) : "—"),
    h("dt", null, "Currency"),
    h("dd", null, inv.total.currency),
  );

  const address = [
    textField(customer, "billing_address"),
    textField(customer, "tax_id") ? `Tax ID ${textField(customer, "tax_id")}` : "",
    textField(customer, "country_code"),
  ]
    .filter((line) => line !== "")
    .join("\n");

  // The billed party comes from the snapshot when it has one, so the reprint matches the
  // document; the live customer record is only the fallback for a draft.
  const billedName = inv.customer.name || textField(customer, "name");
  const parties = h(
    "div",
    { class: "doc-parties" },
    party("Billed to", [billedName, inv.customer.email, address]),
    period && (textField(period, "start") || textField(period, "end"))
      ? party("Service period", [
          [textField(period, "start"), textField(period, "end")].filter(Boolean).join(" → "),
          textField(period, "key"),
        ])
      : null,
    textField(snap, "terms") ? party("Terms", [textField(snap, "terms")]) : null,
  );

  const lines = inv.lines ?? [];
  const subtotal = lines.reduce((sum, line) => sum + line.net_minor, 0);
  const taxTotal = lines.reduce((sum, line) => sum + line.tax_minor, 0);

  const linesTable = h(
    "table",
    { class: "doc-lines" },
    h(
      "thead",
      null,
      h(
        "tr",
        null,
        h("th", null, "Description"),
        h("th", { class: "num" }, "Qty"),
        h("th", { class: "num" }, "Unit"),
        h("th", { class: "num" }, "Net"),
        h("th", { class: "num" }, "Tax"),
        h("th", { class: "num" }, "Total"),
      ),
    ),
    h(
      "tbody",
      null,
      ...lines.map((line) =>
        h(
          "tr",
          null,
          h(
            "td",
            { class: "desc" },
            line.description,
            line.tax_rate ? h("small", null, `Tax ${line.tax_rate}`) : null,
          ),
          h("td", { class: "num" }, quantityText(line.quantity)),
          h("td", { class: "num" }, moneyEl(amt(line.unit_minor), { code: false })),
          h("td", { class: "num" }, moneyEl(amt(line.net_minor), { code: false })),
          h("td", { class: "num" }, moneyEl(amt(line.tax_minor), { code: false })),
          h("td", { class: "num" }, moneyEl(amt(line.total_minor), { code: false })),
        ),
      ),
    ),
  );

  const totals = h(
    "div",
    { class: "doc-totals" },
    lines.length > 0 ? totalRow("Subtotal", moneyEl(amt(subtotal), { code: false })) : null,
    lines.length > 0 ? totalRow("Tax", moneyEl(amt(taxTotal), { code: false })) : null,
    h(
      "div",
      { class: "doc-total-row doc-total-row--grand" },
      h("span", null, "Total"),
      moneyEl(inv.total),
    ),
    totalRow("Paid", moneyEl(inv.paid, { code: false })),
    h(
      "div",
      { class: "doc-total-row doc-total-row--grand" },
      h("span", null, inv.due.minor > 0 ? "Outstanding" : "Settled"),
      moneyEl(inv.due),
    ),
  );

  const taxNote = tax && (textField(tax, "mode") || textField(tax, "rate"))
    ? [
        textField(tax, "mode") ? `${textField(tax, "mode")} tax` : "tax",
        textField(tax, "rate") ? `rate ${textField(tax, "rate")}` : "",
        textField(tax, "jurisdiction") ? `· ${textField(tax, "jurisdiction")}` : "",
        textField(tax, "policy") ? `· policy ${textField(tax, "policy")}` : "",
        textField(tax, "exemption_reason") ? `· ${textField(tax, "exemption_reason")}` : "",
      ]
        .filter(Boolean)
        .join(" ")
    : "";

  return h(
    "section",
    { class: "invoice-doc" },
    h("header", { class: "doc-head" }, issuer, meta),
    parties,
    // The line table is the only block that can exceed the document's width; it scrolls
    // inside the document rather than clipping, because a clipped total is a wrong total.
    h("div", { class: "inv-doc-lines" }, linesTable),
    totals,
    h(
      "footer",
      { class: "doc-foot" },
      h("span", null, `${inv.total.currency} · ${inv.total.scale} decimal place${inv.total.scale === 1 ? "" : "s"}${taxNote ? ` · ${taxNote}` : ""}`),
      h("span", null, "An issued document is immutable. Corrections are credit notes, never edits."),
    ),
  );
}

function party(label: string, lines: string[]): HTMLElement {
  const kept = lines.map((line) => line.trim()).filter((line) => line !== "");
  return h(
    "div",
    null,
    h("div", { class: "doc-party-label" }, label),
    h("div", { class: "doc-party" }, kept.length > 0 ? kept.join("\n") : "—"),
  );
}

function totalRow(label: string, value: Child): HTMLElement {
  return h("div", { class: "doc-total-row" }, h("span", null, label), value);
}

// ---------------------------------------------------------------- journal --

/** One transaction, with its entries and a balance check that is visible, not assumed. */
function journalBlock(tx: JournalTx, amt: (minor: number, code?: string) => Amount): HTMLElement {
  const entries = tx.entries ?? [];
  const debits = entries.reduce((sum, e) => sum + e.debit, 0);
  const credits = entries.reduce((sum, e) => sum + e.credit, 0);
  const balanced = debits === credits;

  return h(
    "div",
    { class: "inv-journal" },
    h(
      "div",
      { class: "inv-journal-head" },
      h("span", { class: "inv-journal-kind" }, titleCase(tx.kind)),
      h("span", { class: "mono faint" }, `${tx.reference_type} ${shortId(tx.reference_id, 10)}`),
      h("span", { class: "spacer" }),
      balanced ? null : badge("Unbalanced", "danger", { dot: true }),
      h("span", { class: "faint", title: dateTime(tx.created_at) }, dateTime(tx.created_at)),
    ),
    h(
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
            h("th", { scope: "col" }, "Account"),
            h("th", { class: "num", scope: "col" }, "Debit"),
            h("th", { class: "num", scope: "col" }, "Credit"),
            h("th", { scope: "col" }, "Memo"),
          ),
        ),
        h(
          "tbody",
          null,
          ...entries.map((entry) =>
            h(
              "tr",
              null,
              h("td", null, h("span", { class: "mono" }, entry.account)),
              h("td", { class: "num" }, entry.debit === 0 ? h("span", { class: "faint" }, "—") : moneyEl(amt(entry.debit, tx.currency), { code: false })),
              h("td", { class: "num" }, entry.credit === 0 ? h("span", { class: "faint" }, "—") : moneyEl(amt(entry.credit, tx.currency), { code: false })),
              h("td", { class: "dim" }, entry.memo || "—"),
            ),
          ),
        ),
        h(
          "tfoot",
          null,
          h(
            "tr",
            null,
            h("td", null, h("span", { class: "faint" }, "Total")),
            h("td", { class: "num" }, moneyEl(amt(debits, tx.currency), { code: false })),
            h("td", { class: "num" }, moneyEl(amt(credits, tx.currency), { code: false })),
            h("td", { class: "dim" }, balanced ? "balanced" : "does not balance"),
          ),
        ),
      ),
    ),
  );
}

// ---------------------------------------------------------------- helpers --

interface DataColumn<T> {
  label: string;
  align?: "left" | "right";
  primary?: boolean;
  render: (row: T) => Child;
}

/** A static table for data that arrives embedded in the detail payload. */
function dataTable<T>(columns: DataColumn<T>[], rows: T[]): HTMLElement {
  return h(
    "div",
    { class: "table-wrap table-wrap--cards" },
    h(
      "table",
      { class: "table" },
      h(
        "thead",
        null,
        h(
          "tr",
          null,
          ...columns.map((col) => h("th", { class: col.align === "right" ? "num" : "", scope: "col" }, col.label)),
        ),
      ),
      h(
        "tbody",
        null,
        ...rows.map((row) => {
          const tr = h("tr");
          for (const col of columns) {
            tr.appendChild(
              h(
                "td",
                {
                  class: [col.align === "right" ? "num" : "", col.primary ? "cell-primary" : ""].filter(Boolean).join(" "),
                  "data-label": col.primary ? undefined : col.label,
                },
                col.render(row),
              ),
            );
          }
          return tr;
        }),
      ),
    ),
  );
}

/** A quantity is numeric::text from Postgres ("1.000"); show it without the padding zeros. */
function quantityText(value: string | number | null | undefined): string {
  if (value === null || value === undefined) return "—";
  const raw = String(value).trim();
  if (!/^-?\d+(\.\d+)?$/.test(raw)) return raw;
  const dot = raw.indexOf(".");
  if (dot < 0) return raw;
  const whole = raw.slice(0, dot);
  const fraction = raw.slice(dot + 1).replace(/0+$/, "");
  return fraction === "" ? whole : `${whole}.${fraction}`;
}

function asRecord(value: unknown): Record<string, unknown> | null {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}

function textField(record: Record<string, unknown> | null, key: string): string {
  const value = record?.[key];
  return typeof value === "string" ? value.trim() : "";
}

function numberField(record: Record<string, unknown>, key: string): number | null {
  const value = record[key];
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

function jsonText(value: unknown): string {
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return "The snapshot could not be rendered.";
  }
}
