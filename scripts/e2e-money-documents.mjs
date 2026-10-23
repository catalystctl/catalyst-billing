#!/usr/bin/env node
// Drives the money-document and small-CRUD affordances in a real browser.
//
// The Go tests prove the contracts; `e2e-ui.mjs` proves the screens render. Neither clicks a
// button. This script clicks them: it signs in through the form, opens an invoice, writes a
// client note, issues a credit note and downloads it, issues a proforma, creates/renames/deletes
// a catalogue server, unlinks a sign-in provider, and then does the customer's half — reads the
// note, downloads the invoice and the credit note.
//
// It is deliberately separate from e2e-ui.mjs: that harness asserts on screens, and adding a
// scenario that writes money documents to it would change what its other assertions see. This
// one owns its fixtures (see browser-money-documents.sh) and asserts only on the flow it drives.
//
// Usage: node scripts/e2e-money-documents.mjs --base=URL --staff=EMAIL --staff-password=PASS \
//          --customer=EMAIL --customer-password=PASS --issued-invoice=UUID --draft-invoice=UUID
//          [--screenshots=dir]

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
const customerPassword = arg("customer-password");
const issuedInvoice = arg("issued-invoice");
const draftInvoice = arg("draft-invoice");
const shotDir = arg("screenshots");

if (!staffEmail || !staffPassword || !customerEmail || !customerPassword || !issuedInvoice || !draftInvoice) {
  console.error(
    "usage: e2e-money-documents.mjs --base=URL --staff=EMAIL --staff-password=PASS " +
      "--customer=EMAIL --customer-password=PASS --issued-invoice=UUID --draft-invoice=UUID",
  );
  process.exit(2);
}
if (shotDir) mkdirSync(shotDir, { recursive: true });

const chrome = process.env.CHROME || "chromium";
const port = 9900 + Math.floor(Math.random() * 400);
const profile = `/tmp/billing-money-e2e-${Date.now()}`;

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
child.stderr.on("data", () => {});

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

const ws = new WebSocket(await waitForEndpoint());
await new Promise((resolve, reject) => {
  ws.onopen = resolve;
  ws.onerror = reject;
});

let nextId = 1;
const pending = new Map();
const consoleErrors = [];
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
    exceptions.push(data.params.exceptionDetails?.exception?.description ?? data.params.exceptionDetails?.text);
  }
  if (data.method === "Runtime.consoleAPICalled" && data.params.type === "error") {
    consoleErrors.push((data.params.args ?? []).map((a) => a.value ?? a.description ?? "").join(" "));
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
  // The page's own words are the useful part of a timeout: a redirect, an error state and a
  // blank shell all look identical from here otherwise.
  const where = await evaluate("location.href").catch(() => "?");
  const text = await evaluate("document.body ? document.body.innerText.slice(0, 300) : ''").catch(() => "");
  throw new Error(
    `timed out waiting for ${description} (last: ${JSON.stringify(last)}) at ${where} — ${JSON.stringify(text)}`,
  );
}

async function goto(path, timeoutMs = 12_000) {
  currentScreen = path;
  await send("Page.navigate", { url: `${base}${path}` }, session);
  await until(`the app to render on ${path}`, "document.querySelector('.app, .auth-panel')", timeoutMs);
  await until(
    `the screen on ${path} to settle`,
    "!document.querySelector('.skeleton') && !document.querySelector('.table-loading')",
    timeoutMs,
  );
  await until(
    `the screen on ${path} to have content`,
    "document.querySelector('.page-head, .card, .table, .empty, .error-state, .invoice-doc, .auth-panel') !== null",
    timeoutMs,
  );
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

/** Clicks the first element matching `selector` whose text matches `pattern`. */
async function clickByText(selector, pattern, { within = "document" } = {}) {
  const found = await until(
    `a ${selector} matching ${pattern}`,
    `(() => {
      const root = ${within};
      const els = Array.from(root.querySelectorAll(${JSON.stringify(selector)}));
      const hit = els.find((el) => ${pattern}.test((el.textContent || "").trim()));
      if (!hit) return false;
      hit.scrollIntoView({ block: "center" });
      const r = hit.getBoundingClientRect();
      window.__hit = { x: r.x + r.width / 2, y: r.y + r.height / 2 };
      return true;
    })()`,
  );
  if (!found) throw new Error(`no ${selector} matching ${pattern}`);
  const box = await evaluate("window.__hit");
  for (const type of ["mousePressed", "mouseReleased"]) {
    await send("Input.dispatchMouseEvent", { type, x: box.x, y: box.y, button: "left", clickCount: 1 }, session);
  }
}

async function typeInto(selector, text) {
  await click(selector);
  await send("Input.insertText", { text }, session);
}

async function fill(selector, text) {
  await until(`the field ${selector}`, `document.querySelector(${JSON.stringify(selector)}) !== null`);
  await evaluate(`(() => {
    const el = document.querySelector(${JSON.stringify(selector)});
    el.focus();
    el.value = "";
    el.dispatchEvent(new Event("input", { bubbles: true }));
  })()`);
  await send("Input.insertText", { text }, session);
}

async function screenshot(name) {
  if (!shotDir) return;
  const { result } = await send("Page.captureScreenshot", { format: "png" }, session);
  if (result?.data) writeFileSync(`${shotDir}/${name}.png`, Buffer.from(result.data, "base64"));
}

/** Signs in through the form, exactly as a person does. */
async function signIn(email, password) {
  await goto("/login");
  await typeInto("#email", email);
  await typeInto("#password", password);
  await click(".auth-form .btn--primary");
  await until("the shell after sign-in", "document.querySelector('.app') !== null", 15_000);
  await until("the session's screens", "document.querySelector('.skeleton') === null", 15_000);
}

/** Signs out, so the next flow starts from a clean session rather than a stale one. */
async function signOut() {
  // The logout route is a session mutation, so it carries the CSRF token the app itself would
  // send. The cookie is the token's own copy, which is what the client falls back to.
  await evaluate(`(async () => {
    const token = document.cookie.split("; ").find((c) => c.startsWith("billing_csrf="))?.split("=")[1];
    await fetch("/api/v1/auth/logout", {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", ...(token ? { "X-CSRF-Token": decodeURIComponent(token) } : {}) },
      body: "{}",
    });
  })()`);
  await goto("/login");
  await until("the sign-in form", "document.querySelector('#email') !== null", 10_000);
}

/** Fetches a document inside the page, so the request carries the session the browser holds. */
async function fetchDocument(href) {
  return evaluate(`(async () => {
    const res = await fetch(${JSON.stringify(href)}, { credentials: "same-origin" });
    const buf = await res.arrayBuffer();
    const head = new TextDecoder().decode(new Uint8Array(buf).slice(0, 5));
    return { status: res.status, type: res.headers.get("content-type") || "", bytes: buf.byteLength, head };
  })()`);
}

// ---------------------------------------------------------------- assertions --

const failures = [];
let passCount = 0;

function check(name, condition, detail = "") {
  if (condition) {
    passCount += 1;
    console.log(`ok   ${name}`);
    return true;
  }
  failures.push(`${name}${detail ? ` — ${detail}` : ""}`);
  console.log(`FAIL ${name}${detail ? ` — ${detail}` : ""}`);
  return false;
}

// ------------------------------------------------------------------- staff --

await signIn(staffEmail, staffPassword);
check("staff: signed in through the form", await evaluate("document.querySelector('.app') !== null"));

// ------------------------------------------------ the invoice's own documents --

await goto(`/invoices/${issuedInvoice}`);
await until("the invoice document", "document.querySelector('.invoice-doc') !== null");

// The document download. It is an anchor carrying `download`, so the router leaves it alone
// and the browser saves the file rather than the SPA trying to render it.
const invoicePdfHref = await evaluate(`(() => {
  const a = Array.from(document.querySelectorAll('a[href]'))
    .find((x) => x.getAttribute('href') === '/api/v1/staff/invoices/${issuedInvoice}/pdf');
  return a ? { href: a.getAttribute('href'), download: a.hasAttribute('download'), native: a.dataset.native } : null;
})()`);
check("invoice: offers a PDF download", invoicePdfHref !== null, JSON.stringify(invoicePdfHref));
if (invoicePdfHref) {
  check("invoice: the PDF link is a native download", invoicePdfHref.download && invoicePdfHref.native === "true");
  const doc = await fetchDocument(invoicePdfHref.href);
  check(
    "invoice: the staff PDF is served",
    doc.status === 200 && doc.type.includes("application/pdf") && doc.head === "%PDF-",
    JSON.stringify(doc),
  );
}

// The client-visible note: write it from this screen, then read it back from the document the
// screen reprints. A note that saves but does not appear is a note nobody can proof-read.
const noteText = "The setup fee was waived on this invoice; see the credit note for the correction.";
await clickByText("button", "/client note|edit note/i");
await until("the client-note dialog", "document.querySelector('#client-note') !== null");
await fill("#client-note", noteText);
await clickByText(".dialog-foot button", "/save note/i");
await until("the note to be saved", "document.querySelector('.dialog') === null", 10_000);
await until(
  "the note to appear on the document",
  `document.querySelector('.invoice-doc') && document.querySelector('.invoice-doc').innerText.includes(${JSON.stringify(noteText)})`,
  10_000,
);
check(
  "invoice: the client note is written and reprinted on the document",
  await evaluate(
    `document.querySelector('.invoice-doc').innerText.includes(${JSON.stringify(noteText)})`,
  ),
);
await screenshot("invoice-note");

// The credit note: issue one from the invoice, then download the document it produced.
await clickByText("button", "/^credit note$/i");
await until("the credit-note dialog", "document.querySelector('#credit-note-reason') !== null");
await fill("#credit-note-amount", "5.00");
await fill("#credit-note-reason", "Setup fee waived as a goodwill gesture.");
await clickByText(".dialog-foot button", "/issue credit note/i");
await until("the credit note to be issued", "document.querySelector('.dialog') === null", 12_000);
await until(
  "the credit note to appear in the list",
  `Array.from(document.querySelectorAll('a[href^="/api/v1/staff/credit-notes/"]')).length > 0`,
  12_000,
);
const creditNotePdf = await evaluate(`(() => {
  const a = document.querySelector('a[href^="/api/v1/staff/credit-notes/"]');
  return a ? { href: a.getAttribute('href'), download: a.hasAttribute('download') } : null;
})()`);
check("credit note: the issued note is listed with a PDF", creditNotePdf !== null, JSON.stringify(creditNotePdf));
if (creditNotePdf) {
  const doc = await fetchDocument(creditNotePdf.href);
  check(
    "credit note: the staff PDF is served",
    doc.status === 200 && doc.type.includes("application/pdf") && doc.head === "%PDF-",
    JSON.stringify(doc),
  );
}
await screenshot("invoice-credit-note");

// ------------------------------------------------------------- the proforma --

await goto(`/invoices/${draftInvoice}`);
await until("the draft invoice", "document.querySelector('.invoice-doc') !== null");
const proformaOffered = await evaluate(
  `Array.from(document.querySelectorAll('button')).some((b) => /issue proforma/i.test(b.textContent || ''))`,
);
check("draft: offers to issue a proforma", proformaOffered);

if (proformaOffered) {
  await clickByText("button", "/issue proforma/i");
  await until("the proforma dialog", "document.querySelector('#proforma-due-days') !== null");
  await fill("#proforma-due-days", "14");
  await clickByText(".dialog-foot button", "/issue proforma/i");
  await until("the proforma to be issued", "document.querySelector('.dialog') === null", 12_000);
  await until(
    "the page to report the proforma",
    "document.body.innerText.includes('This is a proforma')",
    12_000,
  );
  check(
    "proforma: the invoice is issued as a provisional document",
    await evaluate("document.body.innerText.includes('This is a proforma')"),
  );
  const number = await evaluate(`(() => {
    const dd = document.querySelector('.doc-meta dd');
    return dd ? dd.innerText.trim() : "";
  })()`);
  check("proforma: it carries no invoice number", number === "Proforma" || number === "", `number=${number}`);
  await screenshot("invoice-proforma");
}

// ------------------------------------------------------------ catalogue server --

await goto("/catalog-management");
await until("the servers card", "document.body.innerText.includes('Servers')");
await clickByText("button", "/new server/i");
await until("the server dialog", "document.querySelector('#srv-name') !== null");
const serverName = `E2E Node ${Date.now()}`;
await fill("#srv-name", serverName);
await fill("#srv-slug", `e2e-node-${Date.now()}`);
await fill("#srv-region", "eu-west");
await fill("#srv-node", "node-42");
await clickByText(".dialog-foot button", "/create server/i");
await until("the server to be created", "document.querySelector('.dialog') === null", 12_000);
await until("the server to appear", `document.body.innerText.includes(${JSON.stringify(serverName)})`, 12_000);
check("catalog: a server is created from the catalogue screen", true);

const renamed = `${serverName} renamed`;
await clickByText("button", "/^edit$/i");
await until("the server dialog", "document.querySelector('#srv-name') !== null");
await fill("#srv-name", renamed);
await clickByText(".dialog-foot button", "/save server/i");
await until("the rename to land", `document.body.innerText.includes(${JSON.stringify(renamed)})`, 12_000);
check("catalog: the server is renamed", await evaluate(`document.body.innerText.includes(${JSON.stringify(renamed)})`));
await screenshot("catalog-servers");

await clickByText("button", "/^delete$/i");
await until("the delete confirmation", "document.querySelector('.dialog') !== null");
await clickByText(".dialog-foot button", "/delete server/i");
await until("the server to be deleted", "document.querySelector('.dialog') === null", 12_000);
await until(
  "the server to leave the list",
  `!document.body.innerText.includes(${JSON.stringify(renamed)})`,
  12_000,
);
check("catalog: the server is deleted", true);

// -------------------------------------------------------------- sign-in links --

await goto("/providers");
await until("the linked-accounts card", "document.body.innerText.includes('Your own linked accounts')");
await until(
  "the seeded link to render",
  `Array.from(document.querySelectorAll('button')).some((b) => /unlink/i.test(b.textContent || ''))`,
  10_000,
);
check("providers: a linked provider is listed with an Unlink action", true);
await clickByText("button", "/unlink/i");
await until("the unlink confirmation", "document.querySelector('.dialog') !== null");
await clickByText(".dialog-foot button", "/unlink/i");
await until("the unlink to land", "document.querySelector('.dialog') === null", 12_000);
await until(
  "the card to report no links",
  "document.body.innerText.includes('No provider is linked to your own account')",
  12_000,
);
check("providers: the linked provider is unlinked", true);
await screenshot("providers-unlinked");

// ------------------------------------------------------------- the customer --

await signOut();
await signIn(customerEmail, customerPassword);

await goto(`/portal/invoices/${issuedInvoice}`);
await until("the portal invoice", "document.querySelector('.invoice-doc') !== null");

const portalDownload = await evaluate(`(() => {
  const a = Array.from(document.querySelectorAll('a[href]'))
    .find((x) => x.getAttribute('href') === '/api/v1/portal/invoices/${issuedInvoice}/pdf');
  return a ? { href: a.getAttribute('href'), download: a.hasAttribute('download') } : null;
})()`);
check("portal: offers the invoice download", portalDownload !== null, JSON.stringify(portalDownload));
if (portalDownload) {
  const doc = await fetchDocument(portalDownload.href);
  check(
    "portal: the customer's invoice PDF is served",
    doc.status === 200 && doc.type.includes("application/pdf") && doc.head === "%PDF-",
    JSON.stringify(doc),
  );
}

check(
  "portal: the operator's note appears on the customer's invoice",
  await evaluate(`document.body.innerText.includes(${JSON.stringify(noteText)})`),
);

await until(
  "the credit note section",
  "document.body.innerText.includes('Credit notes') && document.querySelector('a[href^=\"/api/v1/portal/credit-notes/\"]') !== null",
  10_000,
);
const portalCreditNote = await evaluate(
  `document.querySelector('a[href^="/api/v1/portal/credit-notes/"]')?.getAttribute('href') ?? null`,
);
check("portal: the credit note is listed", portalCreditNote !== null, String(portalCreditNote));
if (portalCreditNote) {
  const doc = await fetchDocument(portalCreditNote);
  check(
    "portal: the customer's credit note PDF is served",
    doc.status === 200 && doc.type.includes("application/pdf") && doc.head === "%PDF-",
    JSON.stringify(doc),
  );
}
await screenshot("portal-invoice-documents");

// --------------------------------------------------------------------- report --

check("no uncaught exception anywhere in the run", exceptions.length === 0, exceptions.join(" | "));
check("no console error anywhere in the run", consoleErrors.length === 0, consoleErrors.join(" | "));

child.kill("SIGKILL");

console.log("");
if (failures.length > 0) {
  console.log(`FAILED: ${failures.length} assertion(s)`);
  for (const failure of failures) console.log(`  - ${failure}`);
  process.exit(1);
}
console.log(`money-document browser test passed: ${passCount} assertions`);
