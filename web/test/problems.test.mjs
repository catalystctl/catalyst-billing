// Translating refusals.
//
// The boundary these tests pin: the server's `detail` is English (it is a Go format string), so the
// *code* is the translatable unit. `problemText` must find the catalogue's `error.<code>` sentence,
// fall back to the English detail when the catalogue has no row, and never return an empty string —
// a blank error box looks like a successful request whose result failed to draw.
//
// The field list is the other half: a validation refusal names the input, so a client can point at
// it and compose the sentence per field rather than showing one generic sentence the reader cannot
// act on.

import assert from "node:assert/strict";
import test from "node:test";

import {
  initI18n,
  problemFieldText,
  problemFrom,
  problemKey,
  problemText,
  resetI18n,
} from "../.test-build/lib/i18n.js";

const EN = {
  "error.unknown_placeholder": "The message uses a word this event does not supply.",
  "error.forbidden": "You do not have permission to do that.",
  "error.field.required": "{{field}} is needed.",
  "error.invalid_input": "One or more fields are not valid.",
};

const DE = {
  "error.unknown_placeholder": "Die Nachricht verwendet ein Wort, das dieses Ereignis nicht liefert.",
  "error.field.required": "{{field}} ist erforderlich.",
};

function fakeApi({ messages, locale }) {
  return {
    async get(path, request = {}) {
      if (path === "/api/v1/i18n/locales") {
        return {
          locales: [
            { code: "en", name: "English", native_name: "English", is_default: true },
            { code: "de", name: "German", native_name: "Deutsch", is_default: false },
          ],
          default: "en",
          resolved: { locale, source: "preference" },
        };
      }
      if (path === "/api/v1/i18n/messages") {
        return { locale: request.query?.locale, messages: messages[request.query?.locale] ?? {} };
      }
      throw new Error(`unexpected request ${path}`);
    },
    async put() {
      return {};
    },
  };
}

async function boot(locale, messages = { en: EN, de: DE }) {
  resetI18n();
  await initI18n(fakeApi({ messages, locale }));
}

test("a refusal is rendered from its code, in the reader's language", async () => {
  await boot("de");
  // The server's detail is English; the sentence a German operator reads is the catalogue's.
  assert.equal(
    problemText({ code: "unknown_placeholder", detail: "invoice_paid does not supply {{x}}" }),
    DE["error.unknown_placeholder"],
  );
});

test("an untranslated code falls back through the chain, never to the key", async () => {
  await boot("de");
  // The default locale's own catalogue is the second tier (initI18n loads it), so a code the
  // operator translated only in English renders that English sentence for a German reader.
  assert.equal(problemText({ code: "forbidden", detail: "you do not have access to this action" }), "You do not have permission to do that.");
  // A code with no row in any catalogue still renders the server's English detail.
  assert.equal(problemText({ code: "not_a_real_code", detail: "the server said no" }), "the server said no");
});

test("a refusal with neither a translation nor a detail still says something", async () => {
  await boot("en");
  assert.equal(problemText({ code: "not_a_real_code" }), "The request could not be completed.");
  assert.equal(problemText(null, "The template was not saved"), "The template was not saved");
});

test("the key a code maps to is derivable from the response", () => {
  assert.equal(problemKey("unknown_placeholder"), "error.unknown_placeholder");
});

test("a field refusal is composed from the field and its code", async () => {
  await boot("de");
  // Per-field first, with the field label substituted: "which box" is half the sentence.
  assert.equal(
    problemFieldText({ field: "name", code: "required" }, "English name"),
    "English name ist erforderlich.",
  );
  // No per-field row for `unknown` and no generic sentence for it either, so the last tier is a
  // composed English sentence that names the field. Never the key.
  assert.equal(problemFieldText({ field: "code", code: "unknown" }), "code is not valid.");
});

test("problemFrom reads an ApiError-shaped object and a bare problem document", () => {
  const wrapped = {
    problem: {
      status: 422,
      code: "invalid_input",
      detail: "the code is not usable",
      fields: [
        { field: "code", code: "invalid" },
        { field: 3, code: "invalid" },
      ],
    },
  };
  const problem = problemFrom(wrapped);
  assert.equal(problem.code, "invalid_input");
  assert.equal(problem.status, 422);
  // A malformed entry is dropped rather than rendered as `[object Object]`.
  assert.deepEqual(problem.fields, [{ field: "code", code: "invalid" }]);

  const bare = problemFrom({ code: "forbidden" });
  assert.equal(bare.code, "forbidden");
  assert.equal(problemFrom("nope"), null);
  assert.equal(problemFrom(new Error("boom")), null);
});
