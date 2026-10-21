-- 0002_runtime_grants.sql
--
-- Grants for the conventional runtime role.
--
-- The runtime role (billing_app) must be able to read and write rows but must
-- not be able to change the schema. That separation is what turns "a bug could
-- drop a table" into "a bug gets a permission error".
--
-- Role creation belongs to provisioning, not to a migration, so this file is a
-- no-op when the role does not exist. Keeping the grant here rather than only in
-- the cluster bootstrap means a schema recreated from migrations alone (as the
-- integration tests do) still ends up with the right privileges.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'billing_app') THEN
        RAISE NOTICE 'billing_app role absent; skipping runtime grants';
        RETURN;
    END IF;

    GRANT USAGE ON SCHEMA public TO billing_app;
    GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO billing_app;
    GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO billing_app;

    -- Future tables created by the owner role.
    EXECUTE format(
        'ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
        'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO billing_app',
        current_user);
    EXECUTE format(
        'ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public '
        'GRANT USAGE, SELECT ON SEQUENCES TO billing_app',
        current_user);
END $$;
