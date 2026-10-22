#!/usr/bin/env bash
# Run the panel for development.
#
# `make dev` is the entry point. By the time this runs the database is up, the schema is
# current, the binary and the extensions are built, and the fixture configuration and a staff
# account are seeded. This script's only job is to start the processes with the development
# environment, say where to look, and stop cleanly.
#
# It runs in the *foreground*: the log is the terminal, and Ctrl-C stops it. A dev server you
# cannot watch is a dev server you debug by guessing.
#
# The two things that make a fresh installation look broken, both handled here:
#
#   * the session cookie is Secure by default, and a browser will not send it back over plain
#     http — sign-in appears to succeed and every later request is unauthenticated. On loopback
#     the Secure flag is turned off, which .env.example documents as the one legitimate case.
#   * BILLING_PUBLIC_URL is the origin every mutation is checked against, so it must be the
#     exact URL a browser types, port included. It is derived from the address the server
#     actually binds rather than left to drift from it.

set -euo pipefail

cd "$(dirname "$0")/.."

ADDR="${BILLING_DEV_ADDR:-127.0.0.1:8791}"
PORT="${ADDR##*:}"
PID_FILE=/tmp/billing-dev.pid
# The same defaults scripts/dev-config.sh uses to seed the account, so the banner cannot name an
# account that was never created.
DEV_ADMIN_EMAIL="${DEV_ADMIN_EMAIL:-admin@dev.local}"
DEV_ADMIN_PASSWORD="${DEV_ADMIN_PASSWORD:-dev-only-password-1}"

for binary in billing fakegateway fakeprov; do
  if [[ ! -x "./bin/$binary" ]]; then
    echo "dev: ./bin/$binary is missing; run 'make build extensions'" >&2
    exit 1
  fi
done

# A server left behind by an earlier run — a `make dev` whose terminal was closed, or the
# background one scripts/dev-ui.sh starts — would fail this one with "address in use" after a
# confusing delay. dev-stop.sh stops only this checkout's own `bin/billing serve`, and says so.
bash scripts/dev-stop.sh --quiet
if (exec 3<>"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null; then
  echo "dev: 127.0.0.1:$PORT is already in use by a process this checkout did not start." >&2
  echo "     Stop it, or use another address: BILLING_DEV_ADDR=127.0.0.1:8792 make dev" >&2
  exit 1
fi

set -a
# shellcheck disable=SC1091
. ./.env
set +a

export BILLING_HTTP_ADDR="$ADDR"
export BILLING_PUBLIC_URL="http://$ADDR"

# The fixtures. Both take no credentials, so a dev installation can prove the whole path —
# checkout, webhook, provisioning — without an account anywhere.
export BILLING_GATEWAY_EXTENSION="$PWD/bin/fakegateway"
export BILLING_PROVISIONER_EXTENSION="$PWD/bin/fakeprov"
export BILLING_GATEWAY_ACCOUNT="${BILLING_GATEWAY_ACCOUNT:-acct_test}"
# The provider name is set here as well as in the settings table, and both are needed: the
# panel and the jobs resolve it from the table (where the environment would otherwise win),
# but the HTTP server builds its webhook verifier from the environment alone at startup. With
# only the row, every delivery is refused with "no webhook verifier configured".
export BILLING_GATEWAY_PROVIDER="${BILLING_GATEWAY_PROVIDER:-fake}"
# The core verifies a delivery with this. Without it every webhook is rejected, which is the
# correct default for an unconfigured panel and a puzzling one on a workstation.
export BILLING_WEBHOOK_SECRET="${BILLING_WEBHOOK_SECRET:-whsec_dev}"
export BILLING_INSECURE_COOKIES="${BILLING_INSECURE_COOKIES:-1}"

# One process by default: the fixture gateway keeps its payments in memory, so a checkout
# created by the HTTP server and the job that settles it must be the same child process.
# DEV_SEPARATE_WORKER=1 runs the jobs the way production does, in their own process.
SEPARATE_WORKER="${DEV_SEPARATE_WORKER:-0}"

pids=()
cleanup() {
  trap - INT TERM EXIT
  for pid in "${pids[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
  for pid in "${pids[@]}"; do
    wait "$pid" 2>/dev/null || true
  done
  rm -f "$PID_FILE"
}
trap cleanup INT TERM EXIT

if [[ "$SEPARATE_WORKER" == "1" || "$SEPARATE_WORKER" == "true" ]]; then
  ./bin/billing serve &
  pids+=("$!")
  echo "$!" >"$PID_FILE"
  ./bin/billing worker &
  pids+=("$!")
  jobs_mode="separate processes (serve + worker)"
else
  ./bin/billing serve --embedded-worker &
  pids+=("$!")
  echo "$!" >"$PID_FILE"
  jobs_mode="embedded in the serve process"
fi

for _ in $(seq 1 150); do
  if curl -fsS -o /dev/null "http://$ADDR/healthz" 2>/dev/null; then
    break
  fi
  if ! kill -0 "${pids[0]}" 2>/dev/null; then
    echo "dev: the server exited during startup; its last lines are above" >&2
    exit 1
  fi
  sleep 0.1
done

cat <<EOF

  panel      http://$ADDR
  sign in    $DEV_ADMIN_EMAIL / $DEV_ADMIN_PASSWORD   (dev fixture account)
  gateway    fake  (bin/fakegateway, webhook POST /webhooks/fake)
  provision  fake  (bin/fakeprov)
  jobs       $jobs_mode
  database   ${BILLING_DATABASE_URL%%\?*}
  stop       Ctrl-C

  Next, from another terminal — . ./.env first:

    ./bin/billing admin create-customer --email a@example.test --name "A"
    ./bin/billing admin issue-invoice --customer <id> --amount 12.00 --tax-rate 20%

  Everything the panel can do is also in docs/operations.md.

EOF

wait
