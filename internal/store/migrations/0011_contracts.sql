-- 0011_contracts.sql
--
-- Transport contracts that are cheap now and expensive later: scoped idempotency
-- keys and the dual-approval record.
--
-- Both are schema rather than in-process state on purpose. An idempotency key held in
-- memory is useless the moment there are two instances or one restart, which is
-- exactly when a retried refund arrives. An approval held in memory cannot survive
-- the operator closing their laptop between the request and the second signature.

-- ---------------------------------------------------------------- idempotency --

-- One row per (principal, key). The key is *not* global: two customers using the
-- string "retry-1" must not collide, and a key must not be usable by another
-- principal to read someone else's stored response.
CREATE TABLE idempotency_keys (
    key           text NOT NULL,
    principal_id  uuid NOT NULL,
    -- "METHOD /pattern", so a key reused on a different route is a conflict rather
    -- than an answer to a different question.
    route         text NOT NULL,
    -- SHA-256 of the canonical request body. A replay with a different digest is a
    -- conflict, never the stored response.
    request_hash  bytea NOT NULL,
    -- 0 while the original request is still in flight; that is how a concurrent
    -- replay is distinguished from a completed one.
    response_code int NOT NULL,
    response_body jsonb NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    PRIMARY KEY (principal_id, key),
    CONSTRAINT idempotency_keys_code_sane CHECK (response_code >= 0 AND response_code < 600)
);

CREATE INDEX idempotency_keys_expiry_idx ON idempotency_keys (expires_at);

-- ------------------------------------------------------------------ approvals --

-- A high-value action performed by one person, confirmed by a different one.
--
-- The CHECK is the last line of defence: even a bug that sets approved_by from the
-- requesting session cannot produce a self-approval.
CREATE TABLE approvals (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    action        text NOT NULL CHECK (action IN ('refund', 'terminate_batch', 'hold_override')),
    reference_id  uuid NOT NULL,
    requested_by  uuid NOT NULL REFERENCES users (id),
    approved_by   uuid REFERENCES users (id),
    status        text NOT NULL CHECK (status IN ('pending', 'approved', 'rejected', 'expired')),
    reason        text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    decided_at    timestamptz,
    CONSTRAINT approvals_distinct_actors CHECK (approved_by IS NULL OR approved_by <> requested_by),
    CONSTRAINT approvals_decided_consistency CHECK (
        (status = 'pending' AND decided_at IS NULL AND approved_by IS NULL)
        OR (status IN ('approved', 'rejected') AND decided_at IS NOT NULL)
        OR (status = 'expired')
    )
);

CREATE INDEX approvals_pending_idx ON approvals (created_at DESC)
    WHERE status = 'pending';
CREATE INDEX approvals_reference_idx ON approvals (action, reference_id);
