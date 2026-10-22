#!/usr/bin/env node
// Fails when the binary reads an environment variable that .env.example does not document.
//
// This exists because the gap is invisible: an undocumented variable works for whoever wrote
// it and is undiscoverable for everyone else, and the failure mode is an operator guessing at
// a name. The check is deliberately one-directional — a documented-but-unread variable is
// allowed (BILLING_APP_DATABASE_URL is kept on purpose, with a comment saying so), while an
// unread *undocumented* one is a mistake.
//
// Run: node scripts/env-check.mjs   (wired into `make env-check`)

import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, extname } from "node:path";

const ROOT = new URL("..", import.meta.url).pathname;
const NAME = /^(BILLING|POSTGRES)_[A-Z0-9_]+$|^APP_ROLE_PASSWORD$/;

// Derived inside the postgres image from POSTGRES_SUPERUSER; setting it in .env does nothing.
const IGNORED = new Set(["POSTGRES_USER"]);

function walk(dir, out = []) {
  for (const entry of readdirSync(dir)) {
    if (entry === "node_modules" || entry === ".git" || entry === "dist") continue;
    const full = join(dir, entry);
    const st = statSync(full);
    if (st.isDirectory()) walk(full, out);
    else out.push(full);
  }
  return out;
}

const files = [
  ...walk(join(ROOT, "internal")).filter((f) => extname(f) === ".go"),
  ...walk(join(ROOT, "cmd")).filter((f) => extname(f) === ".go"),
  ...readdirSync(join(ROOT, "scripts")).map((f) => join(ROOT, "scripts", f)),
  join(ROOT, "Makefile"),
  join(ROOT, "compose.yaml"),
];

const used = new Set();
for (const file of files) {
  let text;
  try {
    text = readFileSync(file, "utf8");
  } catch {
    continue;
  }
  // Go string literals, plus shell/Make/compose ${VAR} and $VAR forms.
  for (const m of text.matchAll(/"([A-Z][A-Z0-9_]+)"/g)) if (NAME.test(m[1])) used.add(m[1]);
  for (const m of text.matchAll(/\$\{?((?:BILLING|POSTGRES)_[A-Z0-9_]+|APP_ROLE_PASSWORD)/g)) used.add(m[1]);
}

const documented = new Set(
  [...readFileSync(join(ROOT, ".env.example"), "utf8").matchAll(/^([A-Z][A-Z0-9_]+)=/gm)].map((m) => m[1]),
);

const missing = [...used].filter((v) => !documented.has(v) && !IGNORED.has(v)).sort();
const extra = [...documented].filter((v) => !used.has(v)).sort();

console.log(`env: ${used.size} read, ${documented.size} documented in .env.example`);
if (extra.length) console.log(`  documented but not read (allowed): ${extra.join(", ")}`);

if (missing.length) {
  console.error(`\nFAIL: read by the code but not in .env.example:\n  ${missing.join("\n  ")}`);
  process.exit(1);
}
console.log("env: every variable the code reads is documented");
