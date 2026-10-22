#!/usr/bin/env bash
# Stop the development server this checkout started, on one address.
#
# Two things start one: `make dev`, which runs in the foreground, and scripts/dev-ui.sh, which
# starts it in the background. The only process this script will kill is a `bin/billing serve`
# whose working directory is this checkout AND whose BILLING_HTTP_ADDR is the address asked
# for. Both conditions matter: this working directory is shared, several sessions run servers
# here, and a dev helper that kills "whatever holds 8791" eventually kills someone else's.
#
# A worker is never stopped. It holds no address, and its jobs are idempotent batches, so a
# lingering one is harmless — while killing a peer's would not be.

set -euo pipefail

cd "$(dirname "$0")/.."

QUIET=0
if [[ "${1:-}" == "--quiet" ]]; then
  QUIET=1
fi

ADDR="${BILLING_DEV_ADDR:-127.0.0.1:8791}"
PORT="${ADDR##*:}"
# One pid file per address: two servers on two addresses are a legitimate thing to want, and a
# single shared path would make the second one's cleanup delete the first one's record.
PID_FILE="/tmp/billing-dev-${PORT}.pid"
# scripts/dev-ui.sh predates this and writes a fixed path. It is still honoured, and still
# validated by address before anything is killed.
LEGACY_PID_FILE=/tmp/billing-dev.pid
stopped=0

stop_pid() {
  local pid="$1"
  if ! kill -0 "$pid" 2>/dev/null; then
    return 1
  fi
  kill "$pid" 2>/dev/null || true
  for _ in $(seq 1 60); do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 0.05
  done
  kill -9 "$pid" 2>/dev/null || true
}

# is_our_server reports whether a pid is a `bin/billing serve` from this checkout, on ADDR.
#
# The command line is relative when this script starts the process, so the working directory is
# what identifies the checkout; BILLING_HTTP_ADDR identifies which of its servers this is. A pid
# file alone is not enough — /tmp is shared, and a stale one from another address must not take
# a running server down with it.
is_our_server() {
  local pid="$1" cmdline cwd addr
  [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null || return 1
  cmdline="$(tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null || true)"
  cwd="$(readlink "/proc/$pid/cwd" 2>/dev/null || true)"
  [[ "$cwd" == "$PWD" && "$cmdline" == *"bin/billing serve"* ]] || return 1
  addr="$(tr '\0' '\n' <"/proc/$pid/environ" 2>/dev/null | sed -n 's/^BILLING_HTTP_ADDR=//p' || true)"
  [[ -z "$addr" || "$addr" == "$ADDR" ]]
}

for candidate in "$PID_FILE" "$LEGACY_PID_FILE"; do
  [[ -f "$candidate" ]] || continue
  pid="$(cat "$candidate" 2>/dev/null || true)"
  if is_our_server "$pid" && stop_pid "$pid"; then
    echo "dev-stop: stopped bin/billing serve on $ADDR (pid $pid)"
    stopped=1
  fi
  rm -f "$candidate"
done

for pid in $(pgrep -x billing 2>/dev/null || true); do
  if is_our_server "$pid" && stop_pid "$pid"; then
    echo "dev-stop: stopped bin/billing serve on $ADDR (pid $pid)"
    stopped=1
  fi
done

if [[ "$stopped" == "0" && "$QUIET" == "0" ]]; then
  echo "dev-stop: nothing to stop on $ADDR"
fi
