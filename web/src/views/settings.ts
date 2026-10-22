// Account settings.
//
// Six sections, one page, a sticky index. Everything here is about the *account* — the billing
// screens are elsewhere — and every claim on it is something the API actually told us:
//
//   - The identity block comes from /api/v1/auth/me, which reports email, display name, roles
//     and email_verified. It does **not** report whether a second factor is confirmed, so this
//     page says that plainly instead of drawing a green "MFA on" badge from a guess.
//   - Enrolment returns the TOTP secret, not a QR image. The CSP forbids third-party requests
//     (`img-src`/`connect-src` are self), so a QR service is not an option; the secret is shown
//     in mono and can be copied.
//   - Recovery codes are returned exactly once, with `shown_once: true`. They are displayed in
//     a dialog that cannot be dismissed until the customer confirms they have saved them,
//     because a dialog that closes on Escape is how recovery codes get lost.
//
// The theme is not here: it lives in the top bar, where it is one click away on every screen.

import { ApiError } from "../lib/api";
import { h } from "../lib/dom";
import { dateTime, relative, titleCase } from "../lib/format";
import { icon, type IconName } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { openDialog } from "../ui/dialog";
import {
  badge,
  banner,
  btn,
  card,
  copyButton,
  field,
  input,
  kv,
  pageHead,
  withBusy,
} from "../ui/kit";

export const meta = { title: "Account settings", crumb: "Settings" };

interface MFASetup {
  secret: string;
  otpauth_url: string;
  digits: number;
  period: number;
  algorithm: string;
}

interface MFAConfirmation {
  recovery_codes: string[];
  shown_once: boolean;
}

interface Section {
  id: string;
  label: string;
  icon: IconName;
  body: HTMLElement;
}

export default async function settingsPage(ctx: Ctx): Promise<Node> {
  const sections: Section[] = [
    { id: "identity", label: "Identity", icon: "user", body: identitySection(ctx) },
    { id: "security", label: "Security", icon: "shield", body: securitySection(ctx) },
    { id: "password", label: "Password", icon: "lock", body: passwordSection() },
  ];
  if (ctx.can("apikey:manage")) {
    sections.push({ id: "api-keys", label: "API keys", icon: "key", body: apiKeysSection() });
  }
  sections.push(
    { id: "appearance", label: "Appearance", icon: "sun", body: appearanceSection() },
    { id: "session", label: "Session", icon: "clock", body: sessionSection(ctx) },
  );

  const links = new Map<string, HTMLAnchorElement>();
  const setActive = (id: string) => {
    for (const [key, link] of links) link.classList.toggle("is-active", key === id);
  };
  const nav = h("nav", { class: "settings-nav", "aria-label": "Settings sections" });
  for (const section of sections) {
    const link = h(
      "a",
      { class: "settings-link", href: `#${section.id}`, onclick: () => setActive(section.id) },
      icon(section.icon, 14),
      section.label,
    );
    links.set(section.id, link);
    nav.appendChild(link);
  }
  setActive(sections[0]?.id ?? "identity");

  const column = h(
    "div",
    { class: "stack" },
    ...sections.map((section) =>
      h(
        "section",
        { class: "settings-section", id: section.id },
        card({ title: section.label, body: section.body }),
      ),
    ),
  );

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Account settings",
      sub: "Who you are, how you sign in, and how long this session lasts.",
    }),
    h("div", { class: "settings-layout" }, nav, column),
  );
}

// ----------------------------------------------------------------- identity --

function identitySection(ctx: Ctx): HTMLElement {
  const user = ctx.session.user;
  const verified = user.email_verified;
  const noticeHost = h("div");

  const verify = btn("Send verification link", { size: "sm", icon: "mail" }) as HTMLButtonElement;
  verify.addEventListener("click", () => {
    void withBusy(ctx, verify, async () => {
      try {
        // The endpoint takes no body and answers 202 whether or not the address exists or is
        // already verified, so the copy cannot promise that a message was sent.
        await ctx.api.post("/api/v1/auth/verify/request", {});
        noticeHost.replaceChildren(
          banner(
            "success",
            "Verification link requested",
            "If this address still needs verifying, a link is on its way. It can be used once.",
          ),
        );
      } catch (err) {
        if (err instanceof ApiError && (err.status === 429 || err.code === "rate_limited")) {
          noticeHost.replaceChildren(
            banner("warning", "Too many requests", "Wait a minute before asking for another link."),
          );
          return;
        }
        noticeHost.replaceChildren(
          banner(
            "danger",
            "The link could not be requested",
            err instanceof ApiError ? err.detail || "The request was refused." : "The server could not be reached.",
          ),
        );
      }
    });
  });

  return h(
    "div",
    { class: "stack" },
    noticeHost,
    h(
      "div",
      null,
      kv(
        "Email",
        h(
          "span",
          { class: "row-wrap" },
          h("span", null, user.email),
          verified ? badge("Verified", "success", { dot: true }) : badge("Unverified", "warning", { dot: true }),
        ),
      ),
      kv("Display name", user.display_name || h("span", { class: "faint" }, "Not set")),
      kv(
        "Roles",
        user.roles.length > 0
          ? h(
              "span",
              { class: "row-wrap" },
              ...user.roles.map((role) => badge(titleCase(role), user.is_staff ? "accent" : "neutral")),
            )
          : h("span", { class: "faint" }, "No roles"),
      ),
      kv("Account", user.is_staff ? badge("Staff", "accent") : badge("Customer", "neutral")),
    ),
    verified
      ? h("p", { class: "field-hint" }, "Your address is verified. Receipts and payment reminders reach you.")
      : h(
          "div",
          { class: "stack-sm" },
          h(
            "p",
            { class: "field-hint" },
            "An unverified address may not receive receipts or payment reminders. The link expires, and requesting a new one invalidates the old.",
          ),
          h("div", { class: "row-wrap" }, verify),
        ),
  );
}

// ----------------------------------------------------------------- security --

function securitySection(ctx: Ctx): HTMLElement {
  const noticeHost = h("div");

  const enrol = btn("Set up an authenticator app", {
    variant: "primary",
    icon: "shieldCheck",
    size: "sm",
  }) as HTMLButtonElement;
  enrol.addEventListener("click", () => {
    void withBusy(ctx, enrol, async () => {
      let setup: MFASetup;
      try {
        setup = await ctx.api.post<MFASetup>("/api/v1/auth/mfa/enrol", {});
      } catch (err) {
        noticeHost.replaceChildren(mfaBanner(err, "Enrolment could not start"));
        return;
      }
      openEnrolment(ctx, setup, noticeHost);
    });
  });

  const disable = btn("Turn off two-factor", { variant: "ghost", icon: "x", size: "sm" }) as HTMLButtonElement;
  disable.addEventListener("click", () => openDisable(ctx, noticeHost));

  return h(
    "div",
    { class: "stack" },
    noticeHost,
    banner(
      "info",
      "This page cannot tell you whether a second factor is already on",
      "The identity endpoint does not report MFA state, so showing a status here would be a guess. Enrolling will tell you: the server refuses with “already enrolled” if one exists.",
    ),
    h(
      "div",
      { class: "stack-sm" },
      h(
        "p",
        { class: "page-sub" },
        "Two-factor authentication asks for a six-digit code from your authenticator app in addition to your password. Recovery codes get you back in if you lose the app.",
      ),
      h("div", { class: "row-wrap" }, enrol, disable),
    ),
  );
}

function openEnrolment(ctx: Ctx, setup: MFASetup, noticeHost: HTMLElement): void {
  const code = input({
    id: "mfa-code",
    inputmode: "numeric",
    maxlength: 6,
    autocomplete: "one-time-code",
    placeholder: "000000",
    class: "input--mono",
  });
  const errorHost = h("div");
  const confirm = btn("Confirm and turn on", { variant: "primary", type: "submit" }) as HTMLButtonElement;
  const cancel = btn("Cancel", { variant: "ghost" });

  const form = h(
    "form",
    { class: "stack", novalidate: true },
    h(
      "p",
      { class: "page-sub" },
      "Add this account to your authenticator app — by typing the setup secret below, or by copying the setup URI into an app that accepts one — then enter the six-digit code it shows.",
    ),
    h(
      "div",
      { class: "settings-secret" },
      h("span", { class: "field-label" }, "Setup secret"),
      h(
        "div",
        { class: "settings-secret-row" },
        h("code", { class: "code-block settings-secret-value" }, setup.secret),
        copyButton(ctx, setup.secret, "secret"),
      ),
      h(
        "span",
        { class: "field-hint" },
        `${setup.digits} digits, a new code every ${setup.period} seconds, ${setup.algorithm}. There is no QR image: the panel may not call an image service.`,
      ),
    ),
    h("div", { class: "row-wrap" }, copyButton(ctx, setup.otpauth_url, "setup URI")),
    field({ label: "Six-digit code", control: code, id: "mfa-code", hint: "From the app, after adding this account." }),
    errorHost,
  );

  const handle = openDialog({
    title: "Set up two-factor authentication",
    subtitle: "Nothing changes until you confirm a code.",
    body: form,
    footer: h("div", { class: "row spacer" }, cancel, confirm),
    // A request in flight must not be interrupted by Escape or a stray backdrop click.
    locked: () => confirm.disabled,
  });

  cancel.addEventListener("click", () => handle.close());

  form.addEventListener("submit", (event) => {
    event.preventDefault();
    void (async () => {
      errorHost.replaceChildren();
      const value = code.value.replace(/\s+/g, "");
      if (!/^\d{6}$/.test(value)) {
        errorHost.replaceChildren(
          banner("danger", "Enter the six-digit code", "The code is six digits, with no spaces."),
        );
        return;
      }
      confirm.disabled = true;
      let res: MFAConfirmation;
      try {
        res = await ctx.api.post<MFAConfirmation>("/api/v1/auth/mfa/confirm", { code: value });
      } catch (err) {
        errorHost.replaceChildren(mfaBanner(err, "The code could not be confirmed"));
        return;
      } finally {
        confirm.disabled = false;
      }
      // The dialog refuses to close while a request is in flight, so it is unlocked above
      // before closing here; otherwise a successful confirm would leave it on screen.
      handle.close();
      noticeHost.replaceChildren(
        banner(
          "success",
          "Two-factor authentication is on",
          "Keep your recovery codes somewhere safe: they are the only way in if you lose the app.",
        ),
      );
      ctx.toast("Two-factor authentication is on", {
        tone: "success",
        detail: "Store your recovery codes somewhere safe.",
      });
      showRecoveryCodes(ctx, res);
    })();
  });
}

function showRecoveryCodes(ctx: Ctx, res: MFAConfirmation): void {
  const codes = res.recovery_codes ?? [];
  const joined = codes.join("\n");

  const done = btn("I have saved them", { variant: "primary" }) as HTMLButtonElement;
  done.disabled = true;
  const saved = h("input", {
    type: "checkbox",
    onchange: () => {
      done.disabled = !saved.checked;
    },
  });

  const handle = openDialog({
    title: "Save your recovery codes",
    subtitle: res.shown_once
      ? "Shown once. They cannot be retrieved later."
      : "These codes are displayed now; copy them before closing.",
    body: h(
      "div",
      { class: "stack" },
      banner(
        "warning",
        "These codes will not be shown again",
        "Each code signs you in once if you lose your authenticator app. Store them somewhere you will still have when your phone is gone.",
      ),
      codes.length > 0
        ? h("pre", { class: "code-block settings-recovery" }, joined)
        : h(
            "p",
            { class: "page-sub" },
            "The server returned no recovery codes. Contact support before you rely on this second factor.",
          ),
      codes.length > 0
        ? h("div", { class: "row-wrap" }, copyButton(ctx, joined, "all codes"))
        : null,
      h("label", { class: "checkbox" }, saved, h("span", null, "I have saved these codes somewhere safe")),
    ),
    footer: h("div", { class: "row spacer" }, done),
    // Locked until acknowledged: closing this dialog is how the codes are lost.
    locked: () => !saved.checked,
  });

  done.addEventListener("click", () => handle.close());
}

// Built on openDialog rather than ctx.formDialog so this flow controls its own lock: the
// dialog is unlocked before it closes on success, and a request in flight still cannot be
// dismissed. (The shared form dialog keeps its own lock while the submit button is disabled.)
function openDisable(ctx: Ctx, noticeHost: HTMLElement): void {
  const password = input({ id: "mfa-disable-password", type: "password", autocomplete: "current-password" });
  const code = input({
    id: "mfa-disable-code",
    inputmode: "numeric",
    maxlength: 6,
    autocomplete: "one-time-code",
    placeholder: "000000",
    class: "input--mono",
  });
  const errorHost = h("div");
  const submit = btn("Turn off", { variant: "danger-solid", type: "submit" }) as HTMLButtonElement;
  const cancel = btn("Cancel", { variant: "ghost" });

  const form = h(
    "form",
    { class: "stack", novalidate: true },
    field({ label: "Password", control: password, id: "mfa-disable-password", required: true }),
    field({
      label: "Current code",
      control: code,
      id: "mfa-disable-code",
      required: true,
      hint: "From your authenticator app, or one of your recovery codes.",
    }),
    banner("warning", "This lowers the security of your account", "Only your password will be needed to sign in."),
    errorHost,
  );

  const handle = openDialog({
    title: "Turn off two-factor authentication",
    subtitle: "Your password and a current code are both required.",
    body: form,
    footer: h("div", { class: "row spacer" }, cancel, submit),
    locked: () => submit.disabled,
  });

  cancel.addEventListener("click", () => handle.close());

  form.addEventListener("submit", (event) => {
    event.preventDefault();
    void (async () => {
      errorHost.replaceChildren();
      const passwordValue = password.value;
      const codeValue = code.value.replace(/\s+/g, "");
      if (!passwordValue || !codeValue) {
        errorHost.replaceChildren(
          banner("danger", "Both fields are required", "Enter your password and a current code."),
        );
        return;
      }
      submit.disabled = true;
      try {
        await ctx.api.post("/api/v1/auth/mfa/disable", { password: passwordValue, code: codeValue });
      } catch (err) {
        errorHost.replaceChildren(mfaBanner(err, "The second factor could not be turned off"));
        return;
      } finally {
        submit.disabled = false;
      }
      handle.close();
      noticeHost.replaceChildren(
        banner("success", "Two-factor authentication is off", "Only your password is needed to sign in now."),
      );
      ctx.toast("Two-factor authentication is off", { tone: "success" });
    })();
  });
}

/** One plain sentence per MFA failure code. */
function mfaBanner(err: unknown, fallbackTitle: string): HTMLElement {
  if (!(err instanceof ApiError)) {
    return banner("danger", fallbackTitle, "The server could not be reached. Nothing has changed.");
  }
  switch (err.code) {
    case "mfa_already_enrolled":
      return banner(
        "warning",
        "A second factor is already confirmed",
        "Turn it off below before enrolling a new one. The server will not replace it silently.",
      );
    case "mfa_not_pending":
      return banner("warning", "There is no enrolment to confirm", "Start the setup again from the button above.");
    case "mfa_not_enrolled":
      return banner("info", "No second factor is enrolled", "There is nothing to turn off on this account.");
    case "mfa_unavailable":
      return banner(
        "danger",
        "MFA is not configured on this server",
        "Enrolment cannot work here. Contact support; do not retry.",
      );
    case "invalid_code":
      return banner("danger", "That code is not valid", "Check the app and enter the current six digits.");
    case "invalid_credentials":
      return banner("danger", "The password is incorrect", "Nothing has changed.");
    case "rate_limited":
      return banner("warning", "Too many attempts", "Wait a minute before trying again.");
    default:
      return banner("danger", fallbackTitle, err.detail || err.code);
  }
}

// ----------------------------------------------------------------- password --

function passwordSection(): HTMLElement {
  return h(
    "div",
    { class: "stack-sm" },
    h(
      "p",
      { class: "page-sub" },
      "A password change goes through a reset link sent to your email address. There is no password field here, and no endpoint that accepts one without that link.",
    ),
    h("div", { class: "row-wrap" }, btn("Send a reset link", { href: "/forgot-password", icon: "mail", size: "sm" })),
    h(
      "div",
      { class: "banner banner--warning" },
      icon("alertTriangle", 15),
      h(
        "span",
        null,
        h("strong", null, "A reset revokes every session."),
        " Including this one. Every device signed in to this account has to sign in again.",
      ),
    ),
  );
}

// ----------------------------------------------------------------- api keys --

function apiKeysSection(): HTMLElement {
  return h(
    "div",
    { class: "stack-sm" },
    h(
      "p",
      { class: "page-sub" },
      "API keys let automation act as you, narrowed by the scopes you pick. A key is shown once, when it is created, and cannot be read again.",
    ),
    h("div", { class: "row-wrap" }, btn("Manage API keys", { href: "/api-keys", icon: "key", size: "sm" })),
  );
}

// --------------------------------------------------------------- appearance --

function appearanceSection(): HTMLElement {
  return h(
    "div",
    { class: "stack-sm" },
    h(
      "p",
      { class: "page-sub" },
      "Light and dark are switched from the theme control in the top bar, on every screen. The choice is remembered in this browser rather than on the account.",
    ),
    h(
      "p",
      { class: "field-hint" },
      "A first visit follows your operating system's preference; after that the switch wins.",
    ),
  );
}

// ------------------------------------------------------------------ session --

function sessionSection(ctx: Ctx): HTMLElement {
  const expiresAt = ctx.session.expiresAt;
  return h(
    "div",
    { class: "stack" },
    h(
      "div",
      null,
      kv("Signed in as", h("span", { class: "mono" }, ctx.session.user.email)),
      kv(
        "Session expires",
        h("span", { class: "row-wrap" }, h("span", null, dateTime(expiresAt)), h("span", { class: "faint" }, `(${relative(expiresAt)})`)),
      ),
    ),
    h(
      "p",
      { class: "field-hint" },
      "Signing out revokes this session on the server; it cannot be resumed, and signing in again creates a new one. Use Sign out in the account menu to end it now.",
    ),
  );
}
