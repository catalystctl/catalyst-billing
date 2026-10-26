// The Theme Studio's own guards.
//
// Seven screens, one shared module and one runtime change, and five properties that a
// typecheck cannot see:
//
//   * **Every section is routed and its module exists.** A studio screen with no route is a
//     screen only its author can open; a route whose module was renamed is a dead link that
//     renders the router's "not in this build" state. Both look like a working build.
//   * **The studio calls only documented endpoints.** The theme API is new, and a path invented
//     in a view is a 404 behind a button — the exact failure `routes.test.mjs` documents at
//     length for the catalog screens. Every request in the studio goes through
//     `lib/theme-studio.ts`, so this is one list to check rather than thirty call sites.
//   * **Nothing is inline.** The panel's CSP is `script-src 'self'; style-src 'self'` with no
//     'unsafe-inline'. A `style="…"` attribute or a `<script>` in a studio file would work in a
//     dev build with a relaxed policy and fail in production, which is the worst way to find
//     out.
//   * **The contrast arithmetic is the WCAG one.** The meter is the studio's only numeric claim,
//     and it is duplicated from `internal/brand` on purpose (the server's answer is
//     authoritative, but the meter has to update per keystroke). These are the reference points
//     that keep the two implementations honest.
//   * **The theme runtime still works.** `lib/theme.ts` is extended, not rewritten: the three
//     exports and the `catalyst.theme` storage key are depended on by the shell and by
//     `brand.test.mjs`, and by the server's own pre-paint boot script.
//
// The module under test is bundled with esbuild into `.test-build` the same way `test.mjs`
// bundles the other DOM-free modules: it imports the component kit, but only inside functions,
// so importing it in Node touches no document.

import assert from "node:assert/strict";
import { existsSync, mkdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { pathToFileURL } from "node:url";
import { fileURLToPath } from "node:url";
import test from "node:test";

import { buildSync } from "esbuild";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const src = join(root, "src");
const outdir = join(root, ".test-build", "lib");
mkdirSync(outdir, { recursive: true });

// `theme.ts` is bundled too, so "the exports still exist" is a fact about the module rather than
// about the text of the file. Both are checked: the text check is what catches the storage key
// being renamed, which a module import cannot see.
for (const name of ["theme-studio", "theme"]) {
  buildSync({
    entryPoints: [join(src, "lib", `${name}.ts`)],
    outfile: join(outdir, `${name}.js`),
    bundle: true,
    format: "esm",
    platform: "neutral",
    target: "es2022",
    logLevel: "error",
  });
}

const studio = await import(pathToFileURL(join(outdir, "theme-studio.js")).href);
const theme = await import(pathToFileURL(join(outdir, "theme.js")).href);

/** The studio's source files: the seven views and the shared module. */
const STUDIO_FILES = [
  "lib/theme-studio.ts",
  "views/theme-overview.ts",
  "views/theme-presets.ts",
  "views/theme-colors.ts",
  "views/theme-typography.ts",
  "views/theme-layout.ts",
  "views/theme-css.ts",
  "views/theme-history.ts",
].map((relative) => ({ relative, path: join(src, relative), body: readFileSync(join(src, relative), "utf8") }));

/** The seven sections, with the module each route must load. */
const ROUTES = [
  ["/settings/theme", "theme-overview"],
  ["/settings/theme/presets", "theme-presets"],
  ["/settings/theme/colors", "theme-colors"],
  ["/settings/theme/typography", "theme-typography"],
  ["/settings/theme/layout", "theme-layout"],
  ["/settings/theme/css", "theme-css"],
  ["/settings/theme/history", "theme-history"],
];

/**
 * The endpoints the theme API documents (docs/theming-system.md §6.2 and §6.1).
 *
 * `{…}` is a parameter and matches one path segment on either side, so `/theme/{scope}/draft`
 * covers `/theme/panel/draft`. Nothing else in the studio may request an `/api/v1` path.
 */
const DOCUMENTED = [
  "/api/v1/staff/theme",
  "/api/v1/staff/theme/{scope}",
  "/api/v1/staff/theme/{scope}/draft",
  "/api/v1/staff/theme/revisions/{id}",
  "/api/v1/staff/theme/revisions/{id}/publish",
  "/api/v1/staff/theme/revisions/{id}/restore",
  "/api/v1/staff/theme/revisions/{id}/diff",
  "/api/v1/staff/theme/preview-token",
  "/api/v1/staff/theme/{scope}/export",
  "/api/v1/staff/theme/import",
  "/api/v1/theme",
];

function segments(path) {
  return path.split("?")[0].split("/").filter(Boolean);
}

/** True when `actual` can be an instance of the documented `pattern`. */
function matches(pattern, actual) {
  const p = segments(pattern);
  const a = segments(actual);
  if (p.length !== a.length) return false;
  return p.every((segment, index) => segment.startsWith("{") || segment === a[index]);
}

// ---------------------------------------------------------------- routing --

test("every studio section is routed and lazy-loads a module that exists", () => {
  const routes = readFileSync(join(src, "routes.ts"), "utf8").replace(/\s+/g, " ");
  for (const [path, view] of ROUTES) {
    const line = `route("${path}", () => import("./views/${view}"))`;
    assert.ok(routes.includes(line), `routes.ts does not declare ${path} → ${view}`);
    // A dynamic import is what keeps the studio out of the initial bundle. A static import would
    // still pass the route test and would cost every visitor seven screens of code.
    assert.ok(existsSync(join(src, "views", `${view}.ts`)), `${path} loads views/${view}.ts, which does not exist`);
  }
});

test("every studio view carries the sub-nav, and it links to all seven sections", () => {
  // The sub-nav is built once in the shared module, so this asserts the property that matters:
  // each screen passes its own key, and the module's table covers every route. A screen that
  // forgot the nav would be a cul-de-sac with no way back to the others.
  const shared = STUDIO_FILES.find((file) => file.relative === "lib/theme-studio.ts");
  assert.ok(shared, "lib/theme-studio.ts is missing");
  for (const [path] of ROUTES) {
    assert.ok(shared.body.includes(`href: "${path}"`), `the sub-nav does not link to ${path}`);
  }
  for (const file of STUDIO_FILES.filter((entry) => entry.relative.startsWith("views/"))) {
    assert.match(file.body, /studioHead\(\{/, `${file.relative} does not render the shared studio head`);
    assert.match(file.body, /active: "(overview|presets|colors|typography|layout|css|history)"/, `${file.relative} does not mark its section`);
  }
});

// -------------------------------------------------------------- endpoints --

/**
 * Every API path the studio names, with each parameter normalised to one wildcard.
 *
 * `lib/theme.ts` is scanned too: it makes the one public request in this feature (the manifest,
 * read before the shell exists), and a path outside the shared module would otherwise be the
 * only one no guard covered.
 */
function requestedPaths() {
  const sources = [...STUDIO_FILES, { relative: "lib/theme.ts", body: readFileSync(join(src, "lib", "theme.ts"), "utf8") }];
  const paths = sources.flatMap((file) =>
    [...file.body.matchAll(/[`"](\/api\/v1[^`"]*)[`"]/g)].map((match) =>
      // `${scope}` and `${id}` are the parameters the contract has; each normalises to one
      // wildcard segment so the comparison is about the path's shape, not its variable names.
      match[1].replace(/\$\{[^}]*\}/g, "{x}"),
    ),
  );
  assert.ok(paths.length >= 10, `found ${paths.length} API paths in the studio; expected the theme contract`);
  return paths;
}

test("the studio requests only documented endpoints", () => {
  const requested = requestedPaths();
  const undocumented = requested.filter((path) => !DOCUMENTED.some((pattern) => matches(pattern, path)));
  assert.deepEqual(undocumented, [], `the studio requests paths no documented endpoint serves:\n  ${undocumented.join("\n  ")}`);
});

test("every endpoint the studio uses is in the generated API document", () => {
  // `api/openapi.yaml` is generated from the router and gated by `make openapi-check`, so it is
  // the served table without needing Go in this test. A path the router does not serve is a 404
  // behind a button, and the studio's screens would all render perfectly.
  const document = readFileSync(join(root, "..", "api", "openapi.yaml"), "utf8");
  const documented = [...document.matchAll(/^ {4}(\/\S*):$/gm)].map((match) => match[1]);
  assert.ok(documented.length > 100, `read ${documented.length} paths from openapi.yaml; the document moved`);
  const missing = requestedPaths().filter((path) => !documented.some((pattern) => matches(pattern, path)));
  assert.deepEqual([...new Set(missing)], [], `the studio calls endpoints the router does not serve:\n  ${missing.join("\n  ")}`);
});

test("the views make no requests of their own", () => {
  // Every request goes through `lib/theme-studio.ts`. A view that called the API itself would be
  // a second place to check, and the endpoint test above would silently stop covering it. The
  // views do mention paths in prose — "this installation does not serve GET /api/v1/…" — so the
  // assertion is about requests, not about the substring.
  for (const file of STUDIO_FILES) {
    if (file.relative === "lib/theme-studio.ts") continue;
    assert.ok(
      !/\bapi\.(get|post|put|patch|del|postRaw)\s*[<(]/.test(file.body),
      `${file.relative} calls the API client instead of using the shared helper`,
    );
    assert.ok(!/\bfetch\s*\(/.test(file.body), `${file.relative} calls fetch instead of using the API client`);
  }
});

// ------------------------------------------------------------------- CSP --

test("no studio source contains an inline style or a script", () => {
  for (const file of STUDIO_FILES) {
    assert.ok(!/style\s*=/.test(file.body), `${file.relative} sets a style attribute; use a class or the CSSOM`);
    assert.ok(!/\bstyle\s*:/.test(file.body), `${file.relative} passes a style prop to h(); the CSP forbids the attribute`);
    assert.ok(!/<script/i.test(file.body), `${file.relative} contains a <script> tag`);
    assert.ok(!/createElement\(\s*["']script["']/.test(file.body), `${file.relative} creates a script element`);
    assert.ok(!/javascript:/i.test(file.body), `${file.relative} contains a javascript: URL`);
  }
});

test("every theme- class a studio file uses has a rule in view-theme.css", () => {
  // `styles.test.mjs` enforces "every class used in markup is defined somewhere" across the whole
  // app. This is the studio's own half of that: every class it introduces is `theme-`-prefixed,
  // so a class with no rule renders as an unstyled browser default on a screen whose whole point
  // is looking deliberate.
  const css = readFileSync(join(src, "styles", "view-theme.css"), "utf8");
  const declared = new Set([...css.matchAll(/\.([a-zA-Z][\w-]*)/g)].map((match) => match[1]));
  const missing = new Set();
  for (const file of STUDIO_FILES) {
    for (const match of file.body.matchAll(/class:\s*"([^"]+)"/g)) {
      for (const token of match[1].split(/\s+/)) {
        if (!token.startsWith("theme-")) continue;
        if (!declared.has(token)) missing.add(`${token} (${file.relative})`);
      }
    }
  }
  assert.deepEqual([...missing], [], `theme- classes with no rule in view-theme.css: ${[...missing].join(", ")}`);
});

// -------------------------------------------------------------- contrast --

test("the contrast helper returns the WCAG reference ratios", () => {
  const black = studio.parseColour("#000000");
  const white = studio.parseColour("#ffffff");
  assert.deepEqual(black, { r: 0, g: 0, b: 0 });
  assert.deepEqual(white, { r: 255, g: 255, b: 255 });

  assert.equal(studio.contrastRatio(black, white), 21);
  assert.equal(studio.contrastRatio(white, white), 1);
  // Order does not matter: a ratio is a ratio.
  assert.equal(studio.contrastRatio(black, white), studio.contrastRatio(white, black));

  // The published reference points. 4.54:1 is the canonical "just passes AA on white" grey and
  // 4.0:1 is pure red on white; both are quoted in the WCAG understanding documents, which is
  // what makes them a check rather than a restatement of the formula.
  const grey = studio.contrastRatio(studio.parseColour("#767676"), white);
  assert.ok(Math.abs(grey - 4.54) < 0.01, `#767676 on white = ${grey}, expected 4.54`);

  const red = studio.contrastRatio(studio.parseColour("#ff0000"), white);
  assert.ok(Math.abs(red - 4.0) < 0.01, `#ff0000 on white = ${red}, expected 4.0`);

  // A three-digit hex expands each nibble rather than zero-extending it.
  assert.deepEqual(studio.parseColour("#abc"), studio.parseColour("#aabbcc"));
  // Functional forms are read; a token reference is not measurable and must not be invented.
  assert.deepEqual(studio.parseColour("rgba(0, 0, 0, 0.73)"), { r: 0, g: 0, b: 0 });
  assert.equal(studio.parseColour("var(--accent)"), null);
  assert.equal(studio.parseColour("chartreuse"), null);
});

test("the meter measures both modes against their own thresholds", () => {
  const thresholds = studio.CONTRAST_THRESHOLDS;
  assert.equal(thresholds.body, 4.5);
  assert.equal(thresholds.ui, 3);
  assert.equal(thresholds.line, 1.4);

  // The built-in dark palette, as `internal/theme/defaults.go` ships it: body text is legible,
  // the hairline clears the decorative floor, and the ink on the accent clears body text.
  const dark = {
    bg: "#171413",
    text: "#e7e2de",
    textMuted: "#aaa29b",
    textFaint: "#807973",
    border: "#37322e",
    borderStrong: "#4a4139",
    accent: "#ff951e",
    accentInk: "#241407",
  };
  const report = studio.contrastReport(dark);
  assert.ok(report.length >= 6, `measured ${report.length} pairs; expected the six documented ones`);
  for (const result of report) {
    assert.equal(result.threshold, thresholds[result.kind], `${result.key} was measured against the wrong threshold`);
    assert.equal(result.pass, result.ratio >= result.threshold, `${result.key} disagrees with its own ratio`);
  }
  assert.equal(report.find((result) => result.key === "text")?.pass, true, "the default body text is not legible");
  assert.equal(report.find((result) => result.key === "border")?.pass, true, "the default hairline is below the decorative floor");
  assert.equal(report.find((result) => result.key === "accentInk")?.pass, true, "the default ink on the accent is not legible");

  // A palette that is obviously broken must report a failure rather than an empty list, or the
  // meter would be a decoration that always looks green.
  const washed = { bg: "#ffffff", text: "#f2f2f2", textMuted: "#ededed", textFaint: "#eeeeee", border: "#fafafa", borderStrong: "#f7f7f7", accent: "#fff3e0", accentInk: "#ffffff" };
  const failing = studio.contrastReport(washed);
  assert.ok(failing.length > 0, "the washed palette produced no measurements");
  assert.ok(failing.every((result) => !result.pass), "a washed-out palette was reported as passing");
});

test("a palette with a missing colour is not measured", () => {
  // "This token is not a colour" is a different problem from "this pair is illegible", and the
  // server refuses it at save time with a message that names the field.
  assert.deepEqual(studio.contrastReport({}), []);
  assert.deepEqual(studio.contrastReport(null), []);
  assert.deepEqual(studio.contrastReport({ text: "#000000" }), []);
});

// --------------------------------------------------------------- the draft --

test("the working document round-trips through the path helpers", () => {
  const document = {
    schema: 2,
    id: "x",
    name: "X",
    color: { mode: "dark", brand: { primary: "#ff3d7f" }, semantic: {}, dark: {}, light: {} },
    typography: { display: {}, body: {}, mono: {}, scale: {}, leading: {}, tracking: {} },
    space: {},
    shape: { radius: {} },
    elevation: { dark: {}, light: {} },
    motion: {},
    background: {},
    layout: { shell: {} },
  };
  const copy = studio.cloneDocument(document);
  assert.notEqual(copy, document, "cloneDocument returned the same object");
  assert.notEqual(copy.color, document.color, "cloneDocument was shallow");

  assert.equal(studio.readPath(copy, "color.brand.primary"), "#ff3d7f");
  studio.writePath(copy, "color.brand.primary", "#00ff00");
  assert.equal(studio.readPath(copy, "color.brand.primary"), "#00ff00");
  // Writing a path whose parents do not exist creates them, which is what a sparse document
  // needs: a preset may not carry `color.dark.accentInk` at all.
  studio.writePath(copy, "color.light.accentInk", "#ffffff");
  assert.equal(studio.readPath(copy, "color.light.accentInk"), "#ffffff");
  assert.equal(studio.readPath(copy, "color.light.missing"), "");

  // The comparison that decides "unsaved" must not depend on key order: a field written after a
  // round trip would otherwise make an untouched document look dirty.
  const reordered = JSON.parse(JSON.stringify({ ...document }));
  assert.equal(studio.stableStringify(document), studio.stableStringify(reordered));
  studio.writePath(copy, "color.light.accentInk", undefined);
  studio.writePath(copy, "color.brand.primary", "#ff3d7f");
  assert.equal(studio.stableStringify(copy), studio.stableStringify(document), "removing the added key did not restore the document");
});

test("a digest is shortened without losing its identity", () => {
  assert.equal(studio.shortDigest("sha256:9f2c8a1b4d5e6f708192a3b4c5d6e7f8"), "9f2c8a1b4d5e");
  assert.equal(studio.shortDigest("9f2c8a1b4d5e6f70"), "9f2c8a1b4d5e");
  assert.equal(studio.shortDigest(""), "—");
  assert.equal(studio.shortDigest(null), "—");
});

test("the byte counter counts bytes, not characters", () => {
  // The bound is `max_custom_css_bytes`, so a stylesheet with an accented comment must be
  // counted the way the server counts it.
  assert.equal(studio.byteLength("abc"), 3);
  assert.equal(studio.byteLength("é"), 2);
  assert.equal(studio.byteLength("—"), 3);
});

// ------------------------------------------------------------ the runtime --

test("lib/theme.ts still exports the three functions the shell uses", () => {
  assert.equal(typeof theme.initTheme, "function", "initTheme is gone");
  assert.equal(typeof theme.applyTheme, "function", "applyTheme is gone");
  assert.equal(typeof theme.toggleTheme, "function", "toggleTheme is gone");
  // The manifest reader is the addition, and it must not have replaced any of them.
  assert.equal(typeof theme.loadThemeManifest, "function");
  assert.equal(typeof theme.applyThemeManifest, "function");
});

test("the stored choice is still read from the catalyst.theme key", () => {
  // The key is a contract with two readers that are not this module: the shell's pre-paint boot
  // script (`internal/theme/css/generate.go`) and `web/test/brand.test.mjs`'s neighbours. A
  // rename here would silently stop the returning user's choice from being applied.
  const source = readFileSync(join(src, "lib", "theme.ts"), "utf8");
  assert.match(source, /const KEY = "catalyst\.theme"/);
  assert.match(source, /localStorage\.getItem\(KEY\)/);
  assert.match(source, /localStorage\.setItem\(KEY, theme\)/);
});

test("the manifest decides the mode only when nothing is stored", () => {
  // `applyThemeManifest` is pure enough to exercise: it touches the attributes and the storage
  // of the root it is given. A stored choice must survive a manifest that disagrees with it,
  // because the manifest is a default and the choice is a decision.
  const root = fakeRoot({ theme: "light" });
  const written = theme.applyThemeManifest(
    { published: true, meta: { color_scheme: "dark", nav: "rail", density: "roomy", card_style: "flat", table_style: "comfortable" } },
    root,
  );
  assert.ok(written.includes("data-shell"), `applyThemeManifest wrote ${written.join(", ")}`);
  assert.equal(root.dataset.shell, "rail");
  assert.equal(root.dataset.density, "roomy");
  assert.equal(root.dataset.cardVariant, "flat");
  assert.equal(root.dataset.tableVariant, "comfortable");
});

/** A stand-in for an element's `dataset`, plus a localStorage that a test can control. */
function fakeRoot(initial) {
  const dataset = { ...initial };
  const storage = new Map();
  globalThis.localStorage = {
    getItem: (key) => storage.get(key) ?? null,
    setItem: (key, value) => storage.set(key, String(value)),
    removeItem: (key) => storage.delete(key),
  };
  return { dataset };
}
