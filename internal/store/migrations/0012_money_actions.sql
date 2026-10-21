-- 0012_money_actions.sql
--
-- Refunds, credit notes and credit application.
--
-- The refund row already existed; what was missing is who asked for it, why, and
-- when it finished. Those are the three questions a dispute asks, and reconstructing
-- them from logs is not possible because the provider call happens outside the
-- transaction.
--
-- Credit notes are documents, not rows with a mutable amount: a wrong invoice is
-- corrected by an immutable counter-document, exactly as an invoice is corrected by a
-- void plus a reissue rather than an edit.

ALTER TABLE refunds
    ADD COLUMN reason      text NOT NULL DEFAULT '',
    ADD COLUMN requested_by uuid REFERENCES users (id),
    ADD COLUMN approved_by uuid REFERENCES users (id),
    ADD COLUMN finished_at timestamptz;

CREATE INDEX refunds_payment_idx ON refunds (payment_id);
CREATE INDEX refunds_open_idx ON refunds (created_at)
    WHERE status IN ('created', 'pending', 'unknown', 'needs_review');

-- --------------------------------------------------------------- credit notes --

CREATE TABLE credit_notes (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    number      text UNIQUE,
    customer_id uuid NOT NULL REFERENCES customers (id),
    -- Nullable: a goodwill credit need not correct a specific invoice.
    invoice_id  uuid REFERENCES invoices (id),
    currency    text NOT NULL CHECK (currency ~ '^[A-Z0-9]{2,12}$'),
    total_minor bigint NOT NULL CHECK (total_minor > 0),
    -- The tax share is separated so the reversal can post to tax_payable rather than
    -- treating the whole credit as revenue.
    tax_minor   bigint NOT NULL DEFAULT 0 CHECK (tax_minor >= 0),
    reason      text NOT NULL CHECK (reason <> ''),
    status      text NOT NULL CHECK (status IN ('draft', 'issued', 'applied', 'void')),
    issued_at   timestamptz,
    -- Frozen evidence, same idea as an invoice snapshot: what was credited, and why.
    snapshot    jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_by  uuid REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT credit_notes_issued_consistency CHECK (
        (status = 'draft' AND number IS NULL AND issued_at IS NULL) OR status <> 'draft'
    ),
    CONSTRAINT credit_notes_tax_within_total CHECK (tax_minor <= total_minor)
);

CREATE INDEX credit_notes_customer_idx ON credit_notes (customer_id, created_at DESC);
CREATE INDEX credit_notes_invoice_idx ON credit_notes (invoice_id);

-- An issued credit note is immutable in the same way an issued invoice is: the
-- document a customer received cannot be rewritten after the fact.
CREATE OR REPLACE FUNCTION guard_issued_credit_note() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.status <> 'draft' THEN
            RAISE EXCEPTION 'issued credit note % cannot be deleted', OLD.id
                USING ERRCODE = 'restrict_violation';
        END IF;
        RETURN OLD;
    END IF;

    IF OLD.status = 'draft' THEN
        RETURN NEW;
    END IF;

    IF NEW.number       IS DISTINCT FROM OLD.number
       OR NEW.currency  IS DISTINCT FROM OLD.currency
       OR NEW.total_minor IS DISTINCT FROM OLD.total_minor
       OR NEW.tax_minor IS DISTINCT FROM OLD.tax_minor
       OR NEW.invoice_id IS DISTINCT FROM OLD.invoice_id
       OR NEW.customer_id IS DISTINCT FROM OLD.customer_id
       OR NEW.issued_at IS DISTINCT FROM OLD.issued_at
       OR NEW.snapshot  IS DISTINCT FROM OLD.snapshot THEN
        RAISE EXCEPTION 'issued credit note % is immutable', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;

    IF NEW.status IS DISTINCT FROM OLD.status
       AND NOT (OLD.status = 'issued' AND NEW.status IN ('applied', 'void')) THEN
        RAISE EXCEPTION 'credit note % cannot move from % to %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'restrict_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER credit_notes_guard_issued
    BEFORE UPDATE OR DELETE ON credit_notes
    FOR EACH ROW EXECUTE FUNCTION guard_issued_credit_note();

-- ------------------------------------------------------- credit application --

-- Applying customer credit to an invoice is a distinct economic event from granting
-- the credit, and a customer may apply credit to the same invoice more than once.
-- Giving it its own row is what makes "apply" idempotent per application instead of
-- per invoice: the journal transaction references this row's id, and the ledger's
-- (kind, reference_type, reference_id) uniqueness then makes a replay a no-op.
CREATE TABLE credit_applications (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id uuid NOT NULL REFERENCES customers (id),
    invoice_id  uuid NOT NULL REFERENCES invoices (id),
    amount_minor bigint NOT NULL CHECK (amount_minor > 0),
    currency    text NOT NULL CHECK (currency ~ '^[A-Z0-9]{2,12}$'),
    applied_by  uuid REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX credit_applications_invoice_idx ON credit_applications (invoice_id);
CREATE INDEX credit_applications_customer_idx ON credit_applications (customer_id);
