# catalyst-billing

A self-hosted, API-first billing and service automation panel for hosting
providers. Go owns the backend, the business rules, the workers and the extension
host.

This repository currently implements the **vertical slice plus the reliability
layer** described in the product plan: exact money, an immutable invoice
document, an idempotent payment path, a renewal engine, provisioning with
fencing and reconciliation, dunning with auditable suspension decisions, and an
extension protocol. The customer and operator **web UI is not built yet**.

---

## Quick start

Prerequisites: Go 1.24+, `podman` + `podman-compose`, `psql`.

```bash
cp .env.example .env          # development credentials for a loopback cluster
make db-up                    # dedicated PostgreSQL 17 on 127.0.0.1:5442
make migrate                  # apply the embedded schema
make build                    # ./bin/billing

./bin/billing doctor          # connectivity, schema, role privileges, ledger, backlog
make test                     # the whole suite against a real database
```

`make db-up` starts a **dedicated** cluster in a named volume. It is separate from
any other Postgres on the machine on purpose: the tests create and truncate
databases freely.

### Run it

```bash
# terminal 1
set -a && . ./.env && set +a
BILLING_GATEWAY_PROVIDER=fakegateway \
BILLING_GATEWAY_ACCOUNT=acct_test \
BILLING_WEBHOOK_SECRET=whsec_dev \
./bin/billing serve

# terminal 2
BILLING_GATEWAY_EXTENSION=$PWD/bin/fakegateway \
BILLING_PROVISIONER_EXTENSION=$PWD/bin/fakeprov \
BILLING_GATEWAY_ACCOUNT=acct_test \
./bin/billing worker
```

```bash
curl -s localhost:8080/healthz          # liveness, no database access
curl -s localhost:8080/readyz           # database reachable and schema current

# Staff-only: the operator exception list.
curl -b cookies.txt localhost:8080/api/v1/ops/overview | jq

# Customer-facing: what do I owe, what is renewing, is my service ready.
curl -b cookies.txt localhost:8080/api/v1/portal/overview | jq
curl -b cookies.txt localhost:8080/api/v1/portal/invoices | jq
curl -b cookies.txt localhost:8080/api/v1/portal/services | jq
```

### Operator commands

```bash
# The first staff account. Without this there is no way to sign in.
# The password is read from the environment or stdin, never from argv: an argument
# is visible in `ps` and lands in shell history.
read -rsp 'password: ' BILLING_ADMIN_PASSWORD; echo
BILLING_ADMIN_PASSWORD="$BILLING_ADMIN_PASSWORD" \
  ./bin/billing admin create-user --email you@example.test --role administrator
unset BILLING_ADMIN_PASSWORD

./bin/billing admin create-customer --email a@example.test --name "A"
./bin/billing admin issue-invoice --customer <id> --amount 12.00 --tax-rate 20%
./bin/billing migrate --status
./bin/billing doctor --json
```

### Authentication

```bash
# Sign in; the session cookie is HttpOnly and the CSRF token is returned here.
curl -c cookies.txt -X POST localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.test","password":"a long passphrase"}'

curl -b cookies.txt localhost:8080/api/v1/auth/me
curl -b cookies.txt localhost:8080/api/v1/ops/overview
```

The metrics endpoint and the operator overview require staff authentication; the
webhook, liveness and readiness endpoints are public because a provider has no
cookie and an orchestrator has no session.

### Behind a proxy

`X-Forwarded-For` and `X-Forwarded-Host` are **ignored unless the immediate peer is a
configured trusted proxy**. This is a security boundary, not a convenience: believing
those headers unconditionally lets a client choose its own throttling bucket, lock out
another address, poison the audit log, and defeat the origin check.

```bash
BILLING_TRUSTED_PROXIES=10.0.0.0/8,192.168.1.5   # your proxy's networks
```

Empty (the default) is correct for a directly exposed panel.

### Development-only flags

`BILLING_INSECURE_COOKIES=1` disables the `Secure` cookie flag and logs a warning —
use it only on loopback. The session cookie is `HttpOnly` and `SameSite=Lax`
regardless; the CSRF cookie is readable by the client on purpose, because the
double-submit pattern needs it.

---

## What is implemented, and what it guarantees

| Area | Guarantee | Where |
|---|---|---|
| Money | Integer minor units + explicit currency; no binary floats anywhere; exact rational rates; largest-remainder allocation that sums exactly | `internal/money` |
| Billing calendar | Nominal anchors that never drift; month-end clamping; leap-day annual renewal; DST gap/ambiguity rules; persisted business timezone | `internal/calendar` |
| Invoices | Draft is editable; issued is immutable in the database; the issued document is a frozen JSONB snapshot | `internal/billing`, `0001_core.sql` |
| Subledger | Append-only; balanced per transaction at commit; customer-attributed; balances derived, never stored mutable | `internal/ledger` |
| Payments | Verify → persist → process → apply. Duplicate absorbed on event id *and* provider payment id. Settled amount verified against the recorded checkout | `internal/payments`, `internal/checkout` |
| Renewals | One invoice per subscription per period enforced by index; version-guarded schedule advance; catch-up backlog routed to review | `internal/renewal` |
| Provisioning | Persisted intent before the remote call; adapter idempotency keys; unknown outcome reconciled, not retried; generation fencing | `internal/provisioning` |
| Dunning | Persisted escalation state; every suspension decision recorded with reason and generation; manual holds outrank automation; resume after payment | `internal/dunning` |
| Auth | Argon2id passwords, hashed server-side sessions, per-session CSRF, login throttling, deny-by-default RBAC with billing/infrastructure separation | `internal/authn`, `internal/httpapi` |
| Portal | Customer-scoped reads where the scoping is in SQL, so identifier substitution returns "not found" for someone else's record and for a nonexistent one alike; cancellation at period end only | `internal/portal` |
| Retention | Bookkeeping tables are swept on a schedule from policies stored in the database; the sweeper refuses any table it does not own | `internal/retention` |
| Extensions | Versioned JSON-RPC over supervised stdio; bounded frames; deadlines; cancellation; crash isolation; clean environment | `internal/extproto` |
| Operations | One-request exception overview; independent checks that report themselves when broken | `internal/ops` |

### Design rules worth knowing before changing anything

1. **Money never leaves the money package.** `Amount` carries its currency; the
   zero value is invalid, not zero. Arithmetic between currencies fails rather
   than converts.
2. **Rounding is a parameter, not a default.** Every conversion from an exact
   rational to minor units names its mode, and the mode is recorded on the
   invoice snapshot so the total is reproducible.
3. **The database enforces what must never be false.** Journal balance, invoice
   immutability, one-invoice-per-period and provider-payment identity are
   constraints and triggers, not application checks.
4. **A remote call is never the last word.** The provider's own state is
   re-queried; an unverifiable outcome becomes `unknown` and waits for
   reconciliation.
5. **A stale worker cannot be destructive.** Destructive transitions compare the
   service generation under a row lock.
6. **Two credentials.** The owner role runs migrations; the runtime role cannot
   create, alter or drop. `billing doctor` fails if that is not true.

---

## Extensions

An extension is an executable that speaks newline-delimited JSON-RPC 2.0 on
stdin/stdout. The core is the authority: extensions return typed observations and
cannot mark invoices paid or receive database credentials.

Two conformance fixtures ship in this repository and are what the end-to-end
tests drive:

```bash
go build -o bin/fakegateway ./extensions/fakegateway
go build -o bin/fakeprov    ./extensions/fakeprov
```

Both accept a mode through the environment to reproduce failures that are
impossible to trigger on demand against a real provider — `hang`, `crash`,
`oversized`, `slow`, and for the provisioner `create_then_timeout` and
`duplicate`.

**Not a sandbox.** A separate process gives crash isolation, not confinement. A
same-user executable can read the host filesystem. Third-party extensions need a
restricted OS account or container with enforced egress; until that runner
exists, treat installed extensions as fully trusted operator code.

---

## Tests

The suite runs against real PostgreSQL. There are no in-memory substitutes,
because the constraints, triggers, locking and privilege model *are* the thing
under test.

```bash
make test          # go test ./... with BILLING_REQUIRE_DB=1
make test-race     # same, with the race detector
```

- Each **package** gets its own database (`billing_test_<pkg>`), created on demand.
  Within a package, tests are serialised with an advisory lock and start from
  truncated tables.
- The harness **fails** if a migration adds a table it has not classified, because
  an unclassified table is exactly how state leaks between tests.
- `internal/e2e` drives the whole slice: real compiled extensions over the real
  protocol, a real database, real webhooks.
- `make db-unlock` clears a lock leaked by a killed run; the harness reports the
  holder rather than terminating healthy backends heuristically.

Acceptance scenarios covered by tests, by plan number:

| # | Scenario | Test |
|---|---|---|
| 1 | Replayed webhook 100× → one allocation, one operation | `TestVerticalSliceReplayedWebhook` |
| 2 | Two distinct events, one payment, no double credit | `TestTwoEventsForOnePayment` |
| 3 | Payment succeeds remotely, crash before commit | `TestCrashBeforeCommitRecordsOnce` |
| 4 | Provisioner creates then times out | `TestProvisionerTimeoutDoesNotDuplicate` |
| 5 | Two workers, one renewal invoice | `TestConcurrentRenewalRuns`, `TestOneInvoicePerPeriodConcurrent` |
| 6 | Missed runs recover; cancelled subscriptions do not renew | `TestCatchUpAfterDowntime`, `TestCancelledSubscriptionDoesNotRenew` |
| 7 | Month-end, leap-year, DST, business timezone | `internal/calendar`, `TestMonthEndAnchorSurvivesRenewal`, `TestLeapDayAnnualRenewal`, `TestBusinessTimezoneRenewal` |
| 8 | Payment races suspension; stale workers cannot act | `TestStaleWorkerCannotSuspendPaidService`, `TestGenerationChangeInvalidatesRequest` |
| 11 | Extension crash/oversize cannot corrupt financial state | `internal/extproto`, `TestGatewayFailureKeepsEventPending` |
| 14 | Issued invoice totals stable after edits | `TestIssuedInvoiceIsImmutable`, `TestCatalogEditDoesNotRewriteHistory` |
| 15 | Property tests for rounding, allocation, journal balance | `TestAllocateProperties`, `TestPeriodTilingProperty`, `TestJournalMustBalance` |

---

## Not implemented

Listed so nobody assumes otherwise:

- **Web UI.** No rendered frontend, no design system, no components. The customer
  portal and the operator overview exist as JSON APIs; the browser UI does not.
- **Self-service registration, email verification, MFA, passkeys, API keys.**
  Password login, server-side sessions, CSRF and role-based authorization exist;
  the rest does not. Staff accounts are created by `billing admin create-user`.
- **Refunds, credit notes, disputes, partial-payment allocation across invoices.**
  The schema has the tables and the bounds are enforced; the flows are not built.
- **Off-session charging.** Requires recorded consent and a reusable method; the
  v1 protocol has no charge method, so dunning escalates instead of charging.
- **Paymenter import.** No importer, no dry run.
- **Stripe.** Only the fake gateway is wired. The `HMACSigner` implements a
  Stripe-shaped signature scheme, but no Stripe adapter has been exercised against
  the real API.
- **River queue.** Jobs are a ticker over idempotent batches, which is sufficient
  and safe; River would add prioritisation and per-job retry state.
- **A sandboxed extension runner.** Extensions are fully trusted operator code; see
  `docs/extensions.md` for exactly what the host enforces and what it does not.
- **Tax engine.** One explicitly configured rate per installation, inclusive or
  exclusive, recorded as evidence. No jurisdiction logic, no registration, no
  filing.
- **Multi-tenancy, resellers, FX, usage billing, a general ledger.**

---

## Layout

```
cmd/billing/            serve | worker | migrate | doctor | admin
internal/money/         exact amounts, rational rates, allocation, JSON
internal/calendar/      anchors, periods, DST, business timezone
internal/store/         pool, migrations (embedded), schema, test harness
internal/ledger/        balanced append-only subledger
internal/billing/       invoice draft/issue, idempotent payment application, tax
internal/checkout/      payment sessions and recorded expectations
internal/payments/      webhook inbox, verification, authoritative re-query
internal/fulfilment/    paid invoice → provisioning operation
internal/provisioning/  operation worker, fencing, reconciliation
internal/renewal/       due-work scanner, catch-up guard, cancellation
internal/dunning/       escalation, holds, suspension eligibility
internal/ops/           operator exception overview
internal/httpapi/       HTTP surface
internal/extproto/      extension protocol, client, supervisor
extensions/             conformance fixtures
```
