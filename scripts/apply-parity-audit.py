#!/usr/bin/env python3
"""Apply the audited verdicts from docs/parity-audit/*.tsv into docs/paymenter-parity.md.

The audit is split across agents so it can be done in parallel, and agents must not edit one shared
document. Each writes a TSV of `ID<TAB>MARK<TAB>Catalyst today text`, and this script is the single
place the document is written. That is deliberate: N agents editing one table is N merge conflicts,
and a conflict resolved by hand in a status column is exactly how a document starts lying.

Usage:
    python3 scripts/apply-parity-audit.py            # apply, then report
    python3 scripts/apply-parity-audit.py --dry-run  # report only, change nothing
"""

from __future__ import annotations

import argparse
import collections
import glob
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DOC = os.path.join(ROOT, "docs", "paymenter-parity.md")
AUDIT = os.path.join(ROOT, "docs", "parity-audit")

# The four marks the document's legend uses. Kept as escapes so this file survives any locale.
MARKS = {
    "done": "\u2705",
    "partial": "\U0001f7e1",
    "absent": "\u274c",
    "deliberate": "\u26d4",
}
MARK_CHARS = set(MARKS.values())

# A row is `| ID | feature | catalyst today | mark | phase |`. The feature text may contain escaped
# pipes (\|), so the split is on unescaped pipes only.
ROW = re.compile(r"^\|\s*([A-Z]+-\d+)\s*\|")


def split_row(line: str) -> list[str] | None:
    """Split a markdown table row on unescaped pipes, dropping the leading/trailing empties."""
    cells: list[str] = []
    buf: list[str] = []
    i = 0
    while i < len(line):
        ch = line[i]
        if ch == "\\" and i + 1 < len(line) and line[i + 1] == "|":
            buf.append("\\|")
            i += 2
            continue
        if ch == "|":
            cells.append("".join(buf))
            buf = []
            i += 1
            continue
        buf.append(ch)
        i += 1
    cells.append("".join(buf))
    # The row starts and ends with a pipe, so the outer cells are empty.
    if cells and cells[0].strip() == "":
        cells = cells[1:]
    if cells and cells[-1].strip() == "":
        cells = cells[:-1]
    return cells if len(cells) >= 4 else None


def join_row(cells: list[str]) -> str:
    return "|" + "|".join(cells) + "|"


def load_verdicts() -> tuple[dict[str, tuple[str, str]], list[str]]:
    """Read every audit TSV. Later files cannot silently overwrite an earlier verdict for one ID."""
    verdicts: dict[str, tuple[str, str]] = {}
    problems: list[str] = []
    for path in sorted(glob.glob(os.path.join(AUDIT, "*.tsv"))):
        name = os.path.basename(path)
        with open(path, encoding="utf-8") as fh:
            for lineno, raw in enumerate(fh, 1):
                line = raw.rstrip("\n")
                if not line.strip():
                    continue
                parts = line.split("\t")
                if len(parts) != 3:
                    problems.append(f"{name}:{lineno}: expected 3 tab-separated columns, got {len(parts)}")
                    continue
                ident, mark, text = (p.strip() for p in parts)
                if mark not in MARK_CHARS:
                    problems.append(f"{name}:{lineno}: {ident} has mark {mark!r}, not one of the four status marks")
                    continue
                if not text:
                    problems.append(f"{name}:{lineno}: {ident} has empty text")
                    continue
                if "|" in text:
                    # An unescaped pipe would split the table cell.
                    text = text.replace("|", "\\|")
                if ident in verdicts:
                    problems.append(f"{name}:{lineno}: {ident} already verdicted by another file")
                    continue
                verdicts[ident] = (mark, text)
    return verdicts, problems


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    verdicts, problems = load_verdicts()
    if not verdicts:
        print("no audit TSVs found in docs/parity-audit/", file=sys.stderr)
        return 1

    with open(DOC, encoding="utf-8") as fh:
        lines = fh.read().split("\n")

    seen: set[str] = set()
    changed = 0
    before = collections.Counter()
    after = collections.Counter()
    unknown: list[str] = []

    for idx, line in enumerate(lines):
        m = ROW.match(line)
        if not m:
            continue
        ident = m.group(1)
        cells = split_row(line)
        if cells is None:
            continue
        old_mark = cells[3].strip()
        if old_mark in MARK_CHARS:
            before[old_mark] += 1
        if ident not in verdicts:
            unknown.append(ident)
            if old_mark in MARK_CHARS:
                after[old_mark] += 1
            continue
        mark, text = verdicts[ident]
        seen.add(ident)
        cells[2] = f" {text} "
        cells[3] = f" {mark} "
        new_line = join_row(cells)
        if new_line != line:
            changed += 1
        lines[idx] = new_line
        after[mark] += 1

    # An ID in a TSV that the document does not contain is a typo somewhere, and worth failing on.
    extra = sorted(set(verdicts) - seen)
    if extra:
        problems.append(f"{len(extra)} verdicted ID(s) are not rows in the document: {', '.join(extra[:10])}")

    names = {v: k for k, v in MARKS.items()}
    total = sum(after.values())
    print(f"rows updated: {changed}")
    print(f"verdicts applied: {len(seen)} of {len(verdicts)} supplied")
    print("marks after:", ", ".join(f"{names.get(k, k)} {k} = {v}" for k, v in sorted(after.items())), f"(total {total})")
    if before != after:
        print("marks before:", ", ".join(f"{names.get(k, k)} {k} = {v}" for k, v in sorted(before.items())))
    if unknown:
        print(f"WARNING: {len(unknown)} row(s) have no verdict yet: {', '.join(unknown[:12])}")
    for p in problems:
        print("PROBLEM:", p, file=sys.stderr)

    if args.dry_run:
        print("dry run: nothing written")
        return 0 if not problems else 1

    with open(DOC, "w", encoding="utf-8") as fh:
        fh.write("\n".join(lines))
    print(f"wrote {os.path.relpath(DOC, ROOT)}")
    return 0 if not problems else 1


if __name__ == "__main__":
    raise SystemExit(main())
