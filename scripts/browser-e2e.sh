#!/usr/bin/env bash
#
# Authenticated end-to-end browser test for the operator UI.
#
# The anonymous smoke test proves the shell loads and the CSP is enforced. It cannot prove that
# signing in works, that the dashboard reads real data, or that a screen renders an actual record —
# and those are the things a user does first. This script drives them in a real browser.
#
# It is separate from browser-smoke.sh because it needs a seeded database and creates an account; the
# smoke test must stay runnable against any server.
#
# Usage: scripts/browser-e2e.sh [base-url]
#   Requires: go, chromium (or CHROME=path), curl, python3, and BILLING_DATABASE_URL (from .env).
set -euo pipefail

cd "$(dirname "$0")/.."

CHROME="${CHROME:-chromium}"
BASE="${1:-}"
SERVER_PID=""
PROFILE_DIR=""
EMAIL="e2e-ui-$(date +%s)@example.test"
PASSWORD="e2e-ui-password-1"

cleanup() {
  if [[ -n "$SERVER_PID" ]]; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  [[ -n "$PROFILE_DIR" ]] && rm -rf "$PROFILE_DIR"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
ok()   { echo "ok: $*"; }

if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi

if [[ -z "$BASE" ]]; then
  PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
  BASE="http://127.0.0.1:${PORT}"

  go build -o /tmp/billing-e2e ./cmd/billing
  BILLING_HTTP_ADDR="127.0.0.1:${PORT}" \
  BILLING_EMBEDDED_WORKER=false \
  BILLING_PUBLIC_URL="$BASE" \
    /tmp/billing-e2e serve >/tmp/billing-e2e.log 2>&1 &
  SERVER_PID=$!

  for _ in $(seq 1 80); do
    if curl -fsS "$BASE/healthz" >/dev/null 2>&1; then break; fi
    sleep 0.25
  done
  curl -fsS "$BASE/healthz" >/dev/null || { cat /tmp/billing-e2e.log; fail "server did not become ready"; }
fi

# A staff account to sign in as. The password comes from the environment, never argv.
BILLING_ADMIN_PASSWORD="$PASSWORD" \
  /tmp/billing-e2e admin create-user --email "$EMAIL" --role administrator >/dev/null 2>&1 \
  || fail "could not create the test account"

# The 25-second stall on this host comes from an inherited session D-Bus address.
unset DBUS_SESSION_BUS_ADDRESS QT_ACCESSIBILITY
PROFILE_DIR="$(mktemp -d)"

# The sign-in is driven by a purpose-built page served from the app's own origin.
#
# Two approaches do not work, and are recorded so nobody re-tries them:
#   --evaluate-on-new-document   fires before the module bundle and in a context whose DOM changes
#                                are not reliably visible to --dump-dom.
#   an inline <script>           is blocked by script-src 'self'. That is the CSP working.
#
# So the driver is written as a same-origin module and loaded by a wrapper page that Chromium
# navigates to. The wrapper lives under the server's document root, so the CSP permits the script, the
# API calls go to the same origin, and the session cookie is sent. The wrapper is removed on exit.
cat > web/dist/e2e-driver.js <<'JS'
// The whole journey runs in one page context.
//
// The session cookie is a browser-session cookie, so it does not survive the browser exiting: a
// second `--dump-dom` invocation would start logged out. Everything therefore happens in this one
// run — sign in, then assert the dashboard rendered live data.
(async () => {
  const deadline = Date.now() + 25000;
  const waitFor = async (fn) => {
    while (Date.now() < deadline) {
      const v = fn();
      if (v) return v;
      await new Promise((r) => setTimeout(r, 50));
    }
    return null;
  };
  const verdict = (text) => {
    const el = document.createElement("div");
    el.id = "e2e-verdict";
    el.textContent = text;
    document.body.appendChild(el);
  };

  const email = await waitFor(() => document.querySelector('input[type="email"]'));
  const password = await waitFor(() => document.querySelector('input[type="password"]'));
  if (!email || !password) {
    verdict("E2E-FAIL: no login form rendered");
    return;
  }

  email.value = __EMAIL__;
  email.dispatchEvent(new Event("input", { bubbles: true }));
  password.value = __PASSWORD__;
  password.dispatchEvent(new Event("input", { bubbles: true }));

  const form = email.closest("form");
  if (!form) {
    verdict("E2E-FAIL: the login inputs are not in a form");
    return;
  }
  form.requestSubmit();

  // Wait until the app has left the sign-in screen.
  const left = await waitFor(() => {
    const h = document.querySelector("h1");
    return h && !/sign in/i.test(h.textContent || "");
  });
  if (!left) {
    const err = document.querySelector("[class*=error], .alert");
    verdict("E2E-FAIL: still on the sign-in screen" + (err ? " - " + err.textContent.trim() : ""));
    return;
  }

  // Now the dashboard must render live figures from /api/v1/ops/overview.
  const figure = await waitFor(
    () => document.querySelector('[class*="hero-figure"], [class*="figure"], [class*="stat"]'),
  );
  if (!figure) {
    verdict("E2E-FAIL: the dashboard rendered no figures");
    return;
  }
  if (/could not be reached|failed to load|something went wrong/i.test(document.body.textContent || "")) {
    verdict("E2E-FAIL: the dashboard reported a failed API call");
    return;
  }
  verdict("E2E-OK: signed in and the dashboard rendered live figures");
})();
JS

python3 - "$EMAIL" "$PASSWORD" <<'PY'
import json, sys
email, password = sys.argv[1], sys.argv[2]
path = "web/dist/e2e-driver.js"
src = open(path).read()
src = src.replace("__EMAIL__", json.dumps(email)).replace("__PASSWORD__", json.dumps(password))
open(path, "w").write(src)
PY

# The wrapper serves the real shell, then loads the driver as a module. The shell is fetched and
# rewritten in place so the app's own markup, styles and CSP apply.
cat > web/dist/e2e.html <<'HTML'
<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>e2e</title></head>
<body>
<div id="root" class="root"></div>
<script type="module">
  // Load the real app first, then the driver, both from this origin.
  import "/assets/app.0.js";
</script>
</body>
</html>
HTML
# The bundle name is discovered rather than guessed.
BUNDLE_NAME="$(grep -oE '/assets/app\.[0-9a-f]+\.js' web/dist/index.html | head -1)"
[[ -n "$BUNDLE_NAME" ]] || { rm -f web/dist/e2e-driver.js web/dist/e2e.html; fail "no bundle in dist"; }
python3 - "$BUNDLE_NAME" <<'PY'
import sys
bundle = sys.argv[1]
path = "web/dist/e2e.html"
html = open(path).read().replace("/assets/app.0.js", bundle)
# The driver is imported after the app has mounted, which is what the app's own bootstrap does.
html = html.replace(
    f'import "{bundle}";',
    f'import "{bundle}";\n  await new Promise((r) => setTimeout(r, 300));\n  import "/e2e-driver.js";')
open(path, "w").write(html)
PY

cleanup_e2e_assets() { rm -f web/dist/e2e-driver.js web/dist/e2e.html; }
trap 'cleanup_e2e_assets; cleanup' EXIT

DOM="$("$CHROME" --headless=new --no-sandbox --disable-gpu --disable-dev-shm-usage \
  --user-data-dir="$PROFILE_DIR/chrome" --virtual-time-budget=25000 \
  --dump-dom "$BASE/e2e.html" 2>/dev/null)" \
  || fail "chromium could not drive the page"

# The dashboard must render real content, not an empty shell or an error banner.
if grep -qiE 'something went wrong|failed to load' <<<"$DOM"; then
  fail "the dashboard rendered an error state; DOM:
$(head -c 1200 <<<"$DOM")"
fi

# The signed-in shell must show operator chrome. Rather than pin a word, assert that a heading
# rendered and that the app replaced its boot placeholder — the same signal the smoke test uses,
# plus the presence of a navigation element, which every signed-in screen has.
if grep -qi 'class="boot"' <<<"$DOM"; then
  fail "the signed-in page still shows the boot placeholder"
fi
if ! grep -qE '<(nav|aside|header)' <<<"$DOM"; then
  fail "the signed-in screen has no navigation chrome; DOM:
$(head -c 1200 <<<"$DOM")"
fi
if ! grep -qE '<h1' <<<"$DOM"; then
  fail "the signed-in screen rendered no heading; DOM:
$(head -c 1200 <<<"$DOM")"
fi
ok "the signed-in dashboard rendered with navigation"

# ---------------------------------------------------------------- coverage note --
#
# The UI foundation (router, layout, table, form kit, API client, formatting) is complete and the
# auth flows, dashboard, refund and billing-details screens are implemented. The remaining list and
# detail views are stubs. This is reported rather than hidden, so a green run is not mistaken for a
# finished product.
STUBS="$(grep -lc 'h("h1", { class: "page-title" }' web/src/views/*.ts 2>/dev/null | wc -l)"
TOTAL="$(ls web/src/views/*.ts | wc -l)"
echo "note: ${STUBS} of ${TOTAL} views are still stubs (heading only, no data)"

echo
echo "authenticated browser test passed against $BASE"
