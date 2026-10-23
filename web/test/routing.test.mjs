// Where the shop lives, and where the panel lives.
//
// The two used to be one URL. `/` was "the dashboard for staff and the storefront for a
// visitor", decided by the session rather than by the address, and the consequence was not
// cosmetic: once you signed in, the storefront had no URL at all. An administrator could not look
// at their own shop, and a customer who wanted to buy a second thing had to reach it through a
// link somebody remembered to add.
//
// The split — storefront at `/`, panel at `/dashboard`, customer portal at `/portal` — is three
// separate facts and each one can be undone by a small edit, so each one is asserted here:
//
//   1. the route table maps the addresses to the right modules;
//   2. the path predicates agree about who may load them;
//   3. `homeFor` sends each audience to an address that exists.
//
// A predicate test is worth more than a comment because the failure mode of this bug is silent:
// everything still renders, just not the thing you asked for.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

import {
  CUSTOMER_PREFIXES,
  PUBLIC_PATHS,
  isCustomerPath,
  isPublicPath,
  isStorefrontPath,
  routes,
} from "../.test-build/routes.js";
import { homeFor } from "../.test-build/lib/session.js";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const routesSource = readFileSync(join(root, "src/routes.ts"), "utf8");

/** The view module a pattern loads, read from the source: `route("/x", () => import("./views/y"))`. */
function viewFor(pattern) {
  const match = routesSource.match(
    new RegExp(`route\\("${pattern.replace(/[.*+?^${}()|[\\]\\\\]/g, "\\\\$&")}",\\s*\\(\\)\\s*=>\\s*import\\("\\./views/([^"]+)"\\)`),
  );
  return match ? match[1] : null;
}

/** A session shaped just enough for `homeFor`, which reads one field. */
const session = (is_staff) => ({ user: { is_staff } });

test("the storefront is at /, and it is not the dashboard", () => {
  assert.equal(viewFor("/"), "storefront", "the root must load the storefront: it is the shop");
  assert.equal(viewFor("/dashboard"), "dashboard", "the panel needs its own address");
  assert.notEqual(viewFor("/"), "dashboard");
});

test("the storefront is public, and so are the auth pages", () => {
  assert.ok(PUBLIC_PATHS.has("/"), "/ must be in PUBLIC_PATHS or a stranger meets a login wall");
  for (const path of ["/login", "/register", "/forgot-password", "/reset-password", "/verify"]) {
    assert.ok(PUBLIC_PATHS.has(path), `${path} must be reachable without a session`);
  }
  assert.ok(!PUBLIC_PATHS.has("/dashboard"), "the panel is not public");
});

test("isStorefrontPath covers /, the /products alias and one product", () => {
  for (const path of ["/", "/products", "/products/vps-small", "/products/vps-small/"]) {
    assert.equal(isStorefrontPath(path), true, `${path} is a storefront page`);
  }
  for (const path of ["/dashboard", "/portal", "/cart", "/login", "/productsx"]) {
    assert.equal(isStorefrontPath(path), false, `${path} is not a storefront page`);
  }
});

test("a signed-in visitor can still reach the shop", () => {
  // The exact bug: the storefront must stay reachable *after* signing in, for both audiences.
  // `isPublicPath` is what the guard consults before it decides a page is meaningless once you
  // are signed in, so the storefront has to answer true here and be excluded from that redirect.
  assert.equal(isPublicPath("/"), true);
  assert.equal(isPublicPath("/products/vps-small"), true);
  assert.equal(isPublicPath("/dashboard"), false);
  assert.equal(isPublicPath("/invoices"), false);
  assert.equal(isPublicPath("/portal"), false);
});

test("a customer may reach the portal and the funnel, and nothing else", () => {
  for (const path of ["/portal", "/portal/invoices/abc", "/cart", "/checkout", "/tickets/abc", "/account"]) {
    assert.equal(isCustomerPath(path), true, `${path} belongs to a customer`);
  }
  for (const path of ["/dashboard", "/customers", "/invoices", "/", "/settings"]) {
    assert.equal(isCustomerPath(path), false, `${path} is staff-only`);
  }
  // A prefix is a boundary, not a substring: `/portalx` is not the portal.
  assert.equal(isCustomerPath("/portalx"), false);
  assert.equal(isCustomerPath("/cartridge"), false);
});

test("CUSTOMER_PREFIXES stays a list of declared routes' prefixes", () => {
  // `/account` is the one exception, and it is recorded rather than tolerated: it is the Paymenter
  // URL scheme the server's notification templates still classify as customer-facing
  // (`internal/notify/templates.go`), so the frontend keeps the guard in step with the server even
  // though this build serves nothing under it. A customer who follows such a link gets the
  // router's not-found screen instead of a redirect to /login, which is the better of the two
  // answers — the link is stale, not forbidden.
  const legacyAliases = new Map([["/account", "server-side link classification; no screen yet"]]);
  const patterns = routes.map((r) => r.pattern);
  for (const prefix of CUSTOMER_PREFIXES) {
    if (legacyAliases.has(prefix)) continue;
    assert.ok(
      patterns.some((p) => p === prefix || p.startsWith(`${prefix}/`)),
      `${prefix} is in CUSTOMER_PREFIXES but no route lives under it`,
    );
  }
  // The record cannot outlive the gap: once a screen lands under a recorded prefix, the entry has
  // to be deleted, or the next genuinely missing route hides behind it.
  for (const prefix of legacyAliases.keys()) {
    assert.ok(
      !patterns.some((p) => p === prefix || p.startsWith(`${prefix}/`)),
      `${prefix} now has a route, so its record in this test is stale`,
    );
  }
});

test("homeFor sends staff to the panel and customers to the portal", () => {
  assert.equal(homeFor(session(true)), "/dashboard");
  assert.equal(homeFor(session(false)), "/portal");
});

test("homeFor never sends anyone to the storefront", () => {
  // Signing in is not shopping. If this ever returns "/", the bug is back: every redirect to
  // "home" after a sign-in would drop the user on the shop and the panel would become the page
  // you can only reach by typing its URL.
  for (const staff of [true, false]) {
    assert.notEqual(homeFor(session(staff)), "/");
  }
});

test("every landing address homeFor returns is a declared route", () => {
  const patterns = routes.map((r) => r.pattern);
  for (const staff of [true, false]) {
    const home = homeFor(session(staff));
    assert.ok(patterns.includes(home), `homeFor returned ${home}, which no route serves`);
  }
});

test("the rail's Storefront entry points at the shop, not the panel", () => {
  // The two entries are one keystroke apart in the source, and pointing "Storefront" at
  // `/dashboard` produces a menu with two Dashboard items that both work — a defect no route test
  // can see, because both targets are real routes. This is the shape of the mistake, not a
  // hypothetical: it is what the bulk rename that introduced `/dashboard` did.
  const layout = readFileSync(join(root, "src/ui/layout.ts"), "utf8");
  const entries = [...layout.matchAll(/\{ label: "([^"]+)", href: "([^"]+)"/g)].map((m) => ({
    label: m[1],
    href: m[2],
  }));
  const storefront = entries.filter((e) => e.label === "Storefront");
  assert.ok(storefront.length > 0, "the rail must offer a way into the shop");
  for (const entry of storefront) {
    assert.equal(entry.href, "/", "a Storefront entry that is not / is not the shop");
  }
  const dashboard = entries.find((e) => e.label === "Dashboard");
  assert.ok(dashboard, "the staff rail must have a Dashboard entry");
  assert.equal(dashboard.href, "/dashboard");
});

test("no staff screen still links to / as home", () => {
  // A "Back to the dashboard" button pointing at `/` now opens the shop, which is the same class
  // of bug in reverse: the panel's own links would silently leave the panel. The storefront's
  // brand mark and the catalog's "View storefront" are the only legitimate `/` links, and both
  // live in files this checks by exclusion.
  const offenders = [];
  const files = ["providers", "updates", "announcements", "holds", "automation", "currencies", "logs",
    "notification-templates", "support", "support-queue", "tickets", "customers", "customer",
    "invoices", "credits", "upgrades", "cancellations", "tax-rates", "system-settings",
    "marketplace", "service-properties", "security", "coupons", "not-found"];
  for (const name of files) {
    const body = readFileSync(join(root, `src/views/${name}.ts`), "utf8");
    for (const match of body.matchAll(/href:\s*"\/"/g)) offenders.push(`${name}.ts: href "/"`);
    for (const match of body.matchAll(/navigate\("\/"/g)) offenders.push(`${name}.ts: navigate("/")`);
  }
  assert.deepEqual(offenders, [], `staff screens still treat "/" as home:\n  ${offenders.join("\n  ")}`);
});
