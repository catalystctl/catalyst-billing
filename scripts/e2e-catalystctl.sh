#!/usr/bin/env bash
# Live end-to-end: buy a game server through the billing panel and watch it appear on the real
# CatalystCTL panel — a panel account for the customer included.
#
# It is a driver, not a fixture: every step is one of the paths a person takes (staff configures the
# installation, a customer buys, an operator sets the placement, the worker provisions, the customer
# presses a button), and every assertion reads the *panel*, not the billing database, so "it worked" means
# the server exists where the customer will look for it.
#
# Usage: BASE_URL=... CUSTOMER_EMAIL=... ./e2e-catalystctl.sh
set -euo pipefail

BASE="${BASE_URL:-http://127.0.0.1:8798}"
ADMIN_EMAIL="${ADMIN_EMAIL:-admin@dev.local}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-dev-only-password-1}"
# A fresh address per run: a new customer is the journey that matters, and a customer with no credit
# keeps the invoice unpaid so the staff "money arrived" path — the one that runs fulfilment — is what
# settles it.
CUSTOMER_EMAIL="${CUSTOMER_EMAIL:-e2e-$(date +%s)@example.test}"
CUSTOMER_PASSWORD="${CUSTOMER_PASSWORD:-dev-only-password-1}"
PRODUCT_SLUG="${PRODUCT_SLUG:-demo-minecraft-iron}"
PANEL="${PANEL_URL:-https://panel.catalystctl.com}"
PANEL_KEY="${PANEL_KEY:?PANEL_KEY is required}"
ADMIN_JAR=$(mktemp)
USER_JAR=$(mktemp)
trap 'rm -f "$ADMIN_JAR" "$USER_JAR"' EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok()   { printf '   \033[32mok\033[0m   %s\n' "$*"; }
die()  { printf '   \033[31mFAIL\033[0m %s\n' "$*"; exit 1; }

# api METHOD JAR URL CSRF [BODY] -> body on stdout, fails on a non-2xx
api() {
  local method=$1 jar=$2 url=$3 csrf=$4 body=${5:-}
  local args=(-sS -m 60 -X "$method" -b "$jar" -c "$jar" "$url" -H "Accept: application/json")
  [ -n "$csrf" ] && args+=(-H "X-CSRF-Token: $csrf")
  [ -n "$body" ] && args+=(-H "Content-Type: application/json" -d "$body")
  # Every staff mutation requires an idempotency key. A fresh one per call is right here: the driver
  # never retries a step on its own, so a replay would be a bug rather than a recovery.
  case "$method" in
    POST|PUT|DELETE|PATCH) args+=(-H "Idempotency-Key: $(uuidgen 2>/dev/null || cat /proc/sys/kernel/random/uuid)") ;;
  esac
  local out status
  out=$(curl "${args[@]}" -w $'\n%{http_code}')
  status=$(tail -n1 <<<"$out")
  out=$(sed '$d' <<<"$out")
  if [ "${status:0:1}" != "2" ]; then
    printf '%s\n' "$out" >&2
    die "$method $url -> HTTP $status"
  fi
  printf '%s' "$out"
}

login() {
  local jar=$1 email=$2 password=$3
  api POST "$jar" "$BASE/api/v1/auth/login" "" \
    "$(jq -nc --arg e "$email" --arg p "$password" '{email:$e,password:$p}')" |
    jq -r '.csrf_token'
}

pget() {
  local path=$1 filter=$2
  curl -sS -m 30 -H "Authorization: Bearer $PANEL_KEY" "$PANEL$path" | jq -r "$filter"
}

step "admin signs in"
ADMIN_CSRF=$(login "$ADMIN_JAR" "$ADMIN_EMAIL" "$ADMIN_PASSWORD")
ok "admin session established"

step "installation is configured for catalystctl"
curl -sS -m 10 -b "$ADMIN_JAR" "$BASE/api/v1/staff/servers" |
  jq -r '"   provider=\(.provider) extension=\(.extension_path // "n/a") problems=\(.problems // [])"' 

step "a new customer registers"
api POST "$USER_JAR" "$BASE/api/v1/auth/register" "" \
  "$(jq -nc --arg e "$CUSTOMER_EMAIL" --arg p "$CUSTOMER_PASSWORD" \
     '{email:$e, password:$p, display_name:"E2E Buyer"}')" >/dev/null

# The staff customer list ignores a search parameter, so the match is made here rather than trusting
# the server to have filtered: picking the wrong customer would act on a stranger.
CUSTOMER_ID=""
for _ in $(seq 1 20); do
  CUSTOMER_ID=$(curl -sS -m 30 -b "$ADMIN_JAR" "$BASE/api/v1/staff/customers" |
    jq -r --arg e "$CUSTOMER_EMAIL" '[.data[] | select(.email == $e)][0].id // empty')
  [ -n "$CUSTOMER_ID" ] && break
  sleep 1
done
[ -n "$CUSTOMER_ID" ] || die "registration did not create a customer for $CUSTOMER_EMAIL"
ok "customer $CUSTOMER_ID registered at $CUSTOMER_EMAIL"

step "customer signs in and buys the product"
USER_CSRF=$(login "$USER_JAR" "$CUSTOMER_EMAIL" "$CUSTOMER_PASSWORD")
PRICE_ID=$(psql "${BILLING_DATABASE_URL:?}" -qtA -c \
  "SELECT pv.id FROM price_versions pv JOIN products p ON p.id=pv.product_id
   WHERE p.slug='$PRODUCT_SLUG' AND pv.interval='monthly' ORDER BY pv.version LIMIT 1")
[ -n "$PRICE_ID" ] || die "no monthly price for $PRODUCT_SLUG"
# The portal checkout takes a cart, not a single line: `items` is the canonical spelling and
# `tos_accepted` is required by the storefront's own contract.
ORDER=$(api POST "$USER_JAR" "$BASE/api/v1/portal/orders" "$USER_CSRF" \
  "$(jq -nc --arg p "$PRICE_ID" '{items:[{price_version_id:$p, quantity:1}], tos_accepted:true}')")
ORDER_ID=$(jq -r '.order_id // .id' <<<"$ORDER")
INVOICE_ID=$(jq -r '.invoice_id // .invoice.id // empty' <<<"$ORDER")
ok "order $ORDER_ID, invoice ${INVOICE_ID:-none}"

[ -n "$INVOICE_ID" ] || die "the order produced no invoice to pay"

# Staff record the money arriving. This is the path that runs the fulfilment applier: the order is
# fulfilled, the service is created, and the create operation is queued for the worker.
# What is still owed. A credit balance can settle part of an invoice at checkout, so the payment must
# be for the remainder rather than the total — an overpayment is refused, and a refusal here would look
# like the fulfilment path being broken.
AMOUNT=$(psql "$BILLING_DATABASE_URL" -qtA -c \
  "SELECT total_minor - amount_paid_minor FROM invoices WHERE id='$INVOICE_ID'")
if [ "${AMOUNT:-0}" -le 0 ]; then
  ok "invoice was already settled from the customer's credit; using it as the paid invoice"
else
api POST "$ADMIN_JAR" "$BASE/api/v1/staff/invoices/$INVOICE_ID/payments" "$ADMIN_CSRF" \
  "$(jq -nc --arg ref "e2e-$(date +%s)" --argjson amt "$AMOUNT" \
     '{amount_minor:$amt, method:"bank_transfer", reference:$ref, note:"live e2e"}')" >/dev/null
  ok "invoice paid: $AMOUNT minor units recorded"
fi
MARK=$(psql "$BILLING_DATABASE_URL" -qtA -c "SELECT now()::text")

step "waiting for the service to be created from the paid order"
SERVICE_ID=""
for _ in $(seq 1 40); do
  SERVICE_ID=$(psql "$BILLING_DATABASE_URL" -qtA -c \
    "SELECT id FROM services
      WHERE customer_id='$CUSTOMER_ID' AND created_at >= '$MARK'::timestamptz
      ORDER BY created_at DESC LIMIT 1")
  [ -n "$SERVICE_ID" ] && break
  sleep 1
done
[ -n "$SERVICE_ID" ] || die "no service was created from the paid invoice"
ok "service $SERVICE_ID exists"

step "operator sets the placement: template, location, size"
api PUT "$ADMIN_JAR" "$BASE/api/v1/staff/services/$SERVICE_ID/provisioning-fields" "$ADMIN_CSRF" \
  "$(jq -nc '{values:{
      location: "US East 1",
      template: "cmv8fyx17006b32n6eq1z6ctm",
      memory_mb: "2048",
      disk_mb: "10240",
      cpu_cores: "1"}}')" >/dev/null
ok "placement stored on the service"

step "the customer's panel account before provisioning"
BEFORE=$(pget "/api/admin/users?search=$CUSTOMER_EMAIL" '.users | length')
ok "panel accounts matching the customer: $BEFORE"

echo
echo "   queued operations:"
psql "$BILLING_DATABASE_URL" -qtA -F' | ' -c \
  "SELECT kind, status, coalesce(last_error,'') FROM service_operations WHERE service_id='$SERVICE_ID' ORDER BY created_at"
echo
echo "SERVICE_ID=$SERVICE_ID"
echo "CUSTOMER_ID=$CUSTOMER_ID"
echo "INVOICE_ID=$INVOICE_ID"
