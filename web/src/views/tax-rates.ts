// Tax rates.
//
// A rate is a *dated row*, not a setting. That single fact shapes the screen:
//
//  - A rate is created effective from a date and closed by setting an end date. It is not
//    edited once it has taken effect, because the rate is the evidence an issued invoice points
//    at — rewriting it would change what a document already says.
//  - So the destructive action is "Close", which sets `effective_to`, and a delete is offered
//    only for a rate that never took effect (the server refuses it otherwise with `in_use`).
//  - The rate is an exact rational string. "0.19", "19%" and "1/5" are all accepted by the
//    server and canonicalised; the form accepts the same three spellings and shows the
//    canonical form the server returned.
//
// Country "all" is the wildcard the resolver uses when nothing more specific matches, and it is
// the literal the database CHECK allows, so the form offers it as an explicit choice rather
// than asking an operator to type a two-letter code that means "everywhere".

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateOnly, dateTime } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { badge, banner, btn, card, field, input, pageHead, select } from "../ui/kit";
import { bindForm, openDialog } from "../ui/dialog";
import { createTable, type Column } from "../ui/table";

export const meta = { title: "Tax rates", crumb: "Tax rates" };

// ---------------------------------------------------------------- payloads --

/** One rate, verbatim from `taxRateJSON` in internal/httpapi/commerce_tax.go. `rate` is the
 *  canonical rational string; `jurisdiction` is the server's own rendering of country+region. */
interface TaxRate {
  id: string;
  name: string;
  country_code: string;
  region_code: string;
  tax_class: string;
  rate: string;
  inclusive: boolean;
  effective_from: string;
  effective_to?: string;
  enabled: boolean;
  jurisdiction: string;
  created_at: string;
}

interface TaxRateList {
  data: TaxRate[];
}

// ------------------------------------------------------- import payloads --

/** The configured external document, from `taxSourceResponse` in internal/httpapi/tax_import.go. */
interface TaxSource {
  url: string;
  format: "json" | "csv";
  enabled: boolean;
  refresh_interval_hours: number;
  updated_at: string;
}

/** One import run. `report` is the per-row record, present on a stored run and on a fresh import. */
interface TaxRun {
  id: string;
  source_url: string;
  format: string;
  trigger: "manual" | "scheduled";
  fetched_at: string;
  started_at: string;
  finished_at?: string;
  outcome: "running" | "ok" | "failed";
  detail?: string;
  counts: TaxCounts;
  report?: TaxPreview;
}

interface TaxCounts {
  rows: number;
  created: number;
  updated: number;
  unchanged: number;
  protected: number;
  rejected: number;
}

/** What one row of the document would become. The strings are the server's, never re-derived here. */
type TaxOutcome = "create" | "update" | "unchanged" | "protected" | "rejected";

interface TaxChange {
  line: number;
  jurisdiction: string;
  outcome: TaxOutcome;
  reason?: string;
  code?: string;
  current?: {
    id: string;
    rate: string;
    inclusive: boolean;
    effective_from: string;
    origin: "manual" | "import";
    source?: string;
    fetched_at?: string;
  };
  proposed: {
    line: number;
    name: string;
    country_code: string;
    region_code: string;
    tax_class: string;
    rate: string;
    inclusive: boolean;
    effective_from: string;
  };
}

interface TaxPreview {
  source_url: string;
  format: string;
  fetched_at: string;
  rows: number;
  creates: number;
  updates: number;
  unchanged: number;
  protected: number;
  rejected: number;
  changes: TaxChange[];
  rejections: Array<{ line: number; key: string; code: string; detail: string }>;
}

interface TaxSourceResponse {
  configured: boolean;
  source: TaxSource | null;
  last_run?: TaxRun;
}

/** One rate's origin, from the provenance endpoint. Joined to the rate list by id, because the
 *  rate list is commerce's projection and provenance is the importer's fact. */
interface TaxProvenance {
  rate_id: string;
  origin: "manual" | "import";
  source: string;
  fetched_at?: string;
}

/** The wildcard country the migration's CHECK allows, from `commerce.WildcardCountry`. */
const WILDCARD = "all";

/** The classes the pricer resolves. `standard` is `commerce.DefaultTaxClass`; the others are
 *  the conventional reduced and zero bands an operator expects to be able to name. */
const TAX_CLASSES = [
  { value: "standard", label: "Standard" },
  { value: "reduced", label: "Reduced" },
  { value: "zero", label: "Zero" },
];

export default async function taxRatesPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("tax:write")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Tax rates" }),
      banner(
        "danger",
        "You cannot read the tax table",
        "The rate table needs the tax:write permission — it is configuration, not something a billing clerk needs in order to price an invoice.",
        btn("Back to the dashboard", { href: "/dashboard", size: "sm", variant: "ghost" }),
      ),
    );
  }

  let rows: TaxRate[] = [];

  // Provenance is read before the table is built so the "Source" column can render on the first
  // paint. A failure here degrades the column to "—" rather than the page: a rate table without
  // provenance is still a usable rate table, and refusing to render it would be worse.
  const provenance = new Map<string, TaxProvenance>();
  try {
    const loaded = await ctx.api.get<{ data: TaxProvenance[] }>("/api/v1/staff/tax-import/provenance");
    for (const entry of loaded.data) provenance.set(entry.rate_id, entry);
  } catch {
    // Left empty on purpose; the column says so.
  }

  // The source card's state. It is read once here so the card renders on the first paint, and
  // replaced after a save or an import so the screen never shows a stale URL.
  let sourceState: TaxSourceResponse = { configured: false, source: null };
  try {
    sourceState = await ctx.api.get<TaxSourceResponse>("/api/v1/staff/tax-import/source");
  } catch {
    // The card renders its unconfigured prompt; the preview and import will explain the rest.
  }
  const sourceHost = h("div");

  const columns: Column<TaxRate>[] = [
    {
      key: "jurisdiction",
      label: "Jurisdiction",
      primary: true,
      render: (row) =>
        h(
          "div",
          { class: "cell-main" },
          h(
            "div",
            { class: "primary" },
            h("span", { class: "row-title" }, row.jurisdiction || row.country_code),
            row.enabled ? null : badge("Disabled", "neutral", { dot: true }),
            row.inclusive ? badge("Inclusive", "info") : null,
          ),
          h(
            "div",
            { class: "secondary" },
            row.name || "Unnamed rate",
            row.region_code ? ` · ${row.region_code}` : "",
            row.country_code === WILDCARD ? " · matches every country" : "",
          ),
        ),
    },
    {
      key: "class",
      label: "Class",
      hideSmall: true,
      render: (row) => h("span", { class: "dim" }, row.tax_class || "standard"),
    },
    {
      key: "rate",
      label: "Rate",
      align: "right",
      render: (row) =>
        h(
          "span",
          { class: "money", title: `Exact rational: ${row.rate}` },
          h("span", { class: "money-code" }, ratePercent(row.rate)),
        ),
    },
    {
      key: "from",
      label: "Effective from",
      align: "right",
      render: (row) => h("span", { class: "dim nowrap", title: dateTime(row.effective_from) }, dateOnly(row.effective_from)),
    },
    {
      key: "to",
      label: "Effective to",
      align: "right",
      hideSmall: true,
      render: (row) =>
        row.effective_to
          ? h("span", { class: "dim nowrap", title: dateTime(row.effective_to) }, dateOnly(row.effective_to))
          : h("span", { class: "faint nowrap" }, "Open"),
    },
    {
      key: "origin",
      label: "Source",
      hideSmall: true,
      render: (row) => {
        const entry = provenance.get(row.id);
        if (!entry) return h("span", { class: "faint" }, "—");
        if (entry.origin === "manual") {
          return h("span", { class: "dim", title: "Entered in this panel; an import will not replace it" }, "Manual");
        }
        const when = entry.fetched_at ? dateTime(entry.fetched_at) : "unknown time";
        return h(
          "span",
          { class: "dim", title: `Imported from ${entry.source} on ${when}` },
          badge("Imported", "info"),
        );
      },
    },
    {
      key: "actions",
      label: "",
      align: "right",
      width: "1%",
      render: (row) =>
        h(
          "div",
          { class: "row" },
          btn("Edit", { size: "sm", variant: "ghost", icon: "edit", onclick: () => rateDialog(row) }),
          row.effective_to
            ? null
            : btn("Close", {
                size: "sm",
                variant: "ghost",
                icon: "pauseCircle",
                title: "Stop this rate applying from a date, without changing what it already applied to",
                onclick: () => void closeRate(row),
              }),
          // A delete is only offered for a rate that never took effect. The server refuses
          // anything else, and a button that always fails is a bug report waiting to happen.
          row.effective_to
            ? btn("Delete", {
                size: "sm",
                variant: "ghost",
                icon: "trash",
                title: "Remove a rate that no longer applies",
                onclick: () => void deleteRate(row),
              })
            : null,
        ),
    },
  ];

  const table = createTable<TaxRate>(ctx, {
    path: "/api/v1/staff/tax-rates",
    columns,
    empty: {
      title: "No tax rates",
      body: "Tax is off until a rate exists. Add the rate for your home country first; without one the cart prices everything net.",
      action: btn("Add the first rate", { variant: "primary", icon: "plus", onclick: () => rateDialog(null) }),
    },
    rowKey: (row) => row.id,
    footNote: "Rates are dated. A closed rate keeps applying to the period it covered.",
    onLoaded: (loaded) => {
      rows = loaded;
    },
  });

  // --------------------------------------------------------- import surface --

  /** Re-reads the provenance map in place, so a reloaded table paints the new origins. */
  async function refreshProvenance(): Promise<void> {
    try {
      const loaded = await ctx.api.get<{ data: TaxProvenance[] }>("/api/v1/staff/tax-import/provenance");
      provenance.clear();
      for (const entry of loaded.data) provenance.set(entry.rate_id, entry);
    } catch {
      // Keep what we had; the column is a reading aid, not a guard.
    }
  }

  /** Re-reads the source and repaints the card. */
  async function refreshSource(): Promise<void> {
    try {
      sourceState = await ctx.api.get<TaxSourceResponse>("/api/v1/staff/tax-import/source");
    } catch {
      sourceState = { configured: false, source: null };
    }
    swap(sourceHost, importCard());
  }

  /**
   * The source card: where rates come from, when they were last refreshed, and the three actions.
   *
   * It is deliberately separate from the rate table. A rate is a dated row an operator may still
   * edit by hand; the source is installation configuration, and mixing the two would make it look
   * as though importing were the only way to maintain the table.
   */
  function importCard(): HTMLElement {
    const source = sourceState.source;
    const lastRun = sourceState.last_run;
    const actions = h(
      "div",
      { class: "row" },
      btn(source ? "Edit source" : "Configure source", {
        size: "sm",
        variant: source ? "ghost" : "primary",
        icon: "edit",
        onclick: () => sourceDialog(),
      }),
      source
        ? btn("Preview", { size: "sm", variant: "ghost", icon: "search", onclick: () => void previewDialog() })
        : null,
      source
        ? btn("Import now", { size: "sm", variant: "primary", icon: "download", onclick: () => void importNow(false) })
        : null,
    );

    if (!source) {
      return card({
        title: "External source",
        sub: "Fetch rates instead of typing them",
        body: h(
          "div",
          { class: "stack-sm" },
          h(
            "p",
            { class: "dim" },
            "No source is configured, so every rate in this table was entered by hand. Point the panel at a JSON or CSV document and it can fetch, preview and import the rates it states — recording which document each rate came from and when.",
          ),
          h(
            "p",
            { class: "faint" },
            "A rate you typed is never replaced by an import unless you say so at the moment of importing.",
          ),
        ),
        actions,
      });
    }

    const lastRunLine = lastRun
      ? lastRun.outcome === "failed"
        ? h(
            "span",
            { class: "dim" },
            `Last attempt ${dateTime(lastRun.started_at)} failed: ${lastRun.detail || "the source could not be read"}`,
          )
        : h(
            "span",
            { class: "dim" },
            `Last refreshed ${dateTime(lastRun.started_at)} (${lastRun.trigger}) — ${lastRun.counts.created} created, ${lastRun.counts.updated} updated, ${lastRun.counts.unchanged} unchanged, ${lastRun.counts.protected} protected, ${lastRun.counts.rejected} rejected`,
          )
      : h("span", { class: "faint" }, "Never fetched from this source yet.");

    return card({
      title: "External source",
      sub: source.enabled ? "Enabled" : "Switched off",
      actions,
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "div",
          { class: "row-wrap" },
          badge(source.format.toUpperCase(), "neutral"),
          badge(`Every ${source.refresh_interval_hours}h`, "neutral"),
          source.enabled ? badge("Enabled", "success", { dot: true }) : badge("Off", "warning", { dot: true }),
          h("span", { class: "mono dim" }, source.url),
        ),
        lastRunLine,
      ),
    });
  }

  /** The source form. Saving replaces the installation's one source. */
  function sourceDialog(): void {
    const existing = sourceState.source;
    const url = input({
      id: "tax-source-url",
      value: existing?.url ?? "",
      placeholder: "https://example.test/tax-rates.json",
      required: true,
      autocomplete: "off",
    });
    const format = select(
      [
        { value: "json", label: "JSON — an array of rate objects" },
        { value: "csv", label: "CSV — a header row and the same columns" },
      ],
      { id: "tax-source-format", value: existing?.format ?? "json" },
    );
    const interval = input({
      id: "tax-source-interval",
      type: "number",
      value: String(existing?.refresh_interval_hours ?? 24),
      min: "1",
      max: "8760",
    });
    const enabled = h("input", { type: "checkbox", checked: existing?.enabled ?? true });
    const errorHost = h("div");

    const body = h(
      "div",
      { class: "form-grid" },
      field({
        label: "Source URL",
        control: url,
        id: "tax-source-url",
        required: true,
        class: "span-2",
        hint: "http or https. The panel fetches it anonymously; credentials in a URL are refused.",
      }),
      field({ label: "Format", control: format, id: "tax-source-format" }),
      field({
        label: "Refresh every (hours)",
        control: interval,
        id: "tax-source-interval",
        hint: "How often the worker re-fetches. It ticks hourly and only fetches when this has elapsed.",
      }),
      h("div", { class: "span-2" }, h("label", { class: "switch" }, enabled, "Enabled — fetch this source")),
      h(
        "p",
        { class: "faint span-2" },
        "Expected columns: country_code and rate (required), plus region_code, tax_class, inclusive, effective_from and name. A rate is a fraction: 0.19 is 19%. An unrecognised column is ignored.",
      ),
    );

    const submit = btn(existing ? "Save source" : "Save source", { variant: "primary", type: "submit" });
    const cancel = btn("Cancel", { variant: "ghost" });
    const form = h("form", { class: "stack", novalidate: true }, errorHost, body);
    bindForm(form, submit);
    const handle = openDialog({
      title: existing ? "Edit the tax-rate source" : "Configure a tax-rate source",
      subtitle: "One source per installation. Rates already imported keep the URL they came from.",
      body: h("div", { class: "stack" }, form),
      footer: h("div", { class: "row spacer" }, cancel, submit),
      wide: true,
      locked: () => submit instanceof HTMLButtonElement && submit.disabled,
    });
    cancel.addEventListener("click", () => handle.close());

    form.addEventListener("submit", (event) => {
      event.preventDefault();
      void (async () => {
        errorHost.replaceChildren();
        const hours = Number.parseInt(interval.value, 10);
        if (!Number.isFinite(hours) || hours < 1 || hours > 8760) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "The refresh interval is a whole number of hours between 1 and 8760."));
          return;
        }
        if (submit instanceof HTMLButtonElement) submit.disabled = true;
        try {
          const saved = await ctx.api.put<TaxSourceResponse>("/api/v1/staff/tax-import/source", {
            url: url.value.trim(),
            format: format.value,
            enabled: enabled.checked,
            refresh_interval_hours: hours,
          });
          sourceState = { ...saved, last_run: sourceState.last_run };
          swap(sourceHost, importCard());
          handle.close();
          ctx.toast("Source saved", { tone: "success", detail: saved.source?.url ?? "" });
        } catch (err) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, importError(err, "The source could not be saved")));
        } finally {
          if (submit instanceof HTMLButtonElement) submit.disabled = false;
        }
      })();
    });
  }

  /**
   * The preview: what an import would do, before it does it.
   *
   * The overwrite switch is here rather than on the import confirmation because it changes what
   * the preview *says*: with it off, a jurisdiction whose rate an operator typed is reported as
   * protected. An operator should see that list before deciding, not after.
   */
  async function previewDialog(): Promise<void> {
    const overwrite = h("input", { type: "checkbox", checked: false });
    const bodyHost = h("div", { class: "stack" }, h("p", { class: "dim" }, "Fetching the source…"));
    const importBtn = btn("Import these rates", {
      variant: "primary",
      icon: "download",
      // Close on success only: a failure leaves the dialog open so the operator can read the
      // reason next to the rows it applies to, and fix the source rather than re-opening it.
      onclick: () =>
        void (async () => {
          if (await importNow(overwrite.checked)) handle.close();
        })(),
    });
    // btn() returns a button or an anchor; only a button has a disabled state, and the guard is the
    // same one the rest of this file uses rather than a cast.
    const setImportEnabled = (enabled: boolean) => {
      if (importBtn instanceof HTMLButtonElement) importBtn.disabled = !enabled;
    };
    setImportEnabled(false);
    const handle = openDialog({
      title: "Preview an import",
      subtitle: "Nothing is written until you import. The import re-reads the source, so this is a forecast, not a promise.",
      body: bodyHost,
      footer: h(
        "div",
        { class: "row spacer" },
        h("label", { class: "switch" }, overwrite, "Replace rates I entered by hand"),
        btn("Close", { variant: "ghost", onclick: () => handle.close() }),
        importBtn,
      ),
      wide: true,
    });

    const load = async (): Promise<void> => {
      swap(bodyHost, h("p", { class: "dim" }, "Fetching the source…"));
      setImportEnabled(false);
      try {
        const response = await ctx.api.post<{ preview: TaxPreview }>("/api/v1/staff/tax-import/preview", {
          overwrite_manual: overwrite.checked,
        });
        swap(bodyHost, previewBody(response.preview));
        setImportEnabled(response.preview.creates + response.preview.updates > 0);
      } catch (err) {
        swap(bodyHost, h("div", { class: "banner banner--danger" }, importError(err, "The source could not be previewed")));
      }
    };
    overwrite.addEventListener("change", () => void load());
    await load();
  }

  /** Renders a preview: the counts, then every row with what would happen to it. */
  function previewBody(preview: TaxPreview): HTMLElement {
    const changes = preview.changes.length
      ? h(
          "div",
          { class: "stack-sm" },
          ...preview.changes.map((change) =>
            h(
              "div",
              { class: "row-wrap" },
              h("span", { class: "mono" }, change.jurisdiction),
              outcomeBadge(change.outcome),
              h(
                "span",
                { class: "dim" },
                change.current
                  ? `${ratePercent(change.current.rate)} → ${ratePercent(change.proposed.rate)}`
                  : ratePercent(change.proposed.rate),
              ),
              change.reason ? h("span", { class: "faint" }, change.reason) : null,
            ),
          ),
        )
      : h("p", { class: "faint" }, "The document states no rates.");

    const rejections = preview.rejections.length
      ? h(
          "div",
          { class: "stack-sm" },
          h("h3", { class: "section-title" }, `Refused rows (${preview.rejections.length})`),
          ...preview.rejections.map((rejection) =>
            h(
              "div",
              { class: "row-wrap" },
              h("span", { class: "mono" }, `line ${rejection.line}`),
              badge(rejection.code, "warning"),
              h("span", { class: "dim" }, rejection.detail),
            ),
          ),
        )
      : null;

    return h(
      "div",
      { class: "stack" },
      h(
        "div",
        { class: "row-wrap" },
        badge(`${preview.rows} row${preview.rows === 1 ? "" : "s"}`, "neutral"),
        badge(`${preview.creates} to add`, preview.creates ? "success" : "neutral"),
        badge(`${preview.updates} to replace`, preview.updates ? "warning" : "neutral"),
        badge(`${preview.unchanged} unchanged`, "neutral"),
        badge(`${preview.protected} protected`, preview.protected ? "info" : "neutral"),
        badge(`${preview.rejected} refused`, preview.rejected ? "danger" : "neutral"),
      ),
      h("p", { class: "faint" }, `Fetched ${dateTime(preview.fetched_at)} from ${preview.source_url}`),
      changes,
      rejections,
    );
  }

  /** Runs the import and reports what it did. Returns whether it succeeded. */
  async function importNow(overwriteManual: boolean): Promise<boolean> {
    try {
      const result = await ctx.api.post<{ run: TaxRun }>("/api/v1/staff/tax-import/import", {
        overwrite_manual: overwriteManual,
      });
      const counts = result.run.counts;
      ctx.toast("Tax rates imported", {
        tone: "success",
        detail: `${counts.created} added, ${counts.updated} replaced, ${counts.unchanged} unchanged, ${counts.protected} protected, ${counts.rejected} refused`,
      });
      await refreshProvenance();
      await refreshSource();
      await table.reload();
      return true;
    } catch (err) {
      ctx.toast(importError(err, "The import failed"), { tone: "danger", detail: err instanceof ApiError ? err.code : undefined });
      return false;
    }
  }

  /** Maps the import surface's codes to a sentence. The code is the contract; the detail is a hint. */
  function importError(err: unknown, fallback: string): string {
    if (!(err instanceof ApiError)) return `${fallback}: the server could not be reached.`;
    switch (err.code) {
      case "no_source":
        return "No source is configured yet. Save a URL first.";
      case "source_disabled":
        return "The source is switched off. Enable it before fetching.";
      case "source_unreachable":
        return err.detail || "The source could not be reached.";
      case "source_document_invalid":
        return err.detail || "The document could not be read. Check the format and the column names.";
      case "invalid_source":
        return err.detail || "The source URL or format was refused.";
      case "forbidden":
        return "You do not hold tax:write, so nothing was fetched.";
      default:
        return err.detail || `${fallback} (${err.code}).`;
    }
  }

  function outcomeBadge(outcome: TaxOutcome): HTMLElement {
    switch (outcome) {
      case "create":
        return badge("Add", "success");
      case "update":
        return badge("Replace", "warning");
      case "unchanged":
        return badge("Unchanged", "neutral");
      case "protected":
        return badge("Protected", "info");
      default:
        return badge("Refused", "danger");
    }
  }

  swap(sourceHost, importCard());

  // ------------------------------------------------------------- mutations --

  async function closeRate(row: TaxRate): Promise<void> {
    const body = input({
      id: "rate-close-at",
      type: "date",
      value: today(),
      required: true,
    });
    const ok = await ctx.confirm({
      title: `Close “${row.name || row.jurisdiction}”?`,
      body: h(
        "div",
        { class: "stack-sm" },
        h(
          "p",
          null,
          "The rate stops applying from this date. Invoices already issued keep the rate they were priced with, which is the point of a dated row.",
        ),
        field({ label: "Effective to", control: body, id: "rate-close-at", hint: "The last day this rate applies." }),
      ),
      confirmLabel: "Close rate",
      danger: true,
    });
    if (!ok) return;
    try {
      // A partial update: the server merges an absent field onto the stored row, so sending
      // only the end date cannot blank the rate or the name.
      await ctx.api.put(`/api/v1/staff/tax-rates/${encodeURIComponent(row.id)}`, {
        effective_to: isoFromDate(body.value),
      });
      ctx.toast("Rate closed", { tone: "success", detail: `${row.jurisdiction} · from ${body.value}` });
      await table.reload();
    } catch (err) {
      report(err, "The rate could not be closed");
    }
  }

  async function deleteRate(row: TaxRate): Promise<void> {
    const ok = await ctx.confirm({
      title: `Delete “${row.name || row.jurisdiction}”?`,
      body: "Only a rate that has already been closed can be removed, and only when no invoice points at it. If any does, the server refuses and the row stays — which is the right answer.",
      confirmLabel: "Delete rate",
      danger: true,
    });
    if (!ok) return;
    try {
      await ctx.api.del(`/api/v1/staff/tax-rates/${encodeURIComponent(row.id)}`);
      ctx.toast("Rate deleted", { tone: "success" });
      await table.reload();
    } catch (err) {
      report(err, "The rate could not be deleted");
    }
  }

  function report(err: unknown, fallback: string): void {
    if (err instanceof ApiError) {
      ctx.toast(err.detail || fallback, { tone: "danger", detail: err.code });
      return;
    }
    ctx.toast(fallback, { tone: "danger", detail: "The server could not be reached." });
  }

  // ---------------------------------------------------------------- dialog --

  function rateDialog(existing: TaxRate | null): void {
    const isEdit = existing !== null;
    const name = input({
      id: "rate-name",
      value: existing?.name ?? "",
      placeholder: "VAT — Germany",
      maxlength: 120,
    });
    const country = input({
      id: "rate-country",
      value: existing?.country_code ?? "",
      placeholder: "DE",
      class: "input--mono",
      maxlength: 2,
      autocomplete: "off",
    });
    const wildcard = h("input", { type: "checkbox", checked: existing?.country_code === WILDCARD });
    const region = input({
      id: "rate-region",
      value: existing?.region_code ?? "",
      placeholder: "CA (optional)",
      class: "input--mono",
      maxlength: 8,
      autocomplete: "off",
    });
    const taxClass = select(TAX_CLASSES, { id: "rate-class", value: existing?.tax_class || "standard" });
    const rate = input({
      id: "rate-value",
      value: existing?.rate ?? "",
      placeholder: "0.19 or 19%",
      class: "input--mono",
      required: true,
      autocomplete: "off",
    });
    const inclusive = h("input", { type: "checkbox", checked: existing?.inclusive ?? false });
    const from = input({ id: "rate-from", type: "date", value: existing ? isoDate(existing.effective_from) : today() });
    const to = input({ id: "rate-to", type: "date", value: existing?.effective_to ? isoDate(existing.effective_to) : "" });
    const enabled = h("input", { type: "checkbox", checked: existing?.enabled ?? true });

    const countryField = field({
      label: "Country",
      control: country,
      id: "rate-country",
      hint: "ISO 3166-1 alpha-2, e.g. DE. Two upper-case letters.",
    });
    const syncWildcard = () => {
      const wild = wildcard.checked;
      country.disabled = wild;
      if (wild) country.value = "";
      region.disabled = wild;
      if (wild) region.value = "";
      countryField.hidden = wild;
    };
    wildcard.addEventListener("change", syncWildcard);
    syncWildcard();

    const errorHost = h("div");
    const body = h(
      "div",
      { class: "form-grid" },
      field({ label: "Name", control: name, id: "rate-name", hint: "What an operator will recognise in the list.", class: "span-2" }),
      h("div", { class: "span-2" }, h("label", { class: "switch" }, wildcard, "Applies to every country (wildcard)")),
      countryField,
      field({ label: "Region", control: region, id: "rate-region", hint: "A state or province. Empty means the whole country." }),
      field({ label: "Tax class", control: taxClass, id: "rate-class" }),
      field({
        label: "Rate",
        control: rate,
        id: "rate-value",
        required: true,
        hint: "An exact number: 0.19, 19% or 1/5. Never a float.",
      }),
      field({ label: "Effective from", control: from, id: "rate-from", hint: "The first day this rate applies." }),
      field({ label: "Effective to", control: to, id: "rate-to", hint: "Leave empty for a rate that is still open." }),
      h("div", { class: "span-2" }, h("label", { class: "switch" }, inclusive, "The rate is included in the quoted price")),
      h("div", { class: "span-2" }, h("label", { class: "switch" }, enabled, "Enabled")),
    );

    const submit = btn(isEdit ? "Save rate" : "Add rate", { variant: "primary", type: "submit" });
    const cancel = btn("Cancel", { variant: "ghost" });
    const form = h("form", { class: "stack", novalidate: true }, errorHost, body);
    bindForm(form, submit);

    const handle = openDialog({
      title: isEdit ? "Edit tax rate" : "New tax rate",
      subtitle: isEdit
        ? "The window and the presentation can change. A rate that has taken effect cannot have its rate changed — close it and add a new row."
        : "A rate is dated from the moment it applies. Adding a new rate never rewrites the old one.",
      body: h("div", { class: "stack" }, form),
      footer: h("div", { class: "row spacer" }, cancel, submit),
      wide: true,
      locked: () => submit instanceof HTMLButtonElement && submit.disabled,
    });
    cancel.addEventListener("click", () => handle.close());

    form.addEventListener("submit", (event) => {
      event.preventDefault();
      void (async () => {
        errorHost.replaceChildren();
        const wild = wildcard.checked;
        const code = country.value.trim().toUpperCase();
        if (!wild && !/^[A-Z]{2}$/.test(code)) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "A country is two upper-case letters, e.g. DE, or tick the wildcard."));
          return;
        }
        const rawRate = rate.value.trim();
        if (!rawRate) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "A rate is required, e.g. 0.19 or 19%."));
          return;
        }
        if (!/^\d+(\.\d+)?%?$|^\d+\/\d+$/.test(rawRate)) {
          swap(
            errorHost,
            h("div", { class: "banner banner--danger" }, "The rate must be a number like 0.19, a percentage like 19%, or a fraction like 1/5."),
          );
          return;
        }
        const fromIso = isoFromDate(from.value);
        if (!fromIso) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "An effective-from date is required."));
          return;
        }
        const toIso = isoFromDate(to.value);
        if (toIso && Date.parse(toIso) <= Date.parse(fromIso)) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "The effective-to date must be after the effective-from date."));
          return;
        }
        const payload: Record<string, unknown> = {
          name: name.value.trim(),
          country_code: wild ? WILDCARD : code,
          region_code: wild ? "" : region.value.trim().toUpperCase(),
          tax_class: taxClass.value,
          rate: rawRate,
          inclusive: inclusive.checked,
          effective_from: fromIso,
          enabled: enabled.checked,
        };
        if (toIso) payload.effective_to = toIso;

        if (submit instanceof HTMLButtonElement) submit.disabled = true;
        try {
          if (isEdit) await ctx.api.put(`/api/v1/staff/tax-rates/${encodeURIComponent(existing.id)}`, payload);
          else await ctx.api.post("/api/v1/staff/tax-rates", payload);
          ctx.toast(isEdit ? "Rate saved" : "Rate added", { tone: "success", detail: rawRate });
          handle.close();
          await table.reload();
        } catch (err) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, rateError(err)));
        } finally {
          if (submit instanceof HTMLButtonElement) submit.disabled = false;
        }
      })();
    });
  }

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Tax rates",
      sub: "One dated row per jurisdiction. The pricer resolves the most specific rate that applies on the invoice date; a closed row keeps applying to the period it covered.",
      actions: btn("New rate", { variant: "primary", icon: "plus", onclick: () => rateDialog(null) }),
    }),
    banner(
      "info",
      "Tax is off unless it is switched on",
      h(
        "span",
        null,
        "Adding rates does not charge tax by itself: the ",
        h("code", { class: "inline-code" }, "tax.enabled"),
        " setting must be on, and it lives on the ",
        h("a", { class: "linkish", href: "/settings/system?group=tax" }, "system settings"),
        " page.",
      ),
    ),
    sourceHost,
    rows.length === 0 ? null : summaryStrip(rows),
    table.el,
  );
}

/** A one-line summary of the table: how many are open, and the classes in use. It is a
 *  reading aid, not a count the page keeps: the table owns the data. */
function summaryStrip(rows: TaxRate[]): HTMLElement {
  const open = rows.filter((row) => !row.effective_to && row.enabled).length;
  const wildcard = rows.some((row) => row.country_code === WILDCARD);
  const classes = [...new Set(rows.map((row) => row.tax_class || "standard"))].sort();
  return h(
    "div",
    { class: "row-wrap tax-summary" },
    badge(`${open} open rate${open === 1 ? "" : "s"}`, open > 0 ? "success" : "neutral", { dot: true }),
    h("span", { class: "dim" }, `${rows.length} row${rows.length === 1 ? "" : "s"} in the table`),
    wildcard ? badge("Wildcard present", "info") : null,
    h("span", { class: "dim" }, `Classes: ${classes.join(", ")}`),
  );
}

// ----------------------------------------------------------------- helpers --

/** Renders a rational rate as a percentage for display. "0.19" becomes "19%"; a fraction the
 *  server could not express exactly is shown as it is rather than rounded. */
function ratePercent(rate: string): string {
  const trimmed = (rate ?? "").trim();
  if (/^\d+(\.\d+)?$/.test(trimmed)) {
    const [whole = "0", fraction = ""] = trimmed.split(".");
    const shifted = `${whole}${(fraction + "0000").slice(0, 4)}`;
    const cut = shifted.length - 2;
    const major = shifted.slice(0, cut).replace(/^0+(?=\d)/, "");
    const minor = shifted.slice(cut).replace(/0+$/, "");
    return `${major}${minor ? `.${minor}` : ""}%`;
  }
  if (/^\d+\/\d+$/.test(trimmed)) {
    const [num = "0", den = "1"] = trimmed.split("/");
    const value = Number(num) / Number(den);
    if (Number.isFinite(value)) return `${(value * 100).toFixed(2).replace(/\.?0+$/, "")}%`;
  }
  return trimmed;
}

function today(): string {
  const now = new Date();
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())}`;
}

function isoDate(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

/** A `date` input's value as an RFC3339 instant at local midnight, or "" when empty. The
 *  server parses a time, and midnight local is what an operator means by "from this day". */
function isoFromDate(value: string): string {
  const trimmed = value.trim();
  if (!trimmed) return "";
  const date = new Date(`${trimmed}T00:00:00`);
  if (Number.isNaN(date.getTime())) return "";
  return date.toISOString();
}

function rateError(err: unknown): string {
  if (!(err instanceof ApiError)) return "The server could not be reached, so nothing was saved.";
  switch (err.code) {
    case "invalid_input":
      return err.detail || "The rate was refused. Check the country, the rate and the dates.";
    case "in_use":
      return err.detail || "This rate cannot be changed: it has already applied to an invoice.";
    case "not_found":
      return "That rate no longer exists. Reload the page.";
    case "forbidden":
      return "You do not hold tax:write, so the rate was not saved.";
    default:
      return err.detail || `The rate could not be saved (${err.code}).`;
  }
}
