#!/usr/bin/env bash
# Seeds the support/account fixture: a staff account, a customer with a login, a ticket with two
# messages and one attachment. Prints the ticket id on the last line.
set -euo pipefail
cd "$(dirname "$0")/.."
set -a; . ./.env; set +a

BASE="$1"
STAMP="$(date +%s)"
STAFF_EMAIL="p11-staff-${STAMP}@example.test"
STAFF_PASSWORD="p11-staff-password-1"
CUST_EMAIL="p11-cust-${STAMP}@example.test"
CUST_PASSWORD="p11-cust-password-1"

BILLING_ADMIN_PASSWORD="$STAFF_PASSWORD" ./bin/billing admin create-user \
  --email "$STAFF_EMAIL" --role administrator --name "P11 Staff" >/dev/null

CUSTOMER_ID="$(./bin/billing admin create-customer --email "$CUST_EMAIL" --name "P11 Customer" | awk '/created customer/{print $3}')"

BILLING_ADMIN_PASSWORD="$CUST_PASSWORD" ./bin/billing admin create-user \
  --email "$CUST_EMAIL" --name "P11 Customer" >/dev/null

psql "$BILLING_DATABASE_URL" -q -v ON_ERROR_STOP=1 <<SQL >/dev/null
INSERT INTO customer_accounts (user_id, customer_id, role)
SELECT id, '${CUSTOMER_ID}', 'owner' FROM users WHERE email = '${CUST_EMAIL}'
ON CONFLICT DO NOTHING;
SQL

# Sign in as the customer through the real form endpoint and keep the cookies.
COOKIE_JAR="$(mktemp)"
LOGIN="$(curl -fsS -c "$COOKIE_JAR" -H 'Content-Type: application/json' \
  -H "Origin: $BASE" -H "Referer: $BASE/login" \
  -d "{\"email\":\"$CUST_EMAIL\",\"password\":\"$CUST_PASSWORD\"}" \
  "$BASE/api/v1/auth/login")"
CSRF="$(printf '%s' "$LOGIN" | python3 -c 'import json,sys; print(json.load(sys.stdin)["csrf_token"])')"

TICKET="$(curl -fsS -b "$COOKIE_JAR" -H 'Content-Type: application/json' \
  -H "Origin: $BASE" -H "X-CSRF-Token: $CSRF" -H "Idempotency-Key: $(uuidgen)" \
  -d '{"subject":"Server unreachable since 09:10","department":"technical","priority":"high","message":{"body":"The host stopped answering ping at 09:10 UTC. I restarted nothing.\n\nHere is the trace:\n  ping: sendto: No route to host"}}' \
  "$BASE/api/v1/portal/tickets")"
TICKET_ID="$(printf '%s' "$TICKET" | python3 -c 'import json,sys; print(json.load(sys.stdin)["ticket"]["id"])')"
MESSAGE_ID="$(printf '%s' "$TICKET" | python3 -c 'import json,sys; print(json.load(sys.stdin)["messages"][0]["id"])')"

# A staff reply, so the thread has both sides and the status moves to replied.
STAFF_JAR="$(mktemp)"
SLOGIN="$(curl -fsS -c "$STAFF_JAR" -H 'Content-Type: application/json' \
  -H "Origin: $BASE" -H "Referer: $BASE/login" \
  -d "{\"email\":\"$STAFF_EMAIL\",\"password\":\"$STAFF_PASSWORD\"}" \
  "$BASE/api/v1/auth/login")"
SCSRF="$(printf '%s' "$SLOGIN" | python3 -c 'import json,sys; print(json.load(sys.stdin)["csrf_token"])')"

curl -fsS -b "$STAFF_JAR" -H 'Content-Type: application/json' \
  -H "Origin: $BASE" -H "X-CSRF-Token: $SCSRF" -H "Idempotency-Key: $(uuidgen)" \
  -d '{"body":"Thanks for the trace. The hypervisor dropped the vNIC; I have reattached it. Please confirm reachability."}' \
  "$BASE/api/v1/staff/tickets/$TICKET_ID/replies" >/dev/null

# An attachment on the first message, through the raw-body upload the screen uses.
printf 'route to host unreachable\n09:10:12 UTC\n' > /tmp/p11-attach.txt
curl -fsS -b "$COOKIE_JAR" -H 'Content-Type: text/plain' \
  -H "Origin: $BASE" -H "X-CSRF-Token: $CSRF" \
  --data-binary @/tmp/p11-attach.txt \
  "$BASE/api/v1/portal/tickets/$TICKET_ID/attachments?filename=ping-trace.txt&message_id=$MESSAGE_ID" >/dev/null

# An in-app notification, so the inbox has a row to render.
psql "$BILLING_DATABASE_URL" -q -v ON_ERROR_STOP=1 <<SQL >/dev/null
INSERT INTO notifications (user_id, event_key, title, body, url)
SELECT id, 'invoice_paid', 'Invoice INV-000001 paid', 'We received your payment. Nothing is owed on it.', '/portal/invoices'
FROM users WHERE email = '${CUST_EMAIL}';
SQL

rm -f "$COOKIE_JAR" "$STAFF_JAR"
echo "$STAFF_EMAIL $STAFF_PASSWORD $CUST_EMAIL $CUST_PASSWORD $TICKET_ID $CUSTOMER_ID"
