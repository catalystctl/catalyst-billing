# Paymenter parity — status

A living record of what is actually built, kept honest by checking the code rather than the
reports. `docs/paymenter-parity.md` is the backlog; this file is where it stands.

**Last verified:** `go test ./...` against a real PostgreSQL at schema version 0026, green;
**244 browser assertions green** (`bash scripts/browser-e2e.sh`), covering 19 of the 21
commerce/support/account screens, with one reachability gap and one screen not built — both
named below rather than hidden.

**Scale now:** 79 tables (from 47), 204 documented API operations, 26 migrations, 58 view
modules across 57 routes.

## Where the work happens

The backlog is 277 items, so it is built in parallel by a team. `docs/parity-work-packages.md`
records the ownership rules that make that safe. The registry refactor that made it possible:

- Routes register themselves from the feature's own file, so no one edits a shared list.
- OpenAPI descriptions register the same way, so a route cannot ship undescribed.
- Frontend screens are one entry in `web/src/routes.ts`.
- Migrations are numbered centrally; the integrator owns `internal/store/migrations/`.

## Schema

47 tables before this work; **79 now**. The commerce gap named in the backlog (§5) is closed:

`currencies`, `tax_rates`, `coupons`, `coupon_products`, `coupon_redemptions`, `carts`,
`cart_items`, `config_options`, `config_option_prices`, `service_configs`, `product_upgrades`,
`service_cancellations`, `service_upgrades`, `tickets`, `ticket_messages`,
`ticket_attachments`, `ticket_mail_logs`, `notification_templates`,
`notification_preferences`, `notifications`, `settings`, `announcements`, `cron_runs`,
`worker_heartbeat`, `error_logs`, `http_logs`.

Migrations 0020–0026, each with the decision that shaped it recorded at the top of the file:

| Migration | What it decides |
|---|---|
| `0020_commerce.sql` | A coupon is a price modifier with a life, not a discount column; tax is a dated rate table; a currency is presentation only; a cart holds a selection, never a price. |
| `0021_proforma.sql` | A proforma may gain its number exactly once, in the transition that stops it being provisional. Every other frozen field stays frozen. |
| `0022_discount_lines.sql` | A discount is a line on the invoice, so the document explains itself. A charge is non-negative, a discount non-positive, and the discount line carries no tax of its own. |
| `0023_notification_events.sql` | The event set lives in Go; the database keeps a shape check. A closed set would be a migration per event. |
| `0024_notification_dedupe.sql` | The inbox is idempotent on the event instance, not the content — two genuine logins must stay two notifications. |
| `0025_growth.sql` | Affiliate attribution and commission are rows keyed to the customer and the referral, so a payout report reconciles instead of being recomputed. |
| `0026_identity.sql` | A role is a row, so an operator can define one without a deploy; the four shipped roles stay system rows with database-level immutability, because their permission sets encode a separation of duties the approval flow depends on. |

## Packages

| Package | State | What it delivers |
|---|---|---|
| Commerce core | **merged** | Coupons with exact rational percentages and redemptions counted under a row lock; dated tax rates resolved per jurisdiction with an `all` wildcard; currency presentation without touching arithmetic; a cart that re-prices from the catalog. |
| Customer purchase | **merged** | Self-registration (non-enumerating), password change, and customer checkout. `customer_id` comes from the session, never the body. Stock and per-user limits under a lock taken in sorted order. A zero-total order activates with no invoice. |
| Invoice documents | **merged** | A dependency-free PDF writer; proforma conversion; configurable numbering; client-visible notes; void reasons. |
| Support tickets | **merged** | Tickets, threaded messages, attachments with a 10 MB cap, a mail log, and auto-close. Another customer's ticket is a 404, not a 403. |
| Notifications | **merged** | An editable template per event, an in-app inbox, per-user channel preferences with force/choice precedence, and dedupe on the event instance. Templates seed at migrate time. |
| Service lifecycle | **merged** | Cancellation as a request row with a queue; upgrades with exact rational proration and a `service_upgrades` row so a half-applied change is visible; config options priced per price version; stock decremented under a lock. |
| Operator surface | **merged** | System settings with secrets redacted, log viewers, cron history and heartbeat, a revenue dashboard reading the ledger rather than invoice totals, announcements. |
| Identity depth | **merged** | Session list and revoke, audited impersonation (the audit row names both the account and the operator), editable roles with the four system roles immutable in the database, API-key CIDR allowlists enforced. |
| Frontend commerce | **merged** | Twelve screens: cart, checkout, register, coupons, tax rates, currencies, config options, system settings, announcements, logs, cancellations, credit. |
| Credits and pay flow | **merged** | A deposit is a payment with no invoice behind it, so it posts a liability rather than revenue. Auto-use spends a balance in the issuing transaction, keyed by the invoice so a replay spends nothing twice. Pay-with-credit, payment polling and manual payments. |
| Growth and platform | **merged** | Affiliate attribution, commission recorded inside the settlement transaction with exact rational arithmetic, Discord notifications configured through settings. |
| Frontend support/account | **merged** | Tickets on both sides (one component, because the server decides which rows each caller sees), the staff queue, and the customer security, notifications and payment-method screens. |
| Verification | **merged** | The browser harness waits for a screen to finish loading before asserting on it, then drives the new screens with seeded data. It distinguishes *present* from *broken* from *not built*, so a screen that has not landed is a status and a screen that has landed and renders wrong is a failure. 244 assertions green. |

## The epics, by status

`docs/paymenter-parity.md` §4 lists the epics and the acceptance test that proves each one.
This table is where each actually stands, checked against the code rather than against the
package that built it. Three states, and no fourth:

- **built** — the epic's acceptance behaviour exists and is exercised by a test or the browser
  harness.
- **partial** — some of it exists; the parenthetical says what is missing.
- **not started** — nothing of it is in the codebase. Recorded so the gap is visible, not so it
  looks like a plan.

| Epic | IDs | Status | Evidence, or what is missing |
|---|---|---|---|
| E1.1 Registration and account | IDN-01, IDN-03, IDN-06, IDN-19, IDN-21 | **partial** | `internal/register` creates a non-enumerating account; password change and the empty portal work. **`/register` is not in `PUBLIC_PATHS`, so an anonymous visitor is bounced to `/login`** — the form works but the audience it exists for cannot reach it. No email verification flow is enforced (`mail.must_verify` is reported, not required). |
| E1.2 Cart and customer checkout | CT-01…CT-10, SF-18, SF-20, SF-21 | **built** | `carts`/`cart_items` hold a selection and never a price; checkout re-prices from the catalog under a lock; stock and per-user limits are taken in sorted order. One order, one invoice, N services, one balanced posting. |
| E1.3 Plan types and periods | SF-13, SF-14, SF-15 | **partial** | A zero-total order activates with no invoice (the free case). `price_versions.interval` is `CHECK`ed to monthly/quarterly/semiannual/annual, so a **one-time product cannot be expressed** — the schema, not the service, is the blocker. |
| E1.4 Coupons | CPN-01…CPN-09 | **built** | Percentage is an exact rational string, fixed is minor units; redemptions are counted under a row lock; `recurring_cycles` plus `coupon_cycles_applied` makes "first N cycles" a fact; the discount is an invoice *line*, so the document explains itself. |
| E1.5 Tax rates | TAX-01…TAX-08 | **built** | Dated rate table resolved per jurisdiction with an `all` wildcard, inclusive/exclusive per rate, `tax_exempt` with a reason on the billing profile, rate recorded in the invoice snapshot. |
| E1.6 Currencies | CUR-01…CUR-07 | **built** | Presentation only — symbol, position, grouping, scale — with arithmetic untouched. One default enforced by a partial unique index. A price missing in the cart's currency is unavailable, never free. |
| E1.7 Credits | CRD-01…CRD-05, CRD-08, CRD-10 | **built** | A deposit is a payment with no invoice behind it, so it posts a liability rather than revenue; auto-use spends inside the issuing transaction keyed by the invoice; the balance is derived from the subledger, never a column. |
| E1.8 Invoice documents | INV-01, INV-03, INV-04, INV-07, INV-08, INV-10, INV-11, INV-14, INV-16, INV-17 | **built** | Dependency-free PDF writer, configurable numbering, proforma converts exactly once inside the settling transaction, credit notes immutable by trigger. |
| E1.9 Customer pay flow | PAY-17, PAY-18, PAY-19 | **built** | Pay with credit, pay with gateway, remainder shown, settlement waited for; a refresh re-reads and cannot double-charge because the application is keyed by the invoice. |
| E1.10 Storefront completeness | SF-01…SF-03, SF-06, SF-22, CAT-01, CAT-03 | **built** | Stable `/products/:slug` URLs, hidden products absent everywhere, staff create category + product + price in the panel. |
| E2.1 Config options | UPG-07…UPG-11 | **built** | Per-product option tree, priced per price version, stored on the service in `service_configs` with the amount captured at selection time. |
| E2.2 Upgrades/downgrades | UPG-01…UPG-06, CAT-09, SVC-10, CRD-06, OPS-03 | **built** | Proration by exact rational allocation, `service_upgrades` row so a half-applied change is visible, downgrade credits the difference. **No cron cancels a stale upgrade invoice** — `OPS-03` has no job. |
| E2.3 Cancellation workflow | CAN-01…CAN-06, ADM-26, SVC-09 | **built** | A request row with reason and type, a staff queue, end-of-period keeps the service to the paid date, immediate goes through the provisioning queue. |
| E2.4 Stock and limits | SF-07…SF-09 | **built** | `products.stock` and `per_user_limit` decremented under a lock in a sorted order; a zero-stock product cannot be bought. |
| E2.5 Order cancellation | CT-11, OPS-02 | **not started** | `cronjob.order_cancel_days` exists as a setting and **nothing reads it**: `buildJobs` wires payments, provisioning, retention, ticket-autoclose, dunning, renewals, refund-reconcile and notifications, and no order-cancel job. |
| E2.6 Service metadata | SVC-02, SVC-03, SVC-04, SVC-07, SVC-08 | **not started** | `services` has no label, description or custom-property column, and no screen offers one. |
| E2.7 Termination rule | OPS-05 | **not started** | `cronjob.terminate_days` is a setting with no job behind it. |
| E3.1 Tickets | TKT-01…TKT-10, TKT-14, ADM-27 | **built** | Ticket + threaded messages, one component for both audiences because the server decides which rows each caller sees; another customer's ticket is a 404, not a 403. |
| E3.2 Attachments | TKT-03 | **built** | Stored in the database with a 10 MB cap and a checked content type on download. |
| E3.3 Email piping | TKT-11, TKT-12 | **partial** | The model is ready — `message_id` dedupe, `ErrDuplicate`, `ticket_mail_logs` — but **there is no inbound ingestion path**: no IMAP poller and no webhook that turns a message into a ticket reply. |
| E3.4 Notifications | NTF-01…NTF-09, NTF-11, NTF-14 | **built** | An editable template per event seeded at migrate time, an in-app inbox, force/choice/never precedence, dedupe on the event instance. |
| E3.5 Push | NTF-05 | **not started** | No subscription table, no service worker, no sender. |
| E3.6 Ticket cron | OPS-06 | **built** | `ticket-autoclose` job on a six-hour interval reading `tickets.autoclose_days`. |
| E3.7 Activation email | SF-11 | **not started** | `products.email_template` is a column nothing reads. |
| E3.8 Ticket notifications | TKT-13 | **built** | `new_ticket_message` event, rendered with the ticket reference and a customer-facing link. |
| E4.1 Protocol v2 | PAY-10, PAY-11, SVC-11, GRW-05, CT-13, TKT-15 | **partial** | `internal/extproto` declares the spec; the cart/order/ticket events and the service-action hooks are not conformance-tested against a second implementation. |
| E4.2 Stripe | PAY-01, PAY-06, PAY-07, ADM-10 | **not started** | Fake fixtures only. |
| E4.3 PayPal / Mollie | PAY-02…PAY-04 | **not started** | As above. |
| E4.4 Billing agreements | PAY-10, PAY-11 | **not started** | No stored method, no off-session charge. |
| E4.5 Pterodactyl | SVC-16…SVC-22, CAT-08, SF-12, ADM-11, UPG-12 | **not started** | The provisioning queue, fencing and leases are real; the adapter is not. |
| E4.6 Second provisioner | SVC-18, SVC-19 | **not started** | Needs E4.5 first. |
| E4.7 Extension hooks | GRW-05 | **partial** | Extensions can be loaded and can contribute routes; a client nav item and a service action are not exposed. |
| E5.1 System settings | ADM-02, OPS-09, NTF-10, I18N-07 | **built** | Typed accessor over one table, secrets redacted on every read path, audited write, `/settings/system` renders and saves. |
| E5.2 Admin CRUD gaps | ADM-04…ADM-09, ADM-13…ADM-18, ADM-24, ADM-25, ADM-28…ADM-37, CPN-10, CT-10, CT-12, SVC-12…SVC-15, UPG-13, UPG-14, CRD-07, INV-09, INV-18, PAY-13, PAY-14, IDN-22 | **partial** | Coupons, tax rates, currencies, config options, announcements, cancellations, settings and tickets all have staff CRUD. Missing: the service-metadata and order-edit surfaces (they need E2.5/E2.6), and `/credits` is still a placeholder module. |
| E5.3 Roles and sessions | IDN-12, IDN-13, IDN-14, IDN-15, IDN-16, IDN-17, ADM-30 | **built** | Session list/revoke, audited impersonation that names both accounts, editable roles with the four system roles immutable in the database, API-key CIDR allowlists enforced. |
| E5.4 Observability | ADM-17…ADM-22, OPS-08, OPS-15, OPS-16 | **built** | Email, error and HTTP log viewers; cron history; worker heartbeat surfaced through `doctor`. |
| E5.5 CLI parity | OPS-17 | **partial** | `create-user`, `create-customer`, `issue-invoice`, `purge-test-data`. No setting change and no log tail from the CLI. |
| E5.6 Dashboard and navigation | ADM-01, ADM-36, ADM-37 | **built** | Revenue from the ledger rather than invoice totals, the exception queue, and a nav grouped like Paymenter's. |
| E5.7 Catalog polish | CAT-04, CAT-06 | **not started** | No duplicate-product action, no rich-text description editor. |
| E6.1 Announcements | GRW-01 | **built** | Published/expiry computed server-side as `live`; staff publish and unpublish from `/announcements`. |
| E6.2 Affiliates | GRW-02, GRW-03 | **partial** | Attribution and signup commission with exact rational arithmetic. **Recurring commission is deliberately not paid** — a renewal has no signup to attribute and it is a policy decision. |
| E6.3 Social login | IDN-07…IDN-10 | **not started** | |
| E6.4 OAuth2 server | IDN-11 | **not started** | |
| E6.5 Importer | OPS-10, OPS-11 | **not started** | |
| E6.6 i18n | I18N-01…I18N-03, I18N-06, I18N-08, SF-19 | **not started** | |
| E6.7 Updates/telemetry/marketplace | OPS-12…OPS-14, ADM-12, ADM-23 | **not started** | |
| E6.8 Discord notifications | NTF-13, GRW-04 | **built** | Webhook configured through settings, selected events posted, tested. |
| E6.9 Captcha | IDN-18 | **not started** | Registration is rate-limited per source address instead. |
| E6.10 Marketplace breadth | PAY-05, INV-15 | **not started** | |

**Tally:** 23 built, 8 partial, 18 not started, out of 49 epics. The revenue path (P1) is
complete except registration reachability, one-time products and email verification; the
lifecycle gaps in P2 are jobs that were specified and not written; P4 and P6 are almost entirely
open.

## Browser coverage

The harness reports three outcomes per screen and prints all three, so this section can be
checked by running `bash scripts/browser-e2e.sh` rather than trusted.

| Screen | Path | Result |
|---|---|---|
| Register (form) | `/register` | **present** — form fills, submits, shows the accepted state, and does not claim an account was created |
| Register (anonymous) | `/register` | **gap** — not in `PUBLIC_PATHS`, so a visitor is sent to `/login` |
| Cart | `/cart` | **present** — seeded line priced from the catalog |
| Checkout | `/checkout` | **present** — order summary, not the empty state |
| Coupons | `/coupons` | **present** — seeded code in the table |
| Tax rates | `/tax-rates` | **present** — seeded DE 19% rate |
| Currencies | `/currencies` | **present** — seeded currency with its render style |
| Config options | `/config-options?product_id=…` | **present** — seeded dropdown on the seeded product |
| System settings | `/settings/system` | **present** — eight groups of editable fields |
| Support queue | `/support` | **present** — seeded ticket |
| Tickets (staff) | `/tickets` | **present** — seeded ticket |
| Tickets (customer) | `/tickets` | **present** — seeded ticket, different columns |
| New ticket | `/tickets/new` | **present** — fields render and are enabled |
| Logs | `/logs` | **present** — three tabs; the email tab reads the real outbox |
| Announcements | `/announcements` | **present** — seeded published announcement |
| Cancellations | `/cancellations` | **present** — empty queue, correctly |
| Credit (staff) | `/credits` | **not built** — placeholder module |
| Credit (customer) | `/portal/credits` | **present** — balance derived from the seeded posting |
| Security | `/portal/security` | **present** — password, MFA, sessions, login history |
| Notifications | `/portal/notifications` | **present** — inbox and preferences |
| Payment methods | `/portal/payment-methods` | **present** — explains that no method is stored, because no gateway is configured |

All 19 present screens also pass the assertions the harness already had: no error state, a
heading, no `[object Object]`, content under the heading, no unexpected console error, and —
at 390px — no sideways scroll with the table rebuilt into cards.

## Integration work done by hand

Things a package could not do because the file belongs to someone else, or because it is a
shared surface:

- **Proforma conversion in the settlement path.** `internal/fulfilment` converts a proforma
  inside the transaction that settles the payment, so "paid" and "numbered" cannot come apart.
- **`invoice_paid` notification.** Wired in the same transaction, with a test that settles,
  asserts one delivery, replays the webhook, and asserts still one. A provider retrying a
  webhook is the normal case.
- **Template seeding at migrate time.** In `cmdMigrate`, not a migration, so an operator's
  edited wording is never overwritten by a deploy.
- **Ticket auto-close job.** Wired into the worker, reading `tickets.autoclose_days`.
- **The permission set.** Grew from 27 to 45 strings, split by the decision each grants rather
  than by screen, with the four system roles updated to match.
- **The customer route guard.** A customer now reaches their own account screens instead of
  being bounced to `/portal` by a Settings link that went nowhere.

## Not done, and why

Recorded rather than hidden:

- **`order:create:self`.** Checkout borrows `invoice:pay:self`, which is defensible (placing an
  order creates an obligation to pay) but is a borrow. A dedicated permission is a decision,
  not a merge artifact.
- **Registration is not a public path.** `PUBLIC_PATHS` omits `/register`, so the screen is
  unreachable by the visitor it exists for (see the defects section). One line in
  `web/src/routes.ts`.
- **One-time products cannot be expressed.** `price_versions.interval` is `CHECK`ed to
  monthly/quarterly/semiannual/annual, so `SF-13`'s one-time product is a schema change, not a
  service change.
- **Order cancellation and termination have settings and no jobs.** `cronjob.order_cancel_days`
  and `cronjob.terminate_days` are read by nothing (`OPS-02`, `OPS-05`).
- **Service metadata was never added.** `services` has no label, description or custom-property
  column (`SVC-02`/`03`/`04`).
- **Email piping has a model and no ingest.** `message_id` dedupe and `ticket_mail_logs` are in
  place; no IMAP poller or inbound webhook turns a message into a reply (`TKT-11`).
- **The staff credit screen is a placeholder.** `/credits` renders "not available in this
  build"; the customer screen is real. The harness reports it as not built rather than skipping
  it silently.
- **Templated credential mails.** Password reset and email verification still use the
  hardcoded messages. Switching them gets operator-editable wording and an inbox row, but it
  changes the text a customer receives during a security flow and deserves its own review.
- **Stock/limit reconciliation.** Checkout implements stock and per-user limits inline with a
  sorted lock order; `internal/service` now exports its own helpers. One implementation should
  win — and checkout's counts pending subscriptions toward the per-user limit, which is the
  stricter and correct behaviour, so the reconciliation should adopt it rather than the reverse.
- **Recurring affiliate commission** (`GRW-03`). Commission is paid on a signup. A renewal
  invoice has no signup to attribute, and a recurring commission is a policy decision the growth
  package deliberately does not make silently.
- **Live role refresh for the CLI.** The Authorizer reloads on a role edit through HTTP; a role
  created directly in the database is picked up at the next boot.
- **Real gateways and provisioners** (`PAY-01`, `SVC-18`). Still the fake fixtures. This is a
  protocol-and-adapter job, not a schema one.
- **i18n, social login, OAuth2 server, importers, marketplace** (`P6`). Not started.

## The never-regress list, checked

| Rule | Evidence |
|---|---|
| Money is exact | No float in any money path; coupons use `money.ParseRatio`; proration uses `money.Allocate`. |
| Issued invoices are immutable | `TestIssuedInvoiceIsImmutable`, `TestProformaConversionAndItsBoundaries`. |
| Payment ingestion is idempotent | `TestVerticalSliceReplayedWebhook`, `TestPaidInvoiceNotifiesTheCustomerOnce`. |
| Provisioning is queued and fenced | Unchanged; the e2e slice drives it. |
| The audit log is append-only | Trigger-enforced, unchanged. |
| Dual approval is database-enforced | `CHECK` in `0011_contracts.sql`, unchanged. |
| Customer reads are scoped in SQL | Ticket and invoice tests assert 404 for another customer's id. |
| Every route is a table entry | `TestPermissionCoverage` passes with the new routes. |
| Every error is problem+json | `TestEveryErrorIsProblemJSON` passes. |

## Defects the verification caught

Worth recording, because each was invisible to a green unit-test run:

- **A credit deposit credited nobody.** Every test built its own payment applier, so the whole
  repository passed while production wired the plain one — a settled deposit marked its invoice
  paid and left the balance at zero, with no error. The construction is now a named function with
  a test that asserts what the worker actually builds.
- **The impersonation guard was dead code.** It shipped with the impersonation work and nothing
  called it, so an operator acting as a customer could enrol an authenticator on that account: a
  permanent takeover. A control that is not wired is worse than no control, because it is trusted.
- **The API-key allowlist was decorative.** Stored, administered, never checked.
- **A proforma could not become an invoice.** The immutability trigger refused the only UPDATE
  that converts one.
- **A coupon had nowhere to go.** `invoice_lines` refused a negative line, so a discount could only
  be folded into unit prices — a document that shows `$80` against a `$100` price list with nothing
  explaining the difference.
- **The browser harness read a skeleton.** It asserted on `innerText` immediately after
  navigation, so the dashboard checks passed or failed on the nav rail's text rather than the
  page's.
- **`/register` is unreachable by a visitor.** The screen is built, correct, and driven by the
  harness — but `PUBLIC_PATHS` in `web/src/routes.ts` does not list it, so the shell sends an
  anonymous visitor to `/login` before the form renders. Registration works for someone who is
  already signed in, which is the one audience that does not need it. The harness reports this
  as a *reachability gap* rather than a failure, because the fix is a one-line change in a file
  the frontend package owns.
- **`purge-test-data` could not remove what this work added.** Its step list had no entry for
  `tickets`, so a seeded ticket blocked `DELETE FROM customers` and aborted the whole purge
  transaction — the command failed identically on every retry, and the browser harness hid it
  behind `|| true` while leaking a staff account, a customer and a catalogue entry per run. The
  step list now covers tickets, carts, redemptions, cancellations, upgrades, service configs,
  affiliate referrals and notifications; the harness no longer silences the command and counts
  the leftover rows afterwards.
- **A cleanup that cannot fail is a cleanup that cannot tell you it stopped working.** The
  `|| true` on the purge was the reason the leak above survived three runs. The harness now
  prints the purge's own output on failure and names any table that still holds seed rows.
