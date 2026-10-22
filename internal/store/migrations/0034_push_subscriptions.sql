-- 0034_push_subscriptions.sql
--
-- Web push: one row per browser subscription, so a customer can be told about an invoice on
-- the device they are holding rather than only by mail.
--
-- Five decisions shape this file, and each is recorded because the next person will
-- otherwise "simplify" it back into a defect:
--
-- 1. **The endpoint is the key, and it is unique across the whole table, not per user.** A
--    push endpoint is issued by the browser's push service to one subscription on one device;
--    it does not belong to an account. Two accounts signing in on the same laptop share the
--    same endpoint. If the key were (user_id, endpoint) then signing out and signing in as
--    someone else would leave *both* rows alive and the device would receive the previous
--    account's mail — a cross-account leak wearing the previous user's name. With the endpoint
--    as the key, re-subscribing replaces the row, including its owner: the device now belongs
--    to whoever just subscribed it. That is the same rule the browser already applies to the
--    session cookie in that browser.
--
-- 2. **p256dh and auth are stored as the browser sent them, not hashed.** They are not
--    credentials: they are the *encryption* inputs. RFC 8291 derives a per-message key from
--    the device's P-256 public key and this shared auth secret, so the server needs the exact
--    bytes to encrypt at all — a hash would be a column nothing could use. What makes them
--    safe to store is that they are useless without the device's private key, which never
--    leaves the browser, and that the push service can revoke the endpoint at any time.
--
-- 3. **A dead device is deleted, not flagged.** A push service answering 404 or 410 means the
--    subscription no longer exists: the user cleared site data, uninstalled the browser, or the
--    service expired it. There is no retry that can succeed, so there is no `failure_count`
--    and no `last_error` column: the row goes. A `status` column here would be a queue of
--    permanently undeliverable rows that every send walks past.
--
-- 4. **last_used_at is the only liveness signal kept.** It is written when a push is
--    successfully handed to the push service, which is what lets the portal show "this device
--    last received a notice in March" and lets an operator tell a stale laptop from an active
--    phone without guessing from created_at.
--
-- 5. **The endpoint is a URL this server POSTs to, so it must be https.** The value comes from
--    the browser and is therefore attacker-influenced, and the server makes an outbound
--    request to it: that is a server-side request forgery surface by construction. The CHECK
--    refuses anything that is not an absolute https URL, the client follows no redirects, and
--    the request has a short timeout. That does not make SSRF impossible — an attacker can
--    still point the server at an https host they control — but it bounds the damage to
--    "a request to a public TLS endpoint", and the alternative (a hard-coded allowlist of push
--    services) would break every browser whose push service is not on the list.
--
-- The row is owned by the user and goes when the user does: ON DELETE CASCADE is what makes
-- "delete this account" complete without a second cleanup step that someone will forget.

BEGIN;

CREATE TABLE push_subscriptions (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The account that will receive the notification. It is not part of the key; see (1).
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- The push service URL. Unique: it identifies one device subscription (see (1)).
    endpoint    text NOT NULL,
    -- The device's P-256 public key, base64url, uncompressed point (65 bytes). RFC 8291.
    p256dh      text NOT NULL,
    -- The 16-byte shared auth secret, base64url. RFC 8291.
    auth        text NOT NULL,
    -- What the browser said it was, so the portal can name the device. Truncated at the write
    -- path; it is a label, never parsed.
    user_agent  text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    -- Null until a push has been handed to the push service (see (4)).
    last_used_at timestamptz,

    CONSTRAINT push_subscriptions_endpoint_key UNIQUE (endpoint),

    -- https only (see (5)). The length bound keeps a single row from becoming a blob store.
    CONSTRAINT push_subscriptions_endpoint_shape
        CHECK (endpoint ~ '^https://[^[:space:]]+$' AND length(endpoint) <= 2048),
    -- A p256dh is an uncompressed P-256 point in base64url: 65 bytes, so 87 characters with no
    -- padding. The check is on the shape, not the curve: the Go write path decodes the point
    -- and refuses a value that is not on the curve, and this is the guard for a writer that
    -- bypassed it.
    CONSTRAINT push_subscriptions_p256dh_shape CHECK (p256dh ~ '^[A-Za-z0-9_-]{87,120}$'),
    -- 16 bytes in base64url is 22 characters without padding; some browsers send 24 with.
    CONSTRAINT push_subscriptions_auth_shape CHECK (auth ~ '^[A-Za-z0-9_-]{16,64}$'),
    CONSTRAINT push_subscriptions_user_agent_len CHECK (length(user_agent) <= 300)
);

-- The one query the sender makes: every device for one account. The index carries created_at so
-- the portal's "devices on this account" list is served from the index rather than a sort.
CREATE INDEX push_subscriptions_user_idx
    ON push_subscriptions (user_id, created_at DESC);

COMMIT;
