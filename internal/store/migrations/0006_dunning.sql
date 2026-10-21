-- 0006_dunning.sql
--
-- Dunning and suspension eligibility.
--
-- Suspending a paying customer is the most damaging mistake this product can
-- make, so the decision is persisted and reviewable rather than inferred at the
-- moment of action:
--
--   * each open invoice has an explicit dunning state with a step counter;
--   * a service carries manual holds that no automated job may override;
--   * the suspension decision re-reads the balance, the holds and the
--     cancellation state inside the same transaction that enqueues the work.
--
-- The state is persisted so that a restart, a redeploy or a second worker cannot
-- skip a step or repeat one.

CREATE TABLE dunning_states (
    invoice_id      uuid PRIMARY KEY REFERENCES invoices (id),
    -- open: within terms; overdue: past due, reminders running;
    -- exhausted: the retry budget is spent and suspension is permitted.
    state           text NOT NULL CHECK (state IN ('open', 'overdue', 'exhausted', 'resolved')),
    step            int  NOT NULL DEFAULT 0 CHECK (step >= 0),
    -- When the next dunning action is due. Null means nothing is scheduled.
    next_action_at  timestamptz,
    last_action_at  timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX dunning_due_idx ON dunning_states (next_action_at)
    WHERE state IN ('overdue') AND next_action_at IS NOT NULL;

-- Manual holds are an operator's veto over automated destructive actions.
CREATE TABLE service_holds (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id  uuid NOT NULL REFERENCES services (id),
    reason      text NOT NULL,
    -- A hold with no expiry is deliberate and must be cleared by a human.
    expires_at  timestamptz,
    created_by  text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX service_holds_service_idx ON service_holds (service_id);

-- Every escalation is recorded, so "why was this customer suspended" has an
-- answer that is not a guess.
CREATE TABLE suspension_decisions (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id     uuid NOT NULL REFERENCES services (id),
    invoice_id     uuid REFERENCES invoices (id),
    -- suspend: the work was enqueued; held: a reason prevented it.
    decision       text NOT NULL CHECK (decision IN ('suspend', 'held')),
    reason         text NOT NULL,
    -- The service generation at decision time. A later generation invalidates
    -- this decision, which is what stops a stale worker.
    generation     bigint NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX suspension_decisions_service_idx ON suspension_decisions (service_id, created_at DESC);
