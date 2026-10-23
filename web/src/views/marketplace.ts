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
//
// Two more things the screen can now say, because "installed" was doing two jobs (ADM-12, OPS-14):
//
//  - **Enabled is a separate fact from installed.** An artifact kept for rollback is still on disk
//    and still verified; it is not the gateway the deployment should use. The switch is per row and
//    the row shows both, so a support engineer can answer "which of these four is live".
//  - **An offline installation can still install.** Upload takes the artifact from the operator's
//    machine, hashes it here, and records the digest — the same verification with the operator as
//    the source of truth. Scaffold generates a runnable starting point so an internal extension does
//    not begin as an empty file.

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
  /** Whether the deployment should use this artifact. False keeps it installed for rollback. */
  enabled: boolean;
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
  out.appendChild(installedCard(ctx, state, reload));
  out.appendChild(uploadCard(ctx, reload));
  out.appendChild(scaffoldCard(ctx));
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

function installedCard(ctx: Ctx, state: MarketplaceState, reload: () => void): HTMLElement {
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

    // The enable switch. It is a PATCH of one field, and the response is the stored row, so the
    // screen shows what the database holds rather than what was clicked.
    const toggle = btn(row.enabled ? "Disable" : "Enable", { size: "sm", icon: "eye" });
    toggle.addEventListener("click", () => {
      void withBusy(ctx, toggle, async () => {
        await ctx.api.patch(
          `/api/v1/staff/marketplace/installed/${encodeURIComponent(row.name)}/${encodeURIComponent(row.version)}`,
          { enabled: !row.enabled },
        );
        ctx.toast(row.enabled ? `${row.name} is disabled` : `${row.name} is enabled`, {
          tone: "success",
          detail: row.enabled
            ? "The artifact stays installed and verified; the deployment should stop using it."
            : "The deployment will use it once its environment names the path and the worker restarts.",
        });
        reload();
      });
    });

    // The upgrade. It is the ordinary indexed install of the newer version: the index entry is
    // verified and stored as a second row, and the operator disables the old one when they cut over.
    const newer = newestAvailable(state.index, row.name, row.version);
    const upgrade = newer
      ? btn(`Update to ${newer.version}`, { size: "sm", icon: "arrowUpRight" })
      : null;
    if (upgrade && newer) {
      const target = newer;
      upgrade.addEventListener("click", () => {
        void withBusy(ctx, upgrade, async () => {
          await ctx.api.post("/api/v1/staff/marketplace/install", {
            name: target.name,
            version: target.version,
          });
          ctx.toast(`${target.name} ${target.version} is installed`, {
            tone: "success",
            detail: `Disable ${row.version} when you are ready to switch; both artifacts stay verified.`,
          });
          reload();
        });
      });
    }

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
              row.enabled ? badge("enabled", "accent", { dot: true }) : badge("disabled", "neutral"),
              row.pinned ? badge("pinned", "info") : null,
            ),
            h("p", { class: "faint" }, "sha256 ", code(row.sha256)),
            h(
              "p",
              { class: "faint" },
              `installed ${relative(row.installed_at)} · ${row.size_bytes} bytes · `,
              row.staged_path ? mono(row.staged_path) : "stored in the panel",
            ),
            newer
              ? h("p", { class: "field-hint" }, `The index lists ${target_version(newer)}, which is newer than this one.`)
              : null,
          ),
          h("div", { class: "spacer" }),
          h("div", { class: "row" }, upgrade, toggle, download),
        ),
      ),
    );
  }

  return card({
    title: "Installed",
    sub: "Verified and stored. `enabled` is the operator's intent; the process starts only what its environment points at.",
    body: rows,
  });
}

// ---------------------------------------------------------------- local install --

/** The artifact upload (OPS-14).
 *
 *  The bytes go up as they are, with the metadata in headers: a JSON envelope around a binary would
 *  cost a third more and buffer the artifact twice. The expected digest is optional and is what
 *  makes an upload from a build machine checkable at the receiving end. */
function uploadCard(ctx: Ctx, reload: () => void): HTMLElement {
  const name = input({ id: "upload-name", placeholder: "internalgw", autocomplete: "off" });
  const version = input({ id: "upload-version", placeholder: "1.0.0", autocomplete: "off" });
  const kind = h(
    "select",
    { class: "input", id: "upload-kind" },
    ...Object.entries(KIND_LABEL).map(([value, label]) => h("option", { value }, label)),
  ) as HTMLSelectElement;
  const publisher = input({ id: "upload-publisher", placeholder: "Your team", autocomplete: "off" });
  const sha = input({ id: "upload-sha", placeholder: "optional: 64 hex characters to require", autocomplete: "off" });
  const fileInput = h("input", { class: "input", id: "upload-file", type: "file" }) as HTMLInputElement;
  const submit = btn("Upload and verify", { variant: "primary", size: "sm", icon: "arrowUpRight" });

  submit.addEventListener("click", () => {
    const file = fileInput.files?.[0];
    if (!file) {
      ctx.toast("Choose a file", { tone: "danger", detail: "The artifact is what gets verified." });
      return;
    }
    if (!name.value.trim() || !version.value.trim()) {
      ctx.toast("Name and version are required", {
        tone: "danger",
        detail: "They become the directory and the row; a digest with no name cannot be recognised later.",
      });
      return;
    }
    void withBusy(ctx, submit, async () => {
      const result = await ctx.api.postRaw<InstallResult>("/api/v1/staff/marketplace/upload", file, {
        contentType: "application/octet-stream",
        headers: {
          "X-Extension-Name": name.value.trim(),
          "X-Extension-Version": version.value.trim(),
          "X-Extension-Kind": kind.value,
          "X-Extension-Publisher": publisher.value.trim(),
          "X-Extension-Sha256": sha.value.trim(),
        },
      });
      ctx.toast(`${result.name} ${result.version} verified`, {
        tone: "success",
        detail: result.sha256,
      });
      fileInput.value = "";
      sha.value = "";
      reload();
    });
  });

  return card({
    title: "Upload an artifact",
    sub: "For an installation with no index. The digest is computed here, from the bytes you upload.",
    body: h(
      "div",
      { class: "stack-sm" },
      h("div", { class: "grid-2" }, field({ label: "Name", control: name }), field({ label: "Version", control: version })),
      h("div", { class: "grid-2" }, field({ label: "Kind", control: kind }), field({ label: "Publisher", control: publisher })),
      field({ label: "Expected sha256 (optional)", control: sha }),
      field({ label: "Artifact", control: fileInput }),
      h(
        "div",
        { class: "row-wrap" },
        submit,
        h(
          "span",
          { class: "field-hint" },
          "Uploading proves the stored bytes are the bytes you have. It does not prove who built them — there is no index to check a publisher against.",
        ),
      ),
    ),
  });
}

/** The scaffold (ADM-12).
 *
 *  A generated zip rather than a template in the repository, because it carries the operator's name,
 *  kind and version. The download is a plain link: it is a GET that stores nothing, and an operator
 *  who is starting an extension wants a file, not a fetch wrapper. */
function scaffoldCard(ctx: Ctx): HTMLElement {
  const name = input({ id: "scaffold-name", placeholder: "internalgw", autocomplete: "off" });
  const version = input({ id: "scaffold-version", placeholder: "0.1.0", value: "0.1.0", autocomplete: "off" });
  const kind = h(
    "select",
    { class: "input", id: "scaffold-kind" },
    ...Object.entries(KIND_LABEL).map(([value, label]) => h("option", { value }, label)),
  ) as HTMLSelectElement;
  const download = btn("Download starter zip", { size: "sm", icon: "download" });
  download.addEventListener("click", () => {
    const wanted = name.value.trim();
    if (!wanted) {
      ctx.toast("Name your extension", { tone: "danger", detail: "It becomes the directory and the manifest." });
      return;
    }
    const query = new URLSearchParams({ name: wanted, version: version.value.trim() || "0.1.0", kind: kind.value });
    window.location.assign(`/api/v1/staff/marketplace/scaffold?${query.toString()}`);
  });

  return card({
    title: "Start an extension",
    sub: "A manifest, a runnable stdio stub and a README that names the handshake",
    body: h(
      "div",
      { class: "stack-sm" },
      h("div", { class: "grid-2" }, field({ label: "Name", control: name }), field({ label: "Version", control: version })),
      field({ label: "Kind", control: kind }),
      h(
        "div",
        { class: "row-wrap" },
        download,
        h(
          "span",
          { class: "field-hint" },
          "The stub answers initialize and health and refuses everything else, so the panel reports what is missing instead of hanging.",
        ),
      ),
    ),
  });
}

/** newestAvailable returns the newest index entry for name that is strictly newer than version. */
function newestAvailable(index: MarketplaceIndex, name: string, version: string): IndexExtension | null {
  let best: IndexExtension | null = null;
  for (const ext of index.extensions ?? []) {
    if (ext.name !== name) continue;
    if (compareVersions(ext.version, version) <= 0) continue;
    if (!best || compareVersions(ext.version, best.version) > 0) best = ext;
  }
  return best;
}

/** compareVersions orders two semantic versions by major, minor, patch.
 *
 *  Pre-release ordering is deliberately not implemented: the index is the panel's own listing, and a
 *  screen that mis-ordered "1.0.0-rc1" and "1.0.0" would offer the wrong upgrade. The server's
 *  update parser is the authority for releases; this only decides whether to *offer* an update, and
 *  the installer verifies whatever it installs anyway. */
function compareVersions(a: string, b: string): number {
  const parse = (raw: string): number[] => {
    const core = raw.trim().replace(/^v/, "").split("-")[0] ?? "";
    const parts = core.split(".").map((part) => Number.parseInt(part, 10));
    return [parts[0] ?? 0, parts[1] ?? 0, parts[2] ?? 0];
  };
  const left = parse(a);
  const right = parse(b);
  for (let i = 0; i < 3; i += 1) {
    const l = left[i] ?? 0;
    const r = right[i] ?? 0;
    if (l !== r) return l > r ? 1 : -1;
  }
  return 0;
}

/** target_version names a version for a sentence without repeating the field accesses. */
function target_version(ext: IndexExtension): string {
  return `${ext.name} ${ext.version}`;
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
