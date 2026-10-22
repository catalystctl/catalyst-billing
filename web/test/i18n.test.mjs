// The translation helper's contract.
//
// The whole feature is a fallback chain that must never produce an empty string, and a switcher
// that must not exist when there is nothing to switch to. Both are cheap to get subtly wrong —
// a blank label reads as a design decision and a one-option dropdown reads as a control — so
// both are pinned here.
//
// The modules are bundled for Node by test.mjs; nothing under test touches the DOM, except
// `languageSwitcher`, which is exercised only on the path where it returns `null` before it
// builds anything.

import assert from "node:assert/strict";
import test from "node:test";

import {
  currentLocale,
  initI18n,
  interpolate,
  languageOptions,
  languageSwitcher,
  loadMessages,
  onLocaleChange,
  resetI18n,
  setLocale,
  t,
  tx,
} from "../.test-build/lib/i18n.js";

const EN = {
  "storefront.nav.products": "Products",
  "storefront.hero.title": "Servers that come with a ledger.",
  "storefront.hero.body": "Pick a plan and pay.",
  "mail.password_reset.line.3": "The link expires in {{value}} and can be used once.",
  "greeting": "Hello {{name}}, welcome.",
  // A row the server would never send — the schema refuses a blank translation — but a client
  // that trusted it would render an empty label. It must fall through like a missing key.
  "storefront.empty.show_all": "",
};

const DE = {
  "storefront.nav.products": "Produkte",
  "storefront.hero.body": "Wähle einen Tarif und zahle.",
  "storefront.empty.show_all": "",
  // `storefront.hero.title` is deliberately absent: it is the key the fallback test uses.
};

/** A stand-in for lib/api.ts. `messages` is locale -> catalogue, exactly as the endpoint sends
 *  it: already merged with the default locale's text. */
function fakeApi(options = {}) {
  const locales = options.locales ?? [
    { code: "en", name: "English", native_name: "English", is_default: true },
    { code: "de", name: "German", native_name: "Deutsch", is_default: false },
  ];
  const messages = options.messages ?? { en: EN, de: DE };
  const resolved = options.resolved ?? "en";
  const calls = [];
  return {
    calls,
    async get(path, request = {}) {
      calls.push({ path, query: request.query ?? {} });
      if (path === "/api/v1/i18n/locales") {
        return { locales, default: "en", resolved: { locale: resolved, source: "header" } };
      }
      if (path === "/api/v1/i18n/messages") {
        const code = request.query?.locale;
        if (!messages[code]) throw new Error(`no catalogue for ${code}`);
        return { locale: code, messages: messages[code] };
      }
      throw new Error(`unexpected request ${path}`);
    },
  };
}

/** Boots the module in English, which is the state every other test starts from. */
async function boot(options = {}) {
  resetI18n();
  const api = fakeApi(options);
  await initI18n(api);
  return api;
}

test("a key that exists in the locale renders that locale's text", async () => {
  await boot({ resolved: "de" });
  assert.equal(currentLocale(), "de");
  assert.equal(t("storefront.nav.products"), "Produkte");
  assert.equal(t("storefront.hero.body"), "Wähle einen Tarif und zahle.");
});

test("a key missing in the locale falls back to the default locale's text", async () => {
  await boot({ resolved: "de" });
  // Absent from DE, present in EN: the German page shows the English sentence rather than
  // `storefront.hero.title` or nothing at all.
  assert.equal(t("storefront.hero.title"), "Servers that come with a ledger.");
});

test("a blank row falls back rather than rendering nothing", async () => {
  await boot({ resolved: "de" });
  // The schema refuses to store a blank translation, so this only happens if something upstream
  // breaks — and when it does, the customer must not get an empty button.
  assert.equal(t("storefront.empty.show_all"), "storefront.empty.show_all");
  assert.equal(tx("storefront.empty.show_all", "Show everything"), "Show everything");
});

test("a key missing everywhere returns the key itself", async () => {
  await boot();
  assert.equal(t("storefront.hero.nonexistent"), "storefront.hero.nonexistent");
  assert.equal(t("no.such.key"), "no.such.key");
});

test("{{name}} interpolation substitutes the value", async () => {
  await boot();
  assert.equal(t("greeting", { name: "Ada" }), "Hello Ada, welcome.");
  assert.equal(
    t("mail.password_reset.line.3", { value: "30 minutes" }),
    "The link expires in 30 minutes and can be used once.",
  );
  // A numeric value is stringified rather than dropped; 0 is a value, not an absence.
  assert.equal(t("greeting", { name: 0 }), "Hello 0, welcome.");
});

test("a value containing a placeholder is written literally, not expanded", async () => {
  await boot();
  // Single pass. If the substitution re-examined what it wrote, a customer who can type into a
  // field could make a message pull in another variable's value.
  assert.equal(
    t("mail.password_reset.line.3", { value: "{{link}}", link: "https://evil.example" }),
    "The link expires in {{link}} and can be used once.",
  );
});

test("an unknown placeholder is left as written", async () => {
  await boot();
  // Visible and fixable, rather than silently blank — the same rule as i18n.Substitute in Go.
  assert.equal(t("greeting", {}), "Hello {{name}}, welcome.");
});

test("interpolate is the same single pass, exposed", () => {
  assert.equal(interpolate("{{a}}-{{b}}", { a: "{{b}}", b: "x" }), "{{b}}-x");
  assert.equal(interpolate("no placeholders", { a: "x" }), "no placeholders");
  assert.equal(interpolate("{{a}}", { a: "" }), "");
  // A placeholder named after an Object.prototype member must not resolve to a function.
  assert.equal(interpolate("{{constructor}}", {}), "{{constructor}}");
});

test("tx renders the English sentence when the catalogue has nothing", async () => {
  await boot({ resolved: "de" });
  assert.equal(
    tx("login.title", "Sign in"),
    "Sign in",
    "a key the catalogue does not have must render the sentence the view passed",
  );
  // And the catalogue wins when it does have the key.
  assert.equal(tx("storefront.nav.products", "Products"), "Produkte");
});

test("a missing translation never renders blank", async () => {
  await boot({ resolved: "de" });
  const keys = ["storefront.hero.title", "login.title", "no.such.key", "storefront.empty.show_all"];
  for (const key of keys) {
    const rendered = t(key, { name: "x" }, "An English sentence");
    assert.notEqual(rendered, "", `${key} rendered an empty string`);
    assert.equal(typeof rendered, "string");
  }
});

test("the switcher is absent when only one locale is offered", async () => {
  await boot({
    locales: [{ code: "en", name: "English", native_name: "English", is_default: true }],
    messages: { en: EN },
  });
  assert.deepEqual(languageOptions(), [], "one language is not a choice");
  assert.equal(
    languageSwitcher({ onSelect: () => {} }),
    null,
    "the shell must render nothing at all rather than a dead control",
  );
  assert.deepEqual(languageOptions([]), []);
});

test("the switcher lists the native names when there is a choice", async () => {
  await boot({ resolved: "de" });
  const options = languageOptions();
  assert.equal(options.length, 2);
  assert.deepEqual(
    options.map((o) => o.label),
    ["English", "Deutsch"],
  );
  assert.equal(options.find((o) => o.selected)?.code, "de");
});

test("setLocale switches, persists, and notifies once", async () => {
  // A stub for the browser's localStorage, so persistence is actually asserted.
  const store = new Map();
  globalThis.localStorage = {
    getItem: (key) => store.get(key) ?? null,
    setItem: (key, value) => store.set(key, String(value)),
  };
  try {
    await boot();
    const seen = [];
    const off = onLocaleChange((locale) => seen.push(locale));

    await setLocale("de");
    assert.equal(currentLocale(), "de");
    assert.equal(t("storefront.nav.products"), "Produkte");
    assert.deepEqual(seen, ["de"]);
    assert.equal(store.get("catalyst.locale"), "de");

    // Switching to the locale already in force is not a change, and must not re-render the page.
    await setLocale("de");
    assert.deepEqual(seen, ["de"]);

    // A locale the installation does not offer is refused rather than resolved to nothing.
    await setLocale("fr");
    assert.equal(currentLocale(), "de");

    off();
    await setLocale("en");
    assert.deepEqual(seen, ["de"], "an unsubscribed listener must stop hearing about changes");
  } finally {
    delete globalThis.localStorage;
  }
});

test("a stored choice survives a reload", async () => {
  const store = new Map([["catalyst.locale", "de"]]);
  globalThis.localStorage = {
    getItem: (key) => store.get(key) ?? null,
    setItem: (key, value) => store.set(key, String(value)),
  };
  try {
    // The server resolved English (the browser's header, or the default); the person's own
    // click is the more recent deliberate act and must win.
    await boot({ resolved: "en" });
    assert.equal(currentLocale(), "de");
  } finally {
    delete globalThis.localStorage;
  }
});

test("a stored locale that is no longer offered is ignored", async () => {
  const store = new Map([["catalyst.locale", "fr"]]);
  globalThis.localStorage = {
    getItem: (key) => store.get(key) ?? null,
    setItem: (key, value) => store.set(key, String(value)),
  };
  try {
    await boot({ resolved: "de" });
    assert.equal(currentLocale(), "de");
  } finally {
    delete globalThis.localStorage;
  }
});

test("an unreachable locale endpoint leaves the page in English rather than failing", async () => {
  resetI18n();
  const api = {
    async get() {
      throw new Error("network down");
    },
  };
  const locales = await initI18n(api);
  assert.deepEqual(locales, []);
  assert.equal(currentLocale(), "en");
  assert.equal(t("storefront.hero.title"), "storefront.hero.title");
  assert.equal(tx("storefront.hero.title", "Servers that come with a ledger."), "Servers that come with a ledger.");
  assert.equal(languageSwitcher({ onSelect: () => {} }), null);
});

test("the catalogue is fetched once per locale and merged responses are used as given", async () => {
  const api = await boot({ resolved: "de" });
  // `de` and the default `en` (the second tier of the chain) — one request each, then cached.
  const messageCalls = api.calls.filter((call) => call.path === "/api/v1/i18n/messages");
  assert.deepEqual(
    messageCalls.map((call) => call.query.locale).sort(),
    ["de", "en"],
  );

  const before = api.calls.length;
  await loadMessages(api, "de");
  await loadMessages(api, "en");
  assert.equal(api.calls.length, before, "a cached catalogue must not be fetched again");
});
