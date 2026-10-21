#!/usr/bin/env node
// Bundles the pure modules for the Node test runner.
//
// The modules under test import nothing that touches the DOM, so a plain esbuild bundle with
// no browser target runs under `node --test`. This is faster than a browser test runner and
// needs no dependency beyond esbuild, which the build already requires.

import { build } from "esbuild";
import { spawnSync } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = dirname(fileURLToPath(import.meta.url));
const outdir = join(root, ".test-build");

const result = await build({
  entryPoints: {
    "lib/format": join(root, "src/lib/format.ts"),
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

const proc = spawnSync(process.execPath, ["--test", "test/frontend.test.mjs"], {
  cwd: root,
  stdio: "inherit",
});
process.exit(proc.status ?? 1);
