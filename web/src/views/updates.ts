// Updates and telemetry.
//
// Three facts, kept separate on purpose because merging them is how a screen starts lying:
//
//  - **What this build is.** `running_version` comes from the binary's link-time stamp. The
//    default stamp is `dev`, and when it is not a version the page says the build cannot be
//    compared rather than showing "up to date" — a development build is not up to date, it is
//    unknown.
//  - **What is published.** The check is cached on the server, so this page renders
//    `check.cached` and `check.expires_at` rather than implying every visit asked the update
//    server. "Check now" is the only thing that fetches.
//  - **What an operator approved.** An approval records a decision. It does not install
//    anything: the panel is a plain binary that cannot replace itself, and `apply.steps` is
//    the deploy procedure, shown verbatim. There is no button here that downloads an
//    executable, because there is no code behind one.
//
// Telemetry is off unless it is switched on, and the preview is the payload itself: the page
// shows the exact bytes that would be sent, so "nothing identifying" is something an operator
// can check rather than something this page asserts.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateTime, relative, titleCase } from "../lib/format";
import { icon } from "../lib/icons";
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
  select,
  skeletonPage,
  withBusy,
} from "../ui/kit";

export const meta = { title: "Updates", crumb: "Updates" };

// ---------------------------------------------------------------- payloads --

/** The check, from `updates.Result` in internal/updates/check.go. */
interface UpdateCheck {
  configured: boolean;
  source_url: string;
  channel: string;
  running_version: string;
  latest_version: string;
  download_url: string;
  notes: string;
  published_at: string | null;
  outcome: "not_configured" | "up_to_date" | "available" | "ahead" | "unknown" | "unreachable";
  checked_at: string;
  cached: boolean;
  expires_at: string | null;
  failure_code: string;
  detail: string;
}

interface Approval {
  version: string;
  note: string;
  approved_by: string;
  approved_at: string;
}

interface LastSend {
  id: string;
  sent_at: string;
  status: "sent" | "failed";
  http_status: number;
  failure_code: string;
  endpoint_host: string;
}

interface UpdateState {
  running_version: string;
  channel: string;
  check: UpdateCheck;
  approval: Approval | null;
  approval_matches_check: boolean;
  telemetry: {
    enabled: boolean;
    endpoint_configured: boolean;
    endpoint_host: string;
    instance_id: string;
    last_send: LastSend | null;
    /** The job that keeps the "reported daily" promise. Null when nothing schedules a delivery. */
    schedule: { job: string; interval_hours: number; kill_switch: string } | null;
  };
  apply: { mode: string; steps: string[]; note: string };
  server_time: string;
}

/** The preview, from `telemetryPreview` in internal/httpapi/updates.go. */
interface TelemetryPreview {
  enabled: boolean;
  payload: Record<string, unknown> | null;
  payload_json: string;
  endpoint_host: string;
  delivery_possible: boolean;
  fields: string[];
  last_send: LastSend | null;
}

/** The outcome, as the sentence the page shows. A status the server introduces is rendered
 *  with `titleCase` rather than crashing the screen. */
const OUTCOME_LABEL: Record<string, string> = {
  not_configured: "No update server configured",
  up_to_date: "Up to date",
  available: "An update is available",
  ahead: "Ahead of the published release",
  unknown: "This build cannot be compared",
  unreachable: "The update server could not be read",
};

const OUTCOME_TONE: Record<string, "success" | "info" | "warning" | "danger" | "neutral"> = {
  not_configured: "neutral",
  up_to_date: "success",
  available: "info",
  ahead: "warning",
  unknown: "neutral",
  unreachable: "danger",
};

export default async function updatesPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("update:manage")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Updates" }),
      banner(
        "danger",
        "You cannot manage updates",
        "This screen needs the update:manage permission: approving a version is a decision about what the " +
          "whole installation runs.",
        btn("Back to the dashboard", { href: "/dashboard", size: "sm", variant: "ghost" }),
      ),
    );
  }

  const host = h("div");
  const load = (signal: AbortSignal): Promise<UpdateState> =>
    ctx.api.get<UpdateState>("/api/v1/staff/updates", { cache: false, signal });

  const paint = (): void => {
    swap(host, resource(ctx, load, (state) => renderState(ctx, state, paint), { skeleton: skeletonPage }));
  };
  paint();

  return h("div", { class: "page" }, pageHead({ title: "Updates", sub: "Version, update check and telemetry" }), host);
}

// ------------------------------------------------------------------ render --

function renderState(ctx: Ctx, state: UpdateState, reload: () => void): Node {
  const out = h("div", { class: "stack" });

  // The running version and the check, in one card: they are one question.
  out.appendChild(versionCard(ctx, state, reload));

  if (state.check.outcome === "available") {
    out.appendChild(
      banner(
        "info",
        state.approval_matches_check
          ? `Version ${state.check.latest_version} is available and approved`
          : `Version ${state.check.latest_version} is available`,
        state.approval_matches_check
          ? "Approving records the decision. Applying it is the deploy procedure below — the panel does not replace its own binary."
          : "Nothing is installed by approving it: approval records which version you intend to run.",
        state.check.download_url
          ? btn("Release notes", { href: state.check.download_url, size: "sm", variant: "ghost", icon: "external" })
          : undefined,
      ),
    );
  }

  out.appendChild(applyCard(state));
  out.appendChild(telemetryCard(ctx, state, reload));
  return out;
}

// ------------------------------------------------------------------ version --

function versionCard(ctx: Ctx, state: UpdateState, reload: () => void): HTMLElement {
  const check = state.check;
  const tone = OUTCOME_TONE[check.outcome] ?? "neutral";
  const label = OUTCOME_LABEL[check.outcome] ?? titleCase(check.outcome);

  const checkNow = btn("Check now", { icon: "refresh", size: "sm" });
  checkNow.addEventListener("click", () => {
    void withBusy(ctx, checkNow, async () => {
      try {
        await ctx.api.post("/api/v1/staff/updates/check", {});
        ctx.toast("Checked", { tone: "success", detail: "The update server answered." });
        reload();
      } catch (err) {
        report(ctx, err, "The update check did not complete");
      }
    });
  });

  const body = h("div", { class: "stack" });

  body.appendChild(
    metaGrid([
      { label: "Running version", value: h("span", { class: "mono" }, state.running_version) },
      { label: "Channel", value: titleCase(state.channel) },
      {
        label: "Status",
        value: h("span", { class: "row" }, badge(label, tone, { dot: true })),
      },
      {
        label: "Latest published",
        value: check.latest_version ? h("span", { class: "mono" }, check.latest_version) : "—",
      },
    ]),
  );

  if (check.outcome === "unknown") {
    // The honest explanation, not a shrug: a build with no version stamp has nothing to
    // compare, and saying "up to date" would be the wrong answer dressed as a good one.
    body.appendChild(
      banner(
        "neutral",
        "This build has no comparable version",
        `The binary reports “${state.running_version}”. A release build is stamped at link time ` +
          "(`-ldflags \"-X main.version=1.2.3\"`), so a development build cannot be compared with a release.",
      ),
    );
  }
  if (check.outcome === "unreachable") {
    body.appendChild(
      banner(
        "warning",
        "The last check failed",
        `${check.detail || "The update server did not answer."} The failure is cached briefly so a broken server is not asked on every page load.`,
      ),
    );
  }
  if (!check.configured) {
    body.appendChild(
      banner(
        "neutral",
        "No update server is configured",
        "Set a check URL below. The panel fetches it with a bounded timeout and a bounded body, and caches the answer.",
      ),
    );
  }

  if (check.checked_at) {
    body.appendChild(
      h(
        "p",
        { class: "faint" },
        icon("clock", 11),
        check.cached ? " Cached answer from " : " Checked ",
        dateTime(check.checked_at),
        check.cached && check.expires_at ? ` · believed until ${dateTime(check.expires_at)}` : "",
        check.cached ? " · a page load does not ask the update server" : "",
      ),
    );
  }

  if (check.notes) body.appendChild(h("p", { class: "dim" }, check.notes));

  body.appendChild(approvalForm(ctx, state, reload));

  return card({
    title: "Version",
    sub: "What this build is, and what the update server says",
    actions: checkNow,
    body,
  });
}

// ----------------------------------------------------------------- approval --

function approvalForm(ctx: Ctx, state: UpdateState, reload: () => void): HTMLElement {
  const out = h("div", { class: "stack" });

  if (state.approval) {
    const clear = btn("Withdraw approval", { variant: "ghost", size: "sm", icon: "x" });
    clear.addEventListener("click", () => {
      void withBusy(ctx, clear, async () => {
        const ok = await ctx.confirm({
          title: `Withdraw the approval of ${state.approval?.version}?`,
          body: "This records that no version is approved. It does not change what is running — nothing here does.",
          confirmLabel: "Withdraw approval",
          danger: true,
        });
        if (!ok) return;
        try {
          await ctx.api.del("/api/v1/staff/updates/approval");
          ctx.toast("Approval withdrawn", { tone: "success" });
          reload();
        } catch (err) {
          report(ctx, err, "The approval could not be withdrawn");
        }
      });
    });

    out.appendChild(
      h(
        "div",
        { class: "row" },
        badge(
          state.approval_matches_check ? `Approved: ${state.approval.version}` : `Approved: ${state.approval.version}`,
          state.approval_matches_check ? "success" : "neutral",
          { dot: true },
        ),
        state.approval.note ? h("span", { class: "dim" }, state.approval.note) : null,
        h("span", { class: "faint" }, `approved ${relative(state.approval.approved_at)}`),
        clear,
      ),
    );
    return out;
  }

  const versionInput = input({
    placeholder: state.check.latest_version || "1.5.0",
    value: state.check.latest_version || "",
    id: "approve-version",
  });
  const noteInput = input({ placeholder: "Why, in a sentence", id: "approve-note" });
  const approve = btn("Record approval", { variant: "primary", size: "sm", icon: "check" });

  approve.addEventListener("click", () => {
    void withBusy(ctx, approve, async () => {
      const version = versionInput.value.trim();
      if (!version) {
        ctx.toast("A version is required", { tone: "danger", detail: "Approving an empty version records nothing." });
        versionInput.focus();
        return;
      }
      try {
        await ctx.api.post("/api/v1/staff/updates/approval", { version, note: noteInput.value.trim() });
        ctx.toast("Approval recorded", {
          tone: "success",
          detail: "It does not install anything. Apply it with the procedure below.",
        });
        reload();
      } catch (err) {
        report(ctx, err, "The approval could not be recorded");
      }
    });
  });

  out.appendChild(
    h(
      "div",
      { class: "form-grid" },
      field({ label: "Approve version", control: versionInput, hint: "Must be newer than the running version." }),
      field({ label: "Note", control: noteInput, hint: "Recorded in the audit log with your name." }),
      h("div", { class: "form-actions" }, approve),
    ),
  );
  out.appendChild(
    h(
      "p",
      { class: "faint" },
      "Approving records a decision. It does not download or run anything: the panel cannot replace its own binary.",
    ),
  );
  return out;
}

// -------------------------------------------------------------------- apply --

function applyCard(state: UpdateState): HTMLElement {
  return card({
    title: "Applying it",
    sub: "A procedure, not a button",
    body: h(
      "div",
      { class: "stack-sm" },
      h("ol", { class: "stack-sm" }, ...state.apply.steps.map((step) => h("li", null, step))),
      h("p", { class: "faint" }, state.apply.note),
      h(
        "p",
        { class: "faint" },
        "This is why the screen offers no install button: an automated path would have to fetch an artifact with " +
          "nothing to verify it against and overwrite the running executable, and a deployment with no supervisor " +
          "would not even restart it.",
      ),
    ),
  });
}

// ---------------------------------------------------------------- telemetry --

/**
 * The schedule row's sentence.
 *
 * "Not scheduled" is a real state and is said plainly: a build whose worker does not run the
 * telemetry job must not claim a daily delivery just because the switch is on.
 */
function scheduleSentence(
  enabled: boolean,
  schedule: UpdateState["telemetry"]["schedule"],
): string {
  if (!enabled) return "not scheduled — telemetry is off";
  if (!schedule) return "not scheduled — no job in this build sends telemetry";
  const hours = schedule.interval_hours;
  const every = hours === 24 ? "daily" : `every ${hours} hours`;
  return `${every}, from the worker (${schedule.job})`;
}

function telemetryCard(ctx: Ctx, state: UpdateState, reload: () => void): HTMLElement {
  const body = h("div", { class: "stack" });
  const telemetry = state.telemetry;

  body.appendChild(
    metaGrid([
      { label: "Telemetry", value: badge(telemetry.enabled ? "On" : "Off", telemetry.enabled ? "success" : "neutral", { dot: true }) },
      { label: "Endpoint", value: telemetry.endpoint_configured ? h("span", { class: "mono" }, telemetry.endpoint_host) : "not configured" },
      { label: "Installation id", value: mono(telemetry.instance_id, "A random identifier, generated by the database. Not derived from any customer.") },
      {
        label: "Last send",
        value: telemetry.last_send
          ? `${titleCase(telemetry.last_send.status)} · ${relative(telemetry.last_send.sent_at)}`
          : "never",
      },
      {
        // The schedule is part of the promise the switch makes (OPS-13). Until the worker ran the
        // job, "on" meant "on when somebody presses send", which is not what an operator agreed
        // to; the sentence now says when, so a missing send is a visible failure rather than an
        // unstated one. The interval is the job's own, read from the server rather than written
        // into this template, so the two cannot drift.
        label: "Schedule",
        value: scheduleSentence(telemetry.enabled, telemetry.schedule),
      },
    ]),
  );

  if (telemetry.enabled && !telemetry.endpoint_configured) {
    body.appendChild(
      banner(
        "warning",
        "Telemetry is on but there is nowhere to send it",
        "The daily job will do nothing until an endpoint is configured below. Nothing is stored and nothing leaves " +
          "the installation in this state.",
      ),
    );
  }

  if (!telemetry.enabled) {
    body.appendChild(
      banner(
        "neutral",
        "Telemetry is off",
        "Nothing is sent. Turning it on sends a fixed set of counts and versions — no customer identifier, " +
          "name or address, no invoice contents, no hostname or public URL.",
      ),
    );
  }

  // The preview is the payload. It is fetched from its own route so the exact bytes that
  // would leave the installation are what an operator reads, not a re-rendering of them.
  const previewHost = h("div", { class: "stack" });
  body.appendChild(sectionTitle("What would be sent"));
  body.appendChild(previewHost);

  swap(
    previewHost,
    resource(
      ctx,
      (signal) => ctx.api.get<TelemetryPreview>("/api/v1/staff/updates/telemetry", { cache: false, signal }),
      (preview) => renderPreview(preview),
      { skeleton: skeletonPage },
    ),
  );

  body.appendChild(telemetryForm(ctx, state, reload));

  return card({
    title: "Telemetry",
    sub: "Off by default. Fixed payload, no customer data.",
    body,
  });
}

function renderPreview(preview: TelemetryPreview): Node {
  if (!preview.enabled) {
    return emptyState({
      title: "Nothing to preview",
      body: "Telemetry is off, so no payload exists. The switch above turns it on; the payload is shown before anything is sent.",
      icon: "eye",
    });
  }
  if (!preview.payload_json) {
    return emptyState({
      title: "The payload could not be built",
      body: "The counts could not be read. Nothing would be sent in this state.",
      icon: "alertCircle",
    });
  }

  const fields = h(
    "div",
    { class: "row-wrap" },
    ...preview.fields.map((name) => h("span", { class: "chip" }, code(name))),
  );

  return h(
    "div",
    { class: "stack-sm" },
    h("p", { class: "faint" }, "These are the exact bytes that would be sent:"),
    h("pre", { class: "code-block" }, preview.payload_json),
    h("p", { class: "faint" }, "Top-level fields:"),
    fields,
    preview.delivery_possible
      ? null
      : h("p", { class: "faint" }, "No endpoint is configured, so this payload cannot be delivered yet."),
  );
}

function telemetryForm(ctx: Ctx, state: UpdateState, reload: () => void): HTMLElement {
  const checkURL = input({
    id: "update-check-url",
    value: state.check.source_url,
    placeholder: "https://updates.example.com/catalyst.json",
  });
  const channel = select(
    [
      { value: "stable", label: "Stable" },
      { value: "beta", label: "Beta" },
    ],
    { id: "update-channel", value: state.channel },
  );
  const endpoint = input({
    id: "telemetry-endpoint",
    value: "",
    placeholder: state.telemetry.endpoint_configured ? "configured — leave empty to keep it" : "https://telemetry.example.com/ingest",
  });
  const toggle = h("input", { type: "checkbox", checked: state.telemetry.enabled, id: "telemetry-enabled" });
  const save = btn("Save", { variant: "primary", size: "sm", icon: "check" });
  const send = btn("Send now", { size: "sm", icon: "send" });
  if (send instanceof HTMLButtonElement) send.disabled = !state.telemetry.enabled;

  save.addEventListener("click", () => {
    void withBusy(ctx, save, async () => {
      // The endpoint is only sent when it was typed: an empty box means "leave it alone",
      // because the value is a credential the server never returns to fill the field with.
      const payload: Record<string, unknown> = {
        check_url: checkURL.value.trim(),
        channel: channel.value,
        telemetry: toggle.checked,
      };
      if (endpoint.value.trim() !== "") payload.telemetry_endpoint = endpoint.value.trim();
      try {
        await ctx.api.put("/api/v1/staff/updates/settings", payload);
        ctx.toast("Saved", { tone: "success" });
        reload();
      } catch (err) {
        report(ctx, err, "The settings could not be saved");
      }
    });
  });

  send.addEventListener("click", () => {
    void withBusy(ctx, send, async () => {
      const ok = await ctx.confirm({
        title: "Send the telemetry payload now?",
        body: "The payload is the one shown above. The attempt is recorded with the digest of the exact bytes sent, whether or not it succeeds.",
        confirmLabel: "Send",
      });
      if (!ok) return;
      try {
        await ctx.api.post("/api/v1/staff/updates/telemetry/send", {});
        ctx.toast("Payload delivered", { tone: "success" });
        reload();
      } catch (err) {
        report(ctx, err, "The payload was not delivered");
      }
    });
  });

  return h(
    "div",
    { class: "stack-sm" },
    sectionTitle("Settings"),
    h(
      "div",
      { class: "form-grid" },
      field({ label: "Update check URL", control: checkURL, hint: "Served as {version, download_url, notes, published_at}. Empty disables the check." }),
      field({ label: "Channel", control: channel, hint: "Recorded with each check; the server does not filter by it." }),
      field({ label: "Telemetry endpoint", control: endpoint, hint: "Write-only: it is a credential and is never returned." }),
      h(
        "label",
        { class: "switch" },
        toggle,
        h("span", null, "Send telemetry"),
      ),
      h("div", { class: "form-actions" }, save, send),
    ),
    h(
      "p",
      { class: "faint" },
      "Telemetry is off unless this switch is on. When it is on, the worker sends the document above once a day — " +
        "counts, versions and a random installation id, and nothing that identifies a customer. Every attempt is " +
        "recorded in the send history above whether it succeeded or not, and \"Send telemetry\" is for a one-off " +
        "delivery; it does not replace the schedule.",
    ),
  );
}

// ------------------------------------------------------------------ helpers --

/** Reports a failed action with the server's own problem code, which is what a client is
 *  meant to branch on. */
function report(ctx: Ctx, err: unknown, fallback: string): void {
  if (err instanceof ApiError) {
    ctx.toast(err.detail || fallback, { tone: "danger", detail: err.code });
    return;
  }
  if (err instanceof DOMException && err.name === "AbortError") return;
  ctx.toast(fallback, { tone: "danger", detail: String(err) });
}
