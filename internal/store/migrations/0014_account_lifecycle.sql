-- 0014_account_lifecycle.sql
--
-- What a real login screen assumes exists: password reset, email verification,
-- durable notification delivery, TOTP MFA and API keys.
--
-- Two rules run through all of it:
--
--   * Every token is stored as a hash, is single use, and expires. A database leak
--     must not hand anyone a working reset link, recovery code or API key.
--   * Nothing here may be answered differently for a known and an unknown account.
--     Non-enumeration is a schema-level property here: the request row simply is not
--     written for an unknown address, and the response is identical.

CREATE TABLE password_resets (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash bytea NOT NULL,
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT password_resets_expiry CHECK (expires_at > created_at)
);

CREATE UNIQUE INDEX password_resets_token_unique ON password_resets (token_hash);
CREATE INDEX password_resets_user_idx ON password_resets (user_id);

-- Verification is not required to sign in: an operator created by CLI must be able to
-- work before mail is configured. It is required before MFA enrolment and before the
-- account may pay.
CREATE TABLE email_verifications (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash bytea NOT NULL,
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT email_verifications_expiry CHECK (expires_at > created_at)
);

CREATE UNIQUE INDEX email_verifications_token_unique ON email_verifications (token_hash);
CREATE INDEX email_verifications_user_idx ON email_verifications (user_id);

-- ------------------------------------------------------------ notifications --

-- Durable outbox. The row is written in the same transaction as the domain change, so
-- a crash between "invoice issued" and "mail sent" cannot lose the message, and the
-- unique dedupe_key makes a retry incapable of sending it twice.
--
-- `suppressed` exists because "we deliberately did not send it" and "we failed to send
-- it" are different answers to a support ticket, and only one of them is a bug.
CREATE TABLE notification_deliveries (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind       text NOT NULL CHECK (kind IN
                   ('password_reset', 'email_verification', 'invoice_issued', 'dunning_reminder')),
    to_address text NOT NULL CHECK (to_address <> ''),
    subject    text NOT NULL,
    -- The rendered body is stored so "what did we actually send" is answerable.
    body       text NOT NULL,
    status     text NOT NULL CHECK (status IN ('queued', 'sending', 'sent', 'failed', 'suppressed')),
    attempts   int NOT NULL DEFAULT 0,
    last_error text,
    dedupe_key text UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    -- Backoff schedule. A failed delivery is retried when this instant passes, so a
    -- dead SMTP server does not make every pass retry every message.
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    sent_at    timestamptz,
    CONSTRAINT notification_deliveries_sent_consistency CHECK (
        (status = 'sent' AND sent_at IS NOT NULL) OR status <> 'sent'
    )
);

CREATE INDEX notification_deliveries_pending_idx ON notification_deliveries (next_attempt_at)
    WHERE status IN ('queued', 'failed', 'sending');

-- ------------------------------------------------------------------- MFA --

-- The secret is encrypted with the core key, not stored as text: a backup that leaks
-- must not let an attacker generate valid codes for every staff account.
CREATE TABLE mfa_credentials (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind         text NOT NULL CHECK (kind IN ('totp')),
    secret_enc   bytea NOT NULL,
    -- Which key encrypted it, so a rotation can re-wrap without re-enrolling.
    key_id       text NOT NULL,
    confirmed_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT mfa_one_per_kind UNIQUE (user_id, kind)
);

CREATE TABLE mfa_recovery_codes (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash bytea NOT NULL,
    used_at   timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX mfa_recovery_codes_hash_unique ON mfa_recovery_codes (code_hash);
CREATE INDEX mfa_recovery_codes_user_idx ON mfa_recovery_codes (user_id);

-- The second login step. A challenge proves "the password was correct" and nothing
-- else: it carries no session, is single use, and expires in minutes.
CREATE TABLE mfa_challenges (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash bytea NOT NULL,
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT mfa_challenges_expiry CHECK (expires_at > created_at)
);

CREATE UNIQUE INDEX mfa_challenges_token_unique ON mfa_challenges (token_hash);

-- -------------------------------------------------------------- API keys --

-- The key itself is never stored: only its SHA-256 and a short prefix for display.
-- `scopes` can only narrow what the owner holds, never widen it, so revoking a role
-- immediately narrows every key that user owns.
CREATE TABLE api_keys (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         text NOT NULL CHECK (name <> ''),
    prefix       text NOT NULL,
    key_hash     bytea NOT NULL,
    scopes       text[] NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    expires_at   timestamptz,
    revoked_at   timestamptz
);

CREATE UNIQUE INDEX api_keys_hash_unique ON api_keys (key_hash);
CREATE INDEX api_keys_user_idx ON api_keys (user_id) WHERE revoked_at IS NULL;
