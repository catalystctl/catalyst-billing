#!/usr/bin/env node
// Drives the P11 screens — the ticket list, the new-ticket form, the thread, the staff queue and
// the three customer account pages — in a real browser, signed in as both audiences.
//
// It asserts what the existing suite asserts (a heading, no error block, no stringified objects)
// plus the things only these screens can get wrong: that a customer and a staff member see the
// same route with different columns, that a customer reaching the staff queue gets words rather
// than a red block, that a 404 on the session endpoints renders an explanation, and that the
// thread shows both messages and an attachment link that points at the API.
//
// Usage: node scripts/p11-e2e.mjs --base=URL --staff=EMAIL --staff-password=PASS \
//          --customer=EMAIL --customer-password=PASS --ticket=UUID [--screenshots=dir]

import { spawn } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";

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
const ticketID = arg("ticket");
const shotDir = arg("screenshots");
if (shotDir) mkdirSync(shotDir, { recursive: true });

const chrome = process.env.CHROME_BIN || "chromium";
const port = 9500 + Math.floor(Math.random() * 300);
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
    "--user-data-dir=/tmp/p11-e2e-" + Date.now(),
    "--window-size=1440,900",
    "about:blank",
  ],
  { env, stdio: ["ignore", "ignore", "pipe"] },
);

const deadline = Date.now() + 30_000;
let wsUrl = null;
while (Date.now() < deadline && !wsUrl) {
  try {
    const res = await fetch(`http://127.0.0.1:${port}/json/version`);
    wsUrl = (await res.json()).webSocketDebuggerUrl;
  } catch {
    await new Promise((r) => setTimeout(r, 120));
  }
}
if (!wsUrl) {
  child.kill("SIGKILL");
  console.error("chromium did not expose a debugging endpoint");
  process.exit(1);
}

const ws = new WebSocket(wsUrl);
await new Promise((resolve, reject) => {
  ws.onopen = resolve;
  ws.onerror = reject;
});

let nextId = 1;
const pending = new Map();
const events = [];
ws.onmessage = (msg) => {
  const data = JSON.parse(msg.data);
  if (data.id && pending.has(data.id)) {
    pending.get(data.id)(data);
    pending.delete(data.id);
    return;
  }
  events.push(data);
};

function send(method, params = {}, sessionId) {
  const id = nextId++;
  const payload = { id, method, params };
  if (sessionId) payload.sessionId = sessionId;
  ws.send(JSON.stringify(payload));
  return new Promise((resolve) => pending.set(id, resolve));
}

const { result: targetResult } = await send("Target.createTarget", { url: "about:blank" });
const { result: attachResult } = await send("Target.attachToTarget", { targetId: targetResult.targetId, flatten: true });
const session = attachResult.sessionId;

await send("Runtime.enable", {}, session);
await send("Log.enable", {}, session);
await send("Page.enable", {}, session);
await send("Network.enable", {}, session);

const exceptions = [];
const consoleErrors = [];
ws.addEventListener("message", (msg) => {
  const data = JSON.parse(msg.data);
  if (data.method === "Runtime.exceptionThrown") {
    exceptions.push(data.params.exceptionDetails?.exception?.description ?? data.params.exceptionDetails?.text ?? "");
  }
  if (data.method === "Runtime.consoleAPICalled" && data.params.type === "error") {
    consoleErrors.push((data.params.args ?? []).map((a) => a.value ?? a.description ?? "").join(" "));
  }
  if (data.method === "Log.entryAdded" && data.params.entry.level === "error") {
    consoleErrors.push(`${data.params.entry.text} ${data.params.entry.url ?? ""}`);
  }
});

async function evaluate(expression) {
  const { result } = await send(
    "Runtime.evaluate",
    { expression, returnByValue: true, awaitPromise: true },
    session,
  );
  return result?.result?.value;
}

async function until(description, expression, timeoutMs = 12_000) {
  const stop = Date.now() + timeoutMs;
  let last;
  while (Date.now() < stop) {
    last = await evaluate(`(() => { try { return (${expression}); } catch (e) { return "threw: " + e.message; } })()`);
    if (last) return last;
    await new Promise((r) => setTimeout(r, 80));
  }
  throw new Error(`timed out waiting for ${description} (last: ${JSON.stringify(last)})`);
}

async function goto(path) {
  await send("Page.navigate", { url: `${base}${path}` }, session);
  await until(`the app on ${path}`, "document.querySelector('.page, .auth-panel') !== null");
  await until(`the screen on ${path} to settle`, "!document.querySelector('.skeleton') && !document.querySelector('.table-loading')");
  await new Promise((r) => setTimeout(r, 150));
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

async function typeInto(selector, value) {
  await click(selector);
  await send("Input.insertText", { text: value }, session);
}

async function shot(name) {
  if (!shotDir) return;
  const { result } = await send("Page.captureScreenshot", { format: "png" }, session);
  if (result?.data) writeFileSync(`${shotDir}/${name}.png`, Buffer.from(result.data, "base64"));
}

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

async function screen(name, path, expect = [], opts = {}) {
  await goto(path);
  const text = await evaluate("document.body.innerText");
  const html = await evaluate("document.body.innerHTML");
  const hasError = await evaluate("document.querySelector('.error-state') !== null");
  check(`${name}: no error state`, !hasError, hasError ? await evaluate("document.querySelector('.error-state').innerText") : "");
  check(`${name}: has a heading`, /<h1/i.test(html), "no h1");
  check(`${name}: no stringified objects`, !/\[object (Object|HTML\w*Element)\]/.test(text), text.match(/\[object [^\]]+\]/)?.[0] ?? "");
  check(`${name}: renders content`, await evaluate("document.querySelector('.card, .table, .empty, .error-state') !== null"));
  for (const needle of expect) {
    check(`${name}: shows ${JSON.stringify(needle)}`, text.toLowerCase().includes(needle.toLowerCase()), text.slice(0, 300));
  }
  if (opts.absent) {
    for (const needle of opts.absent) {
      check(`${name}: does not show ${JSON.stringify(needle)}`, !text.toLowerCase().includes(needle.toLowerCase()));
    }
  }
  await shot(name);
  return text;
}

async function signIn(email, password) {
  await goto("/login");
  await typeInto("#email", email);
  await typeInto("#password", password);
  await click(".auth-form .btn--primary");
  await until("the shell after sign-in", "document.querySelector('.app') !== null", 15_000);
}

async function signOut() {
  await goto("/");
  await click(".sidebar-foot .who");
  await click(".menu-item--danger");
  await until("the sign-in screen", "document.querySelector('.auth-panel') !== null", 10_000);
}

try {
  // ---------------------------------------------------------------- customer --
  await signIn(customerEmail, customerPassword);

  const list = await screen("tickets-customer", "/tickets", ["Support tickets", "Server unreachable since 09:10", "New ticket"]);
  check("tickets-customer: has a status filter", /status/i.test(await evaluate("document.body.innerText")));
  check("tickets-customer: does not show a staff column", !(await evaluate("document.body.innerText")).includes("Assignee"));
  check("tickets-customer: the row links to the thread", Boolean(await evaluate(`document.querySelector('a.row-title[href="/tickets/${ticketID}"]') !== null`)));

  // The status filter narrows the list without remounting the page.
  await evaluate(`(() => { const s = document.querySelector('#ticket-status'); s.value = 'closed'; s.dispatchEvent(new Event('change')); })()`);
  await until("the filtered list", "!document.querySelector('.table-loading')");
  const closed = await evaluate("document.body.innerText");
  check("tickets-customer: the closed filter is empty with a next step", /No closed tickets/.test(closed), closed.slice(0, 200));
  check("tickets-customer: the empty state offers the way out", /Show every ticket/.test(closed));

  const thread = await screen("ticket-customer", `/tickets/${ticketID}`, [
    "Server unreachable since 09:10",
    "No route to host",
    "hypervisor dropped the vNIC",
    "ping-trace.txt",
    "Reply",
  ]);
  const attachHref = await evaluate(`document.querySelector('.ticket-attachment')?.getAttribute('href') ?? ''`);
  check("ticket-customer: the attachment is an API path", attachHref.startsWith("/api/v1/portal/tickets/attachments/"), attachHref);
  check("ticket-customer: the attachment is a download", (await evaluate(`document.querySelector('.ticket-attachment')?.hasAttribute('download') ?? false`)) === true);
  check("ticket-customer: no blob URL", !attachHref.startsWith("blob:"));
  check("ticket-customer: does not show staff controls", !thread.includes("Queue controls"));
  // A reply reopens a closed ticket and marks an open one replied, so the only stable claim is
  // that the thread is in a live state — the seed's own reply guarantees it is not "closed".
  check("ticket-customer: the thread is live", /Open|Replied/i.test(thread) && !/Closed/i.test(thread), thread.slice(0, 200));

  // A reply really lands: type into the box and send.
  await typeInto("#ticket-reply", "Confirmed — it answers again. Thanks.");
  await click(".card form button[type=submit].btn--primary");
  await until("the reply to appear", "document.body.innerText.includes('Confirmed')", 12_000);
  check("ticket-customer: the reply appears in the thread", (await evaluate("document.body.innerText")).includes("Confirmed — it answers again"));

  await screen("ticket-new", "/tickets/new", ["Open a ticket", "Subject", "Department", "Priority", "Attachments", "10 MB"]);
  check("ticket-new: the department list came from the server", /Technical|Billing|General/i.test(await evaluate("document.body.innerText")));
  // The file picker needs a raw-body POST the client does not have, so the honest state is an
  // explanation rather than a picker that would drop the file.
  const picker = await evaluate("document.querySelector('#ticket-file-input') !== null");
  const newText = await evaluate("document.body.innerText");
  check(
    "ticket-new: uploads are either usable or explained, never silently dropped",
    picker === true || /File uploads are not available in this build/.test(newText),
    newText.slice(0, 300),
  );
  check("ticket-new: the cap is still stated", /10 MB/.test(newText));

  // A real ticket, created through the form.
  await evaluate(`(() => {
    document.querySelector('#ticket-subject').value = 'Created from the browser test';
    document.querySelector('#ticket-message').value = 'This ticket was created by scripts/p11-e2e.mjs.';
  })()`);
  await click("form button[type=submit].btn--primary");
  await until("the new ticket's thread", "location.pathname.startsWith('/tickets/') && location.pathname !== '/tickets/new'", 15_000);
  await until("the new thread to settle", "!document.querySelector('.skeleton')");
  const created = await evaluate("document.body.innerText");
  check("ticket-new: creating a ticket lands on its thread", /Created from the browser test/.test(created), created.slice(0, 200));

  await screen("portal-security", "/portal/security", ["Security", "Password", "Two-factor", "Sessions", "Login history"]);
  const security = await evaluate("document.body.innerText");
  // The session endpoints may or may not exist on the installation this runs against. Both are
  // correct answers, and the screen must be honest about which one it got: a real list with a
  // "This device" badge, or "not available on this installation".
  check(
    "portal-security: the session list is either real or explained",
    /This device/i.test(security) || /not available on this installation/i.test(security),
    security.slice(0, 400),
  );
  check("portal-security: the login history state is honest", /Login history/i.test(security));
  // The password form refuses a mismatch before any request.
  await evaluate(`(() => {
    document.querySelector('#pw-current').value = 'whatever';
    document.querySelector('#pw-new').value = 'a-long-enough-password';
    document.querySelector('#pw-confirm').value = 'different-password';
  })()`);
  await click("#password form button[type=submit]");
  const mismatch = await until("the mismatch message", "document.body.innerText.includes('do not match') ? document.body.innerText : ''", 4000);
  check("portal-security: a password mismatch is refused locally", /do not match/.test(mismatch));

  const inbox = await screen("portal-notifications", "/portal/notifications", ["Notifications", "Inbox", "Invoice INV-000001 paid", "What you are told about"]);
  check("portal-notifications: an unread notice is marked", (await evaluate("document.querySelectorAll('.notif-row--unread').length")) > 0);
  check("portal-notifications: a force event is locked", (await evaluate("document.querySelectorAll('.pref-row--locked').length")) > 0);
  check("portal-notifications: the locked row explains itself", /cannot be turned off/.test(inbox));
  const lockedHasSwitch = await evaluate("document.querySelector('.pref-row--locked input') !== null");
  check("portal-notifications: the locked row has no toggle", lockedHasSwitch === false);

  await click(".notif-row--unread button");
  await until("the notice to be marked read", "document.querySelectorAll('.notif-row--unread').length === 0", 10_000);
  check("portal-notifications: marking one read updates the list", (await evaluate("document.querySelectorAll('.notif-row--unread').length")) === 0);

  // A preference toggle round-trips through the PUT.
  const before = await evaluate("document.querySelector('.pref-row:not(.pref-row--locked) input')?.checked");
  await evaluate(`(() => { const box = document.querySelector('.pref-row:not(.pref-row--locked) input'); box.checked = !box.checked; box.dispatchEvent(new Event('change')); })()`);
  await evaluate(`(() => {
    const form = [...document.querySelectorAll('form')].find((f) => f.querySelector('.pref-list, .pref-row'));
    form.requestSubmit();
  })()`);
  await until("the preferences to save", "document.body.innerText.includes('Preferences saved')", 10_000);
  check("portal-notifications: preferences save", true);
  await goto("/portal/notifications");
  const after = await evaluate("document.querySelector('.pref-row:not(.pref-row--locked) input')?.checked");
  check("portal-notifications: the change persisted", before !== after, `${before} -> ${after}`);

  await screen("portal-payment-methods", "/portal/payment-methods", ["Payment methods", "no card form", "Pay an invoice"]);
  const pm = await evaluate("document.body.innerText");
  check("portal-payment-methods: says nothing is stored", /no card to save and none is kept|No saved payment methods|Nothing is stored/i.test(pm), pm.slice(0, 300));
  check("portal-payment-methods: draws no card number field", (await evaluate("document.querySelector('input[name*=card], input[autocomplete=cc-number]') === null")));
  check("portal-payment-methods: no PAN-shaped input", (await evaluate("document.querySelectorAll('input').length")) < 4);

  // A customer must not be able to reach the staff queue by typing the URL.
  await goto("/support");
  const path = await evaluate("location.pathname");
  check("portal: /support does not reach the staff queue for a customer", path.startsWith("/portal") || /for the support team/.test(await evaluate("document.body.innerText")), path);

  // ---------------------------------------------------------------- staff --
  await signOut();
  await signIn(staffEmail, staffPassword);

  const queue = await screen("support-queue", "/support", ["Support queue", "Server unreachable since 09:10", "Unassigned"]);
  check("support-queue: has the four filters", /Status[\s\S]*Priority[\s\S]*Department[\s\S]*Assignee/.test(queue));
  check("support-queue: rows link to the thread", Boolean(await evaluate(`document.querySelector('a.row-title[href^="/tickets/"]') !== null`)));
  check("support-queue: a customer column exists", (await evaluate("document.body.innerText")).includes("P11 Customer"));

  // The priority filter narrows it, and the empty state names the filter.
  await evaluate(`(() => { const s = document.querySelector('#support-priority'); s.value = 'low'; s.dispatchEvent(new Event('change')); })()`);
  await until("the filtered queue", "!document.querySelector('.table-loading')");
  const filtered = await evaluate("document.body.innerText");
  check("support-queue: a filter that matches nothing says so", /No ticket matches these filters/.test(filtered), filtered.slice(0, 300));
  check("support-queue: the empty state offers Clear filters", /Clear filters/.test(filtered));

  const staffThread = await screen("ticket-staff", `/tickets/${ticketID}`, ["Queue controls", "Assignee", "Conversation", "P11 Customer"]);
  check("ticket-staff: staff see the queue controls", staffThread.includes("Queue controls"));
  check("ticket-staff: staff see the customer", staffThread.includes("P11 Customer"));
  check("ticket-staff: the close action is offered", /Close ticket/.test(staffThread));

  // The staff edit is a PATCH. If the client has no PATCH the screen says so; if it has one the
  // change must land. Either way there is no half-working control.
  const hasPrioritySelect = await evaluate("document.querySelector('#ticket-edit-priority') !== null");
  if (hasPrioritySelect) {
    await evaluate(`(() => { const s = document.querySelector('#ticket-edit-priority'); s.value = 'low'; })()`);
    await click(".card form button[type=submit]");
    await until("the priority change", "document.body.innerText.includes('Ticket updated')", 10_000);
    check("ticket-staff: a staff edit is saved", true);
  } else {
    check(
      "ticket-staff: a missing PATCH is explained, not faked",
      /cannot be changed here yet/.test(staffThread) && /Status, priority and department/.test(staffThread),
      staffThread.slice(0, 400),
    );
  }

  const staffList = await screen("tickets-staff", "/tickets", ["All tickets", "Assignee", "Server unreachable since 09:10"]);
  check("tickets-staff: the list gains staff columns", staffList.includes("Assignee"));

  check("no uncaught exceptions", exceptions.length === 0, exceptions.join(" | "));
  const unexpected = consoleErrors.filter((line) => !/401 \(Unauthorized\)/.test(line) && !/Failed to load resource.*404/.test(line));
  check("no unexpected console errors", unexpected.length === 0, unexpected.join(" | "));
  if (consoleErrors.length > 0) {
    console.log("\nconsole errors seen (including expected 401/404 probes):");
    for (const line of consoleErrors) console.log(`  ${line}`);
  }
} catch (err) {
  failures.push(`the run threw: ${err.message}`);
  console.log(`FAIL the run threw: ${err.message}`);
  try {
    await shot("failure");
  } catch {
    /* best effort */
  }
} finally {
  ws.close();
  child.kill("SIGKILL");
}

console.log(`\n${passes.length} passed, ${failures.length} failed`);
if (failures.length > 0) {
  console.log("\nfailures:");
  for (const line of failures) console.log(`  - ${line}`);
  process.exit(1);
}
