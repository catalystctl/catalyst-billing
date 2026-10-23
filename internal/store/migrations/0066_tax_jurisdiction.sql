-- Tax jurisdiction rules: the facts a rate table cannot state.
--
-- `tax_rates` answers *how much*. It cannot answer *whether the rate applies at all*, and that
-- question is the one a tax authority asks. Two examples the rate table gets wrong by omission:
--
--   * A German business buying a digital service from another EU country supplies a VAT ID and
--     expects an invoice with no VAT and a reverse-charge note. The rate table's answer —
--     "19% for DE" — charges tax the customer is legally not liable for, and the customer cannot
--     reclaim it because the invoice is not a reverse-charge invoice.
--   * A California customer of a seller with no California presence is not owed California sales
--     tax. The rate table's answer — "7.25% for US-CA" once an operator enters it — collects a
--     tax the seller may have no standing to collect.
--
-- Neither is a rate, so neither is expressible as a rate. They are *rules* about a fact set:
-- where the seller is established, where the customer is, whether the customer's VAT ID was
-- validated against a service, and whether the seller has nexus in a US state. This migration
-- stores the two facts the installation must declare itself — nexus, and the VAT-ID checks that
-- have actually been run — and nothing else:
--
--   * The EU membership list, the reverse-charge rule and the shape of a VAT ID are facts of law,
--     not of this installation. They live in code (internal/tax/jurisdiction.go) where a reviewer
--     can read them against the directive, rather than in rows somebody edits at 2am.
--   * The seller's own country, whether reverse charge is enabled, and which validation service
--     to call are settings (settings group `tax`), so they are audited by the existing settings
--     path and do not need a migration per key.
--
-- # Why a validation is a row and not a boolean on the customer
--
-- Reverse charge is a claim about a *check that happened*: which identifier, against which
-- service, when, and what the service answered. A boolean `tax_exempt` on the billing profile
-- (which already exists) is a staff assertion and says none of that. Storing each check
-- append-only is what lets an invoice from last year be explained by the check that justified it,
-- and what stops a cache refresh from silently rewriting that justification.

-- -------------------------------------------------------------- US nexus --

CREATE TABLE tax_nexus (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- ISO 3166-1 alpha-2. Upper-case, like every other country code in the schema, so a lookup
    -- is an equality rather than a case-insensitive scan.
    country_code text NOT NULL CHECK (country_code ~ '^[A-Z]{2}$'),
    -- The subdivision nexus was established in (a US state, a Canadian province). Empty means
    -- "the whole country", which is what a national registration is.
    region_code  text NOT NULL DEFAULT ''
                 CHECK (region_code = '' OR region_code ~ '^[A-Z0-9]{1,8}$'),
    -- Why this jurisdiction is declarable: "warehouse in Reno", "economic nexus since 2023".
    -- It is not decoration — a nexus declaration is an assertion an auditor will ask for the
    -- basis of, and the person who made it is recorded next to it.
    note         text NOT NULL DEFAULT '',
    enabled      boolean NOT NULL DEFAULT true,
    created_by   uuid REFERENCES users (id) ON DELETE SET NULL,
    updated_by   uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- One declaration per jurisdiction. Two rows for the same state would make "is there nexus"
-- depend on row order, which is the same defect the rate table's `tax_rates_one_open` prevents.
CREATE UNIQUE INDEX tax_nexus_jurisdiction ON tax_nexus (country_code, region_code);
CREATE INDEX tax_nexus_lookup ON tax_nexus (country_code, region_code) WHERE enabled;

-- ---------------------------------------------------- VAT-ID validations --

CREATE TABLE vat_id_validations (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The identifier as normalised for the check: upper-case, no spaces, no country punctuation.
    -- The country is a column of its own because the same digit string can be a valid number in
    -- one member state and nonsense in another.
    country_code text NOT NULL CHECK (country_code ~ '^[A-Z]{2}$'),
    vat_id       text NOT NULL CHECK (vat_id ~ '^[A-Z0-9]{2,20}$'),
    -- The three answers that are all different and all actionable:
    --   valid       the service confirmed the identifier; reverse charge may be granted
    --   invalid     the service refused it; the customer is a consumer for this sale
    --   unavailable nothing was asked or no answer came back; reverse charge may NOT be granted
    -- Collapsing `invalid` and `unavailable` would turn "we could not check" into "the customer
    -- lied", and collapsing them the other way turns a negative answer into a zero rate.
    status       text NOT NULL CHECK (status IN ('valid', 'invalid', 'unavailable')),
    -- The registered trader name the service returned, when it returns one. It is evidence that
    -- the identifier belongs to somebody, and it is what makes a mismatch with the billing name
    -- visible to a human later.
    trader_name  text NOT NULL DEFAULT '',
    -- The endpoint the answer came from, verbatim, so the check can be repeated against the same
    -- authority. Empty for a shape refusal (no service was asked).
    service_url  text NOT NULL DEFAULT '',
    detail       text NOT NULL DEFAULT '',
    customer_id  uuid REFERENCES customers (id) ON DELETE SET NULL,
    checked_at   timestamptz NOT NULL DEFAULT now()
);

-- The cache read: the newest check for an identifier. A validation is reused for the configured
-- TTL so a cart re-price does not call an external service on every request.
CREATE INDEX vat_id_validations_lookup ON vat_id_validations (country_code, vat_id, checked_at DESC);
CREATE INDEX vat_id_validations_customer ON vat_id_validations (customer_id, checked_at DESC)
    WHERE customer_id IS NOT NULL;

-- A validation is a historical fact. An UPDATE would rewrite what an issued invoice's
-- reverse-charge treatment was based on, so it is refused here rather than by convention.
CREATE FUNCTION vat_id_validations_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'vat_id_validations is append-only (attempted % on %)', TG_OP, OLD.id
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER vat_id_validations_no_update
    BEFORE UPDATE OR DELETE ON vat_id_validations
    FOR EACH ROW EXECUTE FUNCTION vat_id_validations_append_only();
