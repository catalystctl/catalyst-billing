# Paymenter feature parity — the complete backlog

> **STATUS COLUMN IS HISTORICAL — READ `docs/parity-status.md` FIRST.**
>
> The ✅/🟡/❌/⛔ markers below describe the tree as it stood when this backlog was written. A
> large parity round has since landed, and the markers were not regenerated: at the time of
> writing, 249 items still read ❌ of which most are now built and browser-verified (cart,
> coupons, tax rates, currencies, product upgrades, credit deposits, ticket attachments,
> invoice PDFs, editable notification templates, role CRUD, gateways, servers, imports, locales,
> OAuth clients, updates, marketplace, affiliates, web push, email piping, the cPanel and
> Pterodactyl and Stripe adapters).
>
> Treat this file as the **feature inventory** — it is still the only place every Paymenter
> feature is enumerated with an ID and a phase — and treat `docs/parity-status.md` as the
> **authoritative status**, including its "Not done, and why" and "The management surface is
> incomplete" sections. Regenerating the markers here is deliberately not attempted by hand: a
> 249-row hand-edit would be a new source of error, and the honest record is the status file.
>
> One marker in particular was wrong in the optimistic direction and is corrected in the status
> file: i18n is marked done, and its *backend* is, but nothing in the frontend renders a
> translation.

This is the exhaustive list of what stands between Catalyst Billing and
[Paymenter](https://paymenter.org). It is written to be worked from: every feature Paymenter
has is an item, every item has a status, an ID and a phase, and the items that are decisions
rather than code are called out as decisions.

It replaces the earlier gap analysis. That version was accurate for the revision it read, but
it was a summary: it named the missing *areas* and left the features inside them implicit.
This one enumerates the features.

## 0. How to read this

**Source of truth.** Paymenter's own source, cloned at
`7aaf81e739217a187a1e16600da6f933879dad10` (2026-10-21), cross-checked against a live
Paymenter database (56 core tables, plus six `catalyst_*` bridge tables from an earlier
integration) and the public extension marketplace (`api.paymenter.org/extensions`, 256
listings). It has 25 top-level admin resource classes plus `Audits` and
`InvoiceTransactions` (27), 5 admin pages, 7 widgets and 3 clusters. File references below
are relative to that clone.

**Catalyst state.** `519c526` plus the uncommitted working tree, read directly from
`internal/`, `web/src/` and the migrations. Where the README and the code disagree, the code
wins and the row says so.

**Status legend.**

| Mark | Meaning |
|---|---|
| ✅ | Equivalent, or stronger. Nothing to do. |
| 🟡 | Partial. The gap is named in the row. |
| ❌ | Absent. |
| ⛔ | Deliberate divergence. Not a gap; recorded so nobody "fixes" it by accident. |

**ID scheme.** IDs are stable and grouped by domain: `SF` storefront, `CT` cart/checkout,
`CAT` catalog, `CPN` coupons, `TAX`, `CUR`, `CRD` credits, `INV` invoices, `PAY` payments,
`SVC` services and provisioning, `CAN` cancellations, `UPG` upgrades and config options,
`TKT` tickets, `NTF` notifications, `IDN` identity, `ADM` operator surface, `OPS` platform,
`GRW` growth extensions, `I18N` localization/theming.

**Phase.** `P0` decisions, `P1` revenue path, `P2` service lifecycle, `P3` support and
communication, `P4` real integrations, `P5` operator completeness, `P6` growth and platform.
Phase is a build order, not a priority claim: `P1` is what makes the product sellable.

---

## 1. Verdict

| | Paymenter | Catalyst | Gap |
|---|---|---|---|
| Admin resources | 27 resource classes (25 top-level + Audits + InvoiceTransactions) | ~24 staff screens | 🟡 breadth, not depth |
| Admin pages | 5 | 1 dashboard | ❌ settings, updates, cron stats, extension marketplace |
| Dashboard widgets | 7 | 1 exception dashboard | 🟡 no revenue/active-users/support widgets |
| Data models | 52 model files / 56 core tables | 47 tables | 🟡 34 missing tables (see §5) |
| Client pages | ~26 | 6 portal pages + 2 storefront pages | 🟡 no cart, tickets, credits, payment methods, security, notifications |
| Payment gateways | 4 in-tree (marketplace lists 232 extensions of all kinds) | 1 protocol, fake only | ❌ |
| Server integrations | 8 in-tree | 1 protocol, fake only | ❌ |
| Other extensions | 3 in-tree | 0 | ❌ |
| Locales | 24 | 1 | ❌ |
| Themes | theme packages + 24 marketplace themes | 1 design system, dark mode only | ⛔ / ❌ |
| Settings keys | 77 across 11 groups | env + DB policies, no system settings surface | ❌ |
| API operations | 10 admin REST resources (full CRUD) + Passport/OAuth2 | 78 operations, generated OpenAPI + typed client | ✅ stronger |
| Permissions | editable roles, 169 permission strings | 27 fixed permissions, 4 fixed roles | 🟡 no role editing |
| Audit | `Audits` resource | append-only, trigger-enforced | ✅ stronger |
| Money | floats in places | exact minor units, rational rates | ✅ stronger |
| Invoices | mutable by default | immutable by trigger, snapshot | ✅ stronger |
| Payments | idempotent-ish | idempotent end to end | ✅ stronger |
| Provisioning | job per action | queue, fencing, leases, reconciliation | ✅ stronger |

**The short version.** Catalyst is a better *ledger* and a worse *shop*. It cannot take a
customer from "I want that product" to "I have it" without a staff member, and it cannot take
money from a real provider. That is the parity problem, and most of `P1` is about it.

**At a glance.** 277 numbered items: **48 already satisfied** (13 of them stronger than
Paymenter), **73 partial**, **154 missing**, **2 deliberate divergences**. 64 items sit in
`P1`, the revenue path.

### 1.1 The five revenue blockers

These are not the whole list, but they are the ones that make everything else moot:

1. **No customer self-registration** (`IDN-01`). A storefront visitor has no way to become a
   customer. Only `billing admin create-customer` creates one.
2. **No customer purchase path** (`CT-01`…`CT-08`). `POST /api/v1/staff/orders` exists, but it
   is staff-only. The storefront "Buy" button links to `/login?next=/portal`, and the portal
   has no catalog.
3. **No real payment gateway** (`PAY-01`). The only gateway is `extensions/fakegateway`.
4. **No coupons and no jurisdiction-aware tax** (`CPN-*`, `TAX-01`). One fixed rate per
   installation is not a tax engine, and there is no promotion mechanism.
5. **No invoice PDF** (`INV-08`). A customer cannot download the document they are asked to
   pay. `web/src/views/portal-invoice.ts` says so in a comment instead of shipping a button
   that would fail.

### 1.2 Three things the audit found that are not in Paymenter's list

Found by reading the code against the UI, recorded here because they are cheap to fix and
will confuse the parity work if left implicit:

- **Customers are shown an "Account → Settings" link that bounces them.** `web/src/ui/layout.ts`
  adds the Settings item to every session, but the route guard in `web/src/app.ts` redirects
  any non-staff path outside `/portal` back to `/portal`. So a customer sees the link, clicks
  it, and lands on the portal. Either hide the item for customers or make the account
  sections customer-visible — which is exactly the `IDN-03` / `IDN-06` work.
- **The storefront's Buy button is a login link, not a checkout link.**
  `web/src/views/storefront-product.ts` computes `buyHref = /login?next=/portal` because a
  customer checkout endpoint does not exist. This is the UI half of `CT-07`; it should become
  "add to cart" once `CT-01` lands.
- **`/settings` is staff-only but the API behind it is not.** `GET /api/v1/auth/me` and the
  MFA endpoints are `AccessUser`, so a customer could use them if a page existed. The gap is
  the page and the route guard, not the server. Do not add a second auth stack for customers.

---

## 2. What Catalyst already does better (do not regress)

Recorded so parity work does not discard the parts that are ahead:

- **Exact money.** `{minor, currency, scale}`, rational tax rates, largest-remainder
  allocation that sums exactly. No floats anywhere in the money path.
- **Immutable invoice documents.** A trigger refuses to update an issued invoice; the issued
  document is a frozen JSONB snapshot with tax evidence.
- **Idempotent payment ingestion.** Replayed webhooks allocate once, keyed on event id and
  provider payment id; settled amount is verified against the recorded checkout.
- **Provisioning as a queue, not a call.** Generation fencing, leases, reaping,
  reconciliation, `unknown` as a first-class outcome.
- **Dunning with auditable decisions.** Every suspension decision recorded with reason and
  generation; manual holds outrank automation.
- **Dual approval on money movement.** Enforced by a database `CHECK`, so a bug cannot
  self-approve.
- **Append-only audit log.** Trigger-enforced, written inside the transaction that made the
  change.
- **Route table as data.** Permission coverage, problem+json shape and OpenAPI parity are
  tests, not review habits.
- **SSE event stream**, **retention policies**, **ops exception overview**, **`doctor`**,
  **generated typed client** — all ahead of Paymenter.

---

## 3. The complete feature inventory

Each table is a domain. `Paymenter` describes the behaviour in its source; `Catalyst today`
describes what exists in this repository; the status and ID are the backlog entry.

### 3.1 Public storefront

Source: `routes/web.php`, `app/Livewire/Home.php`, `Products/Index.php`, `Products/Show.php`,
`Products/Checkout.php`, `app/Models/Category.php`, `Product.php`, `Plan.php`, `Price.php`.

| ID | Paymenter feature | Catalyst today | Status | Phase |
|---|---|---|---|---|
| SF-01 | Home page lists top-level categories that have children or visible products, ordered by `sort` | web/src/views/storefront.ts renders a chip per category from GET /api/v1/catalog, where handlePublicCatalog returns every visible category (children and empty ones included) with product_count of its own products, so Paymenter's top-level-only, has-children-or-visible-products rule is not applied. | 🟡 | P1 |
| SF-02 | Nested categories (`parent_id`), category page at `/products/{category}` with child categories | categories.parent_id with the categories_guard_tree trigger (cycle refusal plus a four-level bound) and the public GET /api/v1/catalog/categories and /api/v1/catalog/category/{slug} routes exist, but storefront.ts still reads the flat /api/v1/catalog and filters client-side and web/src/routes.ts declares no /products/:category page. | 🟡 | P1 |
| SF-03 | Product page at `/products/{category}/{product}`: image, rich description, plans, config options, stock | storefront-product.ts shows the logo, the sanitised description, plans in price_versions.sort_order with interval labels and setup fees, but publicProductJSON sends no stock and no config options and the page has no option picker, so a plan with required options cannot be bought from the storefront. | 🟡 | P1 |
| SF-04 | Product `hidden` flag removes it from the storefront but keeps it billable | products.hidden is filtered in SQL by catalog.Public and catalog.PublicProduct through the hidden_branches CTE, so a hidden product or anything under a hidden category is absent from the storefront and still bills. | ✅ | — |
| SF-05 | Product image upload, served from the app | product_images (0018_catalog.sql) stores the bytes, POST /api/v1/staff/catalog/images uploads them and handleMedia serves /api/v1/media/{id} with nosniff, an immutable cache and an SVG sandbox, which is what the CSP img-src 'self' data: allows. | ✅ | — |
| SF-06 | Category image, description, sort | categories has description, sort_order, hidden and parent_id but no image column, and storefront.ts renders a text chip per category, so category imagery is still missing while product images exist. | 🟡 | P1 |
| SF-07 | Product `stock` (null = unlimited), decremented on order, restored on cancel/terminate | products.stock (NULL = unlimited) is locked FOR UPDATE in sorted order and decremented by register.checkoutTx and returned by lifecycle.CancelUnpaidOrders, but no service termination or cancellation path calls service.RestoreStock, so a terminated service never puts its unit back. | 🟡 | P2 |
| SF-08 | Product `per_user_limit` | products.per_user_limit is enforced in register.checkoutTx under the product lock and again by service.CheckPerUserLimit, and both count pending subscriptions as held units, so ten unpaid orders cannot become ten servers. | ✅ | P2 |
| SF-09 | Product `allow_quantity`: `disabled` / `separated` / `combined` | products.allow_quantity exists with disabled, separated and combined and disabled is honoured in commerce.AddItemTx and register (quantity_not_allowed), but separated and combined behave identically: quantity N yields one order item and one subscription rather than N independent services. | 🟡 | P2 |
| SF-10 | Product `sort` order within category | products.sort_order (0018_catalog.sql) orders catalog.Public by category then product and is editable per product in web/src/views/catalog-admin-product.ts. | ✅ | — |
| SF-11 | Product `email_template` snippet used in the activation email | products.email_template (bounded by 0031_product_email_template.sql) is read by internal/notify/activation.go and rendered as {{product_instructions}} in the activation mail, with an empty value falling back to the shipped sentence. | ✅ | P3 |
| SF-12 | Product assigned to a `server` integration | products.server_id, the servers table and PUT /api/v1/staff/catalog/products/{id}/server let an operator assign a node, but nothing reads products.server_id: register.provisionTarget and internal/orders both take only price_versions.provision_region, so the assignment never reaches CreateRequest.Region. | 🟡 | P4 |
| SF-13 | Plan types: `free`, `one-time`, `recurring` | 0028_plan_types.sql widens price_versions.interval to monthly, quarterly, semiannual, annual, one_time and free with a CHECK that a free price is zero, and storefront-product.ts reads the server's recurring flag so a one-time plan is not described as a subscription. | ✅ | P1 |
| SF-14 | Billing period/unit: any `N day/week/month/year` | 0059_n_day_periods.sql and internal/calendar implement every_<N>_days for N in 1..366 as a civil-day period model, but N-week and N-month units are still not expressible: a week is 7 days and months remain the four fixed calendar intervals. | 🟡 | P1 |
| SF-15 | Plan name, sort order | price_versions.name (0018_catalog.sql) and price_versions.sort_order (0028_plan_types.sql) are real columns, with the plan-order write in internal/httpapi/catalog_polish.go and the sort field in catalog-admin-product.ts, and storefront-product.ts renders plans in that order. | ✅ | P1 |
| SF-16 | Price + setup fee per currency per plan | price_versions carries amount_minor, setup_minor, currency and interval per version, prices are append-only through POST /api/v1/staff/catalog/products/{id}/prices, and an issued invoice snapshots the version it priced from. | ✅ | — |
| SF-17 | Unavailable price in the selected currency is shown as unavailable, never free | catalog.Public filters with EXISTS (SELECT 1 FROM price_versions ...) and publicProductJSON emits each price with its own currency and amount, so a currency with no price row is simply not listed rather than rendered as zero. | ✅ | — |
| SF-18 | Currency switcher (session currency; blocked while cart has items) | GET /api/v1/currencies, the selector in cart.ts and the fixed-once-non-empty rule in commerce.CartTx exist, but the storefront never reads the ?currency= parameter cart.ts sets and /api/v1/catalog has no currency filter, so the switcher does not change what the storefront offers. | 🟡 | P1 |
| SF-19 | Language switcher (allowed locales) | internal/i18n resolves from the locales table (enabled flag), GET /api/v1/i18n/locales and /api/v1/i18n/messages feed lib/i18n.ts, and storefront.ts and storefront-product.ts render languageSwitcher plus tx() at build time. | ✅ | P6 |
| SF-20 | Guest browsing; checkout requires login | web/src/routes.ts puts /, /products and /products/:slug in PUBLIC_PATHS and isStorefrontPath, while /cart and /checkout are customer prefixes and every /api/v1/portal/cart route requires a session, so browsing is anonymous and buying is not. | ✅ | P1 |
| SF-21 | TOS acceptance at checkout when configured | general.tos and checkout.tos_required in internal/settings/settings.go drive register.ReadPolicy (ErrTOSRequired to tos_required), carts.tos_accepted_at records the acceptance, and checkout.ts posts the real tos_accepted checkbox. | ✅ | P1 |
| SF-22 | Public storefront SEO/URLs per category | the public API has GET /api/v1/catalog/categories and /api/v1/catalog/category/{slug} with a slug path, but web/src/routes.ts has no category route and storefront.ts keeps one index page filtered by ?category=, so a category has no shareable or crawlable URL. | 🟡 | P1 |

**Note on SF-13/14.** This is a schema decision, not a UI one: `price_versions.interval` is a
`CHECK` list, and the billing calendar assumes month-end anchors. Supporting `one-time` and
`free` is additive (a new `interval` value plus a subscription-less service). Supporting
arbitrary `N days/weeks` is not: it needs a period model the calendar package does not have.
Recommendation: add `one_time` and `free` in `P1`; keep arbitrary periods deferred until a
customer asks.

### 3.2 Cart, checkout and orders

Source: `app/Livewire/Cart.php`, `app/Classes/Cart.php`, `app/Models/Cart.php`, `CartItem.php`,
`Order.php`, `app/Livewire/Products/Checkout.php`, `routes/web.php`.

| ID | Paymenter feature | Catalyst today | Status | Phase |
|---|---|---|---|---|
| CT-01 | Persistent cart (`carts`, `cart_items`) with currency and coupon | carts and cart_items (0020_commerce.sql) hold customer_id UNIQUE, currency, coupon_id and tos_accepted_at, and commerce.CartTx get-or-creates one cart per customer with cart_items_unique merging an identical line. | ✅ | P1 |
| CT-02 | Add to cart from product page; duplicate policy from `allow_quantity` | storefront-product.ts posts /api/v1/portal/cart/items and commerce.AddItemTx applies allow_quantity plus the merge ceiling, reporting already_in_cart, not_applicable and not_found as distinct messages on the product page. | ✅ | P1 |
| CT-03 | Cart line pricing: plan + config options + setup fee, tax applied | commerce.Store.totalsFor prices amount, setup and coupon per line and cartTotals applies the tax pricer, but loadPrice never reads config_option_prices, so an option's price is added only at checkout in register.loadConfig and the cart total can be lower than the invoice. | 🟡 | P1 |
| CT-04 | Update quantity, remove line, clear cart | cart.ts renders quantity steppers, a per-line remove and an Empty cart action backed by PUT and DELETE /api/v1/portal/cart/items/{id} and DELETE /api/v1/portal/cart in commerce_cart.go. | ✅ | P1 |
| CT-05 | Apply/remove coupon on the cart, with validation and live total | cart.ts couponCard applies and removes a code through POST and DELETE /api/v1/portal/cart/coupon, commerce.ApplyCouponTx validates window, scope, uses and currency, and Totals re-validates on every read with coupon_problem shown in the UI. | ✅ | P1 |
| CT-06 | Checkout: login required, email-verified if configured, TOS checkbox | POST /api/v1/portal/orders needs a session and PermInvoicePaySelf, register.ReadPolicy refuses without terms acceptance (tos_required) or a verified address when the installation requires it, and checkout.ts submits the real checkbox state. | ✅ | P1 |
| CT-07 | Checkout creates order + services + one invoice in one transaction; stock and per-user limits enforced under lock | register.CheckoutService.checkoutTx writes orders, order_items, subscriptions and the invoice in one transaction and locks products.stock in sorted order before the limit check, with customer_id derived from the session in checkout_customer.go. | ✅ | P1 |
| CT-08 | Free/zero-total order activates the service immediately and skips the invoice | register.checkout.go treats payable == 0 by skipping the invoice, activating the subscriptions through activateZeroTotal, creating the service and marking the order paid, and checkout.ts reports activated with an empty invoice_id. | ✅ | P1 |
| CT-09 | Order model: user, currency, services, total, invoices | orders and order_items (0001_core.sql) hold customer, currency and total, invoices.order_id and services.order_id (0029_service_metadata.sql) link both sides, and staff_read.go handleStaffOrder returns the order with items, invoices and payment attempts. | ✅ | P1 |
| CT-10 | Admin order CRUD, services relation manager | staff_read.go handleStaffOrders and handleStaffOrder plus orders.go POST /api/v1/staff/orders and the /orders/new screen give create, list and detail only, with no edit, delete or cancel endpoint, and docs/parity-status.md records order editing as a deliberate divergence. | 🟡 | P5 |
| CT-11 | Unpaid order cancelled after N days; pending invoices cancelled; stock restored | lifecycle.CancelUnpaidOrders reads cronjob.order_cancel_days, flips pending_payment under FOR UPDATE, voids open invoices through the void path, reverses the receivable, calls service.RestoreStock and cancels pending subscriptions, wired as a kill-switchable cron job. | ✅ | P2 |
| CT-12 | "Cancel subscription ID" action on an order/service (legacy external subscription) | no cancel-subscription-id action exists: nothing in internal/httpapi or web/src matches cancel_subscription or cancelSubscription, web/src/views/service.ts only prints the internal subscription id with a copy button, and the only cancellation paths are the service-cancellation queue and POST /api/v1/portal/services/{id}/cancel, neither of which calls a gateway. | ❌ | P5 |
| CT-13 | Cart/order events for extensions (`Cart/Created`, `Order/Finalized`, …) | extproto/events.go declares v2 events.subscribe and events.deliver and httpapi/extension_hooks.go forwards the in-process broker to extensions, but only order.created and the ticket kinds are ever published, so cart.created, cart.updated, cart.abandoned, cart.checked_out, order.paid and order.cancelled are declared and never emitted. | 🟡 | P4 |

**Important distinction.** `internal/orders` already prices from the catalog, creates a
subscription, issues the invoice and posts the ledger in one transaction. The gap is not the
pricing engine; it is that only staff can reach it, and there is no cart. `CT-07` is therefore
mostly an authorization and UI change plus a customer-scoped endpoint, not a rewrite. The
customer endpoint must derive `customer_id` from the session, never from the body. The staff
path is already good: `/orders/new` is a real screen with a price-version picker and a
customer picker, not a JSON form.

### 3.3 Catalog and pricing

Source: `app/Admin/Resources/ProductResource.php`, `CategoryResource.php`, migrations for
`categories`, `products`, `plans`, `prices`.

| ID | Paymenter feature | Catalyst today | Status | Phase |
|---|---|---|---|---|
| CAT-01 | Category CRUD (name, slug, description, image, parent, sort) | internal/catalog.CreateCategory/UpdateCategory with the 0049 parent_id tree give name, slug, description, sort, hidden and nesting, but categories has no image column or upload path. | 🟡 | P1 |
| CAT-02 | Product CRUD (name, slug, category, description, image, hidden, sort) | ProductInput and CreateProductTx/UpdateProductTx in internal/catalog/catalog.go carry name, slug, category, short and long description, logo, hidden and sort, served by /api/v1/staff/catalog/products and the catalog-admin.ts dialog. | ✅ | — |
| CAT-03 | Plan/price editor (repeater: period, unit, type, price, setup, currency) | catalog.ValidateInterval and handleAddPrice accept one_time and free and the dialog edits amount, setup, interval, anchor and provision plan/region, but the INTERVALS picker in web/src/views/catalog-admin.ts and INTERVAL_OPTIONS in catalog-admin-product.ts omit one_time and free. | 🟡 | P1 |
| CAT-04 | Duplicate product action | catalog.DuplicateProductTx in internal/catalog/plans.go and POST /api/v1/staff/catalog/products/{id}/duplicate copy the product, its price versions and config options, wired to the Duplicate button in web/src/views/catalog-admin-product.ts. | ✅ | P5 |
| CAT-05 | Product "refresh" action (reload plans/prices) | Deliberate divergence: there is no inline price repeater to reload, because internal/catalog appends immutable price_versions and the panel re-reads plans from /api/v1/staff/catalog/products. | ⛔ | — |
| CAT-06 | Rich text product description | catalog.Description plus the allowlist in internal/catalog/sanitize.go store an html-or-plain description_format that the rich editor in catalog-admin-product.ts writes and the storefront renders. | ✅ | P5 |
| CAT-07 | Product image upload + storage | catalog.PutImageTx stores digest-deduped bytes in product_images and /api/v1/media/{id} serves them with their own content type, keeping the panel CSP img-src 'self' data:. | ✅ | — |
| CAT-08 | Product-level `server_id` and `email_template` | products.server_id is fully wired through catalog.SetProductServerTx, productPlacementWith and the placement control, while products.email_template exists in catalog.Product and planJSON but has no editor and no activation-mail consumer. | 🟡 | P4 |
| CAT-09 | Product→product upgrade graph (`product_upgrades`) | internal/httpapi/upgrades_ext.go implements product_upgrades create/list/delete and reachable-product reads, used by service_upgrades.go and the web/src/views/upgrades.ts screen. | ✅ | P2 |
| CAT-10 | Plan availability by currency (no price = unavailable) | catalog.Public and PublicProduct require a price_versions row so an unpriced product is not shown, and commerce.AddItemTx refuses a price whose currency differs from the cart's. | ✅ | — |
| CAT-11 | Prices are mutable in admin (history via audits) | internal/catalog has no update-price endpoint: AddPriceTx appends a version and ReorderPlansTx writes only price_versions.sort_order, while an issued invoice snapshots the version it priced from. | ✅ | — |

**Deliberate divergence (`CAT-03`, `CAT-11`).** Paymenter edits a price row in place. Catalyst
appends a version because an issued invoice records the version it priced from. The admin
screen must present "new version", never "edit price"; the API already enforces this. Do not
add an edit endpoint to match Paymenter.

### 3.4 Coupons and promotions

Source: `app/Models/Coupon.php`, `CouponProduct.php`, `app/Admin/Resources/CouponResource.php`,
`database/migrations/*coupons*`.

| ID | Paymenter feature | Catalyst today | Status | Phase |
|---|---|---|---|---|
| CPN-01 | Coupon code, percentage or fixed value | commerce.Coupon in internal/commerce/coupons.go supports kind percentage as an exact rational (money.ParseRatio) and kind fixed as AmountMinor plus Currency, matching the coupons CHECK in 0020_commerce.sql. | ✅ | P1 |
| CPN-02 | `applies_to`: price, setup fee, or all | AppliesTo price, setup or all is stored on coupons and enforced by applyCoupon in internal/commerce/coupons.go and couponBase in internal/commerce/cart.go. | ✅ | P1 |
| CPN-03 | `max_uses` globally | coupons.max_uses is enforced by counting coupon_redemptions, and register.resolveCoupon re-counts under SELECT FOR UPDATE on the coupon row. | ✅ | P1 |
| CPN-04 | `max_uses_per_user` | coupons.max_uses_per_user is counted per customer_id in commerce.validateCouponWith and re-checked by the locked query in internal/register/checkout.go with the ErrCouponUsed code. | ✅ | P1 |
| CPN-05 | `starts_at` / `expires_at` window | coupons.starts_at and expires_at are validated in normaliseCoupon and refused at cart and checkout as ErrExpired, ErrCouponNotStarted and ErrCouponExpired. | ✅ | P1 |
| CPN-06 | `recurring`: apply for the first N cycles, or lifetime (`null`/1) | coupons.recurring_cycles is stored and checkout seeds subscriptions.coupon_cycles_applied, but internal/renewal/renewal.go reads only price_versions so a first-N-cycles coupon is never applied to a renewal invoice. | 🟡 | P1 |
| CPN-07 | Product restrictions (`coupon_products`) | coupon_products restricts a coupon to a product set, enforced in commerce.validateCouponWith, ApplyCouponTx and register.resolveCoupon with the ErrCouponNotForYou code. | ✅ | P1 |
| CPN-08 | Coupon stored on the service so renewals recompute the right price | checkout writes subscriptions.coupon_id and coupon_cycles_applied in internal/register/checkout.go, yet the renewal engine never reads either column and re-prices the undiscounted price_version amount. | 🟡 | P1 |
| CPN-09 | Coupon validation at cart and revalidation under lock at checkout | commerce.ApplyCouponTx validates at cart time and register.resolveCoupon revalidates inside the checkout transaction under SELECT FOR UPDATE before insertRedemption. | ✅ | P1 |
| CPN-10 | Admin coupon CRUD + services relation manager | registerCouponRoutes in internal/httpapi/commerce_coupon.go serves list, get, create, update and /redemptions with no delete, mirrored by the retire-only screen web/src/views/coupons.ts. | ✅ | P5 |

**Design warning.** A coupon is a *price modifier with a life*, not a discount column. The
renewal engine currently reads `price_versions`; a coupon that applies for N cycles needs the
subscription to carry both the coupon and a cycle counter, and the invoice snapshot must record
the discount as a line (or a negative line) so the document remains self-explaining. Decide
this before writing the migration (`P0-03`).

### 3.5 Tax

Source: `app/Models/TaxRate.php`, `app/Classes/Settings::tax()`, `app/Classes/Price.php`,
`app/Admin/Resources/TaxRateResource.php`, `database/migrations/*tax_rates*`.

| ID | Paymenter feature | Catalyst today | Status | Phase |
|---|---|---|---|---|
| TAX-01 | Tax rates table: name, rate, country (unique), wildcard `all` | commerce.TaxRate and the tax_rates table in 0020_commerce.sql give a dated row per country_code (wildcard all), region_code and tax_class, guarded by the tax_rates_one_open unique index. | ✅ | P1 |
| TAX-02 | Country selection from the customer's custom property `country` | taxPricerFor in internal/httpapi/commerce_tax.go feeds billing_profiles.country_code and region into commerce.TaxPricer for cart totals, but the server issuer in internal/httpapi/server.go is still billing.FixedTaxPricer{Policy: no-tax} so issued invoices resolve no rate. | 🟡 | P1 |
| TAX-03 | Inclusive vs exclusive tax mode | Inclusive is a per-rate column plus the tax.default_mode setting, both honoured by billing.FixedTaxPricer and commerce.TaxPricer.Price. | ✅ | — |
| TAX-04 | Tax calculated per line, per currency, recorded as evidence | billing.Pricer returns per-line net and tax stored in invoice_lines.tax_rate, and billing.TaxEvidence records mode, rate and jurisdiction in the invoice snapshot. | ✅ | — |
| TAX-05 | EU VAT / jurisdiction rules | There is no jurisdiction rules engine: internal/tax imports a rate table and internal/commerce/tax.go does a (country, region, class) lookup, with no EU place-of-supply or reverse-charge rule and no US nexus determination anywhere. | ❌ | P1 |
| TAX-06 | Tax exemption (reverse charge, valid VAT ID) | billing_profiles.tax_exempt and tax_exemption_reason are staff-set and short-circuit commerce.TaxPricer.Price, but no rule derives the exemption because there is no VAT-ID validation or reverse-charge decision. | 🟡 | P1 |
| TAX-07 | Admin tax-rate CRUD | registerTaxRoutes in internal/httpapi/commerce_tax.go serves /api/v1/staff/tax-rates CRUD and internal/httpapi/tax_import.go adds source, preview, import, runs and provenance plus the tax-rate-refresh job, edited in web/src/views/tax-rates.ts. | ✅ | P5 |
| TAX-08 | Tax included in product/cart display ("incl. tax") | web/src/views/cart.ts renders an Includes rate tax line and a tax total from cartTotals, but web/src/views/storefront.ts and storefront-product.ts show net prices only. | 🟡 | P1 |

**Design note.** The `Pricer` interface already accepts an exact rate and records evidence. A
tax-rate table is a `Pricer` implementation that resolves a rate from
`(country_code, product_class, date)` and returns the same evidence shape. The ledger posting
(`tax_payable`) does not change. This is a contained change — but the jurisdiction *policy*
(where the customer is, what "country" means for a digital service, whether VAT IDs are
validated) is a decision, not code (`P0-04`).

### 3.6 Currencies

Source: `app/Models/Currency.php`, `app/Admin/Resources/CurrencyResource.php`,
`app/Livewire/Components/CurrencySwitch.php`, settings `default_currency`.

| ID | Paymenter feature | Catalyst today | Status | Phase |
|---|---|---|---|---|
| CUR-01 | Currency entity: code, name, prefix, suffix, decimal format (`1.000,00`, `1,000.00`, …) | commerce.Currency and the currencies table in 0020_commerce.sql carry code, name, scale, symbol, symbol_position, group_separator, decimal_mark and group_size, edited in web/src/views/currencies.ts. | ✅ | P1 |
| CUR-02 | Default currency setting | currencies.is_default with its partial unique index and commerce.Store.DefaultCurrency supply the default, which commerce.CartTx falls back to when a cart names no currency. | ✅ | P1 |
| CUR-03 | Storefront currency switcher, cart locked to one currency | web/src/views/cart.ts has a currency selector that locks once the cart holds an item and commerce.AddItemTx adopts the first price's currency, but the storefront has no switcher and ignores the ?currency= the selector redirects to. | 🟡 | P1 |
| CUR-04 | Per-plan price row per currency | price_versions.currency plus catalog.AddPriceTx give each plan its own currency row, and no conversion exists between rows. | ✅ | — |
| CUR-05 | No FX conversion (explicit prices only) | internal/commerce/currencies.go stores presentation only, money.Amount refuses cross-currency arithmetic, and sumAmounts in web/src/lib/format.ts returns null for mixed currencies. | ✅ | — |
| CUR-06 | Admin currency CRUD | registerCurrencyRoutes in internal/httpapi/commerce_currency.go serves /api/v1/staff/currencies list, create, update and delete, and DeleteCurrencyTx refuses a currency with price or invoice history. | ✅ | P5 |
| CUR-07 | Currency-aware formatting (prefix/suffix/grouping) | commerce.Currency.Format and formatCurrency in web/src/lib/format.ts insert the symbol prefix or suffix and the row's grouping and decimal marks, keeping integer minor-unit arithmetic. | ✅ | P1 |

**Deliberate divergence.** Paymenter formats money with floats and `number_format`. Catalyst
must keep integer minor units and render by string insertion. A currency table can supply
symbol/prefix/suffix/grouping, but the arithmetic stays exact.

### 3.7 Customer credit

Source: `app/Models/Credit.php`, `app/Livewire/Client/Credits.php`,
`app/Livewire/Invoices/Show.php`, `CronJob::payInvoiceWithCredits`, settings `credits_*`.

| ID | Paymenter feature | Catalyst today | Status | Phase |
|---|---|---|---|---|
| CRD-01 | Per-customer, per-currency credit balance | internal/credit/credit.go BalanceFor sums journal_entries on ledger.AccountCredit per currency, served to the customer by handlePortalCredits in internal/httpapi/credits.go. | ✅ | — |
| CRD-02 | Customer tops up credit through a gateway (min/max deposit) | credit.Service.StartDeposit in internal/credit/deposit.go enforces credits.minimum_deposit_minor and maximum_deposit_minor, creates a snapshot-marked deposit invoice and opens a gateway session via POST /api/v1/portal/credits/deposit. | ✅ | P1 |
| CRD-03 | Auto-use credit on a new invoice if it covers the total | credit.ApplyCreditToInvoice is called only from internal/register/checkout.go, so auto-use does not run on renewal (internal/renewal/renewal.go), upgrade (internal/service/upgrades.go) or manual (internal/httpapi/staff_write.go) issuance. | 🟡 | P1 |
| CRD-04 | Partial credit payment then pay the remainder | credit.ApplyCredit in internal/credit/apply.go spends min(owed, balance) and leaves the invoice open, and checkout.Service.Start prices the remainder as total_minor minus amount_paid_minor. | ✅ | P1 |
| CRD-05 | Max credit balance | internal/credit/deposit.go refuses a top-up over credits.maximum_credit_minor counting open deposit invoices as reservation, and SettleDeposit reports OverLimit when a settlement overshoots. | ✅ | P1 |
| CRD-06 | Credit returned on downgrade (prorated) | internal/service/upgrades.go records a prorated CreditMinor with CreditPending on a service_upgrades row and Store.ApplyDowngrade is the credit hook, but nothing posts that credit to customer_credit. | 🟡 | P2 |
| CRD-07 | Admin adds credit to a user | credit.Grant issues a numbered credit note (internal/credit/grant.go) and credit.Debit (internal/credit/debit.go) appends a credit_debit entry with a mandatory reason, behind POST /api/v1/staff/customers/{id}/credit and /credit/debit. | ✅ | P5 |
| CRD-08 | Client credits page | web/src/views/portal-credits.ts is routed at /portal/credits in web/src/routes.ts and shows derived balances, GET /api/v1/portal/credits/transactions history and a deposit form. | ✅ | P1 |
| CRD-09 | Credit transactions marked as such (`is_credit_transaction`) | internal/credit/TransactionsFor joins journal_transactions.kind so entries read as credit_debit, adjustment or payment_received, with ledger.RefCreditApplication, RefCreditDeposit and RefCreditGrant distinguishing the events. | ✅ | — |
| CRD-10 | Settings: enabled, min/max deposit, max credit, auto-use, credit-on-downgrade | internal/settings/settings.go declares credits.enabled, minimum_deposit_minor, maximum_deposit_minor, maximum_credit_minor and auto_use, but there is no credit-on-downgrade key. | 🟡 | P1 |

**Design note.** Credit is already a subledger account, so `CRD-02`/`CRD-03` are a payment
intent (gateway) plus a posting rule, not a new balance column. `CRD-03` must be keyed by the
invoice id so a replay does not spend twice.

### 3.8 Invoices

Source: `app/Models/Invoice.php`, `InvoiceItem.php`, `InvoiceTransaction.php`,
`AdjustmentNote.php`, `app/Admin/Resources/InvoiceResource.php`,
`themes/default/views/pdf/invoice.blade.php`, settings `invoices.*`.

| ID | Paymenter feature | Catalyst today | Status | Phase |
|---|---|---|---|---|
| INV-01 | Draft → pending → paid / cancelled lifecycle | invoices_status_check plus the transition table rebuilt in internal/store/migrations/0021_proforma.sql allow draft to open to paid, void, uncollectible, refunded and disputed. | ✅ | P1 |
| INV-02 | Invoice items: description, price, quantity, reference to service/upgrade | invoice_lines in 0001_core.sql carries position, description, quantity, unit, net, tax and total, and 0019_invoice_line_product.sql adds product_id and price_version_id for the service reference. | ✅ | — |
| INV-03 | Invoice numbering: next number, padding, format with `{number}/{year}/{month}/{day}` | invoicepdf.LoadNumbering and seriesPrefix support a configured prefix, padding and {year}/{month}/{day} tokens, but issuance sites still pass billing.DefaultSeries so only proforma conversion honours them. | 🟡 | P1 |
| INV-04 | Proforma invoices: no official number until paid | invoicepdf.IssueProforma and ConvertProforma with 0021_proforma.sql convert once, called from internal/fulfilment/applier.go inside the settling transaction so a paid document cannot stay provisional. | ✅ | P1 |
| INV-05 | Optional immutable invoices (editable while draft) | guard_issued_invoice in internal/store/migrations/0021_proforma.sql freezes number, currency, money, snapshot, customer, order and period once issued, but client_note, void_reason, due_at and amount_paid_minor remain writable. | 🟡 | — |
| INV-06 | Invoice snapshot of customer details on payment | billing.Issuer.CreateDraft writes billing.Snapshot with merchant, customer, prices, totals and TaxEvidence, frozen by the 0021 trigger and reprinted by invoicepdf.FromInvoice. | ✅ | — |
| INV-07 | `bill_to_text`, notes visible to client | invoices.client_note is written by handleSetClientNote and billing_profiles.bill_to_text is read in internal/httpapi/invoice_pdf.go, both printed via invoicepdf.Meta under invoices.notes_client_visible. | ✅ | P1 |
| INV-08 | PDF invoice (client + admin download) | handlePortalInvoicePDF and handleStaffInvoicePDF in internal/httpapi/invoice_pdf.go serve invoicepdf.Render bytes to the customer and to staff, with the portal link in web/src/views/portal-invoice.ts. | ✅ | P1 |
| INV-09 | Cancel invoice with reason | handleVoidWithReason in internal/httpapi/invoice_documents.go stores invoices.void_reason in the same UPDATE as the status, and invoicepdf prints it beside the void banner. | ✅ | P5 |
| INV-10 | Credit/debit adjustment notes with number series and `active/voided` status | credit_notes (0012_money_actions.sql) are issued with CN- numbers by creditnote.Issue, but there are no debit notes and creditnote.StatusVoid is never written by any code path. | 🟡 | P1 |
| INV-11 | Show adjustment notes to clients (setting) | handlePortalInvoiceDocument and portalInvoiceCreditNotes in internal/httpapi/invoice_documents.go list every credit note to its customer with a working portal credit-note PDF route, unconditionally. | ✅ | P1 |
| INV-12 | Invoice `remaining` / `currentBalance` | internal/httpapi/staff_read.go reports due as total_minor minus amount_paid_minor and internal/httpapi/export.go emits balance_due_minor, both derived from payments and their allocations. | ✅ | — |
| INV-13 | Invoice list filters (status, user, date) | handleStaffInvoices in internal/httpapi/staff_read.go filters status, customer_id, currency, from, to and overdue, and web/src/views/invoices.ts filters nothing in the browser. | ✅ | — |
| INV-14 | Admin "publish" (draft→pending), "send email" toggle | POST /api/v1/staff/invoices/{id}/issue publishes a draft and always queues the customer mail through queueInvoiceNotification in internal/httpapi/staff_write.go, so there is no per-issue send toggle. | 🟡 | P1 |
| INV-15 | Invoice PDF template per theme | internal/invoicepdf/document.go renders one fixed layout with an invoicepdf.Brand overlay (accent, logo, name) sourced from internal/brand, and there is no per-theme PDF template. | 🟡 | P6 |
| INV-16 | Invoice reminders (X days before due, X days overdue) | internal/dunning/dunning.go only reminds after the due date using DefaultPolicy (48h interval, three steps) with no pre-due reminder and no settings key for the schedule. | 🟡 | P1 |
| INV-17 | Invoice due date default (7 days) | invoices.due_days defaults to 7 and is read by the proforma handler in internal/httpapi/invoice_documents.go, but manual invoice create (internal/httpapi/staff_write.go) and checkout (internal/register/checkout.go) hardcode 30 days. | 🟡 | P1 |
| INV-18 | Credit/debit note numbering settings | invoices.credit_note_prefix and credit_note_padding are declared in internal/settings/settings.go and parsed by invoicepdf.LoadNumbering, yet creditnote.CreditSeries and credit.CreditSeries stay hardcoded to CN- and width 6. | 🟡 | P5 |

**Design note (`INV-03`, `INV-04`).** Numbering is already a persisted sequence with a
gap-free guarantee. Making the format configurable is a settings read at issue time; making
proforma work means "issue without allocating a number", which the current `CHECK` ties to
`status <> 'draft'`. That constraint must change to allow a `proforma` status, or proforma must
be modelled as a separate document. Decide before coding.

### 3.9 Payments, gateways and billing agreements

Source: `app/Classes/Extension/Gateway.php`, `app/Helpers/ExtensionHelper.php`,
`extensions/Gateways/*`, `app/Models/BillingAgreement.php`,
`app/Livewire/Client/PaymentMethods.php`, `app/Livewire/Invoices/Show.php`.

| ID | Paymenter feature | Catalyst today | Status | Phase |
|---|---|---|---|---|
| PAY-01 | Real gateway: Stripe | extensions/stripe is a real adapter: createCheckout builds a Checkout Session with amount, currency and an Idempotency-Key, queryPayment and refund resolve a session id to its PaymentIntent, webhook.go verifies the t=,v1= signature over timestamp and raw body, and conformance_test.go drives the whole thing through extproto.Launch. | ✅ | P4 |
| PAY-02 | Real gateway: PayPal | No PayPal adapter exists: gatewayProviders in internal/gatewayconfig/providers.go lists only stripe and fake, and extensions/ contains no PayPal process. | ❌ | P4 |
| PAY-03 | Real gateway: PayPal IPN (legacy) | No legacy PayPal IPN listener: the protocol's only webhook entry is gateway.verify_webhook in internal/extproto/spec.go and no adapter exists to parse an IPN payment_status delivery or validate its POST-back. | ❌ | P4 |
| PAY-04 | Real gateway: Mollie | No Mollie adapter: the two entries in gatewayProviders are Stripe and the fixture, and nothing under extensions/ speaks the Mollie API. | ❌ | P4 |
| PAY-05 | Marketplace gateways (Razorpay, Coinbase, BTCPay, bank transfer, 200+) | internal/marketplace can install an extension whose Kind is KindGateway after a SHA-256 check, but gatewayProviders in internal/gatewayconfig/providers.go is a closed two-entry list and no Razorpay, Coinbase or BTCPay adapter exists. | ❌ | P6 |
| PAY-06 | Gateway CRUD: name + extension + config fields | gatewayconfig.Resolver.Save stores one provider plus its credential Fields in the settings table and web/src/views/gateways.ts renders and edits them, but there are no gateway records: one gateway per installation and no create, delete, order or naming. | 🟡 | P4 |
| PAY-07 | Gateway test/connection check | GET /api/v1/staff/gateways/test and /api/v1/staff/servers/test (handleTestGateway, handleTestServer) launch the saved configuration through gatewayconfig.Launch and report a failed describe as ok:false with the reason. | ✅ | P4 |
| PAY-08 | Checkout session with amount/currency and redirect | POST /api/v1/portal/invoices/{id}/checkout (handlePortalCheckout in internal/httpapi/checkout.go) records a payment_attempts row under an operation key and returns the adapter redirect_url, and extensions/stripe createCheckout sends the amount, currency and idempotency key. | ✅ | — |
| PAY-09 | Webhook verification + authoritative re-query | payments.ExtensionVerifier delegates to gateway.verify_webhook, payments.Inbox.Receive stores the normalised payment_id beside the raw payload (0058_gateway_event_payment_id.sql) and Processor.processOne re-queries the provider with query_payment before applying anything. | ✅ | — |
| PAY-10 | Saved payment methods / billing agreements (create, list, remove, default for recurring) | Migration 0036_payment_methods.sql plus payments.Agreements Save, List, Get, SetDefault and Remove give a saved token with one database-enforced default, but no HTTP route or adapter token issuance is wired and internal/payments has no test for it. | 🟡 | P4 |
| PAY-11 | Off-session charging of recurring invoices | payments.Agreements.ChargeInvoice and ChargeDue implement the gateway.off_session_enabled guard, payment_attempts reuse and settlement through Applier.ApplySettledPayment, but nothing implements OffSessionCharger and nothing calls ChargeDue, so no renewal is ever charged. | 🟡 | P4 |
| PAY-12 | Refund through the gateway, partial refunds, `refunded_amount` | refunds.Service.Create writes the intent before calling Gateway.Refund, accepts a partial amount, leaves a timeout as unknown and Reconcile re-asks the provider, while billing derives payments.status partially_refunded or refunded from the refund rows. | ✅ | — |
| PAY-13 | Transaction fee recorded per payment | payments.fee_minor exists (0020_commerce.sql) and handleStaffRecordPayment stores a fee_minor from the operator, but no gateway or webhook path records the provider's own fee. | 🟡 | P5 |
| PAY-14 | Manual payment recording (admin creates a transaction) | handleStaffRecordPayment and handleRecordManualPayment with payments.RecordManual record money that arrived outside a gateway: provider manual, closed-set method and note from 0057_offline_payment_method.sql, a mandatory reference and the ordinary settlement applier. | ✅ | P5 |
| PAY-15 | Transaction status `processing/succeeded/failed` | payments.status covers succeeded, refunded, partially_refunded and disputed, and payment_attempts.status covers created, pending, requires_action, succeeded, failed and cancelled in 0001_core.sql. | ✅ | — |
| PAY-16 | Multiple transactions per invoice (partial payments) | payment_allocations is unique on (payment_id, invoice_id) and billing.Allocate accumulates invoices.amount_paid_minor without forcing paid, so two payments against one invoice are ordinary rows. | ✅ | — |
| PAY-17 | Pay with credit on the invoice page | POST /api/v1/portal/invoices/{id}/pay-with-credit (handlePortalPayWithCredit) spends the customer's balance, and web/src/views/portal-invoice.ts offers it beside the gateway button with the remainder shown. | ✅ | P1 |
| PAY-18 | Payment method icons/branding in the pay modal | handlePortalPaymentMethods returns each configured gateway as id and name taken from gateway.describe Methods and /portal/payment-methods lists them, but there are no per-method icons and payment is still a hosted redirect with no pay modal. | 🟡 | P1 |
| PAY-19 | Payment polling ("check payment status") | GET /api/v1/portal/invoices/{id}/payment-status (handlePortalPaymentStatus) reports the local attempt status and derives settled from the allocation, but no web/src view calls it, so nothing in the UI polls. | 🟡 | P1 |

**Protocol gap (`PAY-10`, `PAY-11`).** The v1 extension protocol has `create_checkout`,
`query_payment`, `refund`, `verify_webhook`. A billing agreement needs
`create_billing_agreement`, `charge`, `cancel_billing_agreement` and a reusable method token.
Adding those is a protocol version bump and a conformance fixture; it is not a gateway adapter
detail.

### 3.10 Services and provisioning

Source: `app/Models/Service.php`, `app/Classes/Extension/Server.php`,
`app/Helpers/ExtensionHelper.php`, `extensions/Servers/*`, `app/Jobs/Server/*`,
`app/Livewire/Services/Show.php`, `app/Admin/Resources/ServiceResource.php`.

| ID | Paymenter feature | Catalyst today | Status | Phase |
|---|---|---|---|---|
| SVC-01 | Service lifecycle: pending → active → suspended → cancelled | services.status carries pending, provisioning, active, suspended, terminating, terminated and error with a services.generation fencing token, and subscriptions.status adds past_due and ended, which is stricter than the four upstream states. | ✅ | — |
| SVC-02 | Service belongs to order, product, plan, customer, currency, quantity, price | services carries customer_id, product_id, order_id and quantity (0029_service_metadata.sql) and plan, price and currency come from subscriptions.price_version_id. | ✅ | P2 |
| SVC-03 | Service label (customer-editable) | services.label is bounded and single-line and is editable by the customer through PUT /api/v1/portal/services/{id}/metadata and by staff through PUT /api/v1/staff/services/{id}/metadata. | ✅ | P2 |
| SVC-04 | Service description derived from product + config options | service.deriveDescription in internal/service/metadata.go builds the description from product, plan, interval and the chosen config options, and the customer's own text wins as effective_description. | ✅ | P2 |
| SVC-05 | Service expiry, next due date, price recalculation | internal/renewal issues from subscriptions.next_billing_at on an internal/calendar anchor of day-of-month plus wall clock with clamping, and renewal re-reads the subscription price_version_id on every period. | ✅ | — |
| SVC-06 | Service `suspend_hold_until` (do not suspend before) | service_holds (0006_dunning.sql) with an expiry is read by handleSuspensionEligibility, the dunning and termination automation and handleListHolds, so a hold really stops a suspend. | ✅ | — |
| SVC-07 | Service config options (`service_configs`) | service_configs (0020_commerce.sql) stores the chosen options priced per price version and internal/httpapi/service_config.go validates, prices and reprices a change. | ✅ | P2 |
| SVC-08 | Service custom properties | services.custom_properties jsonb plus service_property_definitions with show_on_invoice and customer_visible in 0029_service_metadata.sql, and a trigger refuses a value whose key has no definition. | ✅ | P2 |
| SVC-09 | Service cancellation record | service_cancellations (0020_commerce.sql) records type, reason and end date, with POST /api/v1/staff/services/{id}/cancellation, the portal request path and the /cancellations queue. | ✅ | P2 |
| SVC-10 | Service upgrade record | service_upgrades (0020_commerce.sql) plus internal/service/upgrades.go and POST /api/v1/staff/services/{id}/upgrade keep a half-applied change as a pending row instead of losing it. | ✅ | P2 |
| SVC-11 | Server actions exposed to the client (buttons/views/text from the extension) | extproto.ServiceAction and ui.describe with internal/httpapi/extension_hooks.go let an extension declare a service action that the core then calls with the declared permission checked, but no web/src view requests GET /api/v1/extensions/hooks, so no button is ever rendered. | 🟡 | P4 |
| SVC-12 | Admin service CRUD (create/edit/delete, status change, send notification, delete remote) | POST /api/v1/staff/services (handleCreateService) creates, PUT /api/v1/staff/services/{id}/metadata edits and suspend, resume, terminate and cancellation change status, but there is no delete, no send-notification and no delete-remote action. | 🟡 | P5 |
| SVC-13 | Admin "recalculate price" action | Nothing recomputes a service price from the catalogue: the only price change path is internal/service/upgrades.go, and 0029_service_metadata.sql says the plan and price come from the subscription. | ❌ | P5 |
| SVC-14 | Admin "cancel subscription ID" action | internal/service ends the local subscriptions row on a cancellation, but no admin action and no extproto method cancels a provider-side subscription or billing agreement id. | ❌ | P5 |
| SVC-15 | Admin service invoices relation | staff.ServiceDetail.Invoices is returned by handleStaffService and web/src/views/service.ts renders an Invoices tab on the service screen. | ✅ | P5 |
| SVC-16 | Provisioning: create/suspend/unsuspend/terminate/upgrade | provisioning.Worker.execute dispatches create, inspect, suspend, resume and terminate to the extproto provisioner methods and Reconcile uses provisioner.lookup, but there is no upgrade or replace call in the protocol or the worker. | 🟡 | P4 |
| SVC-17 | Provisioning queue with retries and failure visibility | service_operations is a durable queue with a unique operation_key, generations, leases, Worker.Reap for expired leases and Worker.Reconcile for unknown outcomes. | ✅ | — |
| SVC-18 | Server integrations: Pterodactyl, cPanel, DirectAdmin, Plesk, Virtualizor, Virtfusion, Convoy, Enhance | extensions/pterodactyl and extensions/cpanel are two real adapters against different APIs, but gatewayconfig.serverProviders registers only Pterodactyl plus the fixture so cPanel cannot be selected, and DirectAdmin, Plesk, Virtualizor, Virtfusion, Convoy and Enhance are absent. | 🟡 | P4 |
| SVC-19 | Server config fields + test connection | gatewayconfig.Field with GET /api/v1/staff/servers/test gives credential fields and a connection test, but serverProviders in internal/gatewayconfig/providers.go lists only pterodactyl and fake, so the CPANEL_WHM_URL and CPANEL_API_TOKEN values have no form and never reach the child environment. | 🟡 | P4 |
| SVC-20 | Product→server assignment | products.server_id from 0049_catalog_structure.sql with PUT /api/v1/staff/catalog/products/{id}/server and the servers CRUD routes in internal/httpapi/catalog_nesting.go. | ✅ | P4 |
| SVC-21 | Server "config options → environment variables" mapping | extensions/pterodactyl environmentFor passes env.-prefixed metadata to the egg, but provisioning.Operation.metadata() emits only service_id, customer_id, server_id and server_node, so no chosen config option ever becomes an environment variable. | 🟡 | P4 |
| SVC-22 | Upgrade server call on product change | internal/service.apply enqueues a resize operation on a price change, but provisioning.Worker.execute has no resize case and fails it as an unsupported operation kind, and extproto has no provisioner.upgrade. | ❌ | P4 |
| SVC-23 | Server jobs (`CreateJob`, `SuspendJob`, `TerminateJob`) with `deleteExtensionServer` | service_operations with Worker.Run, Reap and Reconcile replaces Paymenter's CreateJob, SuspendJob and TerminateJob, and termination removes the remote resource through provisioner.terminate. | ✅ | — |

### 3.11 Cancellations

Source: `app/Models/ServiceCancellation.php`, `app/Livewire/Services/Cancel.php`,
`app/Admin/Resources/ServiceCancellationResource.php`.

| ID | Paymenter feature | Catalyst today | Status | Phase |
|---|---|---|---|---|
| CAN-01 | Customer cancellation request with `immediate` or `end_of_period` | internal/service/cancellations.go validate and the service_cancellations CHECK accept immediate and end_of_period, but web/src/views/portal-service.ts posts only the end-of-period /api/v1/portal/services/{id}/cancel route, so no customer screen offers an immediate request. | 🟡 | P2 |
| CAN-02 | Cancellation reason (free text, max 255) | service.MaxReasonLength and the service_cancellations reason CHECK bound the text at 255 characters and web/src/views/cancellations.ts renders it, but the portal cancel path portal.Manager.RequestCancellation accepts no reason, so a customer cannot supply one. | 🟡 | P2 |
| CAN-03 | Cancellation record + admin queue (`ServiceCancellationResource`) | service_cancellations, service.Store.Cancellations and GET /api/v1/staff/cancellations back web/src/views/cancellations.ts, but portal.Manager.RequestCancellation only sets subscriptions.cancel_at_period_end and inserts no row, so a portal cancellation never reaches the queue. | 🟡 | P2 |
| CAN-04 | Cancellation notification to customer and staff | notify.EventServiceCancellationReceived and its CancellationReceived template mail the customer from notifyCancellationReceived in internal/httpapi/service_cancellations.go, but no staff notification is emitted and the legacy portal cancel path sends nothing. | 🟡 | P3 |
| CAN-05 | Cancellation prevents renewal | internal/renewal/renewal.go returns OutcomeSkippedCancelled and calls endSubscription when subscriptions.cancel_at_period_end is set, so no further period is invoiced. | ✅ | — |
| CAN-06 | Immediate cancellation triggers terminate | service.Store.ExecuteCancellation enqueues the terminate operation through enqueueOperation with the terminate:<service>:<generation> key and web/src/views/cancellations.ts exposes it as Execute, although the scheduled ExecuteDue job is not wired into cmd/billing/jobs.go. | ✅ | P2 |
| CAN-07 | Client can cancel only if `cancellable` (not already cancelled, not free/one-time) | portal.actionsFor offers cancel only for an active service whose cancel_at_period_end is false, and service.Store.requestCancellation accepts only active or suspended statuses and refuses a second open request, so the action list and the handler agree. | ✅ | — |

**Design note.** "Immediate cancellation" is destructive and should not be a customer-only
switch. Recommendation: model a *request* (`CAN-03`) with a type, then let staff or an
automated rule execute it. That also gives the audit trail Paymenter lacks.

### 3.12 Upgrades, downgrades and config options

Source: `app/Models/ServiceUpgrade.php`, `ProductUpgrade.php`, `ConfigOption.php`,
`ConfigOptionProduct.php`, `ServiceConfig.php`, `app/Livewire/Services/Upgrade.php`,
`app/Services/ServiceUpgrade/ServiceUpgradeService.php`, `app/Admin/Resources/ConfigOptionResource.php`.

| ID | Paymenter feature | Catalyst today | Status | Phase |
|---|---|---|---|---|
| UPG-01 | Product upgrade graph (`product_upgrades`) | product_upgrades in internal/store/migrations/0020_commerce.sql plus internal/httpapi/upgrades_ext.go and web/src/views/upgrades.ts provide the directed graph with admin add and remove, and internal/service/upgrades.go reachable enforces it. | ✅ | P2 |
| UPG-02 | Customer upgrade/downgrade flow: choose product, same billing period, config options | web/src/views/portal-service.ts requests an upgrade from /api/v1/portal/services/{id}/upgrade-options and the store enforces the same interval and currency, but config options are changed through a separate PUT /api/v1/portal/services/{id}/config call rather than as part of the upgrade. | 🟡 | P2 |
| UPG-03 | Prorated price for the remainder of the period | service.ComputeProration and ProratedDifference in internal/service/upgrades.go prorate by the exact remaining fraction of the real billing period with one RoundHalfUp rounding, and prorationJSON in internal/httpapi/service_upgrades.go exposes the ratio. | ✅ | P2 |
| UPG-04 | Upgrade creates an invoice; downgrade credits the difference | an upgrade issues a prorated invoice via issueProratedInvoice, but a downgrade only records CreditMinor with CreditPending in service.Store.upgrade and nothing in internal/credit calls ApplyDowngrade, so the credit is never posted. | 🟡 | P2 |
| UPG-05 | Upgrade applied by a queued server call, then the service is updated | service.Store.ApplyUpgrade reprices the subscription and enqueues a resize operation, but ApplyPendingUpgrades is not scheduled in cmd/billing/jobs.go and no route calls it, so a paid upgrade is never applied automatically. | 🟡 | P2 |
| UPG-06 | Pending upgrade invoices recalculated/cancelled by cron | service.Store.CancelStaleUpgrades voids an unpaid upgrade invoice after StaleUpgradeAfter, but cmd/billing/jobs.go registers no job for it, which docs/parity-status.md E2.2 also records. | 🟡 | P2 |
| UPG-07 | Config options: parent/child (dropdown/radio/checkbox/text/number), `env_variable`, `hidden`, `upgradable`, `sort`, `description` | config_options in internal/store/migrations/0020_commerce.sql carries parent_id, option_type of dropdown/radio/checkbox/text/number, env_variable, hidden, upgradable, sort_order and description, edited by web/src/views/config-options.ts. | ✅ | P2 |
| UPG-08 | Config option pricing per plan/period (child price + setup fee) | config_option_prices keys amount_minor and setup_minor by config_option_id, price_version_id and choice, and web/src/views/config-options.ts edits that matrix per price version. | ✅ | P2 |
| UPG-09 | Config options attached to products (`config_option_products`) | config_options.product_id attaches every option to a product and service.Store.ConfigOptions reads them per product, a single-owner column rather than Paymenter's config_option_products join. | ✅ | P2 |
| UPG-10 | Selected options stored on the service (`service_configs`) and sent to the server | internal/register/checkout.go and internal/customer/service.go write the selection to service_configs and ResolveSelection builds the env map, but provisioning.Worker Operation.metadata sends only service_id, customer_id, server_id and server_node, so env_variable values never reach the adapter. | 🟡 | P2 |
| UPG-11 | Config option upgrade path (change RAM/CPU on an existing service) | service.Store.ChangeServiceConfig re-prices an upgradable option through the same proration and service_upgrades row and PUT /api/v1/portal/services/{id}/config is offered by web/src/views/portal-service.ts. | ✅ | P2 |
| UPG-12 | Checkout config fields supplied by the server extension | internal/extproto/spec.go has no method for an extension to supply checkout fields and checkout options come only from config_options via loadConfig in internal/register/checkout.go. | ❌ | P4 |
| UPG-13 | Admin config option CRUD + products relation | the staff routes in internal/httpapi/service_config.go create, update, delete and price options, rendered by web/src/views/config-options.ts. | ✅ | P5 |
| UPG-14 | Admin "upgradable" flag on options | config_options.upgradable is edited by the Upgradable switch in web/src/views/config-options.ts and enforced by ChangeServiceConfig and handlePortalConfigOptions in internal/httpapi/portal_services.go. | ✅ | P5 |

**Design note.** Config options are the thing hosting customers actually buy. They also change
the price of a *subscription*, not just an order: a service with "8 GB" must renew at the 8 GB
price. That means the subscription must carry its option selection and the renewal engine must
price from it. This is a bigger change than a checkout form; scope it as its own epic.

### 3.13 Support tickets

Source: `app/Models/Ticket.php`, `TicketMessage.php`, `TicketMessageAttachment.php`,
`TicketMailLog.php`, `app/Livewire/Tickets/*`, `app/Admin/Resources/TicketResource.php`,
`app/Console/Commands/FetchEmails.php`, settings `tickets_*`.

| ID | Paymenter feature | Catalyst today | Status | Phase |
|---|---|---|---|---|
| TKT-01 | Ticket table: subject, status, priority, department, user, assigned_to, service | tickets, ticket_messages, ticket_attachments and ticket_mail_logs exist in internal/store/migrations/0020_commerce.sql and every route in internal/httpapi/tickets.go is gated on ticket:read:self, ticket:write:self, ticket:read:all or ticket:write:all. | ✅ | P3 |
| TKT-02 | Ticket messages (user or staff author, body) | ticket_messages stores author_user_id, from_staff, body and message_id, read by Store.threadTx and written by insertMessage in internal/tickets/tickets.go. | ✅ | P3 |
| TKT-03 | Attachments (uuid, filename, path, size, mime) with 10 MB cap and policy-checked download | tickets.ValidateAttachment, AddAttachmentAsCustomer and AddAttachmentAsStaff in internal/tickets/attachments.go enforce MaxAttachmentBytes of 10 MB and a content-type allowlist, and writeAttachment sets nosniff and an attachment Content-Disposition on download. | ✅ | P3 |
| TKT-04 | Client: create ticket (subject, department, priority, service, first message) | POST /api/v1/portal/tickets with subject, department, priority, service_id and a first message runs Store.CreateTx, which also stores custom-field answers and refuses the create naming an unanswered required field. | ✅ | P3 |
| TKT-05 | Client: list tickets with status filter | Store.ListForCustomer filters on customer_id and status with cursor pagination, is served by handlePortalTickets and rendered in web/src/views/tickets.ts. | ✅ | P3 |
| TKT-06 | Client: view thread, reply, close | Store.ThreadForCustomer, ReplyAsCustomerTx and CloseAsCustomerTx back web/src/views/ticket.ts, and requireCloseAnswers refuses a close with the unanswered field's label. | ✅ | P3 |
| TKT-07 | Staff: ticket queue, filters, assignment, status/priority/department change, reply | Store.ListForStaff filters by status, priority, department, assignee and query, and internal/httpapi/tickets_staff.go adds reply, close, assign, patch and staff create for a named customer_id, rendered by web/src/views/support-queue.ts and web/src/views/support.ts. | ✅ | P3 |
| TKT-08 | Departments configured in settings; priorities low/normal/high; statuses open/replied/closed | the tickets settings group in internal/settings/settings.go holds departments, priorities are tickets.Priorities, statuses are tickets.Statuses, and defaults come from tickets.DefaultSettings. | ✅ | P3 |
| TKT-09 | Disable tickets setting; disallow client closing setting | tickets.disabled and tickets.client_closing_disabled are read by settingsFrom and enforced as ErrDisabled and ErrClosingDisabled, edited in web/src/views/system-settings.ts. | ✅ | P3 |
| TKT-10 | Auto-close tickets with no reply for N days | tickets.CloseStale closes stale tickets after tickets.autoclose_days and the ticket-autoclose job in cmd/billing/jobs.go runs it every six hours. | ✅ | P3 |
| TKT-11 | Email piping: IMAP fetch, reply parsing, message-id threading, attachment import, mail log | the webhook POST /api/v1/webhooks/mail/piping and the hand-written IMAP poller in internal/tickets/imap.go share Ingest with message-id threading and ticket_mail_logs, but inbound attachments are counted and refused rather than imported. | 🟡 | P3 |
| TKT-12 | Ticket mail log (`message_id`, subject, from, to, body, status) | ticket_mail_logs in internal/store/migrations/0020_commerce.sql stores message_id, subject, from_address, to_address, body and status, written by Store.logInbound and listed by ListInboundMail. | ✅ | P3 |
| TKT-13 | New-reply email notification + in-app notification | notify.EventNewTicketMessage has mail and in-app templates, but notify.TicketMessage is called only from notifyAssignee in internal/tickets/piping.go, so a staff reply in tickets_staff.go notifies no customer and a portal reply notifies no assignee. | 🟡 | P3 |
| TKT-14 | Support widget on the client dashboard | customers reach tickets from web/src/ui/layout.ts and web/src/views/tickets.ts, but web/src/views/portal.ts has no ticket card, so the only dashboard widget is the staff open_tickets one in internal/httpapi/dashboard_widgets.go. | 🟡 | P3 |
| TKT-15 | Ticket events for extensions (`Ticket/Created`, `TicketMessage/Created`) | internal/extproto/events.go lists ticket.created, ticket.replied, ticket.closed, ticket.assigned and ticket.updated and internal/httpapi/extension_hooks.go delivers them to subscribed extensions. | ✅ | P4 |

**Design note.** The permissions are already a promise the code does not keep
(`internal/authn/authz.go` grants `ticket:read:self` / `ticket:write:self` to customers and to
the support role). Shipping tickets also means deciding what a support agent may see: the
support role currently holds `customer:read:all`, `invoice:read:all`, `service:read:all` and
`service:hold`, which is a reasonable first-line bundle.

### 3.14 Notifications and email

Source: `app/Models/NotificationTemplate.php`, `NotificationPreference.php`,
`NotificationSubscription.php`, `Notification.php`, `app/Helpers/NotificationHelper.php`,
`app/Listeners/*`, `database/seeders/EmailTemplateSeeder.php`, settings `mail_*`.

| ID | Paymenter feature | Catalyst today | Status | Phase |
|---|---|---|---|---|
| NTF-01 | Notification templates per event, admin-editable (subject, mail body, in-app title/body/url) | Hardcoded Go templates for 4 kinds | 🟡 | P3 |
| NTF-02 | Per-template channel toggles (`mail_enabled`, `in_app_enabled`, `enabled`) | None | ❌ | P3 |
| NTF-03 | Per-user notification preferences (`force` / `choice_on` / `choice_off` / `never`) | None | ❌ | P3 |
| NTF-04 | In-app notification inbox: list, unread, mark read, deep link | None | ❌ | P3 |
| NTF-05 | Web push subscriptions (VAPID keys, subscribe, test push) | None | ❌ | P3 |
| NTF-06 | Email log: to, subject, status, error, sent_at; retention | `notification_deliveries` records status/attempts/error but no admin view | 🟡 | P3 |
| NTF-07 | Events: new login, new invoice, invoice paid, payment failed, new order, service activated/suspended/terminated, cancellation received, ticket reply, email verification, password reset | 4 kinds: password reset, email verification, invoice issued, dunning reminder | 🟡 | P3 |
| NTF-08 | CC/BCC per template | None | ❌ | P3 |
| NTF-09 | System emails (cron failure, test email) | No admin test email; worker failures are logged | 🟡 | P3 |
| NTF-10 | Mail settings: disable mail, must verify, SMTP host/port/user/pass/encryption/from | Env-only SMTP; no admin surface | 🟡 | P5 |
| NTF-11 | Mail header/footer/CSS branding | Plain text only | ❌ | P3 |
| NTF-12 | Durable outbox with retries and dedupe | `notification_deliveries` + worker | ✅ stronger | — |
| NTF-13 | Discord notifications extension (webhook, selectable events) | None | ❌ | P6 |
| NTF-14 | Notification on login from a new device | `auth_events` records logins; no email | 🟡 | P3 |

**Design note.** The outbox is already the hard part and it is done. `NTF-01`/`NTF-02` are a
template table plus a render step; `NTF-05` needs a push subscription table and VAPID keys.
Keep the existing rule that templates are plain text unless a sanitizer is introduced: the
current package deliberately does not render markup.

### 3.15 Identity, authentication and access

Source: `routes/web.php`, `app/Livewire/Auth/*`, `app/Livewire/Client/Security.php`,
`app/Models/User.php`, `UserSession.php`, `UserAuthenticationLog.php`, `ApiKey.php`,
`OauthClient.php`, `config/permissions.php`, settings `security`, `social-login`, `other`.

| ID | Paymenter feature | Catalyst today | Status | Phase |
|---|---|---|---|---|
| IDN-01 | Self-registration (first/last name, email, password, captcha, TOS) | No registration endpoint or page; customers are created by CLI | ❌ | P1 |
| IDN-02 | Login with email/password | `POST /api/v1/auth/login` | ✅ | — |
| IDN-03 | TOTP two-factor with a second login step | TOTP + recovery codes, sealed secrets, challenge token; **staff UI only** (no customer security page) | 🟡 | P1 |
| IDN-04 | Email verification with signed link | Hashed single-use token; non-enumerating | ✅ | — |
| IDN-05 | Password reset by email | Hashed single-use token; revokes all sessions | ✅ | — |
| IDN-06 | Change password while signed in | No endpoint or UI (only reset) | ❌ | P1 |
| IDN-07 | Social login: Google | None | ❌ | P6 |
| IDN-08 | Social login: GitHub | None | ❌ | P6 |
| IDN-09 | Social login: Discord | None | ❌ | P6 |
| IDN-10 | OAuth client records + redirect + secret regeneration (Passport) | None | ❌ | P6 |
| IDN-11 | OAuth2 authorization server: `/oauth/authorize`, `/oauth/token`, scopes, `/api/me` | Static API keys only; no OAuth2 flow | ❌ | P6 |
| IDN-12 | API keys with IP allowlist, enabled toggle, last used | `api_keys` with prefix, digest, scopes, expiry, last_used; no IP allowlist | 🟡 | P5 |
| IDN-13 | Editable roles and permissions (Role model, permission matrix) | 4 fixed roles, 27 fixed permissions, code-defined | 🟡 | P5 |
| IDN-14 | Impersonate a user, with a banner and audit trail | None | ❌ | P5 |
| IDN-15 | Session list and per-session logout | Sessions stored, revoked on reset; settings shows the current session only, no list/revoke | 🟡 | P5 |
| IDN-16 | Session validation locked to IP and/or user agent (admin/client/both) | No IP/UA binding | ❌ | P5 |
| IDN-17 | Login history (`user_authentication_logs`) | `auth_events` recorded; no client/staff view | 🟡 | P5 |
| IDN-18 | CAPTCHA: reCAPTCHA v2/v3, Turnstile, hCaptcha | None | ❌ | P6 |
| IDN-19 | TOS URL setting + acceptance | None | ❌ | P1 |
| IDN-20 | Trusted proxies setting (CIDR) | `BILLING_TRUSTED_PROXIES` env, correctly gated | ✅ | — |
| IDN-21 | Registration disabled setting | Registration does not exist | ❌ | P1 |
| IDN-22 | Admin API: users, invoices, invoice-items, tickets, ticket-messages, orders, services, coupons, credits, products, categories, properties | Own staff API (78 operations) but no customer CRUD, no ticket API, no credit grant | 🟡 | P5 |
| IDN-23 | API documentation (Scramble) | Generated OpenAPI + typed client + drift check | ✅ stronger | — |
| IDN-24 | Personal access tokens / `oauth_personal_access_clients` | API keys serve this purpose | ✅ | — |

**Design note (`IDN-01`).** Registration is the single highest-leverage gap: without it the
storefront cannot acquire a customer. It must be paired with rate limiting (already exists),
email verification (exists), and a decision about whether a new user is immediately a
`customer` with a billing account or only after first purchase (`P0-05`).

### 3.16 Operator surface

Source: `app/Admin/Resources/**`, `app/Admin/Pages/**`, `app/Admin/Widgets/**`,
`app/Admin/Clusters/**`, `app/Providers/Filament/AdminPanelProvider.php`.

| ID | Paymenter feature | Catalyst today | Status | Phase |
|---|---|---|---|---|
| ADM-01 | Dashboard: revenue chart, overview stats with period/trend, active users, support table | Exception-focused dashboard with counts and latest invoices | 🟡 | P5 |
| ADM-02 | Settings page (11 groups, 77 keys) | No system settings API or UI; env + DB policies | ❌ | P5 |
| ADM-03 | Product/category CRUD | Catalog CRUD exists | ✅ | — |
| ADM-04 | Plan/price editor, product duplicate | Add-price-version UI exists; no plan type/period editor, no duplicate | 🟡 | P5 |
| ADM-05 | Coupon CRUD | None | ❌ | P5 |
| ADM-06 | Config option CRUD | None | ❌ | P5 |
| ADM-07 | Tax rate CRUD | None | ❌ | P5 |
| ADM-08 | Currency CRUD | None | ❌ | P5 |
| ADM-09 | Custom property CRUD | None | ❌ | P5 |
| ADM-10 | Gateway CRUD + config | None | ❌ | P4 |
| ADM-11 | Server CRUD + config + test connection | None | ❌ | P4 |
| ADM-12 | Extension list, enable/disable, install from marketplace, upload zip, scaffold, upgrade | JSON-RPC extensions; no install UI or marketplace | ❌ | P6 |
| ADM-13 | Role CRUD + permission matrix | Fixed roles | 🟡 | P5 |
| ADM-14 | API key admin CRUD | Own keys only | 🟡 | P5 |
| ADM-15 | OAuth client CRUD | None | ❌ | P6 |
| ADM-16 | Notification template CRUD | None | ❌ | P5 |
| ADM-17 | Email log viewer | None | ❌ | P5 |
| ADM-18 | Failed job viewer with retry/delete/mark resolved | Operations queue with automatic retry; no manual retry | 🟡 | P5 |
| ADM-19 | Error log viewer | None | ❌ | P5 |
| ADM-20 | HTTP log viewer (request/response/headers) | None | ❌ | P5 |
| ADM-21 | Audit viewer | Staff audit screen + append-only log | ✅ stronger | — |
| ADM-22 | Cron statistics page | Ops overview; no per-task run history | 🟡 | P5 |
| ADM-23 | Updates page: check/update app and extensions | None | ❌ | P6 |
| ADM-24 | Invoice transactions resource (create/edit/delete/refund) | Payments list/detail + refund; no manual create | 🟡 | P5 |
| ADM-25 | Adjustment notes resource (credit/debit, void) | Credit notes only | 🟡 | P5 |
| ADM-26 | Service cancellation resource | None | ❌ | P2 |
| ADM-27 | Ticket message resource | None | ❌ | P3 |
| ADM-28 | User detail tabs: tickets, services, invoices, credits, billing agreements | Customer detail: accounts, subscriptions, services, invoices, balance, holds, auth events | 🟡 | P5 |
| ADM-29 | User CRUD (create/edit/delete, role, verified, 2FA) | Read + billing profile; no create/update/delete | 🟡 | P5 |
| ADM-30 | Impersonation | None | ❌ | P5 |
| ADM-31 | Create invoice with item repeater, send-email toggle, publish, PDF | Line editor with product picker + `issue`; no send-email toggle, no PDF | 🟡 | P5 |
| ADM-32 | Create order with product/plan/price/quantity/services | `/orders/new`: price-version picker, customer picker, quantity, deep links | ✅ | — |
| ADM-33 | Create service | None | ❌ | P5 |
| ADM-34 | Add credit to a user | Credit-note application only | 🟡 | P5 |
| ADM-35 | Bulk actions (delete many) | Deliberately absent on immutable records | ⛔ | — |
| ADM-36 | Admin navigation groups (Administration, Configuration, Extensions, System) | Staff nav grouped Overview/Revenue/Operations/Trust | 🟡 | P5 |
| ADM-37 | Admin clusters (Extensions, Invoice, Services) | None | ❌ | P5 |

**Deliberate divergence (`ADM-35`).** Paymenter offers bulk delete. Catalyst refuses to
delete issued invoices, journal rows, audit rows and credit notes at the database level. Bulk
delete on those records would either fail or require disabling the guarantee. Keep it absent;
offer bulk *archive* only where the domain allows it.

### 3.17 Platform, operations and integrations

Source: `routes/console.php`, `app/Console/Commands/*`, `app/Admin/Pages/*`,
`config/settings`, `app/Classes/Settings.php`.

| ID | Paymenter feature | Catalyst today | Status | Phase |
|---|---|---|---|---|
| OPS-01 | Daily cron: create renewal invoices, apply credits, charge agreements | Renewal engine + notify outbox | ✅ | — |
| OPS-02 | Cron: cancel unpaid orders after N days, restore stock | No order-cancel rule | ❌ | P2 |
| OPS-03 | Cron: recalculate/cancel pending upgrade invoices | No upgrades | ❌ | P2 |
| OPS-04 | Cron: suspend overdue services after N days | Dunning engine with configurable steps | ✅ stronger | — |
| OPS-05 | Cron: terminate after N days suspended | Dunning suspends; termination is a staff action (or an approval-gated one) | 🟡 | P2 |
| OPS-06 | Cron: close stale tickets | No tickets | ❌ | P3 |
| OPS-07 | Cron: delete old email logs | Retention policies | ✅ | — |
| OPS-08 | Cron statistics (`cron_stats`) + last run + next run + heartbeat | Ops overview; no run history | 🟡 | P5 |
| OPS-09 | Settings-driven cron thresholds (invoice days, reminder days, cancel/suspend/terminate days) | Dunning policy in DB; no admin surface | 🟡 | P5 |
| OPS-10 | WHMCS importer (CLI, batch, currencies/users/admins/categories/config options/products/tickets/orders/services/cancellations/invoices/items/payments) | None | ❌ | P6 |
| OPS-11 | Paymenter importer | None | ❌ | P6 |
| OPS-12 | Update checker + self-update + extension updates | None | ❌ | P6 |
| OPS-13 | Telemetry (opt-in, daily, anonymous) | None | ❌ | P6 |
| OPS-14 | Extension marketplace client (list, install, upload, update) | None | ❌ | P6 |
| OPS-15 | Debug mode + HTTP/error/debug log tables | Structured logs; no DB log tables/views | 🟡 | P5 |
| OPS-16 | Scheduled task heartbeat (`ScheduleHeartbeatCommand`) | Worker ticker + `doctor`; no heartbeat row | 🟡 | P5 |
| OPS-17 | `app:init`, `app:user:create`, `app:user:password-reset`, `app:settings:change`, `app:logs` CLI | `billing admin create-user/create-customer/issue-invoice/purge-test-data`, `doctor`, `migrate`, `version` | 🟡 | P5 |
| OPS-18 | Health/readiness | `/healthz`, `/readyz`, `doctor` | ✅ stronger | — |
| OPS-19 | Metrics | `/metrics` (staff-auth) | ✅ | — |
| OPS-20 | Webhook inbox with reprocessing visibility | `gateway_events` + staff webhook screen | ✅ | — |

### 3.18 Growth extensions

Source: `extensions/Others/*`.

| ID | Paymenter feature | Catalyst today | Status | Phase |
|---|---|---|---|---|
| GRW-01 | Announcements: model, admin CRUD, home + dashboard display, nav entry, policy | None | ❌ | P6 |
| GRW-02 | Affiliates: affiliate model, signup tracking, referral middleware, order association, commission, payout view, policy, nav, API permission | None | ❌ | P6 |
| GRW-03 | Affiliate commission on order paid, recurring or one-time | None | ❌ | P6 |
| GRW-04 | Discord notifications: webhook, selectable events, rich embeds | None | ❌ | P6 |
| GRW-05 | Extension hook surface: events, navigation, permissions, admin resources/pages, API permissions, checkout config, service actions | JSON-RPC gateway/provisioner only; no UI/event hooks | ❌ | P4 |

**Design note (`GRW-05`).** Paymenter extensions are in-process PHP classes that can register
navigation, permissions, admin resources and event listeners. Catalyst's extensions are
separate executables that speak JSON-RPC and cannot touch the UI. That is a deliberate
security choice. Parity here means *adding declared hook points* (e.g. a signed "client
navigation item" or "service action" returned by the extension), not loading arbitrary code.

### 3.19 Localization and theming

Source: `lang/*` (24 locales), `themes/*`, `app/Livewire/Components/LanguageSwitch.php`,
`LocaleSwitch.php`, `theme-toggle`, settings `app_language`, `allowed_languages`, `theme`.

| ID | Paymenter feature | Catalyst today | Status | Phase |
|---|---|---|---|---|
| I18N-01 | 24 locales, per-user locale, allowed-locale setting | English only | ❌ | P6 |
| I18N-02 | Language switcher in the client UI | None | ❌ | P6 |
| I18N-03 | Translated validation/error messages | English only | ❌ | P6 |
| I18N-04 | Theme packages (`themes/*`, theme settings, color tokens) | One design system, CSS tokens, light/dark toggle | 🟡 | P6 |
| I18N-05 | Theme marketplace (24 listings) | None | ⛔ | — |
| I18N-06 | Mail template branding (header/footer/CSS) | Plain text | ❌ | P3 |
| I18N-07 | Logo (light/dark), favicon, company name, system email settings | No branding settings | ❌ | P5 |
| I18N-08 | Gravatar default avatar | No avatars | ❌ | P6 |

**Deliberate divergence (`I18N-05`).** The CSP is a deliberate boundary (`img-src 'self' data:`,
no inline styles). A theme marketplace that ships arbitrary CSS/JS would either require
loosening it or a sandboxed renderer. Not planned.

---

## 4. The backlog, in build order

Each phase lists its epics, the IDs inside them, and the acceptance test that proves the epic
is done. The acceptance tests are deliberately phrased as observable behaviour, because "the
screen exists" is not parity.

### P0 — Decisions (no code until these are answered)

| Decision | Question | Why it blocks |
|---|---|---|
| P0-01 Multi-tenancy | Single merchant forever, or resellers? | Changes every table's ownership key; doing it later is a rewrite |
| P0-02 Gateway set | Stripe only, or Stripe + PayPal + Mollie? | Determines protocol additions (billing agreements, off-session charge) |
| P0-03 Coupon semantics | Discount as a line item or a price override? How do renewals and snapshots record it? | Changes invoice schema and the renewal engine |
| P0-04 Tax policy | Which jurisdictions at launch? Inclusive or exclusive default? Is a VAT ID validated? | Determines whether a tax table is enough or a rules engine is needed |
| P0-05 Registration | Does a new user get a billing account immediately, or on first purchase? | Changes the customer/auth model and the checkout flow |
| P0-06 Credits | Is credit a deposit (liability) or a refund instrument? Can it be withdrawn? | Determines ledger accounts and reporting |
| P0-07 Proforma invoices | A status on `invoices` or a separate document? | The current `CHECK` forbids a numbered non-draft invoice |
| P0-08 Provisioning set | Pterodactyl first, or Proxmox/cPanel? | Determines the first real adapter and the config-option shape |
| P0-09 Ticket scope | Full email piping at launch, or web-only first? | Email piping is a separate, large subsystem |
| P0-10 Money display | Currency prefix/suffix/grouping table, or a formatting library? | Must not reintroduce floats |

### P1 — The revenue path

**Goal:** an anonymous visitor can find a product, register, buy it, pay with a real provider,
receive a correct invoice and a provisioned service, with coupons and tax applied.

| Epic | IDs | Acceptance |
|---|---|---|
| E1.1 Registration and account | IDN-01, IDN-03, IDN-06, IDN-19, IDN-21 | A new visitor registers, verifies email, signs in, changes password, enrols and uses a second factor, and sees an empty portal; duplicate email and weak password are refused; a bot hitting register is rate-limited |
| E1.2 Cart and customer checkout | CT-01…CT-10, SF-18, SF-20, SF-21 | A customer adds two products, changes quantity, applies a coupon, accepts TOS, checks out; one order, one invoice, N services, one balanced ledger transaction; a concurrent checkout of the last stock unit fails cleanly; staff can open and edit the resulting order |
| E1.3 Plan types and periods | SF-13, SF-14, SF-15 | A free product activates without an invoice; a one-time product creates a service with no renewal; a recurring product creates a subscription with the right next-bill date |
| E1.4 Coupons | CPN-01…CPN-09 | Percentage and fixed coupons, product-restricted, expiry, max uses and max uses per user are enforced under lock; a coupon that applies for 2 cycles discounts exactly 2 invoices and no more; the invoice snapshot explains the discount |
| E1.5 Tax rates | TAX-01…TAX-08 | A customer in DE is charged 19%, in US-CA 0% (or the configured rate), with inclusive and exclusive modes; the rate and jurisdiction are recorded in the snapshot; a tax-exempt customer is charged zero with a reason |
| E1.6 Currencies | CUR-01…CUR-07 | Two currencies are configured with different formats; the storefront switches; the cart locks its currency; a price missing in the selected currency is unavailable, never free |
| E1.7 Credits | CRD-01…CRD-05, CRD-08, CRD-10 | A customer tops up credit through the gateway; the next invoice is paid automatically if credit covers it; partial credit leaves the correct remainder; a replay of the top-up webhook does not double the balance |
| E1.8 Invoice documents | INV-01, INV-03, INV-04, INV-07, INV-08, INV-10, INV-11, INV-14, INV-16, INV-17 | A customer downloads a PDF with the right number, bill-to, lines, tax and totals; a proforma invoice gets its official number only on payment; a credit note is downloadable and immutable |
| E1.9 Customer pay flow | PAY-17, PAY-18, PAY-19 | The portal invoice page offers "pay with credit" and "pay with gateway", shows the remainder, polls or waits for settlement, and never double-charges on refresh |
| E1.10 Storefront completeness | SF-01…SF-03, SF-06, SF-22, CAT-01, CAT-03 | Category pages have stable URLs, nested categories render, product pages show plans and options, a hidden category hides its products everywhere, and staff can create a category with an image and a plan/price without a CLI |

**Definition of done for P1:** a person who has never seen the system can go from `/` to a
paid, provisioned service using a real (test-mode) gateway, and the books balance per currency.

### P2 — Service lifecycle

| Epic | IDs | Acceptance |
|---|---|---|
| E2.1 Config options | UPG-07…UPG-11 | A product offers RAM (dropdown) and a checkbox; the selected options change the price and setup fee; they are stored on the service, sent to the provisioner, and used on renewal |
| E2.2 Upgrades/downgrades | UPG-01…UPG-06, CAT-09, SVC-10, CRD-06, OPS-03 | A customer upgrades mid-cycle: a prorated invoice is issued, payment triggers the server upgrade, the service price changes, and the next renewal uses the new price; a downgrade credits the difference; a stale upgrade invoice is cancelled by the cron |
| E2.3 Cancellation workflow | CAN-01…CAN-06, ADM-26, SVC-09 | A customer requests cancellation with a reason and a type; staff see the queue; end-of-period stops renewal but keeps the service until the paid date; immediate cancellation terminates through the queue; the request is a row, not an inference from `cancel_at_period_end` |
| E2.4 Stock and limits | SF-07…SF-09 | Stock decrements atomically, is restored on cancel/terminate, and a zero-stock product cannot be bought; per-user limits and quantity policy are enforced |
| E2.5 Order cancellation | CT-11, OPS-02 | An unpaid order is cancelled after N days, its pending invoice is voided, and stock is restored |
| E2.6 Service metadata | SVC-02, SVC-03, SVC-04, SVC-07, SVC-08 | Customers can label a service; the description is derived from product + options; custom properties are stored and shown on the invoice when configured; the service carries the order/product/plan/quantity/price a support agent needs |
| E2.7 Termination rule | OPS-05 | A service suspended for N days is terminated through the queue, with the invoice voided and the customer notified; the threshold is configurable |

### P3 — Support and communication

| Epic | IDs | Acceptance |
|---|---|---|
| E3.1 Tickets | TKT-01…TKT-10, TKT-14, ADM-27 | A customer opens a ticket against a service, replies, closes it; staff see the queue, assign, change status/priority/department and reply; a customer cannot read another customer's ticket (404, not 403); a stale ticket closes on the cron |
| E3.2 Attachments | TKT-03 | A 9 MB file uploads; an 11 MB file is refused; a download is policy-checked and served with a safe content type |
| E3.3 Email piping | TKT-11, TKT-12 | A reply to a ticket notification is imported, threaded to the right ticket, attachments are stored, and a duplicate message-id is ignored |
| E3.4 Notifications | NTF-01…NTF-09, NTF-11, NTF-14 | Every event in the Paymenter list has an editable template and an in-app entry; a user can opt out of choice-on events but not force events; the in-app inbox marks read; a new-device login sends an email |
| E3.5 Push | NTF-05 | A user subscribes to web push, receives a test push, and the subscription is stored per device |
| E3.6 Ticket cron | OPS-06 | A ticket with no reply for N days closes and notifies |
| E3.7 Activation email | SF-11 | A product can add a snippet to the activation email, and the rendered message contains it |
| E3.8 Ticket notifications | TKT-13 | A staff reply emails the customer and creates an in-app entry; a customer reply emails the assignee |

### P4 — Real integrations

| Epic | IDs | Acceptance |
|---|---|---|
| E4.1 Protocol v2 | PAY-10, PAY-11, SVC-11, GRW-05, CT-13, TKT-15 | The protocol declares billing-agreement and charge methods and service-action hooks, and emits cart/order/ticket events; conformance fixtures prove them; a v1 extension is still accepted |
| E4.2 Stripe | PAY-01, PAY-06, PAY-07, ADM-10 | A test-mode Stripe checkout settles a real invoice; a replayed webhook allocates once; a refund reaches Stripe and reconciles; a declined card leaves the invoice open; staff configure and test the gateway in the panel |
| E4.3 PayPal / Mollie | PAY-02…PAY-04 | Each gateway passes the same conformance suite as Stripe |
| E4.4 Billing agreements | PAY-10, PAY-11 | A customer saves a method, sets it as default for recurring, and a renewal is charged off-session; a failed charge enters dunning, not a crash |
| E4.5 Pterodactyl | SVC-16…SVC-22, CAT-08, SF-12, ADM-11, UPG-12 | A paid order creates a real server; suspend/resume/terminate work; a timeout is `unknown` and reconciled; product config options map to environment variables; a product can be assigned to a server; staff configure and test the server; the server can contribute checkout fields |
| E4.6 Second provisioner | SVC-18, SVC-19 | A second adapter (cPanel or Proxmox) passes the same conformance suite, proving the protocol is not Pterodactyl-shaped |
| E4.7 Extension hooks | GRW-05 | An extension can contribute a client nav item and a service action without arbitrary code execution |

### P5 — Operator completeness

| Epic | IDs | Acceptance |
|---|---|---|
| E5.1 System settings | ADM-02, OPS-09, NTF-10, I18N-07 | An operator edits company name, logo, favicon, TOS, SMTP, tax mode, cron thresholds and numbering from the panel; the change is audited and takes effect without a restart where the design allows |
| E5.2 Admin CRUD gaps | ADM-04…ADM-09, ADM-13…ADM-18, ADM-24, ADM-25, ADM-28…ADM-37, CPN-10, CT-10, CT-12, SVC-12…SVC-15, UPG-13, UPG-14, CRD-07, INV-09, INV-18, PAY-13, PAY-14, IDN-22 | Every Paymenter admin resource and admin REST resource has a Catalyst equivalent, except the deliberate divergences; each mutation is idempotent and audited |
| E5.3 Roles and sessions | IDN-12, IDN-13, IDN-14, IDN-15, IDN-16, IDN-17, ADM-30 | Roles are editable (or a decision records why not); API keys can be IP-restricted; an admin sees and revokes sessions; impersonation is audited and visibly marked |
| E5.4 Observability | ADM-17…ADM-22, OPS-08, OPS-15, OPS-16 | Email, error and HTTP logs are viewable; cron runs have history; the worker heartbeat is visible and `doctor` reports it |
| E5.5 CLI parity | OPS-17 | Admin can create a user, reset a password, change a setting and tail logs from the CLI |
| E5.6 Dashboard and navigation | ADM-01, ADM-36, ADM-37 | The dashboard shows revenue over a selectable period, active customers and open tickets alongside the exception queue; the staff nav is grouped like Paymenter's |
| E5.7 Catalog polish | CAT-04, CAT-06 | A product can be duplicated and its description edited as rich text (sanitised, CSP-safe) |

### P6 — Growth and platform

| Epic | IDs | Acceptance |
|---|---|---|
| E6.1 Announcements | GRW-01 | Staff publish an announcement; it appears on the storefront and dashboard until unpublished |
| E6.2 Affiliates | GRW-02, GRW-03 | A referral link attributes a signup; a paid order records a commission; the payout report reconciles |
| E6.3 Social login | IDN-07…IDN-10 | Google/GitHub/Discord login creates or links an account; an existing email is not silently merged |
| E6.4 OAuth2 server | IDN-11 | A third-party app obtains a token with scopes and calls the documented API; a token cannot exceed its owner's permissions |
| E6.5 Importer | OPS-10, OPS-11 | A WHMCS dump imports with a dry run, a row-count report and an idempotent re-run |
| E6.6 i18n | I18N-01…I18N-03, I18N-06, I18N-08, SF-19 | The panel and client area render in a second locale; emails use the recipient's locale; the storefront has a language switcher |
| E6.7 Updates/telemetry/marketplace | OPS-12…OPS-14, ADM-12, ADM-23 | The panel reports its version, checks for updates, and can install a signed extension |
| E6.8 Discord notifications | NTF-13, GRW-04 | Selected events post to a Discord webhook |
| E6.9 Captcha | IDN-18 | Registration and password reset require a configured captcha; a failed challenge is refused |
| E6.10 Marketplace breadth | PAY-05, INV-15 | The protocol can host a marketplace gateway without core changes, and a theme can supply an invoice PDF template |

---

## 5. Data-model mapping

Paymenter tables (from the live schema) → Catalyst equivalent.

| Paymenter | Catalyst | Status |
|---|---|---|
| `users` | `users`, `customers`, `customer_accounts` | ✅ (identity split is deliberate) |
| `roles` | `staff_roles` + code-defined `Role` | 🟡 not editable |
| `api_keys` | `api_keys` | ✅ |
| `oauth_clients`, `oauth_*` | — | ❌ |
| `sessions`, `user_sessions` | `sessions` | ✅ |
| `user_authentication_logs` | `auth_events` | ✅ |
| `password_reset_tokens` | `password_resets` | ✅ |
| `settings` | env + `billing_policies` + `retention_policies` | 🟡 no general settings table |
| `extensions` | env (`BILLING_*_EXTENSION`) | 🟡 no records/UI |
| `categories` | `categories` | ✅ |
| `products` | `products` | 🟡 missing stock/limits/server/email_template |
| `plans` | `price_versions` (interval + name) | 🟡 no free/one-time/arbitrary period |
| `prices` | `price_versions` | ✅ |
| `product_upgrades` | — | ❌ |
| `config_options`, `config_option_products` | — | ❌ |
| `custom_properties`, `properties` | — | ❌ |
| `coupons`, `coupon_products` | — | ❌ |
| `tax_rates` | — (fixed pricer) | ❌ |
| `currencies` | — (currency code on price) | ❌ |
| `carts`, `cart_items` | — | ❌ |
| `orders` | `orders`, `order_items` | ✅ |
| `services` | `services`, `subscriptions` | ✅ |
| `service_configs` | `services.config` JSON | 🟡 |
| `service_upgrades` | — | ❌ |
| `service_cancellations` | `subscriptions.cancel_at_period_end` + `cancelled_at` | 🟡 |
| `invoices` | `invoices` | ✅ |
| `invoice_items` | `invoice_lines` | ✅ |
| `invoice_snapshots` | `invoices.snapshot` | ✅ |
| `invoice_transactions` | `payments`, `payment_allocations`, `refunds` | ✅ stronger |
| `adjustment_notes` | `credit_notes`, `credit_applications` | 🟡 credit only |
| `credits` | ledger `customer_credit` | ✅ (no deposit flow) |
| `billing_agreements` | — | ❌ |
| `tickets`, `ticket_messages`, `ticket_message_attachments`, `ticket_mail_logs` | — | ❌ |
| `notification_templates`, `notification_preferences`, `notification_subscriptions`, `notifications` | `notification_deliveries` | 🟡 outbox only |
| `email_logs` | `notification_deliveries` | 🟡 |
| `audits`, `audit_logs` | `audit_events` | ✅ stronger |
| `cron_stats` | — (ops overview) | ❌ |
| `debug_logs` | structured logs | 🟡 |
| `failed_jobs`, `jobs`, `job_batches` | `service_operations` + ticker | 🟡 different model |
| `migrations` | `schema_migrations` (embedded) | ✅ |
| `announcements` (extension) | — | ❌ |
| `affiliates`, `affiliate_orders` (extension) | — | ❌ |

**Missing commerce tables, counted:** 34. `carts`, `cart_items`, `coupons`,
`coupon_products`, `tax_rates`, `currencies`, `config_options`, `config_option_products`,
`custom_properties`, `properties`, `billing_agreements`, `service_configs`,
`service_upgrades`, `product_upgrades`, `service_cancellations`, `tickets`, `ticket_messages`,
`ticket_message_attachments`, `ticket_mail_logs`, `notification_templates`,
`notification_preferences`, `notification_subscriptions`, `notifications`, `email_logs`,
`cron_stats`, `oauth_clients` and the four other `oauth_*` tables, `announcements`,
`affiliates`, `affiliate_orders`. That is the schema gap.

---

## 6. Admin resource mapping

| Paymenter resource | Catalyst screen | Status |
|---|---|---|
| ProductResource | `/catalog-management` | ✅ |
| CategoryResource | `/catalog-management` (categories) | ✅ |
| CouponResource | — | ❌ |
| ConfigOptionResource | — | ❌ |
| TaxRateResource | — | ❌ |
| CurrencyResource | — | ❌ |
| CustomPropertyResource | — | ❌ |
| RoleResource | — | ❌ |
| GatewayResource | — | ❌ |
| ServerResource | — | ❌ |
| ExtensionResource | — | ❌ |
| NotificationTemplateResource | — | ❌ |
| OauthClientResource | — | ❌ |
| ApiResource | `/api-keys` (own keys) | 🟡 |
| UserResource | `/customers`, `/customers/:id` | 🟡 |
| OrderResource | `/orders`, `/orders/:id` | ✅ |
| ServiceResource | `/services`, `/services/:id` | 🟡 |
| ServiceCancellationResource | — | ❌ |
| InvoiceResource | `/invoices`, `/invoices/:id` | ✅ |
| InvoiceTransactionResource | `/payments`, `/payments/:id` | ✅ |
| AdjustmentNoteResource | invoice detail (credit notes) | 🟡 |
| TicketResource | — | ❌ |
| TicketMessageResource (orphaned Pages only; the class is an API resource) | — | ❌ |
| Audits | `/audit` | ✅ stronger |
| EmailLogResource | — | ❌ |
| FailedJobResource | `/operations` | 🟡 |
| ErrorLogResource | — | ❌ |
| HttpLogResource | — | ❌ |
| Pages: Settings | `/settings` (personal only) | 🟡 |
| Pages: CronStats | `/operations` overview | 🟡 |
| Pages: Updates | — | ❌ |
| Pages: Extension (marketplace) | — | ❌ |
| Widgets: Overview/Revenue/ActiveUsers/Support/CronStat | `/` dashboard (exception-focused) | 🟡 |
| Clusters: Extensions/Invoice/Services | single nav | 🟡 |

---

## 7. Client page mapping

| Paymenter page | Catalyst page | Status |
|---|---|---|
| `/` home/storefront | `/` storefront | ✅ |
| `/products/{category}` | `/products?category=` | 🟡 |
| `/products/{category}/{product}` | `/products/:slug` | 🟡 |
| `/products/{category}/{product}/checkout` | — | ❌ |
| `/cart` | — | ❌ |
| `/login` | `/login` | ✅ |
| `/register` | — | ❌ |
| `/2fa` | login second step | ✅ |
| `/password/request`, `/password/reset/{token}` | `/forgot-password`, `/reset-password` | ✅ |
| `/email/verify` | `/verify` | ✅ |
| `/oauth/{provider}` | — | ❌ |
| `/dashboard` | `/portal` | ✅ |
| `/invoices` | `/portal/invoices` | ✅ |
| `/invoices/{invoice}` | `/portal/invoices/:id` | ✅ |
| `/services` | `/portal/services` | ✅ |
| `/services/{service}` | `/portal/services/:id` | 🟡 |
| `/services/{service}/upgrade` | — | ❌ |
| `/tickets` | — | ❌ |
| `/tickets/create` | — | ❌ |
| `/tickets/{ticket}` | — | ❌ |
| `/account` | `/portal/profile` | 🟡 |
| `/account/security` | — (staff `/settings` has MFA; customers have no page) | ❌ |
| `/account/credits` | — (balance on portal overview) | 🟡 |
| `/account/payment-methods` | — | ❌ |
| `/account/notifications` | — | ❌ |
| `/tickets/attachments/{uuid}` | — | ❌ |

---

## 8. Settings key mapping

Paymenter's settings page is a single source of truth (`app/Classes/Settings.php`): 77 keys
across 11 groups. This is the full key list and what Catalyst has for each.

| Group | Key | Catalyst today | Status |
|---|---|---|---|
| general | `company_name` | env / no branding | ❌ |
| general | `timezone` | `BILLING_*`/calendar timezone | 🟡 |
| general | `app_language` | English only | ❌ |
| general | `allowed_languages` | English only | ❌ |
| general | `app_url` | env | 🟡 |
| general | `logo`, `logo_dark`, `favicon` | no branding | ❌ |
| general | `system_email_address` | SMTP from address env | 🟡 |
| general | `tos` | none | ❌ |
| security | `captcha`, `captcha_site_key`, `captcha_secret` | none | ❌ |
| security | `trusted_proxies` | `BILLING_TRUSTED_PROXIES` | ✅ |
| security | `session_validation` | none | ❌ |
| social-login | `oauth_google`, `oauth_google_client_id`, `oauth_google_client_secret` | none | ❌ |
| social-login | `oauth_github`, `oauth_github_client_id`, `oauth_github_client_secret` | none | ❌ |
| social-login | `oauth_discord`, `oauth_discord_client_id`, `oauth_discord_client_secret` | none | ❌ |
| tax | `tax_enabled`, `tax_type` | fixed pricer config | 🟡 |
| mail | `mail_disable`, `mail_must_verify` | none | ❌ |
| mail | `mail_host`, `mail_port`, `mail_username`, `mail_password`, `mail_encryption`, `mail_from_address`, `mail_from_name` | env SMTP | 🟡 |
| mail | `mail_header`, `mail_footer`, `mail_css` | plain text | ❌ |
| tickets | `tickets_disabled` | no tickets | ❌ |
| tickets | `ticket_departments` | no tickets | ❌ |
| tickets | `ticket_client_closing_disabled` | no tickets | ❌ |
| tickets | `ticket_mail_piping`, `ticket_mail_host`, `ticket_mail_port`, `ticket_mail_email`, `ticket_mail_password` | no tickets | ❌ |
| cronjob | `cronjob_time` | worker ticker | 🟡 |
| cronjob | `cronjob_invoice`, `cronjob_invoice_reminder` | renewal/dunning policy | 🟡 |
| cronjob | `cronjob_order_cancel` | none | ❌ |
| cronjob | `cronjob_order_suspend`, `cronjob_order_terminate` | dunning policy | 🟡 |
| cronjob | `cronjob_delete_email_logs` | retention policy | ✅ |
| cronjob | `cronjob_close_ticket` | no tickets | ❌ |
| credits | `credits_enabled` | always on (ledger) | 🟡 |
| credits | `credits_minimum_deposit`, `credits_maximum_deposit`, `credits_maximum_credit` | none | ❌ |
| credits | `credits_auto_use` | none | ❌ |
| credits | `credits_on_downgrade` | no downgrades | ❌ |
| theme | `theme` + theme-defined keys | one design system | 🟡 |
| invoices | `bill_to_text` | none | ❌ |
| invoices | `invoice_number`, `invoice_number_padding`, `invoice_number_format` | fixed `INV-`/6 | 🟡 |
| invoices | `invoice_proforma` | none | ❌ |
| invoices | `immutable_invoices_enabled` | always immutable | ✅ stronger |
| invoices | `notes_client_visible` | none | ❌ |
| invoices | `invoice_snapshot` | always | ✅ stronger |
| invoices | `credit_note_number`, `credit_note_number_padding`, `credit_note_number_format` | fixed `CN-` | 🟡 |
| other | `gravatar_default` | no avatars | ❌ |
| other | `default_currency` | none | ❌ |
| other | `registration_disabled` | no registration | ❌ |
| other | `pagination` | fixed page sizes | 🟡 |
| other | `debug` | structured logs | 🟡 |

---

## 9. Notification event mapping

| Paymenter event key | Trigger | Catalyst today | Status |
|---|---|---|---|
| `email_verification` | Registration | `KindEmailVerification` | ✅ |
| `password_reset` | Reset request | `KindPasswordReset` | ✅ |
| `new_invoice_created` | Invoice issued | `KindInvoiceIssued` | ✅ |
| `invoice_paid` | Payment settles | none | ❌ |
| `invoice_payment_failed` | Gateway failure | none (dunning reminder only) | ❌ |
| `new_order_created` | Checkout | none | ❌ |
| `new_server_created` | Provisioning succeeds | none | ❌ |
| `server_suspended` | Dunning suspends | none | ❌ |
| `server_terminated` | Terminate | none | ❌ |
| `service_cancellation_received` | Cancellation request | none | ❌ |
| `new_ticket_message` | Ticket reply | none | ❌ |
| `new_login_detected` | Login from new device | none | ❌ |

Plus `system` email for cron failure and the admin "send test email" action.

---

## 10. Extension inventory

### 10.1 In-tree gateways (4)

Mollie, PayPal, PayPal IPN, Stripe. Each implements `pay`, and optionally
`supportsBillingAgreements`, `createBillingAgreement`, `charge`, `cancelBillingAgreement`,
`supportsRefunds`, `refund` (`app/Classes/Extension/Gateway.php`).

### 10.2 In-tree servers (8)

Convoy, cPanel, DirectAdmin, Enhance, Plesk, Pterodactyl, Virtfusion, Virtualizor. Each
implements `getConfig`, `testConfig`, `getProductConfig`, `createServer`, `suspendServer`,
`unsuspendServer`, `terminateServer`, `upgradeServer`, `getActions`.

### 10.3 In-tree "other" extensions (3)

Affiliates, Announcements, DiscordNotifications. These hook events, navigation, permissions,
admin resources and API permissions.

### 10.4 Marketplace

256 listings: 232 extensions and 24 themes. Gateways include Razorpay, Coinbase, BTCPay,
Paddle, Lemon Squeezy, Mercado Pago, Midtrans, Tripay, YooKassa, Cashfree, PayU, PayNow,
PhonePe, NOWPayments, OxaPay, and manual bank transfer. Servers include Proxmox, Pterodactyl
variants, VirtFusion, Virtualizor cloud, and stock/SSO add-ons.

**Parity scope recommendation.** Match the in-tree set first (4 gateways, 8 servers). The
marketplace is an ecosystem, not a checklist; parity there is "the protocol can host them",
which `P4` proves with two adapters.

---

## 11. API surface comparison

| Capability | Paymenter | Catalyst |
|---|---|---|
| Auth for API | Passport OAuth2 + personal access tokens | Session cookie + scoped API keys |
| Admin REST resources | 10 (`categories`, `credits`, `users`, `products`, `services`, `orders`, `invoices`, `invoice-items`, `tickets`, `ticket-messages`) | 78 operations across 67 paths: customers, invoices, payments, refunds, services, orders, catalog, approvals, audit, api-keys, webhooks, operations, portal |
| Public API | none (admin only) | public catalog + media |
| API docs | Scramble | generated OpenAPI + typed client, drift-checked |
| Scopes | Passport scopes | API key scopes that narrow the owner's permissions |
| Webhooks | gateway routes per extension | `/webhooks/{provider}` with idempotent inbox |
| SSE | none | `/api/v1/events` |
| Error contract | Laravel validation errors | `application/problem+json` with stable codes |

**Gap.** Paymenter's admin API is a CRUD façade over every model; Catalyst's is
operation-oriented. To match "every model is scriptable", add the missing staff CRUD
(customers, coupons, tax rates, currencies, config options, tickets) rather than a generic
table endpoint. Do not add a generic CRUD endpoint: it would bypass the domain rules that make
the current API safe.

---

## 12. Definition of parity

Parity is reached when all of the following are true. This is the checklist to close the
project against, not a wish list.

**Revenue**
- [ ] An anonymous visitor can register, verify, buy, pay and receive a service without staff.
- [ ] At least one real gateway settles a real (test-mode) payment end to end.
- [ ] At least one real provisioner creates, suspends, resumes and terminates a real service.
- [ ] Coupons, tax rates and multiple currencies work on the same invoice.
- [ ] Credit can be deposited, auto-used, partially applied and refunded.
- [ ] Every invoice has a downloadable PDF with a configurable number.

**Service lifecycle**
- [ ] Config options price a service and survive renewal.
- [ ] Upgrades and downgrades prorate, invoice and call the provisioner.
- [ ] Cancellation is a request with a reason and a type, visible to staff.
- [ ] Stock and per-user limits are enforced under lock.

**Support**
- [ ] A customer can open, reply to and close a ticket with attachments.
- [ ] Email piping threads replies into tickets.
- [ ] Every event has an editable notification template and an in-app entry.
- [ ] Users control their notification preferences; push works.

**Identity**
- [ ] Registration, login, 2FA, verification, reset and password change all work for customers.
- [ ] Social login and an OAuth2 server exist.
- [ ] Roles are editable (or a written decision explains why not).
- [ ] Sessions can be listed and revoked; impersonation is audited.

**Operator**
- [ ] System settings (branding, mail, tax, cron, numbering) are editable in the panel.
- [ ] Every Paymenter admin resource has an equivalent, except the deliberate divergences.
- [ ] Email/error/HTTP logs and cron history are viewable.
- [ ] Imports (WHMCS, Paymenter) run with a dry run.
- [ ] The dashboard reports revenue, active customers and open tickets, not only exceptions.

**Platform**
- [ ] A second locale renders the panel and client area.
- [ ] Announcements, affiliates and Discord notifications are available as extensions.
- [ ] The extension system can contribute a nav item and a service action safely.
- [ ] The app reports its version and can check for updates.
- [ ] A marketplace gateway can be installed without a core change.

**Never regress**
- [ ] Money stays exact; no floats.
- [ ] Issued invoices stay immutable.
- [ ] Payment ingestion stays idempotent.
- [ ] Provisioning stays queued and reconciled.
- [ ] The audit log stays append-only.
- [ ] Dual approval stays enforced by the database.

---

## 13. Non-goals and deliberate divergences

Recorded so parity work does not quietly undo a decision:

1. **Mutable issued invoices.** Paymenter can edit them; Catalyst refuses at the database. A
   wrong invoice is corrected with a credit note, never an update.
2. **Bulk delete of financial records.** The triggers exist for a reason. Offer archive, not
   delete.
3. **Float money and `number_format`.** Currency formatting may add symbols and grouping; it
   must not add floats.
4. **In-process extensions.** Paymenter loads PHP classes that can touch the admin UI and the
   database. Catalyst extensions are separate processes with a typed protocol. Add declared
   hooks; do not load code.
5. **Theme marketplace.** The CSP forbids inline styles/scripts and external images by design.
   A theme marketplace is a liability, not a feature.
6. **A page builder / CMS.** The storefront is a fixed, good template over catalog data.
7. **Multi-tenancy, resellers, FX conversion, usage billing, a general ledger.** Out of scope
   until a customer requires them.
8. **River queue.** The ticker over idempotent batches is sufficient; River would add
   prioritisation and per-job retry state, not correctness.
9. **A sandboxed extension runner.** Extensions are trusted operator code until an OS-level
   runner exists. The README says so; parity does not change it.

---

## 14. Appendix: source references

Paymenter (commit `7aaf81e`):

- Client routes: `routes/web.php`; API routes: `routes/api.php`; schedule: `routes/console.php`.
- Client pages: `app/Livewire/**`; views: `themes/default/views/**`.
- Admin: `app/Admin/Resources/**`, `app/Admin/Pages/**`, `app/Admin/Widgets/**`,
  `app/Admin/Clusters/**`.
- Models: `app/Models/**` (52 files); migrations: `database/migrations/**` (75).
- Settings: `app/Classes/Settings.php`; permissions: `config/permissions.php`.
- Notifications: `app/Helpers/NotificationHelper.php`, `database/seeders/EmailTemplateSeeder.php`.
- Cron: `app/Console/Commands/CronJob.php`; email piping: `FetchEmails.php`.
- Extensions: `app/Classes/Extension/**`, `extensions/**`.
- Importer: `app/Console/Commands/ImportFromWhmcs.php`.

Catalyst:

- Route table: `internal/httpapi/routes.go` and the per-feature `register*` files.
- Schema: `internal/store/migrations/0001…0018`.
- Money/calendar/billing: `internal/money`, `internal/calendar`, `internal/billing`.
- Commerce: `internal/orders`, `internal/checkout`, `internal/catalog`, `internal/payments`,
  `internal/refunds`, `internal/creditnote`, `internal/ledger`.
- Lifecycle: `internal/renewal`, `internal/dunning`, `internal/provisioning`,
  `internal/fulfilment`, `internal/retention`.
- Access: `internal/authn`, `internal/httpapi/auth*.go`, `internal/staff`.
- UI: `web/src/views/**`, `web/src/app.ts`, `web/README.md`.
- Generated API: `api/openapi.yaml`, `api/client/`.
