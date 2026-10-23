// The pieces of a saved payment method that more than one screen needs.
//
// The branding is deliberately drawn here rather than fetched: a provider's card brand is
// display metadata, and rendering a "Visa" glyph from a string we already hold is cheaper
// and safer than loading a logo from a third party. An unknown brand gets a neutral card
// icon, never an invented logo.

import { ApiError } from "../lib/api";
import { h } from "../lib/dom";
import { icon } from "../lib/icons";
import { moneyWithCode, safeExternalURL, type Amount } from "../lib/format";
import type { Ctx } from "../lib/types";
import { openDialog } from "../ui/dialog";
import { badge, btn, withBusy } from "../ui/kit";

export interface SavedMethod {
  id: string;
  provider: string;
  brand?: string;
  last4?: string;
  label?: string;
  display?: string;
  expiry_month?: number;
  expiry_year?: number;
  default?: boolean;
  created_at?: string;
  last_used_at?: string;
  last_failure_at?: string;
  last_failure_code?: string;
}

export interface SavedMethodsPayload {
  methods: SavedMethod[];
  has_default: boolean;
  off_session: { enabled: boolean; reason: string; provider?: string };
}

export interface PayableInvoice {
  id: string;
  number?: string;
  due: Amount;
}

interface GatewayMethod {
  id: string;
  name: string;
  icon?: string;
}

interface GatewayChoice {
  id: string;
  provider?: string;
  name: string;
  icon?: string;
  methods?: GatewayMethod[];
}

/** The one brand vocabulary this UI knows. Everything else is a card. */
export type BrandKey = "visa" | "mastercard" | "amex" | "discover" | "jcb" | "unionpay" | "card";

export function brandKey(brand: string | undefined): BrandKey {
  const b = (brand ?? "").trim().toLowerCase();
  if (b === "visa") return "visa";
  if (b === "mastercard" || b === "mc") return "mastercard";
  if (b === "amex" || b === "american express" || b === "americanexpress") return "amex";
  if (b === "discover") return "discover";
  if (b === "jcb") return "jcb";
  if (b === "unionpay" || b === "union pay") return "unionpay";
  return "card";
}

const BRAND_LABEL: Record<BrandKey, string> = {
  visa: "VISA",
  mastercard: "MC",
  amex: "AMEX",
  discover: "DISC",
  jcb: "JCB",
  unionpay: "UP",
  card: "CARD",
};

const BRAND_COLOUR: Record<BrandKey, string> = {
  visa: "#1a1f71",
  mastercard: "#eb001b",
  amex: "#006fcf",
  discover: "#f26f21",
  jcb: "#0b5c9e",
  unionpay: "#e21836",
  card: "#5b6470",
};

/** A small square brand mark, built from the text and colour already on the row. */
export function methodIcon(brand: string | undefined, size = 30): HTMLElement {
  const key = brandKey(brand);
  const el = h("span", { "aria-hidden": "true" }, BRAND_LABEL[key]);
  el.style.width = `${size}px`;
  el.style.height = `${Math.round(size * 0.66)}px`;
  el.style.background = BRAND_COLOUR[key];
  el.style.color = "#fff";
  el.style.borderRadius = "5px";
  el.style.display = "inline-flex";
  el.style.alignItems = "center";
  el.style.justifyContent = "center";
  el.style.fontSize = size >= 28 ? "10px" : "8px";
  el.style.fontWeight = "700";
  el.style.letterSpacing = "0.02em";
  el.style.flex = "0 0 auto";
  return el;
}

export function methodTitle(m: SavedMethod): string {
  const brand = (m.brand ?? "").trim();
  const brandName = brand ? brand.charAt(0).toUpperCase() + brand.slice(1) : "Card";
  if (m.last4) return `${brandName} ending ${m.last4}`;
  return m.label?.trim() || m.display?.trim() || "Saved payment method";
}

export function methodExpiry(m: SavedMethod): string {
  if (typeof m.expiry_month !== "number" || typeof m.expiry_year !== "number") return "";
  return `expires ${String(m.expiry_month).padStart(2, "0")}/${String(m.expiry_year).slice(-2)}`;
}

/** A pay dialog that names the method and never asks for a card number.
 *
 *  The number is entered on the provider's page. This dialog exists to make that explicit,
 *  to show the saved method that will be used for future renewals, and to keep the customer
 *  on our page until they have chosen to leave it. */
export function openPayDialog(ctx: Ctx, invoice: PayableInvoice, onDone?: () => void): void {
  const body = h("div", { class: "stack" });
  const footer = h("div", { class: "row-wrap" });
  let closed = false;

  const handle = openDialog({
    title: "Pay this invoice",
    subtitle: invoice.number ? `Invoice ${invoice.number}` : "Choose how to pay",
    body,
    footer,
    onClose: () => {
      closed = true;
    },
  });

  const cancel = btn("Cancel", { variant: "ghost" });
  cancel.addEventListener("click", () => handle.close());
  const go = btn(`Continue to payment · ${moneyWithCode(invoice.due)}`, {
    variant: "primary",
    icon: "arrowRight",
  }) as HTMLButtonElement;
  footer.append(cancel, go);

  const render = async (): Promise<void> => {
    body.replaceChildren();
    let saved: SavedMethod[] = [];
    let gateways: GatewayChoice[] = [];
    let policy = "Card details are entered on the payment provider's secure page.";
    try {
      const payload = await ctx.api.get<SavedMethodsPayload>("/api/v1/portal/payment-methods/saved", {
        cache: false,
      });
      saved = payload.methods ?? [];
      if (payload.off_session.enabled) {
        policy = "The default saved card is charged automatically when a renewal comes due.";
      }
    } catch (err) {
      if (!(err instanceof ApiError && (err.status === 404 || err.status === 501))) throw err;
    }
    try {
      const payload = await ctx.api.get<{ payment_methods?: GatewayChoice[] }>(
        "/api/v1/portal/payment-methods",
        { cache: false },
      );
      gateways = payload.payment_methods ?? [];
    } catch {
      gateways = [];
    }
    body.append(
      h(
        "div",
        { class: "stack-sm" },
        h(
          "p",
          { class: "page-sub" },
          `You are paying ${moneyWithCode(invoice.due)}. ${policy}`,
        ),
        saved.length > 0
          ? h(
              "div",
              { class: "portal-list" },
              ...saved.map((m) =>
                h(
                  "div",
                  { class: "portal-row" },
                  h(
                    "div",
                    { class: "portal-row-main" },
                    h(
                      "div",
                      { class: "portal-row-title" },
                      methodIcon(m.brand, 26),
                      h("span", null, methodTitle(m)),
                      m.default ? badge("Default", "accent", { dot: true }) : null,
                    ),
                    h(
                      "div",
                      { class: "portal-row-meta" },
                      methodExpiry(m) || "Saved card",
                      m.last4 ? h("span", { class: "mono" }, `•••• ${m.last4}`) : null,
                    ),
                  ),
                ),
              ),
            )
          : h("p", { class: "field-hint" }, "No card is saved. You can save one after paying."),
        gateways.length > 0
          ? h(
              "div",
              { class: "stack-sm" },
              h("p", { class: "field-label" }, "Pay with"),
              ...gateways.map((gateway) =>
                h(
                  "div",
                  { class: "portal-row" },
                  h(
                    "div",
                    { class: "portal-row-main" },
                    h(
                      "div",
                      { class: "portal-row-title" },
                      icon(gateway.icon === "wallet" ? "wallet" : gateway.icon === "globe" ? "globe" : "card", 18),
                      h("span", null, gateway.name),
                    ),
                    h(
                      "div",
                      { class: "portal-row-meta" },
                      (gateway.methods ?? [])
                        .map((method) => method.name)
                        .join(" · ") || "Card",
                    ),
                  ),
                ),
              ),
            )
          : null,
        h(
          "div",
          { class: "banner banner--info" },
          h(
            "span",
            null,
            h("strong", null, "The card form is on the provider's page."),
            " This panel stores only a reference, a brand, the last four digits and the expiry.",
          ),
        ),
      ),
    );
  };
  void render().catch((err) => {
    body.replaceChildren(h("p", { class: "page-sub" }, "The payment method could not be loaded."));
    ctx.toast("The payment options could not be loaded", { tone: "danger", detail: String(err) });
  });

  go.addEventListener("click", () =>
    void withBusy(ctx, go, async () => {
      let session: { redirect_url: string; amount: Amount; attempt_id: string };
      try {
        session = await ctx.api.post(`/api/v1/portal/invoices/${encodeURIComponent(invoice.id)}/checkout`, {});
      } catch (err) {
        const code = err instanceof ApiError ? err.code : "gateway_unavailable";
        ctx.toast(code === "already_settled" ? "This invoice is already paid" : "The payment could not be started", {
          tone: code === "already_settled" ? "success" : "danger",
          detail:
            code === "already_settled"
              ? "Nothing was charged. Reload the page to see the payment."
              : "The gateway is unavailable. Nothing has been charged; try again.",
        });
        return;
      }
      const target = safeExternalURL(session.redirect_url);
      if (!target) {
        ctx.toast("The payment page could not be opened", {
          tone: "danger",
          detail: "The gateway returned no usable link. Nothing has been charged.",
        });
        return;
      }
      if (!closed) handle.close();
      onDone?.();
      window.location.assign(target);
    }),
  );
}
