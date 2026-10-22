// A 401 is not one thing.
//
// The credential endpoints answer 401 for a wrong secret while the session is healthy: a mistyped
// two-factor code, a wrong current password. Treating those as "session expired" signed the user
// out mid-dialog and threw away the enrolment secret they were typing, so the flow looked broken
// while the server was answering correctly.
//
// This test pins the distinction in the direction that fails safe: only the codes that mean "the
// session is gone" sign you out, and anything unrecognised does not.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

const source = readFileSync(new URL("../src/lib/api.ts", import.meta.url), "utf8");

test("a credential failure does not sign the user out", () => {
  // The predicate must be applied to the 401 branch, not every 401.
  assert.match(
    source,
    /response\.status === 401 && isSessionDead\(problem\.code\)/,
    "the 401 branch must consult the code before ending the session",
  );
});

test("the session-dead set is an allowlist, so a new credential code fails safe", () => {
  const body = source.slice(source.indexOf("function isSessionDead"));
  const allowlist = body.slice(0, body.indexOf("\n}"));

  for (const code of ["unauthenticated", "authentication_required", "operator_session_ended"]) {
    assert.ok(allowlist.includes(`"${code}"`), `${code} must end the session`);
  }
  // The credential codes must NOT appear in the session-dead set.
  for (const code of ["invalid_code", "invalid_credentials", "challenge_expired"]) {
    assert.ok(!allowlist.includes(`"${code}"`), `${code} is a wrong secret, not a dead session`);
  }
});
