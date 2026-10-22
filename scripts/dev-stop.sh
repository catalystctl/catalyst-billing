#!/usr/bin/env bash
# Stop the development server this checkout started.
#
# Two things start one: `make dev`, which runs in the foreground, and scripts/dev-ui.sh, which
# starts it in the background. Both are a `bin/billing serve` running with this checkout as its
# working directory, and that is the only thing this script will kill — never a process matched
# by port number, and never a worker (which holds no address, and whose jobs are idempotent
# batches, so a lingering one is harmless while killing a peer session's would not be).
# This working directory is shared, so "whatever holds 8791" is not a safe way to choose.

set -euo pipefail

cd "$(dirname "$0")/.."

QUIET=0
if [[ "${1:-}" == "--quiet" ]]; then
  QUIET=1
fi

ADDR="${BILLING_DEV_ADDR:-127.0.0.1:8791}"
PID_FILE=/tmp/billing-dev.pid
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

if [[ -f "$PID_FILE" ]]; then
  pid="$(cat "$PID_FILE" 2>/dev/null || true)"
  if [[ -n "$pid" ]] && stop_pid "$pid"; then
    echo "dev-stop: stopped the server from $PID_FILE (pid $pid)"
    stopped=1
  fi
  rm -f "$PID_FILE"
fi

for pid in $(pgrep -x billing 2>/dev/null || true); do
  # Two conditions, both cheap and both precise: the process is a `serve` (a worker in this same
  # directory is a different thing and must survive), and its working directory is this checkout.
  # The command line alone is not enough, because it is relative when the script starts it.
  cmdline="$(tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null || true)"
  cwd="$(readlink "/proc/$pid/cwd" 2>/dev/null || true)"
  if [[ "$cwd" == "$PWD" && "$cmdline" == *"bin/billing serve"* ]] && stop_pid "$pid"; then
    echo "dev-stop: stopped bin/billing serve (pid $pid)"
    stopped=1
  fi
done

if [[ "$stopped" == "0" && "$QUIET" == "0" ]]; then
  echo "dev-stop: nothing to stop on $ADDR"
fi
