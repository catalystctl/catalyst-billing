// Automation — not yet built.
//
// The route and the navigation entry are real; this module is a placeholder so a screen that is
// still being written is visible rather than a dead link. Replace this file.

import { h } from "../lib/dom";
import type { Ctx } from "../lib/types";

export const meta = { title: "Automation", crumb: "Automation" };

export default async function automationPage(_ctx: Ctx): Promise<Node> {
  return h(
    "div",
    { class: "page" },
    h(
      "div",
      { class: "empty-state" },
      h("p", { class: "empty-title" }, "Automation is not available in this build"),
      h(
        "p",
        { class: "empty-body" },
        "Periodic jobs and when they run. The screen is wired into the panel but its module has not shipped yet.",
      ),
      h("a", { class: "btn", href: "/", "data-link": "true" }, "Back to the dashboard"),
    ),
  );
}
