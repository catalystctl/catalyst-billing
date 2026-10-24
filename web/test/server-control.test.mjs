// The live server card and the two invoice actions, asserted where a type-check cannot see them.
//
// These are static guards, in the same spirit as styles.test.mjs: they read the view sources and
// assert the shape of the client. `tsc` genuinely cannot see any of the properties below, and each
// one is a defect in the product rather than in the types:
//
//   * A hard-coded button list and a `can_power`-filtered list compile identically. The difference
//     is whether a customer is offered an action their provider would refuse — a 409 hiding behind
//     a button that looked available.
//   * Deleting the confirmation before `kill` is a one-line change that compiles, keeps every Go
//     test green (the route is fine), and passes a browser test unless that exact button is clicked.
//   * Rendering the server's `detail` sentence and rendering an empty card both compile, and the
//     route answers 200 on a failed provider read, so nothing throws and nothing fails.
//
// What is deliberately NOT asserted here, because a browser proves it better: that the card looks
// right, that buttons appear in the order they are built, and that the toast repeats the server's
// own sentence. Those live in scripts/e2e-ui.mjs.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const src = (...parts) => readFileSync(join(root, "src", ...parts), "utf8");

/** Strips comments, so a route or a decision named in prose cannot satisfy an assertion about code. */
function code(source) {
  return source.replace(/\/\*[\s\S]*?\*\//g, "").replace(/(^|\s)\/\/[^\n]*/g, "$1");
}

const CARD = code(src("ui", "game-server.ts"));
const PORTAL = code(src("views", "portal-service.ts"));
const STAFF = code(src("views", "service.ts"));
const INVOICE = code(src("views", "invoice.ts"));
const TAX = code(src("views", "tax-rates.ts"));

test("the card is one renderer mounted by both service pages", () => {
  // Two implementations would drift, and the drift shows up as a support conversation about a
  // screen the agent cannot reproduce. So both pages mount the same thing, with a different base
  // path — and that path is the only difference between the customer's and the operator's reads.
  assert.match(CARD, /export function mountServerCard\(/);
  for (const [name, page, base] of [
    ["portal-service", PORTAL, '"/api/v1/portal/services"'],
    ["service", STAFF, '"/api/v1/staff/services"'],
  ]) {
    assert.match(page, /mountServerCard\(ctx, \{/, `${name} does not mount the shared card`);
    assert.ok(page.includes(base), `${name} does not pass ${base}`);
  }
  // Neither page builds its own card: a second `/server` request in a view would be the drift.
  assert.ok(!/\/server`/.test(PORTAL), "portal-service reads /server itself instead of the shared card");
  assert.ok(!/\/server`/.test(STAFF), "service reads /server itself instead of the shared card");
});

test("the card reads and writes the routes its base path names", () => {
  assert.match(CARD, /\/server`/);
  assert.match(CARD, /ctx\.api\.post\(`\$\{url\}\/power`/);
  // The body carries the button's own action, never a free-form string from the DOM.
  assert.match(CARD, /\{ action: button\.action \}/);
});

test("power buttons come from can_power, never from the fixed set", () => {
  // POWER_BUTTONS is the closed set the protocol accepts; the render filters it by what the
  // provider says it would accept. Rendering the set directly would offer every action to every
  // server, and the provider would answer 409 behind a button that looked available.
  assert.match(CARD, /const POWER_BUTTONS:/, "the closed set of actions is gone");
  assert.match(
    CARD,
    /POWER_BUTTONS\.filter\(\(b\) => offered\.has\(b\.action\)\)/,
    "the buttons are not filtered by can_power",
  );
  assert.match(CARD, /new Set\(v\.can_power \?\? \[\]\)/, "can_power is not the source of the list");
});

test("force stop asks before it acts, in the shared card", () => {
  // The confirmation is data on the button, not a special case at the call site, so an action
  // added later cannot forget it: `confirm` set means asked, and the render refuses to build a
  // dangerous button without one.
  const at = CARD.indexOf("if (button.confirm)");
  assert.notEqual(at, -1, "the card performs power actions without checking for a confirmation");
  const login = CARD.slice(at, at + 260);
  assert.match(login, /await ctx\.confirm\(/, "the confirmation is not awaited");
  assert.match(login, /if \(!ok\) return/, "the confirmation's answer is ignored");
  // And force stop actually carries one.
  assert.match(CARD, /action: "kill"[\s\S]{0,400}?confirm:/, "force stop carries no confirmation");
});

test("a failed provider read prints the server's sentence, not an empty card", () => {
  // The route answers 200 with `available: false` and a `detail` sentence, so nothing throws. A
  // card that rendered only provider fields would be blank, and blank reads as a bug in billing
  // rather than as "the game panel is down".
  assert.match(CARD, /if \(!view\.available\)/, "there is no degraded branch");
  assert.match(CARD, /view\.detail \?\?/, "the sentence from the API is never rendered");
  assert.match(CARD, /if \(!view\.provisioned\)|provisioned/, "an unprovisioned service is not distinguished");
});

test("the invoice can charge a saved method and move a due date", () => {
  assert.match(INVOICE, /\/charge-saved-method`/);
  assert.match(INVOICE, /\/due-date`/);
  // Charging money asks first.
  const at = INVOICE.indexOf("const chargeSavedMethod");
  assert.notEqual(at, -1, "chargeSavedMethod is gone");
  assert.match(INVOICE.slice(at, at + 900), /await ctx\.confirm\(/, "charging a card does not ask first");
  // The due date is sent as an instant, never a bare date string: a date-only value parsed at
  // midnight shifts a day behind UTC and turns "extend by one day" into "extend by zero".
  assert.match(INVOICE, /due_at: dueAt\.toISOString\(\)/);
  assert.match(INVOICE, /T12:00:00Z/, "the date is parsed at midnight, which shifts it a day");
});

test("the tax screen can check a VAT ID and read the recorded checks", () => {
  assert.match(TAX, /\/api\/v1\/staff\/tax-jurisdiction\/validate-vat/);
  assert.match(TAX, /\/api\/v1\/staff\/tax-jurisdiction\/validations/);
  // The history is asked about the identifier that was checked, not about a customer.
  assert.match(TAX, /vat_id=\$\{encodeURIComponent\(vatID\)\}/);
  // "We could not ask" must not be rendered as "not registered": those are different statements
  // and only one of them is a fact about the customer.
  assert.match(TAX, /banner\(\s*"warning",\s*"The identifier was not validated"/);
});
