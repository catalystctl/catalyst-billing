// Password reset, request half.
//
// The response is deliberately uniform: this page cannot tell you whether an address has an
// account, and neither can the API. Saying "no such user" here would turn the form into a
// customer-list oracle.

import { ApiError } from "../lib/api";
import { mountCaptcha, type CaptchaWidget } from "../lib/captcha";
import { h, swap } from "../lib/dom";
import { brandMark, icon } from "../lib/icons";
import type { AuthCtx } from "../lib/types";
import { btn, field, input } from "../ui/kit";

export const meta = { title: "Reset password", crumb: "Reset password" };

export default async function forgotPasswordPage(ctx: AuthCtx): Promise<Node> {
  const host = h("div", { class: "auth-form" });
  const email = input({
    id: "email",
    type: "email",
    autocomplete: "username",
    placeholder: "you@company.com",
    required: true,
    autofocus: true,
  });
  const captchaHost = h("div", { class: "captcha-host" });
  const submit = btn("Send reset link", { variant: "primary", type: "submit", block: true });
  const errorHost = h("div");

  // The challenge renders nothing when no provider is configured, which is the common case.
  let captcha: CaptchaWidget | null = null;
  void (async () => {
    try {
      captcha = await mountCaptcha(ctx.api, captchaHost);
    } catch {
      // A reset page that refused to submit because a challenge script failed to load would be
      // worse than one that submits and is refused: the refusal carries a reason, and the
      // per-address rate limit is still in front of it.
    }
  })();

  const form = h(
    "form",
    { class: "auth-form", novalidate: true },
    errorHost,
    field({ label: "Email", control: email, id: "email", hint: "We will send a link that expires in 30 minutes." }),
    captchaHost,
    submit,
  );

  form.addEventListener("submit", (event) => {
    event.preventDefault();
    void (async () => {
      errorHost.replaceChildren();
      if (!email.value.trim()) {
        swap(errorHost, h("div", { class: "banner banner--danger" }, icon("alertCircle", 15), "Enter your email address."));
        return;
      }
      if (submit instanceof HTMLButtonElement) submit.disabled = true;
      try {
        await ctx.api.post(
          "/api/v1/auth/password-reset/request",
          {
            email: email.value.trim(),
            // Null when no challenge is configured, which is what the server expects then.
            captcha_token: captcha?.token() ?? undefined,
          },
          { noCSRF: true, noIdempotency: true },
        );
        swap(
          host,
          h(
            "div",
            { class: "stack" },
            h("div", { class: "banner banner--success" }, icon("checkCircle", 15), h("strong", null, "Check your inbox")),
            h(
              "p",
              { class: "page-sub" },
              "If that address belongs to an account, a reset link is on its way. The link can only be used once.",
            ),
            h("a", { class: "btn", href: "/login" }, "Back to sign in"),
          ),
        );
      } catch (err) {
        const message =
          err instanceof ApiError
            ? err.code === "too_many_requests" || err.status === 429
              ? "Too many reset requests. Try again later."
              : err.detail || "The request could not be sent."
            : "The server could not be reached.";
        swap(errorHost, h("div", { class: "banner banner--danger" }, icon("alertCircle", 15), message));
      } finally {
        if (submit instanceof HTMLButtonElement) submit.disabled = false;
      }
    })();
  });

  swap(host, form);

  return h(
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
        h("h2", null, "A reset link that only works once."),
        h(
          "p",
          null,
          "The link is single use, expires in thirty minutes, and signing in anywhere else afterwards revokes every other session.",
        ),
      ),
      h("div", { class: "auth-foot" }, h("span", null, "Self-hosted"), h("span", null, "·"), h("span", null, "No third-party scripts")),
    ),
    h(
      "div",
      { class: "auth-main" },
      h(
        "div",
        { class: "auth-panel" },
        h(
          "div",
          { class: "auth-head" },
          h("h1", null, "Reset your password"),
          h("p", null, "Enter the email address on the account and we will send a link."),
        ),
        host,
        h("p", { class: "auth-switch" }, h("a", { href: "/login" }, "Back to sign in")),
      ),
    ),
  );
}
