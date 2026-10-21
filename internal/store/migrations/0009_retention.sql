-- 0009_retention.sql
--
-- Retention for authentication bookkeeping.
--
-- login_attempts and auth_events grow with every login, successful or not, and
-- nothing ever deleted them. On a panel exposed to the internet that is an
-- unbounded write amplification an attacker can drive: three rows per failed attempt,
-- forever.
--
-- The retention periods are policy, not constants in code, because a regulated
-- deployment may be required to keep authentication records longer.

CREATE TABLE retention_policies (
    table_name   text PRIMARY KEY,
    -- How long rows are kept. Zero means "keep forever", which must be an explicit
    -- choice rather than a default.
    keep_for     interval NOT NULL CHECK (keep_for > interval '0'),
    -- The column that carries the row's age.
    age_column   text NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now()
);

INSERT INTO retention_policies (table_name, keep_for, age_column) VALUES
    -- Failed-attempt rows only matter while they are inside the throttling window,
    -- which is minutes; a day is generous and keeps incident review possible.
    ('login_attempts', interval '1 day',   'created_at'),
    -- Authentication events are the audit trail an incident review reads, so they
    -- are kept much longer than the counters.
    ('auth_events',    interval '180 days', 'created_at'),
    -- Processed webhook payloads are the evidence for a payment; they are kept long
    -- enough to answer a dispute.
    ('gateway_events', interval '400 days', 'received_at');

-- Supports the retention sweep without a sequential scan.
CREATE INDEX login_attempts_age_idx ON login_attempts (created_at);
CREATE INDEX auth_events_age_idx ON auth_events (created_at);
