// Account settings.
//
// Six sections, one page, a sticky index. Everything here is about the *account* — the billing
// screens are elsewhere — and every claim on it is something the API actually told us:
//
//   - The identity block comes from /api/v1/auth/me, which reports email, display name, roles,
//     email_verified and the caller's own second-factor state.
//   - **Security** shows that state — "on since <date>" or "off" — and is where an operator
//     enrols an authenticator, replaces an abandoned setup, or turns the factor off. The flow
//     itself lives in web/src/lib/mfa.ts, because the customer portal needs the identical thing
//     and two copies of a security dialog drift.
//   - A staff account with no factor is told why it matters: its role can change every
//     customer's account, so a stolen staff password is not one customer's problem. The prompt
//     is a warning, not a lock-out — an operator on call must not be shut out of the panel by a
//     policy this build does not actually have.
//   - Recovery codes are returned exactly once, with `shown_once: true`. They are displayed in
//     a dialog that cannot be dismissed until they are acknowledged, and the dialog offers a
//     download as well as a copy button: a clipboard is not a backup.
//
// The theme is not here: it lives in the top bar, where it is one click away on every screen.

import { ApiError } from "../lib/api";
import { h } from "../lib/dom";
import { dateTime, relative, titleCase } from "../lib/format";
import { icon, type IconName } from "../lib/icons";
import { twoFactorSection } from "../lib/mfa";
import type { Ctx } from "../lib/types";
import { badge, banner, btn, card, kv, pageHead, withBusy } from "../ui/kit";

export const meta = { title: "Account settings", crumb: "Settings" };

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
      // The second factor, on the same list as the address and the roles, because "is my account
      // protected" is an identity question and this is where an operator looks for one. The link
      // jumps to the section that changes it: an answer with no way to act on it is a status page,
      // not a settings page.
      kv(
        "Two-factor",
        h(
          "span",
          { class: "row-wrap" },
          twoFactorBadge(ctx.session.mfa),
          h("a", { class: "link", href: "#security" }, ctx.session.mfa.has_mfa ? "Manage" : "Set up"),
        ),
      ),
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

/** One word for the second factor, from the state the server reported. */
function twoFactorBadge(state: Ctx["session"]["mfa"]): HTMLElement {
  if (state.has_mfa) return badge("On", "success", { dot: true });
  if (state.enrolment_required) return badge("Required", "warning", { dot: true });
  return badge("Off", state.available ? "warning" : "neutral", { dot: true });
}

// ----------------------------------------------------------------- security --

function securitySection(ctx: Ctx): HTMLElement {
  // One implementation, shared with the customer portal's security page (web/src/lib/mfa.ts).
  // The operator is told that their role is privileged, because a stolen staff password is not
  // one customer's problem; a customer is not, because there is no policy in this build that says
  // they must have a factor and inventing one in a view would be a decision disguised as a warning.
  //
  // `onChanged` re-renders the identity row above, so "Two-factor: On" cannot go stale after an
  // enrolment on the same page. The shared component has already refreshed the session by then,
  // so the badge is built from the server's answer rather than from a local flag.
  return twoFactorSection(ctx, {
    privileged: true,
    onChanged: () => {
      const row = document.querySelector("#identity .kv:last-child .kv-value");
      if (row) row.replaceChildren(twoFactorBadge(ctx.session.mfa), h("a", { class: "link", href: "#security" }, ctx.session.mfa.has_mfa ? "Manage" : "Set up"));
    },
  });
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
