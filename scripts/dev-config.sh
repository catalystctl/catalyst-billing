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

setting servers provider fake
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
