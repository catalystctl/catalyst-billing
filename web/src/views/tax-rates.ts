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
import { badge, banner, btn, field, input, pageHead, select } from "../ui/kit";
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
        btn("Back to the dashboard", { href: "/", size: "sm", variant: "ghost" }),
      ),
    );
  }

  let rows: TaxRate[] = [];

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
