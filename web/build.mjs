#!/usr/bin/env node
// The frontend build.
//
// It produces exactly what web/web.go serves: an index.html plus content-hashed assets
// under /assets. The hash is what makes the immutable cache header in web.go truthful —
// the name changes when the bytes change, so a returning browser cannot run last week's
// bundle against this week's API.
//
// Deliberately small and dependency-light: esbuild bundles, Node hashes and rewrites.
// There is no dev server in the binary's path, so `go build` and `billing serve` keep
// working with no Node toolchain installed at all — the committed dist/ is the artefact.

import { build } from "esbuild";
import { createHash } from "node:crypto";
import { mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = dirname(fileURLToPath(import.meta.url));
const dist = join(root, "dist");
const watch = process.argv.includes("--watch");

const hash = (buf) => createHash("sha256").update(buf).digest("hex").slice(0, 12);

// One bundle, one stylesheet. Splitting into chunks would add round trips for a panel
// whose entire script is smaller than a single product image on most marketing sites.
const options = {
  entryPoints: [join(root, "src/main.ts")],
  bundle: true,
  format: "esm",
  target: ["es2022", "chrome100", "firefox100", "safari15"],
  minify: !watch,
  sourcemap: false,
  legalComments: "none",
  write: false,
  outdir: dist,
  entryNames: "app",
  assetNames: "[name]-[hash]",
  loader: { ".svg": "text" },
  logLevel: "silent",
};

// The push service worker.
//
// It is a second bundle rather than an entry point of the first because a service worker is not
// a module the page imports: it is a separate script with its own global scope, and it must be a
// classic script at a *stable* URL. `sw.js` is therefore written unhashed — a hashed name would
// make every deploy a different registration URL, which means installing a second worker beside
// the first instead of replacing it — and web.go serves a non-hashed file with `no-cache`, which
// is the correct policy for a script the browser checks for updates on navigation.
//
// It is built here, and not fetched at runtime, for the same reason the rest of the frontend is:
// the binary serves what it was compiled with, so `go build` works with no Node toolchain
// installed (dist/ is committed).
const workerOptions = {
  entryPoints: [join(root, "src/sw.ts")],
  bundle: true,
  format: "iife",
  target: ["es2022", "chrome100", "firefox100", "safari15"],
  minify: !watch,
  sourcemap: false,
  legalComments: "none",
  write: false,
  logLevel: "silent",
};

async function run() {
  const started = Date.now();
  const result = await build(options);

  await rm(dist, { recursive: true, force: true });
  await mkdir(join(dist, "assets"), { recursive: true });

  let jsName = "";
  let cssName = "";
  for (const file of result.outputFiles) {
    const bytes = Buffer.from(file.contents);
    const ext = file.path.endsWith(".css") ? "css" : "js";
    const name = `app.${hash(bytes)}.${ext}`;
    await writeFile(join(dist, "assets", name), bytes);
    if (ext === "js") jsName = name;
    else cssName = name;
  }
  if (!jsName) throw new Error("build produced no JavaScript bundle");

  // The favicon is referenced by a stable name: it is small, it changes rarely, and a
  // hashed name would only make the link in the HTML harder to read.
  await writeFile(join(dist, "assets", "mark.svg"), await readFile(join(root, "src/mark.svg")));

  // The service worker, at a stable URL (see workerOptions above).
  const worker = await build(workerOptions);
  const workerFile = worker.outputFiles.find((file) => file.path.endsWith(".js"));
  if (!workerFile) throw new Error("build produced no service worker");
  await writeFile(join(dist, "sw.js"), Buffer.from(workerFile.contents));

  const template = await readFile(join(root, "src/index.html"), "utf8");
  const html = template
    .replaceAll("{{STYLE}}", cssName ? `/assets/${cssName}` : "")
    .replaceAll("{{SCRIPT}}", `/assets/${jsName}`);

  // A stylesheet link with an empty href would be a request for the current document.
  const cleaned = cssName ? html : html.replace(/[ \t]*<link rel="stylesheet"[^>]*>\n?/, "");

  await writeFile(join(dist, "index.html"), cleaned);
  const size = (await readFile(join(dist, "assets", jsName))).length;
  console.log(
    `web: ${jsName} (${(size / 1024).toFixed(1)} KiB)` +
      (cssName ? `, ${cssName}` : "") +
      `, sw.js (${(workerFile.contents.length / 1024).toFixed(1)} KiB)` +
      ` in ${Date.now() - started}ms`,
  );
}

run().catch((err) => {
  console.error(err.message || err);
  process.exit(1);
});
