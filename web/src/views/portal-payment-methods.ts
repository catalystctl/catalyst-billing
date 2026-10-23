// Saved payment methods — the customer's side of the mandate (PAY-10, PAY-11).
//
// There is still no card form on this page, and that is the point. The number is entered on
// the provider's page, which returns a reusable reference; this panel stores the reference and
// the display metadata (brand, last four, expiry) and never sees the card itself.
//
// The page does four things the previous version could only explain it could not do: add a
// method through the provider's hosted flow, list what is saved, choose the default that a
// renewal will charge, and remove one. It also reports, honestly, whether this installation
// can charge a saved method without the customer present.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateTime } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { badge, banner, btn, card, emptyState, pageHead, resource, skeletonTable, withBusy } from "../ui/kit";
import {
  methodExpiry,
  methodIcon,
  methodTitle,
  type SavedMethod,
  type SavedMethodsPayload,
} from "./payment-methods";

export const meta = { title: "Payment methods", crumb: "Payment methods" };

interface State extends SavedMethodsPayload {
  supported: boolean;
}

export default async function portalPaymentMethodsPage(ctx: Ctx): Promise<Node> {
  const host = h("div", { class: "stack" });
  const paint = (): void => {
    swap(
      host,
      resource(ctx, (signal) => load(ctx, signal), (state) => render(ctx, state, paint), {
        skeleton: () => skeletonTable(2, 3),
      }),
    );
  };
  paint();
  return host;
}

async function load(ctx: Ctx, signal: AbortSignal): Promise<State> {
  // Returning from the provider: confirm the mandate and save the method it produced before
  // listing, and remove the query parameter so a refresh does not re-confirm.
  const pending = ctx.route.query.get("agreement");
  if (pending) {
    try {
      await ctx.api.get(`/api/v1/portal/payment-methods/agreements/${encodeURIComponent(pending)}`, {
        cache: false,
        signal,
      });
      ctx.replaceQuery({ agreement: null });
      ctx.api.invalidate("/api/v1/portal/payment-methods");
      ctx.toast("Payment method saved", {
        tone: "success",
        detail: "The card is stored by the payment provider. Its brand and last four appear here.",
      });
    } catch (err) {
      if (err instanceof DOMException && err.name === "AbortError") throw err;
      ctx.toast("The card was not saved", {
        tone: "danger",
        detail:
          err instanceof ApiError && err.status === 404
            ? "That saved-card request no longer exists."
            : "The payment provider did not confirm the card. Nothing has been charged; try again.",
      });
      ctx.replaceQuery({ agreement: null });
    }
  }

  try {
    const body = await ctx.api.get<SavedMethodsPayload>("/api/v1/portal/payment-methods/saved", {
      cache: false,
      signal,
    });
    return { supported: true, ...body, methods: body.methods ?? [] };
  } catch (err) {
    if (err instanceof ApiError && (err.status === 404 || err.status === 501)) {
      return {
        supported: false,
        methods: [],
        has_default: false,
        off_session: { enabled: false, reason: "this build cannot store payment methods" },
      };
    }
    throw err;
  }
}

function render(ctx: Ctx, state: State, repaint: () => void): Node {
  const add = addButton(ctx, state.supported);
  return h(
    "div",
    { class: "stack" },
    pageHead({
      title: "Payment methods",
      sub: "Cards are entered on the payment provider's page. This panel stores a reference, the brand, the last four digits and the expiry — never the number.",
      actions: h(
        "div",
        { class: "row-wrap" },
        add,
        btn("Back to overview", { href: "/portal", size: "sm", icon: "arrowLeft" }),
      ),
    }),
    offSessionNotice(state),
    state.methods.length === 0
      ? nothingStored(state.supported, add)
      : card({
          title: "Saved methods",
          sub: `${state.methods.length} on this account`,
          flush: true,
          body: h("div", { class: "portal-list" }, ...state.methods.map((row) => methodRow(ctx, row, repaint))),
          foot: h(
            "span",
            { class: "row" },
            icon("shield", 12),
            "The number is held by the payment provider. This panel stores a reference to it, never the card.",
          ),
        }),
  );
}

/** What the installation can actually do, rather than a promise the gateway cannot keep. */
function offSessionNotice(state: State): Node | null {
  if (state.off_session.enabled) {
    return banner(
      "success",
      "Renewals can be charged automatically",
      "The default card is charged when a renewal invoice comes due. If the card is declined, the invoice stays open and the usual reminder path takes over — nothing is suspended by the card alone.",
    );
  }
  return banner(
    "info",
    "Saved cards are not charged automatically",
    state.off_session.reason
      ? `You can still save a card, but a renewal will need paying by hand: ${state.off_session.reason}.`
      : "You can still save a card, but a renewal will need paying by hand on this installation.",
  );
}

function addButton(ctx: Ctx, supported: boolean): HTMLButtonElement {
  const el = btn("Add a card", {
    variant: "primary",
    size: "sm",
    icon: "plus",
    title: "Add a card through the payment provider's secure page",
  }) as HTMLButtonElement;
  if (!supported) {
    el.disabled = true;
    el.title = "This installation cannot store payment methods";
    return el;
  }
  el.addEventListener("click", () =>
    void withBusy(ctx, el, async () => {
      let started: { redirect_url?: string };
      try {
        started = await ctx.api.post("/api/v1/portal/payment-methods/agreements", {});
      } catch (err) {
        if (err instanceof ApiError && err.status === 404) {
          ctx.toast("Saved cards are not available", {
            tone: "danger",
            detail: "This build does not support storing a card. Pay an invoice on the provider's page instead.",
          });
          return;
        }
        ctx.toast("The card page could not be opened", {
          tone: "danger",
          detail:
            err instanceof ApiError && err.code === "gateway_unavailable"
              ? "The payment gateway is not configured. Nothing has been charged."
              : "The payment gateway is unavailable. Nothing has been charged; try again.",
        });
        return;
      }
      const target = started.redirect_url;
      if (!target) {
        ctx.toast("The card page could not be opened", {
          tone: "danger",
          detail: "The provider returned no usable link. Nothing has been charged.",
        });
        return;
      }
      window.location.assign(target);
    }),
  );
  return el;
}

function nothingStored(supported: boolean, add: HTMLButtonElement): HTMLElement {
  return card({
    flush: true,
    body: emptyState({
      title: supported ? "No saved payment methods" : "Nothing is stored on this installation",
      body: supported
        ? "Add a card to pay renewals without entering it every time. The card is entered on the payment provider's page, not here."
        : "This installation has no billing-agreement support, so there is no card to save and none is kept. Each payment is made on the provider's page.",
      icon: "card",
      action: h(
        "div",
        { class: "row-wrap" },
        add,
        btn("Pay an invoice", { href: "/portal/invoices", size: "sm", icon: "invoice" }),
      ),
    }),
  });
}

function methodRow(ctx: Ctx, row: SavedMethod, repaint: () => void): HTMLElement {
  const title = row.label?.trim() || methodTitle(row);
  const expiry = methodExpiry(row);
  const failure = row.last_failure_at
    ? `Refused ${dateTime(row.last_failure_at)}${row.last_failure_code ? ` (${row.last_failure_code})` : ""}`
    : "";
  const actions = h(
    "div",
    { class: "row-wrap" },
    row.default
      ? null
      : defaultButton(ctx, row.id, repaint),
    removeButton(ctx, row.id, title, repaint),
  );
  return h(
    "div",
    { class: "portal-row" },
    h(
      "div",
      { class: "portal-row-main" },
      h(
        "div",
        { class: "portal-row-title" },
        methodIcon(row.brand, 30),
        h("span", null, title),
        row.default ? badge("Default", "accent", { dot: true }) : null,
      ),
      h(
        "div",
        { class: "portal-row-meta" },
        expiry ? h("span", null, expiry) : null,
        row.last4 ? h("span", { class: "mono" }, `•••• ${row.last4}`) : null,
        row.created_at ? h("span", { class: "portal-row-dot" }, "·") : null,
        row.created_at ? h("span", { title: dateTime(row.created_at) }, `added ${dateTime(row.created_at)}`) : null,
        failure ? badge(failure, "danger") : null,
      ),
    ),
    h("div", { class: "portal-row-side" }, actions),
  );
}

function defaultButton(ctx: Ctx, id: string, repaint: () => void): HTMLButtonElement {
  const el = btn("Make default", { size: "sm" }) as HTMLButtonElement;
  el.addEventListener("click", () =>
    void withBusy(ctx, el, async () => {
      try {
        await ctx.api.post(`/api/v1/portal/payment-methods/saved/${encodeURIComponent(id)}/default`, {});
        ctx.api.invalidate("/api/v1/portal/payment-methods");
        ctx.toast("Default card changed", {
          tone: "success",
          detail: "Renewals will use this card when automatic charging is enabled.",
        });
        repaint();
      } catch {
        ctx.toast("The default card could not be changed", { tone: "danger" });
      }
    }),
  );
  return el;
}

function removeButton(ctx: Ctx, id: string, title: string, repaint: () => void): HTMLButtonElement {
  const el = btn("Remove", { size: "sm", variant: "ghost" }) as HTMLButtonElement;
  el.addEventListener("click", async () => {
    const ok = await ctx.confirm({
      title: `Remove ${title}?`,
      body: "The card is deleted from this panel and from the payment provider. A renewal that relied on it will need paying by hand until you save another one.",
      confirmLabel: "Remove card",
      danger: true,
    });
    if (!ok) return;
    void withBusy(ctx, el, async () => {
      try {
        await ctx.api.del(`/api/v1/portal/payment-methods/saved/${encodeURIComponent(id)}`);
        ctx.api.invalidate("/api/v1/portal/payment-methods");
        ctx.toast("Card removed", { tone: "success" });
        repaint();
      } catch {
        ctx.toast("The card could not be removed", { tone: "danger" });
      }
    });
  });
  return el;
}
