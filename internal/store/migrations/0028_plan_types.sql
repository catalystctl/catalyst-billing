-- 0028_plan_types.sql
--
-- Plan types: a price can be free, or charged once, and a plan list has an order.
--
-- Until now every price was recurring. `price_versions.interval` was a closed set of four
-- monthly-ish periods, so a product that is bought once ("a migration service", "a setup
-- package") could not be expressed at all, and a product that costs nothing had to be
-- written as a monthly price of zero — which would have billed the customer zero every
-- month forever, and produced a subscription whose next-bill date is a lie.
--
-- Six decisions shape this file, and each is recorded because the next person will
-- otherwise "simplify" it back into a defect:
--
-- 1. **`price_versions.interval` grows by two values; `subscriptions.interval` does not.**
--    A price is a thing that can be sold in more than one way, so its closed set becomes
--    monthly / quarterly / semiannual / annual / one_time / free. A *subscription* is a
--    recurring schedule by definition: a row that said `one_time` would be a row every
--    consumer of `subscriptions` has to remember to skip, and forgetting once means
--    charging a customer again for something they bought once. So the subscription's set
--    stays the four recurring periods, and the database — not the renewal engine's
--    discipline — is what makes "nothing may try to renew a one-time purchase" true. The
--    absence of a subscription is the enforcement: `services.subscription_id` is nullable
--    (it has been since 0001), so a one-time or free purchase creates a service with no
--    schedule behind it. Nothing is relaxed here, and this paragraph is the record of why
--    there is no relaxation to look for.
--
-- 2. **A free price has no amount, and a one-time price is a real price.** "Free" and
--    "costs nothing" are the same statement, so the database refuses a `free` price with a
--    non-zero amount or setup fee. A `one_time` price carries whatever it costs, including
--    zero. Both are charged once; the difference is only whether there is money to move,
--    which is what lets a free product activate with no invoice at all while a paid
--    one-time product gets an ordinary invoice and an ordinary payment.
--
-- 3. **`price_versions.sort_order` is presentation, not price.** The order of plans on a
--    product page ("Basic, Pro, Enterprise") is a merchandising decision, so it is a
--    column the panel can rewrite. That is deliberately the *only* mutable column on
--    `price_versions`: an amount, a currency or an interval is what an issued invoice
--    priced from, and appending a version is how those change. Reordering changes no
--    amount and no document, so it is an UPDATE rather than a new version — and the API
--    exposes exactly that one field, never a general price update.
--
-- 4. **A description stores its format beside its text.** A rich description is HTML and a
--    plain one is not, and the renderer has to be told which, because escaping HTML shows
--    the tags and parsing plain text runs whatever was typed as markup. Sanitising on
--    write is not a substitute for saying what the text is. `plain` is the default so
--    every existing row keeps rendering exactly as it does today.
--
-- 5. **Arbitrary `N day/week/month/year` periods stay deferred.** Supporting them is not a
--    new enum value: the billing calendar is anchored to a day-of-month and to calendar
--    months, and a "every 17 days" period needs a period model that measures by days,
--    prorates by days and still lands on a stable boundary across DST. That is a rewrite
--    of `internal/calendar`, not a migration, and the backlog (SF-14) says to wait until a
--    customer asks. `one_time` and `free` are additive because they are *not* periods: one
--    is charged once and the other is a zero amount, and neither produces a schedule.
--
-- 6. **Nothing here rewrites an issued invoice, a price, or an audit row.** The new CHECK
--    on `price_versions` only refuses rows that were impossible before, and the interval
--    set only grows, so every existing row still satisfies both.
--
-- The renewal log gains one outcome (`skipped_non_recurring`) for the same reason: a
-- subscription whose price cannot recur is a defect an operator has to see, and folding it
-- into "skipped" would hide it.

BEGIN;

-- ============================================================ plan types ==

-- The old constraint was inline in 0001 and therefore auto-named. Named explicitly from
-- here on, so the next change to the set can name it instead of guessing.
ALTER TABLE price_versions
    DROP CONSTRAINT price_versions_interval_check;

ALTER TABLE price_versions
    ADD CONSTRAINT price_versions_interval_check
    CHECK (interval IN ('monthly', 'quarterly', 'semiannual', 'annual', 'one_time', 'free'));

-- A free price is a zero price. The amount and the setup fee are both refused when
-- non-zero, because "free plus a setup fee" is a paid product wearing a misleading label
-- and a storefront that renders it as "$0" would be lying to the customer.
ALTER TABLE price_versions
    ADD CONSTRAINT price_versions_free_is_zero
    CHECK (interval <> 'free' OR (amount_minor = 0 AND setup_minor = 0));

-- The subscription's closed set is deliberately *not* extended: see decision 1. The
-- constraint is recreated with the same values only so it is named like its neighbour and
-- so this file is where a reader looks to find out why one set grew and the other did not.
ALTER TABLE subscriptions
    DROP CONSTRAINT subscriptions_interval_check;

ALTER TABLE subscriptions
    ADD CONSTRAINT subscriptions_interval_check
    CHECK (interval IN ('monthly', 'quarterly', 'semiannual', 'annual'));

-- The renewal log records what happened to each subscription, and "nothing, because this price
-- is not a schedule" is a new thing that can happen. It is a separate outcome from
-- `skipped_cancelled` and `skipped_not_due` because it is the only one an operator has to act
-- on: it means a subscription points at a one-time or free price, which nothing in the product
-- should be able to create.
ALTER TABLE renewal_events
    DROP CONSTRAINT renewal_events_outcome_check;

ALTER TABLE renewal_events
    ADD CONSTRAINT renewal_events_outcome_check
    CHECK (outcome IN ('invoiced', 'skipped_cancelled', 'skipped_not_due',
                       'skipped_non_recurring', 'needs_review', 'failed'));

-- ---------------------------------------------------------------- ordering --

-- Plan order within a product, ascending. Existing rows all get 0, so the current
-- ordering (newest version first, applied by the read query) is unchanged until an
-- operator sets an order.
ALTER TABLE price_versions
    ADD COLUMN sort_order int NOT NULL DEFAULT 0;

-- The storefront reads a product's plans in display order.
CREATE INDEX price_versions_plan_order_idx ON price_versions (product_id, sort_order, version DESC);

-- ------------------------------------------------------ description format --

-- Which of the two descriptions is markup. `short_description` stays plain: it is a card
-- subtitle rendered as text, and a second format column for it would be a second thing to
-- sanitise for no gain.
ALTER TABLE products
    ADD COLUMN description_format text NOT NULL DEFAULT 'plain'
        CHECK (description_format IN ('plain', 'html'));

COMMIT;
