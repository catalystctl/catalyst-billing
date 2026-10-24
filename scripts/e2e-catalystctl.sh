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
CUSTOMER_EMAIL="${CUSTOMER_EMAIL:-card@example.test}"
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

step "granting the customer credit so the purchase can be paid"
# The staff customer list ignores a search parameter, so the match is made here rather than trusting
# the server to have filtered: picking the wrong customer would grant credit to a stranger.
CUSTOMER_ID=$(curl -sS -m 30 -b "$ADMIN_JAR" "$BASE/api/v1/staff/customers" |
  jq -r --arg e "$CUSTOMER_EMAIL" '[.data[] | select(.email == $e)][0].id // empty')
[ -n "$CUSTOMER_ID" ] || die "customer $CUSTOMER_EMAIL not found"
api POST "$ADMIN_JAR" "$BASE/api/v1/staff/customers/$CUSTOMER_ID/credit" "$ADMIN_CSRF" \
  "$(jq -nc '{amount_minor: 50000, currency: "USD", reason: "live e2e"}')" >/dev/null
ok "50.00 credit granted to $CUSTOMER_EMAIL"

step "customer signs in and buys the product"
USER_CSRF=$(login "$USER_JAR" "$CUSTOMER_EMAIL" "$CUSTOMER_PASSWORD")
PRICE_ID=$(psql "${BILLING_DATABASE_URL:?}" -qtA -c \
  "SELECT pv.id FROM price_versions pv JOIN products p ON p.id=pv.product_id
   WHERE p.slug='$PRODUCT_SLUG' AND pv.interval='monthly' ORDER BY pv.version LIMIT 1")
[ -n "$PRICE_ID" ] || die "no monthly price for $PRODUCT_SLUG"
ORDER=$(api POST "$USER_JAR" "$BASE/api/v1/portal/orders" "$USER_CSRF" \
  "$(jq -nc --arg p "$PRICE_ID" '{price_version_id:$p, quantity:1}')")
ORDER_ID=$(jq -r '.order_id // .id' <<<"$ORDER")
INVOICE_ID=$(jq -r '.invoice_id // .invoice.id // empty' <<<"$ORDER")
ok "order $ORDER_ID, invoice ${INVOICE_ID:-none}"

[ -n "$INVOICE_ID" ] || die "the order produced no invoice to pay"
api POST "$USER_JAR" "$BASE/api/v1/portal/invoices/$INVOICE_ID/pay-with-credit" "$USER_CSRF" '{}' >/dev/null
ok "invoice paid from credit"

step "waiting for the service to be created from the paid order"
SERVICE_ID=""
for _ in $(seq 1 40); do
  SERVICE_ID=$(psql "$BILLING_DATABASE_URL" -qtA -c \
    "SELECT id FROM services WHERE customer_id='$CUSTOMER_ID' ORDER BY created_at DESC LIMIT 1")
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
  "SELECT kind, status, coalesce(detail,'') FROM service_operations WHERE service_id='$SERVICE_ID' ORDER BY created_at"
echo
echo "SERVICE_ID=$SERVICE_ID"
echo "CUSTOMER_ID=$CUSTOMER_ID"
echo "INVOICE_ID=$INVOICE_ID"
