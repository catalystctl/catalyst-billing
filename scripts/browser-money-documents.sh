#!/usr/bin/env bash
#
# Drives the money documents and the small CRUD gaps in a real browser.
#
# `e2e-ui.mjs` asserts that screens render; it never clicks the buttons that move money
# documents. This harness owns the fixtures those buttons need — an issued invoice, a draft to
# issue as a proforma, a credit note to download, a linked sign-in provider to unlink — starts
# the panel against them, and drives every affordance through `e2e-money-documents.mjs`.
#
# The fixtures are created through the product's own admin commands wherever one exists, and by
# stamped inserts where none does (there is no admin command for a draft invoice, a credit note,
# a catalogue server or a social link). Every stamp carries the run's timestamp, so a re-run
# against the same database does not collide with the last one.
#
# Usage: scripts/browser-money-documents.sh [base-url]
#   Requires: go, node (>=22), chromium (or CHROME=path), curl, python3, psql,
#   and BILLING_DATABASE_URL (from .env).
set -euo pipefail

cd "$(dirname "$0")/.."

BASE="${1:-}"
SERVER_PID=""
BIN="/tmp/billing-money-e2e"

# One run at a time: the seed writes installation-wide rows (a provider, a catalogue server) and
# two overlapping runs would delete each other's fixtures mid-flight.
if command -v flock >/dev/null 2>&1; then
  exec 8>"${TMPDIR:-/tmp}/catalyst-billing-money-e2e.lock"
  if ! flock -n 8; then
    echo "FAIL: another money-document run holds the lock; wait for it to finish" >&2
    exit 1
  fi
fi

STAMP="$(date +%s)"
STAFF_EMAIL="money-e2e-staff-${STAMP}@example.test"
STAFF_PASSWORD="money-e2e-staff-password-1"
CUSTOMER_EMAIL="money-e2e-customer-${STAMP}@example.test"
CUSTOMER_PASSWORD="money-e2e-customer-password-1"
PROVIDER_SLUG="money-e2e-provider-${STAMP}"
SERVER_SLUG="money-e2e-server-${STAMP}"

fail() { echo "FAIL: $*" >&2; exit 1; }
ok()   { echo "ok: $*"; }

cleanup() {
  if [[ -n "$SERVER_PID" ]]; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  # The installation-wide rows this run added. They are not scoped to a customer, so the
  # product's purge cannot see them; they are deleted here by their own stamp.
  psql "$BILLING_DATABASE_URL" -q -v ON_ERROR_STOP=0 >/dev/null 2>&1 <<SQL || true
DELETE FROM servers WHERE slug = '${SERVER_SLUG}';
DELETE FROM social_providers WHERE slug = '${PROVIDER_SLUG}';
SQL
  # Everything scoped to the seeded accounts — users, customers, invoices, the journal, credit
  # notes, audit rows — is removed by the product's own command, because those tables refuse a
  # plain DELETE by trigger.
  # `--force` because this cluster's database is named `catalyst_billing`, which the command
  # does not recognise as a test/dev name; the pattern is the guard that matters here.
  "$BIN" admin purge-test-data --pattern 'money-e2e-%@example.test' --yes --force >/dev/null 2>&1 || true
}
trap cleanup EXIT

if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi
[[ -n "${BILLING_DATABASE_URL:-}" ]] || fail "BILLING_DATABASE_URL is not set (see .env.example)"

# ---------------------------------------------------------------- the server --

go build -o "$BIN" ./cmd/billing || fail "could not build the panel"
"$BIN" migrate >/dev/null || fail "could not migrate the database"
ok "built the panel"

if [[ -z "$BASE" ]]; then
  PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
  BASE="http://127.0.0.1:${PORT}"

  BILLING_HTTP_ADDR="127.0.0.1:${PORT}" \
  BILLING_EMBEDDED_WORKER=false \
  BILLING_PUBLIC_URL="$BASE" \
    "$BIN" serve >/tmp/billing-money-e2e.log 2>&1 8>&- &
  SERVER_PID=$!

  for _ in $(seq 1 120); do
    if curl -fsS "$BASE/healthz" >/dev/null 2>&1; then break; fi
    sleep 0.25
  done
  curl -fsS "$BASE/healthz" >/dev/null || { cat /tmp/billing-money-e2e.log; fail "server did not become ready"; }
fi
ok "server is up at $BASE"

# ------------------------------------------------------------------ the seed --

BILLING_ADMIN_PASSWORD="$STAFF_PASSWORD" \
  "$BIN" admin create-user --email "$STAFF_EMAIL" --role administrator --name "Money E2E Staff" >/dev/null \
  || fail "could not create the staff account"

CUSTOMER_ID="$("$BIN" admin create-customer --email "$CUSTOMER_EMAIL" --name "Money E2E Customer" \
  | awk '/created customer/{print $3}')"
[[ -n "$CUSTOMER_ID" ]] || fail "could not create the customer"

INVOICE_NUMBER="$("$BIN" admin issue-invoice --customer "$CUSTOMER_ID" --amount 42.50 --currency USD \
  --description "Managed hosting — money e2e" --tax-rate 20% --merchant "Catalyst Hosting Ltd" \
  | grep -oE 'INV-[0-9]+' | head -1)"
[[ -n "$INVOICE_NUMBER" ]] || fail "could not issue the invoice"

BILLING_ADMIN_PASSWORD="$CUSTOMER_PASSWORD" \
  "$BIN" admin create-user --email "$CUSTOMER_EMAIL" --name "Money E2E Customer" >/dev/null \
  || fail "could not create the customer's login"

ISSUED_ID="$(psql "$BILLING_DATABASE_URL" -tAc "SELECT id FROM invoices WHERE number = '${INVOICE_NUMBER}'" | tr -d '[:space:]')"
[[ -n "$ISSUED_ID" ]] || fail "could not read the issued invoice id"

# The rest of the fixtures. A draft (for the proforma) copies the issued invoice's snapshot,
# because the snapshot is what a document is rendered from and a hand-written one would not be
# the shape the renderer expects.
psql "$BILLING_DATABASE_URL" -v ON_ERROR_STOP=1 -q >/dev/null <<SQL || fail "could not seed the document fixtures"
BEGIN;

INSERT INTO customer_accounts (user_id, customer_id, role)
SELECT id, '${CUSTOMER_ID}', 'owner' FROM users WHERE email = '${CUSTOMER_EMAIL}'
ON CONFLICT DO NOTHING;

INSERT INTO invoices (customer_id, status, currency, subtotal_minor, tax_minor, total_minor, snapshot)
SELECT customer_id, 'draft', currency, subtotal_minor, tax_minor, total_minor, snapshot
FROM invoices WHERE id = '${ISSUED_ID}';

-- A credit note against the issued invoice, so the download has a document to serve on both
-- sides of the portal.
INSERT INTO credit_notes (number, customer_id, invoice_id, currency, total_minor, tax_minor,
  reason, status, issued_at)
VALUES ('CN-MONEY-${STAMP}', '${CUSTOMER_ID}', '${ISSUED_ID}', 'USD', 500, 0,
  'Setup fee waived as a goodwill gesture.', 'issued', now());

-- A catalogue server for the rename/delete flow. It is seeded rather than created in the UI so
-- the "delete" assertion has a row that is not the one the create step just made — the two are
-- separate defects and should fail separately.
INSERT INTO servers (slug, name, provider, region, node, enabled, sort_order)
VALUES ('${SERVER_SLUG}', 'Money E2E Node', 'hetzner', 'eu-west', 'node-1', true, 0);

-- A sign-in provider and one identity linked to the staff account, so the unlink action has
-- something to remove. The identity is linked directly: the linking flow itself is an OAuth
-- redirect to a provider that does not exist in a test installation.
INSERT INTO social_providers (slug, name, issuer_url, authorization_endpoint, token_endpoint,
  userinfo_endpoint, client_id, client_secret, enabled)
VALUES ('${PROVIDER_SLUG}', 'Money E2E Provider', 'https://provider.example.test',
  'https://provider.example.test/authorize', 'https://provider.example.test/token',
  'https://provider.example.test/userinfo', 'client-id', 'client-secret', true);

INSERT INTO social_identities (provider_id, user_id, subject, email)
SELECT p.id, u.id, 'money-e2e-subject', '${STAFF_EMAIL}'
FROM social_providers p, users u
WHERE p.slug = '${PROVIDER_SLUG}' AND u.email = '${STAFF_EMAIL}';

COMMIT;
SQL

DRAFT_ID="$(psql "$BILLING_DATABASE_URL" -tAc "
  SELECT id FROM invoices WHERE customer_id = '${CUSTOMER_ID}' AND status = 'draft' LIMIT 1" | tr -d '[:space:]')"
[[ -n "$DRAFT_ID" ]] || fail "could not read the draft invoice id"
ok "seeded an issued invoice, a draft, a credit note, a server and a linked provider"

# ---------------------------------------------------------------- the browser --

ARGS=(
  "--base=$BASE"
  "--staff=$STAFF_EMAIL"
  "--staff-password=$STAFF_PASSWORD"
  "--customer=$CUSTOMER_EMAIL"
  "--customer-password=$CUSTOMER_PASSWORD"
  "--issued-invoice=$ISSUED_ID"
  "--draft-invoice=$DRAFT_ID"
)
[[ -n "${E2E_SCREENSHOTS:-}" ]] && ARGS+=("--screenshots=$E2E_SCREENSHOTS")

node scripts/e2e-money-documents.mjs "${ARGS[@]}" 8>&- || fail "the money-document browser test reported failures"

echo "money-document browser test passed against $BASE"
