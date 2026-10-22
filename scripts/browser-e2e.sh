#!/usr/bin/env bash
#
# Authenticated end-to-end browser test for the operator panel and the customer portal.
#
# The anonymous smoke test proves the shell loads and the CSP is enforced. It cannot prove that
# signing in works, that a list reads real data, or that a detail screen renders a record — and
# those are the things a user does first. This script seeds a customer, an invoice and a service,
# then drives every screen in a real headless Chromium through scripts/e2e-ui.mjs.
#
# It is separate from browser-smoke.sh because it needs a writable database and creates accounts;
# the smoke test must stay runnable against any server, including a read-only one.
#
# Usage: scripts/browser-e2e.sh [base-url]
#   Requires: go, node (>=22), chromium (or CHROME=path), curl, python3, psql,
#   and BILLING_DATABASE_URL (from .env).
set -euo pipefail

cd "$(dirname "$0")/.."

BASE="${1:-}"
SERVER_PID=""
SHOTS="${E2E_SCREENSHOTS:-}"
STAMP="$(date +%s)"
STAFF_EMAIL="e2e-staff-${STAMP}@example.test"
STAFF_PASSWORD="e2e-staff-password-1"
CUSTOMER_EMAIL="e2e-customer-${STAMP}@example.test"
CUSTOMER_PASSWORD="e2e-customer-password-1"
CUSTOMER_NAME="E2E Customer ${STAMP}"

cleanup() {
  if [[ -n "$SERVER_PID" ]]; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  # The seed is deleted on the way out, pass or fail. A browser test that leaves an account
  # behind on every run fills the audit log with noise, makes the catalog grow a product per
  # run, and eventually changes what the next run sees — a test that mutates the fixture it
  # asserts on stops being a test.
  if [[ -n "${SEEDED:-}" ]]; then
    # --force is required because a developer's database is usually not named "test"; the
    # pattern is what scopes the deletion, and it only matches this script's own accounts.
    ./bin/billing admin purge-test-data --pattern "e2e-%@example.test" --yes --force >/dev/null 2>&1 || true
    psql "$BILLING_DATABASE_URL" -q -v ON_ERROR_STOP=0 >/dev/null 2>&1 <<SQL || true
DELETE FROM services WHERE external_id LIKE 'e2e-svc-%';
DELETE FROM price_versions WHERE product_id IN (
  SELECT id FROM products WHERE slug LIKE 'e2e-hosting-%' OR slug LIKE 'e2e-product-%' OR slug LIKE 'e2e-hidden-%');
DELETE FROM products WHERE slug LIKE 'e2e-hosting-%' OR slug LIKE 'e2e-product-%' OR slug LIKE 'e2e-hidden-%';
DELETE FROM categories WHERE slug LIKE 'e2e-category-%';
SQL
  fi
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
[[ -n "${BILLING_DATABASE_URL:-}" ]] || fail "BILLING_DATABASE_URL is not set (see .env.example)"

# ---------------------------------------------------------------- the server --

if [[ -z "$BASE" ]]; then
  PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
  BASE="http://127.0.0.1:${PORT}"

  go build -o /tmp/billing-e2e ./cmd/billing
  BILLING_HTTP_ADDR="127.0.0.1:${PORT}" \
  BILLING_EMBEDDED_WORKER=false \
  BILLING_PUBLIC_URL="$BASE" \
    /tmp/billing-e2e serve >/tmp/billing-e2e.log 2>&1 &
  SERVER_PID=$!

  for _ in $(seq 1 120); do
    if curl -fsS "$BASE/healthz" >/dev/null 2>&1; then break; fi
    sleep 0.25
  done
  curl -fsS "$BASE/healthz" >/dev/null || { cat /tmp/billing-e2e.log; fail "server did not become ready"; }
fi
ok "server is up at $BASE"

# ---------------------------------------------------------------- the seed --

# Accounts and records are created through the product's own admin commands, so the seed cannot
# produce a state the API would refuse.
BILLING_ADMIN_PASSWORD="$STAFF_PASSWORD" \
  ./bin/billing admin create-user --email "$STAFF_EMAIL" --role administrator --name "E2E Staff" >/dev/null \
  || fail "could not create the staff account"

SEEDED=1
CUSTOMER_ID="$(./bin/billing admin create-customer --email "$CUSTOMER_EMAIL" --name "$CUSTOMER_NAME" \
  | awk '/created customer/{print $3}')"
[[ -n "$CUSTOMER_ID" ]] || fail "could not create the customer"
ok "seeded a customer and a staff account"

INVOICE_NUMBER="$(./bin/billing admin issue-invoice --customer "$CUSTOMER_ID" --amount 42.50 --currency USD \
  --description "Managed hosting — e2e" --tax-rate 20% --merchant "Catalyst Hosting Ltd" \
  | grep -oE 'INV-[0-9]+' | head -1)"
[[ -n "$INVOICE_NUMBER" ]] || fail "could not issue the invoice"

# A customer login linked to that customer, so the portal has a real account behind it.
BILLING_ADMIN_PASSWORD="$CUSTOMER_PASSWORD" \
  ./bin/billing admin create-user --email "$CUSTOMER_EMAIL" --name "$CUSTOMER_NAME" >/dev/null \
  || fail "could not create the customer's login"

# A product, a price version and a service, so the service screens have a record to render.
# There is no admin command for these: they are created by the checkout flow, which needs a
# payment gateway. The insert is guarded so a database that already has them is left alone.
SERVICE_EXTERNAL_ID="e2e-svc-${STAMP}"
psql "$BILLING_DATABASE_URL" -v ON_ERROR_STOP=1 -q >/dev/null <<SQL || fail "could not seed the catalog and a service"
BEGIN;
-- The ids are derived from the run stamp, not constant: a constant id makes the second run of
-- this script fail on the primary key, and a test that only passes on a fresh database is a
-- test that stops being run.
INSERT INTO products (id, slug, name)
VALUES (gen_random_uuid(), 'e2e-hosting-${STAMP}', 'E2E Hosting')
ON CONFLICT (slug) DO NOTHING;

INSERT INTO price_versions (id, product_id, version, currency, amount_minor, setup_minor, interval,
  anchor_day, anchor_timezone, provision_plan, provision_region)
SELECT gen_random_uuid(), p.id, 1, 'USD', 4250, 0, 'monthly', 1, 'UTC', 'e2e.small', 'us-east'
FROM products p WHERE p.slug = 'e2e-hosting-${STAMP}'
ON CONFLICT (product_id, version) DO NOTHING;

INSERT INTO services (id, customer_id, product_id, status, external_id, generation)
SELECT gen_random_uuid(), '${CUSTOMER_ID}', p.id, 'active', '${SERVICE_EXTERNAL_ID}', 1
FROM products p WHERE p.slug = 'e2e-hosting-${STAMP}'
ON CONFLICT DO NOTHING;
INSERT INTO customer_accounts (user_id, customer_id, role)
SELECT id, '${CUSTOMER_ID}', 'owner' FROM users WHERE email = '${CUSTOMER_EMAIL}'
ON CONFLICT DO NOTHING;

-- A category, a published product and a hidden one, so the storefront assertions have
-- something to prove: the published one must appear, the hidden one must not.
INSERT INTO categories (slug, name, description, sort_order)
VALUES ('e2e-category-${STAMP}', 'E2E Hosting', 'Seeded by the browser test', 1)
ON CONFLICT DO NOTHING;

INSERT INTO products (slug, name, short_description, description, category_id, hidden)
SELECT 'e2e-product-${STAMP}', 'E2E ${STAMP} Plan', 'A seeded plan', 'Seeded for the storefront test.',
       c.id, false
FROM categories c WHERE c.slug = 'e2e-category-${STAMP}'
ON CONFLICT DO NOTHING;

INSERT INTO price_versions (product_id, version, name, currency, amount_minor, interval,
  anchor_day, anchor_timezone, provision_plan, provision_region)
SELECT p.id, 1, 'Monthly', 'USD', 1900, 'monthly', 1, 'UTC', 'e2e.small', 'us-east'
FROM products p WHERE p.slug = 'e2e-product-${STAMP}'
ON CONFLICT (product_id, version) DO NOTHING;

INSERT INTO products (slug, name, short_description, category_id, hidden)
SELECT 'e2e-hidden-${STAMP}', 'E2E Hidden ${STAMP}', 'Must not appear', c.id, true
FROM categories c WHERE c.slug = 'e2e-category-${STAMP}'
ON CONFLICT DO NOTHING;

INSERT INTO price_versions (product_id, version, name, currency, amount_minor, interval,
  anchor_day, anchor_timezone)
SELECT p.id, 1, 'Monthly', 'USD', 9900, 'monthly', 1, 'UTC'
FROM products p WHERE p.slug = 'e2e-hidden-${STAMP}'
ON CONFLICT (product_id, version) DO NOTHING;
COMMIT;
SQL
ok "seeded a catalog product, a service and the portal login"

# ---------------------------------------------------------------- the drive --

ARGS=(
  "--base=$BASE"
  "--staff=$STAFF_EMAIL"
  "--staff-password=$STAFF_PASSWORD"
  "--customer=$CUSTOMER_EMAIL"
  "--customer-password=$CUSTOMER_PASSWORD"
  "--expect-customer=$CUSTOMER_EMAIL"
  "--expect-invoice=$INVOICE_NUMBER"
  "--expect-service=$SERVICE_EXTERNAL_ID"
  "--service-customer=$CUSTOMER_ID"
  "--expect-product=E2E ${STAMP} Plan"
  "--expect-product-slug=e2e-product-${STAMP}"
  "--expect-hidden=E2E Hidden ${STAMP}"
)
[[ -n "$SHOTS" ]] && ARGS+=("--screenshots=$SHOTS")

node scripts/e2e-ui.mjs "${ARGS[@]}" || fail "the browser test reported failures"

echo
echo "authenticated browser test passed against $BASE"
