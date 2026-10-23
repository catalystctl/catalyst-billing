// Stylesheet invariants.
//
// These read the stylesheets and the source markup and assert the small number of properties
// whose absence does not throw, does not fail a typecheck and does not show up in any unit
// test — but does make a screen look broken. Each one below was a real defect found by
// rendering the panel in a real browser:
//
//   * `.truncate` on an inline element clips nothing, because an inline box cannot be a
//     scroll container. Every use is a <span>, so the rule must make it a block.
//   * `overflow-wrap: anywhere` on every table cell drops each cell's min-content width to a
//     single character, and the whole table collapses into a ribbon of single letters.
//   * `.money-code` sets a relative size, which at body size lands under 10px.
//   * `.filter-bar` must bottom-align, or a labelled field and the button beside it sit on
//     different baselines.
//
// The assertions are about intent, not exact values: a redesign may change 10px to 11px and
// this file should still pass.

import assert from "node:assert/strict";
import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import test from "node:test";

const root = new URL("../", import.meta.url).pathname;
const stylesDir = join(root, "src/styles");

/**
 * Parse a stylesheet into flat rules, remembering which at-rules enclose each one.
 *
 * A flat regex is not good enough: comments sit between rules (so a "selector list" would
 * include the comment text) and `@media` bodies nest (so a rule inside a media query looks
 * like a top-level rule). Both mistakes make a "must not be set outside its media query"
 * assertion pass or fail for the wrong reason.
 */
function parseRules(css) {
  const rules = [];
  const stack = [];
  let prelude = "";
  let i = 0;

  while (i < css.length) {
    const ch = css[i];

    if (ch === "/" && css[i + 1] === "*") {
      const end = css.indexOf("*/", i + 2);
      i = end === -1 ? css.length : end + 2;
      continue;
    }
    if (ch === "{") {
      const at = /^@([\w-]+)/.exec(prelude.trim());
      stack.push(at ? `@${at[1]}` : prelude.trim());
      prelude = "";
      i += 1;
      continue;
    }
    if (ch === "}") {
      const selector = stack.pop() ?? "";
      if (selector && !selector.startsWith("@")) {
        rules.push({ selector, body: prelude, at: [...stack] });
      }
      prelude = "";
      i += 1;
      continue;
    }
    prelude += ch;
    i += 1;
  }
  return rules;
}

const rules = readdirSync(stylesDir)
  .filter((name) => name.endsWith(".css"))
  .sort()
  .flatMap((name) => parseRules(readFileSync(join(stylesDir, name), "utf8")));

if (rules.length === 0) throw new Error("no CSS rules parsed — the parser or the path is wrong");

/** Declarations for every rule whose selector list contains `selector` exactly. */
function declarations(selector) {
  const out = [];
  for (const rule of rules) {
    const selectors = rule.selector.split(",").map((s) => s.trim());
    if (!selectors.includes(selector)) continue;
    for (const part of rule.body.split(";")) {
      const idx = part.indexOf(":");
      if (idx === -1) continue;
      out.push({ prop: part.slice(0, idx).trim(), value: part.slice(idx + 1).trim(), at: rule.at });
    }
  }
  return out;
}

/** The last declared value of `prop`, ignoring rules inside at-rules when `topLevel`. */
function value(selector, prop, { topLevel = false } = {}) {
  const hits = declarations(selector).filter(
    (d) => d.prop === prop && (!topLevel || d.at.length === 0),
  );
  return hits.length ? hits[hits.length - 1].value : null;
}

test(".truncate is a block so that it can actually truncate", () => {
  // An inline box ignores `overflow`, so `text-overflow: ellipsis` never fires on it.
  const display = value(".truncate", "display");
  assert.ok(display, ".truncate must set display; without it the span stays inline and clips nothing");
  assert.notEqual(display, "inline", ".truncate must not be inline: an inline box cannot clip its own overflow");
  assert.equal(value(".truncate", "text-overflow"), "ellipsis");
  assert.equal(value(".truncate", "overflow"), "hidden");
});

test("table cells do not use overflow-wrap: anywhere", () => {
  // `anywhere` lowers min-content to one character, which collapses every column of an
  // auto-laid-out table. `break-word` breaks the same long tokens without doing that.
  for (const selector of [".table td", ".table th"]) {
    const wrap = value(selector, "overflow-wrap") ?? value(selector, "word-break");
    if (wrap) assert.notEqual(wrap, "anywhere", `${selector}: use break-word, not anywhere`);
  }
});

test(".money-code keeps the currency legible", () => {
  const size = value(".money-code", "font-size");
  assert.ok(size, ".money-code must set a size");
  const absolute = /^(\d+(?:\.\d+)?)px$/.exec(size);
  if (absolute) {
    assert.ok(Number(absolute[1]) >= 10, `.money-code is ${size}, which is too small to read`);
  } else {
    assert.match(size, /max\(\s*1[0-9](?:\.\d+)?px/, `.money-code should floor its relative size (found ${size})`);
  }
});

test(".filter-bar aligns on the bottom edge so labels and buttons share a line", () => {
  // A labelled control is a column (label above, input below); a button is a single line.
  // Centring them puts the button halfway up the input beside it — the defect in the report.
  assert.equal(value(".filter-bar", "align-items"), "flex-end");
  assert.equal(value(".filter-bar", "flex-wrap"), "wrap");
});

test("a narrow-screen table can drop a column instead of hiding the last one", () => {
  // The hiding must happen inside a media query. A `display` outside one applies at every
  // width and the column would simply never appear.
  assert.equal(value(".hide-sm", "display", { topLevel: true }), null, ".hide-sm must not set display outside its media query");
  assert.equal(value(".hide-md", "display", { topLevel: true }), null, ".hide-md must not set display outside its media query");

  const hidden = (selector) =>
    declarations(selector).some((d) => d.prop === "display" && d.value.startsWith("none") && d.at.some((a) => a === "@media"));
  assert.ok(hidden(".hide-sm"), "no media-query rule hides .hide-sm");
  assert.ok(hidden(".hide-md"), "no media-query rule hides .hide-md");
});

// A class in markup with no rule renders as an unstyled browser default. That is how
// `.inline-row` became an input and a badge stacked on each other, and how `.stack-xs` turned
// two lines into one. The list below is the reviewed remainder: hooks that need no styling of
// their own, or that are styled through a parent rule. Anything new must be handled (styled, or
// added here with a reason), so this cannot silently grow.
const UNSTYLED_BY_DESIGN = new Map([
  ["resource", "wrapper around a page or card body; a plain block is the intended layout"],
  ["empty-state", "wrapper; the inner .empty rules do the styling"],
  ["link", "class-name fragment matched inside a template literal, not a class"],
  ["otp-digit", "behaviour hook; the parent rule `.otp input` styles the box"],
  ["menu-item-label", "the menu item's own rules cover its children"],
  ["menu-item--mfa", "a modifier on an <a class=\"menu-item\">; the base class does the styling"],
  ["flow-name", "text inside .flow-step, which sets colour, size and padding for the chip"],
  ["captcha-host", "mount point for a third-party widget; empty when none is configured, so it must"],
]);

test("classes used in markup are defined somewhere", () => {
  const declared = new Set();
  for (const rule of rules) {
    for (const m of rule.selector.matchAll(/\.([a-zA-Z][\w-]*)/g)) declared.add(m[1]);
  }

  const sources = [];
  const walk = (dir) => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const p = join(dir, entry.name);
      if (entry.isDirectory()) walk(p);
      else if (entry.name.endsWith(".ts")) sources.push(readFileSync(p, "utf8"));
    }
  };
  walk(join(root, "src"));

  const missing = new Map();
  for (const src of sources) {
    for (const m of src.matchAll(/class:\s*"([^"]+)"/g)) {
      for (const token of m[1].split(/\s+/)) {
        if (!token || token.includes("$") || token.includes("{")) continue;
        if (declared.has(token) || UNSTYLED_BY_DESIGN.has(token)) continue;
        missing.set(token, (missing.get(token) ?? 0) + 1);
      }
    }
  }

  const problems = [...missing.entries()]
    .sort((a, b) => b[1] - a[1])
    .map(([name, count]) => `${name} (${count} use${count === 1 ? "" : "s"})`);
  assert.deepEqual(
    problems,
    [],
    `used in markup but defined in no stylesheet: ${problems.join(", ")}. Style it, or add it to UNSTYLED_BY_DESIGN with a reason.`,
  );
});

test("every allowlisted unstyled class is still in use", () => {
  // A stale allowlist entry hides the next real one: if the markup stopped using a class, the
  // exemption should go with it.
  const sources = [];
  const walk = (dir) => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const p = join(dir, entry.name);
      if (entry.isDirectory()) walk(p);
      else if (entry.name.endsWith(".ts")) sources.push(readFileSync(p, "utf8"));
    }
  };
  walk(join(root, "src"));
  const all = sources.join("\n");

  for (const name of UNSTYLED_BY_DESIGN.keys()) {
    if (name === "link") continue; // a fragment, not a class; see the entry
    assert.ok(all.includes(name), `UNSTYLED_BY_DESIGN lists "${name}" but no source uses it any more`);
  }
});
