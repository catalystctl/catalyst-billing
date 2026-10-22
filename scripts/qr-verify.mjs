#!/usr/bin/env node
// Verifies the QR encoder against an independent implementation.
//
// web/src/lib/qr.ts generates the enrolment symbol in the page, because the panel's CSP forbids
// a third-party image service and a TOTP secret must not leave the origin. That makes the
// encoder security-relevant: a symbol that no phone can read sends the customer back to typing
// a 32-character base32 secret, which is the outcome the QR exists to avoid.
//
// A self-consistent encoder proves nothing — the same wrong table produces a matrix that decodes
// with the same wrong table. So this compares against `qrencode` (a separate implementation in C)
// module for module. Another encoder may pick a different mask, which is legitimate, so each of
// this encoder's eight candidates is compared and one match is required.
//
// Requires: qrencode and python3 with PIL. Both are present on the development host; this is a
// verification script rather than part of `npm test`, which must stay dependency-free.
//
// Usage: node scripts/qr-verify.mjs

import { execFileSync } from "node:child_process";
import { createRequire } from "node:module";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const work = mkdtempSync(join(tmpdir(), "qr-verify-"));

// esbuild belongs to the web package, and this script lives outside it, so it is resolved from
// web/package.json rather than from here.
const { build } = createRequire(join(root, "web/package.json"))("esbuild");

const bundle = join(work, "qr.mjs");
await build({
  entryPoints: [join(root, "web/src/lib/qr.ts")],
  bundle: true,
  format: "esm",
  platform: "neutral",
  target: "es2022",
  outfile: bundle,
  logLevel: "error",
});
const { qrCandidates } = await import(pathToFileURL(bundle).href);

/** Reads a QR PNG produced with margin 0 and size 1 as a boolean matrix. */
function readPng(path) {
  const script = `
from PIL import Image
im = Image.open(${JSON.stringify(path)}).convert("L")
w, h = im.size
for y in range(h):
    print("".join("1" if im.getpixel((x, y)) < 128 else "0" for x in range(w)))
`;
  const out = execFileSync("python3", ["-c", script], { encoding: "utf8" });
  return out
    .trim()
    .split("\n")
    .map((line) => [...line].map((c) => c === "1"));
}

function reference(text, level, version) {
  const png = join(work, `ref-${Math.random().toString(36).slice(2)}.png`);
  // `-8` forces byte mode. Without it qrencode segments the input into numeric and alphanumeric
  // runs where it can, which is a smaller symbol for the same text and therefore a different
  // matrix: both are valid QR codes, and the comparison has to be between the same mode.
  execFileSync("qrencode", ["-l", level, "-8", "-v", String(version), "-m", "0", "-s", "1", "-o", png, text]);
  const matrix = readPng(png);
  rmSync(png, { force: true });
  return matrix;
}

function same(a, b) {
  if (a.length !== b.length) return false;
  for (let r = 0; r < a.length; r++) {
    if (a[r].length !== b[r].length) return false;
    for (let c = 0; c < a[r].length; c++) if (a[r][c] !== b[r][c]) return false;
  }
  return true;
}

const cases = [
  "otpauth://totp/Catalyst%20Billing%3Aops%40example.test?secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP&issuer=Catalyst+Billing&algorithm=SHA1&digits=6&period=30",
  "otpauth://totp/Catalyst%20Billing%3Aa%40b.test?secret=JBSWY3DPEHPK3PXP&issuer=Catalyst+Billing&algorithm=SHA1&digits=6&period=30",
  "hello",
  "https://panel.example.test/settings#security",
  "x".repeat(120),
  "x".repeat(200),
  "ünïcødé — a label with an em dash",
];

let failures = 0;
for (const level of ["M"]) {
  for (const text of cases) {
    const candidates = qrCandidates(text, level);
    if (!candidates) {
      console.log(`skip ${level} ${text.length} bytes: too long for the version table`);
      continue;
    }
    const version = candidates[0].version;
    const ref = reference(text, level, version);
    const match = candidates.find((candidate) => same(candidate.modules, ref));
    if (match) {
      console.log(`ok   ${level} v${version} mask ${match.mask} — ${text.length} bytes`);
    } else {
      failures++;
      const chosen = candidates[0];
      console.log(
        `FAIL ${level} v${version} — ${text.length} bytes: no candidate matches qrencode ` +
          `(ours ${chosen.size}×${chosen.size}, reference ${ref.length}×${ref[0]?.length})`,
      );
      if (process.env.QR_VERIFY_DUMP) {
        writeFileSync(join(work, "ours.txt"), chosen.modules.map((r) => r.map((m) => (m ? "1" : "0")).join("")).join("\n"));
        writeFileSync(join(work, "ref.txt"), ref.map((r) => r.map((m) => (m ? "1" : "0")).join("")).join("\n"));
        console.log(`     wrote ${work}/ours.txt and ${work}/ref.txt`);
      }
    }
  }
}

rmSync(work, { recursive: true, force: true });
if (failures > 0) {
  console.log(`\n${failures} mismatch(es)`);
  process.exit(1);
}
console.log("\nqr-verify: ok");
