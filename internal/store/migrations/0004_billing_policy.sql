-- 0004_billing_policy.sql
--
-- Billing policy as data, not as constants in code.
--
-- Every one of these values changes customer outcomes (when an invoice appears,
-- when it is late, when a service is suspended), so they must be reviewable,
-- auditable and changeable without a deploy. v1 has a single row: one merchant,
-- one policy. Per-customer overrides are a later feature.

CREATE TABLE billing_policies (
    id                     text PRIMARY KEY,
    -- How many days before the period start the renewal invoice is created.
    invoice_lead_days      int NOT NULL CHECK (invoice_lead_days BETWEEN 0 AND 90),
    -- Days from issue until the invoice is due.
    payment_terms_days     int NOT NULL CHECK (payment_terms_days BETWEEN 0 AND 90),
    -- Days after the due date before the invoice counts as overdue.
    grace_days             int NOT NULL CHECK (grace_days BETWEEN 0 AND 90),
    -- Catch-up limits. A worker that has been down for a month must not silently
    -- charge every affected customer; oversized catch-up goes to review.
    catchup_max_periods    int NOT NULL CHECK (catchup_max_periods BETWEEN 1 AND 36),
    catchup_max_age_days   int NOT NULL CHECK (catchup_max_age_days BETWEEN 0 AND 365),
    updated_at             timestamptz NOT NULL DEFAULT now()
);

INSERT INTO billing_policies (id, invoice_lead_days, payment_terms_days, grace_days,
                              catchup_max_periods, catchup_max_age_days)
VALUES ('default', 7, 7, 3, 2, 30);

ALTER TABLE subscriptions
    -- Cancellation at period end is a scheduled intent, not an immediate state
    -- change: the subscription stays active and still renews its final period.
    ADD COLUMN cancel_at_period_end boolean NOT NULL DEFAULT false,
    ADD COLUMN cancelled_at timestamptz,
    ADD COLUMN ended_at timestamptz;

-- The due-work scanner reads this index.
CREATE INDEX subscriptions_due_idx ON subscriptions (next_billing_at)
    WHERE status IN ('active', 'past_due');

-- Every renewal run records what it did, so an unexpected charge can be traced
-- to the run that produced it.
CREATE TABLE renewal_runs (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    started_at     timestamptz NOT NULL DEFAULT now(),
    finished_at    timestamptz,
    scanned        int NOT NULL DEFAULT 0,
    invoiced       int NOT NULL DEFAULT 0,
    skipped        int NOT NULL DEFAULT 0,
    needs_review   int NOT NULL DEFAULT 0,
    error          text
);

CREATE TABLE renewal_events (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id          uuid NOT NULL REFERENCES renewal_runs (id),
    subscription_id uuid NOT NULL REFERENCES subscriptions (id),
    invoice_id      uuid REFERENCES invoices (id),
    period_key      text NOT NULL,
    outcome         text NOT NULL CHECK (outcome IN
                        ('invoiced', 'skipped_cancelled', 'skipped_not_due', 'needs_review', 'failed')),
    detail          text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- A run may only record one outcome per subscription per period.
    CONSTRAINT renewal_events_unique UNIQUE (run_id, subscription_id, period_key)
);
