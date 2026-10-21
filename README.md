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

# Remove test fixtures. Six tables refuse deletion by trigger (the journal, the audit log,
# issued invoices and credit notes), so this is a reviewed command rather than a DELETE: it
# requires a pattern and --yes, refuses a database whose name is not test/dev, runs in one
# transaction with the triggers re-enabled before commit, and verifies afterwards that the
# ledger still balances. --dry-run runs the real deletes and rolls back.
./bin/billing admin purge-test-data --pattern 'smoke-%@example.test' --dry-run

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
| Audit | Append-only by trigger; written by the domain service inside the transaction that made the change; credentials redacted by field name before the insert | `internal/audit`, `0010_audit.sql` |
| Refunds | Refundable computed under a row lock; intent committed before the provider call; a timeout is `unknown` and posts nothing; the reconciler asks the provider again with the same key instead of retrying | `internal/refunds` |
| Credit notes | Immutable counter-documents in their own gap-free series; a wrong invoice is corrected, never edited; credit is a subledger account spent by an explicit action | `internal/creditnote` |
| Approvals | A second, distinct staff actor for high-value actions, enforced by a `CHECK` so a bug cannot self-approve | `internal/approvals`, `0011_contracts.sql` |
| Account lifecycle | Hashed single-use expiring tokens; non-enumerating responses that match in body and in cost; a reset revokes every session; TOTP secrets sealed with AES-256-GCM | `internal/authn`, `internal/httpapi` |
| Notifications | A durable outbox written in the same transaction as the change, drained with bounded retries; "we did not send it" and "we failed to send it" are different recorded states | `internal/notify` |
| API surface | Every route is a table entry, so permission coverage, the error envelope and OpenAPI parity are checked rather than assumed | `internal/httpapi`, `internal/openapi` |

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
7. **A route is a table entry, not a `mux.HandleFunc`.** Registration applies the
   wrappers the declaration implies, which is what makes "no route ships without a
   permission check" a test rather than a review habit.
8. **An audit row commits with the change it describes.** Writing it afterwards is
   the fallback, not the design: a row for a rolled-back change is a lie, and a
   change with no row is a gap.

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

## The web UI gate

The web UI is the last thing to build, not the first. Before this work the server
exposed **14 routes**: 5 customer reads, 1 customer mutation, 3 auth, 2 ops, 1
webhook and 2 health checks. There was no staff read surface, no staff write
surface, no refund, no credit note and no password reset, so most of the operator
screens would have had nothing to call.

That gate is now **closed**. Every item that was listed as missing is implemented,
tested against a real database, and described in
[`docs/ui-gate.md`](docs/ui-gate.md). What follows is what exists, in the order the
work was done, with the property each piece is held to.

### What was built

**Contracts first (Gate E), because they are cheap at 14 routes and expensive at 40.**

| Item | What exists |
|---|---|
| Route table | Every route is one entry in `internal/httpapi/routes.go`, registered by a feature's own `init`. The cross-cutting wrappers (authentication, permission, staff guard, CSRF, idempotency, rate class) are applied by the registrar, so a new route cannot ship without them. |
| OpenAPI | Generated from that same table (`make openapi`). The parity test compares in both directions: served-but-undocumented fails, and documented-but-not-served fails. |
| Typed client | Generated from the document (`make client`) and compiled in CI, with tests that drive it against a real server. |
| Pagination | Keyset cursors on every list (`internal/paginate`), never OFFSET. A malformed cursor is `400 invalid_cursor`, never a silent reset to page one. |
| Idempotency | Scoped to (principal, key), bound to the route and a digest of the body, claim-then-store. A replay with the same body returns the stored response; a different body is `422`; a 5xx is not stored, so a retry can retry. |
| Error contract | `application/problem+json` with a stable `code`, proven route by route by `TestEveryErrorIsProblemJSON`. |
| Operation resource | `GET /api/v1/staff/operations/{id}` — the status URL a `202` points at, with `lease_expires_at` derived from an exported lease constant. |
| SSE | `GET /api/v1/events`, filtered per caller (a customer sees only their own events, staff only events whose permission they hold), `Last-Event-ID` resume, 20-second heartbeat. |

**Trust surface (Gate F).**

| Item | What exists |
|---|---|
| Audit log | `audit_events`, append-only by trigger. Written by the domain services inside the transaction that made the change, with credentials redacted by field name before the insert. Readable at `GET /api/v1/staff/audit` with actor, action, target and date filters. |
| Rate limiting | Per principal, per route class (read / mutation / export). A `429` carries `Retry-After`. Credential endpoints have their own tighter budget keyed by address. |
| Permission coverage | `TestPermissionCoverage` enumerates the route table: every staff and portal route declares a permission or a dynamic permission, every staff route is staff-only, and every session mutation requires CSRF. |

**Staff read surface (Gate A).**

Customers (list with search, filters, keyset pagination, and a detail view joining
accounts, subscriptions, services, the last invoices, per-currency balance, holds and
recent auth events), invoices (list with status/customer/currency/date/overdue filters,
detail with the snapshot verbatim plus lines, payments, allocations, refunds, credit
notes and journal postings), services (list and detail with the open-operation count
and the newest failure), a separate suspension-eligibility endpoint that records its
decision, orders, payments, the operation queue, the webhook inbox, the catalog, and
`POST /api/v1/portal/invoices/{id}/checkout` so the customer "Pay" button finally has
somewhere to go.

Two rules are structural rather than incidental: balances are per currency and never
summed across them, and `overdue` is derived (`status = 'open' AND due_at < now()`),
never stored.

**Staff mutations (Gate B).**

Suspend and resume go through the dunning engine, never by inserting an operation
directly, so the eligibility decision and the generation are recorded. A manual hold
refuses suspension; overriding it needs a second staff actor, recorded in
`suspension_decisions` with both ids and enforced by a `CHECK`. Termination requires the
operator to echo the service's `external_id` or id. Holds take their `created_by` from
the session. Manual invoices are priced by the server, `issue_now` drafts and issues in
one transaction, a paid invoice cannot be voided, and billing profiles are one
implementation behind two doors (customer and staff).

**Money actions (Gate D).**

Refunds compute the refundable balance under a row lock, write the intent row before the
provider call, and distinguish three outcomes: confirmed (post), rejected (record, post
nothing), and unknown (record, post nothing, reconcile by asking the provider again with
the same operation key). Credit notes are immutable counter-documents in their own
gap-free `CN-` series. Applying credit is its own event, keyed by its own id, so a replay
is a no-op. Dual approval gates amounts above a configurable threshold, and the database
refuses a self-approval even if the service is bypassed.

**Account lifecycle (Gate C).**

Password reset and email verification with hashed, single-use, expiring tokens and a
non-enumerating response that is identical in body *and* cost for a known and an unknown
address. A completed reset revokes every session. A durable notification outbox written in
the same transaction as the change, drained by a worker with bounded retries. TOTP MFA
(RFC 6238, secrets sealed with AES-256-GCM under a key from the environment), recovery
codes, and a login second step that issues a session only after the code. API keys stored
as a digest plus an eight-character prefix, shown once, scoped so they can only narrow
their owner's permissions.

**Serving the UI (Gate G).**

The frontend is embedded. `scripts/browser-smoke.sh` drives a headless Chromium against a
real server and asserts the things only a browser can prove: the shell renders, the module
executes under the CSP, a client-side route survives a reload, a hashed asset is
immutable, an unknown API path stays `problem+json`, and an injected inline script is
refused. `make verify` runs it.

### Definition of ready for the web UI — status

1. Every screen in the plan's §9 navigation has a documented endpoint, and that endpoint
   exists. **Met**: the route table and `api/openapi.yaml` are the same set, checked by a
   test.
2. `GET /api/v1/auth/me` returns the permissions the UI uses to hide actions, and a hidden
   action is also refused by the server. **Met**: permissions are enforced by the route
   table, not by the client.
3. Every list is paginated; every mutation takes an idempotency key; every error has a
   stable code. **Met**, and each is a test.
4. OpenAPI is checked in and a typed client is generated from it in CI. **Met**:
   `make openapi-check client-check`.
5. A refund, a credit note and a termination all have a tested path that ends in a balanced
   ledger. **Met**: `internal/refunds`, `internal/creditnote` and `internal/httpapi` each
   assert the ledger balances per currency.
6. Password reset and email verification work end to end, so the login screen is not a dead
   end. **Met**, including the queued message.

### Known limits of what was built

Stated here rather than discovered later:

- **The event broker is process-local.** With more than one instance, a client connected to
  instance A does not see an event published on instance B. Correcting that needs a durable
  log and a per-client cursor, which is a different design; the SSE contract is documented
  as such rather than implying more.
- **The rate limiter is in-process and fixed-window.** The effective limit is per instance
  and can be exceeded by one window at a boundary. It bounds load; it is not a quota.
- **Refund reconciliation is a poll**, every two minutes, over unresolved refunds. It asks
  the provider rather than retrying, so it is safe, but it is not push.
- **The frontend is partly built.** The foundation is complete and used: a hash-based
  router, layout, table and form kit, typed API client, formatting and icons (~2,900 lines
  of TypeScript, typechecked and bundled with esbuild, embedded in the binary). Real
  screens exist for sign-in, password reset, email verification, the dashboard, refunds
  and billing details.

  **26 of 34 views are stubs** — a heading and nothing else. `scripts/browser-e2e.sh`
  prints the count on every run so a green build is not mistaken for a finished product:

  ```
  note: 26 of 34 views are still stubs (heading only, no data)
  ```

  Every endpoint those screens need is implemented and documented; the remaining work is
  view code, not server work.
- **`pg_trgm` is optional.** The customer search index tries it and falls back to a
  `lower(name)` prefix index, so a deployment that cannot install the extension still
  migrates. Which one is in force is visible in `\d customers`.

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
- [`docs/ui-gate.md`](docs/ui-gate.md) — the implementation specification for the web UI
  gate: DDL, endpoint contracts, rules, errors and the tests that prove each one. Every
  task in it is now implemented; the document is kept as the record of *why* each rule
  exists.
- [`api/openapi.yaml`](api/openapi.yaml) — the API document. Generated from the route
  table by `make openapi`; `make openapi-check` fails when it drifts.
- [`api/client`](api/client) — the typed client generated from that document.

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
internal/audit/         append-only application audit log, with redaction
internal/approvals/     dual approval for high-value actions
internal/refunds/       refund intent, provider call, reconciliation, posting
internal/creditnote/    credit notes and customer credit application
internal/notify/        durable notification outbox and the SMTP sender
internal/events/        in-process event broker behind SSE
internal/paginate/      keyset cursors
internal/staff/         operator read surface
internal/openapi/       the API document, generated from the route table
internal/httpapi/       HTTP surface: route table, middleware, handlers
internal/extproto/      extension protocol, client, supervisor
api/openapi.yaml        the generated API document
api/client/             the generated typed client
web/                    the embedded frontend shell
extensions/             conformance fixtures
scripts/                browser smoke test
```
