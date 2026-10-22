// Billing details, in the customer's own hands.
//
// The form is `profileForm` from ./billing-profile — the same component the staff customer
// screen uses, pointed at the portal's own endpoint. Two doors, one form: a customer typing an
// address and an operator typing the same address must produce the same record, and the
// validation is the server's either way.

import { h } from "../lib/dom";
import type { Ctx } from "../lib/types";
import { profileForm } from "./billing-profile";

export const meta = { title: "Billing details", crumb: "Billing details" };

export default async function portalProfilePage(ctx: Ctx): Promise<Node> {
  return h(
    "div",
    { class: "page" },
    profileForm(ctx, { path: "/api/v1/portal/billing-profile", title: "Billing details" }),
  );
}
