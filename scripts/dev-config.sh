#!/usr/bin/env bash
# Point a development installation at the fixture gateway and provisioner, and make sure there
# is a staff account to sign in with.
#
# `make dev` runs this; it is idempotent, so running it again changes nothing but the
# updated_at of the rows it writes.
#
# This is a development fixture command, not a deployment step: it names the conformance
# fixtures, which settle nothing and create nothing. Two of the three settings rows have no
# environment equivalent, which is why they have to be written at all:
#
#   * servers.provider — a deployment names its provisioner by the binary
#     BILLING_PROVISIONER_EXTENSION points at, and the *panel* names it here. Without this row
#     the provisioning job fails every cycle with "missing required configuration:
#     BILLING_PROVISIONER_EXTENSION" even though that variable is set. That is the single most
#     confusing thing about a fresh dev database.
#   * gateway.enabled — payments are off by default, because a gateway that starts taking money
#     on first boot is a surprise rather than a feature.

set -euo pipefail

cd "$(dirname "$0")/.."

if [[ ! -x ./bin/billing ]]; then
  echo "dev-config: ./bin/billing is missing; run make build" >&2
  exit 1
fi

set -a
# shellcheck disable=SC1091
. ./.env
set +a

DEV_ADMIN_EMAIL="${DEV_ADMIN_EMAIL:-admin@dev.local}"
DEV_ADMIN_PASSWORD="${DEV_ADMIN_PASSWORD:-dev-only-password-1}"
DEV_ADMIN_NAME="${DEV_ADMIN_NAME:-Dev Operator}"

setting() {
  # The CLI prints a note about the audit row's actor on every write. It is honest, and it is
  # noise in a script that runs on every `make dev`, so it is dropped and the value is echoed.
  ./bin/billing admin setting set "$1" "$2" "$3" 2>&1 | sed -n 's/^set /dev: set /p'
}

# The fixture provisioner is a default, not a policy.
#
# `make dev` runs this on every start. On an installation where somebody has pointed the panel at a real
# provisioner — which is exactly what an operator does to verify a live integration — overwriting that with
# `fake` is not a harmless dev convenience: every service then reports "The server panel does not recognise
# this service", because the fixture never created any of them. That happened, and it took a while to
# diagnose because the message describes the customer's server, not the installation.
#
# So the row is written only when it has not been set, or when it is already the fixture. Anything else is
# left alone and said out loud. DEV_FORCE_FIXTURES=1 restores the old unconditional behaviour.
current_provider="$(./bin/billing admin setting get servers provider 2>/dev/null || true)"
case "$current_provider" in
  ""|fake)
    setting servers provider fake
    ;;
  *)
    if [ "${DEV_FORCE_FIXTURES:-0}" = "1" ]; then
      setting servers provider fake
    else
      echo "dev: leaving servers.provider=$current_provider alone (a real provisioner is configured)"
      echo "dev: set DEV_FORCE_FIXTURES=1 to switch this database back to the fixtures"
    fi
    ;;
esac

setting gateway provider stripe
setting gateway enabled true

# A staff account. `admin create-user` refuses a duplicate, which is the behaviour that matters
# in production; here the refusal means "already seeded", so it is not an error. Any other
# failure is: a silent skip would leave the panel with no way to sign in.
if out="$(BILLING_ADMIN_PASSWORD="$DEV_ADMIN_PASSWORD" ./bin/billing admin create-user \
  --email "$DEV_ADMIN_EMAIL" --role administrator --name "$DEV_ADMIN_NAME" 2>&1)"; then
  echo "dev: created staff account $DEV_ADMIN_EMAIL"
else
  case "$out" in
  *"already exists"*) echo "dev: staff account $DEV_ADMIN_EMAIL already exists" ;;
  *)
    echo "dev-config: could not create the staff account:" >&2
    echo "$out" >&2
    exit 1
    ;;
  esac
fi
