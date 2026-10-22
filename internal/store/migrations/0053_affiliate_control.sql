-- 0053_affiliate_control.sql
--
-- Staff control over an affiliate: suspend (ban), reinstate, archive (delete), and the
-- reason/actor for each. A custom commission rate needed no column — 0025 already stores
-- `commission_rate` as an exact rational bounded at 100% by growth.ParseRate — so the only
-- schema work is here, and it is deliberately small.
--
-- Three decisions, recorded because the next person would otherwise take one of them the
-- other way and reintroduce a defect:
--
-- 1. **Suspension is `enabled = false`, not a second status column.**
--    0025 already made `enabled` the switch that stops a code accepting new attributions
--    (growth.affiliateForCode refuses a disabled code) *and* stops a commission being
--    recorded (growth.recordCommission returns ErrAffiliateDisabled); 0043's recurring
--    path reads the same flag. A `status` column beside it would be a second source of
--    truth for the one question that matters — "may this affiliate earn?" — and the two
--    would drift the first time somebody set one and not the other. What 0025 lacked was
--    not a column but a *reason*: who suspended, when, and why. So this migration adds
--    those, and the status the API reports is derived, in one place, from the state that
--    already gates earning:
--
--        deleted_at IS NOT NULL -> 'deleted'
--        enabled = false        -> 'suspended'
--        otherwise              -> 'active'
--
--    A row disabled before this migration (a retired affiliate, or the seed data) has no
--    `suspended_at` and derives as 'suspended'. That is left alone rather than backfilled
--    from `updated_at`: inventing a suspension time and an operator for a decision nobody
--    recorded would make the audit trail lie.
--
-- 2. **Deleting is an archive, never a DELETE of the row.**
--    `affiliate_referrals` (0025) and `affiliate_recurring_commissions` (0043) reference
--    `affiliates` with no ON DELETE rule, so the database already refuses to remove an
--    affiliate that earned anything. That refusal is correct and must not be "fixed" with
--    a cascade: a deleted affiliate with unpaid commission is a debt we still owe, and the
--    commission rows are the explanation of that debt. `deleted_at` retires the row from
--    the active list and from new attribution, and leaves the ledger intact — so the payout
--    report keeps naming a deleted affiliate for as long as their balance is non-zero, and
--    a payout can still settle it. (Note that `affiliate_balances` and `affiliate_payouts`
--    *do* cascade from `affiliates`; because the delete path never removes the affiliates
--    row, that cascade never fires. It stays as it is, because deleting a summary of money
--    already paid out is not something any code path should be able to do.)
--
-- 3. **A suspension and a deletion cannot both be current.** Deleting clears the
--    suspension fields and a CHECK keeps them cleared, so "what is this affiliate's state"
--    has exactly one answer instead of two rows of history disagreeing. The history itself
--    lives where the rest of the operator history lives: the audit log, one row per action.
--
-- Nothing here rewrites an issued invoice, a journal entry, an audit row or a commission.

BEGIN;

ALTER TABLE affiliates
    -- When, why and by whom the affiliate was suspended. `suspended_at IS NULL` with
    -- `enabled = false` is the legacy "retired" state described in decision 1.
    ADD COLUMN suspended_at     timestamptz,
    ADD COLUMN suspended_reason text NOT NULL DEFAULT '',
    ADD COLUMN suspended_by     uuid REFERENCES users (id),
    -- When, why and by whom the affiliate was archived. Once set, it is never cleared: an
    -- affiliate is reinstated from a suspension, not from a deletion.
    ADD COLUMN deleted_at       timestamptz,
    ADD COLUMN deleted_reason   text NOT NULL DEFAULT '',
    ADD COLUMN deleted_by       uuid REFERENCES users (id);

-- A suspension or a deletion that did not actually stop the affiliate earning would be a
-- record of an action that has no effect, which is worse than no record at all. These two
-- constraints are what make the derived status in decision 1 truthful.
ALTER TABLE affiliates
    ADD CONSTRAINT affiliates_suspended_is_disabled
        CHECK (suspended_at IS NULL OR enabled = false),
    ADD CONSTRAINT affiliates_deleted_is_disabled
        CHECK (deleted_at IS NULL OR enabled = false),
    ADD CONSTRAINT affiliates_deleted_not_suspended
        CHECK (deleted_at IS NULL OR suspended_at IS NULL);

-- The affiliate list is "who is in the programme", which excludes the archive. A partial
-- index keeps that list from scanning every affiliate that ever left.
CREATE INDEX affiliates_active_idx ON affiliates (created_at DESC)
    WHERE deleted_at IS NULL;

COMMIT;
