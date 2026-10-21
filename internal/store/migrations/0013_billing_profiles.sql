-- 0013_billing_profiles.sql
--
-- Per-customer billing details.
--
-- These are *input to the next invoice*, never a rewrite of a past one: the issuer
-- copies them into `invoices.snapshot` at issuance, and the issued-invoice trigger
-- freezes the snapshot. A customer correcting their address therefore cannot change
-- what a tax authority already received.
--
-- `tax_id` is stored as given and never validated against a jurisdiction. Validating
-- it would imply a compliance claim this product does not make; the field exists so a
-- customer can put the identifier they use on their own paperwork.

CREATE TABLE billing_profiles (
    customer_id   uuid PRIMARY KEY REFERENCES customers (id) ON DELETE CASCADE,
    legal_name    text NOT NULL DEFAULT '',
    address_line1 text NOT NULL DEFAULT '',
    address_line2 text NOT NULL DEFAULT '',
    city          text NOT NULL DEFAULT '',
    postal_code   text NOT NULL DEFAULT '',
    region        text NOT NULL DEFAULT '',
    -- ISO 3166-1 alpha-2, or empty for "not supplied". Uppercase only: a lower-case
    -- code is a client bug, and normalising silently would hide it.
    country_code  text NOT NULL DEFAULT ''
                  CHECK (country_code = '' OR country_code ~ '^[A-Z]{2}$'),
    tax_id        text NOT NULL DEFAULT '',
    updated_at    timestamptz NOT NULL DEFAULT now()
);
