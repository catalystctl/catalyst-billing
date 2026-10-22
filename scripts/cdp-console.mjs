#!/usr/bin/env node
// A small CDP driver: opens a page, records console messages and exceptions, and prints them.
//
// This exists because `--dump-dom` shows the rendered result but not why a render did not
// happen. When the app leaves the boot screen in place, the answer is always in the console,
// and a headless browser that cannot show you the console turns a one-line error into an
// afternoon.
//
// Usage: node scripts/cdp-console.mjs <url> [--wait=ms] [--cookie=name=value] [--shot=file.png]

import { spawn } from "node:child_process";
import { writeFileSync } from "node:fs";

const args = process.argv.slice(2);
const url = args.find((a) => !a.startsWith("--"));
const flag = (name, fallback) => {
  const hit = args.find((a) => a.startsWith(`--${name}=`));
  return hit ? hit.slice(name.length + 3) : fallback;
};
if (!url) {
  console.error("usage: cdp-console.mjs <url> [--wait=ms] [--shot=file.png]");
  process.exit(2);
}
const waitMs = Number(flag("wait", "3000"));
const shot = flag("shot", "");
// --cookie=name=value, repeatable: a session cookie is HttpOnly, so a test that needs to be
// signed in cannot set it from page script and must inject it through the protocol.
const cookies = args.filter((a) => a.startsWith("--cookie=")).map((a) => a.slice("--cookie=".length));

const chrome = process.env.CHROME_BIN || "chromium";
const port = 9200 + Math.floor(Math.random() * 300);

// DBUS_SESSION_BUS_ADDRESS and QT_ACCESSIBILITY make a headless Chromium stall ~25s on its
// first navigation on this host, so they are stripped from the child environment.
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
    "--user-data-dir=/tmp/cdp-" + Date.now(),
    "about:blank",
  ],
  { env, stdio: ["ignore", "ignore", "pipe"] },
);

const deadline = Date.now() + 30_000;
let wsUrl = null;
while (Date.now() < deadline && !wsUrl) {
  try {
    const res = await fetch(`http://127.0.0.1:${port}/json/version`);
    const body = await res.json();
    wsUrl = body.webSocketDebuggerUrl;
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
const targetId = targetResult.targetId;
const { result: attachResult } = await send("Target.attachToTarget", { targetId, flatten: true });
const sessionId = attachResult.sessionId;

await send("Runtime.enable", {}, sessionId);
await send("Log.enable", {}, sessionId);
await send("Page.enable", {}, sessionId);
await send("Network.enable", {}, sessionId);

for (const pair of cookies) {
  const index = pair.indexOf("=");
  const name = pair.slice(0, index);
  const value = pair.slice(index + 1);
  const { hostname } = new URL(url);
  await send(
    "Network.setCookie",
    { name, value, domain: hostname, path: "/", httpOnly: true, secure: true, sameSite: "Lax" },
    sessionId,
  );
}

await send("Page.navigate", { url }, sessionId);
await new Promise((r) => setTimeout(r, waitMs));

const messages = [];
for (const ev of events) {
  if (ev.method === "Runtime.consoleAPICalled") {
    const text = (ev.params.args ?? [])
      .map((a) => a.value ?? a.description ?? a.unserializableValue ?? "")
      .join(" ");
    messages.push(`[console.${ev.params.type}] ${text}`);
  }
  if (ev.method === "Runtime.exceptionThrown") {
    const d = ev.params.exceptionDetails;
    const stack = (d.stackTrace?.callFrames ?? [])
      .slice(0, 4)
      .map((f) => `    at ${f.functionName || "<anonymous>"} ${f.url}:${f.lineNumber + 1}:${f.columnNumber + 1}`)
      .join("\n");
    messages.push(`[exception] ${d.exception?.description ?? d.text}\n${stack}`);
  }
  if (ev.method === "Log.entryAdded") {
    const e = ev.params.entry;
    if (e.level === "error" || e.level === "warning") {
      messages.push(`[log.${e.level}] ${e.text} ${e.url ?? ""}`);
    }
  }
  if (ev.method === "Network.loadingFailed") {
    messages.push(`[network.failed] ${ev.params.errorText} ${ev.params.type}`);
  }
}

if (shot) {
  const { result } = await send("Page.captureScreenshot", { format: "png" }, sessionId);
  if (result?.data) writeFileSync(shot, Buffer.from(result.data, "base64"));
}

console.log(messages.length > 0 ? messages.join("\n") : "(no console output)");

ws.close();
child.kill("SIGKILL");
process.exit(0);
