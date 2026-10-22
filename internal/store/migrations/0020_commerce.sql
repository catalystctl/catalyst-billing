-- 0020_commerce.sql
--
-- The commercial surface a shop needs and a ledger does not: promotions, jurisdiction
-- tax, configured currencies, configurable product options, a cart, service cancellation
-- requests, upgrades, support tickets, notification templates and in-app notifications,
-- and the settings table that lets an operator change any of it without a deploy.
--
-- Five decisions shape this file, and each is recorded because the next person will
-- otherwise "simplify" it back into a defect:
--
-- 1. **A coupon is a price modifier with a life, not a discount column.** The renewal
--    engine reads `price_versions`; a coupon that applies for N cycles needs the
--    subscription to carry both the coupon and a cycle counter, and the invoice snapshot
--    must record the discount as a line so the document stays self-explaining. So
--    `coupons` is a dated, limited, product-scoped rule, `coupon_redemptions` counts uses
--    under a lock, and `subscriptions.coupon_id` + `coupon_cycles_applied` make the
--    "first N cycles" rule a fact rather than a guess.
--
-- 2. **Tax is a dated rate table, resolved per jurisdiction.** The `Pricer` interface
--    already takes an exact rate and records evidence, so a rate table is a `Pricer`
--    implementation rather than a new money path. Rates are dated rows: a change appends a
--    new rate effective from a date instead of rewriting a filed period.
--
-- 3. **A currency is configuration; arithmetic stays exact.** `currencies` supplies the
--    symbol, the position and the grouping, and nothing else. Amounts remain integer minor
--    units; no exchange rate is stored, because a price is explicit per currency.
--
-- 4. **A cart is a draft order, not a second pricing engine.** `carts`/`cart_items` hold a
--    selection and its currency; checkout re-prices from the catalog under a lock and
--    writes the same order/invoice rows the staff path writes. A cart line therefore
--    stores *what* was chosen, never *what it costs*.
--
-- 5. **A ticket is a row, and a cancellation is a request.** Both are records a support
--    agent can act on and an auditor can read, rather than an inference from a boolean on
--    another table.
--
-- Append-only and immutability rules from earlier migrations are untouched: nothing here
-- rewrites an issued invoice, a journal entry or an audit row.

BEGIN;

-- =============================================================== currencies ==
--
-- Paymenter has a Currency resource because `number_format` needs a symbol, a position
-- and a grouping separator. Catalyst keeps exact minor units and renders by string
-- insertion, so this table supplies *presentation only* — it can never change arithmetic.
--
-- `scale` is deliberately part of the row: a currency's minor-unit exponent is a property
-- of the currency, not of a setting, and storing it means a JPY amount cannot be rendered
-- with two decimal places by a formatting typo.

CREATE TABLE currencies (
    code        text PRIMARY KEY CHECK (code ~ '^[A-Z0-9]{2,12}$'),
    name        text NOT NULL,
    -- ISO 4217 minor-unit exponent. Matches money.MaxScale.
    scale       int  NOT NULL CHECK (scale BETWEEN 0 AND 4),
    symbol      text NOT NULL DEFAULT '',
    -- 'prefix' renders "$12.34"; 'suffix' renders "12.34 €".
    symbol_position text NOT NULL DEFAULT 'prefix'
                    CHECK (symbol_position IN ('prefix', 'suffix')),
    -- Thousands separator and decimal mark, as characters. A grouping of '' means none.
    group_separator text NOT NULL DEFAULT ',',
    decimal_mark    text NOT NULL DEFAULT '.',
    -- Digits between grouping separators; 0 disables grouping.
    group_size  int  NOT NULL DEFAULT 3 CHECK (group_size BETWEEN 0 AND 4),
    -- Exactly one currency is the default a new price is created in. Enforced by a partial
    -- unique index below rather than by a trigger, so "two defaults" is impossible.
    is_default  boolean NOT NULL DEFAULT false,
    -- A disabled currency keeps its historical prices and invoices but is not offered at
    -- checkout. Deleting one would orphan the price rows that reference it by code.
    enabled     boolean NOT NULL DEFAULT true,
    sort_order  int  NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX currencies_one_default ON currencies (is_default) WHERE is_default;
CREATE INDEX currencies_offer_idx ON currencies (enabled, sort_order, code);

-- The currencies the money package already knows the scale of. Seeding them means a fresh
-- installation can take a payment in USD without an operator configuring anything first;
-- an operator adds or edits rows for anything else. `ON CONFLICT DO NOTHING` keeps a
-- re-run harmless and never overwrites an operator's customisation.
INSERT INTO currencies (code, name, scale, symbol, symbol_position, is_default, sort_order) VALUES
    ('USD', 'US Dollar',        2, '$',  'prefix', true,  10),
    ('EUR', 'Euro',             2, '€',  'prefix', false, 20),
    ('GBP', 'Pound Sterling',   2, '£',  'prefix', false, 30),
    ('CAD', 'Canadian Dollar',  2, 'CA$','prefix', false, 40),
    ('AUD', 'Australian Dollar',2, 'A$', 'prefix', false, 50),
    ('JPY', 'Japanese Yen',     0, '¥',  'prefix', false, 60)
ON CONFLICT (code) DO NOTHING;

-- ==================================================================== tax ====
--
-- A rate is (jurisdiction, tax class, effective window) → an exact rational rate. The
-- `all` country is the wildcard Paymenter uses for "everything not otherwise matched", so
-- a shop can set a domestic rate and a fallback without listing every country.
--
-- `rate` is stored as a text rational ("0.19", "1/5") for the same reason every other rate
-- in this system is: a float cannot represent 0.1 exactly, and a tax rate is money.

CREATE TABLE tax_rates (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name         text NOT NULL,
    -- ISO 3166-1 alpha-2, or 'all' for the wildcard. Upper-case so a lookup is a plain
    -- equality rather than a case-insensitive scan.
    country_code text NOT NULL CHECK (country_code = 'all' OR country_code ~ '^[A-Z]{2}$'),
    -- Optional subdivision (a US state, a Canadian province). Empty means "the whole
    -- country". Paymenter has no subdivision, so this is additive rather than a divergence.
    region_code  text NOT NULL DEFAULT '',
    -- Tax class, so a reduced rate for a category of product is expressible without a
    -- second table. 'standard' is what a row with no class means.
    tax_class    text NOT NULL DEFAULT 'standard',
    rate         text NOT NULL CHECK (rate ~ '^[0-9]+(\.[0-9]+)?$|^[0-9]+/[0-9]+$'),
    -- Inclusive means the rate is contained in the price rather than added to it. It is
    -- per-rate rather than global because an EU price is quoted inclusive and a US price
    -- exclusive, and one installation can sell into both.
    inclusive    boolean NOT NULL DEFAULT false,
    -- Validity window. A rate change closes the old row and opens a new one, so an invoice
    -- issued last quarter can still be explained by the rate that applied then.
    effective_from timestamptz NOT NULL DEFAULT now(),
    effective_to   timestamptz,
    enabled      boolean NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT tax_rates_window CHECK (effective_to IS NULL OR effective_to > effective_from)
);

-- At most one open-ended rate per (jurisdiction, region, class). A second concurrent rate
-- for the same place would make the resolved rate depend on row order.
CREATE UNIQUE INDEX tax_rates_one_open ON tax_rates (country_code, region_code, tax_class)
    WHERE effective_to IS NULL AND enabled;
CREATE INDEX tax_rates_lookup_idx ON tax_rates (country_code, region_code, tax_class, effective_from DESC);

-- ================================================================ coupons ====
--
-- The columns are Paymenter's feature list, each with the reason it exists:
--
--   * `kind` is percentage or fixed, and `value` is interpreted by it: a percentage is a
--     rational ("10" or "12.5"), a fixed amount is minor units in the coupon's currency.
--   * `applies_to` says whether the discount reduces the recurring price, the setup fee or
--     both. A coupon that discounts setup only must not reduce the renewal.
--   * `max_uses` and `max_uses_per_user` are enforced by counting redemptions under a lock,
--     not by a mutable counter that a race can overshoot.
--   * `starts_at`/`expires_at` are the window; a null bound is unbounded.
--   * `recurring_cycles` is the "first N cycles" rule. NULL means lifetime; 1 means the
--     first invoice only. It is read by the renewal engine together with
--     `subscriptions.coupon_cycles_applied`.
--   * `currency` scopes a fixed-value coupon, because "10 off" means nothing without one.

CREATE TABLE coupons (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code         text NOT NULL,
    description  text NOT NULL DEFAULT '',
    kind         text NOT NULL CHECK (kind IN ('percentage', 'fixed')),
    -- A percentage is a rational string; a fixed amount is minor units. Exactly one is used,
    -- decided by `kind`, and the CHECK below refuses the other being set.
    percentage   text CHECK (percentage IS NULL OR percentage ~ '^[0-9]+(\.[0-9]+)?$'),
    amount_minor bigint CHECK (amount_minor IS NULL OR amount_minor > 0),
    -- Required for a fixed coupon, forbidden for a percentage one.
    currency     text CHECK (currency IS NULL OR currency ~ '^[A-Z0-9]{2,12}$'),
    applies_to   text NOT NULL DEFAULT 'all' CHECK (applies_to IN ('price', 'setup', 'all')),
    max_uses            int CHECK (max_uses IS NULL OR max_uses > 0),
    max_uses_per_user   int CHECK (max_uses_per_user IS NULL OR max_uses_per_user > 0),
    starts_at    timestamptz,
    expires_at   timestamptz,
    -- NULL = applies to every cycle; 1 = the first invoice only; N = the first N cycles.
    recurring_cycles int CHECK (recurring_cycles IS NULL OR recurring_cycles > 0),
    -- A retired coupon stops accepting new redemptions but keeps its history, because the
    -- invoices it discounted reference it. Deleting it would make those invoices
    -- unexplainable, so there is no delete.
    enabled      boolean NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT coupons_value_matches_kind CHECK (
        (kind = 'percentage' AND percentage IS NOT NULL AND amount_minor IS NULL)
        OR (kind = 'fixed' AND amount_minor IS NOT NULL AND percentage IS NULL AND currency IS NOT NULL)
    ),
    CONSTRAINT coupons_window CHECK (expires_at IS NULL OR starts_at IS NULL OR expires_at > starts_at)
);

CREATE UNIQUE INDEX coupons_code_unique ON coupons (upper(code));

-- A coupon may be restricted to a set of products. No rows means "every product", which is
-- the common case and keeps the default free of a join.
CREATE TABLE coupon_products (
    coupon_id  uuid NOT NULL REFERENCES coupons (id) ON DELETE CASCADE,
    product_id uuid NOT NULL REFERENCES products (id),
    PRIMARY KEY (coupon_id, product_id)
);

-- One row per use. The unique (coupon_id, invoice_id) is what makes a replayed checkout a
-- no-op instead of a second redemption: the use is recorded against the invoice it
-- discounted, not counted in a column somebody has to remember to increment.
CREATE TABLE coupon_redemptions (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    coupon_id   uuid NOT NULL REFERENCES coupons (id),
    customer_id uuid NOT NULL REFERENCES customers (id),
    invoice_id  uuid REFERENCES invoices (id),
    subscription_id uuid REFERENCES subscriptions (id),
    -- The discount actually granted, in the invoice's currency, so a report does not have
    -- to re-run the pricing to know what a promotion cost.
    discount_minor bigint NOT NULL CHECK (discount_minor >= 0),
    currency    text NOT NULL CHECK (currency ~ '^[A-Z0-9]{2,12}$'),
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX coupon_redemptions_invoice ON coupon_redemptions (coupon_id, invoice_id)
    WHERE invoice_id IS NOT NULL;
CREATE INDEX coupon_redemptions_coupon_idx ON coupon_redemptions (coupon_id, created_at DESC);
CREATE INDEX coupon_redemptions_customer_idx ON coupon_redemptions (coupon_id, customer_id);

-- The subscription carries the coupon and how many cycles it has been applied to. This is
-- what makes "first 2 cycles" a fact: the renewal engine compares the counter to the
-- coupon's `recurring_cycles`, and the invoice snapshot records which cycle it was.
ALTER TABLE subscriptions
    ADD COLUMN coupon_id uuid REFERENCES coupons (id),
    ADD COLUMN coupon_cycles_applied int NOT NULL DEFAULT 0 CHECK (coupon_cycles_applied >= 0);

-- ======================================================= config options ======
--
-- Config options are what a hosting customer actually buys: "8 GB", "2 vCPU", "extra
-- backup". They change the price of a *subscription*, not just an order, so the selection
-- lives on the service and the renewal engine prices from it.
--
-- The shape is Paymenter's: a tree of options per product, each with a type and, per
-- price version, an amount. `env_variable` is how an option reaches the provisioner.

CREATE TABLE config_options (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    product_id    uuid NOT NULL REFERENCES products (id) ON DELETE CASCADE,
    -- Parent/child lets a "backup" option appear only under a chosen plan. Null is a
    -- top-level option.
    parent_id     uuid REFERENCES config_options (id) ON DELETE CASCADE,
    name          text NOT NULL,
    description   text NOT NULL DEFAULT '',
    -- The type decides what the customer sees and what a valid value is.
    option_type   text NOT NULL CHECK (option_type IN
                      ('dropdown', 'radio', 'checkbox', 'text', 'number')),
    -- For a dropdown/radio: the choices. For a checkbox: ["on"]. For text/number: empty.
    choices       jsonb NOT NULL DEFAULT '[]'::jsonb,
    -- Sent to the provisioner as this environment variable when the option is selected.
    env_variable  text NOT NULL DEFAULT '',
    -- A required option must be chosen before checkout.
    required      boolean NOT NULL DEFAULT false,
    -- Hidden options exist for internal use and are not shown at checkout.
    hidden        boolean NOT NULL DEFAULT false,
    -- Upgradable options can be changed on a live service, which issues a prorated invoice.
    upgradable    boolean NOT NULL DEFAULT true,
    sort_order    int NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX config_options_product_idx ON config_options (product_id, sort_order, name);

-- The price of one choice, per price version, because a price version is the immutable
-- thing an invoice prices from. A choice with no row here is free.
CREATE TABLE config_option_prices (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    config_option_id uuid NOT NULL REFERENCES config_options (id) ON DELETE CASCADE,
    price_version_id uuid NOT NULL REFERENCES price_versions (id) ON DELETE CASCADE,
    -- Which choice this price applies to. Empty for a checkbox/text option whose value
    -- carries no price of its own.
    choice           text NOT NULL DEFAULT '',
    amount_minor     bigint NOT NULL DEFAULT 0 CHECK (amount_minor >= 0),
    setup_minor      bigint NOT NULL DEFAULT 0 CHECK (setup_minor >= 0),
    CONSTRAINT config_option_prices_unique UNIQUE (config_option_id, price_version_id, choice)
);

-- The selection stored on a service. This is what the renewal engine reads to price the
-- next period, and what the provisioner reads to build the environment.
CREATE TABLE service_configs (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id  uuid NOT NULL REFERENCES services (id) ON DELETE CASCADE,
    config_option_id uuid NOT NULL REFERENCES config_options (id),
    -- The chosen value: the choice text, the number, or the free text.
    value       text NOT NULL,
    -- The price this selection contributed, captured at selection time so a later option
    -- price change does not silently re-price an existing service.
    amount_minor bigint NOT NULL DEFAULT 0 CHECK (amount_minor >= 0),
    setup_minor  bigint NOT NULL DEFAULT 0 CHECK (setup_minor >= 0),
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT service_configs_unique UNIQUE (service_id, config_option_id)
);

-- ============================================================= product rules ==
--
-- Stock, per-user limits and quantity policy. A null stock is unlimited, which is the
-- difference between "no row" and "zero" that a customer would notice.

ALTER TABLE products
    ADD COLUMN stock          int CHECK (stock IS NULL OR stock >= 0),
    ADD COLUMN per_user_limit int CHECK (per_user_limit IS NULL OR per_user_limit > 0),
    -- disabled: one per order, no quantity field. separated: N independent services.
    -- combined: N on one service.
    ADD COLUMN allow_quantity text NOT NULL DEFAULT 'disabled'
        CHECK (allow_quantity IN ('disabled', 'separated', 'combined')),
    -- A product's provisioner and activation snippet. The provisioner is an extension
    -- name; a null means "the installation default".
    ADD COLUMN server_extension text NOT NULL DEFAULT '',
    ADD COLUMN email_template   text NOT NULL DEFAULT '';

-- The upgrade graph: which products a customer may move to from this one. A downgrade is
-- the same edge traversed in reverse, so there is one table rather than two.
CREATE TABLE product_upgrades (
    product_id      uuid NOT NULL REFERENCES products (id) ON DELETE CASCADE,
    upgrade_to_id   uuid NOT NULL REFERENCES products (id) ON DELETE CASCADE,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT product_upgrades_not_self CHECK (product_id <> upgrade_to_id),
    PRIMARY KEY (product_id, upgrade_to_id)
);

-- =================================================================== cart ====
--
-- A cart holds a *selection*, not a price. Checkout re-prices from the catalog under a
-- lock, which is why no money column appears here: a stored price is a price a customer can
-- hold the shop to after the catalog changed.
--
-- The cart's currency is fixed once it has an item, because mixing currencies in one order
-- is meaningless without an exchange rate and this system deliberately has none.

CREATE TABLE carts (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id uuid NOT NULL REFERENCES customers (id) ON DELETE CASCADE,
    currency    text NOT NULL CHECK (currency ~ '^[A-Z0-9]{2,12}$'),
    coupon_id   uuid REFERENCES coupons (id),
    -- Terms acceptance is recorded against the cart so checkout can refuse a cart whose
    -- acceptance predates a change to the terms.
    tos_accepted_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    -- One open cart per customer. A second "add to cart" joins the first rather than
    -- silently creating a parallel cart the customer cannot see.
    CONSTRAINT carts_one_per_customer UNIQUE (customer_id)
);

CREATE TABLE cart_items (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    cart_id          uuid NOT NULL REFERENCES carts (id) ON DELETE CASCADE,
    price_version_id uuid NOT NULL REFERENCES price_versions (id),
    quantity         int  NOT NULL DEFAULT 1 CHECK (quantity > 0 AND quantity <= 1000),
    -- The chosen config options, as {config_option_id: value}. Validated against the
    -- product's options at checkout; stored as JSON because it is a selection, not a
    -- relational fact, and it is re-validated rather than trusted.
    config           jsonb NOT NULL DEFAULT '{}'::jsonb,
    -- A human-readable label the customer set ("my web server"), carried onto the service.
    label            text NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL DEFAULT now(),
    -- The same price version added twice is one line with a larger quantity, which is what
    -- `allow_quantity = combined` means. A `separated` product is added as two rows with
    -- different configs, so the key includes the config.
    CONSTRAINT cart_items_unique UNIQUE (cart_id, price_version_id, config)
);

-- ====================================================== service lifecycle ====
--
-- A cancellation is a request row: it has a reason, a type and an actor, and staff can see
-- the queue. Paymenter stores the same thing; Catalyst additionally keeps it auditable,
-- which is why it is a table rather than a boolean on the subscription.

CREATE TABLE service_cancellations (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id   uuid NOT NULL REFERENCES services (id),
    -- requested_by is the user who asked; a staff-initiated cancellation records the
    -- operator, and the audit log records both.
    requested_by uuid REFERENCES users (id),
    reason       text NOT NULL DEFAULT '' CHECK (length(reason) <= 255),
    cancellation_type text NOT NULL CHECK (cancellation_type IN ('immediate', 'end_of_period')),
    status       text NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'scheduled', 'executed', 'cancelled', 'rejected')),
    -- When the cancellation takes effect. For end_of_period this is the paid-through date.
    effective_at timestamptz,
    executed_at  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX service_cancellations_queue_idx ON service_cancellations (status, created_at DESC);
CREATE INDEX service_cancellations_service_idx ON service_cancellations (service_id, created_at DESC);

-- An upgrade is a priced change to a service: from one price version to another, with the
-- prorated amount and the invoice that charges it. The row exists so a half-applied upgrade
-- (invoice issued, server not yet changed) is visible and recoverable.
CREATE TABLE service_upgrades (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id        uuid NOT NULL REFERENCES services (id),
    from_price_version_id uuid REFERENCES price_versions (id),
    to_price_version_id   uuid NOT NULL REFERENCES price_versions (id),
    -- The prorated charge (positive) or credit (negative) for the remainder of the period.
    prorated_minor    bigint NOT NULL DEFAULT 0,
    currency          text NOT NULL CHECK (currency ~ '^[A-Z0-9]{2,12}$'),
    invoice_id        uuid REFERENCES invoices (id),
    -- pending: invoice unpaid. applied: the service now runs the new version.
    -- cancelled: the pending invoice was voided before payment.
    status            text NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending', 'applied', 'cancelled', 'failed')),
    created_at        timestamptz NOT NULL DEFAULT now(),
    applied_at        timestamptz
);

CREATE INDEX service_upgrades_pending_idx ON service_upgrades (status, created_at)
    WHERE status = 'pending';

-- ================================================================ tickets ====
--
-- The permissions already promise this (ticket:read:self / ticket:write:self have been
-- granted to customers since the first auth migration and gated nothing). These tables are
-- what make the promise true.

CREATE TABLE tickets (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Human-facing reference, allocated from the same gap-free mechanism invoices use.
    number       text UNIQUE,
    customer_id  uuid NOT NULL REFERENCES customers (id),
    user_id      uuid REFERENCES users (id),
    -- The service a ticket is about, when it is about one.
    service_id   uuid REFERENCES services (id),
    subject      text NOT NULL CHECK (length(subject) > 0 AND length(subject) <= 200),
    department   text NOT NULL DEFAULT 'general',
    priority     text NOT NULL DEFAULT 'normal' CHECK (priority IN ('low', 'normal', 'high')),
    status       text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'replied', 'closed')),
    assigned_to  uuid REFERENCES users (id),
    -- The customer who opens a ticket is the one who may read it. Denormalised so the
    -- scoping predicate is a plain equality rather than a join through customer_accounts.
    opened_by    uuid REFERENCES users (id),
    last_reply_at timestamptz,
    closed_at    timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX tickets_customer_idx ON tickets (customer_id, created_at DESC);
CREATE INDEX tickets_queue_idx ON tickets (status, priority, created_at DESC);
CREATE INDEX tickets_assignee_idx ON tickets (assigned_to, status) WHERE assigned_to IS NOT NULL;
-- Auto-close scans this: a ticket with no reply for N days.
CREATE INDEX tickets_stale_idx ON tickets (last_reply_at) WHERE status <> 'closed';

CREATE TABLE ticket_messages (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ticket_id  uuid NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    -- Exactly one author is set: a user (customer or staff) or nothing (a system note).
    author_user_id uuid REFERENCES users (id),
    -- True when the author is staff. Denormalised so rendering a thread does not need the
    -- role table, and so a role change does not rewrite history.
    from_staff boolean NOT NULL DEFAULT false,
    body       text NOT NULL CHECK (length(body) > 0),
    -- Email piping records the Message-ID it imported, so a duplicate delivery is ignored.
    message_id text,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ticket_messages_thread_idx ON ticket_messages (ticket_id, created_at);
CREATE UNIQUE INDEX ticket_messages_message_id ON ticket_messages (message_id)
    WHERE message_id IS NOT NULL;

-- Attachments live in the database for the same reason product images do: a container
-- restart must not lose them, and the CSP means a download is served by our own endpoint
-- with a checked content type.
CREATE TABLE ticket_attachments (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id uuid NOT NULL REFERENCES ticket_messages (id) ON DELETE CASCADE,
    filename   text NOT NULL,
    content_type text NOT NULL,
    bytes      bytea NOT NULL,
    byte_size  int NOT NULL CHECK (byte_size > 0 AND byte_size <= 10485760),
    digest     text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ticket_attachments_message_idx ON ticket_attachments (message_id);

-- The mail log Paymenter keeps: what was sent, to whom, and whether it worked. It is
-- distinct from notification_deliveries (the outbox) because a piped inbound message also
-- belongs here and never went through the outbox.
CREATE TABLE ticket_mail_logs (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id text,
    subject    text NOT NULL DEFAULT '',
    from_address text NOT NULL DEFAULT '',
    to_address   text NOT NULL DEFAULT '',
    body       text NOT NULL DEFAULT '',
    status     text NOT NULL DEFAULT 'received'
               CHECK (status IN ('received', 'sent', 'failed', 'ignored')),
    error      text,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ticket_mail_logs_message_idx ON ticket_mail_logs (message_id);

-- ========================================================== notifications ====
--
-- The outbox (notification_deliveries) is already the hard part and it is done. These are
-- the two things it lacks: an editable template per event, and an in-app inbox.

CREATE TABLE notification_templates (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The event key, e.g. 'invoice_paid'. Unique: one template per event.
    event_key   text NOT NULL,
    name        text NOT NULL,
    subject     text NOT NULL DEFAULT '',
    body        text NOT NULL DEFAULT '',
    -- In-app title/body/url, separate from the mail body because a push is not an email.
    in_app_title text NOT NULL DEFAULT '',
    in_app_body  text NOT NULL DEFAULT '',
    in_app_url   text NOT NULL DEFAULT '',
    -- Per-template channel toggles. `enabled` is the master switch.
    enabled         boolean NOT NULL DEFAULT true,
    mail_enabled    boolean NOT NULL DEFAULT true,
    in_app_enabled  boolean NOT NULL DEFAULT true,
    -- CC/BCC for the mail channel.
    cc_addresses  text NOT NULL DEFAULT '',
    bcc_addresses text NOT NULL DEFAULT '',
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX notification_templates_event ON notification_templates (event_key);

-- Per-user channel preference. 'force' means the user cannot opt out (a password reset);
-- 'choice_on'/'choice_off' is the user's own choice; 'never' means never send.
CREATE TABLE notification_preferences (
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    event_key  text NOT NULL,
    preference text NOT NULL CHECK (preference IN ('force', 'choice_on', 'choice_off', 'never')),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, event_key)
);

-- The in-app inbox. A row per user per event, with a read flag and a deep link.
CREATE TABLE notifications (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    event_key  text NOT NULL,
    title      text NOT NULL,
    body       text NOT NULL DEFAULT '',
    url        text NOT NULL DEFAULT '',
    read_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX notifications_inbox_idx ON notifications (user_id, created_at DESC);
CREATE INDEX notifications_unread_idx ON notifications (user_id) WHERE read_at IS NULL;

-- ================================================================= settings ==
--
-- One table, typed by group and key, so an operator can change branding, mail, tax mode,
-- cron thresholds and document numbering without a deploy. Every value is text and is
-- parsed by its reader: a settings table with typed columns per setting is a migration per
-- setting, which is how a settings page becomes a schema.
--
-- The group/key pair is the primary key, and `is_secret` marks values that must be
-- redacted from every read path — a gateway secret is a setting, and a settings page that
-- echoes it back is a credential leak.

CREATE TABLE settings (
    group_key  text NOT NULL,
    key        text NOT NULL,
    value      text NOT NULL DEFAULT '',
    -- JSON-encoded for structured values (a department list, a logo reference).
    is_secret  boolean NOT NULL DEFAULT false,
    updated_by uuid REFERENCES users (id),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (group_key, key)
);

CREATE INDEX settings_group_idx ON settings (group_key, key);

-- The defaults an installation needs to render a storefront and send a mail. They are
-- inserted here rather than read from the environment so the settings page has something
-- to show and the operator has something to change. A key absent from this seed is read
-- with a code-side default, so adding one is not a migration.
INSERT INTO settings (group_key, key, value) VALUES
    ('general',  'company_name',        'Catalyst Hosting'),
    ('general',  'system_email_address','no-reply@localhost'),
    ('general',  'tos',                 ''),
    ('general',  'registration_disabled','false'),
    ('general',  'pagination',          '25'),
    ('invoices', 'bill_to_text',        ''),
    ('invoices', 'notes_client_visible','true'),
    ('invoices', 'invoice_number_prefix','INV-'),
    ('invoices', 'invoice_number_padding','6'),
    ('invoices', 'credit_note_prefix',  'CN-'),
    ('invoices', 'credit_note_padding', '6'),
    ('invoices', 'due_days',            '7'),
    ('invoices', 'proforma',            'false'),
    ('credits',  'enabled',             'true'),
    ('credits',  'minimum_deposit_minor','500'),
    ('credits',  'maximum_deposit_minor','100000'),
    ('credits',  'maximum_credit_minor','1000000'),
    ('credits',  'auto_use',            'true'),
    ('tax',      'enabled',             'false'),
    ('tax',      'default_mode',        'exclusive'),
    ('tickets',  'disabled',            'false'),
    ('tickets',  'client_closing_disabled','false'),
    ('tickets',  'departments',         '["general","billing","technical"]'),
    ('tickets',  'autoclose_days',      '14'),
    ('cronjob',  'order_cancel_days',   '14'),
    ('cronjob',  'terminate_days',      '30'),
    ('mail',     'disable',             'false'),
    ('mail',     'must_verify',         'false'),
    ('announcements','enabled',         'true')
ON CONFLICT (group_key, key) DO NOTHING;

-- ============================================================ announcements ==
--
-- Paymenter ships this as an extension; it is four columns, so it is a table and a screen
-- rather than a plugin.

CREATE TABLE announcements (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    title      text NOT NULL,
    body       text NOT NULL DEFAULT '',
    -- Where it is shown: the storefront, the client dashboard, or both.
    placement  text NOT NULL DEFAULT 'both' CHECK (placement IN ('storefront', 'dashboard', 'both')),
    published  boolean NOT NULL DEFAULT false,
    published_at timestamptz,
    expires_at timestamptz,
    created_by uuid REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX announcements_live_idx ON announcements (published, published_at DESC);

-- =================================================================== cron ====
--
-- A row per job run, so the operator can see whether the renewals actually ran rather than
-- inferring it from an empty inbox. Paymenter calls this cron_stats.

CREATE TABLE cron_runs (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    job         text NOT NULL,
    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    outcome     text NOT NULL DEFAULT 'running'
                CHECK (outcome IN ('running', 'ok', 'failed', 'skipped')),
    detail      text NOT NULL DEFAULT '',
    items       int NOT NULL DEFAULT 0
);

CREATE INDEX cron_runs_job_idx ON cron_runs (job, started_at DESC);

-- The heartbeat a supervisor reads. One row, upserted by the worker.
CREATE TABLE worker_heartbeat (
    id         text PRIMARY KEY,
    beat_at    timestamptz NOT NULL DEFAULT now(),
    jobs       int NOT NULL DEFAULT 0,
    version    text NOT NULL DEFAULT ''
);

-- ==================================================================== logs ====
--
-- The error and HTTP log viewers Paymenter has. They are tables rather than only stdout
-- because an operator with no log aggregator still needs to answer "what failed last
-- night" from the panel.

CREATE TABLE error_logs (
    id         bigserial PRIMARY KEY,
    level      text NOT NULL DEFAULT 'error',
    source     text NOT NULL DEFAULT '',
    message    text NOT NULL,
    detail     text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX error_logs_recent_idx ON error_logs (created_at DESC);

CREATE TABLE http_logs (
    id          bigserial PRIMARY KEY,
    method      text NOT NULL,
    path        text NOT NULL,
    status      int NOT NULL,
    duration_ms int NOT NULL DEFAULT 0,
    actor_id    uuid,
    ip          inet,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX http_logs_recent_idx ON http_logs (created_at DESC);

-- ============================================================== invoices =====
--
-- Three additive columns for parity: a client-visible note, a void reason, and a proforma
-- flag. The proforma flag is a *column* rather than a status because the status CHECK is
-- what ties a number to a non-draft invoice: a proforma has no number, and adding a status
-- would mean weakening that constraint.

ALTER TABLE invoices
    ADD COLUMN client_note text NOT NULL DEFAULT '',
    ADD COLUMN void_reason text NOT NULL DEFAULT '',
    -- A proforma is issued but unnumbered; it becomes a real invoice on payment.
    ADD COLUMN proforma boolean NOT NULL DEFAULT false;

-- A proforma must not carry a number: that is the whole point of calling it provisional.
ALTER TABLE invoices
    ADD CONSTRAINT invoices_proforma_unnumbered CHECK (NOT proforma OR number IS NULL);

-- Payments carry the provider fee so a reconciliation can report net, not just gross.
ALTER TABLE payments
    ADD COLUMN fee_minor bigint NOT NULL DEFAULT 0 CHECK (fee_minor >= 0);

-- =========================================================== billing profile ==
--
-- The free-text bill-to block and the tax fields the tax engine reads.

ALTER TABLE billing_profiles
    ADD COLUMN bill_to_text text NOT NULL DEFAULT '',
    ADD COLUMN tax_exempt boolean NOT NULL DEFAULT false,
    ADD COLUMN tax_exemption_reason text NOT NULL DEFAULT '';

COMMIT;
