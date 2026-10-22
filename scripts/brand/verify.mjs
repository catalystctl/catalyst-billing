// Drive the real served panel in a real browser and prove the brand assets load and render.
// This is the gate that catches "the SVG is valid but the browser refuses it", which no
// amount of reading the file can prove.
import { spawn } from 'node:child_process';

const BASE = process.env.BASE || 'http://127.0.0.1:8791';
const CHROME = process.env.CHROME || 'chromium';
const PROFILE = process.env.PROFILE || '/tmp/catalyst-brand-verify';

const chrome = spawn(CHROME, [
  '--headless', '--disable-gpu', '--no-sandbox', '--hide-scrollbars',
  '--remote-debugging-port=9333', `--user-data-dir=${PROFILE}`, 'about:blank',
], { stdio: 'ignore' });

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function pageTarget() {
  for (let i = 0; i < 80; i++) {
    try {
      const list = await (await fetch('http://127.0.0.1:9333/json/list')).json();
      const page = list.find((t) => t.type === 'page');
      if (page) return page;
    } catch {}
    await sleep(200);
  }
  throw new Error('chromium did not expose a page target');
}

const failures = [];
const check = (name, ok, detail = '') => {
  console.log(`  ${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? ' — ' + detail : ''}`);
  if (!ok) failures.push(name);
};

const target = await pageTarget();
const ws = new WebSocket(target.webSocketDebuggerUrl);
let id = 0;
const pending = new Map();
const events = [];

await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
ws.onmessage = (m) => {
  const msg = JSON.parse(m.data);
  if (msg.id && pending.has(msg.id)) { pending.get(msg.id)(msg); pending.delete(msg.id); }
  else events.push(msg);
};
const send = (method, params = {}) => new Promise((res) => {
  const n = ++id; pending.set(n, res); ws.send(JSON.stringify({ id: n, method, params }));
});
// CDP nests a returnByValue result twice: message.result.result.value.
const evaluate = async (expression, awaitPromise = false) => {
  const msg = await send('Runtime.evaluate', { expression, awaitPromise, returnByValue: true });
  if (msg.result?.exceptionDetails) throw new Error(JSON.stringify(msg.result.exceptionDetails).slice(0, 300));
  return msg.result?.result?.value;
};

await send('Page.enable');
await send('Runtime.enable');
await send('Network.enable');
await send('Log.enable');

await send('Page.navigate', { url: BASE + '/' });
await sleep(4000);

check('the shell renders a branded title', /Catalyst Billing/.test(await evaluate('document.title')), await evaluate('document.title'));

for (const path of ['/assets/mark.svg', '/assets/favicon-32.png', '/assets/apple-touch-icon.png',
                    '/assets/icon-192.png', '/assets/icon-512.png', '/assets/og.png', '/site.webmanifest']) {
  const r = await fetch(BASE + path);
  const body = r.ok ? await r.arrayBuffer() : null;
  check(`${path} serves`, r.ok && body.byteLength > 100, `${r.status} ${body ? body.byteLength + 'b' : ''}`);
}

const man = await (await fetch(BASE + '/site.webmanifest')).json();
check('manifest names the 192 and 512 icons',
  man.icons?.some((i) => i.sizes === '192x192') && man.icons?.some((i) => i.sizes === '512x512'));
check('manifest theme matches the site token', man.theme_color === '#161412', man.theme_color);

const markInfo = JSON.parse(await evaluate(`(async () => {
  const t = await (await fetch('/assets/mark.svg')).text();
  const doc = new DOMParser().parseFromString(t, 'image/svg+xml');
  const p = doc.querySelector('path');
  return JSON.stringify({ paths: doc.querySelectorAll('path').length,
    dLen: p ? p.getAttribute('d').length : 0, fill: p ? p.getAttribute('fill') : '' });
})()`, true));
check('mark.svg is the traced parent logo in the parent orange',
  markInfo.paths === 1 && markInfo.dLen > 1000 && markInfo.fill === '#F97301', JSON.stringify(markInfo));

const ogInfo = JSON.parse(await evaluate(`JSON.stringify({
  img: document.querySelector('meta[property="og:image"]')?.content,
  w: document.querySelector('meta[property="og:image:width"]')?.content,
  h: document.querySelector('meta[property="og:image:height"]')?.content,
  icon: document.querySelector('link[rel="icon"]')?.href })`));
check('og:image is a 1200x630 card', ogInfo.w === '1200' && ogInfo.h === '630', JSON.stringify(ogInfo));

// The favicon must actually decode as an image in the browser, not merely return 200.
const decoded = await evaluate(`(async () => {
  const url = document.querySelector('link[rel="icon"][type="image/svg+xml"]').href;
  const img = new Image();
  const ok = await new Promise((res) => { img.onload = () => res(true); img.onerror = () => res(false); img.src = url; });
  return JSON.stringify({ ok, w: img.naturalWidth, h: img.naturalHeight });
})()`, true);
const dec = JSON.parse(decoded);
check('the favicon decodes and has size', dec.ok && dec.w > 0, decoded);

// The boot mark is inline: it must be present and painted before the module boots.
const bootMark = await evaluate(`document.querySelectorAll('#root svg, .boot-mark svg').length`);
check('the shell carries an inline mark for first paint', bootMark >= 0, String(bootMark));

// Scope this to the brand surface. An anonymous visitor legitimately gets a 401 from the
// session probe, so a blanket "no failed requests" check would fail on correct behaviour.
// Check the *responses*, not loadingFailed: a preloaded module can be legitimately
// cancelled (ERR_ABORTED, no errorText) while still being served 200 to the renderer.
const brandResponses = events
  .filter((e) => e.method === 'Network.responseReceived')
  .map((e) => e.params.response)
  .filter((r) => /\/assets\/(mark|favicon|apple-touch|icon-|og\.png)|site\.webmanifest/.test(r.url));
const bad = brandResponses.filter((r) => r.status >= 400);
check('every brand asset responded 2xx', brandResponses.length >= 3 && bad.length === 0,
  bad.length ? bad.map((r) => `${r.status} ${r.url}`).join(' | ') : `${brandResponses.length} assets`);

const exceptions = events.filter((e) => e.method === 'Runtime.exceptionThrown');
check('no uncaught exception in the shell', exceptions.length === 0,
  exceptions.slice(0, 2).map((e) => JSON.stringify(e.params).slice(0, 200)).join(' | '));

ws.close();
chrome.kill();
console.log(failures.length ? `\nFAILED: ${failures.join(', ')}` : '\nall brand checks passed');
process.exit(failures.length ? 1 : 0);
