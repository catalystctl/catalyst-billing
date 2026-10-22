// Self-registration.
//
// A public page, so it receives `AuthCtx` rather than `Ctx` — the same context sign-in gets,
// with `onSignedIn` and no session. Registration deliberately does **not** sign the visitor in:
// `POST /api/v1/auth/register` answers 202 with `{status: "accepted", verification_required}`
// whether the account was created or the address already existed, because a form that answers
// differently for a duplicate is the cheapest account-discovery oracle there is. The
// consequence for this screen is the whole design:
//
//  - There is no "welcome, you are in" state to route to. Success is either "sign in with the
//    password you just chose" or "check your email", and the page says which.
//  - The copy never claims an account was created, because the response cannot confirm it. It
//    says the request was accepted, which is exactly what the server said.
//
// The form also refuses to be a worse oracle than the server: every refusal the endpoint
// documents (`registration_disabled`, `invalid_email`, `weak_password`, `tos_required`,
// `too_many_attempts`) is shown next to the form, because each one is actionable and none of
// them reveals whether a given address exists.

import { ApiError } from "../lib/api";
import { fill, h, swap } from "../lib/dom";
import { brandMark, icon } from "../lib/icons";
import type { AuthCtx } from "../lib/types";
import { btn, field, input } from "../ui/kit";

export const meta = { title: "Create an account" };

interface RegisterAccepted {
  status: string;
  /** Policy, not a fact about the address: the same for every caller, read from
   *  `mail.must_verify`. So showing it reveals nothing about any particular account. */
  verification_required: boolean;
}

export default async function registerPage(ctx: AuthCtx): Promise<Node> {
  const formHost = h("div", { class: "auth-form" });
  const errorHost = h("div");
  const panel = h("div", { class: "auth-panel" });

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
        h("h2", null, "Buy something, and keep the paperwork."),
        h(
          "p",
          null,
          "An account is where your invoices, your services and your payment history live. It takes a minute to create one.",
        ),
        h(
          "div",
          { class: "auth-points" },
          point("An invoice you can download and check"),
          point("Services that renew on a date you can see"),
          point("Cancel from the portal, without an email"),
        ),
      ),
      h(
        "div",
        { class: "auth-foot" },
        h("span", null, "Self-hosted"),
        h("span", null, "·"),
        h("span", null, "No third-party scripts"),
      ),
    ),
    h("div", { class: "auth-main" }, panel),
  );

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
          code ? h("span", { class: "mono" }, code) : null,
        ),
      ),
    );
  };

  // -------------------------------------------------------------- the form --

  const showForm = (): void => {
    errorHost.replaceChildren();

    const name = input({
      id: "name",
      name: "display_name",
      autocomplete: "name",
      placeholder: "Ada Lovelace",
      required: true,
      autofocus: true,
      maxlength: 200,
    });
    const email = input({
      id: "email",
      name: "email",
      type: "email",
      autocomplete: "email",
      placeholder: "you@company.com",
      required: true,
    });
    const password = input({
      id: "password",
      name: "password",
      type: "password",
      autocomplete: "new-password",
      placeholder: "At least 12 characters",
      required: true,
    });
    const tos = h("input", { type: "checkbox", id: "tos" });
    const submit = btn("Create account", { variant: "primary", type: "submit", block: true });

    const form = h(
      "form",
      { class: "auth-form", novalidate: true },
      errorHost,
      field({ label: "Name", control: name, id: "name" }),
      field({ label: "Email", control: email, id: "email", hint: "The invoice and the service receipt go here." }),
      field({
        label: "Password",
        control: password,
        id: "password",
        hint: "Twelve characters or more. Length beats punctuation.",
      }),
      h(
        "label",
        { class: "checkbox", for: "tos" },
        tos,
        h("span", null, "I accept this installation's terms of service."),
      ),
      submit,
      h(
        "div",
        { class: "auth-links" },
        h("a", { href: "/login" }, "Already have an account? Sign in"),
        h("span", null, ""),
      ),
    );

    form.addEventListener("submit", (event) => {
      event.preventDefault();
      void (async () => {
        errorHost.replaceChildren();
        const displayName = name.value.trim();
        const address = email.value.trim();
        if (!displayName) {
          showError("Enter the name to put on the account.");
          return;
        }
        if (!address) {
          showError("Enter an email address.");
          return;
        }
        if (!password.value) {
          showError("Choose a password.");
          return;
        }
        if (submit instanceof HTMLButtonElement) submit.disabled = true;
        try {
          const res = await ctx.api.post<RegisterAccepted>(
            "/api/v1/auth/register",
            {
              // The server's own field names, from handleRegister's request struct. There is
              // no customer id, no role and no verification flag: all three are the server's.
              email: address,
              password: password.value,
              display_name: displayName,
              accept_tos: tos.checked,
            },
            // A public entry point: there is no session to bind a CSRF token to, and the
            // handler enforces same-origin itself.
            { noCSRF: true, noIdempotency: true },
          );
          showAccepted(address, Boolean(res?.verification_required));
        } catch (err) {
          showError(registerMessage(err), err instanceof ApiError ? err.code : undefined);
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
        h("h1", null, "Create an account"),
        h("p", null, "One account holds your invoices, services and payment history."),
      ),
      formHost,
    );
    window.requestAnimationFrame(() => name.focus());
  };

  // --------------------------------------------------------- accepted state --

  const showAccepted = (address: string, verificationRequired: boolean): void => {
    const signIn = btn("Sign in", {
      variant: "primary",
      block: true,
      icon: "logoutBox",
      onclick: () => ctx.navigate(`/login?next=${encodeURIComponent("/portal")}`),
    });

    fill(
      panel,
      h(
        "div",
        { class: "auth-head" },
        h("h1", null, verificationRequired ? "Check your email" : "Check your email, then sign in"),
        h(
          "p",
          null,
          verificationRequired
            ? "One more step: confirm the address before you can sign in."
            : "Your request was accepted. Sign in with the password you just chose.",
        ),
      ),
      h(
        "div",
        { class: "stack" },
        h(
          "div",
          { class: "banner banner--info" },
          icon(verificationRequired ? "mail" : "checkCircle", 15),
          h(
            "div",
            { class: "stack-sm" },
            h(
              "strong",
              null,
              verificationRequired ? "A verification link is on its way" : "The account is ready to use",
            ),
            h(
              "span",
              null,
              verificationRequired
                ? `We sent a link to ${address}. It expires, so open it soon. If it does not arrive, check the spam folder and request another from the sign-in screen.`
                : `If ${address} was not already registered, the account exists now. If it was, nothing changed — signing in with your existing password still works.`,
            ),
          ),
        ),
        // The one sentence this page must not get wrong: the endpoint never confirms that a
        // new account was created, so the page does not claim it either.
        h(
          "p",
          { class: "field-hint" },
          "For privacy, this page does not say whether an address was already registered. If you already had an account, your existing password still works.",
        ),
        signIn,
      ),
    );
    window.requestAnimationFrame(() => {
      (panel.querySelector(".btn--primary") as HTMLElement | null)?.focus();
    });
  };

  showForm();
  return page;
}

/** Plain copy for the refusals `handleRegister` documents. A code that is not listed falls
 *  back to the server's detail, which is already written for a human. */
function registerMessage(err: unknown): string {
  if (!(err instanceof ApiError)) {
    return "The server could not be reached. Nothing was created — try again.";
  }
  switch (err.code) {
    case "registration_disabled":
      return "Registration is closed on this installation. Contact the operator for an account.";
    case "invalid_email":
      return "That does not look like an email address. Check it and try again.";
    case "invalid_display_name":
      return "A name is required, up to 200 characters.";
    case "weak_password":
      // The server names the rule it applied ("at least 12 characters"), so its detail is the
      // most useful sentence available.
      return err.detail || "That password is too weak. Use at least twelve characters.";
    case "tos_required":
      return "Tick the box to accept the terms of service.";
    case "too_many_attempts":
      return "Too many attempts from this address. Wait a few minutes and try again.";
    case "overloaded":
      return "The server is busy. Try again in a moment.";
    case "bad_origin":
      return "The request did not come from this site. Reload the page and try again.";
    case "registration_unavailable":
      return "Registration is not configured on this installation. Contact the operator.";
    case "storage_unavailable":
      return "This installation has no database configured, so accounts cannot be created.";
    default:
      return err.detail || "The account could not be created.";
  }
}

function point(text: string): HTMLElement {
  return h("div", { class: "auth-point" }, icon("check", 14), h("span", null, text));
}
