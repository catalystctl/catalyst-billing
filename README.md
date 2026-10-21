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

### The web UI gate

The web UI is the last thing to build, not the first. The current server exposes
**14 routes**: 5 customer reads, 1 customer mutation, 3 auth, 2 ops, 1 webhook and
2 health checks. There is no staff read surface and no staff write surface at all,
so most of the operator screens would have nothing to call. Building the frontend
first means inventing the API in the frontend, which is the expensive way to
discover it was wrong.

Two kinds of gate are listed below:

- **Hard** — the screen cannot be written at all without it. A list screen with no
  list endpoint has nothing to render.
- **Soft** — the screen can be written, but it cannot ship. Shipping it would
  either be unsafe or would have to be rewritten.

**Each task is fully specified in [`docs/ui-gate.md`](docs/ui-gate.md)**: the schema
it needs (as DDL), the endpoint contract with status codes and error codes, the rules
the implementation must enforce, and the tests that prove it. The tables here are the
index; that document is the work order. Where a decision is genuinely open it is
called out inline as a recommendation with the reason, rather than left for the
implementer to guess.

#### Suggested order

| Step | Do | Why this position |
|---|---|---|
| 1 | **Gate E contracts** (OpenAPI, pagination, idempotency keys, error envelope, operation resource) | Cheapest while there are 14 routes; at 40 it means editing all 40. Pagination is already missing on a live endpoint, so it is also a fix, not only a convention. |
| 2 | **Audit log from Gate F** | Every mutation added later must write to it. Retrofitting it after the money actions exist means auditing the actions twice. |
| 3 | **Gate A staff reads** | The operator workspace is the largest screen set, and every other screen links from it. |
| 4 | **Gate B staff writes** | Actions need the read surface to hang off, and the audit log to record them. |
| 5 | **Gate D money actions** | Refund and credit note need the write surface, the audit log, and dual approval — all three exist by now. |
| 6 | **Gate C account lifecycle** (parallel track) | Independent of the staff surface. Start it alongside step 3 if a second person is free, because password reset is the first thing a real user needs. |
| 7 | **Gate G serving** | Trivial, and nothing can be demoed in a browser before it. |

#### Gate A — API surface the screens call (hard)

Spec: [`docs/ui-gate.md#gate-a--staff-read-surface`](docs/ui-gate.md#gate-a--staff-read-surface)

| Missing | Why the UI cannot start | Done when |
|---|---|---|
| Staff customer list, detail and search | The customer workspace joins billing, services, support and audit; none of it is reachable | `GET /api/v1/staff/customers` with filter, sort and cursor; detail returns the joined view |
| Staff invoice list and detail, all customers | Operator billing screen has no source | Same shape as the portal endpoints, scoped by permission rather than by customer |
| Staff service list and detail | The services screen and every action button need it | Includes current operation state and the last failure reason |
| Staff order and checkout list | Orders exist in the schema and are invisible | Order detail shows the price snapshot and the resulting invoice |
| Staff payment attempt and payment history | Support cannot answer "did they pay?" without it | Attempts with provider refs, status, and the allocation each payment produced |
| Operation and job queue view | The plan's "unknown remote operation" exception links nowhere | `GET /api/v1/staff/operations` filtered by status, with the last error |
| Catalog read (products, price versions) | Checkout and the price editor need it | Versioned list; edits create a new version, never mutate |
| Checkout session creation over HTTP | `internal/checkout` is implemented and unreachable — the customer "Pay" button has no endpoint | `POST /api/v1/portal/invoices/{id}/checkout` returns the redirect URL and the exact amount |

#### Gate B — mutations the screens offer (hard)

Spec: [`docs/ui-gate.md#gate-b--staff-mutations`](docs/ui-gate.md#gate-b--staff-mutations)

| Missing | Why | Done when |
|---|---|---|
| Staff service actions (suspend, resume, terminate) | Dunning can enqueue them; a human cannot | Enqueues an operation with the current generation, and the response states the consequence |
| Dunning hold create and clear | The recovery procedure in `docs/operations.md` requires a UI or a CLI | Both, and the hold is visible on the customer |
| Manual invoice issue and void | Operator escape hatch; `billing admin issue-invoice` exists, so the API is a thin wrapper | Same domain code as the CLI, no shortcut |
| Billing profile write | A customer cannot correct their own address or tax id | Validated, snapshotted on the next invoice |

#### Gate C — account lifecycle the login screen assumes (soft)

Spec: [`docs/ui-gate.md#gate-c--account-lifecycle`](docs/ui-gate.md#gate-c--account-lifecycle)

| Missing | Why it blocks shipping | Done when |
|---|---|---|
| Password reset | A login page without it generates a support ticket per forgotten password | One-use, expiring, hashed token; non-enumerating response; throttled |
| Email verification flow | The `email_verified_at` column exists and nothing sets it | Token plus a verification screen |
| **Notification delivery (SMTP)** | Reset, verification and dunning reminders all need it; no notifications module exists | A delivery interface with a recorded attempt per message, so "we never sent it" is answerable |
| MFA enrolment (TOTP) | The plan requires mandatory staff MFA before production | Enrolment, recovery codes, and a second step in login |
| API key create and revoke | Required before automation exists; also the first screen that shows a secret exactly once | Hashed, scoped, expiring, shown once |

MFA and registration can be sequenced **after** the first operator screens if the
panel stays on loopback. They are gates before it is reachable from the internet,
not before the first screen is written.

#### Gate D — money actions the UI must not offer without (hard)

Spec: [`docs/ui-gate.md#gate-d--money-actions`](docs/ui-gate.md#gate-d--money-actions)

| Missing | Why it blocks the UI | Done when |
|---|---|---|
| Refund flow | The table exists and nothing can create a refund. A UI with a disabled refund button is worse than no button | Provider call, `unknown` outcome handled like provisioning, allocation and posting in one transaction |
| Credit note | The plan's correction path for an issued invoice; without it a wrong invoice can only be voided | Immutable document, linked to the invoice it corrects |
| Customer credit application | Refunds and overpayments have nowhere to land | Credit is a subledger account, applied by an explicit action, never silently |
| Dual approval for high-value refunds and termination batches | The plan requires it, and it is a UI flow as much as a policy | An approval record that names both actors |

#### Gate E — contracts that are cheap now and expensive later (hard)

Spec: [`docs/ui-gate.md#gate-e--contracts`](docs/ui-gate.md#gate-e--contracts)

These are not screens. They are the things that, if retrofitted after 30 endpoints
exist, mean touching all 30.

| Missing | Why now | Done when |
|---|---|---|
| **OpenAPI document** | The plan requires it; without it the frontend hand-writes types and they drift silently | Generated from the handlers, checked in CI, and a typed client built from it |
| **Pagination on every list** | `internal/portal` currently returns every invoice a customer has. A UI built against that breaks at scale, not in review | Cursor-based, with a documented default and maximum page size |
| **Scoped idempotency keys on mutations** | Plan §10 requires them. A UI retries on a flaky network; without this a retry refunds twice | Key bound to principal, route and request digest; reuse with different input fails |
| **Uniform error contract** | `problem+json` exists on some paths; a UI needs one shape to render | Every error path returns the same envelope with a stable `code` |
| **Operation resource for long actions** | Plan §10: a long action returns an operation, not a held connection | The action returns `202` plus an operation URL |
| **SSE for job and service updates** | Plan §4. Otherwise the provisioning screen is a poll loop and the operator sees stale state | One endpoint streaming committed state changes |

#### Gate F — the trust surface the operator workspace renders (soft)

Spec: [`docs/ui-gate.md#gate-f--trust-surface`](docs/ui-gate.md#gate-f--trust-surface)

| Missing | Why | Done when |
|---|---|---|
| **Application audit log** | Only `auth_events` exists. Plan §8 requires actor, action, target, correlation and redacted changes for *every* staff action | Append-only table written by the domain services, not by handlers |
| Request rate limiting per principal | Throttling covers login only; a UI invites bulk actions | Bounded requests per principal per window, with a documented limit |
| Staff permission enforcement on every new route | Permissions exist and are enforced on 7 routes, but all of them are customer-facing or reporting — there are no staff routes yet to protect | Every staff route carries `requirePermission`, and a test enumerates the routes to prove none is missing one |

#### Gate G — serving the UI (soft, last)

Spec: [`docs/ui-gate.md#gate-g--serving-the-ui`](docs/ui-gate.md#gate-g--serving-the-ui)

| Missing | Why | Done when |
|---|---|---|
| Static asset serving and embed | Plan §4: embed the built frontend in the binary | `billing serve` serves the SPA with a strict CSP, hashed asset names, and no directory listing |
| CSP and security headers | User and extension content is rendered in an authenticated origin | Documented policy, applied by the static handler, verified in a browser test |

### Definition of ready for the web UI

Start the frontend when all of these are true:

1. Every screen in the plan's §9 navigation has a documented endpoint, and that
   endpoint exists.
2. `GET /api/v1/auth/me` returns the permissions the UI uses to hide actions, and a
   hidden action is also refused by the server.
3. Every list is paginated; every mutation takes an idempotency key; every error
   has a stable code.
4. OpenAPI is checked in and a typed client is generated from it in CI.
5. A refund, a credit note and a termination all have a tested path that ends in a
   balanced ledger.
6. Password reset and email verification work end to end, so the login screen is
   not a dead end.

### Deferred beyond v1

Safe to build the UI without, and listed so nobody assumes otherwise:

- **River queue.** Jobs are a ticker over idempotent batches, which is sufficient
  and safe; River would add prioritisation and per-job retry state.
- **A sandboxed extension runner.** Extensions are fully trusted operator code; see
  `docs/extensions.md` for exactly what the host enforces and what it does not.
  Extension *installation* UI is deferred with it.
- **Off-session charging.** Requires recorded consent and a reusable method; the v1
  protocol has no charge method, so dunning escalates instead of charging. The UI
  offers "pay now" instead.
- **Paymenter import.** No importer, no dry run. Import has no UI in v1.
- **Stripe.** Only the fake gateway is wired. The `HMACSigner` implements a
  Stripe-shaped signature scheme, but no Stripe adapter has been exercised against
  the real API. The UI is gateway-agnostic, so this does not block it.
- **Tax engine.** One explicitly configured rate per installation, inclusive or
  exclusive, recorded as evidence. No jurisdiction logic, no registration, no
  filing. Settings shows the configured rate as read-only evidence.
- **Support tickets and attachments.** Plan v1 includes lightweight tickets; they
  are a real screen set and are the one plan feature that needs its own design
  before the API.
- **Multi-tenancy, resellers, FX, usage billing, a general ledger.**
- **A design system.** Not a backend gate: the frontend work begins with it.

---

## Documentation

- [`docs/extensions.md`](docs/extensions.md) — the extension host: what is enforced,
  with the test that proves each property, and what is explicitly not.
- [`docs/operations.md`](docs/operations.md) — deploying, the two database roles, the
  job table with kill switches, what the operator overview reports, recovery
  procedures, backup and restore.
- [`docs/ui-gate.md`](docs/ui-gate.md) — the implementation specification for every
  task standing between here and the web UI: DDL, endpoint contracts, rules, errors and
  required tests.

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
