-- 0046_manual_create.sql
--
-- A service an operator creates by hand, and how it differs from one a customer bought.
--
-- Until now the panel could list and act on customers and services but not create either, so a
-- phone order, a manually onboarded account, a comped customer, or a service added to an existing
-- customer all needed SQL. This migration adds no new table: it records the one thing the existing
-- rows cannot say, which is *where a service came from*.
--
-- Six decisions shape it, and each is recorded because the next person will otherwise
-- "simplify" them back into the defect they prevent.
--
-- 1. **A manual service creates a subscription but no invoice.** The renewal engine finds work by
--    scanning `subscriptions` for `status IN ('active','past_due') AND next_billing_at <= ...`, so
--    a service with no subscription row would be invisible to billing for ever and an operator
--    would have to remember to invoice it by hand — which is the exact "do it in SQL" problem this
--    closes. So a recurring price gets a real subscription, with its period derived from the
--    price's own anchor and timezone by `internal/calendar`, and the first period starts at
--    creation rather than at a payment that never happened.
--
--    What it does *not* get is an invoice. An invoice is a demand for money backed by an immutable
--    document and a ledger posting; a manual creation is the delivery of something already agreed
--    (a transfer that cleared last week, an account onboarded from another panel, a comped
--    service). The panel already has exactly one path that raises a bill and provisions on
--    payment: `POST /api/v1/staff/invoices` with a `product_id` line, which
--    `internal/fulfilment` turns into a service when it settles. This migration deliberately does
--    not become a second one, so `invoices` is still written only by `billing.Issuer`.
--
--    The honest consequence, stated rather than hidden: a manual service that is not comped will
--    be invoiced at the catalog price when its first period ends, because the subscription the
--    renewal engine sees says so. That is what "the customer is on this plan" means. A service
--    that must never be billed is comped instead (decision 2).
--
-- 2. **A comped service gets no subscription at all.** "Comp" is not a flag on a subscription or a
--    zero-amount price version (0028 refused to let a subscription point at a non-recurring price
--    precisely so "nothing may renew this" is a fact, not a code path). The absence of the row is
--    the enforcement: `services.subscription_id` is nullable and has been since 0001, so a comped
--    service is a service with no schedule behind it — nothing invoices it, nothing suspends it for
--    non-payment, and the panel shows "no next bill" rather than a date that is a lie.
--
-- 3. **Stock and per-user limits are enforced by the same helpers checkout uses.** The manual path
--    calls `service.CheckPurchaseAllowed` and `service.DecrementStock` inside the creating
--    transaction, which is the identical code the customer checkout path runs. A manual creation
--    that could put a sold-out product into someone's hands would be a bug an operator finds in
--    production, and duplicating the rule here would be how it happens. The decrement is in the
--    same transaction as the insert, so a rollback returns the unit.
--
-- 4. **The service records its own origin, in columns, as well as in the audit log.** An auditor's
--    first question about a service is "is there an order behind this, who put it there, and why",
--    and answering it from `audit_events` alone means knowing which action name to look for and
--    joining on a target id. `origin`, `origin_reason` and `origin_actor` make that one query. The
--    audit row is still written in the same transaction (a change with no audit row is a gap), so
--    the two cannot disagree: the columns are the current fact, the audit row is the history that
--    survives it.
--
-- 5. **"Manual implies a reason" is a CHECK, not a convention.** The house rule for anything an
--    auditor asks about is that the database refuses the row that would leave the question
--    unanswerable, so `origin = 'manual'` with a blank `origin_reason` cannot be stored. The
--    default is `purchase`, so every existing row is already correct and nothing that sells a
--    service today changes behaviour.
--
-- 6. **Nothing here relaxes a constraint, and no table is added.** The columns are additive with
--    defaults; no CHECK is widened; no row is rewritten. Because there is no new table, the
--    truncate/classification list in `internal/store/storetest` needs no entry — `services` is
--    already on it, and a column on a listed table is covered.

BEGIN;

-- ------------------------------------------------------------- service origin --

ALTER TABLE services
    -- 'purchase' is everything that came through an order: a customer checkout, a staff
    -- invoice, a free product. 'manual' is an operator adding a service directly.
    ADD COLUMN origin text NOT NULL DEFAULT 'purchase'
        CHECK (origin IN ('purchase', 'manual')),
    -- Why it was added. Required for a manual service by the CHECK below; always empty for a
    -- purchase, where the reason is the order.
    ADD COLUMN origin_reason text NOT NULL DEFAULT '',
    -- The operator who added it. Nullable because the actor may be an API key's owner or, in a
    -- future import path, nothing at all — but never guessed.
    ADD COLUMN origin_actor uuid REFERENCES users (id);

-- The rule, in the database: a service with no order behind it must say why.
ALTER TABLE services
    ADD CONSTRAINT services_manual_reason
    CHECK (origin <> 'manual' OR length(btrim(origin_reason)) > 0);

-- The report an auditor actually asks for: every service that was created by hand, newest first.
-- Partial, because a purchase row is never in the answer and the index should not carry the
-- whole table.
CREATE INDEX services_manual_origin_idx ON services (created_at DESC) WHERE origin = 'manual';

COMMIT;
