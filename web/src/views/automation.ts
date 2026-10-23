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
//
// The third section is the run history *per job* — how often each has failed, when it last
// succeeded, and a retry (OPS-09, OPS-13). A list of intentions plus a heartbeat still cannot
// answer "which job is broken", and `cron_runs` could: it is written by the worker and by the retry
// itself, so the two are one history. The retry can only run the jobs this process can construct
// (the lifecycle rules and the maintenance rules); a job that lives in the worker answers
// `job_not_retryable` rather than disappearing from the button list, because "you cannot do this
// from the panel" is information an operator should have before they go looking.

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

/** One job's aggregate, from `handleCronStatistics` in internal/httpapi/cron_failures.go. */
interface JobStatistics {
  job: string;
  runs: number;
  failures: number;
  running: number;
  skipped: number;
  avg_duration_ms: number;
  max_duration_ms: number;
  last_run_at: string | null;
  last_success_at: string | null;
  last_failure_at: string | null;
  last_outcome: string;
  last_detail: string;
}

interface StatisticsResponse {
  window_days: number;
  since: string;
  data: JobStatistics[];
  note: string;
}

/** One failed run, from the same file's failure list. */
interface FailedRun {
  id: string;
  job: string;
  started_at: string;
  finished_at: string | null;
  outcome: string;
  detail: string;
  items: number;
  duration_ms: number | null;
}

/** What the unattended charger would take right now, from `handleChargeDuePlan` in
 *  internal/httpapi/charge_due_plan.go. `enabled` is the setting, `kill_switch_set` is the
 *  process-level switch, and `reason` is set when the sweep would do nothing. */
interface ChargePlan {
  enabled: boolean;
  kill_switch_set: boolean;
  reason?: string;
  provider?: string;
  retry_window_hours: number;
  batch: number;
  candidates: ChargePlanRow[];
  skipped: ChargePlanRow[];
  without_method: number;
  truncated: boolean;
  totals: Array<{ currency: string; amount: string; amount_minor: number; invoices: number }>;
  generated_at: string;
}

interface ChargePlanRow {
  invoice_id: string;
  invoice_number: string;
  customer_name: string;
  currency: string;
  amount: string;
  amount_minor: number;
  due_at: string;
  method_brand?: string;
  method_last4?: string;
  chargeable: boolean;
  skip_reason?: string;
  attempt_count: number;
  last_attempt?: { status: string; at: string } | null;
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
        btn("Back to the dashboard", { href: "/dashboard", size: "sm", variant: "ghost" }),
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
    let statistics: StatisticsResponse | null = null;
    let failures: FailedRun[] = [];
    let chargePlan: ChargePlan | null = null;
    // The plan names the cards about to be charged, so it needs the permission to charge rather
    // than the permission to read logs. Asking without it is a 403 the screen already expects.
    if (ctx.can("payment:reconcile")) {
      try {
        chargePlan = await ctx.api.get<ChargePlan>("/api/v1/staff/payments/charge-due", {
          cache: false,
          signal,
        });
      } catch (err) {
        if (!(err instanceof ApiError) && !(err instanceof DOMException && err.name === "AbortError")) {
          throw err;
        }
      }
    }
    if (canReadLogs) {
      try {
        heartbeat = await ctx.api.get<HeartbeatResponse>("/api/v1/staff/cron/heartbeat", {
          cache: false,
          signal,
        });
        statistics = await ctx.api.get<StatisticsResponse>("/api/v1/staff/cron/statistics?days=7", {
          cache: false,
          signal,
        });
        const page = await ctx.api.get<{ data: FailedRun[] }>("/api/v1/staff/cron/failures?limit=10", {
          cache: false,
          signal,
        });
        failures = page.data ?? [];
      } catch (err) {
        if (err instanceof ApiError && (err.status === 403 || err.status === 404)) {
          heartbeatDenied = true;
        } else if (!(err instanceof DOMException && err.name === "AbortError")) {
          throw err;
        }
      }
    }
    return { body, heartbeat, heartbeatDenied, canReadLogs, statistics, failures, chargePlan };
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
      render(ctx, data.body, data.heartbeat, data.heartbeatDenied, data.canReadLogs,
        data.statistics, data.failures, data.chargePlan, () => paint()),
    );
  }
}

// ------------------------------------------------------------------ render --

function render(
  ctx: Ctx,
  body: AutomationResponse,
  heartbeat: HeartbeatResponse | null,
  heartbeatDenied: boolean,
  canReadLogs: boolean,
  statistics: StatisticsResponse | null,
  failures: FailedRun[],
  chargePlan: ChargePlan | null,
  reload: () => void,
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
    chargePlanCard(chargePlan, body.data.find((job) => job.name === "charge-agreements")),
    runsCard(ctx, statistics, failures, reload),
    jobsCard(body.data),
  );
}

/** runsCard reports each job's recent history and offers a retry.
 *
 *  Two lists, because they answer two questions: the aggregate says "this job fails every tick", and
 *  the rows say what the last failures actually said. A screen with only the aggregate sends an
 *  operator to the logs; a screen with only the rows hides the pattern.
 *
 *  The failures are rendered even when nothing has failed: "no failures in seven days" is the answer
 *  to the question, and an empty card that says nothing would look like a screen that did not
 *  load. */
function runsCard(
  ctx: Ctx,
  statistics: StatisticsResponse | null,
  failures: FailedRun[],
  reload: () => void,
): HTMLElement {
  if (statistics === null) {
    return card({
      title: "Recent runs",
      body: emptyState({
        title: "The run history is not visible to you",
        body: "Reading it needs log:read. The job list below is complete; only the history is withheld.",
        icon: "clock",
      }),
    });
  }

  const failing = statistics.data.filter((row) => row.failures > 0 || row.running > 0);

  return card({
    title: "Recent runs",
    sub: `last ${statistics.window_days} days · ${statistics.data.length} job${statistics.data.length === 1 ? "" : "s"} with history`,
    body: h(
      "div",
      { class: "stack-sm" },
      failing.length === 0
        ? h(
            "p",
            { class: "field-hint" },
            "No job has failed or is still running inside the window. A job that has not run at all is not listed here — " +
              "the table below says which jobs are registered.",
          )
        : h(
            "div",
            { class: "stack-xs" },
            ...failing.map((row) =>
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
                      h("strong", null, row.job),
                      row.running > 0
                        ? badge("running", "info", { dot: true })
                        : badge(`${row.failures} failed`, "danger", { dot: true }),
                      row.last_success_at
                        ? h("span", { class: "field-hint faint" }, `last success ${relative(row.last_success_at)}`)
                        : h("span", { class: "field-hint faint" }, "no successful run in the window"),
                    ),
                    h(
                      "p",
                      { class: "faint" },
                      `${row.runs} run${row.runs === 1 ? "" : "s"} · avg ${row.avg_duration_ms}ms · max ${row.max_duration_ms}ms` +
                        (row.skipped > 0 ? ` · ${row.skipped} skipped` : ""),
                    ),
                    row.last_detail ? h("p", { class: "faint" }, row.last_detail) : null,
                  ),
                  h("div", { class: "spacer" }),
                  retryButton(ctx, row.job, reload),
                ),
              ),
            ),
          ),
      failures.length > 0
        ? h(
            "div",
            { class: "stack-xs" },
            h("span", { class: "field-label" }, "Failures"),
            ...failures.map((run) =>
              h(
                "div",
                { class: "row-wrap" },
                h(
                  "div",
                  { class: "cell-main" },
                  h("div", { class: "primary" }, run.job),
                  h("div", { class: "secondary" }, run.detail || "no detail was recorded"),
                ),
                h(
                  "span",
                  { class: "field-hint faint nowrap" },
                  `${dateTime(run.started_at)} · ${run.duration_ms ?? 0}ms`,
                ),
              ),
            ),
          )
        : null,
    ),
  });
}

/** chargePlanCard shows what the unattended charger would take right now.
 *
 *  It is on this screen because this is where an operator decides what to switch off. Enabling a job
 *  that charges stored cards with nobody watching is a decision that needs its consequence visible
 *  next to the switch, and "the sweep charges what is due" is not something a person can check. */
function chargePlanCard(plan: ChargePlan | null, job: AutomationJob | undefined): HTMLElement {
  if (plan === null) {
    return card({
      title: "Next charge run",
      body: h(
        "p",
        { class: "field-hint" },
        "Reading the plan needs payment:reconcile, because it names the customers and cards the run " +
          "would use. The job list below is complete.",
      ),
    });
  }

  // The reason is the sentence an operator needs first, and each one has a different fix.
  const state = !plan.enabled
    ? { tone: "info" as const, title: "Off-session charging is switched off",
        body: "gateway.off_session_enabled is off, so the run would charge nothing. The figures below are what it " +
          "would take if it were on." }
    : plan.kill_switch_set
      ? { tone: "warning" as const, title: "The process kill switch is set",
          body: `${job?.kill_switch ?? "The kill switch"} is set in the worker's environment, so the job is not scheduled. ` +
            "The figures below are what it would take if the switch were cleared." }
      : plan.reason === "not_configured"
        ? { tone: "warning" as const, title: "No gateway is configured",
            body: "A saved card cannot be charged without a gateway, so the run would do nothing." }
        : null;

  const rows = plan.candidates.length === 0
    ? emptyState({
        title: "Nothing would be charged",
        body: plan.skipped.length === 0
          ? "No invoice is due with a saved card and an active mandate. This is the healthy state."
          : `${plan.skipped.length} invoice${plan.skipped.length === 1 ? " is" : "s are"} due but left alone — the list below says why.`,
        icon: "check",
      })
    : h(
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
              h("th", { scope: "col" }, "Invoice"),
              h("th", { scope: "col" }, "Customer"),
              h("th", { scope: "col" }, "Card"),
              h("th", { scope: "col" }, "Amount"),
              h("th", { scope: "col" }, "Overdue"),
            ),
          ),
          h(
            "tbody",
            null,
            ...plan.candidates.map((row) =>
              h(
                "tr",
                null,
                h("td", { class: "cell-primary" }, h("span", { class: "mono" }, row.invoice_number)),
                h("td", { "data-label": "Customer" }, row.customer_name),
                h(
                  "td",
                  { "data-label": "Card" },
                  row.method_last4 ? `${row.method_brand || "card"} ···· ${row.method_last4}` : "saved method",
                ),
                h("td", { "data-label": "Amount" }, h("span", { class: "money" }, row.amount)),
                h("td", { "data-label": "Overdue" }, relative(row.due_at)),
              ),
            ),
          ),
        ),
      );

  return card({
    title: "Next charge run",
    sub: plan.provider
      ? `${plan.provider} · retry window ${plan.retry_window_hours}h · up to ${plan.batch} invoices per run`
      : `retry window ${plan.retry_window_hours}h · up to ${plan.batch} invoices per run`,
    actions: badge(
      plan.candidates.length === 0 ? "nothing to charge" : `${plan.candidates.length} to charge`,
      plan.candidates.length === 0 ? "neutral" : "warning",
      { dot: true },
    ),
    body: h(
      "div",
      { class: "stack-sm" },
      state ? banner(state.tone, state.title, state.body) : null,
      plan.totals.length > 0
        ? h(
            "div",
            { class: "row-wrap" },
            ...plan.totals.map((total) =>
              h(
                "div",
                { class: "stack-xs" },
                h("span", { class: "field-label" }, total.currency),
                h("span", { class: "money money--lg" }, total.amount),
                h("span", { class: "field-hint faint" }, `${total.invoices} invoice${total.invoices === 1 ? "" : "s"}`),
              ),
            ),
          )
        : null,
      plan.truncated
        ? h(
            "p",
            { class: "field-hint" },
            `This is the next run only: the batch stops at ${plan.batch}, so more invoices are waiting behind it.`,
          )
        : null,
      rows,
      plan.skipped.length > 0 ? skippedList(plan.skipped) : null,
      plan.without_method > 0
        ? h(
            "p",
            { class: "field-hint" },
            `${plan.without_method} due invoice${plan.without_method === 1 ? "" : "s"} cannot be charged at all: the ` +
              "customer has no saved card. Those go to dunning like any other unpaid invoice.",
          )
        : null,
      h(
        "p",
        { class: "field-hint" },
        "A preview, not a promise: the run reads the invoice, the card and the mandate again when it charges, and a " +
          "decline goes to dunning rather than to a suspension.",
      ),
    ),
  });
}

/** skippedList says why a due invoice is left alone. It is the half of the answer that makes the
 *  other half trustworthy: a plan that silently omitted an invoice would look like a plan that had
 *  nothing to do. */
function skippedList(rows: ChargePlanRow[]): HTMLElement {
  const why = (row: ChargePlanRow): string => {
    switch (row.skip_reason) {
      case "retry_window":
        return row.last_attempt
          ? `attempted ${relative(row.last_attempt.at)} and ${row.last_attempt.status}`
          : "attempted recently";
      case "checkout_open":
        return "the customer has a checkout open";
      case "no_mandate":
        return "the saved card has no active mandate";
      default:
        return row.skip_reason || "skipped";
    }
  };
  return h(
    "details",
    { class: "log-detail-disclosure" },
    h("summary", null, `${rows.length} due invoice${rows.length === 1 ? "" : "s"} left alone`),
    h(
      "div",
      { class: "stack-xs log-detail" },
      ...rows.slice(0, 20).map((row) =>
        h(
          "div",
          { class: "row-between" },
          h(
            "span",
            null,
            h("span", { class: "mono" }, row.invoice_number),
            h("span", { class: "faint" }, ` · ${row.customer_name}`),
          ),
          h("span", { class: "field-hint faint" }, `${row.amount} · ${why(row)}`),
        ),
      ),
      rows.length > 20
        ? h("p", { class: "field-hint faint" }, `…and ${rows.length - 20} more.`)
        : null,
    ),
  );
}

/** retryButton runs one job now.
 *
 *  It exists for the jobs this process can build. The others answer `job_not_retryable` with a
 *  sentence naming the worker; the button is offered anyway so the refusal is explained rather than
 *  the control silently missing. */
function retryButton(ctx: Ctx, job: string, reload: () => void): HTMLElement {
  const button = btn("Run now", { size: "sm", icon: "refresh" });
  button.addEventListener("click", () => {
    void (async () => {
      if (button instanceof HTMLButtonElement) button.disabled = true;
      try {
        const result = await ctx.api.post<{ outcome: string; detail: string; duration_ms: number }>(
          `/api/v1/staff/cron/${encodeURIComponent(job)}/retry`,
          {},
        );
        ctx.toast(
          result.outcome === "ok" ? `${job} ran` : `${job} failed`,
          {
            tone: result.outcome === "ok" ? "success" : "danger",
            detail: result.detail || `${result.duration_ms}ms`,
          },
        );
        reload();
      } catch (err) {
        if (err instanceof ApiError) {
          // `job_not_retryable` is not a failure of the screen: it says where the job lives.
          ctx.toast(err.detail || err.code, { tone: err.status === 422 ? "info" : "danger", detail: err.code });
        } else if (!(err instanceof DOMException && err.name === "AbortError")) {
          ctx.toast("The job could not be run", { tone: "danger", detail: String(err) });
        }
      } finally {
        if (button instanceof HTMLButtonElement) button.disabled = false;
      }
    })();
  });
  return button;
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
      // A job row carries five cells — name, period, purpose, kill switch and state — which is
      // more than a phone can show side by side. `table-wrap--cards` rebuilds the row as a card
      // below 780px (tables.css), and each cell keeps its column name through `data-label`.
      // Without it the table scrolls sideways and the copy button beside the kill switch sits
      // outside the viewport, where a phone cannot reach it.
      "div",
      { class: "table-wrap table-wrap--cards" },
      h(
        "table",
        { class: "table jobs-table" },
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
        // The Go source is a path, and a path has no spaces in it: without a break opportunity at
        // the slashes it is cut mid-word ("jobs.g" / "o") in the narrow job column.
        h("div", { class: "secondary faint mono" }, ...wrappablePath(job.source)),
      ),
    ),
    h(
      "td",
      { "data-label": "Every", title: `${job.interval_seconds} seconds` },
      h("span", { class: "mono nowrap" }, job.interval),
    ),
    h(
      "td",
      // The purpose of a job is a sentence, and in card mode a table cell puts its value beside
      // the column label — a sentence beside a label is a column two words wide. `cell-block`
      // stacks the label above the value (pages.css) so the sentence gets the whole card.
      { class: "cell-block", "data-label": "What it does" },
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
      { "data-label": "Kill switch" },
      job.kill_switch
        ? h(
            "div",
            { class: "cell-main" },
            h(
              "div",
              { class: "row" },
              // The variable name is one unbreakable token and the longest is 30 characters, so
              // as a single token it widens this column until the State column no longer fits
              // the card at 1024. It may wrap — but at its underscores, not mid-word, so the
              // pieces still read as the name (see `wrappableVariable`).
              h("span", { class: "mono kill-switch" }, ...wrappableVariable(job.kill_switch)),
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
      { "data-label": "State" },
      job.disabled
        ? badge("Switched off", "danger", { dot: true })
        : badge("Running", "success", { dot: true }),
    ),
  );
}

/** wrappablePath splits a file path at its slashes and puts a `<wbr>` in each gap, so a long
 *  source path (`internal/lifecycle/lifecycle.go`) breaks at a slash instead of mid-word when the
 *  job column is narrow. The text is unchanged; only break opportunities are added. */
function wrappablePath(path: string): Node[] {
  const parts = path.split("/");
  const nodes: Node[] = [];
  parts.forEach((part, index) => {
    const last = index === parts.length - 1;
    nodes.push(document.createTextNode(last ? part : `${part}/`));
    if (!last) nodes.push(h("wbr"));
  });
  return nodes;
}

/** wrappableVariable splits an environment variable name at its underscores and puts a `<wbr>`
 *  in each gap, so the name can take a second line between words instead of running out of a
 *  narrow column. The name is unchanged for a reader (and for a copy: it is text, not markup), and
 *  a name short enough to fit is still on one line.
 *
 *  The underscores stay attached to the piece before them, so a break reads as a complete word
 *  (`BILLING_DISABLE_` / `PAYMENTS`) rather than a fragment. */
function wrappableVariable(variable: string): Node[] {
  const parts = variable.split("_");
  const nodes: Node[] = [];
  parts.forEach((part, index) => {
    const last = index === parts.length - 1;
    nodes.push(document.createTextNode(last ? part : `${part}_`));
    if (!last) nodes.push(h("wbr"));
  });
  return nodes;
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
