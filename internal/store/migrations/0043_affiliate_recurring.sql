-- 0043_affiliate_recurring.sql
--
-- Recurring affiliate commission (GRW-03) and a payout that is a record rather than an edit.
--
-- Three decisions are recorded here, because each one is a fork the next person would
-- otherwise take the other way and reintroduce a defect:
--
-- 1. **A renewal commission is its own table, not a third shape in `affiliate_referrals`.**
--    0025 gave that table two row shapes (an attribution with `order_id IS NULL`, a
--    commission with `order_id` set) and made the shape a CHECK plus two partial unique
--    indexes. A renewal has no order — that is precisely why GRW-03 was left unpaid — so a
--    renewal commission row would have to be `order_id IS NULL` with a non-zero amount,
--    which breaks the shape CHECK *and* collides with the one-attribution-per-customer
--    unique index (`ON (customer_id) WHERE order_id IS NULL`). Relaxing those invariants
--    would put a rewrite of the attribution rules inside a feature that is about renewals,
--    and the attribution rules are what stop an existing account being absorbed into a
--    referral. So the recurring commission gets a table of its own with the same shape of
--    guarantee: one row per renewal invoice (unique index), amounts as integer minor units,
--    a captured rate, and a paid_at that agrees with status.
--
-- 2. **The cycle bound is a count of the rows that already exist, per referred customer.**
--    `affiliates.recurring_cycles` (a setting, default 0) bounds how many renewal invoices
--    of one referred customer earn a commission; 0 means every renewal, which is a
--    deliberate choice and not a missing value. The bound is a *count of commission rows
--    for that customer*, so it is answerable from the rows themselves rather than from a
--    separate counter that could drift. `renewal_index` records which renewal the row paid,
--    so a payout report can say "renewal 2 of 3" without recomputing anything.
--
-- 3. **A payout is an append-only row, and the commission rows point at it.** Before this
--    migration, paying an affiliate meant flipping every commission to `paid` and zeroing
--    the materialised balance; nothing recorded *which* payout settled *which* commission,
--    so a report could state a total but not explain it. `affiliate_payouts` is that record
--    (amount, currency, method, reference, the period it covers, the operator who ran it),
--    and `payout_id` on the commission rows is the link. The commission amount is never
--    rewritten by a payout — only the fact that it was settled, and by what. The balance
--    stays materialised for cheap reads, but the rows are now the explanation, and the two
--    are required to agree (the payout refuses to run when they do not).
--
-- Nothing here rewrites an issued invoice, a journal entry or an audit row.

BEGIN;

-- ============================================================== affiliate payouts ==
--
-- One row per settlement. `amount_minor > 0` because a zero payout is not a payout: a
-- double-clicked action finds nothing outstanding and is refused, which is what makes the
-- endpoint safe even before the Idempotency-Key layer answers.

CREATE TABLE affiliate_payouts (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    affiliate_id uuid NOT NULL REFERENCES affiliates (id) ON DELETE CASCADE,
    currency     text NOT NULL CHECK (currency ~ '^[A-Z0-9]{2,12}$'),
    amount_minor bigint NOT NULL CHECK (amount_minor > 0),
    -- How the money left. The same closed set as affiliates.payout_method, repeated here
    -- because the method recorded on a payout is a fact about that payment, not about the
    -- affiliate's current preference.
    method       text NOT NULL DEFAULT 'credit'
                 CHECK (method IN ('credit', 'paypal', 'bank_transfer', 'manual')),
    -- A provider or bank reference, free text: it is what an operator quotes when
    -- reconciling against a bank statement.
    reference    text NOT NULL DEFAULT '',
    note         text NOT NULL DEFAULT '',
    -- The window of commission rows the operator was looking at when they paid. It is
    -- context for the reconciliation, not a filter on what was settled: a payout settles
    -- the whole outstanding balance, because a commission is not divisible.
    period_from  timestamptz,
    period_to    timestamptz,
    created_by   uuid REFERENCES users (id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT affiliate_payouts_period CHECK (
        (period_from IS NULL AND period_to IS NULL)
        OR (period_from IS NOT NULL AND period_to IS NOT NULL AND period_from < period_to)
    )
);

CREATE INDEX affiliate_payouts_affiliate_idx
    ON affiliate_payouts (affiliate_id, created_at DESC);

-- ================================================== affiliate recurring commissions ==
--
-- The renewal half of the commission ledger. `invoice_id` is unique, and that index is the
-- idempotency guard: a replayed "invoice paid" event for a renewal conflicts here and the
-- insert becomes a no-op, exactly as the order commission's unique index does in 0025.

CREATE TABLE affiliate_recurring_commissions (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    affiliate_id     uuid NOT NULL REFERENCES affiliates (id),
    customer_id      uuid NOT NULL REFERENCES customers (id),
    -- The renewal invoice that earned it. Unique: one commission per renewal, whatever the
    -- provider replays.
    invoice_id       uuid NOT NULL REFERENCES invoices (id),
    commission_minor bigint NOT NULL CHECK (commission_minor >= 0),
    currency         text NOT NULL CHECK (currency ~ '^[A-Z0-9]{2,12}$'),
    -- The rate that produced commission_minor, canonicalised by money.ParseRatio. Captured
    -- so a later rate change cannot rewrite what a past renewal earned.
    commission_rate  text NOT NULL CHECK (commission_rate ~ '^[0-9]+/[0-9]+$'),
    -- Which renewal of this customer it was: 1 for the first renewal after the signup
    -- order. Together with affiliates.recurring_cycles it is the bound, stated on the row.
    renewal_index    int NOT NULL CHECK (renewal_index >= 1),
    status           text NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending', 'approved', 'paid')),
    -- The payout that settled it. NULL means owed.
    payout_id        uuid REFERENCES affiliate_payouts (id),
    earned_at        timestamptz NOT NULL DEFAULT now(),
    paid_at          timestamptz,
    -- The same rule 0025 states for an order commission: paid means it went out and has a
    -- timestamp, and a half-applied payout cannot be read as either state.
    CONSTRAINT affiliate_recurring_commissions_paid_at CHECK (
        (status = 'paid') = (paid_at IS NOT NULL)
    )
);

CREATE UNIQUE INDEX affiliate_recurring_commissions_one_per_invoice
    ON affiliate_recurring_commissions (invoice_id);
-- The cycle bound is a count for one customer, and the payout is a scan of one affiliate's
-- unsettled rows in one currency. Both are partial indexes so neither reads settled history.
CREATE INDEX affiliate_recurring_commissions_customer_idx
    ON affiliate_recurring_commissions (customer_id);
CREATE INDEX affiliate_recurring_commissions_unsettled_idx
    ON affiliate_recurring_commissions (affiliate_id, currency)
    WHERE payout_id IS NULL;

-- ============================================ link the order commissions to a payout ==
--
-- Purely additive: a nullable column, so every existing row and every existing insert keeps
-- its meaning. The one-attribution and one-commission-per-order indexes in 0025 are
-- untouched, which is the whole point of decision 1 above.

ALTER TABLE affiliate_referrals
    ADD COLUMN payout_id uuid REFERENCES affiliate_payouts (id);

-- The payout action reads exactly this: the affiliate's unsettled commissions in one
-- currency. Without the partial index that is a scan of the affiliate's whole history.
CREATE INDEX affiliate_referrals_unsettled_idx
    ON affiliate_referrals (affiliate_id, currency)
    WHERE payout_id IS NULL;

-- ======================================== reconstruct the payouts already made ==
--
-- Until this migration, paying an affiliate marked its commissions `paid`, zeroed the balance
-- and incremented `paid_minor`, but recorded nothing that said *which* payment settled them.
-- A report built on rows alone would therefore show those commissions as earned and un-paid,
-- and `earned - paid` would never equal what is owed — a false alarm on every installation
-- that has ever paid an affiliate.
--
-- The information needed to reconstruct the missing record is still there: a commission with
-- `status = 'paid'` was settled by exactly one payment, and the rows settled together share
-- the payment's instant closely enough that one payout per (affiliate, currency) group of
-- already-paid rows is the only faithful reconstruction available. It is labelled as a
-- reconstruction rather than presented as a record somebody made, because an operator reading
-- the history deserves to know the difference. Going forward the row is written by the payout
-- itself.
WITH settled AS (
    SELECT affiliate_id,
           currency,
           sum(commission_minor) AS amount_minor,
           max(paid_at)          AS paid_at
    FROM affiliate_referrals
    WHERE order_id IS NOT NULL
      AND status = 'paid'
      AND payout_id IS NULL
    GROUP BY affiliate_id, currency
    HAVING sum(commission_minor) > 0
), recorded AS (
    INSERT INTO affiliate_payouts
        (affiliate_id, currency, amount_minor, method, reference, note, created_at)
    SELECT s.affiliate_id, s.currency, s.amount_minor, a.payout_method, '',
           'reconstructed from commissions settled before payouts were recorded',
           coalesce(s.paid_at, now())
    FROM settled s
    JOIN affiliates a ON a.id = s.affiliate_id
    RETURNING id, affiliate_id, currency, created_at
)
UPDATE affiliate_referrals r
SET payout_id = recorded.id
FROM recorded
WHERE r.affiliate_id = recorded.affiliate_id
  AND r.currency = recorded.currency
  AND r.order_id IS NOT NULL
  AND r.status = 'paid'
  AND r.payout_id IS NULL
  AND r.paid_at <= recorded.created_at;

COMMIT;
