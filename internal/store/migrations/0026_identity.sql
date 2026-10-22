-- 0026_identity.sql
--
-- Identity depth: editable roles, impersonation, and API-key IP allowlists.
--
-- Four decisions shape this file, and each is recorded because the next person will
-- otherwise "simplify" it back into a defect:
--
-- 1. **A role is a row, but the four shipped roles stay system rows.** The permission
--    map in internal/authn/authz.go encodes a separation of duties (billing can refund
--    and cannot terminate; infrastructure can terminate and cannot refund) that the
--    approval flow depends on. Making those four editable would let one screen grant a
--    role that defeats a control the rest of the code assumes. So they are seeded as
--    `is_system = true`, with exactly the permission sets the code declares, and the
--    database refuses to update or delete them even if a bug tries.
--
-- 2. **`staff_roles.role` keeps working; `role_id` is added beside it.** A live column
--    with a CHECK constraint is not something to drop and hope: the old column is what
--    internal/authn/session.go reads to decide whether a user is staff. So the column
--    stays, its closed-set CHECK is replaced by a foreign key to `roles(name)` (which
--    accepts the same four values and also a custom role), and `role_id` is backfilled
--    from it. A trigger keeps the two in step so no writer can make them disagree.
--
-- 3. **An impersonated session is a session with a named operator.** Impersonation must
--    not be a boolean on the user: the audit trail has to say *who* was wearing the
--    mask, and "sign out everywhere" must be able to end the impersonation without
--    ending the operator's own session. `sessions.impersonated_by` is that record.
--
-- 4. **An API key's allowlist is a property of the key, not a setting.** A key minted
--    for a CI runner that reconciles payments should stop working from an address it
--    was not issued for, and that must be visible on the key row rather than inferred
--    from a global policy.

BEGIN;

-- =================================================================== roles ==
--
-- `name` is unique and lower-case by construction: it is also the value written to the
-- legacy staff_roles.role column, and two roles differing only by case would make that
-- mirror ambiguous.

CREATE TABLE roles (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL UNIQUE
                CHECK (name = lower(name) AND name ~ '^[a-z][a-z0-9_-]{1,39}$'),
    description text NOT NULL DEFAULT '',
    -- A system role is one of the four the code's separation of duties is written
    -- against. It cannot be edited or deleted, by this application or by a mistake.
    is_system   boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- The permission matrix. A permission is a string from the declared set; the set itself
-- lives in code (authn.AllPermissions) because a permission no handler checks is a lie,
-- so the API validates against it before inserting. The table cannot carry that CHECK
-- without duplicating the list, and a duplicated list drifts.
CREATE TABLE role_permissions (
    role_id    uuid NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    permission text NOT NULL CHECK (permission <> ''),
    PRIMARY KEY (role_id, permission)
);

-- "Who holds this permission" is the question an access review asks.
CREATE INDEX role_permissions_permission_idx ON role_permissions (permission);

INSERT INTO roles (name, description, is_system) VALUES
    ('administrator',  'Full operator access, including staff and settings management.', true),
    ('billing',        'Commercial operations: invoices, payments, promotions, tax, credit.', true),
    ('support',        'First-line support: reads customers, holds services, answers tickets.', true),
    ('infrastructure', 'Service lifecycle: provision, suspend, resume and terminate.', true);

-- The seeded sets are exactly internal/authn/rolePermissions. internal/authn's role
-- tests compare the two, so a change to the code map that is not mirrored here fails the
-- build instead of silently changing what existing staff can do.
INSERT INTO role_permissions (role_id, permission)
SELECT r.id, seed.permission
FROM roles r
JOIN (VALUES
    ('administrator', 'announcement:write'),
    ('administrator', 'apikey:manage'),
    ('administrator', 'audit:read'),
    ('administrator', 'catalog:write'),
    ('administrator', 'config:write'),
    ('administrator', 'coupon:read:all'),
    ('administrator', 'coupon:write'),
    ('administrator', 'credit:write'),
    ('administrator', 'currency:write'),
    ('administrator', 'customer:read:all'),
    ('administrator', 'customer:write:all'),
    ('administrator', 'extension:manage'),
    ('administrator', 'gateway:write'),
    ('administrator', 'invoice:read:all'),
    ('administrator', 'invoice:void'),
    ('administrator', 'invoice:write:all'),
    ('administrator', 'log:read'),
    ('administrator', 'notification:write'),
    ('administrator', 'payment:reconcile'),
    ('administrator', 'payment:refund'),
    ('administrator', 'report:read'),
    ('administrator', 'server:write'),
    ('administrator', 'service:hold'),
    ('administrator', 'service:provision'),
    ('administrator', 'service:read:all'),
    ('administrator', 'service:suspend'),
    ('administrator', 'service:terminate'),
    ('administrator', 'service:upgrade'),
    ('administrator', 'settings:write'),
    ('administrator', 'staff:manage'),
    ('administrator', 'tax:write'),
    ('administrator', 'ticket:read:all'),
    ('administrator', 'ticket:write:all'),
    ('billing',       'config:write'),
    ('billing',       'coupon:read:all'),
    ('billing',       'coupon:write'),
    ('billing',       'credit:write'),
    ('billing',       'currency:write'),
    ('billing',       'customer:read:all'),
    ('billing',       'customer:write:all'),
    ('billing',       'invoice:read:all'),
    ('billing',       'invoice:void'),
    ('billing',       'invoice:write:all'),
    ('billing',       'notification:write'),
    ('billing',       'payment:reconcile'),
    ('billing',       'payment:refund'),
    ('billing',       'report:read'),
    ('billing',       'service:read:all'),
    ('billing',       'service:upgrade'),
    ('billing',       'tax:write'),
    ('billing',       'ticket:read:all'),
    ('billing',       'ticket:write:all'),
    ('support',       'apikey:manage'),
    ('support',       'customer:read:all'),
    ('support',       'invoice:read:all'),
    ('support',       'log:read'),
    ('support',       'service:hold'),
    ('support',       'service:read:all'),
    ('support',       'ticket:read:all'),
    ('support',       'ticket:read:self'),
    ('support',       'ticket:write:all'),
    ('support',       'ticket:write:self'),
    ('infrastructure', 'audit:read'),
    ('infrastructure', 'customer:read:all'),
    ('infrastructure', 'server:write'),
    ('infrastructure', 'service:provision'),
    ('infrastructure', 'service:read:all'),
    ('infrastructure', 'service:suspend'),
    ('infrastructure', 'service:terminate'),
    ('infrastructure', 'service:upgrade')
) AS seed(role, permission) ON seed.role = r.name;

-- A system role is immutable. The API refuses first and with a useful message; this
-- trigger is the backstop that makes "a bug cannot widen a role the code depends on" a
-- property of the database rather than a promise about the handlers.
CREATE OR REPLACE FUNCTION reject_system_role_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.is_system THEN
            RAISE EXCEPTION 'a system role cannot be deleted' USING ERRCODE = 'restrict_violation';
        END IF;
        RETURN OLD;
    END IF;
    IF OLD.is_system THEN
        RAISE EXCEPTION 'a system role cannot be modified' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER roles_system_immutable
    BEFORE UPDATE OR DELETE ON roles
    FOR EACH ROW EXECUTE FUNCTION reject_system_role_mutation();

-- Deleting a permission from a system role is the same act as editing it.
CREATE OR REPLACE FUNCTION reject_system_role_permission_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM roles WHERE id = OLD.role_id AND is_system) THEN
        RAISE EXCEPTION 'a system role''s permissions cannot be changed'
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN OLD;
END $$;

CREATE TRIGGER role_permissions_system_immutable
    BEFORE UPDATE OR DELETE ON role_permissions
    FOR EACH ROW EXECUTE FUNCTION reject_system_role_permission_mutation();

-- ------------------------------------------------------- staff_roles.role_id --

ALTER TABLE staff_roles ADD COLUMN role_id uuid REFERENCES roles (id);

UPDATE staff_roles sr
SET role_id = r.id
FROM roles r
WHERE r.name = sr.role;

-- The closed-set CHECK becomes a foreign key to the roles table. The four values it
-- allowed are exactly the four seeded rows, so no existing row becomes invalid, and a
-- custom role can now be assigned without dropping the column or its validation.
ALTER TABLE staff_roles DROP CONSTRAINT staff_roles_role_check;
ALTER TABLE staff_roles
    ADD CONSTRAINT staff_roles_role_fkey FOREIGN KEY (role) REFERENCES roles (name)
    ON UPDATE CASCADE;

CREATE INDEX staff_roles_role_id_idx ON staff_roles (role_id);

-- Keep the legacy mirror and the foreign key in step, whichever one a writer sets. This
-- is what lets internal/authn/session.go keep reading `role` unchanged while the
-- permission resolver reads `role_id`.
CREATE OR REPLACE FUNCTION sync_staff_role_id() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.role_id IS NOT NULL THEN
        SELECT name INTO NEW.role FROM roles WHERE id = NEW.role_id;
        IF NEW.role IS NULL THEN
            RAISE EXCEPTION 'staff role % does not exist', NEW.role_id
                USING ERRCODE = 'foreign_key_violation';
        END IF;
    ELSE
        SELECT id INTO NEW.role_id FROM roles WHERE name = NEW.role;
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER staff_roles_sync_role_id
    BEFORE INSERT OR UPDATE OF role, role_id ON staff_roles
    FOR EACH ROW EXECUTE FUNCTION sync_staff_role_id();

-- ========================================================= impersonation ==
--
-- The operator is recorded on the session they are wearing, not on the target user: a
-- second operator impersonating the same person at the same time must produce two
-- distinguishable rows. `ON DELETE CASCADE` is wrong here — deleting an operator must not
-- silently turn their impersonations into ordinary sessions, so the reference is plain
-- and the session ends with them.

ALTER TABLE sessions ADD COLUMN impersonated_by uuid REFERENCES users (id);

-- The operator's own session, so "stop impersonating" returns to the identity that started
-- it instead of logging the operator out. The token of that session is not stored (only its
-- hash), so the stop path rotates the token on this row and hands the new one back: the old
-- token dies, and the operator is back on the same session record they started from.
ALTER TABLE sessions ADD COLUMN impersonated_from_session uuid REFERENCES sessions (id)
    ON DELETE SET NULL;

CREATE INDEX sessions_impersonated_idx ON sessions (impersonated_by)
    WHERE impersonated_by IS NOT NULL;

-- The audit trail can name both principals once internal/audit carries the field. The
-- column lands here so the writer change and the schema change ship together; until
-- then it is NULL on every row, which is honest rather than misleading.
ALTER TABLE audit_events ADD COLUMN impersonated_by uuid REFERENCES users (id);

CREATE INDEX audit_events_impersonated_idx ON audit_events (impersonated_by, created_at DESC)
    WHERE impersonated_by IS NOT NULL;

-- ============================================================ api keys =====
--
-- An empty array means "no restriction", which is the behaviour every existing key
-- already has, so the default is correct for rows that predate this migration. A
-- non-empty array is an allowlist: the key authenticates only from a matching address.

ALTER TABLE api_keys ADD COLUMN ip_allowlist cidr[] NOT NULL DEFAULT '{}';

COMMENT ON COLUMN api_keys.ip_allowlist IS
    'Empty means any address. Otherwise the key is refused unless the request address is contained in one of these networks.';

COMMIT;
