// The cancellation queue.
//
// A cancellation is a *request* with a reason, a type and an actor, not a button that terminates
// a server. The queue is where staff decide:
//
//  - **Execute** acts on a pending immediate request. It enqueues the existing terminate
//    operation rather than terminating anything itself, so the response is a 202 with an
//    operation id — the service disappears when the worker has spoken to the provider. The page
//    says that rather than claiming "terminated".
//  - **Reject** requires a reason. The server refuses a rejection without one
//    (`reason_required`), because rejecting a customer's request silently turns the queue into a
//    black hole.
//
// A scheduled (end-of-period) request is not executable: it stays until the period ends. So the
// action column is computed from `status` and `type`, and a row that cannot be acted on says why
// instead of offering a button that would fail with `not_pending`.
//
// The endpoint is `GET /api/v1/staff/cancellations`, which answers `{cancellations, statuses}` —
// not the `{data, next_cursor}` page shape — so this screen builds its own table from the same
// `.table` markup and takes the status filter from the server's own list.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateOnly, dateTime, humanStatus, relative } from "../lib/format";
import type { Ctx } from "../lib/types";
import {
  badge,
  banner,
  btn,
  card,
  emptyState,
  field,
  pageHead,
  resource,
  select,
  skeletonTable,
  statusBadge,
  textarea,
  withBusy,
} from "../ui/kit";
import { openDialog } from "../ui/dialog";

export const meta = { title: "Cancellations", crumb: "Cancellations" };

// ---------------------------------------------------------------- payloads --

/** One request, from `cancellationJSON` in internal/httpapi/service_cancellations.go. `customer`
 *  and `product_name` are present only when the store joined them, which the queue does. */
interface Cancellation {
  id: string;
  service_id: string;
  type: string;
  status: string;
  reason: string;
  created_at: string;
  requested_by?: string;
  effective_at?: string;
  executed_at?: string;
  customer?: { id: string; name: string; email: string };
  product_name?: string;
  service_status?: string;
}

interface CancellationList {
  cancellations: Cancellation[] | null;
  statuses: string[] | null;
}

/** The statuses `handleStaffCancellations` accepts. The server sends this list, so it is only a
 *  fallback for a build that does not. */
const FALLBACK_STATUSES = ["pending", "scheduled", "executed", "cancelled", "rejected"];

/** The open queue is "pending and scheduled"; an empty status means exactly that. */
const OPEN = "";

export default async function cancellationsPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("service:read:all")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Cancellations" }),
      banner(
        "danger",
        "You cannot read the cancellation queue",
        "The queue needs the service:read:all permission. Executing a request additionally needs service:terminate.",
        btn("Back to the dashboard", { href: "/", size: "sm", variant: "ghost" }),
      ),
    );
  }

  const canExecute = ctx.can("service:terminate");
  const canReject = ctx.can("service:suspend");
  const status = ctx.route.query.get("status") ?? OPEN;

  const host = h("div");

  /** Loads the queue, or reports that this build does not serve it.
   *
   *  `GET /api/v1/staff/cancellations` is the service-lifecycle package's route. Until it is
   *  merged the request is a 404, which is a fact about the installation rather than a failed
   *  read — retrying produces the same answer, so the screen explains instead of offering one. */
  const load = async (signal: AbortSignal): Promise<CancellationList | "unavailable"> => {
    try {
      return await ctx.api.get<CancellationList>("/api/v1/staff/cancellations", {
        query: { status: status || undefined, limit: 100 },
        cache: false,
        signal,
      });
    } catch (err) {
      if (err instanceof ApiError && (err.status === 404 || err.status === 501)) return "unavailable";
      throw err;
    }
  };

  const paint = (): void => {
    swap(
      host,
      resource(
        ctx,
        load,
        (data) => (data === "unavailable" ? unavailableCard() : render(data)),
        { skeleton: () => h("div", { class: "stack" }, skeletonTable(8, 5)) },
      ),
    );
  };

  // ------------------------------------------------------------- mutations --

  const execute = async (row: Cancellation, button: HTMLButtonElement): Promise<void> => {
    const ok = await ctx.confirm({
      title: `Cancel ${row.product_name || "this service"} now?`,
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "p",
          null,
          "This records the request as executed and queues the terminate operation. The service disappears when the worker has spoken to the provider, not before.",
        ),
        row.reason ? h("p", { class: "field-hint" }, `The customer's reason: “${row.reason}”`) : null,
        row.customer
          ? h("p", { class: "field-hint" }, `Customer: ${row.customer.name || row.customer.email}`)
          : null,
      ),
      confirmLabel: "Cancel the service",
      danger: true,
    });
    if (!ok) return;
    await withBusy(ctx, button, async () => {
      try {
        const res = await ctx.api.post<{ status: string; operation_id?: string; already_terminated?: boolean }>(
          `/api/v1/staff/cancellations/${encodeURIComponent(row.id)}/execute`,
          {},
        );
        if (res?.already_terminated) {
          ctx.toast("The service was already terminated", { tone: "info", detail: "The request is recorded as executed." });
        } else {
          ctx.toast("Cancellation executed", {
            tone: "success",
            detail: res?.operation_id
              ? "The terminate operation is queued. The service goes away when the worker has run it."
              : "The request is recorded as executed.",
          });
        }
        paint();
      } catch (err) {
        report(err, "The cancellation could not be executed");
      }
    });
  };

  const reject = (row: Cancellation): void => {
    const reason = textarea({
      id: "cancel-reject-reason",
      placeholder: "The customer asked to keep the service until the paid period ends.",
      maxlength: 255,
      required: true,
    });
    const errorHost = h("div");
    const form = h(
      "form",
      { class: "stack", novalidate: true },
      errorHost,
      field({
        label: "Reason",
        control: reason,
        id: "cancel-reject-reason",
        required: true,
        hint: "Required. The customer and the next agent both read this, so write it for them.",
      }),
    );
    const submit = btn("Reject request", { variant: "danger-solid", type: "submit" });
    const cancel = btn("Keep it open", { variant: "ghost" });
    const formId = "cancel-reject-form";
    form.id = formId;
    submit.setAttribute("form", formId);

    const handle = openDialog({
      title: `Reject the cancellation of ${row.product_name || "this service"}?`,
      subtitle: "The service keeps running and any schedule the request set is cleared. The customer is not charged again by this action.",
      body: h("div", { class: "stack" }, form),
      footer: h("div", { class: "row spacer" }, cancel, submit),
      locked: () => submit instanceof HTMLButtonElement && submit.disabled,
    });
    cancel.addEventListener("click", () => handle.close());

    form.addEventListener("submit", (event) => {
      event.preventDefault();
      void (async () => {
        errorHost.replaceChildren();
        const text = reason.value.trim();
        if (!text) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "A rejection needs a reason."));
          return;
        }
        if (submit instanceof HTMLButtonElement) submit.disabled = true;
        try {
          await ctx.api.post(`/api/v1/staff/cancellations/${encodeURIComponent(row.id)}/reject`, { reason: text });
          ctx.toast("Request rejected", { tone: "success", detail: "The service keeps running." });
          handle.close();
          paint();
        } catch (err) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, cancellationError(err)));
        } finally {
          if (submit instanceof HTMLButtonElement) submit.disabled = false;
        }
      })();
    });
  };

  const report = (err: unknown, fallback: string): void => {
    if (err instanceof ApiError) {
      ctx.toast(err.detail || fallback, { tone: "danger", detail: err.code });
      return;
    }
    ctx.toast(fallback, { tone: "danger", detail: "The server could not be reached." });
  };

  const unavailableCard = (): HTMLElement =>
    card({
      title: "The cancellation queue is not available in this build",
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "p",
          { class: "field-hint" },
          "This installation does not serve ",
          h("code", { class: "inline-code" }, "GET /api/v1/staff/cancellations"),
          ", so there is no queue to read. Reloading will give the same answer until a build that includes the cancellation surface is deployed.",
        ),
        h(
          "p",
          { class: "field-hint" },
          "A customer's request is still recorded when they file one — the rows are in the database and appear here as soon as a build that can read them is running. Until then, a request that needs a decision should be handled from the service's own screen.",
        ),
        h(
          "div",
          { class: "row" },
          btn("Services", { href: "/services", size: "sm", icon: "server" }),
          btn("Back to the dashboard", { href: "/", size: "sm", variant: "ghost", icon: "home" }),
        ),
      ),
    });

  // ---------------------------------------------------------------- render --

  const render = (data: CancellationList): Node => {
    const rows = data.cancellations ?? [];
    const statuses = data.statuses ?? FALLBACK_STATUSES;
    const pending = rows.filter((row) => row.status === "pending").length;

    const picker = select(
      [
        { value: OPEN, label: "Open queue (pending and scheduled)" },
        ...statuses.map((value) => ({ value, label: humanStatus(value) })),
      ],
      { id: "cancel-status", value: status },
    );
    picker.addEventListener("change", () => {
      ctx.replaceQuery({ status: picker.value || null });
      paint();
    });

    const toolbar = h(
      "div",
      { class: "toolbar" },
      h("div", { class: "log-filters log-filters--inline" }, field({ label: "Show", control: picker, id: "cancel-status" })),
      h(
        "div",
        { class: "toolbar-end" },
        pending > 0 ? badge(`${pending} waiting for a decision`, "warning", { dot: true }) : null,
        btn("Refresh", { size: "sm", variant: "ghost", icon: "refresh", onclick: () => paint() }),
      ),
    );

    if (rows.length === 0) {
      return h(
        "div",
        { class: "stack" },
        toolbar,
        card({
          flush: true,
          body: emptyState({
            title: status === OPEN ? "Nothing waiting" : `No ${humanStatus(status).toLowerCase()} requests`,
            body:
              status === OPEN
                ? "A customer's cancellation request appears here with its reason. An end-of-period request needs no decision until the period ends; an immediate one does."
                : "Nothing in the queue has that status. The open queue shows what still needs a decision.",
            icon: "logout",
            action:
              status === OPEN
                ? btn("See every request", { size: "sm", variant: "ghost", onclick: () => void switchTo("pending") })
                : btn("Back to the open queue", { size: "sm", onclick: () => void switchTo(OPEN) }),
          }),
        }),
      );
    }

    return h(
      "div",
      { class: "stack" },
      toolbar,
      card({
        flush: true,
        body: h(
          "div",
          { class: "table-wrap table-wrap--cards" },
          h(
            "table",
            { class: "table" },
            h(
              "thead",
              null,
              h(
                "tr",
                null,
                h("th", { scope: "col" }, "Service"),
                h("th", { scope: "col" }, "Type"),
                h("th", { scope: "col" }, "Status"),
                h("th", { scope: "col", class: "hide-sm" }, "Requested"),
                h("th", { scope: "col", class: "hide-sm" }, "Reason"),
                h("th", { scope: "col", class: "num" }, ""),
              ),
            ),
            h("tbody", null, ...rows.map((row) => tableRow(row))),
          ),
        ),
      }),
    );
  };

  const switchTo = (next: string): void => {
    ctx.replaceQuery({ status: next || null });
    paint();
  };

  const tableRow = (row: Cancellation): HTMLElement => {
    const actionable = row.status === "pending" && row.type === "immediate";
    const executeBtn = actionable && canExecute
      ? btn("Execute", { size: "sm", variant: "danger", icon: "zap", title: "Queue the terminate operation" }) as HTMLButtonElement
      : null;
    if (executeBtn) executeBtn.addEventListener("click", () => void execute(row, executeBtn));
    const rejectBtn = row.status === "pending" && canReject
      ? btn("Reject", { size: "sm", variant: "ghost", icon: "x", onclick: () => reject(row) })
      : null;

    return h(
      "tr",
      null,
      h(
        "td",
        { class: "cell-primary" },
        h(
          "div",
          { class: "cell-main" },
          h(
            "div",
            { class: "primary" },
            h("a", { class: "row-title", href: `/services/${encodeURIComponent(row.service_id)}` }, row.product_name || "Service"),
          ),
          h(
            "div",
            { class: "secondary" },
            row.customer
              ? h("a", { class: "linkish", href: `/customers/${encodeURIComponent(row.customer.id)}` }, row.customer.name || row.customer.email)
              : h("span", { class: "faint" }, "Unknown customer"),
            row.service_status ? ` · ${humanStatus(row.service_status)}` : "",
          ),
        ),
      ),
      h("td", { "data-label": "Type" }, h("span", { class: "dim" }, row.type === "immediate" ? "Immediate" : "End of period")),
      h(
        "td",
        { "data-label": "Status" },
        h(
          "div",
          { class: "stack-sm" },
          statusBadge(row.status),
          row.effective_at
            ? h("span", { class: "faint nowrap" }, `effective ${dateOnly(row.effective_at)}`)
            : null,
        ),
      ),
      h(
        "td",
        { class: "hide-sm", "data-label": "Requested" },
        h("span", { class: "dim nowrap", title: dateTime(row.created_at) }, relative(row.created_at)),
      ),
      h(
        "td",
        { class: "hide-sm", "data-label": "Reason" },
        row.reason ? h("span", { class: "cancel-reason", title: row.reason }, row.reason) : h("span", { class: "faint" }, "—"),
      ),
      h(
        "td",
        { class: "num", "data-label": "Actions" },
        h(
          "div",
          { class: "row cancel-actions" },
          executeBtn,
          rejectBtn,
          !actionable && row.status === "pending"
            ? h(
                "span",
                { class: "faint nowrap", title: "An end-of-period request needs no decision now: it stays until the period ends." },
                "Ends with the period",
              )
            : null,
          !actionable && row.status === "scheduled"
            ? h("span", { class: "faint nowrap" }, "Scheduled")
            : null,
          row.status === "executed" && row.executed_at
            ? h("span", { class: "faint nowrap", title: dateTime(row.executed_at) }, "Executed")
            : null,
        ),
      ),
    );
  };

  paint();

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Cancellations",
      sub: "Requests from customers to stop a service. An immediate request is a decision; an end-of-period request runs out the period it was paid for.",
      actions: btn("Services", { href: "/services", size: "sm", variant: "ghost", icon: "server" }),
    }),
    banner(
      "info",
      "Executing queues the terminate, it does not do it",
      h(
        "span",
        null,
        "The worker talks to the provider. Until it does, the service still exists — which is why the response carries an operation id and the service screen shows the operation.",
      ),
    ),
    host,
  );
}

// ----------------------------------------------------------------- helpers --

function cancellationError(err: unknown): string {
  if (!(err instanceof ApiError)) return "The server could not be reached, so nothing changed.";
  switch (err.code) {
    case "reason_required":
      return "A rejection needs a reason. Write one and try again.";
    case "not_pending":
      return "That request is no longer pending — someone else has already decided it. Reload the queue.";
    case "already_requested":
      return "A cancellation request is already open for this service.";
    case "not_cancellable":
      return err.detail || "This service cannot be cancelled in its current state.";
    case "not_found":
      return "That request or service no longer exists. Reload the queue.";
    case "forbidden":
      return "You do not hold the permission this action needs.";
    default:
      return err.detail || `The request could not be decided (${err.code}).`;
  }
}
