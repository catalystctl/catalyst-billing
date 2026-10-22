-- 0049_catalog_structure.sql
--
-- Two structural gaps in the catalogue: categories cannot nest (SF-02), and a product cannot
-- say which server it lives on (SF-12 / CAT-08).
--
-- 0018 built a flat category list and said why: "a tree costs a recursive query on every page
-- for no benefit until somebody actually asks for sub-categories". Somebody has. A shop cannot
-- express "Game servers > Minecraft", so the storefront filter is one long list and an operator
-- works around it by naming categories "Game servers — Minecraft". This migration adds the
-- parent, and — because a self-referencing tree is the one shape a database will happily make
-- unreadable — the two invariants that make it safe.
--
-- Five decisions shape this file, and each is recorded because the next person will otherwise
-- "simplify" one of them back into a defect.
--
-- 1. **A cycle is refused by the database, not by the service.** Two concurrent writes can each
--    look safe on its own: transaction A sets A.parent = B while transaction B sets B.parent = A,
--    and neither can see the other's uncommitted row, so both ancestor walks come back clean and
--    the pair commits a cycle. A code-only check therefore cannot enforce this, however careful
--    it is. So the check is a trigger (`categories_guard_tree`), and the trigger first takes
--    `pg_advisory_xact_lock` on the category tree. That lock is held until the writing
--    transaction commits, which is exactly the window the check needs: the second writer walks a
--    tree that includes the first writer's committed row, finds itself, and is refused. Category
--    writes are an operator action — a handful per installation, ever — so serialising them
--    costs nothing anyone can observe, and the alternative is a tree that a recursive read can
--    only escape by a depth cap. The service layer checks the same thing first (in
--    internal/catalog/nesting.go) so an operator gets a sentence instead of a SQLSTATE; the
--    trigger is what makes it true.
--
-- 2. **Nesting is bounded at four levels.** A pathological tree makes every render, breadcrumb
--    and recursive read unbounded, and a storefront that a data-entry mistake can make slow is a
--    storefront that falls over on a busy Saturday. Four is deeper than any real catalogue needs
--    ("Game servers > Minecraft > Modded" is three) and shallow enough to render as a menu. The
--    bound is checked on the *subtree*, not on the row: moving a three-deep branch under a
--    two-deep parent is refused even though the moved node itself would only be at level three,
--    because its leaf would land at level six.
--
-- 3. **Deleting a category that still has children is refused, not reparented.** The two are not
--    equivalent. Reparenting silently moves a child to the grandparent, which changes a URL a
--    customer may have bookmarked and the grouping an operator sees, and neither is what "delete
--    this category" asked for. `ON DELETE RESTRICT` plus an explicit count in
--    `catalog.DeleteCategoryTx` refuses with a sentence and a stable code, and the operator
--    either moves the children out first or deletes them one at a time — deliberately, seeing
--    each step. That is the same rule 0018 applied to products (`ON DELETE SET NULL` there,
--    because a product survives its category), and the stricter one here is right for a
--    structural edit: an orphaned product still sells, an orphaned subcategory is a URL nobody
--    can find.
--
-- 4. **`products.server_id` does not invent a second routing mechanism.** This installation
--    configures exactly one provisioning provider, in the `servers.*` settings group (provider,
--    panel URL, API key) — those settings say *which provider*, and there is no table behind
--    them. What an operator could not express is *which node on that provider* a product is sold
--    onto, which is what Paymenter's `servers` resource is. So `servers` here is that list of
--    targets: a name, the provider it belongs to, and a region/node label. The provisioning path
--    already carries exactly one target — `price_versions.provision_plan` /
--    `provision_region`, copied into `services.config` at checkout and read by the worker as
--    `CreateRequest.Region` — so a product's server feeds *that same field* and nothing else:
--    the plan's region wins when it names one (an existing price must keep provisioning exactly
--    where it did), and the product's server supplies the target when the plan does not. There
--    is no second lookup, no second field and no per-adapter special case.
--
-- 5. **Both new columns are nullable, and no existing row changes meaning.** Every category that
--    exists is a root; every product that exists has no server. A `NOT NULL` column would have to
--    invent a value for rows whose owner never chose one, and an invented parent or node is worse
--    than an absent one: absent means "exactly as before", invented means a provisioning call to
--    a node nobody selected. Nothing here rewrites an issued invoice, a price version, an order
--    or an audit row — `products.server_id` is catalogue metadata like `category_id`: it decides
--    where the *next* purchase is provisioned and says nothing about services that already exist.
--
-- `products.server_extension` (0020) is deliberately left alone. It names a *provisioning
-- extension*, which is a different axis from *which node that extension talks to*, and nothing in
-- the provisioning path has ever read it. Folding the two would mean a product could not use the
-- installation's default extension on one specific node.

BEGIN;

-- =================================================================== servers ==

-- A provisioner target on the configured provider. `region` is what the adapter receives as
-- `CreateRequest.Region`; `node` is the provider's own node/group label, passed beside it in the
-- metadata map, because most panels (Pterodactyl included) address a target by node id rather
-- than by a region name.
CREATE TABLE servers (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        text NOT NULL,
    name        text NOT NULL CHECK (length(btrim(name)) > 0),
    -- The provisioning extension this target belongs to, as an extension name. Empty means
    -- "whichever extension the installation is configured with", which is what a single-provider
    -- shop wants and what every row this migration leaves behind says.
    provider    text NOT NULL DEFAULT '',
    -- Passed to the adapter as the create request's region. Empty means the adapter's own
    -- default, which is what every installation has been getting until now.
    region      text NOT NULL DEFAULT '',
    -- The provider's node/group label, passed as metadata. Empty for providers that address a
    -- target by region alone.
    node        text NOT NULL DEFAULT '',
    -- A disabled server keeps the products already sold on it and their services, and is not
    -- offered for a new assignment. Deleting a row is refused while a product points at it, for
    -- the same reason a category with children is: a delete is not a hide.
    enabled     boolean NOT NULL DEFAULT true,
    sort_order  int     NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX servers_slug_unique ON servers (lower(slug));
CREATE INDEX servers_offer_idx ON servers (enabled, sort_order, name);

CREATE TRIGGER servers_touch_updated_at
    BEFORE UPDATE ON servers
    FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- ================================================================ categories ==

ALTER TABLE categories
    ADD COLUMN parent_id uuid REFERENCES categories (id) ON DELETE RESTRICT;

-- The tree read orders siblings within a parent, so the index carries the same columns.
CREATE INDEX categories_parent_idx ON categories (parent_id, sort_order, name);

-- The guard. Two SQLSTATEs would be indistinguishable to a client, so the failures carry
-- constraint names instead: `categories_no_cycle` and `categories_depth_bound`. The Go layer maps
-- them to stable codes (`category_cycle`, `category_too_deep`); see internal/catalog/nesting.go.
CREATE OR REPLACE FUNCTION categories_guard_tree() RETURNS trigger AS $$
DECLARE
    max_depth      constant int := 4;
    parent_depth   int := 0;
    subtree_height int := 1;
    found_cycle    boolean := false;
BEGIN
    -- Held until this transaction commits. See decision 1: the ancestor walk below is only
    -- authoritative if no other transaction is re-parenting a node it can see.
    PERFORM pg_advisory_xact_lock(hashtext('catalog.categories.tree'));

    IF NEW.parent_id IS NOT NULL THEN
        IF NEW.parent_id = NEW.id THEN
            RAISE EXCEPTION 'a category cannot be its own parent'
                USING ERRCODE = '23514', CONSTRAINT = 'categories_no_cycle';
        END IF;

        -- Walk up from the proposed parent. If this category is in that chain, the write would
        -- close a loop. The 64-level stop is a belt-and-braces for data written before this
        -- trigger existed; nothing this trigger wrote can produce a chain that long.
        WITH RECURSIVE ancestors AS (
            SELECT c.id, c.parent_id, 1 AS depth
              FROM categories c
             WHERE c.id = NEW.parent_id
            UNION ALL
            SELECT c.id, c.parent_id, a.depth + 1
              FROM categories c
              JOIN ancestors a ON c.id = a.parent_id
             WHERE a.depth < 64
        )
        SELECT COALESCE(max(depth), 0), COALESCE(bool_or(id = NEW.id), false)
          INTO parent_depth, found_cycle
          FROM ancestors;

        IF found_cycle THEN
            RAISE EXCEPTION 'category % cannot be moved under its own descendant', NEW.id
                USING ERRCODE = '23514', CONSTRAINT = 'categories_no_cycle';
        END IF;
    END IF;

    -- How tall is the branch this write carries? A leaf is one level; a node with children is
    -- deeper, because a move takes its whole subtree with it (decision 2).
    WITH RECURSIVE descendants AS (
        SELECT c.id, 1 AS depth
          FROM categories c
         WHERE c.parent_id = NEW.id
        UNION ALL
        SELECT c.id, d.depth + 1
          FROM categories c
          JOIN descendants d ON c.parent_id = d.id
         WHERE d.depth < 64
    )
    SELECT COALESCE(max(depth), 0) INTO subtree_height FROM descendants;
    subtree_height := subtree_height + 1;

    IF parent_depth + subtree_height > max_depth THEN
        RAISE EXCEPTION 'category nesting is limited to % levels; this write would reach level %',
            max_depth, parent_depth + subtree_height
            USING ERRCODE = '23514', CONSTRAINT = 'categories_depth_bound';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER categories_guard_tree
    BEFORE INSERT OR UPDATE OF parent_id ON categories
    FOR EACH ROW EXECUTE FUNCTION categories_guard_tree();

-- ================================================================= products ==

-- The provisioner target a product is sold onto. Nullable for the reason in decision 5, and
-- `ON DELETE SET NULL` for the reason in decision 3's neighbour: the API refuses to delete a
-- server a product points at, so this is the belt-and-braces for a hand-written DELETE, and a
-- product that loses its server falls back to its plan's region — exactly what it had before.
ALTER TABLE products
    ADD COLUMN server_id uuid REFERENCES servers (id) ON DELETE SET NULL;

-- "Which products are on this server" decides whether the row may be deleted, so it is indexed.
-- Partial, because almost every row is NULL and there is nothing to find.
CREATE INDEX products_server_idx ON products (server_id) WHERE server_id IS NOT NULL;

COMMENT ON COLUMN categories.parent_id IS
    'The parent category, or NULL for a root. Cycles and the four-level depth bound are enforced '
    'by the categories_guard_tree trigger (constraints categories_no_cycle / '
    'categories_depth_bound), not by the callers: two concurrent writes can each look safe and '
    'still form a cycle.';

COMMENT ON COLUMN products.server_id IS
    'The provisioner target this product is sold onto. NULL means "whatever the price version '
    'says", which is what every product did before 0049. The plan''s provision_region wins when '
    'it is set; this column supplies the region when it is not, and reaches the adapter as '
    'CreateRequest.Region — never as a second routing mechanism.';

COMMIT;
