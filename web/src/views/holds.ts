// Holds.
//
// A hold is the one instruction in this panel that says "do not act on this service". It
// outranks dunning, suspension and termination, which is why the screen is a register and not a
// button: the question it answers is "which services are we deliberately leaving alone, since
// when, and who said so".
//
// Three facts are stated on the page rather than left to be discovered:
//
//  - **A hold stops automation, not the calendar.** The held service keeps running and its
//    subscription keeps renewing, which is why every row carries the next billing date. An
//    operator who reads a hold as "this customer is not being invoiced" is reading it wrong,
//    and the page says so in as many words.
//  - **A hold moves no money.** Placing one issues no invoice; lifting one issues no catch-up
//    invoice for the period it covered, and no credit either. The confirmation says that before
//    the operator clicks, because "will this bill them for last month" is the question a
//    cautious person asks and a careless one does not.
//  - **A lapsed hold is not a hold.** A hold with an expiry that nobody renewed has stopped
//    protecting the service, so the filter offers `lapsed` explicitly instead of hiding those
//    rows in a history nobody opens.
//
// Placing and lifting use the routes that already existed
// (`POST /api/v1/staff/services/{id}/holds`, `DELETE /api/v1/staff/holds/{hold_id}`), guarded
// by `service:hold` — the permission that exists for exactly this, separate from
// `service:suspend` because a hold is the opposite instruction.

import { ApiError } from "../lib/api";
import { h } from "../lib/dom";
import { dateTime, humanStatus, relative } from "../lib/format";
import type { Ctx } from "../lib/types";
import {
  badge,
  banner,
  btn,
  field,
  input,
  pageHead,
  searchInput,
  select,
  textarea,
  withBusy,
} from "../ui/kit";
import { createTable } from "../ui/table";

export const meta = { title: "Holds", crumb: "Holds" };

// ---------------------------------------------------------------- payloads --

/** One hold, from `holdRegisterJSON` in internal/httpapi/holds_ext.go. */
interface HoldRow {
  id: string;
  service_id: string;
  service_status: string;
  generation: number;
  product_name: string;
  product_slug: string;
  customer: { id: string; name: string; email: string };
  reason: string;
  created_at: string;
  expires_at: string | null;
  expired: boolean;
  placed_by: { user_id: string; email: string; display_name: string } | null;
  subscription: { status: string; next_billing_at: string | null } | null;
  open_invoices: number;
  last_invoice_no: string;
}

interface HoldCounts {
  active: number;
  expiring_7d: number;
  lapsed: number;
  never_expires: number;
}

interface HoldPage {
  data: HoldRow[];
  next_cursor: string | null;
  counts?: HoldCounts;
}

interface ServiceRow {
  id: string;
  product: { id: string; name: string };
  status: string;
}

interface CustomerRow {
  id: string;
  email: string;
  name: string;
}

const STATUS_OPTIONS = [
  { value: "active", label: "Active holds" },
  { value: "expired", label: "Lapsed (expiry passed)" },
  { value: "all", label: "Every hold" },
];

export default async function holdsPage(ctx: Ctx): Promise<Node> {
  if (!ctx.can("service:read:all")) {
    return h(
      "div",
      { class: "page" },
      pageHead({ title: "Holds" }),
      banner(
        "danger",
        "You cannot read the holds register",
        "The register needs the service:read:all permission. Placing and lifting a hold needs service:hold.",
        btn("Back to the dashboard", { href: "/", size: "sm", variant: "ghost" }),
      ),
    );
  }

  const canHold = ctx.can("service:hold");
  let status = ctx.route.query.get("status") ?? "active";
  let search = ctx.route.query.get("q") ?? "";

  const countsHost = h("div", { class: "grid-4" });

  const empty: { title: string; body?: string; action?: Node } = { title: "", body: "" };

  const table = createTable<HoldRow>(ctx, {
    path: "/api/v1/staff/holds",
    query: { status, q: search || undefined },
    columns: [
      {
        key: "service",
        label: "Service",
        primary: true,
        render: (row) =>
          h(
            "div",
            { class: "cell-main" },
            h(
              "div",
              { class: "primary" },
              h(
                "a",
                { class: "row-title", href: `/services/${encodeURIComponent(row.service_id)}` },
                row.product_name || "Service",
              ),
              " ",
              statusChip(row.service_status),
            ),
            h(
              "div",
              { class: "secondary" },
              h(
                "a",
                { class: "linkish", href: `/customers/${encodeURIComponent(row.customer.id)}` },
                row.customer.name || row.customer.email,
              ),
              row.customer.name ? ` · ${row.customer.email}` : "",
            ),
          ),
      },
      {
        key: "reason",
        label: "Why",
        render: (row) => h("span", { class: "cancel-reason", title: row.reason }, row.reason),
      },
      {
        key: "placed",
        label: "Placed",
        hideSmall: true,
        hideMedium: true,
        render: (row) =>
          h("span", { class: "dim nowrap", title: dateTime(row.created_at) }, relative(row.created_at)),
      },
      {
        key: "by",
        label: "By",
        hideSmall: true,
        hideMedium: true,
        render: (row) =>
          row.placed_by
            ? h(
                "span",
                { class: "dim", title: row.placed_by.user_id || "not a user" },
                row.placed_by.display_name || row.placed_by.email || row.placed_by.user_id,
              )
            : h("span", { class: "faint" }, "—"),
      },
      {
        key: "expires",
        label: "Expires",
        hideSmall: true,
        render: (row) => {
          if (!row.expires_at) return h("span", { class: "faint nowrap" }, "Until cleared");
          const lapsed = row.expired;
          return h(
            "span",
            { class: lapsed ? "badge badge--warning nowrap" : "dim nowrap", title: dateTime(row.expires_at) },
            lapsed ? `Lapsed ${relative(row.expires_at)}` : relative(row.expires_at),
          );
        },
      },
      {
        key: "billing",
        label: "Billing",
        hideSmall: true,
        render: (row) => {
          const next = row.subscription?.next_billing_at;
          return h(
            "div",
            { class: "stack-sm" },
            next
              ? h("span", { class: "dim nowrap", title: dateTime(next) }, `renews ${relative(next)}`)
              : h("span", { class: "faint nowrap" }, "no schedule"),
            row.open_invoices > 0
              ? h("span", { class: "faint nowrap" }, `${row.open_invoices} open invoice${row.open_invoices === 1 ? "" : "s"}`)
              : null,
          );
        },
      },
      {
        key: "actions",
        label: "",
        align: "right",
        width: "1%",
        render: (row) =>
          canHold && !row.expired
            ? (btn("Lift", {
                size: "sm",
                variant: "ghost",
                icon: "undo",
                title: "Clear this hold. Nothing is billed and nothing is credited.",
                onclick: () => void lift(row),
              }) as HTMLButtonElement)
            : row.expired
              ? h("span", { class: "faint nowrap" }, "Lapsed")
              : h("span", { class: "faint" }, "—"),
      },
    ],
    empty,
    rowKey: (row) => row.id,
    onLoaded: (_rows, page) => paintCounts((page as unknown as HoldPage).counts),
    footNote: "A hold is a veto on automation, not a pause on billing.",
  });

  function paintCounts(counts: HoldCounts | undefined): void {
    const c = counts ?? { active: 0, expiring_7d: 0, lapsed: 0, never_expires: 0 };
    countsHost.replaceChildren(
      stat("Active holds", c.active, "services automation must not touch"),
      stat("Expiring this week", c.expiring_7d, "a lapsed hold stops protecting"),
      stat("Until cleared", c.never_expires, "no expiry: a human must lift it"),
      stat("Lapsed", c.lapsed, "expiry passed, hold still on the row"),
    );
  }
  paintCounts(undefined);

  // ------------------------------------------------------------- mutations --

  async function lift(row: HoldRow): Promise<void> {
    const ok = await ctx.confirm({
      title: `Lift the hold on ${row.product_name || "this service"}?`,
      body: h(
        "div",
        { class: "stack-sm" },
        h("p", null, "Automation may act on this service again: dunning can escalate, and a suspension or termination that the hold was blocking becomes possible."),
        h(
          "p",
          { class: "field-hint" },
          "Nothing is billed and nothing is credited. Lifting a hold does not issue a catch-up invoice for the period it covered — the subscription simply renews on its normal schedule.",
        ),
        h("p", { class: "field-hint" }, `The reason on the hold: “${row.reason}”`),
      ),
      confirmLabel: "Lift the hold",
    });
    if (!ok) return;
    await withBusy(ctx, null, async () => {
      try {
        await ctx.api.del(`/api/v1/staff/holds/${encodeURIComponent(row.id)}`);
        ctx.toast("Hold lifted", {
          tone: "success",
          detail: "Nothing was billed for the period it covered.",
        });
        await table.reload();
      } catch (err) {
        report(err, "The hold could not be lifted");
      }
    });
  }

  function placeHold(): void {
    const customerSearch = input({
      id: "hold-customer",
      placeholder: "customer@example.com or a name",
      autocomplete: "off",
    });
    const customerResults = h("div", { class: "stack-sm" });
    const serviceSelect = select([{ value: "", label: "Choose a customer first" }], {
      id: "hold-service",
    });
    const reason = textarea({
      id: "hold-reason",
      placeholder: "Dispute over the June invoice. Do not suspend or terminate while support investigates.",
      maxlength: 500,
    });
    const expiresHours = input({
      id: "hold-expiry",
      type: "number",
      min: 1,
      max: 8760,
      step: 1,
      inputmode: "numeric",
      placeholder: "Leave empty to hold until a human lifts it",
    });

    let selectedCustomer = "";

    const pickCustomer = async (customer: CustomerRow): Promise<void> => {
      selectedCustomer = customer.id;
      customerResults.replaceChildren(
        h("span", { class: "badge badge--info" }, `${customer.name || customer.email} · ${customer.email}`),
      );
      serviceSelect.replaceChildren(h("option", { value: "" }, "Loading services…"));
      try {
        const page = await ctx.api.get<{ data: ServiceRow[] }>("/api/v1/staff/services", {
          query: { customer_id: customer.id, limit: 50 },
          cache: false,
        });
        const rows = page.data ?? [];
        if (rows.length === 0) {
          serviceSelect.replaceChildren(h("option", { value: "" }, "This customer has no services"));
          return;
        }
        serviceSelect.replaceChildren(
          ...rows.map((row) =>
            h(
              "option",
              { value: row.id },
              `${row.product.name} · ${humanStatus(row.status)} · ${row.id.slice(0, 8)}`,
            ),
          ),
        );
      } catch (err) {
        serviceSelect.replaceChildren(h("option", { value: "" }, "The services could not be read"));
        report(err, "The customer's services could not be read");
      }
    };

    const runSearch = async (term: string): Promise<void> => {
      const q = term.trim();
      if (q.length < 2) {
        customerResults.replaceChildren(h("span", { class: "field-hint" }, "Type at least two characters."));
        return;
      }
      try {
        const page = await ctx.api.get<{ data: CustomerRow[] }>("/api/v1/staff/customers", {
          query: { q, limit: 8 },
          cache: false,
        });
        const rows = page.data ?? [];
        customerResults.replaceChildren(
          ...(rows.length === 0
            ? [h("span", { class: "field-hint" }, "No customer matches that.")]
            : rows.map((row) =>
                h(
                  "button",
                  {
                    class: "chip",
                    type: "button",
                    onclick: () => void pickCustomer(row),
                  },
                  `${row.name || row.email} · ${row.email}`,
                ),
              )),
        );
      } catch (err) {
        report(err, "The customer search failed");
      }
    };

    let timer: number | undefined;
    customerSearch.addEventListener("input", () => {
      window.clearTimeout(timer);
      timer = window.setTimeout(() => void runSearch(customerSearch.value), 250);
    });

    void ctx.formDialog({
      title: "Place a hold",
      subtitle: "The hold outranks dunning, suspension and termination until it expires or a human lifts it.",
      submitLabel: "Place the hold",
      body: h(
        "div",
        { class: "stack" },
        field({
          label: "Customer",
          control: customerSearch,
          required: true,
          hint: "Search by email or name, then pick the service to hold.",
        }),
        customerResults,
        field({ label: "Service", control: serviceSelect, required: true }),
        field({
          label: "Why",
          control: reason,
          required: true,
          hint: "Required. This is what the next agent reads when they ask why the box is untouchable.",
        }),
        field({
          label: "Expires in hours",
          control: expiresHours,
          hint: "Optional. Empty means the hold lasts until a human lifts it, which is the safe default: a hold that expires on its own is a hold nobody notices has lapsed.",
        }),
      ),
      onSubmit: async () => {
        const serviceID = serviceSelect.value;
        const why = reason.value.trim();
        if (!selectedCustomer) return "Choose the customer first.";
        if (!serviceID) return "Choose the service to hold.";
        if (!why) return "A hold needs a reason.";

        let seconds = 0;
        const raw = expiresHours.value.trim();
        if (raw !== "") {
          const hours = Number(raw);
          if (!Number.isInteger(hours) || hours < 1 || hours > 8760) {
            return "Expires in hours must be a whole number between 1 and 8760, or empty.";
          }
          seconds = hours * 3600;
        }

        try {
          await ctx.api.post(`/api/v1/staff/services/${encodeURIComponent(serviceID)}/holds`, {
            reason: why,
            expires_in_seconds: seconds,
          });
          ctx.toast("Hold placed", {
            tone: "success",
            detail: "The service keeps running, and no invoice was issued.",
          });
          await table.reload();
          return undefined;
        } catch (err) {
          if (!(err instanceof ApiError)) return "The server could not be reached, so no hold was placed.";
          switch (err.code) {
            case "reason_required":
              return "A hold needs a reason.";
            case "not_found":
              return "That service no longer exists. Reload and pick it again.";
            case "forbidden":
              return "Placing a hold needs the service:hold permission.";
            default:
              return err.detail || `The hold could not be placed (${err.code}).`;
          }
        }
      },
    });
  }

  function report(err: unknown, fallback: string): void {
    if (err instanceof ApiError) {
      ctx.toast(err.detail || fallback, { tone: "danger", detail: err.code });
      return;
    }
    ctx.toast(fallback, { tone: "danger", detail: "The server could not be reached." });
  }

  describeEmpty(canHold);

  const statusPicker = select(STATUS_OPTIONS, {
    id: "hold-status",
    class: "select--sm",
    value: status,
    onchange: () => {
      status = statusPicker.value;
      ctx.replaceQuery({ status: status === "active" ? null : status });
      void table.reload({ status, q: search || undefined });
    },
  });

  const searchBox = searchInput({
    value: search,
    placeholder: "Customer or product…",
    oninput: (value) => {
      window.clearTimeout(searchTimer);
      searchTimer = window.setTimeout(() => {
        search = value.trim();
        ctx.replaceQuery({ q: search || null });
        void table.reload({ status, q: search || undefined });
      }, 250);
    },
  });
  let searchTimer: number | undefined;

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Holds",
      sub: "Services an operator has told automation to leave alone. A hold outranks dunning, suspension and termination.",
      actions: canHold
        ? btn("Place a hold", { variant: "primary", icon: "pauseCircle", onclick: () => placeHold() })
        : null,
    }),
    banner(
      "warning",
      "A hold stops automation, not the calendar",
      h(
        "span",
        null,
        "The held service keeps running and its subscription keeps renewing, so the “Billing” column is the honest answer to “when is this customer invoiced next”. ",
        h("strong", null, "A hold moves no money: "),
        "placing one issues no invoice, and lifting one never bills the period it covered.",
      ),
    ),
    countsHost,
    h(
      "div",
      { class: "filter-bar holds-filters" },
      field({ label: "Show", control: statusPicker, id: "hold-status" }),
      h("div", { class: "filter-bar__grow" }, searchBox),
      btn("Refresh", { size: "sm", variant: "ghost", icon: "refresh", onclick: () => void table.reload() }),
    ),
    table.el,
  );

  // ----------------------------------------------------------------- helpers --

  function describeEmpty(canWrite: boolean): void {
    empty.title = "No services are on hold";
    empty.body =
      "A hold is how an operator says “do not touch this box” while a dispute, a chargeback or a migration is open. It blocks dunning, suspension and termination without stopping the service or its billing.";
    empty.action = canWrite
      ? btn("Place a hold", { size: "sm", icon: "pauseCircle", onclick: () => placeHold() })
      : undefined;
  }

  function statusChip(value: string): HTMLElement {
    return badge(humanStatus(value), value === "active" ? "success" : value === "suspended" ? "warning" : "neutral", {
      dot: true,
    });
  }

  function stat(label: string, value: number, hint: string): HTMLElement {
    return h(
      "section",
      { class: "card stat" },
      h("span", { class: "stat-label" }, label),
      h("span", { class: "stat-value" }, String(value)),
      h("span", { class: "stat-hint" }, hint),
    );
  }
}
