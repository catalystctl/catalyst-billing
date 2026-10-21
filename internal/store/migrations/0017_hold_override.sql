-- 0017_hold_override.sql
--
-- What a second signature on a hold override needs to be auditable.
--
-- The decision row already recorded what the engine decided. It did not record *who*
-- decided it, because until now every decision was automatic. A hold override is the first
-- decision a human can make, and a human decision with no named human is not auditable: the
-- next incident review asks "who approved suspending a customer who was in a dispute", and
-- the answer has to be a person, not "the system".
--
-- approved_by is separate from decided_by on purpose. The two columns are the whole point of
-- dual approval: if one column held both, a row could not express "requested by A, approved
-- by B", which is the only shape that proves a second signature happened.

ALTER TABLE suspension_decisions
    ADD COLUMN decided_by   uuid REFERENCES users (id),
    ADD COLUMN approved_by  uuid REFERENCES users (id),
    ADD COLUMN override_hold boolean NOT NULL DEFAULT false;

ALTER TABLE suspension_decisions
    ADD CONSTRAINT suspension_decisions_distinct_actors
        CHECK (approved_by IS NULL OR approved_by <> decided_by);

-- An override must name both actors; an ordinary decision must not claim an approval.
ALTER TABLE suspension_decisions
    ADD CONSTRAINT suspension_decisions_override_consistency
        CHECK (NOT override_hold OR (decided_by IS NOT NULL AND approved_by IS NOT NULL));

CREATE INDEX suspension_decisions_override_idx ON suspension_decisions (service_id, created_at DESC)
    WHERE override_hold;
