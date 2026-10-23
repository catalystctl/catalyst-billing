// The extension marketplace.
//
// The screen's job is to be honest about three things, because each one is easy to fake:
//
//  - **An install is verified, not running.** The core loads extensions as separate processes
//    from paths in its own environment (`BILLING_GATEWAY_EXTENSION`,
//    `BILLING_PROVISIONER_EXTENSION`) and has no runtime registration. So a row here says
//    `verified` and carries the activation instruction, and nothing says "running". There is no
//    progress bar: a bar would be a claim about a download the panel is not doing on your
//    behalf after the fact.
//
//  - **A digest from the index is not a signature.** Whoever controls the index controls the
//    digest too, so an unpinned install proves the transfer was not corrupted and nothing more.
//    The page states that once, from the server's `trust` field, and the pin field is right
//    next to the install button because it is the only check a compromised index cannot forge.
//
//  - **A failure leaves nothing.** A refused install produces a problem document with a stable
//    code and no row; the page reloads the installed list after every attempt rather than
//    optimistically adding a row that the server refused.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateTime, relative, titleCase } from "../lib/format";
import type { Ctx } from "../lib/types";
import {
  badge,
  banner,
  btn,
  card,
  code,
  emptyState,
  field,
  input,
  metaGrid,
  mono,
  pageHead,
  resource,
  sectionTitle,
  skeletonPage,
  withBusy,
} from "../ui/kit";

export const meta = { title: "Marketplace", crumb: "Marketplace" };

// ---------------------------------------------------------------- payloads --

/** One entry in the index, from `marketplace.Extension`. */
interface IndexExtension {
  name: string;
  version: string;
  publisher: string;
  description: string;
  kind: "gateway" | "provisioner" | "other";
  download_url: string;
  sha256: string;
  homepage: string;
}

interface MarketplaceIndex {
  schema_version: number;
  extensions: IndexExtension[];
}

/** One installed extension, from `marketplace.Installed`. */
interface Installed {
  name: string;
  version: string;
  publisher: string;
  description: string;
  kind: string;
  sha256: string;
  pinned: boolean;
  source_url: string;
  size_bytes: number;
  staged_path: string;
  installed_at: string;
  status: "verified";
}

interface MarketplaceState {
  configured: boolean;
  index_url: string;
  install_dir: string;
  fetched_at: string | null;
  cached: boolean;
  index: MarketplaceIndex;
  failure_code: string;
  installed: Installed[];
  trust: string;
}

/** The install result, from `marketplace.InstallResult`. */
interface InstallResult {
  name: string;
  version: string;
  sha256: string;
  pinned: boolean;
  verified_at: string;
  size_bytes: number;
  staged_path: string;
  status: "verified";
  activation: string;
  trust: string;
}

const KIND_LABEL: Record<string, string> = {
  gateway: "Payment gateway",
  provisioner: "Provisioner",
  other: "Other",
};

export default async function marketplacePage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("extension:manage")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Marketplace" }),
      banner(
        "danger",
        "You cannot install extensions",
        "This screen needs the extension:manage permission: an extension runs as a separate process with the " +
          "credentials it is given.",
        btn("Back to the dashboard", { href: "/dashboard", size: "sm", variant: "ghost" }),
      ),
    );
  }

  const host = h("div");
  const load = (signal: AbortSignal): Promise<MarketplaceState> =>
    ctx.api.get<MarketplaceState>("/api/v1/staff/marketplace", { cache: false, signal });

  const paint = (): void => {
    swap(host, resource(ctx, load, (state) => renderState(ctx, state, paint), { skeleton: skeletonPage }));
  };
  paint();

  return h("div", { class: "page" }, pageHead({ title: "Marketplace", sub: "Verified extension installs" }), host);
}

// ------------------------------------------------------------------ render --

function renderState(ctx: Ctx, state: MarketplaceState, reload: () => void): Node {
  const out = h("div", { class: "stack" });

  out.appendChild(configCard(ctx, state, reload));

  if (!state.configured) {
    out.appendChild(
      card({
        body: emptyState({
          title: "No marketplace index is configured",
          body: "Point the panel at a JSON index and the published extensions appear here. Nothing is fetched until then.",
          icon: "box",
        }),
      }),
    );
    return out;
  }

  if (state.failure_code) {
    out.appendChild(
      banner(
        "warning",
        "The index could not be read",
        `The last fetch failed with ${state.failure_code}. What is shown below is nothing at all, not an empty marketplace — ` +
          "the two are different, and this one is a problem with the index.",
      ),
    );
  }

  out.appendChild(
    banner(
      "neutral",
      "A digest from the index is not a signature",
      state.trust,
    ),
  );

  out.appendChild(indexCard(ctx, state, reload));
  out.appendChild(installedCard(ctx, state));
  return out;
}

// ------------------------------------------------------------------- config --

function configCard(ctx: Ctx, state: MarketplaceState, reload: () => void): HTMLElement {
  const indexURL = input({
    id: "marketplace-index-url",
    value: state.index_url,
    placeholder: "https://extensions.example.com/index.json",
  });
  const installDir = input({
    id: "marketplace-install-dir",
    value: state.install_dir,
    placeholder: "/var/lib/billing/extensions",
  });
  const save = btn("Save", { variant: "primary", size: "sm", icon: "check" });
  const refresh = btn("Refresh index", { size: "sm", icon: "refresh" });
  if (refresh instanceof HTMLButtonElement) refresh.disabled = !state.configured;

  save.addEventListener("click", () => {
    void withBusy(ctx, save, async () => {
      try {
        await ctx.api.put("/api/v1/staff/marketplace/settings", {
          index_url: indexURL.value.trim(),
          install_dir: installDir.value.trim(),
        });
        ctx.toast("Saved", { tone: "success" });
        reload();
      } catch (err) {
        report(ctx, err, "The marketplace settings could not be saved");
      }
    });
  });

  refresh.addEventListener("click", () => {
    void withBusy(ctx, refresh, async () => {
      try {
        await ctx.api.post("/api/v1/staff/marketplace/refresh", {});
        ctx.toast("Index refreshed", { tone: "success" });
        reload();
      } catch (err) {
        report(ctx, err, "The index could not be refreshed");
      }
    });
  });

  const meta = metaGrid([
    {
      label: "Last fetched",
      value: state.fetched_at ? `${dateTime(state.fetched_at)}${state.cached ? " (cached)" : ""}` : "never",
    },
    { label: "Staging", value: state.install_dir ? mono(state.install_dir) : "not configured — artifacts are stored in the panel" },
  ]);

  return card({
    title: "Configuration",
    sub: "Where the listing comes from, and where a verified artifact is written",
    actions: refresh,
    body: h(
      "div",
      { class: "stack-sm" },
      h(
        "div",
        { class: "form-grid" },
        field({
          label: "Index URL",
          control: indexURL,
          hint: "A JSON listing: name, version, publisher, description, download_url and sha256 per extension.",
        }),
        field({
          label: "Staging directory",
          control: installDir,
          hint: "Optional. A verified artifact is written here atomically. Empty means the panel stores it only, and you download it from this page.",
        }),
        h("div", { class: "form-actions" }, save),
      ),
      meta,
    ),
  });
}

// -------------------------------------------------------------------- index --

function indexCard(ctx: Ctx, state: MarketplaceState, reload: () => void): HTMLElement {
  const extensions = state.index.extensions ?? [];
  if (extensions.length === 0) {
    return card({
      title: "Available extensions",
      body: emptyState({
        title: state.failure_code ? "The index could not be read" : "The index lists no extensions",
        body: state.failure_code
          ? "Refresh the index once it is reachable. Nothing is installed or hidden by this failure."
          : "The index answered and listed nothing to install.",
        icon: "box",
      }),
    });
  }

  const grid = h("div", { class: "stack-sm" });
  for (const ext of extensions) {
    grid.appendChild(extensionRow(ctx, ext, state, reload));
  }
  return card({
    title: "Available extensions",
    sub: `${extensions.length} published`,
    body: grid,
  });
}

function extensionRow(ctx: Ctx, ext: IndexExtension, state: MarketplaceState, reload: () => void): HTMLElement {
  const installed = state.installed.find((row) => row.name === ext.name && row.version === ext.version);
  const pin = input({
    id: `pin-${ext.name}-${ext.version}`,
    placeholder: "Pinned sha256 (optional)",
  });
  const install = btn(installed ? "Re-verify" : "Install", {
    variant: installed ? "default" : "primary",
    size: "sm",
    icon: "download",
  });

  const resultHost = h("div");

  install.addEventListener("click", () => {
    void withBusy(ctx, install, async () => {
      const pinned = pin.value.trim();
      const body: Record<string, unknown> = { name: ext.name, version: ext.version };
      if (pinned !== "") body.pinned_sha256 = pinned;
      try {
        const result = await ctx.api.post<InstallResult>("/api/v1/staff/marketplace/install", body);
        ctx.toast(`Verified ${result.name} ${result.version}`, {
          tone: "success",
          detail: "The digest matched. It is not running: see the activation step.",
        });
        swap(resultHost, installResult(result));
        reload();
      } catch (err) {
        // A refusal is a real answer and is rendered where the button is, not only as a
        // toast: "the digest did not match" is the sentence this feature exists to say.
        swap(resultHost, refusal(err, ext));
        reload();
      }
    });
  });

  return h(
    "div",
    { class: "card card--flat" },
    h(
      "div",
      { class: "row-wrap" },
      h(
        "div",
        { class: "stack-sm" },
        h(
          "div",
          { class: "row" },
          h("strong", null, ext.name),
          mono(ext.version),
          badge(KIND_LABEL[ext.kind] ?? titleCase(ext.kind), "neutral"),
          installed ? badge("verified", "success", { dot: true }) : null,
        ),
        h("p", { class: "dim" }, ext.description || "No description."),
        h(
          "p",
          { class: "faint" },
          ext.publisher ? `Published by ${ext.publisher} · ` : "",
          "sha256 ",
          code(ext.sha256),
        ),
      ),
      h("div", { class: "spacer" }),
      h("div", { class: "stack-sm" }, pin, install),
    ),
    resultHost,
  );
}

function installResult(result: InstallResult): HTMLElement {
  return h(
    "div",
    { class: "stack-sm" },
    banner(
      "success",
      `Verified ${result.name} ${result.version}`,
      "The downloaded bytes hash to the digest, and that is what is stored.",
    ),
    metaGrid([
      { label: "Digest", value: code(result.sha256) },
      { label: "Pinned", value: result.pinned ? "yes — checked against your out-of-band digest" : "no" },
      { label: "Size", value: `${result.size_bytes} bytes` },
      { label: "Verified", value: dateTime(result.verified_at) },
      { label: "Staged at", value: result.staged_path ? mono(result.staged_path) : "stored in the panel only" },
    ]),
    h("p", { class: "faint" }, result.trust),
    h("p", { class: "dim" }, result.activation),
  );
}

function refusal(err: unknown, ext: IndexExtension): HTMLElement {
  if (err instanceof ApiError) {
    // The digest code is the one that matters, so it gets its own sentence rather than a
    // generic "that did not work".
    if (err.code === "marketplace_digest_mismatch") {
      return banner(
        "danger",
        `${ext.name} ${ext.version} was not installed`,
        `${err.detail} Nothing was recorded and nothing was staged. If you pinned the digest from the release ` +
          "announcement, the index and the artifact disagree — do not install this artifact.",
      );
    }
    return banner("danger", `${ext.name} ${ext.version} was not installed`, `${err.detail} (${err.code})`);
  }
  if (err instanceof DOMException && err.name === "AbortError") return h("span", {});
  return banner("danger", `${ext.name} ${ext.version} was not installed`, String(err));
}

// ---------------------------------------------------------------- installed --

function installedCard(ctx: Ctx, state: MarketplaceState): HTMLElement {
  if (state.installed.length === 0) {
    return card({
      title: "Installed",
      body: emptyState({
        title: "Nothing is installed",
        body: "A verified extension appears here with the digest that was checked.",
        icon: "box",
      }),
    });
  }

  const rows = h("div", { class: "stack-sm" });
  for (const row of state.installed) {
    const download = btn("Download artifact", { size: "sm", icon: "download" });
    download.addEventListener("click", () => {
      // The browser fetches the stored bytes; the server sets the digest in a header and a
      // content type that cannot be rendered.
      window.location.assign(
        `/api/v1/staff/marketplace/installed/${encodeURIComponent(row.name)}/${encodeURIComponent(row.version)}/artifact`,
      );
    });

    rows.appendChild(
      h(
        "div",
        { class: "card card--flat" },
        h(
          "div",
          { class: "row-wrap" },
          h(
            "div",
            { class: "stack-sm" },
            h(
              "div",
              { class: "row" },
              h("strong", null, row.name),
              mono(row.version),
              badge("verified", "success", { dot: true }),
              row.pinned ? badge("pinned", "info") : null,
            ),
            h("p", { class: "faint" }, "sha256 ", code(row.sha256)),
            h(
              "p",
              { class: "faint" },
              `installed ${relative(row.installed_at)} · ${row.size_bytes} bytes · `,
              row.staged_path ? mono(row.staged_path) : "stored in the panel",
            ),
          ),
          h("div", { class: "spacer" }),
          download,
        ),
      ),
    );
  }

  return card({
    title: "Installed",
    sub: "Verified and stored. Not running: the core starts only what its environment names.",
    body: rows,
  });
}

// ------------------------------------------------------------------ helpers --

function report(ctx: Ctx, err: unknown, fallback: string): void {
  if (err instanceof ApiError) {
    ctx.toast(err.detail || fallback, { tone: "danger", detail: err.code });
    return;
  }
  if (err instanceof DOMException && err.name === "AbortError") return;
  ctx.toast(fallback, { tone: "danger", detail: String(err) });
}
