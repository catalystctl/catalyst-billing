// Credit, for staff (CRD-07, E5.2).
//
// One question, answered in three halves: **who holds credit**, **what did we grant**, and
// **what did we take away**. The customer's own screen (portal-credits.ts) answers "how much do I
// have"; this one exists because an operator needs the other direction — find a customer, add
// goodwill credit, deduct credit that was granted in error, and be able to show afterwards what
// was done, by whom, and what the balance became.
//
// Three facts shape the page, and each is the server's rather than the page's:
//
//   - **A balance is derived, never stored.** `GET /api/v1/staff/credit/customers` sums the
//     `customer_credit` subledger account per currency — the same source the customer's portal
//     reads. So the page renders one line per currency and never a total: adding USD to EUR would
//     be arithmetic on unrelated quantities, and there is no exchange rate in this product.
//   - **A grant is a document.** `POST /api/v1/staff/customers/{id}/credit` issues a numbered credit
//     note with a reason that reaches the audit row. The dialog requires the reason for that
//     reason: a grant with no reason is a balance change nobody can explain in six months.
//   - **A deduction is an entry, and it needs a reason even more.**
//     `POST /api/v1/staff/customers/{id}/credit/debit` appends a journal entry of kind
//     `credit_debit`; nothing is edited, the balance can never be taken below zero (the server
//     refuses rather than clamps), and the reason is what the customer reads beside the negative
//     amount in their own credit history. A silent deduction is indistinguishable from a bug to
//     the person whose balance moved, so the dialog will not submit without one.
//
// The evidence is read back, not assumed. After a grant the page reads
// `GET /api/v1/staff/credit/grants/{id}` and shows the note, its audit rows and the balance it
// left. After a deduction it shows the audit row the server returned with the 201, because that
// row was written inside the posting's transaction and is the record rather than a description
// of it. A toast that said "done" would be the page's word for it.
//
// Two permissions, deliberately separate: `credit:read:all` reads the ledger and `credit:write`
// moves money. Support holds the first and not the second, so the screen renders for them and the
// grant and debit buttons do not.
//
// The idempotency key is minted once per dialog and reused across retries. That is what makes
// "the network dropped, press it again" safe: the same key is the same grant (or the same
// deduction), and the server replays what it already recorded instead of moving the money twice.

import { ApiError } from "../lib/api";
import { h, swap } from "../lib/dom";
import { dateTime, idempotencyKey, relative, type Amount } from "../lib/format";
import { icon } from "../lib/icons";
import type { Ctx } from "../lib/types";
import {
  amountFromInput,
  badge,
  banner,
  btn,
  card,
  field,
  input,
  metaGrid,
  moneyEl,
  pageHead,
  searchInput,
  select,
  skeletonLines,
  textarea,
} from "../ui/kit";
import { createTable, type Column } from "../ui/table";
import { loadCurrencies, scaleOf, type PublicCurrency } from "./cart";

export const meta = { title: "Credit", crumb: "Customer credit" };

/** The server refuses a one-character search rather than scanning the table, so a one-character
 *  term is not sent at all. */
const MIN_SEARCH = 2;

// ---------------------------------------------------------------- payloads --

/** `GET /api/v1/staff/credit/customers` — `handleStaffCreditCustomers` in
 *  internal/httpapi/credits_staff.go. */
interface CreditCustomer {
  id: string;
  email: string;
  name: string;
  created_at: string;
  /** One entry per currency with a positive balance. An empty array means "nothing on account",
   *  not "we could not read it". */
  balances: Amount[] | null;
}

/** `GET /api/v1/staff/credit/grants` — one goodwill grant. */
interface CreditGrant {
  id: string;
  number: string;
  customer_id: string;
  customer_email: string;
  amount: Amount;
  reason: string;
  status: string;
  created_at: string;
  created_by: string;
  created_by_email: string;
}

/** The 201 from `POST /api/v1/staff/customers/{id}/credit`.
 *
 *  `audit` is the row the grant wrote, read back after the commit. It is nullable on purpose: the
 *  grant is committed whether or not the read-back succeeded, and the page must not present a
 *  display field as a condition of success. */
interface GrantIssued {
  id: string;
  number: string;
  customer_id: string;
  amount: Amount;
  balance: Amount;
  reason: string;
  status: string;
  replayed: boolean;
  audit: AuditRow | null;
}

/** One audit row, as `handleStaffCreditGrant` returns it. */
interface AuditRow {
  id: number;
  action: string;
  outcome: string;
  actor_id: string;
  actor_email: string;
  created_at: string;
  changes: Record<string, unknown> | null;
}

/** The 201 from `POST /api/v1/staff/customers/{id}/credit/debit`.
 *
 *  `balance` is the balance the entry left behind, and `audit` is the row written inside the
 *  posting's transaction. Both come from the server; neither is computed here.
 */
interface DebitIssued {
  id: string;
  customer_id: string;
  amount: Amount;
  balance: Amount;
  reason: string;
  kind: string;
  replayed: boolean;
  audit: AuditRow | null;
}

/** `GET /api/v1/staff/credit/grants/{id}` — the note, its evidence and the balance now. */
interface GrantEvidence {
  grant: CreditGrant;
  audit: AuditRow[] | null;
  balance: Amount;
  balances: Amount[] | null;
  customer: { id: string; email: string };
}

// -------------------------------------------------------------------- page --

export default async function creditsPage(ctx: Ctx): Promise<Node> {
  // Reading the ledger is credit:read:all. A screen that cannot read it would render an error for
  // a user who has no reason to be here, so it sends them home instead.
  if (!ctx.can("credit:read:all")) {
    ctx.navigate("/", { replace: true });
    return h("div");
  }
  // Both directions of a balance change need credit:write: a grant mints credit, a deduction
  // takes it away, and they are the same money movement from opposite ends.
  const canWrite = ctx.can("credit:write");

  let term = (ctx.route.query.get("q") ?? "").trim();
  let onlyWithCredit = ctx.route.query.get("with_credit") === "true";

  // The currencies are loaded once and used for two things: the scale of a typed amount (a JPY
  // grant has no minor units) and the list of codes the dialog offers.
  let currencies: PublicCurrency[] = [];
  try {
    currencies = await loadCurrencies(ctx.api);
  } catch {
    // A failed currency read is not fatal: every amount still renders through the plain fallback,
    // and the dialog falls back to the customer's own balance or USD.
    currencies = [];
  }

  /** The evidence host: a grant's credit note, audit row and balance render here. */
  const evidenceHost = h("div");

  const queryOf = (search: string, withCredit: boolean) => ({
    q: search.length >= MIN_SEARCH ? search : undefined,
    with_credit: withCredit ? "true" : undefined,
  });

  const hint = h(
    "span",
    { class: "faint cust-hint", hidden: term.length !== 1 },
    `Keep typing — the search needs ${MIN_SEARCH} characters.`,
  );

  const columns: Column<CreditCustomer>[] = [
    {
      key: "customer",
      label: "Customer",
      primary: true,
      render: (row) =>
        h(
          "div",
          { class: "cell-main" },
          h("div", { class: "primary" }, h("a", { class: "row-title", href: `/customers/${row.id}` }, row.name || row.email)),
          h("div", { class: "secondary" }, row.name ? row.email : "No name on file"),
        ),
    },
    {
      key: "balances",
      label: "Credit on account",
      align: "right",
      render: (row) => balancesCell(row.balances ?? []),
    },
    {
      key: "created",
      label: "Customer since",
      align: "right",
      hideSmall: true,
      render: (row) => h("span", { class: "faint", title: dateTime(row.created_at) }, relative(row.created_at)),
    },
  ];

  if (canWrite) {
    columns.push({
      key: "grant",
      label: "",
      align: "right",
      width: "1%",
      // Two actions, deliberately both visible rather than one behind a menu: an operator who
      // needs to take credit away is usually looking at the row that holds it, and hiding the
      // action is how the SQL workaround starts. The debit is offered only when there is a
      // balance to take from — a button that can only be refused is worse than an absent one.
      render: (row) =>
        h(
          "div",
          { class: "row-wrap" },
          btn("Grant", { size: "sm", variant: "ghost", icon: "plus", onclick: () => void openGrantDialog(row) }),
          (row.balances ?? []).length > 0
            ? btn("Debit", { size: "sm", variant: "ghost", icon: "minus", onclick: () => void openDebitDialog(row) })
            : null,
        ),
    });
  }

  const table = createTable<CreditCustomer>(ctx, {
    path: "/api/v1/staff/credit/customers",
    columns,
    query: queryOf(term, onlyWithCredit),
    limit: 25,
    rowHref: (row) => `/customers/${row.id}`,
    empty: {
      title: term ? `No customer matches “${term}”` : onlyWithCredit ? "Nobody holds credit" : "No customer records yet",
      body: term
        ? "Search matches the email and the name. Widen it, or clear it to see everyone."
        : onlyWithCredit
          ? "No customer has a credit balance. Clear the filter to see everyone, or grant credit from a customer's row."
          : "A customer record is created outside this panel — by the CLI or the provisioning pipeline — so there is no create screen here.",
      action: h(
        "div",
        { class: "row-wrap" },
        btn("Back to overview", { href: "/", icon: "home", size: "sm" }),
        btn("Customers", { href: "/customers", icon: "users", size: "sm", variant: "ghost" }),
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
    if (timer !== undefined) window.clearTimeout(timer);
    timer = window.setTimeout(() => {
      ctx.replaceQuery({ q: term || null, with_credit: onlyWithCredit ? "true" : null });
      void table.reload(queryOf(term, onlyWithCredit));
    }, 250);
  };

  const search = searchInput({
    value: term,
    placeholder: "Search by name or email",
    oninput: applySearch,
  });
  search.classList.add("cust-search");

  const onlyToggle = h(
    "label",
    { class: "checkbox" },
    h("input", {
      type: "checkbox",
      checked: onlyWithCredit,
      onchange: (event: Event) => {
        onlyWithCredit = (event.target as HTMLInputElement).checked;
        ctx.replaceQuery({ q: term || null, with_credit: onlyWithCredit ? "true" : null });
        void table.reload(queryOf(term, onlyWithCredit));
      },
    }),
    h("span", null, "Only customers with credit"),
  );

  const toolbar = h(
    "div",
    { class: "toolbar" },
    search,
    onlyToggle,
    hint,
    h(
      "div",
      { class: "toolbar-end" },
      btn("Refresh", { icon: "refresh", size: "sm", onclick: () => void table.reload(queryOf(term, onlyWithCredit)) }),
    ),
  );

  // ------------------------------------------------------------- the grant --

  /** Opens the grant dialog for one customer.
   *
   *  The amount is validated against the currency's scale here and again on the server; checking
   *  here is about telling the operator before the round trip, not about trusting the client. The
   *  reason is required by both, because the audit row is only evidence if it says why. */
  async function openGrantDialog(row: CreditCustomer): Promise<void> {
    // The codes offered: what the customer already holds first, then the installation's default,
    // then everything configured. A grant into a new currency is legitimate — a customer refunded
    // in EUR should be credited in EUR.
    const held = (row.balances ?? []).map((amount) => amount.currency);
    const fallback = currencies.find((c) => c.is_default)?.code ?? currencies[0]?.code ?? "USD";
    const codes = [...new Set([...held, fallback, ...currencies.map((c) => c.code)])];
    const preferred = held[0] ?? fallback;

    const amount = input({
      id: "grant-amount",
      placeholder: "25.00",
      inputmode: "decimal",
      class: "input--mono",
      required: true,
    });
    const currency = select(
      codes.map((code) => ({ value: code, label: code })),
      { id: "grant-currency", value: preferred },
    );
    const reason = textarea({
      id: "grant-reason",
      placeholder: "Why is this credit being granted? The reason is recorded on the note and in the audit log.",
      maxlength: 500,
    });
    // One key per dialog, not per attempt: a retry after a dropped connection is the same grant.
    const key = idempotencyKey();

    await ctx.formDialog({
      title: "Grant credit",
      subtitle: `${row.name || row.email} · credit is a numbered note with no money behind it, so it always needs a reason.`,
      submitLabel: "Grant credit",
      body: h(
        "div",
        { class: "stack" },
        h(
          "div",
          { class: "banner banner--info" },
          icon("info", 15),
          h(
            "span",
            null,
            "This issues a credit note and posts it to the customer's credit balance. It cannot be undone from this screen; a mistake is corrected with another note.",
          ),
        ),
        h(
          "div",
          { class: "credit-deposit-form" },
          field({ label: "Amount", control: amount, id: "grant-amount", required: true }),
          field({ label: "Currency", control: currency, id: "grant-currency" }),
        ),
        field({
          label: "Reason",
          control: reason,
          id: "grant-reason",
          required: true,
          hint: "Recorded on the credit note and in the audit row. Write it for the person who reads it in a year.",
        }),
      ),
      onSubmit: async () => {
        const code = currency.value.toUpperCase();
        const scale = scaleOf(currencies, code);
        const minor = amountFromInput(amount.value, scale);
        if (minor === null || minor <= 0) {
          return `Enter an amount greater than zero, e.g. ${scale === 0 ? "500" : "25.00"}.`;
        }
        const why = reason.value.trim();
        if (!why) {
          return "A grant needs a reason: the note and the audit row both carry it.";
        }

        let issued: GrantIssued;
        try {
          issued = await ctx.api.post<GrantIssued>(
            `/api/v1/staff/customers/${encodeURIComponent(row.id)}/credit`,
            { amount_minor: minor, currency: code, reason: why },
            { idempotencyKey: key },
          );
        } catch (err) {
          if (!(err instanceof ApiError)) {
            return "The server could not be reached, so no credit was granted.";
          }
          switch (err.code) {
            case "reason_required":
              return "A grant needs a reason.";
            case "invalid_amount":
              return "The amount must be positive.";
            case "invalid_currency":
              return "Choose a currency for the grant.";
            case "not_found":
              return "This customer no longer exists, so nothing was granted.";
            case "forbidden":
              return "Your account cannot grant credit (credit:write).";
            default:
              return err.detail || `The grant was refused (${err.code}).`;
          }
        }

        ctx.toast(issued.replayed ? "That grant was already recorded" : "Credit granted", {
          tone: "success",
          detail: `${issued.number} · ${issued.amount.display || issued.amount.currency} · balance ${issued.balance.display || issued.balance.currency}`,
        });
        // The list first: the balance it shows is now wrong, and a stale row beside a fresh
        // confirmation is exactly the kind of disagreement this screen exists to avoid.
        void table.reload(queryOf(term, onlyWithCredit));
        void showEvidence(issued.id);
        return undefined;
      },
    });
  }

  // ------------------------------------------------------------ the deduction --

  /** Opens the deduction dialog for one customer.
   *
   *  Three things are stated in the dialog rather than discovered afterwards: the deduction is
   *  permanent (a correction is another entry, not an undo), the balance cannot go below zero, and
   *  the reason is what the customer will read. The amount is validated here against the
   *  currency's scale so the operator is told before the round trip; the server validates it
   *  again, and the server is the one that decides.
   */
  async function openDebitDialog(row: CreditCustomer): Promise<void> {
    const held = row.balances ?? [];
    // Only currencies the customer actually holds are offered: a debit is taken from a balance,
    // and offering a currency they hold nothing in would produce a request the server must refuse.
    const codes = held.map((amount) => amount.currency);
    const preferred = codes[0] ?? currencies.find((c) => c.is_default)?.code ?? "USD";
    const heldText = held
      .map((amount) => `${amount.display || amount.currency}`)
      .join(", ");

    const amount = input({
      id: "debit-amount",
      placeholder: "10.00",
      inputmode: "decimal",
      class: "input--mono",
      required: true,
    });
    const currency = select(
      codes.map((code) => ({ value: code, label: code })),
      { id: "debit-currency", value: preferred },
    );
    const reason = textarea({
      id: "debit-reason",
      placeholder: "Why is this credit being taken away? The customer sees this on their own credit history.",
      maxlength: 500,
    });
    // One key per dialog, not per attempt: a retry after a dropped connection is the same
    // deduction, and the server replays the entry it already recorded.
    const key = idempotencyKey();

    await ctx.formDialog({
      title: "Take credit away",
      subtitle: `${row.name || row.email} · holds ${heldText}. A deduction is a new ledger entry; the customer sees the reason.`,
      submitLabel: "Debit credit",
      danger: true,
      body: h(
        "div",
        { class: "stack" },
        h(
          "div",
          { class: "banner banner--warning" },
          icon("info", 15),
          h(
            "span",
            null,
            "This posts a debit against the customer's credit balance. It cannot be undone from this screen — a mistake is corrected with another entry — and it is refused if it would take the balance below zero.",
          ),
        ),
        h(
          "div",
          { class: "credit-deposit-form" },
          field({ label: "Amount", control: amount, id: "debit-amount", required: true }),
          field({ label: "Currency", control: currency, id: "debit-currency" }),
        ),
        field({
          label: "Reason",
          control: reason,
          id: "debit-reason",
          required: true,
          hint: "Recorded on the ledger entry and in the audit row, and shown to the customer in their credit history. Write it for them, not for us.",
        }),
      ),
      onSubmit: async () => {
        const code = currency.value.toUpperCase();
        const scale = scaleOf(currencies, code);
        const minor = amountFromInput(amount.value, scale);
        if (minor === null || minor <= 0) {
          return `Enter an amount greater than zero, e.g. ${scale === 0 ? "500" : "10.00"}.`;
        }
        const why = reason.value.trim();
        if (!why) {
          return "A deduction needs a reason: the customer sees it, and the audit row carries it.";
        }

        let issued: DebitIssued;
        try {
          issued = await ctx.api.post<DebitIssued>(
            `/api/v1/staff/customers/${encodeURIComponent(row.id)}/credit/debit`,
            { amount_minor: minor, currency: code, reason: why },
            { idempotencyKey: key },
          );
        } catch (err) {
          if (!(err instanceof ApiError)) {
            return "The server could not be reached, so no credit was taken.";
          }
          switch (err.code) {
            case "insufficient_credit":
              // The server's message quotes the balance it refused against, which is the number
              // the operator needs. Replacing it with "not enough credit" would lose it.
              return err.detail || "The balance cannot support that deduction.";
            case "reason_required":
              return "A deduction needs a reason.";
            case "invalid_amount":
              return "The amount must be positive.";
            case "invalid_currency":
              return "Choose a currency for the deduction.";
            case "not_found":
              return "This customer no longer exists, so nothing was taken.";
            case "idempotency_key_reused":
              return "That request was already used for a different deduction. Close this dialog and start again.";
            case "forbidden":
              return "Your account cannot move credit (credit:write).";
            default:
              return err.detail || `The deduction was refused (${err.code}).`;
          }
        }

        ctx.toast(issued.replayed ? "That deduction was already recorded" : "Credit taken away", {
          tone: "success",
          detail: `${issued.amount.display || issued.amount.currency} · balance ${issued.balance.display || issued.balance.currency}`,
        });
        // The list first: its balance is now wrong, and a stale row beside a fresh confirmation is
        // the disagreement this screen exists to avoid.
        void table.reload(queryOf(term, onlyWithCredit));
        showDebitEvidence(issued, row);
        return undefined;
      },
    });
  }

  /** Renders the deduction the server recorded, from the response it returned.
   *
   *  No second read: the 201 carries the balance it left and the audit row written inside the
   *  posting's transaction, so what is shown is what the server committed rather than what this
   *  page hoped it did.
   */
  const showDebitEvidence = (issued: DebitIssued, row: CreditCustomer): void => {
    const audit = issued.audit;
    swap(
      evidenceHost,
      card({
        title: "Credit deducted",
        sub: `${row.name || row.email} · the entry is permanent and the reason is on the customer's own statement`,
        actions: btn("Dismiss", { size: "sm", variant: "ghost", icon: "x", onclick: () => swap(evidenceHost, h("div")) }),
        body: h(
          "div",
          { class: "stack" },
          metaGrid([
            { label: "Taken", value: moneyEl(issued.amount, { size: "lg", signed: true }) },
            { label: "Balance after", value: moneyEl(issued.balance, { size: "lg" }) },
            { label: "Entry", value: h("span", { class: "mono" }, issued.kind) },
            { label: "Reason", value: h("span", null, issued.reason) },
          ]),
          h(
            "p",
            { class: "field-hint" },
            audit
              ? `Audit row #${audit.id} · ${audit.action} · ${audit.actor_email || "system"} · ${dateTime(audit.created_at)}`
              : "The deduction is committed. Its audit row could not be read back just now, so it is not shown here; it is in the audit log.",
          ),
        ),
      }),
    );
  };

  // ------------------------------------------------------------ the evidence --

  /** Reads one grant back and renders its note, its audit rows and the balance it left. */
  async function showEvidence(grantID: string): Promise<void> {
    swap(evidenceHost, card({ title: "Reading the grant…", body: skeletonLines(3) }));
    let evidence: GrantEvidence;
    try {
      evidence = await ctx.api.get<GrantEvidence>(`/api/v1/staff/credit/grants/${encodeURIComponent(grantID)}`, {
        cache: false,
      });
    } catch (err) {
      swap(
        evidenceHost,
        banner(
          "warning",
          "The grant was made, but its evidence could not be read",
          err instanceof ApiError
            ? err.detail || "The credit note and its audit row are in the database; this screen could not read them just now."
            : "The server could not be reached. The credit note and its audit row are in the database.",
          btn("Try again", { size: "sm", variant: "ghost", icon: "refresh", onclick: () => void showEvidence(grantID) }),
        ),
      );
      return;
    }
    swap(evidenceHost, evidenceCard(evidence));
  }

  const evidenceCard = (evidence: GrantEvidence): HTMLElement => {
    const grant = evidence.grant;
    const rows = evidence.audit ?? [];
    return card({
      title: `Credit note ${grant.number}`,
      sub: `${grant.customer_email} · ${dateTime(grant.created_at)}`,
      actions: btn("Dismiss", { size: "sm", variant: "ghost", icon: "x", onclick: () => swap(evidenceHost, h("div")) }),
      body: h(
        "div",
        { class: "stack" },
        metaGrid([
          { label: "Granted", value: moneyEl(grant.amount, { size: "lg", signed: true }) },
          { label: "Balance after", value: moneyEl(evidence.balance, { size: "lg" }) },
          { label: "Status", value: badge(grant.status, grant.status === "issued" ? "success" : "neutral", { dot: true }) },
          { label: "Reason", value: h("span", null, grant.reason) },
        ]),
        h(
          "p",
          { class: "field-hint" },
          "The balance is the ledger's balance now, not a stored number and not a replay of the balance at issue time.",
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
              h("tr", null, h("th", { scope: "col" }, "Audit"), h("th", { scope: "col" }, "Operator"), h("th", { scope: "col" }, "When")),
            ),
            h(
              "tbody",
              null,
              ...(rows.length === 0
                ? [
                    h(
                      "tr",
                      null,
                      h("td", { colspan: 3 }, h("span", { class: "faint" }, "No audit rows were written against this note.")),
                    ),
                  ]
                : rows.map((row) =>
                    h(
                      "tr",
                      null,
                      h(
                        "td",
                        null,
                        h("span", { class: "mono" }, row.action),
                        row.outcome === "success" ? null : h("span", { class: "faint" }, ` ${row.outcome}`),
                      ),
                      h("td", null, row.actor_email || h("span", { class: "faint" }, "system")),
                      h("td", { title: dateTime(row.created_at) }, h("span", { class: "faint" }, relative(row.created_at))),
                    ),
                  )),
            ),
          ),
        ),
        h(
          "p",
          { class: "field-hint" },
          "The audit row is append-only and was written in the transaction that issued the note, so it is the record rather than a description of it. The note itself cannot be edited; a correction is another note.",
        ),
      ),
    });
  };

  return h(
    "div",
    { class: "page" },
    pageHead({
      title: "Credit",
      sub: "Customer credit balances, and both directions of movement: goodwill credit granted, and credit taken away. A balance is the ledger's, per currency; a grant is a numbered note and a deduction is a ledger entry with a reason.",
      actions: canWrite
        ? btn("Audit log", { href: "/audit", icon: "shield", size: "sm" })
        : btn("Customers", { href: "/customers", icon: "users", size: "sm" }),
    }),
    h("div", { class: "stack" }, evidenceHost, toolbar, table.el),
  );
}

// ----------------------------------------------------------------- helpers --

/** One line per currency, never a total.
 *
 *  A customer can hold USD credit and EUR credit at once; adding them would produce a number no
 *  ledger contains and no exchange rate supports. "Nothing on account" is said plainly rather than
 *  rendered as a zero, because a zero balance and no balance are the same thing to this screen. */
function balancesCell(amounts: Amount[]): HTMLElement {
  if (amounts.length === 0) return h("span", { class: "faint" }, "Nothing on account");
  return h("div", { class: "cust-amounts" }, ...amounts.map((amount) => moneyEl(amount)));
}

