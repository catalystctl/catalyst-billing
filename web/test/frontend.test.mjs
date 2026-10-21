// Frontend tests.
//
// Run with `node --test` after bundling the pure modules with esbuild. The modules under test
// are deliberately free of DOM access so they can be exercised in Node: the money formatter,
// the amount parser, the status→tone map and the route matcher.
//
// The money functions are the point. A panel that renders 1,240.00 as 1240 or 1.24 disagrees
// with the ledger at exactly the moment somebody is reconciling it, and a test that pins the
// string is the only way to keep that honest across a refactor.

import assert from "node:assert/strict";
import test from "node:test";

import { formatMinor, money, parseMoneyToMinor, statusTone, humanStatus, safeExternalURL, age } from "../.test-build/lib/format.js";
import { matchRoute, route } from "../.test-build/lib/router.js";

test("formatMinor inserts the decimal point without floating point", () => {
  assert.equal(formatMinor(0, 2), "0.00");
  assert.equal(formatMinor(5, 2), "0.05");
  assert.equal(formatMinor(50, 2), "0.50");
  assert.equal(formatMinor(124000, 2), "1,240.00");
  assert.equal(formatMinor(-124000, 2), "-1,240.00");
  assert.equal(formatMinor(123456789, 2), "1,234,567.89");
  // A zero-decimal currency must not grow a decimal point.
  assert.equal(formatMinor(1200, 0), "1,200");
  // Three-decimal currencies (KWD, BHD) keep all three digits.
  assert.equal(formatMinor(1234, 3), "1.234");
  // A value smaller than one minor unit at the given scale is padded, not truncated.
  assert.equal(formatMinor(7, 3), "0.007");
});

test("money uses the API's own display scale", () => {
  assert.equal(money({ minor: 2880, currency: "USD", scale: 2 }), "28.80");
  assert.equal(money({ minor: 2880, currency: "JPY", scale: 0 }), "2,880");
  assert.equal(money(null), "—");
});

test("parseMoneyToMinor is the inverse for well-formed input", () => {
  assert.equal(parseMoneyToMinor("1,240.00"), 124000);
  assert.equal(parseMoneyToMinor("1240"), 124000);
  assert.equal(parseMoneyToMinor("0.05"), 5);
  assert.equal(parseMoneyToMinor("12.3"), 1230);
  assert.equal(parseMoneyToMinor(" 12.34 "), 1234);
  assert.equal(parseMoneyToMinor("-12.34"), -1234);
  assert.equal(parseMoneyToMinor("12.345"), 1234); // extra precision is truncated, not rounded up
  assert.equal(parseMoneyToMinor("1200", 0), 1200);
});

test("parseMoneyToMinor rejects what is not an amount", () => {
  for (const input of ["", " ", "abc", "1.2.3", "12,3x", ".", "-", "1e5"]) {
    assert.equal(parseMoneyToMinor(input), null, `expected null for ${JSON.stringify(input)}`);
  }
});

test("statusTone gives one colour per meaning", () => {
  assert.equal(statusTone("paid"), "success");
  assert.equal(statusTone("succeeded"), "success");
  assert.equal(statusTone("open"), "info");
  assert.equal(statusTone("overdue"), "warning");
  assert.equal(statusTone("failed"), "danger");
  assert.equal(statusTone("refunded"), "neutral");
  assert.equal(statusTone(undefined), "neutral");
  assert.equal(statusTone("a-status-invented-later"), "neutral");
});

test("humanStatus never returns an empty label", () => {
  assert.equal(humanStatus("pending_payment"), "Awaiting payment");
  assert.equal(humanStatus("needs_review"), "Needs review");
  assert.equal(humanStatus("some_new_state"), "Some New State");
  assert.equal(humanStatus(""), "—");
});

test("safeExternalURL only allows http(s)", () => {
  assert.equal(safeExternalURL("https://pay.example.com/x"), "https://pay.example.com/x");
  assert.equal(safeExternalURL("javascript:alert(1)"), null);
  assert.equal(safeExternalURL("data:text/html,<script>"), null);
  assert.equal(safeExternalURL(""), null);
});

test("age is coarse and never negative", () => {
  const now = Date.now();
  assert.equal(age(new Date(now - 30_000).toISOString()), "now");
  assert.equal(age(new Date(now - 5 * 60_000).toISOString()), "5m");
  assert.equal(age(new Date(now - 3 * 3600_000).toISOString()), "3h");
  assert.equal(age(new Date(now - 4 * 86400_000).toISOString()), "4d");
  assert.equal(age(new Date(now + 3600_000).toISOString()), "—");
  assert.equal(age(null), "—");
});

test("route matching captures params and rejects a wrong shape", () => {
  const routes = [
    route("/", async () => ({})),
    route("/customers", async () => ({})),
    route("/customers/:id", async () => ({})),
    route("/portal/invoices/:id", async () => ({})),
  ];

  const match = matchRoute(routes, "/customers/abc-123", "?tab=invoices");
  assert.ok(match);
  assert.equal(match.params.id, "abc-123");
  assert.equal(match.query.get("tab"), "invoices");

  assert.equal(matchRoute(routes, "/customers/abc-123/extra", ""), null);
  assert.equal(matchRoute(routes, "/nope", ""), null);
  assert.equal(matchRoute(routes, "/", "")?.route.pattern, "/");

  // A trailing slash is the same route: a pasted link often has one.
  assert.equal(matchRoute(routes, "/customers/", "")?.route.pattern, "/customers");
});

test("route matching decodes an escaped segment", () => {
  const routes = [route("/customers/:id", async () => ({}))];
  const match = matchRoute(routes, "/customers/a%20b", "");
  assert.equal(match?.params.id, "a b");
});
