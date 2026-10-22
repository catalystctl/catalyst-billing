// The customer's services.
//
// `GET /api/v1/portal/services` answers `{services: [...]}` where each row is `serviceJSON`:
// id, name, status, actions, and next_bill_at when there is a billing schedule. `actions` is
// computed by the server from the same rules the action handlers enforce, so this page can
// promise only what the server would accept — a "Cancel" that the API refuses is a broken
// promise, not a missing feature.

import { h } from "../lib/dom";
import { dateOnly, plural } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { btn, card, emptyState, pageHead, resource, skeletonTable, statusBadge } from "../ui/kit";
import { portalLoadError, type PortalService } from "./portal";

export const meta = { title: "Services", crumb: "Services" };

interface ServiceList {
  services: PortalService[];
}

export default async function portalServicesPage(ctx: Ctx): Promise<Node> {
  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Services",
      sub: "What you have with us, when it renews, and what you can change yourself.",
      actions: btn("Back to overview", { href: "/portal", size: "sm", icon: "arrowLeft" }),
    }),
    resource(
      ctx,
      async (signal) => {
        try {
          return await ctx.api.get<ServiceList>("/api/v1/portal/services", { cache: false, signal });
        } catch (err) {
          throw portalLoadError(err);
        }
      },
      (data) => {
        const services = data.services ?? [];
        return card({
          title: "Your services",
          sub: services.length > 0 ? plural(services.length, "service") : undefined,
          flush: true,
          body:
            services.length === 0
              ? emptyState({
                  title: "No services yet",
                  body: "When you buy something, it appears here with its renewal date.",
                  icon: "server",
                  action: btn("Back to overview", { href: "/portal", size: "sm" }),
                })
              : h("div", null, ...services.map((svc) => serviceRow(svc))),
          foot:
            services.length > 0
              ? h(
                  "span",
                  { class: "row" },
                  icon("info", 12),
                  "Cancelling stops a service at the end of the period you have paid for, never before.",
                )
              : undefined,
        });
      },
      { skeleton: () => skeletonTable(5, 4) },
    ),
  );
}

function serviceRow(svc: PortalService): HTMLElement {
  const href = `/portal/services/${encodeURIComponent(svc.id)}`;
  const action = svc.actions.includes("cancel")
    ? "Cancellation available"
    : svc.actions.includes("pay_to_restore")
      ? "Suspended — pay to restore"
      : null;

  return h(
    "div",
    { class: "portal-service" },
    h(
      "div",
      { class: "portal-service-main" },
      h(
        "div",
        { class: "portal-service-name" },
        h("a", { class: "row-title", href }, svc.name || "Service"),
        statusBadge(svc.status),
      ),
      h(
        "div",
        { class: "portal-service-meta" },
        h("span", null, svc.next_bill_at ? `Next bill ${dateOnly(svc.next_bill_at)}` : "No renewal scheduled"),
        action ? h("span", { class: "portal-row-dot" }, "·") : null,
        action ? h("span", null, action) : null,
      ),
    ),
    btn("Manage", { href, size: "sm", variant: "ghost", iconAfter: "chevronRight" }),
  );
}
