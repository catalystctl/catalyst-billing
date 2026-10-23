#!/usr/bin/env node
// Every route in the table, in a real browser.
//
// scripts/e2e-ui.mjs asserts 37 named screens in depth: what they contain, what they must not
// contain, that a dialog opens and closes. That is the right test for a screen whose content
// matters, and the wrong one for the question "is there any route in the table that a user
// cannot open?" — a question about the whole table, not about a curated list.
//
// The gap this closes is a specific one. A route can be declared, listed in the navigation,
// and still be a dead end: its view module may not be in the bundle, it may throw while
// rendering, or it may redirect because the guard and the table disagree. The named-screen
// harness cannot see that, because it only visits the screens somebody remembered to add to
// it. This script reads web/src/routes.ts — the same list the router and the navigation are
// generated from — and visits *every* entry.
//
// It is a separate file rather than a mode of e2e-ui.mjs for two reasons:
//
//   1. The assertions are different in kind. A named screen asserts its content; a route
//      asserts only that it is not a dead end. Mixing them makes the ledger mean two things.
//   2. e2e-ui.mjs was being edited concurrently when this was written. A second file cannot
//      conflict, and browser-e2e.sh runs both, so nothing is lost by splitting.
//
// Usage:
//   node scripts/e2e-routes.mjs --base=http://127.0.0.1:8791 \
//     --staff=... --staff-password=... \
//     [--customer=... --customer-password=...] \
//     [--customer-id=... --invoice-id=... --service-id=... --ticket-id=... \
//      --product-id=... --product-slug=...] [--screenshots=dir] [--strict-placeholders]
//
// Three outcomes, the same three the named-screen harness uses, because the distinction is
// what keeps this usable while packages land in parallel:
//
//   ok           the route rendered, with a heading, content and no error.
//   not built    the route is declared and the router found it, and the view says in its own
//                words that it is a placeholder ("Not built yet", "not available in this
//                build"). Reported in the ledger, does not fail the run — unless
//                --strict-placeholders is passed, which is what CI should run once every
//                screen exists.
//   FAIL         the router answered "Page not found" (the table and the router disagree, so
//                a link to it is a dead link), or the route redirected away from itself, or
//                it threw, or it logged a console error, or it rendered nothing.
//
// The line between the first two and the last is the whole point: a half-landed feature is a
// status, and a dead link is a defect. Collapsing them either way loses something — tolerate
// everything and the guard stops guarding, fail on everything and nobody can run the harness
// while a package is in flight.
//
// Record ids are optional. Without them a route with a :param is visited with a valid but
// nonexistent id, and its record-level "not found" state is tolerated — that is an honest
// answer, and the point of the pass is that the *route* renders rather than that the record
// exists. With them, the screen must render the record and must not show an error state.

import { spawn } from "node:child_process";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");

// ---------------------------------------------------------------- arguments --

const args = process.argv.slice(2);
const arg = (name, fallback = "") => {
  const hit = args.find((a) => a.startsWith(`--${name}=`));
  return hit ? hit.slice(name.length + 3) : fallback;
};

const base = arg("base", "http://127.0.0.1:8791").replace(/\/$/, "");
const staffEmail = arg("staff");
const staffPassword = arg("staff-password");
const customerEmail = arg("customer");
const customerPassword = arg("customer-password");
const shotDir = arg("screenshots");
// A declared route whose view is still a placeholder is tolerated by default, because the
// commerce, support and platform screens land in parallel and a harness that is red for a
// screen nobody has written yet stops being run. `--strict-placeholders` is the other half:
// once the build is complete, CI turns it on and "not built yet" becomes a failure too.
const strictPlaceholders = args.includes("--strict-placeholders");

if (!staffEmail || !staffPassword) {
  console.error("usage: e2e-routes.mjs --base=URL --staff=EMAIL --staff-password=PASS [--customer=... --customer-password=...]");
  process.exit(2);
}
if (shotDir) mkdirSync(shotDir, { recursive: true });

// ------------------------------------------------------- the route table --

// Read, not imported: routes.ts is TypeScript with dynamic imports, and the point is to test
// the table as written. The pattern is the one the table uses and nothing else, so a table
// that stops matching it is a failure of this harness rather than a silent zero-route pass.
const routesSource = readFileSync(join(root, "web/src/routes.ts"), "utf8");

const table = [...routesSource.matchAll(/route\("([^"]+)",\s*\(\)\s*=>\s*import\("\.\/views\/([^"]+)"\)/g)].map(
  (m) => ({ path: m[1], view: m[2] }),
);
if (table.length < 20) {
  console.error(`e2e-routes: read ${table.length} routes from web/src/routes.ts; the table has moved or changed shape`);
  process.exit(2);
}

/** The set literal from routes.ts, e.g. PUBLIC_PATHS. */
function setLiteral(name) {
  const match = routesSource.match(new RegExp(`${name}\\s*=\\s*new Set\\(\\[([^\\]]*)\\]`));
  if (!match) throw new Error(`${name} not found in web/src/routes.ts`);
  return [...match[1].matchAll(/"([^"]+)"/g)].map((m) => m[1]);
}

/** The array literal from routes.ts, e.g. CUSTOMER_PREFIXES. */
function arrayLiteral(name) {
  const match = routesSource.match(new RegExp(`${name}[^=]*=\\s*\\[([^\\]]*)\\]`));
  if (!match) throw new Error(`${name} not found in web/src/routes.ts`);
  return [...match[1].matchAll(/"([^"]+)"/g)].map((m) => m[1]);
}

const publicPaths = new Set(setLiteral("PUBLIC_PATHS"));
const customerPrefixes = arrayLiteral("CUSTOMER_PREFIXES");

const isPublic = (path) =>
  publicPaths.has(path) || path === "/products" || path.startsWith("/products/");
const isCustomer = (path) => customerPrefixes.some((p) => path === p || path.startsWith(`${p}/`));

/** True for the storefront: `/`, `/products` and one product.
 *
 *  Read from routes.ts rather than re-derived, because the storefront is public for *every*
 *  audience — a stranger, a customer and an operator — and it is therefore not "a public route"
 *  in the sense the staff and customer phases care about. It is swept once, anonymously, and
 *  skipped in the signed-in phases: `/` does not redirect for a signed-in user (that is the whole
 *  point of the split), so it would pass there too, but asserting it once is the honest shape. */
const isStorefront = (path) => path === "/" || path === "/products" || path.startsWith("/products/");

// ------------------------------------------------------------- parameters --

// A record id per path segment. The order matters: `/portal/invoices/:id` is an invoice, and
// `/customers/:id/portal` is a customer. A route with no id available is visited with a
// well-formed uuid that exists nowhere, which every detail screen answers with its own
// not-found state.
const ids = {
  customer: arg("customer-id"),
  invoice: arg("invoice-id"),
  service: arg("service-id"),
  ticket: arg("ticket-id"),
  product: arg("product-id"),
  slug: arg("product-slug"),
};
const NOWHERE = "00000000-0000-4000-8000-000000000000";

/** Fills :params and says whether every value came from a real seeded record. */
function concretize(path) {
  // The record is decided by the path, not by the parameter's name: every detail route calls
  // its parameter `:id`, so `ids[name]` would never resolve. The order matters —
  // `/customers/:id/portal` is a customer, `/portal/invoices/:id` is an invoice.
  const record =
    /^\/customers\//.test(path) ? "customer"
    : /^\/portal\/invoices\//.test(path) || /^\/invoices\//.test(path) ? "invoice"
    : /^\/portal\/services\//.test(path) || /^\/services\//.test(path) ? "service"
    : /^\/tickets\//.test(path) ? "ticket"
    : /^\/catalog(-management)?\//.test(path) ? "product"
    : "";
  let strict = true;
  const filled = path.replace(/:([a-z]+)/g, (_, name) => {
    if (name === "slug") {
      if (ids.slug) return encodeURIComponent(ids.slug);
      strict = false;
      return "no-such-product";
    }
    const value = ids[record];
    if (value) return encodeURIComponent(value);
    strict = false;
    return NOWHERE;
  });
  return { path: filled, strict };
}

// ---------------------------------------------------------------- chromium --

const chrome = process.env.CHROME_BIN || "chromium";
const port = 9800 + Math.floor(Math.random() * 400);
const profile = `/tmp/billing-routes-${Date.now()}`;

// A headless Chromium on this host stalls on its first navigation when the session bus
// variables are inherited from the desktop session, so they are stripped.
const env = { ...process.env };
delete env.DBUS_SESSION_BUS_ADDRESS;
delete env.QT_ACCESSIBILITY;

const child = spawn(
  chrome,
  [
    "--headless=new",
    "--no-sandbox",
    "--disable-gpu",
    "--disable-dev-shm-usage",
    `--remote-debugging-port=${port}`,
    `--user-data-dir=${profile}`,
    "--window-size=1440,900",
    "about:blank",
  ],
  { env, stdio: ["ignore", "ignore", "pipe"] },
);

const debugLog = [];
child.stderr.on("data", (chunk) => debugLog.push(chunk.toString()));

async function waitForEndpoint() {
  const deadline = Date.now() + 30_000;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(`http://127.0.0.1:${port}/json/version`);
      const body = await res.json();
      if (body.webSocketDebuggerUrl) return body.webSocketDebuggerUrl;
    } catch {
      /* not up yet */
    }
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(`chromium did not expose a debugging endpoint:\n${debugLog.join("").slice(-2000)}`);
}

const wsUrl = await waitForEndpoint();
const ws = new WebSocket(wsUrl);
await new Promise((resolve, reject) => {
  ws.onopen = resolve;
  ws.onerror = reject;
});

let nextId = 1;
const pending = new Map();
// An exception is attributed to the route that was on display, so a failure names the route
// rather than dumping a stack trace at the end of the run.
const exceptions = [];
const consoleErrors = [];
let currentRoute = "startup";

ws.onmessage = (msg) => {
  const data = JSON.parse(msg.data);
  if (data.id && pending.has(data.id)) {
    pending.get(data.id)(data);
    pending.delete(data.id);
    return;
  }
  if (data.method === "Runtime.exceptionThrown") {
    const d = data.params.exceptionDetails;
    exceptions.push({ route: currentRoute, text: d.exception?.description ?? d.text });
  }
  if (data.method === "Runtime.consoleAPICalled" && data.params.type === "error") {
    const line = (data.params.args ?? []).map((a) => a.value ?? a.description ?? "").join(" ");
    consoleErrors.push({ route: currentRoute, text: line });
  }
};

function send(method, params = {}, sessionId) {
  const id = nextId++;
  const payload = { id, method, params };
  if (sessionId) payload.sessionId = sessionId;
  ws.send(JSON.stringify(payload));
  return new Promise((resolve) => pending.set(id, resolve));
}

const { result: target } = await send("Target.createTarget", { url: "about:blank" });
const { result: attached } = await send("Target.attachToTarget", { targetId: target.targetId, flatten: true });
const session = attached.sessionId;

await send("Runtime.enable", {}, session);
await send("Page.enable", {}, session);
await send("Network.enable", {}, session);
await send("Emulation.setDeviceMetricsOverride", { width: 1440, height: 900, deviceScaleFactor: 1, mobile: false }, session);

// ---------------------------------------------------------------- page api --

async function evaluate(expression, awaitPromise = true) {
  const { result } = await send(
    "Runtime.evaluate",
    { expression, awaitPromise, returnByValue: true, userGesture: true },
    session,
  );
  if (result?.exceptionDetails) {
    throw new Error(result.exceptionDetails.exception?.description ?? result.exceptionDetails.text);
  }
  return result?.result?.value;
}

async function until(description, expression, timeoutMs = 8000) {
  const deadline = Date.now() + timeoutMs;
  let last;
  while (Date.now() < deadline) {
    last = await evaluate(`(() => { try { return (${expression}); } catch (e) { return "threw: " + e.message; } })()`);
    if (last) return last;
    await new Promise((r) => setTimeout(r, 80));
  }
  throw new Error(`timed out waiting for ${description} (last: ${JSON.stringify(last)})`);
}

/**
 * Navigates and waits for the app to reach a terminal state.
 *
 * "Terminal" is any of: the shell with content, an auth panel, a store page, an empty state,
 * an error state. Waiting for a specific element is how a route whose view is missing looks
 * like a timeout instead of like the dead end it is.
 */
async function visit(path, timeoutMs = 12_000) {
  currentRoute = path;
  await send("Page.navigate", { url: `${base}${path}` }, session);
  const appeared = await until(
    `something to render on ${path}`,
    "document.querySelector('.page, .auth-panel, .store-page, .empty-state, .error-state, .card') !== null",
    timeoutMs,
  ).catch(() => false);
  if (appeared) {
    await until(
      `the screen on ${path} to settle`,
      "!document.querySelector('.skeleton') && !document.querySelector('.table-loading')",
      timeoutMs,
    ).catch(() => false);
  }
  await new Promise((r) => setTimeout(r, 120));
  return evaluate(`(() => {
    const q = (s) => document.querySelector(s);
    const contentSelector = ".card, .table, .empty, .empty-state, .error-state, .banner, .split, .grid, .meta-grid, .invoice-doc, .settings-nav, .auth-form, .auth-panel, .store-card, .store-product, .store-page, .page-head";
    return {
      path: location.pathname,
      search: location.search,
      h1: q("h1") ? q("h1").innerText : null,
      titles: [...document.querySelectorAll(".empty-title, .card-title, .page-title")].map((el) => el.innerText).join(" \\u00b7 "),
      errorText: q(".error-state") ? q(".error-state").innerText : null,
      contentNodes: document.querySelectorAll(contentSelector).length,
      text: document.body.innerText || "",
    };
  })()`);
}

async function click(selector) {
  await until(`a clickable ${selector}`, `document.querySelector(${JSON.stringify(selector)}) !== null`);
  const box = await evaluate(`(() => {
    const el = document.querySelector(${JSON.stringify(selector)});
    el.scrollIntoView({ block: "center" });
    const r = el.getBoundingClientRect();
    return { x: r.x + r.width / 2, y: r.y + r.height / 2 };
  })()`);
  for (const type of ["mousePressed", "mouseReleased"]) {
    await send("Input.dispatchMouseEvent", { type, x: box.x, y: box.y, button: "left", clickCount: 1 }, session);
  }
}

async function typeInto(selector, text) {
  await click(selector);
  await send("Input.insertText", { text }, session);
}

async function screenshot(name) {
  if (!shotDir) return;
  const { result } = await send("Page.captureScreenshot", { format: "png" }, session);
  if (result?.data) writeFileSync(`${shotDir}/${name}.png`, Buffer.from(result.data, "base64"));
}

/** Signs in through the form, which is also what proves the form works. */
async function signIn(email, password) {
  await visit("/login");
  await typeInto("#email", email);
  await typeInto("#password", password);
  await click(".auth-form .btn--primary");
  await until("the shell after sign-in", "document.querySelector('.app') !== null", 15_000);
}

/** Signs out through the menu, so the next phase starts with no session.
 *
 *  `from` is the screen whose shell holds the user menu, and it must be a signed-in screen: the
 *  storefront at `/` has no shell, so navigating there to sign out would look for a sidebar that
 *  does not exist. That is exactly what this did after `/` became the shop, and the sweep failed
 *  with "timed out waiting for .sidebar-foot .who" — a harness bug that reads like a panel bug. */
async function signOut(from = "/dashboard") {
  await visit(from);
  await click(".sidebar-foot .who");
  await click(".menu-item--danger");
  await until("the sign-in screen", "document.querySelector('.auth-panel') !== null", 10_000);
}

// ---------------------------------------------------------------- the pass --

const failures = [];
const passes = [];

function check(name, condition, detail = "") {
  if (condition) {
    passes.push(name);
    return true;
  }
  failures.push(`${name}${detail ? ` — ${detail}` : ""}`);
  console.log(`FAIL ${name}${detail ? ` — ${detail}` : ""}`);
  return false;
}

/**
 * The markers that mean "this route is a dead link", as opposed to "this route rendered an
 * honest answer".
 *
 *   - the router's own not-found screen: the route table and the router disagree, which is
 *     exactly the class of dead link this pass exists to catch;
 *
 * That is the only entry, and it is deliberate. "This screen is not available in this build"
 * is *not* a dead link: the route is declared, the router found it, and its chunk is simply
 * not in the bundle yet. Treating the two the same would make this pass red every time a
 * package lands a route before its view, which is the normal order of work — and the obvious
 * response to that, deleting the route from the sweep, hides the dead link again.
 */
const DEAD_LINK_MARKERS = [/page not found/i, /that page does not exist/i];

/**
 * The markers that mean "declared, routed, and not built yet".
 *
 * A placeholder is reported in the ledger and does not fail the run, because a half-landed
 * feature must be *visible* rather than silently skipped: a run that says "75 routes ok" when
 * one of them is an empty card is a run that lies. `--strict-placeholders` turns every one of
 * these into a failure, which is what CI runs once the build is complete.
 */
const PLACEHOLDER_MARKERS = [
  /not available in this build/i,
  /has not shipped yet/i,
  /not built yet/i,
  /not available on this installation/i,
];

function firstMarker(titles, markers) {
  for (const marker of markers) {
    const hit = titles.match(marker);
    if (hit) return hit[0];
  }
  return "";
}

/** The router's not-found screen: the route table and the router disagree. */
function deadLink(state) {
  return firstMarker(state.titles, DEAD_LINK_MARKERS);
}

/** A declared route whose view says, in its own words, that it is not built. */
function placeholderReason(state) {
  return firstMarker(state.titles, PLACEHOLDER_MARKERS);
}

const ledger = [];
const placeholders = [];

/** Visits one route and records what happened. */
async function checkRoute(route, { strict, phase }) {
  const before = { exceptions: exceptions.length, console: consoleErrors.length };
  const state = await visit(route.path);
  const where = `${phase}: ${route.path}`;

  const dead = deadLink(state);
  // A placeholder is only "not built yet" when the router did find the route. A not-found
  // screen wins, because that is the more serious answer.
  const notBuilt = dead ? "" : placeholderReason(state);
  const thrown = exceptions.slice(before.exceptions).filter((e) => e.route === route.path);
  const logged = consoleErrors.slice(before.console).filter((e) => e.route === route.path);

  // A route that redirects is not necessarily broken, and the two cases are named so the reason
  // stays readable: a screen is free to write a query string into its own URL, and a detail route
  // whose record does not exist may legitimately send the user back to its list. Anything else is
  // the guard disagreeing with the table, which is a dead end with a redirect in front of it.
  const wanted = route.path.split("?")[0];
  const redirected = state.path !== wanted;

  const problems = [];
  if (dead) problems.push(`dead link: ${dead}`);
  if (redirected) problems.push(`redirected to ${state.path}${state.search}`);
  if (thrown.length > 0) problems.push(`uncaught: ${thrown.map((e) => e.text).join(" | ").slice(0, 300)}`);
  // A 401 is the shell probing for a session; it is expected on the public pages and is not
  // an error a user sees.
  const unexpected = logged.filter((e) => !/401 \(Unauthorized\)/.test(e.text));
  if (unexpected.length > 0) problems.push(`console: ${unexpected.map((e) => e.text).join(" | ").slice(0, 300)}`);
  // A heading is how a rendered screen announces itself. Some screens are a single card with
  // no <h1> (the portal's billing details), so a card or page title counts.
  if (!state.h1 && !state.titles) problems.push("no heading");
  if (state.contentNodes === 0) problems.push("renders no content");
  // A render failure is always a defect. A record-level "not found" is the correct answer for
  // a route visited with an id that was not seeded, so it is tolerated on those routes only.
  if (/something went wrong rendering|screen could not be loaded/i.test(state.errorText ?? "")) {
    problems.push(`render failure: ${state.errorText.slice(0, 200)}`);
  } else if (strict && state.errorText) {
    problems.push(`error state: ${state.errorText.slice(0, 200)}`);
  }

  // Not built yet: recorded, printed, and not a failure — unless the caller asked for strict
  // mode, or the placeholder also threw, redirected or rendered nothing (those are problems
  // above and still fail).
  if (notBuilt && problems.length === 0) {
    if (!strictPlaceholders) {
      placeholders.push({ path: route.path, phase, reason: notBuilt });
      ledger.push({ path: route.path, phase, status: "placeholder", problems: [`not built yet: ${notBuilt}`] });
      console.log(`gap  ${where} — not built yet: ${notBuilt}`);
      await screenshot(`placeholder-${phase}-${route.path.replace(/[^a-z0-9]+/gi, "-")}`.slice(0, 80));
      return "placeholder";
    }
    problems.push(`not built yet (strict mode): ${notBuilt}`);
  }

  const status = problems.length === 0 ? "ok" : "FAIL";
  ledger.push({ path: route.path, phase, status, problems });
  await screenshot(`${phase}-${route.path.replace(/[^a-z0-9]+/gi, "-")}`.slice(0, 80));

  if (problems.length > 0) {
    failures.push(`${where} — ${problems.join("; ")}`);
    console.log(`FAIL ${where} — ${problems.join("; ")}`);
    return false;
  }
  passes.push(where);
  console.log(`ok   ${where}`);
  return true;
}

try {
  // 1. The public routes, as a stranger. This is the phase that must work with no session.
  for (const route of table.filter((r) => isPublic(r.path))) {
    const { path, strict } = concretize(route.path);
    // A public route has no seeded record behind it; a product slug is the one exception.
    await checkRoute({ ...route, path }, { strict: strict && route.path.includes(":slug"), phase: "anonymous" });
  }

  // 2. The staff routes, as an administrator. The storefront is excluded: it is public for every
  // audience, it was swept in phase 1, and it deliberately does not redirect a signed-in operator
  // (that is the fix this sweep guards).
  await signIn(staffEmail, staffPassword);
  for (const route of table.filter((r) => !isPublic(r.path) && !isCustomer(r.path) && !isStorefront(r.path))) {
    const { path, strict } = concretize(route.path);
    await checkRoute({ ...route, path }, { strict, phase: "staff" });
  }

  // 3. The customer routes, as the seeded customer.
  if (customerEmail && customerPassword) {
    await signOut();
    await signIn(customerEmail, customerPassword);
    for (const route of table.filter((r) => isCustomer(r.path) && !isStorefront(r.path))) {
      const { path, strict } = concretize(route.path);
      await checkRoute({ ...route, path }, { strict, phase: "customer" });
    }
  }

  check("no uncaught exceptions on any route", exceptions.length === 0, exceptions.map((e) => `[${e.route}] ${e.text}`).join(" | ").slice(0, 500));
  const unexpectedConsole = consoleErrors.filter((e) => !/401 \(Unauthorized\)/.test(e.text));
  check("no unexpected console errors", unexpectedConsole.length === 0, unexpectedConsole.map((e) => `[${e.route}] ${e.text}`).join(" | ").slice(0, 500));
} catch (err) {
  failures.push(`the run threw: ${err.message}`);
  console.log(`FAIL the run threw: ${err.message}`);
  try {
    await screenshot("failure");
  } catch {
    /* best effort */
  }
} finally {
  ws.close();
  child.kill("SIGKILL");
}

console.log(
  `\n${passes.length} routes ok, ${placeholders.length} not built yet, ${failures.length} failed ` +
    `(${table.length} in the table)`,
);

// The placeholder list, printed whether the run passed or failed. A run that said "75 routes
// ok" while one of them was an empty card would be a run that lies, and the whole point of
// this pass is that a screen nobody can use is visible as a test result rather than as a bug
// report. `--strict-placeholders` makes each of these a failure.
if (placeholders.length > 0) {
  console.log(`\nnot built yet (${placeholders.length}):`);
  for (const entry of placeholders) console.log(`  gap  ${entry.phase}: ${entry.path} — ${entry.reason}`);
  if (!strictPlaceholders) console.log("     tolerated: a declared route with a placeholder view does not fail this pass");
}

if (failures.length > 0) {
  console.log("\nfailures:");
  for (const f of failures) console.log(`  - ${f}`);
  process.exit(1);
}
// The closing line states what was actually proved, not what the pass would like to have
// proved. "Every route renders" is false while one of them is a placeholder card, and a
// harness that rounds that up is the reason a missing screen survives to a bug report.
if (placeholders.length > 0) {
  console.log(
    `route sweep: no dead links (${passes.length} ok, ${placeholders.length} declared but not built yet)`,
  );
} else {
  console.log("route sweep: every route in the table renders");
}
