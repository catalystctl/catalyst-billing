-- 0008_invariants.sql
--
-- Structural fixes for invariants that were only enforced by convention.
--
-- Each of these closes a hole that was reachable in practice, not in theory.

-- A provider payment can be allocated exactly once, in total. Without this, two
-- recoveries of the same payment could credit two invoices while the ledger
-- recorded one payment.
CREATE UNIQUE INDEX payment_allocations_payment_unique
    ON payment_allocations (payment_id);

-- One provider reference per checkout attempt: amount verification resolves a
-- payment to an attempt by this column, and a non-unique match would let a
-- lookup bind to the wrong row.
CREATE UNIQUE INDEX payment_attempts_provider_ref_unique
    ON payment_attempts (gateway, provider_ref)
    WHERE provider_ref IS NOT NULL;

-- Issued invoices: the remaining lifecycle columns are guarded too. The trigger
-- is replaced rather than extended so the whole rule is readable in one place.
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

    -- Immutable identity and money.
    IF NEW.number       IS DISTINCT FROM OLD.number
       OR NEW.currency  IS DISTINCT FROM OLD.currency
       OR NEW.subtotal_minor IS DISTINCT FROM OLD.subtotal_minor
       OR NEW.tax_minor IS DISTINCT FROM OLD.tax_minor
       OR NEW.total_minor IS DISTINCT FROM OLD.total_minor
       OR NEW.issued_at IS DISTINCT FROM OLD.issued_at
       OR NEW.snapshot  IS DISTINCT FROM OLD.snapshot
       OR NEW.customer_id IS DISTINCT FROM OLD.customer_id
       OR NEW.order_id IS DISTINCT FROM OLD.order_id
       OR NEW.subscription_id IS DISTINCT FROM OLD.subscription_id
       OR NEW.period_key IS DISTINCT FROM OLD.period_key THEN
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

-- 'refunded' and 'disputed' join the allowed invoice states so the trigger above
-- can express those transitions.
ALTER TABLE invoices DROP CONSTRAINT invoices_status_check;
ALTER TABLE invoices ADD CONSTRAINT invoices_status_check
    CHECK (status IN ('draft', 'open', 'paid', 'void', 'uncollectible', 'refunded', 'disputed'));

-- A dunning state must never outlive its invoice.
ALTER TABLE dunning_states
    DROP CONSTRAINT dunning_states_invoice_id_fkey;
ALTER TABLE dunning_states
    ADD CONSTRAINT dunning_states_invoice_id_fkey
    FOREIGN KEY (invoice_id) REFERENCES invoices (id) ON DELETE CASCADE;
