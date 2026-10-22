-- 0056_staff_billing_identity.sql
--
-- Give every existing staff account a billing identity, so an agent can buy and hold a
-- service without an operator creating a `customers` row by hand and linking it.
--
-- Why this is a migration and not only a code change: `authn.CreateUser` now creates the
-- billing identity at user-creation time, but that only helps accounts created *after* this
-- ships. Every account that already exists — including the bootstrap administrator made by
-- `billing admin create-user`, which is the account an operator is most likely to want to
-- buy a test server with — has no billing record, and cannot check out, because checkout and
-- the portal both resolve the customer through `customer_accounts`.
--
-- What this does NOT do, deliberately:
--
--   * It does not link a login to a billing record another login already owns. Two logins on
--     one billing account would give the second one access to the first one's invoices
--     through the customer-scoped path (`WHERE ca.user_id = $n`), which is exactly the
--     privilege escalation this feature must not introduce. Such an account is left alone and
--     reported by the query below, not silently shared.
--   * It does not create a *second* billing record for an address that already has one. An
--     imported customer whose address nobody could sign in with is adopted instead: one
--     address is one billing account, which is also what the unique index on
--     `lower(customers.email)` enforces.
--   * It does not touch anyone's permissions. The row written here is a `customer_accounts`
--     link and nothing else: a staff member's reach is still decided by their role, and the
--     customer-scoped queries they can now reach are scoped to the record created here.
--
-- Both statements are idempotent, so re-running this file against a database that has already
-- had it applied changes nothing — which is what makes it safe to test by executing the file
-- itself.
--
-- Reviewing the result afterwards, the second query is the accounts this migration could not
-- fix, and each of them needs a human decision (usually: the address belongs to a customer
-- record another login owns):
--
--   -- every staff account, and the billing record it owns (NULL means "still cannot buy")
--   SELECT u.email, c.id AS customer_id
--   FROM users u
--   JOIN staff_roles sr ON sr.user_id = u.id
--   LEFT JOIN customer_accounts ca ON ca.user_id = u.id
--   LEFT JOIN customers c ON c.id = ca.customer_id
--   ORDER BY c.id NULLS FIRST, u.email;
--
--   -- a staff account whose address is already another login's billing account
--   SELECT u.email, owner.user_id
--   FROM users u
--   JOIN staff_roles sr ON sr.user_id = u.id
--   JOIN customers c ON lower(c.email) = lower(u.email)
--   JOIN customer_accounts owner ON owner.customer_id = c.id AND owner.user_id <> u.id
--   WHERE NOT EXISTS (SELECT 1 FROM customer_accounts ca WHERE ca.user_id = u.id);

BEGIN;

-- 0. Provenance for the records this file (and `ensureBillingIdentity`) create.
--
-- Deleting a login has to tell two billing records apart: one that exists *because a login
-- needed one*, which holds nothing but an address and should go when the login goes, and one an
-- operator or an import created, which carries a name, an address and possibly attribution that
-- outlive the login and must never be cascaded away. Without this column the delete path could
-- only refuse both, which would make every account in the panel undeletable — including the
-- mistyped address created a minute ago, the one case where delete is the right answer.
--
-- The default is false, so every existing row and every INSERT elsewhere in the codebase
-- (the importer, the customers screen, registration) keeps the meaning it has today.
-- IF NOT EXISTS keeps this file re-runnable, which is what lets a test execute it.
ALTER TABLE customers
    ADD COLUMN IF NOT EXISTS provisioned_for_login boolean NOT NULL DEFAULT false;

COMMENT ON COLUMN customers.provisioned_for_login IS
    'True when this billing record was created because a login needed one (authn.ensureBillingIdentity, migration 0056). Only such a record may be removed together with its login.';

-- 1. A billing record for every staff login that has none and whose address no billing record
--    uses yet. The name is the display name, because that is what an operator calls the person
--    and what the customer record shows on the next document; the address is the fallback for
--    the accounts created before display names were mandatory.
--
--    ON CONFLICT DO NOTHING without a target: the unique index that can collide is the
--    expression index on `lower(email)`, which has no constraint name to name here.
INSERT INTO customers (email, name, provisioned_for_login)
SELECT u.email, COALESCE(NULLIF(u.display_name, ''), u.email), true
FROM users u
JOIN staff_roles sr ON sr.user_id = u.id
WHERE NOT EXISTS (SELECT 1 FROM customer_accounts ca WHERE ca.user_id = u.id)
  AND NOT EXISTS (SELECT 1 FROM customers c WHERE lower(c.email) = lower(u.email))
ON CONFLICT DO NOTHING;

-- 2. Link it. This covers both records created by step 1 and records an import already made
--    for the same address; a record another login already owns is excluded, which is the rule
--    stated in the header. `owner` is the role the registration path and the screen use: the
--    person the account belongs to.
INSERT INTO customer_accounts (user_id, customer_id, role)
SELECT u.id, c.id, 'owner'
FROM users u
JOIN staff_roles sr ON sr.user_id = u.id
JOIN customers c ON lower(c.email) = lower(u.email)
WHERE NOT EXISTS (SELECT 1 FROM customer_accounts ca WHERE ca.user_id = u.id)
  AND NOT EXISTS (SELECT 1 FROM customer_accounts owner WHERE owner.customer_id = c.id)
ON CONFLICT (user_id, customer_id) DO NOTHING;

COMMIT;
