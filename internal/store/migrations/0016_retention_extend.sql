-- 0016_retention_extend.sql
--
-- Retention for the tables added by the web-UI gate.
--
-- Every table added here grows on every request or every login, so leaving them
-- without a policy reintroduces exactly the unbounded growth 0009 removed.
--
-- The periods are policy, not constants: a regulated deployment may be required to
-- keep delivery evidence longer, and changing that must not be a deploy.

INSERT INTO retention_policies (table_name, keep_for, age_column) VALUES
    -- An idempotency key only matters while a client might retry. The API advertises
    -- 24 hours; two days of history is generous and still bounded.
    ('idempotency_keys',        interval '2 days',   'created_at'),
    -- "What did we send, and when" is evidence for a support ticket. Half a year.
    ('notification_deliveries', interval '180 days', 'created_at'),
    -- Reset and verification tokens are dead after 30 minutes and 24 hours
    -- respectively; a week of history is enough to investigate a reported failure.
    ('password_resets',         interval '7 days',   'created_at'),
    ('email_verifications',     interval '7 days',   'created_at'),
    ('mfa_challenges',          interval '1 day',    'created_at')
ON CONFLICT (table_name) DO UPDATE
    SET keep_for = EXCLUDED.keep_for, age_column = EXCLUDED.age_column;

CREATE INDEX notification_deliveries_age_idx ON notification_deliveries (created_at);
CREATE INDEX password_resets_age_idx ON password_resets (created_at);
CREATE INDEX email_verifications_age_idx ON email_verifications (created_at);
CREATE INDEX mfa_challenges_age_idx ON mfa_challenges (created_at);
