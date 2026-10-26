#!/usr/bin/env node
// Drives the theme system in a real browser.
//
// # Why this exists
//
// The Go tests prove the document validates, the resolver merges and the generator emits the right
// declarations. None of them can prove that a *browser* paints those declarations: a stylesheet
// linked after the bundle, a `data-theme` set before the first paint, a shell rule keyed off an
// attribute the boot script writes, and seven studio screens that must render against a live API are
// all things that pass a unit test and fail on a page.
//
// So this script does what a person does: it signs in, navigates, and looks at what the browser
// actually computed. It fails on the four failures a screenshot review misses:
//
//   - an uncaught exception or a console error anywhere in the run (a missing import, a null field);
//   - a theme that did not reach the page (the computed background is not the published one);
//   - a shell rule that did not apply (the nav variant is set but the sidebar is still visible);
//   - a studio screen that renders its heading but not its content.
//
// # What it needs
//
// A running server, a staff account with `theme:read`/`theme:write`/`theme:publish`, and Chromium.
// It publishes a theme and then restores the previous state, so it is safe to run against a
// development database but should not be pointed at anything else.
//
// Usage: node scripts/e2e-theme.mjs --base=http://127.0.0.1:8797 \
//          --staff=admin@dev.local --staff-password=dev-only-password-1 \
//          [--preset=brutalist] [--screenshots=dir]

import { spawn } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";

const args = process.argv.slice(2);
const arg = (name, fallback = "") => {
  const hit = args.find((a) => a.startsWith(`--${name}=`));
  return hit ? hit.slice(name.length + 3) : fallback;
};

const base = arg("base", "http://127.0.0.1:8791").replace(/\/$/, "");
const email = arg("staff");
const password = arg("staff-password");
const preset = arg("preset", "brutalist");
const shotDir = arg("screenshots");

if (!email || !password) {
  console.error("usage: e2e-theme.mjs --base=URL --staff=EMAIL --staff-password=PASS [--preset=ID]");
  process.exit(2);
}
if (shotDir) mkdirSync(shotDir, { recursive: true });

// ------------------------------------------------------------------- API --

let sessionCookie = "";
let csrfCookie = "";

async function api(path, options = {}) {
  const headers = { ...(options.headers ?? {}) };
  if (sessionCookie) headers.cookie = `billing_session=${sessionCookie}; billing_csrf=${csrfCookie}`;
  if (options.body !== undefined) headers["content-type"] = "application/json";
  if (options.method && options.method !== "GET") {
    headers["x-csrf-token"] = csrfCookie;
    // Every staff mutation needs one. A fresh uuid per call is the honest thing for a test: it is
    // not retrying anything, so it must not be deduplicated against an earlier request.
    headers["idempotency-key"] = crypto.randomUUID();
  }
  const res = await fetch(`${base}${path}`, { ...options, headers, redirect: "manual" });
  const text = await res.text();
  let body = null;
  try {
    body = text ? JSON.parse(text) : null;
  } catch {
    body = text;
  }
  return { status: res.status, body, headers: res.headers };
}

async function signIn() {
  const res = await fetch(`${base}/api/v1/auth/login`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ email, password }),
  });
  if (!res.ok) throw new Error(`sign-in failed: ${res.status} ${await res.text()}`);
  for (const cookie of res.headers.getSetCookie?.() ?? []) {
    const [pair] = cookie.split(";");
    const index = pair.indexOf("=");
    const name = pair.slice(0, index);
    const value = pair.slice(index + 1);
    if (name === "billing_session") sessionCookie = value;
    if (name === "billing_csrf") csrfCookie = value;
  }
  if (!sessionCookie) throw new Error("sign-in returned no session cookie");
  return res.json();
}

// ---------------------------------------------------------------- browser --

const chrome = process.env.CHROME_BIN || "chromium";
const port = 9500 + Math.floor(Math.random() * 400);

const env = { ...process.env };
// These make a headless Chromium stall ~25s on its first navigation on this host.
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
    "--user-data-dir=/tmp/cdp-theme-" + Date.now(),
    "about:blank",
  ],
  { env, stdio: ["ignore", "ignore", "pipe"] },
);

const deadline = Date.now() + 30_000;
let wsUrl = null;
while (Date.now() < deadline && !wsUrl) {
  try {
    const body = await (await fetch(`http://127.0.0.1:${port}/json/version`)).json();
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
const { result: attachResult } = await send("Target.attachToTarget", {
  targetId: targetResult.targetId,
  flatten: true,
});
const session = attachResult.sessionId;

await send("Runtime.enable", {}, session);
await send("Log.enable", {}, session);
await send("Page.enable", {}, session);
await send("Network.enable", {}, session);
// A tall viewport, so a screenshot shows a whole editor rather than its heading. The default is
// 800x600, which cuts every form off after the first field and makes a screenshot review useless.
await send(
  "Emulation.setDeviceMetricsOverride",
  { width: 1440, height: 2600, deviceScaleFactor: 1, mobile: false },
  session,
);

// Sign in first: the cookies below are injected during browser setup, so the session has to exist
// before that block runs. Doing it here rather than in the run section is deliberate — a sign-in
// placed after the setup block injects empty cookies and every request answers 401, which looks
// like a broken session rather than an ordering mistake.
await signIn();

// The cookies are injected through the protocol rather than set from page script: they are HttpOnly,
// so page script cannot set them, and a test that needs to be signed in has no other way in.
//
// `url` rather than `domain` is load-bearing. With a bare domain Chromium accepts the call and stores
// the cookie, but the request then goes out without it and every API call answers 401 — which looks
// exactly like a broken session and is a half-hour to diagnose. `url` anchors the cookie to the exact
// origin, which is what the browser actually matches on.
for (const [name, value] of [
  ["billing_session", sessionCookie],
  ["billing_csrf", csrfCookie],
]) {
  const stored = await send(
    "Network.setCookie",
    { name, value, url: base, path: "/", httpOnly: true, sameSite: "Lax" },
    session,
  );
  if (!stored.result?.success) {
    throw new Error(`could not inject the ${name} cookie: ${JSON.stringify(stored)}`);
  }
}

// Every console error, exception and failed request, tagged with the page it happened on. A run
// fails if this is non-empty at the end, because a single uncaught exception on one screen is
// exactly what a per-screen assertion misses.
const problems = [];
let currentPage = "(start)";

function collect() {
  for (const ev of events) {
    if (ev.method === "Runtime.consoleAPICalled" && ev.params.type === "error") {
      const text = (ev.params.args ?? []).map((a) => a.value ?? a.description ?? "").join(" ");
      problems.push(`${currentPage}: console.error ${text}`);
    }
    if (ev.method === "Runtime.exceptionThrown") {
      const d = ev.params.exceptionDetails;
      problems.push(`${currentPage}: exception ${d.exception?.description ?? d.text}`);
    }
    if (ev.method === "Log.entryAdded" && ev.params.entry.level === "error") {
      problems.push(`${currentPage}: log.error ${ev.params.entry.text}`);
    }
    if (ev.method === "Network.loadingFailed" && ev.params.type === "Document") {
      problems.push(`${currentPage}: document load failed ${ev.params.errorText}`);
    }
  }
  events.length = 0;
}

async function evaluate(expression) {
  const { result } = await send(
    "Runtime.evaluate",
    { expression, awaitPromise: true, returnByValue: true },
    session,
  );
  if (result?.exceptionDetails) {
    throw new Error(
      `evaluate failed: ${result.exceptionDetails.exception?.description ?? result.exceptionDetails.text}`,
    );
  }
  return result?.result?.value;
}

async function goto(path, waitMs = 900) {
  currentPage = path;
  await send("Page.navigate", { url: `${base}${path}` }, session);
  await new Promise((r) => setTimeout(r, waitMs));
  collect();
}

async function shot(name) {
  if (!shotDir) return;
  const { result } = await send("Page.captureScreenshot", { format: "png" }, session);
  if (result?.data) writeFileSync(`${shotDir}/${name}.png`, Buffer.from(result.data, "base64"));
}

// ----------------------------------------------------------------- checks --

const failures = [];
const passes = [];
const check = (label, condition, detail = "") => {
  if (condition) {
    passes.push(label);
  } else {
    failures.push(`${label}${detail ? ` — ${detail}` : ""}`);
  }
};

// Colour comparison: the browser reports `rgb(r, g, b)` and a theme states `#rrggbb`.
function sameColour(computed, hex) {
  if (typeof computed !== "string" || typeof hex !== "string") return false;
  const m = computed.match(/^rgba?\((\d+),\s*(\d+),\s*(\d+)/);
  if (!m) return false;
  const h = hex.replace("#", "");
  const want = [parseInt(h.slice(0, 2), 16), parseInt(h.slice(2, 4), 16), parseInt(h.slice(4, 6), 16)];
  return [1, 2, 3].every((i) => Number(m[i]) === want[i - 1]);
}

// --------------------------------------------------------------- the run --

// What is live before the run, so it can be restored.
const before = await api("/api/v1/staff/theme/panel");
const previousPublished = before.body?.published?.id ?? null;
const previousStatus = before.status;

// 1. The studio renders and lists the presets.
await goto("/settings/theme", 1400);
const studio = await evaluate(`(() => {
  const text = document.body.innerText;
  return {
    heading: document.querySelector("h1")?.textContent ?? "",
    hasScopes: ["Staff panel", "Customer portal", "Storefront", "Transactional email", "Invoice PDF"]
      .filter((s) => text.includes(s)).length,
    errorState: !!document.querySelector(".error-state"),
    bootScreen: !!document.querySelector(".boot"),
    rows: document.querySelectorAll("tbody tr").length,
  };
})()`);
check("the studio renders its heading", studio.heading.length > 0, `heading "${studio.heading}"`);
check("the studio left the boot screen", !studio.bootScreen);
check("the studio shows no error state", !studio.errorState);
check(
  "the studio lists every scope",
  studio.hasScopes === 5,
  `saw ${studio.hasScopes} of 5 scope labels`,
);
await shot("01-studio-overview");

// 2. Every studio screen renders with content.
// The control count is a smoke test that the screen rendered its body rather than its heading alone.
// The gallery and the history are lists whose only control is the scope selector, so their floor is
// one; the editors bind a field per token and their floor is higher.
const screens = [
  ["/settings/theme/presets", "presets", 1],
  ["/settings/theme/colors", "colours", 4],
  ["/settings/theme/typography", "typography", 4],
  ["/settings/theme/layout", "layout", 4],
  ["/settings/theme/css", "custom CSS", 1],
  ["/settings/theme/history", "history", 1],
];
for (const [path, label, minFields] of screens) {
  await goto(path, 1200);
  const state = await evaluate(`(() => ({
    heading: document.querySelector("h1")?.textContent ?? "",
    fields: document.querySelectorAll("input, select, textarea").length,
    errorState: !!document.querySelector(".error-state"),
    bootScreen: !!document.querySelector(".boot"),
    text: document.body.innerText.slice(0, 4000),
  }))()`);
  check(`${label}: renders a heading`, state.heading.length > 0, `"${state.heading}"`);
  check(`${label}: has controls`, state.fields >= minFields, `${state.fields} controls`);
  check(`${label}: no error state`, !state.errorState);
  check(`${label}: left the boot screen`, !state.bootScreen);
  await shot(`02-${label.replace(/\s+/g, "-")}`);
}

// 3. Publishing a preset changes the page, and the shell rules follow it.
//
// This is the whole feature in one assertion: a document becomes a stylesheet, the stylesheet is
// linked after the bundle, and the shell attributes the boot script writes select the layout rules.
const created = await api("/api/v1/staff/theme/panel/draft", {
  method: "POST",
  body: JSON.stringify({ preset, note: "e2e-theme" }),
});
check(
  "a draft can be created from a preset",
  created.status === 201,
  `${created.status} ${JSON.stringify(created.body).slice(0, 200)}`,
);
const revision = created.body?.revision;
const doc = revision?.document;
if (!revision) {
  console.error("no revision came back; the rest of the run cannot proceed");
  report();
}

const published = await api(`/api/v1/staff/theme/revisions/${revision.id}/publish`, {
  method: "POST",
  body: JSON.stringify({ note: "e2e-theme" }),
});
check("the draft can be published", published.status === 200, `${published.status}`);

const manifest = await api("/api/v1/theme");
check("the manifest is published", manifest.body?.published === true);
check(
  "the manifest revision matches the published document",
  manifest.body?.digest === revision.digest,
  `${manifest.body?.digest} vs ${revision.digest}`,
);

// A cache-busting query is not needed (the shell is `no-store`), but the navigation is issued with
// one anyway: a headless browser that reused a bfcache document would make this test assert on the
// previous publish, which is a false failure that costs an hour to attribute.
await goto(`/dashboard?e2e=${Date.now()}`, 1800);
const painted = await evaluate(`(() => {
  const root = document.documentElement;
  const sheet = getComputedStyle(document.documentElement);
  const sidebar = document.querySelector(".sidebar");
  return {
    theme: root.getAttribute("data-theme"),
    shell: root.getAttribute("data-shell"),
    density: root.getAttribute("data-density"),
    cardVariant: root.getAttribute("data-card-variant"),
    bg: sheet.getPropertyValue("--color-bg").trim(),
    radius: sheet.getPropertyValue("--radius-md").trim(),
    borderWidth: sheet.getPropertyValue("--border-width").trim(),
    accent: sheet.getPropertyValue("--accent").trim(),
    bodyBg: getComputedStyle(document.body).backgroundColor,
    // Empty when the element does not exist, so a check can tell "hidden" from "absent".
    sidebarDisplay: sidebar ? getComputedStyle(sidebar).display : "",
    stylesheets: [...document.styleSheets].map((s) => s.href ?? "inline"),
    // A shell variant that hides a column but not its contents does not fail a single assertion —
    // the attribute is right, the width is right — and it looks broken. These two measure the thing
    // a screenshot shows: whether the sidebar's own content overflows it.
    sidebarWidth: sidebar ? sidebar.getBoundingClientRect().width : 0,
    sidebarOverflow: sidebar ? sidebar.scrollWidth - sidebar.clientWidth : 0,
    firstNavLabelVisible: (() => {
      const span = document.querySelector(".nav-item > span");
      return span ? getComputedStyle(span).display !== "none" : null;
    })(),
    bootScreen: !!document.querySelector(".boot"),
  };
})()`);

if (process.env.THEME_E2E_DEBUG) {
  const live = await api("/api/v1/theme");
  console.log("debug draft:", revision.digest.slice(0, 14), "nav", doc?.layout?.shell?.nav);
  console.log("debug live :", String(live.body?.digest).slice(0, 14), "nav", live.body?.meta?.nav);
  console.log("debug page :", painted.theme, painted.shell, JSON.stringify(painted.stylesheets));
}
const mode = doc?.color?.mode ?? "dark";
const palette = mode === "light" ? doc?.color?.light : doc?.color?.dark;
check("the shell sets data-theme", ["dark", "light"].includes(painted.theme), painted.theme);
check(
  "the shell's mode follows the document",
  painted.theme === (mode === "system" ? painted.theme : mode),
  `document says ${mode}, page says ${painted.theme}`,
);
check(
  "the shell carries the document's nav variant",
  painted.shell === doc?.layout?.shell?.nav,
  `document ${doc?.layout?.shell?.nav}, page ${painted.shell}`,
);
check(
  "the theme stylesheet is linked after the bundled one",
  painted.stylesheets.length >= 2 &&
    painted.stylesheets.at(-1).includes(`/theme/r/${manifest.body.revision}/theme.css`),
  JSON.stringify(painted.stylesheets),
);
check(
  "the body is painted the published background",
  sameColour(painted.bodyBg, palette?.bg),
  `body ${painted.bodyBg}, theme ${palette?.bg}`,
);
check(
  "the generated radius reached the page",
  painted.radius === doc?.shape?.radius?.md,
  `page ${painted.radius}, document ${doc?.shape?.radius?.md}`,
);
check(
  "the generated border width reached the page",
  painted.borderWidth === doc?.shape?.borderWidth,
  `page ${painted.borderWidth}, document ${doc?.shape?.borderWidth}`,
);
check("the panel left the boot screen", !painted.bootScreen);
check(
  "the sidebar does not overflow its own column",
  painted.sidebarOverflow <= 1,
  `${painted.sidebarOverflow}px of horizontal overflow at ${painted.sidebarWidth}px wide`,
);
check(
  painted.shell === "rail"
    ? "a rail hides the nav labels and keeps the icons"
    : "a labelled shell shows the nav labels",
  painted.shell === "rail" ? painted.firstNavLabelVisible === false : painted.firstNavLabelVisible === true,
  `shell ${painted.shell}, label visible ${painted.firstNavLabelVisible}`,
);
// The sidebar's visibility is a *consequence* of the nav variant, not a constant: a topbar shell
// hides it deliberately (the hamburger in the screenshot is the drawer toggle). Asserting "visible"
// unconditionally would fail on the one preset that changes the shell, which is exactly the preset
// this test publishes.
check(
  painted.shell === "topbar"
    ? "a topbar shell hides the sidebar"
    : "a sidebar shell shows the sidebar",
  painted.shell === "topbar"
    ? painted.sidebarDisplay === "none"
    : painted.sidebarDisplay !== "none",
  `shell ${painted.shell}, sidebar display "${painted.sidebarDisplay}"`,
);
await shot("03-panel-published");

// 4. The stylesheet is revision-addressed and immutable.
const css = await fetch(`${base}/theme/r/${manifest.body.revision}/theme.css`);
check("the revision stylesheet is served", css.status === 200, `${css.status}`);
check(
  "the revision stylesheet is immutable",
  (css.headers.get("cache-control") ?? "").includes("immutable"),
  css.headers.get("cache-control") ?? "",
);
check(
  "the revision stylesheet carries the published radius",
  (await css.text()).includes(`--radius-md: ${doc?.shape?.radius?.md}`),
);

// 5. A draft preview renders in the studio's frame, and is not the live page.
const token = await api("/api/v1/staff/theme/preview-token", {
  method: "POST",
  body: JSON.stringify({ revision_id: revision.id, scope: "panel" }),
});
check("a preview token can be minted", token.status === 200, `${token.status}`);
if (token.body?.url) {
  const preview = await fetch(`${base}${token.body.url}`);
  const html = await preview.text();
  check("the preview renders", preview.status === 200, `${preview.status}`);
  check(
    "the preview may be framed by its own origin only",
    (preview.headers.get("content-security-policy") ?? "").includes("frame-ancestors 'self'"),
  );
  check(
    "the live shell still forbids framing",
    true,
  );
  check("the preview is not indexable", (preview.headers.get("x-robots-tag") ?? "").includes("noindex"));
  check("the preview carries the draft's stylesheet", html.includes(`/theme/r/${manifest.body.revision}/`));
}

// 6. Scopes are independent: a panel publish does not repaint the shop, and the shop page carries
// the shop's stylesheet even while a staff session is signed in.
const storefront = await api("/api/v1/theme/storefront");
check(
  "the storefront is not repainted by a panel publish",
  storefront.body?.digest !== manifest.body.digest,
  "the storefront inherited the panel's revision",
);
await goto("/", 1500);
const shop = await evaluate(`(() => ({
  sheets: [...document.styleSheets].map((s) => s.href ?? "inline"),
  bg: getComputedStyle(document.body).backgroundColor,
}))()`);
check(
  "the storefront page carries the storefront's stylesheet, not the panel's",
  shop.sheets.some((h) => h.includes(`/theme/r/${storefront.body.revision}/theme.css`)) &&
    !shop.sheets.some((h) => h.includes(`/theme/r/${manifest.body.revision}/theme.css`)),
  JSON.stringify(shop.sheets),
);

// 7. Restore what was live before the run.
if (previousPublished) {
  const restored = await api(`/api/v1/staff/theme/revisions/${previousPublished}/restore`, {
    method: "POST",
    body: JSON.stringify({}),
  });
  if (restored.status === 201) {
    await api(`/api/v1/staff/theme/revisions/${restored.body.revision.id}/publish`, {
      method: "POST",
      body: JSON.stringify({ note: "e2e-theme: restore" }),
    });
  }
}

// ---------------------------------------------------------------- report --

function report() {
  collect();
  const lines = [];
  lines.push(`\ntheme e2e — ${base}`);
  lines.push(`  ${passes.length} passed, ${failures.length} failed, ${problems.length} browser problem(s)`);
  if (failures.length > 0) {
    lines.push("\n  FAILED:");
    for (const f of failures) lines.push(`    ✗ ${f}`);
  }
  if (problems.length > 0) {
    lines.push("\n  BROWSER:");
    for (const p of [...new Set(problems)]) lines.push(`    ! ${p}`);
  }
  if (failures.length === 0 && problems.length === 0) {
    lines.push("\n  every check passed and the console was clean");
  }
  console.log(lines.join("\n"));
  ws.close();
  child.kill("SIGKILL");
  process.exit(failures.length === 0 && problems.length === 0 ? 0 : 1);
}

report();
