-- 0061_storefront_merchandising.sql
--
-- The storefront's merchandising surface: a category that can look like something (SF-06,
-- CAT-01), and a product an operator can promote and mark as out of stock (SF-03, SF-06,
-- SF-09).
--
-- Three things are added, and each is a column rather than a table, because each is a
-- property of a row that already exists:
--
-- 1. **A category has an image.** Products have had one since 0018 (`products.image_id`,
--    `product_images` with digest dedupe and a 256 KiB cap) and categories have not, so a
--    storefront's category strip could only ever be text. The column references the same
--    `product_images` table, with `ON DELETE SET NULL` rather than a cascade: an image is
--    content, and deleting one must not delete the category that was using it. The storefront
--    already serves those bytes through `/api/v1/media/{id}` with an immutable cache and the
--    SVG sandbox, so a category image needs no second serving path, no second size cap and no
--    second allowlist — only the reference.
--
-- 2. **A product can be featured.** `hidden` is "stop selling this"; it says nothing about
--    which of the products still on sale deserve the front of the shop. Without a flag, an
--    operator expressing "these three are the ones we push" has to abuse `sort_order`, which
--    is per-category and would reorder the grid. `featured` is a promotion flag: it changes
--    nothing about price, visibility or billing, and a featured hidden product stays hidden —
--    every storefront read applies the existing visibility predicates first, so this column
--    can never widen what a visitor may see.
--
-- 3. **Stock and quantity policy become editable catalogue metadata.** `stock`,
--    `per_user_limit` and `allow_quantity` have existed since 0020 but only through SQL: no
--    API path and no screen wrote them, so an operator could not put a product out of stock
--    or sell more than one of it. They remain on `products` (the checkout lock in
--    `internal/register` reads `products.stock FOR UPDATE`, so a separate table would be a
--    second source of truth for the same fact) and this migration adds nothing but the
--    documentation of the rule the API enforces:
--
--      `stock` NULL means unlimited, 0 means sold out, and negative is not a number of
--      servers anyone can ship.
--
--    The constraints that express that already exist from 0020 (`stock >= 0`,
--    `per_user_limit > 0`, `allow_quantity IN ('disabled','separated','combined')`), which is
--    why this file does not restate them. What it does add is the index the storefront read
--    needs once it filters on stock: "show what is in stock" is a predicate on
--    `(hidden, stock)`, and the existing `products_storefront_idx` does not carry `stock`.
--
-- Nothing here rewrites an issued invoice, a price version, an order or an audit row. Every
-- new column is either nullable or defaulted, so a catalogue that existed before this
-- migration reads exactly as it did: no category has an image, no product is featured, and
-- every product keeps the stock and quantity policy it was created with.

BEGIN;

-- ------------------------------------------------------- category imagery --

ALTER TABLE categories
    ADD COLUMN image_id uuid REFERENCES product_images (id) ON DELETE SET NULL;

-- `sort_order` decides the strip's order, and a category row on the storefront now renders an
-- image, so the index that already serves the strip gains nothing from a second one on
-- `image_id`. The partial index below is for the reverse question — "is this image used?" —
-- which the media endpoint does not ask today and a cleanup job would.
CREATE INDEX categories_image_idx ON categories (image_id) WHERE image_id IS NOT NULL;

COMMENT ON COLUMN categories.image_id IS
    'The category''s logo, stored in product_images and served by /api/v1/media/{id}. NULL means '
    'no image, which renders as a generated mark rather than a broken <img>. ON DELETE SET NULL '
    'because an image is content: removing bytes must not remove the category that showed them.';

-- ------------------------------------------------------------ featured --

ALTER TABLE products
    ADD COLUMN featured boolean NOT NULL DEFAULT false;

-- The storefront's featured row is "published, priced, featured", in catalogue order. Partial,
-- because almost every product is not featured and there is nothing to find on those rows.
CREATE INDEX products_featured_idx ON products (featured, sort_order, name) WHERE featured;

COMMENT ON COLUMN products.featured IS
    'A promotion flag: put this product at the front of the storefront. It is not a visibility '
    'grant — the public reads apply "not hidden, in a visible category, priced" first, so a '
    'featured hidden product is still hidden.';

-- ------------------------------------------------------- stock read path --

-- The storefront shows "out of stock" and refuses the buy button, so its read filters on
-- `stock = 0`. That predicate is not in `products_storefront_idx`, and a shop with one sold-out
-- product otherwise sorts the whole catalogue to find it.
CREATE INDEX products_stock_idx ON products (category_id, sort_order) WHERE stock = 0;

COMMENT ON COLUMN products.stock IS
    'Units available. NULL is unlimited, 0 is sold out. Decremented under a FOR UPDATE lock at '
    'checkout and restored when an unpaid order is cancelled; the storefront reads it only to '
    'say "out of stock" and to disable the buy button.';

COMMENT ON COLUMN products.allow_quantity IS
    'disabled: one per order and the storefront offers no quantity field. separated: quantity N '
    'is N independent services. combined: quantity N is one service with N units.';

COMMIT;
