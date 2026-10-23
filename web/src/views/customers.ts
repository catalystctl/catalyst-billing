// The customer list, and the two ways an operator creates or corrects a customer record.
//
// One row per customer, with the three numbers a support conversation starts from: how many
// invoices are open, what is owed, and whether the customer can sign in at all. "Can they see
// the portal?" is the question this screen exists to answer quickly, which is why `has_login`
// gets a badge of its own instead of being implied by an empty accounts list three clicks
// away.
//
// `outstanding` is a list of amounts, one per currency, and this page renders it as a list.
// A customer can hold a USD invoice and a EUR invoice at the same time; adding them would
// produce a number that no ledger contains and no exchange rate supports.
//
// "New customer" creates a billing account with **no password the caller knows**: the server
// stores the hash of randomness it discards, and the customer is invited to choose a password
// through the ordinary reset flow. That is why the dialog never asks for one and the success
// message says whether the invitation was queued — an account nobody can sign into and nobody
// told about is the failure mode this screen has to make visible.

import { h } from "../lib/dom";
import type { Amount } from "../lib/format";
import { dateTime, relative } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import { bindForm, openDialog } from "../ui/dialog";
import {
  badge,
  btn,
  field,
  iconBtn,
  input,
  moneyEl,
  pageHead,
  searchInput,
  select,
} from "../ui/kit";
import { createTable, type Column } from "../ui/table";

export const meta = { title: "Customers", crumb: "Customers" };

interface CustomerRow {
  id: string;
  email: string;
  name: string;
  created_at: string;
  open_invoices: number;
  outstanding: Amount[] | null;
  services: number;
  has_login: boolean;
}

type SortKey = "created_at" | "email" | "name";

interface SortChoice {
  key: SortKey;
  label: string;
  /** created_at descends (recent signups first); the name sorts ascend, which is how a
   *  directory is read. The server defaults to descending, so asc has to be asked for. */
  direction: "asc" | "desc";
}

const SORTS: SortChoice[] = [
  { key: "created_at", label: "Newest first", direction: "desc" },
  { key: "email", label: "Email A–Z", direction: "asc" },
  { key: "name", label: "Name A–Z", direction: "asc" },
];

const DEFAULT_SORT = SORTS[0] as SortChoice;

/** The server refuses a one-character search with `invalid_query` rather than scanning the
 *  table, so a one-character term is not sent at all. */
const MIN_SEARCH = 2;

/** The CSV export path for the current search.
 *
 *  Only `q` travels. The sort the operator picked on screen is a view preference, not a
 *  filter: an export that silently reordered itself because somebody clicked "Email A–Z"
 *  would be a different file with the same name, and the server refuses a parameter it
 *  does not honour rather than ignoring it. A one-character term is dropped for the same
 *  reason the list drops it — the server refuses to scan on it. */
function exportHref(term: string): string {
  const q = term.length >= MIN_SEARCH ? term : "";
  return `/api/v1/staff/exports/customers.csv${q ? `?q=${encodeURIComponent(q)}` : ""}`;
}

export default async function customersPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("customer:read:all")) {
    ctx.navigate("/dashboard", { replace: true });
    return h("div");
  }

  let term = (ctx.route.query.get("q") ?? "").trim();
  let sort = SORTS.find((choice) => choice.key === ctx.route.query.get("sort")) ?? DEFAULT_SORT;

  const queryOf = (search: string, order: SortChoice) => ({
    q: search.length >= MIN_SEARCH ? search : undefined,
    sort: order.key,
    direction: order.direction,
  });

  const hint = h(
    "span",
    { class: "faint cust-hint", hidden: term.length !== 1 },
    `Keep typing — the search needs ${MIN_SEARCH} characters.`,
  );

  const canWrite = ctx.can("customer:write:all");

  // The export is a link with `download`, not a fetch: the server streams text/csv with
  // Content-Disposition, so the browser saves it and the table never passes through
  // JavaScript. The search term travels with it, so the file is the list on screen.
  const exportLink = h(
    "a",
    {
      class: "btn btn--sm",
      href: exportHref(term),
      download: "",
      title: "Download every customer matching this search as CSV",
    },
    icon("download", 14),
    "Export CSV",
  );

  const columns: Column<CustomerRow>[] = [
    {
      key: "customer",
      label: "Customer",
      primary: true,
      render: (row) =>
        h(
          "div",
          { class: "cell-main" },
          h(
            "div",
            { class: "primary" },
            h("a", { class: "row-title", href: `/customers/${row.id}` }, row.name || row.email),
          ),
          h("div", { class: "secondary" }, row.name ? row.email : "No name on file"),
        ),
    },
    {
      key: "open_invoices",
      label: "Open invoices",
      align: "right",
      hideSmall: true,
      render: (row) =>
        // A count column reads as numbers: "0" in a dim tone scans; the word "None" in a
        // column of integers is a different kind of value in the same place.
        h("span", { class: row.open_invoices > 0 ? "mono" : "mono faint" }, String(row.open_invoices)),
    },
    {
      key: "outstanding",
      label: "Outstanding",
      align: "right",
      render: (row) => outstandingCell(row.outstanding ?? []),
    },
    {
      key: "services",
      label: "Services",
      align: "right",
      hideSmall: true,
      render: (row) =>
        h("span", { class: row.services > 0 ? "mono" : "mono faint" }, String(row.services)),
    },
    {
      key: "has_login",
      label: "Login",
      render: (row) =>
        row.has_login
          ? badge("Portal access", "success", { dot: true })
          : badge("No login", "neutral", { dot: true }),
    },
    {
      key: "created",
      label: "Created",
      align: "right",
      hideSmall: true,
      render: (row) => h("span", { class: "faint", title: dateTime(row.created_at) }, relative(row.created_at)),
    },
  ];

  // The edit action is a real control inside a clickable row, so the table's row navigation
  // leaves it alone: opening the dialog must not also navigate to the customer.
  if (canWrite) {
    columns.push({
      key: "actions",
      label: "",
      align: "right",
      width: "1%",
      render: (row) =>
        iconBtn("edit", `Edit ${row.name || row.email}`, () => void editCustomer(row)),
    });
  }

  const table = createTable<CustomerRow>(ctx, {
    path: "/api/v1/staff/customers",
    columns,
    query: queryOf(term, sort),
    limit: 25,
    rowHref: (row) => `/customers/${row.id}`,
    empty: {
      title: term ? `No customer matches “${term}”` : "No customer records yet",
      body: term
        ? "Search matches the email and the name. Widen it, or clear it to see everyone."
        : canWrite
          ? "A phone order, a manually onboarded account and a comped customer all start here. " +
            "Creating one sends the customer an invitation to choose their own password — you never " +
            "set one for them."
          : "A customer record is created by an operator with customer:write:all, or by self-" +
            "registration on the storefront. Once the record exists it appears in this list.",
      action: h(
        "div",
        { class: "row-wrap" },
        canWrite
          ? btn("New customer", { icon: "plus", size: "sm", variant: "primary", onclick: () => void createCustomer() })
          : null,
        btn("Read the audit log", { href: "/audit", icon: "shield", size: "sm" }),
        btn("Back to overview", { href: "/dashboard", icon: "home", size: "sm" }),
      ),
    },
  });

  let timer: number | undefined;
  ctx.onDispose(() => {
    if (timer !== undefined) window.clearTimeout(timer);
  });

  const applySearch = (value: string) => {
    term = value.trim();
    hint.hidden = term.length !== 1;
    exportLink.href = exportHref(term);
    if (timer !== undefined) window.clearTimeout(timer);
    timer = window.setTimeout(() => {
      // The query string keeps the filter across a refresh and makes the URL shareable.
      ctx.replaceQuery({ q: term || null, sort: sort.key });
      void table.reload(queryOf(term, sort));
    }, 250);
  };

  const search = searchInput({
    value: term,
    placeholder: "Search by name or email",
    oninput: applySearch,
  });
  // The kit's search box is a fixed 200px; in this toolbar it should take the slack.
  search.classList.add("cust-search");

  const sortSelect = select(
    SORTS.map((choice) => ({ value: choice.key, label: choice.label })),
    {
      id: "customer-sort",
      value: sort.key,
      onchange: (event: Event) => {
        const key = (event.target as HTMLSelectElement).value as SortKey;
        sort = SORTS.find((choice) => choice.key === key) ?? DEFAULT_SORT;
        ctx.replaceQuery({ sort: sort.key, q: term || null });
        void table.reload(queryOf(term, sort));
      },
    },
  );

  const toolbar = h(
    "div",
    { class: "toolbar" },
    search,
    h("label", { class: "cust-sort", for: "customer-sort" }, h("span", { class: "faint" }, "Sort"), sortSelect),
    hint,
    h(
      "div",
      { class: "toolbar-end" },
      btn("Refresh", {
        icon: "refresh",
        size: "sm",
        onclick: () => void table.reload(queryOf(term, sort)),
      }),
    ),
  );


  // ------------------------------------------------------------- dialogs --

  const createCustomer = async (): Promise<void> => {
    customerDialog(null);
  };

  /** Opens the edit dialog, loading the bill-to block first.
   *
   *  It loads before it opens on purpose. The update endpoint replaces the address when the body
   *  carries one, so opening with an empty form because a fetch failed and then submitting would
   *  wipe an address the operator never meant to touch. A failed load is reported and the dialog
   *  does not open, which is the only safe answer.
   */
  const editCustomer = async (row: CustomerRow): Promise<void> => {
    let profile: Profile | null = null;
    try {
      profile = await ctx.api.get<Profile>(`/api/v1/staff/customers/${encodeURIComponent(row.id)}/billing-profile`);
    } catch (err) {
      const detail = err instanceof Error ? err.message : "The address could not be loaded.";
      ctx.toast("The customer could not be opened", { tone: "danger", detail });
      return;
    }
    customerDialog(row, profile);
  };

  /** The create/edit dialog. `existing` null means create. */
  const customerDialog = (existing: CustomerRow | null, profile: Profile | null = null): void => {
    const email = input({
      id: "cust-email",
      type: "email",
      value: existing?.email ?? "",
      placeholder: "customer@example.com",
      required: true,
      autocomplete: "off",
    });
    const name = input({
      id: "cust-name",
      value: existing?.name ?? "",
      placeholder: "Acme Hosting Ltd",
      required: true,
      maxlength: 200,
    });
    const legalName = input({ id: "cust-legal", value: profile?.legal_name ?? "", maxlength: 200 });
    const line1 = input({ id: "cust-line1", value: profile?.address_line1 ?? "", maxlength: 200 });
    const line2 = input({ id: "cust-line2", value: profile?.address_line2 ?? "", maxlength: 200 });
    const city = input({ id: "cust-city", value: profile?.city ?? "", maxlength: 200 });
    const postal = input({ id: "cust-postal", value: profile?.postal_code ?? "", maxlength: 200 });
    const region = input({ id: "cust-region", value: profile?.region ?? "", maxlength: 200 });
    const country = input({
      id: "cust-country",
      value: profile?.country_code ?? "",
      placeholder: "DE",
      maxlength: 2,
      class: "input--mono",
    });
    const taxID = input({ id: "cust-taxid", value: profile?.tax_id ?? "", maxlength: 200 });

    // The exemption is a three-way choice, not a checkbox: an absent `tax_exempt` means "leave it
    // alone", and a checkbox could only ever send false or true — so editing an address would
    // silently un-exempt whoever had not ticked it.
    const taxChoice = select(existing ? TAX_OPTIONS : TAX_OPTIONS.slice(1), {
      id: "cust-tax",
      value: existing ? "unchanged" : "taxable",
    });
    const taxReason = input({
      id: "cust-tax-reason",
      placeholder: "EU reverse charge — VAT ID on file",
      maxlength: 200,
    });
    const reasonField = field({
      label: "Exemption reason",
      control: taxReason,
      id: "cust-tax-reason",
      hint: "Required when the account is exempt: an exemption without a reason cannot be defended.",
    });
    const syncReason = (): void => {
      reasonField.hidden = taxChoice.value !== "exempt";
    };
    taxChoice.addEventListener("change", syncReason);
    syncReason();

    const errorHost = h("div");
    const form = h(
      "form",
      { class: "stack", novalidate: true },
      h(
        "div",
        { class: "form-grid" },
        field({ label: "Email", control: email, id: "cust-email", required: true, class: "span-2" }),
        field({ label: "Display name", control: name, id: "cust-name", required: true }),
        field({ label: "Legal name", control: legalName, id: "cust-legal", hint: "Printed on invoices." }),
        field({ label: "Address", control: line1, id: "cust-line1", class: "span-2" }),
        field({ label: "Address line 2", control: line2, id: "cust-line2", class: "span-2" }),
        field({ label: "City", control: city, id: "cust-city" }),
        field({ label: "Postal code", control: postal, id: "cust-postal" }),
        field({ label: "Region", control: region, id: "cust-region" }),
        field({
          label: "Country",
          control: country,
          id: "cust-country",
          hint: "ISO 3166-1 alpha-2, upper case. It picks the tax jurisdiction.",
        }),
        field({ label: "Tax ID", control: taxID, id: "cust-taxid" }),
        field({ label: "Tax exemption", control: taxChoice, id: "cust-tax" }),
        reasonField,
      ),
    );

    const submit = btn(existing ? "Save customer" : "Create customer", {
      variant: "primary",
      type: "submit",
    });
    const cancel = btn("Cancel", { variant: "ghost" });
    bindForm(form, submit);

    const handle = openDialog({
      title: existing ? "Edit customer" : "New customer",
      subtitle: existing
        ? "Changes here never touch an invoice that has already been issued: each document keeps the details it was issued with."
        : "The account is created without a password. The customer is emailed a link to choose one, so nobody — including you — ever sets or sees their password.",
      body: h("div", { class: "stack" }, form, errorHost),
      footer: h("div", { class: "row spacer" }, cancel, submit),
      wide: true,
      locked: () => submit instanceof HTMLButtonElement && submit.disabled,
    });
    cancel.addEventListener("click", () => handle.close());

    const addressOf = (): Record<string, string> | null => {
      const fields: Record<string, string> = {
        legal_name: legalName.value.trim(),
        address_line1: line1.value.trim(),
        address_line2: line2.value.trim(),
        city: city.value.trim(),
        postal_code: postal.value.trim(),
        region: region.value.trim(),
        country_code: country.value.trim().toUpperCase(),
        tax_id: taxID.value.trim(),
      };
      // No address at all means "store none", which is the honest state for a customer an operator
      // only knows an address for: an empty profile row would claim someone cleared fields.
      return Object.values(fields).some((value) => value !== "") ? fields : null;
    };

    form.addEventListener("submit", (event) => {
      event.preventDefault();
      void (async () => {
        errorHost.replaceChildren();
        if (!email.value.trim() || !name.value.trim()) {
          errorHost.replaceChildren(
            h("div", { class: "banner banner--danger" }, "An email address and a display name are required."),
          );
          return;
        }
        const address = addressOf();
        const payload: Record<string, unknown> = {
          email: email.value.trim(),
          name: name.value.trim(),
        };
        if (address) payload.billing_address = address;
        if (taxChoice.value !== "unchanged") {
          payload.tax_exempt = taxChoice.value === "exempt";
          payload.tax_exemption_reason = taxChoice.value === "exempt" ? taxReason.value.trim() : "";
        }

        if (submit instanceof HTMLButtonElement) submit.disabled = true;
        try {
          if (existing) {
            await ctx.api.put(`/api/v1/staff/customers/${encodeURIComponent(existing.id)}`, payload);
            ctx.toast("Customer saved", {
              tone: "success",
              detail: "Issued invoices keep the details they were issued with.",
            });
          } else {
            const created = await ctx.api.post<{ invitation_sent?: boolean; invitation_detail?: string }>(
              "/api/v1/staff/customers",
              payload,
            );
            if (created.invitation_sent) {
              ctx.toast("Customer created", {
                tone: "success",
                detail: "They have been emailed a link to choose their password.",
              });
            } else {
              ctx.toast("Customer created, but the invitation was not sent", {
                tone: "info",
                detail:
                  created.invitation_detail ||
                  "Send them a password reset from their security page once mail is working.",
              });
            }
          }
          handle.close();
          void table.reload(queryOf(term, sort));
        } catch (err) {
          errorHost.replaceChildren(h("div", { class: "banner banner--danger" }, customerError(err)));
        } finally {
          if (submit instanceof HTMLButtonElement) submit.disabled = false;
        }
      })();
    });
  };

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Customers",
      sub: "Every customer record on the platform, with what each one owes per currency.",
      actions: h(
        "div",
        { class: "row" },
        exportLink,
        btn("Audit log", { href: "/audit", icon: "shield", size: "sm" }),
        canWrite
          ? btn("New customer", {
              variant: "primary",
              icon: "plus",
              size: "sm",
              onclick: () => void createCustomer(),
            })
          : null,
      ),
    }),
    h("div", { class: "stack" }, toolbar, table.el),
  );
}

/** The tax exemption is a three-way choice: the API's partial edit has an "absent means unchanged"
 *  state, and a checkbox has no way to express it. */
type TaxChoice = "unchanged" | "taxable" | "exempt";

const TAX_OPTIONS: Array<{ value: TaxChoice; label: string }> = [
  { value: "unchanged", label: "Leave unchanged" },
  { value: "taxable", label: "Taxable" },
  { value: "exempt", label: "Exempt" },
];

/** The bill-to block as the staff billing-profile endpoint returns it. */
interface Profile {
  legal_name: string;
  address_line1: string;
  address_line2: string;
  city: string;
  postal_code: string;
  region: string;
  country_code: string;
  tax_id: string;
}

/** Maps the server's stable codes to a sentence the operator can act on. The code is what the
 *  client branches on; the sentence is what a person reads. */
function customerError(err: unknown): string {
  const code = (err as { code?: string }).code ?? "";
  switch (code) {
    case "email_taken":
      return "That address already has a customer record or a login. Open that record instead.";
    case "invalid_email":
      return "That email address is not usable.";
    case "invalid_name":
      return "A display name between 1 and 200 characters is required.";
    case "invalid_profile":
      return (err as { detail?: string }).detail || "One of the address fields is not acceptable.";
    case "tax_exemption_reason_required":
      return "An exempt account needs a reason.";
    case "not_found":
      return "That customer no longer exists.";
    default:
      return (err as { detail?: string }).detail || "The customer could not be saved.";
  }
}

/** One line per currency. Never a total: the panel has no exchange rate and the ledger has
 *  no cross-currency sum. */
function outstandingCell(amounts: Amount[]): HTMLElement {
  if (amounts.length === 0) return h("span", { class: "faint" }, "Settled");
  return h("div", { class: "cust-amounts" }, ...amounts.map((amount) => moneyEl(amount)));
}
