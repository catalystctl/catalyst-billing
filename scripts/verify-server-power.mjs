#!/usr/bin/env node
// Click a power button in a real browser and prove the panel acted on it.
//
// verify-server-card.mjs proves the card *renders* the truth. This proves the button *does* something: it
// signs in, opens the service page, presses Stop, and then watches both the card and the panel until the
// server is stopped. A button that renders correctly and does nothing is exactly the failure a screenshot
// cannot catch, so this drives the click and checks the effect in two independent places.
//
// Usage:
//   BASE_URL=… POWER_ACTION=stop SERVICE_ID=… PANEL_KEY=… PANEL_SERVER_ID=… node scripts/verify-server-power.mjs

import { spawn } from "node:child_process";
import { writeFileSync, mkdirSync } from "node:fs";

const BASE = process.env.BASE_URL || "http://127.0.0.1:8798";
const SERVICE = process.env.SERVICE_ID;
const EMAIL = process.env.LOGIN_EMAIL || "admin@dev.local";
const PASSWORD = process.env.LOGIN_PASSWORD || "dev-only-password-1";
const AS_CUSTOMER = process.env.AS_CUSTOMER === "1";
const ACTION = (process.env.POWER_ACTION || "stop").toLowerCase();
const PANEL = process.env.PANEL_URL || "https://panel.catalystctl.com";
const PANEL_KEY = process.env.PANEL_KEY;
const PANEL_SERVER = process.env.PANEL_SERVER_ID;
const SHOTS = process.env.SHOTS || "./shots";

if (!SERVICE) {
  console.error("SERVICE_ID is required");
  process.exit(2);
}

const failures = [];
const check = (name, ok, detail) => {
  if (ok) console.log(`  ok   ${name}`);
  else {
    console.log(`  FAIL ${name}${detail ? ` — ${detail}` : ""}`);
    failures.push(name);
  }
};

/** Read the server's state from the panel, which is the authority. */
async function panelState() {
  const res = await fetch(`${PANEL}/api/servers/${PANEL_SERVER}`, {
    headers: { Authorization: `Bearer ${PANEL_KEY}` },
  });
  const body = await res.json();
  return body?.data?.status ?? "unknown";
}

const chromeBin = process.env.CHROME_BIN || "chromium";
const port = 9900 + Math.floor(Math.random() * 90);
const profile = `/tmp/billing-power-${Date.now()}`;
const env = { ...process.env };
// A headless Chromium on this host stalls on its first navigation when the desktop session bus variables
// are inherited.
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
  const res = await send("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true });
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
    await new Promise((r) => setTimeout(r, 250));
  }
  throw new Error(`timed out waiting for ${description} (last: ${JSON.stringify(last)})`);
}

async function goto(path) {
  await send("Page.navigate", { url: new URL(path, BASE).toString() });
  await until(`load of ${path}`, "document.readyState === 'complete'", 25_000);
}

async function typeInto(selector, text) {
  await until(`selector ${selector}`, `document.querySelector(${JSON.stringify(selector)}) !== null`);
  await evaluate(`document.querySelector(${JSON.stringify(selector)}).focus()`);
  for (const ch of text) await send("Input.insertText", { text: ch });
}

async function shot(name) {
  const res = await send("Page.captureScreenshot", { format: "png", captureBeyondViewport: true });
  mkdirSync(SHOTS, { recursive: true });
  writeFileSync(`${SHOTS}/${name}.png`, Buffer.from(res.data, "base64"));
  console.log(`  shot ${SHOTS}/${name}.png`);
}

/** The card's own text, which is what a person reads. */
const cardTextExpr = `(() => {
  const card = [...document.querySelectorAll('section.card')]
    .find((c) => c.querySelector('.card-title')?.textContent.trim().startsWith('Server'));
  return card?.innerText ?? '';
})()`;

/** Click a button by its visible label, inside the server card. */
const clickButtonExpr = (label) => `(() => {
  const card = [...document.querySelectorAll('section.card')]
    .find((c) => c.querySelector('.card-title')?.textContent.trim().startsWith('Server'));
  if (!card) return 'no card';
  const btn = [...card.querySelectorAll('button')].find((b) => b.innerText.trim().toLowerCase() === ${JSON.stringify(label)});
  if (!btn) return 'no button';
  if (btn.disabled) return 'disabled';
  btn.click();
  return 'clicked';
})()`;

const main = async () => {
  await connect();
  await send("Page.enable");
  await send("Runtime.enable");

  console.log(`signing in as ${EMAIL}`);
  await goto("/login");
  await typeInto("#email", EMAIL);
  await typeInto("#password", PASSWORD);
  await evaluate(`document.querySelector("form.auth-form, form")?.requestSubmit()`);
  await until("post-login redirect", "location.pathname !== '/login'", 25_000);

  const path = AS_CUSTOMER ? `/portal/services/${SERVICE}` : `/services/${SERVICE}`;
  console.log(`opening ${path}`);
  await goto(path);
  await until("the server card", `(() => ${cardTextExpr})().length > 20`, 30_000);

  const before = await panelState();
  console.log(`panel state before the click: ${before}`);
  await shot(`power-before-${ACTION}`);

  // A disabled control is worth failing on explicitly: it means the adapter did not offer the action for
  // the current state, which is a different problem from the click not working.
  const clicked = await evaluate(clickButtonExpr(ACTION));
  check(`the ${ACTION} button is present and enabled`, clicked === "clicked", `click returned ${clicked}`);

  // A confirmation dialog appears for restart and force-stop. Accepting it is part of the customer's path.
  if (clicked === "clicked") {
    const hadDialog = await evaluate(
      `!!document.querySelector('dialog[open], .dialog[open], [role="dialog"]')`,
    );
    if (hadDialog) {
      const confirmed = await evaluate(`(() => {
        const d = document.querySelector('dialog[open], .dialog[open], [role="dialog"]');
        const btn = [...(d?.querySelectorAll('button') ?? [])]
          .find((b) => /^(stop|restart|force stop|yes|confirm)$/i.test(b.innerText.trim()));
        if (!btn) return 'no confirm';
        btn.click();
        return 'confirmed';
      })()`);
      console.log(`  dialog confirmed: ${confirmed}`);
    }
  }

  // The effect, checked in the panel rather than in the page: the page could be lying or optimistic.
  //
  // The panel applies power changes asynchronously, so the wait is for the state to *settle* into the target,
  // not for the first sample after the click. Polling once and comparing is how this assertion lied the first
  // time it ran: the click had worked, and the very next read still said "stopped" because the panel had not
  // started the container yet.
  const wantState = { stop: "stopped", kill: "stopped", start: "running", restart: "running" }[ACTION] ?? null;
  let after = before;
  const deadline = Date.now() + 120_000;
  while (Date.now() < deadline) {
    after = await panelState();
    if (wantState ? after === wantState : after !== before) break;
    await new Promise((r) => setTimeout(r, 2500));
  }
  console.log(`panel state after the click:  ${after}`);
  check("the panel's state changed", after !== before, `still ${after}`);
  if (wantState) check(`the panel reports ${wantState}`, after === wantState, `reports ${after}`);

  // The card must agree, because it re-reads the provider after every action. It polls on its own every
  // 15 seconds, so the wait has to allow for that; clicking its Refresh first asserts the button works and
  // makes the check about the data rather than about the timer.
  await evaluate(`(() => {
    const btn = [...document.querySelectorAll('section.card button')].find((b) => b.innerText.trim() === 'Refresh');
    if (btn) btn.click();
    return !!btn;
  })()`);
  await until(
    "the card to show the new state",
    `(() => ${cardTextExpr})().toLowerCase().includes(${JSON.stringify(wantState ?? "running")})`,
    45_000,
  ).catch(() => {});
  const cardAfter = await evaluate(cardTextExpr);
  const buttonsAfter = await evaluate(`[...document.querySelectorAll('section.card button')].map((b) => b.innerText.trim())`);
  check("the card shows the new state", !wantState || cardAfter.toLowerCase().includes(wantState),
    cardAfter.slice(0, 200));
  // And its buttons follow: a stopped server offers start and not stop.
  if (wantState === "stopped") {
    check("the card now offers start", buttonsAfter.some((b) => /^start$/i.test(b)), JSON.stringify(buttonsAfter));
    check("the card no longer offers stop", !buttonsAfter.some((b) => /^stop$/i.test(b)), JSON.stringify(buttonsAfter));
  }
  if (wantState === "running") {
    check("the card now offers stop", buttonsAfter.some((b) => /^stop$/i.test(b)), JSON.stringify(buttonsAfter));
    check("the card no longer offers start", !buttonsAfter.some((b) => /^start$/i.test(b)), JSON.stringify(buttonsAfter));
  }

  await shot(`power-after-${ACTION}`);
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
