-- The per-field validation sentences, under the keys the client actually looks up.
--
-- These rows were first written into 0068 as `error.field_required` and `error.field_invalid`,
-- while `web/src/lib/i18n.ts` composes `error.field.<code>` — so every field error silently fell
-- back to English and nobody could tell from the screen that a translation existed. The fix could
-- not be an edit to 0068: that migration had already been applied, the runner records a checksum,
-- and a changed file makes every subsequent start refuse with "applied migration file has changed".
-- The rule is not bureaucracy — rewriting an applied migration means two installations that both
-- claim to be at the same version have different schemas, and there is no way to tell them apart.
--
-- So the correction is here, as a new migration: retract the two keys nothing reads, and insert the
-- family the client asks for. The obsolete rows are deleted rather than left in place, because a
-- catalogue that carries both spellings invites the next person to populate the wrong one.
--
-- `text` carries `{{field}}`: the field NAME is substituted into it, so one row covers every input
-- in the product instead of one row per input.

DELETE FROM message_catalog
 WHERE locale_code = 'en'
   AND key IN ('error.field_required', 'error.field_invalid');

INSERT INTO message_catalog (locale_code, key, text) VALUES
    ('en', 'error.field.required', '{{field}} is required.'),
    ('en', 'error.field.invalid', '{{field}} is not valid.'),
    ('en', 'error.field.duplicate', '{{field}} is already in use.'),
    ('en', 'error.field.unknown', '{{field}} is not one this installation recognises.'),

    -- The refusals the settings and notification surfaces return as problem codes. Each one is a
    -- different answer to "why did that not save", and a generic "invalid" for all four is the
    -- reason an operator ends up reading the server log to find out.
    ('en', 'error.notifications_unavailable', 'The notification queue is not configured on this installation.'),
    ('en', 'error.unknown_setting', 'That setting does not exist.'),
    ('en', 'error.invalid_value', 'One of the values is not valid.'),
    ('en', 'error.invalid_body', 'The request body is not valid JSON.')
ON CONFLICT (locale_code, key) DO UPDATE SET text = excluded.text;
