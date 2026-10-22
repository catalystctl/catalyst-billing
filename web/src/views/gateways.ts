// Payment gateway configuration.
//
// This screen is where an operator points the panel at a provider and finds out whether it
// worked. Four rules shape it, and each one comes from the server's design rather than from
// taste:
//
//  - **A secret is write-only.** `GET /api/v1/staff/gateways` reports `is_set` and `source`,
//    never a value. So a secret renders as an empty password box labelled "set" or "not set",
//    and leaving it empty means "keep what is stored" — the server skips the marker, and this
//    page never sends a box the operator did not type into. There is no control anywhere that
//    can reveal a stored credential, because there is no response that contains one.
//
//  - **The environment can win, and the screen says so.** When a value comes from an
//    environment variable, `source` is `env` and `editable` is false: the field is disabled and
//    labelled, and the server would refuse the write anyway. Silently storing a value the
//    runtime ignores would be a screen that lies.
//
//  - **The test is a read, and it tests what is saved.** `GET /api/v1/staff/gateways/test`
//    launches the configured extension, performs the protocol handshake and calls
//    `gateway.describe`. It changes nothing, which is why the button is safe to press. It tests
//    the *stored* configuration, not the form: accepting credentials in a GET would put a
//    secret in a URL, an access log and a browser history. So the button is labelled "save and
//    test" when there are unsaved changes, and it says what it will do.
//
//  - **A failed test is an answer, not an error.** The response is 200 with `ok: false` and a
//    stable code, so the reason renders next to the button that caused it instead of as a
//    request failure the operator has to interpret.
//
// The webhook URL is shown for pasting into the provider. It is derived server-side from
// `BILLING_PUBLIC_URL`, not from this browser's address, because the address a provider must
// deliver to is the deployment's public origin.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateTime, titleCase } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
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
  pageHead,
  skeletonPage,
  switchField,
  withBusy,
} from "../ui/kit";

export const meta = { title: "Payment gateways", crumb: "Gateways" };

// ---------------------------------------------------------------- payloads --

/** One credential field, from `Value` in internal/gatewayconfig/config.go.
 *
 *  `value` is absent for a secret — the struct does not carry one — which is the reason this
 *  page cannot render one. `is_set` is the only fact about a secret's contents it receives. */
interface ConfigValue {
  key: string;
  label: string;
  env: string;
  hint?: string;
  is_secret: boolean;
  is_set: boolean;
  /** `store`, `env` or `default`. */
  source: string;
  editable: boolean;
}

/** A provider this build can launch, from `Provider`. */
interface ProviderChoice {
  id: string;
  name: string;
  extension: string;
  fields: Array<{ key: string; label: string; env: string; is_secret: boolean; hint?: string }>;
  env_only?: Array<{ key: string; env: string; hint?: string }>;
  note?: string;
}

/** The configuration as it reads, from `Config`. */
interface IntegrationConfig {
  kind: string;
  provider: string;
  provider_name: string;
  provider_source: string;
  provider_editable: boolean;
  enabled: boolean;
  enabled_source: string;
  enabled_editable: boolean;
  off_session_enabled: boolean;
  extension_path: string;
  extension_source: string;
  values: ConfigValue[] | null;
  webhook_url?: string;
  providers: ProviderChoice[] | null;
  notes?: string[];
}

/** A connection test's answer, from `integrationTestResult`. */
interface TestResult {
  ok: boolean;
  code?: string;
  detail?: string;
  gateway?: {
    provider: string;
    provider_name: string;
    extension: string;
    publisher: string;
    protocol_version: number;
    capabilities: string[] | null;
    currencies: string[] | null;
    methods: Array<{ id: string; name: string }> | null;
    supports_refunds: boolean;
    supports_webhooks: boolean;
    webhook_url?: string;
    duration_ms: number;
  };
  notes?: string[];
  tested_at: string;
}

const ENDPOINT = "/api/v1/staff/gateways";
const TEST_ENDPOINT = "/api/v1/staff/gateways/test";

export default async function gatewaysPage(ctx: Ctx): Promise<Node> {
  const host = h("div", { class: "resource" }, skeletonPage());
  const controller = new AbortController();
  ctx.onDispose(() => controller.abort());

  const load = async (): Promise<void> => {
    try {
      const cfg = await ctx.api.get<IntegrationConfig>(ENDPOINT, { cache: false, signal: controller.signal });
      if (controller.signal.aborted) return;
      swap(host, render(ctx, cfg, load));
    } catch (err) {
      if (controller.signal.aborted) return;
      swap(host, errorState(err, () => {
        swap(host, skeletonPage());
        void load();
      }));
    }
  };
  void load();
  return host;
}

function render(ctx: Ctx, cfg: IntegrationConfig, reload: () => Promise<void>): Node {
  const providers = cfg.providers ?? [];
  const chosen = providers.find((p) => p.id === cfg.provider) ?? null;

  // The form is uncontrolled until Save: the server's committed state is what renders, and
  // what is on screen after a save is what the server stored rather than what was typed.
  const providerSelect = h("select", { class: "input", id: "gw-provider" }) as HTMLSelectElement;
  providerSelect.append(new Option("Choose a provider…", ""));
  for (const p of providers) {
    providerSelect.append(new Option(p.name, p.id));
  }
  providerSelect.value = cfg.provider;
  providerSelect.disabled = !cfg.provider_editable;

  const fields = new Map<string, HTMLInputElement>();
  const fieldHost = h("div", { class: "stack-sm" });

  const paintFields = (): void => {
    fields.clear();
    const id = providerSelect.value;
    const provider = providers.find((p) => p.id === id) ?? null;
    if (!provider) {
      swap(
        fieldHost,
        h(
          "p",
          { class: "field-hint" },
          providers.length === 0
            ? "This build ships no launchable gateway extensions, so there is nothing to configure."
            : "Choose a provider to see the credentials it needs.",
        ),
      );
      return;
    }

    // A stored value for the newly-chosen provider, if the operator is switching back to one
    // that was configured before.
    const stored = new Map((cfg.values ?? []).map((v) => [v.key, v]));

    const rows: Node[] = provider.fields.map((f) => {
      const current = stored.get(f.key);
      const fromEnv = current?.source === "env";
      const editable = current ? current.editable : true;

      const el = input({
        type: f.is_secret ? "password" : "text",
        id: `gw-${f.key}`,
        name: f.key,
        autocomplete: "off",
        spellcheck: "false",
        placeholder: f.is_secret
          ? current?.is_set
            ? "Stored — leave blank to keep it"
            : "Not set"
          : fromEnv
            ? "Supplied by the environment"
            : "",
        disabled: !editable || fromEnv,
      });
      // A non-secret value is echoed back so the operator can see what is stored; a secret's
      // value is not in the response at all.
      if (!f.is_secret && current && !fromEnv) {
        const raw = (current as unknown as { value?: string }).value;
        if (typeof raw === "string") el.value = raw;
      }
      fields.set(f.key, el);

      // field() takes a string label, so the state badges sit beside the control rather than
      // inside the label element.
      const marks: Node[] = [];
      if (f.is_secret && current?.is_set) marks.push(badge("set", "success"));
      if (fromEnv) marks.push(badge("from environment", "info"));
      return field({
        label: f.label,
        control: marks.length > 0 ? h("div", { class: "inline-row" }, el, ...marks) : el,
        id: `gw-${f.key}`,
        hint: f.hint ?? (fromEnv ? `Supplied by ${f.env}; change it in the deployment, not here.` : undefined),
      });
    });

    const envOnly = provider.env_only ?? [];
    if (envOnly.length > 0) {
      rows.push(
        h(
          "div",
          { class: "stack-sm" },
          h("p", { class: "field-hint" }, "These can only be set in the deployment's environment:"),
          h(
            "ul",
            { class: "plain-list" },
            ...envOnly.map((e) =>
              h(
                "li",
                null,
                h("code", null, e.env),
                e.hint ? h("span", { class: "field-hint" }, ` — ${e.hint}`) : null,
              ),
            ),
          ),
        ),
      );
    }
    if (provider.note) {
      rows.push(h("p", { class: "field-hint" }, provider.note));
    }
    swap(fieldHost, h("div", { class: "stack-sm" }, ...rows));
  };

  providerSelect.addEventListener("change", paintFields);

  // ---- the switch, the off-session switch, and the webhook URL --------------------------

  const enabledSwitch = switchField("Gateway is enabled", {
    id: "gw-enabled",
    name: "enabled",
    checked: cfg.enabled,
    disabled: !cfg.enabled_editable,
  }) as HTMLElement;

  const offSessionSwitch = switchField("Charge renewals off-session", {
    id: "gw-offsession",
    name: "off_session_enabled",
    checked: cfg.off_session_enabled,
  }) as HTMLElement;

  // ---- save, and save-and-test --------------------------------------------------------

  const save = async (thenTest: boolean): Promise<void> => {
    const values: Record<string, string> = {};
    for (const [key, el] of fields) {
      // An empty secret box means "leave it alone": sending "" would overwrite a working
      // credential with nothing, which is the single most likely way to break a live gateway
      // from this screen.
      if (el.type === "password" && el.value === "") continue;
      values[key] = el.value;
    }
    const enabledInput = enabledSwitch.querySelector("input") as HTMLInputElement | null;
    const offSessionInput = offSessionSwitch.querySelector("input") as HTMLInputElement | null;

    try {
      await ctx.api.put<IntegrationConfig>(ENDPOINT, {
        provider: providerSelect.value,
        enabled: enabledInput?.checked ?? cfg.enabled,
        off_session_enabled: offSessionInput?.checked ?? cfg.off_session_enabled,
        values,
      });
      ctx.toast("Gateway configuration saved", { tone: "success" });
    } catch (err) {
      ctx.toast("The configuration was not saved", { tone: "danger", detail: describeWriteError(err) });
      return;
    }

    await reload();
    if (thenTest) {
      // The reload replaced this node, so the test is started from the fresh page.
      ctx.toast("Saved — press Test connection to check it", { tone: "info" });
    }
  };

  const testButton = btn("Test connection", {
    icon: "zap",
    variant: "default",
    onclick: (e) => {
      const button = e.currentTarget as HTMLButtonElement;
      void withBusy(ctx, button, async () => {
        const result = await runTest(ctx);
        swap(resultHost, renderTestResult(ctx, result));
        resultHost.scrollIntoView({ block: "nearest", behavior: "smooth" });
      });
    },
  });

  const saveButton = btn("Save", {
    icon: "check",
    variant: "primary",
    onclick: (e) => {
      const button = e.currentTarget as HTMLButtonElement;
      void withBusy(ctx, button, () => save(false));
    },
  });

  const resultHost = h("div");

  // ---- the page ----------------------------------------------------------------------

  return h(
    "div",
    { class: "page stack" },
    pageHead({
      title: "Payment gateways",
      sub: "Point the panel at a provider, then test the connection before you switch payments on.",
    }),

    // Notes are the server's own remarks about the current configuration — an extension that
    // is not where it was expected, a value the environment overrode. They are the cheapest
    // place to put a fact that would otherwise be a mystery.
    (cfg.notes ?? []).length > 0
      ? banner("info", "About this configuration", h("ul", { class: "plain-list" }, ...(cfg.notes ?? []).map((n) => h("li", null, n))))
      : null,

    providers.length === 0
      ? card({
          body: emptyState({
            title: "No gateway extensions in this build",
            body: "A gateway is an extension binary. Build one from extensions/ and point the deployment at it, then come back.",
            icon: "card",
          }),
        })
      : null,

    card({
      title: "Provider",
      sub: "The provider decides which credentials are needed and which currencies can be settled.",
      body: h(
        "div",
        { class: "stack" },
        field({
          label: "Provider",
          control: providerSelect,
          id: "gw-provider",
          hint: cfg.provider_source === "env" ? "Supplied by the environment." : undefined,
        }),
        fieldHost,
      ),
    }),

    card({
      title: "Switches",
      body: h(
        "div",
        { class: "stack-sm" },
        enabledSwitch,
        h(
          "p",
          { class: "field-hint" },
          "With the gateway off, the storefront still shows prices but cannot take a payment. The connection test works either way — testing before enabling is the point.",
        ),
        offSessionSwitch,
        h(
          "p",
          { class: "field-hint" },
          "Off-session charging is the one setting that moves money with no human in the loop: a renewal charges the saved card without the customer present. Leave it off until you have watched a manual renewal settle.",
        ),
      ),
    }),

    cfg.webhook_url
      ? card({
          title: "Webhook",
          sub: "Paste this into the provider's dashboard so it can tell the panel what happened.",
          body: h(
            "div",
            { class: "stack-sm" },
            h(
              "div",
              { class: "inline-row" },
              h("code", { class: "code-block" }, cfg.webhook_url),
              copyButton(ctx, cfg.webhook_url, "Copy URL"),
            ),
            h(
              "p",
              { class: "field-hint" },
              "The signing secret above is what makes a delivery trustworthy: without it, anyone who learns this URL could settle an invoice.",
            ),
          ),
        })
      : null,

    card({
      title: "Connection",
      sub: "Launches the configured extension, performs the protocol handshake and asks it what it supports.",
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "div",
          { class: "inline-row" },
          saveButton,
          testButton,
        ),
        h(
          "p",
          { class: "field-hint" },
          "The test runs against the saved configuration, not what is typed above — a credential in a GET would end up in a URL, an access log and the browser's history. Press Save first if you have changed something.",
        ),
        resultHost,
        cfg.extension_path
          ? h("p", { class: "field-hint" }, "Extension: ", h("code", null, cfg.extension_path))
          : null,
        chosen?.note ? h("p", { class: "field-hint" }, chosen.note) : null,
      ),
    }),
  );
}

// ------------------------------------------------------------------- test --

async function runTest(ctx: Ctx): Promise<TestResult> {
  try {
    return await ctx.api.get<TestResult>(TEST_ENDPOINT, { cache: false });
  } catch (err) {
    // A problem document here is a request-level failure (no permission, no database), not a
    // failed connection — those arrive as a 200 with ok:false.
    return {
      ok: false,
      code: err instanceof ApiError ? err.code : "request_failed",
      detail: err instanceof Error ? err.message : "The test could not be run.",
      tested_at: new Date().toISOString(),
    };
  }
}

function renderTestResult(ctx: Ctx, result: TestResult): Node {
  if (!result.ok) {
    return banner(
      "warning",
      "The connection test did not succeed",
      h(
        "div",
        { class: "stack-sm" },
        h("p", null, result.detail ?? "The extension did not answer as expected."),
        result.code ? h("p", { class: "field-hint" }, "Reason code: ", h("code", null, result.code)) : null,
        (result.notes ?? []).length > 0
          ? h("ul", { class: "plain-list" }, ...(result.notes ?? []).map((n) => h("li", null, n)))
          : null,
      ),
    );
  }

  const gw = result.gateway;
  if (!gw) {
    return banner("info", "The test passed", h("p", null, "The extension answered but described nothing."));
  }

  return h(
    "div",
    { class: "stack-sm" },
    banner(
      "success",
      `Connected to ${gw.provider_name || gw.provider}`,
      h(
        "p",
        null,
        "The extension handshook at protocol v",
        String(gw.protocol_version),
        " and answered in ",
        String(gw.duration_ms),
        " ms. Tested ",
        dateTime(result.tested_at),
        ".",
      ),
    ),
    h(
      "div",
      { class: "kv-grid" },
      kvRow("Extension", h("span", null, gw.extension, gw.publisher ? h("span", { class: "field-hint" }, ` · ${gw.publisher}`) : null)),
      kvRow("Currencies", (gw.currencies ?? []).length > 0 ? h("span", { class: "chip-row" }, ...(gw.currencies ?? []).map((c) => badge(c, "neutral"))) : h("span", { class: "muted" }, "None advertised")),
      kvRow("Methods", (gw.methods ?? []).length > 0 ? h("span", { class: "chip-row" }, ...(gw.methods ?? []).map((m) => badge(m.name || m.id, "neutral"))) : h("span", { class: "muted" }, "None advertised")),
      kvRow("Refunds", gw.supports_refunds ? badge("supported", "success") : badge("not supported", "warning")),
      kvRow("Webhooks", gw.supports_webhooks ? badge("supported", "success") : badge("not supported", "warning")),
      kvRow("Capabilities", h("span", { class: "chip-row" }, ...(gw.capabilities ?? []).map((c) => h("code", null, c)))),
    ),
    // A gateway that cannot refund or cannot receive webhooks is usable but incomplete, and the
    // operator should hear that from the test rather than from a customer.
    !gw.supports_webhooks
      ? banner(
          "warning",
          "This gateway cannot report payments back",
          h(
            "p",
            null,
            "Without webhooks a payment is only discovered when someone asks — a customer who pays and closes the tab keeps an open invoice until a reconciliation run finds it.",
          ),
        )
      : null,
    gw.webhook_url
      ? h(
          "div",
          { class: "inline-row" },
          h("code", { class: "code-block" }, gw.webhook_url),
          copyButton(ctx, gw.webhook_url, "Copy URL"),
        )
      : null,
  );
}

function kvRow(label: string, value: Node): Node {
  return h("div", { class: "kv-row" }, h("span", { class: "kv-key" }, titleCase(label)), h("span", { class: "kv-value" }, value));
}

/** Turns a write failure into a sentence.
 *
 *  `env_override` is the one that matters: the server refuses a write when the environment
 *  supplies the value, and the message names the variable. Relaying it verbatim is right,
 *  because the operator's next action is to edit a deployment, not this form. */
function describeWriteError(err: unknown): string {
  if (err instanceof ApiError) {
    switch (err.code) {
      case "env_override":
        return err.message;
      case "unknown_provider":
        return "That provider is not one this build can launch.";
      case "provider_required":
        return "Choose a provider before saving.";
      case "nothing_to_update":
        return "Nothing changed, so nothing was saved.";
      case "unknown_setting":
      case "invalid_value":
        return err.message;
      default:
        return err.message || "The configuration could not be saved.";
    }
  }
  return err instanceof Error ? err.message : "The configuration could not be saved.";
}
