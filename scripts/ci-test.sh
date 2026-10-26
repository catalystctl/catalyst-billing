#!/usr/bin/env bash
#
# ci-test.sh — run the Go suite as N balanced shards, each with its own database.
#
#   bash scripts/ci-test.sh --shard 1 --total 6
#   bash scripts/ci-test.sh --total 6 --list     # print the plan, run nothing
#   bash scripts/ci-test.sh --total 6 --verify   # prove the plan is complete
#
# # Why this exists
#
# `go test ./...` is not parallel *enough* at package granularity here.
# `internal/httpapi` holds 702 of the repo's 2,255 test functions and takes about
# 336s on its own, which is 92% of the wall clock of the entire suite: every
# other package is finished while httpapi is still going. Adding more packages
# to the pot cannot shorten that. The only thing that shortens it is cutting that
# one package into pieces that run on different machines.
#
# Tests inside a package cannot be parallelised. `internal/store/storetest` gives
# each test *package* its own database, and inside a package it takes an advisory
# lock and starts every test from truncated tables, so tests in one process are
# serial by construction — there is not a single `t.Parallel()` in the repo. The
# split therefore has to be across processes, which means across shards, each
# with its own PostgreSQL instance so that two slices of httpapi never share a
# database or a lock.
#
# Every other package still runs whole in exactly one shard: a package pays its
# database create-and-migrate once, and paying that twice costs more than the
# balancing gains.
#
# # The plan
#
# A work unit is either a whole package or one slice of `internal/httpapi`. Each
# unit carries a weight of `test functions + SETUP_COST`, where SETUP_COST stands
# in for the fixed per-package cost (create the database, migrate it, build the
# harness, close the pool). The weights only balance the shards; nothing here
# decides pass or fail. Units are packed longest-first onto the least loaded
# shard — the LPT approximation, which lands within a few percent of optimal for
# a list this size — and `--verify` proves the packing is complete and disjoint.
#
# Choices that look odd but are deliberate:
#
#   * `-vet=off`. `go test` runs its own subset of vet on every package as a
#     pretest; CI runs `go vet ./...` on its own in the lint job, so paying for
#     it again in every shard is pure duplication.
#   * `-count=1`. There is no result cache in CI and a stale cached PASS is the
#     one thing a money test suite must never report.
#   * `-json`, pid-free, into a file. The raw verbose log for 2,255 tests is
#     noise on success and exactly what you want on failure, and the same stream
#     is what the "no database test may skip" assertion reads, so the assertion
#     costs a second test run nothing.
#   * the httpapi slice and the rest of the shard run *concurrently*. They touch
#     different databases, so they cannot collide, and running them in sequence
#     would leave the slice as each shard's tail.
#
set -euo pipefail

# A test function is only ever named TestXxx at the start of a line. Enumerating
# them from source keeps the plan instant: `go test -list` would compile the
# package, and the plan is needed before anything should be compiled.
test_func_re='^func Test[A-Za-z0-9_]*('

# Rough seconds-equivalent of one package's fixed setup, in the same unit as a
# test function so the two can be added. A package with no tests of its own is
# excluded: it contributes no signal and `go vet ./...` already compiles it.
SETUP_COST="${SETUP_COST:-4}"

SPLIT_PKG="./internal/httpapi"

# Packages in which a skip means the invariants that matter most were not
# tested. Keep this in step with the old `grep '^--- SKIP'` step it replaces.
MUST_NOT_SKIP=(
  "github.com/catalyst/billing/internal/billing"
  "github.com/catalyst/billing/internal/store"
  "github.com/catalyst/billing/internal/e2e"
)

die() { printf '%s\n' "$*" >&2; exit 1; }

shard=0
total=0
mode=run
coverprofile=""

while [ $# -gt 0 ]; do
  case "$1" in
    --shard) shard="${2:?--shard needs a number}"; shift 2 ;;
    --total) total="${2:?--total needs a number}"; shift 2 ;;
    --coverprofile) coverprofile="${2:?--coverprofile needs a path}"; shift 2 ;;
    --list) mode=list; shift ;;
    --verify) mode=verify; shift ;;
    -h|--help) sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[ "$total" -ge 1 ] || die "--total must be at least 1"
case "$mode" in
  run)
    if [ "$shard" -lt 1 ] || [ "$shard" -gt "$total" ]; then die "--shard must be 1..$total"; fi
    ;;
  list)
    # 0 means "just the table"; anything else must name a real shard.
    if [ "$shard" -ne 0 ]; then
      if [ "$shard" -lt 1 ] || [ "$shard" -gt "$total" ]; then die "--shard must be 1..$total"; fi
    fi
    ;;
esac

# ---------------------------------------------------------------- the plan --

# split_tests prints internal/httpapi's test functions in a stable order.
split_tests() {
  local dir
  dir="$(go list -f '{{.Dir}}' "$SPLIT_PKG")"
  # shellcheck disable=SC2086
  grep -h "$test_func_re" "$dir"/*_test.go |
    sed 's/^func \(Test[A-Za-z0-9_]*\).*/\1/' |
    LC_ALL=C sort
}

# pkg_units prints one `weight<TAB>pkg<TAB>import-path` line per package that has
# test functions, excluding the package that is being split.
pkg_units() {
  local pkg dir n
  while IFS='|' read -r pkg dir; do
    [ "$pkg" = "github.com/catalyst/billing/internal/httpapi" ] && continue
    n=0
    if compgen -G "$dir/*_test.go" >/dev/null; then
      # Counting matches across many files, which is what wc is for here;
      # `grep -c` would return one count per file and need summing anyway.
      # shellcheck disable=SC2086,SC2126
      n=$(grep -h "$test_func_re" "$dir"/*_test.go | wc -l) || n=0
    fi
    [ "$n" -eq 0 ] && continue
    printf '%s\tpkg\t%s\n' "$((n + SETUP_COST))" "$pkg"
  done < <(go list -f '{{.ImportPath}}|{{.Dir}}' ./...)
}

# slice_for prints the `-run` regex for shard $1: every test name whose index is
# congruent to the shard, so the deal is even even when test costs cluster
# alphabetically.
slice_for() {
  local want="$1" idx=0 name out=""
  for name in "${split_names[@]}"; do
    if [ $((idx % total)) -eq $((want - 1)) ]; then
      out+="$name|"
    fi
    idx=$((idx + 1))
  done
  [ -n "$out" ] || return 0
  printf '^(%s)$' "${out%|}"
}

# plan prints the whole plan as `shard<TAB>weight<TAB>kind<TAB>payload` lines.
# httpapi slices are pinned to a shard each; everything else is packed by weight.
plan() {
  local i regex weight forced units
  forced=""
  for i in $(seq 1 "$total"); do
    regex="$(slice_for "$i")"
    [ -n "$regex" ] || continue
    weight=$(( $(printf '%s' "$regex" | tr '|' '\n' | wc -l) + SETUP_COST ))
    forced+="$i	$weight	httpapi	$regex"$'\n'
  done
  forced="${forced%$'\n'}"
  units="$(pkg_units)"
  {
    [ -n "$forced" ] && printf '%s\n' "$forced"
    [ -n "$units" ] && printf '%s\n' "$units"
  } | awk -v T="$total" '
      BEGIN { FS = "\t" }
      NF == 4 { load[$1] += $2; pinned[++p] = $0; next }
      NF == 3 { unit[++n] = $0; next }
      END {
        # LPT: heaviest first, onto the least loaded shard.
        for (i = 1; i <= n; i++) { split(unit[i], f, "\t"); w[i] = f[1] + 0; l[i] = unit[i] }
        for (i = 2; i <= n; i++) {
          kw = w[i]; kl = l[i]; j = i - 1
          while (j >= 1 && w[j] < kw) { w[j+1] = w[j]; l[j+1] = l[j]; j-- }
          w[j+1] = kw; l[j+1] = kl
        }
        for (i = 1; i <= n; i++) {
          best = 1
          for (s = 2; s <= T; s++) if (load[s] < load[best]) best = s
          split(l[i], f, "\t"); load[best] += f[1]
          printf "%d\t%s\t%s\t%s\n", best, f[1], f[2], f[3]
        }
        for (i = 1; i <= p; i++) print pinned[i]
      }'
}

mapfile -t split_names < <(split_tests)
[ "${#split_names[@]}" -gt 0 ] || die "no test functions found in $SPLIT_PKG"
all_plan="$(plan)"

# ------------------------------------------------------------------- checks --

# verify proves the plan covers the suite exactly once. A shard plan that
# silently drops a package is the worst kind of green build, so this is a hard
# gate, not a warning.
if [ "$mode" = verify ]; then
  expected_pkgs="$(pkg_units | cut -f3 | LC_ALL=C sort)"
  planned_pkgs="$(printf '%s\n' "$all_plan" | awk -F'\t' '$3 == "pkg" { print $4 }' | LC_ALL=C sort)"

  if [ "$expected_pkgs" != "$planned_pkgs" ]; then
    die "verify: the plan does not cover every package with tests
--- expected ---
$expected_pkgs
--- planned ---
$planned_pkgs"
  fi
  dupes="$(printf '%s\n' "$planned_pkgs" | uniq -d)"
  [ -z "$dupes" ] || die "verify: packages assigned to more than one shard: $dupes"

  # Every test function of the split package must appear in exactly one slice.
  # A slice is `^(TestA|TestB|...)$`, so unwrap it and split on the alternation.
  planned_tests="$(printf '%s\n' "$all_plan" | awk -F'\t' '$3 == "httpapi" { print $4 }' |
    while IFS= read -r slice; do
      slice="${slice#'^('}"
      slice="${slice%')$'}"
      printf '%s\n' "${slice//|/$'\n'}"
    done | LC_ALL=C sort)"
  if [ "$planned_tests" != "$(printf '%s\n' "${split_names[@]}")" ]; then
    die "verify: the httpapi slices are not a partition of its test functions"
  fi
  dupes="$(printf '%s\n' "$planned_tests" | uniq -d)"
  [ -z "$dupes" ] || die "verify: httpapi tests in more than one slice: $dupes"

  printf 'verify: %d packages + %d %s test functions, partitioned across %d shards\n' \
    "$(printf '%s\n' "$planned_pkgs" | wc -l)" "${#split_names[@]}" "$SPLIT_PKG" "$total"
  printf '%s\n' "$all_plan" |
    awk -F'\t' '{ w[$1] += $2; n[$1]++; if ($3 == "httpapi") h[$1] = 1 }
      END {
        for (s = 1; s <= '"$total"'; s++)
          printf "  shard %d: weight %4d, %2d packages%s\n", s, w[s], n[s], (h[s] ? " + httpapi slice" : "")
      }'
  exit 0
fi

mine="$(printf '%s\n' "$all_plan" | awk -F'\t' -v s="$shard" '$1 == s')"

# --list shows the same table plus, with --shard, what that shard would run.
if [ "$mode" = list ]; then
  printf '%s\n' "$all_plan" |
    awk -F'\t' '{ w[$1] += $2; n[$1]++; if ($3 == "httpapi") h[$1] = 1 }
      END {
        for (s = 1; s <= '"$total"'; s++)
          printf "shard %d: weight %4d, %2d packages%s\n", s, w[s], n[s], (h[s] ? " + httpapi slice" : "")
      }'
  if [ -n "$mine" ]; then
    printf '\nshard %d units:\n' "$shard"
    printf '%s\n' "$mine" | awk -F'\t' '{ printf "  %-8s %4s  %s\n", $3, $2, substr($4, 1, 100) }'
  fi
  exit 0
fi

# --------------------------------------------------------------------- run ---

shard_pkgs="$(printf '%s\n' "$mine" | awk -F'\t' '$3 == "pkg" { print $4 }' | tr '\n' ' ')"
shard_regex="$(printf '%s\n' "$mine" | awk -F'\t' '$3 == "httpapi" { print $4 }')"
shard_weight="$(printf '%s\n' "$mine" | awk -F'\t' '{ s += $2 } END { print s + 0 }')"

command -v jq >/dev/null || die "jq is required to read the test stream"
[ -n "$shard_pkgs" ] || [ -n "$shard_regex" ] || die "shard $shard has no work: --total is larger than the plan"

flags=(-count=1 -vet=off -timeout "${GO_TEST_TIMEOUT:-10m}")
[ "${GO_TEST_RACE:-1}" = "0" ] || flags+=(-race)

logs="$(mktemp -d)"
trap 'rm -rf "$logs"' EXIT

printf 'shard %d/%d: weight %d, %d packages%s\n' \
  "$shard" "$total" "$shard_weight" \
  "$(printf '%s' "$shard_pkgs" | wc -w)" \
  "$([ -n "$shard_regex" ] && echo ", httpapi slice" || echo "")"

status=0
pids=()

if [ -n "$shard_regex" ]; then
  # shellcheck disable=SC2086
  ( go test "$SPLIT_PKG" -run "$shard_regex" "${flags[@]}" \
      ${coverprofile:+-covermode=atomic -coverprofile="$logs/httpapi.cov"} \
      -json >"$logs/httpapi.json" 2>&1 ) &
  pids+=($!)
fi
if [ -n "$shard_pkgs" ]; then
  # shellcheck disable=SC2086
  ( go test $shard_pkgs "${flags[@]}" \
      ${coverprofile:+-covermode=atomic -coverprofile="$logs/pkgs.cov"} \
      -json >"$logs/pkgs.json" 2>&1 ) &
  pids+=($!)
fi
for pid in "${pids[@]}"; do
  wait "$pid" || status=1
done

cat "$logs"/*.json >"$logs/all.json" 2>/dev/null || true

# Per-package result lines, in the shape `go test` would have printed them.
printf '%s\n' "$(jq -r '
  select((.Action == "pass" or .Action == "fail") and .Test == null)
  | "\(if .Action == "fail" then "FAIL" else "ok  " end)\t\(.Package)\t\(.Elapsed // 0 | tostring)s"' \
  "$logs/all.json" | LC_ALL=C sort)"

# On failure, the whole verbose stream: the failing test's own output is the
# only thing worth reading, and nothing else is printed on success.
if [ "$status" -ne 0 ]; then
  printf '\n--- output of the tests that ran in this shard ---\n'
  jq -r 'select(.Action == "output") | .Output' "$logs/all.json" | sed '/^$/d'
fi

# A skip is reported as a note, and is a failure in the packages where a skip
# would mean the money invariants were not exercised at all.
skips="$(jq -r 'select(.Action == "skip" and .Test != null) | "\(.Package)\t\(.Test)"' "$logs/all.json")"
if [ -n "$skips" ]; then
  printf '\n%d test(s) skipped in this shard:\n' "$(printf '%s\n' "$skips" | wc -l)"
  printf '%s\n' "$skips" | sed 's/^/  /'
  for guarded in "${MUST_NOT_SKIP[@]}"; do
    hit="$(printf '%s\n' "$skips" | awk -F'\t' -v p="$guarded" '$1 == p')"
    if [ -n "$hit" ]; then
      printf '\nFAIL: a database-backed test skipped, so a green build would not mean anything:\n%s\n' "$hit" >&2
      status=1
    fi
  done
fi

if [ -n "$coverprofile" ]; then
  # The two halves of a shard are separate profiles; concatenating them (with
  # the duplicate `mode:` header dropped) is how Go merges text profiles.
  {
    head -1 "$logs"/pkgs.cov 2>/dev/null || head -1 "$logs"/httpapi.cov
    cat "$logs"/pkgs.cov "$logs"/httpapi.cov 2>/dev/null | grep -v '^mode:' || true
  } >"$coverprofile"
  printf 'coverage profile: %s\n' "$coverprofile"
fi

exit "$status"
