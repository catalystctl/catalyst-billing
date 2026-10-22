-- 0052_manual_price_lines.sql
--
-- A negotiated price is a property of the line, not of the catalog.
--
-- Until now an order line could only be priced from a price version: the amount came from
-- `price_versions.amount_minor` and the caller had no way to say "this customer pays 5.00".
-- The obvious shortcut — edit the price version, or insert a private one for this customer —
-- is wrong in a way that is invisible at the time and expensive later: the price list is
-- shared, so the next customer who buys the same product silently pays the discounted amount,
-- and the catalog no longer records what the product costs.
--
-- So the override lives where the charge lives. `unit_minor` on the order item and the invoice
-- line already holds the amount that was actually charged; what those rows could not say is
-- *why* that number differs from the catalog, or that it differs at all. These two columns add
-- exactly that fact:
--
--   manual_price        — the amount on this line was typed by a person, not read from a price
--                         version. A report that wants "what the catalog says" filters it out.
--   manual_price_reason — the justification, mandatory.
--
-- The pair is coupled by a constraint rather than by convention: a manual price with no reason
-- is refused, and a reason on a catalog-priced line is refused too, because a reason that is
-- not attached to an override explains nothing and would make "why is this discounted?"
-- ambiguous. Blank-but-present reasons ('   ') are refused for the same reason.
--
-- Nothing here touches `products` or `price_versions`. That is the point of the migration, and
-- a test asserts the catalog is byte-identical after a negotiated order.
--
-- Both columns are additive with defaults, so every existing row stays correct without a
-- backfill and every existing insert that does not name the columns keeps working.

BEGIN;

ALTER TABLE order_items
    ADD COLUMN manual_price        boolean NOT NULL DEFAULT false,
    ADD COLUMN manual_price_reason text;

ALTER TABLE order_items
    ADD CONSTRAINT order_items_manual_price_reason CHECK (
        (manual_price AND manual_price_reason IS NOT NULL
                      AND btrim(manual_price_reason) <> '')
     OR (NOT manual_price AND manual_price_reason IS NULL)
    );

ALTER TABLE invoice_lines
    ADD COLUMN manual_price        boolean NOT NULL DEFAULT false,
    ADD COLUMN manual_price_reason text;

ALTER TABLE invoice_lines
    ADD CONSTRAINT invoice_lines_manual_price_reason CHECK (
        (manual_price AND manual_price_reason IS NOT NULL
                      AND btrim(manual_price_reason) <> '')
     OR (NOT manual_price AND manual_price_reason IS NULL)
    );

-- "How much of this month's revenue was negotiated rather than catalog-priced" is a question
-- finance asks, and the answer must come from the document rather than from a re-read of the
-- order. The partial index keeps that query off the full line table.
CREATE INDEX invoice_lines_manual_price_idx ON invoice_lines (invoice_id)
    WHERE manual_price;

COMMIT;
