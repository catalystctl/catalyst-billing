// Email verification.
//
// A GET, because the link in the email is a link. The endpoint is public: someone may open
// it on a device where they are not signed in, and the token is the proof.

import { ApiError } from "../lib/api";
import { h } from "../lib/dom";
import { brandMark, icon } from "../lib/icons";
import type { AuthCtx } from "../lib/types";
import { spinner } from "../ui/kit";

export const meta = { title: "Verify email" };

export default async function verifyPage(ctx: AuthCtx): Promise<Node> {
  const token = ctx.query.get("token") ?? "";
  const body = h("div", { class: "stack" }, h("div", { class: "row" }, spinner(), h("span", { class: "dim" }, "Checking your link…")));

  const panel = h(
    "div",
    { class: "auth-panel" },
    h("div", { class: "auth-head" }, h("h1", null, "Verify your email")),
    body,
  );

  const page = h(
    "div",
    { class: "auth" },
    h(
      "aside",
      { class: "auth-aside" },
      h(
        "div",
        { class: "auth-brand" },
        h("span", { class: "brand-mark" }, brandMark(18)),
        h("span", null, "Catalyst Billing"),
      ),
      h(
        "div",
        { class: "auth-pitch" },
        h("h2", null, "A verified address is how a receipt reaches you."),
        h("p", null, "Verification is not required to sign in. It is required before a receipt or a reset link can be trusted."),
      ),
      h("div", { class: "auth-foot" }, h("span", null, "Self-hosted"), h("span", null, "·"), h("span", null, "No third-party scripts")),
    ),
    h("div", { class: "auth-main" }, panel),
  );

  if (!token) {
    body.replaceChildren(
      h("div", { class: "banner banner--danger" }, icon("alertCircle", 15), "This link has no token."),
      h("a", { class: "btn", href: "/login" }, "Back to sign in"),
    );
    return page;
  }

  try {
    await ctx.api.get("/api/v1/auth/verify", { query: { token }, cache: false });
    body.replaceChildren(
      h(
        "div",
        { class: "banner banner--success" },
        icon("checkCircle", 15),
        h("div", { class: "stack-sm" }, h("strong", null, "Your address is verified"), h("span", null, "Receipts and security notices will reach you.")),
      ),
      h("a", { class: "btn btn--primary", href: ctx.api ? "/" : "/login" }, "Continue"),
    );
  } catch (err) {
    const message =
      err instanceof ApiError && err.code === "invalid_token"
        ? "This link is invalid, already used, or expired. Request a new one from your account settings."
        : err instanceof ApiError
          ? err.detail || "The address could not be verified."
          : "The server could not be reached.";
    body.replaceChildren(
      h("div", { class: "banner banner--danger" }, icon("alertCircle", 15), message),
      h("a", { class: "btn", href: "/settings" }, "Open account settings"),
    );
  }

  return page;
}
