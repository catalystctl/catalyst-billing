// Cart — not yet built.
//
// This module exists so the route is real and the navigation entry resolves. The panel must not
// link to a screen that does not exist, and a missing chunk must not look like a crash; a
// placeholder that says so plainly is the honest state for a screen that is still being written.
//
// It is deliberately a whole module rather than a route-table omission: the route table is what
// the navigation is generated from, so declaring the route is what makes the menu entry appear
// and disappear correctly. Replace this file with the real screen.

import { h } from "../lib/dom";
import type { Ctx } from "../lib/types";

export const meta = { title: "Cart", crumb: "Your cart" };

export default async function cartPage(_ctx: Ctx): Promise<Node> {
  return h(
    "div",
    { class: "page" },
    h(
      "div",
      { class: "empty-state" },
      h("p", { class: "empty-title" }, "Cart is not available in this build"),
      h(
        "p",
        { class: "empty-body" },
        "The screen is wired into the panel but its module has not shipped yet. Everything else still works.",
      ),
      h("a", { class: "btn", href: "/", "data-link": "true" }, "Back to the dashboard"),
    ),
  );
}
