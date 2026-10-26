-- 0074_period_units.sql
--
-- A price can also be sold as an explicit number of weeks or months: "every 2 weeks",
-- "every 3 months".
--
-- 0059 added the day-count form `every_<N>_days`. This file adds the other two explicit
-- units without touching a single existing row:
--
--   * An N-week period is 7*N civil days, so it is the same day-count model as 0059.
--   * An N-month period is a *calendar* period (the model the four named intervals use),
--     so month-end clamping and leap days come out exactly as they do for "quarterly".
--     It is deliberately not 30*N days.
--
-- Both forms stay in the self-describing `interval` string rather than adding columns, so
-- a row still carries one period and cannot say "monthly" with a count beside it. The
-- bounds match internal/calendar: 1..52 weeks and 1..12 months, with the canonical
-- spelling (no leading zeros) enforced by the pattern.
--
-- The new CHECKs are strict supersets of the 0059 ones: every value that was legal
-- before is legal now with the same meaning, so existing prices, subscriptions and
-- issued documents are untouched.

BEGIN;

-- ===================================================== price_versions.interval ==

ALTER TABLE price_versions
    DROP CONSTRAINT price_versions_interval_check;

ALTER TABLE price_versions
    ADD CONSTRAINT price_versions_interval_check
    CHECK (
        interval IN ('monthly', 'quarterly', 'semiannual', 'annual', 'one_time', 'free')
        OR (
            interval ~ '^every_[1-9][0-9]{0,2}_days$'
            AND substring(interval FROM '^every_([0-9]{1,3})_days$')::int BETWEEN 1 AND 366
        )
        OR (
            interval ~ '^every_[1-9][0-9]?_weeks$'
            AND substring(interval FROM '^every_([0-9]{1,2})_weeks$')::int BETWEEN 1 AND 52
        )
        OR (
            interval ~ '^every_[1-9][0-9]?_months$'
            AND substring(interval FROM '^every_([0-9]{1,2})_months$')::int BETWEEN 1 AND 12
        )
    );

-- ====================================================== subscriptions.interval ==

-- A subscription is a schedule by definition (see 0028), so the recurring explicit forms
-- are accepted here too. `one_time` and `free` remain refused.
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
        OR (
            interval ~ '^every_[1-9][0-9]?_weeks$'
            AND substring(interval FROM '^every_([0-9]{1,2})_weeks$')::int BETWEEN 1 AND 52
        )
        OR (
            interval ~ '^every_[1-9][0-9]?_months$'
            AND substring(interval FROM '^every_([0-9]{1,2})_months$')::int BETWEEN 1 AND 12
        )
    );

COMMIT;
