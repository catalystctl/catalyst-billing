// Branding.
//
// PLACEHOLDER — replaced by the branding work. The route exists so the screen is reachable and the
// harness knows it by name before it is built.
//
// What it will be: a per-tenant brand — company name, logo, accent colour — that reaches the panel
// chrome and the invoice PDF. Deliberately not a theme registry: light and dark stay as they are,
// and an unset brand falls back to today's appearance byte-identically.

import { h } from "../lib/dom";
import { pageHead, card } from "../ui/kit";
import type { Ctx } from "../lib/types";

export const meta = { title: "Branding", crumb: "Branding" };

export default async function brandingPage(_ctx: Ctx): Promise<Node> {
  return h(
    "div",
    { class: "stack" },
    pageHead({
      title: "Branding",
      sub: "The name, logo and colour a customer sees on the panel and on their invoices.",
    }),
    card({
      title: "Not built yet",
      sub: "This screen is a placeholder so the route and the navigation entry are real.",
      body: h(
        "p",
        { class: "muted" },
        "Per-tenant branding is not implemented. The API and this screen land together.",
      ),
    }),
  );
}
