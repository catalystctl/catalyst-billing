-- 0050_customer_closure_and_mfa_reset.sql
--
-- Two facts the schema could not state: that a customer has closed their account, and that
-- an operator has taken somebody's second factor away and that person must enrol a new one.
--
-- Three decisions shape this file, and each is written down because the next person will
-- otherwise "simplify" one of them back into a defect.
--
-- 1. **Closing an account is a soft delete, and this is the column that says so.**
--    `deleted_at` / `deleted_by` / `deletion_kind` mark the record closed. Nothing is
--    removed. The alternative — `DELETE FROM customers` — is not available even in
--    principle: `invoices`, `orders`, `services`, `subscriptions`, `credit` and `tickets`
--    all reference `customers (id)` with NO ACTION, and `audit_events` names the actor of
--    every change. A hard delete would therefore either fail on a foreign key (a customer
--    who ever bought anything) or succeed only for a record that has no history at all —
--    and in the second case it would still destroy the audit rows that explain how the
--    account was created and closed. A closed account also has to stay readable for tax
--    and accounting retention, which outlives the customer's wish to leave by years. So the
--    row stays, the *login* stops working, and every list that shows customers still finds
--    it — a record staff cannot find is a record they cannot answer a question about.
--
--    `deletion_kind` distinguishes the two callers, because they are different operations
--    with different guards and different evidence: `self` is the customer closing their own
--    account from the portal, `staff` is an operator closing a record. The CHECK keeps the
--    two columns consistent, so a half-written closure cannot be stored.
--
-- 2. **The customer's address stays on the closed row.** Releasing `lower(email)` would let
--    a stranger re-register an address that still has invoices and audit rows against it,
--    and the unique index on `customers (lower(email))` is what keeps a customer's billing
--    history attached to one identity. An operator who genuinely needs the address for a
--    new record changes the old row's address first (a deliberate, audited edit) rather than
--    having the schema do it silently.
--
-- 3. **A staff MFA reset is recorded on the account it happened to.** `mfa_enrolment_required`
--    is the demand: the next session for that account is told to enrol a new factor, and the
--    portal will not leave the enrolment screen until one is confirmed. `mfa_reset_at` and
--    `mfa_reset_by` say when and by whom, which is the question the customer's next support
--    conversation starts with. The audit row (`user.mfa_reset`) is the durable evidence; these
--    columns are the current fact that a screen can read without joining the audit table.
--
--    The default is `false`, so every existing account is unchanged: nobody is asked to enrol
--    because of this migration.
--
-- No table is created, so nothing needs adding to the test harness's truncate list, and no
-- grant needs re-issuing: `billing_app` holds table-level privileges, which cover columns
-- added later.

-- ------------------------------------------------------- customer closure --

ALTER TABLE customers
    ADD COLUMN deleted_at      timestamptz,
    -- NULL when the customer closed their own account; the operator's id otherwise.
    -- Deliberately not ON DELETE CASCADE: an operator who closed accounts has audit rows
    -- naming them, so their own account is already undeletable and a cascade here would be
    -- a second, quieter way to lose the attribution.
    ADD COLUMN deleted_by      uuid REFERENCES users (id),
    ADD COLUMN deletion_kind   text CHECK (deletion_kind IN ('self', 'staff')),
    -- Why, in the operator's or the customer's words. Empty is allowed: a customer closing
    -- their own account is not required to justify it.
    ADD COLUMN deletion_reason text NOT NULL DEFAULT '';

-- A closure is either complete or absent. Without this, a bug that set `deleted_at` but not
-- `deletion_kind` would produce a row the screen cannot explain.
ALTER TABLE customers
    ADD CONSTRAINT customers_deletion_consistent CHECK (
        (deleted_at IS NULL AND deletion_kind IS NULL)
        OR (deleted_at IS NOT NULL AND deletion_kind IS NOT NULL)
    );

-- The support question is "which accounts are closed", so the partial index is the one the
-- filter needs and it costs nothing for the open rows that every other query reads.
CREATE INDEX customers_closed_idx ON customers (deleted_at) WHERE deleted_at IS NOT NULL;

-- ----------------------------------------------------------- mfa reset --

ALTER TABLE users
    -- The demand, not a policy flag: it is set by an operator's reset and cleared when the
    -- account confirms a new factor. Nothing else sets it, so `true` always means "somebody
    -- took a factor away and this account owes the panel a new one".
    ADD COLUMN mfa_enrolment_required boolean NOT NULL DEFAULT false,
    ADD COLUMN mfa_reset_at           timestamptz,
    ADD COLUMN mfa_reset_by           uuid REFERENCES users (id);

-- The three travel together: a reset with no timestamp is a demand nobody can date, and a
-- timestamp with no actor is a change nobody can attribute.
ALTER TABLE users
    ADD CONSTRAINT users_mfa_reset_consistent CHECK (
        (mfa_reset_at IS NULL AND mfa_reset_by IS NULL)
        OR (mfa_reset_at IS NOT NULL AND mfa_reset_by IS NOT NULL)
    );
