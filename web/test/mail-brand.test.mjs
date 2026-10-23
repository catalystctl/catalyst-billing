// The mail-brand overlay and the accent presets.
//
// The screen these feed is easy to get "working" and hard to get right: the defaults have to agree
// with the Go settings registry, an empty value means "fall back" rather than "blank", and a preset
// must never be able to write a token that is not an accent. Each of those is a decision this module
// makes, so each is pinned here rather than in a screenshot test.

import assert from "node:assert/strict";
import test from "node:test";

import {
  ACCENT_PRESETS,
  MAIL_BRAND_DEFAULTS,
  MAIL_BRAND_KEYS,
  mailBrandFromSettings,
  mailBrandToSettings,
  presetAccents,
  presetMatches,
  safeAccent,
} from "../.test-build/lib/mail-brand.js";

test("an installation that never opened the mail screen reads the registry's defaults", () => {
  const brand = mailBrandFromSettings([]);
  assert.deepEqual(brand, MAIL_BRAND_DEFAULTS);
  // Branding on, and no hard-coded words: the header falls back to the company name at send time.
  assert.equal(brand.html, true);
  assert.equal(brand.header, "");
  assert.equal(brand.footer, "");
  assert.equal(brand.accent, "");
  assert.equal(brand.logo_url, "");
});

test("a group read is read by key, and an unparseable boolean keeps the default", () => {
  const brand = mailBrandFromSettings([
    { key: "brand_html", value: "false" },
    { key: "brand_header", value: "  Northwind  " },
    { key: "brand_footer", value: "12 Harbour Road" },
    { key: "brand_accent", value: "#0F766E" },
    { key: "brand_logo_url", value: "https://cdn.example.test/logo.png" },
    { key: "smtp_host", value: "smtp.example.test" },
  ]);
  assert.equal(brand.html, false);
  assert.equal(brand.header, "Northwind");
  assert.equal(brand.footer, "12 Harbour Road");
  assert.equal(brand.accent, "#0F766E");
  assert.equal(brand.logo_url, "https://cdn.example.test/logo.png");

  // A stored value that is not a boolean does not silently switch branding off.
  const odd = mailBrandFromSettings([{ key: "brand_html", value: "maybe" }]);
  assert.equal(odd.html, true);
});

test("what the screen writes is what the settings table stores", () => {
  const values = mailBrandToSettings({
    html: false,
    header: " Northwind ",
    footer: "",
    accent: "#0F766E",
    logo_url: " https://cdn.example.test/logo.png ",
  });
  assert.deepEqual(values, {
    [MAIL_BRAND_KEYS.html]: "false",
    [MAIL_BRAND_KEYS.header]: "Northwind",
    [MAIL_BRAND_KEYS.footer]: "",
    [MAIL_BRAND_KEYS.accent]: "#0f766e",
    [MAIL_BRAND_KEYS.logo_url]: "https://cdn.example.test/logo.png",
  });
  // And the round trip gives the same object back, so a save then a reload is not a surprise.
  const keys = Object.entries(values).map(([key, value]) => ({ key, value }));
  assert.deepEqual(mailBrandFromSettings(keys), {
    html: false,
    header: "Northwind",
    footer: "",
    accent: "#0f766e",
    logo_url: "https://cdn.example.test/logo.png",
  });
});

test("an accent is only ever a hex colour, lower-cased, or nothing", () => {
  assert.equal(safeAccent("#0F766E"), "#0f766e");
  assert.equal(safeAccent("red; background:url(x)"), "");
  assert.equal(safeAccent("url(javascript:alert(1))"), "");
  assert.equal(safeAccent("#12345"), "");
  assert.equal(safeAccent(""), "");
});

test("every preset is a legible pair in both modes, and touches nothing but the accents", () => {
  assert.ok(ACCENT_PRESETS.length >= 4, "a menu of one preset is not a menu");
  const names = new Set();
  for (const preset of ACCENT_PRESETS) {
    assert.ok(!names.has(preset.name), `duplicate preset name ${preset.name}`);
    names.add(preset.name);
    const accents = presetAccents(preset);
    assert.match(accents.light, /^#[0-9a-f]{6}$/);
    assert.match(accents.dark, /^#[0-9a-f]{6}$/);
    // The two modes need different luminances for the same intent: a light accent that equals the
    // dark one is a pair that is washed out in one of them.
    assert.notEqual(accents.light, accents.dark, `${preset.name} uses one colour for both modes`);
    // A preset contributes exactly the accent family; there is no field for a surface or a text
    // colour, which is what keeps a preset from being a theme registry.
    assert.deepEqual(Object.keys(accents).sort(), ["dark", "light"]);
  }
});

test("a preset is marked active only when both stored accents are its own", () => {
  const teal = ACCENT_PRESETS.find((preset) => preset.name === "Teal");
  assert.ok(teal);
  assert.equal(presetMatches(teal, { accent_light: "#0F766E", accent_dark: "#5EEAD4" }), true);
  assert.equal(presetMatches(teal, { accent_light: "#0F766E", accent_dark: "" }), false);
  assert.equal(presetMatches(teal, { accent_light: "", accent_dark: "" }), false);
});
