// Sign-in providers.
//
// `social_providers` is a registry, not three code paths: Google, GitHub and Discord are rows,
// so a fourth provider is an insert and the endpoints, the scopes and the claim names are
// configuration. This screen is where an operator points the panel at an identity provider and
// finds out whether it will work.
//
// Four rules shape it, and each one comes from the server's design rather than from taste:
//
//  - **A client secret is write-only.** The read reports `has_secret` and never the value
//    (`providerJSON` in internal/httpapi/social.go has no secret field at all), so the field
//    renders empty and labelled "set" or "not set". Leaving it empty means "keep what is
//    stored" — `UpdateProvider` uses `COALESCE(NULLIF($9,''), client_secret)` — which is what
//    makes it safe to save a form that cannot show the credential. No control on this page can
//    reveal a stored secret, because no response contains one.
//
//  - **An existing account is never merged, and that refusal is the point.** A sign-in whose
//    verified email matches an account that already exists is refused with `account_exists`:
//    no session, no link, no merge. The person signs in with their password and links the
//    provider from their own security page. Merging on an email claim would let anyone who can
//    create an identity at a provider take over an account here, so the refusal is not a
//    limitation to work around — it is the control.
//
//  - **Disabled is a state, not a deletion.** A disabled provider is still listed: its links
//    survive, and re-enabling it restores the button on the sign-in screen.
//
//  - **Discord notifications are a different surface.** The webhook URL is a credential and the
//    read never returns it either (`growth.DiscordConfig` has no URL field), so it gets its own
//    card with the same write-only rule rather than being folded into a provider.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateTime, relative } from "../lib/format";
import type { Ctx } from "../lib/types";
import {
  badge,
  banner,
  btn,
  card,
  checkboxField,
  emptyState,
  errorState,
  field,
  input,
  metaGrid,
  mono,
  pageHead,
  skeletonPage,
  switchField,
  withBusy,
} from "../ui/kit";

export const meta = { title: "Sign-in providers", crumb: "Sign-in providers" };

// ---------------------------------------------------------------- payloads --

/** One provider, from `providerJSON` in internal/httpapi/social.go. There is no secret field:
 *  `has_secret` is the only fact about the credential this page ever receives. */
interface Provider {
  id: string;
  slug: string;
  name: string;
  issuer_url: string;
  authorization_endpoint: string;
  token_endpoint: string;
  userinfo_endpoint: string;
  client_id: string;
  has_secret: boolean;
  scopes: string[] | null;
  subject_claim: string;
  email_claim: string;
  email_verified_claim: string;
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

/** One linked identity, from `handleSocialLinks`. The API exposes these for the signed-in
 *  account only; there is deliberately no cross-user link listing. */
interface SocialLink {
  id: string;
  provider_slug: string;
  provider_name: string;
  email: string;
  created_at: string;
  subject: string;
}

/** The Discord configuration, from `growth.DiscordConfig`. `webhook_configured` is a boolean:
 *  the URL is a credential and is never returned. */
interface DiscordConfig {
  enabled: boolean;
  events: string[] | null;
  webhook_configured: boolean;
  available_events: string[] | null;
}

export default async function providersPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("oauth:manage")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Sign-in providers" }),
      banner(
        "danger",
        "You cannot manage sign-in providers",
        "The registry holds client credentials, so it needs the oauth:manage permission.",
        btn("Back to the dashboard", { href: "/", size: "sm", variant: "ghost" }),
      ),
    );
  }

  const host = h("div", { class: "page" }, skeletonPage());

  const load = async (): Promise<void> => {
    try {
      const providers = await ctx.api.get<{ data: Provider[] }>("/api/v1/staff/oauth-providers", {
        cache: false,
      });
      swap(host, render(providers.data ?? []));
    } catch (err) {
      swap(host, errorState(err, () => void load()));
    }
  };

  const render = (providers: Provider[]): Node => {
    const enabled = providers.filter((p) => p.enabled).length;
    return h(
      "div",
      { class: "page" },
      pageHead({
        title: "Sign-in providers",
        sub: "OpenID Connect providers a person can sign in with. Each is a row, not a code path: the endpoints, the scopes and the claim names are configuration.",
        actions: btn("Add provider", { variant: "primary", icon: "plus", onclick: () => providerDialog(null) }),
      }),

      banner(
        "warning",
        "An existing account is never merged",
        h(
          "span",
          null,
          "If a provider hands back a verified email that already belongs to an account here, sign-in is refused with ",
          h("code", { class: "inline-code" }, "account_exists"),
          " — no session, no link, no merge. The person signs in with their password and links the provider from their own security page. ",
          h(
            "strong",
            null,
            "That refusal is what prevents an account takeover:",
          ),
          " anyone who can create an identity at a provider can claim any email, so trusting the claim would hand them the account.",
        ),
      ),

      h(
        "div",
        { class: "grid-4" },
        stat("Providers", providers.length, "configured"),
        stat("Enabled", enabled, "shown on the sign-in screen"),
        stat("Disabled", providers.length - enabled, "rows kept, button hidden"),
        stat("With a secret", providers.filter((p) => p.has_secret).length, "write-only, never returned"),
      ),

      providers.length === 0
        ? card({
            flush: true,
            body: emptyState({
              title: "No sign-in providers",
              body: "With none configured the sign-in screen shows no third-party buttons and everyone signs in with a password. Add one to offer a provider.",
              icon: "globe",
              action: btn("Add provider", { variant: "primary", icon: "plus", onclick: () => providerDialog(null) }),
            }),
          })
        : h("div", { class: "stack" }, ...providers.map(providerCard)),

      ownLinksCard(),
      discordCard(),
    );
  };

  // ------------------------------------------------------------- one provider --

  const providerCard = (provider: Provider): HTMLElement =>
    card({
      title: h(
        "span",
        { class: "row" },
        provider.name,
        provider.enabled ? badge("Enabled", "success", { dot: true }) : badge("Disabled", "neutral", { dot: true }),
        provider.has_secret ? null : badge("No secret", "warning", { dot: true }),
      ),
      sub: h("span", { class: "mono" }, provider.slug),
      actions: h(
        "div",
        { class: "row" },
        btn("Edit", { size: "sm", icon: "edit", onclick: () => providerDialog(provider) }),
        btn("Delete", {
          size: "sm",
          variant: "danger",
          icon: "trash",
          onclick: () => void removeProvider(provider),
        }),
      ),
      body: h(
        "div",
        { class: "stack" },
        metaGrid([
          { label: "Issuer", value: h("span", { class: "mono" }, provider.issuer_url || "—") },
          { label: "Client id", value: mono(provider.client_id) },
          {
            label: "Client secret",
            value: provider.has_secret
              ? h("span", { class: "row" }, badge("Stored", "success", { dot: true }), h("span", { class: "faint" }, "write-only"))
              : h("span", { class: "faint" }, "Not set — the provider cannot complete a sign-in"),
          },
          {
            label: "Scopes",
            value:
              (provider.scopes ?? []).length > 0
                ? h(
                    "div",
                    // A scope is one unbroken word; `code-chips` lets the chip wrap inside its
                    // row rather than run past the edge of the card at 390px.
                    { class: "trust-scopes code-chips" },
                    ...(provider.scopes ?? []).map((scope) => h("span", { class: "chip mono" }, scope)),
                  )
                : h("span", { class: "faint" }, "openid, email"),
          },
          {
            label: "Claim mapping",
            value: h(
              "div",
              { class: "stack-sm" },
              h("span", { class: "mono" }, `subject ← ${provider.subject_claim || "sub"}`),
              h("span", { class: "mono" }, `email ← ${provider.email_claim || "email"}`),
              provider.email_verified_claim
                ? h("span", { class: "mono" }, `verified ← ${provider.email_verified_claim}`)
                : h(
                    "span",
                    { class: "faint" },
                    "no verified claim: the provider cannot prove the address, so linking stays password-first",
                  ),
            ),
          },
          { label: "Updated", value: h("span", { class: "dim", title: dateTime(provider.updated_at) }, relative(provider.updated_at)) },
        ]),
        h(
          "p",
          { class: "field-hint" },
          "Callback URL to register with the provider: ",
          h("code", { class: "inline-code" }, `/oauth/social/${provider.slug}/callback`),
          " on this panel's public origin. The server builds it from configuration, never from a request's Host header.",
        ),
      ),
    });

  const removeProvider = async (provider: Provider): Promise<void> => {
    const ok = await ctx.confirm({
      title: `Delete ${provider.name}?`,
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "p",
          null,
          "The row and every identity linked through it are removed. Anyone who signed in with this provider loses that route in — they still have their password.",
        ),
        h(
          "p",
          { class: "field-hint" },
          "Disabling instead keeps the links and only hides the button. That is the reversible choice.",
        ),
      ),
      confirmLabel: "Delete the provider",
      danger: true,
    });
    if (!ok) return;
    await withBusy(ctx, null, async () => {
      try {
        await ctx.api.del(`/api/v1/staff/oauth-providers/${encodeURIComponent(provider.id)}`);
        ctx.toast("Provider deleted", { tone: "success", detail: provider.name });
        await load();
      } catch (err) {
        report(err, "The provider could not be deleted");
      }
    });
  };

  // ------------------------------------------------------------ create / edit --

  const providerDialog = (existing: Provider | null): void => {
    const slug = input({
      id: "provider-slug",
      value: existing?.slug ?? "",
      placeholder: "discord",
      autocomplete: "off",
      maxlength: 32,
    });
    const name = input({
      id: "provider-name",
      value: existing?.name ?? "",
      placeholder: "Discord",
      autocomplete: "off",
    });
    const issuer = input({
      id: "provider-issuer",
      value: existing?.issuer_url ?? "",
      placeholder: "https://discord.com",
      autocomplete: "off",
      class: "input--mono",
    });
    const authorize = input({
      id: "provider-authorize",
      value: existing?.authorization_endpoint ?? "",
      placeholder: "https://discord.com/oauth2/authorize",
      autocomplete: "off",
      class: "input--mono",
    });
    const token = input({
      id: "provider-token",
      value: existing?.token_endpoint ?? "",
      placeholder: "https://discord.com/api/oauth2/token",
      autocomplete: "off",
      class: "input--mono",
    });
    const userinfo = input({
      id: "provider-userinfo",
      value: existing?.userinfo_endpoint ?? "",
      placeholder: "https://discord.com/api/users/@me",
      autocomplete: "off",
      class: "input--mono",
    });
    const clientID = input({
      id: "provider-client-id",
      value: existing?.client_id ?? "",
      autocomplete: "off",
      class: "input--mono",
    });
    const clientSecret = input({
      id: "provider-client-secret",
      type: "password",
      autocomplete: "new-password",
      placeholder: existing?.has_secret ? "Stored — leave empty to keep it" : "The client secret",
    });
    const scopes = input({
      id: "provider-scopes",
      value: (existing?.scopes ?? []).join(" "),
      placeholder: "openid email profile",
      autocomplete: "off",
      class: "input--mono",
    });
    const subjectClaim = input({
      id: "provider-subject-claim",
      value: existing?.subject_claim ?? "sub",
      placeholder: "sub",
      autocomplete: "off",
      class: "input--mono",
    });
    const emailClaim = input({
      id: "provider-email-claim",
      value: existing?.email_claim ?? "email",
      placeholder: "email",
      autocomplete: "off",
      class: "input--mono",
    });
    const verifiedClaim = input({
      id: "provider-verified-claim",
      value: existing?.email_verified_claim ?? "",
      placeholder: "email_verified",
      autocomplete: "off",
      class: "input--mono",
    });
    let enabled = existing?.enabled ?? true;

    void ctx.formDialog({
      title: existing ? `Edit ${existing.name}` : "Add a sign-in provider",
      subtitle: existing
        ? "An empty client secret keeps the stored one: the field cannot show it, so saving must not wipe it."
        : "Paste the issuer URL and the client credentials. The three endpoints are filled in from OIDC discovery when they are left empty.",
      submitLabel: existing ? "Save" : "Add provider",
      body: h(
        "div",
        { class: "stack" },
        h(
          "div",
          { class: "row" },
          field({ label: "Slug", control: slug, required: true, hint: "a-z, 0-9, - and _. Appears in the callback URL." }),
          field({ label: "Name", control: name, required: true, hint: "What the sign-in button says." }),
        ),
        field({ label: "Issuer URL", control: issuer, required: true, hint: "The provider's base URL; discovery is read from <issuer>/.well-known/openid-configuration." }),
        h(
          "div",
          { class: "row" },
          field({ label: "Authorization endpoint", control: authorize, hint: "Optional with discovery." }),
          field({ label: "Token endpoint", control: token, hint: "Optional with discovery." }),
        ),
        field({ label: "Userinfo endpoint", control: userinfo, hint: "Optional with discovery." }),
        h(
          "div",
          { class: "row" },
          field({ label: "Client id", control: clientID, required: true }),
          field({
            label: "Client secret",
            control: clientSecret,
            hint: existing?.has_secret
              ? "Stored and write-only. Leave this empty to keep the current secret."
              : "Write-only: it is never returned by any read.",
          }),
        ),
        field({
          label: "Scopes",
          control: scopes,
          hint: "Space separated. Defaults to openid and email.",
        }),
        h(
          "div",
          { class: "row" },
          field({
            label: "Subject claim",
            control: subjectClaim,
            hint: "The claim that identifies the person at the provider.",
          }),
          field({ label: "Email claim", control: emailClaim }),
        ),
        field({
          label: "Email verified claim",
          control: verifiedClaim,
          hint: "The claim that proves the address. Without it the provider cannot be used to link an account.",
        }),
        switchField("Enabled — show this provider on the sign-in screen", {
          checked: enabled,
          onchange: () => {
            enabled = !enabled;
          },
        }),
      ),
      onSubmit: async () => {
        const payload = {
          slug: slug.value.trim(),
          name: name.value.trim(),
          issuer_url: issuer.value.trim(),
          authorization_endpoint: authorize.value.trim(),
          token_endpoint: token.value.trim(),
          userinfo_endpoint: userinfo.value.trim(),
          client_id: clientID.value.trim(),
          client_secret: clientSecret.value,
          scopes: scopes.value
            .split(/[\s,]+/)
            .map((s) => s.trim())
            .filter(Boolean),
          subject_claim: subjectClaim.value.trim() || "sub",
          email_claim: emailClaim.value.trim() || "email",
          email_verified_claim: verifiedClaim.value.trim(),
          enabled,
        };
        if (!payload.slug || !payload.name || !payload.client_id || !payload.issuer_url) {
          return "A slug, a name, an issuer URL and a client id are required.";
        }
        try {
          if (existing) {
            await ctx.api.put(`/api/v1/staff/oauth-providers/${encodeURIComponent(existing.id)}`, payload);
          } else {
            await ctx.api.post("/api/v1/staff/oauth-providers", payload);
          }
          ctx.toast(existing ? "Provider saved" : "Provider added", {
            tone: "success",
            detail: payload.enabled
              ? "It now appears on the sign-in screen."
              : "It is saved but disabled, so no button is shown.",
          });
          await load();
          return undefined;
        } catch (err) {
          if (!(err instanceof ApiError)) return "The server could not be reached, so nothing changed.";
          switch (err.code) {
            case "invalid_provider":
              return err.detail || "The provider configuration was refused.";
            case "provider_unreachable":
              return `The issuer could not be read: ${err.detail || "discovery failed"}. Check the issuer URL, or fill in the three endpoints by hand.`;
            case "not_found":
              return "That provider no longer exists. Reload the page.";
            default:
              return err.detail || `The provider could not be saved (${err.code}).`;
          }
        }
      },
    });
  };

  // ------------------------------------------------------------ own links --

  const ownLinksCard = (): HTMLElement => {
    const body = h("div", { class: "stack-sm" }, h("span", { class: "faint" }, "Loading…"));
    void (async () => {
      try {
        const page = await ctx.api.get<{ data: SocialLink[] }>("/api/v1/portal/social-links", {
          cache: false,
        });
        const links = page.data ?? [];
        if (links.length === 0) {
          swap(
            body,
            h(
              "span",
              { class: "field-hint" },
              "No provider is linked to your own account. Linking is done from your security page, not from here.",
            ),
          );
          return;
        }
        swap(
          body,
          h(
            "div",
            { class: "stack-sm" },
            ...links.map((link) =>
              h(
                "div",
                { class: "row" },
                badge(link.provider_name || link.provider_slug, "info", { dot: true }),
                h("span", { class: "dim" }, link.email || "no email on the identity"),
                h(
                  "span",
                  { class: "faint nowrap", title: dateTime(link.created_at) },
                  `linked ${relative(link.created_at)}`,
                ),
                h(
                  "span",
                  { class: "spacer faint mono", title: "The provider's opaque subject id" },
                  link.subject,
                ),
              ),
            ),
          ),
        );
      } catch (err) {
        swap(
          body,
          h(
            "span",
            { class: "field-hint" },
            err instanceof ApiError && err.status === 403
              ? "Linked accounts are only readable for the signed-in account, and your role cannot read them."
              : "Your linked accounts could not be read.",
          ),
        );
      }
    })();

    return card({
      title: "Your own linked accounts",
      sub: "the API exposes links per account, not across users",
      body: h(
        "div",
        { class: "stack-sm" },
        body,
        h(
          "p",
          { class: "field-hint" },
          "Links belong to an account: there is no endpoint that lists another person's, by design — a list of who signed in with what is a list of targets. This card shows yours so you can confirm a provider works end to end.",
        ),
      ),
    });
  };

  // --------------------------------------------------------------- discord --

  const discordCard = (): HTMLElement => {
    const body = h("div", { class: "stack-sm" }, h("span", { class: "faint" }, "Loading…"));
    const canWrite = ctx.can("settings:write");

    void (async () => {
      let config: DiscordConfig;
      try {
        config = await ctx.api.get<DiscordConfig>("/api/v1/staff/discord", { cache: false });
      } catch (err) {
        swap(
          body,
          h(
            "span",
            { class: "field-hint" },
            err instanceof ApiError && err.status === 403
              ? "Reading the Discord configuration needs report:read."
              : "The Discord configuration could not be read.",
          ),
        );
        return;
      }

      let enabled = config.enabled;
      const webhook = input({
        id: "discord-webhook",
        type: "password",
        autocomplete: "new-password",
        placeholder: config.webhook_configured ? "Stored — leave empty to keep it" : "https://discord.com/api/webhooks/…",
      });
      const clearBox = h("input", { type: "checkbox" });
      const selected = new Set(config.events ?? []);
      const eventBoxes = new Map<string, HTMLInputElement>();
      for (const key of config.available_events ?? []) {
        eventBoxes.set(key, h("input", { type: "checkbox", checked: selected.has(key) }));
      }

      const save = btn("Save", { variant: "primary", icon: "check" }) as HTMLButtonElement;
      save.disabled = !canWrite;
      save.addEventListener("click", () => {
        void withBusy(ctx, save, async () => {
          try {
            await ctx.api.put("/api/v1/staff/discord", {
              enabled,
              webhook_url: webhook.value,
              events: [...eventBoxes.entries()].filter(([, box]) => box.checked).map(([key]) => key),
              clear_webhook: clearBox.checked,
            });
            ctx.toast("Discord configuration saved", { tone: "success" });
            await load();
          } catch (err) {
            report(err, "The Discord configuration could not be saved");
          }
        });
      });

      swap(
        body,
        h(
          "div",
          { class: "stack" },
          h(
            "div",
            { class: "row" },
            switchField("Notify Discord", {
              checked: enabled,
              onchange: () => {
                enabled = !enabled;
              },
            }),
            config.webhook_configured
              ? badge("Webhook stored", "success", { dot: true })
              : badge("No webhook", "warning", { dot: true }),
          ),
          field({
            label: "Webhook URL",
            control: webhook,
            hint: config.webhook_configured
              ? "Stored and write-only: the read reports only that a webhook exists. Leave empty to keep it."
              : "Write-only. A URL that is echoed back is a credential in every proxy log between here and the operator.",
          }),
          checkboxField("Remove the stored webhook", {
            checked: false,
            onchange: () => {
              clearBox.checked = !clearBox.checked;
            },
          }),
          h(
            "div",
            { class: "stack-sm" },
            h("span", { class: "field-label" }, "Events"),
            ...(config.available_events ?? []).map((key) =>
              h("label", { class: "checkbox" }, eventBoxes.get(key) as HTMLInputElement, h("span", null, key)),
            ),
          ),
          h(
            "div",
            { class: "row" },
            save,
            canWrite ? null : h("span", { class: "faint" }, "Saving needs the settings:write permission."),
          ),
        ),
      );
    })();

    return card({
      title: "Discord notifications",
      sub: "a separate surface: the webhook is a credential too",
      body,
    });
  };

  const report = (err: unknown, fallback: string): void => {
    if (err instanceof ApiError) {
      ctx.toast(err.detail || fallback, { tone: "danger", detail: err.code });
      return;
    }
    ctx.toast(fallback, { tone: "danger", detail: "The server could not be reached." });
  };

  void load();
  return host;
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
