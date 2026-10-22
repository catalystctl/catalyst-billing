-- 0045_user_admin.sql
--
-- Staff user administration: the database-side guard that makes "there is always an
-- administrator" a property of the schema rather than a promise about a handler.
--
-- The screen (`/users`) can now demote, disable and delete the account an operator is
-- signed in as. That is the most damaging thing this panel can be asked to do, and it is
-- the one refusal that has to survive two operators clicking at the same moment, so it
-- lives here rather than in the handler.
--
-- Four decisions are recorded, because each is easy to "simplify" back into a defect:
--
-- 1. **The last active administrator is a database invariant, and the count is taken
--    under a lock.** The handler refuses first, with a code an operator can act on. But
--    under READ COMMITTED two concurrent demotions of two different administrators each
--    see the other's row still present, each conclude "one remains", and both commit —
--    leaving none. A BEFORE trigger alone does not fix that either: it counts the same
--    stale snapshot. So the trigger takes a transaction-scoped advisory lock before
--    counting. The second transaction waits for the first to commit, and its next
--    statement sees the committed result — which requires the counting function to be
--    VOLATILE, because a STABLE one would keep the snapshot it started with and count the
--    stale row anyway. The lock is a constant here and is deliberately *not* the migration
--    lock (0045 does not run at request time; this does).
--
-- 2. **A disabled administrator does not count.** "Cannot sign in" lives in
--    `users.status`, so disabling the last administrator locks the panel out exactly as
--    demoting them does. Both paths therefore call one counting function, and there is a
--    trigger on `users.status` as well as on `staff_roles`, so the two cannot disagree
--    about who counts.
--
-- 3. **The guard reads the role the row will actually hold.** 0026 keeps
--    `staff_roles.role` and `staff_roles.role_id` in step with a BEFORE trigger, and the
--    firing order of two BEFORE triggers on one table is alphabetical rather than
--    declared. A writer that sets only `role_id` would look like "no change" to a guard
--    that read `NEW.role`. This trigger resolves the effective role from `role_id`
--    whenever that column is what changed, so neither write path can slip past it.
--
-- 4. **Deleting an account is not made easy.** Nothing here adds ON DELETE CASCADE to a
--    reference to `users`. The foreign keys that matter — `audit_events.actor_id`,
--    `customer_accounts.user_id` — keep their default NO ACTION, so an account with a
--    history cannot be deleted at all: the database has the last word, and the API turns
--    that into a named refusal that points at disabling instead. `sessions` cascades,
--    because a session is not a record of anything: it is a live credential, and deleting
--    the account must end it.
--
-- 5. **`sync_staff_role_id` is replaced, because the 0026 version silently ignored a role
--    change.** 0026 keeps `staff_roles.role` and `role_id` in step, but its trigger only
--    mirrors `role_id` → `role`: a writer that set `role` on an existing row had its value
--    overwritten with the name of the old `role_id`, so `UPDATE staff_roles SET role =
--    'support'` updated one row and changed nothing. That is a demotion that appears to
--    work and does not, which is precisely the failure this migration's guard exists to
--    prevent, so the function is replaced here: whichever of the two columns a statement
--    changed decides, and a statement that changed both is resolved in favour of `role_id`
--    (the foreign key).

BEGIN;

-- The question both guards ask, in one place so they cannot drift: how many *other*
-- active accounts hold the administrator role.
--
-- **VOLATILE, not STABLE, and that is the whole point of the race guard.** A STABLE or
-- IMMUTABLE function runs with the snapshot of the calling statement, and the calling
-- statement here is the UPDATE that is blocked on the advisory lock below. Blocking does
-- not refresh that snapshot, so the transaction that waited would still see the other
-- administrator as present and conclude "one remains" — which is exactly the race this
-- trigger exists to close. A VOLATILE function takes a fresh snapshot for each statement it
-- runs, so the wait, then the count, sees the committed result. It was measured: with STABLE
-- two concurrent demotions both succeeded and left the panel with no administrator.
--
-- `excluding` is a user id, never NULL from either caller. The NULL branch is there so a
-- future caller reading a nullable column gets a count instead of a silently empty result:
-- `sr.user_id <> NULL` is NULL, which would read as "no administrator remains".
CREATE OR REPLACE FUNCTION active_administrators(excluding uuid) RETURNS integer
LANGUAGE sql VOLATILE AS $$
    SELECT count(*)
      FROM staff_roles sr
      JOIN users u ON u.id = sr.user_id
     WHERE sr.role = 'administrator'
       AND u.status = 'active'
       AND (excluding IS NULL OR sr.user_id <> excluding)
$$;

COMMENT ON FUNCTION active_administrators(uuid) IS
    'How many active accounts other than the given one hold the administrator role.';

-- ------------------------------------------------- role/role_id, both ways --
--
-- See decision 5 in the header. The 0026 version mirrors role_id → role only, so setting
-- `role` on an existing row was silently overwritten and the demotion did nothing. This one
-- lets either column drive the other, and prefers role_id when a statement changed both,
-- because role_id is the foreign key.
CREATE OR REPLACE FUNCTION sync_staff_role_id() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.role_id IS NOT NULL THEN
            SELECT name INTO NEW.role FROM roles WHERE id = NEW.role_id;
        ELSE
            SELECT id INTO NEW.role_id FROM roles WHERE name = NEW.role;
        END IF;
        IF NEW.role IS NULL THEN
            RAISE EXCEPTION 'staff role % does not exist', COALESCE(NEW.role, NEW.role_id::text)
                USING ERRCODE = 'foreign_key_violation';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.role_id IS DISTINCT FROM OLD.role_id THEN
        SELECT name INTO NEW.role FROM roles WHERE id = NEW.role_id;
        IF NEW.role IS NULL THEN
            RAISE EXCEPTION 'staff role % does not exist', NEW.role_id
                USING ERRCODE = 'foreign_key_violation';
        END IF;
    ELSIF NEW.role IS DISTINCT FROM OLD.role THEN
        SELECT id INTO NEW.role_id FROM roles WHERE name = NEW.role;
        IF NEW.role_id IS NULL THEN
            RAISE EXCEPTION 'staff role % does not exist', NEW.role
                USING ERRCODE = 'foreign_key_violation';
        END IF;
    END IF;
    RETURN NEW;
END $$;

-- --------------------------------------------------- staff_roles: demotion --

CREATE OR REPLACE FUNCTION keep_an_administrator_staff_role() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    effective_role text;
BEGIN
    IF TG_OP = 'DELETE' THEN
        -- Only losing the administrator role matters; every other role can go freely.
        IF OLD.role <> 'administrator' THEN
            RETURN OLD;
        END IF;
    ELSE
        effective_role := NEW.role;
        IF NEW.role_id IS DISTINCT FROM OLD.role_id THEN
            SELECT name INTO effective_role FROM roles WHERE id = NEW.role_id;
        END IF;
        -- Not a demotion, or still an administrator: nothing to guard.
        IF OLD.role <> 'administrator' OR effective_role = 'administrator' THEN
            RETURN NEW;
        END IF;
    END IF;

    PERFORM pg_advisory_xact_lock(8421196317004213);
    IF active_administrators(OLD.user_id) = 0 THEN
        RAISE EXCEPTION 'the last active administrator cannot be demoted or removed'
            USING ERRCODE = 'restrict_violation',
                  HINT = 'grant administrator to another active account first';
    END IF;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END $$;

-- Named to sort after `staff_roles_sync_role_id` (0026) so NEW.role is already the
-- mirrored value. The function does not rely on that order — it resolves the role itself
-- when role_id is what changed — but a guard that runs last is easier to reason about.
CREATE TRIGGER staff_roles_zz_keep_administrator
    BEFORE UPDATE OR DELETE ON staff_roles
    FOR EACH ROW EXECUTE FUNCTION keep_an_administrator_staff_role();

-- ------------------------------------------------------- users: disabling --

CREATE OR REPLACE FUNCTION keep_an_administrator_user() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    -- Anything other than "was active and is not any more" is not this guard's business.
    IF NEW.status = 'active' OR OLD.status <> 'active' THEN
        RETURN NEW;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM staff_roles WHERE user_id = NEW.id AND role = 'administrator') THEN
        RETURN NEW;
    END IF;

    PERFORM pg_advisory_xact_lock(8421196317004213);
    IF active_administrators(NEW.id) = 0 THEN
        RAISE EXCEPTION 'the last active administrator cannot be disabled'
            USING ERRCODE = 'restrict_violation',
            HINT = 'grant administrator to another active account first';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER users_zz_keep_administrator
    BEFORE UPDATE OF status ON users
    FOR EACH ROW EXECUTE FUNCTION keep_an_administrator_user();

-- --------------------------------------------------------------- indexes ---
--
-- The screen's two queries. The list is keyset-paginated newest-first, so the cursor's
-- sort key is (created_at, id) and the index has to match it or the first page of a large
-- installation sorts the whole table.
CREATE INDEX users_created_idx ON users (created_at DESC, id DESC);

-- "Who holds this role" is the access-review question the roles screen asks, and the
-- administrator count is asked on every demotion.
CREATE INDEX staff_roles_role_idx ON staff_roles (role);
CREATE INDEX staff_roles_administrator_idx ON staff_roles (user_id)
    WHERE role = 'administrator';

COMMIT;
