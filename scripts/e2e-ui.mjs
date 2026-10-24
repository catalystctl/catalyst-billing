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
import { createHmac } from "node:crypto";
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

// ------------------------------------------------------------------- TOTP --
//
// Two-factor enrolment cannot be tested with a fake code, and it must not be made testable by
// weakening the flow — the whole point of the factor is that a code is hard to produce. So the
// harness computes a real one from the secret the screen displays, exactly as an authenticator app
// would: RFC 4226 HOTP over HMAC-SHA1 with a 30-second counter, RFC 6238 for the time step, six
// digits.
//
// That the server accepts it is also the strongest available proof that the secret the page showed
// is the credential: a wrong secret produces a wrong code, and the confirm endpoint refuses it.

/** Decodes the base32 an authenticator secret is written in (RFC 4648, no padding required). */
function base32Decode(secret) {
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";
  const clean = String(secret).replace(/[\s=]/g, "").toUpperCase();
  let bits = 0;
  let value = 0;
  const out = [];
  for (const character of clean) {
    const index = alphabet.indexOf(character);
    if (index < 0) throw new Error(`not a base32 secret: ${JSON.stringify(character)}`);
    value = (value << 5) | index;
    bits += 5;
    if (bits >= 8) {
      out.push((value >>> (bits - 8)) & 0xff);
      bits -= 8;
    }
  }
  return Buffer.from(out);
}

/** The six-digit code for a moment in time, defaulting to now. */
function totpNow(secret, { digits = 6, period = 30, algorithm = "sha1", at = Date.now() } = {}) {
  const counter = Math.floor(at / 1000 / period);
  const message = Buffer.alloc(8);
  message.writeUInt32BE(Math.floor(counter / 2 ** 32), 0);
  message.writeUInt32BE(counter % 2 ** 32, 4);
  const digest = createHmac(algorithm, base32Decode(secret)).update(message).digest();
  const offset = digest[digest.length - 1] & 0x0f;
  const binary =
    ((digest[offset] & 0x7f) << 24) |
    ((digest[offset + 1] & 0xff) << 16) |
    ((digest[offset + 2] & 0xff) << 8) |
    (digest[offset + 3] & 0xff);
  return String(binary % 10 ** digits).padStart(digits, "0");
}

/**
 * A code that will still be valid by the time it is typed.
 *
 * A code computed 29.5 seconds into a 30-second step expires before the browser has finished
 * typing it, and the failure looks like "the server rejected a correct code" rather than "the test
 * was unlucky". So the helper waits out the tail of a step instead of racing it.
 */
async function freshTotp(secret) {
  const stepMs = 30_000;
  const remaining = stepMs - (Date.now() % stepMs);
  if (remaining < 5000) await new Promise((resolve) => setTimeout(resolve, remaining + 250));
  return totpNow(secret);
}

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

  // Wait for the screen to finish loading before reading it.
  //
  // A view renders a skeleton first and replaces it when its data arrives, so reading
  // innerText immediately after navigation sees the skeleton — and the assertions then pass
  // or fail on the nav rail's text rather than the page's. That is how this harness missed a
  // dashboard regression for a while: "Overview" matched the rail label, "Needs attention"
  // did not exist yet, and the screen was fine.
  //
  // The condition is deliberately about the *absence of a skeleton* rather than the presence
  // of a particular string: waiting for an expected string would make a genuinely missing
  // element wait until timeout and then report a timeout instead of the missing text.
  await until(
    `${name}: the screen finishes loading`,
    `document.querySelector('.skeleton, [aria-busy="true"]') === null`,
    15_000,
  ).catch(() => {
    // A screen with no skeleton is already settled; a stuck skeleton is reported by the
    // assertions below rather than as a timeout, so the failure names the real problem.
  });

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

// -------------------------------------------------------------- the MFA flow --
//
// One driver, used for both audiences, because the flow is one flow: the operator's settings page
// and the customer's security page render the same component (web/src/lib/mfa.ts) and differ only
// in wording and in the ids of their dialog fields.
//
// What it proves, in order, and why each step is worth an assertion rather than a screenshot:
//
//   1. The operator reaches the screen by *clicking the navigation*, not by typing the URL. That is
//      the difference between "the feature exists" and "the feature is discoverable", and it is the
//      defect this whole task was opened for.
//   2. The screen states the current state as a fact: a badge reading Off, and later On. A badge
//      that is a guess is worse than no badge, so the text is compared to what the server said.
//   3. Enrolment shows a scannable code, the base32 secret, and the setup URI — and the QR is
//      structurally a QR (a square symbol of a legal version, with dark modules), not an empty
//      frame that happens to have the right class name.
//   4. The code is computed from the *displayed* secret with real HOTP/HMAC-SHA1. The server
//      accepting it is what proves the secret on screen is the credential.
//   5. Recovery codes appear once, ten of them, in a dialog that cannot be dismissed until they are
//      acknowledged, and they can be downloaded as a file (a clipboard is not a backup).
//   6. After confirming, the state reads On — and still reads On after a full page reload, which is
//      what distinguishes a real state read from a local flag set by the dialog.
//   7. Turning it off demands BOTH the password and a current code. A wrong password is refused
//      while the code is right; the pair succeeds. This is the property that stops a stolen session
//      from removing the factor, and it is asserted against the real form.

/** Reads the setup secret out of an open enrolment dialog. */
async function readSetupSecret() {
  return evaluate(`(() => {
    const el = document.querySelector('.settings-secret-value');
    return el ? el.textContent.trim() : '';
  })()`);
}

/**
 * Drives enrolment on the page currently open.
 *
 * Returns the secret it used so a caller can assert on it, and the codes it saw.
 */
async function driveEnrolment({
  enrolButtonText = /set up an authenticator app|set up a second factor/i,
  codeFieldId,
  requireQr = true,
  requireDownload = true,
  // Distinct names per audience: both drives open the same dialog, and one file name would leave the
  // second screenshot overwriting the first.
  shotPrefix = "mfa",
}) {
  const opened = await evaluate(`(() => {
    const button = Array.from(document.querySelectorAll('button')).find((b) =>
      ${enrolButtonText}.test((b.textContent || '').trim()));
    if (!button) return null;
    button.click();
    return (button.textContent || '').trim();
  })()`);
  check("mfa: the enrol button exists and is pressable", Boolean(opened), "no enrol button found");
  if (!opened) return null;

  await until("the enrolment dialog", "document.querySelector('.settings-secret-value') !== null", 10_000);
  const secret = await readSetupSecret();
  check("mfa: the setup secret is a base32 secret", /^[A-Z2-7]{16,}$/.test(secret), secret ? `got ${secret.length} chars` : "no secret");

  // The QR is generated in the page (no third-party image service: the CSP forbids one, and a TOTP
  // secret must not leave the origin). It has to be a real symbol: a square whose side is
  // 4·version+17, drawn from a non-empty path.
  const qr = await evaluate(`(() => {
    const svg = document.querySelector('.settings-qr');
    if (!svg) return { present: false };
    const box = (svg.getAttribute('viewBox') || '').split(/\\s+/).map(Number);
    const path = svg.querySelector('path');
    const d = path ? (path.getAttribute('d') || '') : '';
    return {
      present: true,
      width: box[2],
      height: box[3],
      pathLength: d.length,
      modules: (d.match(/M/g) || []).length,
      darkFill: path ? getComputedStyle(path).fill : '',
    };
  })()`);
  if (requireQr) {
    check("mfa: the enrolment dialog draws a QR code", qr.present === true, JSON.stringify(qr));
  } else if (!qr.present) {
    // The customer's page currently offers the secret and the setup URI but no symbol. That is not
    // a CSP constraint — the encoder in web/src/lib/qr.ts runs in the page and fetches nothing — it
    // is a gap on that screen, so it is reported rather than asserted away.
    console.log("     note: mfa: this page shows the setup secret but no QR code");
  }
  if (qr.present) {
    // Quiet zone of four modules on each side is part of the drawing, so the symbol side is
    // width - 8 and must be a legal version: 21, 25, 29 … 57 for the versions the encoder covers.
    const side = (qr.width ?? 0) - 8;
    check("mfa: the QR is a legal symbol size", side >= 21 && side <= 57 && (side - 17) % 4 === 0, `side=${side} (viewBox ${qr.width})`);
    check("mfa: the QR is square", qr.width === qr.height, `${qr.width}x${qr.height}`);
    check("mfa: the QR has dark modules", (qr.modules ?? 0) > 10, `paths=${qr.modules}`);
  }

  const hasCopy = await evaluate("document.querySelector('.settings-secret .copy') !== null");
  check("mfa: the setup secret can be copied", hasCopy);
  await screenshot(`${shotPrefix}-enrolment-dialog`);

  // A real code, from the secret the page showed.
  const code = await freshTotp(secret);
  await typeInto(`#${codeFieldId}`, code);
  const confirmed = await evaluate(`(() => {
    const button = Array.from(document.querySelectorAll('.dialog .btn--primary')).find((b) => /confirm/i.test(b.textContent || ''));
    if (!button) return false;
    button.click();
    return true;
  })()`);
  check("mfa: the confirm button is pressable", confirmed, "no Confirm button in the dialog footer");

  // The recovery codes, shown once.
  const shown = await until("the recovery-code dialog", "document.querySelector('.settings-recovery') !== null", 12_000).catch(() => false);
  check("mfa: confirming shows the recovery codes", Boolean(shown), "no recovery-code dialog appeared");
  if (!shown) return { secret, codes: [] };

  await screenshot(`${shotPrefix}-recovery-codes`);
  const codes = await evaluate(`(() => {
    const pre = document.querySelector('.settings-recovery');
    return pre ? pre.textContent.split(/\\s+/).filter(Boolean) : [];
  })()`);
  check("mfa: ten recovery codes are shown", codes.length === 10, `got ${codes.length}`);
  check("mfa: the codes are not the TOTP secret", !codes.includes(secret));

  // Downloadable, because a clipboard is not a backup. The href is a blob the page built itself,
  // so this is also a check that nothing was uploaded to render the codes.
  const download = await evaluate(`(() => {
    const a = Array.from(document.querySelectorAll('.dialog a[download]'))[0];
    if (!a) return { present: false };
    return { present: true, href: a.getAttribute('href') || '', name: a.getAttribute('download') || '', text: a.textContent.trim() };
  })()`);
  if (requireDownload) {
    check("mfa: the codes can be downloaded", download.present === true, JSON.stringify(download));
  } else if (!download.present) {
    // Reported, not asserted away: this page offers the codes for copying only, and a clipboard is
    // not a backup. The helper that fixes it is exported by web/src/lib/mfa.ts
    // (`recoveryCodeDownload`), so adopting it on that screen is one import.
    console.log("     note: mfa: this page offers the recovery codes for copying only, with no download");
  }
  if (download.present) {
    check("mfa: the download is a local blob", download.href.startsWith("blob:"), download.href);
    check("mfa: the download has a filename", /recovery/i.test(download.name), download.name);
  }

  // The dialog is locked until acknowledged: closing it is how codes are lost.
  const dismissible = await evaluate(`(() => {
    const done = Array.from(document.querySelectorAll('.dialog .btn--primary')).find((b) => /saved/i.test(b.textContent || ''));
    return done ? done.disabled : null;
  })()`);
  check("mfa: the recovery dialog cannot be dismissed unacknowledged", dismissible === true, `done.disabled=${dismissible}`);

  await click(".dialog .checkbox input");
  await evaluate(`(() => {
    const done = Array.from(document.querySelectorAll('.dialog .btn--primary')).find((b) => /saved/i.test(b.textContent || ''));
    if (done) done.click();
    return true;
  })()`);
  await until("the recovery dialog to close", "document.querySelector('.settings-recovery') === null", 8000).catch(() => {});

  return { secret, codes };
}

/** The badge text of the two-factor section, as rendered. */
async function mfaBadge() {
  return evaluate(`(() => {
    const row = document.querySelector('.settings-status-row');
    if (!row) return '';
    const badge = row.querySelector('.badge');
    return badge ? badge.textContent.trim() : '';
  })()`);
}

/** Turns the factor off through the dialog, proving both factors are demanded. */
async function driveDisable({ password, secret, passwordFieldId, codeFieldId, wrongPasswordFirst = true }) {
  const opened = await evaluate(`(() => {
    const button = Array.from(document.querySelectorAll('button')).find((b) => /turn off two-factor/i.test(b.textContent || ''));
    if (!button) return false;
    button.click();
    return true;
  })()`);
  check("mfa: the disable button exists and is pressable", opened, "no Turn off two-factor button");
  if (!opened) return false;

  await until("the disable dialog", `document.querySelector('#${passwordFieldId}') !== null`, 8000);

  // A stolen session alone must not be able to remove the factor: the right code with a wrong
  // password has to be refused, and the dialog has to stay open saying so.
  if (wrongPasswordFirst) {
    await typeInto(`#${passwordFieldId}`, "definitely not the password");
    await typeInto(`#${codeFieldId}`, await freshTotp(secret));
    await evaluate(`(() => {
      const b = Array.from(document.querySelectorAll('.dialog .btn--danger-solid, .dialog .btn--primary')).find((x) => /turn off/i.test(x.textContent || ''));
      if (b) b.click();
      return true;
    })()`);
    const refused = await until("the refusal for a wrong password", "document.querySelector('.dialog .banner--danger') !== null", 10_000).catch(() => false);
    check("mfa: a wrong password is refused even with a valid code", Boolean(refused), "the dialog did not report a refusal");
    const stillOpen = await evaluate(`document.querySelector('#${passwordFieldId}') !== null`);
    check("mfa: the disable dialog stays open after a refusal", stillOpen);
    await evaluate(`(() => { const el = document.querySelector('#${passwordFieldId}'); if (el) el.value = ''; const c = document.querySelector('#${codeFieldId}'); if (c) c.value = ''; return true; })()`);
  }

  await typeInto(`#${passwordFieldId}`, password);
  await typeInto(`#${codeFieldId}`, await freshTotp(secret));
  await evaluate(`(() => {
    const b = Array.from(document.querySelectorAll('.dialog .btn--danger-solid, .dialog .btn--primary')).find((x) => /turn off/i.test(x.textContent || ''));
    if (b) b.click();
    return true;
  })()`);

  const closed = await until("the disable dialog to close", `document.querySelector('#${passwordFieldId}') === null`, 12_000).catch(() => false);
  check("mfa: password plus a current code turns the factor off", Boolean(closed), "the dialog did not close");
  return Boolean(closed);
}

// ------------------------------------------------- the new-screen ledger --

// The commerce, support and account screens are built by parallel packages and land in
// stages, so the harness must be able to tell "this build does not have that screen" from
// "that screen is broken". The distinction is made from what the page actually renders,
// not from a list the harness keeps: a screen that is not in the build says so in its
// *title*, and a screen that is in the build and broken says something else.
//
// The markers are matched against `.empty-title` and `.card-title` only, never against the
// whole body text. That is deliberate and was a real bug: matching body text made
// `/tickets/new` look absent because it renders an informational banner reading "File
// uploads are not available in this build" while the form itself is perfectly present. A
// banner's title is a bare `<strong>`, so restricting the match to title elements
// distinguishes "this screen is a placeholder" from "this screen has a note about a
// sub-feature".
const MISSING_MARKERS = [
  // The route table has no entry for the path (app.ts renderNotFound).
  /that page does not exist/i,
  // The route is declared but its chunk is not in the bundle (app.ts, unresolved import),
  // or the view module is still the placeholder that says so.
  /not available in this build/i,
  // A real screen whose endpoint this build does not serve, e.g. config options or logs.
  /not available on this installation/i,
  /has not shipped yet/i,
  // A declared route whose view is a placeholder card. Without this the screen counts as
  // `present` and the ledger says "0 gaps" while the operator sees an empty card — which is
  // how /ticket-fields stayed invisible for as long as it did.
  /not built yet/i,
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
      search: location.search,
      title: document.title,
      h1: q("h1") ? q("h1").innerText : null,
      hasCard: !!q(".card"),
      hasTable: !!q(".table"),
      hasEmpty: !!q(".empty"),
      hasEmptyState: !!q(".empty-state"),
      errorText: q(".error-state") ? q(".error-state").innerText : null,
      emptyTitle: q(".empty .empty-title, .empty-state .empty-title") ? q(".empty .empty-title, .empty-state .empty-title").innerText : "",
      // The titles only. See MISSING_MARKERS for why body text is not used.
      titles: [...document.querySelectorAll(".empty-title, .card-title")].map((el) => el.innerText).join(" \\u00b7 "),
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
    const hit = state.titles.match(marker);
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
  //
  // The comparison is on the *pathname*: a screen is free to rewrite its own query string
  // (config options writes `product_id` into the URL so the choice survives a refresh), and
  // comparing the whole path+query reported that as a redirect.
  const wantedPath = path.split("?")[0];
  const redirected = state.path !== wantedPath;
  const reason = redirected ? "" : missingReason(state);

  if (reason) {
    screenLedger.push({ name, path, status: "missing", reason });
    console.log(`skip ${name} (${path}) — not in this build: ${reason}`);
    return { status: "missing", state };
  }

  const before = failures.length;
  if (redirected) {
    check(`${name}: stays on ${wantedPath}`, false, `redirected to ${state.path}${state.search}`);
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
  if (state.path !== path.split("?")[0] || missingReason(state)) return;
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

  // Self-registration. Two separate questions, and they are asked separately because the
  // answers are different:
  //
  //   1. Can a *stranger* reach it? That is a property of the route table (`PUBLIC_PATHS` in
  //      web/src/routes.ts), not of the screen, and it is the whole point of a registration
  //      page: if the shell bounces an anonymous visitor to /login, the screen is unreachable
  //      by the only audience it exists for, and every "registration works" claim is false
  //      for a real visitor. That is recorded as a *gap* — visible in the ledger, not
  //      silently skipped and not a failure of this harness, because the fix is a one-line
  //      change in a file this package does not own.
  //   2. Does the form work? That is a property of the screen, and it is driven below.
  //
  // The endpoint never says whether an address already existed (that would be an account
  // discovery oracle), so there is no "signed-in landing" to assert. Success is one of two
  // states: "check your email" when verification is required, or "check your email, then
  // sign in" when it is not. Both are the accepted state and both are what this asserts.
  {
    const state = await visit("/register");
    const reachable = state.path === "/register" && !missingReason(state);
    if (reachable) {
      screenLedger.push({ name: "register-anonymous", path: "/register", status: "present", reason: "" });
    } else {
      const why = missingReason(state) || `the shell sent an anonymous visitor to ${state.path}`;
      screenLedger.push({ name: "register-anonymous", path: "/register", status: "gap", reason: why });
      console.log(`gap  register-anonymous     /register — ${why}`);
      console.log(
        "     note: /register is not in PUBLIC_PATHS (web/src/routes.ts), so a visitor with no " +
          "session cannot reach it. The form itself is driven below, signed in, so a broken " +
          "form and an unreachable screen are reported as the two different problems they are.",
      );
    }
  }

  // Sign in through the form, which is what proves the login screen works rather than just
  // the API behind it.
  await goto("/login");

  // The sign-in page must offer the way to sign up. This is the defect the register screen was
  // hiding behind: /register worked and was public, and nothing pointed at it, so self-registration
  // was reachable only by someone who already knew the URL. Asserted on the anonymous login screen
  // because that is where a visitor lands — and it is invisible to anyone who already has an
  // account, which is why it survived so long.
  {
    const signup = await evaluate(`(() => {
      const a = Array.from(document.querySelectorAll('a')).find((x) => /create an account/i.test(x.textContent || ''));
      return a ? a.getAttribute('href') : null;
    })()`);
    check("login: offers a link to create an account", signup === "/register", `href=${signup}`);
  }

  await typeInto("#email", staffEmail);
  await typeInto("#password", staffPassword);
  await click('.auth-form .btn--primary');
  await until("the shell after sign-in", "document.querySelector('.app') !== null", 12_000);

  const staffHome = await assertScreen("dashboard", {
    expect: ["Overview", "Needs attention"],
  });
  check("dashboard: renders the exception list or says it is clear", /Needs attention|Nothing is on fire/.test(staffHome));

  // The storefront, as a signed-in operator.
  //
  // This is the regression the whole `/` vs `/dashboard` split exists for: while `/` was "the
  // dashboard for staff and the storefront for a visitor", an administrator had no URL at all
  // for their own shop, so "is the price on the site right?" could not be answered without
  // signing out. Four assertions, because there are four ways to get it wrong again: the shop
  // must render at `/`, it must render *in place* rather than redirecting, it must not be the
  // panel, and it must carry a way back into the panel.
  {
    await goto("/");
    const store = await evaluate(`(() => ({
      path: location.pathname,
      storefront: document.querySelector('.store-page') !== null,
      shell: document.querySelector('.app') !== null,
      back: [...document.querySelectorAll('.store-nav a, .store-hero-actions a')]
        .map((a) => ({ href: a.getAttribute('href'), label: a.innerText.trim() }))
        .filter((a) => a.href === '/dashboard'),
      heading: document.querySelector('h1')?.innerText ?? '',
    }))()`);
    check("storefront: an operator sees the shop at /", store.storefront, JSON.stringify(store));
    check("storefront: / is not redirected for an operator", store.path === "/", store.path);
    check("storefront: the panel shell is not wrapped around the shop", !store.shell);
    check("storefront: the shop renders its own heading", store.heading.length > 0, store.heading);
    check("storefront: it offers a link back to the panel", store.back.length > 0, JSON.stringify(store.back));
    if (store.back.length > 0) {
      await click('.store-nav a[href="/dashboard"], .store-hero-actions a[href="/dashboard"]');
      const backIn = await until("the panel after clicking the storefront's dashboard link", "location.pathname === '/dashboard' && document.querySelector('.app') !== null", 12_000).catch(() => false);
      check("storefront: that link returns to the panel", Boolean(backIn));
    }
  }

  // The management writes. These are the ones an operator actually needs and the panel spent a
  // long time without: a button that opens a form, and a form with real fields in it. A list
  // screen with no create action is the defect this asserts against.
  //
  // Each dialog is dismissed with Escape rather than by hunting for its Cancel button: the dialog
  // traps focus and closes on Escape by design, so that is the one dismissal that works for every
  // dialog on the panel. Leaving one open would hide the next screen's content behind the overlay
  // and fail an assertion three steps later, which is exactly what happened the first time.
  for (const surface of [
    { name: "customers", path: "/customers", action: /new customer/i },
    { name: "services", path: "/services", action: /new service/i },
  ]) {
    const state = await visit(surface.path);
    if (missingReason(state)) {
      console.log(`gap  ${surface.name} create — ${missingReason(state)}`);
      continue;
    }
    const opened = await evaluate(`(() => {
      const b = Array.from(document.querySelectorAll('button')).find((x) => ${surface.action}.test(x.textContent || ''));
      if (!b) return false;
      b.click();
      return true;
    })()`);
    check(`${surface.name}: offers a create action`, opened, "no create button found");
    if (opened) {
      await until(`${surface.name}'s create form`, "document.querySelector('[role=dialog] input') !== null", 5_000).catch(() => {});
      const fields = await evaluate("document.querySelectorAll('[role=dialog] input, [role=dialog] select, [role=dialog] textarea').length");
      check(`${surface.name}: the create form has fields`, fields > 0, `fields=${fields}`);
      // Close without submitting: this run must not create records the later assertions do not
      // expect, and a half-submitted create would corrupt the seed.
      await evaluate("document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))");
      await until(`${surface.name}'s dialog to close`, "document.querySelector('[role=dialog]') === null", 3_000).catch(() => {});
      const stillOpen = await evaluate("document.querySelector('[role=dialog]') !== null");
      check(`${surface.name}: the create dialog closes on Escape`, !stillOpen, "a dialog stayed open over the page");
    }
  }


  // Navigation by clicking the rail, not by URL: the rail is what a user uses.
  await click('.nav-item[data-href="/customers"]');
  await until("the customer list", "location.pathname === '/customers' && document.querySelector('table, .empty') !== null");
  await assertScreen("customers", { expect: ["Customers"] });

  const customerEmail = arg("expect-customer");
  if (customerEmail) {
    const text = await pageText();
    check("customers: lists the seeded customer", text.includes(customerEmail));
    // The seeded customer, opened by its id when the harness was given one. Clicking the first
    // row of the list looks equivalent and is not: the list is newest-first, so any other fixture
    // in the database — another package's test run, a colleague's seed — becomes "the customer"
    // and every assertion below is then made about a record this run did not create. That is how
    // a green harness reports on somebody else's data.
    if (serviceCustomerId) {
      await goto(`/customers/${serviceCustomerId}`);
    } else {
      await click(`a[href^="/customers/"]`);
    }
    await until("the customer detail", "location.pathname.startsWith('/customers/') && document.querySelector('.page') !== null");
    await until("the customer detail data", "document.querySelector('.page-skeleton') === null", 10_000);
    // Remembered so the impersonation walk below can come back here: the record is where an
    // operator decides to act as the customer, so that is the button under test.
    const customerRecordPath = await evaluate("location.pathname");
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

    // ------------------------------------------------------------ impersonation --
    //
    // The capability was API-only: audited, tested, and unreachable without a curl command,
    // which in practice means nobody used it during a support call. This walks the path a
    // person walks — the button on the record, the confirmation, the banner, the stop — and it
    // asserts the two facts that make the session safe rather than merely convenient: the
    // banner follows the operator onto every screen, and the *server* refuses money while it is
    // up. Hiding the Pay button is not the restriction under test here.
    await goto(customerRecordPath);
    const offered = await until(
      "the offer to sign in as the customer",
      `[...document.querySelectorAll('.page-actions .btn')].some((b) => /Sign in as this customer/i.test(b.textContent || ''))`,
      12_000,
    ).then(() => true).catch(() => false);
    check("customer record: offers to sign in as the customer", offered, "no such button in the page actions");

    if (offered) {
      await evaluate(`(() => {
        const b = [...document.querySelectorAll('.page-actions .btn')]
          .find((x) => /Sign in as this customer/i.test(x.textContent || ''));
        b.click();
        return true;
      })()`);
      await until("the confirmation dialog", "document.querySelector('[role=dialog]') !== null", 5_000);
      const dialogText = await evaluate("document.querySelector('[role=dialog]').innerText");
      // The limits are stated before the session exists, not discovered by hitting a 403 in the
      // middle of a support call.
      check("impersonation: the dialog states the money limit", /money/i.test(dialogText), dialogText.slice(0, 160));
      check("impersonation: the dialog states the audit", /record|audit/i.test(dialogText), dialogText.slice(0, 160));

      await evaluate(`(() => {
        const b = [...document.querySelectorAll('[role=dialog] button')]
          .find((x) => /Sign in as them/i.test(x.textContent || ''));
        if (!b) return false;
        b.click();
        return true;
      })()`);

      // The borrowed session is a customer session, so the app lands on their portal.
      const landed = await until("the impersonated portal", "location.pathname === '/portal'", 12_000)
        .then(() => true)
        .catch(() => false);
      check("impersonation: the session lands in the customer's portal", landed, `path: ${await evaluate("location.pathname")}`);

      const banner = await until(
        "the impersonation banner",
        "document.querySelector('#impersonation-banner') !== null",
        12_000,
      ).then(() => true).catch(() => false);
      const bannerText = banner ? await evaluate("document.querySelector('#impersonation-banner').innerText") : "";
      check("impersonation: a banner is up", banner);
      check("impersonation: the banner names the account", bannerText.includes(customerEmail), bannerText);
      check("impersonation: the banner states the limits", /refused/i.test(bannerText), bannerText);
      check("impersonation: the banner offers the way out", /Stop impersonating/i.test(bannerText), bannerText);

      // Every screen, not just the landing one.
      await goto("/portal/services");
      const stillThere = await evaluate("document.querySelector('#impersonation-banner') !== null");
      check("impersonation: the banner survives navigation", stillThere === true);

      // The storefront renders outside the shell, so it is the one screen where the banner has
      // to be mounted by hand. Missing there, it is the screen an operator could shop from while
      // wearing a customer's identity.
      await visit("/products");
      const storefrontBanner = await evaluate("document.querySelector('#impersonation-banner') !== null");
      check("impersonation: the banner is on the storefront too", storefrontBanner === true);
      await goto("/portal");

      // The server refuses, and this asks it directly with the impersonated cookie. A refusal
      // from the UI would prove only that the UI agreed with itself. The CSRF header and the
      // idempotency key travel with it on purpose: without them a 403 would be `csrf_failed` or
      // `idempotency_key_required`, and the test would pass on the wrong refusal.
      const refusal = await evaluate(`(async () => {
        const csrf = (document.cookie.match(/(?:^|;\\s*)billing_csrf=([^;]+)/) || [])[1] || '';
        const r = await fetch('/api/v1/portal/credits/deposit', {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
            'X-CSRF-Token': decodeURIComponent(csrf),
            'Idempotency-Key': crypto.randomUUID(),
          },
          credentials: 'same-origin',
          body: JSON.stringify({ amount_minor: 1000, currency: 'USD' }),
        });
        let code = '';
        try { code = (await r.json()).code || ''; } catch (e) { code = 'unreadable'; }
        return { status: r.status, code };
      })()`);
      check(
        "impersonation: the server refuses money",
        refusal.status === 403 && refusal.code === "impersonating",
        JSON.stringify(refusal),
      );

      // And the way out, from the banner rather than from a URL somebody has to remember.
      await evaluate(`(() => {
        const b = [...document.querySelectorAll('#impersonation-banner button')]
          .find((x) => /Stop impersonating/i.test(x.textContent || ''));
        if (!b) return false;
        b.click();
        return true;
      })()`);
      // The staff home is /dashboard, not /. `/` is the storefront — a staff session that landed
      // there would be a bug, not a homecoming, and asserting `/` would have passed only by
      // matching a page the operator should never be sent to. `homeFor()` in lib/session.ts is
      // the single definition, and the storefront-link assertion above already relies on it.
      const backHome = await until("the operator's own session", "location.pathname === '/dashboard'", 12_000)
        .then(() => true)
        .catch(() => false);
      check("impersonation: stopping returns the operator to their own session", backHome, `path: ${await evaluate("location.pathname")}`);
      const bannerGone = await evaluate("document.querySelector('#impersonation-banner') === null");
      check("impersonation: the banner goes when the impersonation ends", bannerGone === true);
      // The rail is staff navigation again: the session really is the operator's.
      const staffRail = await until("the staff rail", "document.querySelector('.nav-item[data-href=\"/customers\"]') !== null", 8_000)
        .then(() => true)
        .catch(() => false);
      check("impersonation: the operator's own navigation is back", staffRail);
    }
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

  // ------------------------------------------------------- the second factor --
  //
  // The operator's half of the MFA work. The screen existed before this test and nobody could find
  // it: enrolment lived at the bottom of a settings page reachable only by knowing the URL, and the
  // page said it could not tell you whether a factor was already on. So the first assertion is
  // about *reaching* it, and the rest is about the flow actually working.
  {
    // 1. The shell says the account has no second factor, on the first screen the operator lands
    //    on. This is the "see at a glance" requirement, and it is a prompt rather than a lock-out
    //    because an operator on call must not be shut out of the panel by a warning.
    await goto("/dashboard");
    const prompt = await evaluate(`(() => {
      const el = document.querySelector('.mfa-prompt');
      return el ? { text: el.innerText, href: el.querySelector('a')?.getAttribute('href') ?? '' } : null;
    })()`);
    check("mfa-prompt: a privileged account with no factor is prompted", prompt !== null, "no .mfa-prompt on the dashboard");
    if (prompt) {
      check("mfa-prompt: it says why it matters", /second factor/i.test(prompt.text), prompt.text.slice(0, 160));
      check("mfa-prompt: it links to the screen that fixes it", prompt.href === "/settings", prompt.href);
      // And the link is a real path: clicking it lands on the settings page with the section.
      await click(".mfa-prompt a");
      await until("the settings screen from the prompt", "location.pathname === '/settings'", 8000);
      check("mfa-prompt: the link reaches the settings screen", (await evaluate("location.pathname")) === "/settings");
    }

    // 2. Reachable by clicking the navigation, which is the acceptance criterion: an operator finds
    //    it without knowing the URL.
    await goto("/dashboard");
    await click('.nav-item[data-href="/settings"]');
    await until("the settings screen from the nav", "location.pathname === '/settings'", 8000);
    check("mfa: the account settings screen is in the navigation", (await evaluate("location.pathname")) === "/settings");

    // 3. The state is stated as a fact before anything is done: Off, because this account is new.
    await until("the two-factor status", "document.querySelector('.settings-status-row') !== null", 10_000);
    check("mfa: the screen reports the current state", (await mfaBadge()) === "Off", await mfaBadge());
    // The user menu answers the same question from anywhere in the panel.
    const menuState = await evaluate(`(() => {
      const item = document.querySelector('.menu-item--mfa');
      return item ? item.textContent.replace(/\s+/g, ' ').trim() : '';
    })()`);
    check("mfa: the account menu says the factor is off", /two-factor\s*·?\s*off/i.test(menuState), menuState);

    // 4. Enrol, with a real code computed from the secret the screen showed.
    const enrolled = await driveEnrolment({ codeFieldId: "mfa-code" });
    if (enrolled?.secret) {
      check("mfa: the setup secret is the standard 32 base32 characters", enrolled.secret.length === 32, `${enrolled.secret.length} chars`);

      // 5. The state now reads On — and still does after a reload, which is what proves the page
      //    reads the server rather than a flag the dialog set.
      await until("the status to say On", `(() => { const b = document.querySelector('.settings-status-row .badge'); return b && b.textContent.trim() === 'On'; })()`, 10_000).catch(() => {});
      check("mfa: the state reads On after confirming", (await mfaBadge()) === "On", await mfaBadge());
      check("mfa: the prompt is gone once the factor is on", (await evaluate("document.querySelector('.mfa-prompt') === null")));

      await goto("/settings");
      await until("the status after a reload", "document.querySelector('.settings-status-row .badge') !== null", 10_000);
      check("mfa: the state survives a reload (it is read, not remembered)", (await mfaBadge()) === "On", await mfaBadge());
      const detail = await evaluate("document.querySelector('.settings-status-detail')?.innerText ?? ''");
      check("mfa: the screen says when the factor was added", /on since/i.test(detail), detail.slice(0, 160));
      check("mfa: the screen counts the recovery codes left", /recovery code/i.test(detail), detail.slice(0, 200));

      // 6. The factor actually gates the sign-in. Without this step the harness would prove that
      //    enrolment *records* a factor, not that the factor is enforced — and an enrolment that
      //    does not challenge a login is the one failure mode a "state reads On" assertion cannot
      //    see. So: sign out, sign in with the password, expect the challenge, and complete it with
      //    a code computed from the secret.
      await goto("/settings");
      await evaluate(`(() => {
        const who = document.querySelector('.who');
        if (who) who.click();
        const out = document.querySelector('.menu-item--danger');
        if (out) out.click();
        return true;
      })()`);
      await until("the sign-in screen after signing out", "document.querySelector('.auth-panel') !== null", 12_000);
      await typeInto("#email", staffEmail);
      await typeInto("#password", staffPassword);
      await click(".auth-form .btn--primary");
      const challenged = await until("the second-factor challenge", "document.querySelectorAll('.otp-digit').length === 6", 12_000).catch(() => false);
      check("mfa: signing in now demands a code", Boolean(challenged), "no six-digit challenge appeared");
      if (challenged) {
        const code = await freshTotp(enrolled.secret);
        const filled = await evaluate(`(() => {
          const digits = Array.from(document.querySelectorAll('.otp-digit'));
          const code = ${JSON.stringify(code)};
          digits.forEach((el, i) => {
            el.value = code[i] ?? '';
            el.dispatchEvent(new Event('input', { bubbles: true }));
          });
          return digits.map((d) => d.value).join('');
        })()`);
        check("mfa: the challenge takes six digits", filled.length === 6, filled);
        await click(".auth-form .btn--primary");
        const back = await until("the panel after the second factor", "document.querySelector('.app') !== null", 15_000).catch(() => false);
        check("mfa: a current code completes the sign-in", Boolean(back), "the panel did not load after the code");
        // A wrong code must be refused and must not sign the user out of the challenge — that is the
        // property a customer hits every time their phone's clock drifts.
        if (back) {
          const refused = await evaluate("document.querySelector('.error-state, .banner--danger, .auth-error') !== null");
          check("mfa: a completed challenge leaves no error on the panel", !refused);
        }
      }

      // 7. Turning it off needs both factors. The helper tries the right code with a wrong password
      //    first, then the pair.
      await goto("/settings");
      await until("the two-factor section after re-signing in", "document.querySelector('.settings-status-row') !== null", 12_000);
      check("mfa: the state is still On after signing back in", (await mfaBadge()) === "On", await mfaBadge());
      await driveDisable({
        password: staffPassword,
        secret: enrolled.secret,
        passwordFieldId: "mfa-disable-password",
        codeFieldId: "mfa-disable-code",
      });
      await until("the status to say Off", `(() => { const b = document.querySelector('.settings-status-row .badge'); return b && b.textContent.trim() === 'Off'; })()`, 10_000).catch(() => {});
      check("mfa: the state reads Off after disabling", (await mfaBadge()) === "Off", await mfaBadge());
      check("mfa: the prompt returns when the factor is removed", (await evaluate("document.querySelector('.mfa-prompt') !== null")));
    }
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

  // The registration form itself, driven signed in.
  //
  // It belongs with the anonymous check above conceptually, but the shell bounces a stranger
  // to /login before the form renders (the gap recorded above), so the only way to exercise
  // the form today is with a session. That is not the audience the screen is for, and the gap
  // is reported as the defect it is — but a form that does not submit is a *second*,
  // independent defect, and this is the only place that can see it. When /register joins
  // PUBLIC_PATHS this block keeps working unchanged; it just stops being the only path to it.
  {
    const state = await visit("/register");
    if (state.path === "/register" && !missingReason(state)) {
      const before = failures.length;
      check("register: no error state", !state.errorText, state.errorText ? state.errorText.slice(0, 300) : "");
      check("register: has a heading", Boolean(state.h1), "no <h1>");
      const fields = await evaluate("document.querySelectorAll('.auth-form input').length");
      check("register: renders its form", fields > 0, `inputs=${fields}`);
      await typeInto("#name", "E2E Registration");
      await typeInto("#email", `e2e-register-${Date.now()}@example.test`);
      await typeInto("#password", "e2e-registration-password-1");
      // The terms box is a real requirement on installations that configure terms, so it is
      // ticked when it exists rather than assumed to be optional.
      if (await evaluate("document.querySelector('#tos') !== null")) await click("#tos");
      await click('.auth-form .btn--primary');
      const accepted = await until(
        "the accepted state after registering",
        "/check your email/i.test(document.body.innerText)",
        10_000,
      ).catch(() => false);
      const after = await pageText();
      check("register: submitting shows the accepted state", Boolean(accepted), after.slice(0, 400));
      check("register: says to check the email", /check your email/i.test(after), after.slice(0, 400));
      // The endpoint is deliberately non-enumerating, so the page must not claim an account
      // was created. This is the sentence that keeps the privacy promise visible.
      check(
        "register: does not claim the account exists",
        !/your account has been created|account created successfully/i.test(after),
      );
      await screenshot("register-accepted");
      screenLedger.push({
        name: "register-form",
        path: "/register",
        status: failures.length > before ? "broken" : "present",
        reason: failures.length > before ? failures.slice(before).join("; ") : "",
      });
    }
    // Return to the shell: the register screen is a full-page auth layout, and the rest of the
    // staff pass expects the shell to be mounted.
    await goto("/dashboard");
  }

  await assertNewScreen("coupons", {
    path: "/coupons",
    expect: ["Coupons", ...(seededCoupon ? [seededCoupon] : [])],
    requireTable: true,
  });

  // The rate table is the screen; the import card is the surface that lets an installation stop
  // typing rates by hand. Both are asserted because a screen that renders its table and silently
  // loses its configuration card is exactly the regression a screenshot review misses: the page
  // looks right, and there is no longer any way to configure a source.
  await assertNewScreen("tax-rates", {
    path: "/tax-rates",
    expect: ["Tax rates", "External source", ...(seededTaxRate ? [seededTaxRate] : [])],
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

  // What the desk asks for, and what must be answered before a ticket can be closed. The
  // placeholder that used to stand at this route is gone: the screen lists the configured fields
  // (or an empty state that explains what a field is) and offers to add one.
  await assertNewScreen("ticket-fields", {
    path: "/ticket-fields",
    expect: ["Ticket fields", "Required to close"],
    absent: ["Not built yet"],
  });

  // Opening a ticket on a customer's behalf — the phone call the queue could not record before.
  // The form is the customer's form plus the customer picker, and the picker is the thing that
  // proves this screen is the staff one.
  await assertNewScreen("support-new", {
    path: "/support/new",
    expect: ["Open a ticket", "Find the customer"],
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

  // The staff credit screen lists balances and moves credit in both directions. The debit action
  // is the half that a read-only balance screen leaves to SQL, so it is checked by name: a screen
  // that renders a balance and offers no way to correct one is the gap this screen closed.
  const creditsScreen = await assertNewScreen("credits-staff", {
    path: "/credits",
    expect: ["Credit", ...(customerEmail ? [customerEmail] : [])],
  });
  if (creditsScreen.status === "present" && customerEmail) {
    // The seeded customer holds credit, so the row that carries it must offer the deduction. The
    // controls are read from the DOM rather than assumed: a handler with no button is a feature
    // no operator can reach.
    const controls = await evaluate(
      "[...document.querySelectorAll('button')].map((b) => (b.textContent || '').trim())",
    );
    check(
      "credits-staff: offers a debit action",
      Array.isArray(controls) && controls.some((label) => /^debit$/i.test(String(label))),
      JSON.stringify(controls),
    );
    // And the dialog behind it must open with the fields the server requires, or the action is a
    // button that cannot do anything. The button is clicked by its label rather than by position:
    // the page head has actions of its own and "the first button" is not the debit one.
    const clickedDebit = await evaluate(`(() => {
      const el = [...document.querySelectorAll('button')]
        .find((b) => /^debit$/i.test((b.textContent || '').trim()));
      if (!el) return false;
      el.click();
      return true;
    })()`);
    if (clickedDebit) {
      const opened = await until(
        "the debit dialog",
        "document.querySelector('[role=dialog]') !== null",
        3_000,
      )
        .then(() => true)
        .catch(() => false);
      check("credits-staff: the debit action opens a dialog", opened, "no dialog appeared");
      if (opened) {
        const dialog = await pageText();
        check(
          "credits-staff: the debit dialog asks for a reason",
          /reason/i.test(dialog),
          dialog.slice(0, 200),
        );
        await evaluate("document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))");
        await until("the debit dialog to close", "document.querySelector('[role=dialog]') === null", 3_000).catch(() => {});
      }
    }
  }
  await assertNewScreen("affiliates", { path: "/affiliates", expect: ["Affiliates"] });
  await assertNewScreen("locales", { path: "/locales", expect: ["Locales", "Languages"] });
  await assertNewScreen("imports", { path: "/imports", expect: ["Imports", "The dump"] });
  await assertNewScreen("updates", { path: "/settings/updates", expect: ["Updates"] });
  await assertNewScreen("marketplace", { path: "/marketplace", expect: ["Marketplace"] });
  await assertNewScreen("oauth-clients", { path: "/oauth-clients", expect: ["OAuth clients"] });

  // The operator screens that were APIs with no UI. Each of these was served and tested and
  // unreachable by a human, which is the class of gap the route-vs-view audit found.
  for (const surface of [
    { name: "holds", path: "/holds", expect: ["Holds"] },
    { name: "upgrades", path: "/upgrades", expect: ["Upgrades"] },
    { name: "providers", path: "/providers", expect: ["Sign-in providers"] },
    { name: "security", path: "/security", expect: ["Security"] },
    { name: "notification-templates", path: "/notification-templates", expect: ["Notification templates"] },
    { name: "automation", path: "/automation", expect: ["Automation"] },
    { name: "service-properties", path: "/service-properties", expect: ["Service properties"] },
    // Declared, routed and in the navigation, and its view is still a placeholder. Listed so
    // the ledger reports it as a gap rather than leaving it to be discovered: a screen that is
    // in the menu and not built is the state the route sweep reports as "not built yet".
    { name: "ticket-fields", path: "/ticket-fields", expect: ["Ticket fields"] },
    { name: "users", path: "/users", expect: ["Users"] },
    { name: "roles", path: "/roles", expect: ["Roles"] },
  ]) {
    await assertNewScreen(surface.name, { path: surface.path, expect: surface.expect });
  }

  // Gateway and provisioner configuration. These two are the screens an operator must be able
  // to use before the panel can take money or build a server, so they get more than a heading
  // check: the provider select must be populated from the server's own provider list, and the
  // connection test must be pressable and answer. A test that is not pressable is the same as
  // no test, and it is the failure a heading-only assertion cannot see.
  for (const surface of [
    { name: "gateways", path: "/gateways", endpoint: "/api/v1/staff/gateways", providerId: "#gw-provider", testPath: "/api/v1/staff/gateways/test", expect: ["Payment gateways", "Provider", "Connection"] },
    { name: "servers", path: "/servers", endpoint: "/api/v1/staff/servers", providerId: "#srv-provider", testPath: "/api/v1/staff/servers/test", expect: ["Provisioning servers", "Provisioner", "Connection"] },
  ]) {
    const result = await assertNewScreen(surface.name, { path: surface.path, expect: surface.expect });
    if (result.status !== "present") continue;

    // The provider list comes from the API, so an empty select is a screen wired to nothing.
    const options = await evaluate(`document.querySelectorAll('${surface.providerId} option').length`);
    check(`${surface.name}: offers the providers the server knows`, options > 1, `options=${options}`);

    // Choosing a provider must reveal the credential fields that provider declares. This is
    // the branch a static assertion never reaches: the fields are painted by a change handler.
    const chosen = await evaluate(`(() => {
      const sel = document.querySelector('${surface.providerId}');
      const real = Array.from(sel.options).find((o) => o.value !== '');
      if (!real) return null;
      sel.value = real.value;
      sel.dispatchEvent(new Event('change', { bubbles: true }));
      return real.value;
    })()`);
    if (chosen) {
      const painted = await evaluate("document.querySelectorAll('.field input, .field select').length");
      check(`${surface.name}: shows ${chosen}'s credential fields`, painted > 0, `fields=${painted}`);
      // A secret is write-only: no password input may carry a value the page could reveal.
      const leaked = await evaluate(`Array.from(document.querySelectorAll('input[type=password]')).filter((i) => i.value !== '').length`);
      check(`${surface.name}: no secret is rendered into the form`, leaked === 0, `${leaked} password field(s) had a value`);
    }

    // The connection test must be pressable and must answer with a result the operator can
    // read, whether that answer is "connected" or "not configured" — both are honest.
    const pressed = await evaluate(`(() => {
      const b = Array.from(document.querySelectorAll('button')).find((x) => /test connection/i.test(x.textContent || ''));
      if (!b) return false;
      b.click();
      return true;
    })()`);
    check(`${surface.name}: the connection test is pressable`, pressed, "no Test connection button");
    if (pressed) {
      // The test launches a child process, so it is not instant; wait for the answer rather
      // than for a fixed delay, or the assertion races the thing it is testing.
      await until(
        `${surface.name}'s test answer`,
        "!!document.querySelector('.banner')",
        30_000,
      ).catch(() => {});
      const answered = await evaluate("!!document.querySelector('.banner')");
      check(`${surface.name}: the connection test answers`, answered, "no result banner appeared");
    }
  }

  // ------------------------------------------------------------------ branding --
  //
  // The tenant's brand is the one setting whose *effect* is the feature: a form that saves is
  // not a brand, and a brand that reaches the panel but not the invoice is a brand the customer
  // never sees. So this block does not stop at "the screen renders". It sets a brand through the
  // form, and then reads the document back:
  //
  //   1. the accent is on <html> as an inline custom property, in the colour that was typed;
  //   2. the sidebar wordmark is the tenant's name, not the built-in one;
  //   3. switching the theme swaps the accent to the *other* mode's colour, which is what proves
  //      a brand is an overlay on the two themes rather than a replacement for one of them;
  //   4. resetting removes the inline property entirely — not "sets it to the default", which is
  //      the difference between a fallback and a lookalike, and the property the invoice's
  //      byte-identical fallback rests on;
  //   5. the wordmark comes back.
  //
  // The brand is reset at the end so the rest of the run sees the panel it expects.
  {
    const branding = await assertNewScreen("branding", {
      path: "/branding",
      expect: ["Branding", "Accent"],
    });
    if (branding.status === "present") {
      // Start from an unset brand, whatever a previous run left behind.
      //
      // This block resets the brand at the END, which is enough when a run finishes. It is not
      // enough when one crashes: the brand persists, and the next run's assertions read the
      // previous run's colours and fail with values nobody in this run typed. Clearing first makes
      // the block self-healing, which matters because the harness is run by several agents.
      await evaluate(`(() => {
        const b = Array.from(document.querySelectorAll('button'))
          .find((x) => /reset to the built-in/i.test(x.textContent || ''));
        if (b) b.click();
        return Boolean(b);
      })()`);
      await until(
        "the brand to be clear before the assertions",
        "document.documentElement.style.getPropertyValue('--accent').trim() === ''",
        10_000,
      ).catch(() => {});

      // Two distinct accents, so "the accent followed the theme" cannot be satisfied by the
      // built-in colour by accident.
      const LIGHT = "#c0392b";
      const DARK = "#1e88e5";
      const setField = async (id, value) => {
        const ok = await evaluate(`(() => {
          const el = document.querySelector(${JSON.stringify(id)});
          if (!el) return false;
          el.value = ${JSON.stringify(value)};
          el.dispatchEvent(new Event('input', { bubbles: true }));
          return true;
        })()`);
        check(`branding: the ${id} field accepts a value`, ok, `no ${id} field`);
      };
      await setField("#brand-name", "Acme Hosting");
      await setField("#brand-accent-light", LIGHT);
      await setField("#brand-accent-dark", DARK);

      // The preview must have repainted from the form, before any save: a preview that only
      // updates after a round trip is a preview of the last save, not of the values on screen.
      const preview = await evaluate(`(() => {
        const el = document.querySelector('.brand-preview');
        if (!el) return null;
        return {
          accent: el.style.getPropertyValue('--accent').trim(),
          name: (el.querySelector('.brand-name') || {}).textContent || '',
        };
      })()`);
      check("branding: the preview paints the typed accent", preview?.accent === LIGHT, JSON.stringify(preview));
      check("branding: the preview shows the typed name", (preview?.name || "").includes("Acme"), preview?.name ?? "");

      const saved = await evaluate(`(() => {
        const b = Array.from(document.querySelectorAll('button')).find((x) => /save brand/i.test(x.textContent || ''));
        if (!b) return false;
        b.click();
        return true;
      })()`);
      check("branding: the save button is pressable", saved, "no Save brand button");

      // Wait for the effect rather than for a delay: the shell applies the brand when the save
      // resolves, and the wordmark is the visible consequence.
      // Assert the accent we typed, not merely that some accent exists.
      //
      // This check used to be `!== ''`, which a brand left behind by an EARLIER run satisfies
      // perfectly — so it passed while the save was doing nothing, and the real failure surfaced
      // three assertions later as a mystery colour nobody in this run had typed.
      const applied = await until(
        "the brand to reach the shell",
        `document.documentElement.style.getPropertyValue('--accent').trim() === ${JSON.stringify(LIGHT)}`,
        10_000,
      ).catch(() => false);
      check(
        "branding: saving applies the accent to the panel",
        Boolean(applied),
        await evaluate("document.documentElement.style.getPropertyValue('--accent').trim()"),
      );

      const painted = await evaluate(`(() => {
        const mode = document.documentElement.dataset.theme === 'light' ? 'light' : 'dark';
        return {
          mode,
          inlineAccent: document.documentElement.style.getPropertyValue('--accent').trim(),
          wordmark: (document.querySelector('.sidebar .brand-name') || {}).textContent || '',
          // The properties a brand must never touch. A brand that could set these would be a
          // theme registry by another name, and would be able to make one mode unreadable.
          touchesSurface: document.documentElement.style.getPropertyValue('--bg') !== '',
          touchesText: document.documentElement.style.getPropertyValue('--text') !== '',
        };
      })()`);
      const expected = painted.mode === "light" ? LIGHT : DARK;
      check(
        `branding: the ${painted.mode} theme uses its own accent`,
        painted.inlineAccent === expected,
        `${painted.inlineAccent} (want ${expected})`,
      );
      check("branding: the sidebar wordmark is the tenant's name", painted.wordmark.includes("Acme"), painted.wordmark);
      check("branding: the brand does not touch the surface colour", !painted.touchesSurface);
      check("branding: the brand does not touch the text colour", !painted.touchesText);

      // The theme toggle must swap the accent, not keep the one that was painted last. This is
      // the assertion that the light and dark modes are still two modes.
      const beforeToggle = painted.mode;
      await click("#theme-toggle");
      const afterToggle = await evaluate(`(() => {
        const mode = document.documentElement.dataset.theme === 'light' ? 'light' : 'dark';
        return { mode, accent: document.documentElement.style.getPropertyValue('--accent').trim() };
      })()`);
      check("branding: the theme toggle still switches the theme", afterToggle.mode !== beforeToggle, `${beforeToggle} -> ${afterToggle.mode}`);
      check(
        `branding: the ${afterToggle.mode} theme uses its own accent`,
        afterToggle.accent === (afterToggle.mode === "light" ? LIGHT : DARK),
        `${afterToggle.accent}`,
      );
      // Back to the mode the run started in.
      await click("#theme-toggle");

      // The public read is what a storefront and a fresh panel use, so the brand must be there
      // and must be the values that were saved.
      //
      // `no-store` is required here and is not a detail: the endpoint is deliberately
      // cacheable (`public, max-age=60`) because it is on the storefront's critical path, and
      // the panel fetched it at startup — so a plain `fetch` would be answered from the HTTP
      // cache with the *empty* brand the page loaded with, and the assertion would fail while
      // the server was perfectly correct. The question here is what the server says, not what
      // this browser remembered.
      const api = await evaluate(`fetch('/api/v1/branding', {
        cache: 'no-store',
        headers: { Accept: 'application/json' },
      }).then((r) => r.json()).catch(() => null)`);
      check("branding: the public brand read carries the saved name", api?.name === "Acme Hosting", JSON.stringify(api?.name));
      check("branding: the public brand read carries both accents", api?.accent_light === LIGHT && api?.accent_dark === DARK, JSON.stringify([api?.accent_light, api?.accent_dark]));

      await screenshot("branding-branded");

      // Reset, and prove the fallback in the browser: the inline property is gone, not defaulted.
      const reset = await evaluate(`(() => {
        const b = Array.from(document.querySelectorAll('button')).find((x) => /reset to the built-in/i.test(x.textContent || ''));
        if (!b) return false;
        b.click();
        return true;
      })()`);
      check("branding: the reset button is pressable", reset, "no Reset button");
      const cleared = await until(
        "the brand to be cleared",
        "document.documentElement.style.getPropertyValue('--accent').trim() === ''",
        10_000,
      ).catch(() => false);
      check("branding: resetting removes the accent from the panel", Boolean(cleared));
      const restored = await evaluate(`(() => ({
        wordmark: (document.querySelector('.sidebar .brand-name') || {}).textContent || '',
        anyBrandProperty: ['--accent', '--accent-ink', '--accent-text', '--accent-soft', '--accent-soft-strong']
          .some((p) => document.documentElement.style.getPropertyValue(p) !== ''),
      }))()`);
      check("branding: resetting restores the built-in wordmark", restored.wordmark.includes("Catalyst"), restored.wordmark);
      check("branding: an unset brand writes no custom property at all", !restored.anyBrandProperty);
    }
  }

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
  await goto("/dashboard");
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
  await goto("/dashboard");
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
    await goto("/dashboard");
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
    // The funnel has to be reachable from where a customer starts. Before this, the storefront
    // and the cart had no link anywhere in the panel, so buying meant typing a URL.
    const portalDoors = await evaluate(`(() => {
      const hrefs = [...document.querySelectorAll('.portal-hero a')].map((a) => a.getAttribute('href'));
      return { products: hrefs.includes('/products'), cart: hrefs.includes('/cart') };
    })()`);
    check("portal: offers a way to the storefront", portalDoors.products, JSON.stringify(portalDoors));
    check("portal: offers a way to the cart", portalDoors.cart, JSON.stringify(portalDoors));

    // The storefront, as a signed-in customer. The same four properties as the operator's pass
    // above, with one difference that matters: a customer's way back is the portal, not the panel,
    // because `/dashboard` is staff-only and would bounce them. A customer who could reach the shop
    // but not return to their own invoices had traded one dead end for another.
    {
      await goto("/");
      const shop = await evaluate(`(() => ({
        path: location.pathname,
        storefront: document.querySelector('.store-page') !== null,
        shell: document.querySelector('.app') !== null,
        back: [...document.querySelectorAll('.store-nav a, .store-hero-actions a')]
          .map((a) => a.getAttribute('href'))
          .filter((href) => href === '/portal' || href === '/dashboard'),
      }))()`);
      check("storefront: a customer sees the shop at /", shop.storefront, JSON.stringify(shop));
      check("storefront: / is not redirected for a customer", shop.path === "/", shop.path);
      check("storefront: no panel shell around a customer's shop", !shop.shell);
      check("storefront: a customer's way back is the portal", shop.back.includes("/portal"), JSON.stringify(shop.back));
      check("storefront: a customer is not sent to the staff panel", !shop.back.includes("/dashboard"), JSON.stringify(shop.back));
    }
    for (const [name, path] of [
      ["portal-invoices", "/portal/invoices"],
      ["portal-services", "/portal/services"],
      ["portal-profile", "/portal/profile"],
    ]) {
      await assertScreen(name, { path, expect: [] });
    }

    // ------------------------------------------- the live server card (SVC-13) --
    //
    // The card is mounted empty and filled by a second request, so "the page rendered" proves
    // nothing about it. Same for the power buttons: they are built from `can_power`, and a card
    // that failed to load shows none. So this waits for the card to settle and then asserts that it
    // says one of the three true things — a live state, "no server yet", or a sentence explaining
    // that the provider could not be asked — because an empty box is the failure this guards.
    {
      await goto("/portal/services");
      const href = await evaluate(`(() => {
        const a = document.querySelector('a[href^="/portal/services/"]');
        return a ? a.getAttribute('href') : null;
      })()`);
      if (href) {
        await goto(href);
        const card = await evaluate(`(async () => {
          const deadline = Date.now() + 8000;
          let body = null;
          while (Date.now() < deadline) {
            const titles = [...document.querySelectorAll('h2, h3, .card-title')];
            const head = titles.find((t) => /^Server$/i.test((t.textContent || '').trim()));
            if (head) {
              const el = head.closest('.card') || head.parentElement;
              const text = el ? el.textContent || '' : '';
              if (!/Checking the server/i.test(text)) { body = text; break; }
            }
            await new Promise((r) => setTimeout(r, 150));
          }
          return body;
        })()`);
        if (card === null) {
          check("portal service: a Server card settles instead of sitting on 'Checking'", false);
        } else {
          check(
            "portal service: the Server card states a fact, not a blank box",
            /State|has no server yet|could not be read|not available|does not offer power|does not recognise/i.test(card),
            card.replace(/\s+/g, " ").slice(0, 240),
          );
          // "The server panel does not recognise this service" is one of the settled, truthful
          // answers, not a failure: the seeded service carries a provider id the fixture does not
          // know, so this exercises the path where the panel asked and was told no — which the card
          // must report rather than render as an empty box or a 5xx.
          //
          // Whatever buttons it does show must be actions the API accepts. A "Reboot" would be a
          // button whose only outcome is a 400.
          const labels = await evaluate(`(() => {
            const titles = [...document.querySelectorAll('h2, h3, .card-title')];
            const head = titles.find((t) => /^Server$/i.test((t.textContent || '').trim()));
            const el = head ? (head.closest('.card') || head.parentElement) : null;
            return el ? [...el.querySelectorAll('button')].map((b) => b.textContent.trim()) : [];
          })()`);
          const allowed = ["Start", "Restart", "Stop", "Force stop", "Retry"];
          const bad = labels.filter((l) => !allowed.includes(l));
          check(
            "portal service: the Server card offers only actions the API accepts",
            bad.length === 0,
            JSON.stringify(labels),
          );
        }
      }
      await goto("/portal");
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

    // ------------------------------------------------ the purchase funnel --
    //
    // Storefront -> add to cart -> cart -> checkout -> invoice -> paid, entirely through the
    // browser. This is the path the panel did not have: the product page's only call to action
    // was `href="/login?next=/portal"`, which forgot the product, and nothing in the frontend
    // ever called `POST /api/v1/portal/cart/items`, so a customer could not buy anything without
    // an operator typing the order into /orders/new.
    //
    // Every step is a click or a keystroke. The harness reads the URL it landed on and the text
    // the screen rendered; it never calls the API to make the funnel work.
    //
    // The last step has two honest shapes and the assertion covers both: a customer holding
    // credit has it applied at issuance (`credits.auto_use`), and an installation that leaves
    // that off leaves the invoice payable, where "Pay with credit" settles it in one click.
    // Neither needs a payment gateway. A gateway payment cannot be completed here — the
    // fixture gateway's hosted page is `https://fakegateway.invalid/...` — so the funnel stops
    // at a paid invoice and the card charge stays the provider's boundary.
    const funnelSlug = arg("expect-product-slug");
    const funnelProduct = arg("expect-product");
    if (funnelSlug) {
      // The seed puts a line in the cart so the cart screen has content. This test adds its own,
      // so it starts by emptying it — through the control a customer would use.
      await visit("/cart");
      const removed = await evaluate(`(() => {
        const b = document.querySelector('.cart-line-side button[aria-label^="Remove"]');
        if (!b) return false;
        b.click();
        return true;
      })()`);
      check("funnel: the cart can be emptied from the cart", removed, "no remove control on the seeded line");
      const emptied = await until("the cart to empty", "/your cart is empty/i.test(document.body.innerText)", 10_000).catch(() => false);
      check("funnel: removing its only line empties the cart", Boolean(emptied));

      // A signed-in customer must be able to reach the storefront: it is the first step of the
      // funnel, and the shell used to send them to the portal from every storefront URL.
      await goto(`/products/${funnelSlug}`);
      const productPath = await evaluate("location.pathname");
      check("funnel: a signed-in customer stays on the product page", productPath === `/products/${funnelSlug}`, productPath);
      const productText = await pageText();
      check("funnel: the product page names the product", productText.includes(funnelProduct), productText.slice(0, 300));
      check("funnel: the product page offers to order", await evaluate("document.querySelector('#product-order') !== null"), "no #product-order");
      await screenshot("funnel-product-page");

      // Add *that* plan. The button used to send the customer to the client area and forget it.
      await click("#product-order");
      const landedOnCart = await until("the cart after ordering", "location.pathname === '/cart'", 15_000).catch(() => false);
      check("funnel: ordering a plan lands on the cart", Boolean(landedOnCart), await evaluate("location.pathname"));
      await until("the cart to render its line", "document.querySelector('.cart-line') !== null", 15_000).catch(() => {});
      const cartAfterAdd = await pageText();
      check("funnel: the cart holds the plan that was ordered", cartAfterAdd.includes(funnelProduct), cartAfterAdd.slice(0, 300));
      const cartQty = await evaluate("document.querySelector('.cart-qty-value')?.innerText ?? ''");
      check("funnel: the ordered plan is one line of one", cartQty.trim() === "1", `quantity=${cartQty}`);
      // The amount the cart shows is the server's. It is captured here so the invoice can be
      // compared against it rather than against a number this test invented.
      const cartTotal = await evaluate(
        "document.querySelector('.cart-total-row--strong .cart-total-value')?.innerText ?? ''",
      );
      const cartTotalNumber = String(cartTotal).match(/[0-9][0-9.,]*/)?.[0] ?? "";
      check("funnel: the cart shows a priced total", cartTotalNumber !== "", cartTotal);
      await screenshot("funnel-cart");

      // The same plan a second time. A product sold one at a time is refused with a message that
      // says so; a product that allows a quantity merges into one line (proved in the Go tests,
      // internal/httpapi TestPortalCartDoubleAdd). Either way the customer is never left with a
      // cart the checkout will reject.
      await goto(`/products/${funnelSlug}`);
      await click("#product-order");
      const refused = await until(
        "the refusal for a second add",
        "/already in your cart/i.test(document.body.innerText)",
        12_000,
      ).catch(() => false);
      check("funnel: adding the same plan twice is refused with a clear message", Boolean(refused));
      const stayedPut = await evaluate("location.pathname");
      check("funnel: the refused add does not navigate away", stayedPut === `/products/${funnelSlug}`, stayedPut);
      await screenshot("funnel-double-add-refused");

      // Checkout: the cart's own button, then the order's own button.
      await goto("/cart");
      const offersCheckout = await evaluate("document.querySelector('a[href=\"/checkout\"]') !== null");
      check("funnel: the cart offers checkout", offersCheckout, "no link to /checkout");
      await click('a[href="/checkout"]');
      await until("the checkout screen", "location.pathname === '/checkout' && document.querySelector('.cart-totals') !== null", 15_000);
      const checkoutText = await pageText();
      check("funnel: checkout shows the order it is about to place", checkoutText.includes(funnelProduct), checkoutText.slice(0, 300));
      check("funnel: checkout offers to place the order", await evaluate("document.querySelector('#place-order') !== null"), "no #place-order");
      // The terms box is ticked when it is rendered: the server, not this test, decides whether
      // it is required, and a refusal here is reported as the refusal it is.
      if (await evaluate("document.querySelector('#checkout-tos') !== null")) await click("#checkout-tos");
      await click("#place-order");
      const onInvoice = await until(
        "the invoice after placing the order",
        "location.pathname.startsWith('/portal/invoices/')",
        25_000,
      ).catch(() => false);
      check("funnel: placing the order opens the invoice", Boolean(onInvoice), await evaluate("location.pathname"));
      await until("the invoice document", "document.querySelector('.invoice-doc') !== null", 15_000).catch(() => {});
      const invoiceText = await pageText();
      check("funnel: the invoice has a number", /INV-[0-9]+/.test(invoiceText), invoiceText.slice(0, 300));
      check(
        "funnel: the invoice is for the amount the cart showed",
        cartTotalNumber !== "" && invoiceText.includes(cartTotalNumber),
        `cart=${cartTotalNumber}`,
      );

      // Paid. Credit on the account settles it at issuance when `credits.auto_use` is on; when it
      // is off the invoice stays payable and the customer spends the balance with one click.
      let paid = /paid in full/i.test(invoiceText);
      if (!paid) {
        const creditOffered = await evaluate("document.querySelector('#invoice-pay-with-credit') !== null");
        check("funnel: a payable invoice offers to be paid from credit", creditOffered, "no #invoice-pay-with-credit");
        if (creditOffered) {
          await click("#invoice-pay-with-credit");
          paid = await until("the invoice to be paid", "/paid in full/i.test(document.body.innerText)", 20_000).catch(() => false);
        }
      }
      check("funnel: the invoice ends up paid", paid, invoiceText.slice(0, 400));
      const invoiceBadge = await evaluate("document.querySelector('.page-head .badge')?.innerText ?? ''");
      check("funnel: the invoice badge says paid", /paid/i.test(invoiceBadge), invoiceBadge);
      await screenshot("funnel-invoice-paid");

      // And the portal lists it as paid, which is what the customer will look at tomorrow.
      await goto("/portal/invoices");
      const portalInvoices = await pageText();
      check("funnel: the portal lists the new invoice", /INV-[0-9]+/.test(portalInvoices), portalInvoices.slice(0, 300));
      check("funnel: the portal shows it as paid", /paid/i.test(portalInvoices), portalInvoices.slice(0, 300));
      await screenshot("funnel-portal-invoices");
    }

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

    // The customer's half of the same flow. The screen is built by another package and its
    // selectors differ slightly, but the contract is the same one and it is driven the same way:
    // a real code from the secret on screen, recovery codes shown once, and a disable that demands
    // the password and a code together. The factor is removed at the end so the account is left as
    // it was found — a run that left MFA on would break the next sign-in it performs.
    {
      await goto("/portal/security");
      const state = await screenState();
      if (!missingReason(state)) {
        await until("the portal two-factor section", "document.querySelector('#two-factor') !== null", 10_000);
        const before = await evaluate("document.querySelector('#two-factor .badge')?.textContent.trim() ?? ''");
        check("portal-mfa: the customer's security page reports a state", before.length > 0, "no badge in the two-factor section");

        if (before === "Off") {
          const enrolled = await driveEnrolment({
            codeFieldId: "portal-mfa-code",
            requireQr: false,
            requireDownload: false,
            shotPrefix: "portal-mfa",
          });
          if (enrolled?.secret) {
            await until("the portal status to say On", `(() => { const b = document.querySelector('#two-factor .badge'); return b && b.textContent.trim() === 'On'; })()`, 10_000).catch(() => {});
            const after = await evaluate("document.querySelector('#two-factor .badge')?.textContent.trim() ?? ''");
            check("portal-mfa: the customer's state reads On after confirming", after === "On", after);

            await driveDisable({
              password: customerPassword,
              secret: enrolled.secret,
              passwordFieldId: "portal-mfa-disable-password",
              codeFieldId: "portal-mfa-disable-code",
            });
            await until("the portal status to say Off", `(() => { const b = document.querySelector('#two-factor .badge'); return b && b.textContent.trim() === 'Off'; })()`, 10_000).catch(() => {});
            const restored = await evaluate("document.querySelector('#two-factor .badge')?.textContent.trim() ?? ''");
            check("portal-mfa: the customer's state reads Off again", restored === "Off", restored);
          }
        } else {
          // A previous run left a factor behind, or support reset it. Both are legitimate states;
          // the point is that the screen says which one, so the harness reports it and moves on.
          console.log(`     note: portal-mfa: the customer already has a factor (${before}); enrolment not driven`);
          check("portal-mfa: an existing factor is reported by name", /on|removed/i.test(before), before);
        }
      }
    }

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

    // ---------------------------------------- the funnel for a visitor --
    //
    // The same first step, from the other audience. The product page is public and its buy
    // button has to survive the sign-in round trip: the defect this replaces was
    // `href="/login?next=/portal"`, which dropped the product entirely, so a visitor who
    // clicked "Order this plan" arrived at an empty portal with nothing in the cart and no way
    // to add one. The chosen plan now travels in `next`, and the product page adds it on the
    // way back.
    if (funnelSlug) {
      // The order placed above consumed the cart, so this starts from an empty one. If the cart
      // still held the line, the add below would be refused as a duplicate — which is the
      // behaviour proved earlier, not a failure of this step.
      await visit("/cart");
      const stillHeld = await evaluate(`(() => {
        const b = document.querySelector('.cart-line-side button[aria-label^="Remove"]');
        if (!b) return false;
        b.click();
        return true;
      })()`);
      if (stillHeld) {
        await until("the cart to empty", "/your cart is empty/i.test(document.body.innerText)", 10_000).catch(() => {});
      }

      // Sign out, the way a customer does.
      await goto("/dashboard");
      await click(".sidebar-foot .who");
      await click(".menu-item--danger");
      await until("the sign-in screen", "document.querySelector('.auth-panel') !== null", 8000);

      await goto(`/products/${funnelSlug}`);
      const visitorPath = await evaluate("location.pathname");
      check("funnel-visitor: the product page is public", visitorPath === `/products/${funnelSlug}`, visitorPath);
      await click("#product-order");
      const onSignIn = await until("the sign-in page for a visitor", "location.pathname === '/login'", 12_000).catch(() => false);
      check("funnel-visitor: ordering as a visitor asks for a sign-in", Boolean(onSignIn), await evaluate("location.pathname"));
      const next = await evaluate("new URLSearchParams(location.search).get('next') ?? ''");
      check(
        "funnel-visitor: the sign-in page remembers which plan was chosen",
        next.includes(funnelSlug) && /add=/.test(next),
        next,
      );

      // The sign-in screen is a module the router loads on demand, so its form can be replaced
      // once more after the first paint. Waiting for the form itself — not just for the pathname —
      // is what keeps the keystrokes and the click on the same instance.
      await until(
        "the sign-in form for a visitor",
        "document.querySelector('.auth-panel form.auth-form #email') !== null && document.querySelector('.skeleton') === null",
        15_000,
      ).catch(() => {});
      await new Promise((r) => setTimeout(r, 200));

      await typeInto("#email", customerEmail);
      await typeInto("#password", customerPassword);
      // The sign-in screen is a module the router loads on demand, so it can be replaced once
      // more after its first paint. Checking what the form actually holds — rather than assuming
      // the keystrokes landed — turns that into a named failure instead of a mysterious one.
      const typed = await evaluate(
        "[document.querySelector('#email')?.value ?? '', document.querySelector('#password')?.value ?? '']",
      );
      check(
        "funnel-visitor: the sign-in form holds what was typed",
        typed[0] === customerEmail && typed[1] === customerPassword,
        JSON.stringify(typed),
      );
      await click(".auth-form .btn--primary");
      const backToCart = await until("the cart after signing in", "location.pathname === '/cart'", 25_000).catch(() => false);
      check("funnel-visitor: signing in adds the plan and opens the cart", Boolean(backToCart), await evaluate("location.pathname"));
      if (!backToCart) {
        // The most likely reason a password sign-in stops here is not the funnel: the customer
        // account has a second factor enrolled, left behind by the MFA block that runs just above
        // (or by an interrupted earlier run). Saying so turns a confusing "landed on /login" into
        // the state that actually needs clearing.
        const challenged = await evaluate("document.querySelectorAll('.otp-digit').length > 0");
        if (challenged) {
          console.log(
            "     note: this customer account has a second factor enrolled, so the password sign-in " +
              "was challenged for a code. The MFA block above is expected to remove the factor; a " +
              "leftover one fails this step, not the funnel.",
          );
        }
        console.log(`     note: after sign-in the screen reads: ${(await pageText()).slice(0, 300)}`);
      }
      await until("the cart to render its line", "document.querySelector('.cart-line') !== null", 15_000).catch(() => {});
      const visitorCart = await pageText();
      check("funnel-visitor: the cart holds the plan the visitor chose", visitorCart.includes(funnelProduct), visitorCart.slice(0, 300));
      await screenshot("funnel-visitor-cart");
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
  const order = { present: 0, broken: 1, gap: 2, missing: 3 };
  const sorted = [...screenLedger].sort((a, b) => order[a.status] - order[b.status]);
  const count = (s) => screenLedger.filter((e) => e.status === s).length;
  console.log(
    `\nnew screens: ${count("present")} present, ${count("broken")} broken, ` +
      `${count("gap")} reachability gaps, ${count("missing")} not in this build`,
  );
  for (const entry of sorted) {
    const mark =
      entry.status === "present" ? "ok   " : entry.status === "broken" ? "FAIL " : entry.status === "gap" ? "gap  " : "skip ";
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
