// Ticket fields.
//
// PLACEHOLDER — replaced by the ticket-fields work. It exists so the route, the nav entry and the
// browser harness know the screen by name before it is built, which is the honest state: the
// route table is generated from the routes, so a missing view is a dead link and a missing route
// is an invisible feature.
//
// What it will be: the definition of what a ticket asks for. A field is either required or
// optional, and an optional field may additionally be required *to close* — which is the
// distinction that makes a queue trustworthy, because "we always ask for the server name" is a
// policy that has to be enforced somewhere other than a person's memory.

import { h } from "../lib/dom";
import { pageHead, card } from "../ui/kit";
import type { Ctx } from "../lib/types";

export const meta = { title: "Ticket fields", crumb: "Ticket fields" };

export default async function ticketFieldsPage(_ctx: Ctx): Promise<Node> {
  return h(
    "div",
    { class: "stack" },
    pageHead({
      title: "Ticket fields",
      sub: "What a ticket asks for, and what must be answered before it can be closed.",
    }),
    card({
      title: "Not built yet",
      sub: "This screen is a placeholder so the route and the navigation entry are real.",
      body: h(
        "p",
        { class: "muted" },
        "Ticket custom fields are not implemented. The API and this screen land together.",
      ),
    }),
  );
}
