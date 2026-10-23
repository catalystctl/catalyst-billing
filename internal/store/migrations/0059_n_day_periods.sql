-- 0059_n_day_periods.sql
--
-- A price can be sold as a fixed number of days: "every 45 days".
--
-- Until now a period was always a whole number of calendar months. That is the model
-- that makes month ends and leap days come out right, and it stays exactly as it is.
-- A day count is a *second* period model: it has no month to hang from, so it is
-- counted in civil days from an explicit origin. `internal/calendar` now carries both
-- models and chooses between them from the stored interval string; it deliberately does
-- not blend them, because a blended model would move the renewal date of every existing
-- subscription.
--
-- Four decisions shape this file:
--
-- 1. **The day count lives in the existing `interval` string, not a second column.**
--    A period is one fact. Two columns (`interval` = 'days' plus `interval_days`) would
--    let a row say `monthly` with a day count beside it, and every reader would have to
--    know which one wins. The stored form is `every_<N>_days` — self-describing in a
--    column an operator reads, and impossible to confuse with a calendar name. N is
--    bounded to 1..366 in the CHECK as well as in Go, because the database is what makes
--    the bound true for a row written by hand.
--
-- 2. **`subscriptions.interval` grows with it.** 0028 deliberately did not extend the
--    subscription set: `one_time` and `free` are not schedules, so a subscription row
--    claiming one would be a row every consumer must remember to skip. A day count *is*
--    a schedule — it renews — so the set that a subscription may carry grows by exactly
--    the recurring day-count form and by nothing else. `one_time` and `free` are still
--    refused there.
--
-- 3. **No row is rewritten and no constraint is relaxed.** The new CHECK is a strict
--    superset of the old one: every value that was legal before is legal now, with the
--    same meaning, so the existing subscriptions and price versions are untouched. The
--    canonical-spelling rule (`every_45_days`, never `every_045_days`) is enforced by the
--    pattern, so one period has one string and a period key cannot be duplicated by a
--    second spelling of the same length.
--
-- 4. **Nothing here changes a document.** An issued invoice records the period it billed
--    as a key and two instants; this migration adds no column to `invoices` and cannot
--    reinterpret one. A period that was billed before this file is still described by the
--    same two dates after it.

BEGIN;

-- ===================================================== price_versions.interval ==

ALTER TABLE price_versions
    DROP CONSTRAINT price_versions_interval_check;

ALTER TABLE price_versions
    ADD CONSTRAINT price_versions_interval_check
    CHECK (
        interval IN ('monthly', 'quarterly', 'semiannual', 'annual', 'one_time', 'free')
        OR (
            -- The canonical day-count spelling: no leading zeros, at most three
            -- digits, and a length between one day and one leap year.
            interval ~ '^every_[1-9][0-9]{0,2}_days$'
            AND substring(interval FROM '^every_([0-9]{1,3})_days$')::int BETWEEN 1 AND 366
        )
    );

-- ====================================================== subscriptions.interval ==

-- The subscription set grows by the recurring day-count form only. A subscription is a
-- schedule by definition (see 0028, decision 1), and a day count is a schedule; a
-- one-time or free price is still not, so those two values remain refused here.
ALTER TABLE subscriptions
    DROP CONSTRAINT subscriptions_interval_check;

ALTER TABLE subscriptions
    ADD CONSTRAINT subscriptions_interval_check
    CHECK (
        interval IN ('monthly', 'quarterly', 'semiannual', 'annual')
        OR (
            interval ~ '^every_[1-9][0-9]{0,2}_days$'
            AND substring(interval FROM '^every_([0-9]{1,3})_days$')::int BETWEEN 1 AND 366
        )
    );

COMMIT;
