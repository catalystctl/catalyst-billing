-- 0065_adjustment_notes.sql
--
-- Adjustment notes: debit notes, and the state a note reaches when it is cancelled.
--
-- INV-10 asked for credit *and* debit notes in a numbered series with an active/voided
-- status. `0012_money_actions.sql` already modelled the credit note and its number series,
-- and its guard already permitted the transition `issued -> void`; what was missing was a
-- way to tell the two directions apart, somewhere to record why a note was cancelled, and
-- any code path that wrote the status at all.
--
-- Why a column rather than a second table: a debit note is the same document with the
-- opposite sign — same customer, same optional invoice, same reason, same number sequence
-- apparatus, same freeze-once-issued rule. A second table would duplicate all of that and
-- then need a second trigger, a second PDF loader and a second list query, and the two
-- would drift the first time one of them gained a column. The `kind` discriminator is what
-- makes one table hold both, and it is frozen by the guard below so a note cannot change
-- direction after it has a number.
--
-- Why the void timestamp is a column and not just a status: `voided_at` is what a document
-- prints when someone reprints a cancelled note, and it is what distinguishes "cancelled on
-- the 3rd" from "we noticed on the 9th". The CHECK ties it to the status so the two can
-- never disagree.

BEGIN;

ALTER TABLE credit_notes ADD COLUMN kind text NOT NULL DEFAULT 'credit';
ALTER TABLE credit_notes ADD CONSTRAINT credit_notes_kind_check
    CHECK (kind IN ('credit', 'debit'));

ALTER TABLE credit_notes ADD COLUMN void_reason text;
ALTER TABLE credit_notes ADD COLUMN voided_at timestamptz;

-- Rows that were already void carry no timestamp, because no code path wrote one. Backfill
-- them before the CHECK so the constraint describes every row rather than every row the
-- migration happened to see. `created_at` is the honest fallback: it is the latest moment
-- we know the note existed, and inventing `now()` would claim it was cancelled today.
UPDATE credit_notes SET voided_at = created_at WHERE status = 'void' AND voided_at IS NULL;
UPDATE credit_notes SET void_reason = 'cancelled before notes recorded a reason'
WHERE status = 'void' AND (void_reason IS NULL OR void_reason = '');

ALTER TABLE credit_notes ADD CONSTRAINT credit_notes_void_consistency CHECK (
    (status = 'void') = (voided_at IS NOT NULL)
);
ALTER TABLE credit_notes ADD CONSTRAINT credit_notes_void_reason_present CHECK (
    status <> 'void' OR (void_reason IS NOT NULL AND void_reason <> '')
);

-- A voided note is not a payment document and a debit note is not a credit, so the two
-- reads that matter most filter on both columns.
CREATE INDEX credit_notes_invoice_kind_idx ON credit_notes (invoice_id, kind, created_at DESC);
CREATE INDEX credit_notes_customer_kind_idx ON credit_notes (customer_id, kind, created_at DESC);

-- Re-create the guard with the three new rules. Every rule from 0012 is carried over
-- unchanged; the additions are the kind freeze, the void reason/timestamp pairing and the
-- terminal state of a voided note.
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

    -- A note's direction is fixed when it is written. Flipping a credit to a debit after it
    -- has a number would leave every copy already sent stating the opposite of our record.
    IF NEW.kind IS DISTINCT FROM OLD.kind THEN
        RAISE EXCEPTION 'issued adjustment note % cannot change kind', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;

    -- A void is the last word on a document. Anything after it — including a second void
    -- reason — would be a correction of a cancellation.
    IF OLD.status = 'void' THEN
        RAISE EXCEPTION 'voided adjustment note % is final', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;

    -- The cancellation evidence may only arrive with the cancellation: a reason written
    -- days earlier would describe a decision nobody had made yet.
    IF (NEW.void_reason IS DISTINCT FROM OLD.void_reason
        OR NEW.voided_at IS DISTINCT FROM OLD.voided_at)
       AND NEW.status IS NOT DISTINCT FROM OLD.status THEN
        RAISE EXCEPTION 'adjustment note % can only record a void reason when it is voided', OLD.id
            USING ERRCODE = 'restrict_violation';
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
        RAISE EXCEPTION 'adjustment note % cannot move from % to %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'restrict_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

COMMIT;
