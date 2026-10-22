// One service, and the only mutation a customer may perform: schedule its cancellation.
//
// The rule the API enforces is the rule this page explains: cancelling stops the service at
// the end of the period that has already been paid for, never immediately. `portal.Service`
// says so in its comment ("taking it away early is both unfair and a refund event"), the
// confirm dialog says it before the click, and the response's own `message` and `ends_at` say
// it again afterwards — from the server, not from a string this file made up.
//
// A service that is not this customer's answers 404, and one this customer may not read
// answers 403. Both are rendered as "not available": the API makes them indistinguishable on
// purpose, and a page that turned one into a stack trace would leak the difference.

import { ApiError } from "../lib/api";
import { h } from "../lib/dom";
import { dateOnly, dateTime, shortId } from "../lib/format";
import type { Ctx } from "../lib/types";
import {
  banner,
  btn,
  card,
  emptyState,
  errorState,
  metaGrid,
  pageHead,
  skeletonPage,
  statusBadge,
  withBusy,
} from "../ui/kit";
import type { PortalService } from "./portal";

export const meta = { title: "Service", crumb: "Service" };

interface Cancellation {
  status: string;
  ends_at: string;
  message: string;
}

export default async function portalServicePage(ctx: Ctx): Promise<Node> {
  const id = ctx.route.params.id ?? "";
  const host = h("div", { class: "page" }, skeletonPage());
  let current: PortalService | null = null;
  let notice: Node | null = null;

  const load = async (): Promise<void> => {
    try {
      current = await ctx.api.get<PortalService>(`/api/v1/portal/services/${encodeURIComponent(id)}`, {
        cache: false,
      });
      render();
    } catch (err) {
      if (err instanceof ApiError && (err.status === 403 || err.status === 404)) {
        host.replaceChildren(unavailable());
        return;
      }
      host.replaceChildren(
        h(
          "div",
          { class: "stack" },
          pageHead({ title: "Service", actions: btn("Back to services", { href: "/portal/services", size: "sm", icon: "arrowLeft" }) }),
          errorState(err, () => {
            host.replaceChildren(skeletonPage());
            void load();
          }),
        ),
      );
    }
  };

  const render = (): void => {
    if (!current) return;
    const svc = current;
    const nodes: Node[] = [
      pageHead({
        title: svc.name || "Service",
        badge: statusBadge(svc.status),
        sub: `Service ${shortId(svc.id)}`,
        actions: btn("Back to services", { href: "/portal/services", size: "sm", icon: "arrowLeft" }),
      }),
    ];
    if (notice) nodes.push(notice);
    nodes.push(
      h(
        "div",
        { class: "split" },
        h(
          "div",
          { class: "stack" },
          card({
            title: "Service",
            body: metaGrid([
              { label: "Status", value: statusBadge(svc.status) },
              {
                label: "Next bill",
                value: svc.next_bill_at ? dateTime(svc.next_bill_at) : "No renewal scheduled",
              },
              { label: "Service id", value: h("span", { class: "mono" }, svc.id) },
            ]),
          }),
          actionCard(svc, async (button) => {
            const confirmed = await ctx.confirm({
              title: "Cancel this service?",
              body:
                `${svc.name || "This service"} keeps running until the end of the period you have already paid for` +
                (svc.next_bill_at ? `, and its next bill is due ${dateOnly(svc.next_bill_at)}` : "") +
                ". After that it stops, and nothing further is billed.",
              confirmLabel: "Schedule cancellation",
              cancelLabel: "Keep the service",
              danger: true,
              note: "You cannot undo this from the portal. Contact support if you change your mind.",
            });
            if (!confirmed) return;
            await withBusy(ctx, button, async () => {
              try {
                const res = await ctx.api.post<Cancellation>(
                  `/api/v1/portal/services/${encodeURIComponent(svc.id)}/cancel`,
                  {},
                );
                notice = banner(
                  "success",
                  "Cancellation scheduled",
                  h("span", null, `${res.message} Your service ends on ${dateTime(res.ends_at)}.`),
                );
                ctx.toast("Cancellation scheduled", {
                  tone: "success",
                  detail: `Your service ends on ${dateOnly(res.ends_at)}.`,
                });
                await load();
              } catch (err) {
                reportCancelError(ctx, err);
              }
            });
          }),
        ),
        card({
          title: "What you can do here",
          body: h(
            "div",
            { class: "stack-sm" },
            h(
              "p",
              { class: "page-sub" },
              "Cancelling schedules the end of this service at the end of the period you have paid for. The service keeps working until then.",
            ),
            h(
              "p",
              { class: "page-sub" },
              "Suspending, upgrading or terminating a service immediately is handled by support — the portal cannot take away time you have paid for.",
            ),
            h(
              "div",
              { class: "row-wrap" },
              btn("See your invoices", { href: "/portal/invoices", size: "sm", icon: "invoice" }),
              btn("Back to overview", { href: "/portal", size: "sm", icon: "arrowLeft" }),
            ),
          ),
        }),
      ),
    );
    host.replaceChildren(...nodes);
  };

  void load();
  return host;
}

/** The action card, chosen from the server-computed `actions` list. */
function actionCard(
  svc: PortalService,
  onCancel: (button: HTMLButtonElement) => Promise<void>,
): HTMLElement {
  if (svc.actions.includes("cancel")) {
    const button = btn("Cancel at period end", { variant: "danger", icon: "x" }) as HTMLButtonElement;
    button.addEventListener("click", () => void onCancel(button));
    return card({
      title: "Cancel this service",
      sub: "Runs to the end of the paid period",
      body: h(
        "div",
        { class: "stack" },
        h(
          "p",
          { class: "page-sub" },
          "Cancelling does not switch the service off now. It runs until the end of the period you have already paid for" +
            (svc.next_bill_at ? ` (next bill ${dateOnly(svc.next_bill_at)})` : "") +
            ", and then stops. Nothing further is billed.",
        ),
        h("div", { class: "row-wrap" }, button),
        h(
          "p",
          { class: "field-hint" },
          "This is the only change the portal can make to a service. Everything else is a support request.",
        ),
      ),
    });
  }

  if (svc.actions.includes("pay_to_restore")) {
    return card({
      title: "This service is suspended",
      body: h(
        "div",
        { class: "stack" },
        banner(
          "warning",
          "Suspended until the outstanding invoice is paid",
          "Paying the open invoice restores the service. The portal cannot restore it any other way.",
        ),
        h(
          "div",
          { class: "row-wrap" },
          btn("See what is owed", { href: "/portal/invoices", variant: "primary", icon: "invoice", size: "sm" }),
        ),
      ),
    });
  }

  return card({
    title: "No changes available",
    body: h(
      "div",
      { class: "stack-sm" },
      h(
        "p",
        { class: "page-sub" },
        svc.status === "active"
          ? "Cancellation is already scheduled for this service, so there is nothing left to change from here. It runs until the end of the period you have paid for."
          : "There is nothing you can change on this service from the portal. Contact support if you need something.",
      ),
      h(
        "div",
        { class: "row-wrap" },
        btn("See your invoices", { href: "/portal/invoices", size: "sm", icon: "invoice" }),
      ),
    ),
  });
}

function reportCancelError(ctx: Ctx, err: unknown): void {
  if (!(err instanceof ApiError)) {
    ctx.toast("The cancellation could not be scheduled", {
      tone: "danger",
      detail: "The server could not be reached. Nothing has changed.",
    });
    return;
  }
  switch (err.code) {
    case "not_found":
    case "forbidden":
      ctx.toast("This service is not available", {
        tone: "danger",
        detail: "It may belong to another account, or it may no longer exist.",
      });
      return;
    case "rate_limited":
      ctx.toast("Too many attempts", { tone: "danger", detail: "Wait a minute and try again." });
      return;
    default:
      ctx.toast("The cancellation could not be scheduled", {
        tone: "danger",
        detail: err.detail || err.code,
      });
  }
}

function unavailable(): Node {
  return h(
    "div",
    { class: "stack" },
    pageHead({
      title: "Service",
      actions: btn("Back to services", { href: "/portal/services", size: "sm", icon: "arrowLeft" }),
    }),
    card({
      flush: true,
      body: emptyState({
        title: "This service is not available",
        body: "It may have been cancelled, or it may belong to another account. Your services list has everything you can see.",
        icon: "lock",
        action: btn("Your services", { href: "/portal/services", size: "sm", icon: "server" }),
      }),
    }),
  );
}
