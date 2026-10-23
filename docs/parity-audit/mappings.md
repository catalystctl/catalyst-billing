# Parity audit — the verdict table and the six mapping sections

Read-only audit of `docs/paymenter-parity.md` at `5c922bd`, covering **everything outside the
numbered inventory rows in §3**: §1 Verdict, §5 Data-model mapping, §6 Admin resource mapping,
§7 Client page mapping, §8 Settings key mapping, §9 Notification event mapping, §10 Extension
inventory.

The §3 rows (`SF-01`, `CT-01`, …) are audited by other agents and are not touched here.

**Method.** Every cell in each table was re-derived from the tree, not from the previous
audit's opinion. Where the claim is about Paymenter itself (a count of Paymenter's own
resources, keys or routes) it cannot be checked from this repository and is marked
`unverifiable-from-tree`; only the Catalyst column and the gap/status cell are audited for
those rows.

Reproduce any count below from the repository root:

```sh
ls web/src/views/*.ts | wc -l                                    # 79
grep -c '^  route(' web/src/routes.ts                            # 79
grep -hoi 'CREATE TABLE[A-Z ]*\(IF NOT EXISTS \)\?[a-z_]*' \
  internal/store/migrations/*.sql | sed -E \
  's/CREATE TABLE( IF NOT EXISTS)? //I' | sort -u | wc -l         # 109
grep -cE '\{Group: "[a-z0-9]+", Key:' internal/settings/settings.go # 72
grep -oE '\{Group: "[a-z0-9]+"' internal/settings/settings.go \
  | sort -u | wc -l                                              # 17
grep -c 'Permission = "' internal/authn/authz.go                 # 52
grep -c 'operationId:' api/openapi.yaml                          # 353
grep -cE '^    /[^:]*:' api/openapi.yaml                         # 278
ls -d extensions/*/ | wc -l                                      # 6
go run ./cmd/openapigen -check                                   # "the document matches the router"
```

**Headline.** The mapping sections were written against a tree that no longer exists. Twenty-five
of the thirty-three tables §5 called missing are in the schema under the name it gave, six more
exist under another name, and exactly **one** (`notification_subscriptions`) is genuinely absent;
thirteen of the twenty-six client pages §7 called absent are now routes; nine of the twelve
notification events §9 called absent now have a Go function. The single most damaging line in
the document is §1's `API operations | 78 operations` — the generated contract has **353
operations across 278 paths**, and `openapigen -check` proves the document is current, so the
number is not a stale draft but wrong by a factor of four and a half.

---

## §1 Verdict

**18 cells checked, 13 stale.** (Cells = the 18 data rows of the verdict table. The
`At a glance. 277 numbered items: 48 already satisfied / 73 partial / 154 missing / 2 deliberate
divergences` line is a roll-up of §3 and is therefore owned by the §3 auditors; it is out of
scope here but it is *also* stale — see the note at the end of this section.)

| # | Item | OLD claim (quoted) | CORRECTED claim | Evidence |
|---|---|---|---|---|
| 1 | Admin resources | `27 resource classes …` / `~24 staff screens` | **53 staff screens** (55 staff routes, 53 distinct staff view modules) | `web/src/views/*.ts` → 79 files; `routes.ts` declares 79 routes → 8 public + 16 customer + 55 staff; staff-only unique modules = 53 (`grep -c '^  route(' web/src/routes.ts` → 79). Examples absent from the old count: `coupons.ts`, `tax-rates.ts`, `currencies.ts`, `config-options.ts`, `roles.ts`, `gateways.ts`, `servers.ts`, `marketplace.ts`, `updates.ts`, `imports.ts`, `locales.ts`, `oauth-clients.ts`, `notification-templates.ts`, `automation.ts`, `service-properties.ts`, `holds.ts`, `upgrades.ts`, `providers.ts`, `ticket-fields.ts`, `branding.ts`, `logs.ts`, `cancellations.ts`, `support-queue.ts`, `users.ts`, `security.ts`, `affiliates.ts`, `credits.ts` |
| 2 | Admin pages | `5` / `1 dashboard` / `❌ settings, updates, cron stats, extension marketplace` | **8 operator pages**: system settings, branding, updates, marketplace, cron/automation, imports, locales, logs | `web/src/routes.ts` lines for `/settings/system`→`system-settings.ts`, `/branding`→`branding.ts`, `/settings/updates`→`updates.ts`, `/marketplace`→`marketplace.ts`, `/automation`→`automation.ts`, `/imports`→`imports.ts`, `/locales`→`locales.ts`, `/logs`→`logs.ts` |
| 3 | Dashboard widgets | `7` / `1 exception dashboard` / `🟡 no revenue/active-users/support widgets` | **one dashboard with the full widget set**: revenue by currency, active customers, new customers, open tickets, tickets opened, overdue invoices, paid-not-provisioned, plus the exception list | `web/src/views/dashboard.ts` `interface DashboardWidgets` — `revenue`, `active_customers`, `new_customers`, `open_tickets`, `tickets_opened`, `overdue_invoices`, `paid_not_provisioned`; period selector `PERIODS` (7d/30d/90d/custom) |
| 4 | Data models | `52 model files / 56 core tables` / `47 tables` / `🟡 34 missing tables` | **109 tables** in the migrations, + `schema_migrations` created by the runner = 110 | `grep -hoi 'CREATE TABLE…' internal/store/migrations/*.sql \| sort -u \| wc -l` → 109; `internal/store/migrate.go:157` `CREATE TABLE IF NOT EXISTS schema_migrations` |
| 5 | Client pages | `~26` / `6 portal pages + 2 storefront pages` / `🟡 no cart, tickets, credits, payment methods, security, notifications` | **23 client-facing pages**: 11 under `/portal`, 5 customer commerce/support (`/cart`, `/checkout`, `/tickets`, `/tickets/new`, `/tickets/:id`), 2 storefront, 5 auth (`/login`, `/register`, `/forgot-password`, `/reset-password`, `/verify`) | `web/src/routes.ts`: `route("/cart"…)`, `route("/checkout"…)`, `route("/tickets"…)`, `route("/portal/credits"…)`, `route("/portal/payment-methods"…)`, `route("/portal/security"…)`, `route("/portal/notifications"…)` |
| 6 | Payment gateways | `1 protocol, fake only` / `❌` | **2 in-tree gateways, 1 real**: `extensions/stripe` (real Stripe adapter, checkout + webhook + refund) and `extensions/fakegateway` (fixture) | `extensions/stripe/stripe.go`, `webhook.go`; methods `gateway.describe`, `gateway.create_checkout`, `gateway.query_payment`, `gateway.verify_webhook`, `gateway.refund` in `extensions/stripe/*.go`; `grep -rhoE '"gateway\.[a-z_]+"' extensions/stripe/*.go` |
| 7 | Server integrations | `1 protocol, fake only` / `❌` | **3 in-tree provisioners, 2 real**: `extensions/pterodactyl`, `extensions/cpanel`, `extensions/fakeprov` | `extensions/pterodactyl/ptero.go`, `extensions/cpanel/whm.go`; both expose `provisioner.create/describe/inspect/lookup/suspend/resume/terminate/validate`; `extensions/README.md` "Real adapters — `cpanel` is not a fixture" |
| 8 | Other extensions | `3` / `0` / `❌` | **1 in-tree hook extension** (`extensions/example-hooks`) + a real extension host (`installed_extensions`, `installed_extension_artifacts`, `/marketplace`) | `extensions/example-hooks/main.go` — `ui.describe`, `events.subscribe`, `service.reboot`; `internal/store/migrations/` `installed_extensions`, `installed_extension_artifacts`, `marketplace_index`, `marketplace_config`; `web/src/views/marketplace.ts` |
| 9 | Locales | `24` / `1` / `❌` | **2 seeded locales** (`en` enabled + default, `de` disabled) over a full i18n system: `locales` + `message_catalog` tables, 88 seeded keys, a `/locales` editor, and `web/src/lib/i18n.ts` on the client | `internal/store/migrations/0037_i18n.sql:128-131` (`INSERT INTO locales … ('en'…),('de'…)`); `grep -c "^    ('" …/0037_i18n.sql` → 88; `internal/i18n/catalog.go`; `web/src/lib/i18n.ts` |
| 10 | Themes | `theme packages + 24 marketplace themes` / `1 design system, dark mode only` / `⛔ / ❌` | **2 themes (dark + light)** on one token design system, plus an operator brand overlay (name, logo, two accents) with a `/branding` screen | `web/src/lib/theme.ts` `export type Theme = "dark" \| "light"`; `web/src/styles/tokens.css` two `data-theme` blocks; `settings.go` group `branding` (`name`, `logo_id`, `accent_light`, `accent_dark`); `web/src/views/branding.ts` |
| 11 | Settings keys | `77 across 11 groups` / `env + DB policies, no system settings surface` / `❌` | **72 keys across 17 groups** in a real `settings` table with a `/settings/system` screen and `GET/PUT /api/v1/staff/settings` | `grep -cE '\{Group: "[a-z0-9]+", Key:' internal/settings/settings.go` → 72; `grep -oE '\{Group: "[a-z0-9]+"' … \| sort -u \| wc -l` → 17 (general, invoices, credits, tax, tickets, cronjob, mail, announcements, checkout, affiliates, gateway, servers, push, captcha, i18n, updates, branding); `internal/httpapi/system_settings.go:41-42`; `web/src/views/system-settings.ts` |
| 12 | API operations | `10 admin REST resources` / `78 operations, generated OpenAPI + typed client` / `✅ stronger` | **353 operations across 278 paths** in the generated contract, and it is current | `grep -c 'operationId:' api/openapi.yaml` → 353; `grep -cE '^    /[^:]*:' api/openapi.yaml` → 278; `go run ./cmd/openapigen -check` → `openapigen: the document matches the router` |
| 13 | Permissions | `editable roles, 169 permission strings` / `27 fixed permissions, 4 fixed roles` / `🟡 no role editing` | **52 fixed permissions + 4 system roles, and custom roles are editable** (`POST/PUT/DELETE /staff/roles`, `/roles` screen) | `grep -c 'Permission = "' internal/authn/authz.go` → 52; `RoleAdministrator`, `RoleBilling`, `RoleSupport`, `RoleInfrastructure` at `authz.go:173-176`; `internal/authn/roles.go` `CreateRole`, `UpdateRolePermissions`, `DeleteRole`, `ListRoles`; `web/src/views/roles.ts` |

**Accurate cells (5).** `Audit` (`✅ stronger` — `audit_events` + append-only trigger), `Money`
(`✅ stronger` — `internal/money`), `Invoices` (`✅ stronger` — issued-invoice trigger +
`invoices.snapshot`), `Payments` (`✅ stronger` — `gateway_events` + `idempotency_keys`),
`Provisioning` (`✅ stronger` — `service_operations` + leases/fencing), and the `5` in the
`Admin pages` Paymenter column (unverifiable-from-tree).

**Note on the `At a glance` line.** `277 numbered items: 48 already satisfied, 73 partial, 154
missing, 2 deliberate divergences` does not survive contact with the tree either — commit
`5c922bd`'s own message records the re-derived result as `120 done / 62 absent / 88 partial`
over 285 rows, and `docs/parity-audit/*.tsv` is the per-row evidence for that pass. Whoever
finishes the §3 audit must also fix this roll-up; it is listed here only because it sits inside
§1, and it is not counted in the 18 cells above.

---

## §5 Data-model mapping

**42 cells checked, 25 stale** (the 42 Paymenter-table rows). The closing
`Missing commerce tables, counted: 34` paragraph is a 43rd claim and it is stale too, for a
third reason — the arithmetic does not agree with its own list.

| # | Row | OLD claim (quoted) | CORRECTED claim | Evidence |
|---|---|---|---|---|
| 1 | `roles` | `staff_roles + code-defined Role` / `🟡 not editable` | **editable** — `roles` + `role_permissions` tables, `staff_roles` for staff assignment; `CreateRole`/`UpdateRolePermissions`/`DeleteRole` | `internal/store/migrations/0027_permission_expansion.sql` (`roles`, `role_permissions`); `internal/authn/roles.go:65,112,160,190`; `web/src/views/roles.ts` |
| 2 | `oauth_clients`, `oauth_*` | `—` / `❌` | **all present**: `oauth_clients`, `oauth_consents`, `oauth_auth_codes`, `oauth_grants`, `oauth_access_tokens`, `oauth_signing_keys` | `internal/store/migrations/0038_oauth.sql:73,113,132,160,186,294` |
| 3 | `settings` | `env + billing_policies + retention_policies` / `🟡 no general settings table` | **`settings` table exists** (one row per `(group, key)`), 72 declared keys, read/written as a group | `internal/store/migrations/0020_commerce.sql:553` `CREATE TABLE settings`; `internal/settings/settings.go` `definitions` |
| 4 | `extensions` | `env (BILLING_*_EXTENSION)` / `🟡 no records/UI` | **`installed_extensions`, `installed_extension_artifacts`, `marketplace_index`, `marketplace_config`** + `/marketplace` screen | `0042_updates.sql:134,152,174,199`; `web/src/views/marketplace.ts`; `internal/httpapi/extension_hooks.go` |
| 5 | `products` | `products` / `🟡 missing stock/limits/server/email_template` | **all four now exist**: `stock`, `per_user_limit`, `allow_quantity`, `server_id`, `email_template` | `0020_commerce.sql:291-296` (`stock`, `per_user_limit`, `allow_quantity`); `0049_catalog_structure.sql:199` (`server_id`); `0031_product_email_template.sql:39` (`email_template`) |
| 6 | `plans` | `price_versions (interval + name)` / `🟡 no free/one-time/arbitrary period` | **free, one-time and arbitrary n-day periods all exist** | `0028_plan_types.sql` — `price_versions.interval` grows to `monthly/quarterly/semiannual/annual/one_time/free`; `0059_n_day_periods.sql` — `every_<N>_days` in the same `interval` string |
| 7 | `product_upgrades` | `—` / `❌` | **`product_upgrades` exists** | `0020_commerce.sql:305` |
| 8 | `config_options`, `config_option_products` | `—` / `❌` | **`config_options` + `config_option_prices` exist** (the join table is replaced by a price table keyed to the option and the product's price version) | `0020_commerce.sql:228` (`config_options`), `:258` (`config_option_prices`); `web/src/views/config-options.ts` |
| 9 | `custom_properties`, `properties` | `—` / `❌` | **`services.custom_properties` (jsonb) + `service_property_definitions` exist**, with a trigger refusing a value with no definition | `0029_service_metadata.sql:70` (`ADD COLUMN custom_properties jsonb`), `:93-95` (object/string CHECKs), `service_property_definitions` table; `web/src/views/service-properties.ts` |
| 10 | `coupons`, `coupon_products` | `—` / `❌` | **both exist**, plus `coupon_redemptions` | `0020_commerce.sql:150,185,194`; `web/src/views/coupons.ts` |
| 11 | `tax_rates` | `— (fixed pricer)` / `❌` | **`tax_rates` exists** (dated rows) + `tax_import_sources`/`tax_import_runs` | `0020_commerce.sql:102`; `0055_tax_import.sql:47,72`; `internal/tax`; `web/src/views/tax-rates.ts` |
| 12 | `currencies` | `— (currency code on price)` / `❌` | **`currencies` exists** with `is_default` (partial unique index) | `0020_commerce.sql:52`, `:77` `CREATE UNIQUE INDEX currencies_one_default`; `web/src/views/currencies.ts` |
| 13 | `carts`, `cart_items` | `—` / `❌` | **both exist** | `0020_commerce.sql:322,337`; `web/src/views/cart.ts` |
| 14 | `service_configs` | `services.config` JSON / `🟡` | **`service_configs` table exists** | `0020_commerce.sql:272` |
| 15 | `service_upgrades` | `—` / `❌` | **`service_upgrades` exists** | `0020_commerce.sql:383`; `internal/httpapi/service_upgrades.go` |
| 16 | `service_cancellations` | `subscriptions.cancel_at_period_end + cancelled_at` / `🟡` | **`service_cancellations` table exists** (request with reason and type) | `0020_commerce.sql:361`; `internal/httpapi/service_cancellations.go:55-57`; `web/src/views/cancellations.ts` |
| 17 | `credits` | `ledger customer_credit` / `✅ (no deposit flow)` | **deposit flow exists** — credit grant + staff debit entry type | `internal/httpapi/credits.go`, `internal/httpapi/payflow.go`, `internal/openapi/descriptions_credits.go`; `0051_credit_debit.sql` (staff debit entry type); `web/src/views/portal-credits.ts` |
| 18 | `billing_agreements` | `—` / `❌` | **`payment_methods` + `gateway.off_session_enabled`** — the migration names PAY-10 billing agreements explicitly | `0036_payment_methods.sql:1-3` ("Billing agreements (PAY-10): a saved payment method…"); `internal/gatewayconfig/config.go:88-90`; `web/src/views/portal-payment-methods.ts` |
| 19 | `tickets`, `ticket_messages`, `ticket_message_attachments`, `ticket_mail_logs` | `—` / `❌` | **all four exist** (`ticket_message_attachments` is named `ticket_attachments`) | `0020_commerce.sql:409,437,458,474`; `0044_ticket_piping.sql`; `0054_ticket_fields.sql` (`ticket_fields`, `ticket_field_values`); `internal/httpapi/ticket_attachments.go` |
| 20 | `notification_templates`, `notification_preferences`, `notification_subscriptions`, `notifications` | `notification_deliveries` / `🟡 outbox only` | **three of the four exist** — `notification_templates`, `notification_preferences`, `notifications`; only `notification_subscriptions` has no analogue (per-event preferences in `notification_preferences` replace it) | `0020_commerce.sql:494,519,528`; `0023_notification_events.sql`; `0024_notification_dedupe.sql`; `internal/notify/preferences.go`; `web/src/views/notification-templates.ts` |
| 21 | `email_logs` | `notification_deliveries` / `🟡` | **a real email log surface exists** — the durable outbox is the source, rendered as a tab | `web/src/views/logs.ts:1-6` ("Email — what was sent, to whom, and whether it worked. The durable outbox is the source") |
| 22 | `cron_stats` | `— (ops overview)` / `❌` | **`cron_runs` + `worker_heartbeat` exist**, with an `/automation` screen that reads the registered job list | `0020_commerce.sql:628` (`cron_runs`), `:642` (`worker_heartbeat`); `web/src/views/automation.ts` |
| 23 | `debug_logs` | `structured logs` / `🟡` | **`error_logs` and `http_logs` tables** with a three-tab `/logs` screen | `0020_commerce.sql:655` (`error_logs`), `:666` (`http_logs`); `web/src/views/logs.ts` |
| 24 | `announcements` (extension) | `—` / `❌` | **`announcements` exists** (core, not an extension) | `0020_commerce.sql:607`; `settings.go` group `announcements`; `web/src/views/announcements.ts` |
| 25 | `affiliates`, `affiliate_orders` (extension) | `—` / `❌` | **`affiliates`, `affiliate_referrals`, `affiliate_balances`, `affiliate_payouts`, `affiliate_recurring_commissions` exist** | `0025_growth.sql:52,86,107`; `0043_affiliate_recurring.sql`; `0053_affiliate_control.sql`; `web/src/views/affiliates.ts`, `web/src/views/portal-affiliate.ts` |
| — | Closing paragraph (43rd claim) | `Missing commerce tables, counted: 34` | **recount: 33 tables are named in the sentence, and the arithmetic does not reach 34** (25 now exist under the same name, 6 exist under another name, 1 — `notification_subscriptions` — is genuinely missing) | see below |

**The `34` is wrong three ways.** (a) The sentence enumerates **29 table names** plus "the four
other `oauth_*` tables", i.e. 33 items, so the stated total of 34 only closes if that last
phrase is five tables — and on the Catalyst side it is: `oauth_clients` plus
`oauth_consents`, `oauth_auth_codes`, `oauth_grants`, `oauth_access_tokens`,
`oauth_signing_keys`. (b) **25 of the 33 now exist under the name the sentence gives.**
(c) Of the remaining 9, **7 exist under another name or shape** and only **2 are absent as a
table** — one of which (`billing_agreements`) has a capability substitute:

| Named as missing | Status now | Where |
|---|---|---|
| `carts`, `cart_items`, `coupons`, `coupon_products`, `tax_rates`, `currencies`, `config_options`, `service_configs`, `service_upgrades`, `product_upgrades`, `service_cancellations`, `tickets`, `ticket_messages`, `ticket_mail_logs`, `notification_templates`, `notification_preferences`, `notifications`, `oauth_clients`, `announcements`, `affiliates` | **exist** | migrations as cited above |
| `oauth_access_tokens`, `oauth_auth_codes`, `oauth_consents`, `oauth_grants`, `oauth_signing_keys` | **exist** | `0038_oauth.sql` |
| `config_option_products` | **exists as `config_option_prices`** | `0020_commerce.sql:258` |
| `custom_properties`, `properties` | **exist as `services.custom_properties` + `service_property_definitions`** | `0029_service_metadata.sql` |
| `ticket_message_attachments` | **exists as `ticket_attachments`** | `0020_commerce.sql:458` |
| `cron_stats` | **exists as `cron_runs` + `worker_heartbeat`** | `0020_commerce.sql:628,642` |
| `email_logs` | **exists as the outbox + `/logs` email tab** | `web/src/views/logs.ts` |
| `affiliate_orders` | **exists as `affiliate_referrals`** | `0025_growth.sql:107` |
| `notification_subscriptions` | **genuinely absent** — per-event preferences in `notification_preferences` replace it | — |
| `billing_agreements` | **genuinely absent as a table; the capability is `payment_methods`** | `0036_payment_methods.sql` |

**Real answer:** the schema gap is **one table** (`notification_subscriptions`), not 34.

**Accurate rows in §5 (17):** `users`, `api_keys`, `sessions`/`user_sessions`,
`user_authentication_logs`, `password_reset_tokens`, `categories`, `prices`, `orders`,
`services`, `invoices`, `invoice_items`, `invoice_snapshots`, `invoice_transactions`,
`adjustment_notes` (still `🟡 credit only`), `audits`/`audit_logs`,
`failed_jobs`/`jobs`/`job_batches`, `migrations`.

---

## §6 Admin resource mapping

**34 cells checked, 25 stale.** A "cell" is the `Catalyst screen` + `Status` pair for one
Paymenter resource.

| # | Paymenter resource | OLD claim (quoted) | CORRECTED claim | Evidence |
|---|---|---|---|---|
| 1 | `CouponResource` | `—` / `❌` | `/coupons` | `web/src/views/coupons.ts`; `routes.ts` `route("/coupons", …)`; nav "Revenue → Coupons" gated on `coupon:read:all` (`web/src/ui/layout.ts`) |
| 2 | `ConfigOptionResource` | `—` / `❌` | `/config-options` | `web/src/views/config-options.ts`; nav "Configuration → Config options" (`config:write`) |
| 3 | `TaxRateResource` | `—` / `❌` | `/tax-rates` | `web/src/views/tax-rates.ts`; nav "Configuration → Tax rates" (`tax:write`) |
| 4 | `CurrencyResource` | `—` / `❌` | `/currencies` | `web/src/views/currencies.ts`; nav "Configuration → Currencies" (`currency:write`) |
| 5 | `CustomPropertyResource` | `—` / `❌` | `/service-properties` | `web/src/views/service-properties.ts`; `service_property_definitions` (`0029_service_metadata.sql`) |
| 6 | `RoleResource` | `—` / `❌` | `/roles` | `web/src/views/roles.ts`; nav "People → Roles" (`staff:manage`); `internal/authn/roles.go` |
| 7 | `GatewayResource` | `—` / `❌` | `/gateways` | `web/src/views/gateways.ts`; nav "Integrations → Gateways" (`gateway:write`); settings group `gateway` |
| 8 | `ServerResource` | `—` / `❌` | `/servers` | `web/src/views/servers.ts`; nav "Integrations → Servers" (`server:write`); settings group `servers`; `servers` table |
| 9 | `ExtensionResource` | `—` / `❌` | `/marketplace` | `web/src/views/marketplace.ts`; nav "Integrations → Marketplace" (`extension:manage`); `installed_extensions` |
| 10 | `NotificationTemplateResource` | `—` / `❌` | `/notification-templates` | `web/src/views/notification-templates.ts`; nav "Platform → Notification templates" (`notification:write`) |
| 11 | `OauthClientResource` | `—` / `❌` | `/oauth-clients` | `web/src/views/oauth-clients.ts`; nav "Platform → OAuth clients" (`oauth:manage`) |
| 12 | `UserResource` | ``/customers`, `/customers/:id`` / `🟡` | `/customers`, `/customers/:id`, **`/users`** (staff users), **`/security`** (sessions/MFA) | `web/src/views/customers.ts`, `customer.ts`, `users.ts`, `security.ts`; nav "People → Users/Security" (`staff:manage`); `internal/authn/users.go`, `sessions.go` |
| 13 | `ServiceResource` | ``/services`, `/services/:id`` / `🟡` | `/services`, `/services/:id`, plus `/holds`, `/cancellations`, `/upgrades`, `/service-properties` | `web/src/views/services.ts`, `service.ts`, `holds.ts`, `cancellations.ts`, `upgrades.ts`, `service-properties.ts`; `internal/httpapi/service_upgrades.go`, `internal/httpapi/holds_ext.go` |
| 14 | `ServiceCancellationResource` | `—` / `❌` | `/cancellations` | `web/src/views/cancellations.ts`; nav "Operations → Cancellations" (`service:read:all`); `internal/httpapi/service_cancellations.go` |
| 15 | `TicketResource` | `—` / `❌` | `/support` (staff queue) and `/tickets` | `web/src/views/support-queue.ts` (staff queue, `ticket:read:all`), `tickets.ts`; `internal/httpapi/tickets*.go` |
| 16 | `TicketMessageResource` | `—` / `❌` | ticket detail screen with replies and attachments | `web/src/views/ticket.ts`, `support.ts` (staff "open a ticket for a customer"); `internal/httpapi/ticket_attachments.go` |
| 17 | `EmailLogResource` | `—` / `❌` | `/logs` — "Email" tab | `web/src/views/logs.ts:1-6`; `notification_deliveries` |
| 18 | `ErrorLogResource` | `—` / `❌` | `/logs` — "Errors" tab | `web/src/views/logs.ts:1-6`; `error_logs` (`0020_commerce.sql:655`) |
| 19 | `HttpLogResource` | `—` / `❌` | `/logs` — "HTTP" tab | `web/src/views/logs.ts:1-6`; `http_logs` (`0020_commerce.sql:666`) |
| 20 | `Pages: Settings` | ``/settings` (personal only)` / `🟡` | `/settings` (personal) **and `/settings/system` (system settings, 72 keys in 17 groups)** | `web/src/views/settings.ts`, `system-settings.ts`; `internal/httpapi/system_settings.go:41-42` |
| 21 | `Pages: CronStats` | ``/operations` overview` / `🟡` | `/automation` (job list from the registered jobs + worker heartbeat) plus `/operations` | `web/src/views/automation.ts:1-20`; `web/src/views/operations.ts`; `cron_runs`, `worker_heartbeat` |
| 22 | `Pages: Updates` | `—` / `❌` | `/settings/updates` | `web/src/views/updates.ts`; nav "Platform → Updates" (`update:manage`); `update_check`, `update_config`, `update_approval` tables (`0042_updates.sql`) |
| 23 | `Pages: Extension (marketplace)` | `—` / `❌` | `/marketplace` | `web/src/views/marketplace.ts`; `marketplace_index`, `marketplace_config` |
| 24 | `Widgets: Overview/Revenue/ActiveUsers/Support/CronStat` | ``/` dashboard (exception-focused)` / `🟡` | the dashboard now carries **revenue, active customers, new customers, open tickets, tickets opened, overdue invoices** as well as the exception list | `web/src/views/dashboard.ts` `interface DashboardWidgets`; `GET /api/v1/staff/dashboard/widgets` |
| 25 | `Clusters: Extensions/Invoice/Services` | `single nav` / `🟡` | **10 nav groups**: Overview, Revenue, People, Operations, Support, Configuration, Integrations, Platform, Trust, Account | `web/src/ui/layout.ts` `navGroups()` |

**Accurate cells (9):** `ProductResource` → `/catalog-management` (`catalog-admin.ts`),
`CategoryResource` → `/catalog-management` (`catalog-admin.ts`, `internal/httpapi/catalog_nesting.go`
for the tree), `ApiResource` → `/api-keys` `🟡` (still own-keys only: the staff endpoints in
`internal/httpapi/apikeys.go:33-36` act on the caller's keys),
`OrderResource` → `/orders`, `/orders/:id`, `InvoiceResource` → `/invoices`, `/invoices/:id`,
`InvoiceTransactionResource` → `/payments`, `/payments/:id`, `AdjustmentNoteResource` →
invoice detail credit notes (`credit_notes`, `credit_applications`), `Audits` → `/audit`
`✅ stronger`, `FailedJobResource` → `/operations` `🟡` (`service_operations`).

---

## §7 Client page mapping

**26 cells checked, 13 stale.**

| # | Paymenter page | OLD claim (quoted) | CORRECTED claim | Evidence |
|---|---|---|---|---|
| 1 | `/products/{category}/{product}/checkout` | `—` / `❌` | `/checkout` | `routes.ts` `route("/checkout", () => import("./views/checkout"))`; `CUSTOMER_PREFIXES` includes `/checkout`; `web/src/views/checkout.ts` |
| 2 | `/cart` | `—` / `❌` | `/cart` | `routes.ts` `route("/cart", …)`; `web/src/views/cart.ts`; `carts`/`cart_items` tables |
| 3 | `/register` | `—` / `❌` | `/register` | `routes.ts` `route("/register", …)`; `PUBLIC_PATHS` includes `/register`; `web/src/views/register.ts`; `internal/httpapi/register.go` |
| 4 | `/oauth/{provider}` | `—` / `❌` | `GET /oauth/social/{slug}/start` (server redirect, not an SPA page) | `internal/httpapi/social.go:24-25` — "GET /oauth/social/{slug}/start the redirect to the provider… /callback the provider's return"; `social_providers` (`0038_oauth.sql:211`); `web/src/views/providers.ts` |
| 5 | `/services/{service}/upgrade` | `—` / `❌` | `/portal/services/:id` — the upgrade section of the service page | `web/src/views/portal-service.ts:16-98` (`UpgradeOption`, `UpgradePrice`, upgrade-options call); `internal/httpapi/service_upgrades.go:30,32` (`/api/v1/portal/services/{id}/upgrade-options`, `…/upgrade`) |
| 6 | `/tickets` | `—` / `❌` | `/tickets` | `routes.ts` `route("/tickets", …)`; `web/src/views/tickets.ts`; nav "Your account → Tickets" in `layout.ts` |
| 7 | `/tickets/create` | `—` / `❌` | `/tickets/new` | `routes.ts` `route("/tickets/new", () => import("./views/ticket-new"))` |
| 8 | `/tickets/{ticket}` | `—` / `❌` | `/tickets/:id` | `routes.ts` `route("/tickets/:id", () => import("./views/ticket"))` |
| 9 | `/account/security` | `— (staff /settings has MFA; customers have no page)` / `❌` | `/portal/security` | `routes.ts` `route("/portal/security", …)`; `web/src/views/portal-security.ts`; nav "Account → Security" for customers in `layout.ts` |
| 10 | `/account/credits` | `— (balance on portal overview)` / `🟡` | `/portal/credits` — a full statement screen | `routes.ts` `route("/portal/credits", …)`; `web/src/views/portal-credits.ts`; nav "Your account → Credit" |
| 11 | `/account/payment-methods` | `—` / `❌` | `/portal/payment-methods` | `routes.ts` `route("/portal/payment-methods", …)`; `web/src/views/portal-payment-methods.ts`; `payment_methods` (`0036_payment_methods.sql`) |
| 12 | `/account/notifications` | `—` / `❌` | `/portal/notifications` | `routes.ts` `route("/portal/notifications", …)`; `web/src/views/portal-notifications.ts`; `notification_preferences`; `internal/notify/preferences.go` |
| 13 | `/tickets/attachments/{uuid}` | `—` / `❌` | `GET /api/v1/portal/tickets/attachments/{id}` (server route, streamed download) | `internal/httpapi/ticket_attachments.go:70-72` `r.get("/api/v1/portal/tickets/attachments/{id}", authn.PermTicketReadSelf, …)` |

**Accurate cells (13):** `/` → `/` (`storefront.ts`), `/products/{category}` →
`/products?category=` `🟡` (`storefront.ts:84,131` filters on `?category=`),
`/products/{category}/{product}` → `/products/:slug` `🟡` (`storefront-product.ts`), `/login` →
`/login`, `/2fa` → login second step, `/password/request`+`/password/reset/{token}` →
`/forgot-password`+`/reset-password`, `/email/verify` → `/verify`, `/dashboard` → `/portal`,
`/invoices` → `/portal/invoices`, `/invoices/{invoice}` → `/portal/invoices/:id`, `/services` →
`/portal/services`, `/services/{service}` → `/portal/services/:id` `🟡`, `/account` →
`/portal/profile` `🟡`.

---

## §8 Settings key mapping

**45 cells checked (77 keys), 34 stale.** A "cell" is one row; a row that lists several keys is
counted stale if any of its Catalyst columns is wrong.

| # | Group / key | OLD claim (quoted) | CORRECTED claim | Evidence |
|---|---|---|---|---|
| 1 | general `company_name` | `env / no branding` / `❌` | `general.company_name` (default `"Catalyst Hosting"`), plus the `branding.name` overlay | `settings.go:161`; group `branding` (`name`) |
| 2 | general `timezone` | `BILLING_*`/calendar timezone / `🟡` | **no `timezone` setting and no `BILLING_TIMEZONE`** — the timezone is per price (`price_versions.anchor_timezone`) | `0001_core.sql:121` (`timezone text NOT NULL` on `price_versions`); no `BILLING_TIMEZONE` in `grep -rhoE 'BILLING_[A-Z_]+' --include=*.go .` |
| 3 | general `app_language` | `English only` / `❌` | `i18n.default_locale` (default `"en"`) | `settings.go` group `i18n`; `0037_i18n.sql` |
| 4 | general `allowed_languages` | `English only` / `❌` | `i18n.enabled_locales` (JSON array, default `["en"]`) | `settings.go` group `i18n` |
| 5 | general `logo`, `logo_dark`, `favicon` | `no branding` / `❌` | `branding.logo_id` + `branding.name` + `branding.accent_light`/`accent_dark` (one logo, not two — a per-theme accent pair replaces `logo_dark`) | `settings.go` group `branding`; `web/src/views/branding.ts`; `0060_branding.sql` |
| 6 | general `system_email_address` | `SMTP from address env` / `🟡` | `general.system_email_address` (default `no-reply@localhost`) | `settings.go:162` |
| 7 | general `tos` | `none` / `❌` | `general.tos` + `checkout.tos_required` | `settings.go:163`; group `checkout` |
| 8 | security `captcha`, `captcha_site_key`, `captcha_secret` | `none` / `❌` | `captcha.provider` (`none\|turnstile\|recaptcha\|hcaptcha`), `captcha.site_key`, `captcha.secret_key` | `settings.go` group `captcha`; `internal/captcha`; `internal/httpapi/captcha.go` |
| 9 | social-login `oauth_google*` | `none` / `❌` | `social_providers` rows (generic OIDC: slug, issuer, endpoints, client id/secret, scopes) + `/providers` screen | `0038_oauth.sql:211`; `internal/oauth/social.go`; `web/src/views/providers.ts` |
| 10 | social-login `oauth_github*` | `none` / `❌` | same — a provider row, slug `github` | as above |
| 11 | social-login `oauth_discord*` | `none` / `❌` | same — a provider row, slug `discord` | as above |
| 12 | tax `tax_enabled`, `tax_type` | `fixed pricer config` / `🟡` | `tax.enabled` (bool) and `tax.default_mode` (`exclusive\|inclusive`), plus a real `tax_rates` table | `settings.go` group `tax`; `0020_commerce.sql:102` |
| 13 | mail `mail_disable`, `mail_must_verify` | `none` / `❌` | `mail.disable`, `mail.must_verify` | `settings.go` group `mail` |
| 14 | mail `mail_host … mail_from_name` | `env SMTP` / `🟡` | `mail.smtp_host`, `smtp_port`, `smtp_username`, `smtp_password` (secret), `smtp_encryption`, `smtp_from` — `from_address`/`from_name` collapse into one `smtp_from` | `settings.go` group `mail`; `internal/notify/smtp.go` |
| 15 | tickets `tickets_disabled` | `no tickets` / `❌` | `tickets.disabled` | `settings.go` group `tickets` |
| 16 | tickets `ticket_departments` | `no tickets` / `❌` | `tickets.departments` (JSON, default `["general","billing","technical"]`) | `settings.go` group `tickets` |
| 17 | tickets `ticket_client_closing_disabled` | `no tickets` / `❌` | `tickets.client_closing_disabled` | `settings.go` group `tickets` |
| 18 | tickets `ticket_mail_piping`, `ticket_mail_host/port/email/password` | `no tickets` / `❌` | `mail.piping_address`, `mail.piping_secret` (secret), `mail.imap_host`, `imap_port`, `imap_username`, `imap_password` (secret) | `settings.go` group `mail` (piping block, TKT-11); `0044_ticket_piping.sql`; `ticket_mail_logs` |
| 19 | cronjob `cronjob_order_cancel` | `none` / `❌` | `cronjob.order_cancel_days` (default 14) | `settings.go` group `cronjob` |
| 20 | cronjob `cronjob_order_suspend`, `cronjob_order_terminate` | `dunning policy` / `🟡` | `cronjob.terminate_days` (default 30) + the dunning policy tables | `settings.go` group `cronjob`; `0006_dunning.sql` |
| 21 | cronjob `cronjob_close_ticket` | `no tickets` / `❌` | `tickets.autoclose_days` (default 14) | `settings.go` group `tickets` |
| 22 | credits `credits_enabled` | `always on (ledger)` / `🟡` | `credits.enabled` (bool, default true) | `settings.go` group `credits` |
| 23 | credits `credits_minimum_deposit`, `credits_maximum_deposit`, `credits_maximum_credit` | `none` / `❌` | `credits.minimum_deposit_minor`, `credits.maximum_deposit_minor`, `credits.maximum_credit_minor` | `settings.go` group `credits` |
| 24 | credits `credits_auto_use` | `none` / `❌` | `credits.auto_use` (bool, default true) | `settings.go` group `credits` |
| 25 | theme `theme` + theme-defined keys | `one design system` / `🟡` | `branding.*` (name, logo, two accents) + a client theme choice (`dark`/`light`) held in `localStorage`, not a settings key | `settings.go` group `branding`; `web/src/lib/theme.ts` |
| 26 | invoices `bill_to_text` | `none` / `❌` | `invoices.bill_to_text` | `settings.go` group `invoices` |
| 27 | invoices `invoice_number`, `invoice_number_padding`, `invoice_number_format` | `fixed INV-/6` / `🟡` | `invoices.invoice_number_prefix` (default `INV-`), `invoices.invoice_number_padding` (default 6); there is still **no format template** — that half of the row is right | `settings.go` group `invoices`; `invoice_number_sequences` |
| 28 | invoices `invoice_proforma` | `none` / `❌` | `invoices.proforma` (bool) | `settings.go` group `invoices`; `0021_proforma.sql` |
| 29 | invoices `notes_client_visible` | `none` / `❌` | `invoices.notes_client_visible` (bool, default true) | `settings.go` group `invoices` |
| 30 | invoices `credit_note_number`, `credit_note_number_padding`, `credit_note_number_format` | `fixed CN-` / `🟡` | `invoices.credit_note_prefix` (default `CN-`), `invoices.credit_note_padding` (default 6); no format template | `settings.go` group `invoices` |
| 31 | other `default_currency` | `none` / `❌` | `currencies.is_default` with a partial unique index enforcing exactly one default | `0020_commerce.sql:68,77` |
| 32 | other `registration_disabled` | `no registration` / `❌` | `general.registration_disabled` (bool, default false) | `settings.go:164` |
| 33 | other `pagination` | `fixed page sizes` / `🟡` | `general.pagination` (int, default 25) | `settings.go:165` |
| 34 | other `debug` | `structured logs` / `🟡` | structured logs **and** `error_logs`/`http_logs` tables with a `/logs` viewer | `0020_commerce.sql:655,666`; `web/src/views/logs.ts` |

**Accurate cells (11):** security `trusted_proxies` → `BILLING_TRUSTED_PROXIES` (`✅`),
security `session_validation` → none (`❌` — no `session_validation` symbol anywhere), general
`app_url` → `BILLING_PUBLIC_URL` (`🟡`), mail `mail_header`/`mail_footer`/`mail_css` → none
(`❌` — no such key or template slot), cronjob `cronjob_time` → worker ticker (`🟡`),
cronjob `cronjob_invoice`/`cronjob_invoice_reminder` → renewal/dunning policy (`🟡`),
cronjob `cronjob_delete_email_logs` → retention policy (`✅`), credits `credits_on_downgrade` →
none (`❌`), invoices `immutable_invoices_enabled` → always (`✅ stronger`), invoices
`invoice_snapshot` → always (`✅ stronger`), other `gravatar_default` → none (`❌` — no
`gravatar` symbol).

**The "77 across 11 groups" is Paymenter's number and is unverifiable-from-tree.** What is
verifiable is the Catalyst column, and the correct figure is **72 keys in 17 declared groups**.

---

## §9 Notification event mapping

**12 cells checked, 9 stale.** (The trailing "Plus `system` email for cron failure and the
admin 'send test email' action" line is still accurate — neither exists.)

| # | Paymenter event key | OLD claim (quoted) | CORRECTED claim | Evidence |
|---|---|---|---|---|
| 1 | `invoice_paid` | `none` / `❌` | `EventInvoicePaid = "invoice_paid"` → `func InvoicePaid(ctx, tx, queue, InvoicePaidInput)` | `internal/notify/events.go` const block; `func InvoicePaid` at `events.go:470` |
| 2 | `invoice_payment_failed` | `none (dunning reminder only)` / `❌` | `EventInvoicePaymentFailed = "invoice_payment_failed"` → `func InvoicePaymentFailed` | `events.go` const block; `func InvoicePaymentFailed` at `events.go:498` |
| 3 | `new_order_created` | `none` / `❌` | `EventNewOrderCreated = "new_order_created"` → `func OrderCreated` | `events.go` const block; `func OrderCreated` at `events.go:527` |
| 4 | `new_server_created` | `none` / `❌` | `EventNewServerCreated = "new_server_created"` → `func ServerCreated` | `events.go` const block; `func ServerCreated` at `events.go:559` |
| 5 | `server_suspended` | `none` / `❌` | `EventServerSuspended = "server_suspended"` → `func ServerSuspended` | `events.go` const block; `func ServerSuspended` at `events.go:597` |
| 6 | `server_terminated` | `none` / `❌` | `EventServerTerminated = "server_terminated"` → `func ServerTerminated` | `events.go` const block; `func ServerTerminated` at `events.go:622` |
| 7 | `service_cancellation_received` | `none` / `❌` | `EventServiceCancellationReceived = "service_cancellation_received"` → `func CancellationReceived` | `events.go` const block; `func CancellationReceived` at `events.go:648` |
| 8 | `new_ticket_message` | `none` / `❌` | `EventNewTicketMessage = "new_ticket_message"` → `func TicketMessage` | `events.go` const block; `func TicketMessage` at `events.go:674` |
| 9 | `new_login_detected` | `none` / `❌` | `EventNewLoginDetected = "new_login_detected"` → `func NewLogin` | `events.go` const block; `func NewLogin` at `events.go:709` |

**Accurate cells (3).** `email_verification` → `EventEmailVerification = KindEmailVerification`
and `password_reset` → `EventPasswordReset = KindPasswordReset` are still `✅` — the two
security events reuse the existing outbox kinds (`internal/notify/notify.go:29-30`), so the
claimed symbol is right, only the name grew an `Event*` alias. `new_invoice_created` →
`KindInvoiceIssued` is accurate in substance; the function is now `func InvoiceIssued` behind
`EventNewInvoiceCreated` — a rename, not a gap.

**The whole table has been rebuilt.** `internal/notify/events.go` now declares **12 event keys**
in one const block, one Go function per event, a `Catalogue()` for the preferences screen, an
`IsKnownEvent(key)` validator, per-event dedupe keys, `notification_templates` rows to edit and
`notification_preferences` to opt out. `0023_notification_events.sql` and
`0024_notification_dedupe.sql` are the schema half. The "Plus `system` email for cron failure and
the admin 'send test email' action" sentence is the only part still true: no `"system"` event
and no send-test-email handler exist (`grep -rn 'test-email\|TestMail\|SendTest' internal/`
returns nothing).

---

## §10 Extension inventory

**5 cells checked, 4 stale.** A "cell" is one of the four subsection claims plus the
parity-scope recommendation.

| # | Subsection | OLD claim (quoted) | CORRECTED claim | Evidence |
|---|---|---|---|---|
| 1 | 10.1 In-tree gateways | `Mollie, PayPal, PayPal IPN, Stripe` (Paymenter, unverifiable) — but §1's Catalyst cell said `1 protocol, fake only` | **Catalyst has 2 in-tree gateways**: `extensions/stripe` (real — `gateway.describe`, `create_checkout`, `query_payment`, `verify_webhook`, `refund`; `stripe.go`, `webhook.go`) and `extensions/fakegateway` (fixture, v1 protocol only) | `grep -rhoE '"gateway\.[a-z_]+"' extensions/stripe/*.go` → `create_checkout describe query_payment refund verify_webhook`; `extensions/fakegateway/main.go` adds `fixture.complete_payment` |
| 2 | 10.2 In-tree servers | `Convoy, cPanel, DirectAdmin, Enhance, Plesk, Pterodactyl, Virtfusion, Virtualizor` (Paymenter, unverifiable) — but §1 said `1 protocol, fake only` | **Catalyst has 3 in-tree provisioners**: `extensions/pterodactyl` (real — Pterodactyl application API), `extensions/cpanel` (real — WHM `json-api` over HTTPS), `extensions/fakeprov` (fixture) | `extensions/pterodactyl/ptero.go`; `extensions/cpanel/whm.go`, `names.go`, `state.go`; `extensions/README.md` "Real adapters — `cpanel` is not a fixture. It is an adapter for a live WHM server" |
| 3 | 10.3 In-tree "other" extensions | `Affiliates, Announcements, DiscordNotifications` (Paymenter) / `0` (Catalyst, §1) | **Catalyst has 1 in-tree hook extension**: `extensions/example-hooks` (`ui.describe`, `events.subscribe`/`events.deliver`, `service.reboot`, protocol v2). Announcements and affiliates are **core** (tables + screens), not extensions. No Discord notifier. | `extensions/example-hooks/main.go`; `extensions/example-hooks/conformance_test.go`; `extensions/README.md` "Programming `example-hooks`"; `announcements`/`affiliates` tables and `web/src/views/announcements.ts`, `affiliates.ts` |
| 4 | 10.4 Marketplace | `256 listings: 232 extensions and 24 themes` (Paymenter, unverifiable) — and the recommendation `Match the in-tree set first (4 gateways, 8 servers)` | the recommendation's premise is wrong for Catalyst: the in-tree set is **2 gateways + 3 servers + 1 hook extension**, and `P4` has already landed the two real adapters it was supposed to prove | `ls -d extensions/*/` → `cpanel example-hooks fakegateway fakeprov pterodactyl stripe` (6 directories, 3 fixtures + 3 real/hook); `extensions/stripe/conformance_test.go`, `extensions/example-hooks/conformance_test.go` |

**Accurate cell (1).** `10.4 Marketplace`'s Paymenter listing count (`256 listings: 232
extensions and 24 themes`) is unverifiable-from-tree and is not contradicted by anything here;
only the recommendation built on it is stale.

**The stale claim that matters most is in §1, not §10:** `Payment gateways: 1 protocol, fake
only` and `Server integrations: 1 protocol, fake only` are both false. The protocol is real
(NDJSON JSON-RPC 2.0 over stdin/stdout, `internal/extproto`, version 2 with `ui.describe` and
`events.subscribe`) and there are three real adapter executables behind it. §10 itself was
written as a description of *Paymenter's* extensions and never updated to describe Catalyst's;
the missing content is a "what Catalyst ships in-tree" table, not a correction to a number.

**One internal inconsistency worth fixing while §10 is open:** `extensions/README.md` opens with
"**Four** real extension executables live here" and its table lists only `fakegateway`,
`fakeprov`, `cpanel`, `example-hooks` — `stripe` and `pterodactyl` are missing from it, and the
"Real adapters" section names only `cpanel`. That is a documentation gap in the extensions
README, not in `paymenter-parity.md`, but it is the same stale-tree problem.

---

## Recomputed tallies

The real number for every count in §1. Left column is the document's claim; right column is what
the tree says, with the command that produces it.

| §1 row | Document claims | **Real number** | Command / source |
|---|---|---|---|
| Tables | `47 tables` (`🟡 34 missing`) | **109 tables** in `internal/store/migrations/*.sql`, **110** including `schema_migrations`; **1 genuinely missing** (`notification_subscriptions`) | `grep -hoi 'CREATE TABLE[A-Z ]*\(IF NOT EXISTS \)\?[a-z_]*' internal/store/migrations/*.sql \| sed -E 's/CREATE TABLE( IF NOT EXISTS)? //I' \| sort -u \| wc -l` → 109; `internal/store/migrate.go:157` |
| Staff screens | `~24 staff screens` | **53 staff-only screens** (55 staff routes) | `routes.ts` 79 routes − 8 public − 16 customer = 55 staff routes, 53 distinct staff view modules |
| Admin pages | `1 dashboard` | **8 operator pages** (system settings, branding, updates, marketplace, automation, imports, locales, logs) + the dashboard | `web/src/routes.ts`; `web/src/ui/layout.ts` `navGroups()` |
| Dashboards | `1 exception dashboard` | **1 dashboard**, with **7 widget figures** (revenue by currency, active customers, new customers, open tickets, tickets opened, overdue invoices, paid-not-provisioned) plus the exception list | `web/src/views/dashboard.ts` `interface DashboardWidgets` |
| Portal pages | `6 portal pages` | **11 routes under `/portal`** (`/portal`, `/portal/invoices`, `/portal/invoices/:id`, `/portal/services`, `/portal/services/:id`, `/portal/profile`, `/portal/credits`, `/portal/security`, `/portal/notifications`, `/portal/payment-methods`, `/portal/affiliate`) | `grep -oE '^  route\("[^"]*"' web/src/routes.ts` |
| Customer pages beyond `/portal` | not counted | **5** (`/cart`, `/checkout`, `/tickets`, `/tickets/new`, `/tickets/:id`) | `CUSTOMER_PREFIXES` in `web/src/routes.ts` |
| Storefront pages | `2 storefront pages` | **2** (`/` index, `/products/:slug` product) + `/products` alias — *unchanged* | `web/src/routes.ts` |
| Total client-facing pages | `~26` (Paymenter side) | **23** Catalyst client-facing pages (7 distinct public/storefront views behind 8 public routes, 16 customer views behind 16 customer routes) | `web/src/routes.ts` |
| Route entries / view modules | — | **79 route entries**, **76 distinct view modules**, **79 view files** (3 unrouted: `billing-profile`, `refund`, `not-found`) | `grep -c '^  route(' web/src/routes.ts` → 79; `ls web/src/views/*.ts \| wc -l` → 79 |
| Payment gateways | `1 protocol, fake only` | **2 in-tree gateways, 1 real** (`stripe`, `fakegateway`) | `ls -d extensions/*/`; `extensions/stripe/` |
| Server integrations | `1 protocol, fake only` | **3 in-tree provisioners, 2 real** (`pterodactyl`, `cpanel`, `fakeprov`) | `ls -d extensions/*/`; `extensions/pterodactyl/`, `extensions/cpanel/` |
| Other extensions | `0` | **1** (`example-hooks`), plus a real extension host (`installed_extensions`, `/marketplace`) | `extensions/example-hooks/main.go`; `web/src/views/marketplace.ts` |
| Extension executables total | — | **6 directories**: 3 fixtures (`fakegateway`, `fakeprov`, `example-hooks`) + 3 real (`stripe`, `pterodactyl`, `cpanel`) | `ls -d extensions/*/ \| wc -l` → 6 |
| Locales | `1` | **2 seeded** (`en` enabled + default, `de` disabled), **88** catalogue keys, a `/locales` editor | `internal/store/migrations/0037_i18n.sql:128-131`; `grep -c "^    ('" …/0037_i18n.sql` → 88 |
| Themes | `1 design system, dark mode only` | **2 themes (dark + light)** on one token system, plus a brand overlay (name, logo, two accents) | `web/src/lib/theme.ts`; `web/src/styles/tokens.css`; `settings.go` group `branding` |
| Settings keys | `77 across 11 groups` (Paymenter) / `env + DB policies` (Catalyst) | **72 keys across 17 groups** in a real `settings` table, with `GET`/`PUT /api/v1/staff/settings` and a `/settings/system` screen | `grep -cE '\{Group: "[a-z0-9]+", Key:' internal/settings/settings.go` → 72; `grep -oE '\{Group: "[a-z0-9]+"' … \| sort -u \| wc -l` → 17 |
| API operations | `78 operations` across `67 paths` | **353 operations across 278 paths**, and the document is current | `grep -c 'operationId:' api/openapi.yaml` → 353; `grep -cE '^    /[^:]*:' api/openapi.yaml` → 278; `go run ./cmd/openapigen -check` → `openapigen: the document matches the router` |
| Permissions | `27 fixed permissions` | **52 permissions** | `grep -c 'Permission = "' internal/authn/authz.go` → 52 |
| Roles | `4 fixed roles` / `no role editing` | **4 system roles** (`administrator`, `billing`, `support`, `infrastructure`) **+ editable custom roles** (`POST`/`PUT`/`DELETE /staff/roles`, `/roles` screen) | `internal/authn/authz.go:173-176`; `internal/authn/roles.go`; `web/src/views/roles.ts` |
| Missing commerce tables | `34` | **1** (`notification_subscriptions`); the other 32 named tables exist or exist under another name | see §5 above |

### Recomputed §1 verdict table

Drop-in replacement for the 18-row table, with only the stale rows changed:

| | Paymenter | Catalyst | Gap |
|---|---|---|---|
| Admin resources | 27 resource classes (25 top-level + Audits + InvoiceTransactions) | 53 staff screens | 🟡 breadth, not depth |
| Admin pages | 5 | 8 operator pages + 1 dashboard | ✅ settings, updates, cron stats, extension marketplace all exist |
| Dashboard widgets | 7 | 1 dashboard, 7 figures (revenue, active customers, new customers, open tickets, tickets opened, overdue, paid-not-provisioned) + exception list | ✅ |
| Data models | 52 model files / 56 core tables | 109 tables | ✅ 1 missing table (`notification_subscriptions`) |
| Client pages | ~26 | 23 client pages (11 portal + 5 commerce/support + 2 storefront + 5 auth) | 🟡 no invoice PDF, no separate `/account` tree |
| Payment gateways | 4 in-tree | 2 in-tree (Stripe real, fakegateway fixture) | 🟡 1 real adapter vs 4 |
| Server integrations | 8 in-tree | 3 in-tree (Pterodactyl, cPanel real; fakeprov fixture) | 🟡 2 real adapters vs 8 |
| Other extensions | 3 in-tree | 1 in-tree (example-hooks) + extension host | 🟡 no Discord notifier |
| Locales | 24 | 2 seeded (1 enabled) over a full i18n system | 🟡 |
| Themes | theme packages + 24 marketplace themes | 1 design system, dark + light, brand overlay | ⛔ / ❌ |
| Settings keys | 77 across 11 groups | 72 across 17 groups, editable in the panel | ✅ |
| API operations | 10 admin REST resources | 353 operations across 278 paths, generated OpenAPI + typed client | ✅ stronger |
| Permissions | editable roles, 169 permission strings | 52 permissions, 4 system roles + editable custom roles | 🟡 no role *hierarchy*; editing exists |
| Audit | `Audits` resource | append-only, trigger-enforced | ✅ stronger |
| Money | floats in places | exact minor units, rational rates | ✅ stronger |
| Invoices | mutable by default | immutable by trigger, snapshot | ✅ stronger |
| Payments | idempotent-ish | idempotent end to end | ✅ stronger |
| Provisioning | job per action | queue, fencing, leases, reconciliation | ✅ stronger |

**Consequence for the prose under §1.** "The short version. Catalyst is a better *ledger* and a
worse *shop*. It cannot take a customer from 'I want that product' to 'I have it' without a
staff member, and it cannot take money from a real provider." — the second clause is now false
(`/register` → `/cart` → `/checkout` exists, and `extensions/stripe` takes money from a real
provider in test mode). "No real payment gateway (`PAY-01`). The only gateway is
`extensions/fakegateway`." — false. "No coupons and no jurisdiction-aware tax" — false
(`coupons`, `coupon_products`, `tax_rates`, `currencies`). Blockers 1, 2 and 3 of §1.1 should be
re-derived by whoever owns the `CT-*`/`PAY-*`/`CPN-*`/`TAX-*` rows; this audit did not touch §3.

---

## Summary of denominators

| Section | Cells checked | Stale |
|---|---|---|
| §1 Verdict | 18 | 13 |
| §5 Data-model mapping | 42 (+1 closing count) | 25 (+1) |
| §6 Admin resource mapping | 34 | 25 |
| §7 Client page mapping | 26 | 13 |
| §8 Settings key mapping | 45 rows / 77 keys | 34 |
| §9 Notification event mapping | 12 (+1 trailing note) | 9 |
| §10 Extension inventory | 5 | 4 |
| **Total** | **182** | **123** |
