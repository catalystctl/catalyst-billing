// The data table.
//
// Every list in the panel is this component: customers, invoices, services, payments, the
// operation queue, the audit log. That is not just reuse — it is how pagination, loading,
// empty and error states behave the same everywhere, and how a keyset cursor cannot be
// mishandled on one screen and handled correctly on the next.
//
// Keyset pagination has no page numbers, because the server does not have them: a cursor is
// "the row after this one". The component therefore keeps a stack of cursors it has visited,
// which gives Back without pretending the data set is stable — and a row inserted between
// two pages is neither skipped nor duplicated, which offset pagination cannot promise.

import type { Page } from "../lib/api";
import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import type { Child } from "../lib/dom";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { emptyState, errorState, skeletonTable } from "./kit";

export interface Column<T> {
  key: string;
  label: string;
  align?: "left" | "right";
  /** A CSS width, e.g. "1%" for a compact action column. */
  width?: string;
  /** The cell that becomes the card title on a narrow screen. */
  primary?: boolean;
  /** Extra class on every cell in the column. */
  className?: string;
  /** Hide the column below 780px (it is still available in the card's other cells). */
  hideSmall?: boolean;
  /**
   * Hide the column between 780px and 1100px, where the card is too narrow for every column
   * but too wide for the card layout. Without it the table keeps its full width and its last
   * column is pushed out of sight with nothing to say it is there.
   */
  hideMedium?: boolean;
  render: (row: T) => Child;
}

export interface TableOptions<T> {
  /** The API path, without the query. */
  path: string;
  columns: Column<T>[];
  /** Extra query parameters sent with every page request. */
  query?: Record<string, string | undefined>;
  empty: { title: string; body?: string; action?: Child };
  rowHref?: (row: T) => string | null;
  rowKey?: (row: T) => string;
  /** A note in the footer, e.g. "Showing 25 per page". */
  footNote?: Child;
  limit?: number;
  /** Called with each page of rows, for a page that keeps a count or a summary. */
  onLoaded?: (rows: T[], page: Page<T>) => void;
  /** Extra class on the table element, for a screen that needs its own column widths. */
  class?: string;
}

export interface Table<T> {
  el: HTMLElement;
  reload(query?: Record<string, string | undefined>): Promise<void>;
  rows(): T[];
  setLoading(on: boolean): void;
}

export function createTable<T>(ctx: Ctx, options: TableOptions<T>): Table<T> {
  let query: Record<string, string | undefined> = { ...options.query };
  let cursorStack: Array<string | null> = [null];
  let index = 0;
  let rows: T[] = [];
  let loading = false;
  let generation = 0;
  let nextCursor: string | null = null;
  let lastError: unknown = null;
  let loaded = false;

  const tbody = h("tbody");
  const thead = h("thead");
  const table = h("table", { class: options.class ? `table ${options.class}` : "table" }, thead, tbody);
  const wrap = h("div", { class: "table-wrap table-wrap--cards" }, table);
  const footHost = h("div");
  const card = h("section", { class: "card" }, wrap, footHost);

  const buildHead = () => {
    const tr = h("tr");
    for (const col of options.columns) {
      tr.appendChild(
        h(
          "th",
          {
            class: [col.align === "right" ? "num" : "", col.hideSmall ? "hide-sm" : "", col.hideMedium ? "hide-md" : ""].filter(Boolean).join(" "),
            scope: "col",
            width: col.width,
          },
          col.label,
        ),
      );
    }
    swap(thead, tr);
  };

  const renderFoot = () => {
    if (!loaded || lastError) {
      swap(footHost, h("div"));
      return;
    }
    const hasPrev = index > 0;
    const hasNext = Boolean(nextCursor);
    if (!hasPrev && !hasNext && !options.footNote) {
      swap(footHost, h("div"));
      return;
    }
    const pager = h(
      "div",
      { class: "pager" },
      h(
        "button",
        {
          class: "pager-btn",
          type: "button",
          disabled: !hasPrev,
          onclick: () => {
            if (!hasPrev) return;
            index -= 1;
            void load();
          },
        },
        icon("chevronLeft", 12),
        "Previous",
      ),
      h(
        "button",
        {
          class: "pager-btn",
          type: "button",
          disabled: !hasNext,
          onclick: () => {
            if (!nextCursor) return;
            cursorStack = cursorStack.slice(0, index + 1);
            cursorStack.push(nextCursor);
            index += 1;
            void load();
          },
        },
        "Next",
        icon("chevronRight", 12),
      ),
    );
    swap(
      footHost,
      h(
        "div",
        { class: "table-foot" },
        h("span", null, rows.length === 0 ? "No rows" : `${rows.length} row${rows.length === 1 ? "" : "s"}`),
        options.footNote ?? null,
        pager,
      ),
    );
  };

  const renderRows = () => {
    tbody.replaceChildren();
    if (rows.length === 0) {
      const cell = h("td", { colspan: options.columns.length }, emptyState({ ...options.empty, icon: "inbox" }));
      tbody.appendChild(h("tr", { class: "empty-row" }, cell));
      return;
    }
    for (const row of rows) {
      const tr = h("tr");
      const href = options.rowHref?.(row) ?? null;
      if (href) {
        tr.dataset.href = href;
        tr.classList.add("is-clickable");
        tr.addEventListener("click", (event) => {
          const target = event.target as HTMLElement;
          // A real control inside the row (a link, a button) keeps its own behaviour.
          if (target.closest("a,button,input,select,textarea,label")) return;
          ctx.navigate(href);
        });
      }
      if (options.rowKey) tr.dataset.key = options.rowKey(row);

      for (const col of options.columns) {
        const td = h(
          "td",
          {
            class: [
              col.align === "right" ? "num" : "",
              col.primary ? "cell-primary" : "",
              col.hideSmall ? "hide-sm" : "",
              col.hideMedium ? "hide-md" : "",
              col.className ?? "",
            ]
              .filter(Boolean)
              .join(" "),
            "data-label": col.primary ? undefined : col.label,
          },
          col.render(row),
        );
        tr.appendChild(td);
      }
      tbody.appendChild(tr);
    }
  };

  const load = async (): Promise<void> => {
    const mine = ++generation;
    loading = true;
    card.classList.add("table-loading");
    lastError = null;
    if (!loaded) swap(wrap, skeletonTable(7, Math.min(options.columns.length, 6)));

    try {
      const page = await ctx.api.get<Page<T>>(options.path, {
        query: {
          ...query,
          limit: options.limit ?? 25,
          cursor: cursorStack[index] ?? undefined,
        },
        cache: false,
      });
      if (mine !== generation) return;
      rows = page.data;
      nextCursor = page.next_cursor ?? null;
      loaded = true;
      swap(wrap, h("div", { class: "table-wrap table-wrap--cards" }, table));
      renderRows();
      renderFoot();
      options.onLoaded?.(rows, page);
    } catch (err) {
      if (mine !== generation) return;
      if (err instanceof DOMException && err.name === "AbortError") return;
      lastError = err;
      loaded = true;
      swap(
        wrap,
        errorState(err, () => {
          void load();
        }),
      );
      swap(footHost, h("div"));
      if (err instanceof ApiError && err.status === 401) return;
    } finally {
      if (mine === generation) {
        loading = false;
        card.classList.remove("table-loading");
      }
    }
  };

  buildHead();
  void load();

  return {
    el: card,
    reload(next) {
      if (next) query = { ...next };
      // A filter change resets the cursor stack: a cursor is only meaningful for the query
      // that produced it.
      cursorStack = [null];
      index = 0;
      return load();
    },
    rows: () => rows,
    setLoading(on) {
      loading = on;
      card.classList.toggle("table-loading", on);
    },
  };
}
