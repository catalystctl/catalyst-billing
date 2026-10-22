// Automation.
//
// The question this screen answers is the one an operator asks at 03:00 when something is
// cancelling orders: **what runs on this box, how often, and which switch stops it**. The job
// list comes from the code that registers the jobs (the server reads `lifecycle.Jobs()` and the
// worker's own registration), so it cannot describe a job that no longer exists — and a new job
// cannot be missing from it.
//
// Two things it deliberately does *not* do:
//
//  - **It offers no controls.** A kill switch is an environment variable read when the worker
//    starts. A toggle here would change this request's process and nothing else, so the screen
//    shows the variable's name and its state and says plainly that stopping a job is a restart.
//    The server's own note says the same thing, and it is rendered rather than paraphrased.
//  - **It does not hide a switched-off job.** "What runs on this box" has to include what has been
//    switched off, or a stopped job is indistinguishable from a job that was never there.
//
// The worker's heartbeat is read from the cron surface when the operator holds log:read. It is
// the other half of the same question: a job list with no heartbeat is a list of intentions.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateTime, relative } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { badge, banner, btn, card, emptyState, pageHead, resource } from "../ui/kit";

export const meta = { title: "Automation", crumb: "Automation" };

// ---------------------------------------------------------------- payloads --

/** One periodic job, from `handleAutomationJobs` in internal/httpapi/cron_ext.go. */
interface AutomationJob {
  name: string;
  interval_seconds: number;
  interval: string;
  summary: string;
  disabled: boolean;
  kill_switch: string | null;
  kill_switch_set: boolean;
  switch_effect?: string;
  settings?: string[];
  source: string;
}

interface AutomationResponse {
  data: AutomationJob[];
  kill_switches: { mechanism: string; read_only: boolean; note: string };
  worker: { embedded_switch: string; embedded: boolean };
}

/** The worker heartbeat, from `handleHeartbeat` in internal/httpapi/cron.go. A worker that has
 *  never run has `present: false` rather than a 404. */
interface HeartbeatResponse {
  present: boolean;
  stale: boolean;
  stale_after_seconds: number;
  detail?: string;
  heartbeat?: {
    id: string;
    beat_at: string;
    age_seconds: number;
    jobs: number;
    version: string;
    stale: boolean;
    stale_after_seconds: number;
  } | null;
}

export default async function automationPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("settings:write")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Automation" }),
      banner(
        "danger",
        "You cannot see the automation list",
        "This screen needs the settings:write permission: it is the screen an operator uses to decide what to switch off, and that is a configuration decision.",
        btn("Back to the dashboard", { href: "/", size: "sm", variant: "ghost" }),
      ),
    );
  }

  const canReadLogs = ctx.can("log:read");

  // The host exists before the first paint, so Reload swaps the body rather than remounting the
  // page: the header and its button keep their identity.
  const host = h("div");
  const paint = () => swap(host, pageBody());

  const load = async (signal: AbortSignal) => {
    const body = await ctx.api.get<AutomationResponse>("/api/v1/staff/automation", {
      cache: false,
      signal,
    });
    // The heartbeat is a second, optional read: it needs log:read, and its absence is not a
    // failure of this screen. A 403 is an answer ("this operator cannot see the worker's state"),
    // not an error to render.
    let heartbeat: HeartbeatResponse | null = null;
    let heartbeatDenied = false;
    if (canReadLogs) {
      try {
        heartbeat = await ctx.api.get<HeartbeatResponse>("/api/v1/staff/cron/heartbeat", {
          cache: false,
          signal,
        });
      } catch (err) {
        if (err instanceof ApiError && (err.status === 403 || err.status === 404)) {
          heartbeatDenied = true;
        } else if (!(err instanceof DOMException && err.name === "AbortError")) {
          throw err;
        }
      }
    }
    return { body, heartbeat, heartbeatDenied, canReadLogs };
  };

  paint();

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Automation",
      sub: "Every periodic job this installation runs, how often, and the environment variable that stops it. Read from the code that registers them.",
      actions: btn("Reload", { icon: "refresh", size: "sm", onclick: () => paint() }),
    }),
    host,
  );

  function pageBody(): Node {
    return resource(ctx, load, (data) =>
      render(data.body, data.heartbeat, data.heartbeatDenied, data.canReadLogs),
    );
  }
}

// ------------------------------------------------------------------ render --

function render(
  body: AutomationResponse,
  heartbeat: HeartbeatResponse | null,
  heartbeatDenied: boolean,
  canReadLogs: boolean,
): Node {
  if (body.data.length === 0) {
    return card({
      body: emptyState({
        title: "No periodic jobs are registered",
        body: "This build registers no jobs, or the worker is not configured. Nothing on this installation runs on a timer.",
        icon: "clock",
      }),
    });
  }

  const switchedOff = body.data.filter((job) => job.disabled).length;

  return h(
    "div",
    { class: "stack" },
    banner(
      "info",
      "Kill switches are environment variables",
      body.kill_switches.note ||
        "A kill switch is an environment variable read when the worker starts. Changing one is a restart, not a click, so this screen reports their state and offers no control.",
    ),
    switchedOff > 0
      ? banner(
          "warning",
          `${switchedOff} of ${body.data.length} jobs are switched off`,
          "A switched-off job does nothing at all: it does not fall back to a default threshold, and it records no run. The rows below say which switch did it.",
        )
      : null,
    heartbeatCard(heartbeat, heartbeatDenied, canReadLogs, body),
    jobsCard(body.data),
  );
}

/** heartbeatCard reports the worker's own liveness.
 *
 *  A job list answers "what should run"; the heartbeat answers "is anything running". They are
 *  different questions and a screen that showed only the first would let a dead worker look
 *  healthy. */
function heartbeatCard(
  heartbeat: HeartbeatResponse | null,
  denied: boolean,
  canReadLogs: boolean,
  body: AutomationResponse,
): HTMLElement {
  if (!canReadLogs) {
    return card({
      title: "Worker",
      body: h(
        "p",
        { class: "field-hint" },
        "Reading the worker's heartbeat needs the log:read permission. The job list above is the configuration; the heartbeat is the evidence it is running.",
      ),
    });
  }
  if (denied || !heartbeat) {
    return card({
      title: "Worker",
      body: h("p", { class: "field-hint" }, "The worker's heartbeat could not be read on this installation."),
    });
  }
  if (!heartbeat.present || !heartbeat.heartbeat) {
    return card({
      title: "Worker",
      actions: badge("Never reported", "danger", { dot: true }),
      body: h(
        "p",
        { class: "field-hint" },
        "No worker has written a heartbeat. Either none is running, or it has never started against this database — nothing on this screen is actually being executed.",
      ),
    });
  }

  const hb = heartbeat.heartbeat;
  const state = heartbeat.stale
    ? badge("Stale", "warning", { dot: true })
    : badge("Live", "success", { dot: true });

  return card({
    title: "Worker",
    actions: state,
    body: h(
      "div",
      { class: "stack-sm" },
      h(
        "div",
        { class: "meta-grid" },
        h("div", { class: "meta" }, h("span", { class: "meta-label" }, "Last reported"), h("span", { class: "meta-value", title: dateTime(hb.beat_at) }, relative(hb.beat_at))),
        h("div", { class: "meta" }, h("span", { class: "meta-label" }, "Jobs it is running"), h("span", { class: "meta-value mono" }, String(hb.jobs))),
        h("div", { class: "meta" }, h("span", { class: "meta-label" }, "Version"), h("span", { class: "meta-value mono" }, hb.version || "—")),
      ),
      heartbeat.stale
        ? h(
            "p",
            { class: "field-hint" },
            `The last beat is older than ${heartbeat.stale_after_seconds} seconds, so the worker is dead or blocked. A heartbeat row that exists is not the same as a worker that is alive.`,
          )
        : null,
      h(
        "p",
        { class: "field-hint" },
        body.worker.embedded
          ? `The worker runs inside the web process (${body.worker.embedded_switch} is set), so it stops when the panel stops.`
          : `The worker runs as its own process. ${body.worker.embedded_switch} is not set.`,
      ),
    ),
  });
}

/** jobsCard is the list itself: name, period, purpose, switch and state. */
function jobsCard(jobs: AutomationJob[]): HTMLElement {
  return card({
    title: "Periodic jobs",
    sub: `${jobs.length} registered`,
    flush: true,
    body: h(
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
            h("th", { scope: "col" }, "Job"),
            h("th", { scope: "col" }, "Every"),
            h("th", { scope: "col" }, "What it does"),
            h("th", { scope: "col" }, "Kill switch"),
            h("th", { scope: "col" }, "State"),
          ),
        ),
        h("tbody", null, ...jobs.map((job) => jobRow(job))),
      ),
    ),
  });
}

function jobRow(job: AutomationJob): HTMLElement {
  return h(
    "tr",
    null,
    h(
      "td",
      { class: "cell-primary" },
      h(
        "div",
        { class: "cell-main" },
        h("div", { class: "primary mono" }, job.name),
        h("div", { class: "secondary faint" }, job.source),
      ),
    ),
    h(
      "td",
      { title: `${job.interval_seconds} seconds` },
      h("span", { class: "mono nowrap" }, job.interval),
    ),
    h(
      "td",
      null,
      h("span", null, job.summary),
      job.settings && job.settings.length > 0
        ? h(
            "div",
            { class: "row-wrap" },
            ...job.settings.map((key) => h("span", { class: "chip mono" }, key)),
          )
        : null,
      job.switch_effect ? h("div", { class: "field-hint" }, `The switch ${job.switch_effect}.`) : null,
    ),
    h(
      "td",
      null,
      job.kill_switch
        ? h(
            "div",
            { class: "cell-main" },
            h(
              "div",
              { class: "row" },
              h("span", { class: "mono" }, job.kill_switch),
              copyButtonText(job.kill_switch),
            ),
            h(
              "div",
              { class: "secondary" },
              job.kill_switch_set ? "set" : "not set",
            ),
          )
        : h("span", { class: "faint nowrap" }, "none"),
    ),
    h(
      "td",
      null,
      job.disabled
        ? badge("Switched off", "danger", { dot: true })
        : badge("Running", "success", { dot: true }),
    ),
  );
}

/** copyButtonText copies the environment assignment an operator would paste.
 *
 *  The value is what actually stops the job: `=1` is one of the three spellings the worker
 *  accepts, and handing over the whole line means the operator does not have to remember which.
 *  It is a plain button rather than the kit's `copyButton` because that one is styled for a code
 *  block and this sits inside a table cell. */
function copyButtonText(variable: string): HTMLElement {
  const line = `${variable}=1`;
  return h(
    "button",
    {
      class: "copy",
      type: "button",
      title: `Copy ${line}`,
      "aria-label": `Copy ${line}`,
      onclick: async (event: MouseEvent) => {
        event.stopPropagation();
        try {
          await navigator.clipboard.writeText(line);
          const button = event.currentTarget as HTMLElement;
          button.classList.add("is-copied");
          window.setTimeout(() => button.classList.remove("is-copied"), 1200);
        } catch {
          // A clipboard that is unavailable is not worth an error dialog: the variable is on
          // screen and can be typed.
        }
      },
    },
    icon("copy", 11),
  );
}
