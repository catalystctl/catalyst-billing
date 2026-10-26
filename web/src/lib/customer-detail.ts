// ADM-28 — the customer detail screen's list tabs, as pure functions.
//
// The screen itself builds DOM, so the two things that are easy to get wrong live here where
// they can be run under `node --test`:
//
//  1. **The request a tab makes.** The tickets tab asks the support queue for one customer,
//     exactly, and the credits tab asks the credit surface for one customer's grants. Both are
//     the existing staff endpoints with a `customer_id` parameter — there is no new route, and
//     no second description of a customer in the client.
//  2. **The wire shape of a billing agreement.** The provider token is the one value that must
//     never reach a browser. The staff endpoint already omits it, and these normalisers omit it
//     again: a payload that grew a token field would still produce rows without one, so the
//     failure is a missing column rather than a leaked credential.
//
// The tabs are gated by the same permissions the endpoints demand. A tab the caller cannot read
// is not rendered, because a tab that always answers 403 is a worse lie than an absent one.

/** One tab on the customer record. `permission` mirrors the handler's guard. */
export interface CustomerTab {
  key: "overview" | "invoices" | "tickets" | "credits" | "agreements" | "billing" | "activity";
  label: string;
  permission?: "ticket:read:all" | "credit:read:all" | "customer:read:all";
}

export const CUSTOMER_TABS: readonly CustomerTab[] = [
  { key: "overview", label: "Overview" },
  { key: "invoices", label: "Invoices" },
  { key: "tickets", label: "Tickets", permission: "ticket:read:all" },
  { key: "credits", label: "Credits", permission: "credit:read:all" },
  { key: "agreements", label: "Billing agreements", permission: "customer:read:all" },
  { key: "billing", label: "Billing details" },
  { key: "activity", label: "Activity" },
] as const;

export type CustomerTabKey = CustomerTab["key"];

/** The tabs this caller may read. Permission-less tabs are always present. */
export function visibleTabs(can: (permission: string) => boolean): CustomerTab[] {
  return CUSTOMER_TABS.filter((tab) => !tab.permission || can(tab.permission));
}

/** A tab key from the query string, or `overview`. An unreadable tab is not a valid key: a URL
 *  naming a tab the caller cannot see falls back rather than rendering a refusal panel. */
export function tabFromQuery(
  value: string | null | undefined,
  can: (permission: string) => boolean = () => true,
): CustomerTabKey {
  const match = visibleTabs(can).find((tab) => tab.key === value);
  return match?.key ?? "overview";
}

/** The page size every customer-detail list asks for. Enough to answer the question the tab is
 *  for ("have they written in?", "what did we grant?") without turning the record into a queue. */
export const CUSTOMER_LIST_LIMIT = 10;

/** The tickets tab's request: the support queue, scoped to this customer. */
export function customerTicketsQuery(customerId: string, limit = CUSTOMER_LIST_LIMIT): {
  customer_id: string;
  limit: number;
} {
  return { customer_id: customerId, limit };
}

/** The credits tab's request: this customer's grants from the credit surface. */
export function customerCreditsQuery(customerId: string, limit = CUSTOMER_LIST_LIMIT): {
  customer_id: string;
  limit: number;
} {
  return { customer_id: customerId, limit };
}

/** One row of the tickets tab, in the shape the panel renders. */
export interface CustomerTicketRow {
  id: string;
  number: string;
  subject: string;
  status: string;
  priority: string;
  department: string;
  updated_at: string | null;
  last_reply_at: string | null;
}

/** The queue's wire payload. Exported so a caller can type the request without restating it. */
export interface CustomerTicketsPayload {
  tickets?: WireTicket[] | null;
}

interface WireTicket {
  id?: string;
  number?: string;
  subject?: string;
  status?: string;
  priority?: string;
  department?: string;
  updated_at?: string | null;
  last_reply_at?: string | null;
}

/** The queue's `{ tickets: [...] }` payload, normalised. An absent or malformed list is empty,
 *  never a throw: one bad row must not blank a tab that otherwise has rows. */
export function ticketRows(payload: CustomerTicketsPayload | null | undefined): CustomerTicketRow[] {
  const list = payload?.tickets;
  if (!Array.isArray(list)) return [];
  const out: CustomerTicketRow[] = [];
  for (const row of list) {
    if (!row || typeof row.id !== "string" || row.id === "") continue;
    out.push({
      id: row.id,
      number: row.number ?? "",
      subject: row.subject ?? "(no subject)",
      status: row.status ?? "",
      priority: row.priority ?? "",
      department: row.department ?? "",
      updated_at: row.updated_at ?? null,
      last_reply_at: row.last_reply_at ?? null,
    });
  }
  return out;
}

/** One credit grant, in the shape the panel renders. */
export interface CreditGrantRow {
  id: string;
  number: string;
  currency: string;
  amount_minor: number;
  scale: number;
  reason: string;
  status: string;
  created_at: string | null;
  created_by_email: string;
}

interface WireAmount {
  minor?: number;
  currency?: string;
  scale?: number;
}

/** The credit surface's paged payload. */
export interface CreditGrantsPayload {
  data?: WireGrant[] | null;
  next_cursor?: string | null;
}

interface WireGrant {
  id?: string;
  number?: string;
  amount?: WireAmount | null;
  reason?: string;
  status?: string;
  created_at?: string | null;
  created_by_email?: string;
}

/** The credit surface's paged `{ data: [...] }` payload, normalised. */
export function creditGrantRows(payload: CreditGrantsPayload | null | undefined): CreditGrantRow[] {
  const list = payload?.data;
  if (!Array.isArray(list)) return [];
  const out: CreditGrantRow[] = [];
  for (const row of list) {
    if (!row || typeof row.id !== "string" || row.id === "") continue;
    out.push({
      id: row.id,
      number: row.number ?? "",
      currency: row.amount?.currency ?? "",
      amount_minor: typeof row.amount?.minor === "number" ? row.amount.minor : 0,
      scale: typeof row.amount?.scale === "number" ? row.amount.scale : 2,
      reason: row.reason ?? "",
      status: row.status ?? "",
      created_at: row.created_at ?? null,
      created_by_email: row.created_by_email ?? "",
    });
  }
  return out;
}

/** A saved payment method, without the provider token. There is no field here that could carry
 *  one: the token lives on the server and never has a column on this screen. */
export interface SavedMethodRow {
  id: string;
  provider: string;
  display: string;
  brand: string;
  last4: string;
  is_default: boolean;
  expiry_month: number | null;
  expiry_year: number | null;
  last_failure_code: string;
}

/** A billing agreement (mandate), without the provider's agreement token. */
export interface BillingAgreementRow {
  id: string;
  provider: string;
  status: string;
  payment_method_id: string;
  terms_version: string;
  consent_at: string | null;
  cancelled_at: string | null;
  created_at: string | null;
}

export interface BillingAgreementsView {
  methods: SavedMethodRow[];
  agreements: BillingAgreementRow[];
  has_default: boolean;
}

/** `/staff/customers/{id}/payment-methods`: methods plus mandates. */
export interface BillingAgreementsPayload {
  methods?: WireMethod[] | null;
  agreements?: WireAgreement[] | null;
}

interface WireMethod {
  id?: string;
  provider?: string;
  display?: string;
  brand?: string;
  last4?: string;
  default?: boolean;
  expiry_month?: number;
  expiry_year?: number;
  last_failure_code?: string;
}

interface WireAgreement {
  id?: string;
  provider?: string;
  status?: string;
  payment_method_id?: string;
  terms_version?: string;
  consent_at?: string | null;
  cancelled_at?: string | null;
  created_at?: string | null;
}

/** The billing-agreements tab: `/staff/customers/{id}/payment-methods`, normalised.
 *
 *  Every field is picked explicitly rather than spread from the payload. That is the whole point:
 *  a wire object that grew `token`, `provider_token` or `provider_customer_id` would produce rows
 *  that still do not contain them. The test in `web/test/customer-detail.test.mjs` feeds a payload
 *  with all three and asserts they are absent. */
export function billingAgreements(payload: BillingAgreementsPayload | null | undefined): BillingAgreementsView {
  const methods: SavedMethodRow[] = [];
  if (Array.isArray(payload?.methods)) {
    for (const row of payload.methods) {
      if (!row || typeof row.id !== "string" || row.id === "") continue;
      methods.push({
        id: row.id,
        provider: row.provider ?? "",
        display: row.display || methodFallback(row.brand, row.last4),
        brand: row.brand ?? "",
        last4: row.last4 ?? "",
        is_default: row.default === true,
        expiry_month: typeof row.expiry_month === "number" ? row.expiry_month : null,
        expiry_year: typeof row.expiry_year === "number" ? row.expiry_year : null,
        last_failure_code: row.last_failure_code ?? "",
      });
    }
  }
  const agreements: BillingAgreementRow[] = [];
  if (Array.isArray(payload?.agreements)) {
    for (const row of payload.agreements) {
      if (!row || typeof row.id !== "string" || row.id === "") continue;
      agreements.push({
        id: row.id,
        provider: row.provider ?? "",
        status: row.status ?? "",
        payment_method_id: row.payment_method_id ?? "",
        terms_version: row.terms_version ?? "",
        consent_at: row.consent_at ?? null,
        cancelled_at: row.cancelled_at ?? null,
        created_at: row.created_at ?? null,
      });
    }
  }
  return { methods, agreements, has_default: methods.some((m) => m.is_default) };
}

function methodFallback(brand?: string, last4?: string): string {
  const name = (brand ?? "").trim();
  if (name && last4) return `${name} ending ${last4}`;
  if (last4) return `Card ending ${last4}`;
  if (name) return name;
  return "Saved payment method";
}

/** "07/2029" from the two halves, or "—". */
export function expiryLabel(month: number | null, year: number | null): string {
  if (typeof month !== "number" || typeof year !== "number") return "—";
  const mm = String(month).padStart(2, "0");
  return `${mm}/${year}`;
}

/** The status of a mandate in a sentence a support agent can act on. */
export function agreementStatusLabel(status: string): string {
  switch (status) {
    case "active":
      return "Active — may be charged automatically";
    case "pending":
      return "Awaiting the customer's confirmation";
    case "cancelled":
      return "Cancelled — no further charges";
    case "failed":
      return "Failed — the provider refused it";
    case "":
      return "—";
    default:
      return status;
  }
}
