// Users.
//
// One screen for two audiences, because they are the same act: a person who needs a login.
// Onboarding a support agent, a second administrator and somebody who rang up to buy a
// server are all "create an account, give it the right role", and splitting them into two
// screens would mean two code paths for one decision.
//
// What the screen is careful about, and why:
//
//  - **It cannot lock the operator out.** Disabling or deleting the account you are signed
//    in as is refused by the server with `self_lockout`, and demoting or disabling the last
//    active administrator is refused with `last_administrator` — which is a database
//    invariant, not a hopeful check. The screen shows the refusal instead of pretending the
//    click worked.
//  - **A password is never returned.** Setting one reports how many sessions it ended and
//    nothing else; there is no field on this page that holds a password after the dialog
//    closes.
//  - **Disable is the normal answer, delete is the exception.** An account with invoices or
//    with audit rows cannot be deleted at all, and the refusal (`user_has_history`) says what
//    holds it. The delete confirmation therefore asks for the address to be typed: it is a
//    rare action that cannot be undone.
//  - **Custom roles are offered, not guessed.** The role list is read from the roles screen's
//    endpoint, so a role an operator defined there is assignable here without a deploy.

import { ApiError } from "../lib/api";
import { h } from "../lib/dom";
import { dateTime, humanStatus, relative, type Tone } from "../lib/format";
import type { Ctx } from "../lib/types";
import {
  badge,
  banner,
  btn,
  card,
  emptyState,
  field,
  input,
  pageHead,
  searchInput,
  select,
} from "../ui/kit";
import { createTable, type Column } from "../ui/table";

export const meta = { title: "Users", crumb: "Users" };

/** The account shape from `userJSON` in internal/httpapi/users.go. */
interface UserRow {
  id: string;
  email: string;
  display_name: string;
  status: string;
  email_verified: boolean;
  /** The staff role name, empty for a customer. */
  role: string;
  is_staff: boolean;
  customer_id: string | null;
  created_at: string;
  last_sign_in_at: string | null;
}

interface RoleOption {
  value: string;
  label: string;
}

/** The four system roles are named here only as a fallback: the list is served, and a custom
 *  role created on /roles must appear without a rebuild. */
const FALLBACK_ROLES: RoleOption[] = [
  { value: "administrator", label: "Administrator" },
  { value: "billing", label: "Billing" },
  { value: "support", label: "Support" },
  { value: "infrastructure", label: "Infrastructure" },
];

const MIN_SEARCH = 2;

export default async function usersPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("staff:manage")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Users" }),
      card({
        body: emptyState({
          title: "Not available for your role",
          body: "Creating and editing accounts needs the staff:manage permission.",
          icon: "lock",
        }),
      }),
    );
  }

  let term = (ctx.route.query.get("q") ?? "").trim();
  let scope = (ctx.route.query.get("staff") ?? "").trim();

  const queryOf = () => ({
    q: term.length >= MIN_SEARCH ? term : undefined,
    staff: scope === "" ? undefined : scope,
  });

  const hint = h(
    "span",
    { class: "faint", hidden: term.length !== 1 },
    `Keep typing — the search needs ${MIN_SEARCH} characters.`,
  );

  const columns: Column<UserRow>[] = [
    {
      key: "person",
      label: "Person",
      primary: true,
      render: (row) =>
        h(
          "div",
          { class: "cell-main" },
          h("div", { class: "primary" }, row.display_name || row.email),
          h("div", { class: "secondary" }, row.display_name ? row.email : "No display name"),
        ),
    },
    {
      key: "role",
      label: "Role",
      render: (row) =>
        row.is_staff
          ? badge(roleLabel(row.role), "info", { dot: true })
          : badge("Customer", "neutral", { dot: true }),
    },
    {
      key: "verified",
      label: "Address",
      hideSmall: true,
      render: (row) =>
        row.email_verified
          ? badge("Verified", "success", { dot: true })
          : badge("Unverified", "warning", { dot: true }),
    },
    {
      key: "status",
      label: "Status",
      render: (row) => badge(humanStatus(row.status), statusTone(row.status), { dot: true }),
    },
    {
      key: "last_sign_in",
      label: "Last sign-in",
      align: "right",
      hideSmall: true,
      render: (row) =>
        row.last_sign_in_at
          ? h(
              "span",
              { class: "dim nowrap", title: dateTime(row.last_sign_in_at) },
              relative(row.last_sign_in_at),
            )
          : h("span", { class: "faint nowrap" }, "Never"),
    },
    {
      key: "actions",
      label: "",
      align: "right",
      width: "1%",
      render: (row) =>
        h(
          "div",
          { class: "row-wrap" },
          btn("Edit", {
            size: "sm",
            icon: "edit",
            title: `Edit ${row.email}`,
            onclick: () => void openEdit(row),
          }),
          btn("Password", {
            size: "sm",
            variant: "ghost",
            icon: "key",
            title: `Set a password for ${row.email}`,
            onclick: () => void openPassword(row),
          }),
          row.status === "active"
            ? btn("Disable", {
                size: "sm",
                variant: "ghost",
                icon: "lock",
                title: `Disable ${row.email}`,
                onclick: () => void setStatus(row, "disabled"),
              })
            : btn("Enable", {
                size: "sm",
                variant: "ghost",
                icon: "checkCircle",
                title: `Enable ${row.email}`,
                onclick: () => void setStatus(row, "active"),
              }),
          btn("Delete", {
            size: "sm",
            variant: "ghost",
            icon: "trash",
            title: `Delete ${row.email}`,
            onclick: () => void deleteUser(row),
          }),
        ),
    },
  ];

  const empty: { title: string; body?: string; action?: Node } = { title: "", body: "" };

  const table = createTable<UserRow>(ctx, {
    path: "/api/v1/staff/users",
    columns,
    query: queryOf(),
    limit: 25,
    empty,
    rowKey: (row) => row.id,
    footNote: "Newest first. A customer with no customer record can sign in and find nothing.",
  });

  function describeEmpty(): void {
    empty.title = term ? `No account matches “${term}”` : "No accounts yet";
    empty.body = term
      ? "Search matches the address and the display name. Widen it, or clear it to see everyone."
      : "Create the first account here: an administrator, an agent, or a customer who needs a way in.";
    empty.action = btn("Create user", { size: "sm", icon: "plus", onclick: () => void openCreate() });
  }

  // ------------------------------------------------------------- role list --

  let roles: RoleOption[] = FALLBACK_ROLES;

  async function loadRoles(): Promise<RoleOption[]> {
    try {
      const page = await ctx.api.get<{ data: Array<{ name: string; description: string }> }>(
        "/api/v1/staff/roles",
      );
      const out = page.data.map((role) => ({ value: role.name, label: roleLabel(role.name) }));
      if (out.length > 0) roles = out;
    } catch {
      // The fallback keeps the screen usable: the four system roles are the ones a fresh
      // installation has, and the server refuses an unknown name anyway.
    }
    return roles;
  }

  function roleSelect(current: string, id: string): HTMLSelectElement {
    return select(
      [{ value: "", label: "Customer (no staff role)" }, ...roles],
      { id, value: current },
    );
  }

  // --------------------------------------------------------------- create --

  async function openCreate(): Promise<void> {
    await loadRoles();

    const email = input({
      id: "user-email",
      name: "email",
      type: "email",
      placeholder: "person@example.com",
      autocomplete: "off",
    });
    const name = input({
      id: "user-name",
      name: "display_name",
      placeholder: "Ada Lovelace",
      autocomplete: "off",
      maxlength: 200,
    });
    const password = input({
      id: "user-password",
      name: "password",
      type: "password",
      autocomplete: "new-password",
      placeholder: "At least 12 characters",
    });
    const role = roleSelect("", "user-role");
    const verified = h("input", { type: "checkbox", checked: true, id: "user-verified" });

    await ctx.formDialog({
      title: "Create a user",
      subtitle: "One act: the account and the role it holds are written together.",
      submitLabel: "Create user",
      body: h(
        "div",
        { class: "stack" },
        field({ label: "Email", control: email, required: true }),
        field({
          label: "Display name",
          control: name,
          required: true,
          hint: "What the panel calls them. The customer sees this on their own account.",
        }),
        field({
          label: "Password",
          control: password,
          required: true,
          hint: "At least 12 characters. The same rule registration applies: the server answers a weak one with the same weak_password code. Hand it to them and ask them to change it.",
        }),
        field({
          label: "Role",
          control: role,
          hint: "A custom role is defined on the Roles screen. Choosing Customer creates an account with no staff access.",
        }),
        h(
          "label",
          { class: "checkbox" },
          verified,
          h("span", null, "Mark the address as verified"),
        ),
        banner(
          "info",
          "The password is never returned",
          "No response from this screen contains it, and the audit row records only that it was set.",
        ),
      ),
      onSubmit: async () => {
        if (!email.value.trim()) return "An email address is required.";
        if (!name.value.trim()) return "A display name is required.";
        if (!password.value) return "A password is required.";

        try {
          const created = await ctx.api.post<UserRow>("/api/v1/staff/users", {
            email: email.value.trim(),
            display_name: name.value.trim(),
            password: password.value,
            role: role.value,
            email_verified: verified.checked,
          });
          ctx.toast("Account created", {
            tone: "success",
            detail: created.is_staff ? `${created.email} · ${roleLabel(created.role)}` : created.email,
          });
          await table.reload(queryOf());
          return undefined;
        } catch (err) {
          return createError(err);
        }
      },
    });
  }

  // ----------------------------------------------------------------- edit --

  async function openEdit(row: UserRow): Promise<void> {
    await loadRoles();

    const name = input({ id: "edit-name", name: "display_name", value: row.display_name, maxlength: 200 });
    const email = input({ id: "edit-email", name: "email", type: "email", value: row.email });
    const role = roleSelect(row.role, "edit-role");
    const status = select(
      [
        { value: "active", label: "Active" },
        { value: "disabled", label: "Disabled — cannot sign in" },
        { value: "pending", label: "Pending — awaiting verification" },
      ],
      { id: "edit-status", value: row.status },
    );

    await ctx.formDialog({
      title: `Edit ${row.display_name || row.email}`,
      subtitle: "Only the fields you change are sent, so a rename cannot clear a role.",
      submitLabel: "Save changes",
      body: h(
        "div",
        { class: "stack" },
        field({ label: "Display name", control: name, required: true }),
        field({
          label: "Email",
          control: email,
          required: true,
          hint: "Changing the address clears its verification: a confirmation given for one mailbox does not transfer to another.",
        }),
        field({ label: "Role", control: role }),
        field({
          label: "Status",
          control: status,
          hint: "Disabling ends every session the account has. It is almost always the right answer where deleting is refused.",
        }),
        row.is_staff
          ? banner(
              "warning",
              "This account is staff",
              "Demoting or disabling the only active administrator is refused, by the database as well as by the server.",
            )
          : null,
      ),
      onSubmit: async () => {
        const patch: Record<string, string> = {};
        if (name.value.trim() !== row.display_name) patch.display_name = name.value.trim();
        if (email.value.trim() !== row.email) patch.email = email.value.trim();
        if (role.value !== row.role) patch.role = role.value;
        if (status.value !== row.status) patch.status = status.value;

        if (Object.keys(patch).length === 0) {
          ctx.toast("Nothing changed", { tone: "info" });
          return undefined;
        }

        try {
          await ctx.api.patch<UserRow>(`/api/v1/staff/users/${row.id}`, patch);
          ctx.toast("Account updated", { tone: "success", detail: row.email });
          await table.reload(queryOf());
          return undefined;
        } catch (err) {
          return updateError(err);
        }
      },
    });
  }

  // ------------------------------------------------------------- password --

  async function openPassword(row: UserRow): Promise<void> {
    const password = input({
      id: "password-new",
      name: "password",
      type: "password",
      autocomplete: "new-password",
      placeholder: "At least 12 characters",
    });
    const confirm = input({
      id: "password-confirm",
      name: "confirm",
      type: "password",
      autocomplete: "new-password",
      placeholder: "Type it again",
    });

    await ctx.formDialog({
      title: `Set a password for ${row.display_name || row.email}`,
      subtitle: "Every session the account has is ended, because whoever held the old password may still be holding one.",
      submitLabel: "Set password",
      body: h(
        "div",
        { class: "stack" },
        field({
          label: "New password",
          control: password,
          required: true,
          hint: "At least 12 characters — the same rule registration applies.",
        }),
        field({ label: "Repeat it", control: confirm, required: true }),
        banner(
          "info",
          "You will know this password",
          "Tell the user to change it from their security screen once they are in. The audit trail records that you set it.",
        ),
      ),
      onSubmit: async () => {
        if (!password.value) return "A password is required.";
        if (password.value !== confirm.value) return "The two passwords do not match.";

        try {
          const result = await ctx.api.post<{ status: string; sessions_revoked: number }>(
            `/api/v1/staff/users/${row.id}/password`,
            { password: password.value },
          );
          ctx.toast("Password set", {
            tone: "success",
            detail:
              result.sessions_revoked > 0
                ? `${result.sessions_revoked} session(s) ended · ${row.email}`
                : row.email,
          });
          await table.reload(queryOf());
          return undefined;
        } catch (err) {
          if (err instanceof ApiError && err.code === "weak_password") {
            return err.detail || "That password is too short.";
          }
          if (err instanceof ApiError) return err.detail || `The password was refused (${err.code}).`;
          return "The server could not be reached, so no password was set.";
        }
      },
    });
  }

  // --------------------------------------------------------------- status --

  async function setStatus(row: UserRow, status: "active" | "disabled"): Promise<void> {
    const disabling = status === "disabled";
    const ok = await ctx.confirm({
      title: disabling ? `Disable ${row.email}?` : `Enable ${row.email}?`,
      body: disabling
        ? "The account stops being able to sign in and every live session ends. Nothing is deleted, and the history stays readable."
        : "The account can sign in again with its existing password.",
      confirmLabel: disabling ? "Disable account" : "Enable account",
      danger: disabling,
    });
    if (!ok) return;

    try {
      await ctx.api.patch<UserRow>(`/api/v1/staff/users/${row.id}`, { status });
      ctx.toast(disabling ? "Account disabled" : "Account enabled", {
        tone: "success",
        detail: row.email,
      });
      await table.reload(queryOf());
    } catch (err) {
      ctx.toast(describeFailure(err, "The account could not be changed"), {
        tone: "danger",
        detail: err instanceof ApiError ? err.code : undefined,
      });
    }
  }

  // --------------------------------------------------------------- delete --

  async function deleteUser(row: UserRow): Promise<void> {
    const ok = await ctx.confirm({
      title: `Delete ${row.email}?`,
      body:
        "An account with invoices or audit history cannot be deleted: those records have to outlive it, and the " +
        "refusal says what holds it. Disabling is the usual answer. If this account is genuinely unused, typing " +
        "the address confirms the delete, which cannot be undone.",
      confirmLabel: "Delete account",
      danger: true,
      phrase: row.email,
    });
    if (!ok) return;

    try {
      await ctx.api.del(`/api/v1/staff/users/${row.id}`);
      ctx.toast("Account deleted", { tone: "success", detail: row.email });
      await table.reload(queryOf());
    } catch (err) {
      ctx.toast(describeFailure(err, "The account could not be deleted"), {
        tone: "danger",
        detail: err instanceof ApiError ? err.code : undefined,
      });
    }
  }

  // -------------------------------------------------------------- toolbar --

  let timer: number | undefined;
  ctx.onDispose(() => {
    if (timer !== undefined) window.clearTimeout(timer);
  });

  const search = searchInput({
    value: term,
    placeholder: "Search by address or name",
    oninput: (value) => {
      term = value.trim();
      hint.hidden = term.length !== 1;
      if (timer !== undefined) window.clearTimeout(timer);
      timer = window.setTimeout(() => {
        ctx.replaceQuery({ q: term || null, staff: scope || null });
        void table.reload(queryOf());
      }, 250);
    },
  });

  const scopeSelect = select(
    [
      { value: "", label: "Everyone" },
      { value: "true", label: "Staff only" },
      { value: "false", label: "Customers only" },
    ],
    {
      id: "user-scope",
      value: scope,
      onchange: (event: Event) => {
        scope = (event.target as HTMLSelectElement).value;
        ctx.replaceQuery({ staff: scope || null, q: term || null });
        void table.reload(queryOf());
      },
    },
  );

  const toolbar = h(
    "div",
    { class: "toolbar" },
    search,
    h("label", { class: "row", for: "user-scope" }, h("span", { class: "faint" }, "Show"), scopeSelect),
    hint,
    h(
      "div",
      { class: "toolbar-end" },
      btn("Refresh", { icon: "refresh", size: "sm", onclick: () => void table.reload(queryOf()) }),
    ),
  );

  describeEmpty();

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Users",
      sub: "Everyone who can sign in: the agents and administrators who run the panel, and the customers who own an account.",
      actions: h(
        "div",
        { class: "row-wrap" },
        btn("Roles", { href: "/roles", icon: "shield", size: "sm" }),
        btn("Create user", { variant: "primary", icon: "plus", onclick: () => void openCreate() }),
      ),
    }),
    banner(
      "info",
      "Disable is the safe operation, delete is the rare one",
      "An account with invoices or audit history cannot be deleted, because those records have to outlive it. Disabling ends its sessions and keeps the history readable.",
    ),
    h("div", { class: "stack" }, toolbar, table.el),
  );
}

// ---------------------------------------------------------------- helpers --

/** "infrastructure" → "Infrastructure", "support-lead" → "Support lead". */
function roleLabel(role: string): string {
  if (!role) return "Customer";
  const spaced = role.replace(/[-_]/g, " ");
  return spaced.charAt(0).toUpperCase() + spaced.slice(1);
}

function statusTone(status: string): Tone {
  switch (status) {
    case "active":
      return "success";
    case "pending":
      return "warning";
    case "disabled":
      return "danger";
    default:
      return "neutral";
  }
}

/** The create dialog's error text, by the server's stable code. */
function createError(err: unknown): string {
  if (!(err instanceof ApiError)) return "The server could not be reached, so no account was created.";
  switch (err.code) {
    case "email_taken":
      return "That address already has an account. Edit it instead, or reset its password.";
    case "weak_password":
      return err.detail || "That password is too short.";
    case "invalid_email":
      return "That is not a usable email address.";
    case "invalid_user":
      return err.detail || "The account details were refused.";
    case "unknown_role":
      return err.detail || "That role does not exist.";
    case "auth_not_configured":
      return "Authentication is not configured on this server.";
    default:
      return err.detail || `The account could not be created (${err.code}).`;
  }
}

/** The edit dialog's error text. The two lockout codes are the ones worth naming. */
function updateError(err: unknown): string {
  if (!(err instanceof ApiError)) return "The server could not be reached, so nothing was changed.";
  switch (err.code) {
    case "last_administrator":
      return "This is the last active administrator. Grant administrator to somebody else first.";
    case "self_lockout":
      return "You cannot disable the account you are signed in as.";
    case "email_taken":
      return "Another account already has that address.";
    case "invalid_email":
      return "That is not a usable email address.";
    case "unknown_role":
      return err.detail || "That role does not exist.";
    case "invalid_user":
      return err.detail || "The change was refused.";
    default:
      return err.detail || `The account could not be changed (${err.code}).`;
  }
}

/** A toast message for a refusal whose detail is worth showing. */
function describeFailure(err: unknown, fallback: string): string {
  if (!(err instanceof ApiError)) return fallback;
  switch (err.code) {
    case "last_administrator":
      return "The last active administrator cannot be changed this way";
    case "self_lockout":
      return "You cannot lock yourself out";
    case "user_has_history":
      return "That account has history, so it cannot be deleted — disable it instead";
    default:
      return err.detail || fallback;
  }
}
