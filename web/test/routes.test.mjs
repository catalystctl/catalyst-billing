// Every internal link must point at a route that exists.
//
// This is the test that would have caught the bug it was written for. When the catalog gained
// write screens, the old read-only pages stayed routed while the API response shape changed
// underneath them: `/catalog/:id` still loaded, still linked from services and orders, and threw
// "Cannot read properties of undefined (reading 'name')" because it expected `{product: ...}`
// and received the product itself. The new screens passed every test because the tests opened
// the new URLs directly. Nothing checked that the links a user actually clicks lead somewhere
// that works.
//
// The check is static on purpose: it reads the route table and the href literals out of the
// source, so it needs no DOM and no server, and it fails in milliseconds.

import assert from "node:assert/strict";
import { readFileSync, readdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const src = join(root, "src");

function readTree(dir) {
  const out = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = join(dir, entry.name);
    if (entry.isDirectory()) out.push(...readTree(full));
    else if (entry.name.endsWith(".ts")) out.push({ path: full, body: readFileSync(full, "utf8") });
  }
  return out;
}

const files = readTree(src);

/**
 * Paths the Go server answers directly. These are linked with data-native="true" so the router
 * leaves them alone, and they are not SPA routes.
 */
const SERVER_PATHS = new Set(["/healthz", "/readyz", "/metrics", "/api/v1/catalog"]);

/**
 * Files in views/ that are not pages. They export a component another page composes, so no
 * route imports them and that is correct. Listing them explicitly is the point: a new shared
 * component has to be added here, which is a moment to ask whether it is really shared or
 * whether a route was forgotten.
 */
// Views that are reached without being named by a route entry.
//
// `not-found` is imported by the router itself as the fallback for a path no route matches, so it
// has no `route(...)` line and never will. `billing-profile` and `refund` are rendered inside
// other screens. Anything else in this set is suspicious: a view nobody routes to is either dead
// code or a route somebody forgot to add, which is why the test exists.
const SHARED_VIEW_MODULES = new Set(["billing-profile", "refund", "not-found"]);

/**
 * The module that declares the route table.
 *
 * It is `routes.ts`, not `app.ts`. The table moved there when it outgrew the bootstrap file, and
 * these tests kept reading `app.ts` — where the `route(...)` calls no longer are, so they found
 * zero patterns and asserted on nothing. That is the worst state for a guard to be in: green
 * because it stopped looking.
 */
function routesModule() {
  const mod = files.find((f) => f.path.endsWith("/routes.ts"));
  assert.ok(mod, "routes.ts not found: the route table moved again, so these tests need updating");
  return mod;
}

/** Route patterns from routes.ts: route("/customers/:id", ...) */
function routePatterns() {
  const app = routesModule();
  const patterns = [];
  for (const match of app.body.matchAll(/route\("([^"]+)"/g)) {
    patterns.push(match[1]);
  }
  assert.ok(patterns.length > 20, `expected the route table, found ${patterns.length} patterns`);
  return patterns;
}

/** Splits a path into segments, ignoring query and hash. */
function segments(path) {
  return path.split("?")[0].split("#")[0].split("/").filter(Boolean);
}

/** True when a concrete path matches a route pattern with :params. */
function matches(pattern, path) {
  const p = segments(pattern);
  const s = segments(path);
  if (p.length !== s.length) return false;
  return p.every((seg, i) => seg.startsWith(":") || seg === s[i]);
}

/**
 * Internal hrefs written as literals. Template literals with an interpolation are resolved to
 * their static prefix plus a placeholder, so `/customers/${id}` becomes `/customers/:x` and can
 * still be matched against `/customers/:id`.
 */
function hrefs() {
  const found = [];
  for (const file of files) {
    for (const match of file.body.matchAll(/href:\s*[`"']([^`"']+)[`"']/g)) {
      const raw = match[1];
      if (!raw.startsWith("/")) continue; // external or relative
      if (raw.startsWith("//")) continue;
      if (raw.startsWith("/api/")) continue; // an API path, not a route
      if (SERVER_PATHS.has(raw)) continue; // served by Go, not by the SPA router
      found.push({ href: raw, file: file.path.replace(root + "/", "") });
    }
    // href: `/customers/${id}` — the interpolation is one or more segments.
    for (const match of file.body.matchAll(/href:\s*`([^`]+)`/g)) {
      const raw = match[1];
      if (!raw.startsWith("/")) continue;
      if (raw.startsWith("/api/")) continue;
      const normalized = raw.replace(/\$\{[^}]+\}/g, ":x");
      if (SERVER_PATHS.has(normalized)) continue;
      found.push({ href: normalized, file: file.path.replace(root + "/", "") });
    }
  }
  return found;
}

test("every internal href resolves to a declared route", () => {
  const patterns = routePatterns();
  const missing = [];
  for (const { href, file } of hrefs()) {
    if (href.startsWith("/api/")) continue;
    if (patterns.some((pattern) => matches(pattern, href))) continue;
    // A bare "#anchor" link is not a route.
    if (href.startsWith("#")) continue;
    missing.push(`${href}  (in ${file})`);
  }
  assert.deepEqual(missing, [], `these links point at no route:\n  ${missing.join("\n  ")}`);
});

test("every route module exists", () => {
  const app = routesModule();
  const missing = [];
  for (const match of app.body.matchAll(/import\("\.\/views\/([^"]+)"\)/g)) {
    const name = match[1];
    const candidates = [`${name}.ts`, `${name}/index.ts`];
    const exists = files.some((f) => candidates.some((c) => f.path.endsWith(`/views/${c}`)));
    if (!exists) missing.push(name);
  }
  assert.deepEqual(missing, [], `routes import modules that do not exist: ${missing.join(", ")}`);
});

test("no view file is orphaned", () => {
  // A view nobody routes to is either dead code or a route somebody forgot to add. Both are
  // worth failing on: the catalog's old screens sat unrouted-but-linked for exactly this reason.
  const app = routesModule();
  const imported = new Set([...app.body.matchAll(/import\("\.\/views\/([^"]+)"\)/g)].map((m) => m[1]));
  const views = files
    .filter((f) => f.path.includes("/views/"))
    .map((f) => f.path.split("/views/")[1].replace(/\.ts$/, ""));
  const orphans = views.filter((v) => !imported.has(v) && !SHARED_VIEW_MODULES.has(v));
  assert.deepEqual(orphans, [], `views that no route imports: ${orphans.join(", ")}`);
});
