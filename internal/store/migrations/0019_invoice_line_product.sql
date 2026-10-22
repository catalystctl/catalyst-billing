-- 0019_invoice_line_product.sql
--
-- A manual invoice can now sell a product.
--
-- Fulfilment decides what to provision from the invoice's *order*: `orders` → `order_items` →
-- `price_versions` → `products`. That is exactly right for a checkout, and it means a manual
-- invoice — the kind an operator writes for a customer who paid by bank transfer — provisions
-- nothing when it is paid, because there is no order behind it. The service the customer paid
-- for simply never appears, and the only trace is the `paid_not_provisioned` exception.
--
-- The fix is to let an invoice line name the product it sells. An invoice line is already the
-- immutable record of *what was charged*; this adds the one fact fulfilment needs that it did
-- not carry: which product, and at which price version.
--
-- Both columns are nullable. A line for "one hour of support" or "prorated difference" is a
-- legitimate invoice line that provisions nothing, and most lines will stay that way.
--
-- price_version_id is nullable *separately* from product_id because an operator may sell a
-- product at a negotiated price that has no version — the provision plan is then read from the
-- product's current version, while the amount charged stays what the operator typed. Keeping
-- the two independent is what makes "custom invoice for a standard product" expressible
-- without inventing a price version for it.

BEGIN;

ALTER TABLE invoice_lines
    ADD COLUMN product_id       uuid REFERENCES products (id) ON DELETE SET NULL,
    ADD COLUMN price_version_id uuid REFERENCES price_versions (id) ON DELETE SET NULL;

-- Fulfilment looks up an invoice's lines by product, so that is the index that matters.
CREATE INDEX invoice_lines_product_idx ON invoice_lines (invoice_id, product_id)
    WHERE product_id IS NOT NULL;

-- One product per invoice line is the model. A line that named two products would make
-- "how many services does this invoice create" ambiguous, and the quantity column already
-- expresses "three of them".

COMMIT;
