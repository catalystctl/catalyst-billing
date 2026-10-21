// Not found.

import { h } from "../lib/dom";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { btn, emptyState } from "../ui/kit";

export const meta = { title: "Not found" };

export default function notFoundPage(ctx: Ctx): Node {
  return h(
    "div",
    { class: "page" },
    h(
      "div",
      { class: "card" },
      emptyState({
        title: "That page does not exist",
        body: `Nothing is routed at ${ctx.route.path}. A stale bookmark or a mistyped link is the usual cause.`,
        icon: "search",
        action: h(
          "div",
          { class: "row" },
          btn("Back to dashboard", { href: ctx.session.user.is_staff ? "/" : "/portal", icon: "home" }),
          btn("Search", {
            icon: "search",
            onclick: () =>
              document.dispatchEvent(new KeyboardEvent("keydown", { key: "k", metaKey: true, bubbles: true })),
          }),
        ),
      }),
    ),
    h(
      "p",
      { class: "faint center" },
      icon("info", 12),
      " If you followed a link from inside the panel, that link is a bug — the route table and the navigation are generated from the same source.",
    ),
  );
}
