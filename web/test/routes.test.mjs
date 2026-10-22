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

// ---------------------------------------------------------------- reachability --
//
// The tests above prove that a link resolves. They cannot prove that a route can be *arrived
// at*: a screen with no navigation entry and no link is a screen only its author can open, and
// every href test passes because there is no href to check.
//
// These are the static half of scripts/reachability.sh. That script diffs the served route
// table (built from the router) against this source tree, which needs Go and is the
// authoritative sweep; these tests need only Node, so they run on every `npm test` and fail on
// the classes of gap that do not need the server to see.

/** A source file with its comments removed.
 *
 * Comments in this codebase quote paths and permissions on purpose — the explanation of why a
 * route exists names it — and scanning them would report the documentation as a call site.
 * Block comments and whole-line `//` comments are removed; a trailing `//` is not, because
 * `https://` in a string would be mangled by the naive rule. */
function uncommented(body) {
  return body.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*\/\/.*$/gm, "");
}

/** Constants declared in one file, so `${ENDPOINT}` resolves to that file's value.
 *
 * Per file, not global: four views declare `const ENDPOINT` with four different values, and a
 * shared map resolved all of them to whichever file was read last. */
function constsOf(body) {
  const out = {};
  for (const match of body.matchAll(
    /(?:export\s+)?const\s+([A-Z][A-Z0-9_]*)\s*(?::\s*string\s*)?=\s*[`"']([^`"']*)[`"']/g,
  )) {
    out[match[1]] = match[2];
  }
  return out;
}

/** A link or API path reduced to a pattern: query dropped, interpolation → {x}. */
function patternize(raw, consts) {
  return raw
    .split("?")[0]
    .split("#")[0]
    .replace(/\$\{([A-Za-z_][A-Za-z0-9_]*)\}/g, (_, name) => consts[name] ?? "{x}")
    .replace(/\$\{[^}]*\}/g, "{x}");
}

/**
 * True when `link` can be an instance of `pattern`.
 *
 * A parameter matches anything on either side: a view that dispatches
 * `/staff/services/${id}/${action}` covers `/resume` as surely as `/suspend`, and this test
 * cannot know which. A wildcard may not stand alone, though: app.ts builds a breadcrumb href
 * as `` `/${segment}` ``, and without the shared-literal rule that single link made every
 * one-segment route look reachable.
 */
function covers(pattern, link) {
  const p = segments(pattern);
  const s = segments(link);
  if (p.length !== s.length) return false;
  let shared = false;
  for (let i = 0; i < p.length; i++) {
    if (p[i].startsWith(":") || p[i].startsWith("{") || s[i].startsWith("{")) continue;
    if (p[i] !== s[i]) return false;
    shared = true;
  }
  return shared || p.length === 0;
}

/** Every place the frontend sends the browser: links, navigations and built hrefs. */
function links() {
  const out = [];
  for (const file of files) {
    const body = uncommented(file.body);
    const consts = constsOf(body);
    const add = (raw) => {
      if (!raw.startsWith("/") || raw.startsWith("//") || raw.startsWith("/api/")) return;
      out.push({ link: patternize(raw, consts), file: file.path.replace(root + "/", "") });
    };
    for (const match of body.matchAll(/(?:href|navigate)\s*[:=]?\s*\(?\s*[`"'](\/[^`"']*)[`"']/g)) add(match[1]);
    for (const match of body.matchAll(/navigate\(\s*[`"']([^`"']+)[`"']/g)) add(match[1]);
    for (const match of body.matchAll(/`(\/[^`]*)`/g)) add(match[1]);
  }
  return out;
}

/** The navigation entries, with the permission each is filtered by. */
function navEntries() {
  const layout = files.find((f) => f.path.endsWith("/web/src/ui/layout.ts"));
  assert.ok(layout, "web/src/ui/layout.ts not found: the navigation moved");
  const body = uncommented(layout.body);
  const consts = constsOf(body);
  const out = [];
  for (const line of body.split("\n")) {
    const href = line.match(/href:\s*[`"']([^`"']+)[`"']/);
    if (!href) continue;
    const permission = line.match(/permission:\s*"([^"]+)"/);
    out.push({ href: patternize(href[1], consts), permission: permission ? permission[1] : null });
  }
  assert.ok(out.length > 20, `expected the navigation, found ${out.length} entries`);
  return out;
}

/** Every permission this build declares, read from the Go authorization table. */
function permissionCatalogue() {
  const source = readFileSync(join(root, "..", "internal/authn/authz.go"), "utf8");
  const permissions = new Set([...source.matchAll(/Permission\s*=\s*"([^"]+)"/g)].map((m) => m[1]));
  assert.ok(permissions.size > 20, `read ${permissions.size} permissions from authz.go; the table moved`);
  return permissions;
}

/** The paths api/openapi.yaml documents, which is generated from the router and checked by
 *  `make openapi-check`, so it is the served table without needing Go in this test. */
function documentedApiPaths() {
  const doc = readFileSync(join(root, "..", "api", "openapi.yaml"), "utf8");
  const paths = [...doc.matchAll(/^ {4}(\/\S*):$/gm)].map((m) => m[1]);
  assert.ok(paths.length > 100, `read ${paths.length} paths from openapi.yaml; the document moved`);
  return paths;
}

/** The first argument of an api call, up to its matching parenthesis. */
function firstArgument(body, start) {
  let depth = 1;
  let i = start;
  while (i < body.length && depth > 0) {
    const ch = body[i];
    if (ch === '"' || ch === "'" || ch === "`") {
      const quote = ch;
      i++;
      while (i < body.length && body[i] !== quote) {
        if (body[i] === "\\") i++;
        i++;
      }
    } else if (ch === "(") depth++;
    else if (ch === ")") depth--;
    i++;
  }
  return body.slice(start, i - 1);
}

/** Every API path a view actually requests, with the method it uses. */
function apiCalls() {
  const out = [];
  for (const file of files) {
    const body = uncommented(file.body);
    const consts = constsOf(body);
    const add = (method, raw) => {
      const path = patternize(raw, consts);
      if (path.startsWith("/api/v1")) out.push({ method, path, file: file.path.replace(root + "/", "") });
    };
    for (const call of body.matchAll(/\.(get|post|put|patch|del|delete)\s*(?:<[^(]*?>)?\s*\(/g)) {
      const method = call[1] === "del" || call[1] === "delete" ? "DELETE" : call[1].toUpperCase();
      const argument = firstArgument(body, call.index + call[0].length);
      for (const literal of argument.matchAll(/[`"'](\/api\/v1\/[^`"']*)[`"']/g)) add(method, literal[1]);
      for (const template of argument.matchAll(/`([^`]*)`/g)) add(method, template[1]);
      for (const identifier of argument.matchAll(/[,(]\s*([A-Z][A-Z0-9_]*)\s*[,)]/g)) {
        if (consts[identifier[1]]) add(method, consts[identifier[1]]);
      }
    }
    for (const call of body.matchAll(/fetch\(\s*[`"'](\/api\/v1\/[^`"']*)/g)) add("FETCH", call[1]);
  }
  return out;
}

// A route with no link and no navigation entry, and why that is deliberate. Adding a route to
// this list is a decision; forgetting a link is a bug, and this is the difference.
const UNLINKED_ROUTES = new Map([
  ["/reset-password", "reached from the emailed reset link, which carries the token"],
  ["/verify", "reached from the emailed verification link, which carries the token"],
  ["/catalog/:id", "the retired catalog URL: kept so old links work, superseded by /catalog-management/:id"],
]);

test("every navigation entry points at a declared route", () => {
  // The navigation filters an entry whose route is undeclared (hasRoute in routes.ts), which is
  // what makes a link in the panel never a bug. The cost is that a typo in an href does not
  // fail — it silently removes the screen from the menu, and the screen becomes reachable only
  // by URL. That is the failure this asserts against.
  const patterns = routePatterns();
  const dead = navEntries()
    .filter((entry) => !patterns.some((pattern) => covers(pattern, entry.href)))
    .map((entry) => entry.href);
  assert.deepEqual(dead, [], `the navigation points at routes that do not exist: ${dead.join(", ")}`);
});

test("every navigation permission is one this build declares", () => {
  // The same silence in a different place: an entry filtered by a misspelled permission is
  // hidden from every role, so the screen has no way in at all.
  const catalogue = permissionCatalogue();
  const unknown = navEntries()
    .filter((entry) => entry.permission && !catalogue.has(entry.permission))
    .map((entry) => `${entry.href} (${entry.permission})`);
  assert.deepEqual(unknown, [], `navigation entries filtered by undeclared permissions: ${unknown.join(", ")}`);
});

test("every declared route has a way in", () => {
  const routes = routePatterns();
  const all = links();
  const unreachable = routes.filter(
    (pattern) => !UNLINKED_ROUTES.has(pattern) && !all.some(({ link }) => covers(pattern, link)),
  );
  assert.deepEqual(
    unreachable,
    [],
    `these screens have no navigation entry and no link, so only a URL reaches them:\n  ${unreachable.join("\n  ")}`,
  );
});

test("every permission a screen gates on is one this build declares", () => {
  // A gate on a permission that does not exist can never be satisfied, so the screen renders
  // its "you need X" state for every operator who opens it — including the administrator. That
  // is a dead end that looks like a permissions problem, and it is one typo away.
  const catalogue = permissionCatalogue();
  const unknown = [];
  for (const file of files) {
    for (const match of uncommented(file.body).matchAll(/(?:can|has)\(\s*"([a-z][a-z0-9]*(?::[a-z0-9_]+)+)"/g)) {
      if (!catalogue.has(match[1])) unknown.push(`${match[1]}  (in ${file.path.replace(root + "/", "")})`);
    }
  }
  assert.deepEqual(unknown, [], `screens gate on permissions this build never declares:\n  ${unknown.join("\n  ")}`);
});

test("every API path a view requests is one the server serves", () => {
  // A button that calls an endpoint nobody serves is a dead end that no route test can see:
  // the screen renders, the button is there, and the click is a 404. openapi.yaml is generated
  // from the router and checked against it by `make openapi-check`, so it is the served table.
  const documented = documentedApiPaths();
  const orphaned = apiCalls()
    .filter((call) => !documented.some((path) => covers(path, call.path)))
    .map((call) => `${call.method} ${call.path}  (in ${call.file})`);
  const unique = [...new Set(orphaned)];
  assert.deepEqual(unique, [], `views call endpoints the server does not serve:\n  ${unique.join("\n  ")}`);
});
