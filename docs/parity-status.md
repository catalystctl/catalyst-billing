# Paymenter parity — status

A living record of what is actually built, kept honest by checking the code rather than the
reports. `docs/paymenter-parity.md` is the backlog; this file is where it stands.

**Last verified:** `go test ./...` against a real PostgreSQL at schema version 0026, green;
107 browser assertions green.

**Scale now:** 76 tables (from 47), 190 documented API operations, 25 migrations.

## Where the work happens

The backlog is 277 items, so it is built in parallel by a team. `docs/parity-work-packages.md`
records the ownership rules that make that safe. The registry refactor that made it possible:

- Routes register themselves from the feature's own file, so no one edits a shared list.
- OpenAPI descriptions register the same way, so a route cannot ship undescribed.
- Frontend screens are one entry in `web/src/routes.ts`.
- Migrations are numbered centrally; the integrator owns `internal/store/migrations/`.

## Schema

47 tables before this work; **76 now**. The commerce gap named in the backlog (§5) is closed:

`currencies`, `tax_rates`, `coupons`, `coupon_products`, `coupon_redemptions`, `carts`,
`cart_items`, `config_options`, `config_option_prices`, `service_configs`, `product_upgrades`,
`service_cancellations`, `service_upgrades`, `tickets`, `ticket_messages`,
`ticket_attachments`, `ticket_mail_logs`, `notification_templates`,
`notification_preferences`, `notifications`, `settings`, `announcements`, `cron_runs`,
`worker_heartbeat`, `error_logs`, `http_logs`.

Migrations 0020–0024, each with the decision that shaped it recorded at the top of the file:

| Migration | What it decides |
|---|---|
| `0020_commerce.sql` | A coupon is a price modifier with a life, not a discount column; tax is a dated rate table; a currency is presentation only; a cart holds a selection, never a price. |
| `0021_proforma.sql` | A proforma may gain its number exactly once, in the transition that stops it being provisional. Every other frozen field stays frozen. |
| `0022_discount_lines.sql` | A discount is a line on the invoice, so the document explains itself. A charge is non-negative, a discount non-positive, and the discount line carries no tax of its own. |
| `0023_notification_events.sql` | The event set lives in Go; the database keeps a shape check. A closed set would be a migration per event. |
| `0024_notification_dedupe.sql` | The inbox is idempotent on the event instance, not the content — two genuine logins must stay two notifications. |
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
| Verification | **merged** | The browser harness waits for a screen to finish loading before asserting on it, and covers the new screens. 107 assertions green. |

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
