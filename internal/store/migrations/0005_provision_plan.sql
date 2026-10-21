-- 0005_provision_plan.sql
--
-- The adapter-side plan identifier is catalog configuration, not identity.
--
-- Provisioning needs to know which plan to buy from the infrastructure adapter.
-- That is a property of the price being sold, captured when the service is
-- created, so it lives with the price version rather than being derived from the
-- operation key or from the product name at call time.

ALTER TABLE price_versions
    ADD COLUMN provision_plan text NOT NULL DEFAULT '',
    -- The region the adapter should provision into. A provisioner that offers
    -- regions requires one, so it is catalog configuration like the plan.
    ADD COLUMN provision_region text NOT NULL DEFAULT '';
