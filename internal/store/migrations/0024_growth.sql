-- 0024_growth.sql
--
-- The growth extensions: affiliate attribution, commission on a paid order, and the
-- configuration for Discord notifications.
--
-- Four decisions shape this file, and each is recorded because the next person will
-- otherwise "simplify" it back into a defect:
--
-- 1. **A commission rate is a rational, never a float.** `commission_rate` is text and is
--    parsed with money.ParseRatio, exactly like a tax rate or a coupon percentage. A rate
--    of 10% is stored as `1/10`, not `0.1`: the canonical rational is what makes a later
--    recomputation of a payout reproduce the same minor units. The CHECK accepts the
--    shapes ParseRatio accepts (a decimal, a percentage, or a fraction) so a hand-written
--    row cannot store something the code cannot read.
--
-- 2. **Attribution and commission are rows in one table, and uniqueness is what makes
--    them idempotent.** A referral row with `order_id IS NULL` is the attribution (the
--    fact that a signup came from a code); a row with `order_id` set is the commission
--    earned on that order. Two partial unique indexes express the two invariants the
--    payout depends on: one attribution per customer, and at most one commission per
--    order. That second index — not a service-level "have I already done this?" check —
--    is what makes a replayed order-paid event a no-op instead of a doubled commission.
--
-- 3. **The balance is a per-currency materialised total, and the referrals are the
--    ledger.** `affiliate_balances` exists because a payout report must not sum every
--    historical commission to answer "what do we owe"; it is maintained in the same
--    transaction that inserts the commission, and it can never go negative (CHECK). The
--    referral rows remain the explanation, so a balance that disagrees with its rows is
--    a reportable defect rather than an unexplainable number. There is no journal posting
--    for a commission: `ledger_accounts` is a closed set owned by 0001, and adding a
--    marketing-expense account is that migration's decision, not this one's.
--
-- 4. **Discord configuration lives in `settings`, so there is no `discord_settings`
--    table.** 0020 already defines the operator-editable settings surface, and a second
--    configuration table would be a second thing to keep in step. `webhook_url` is a
--    credential: it is inserted with `is_secret = true` so every read path that honours
--    the flag redacts it, and the growth package additionally refuses to return it from
--    its own config reads (see growth.PublicConfig).
--
-- Nothing here rewrites an issued invoice, a journal entry or an audit row.

BEGIN;

-- ================================================================ affiliates ==
--
-- An affiliate is a customer who may refer other customers. It is keyed on `user_id`
-- rather than on `customer_id` because the person who owns the code signs in as a user,
-- and the portal endpoint resolves the customer from the session and then joins here.
-- One code per user: two codes for one person would split their earnings across two
-- payout reports for no reason.

CREATE TABLE affiliates (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users (id),
    -- The code that appears in a referral link. Upper-case so a lookup is a plain
    -- equality rather than a case-insensitive scan; the CHECK refuses a lower-case row
    -- so the canonical form is a fact about the data, not a convention in the reader.
    code       text NOT NULL CHECK (code ~ '^[A-Z0-9][A-Z0-9_-]{2,31}$'),
    CONSTRAINT affiliates_code_canonical CHECK (code = upper(code)),
    -- An exact rational: '0.1', '10%' or '1/10'. Never a float. Bounded above at 100%
    -- by the service (money.ParseRatio plus a range check), because a rate the code
    -- cannot parse would make a paid order fail to record its commission.
    commission_rate text NOT NULL
        CHECK (commission_rate ~ '^[0-9]+(\.[0-9]+)?%?$|^[0-9]+/[0-9]+$'),
    -- How the money leaves. A closed set: 'credit' puts it on the customer's credit
    -- balance, the others are settled out of band by staff. A free-text field would make
    -- the payout screen a guess.
    payout_method text NOT NULL DEFAULT 'credit'
        CHECK (payout_method IN ('credit', 'paypal', 'bank_transfer', 'manual')),
    -- A retired affiliate keeps their code in history but stops accepting new
    -- attributions; deleting the row would orphan the commissions it earned.
    enabled    boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX affiliates_code_unique ON affiliates (upper(code));
CREATE UNIQUE INDEX affiliates_one_per_user ON affiliates (user_id);

-- ========================================================= affiliate balances ==
--
-- The materialised payable, per currency. Commission is earned in the currency of the
-- order that produced it, so a single "balance" column would be meaningless the moment
-- an installation sells in two currencies.

CREATE TABLE affiliate_balances (
    affiliate_id  uuid NOT NULL REFERENCES affiliates (id) ON DELETE CASCADE,
    currency      text NOT NULL CHECK (currency ~ '^[A-Z0-9]{2,12}$'),
    -- Earned and not yet paid out.
    balance_minor bigint NOT NULL DEFAULT 0 CHECK (balance_minor >= 0),
    -- Lifetime paid out, kept here so the payout report does not have to scan history.
    paid_minor    bigint NOT NULL DEFAULT 0 CHECK (paid_minor >= 0),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (affiliate_id, currency)
);

-- ======================================================== affiliate referrals ==
--
-- One table, two row shapes, each with its own uniqueness rule:
--
--   * attribution  (order_id IS NULL, commission_minor = 0, status 'pending')
--   * commission   (order_id set, commission_minor >= 0, status pending|approved|paid)
--
-- `commission_rate` is captured on the commission row so a later rate change cannot
-- rewrite what a past order earned — the same reason an invoice snapshots its prices.

CREATE TABLE affiliate_referrals (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    affiliate_id  uuid NOT NULL REFERENCES affiliates (id),
    customer_id   uuid NOT NULL REFERENCES customers (id),
    attributed_at timestamptz NOT NULL DEFAULT now(),
    -- The order the commission was earned on. NULL on the attribution row, which is
    -- written at signup, before any order exists.
    order_id      uuid REFERENCES orders (id),
    commission_minor bigint NOT NULL DEFAULT 0 CHECK (commission_minor >= 0),
    -- The currency the commission is denominated in: the order's currency.
    currency      text NOT NULL CHECK (currency ~ '^[A-Z0-9]{2,12}$'),
    status        text NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending', 'approved', 'paid')),
    -- The rate that produced commission_minor, canonicalised. Empty on an attribution row.
    commission_rate text NOT NULL DEFAULT '',
    paid_at       timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    -- An attribution row earns nothing; a commission row is the only row that may.
    CONSTRAINT affiliate_referrals_shape CHECK (
        (order_id IS NULL AND commission_minor = 0 AND status = 'pending')
        OR (order_id IS NOT NULL)
    ),
    -- Paid means it went out, so it has a timestamp; the reverse is also true, so a
    -- half-applied payout cannot be read as either state.
    CONSTRAINT affiliate_referrals_paid_at CHECK ((status = 'paid') = (paid_at IS NOT NULL))
);

-- One attribution per customer. A second referral code for the same customer is refused
-- rather than re-pointing the first: silently moving an attribution would move money
-- between affiliates.
CREATE UNIQUE INDEX affiliate_referrals_one_attribution
    ON affiliate_referrals (customer_id) WHERE order_id IS NULL;
-- One commission per order. This is the replay guard: a re-delivered order-paid event
-- conflicts here and the insert becomes a no-op.
CREATE UNIQUE INDEX affiliate_referrals_one_commission
    ON affiliate_referrals (order_id) WHERE order_id IS NOT NULL;
CREATE INDEX affiliate_referrals_affiliate_idx
    ON affiliate_referrals (affiliate_id, status, created_at DESC);
CREATE INDEX affiliate_referrals_customer_idx
    ON affiliate_referrals (customer_id, created_at DESC);

-- ================================================================== discord ==
--
-- The settings a Discord webhook needs. The URL is a credential and is marked secret at
-- the point of insertion, so a generic settings read that honours `is_secret` redacts it
-- and the growth package's own config read never returns it.
--
-- `events` is a JSON array of event keys, because the selection is a set the operator
-- edits as a whole. An empty array means "notify for nothing", which is the safe default
-- for a channel nobody has configured yet.

INSERT INTO settings (group_key, key, value, is_secret) VALUES
    ('discord', 'enabled',     'false', false),
    ('discord', 'webhook_url', '',      true),
    ('discord', 'events',      '[]',    false)
ON CONFLICT (group_key, key) DO NOTHING;

COMMIT;
