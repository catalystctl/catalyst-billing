-- 0075_quantity_modes.sql
--
-- SF-09: `separated` and `combined` must stop being two spellings of one order shape.
--
-- `products.allow_quantity` has had three values since 0020, but checkout treated
-- `separated` exactly like `combined`: quantity N produced one order item, one
-- subscription and (after payment) one service. They now differ:
--
--   * combined  — one order item of quantity N, one subscription, one service whose
--                 `services.quantity` is N (the column 0029 added).
--   * separated — N order items of quantity one, N subscriptions and N services. Each
--                 unit is an independent thing an operator can suspend, renew or move
--                 on its own.
--
-- The one structural fact needed for that is which subscription an order line belongs
-- to. Previously fulfilment linked every service from an invoice to the invoice's
-- single `subscription_id`, so an order with several schedules could only point all of
-- its services at the first one. That is already wrong for a multi-line order; it is
-- unusable for N separated units. Recording it on the order line is the smallest fix:
-- it is the line's own schedule, not the document's, and it stays nullable so a
-- one-time line (which has no schedule) and every pre-existing row are unaffected.
--
-- Nothing here rewrites an issued invoice, a journal entry or an audit row.

BEGIN;

ALTER TABLE order_items
    ADD COLUMN subscription_id uuid REFERENCES subscriptions (id) ON DELETE SET NULL;

-- Fulfilment asks "what does this invoice's order sell, and which schedule does each
-- line fund", so the lookup follows the order and then the line. The partial index keeps
-- the nullable, historical rows out of it.
CREATE INDEX order_items_subscription_idx
    ON order_items (subscription_id)
    WHERE subscription_id IS NOT NULL;

COMMENT ON COLUMN order_items.subscription_id IS
    'The schedule this order line funds. NULL for a one-time line. A separated quantity '
    'creates one line and one subscription per unit, so fulfilment links each service to '
    'its own schedule rather than to the invoice''s first one.';

COMMIT;
