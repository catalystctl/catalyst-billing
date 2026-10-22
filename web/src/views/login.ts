// Sign in.
//
// The first screen is the one that decides whether the rest looks considered. Three rules:
// the form is the only thing asking for attention; an error appears next to the form, not in
// a toast that can be missed; and the second factor is a continuation of the same panel
// rather than a separate page, because a redirect in the middle of a login is where people
// abandon it.

import { ApiError } from "../lib/api";
import { fill, h, swap } from "../lib/dom";
import { tx } from "../lib/i18n";
import { brandMark, icon } from "../lib/icons";
import { loadSession } from "../lib/session";
import type { AuthCtx } from "../lib/types";
import { btn, field, input } from "../ui/kit";

export const meta = { title: "Sign in" };

// The page title is set by the shell from `meta`, which is a module-level constant and therefore
// cannot follow a language switch; a translated <title> needs a hook in the shell that does not
// exist yet. Everything the person reads on the page itself is translated.

/** A provider the server offers, from `GET /api/v1/auth/providers`. */
interface Provider {
  slug: string;
  name: string;
  /** Where the button goes. A full navigation, not an SPA route: the server has to set a state. */
  start_url: string;
}

/** What the server says when a social callback refused. The server sends a code, not a
 *  sentence, because the wording belongs to the screen — `account_exists` in particular has to
 *  explain that accounts are never merged, which is a decision rather than an error.
 *
 *  A function rather than a table, because a table is built once at import and would keep the
 *  language the module was first loaded in after the visitor switches.
 */
function socialError(code: string): string {
  switch (code) {
    case "account_exists":
      return tx(
        "login.social.account_exists",
        "An account with that email address already exists. Sign in with your password below, then link the provider from your security page — accounts are never merged automatically.",
      );
    case "no_account":
      return tx(
        "login.social.no_account",
        "No account is linked to that identity. Sign in with your password first, then link the provider from your security page.",
      );
    case "email_mismatch":
      return tx(
        "login.social.email_mismatch",
        "The provider's email address is not this account's, so it was not linked.",
      );
    case "email_unverified":
      return tx("login.social.email_unverified", "The provider did not confirm that email address, so it was not linked.");
    case "identity_taken":
      return tx("login.social.identity_taken", "That provider account is already linked to a different account.");
    case "state_invalid":
      return tx("login.social.state_invalid", "That sign-in link expired or was already used. Start again.");
    case "provider_denied":
      return tx("login.social.provider_denied", "The provider did not complete the sign-in.");
    case "provider_failed":
      return tx(
        "login.social.provider_failed",
        "The provider could not be reached. Try again, or sign in with your password.",
      );
    case "mfa_required":
      return tx(
        "login.social.mfa_required",
        "This account uses two-step verification. Sign in with your password so the code can be checked.",
      );
    case "account_disabled":
      return tx("login.social.account_disabled", "This account is disabled. Ask an administrator to re-enable it.");
    case "account_missing":
      return tx("login.social.account_missing", "The account this identity was linked to no longer exists.");
    default:
      return tx("login.social.unknown", "The sign-in could not be completed. Try again, or use your password.");
  }
}

export default async function loginPage(ctx: AuthCtx): Promise<Node> {
  const providers = await loadProviders(ctx);
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
      h("h2", null, tx("login.pitch.title", "Every invoice, every posting, one honest ledger.")),
      h(
        "p",
        null,
        tx(
          "login.pitch.body",
          "The operator panel for the billing engine: customers, invoices, services and the money that connects them.",
        ),
      ),
      h(
        "div",
        { class: "auth-points" },
        point(tx("login.pitch.point.postings", "Immutable postings, corrections by reversal")),
        point(tx("login.pitch.point.approval", "Dual approval on money that moves")),
        point(tx("login.pitch.point.audit", "An audit trail that names a person")),
      ),
    ),
    h(
      "div",
      { class: "auth-foot" },
      h("span", null, tx("auth.foot.self_hosted", "Self-hosted")),
      h("span", null, "·"),
      h("span", null, tx("auth.foot.no_scripts", "No third-party scripts")),
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

  /** Reports why a social callback came back to this screen.
   *
   *  The server sends a code, and this screen owns the sentence — including the one that
   *  matters most: an existing account is never merged, so the person is told to sign in with
   *  their password and link the provider themselves. */
  const showSocialNotice = (): void => {
    const code = ctx.query.get("social_error");
    if (!code) return;
    showError(socialError(code), code);
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
      placeholder: tx("login.password.placeholder", "Your password"),
      required: true,
    });
    const submit = btn(tx("login.submit", "Sign in"), { variant: "primary", type: "submit", block: true });

    const form = h(
      "form",
      { class: "auth-form", novalidate: true },
      errorHost,
      field({ label: tx("login.email.label", "Email"), control: email, id: "email" }),
      field({
        label: tx("login.password.label", "Password"),
        control: password,
        id: "password",
      }),
      submit,
      h(
        "div",
        { class: "auth-links" },
        h("a", { href: "/forgot-password" }, tx("login.forgot", "Forgot your password?")),
        // The way to create an account.
        //
        // `/register` has existed and worked as a public route for a while, and nothing on this
        // page pointed at it — so self-registration was reachable only by someone who already
        // knew the URL. A sign-in page with no way to sign up is the first thing a new visitor
        // hits and the last thing an operator would notice, because they already have an account.
        //
        // It is rendered unconditionally: whether a shop accepts registrations is the server's
        // decision (POST /api/v1/auth/register answers `registration_unavailable` when it does
        // not), and hiding the link on a guess would make the page lie in the other direction.
        h("a", { href: "/register" }, tx("login.create_account", "Create an account")),
      ),
    );

    form.addEventListener("submit", (event) => {
      event.preventDefault();
      void (async () => {
        errorHost.replaceChildren();
        if (!email.value.trim() || !password.value) {
          showError(tx("login.error.empty", "Enter your email and password."));
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
            showError(tx("login.error.no_session", "The session could not be established."), "session_missing");
            return;
          }
          ctx.onSignedIn(session);
        } catch (err) {
          if (err instanceof ApiError) {
            if (err.code === "too_many_attempts") {
              showError(tx("login.error.too_many", "Too many attempts. Wait a few minutes and try again."), err.code);
            } else if (err.code === "overloaded") {
              showError(tx("login.error.overloaded", "The server is busy. Try again in a moment."), err.code);
            } else if (err.code === "account_disabled") {
              showError(
                tx("login.error.account_disabled", "This account is disabled. Ask an administrator to re-enable it."),
                err.code,
              );
            } else {
              showError(tx("login.error.bad_credentials", "That email and password do not match an account."), err.code);
            }
          } else {
            showError(tx("login.error.unreachable", "The server could not be reached."));
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
        h("h1", null, tx("login.title", "Sign in")),
        h(
          "p",
          null,
          tx("login.subtitle", "Use the account your administrator created for you."),
        ),
      ),
      formHost,
      providerBlock(providers),
      h(
        "p",
        { class: "auth-switch" },
        tx("login.customer_note", "Customer? Your invoices are in the same portal."),
      ),
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
        "aria-label": tx("login.mfa.digit", "Digit {{n}}", { n: i + 1 }),
      }),
    );
    const codeRow = h("div", { class: "otp" }, ...digits);
    const recovery = input({
      id: "recovery",
      type: "text",
      autocomplete: "off",
      placeholder: tx("login.mfa.recovery_placeholder", "Recovery code"),
      class: "input--mono",
    });
    const submit = btn(tx("login.mfa.submit", "Verify"), { variant: "primary", type: "submit", block: true });
    let usingRecovery = false;

    const form = h(
      "form",
      { class: "auth-form", novalidate: true },
      errorHost,
      h(
        "div",
        { class: "field" },
        h("span", { class: "field-label" }, tx("login.mfa.code_label", "Authentication code")),
        codeRow,
      ),
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
          usingRecovery
            ? tx("login.mfa.use_authenticator", "Use an authenticator code")
            : tx("login.mfa.use_recovery", "Use a recovery code"),
        ),
        h(
          "button",
          { class: "linkish", type: "button", onclick: () => signIn() },
          tx("login.mfa.start_over", "Start over"),
        ),
      ),
    );

    const readCode = () => digits.map((d) => d.value).join("");
    const submitCode = async () => {
      const code = usingRecovery ? recovery.value.trim() : readCode();
      if (!usingRecovery && code.length !== 6) {
        showError(tx("login.mfa.error.six_digits", "Enter all six digits."));
        return;
      }
      if (!code) {
        showError(tx("login.mfa.error.recovery_required", "Enter your recovery code."));
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
          showError(tx("login.error.no_session", "The session could not be established."), "session_missing");
          return;
        }
        ctx.onSignedIn(session);
      } catch (err) {
        const message =
          err instanceof ApiError && err.code === "invalid_code"
            ? tx("login.mfa.error.invalid", "That code is not valid. Codes change every 30 seconds.")
            : err instanceof ApiError
              ? err.detail || tx("login.mfa.error.unverified", "The code could not be verified.")
              : tx("login.error.unreachable", "The server could not be reached.");
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
        h("h1", null, tx("login.mfa.title", "Two-step verification")),
        h(
          "p",
          null,
          tx("login.mfa.subtitle", "Enter the six-digit code from your authenticator app."),
        ),
      ),
      formHost,
      h(
        "p",
        { class: "auth-note" },
        tx(
          "login.mfa.note",
          "Your password was accepted. This step keeps the account safe if it is ever reused elsewhere.",
        ),
      ),
    );
    swap(formHost, form);
    window.requestAnimationFrame(() => digits[0]?.focus());
  };

  signIn();
  showSocialNotice();
  return page;
}

/** Fetches the configured providers.
 *
 *  A failure is not an error the person can act on, and it must not stop them signing in with
 *  a password, so it degrades to "no providers" — which is the same thing the screen shows on
 *  an installation that has configured none. */
async function loadProviders(ctx: AuthCtx): Promise<Provider[]> {
  try {
    const res = await ctx.api.get<{ data: Provider[] }>("/api/v1/auth/providers");
    return Array.isArray(res.data) ? res.data : [];
  } catch {
    return [];
  }
}

/** Renders the provider buttons, or nothing at all when none is configured. */
function providerBlock(providers: Provider[]): Node | null {
  if (providers.length === 0) return null;
  return h(
    "div",
    { class: "stack-sm", style: "margin-top: var(--s-3)" },
    h(
      "div",
      {
        class: "auth-links",
        style: "justify-content: center; color: var(--text-faint)",
      },
      h("span", null, tx("login.providers.or", "or continue with")),
    ),
    h(
      "div",
      { class: "stack-sm" },
      ...providers.map((provider) =>
        btn(provider.name, {
          href: provider.start_url,
          icon: "external",
          block: true,
          title: tx("login.providers.with", "Sign in with {{provider}}", { provider: provider.name }),
        }),
      ),
    ),
  );
}

function point(text: string): HTMLElement {
  return h("div", { class: "auth-point" }, icon("check", 14), h("span", null, text));
}
