-- 0018_catalog.sql
--
-- What a storefront needs that the catalog did not have.
--
-- Until now `products` was (id, slug, name) and existed only because the checkout flow
-- inserted it. That is enough to price an invoice and nothing else: there is no way to
-- describe what is being sold, no way to group it, no way to hide it, and no way to show a
-- customer what they are buying. A storefront needs all four.
--
-- Three decisions shape this migration.
--
-- 1. Product *metadata* is mutable; a product's *price* is not. Editing a description is an
--    update; changing a price is a new `price_versions` row, because an issued invoice
--    snapshots the version it priced from. Keeping the two in one table would invite an
--    UPDATE that silently rewrites history, so the split that already existed is preserved
--    and made explicit here.
--
-- 2. The logo is a stored image, not a URL. The panel's CSP is `img-src 'self' data:`, so an
--    external logo URL is refused by the browser, and loosening that directive would let a
--    product record make every visitor beacon to a third party. The bytes live in a table and
--    are served by an endpoint that sets its own content type; `products.image_id` points at
--    one.
--
-- 3. Categories are flat. Paymenter models them as a tree; a storefront needs one level of
--    grouping, and a tree costs a recursive query on every page for no benefit until somebody
--    actually asks for sub-categories. `sort_order` gives the same control with a plain
--    ORDER BY, and adding a parent_id later is an additive migration.

BEGIN;

-- --------------------------------------------------------------- categories --

CREATE TABLE categories (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        text NOT NULL,
    name        text NOT NULL,
    description text NOT NULL DEFAULT '',
    -- Ordering is explicit rather than alphabetical: "VPS, Dedicated, Shared" is a marketing
    -- decision, not a lexicographic one.
    sort_order  int  NOT NULL DEFAULT 0,
    -- A hidden category stays in the database (its products keep their grouping) but is not
    -- offered in the storefront. Deleting a category to hide it would orphan its products.
    hidden      boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX categories_slug_unique ON categories (lower(slug));
CREATE INDEX categories_order_idx ON categories (hidden, sort_order, name);

-- ------------------------------------------------------------ product images --
--
-- Bytes in the database, not on disk. A self-hosted panel must survive a container restart
-- without a volume for uploads, and a logo is small (the size cap below is enforced by the
-- API, not by trust). The digest is stored so the same image uploaded twice is one row, and
-- so an <img> can be cached immutably by content.

CREATE TABLE product_images (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    content_type text NOT NULL CHECK (content_type IN ('image/png', 'image/jpeg', 'image/webp', 'image/svg+xml')),
    bytes       bytea NOT NULL,
    byte_size   int   NOT NULL CHECK (byte_size > 0 AND byte_size <= 262144),
    -- sha256 of the bytes, hex. Unique: identical uploads collapse to one row.
    digest      text  NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX product_images_digest_unique ON product_images (digest);

-- ------------------------------------------------------------ product columns --

ALTER TABLE products
    ADD COLUMN description  text NOT NULL DEFAULT '',
    ADD COLUMN short_description text NOT NULL DEFAULT '',
    ADD COLUMN category_id  uuid REFERENCES categories (id) ON DELETE SET NULL,
    ADD COLUMN image_id     uuid REFERENCES product_images (id) ON DELETE SET NULL,
    -- Hidden products are not offered in the storefront. They still exist, still bill, and
    -- are still visible to staff: "stop selling this" is not "delete this".
    ADD COLUMN hidden       boolean NOT NULL DEFAULT false,
    -- Presentation order within a category, same reasoning as categories.sort_order.
    ADD COLUMN sort_order   int NOT NULL DEFAULT 0,
    ADD COLUMN updated_at   timestamptz NOT NULL DEFAULT now();

-- A product is looked up by slug in the storefront on every request.
CREATE INDEX products_storefront_idx ON products (hidden, category_id, sort_order, name);

-- --------------------------------------------------------- price version name --

-- Paymenter calls this a "plan": the customer-facing name of a price point ("8 GB", "Annual").
-- A version number alone is an implementation detail and cannot be shown on a card.
ALTER TABLE price_versions
    ADD COLUMN name text NOT NULL DEFAULT '';

-- A storefront lists prices by product, newest version first.
CREATE INDEX price_versions_product_idx ON price_versions (product_id, version DESC);

-- ------------------------------------------------------------- invariants --

-- updated_at is maintained by trigger rather than by every caller remembering to set it. A
-- column that is sometimes stale is worse than no column: the catalog screen sorts by it.
CREATE OR REPLACE FUNCTION touch_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER products_touch_updated_at
    BEFORE UPDATE ON products
    FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TRIGGER categories_touch_updated_at
    BEFORE UPDATE ON categories
    FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- The storefront must never show a price of zero as if it were free, and must never show a
-- product with no price at all. Neither is expressible as a constraint on one table, so it is
-- enforced in the query (an inner join on price_versions) and stated here so the next person
-- does not "fix" it by switching to a left join.

COMMIT;
