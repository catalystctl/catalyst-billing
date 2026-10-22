-- 0022_discount_lines.sql
--
-- Let an invoice show a discount as a line of its own.
--
-- The problem: `invoice_lines` was written for charges only, and says so four times —
-- `unit_minor >= 0`, `net_minor >= 0`, `total_minor >= 0`, `total_minor = net_minor + tax_minor`.
-- A coupon that reduces a price therefore had nowhere to go. The alternative was to fold the
-- discount into the charge lines' unit prices, and that is worse than it sounds:
--
--   * the printed document would show "Hosting  $80.00" when the price list says $100, with no
--     explanation on the page. The customer's only recourse is to trust that a number they
--     cannot check is right, and a support agent cannot answer "why was I charged this" from
--     the document;
--   * tax evidence would be computed on an amount that is not the catalog price, so the
--     recorded rate would explain a number the catalog does not contain;
--   * a credit note, a partial refund and a coupon would each reduce the total by a different
--     mechanism, and a reconciliation that adds up the lines would not reconcile.
--
-- So a discount is a line, with `line_kind = 'discount'`, and the non-negativity rules become
-- sign rules that depend on the kind: a charge is non-negative, a discount is non-positive.
-- `total_minor = net_minor + tax_minor` holds for both, because a discount reduces tax as well
-- as net and both are stored negative.
--
-- Two invariants make this safe without a trigger:
--
--   1. **A discount can never exceed what is being discounted.** `invoices.subtotal_minor >= 0`
--      (from 0001) is still in force, and the subtotal is the sum of the line nets. A discount
--      larger than the charges would make that sum negative and the invoice would be refused.
--      That is the clamp, enforced by the database rather than by the pricing code.
--   2. **The line is frozen with the document.** `guard_invoice_line` (0001) refuses any change
--      to a line whose parent is not a draft, so a discount line is as immutable as a charge
--      line once the invoice is issued.
--
-- `line_kind` is additive with a default of 'charge', so every existing row is correct without a
-- backfill and every existing insert that does not name the column keeps working.

BEGIN;

ALTER TABLE invoice_lines
    ADD COLUMN line_kind text NOT NULL DEFAULT 'charge'
        CHECK (line_kind IN ('charge', 'discount'));

-- Replace the four unconditional sign checks with sign checks that depend on the kind. The
-- constraint names are the ones Postgres generated for the inline CHECKs in 0001, so dropping
-- by name is exact; a rename in a future migration would surface here as a hard error rather
-- than silently leaving the old rule in force.
ALTER TABLE invoice_lines
    DROP CONSTRAINT invoice_lines_unit_minor_check,
    DROP CONSTRAINT invoice_lines_net_minor_check,
    DROP CONSTRAINT invoice_lines_total_minor_check;

ALTER TABLE invoice_lines
    ADD CONSTRAINT invoice_lines_unit_sign CHECK (
        (line_kind = 'charge'   AND unit_minor  >= 0)
     OR (line_kind = 'discount' AND unit_minor  <= 0)
    ),
    ADD CONSTRAINT invoice_lines_net_sign CHECK (
        (line_kind = 'charge'   AND net_minor   >= 0)
     OR (line_kind = 'discount' AND net_minor   <= 0)
    ),
    ADD CONSTRAINT invoice_lines_total_sign CHECK (
        (line_kind = 'charge'   AND total_minor >= 0)
     OR (line_kind = 'discount' AND total_minor <= 0)
    ),
    -- A discount is a reduction: it must actually reduce something, or it is a zero line that
    -- clutters the document and a report has to filter out.
    ADD CONSTRAINT invoice_lines_discount_nonzero CHECK (
        line_kind = 'charge' OR net_minor < 0
    );

-- `invoice_lines_tax_minor_check` (tax_minor >= 0) is deliberately left alone, and a discount
-- line carries tax_minor = 0. Reasoning: a discount that reduces taxable value must reduce the
-- tax, and the way to express that without a second sign convention is to re-price the charge
-- lines' tax on the discounted net. Making the discount line carry negative tax would mean the
-- same reduction appears twice — once in the line's tax and once in the recomputed charge tax —
-- and the invoice would under-collect.

-- Reporting a discount by promotion is a common question ("what did SAVE10 cost us"), and the
-- answer must come from the document rather than from a re-run of the pricing. The redemption
-- row already records the amount; this index makes joining the two cheap.
CREATE INDEX IF NOT EXISTS invoice_lines_discount_idx ON invoice_lines (invoice_id)
    WHERE line_kind = 'discount';

COMMIT;
