#!/usr/bin/env node
// Drive the live server card in a real headless Chromium.
//
// This is not a unit test: it signs in, loads the service page of a service whose provisioned id points
// at the real Catalyst panel, and asserts what a person would see. The assertions are the customer's
// requirements, one line each — the server's name, its address, the button that opens the panel, the
// power buttons that match its current state, and the metrics.

import { spawn } from "node:child_process";
import { writeFileSync, mkdirSync } from "node:fs";

const BASE = process.env.BASE_URL || "http://127.0.0.1:8797";
const SERVICE = process.env.SERVICE_ID;
const EMAIL = process.env.ADMIN_EMAIL || "admin@dev.local";
const PASSWORD = process.env.ADMIN_PASSWORD || "dev-only-password-1";
const AS_CUSTOMER = process.env.AS_CUSTOMER === "1";
const SHOTS = process.env.SHOTS || "/tmp/verify-shots";

if (!SERVICE) {
  console.error("SERVICE_ID is required");
  process.exit(2);
}

const failures = [];
function check(name, condition, detail) {
  if (condition) {
    console.log(`  ok   ${name}`);
  } else {
    console.log(`  FAIL ${name}${detail ? ` — ${detail}` : ""}`);
    failures.push(name);
  }
}

const chromeBin = process.env.CHROME_BIN || "chromium";
const port = 9900 + Math.floor(Math.random() * 90);
const profile = `/tmp/billing-card-${Date.now()}`;
const env = { ...process.env };
// A headless Chromium on this host stalls on its first navigation when the desktop session bus
// variables are inherited.
delete env.DBUS_SESSION_BUS_ADDRESS;
delete env.QT_ACCESSIBILITY;

const child = spawn(
  chromeBin,
  [
    "--headless=new",
    "--no-sandbox",
    "--disable-gpu",
    "--disable-dev-shm-usage",
    `--remote-debugging-port=${port}`,
    `--user-data-dir=${profile}`,
    "--window-size=1440,1200",
    "about:blank",
  ],
  { env, stdio: ["ignore", "ignore", "pipe"] },
);
const chromeLog = [];
child.stderr.on("data", (c) => chromeLog.push(c.toString()));

// endpoint returns the *page* target's socket, not the browser's: the Page domain only exists on a
// page target, and the browser endpoint refuses Page.enable.
async function endpoint() {
  const deadline = Date.now() + 30_000;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(`http://127.0.0.1:${port}/json/list`);
      const targets = await res.json();
      const page = targets.find((t) => t.type === "page" && t.webSocketDebuggerUrl);
      if (page) return page.webSocketDebuggerUrl;
    } catch {
      /* not up yet */
    }
    await new Promise((r) => setTimeout(r, 150));
  }
  throw new Error("chromium did not expose a page target:\n" + chromeLog.join(""));
}

let ws;
let nextId = 1;
const pending = new Map();

async function connect() {
  const url = await endpoint();
  ws = new WebSocket(url);
  await new Promise((resolve, reject) => {
    ws.addEventListener("open", resolve, { once: true });
    ws.addEventListener("error", reject, { once: true });
  });
  ws.addEventListener("message", (event) => {
    const msg = JSON.parse(event.data);
    if (msg.id && pending.has(msg.id)) {
      const { resolve, reject } = pending.get(msg.id);
      pending.delete(msg.id);
      if (msg.error) reject(new Error(JSON.stringify(msg.error)));
      else resolve(msg.result);
    }
  });
}

function send(method, params = {}) {
  const id = nextId++;
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    ws.send(JSON.stringify({ id, method, params }));
  });
}

async function evaluate(expression) {
  const res = await send("Runtime.evaluate", {
    expression,
    awaitPromise: true,
    returnByValue: true,
  });
  if (res.exceptionDetails) throw new Error(JSON.stringify(res.exceptionDetails));
  return res.result.value;
}

async function until(description, expression, timeoutMs = 20_000) {
  const deadline = Date.now() + timeoutMs;
  let last;
  while (Date.now() < deadline) {
    try {
      last = await evaluate(expression);
      if (last) return last;
    } catch (err) {
      last = String(err);
    }
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error(`timed out waiting for ${description} (last: ${JSON.stringify(last)})`);
}

async function goto(path) {
  await send("Page.navigate", { url: new URL(path, BASE).toString() });
  await until(`load of ${path}`, "document.readyState === 'complete'", 25_000);
}

async function typeInto(selector, text) {
  await until(`selector ${selector}`, `document.querySelector(${JSON.stringify(selector)}) !== null`).catch(async (err) => {
    // A missing input is almost always a page that has not hydrated. Saying what the page actually
    // contains turns "timed out waiting for #email" into something actionable.
    const where = await evaluate("location.href");
    const seen = await evaluate("document.body ? document.body.innerText.slice(0, 300) : '(no body)'");
    const ids = await evaluate("[...document.querySelectorAll('input,form,button')].map(e => e.id || e.tagName).join(',')");
    throw new Error(`${err.message}\n    at: ${where}\n    controls: ${ids}\n    text: ${seen}`);
  });
  await evaluate(`document.querySelector(${JSON.stringify(selector)}).focus()`);
  for (const ch of text) {
    await send("Input.insertText", { text: ch });
  }
}

async function shot(name) {
  const res = await send("Page.captureScreenshot", { format: "png", captureBeyondViewport: true });
  mkdirSync(SHOTS, { recursive: true });
  const path = `${SHOTS}/${name}.png`;
  writeFileSync(path, Buffer.from(res.data, "base64"));
  console.log(`  shot ${path}`);
}

const main = async () => {
  await connect();
  await send("Page.enable");
  await send("Runtime.enable");
  await send("Network.enable");

  // Collect page errors: a card that throws before painting looks like an empty card.
  const pageErrors = [];
  ws.addEventListener("message", (event) => {
    const msg = JSON.parse(event.data);
    if (msg.method === "Runtime.exceptionThrown") {
      pageErrors.push(msg.params.exceptionDetails?.exception?.description ?? "exception");
    }
  });

  console.log(`signing in as ${EMAIL}`);
  await goto("/login");
  await typeInto("#email", EMAIL);
  await typeInto("#password", PASSWORD);
  await evaluate(`document.querySelector("form.auth-form, form")?.requestSubmit()`);
  await until("post-login redirect", "location.pathname !== '/login'", 25_000);

  const path = AS_CUSTOMER ? `/portal/services/${SERVICE}` : `/services/${SERVICE}`;
  console.log(`opening ${path}`);
  await goto(path);

  // The card fills asynchronously: the page paints, then the live read lands.
  await until(
    "the server card to have content",
    `(() => {
       const card = [...document.querySelectorAll('section.card')]
         .find((c) => c.querySelector('.card-title')?.textContent.trim().startsWith('Server'));
       if (!card) return false;
       const body = card.querySelector('.card-body');
       return !!body && body.innerText.trim().length > 20;
     })()`,
    30_000,
  );

  // Read the card's text once, and assert against it: this is what the customer sees.
  const cardText = await evaluate(`(() => {
     const card = [...document.querySelectorAll('section.card')]
       .find((c) => c.querySelector('.card-title')?.textContent.trim().startsWith('Server'));
     return card?.innerText ?? document.body.innerText;
   })()`);
  const links = await evaluate(`[...document.querySelectorAll('a')].map(a => a.getAttribute('href') || '')`);
  const buttons = await evaluate(`[...document.querySelectorAll('button')].map(b => b.innerText.trim())`);

  console.log("\ncard text:\n" + cardText.split("\n").filter(Boolean).map((l) => "  | " + l).join("\n") + "\n");

  // The assertions follow the state the panel reports, because the card's whole job is to follow the
  // provider: a running server must offer stop/restart/force-stop and must not offer start, and a stopped
  // one the reverse. Asserting a fixed button set would test the fixture, not the feature.
  const state = (await evaluate(`(() => {
      const card = [...document.querySelectorAll('section.card')]
        .find((c) => c.querySelector('.card-title')?.textContent.trim().startsWith('Server'));
      return card?.innerText ?? '';
    })()`));
  // The state word can appear anywhere in the card (the pill sits beside the name, the sentence below it),
  // so the whole card is searched rather than a fixed line.
  const reported = /\b(running|starting|stopped|stopping|restarting|installing|suspended|crashed)\b/i.exec(state);
  const stateWord = (reported?.[1] ?? '').toLowerCase();

  const panelName = process.env.EXPECT_NAME ?? '';
  check("the server's name is shown", panelName ? state.includes(panelName) : /Server \S+/.test(state),
    `expected ${panelName || '<name from the panel>'} in the card`);
  // An address is host:port — or a bare host when the provider reports no port.
  check("the address is shown", /\b(\d{1,3}\.){3}\d{1,3}(:\d{1,5})?\b|\b[a-z0-9.-]+:\d{1,5}\b/i.test(cardText),
    "no host:port in the card");
  check("a link to the panel is present", links.some((h) => h.includes("panel.catalystctl.com/servers/")), JSON.stringify(links));
  check("the state is shown", stateWord !== '', `no recognisable state in the card heading`);

  const has = (re) => buttons.some((b) => re.test(b));
  if (stateWord === "running" || stateWord === "starting") {
    check("a running server offers stop", has(/^stop$/i), JSON.stringify(buttons));
    check("a running server offers restart", has(/^restart$/i), JSON.stringify(buttons));
    check("a running server offers force stop", has(/^force stop$/i), JSON.stringify(buttons));
    check("a running server does not offer start", !has(/^start$/i), JSON.stringify(buttons));
  } else if (stateWord === "stopped" || stateWord === "crashed") {
    check("a stopped server offers start", has(/^start$/i), JSON.stringify(buttons));
    check("a stopped server does not offer stop", !has(/^stop$/i), JSON.stringify(buttons));
  } else {
    console.log(`  note state ${stateWord || "unknown"}; no power buttons are expected`);
  }
  check("metrics are rendered", /cpu|memory|disk/i.test(cardText), cardText.slice(0, 300));

  await shot(AS_CUSTOMER ? "portal-service-server" : "staff-service-server");
  check("no uncaught page errors", pageErrors.length === 0, pageErrors.join("; "));
};

main()
  .then(() => {
    child.kill("SIGTERM");
    if (failures.length) {
      console.error(`\n${failures.length} assertion(s) failed: ${failures.join(", ")}`);
      process.exit(1);
    }
    console.log("\nall assertions passed");
    process.exit(0);
  })
  .catch((err) => {
    console.error("failed:", err.message);
    child.kill("SIGTERM");
    process.exit(1);
  });
