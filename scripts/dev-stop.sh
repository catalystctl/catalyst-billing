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
  # The command line is relative when the script starts the process, so the working directory is
  # what identifies the checkout; the address identifies which of its servers this is.
  cmdline="$(tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null || true)"
  cwd="$(readlink "/proc/$pid/cwd" 2>/dev/null || true)"
  if [[ "$cwd" != "$PWD" || "$cmdline" != *"bin/billing serve"* ]]; then
    continue
  fi
  addr="$(tr '\0' '\n' <"/proc/$pid/environ" 2>/dev/null | sed -n 's/^BILLING_HTTP_ADDR=//p' || true)"
  if [[ -n "$addr" && "$addr" != "$ADDR" ]]; then
    continue
  fi
  if stop_pid "$pid"; then
    echo "dev-stop: stopped bin/billing serve on $ADDR (pid $pid)"
    stopped=1
  fi
done

if [[ "$stopped" == "0" && "$QUIET" == "0" ]]; then
  echo "dev-stop: nothing to stop on $ADDR"
fi
