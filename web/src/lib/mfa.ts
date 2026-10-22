// The second-factor flow, shared by the operator's account settings and the customer's security
// page.
//
// Both screens used to carry their own copy of this: two enrol buttons, two confirmation dialogs,
// two recovery-code dialogs, two disable dialogs. The behaviour was the same on purpose — a
// customer and an operator enrolling a factor must not get two different dialogs — but duplicated
// code drifts, and the drift here is a security bug rather than a cosmetic one. So the flow lives
// in one place and each page supplies only what genuinely differs: its wording and whether the
// account holds a privileged role.
//
// What the flow guarantees, and why each part matters:
//
//   - **State is read, never assumed, and from one source.** `GET /api/v1/auth/mfa` returns the
//     same object `GET /api/v1/portal/mfa` does — both are built from `authn.MFAStatusFor` — so the
//     screen shows "on since <date>", how many recovery codes are left, and whether an operator's
//     reset left an enrolment demand outstanding. It replaced a notice that said the page could
//     not tell you whether a second factor was on, which was honest and useless.
//   - **The secret is shown three ways.** A QR code rendered in the page (no third-party image
//     service: the CSP forbids it and a TOTP secret must not leave the origin), the base32 secret
//     in mono with a copy button, and the `otpauth://` URI. A phone scans; a password manager and
//     a desktop app paste; nobody is left typing 32 characters from a screenshot.
//   - **Confirmation is what turns it on.** Nothing changes until a current code is accepted, so
//     an abandoned enrolment leaves the account exactly as protected as it was.
//   - **Recovery codes are shown once, and can be downloaded.** They are the only way back in
//     without the app, so the dialog cannot be dismissed until they are acknowledged, and it
//     offers a file as well as a copy button: a clipboard is not a backup.
//   - **Disabling needs the password and a current code.** A stolen session alone must not be
//     able to remove the factor, and neither must a stolen password. The server enforces this
//     (`authn.DisableMFA`); the dialog asks for both because a form that asks for one field and
//     then refuses is a form that wasted the customer's time.
//   - **Impersonation is refused by the server.** An operator acting as a customer cannot enrol
//     or remove that customer's factor, which is the right answer: enrolling an authenticator is
//     a permanent takeover of somebody else's recovery path.

import { ApiError } from "./api";
import { h } from "./dom";
import { dateTime, relative } from "./format";
import type { Ctx, MfaState } from "./types";
import { qrModules, qrSvgPath } from "./qr";
import { bindForm, openDialog } from "../ui/dialog";
import { badge, banner, btn, copyButton, field, input, withBusy } from "../ui/kit";

/** The enrolment response: the secret, once. */
export interface MfaSetup {
  secret: string;
  otpauth_url: string;
  digits: number;
  period: number;
  algorithm: string;
}

/** The confirmation response: recovery codes, once. */
export interface MfaConfirmation {
  recovery_codes: string[];
  shown_once: boolean;
}

/** The state a screen shows before it has asked the server: nothing enrolled, and ask. */
export const MFA_UNKNOWN: MfaState = {
  has_mfa: false,
  enrolled_at: null,
  recovery_codes_remaining: 0,
  enrolment_required: false,
  reset_at: null,
  reset_by: null,
  available: true,
};

/** Where the shared state read lives. It is the caller's own state by construction — there is no
 *  id in the path — and `customer:read:self` is in the baseline every signed-in login holds, so
 *  an operator and a customer reach it the same way. */
const STATE_PATH = "/api/v1/auth/mfa";

export interface TwoFactorOptions {
  /**
   * True for an account whose role can change other people's accounts. Such an account is shown
   * a warning when it has no second factor, because a stolen staff password is not one customer's
   * problem.
   *
   * It is derived from the session (`is_staff`), not from a settings flag: this build has no
   * "require MFA" policy key, and inventing one here would be a policy decision hidden in a view.
   */
  privileged?: boolean;
  /** Called after the state changes, so the page can update anything else that depends on it. */
  onChanged?: (state: MfaState) => void;
}

/**
 * The two-factor section body: current state, what it means, and the two actions.
 *
 * It re-reads the state after every change, so the badge and the buttons cannot drift from the
 * server — the previous version of both pages showed a permanent "cannot tell" notice and offered
 * an enrol button to accounts that already had a factor.
 */
export function twoFactorSection(ctx: Ctx, options: TwoFactorOptions = {}): HTMLElement {
  let state: MfaState = ctx.session.mfa ?? MFA_UNKNOWN;

  const statusHost = h("div", { class: "settings-status" });
  const noticeHost = h("div");
  const actions = h("div", { class: "row-wrap" });

  const host = h(
    "div",
    { class: "stack" },
    noticeHost,
    statusHost,
    h(
      "p",
      { class: "page-sub" },
      "Two-factor authentication asks for a six-digit code from your authenticator app as well as your password. " +
        "Recovery codes get you back in if you lose the app — they are shown once, at enrolment.",
    ),
    actions,
  );

  const refresh = async () => {
    try {
      state = await ctx.api.get<MfaState>(STATE_PATH, { cache: false });
    } catch {
      // A failed read leaves the last known state on screen rather than claiming the factor is
      // off. `available` stays as it was, so the buttons do not appear or vanish on a blip.
      return;
    }
    render();
    options.onChanged?.(state);
    // The shell's copy of the session carries the same flag; refreshing it keeps the account
    // menu's "two-factor: on/off" line truthful after an enrolment without a page reload.
    await ctx.refreshSession().catch(() => undefined);
  };

  const startEnrolment = (trigger: HTMLButtonElement) => {
    void withBusy(ctx, trigger, async () => {
      let setup: MfaSetup;
      try {
        setup = await ctx.api.post<MfaSetup>("/api/v1/auth/mfa/enrol", {});
      } catch (err) {
        noticeHost.replaceChildren(mfaFailure(err, "Enrolment could not start"));
        return;
      }
      openEnrolment(ctx, setup, {
        onConfirmed: async (confirmation) => {
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
          await refresh();
          showRecoveryCodes(ctx, confirmation);
        },
      });
    });
  };


  const render = () => {
    const required = state.enrolment_required && !state.has_mfa;
    const tone = state.has_mfa ? "success" : required ? "warning" : options.privileged ? "warning" : "neutral";
    const label = state.has_mfa ? "On" : required ? "Required" : "Off";

    // The sentence under the badge. Every branch is a fact the server reported, including the
    // two that only exist because a *staff* reset can leave them behind: the demand, and the name
    // of the operator who did it. A customer whose factor vanished is entitled to see that it was
    // not a compromise.
    let detail: string;
    if (state.has_mfa) {
      detail = state.enrolled_at
        ? `On since ${dateTime(state.enrolled_at)} (${relative(state.enrolled_at)}). A code is required at every sign-in.`
        : "On. A code is required at every sign-in.";
      // The recovery-code count is the warning that arrives before the lockout rather than after
      // it, so it is stated whenever it is knowable.
      if (state.recovery_codes_remaining === 0) {
        detail += " No unused recovery codes are left — if you lose the app you will need an operator to reset the factor.";
      } else {
        detail += ` ${state.recovery_codes_remaining} unused recovery code${state.recovery_codes_remaining === 1 ? "" : "s"} left.`;
      }
    } else if (required) {
      detail =
        "Off, and an operator has asked you to set up a new factor — the previous one was removed so you could get back in. " +
        "Set one up now; a password alone is not enough to keep this account safe.";
    } else {
      detail = "Off. Your password alone gets into this account.";
    }

    statusHost.replaceChildren(
      h(
        "div",
        { class: "settings-status-row" },
        h("span", { class: "field-label" }, "Two-factor authentication"),
        badge(label, tone, { dot: true }),
        h("span", { class: "settings-status-detail" }, detail),
      ),
      state.reset_at
        ? h(
            "p",
            { class: "field-hint" },
            `A support reset of the second factor is recorded on ${dateTime(state.reset_at)}${
              state.reset_by ? ` by ${state.reset_by}` : ""
            }. The audit log has the entry; nothing about the old factor can be recovered.`,
          )
        : h("span", { hidden: true }),
    );

    const buttons: HTMLElement[] = [];
    if (!state.available) {
      buttons.push(
        banner(
          "danger",
          "The two-factor state could not be read",
          "The server did not answer the status request, so this page will not offer to change anything. Reload; if it persists, the installation has a problem worth reporting.",
        ),
      );
    } else if (!state.has_mfa) {
      // There is no "resume": an unconfirmed enrolment's secret is sealed and never readable
      // again, and `EnrolMFA` replaces it, so the button always starts a new setup.
      const enrolButton = btn(required ? "Set up a second factor now" : "Set up an authenticator app", {
        variant: "primary",
        icon: "shieldCheck",
        size: "sm",
      }) as HTMLButtonElement;
      enrolButton.addEventListener("click", () => startEnrolment(enrolButton));
      buttons.push(enrolButton);
    }
    if (state.has_mfa) {
      buttons.push(
        btn("Turn off two-factor", {
          variant: "ghost",
          icon: "x",
          size: "sm",
          onclick: () => openDisable(ctx, { onDisabled: refresh }),
        }) as HTMLButtonElement,
      );
    }

    // The prompt for a privileged account. It is not a blocker: shutting an operator out of the
    // panel they are on call for is a worse failure than a warning they can ignore, and this build
    // has no policy that would justify refusing the session.
    if (options.privileged && !state.has_mfa && state.available) {
      buttons.unshift(
        banner(
          "warning",
          "Your role can change every customer's account",
          "Add a second factor. A stolen staff password must not be enough on its own.",
        ),
      );
    }

    actions.replaceChildren(...buttons);
  };

  render();
  // The session may be stale — a page rendered from a cached identity, or a factor enrolled in
  // another tab — so the state is re-read once on mount. It is one indexed query.
  void refresh();

  return host;
}

// ------------------------------------------------------------------ enrolment --

function openEnrolment(ctx: Ctx, setup: MfaSetup, callbacks: { onConfirmed: (res: MfaConfirmation) => void }): void {
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
      "Add this account to your authenticator app by scanning the code, or by typing the setup secret, then enter the six digits it shows.",
    ),
    mfaQrPanel(setup),
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
        `${setup.digits} digits, a new code every ${setup.period} seconds, ${setup.algorithm}. ` +
          "The secret is the same thing the QR code carries; either one adds the account.",
      ),
    ),
    h("div", { class: "row-wrap" }, copyButton(ctx, setup.otpauth_url, "setup URI")),
    field({
      label: "Six-digit code",
      control: code,
      id: "mfa-code",
      hint: "From the app, after adding this account.",
    }),
    errorHost,
  );

  // The footer is a sibling of the body, so the submit button has to be associated with the form
  // explicitly. Without this the primary button is inert — the dialog opens, the field takes a
  // code, and clicking "Confirm" does nothing at all, which reads as a broken screen rather than
  // a wiring bug. `bindForm` is the one place that knows this.
  bindForm(form, confirm);

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
        errorHost.replaceChildren(banner("danger", "Enter the six-digit code", "The code is six digits, with no spaces."));
        code.focus();
        return;
      }
      confirm.disabled = true;
      let res: MfaConfirmation;
      try {
        res = await ctx.api.post<MfaConfirmation>("/api/v1/auth/mfa/confirm", { code: value });
      } catch (err) {
        errorHost.replaceChildren(mfaFailure(err, "The code could not be confirmed"));
        return;
      } finally {
        confirm.disabled = false;
      }
      // The dialog refuses to close while a request is in flight, so it is unlocked above before
      // closing here; otherwise a successful confirm would leave it on screen.
      handle.close();
      callbacks.onConfirmed(res);
    })();
  });
}

/**
 * The scannable form of the setup URI, or an honest note when the URI is too long for the
 * encoder's version table.
 *
 * Exported because another screen renders the same secret: the customer's security page
 * (web/src/views/portal-security.ts) shows the base32 secret and the setup URI, and a symbol is a
 * one-line addition there. `qrModules` fetches nothing, so the CSP is not a reason to omit it.
 */
export function mfaQrPanel(setup: MfaSetup): HTMLElement {
  const qr = qrModules(setup.otpauth_url, "M");
  if (!qr) {
    return banner(
      "info",
      "Scanning is not available for this setup link",
      "The setup link is longer than the QR code this panel draws, which happens with a very long " +
        "address. Type the setup secret below — it is the same credential.",
    );
  }
  const scale = 4;
  const quiet = 4;
  const extent = qr.size + quiet * 2;
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("class", "settings-qr");
  svg.setAttribute("viewBox", `0 0 ${extent} ${extent}`);
  svg.setAttribute("width", String(Math.min(extent * scale, 232)));
  svg.setAttribute("height", String(Math.min(extent * scale, 232)));
  svg.setAttribute("role", "img");
  svg.setAttribute("aria-label", "QR code containing the authenticator setup link");
  // `shape-rendering: crispEdges` keeps the modules square at fractional device pixel ratios;
  // a blurred edge is what makes a camera fail to lock on.
  svg.setAttribute("shape-rendering", "crispEdges");
  const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
  path.setAttribute("d", qrSvgPath(qr));
  path.setAttribute("transform", `translate(${quiet} ${quiet})`);
  svg.appendChild(path);

  return h(
    "div",
    { class: "settings-qr-panel" },
    h("div", { class: "settings-qr-frame" }, svg),
    h(
      "div",
      { class: "stack-sm" },
      h("span", { class: "field-label" }, "Scan with your authenticator app"),
      h(
        "span",
        { class: "field-hint" },
        "Point the camera at the code. The app adds the account and starts producing codes; nothing is on until you confirm one below.",
      ),
    ),
  );
}

// ------------------------------------------------------------- recovery codes --

function showRecoveryCodes(ctx: Ctx, res: MfaConfirmation): void {
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
    subtitle: res.shown_once
      ? "Shown once. They cannot be retrieved later."
      : "These codes are displayed now; save them before closing.",
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
        ? h(
            "div",
            { class: "row-wrap" },
            copyButton(ctx, joined, "all codes"),
            recoveryCodeDownload(ctx, codes),
          )
        : null,
      h("label", { class: "checkbox" }, saved, h("span", null, "I have saved these codes somewhere safe")),
    ),
    footer: h("div", { class: "row spacer" }, done),
    // Locked until acknowledged: closing this dialog is how the codes are lost.
    locked: () => !saved.checked,
  });

  done.addEventListener("click", () => handle.close());
}

/**
 * A file of the recovery codes.
 *
 * A copy button is not a backup: the clipboard is lost on the next copy, and the codes have to
 * survive until the phone is gone. The file is built in the page from a Blob, so nothing is
 * uploaded and the CSP's `connect-src 'self'` is untouched — a `blob:` URL is a local object, not
 * a request.
 */
export function recoveryCodeDownload(ctx: Ctx, codes: string[]): HTMLAnchorElement {
  const lines = [
    "Catalyst Billing — two-factor recovery codes",
    `Account: ${ctx.session.user.email}`,
    `Generated: ${new Date().toISOString()}`,
    "",
    "Each code works once. Keep this file somewhere you will still have when your phone is gone.",
    "",
    ...codes,
    "",
  ];
  const blob = new Blob([lines.join("\n")], { type: "text/plain;charset=utf-8" });
  const url = URL.createObjectURL(blob);
  const anchor = h("a", {
    class: "btn btn--sm",
    href: url,
    download: "catalyst-billing-recovery-codes.txt",
    title: "Download the recovery codes as a text file",
  }) as HTMLAnchorElement;
  anchor.append("Download as a file");
  // The object URL is revoked when the page goes away rather than on click: a click may be
  // cancelled in the save dialog, and revoking first would leave a dead link.
  ctx.onDispose(() => URL.revokeObjectURL(url));
  return anchor;
}

// -------------------------------------------------------------------- disable --

function openDisable(ctx: Ctx, callbacks: { onDisabled: () => void | Promise<void> }): void {
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
    banner(
      "warning",
      "Both are required, on purpose",
      "A stolen session on its own must not be able to remove the second factor, and neither must a stolen password.",
    ),
    errorHost,
  );

  bindForm(form, submit);

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
        errorHost.replaceChildren(mfaFailure(err, "The second factor could not be turned off"));
        return;
      } finally {
        submit.disabled = false;
      }
      handle.close();
      ctx.toast("Two-factor authentication is off", { tone: "success" });
      await callbacks.onDisabled();
    })();
  });
}

// -------------------------------------------------------------------- failure --

/** One plain sentence per MFA failure code, so both audiences read the same words for the same
 *  refusal. */
export function mfaFailure(err: unknown, fallbackTitle: string): HTMLElement {
  if (!(err instanceof ApiError)) {
    return banner("danger", fallbackTitle, "The server could not be reached. Nothing has changed.");
  }
  switch (err.code) {
    case "mfa_already_enrolled":
      return banner(
        "warning",
        "A second factor is already confirmed",
        "Turn it off before enrolling a new one. The server will not replace it silently.",
      );
    case "mfa_not_pending":
      return banner("warning", "There is no enrolment to confirm", "Start the setup again from the button above.");
    case "mfa_not_enrolled":
      return banner("info", "No second factor is enrolled", "There is nothing to turn off on this account.");
    case "mfa_unavailable":
      return banner(
        "danger",
        "Two-factor is not configured on this server",
        "Enrolment cannot work here. Contact whoever runs this installation; retrying will not help.",
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
