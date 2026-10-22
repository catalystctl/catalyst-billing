// Creating a manual invoice.
//
// Two decisions shape this screen:
//
//  - The customer is chosen from a search of real customers, not typed as an id. The API
//    takes `customer_id`, and a mistyped uuid is a 404 at best; a picker that shows the email
//    and the open balance is what stops an invoice going to the wrong party.
//  - The client never computes a total it will send. It sends a unit price and a quantity;
//    the server's pricer computes net, tax and total (handleCreateInvoice). The running
//    subtotal below is a reading aid and is labelled as one, because a client-side total that
//    disagreed with the issued document by a cent would be worse than no total at all.
//
// The inline error cases are the server's own codes (no_lines, mixed_currency,
// invalid_currency, invalid_quantity, invalid_line): the form checks them first so the
// operator is told which line is wrong instead of receiving a 400 for the whole body.

import { ApiError } from "../lib/api";
import { money, type Amount } from "../lib/format";
import { debounce, fill, h } from "../lib/dom";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  amountFromInput,
  banner,
  btn,
  card,
  errorState,
  field,
  iconBtn,
  input,
  moneyEl,
  pageHead,
  searchInput,
  select,
  textarea,
  avatar,
} from "../ui/kit";

export const meta = { title: "New invoice", crumb: "New invoice" };

/** A row of GET /api/v1/staff/customers?q=… */
interface CustomerHit {
  id: string;
  email: string;
  name: string;
  open_invoices: number;
  outstanding: Amount[];
  has_login: boolean;
}

interface CreatedInvoice {
  id: string;
  number: string;
  status: string;
}

interface CatalogProduct {
  id: string;
  name: string;
  slug: string;
  hidden: boolean;
  prices: Array<{
    id: string;
    version: number;
    name: string;
    currency: string;
    amount: Amount;
  }>;
}

interface LineRow {
  el: HTMLElement;
  desc: HTMLInputElement;
  qty: HTMLInputElement;
  unit: HTMLInputElement;
  total: HTMLElement;
  /** Empty for an ordinary line that provisions nothing. */
  product: HTMLSelectElement;
  /** The chosen price version, when the operator picked one of the product's plans. */
  version: HTMLSelectElement;
}

export default async function invoiceNewPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("invoice:write:all")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "New invoice" }),
      banner(
        "danger",
        "You cannot create invoices",
        "Creating a manual invoice needs the invoice:write:all permission. Ask an administrator if you should have it.",
        btn("Back to invoices", { href: "/invoices", size: "sm", variant: "ghost" }),
      ),
    );
  }

  // ------------------------------------------------------------- customer --
  let selected: CustomerHit | null = null;
  let searchController: AbortController | null = null;

  const pickerHost = h("div", { class: "inv-picker" });
  const resultsHost = h("div", { class: "search-results" });
  const resultsCard = h("div", { class: "card inv-picker-results", hidden: true }, resultsHost);

  const renderSelected = () => {
    if (!selected) {
      pickerHost.replaceChildren(
        h(
          "div",
          { class: "stack-sm" },
          searchInput({
            placeholder: "Search by email or name",
            oninput: (value) => void search(value),
          }),
          resultsCard,
          h("p", { class: "field-hint" }, "Start typing: results come from the customer table, not from this browser."),
        ),
      );
      return;
    }
    pickerHost.replaceChildren(
      h(
        "div",
        { class: "inv-picked" },
        avatar(selected.name || selected.email),
        h(
          "div",
          { class: "cell-main" },
          h("div", { class: "primary" }, selected.name || selected.email),
          h(
            "div",
            { class: "secondary" },
            selected.name ? selected.email : "No display name",
            selected.open_invoices > 0 ? ` · ${selected.open_invoices} open invoice${selected.open_invoices === 1 ? "" : "s"}` : "",
          ),
        ),
        h(
          "div",
          { class: "row" },
          h("a", { class: "linkish", href: `/customers/${selected.id}` }, "Open customer"),
          btn("Change", {
            variant: "ghost",
            size: "sm",
            icon: "search",
            onclick: () => {
              selected = null;
              renderSelected();
            },
          }),
        ),
      ),
    );
  };

  const search = debounce(async (raw: string) => {
    const q = raw.trim();
    searchController?.abort();
    if (q.length === 0) {
      resultsCard.hidden = true;
      resultsHost.replaceChildren();
      return;
    }
    const controller = new AbortController();
    searchController = controller;
    resultsHost.replaceChildren(
      h("div", { class: "inv-picker-state" }, h("span", { class: "spinner" }), "Searching…"),
    );
    resultsCard.hidden = false;
    try {
      const page = await ctx.api.get<{ data: CustomerHit[]; next_cursor: string | null }>(
        "/api/v1/staff/customers",
        { query: { q, limit: 8 }, cache: false, signal: controller.signal },
      );
      if (controller.signal.aborted) return;
      if (page.data.length === 0) {
        resultsHost.replaceChildren(
          h("div", { class: "inv-picker-state" }, `No customer matches “${q}”. The customer must exist before an invoice can be issued.`),
        );
        return;
      }
      resultsHost.replaceChildren(
        ...page.data.map((hit) =>
          h(
            "button",
            {
              class: "search-hit",
              type: "button",
              onclick: () => {
                selected = hit;
                resultsCard.hidden = true;
                renderSelected();
              },
            },
            h("span", { class: "search-hit-icon" }, icon("user", 13)),
            h(
              "div",
              { class: "search-hit-body" },
              h("div", { class: "search-hit-title" }, hit.name || hit.email),
              h("div", { class: "search-hit-sub" }, hit.name ? hit.email : "No display name"),
            ),
            h(
              "span",
              { class: "search-hit-side" },
              hit.open_invoices > 0 ? `${hit.open_invoices} open` : "No open invoices",
            ),
          ),
        ),
      );
    } catch (err) {
      if (controller.signal.aborted || (err instanceof DOMException && err.name === "AbortError")) return;
      resultsHost.replaceChildren(errorState(err, () => void search(q)));
    }
  }, 250);

  ctx.onDispose(() => searchController?.abort());
  renderSelected();

  // ----------------------------------------------------------------- lines --
  // The catalog, for the per-line product picker.
  //
  // Awaited before the form is built. Firing it and filling the pickers later left the Product
  // select showing only "None" for the first second of the page's life, so an operator who
  // typed quickly got an invoice line that provisions nothing — the exact thing this form
  // exists to prevent. A short wait for the list is better than a form that silently cannot
  // sell anything.
  let products: CatalogProduct[] = [];
  try {
    const page = await ctx.api.get<{ data: CatalogProduct[] }>("/api/v1/staff/catalog/products", {
      cache: false,
    });
    products = (page.data ?? []).filter((p) => !p.hidden);
  } catch {
    // A failure here leaves the picker with only "None", which is still a usable invoice form —
    // it just cannot sell something that provisions. The invoice itself still works.
    products = [];
  }

  const rows: LineRow[] = [];
  const linesHost = h("div", { class: "lines-editor" });
  const totalsHost = h("div", { class: "doc-totals" });
  let lineSeq = 0;

  const currencyInput = input({
    id: "invoice-currency",
    value: "USD",
    placeholder: "USD",
    maxlength: 12,
    class: "input--mono",
    oninput: () => recompute(),
  });

  const currencyCode = (): string => currencyInput.value.trim().toUpperCase() || "USD";
  const scale = (): number => currencyScale(currencyCode());

  const recompute = () => {
    const currency = currencyCode();
    const lineScale = scale();
    let subtotal = 0;
    let priced = 0;
    for (const row of rows) {
      const quantity = parseQuantity(row.qty.value);
      const unitMinor = amountFromInput(row.unit.value, lineScale);
      if (quantity === null || unitMinor === null) {
        row.total.replaceChildren(h("span", { class: "faint" }, "—"));
        continue;
      }
      const lineTotal = quantity * unitMinor;
      subtotal += lineTotal;
      priced += 1;
      row.total.replaceChildren(moneyEl({ minor: lineTotal, currency, scale: lineScale }, { code: false }));
    }

    const amount: Amount = { minor: subtotal, currency, scale: lineScale };
    totalsHost.replaceChildren(
      h(
        "div",
        { class: "doc-total-row" },
        h("span", null, priced === rows.length ? "Subtotal" : "Subtotal of the priced lines"),
        moneyEl(amount),
      ),
      h(
        "div",
        { class: "doc-total-row" },
        h("span", null, "Tax"),
        h("span", { class: "faint" }, "computed on issue"),
      ),
      h(
        "div",
        { class: "doc-total-row doc-total-row--grand" },
        h("span", null, "Before tax"),
        moneyEl(amount),
      ),
      h(
        "p",
        { class: "field-hint" },
        "The server prices tax from the merchant's tax policy. This subtotal is a reading aid; the issued document is authoritative.",
      ),
    );
  };

  const addLine = (initial?: { description?: string; quantity?: string; unit?: string }) => {
    lineSeq += 1;
    const id = `invoice-line-${lineSeq}`;
    const desc = input({
      id: `${id}-desc`,
      placeholder: "Managed hosting — March",
      value: initial?.description ?? "",
      maxlength: 200,
      oninput: () => recompute(),
    });
    const qty = input({
      id: `${id}-qty`,
      type: "number",
      min: 1,
      step: 1,
      inputmode: "numeric",
      value: initial?.quantity ?? "1",
      oninput: () => recompute(),
    });
    const unit = input({
      id: `${id}-unit`,
      placeholder: "0.00",
      inputmode: "decimal",
      class: "input--mono",
      value: initial?.unit ?? "",
      oninput: () => recompute(),
    });
    const total = h("div", { class: "line-total" }, h("span", { class: "faint" }, "—"));

    // Which product this line sells. Empty is the default and means "nothing to provision":
    // support time, a prorated difference, a late fee. Choosing one is what makes a manual
    // invoice deliver a service when it is paid.
    const product = select(
      [{ value: "", label: "None" }, ...products.map((p) => ({ value: p.id, label: p.name }))],
      { id: `${id}-product` },
    );
    const version = select([{ value: "", label: "Current" }], { id: `${id}-version` });

    product.addEventListener("change", () => {
      const chosen = products.find((p) => p.id === product.value);
      // Default the description and price from the product so the common case is two clicks,
      // but leave them editable: an operator selling at a negotiated price types over it.
      if (chosen) {
        if (!desc.value.trim()) desc.value = chosen.name;
        version.replaceChildren(
          h("option", { value: "" }, "Current"),
          ...chosen.prices.map((price) =>
            h(
              "option",
              { value: price.id },
              `${price.name || `v${price.version}`} — ${price.currency} ${money(price.amount)}`,
            ),
          ),
        );
        if (chosen.prices.length > 0 && !unit.value.trim()) {
          unit.value = money(chosen.prices[0]!.amount);
          const code = chosen.prices[0]!.currency;
          if (code !== currencyCode()) {
            // A single invoice has one currency; adopting the product's is the honest move
            // rather than quietly charging in a different one than the price says.
            currencyInput.value = code;
          }
        }
      }
      version.disabled = !chosen || chosen.prices.length === 0;
      recompute();
    });

    version.addEventListener("change", () => {
      const chosen = products.find((p) => p.id === product.value);
      const price = chosen?.prices.find((p) => p.id === version.value);
      if (price) {
        unit.value = money(price.amount);
        recompute();
      }
    });

    const row: LineRow = { el: h("div", { class: "line-row" }), desc, qty, unit, total, product, version };
    // The append order is the grid order and must match the header row exactly. It is written
    // out rather than assembled from parts for that reason: a control inserted in the wrong
    // place does not error, it just silently shifts every column after it.
    row.el.append(
      field({ label: "Description", control: desc, id: `${id}-desc`, class: "field--desc" }),
      field({ label: "Qty", control: qty, id: `${id}-qty` }),
      field({ label: "Unit price", control: unit, id: `${id}-unit` }),
      field({ label: "Product", control: product, id: `${id}-product`, class: "line-product" }),
      field({ label: "Plan", control: version, id: `${id}-version`, class: "line-version" }),
      total,
      iconBtn("trash", "Remove this line", () => {
        const at = rows.indexOf(row);
        if (at >= 0) rows.splice(at, 1);
        row.el.remove();
        recompute();
      }),
    );
    rows.push(row);
    linesHost.appendChild(row.el);
    recompute();
    return row;
  };

  const addLineButton = btn("Add line", { icon: "plus", size: "sm", variant: "ghost", onclick: () => void addLine() });
  addLine();

  // --------------------------------------------------------------- options --
  const issueBox = h("input", { type: "checkbox", id: "invoice-issue-now" });
  issueBox.addEventListener("change", () => syncSubmitLabel());
  const issueField = h(
    "label",
    { class: "checkbox" },
    issueBox,
    h("span", null, "Issue immediately (allocate a number and post to the ledger)"),
  );

  const dueDays = input({
    id: "invoice-due-days",
    type: "number",
    min: 1,
    step: 1,
    value: 30,
    inputmode: "numeric",
    class: "input--mono",
  });
  const memo = textarea({
    id: "invoice-memo",
    placeholder: "Anything the customer should read on the document. Optional.",
    maxlength: 500,
  });

  const errorHost = h("div");
  const submit = btn("Create draft", { variant: "primary", type: "submit", icon: "invoice" });

  const syncSubmitLabel = () => {
    fill(submit, icon("invoice", 14), issueBox.checked ? "Create and issue" : "Create draft");
  };
  syncSubmitLabel();

  // ---------------------------------------------------------------- submit --
  const problem = (): { title: string; body: string } | null => {
    if (!selected) {
      return {
        title: "Choose a customer",
        body: "An invoice needs a billing party. Search for the customer by email or name above.",
      };
    }
    const currency = currencyCode();
    if (!/^[A-Z0-9]{2,12}$/.test(currency)) {
      return {
        title: "That currency is not valid",
        body: "A currency is 2 to 12 characters of A–Z and 0–9, for example USD or EUR.",
      };
    }
    const due = dueDays.value.trim();
    if (due !== "" && !/^\d+$/.test(due)) {
      return { title: "That due date is not a number", body: "Due in is a whole number of days, for example 30." };
    }
    if (rows.length === 0) {
      return { title: "An invoice needs at least one line", body: "Add a line with a description, a quantity and a unit price." };
    }
    for (const [index, row] of rows.entries()) {
      const label = `Line ${index + 1}`;
      if (row.desc.value.trim() === "") {
        return { title: "A line has no description", body: `${label} needs a description: it is printed on the document.` };
      }
      if (parseQuantity(row.qty.value) === null) {
        return { title: "That quantity is not usable", body: `${label} needs a whole number greater than zero.` };
      }
      if (amountFromInput(row.unit.value, scale()) === null) {
        return {
          title: "That unit price is not a number",
          body: `${label} needs a price such as 120.00. Use digits and a decimal point.`,
        };
      }
    }
    return null;
  };

  const submitInvoice = async () => {
    if (!selected) return;
    const currency = currencyCode();
    const body = {
      customer_id: selected.id,
      issue_now: issueBox.checked,
      due_in_days: dueInDays(dueDays.value),
      memo: memo.value.trim(),
      lines: rows.map((row) => ({
        description: row.desc.value.trim(),
        quantity: parseQuantity(row.qty.value) ?? 0,
        unit_amount_minor: amountFromInput(row.unit.value, scale()) ?? 0,
        currency,
        // Omitted entirely for an ordinary line, so the server stores NULL and fulfilment
        // knows there is nothing to provision.
        ...(row.product.value ? { product_id: row.product.value } : {}),
        ...(row.version.value ? { price_version_id: row.version.value } : {}),
      })),
    };

    if (submit instanceof HTMLButtonElement) submit.disabled = true;
    try {
      const created = await ctx.api.post<CreatedInvoice>("/api/v1/staff/invoices", body);
      ctx.toast(created.status === "draft" ? "Draft invoice created" : "Invoice issued", {
        tone: "success",
        detail: created.number ? `${created.number} · ${created.status}` : "No number until it is issued.",
      });
      ctx.navigate(`/invoices/${created.id}`);
    } catch (err) {
      const message =
        err instanceof ApiError
          ? INLINE_ERRORS[err.code] ?? err.detail ?? "The invoice could not be created."
          : "The server could not be reached. Nothing was created.";
      errorHost.replaceChildren(banner("danger", "Not created", message));
      if (submit instanceof HTMLButtonElement) submit.disabled = false;
    }
  };

  const form = h(
    "form",
    { class: "stack", novalidate: true },
    errorHost,
    card({
      class: "inv-customer-card",
      title: "Customer",
      sub: "Who is being billed",
      body: pickerHost,
    }),
    card({
      title: "Lines",
      sub: "One currency for the whole document",
      body: h(
        "div",
        { class: "stack" },
        field({
          label: "Currency",
          control: currencyInput,
          id: "invoice-currency",
          hint: "ISO 4217. Every line uses this currency; the server refuses a mixed document.",
        }),
        h(
          "div",
          { class: "inv-lines" },
          h(
            "div",
            { class: "line-head" },
            h("span", null, "Description"),
            h("span", null, "Qty"),
            h("span", null, "Unit price"),
            h("span", null, "Product"),
            h("span", null, "Plan"),
            h("span", { class: "num" }, "Line total"),
            h("span", null, ""),
          ),
          linesHost,
        ),
        h("div", { class: "row" }, addLineButton),
        h("div", { class: "divider" }),
        totalsHost,
      ),
    }),
    card({
      title: "Terms",
      body: h(
        "div",
        { class: "form-grid" },
        field({
          label: "Due in",
          control: dueDays,
          id: "invoice-due-days",
          hint: "Days from issue. Empty or zero means the merchant's default.",
        }),
        field({
          label: "Memo",
          control: memo,
          id: "invoice-memo",
          hint: "Optional. Recorded on the invoice and visible to the customer.",
          class: "span-2",
        }),
        h("div", { class: "span-2" }, issueField),
      ),
    }),
    h(
      "div",
      { class: "form-actions" },
      btn("Cancel", { href: "/invoices", variant: "ghost" }),
      submit,
    ),
  );

  form.addEventListener("submit", (event) => {
    event.preventDefault();
    errorHost.replaceChildren();
    const found = problem();
    if (found) {
      errorHost.replaceChildren(banner("danger", found.title, found.body));
      return;
    }
    void submitInvoice();
  });

  // ------------------------------------------------------------------ page --
  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "New invoice",
      sub: "A manual document for one customer. Recurring billing is the subscription engine's job; this is the correction and the one-off.",
      actions: btn("Back to invoices", { href: "/invoices", variant: "ghost", size: "sm", icon: "arrowLeft" }),
    }),
    h(
      "div",
      { class: "split" },
      form,
      h(
        "div",
        { class: "stack" },
        card({
          title: "Draft or issue?",
          body: h(
            "div",
            { class: "stack-sm inv-prose" },
            h(
              "p",
              null,
              h("strong", null, "A draft"),
              " takes no number and posts nothing. It is a document waiting for a decision, and you can issue it from its own page later.",
            ),
            h(
              "p",
              null,
              h("strong", null, "Issuing"),
              " allocates the next number in the series, posts to the subledger and queues the customer's notification in the same transaction. An issued invoice is immutable.",
            ),
            h(
              "p",
              null,
              "Mistakes after issue are corrected with a credit note — never by editing the document, and never by deleting it.",
            ),
          ),
        }),
        card({
          title: "Where the numbers come from",
          body: h(
            "div",
            { class: "stack-sm inv-prose" },
            h("p", null, "You set a unit price and a whole-number quantity. The server's pricer computes net, tax and total."),
            h("p", null, "A client cannot choose what to charge: the total is never sent, only the inputs to it."),
            h(
              "p",
              null,
              "Amounts are entered in the currency's major unit and sent as minor units — no floating point on either side.",
            ),
          ),
        }),
        card({
          title: "Not offered here",
          body: h(
            "p",
            { class: "inv-prose" },
            "Editing an issued line, changing a payment or deleting a document. Those endpoints do not exist, because the ledger's history is append-only.",
          ),
        }),
      ),
    ),
  );
}

const INLINE_ERRORS: Record<string, string> = {
  no_lines: "An invoice needs at least one line.",
  mixed_currency: "All lines of an invoice must use the same currency.",
  invalid_currency: "That currency code is not valid. Use 2 to 12 characters of A–Z and 0–9.",
  invalid_quantity: "Quantity must be a whole number greater than zero.",
  invalid_line: "Every line needs a description.",
  invalid_invoice: "The invoice was refused: check the customer and the lines.",
  not_found: "That customer no longer exists. Search again.",
  issuer_unavailable: "Invoice issuance is not configured on this server.",
  invalid_body: "The request was refused as malformed. Reload the page and try again.",
};

/** Days from issue, defaulting to the merchant's terms when the field is empty. */
function dueInDays(raw: string): number {
  const trimmed = raw.trim();
  return /^\d+$/.test(trimmed) && Number(trimmed) > 0 ? Number(trimmed) : 30;
}

/** A whole number of units. The API multiplies it into thousandths itself. */
function parseQuantity(raw: string): number | null {
  const trimmed = raw.trim();
  if (!/^\d+$/.test(trimmed)) return null;
  const value = Number(trimmed);
  return Number.isSafeInteger(value) && value > 0 ? value : null;
}

// Mirrors internal/money's ISO 4217 table. The server falls back to scale 2 for a code it
// does not know (staffCurrency), so an unknown code here parses the same way it will be
// stored — no client-side guess that the document would then contradict.
const ZERO_DECIMAL = new Set([
  "BIF", "CLP", "DJF", "GNF", "ISK", "JPY", "KMF", "KRW", "PYG", "RWF", "UGX", "UYI",
  "VND", "VUV", "XAF", "XOF", "XPF",
]);
const THREE_DECIMAL = new Set(["BHD", "IQD", "JOD", "KWD", "LYD", "OMR", "TND"]);
const FOUR_DECIMAL = new Set(["CLF", "UYW"]);

function currencyScale(code: string): number {
  if (ZERO_DECIMAL.has(code)) return 0;
  if (THREE_DECIMAL.has(code)) return 3;
  if (FOUR_DECIMAL.has(code)) return 4;
  return 2;
}
