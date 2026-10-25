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
| E1.1 Registration and account | IDN-01, IDN-03, IDN-06, IDN-19, IDN-21 | **built** | `/register` is in `PUBLIC_PATHS` and the sign-in page links to it; `mail.must_verify` is enforced by a session gate, the token is single-use and expires, and a resend is reachable without enumerating addresses. |
| E1.2 Cart and customer checkout | CT-01…CT-10, SF-18, SF-20, SF-21 | **built** | `carts`/`cart_items` hold a selection and never a price; checkout re-prices from the catalog under a lock; stock and per-user limits are taken in sorted order. One order, one invoice, N services, one balanced posting. Terms acceptance recorded with a hash of the text accepted. |
| E1.3 Plan types and periods | SF-13, SF-14, SF-15 | **built** | `one_time` and `free` are in the interval set: a free price activates with no invoice, a one-time price creates a service with **no** subscription so the renewal engine never sees it. **Arbitrary N-day periods are expressible** as `every_<N>_days` (N in 1..366): `internal/calendar` carries a second period model that counts civil days from an explicit origin, the renewal engine bills exactly N days later, proration is the exact rational of that period, and the calendar intervals are proven unchanged by a golden test. An arbitrary N-*month* unit is still the fixed calendar set, because a month is deliberately not a day count. |
| E1.4 Coupons | CPN-01…CPN-09 | **built** | Percentage is an exact rational string, fixed is minor units; redemptions counted under a row lock; `recurring_cycles` plus `coupon_cycles_applied` makes "first N cycles" a fact; the discount is an invoice *line*, so the document explains itself. A 100%-off coupon on a one-time cart activates with no subscription (tested). |
| E1.5 Tax rates | TAX-01…TAX-08 | **built** | Dated rate table resolved per jurisdiction with an `all` wildcard, inclusive/exclusive per rate, `tax_exempt` with a reason on the billing profile, rate recorded in the invoice snapshot. No jurisdiction *rules engine* (EU VAT / reverse charge) — a rate plus a policy setting. |
| E1.6 Currencies | CUR-01…CUR-07 | **built** | Presentation only — symbol, position, grouping, scale — with arithmetic untouched. One default enforced by a partial unique index. A price missing in the cart's currency is unavailable, never free. Storefront switcher with a cart lock. |
| E1.7 Credits | CRD-01…CRD-05, CRD-08, CRD-10 | **built** | A deposit is a payment with no invoice behind it, so it posts a liability rather than revenue; auto-use spends inside the issuing transaction keyed by the invoice; the balance is derived from the subledger, never a column. Staff screen lists balances and grants credit. |
| E1.8 Invoice documents | INV-01, INV-03, INV-04, INV-07, INV-08, INV-10, INV-11, INV-14, INV-16, INV-17 | **built** | Dependency-free PDF writer, configurable numbering, proforma converts exactly once inside the settling transaction, credit notes immutable by trigger. |
| E1.9 Customer pay flow | PAY-17, PAY-18, PAY-19 | **built** | Pay with credit, pay with gateway, remainder shown, settlement waited for; a refresh re-reads and cannot double-charge because the application is keyed by the invoice. |
| E1.10 Storefront completeness | SF-01…SF-03, SF-06, SF-22, CAT-01, CAT-03 | **built** | Stable `/products/:slug` URLs, hidden products absent everywhere, staff create category + product + price in the panel, and **categories nest** — the cycle and depth bounds are enforced in the database rather than only the service layer, because two concurrent writes can each look safe. |
| E2.1 Config options | UPG-07…UPG-11 | **built** | Per-product option tree, priced per price version, stored on the service in `service_configs`. A renewal now carries one line per chosen option with a non-zero recurring amount, priced at the subscription's own price version so an option and its base cannot drift; `setup_minor` never recurs; a selection whose price row has vanished goes to `needs_review` rather than billing zero. Seven tests, including that a subscription with no options renews byte-identically. |
| E2.2 Upgrades/downgrades | UPG-01…UPG-06, CAT-09, SVC-10, CRD-06, OPS-03 | **built** | Proration by exact rational allocation, `service_upgrades` row so a half-applied change is visible, downgrade credits the difference, the provisioning operation key is derived from the service and target. No cron cancels a stale upgrade invoice. |
| E2.3 Cancellation workflow | CAN-01…CAN-06, ADM-26, SVC-09 | **built** | A request row with reason and type, a staff queue, end-of-period keeps the service to the paid date, immediate goes through the provisioning queue. |
| E2.4 Stock and limits | SF-07…SF-09 | **built** | `products.stock` and `per_user_limit` decremented under a lock in a sorted order; a zero-stock product cannot be bought. `CheckPerUserLimit` counts **pending subscriptions** as held units, which is the stricter of the two rules the codebase had and the one that stops ten unpaid orders becoming ten servers. |
| E2.5 Order cancellation | CT-11, OPS-02 | **built** | `internal/lifecycle` reads `cronjob.order_cancel_days`, cancels an unpaid order, voids its invoice through the existing void path and restores stock. Bounded, idempotent, kill-switchable (`BILLING_DISABLE_ORDER_CANCEL`). |
| E2.6 Service metadata | SVC-02, SVC-03, SVC-04, SVC-07, SVC-08 | **built** | Label, description and custom properties with per-definition `show_on_invoice` and `customer_visible`; the customer's own name wins over a derived one so a service is never blank. |
| E2.7 Termination rule | OPS-05 | **built** | `cronjob.terminate_days` drives a job that terminates through the provisioning queue, voids open invoices and notifies. A service under a manual hold is never taken by the job; kill-switchable (`BILLING_DISABLE_TERMINATION`). |
| E3.1 Tickets | TKT-01…TKT-10, TKT-14, ADM-27 | **built** | Ticket + threaded messages, one component for both audiences because the server decides which rows each caller sees; another customer's ticket is a 404, not a 403. |
| E3.2 Attachments | TKT-03 | **built** | Stored in the database with a 10 MB cap and a checked content type on download. |
| E3.3 Email piping | TKT-11, TKT-12 | **built** | Two ingest paths over one attribution core: a provider webhook authenticated by `mail.piping_secret`, and an IMAP poller on `net/textproto`. Attribution refuses an ambiguous match rather than guessing; a message from an address that does not match the ticket's customer is never appended; loop prevention drops an auto-reply or the panel's own address. |
| E3.4 Notifications | NTF-01…NTF-09, NTF-11, NTF-14 | **built** | An editable template per event seeded at migrate time, an in-app inbox, force/choice/never precedence, dedupe on the event instance. |
| E3.5 Push | NTF-05 | **built** | VAPID, RFC 8291 encryption and the POST to the subscription endpoint with no new module dependency; the encryption is proved by decrypting the ciphertext in a test. A 404/410 deletes the subscription. Service worker served from a stable URL with no-cache. |
| E3.6 Ticket cron | OPS-06 | **built** | `ticket-autoclose` job on a six-hour interval reading `tickets.autoclose_days`. |
| E3.7 Activation email | SF-11 | **built** | `products.email_template` is rendered into the activation message as `{{product_instructions}}`; empty means the shipped default sentence. Optional on update (`*string` + `COALESCE`) so a form that does not show it cannot wipe it. |
| E3.8 Ticket notifications | TKT-13 | **built** | `new_ticket_message` event, rendered with the ticket reference and a customer-facing link. |
| E4.1 Protocol v2 | PAY-10, PAY-11, SVC-11, GRW-05, CT-13, TKT-15 | **built** | Version 2 declared alongside 1; cart/order/ticket events reach a subscribed extension as notifications, never as calls that block a request; a v1 extension is still accepted and sees none of it. Conformance-tested against `extensions/example-hooks` through `extproto.Client`. |
| E4.2 Stripe | PAY-01, PAY-06, PAY-07, ADM-10 | **built** | `extensions/stripe`: checkout sessions, payment intents, refunds and webhook signatures, no SDK. The operation key is Stripe's Idempotency-Key; a refund resolves the session id the core records back to the PaymentIntent; a delivery is authenticated over timestamp and body. Configured from `/gateways`. |
| E4.3 PayPal / Mollie | PAY-02…PAY-04 | **not started** | Stripe is the only built gateway. The protocol can host more — `extensions/stripe` is the worked example — but no PayPal or Mollie adapter exists. |
| E4.4 Billing agreements | PAY-10, PAY-11 | **built** | A saved provider token with brand/last-four and a single default enforced in the database; off-session charging gated on `gateway.off_session_enabled`, routed through the same settlement path as every other payment, a decline enters dunning. The token appears in no response body. |
| E4.5 Pterodactyl | SVC-16…SVC-22, CAT-08, SF-12, ADM-11, UPG-12 | **built** | `extensions/pterodactyl` writes the operation key to Pterodactyl's own `external_id` and looks up by it, so reconciliation survives a process restart; a plan is a configured bundle of limits; config options reach the egg as environment variables. Configured from `/servers`, and **`products.server_id` exists** so a product declares the server it lives on. |
| E4.6 Second provisioner | SVC-18, SVC-19 | **built** | `extensions/cpanel` speaks the same protocol against WHM's `json-api`, which is what proves the protocol is not Pterodactyl-shaped. Its real finding: the core passed **no environment** to an extension, so both adapters were unconfigurable while the panel reported them healthy. |
| E4.7 Extension hooks | GRW-05 | **built** | An extension declares nav items and service actions as data via `ui.describe`; the core refuses a permission not in the closed set, so a hook cannot invent a capability. The whole surface is two methods: describe them, call one. |
| E5.1 System settings | ADM-02, OPS-09, NTF-10, I18N-07 | **built** | Typed accessor over one table, secrets redacted on every read path, audited write, `/settings/system` renders and saves. |
| E5.2 Admin CRUD gaps | ADM-04…ADM-09, ADM-13…ADM-18, ADM-24, ADM-25, ADM-28…ADM-37, CPN-10, CT-10, CT-12, SVC-12…SVC-15, UPG-13, UPG-14, CRD-07, INV-09, INV-18, PAY-13, PAY-14, IDN-22 | **built** | Every resource has a staff screen, and the writes exist: coupons, tax rates, currencies, config options, announcements, cancellations, settings, tickets, credits, gateways, servers, imports, locales, OAuth clients, updates, marketplace, affiliates, **and creation of a customer, a service and a staff user**. Order editing is deliberately absent — an order is a record of what was agreed, and corrections go through credit notes and cancellations rather than rewriting it. |
| E5.3 Roles and sessions | IDN-12, IDN-13, IDN-14, IDN-15, IDN-16, IDN-17, ADM-30 | **built** | `/security` lists every active session and the sign-in history and can revoke one; `/roles` edits custom roles against the real permission catalogue with the four system roles immutable in the database; `/users` creates a person with a role. Audited impersonation names both accounts, API-key CIDR allowlists are enforced, and the last administrator cannot be demoted or disabled. |
| E5.4 Observability | ADM-17…ADM-22, OPS-08, OPS-15, OPS-16 | **built** | Email, error and HTTP log viewers; cron run history with failures, per-job statistics and retry; worker heartbeat per instance, surfaced through `doctor` and the automation screen. |
| E5.5 CLI parity | OPS-17 | **built** | `create-user`, `create-customer`, `issue-invoice`, `purge-test-data`, `setting set` and `logs tail`; a setting write goes through the same validation the HTTP path uses and writes an audit row naming `BILLING_ADMIN_ACTOR`. |
| E5.6 Dashboard and navigation | ADM-01, ADM-36, ADM-37 | **built** | Revenue from the ledger rather than invoice totals, the exception queue, a selectable period, and a nav grouped like Paymenter's. |
| E5.7 Catalog polish | CAT-04, CAT-06 | **built** | A duplicate-product action copying price versions and config options with an audit row naming the source; a rich-text description with a `plain`/`html` format and a sanitising test that proves a hostile payload is neutralised. |
| E6.1 Announcements | GRW-01 | **built** | Published/expiry computed server-side as `state` (draft, scheduled, expired, live) from the server's clock; staff publish and unpublish from `/announcements`, and the notice renders on the storefront and in the customer dashboard. `announcements.enabled` is **read** by the public feed and by the staff list, so switching it off empties the feed instead of leaving a toggle that changes nothing. |
| E6.2 Affiliates | GRW-02, GRW-03 | **built** | Attribution and signup commission with exact rational arithmetic; **recurring** commission attributed to the referral on the customer, bounded by `affiliates.recurring_cycles`, written inside the settlement transaction and idempotent on the invoice, plus a payout report that reconciles. |
| E6.3 Social login | IDN-07…IDN-10 | **built** | A generic OIDC provider row rather than three hardcoded implementations; signed single-use state; an existing account with the same email is **refused rather than silently merged**, which is the one decision that prevents an account takeover, and it has a test. |
| E6.4 OAuth2 server | IDN-11 | **built** | Authorization-code grant with mandatory S256 PKCE for public clients, rotating refresh tokens, and a token that carries its owner's permissions as a **ceiling**: narrowed at issue time and again at use time, so a permission removed from a role narrows a token minted before the revocation. |
| E6.5 Importer | OPS-10, OPS-11 | **built** | WHMCS and Paymenter dumps; the dry run is the default and a real run is idempotent on the legacy id; money parsed exactly, tested on a 0-decimal and a 3-decimal currency as well as a 2-decimal one; a duplicate invoice number refuses the import and names the rows. |
| E6.6 i18n | I18N-01…I18N-03, I18N-06, I18N-08, SF-19 | **built** | Backend: locales, message catalogue, resolution chain (preference > header > default), coverage, 88 seeded keys, ten routes, a working `/locales` editor. Frontend: `web/src/lib/i18n.ts` holds the merged catalogue, persists an explicit choice like the theme, and interpolates `{{name}}` in a single pass so a customer-typed value cannot pull in another variable. The sign-in, register, forgot-password, storefront and portal surfaces render through it, and the switcher is absent when only one locale is enabled. Thirty web tests, including a missing key falling back rather than rendering blank. |
| E6.7 Updates/telemetry/marketplace | OPS-12…OPS-14, ADM-12, ADM-23 | **built** | Version reporting, a cached update check, an approved-target-version state, telemetry off by default with a payload asserted to carry no customer data, and a marketplace install that verifies SHA-256 **before** anything is executed. |
| E6.8 Discord notifications | NTF-13, GRW-04 | **built** | Webhook configured through settings, selected events posted, tested. |
| E6.9 Captcha | IDN-18 | **built** | One parameterised verifier for Turnstile/reCAPTCHA/hCaptcha; the panel's CSP is widened by exactly the configured provider's origins in `script-src`/`frame-src`/`connect-src` and by nothing when no provider is set. Per-source rate limiting stays as the backstop. |
| E6.10 Marketplace breadth | PAY-05, INV-15 | **partial** | Stripe is still the only gateway (PAY-05 not started). INV-15 is **built as a per-tenant brand overlay, not a theme registry**: a name, a logo and one accent per colour mode, configured from `/branding`, reaching the panel chrome and the invoice PDF. An unset brand emits no CSS custom property and no PDF operator, proven by a byte-hash test; a brand can only set the accent family, so it cannot break light or dark. A full theme registry (versioned documents, presets, per-scope inheritance, custom CSS) remains unbuilt and is designed in `docs/theming-system.md`. |

**Tally:** 47 built, 1 partial, 1 not started, out of 49 epics.

Every endpoint the panel serves is now either reachable or recorded with a reason in
`docs/reachability.md`, and `scripts/reachability.sh --check` fails if that stops being true. The
browser harness opens all 79 routes and drives 41 screens: **510 assertions, 0 failed, 0 not built
yet.**

What the last rounds added, all browser-driven rather than asserted: a customer can **buy** a product
end to end (the funnel was API-only — see below); an operator can set a customer's password, reset
their second factor and close their account; credit can be **taken away**, as an append-only entry
with a reason the customer can read; an order line can carry a **negotiated price** without touching
the catalog; an affiliate can be suspended, deleted, or given a custom rate; a phone call can become
a **ticket**, with custom fields that can be required to *close* and not only to create; ticket
uploads work for both audiences; tax rates can be **imported** from a source, previewed, and kept
with provenance; a staff login **owns a billing account** automatically; an **offline payment** can
be recorded so a bank transfer marks an invoice paid; invoices, customers and services export as
**streaming CSV** with exact money; and **MFA is findable** — enrolment with a locally drawn QR,
recovery codes shown once, and a disable that demands the password *and* a code.

The single most serious thing found this round is recorded in `docs/reachability.md`: the purchase
funnel did not exist. Every endpoint worked, and nothing in the frontend ever added an item to a
cart, so a customer could not buy anything and every order was an operator typing it in. It was
found by diffing the served route table against the view table — the method that has now found this
class of defect three times — and `scripts/reachability.sh --check` makes it a test rather than a
report.

Every epic that gates taking money, provisioning a server, or managing a person is built. What is
left is two specific things, and neither is a half-finished epic:

| Epic | Status | The one thing missing |
|---|---|---|
| E4.3 PayPal / Mollie | not started | Stripe is the only built gateway. The protocol can host more — `extensions/stripe` is the worked example — but no PayPal or Mollie adapter exists. |
| E6.10 marketplace breadth | partial | A per-tenant brand overlay (name, logo, one accent per colour mode) reaches the panel and the invoice PDF; a versioned theme registry — presets, per-scope inheritance, custom CSS — is designed in `docs/theming-system.md` and not built. PAY-05 (a second gateway) is untouched. |

Closed this round, each with a test that can fail: **a paid config option now recurs on every
renewal** (it was a live revenue leak — the renewal invoice carried only the base line, so a
surcharge was charged once and never again while the resource kept running); **the i18n frontend**
(a helper holding the merged catalogue, single-pass interpolation, and the sign-in, register,
storefront and portal surfaces actually translated); **nested categories** with the cycle and
depth bounds enforced in the database, because two concurrent writes can each look safe; and
**an arbitrary N-day billing period**, which is a second period model rather than a number added
to the first — the calendar intervals are proven byte-identical to their pre-change arithmetic by
a differential test, so "every 45 days" could not move the renewal date of an existing
subscription.

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
| Announcements | `/announcements` | **present** — seeded published announcement, its state named (live/scheduled/expired), and a banner when the feed is switched off |
| Announcement feed (customer) | `/portal`, `/products` | **present** — the seeded announcement renders for the customer and for a visitor; it disappears when `announcements.enabled` is off |
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

## What this round closed

Every item in the previous "not done" list that could be closed has been. Each of these is a
decision with a test behind it, not a checkbox:

- **`order:create:self`** exists, so checkout no longer borrows `invoice:pay:self` to sell a free
  product. Nine other permissions came with it, each split where a read and a write deserved
  different grants: `credit:read:all` versus `credit:write`, `service:write:self` versus
  `service:read:self`, `paymentmethod:*:self` for a saved instrument.
- **Registration is a public path**, and `mail.must_verify` is enforced end to end: the session
  gate refuses an unverified account, the token is single-use and expires, and a resend is
  reachable without revealing whether an address exists.
- **One-time and free products exist.** A free price activates with no invoice; a one-time price
  creates a service with no subscription, so the renewal engine never sees it.
- **Order cancellation and termination run.** `cronjob.order_cancel_days` and
  `cronjob.terminate_days` were settings nothing read; they are now two bounded, idempotent,
  individually kill-switchable jobs. Termination goes through the provisioning queue, and a
  service under a manual hold is never taken by the job.
- **Service metadata exists**: label, description, custom properties, with the customer's own
  name winning over a derived one so a service is never blank.
- **The staff credit screen is real.** `/credits` lists balances, searches, and grants credit
  through the existing money path.
- **Credential mail is templated.** Password reset and verification are editable templates with
  their own events; the non-enumeration, single-use and expiry properties are each tested.
- **A product can word its activation email.** `products.email_template` was a column nothing
  read; it is now rendered into the activation message.
- **Stock and per-user limits reconciled.** `internal/service`'s helper adopted checkout's
  stricter rule — a pending subscription occupies a slot — because the looser one let a customer
  place ten unpaid orders and take delivery of ten services the moment the last payment settled.
- **Live role refresh** works without a restart, as a short TTL with an explicit hook rather than
  a query on the guarded path.
- **Real gateways and provisioners exist**: a Stripe adapter and a Pterodactyl adapter, both
  driven through the core's own protocol client in their conformance tests, and both
  configurable from the panel.
- **A locale catalog.** Two locales ship, `de` disabled but fully translated; a missing key falls
  back rather than rendering blank, and a customer's stored choice beats their browser's header.
- **Email piping works.** Both halves of `TKT-11`: a provider webhook authenticated by the piping
  secret, and an IMAP poller written on `net/textproto` rather than by adding a mail library.
  Attribution refuses an ambiguous match instead of guessing, a message from an address that does
  not match the ticket's customer is never appended to it, and a mail loop is made impossible
  rather than unlikely.
- **A cPanel provisioner.** `extensions/cpanel` speaks the same protocol as the Pterodactyl
  adapter, which is what proves the protocol is not Pterodactyl-shaped. It also produced the
  finding that mattered most this round: the core passed no environment to an extension at all,
  so both real adapters were unconfigurable and the panel reported them as healthy.
- **Extensions can contribute a nav item and a service action**, as data, with the permission
  checked against the closed set — never as code the core executes.
- **Web push**, with RFC 8291 encryption proved by decrypting the ciphertext in a test, and a
  service worker served from a stable URL.
- **OAuth2 and social login**, where a token is an attenuation of its owner's permissions rather
  than a second authorization path, and an existing account is never silently merged by email.
- **Captcha** on registration and password reset, with the panel's CSP widened by exactly the
  configured provider's origins and by nothing at all when no provider is set.
- **Recurring affiliate commission** that reconciles against a payout report, bounded by the
  configured cycles and idempotent on the invoice.
- **An importer** whose dry run is the default and whose money parsing is exact for a 0-decimal
  and a 3-decimal currency as well as a 2-decimal one.

## The management surface is complete

This was the largest gap and it is closed. It was found by comparing the route table against the
view table rather than by reading the epics, and it was real: the panel listed and acted on
customers, services, orders and invoices but could not **create** most of what it manages, and
thirteen API surfaces had no screen at all.

What an operator can now do, all browser-verified:

| Capability | Before | Now |
|---|---|---|
| Create a staff user | `billing admin create-user` on the host | `/users` — email, name, password and role in one act, for a customer or any of the four staff roles |
| Create another administrator | shell only | same screen; the last administrator cannot be demoted or disabled, and an operator cannot disable themselves |
| Manage roles | API served, no screen | `/roles` — the four system roles read-only (the separation of duties is written against them), custom roles created and edited against the real permission catalogue |
| Create a customer | SQL | `/customers` — no password, invited through the reset flow, with address and tax exemption |
| Create a service | SQL | `/services` — reuses checkout's subscription creation so the renewal engine sees it, takes stock and the per-user limit the same way, audits the operator, and a comped service produces no subscription |
| Register from the sign-in page | `/register` worked and nothing linked to it | a link on `/login`, pinned by a browser assertion |
| See sessions and sign-in history | nothing | `/security` — every active session, revoke one, and the success/failure history |
| See and lift holds | nothing | `/holds` — a hold stops billing without stopping the service, so "why is this customer not invoiced" now has an answer |
| Edit the upgrade graph | SQL | `/upgrades` — add and remove edges with the proration consequence stated |
| Configure sign-in providers | SQL | `/providers` — write-only secrets, and the account-takeover refusal stated plainly |
| Edit notification templates | hard-coded | `/notification-templates` — every event, editable, with a live preview; an unknown `{{placeholder}}` is a 422 at save time, not a 500 |
| See what runs on the box | nothing | `/automation` — every periodic job with its interval and kill switch |
| Edit service property definitions | SQL | `/service-properties` — the catalogue that decides what a service may carry and what prints on a document |

## Not done, and why

One epic is not started and one is partial. Both are breadth, not capability:

- **E4.3 PayPal / Mollie — not started.** Stripe is built and the protocol hosts more; these two
  adapters are not written. A shop that needs PayPal needs someone to write one adapter, and
  `extensions/stripe` is the worked example with a conformance test to copy. Deliberately deferred:
  no customer has asked for it.
- **E6.10 — partial.** The invoice PDF is branded per tenant now (name, logo, accent per colour
  mode), which is the part that mattered. What is not built is a *versioned theme registry* —
  presets, per-scope overrides, a theme applied to one product line but not another. That is a
  different and much larger thing than branding, and the boundary is stated in `docs/branding.md` §5
  so a later reader does not mistake the overlay for the registry.

### Two defects that are capability, not breadth

These are not on the epic list because they are not missing features — they are features that exist
and are not reached. Both are recorded here because a tested, complete-looking implementation is the
exact thing a reader will assume is working.

- **Configured tax never reaches an invoice, and the cart already quotes it.** `internal/tax` is a
  full jurisdiction engine — EU place-of-supply, VIES VAT-ID validation, US subnational nexus,
  reverse charge, and a refusal that refuses rather than charging 0%. `Server.IssuerFor` prices at
  the customer's jurisdiction. Nothing in production calls it: checkout, the manual invoice, orders
  and the renewal worker all issue through the shared issuer, which is
  `FixedTaxPricer{Policy:"no-tax"}`. Meanwhile `taxPricerFor` is what the CART uses, so the panel
  quotes one number and documents another. With `tax.enabled` on and rates configured, an
  inclusive-tax installation shows totals that agree numerically while the invoice under-states net
  and omits the VAT amount — a VAT invoice that cannot support the customer's input-VAT reclaim —
  and an exclusive-tax installation's cart total and invoice total disagree outright.

  The storefront tax label and `GET /api/v1/tax/disclosure` are therefore deliberately **not**
  shipped: a tax label beside an invoice with no tax line is a false statement to the customer.
  The fix is one change across all four issuance paths, and it must be all four — half of it means
  the first invoice of a subscription carries VAT while every renewal does not, which is worse than
  uniformly carrying none. The seam must live below `httpapi` (three of the four paths are outside
  it: `register.NewCheckoutService`, `orders.New`, `renewal.Engine`), and the rate must be resolved
  inside the issuing transaction so the immutable `snapshot` records the rate that actually applied.
  One commercial decision is open: when VIES is unreachable, refuse, or fall back to the destination
  rate and record the substitution in the snapshot. Refusing turns a third-party outage into failed
  EU B2B checkout; the fallback over-collects and is recoverable.

- **Outbound mail is not localised, though the panel is.** The client renders in the user's locale
  and every email goes out in English, so a customer who picked German gets a German panel and an
  English invoice. `notify.NewLocalizedSender` exists on an unmerged branch
  (`agent-team/i18n-ieqw64`) and is referenced only by its own test; its resolver method
  `CatalogSet` is absent from master's `i18n.Store`. Finishing it is: port
  `internal/notify/locale.go`, add `CatalogSet`, place one line in `cmd/billing/jobs.go` at the
  notifications sender, and prove a non-default-locale recipient gets translated mail.

Three things moved from this list to **done** in the last round, each with a test that can fail:

- **An arbitrary N-day billing period** is expressible as `every_<N>_days` (N in 1..366).
  `internal/calendar` now carries a second period model that counts civil days from an explicit
  origin, kept deliberately apart from the calendar-month model. The calendar intervals are proven
  byte-identical by a differential test rather than by a claim about the diff.
- **A per-tenant invoice PDF** exists, with an unset brand proven byte-identical by hash.
- **The purchase funnel** — a customer can buy end to end in a browser, which was the single largest
  gap in the product.

Two things on this list are decisions rather than gaps:

- **Self-applying panel updates.** The update check and the approved target version are real; the
  panel does not replace its own binary, because doing that safely needs a deployment shape this
  repository does not have. An update is approved and the operator deploys. That is the correct
  behaviour, not a missing feature.
- **Order editing.** Deliberately absent. An order is the record of what was agreed; corrections
  go through credit notes and cancellations, which is what keeps an issued invoice immutable.

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
