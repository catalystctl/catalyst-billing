// Refunding a payment.
//
// Shared by the payment screen and the invoice screen, because the two questions an operator
// actually asks are "refund this payment" and "refund the payment behind this invoice".
//
// Three behaviours here are the API contract, not UI preference:
//
//  - The amount is optional. Absent means "everything still refundable", which is what the
//    button says, and the server computes it under a row lock.
//  - A refund above the configured threshold answers `409 approval_required` and does not
//    move money. The dialog then offers to *request* the approval, because the requester
//    cannot approve their own refund.
//  - An `unknown` outcome is not a failure. It is a 202: the provider may have refunded, so
//    the dialog says "check the provider" instead of inviting a retry that refunds twice.

import { ApiError } from "../lib/api";
import type { Amount } from "../lib/format";
import { money, moneyWithCode } from "../lib/format";
import { h } from "../lib/dom";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { banner, btn, field, input, textarea } from "../ui/kit";
import { openDialog } from "../ui/dialog";

export interface RefundOptions {
  paymentId: string;
  /** The payment amount, for the "everything" hint. */
  paymentAmount?: Amount;
  /** Called after a refund is created, accepted, or an approval is requested. */
  onDone?: (result: RefundResult) => void;
}

export interface RefundResult {
  id: string;
  status: string;
  amount: Amount;
  outcome?: string;
  replayed?: boolean;
}

export function openRefundDialog(ctx: Ctx, options: RefundOptions): void {
  const amountInput = input({
    id: "refund-amount",
    placeholder: options.paymentAmount ? money(options.paymentAmount) : "0.00",
    class: "input--mono",
    inputmode: "decimal",
    autocomplete: "off",
  });
  const reasonInput = textarea({
    id: "refund-reason",
    placeholder: "Why is this being refunded? The reason is recorded in the audit log.",
    maxlength: 500,
  });
  const errorHost = h("div");
  const resultHost = h("div");

  const submit = btn("Refund", { variant: "danger-solid", type: "submit" });
  const cancel = btn("Cancel", { variant: "ghost" });

  const form = h(
    "form",
    { class: "stack", novalidate: true },
    errorHost,
    resultHost,
    h(
      "div",
      { class: "banner banner--info" },
      icon("info", 15),
      h(
        "span",
        null,
        options.paymentAmount
          ? h("span", null, "Payment amount: ", h("strong", null, moneyWithCode(options.paymentAmount)), ". Leave the amount empty to refund everything still refundable.")
          : "Leave the amount empty to refund everything still refundable.",
      ),
    ),
    field({
      label: "Amount",
      control: amountInput,
      id: "refund-amount",
      hint: "In the payment's currency. An empty amount means the full refundable balance.",
    }),
    field({
      label: "Reason",
      control: reasonInput,
      id: "refund-reason",
      hint: "Required for the audit trail. Never shown to the customer verbatim.",
    }),
  );

  const handle = openDialog({
    title: "Refund a payment",
    subtitle: "A refund does not change the invoice: the customer did pay. The ledger gets a reversing posting.",
    body: h("div", { class: "stack" }, form),
    footer: h("div", { class: "row spacer" }, cancel, submit),
    onClose: () => {},
    locked: () => submit instanceof HTMLButtonElement && submit.disabled,
  });

  cancel.addEventListener("click", () => handle.close());

  const run = async (body: Record<string, unknown>) => {
    if (submit instanceof HTMLButtonElement) submit.disabled = true;
    errorHost.replaceChildren();
    resultHost.replaceChildren();
    try {
      const result = await ctx.api.post<RefundResult>(`/api/v1/staff/payments/${options.paymentId}/refunds`, body);
      options.onDone?.(result);
      if (result.status === "unknown" || result.outcome === "unknown") {
        resultHost.replaceChildren(
          banner(
            "warning",
            "The provider did not answer",
            "The refund may have happened. Do not retry: the reconciler will ask the provider and resolve it. The refund is recorded and visible in the payment's history.",
          ),
        );
        if (submit instanceof HTMLButtonElement) submit.disabled = false;
        return;
      }
      ctx.toast(result.replayed ? "Refund already recorded" : "Refund created", {
        tone: "success",
        detail: `${money(result.amount)} ${result.amount.currency} · ${result.status}`,
      });
      handle.close();
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.code === "approval_required") {
          renderApprovalOffer(err);
          return;
        }
        const message =
          err.code === "exceeds_refundable"
            ? err.detail || "That is more than is still refundable."
            : err.code === "not_refundable"
              ? err.detail || "This payment cannot be refunded."
              : err.code === "provider_refused"
                ? "The provider refused the refund. Nothing was recorded, so it is safe to try a different amount."
                : err.code === "provider_unavailable"
                  ? "The configured provider does not support refunds."
                  : err.detail || "The refund could not be created.";
        errorHost.replaceChildren(banner("danger", "Refund failed", message));
      } else {
        errorHost.replaceChildren(banner("danger", "Refund failed", "The server could not be reached."));
      }
      if (submit instanceof HTMLButtonElement) submit.disabled = false;
    }
  };

  const renderApprovalOffer = (err: ApiError) => {
    const approval = err.problem.approval as { id?: string } | undefined;
    errorHost.replaceChildren(
      banner(
        "warning",
        "A second signature is required",
        "This refund is above the approval threshold. You can request it now; a different staff member with the refund permission must approve it before any money moves.",
      ),
    );
    resultHost.replaceChildren(
      h(
        "div",
        { class: "row" },
        btn("Request approval", {
          icon: "send",
          onclick: async (event) => {
            const button = event.currentTarget as HTMLButtonElement;
            button.disabled = true;
            try {
              await runWithApprovalRequest();
            } finally {
              button.disabled = false;
            }
          },
        }),
        approval?.id ? h("span", { class: "faint mono" }, `approval ${approval.id.slice(0, 8)}`) : null,
      ),
    );
  };

  const runWithApprovalRequest = async () => {
    const amount = parseAmount(amountInput.value, options.paymentAmount);
    try {
      await ctx.api.post(`/api/v1/staff/payments/${options.paymentId}/refunds`, {
        amount_minor: amount ?? 0,
        reason: reasonInput.value.trim(),
        request_approval: true,
      });
      ctx.toast("Approval requested", {
        tone: "success",
        detail: "A second staff member must approve it before the refund runs.",
      });
      handle.close();
    } catch (err) {
      errorHost.replaceChildren(
        banner(
          "danger",
          "Could not request approval",
          err instanceof ApiError ? err.detail || err.code : "The server could not be reached.",
        ),
      );
    }
  };

  form.addEventListener("submit", (event) => {
    event.preventDefault();
    const amount = parseAmount(amountInput.value, options.paymentAmount);
    if (amountInput.value.trim() !== "" && amount === null) {
      errorHost.replaceChildren(banner("danger", "That amount is not a number", "Use digits and a decimal point, e.g. 120.00."));
      return;
    }
    if (reasonInput.value.trim().length < 3) {
      errorHost.replaceChildren(banner("danger", "A reason is required", "Write at least a few words: it is what an incident review reads."));
      reasonInput.focus();
      return;
    }
    void run({
      // Zero means "everything refundable" to the API; send it explicitly so the field's
      // emptiness is a deliberate choice rather than an omission.
      amount_minor: amount ?? 0,
      reason: reasonInput.value.trim(),
    });
  });

  window.requestAnimationFrame(() => amountInput.focus());
}

function parseAmount(raw: string, reference?: Amount): number | null {
  const trimmed = raw.trim();
  if (trimmed === "") return null;
  const scale = reference?.scale ?? 2;
  const cleaned = trimmed.replace(/[,\s]/g, "");
  if (!/^\d*(\.\d{0,})?$/.test(cleaned) || cleaned === "" || cleaned === ".") return null;
  const [whole = "0", fraction = ""] = cleaned.split(".");
  const padded = (fraction + "0".repeat(scale)).slice(0, scale);
  const value = Number(whole) * 10 ** scale + Number(padded || 0);
  return Number.isFinite(value) ? Math.round(value) : null;
}
