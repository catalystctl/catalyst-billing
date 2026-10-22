// Coupons.
//
// A coupon is configuration with a history, and this screen is shaped around the one rule the
// API enforces: **there is no delete.** A retired coupon is `enabled: false`, because every
// invoice it discounted references it, and an invoice whose discount cannot be explained is
// not a valid document. So the destructive-looking action here is "Retire", the row stays, and
// the storefront stops accepting the code.
//
// Two more consequences of the wire shape (`couponJSON` in internal/httpapi/commerce_coupon.go):
//
//  - The list carries the *cap* (`max_uses`) but not the number of redemptions. "Uses" is
//    therefore read from `GET /api/v1/staff/coupons/{id}/redemptions` per row, and it shows
//    "—" until that answers. Showing the cap in a column called "Uses" would be a plausible
//    number that is not the answer to the question.
//  - `kind` cannot change on an update. Switching percentage to fixed would reinterpret every
//    historic redemption, so the server refuses it and this form disables the field when
//    editing, with the reason next to it.
//
// The amounts are exact: a percentage is a rational *string* ("10", "12.5") and a fixed
// discount is minor units plus a currency. Neither is ever a float here.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateTime, formatCurrency, plainCurrency, type Amount } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  amountFromInput,
  badge,
  banner,
  btn,
  emptyState,
  field,
  input,
  pageHead,
  select,
} from "../ui/kit";
import { bindForm, openDialog } from "../ui/dialog";
import { createTable, type Column } from "../ui/table";
import { fmtMoney, formatsOf, loadCurrencies, type PublicCurrency } from "./cart";

export const meta = { title: "Coupons", crumb: "Coupons" };

// ---------------------------------------------------------------- payloads --

/** One coupon, verbatim from `couponJSON`. `percentage` is present only for a percentage
 *  coupon; `amount_minor`/`currency` only for a fixed one. Both limits may be null. */
interface Coupon {
  id: string;
  code: string;
  description: string;
  kind: string;
  applies_to: string;
  enabled: boolean;
  product_ids: string[] | null;
  max_uses: number | null;
  max_uses_per_user: number | null;
  recurring_cycles: number | null;
  created_at: string;
  updated_at: string;
  percentage?: string;
  amount_minor?: number;
  currency?: string;
  starts_at?: string;
  expires_at?: string;
}

/** One redemption, from `handleListRedemptions`. */
interface Redemption {
  id: string;
  coupon_id: string;
  customer_id: string;
  invoice_id: string;
  subscription_id: string;
  discount: Amount;
  created_at: string;
}

interface CouponList {
  data: Coupon[];
}

interface RedemptionList {
  data: Redemption[];
}

interface ProductHit {
  id: string;
  name: string;
  slug: string;
}

const KINDS = [
  { value: "percentage", label: "Percentage off" },
  { value: "fixed", label: "Fixed amount off" },
];

const APPLIES = [
  { value: "all", label: "Price and setup fee" },
  { value: "price", label: "Recurring price only" },
  { value: "setup", label: "Setup fee only" },
];

/** How many coupons get their redemption count read on load. A coupon table is configuration,
 *  not a ledger; the cap stops a pathological table from issuing a hundred requests. */
const COUNT_LIMIT = 60;

export default async function couponsPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("coupon:read:all")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Coupons" }),
      banner(
        "danger",
        "You cannot read promotions",
        "Reading the coupon list needs the coupon:read:all permission. Ask an administrator if you should have it.",
        btn("Back to the dashboard", { href: "/", size: "sm", variant: "ghost" }),
      ),
    );
  }

  const canWrite = ctx.can("coupon:write");
  const currencies = await loadCurrencies(ctx.api).catch(() => [] as PublicCurrency[]);
  const formats = formatsOf(currencies);

  // The count and the redemption dialog both need the row's own data, so the page keeps the
  // last page of rows rather than re-reading one by id.
  const countHosts = new Map<string, HTMLElement>();
  const countCache = new Map<string, number | null>();

  const renderUses = (row: Coupon): HTMLElement => {
    const host = h("span", { class: "faint" }, countCache.has(row.id) ? countText(row, countCache.get(row.id) ?? null) : "…");
    countHosts.set(row.id, host);
    return host;
  };

  const columns: Column<Coupon>[] = [
    {
      key: "code",
      label: "Code",
      primary: true,
      render: (row) =>
        h(
          "div",
          { class: "cell-main" },
          h(
            "div",
            { class: "primary" },
            h("span", { class: "mono row-title" }, row.code),
            row.enabled ? null : badge("Retired", "neutral", { dot: true }),
          ),
          h(
            "div",
            { class: "secondary" },
            row.description || (row.product_ids && row.product_ids.length > 0 ? `${row.product_ids.length} product restriction${row.product_ids.length === 1 ? "" : "s"}` : "No description"),
          ),
        ),
    },
    {
      key: "kind",
      label: "Kind",
      hideSmall: true,
      render: (row) => h("span", { class: "dim" }, row.kind === "fixed" ? "Fixed amount" : "Percentage"),
    },
    {
      key: "value",
      label: "Value",
      align: "right",
      render: (row) => couponValue(row, formats),
    },
    {
      key: "applies",
      label: "Applies to",
      hideSmall: true,
      render: (row) => h("span", { class: "dim" }, appliesLabel(row.applies_to)),
    },
    {
      key: "uses",
      label: "Uses",
      align: "right",
      render: renderUses,
    },
    {
      key: "window",
      label: "Window",
      hideSmall: true,
      render: (row) => windowCell(row),
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
          btn("Redemptions", {
            size: "sm",
            variant: "ghost",
            icon: "users",
            onclick: () => void showRedemptions(row),
          }),
          canWrite
            ? row.enabled
              ? btn("Retire", {
                  size: "sm",
                  variant: "ghost",
                  icon: "pauseCircle",
                  title: "Stop accepting this code; the invoices it discounted keep their record",
                  onclick: () => void retire(row),
                })
              : btn("Re-enable", {
                  size: "sm",
                  variant: "ghost",
                  icon: "playCircle",
                  onclick: () => void reEnable(row),
                })
            : null,
          canWrite
            ? h(
                "button",
                {
                  class: "btn btn--ghost btn--icon btn--sm",
                  type: "button",
                  title: "Edit this coupon",
                  "aria-label": `Edit ${row.code}`,
                  onclick: () => couponDialog(row),
                },
                icon("edit", 13),
              )
            : null,
        ),
    },
  ];

  const table = createTable<Coupon>(ctx, {
    path: "/api/v1/staff/coupons",
    columns,
    // `{data}` with no cursor: a promotion list is small, so one request is the whole list.
    empty: {
      title: "No coupons yet",
      body: "A coupon is a code that reduces a price for a while. Create one, then tell the people you want to have it.",
      action: canWrite ? btn("Create the first coupon", { variant: "primary", icon: "plus", onclick: () => couponDialog(null) }) : null,
    },
    rowKey: (row) => row.id,
    footNote: "Retired codes keep their history. Nothing here is ever deleted.",
    onLoaded: (loaded) => {
      void fillCounts(loaded);
    },
  });

  /** Reads each coupon's redemption count. A failure leaves "—" rather than a zero, because
   *  "we could not read it" and "nobody used it" are different facts. */
  async function fillCounts(list: Coupon[]): Promise<void> {
    const pending = list.slice(0, COUNT_LIMIT).filter((row) => !countCache.has(row.id));
    await Promise.all(
      pending.map(async (row) => {
        try {
          const res = await ctx.api.get<RedemptionList>(
            `/api/v1/staff/coupons/${encodeURIComponent(row.id)}/redemptions`,
            { cache: false },
          );
          countCache.set(row.id, (res.data ?? []).length);
        } catch {
          countCache.set(row.id, null);
        }
        const host = countHosts.get(row.id);
        if (host) swap(host, countText(row, countCache.get(row.id) ?? null));
      }),
    );
  }

  function countText(row: Coupon, count: number | null): Node {
    if (count === null) return h("span", { class: "faint" }, "—");
    const cap = row.max_uses;
    return h(
      "span",
      { class: count > 0 ? "mono" : "mono faint" },
      cap === null || cap === undefined ? String(count) : `${count} / ${cap}`,
    );
  }

  // ------------------------------------------------------------- mutations --

  async function retire(row: Coupon): Promise<void> {
    const ok = await ctx.confirm({
      title: `Retire “${row.code}”?`,
      body: "The code stops being accepted immediately and the cart refuses it. The invoices it already discounted keep their record, and the coupon stays in this list so those invoices can be explained.",
      confirmLabel: "Retire coupon",
      danger: true,
    });
    if (!ok) return;
    await setEnabled(row, false);
  }

  async function reEnable(row: Coupon): Promise<void> {
    const ok = await ctx.confirm({
      title: `Re-enable “${row.code}”?`,
      body: "The code is accepted again, subject to its own usage cap and expiry window. If either has passed, it will still refuse at the cart.",
      confirmLabel: "Re-enable",
    });
    if (!ok) return;
    await setEnabled(row, true);
  }

  async function setEnabled(row: Coupon, enabled: boolean): Promise<void> {
    try {
      // A partial update: every absent field keeps its stored value, which is what makes a
      // one-field change safe on a promotion that has already been redeemed.
      await ctx.api.put(`/api/v1/staff/coupons/${encodeURIComponent(row.id)}`, { enabled });
      ctx.toast(enabled ? "Coupon re-enabled" : "Coupon retired", {
        tone: "success",
        detail: enabled ? row.code : `${row.code} · the code no longer applies`,
      });
      await table.reload();
    } catch (err) {
      report(err, enabled ? "The coupon could not be re-enabled" : "The coupon could not be retired");
    }
  }

  // ------------------------------------------------------------ redemptions --

  async function showRedemptions(row: Coupon): Promise<void> {
    const host = h("div", { class: "stack" }, h("div", { class: "skeleton-rows" }, h("div", { class: "skeleton skeleton-line w-80" })));
    const handle = openDialog({
      title: `Redemptions — ${row.code}`,
      subtitle: "Every use of this code, with the invoice it discounted. This is the report that answers what the promotion cost.",
      body: host,
      wide: true,
      footer: h("div", { class: "row spacer" }, btn("Close", { variant: "ghost", onclick: () => handle.close() })),
    });
    try {
      const res = await ctx.api.get<RedemptionList>(`/api/v1/staff/coupons/${encodeURIComponent(row.id)}/redemptions`, {
        cache: false,
      });
      const list = res.data ?? [];
      if (list.length === 0) {
        swap(
          host,
          emptyState({
            title: "Nobody has used this code yet",
            body: "A redemption appears here the moment an invoice is issued with the code applied.",
            icon: "tag",
          }),
        );
        return;
      }
      const total = list.reduce((sum, red) => (red.discount?.currency === list[0]?.discount?.currency ? sum + red.discount.minor : sum), 0);
      swap(
        host,
        h(
          "div",
          { class: "stack" },
          h(
            "div",
            { class: "row-wrap" },
            badge(`${list.length} redemption${list.length === 1 ? "" : "s"}`, "info"),
            h(
              "span",
              { class: "dim" },
              "Total discounted ",
              fmtMoney(list[0]?.discount ? { ...list[0].discount, minor: total } : null, formats),
            ),
          ),
          h(
            "div",
            { class: "table-wrap" },
            h(
              "table",
              { class: "table" },
              h(
                "thead",
                null,
                h(
                  "tr",
                  null,
                  h("th", { scope: "col" }, "When"),
                  h("th", { scope: "col" }, "Customer"),
                  h("th", { scope: "col" }, "Invoice"),
                  h("th", { scope: "col", class: "num" }, "Discount"),
                ),
              ),
              h(
                "tbody",
                null,
                ...list.map((red) =>
                  h(
                    "tr",
                    null,
                    h("td", null, h("span", { title: dateTime(red.created_at) }, dateTime(red.created_at))),
                    h(
                      "td",
                      null,
                      red.customer_id
                        ? h("a", { class: "linkish", href: `/customers/${red.customer_id}` }, shortRef(red.customer_id))
                        : h("span", { class: "faint" }, "—"),
                    ),
                    h(
                      "td",
                      null,
                      red.invoice_id
                        ? h("a", { class: "linkish", href: `/invoices/${red.invoice_id}` }, shortRef(red.invoice_id))
                        : h("span", { class: "faint" }, "—"),
                    ),
                    h("td", { class: "num" }, fmtMoney(red.discount, formats)),
                  ),
                ),
              ),
            ),
          ),
        ),
      );
    } catch (err) {
      swap(
        host,
        banner(
          "danger",
          "The redemptions could not be read",
          err instanceof ApiError ? err.detail || err.code : "The server could not be reached.",
        ),
      );
    }
  }

  // ---------------------------------------------------------------- dialog --

  function couponDialog(existing: Coupon | null): void {
    const isEdit = existing !== null;
    const code = input({
      id: "coupon-code",
      value: existing?.code ?? "",
      placeholder: "SAVE10",
      class: "input--mono",
      maxlength: 64,
      autocomplete: "off",
      required: true,
    });
    const description = input({
      id: "coupon-desc",
      value: existing?.description ?? "",
      placeholder: "Launch week — ten percent off",
      maxlength: 200,
    });
    const kind = select(KINDS, { id: "coupon-kind", value: existing?.kind ?? "percentage" });
    if (isEdit) kind.disabled = true;
    const percentage = input({
      id: "coupon-percentage",
      value: existing?.percentage ?? "",
      placeholder: "10",
      class: "input--mono",
      inputmode: "decimal",
    });
    const currencyCodes = currencies.length > 0 ? currencies.map((c) => c.code) : [existing?.currency ?? "USD"];
    const currency = select(
      currencyCodes.map((code) => ({ value: code, label: code })),
      { id: "coupon-currency", value: existing?.currency ?? currencyCodes[0] ?? "USD" },
    );
    const amount = input({
      id: "coupon-amount",
      value: existing?.amount_minor !== undefined ? majorOf(existing.amount_minor, existing.currency ?? "USD") : "",
      placeholder: "5.00",
      class: "input--mono",
      inputmode: "decimal",
    });
    const appliesTo = select(APPLIES, { id: "coupon-applies", value: existing?.applies_to ?? "all" });
    const maxUses = input({
      id: "coupon-max-uses",
      type: "number",
      min: 1,
      step: 1,
      value: existing?.max_uses ?? "",
      placeholder: "Unlimited",
    });
    const maxPerUser = input({
      id: "coupon-max-per-user",
      type: "number",
      min: 1,
      step: 1,
      value: existing?.max_uses_per_user ?? "",
      placeholder: "Unlimited",
    });
    const startsAt = input({ id: "coupon-starts", type: "datetime-local", value: localInput(existing?.starts_at) });
    const expiresAt = input({ id: "coupon-expires", type: "datetime-local", value: localInput(existing?.expires_at) });
    const cycles = input({
      id: "coupon-cycles",
      type: "number",
      min: 1,
      step: 1,
      value: existing?.recurring_cycles ?? "",
      placeholder: "Every cycle",
    });
    const enabled = h("input", { type: "checkbox", checked: existing?.enabled ?? true });

    // The percentage and the fixed amount are alternatives, never both. The server refuses a
    // body that carries both, so the form hides the one that does not apply.
    const percentageField = field({
      label: "Percentage",
      control: percentage,
      id: "coupon-percentage",
      hint: "An exact number, e.g. 10 or 12.5. Not a float: the server stores it as a rational.",
    });
    const amountFields = h(
      "div",
      { class: "form-grid span-2" },
      field({ label: "Amount", control: amount, id: "coupon-amount", hint: "In major units, e.g. 5.00" }),
      field({ label: "Currency", control: currency, id: "coupon-currency" }),
    );
    const syncKind = () => {
      const fixed = kind.value === "fixed";
      percentageField.hidden = fixed;
      amountFields.hidden = !fixed;
    };
    kind.addEventListener("change", syncKind);
    syncKind();

    // The product restriction. The picker needs the catalog, which is a different permission;
    // when it is not readable the ids are still accepted as text so a coupon can be restricted.
    const productHost = h("div", { class: "stack-sm" });
    const selectedProducts = new Set<string>(existing?.product_ids ?? []);
    let products: ProductHit[] | null = null;
    let productProblem = "";

    const renderProducts = () => {
      if (products === null) {
        swap(
          productHost,
          h(
            "div",
            { class: "stack-sm" },
            h("p", { class: "field-hint" }, productProblem || "No restriction: the code applies to anything in the cart."),
          ),
        );
        return;
      }
      if (products.length === 0) {
        swap(productHost, h("p", { class: "field-hint" }, "The catalog is empty, so there is nothing to restrict this code to."));
        return;
      }
      swap(
        productHost,
        h(
          "div",
          { class: "coupon-products" },
          ...products.map((product) => {
            const box = h("input", { type: "checkbox", checked: selectedProducts.has(product.id) });
            box.addEventListener("change", () => {
              if (box.checked) selectedProducts.add(product.id);
              else selectedProducts.delete(product.id);
            });
            return h(
              "label",
              { class: "coupon-product" },
              box,
              h("span", null, product.name),
              h("span", { class: "faint mono" }, product.slug),
            );
          }),
        ),
      );
    };

    void (async () => {
      try {
        const res = await ctx.api.get<{ data: ProductHit[] }>("/api/v1/staff/catalog/products", { cache: false });
        products = res.data ?? [];
      } catch (err) {
        products = null;
        productProblem =
          err instanceof ApiError && (err.status === 403 || err.status === 404)
            ? "The catalog is not readable with your permissions, so this coupon cannot be restricted to products from here. Save it unrestricted, then ask someone with catalog access to add the restriction."
            : "The catalog could not be read. Saving now leaves the coupon unrestricted.";
      }
      renderProducts();
    })();
    renderProducts();

    const errorHost = h("div");
    const body = h(
      "div",
      { class: "form-grid" },
      field({ label: "Code", control: code, id: "coupon-code", required: true, hint: "2–64 letters, digits, hyphens or underscores.", class: "span-2" }),
      field({ label: "Description", control: description, id: "coupon-desc", class: "span-2" }),
      field({
        label: "Kind",
        control: kind,
        id: "coupon-kind",
        hint: isEdit ? "The kind cannot change: it would reinterpret every historic redemption. Retire this code and create a replacement." : undefined,
      }),
      field({ label: "Applies to", control: appliesTo, id: "coupon-applies" }),
      percentageField,
      amountFields,
      field({ label: "Max uses", control: maxUses, id: "coupon-max-uses", hint: "Total redemptions. Empty means unlimited." }),
      field({ label: "Max uses per customer", control: maxPerUser, id: "coupon-max-per-user", hint: "Empty means unlimited." }),
      field({ label: "Starts", control: startsAt, id: "coupon-starts", hint: "Empty means active now." }),
      field({ label: "Expires", control: expiresAt, id: "coupon-expires", hint: "Empty means it never expires." }),
      field({
        label: "Recurring cycles",
        control: cycles,
        id: "coupon-cycles",
        hint: "How many billing cycles the discount repeats for. Empty means every cycle; 1 means the first invoice only.",
      }),
      h("div", { class: "span-2" }, h("label", { class: "switch" }, enabled, "Enabled")),
      h(
        "div",
        { class: "span-2 stack-sm" },
        h("span", { class: "field-label" }, "Product restriction"),
        productHost,
      ),
    );

    const submit = btn(isEdit ? "Save coupon" : "Create coupon", { variant: "primary", type: "submit" });
    const cancel = btn("Cancel", { variant: "ghost" });
    const form = h("form", { class: "stack", novalidate: true }, errorHost, body);
    bindForm(form, submit);

    const handle = openDialog({
      title: isEdit ? `Edit ${existing.code}` : "New coupon",
      subtitle: isEdit
        ? "A coupon's terms can be tightened but a limit cannot be cleared. To change the shape of a promotion that has been used, retire it and create a replacement."
        : "A code that reduces a price for a while. It is validated against the cart at checkout, not at creation.",
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
        const trimmed = code.value.trim().toUpperCase();
        if (!/^[A-Z0-9_-]{2,64}$/.test(trimmed)) {
          swap(
            errorHost,
            h("div", { class: "banner banner--danger" }, "A code is 2–64 characters of letters, digits, hyphens and underscores."),
          );
          return;
        }
        const fixed = kind.value === "fixed";
        const payload: Record<string, unknown> = {
          code: trimmed,
          description: description.value.trim(),
          kind: kind.value,
          applies_to: appliesTo.value,
          enabled: enabled.checked,
        };
        if (fixed) {
          const minor = amountFromInput(amount.value, scaleFor(currencies, currency.value));
          if (minor === null || minor <= 0) {
            swap(errorHost, h("div", { class: "banner banner--danger" }, "A fixed coupon needs a positive amount, e.g. 5.00."));
            return;
          }
          payload.amount_minor = minor;
          payload.currency = currency.value;
        } else {
          const raw = percentage.value.trim();
          if (!/^\d+(\.\d+)?$/.test(raw) || Number(raw) <= 0 || Number(raw) > 100) {
            swap(errorHost, h("div", { class: "banner banner--danger" }, "A percentage is a number greater than 0 and at most 100, e.g. 10 or 12.5."));
            return;
          }
          payload.percentage = raw;
        }
        for (const [key, box, label] of [
          ["max_uses", maxUses, "Max uses"],
          ["max_uses_per_user", maxPerUser, "Max uses per customer"],
          ["recurring_cycles", cycles, "Recurring cycles"],
        ] as const) {
          const raw = box.value.trim();
          if (raw === "") continue;
          const parsed = Number(raw);
          if (!Number.isInteger(parsed) || parsed < 1) {
            swap(errorHost, h("div", { class: "banner banner--danger" }, `${label} must be a whole number of at least 1, or empty.`));
            return;
          }
          payload[key] = parsed;
        }
        const starts = isoFromLocal(startsAt.value);
        if (starts === null) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "The start date is not a valid date and time."));
          return;
        }
        if (starts) payload.starts_at = starts;
        const expires = isoFromLocal(expiresAt.value);
        if (expires === null) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "The expiry is not a valid date and time."));
          return;
        }
        if (expires) payload.expires_at = expires;
        if (starts && expires && Date.parse(expires) <= Date.parse(starts)) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, "The expiry must be after the start."));
          return;
        }
        // An empty array clears the restriction on update and means "no restriction" on create.
        // `nil` would mean "leave it alone" on update, which is not what an operator who
        // unticked every box asked for.
        if (products !== null) payload.product_ids = [...selectedProducts];

        if (submit instanceof HTMLButtonElement) submit.disabled = true;
        try {
          if (isEdit) await ctx.api.put(`/api/v1/staff/coupons/${encodeURIComponent(existing.id)}`, payload);
          else await ctx.api.post("/api/v1/staff/coupons", payload);
          ctx.toast(isEdit ? "Coupon saved" : "Coupon created", { tone: "success", detail: trimmed });
          handle.close();
          await table.reload();
        } catch (err) {
          swap(errorHost, h("div", { class: "banner banner--danger" }, couponError(err)));
        } finally {
          if (submit instanceof HTMLButtonElement) submit.disabled = false;
        }
      })();
    });
  }

  function report(err: unknown, fallback: string): void {
    if (err instanceof ApiError) {
      ctx.toast(err.detail || fallback, { tone: "danger", detail: err.code });
      return;
    }
    ctx.toast(fallback, { tone: "danger", detail: "The server could not be reached." });
  }

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Coupons",
      sub: "Codes that reduce a price for a while. A code is retired, never deleted: the invoices it discounted still point at it.",
      actions: canWrite ? btn("New coupon", { variant: "primary", icon: "plus", onclick: () => couponDialog(null) }) : null,
    }),
    !canWrite
      ? banner(
          "info",
          "Read only",
          h(
            "span",
            null,
            "Changing promotions needs the ",
            h("code", { class: "inline-code" }, "coupon:write"),
            " permission.",
          ),
        )
      : null,
    // The table renders its own skeleton, empty state and error state; a list screen does not
    // need a second one around it.
    table.el,
  );
}

// ----------------------------------------------------------------- helpers --

function couponValue(row: Coupon, formats: ReturnType<typeof formatsOf>): Node {
  if (row.kind === "fixed") {
    if (row.amount_minor === undefined || !row.currency) return h("span", { class: "faint" }, "—");
    return fmtMoney({ minor: row.amount_minor, currency: row.currency, scale: scaleForFormats(formats, row.currency) }, formats);
  }
  const pct = row.percentage ?? "";
  return h("span", { class: "mono" }, `${pct}%`);
}

function appliesLabel(value: string): string {
  switch (value) {
    case "price":
      return "Recurring price";
    case "setup":
      return "Setup fee";
    default:
      return "Price and setup";
  }
}

function windowCell(row: Coupon): Node {
  const starts = row.starts_at ? dateTime(row.starts_at) : "Always";
  const expires = row.expires_at ? dateTime(row.expires_at) : "No end";
  return h(
    "div",
    { class: "cell-main" },
    h("div", { class: "secondary" }, `From ${starts}`),
    h("div", { class: "secondary" }, `Until ${expires}`),
  );
}

function shortRef(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id;
}

function scaleFor(currencies: PublicCurrency[], code: string): number {
  return currencies.find((row) => row.code.toUpperCase() === code.toUpperCase())?.scale ?? 2;
}

function scaleForFormats(formats: ReturnType<typeof formatsOf>, code: string): number {
  return formats.get(code.toUpperCase())?.scale ?? plainCurrency(code).scale;
}

/** Renders minor units as a major-unit string for an edit form, without floating point. */
function majorOf(minor: number, currency: string): string {
  const scale = plainCurrency(currency).scale;
  const digits = String(Math.abs(Math.trunc(minor))).padStart(scale + 1, "0");
  const cut = digits.length - scale;
  return `${minor < 0 ? "-" : ""}${digits.slice(0, cut)}${scale > 0 ? `.${digits.slice(cut)}` : ""}`;
}

/** A `datetime-local` value from an RFC3339 timestamp, in the browser's own zone. */
function localInput(value: string | undefined): string {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

/** An RFC3339 timestamp from a `datetime-local` value. Empty means "not set" and `null` means
 *  "not a date", which the form reports rather than sending. */
function isoFromLocal(value: string): string | null {
  const trimmed = value.trim();
  if (!trimmed) return "";
  const date = new Date(trimmed);
  if (Number.isNaN(date.getTime())) return null;
  return date.toISOString();
}

/** The documented refusals from `commerceError` and `normaliseCoupon`, in plain words. */
function couponError(err: unknown): string {
  if (!(err instanceof ApiError)) return "The server could not be reached, so nothing was saved.";
  switch (err.code) {
    case "code_taken":
      return "That code is already used by another coupon. Codes must be unique, including retired ones.";
    case "invalid_input":
      return err.detail || "The coupon was refused. Check the code, the value and the limits.";
    case "in_use":
      return err.detail || "The coupon cannot be changed in that way.";
    case "forbidden":
      return "You do not hold coupon:write, so the coupon was not saved.";
    default:
      return err.detail || `The coupon could not be saved (${err.code}).`;
  }
}
