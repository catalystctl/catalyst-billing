// API keys.
//
// Keys are per-user, and the list never contains one. The create response carries the key
// exactly once (`key` + `shown_once: true`) and the server keeps only a SHA-256 hash, so the
// only moment the value exists in a browser is between the 201 and the operator dismissing
// the dialog. That single fact shapes this screen:
//
//  - The key is displayed in a locked dialog, not a toast. A toast disappears on a timer,
//    and a secret that disappears is a secret that was never copied. The dialog has one
//    exit: "I saved it".
//  - The list is reloaded from the create response's *other* fields, never from the key. No
//    state on this page holds the secret after the dialog is gone, so nothing can render it
//    a second time.
//  - A scope the operator does not hold is refused by the server with `scope_not_held`. The
//    create dialog shows that refusal in place, because the fix is to change the scope list.

import { ApiError } from "../lib/api";
import { h } from "../lib/dom";
import { dateTime, humanStatus, relative, type Tone } from "../lib/format";
import type { Ctx } from "../lib/types";
import {
  badge,
  banner,
  btn,
  card,
  copyButton,
  emptyState,
  field,
  input,
  metaGrid,
  mono,
  pageHead,
} from "../ui/kit";
import { openDialog, type DialogHandle } from "../ui/dialog";
import { createTable, type Column } from "../ui/table";

export const meta = { title: "API keys", crumb: "API keys" };

/** The list shape from `apiKeyResponse` in internal/httpapi/apikeys.go. `revoked` and
 *  `expired` are booleans the server computes; the timestamps may be null. */
interface APIKeyRow {
  id: string;
  name: string;
  prefix: string;
  scopes: string[];
  created_at: string;
  last_used_at: string | null;
  expires_at: string | null;
  revoked_at: string | null;
  revoked: boolean;
  expired: boolean;
}

/** The 201: the list shape plus the secret and the contract flag that says it is the only
 *  time it is sent. */
interface CreatedKey extends APIKeyRow {
  key: string;
  shown_once: boolean;
}

export default async function apiKeysPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("apikey:manage")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "API keys" }),
      card({
        body: emptyState({
          title: "Not available for your role",
          body: "Managing API keys needs the apikey:manage permission.",
          icon: "lock",
        }),
      }),
    );
  }

  const tableHost = h("div");
  const empty: { title: string; body?: string; action?: Node } = { title: "", body: "" };

  const columns: Column<APIKeyRow>[] = [
    {
      key: "name",
      label: "Name",
      primary: true,
      render: (row) =>
        h(
          "div",
          { class: "cell-main" },
          h("div", { class: "primary" }, row.name),
          row.revoked_at
            ? h("div", { class: "secondary" }, `revoked ${relative(row.revoked_at)}`)
            : null,
        ),
    },
    {
      key: "prefix",
      label: "Prefix",
      render: (row) => mono(row.prefix),
    },
    {
      key: "scopes",
      label: "Scopes",
      render: (row) =>
        row.scopes.length === 0
          ? h("span", { class: "faint" }, "All your permissions")
          : h(
              "div",
              { class: "trust-scopes" },
              ...row.scopes.map((scope) => h("span", { class: "chip mono" }, scope)),
            ),
    },
    {
      key: "created",
      label: "Created",
      align: "right",
      render: (row) => h("span", { class: "dim nowrap", title: dateTime(row.created_at) }, relative(row.created_at)),
    },
    {
      key: "last_used",
      label: "Last used",
      align: "right",
      hideSmall: true,
      render: (row) =>
        row.last_used_at
          ? h("span", { class: "dim nowrap", title: dateTime(row.last_used_at) }, relative(row.last_used_at))
          : h("span", { class: "faint nowrap" }, "Never"),
    },
    {
      key: "expires",
      label: "Expires",
      align: "right",
      render: (row) =>
        row.expires_at
          ? h("span", { class: "dim nowrap", title: dateTime(row.expires_at) }, relative(row.expires_at))
          : h("span", { class: "faint nowrap" }, "Never"),
    },
    {
      key: "status",
      label: "Status",
      render: (row) => {
        const state = keyState(row);
        return badge(state.label, state.tone, { dot: true });
      },
    },
    {
      key: "actions",
      label: "",
      align: "right",
      width: "1%",
      render: (row) =>
        row.revoked
          ? h("span", { class: "faint" }, "—")
          : h(
              "div",
              { class: "row-wrap" },
              // The network restriction is a property of a live key, so it is offered on the
              // row rather than in the create form: a key that leaks is narrowed first and
              // revoked second, and narrowing must not need a new key.
              btn("Networks", {
                size: "sm",
                icon: "globe",
                title: "Restrict this key to a set of networks",
                onclick: () => void editAllowlist(row),
              }),
              btn("Revoke", {
                size: "sm",
                variant: "danger",
                icon: "trash",
                title: "Revoke this key",
                onclick: () => void revokeKey(row),
              }),
            ),
    },
  ];

  const table = createTable<APIKeyRow>(ctx, {
    path: "/api/v1/staff/api-keys",
    columns,
    // The endpoint returns `{data}` without a cursor: a user has a handful of keys, so one
    // request is the whole list and the pager stays disabled.
    empty,
    rowKey: (row) => row.id,
    footNote: "Keys are per-user: this list is yours alone.",
  });
  tableHost.appendChild(table.el);

  function describeEmpty(): void {
    empty.title = "No API keys yet";
    empty.body =
      "A key carries your permissions, narrowed by the scopes you choose, and is shown once when it is created. Create one for the script or integration that will use it.";
    empty.action = btn("Create key", { size: "sm", icon: "plus", onclick: () => openCreate() });
  }

  function openCreate(): void {
    const name = input({
      id: "key-name",
      name: "name",
      placeholder: "Nightly invoice export",
      autocomplete: "off",
      maxlength: 80,
    });
    const scopes = input({
      id: "key-scopes",
      name: "scopes",
      class: "input--mono",
      placeholder: "invoice:read:all, payment:refund",
      autocomplete: "off",
    });
    const days = input({
      id: "key-days",
      name: "expires_in_days",
      type: "number",
      min: 1,
      max: 3650,
      step: 1,
      inputmode: "numeric",
      placeholder: "90",
    });

    const held = [...ctx.session.permissions].sort();
    const heldList = h(
      "details",
      { class: "trust-help" },
      h("summary", null, `Permissions you hold (${held.length})`),
      h(
        "div",
        { class: "trust-scope-list" },
        ...held.map((permission) =>
          h(
            "button",
            {
              class: "chip mono trust-scope-pick",
              type: "button",
              title: `Add ${permission}`,
              onclick: () => addScope(permission),
            },
            permission,
          ),
        ),
      ),
    );

    function addScope(permission: string): void {
      const current = parseScopes(scopes.value);
      if (current.includes(permission)) return;
      current.push(permission);
      scopes.value = current.join(", ");
    }

    void ctx.formDialog({
      title: "Create an API key",
      subtitle: "The key is shown once, in the next dialog. Have the place that will store it ready.",
      submitLabel: "Create key",
      body: h(
        "div",
        { class: "stack" },
        field({
          label: "Name",
          control: name,
          required: true,
          hint: "What will use this key? A name you will recognise in six months.",
        }),
        field({
          label: "Scopes",
          control: scopes,
          hint: "Comma-separated permission names. Leave empty to carry everything you hold. You cannot grant a scope you do not hold: the server refuses it with scope_not_held.",
        }),
        heldList,
        field({
          label: "Expires in days",
          control: days,
          hint: "Optional. 1–3650 days; leave it empty for a key that does not expire.",
        }),
      ),
      onSubmit: async () => {
        const keyName = name.value.trim();
        if (!keyName) return "A key needs a name.";

        const rawDays = days.value.trim();
        let expiresInDays = 0;
        if (rawDays !== "") {
          const parsed = Number(rawDays);
          if (!Number.isInteger(parsed) || parsed < 1 || parsed > 3650) {
            return "Expires in days must be a whole number between 1 and 3650, or empty.";
          }
          expiresInDays = parsed;
        }

        try {
          const created = await ctx.api.post<CreatedKey>("/api/v1/staff/api-keys", {
            name: keyName,
            scopes: parseScopes(scopes.value),
            expires_in_days: expiresInDays,
          });
          // The list is refreshed from the list endpoint, which has no key in it. The only
          // copy of the secret is the one in the dialog below.
          void table.reload();
          showKeyOnce(created);
          return undefined;
        } catch (err) {
          if (!(err instanceof ApiError)) return "The server could not be reached, so no key was created.";
          switch (err.code) {
            case "scope_not_held":
              return err.detail || "You cannot grant a scope you do not hold.";
            case "name_required":
              return "A key needs a name.";
            case "invalid_expiry":
              return "Expires in days must be between 1 and 3650, or absent.";
            case "invalid_key_request":
              return err.detail || "The key request was refused.";
            case "auth_not_configured":
              return "API keys are not configured on this server.";
            default:
              return err.detail || `The key could not be created (${err.code}).`;
          }
        }
      },
    });
  }

  /** The one-time display. Locked: Escape, the backdrop and the close button are all
   *  removed as exits, because the value behind them cannot be recovered. */
  function showKeyOnce(created: CreatedKey): void {
    let saved = false;
    let handle: DialogHandle | null = null;
    const secret = created.key;

    handle = openDialog({
      title: "Copy this key now",
      subtitle: "This is the only time it is shown. The server keeps a hash, not the key.",
      body: h(
        "div",
        { class: "stack" },
        banner(
          "danger",
          "This key is never shown again",
          "It is not stored anywhere it can be read back: only a SHA-256 hash is kept. If you close this dialog without copying it, the key is gone — revoke it and create another.",
        ),
        h(
          "div",
          { class: "trust-key-box" },
          h("code", { class: "trust-key" }, secret),
          h("div", { class: "trust-key-actions" }, copyButton(ctx, secret, "Copy key")),
        ),
        metaGrid([
          { label: "Name", value: created.name },
          { label: "Prefix", value: mono(created.prefix) },
          {
            label: "Scopes",
            value: created.scopes.length > 0 ? created.scopes.join(", ") : "All your permissions",
          },
          { label: "Expires", value: created.expires_at ? dateTime(created.expires_at) : "Never" },
        ]),
        h(
          "p",
          { class: "faint" },
          created.shown_once
            ? "The create response is marked shown_once: this dialog is the only place the key ever appears."
            : "The create response did not confirm shown_once. Treat this key as unrecoverable once this dialog closes.",
        ),
      ),
      footer: h(
        "div",
        { class: "row spacer" },
        btn("I saved it", {
          variant: "primary",
          icon: "check",
          onclick: () => {
            saved = true;
            handle?.close();
          },
        }),
      ),
      locked: () => !saved,
    });

    // The header's close button is a second way out of a dialog whose only exit must be a
    // deliberate acknowledgement.
    handle.el.querySelector(".dialog-close")?.remove();

    // Navigating away must not leave an overlay that cannot be dismissed behind: the lock is
    // about the operator's click, not about trapping them in a page that no longer exists.
    ctx.onDispose(() => {
      saved = true;
      handle?.close();
    });
  }

  /** The network restriction for one key.
   *
   *  It is a separate read and a separate write because the list does not carry it: the
   *  allowlist is only interesting for the key you are about to change, and fetching it per row
   *  would put a second query behind a table render. The dialog reads the current value, so an
   *  operator edits the rule rather than replacing it blind. */
  async function editAllowlist(row: APIKeyRow): Promise<void> {
    let current: string[];
    try {
      const body = await ctx.api.get<{ ip_allowlist: string[] }>(
        `/api/v1/staff/api-keys/${row.id}/ip-allowlist`,
        { cache: false },
      );
      current = Array.isArray(body.ip_allowlist) ? body.ip_allowlist : [];
    } catch (err) {
      ctx.toast(err instanceof ApiError ? err.detail || humanStatus(err.code) : "The networks could not be read", {
        tone: "danger",
        detail: err instanceof ApiError ? err.code : undefined,
      });
      return;
    }

    const ranges = input({
      id: "key-allowlist",
      name: "ip_allowlist",
      class: "input--mono",
      placeholder: "203.0.113.0/24, 198.51.100.7/32",
      value: current.join(", "),
      autocomplete: "off",
    });

    void ctx.formDialog({
      title: `Networks for “${row.name}”`,
      subtitle: current.length === 0
        ? "This key works from any address. A restriction narrows it without changing the key."
        : "This key works only from these networks. Remove them all to accept it from anywhere.",
      submitLabel: "Save networks",
      body: field({
        label: "Allowed networks (CIDR)",
        control: ranges,
        hint: "Comma-separated IPv4 or IPv6 ranges, for example 203.0.113.0/24. Empty means no restriction. A request from any other address is refused as if the key did not exist.",
      }),
      onSubmit: async () => {
        const list = ranges.value
          .split(",")
          .map((entry) => entry.trim())
          .filter(Boolean);
        try {
          const saved = await ctx.api.put<{ ip_allowlist: string[] }>(
            `/api/v1/staff/api-keys/${row.id}/ip-allowlist`,
            { ip_allowlist: list },
          );
          const stored = Array.isArray(saved.ip_allowlist) ? saved.ip_allowlist : list;
          // The list does not carry the allowlist, so there is nothing to reload: the
          // confirmation is the toast, and reopening the dialog reads the stored rule.
          ctx.toast("Networks saved", {
            tone: "success",
            detail: stored.length > 0 ? stored.join(", ") : "Any network",
          });
          return undefined;
        } catch (err) {
          if (!(err instanceof ApiError)) return "The server could not be reached, so nothing changed.";
          switch (err.code) {
            case "invalid_allowlist":
              return err.detail || "One of those is not a CIDR range.";
            case "not_found":
              return "That key no longer exists. Reload the page.";
            default:
              return err.detail || `The networks could not be saved (${err.code}).`;
          }
        }
      },
    });
  }

  async function revokeKey(row: APIKeyRow): Promise<void> {
    const ok = await ctx.confirm({
      title: `Revoke “${row.name}”?`,
      body: "Anything using this key stops working immediately. A revoked key cannot be restored — create a new one and give it to the caller instead.",
      confirmLabel: "Revoke key",
      danger: true,
    });
    if (!ok) return;

    try {
      await ctx.api.del(`/api/v1/staff/api-keys/${row.id}`);
      ctx.toast("Key revoked", { tone: "success", detail: `${row.name} · ${row.prefix}` });
      await table.reload();
    } catch (err) {
      ctx.toast(err instanceof ApiError ? err.detail || humanStatus(err.code) : "The key could not be revoked", {
        tone: "danger",
        detail: err instanceof ApiError ? err.code : undefined,
      });
    }
  }

  describeEmpty();

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "API keys",
      sub: "Credentials for your own automation. A key carries your permissions narrowed by its scopes, and stops working the moment it is revoked or your roles change.",
      actions: btn("Create key", { variant: "primary", icon: "plus", onclick: () => openCreate() }),
    }),
    banner(
      "info",
      "A key is shown once",
      "The create response is the only one that contains the key: the server keeps a hash, so there is no screen and no endpoint that reveals it again.",
    ),
    tableHost,
  );
}

// ------------------------------------------------------------------ helpers --

function keyState(row: APIKeyRow): { label: string; tone: Tone } {
  if (row.revoked) return { label: "Revoked", tone: "danger" };
  if (row.expired) return { label: "Expired", tone: "warning" };
  return { label: "Active", tone: "success" };
}

/** "a, b c" and "a,b,c" are the same list; an empty entry is not a scope. */
function parseScopes(raw: string): string[] {
  const seen = new Set<string>();
  for (const part of raw.split(/[\s,]+/)) {
    const scope = part.trim();
    if (scope) seen.add(scope);
  }
  return [...seen];
}
