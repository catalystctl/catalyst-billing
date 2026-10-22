-- 0029_service_metadata.sql
--
-- What a service *is*, in words: a label a customer chose, a description of what the box
-- actually is, and the operator-defined properties a support agent needs before they can
-- answer a single question about it.
--
-- Until now `services` was (customer, product, status, config). That is enough to provision
-- and to bill, and it is not enough to *talk about*: a customer cannot name the server they
-- rent, a support agent cannot see that it is the 8 GB plan in rack B, and a document cannot
-- say which installation it belongs to. Five decisions shape this file.
--
-- 1. **An empty string means "nobody has written one", which is why the new text columns are
--    NOT NULL and default to ''. `label` and `description` record only what a human typed;
--    the read path derives the rest (the product's name for a label, the product plus the
--    selected config options for a description) so a service is never blank. NULL would make
--    "the customer cleared their label" and "the customer never had one" the same value, and
--    the UI would have to special-case both.
--
-- 2. **Custom properties are a definition plus a value.** `services.custom_properties` holds
--    the values — one JSON object per service, as the parity backlog asks — and the *shape*
--    lives in `service_property_definitions`: the label a human reads, the order it prints in,
--    whether it may appear on a document, whether the customer may see it. A value with no
--    definition cannot be rendered (there is no label) and cannot be trusted to be
--    document-safe, so a trigger refuses to store one. That is why the writer refuses an
--    unknown key rather than storing an orphan the UI would silently drop.
--
-- 3. **"Show on the invoice" is a property of the definition, not a global setting.**
--    `service_property_definitions.show_on_invoice` is the mechanism the parity item asks
--    for. The alternative — one `services.show_properties_on_invoice` setting — makes the
--    decision installation-wide, and the whole point of a custom property is that it is the
--    operator's own field: a rack number belongs on the document, an internal asset tag does
--    not. A property is printed exactly when its definition says so *and* the service carries
--    a value; nothing is printed by default, and `customer_visible` separately controls
--    whether the customer sees it on their own service page.
--
-- 4. **A document never receives a raw string.** The database rejects a control character in
--    a property value, a label or a description (jsonpath `like_regex` and a character-class
--    CHECK, because a JSON string's bytes are not visible to a plain regex), and caps a value
--    at 200 characters and the whole object at 8 KiB. The render path sanitises again —
--    control characters stripped, whitespace collapsed, length capped — because the value
--    that reaches a document may have been written before this file existed, and because the
--    customer-authored label and description are the same hazard with a different author.
--    `service.InvoiceProperties` is the only supported way to get property values out for a
--    document.
--
-- 5. **Quantity and order belong on the row.** SVC-07 asks a support agent for the product,
--    plan, quantity and price. The product is `product_id` and the plan and price come through
--    the subscription's `price_version_id`, which the row already carries. Quantity was only
--    ever on `order_items`, and the order was not on the row at all, so "how many of these did
--    they buy, and on which order" needed a join through a subscription that a manual invoice
--    does not have. Both are added, `order_id` is backfilled from the invoice that shares the
--    service's subscription, and both stay nullable/defaulted for the services that predate
--    the column: a wrong quantity is worse than an honest 1.
--
-- Nothing here rewrites an issued invoice, a journal entry or an audit row.

BEGIN;

-- ==================================================== label, description ===
--
-- Bounded because both are printed: a 4 KiB label is a layout defect on a portal page and a
-- description is a paragraph, not a document.

ALTER TABLE services
    ADD COLUMN label       text NOT NULL DEFAULT '',
    ADD COLUMN description text NOT NULL DEFAULT '',
    -- The values of the operator-defined properties, keyed by
    -- service_property_definitions.key. An object, never an array: a property is a key with a
    -- value, and an array of them would make "is it set" a search instead of a lookup.
    ADD COLUMN custom_properties jsonb NOT NULL DEFAULT '{}'::jsonb,
    -- SVC-07: how many of the product this service represents. One is the honest default for
    -- every row that predates the column.
    ADD COLUMN quantity int NOT NULL DEFAULT 1,
    -- SVC-07: the order the service was sold on. Nullable because a service can be
    -- provisioned from a manual invoice that has no order at all.
    ADD COLUMN order_id uuid REFERENCES orders (id) ON DELETE SET NULL;

-- A label is one line. A description may be several, so tabs and newlines are removed before
-- the control-character test rather than forbidden outright.
ALTER TABLE services
    ADD CONSTRAINT services_label_bounded
        CHECK (char_length(label) <= 120),
    ADD CONSTRAINT services_label_single_line
        CHECK (label !~ '[[:cntrl:]]'),
    ADD CONSTRAINT services_description_bounded
        CHECK (char_length(description) <= 2000),
    ADD CONSTRAINT services_description_no_control
        CHECK (translate(description, E'\n\r\t', '') !~ '[[:cntrl:]]'),
    ADD CONSTRAINT services_quantity_positive
        CHECK (quantity > 0),
    -- Values only. A number or a nested object has no label to print and no sanitiser that
    -- knows what to do with it, so it is refused here rather than rendered as `[object]`.
    ADD CONSTRAINT services_custom_properties_object
        CHECK (jsonb_typeof(custom_properties) = 'object'),
    ADD CONSTRAINT services_custom_properties_strings
        CHECK (NOT (custom_properties @? '$.* ? (@.type() != "string")')),
    ADD CONSTRAINT services_custom_properties_no_control
        CHECK (NOT (custom_properties @? '$.* ? (@ like_regex "[[:cntrl:]]")')),
    ADD CONSTRAINT services_custom_properties_value_length
        CHECK (NOT (custom_properties @? '$.* ? (@ like_regex "^.{201,}$")')),
    ADD CONSTRAINT services_custom_properties_size
        CHECK (pg_column_size(custom_properties) <= 8192);

-- Support looks a service up by the order it came from ("the invoice they paid last week").
CREATE INDEX services_order_idx ON services (order_id) WHERE order_id IS NOT NULL;

-- updated_at is maintained by trigger rather than by every caller remembering to set it
-- (0018 established the idiom). A metadata edit and a provisioning transition now stamp the
-- column the same way.
CREATE TRIGGER services_touch_updated_at
    BEFORE UPDATE ON services
    FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- Backfill the order from the invoice that shares the subscription. One row per subscription:
-- the earliest issued invoice is the one that sold it, and a renewal invoice carries the same
-- order anyway. A service whose subscription has no order stays NULL, which is honest.
UPDATE services s
   SET order_id = first_invoice.order_id
  FROM (
        SELECT DISTINCT ON (i.subscription_id) i.subscription_id, i.order_id
          FROM invoices i
         WHERE i.subscription_id IS NOT NULL
           AND i.order_id IS NOT NULL
         ORDER BY i.subscription_id, i.issued_at NULLS LAST, i.created_at
       ) AS first_invoice
 WHERE s.subscription_id = first_invoice.subscription_id
   AND s.order_id IS NULL;

-- ============================================ custom property definitions ===
--
-- The definition is the operator's answer to "what do we track about a service". It is
-- deliberately not per product: "rack" and "control panel URL" describe the installation, not
-- the catalog entry, and a per-product definition would make the same rack a different
-- property on every product.

CREATE TABLE service_property_definitions (
    -- The key is what lives in services.custom_properties, so it is the identity, and the
    -- pattern is what makes it safe to print as a JSON key and read in a form field.
    key              text PRIMARY KEY CHECK (key ~ '^[a-z][a-z0-9_]{0,63}$'),
    -- What a human reads on the service page and on a document. Never the key: `asset_tag`
    -- is not a label.
    label            text NOT NULL CHECK (label <> '' AND char_length(label) <= 120),
    description      text NOT NULL DEFAULT '' CHECK (char_length(description) <= 500),
    -- Decision 3 above: a property reaches a document only when this is true.
    show_on_invoice  boolean NOT NULL DEFAULT false,
    -- Whether the customer sees the property on their own service page. An internal asset tag
    -- is a real property that the customer should not read.
    customer_visible boolean NOT NULL DEFAULT true,
    sort_order       int NOT NULL DEFAULT 0,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX service_property_definitions_order_idx
    ON service_property_definitions (sort_order, key);

CREATE TRIGGER service_property_definitions_touch_updated_at
    BEFORE UPDATE ON service_property_definitions
    FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- A value whose key has no definition is a value nobody can render. Refusing it here means the
-- API's validation and the schema agree, and a hand-written UPDATE cannot create the orphan.
CREATE OR REPLACE FUNCTION services_check_custom_property_keys() RETURNS trigger AS $$
DECLARE
    unknown_key text;
BEGIN
    IF TG_OP = 'UPDATE' AND NEW.custom_properties IS NOT DISTINCT FROM OLD.custom_properties THEN
        RETURN NEW;
    END IF;

    SELECT k INTO unknown_key
      FROM jsonb_object_keys(NEW.custom_properties) AS k
     WHERE NOT EXISTS (SELECT 1 FROM service_property_definitions d WHERE d.key = k)
     LIMIT 1;

    IF unknown_key IS NOT NULL THEN
        RAISE EXCEPTION 'service property "%" has no definition', unknown_key
            USING ERRCODE = 'foreign_key_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER services_custom_property_keys
    BEFORE INSERT OR UPDATE ON services
    FOR EACH ROW EXECUTE FUNCTION services_check_custom_property_keys();

-- The other direction: deleting a definition that a service still carries would leave the value
-- unrenderable, which is the same orphan. An operator clears the values first, or renames the
-- definition instead.
CREATE OR REPLACE FUNCTION service_property_definitions_guard_delete() RETURNS trigger AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM services WHERE custom_properties ? OLD.key) THEN
        RAISE EXCEPTION 'service property "%" is set on at least one service', OLD.key
            USING ERRCODE = 'foreign_key_violation';
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER service_property_definitions_guard_delete
    BEFORE DELETE ON service_property_definitions
    FOR EACH ROW EXECUTE FUNCTION service_property_definitions_guard_delete();

COMMIT;
