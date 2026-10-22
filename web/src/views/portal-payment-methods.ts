// Saved payment methods — and the honest state when there are none to save.
//
// The protocol has no charge method and no `create_billing_agreement`, so this installation
// cannot store a card and cannot charge one off-session. The backlog says so (PAY-10 is open, and
// `docs/paymenter-parity.md` lists `billing_agreements` as absent), and the payment flow reflects
// it: `POST /api/v1/portal/invoices/{id}/checkout` returns a gateway redirect URL, and the card
// is entered on the gateway's page every time.
//
// So this screen has exactly one job, and it is not to draw a card form. A card form on our own
// origin would pull the panel into PCI scope — the number would touch our DOM, our logs and our
// error reporting — to add a feature the backend cannot accept the output of. That is not a
// feature; it is an incident waiting for a bad afternoon.
//
// What it does instead:
//
//   - asks `GET /api/v1/portal/payment-methods`, which is the endpoint the billing-agreement work
//     will provide. If it answers with rows, they are listed (the shape is coded defensively:
//     a saved method needs only an id and a label to render).
//   - if it answers 404 — the likely case today — the page says plainly that nothing is stored,
//     explains that the card is entered at the payment page each time, and links to the invoices
//     that can be paid. A 404 is a fact about the build, so it is not an error state.
//   - if it answers with an empty list, it says the same thing, because "no saved methods" and
//     "no support for saved methods" lead the customer to the same next step.

import { ApiError } from "../lib/api";
import { h } from "../lib/dom";
import { dateTime } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { badge, banner, btn, card, emptyState, pageHead, resource, skeletonTable } from "../ui/kit";

export const meta = { title: "Payment methods", crumb: "Payment methods" };

/** One saved method. Only `id` and a human label are required: the billing-agreement protocol is
 *  not built yet, so this screen must not depend on a field name that has not been agreed. Every
 *  other field is read only when it is present. */
interface SavedMethod {
  id: string;
  brand?: string;
  last4?: string;
  label?: string;
  expiry_month?: number;
  expiry_year?: number;
  default?: boolean;
  created_at?: string;
}

interface MethodsPayload {
  data?: unknown;
  /** Some drafts of the endpoint answer `{methods: [...]}`; accept either rather than crash. */
  methods?: unknown;
}

export default async function portalPaymentMethodsPage(ctx: Ctx): Promise<Node> {
  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Payment methods",
      sub: "Cards are entered on the payment page, which is run by the payment provider. This panel never sees the number.",
      actions: btn("Back to overview", { href: "/portal", size: "sm", icon: "arrowLeft" }),
    }),
    resource(
      ctx,
      async (signal) => {
        try {
          const body = await ctx.api.get<MethodsPayload>("/api/v1/portal/payment-methods", { cache: false, signal });
          const raw = Array.isArray(body?.data) ? body.data : Array.isArray(body?.methods) ? body.methods : [];
          return { supported: true as const, rows: raw as SavedMethod[] };
        } catch (err) {
          // The endpoint does not exist on an installation without billing agreements. That is
          // the expected answer today, so it becomes "nothing is stored" rather than an error.
          if (err instanceof ApiError && (err.status === 404 || err.status === 501)) {
            return { supported: false as const, rows: [] as SavedMethod[] };
          }
          throw err;
        }
      },
      (data) => {
        if (data.rows.length === 0) return nothingStored(data.supported);
        return h(
          "div",
          { class: "stack" },
          card({
            title: "Saved methods",
            sub: `${data.rows.length} on this account`,
            flush: true,
            body: h("div", { class: "portal-list" }, ...data.rows.map((row) => methodRow(row))),
            foot: h(
              "span",
              { class: "row" },
              icon("shield", 12),
              "The number is held by the payment provider. This panel stores a reference to it, never the card.",
            ),
          }),
          payNote(),
        );
      },
      { skeleton: () => skeletonTable(2, 3) },
    ),
  );
}

/** The state that matters today: no stored methods, because the installation has no
 *  billing-agreement support (or because the customer has never saved one). The wording differs
 *  slightly between the two, and both point at the one action that exists. */
function nothingStored(supported: boolean): HTMLElement {
  return h(
    "div",
    { class: "stack" },
    card({
      flush: true,
      body: emptyState({
        title: supported ? "No saved payment methods" : "Nothing is stored on this installation",
        body: supported
          ? "Cards are entered at the payment page each time. If your provider supports saving one, it will be offered there, and it will appear here afterwards."
          : "This installation has no billing-agreement support, so there is no card to save and none is kept. Each payment is made on the provider's page, where you enter the card, and nothing about it is stored here.",
        icon: "card",
        action: h(
          "div",
          { class: "row-wrap" },
          btn("Pay an invoice", { href: "/portal/invoices", size: "sm", variant: "primary", icon: "invoice" }),
          btn("Back to overview", { href: "/portal", size: "sm", icon: "home" }),
        ),
      }),
    }),
    payNote(),
  );
}

/** Why there is no card form here. It is stated rather than implied, because "add a card" is the
 *  first thing a customer looks for on a page with this title. */
function payNote(): HTMLElement {
  return h(
    "div",
    { class: "stack" },
    banner(
      "info",
      "Why there is no card form on this page",
      "A card form here would put this panel in the path of the card number — in its forms, its logs and its error reports — and the backend has no way to charge a stored card. Payments are made on the provider's page, which is the part of the flow that is allowed to see a card.",
    ),
    card({
      title: "How paying works",
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "p",
          { class: "page-sub" },
          "Open an invoice and choose Pay. The panel asks the server for a payment session and sends you to the provider's page with the amount already set — the amount comes from the invoice, never from the browser.",
        ),
        h(
          "p",
          { class: "field-hint" },
          "When the provider confirms the payment, the invoice is marked paid here and a receipt is emailed. You are returned to the invoice afterwards.",
        ),
        h("div", { class: "row-wrap" }, btn("See your invoices", { href: "/portal/invoices", size: "sm", icon: "invoice" })),
      ),
    }),
  );
}

/** One saved method. Rendered from whatever the row carries: a brand and last four if it has
 *  them, its own label otherwise, and the id as a last resort. */
function methodRow(row: SavedMethod): HTMLElement {
  const title = row.label?.trim() || describeMethod(row);
  const expiry =
    typeof row.expiry_month === "number" && typeof row.expiry_year === "number"
      ? `expires ${String(row.expiry_month).padStart(2, "0")}/${String(row.expiry_year).slice(-2)}`
      : "";
  return h(
    "div",
    { class: "portal-row" },
    h(
      "div",
      { class: "portal-row-main" },
      h(
        "div",
        { class: "portal-row-title" },
        icon("card", 13),
        h("span", null, title),
        row.default ? badge("Default", "accent", { dot: true }) : null,
      ),
      h(
        "div",
        { class: "portal-row-meta" },
        expiry ? h("span", null, expiry) : null,
        row.created_at ? h("span", { class: "portal-row-dot" }, "·") : null,
        row.created_at ? h("span", { title: dateTime(row.created_at) }, `added ${dateTime(row.created_at)}`) : null,
      ),
    ),
    h(
      "div",
      { class: "portal-row-side" },
      // No remove button: there is no documented endpoint to delete a saved method, and a button
      // that 404s is worse than a sentence explaining where to change it.
      h("span", { class: "faint nowrap" }, "Managed at the provider"),
    ),
  );
}

function describeMethod(row: SavedMethod): string {
  const brand = row.brand?.trim();
  const last4 = row.last4?.trim();
  if (brand && last4) return `${brand} ending ${last4}`;
  if (last4) return `Card ending ${last4}`;
  if (brand) return brand;
  return "Saved payment method";
}
