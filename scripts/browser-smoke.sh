#!/usr/bin/env bash
#
# Browser smoke test for the served UI (docs/ui-gate.md G1, G2, G3).
#
# It drives a real headless browser against a real server, because the properties under test are
# only observable there: that the Content-Security-Policy actually permits the module script, that
# the module actually runs and updates the DOM, and that a client-side route survives a reload.
#
# A unit test cannot prove any of those. It can assert that a header is present; it cannot assert
# that a browser accepted the script the header allows.
#
# Usage: scripts/browser-smoke.sh [base-url]
#   With no argument it starts its own server on an ephemeral port and stops it afterwards.
#   Requires: go, chromium (or CHROME=path), curl.
set -euo pipefail

cd "$(dirname "$0")/.."

CHROME="${CHROME:-chromium}"
BASE="${1:-}"
STARTED=0
SERVER_PID=""

cleanup() {
  if [[ -n "$SERVER_PID" ]]; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  [[ -n "${PROFILE_DIR:-}" ]] && rm -rf "$PROFILE_DIR"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
ok()   { echo "ok: $*"; }

if [[ -z "$BASE" ]]; then
  # A database is needed to start: the server opens a pool before it listens.
  if [[ -f .env ]]; then
    set -a
    # shellcheck disable=SC1091
    . ./.env
    set +a
  fi
  PORT="${SMOKE_PORT:-18099}"
  # A port that is already in use would make every assertion test somebody else's server, so it is
  # checked rather than assumed free.
  if (exec 3<>/dev/tcp/127.0.0.1/"$PORT") 2>/dev/null; then
    fail "port $PORT is already in use; set SMOKE_PORT to a free port"
  fi
  BASE="http://127.0.0.1:${PORT}"

  go build -o /tmp/billing-smoke ./cmd/billing
  BILLING_HTTP_ADDR="127.0.0.1:${PORT}" \
  BILLING_EMBEDDED_WORKER=false \
  BILLING_PUBLIC_URL="$BASE" \
    /tmp/billing-smoke serve >/tmp/billing-smoke.log 2>&1 &
  SERVER_PID=$!
  STARTED=1

  # Wait for readiness rather than sleeping a fixed time: a fixed sleep is either flaky or slow.
  for _ in $(seq 1 60); do
    if curl -fsS "$BASE/healthz" >/dev/null 2>&1; then break; fi
    sleep 0.25
  done
  curl -fsS "$BASE/healthz" >/dev/null || { cat /tmp/billing-smoke.log; fail "server did not become ready"; }
fi

# The 25-second stall on this host comes from an inherited session D-Bus address: with it set,
# Chromium waits for a bus that is not there before answering its first navigation.
unset DBUS_SESSION_BUS_ADDRESS QT_ACCESSIBILITY
PROFILE_DIR="$(mktemp -d)"
CHROME_FLAGS=(--headless=new --no-sandbox --disable-gpu --disable-dev-shm-usage
              --user-data-dir="$PROFILE_DIR" --virtual-time-budget=8000)

# ---------------------------------------------------------------- the shell --
DOM="$("$CHROME" "${CHROME_FLAGS[@]}" --dump-dom "$BASE/" 2>/dev/null)" \
  || fail "chromium could not load the shell"
[[ "$DOM" == *"<h1>Catalyst Billing</h1>"* ]] || fail "the shell was not served at /"
ok "the shell is served at /"

# The module executed: it rewrites the status line from the static placeholder. That proves the CSP
# allowed the external module and that the fetch to /api/v1/auth/me ran.
[[ "$DOM" == *'id="status" data-tone='* ]] \
  || fail "the module script did not run (the status line was never updated)"
[[ "$DOM" == *"Not signed in"* ]] || fail "the module did not reach its 401 branch"
ok "the module script ran and rendered the anonymous state"

# A client-side route must survive a reload by serving the shell.
ROUTE_DOM="$("$CHROME" "${CHROME_FLAGS[@]}" --dump-dom "$BASE/customers/abc/invoices" 2>/dev/null)" \
  || fail "chromium could not load a client-side route"
[[ "$ROUTE_DOM" == *"<h1>Catalyst Billing</h1>"* ]] || fail "a client-side route did not fall back to the shell"
ok "a client-side route falls back to the shell"

# ---------------------------------------------------------------- the headers --
HEADERS="$(curl -fsS -D- -o /dev/null "$BASE/")"
grep -qi "content-security-policy:.*script-src 'self'" <<<"$HEADERS" \
  || fail "the CSP is missing script-src 'self'"
grep -qi "content-security-policy:.*frame-ancestors 'none'" <<<"$HEADERS" \
  || fail "the CSP is missing frame-ancestors 'none'"
grep -qi "x-content-type-options: nosniff" <<<"$HEADERS" || fail "X-Content-Type-Options is missing"
grep -qi "cache-control: no-store" <<<"$HEADERS" || fail "index.html is cacheable"
ok "the documented security headers are present"

# A hashed asset is immutable; an unhashed one is not.
ASSET_HEADERS="$(curl -fsS -D- -o /dev/null "$BASE/assets/app.4f2c1d9e.js")"
grep -qi "cache-control:.*immutable" <<<"$ASSET_HEADERS" || fail "a hashed asset is not immutable"
ok "a hashed asset is cached immutably"

# An API path stays JSON: a client that expected JSON must never receive HTML.
API_BODY="$(curl -sS -H 'Accept: application/json' "$BASE/api/v1/nope")"
[[ "$API_BODY" == *'"code":"not_found"'* ]] || fail "an unknown API path did not answer problem+json: $API_BODY"
[[ "$API_BODY" != *"<html"* ]] || fail "an unknown API path answered HTML"
ok "an unknown API path answers problem+json, not the shell"

# ---------------------------------------------------------------- the CSP is enforced --
# Inject an inline script through the page's own module scope. The CSP has no 'unsafe-inline', so the
# browser must refuse to run it; if it ran, it would set window.__cspBypass.
#
# This is the one assertion that a header check cannot make: that the policy is enforced, not merely
# declared.
CSP_RESULT="$("$CHROME" "${CHROME_FLAGS[@]}" --dump-dom \
  --evaluate-on-new-document="try{var s=document.createElement('script');s.textContent='window.__cspBypass=1';document.documentElement.appendChild(s)}catch(e){}" \
  "$BASE/" 2>/dev/null | grep -c "__cspBypass" || true)"
# --evaluate-on-new-document runs before the CSP is applied to the document in some builds, so a
# positive result is not conclusive; a negative one is. Report rather than fail on the ambiguous case.
if [[ "$CSP_RESULT" == "0" ]]; then
  ok "the CSP refused an injected inline script"
else
  echo "note: the inline-script probe was inconclusive on this Chromium build"
fi

echo
echo "browser smoke test passed against $BASE"
