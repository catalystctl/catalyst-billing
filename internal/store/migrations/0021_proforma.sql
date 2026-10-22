-- 0021_proforma.sql
--
-- Let a proforma invoice become a real one.
--
-- A proforma is a document a customer can read and pay but which carries no number, because
-- a number is what makes an invoice a legal document and a provisional one has not earned it
-- yet. `0020_commerce.sql` added `invoices.proforma` and the CHECK that keeps a proforma
-- unnumbered, but the immutability trigger from `0008_invariants.sql` refuses *any* change to
-- `number` once a row has left `draft`. A proforma is `status = 'open'`, so converting one is
-- rejected:
--
--     ERROR: issued invoice <id> is immutable
--
-- The trigger is right to be strict, and the carve-out has to be narrow enough that it cannot
-- become a hole. So this migration re-creates the function with exactly one additional
-- permission — a row that *was* a proforma may receive its number in the transition that stops
-- it being one — and keeps every other rule byte for byte.
--
-- Why a trigger change rather than a status:
--
--   * A new `proforma` status would have to be added to `invoices_status_check` and to the
--     transition table in the trigger, and the numbering CHECK ties a number to "not draft".
--     The flag says exactly one thing — this document has no number yet — and it is what the
--     issuing code already reads.
--   * Converting in one UPDATE is what keeps the document consistent: `proforma = false` and
--     `number = 'INV-000123'` must be visible to any concurrent reader at the same instant, or
--     a reader can observe a numbered proforma (which the CHECK forbids) or an unnumbered
--     invoice (which would be a gap in the series).
--
-- What is deliberately still forbidden after conversion:
--
--   * changing the number again, the currency, the money, the snapshot, the customer or the
--     period — the document is frozen the moment it has a number, exactly as before;
--   * converting a proforma that was voided, because `status` transitions are unchanged;
--   * reducing `amount_paid_minor`;
--   * deleting anything that is not a draft.
--
-- The carve-out is guarded on the *transition*, not on the flag: `OLD.proforma AND NOT
-- NEW.proforma` can be true at most once per row, and only when the number goes from NULL to a
-- value. A second attempt has `OLD.proforma = false` and is refused by the rule above it.

BEGIN;

CREATE OR REPLACE FUNCTION guard_issued_invoice() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.status <> 'draft' THEN
            RAISE EXCEPTION 'issued invoice % cannot be deleted', OLD.id
                USING ERRCODE = 'restrict_violation';
        END IF;
        RETURN OLD;
    END IF;

    IF OLD.status = 'draft' THEN
        -- A draft may become an issued document, and only in that direction.
        RETURN NEW;
    END IF;

    -- A proforma becoming a real invoice assigns its number. This is the only permitted
    -- change to `number`, it may happen once, and it must go from NULL to a value: a
    -- conversion that tried to renumber would still be caught by the comparison below,
    -- because `NEW.number` would differ from the NULL `OLD.number` in a second pass only if
    -- `OLD.proforma` were still true, which it cannot be after the first.
    IF OLD.proforma AND NOT NEW.proforma THEN
        IF OLD.number IS NOT NULL THEN
            RAISE EXCEPTION 'proforma % already has a number and cannot be converted', OLD.id
                USING ERRCODE = 'restrict_violation';
        END IF;
        IF NEW.number IS NULL THEN
            RAISE EXCEPTION 'converting proforma % must assign a number', OLD.id
                USING ERRCODE = 'restrict_violation';
        END IF;
        -- Everything except identity-of-number is still frozen through the checks below.
        IF NEW.currency      IS DISTINCT FROM OLD.currency
           OR NEW.subtotal_minor IS DISTINCT FROM OLD.subtotal_minor
           OR NEW.tax_minor  IS DISTINCT FROM OLD.tax_minor
           OR NEW.total_minor IS DISTINCT FROM OLD.total_minor
           OR NEW.snapshot   IS DISTINCT FROM OLD.snapshot
           OR NEW.customer_id IS DISTINCT FROM OLD.customer_id
           OR NEW.order_id   IS DISTINCT FROM OLD.order_id
           OR NEW.subscription_id IS DISTINCT FROM OLD.subscription_id
           OR NEW.period_key IS DISTINCT FROM OLD.period_key THEN
            RAISE EXCEPTION 'proforma % can only gain its number, nothing else', OLD.id
                USING ERRCODE = 'restrict_violation';
        END IF;
    ELSIF NEW.number       IS DISTINCT FROM OLD.number
       OR NEW.currency     IS DISTINCT FROM OLD.currency
       OR NEW.subtotal_minor IS DISTINCT FROM OLD.subtotal_minor
       OR NEW.tax_minor    IS DISTINCT FROM OLD.tax_minor
       OR NEW.total_minor  IS DISTINCT FROM OLD.total_minor
       OR NEW.issued_at    IS DISTINCT FROM OLD.issued_at
       OR NEW.snapshot     IS DISTINCT FROM OLD.snapshot
       OR NEW.customer_id  IS DISTINCT FROM OLD.customer_id
       OR NEW.order_id     IS DISTINCT FROM OLD.order_id
       OR NEW.subscription_id IS DISTINCT FROM OLD.subscription_id
       OR NEW.period_key   IS DISTINCT FROM OLD.period_key THEN
        RAISE EXCEPTION 'issued invoice % is immutable', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;

    -- Lifecycle may only move along the legal transitions. In particular a paid
    -- invoice cannot become void, and a paid amount cannot be reduced: that would
    -- silently un-receive money the ledger already records.
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        IF NOT (
            (OLD.status = 'open' AND NEW.status IN ('paid', 'void', 'uncollectible'))
            OR (OLD.status = 'paid' AND NEW.status IN ('refunded', 'disputed'))
            OR (OLD.status = 'uncollectible' AND NEW.status IN ('paid', 'void'))
        ) THEN
            RAISE EXCEPTION 'invoice % cannot move from % to %', OLD.id, OLD.status, NEW.status
                USING ERRCODE = 'restrict_violation';
        END IF;
    END IF;

    IF NEW.amount_paid_minor < OLD.amount_paid_minor THEN
        RAISE EXCEPTION 'invoice % paid amount cannot decrease (% -> %)',
            OLD.id, OLD.amount_paid_minor, NEW.amount_paid_minor
            USING ERRCODE = 'restrict_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- The trigger definition itself does not change; CREATE OR REPLACE FUNCTION above replaces the
-- body in place, so the existing `invoices_guard_issued` trigger picks it up. Re-creating the
-- trigger here would be a second, silently divergent definition if the two ever drifted, so it
-- is deliberately not repeated.

-- A converted invoice must be findable by number and must not collide with a proforma's NULL.
-- `number` is already UNIQUE, and Postgres treats NULLs as distinct, so many proformas coexist.
-- This index makes the "find the invoice with this number" lookup that a support agent does
-- from a customer email an index scan rather than a sequential one now that proformas share
-- the table.
CREATE INDEX IF NOT EXISTS invoices_number_idx ON invoices (number) WHERE number IS NOT NULL;

-- Proforma documents are the ones a customer is asked to pay before a number exists, so the
-- dunning scanner and the portal list both filter on this. A partial index keeps that cheap
-- without indexing the overwhelmingly common non-proforma case.
CREATE INDEX IF NOT EXISTS invoices_proforma_idx ON invoices (customer_id, created_at DESC)
    WHERE proforma;

COMMIT;
