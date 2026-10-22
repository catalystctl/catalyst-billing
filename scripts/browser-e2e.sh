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

# One run at a time.
#
# The harness seeds and truncates installation-wide rows — the support desk's switch, a currency,
# an announcement, the staff account — in the shared development database. Two runs overlapping
# do not merely interleave: the second run's seed deletes the first run's staff user while the
# first is still driving a browser, and every screen then redirects to /login. The failure looks
# like a session bug in the panel and is not.
#
# An exclusive lock on the script itself is the whole fix. It is taken with `flock` where that
# exists and skipped where it does not, because a harness that refuses to run on a machine without
# util-linux would be worse than one that can be run twice by accident.
if command -v flock >/dev/null 2>&1; then
  exec 9>"${TMPDIR:-/tmp}/catalyst-billing-e2e.lock"
  if ! flock -n 9; then
    echo "FAIL: another browser-e2e run holds the lock; wait for it to finish" >&2
    exit 1
  fi
fi
SHOTS="${E2E_SCREENSHOTS:-}"
STAMP="$(date +%s)"
# A currency code is `^[A-Z0-9]{2,12}$`, so the run stamp rides along in it: "ZZ" plus ten
# digits is twelve characters, valid, and cannot collide with a real ISO code.
CURRENCY_CODE="ZZ${STAMP}"
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
  #
  # The split of responsibility is deliberate, because getting it wrong hides a real bug:
  #
  #   * `admin purge-test-data` owns everything scoped to a customer or a user — accounts,
  #     invoices, the journal, tickets, carts, redemptions. Those tables refuse DELETE by
  #     trigger (the journal, the audit log, issued invoices), so purge is the only thing that
  #     can remove them, and it is the product's own reviewed command rather than a DELETE
  #     this script invented.
  #   * This script owns the *installation-wide configuration* it added — a coupon, a tax
  #     rate, a currency, an announcement, a config option. Those hang from no customer, so
  #     purge cannot reach them by design, and they are this script's to clean.
  #
  # The purge is NOT silenced. An earlier version ran it with `|| true` and a missing step in
  # its list (no `tickets` entry) aborted its transaction on the foreign key, so three runs in
  # a row leaked a staff account, a customer and a catalogue entry while the script reported
  # success. A cleanup that cannot fail is a cleanup that cannot tell you it stopped working,
  # so a purge failure is now printed with its output and the leftover rows are counted and
  # named at the end.
  if [[ -n "${SEEDED:-}" ]]; then
    # 1. The configuration fixtures. Children before parents: a config option's prices and a
    #    coupon's product restrictions and redemptions are all keyed to the row above them.
    psql "$BILLING_DATABASE_URL" -q -v ON_ERROR_STOP=0 >/dev/null 2>&1 <<SQL || true
DELETE FROM config_option_prices WHERE config_option_id IN (
  SELECT id FROM config_options WHERE name LIKE 'e2e-option-%');
DELETE FROM config_options WHERE name LIKE 'e2e-option-%';
DELETE FROM coupon_products WHERE coupon_id IN (
  SELECT id FROM coupons WHERE code LIKE 'E2E-%');
DELETE FROM coupon_redemptions WHERE coupon_id IN (
  SELECT id FROM coupons WHERE code LIKE 'E2E-%');
DELETE FROM coupons WHERE code LIKE 'E2E-%';
DELETE FROM tax_rates WHERE name LIKE 'e2e-tax-%';
DELETE FROM announcements WHERE title LIKE 'e2e-announcement-%';
DELETE FROM currencies WHERE code LIKE 'ZZ%';
SQL

    # The support desk's switch is restored to whatever this installation had before the run,
    # rather than forced off. A harness that left it on would change the behaviour of the next
    # person's installation, and one that always turned it off would hide a screen it is
    # supposed to exercise.
    if [[ -n "${TICKETS_WERE_DISABLED:-}" ]]; then
      psql "$BILLING_DATABASE_URL" -q -v ON_ERROR_STOP=0 >/dev/null 2>&1 <<SQL || true
UPDATE settings SET value = '${TICKETS_WERE_DISABLED}', updated_at = now()
WHERE group_key = 'tickets' AND key = 'disabled';
SQL
    fi

    # 2. The accounts and everything scoped to them. --force is required because a
    #    developer's database is usually not named "test"; the pattern is what scopes the
    #    deletion, and it only matches this script's own accounts.
    local purge_log
    if ! purge_log="$(./bin/billing admin purge-test-data --pattern "e2e-%@example.test" --yes --force 2>&1)"; then
      echo "WARNING: admin purge-test-data failed; the seed was not fully removed" >&2
      printf '%s\n' "$purge_log" | tail -5 >&2
    fi

    # 3. The catalog. Its price versions are referenced by cart items, and the cart is
    #    customer-scoped so purge has already removed it above — that ordering is why this
    #    block can run at all. Deleting a price version a live cart still points at is
    #    refused by the foreign key, which is the correct behaviour and not something to
    #    bypass.
    psql "$BILLING_DATABASE_URL" -q -v ON_ERROR_STOP=0 >/dev/null 2>&1 <<SQL || true
DELETE FROM services WHERE external_id LIKE 'e2e-svc-%';
DELETE FROM price_versions WHERE product_id IN (
  SELECT id FROM products WHERE slug LIKE 'e2e-hosting-%' OR slug LIKE 'e2e-product-%' OR slug LIKE 'e2e-hidden-%');
DELETE FROM products WHERE slug LIKE 'e2e-hosting-%' OR slug LIKE 'e2e-product-%' OR slug LIKE 'e2e-hidden-%';
DELETE FROM categories WHERE slug LIKE 'e2e-category-%';
SQL

    # 4. Prove it. A leak is reported by table and count, so a regression in the purge shows
    #    up as a named leftover row rather than as a database that quietly grows a product
    #    per run until something unrelated fails.
    local leftovers
    leftovers="$(psql "$BILLING_DATABASE_URL" -tAq -v ON_ERROR_STOP=0 2>/dev/null <<'SQL' || true
SELECT format('%s=%s', 'products', count(*)) FROM products WHERE slug LIKE 'e2e-%'
UNION ALL SELECT format('%s=%s', 'users', count(*)) FROM users WHERE email LIKE 'e2e-%@example.test'
UNION ALL SELECT format('%s=%s', 'customers', count(*)) FROM customers WHERE email LIKE 'e2e-%@example.test'
UNION ALL SELECT format('%s=%s', 'tickets', count(*)) FROM tickets WHERE subject LIKE 'e2e-ticket-%'
UNION ALL SELECT format('%s=%s', 'carts', count(*)) FROM carts c JOIN customers cu ON cu.id = c.customer_id
  WHERE cu.email LIKE 'e2e-%@example.test'
UNION ALL SELECT format('%s=%s', 'coupons', count(*)) FROM coupons WHERE code LIKE 'E2E-%'
UNION ALL SELECT format('%s=%s', 'currencies', count(*)) FROM currencies WHERE code LIKE 'ZZ%'
UNION ALL SELECT format('%s=%s', 'announcements', count(*)) FROM announcements WHERE title LIKE 'e2e-announcement-%'
UNION ALL SELECT format('%s=%s', 'config_options', count(*)) FROM config_options WHERE name LIKE 'e2e-option-%'
SQL
)"
    local leaked
    leaked="$(printf '%s\n' "$leftovers" | grep -v '=0$' | tr '\n' ' ' || true)"
    if [[ -n "$leaked" ]]; then
      echo "WARNING: the seed was not fully cleaned up: $leaked" >&2
    fi
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

# ------------------------------------------------ the commerce/support fixtures --
#
# The screens the parallel packages built need data of their own. Each insert is guarded with
# ON CONFLICT DO NOTHING like the catalog seed above, so a re-run against a database that
# already holds the row is a no-op rather than an error. The column names come from
# 0020_commerce.sql, which is the schema these rows live in; a name guessed here would fail
# loudly on the first run and be "fixed" by deleting the assertion.
#
# The fixtures and what each one proves:
#
#   coupon            /coupons lists a code, a kind and a value it read from the database
#   tax rate          /tax-rates lists a jurisdiction and an exact rational rate
#   currency          /currencies lists a configured currency with its render style
#   config option     /config-options shows an option attached to the seeded product
#   ticket + message  /support and /tickets show a ticket, and its thread has a body
#   credit balance    /portal/credits has a positive balance (a ledger posting, not a column)
#   announcement      /announcements lists a published announcement
#   cart line         /cart and /checkout are not empty, so they render the priced cart
#
# The credit balance is a *balanced journal transaction*, because that is what the balance is
# derived from: `GET /portal/credits` sums `journal_entries` on `customer_credit`. Inserting a
# number into a column would prove nothing, and the deferred balance trigger would refuse an
# unbalanced transaction anyway. Debit `refunds` (an expense) and credit `customer_credit`
# (a liability) is the same pair a goodwill credit note posts.
TICKET_SUBJECT="e2e-ticket-${STAMP} the host stopped answering"
CONFIG_OPTION_NAME="e2e-option-${STAMP} memory"
ANNOUNCEMENT_TITLE="e2e-announcement-${STAMP} maintenance window"
COUPON_CODE="E2E-${STAMP}"
TAX_RATE_NAME="e2e-tax-${STAMP} Germany VAT"

# The support desk's previous state, captured before the seed switches it on so cleanup can put
# it back. It is captured here, in the shell, rather than inside the SQL block below: a command
# substitution in a heredoc is inserted as text into the statement, not executed.
TICKETS_WERE_DISABLED="$(psql "$BILLING_DATABASE_URL" -tAc "SELECT coalesce((SELECT value FROM settings WHERE group_key = 'tickets' AND key = 'disabled'), 'false')" | tr -d '[:space:]')"
: "${TICKETS_WERE_DISABLED:=false}"

psql "$BILLING_DATABASE_URL" -v ON_ERROR_STOP=1 -q >/dev/null <<SQL || fail "could not seed the commerce and support fixtures"
BEGIN;

-- The support desk must be switched on, because this run asserts on the ticket form and on a
-- seeded ticket appearing in the queue. The setting is installation-wide state that an operator
-- (or an earlier manual test) may have left off, and a switched-off desk is a legitimate
-- configuration: the screens render it honestly rather than breaking. But this harness is here
-- to exercise the form, so it states the precondition instead of inheriting it.
--
-- The value is restored on the way out, alongside the other installation-wide configuration this
-- script owns. Leaving it forced on would change what the next person's installation does.
INSERT INTO settings (group_key, key, value, updated_at)
VALUES ('tickets', 'disabled', 'false', now())
ON CONFLICT (group_key, key) DO UPDATE SET value = 'false', updated_at = now();

-- A percentage coupon. `percentage` is a rational string, never a float, and the CHECK in
-- 0020 refuses a row that carries both a percentage and an amount.
INSERT INTO coupons (code, description, kind, percentage, applies_to, max_uses, enabled)
VALUES ('${COUPON_CODE}', 'Seeded by the browser test', 'percentage', '10', 'all', 100, true)
ON CONFLICT DO NOTHING;

-- One dated rate. `rate` is text for the same reason: an exact rational, not a float.
-- The unique index allows one open-ended rate per (country, region, class), so this is a
-- plain insert guarded on that key.
INSERT INTO tax_rates (name, country_code, region_code, tax_class, rate, inclusive, enabled)
VALUES ('${TAX_RATE_NAME}', 'DE', '', 'standard', '0.19', false, true)
ON CONFLICT DO NOTHING;

-- A currency with a deliberately unusual presentation, so the screen has something to render
-- that the seeded ISO rows do not: a suffix symbol, a space group separator and a comma
-- decimal mark. It is disabled so it is never offered at checkout — this is a fixture for the
-- configuration screen, not a change to what the shop sells in.
INSERT INTO currencies (code, name, scale, symbol, symbol_position, group_separator,
  decimal_mark, group_size, is_default, enabled, sort_order)
VALUES ('${CURRENCY_CODE}', 'E2E Test Currency', 2, 'Z', 'suffix', ' ', ',', 3, false, false, 999)
ON CONFLICT (code) DO NOTHING;

-- A published announcement. `published_at` must be set for `live` to be true, so it is.
INSERT INTO announcements (title, body, placement, published, published_at)
VALUES ('${ANNOUNCEMENT_TITLE}', 'The panel will be read-only for ten minutes.', 'both', true, now())
ON CONFLICT DO NOTHING;

-- A dropdown option on the seeded product, with two choices. `choices` is jsonb.
INSERT INTO config_options (product_id, name, description, option_type, choices, env_variable,
  required, sort_order)
SELECT p.id, '${CONFIG_OPTION_NAME}', 'Seeded by the browser test', 'dropdown',
       '["1 GB", "2 GB"]'::jsonb, 'E2E_MEMORY', false, 1
FROM products p WHERE p.slug = 'e2e-product-${STAMP}'
ON CONFLICT DO NOTHING;

-- A ticket with a first message, so both the list and the thread have something to render.
-- The number comes from the shared invoice sequence under the "ticket" scope, which is what
-- the product does; a ticket with no number would render "—" in the list and prove less.
INSERT INTO invoice_number_sequences (scope, next_value)
VALUES ('ticket', 1)
ON CONFLICT (scope) DO NOTHING;

WITH allocated AS (
  UPDATE invoice_number_sequences SET next_value = next_value + 1
  WHERE scope = 'ticket' RETURNING next_value - 1 AS n
)
INSERT INTO tickets (number, customer_id, user_id, subject, department, priority, status,
  opened_by, last_reply_at)
SELECT 'TKT-' || lpad(n::text, 6, '0'), '${CUSTOMER_ID}', u.id,
       '${TICKET_SUBJECT}', 'technical', 'high', 'open', u.id, now()
FROM allocated, users u WHERE u.email = '${CUSTOMER_EMAIL}';

INSERT INTO ticket_messages (ticket_id, author_user_id, from_staff, body)
SELECT t.id, u.id, false, 'The host stopped answering ping at 09:10 UTC and has not come back.'
FROM tickets t, users u
WHERE t.subject = '${TICKET_SUBJECT}' AND u.email = '${CUSTOMER_EMAIL}';

-- A credit balance, as a balanced posting. The reference id is a fresh uuid, so a re-run adds
-- a second posting rather than failing on the ledger's (kind, reference_type, reference_id)
-- unique key — and the balance assertion is "greater than zero", which both satisfy.
WITH tx AS (
  INSERT INTO journal_transactions (kind, reference_type, reference_id, currency)
  VALUES ('adjustment', 'e2e_seed', gen_random_uuid(), 'USD')
  RETURNING id
)
INSERT INTO journal_entries (transaction_id, account_code, currency, debit_minor, credit_minor,
  memo, customer_id)
SELECT tx.id, v.account, 'USD', v.debit, v.credit, 'Seeded credit for the browser test',
       '${CUSTOMER_ID}'
FROM tx, (VALUES ('refunds', 2500, 0), ('customer_credit', 0, 2500)) AS v(account, debit, credit);

-- A cart with one line, so /cart and /checkout render a priced selection rather than their
-- empty state. A cart holds a selection and its currency, never a price: the price comes from
-- the price version at read time.
INSERT INTO carts (customer_id, currency)
VALUES ('${CUSTOMER_ID}', 'USD')
ON CONFLICT (customer_id) DO NOTHING;

INSERT INTO cart_items (cart_id, price_version_id, quantity)
SELECT c.id, pv.id, 1
FROM carts c
JOIN products p ON p.slug = 'e2e-product-${STAMP}'
JOIN price_versions pv ON pv.product_id = p.id
WHERE c.customer_id = '${CUSTOMER_ID}'
ON CONFLICT DO NOTHING;

COMMIT;
SQL
ok "seeded a coupon, a tax rate, a currency, a config option, a ticket, credit and a cart line"

# The config-options screen is per product, so the harness needs the product's id and not its
# slug. Reading it back from the database is the only honest source: the insert above selected
# the id rather than generating one here.
CONFIG_PRODUCT_ID="$(psql "$BILLING_DATABASE_URL" -tAq -v ON_ERROR_STOP=1 \
  -c "SELECT id FROM products WHERE slug = 'e2e-product-${STAMP}'")" || fail "could not read the seeded product id"
[[ -n "$CONFIG_PRODUCT_ID" ]] || fail "the seeded product has no id"

# ---------------------------------------------------------------- the drive --

# The record ids the all-routes pass needs.
#
# The 37 named screens below are opened by URL, so they need no ids. The route sweep opens
# *every* route in the table, including the detail screens, and a detail screen is only
# exercised honestly against a record that exists: "no such invoice" would pass the same
# assertions as a working invoice page, and the pass would prove nothing. These are read back
# from the rows seeded above rather than generated, because the id is the database's to
# assign.
INVOICE_ID="$(psql "$BILLING_DATABASE_URL" -tAq -v ON_ERROR_STOP=1 \
  -c "SELECT id FROM invoices WHERE number = '${INVOICE_NUMBER}'")" || fail "could not read the seeded invoice id"
SERVICE_ID="$(psql "$BILLING_DATABASE_URL" -tAq -v ON_ERROR_STOP=1 \
  -c "SELECT id FROM services WHERE external_id = '${SERVICE_EXTERNAL_ID}'")" || fail "could not read the seeded service id"
TICKET_ID="$(psql "$BILLING_DATABASE_URL" -tAq -v ON_ERROR_STOP=1 \
  -c "SELECT id FROM tickets WHERE subject = '${TICKET_SUBJECT}'")" || fail "could not read the seeded ticket id"
PRODUCT_ID="$(psql "$BILLING_DATABASE_URL" -tAq -v ON_ERROR_STOP=1 \
  -c "SELECT id FROM products WHERE slug = 'e2e-product-${STAMP}'")" || fail "could not read the seeded product id"
for pair in "invoice:$INVOICE_ID" "service:$SERVICE_ID" "ticket:$TICKET_ID" "product:$PRODUCT_ID"; do
  [[ -n "${pair#*:}" ]] || fail "the seed produced no ${pair%%:*} id"
done

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
  # The commerce/support fixtures. Each is the value the screen must show, so an empty state
  # on a screen that was seeded is a finding rather than a fixture gap.
  "--expect-coupon=${COUPON_CODE}"
  "--expect-tax-rate=${TAX_RATE_NAME}"
  "--expect-currency=${CURRENCY_CODE}"
  "--expect-announcement=${ANNOUNCEMENT_TITLE}"
  "--expect-ticket=${TICKET_SUBJECT}"
  "--expect-cart-product=E2E ${STAMP} Plan"
  "--expect-config-product=${CONFIG_PRODUCT_ID}"
)
[[ -n "$SHOTS" ]] && ARGS+=("--screenshots=$SHOTS")

node scripts/e2e-ui.mjs "${ARGS[@]}" || fail "the browser test reported failures"

# Every route in the table, including the ones the named-screen pass above does not know
# about.
#
# The two passes ask different questions and both have to be asked. The named screens assert
# what a screen *contains*; this one asserts that every entry in web/src/routes.ts can be
# opened at all, which is the only way a route whose view module is missing, or whose guard
# disagrees with the table, becomes a failing test rather than a link somebody clicks once.
# It runs second because it is the cheaper assertion and because a failure here should not
# hide a content failure above it.
node scripts/e2e-routes.mjs \
  "--base=$BASE" \
  "--staff=$STAFF_EMAIL" \
  "--staff-password=$STAFF_PASSWORD" \
  "--customer=$CUSTOMER_EMAIL" \
  "--customer-password=$CUSTOMER_PASSWORD" \
  "--customer-id=$CUSTOMER_ID" \
  "--invoice-id=$INVOICE_ID" \
  "--service-id=$SERVICE_ID" \
  "--ticket-id=$TICKET_ID" \
  "--product-id=$PRODUCT_ID" \
  "--product-slug=e2e-product-${STAMP}" \
  || fail "the route sweep reported failures"

echo
echo "authenticated browser test passed against $BASE"
