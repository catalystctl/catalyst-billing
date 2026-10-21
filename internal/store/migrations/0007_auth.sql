-- 0007_auth.sql
--
-- Accounts, sessions and staff authorization.
--
-- Design decisions that are load-bearing:
--
--   * A session token is stored only as a hash. A database leak must not yield
--     usable session cookies.
--   * Sessions are server-side and revocable. Logout and "sign out everywhere"
--     must actually work, which a stateless token cannot offer.
--   * Every session carries its own CSRF secret, so a CSRF token cannot be
--     replayed across sessions.
--   * Authorization is deny-by-default: a permission is granted by a role, and an
--     unknown permission is a denial, not an allowance.
--   * Login attempts are recorded for throttling. The record is per-account and
--     per-address so neither a single account nor a single source can be hammered.

CREATE TABLE users (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email             text NOT NULL,
    -- Argon2id encoded hash: algorithm, parameters and salt travel with it, so
    -- parameters can be raised later without a schema change.
    password_hash     text NOT NULL,
    display_name      text NOT NULL DEFAULT '',
    -- Null until the address is confirmed. Verification is a product decision;
    -- the column exists so it cannot be forgotten.
    email_verified_at timestamptz,
    -- active: normal; disabled: cannot authenticate; pending: awaiting verification.
    status            text NOT NULL DEFAULT 'active'
                      CHECK (status IN ('active', 'disabled', 'pending')),
    -- Bumped on any credential change; sessions issued before it are rejected.
    credential_version int NOT NULL DEFAULT 1,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- Case-insensitive uniqueness: two accounts must not differ only by case.
CREATE UNIQUE INDEX users_email_unique ON users (lower(email));

-- A customer account is a billing entity, which may have several people attached
-- to it. v1 always has exactly one owner, but the join table is the shape that
-- lets a second contact be added later without a migration.
CREATE TABLE customer_accounts (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users (id),
    -- Denormalised link to the billing customer record.
    customer_id uuid NOT NULL REFERENCES customers (id),
    role       text NOT NULL DEFAULT 'owner' CHECK (role IN ('owner', 'billing', 'technical')),
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT customer_accounts_unique UNIQUE (user_id, customer_id)
);

CREATE INDEX customer_accounts_customer_idx ON customer_accounts (customer_id);

-- Staff roles are a closed set. Permissions are derived from the role in code so
-- that adding a permission to a role is a reviewed code change, not a data edit.
CREATE TABLE staff_roles (
    user_id    uuid PRIMARY KEY REFERENCES users (id),
    role       text NOT NULL CHECK (role IN ('administrator', 'billing', 'support', 'infrastructure')),
    granted_by uuid REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- SHA-256 of the session token. The token itself is never stored.
    token_hash    bytea NOT NULL,
    -- Per-session CSRF secret, so a token from one session is useless in another.
    csrf_secret   bytea NOT NULL,
    -- The credential_version at issue time. A password change invalidates every
    -- session issued before it.
    credential_version int NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    revoked_at    timestamptz,
    -- Recorded for the operator's "where am I signed in" view and for triage.
    user_agent    text NOT NULL DEFAULT '',
    ip            inet,
    CONSTRAINT sessions_expiry_after_creation CHECK (expires_at > created_at)
);

CREATE UNIQUE INDEX sessions_token_unique ON sessions (token_hash);
CREATE INDEX sessions_user_idx ON sessions (user_id) WHERE revoked_at IS NULL;
CREATE INDEX sessions_expiry_idx ON sessions (expires_at) WHERE revoked_at IS NULL;

-- Login throttling. One row per (bucket, subject) with a rolling counter.
CREATE TABLE login_attempts (
    id           bigserial PRIMARY KEY,
    -- 'account' or 'address': throttling an account protects the account,
    -- throttling an address protects against one source spraying many accounts.
    bucket       text NOT NULL CHECK (bucket IN ('account', 'address')),
    subject      text NOT NULL,
    succeeded    boolean NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX login_attempts_lookup_idx ON login_attempts (bucket, subject, created_at DESC);

-- Append-only audit trail for authentication events. These are the events an
-- incident review always wants and a normal request log never has.
CREATE TABLE auth_events (
    id         bigserial PRIMARY KEY,
    user_id    uuid REFERENCES users (id),
    event      text NOT NULL,
    detail     text NOT NULL DEFAULT '',
    ip         inet,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX auth_events_user_idx ON auth_events (user_id, created_at DESC);
