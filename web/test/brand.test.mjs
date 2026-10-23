// The tenant's brand, as arithmetic.
//
// Two properties are worth a test that runs without a browser, and they are the two the
// acceptance criteria name:
//
//   * **A theme cannot be broken.** `paintFor` returns the accent family and nothing else, and
//     returns `null` unless an accent is configured for the mode being painted. So an unset
//     brand, and a brand that set only one mode's accent, write nothing for the other mode —
//     the stylesheet's own tokens stand, in both light and dark.
//   * **An unset brand is a fallback, not a lookalike.** `isBrandEmpty` is true for the empty
//     brand and for every field being empty, and `applyBrand` with it writes no property at all.
//
// The colour arithmetic is the mirror of `internal/brand/brand.go`, and the same cases are
// asserted there: two implementations of WCAG luminance that disagreed would put a legible
// colour on the panel and an illegible one on the invoice.

import assert from "node:assert/strict";
import test from "node:test";

import {
  BRAND_PROPERTIES,
  NO_BRAND,
  accentFor,
  applyBrand,
  clearBrand,
  contrast,
  derivedFor,
  hexOf,
  isBrandEmpty,
  luminance,
  mix,
  paintFor,
  parseHex,
  readableHex,
} from "../.test-build/lib/brand.js";

/** A minimal stand-in for an element's `style`, which is all `applyBrand` touches. */
function fakeRoot() {
  const properties = new Map();
  return {
    style: {
      setProperty(name, value) {
        properties.set(name, value);
      },
      removeProperty(name) {
        properties.delete(name);
      },
    },
    read() {
      return Object.fromEntries(properties);
    },
  };
}

// ------------------------------------------------------------------ parsing --

test("parseHex accepts the forms an operator types", () => {
  assert.deepEqual(parseHex("#3b5bdb"), { r: 0x3b, g: 0x5b, b: 0xdb });
  assert.deepEqual(parseHex("3b5bdb"), { r: 0x3b, g: 0x5b, b: 0xdb });
  assert.deepEqual(parseHex("  #3B5BDB "), { r: 0x3b, g: 0x5b, b: 0xdb });
  // Three digits expand each nibble rather than zero-extending it.
  assert.deepEqual(parseHex("#abc"), { r: 0xaa, g: 0xbb, b: 0xcc });
  assert.deepEqual(parseHex("#000"), { r: 0, g: 0, b: 0 });
});

test("parseHex refuses what is not a colour", () => {
  for (const input of ["", "   ", "#", "3b5bd", "#3b5bdba", "#3b5bdg", "rgb(59,91,219)", "rebeccapurple", "#3b5bdb;"]) {
    assert.equal(parseHex(input), null, `${JSON.stringify(input)} was accepted`);
  }
});

test("hexOf round-trips the canonical form", () => {
  for (const hex of ["#3b5bdb", "#7b95ff", "#000000", "#ffffff", "#c0392b"]) {
    assert.equal(hexOf(parseHex(hex)), hex);
  }
});

// ---------------------------------------------------------------- contrast --

test("contrast matches the WCAG reference points", () => {
  const black = { r: 0, g: 0, b: 0 };
  const white = { r: 255, g: 255, b: 255 };
  assert.equal(contrast(white, white), 1);
  const bw = contrast(black, white);
  assert.ok(bw > 20.9 && bw < 21.1, `black on white = ${bw}`);
  assert.equal(contrast(black, white), contrast(white, black));
});

test("the built-in accents are legible on their own backgrounds", () => {
  // The light accent on the light background, and the dark accent on the dark one. These are
  // the references a tenant's colour has to meet, and the values `tokens.css` ships.
  const lightAccent = parseHex("#3b5bdb");
  const lightBg = parseHex("#f5f6f8");
  const darkAccent = parseHex("#7b95ff");
  const darkBg = parseHex("#0a0d13");
  assert.ok(contrast(lightAccent, lightBg) >= 4.5, "the light accent fails AA on the light background");
  assert.ok(contrast(darkAccent, darkBg) >= 4.5, "the dark accent fails AA on the dark background");
});

test("readableHex darkens a pale colour and leaves a legible one alone", () => {
  const white = parseHex("#ffffff");
  const ok = parseHex("#3b5bdb");
  assert.equal(readableHex(ok, white), "#3b5bdb");

  const pale = parseHex("#f1e05a");
  assert.ok(contrast(pale, white) < 4.5, "the fixture is not pale");
  const adjusted = readableHex(pale, white);
  assert.ok(contrast(parseHex(adjusted), white) >= 4.5, `adjusted colour ${adjusted} still fails AA`);
  // The hue survives: a "readable" grey would be a different brand colour.
  const rgb = parseHex(adjusted);
  assert.ok(rgb.r >= rgb.g, "the hue was not preserved");
});

test("mix is exact at its ends", () => {
  const black = { r: 0, g: 0, b: 0 };
  const white = { r: 255, g: 255, b: 255 };
  assert.deepEqual(mix(black, white, 0), black);
  assert.deepEqual(mix(black, white, 1), white);
  assert.equal(hexOf(mix(black, white, 0.5)), "#808080");
});

test("luminance is ordered and bounded", () => {
  assert.equal(luminance({ r: 0, g: 0, b: 0 }), 0);
  assert.ok(Math.abs(luminance({ r: 255, g: 255, b: 255 }) - 1) < 1e-9);
});

// ------------------------------------------------------------------- paint --

test("an unset brand paints nothing at all", () => {
  assert.equal(isBrandEmpty(NO_BRAND), true);
  assert.equal(isBrandEmpty(null), true);
  assert.equal(isBrandEmpty(undefined), true);
  for (const mode of ["light", "dark"]) {
    assert.equal(paintFor(NO_BRAND, mode), null, `${mode} painted something for an unset brand`);
    assert.equal(accentFor(NO_BRAND, mode), "");
  }
});

test("a brand with only a name paints no colour", () => {
  const named = { ...NO_BRAND, name: "Acme Hosting" };
  assert.equal(isBrandEmpty(named), false, "a name is a brand");
  // The name reaches the chrome, but the accent must not be invented: no accent is configured,
  // so the built-in colour stands in both modes.
  assert.equal(paintFor(named, "light"), null);
  assert.equal(paintFor(named, "dark"), null);
});

test("the accent is per mode, and an unset mode falls back", () => {
  const brand = { ...NO_BRAND, accent_light: "#c0392b", accent_dark: "" };
  const light = paintFor(brand, "light");
  assert.ok(light, "the light accent did not paint");
  assert.equal(light.accent, "#c0392b");
  // The dark mode has no accent, so it paints nothing and keeps the built-in token. This is the
  // property that makes "the themes still work" a fact: a brand cannot leak into the other mode.
  assert.equal(paintFor(brand, "dark"), null);
  assert.equal(accentFor(brand, "dark"), "");
});

test("paintFor writes the accent family only", () => {
  const brand = { ...NO_BRAND, accent_dark: "#7b95ff" };
  const paint = paintFor(brand, "dark");
  assert.ok(paint);
  // The five properties are the accent family. A brand that could set --bg or --text would be a
  // theme registry by another name, and the first tenant to pick a dark surface with dark text
  // would have a panel they cannot read in one mode only.
  assert.deepEqual([...BRAND_PROPERTIES], [
    "--accent",
    "--accent-ink",
    "--accent-text",
    "--accent-soft",
    "--accent-soft-strong",
  ]);
  for (const property of Object.keys(paint)) {
    assert.ok(
      ["accent", "ink", "text", "soft", "softStrong"].includes(property),
      `paintFor produced an unexpected field ${property}`,
    );
  }
  // The ink is chosen for contrast against the accent: the dark accent is pale, so its ink is
  // near-black, exactly as --accent-ink is in the dark theme.
  assert.equal(paint.ink, "#0a0d13");
});

test("applyBrand writes nothing for an empty brand and clears a previous brand", () => {
  const root = fakeRoot();
  // An installation with no brand must not touch the document at all: no property, so the
  // stylesheet's own tokens are the only source and the rendering is byte-identical.
  assert.deepEqual(applyBrand(NO_BRAND, "light", root), []);
  assert.deepEqual(root.read(), {});

  // A brand writes its family.
  const written = applyBrand({ ...NO_BRAND, accent_light: "#c0392b" }, "light", root);
  assert.deepEqual(written.sort(), [...BRAND_PROPERTIES].sort());
  assert.equal(root.read()["--accent"], "#c0392b");

  // Switching to a mode with no accent removes the previous override rather than keeping it:
  // this is what stops a light accent from leaking into the dark theme.
  assert.deepEqual(applyBrand({ ...NO_BRAND, accent_light: "#c0392b" }, "dark", root), []);
  assert.deepEqual(root.read(), {});

  // And clearing is explicit and complete.
  applyBrand({ ...NO_BRAND, accent_dark: "#7b95ff" }, "dark", root);
  assert.equal(root.read()["--accent"], "#7b95ff");
  clearBrand(root);
  assert.deepEqual(root.read(), {});
});

test("applyBrand never writes a non-accent property", () => {
  const root = fakeRoot();
  applyBrand({ ...NO_BRAND, accent_light: "#c0392b", accent_dark: "#7b95ff" }, "dark", root);
  for (const property of Object.keys(root.read())) {
    assert.ok(BRAND_PROPERTIES.includes(property), `applyBrand wrote ${property}`);
  }
});

// ---------------------------------------------------------------- derivation --

test("the derivation prefers what the server sent", () => {
  // The panel and the invoice must agree about whether a colour is legible, and two
  // implementations of WCAG arithmetic are two answers. When the server sends one, it wins.
  const brand = {
    ...NO_BRAND,
    accent_light: "#c0392b",
    resolved: {
      light: { ink: "#ffffff", text: "#a5321f", contrast: 5.9, legible: true, set: true },
      dark: { ink: "", text: "", contrast: 0, legible: true, set: false },
    },
  };
  const derived = derivedFor(brand, "light");
  assert.equal(derived.text, "#a5321f");
  assert.equal(derived.contrast, 5.9);
  assert.equal(derivedFor(brand, "dark").set, false);
});

test("the derivation falls back to local arithmetic for an older server", () => {
  const brand = { ...NO_BRAND, accent_light: "#f1e05a" };
  const derived = derivedFor(brand, "light");
  assert.equal(derived.set, true);
  assert.equal(derived.legible, false, "a pale yellow was reported as legible");
  assert.ok(derived.contrast < 4.5);
  // The offered colour is a readable relative of the same hue.
  assert.ok(contrast(parseHex(derived.text), parseHex("#f5f6f8")) >= 4.5);
});

test("a malformed stored accent is not painted", () => {
  const brand = { ...NO_BRAND, accent_light: "chartreuse" };
  assert.equal(paintFor(brand, "light"), null);
  assert.equal(accentFor(brand, "light"), "chartreuse", "the raw value is still reported");
});
