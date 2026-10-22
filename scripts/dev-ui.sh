#!/usr/bin/env bash
# Rebuild the binary and restart the local dev server.
#
# The frontend is embedded with //go:embed, so a change to web/dist is invisible until the Go
# binary is rebuilt. Forgetting that step looks exactly like a broken page: the old bundle
# renders the old screen, and the new source is never reached. This script exists so the two
# steps cannot drift apart while working on the UI.

set -euo pipefail

cd "$(dirname "$0")/.."

ADDR="${BILLING_DEV_ADDR:-127.0.0.1:8791}"
PORT="${ADDR##*:}"

echo "web: building"
(cd web && node build.mjs)

echo "go:  building"
go build -o bin/billing ./cmd/billing

# Stop only the server this script started, matched by the address it listens on, so a
# developer's other instance on another port is left alone.
if [[ -f /tmp/billing-dev.pid ]]; then
  old="$(cat /tmp/billing-dev.pid)"
  if kill -0 "$old" 2>/dev/null; then
    kill "$old" 2>/dev/null || true
    for _ in $(seq 1 40); do
      kill -0 "$old" 2>/dev/null || break
      sleep 0.05
    done
  fi
  rm -f /tmp/billing-dev.pid
fi

set -a
# shellcheck disable=SC1091
. ./.env
set +a

export BILLING_HTTP_ADDR="$ADDR"
export BILLING_PUBLIC_URL="http://$ADDR"
# The worker is a separate process in production; here it would compete for the same queue
# while a test is trying to observe it.
export BILLING_EMBEDDED_WORKER="${BILLING_EMBEDDED_WORKER:-false}"

setsid nohup ./bin/billing serve >/tmp/billing-dev.log 2>&1 </dev/null &
echo $! >/tmp/billing-dev.pid

for _ in $(seq 1 100); do
  if curl -fsS -o /dev/null "http://$ADDR/healthz" 2>/dev/null; then
    echo "dev: serving http://$ADDR"
    exit 0
  fi
  sleep 0.1
done

echo "dev: server did not become healthy; last log lines:" >&2
tail -20 /tmp/billing-dev.log >&2
exit 1
