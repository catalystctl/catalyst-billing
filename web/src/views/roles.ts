// Roles.
//
// The screen that answers "who can do this". It shows the four system roles read-only — they
// are immutable in the database by trigger, because their permission sets encode a separation
// of duties the approval flow depends on (billing can refund and cannot terminate,
// infrastructure the reverse) — and the custom roles an operator defines, which are editable.
//
// Every role shows two numbers that are the point of the screen: how many accounts hold it,
// and how many of those can actually sign in. A role held only by disabled accounts grants
// nothing, and that is exactly what an operator needs to know before deleting it or judging
// what it is worth.
//
// The permission list is *served*, never embedded: a client that hard-coded the catalogue
// would offer a permission this build does not check, and the server would refuse the whole
// submit. `GET /api/v1/staff/permissions` is the same set the authorizer uses.
//
// Deleting a role somebody holds is refused rather than reassigned. A silent reassignment
// changes somebody's access without a decision, and "my permissions changed and nobody told
// me" is an incident: the operator moves the holders on the Users screen first, which is a
// deliberate act.

import { ApiError } from "../lib/api";
import { h } from "../lib/dom";
import { dateTime, relative } from "../lib/format";
import type { Ctx } from "../lib/types";
import {
  badge,
  banner,
  btn,
  card,
  emptyState,
  field,
  input,
  mono,
  pageHead,
  swap,
  textarea,
} from "../ui/kit";

export const meta = { title: "Roles", crumb: "Roles" };

/** The role shape from `roleJSON` in internal/httpapi/roles_ext.go. */
interface RoleRow {
  id: string;
  name: string;
  description: string;
  is_system: boolean;
  created_at: string;
  permissions: string[];
  holders: number;
  active_holders: number;
}

interface Catalogue {
  permissions: string[];
  system: string[];
}

export default async function rolesPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("staff:manage")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Roles" }),
      card({
        body: emptyState({
          title: "Not available for your role",
          body: "Defining roles needs the staff:manage permission.",
          icon: "lock",
        }),
      }),
    );
  }

  const host = h("div", { class: "stack" });
  let roles: RoleRow[] = [];
  let catalogue: Catalogue = { permissions: [], system: [] };

  async function load(): Promise<void> {
    swap(host, h("div", { class: "faint" }, "Loading roles…"));
    try {
      const [rolePage, perms] = await Promise.all([
        ctx.api.get<{ data: RoleRow[] }>("/api/v1/staff/roles"),
        ctx.api.get<{ data: string[]; system: string[] }>("/api/v1/staff/permissions"),
      ]);
      roles = rolePage.data;
      catalogue = { permissions: perms.data, system: perms.system };
      render();
    } catch (err) {
      swap(
        host,
        card({
          body: emptyState({
            title: "Roles could not be loaded",
            body: err instanceof ApiError ? err.detail || err.code : "The request failed.",
            icon: "alertCircle",
          }),
        }),
      );
    }
  }

  function render(): void {
    const system = roles.filter((role) => role.is_system);
    const custom = roles.filter((role) => !role.is_system);

    swap(
      host,
      h(
        "div",
        { class: "stack" },
        card({
          title: "System roles",
          sub: "Shipped with the panel and immutable: the separation of duties is written against these four.",
          flush: true,
          body: roleTable(system, false),
        }),
        card({
          title: "Custom roles",
          sub:
            custom.length === 0
              ? "A role you define, with the permissions you choose."
              : `${custom.length} defined here. Assigning one happens on the Users screen.`,
          flush: custom.length > 0,
          body:
            custom.length === 0
              ? emptyState({
                  title: "No custom roles yet",
                  body:
                    "A custom role is for a job the four shipped roles do not describe — a support lead who can also " +
                    "hold services, or a clerk who may read the audit log but not write it. Define one here and " +
                    "assign it on the Users screen.",
                  action: btn("Create role", {
                    size: "sm",
                    icon: "plus",
                    onclick: () => void openCreate(),
                  }),
                })
              : roleTable(custom, true),
        }),
        banner(
          "info",
          "Deleting a role somebody holds is refused",
          "Reassigning silently would change somebody's access without a decision. Move the holders on the Users screen first, then delete the role.",
        ),
      ),
    );
  }

  /** One table for both sections: a system role differs only in having no actions. */
  function roleTable(rows: RoleRow[], editable: boolean): HTMLElement {
    return h(
      "div",
      { class: "table-wrap" },
      h(
        "table",
        { class: "table" },
        h(
          "thead",
          null,
          h(
            "tr",
            null,
            h("th", { scope: "col" }, "Role"),
            h("th", { scope: "col" }, "Grants"),
            h("th", { scope: "col", class: "num" }, "Holders"),
            h("th", { scope: "col", class: "num" }, "Active"),
            h("th", { scope: "col" }, "Created"),
            h("th", { scope: "col", width: "1%" }, ""),
          ),
        ),
        h(
          "tbody",
          null,
          ...rows.map((role) =>
            h(
              "tr",
              { "data-key": role.id },
              h(
                "td",
                { class: "cell-primary" },
                h(
                  "div",
                  { class: "row spacer" },
                  h("div", { class: "primary" }, role.name),
                  role.is_system ? badge("System", "neutral", { dot: true }) : null,
                ),
                h("div", { class: "secondary" }, role.description || "No description"),
              ),
              h("td", null, grantSummary(role)),
              h(
                "td",
                { class: "num" },
                h("span", { class: role.holders > 0 ? "mono" : "mono faint" }, String(role.holders)),
              ),
              h(
                "td",
                { class: "num" },
                h(
                  "span",
                  { class: role.active_holders > 0 ? "mono" : "mono faint" },
                  String(role.active_holders),
                ),
              ),
              h(
                "td",
                null,
                h("span", { class: "faint nowrap", title: dateTime(role.created_at) }, relative(role.created_at)),
              ),
              h(
                "td",
                { class: "num" },
                editable
                  ? h(
                      "div",
                      { class: "row-wrap" },
                      btn("Permissions", {
                        size: "sm",
                        icon: "shieldCheck",
                        onclick: () => void openPermissions(role),
                      }),
                      btn("Rename", {
                        size: "sm",
                        variant: "ghost",
                        icon: "edit",
                        onclick: () => void openRename(role),
                      }),
                      btn("Delete", {
                        size: "sm",
                        variant: "ghost",
                        icon: "trash",
                        onclick: () => void deleteRole(role),
                      }),
                    )
                  : h("span", { class: "faint", title: "A system role cannot be edited or deleted" }, "—"),
              ),
            ),
          ),
        ),
      ),
    );
  }

  // -------------------------------------------------------------- create --

  async function openCreate(): Promise<void> {
    const name = input({
      id: "role-name",
      name: "name",
      placeholder: "support-lead",
      autocomplete: "off",
      maxlength: 40,
    });
    const description = textarea({
      id: "role-description",
      name: "description",
      placeholder: "First line support, plus the authority to place a hold.",
      maxlength: 500,
    });
    const matrix = permissionMatrix(catalogue.permissions, []);

    await ctx.formDialog({
      title: "Create a custom role",
      subtitle: "A name, a description, and the permissions it grants.",
      submitLabel: "Create role",
      body: h(
        "div",
        { class: "stack" },
        field({
          label: "Name",
          control: name,
          required: true,
          hint: "Lower-case letters, digits, '-' and '_', 2–40 characters. The four shipped names are reserved.",
        }),
        field({ label: "Description", control: description, hint: "What job is this role for?" }),
        matrix.el,
      ),
      onSubmit: async () => {
        const chosen = matrix.values();
        if (!name.value.trim()) return "A role needs a name.";
        if (chosen.length === 0) return "A role with no permissions grants nothing: choose at least one.";

        try {
          await ctx.api.post<RoleRow>("/api/v1/staff/roles", {
            name: name.value.trim(),
            description: description.value.trim(),
            permissions: chosen,
          });
          ctx.toast("Role created", { tone: "success", detail: name.value.trim() });
          await load();
          return undefined;
        } catch (err) {
          return roleError(err, "The role could not be created");
        }
      },
    });
  }

  // ---------------------------------------------------------- permissions --

  async function openPermissions(role: RoleRow): Promise<void> {
    const matrix = permissionMatrix(catalogue.permissions, role.permissions);

    await ctx.formDialog({
      title: `Permissions for ${role.name}`,
      subtitle: "The submitted set replaces the stored one: what you see is what the role grants afterwards.",
      submitLabel: "Save permissions",
      body: h(
        "div",
        { class: "stack" },
        h(
          "p",
          { class: "faint" },
          `${role.holders} account${role.holders === 1 ? "" : "s"} hold this role. A change takes effect on their next request.`,
        ),
        matrix.el,
      ),
      onSubmit: async () => {
        const chosen = matrix.values();
        try {
          await ctx.api.put<RoleRow>(`/api/v1/staff/roles/${role.id}/permissions`, { permissions: chosen });
          ctx.toast("Permissions saved", {
            tone: "success",
            detail: `${role.name} · ${chosen.length} granted`,
          });
          await load();
          return undefined;
        } catch (err) {
          return roleError(err, "The permissions could not be saved");
        }
      },
    });
  }

  // -------------------------------------------------------------- rename --

  async function openRename(role: RoleRow): Promise<void> {
    const name = input({ id: "rename-name", name: "name", value: role.name, maxlength: 40 });
    const description = textarea({
      id: "rename-description",
      name: "description",
      value: role.description,
      maxlength: 500,
    });

    await ctx.formDialog({
      title: `Rename ${role.name}`,
      subtitle: "Renaming moves the accounts holding the role with it: permissions are resolved by name.",
      submitLabel: "Save",
      body: h(
        "div",
        { class: "stack" },
        field({ label: "Name", control: name, required: true }),
        field({ label: "Description", control: description }),
        role.holders > 0
          ? banner(
              "warning",
              `${role.holders} account${role.holders === 1 ? "" : "s"} hold this role`,
              "They keep the same permissions; only the name they are shown changes.",
            )
          : null,
      ),
      onSubmit: async () => {
        const patch: Record<string, string> = {};
        if (name.value.trim() !== role.name) patch.name = name.value.trim();
        if (description.value.trim() !== role.description) patch.description = description.value.trim();
        if (Object.keys(patch).length === 0) {
          ctx.toast("Nothing changed", { tone: "info" });
          return undefined;
        }
        try {
          await ctx.api.patch<RoleRow>(`/api/v1/staff/roles/${role.id}`, patch);
          ctx.toast("Role updated", { tone: "success", detail: name.value.trim() });
          await load();
          return undefined;
        } catch (err) {
          return roleError(err, "The role could not be updated");
        }
      },
    });
  }

  // -------------------------------------------------------------- delete --

  async function deleteRole(role: RoleRow): Promise<void> {
    const held = role.holders > 0;
    const ok = await ctx.confirm({
      title: `Delete the role “${role.name}”?`,
      body: held
        ? `${role.holders} account${role.holders === 1 ? "" : "s"} still hold this role, so the server will refuse ` +
          "until they are moved. Reassigning automatically would change their access without a decision."
        : "Nobody holds this role. Deleting it cannot be undone, but a role is only a name and a permission set: " +
          "creating it again is the same three fields.",
      confirmLabel: "Delete role",
      danger: true,
      phrase: held ? undefined : role.name,
    });
    if (!ok) return;

    try {
      await ctx.api.del(`/api/v1/staff/roles/${role.id}`);
      ctx.toast("Role deleted", { tone: "success", detail: role.name });
      await load();
    } catch (err) {
      if (err instanceof ApiError && err.code === "role_in_use") {
        ctx.toast("That role is still assigned", {
          tone: "danger",
          detail: "Move the accounts holding it to another role on the Users screen, then delete it.",
        });
        return;
      }
      ctx.toast(err instanceof ApiError ? err.detail || err.code : "The role could not be deleted", {
        tone: "danger",
        detail: err instanceof ApiError ? err.code : undefined,
      });
    }
  }

  // -------------------------------------------------------------- render --

  const page = h(
    "div",
    { class: "page" },
    pageHead({
      title: "Roles",
      sub: "What each role grants, and who holds it. The four system roles are immutable; a custom role is yours to define.",
      actions: h(
        "div",
        { class: "row-wrap" },
        btn("Users", { href: "/users", icon: "users", size: "sm" }),
        btn("Create role", {
          variant: "primary",
          icon: "plus",
          onclick: () => void openCreate(),
        }),
      ),
    }),
    banner(
      "info",
      "A role is a named set of permissions",
      "A user holds exactly one role. Their effective permissions are that role's set plus the customer baseline every account has, so a staff member can still see their own invoices.",
    ),
    host,
  );

  void load();
  return page;
}

// ---------------------------------------------------------------- helpers --

/** A grouped permission matrix. Returns the element and a reader for the checked values. */
function permissionMatrix(
  available: string[],
  selected: string[],
): { el: HTMLElement; values: () => string[] } {
  const chosen = new Set(selected);
  const boxes: HTMLInputElement[] = [];
  const count = h("span", { class: "faint" }, `${selected.length} of ${available.length} selected`);

  const groups = new Map<string, string[]>();
  for (const permission of available) {
    const prefix = permission.split(":")[0] ?? permission;
    const list = groups.get(prefix) ?? [];
    list.push(permission);
    groups.set(prefix, list);
  }

  const el = h(
    "div",
    { class: "stack" },
    h("div", { class: "row spacer" }, h("strong", null, "Permissions"), count),
    ...[...groups.entries()].map(([prefix, permissions]) =>
      h(
        "fieldset",
        { class: "stack-sm" },
        h("legend", null, prefix),
        ...permissions.map((permission) => {
          const box = h("input", { type: "checkbox", checked: chosen.has(permission) });
          box.dataset.permission = permission;
          box.addEventListener("change", () => {
            if (box.checked) chosen.add(permission);
            else chosen.delete(permission);
            count.textContent = `${chosen.size} of ${available.length} selected`;
          });
          boxes.push(box);
          return h("label", { class: "row" }, box, mono(permission));
        }),
      ),
    ),
  );

  return {
    el,
    values: () => boxes.filter((box) => box.checked).map((box) => box.dataset.permission ?? ""),
  };
}

/** A compact "what it grants" cell: the count plus the first few names, expandable. */
function grantSummary(role: RoleRow): HTMLElement {
  if (role.permissions.length === 0) return h("span", { class: "faint" }, "Nothing");
  const shown = role.permissions.slice(0, 2);
  return h(
    "div",
    { class: "stack-sm" },
    h(
      "div",
      { class: "row-wrap" },
      ...shown.map((permission) => h("span", { class: "chip mono" }, permission)),
      role.permissions.length > shown.length
        ? h("span", { class: "faint" }, `+${role.permissions.length - shown.length} more`)
        : null,
    ),
    h(
      "details",
      { class: "trust-help" },
      h("summary", null, `All ${role.permissions.length} permission${role.permissions.length === 1 ? "" : "s"}`),
      h(
        "div",
        { class: "row-wrap" },
        ...role.permissions.map((permission) => h("span", { class: "chip mono" }, permission)),
      ),
    ),
  );
}

function roleError(err: unknown, fallback: string): string {
  if (!(err instanceof ApiError)) return `${fallback}: the server could not be reached.`;
  switch (err.code) {
    case "system_role":
      return "A system role cannot be changed: the separation of duties depends on it.";
    case "role_in_use":
      return err.detail || "That role is still assigned to somebody.";
    case "invalid_role":
      return err.detail || "The role name or description was refused.";
    case "unknown_permission":
      return err.detail || "One of the permissions is not one this build declares.";
    case "not_found":
      return "That role no longer exists.";
    default:
      return err.detail || `${fallback} (${err.code}).`;
  }
}
