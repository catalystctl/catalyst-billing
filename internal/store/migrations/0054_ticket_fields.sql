-- 0054_ticket_fields.sql
--
-- Three gaps in the support desk, all of them "the endpoint exists but the thing behind it does
-- not":
--
--   1. Staff could reply to, assign and close a ticket but not *open* one. A customer on the
--      phone cannot be helped by an agent who has to ask them to submit a form. `tickets` gains
--      `opened_by_staff`, so a ticket opened by the team is distinguishable from one the customer
--      wrote — permanently, because "who opened this" is what an audit asks six months later.
--
--   2. There was no way to say what a ticket must ask for. `ticket_fields` is that definition.
--
--   3. There was nowhere to put the answer. `ticket_field_values` is that, and the rest of this
--      comment is about why it is shaped the way it is.
--
-- ## Why the values are typed columns, not a JSON blob
--
-- The obvious schema is `tickets.custom_fields jsonb` and a definition table. It is also the one
-- that makes every question an operator will ask unanswerable: "which open tickets are about
-- eu-west?" is a JSON path expression with no index and no type, and "is this value one of the
-- options the field offers" is a check nobody writes. A field that cannot be queried or validated
-- is a text box with extra steps.
--
-- So a value is a row with `field_type` copied from its definition and exactly one of
-- `value_text` / `value_bool` set. That gives three things a JSON blob cannot:
--
--   * `ticket_field_values_shape_check` — a CHECK constraint, not a convention. A checkbox row
--     cannot hold a string; a text row cannot hold a boolean; and an empty answer is not a row at
--     all (`btrim(value_text) <> ''`), which is what makes "answered" a fact rather than a
--     judgement call. The close-time requirement in internal/tickets/fields.go is then a
--     `NOT EXISTS` on this table and nothing more.
--   * `ticket_field_values (field_id, value_text)` is an index, so "every ticket with region =
--     eu-west" is a lookup.
--   * `field_type` is denormalised *and kept in step by a trigger*, because a CHECK cannot join.
--     Without the trigger the copy would be a lie the first time a definition's type changed;
--     with it, the type cannot change while any ticket carries a value.
--
-- ## Why a select's option set is enforced by a trigger
--
-- `def.options ? NEW.value_text` is a real membership test, and it belongs in the database for the
-- same reason a foreign key does: the service layer checks it too (so an operator gets a sentence
-- instead of a SQLSTATE), but a second writer — an importer, a script, a future endpoint — cannot
-- store a value the field does not offer. The same trigger refuses a write to an archived field,
-- and `ticket_fields_guard_options` refuses an option removal that would orphan an answered
-- ticket. Archiving rather than deleting is the ordinary lifecycle, so the foreign key is
-- RESTRICT: a definition that tickets point at is not deletable, it is archivable.
--
-- Everything here is additive and idempotent: no column changes type, no row is rewritten.

BEGIN;

-- =========================================================== staff-opened ====
--
-- A ticket the team opened for a customer. `opened_by` already holds the login that opened it
-- (the staff user, in this case); this flag is what distinguishes that from a customer who wrote
-- in, and the CHECK makes "staff-opened" and "nobody opened it" impossible to combine — a
-- staff-opened ticket always names the staff member.

ALTER TABLE tickets
    ADD COLUMN IF NOT EXISTS opened_by_staff boolean NOT NULL DEFAULT false;

ALTER TABLE tickets
    DROP CONSTRAINT IF EXISTS tickets_opened_by_staff_check,
    ADD CONSTRAINT tickets_opened_by_staff_check
        CHECK (NOT opened_by_staff OR opened_by IS NOT NULL);

-- The queue's "we opened this on their behalf" filter. Partial: staff-opened tickets are the
-- minority of a support desk's history.
CREATE INDEX IF NOT EXISTS tickets_opened_by_staff_idx
    ON tickets (created_at DESC, id DESC)
    WHERE opened_by_staff;

-- ======================================================== field definitions ====
--
-- A field is text, select or checkbox, and two independent booleans decide when it is required:
-- at creation, and to close. They are independent on purpose — "what is your server name?" is
-- required to open; "was this resolved to your satisfaction?" is required to close — and a field
-- may be either, both, or neither.
--
-- `key` is the stable slug the API and the stored value speak. It is immutable in practice
-- (renaming it would break any client that sends it) and the CHECK keeps it to a shape that is
-- safe in a URL and in JSON without escaping.

CREATE TABLE IF NOT EXISTS ticket_fields (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    key         text NOT NULL UNIQUE
                CHECK (key ~ '^[a-z][a-z0-9_]{1,63}$'),
    label       text NOT NULL
                CHECK (length(btrim(label)) BETWEEN 1 AND 120),
    field_type  text NOT NULL
                CHECK (field_type IN ('text', 'select', 'checkbox')),
    -- A select's option set. NULL for the other two types: an option list on a text field is a
    -- value nobody reads, and the CHECK refuses it rather than letting it rot.
    options     jsonb,
    required_on_create boolean NOT NULL DEFAULT false,
    required_to_close  boolean NOT NULL DEFAULT false,
    sort_order  int NOT NULL DEFAULT 100 CHECK (sort_order >= 0),
    -- Archiving retires a field: it stops being asked and stops being required, and the answers
    -- already given stay readable. Deleting would erase history a closed ticket points at.
    archived_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT ticket_fields_options_check CHECK (
        (field_type = 'select'
            AND jsonb_typeof(options) = 'array'
            AND jsonb_array_length(options) BETWEEN 1 AND 50
            -- Every element is a non-empty string. Duplicates are refused by the service layer
            -- (a CHECK cannot deduplicate without a function, and a function would be a second
            -- place for the rule to live); the database's job here is that the array is a list
            -- of usable options rather than, say, an object or a list of numbers.
            AND NOT jsonb_path_exists(options, '$[*] ? (@.type() != "string" || @ == "")'))
        OR (field_type <> 'select' AND options IS NULL)
    ),

    -- A field whose key collides with a column of the ticket would be ambiguous on the wire:
    -- `fields.subject` next to the ticket's own `subject` is a bug waiting to be written.
    CONSTRAINT ticket_fields_reserved_key_check CHECK (
        key NOT IN ('subject', 'department', 'priority', 'status', 'service_id',
                    'message', 'body', 'customer_id', 'assignee', 'attachment')
    )
);

-- The list the settings screen and the create form both read: unarchived, in display order.
CREATE INDEX IF NOT EXISTS ticket_fields_order_idx
    ON ticket_fields (sort_order, key)
    WHERE archived_at IS NULL;

-- ============================================================= field values ====
--
-- One answer. The primary key is (ticket, field): a ticket answers a field once, and answering it
-- again is an UPDATE, so "required to close" can never be satisfied twice or ambiguously.

CREATE TABLE IF NOT EXISTS ticket_field_values (
    ticket_id  uuid NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    field_id   uuid NOT NULL REFERENCES ticket_fields (id) ON DELETE RESTRICT,
    -- Copied from the definition so the shape CHECK below is a constraint rather than a trigger.
    -- ticket_field_values_guard keeps it honest.
    field_type text NOT NULL
               CHECK (field_type IN ('text', 'select', 'checkbox')),
    value_text text,
    value_bool boolean,
    answered_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (ticket_id, field_id),

    CONSTRAINT ticket_field_values_shape_check CHECK (
        (field_type IN ('text', 'select')
            AND value_text IS NOT NULL
            AND length(value_text) <= 2000
            -- An empty answer is the absence of a row, not a row with an empty string: that is
            -- what lets the close-time requirement be `NOT EXISTS`.
            AND btrim(value_text) <> ''
            AND value_bool IS NULL)
        OR (field_type = 'checkbox'
            AND value_bool IS NOT NULL
            AND value_text IS NULL)
    )
);

-- "Every open ticket whose region is eu-west" is this index, not a JSON path.
CREATE INDEX IF NOT EXISTS ticket_field_values_lookup_idx
    ON ticket_field_values (field_id, value_text)
    WHERE value_text IS NOT NULL;

-- A ticket's own answers, read with the thread.
CREATE INDEX IF NOT EXISTS ticket_field_values_ticket_idx
    ON ticket_field_values (ticket_id);

-- ------------------------------------------------------------- the guard --

-- Why a trigger and not a CHECK: a CHECK cannot read another table, so it cannot know what type
-- the definition says, what options it offers, or whether it has been archived. Every write to an
-- answer goes through this function, whichever code path made it.
CREATE OR REPLACE FUNCTION ticket_field_values_guard() RETURNS trigger AS $$
DECLARE
    def ticket_fields%ROWTYPE;
BEGIN
    SELECT * INTO def FROM ticket_fields WHERE id = NEW.field_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ticket field % does not exist', NEW.field_id
            USING ERRCODE = 'foreign_key_violation';
    END IF;

    IF def.field_type <> NEW.field_type THEN
        RAISE EXCEPTION 'ticket field "%" is a % field, not a % field',
            def.key, def.field_type, NEW.field_type
            USING ERRCODE = 'check_violation';
    END IF;

    IF def.archived_at IS NOT NULL THEN
        -- Reading an archived field's existing answer is fine (the row is already there and
        -- unchanged); writing a new one is not.
        IF TG_OP = 'INSERT'
           OR NEW.value_text IS DISTINCT FROM OLD.value_text
           OR NEW.value_bool IS DISTINCT FROM OLD.value_bool THEN
            RAISE EXCEPTION 'ticket field "%" is archived and takes no new answers', def.key
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    IF NEW.field_type = 'select' AND NOT (def.options ? NEW.value_text) THEN
        RAISE EXCEPTION 'ticket field "%" does not offer the value "%"', def.key, NEW.value_text
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS ticket_field_values_guard ON ticket_field_values;
CREATE TRIGGER ticket_field_values_guard
    BEFORE INSERT OR UPDATE ON ticket_field_values
    FOR EACH ROW EXECUTE FUNCTION ticket_field_values_guard();

-- The other direction: a definition change must not strand the answers already given. Removing an
-- option that a ticket selected, or changing the type under an answered ticket, is refused — an
-- operator adds an option, or archives the field and creates a new one.
CREATE OR REPLACE FUNCTION ticket_fields_guard_change() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.field_type <> OLD.field_type
           AND EXISTS (SELECT 1 FROM ticket_field_values v WHERE v.field_id = NEW.id) THEN
            RAISE EXCEPTION 'ticket field "%" already has answers, so its type cannot change', NEW.key
                USING ERRCODE = 'check_violation';
        END IF;

        IF NEW.field_type = 'select'
           AND NEW.options IS DISTINCT FROM OLD.options
           AND EXISTS (
                SELECT 1 FROM ticket_field_values v
                 WHERE v.field_id = NEW.id
                   AND v.value_text IS NOT NULL
                   AND NOT (NEW.options ? v.value_text)) THEN
            RAISE EXCEPTION 'ticket field "%" has an answer that the new option list does not offer', NEW.key
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS ticket_fields_guard_change ON ticket_fields;
CREATE TRIGGER ticket_fields_guard_change
    BEFORE UPDATE ON ticket_fields
    FOR EACH ROW EXECUTE FUNCTION ticket_fields_guard_change();

COMMIT;
