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
TEMPLATE_ID="${TEMPLATE_ID:-cmv8fyx17006b32n6eq1z6ctm}"
NODE_ID="${NODE_ID:-cmv8godh6000l32pgi40ecwg6}"
LOCATION_NAME="${LOCATION_NAME:-US East 1}"
EXTENSION="${BILLING_PROVISIONER_EXTENSION:-$PWD/bin/catalystctl}"
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

step "staff configure the installation: the panel's address and key"
# The two things that belong to the installation rather than to a product. Everything about where a
# server goes is set per service further down.
api PUT "$ADMIN_JAR" "$BASE/api/v1/staff/servers" "$ADMIN_CSRF" \
  "$(jq -nc --arg u "$PANEL" --arg k "$PANEL_KEY" \
     '{provider:"catalystctl", values:{panel_url:$u, api_key:$k}}')" >/dev/null
INTEGRATION=$(curl -sS -m 10 -b "$ADMIN_JAR" "$BASE/api/v1/staff/servers")
PROVIDER=$(jq -r '.provider' <<<"$INTEGRATION")
[ "$PROVIDER" = "catalystctl" ] || die "the provider is $PROVIDER, not catalystctl"
jq -r '"   provider=\(.provider) extension=\(.extension_path // "n/a") problems=\(.problems // [])"' <<<"$INTEGRATION"
# The extension is resolved from the bin directory, so a missing binary is a real configuration
# problem and is worth catching here rather than in the queue.
EXT=$(jq -r '.extension_path // empty' <<<"$INTEGRATION")
if [ -n "$EXT" ]; then
  [ -x "$EXT" ] || die "the configured extension $EXT is not executable"
  ok "extension resolved to $EXT"
fi

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
MARK=$(psql "$BILLING_DATABASE_URL" -qtA -c "SELECT clock_timestamp()::text")
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

step "the panel can actually place a server (IP pools)"
# A panel assigns each server an address from an IP pool, one per network. With no pool the panel
# refuses every create with SERVER_CREATE_FAILED "No IP pool configured for this network" — a
# prerequisite worth naming before a purchase fails five times in a queue.
BRIDGE_POOL=$(pget "/api/admin/ip-pools" '[.data[]? | select(.networkName=="bridge")] | length')
if [ "${BRIDGE_POOL:-0}" -lt 1 ]; then
  die "the panel has no bridge IP pool, so it cannot assign an address to a new server.
   Add one on the panel (Admin → IP pools): networkName=bridge and a CIDR that cannot collide
   with the node's own network."
fi
ok "bridge IP pool present: $(pget "/api/admin/ip-pools" '[.data[]? | select(.networkName=="bridge") | .cidr] | join(", ")')"

step "the service's declared placement fields come from the panel"
DECLARED=$(api GET "$ADMIN_JAR" "$BASE/api/v1/staff/services/$SERVICE_ID/provisioning-fields" "")
jq -r '.fields[]? | "   \(.key)  \(.type)  \(if .required then "required" else "optional" end)  \((.choices // []) | length) choices"' <<<"$DECLARED"
LOC_CHOICES=$(jq -r '[.fields[]? | select(.key=="location") | .choices[]?] | join(", ")' <<<"$DECLARED")
[ -n "$LOC_CHOICES" ] || die "the location dropdown is empty; the adapter could not list the panel"
case "$LOC_CHOICES" in
  *"$LOCATION_NAME"*) ok "the location dropdown lists $LOCATION_NAME" ;;
  *) die "the location dropdown does not contain $LOCATION_NAME (got: $LOC_CHOICES)" ;;
esac

step "operator sets the placement: template, location, size"
api PUT "$ADMIN_JAR" "$BASE/api/v1/staff/services/$SERVICE_ID/provisioning-fields" "$ADMIN_CSRF" \
  "$(jq -nc --arg loc "$LOCATION_NAME" --arg tpl "$TEMPLATE_ID" --arg node "$NODE_ID" '{values:{
      location: $loc,
      template: $tpl,
      node: $node,
      memory_mb: "2048",
      disk_mb: "10240",
      cpu_cores: "1"}}')" >/dev/null
ok "placement stored on the service"

step "the worker provisions the service (no embedded worker in serve)"
WORKER_LOG=/tmp/e2e-worker.log
BILLING_PROVISIONER_EXTENSION="$EXTENSION" timeout 60 ./bin/billing worker >"$WORKER_LOG" 2>&1 &
WORKER_PID=$!

SERVICE_ACTIVE=""
for _ in $(seq 1 40); do
  if ! kill -0 "$WORKER_PID" 2>/dev/null && [ -z "$SERVICE_ACTIVE" ]; then
    : # the worker exited; the assertions below will say what happened
  fi
  ROW=$(psql "$BILLING_DATABASE_URL" -qtA -F'|' -c \
    "SELECT status, COALESCE(external_id,'') FROM services WHERE id='$SERVICE_ID'")
  case "$ROW" in
    active\|*) SERVICE_ACTIVE=${ROW#*|}; break ;;
  esac
  sleep 1
done
kill "$WORKER_PID" 2>/dev/null || true
wait "$WORKER_PID" 2>/dev/null || true

step "operations"
psql "$BILLING_DATABASE_URL" -qtA -F' | ' -c \
  "SELECT kind, status, attempts, coalesce(last_error,'') FROM service_operations
    WHERE service_id='$SERVICE_ID' ORDER BY created_at"

[ -n "$SERVICE_ACTIVE" ] || die "the service was not provisioned (see $WORKER_LOG)"
ok "service is active with external id $SERVICE_ACTIVE"

step "the panel: a customer account, and a server owned by it"
PANEL_USER=$(pget "/api/admin/users?search=$CUSTOMER_EMAIL" \
  "[.users[] | select(.email == \"$CUSTOMER_EMAIL\")][0].id // empty")
[ -n "$PANEL_USER" ] || die "no panel account was created for $CUSTOMER_EMAIL"
ok "panel account $PANEL_USER exists for the customer"

pget "/api/servers/$SERVICE_ACTIVE" \
  '"   name=\(.data.name) state=\(.data.status) owner=\(.data.ownerId) template=\(.data.templateId) node=\(.data.nodeId) location=\(.data.locationId) memory=\(.data.allocatedMemoryMb)MB port=\(.data.primaryPort)"'

OWNER=$(pget "/api/servers/$SERVICE_ACTIVE" '.data.ownerId')
[ "$OWNER" = "$PANEL_USER" ] || die "the server is owned by $OWNER, not the customer's account $PANEL_USER"
ok "the server belongs to the customer's panel account"

TEMPLATE=$(pget "/api/servers/$SERVICE_ACTIVE" '.data.templateId')
[ "$TEMPLATE" = "$TEMPLATE_ID" ] || die "template is $TEMPLATE, want $TEMPLATE_ID"
NODE=$(pget "/api/servers/$SERVICE_ACTIVE" '.data.nodeId')
[ "$NODE" = "$NODE_ID" ] || die "node is $NODE, want $NODE_ID"
MEMORY=$(pget "/api/servers/$SERVICE_ACTIVE" '.data.allocatedMemoryMb')
[ "$MEMORY" = "2048" ] || die "memory is $MEMORY, want 2048"
ok "placed on the requested template, node and size"

step "bridge networking: the panel reserved a host port for this server"
MODE=$(pget "/api/servers/$SERVICE_ACTIVE" '.data.networkMode')
[ "$MODE" = "bridge" ] || die "network mode is $MODE, want bridge"
ok "network mode is bridge"
# The whole reason for bridge: the panel holds an allocation for the server, which is what stops two
# creates from colliding on one port and what gives the customer a stable address.
# The resource's own allocation list reports the reserved host port, which is the thing the customer
# connects to and the thing a second create cannot take.
HOST_PORT=$(pget "/api/servers/$SERVICE_ACTIVE/allocations" '[.data[]? | select(.isPrimary) | .hostPort][0] // empty')
[ -n "$HOST_PORT" ] || die "the server holds no primary allocation; its port is not reserved"
ok "the panel reserved host port $HOST_PORT for it"
# And the node's table agrees that the allocation belongs to this server, not merely that a timer says so.
OWNER_OF_PORT=$(pget "/api/nodes/$NODE_ID/allocations" "[.data[]? | select(.port == $HOST_PORT) | .serverId][0] // empty")
[ "$OWNER_OF_PORT" = "$SERVICE_ACTIVE" ] ||
  die "port $HOST_PORT is held by $OWNER_OF_PORT, not this server"
ok "the node's allocation table binds port $HOST_PORT to this server"

step "the customer's portal card reports the same server"
PORTAL_CSRF=$(login "$USER_JAR" "$CUSTOMER_EMAIL" "$CUSTOMER_PASSWORD")
CARD=$(api GET "$USER_JAR" "$BASE/api/v1/portal/services/$SERVICE_ID/server" "")
jq -r '"   name=\(.name) state=\(.state) address=\(.address) can_power=\(.can_power // []) link=\(.panel_url // "none")"' <<<"$CARD"
CARD_NAME=$(jq -r '.name // empty' <<<"$CARD")
[ -n "$CARD_NAME" ] || die "the card reported no server name"
jq -e '.available == true and (.address | length > 0) and (.panel_url | length > 0)' <<<"$CARD" >/dev/null ||
  die "the card is missing one of: available, address, panel link"
ok "the card shows the name, address and panel link"

step "the customer starts the server from the billing panel"
BEFORE_STATE=$(pget "/api/servers/$SERVICE_ACTIVE" '.data.status')
api POST "$USER_JAR" "$BASE/api/v1/portal/services/$SERVICE_ID/server/power" "$PORTAL_CSRF" \
  '{"action":"start"}' >/dev/null
sleep 3
AFTER_STATE=$(pget "/api/servers/$SERVICE_ACTIVE" '.data.status')
ok "panel state $BEFORE_STATE -> $AFTER_STATE"
# What this proves is that the action reached the panel and changed the server's state. Whether the
# process keeps running is the panel's business: a template whose game has not been installed yet comes up
# and dies ("crashed"), exactly as it would from the panel's own console. Asserting "running" would make
# this test a test of Paper's installer.
case "$AFTER_STATE" in
  stopped) die "the panel did not act on the start request (state still $AFTER_STATE)" ;;
  crashed) printf '   \033[33mnote\033[0m the server started and exited: template %s needs its installer run first (panel-side)\n' "$TEMPLATE_ID" ;;
  *) ;;
esac

step "the power action is audited and attributed"
psql "$BILLING_DATABASE_URL" -qtA -F' | ' -c \
  "SELECT action, outcome, actor_id IS NOT NULL AS attributed
     FROM audit_events WHERE action LIKE 'service.power%' AND target_id='$SERVICE_ID'
     ORDER BY created_at DESC LIMIT 2"

step "suspend and resume travel to the panel"
# The staff suspend route refuses a service with no overdue invoice (dunning decides eligibility, which is
# correct). So this exercises the adapter's suspend/resume the way dunning does: by queueing the operation
# the worker claims. What is under test is the adapter's conversation with the panel, not the eligibility
# rule that is already covered by its own tests.
queue_op() {
  psql "$BILLING_DATABASE_URL" -qtA -c \
    "INSERT INTO service_operations (service_id, kind, operation_key, generation, status)
     VALUES ('$SERVICE_ID', '$1', 'e2e-$1-$(date +%s%N)', 1, 'queued')" >/dev/null
}
run_worker() {
  BILLING_PROVISIONER_EXTENSION="$EXTENSION" timeout 60 ./bin/billing worker >"$WORKER_LOG" 2>&1 || true
}

queue_op suspend
run_worker
SUSPENDED=$(pget "/api/servers/$SERVICE_ACTIVE" '.data.suspendedAt // empty')
if [ -n "$SUSPENDED" ]; then
  ok "the panel reports the server suspended ($SUSPENDED)"
else
  psql "$BILLING_DATABASE_URL" -qtA -F' | ' -c \
    "SELECT kind, status, coalesce(last_error,'') FROM service_operations
      WHERE service_id='$SERVICE_ID' AND kind='suspend'"
  die "the panel did not report the server suspended"
fi

queue_op resume
run_worker
CLEARED=$(pget "/api/servers/$SERVICE_ACTIVE" '.data.suspendedAt // "cleared"')
[ "$CLEARED" = "cleared" ] || die "the resume did not clear the suspension (suspendedAt=$CLEARED)"
ok "the resume cleared the suspension"

step "cleanup"
if [ "${KEEP:-0}" = "1" ]; then
  printf '   \033[33mkept\033[0m PANEL_SERVER_ID=%s (KEEP=1)\n' "$SERVICE_ACTIVE"
else
  # A live panel accumulates strays, so a run that is finished removes what it created. The billing
  # side is left intact: the point of the record is that this service did exist.
  # The server first: the panel refuses to delete an account that still owns one, so the order matters.
  curl -sS -m 30 -X DELETE -H "Authorization: Bearer $PANEL_KEY" "$PANEL/api/servers/$SERVICE_ACTIVE" \
    >/dev/null 2>&1 && ok "removed the panel server" || printf '   \033[33mnote\033[0m could not remove %s\n' "$SERVICE_ACTIVE"
  # The account this run created. The panel's delete is a POST to /delete, and it refuses (500) while the
  # account still owns a server — which is why it comes second.
  curl -sS -m 30 -X POST -H "Authorization: Bearer $PANEL_KEY" "$PANEL/api/admin/users/$PANEL_USER/delete" \
    >/dev/null 2>&1 && ok "removed the panel account" || printf '   \033[33mnote\033[0m could not remove account %s\n' "$PANEL_USER"
fi

echo
echo "SERVICE_ID=$SERVICE_ID"
echo "CUSTOMER_ID=$CUSTOMER_ID"
echo "CUSTOMER_EMAIL=$CUSTOMER_EMAIL"
echo "PANEL_USER_ID=$PANEL_USER"
echo "PANEL_SERVER_ID=$SERVICE_ACTIVE"
