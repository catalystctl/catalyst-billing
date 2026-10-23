-- Mail branding, the Discord mirror marker, and the problem-code catalogue (NTF-11, NTF-13,
-- I18N-03, I18N-06).
--
-- Three changes that belong together because they are the same idea in three places: a message
-- the product sends should carry the tenant's brand, should record whether it reached a channel
-- other than mail, and should be explainable in the reader's own language.
--
-- 1. `notification_deliveries.body_html` is the branded multipart/alternative part. It is stored
--    on the row rather than rendered by the drain because the drain runs later, in another
--    process: what it delivers must be what the transaction rendered, with the brand as it was
--    when the change committed. The default is the empty string, so every row written before this
--    migration (and every installation that turns branding off) is a plain-text message — the
--    column adds a representation, it never replaces the plain-text body, which stays in `body`.
--
-- 2. `notification_mirrors` is the marker that makes the Discord mirror post an event once. The
--    natural key is the outbox dedupe key, which is per event *instance*: a delivery row is per
--    recipient, so three CC'd addresses are three rows and would otherwise be three identical
--    posts in the channel, while a replayed webhook writes no new row and posts nothing.
--
-- 3. `error.<code>` catalogue rows. A problem document already carries a stable machine code; what
--    it does not carry is a sentence a non-English reader can read. The code is the translatable
--    unit — the English `detail` stays as the developer-facing explanation and as the client's
--    fallback — so every code the notification and locale surfaces emit gets a row here, and an
--    operator translates it on /locales like any other key. This is deliberately not an attempt
--    to translate Go's error strings: that boundary is why the code exists at all.

BEGIN;

-- ------------------------------------------------------------- mail branding --

ALTER TABLE notification_deliveries
    ADD COLUMN IF NOT EXISTS body_html text NOT NULL DEFAULT '';

COMMENT ON COLUMN notification_deliveries.body_html IS
    'The branded HTML alternative, or empty for a plain-text message. The plain-text body is always in `body`.';

-- ----------------------------------------------------------- discord mirror --

CREATE TABLE notification_mirrors (
    -- The outbox dedupe key of the event instance that was mirrored, e.g. "invoice_paid:<id>".
    dedupe_key  text PRIMARY KEY,
    -- The notification event key, kept for the reader ("what was this about") rather than as a
    -- foreign key: a mirror outlives a renamed event, and a row that cannot be joined is still a
    -- row that says "already posted".
    event_key   text NOT NULL CHECK (event_key <> ''),
    mirrored_at timestamptz NOT NULL DEFAULT now(),
    -- Why a post did not reach the channel. "The operator has not selected this event" and "the
    -- webhook returned 500" are different answers to "why is this missing", and only one is a
    -- defect.
    last_error  text
);

-- The drain asks "which settled deliveries have no mirror yet". The partial index keeps that
-- question proportional to the events that are waiting rather than to the mail history.
CREATE INDEX notification_deliveries_mirror_idx
    ON notification_deliveries (dedupe_key, created_at)
    WHERE dedupe_key IS NOT NULL AND status IN ('sent', 'suppressed');

-- ------------------------------------------------------- problem-code catalog --

INSERT INTO message_catalog (locale_code, key, text) VALUES
    ('en', 'error.unknown_placeholder', 'One of the message''s placeholders is not supplied by this event.'),
    ('en', 'error.force_event', 'This notification is required and cannot be turned off.'),
    ('en', 'error.unknown_event', 'There is no such notification event.'),
    ('en', 'error.template_missing', 'This notification has no message configured yet.'),
    ('en', 'error.not_found', 'That record was not found.'),
    ('en', 'error.invalid_cursor', 'The page cursor is not valid. Start from the first page.'),
    ('en', 'error.invalid_input', 'One or more fields are not valid.'),
    -- The per-field sentences the client composes a validation refusal from. The key mirrors
    -- `error.field.<code>` in web/src/lib/i18n.ts: the field NAME is substituted into `{{field}}`,
    -- so one row covers every input rather than one row per input.
    ('en', 'error.field.required', '{{field}} is required.'),
    ('en', 'error.field.invalid', '{{field}} is not valid.'),
    ('en', 'error.field.duplicate', '{{field}} is already in use.'),
    ('en', 'error.field.unknown', '{{field}} is not one this installation recognises.'),
    ('en', 'error.internal_error', 'The request could not be completed. Try again.'),
    ('en', 'error.authentication_required', 'Sign in to continue.'),
    ('en', 'error.forbidden', 'You do not have permission to do that.'),
    ('en', 'error.locale_not_enabled', 'That language is not offered by this installation.'),
    ('en', 'error.duplicate_locale', 'That language is already configured.'),
    ('en', 'error.default_locale', 'Make another language the default first.'),
    ('en', 'error.storage_unavailable', 'That needs a database, and none is configured.'),
    ('en', 'error.database_not_configured', 'That needs a database, and none is configured.'),
    ('en', 'error.rate_limited', 'Too many requests. Wait a moment and try again.'),
    ('en', 'error.notifications_unavailable', 'The notification queue is not configured on this installation.'),
    ('en', 'error.unknown_setting', 'That setting does not exist.'),
    ('en', 'error.invalid_value', 'One of the values is not valid.'),
    ('en', 'error.invalid_body', 'The request body is not valid JSON.')
ON CONFLICT (locale_code, key) DO NOTHING;

COMMIT;
