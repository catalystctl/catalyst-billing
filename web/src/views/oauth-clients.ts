// OAuth2 clients and social providers.
//
// Two lists on one screen because they are two halves of the same question: which applications
// may act for a customer, and which identity providers may sign one in. The screen is shaped by
// three facts about the server:
//
//  - **A secret is shown once.** The create and rotate responses are the only ones that carry
//    it; the server keeps a SHA-256 digest, so there is no endpoint that can reveal it again.
//    The dialog below is therefore locked — Escape, the backdrop and the close button are not
//    exits — and the list is reloaded from the list endpoint, which has no secret in it.
//
//  - **A redirect URI is exact.** There is no prefix match and no wildcard on the server, so
//    the form says so rather than letting an operator type `https://app.example/*` and wonder
//    why the authorization request is refused.
//
//  - **A provider is data, not code.** Google, GitHub and Discord are rows. The endpoints can be
//    left blank when the issuer publishes OIDC discovery, which is why the form asks for the
//    issuer first and calls the three endpoints "advanced".

import { ApiError } from "../lib/api";
import { h } from "../lib/dom";
import { dateTime, relative, type Tone } from "../lib/format";
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
  textarea,
} from "../ui/kit";
import { openDialog, type DialogHandle } from "../ui/dialog";
import { createTable, type Column } from "../ui/table";

export const meta = { title: "OAuth clients", crumb: "OAuth clients" };

/** A registered client. There is no secret field: the list endpoint never has one. */
interface ClientRow {
  id: string;
  client_id: string;
  name: string;
  confidential: boolean;
  has_secret: boolean;
  redirect_uris: string[];
  allowed_scopes: string[];
  enabled: boolean;
  created_at: string;
  secret_rotated_at: string | null;
}

/** The 201/200 from create and rotate: the row plus the secret, once. */
interface ClientWithSecret extends ClientRow {
  secret?: string;
  shown_once?: boolean;
}

/** A configured social provider, with the secret redacted to a flag. */
interface ProviderRow {
  id: string;
  slug: string;
  name: string;
  issuer_url: string;
  authorization_endpoint: string;
  token_endpoint: string;
  userinfo_endpoint: string;
  client_id: string;
  has_secret: boolean;
  scopes: string[];
  subject_claim: string;
  email_claim: string;
  email_verified_claim: string;
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

export default async function oauthClientsPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("oauth:manage")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "OAuth clients" }),
      card({
        body: emptyState({
          title: "Not available for your role",
          body: "Registering OAuth2 clients needs the oauth:manage permission. A client acts on behalf of a customer, so registering one is an access decision.",
          icon: "lock",
        }),
      }),
    );
  }

  // ------------------------------------------------------------- clients --

  const clientsHost = h("div");
  const clientsEmpty: { title: string; body?: string; action?: Node } = { title: "", body: "" };

  const clientColumns: Column<ClientRow>[] = [
    {
      key: "name",
      label: "Client",
      primary: true,
      render: (row) =>
        h(
          "div",
          { class: "cell-main" },
          h("div", { class: "primary" }, row.name),
          h("div", { class: "secondary mono" }, row.client_id),
        ),
    },
    {
      key: "redirects",
      label: "Redirect URIs",
      render: (row) =>
        h(
          "div",
          { class: "trust-scopes" },
          ...row.redirect_uris.map((uri) => h("span", { class: "chip mono", title: uri }, shorten(uri))),
        ),
    },
    {
      key: "scopes",
      label: "Allowed scopes",
      render: (row) =>
        h(
          "div",
          { class: "trust-scopes" },
          ...row.allowed_scopes.map((scope) => h("span", { class: "chip mono" }, scope)),
        ),
    },
    {
      key: "kind",
      label: "Kind",
      render: (row) =>
        row.has_secret
          ? badge("Confidential", "neutral", { dot: true })
          : badge("Public · PKCE", "info", { dot: true }),
    },
    {
      key: "created",
      label: "Created",
      align: "right",
      hideSmall: true,
      render: (row) =>
        h("span", { class: "dim nowrap", title: dateTime(row.created_at) }, relative(row.created_at)),
    },
    {
      key: "actions",
      label: "",
      align: "right",
      width: "1%",
      render: (row) =>
        h(
          "div",
          { class: "row" },
          btn("Rotate", {
            size: "sm",
            icon: "refresh",
            title: "Replace the client secret",
            onclick: () => void rotateSecret(row),
          }),
          row.has_secret
            ? btn("Disable secret", {
                size: "sm",
                icon: "lock",
                title: "Destroy the secret and require PKCE",
                onclick: () => void disableSecret(row),
              })
            : null,
          btn("Delete", {
            size: "sm",
            variant: "danger",
            icon: "trash",
            title: "Delete this client and every token it issued",
            onclick: () => void deleteClient(row),
          }),
        ),
    },
  ];

  const clientTable = createTable<ClientRow>(ctx, {
    path: "/api/v1/staff/oauth-clients",
    columns: clientColumns,
    empty: clientsEmpty,
    rowKey: (row) => row.id,
    footNote: "A client's secret is shown once, when it is created or rotated.",
  });
  clientsHost.appendChild(clientTable.el);

  function describeClientsEmpty(): void {
    clientsEmpty.title = "No OAuth2 clients";
    clientsEmpty.body =
      "Register an application to let it obtain tokens for a customer's account. The token carries only the scopes the customer holds, and stops doing so the moment a permission is removed.";
    clientsEmpty.action = btn("Register client", { size: "sm", icon: "plus", onclick: () => openCreateClient() });
  }

  function openCreateClient(): void {
    const name = input({ id: "client-name", name: "name", placeholder: "Customer portal", maxlength: 120 });
    const redirects = textarea({
      id: "client-redirects",
      name: "redirect_uris",
      class: "input--mono",
      placeholder: "https://app.example.com/oauth/callback",
    });
    const scopes = input({
      id: "client-scopes",
      name: "allowed_scopes",
      class: "input--mono",
      placeholder: "invoice:read:self, service:read:self",
    });
    const confidential = input({ id: "client-confidential", type: "checkbox", checked: true });

    const held = [...ctx.session.permissions].sort();

    void ctx.formDialog({
      title: "Register an OAuth2 client",
      subtitle: "A confidential client gets a secret once. A public client (a browser, a CLI) cannot keep one and must use PKCE.",
      submitLabel: "Register client",
      body: h(
        "div",
        { class: "stack" },
        field({ label: "Name", control: name, required: true, hint: "What will appear on the consent screen." }),
        field({
          label: "Redirect URIs",
          control: redirects,
          hint: "One per line. Absolute, http(s), no fragment and no wildcard: the server matches the exact string, because a prefix match is how a code leaks.",
        }),
        field({
          label: "Allowed scopes",
          control: scopes,
          hint: "Comma-separated permission names. This is the ceiling: a token can never exceed it, or what the customer holds.",
        }),
        h(
          "details",
          { class: "trust-help" },
          h("summary", null, `Permissions this build declares (${held.length})`),
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
                  onclick: () => {
                    const current = splitList(scopes.value);
                    if (current.includes(permission)) return;
                    current.push(permission);
                    scopes.value = current.join(", ");
                  },
                },
                permission,
              ),
            ),
          ),
        ),
        h("label", { class: "checkbox" }, confidential, h("span", null, "Confidential client (holds a secret)")),
      ),
      onSubmit: async () => {
        const clientName = name.value.trim();
        if (!clientName) return "A client needs a name.";
        const uris = redirects.value
          .split("\n")
          .map((line) => line.trim())
          .filter(Boolean);
        if (uris.length === 0) return "At least one redirect URI is required.";
        const allowed = splitList(scopes.value);
        if (allowed.length === 0) return "At least one allowed scope is required.";

        try {
          const created = await ctx.api.post<ClientWithSecret>("/api/v1/staff/oauth-clients", {
            name: clientName,
            redirect_uris: uris,
            allowed_scopes: allowed,
            confidential: confidential.checked,
          });
          void clientTable.reload();
          if (created.secret) showSecretOnce(created, "Client secret");
          return undefined;
        } catch (err) {
          if (!(err instanceof ApiError)) return "The server could not be reached, so no client was created.";
          return err.code === "invalid_client_request"
            ? err.detail || "The client request was refused."
            : err.detail || `The client could not be registered (${err.code}).`;
        }
      },
    });
  }

  async function rotateSecret(row: ClientRow): Promise<void> {
    const ok = await ctx.confirm({
      title: `Rotate the secret for “${row.name}”?`,
      body: "The new secret is shown once. The old one stops working immediately, so the application has to be updated with the new value. Tokens already issued keep working until they expire.",
      confirmLabel: "Rotate secret",
    });
    if (!ok) return;
    try {
      const rotated = await ctx.api.post<ClientWithSecret>(`/api/v1/staff/oauth-clients/${row.id}/rotate-secret`, {});
      await clientTable.reload();
      showSecretOnce(rotated, "New client secret");
    } catch (err) {
      ctx.toast(err instanceof ApiError ? err.detail || "The secret could not be rotated" : "The secret could not be rotated", {
        tone: "danger",
        detail: err instanceof ApiError ? err.code : undefined,
      });
    }
  }

  async function disableSecret(row: ClientRow): Promise<void> {
    const ok = await ctx.confirm({
      title: `Disable the secret for “${row.name}”?`,
      body: "The secret is destroyed, not hidden: the client becomes a public one and must use PKCE. This cannot be undone — rotate afterwards to give it a secret again.",
      confirmLabel: "Disable secret",
      danger: true,
    });
    if (!ok) return;
    try {
      await ctx.api.del(`/api/v1/staff/oauth-clients/${row.id}/secret`);
      ctx.toast("Secret disabled", { tone: "success", detail: `${row.name} now requires PKCE` });
      await clientTable.reload();
    } catch (err) {
      ctx.toast(err instanceof ApiError ? err.detail || "The secret could not be disabled" : "The secret could not be disabled", {
        tone: "danger",
        detail: err instanceof ApiError ? err.code : undefined,
      });
    }
  }

  async function deleteClient(row: ClientRow): Promise<void> {
    const ok = await ctx.confirm({
      title: `Delete “${row.name}”?`,
      body: "Every authorization code, grant and token it issued is deleted with it, and anything using it stops working immediately. This cannot be undone.",
      confirmLabel: "Delete client",
      danger: true,
      phrase: row.name,
    });
    if (!ok) return;
    try {
      await ctx.api.del(`/api/v1/staff/oauth-clients/${row.id}`);
      ctx.toast("Client deleted", { tone: "success", detail: row.name });
      await clientTable.reload();
    } catch (err) {
      ctx.toast(err instanceof ApiError ? err.detail || "The client could not be deleted" : "The client could not be deleted", {
        tone: "danger",
        detail: err instanceof ApiError ? err.code : undefined,
      });
    }
  }

  /** The one-time display, locked: the value behind it cannot be recovered. */
  function showSecretOnce(created: ClientWithSecret, title: string): void {
    const secret = created.secret ?? "";
    let saved = false;
    let handle: DialogHandle | null = null;

    handle = openDialog({
      title,
      subtitle: "This is the only time it is shown. The server keeps a hash, not the secret.",
      body: h(
        "div",
        { class: "stack" },
        banner(
          "danger",
          "This secret is never shown again",
          "Only a SHA-256 digest is stored, so no screen and no endpoint can reveal it. If you close this dialog without copying it, rotate the secret and use the new one.",
        ),
        h(
          "div",
          { class: "trust-key-box" },
          h("code", { class: "trust-key" }, secret),
          h("div", { class: "trust-key-actions" }, copyButton(ctx, secret, "Copy secret")),
        ),
        metaGrid([
          { label: "Client", value: created.name },
          { label: "Client id", value: mono(created.client_id) },
          { label: "Redirect URIs", value: created.redirect_uris.join(", ") },
          { label: "Allowed scopes", value: created.allowed_scopes.join(", ") },
        ]),
        h(
          "p",
          { class: "faint" },
          created.shown_once
            ? "The response is marked shown_once: this dialog is the only place the secret ever appears."
            : "The response did not confirm shown_once. Treat this secret as unrecoverable once this dialog closes.",
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
    handle.el.querySelector(".dialog-close")?.remove();
    ctx.onDispose(() => {
      saved = true;
      handle?.close();
    });
  }

  // ----------------------------------------------------------- providers --

  const providersHost = h("div");
  const providersEmpty: { title: string; body?: string; action?: Node } = { title: "", body: "" };

  const providerColumns: Column<ProviderRow>[] = [
    {
      key: "name",
      label: "Provider",
      primary: true,
      render: (row) =>
        h(
          "div",
          { class: "cell-main" },
          h("div", { class: "primary" }, row.name),
          h("div", { class: "secondary mono" }, `/oauth/social/${row.slug}/start`),
        ),
    },
    {
      key: "issuer",
      label: "Issuer",
      render: (row) => h("span", { class: "dim mono", title: row.issuer_url }, shorten(row.issuer_url)),
    },
    {
      key: "claims",
      label: "Claims",
      hideSmall: true,
      render: (row) =>
        h(
          "div",
          { class: "trust-scopes" },
          h("span", { class: "chip mono" }, `subject: ${row.subject_claim}`),
          h("span", { class: "chip mono" }, `email: ${row.email_claim}`),
          row.email_verified_claim
            ? h("span", { class: "chip mono" }, `verified: ${row.email_verified_claim}`)
            : null,
        ),
    },
    {
      key: "status",
      label: "Status",
      render: (row) => providerState(row),
    },
    {
      key: "actions",
      label: "",
      align: "right",
      width: "1%",
      render: (row) =>
        h(
          "div",
          { class: "row" },
          btn(row.enabled ? "Disable" : "Enable", {
            size: "sm",
            icon: row.enabled ? "lock" : "check",
            onclick: () => void toggleProvider(row),
          }),
          btn("Delete", {
            size: "sm",
            variant: "danger",
            icon: "trash",
            onclick: () => void deleteProvider(row),
          }),
        ),
    },
  ];

  const providerTable = createTable<ProviderRow>(ctx, {
    path: "/api/v1/staff/oauth-providers",
    columns: providerColumns,
    empty: providersEmpty,
    rowKey: (row) => row.id,
    footNote: "A disabled provider is not offered on the sign-in screen at all.",
  });
  providersHost.appendChild(providerTable.el);

  function describeProvidersEmpty(): void {
    providersEmpty.title = "No social providers";
    providersEmpty.body =
      "With none configured the sign-in screen shows no social buttons. Add a provider to offer one — Google, GitHub and Discord are rows here, not separate integrations.";
    providersEmpty.action = btn("Add provider", { size: "sm", icon: "plus", onclick: () => openCreateProvider() });
  }

  function openCreateProvider(): void {
    const slug = input({ id: "provider-slug", name: "slug", placeholder: "google", class: "input--mono", maxlength: 32 });
    const name = input({ id: "provider-name", name: "name", placeholder: "Google", maxlength: 80 });
    const issuer = input({
      id: "provider-issuer",
      name: "issuer_url",
      placeholder: "https://accounts.google.com",
      class: "input--mono",
    });
    const authorization = input({ id: "provider-authorization", name: "authorization_endpoint", class: "input--mono" });
    const token = input({ id: "provider-token", name: "token_endpoint", class: "input--mono" });
    const userinfo = input({ id: "provider-userinfo", name: "userinfo_endpoint", class: "input--mono" });
    const clientID = input({ id: "provider-client-id", name: "client_id", class: "input--mono" });
    const clientSecret = input({ id: "provider-client-secret", name: "client_secret", type: "password", class: "input--mono" });
    const scopes = input({ id: "provider-scopes", name: "scopes", class: "input--mono", value: "openid, email" });
    const subjectClaim = input({ id: "provider-subject", name: "subject_claim", class: "input--mono", value: "sub" });
    const emailClaim = input({ id: "provider-email", name: "email_claim", class: "input--mono", value: "email" });
    const verifiedClaim = input({ id: "provider-verified", name: "email_verified_claim", class: "input--mono", placeholder: "email_verified" });
    const enabled = input({ id: "provider-enabled", type: "checkbox" });

    void ctx.formDialog({
      title: "Add a social provider",
      subtitle: "The issuer URL is enough for a provider that publishes OIDC discovery. GitHub and Discord do not, so fill in the endpoints.",
      submitLabel: "Add provider",
      body: h(
        "div",
        { class: "stack" },
        h(
          "div",
          { class: "form-grid" },
          field({ label: "Slug", control: slug, required: true, hint: "Lower-case, used in the start URL." }),
          field({ label: "Name", control: name, required: true, hint: "The button label on the sign-in screen." }),
        ),
        field({
          label: "Issuer URL",
          control: issuer,
          required: true,
          hint: "The endpoints below are discovered from <issuer>/.well-known/openid-configuration when they are empty.",
        }),
        h(
          "details",
          { class: "trust-help" },
          h("summary", null, "Advanced: endpoints and claim mapping"),
          h(
            "div",
            { class: "stack" },
            field({ label: "Authorization endpoint", control: authorization }),
            field({ label: "Token endpoint", control: token }),
            field({ label: "Userinfo endpoint", control: userinfo }),
            h(
              "div",
              { class: "form-grid" },
              field({ label: "Subject claim", control: subjectClaim, hint: "A dotted path is allowed, e.g. user.id." }),
              field({ label: "Email claim", control: emailClaim }),
              field({
                label: "Email verified claim",
                control: verifiedClaim,
                hint: "Optional. When set, linking requires it to be true.",
              }),
            ),
          ),
        ),
        field({ label: "Client id", control: clientID, required: true }),
        field({
          label: "Client secret",
          control: clientSecret,
          required: true,
          hint: "Stored server-side and never returned. The read paths report only whether one is configured.",
        }),
        field({ label: "Scopes", control: scopes, hint: "Comma-separated." }),
        h("label", { class: "checkbox" }, enabled, h("span", null, "Enabled (show it on the sign-in screen)")),
      ),
      onSubmit: async () => {
        const body = {
          slug: slug.value.trim(),
          name: name.value.trim(),
          issuer_url: issuer.value.trim(),
          authorization_endpoint: authorization.value.trim(),
          token_endpoint: token.value.trim(),
          userinfo_endpoint: userinfo.value.trim(),
          client_id: clientID.value.trim(),
          client_secret: clientSecret.value,
          scopes: splitList(scopes.value),
          subject_claim: subjectClaim.value.trim(),
          email_claim: emailClaim.value.trim(),
          email_verified_claim: verifiedClaim.value.trim(),
          enabled: enabled.checked,
        };
        if (!body.slug) return "A provider needs a slug.";
        if (!body.issuer_url) return "A provider needs an issuer URL.";
        if (!body.client_id) return "A provider needs a client id.";
        if (!body.client_secret) return "A provider needs a client secret.";
        try {
          await ctx.api.post("/api/v1/staff/oauth-providers", body);
          ctx.toast("Provider added", { tone: "success", detail: body.name || body.slug });
          await providerTable.reload();
          return undefined;
        } catch (err) {
          if (!(err instanceof ApiError)) return "The server could not be reached, so no provider was added.";
          switch (err.code) {
            case "provider_unreachable":
              return err.detail || "The issuer's discovery document could not be read. Fill in the endpoints by hand.";
            case "invalid_provider":
              return err.detail || "The provider configuration was refused.";
            default:
              return err.detail || `The provider could not be added (${err.code}).`;
          }
        }
      },
    });
  }

  async function toggleProvider(row: ProviderRow): Promise<void> {
    try {
      // A full replacement, with an empty secret: the server keeps the stored one, which is
      // what lets the enable toggle avoid wiping a credential it never received.
      await ctx.api.put(`/api/v1/staff/oauth-providers/${row.id}`, {
        slug: row.slug,
        name: row.name,
        issuer_url: row.issuer_url,
        authorization_endpoint: row.authorization_endpoint,
        token_endpoint: row.token_endpoint,
        userinfo_endpoint: row.userinfo_endpoint,
        client_id: row.client_id,
        client_secret: "",
        scopes: row.scopes,
        subject_claim: row.subject_claim,
        email_claim: row.email_claim,
        email_verified_claim: row.email_verified_claim,
        enabled: !row.enabled,
      });
      ctx.toast(row.enabled ? "Provider disabled" : "Provider enabled", {
        tone: "success",
        detail: row.name,
      });
      await providerTable.reload();
    } catch (err) {
      ctx.toast(err instanceof ApiError ? err.detail || "The provider could not be updated" : "The provider could not be updated", {
        tone: "danger",
        detail: err instanceof ApiError ? err.code : undefined,
      });
    }
  }

  async function deleteProvider(row: ProviderRow): Promise<void> {
    const ok = await ctx.confirm({
      title: `Delete “${row.name}”?`,
      body: "Every account linked through it loses that sign-in method, and the people who used it will need another way in. This cannot be undone.",
      confirmLabel: "Delete provider",
      danger: true,
    });
    if (!ok) return;
    try {
      await ctx.api.del(`/api/v1/staff/oauth-providers/${row.id}`);
      ctx.toast("Provider deleted", { tone: "success", detail: row.name });
      await providerTable.reload();
    } catch (err) {
      ctx.toast(err instanceof ApiError ? err.detail || "The provider could not be deleted" : "The provider could not be deleted", {
        tone: "danger",
        detail: err instanceof ApiError ? err.code : undefined,
      });
    }
  }

  describeClientsEmpty();
  describeProvidersEmpty();

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "OAuth clients",
      sub: "Applications that may act for a customer, and the identity providers that may sign one in. A token carries only what its owner holds, and narrows the moment that changes.",
      actions: h(
        "div",
        { class: "row" },
        btn("Add provider", { icon: "link", onclick: () => openCreateProvider() }),
        btn("Register client", { variant: "primary", icon: "plus", onclick: () => openCreateClient() }),
      ),
    }),
    banner(
      "info",
      "A secret is shown once",
      "The create and rotate responses are the only ones that carry a client secret: the server keeps a SHA-256 digest, so there is nothing to reveal again.",
    ),
    card({ title: "Clients", body: clientsHost }),
    card({
      title: "Social providers",
      sub: "Shown as buttons on the sign-in screen. With none enabled, the screen shows none.",
      body: providersHost,
    }),
  );
}

// ------------------------------------------------------------------ helpers --

function splitList(raw: string): string[] {
  return raw
    .split(",")
    .map((part) => part.trim())
    .filter(Boolean);
}

function shorten(value: string): string {
  return value.length > 42 ? `${value.slice(0, 26)}…${value.slice(-14)}` : value;
}

function providerState(row: ProviderRow): HTMLElement {
  const state: { label: string; tone: Tone } = row.enabled
    ? { label: "Enabled", tone: "success" }
    : { label: "Disabled", tone: "neutral" };
  return badge(state.label, state.tone, { dot: true });
}
