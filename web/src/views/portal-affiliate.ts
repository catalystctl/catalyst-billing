// Affiliate (customer portal).
//
// PLACEHOLDER — replaced by the portal-affiliate work. The route exists so the screen is reachable
// and the harness knows it by name before it is built; a missing route hides a feature, a
// placeholder route is an honest "not yet".
//
// What it will be: a customer who is an affiliate seeing their own referral link, who they referred,
// and what they have earned. `GET /api/v1/portal/affiliate` is served and, until this screen, read by
// nothing — so an affiliate could earn commission and had no way to see it.

import { h } from "../lib/dom";
import { pageHead, card } from "../ui/kit";
import type { Ctx } from "../lib/types";

export const meta = { title: "Affiliate", crumb: "Affiliate" };

export default async function portalAffiliatePage(_ctx: Ctx): Promise<Node> {
  return h(
    "div",
    { class: "stack" },
    pageHead({
      title: "Affiliate",
      sub: "Your referral link, who you referred, and what you have earned.",
    }),
    card({
      title: "Not built yet",
      sub: "This screen is a placeholder so the route and the navigation entry are real.",
      body: h(
        "p",
        { class: "muted" },
        "The affiliate view is not implemented. The API and this screen land together.",
      ),
    }),
  );
}
