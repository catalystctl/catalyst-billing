// Currencies.
//
// A currency row is **presentation only**: symbol, position, grouping and the minor-unit scale.
// The arithmetic lives in the money package and is never derived from this table, which is why
// this screen is safe for a storefront to read and why editing it cannot change what a price
// *is*.
//
// The screen is built around the one thing an operator cannot verify by reading a form: what the
// format actually renders. So every row — and the editor, live as you type — shows the server's
// own `example`, which is 123456 minor units formatted with the row's rules. A grouping
// separator that collides with the decimal mark is visible there before a customer sees it.
//
// Two invariants the API enforces are surfaced rather than discovered:
//
//  - **Exactly one default.** "Set default" promotes a currency and demotes the incumbent in one
//    transaction. A currency that is the default cannot be disabled; make another the default
//    first.
//  - **The scale is a migration, not an edit.** Changing it reinterprets every stored minor
//    amount (1000 is 10.00 in USD and 1000 in JPY), so the field is disabled on an existing row.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { badge, banner, btn, field, input, pageHead, select } from "../ui/kit";
import { bindForm, openDialog } from "../ui/dialog";
import { createTable, type Column } from "../ui/table";

export const meta = { title: "Currencies", crumb: "Currencies" };

// ---------------------------------------------------------------- payloads --

/** One currency, verbatim from `currencyJSON` in internal/httpapi/commerce_currency.go.
 *  `example` is the server's rendering of 123456 minor units in this row's style. */
interface Currency {
  code: string;
  name: string;
  scale: number;
  symbol: string;
  symbol_position: string;
  group_separator: string;
  decimal_mark: string;
  group_size: number;
  is_default: boolean;
  enabled: boolean;
  sort_order: number;
  example: string;
  created_at: string;
  updated_at: string;
}

interface CurrencyList {
  data: Currency[];
}

/** The scale for the common codes, so a new row starts with the right exponent rather than
 *  always 2. The server validates whatever is sent; this is a convenience, not the source of
 *  truth. */
const KNOWN_SCALE: Record<string, number> = {
  JPY: 0,
  KRW: 0,
  VND: 0,
  CLP: 0,
  ISK: 0,
  XOF: 0,
  XAF: 0,
  BHD: 3,
  KWD: 3,
  OMR: 3,
  TND: 3,
  JOD: 3,
  IQD: 3,
  LYD: 3,
};

const POSITIONS = [
  { value: "prefix", label: "Before the amount ($12.34)" },
  { value: "suffix", label: "After the amount (12,34 €)" },
];

export default async function currenciesPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("currency:write")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Currencies" }),
      banner(
        "danger",
        "You cannot configure currencies",
        "This screen needs the currency:write permission. It decides which currencies are offered and how every amount renders.",
        btn("Back to the dashboard", { href: "/", size: "sm", variant: "ghost" }),
      ),
    );
  }

  let rows: Currency[] = [];

  const columns: Column<Currency>[] = [
    {
      key: "code",
      label: "Currency",
      primary: true,
      render: (row) =>
        h(
          "div",
          { class: "cell-main" },
          h(
            "div",
            { class: "primary" },
            h("span", { class: "mono row-title" }, row.code),
            row.is_default ? badge("Default", "accent", { dot: true }) : null,
            row.enabled ? null : badge("Hidden", "neutral", { dot: true }),
          ),
          h("div", { class: "secondary" }, row.name),
        ),
    },
    {
      key: "example",
      label: "Renders as",
      render: (row) => h("span", { class: "money mono" }, row.example || "—"),
    },
    {
      key: "format",
      label: "Format",
      hideSmall: true,
      render: (row) =>
        h(
          "div",
          { class: "cell-main" },
          h("div", { class: "secondary mono" }, `${row.symbol || "∅"} ${row.symbol_position}`),
          h(
            "div",
            { class: "secondary" },
            `group ${JSON.stringify(row.group_separator)} · decimal ${JSON.stringify(row.decimal_mark)} · size ${row.group_size}`,
          ),
        ),
    },
    {
      key: "scale",
      label: "Scale",
      align: "right",
      hideSmall: true,
      render: (row) => h("span", { class: "mono" }, String(row.scale)),
    },
    {
      key: "sort",
      label: "Order",
      align: "right",
      hideSmall: true,
      render: (row) => h("span", { class: "mono faint" }, String(row.sort_order)),
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
          row.is_default
            ? h("span", { class: "faint nowrap" }, "Default")
            : btn("Set default", {
                size: "sm",
                variant: "ghost",
                icon: "checkCircle",
                title: "Make this the currency a new cart starts in",
                onclick: () => void setDefault(row),
              }),
          btn("Edit", { size: "sm", variant: "ghost", icon: "edit", onclick: () => currencyDialog(row) }),
          row.is_default
            ? null
            : btn(row.enabled ? "Hide" : "Show", {
                size: "sm",
                variant: "ghost",
                icon: row.enabled ? "eye" : "eye",
                title: row.enabled ? "Remove from the storefront switcher" : "Offer this currency again",
                onclick: () => void toggleEnabled(row),
              }),
        ),
    },
  ];

  const table = createTable<Currency>(ctx, {
    path: "/api/v1/staff/currencies",
    columns,
    empty: {
      title: "No currencies",
      body: "A price is denominated in a currency, so nothing can be sold until at least one exists. Add the currency you invoice in and mark it the default.",
      action: btn("Add a currency", { variant: "primary", icon: "plus", onclick: () => currencyDialog(null) }),
    },
    rowKey: (row) => row.code,
    footNote: "Presentation only. The scale is part of the row because a JPY amount must never render with two decimals.",
    onLoaded: (loaded) => {
      rows = loaded;
    },
  });

  // ------------------------------------------------------------- mutations --

  async function setDefault(row: Currency): Promise<void> {
    const ok = await ctx.confirm({
      title: `Make ${row.code} the default?`,
      body: `A new cart starts in ${row.code}, and the storefront opens on it. The current default is demoted in the same transaction, so there is never a moment with no default.`,
      confirmLabel: `Set ${row.code} as default`,
    });
    if (!ok) return;
    try {
      // A partial update: only the flag. The server merges the rest onto the stored row.
      await ctx.api.put(`/api/v1/staff/currencies/${encodeURIComponent(row.code)}`, { is_default: true });
      ctx.toast(`${row.code} is now the default`, { tone: "success" });
      await table.reload();
    } catch (err) {
      report(err, "The default currency could not be changed");
    }
  }

  async function toggleEnabled(row: Currency): Promise<void> {
    const next = !row.enabled;
    if (!next && row.is_default) {
      ctx.toast("The default currency cannot be hidden", {
        tone: "danger",
        detail: "Make another currency the default first.",
      });
      return;
    }
    try {
      await ctx.api.put(`/api/v1/staff/currencies/${encodeURIComponent(row.code)}`, { enabled: next });
      ctx.toast(next ? `${row.code} is offered again` : `${row.code} is hidden`, {
        tone: "success",
        detail: next ? undefined : "Existing prices in it are untouched; the switcher stops offering it.",
      });
      await table.reload();
    } catch (err) {
      report(err, next ? "The currency could not be shown" : "The currency could not be hidden");
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

  function currencyDialog(existing: Currency | null): void {
    const isEdit = existing !== null;

    const code = input({
      id: "cur-code",
      value: existing?.code ?? "",
      placeholder: "EUR",
      class: "input--mono",
      maxlength: 12,
      required: true,
      autocomplete: "off",
    });
    if (isEdit) code.disabled = true;
    const name = input({ id: "cur-name", value: existing?.name ?? "", placeholder: "Euro", maxlength: 80, required: true });
    const scale = input({
      id: "cur-scale",
      type: "number",
      min: 0,
      max: 6,
      step: 1,
      value: existing?.scale ?? 2,
      required: true,
    });
    if (isEdit) scale.disabled = true;
    const symbol = input({
      id: "cur-symbol",
      value: existing?.symbol ?? "",
      placeholder: "€",
      class: "input--mono",
      maxlength: 8,
    });
    const position = select(POSITIONS, { id: "cur-position", value: existing?.symbol_position ?? "prefix" });
    const groupSep = input({
      id: "cur-group-sep",
      value: existing?.group_separator ?? ",",
      placeholder: ",",
      class: "input--mono",
      maxlength: 3,
    });
    const decimalMark = input({
      id: "cur-decimal",
      value: existing?.decimal_mark ?? ".",
      placeholder: ".",
      class: "input--mono",
      maxlength: 3,
    });
    const groupSize = input({
      id: "cur-group-size",
      type: "number",
      min: 0,
      max: 4,
      step: 1,
      value: existing?.group_size ?? 3,
    });
    const sortOrder = input({
      id: "cur-sort",
      type: "number",
      value: existing?.sort_order ?? 0,
      step: 1,
    });
    const isDefault = h("input", { type: "checkbox", checked: existing?.is_default ?? false });
    const enabled = h("input", { type: "checkbox", checked: existing?.enabled ?? true });

    // The live preview: the same rules the server applies, so an operator sees the effect of a
    // separator change as they type rather than after a save.
    const preview = h("div", { class: "cur-preview" });
    const renderPreview = () => {
      // The digits are built here rather than with `formatMinor`, which groups with a comma of
      // its own: using it and then grouping again produced "1,,234" — a preview that lied about
      // the very thing it exists to show.
      const scaleValue = Number(scale.value) || 0;
      const raw = String(123456);
      const padded = scaleValue > 0 ? raw.padStart(scaleValue + 1, "0") : raw;
      const cut = padded.length - scaleValue;
      const wholeRaw = scaleValue > 0 ? padded.slice(0, cut) : padded;
      const fractionRaw = scaleValue > 0 ? padded.slice(cut) : "";
      const digits = scaleValue > 0 ? `${wholeRaw}.${fractionRaw}` : wholeRaw;
      const groupSizeValue = Number(groupSize.value) || 0;
      const sep = groupSep.value;
      const dec = decimalMark.value || ".";
      const [whole = "0", fraction] = digits.split(".");
      let grouped = whole;
      if (groupSizeValue > 0 && sep) {
        grouped = "";
        for (let i = 0; i < whole.length; i += 1) {
          if (i > 0 && (whole.length - i) % groupSizeValue === 0) grouped += sep;
          grouped += whole[i];
        }
      }
      const body = fraction !== undefined ? `${grouped}${dec}${fraction}` : grouped;
      const withSymbol = symbol.value
        ? position.value === "suffix"
          ? `${body} ${symbol.value}`
          : `${symbol.value}${body}`
        : body;
      swap(
        preview,
        h(
          "div",
          { class: "stack-sm" },
          h("span", { class: "field-label" }, "Preview"),
          h("span", { class: "cur-preview-value mono" }, withSymbol),
          h("span", { class: "field-hint" }, "123456 minor units, rendered with the rules on the left."),
          sep && dec && sep === dec
            ? h(
                "span",
                { class: "field-error" },
                icon("alertCircle", 11),
                "The group separator and the decimal mark are the same character. The server refuses this.",
              )
            : null,
        ),
      );
    };
    for (const control of [scale, symbol, position, groupSep, decimalMark, groupSize]) {
      control.addEventListener("input", renderPreview);
      control.addEventListener("change", renderPreview);
    }
    // A code suggests its own scale on create, once, and then leaves the field alone.
    code.addEventListener("input", () => {
      if (isEdit) return;
      const known = KNOWN_SCALE[code.value.trim().toUpperCase()];
      if (known !== undefined) scale.value = String(known);
      renderPreview();
    });
    renderPreview();

    const errorHost = h("div");
    const body = h(
      "div",
      { class: "form-grid" },
      field({
        label: "Code",
        control: code,
        id: "cur-code",
        required: true,
        hint: isEdit ? "The code is the primary key and every price references it, so it cannot be renamed." : "ISO 4217, e.g. EUR.",
      }),
      field({ label: "Name", control: name, id: "cur-name", required: true }),
      field({
        label: "Minor-unit scale",
        control: scale,
        id: "cur-scale",
        required: true,
        hint: isEdit ? "Fixed once prices exist: changing it would reinterpret every stored amount." : "2 for most currencies, 0 for JPY, 3 for KWD.",
      }),
      field({ label: "Sort order", control: sortOrder, id: "cur-sort", hint: "Lower appears first in the switcher." }),
      field({ label: "Symbol", control: symbol, id: "cur-symbol", hint: "Optional. Empty renders a bare number." }),
      field({ label: "Symbol position", control: position, id: "cur-position" }),
      field({ label: "Group separator", control: groupSep, id: "cur-group-sep", hint: "Empty means no grouping." }),
      field({ label: "Decimal mark", control: decimalMark, id: "cur-decimal" }),
      field({ label: "Group size", control: groupSize, id: "cur-group-size", hint: "Digits between separators. 0 disables grouping." }),
      h("div", { class: "span-2" }, h("label", { class: "switch" }, isDefault, "Make this the default currency")),
      h("div", { class: "span-2" }, h("label", { class: "switch" }, enabled, "Offer this currency in the storefront")),
      h("div", { class: "span-2" }, preview),
    );

    const submit = btn(isEdit ? "Save currency" : "Add currency", { variant: "primary", type: "submit" });
    const cancel = btn("Cancel", { variant: "ghost" });
    const form = h("form", { class: "stack", novalidate: true }, errorHost, body);
    bindForm(form, submit);

    const handle = openDialog({
      title: isEdit ? `Edit ${existing.code}` : "New currency",
      subtitle: "Presentation only: this decides how an amount renders, never what it is worth.",
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
        const trimmedCode = code.value.trim().toUpperCase();
        if (!isEdit && !/^[A-Z0-9]{2,12}$/.test(trimmedCode)) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "A currency code is 2–12 upper-case letters or digits, e.g. EUR."));
          return;
        }
        if (!name.value.trim()) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "A name is required."));
          return;
        }
        const scaleValue = Number(scale.value);
        if (!Number.isInteger(scaleValue) || scaleValue < 0 || scaleValue > 6) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "The scale is a whole number between 0 and 6."));
          return;
        }
        const groupSizeValue = Number(groupSize.value) || 0;
        if (!Number.isInteger(groupSizeValue) || groupSizeValue < 0 || groupSizeValue > 4) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "The group size is a whole number between 0 and 4."));
          return;
        }
        if (groupSep.value && groupSep.value === decimalMark.value) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "The group separator and the decimal mark must differ."));
          return;
        }

        const payload: Record<string, unknown> = {
          name: name.value.trim(),
          symbol: symbol.value.trim(),
          symbol_position: position.value,
          group_separator: groupSep.value,
          decimal_mark: decimalMark.value || ".",
          group_size: groupSizeValue,
          is_default: isDefault.checked,
          enabled: enabled.checked,
          sort_order: Number(sortOrder.value) || 0,
        };
        // The scale and the code are only read on create. Sending them on an update would be
        // ignored at best and refused at worst, so they are left out.
        if (!isEdit) {
          payload.code = trimmedCode;
          payload.scale = scaleValue;
        }

        if (submit instanceof HTMLButtonElement) submit.disabled = true;
        try {
          if (isEdit) await ctx.api.put(`/api/v1/staff/currencies/${encodeURIComponent(existing.code)}`, payload);
          else await ctx.api.post("/api/v1/staff/currencies", payload);
          ctx.toast(isEdit ? "Currency saved" : "Currency added", { tone: "success", detail: isEdit ? existing.code : trimmedCode });
          handle.close();
          await table.reload();
        } catch (err) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, currencyError(err)));
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
      title: "Currencies",
      sub: "Which currencies are offered, and how each one renders. The format is presentation; the arithmetic is the money package's and is not configurable.",
      actions: btn("New currency", { variant: "primary", icon: "plus", onclick: () => currencyDialog(null) }),
    }),
    banner(
      "info",
      "There is no exchange rate",
      "A price is set per currency. A cart adopts the currency of its first item and cannot mix, so adding EUR does not convert a USD price — it makes a EUR price possible.",
    ),
    rows.length === 0 ? null : defaultStrip(rows),
    table.el,
  );
}

/** Which currency is the default, said once at the top: it is the one fact every other row is
 *  read against. */
function defaultStrip(rows: Currency[]): HTMLElement {
  const def = rows.find((row) => row.is_default);
  const offered = rows.filter((row) => row.enabled).length;
  return h(
    "div",
    { class: "row-wrap cur-summary" },
    def
      ? h("span", { class: "dim" }, "Default: ", h("span", { class: "mono" }, def.code), ` — a new cart starts here, and it renders as ${def.example}`)
      : h("span", { class: "badge badge--warning" }, "No default currency — a new cart cannot be created"),
    h("span", { class: "dim" }, `${offered} of ${rows.length} offered in the storefront`),
  );
}

function currencyError(err: unknown): string {
  if (!(err instanceof ApiError)) return "The server could not be reached, so nothing was saved.";
  switch (err.code) {
    case "code_taken":
      return "That currency code is already configured.";
    case "in_use":
      return err.detail || "That change is not allowed: the currency is the default, or changing the scale would reinterpret stored amounts.";
    case "invalid_input":
      return err.detail || "The currency was refused. Check the separators and the scale.";
    case "not_found":
      return "That currency no longer exists. Reload the page.";
    case "forbidden":
      return "You do not hold currency:write, so the currency was not saved.";
    default:
      return err.detail || `The currency could not be saved (${err.code}).`;
  }
}
