// ADM-28 — the customer-detail tickets, credits and billing-agreement tabs.
//
// The screen is DOM, so the rules that can fail silently live in `lib/customer-detail`:
//
//   * the request each tab makes — the existing staff endpoints with an exact `customer_id`,
//     never a new route and never a client-side filter over the whole queue;
//   * the permission gate — a tab whose endpoint would answer 403 is not rendered;
//   * the wire normalisers, including the billing-agreements one. A provider token is the one
//     value that must never reach a browser: the test feeds a payload that contains three
//     token-shaped fields and asserts none of them survives into a row.
//
// The last block reads the view's source. A helper that the screen stopped calling is a unit
// test that passes while the feature is gone, which is the failure mode this file exists to
// prevent.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

import {
  agreementStatusLabel,
  billingAgreements,
  CUSTOMER_TABS,
  creditGrantRows,
  customerCreditsQuery,
  customerTicketsQuery,
  expiryLabel,
  tabFromQuery,
  ticketRows,
  visibleTabs,
} from "../.test-build/lib/customer-detail.js";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const view = readFileSync(join(root, "src/views/customer.ts"), "utf8");

// -------------------------------------------------------------------- tabs --

test("tickets, credits and billing agreements are tabs on the record", () => {
  const keys = CUSTOMER_TABS.map((tab) => tab.key);
  for (const key of ["overview", "invoices", "tickets", "credits", "agreements", "billing", "activity"]) {
    assert.ok(keys.includes(key), `missing tab ${key}`);
  }
});

test("a tab the caller cannot read is not offered", () => {
  assert.deepEqual(
    visibleTabs(() => false).map((tab) => tab.key),
    ["overview", "invoices", "billing", "activity"],
  );
  assert.deepEqual(
    visibleTabs((permission) => permission === "ticket:read:all").map((tab) => tab.key),
    ["overview", "invoices", "tickets", "billing", "activity"],
  );
  assert.deepEqual(
    visibleTabs(() => true).map((tab) => tab.key),
    ["overview", "invoices", "tickets", "credits", "agreements", "billing", "activity"],
  );
});

test("the tab query falls back to overview when the tab is not readable", () => {
  assert.equal(tabFromQuery("tickets", () => true), "tickets");
  assert.equal(tabFromQuery("tickets", () => false), "overview");
  assert.equal(tabFromQuery("not-a-tab", () => true), "overview");
  assert.equal(tabFromQuery(null, () => true), "overview");
});

// ----------------------------------------------------------------- requests --

test("the tickets tab asks the queue for this customer, exactly", () => {
  assert.deepEqual(customerTicketsQuery("0f8fad5b-d9cb-469f-a165-70867728950e"), {
    customer_id: "0f8fad5b-d9cb-469f-a165-70867728950e",
    limit: 10,
  });
});

test("the credits tab asks the credit surface for this customer", () => {
  assert.deepEqual(customerCreditsQuery("0f8fad5b-d9cb-469f-a165-70867728950e"), {
    customer_id: "0f8fad5b-d9cb-469f-a165-70867728950e",
    limit: 10,
  });
});

test("the view calls the existing staff endpoints, not a new one", () => {
  assert.match(view, /\/api\/v1\/staff\/tickets/);
  assert.match(view, /customerTicketsQuery\(/);
  assert.match(view, /\/api\/v1\/staff\/credit\/grants/);
  assert.match(view, /customerCreditsQuery\(/);
  assert.match(view, /\/api\/v1\/staff\/customers\/\$\{encodeURIComponent\(customerId\)\}\/payment-methods/);
  assert.match(view, /billingAgreements\(/);
});

// ------------------------------------------------------------------ tickets --

test("ticket rows are normalised and malformed rows are dropped", () => {
  const rows = ticketRows({
    tickets: [
      {
        id: "t1",
        number: "T-1",
        subject: "Printer on fire",
        status: "open",
        priority: "high",
        department: "general",
        updated_at: "2026-01-01T00:00:00Z",
        last_reply_at: null,
      },
      { id: "", subject: "no id" },
      { subject: "no id at all" },
    ],
  });
  assert.equal(rows.length, 1);
  assert.equal(rows[0].id, "t1");
  assert.equal(rows[0].subject, "Printer on fire");
  assert.equal(rows[0].priority, "high");
  assert.equal(rows[0].last_reply_at, null);
});

test("an empty or missing ticket payload is an empty list, not a throw", () => {
  assert.deepEqual(ticketRows({ tickets: [] }), []);
  assert.deepEqual(ticketRows({}), []);
  assert.deepEqual(ticketRows(null), []);
});

// ------------------------------------------------------------------ credits --

test("credit grants keep their amount, currency and scale", () => {
  const rows = creditGrantRows({
    data: [
      {
        id: "g1",
        number: "CN-1",
        amount: { minor: 1250, currency: "USD", scale: 2 },
        reason: "Goodwill",
        status: "issued",
        created_at: "2026-01-02T03:04:05Z",
        created_by_email: "agent@example.test",
      },
    ],
  });
  assert.equal(rows.length, 1);
  assert.deepEqual(
    { minor: rows[0].amount_minor, currency: rows[0].currency, scale: rows[0].scale },
    { minor: 1250, currency: "USD", scale: 2 },
  );
  assert.equal(rows[0].reason, "Goodwill");
  assert.equal(rows[0].created_by_email, "agent@example.test");
});

// ------------------------------------------------------- billing agreements --

test("billing agreements split methods from mandates", () => {
  const view = billingAgreements({
    methods: [
      { id: "m1", provider: "stripe", brand: "visa", last4: "4242", default: true, expiry_month: 7, expiry_year: 2029 },
      { id: "m2", provider: "stripe", brand: "mastercard", last4: "4444", default: false },
    ],
    agreements: [
      {
        id: "a1",
        provider: "stripe",
        status: "active",
        payment_method_id: "m1",
        terms_version: "2026-01",
        consent_at: "2026-01-01T00:00:00Z",
      },
    ],
  });
  assert.equal(view.methods.length, 2);
  assert.equal(view.has_default, true);
  assert.equal(view.methods[0].display, "visa ending 4242");
  assert.equal(view.agreements.length, 1);
  assert.equal(view.agreements[0].status, "active");
});

test("provider tokens and provider customer ids never reach a row", () => {
  const view = billingAgreements({
    methods: [
      {
        id: "m1",
        provider: "stripe",
        display: "Visa ending 4242",
        // None of these belong on this screen. The server omits them; the normaliser must
        // omit them again if the wire shape ever grows one.
        token: "pm_secret_token",
        provider_token: "pm_secret_token",
        provider_customer_id: "cus_secret",
        card_number: "4242424242424242",
      },
    ],
    agreements: [
      {
        id: "a1",
        provider: "stripe",
        status: "active",
        payment_method_id: "m1",
        provider_agreement_id: "seti_secret",
        provider_token: "pm_secret_token",
      },
    ],
  });
  const encoded = JSON.stringify(view);
  for (const forbidden of ["pm_secret_token", "cus_secret", "4242424242424242", "seti_secret"]) {
    assert.equal(encoded.includes(forbidden), false, `leaked ${forbidden}`);
  }
  assert.equal("token" in view.methods[0], false);
  assert.equal("provider_token" in view.methods[0], false);
  assert.equal("provider_agreement_id" in view.agreements[0], false);
});

test("the agreements view is empty rather than broken on a missing payload", () => {
  assert.deepEqual(billingAgreements(null), { methods: [], agreements: [], has_default: false });
  assert.deepEqual(billingAgreements({}), { methods: [], agreements: [], has_default: false });
});

test("method display falls back to the last four and then to a sentence", () => {
  const withLast4 = billingAgreements({ methods: [{ id: "m1", last4: "4242" }] });
  assert.equal(withLast4.methods[0].display, "Card ending 4242");
  const bare = billingAgreements({ methods: [{ id: "m1" }] });
  assert.equal(bare.methods[0].display, "Saved payment method");
});

test("expiry and agreement status read as sentences", () => {
  assert.equal(expiryLabel(7, 2029), "07/2029");
  assert.equal(expiryLabel(null, 2029), "—");
  assert.equal(agreementStatusLabel("active"), "Active — may be charged automatically");
  assert.equal(agreementStatusLabel("cancelled"), "Cancelled — no further charges");
  assert.equal(agreementStatusLabel(""), "—");
});

// ---------------------------------------------------------------- the view --

test("the screen stops presenting a grant as a link to a route that does not exist", () => {
  // `/credits` opens a grant in a drawer; there is no `/credits/grants/:id` page. The routes
  // test would catch the dead link, but pinning it here says why the row is not an anchor.
  assert.equal(view.includes("/credits/grants/"), false);
});

test("the view renders the three new tabs and does not name a provider token", () => {
  assert.match(view, /case "tickets":/);
  assert.match(view, /case "credits":/);
  assert.match(view, /case "agreements":/);
  assert.match(view, /tabItemsFor\(ctx\)/);
  for (const forbidden of ["provider_token", "provider_agreement_id", "card_number"]) {
    assert.equal(view.includes(forbidden), false, `the view mentions ${forbidden}`);
  }
});
