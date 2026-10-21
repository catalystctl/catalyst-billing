-- 0001_core.sql
--
-- Core schema for the billing vertical slice: catalog snapshot -> invoice ->
-- payment allocation -> balanced subledger -> provisioning operation.
--
-- Applied by the migration runner inside one transaction per file; do not add
-- BEGIN/COMMIT here.
--
-- Invariants enforced here rather than in application code, because a bug in
-- one code path must not be able to corrupt money:
--
--   * amounts are integer minor units plus an explicit currency, never floats;
--   * a journal transaction must balance (deferred constraint trigger, so
--     entries can be inserted in any order inside one transaction);
--   * journal entries and journal transactions are append-only;
--   * an issued invoice's money and number can never change;
--   * one renewal invoice per subscription per billing period;
--   * one payment row per provider payment id, so replayed webhooks cannot
--     double-credit;
--   * one provisioning operation per logical operation key.


CREATE TABLE ledger_accounts (
    code       text PRIMARY KEY,
    kind       text NOT NULL CHECK (kind IN ('asset', 'liability', 'income', 'expense')),
    name       text NOT NULL
);

INSERT INTO ledger_accounts (code, kind, name) VALUES
    ('ar',              'asset',     'Accounts receivable'),
    ('cash_clearing',   'asset',     'Payment clearing'),
    ('customer_credit', 'liability', 'Customer credit balance'),
    ('tax_payable',     'liability', 'Tax payable'),
    ('revenue',         'income',    'Service revenue'),
    ('refunds',         'expense',   'Refunds and chargebacks');

-- ---------------------------------------------------------------- customers --

CREATE TABLE customers (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email      text NOT NULL,
    name       text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Case-insensitive uniqueness: 'A@x.test' and 'a@x.test' are one account.
-- Expressed as an index because a table constraint cannot contain an
-- expression.
CREATE UNIQUE INDEX customers_email_unique ON customers (lower(email));

-- ------------------------------------------------------------------ catalog --

CREATE TABLE products (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug       text NOT NULL UNIQUE,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Prices are versioned and never edited in place: an invoice snapshots the
-- price version it was issued from, so later catalog edits cannot rewrite
-- history.
CREATE TABLE price_versions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    product_id      uuid NOT NULL REFERENCES products (id),
    version         int  NOT NULL,
    currency        text NOT NULL CHECK (currency ~ '^[A-Z0-9]{2,12}$'),
    amount_minor    bigint NOT NULL CHECK (amount_minor >= 0),
    setup_minor     bigint NOT NULL DEFAULT 0 CHECK (setup_minor >= 0),
    interval        text NOT NULL CHECK (interval IN ('monthly', 'quarterly', 'semiannual', 'annual')),
    anchor_day      int  NOT NULL CHECK (anchor_day BETWEEN 1 AND 31),
    anchor_timezone text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT price_versions_unique UNIQUE (product_id, version)
);

-- ------------------------------------------------------------------- orders --

CREATE TABLE orders (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id   uuid NOT NULL REFERENCES customers (id),
    status        text NOT NULL CHECK (status IN
                      ('pending_payment', 'paid', 'fulfilling', 'completed', 'cancelled', 'needs_review')),
    currency      text NOT NULL CHECK (currency ~ '^[A-Z0-9]{2,12}$'),
    total_minor   bigint NOT NULL CHECK (total_minor >= 0),
    version       bigint NOT NULL DEFAULT 1,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE order_items (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id         uuid NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    price_version_id uuid NOT NULL REFERENCES price_versions (id),
    description      text NOT NULL,
    quantity         int    NOT NULL CHECK (quantity > 0),
    unit_minor       bigint NOT NULL CHECK (unit_minor >= 0),
    line_minor       bigint NOT NULL CHECK (line_minor >= 0),
    setup_minor      bigint NOT NULL DEFAULT 0 CHECK (setup_minor >= 0),
    position         int    NOT NULL
);

-- ---------------------------------------------------------------- invoicing --

-- Numbers are allocated under a row lock inside the issuing transaction, so a
-- number is never handed out twice and never recycled after a rollback.
CREATE TABLE invoice_number_sequences (
    scope      text PRIMARY KEY,
    next_value bigint NOT NULL CHECK (next_value > 0)
);

CREATE TABLE subscriptions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id     uuid NOT NULL REFERENCES customers (id),
    price_version_id uuid NOT NULL REFERENCES price_versions (id),
    status          text NOT NULL CHECK (status IN ('pending', 'active', 'past_due', 'cancelled', 'ended')),
    interval        text NOT NULL CHECK (interval IN ('monthly', 'quarterly', 'semiannual', 'annual')),
    anchor_day      int  NOT NULL CHECK (anchor_day BETWEEN 1 AND 31),
    anchor_hour     int  NOT NULL DEFAULT 0 CHECK (anchor_hour BETWEEN 0 AND 23),
    anchor_minute   int  NOT NULL DEFAULT 0 CHECK (anchor_minute BETWEEN 0 AND 59),
    timezone        text NOT NULL,
    -- Denormalised next billing instant so the due-work scanner is an index
    -- scan rather than a per-row calendar computation.
    current_period_start timestamptz NOT NULL,
    current_period_end   timestamptz NOT NULL,
    next_billing_at      timestamptz NOT NULL,
    version              bigint NOT NULL DEFAULT 1,
    created_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT subscriptions_period_order CHECK (current_period_start < current_period_end)
);

CREATE TABLE invoices (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    number           text UNIQUE,
    customer_id      uuid NOT NULL REFERENCES customers (id),
    order_id         uuid REFERENCES orders (id),
    subscription_id  uuid REFERENCES subscriptions (id),
    -- Identifies the billing period this renewal invoice belongs to. The
    -- partial unique index below makes double invoicing impossible even when
    -- two workers race.
    period_key       text,
    status           text NOT NULL CHECK (status IN ('draft', 'open', 'paid', 'void', 'uncollectible')),
    currency         text NOT NULL CHECK (currency ~ '^[A-Z0-9]{2,12}$'),
    subtotal_minor   bigint NOT NULL CHECK (subtotal_minor >= 0),
    tax_minor        bigint NOT NULL DEFAULT 0 CHECK (tax_minor >= 0),
    total_minor      bigint NOT NULL CHECK (total_minor >= 0),
    amount_paid_minor bigint NOT NULL DEFAULT 0 CHECK (amount_paid_minor >= 0),
    issued_at        timestamptz,
    due_at           timestamptz,
    -- Immutable snapshot of everything needed to reprint the document exactly
    -- as issued: merchant, customer, prices, tax evidence, terms.
    snapshot         jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT invoices_total_matches CHECK (total_minor = subtotal_minor + tax_minor),
    CONSTRAINT invoices_paid_within_total CHECK (amount_paid_minor <= total_minor),
    CONSTRAINT invoices_issued_consistency CHECK (
        (status = 'draft' AND number IS NULL AND issued_at IS NULL)
        OR (status <> 'draft')
    )
);

CREATE UNIQUE INDEX invoices_one_per_period
    ON invoices (subscription_id, period_key)
    WHERE subscription_id IS NOT NULL AND period_key IS NOT NULL AND status <> 'void';

CREATE INDEX invoices_customer_idx ON invoices (customer_id, created_at DESC);
CREATE INDEX invoices_open_due_idx ON invoices (due_at) WHERE status = 'open';

CREATE TABLE invoice_lines (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    invoice_id   uuid NOT NULL REFERENCES invoices (id) ON DELETE CASCADE,
    position     int NOT NULL,
    description  text NOT NULL,
    quantity     numeric(20, 6) NOT NULL CHECK (quantity > 0),
    unit_minor   bigint NOT NULL CHECK (unit_minor >= 0),
    net_minor    bigint NOT NULL CHECK (net_minor >= 0),
    tax_rate     text NOT NULL DEFAULT '0',
    tax_minor    bigint NOT NULL DEFAULT 0 CHECK (tax_minor >= 0),
    total_minor  bigint NOT NULL CHECK (total_minor >= 0),
    CONSTRAINT invoice_lines_total CHECK (total_minor = net_minor + tax_minor),
    CONSTRAINT invoice_lines_unique_position UNIQUE (invoice_id, position)
);

-- ---------------------------------------------------------------- payments --

CREATE TABLE payment_attempts (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    invoice_id       uuid NOT NULL REFERENCES invoices (id),
    gateway          text NOT NULL,
    -- Stable provider idempotency key. Unique so a retried "create checkout"
    -- can never create a second provider-side charge.
    operation_key    text NOT NULL UNIQUE,
    status           text NOT NULL CHECK (status IN
                         ('created', 'pending', 'requires_action', 'succeeded', 'failed', 'cancelled')),
    amount_minor     bigint NOT NULL CHECK (amount_minor >= 0),
    currency         text NOT NULL CHECK (currency ~ '^[A-Z0-9]{2,12}$'),
    provider_ref     text,
    redirect_url     text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX payment_attempts_invoice_idx ON payment_attempts (invoice_id);

CREATE TABLE payments (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    provider          text NOT NULL,
    provider_account  text NOT NULL,
    provider_payment_id text NOT NULL,
    amount_minor      bigint NOT NULL CHECK (amount_minor > 0),
    currency          text NOT NULL CHECK (currency ~ '^[A-Z0-9]{2,12}$'),
    status            text NOT NULL CHECK (status IN ('succeeded', 'refunded', 'partially_refunded', 'disputed')),
    received_at       timestamptz NOT NULL DEFAULT now(),
    -- The provider's payment id is the authoritative identity. This constraint,
    -- not webhook deduplication, is what prevents two distinct events about the
    -- same payment from crediting twice.
    CONSTRAINT payments_provider_unique UNIQUE (provider, provider_account, provider_payment_id)
);

CREATE TABLE payment_allocations (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    payment_id  uuid NOT NULL REFERENCES payments (id),
    invoice_id  uuid NOT NULL REFERENCES invoices (id),
    amount_minor bigint NOT NULL CHECK (amount_minor > 0),
    currency    text NOT NULL CHECK (currency ~ '^[A-Z0-9]{2,12}$'),
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT payment_allocations_unique UNIQUE (payment_id, invoice_id)
);

CREATE TABLE refunds (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    payment_id         uuid NOT NULL REFERENCES payments (id),
    provider_refund_id text,
    operation_key      text NOT NULL UNIQUE,
    amount_minor       bigint NOT NULL CHECK (amount_minor > 0),
    currency           text NOT NULL CHECK (currency ~ '^[A-Z0-9]{2,12}$'),
    status             text NOT NULL CHECK (status IN
                           ('created', 'pending', 'succeeded', 'failed', 'unknown', 'needs_review')),
    created_at         timestamptz NOT NULL DEFAULT now()
);

-- -------------------------------------------------------------- webhook inbox --

CREATE TABLE gateway_events (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    provider          text NOT NULL,
    provider_account  text NOT NULL,
    event_id          text NOT NULL,
    event_type        text NOT NULL,
    payload           jsonb NOT NULL,
    signature_valid   boolean NOT NULL,
    received_at       timestamptz NOT NULL DEFAULT now(),
    processed_at      timestamptz,
    attempts          int NOT NULL DEFAULT 0,
    last_error        text,
    CONSTRAINT gateway_events_unique UNIQUE (provider, provider_account, event_id)
);

CREATE INDEX gateway_events_pending_idx ON gateway_events (received_at)
    WHERE processed_at IS NULL;

-- ------------------------------------------------------------ provisioning --

CREATE TABLE services (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id    uuid NOT NULL REFERENCES customers (id),
    subscription_id uuid REFERENCES subscriptions (id),
    product_id     uuid NOT NULL REFERENCES products (id),
    status         text NOT NULL CHECK (status IN
                       ('pending', 'provisioning', 'active', 'suspended', 'terminating', 'terminated', 'error')),
    external_id    text,
    config         jsonb NOT NULL DEFAULT '{}'::jsonb,
    -- Fencing token: a worker may only apply a destructive transition if its
    -- generation is still the current one.
    generation     bigint NOT NULL DEFAULT 1,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE service_operations (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id     uuid NOT NULL REFERENCES services (id),
    kind           text NOT NULL CHECK (kind IN
                       ('create', 'inspect', 'suspend', 'resume', 'terminate', 'resize')),
    -- Logical operation identity. Retries reuse it, so a worker restart cannot
    -- turn one create into two servers.
    operation_key  text NOT NULL UNIQUE,
    generation     bigint NOT NULL,
    status         text NOT NULL CHECK (status IN
                       ('queued', 'running', 'succeeded', 'retry_wait', 'unknown', 'failed', 'needs_review')),
    attempts       int NOT NULL DEFAULT 0,
    last_error     text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    started_at     timestamptz,
    finished_at    timestamptz
);

CREATE INDEX service_operations_open_idx ON service_operations (status)
    WHERE status IN ('queued', 'running', 'retry_wait', 'unknown');

-- ------------------------------------------------------------------ ledger --

CREATE TABLE journal_transactions (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind           text NOT NULL CHECK (kind IN
                       ('invoice_issued', 'payment_received', 'refund', 'credit_note', 'adjustment')),
    reference_type text NOT NULL,
    reference_id   uuid NOT NULL,
    currency       text NOT NULL CHECK (currency ~ '^[A-Z0-9]{2,12}$'),
    created_at     timestamptz NOT NULL DEFAULT now(),
    -- One posting per economic event: replaying it is a no-op, not a second
    -- posting.
    CONSTRAINT journal_transactions_unique UNIQUE (kind, reference_type, reference_id)
);

CREATE TABLE journal_entries (
    id             bigserial PRIMARY KEY,
    transaction_id uuid NOT NULL REFERENCES journal_transactions (id),
    account_code   text NOT NULL REFERENCES ledger_accounts (code),
    currency       text NOT NULL CHECK (currency ~ '^[A-Z0-9]{2,12}$'),
    debit_minor    bigint NOT NULL DEFAULT 0 CHECK (debit_minor >= 0),
    credit_minor   bigint NOT NULL DEFAULT 0 CHECK (credit_minor >= 0),
    memo           text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    -- Exactly one side of each entry is populated.
    CONSTRAINT journal_entries_one_side CHECK (
        (debit_minor > 0 AND credit_minor = 0) OR (credit_minor > 0 AND debit_minor = 0)
    )
);

CREATE INDEX journal_entries_transaction_idx ON journal_entries (transaction_id);

-- Append-only: corrections are reversals, never edits.
CREATE OR REPLACE FUNCTION reject_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% is append-only (attempted %)', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER journal_entries_append_only
    BEFORE UPDATE OR DELETE ON journal_entries
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

CREATE TRIGGER journal_transactions_append_only
    BEFORE UPDATE OR DELETE ON journal_transactions
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- Balance is validated at COMMIT, which lets a posting service insert both
-- sides of a transaction without ordering constraints while making an
-- unbalanced transaction impossible to commit.
CREATE OR REPLACE FUNCTION assert_journal_balanced() RETURNS trigger AS $$
DECLARE
    tx_id      uuid;
    debits     bigint;
    credits    bigint;
    currencies int;
BEGIN
    tx_id := COALESCE(NEW.transaction_id, OLD.transaction_id);

    SELECT COALESCE(SUM(debit_minor), 0),
           COALESCE(SUM(credit_minor), 0),
           COUNT(DISTINCT currency)
      INTO debits, credits, currencies
      FROM journal_entries
     WHERE transaction_id = tx_id;

    IF currencies > 1 THEN
        RAISE EXCEPTION 'journal transaction % mixes currencies', tx_id
            USING ERRCODE = 'check_violation';
    END IF;
    IF debits <> credits THEN
        RAISE EXCEPTION 'journal transaction % is unbalanced: debits % <> credits %',
            tx_id, debits, credits
            USING ERRCODE = 'check_violation';
    END IF;
    IF debits = 0 THEN
        RAISE EXCEPTION 'journal transaction % has no entries', tx_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER journal_entries_balanced
    AFTER INSERT OR UPDATE ON journal_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION assert_journal_balanced();

-- The entry-level trigger above never fires for a transaction with no entries,
-- which would let an empty transaction commit. This second trigger is attached
-- to the transaction itself so "a transaction must have balanced entries" holds
-- even when there are none. Both are deferred, so entries may be inserted in any
-- order after their parent.
CREATE OR REPLACE FUNCTION assert_journal_transaction_has_entries() RETURNS trigger AS $$
DECLARE
    entries bigint;
BEGIN
    SELECT count(*) INTO entries FROM journal_entries WHERE transaction_id = NEW.id;
    IF entries = 0 THEN
        RAISE EXCEPTION 'journal transaction % has no entries', NEW.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER journal_transactions_have_entries
    AFTER INSERT ON journal_transactions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION assert_journal_transaction_has_entries();

-- ------------------------------------------------------- document integrity --

-- A draft invoice may be edited. Once issued, its money and identity are frozen;
-- corrections must be credit notes or voids.
CREATE OR REPLACE FUNCTION guard_issued_invoice() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.status <> 'draft' THEN
            RAISE EXCEPTION 'issued invoice % cannot be deleted', OLD.id
                USING ERRCODE = 'restrict_violation';
        END IF;
        RETURN OLD;
    END IF;

    IF OLD.status <> 'draft' THEN
        IF NEW.number IS DISTINCT FROM OLD.number
           OR NEW.currency IS DISTINCT FROM OLD.currency
           OR NEW.subtotal_minor IS DISTINCT FROM OLD.subtotal_minor
           OR NEW.tax_minor IS DISTINCT FROM OLD.tax_minor
           OR NEW.total_minor IS DISTINCT FROM OLD.total_minor
           OR NEW.issued_at IS DISTINCT FROM OLD.issued_at
           OR NEW.snapshot IS DISTINCT FROM OLD.snapshot
           OR NEW.customer_id IS DISTINCT FROM OLD.customer_id THEN
            RAISE EXCEPTION 'issued invoice % is immutable', OLD.id
                USING ERRCODE = 'restrict_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER invoices_guard_issued
    BEFORE UPDATE OR DELETE ON invoices
    FOR EACH ROW EXECUTE FUNCTION guard_issued_invoice();

CREATE OR REPLACE FUNCTION guard_invoice_line() RETURNS trigger AS $$
DECLARE
    parent_status text;
    parent_id     uuid;
BEGIN
    parent_id := COALESCE(NEW.invoice_id, OLD.invoice_id);
    SELECT status INTO parent_status FROM invoices WHERE id = parent_id;
    IF parent_status IS DISTINCT FROM 'draft' THEN
        RAISE EXCEPTION 'lines of invoice % are frozen once issued', parent_id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER invoice_lines_guard
    BEFORE INSERT OR UPDATE OR DELETE ON invoice_lines
    FOR EACH ROW EXECUTE FUNCTION guard_invoice_line();
