// Payment gateways — not yet built.
//
// The route and the navigation entry are real; the module is a placeholder so a half-landed
// feature is visible instead of a dead link. Replace this file with the real screen.

import { h } from "../lib/dom";
import type { Ctx } from "../lib/types";

export const meta = { title: "Payment gateways", crumb: "Gateways" };

export default async function gatewaysPage(_ctx: Ctx): Promise<Node> {
  return h(
    "div",
    { class: "page" },
    h(
      "div",
      { class: "empty-state" },
      h("p", { class: "empty-title" }, "Gateway configuration is not available in this build"),
      h(
        "p",
        { class: "empty-body" },
        "The screen is wired into the panel but its module has not shipped yet. Everything else still works.",
      ),
      h("a", { class: "btn", href: "/", "data-link": "true" }, "Back to the dashboard"),
    ),
  );
}
