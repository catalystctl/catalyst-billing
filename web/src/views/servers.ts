// Provisioning server configuration.
//
// The same shape as the gateway screen, and for the same reasons — a secret is write-only, the
// environment can win and the screen says so, and the connection test is a read that tests what
// is saved rather than what is typed. What differs is what the test reports: a gateway
// advertises currencies and methods, a provisioner advertises the plans it can build and the
// regions it can build them in.
//
// That difference is the reason this is a separate screen rather than a parameter of the
// gateway one. A provisioner is what turns a paid invoice into a running server, and the two
// questions an operator asks of it are "can it reach the panel" and "does the plan a product
// sells exist as a size this provisioner will accept". The second is the one that fails
// silently: a product can be sold against a plan id nothing can provision, and no invoice will
// ever look wrong.
//
// The screen therefore renders the plans as a table with their limits, so a plan id in the
// catalogue can be compared against the sizes the adapter actually offers. A plan the catalogue
// sells and the provisioner does not advertise is the defect this screen exists to make
// visible — and it is stated in words on the page, because a table the operator has to diff by
// eye is a table they will not diff.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateTime } from "../lib/format";
import type { Amount } from "../lib/format";
import type { Ctx } from "../lib/types";
import {
  badge,
  banner,
  btn,
  card,
  emptyState,
  errorState,
  field,
  input,
  metaGrid,
  pageHead,
  skeletonPage,
  withBusy,
} from "../ui/kit";

export const meta = { title: "Provisioning servers", crumb: "Servers" };

// ---------------------------------------------------------------- payloads --

interface ConfigValue {
  key: string;
  label: string;
  env: string;
  hint?: string;
  is_secret: boolean;
  is_set: boolean;
  /** `environment`, `panel`, `default` or `unset`, from `gatewayconfig.Source*`. */
  source: string;
  editable: boolean;
}

/** A value the deployment supplies, which this panel cannot override. The string is the server's
 *  own (`gatewayconfig.SourceEnvironment`); comparing against `"env"` matched nothing, so an
 *  environment-supplied credential rendered as an empty, editable box that the server would then
 *  refuse to write. */
const SOURCE_ENVIRONMENT = "environment";

interface ProviderChoice {
  id: string;
  name: string;
  extension: string;
  /** A provider that needs no credentials reports `null`, not an empty list. */
  fields: Array<{ key: string; label: string; env: string; is_secret: boolean; hint?: string }> | null;
  env_only?: Array<{ key: string; env: string; hint?: string }>;
  note?: string;
}

interface IntegrationConfig {
  kind: string;
  provider: string;
  provider_name: string;
  provider_source: string;
  provider_editable: boolean;
  enabled: boolean;
  enabled_source: string;
  enabled_editable: boolean;
  extension_path: string;
  extension_source: string;
  values: ConfigValue[] | null;
  providers: ProviderChoice[] | null;
  notes?: string[];
}

interface TestResult {
  ok: boolean;
  code?: string;
  detail?: string;
  server?: {
    provider: string;
    provider_name: string;
    extension: string;
    publisher: string;
    protocol_version: number;
    capabilities: string[] | null;
    plans: Array<{ id: string; name: string; amount?: Amount; period?: string }> | null;
    regions: string[] | null;
    duration_ms: number;
  };
  notes?: string[];
  tested_at: string;
}

const ENDPOINT = "/api/v1/staff/servers";
const TEST_ENDPOINT = "/api/v1/staff/servers/test";

export default async function serversPage(ctx: Ctx): Promise<Node> {
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

  const providerSelect = h("select", { class: "input", id: "srv-provider" }) as HTMLSelectElement;
  providerSelect.append(new Option("Choose a provisioner…", ""));
  for (const p of providers) {
    providerSelect.append(new Option(p.name, p.id));
  }
  providerSelect.value = cfg.provider;
  providerSelect.disabled = !cfg.provider_editable;

  const fields = new Map<string, HTMLInputElement>();
  const fieldHost = h("div", { class: "stack-sm" });

  const paintFields = (): void => {
    fields.clear();
    const provider = providers.find((p) => p.id === providerSelect.value) ?? null;
    if (!provider) {
      swap(
        fieldHost,
        h(
          "p",
          { class: "field-hint" },
          providers.length === 0
            ? "This build ships no launchable provisioner extensions, so there is nothing to configure."
            : "Choose a provisioner to see the credentials it needs.",
        ),
      );
      return;
    }
    const stored = new Map((cfg.values ?? []).map((v) => [v.key, v]));

    const rows: Node[] = (provider.fields ?? []).map((f) => {
      const current = stored.get(f.key);
      const fromEnv = current?.source === SOURCE_ENVIRONMENT;
      const editable = current ? current.editable : true;

      const el = input({
        type: f.is_secret ? "password" : "text",
        id: `srv-${f.key}`,
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
      if (!f.is_secret && current && !fromEnv) {
        const raw = (current as unknown as { value?: string }).value;
        if (typeof raw === "string") el.value = raw;
      }
      fields.set(f.key, el);

      const marks: Node[] = [];
      if (f.is_secret && current?.is_set) marks.push(badge("set", "success"));
      if (fromEnv) marks.push(badge("from environment", "info"));
      return field({
        label: f.label,
        // The marks travel together: beside the input while there is room, and as one group
        // under it when there is not. Wrapped individually, "set" stays on the line and "from
        // environment" drops below it, which reads as two unrelated states.
        control:
          marks.length > 0
            ? h("div", { class: "inline-row" }, el, h("span", { class: "chip-row" }, ...marks))
            : el,
        id: `srv-${f.key}`,
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
              h("li", null, h("code", null, e.env), e.hint ? h("span", { class: "field-hint" }, ` — ${e.hint}`) : null),
            ),
          ),
        ),
      );
    }
    if (provider.note) rows.push(h("p", { class: "field-hint" }, provider.note));
    swap(fieldHost, h("div", { class: "stack-sm" }, ...rows));
  };
  providerSelect.addEventListener("change", paintFields);
  // The card is titled "Provisioner" and says it is where the credentials live; without this
  // first paint the configured provisioner's fields are absent until the operator re-picks the
  // provisioner that is already selected.
  paintFields();

  const save = async (): Promise<void> => {
    const values: Record<string, string> = {};
    for (const [key, el] of fields) {
      if (el.type === "password" && el.value === "") continue;
      values[key] = el.value;
    }
    try {
      await ctx.api.put<IntegrationConfig>(ENDPOINT, { provider: providerSelect.value, values });
      ctx.toast("Provisioner configuration saved", { tone: "success" });
    } catch (err) {
      ctx.toast("The configuration was not saved", { tone: "danger", detail: describeWriteError(err) });
      return;
    }
    await reload();
  };

  const resultHost = h("div");

  return h(
    "div",
    { class: "page stack" },
    pageHead({
      title: "Provisioning servers",
      sub: "The provisioner turns a paid invoice into a running server. Test it before a customer needs it.",
    }),

    (cfg.notes ?? []).length > 0
      ? banner("info", "About this configuration", h("ul", { class: "plain-list" }, ...(cfg.notes ?? []).map((n) => h("li", null, n))))
      : null,

    providers.length === 0
      ? card({
          body: emptyState({
            title: "No provisioner extensions in this build",
            body: "A provisioner is an extension binary. Build one from extensions/ and point the deployment at it, then come back.",
            icon: "layers",
          }),
        })
      : null,

    card({
      title: "Provisioner",
      sub: "A plan is a bundle of limits. The provisioner decides which plan ids it will build, and this screen is where you find out which.",
      body: h(
        "div",
        { class: "stack" },
        field({
          label: "Provisioner",
          control: providerSelect,
          id: "srv-provider",
          hint: cfg.provider_source === SOURCE_ENVIRONMENT ? "Supplied by the environment." : undefined,
        }),
        fieldHost,
      ),
    }),

    card({
      title: "Connection",
      sub: "Launches the configured extension, performs the protocol handshake and asks which plans and regions it can build.",
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "div",
          { class: "inline-row" },
          btn("Save", {
            icon: "check",
            variant: "primary",
            onclick: (e) => {
              const button = e.currentTarget as HTMLButtonElement;
              void withBusy(ctx, button, () => save());
            },
          }),
          btn("Test connection", {
            icon: "zap",
            onclick: (e) => {
              const button = e.currentTarget as HTMLButtonElement;
              void withBusy(ctx, button, async () => {
                const result = await runTest(ctx);
                swap(resultHost, renderTestResult(result));
                resultHost.scrollIntoView({ block: "nearest", behavior: "smooth" });
              });
            },
          }),
        ),
        h(
          "p",
          { class: "field-hint" },
          "The test runs against the saved configuration, not what is typed above. Press Save first if you have changed something.",
        ),
        resultHost,
        cfg.extension_path ? h("p", { class: "field-hint" }, "Extension: ", h("code", null, cfg.extension_path)) : null,
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
    return {
      ok: false,
      code: err instanceof ApiError ? err.code : "request_failed",
      detail: err instanceof Error ? err.message : "The test could not be run.",
      tested_at: new Date().toISOString(),
    };
  }
}

function renderTestResult(result: TestResult): Node {
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

  const srv = result.server;
  if (!srv) {
    return banner("info", "The test passed", h("p", null, "The extension answered but described nothing."));
  }

  const plans = srv.plans ?? [];
  const regions = srv.regions ?? [];

  return h(
    "div",
    { class: "stack-sm" },
    banner(
      "success",
      `Connected to ${srv.provider_name || srv.provider}`,
      h(
        "p",
        null,
        "The extension handshook at protocol v",
        String(srv.protocol_version),
        " and answered in ",
        String(srv.duration_ms),
        " ms. Tested ",
        dateTime(result.tested_at),
        ".",
      ),
    ),

    metaGrid([
      { label: "Extension", value: h("span", null, srv.extension, srv.publisher ? h("span", { class: "faint" }, ` · ${srv.publisher}`) : null) },
      { label: "Protocol", value: h("code", null, `v${srv.protocol_version}`) },
      { label: "Plans", value: String(plans.length) },
      { label: "Regions", value: regions.length > 0 ? regions.join(", ") : "Any the panel offers" },
    ]),

    // The plan table is the point of the screen. A product sold against a plan id the
    // provisioner does not advertise produces an invoice that looks correct and a server that
    // never appears, so the ids are shown verbatim and the wording says how to use them.
    plans.length > 0
      ? h(
          "div",
          { class: "stack-sm" },
          h("h3", { class: "section-title" }, "Plans this provisioner will build"),
          h(
            "div",
            { class: "table-wrap table-wrap--cards" },
            h(
              "table",
              { class: "table" },
              h(
                "thead",
                null,
                h("tr", null,
                  h("th", { scope: "col" }, "Plan id"),
                  h("th", { scope: "col" }, "Name"),
                  h("th", { scope: "col" }, "Period"),
                ),
              ),
              h(
                "tbody",
                null,
                ...plans.map((p) =>
                  h(
                    "tr",
                    null,
                    h("td", { class: "cell-primary" }, h("code", null, p.id)),
                    h("td", { "data-label": "Name" }, p.name || p.id),
                    h("td", { "data-label": "Period" }, p.period ?? "—"),
                  ),
                ),
              ),
            ),
          ),
          h(
            "p",
            { class: "field-hint" },
            "A product's plan id must be one of these. If a product sells a plan that is not in this list, checkout will take the money and provisioning will refuse the job — so compare this list against the catalogue before you sell anything.",
          ),
        )
      : banner(
          "warning",
          "This provisioner advertises no plans",
          h("p", null, "A provisioner with no plans cannot build anything. Check its configuration — the plan list is usually supplied by an environment variable."),
        ),

    (srv.capabilities ?? []).length > 0
      ? h(
          "p",
          { class: "field-hint" },
          "Capabilities: ",
          h("span", { class: "chip-row" }, ...(srv.capabilities ?? []).map((c) => h("code", null, c))),
        )
      : null,
  );
}

function describeWriteError(err: unknown): string {
  if (err instanceof ApiError) {
    switch (err.code) {
      case "env_override":
        return err.message;
      case "unknown_provider":
        return "That provisioner is not one this build can launch.";
      case "provider_required":
        return "Choose a provisioner before saving.";
      case "nothing_to_update":
        return "Nothing changed, so nothing was saved.";
      default:
        return err.message || "The configuration could not be saved.";
    }
  }
  return err instanceof Error ? err.message : "The configuration could not be saved.";
}
