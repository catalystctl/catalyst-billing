# Operations

Everything an operator needs to run this, and the things that will bite if ignored.

## Deploying

One binary, three roles. A small installation runs `serve` and `worker` in one
process (`--embedded-worker`); a larger one runs the same binary in separate units.

```bash
billing migrate          # needs the owner role; the runtime role cannot DDL
billing doctor           # fails non-zero on any problem; run it in your deploy script
billing serve            # HTTP
billing worker           # background jobs
```

`billing doctor` checks connectivity, schema currency, that the runtime role cannot
create schema objects, that the ledger balances, the webhook backlog, and that each
configured extension starts and completes its handshake. A deploy that skips it will
find the same problems from a support ticket instead.

## The two roles

| Role | May | Used by |
|---|---|---|
| owner (e.g. `billing`) | DDL, migrations | `billing migrate`, `billing admin` |
| runtime (`billing_app`) | SELECT/INSERT/UPDATE/DELETE, sequences | `billing serve`, `billing worker` |

`billing doctor` fails if the runtime URL can create schema objects. That is the
check that turns "a bug could drop a table" into "a bug gets a permission error".

## Background jobs

Each job is a bounded batch that derives its work from persisted state, so running
it twice or running two workers is safe. Intervals are deliberately short: the
work is cheap when there is nothing to do.

| Job | Interval | What it does | Kill switch |
|---|---|---|---|
| payments | 5s | inbox → authoritative re-query → applied payment | `BILLING_DISABLE_PAYMENTS` |
| provisioning | 5s | reaps expired leases, executes operations, reconciles unknowns | `BILLING_DISABLE_PROVISIONING` |
| notifications | 15s | drains the durable message outbox | `BILLING_DISABLE_NOTIFICATIONS` |
| dunning | 60s | escalates overdue invoices, resumes paid ones, decides suspensions | `BILLING_DISABLE_SUSPENSION` |
| renewals | 60s | invoices due subscriptions | — |
| ticket-piping | 60s | polls the support mailbox and turns new mail into ticket replies | — |
| refund-reconcile | 2m | resolves refunds whose provider call timed out | `BILLING_DISABLE_REFUND_RECONCILE` |
| service-upgrades | 2m | applies paid upgrades, credits and applies downgrades, voids upgrade invoices nobody paid | `BILLING_DISABLE_SERVICE_UPGRADES` |
| charge-agreements | 15m | charges the saved default card of an invoice that is due, after `cronjob.charge_retry_hours` | `BILLING_DISABLE_CHARGE_DUE` |
| abandoned_carts | 30m | releases the stock held by carts nobody checked out | `BILLING_DISABLE_CART_ABANDON` |
| order-cancel | 30m | cancels unpaid orders older than `cronjob.order_cancel_days`, voiding their invoices and restoring stock | `BILLING_DISABLE_ORDER_CANCEL` |
| terminate-suspended | 30m | terminates services suspended longer than `cronjob.terminate_days`, through the operation queue | `BILLING_DISABLE_TERMINATION` |
| retention | 6h | deletes bookkeeping rows past their policy | — |
| ticket-autoclose | 6h | closes tickets nobody replied to for `tickets.autoclose_days` | — |
| tax-rate-refresh | 60m | fetches the configured tax-rate source; the source's own refresh interval decides whether a tick fetches | — |
| telemetry-send | 24h | sends the opt-in anonymous usage report when `updates.telemetry` is on | `BILLING_DISABLE_TELEMETRY` |

The kill switches are the emergency brake: a misbehaving gateway or provisioner can
be stopped without a deploy. Stopping provisioning does not lose work — the
operations stay queued and are picked up when the switch is cleared.

A switched-off job is absent from `cron_runs` entirely rather than recorded as
skipped, so "the job did not run" and "the job ran and did nothing" are different
rows. The automation screen shows every job with its switch state, which is the only
place a switched-off job is visible at all.

`charge-agreements` is the one job that moves money with nobody in the room, so it is
worth knowing exactly what its switch does. Stopping it stops unattended charging and
nothing else: renewals still issue invoices, dunning still reminds and still decides
suspensions on its own schedule, and a single invoice can still be charged by hand
from the invoice screen. Nothing is queued either — a tick that does not run is a tick
that did not happen, and the invoice stays due — so clearing the switch does not
produce a burst of retroactive charges. Before turning it back on, read
`GET /api/v1/staff/payments/charge-due`: it lists what the next tick would take, the
amount per currency, and the invoices it would skip and why. It resolves no gateway,
so asking it cannot charge anything.

That job's safety does not depend on there being one worker. The attempt row is
inserted before the provider is called and `payment_attempts.operation_key` is
UNIQUE, so two workers scanning the same invoice produce one provider round-trip and
one attempt row, and the loser is refused by the database rather than by an in-memory
check. There is deliberately no cross-process lock: the loser's refusal is the only
signal the panel has that two workers are running, and silencing it to tidy the run
history would remove the one symptom. A decline goes to dunning and never straight to
suspension.

`telemetry-send` is off by default and doubly switched: `updates.telemetry` is the
operator's consent on the settings screen, and `BILLING_DISABLE_TELEMETRY` is the
process-level brake for a deployment that would rather hold it in the environment.
Stopping it stops the daily report and nothing else — the update check is a separate
action, and "send now" still works, because an operator who wants to send one report
should not have to re-enable a schedule to do it. A delivery recorded inside twenty
hours is not repeated, so restarting the worker cannot turn one day's report into
three.

`terminate-suspended` and `order-cancel` have their own switches because they are the
two rules that destroy something a customer paid for, and `service-upgrades` has its
own because it is the job that *voids documents*: an operator whose catalogue produced
a burst of wrong upgrade prices needs to stop that without also stopping terminations
or order cancellation.

## What to watch

The operator overview (`GET /api/v1/ops/overview`) is the first page to open, and its
exception list is ordered by what it costs to ignore:

| Code | Severity | Meaning | What to do |
|---|---|---|---|
| `paid_not_provisioned` | critical | the customer paid and has nothing | inspect the operation; requeue or provision manually and record the external id |
| `unknown_operations` | critical | a remote call's outcome is not known | reconcile against the provider, then mark it; never retry blindly |
| `ledger_imbalance` | critical | the subledger does not sum to zero | stop and investigate; this is a defect, not a queue |
| `overdue_invoices` | warning | open past the due date | let dunning run or chase |
| `stalled_webhooks` | warning | events unprocessed for over ten minutes | check the worker and the gateway extension |
| `failed_operations` | warning | provisioning exhausted its retries | read the last error, fix the cause, requeue with the same key |
| `renewals_needing_review` | info | catch-up billing was withheld | review before billing |
| `suspensions_withheld` | info | automation declined to suspend | check the reason: a hold, a fresh payment, a policy limit |

`counts.available: false` means a count query failed. It is deliberately not reported
as zero: a broken database that looks like an empty one is the most misleading
dashboard state there is.

`GET /metrics` exposes webhook counters and uptime in Prometheus format. Both
endpoints require a staff session.

## Recovery procedures

**A worker died mid-call.** The operation stays `running` with an expired lease. The
claim query treats an expired lease as claimable, so the next tick recovers it; the
`provisioning` job also calls `Reap`, which records why it acted. The retry reuses
the same operation key, and reconciliation uses `provisioner.lookup` rather than
replaying create, so the customer does not get a second server.

**A webhook arrived but was never processed.** It is still in `gateway_events` with
`processed_at` null and `last_error` set. Nothing was lost. Fix the cause and the
next tick processes it; the payment is idempotent on the provider payment id, so
reprocessing cannot double-credit.

Read `last_error` before doing anything else, because the three shapes it takes need
different answers:

- *"carries no payment id"* — the row was ingested before the normalised payment id
  was stored (`gateway_events.payment_id` is NULL, migration 0058). The core cannot
  re-derive it: it does not parse provider payloads, and the signature headers are
  gone. Resend the event from the provider's dashboard. The redelivery is reported as
  a duplicate and nothing is re-processed, but it fills in `payment_id` and
  `event_type` on the unprocessed row, and the next tick applies the payment. A resend
  cannot repair a row that is already processed; those need
  `UPDATE gateway_events SET processed_at = NULL WHERE event_id = '…'` first, which is
  only correct when no payment was applied for it (check `payments`).
- *"names payment … but type … is not a settlement kind"* — the extension reported a
  settled payment under an event kind the core does not know. This is an extension
  bug, not an operator problem: a gateway must report `payment.succeeded` for a
  settlement (see `VerifyWebhookResult.EventType`). The event stays pending so it is
  visible rather than silently skipped; fixing the adapter and re-queueing the row
  settles it.
- anything else — the provider call failed (`query payment …`), the amount did not
  match the recorded checkout, or the payment has no invoice reference. Each is
  recorded verbatim; the invoice is deliberately left alone until the cause is known.

**The ledger does not balance.** This is always a defect. Every posting is
append-only, so the cause is findable: compare `journal_transactions` against the
documents they reference. Do not "fix" it with an adjustment until you know what
happened.

**A customer was suspended wrongly.** Check `suspension_decisions` for the reason and
the generation at decision time. Place a hold (`service_holds`) so automation does not
act again, correct the cause, then resume. A hold outranks every automated rule.

**An extension is crash-looping.** It is disabled after repeated failures, and
`billing doctor` reports why it will not start. Payments and provisioning stay
queued; nothing is lost, and nothing is retried blindly.

**A refund's outcome is unknown.** The refund row is `unknown` (or `created`, if the
process died before the provider answered) and nothing was posted. The
`refund-reconcile` job asks the provider again using the same operation key, so asking
is safe: a refund that actually succeeded comes back as the original result rather
than as a second refund. Do not retry it by hand with a new key — that is how a
customer is refunded twice. If it stays `unknown` after several passes, the provider's
dashboard is the authority; record what you find and let the next pass settle it.

**A refund was made twice.** It cannot happen through the panel: the refundable amount
is computed under a row lock on the payment and the intent row is committed before the
provider is called, so a concurrent second request sees the first as already spent.
If the provider shows two refunds, one of them did not come from here — check
`refunds.operation_key`, which is prefixed `refund_` for every refund this product
initiated.

## What cannot be deleted, and why

Four tables refuse mutation by trigger, not by convention:

| Table | Refuses | Why |
|---|---|---|
| `journal_entries`, `journal_transactions` | UPDATE, DELETE | the books are evidence; a correction is a reversal posting |
| `audit_events` | UPDATE, DELETE | an audit trail that can be edited is not an audit trail |
| `credit_notes` (once issued) | UPDATE of the amount, number or snapshot; DELETE | the document the customer received cannot be rewritten |
| `invoices` (once issued) | UPDATE of the snapshot | same reason: a tax authority already has a copy |

This is deliberate and it has an operational consequence worth stating plainly: **you
cannot delete these rows, even as the owner, without disabling the trigger.**

Removing test fixtures therefore has a supported path:

```sh
billing admin purge-test-data --pattern 'smoke-%@example.test' --dry-run   # preview
billing admin purge-test-data --pattern 'smoke-%@example.test' --yes       # delete
```

The command is built so that the dangerous version of it is hard to reach:

| Guard | Behaviour |
|---|---|
| `--pattern` is mandatory | there is no "purge everything" mode; a command that can empty the books with one flag eventually does |
| the pattern must be ≥ 4 characters | `a%` would match nearly everything |
| `--yes` is mandatory | a shell history recall cannot delete data |
| the database name must look like test/dev | `--force` is required otherwise, so a typo cannot reach production |
| `--dry-run` runs the real deletes and rolls back | the preview cannot disagree with the real run, and it proves the deletes are foreign-key-safe before anything commits |
| one transaction, triggers re-enabled before commit | an error anywhere leaves the guarantees switched on, because PostgreSQL DDL is transactional |
| it verifies afterwards | fails loudly if any trigger is left disabled or the ledger no longer balances |

`--dry-run` is the recommended first step against any database you have not purged before.

If you find yourself disabling these triggers by hand on a production database, stop.
Deleting financial records is a decision for the business and its accountant, not a
maintenance task. The supported correction is a counter-document: a credit note, a
refund, or a reversal posting.

## Backup and restore

Back up together, or the restore is not a restore:

1. PostgreSQL — use point-in-time recovery, not only nightly dumps. A dump loses
   every payment since it was taken.
2. The extension configuration and any secrets the core holds.
3. The encryption keys used for gateway credentials, stored separately from the
   database backup. A backup containing both is a plaintext credential store.

After a restore, expect the world to have moved: payments may have settled, servers
may have been created or destroyed. Reconcile before enabling workers — the
`unknown_operations` and `stalled_webhooks` exceptions are exactly what surfaces the
divergence.

## Scaling notes

- Payment, provisioning and notification throughput is governed by the provider's
  rate limits, not by the panel. More workers do not buy more charges.
- The renewal scanner is an index scan on `subscriptions (next_billing_at)`. The
  benchmark target in the plan (10,000 due subscriptions in five minutes) is bounded
  by invoice issuance, which takes a row lock per subscription on the number series.
- Add caches only after profiling. The overview is one query per check and is not on
  a hot path.

## Development

```bash
make db-up        # dedicated PostgreSQL 17 on 127.0.0.1:5442
make test         # the whole suite against it
make test-race    # with the race detector
make verify       # fmt + vet + race, the same sequence CI runs
make cover        # coverage across the packages that have tests
make db-unlock    # clear a test lock leaked by a killed run
make openapi      # regenerate api/openapi.yaml from the route table
make openapi-check # fail if the document has drifted from the router
make client       # regenerate the typed client in api/client
make smoke        # drive the served UI in a real headless browser
```

`make verify` runs formatting, vet, the OpenAPI and client checks, the suite under the
race detector, and the browser smoke test. The smoke test needs a headless Chromium
(`CHROME` overrides the path) and asserts things only a browser can prove: that the
module executes under the Content-Security-Policy, that a client-side route survives a
reload, and that an injected inline script is refused.

### Configuration the gate added

| Variable | Purpose |
|---|---|
| `BILLING_PUBLIC_URL` | the origin used in reset links and gateway redirects. Never derived from the `Host` header: a redirect built from it is a redirect an attacker can aim at their own domain. |
| `BILLING_SMTP_HOST` / `_PORT` / `_USERNAME` / `_PASSWORD` / `_FROM` | the mail relay. With no host, messages are written to the log and still recorded, so a development reset flow is observable end to end. |
| `BILLING_MFA_KEY` / `BILLING_MFA_KEY_ID` | the 32-byte AES-256-GCM key that seals TOTP secrets, and its id so a rotation can re-wrap without re-enrolling. Enrolment refuses when it is absent rather than storing a secret in the clear. |
| `BILLING_REFUND_APPROVAL_THRESHOLD_MINOR` | refunds at or above this need a second staff signature. `0` disables the check deliberately. A malformed value fails at startup rather than falling back, because falling back would quietly lower the bar. |
| `BILLING_TERMINATION_BATCH_APPROVAL` | the batch size at which a termination batch needs a second signature. |
| `BILLING_CHECKOUT_SUCCESS_URL` / `_CANCEL_URL` | where the gateway returns the customer. |

Each test package gets its own database (`billing_test_<pkg>`), created on demand.
The harness fails if a migration adds a table it has not classified, because an
unclassified table is how state leaks between tests.
