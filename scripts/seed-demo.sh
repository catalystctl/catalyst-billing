#!/usr/bin/env bash
# Seed a development database with a complete, coherent demo dataset.
#
# `make dev` runs this after the migrations, so a fresh installation has something to look at:
# customers, a catalog, subscriptions, services, invoices in every state, payments, a refund, a
# credit note, dunning, tickets, notifications, webhooks and an audit trail.
#
# Two rules this script obeys, and they are why it is not a single INSERT batch:
#
#   * **Money goes through the product.** Customers, users and invoices are created with
#     `billing admin`, so invoice numbering, the frozen document snapshot and the subledger
#     postings are the ones the code produces. A hand-written invoice would look right and be
#     wrong.
#   * **The ledger still balances.** Everything the admin CLI cannot create — catalog, orders,
#     subscriptions, services, payments, refunds, credit notes — is inserted directly, and the
#     journal rows that accompany it are written as balanced pairs in the same transaction. The
#     database has a deferred trigger for this, so an unbalanced insert aborts the seed instead
#     of quietly leaving a demo database whose `doctor` output is a lie.
#
# Re-running is safe: the script first purges anything it created (everything lives under
# `@demo.test` or a `demo-` slug prefix), then rebuilds it.
#
# The demo password is deliberately weak and printed at the end. It is a fixture, not a secret.

set -euo pipefail

cd "$(dirname "$0")/.."

set -a
# shellcheck disable=SC1091
. ./.env
set +a

if [[ ! -x ./bin/billing ]]; then
  echo "seed-demo: ./bin/billing is missing; run make build" >&2
  exit 1
fi

DOMAIN=demo.test
PASSWORD="${DEMO_PASSWORD:-demo-password-1}"
STAFF_PASSWORD="${DEMO_STAFF_PASSWORD:-demo-staff-password-1}"

psql_q() { psql "$BILLING_DATABASE_URL" -v ON_ERROR_STOP=1 -q "$@"; }
psql_t() { psql "$BILLING_DATABASE_URL" -v ON_ERROR_STOP=1 -tAq "$@"; }

say() { printf '  %s\n' "$*"; }

# ---------------------------------------------------------------- reset --
# purge-test-data refuses a whole-database delete on purpose, so the demo domain is named
# explicitly. Anything the command cannot reach (catalog rows, which are not scoped to a
# customer) is removed below by slug prefix.
# The seed creates an affiliate, and an affiliate names a user, so it is removed here rather than
# left for `purge-test-data` to trip over: an affiliate row is the one thing in this dataset that a
# customer purge cannot reach without knowing the programme's table names.
psql_q <<'SQL'
delete from affiliate_payouts where affiliate_id in
  (select id from affiliates where user_id in (select id from users where email like '%@demo.test'));
delete from affiliate_balances where affiliate_id in
  (select id from affiliates where user_id in (select id from users where email like '%@demo.test'));
delete from affiliate_recurring_commissions where affiliate_id in
  (select id from affiliates where user_id in (select id from users where email like '%@demo.test'));
delete from affiliate_referrals where affiliate_id in
  (select id from affiliates where user_id in (select id from users where email like '%@demo.test'));
delete from affiliates where user_id in (select id from users where email like '%@demo.test');
SQL

if [[ "$(psql_t -c "select count(*) from customers where email like '%@$DOMAIN'")" != "0" ]]; then
  say "removing the previous demo dataset"
  # A failed purge must stop the run. Swallowing it was a bug: the seed then re-created accounts
  # on top of the leftovers and failed much later with a confusing duplicate-key error, instead of
  # saying what had actually gone wrong.
  if ! purge_out="$(./bin/billing admin purge-test-data --pattern "%@$DOMAIN" --yes --force 2>&1)"; then
    echo "seed-demo: could not remove the previous demo dataset:" >&2
    printf '%s\n' "$purge_out" >&2
    exit 1
  fi
fi
psql_q <<'SQL'
delete from coupon_products where coupon_id in (select id from coupons where code like 'DEMO-%');
delete from coupons where code like 'DEMO-%';
-- A price version references its product, so the children go first. The catalog is not scoped to a
-- customer, which is why `purge-test-data` cannot reach it.
-- Webhook deliveries are not scoped to a customer either: the inbox is keyed by provider and the
-- provider's own event id, which is what makes a replayed delivery a no-op in production.
delete from gateway_events where provider = 'stripe' and provider_account = 'acct_demo' and event_id like 'evt_demo_%';
delete from price_versions where product_id in (select id from products where slug like 'demo-%');
delete from product_upgrades where product_id in (select id from products where slug like 'demo-%');
delete from products where slug like 'demo-%';
delete from categories where slug like 'demo-%';
SQL

# ---------------------------------------------------------------- users --
# Every account is created by the CLI so the password hash is a real Argon2id hash from the
# same code path a customer uses. A hand-written hash would be a login nobody can use.
make_user() { # email name staff-role
  local email="$1" name="$2" role="${3:-}"
  local out
  if [[ -n "$role" ]]; then
    out="$(BILLING_ADMIN_PASSWORD="$STAFF_PASSWORD" ./bin/billing admin create-user \
      --email "$email" --name "$name" --role "$role" 2>&1)"
  else
    out="$(BILLING_ADMIN_PASSWORD="$PASSWORD" ./bin/billing admin create-user \
      --email "$email" --name "$name" 2>&1)"
  fi
  local id
  id="$(sed -n 's/^created user \([0-9a-f-]*\).*/\1/p' <<<"$out")"
  if [[ -z "$id" ]]; then
    # An account that survived a previous run is reused rather than treated as a failure. The
    # seed has to be re-runnable from any state, including one left by an interrupted run — that
    # is the whole point of it being a `make` target.
    id="$(psql_t -c "select id from users where email = '$email'")"
    if [[ -z "$id" ]]; then
      echo "seed-demo: could not create $email: $out" >&2
      exit 1
    fi
    # A reused account keeps its own password, so the banner at the end would be a lie. Say so
    # rather than leaving someone to discover it at the sign-in form.
    echo "  note: $email already existed; its password was left unchanged" >&2
  fi
  printf '%s' "$id"
}

make_customer() { # email name
  local out id
  out="$(./bin/billing admin create-customer --email "$1" --name "$2" 2>&1)"
  id="$(sed -n 's/^created customer \([0-9a-f-]*\).*/\1/p' <<<"$out")"
  if [[ -z "$id" ]]; then
    id="$(psql_t -c "select id from customers where email = '$1'")"
    if [[ -z "$id" ]]; then
      echo "seed-demo: could not create customer $1: $out" >&2
      exit 1
    fi
  fi
  printf '%s' "$id"
}

say "staff accounts"
S_ADMIN="$(psql_t -c "select id from users where email='admin@dev.local'")"
if [[ -z "$S_ADMIN" ]]; then
  S_ADMIN="$(make_user "admin@dev.local" "Dev Operator" administrator)"
fi
S_SUPPORT="$(make_user "support@$DOMAIN" "Mara Okafor" support)"
S_BILLING="$(make_user "billing@$DOMAIN" "Tomas Lindqvist" billing)"
S_INFRA="$(make_user "infra@$DOMAIN" "Ravi Prasad" infrastructure)"

say "customers"
C1="$(make_customer "northwind@$DOMAIN"     "Northwind Hosting")"
C2="$(make_customer "acme@$DOMAIN"          "Acme Games Collective")"
C3="$(make_customer "priya@$DOMAIN"         "Priya Raman")"
C4="$(make_customer "jonas@$DOMAIN"         "Jonas Weber")"
C5="$(make_customer "sakura@$DOMAIN"        "Sakura Cloud KK")"
C6="$(make_customer "fixture@$DOMAIN"       "Test Fixture Co")"

# A portal login per customer, linked through customer_accounts. The link is explicit because an
# unverified email address is not proof that the person owns the account.
say "portal logins"
U1="$(make_user "northwind@$DOMAIN" "Nadia North" )"
U2="$(make_user "acme@$DOMAIN"      "Andre Costa" )"
U3="$(make_user "priya@$DOMAIN"     "Priya Raman" )"
U4="$(make_user "jonas@$DOMAIN"     "Jonas Weber" )"
U5="$(make_user "sakura@$DOMAIN"    "Saki Tanaka" )"
U6="$(make_user "fixture@$DOMAIN"   "Test Fixture" )"
psql_q <<SQL
-- The link is normally already here: create-user calls authn.EnsureBillingIdentity, which adopts
-- the customer with the same address and writes the row itself. Inserting it again is what
-- produced "duplicate key value violates unique constraint customer_accounts_unique". So this is
-- a no-op on the normal path and only fills in a customer whose login was created some other way.
-- (No backticks in this heredoc: it is unquoted, so a backtick would be command-substituted.)
insert into customer_accounts (user_id, customer_id, role) values
  ('$U1','$C1','owner'), ('$U2','$C2','owner'), ('$U3','$C3','owner'),
  ('$U4','$C4','owner'), ('$U5','$C5','owner'), ('$U6','$C6','owner')
on conflict (user_id, customer_id) do nothing;
SQL

# ---------------------------------------------------------------- catalog --
say "catalog"
CAT_GAME="$(uuidgen)"; CAT_VPS="$(uuidgen)"; CAT_WEB="$(uuidgen)"; CAT_ADDON="$(uuidgen)"
psql_q <<SQL
insert into categories (id, slug, name, description, sort_order, hidden) values
  ('$CAT_GAME',  'demo-game-servers', 'Game Servers', 'Low-latency game hosting with DDoS protection.', 1, false),
  ('$CAT_VPS',   'demo-vps',          'Cloud VPS',    'Root access, NVMe storage, hourly snapshots.',   2, false),
  ('$CAT_WEB',   'demo-web-hosting',  'Web Hosting',  'Managed hosting with a control panel and TLS.',  3, false),
  ('$CAT_ADDON', 'demo-addons',       'Add-ons',      'Extras that attach to an existing service.',     4, false);
SQL

# One product per row, with the price versions the storefront offers. Amounts are minor units.
add_product() { # slug name category short description sort
  printf '%s' "$(uuidgen)"
}
P_IRON="$(uuidgen)"; P_DIAMOND="$(uuidgen)"; P_VALHEIM="$(uuidgen)"
P_VPS4="$(uuidgen)"; P_VPS8="$(uuidgen)"; P_WEB="$(uuidgen)"; P_BACKUP="$(uuidgen)"

psql_q <<SQL
insert into products (id, slug, name, description, short_description, category_id, hidden, sort_order) values
  ('$P_IRON',    'demo-minecraft-iron',    'Minecraft — Iron',    '4 GB RAM, 2 vCPU, 10 player slots. Java and Bedrock.','Reliable survival server for a small group.',  '$CAT_GAME', false, 1),
  ('$P_DIAMOND', 'demo-minecraft-diamond', 'Minecraft — Diamond', '8 GB RAM, 4 vCPU, 40 player slots, mod support.',      'For modpacks and busy servers.',              '$CAT_GAME', false, 2),
  ('$P_VALHEIM', 'demo-valheim-duo',       'Valheim — Duo',       '6 GB RAM, 3 vCPU, up to 10 players.',                  'A Viking world for you and a friend.',        '$CAT_GAME', false, 3),
  ('$P_VPS4',    'demo-vps-4gb',           'Cloud VPS — 4 GB',    '4 GB RAM, 2 vCPU, 80 GB NVMe, root access.',           'The everyday VPS.',                           '$CAT_VPS',  false, 1),
  ('$P_VPS8',    'demo-vps-8gb',           'Cloud VPS — 8 GB',    '8 GB RAM, 4 vCPU, 160 GB NVMe, root access.',          'More headroom for databases and builds.',      '$CAT_VPS',  false, 2),
  ('$P_WEB',     'demo-web-hosting',       'Managed Web Hosting', '20 GB SSD, unlimited mailboxes, free TLS.',            'PHP, Node and static sites.',                  '$CAT_WEB',  false, 1),
  ('$P_BACKUP',  'demo-daily-backups',     'Daily Backups',       'Fourteen daily snapshots, off-site, one-click restore.','Attaches to any service.',                    '$CAT_ADDON',false, 1);
SQL

PV_IRON_M="$(uuidgen)"; PV_IRON_A="$(uuidgen)"
PV_DIAMOND_M="$(uuidgen)"; PV_DIAMOND_A="$(uuidgen)"
PV_VALHEIM_M="$(uuidgen)"
PV_VPS4_M="$(uuidgen)"; PV_VPS4_Q="$(uuidgen)"
PV_VPS8_M="$(uuidgen)"
PV_WEB_M="$(uuidgen)"; PV_WEB_A="$(uuidgen)"
PV_BACKUP_M="$(uuidgen)"

psql_q <<SQL
insert into price_versions
  (id, product_id, version, currency, amount_minor, setup_minor, interval, anchor_day, anchor_timezone, name, sort_order)
values
  ('$PV_IRON_M',    '$P_IRON',    1, 'USD', 1200,    0, 'monthly',   1, 'UTC', 'Monthly',  1),
  ('$PV_IRON_A',    '$P_IRON',    2, 'USD', 12000,   0, 'annual',    1, 'UTC', 'Annual',   2),
  ('$PV_DIAMOND_M', '$P_DIAMOND', 1, 'USD', 2400,    0, 'monthly',   1, 'UTC', 'Monthly',  1),
  ('$PV_DIAMOND_A', '$P_DIAMOND', 2, 'USD', 24000,   0, 'annual',    1, 'UTC', 'Annual',   2),
  ('$PV_VALHEIM_M', '$P_VALHEIM', 1, 'USD', 1800,    0, 'monthly',   1, 'UTC', 'Monthly',  1),
  ('$PV_VPS4_M',    '$P_VPS4',    1, 'USD', 2000,    0, 'monthly',   1, 'UTC', 'Monthly',  1),
  ('$PV_VPS4_Q',    '$P_VPS4',    2, 'USD', 5400,    0, 'quarterly', 1, 'UTC', 'Quarterly',2),
  ('$PV_VPS8_M',    '$P_VPS8',    1, 'USD', 4000,    0, 'monthly',   1, 'UTC', 'Monthly',  1),
  ('$PV_WEB_M',     '$P_WEB',     1, 'USD', 800,     0, 'monthly',   1, 'UTC', 'Monthly',  1),
  ('$PV_WEB_A',     '$P_WEB',     2, 'USD', 8000,    0, 'annual',    1, 'UTC', 'Annual',   2),
  ('$PV_BACKUP_M',  '$P_BACKUP',  1, 'USD', 300,     0, 'monthly',   1, 'UTC', 'Monthly',  1);
SQL

# ---------------------------------------------------------------- coupons --
say "coupons"
psql_q <<SQL
insert into coupons (code, description, kind, percentage, amount_minor, currency, applies_to,
                     max_uses, max_uses_per_user, starts_at, expires_at, enabled)
values
  ('DEMO-WELCOME10', '10% off the first invoice',            'percentage', '10', null,  null,  'price', 500, 1, now() - interval '30 days', now() + interval '90 days', true),
  ('DEMO-TWENTYOFF', 'USD 20 off, setup and recurring',      'fixed',      null,   2000,  'USD', 'all',   100, 1, now() - interval '10 days', now() + interval '60 days', true),
  ('DEMO-EXPIRED',   'Expired campaign, kept for the UI',    'percentage', '25', null,  null,  'price', 50,  1, now() - interval '90 days', now() - interval '30 days', true);
SQL

# ------------------------------------------------------------ subscriptions --
# A subscription is the billing relationship; a service is the thing that actually runs. They are
# separate rows on purpose (one subscription can own several services), so both are seeded.
say "subscriptions and services"
SUB1="$(uuidgen)"; SUB2="$(uuidgen)"; SUB3="$(uuidgen)"; SUB4="$(uuidgen)"; SUB5="$(uuidgen)"; SUB6="$(uuidgen)"

psql_q <<SQL
insert into subscriptions
  (id, customer_id, price_version_id, status, interval, anchor_day, anchor_hour, anchor_minute,
   timezone, current_period_start, current_period_end, next_billing_at, version, created_at,
   cancel_at_period_end)
values
  ('$SUB1','$C1','$PV_VPS8_M',   'active',   'monthly', 1, 9, 0, 'UTC', now() - interval '18 days', now() + interval '12 days', now() + interval '12 days', 3, now() - interval '400 days', false),
  ('$SUB2','$C2','$PV_DIAMOND_M','past_due', 'monthly', 1, 9, 0, 'UTC', now() - interval '40 days', now() - interval '10 days', now() - interval '10 days', 5, now() - interval '250 days', false),
  ('$SUB3','$C3','$PV_IRON_M',   'active',   'monthly', 1, 9, 0, 'UTC', now() - interval '5 days',  now() + interval '25 days', now() + interval '25 days', 1, now() - interval '120 days', false),
  ('$SUB4','$C4','$PV_VALHEIM_M','active',   'monthly', 1, 9, 0, 'UTC', now(),                    now() + interval '30 days', now() + interval '30 days', 1, now() - interval '2 days',   false),
  ('$SUB5','$C5','$PV_WEB_A',    'active',   'annual',  1, 9, 0, 'UTC', now() - interval '200 days', now() + interval '165 days', now() + interval '165 days', 2, now() - interval '500 days', false),
  ('$SUB6','$C6','$PV_VPS4_M',   'cancelled','monthly', 1, 9, 0, 'UTC', now() - interval '45 days', now() - interval '15 days', now() - interval '15 days', 2, now() - interval '300 days', true);
SQL

# The subscription each invoice and order belongs to, so the demo is internally consistent.
O1="$(uuidgen)"; O2="$(uuidgen)"; O3="$(uuidgen)"; O4="$(uuidgen)"; O5="$(uuidgen)"; O6="$(uuidgen)"
psql_q <<SQL
insert into orders (id, customer_id, status, currency, total_minor, version, created_at, updated_at) values
  ('$O1','$C1','completed',      'USD', 4000, 1, now() - interval '400 days', now() - interval '400 days'),
  ('$O2','$C2','completed',      'USD', 2400, 1, now() - interval '250 days', now() - interval '250 days'),
  ('$O3','$C3','paid',           'USD', 1200, 1, now() - interval '120 days', now() - interval '120 days'),
  ('$O4','$C4','fulfilling',     'USD', 1800, 1, now() - interval '2 days',   now() - interval '2 days'),
  ('$O5','$C5','completed',      'USD', 8000, 1, now() - interval '500 days', now() - interval '500 days'),
  ('$O6','$C6','cancelled',      'USD', 2000, 1, now() - interval '300 days', now() - interval '15 days');

insert into order_items (order_id, price_version_id, description, quantity, unit_minor, line_minor, setup_minor, position) values
  ('$O1','$PV_VPS8_M',   'Cloud VPS — 8 GB',   1, 4000, 4000, 0, 1),
  ('$O2','$PV_DIAMOND_M','Minecraft — Diamond', 1, 2400, 2400, 0, 1),
  ('$O3','$PV_IRON_M',   'Minecraft — Iron',    1, 1200, 1200, 0, 1),
  ('$O4','$PV_VALHEIM_M','Valheim — Duo',       1, 1800, 1800, 0, 1),
  ('$O5','$PV_WEB_A',    'Managed Web Hosting', 1, 8000, 8000, 0, 1),
  ('$O6','$PV_VPS4_M',   'Cloud VPS — 4 GB',    1, 2000, 2000, 0, 1);
SQL

SV1="$(uuidgen)"; SV2="$(uuidgen)"; SV3="$(uuidgen)"; SV4="$(uuidgen)"; SV5="$(uuidgen)"; SV6="$(uuidgen)"
psql_q <<SQL
insert into services
  (id, customer_id, subscription_id, product_id, status, external_id, generation, created_at,
   updated_at, label, description, quantity, order_id, config)
values
  ('$SV1','$C1','$SUB1','$P_VPS8',   'active',      'vps-northwind-01',  4, now() - interval '400 days', now() - interval '1 day',  'Production VPS',   'Runs the customer-facing API and its Postgres replica.', 1, '$O1', '{"region":"eu-west","image":"debian-12"}'::jsonb),
  ('$SV2','$C2','$SUB2','$P_DIAMOND','suspended',   'mc-acme-01',        7, now() - interval '250 days', now() - interval '8 days', 'Acme Survival',    'Suspended after two failed renewal attempts.',           1, '$O2', '{"region":"us-east","slots":40}'::jsonb),
  ('$SV3','$C3','$SUB3','$P_IRON',   'active',      'mc-priya-01',       2, now() - interval '120 days', now() - interval '3 days', 'Priya SMP',        'Small survival world with friends.',                     1, '$O3', '{"region":"eu-west","slots":10}'::jsonb),
  ('$SV4','$C4','$SUB4','$P_VALHEIM', 'provisioning','valheim-jonas-01', 1, now() - interval '2 days',   now() - interval '2 days', 'Valheim Duo',      'Queued; the provisioner is creating the container.',      1, '$O4', '{"region":"us-east"}'::jsonb),
  ('$SV5','$C5','$SUB5','$P_WEB',    'active',      'web-sakura-01',     3, now() - interval '500 days', now() - interval '20 days','Sakura main site', 'Marketing site and blog, 12 GB used.',                   1, '$O5', '{"php":"8.3"}'::jsonb),
  ('$SV6','$C6','$SUB6','$P_VPS4',   'terminated',  'vps-fixture-01',    5, now() - interval '300 days', now() - interval '15 days','Fixture VPS',      'Terminated at the end of the cancellation period.',       1, '$O6', '{}'::jsonb);
SQL

# A second service on Northwind's subscription: one subscription, several services.
SV7="$(uuidgen)"
psql_q <<SQL
insert into services
  (id, customer_id, subscription_id, product_id, status, external_id, generation, created_at,
   updated_at, label, description, quantity, order_id, config)
values
  ('$SV7','$C1','$SUB1','$P_BACKUP','active','backup-northwind-01',2, now() - interval '200 days',
   now() - interval '1 day','Nightly backups','Attached add-on for the production VPS.',1,'$O1','{}'::jsonb);
SQL

say "service operations and holds"
psql_q <<SQL
insert into service_operations (service_id, kind, operation_key, generation, status, attempts, last_error, created_at, started_at, finished_at) values
  ('$SV4','create',  'op-create-'||'$SV4',  1, 'retry_wait', 2, 'provisioner.create: connection reset by peer', now() - interval '2 days', now() - interval '2 days', null),
  ('$SV2','suspend', 'op-suspend-'||'$SV2', 7, 'succeeded',  1, null, now() - interval '8 days', now() - interval '8 days', now() - interval '8 days'),
  ('$SV6','terminate','op-term-'||'$SV6',   5, 'succeeded',  1, null, now() - interval '15 days', now() - interval '15 days', now() - interval '15 days'),
  ('$SV1','resize',  'op-resize-'||'$SV1',  4, 'needs_review',3,'provisioner.resize: plan not offered in eu-west', now() - interval '1 day', now() - interval '1 day', null);
SQL

psql_q <<SQL
insert into service_holds (service_id, reason, created_by, created_at) values
  ('$SV5','Customer asked us to hold off on the renewal while they confirm the invoice.', '$S_BILLING', now() - interval '6 days');
SQL

# ---------------------------------------------------------------- invoices --
# Issued through the CLI: the number comes from the sequence, the snapshot is the frozen document,
# and the subledger posting is the code's own.
say "invoices"
issue() { # customer amount tax-rate
  local out number
  out="$(./bin/billing admin issue-invoice --customer "$1" --amount "$2" --tax-rate "$3" 2>&1)"
  number="$(sed -n 's/^issued \(INV-[0-9]*\):.*/\1/p' <<<"$out")"
  if [[ -z "$number" ]]; then
    echo "seed-demo: could not issue an invoice for $1: $out" >&2
    exit 1
  fi
  printf '%s' "$number"
}

INV_PAID_1="$(issue "$C1" 40.00 20%)"   # Northwind, settled last month
INV_PAID_2="$(issue "$C1" 40.00 20%)"   # Northwind, settled two months ago
INV_OPEN_1="$(issue "$C1" 40.00 20%)"   # Northwind, current, due in 5 days
INV_OVERDUE="$(issue "$C2" 24.00 20%)"  # Acme, overdue: the dunning case
INV_PAID_3="$(issue "$C3" 12.00 20%)"   # Priya, settled
INV_OPEN_2="$(issue "$C3" 12.00 20%)"   # Priya, due soon
INV_OPEN_3="$(issue "$C4" 18.00 20%)"   # Jonas, first invoice
INV_PAID_4="$(issue "$C5" 80.00 20%)"   # Sakura, settled then partly refunded
INV_PAID_5="$(issue "$C6" 20.00 20%)"   # Fixture Co, settled
INV_VOID="$(issue "$C5" 15.00 20%)"     # Sakura, issued in error, voided

# Dates and lifecycle.
#
# A demo needs history, and history is exactly what the immutability trigger refuses to let us
# write: `issued_at` is one of the columns an issued invoice may never change. Rather than fake the
# dates in the snapshot (which would make the document disagree with its own row), the trigger is
# lifted for the length of one transaction and restored inside it. ALTER TABLE is transactional, so
# a failure here rolls the trigger back on rather than leaving the guard disabled.
#
# Nothing but the two timestamps is touched: totals, snapshot and identity stay exactly as issued.
psql_q <<SQL
begin;
alter table invoices disable trigger invoices_guard_issued;
update invoices set issued_at = now() - interval '60 days', due_at = now() - interval '53 days' where number = '$INV_PAID_2';
update invoices set issued_at = now() - interval '30 days', due_at = now() - interval '23 days' where number = '$INV_PAID_1';
update invoices set issued_at = now() - interval '5 days',  due_at = now() + interval '5 days'  where number = '$INV_OPEN_1';
update invoices set issued_at = now() - interval '20 days', due_at = now() - interval '10 days' where number = '$INV_OVERDUE';
update invoices set issued_at = now() - interval '35 days', due_at = now() - interval '28 days' where number = '$INV_PAID_3';
update invoices set issued_at = now() - interval '3 days',  due_at = now() + interval '7 days'  where number = '$INV_OPEN_2';
update invoices set issued_at = now() - interval '2 days',  due_at = now() + interval '12 days' where number = '$INV_OPEN_3';
update invoices set issued_at = now() - interval '40 days', due_at = now() - interval '33 days' where number = '$INV_PAID_4';
update invoices set issued_at = now() - interval '70 days', due_at = now() - interval '63 days' where number = '$INV_PAID_5';
update invoices set issued_at = now() - interval '9 days',  due_at = now() - interval '2 days'  where number = '$INV_VOID';
alter table invoices enable trigger invoices_guard_issued;
commit;
SQL

# Settle the ones that are paid, with a payment, an allocation and the journal pair the billing
# service writes: cash_clearing debit, accounts_receivable credit.
settle() { # number provider-id days-ago
  local number="$1" ref="$2" days="$3"
  # One transaction: the journal is checked by a DEFERRED constraint trigger, which fires at COMMIT.
  # Written as separate statements each would commit on its own, and the transaction row would be
  # rejected for having no entries yet.
  psql_q <<SQL
begin;
with inv as (select id, customer_id, currency, total_minor from invoices where number = '$number'),
     p as (insert into payments (provider, provider_account, provider_payment_id, amount_minor, currency, status, received_at)
           select 'stripe', 'acct_demo', '$ref', total_minor, currency, 'succeeded', now() - interval '$days days' from inv
           returning id, currency, amount_minor)
insert into payment_allocations (payment_id, invoice_id, amount_minor, currency)
select p.id, inv.id, p.amount_minor, p.currency from p, inv;

with inv as (select id, customer_id, currency, total_minor from invoices where number = '$number')
insert into journal_transactions (kind, reference_type, reference_id, currency, created_at)
select 'payment_received', 'payment', p.id, inv.currency, now() - interval '$days days'
from inv join payments p on p.provider_payment_id = '$ref';

with t as (select jt.id, jt.currency, i.customer_id, i.total_minor
           from journal_transactions jt
           join payments p on p.id = jt.reference_id
           join invoices i on i.number = '$number'
           where p.provider_payment_id = '$ref')
insert into journal_entries (transaction_id, account_code, currency, debit_minor, credit_minor, memo, customer_id)
select id, 'cash_clearing', currency, total_minor, 0, 'payment $number', customer_id from t
union all
select id, 'ar', currency, 0, total_minor, 'payment $number', customer_id from t;

update invoices set status = 'paid', amount_paid_minor = total_minor where number = '$number';
commit;
SQL
}

settle "$INV_PAID_2" "pi_demo_northwind_2" 53
settle "$INV_PAID_1" "pi_demo_northwind_1" 23
settle "$INV_PAID_3" "pi_demo_priya_1"     28
settle "$INV_PAID_4" "pi_demo_sakura_1"    33
settle "$INV_PAID_5" "pi_demo_fixture_1"   63

psql_q <<SQL
update invoices set status = 'void', void_reason = 'Issued against the wrong customer; replaced by the annual invoice.' where number = '$INV_VOID';
SQL

# ------------------------------------------------------------- dunning --
say "dunning"
psql_q <<SQL
insert into dunning_states (invoice_id, state, step, next_action_at, last_action_at)
select id, 'exhausted', 3, now() + interval '2 days', now() - interval '8 days'
from invoices where number = '$INV_OVERDUE';

insert into suspension_decisions (service_id, invoice_id, decision, reason, generation, decided_by, created_at)
select '$SV2', id, 'suspend', 'Invoice unpaid after two reminders and a final notice.', 7, '$S_BILLING', now() - interval '8 days'
from invoices where number = '$INV_OVERDUE';
SQL

# ---------------------------------------------------- refunds and credit --
say "refunds and credit notes"
REFUND_ID="$(uuidgen)"
psql_q <<SQL
begin;
insert into refunds (id, payment_id, provider_refund_id, operation_key, amount_minor, currency, status, created_at, reason, requested_by, approved_by, finished_at)
select '$REFUND_ID', p.id, 're_demo_sakura_1', 'refund:demo-sakura-1', 2000, p.currency, 'succeeded',
       now() - interval '20 days', 'Downgraded from the annual to the monthly plan mid-term.', '$S_BILLING', '$S_ADMIN', now() - interval '20 days'
from payments p where p.provider_payment_id = 'pi_demo_sakura_1';

insert into journal_transactions (kind, reference_type, reference_id, currency, created_at)
select 'refund', 'refund', '$REFUND_ID', p.currency, now() - interval '20 days'
from payments p where p.provider_payment_id = 'pi_demo_sakura_1';

with t as (select jt.id, jt.currency, i.customer_id, 2000::bigint as amount
           from journal_transactions jt
           join invoices i on i.number = '$INV_PAID_4'
           where jt.reference_id = '$REFUND_ID')
insert into journal_entries (transaction_id, account_code, currency, debit_minor, credit_minor, memo, customer_id)
select id, 'refunds', currency, amount, 0, 'refund for $INV_PAID_4', customer_id from t
union all
select id, 'cash_clearing', currency, 0, amount, 'refund for $INV_PAID_4', customer_id from t;

update payments set status = 'partially_refunded' where provider_payment_id = 'pi_demo_sakura_1';
update invoices set status = 'refunded' where number = '$INV_PAID_4';
commit;
SQL

CN_ID="$(uuidgen)"
# The number comes from the same persisted series the issuer uses, so a seeded credit note cannot
# collide with one issued later from the UI.
CN_NUMBER="$(psql_t -c "insert into invoice_number_sequences (scope, next_value) values ('credit_note', 2) on conflict (scope) do update set next_value = invoice_number_sequences.next_value + 1 returning 'CN-'||lpad((next_value - 1)::text, 6, '0')")"
psql_q <<SQL
begin;
insert into credit_notes (id, number, customer_id, invoice_id, currency, total_minor, tax_minor, reason, status, issued_at, snapshot, created_by)
select '$CN_ID', '$CN_NUMBER', '$C2', i.id, 'USD', 1200, 200,
       'Goodwill credit after the outage on the 3rd.', 'issued', now() - interval '6 days',
       jsonb_build_object('number','$CN_NUMBER','reason','Goodwill credit after the outage on the 3rd.','total_minor',1200), '$S_BILLING'
from invoices i where i.number = '$INV_OVERDUE';

insert into journal_transactions (kind, reference_type, reference_id, currency, created_at)
values ('credit_note', 'credit_note', '$CN_ID', 'USD', now() - interval '6 days');

insert into journal_entries (transaction_id, account_code, currency, debit_minor, credit_minor, memo, customer_id)
select id, 'revenue', 'USD', 1000, 0, 'credit note $CN_NUMBER', '$C2'::uuid from journal_transactions where reference_id = '$CN_ID'
union all
select id, 'tax_payable', 'USD', 200, 0, 'credit note $CN_NUMBER', '$C2'::uuid from journal_transactions where reference_id = '$CN_ID'
union all
select id, 'ar', 'USD', 0, 1200, 'credit note $CN_NUMBER', '$C2'::uuid from journal_transactions where reference_id = '$CN_ID';
commit;
SQL

# ---------------------------------------------------------------- tickets --
say "tickets"
T1="$(uuidgen)"; T2="$(uuidgen)"; T3="$(uuidgen)"
psql_q <<SQL
insert into tickets (id, number, customer_id, user_id, service_id, subject, department, priority, status, assigned_to, opened_by, last_reply_at, closed_at, created_at, updated_at) values
  ('$T1','TCK-000001','$C2','$U2','$SV2','Server suspended but the invoice was paid','billing','high','replied','$S_SUPPORT','$U2', now() - interval '1 day', null, now() - interval '3 days', now() - interval '1 day'),
  ('$T2','TCK-000002','$C4','$U4','$SV4','Provisioning has been stuck for two days','technical','normal','open',null,'$U4', now() - interval '6 hours', null, now() - interval '6 hours', now() - interval '6 hours'),
  ('$T3','TCK-000003','$C1','$U1','$SV1','Please confirm the backup retention window','general','low','closed','$S_SUPPORT','$U1', now() - interval '12 days', now() - interval '10 days', now() - interval '13 days', now() - interval '10 days');

insert into ticket_messages (ticket_id, author_user_id, from_staff, body, created_at) values
  ('$T1','$U2', false, 'Our Minecraft server went offline this morning and the panel says suspended. We paid invoice on the 12th — see the payment in the portal. Please resume it.', now() - interval '3 days'),
  ('$T1','$S_SUPPORT', true, 'Thanks for the report. I can see the payment settled but the suspension had already been queued. I have asked billing to confirm the allocation before we resume.', now() - interval '2 days'),
  ('$T1','$S_BILLING', true, 'Confirmed: the payment allocated to the overdue invoice, so the suspension was stale. Resuming now, and the credit note covers the outage window.', now() - interval '1 day'),
  ('$T2','$U4', false, 'I ordered the Valheim server two days ago and the status still says provisioning. The order page shows it as fulfilling. Is something stuck?', now() - interval '6 hours'),
  ('$T3','$U1', false, 'How many daily backups do we keep, and can we restore to a specific hour?', now() - interval '13 days'),
  ('$T3','$S_SUPPORT', true, 'Fourteen daily snapshots, off-site, and restores are point-in-time to the hour. Closing this as answered — reopen if you need more.', now() - interval '10 days');
SQL

# ---------------------------------------------------------- notifications --
say "notifications"
psql_q <<SQL
insert into notifications (user_id, event_key, title, body, url, read_at, created_at) values
  ('$U2','invoice.overdue','Invoice overdue','Invoice $INV_OVERDUE is 10 days past due. Your server will stay suspended until it is settled.','/portal/invoices', null, now() - interval '10 days'),
  ('$U2','credit_note.issued','Credit note issued','A credit of USD 12.00 was applied to your account.','/portal/invoices', null, now() - interval '6 days'),
  ('$U4','service.provisioning','Your server is being created','We are setting up Valheim — Duo. This usually takes a few minutes.','/portal/services', null, now() - interval '2 days'),
  ('$U1','invoice.paid','Payment received','We received your payment for $INV_PAID_1. Thank you.','/portal/invoices', now() - interval '22 days', now() - interval '23 days'),
  ('$U1','service.ready','Your VPS is ready','Production VPS is online and accepting connections.','/portal/services', now() - interval '390 days', now() - interval '400 days');
SQL

# ------------------------------------------------------------- webhooks --
say "webhook inbox"
psql_q <<SQL
insert into gateway_events (provider, provider_account, event_id, event_type, payload, signature_valid, received_at, processed_at, attempts) values
  ('stripe','acct_demo','evt_demo_settled_1','checkout.session.completed',
   jsonb_build_object('id','evt_demo_settled_1','type','checkout.session.completed','data',jsonb_build_object('object',jsonb_build_object('id','cs_demo_1','payment_intent','pi_demo_northwind_1','amount_total',4800,'currency','usd'))),
   true, now() - interval '23 days', now() - interval '23 days', 1),
  ('stripe','acct_demo','evt_demo_settled_2','checkout.session.completed',
   jsonb_build_object('id','evt_demo_settled_2','type','checkout.session.completed','data',jsonb_build_object('object',jsonb_build_object('id','cs_demo_2','payment_intent','pi_demo_priya_1','amount_total',1440,'currency','usd'))),
   true, now() - interval '28 days', now() - interval '28 days', 1),
  ('stripe','acct_demo','evt_demo_unhandled_1','customer.updated',
   jsonb_build_object('id','evt_demo_unhandled_1','type','customer.updated','data',jsonb_build_object('object',jsonb_build_object('id','cus_demo_1'))),
   true, now() - interval '2 days', now() - interval '2 days', 1);
SQL

# ---------------------------------------------------------------- audit --
say "audit trail"
psql_q <<SQL
insert into audit_events (actor_id, actor_kind, action, target_type, target_id, outcome, changes, created_at) values
  ('$S_ADMIN','user','invoice.issue','invoice',(select id from invoices where number='$INV_OPEN_1'),'success','{"amount":"40.00","tax_rate":"20%"}'::jsonb, now() - interval '5 days'),
  ('$S_BILLING','user','refund.create','refund','$REFUND_ID','success','{"amount":"20.00","reason":"plan downgrade"}'::jsonb, now() - interval '20 days'),
  ('$S_BILLING','user','invoice.void','invoice',(select id from invoices where number='$INV_VOID'),'success','{"reason":"wrong customer"}'::jsonb, now() - interval '9 days'),
  ('$S_SUPPORT','user','ticket.reply','ticket','$T1','success','{}'::jsonb, now() - interval '1 day'),
  ('$S_INFRA','user','service.terminate','service','$SV6','success','{"external_id":"vps-fixture-01"}'::jsonb, now() - interval '15 days');
SQL

# ------------------------------------------------------------ affiliates --
say "affiliates"
psql_q <<SQL
insert into affiliates (user_id, code, commission_rate, payout_method, enabled)
select '$U1', 'NORTHWIND', '10', 'credit', true
where not exists (select 1 from affiliates where code = 'NORTHWIND');
SQL

# ------------------------------------------------------------- verification --
say "verifying"
BALANCE="$(psql_t -c "select coalesce(string_agg(currency||':'||total, ', '), 'none') from (select currency, sum(debit_minor - credit_minor) as total from journal_entries group by currency) s")"
echo "  ledger per currency (must all be 0): $BALANCE"
if [[ "$BALANCE" != "none" ]]; then
  bad="$(psql_t -c "select count(*) from (select currency, sum(debit_minor-credit_minor) t from journal_entries group by currency) s where t <> 0")"
  if [[ "$bad" != "0" ]]; then
    echo "seed-demo: the ledger does not balance; refusing to report success" >&2
    exit 1
  fi
fi

psql_t -c "
select 'customers='||(select count(*) from customers)
     ||' users='||(select count(*) from users)
     ||' products='||(select count(*) from products)
     ||' invoices='||(select count(*) from invoices)
     ||' paid='||(select count(*) from invoices where status='paid')
     ||' open='||(select count(*) from invoices where status='open')
     ||' services='||(select count(*) from services)
     ||' payments='||(select count(*) from payments)
     ||' tickets='||(select count(*) from tickets)
     ||' notifications='||(select count(*) from notifications)" | sed 's/^/  /'

cat <<EOF

  Demo data ready. Every portal login uses the same password.

    staff      admin@dev.local        / ${DEV_ADMIN_PASSWORD:-dev-only-password-1}
               support@$DOMAIN        / $STAFF_PASSWORD   (support)
               billing@$DOMAIN        / $STAFF_PASSWORD   (billing)
               infra@$DOMAIN          / $STAFF_PASSWORD   (infrastructure)

    customers  northwind@$DOMAIN      / $PASSWORD
               acme@$DOMAIN           / $PASSWORD   (overdue + suspended server)
               priya@$DOMAIN          / $PASSWORD
               jonas@$DOMAIN          / $PASSWORD   (provisioning in progress)
               sakura@$DOMAIN         / $PASSWORD   (refund + credit note)
               fixture@$DOMAIN        / $PASSWORD   (cancelled + terminated)

EOF
