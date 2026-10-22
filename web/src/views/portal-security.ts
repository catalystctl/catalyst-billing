// The customer's security page.
//
// Modelled on web/src/views/settings.ts — a sticky index, one column of sections, every claim on
// the page something the API actually told us — but pointed at a customer's own account rather
// than at an operator's. The sections answer the four questions a customer has:
//
//  1. **Password.** `POST /api/v1/auth/password/change` takes the current password and the new
//     one. It revokes every *other* session and keeps this one, and the response says how many
//     went, so the screen can report it rather than implying it.
//  2. **Two-factor.** The same enrolment flow the staff settings page uses: enrol returns a TOTP
//     secret (never a QR image — the CSP forbids third-party requests), confirm returns recovery
//     codes exactly once, and the dialog holding them cannot be dismissed until they are
//     acknowledged. That flow is duplicated here rather than imported because `securitySection`
//     is not exported from settings.ts and that file belongs to another package; the *behaviour*
//     is the same on purpose, because a customer and an operator enrolling a factor must not get
//     two different dialogs.
//  3. **Sessions.** `GET /api/v1/portal/sessions` lists where the account is signed in, with one
//     row per session, a revoke per row and "sign out everywhere else". These endpoints are being
//     built in parallel, so a 404 renders "not available on this installation" and the rest of
//     the page keeps working — the honest state for a screen whose backing route may not exist
//     yet.
//  4. **Login history.** `GET /api/v1/portal/login-history` is the audit trail of sign-ins. Same
//     defensive treatment: 404 is a state, not an error.
//
// Nothing here draws a "two-factor is on" badge. `/api/v1/auth/me` does not report MFA state, so
// a green badge would be a guess; the page says so and lets the server refuse a double enrolment
// with `mfa_already_enrolled`, which is a fact.

import { ApiError, type Page } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateTime, relative } from "../lib/format";
import { icon, type IconName } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { openDialog } from "../ui/dialog";
import {
  badge,
  banner,
  btn,
  card,
  copyButton,
  emptyState,
  errorState,
  field,
  input,
  kv,
  pageHead,
  resource,
  skeletonTable,
  withBusy,
} from "../ui/kit";

export const meta = { title: "Security", crumb: "Security" };

// ---------------------------------------------------------------- payloads --

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

/** One session, as `sessionJSON` emits it. `current` is computed by the server against the
 *  caller's own session id, which is why this screen never has to guess which row is "this
 *  device". */
interface SessionRow {
  id: string;
  user_id: string;
  created_at: string;
  last_seen_at: string;
  expires_at: string;
  ip: string | null;
  user_agent: string;
  current: boolean;
  impersonated_by?: string | null;
}

interface LoginEvent {
  id: number;
  event: string;
  detail: string;
  ip: string | null;
  created_at: string;
}

interface Section {
  id: string;
  label: string;
  icon: IconName;
  body: HTMLElement;
}

export default async function portalSecurityPage(ctx: Ctx): Promise<Node> {
  const sections: Section[] = [
    { id: "password", label: "Password", icon: "lock", body: passwordSection(ctx) },
    { id: "two-factor", label: "Two-factor", icon: "shield", body: twoFactorSection(ctx) },
    { id: "sessions", label: "Sessions", icon: "wifi", body: sessionsSection(ctx) },
    { id: "history", label: "Login history", icon: "clock", body: historySection(ctx) },
  ];

  const links = new Map<string, HTMLAnchorElement>();
  const setActive = (id: string) => {
    for (const [key, link] of links) link.classList.toggle("is-active", key === id);
  };
  const nav = h("nav", { class: "settings-nav", "aria-label": "Security sections" });
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
  setActive(sections[0]?.id ?? "password");

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
      title: "Security",
      sub: "Your password, your second factor, and every device signed in to this account.",
      actions: btn("Back to overview", { href: "/portal", size: "sm", icon: "arrowLeft" }),
    }),
    h("div", { class: "settings-layout" }, nav, column),
  );
}

// ----------------------------------------------------------------- password --

function passwordSection(ctx: Ctx): HTMLElement {
  const noticeHost = h("div");
  const current = input({ id: "pw-current", type: "password", autocomplete: "current-password" });
  const next = input({ id: "pw-new", type: "password", autocomplete: "new-password" });
  const confirm = input({ id: "pw-confirm", type: "password", autocomplete: "new-password" });
  const errorHost = h("div");
  const submit = btn("Change password", { variant: "primary", type: "submit" }) as HTMLButtonElement;

  const form = h(
    "form",
    { class: "stack", novalidate: true },
    h(
      "div",
      { class: "grid-2" },
      field({ label: "Current password", control: current, id: "pw-current", required: true }),
      field({
        label: "New password",
        control: next,
        id: "pw-new",
        required: true,
        hint: "At least 12 characters. Length beats punctuation.",
      }),
      field({
        label: "Repeat the new password",
        control: confirm,
        id: "pw-confirm",
        required: true,
        hint: "Checked here so a typo does not lock you out.",
      }),
    ),
    banner(
      "warning",
      "Every other device is signed out",
      "Changing the password keeps this session and revokes the rest. That is the point of changing it when you suspect the old one leaked.",
    ),
    errorHost,
    h("div", { class: "row-wrap" }, submit),
  );

  form.addEventListener("submit", (event) => {
    event.preventDefault();
    void withBusy(ctx, submit, async () => {
      errorHost.replaceChildren();
      const currentValue = current.value;
      const nextValue = next.value;
      if (!currentValue || !nextValue) {
        errorHost.replaceChildren(banner("danger", "Both passwords are required", "The current one proves it is you."));
        return;
      }
      if (nextValue !== confirm.value) {
        errorHost.replaceChildren(banner("danger", "The new passwords do not match", "Retype the second one."));
        confirm.focus();
        return;
      }
      if (nextValue.length < 12) {
        // The server's bound is 12 (`authn.MinPasswordLength`); refusing here says so before a
        // round trip, and the server still refuses if this check is ever wrong.
        errorHost.replaceChildren(banner("danger", "That password is too short", "The minimum is 12 characters."));
        next.focus();
        return;
      }

      let revoked = 0;
      try {
        const result = await ctx.api.post<{ sessions_revoked?: number }>("/api/v1/auth/password/change", {
          current_password: currentValue,
          new_password: nextValue,
        });
        revoked = typeof result?.sessions_revoked === "number" ? result.sessions_revoked : 0;
      } catch (err) {
        errorHost.replaceChildren(passwordFailure(err));
        return;
      }

      form.reset();
      noticeHost.replaceChildren(
        banner(
          "success",
          "Password changed",
          revoked > 0
            ? `${revoked} other session${revoked === 1 ? " was" : "s were"} signed out. This one is still signed in.`
            : "No other device was signed in, so there was nothing to sign out.",
        ),
      );
      ctx.toast("Password changed", { tone: "success" });
    });
  });

  return h(
    "div",
    { class: "stack" },
    noticeHost,
    h(
      "p",
      { class: "page-sub" },
      "A password change needs the current password. If you have forgotten it, the reset link on the sign-in screen is the way back in — that path revokes every session, including this one.",
    ),
    form,
    h(
      "div",
      { class: "row-wrap" },
      btn("Forgotten your password?", { href: "/forgot-password", size: "sm", variant: "ghost", icon: "mail" }),
    ),
  );
}

function passwordFailure(err: unknown): HTMLElement {
  if (!(err instanceof ApiError)) {
    return banner("danger", "The password was not changed", "The server could not be reached. Nothing has changed.");
  }
  switch (err.code) {
    case "invalid_credentials":
      return banner("danger", "The current password is wrong", "Nothing has changed. Check it and try again.");
    case "weak_password":
      return banner("danger", "That password is too weak", err.detail || "The minimum is 12 characters.");
    case "rate_limited":
      return banner("warning", "Too many attempts", "Wait a minute before trying again.");
    default:
      return banner("danger", "The password was not changed", err.detail || err.code);
  }
}

// -------------------------------------------------------------- two-factor --

function twoFactorSection(ctx: Ctx): HTMLElement {
  const noticeHost = h("div");

  const enrol = btn("Set up an authenticator app", { variant: "primary", icon: "shieldCheck", size: "sm" }) as HTMLButtonElement;
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
      "The identity endpoint does not report MFA state, so a status here would be a guess. Enrolling will tell you: the server refuses with “already enrolled” if one exists.",
    ),
    h(
      "div",
      { class: "stack-sm" },
      h(
        "p",
        { class: "page-sub" },
        "Two-factor authentication asks for a six-digit code from your authenticator app as well as your password. Recovery codes get you back in if you lose the app — and they are shown once, at enrolment.",
      ),
      h("div", { class: "row-wrap" }, enrol, disable),
    ),
  );
}

function openEnrolment(ctx: Ctx, setup: MFASetup, noticeHost: HTMLElement): void {
  const code = input({
    id: "portal-mfa-code",
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
      "Add this account to your authenticator app — by typing the setup secret, or by copying the setup URI into an app that accepts one — then enter the six-digit code it shows.",
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
    field({ label: "Six-digit code", control: code, id: "portal-mfa-code", hint: "From the app, after adding this account." }),
    errorHost,
  );

  const handle = openDialog({
    title: "Set up two-factor authentication",
    subtitle: "Nothing changes until you confirm a code.",
    body: form,
    footer: h("div", { class: "row spacer" }, cancel, confirm),
    locked: () => confirm.disabled,
  });

  cancel.addEventListener("click", () => handle.close());

  form.addEventListener("submit", (event) => {
    event.preventDefault();
    void (async () => {
      errorHost.replaceChildren();
      const value = code.value.replace(/\s+/g, "");
      if (!/^\d{6}$/.test(value)) {
        errorHost.replaceChildren(banner("danger", "Enter the six-digit code", "The code is six digits, with no spaces."));
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
      handle.close();
      noticeHost.replaceChildren(
        banner(
          "success",
          "Two-factor authentication is on",
          "Keep your recovery codes somewhere safe: they are the only way in if you lose the app.",
        ),
      );
      ctx.toast("Two-factor authentication is on", { tone: "success", detail: "Store your recovery codes somewhere safe." });
      showRecoveryCodes(ctx, res);
    })();
  });
}

function showRecoveryCodes(ctx: Ctx, res: MFAConfirmation): void {
  const codes = Array.isArray(res.recovery_codes) ? res.recovery_codes : [];
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
    subtitle: res.shown_once ? "Shown once. They cannot be retrieved later." : "These codes are displayed now; copy them before closing.",
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
        : h("p", { class: "page-sub" }, "The server returned no recovery codes. Contact support before relying on this second factor."),
      codes.length > 0 ? h("div", { class: "row-wrap" }, copyButton(ctx, joined, "all codes")) : null,
      h("label", { class: "checkbox" }, saved, h("span", null, "I have saved these codes somewhere safe")),
    ),
    footer: h("div", { class: "row spacer" }, done),
    // Locked until acknowledged: closing this dialog is how the codes are lost.
    locked: () => !saved.checked,
  });

  done.addEventListener("click", () => handle.close());
}

function openDisable(ctx: Ctx, noticeHost: HTMLElement): void {
  const password = input({ id: "portal-mfa-disable-password", type: "password", autocomplete: "current-password" });
  const code = input({
    id: "portal-mfa-disable-code",
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
    field({ label: "Password", control: password, id: "portal-mfa-disable-password", required: true }),
    field({
      label: "Current code",
      control: code,
      id: "portal-mfa-disable-code",
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
        errorHost.replaceChildren(banner("danger", "Both fields are required", "Enter your password and a current code."));
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

/** One plain sentence per MFA failure code — the same mapping the staff settings page uses, so a
 *  customer and an operator read the same words for the same refusal. */
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
      return banner("danger", "MFA is not configured on this server", "Enrolment cannot work here. Contact support; do not retry.");
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

// ----------------------------------------------------------------- sessions --

function sessionsSection(ctx: Ctx): HTMLElement {
  // The session endpoints are being built in parallel with this screen. A 404 means "this
  // installation does not have them", which is a fact about the build, not a failure of the
  // request, so it is rendered as an explanation rather than as an error with a retry.
  return resource(
    ctx,
    async (signal) => {
      try {
        const body = await ctx.api.get<{ data?: unknown }>("/api/v1/portal/sessions", { cache: false, signal });
        return { rows: Array.isArray(body?.data) ? (body.data as SessionRow[]) : [], available: true };
      } catch (err) {
        if (err instanceof ApiError && (err.status === 404 || err.status === 501)) {
          return { rows: [] as SessionRow[], available: false };
        }
        throw err;
      }
    },
    (data) => {
      if (!data.available) {
        return emptyState({
          title: "Session management is not available on this installation",
          body: "This build has no endpoint for listing or revoking sessions. Changing your password still signs every other device out, which is the same protection by a different route.",
          icon: "lock",
        });
      }
      if (data.rows.length === 0) {
        return emptyState({
          title: "No active sessions listed",
          body: "The server reported no sessions for this account. If you are reading this, this session exists but is not in the list — a reload may fix it.",
          icon: "wifi",
        });
      }
      return sessionList(ctx, data.rows);
    },
    { skeleton: () => skeletonTable(3, 4) },
  );
}

function sessionList(ctx: Ctx, rows: SessionRow[]): HTMLElement {
  const host = h("div", { class: "stack" });
  const noticeHost = h("div");

  const reload = () => {
    // Replacing the whole section would remount the resource and lose the notice, so the list
    // is refreshed in place: one request, the same section, the receipt still on screen.
    void (async () => {
      try {
        const body = await ctx.api.get<{ data?: unknown }>("/api/v1/portal/sessions", { cache: false });
        const next = Array.isArray(body?.data) ? (body.data as SessionRow[]) : [];
        swap(listHost, next.length > 0 ? h("div", { class: "stack-sm" }, ...next.map((row) => sessionRow(ctx, row, reload, noticeHost))) : h("div"));
      } catch (err) {
        if (err instanceof ApiError && err.status === 404) return;
        ctx.toast("The session list could not be refreshed", { tone: "danger", detail: err instanceof ApiError ? err.detail : undefined });
      }
    })();
  };

  const listHost = h("div", { class: "stack-sm" }, ...rows.map((row) => sessionRow(ctx, row, reload, noticeHost)));

  const others = rows.filter((row) => !row.current).length;
  const signOutOthers = btn(
    others > 0 ? `Sign out everywhere else (${others})` : "Sign out everywhere else",
    { variant: "danger", size: "sm", icon: "logout" },
  ) as HTMLButtonElement;
  signOutOthers.disabled = others === 0;
  signOutOthers.addEventListener("click", () => {
    void withBusy(ctx, signOutOthers, async () => {
      const confirmed = await ctx.confirm({
        title: "Sign out everywhere else?",
        body: "Every other device is signed out immediately. This one stays signed in. Anyone using the account elsewhere will have to sign in again.",
        confirmLabel: "Sign out other devices",
        danger: true,
      });
      if (!confirmed) return;
      try {
        const result = await ctx.api.post<{ count?: number }>("/api/v1/portal/sessions/revoke-others", {});
        const count = typeof result?.count === "number" ? result.count : others;
        noticeHost.replaceChildren(
          banner("success", "Other sessions revoked", count > 0 ? `${count} session${count === 1 ? " was" : "s were"} signed out. This one is still active.` : "There was nothing else to sign out."),
        );
        ctx.toast("Other sessions signed out", { tone: "success" });
        reload();
      } catch (err) {
        noticeHost.replaceChildren(sessionFailure(err, "The other sessions could not be signed out"));
      }
    });
  });

  return h(
    "div",
    { class: "stack" },
    noticeHost,
    h(
      "p",
      { class: "page-sub" },
      "Where this account is signed in right now. A session ends when you sign out, when it expires, or when it is revoked here.",
    ),
    listHost,
    h("div", { class: "row-wrap" }, signOutOthers),
  );
}

function sessionRow(ctx: Ctx, row: SessionRow, reload: () => void, noticeHost: HTMLElement): HTMLElement {
  const revoke = btn(row.current ? "Sign out" : "Revoke", {
    variant: row.current ? "ghost" : "default",
    size: "sm",
    icon: row.current ? "logout" : "x",
  }) as HTMLButtonElement;

  revoke.addEventListener("click", () => {
    void withBusy(ctx, revoke, async () => {
      const confirmed = await ctx.confirm({
        title: row.current ? "Sign out of this device?" : "Revoke this session?",
        body: row.current
          ? "This signs you out here and returns you to the sign-in screen."
          : "That device is signed out immediately and has to sign in again. Nothing it has already done changes.",
        confirmLabel: row.current ? "Sign out" : "Revoke session",
        danger: true,
      });
      if (!confirmed) return;
      try {
        const result = await ctx.api.del<{ current?: boolean }>(`/api/v1/portal/sessions/${encodeURIComponent(row.id)}`);
        if (result?.current) {
          // The handler cleared the cookies for the session that made the request. Reloading is
          // what makes the shell notice; a client-side navigation would keep a dead session.
          window.location.assign("/login");
          return;
        }
        ctx.toast("Session revoked", { tone: "success" });
        reload();
      } catch (err) {
        noticeHost.replaceChildren(sessionFailure(err, "The session could not be revoked"));
      }
    });
  });

  return h(
    "div",
    { class: `session-row${row.current ? " session-row--current" : ""}` },
    h(
      "div",
      { class: "session-main" },
      h(
        "div",
        { class: "session-title" },
        icon(row.current ? "wifi" : "globe", 13),
        h("span", null, describeAgent(row.user_agent)),
        row.current ? badge("This device", "success", { dot: true }) : null,
        row.impersonated_by ? badge("Impersonated", "warning", { dot: true }) : null,
      ),
      h(
        "div",
        { class: "session-meta" },
        h("span", { class: "mono" }, row.ip || "address unknown"),
        h("span", { class: "dot-sep" }, "·"),
        h("span", { title: dateTime(row.last_seen_at) }, `last seen ${relative(row.last_seen_at)}`),
        h("span", { class: "dot-sep" }, "·"),
        h("span", { title: dateTime(row.created_at) }, `signed in ${relative(row.created_at)}`),
        h("span", { class: "dot-sep" }, "·"),
        h("span", { title: dateTime(row.expires_at) }, `expires ${relative(row.expires_at)}`),
      ),
    ),
    h("div", { class: "session-side" }, revoke),
  );
}

function sessionFailure(err: unknown, title: string): HTMLElement {
  if (!(err instanceof ApiError)) {
    return banner("danger", title, "The server could not be reached. Nothing has changed.");
  }
  if (err.status === 404) {
    return banner("info", "Session management is not available on this installation", "This build has no endpoint for it.");
  }
  switch (err.code) {
    case "no_session":
      return banner(
        "info",
        "This request is not signed in by a session",
        "An API key has no session to keep, so there is nothing to revoke here.",
      );
    default:
      return banner("danger", title, err.detail || err.code);
  }
}

/** A user agent is a long, unreadable string. This turns the common ones into "Chrome on macOS"
 *  and falls back to the raw string truncated, so nothing is invented and nothing overflows. */
function describeAgent(agent: string): string {
  const raw = (agent || "").trim();
  if (!raw) return "Unknown browser";
  const browser =
    /Edg\//.test(raw) ? "Edge"
    : /OPR\//.test(raw) ? "Opera"
    : /Firefox\//.test(raw) ? "Firefox"
    : /Chrome\//.test(raw) ? "Chrome"
    : /Safari\//.test(raw) ? "Safari"
    : /curl\//i.test(raw) ? "curl"
    : null;
  const system =
    /Windows/.test(raw) ? "Windows"
    : /iPhone|iPad|iPod/.test(raw) ? "iOS"
    : /Android/.test(raw) ? "Android"
    : /Mac OS X|Macintosh/.test(raw) ? "macOS"
    : /Linux/.test(raw) ? "Linux"
    : null;
  if (browser && system) return `${browser} on ${system}`;
  if (browser) return browser;
  return raw.length > 42 ? `${raw.slice(0, 41)}…` : raw;
}

// ------------------------------------------------------------ login history --

function historySection(ctx: Ctx): HTMLElement {
  return resource(
    ctx,
    async (signal) => {
      try {
        const page = await ctx.api.get<Page<LoginEvent> & { data?: unknown }>("/api/v1/portal/login-history", {
          query: { limit: 25 },
          cache: false,
          signal,
        });
        return { rows: Array.isArray(page?.data) ? (page.data as LoginEvent[]) : [], available: true };
      } catch (err) {
        if (err instanceof ApiError && (err.status === 404 || err.status === 501)) {
          return { rows: [] as LoginEvent[], available: false };
        }
        throw err;
      }
    },
    (data) => {
      if (!data.available) {
        return emptyState({
          title: "Login history is not available on this installation",
          body: "This build has no endpoint for it. Nothing is wrong with your account; there is simply no list to show.",
          icon: "clock",
        });
      }
      if (data.rows.length === 0) {
        return emptyState({
          title: "No sign-ins recorded",
          body: "The history fills as accounts sign in. If you have signed in before and this is empty, the installation may not be recording auth events.",
          icon: "clock",
        });
      }
      return h(
        "div",
        { class: "timeline" },
        ...data.rows.map((event) =>
          h(
            "div",
            { class: "tl-item" },
            h("span", { class: `tl-dot tl-dot--${eventTone(event.event)}` }, icon(eventIcon(event.event), 11)),
            h(
              "div",
              { class: "tl-body" },
              h(
                "div",
                { class: "tl-title" },
                h("span", null, eventLabel(event.event)),
                event.ip ? h("span", { class: "mono faint" }, event.ip) : null,
              ),
              event.detail ? h("div", { class: "tl-detail" }, event.detail) : null,
              h("div", { class: "tl-time", title: dateTime(event.created_at) }, `${dateTime(event.created_at)} · ${relative(event.created_at)}`),
            ),
          ),
        ),
      );
    },
    { skeleton: () => skeletonTable(4, 2) },
  );
}

function eventLabel(event: string): string {
  switch (event) {
    case "login":
    case "login_succeeded":
      return "Signed in";
    case "login_failed":
      return "Failed sign-in";
    case "logout":
      return "Signed out";
    case "password_changed":
    case "password_reset":
      return "Password changed";
    case "mfa_enrolled":
      return "Two-factor turned on";
    case "mfa_disabled":
      return "Two-factor turned off";
    case "session_revoked":
      return "Session revoked";
    case "email_verified":
      return "Email verified";
    default:
      return event.replace(/[_-]+/g, " ").replace(/^\w/, (character) => character.toUpperCase());
  }
}

function eventTone(event: string): "success" | "danger" | "warning" | "accent" {
  if (event === "login_failed") return "danger";
  if (event === "mfa_disabled" || event === "session_revoked") return "warning";
  if (event === "login" || event === "login_succeeded") return "success";
  return "accent";
}

function eventIcon(event: string): IconName {
  if (event === "login_failed") return "alertTriangle";
  if (event === "logout") return "logout";
  if (event === "login" || event === "login_succeeded") return "checkCircle";
  return "clock";
}
