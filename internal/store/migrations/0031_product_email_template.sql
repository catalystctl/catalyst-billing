-- 0031_product_email_template.sql
--
-- Give the per-product activation snippet a bound, a reader and a comment.
--
-- Decision, recorded because the work package reserved two numbers: **0030 is not used.** The
-- template and event tables already carry the credential events. `0020_commerce.sql` created
-- `notification_templates` (one row per event, with the in-app channel beside the mail channel)
-- and `0023_notification_events.sql` replaced the closed `kind` set with a shape check, so
-- routing the password-reset and email-verification mails through the editable templates needs
-- no schema change at all. A migration that only re-seeds rows would also be wrong: the seeding
-- is `notify.SeedTemplates` at boot, which is idempotent and never overwrites an operator's
-- edit, and a migration that inserted defaults would silently re-clobber that edit if anyone
-- ever re-ran it. **0031 is used, and only for the product column.**
--
-- The column itself is not new. `0020_commerce.sql` added `products.email_template` as
-- `text NOT NULL DEFAULT ''` and nothing has read it since: a product could carry instructions
-- for the customer and the activation mail said the same generic sentence for every product.
-- The activation event (`notify.ServiceActivated`) now renders the column into the message as
-- `{{product_instructions}}`.
--
-- What the column lacked, and what this file adds:
--
-- 1. **A bound.** The snippet is operator prose, rendered as text/plain into a customer's mail.
--    An unbounded text column is a blob store, and a 200 KB "snippet" is not a message. 4000
--    characters is several paragraphs — more than any setup instruction — and the CHECK is what
--    makes the bound true for every writer, including a future importer that does not go through
--    the Go validator.
--
-- 2. **A comment that names the reader.** The reason the column was dead for so long is that
--    nothing said what read it. A comment on the column is the cheapest place to record that:
--    whoever opens `\d products` next sees where the value goes.
--
-- The bound is a `length()` (characters), not `octet_length()` (bytes), because the Go validator
-- in `internal/catalog` counts bytes and is therefore the stricter of the two. A snippet that
-- passes Go cannot fail this CHECK, and the CHECK still catches a writer that bypasses Go.

BEGIN;

ALTER TABLE products
    ADD CONSTRAINT products_email_template_len
    CHECK (length(email_template) <= 4000);

COMMENT ON COLUMN products.email_template IS
    'Per-product activation snippet, plain text. Rendered into the activation mail as '
    '{{product_instructions}} by notify.ServiceActivated. Empty means the shipped default '
    'sentence is used. Editable through PUT /api/v1/staff/catalog/products/{id} (catalog:write).';

COMMIT;
