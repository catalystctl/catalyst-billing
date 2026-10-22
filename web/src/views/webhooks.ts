// The webhook inbox.
//
// A gateway event is never dropped: `gateway_events` stores the payload, and a worker
// reprocesses anything that failed. That is why this screen is an inbox and not an error
// log — an unprocessed row is a queue position, not a lost payment. The banner says so,
// because an operator who assumes a red row means lost money will reconcile by hand.

import { h, swap } from "../lib/dom";
import { dateTime, relative, truncate } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  banner,
  btn,
  card,
  emptyState,
  field,
  input,
  mono,
  pageHead,
  select,
  switchField,
} from "../ui/kit";
import { createTable, type Column } from "../ui/table";

export const meta = { title: "Webhooks", crumb: "Webhooks" };

interface WebhookRow {
  id: string;
  provider: string;
  provider_account: string;
  event_id: string;
  event_type: string;
  received_at: string;
  processed_at: string | null;
  attempts: number;
  last_error: string | null;
}

interface Filters {
  /** "true" | "false" | "" — the API's `processed` parameter, omitted for "any". */
  processed: string;
  provider: string;
}

export default async function webhooksPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("payment:reconcile")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Webhook inbox" }),
      card({
        body: emptyState({
          title: "Not available for your role",
          body: "The webhook inbox needs the payment:reconcile permission.",
          icon: "lock",
        }),
      }),
    );
  }

  let filters = filtersFrom(ctx.route.query);
  const bannerHost = h("div", { hidden: true });
  const tableHost = h("div");

  const empty: { title: string; body?: string; action?: Node } = { title: "", body: "" };

  const columns: Column<WebhookRow>[] = [
    {
      key: "event",
      label: "Event",
      primary: true,
      render: (row) =>
        h(
          "div",
          { class: "cell-main" },
          h("div", { class: "primary" }, h("span", { class: "mono" }, row.event_type)),
          h(
            "div",
            { class: "secondary" },
            icon("zap", 10),
            h("span", { class: "mono" }, row.event_id),
          ),
        ),
    },
    {
      key: "provider",
      label: "Provider",
      render: (row) =>
        h(
          "div",
          { class: "cell-main" },
          h("div", { class: "primary" }, row.provider || "—"),
          row.provider_account ? h("div", { class: "secondary" }, h("span", { class: "mono" }, row.provider_account)) : null,
        ),
    },
    {
      key: "received",
      label: "Received",
      align: "right",
      render: (row) => h("span", { class: "dim nowrap", title: dateTime(row.received_at) }, relative(row.received_at)),
    },
    {
      key: "processed",
      label: "Processed",
      align: "right",
      render: (row) =>
        row.processed_at
          ? h("span", { class: "dim nowrap", title: dateTime(row.processed_at) }, relative(row.processed_at))
          : h("span", { class: "faint nowrap" }, "Not processed"),
    },
    {
      key: "attempts",
      label: "Attempts",
      align: "right",
      render: (row) =>
        row.attempts > 0 ? h("span", { class: "queue-attempts" }, mono(row.attempts)) : mono(row.attempts),
    },
    {
      key: "last_error",
      label: "Last error",
      hideSmall: true,
      render: (row) =>
        row.last_error
          ? h("span", { class: "error-text", title: row.last_error }, truncate(row.last_error, 80))
          : h("span", { class: "faint" }, "—"),
    },
  ];

  const table = createTable<WebhookRow>(ctx, {
    path: "/api/v1/staff/webhooks",
    columns,
    query: queryOf(filters),
    limit: 50,
    empty,
    rowKey: (row) => row.id,
    onLoaded: (rows) => renderBanner(rows),
  });
  tableHost.appendChild(table.el);

  const stateSelect = select(
    [
      { value: "", label: "Any state" },
      { value: "false", label: "Unprocessed" },
      { value: "true", label: "Processed" },
    ],
    { id: "webhook-state", value: filters.processed, onchange: (event) => setFilters({ ...filters, processed: (event.target as HTMLSelectElement).value }) },
  );

  const onlyUnprocessed = switchField("Only unprocessed", {
    checked: filters.processed === "false",
    onchange: (event) => setFilters({ ...filters, processed: (event.target as HTMLInputElement).checked ? "false" : "" }),
  });
  const onlyUnprocessedBox = onlyUnprocessed.querySelector("input");

  const providerInput = input({
    id: "webhook-provider",
    type: "search",
    value: filters.provider,
    placeholder: "Provider…",
    onchange: (event) => setFilters({ ...filters, provider: (event.target as HTMLInputElement).value.trim() }),
  });

  const clearButton = btn("Clear filters", {
    size: "sm",
    variant: "ghost",
    icon: "x",
    onclick: () => setFilters({ processed: "", provider: "" }),
  });

  function setFilters(next: Filters): void {
    filters = next;
    stateSelect.value = next.processed;
    if (onlyUnprocessedBox instanceof HTMLInputElement) onlyUnprocessedBox.checked = next.processed === "false";
    providerInput.value = next.provider;
    clearButton.hidden = !hasFilters(next);
    ctx.replaceQuery({
      processed: next.processed || null,
      provider: next.provider || null,
    });
    describeEmpty();
    void table.reload(queryOf(next));
  }

  /** The banner is about the rows on screen, not a global count: the API returns a page,
   *  and claiming "12 events are stuck" from one page would be a guess. */
  function renderBanner(rows: WebhookRow[]): void {
    const needing = rows.filter((row) => row.attempts > 0 || (row.last_error ?? "") !== "");
    // `hidden` rather than an empty element: a zero-height child still costs a flex gap.
    bannerHost.hidden = needing.length === 0;
    if (needing.length === 0) {
      swap(bannerHost, h("div"));
      return;
    }
    swap(
      bannerHost,
      banner(
        "info",
        needing.length === 1
          ? "One event on this page was retried or failed"
          : `${needing.length} events on this page were retried or failed`,
        "Nothing is lost. Every delivery is stored in gateway_events, and the worker reprocesses anything unprocessed on its next pass — so fix the cause and let it retry rather than reconciling by hand.",
      ),
    );
  }

  function describeEmpty(): void {
    if (hasFilters(filters)) {
      empty.title = "Nothing matches these filters";
      empty.body = "No stored event matches this state and provider. Widen the filters to see the rest of the inbox.";
      empty.action = btn("Clear filters", {
        size: "sm",
        icon: "x",
        onclick: () => setFilters({ processed: "", provider: "" }),
      });
      return;
    }
    empty.title = "No webhook events yet";
    empty.body =
      "Nothing has arrived from a payment provider. Events appear here the moment the gateway delivers them, processed or not.";
    empty.action = undefined;
  }

  describeEmpty();
  clearButton.hidden = !hasFilters(filters);

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Webhook inbox",
      sub: "Every event the payment providers delivered, newest first. An unprocessed row is waiting for the worker, not lost.",
      actions: btn("Refresh", {
        icon: "refresh",
        size: "sm",
        onclick: () => void table.reload(queryOf(filters)),
      }),
    }),
    bannerHost,
    card({
      body: h(
        "div",
        { class: "queue-filters" },
        field({ label: "State", control: stateSelect }),
        field({ label: "Provider", control: providerInput }),
        h("div", { class: "queue-filters-toggle" }, onlyUnprocessed),
        h("div", { class: "queue-filters-end" }, clearButton),
      ),
    }),
    tableHost,
  );
}

// ------------------------------------------------------------------ helpers --

function filtersFrom(query: URLSearchParams): Filters {
  const processed = query.get("processed") ?? "";
  return {
    processed: processed === "true" || processed === "false" ? processed : "",
    provider: query.get("provider") ?? "",
  };
}

function queryOf(filters: Filters): Record<string, string | undefined> {
  return {
    processed: filters.processed || undefined,
    provider: filters.provider || undefined,
  };
}

function hasFilters(filters: Filters): boolean {
  return Boolean(filters.processed || filters.provider);
}
