// Security: every live session, and the sign-in trail behind it.
//
// The portal's own screen answers "where am I signed in". This one answers the incident
// question: which accounts have sessions open right now, from where, and which of them is being
// tried. Two lists, and each answers a different half:
//
//  - **Sessions** is a read of `sessions` across every account, with the address, the agent, the
//    start and the last activity. Revoking one ends it server-side immediately — the row is
//    marked revoked, so the next request with that cookie is anonymous.
//  - **Sign-ins** is a read of `auth_events`: `login_succeeded`, `login_failed`,
//    `login_rejected` and `login_throttled`. The summary above it is the last day's failures by
//    account and by address, which is what "is this account being attacked" actually means.
//
// Two deliberate refusals:
//
//  - The operator cannot revoke the session they are holding this screen with. That is a
//    sign-out, it belongs on the logout route, and doing it by mis-click in a table of identical
//    rows throws them out mid-incident. The server refuses it (`own_session`) and the button is
//    disabled here with the reason in its title.
//  - Revoking somebody else's session is an administrative act, so it is audited in the same
//    transaction that ends it, and the account's own sign-in trail records that an operator did
//    it. A customer whose session vanished is entitled to see it was not a compromise.

import { ApiError } from "../lib/api";
import { h } from "../lib/dom";
import { dateTime, relative, truncate } from "../lib/format";
import type { Ctx } from "../lib/types";
import {
  badge,
  banner,
  btn,
  field,
  pageHead,
  searchInput,
  sectionTitle,
  select,
  withBusy,
} from "../ui/kit";
import { createTable } from "../ui/table";

export const meta = { title: "Security", crumb: "Security" };

// ---------------------------------------------------------------- payloads --

/** One live session, from `securitySessionJSON` in internal/httpapi/sessions_ext.go. */
interface SessionRow {
  id: string;
  user: { id: string; email: string; display_name: string };
  created_at: string;
  last_seen_at: string;
  expires_at: string;
  ip: string;
  user_agent: string;
  current: boolean;
  impersonated_by: string | null;
  impersonator: string | null;
}

interface SessionSummary {
  sessions: number;
  users: number;
  impersonations: number;
}

/** One row of the sign-in trail. `user` is null for an attempt against an address that has no
 *  account, which is exactly the row an operator investigating a spray wants to see. */
interface SignInRow {
  id: number;
  event: string;
  outcome: string;
  detail: string;
  ip: string;
  created_at: string;
  user: { id: string; email: string } | null;
}

interface SignInSummary {
  window_hours: number;
  successes: number;
  failures: number;
  top_failing_accounts: Array<{ label: string; count: number }> | null;
  top_failing_ips: Array<{ label: string; count: number }> | null;
}

export default async function securityPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("staff:manage")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Security" }),
      banner(
        "danger",
        "You cannot read the security screen",
        "Seeing every account's sessions is an administrative act: it needs the staff:manage permission, the same one that grants roles and starts an impersonation.",
        btn("Back to the dashboard", { href: "/dashboard", size: "sm", variant: "ghost" }),
      ),
    );
  }

  const sessionSummaryHost = h("div", { class: "grid-4" });
  const signInSummaryHost = h("div", { class: "stack-sm" });

  let sessionQuery = ctx.route.query.get("q") ?? "";
  let outcome = ctx.route.query.get("outcome") ?? "all";
  let signInQuery = sessionQuery;

  // ------------------------------------------------------------------ sessions --

  const sessions = createTable<SessionRow>(ctx, {
    path: "/api/v1/staff/security/sessions",
    query: { q: sessionQuery || undefined },
    columns: [
      {
        key: "who",
        label: "Who",
        primary: true,
        render: (row) =>
          h(
            "div",
            { class: "cell-main" },
            h(
              "div",
              { class: "primary" },
              h(
                "a",
                { class: "row-title", href: `/customers?q=${encodeURIComponent(row.user.email)}` },
                row.user.display_name || row.user.email,
              ),
              " ",
              row.current ? badge("This session", "info", { dot: true }) : null,
              row.impersonated_by ? badge("Impersonated", "warning", { dot: true }) : null,
            ),
            h(
              "div",
              { class: "secondary" },
              row.user.email,
              row.impersonated_by
                ? ` · impersonated by ${row.impersonator || row.impersonated_by}`
                : "",
            ),
          ),
      },
      {
        key: "from",
        label: "From",
        render: (row) =>
          h(
            "div",
            { class: "stack-sm" },
            h("span", { class: "mono" }, row.ip || "unknown address"),
            h(
              "span",
              { class: "faint", title: row.user_agent },
              truncate(row.user_agent || "no user agent recorded", 42),
            ),
          ),
      },
      {
        key: "started",
        label: "Started",
        hideSmall: true,
        render: (row) =>
          h("span", { class: "dim nowrap", title: dateTime(row.created_at) }, relative(row.created_at)),
      },
      {
        key: "seen",
        label: "Last seen",
        hideSmall: true,
        render: (row) =>
          h("span", { class: "dim nowrap", title: dateTime(row.last_seen_at) }, relative(row.last_seen_at)),
      },
      {
        key: "expires",
        label: "Expires",
        hideSmall: true,
        render: (row) =>
          h("span", { class: "faint nowrap", title: dateTime(row.expires_at) }, relative(row.expires_at)),
      },
      {
        key: "actions",
        label: "",
        align: "right",
        width: "1%",
        render: (row) =>
          row.current
            ? h(
                "span",
                {
                  class: "faint nowrap",
                  title:
                    "This is the session making the request. Use “Sign out” in the account menu to end it.",
                },
                "Current session",
              )
            : (btn("Revoke", {
                size: "sm",
                variant: "danger",
                icon: "logout",
                title: "End this session now. The person is signed out on their next request.",
                onclick: () => void revoke(row),
              }) as HTMLButtonElement),
      },
    ],
    empty: {
      title: "No live sessions",
      body: "Every session is closed. A session appears here as soon as somebody signs in.",
    },
    rowKey: (row) => row.id,
    onLoaded: (_rows, page) => {
      const summary = (page as unknown as { summary?: SessionSummary }).summary;
      const s = summary ?? { sessions: 0, users: 0, impersonations: 0 };
      sessionSummaryHost.replaceChildren(
        stat("Live sessions", s.sessions, "rows that can still authenticate"),
        stat("Accounts", s.users, "distinct people signed in"),
        stat("Impersonations", s.impersonations, "sessions an operator opened"),
        stat("Shown", _rows.length, "on this page"),
      );
    },
    footNote: "Revoking a session is audited against the account it ends.",
  });

  async function revoke(row: SessionRow): Promise<void> {
    const ok = await ctx.confirm({
      title: `Revoke ${row.user.email}'s session?`,
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "p",
          null,
          `The session started ${relative(row.created_at)} from ${row.ip || "an unknown address"}. It stops working on the next request: the person is signed out of that device, and their other sessions are untouched.`,
        ),
        h(
          "p",
          { class: "field-hint" },
          "This is audited against their account, and their own sign-in history records that an operator ended it — so a session that disappears is explainable rather than alarming.",
        ),
      ),
      confirmLabel: "Revoke the session",
      danger: true,
    });
    if (!ok) return;
    await withBusy(ctx, null, async () => {
      try {
        await ctx.api.del(`/api/v1/staff/security/sessions/${encodeURIComponent(row.id)}`);
        ctx.toast("Session revoked", { tone: "success", detail: row.user.email });
        await sessions.reload();
      } catch (err) {
        if (err instanceof ApiError && err.code === "own_session") {
          ctx.toast("That is your own current session", {
            tone: "info",
            detail: "Sign out from the account menu instead; this screen will not end it for you.",
          });
          await sessions.reload();
          return;
        }
        report(err, "The session could not be revoked");
      }
    });
  }

  // --------------------------------------------------------------- sign-ins --

  const signIns = createTable<SignInRow>(ctx, {
    path: "/api/v1/staff/security/sign-ins",
    query: { outcome: outcome === "all" ? undefined : outcome, q: signInQuery || undefined },
    columns: [
      {
        key: "when",
        label: "When",
        primary: true,
        render: (row) =>
          h(
            "div",
            { class: "cell-main" },
            h("div", { class: "primary" }, relative(row.created_at)),
            h("div", { class: "secondary", title: dateTime(row.created_at) }, dateTime(row.created_at)),
          ),
      },
      {
        key: "account",
        label: "Account",
        render: (row) =>
          row.user
            ? h("span", { class: "mono" }, row.user.email)
            : h("span", { class: "faint" }, "unknown account"),
      },
      {
        key: "outcome",
        label: "Result",
        render: (row) =>
          row.outcome === "success"
            ? badge("Signed in", "success", { dot: true })
            : badge("Failed", "danger", { dot: true }),
      },
      {
        key: "detail",
        label: "Detail",
        hideSmall: true,
        render: (row) =>
          row.detail ? h("span", { class: "dim" }, row.detail) : h("span", { class: "faint" }, "—"),
      },
      {
        key: "ip",
        label: "From",
        align: "right",
        render: (row) => h("span", { class: "mono" }, row.ip || "—"),
      },
    ],
    empty: {
      title: "No sign-in attempts recorded",
      body: "Every sign-in — successful or not — is written to the account's authentication trail and appears here.",
    },
    rowKey: (row) => String(row.id),
    onLoaded: (_rows, page) => {
      const summary = (page as unknown as { summary?: SignInSummary }).summary;
      if (!summary) return;
      const children: Node[] = [
        h(
          "div",
          { class: "grid-4" },
          stat("Signed in", summary.successes, `in the last ${summary.window_hours} hours`),
          stat("Failed", summary.failures, "attempts that did not get in"),
          stat("Accounts targeted", (summary.top_failing_accounts ?? []).length, "with failures in the window"),
          stat("Addresses", (summary.top_failing_ips ?? []).length, "sources of those failures"),
        ),
      ];
      const failures = summary.top_failing_accounts ?? [];
      if (failures.length > 0) {
        children.push(
          h(
            "div",
            { class: "stack-sm" },
            h("span", { class: "field-label" }, "Failing accounts"),
            ...failures.map((row) =>
              h(
                "div",
                { class: "row" },
                h("span", { class: "mono" }, row.label),
                badge(`${row.count} failure${row.count === 1 ? "" : "s"}`, "warning", { dot: true }),
              ),
            ),
          ),
        );
      }
      const addresses = summary.top_failing_ips ?? [];
      if (addresses.length > 0) {
        children.push(
          h(
            "div",
            { class: "stack-sm" },
            h("span", { class: "field-label" }, "Failing addresses"),
            ...addresses.map((row) =>
              h(
                "div",
                { class: "row" },
                h("span", { class: "mono" }, row.label),
                badge(`${row.count} failure${row.count === 1 ? "" : "s"}`, "warning", { dot: true }),
              ),
            ),
          ),
        );
      }
      signInSummaryHost.replaceChildren(...children);
    },
    footNote: "Successful and failed attempts alike: a history with only the successes cannot answer “is this account being attacked”.",
  });

  function report(err: unknown, fallback: string): void {
    if (err instanceof ApiError) {
      ctx.toast(err.detail || fallback, { tone: "danger", detail: err.code });
      return;
    }
    ctx.toast(fallback, { tone: "danger", detail: "The server could not be reached." });
  }

  const outcomePicker = select(
    [
      { value: "all", label: "Every attempt" },
      { value: "success", label: "Successful only" },
      { value: "failure", label: "Failures only" },
    ],
    {
      id: "sign-in-outcome",
      value: outcome,
      onchange: () => {
        outcome = outcomePicker.value;
        ctx.replaceQuery({ outcome: outcome === "all" ? null : outcome });
        void signIns.reload({ outcome: outcome === "all" ? undefined : outcome, q: signInQuery || undefined });
      },
    },
  );

  let searchTimer: number | undefined;
  const searchBox = searchInput({
    value: sessionQuery,
    placeholder: "Account email…",
    oninput: (value) => {
      window.clearTimeout(searchTimer);
      searchTimer = window.setTimeout(() => {
        sessionQuery = value.trim();
        signInQuery = sessionQuery;
        ctx.replaceQuery({ q: sessionQuery || null });
        void sessions.reload({ q: sessionQuery || undefined });
        void signIns.reload({
          outcome: outcome === "all" ? undefined : outcome,
          q: signInQuery || undefined,
        });
      }, 250);
    },
  });

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Security",
      sub: "Every live session and every sign-in attempt, across all accounts. This is the screen an incident starts from.",
    }),

    banner(
      "info",
      "Revoking a session ends it on the server",
      "The row is marked revoked, so the next request carrying that cookie is anonymous. The person's other sessions are untouched, and the revocation is audited against their account.",
    ),

    h(
      "div",
      { class: "stack" },
      h(
        "div",
        { class: "stack" },
        sectionTitle(
          "Active sessions",
          btn("Refresh", {
            size: "sm",
            variant: "ghost",
            icon: "refresh",
            onclick: () => {
              void sessions.reload();
              void signIns.reload();
            },
          }),
        ),
        h(
          "p",
          { class: "field-hint" },
          "Who is signed in, from where, since when and when they were last seen. The current session is marked and cannot be revoked here: that is a sign-out, and doing it by mis-click would throw you out of the incident you are handling.",
        ),
        sessionSummaryHost,
        h("div", { class: "toolbar" }, h("div", { class: "log-filters log-filters--inline" }, searchBox)),
        sessions.el,
      ),

      h(
        "div",
        { class: "stack" },
        sectionTitle("Sign-in history"),
        h(
          "p",
          { class: "field-hint" },
          "A failure is not proof of an attack: it is also a forgotten password. What makes it an attack is the count against one account or one address in a short window, which is what the summary below shows.",
        ),
        // The outcome filter is a control, so it belongs in the bar under the heading — not
        // inside it. A labelled field in a `.section-title` makes the heading as tall as a
        // form row and draws the heading's rule across the middle of the select, which is
        // what this screen used to look like.
        h("div", { class: "filter-bar" }, field({ label: "Show", control: outcomePicker, id: "sign-in-outcome" })),
        signInSummaryHost,
        signIns.el,
      ),
    ),
  );
}

// ------------------------------------------------------------------ helpers --

function stat(label: string, value: number, hint: string): HTMLElement {
  return h(
    "section",
    { class: "card stat" },
    h("span", { class: "stat-label" }, label),
    h("span", { class: "stat-value" }, String(value)),
    h("span", { class: "stat-hint" }, hint),
  );
}
