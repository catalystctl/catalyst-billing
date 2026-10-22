#!/usr/bin/env node
// Drives the operator panel and the customer portal in a real browser.
//
// The unit tests pin formatting and routing, and the Go tests pin the API. Neither can prove
// that a screen renders: a missing import, a null field from the wire, a CSS rule that hides a
// table cell, or a button that is not associated with its form all pass every test that does
// not execute the DOM. This script executes the DOM, through the same code path a person uses:
// it types into the sign-in form, clicks the navigation, opens a record, and opens a dialog.
//
// It fails on three things a screenshot review would miss:
//   - an uncaught exception or a console error anywhere in the run;
//   - an `.error-state` block on a screen that should have data;
//   - a screen that renders its heading but not its content.
//
// # The new-screen loop, and why a 404 is tolerated
//
// The commerce, support and account screens were built by parallel packages and land in
// stages, so at any moment some of them exist and some do not. A harness that treated a
// missing screen as a failure would be red for reasons that are not defects, and the
// obvious response to that — commenting the screen out of the list — hides the gap
// instead of reporting it. So the loop distinguishes three outcomes per screen:
//
//   present   the screen rendered with content and no error state, and every assertion
//             about it passed. A screen that exists and renders *wrong* is a failure: an
//             error block, an uncaught exception, a console error, a `[object Object]`, a
//             heading with nothing under it, or a table that overflows a phone.
//   missing   the build does not contain the screen: the route is undeclared, its chunk is
//             not in the bundle, or the screen itself reports that the API behind it is
//             absent. This is reported by name and does not fail the run — the point is
//             that the status is *visible*, not that it is green.
//   failed    present, but one of the assertions failed. This fails the run.
//
// The final report lists all three, so "8 of 19 screens present" is printed rather than
// inferred from the absence of a line.
//
// Usage: node scripts/e2e-ui.mjs --base=http://127.0.0.1:8791 \
//          --staff=ops@example.test --staff-password=... \
//          [--customer=... --customer-password=...] [--screenshots=dir]

import { spawn } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";

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
const serviceCustomerId = arg("service-customer");
const customerPassword = arg("customer-password");
const shotDir = arg("screenshots");

if (!staffEmail || !staffPassword) {
  console.error("usage: e2e-ui.mjs --base=URL --staff=EMAIL --staff-password=PASS [--customer=EMAIL --customer-password=PASS]");
  process.exit(2);
}
if (shotDir) mkdirSync(shotDir, { recursive: true });

// ---------------------------------------------------------------- chromium --

const chrome = process.env.CHROME_BIN || "chromium";
const port = 9400 + Math.floor(Math.random() * 400);
const profile = `/tmp/billing-e2e-${Date.now()}`;

// A headless Chromium on this host stalls ~25s on its first navigation when the session bus
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
  throw new Error("chromium did not expose a debugging endpoint");
}

const wsUrl = await waitForEndpoint();
const ws = new WebSocket(wsUrl);
await new Promise((resolve, reject) => {
  ws.onopen = resolve;
  ws.onerror = reject;
});

let nextId = 1;
const pending = new Map();
const consoleErrors = [];
// Console errors are attributed to the screen that was on display when they happened, so the
// report can say "coupons: <message>" rather than dumping one undifferentiated list at the
// end. A console error is the least visible defect there is, and "it happened somewhere" is
// not an actionable finding.
const consoleScreenErrors = [];
const exceptions = [];
let currentScreen = "startup";

ws.onmessage = (msg) => {
  const data = JSON.parse(msg.data);
  if (data.id && pending.has(data.id)) {
    pending.get(data.id)(data);
    pending.delete(data.id);
    return;
  }
  if (data.method === "Runtime.exceptionThrown") {
    const d = data.params.exceptionDetails;
    const where = (d.stackTrace?.callFrames ?? [])
      .slice(0, 3)
      .map((f) => `${f.functionName || "<anonymous>"} ${f.url.split("/").pop()}:${f.lineNumber + 1}`)
      .join(" <- ");
    exceptions.push(`${d.exception?.description ?? d.text} ${where}`);
  }
  if (data.method === "Runtime.consoleAPICalled" && data.params.type === "error") {
    const line = (data.params.args ?? []).map((a) => a.value ?? a.description ?? "").join(" ");
    consoleErrors.push(line);
    consoleScreenErrors.push({ screen: currentScreen, message: line });
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

/** Waits for a predicate in the page, returning its value or throwing on timeout. */
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

async function goto(path, timeoutMs = 10_000) {
  currentScreen = path;
  await send("Page.navigate", { url: `${base}${path}` }, session);
  await until(
    `the app to render on ${path}`,
    "document.querySelector('.page, .auth-panel, .store-page')",
    timeoutMs,
  );
  // The shell paints a skeleton before the screen's data arrives, and a table paints its own
  // while a page is in flight. Waiting only for `.page-skeleton` returns before the rows
  // exist, which made an assertion about content pass or fail on timing. No skeleton anywhere
  // is the honest definition of "settled".
  await until(
    `the screen on ${path} to settle`,
    "!document.querySelector('.skeleton') && !document.querySelector('.table-loading')",
    timeoutMs,
  );
  // The screen has rendered something structural — a card, a table, an empty state, an error,
  // a document. Checking for a *specific* element was too narrow: the portal's billing-details
  // screen is a single card with no page head, and it timed out on a predicate that only knew
  // about tables and headings.
  await until(
    `the screen on ${path} to have content`,
    "document.querySelector('.page-head, .card, .table, .empty, .error-state, .invoice-doc, .auth-panel, .settings-nav, .store-card, .store-hero, .store-product, .store-section') !== null",
    timeoutMs,
  );
  // One frame for the swap that replaced the skeleton to lay out.
  await new Promise((r) => setTimeout(r, 120));
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

async function pageText() {
  return evaluate("document.body.innerText");
}

// ---------------------------------------------------------------- assertions --

const failures = [];
const passes = [];

function check(name, condition, detail = "") {
  if (condition) {
    passes.push(name);
    console.log(`ok   ${name}`);
    return true;
  }
  failures.push(`${name}${detail ? ` — ${detail}` : ""}`);
  console.log(`FAIL ${name}${detail ? ` — ${detail}` : ""}`);
  return false;
}

/** Asserts a screen rendered its content: a heading, no error block, and expected text. */
async function assertScreen(name, { path, expect = [], absent = [], allowEmptyState = true }) {
  if (path) await goto(path);
  const text = await pageText();
  const html = await evaluate("document.body.innerHTML");

  const hasError = await evaluate("document.querySelector('.error-state') !== null");
  check(`${name}: no error state`, !hasError, hasError ? await evaluate("document.querySelector('.error-state').innerText") : "");
  check(`${name}: has a heading`, /<h1/i.test(html), "no <h1> in the rendered screen");

  // innerText reflects `text-transform`, so a label styled uppercase reads "OUTSTANDING".
  // Matching case-insensitively keeps the assertion about the content, not the typography.
  const haystack = text.toLowerCase();
  // A node interpolated into a template literal stringifies to "[object HTMLSpanElement]",
  // and an object rendered without a formatter becomes "[object Object]". Both are the same
  // class of defect — a value reaching the DOM without a renderer — and both are invisible to
  // a test that only checks that the page is non-empty. Found this way: an invoice legend
  // read "Paid [object HTMLSpanElement]".
  check(`${name}: no stringified objects`, !/\[object (Object|HTML\w*Element)\]/.test(text), text.match(/\[object [^\]]+\]/)?.[0] ?? "");

  for (const needle of expect) {
    check(`${name}: shows ${JSON.stringify(needle)}`, haystack.includes(needle.toLowerCase()));
  }
  for (const needle of absent) {
    check(`${name}: does not show ${JSON.stringify(needle)}`, !haystack.includes(needle.toLowerCase()));
  }
  if (allowEmptyState) {
    const empty = await evaluate("document.querySelector('.empty') !== null");
    if (empty) {
      const emptyTitle = await evaluate("document.querySelector('.empty .empty-title')?.innerText ?? ''");
      console.log(`     note: ${name} rendered its empty state (${emptyTitle})`);
    }
  }
  await screenshot(name.toLowerCase().replace(/[^a-z0-9]+/g, "-"));
  return text;
}

// ------------------------------------------------- the new-screen ledger --

// The commerce, support and account screens are built by parallel packages and land in
// stages, so the harness must be able to tell "this build does not have that screen" from
// "that screen is broken". The distinction is made from what the page actually renders,
// not from a list the harness keeps: a screen that is not in the build says so, and a
// screen that is in the build and broken says something else.
const MISSING_MARKERS = [
  // The route table has no entry for the path (app.ts renderNotFound).
  /that page does not exist/i,
  // The route is declared but its chunk is not in the bundle (app.ts, unresolved import),
  // or the view module is still the placeholder that says so.
  /not available in this build/i,
  // A real screen whose endpoint this build does not serve, e.g. config options or logs.
  /not available on this installation/i,
  /does not serve (?:GET|POST|PUT|DELETE) \/api\//i,
  /has not shipped yet/i,
];

const screenLedger = [];

/** Reads everything the assertions need in one round trip. */
async function screenState() {
  return evaluate(`(() => {
    const q = (s) => document.querySelector(s);
    const contentSelector = ".card, .table, .empty, .empty-state, .banner, .split, .grid, .meta-grid, .invoice-doc, .settings-nav, .auth-form, .store-card, .store-product";
    const text = document.body.innerText || "";
    const tr = q(".table tbody tr");
    return {
      path: location.pathname,
      title: document.title,
      h1: q("h1") ? q("h1").innerText : null,
      hasCard: !!q(".card"),
      hasTable: !!q(".table"),
      hasEmpty: !!q(".empty"),
      hasEmptyState: !!q(".empty-state"),
      errorText: q(".error-state") ? q(".error-state").innerText : null,
      emptyTitle: q(".empty .empty-title, .empty-state .empty-title") ? q(".empty .empty-title, .empty-state .empty-title").innerText : "",
      contentNodes: document.querySelectorAll(contentSelector).length,
      overflow: document.documentElement.scrollWidth > window.innerWidth + 1,
      rowDisplay: tr ? getComputedStyle(tr).display : null,
      text,
    };
  })()`);
}

/**
 * Visits a screen without demanding that it exist.
 *
 * `goto()` throws when nothing renders, which is right for a screen the suite depends on
 * and wrong for one that may not be in this build. This waits for *any* of the terminal
 * states — a page, an error, an empty state, or the not-found card — and returns whatever
 * it found, including "nothing yet" after the timeout, so the caller can classify it.
 */
async function visit(path, timeoutMs = 12_000) {
  currentScreen = path;
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
  // A frame for the swap that replaced the skeleton to lay out.
  await new Promise((r) => setTimeout(r, 150));
  return screenState();
}

/** Which of the tolerated "not in this build" markers the page shows, if any. */
function missingReason(state) {
  for (const marker of MISSING_MARKERS) {
    const hit = state.text.match(marker);
    if (hit) return hit[0];
  }
  return "";
}

/**
 * One screen in the tolerant loop.
 *
 * Returns `present`, `missing` or `broken`. `missing` is reported and does not fail the run;
 * `broken` does. The difference is the whole point: a screen that has not landed yet is a
 * status, and a screen that has landed and renders wrong is a defect.
 */
async function assertNewScreen(name, opts) {
  const { path, expect = [], absent = [], requireTable = false } = opts;
  const state = await visit(path);

  // A redirect is not a missing screen. These are all routes the audience may reach, so
  // landing somewhere else means the guard or the router disagrees with the route table.
  const redirected = state.path !== path;
  const reason = redirected ? "" : missingReason(state);

  if (reason) {
    screenLedger.push({ name, path, status: "missing", reason });
    console.log(`skip ${name} (${path}) — not in this build: ${reason}`);
    return { status: "missing", state };
  }

  const before = failures.length;
  if (redirected) {
    check(`${name}: stays on ${path}`, false, `redirected to ${state.path}`);
  }
  check(`${name}: no error state`, !state.errorText, state.errorText ? state.errorText.slice(0, 300) : "");
  check(`${name}: has a heading`, Boolean(state.h1), "no <h1> in the rendered screen");
  check(`${name}: no stringified objects`, !/\[object (Object|HTML\w*Element)\]/.test(state.text), state.text.match(/\[object [^\]]+\]/)?.[0] ?? "");
  // "Renders its heading but no content" is the failure this catches: a page head with an
  // empty body is indistinguishable from a working screen to a test that only looks for a
  // heading, and it is what a half-finished view actually looks like.
  check(`${name}: renders content`, state.contentNodes > 0, `no card, table, empty state or banner (${state.contentNodes})`);
  if (requireTable) check(`${name}: renders a table`, state.hasTable, "no .table");

  const haystack = state.text.toLowerCase();
  for (const needle of expect) {
    check(`${name}: shows ${JSON.stringify(needle)}`, haystack.includes(String(needle).toLowerCase()));
  }
  for (const needle of absent) {
    check(`${name}: does not show ${JSON.stringify(needle)}`, !haystack.includes(String(needle).toLowerCase()));
  }
  if (state.hasEmpty) {
    console.log(`     note: ${name} rendered its empty state (${state.emptyTitle})`);
  }
  await screenshot(name.toLowerCase().replace(/[^a-z0-9]+/g, "-"));

  const status = failures.length > before ? "broken" : "present";
  screenLedger.push({ name, path, status, reason: status === "broken" ? failures.slice(before).join("; ") : "" });
  return { status, state };
}

/**
 * The mobile pass for one screen: a table must become cards, and nothing may scroll
 * sideways. A screen without a table is skipped rather than failed — the layout rule only
 * applies where there is a table to lay out.
 */
async function assertNoOverflow(name, path) {
  const state = await visit(path);
  if (state.path !== path || missingReason(state)) return;
  check(`mobile: ${name} does not scroll sideways`, !state.overflow);
  if (state.hasTable && state.rowDisplay) {
    check(
      `mobile: ${name} turns its table into cards`,
      state.rowDisplay === "block" || state.rowDisplay === "flex",
      state.rowDisplay,
    );
  }
}

// ---------------------------------------------------------------- the run --

try {
  // The storefront, as a stranger. This runs before any sign-in because it is the one surface
  // that must work with no session at all, and it is easy to break by making "/" require one.
  const storefront = await assertScreen("storefront", { path: "/", expect: ["Servers that come with a ledger"] });
  check("storefront: shows a category filter", /All products/.test(storefront));
  if (arg("expect-product")) {
    check("storefront: lists the published product", storefront.includes(arg("expect-product")));
  }
  if (arg("expect-hidden")) {
    check("storefront: does not list a hidden product", !storefront.includes(arg("expect-hidden")));
  }
  // A product page must render for a stranger, not bounce to a login screen.
  if (arg("expect-product-slug")) {
    await goto(`/products/${arg("expect-product-slug")}`);
    const productPage = await pageText();
    check("storefront: the product page is public", productPage.includes(arg("expect-product")));
    check("storefront: the product page shows a price", /per month|per year|\/ monthly|\/ annual/i.test(productPage));
  }

  // Self-registration, as a stranger, before any sign-in. This is the one screen that turns
  // a visitor into a customer, and it is public — so it is driven here rather than after the
  // staff sign-in, where a stale session would silently take over the form.
  //
  // The endpoint never says whether an address already existed (that would be an account
  // discovery oracle), so there is no "signed-in landing" to assert. Success is one of two
  // states: "check your email" when verification is required, or "check your email, then
  // sign in" when it is not. Both are the accepted state and both are what this asserts.
  {
    const registerEmail = `e2e-register-${Date.now()}@example.test`;
    const state = await visit("/register");
    const reason = state.path === "/register" ? missingReason(state) : "";
    if (reason || state.path !== "/register") {
      screenLedger.push({
        name: "register",
        path: "/register",
        status: "missing",
        reason: reason || `redirected to ${state.path}`,
      });
      console.log(`skip register (/register) — not in this build: ${reason || `redirected to ${state.path}`}`);
    } else {
      const before = failures.length;
      check("register: no error state", !state.errorText, state.errorText ? state.errorText.slice(0, 300) : "");
      check("register: has a heading", Boolean(state.h1), "no <h1>");
      check("register: renders its form", (await evaluate("document.querySelectorAll('.auth-form input').length")) > 0);
      await typeInto("#name", "E2E Registration");
      await typeInto("#email", registerEmail);
      await typeInto("#password", "e2e-registration-password-1");
      // The terms box is a real requirement on installations that configure terms, so it is
      // ticked when it exists rather than assumed to be optional.
      const tos = await evaluate("document.querySelector('#tos') !== null");
      if (tos) await click("#tos");
      await click('.auth-form .btn--primary');
      const accepted = await until(
        "the accepted state after registering",
        "/check your email/i.test(document.body.innerText) || document.querySelector('.banner') !== null",
        10_000,
      ).catch(() => false);
      const after = await pageText();
      check("register: submitting shows the accepted state", Boolean(accepted), after.slice(0, 300));
      check("register: says to check the email", /check your email/i.test(after), after.slice(0, 300));
      // The endpoint is deliberately non-enumerating, so the page must not claim an account
      // was created. This is the sentence that keeps the privacy promise visible.
      check(
        "register: does not claim the account exists",
        !/your account has been created|account created successfully/i.test(after),
      );
      await screenshot("register-accepted");
      const status = failures.length > before ? "broken" : "present";
      screenLedger.push({ name: "register", path: "/register", status, reason: "" });
    }
  }

  // Sign in through the form, which is what proves the login screen works rather than just
  // the API behind it.
  await goto("/login");
  await typeInto("#email", staffEmail);
  await typeInto("#password", staffPassword);
  await click('.auth-form .btn--primary');
  await until("the shell after sign-in", "document.querySelector('.app') !== null", 12_000);

  const staffHome = await assertScreen("dashboard", {
    expect: ["Overview", "Needs attention"],
  });
  check("dashboard: renders the exception list or says it is clear", /Needs attention|Nothing is on fire/.test(staffHome));

  // Navigation by clicking the rail, not by URL: the rail is what a user uses.
  await click('.nav-item[data-href="/customers"]');
  await until("the customer list", "location.pathname === '/customers' && document.querySelector('table, .empty') !== null");
  await assertScreen("customers", { expect: ["Customers"] });

  const customerEmail = arg("expect-customer");
  if (customerEmail) {
    const text = await pageText();
    check("customers: lists the seeded customer", text.includes(customerEmail));
    await click(`a[href^="/customers/"]`);
    await until("the customer detail", "location.pathname.startsWith('/customers/') && document.querySelector('.page') !== null");
    await until("the customer detail data", "document.querySelector('.page-skeleton') === null", 10_000);
    const detail = await pageText();
    check("customer detail: shows the email", detail.includes(customerEmail));
    check("customer detail: has tabs", /Overview/.test(detail) && /Invoices/.test(detail));

    // An operator has to be able to see what the customer sees. Follow the link rather than
    // guessing the URL, because a link that points nowhere is the failure worth catching.
    // The link is rendered with the customer's data, so waiting for the page shell is not
    // enough: clicking before it exists silently does nothing and the next assertion times out
    // with a misleading message.
    await until(
      "the link to the customer's portal",
      "document.querySelector('a[href^=\"/customers/\"][href$=\"/portal\"]') !== null",
      12_000,
    );
    await click('a[href^="/customers/"][href$="/portal"]');
    await until(
      "the staff portal view",
      "location.pathname.endsWith('/portal') && document.querySelector('.page-skeleton') === null",
      12_000,
    );
    const viewed = await pageText();
    check("staff portal view: says it is a staff view", /viewing this as staff/i.test(viewed));
    check("staff portal view: shows the customer", viewed.includes(customerEmail));
    check("staff portal view: shows a balance", /Outstanding/i.test(viewed));
    // Read-only is the contract. A Pay button here would be impersonation that can move money.
    check("staff portal view: offers no payment action", !/\bPay now\b/i.test(viewed));
  }

  const invoiceNumber = arg("expect-invoice");
  await goto("/invoices");
  if (invoiceNumber) {
    const text = await pageText();
    check("invoices: lists the seeded invoice", text.includes(invoiceNumber));
    await click('a[href^="/invoices/"]:not([href$="/new"])');
    await until("the invoice document", "document.querySelector('.invoice-doc') !== null", 10_000);
    const doc = await pageText();
    check("invoice: renders the document", doc.includes(invoiceNumber));
    check("invoice: shows the totals", /Total/.test(doc) && /Outstanding/.test(doc));
    check("invoice: offers an action", /Credit note|Apply credit|Void/.test(doc));
  } else {
    await assertScreen("invoices", { expect: ["Invoices"] });
  }

  const serviceExternalId = arg("expect-service");
  // Filtered to the seeded customer: the list is cursor-paginated newest-first, so asserting
  // that a particular row is on page one is an assertion about how many records the database
  // happens to hold, not about the screen. A filter makes the row the only one.
  await goto(serviceCustomerId ? `/services?customer_id=${serviceCustomerId}` : "/services");
  if (serviceExternalId) {
    const text = await pageText();
    check("services: lists the seeded service", text.includes(serviceExternalId));
    await click('a[href^="/services/"]');
    await until("the service detail", "location.pathname.startsWith('/services/') && document.querySelector('.page') !== null");
    await until("the service detail data", "document.querySelector('.page-skeleton') === null", 10_000);
    const detail = await pageText();
    check("service detail: shows the external id", detail.includes(serviceExternalId));
    check("service detail: offers the lifecycle actions", /Suspend|Resume|Terminate/.test(detail));
  } else {
    await assertScreen("services", { expect: ["Services"] });
  }

  // A manual invoice can sell a product, which is what makes it provision on payment. The
  // picker must actually be populated: an empty one silently produces an invoice that
  // delivers nothing, which is the defect this control exists to prevent.
  await goto("/invoices/new");
  const lineProductOptions = await evaluate(
    "[...document.querySelectorAll('select[id$=\"-product\"] option')].map(o => o.text).join(' | ')",
  );
  check(
    "invoice: the line can name a product",
    lineProductOptions.split("|").length > 1,
    `options: ${lineProductOptions}`,
  );
  check("invoice: a line with no product says so", /None/.test(lineProductOptions), lineProductOptions);

  // The catalog, reached the way a person reaches it: click the rail, then open a product.
  // This is the path that was broken — the old read-only screens stayed linked while the API
  // shape changed under them, so opening a product threw and the write buttons were absent.
  await click('.nav-item[data-href="/catalog-management"]');
  await until("the catalog screen", "location.pathname === '/catalog-management' && document.querySelector('.page-head') !== null");
  const catalogActions = await evaluate("[...document.querySelectorAll('.page-actions .btn')].map(b => b.innerText).join(' | ')");
  check("catalog: offers to create a product", /New product/.test(catalogActions), catalogActions);
  check("catalog: offers to create a category", /New category/.test(catalogActions), catalogActions);
  const firstProduct = await evaluate("document.querySelector('.cat-name a')?.getAttribute('href') ?? ''");
  if (firstProduct) {
    await click(".cat-name a");
    await until("the product editor", "location.pathname.startsWith('/catalog-management/') && document.querySelector('.page-title') !== null");
    await until("the product editor data", "document.querySelector('.skeleton') === null", 10_000);
    const editorText = await pageText();
    check("catalog: a product opens without an error", !(await evaluate("document.querySelector('.error-state') !== null")));
    check("catalog: the product editor shows its price versions", /Price versions/.test(editorText));
    // The old page threw "Cannot read properties of undefined (reading 'name')" here.
    check("catalog: the product editor names the product", /VPS|Managed|Storage|E2E/.test(editorText));
  }

  // The retired URLs must still land on the new screens: they were linked from services and
  // orders, so a bookmark or an old link cannot be allowed to 404.
  await goto("/catalog");
  const legacy = await pageText();
  check("catalog: /catalog still reaches the catalog screen", /New product/.test(legacy) || /Products/.test(legacy));

  // Every remaining staff screen: rendered, with a heading and no error state.
  for (const [name, path, expect] of [
    ["queue", "/operations", ["Operation queue"]],
    ["webhooks", "/webhooks", ["Webhook"]],
    ["payments", "/payments", ["Payments"]],
    ["orders", "/orders", ["Orders"]],
    ["catalog", "/catalog-management", ["Catalog", "Products"]],
    ["approvals", "/approvals", ["Approvals"]],
    ["audit", "/audit", ["Audit log"]],
    ["api-keys", "/api-keys", ["API keys"]],
    ["settings", "/settings", ["Account settings"]],
  ]) {
    await assertScreen(name, { path, expect });
  }

  // ---------------------------------------------------- the new staff screens --
  //
  // The screens the parallel packages built. Each is tolerated as absent, but a screen that
  // is present and renders wrong fails the run — see `assertNewScreen`.
  //
  // The seed gives each of these data, so an empty state here is a finding rather than a
  // fixture gap: a coupon, a tax rate, a currency, a config option on the seeded product, a
  // ticket with a message and an announcement. `expect` names the seeded value, which is what
  // proves the screen read the database rather than rendering an empty shell.
  const seededCoupon = arg("expect-coupon");
  const seededTaxRate = arg("expect-tax-rate");
  const seededCurrency = arg("expect-currency");
  const seededTicket = arg("expect-ticket");
  const seededAnnouncement = arg("expect-announcement");
  const seededConfigProduct = arg("expect-config-product");

  await assertNewScreen("coupons", {
    path: "/coupons",
    expect: ["Coupons", ...(seededCoupon ? [seededCoupon] : [])],
    requireTable: true,
  });

  await assertNewScreen("tax-rates", {
    path: "/tax-rates",
    expect: ["Tax rates", ...(seededTaxRate ? [seededTaxRate] : [])],
    requireTable: true,
  });

  await assertNewScreen("currencies", {
    path: "/currencies",
    expect: ["Currencies", ...(seededCurrency ? [seededCurrency] : [])],
    requireTable: true,
  });

  // Config options are per product, so the screen is opened for the seeded product. Without
  // the query the screen correctly asks which product to configure, which proves nothing
  // about the options themselves.
  if (seededConfigProduct) {
    await assertNewScreen("config-options", {
      path: `/config-options?product_id=${encodeURIComponent(seededConfigProduct)}`,
      expect: ["Config options"],
    });
  } else {
    await assertNewScreen("config-options", { path: "/config-options", expect: ["Config options"] });
  }

  await assertNewScreen("system-settings", {
    path: "/settings/system",
    expect: ["System settings"],
  });

  // The staff ticket queue and the all-tickets list are different screens over the same
  // table; both must render the seeded ticket.
  await assertNewScreen("support-queue", {
    path: "/support",
    expect: ["Support", ...(seededTicket ? [seededTicket] : [])],
  });
  await assertNewScreen("tickets-staff", {
    path: "/tickets",
    expect: ["Tickets", ...(seededTicket ? [seededTicket] : [])],
  });

  await assertNewScreen("logs", { path: "/logs", expect: ["Logs"] });

  await assertNewScreen("announcements", {
    path: "/announcements",
    expect: ["Announcements", ...(seededAnnouncement ? [seededAnnouncement] : [])],
  });

  await assertNewScreen("cancellations", {
    path: "/cancellations",
    expect: ["Cancellations"],
  });

  // The staff credit screen is the one that has not landed: it is a placeholder module. It is
  // in the list deliberately so the report says so rather than omitting it.
  await assertNewScreen("credits-staff", { path: "/credits", expect: ["Credit"] });

  // A settings screen that offers to save must actually be able to: the form fields come
  // from the settings API, so an empty form is a broken screen even though it has a heading.
  await visit("/settings/system");
  if (!missingReason(await screenState())) {
    const fieldCount = await evaluate("document.querySelectorAll('.settings-nav button, .settings-nav a, input[id], select[id]').length");
    check("system-settings: renders editable fields", fieldCount > 0, `controls=${fieldCount}`);
  }

  // Buying a product: the screen the catalog was missing. It is a form over the catalog, so
  // assert it loaded real products rather than only that a heading rendered.
  await goto("/orders/new");
  await until("the order form", "document.querySelector('#order-product') !== null", 10_000);
  // The form renders before the catalog fetch resolves, so wait for the options rather than
  // for the element: asserting on an empty select would be a race, not a test.
  await until("the catalog to load", "document.querySelectorAll('#order-product option').length > 0", 10_000);
  const productOptions = await evaluate("document.querySelectorAll('#order-product option').length");
  check("order-new: lists catalog products", productOptions > 0, `options=${productOptions}`);
  const priceOptions = await evaluate("document.querySelectorAll('#order-price option').length");
  check("order-new: lists the product's prices", priceOptions > 0, `options=${priceOptions}`);
  await assertScreen("order-new", { expect: ["New order", "Billing account"] });

  // The command palette: opened by keyboard, searched, and used to navigate.
  await goto("/");
  await send("Input.dispatchKeyEvent", { type: "keyDown", key: "k", code: "KeyK", windowsVirtualKeyCode: 75, modifiers: 2 }, session);
  await send("Input.dispatchKeyEvent", { type: "keyUp", key: "k", code: "KeyK", windowsVirtualKeyCode: 75, modifiers: 2 }, session);
  const paletteOpen = await until("the palette", "document.querySelector('.palette') !== null", 4000).catch(() => false);
  check("palette: opens on Ctrl+K", Boolean(paletteOpen));
  if (paletteOpen) {
    await send("Input.insertText", { text: "invo" }, session);
    await until("palette results", "document.querySelectorAll('.palette-item').length > 0", 4000);
    await send("Input.dispatchKeyEvent", { type: "keyDown", key: "Enter", code: "Enter", windowsVirtualKeyCode: 13 }, session);
    await send("Input.dispatchKeyEvent", { type: "keyUp", key: "Enter", code: "Enter", windowsVirtualKeyCode: 13 }, session);
    const navigated = await until("navigation from the palette", "location.pathname.startsWith('/invoice')", 5000).catch(() => false);
    check("palette: Enter navigates to the match", Boolean(navigated));
  }

  // The theme toggle must actually change the theme, not just the icon.
  await goto("/");
  const before = await evaluate("document.documentElement.dataset.theme");
  await click("#theme-toggle");
  const after = await evaluate("document.documentElement.dataset.theme");
  check("theme: the toggle switches the theme", before !== after, `${before} -> ${after}`);
  await click("#theme-toggle");

  // A mobile viewport must rebuild the table into cards rather than overflow.
  await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true }, session);
  await goto("/customers");
  const mobile = await evaluate(`(() => {
    const table = document.querySelector('.table');
    if (!table) return { skipped: true };
    const style = getComputedStyle(table.querySelector('tbody tr'));
    const overflow = document.documentElement.scrollWidth > window.innerWidth + 1;
    return { display: style.display, overflow };
  })()`);
  if (!mobile.skipped) {
    check("mobile: the table becomes cards", mobile.display === "block" || mobile.display === "flex", mobile.display);
    check("mobile: the page does not scroll sideways", !mobile.overflow);
  }
  await screenshot("mobile-customers");

  // The new screens get the same phone treatment. Each of these is a table on a desktop and
  // must be a card list at 390px; a table that keeps its columns makes the page scroll
  // sideways, which on a phone means the last column is unreachable. A screen that is not in
  // this build is skipped by name rather than counted as passing.
  for (const [name, path] of [
    ["coupons", "/coupons"],
    ["tax-rates", "/tax-rates"],
    ["currencies", "/currencies"],
    ["support-queue", "/support"],
    ["tickets", "/tickets"],
    ["announcements", "/announcements"],
    ["cancellations", "/cancellations"],
    ["system-settings", "/settings/system"],
    ["logs", "/logs"],
  ]) {
    await assertNoOverflow(name, path);
  }
  await send("Emulation.setDeviceMetricsOverride", { width: 1440, height: 900, deviceScaleFactor: 1, mobile: false }, session);

  // The customer portal, as a customer. This is the surface with the least traffic in review
  // and the most distinct permissions, and it is where a null role list crashed the shell.
  if (customerEmail && customerPassword) {
    await goto("/");
    await click(".sidebar-foot .who");
    await click(".menu-item--danger");
    await until("the sign-in screen", "document.querySelector('.auth-panel') !== null", 8000);
    await typeInto("#email", customerEmail);
    await typeInto("#password", customerPassword);
    await click(".auth-form .btn--primary");
    await until("the portal after sign-in", "document.querySelector('.app') !== null", 12_000);
    // The portal hero is rendered from its own request, after the shell. Waiting for the shell
    // alone let the assertions read a page that had not fetched its balance yet, which failed
    // intermittently. Waiting for the hero's data is the honest barrier.
    await until(
      "the portal to load its balance",
      "document.querySelector('.portal-balance') !== null || document.querySelector('.error-state') !== null",
      12_000,
    );

    const portal = await assertScreen("portal", { expect: ["Outstanding"] });
    check("portal: greets the customer", /Good (morning|afternoon|evening)|Working late/.test(portal));
    for (const [name, path] of [
      ["portal-invoices", "/portal/invoices"],
      ["portal-services", "/portal/services"],
      ["portal-profile", "/portal/profile"],
    ]) {
      await assertScreen(name, { path, expect: [] });
    }

    // ------------------------------------------------ the customer's new screens --
    //
    // The cart and checkout are seeded with a line by the shell script, so "Your cart is
    // empty" here is a finding, not a fixture gap. The ticket list must show the seeded
    // ticket. The account screens are allowed to be empty — an empty session list or an
    // empty inbox is a correct state — but they must render their structure.
    const seededCartProduct = arg("expect-cart-product");
    const seededTicketNumber = arg("expect-ticket");

    await assertNewScreen("cart", {
      path: "/cart",
      expect: ["Your cart", ...(seededCartProduct ? [seededCartProduct] : [])],
      absent: ["Your cart is empty"],
    });

    await assertNewScreen("checkout", {
      path: "/checkout",
      expect: ["Checkout"],
      absent: ["There is nothing to check out"],
    });

    await assertNewScreen("tickets", {
      path: "/tickets",
      expect: ["Support tickets", ...(seededTicketNumber ? [seededTicketNumber] : [])],
    });

    await assertNewScreen("ticket-new", {
      path: "/tickets/new",
      expect: ["Open a ticket", "Subject"],
    });

    // The form must be submittable, which means the fields exist and are enabled. A ticket
    // form that renders its labels with no inputs is the defect this catches.
    await visit("/tickets/new");
    if (!missingReason(await screenState())) {
      const fields = await evaluate(
        "document.querySelectorAll('input[id], textarea[id], select[id]').length",
      );
      check("ticket-new: renders its fields", fields > 0, `fields=${fields}`);
    }

    await assertNewScreen("portal-credits", { path: "/portal/credits", expect: ["Credit"] });

    await assertNewScreen("portal-security", {
      path: "/portal/security",
      expect: ["Security"],
    });

    await assertNewScreen("portal-notifications", {
      path: "/portal/notifications",
      expect: ["Notifications"],
    });

    await assertNewScreen("portal-payment-methods", {
      path: "/portal/payment-methods",
      expect: ["Payment methods"],
    });

    // A customer must not reach any staff screen, old or new. The old list covered
    // /customers; the new screens are the ones a mis-scoped route guard would expose, so
    // each is tried explicitly and must land back inside the portal.
    for (const staffPath of [
      "/customers",
      "/coupons",
      "/tax-rates",
      "/currencies",
      "/config-options",
      "/settings/system",
      "/support",
      "/logs",
      "/announcements",
      "/cancellations",
      "/credits",
    ]) {
      await visit(staffPath);
      const landed = await evaluate("location.pathname");
      check(`portal: a customer cannot reach ${staffPath}`, landed.startsWith("/portal"), landed);
    }
  }

  // Nothing threw anywhere in the run.
  check("no uncaught exceptions", exceptions.length === 0, exceptions.join(" | "));
  // Console errors are reported but only fail when they are not the expected 401 probe: the app
  // asks /auth/me once to decide whether a session exists, and a signed-out answer is a 401.
  const unexpected = consoleErrors.filter((line) => !/401 \(Unauthorized\)/.test(line));
  check("no unexpected console errors", unexpected.length === 0, unexpected.join(" | "));
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

console.log(`\n${passes.length} passed, ${failures.length} failed`);

// The screen ledger, printed whether the run passed or failed. This is the part that makes
// the harness usable while the work lands in stages: it says which screens were present,
// which were not, and which were present and wrong — by name, with the reason. A run that
// only printed its failures would leave "8 of 19" to be counted from the log.
if (screenLedger.length > 0) {
  const order = { present: 0, broken: 1, missing: 2 };
  const sorted = [...screenLedger].sort((a, b) => order[a.status] - order[b.status]);
  const count = (s) => screenLedger.filter((e) => e.status === s).length;
  console.log(
    `\nnew screens: ${count("present")} present, ${count("broken")} broken, ${count("missing")} not in this build`,
  );
  for (const entry of sorted) {
    const mark = entry.status === "present" ? "ok   " : entry.status === "broken" ? "FAIL " : "skip ";
    const detail = entry.reason ? ` — ${entry.reason}` : "";
    console.log(`  ${mark}${entry.name.padEnd(22)} ${entry.path}${detail}`);
  }
}

// Every console error, attributed to the screen that was showing. Reported even when the run
// passes, because "no unexpected console errors" only says none was unexpected.
if (consoleScreenErrors.length > 0) {
  console.log("\nconsole errors:");
  for (const { screen, message } of consoleScreenErrors) {
    console.log(`  - [${screen}] ${message}`);
  }
}

if (failures.length > 0) {
  console.log("\nfailures:");
  for (const f of failures) console.log(`  - ${f}`);
  process.exit(1);
}
console.log("ui e2e: ok");
