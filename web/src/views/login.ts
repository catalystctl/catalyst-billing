// Sign in.
//
// The first screen is the one that decides whether the rest looks considered. Three rules:
// the form is the only thing asking for attention; an error appears next to the form, not in
// a toast that can be missed; and the second factor is a continuation of the same panel
// rather than a separate page, because a redirect in the middle of a login is where people
// abandon it.

import { ApiError } from "../lib/api";
import { fill, h, swap } from "../lib/dom";
import { brandMark, icon } from "../lib/icons";
import { loadSession } from "../lib/session";
import type { AuthCtx } from "../lib/types";
import { btn, field, input } from "../ui/kit";

export const meta = { title: "Sign in" };

export default async function loginPage(ctx: AuthCtx): Promise<Node> {
  const formHost = h("div", { class: "auth-form" });
  const errorHost = h("div");

  const aside = h(
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
      h("h2", null, "Every invoice, every posting, one honest ledger."),
      h("p", null, "The operator panel for the billing engine: customers, invoices, services and the money that connects them."),
      h(
        "div",
        { class: "auth-points" },
        point("Immutable postings, corrections by reversal"),
        point("Dual approval on money that moves"),
        point("An audit trail that names a person"),
      ),
    ),
    h(
      "div",
      { class: "auth-foot" },
      h("span", null, "Self-hosted"),
      h("span", null, "·"),
      h("span", null, "No third-party scripts"),
    ),
  );

  const panel = h("div", { class: "auth-panel" });
  const page = h("div", { class: "auth" }, aside, h("div", { class: "auth-main" }, panel));

  const showError = (message: string, code?: string) => {
    swap(
      errorHost,
      h(
        "div",
        { class: "banner banner--danger", role: "alert" },
        icon("alertCircle", 15),
        h(
          "div",
          { class: "stack-sm" },
          h("strong", null, message),
          code ? h("span", { class: "mono", style: undefined }, code) : null,
        ),
      ),
    );
  };

  const signIn = (): void => {
    errorHost.replaceChildren();
    const email = input({
      id: "email",
      name: "email",
      type: "email",
      autocomplete: "username",
      placeholder: "you@company.com",
      required: true,
      autofocus: true,
    });
    const password = input({
      id: "password",
      name: "password",
      type: "password",
      autocomplete: "current-password",
      placeholder: "Your password",
      required: true,
    });
    const submit = btn("Sign in", { variant: "primary", type: "submit", block: true });

    const form = h(
      "form",
      { class: "auth-form", novalidate: true },
      errorHost,
      field({ label: "Email", control: email, id: "email" }),
      field({
        label: "Password",
        control: password,
        id: "password",
      }),
      submit,
      h(
        "div",
        { class: "auth-links" },
        h("a", { href: "/forgot-password" }, "Forgot your password?"),
        h("span", null, ""),
      ),
    );

    form.addEventListener("submit", (event) => {
      event.preventDefault();
      void (async () => {
        errorHost.replaceChildren();
        if (!email.value.trim() || !password.value) {
          showError("Enter your email and password.");
          return;
        }
        if (submit instanceof HTMLButtonElement) submit.disabled = true;
        try {
          const res = await ctx.api.post<{
            mfa_required?: boolean;
            challenge_token?: string;
            csrf_token?: string;
          }>(
            "/api/v1/auth/login",
            { email: email.value.trim(), password: password.value },
            { noCSRF: true, noIdempotency: true },
          );
          if (res.mfa_required && res.challenge_token) {
            showChallenge(res.challenge_token);
            return;
          }
          const session = await loadSession(ctx.api);
          if (!session) {
            showError("The session could not be established.", "session_missing");
            return;
          }
          ctx.onSignedIn(session);
        } catch (err) {
          if (err instanceof ApiError) {
            if (err.code === "too_many_attempts") {
              showError("Too many attempts. Wait a few minutes and try again.", err.code);
            } else if (err.code === "overloaded") {
              showError("The server is busy. Try again in a moment.", err.code);
            } else if (err.code === "account_disabled") {
              showError("This account is disabled. Ask an administrator to re-enable it.", err.code);
            } else {
              showError("That email and password do not match an account.", err.code);
            }
          } else {
            showError("The server could not be reached.");
          }
        } finally {
          if (submit instanceof HTMLButtonElement) submit.disabled = false;
        }
      })();
    });

    swap(formHost, form);
    fill(
      panel,
      h(
        "div",
        { class: "auth-head" },
        h("h1", null, "Sign in"),
        h("p", null, "Use the account your administrator created for you."),
      ),
      formHost,
      h("p", { class: "auth-switch" }, "Customer? Your invoices are in the same portal."),
    );
    window.requestAnimationFrame(() => email.focus());
  };

  const showChallenge = (challengeToken: string): void => {
    errorHost.replaceChildren();
    const digits = Array.from({ length: 6 }, (_, i) =>
      input({
        type: "text",
        inputmode: "numeric",
        maxlength: 1,
        autocomplete: i === 0 ? "one-time-code" : "off",
        class: "otp-digit",
        "aria-label": `Digit ${i + 1}`,
      }),
    );
    const codeRow = h("div", { class: "otp" }, ...digits);
    const recovery = input({
      id: "recovery",
      type: "text",
      autocomplete: "off",
      placeholder: "Recovery code",
      class: "input--mono",
    });
    const submit = btn("Verify", { variant: "primary", type: "submit", block: true });
    let usingRecovery = false;

    const form = h(
      "form",
      { class: "auth-form", novalidate: true },
      errorHost,
      h("div", { class: "field" }, h("span", { class: "field-label" }, "Authentication code"), codeRow),
      h("div", { class: "field", hidden: true }, recovery),
      submit,
      h(
        "div",
        { class: "auth-links" },
        h(
          "button",
          {
            class: "linkish",
            type: "button",
            onclick: () => {
              usingRecovery = !usingRecovery;
              const codeField = form.children[1] as HTMLElement | null;
              const recoveryField = form.children[2] as HTMLElement | null;
              if (codeField) codeField.hidden = usingRecovery;
              if (recoveryField) recoveryField.hidden = !usingRecovery;
              (usingRecovery ? recovery : digits[0])?.focus();
            },
          },
          usingRecovery ? "Use an authenticator code" : "Use a recovery code",
        ),
        h(
          "button",
          { class: "linkish", type: "button", onclick: () => signIn() },
          "Start over",
        ),
      ),
    );

    const readCode = () => digits.map((d) => d.value).join("");
    const submitCode = async () => {
      const code = usingRecovery ? recovery.value.trim() : readCode();
      if (!usingRecovery && code.length !== 6) {
        showError("Enter all six digits.");
        return;
      }
      if (!code) {
        showError("Enter your recovery code.");
        return;
      }
      if (submit instanceof HTMLButtonElement) submit.disabled = true;
      try {
        await ctx.api.post(
          "/api/v1/auth/mfa/verify",
          { challenge_token: challengeToken, code },
          { noCSRF: true, noIdempotency: true },
        );
        const session = await loadSession(ctx.api);
        if (!session) {
          showError("The session could not be established.", "session_missing");
          return;
        }
        ctx.onSignedIn(session);
      } catch (err) {
        const message =
          err instanceof ApiError && err.code === "invalid_code"
            ? "That code is not valid. Codes change every 30 seconds."
            : err instanceof ApiError
              ? err.detail || "The code could not be verified."
              : "The server could not be reached.";
        showError(message, err instanceof ApiError ? err.code : undefined);
        if (!usingRecovery) {
          for (const d of digits) d.value = "";
          digits[0]?.focus();
        }
      } finally {
        if (submit instanceof HTMLButtonElement) submit.disabled = false;
      }
    };

    digits.forEach((digit, i) => {
      digit.addEventListener("input", () => {
        const value = digit.value.replace(/\D/g, "").slice(-1);
        digit.value = value;
        if (value && i < digits.length - 1) digits[i + 1]?.focus();
      });
      digit.addEventListener("keydown", (event) => {
        if (event.key === "Backspace" && !digit.value && i > 0) digits[i - 1]?.focus();
      });
      digit.addEventListener("paste", (event) => {
        const text = event.clipboardData?.getData("text")?.replace(/\D/g, "") ?? "";
        if (!text) return;
        event.preventDefault();
        text.slice(0, 6).split("").forEach((char, j) => {
          const target = digits[j];
          if (target) target.value = char;
        });
        digits[Math.min(text.length, 5)]?.focus();
      });
    });

    form.addEventListener("submit", (event) => {
      event.preventDefault();
      void submitCode();
    });

    fill(
      panel,
      h(
        "div",
        { class: "auth-head" },
        h("h1", null, "Two-step verification"),
        h("p", null, "Enter the six-digit code from your authenticator app."),
      ),
      formHost,
      h("p", { class: "auth-note" }, "Your password was accepted. This step keeps the account safe if it is ever reused elsewhere."),
    );
    swap(formHost, form);
    window.requestAnimationFrame(() => digits[0]?.focus());
  };

  signIn();
  return page;
}

function point(text: string): HTMLElement {
  return h("div", { class: "auth-point" }, icon("check", 14), h("span", null, text));
}
