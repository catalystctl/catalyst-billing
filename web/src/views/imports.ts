// Imports.
//
// This screen exists because moving off another billing panel is the moment a shop is most
// likely to lose data, and the operator needs to see what will happen before it happens. Three
// decisions shape it:
//
//  - **The dry run is the default and the commit is a second, deliberate act.** The server
//    refuses to write unless the body says `commit: true`, and this screen never sends that
//    from the button an operator presses first. A migration that half-runs because someone
//    clicked the wrong button is unrecoverable in a way a refused dry run is not.
//
//  - **A rejection is shown with its row and its line.** `Row` is the n-th data row and `Line`
//    is the line in the file, because those answer different questions: Row tells you which
//    record, Line tells you where to look in the editor. Showing only one of them is the reason
//    people give up on an import report and edit the CSV by hand.
//
//  - **Nothing is claimed about a run that did not finish.** The report's `dry_run` flag is
//    rendered at the top, because it is the single most important fact in the document, and a
//    commit that was rolled back shows as a conflict rather than as a partial success.
//
// The CSV text is pasted or dropped rather than uploaded as a file: the request body is JSON and
// the server bounds it at 1 MiB, so the screen says so rather than letting a large file fail with
// a body-too-large error after the operator has waited.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateTime, titleCase } from "../lib/format";
import type { Ctx } from "../lib/types";
import {
  badge,
  banner,
  btn,
  card,
  emptyState,
  errorState,
  field,
  pageHead,
  resource,
  select,
  skeletonPage,
  textarea,
  withBusy,
} from "../ui/kit";

export const meta = { title: "Imports", crumb: "Imports" };

// ---------------------------------------------------------------- payloads --

/** One refused row, from `importer.Rejection`. */
interface Rejection {
  file: string;
  row: number;
  line: number;
  entity: string;
  legacy_id?: string;
  code: string;
  detail: string;
}

/** One thing imported that the operator should look at, from `importer.Warning`. */
interface Warning {
  file: string;
  row: number;
  entity: string;
  code: string;
  detail: string;
}

/** One document's outcome, from `importer.FileReport`. */
interface FileReport {
  file: string;
  rows_read: number;
  created?: number;
  updated?: number;
  unchanged?: number;
  rejected?: number;
  error?: string;
}

/** One entity's outcome, from `importer.TableReport`. */
interface TableReport {
  entity: string;
  created: number;
  updated: number;
  unchanged: number;
  rejected: number;
}

/** The report, from `importer.Report`. */
interface ImportReport {
  source: string;
  /** True when nothing was written. Rendered first, because it is the most important fact. */
  dry_run: boolean;
  run_id?: string;
  started_at: string;
  duration_ms: number;
  totals: {
    rows_read: number;
    created: number;
    updated: number;
    unchanged: number;
    rejected: number;
  };
  files: FileReport[] | null;
  tables: TableReport[] | null;
  rejections: Rejection[] | null;
  warnings: Warning[] | null;
}

/** One recorded run, from `importRunJSON`. */
interface RunRecord {
  id: string;
  source: string;
  actor_id?: string | null;
  created: number;
  unchanged: number;
  rejected: number;
  created_at: string;
  report?: ImportReport;
}

const ENDPOINT = "/api/v1/staff/imports";

// The document names the importer reads, in the order it processes them. The order is the
// server's and it matters — clients before invoices — so it is stated once here and the screen
// renders the inputs in that order rather than letting an operator guess.
const DOCUMENTS = ["clients", "invoices", "invoice_items", "services", "transactions"] as const;

/** The body limit the server enforces, so the screen can warn before a paste rather than after. */
const MAX_BODY_BYTES = 1 << 20;

export default async function importsPage(ctx: Ctx): Promise<Node> {
  const host = h("div", { class: "resource" }, skeletonPage());
  const controller = new AbortController();
  ctx.onDispose(() => controller.abort());

  const load = async (): Promise<void> => {
    try {
      const runs = await ctx.api.get<{ data: RunRecord[] }>(ENDPOINT, {
        cache: false,
        signal: controller.signal,
      });
      if (controller.signal.aborted) return;
      swap(host, render(ctx, runs.data ?? [], load));
    } catch (err) {
      if (controller.signal.aborted) return;
      swap(host, errorState(err, () => {
        swap(host, skeletonPage());
        void load();
      }));
    }
  };
  void load();
  return host;
}

function render(ctx: Ctx, runs: RunRecord[], reload: () => Promise<void>): Node {
  const sourceSelect = select(
    [
      { value: "whmcs", label: "WHMCS" },
      { value: "paymenter", label: "Paymenter" },
    ],
    { id: "import-source", name: "source" },
  );

  const currencyInput = h("input", {
    class: "input",
    id: "import-currency",
    name: "default_currency",
    value: "USD",
    maxlength: "3",
    spellcheck: "false",
    placeholder: "USD",
  }) as HTMLInputElement;

  const inputs = new Map<string, HTMLTextAreaElement>();
  const documentFields = DOCUMENTS.map((name) => {
    const area = textarea({
      id: `import-file-${name}`,
      name,
      rows: 3,
      spellcheck: "false",
      placeholder: `${name}.csv — paste the contents, header row included`,
    }) as HTMLTextAreaElement;
    inputs.set(name, area);
    return field({
      label: titleCase(name.replace("_", " ")),
      control: area,
      id: `import-file-${name}`,
      hint: "Leave empty if this dump has no such document.",
    });
  });

  const reportHost = h("div");

  const buildBody = (commit: boolean) => {
    const files: Record<string, string> = {};
    let bytes = 0;
    for (const [name, area] of inputs) {
      const text = area.value.trim();
      if (text === "") continue;
      files[name] = text;
      bytes += text.length;
    }
    return { files, bytes, body: { source: sourceSelect.value, commit, default_currency: currencyInput.value.trim().toUpperCase(), files } };
  };

  const run = async (commit: boolean, button: HTMLButtonElement): Promise<void> => {
    const { files, bytes, body } = buildBody(commit);
    if (Object.keys(files).length === 0) {
      ctx.toast("Paste at least one document first", { tone: "danger" });
      return;
    }
    if (bytes > MAX_BODY_BYTES) {
      ctx.toast("That is too much text for one request", {
        tone: "danger",
        detail: `The server accepts ${Math.round(MAX_BODY_BYTES / 1024)} KiB per import; this is ${Math.round(bytes / 1024)} KiB. Split the dump and import it in parts.`,
      });
      return;
    }

    await withBusy(ctx, button, async () => {
      try {
        const report = await ctx.api.post<ImportReport>(ENDPOINT, body);
        swap(reportHost, renderReport(ctx, report, () => {
          // A commit changed the run history, so the list below is stale.
          if (!report.dry_run) void reload();
        }));
        reportHost.scrollIntoView({ block: "nearest", behavior: "smooth" });
        ctx.toast(report.dry_run ? "Dry run finished — nothing was written" : "Import finished", {
          tone: report.dry_run ? "info" : "success",
        });
      } catch (err) {
        swap(reportHost, renderFailure(ctx, err));
        reportHost.scrollIntoView({ block: "nearest", behavior: "smooth" });
      }
    });
  };

  return h(
    "div",
    { class: "page stack" },
    pageHead({
      title: "Imports",
      sub: "Move customers, invoices, services and payments from another billing panel, and see exactly what will happen before it does.",
    }),

    banner(
      "info",
      "A dry run is the default",
      h(
        "p",
        null,
        "The first button reads the dump and reports what it would do without writing anything. Only the second button writes, and it only appears once a dry run has been read.",
      ),
    ),

    card({
      title: "The dump",
      sub: "Paste each document's CSV text. The header row is required.",
      body: h(
        "div",
        { class: "stack" },
        h(
          "div",
          { class: "grid-2" },
          field({ label: "Source panel", control: sourceSelect, id: "import-source" }),
          field({
            label: "Default currency",
            control: currencyInput,
            id: "import-currency",
            hint: "Used for rows that carry no currency of their own.",
          }),
        ),
        h("div", { class: "stack-sm" }, ...documentFields),
      ),
    }),

    card({
      title: "Run it",
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "div",
          { class: "inline-row" },
          btn("Dry run", {
            icon: "eye",
            variant: "primary",
            onclick: (e) => void run(false, e.currentTarget as HTMLButtonElement),
          }),
          btn("Import for real", {
            icon: "database",
            variant: "danger",
            onclick: (e) => {
              const button = e.currentTarget as HTMLButtonElement;
              void ctx
                .confirm({
                  title: "Write this import?",
                  body: "This creates and updates customers, invoices, services and payments. It is idempotent — running the same dump again changes nothing new — but a wrong dump writes wrong records. Run a dry run first.",
                  confirmLabel: "Import",
                  danger: true,
                  // Typing the word is the guard: this button writes customer and invoice
                  // records, and a mis-click is not recoverable by re-running.
                  phrase: "import",
                })
                .then((ok) => {
                  if (ok) void run(true, button);
                });
            },
          }),
        ),
        h(
          "p",
          { class: "field-hint" },
          "Importing for real is idempotent on the dump's own identifiers, so re-running the same file reports each row as unchanged rather than duplicating it.",
        ),
        reportHost,
      ),
    }),

    runs.length > 0
      ? card({
          title: "Previous runs",
          sub: "Newest first. A run's report is kept so an operator can answer 'what did that import actually do' months later.",
          body: h(
            "div",
            { class: "table-wrap table-wrap--cards" },
            h(
              "table",
              { class: "table" },
              h("thead", null, h("tr", null,
                h("th", { scope: "col" }, "When"),
                h("th", { scope: "col" }, "Source"),
                h("th", { scope: "col", class: "num" }, "Created"),
                h("th", { scope: "col", class: "num" }, "Unchanged"),
                h("th", { scope: "col", class: "num" }, "Rejected"),
                h("th", { scope: "col", "aria-label": "Report" }),
              )),
              h(
                "tbody",
                null,
                ...runs.map((r) =>
                  h(
                    "tr",
                    null,
                    h("td", { class: "cell-primary" }, dateTime(r.created_at)),
                    h("td", { "data-label": "Source" }, badge(r.source, "neutral")),
                    h("td", { "data-label": "Created", class: "num" }, String(r.created)),
                    h("td", { "data-label": "Unchanged", class: "num" }, String(r.unchanged)),
                    h("td", { "data-label": "Rejected", class: "num" }, r.rejected > 0 ? badge(String(r.rejected), "warning") : "0"),
                    h(
                      "td",
                      { class: "cell-actions" },
                      h("div", { class: "table-actions" },
                        btn("Report", {
                          variant: "ghost",
                          size: "sm",
                          onclick: () => {
                            if (r.report) {
                              swap(reportHost, renderReport(ctx, r.report, () => {}));
                              reportHost.scrollIntoView({ block: "nearest", behavior: "smooth" });
                            } else {
                              ctx.toast("That run's report was not retained", { tone: "info" });
                            }
                          },
                        }),
                      ),
                    ),
                  ),
                ),
              ),
            ),
          ),
        })
      : card({
          body: emptyState({
            title: "No imports yet",
            body: "A completed import is recorded here with its full report, so the question 'what did that migration actually do' has an answer later.",
            icon: "database",
          }),
        }),
  );
}

// ----------------------------------------------------------------- report --

function renderReport(ctx: Ctx, report: ImportReport, onCommitted: () => void): Node {
  const t = report.totals;
  const rejections = report.rejections ?? [];
  const warnings = report.warnings ?? [];
  const tables = report.tables ?? [];

  // A commit is announced once, on the render that follows it. The report itself does not know
  // whether the caller has seen it, so the notice is keyed on the fact rather than on state.
  if (!report.dry_run) onCommitted();

  return h(
    "div",
    { class: "stack-sm" },
    report.dry_run
      ? banner(
          "info",
          "Dry run — nothing was written",
          h(
            "p",
            null,
            "Read ",
            String(t.rows_read),
            " rows in ",
            String(report.duration_ms),
            " ms. ",
            String(t.created),
            " would be created, ",
            String(t.updated),
            " updated, ",
            String(t.rejected),
            " refused.",
          ),
        )
      : banner(
          "success",
          "Import written",
          h(
            "p",
            null,
            "Created ",
            String(t.created),
            ", updated ",
            String(t.updated),
            ", unchanged ",
            String(t.unchanged),
            ", refused ",
            String(t.rejected),
            " of ",
            String(t.rows_read),
            " rows in ",
            String(report.duration_ms),
            " ms.",
          ),
        ),

    // A refusal is the reason an operator is on this screen, so it comes before the summary
    // tables rather than after them.
    rejections.length > 0
      ? h(
          "div",
          { class: "stack-sm" },
          h("h3", { class: "section-title" }, `${rejections.length} row${rejections.length === 1 ? "" : "s"} refused`),
          h(
            "p",
            { class: "field-hint" },
            "Row is the n-th data row of the document; line is where to look in the file. Nothing was written for a refused row — fix it and run the dump again.",
          ),
          h(
            "div",
            { class: "table-wrap table-wrap--cards" },
            h(
              "table",
              { class: "table" },
              h("thead", null, h("tr", null,
                h("th", { scope: "col" }, "Document"),
                h("th", { scope: "col", class: "num" }, "Row"),
                h("th", { scope: "col", class: "num" }, "Line"),
                h("th", { scope: "col" }, "Entity"),
                h("th", { scope: "col" }, "Reason"),
                h("th", { scope: "col" }, "Detail"),
              )),
              h(
                "tbody",
                null,
                ...rejections.slice(0, 200).map((r) =>
                  h(
                    "tr",
                    null,
                    h("td", { class: "cell-primary" }, h("code", null, r.file)),
                    h("td", { "data-label": "Row", class: "num" }, String(r.row)),
                    h("td", { "data-label": "Line", class: "num" }, String(r.line)),
                    h("td", { "data-label": "Entity" }, r.entity),
                    h("td", { "data-label": "Reason" }, h("code", null, r.code)),
                    h("td", { "data-label": "Detail" }, r.detail),
                  ),
                ),
              ),
            ),
          ),
          rejections.length > 200
            ? h("p", { class: "field-hint" }, `Showing the first 200 of ${rejections.length}; the rest are in the run's report.`)
            : null,
        )
      : null,

    tables.length > 0
      ? h(
          "div",
          { class: "stack-sm" },
          h("h3", { class: "section-title" }, "By entity"),
          h(
            "div",
            { class: "table-wrap table-wrap--cards" },
            h(
              "table",
              { class: "table" },
              h("thead", null, h("tr", null,
                h("th", { scope: "col" }, "Entity"),
                h("th", { scope: "col", class: "num" }, "Created"),
                h("th", { scope: "col", class: "num" }, "Updated"),
                h("th", { scope: "col", class: "num" }, "Unchanged"),
                h("th", { scope: "col", class: "num" }, "Rejected"),
              )),
              h(
                "tbody",
                null,
                ...tables.map((row) =>
                  h(
                    "tr",
                    null,
                    h("td", { class: "cell-primary" }, titleCase(row.entity)),
                    h("td", { "data-label": "Created", class: "num" }, String(row.created)),
                    h("td", { "data-label": "Updated", class: "num" }, String(row.updated)),
                    h("td", { "data-label": "Unchanged", class: "num" }, String(row.unchanged)),
                    h("td", { "data-label": "Rejected", class: "num" }, row.rejected > 0 ? badge(String(row.rejected), "warning") : "0"),
                  ),
                ),
              ),
            ),
          ),
        )
      : null,

    warnings.length > 0
      ? banner(
          "warning",
          `${warnings.length} thing${warnings.length === 1 ? "" : "s"} to look at`,
          h(
            "ul",
            { class: "plain-list" },
            ...warnings.slice(0, 20).map((w) =>
              h("li", null, h("code", null, w.code), " — ", w.detail, ` (${w.file}, row ${w.row})`),
            ),
          ),
        )
      : null,
  );
}

// ---------------------------------------------------------------- failure --

function renderFailure(ctx: Ctx, err: unknown): Node {
  if (!(err instanceof ApiError)) {
    return banner("danger", "The import could not be run", h("p", null, err instanceof Error ? err.message : "Unknown failure."));
  }
  switch (err.code) {
    case "import_conflict":
      // The server rolled back, so nothing was written. Saying so is the whole message: an
      // operator who believes a failed import half-wrote will do something drastic.
      return banner(
        "warning",
        "Another import wrote the same records at the same time",
        h("p", null, "Nothing was changed. Run the import again."),
      );
    case "unknown_source":
      return banner("danger", "That source panel is not supported", h("p", null, "This build reads WHMCS and Paymenter dumps."));
    case "missing_file":
      return banner("danger", "A required document is missing", h("p", null, err.message));
    case "invalid_input":
      return banner("danger", "The dump could not be read", h("p", null, err.message));
    case "payload_too_large":
      return banner("danger", "That is too much text for one request", h("p", null, err.message));
    default:
      return banner("danger", "The import could not be completed", h("p", null, err.message || "Nothing was written."));
  }
}
