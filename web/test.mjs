#!/usr/bin/env node
// Bundles the pure modules for the Node test runner.
//
// The modules under test import nothing that touches the DOM, so a plain esbuild bundle with
// no browser target runs under `node --test`. This is faster than a browser test runner and
// needs no dependency beyond esbuild, which the build already requires.

import { build } from "esbuild";
import { readdirSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = dirname(fileURLToPath(import.meta.url));
const outdir = join(root, ".test-build");

const result = await build({
  entryPoints: {
    "lib/format": join(root, "src/lib/format.ts"),
    "lib/i18n": join(root, "src/lib/i18n.ts"),
    "lib/router": join(root, "src/lib/router.ts"),
  },
  bundle: true,
  format: "esm",
  platform: "neutral",
  target: "es2022",
  outdir,
  outExtension: { ".js": ".js" },
  logLevel: "error",
  write: true,
});

if (result.errors.length > 0) process.exit(1);

// Every test file is discovered, never listed.
//
// The list used to be written out here by hand, which meant a new test file silently did not run:
// `npm test` reported 30 passing and the 31st file was simply absent from the output. That is the
// same failure as a guard that has stopped looking — it passes while asserting nothing. A glob
// cannot fall behind.
const testFiles = readdirSync(new URL("./test", import.meta.url))
  .filter((name) => name.endsWith(".test.mjs"))
  .sort()
  .map((name) => `test/${name}`);

if (testFiles.length === 0) {
  console.error("web: no test files found — the discovery glob is wrong");
  process.exit(1);
}

const proc = spawnSync(process.execPath, ["--test", ...testFiles], {
  cwd: root,
  stdio: "inherit",
});
process.exit(proc.status ?? 1);
