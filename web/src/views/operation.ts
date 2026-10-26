// One operation, in detail.
//
// This is the status URL the API returns from a 202 (`/api/v1/staff/operations/{id}`), and
// it answers the only question an operator has after a long action: did it land? The
// timeline is the answer, the operation key is what a retry reuses, and the lease sentence
// is what tells them whether to wait for the reaper or intervene.

import { h, swap } from "../lib/dom";
import { dateTime, humanStatus, relative, shortId } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  banner,
  btn,
  card,
  copyButton,
  emptyState,
  errorState,
  kv,
  mono,
  pageHead,
  skeletonPage,
  statusBadge,
} from "../ui/kit";
import {
  deleteOperation,
  hasCapabilities,
  operationCapabilities,
  resolveOperation,
  retryOperation,
} from "./operations";

export const meta = { title: "Operation", crumb: "Operation" };

/** How often the page re-reads a queued or running operation. A worker ticks every few
 *  seconds, so a tighter poll would only spend requests on an answer that cannot change. */
const REFRESH_MS = 5000;

/** `provisioning.DefaultLeaseDuration`. The server also sends `lease_expires_at`; this is
 *  the fallback for a payload that does not carry it. */
const LEASE_MINUTES = 5;

const PENDING = new Set(["queued", "running"]);

type TimelineTone = "neutral" | "success" | "danger" | "warning" | "accent";

/** The wire shape. The list endpoint nests the service; the detail endpoint returns
 *  `service_id` and `customer` at the top level. Both are read here so the page does not
 *  break on either, and neither field is invented when it is absent. */
interface OperationPayload {
  id: string;
  service_id?: string;
  service?: { id: string; customer?: { id: string; email: string } | null } | null;
  customer?: { id: string; email: string } | null;
  kind: string;
  operation_key: string;
  generation: number;
  status: string;
  attempts: number;
  last_error?: string | null;
  created_at: string;
  started_at?: string | null;
  finished_at?: string | null;
  lease_expires_at?: string | null;
}

interface Operation {
  id: string;
  serviceId: string;
  customerId: string;
  customerEmail: string;
  kind: string;
  operationKey: string;
  generation: number;
  status: string;
  attempts: number;
  lastError: string;
  createdAt: string;
  startedAt: string | null;
  finishedAt: string | null;
  leaseExpiresAt: string | null;
}

export default async function operationPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("service:read:all")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Operation" }),
      card({
        body: emptyState({
          title: "Not available for your role",
          body: "Reading an operation needs the service:read:all permission.",
          icon: "lock",
        }),
      }),
    );
  }

  const id = ctx.route.params.id ?? "";
  // `hidden` while empty: a zero-height child still costs a flex gap in `.page`.
  const notice = h("div", { hidden: true });
  const body = h("div", { class: "stack" }, skeletonPage());

  const controller = new AbortController();
  ctx.onDispose(() => controller.abort());

  let timer: number | undefined;
  const stopTimer = () => {
    if (timer !== undefined) {
      window.clearInterval(timer);
      timer = undefined;
    }
  };
  ctx.onDispose(stopTimer);

  const schedule = (op: Operation) => {
    stopTimer();
    if (!PENDING.has(op.status)) return;
    timer = window.setInterval(() => void load(true), REFRESH_MS);
  };

  let inFlight = false;
  const load = async (background = false): Promise<void> => {
    // A slow response must not let a second poll stack on top of the first.
    if (inFlight) return;
    inFlight = true;
    if (!background) swap(body, skeletonPage());
    try {
      const raw = await ctx.api.get<OperationPayload>(
        `/api/v1/staff/operations/${encodeURIComponent(id)}`,
        { cache: false, signal: controller.signal },
      );
      if (controller.signal.aborted) return;
      const op = normalise(raw);
      notice.hidden = true;
      swap(notice, h("div"));
      render(ctx, op, () => void load(true), body);
      schedule(op);
    } catch (err) {
      if (controller.signal.aborted || (err instanceof DOMException && err.name === "AbortError")) return;
      stopTimer();
      if (background) {
        // Keep the last good render: the operator is reading it, and replacing it with an
        // error would hide the state they came for. Say that refreshing stopped instead.
        notice.hidden = false;
        swap(
          notice,
          banner(
            "warning",
            "Auto-refresh stopped",
            "The last poll failed, so this view may be stale.",
            btn("Try again", { size: "sm", icon: "refresh", onclick: () => void load(true) }),
          ),
        );
        return;
      }
      swap(body, errorState(err, () => void load()));
    } finally {
      inFlight = false;
    }
  };

  void load();
  return h("div", { class: "page" }, notice, body);
}

// ------------------------------------------------------------------ rendering --

function render(ctx: Ctx, op: Operation, refresh: () => void, host: HTMLElement): void {
  ctx.setTitle(`Operation ${shortId(op.id)}`);
  ctx.setCrumbs([
    { label: "Queue", href: "/operations" },
    { label: `${humanStatus(op.kind)} · ${shortId(op.id)}` },
  ]);

  const pending = PENDING.has(op.status);
  const caps = operationCapabilities(ctx, op);

  // ADM-18: the three things an operator can do to a row the worker gave up on. Each one is
  // offered only when the server would accept it: the status has to be one the action accepts,
  // and the session has to hold the permission the route demands. A button that always
  // answers 403 or 409 is worse than no button.
  const runRetry = () => {
    void (async () => {
      if (await retryOperation(ctx, op)) refresh();
    })();
  };
  const runResolve = () => {
    void (async () => {
      if (await resolveOperation(ctx, op)) refresh();
    })();
  };
  const runDelete = () => {
    void (async () => {
      // The row is gone after a delete, so this page has nothing left to show: the queue is the
      // only honest destination, and going back there beats re-reading a 404.
      if (await deleteOperation(ctx, op)) ctx.navigate("/operations");
    })();
  };

  const head = pageHead({
    title: `${humanStatus(op.kind)} operation`,
    badge: statusBadge(op.status),
    sub: [
      op.customerEmail ? `for ${op.customerEmail}` : null,
      `generation ${op.generation}`,
      `${op.attempts} attempt${op.attempts === 1 ? "" : "s"}`,
    ]
      .filter(Boolean)
      .join(" · "),
    actions: h(
      "div",
      { class: "row-wrap" },
      pending ? h("span", { class: "queue-live" }, icon("activity", 12), `Auto-refresh every ${REFRESH_MS / 1000}s`) : null,
      caps.retry
        ? btn("Retry", {
            size: "sm",
            icon: "rotate",
            title: "Requeue this operation under the same key with a fresh attempt budget",
            onclick: runRetry,
          })
        : null,
      caps.resolve
        ? btn("Resolve", {
            size: "sm",
            icon: "checkCircle",
            title: "Record that you settled the remote state by hand",
            onclick: runResolve,
          })
        : null,
      caps.delete
        ? btn("Delete", {
            size: "sm",
            variant: "danger",
            icon: "trash",
            title: "Remove this failed operation from the queue",
            onclick: runDelete,
          })
        : null,
      btn("Refresh", { size: "sm", icon: "refresh", onclick: refresh }),
    ),
  });

  const timeline = card({
    title: "Lifecycle",
    sub: "created → started → finished",
    body: h(
      "div",
      { class: "timeline" },
      timelineItem("Created", op.createdAt, "accent", `Enqueued under generation ${op.generation}.`),
      timelineItem(
        "Started",
        op.startedAt,
        op.startedAt ? "success" : "neutral",
        op.startedAt
          ? "A worker claimed the operation and took a lease."
          : "No worker has claimed this operation yet.",
        "Not started",
      ),
      timelineItem(
        "Finished",
        op.finishedAt,
        finishedTone(op.status),
        op.finishedAt
          ? `Closed as ${humanStatus(op.status).toLowerCase()} after ${op.attempts} attempt${op.attempts === 1 ? "" : "s"}.`
          : pending
            ? "Still in flight; this page refreshes until it settles."
            : `No finish time recorded, but the status is ${humanStatus(op.status).toLowerCase()}.`,
        "Not finished",
      ),
    ),
  });

  const errorCard = op.lastError
    ? card({
        title: "Last error",
        sub: "What the worker recorded, verbatim",
        body: h("pre", { class: "code-block error-code-block" }, op.lastError),
      })
    : null;

  const facts = card({
    title: "Operation",
    body: h(
      "div",
      { class: "stack-sm" },
      kv("Status", statusBadge(op.status)),
      kv("Kind", humanStatus(op.kind)),
      kv("Generation", mono(op.generation)),
      kv("Attempts", mono(op.attempts)),
      kv("Created", h("span", { class: "dim", title: dateTime(op.createdAt) }, relative(op.createdAt))),
      kv("Started", op.startedAt ? h("span", { class: "dim", title: dateTime(op.startedAt) }, relative(op.startedAt)) : h("span", { class: "faint" }, "—")),
      kv("Finished", op.finishedAt ? h("span", { class: "dim", title: dateTime(op.finishedAt) }, relative(op.finishedAt)) : h("span", { class: "faint" }, "—")),
      h(
        "div",
        { class: "op-key" },
        h("span", { class: "kv-key" }, "Operation key"),
        h("div", { class: "op-key-value" }, mono(op.operationKey), copyButton(ctx, op.operationKey, "key")),
      ),
    ),
  });

  const lease = card({
    title: "Worker lease",
    body: h("p", { class: "lease-sentence" }, leaseSentence(op)),
  });

  const links = card({
    body: h(
      "div",
      { class: "stack-sm" },
      op.serviceId
        ? btn("Open the service", { href: `/services/${op.serviceId}`, size: "sm", icon: "server", block: true })
        : h("span", { class: "faint" }, "The payload did not include the service id."),
      op.customerId
        ? btn("Open the customer", { href: `/customers/${op.customerId}`, size: "sm", icon: "user", block: true })
        : null,
    ),
  });

  // The actions card is the explanation, not a second set of buttons: when nothing is on
  // offer, the page says *why* — a role that lacks the permission reads a different sentence
  // from an operator looking at a row that is still queued, and both are different from a
  // succeeded row, which needs neither.
  const actions = hasCapabilities(caps)
    ? card({
        title: "Operator actions",
        sub: "What you can do about this operation",
        body: h(
          "div",
          { class: "stack-sm" },
          h(
            "p",
            { class: "lease-sentence" },
            op.status === "failed" || op.status === "unknown" || op.status === "needs_review"
              ? "A retry queues the provider call again with a fresh attempt budget. A resolve records that you settled the remote state by hand, and is what an uncertain outcome needs before it can leave the queue."
              : "This operation is settled, so there is nothing left to retry or resolve.",
          ),
          caps.delete
            ? h(
                "p",
                { class: "lease-sentence" },
                "Deleting removes the row. It does not undo anything the worker already did, so a failure over a real remote change should be resolved rather than deleted.",
              )
            : null,
        ),
      })
    : card({
        title: "Operator actions",
        body: h(
          "p",
          { class: "lease-sentence" },
          PENDING.has(op.status)
            ? "The worker still has this operation. Retrying or resolving it while it is queued or running would queue the same work twice, so wait for it to settle — the page refreshes itself until it does."
            : op.status === "succeeded"
              ? "This operation succeeded, so there is nothing to retry, resolve or delete."
              : "Your role cannot retry or resolve operations. A retry needs service:provision; resolving and deleting need service:write:all.",
        ),
      });

  swap(
    host,
    h(
      "div",
      { class: "stack" },
      head,
      h(
        "div",
        { class: "split" },
        h("div", { class: "stack" }, timeline, ...(errorCard ? [errorCard] : [])),
        h("div", { class: "stack" }, facts, lease, actions, links),
      ),
    ),
  );
}

function timelineItem(
  title: string,
  at: string | null,
  tone: TimelineTone,
  detail: string,
  missing = "Not yet",
): HTMLElement {
  return h(
    "div",
    { class: "tl-item" },
    h(
      "span",
      { class: `tl-dot${tone !== "neutral" ? ` tl-dot--${tone}` : ""}` },
      at ? icon("check", 9) : icon("circle", 9),
    ),
    h(
      "div",
      { class: "tl-body" },
      h(
        "div",
        { class: "tl-title" },
        title,
        h("span", { class: "tl-time" }, at ? dateTime(at) : missing),
      ),
      h("div", { class: "tl-detail" }, at ? `${detail} (${relative(at)})` : detail),
    ),
  );
}

function finishedTone(status: string): TimelineTone {
  switch (status) {
    case "succeeded":
      return "success";
    case "failed":
      return "danger";
    case "unknown":
    case "needs_review":
    case "retry_wait":
      return "warning";
    default:
      return "neutral";
  }
}

function leaseSentence(op: Operation): string {
  if (op.status !== "running") {
    return `No lease is held: the worker only holds one while an operation is running, and this one is ${humanStatus(
      op.status,
    ).toLowerCase()}.`;
  }
  const expiry = leaseExpiry(op);
  if (!expiry) {
    return "This operation is running but has no start time, so the lease cannot be computed. Check the worker's clock and the operation row.";
  }
  const minutes = Math.ceil((expiry.getTime() - Date.now()) / 60_000);
  if (minutes <= 0) {
    return `The ${LEASE_MINUTES}-minute lease expired ${relative(expiry)} (${dateTime(
      expiry.toISOString(),
    )}). The reaper returns this operation to the queue under the same key on its next pass, so do not retry it by hand.`;
  }
  return `The worker holds a ${LEASE_MINUTES}-minute lease that expires in ${minutes} minute${
    minutes === 1 ? "" : "s"
  } (${dateTime(expiry.toISOString())}). If the worker dies before then, the reaper returns this operation to the queue under the same key.`;
}

function leaseExpiry(op: Operation): Date | null {
  if (op.leaseExpiresAt) {
    const at = new Date(op.leaseExpiresAt);
    if (!Number.isNaN(at.getTime())) return at;
  }
  if (!op.startedAt) return null;
  const started = new Date(op.startedAt);
  if (Number.isNaN(started.getTime())) return null;
  return new Date(started.getTime() + LEASE_MINUTES * 60_000);
}

function normalise(raw: OperationPayload): Operation {
  const nested = raw.service;
  return {
    id: raw.id,
    serviceId: raw.service_id ?? nested?.id ?? "",
    customerId: raw.customer?.id ?? nested?.customer?.id ?? "",
    customerEmail: raw.customer?.email ?? nested?.customer?.email ?? "",
    kind: raw.kind,
    operationKey: raw.operation_key,
    generation: raw.generation,
    status: raw.status,
    attempts: raw.attempts,
    lastError: (raw.last_error ?? "").trim(),
    createdAt: raw.created_at,
    startedAt: raw.started_at ?? null,
    finishedAt: raw.finished_at ?? null,
    leaseExpiresAt: raw.lease_expires_at ?? null,
  };
}
