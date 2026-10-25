// The live game-server card: what the customer's server is doing, where to reach it, and the buttons
// that start it.
//
// # Why this is a shared renderer rather than two cards
//
// The customer's service page and the staff service page answer the same question ("is it running,
// and where do I connect"), and a support agent should see exactly what the customer sees. Two
// implementations would drift, and the drift would show up as a support conversation about a screen
// the agent cannot reproduce.
//
// # What it does not do
//
// It renders no figure it was not given. A provider that cannot measure CPU says nothing, and the
// card shows a dash: "0%" and "not measured" are different statements and only one of them is true.
// It also never offers an action the provider did not list in `can_power` — the adapter knows its own
// state machine, and a button that exists only to produce a refusal is worse than an absent one.

import { ApiError } from "../lib/api";
import { h } from "../lib/dom";
import { number } from "../lib/format";
import type { Ctx } from "../lib/types";
import { banner, btn, card, metaGrid, statusBadge, withBusy } from "../ui/kit";

/** The fields the endpoint returns, from `writeServerView`.
 *
 *  Every one is optional except the flags, because "the panel could not be reached" is a normal
 *  answer: it is not a 5xx, it is a card that explains itself. */
export interface ServerView {
  service_id: string;
  service_status: string;
  /** False for a service with no resource yet — a pending order, or a product with no server. */
  provisioned: boolean;
  /** True only when the adapter answered with live data. */
  available: boolean;
  /** A sentence explaining why the card has no data. */
  detail?: string;
  /** The machine-readable failure class: not_configured, unsupported, not_recognised, timeout,
   *  unreachable. It is what makes a failure diagnosable — "does not recognise this service" is true both
   *  for a deleted server and for an installation whose provider was changed, and only one of those is the
   *  customer's problem. */
  reason?: string;
  provider?: string;
  extension?: string;
  name?: string;
  /** The provider's own state word: running, stopped, starting, installing, … */
  state?: string;
  status?: string;
  address?: string;
  host?: string;
  port?: number;
  panel_url?: string;
  plan_id?: string;
  region?: string;
  /** The actions the provider would accept right now. */
  can_power?: string[];
  observed_at?: string;
  stats?: ServerStats;
}

/** The metrics block. Absent fields were not measured. */
export interface ServerStats {
  cpu_percent?: number;
  memory_used_mb?: number;
  memory_limit_mb?: number;
  disk_used_mb?: number;
  disk_limit_mb?: number;
  network_rx_bytes?: number;
  network_tx_bytes?: number;
  observed_at?: string;
}

/** One power action's presentation. */
interface PowerButton {
  action: string;
  label: string;
  icon: "playCircle" | "pauseCircle" | "rotate" | "zap";
  variant: "default" | "primary" | "danger";
  /** The question asked before the call; empty means no confirmation. */
  confirm: string;
}

// POWER_BUTTONS is the whole vocabulary, in the order a page offers it.
//
// `kill` is the only destructive one, so it is the only one behind a confirmation that says what will
// be lost: force-stopping a server can lose everything since its last save, and a customer who was
// not told that will reasonably be upset when it happens.
const POWER_BUTTONS: PowerButton[] = [
  { action: "start", label: "Start", icon: "playCircle", variant: "primary", confirm: "" },
  { action: "stop", label: "Stop", icon: "pauseCircle", variant: "default", confirm: "" },
  {
    action: "restart",
    label: "Restart",
    icon: "rotate",
    variant: "default",
    confirm: "Restart this server? Players are disconnected and it comes back up in a moment.",
  },
  {
    action: "kill",
    label: "Force stop",
    icon: "zap",
    variant: "danger",
    confirm:
      "Force stop this server? This cuts the power without saving, so anything since the last save is lost. " +
      "Use it only when a normal stop does not work.",
  },
];

/** How often a visible card refreshes itself. */
const POLL_MS = 15_000;

export interface ServerCardOptions {
  /** The service whose server to show. */
  serviceId: string;
  /** The base path for the read, e.g. "/api/v1/portal/services". */
  basePath: string;
  /** Whether the viewer may press the power buttons. */
  canPower: boolean;
  /** Extra prose for a service that has no server yet, when the page has context to add. */
  pendingNote?: string;
}

/**
 * mountServerCard renders the card and keeps it current.
 *
 * It returns the element immediately and fills it asynchronously, so the service page never waits on
 * the panel to draw. Cleanup is registered on the page context: a navigation that left a 15-second
 * timer running would keep calling the panel from a page nobody is looking at.
 */
export function mountServerCard(ctx: Ctx, opts: ServerCardOptions): HTMLElement {
  const host = h("div", { class: "stack" });
  let view: ServerView | null = null;
  let failure: string | null = null;
  let loading = true;
  let timer: number | null = null;
  let disposed = false;

  const url = `${opts.basePath}/${encodeURIComponent(opts.serviceId)}/server`;

  const stopPolling = (): void => {
    if (timer !== null) {
      window.clearInterval(timer);
      timer = null;
    }
  };

  const startPolling = (): void => {
    stopPolling();
    if (disposed) return;
    timer = window.setInterval(() => {
      // A hidden tab does not need a 15-second poll, and a background poll against a remote panel is
      // the kind of quiet load that adds up across every open session.
      if (document.hidden) return;
      void load({ quiet: true });
    }, POLL_MS);
  };

  ctx.onDispose(() => {
    disposed = true;
    stopPolling();
  });

  const load = async (loadOpts: { quiet?: boolean } = {}): Promise<void> => {
    if (!loadOpts.quiet) {
      loading = true;
      paint();
    }
    try {
      view = await ctx.api.get<ServerView>(url, { cache: false });
      failure = null;
    } catch (err) {
      failure = problemSentence(err);
      view = null;
    }
    loading = false;
    paint();
  };

  const power = async (button: PowerButton, element: HTMLButtonElement): Promise<void> => {
    if (button.confirm) {
      const ok = await ctx.confirm({
        title: `${button.label} this server?`,
        body: button.confirm,
        confirmLabel: button.label,
        danger: button.variant === "danger",
      });
      if (!ok) return;
    }
    await withBusy(ctx, element, async () => {
      try {
        await ctx.api.post(`${url}/power`, { action: button.action });
        ctx.toast(`${button.label} requested`, { tone: "success" });
      } catch (err) {
        ctx.toast(`Could not ${button.label.toLowerCase()} the server`, {
          tone: "danger",
          detail: problemSentence(err),
        });
      }
      // Re-read either way: a refused action may still have changed the provider's state, and the
      // buttons a page offers must follow what the provider says rather than what was hoped for.
      await load({ quiet: true });
    });
  };

  const paint = (): void => {
    if (disposed) return;
    host.replaceChildren(...(loading && !view ? [card({ title: "Server", body: skeleton() })] : [render()]));
  };

  const render = (): HTMLElement => {
    if (failure) {
      return card({
        title: "Server",
        body: banner("warning", "The server panel could not be reached", failure),
        actions: btn("Try again", {
          size: "sm",
          icon: "refresh",
          onclick: () => void load(),
        }),
      });
    }
    if (!view) {
      return card({ title: "Server", body: skeleton() });
    }
    if (!view.provisioned) {
      return card({
        title: "Server",
        body: h(
          "p",
          { class: "page-sub" },
          opts.pendingNote ??
            view.detail ??
            "This service has no server yet. It appears here once it has been provisioned.",
        ),
      });
    }
    if (!view.available) {
      // The integration is named here on purpose. A changed or misconfigured provider is invisible from a
      // customer-facing sentence alone, and it makes *every* service report the same failure at once. This
      // line is what turns "contact support" into a diagnosis: "Integration: fake" when the install is
      // supposed to be on catalystctl.
      const integration = view.provider
        ? h("p", { class: "page-sub", style: "margin: 0" }, `Integration: ${view.provider}`)
        : null;
      const hint =
        view.reason === "panel_misconfigured"
          ? "This is a problem with this installation's configuration, not with your server. Support can " +
            "fix it from the servers integration screen."
          : view.reason === "not_recognised"
          ? "This can mean the server was removed, or that this installation's server integration has " +
            "changed. Support can tell which from the integration named above."
          : null;
      return card({
        title: "Server",
        body: h(
          "div",
          { class: "stack-sm" },
          h("p", { class: "page-sub" }, view.detail ?? "The server's live state is not available right now."),
          integration,
          hint ? h("p", { class: "page-sub", style: "margin: 0" }, hint) : null,
          h("div", { class: "row-wrap" }, btn("Ask support", { href: "/tickets/new", size: "sm", icon: "message" })),
        ),
        actions: btn("Retry", { size: "sm", icon: "refresh", onclick: () => void load() }),
      });
    }

    return card({
      title: "Server",
      sub: view.provider ? `via ${view.provider}` : undefined,
      actions: h(
        "div",
        { class: "row-wrap" },
        view.panel_url
          ? btn("Open in panel", {
              href: view.panel_url,
              size: "sm",
              icon: "external",
              title: "Open this server's console in the game panel",
              // The panel is a different origin with its own session. Opening it in a new tab keeps
              // the billing screen the customer was reading.
              target: "_blank",
              rel: "noopener noreferrer",
            })
          : null,
        btn("Refresh", { size: "sm", icon: "refresh", onclick: () => void load() }),
      ),
      body: h(
        "div",
        { class: "stack" },
        heading(view),
        addressRow(view),
        statsGrid(view.stats),
        powerRow(view),
        freshness(view),
      ),
    });
  };

  /** The server's name, its state, and a pill. The name comes from the panel on every read, never
   *  from a copy kept here: a server renamed in the panel must not show its old name forever. */
  const heading = (v: ServerView): HTMLElement =>
    h(
      "div",
      { class: "row-wrap", style: "justify-content: space-between; align-items: baseline; gap: 12px" },
      h(
        "div",
        { class: "stack-sm" },
        h("h3", { style: "margin: 0" }, v.name || "Your server"),
        h("span", { class: "page-sub" }, stateSentence(v)),
      ),
      statusBadge(v.state || v.status || null, { dot: true, label: stateLabel(v) }),
    );

  /** The address, with a copy button. This is the line a customer actually needs. */
  const addressRow = (v: ServerView): HTMLElement => {
    if (!v.address) {
      return h("p", { class: "page-sub" }, "The panel has not assigned an address yet.");
    }
    const copy = btn("Copy", {
      size: "sm",
      icon: "copy",
      onclick: () => {
        void navigator.clipboard?.writeText(v.address ?? "").then(
          () => ctx.toast("Address copied", { tone: "success" }),
          () => ctx.toast("Could not copy the address", { tone: "danger" }),
        );
      },
    });
    return h(
      "div",
      { class: "row-wrap", style: "align-items: center; gap: 8px" },
      h("span", { class: "meta-label" }, "Address"),
      h("code", { class: "mono", style: "font-size: 1.05em" }, v.address),
      copy,
    );
  };

  /** Metrics. A missing figure is a dash, never a zero. */
  const statsGrid = (stats: ServerStats | undefined): HTMLElement => {
    if (!stats) {
      return h("p", { class: "page-sub" }, "No performance figures yet. A stopped server reports none.");
    }
    return metaGrid([
      { label: "CPU", value: stats.cpu_percent === undefined ? "—" : `${stats.cpu_percent.toFixed(1)}%` },
      { label: "Memory", value: pair(stats.memory_used_mb, stats.memory_limit_mb, " MB") },
      { label: "Disk", value: pair(stats.disk_used_mb, stats.disk_limit_mb, " MB") },
      {
        label: "Network",
        value:
          stats.network_rx_bytes === undefined && stats.network_tx_bytes === undefined
            ? "—"
            : `${bytes(stats.network_rx_bytes ?? 0)} in · ${bytes(stats.network_tx_bytes ?? 0)} out`,
      },
    ]);
  };

  /** The buttons the provider says it would accept. */
  const powerRow = (v: ServerView): Node => {
    const offered = new Set(v.can_power ?? []);
    const buttons = POWER_BUTTONS.filter((b) => offered.has(b.action));
    if (!opts.canPower || buttons.length === 0) {
      return h("p", { class: "page-sub" }, powerSentence(v, opts.canPower));
    }
    return h(
      "div",
      { class: "row-wrap" },
      ...buttons.map((b) => {
        const element = btn(b.label, {
          variant: b.variant,
          size: "sm",
          icon: b.icon,
          onclick: () => undefined,
        });
        if (element instanceof HTMLButtonElement) {
          element.addEventListener("click", () => void power(b, element));
        }
        return element;
      }),
    );
  };

  /** How old the figures are. A dashboard that does not say is a dashboard people trust wrongly. */
  const freshness = (v: ServerView): Node | null => {
    const stamp = v.stats?.observed_at ?? v.observed_at;
    if (!stamp) return null;
    const at = new Date(stamp);
    if (Number.isNaN(at.getTime())) return null;
    return h(
      "p",
      { class: "page-sub", style: "margin: 0" },
      `Figures from ${at.toLocaleString()} · refreshed every 15 seconds`,
    );
  };

  void load();
  startPolling();
  return host;
}

/** "used / limit" with an em dash for anything not measured. */
function pair(used: number | undefined, limit: number | undefined, unit: string): string {
  if (used === undefined) return "—";
  if (limit === undefined) return `${number(used)}${unit}`;
  return `${number(used)} / ${number(limit)}${unit}`;
}

/** Bytes as a short human figure. */
function bytes(value: number): string {
  if (!value) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let size = value;
  let unit = 0;
  while (size >= 1024 && unit < units.length - 1) {
    size /= 1024;
    unit++;
  }
  return `${size >= 10 || unit === 0 ? Math.round(size) : size.toFixed(1)} ${units[unit]}`;
}

/** The provider's state word as a label a customer recognises. */
function stateLabel(v: ServerView): string {
  switch ((v.state ?? "").toLowerCase()) {
    case "running":
      return "Running";
    case "stopped":
    case "offline":
    case "exited":
      return "Stopped";
    case "starting":
      return "Starting";
    case "stopping":
      return "Stopping";
    case "restarting":
      return "Restarting";
    case "installing":
      return "Installing";
    case "unhealthy":
      return "Not responding";
    case "crashed":
      return "Crashed";
    case "suspended":
      return "Suspended";
    default:
      return v.state ?? v.status ?? "Unknown";
  }
}

/** One sentence describing what the state means for the customer. */
function stateSentence(v: ServerView): string {
  switch ((v.state ?? "").toLowerCase()) {
    case "running":
      return "Your server is online and accepting players.";
    case "stopped":
    case "offline":
    case "exited":
      return "Your server is stopped. Start it to play.";
    case "starting":
      return "Your server is starting up. This usually takes under a minute.";
    case "stopping":
      return "Your server is shutting down.";
    case "restarting":
      return "Your server is restarting.";
    case "installing":
      return "Your server is installing. It becomes available when this finishes.";
    case "unhealthy":
      return "Your server is running but not responding to its health check. A restart usually fixes this.";
    case "crashed":
      return "Your server stopped unexpectedly. Start it again; if it keeps crashing, contact support.";
    case "suspended":
      return "Your server is suspended, so it cannot be started from here.";
    default:
      return "Waiting for the panel to report a state.";
  }
}

/** Why the buttons are not offered, when they are not. */
function powerSentence(v: ServerView, canPower: boolean): string {
  if (!canPower) {
    return "You do not have permission to control this server from here.";
  }
  if (v.state === "suspended") {
    return "A suspended server cannot be started. Contact support to restore it.";
  }
  return "This server is changing state, so the power controls are unavailable for a moment.";
}

/** A loading placeholder sized like the card it replaces. */
function skeleton(): HTMLElement {
  return h(
    "div",
    { class: "stack-sm", "aria-busy": "true" },
    h("div", { class: "skeleton skeleton-line", style: "width: 40%" }),
    h("div", { class: "skeleton skeleton-line", style: "width: 60%" }),
    h("div", { class: "skeleton skeleton-line", style: "width: 75%" }),
  );
}

/** A failure sentence that never leaks the provider's internals. */
function problemSentence(err: unknown): string {
  if (!(err instanceof ApiError)) {
    return "The server panel could not be reached. Your service is unaffected; try again in a moment.";
  }
  switch (err.code) {
    case "server_not_found":
      return "The server panel does not recognise this service. Please contact support.";
    case "power_timeout":
      return "The server panel did not answer in time. Your service is unaffected; try again in a moment.";
    case "power_rejected":
      return err.detail || "The server panel refused that action.";
    case "server_not_found":
      return "The server panel does not recognise this service. Please contact support.";
    case "provider_not_configured":
      return "Server control is not set up yet. Please contact support.";
    case "panel_misconfigured":
      return "This installation's server panel address is misconfigured, so live server details cannot be read.";
    case "power_unsupported":
      return "This provider does not support power control from the billing panel.";
    case "not_provisioned":
      return "This service has no server yet.";
    case "not_found":
      return "This service is not available.";
    case "rate_limited":
      return "Too many attempts. Wait a moment and try again.";
    default:
      return err.detail || "The request could not be completed. Your service is unaffected.";
  }
}
