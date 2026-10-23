-- 0067_operator_parity.sql
--
-- Two things the operator surface could not express, and one index that keeps the first cheap
-- (ADM-12, OPS-14).
--
-- # `installed_extensions.enabled`
--
-- An extension's row meant "verified and stored". Whether the deployment was *using* it was
-- recorded nowhere but an environment variable, so the marketplace screen could not show an
-- operator which of their installed gateways was live, and could not offer a way to stop using one
-- without deleting the artifact they might need for a rollback. The column is that missing bit.
--
-- It defaults to true, which is the state every existing row is in: an artifact that was installed
-- before this migration was installed because somebody intended to use it, and a migration that
-- silently switched them all off would take payments down.
--
-- `enabled` is deliberately not what decides whether a process launches an extension — the
-- deployment resolves the path from its environment or the settings table, and the panel cannot
-- reach into a running worker. It is the operator's declaration of intent, read by the resolution
-- path, and the screen states the remaining step rather than implying the switch is enough.
ALTER TABLE installed_extensions
    ADD COLUMN enabled boolean NOT NULL DEFAULT true;

COMMENT ON COLUMN installed_extensions.enabled IS
    'Operator intent: should this artifact be used? False keeps the verified bytes for rollback while telling every reader the extension is not in service.';

-- The marketplace screen and the resolution path both ask "the newest installed row for this
-- name, and is it enabled". Without this index that is a sort of the whole table on a page an
-- operator reloads while debugging a gateway.
CREATE INDEX installed_extensions_enabled_idx
    ON installed_extensions (name, installed_at DESC) INCLUDE (version, enabled);

-- # The importer's entity vocabulary (OPS-10)
--
-- `import_records.entity` was a closed set of the five entities migration 0039 knew about. The
-- importer now also reads currencies, admins, products and tickets from a dump, and a mapping row
-- is what makes a second run of the same dump idempotent — without these values the run fails at
-- the first new entity with a constraint violation, after it has already written rows.
--
-- The list is the union of the old set and the new one; dropping and re-adding the constraint is
-- how a CHECK is extended in PostgreSQL. Every existing row satisfies the new predicate, so the
-- re-validation is a formality.
ALTER TABLE import_records DROP CONSTRAINT import_records_entity_check;
ALTER TABLE import_records ADD CONSTRAINT import_records_entity_check CHECK (entity IN
    ('customer', 'invoice', 'invoice_line', 'service', 'payment',
     'currency', 'admin', 'product', 'ticket'));
