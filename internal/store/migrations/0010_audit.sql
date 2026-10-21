-- 0010_audit.sql
--
-- Application audit log.
--
-- `auth_events` records authentication only. It cannot answer "who voided this
-- invoice, and why", which is the question every incident review starts with. This
-- table is that record.
--
-- Design decisions that are load-bearing:
--
--   * The row is written by the *domain service* inside the same transaction as the
--     change, not by the HTTP handler. A handler can be forgotten when a new route is
--     added; a service that writes money cannot, because the write is part of the
--     same function that moves the money.
--   * `changes` is a redacted diff. Secrets never reach this table: the redactor in
--     internal/audit runs over the map before the insert, and a field whose name looks
--     like a credential is replaced with a marker rather than truncated.
--   * Append-only, using the same trigger as the ledger. An audit trail that can be
--     edited is not an audit trail.
--   * `actor_id` is nullable so a system action (a worker, a sweeper) is still
--     attributable to a kind rather than silently attributed to a person.

CREATE TABLE audit_events (
    id             bigserial PRIMARY KEY,
    actor_id       uuid REFERENCES users (id),
    actor_kind     text NOT NULL CHECK (actor_kind IN ('user', 'api_key', 'system')),
    -- Dotted verb, e.g. invoice.void, refund.create, service.suspend. Not an enum:
    -- a new action must not require a migration, but it must be greppable.
    action         text NOT NULL CHECK (action <> ''),
    target_type    text NOT NULL CHECK (target_type <> ''),
    target_id      uuid,
    outcome        text NOT NULL CHECK (outcome IN ('success', 'failure', 'denied')),
    -- Redacted diff: field name -> old/new value, with credentials removed.
    changes        jsonb NOT NULL DEFAULT '{}'::jsonb,
    correlation_id text NOT NULL DEFAULT '',
    ip             inet,
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- "What happened to this record" is the primary read.
CREATE INDEX audit_events_target_idx ON audit_events (target_type, target_id, created_at DESC);
-- "What did this person do" is the secondary read, and the one a review asks for.
CREATE INDEX audit_events_actor_idx ON audit_events (actor_id, created_at DESC);
CREATE INDEX audit_events_action_idx ON audit_events (action, created_at DESC);
CREATE INDEX audit_events_created_idx ON audit_events (created_at DESC, id DESC);

CREATE TRIGGER audit_events_append_only
    BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();
