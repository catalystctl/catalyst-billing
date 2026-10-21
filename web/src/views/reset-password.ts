// Password reset, completion half.
//
// The token travels in the query string because it arrives in an email link. It is used
// once, and a successful reset revokes every existing session — so the copy says that
// plainly instead of leaving someone to discover it on their other device.

import { ApiError } from "../lib/api";
import { fill, h, swap } from "../lib/dom";
import { brandMark, icon } from "../lib/icons";
import type { AuthCtx } from "../lib/types";
import { btn, field, input } from "../ui/kit";

export const meta = { title: "Choose a password" };

export default async function resetPasswordPage(ctx: AuthCtx): Promise<Node> {
  const token = ctx.query.get("token") ?? "";
  const host = h("div", { class: "auth-form" });
  const panel = h("div", { class: "auth-panel" });

  const shell = h(
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
        h("h2", null, "One new password. Every old session, closed."),
        h(
          "p",
          null,
          "Choosing a password here revokes every session that was signed in with the old one. That is the point: if someone else had access, they lose it now.",
        ),
      ),
      h("div", { class: "auth-foot" }, h("span", null, "Self-hosted"), h("span", null, "·"), h("span", null, "No third-party scripts")),
    ),
    h("div", { class: "auth-main" }, panel),
  );

  if (!token) {
    fill(
      panel,
      h(
        "div",
        { class: "auth-head" },
        h("h1", null, "This link is incomplete"),
        h("p", null, "A reset link needs its token. Request a new one and use the link exactly as it arrived."),
      ),
      h("a", { class: "btn btn--primary", href: "/forgot-password" }, "Request a new link"),
    );
    return shell;
  }

  const password = input({
    id: "new-password",
    type: "password",
    autocomplete: "new-password",
    placeholder: "At least 12 characters",
    required: true,
    autofocus: true,
  });
  const confirm = input({
    id: "confirm-password",
    type: "password",
    autocomplete: "new-password",
    placeholder: "Repeat the password",
    required: true,
  });
  const submit = btn("Set new password", { variant: "primary", type: "submit", block: true });
  const errorHost = h("div");

  const form = h(
    "form",
    { class: "auth-form", novalidate: true },
    errorHost,
    field({
      label: "New password",
      control: password,
      id: "new-password",
      hint: "Longer is better. A passphrase of four words beats a clever eight characters.",
    }),
    field({ label: "Confirm password", control: confirm, id: "confirm-password" }),
    submit,
  );

  form.addEventListener("submit", (event) => {
    event.preventDefault();
    void (async () => {
      errorHost.replaceChildren();
      if (password.value.length < 12) {
        swap(errorHost, h("div", { class: "banner banner--danger" }, icon("alertCircle", 15), "Use at least 12 characters."));
        return;
      }
      if (password.value !== confirm.value) {
        swap(errorHost, h("div", { class: "banner banner--danger" }, icon("alertCircle", 15), "The two passwords do not match."));
        return;
      }
      if (submit instanceof HTMLButtonElement) submit.disabled = true;
      try {
        await ctx.api.post(
          "/api/v1/auth/password-reset/complete",
          { token, new_password: password.value },
          { noCSRF: true, noIdempotency: true },
        );
        fill(
          panel,
          h(
            "div",
            { class: "auth-head" },
            h("h1", null, "Password changed"),
            h("p", null, "Every existing session has been signed out. Sign in with the new password."),
          ),
          h("a", { class: "btn btn--primary btn--block", href: "/login" }, "Sign in"),
        );
      } catch (err) {
        const message =
          err instanceof ApiError
            ? err.code === "weak_password"
              ? "That password is too weak. Use at least 12 characters."
              : err.code === "invalid_token"
                ? "This link is invalid, already used, or expired. Request a new one."
                : err.detail || "The password could not be changed."
            : "The server could not be reached.";
        swap(errorHost, h("div", { class: "banner banner--danger" }, icon("alertCircle", 15), message));
        if (err instanceof ApiError && err.code === "invalid_token") {
          host.appendChild(h("a", { class: "btn", href: "/forgot-password" }, "Request a new link"));
        }
      } finally {
        if (submit instanceof HTMLButtonElement) submit.disabled = false;
      }
    })();
  });

  swap(host, form);
  fill(
    panel,
    h(
      "div",
      { class: "auth-head" },
      h("h1", null, "Choose a new password"),
      h("p", null, "This link works once, and only for thirty minutes after it was sent."),
    ),
    host,
  );
  return shell;
}
