-- 0051_credit_debit.sql
--
-- A staff debit of a customer's credit balance: the entry type the ledger was missing.
--
-- Until now the subledger could only *add* credit. A grant (internal/credit/grant.go) credits
-- `customer_credit`; an application debits it but only against an invoice, where the money moves
-- to `receivable`. Nothing could reduce a balance without a document to hang it on, so an
-- operator who had granted credit in error — or who had to take back credit after a chargeback
-- that arrived outside the gateway — had exactly one tool: SQL. That is unauditable by
-- construction, and it leaves no trace in the customer's own statement.
--
-- This migration adds the missing entry type. It adds *only* the entry type: no balance column,
-- no mutable state, nothing that could disagree with the journal.
--
-- 1. **Why a new `kind` rather than reusing `adjustment`.** Grants already post as
--    `adjustment`, and so do deposits' grants and order corrections. An operator reading
--    `journal_transactions` to answer "why did this customer's credit drop" cannot tell a
--    goodwill grant from a claw-back if both are `adjustment` — the direction is in the entries
--    and the reason is in a memo, but the *class* of event is not. A debit is a distinct
--    economic event: value leaves the customer, and it leaves it because a person decided so.
--    Giving it its own kind is what makes the ledger self-describing, and the kind is the first
--    column anyone filters on. It is also what lets the customer-facing statement label the row
--    ("taken off by staff") instead of calling it an adjustment.
--
--    The alternative — a `credit_debits` table of its own — was rejected for the reason the
--    balance is derived rather than stored: a second table recording the same event is a second
--    source of truth, and the first time the two disagree the ledger is right and the table is
--    wrong. The debit needs no document: nothing is issued to the customer, no number is
--    consumed, and the paper trail is the audit row written in the same transaction. A grant
--    needs a credit note because a tax filing refers to note numbers; a debit does not.
--
-- 2. **Why `reference_type` needs no migration.** It is plain text in 0001 with no CHECK, and
--    the unique key is (kind, reference_type, reference_id). The debit posts with
--    reference_type = 'credit_debit' and reference_id = a UUID derived from the request's
--    Idempotency-Key, so a double-submit collides on that unique key and is a no-op rather than
--    a second deduction — the same construction the credit application already uses.
--
-- 3. **Why the non-negative rule is not enforced here.** A CHECK on `journal_entries` cannot
--    express "the running balance of this customer in this currency never goes below zero": a
--    row-level check sees one entry, and a deferred constraint trigger would have to sum the
--    account at COMMIT for every posting in the system, including the legitimate sequences that
--    momentarily look negative. The rule belongs where the decision is made — inside the
--    transaction that holds the customer row lock, in internal/credit/debit.go — and it is
--    proven there by a test that debits one minor unit more than the balance and asserts both
--    the refusal and the unchanged balance.
--
-- 4. **Nothing here weakens append-only.** The triggers from 0001 (`journal_entries_append_only`,
--    `journal_transactions_append_only`) already refuse UPDATE and DELETE on both tables, and the
--    debit is an INSERT. A correction to a debit is another posting, never an edit.
--
-- The kind is added to the CHECK by dropping and re-adding it: the constraint from 0001 is a
-- value list, and there is no way to extend one in place. The drop and the add are in the same
-- transaction as the rest of this file, so no window exists in which the constraint is absent.

BEGIN;

ALTER TABLE journal_transactions
    DROP CONSTRAINT journal_transactions_kind_check;

ALTER TABLE journal_transactions
    ADD CONSTRAINT journal_transactions_kind_check
    CHECK (kind IN ('invoice_issued', 'payment_received', 'refund', 'credit_note',
                    'adjustment', 'credit_debit'));

COMMENT ON CONSTRAINT journal_transactions_kind_check ON journal_transactions IS
    'The closed set of economic events. credit_debit (0051) is a staff deduction from a '
    'customer''s credit balance: it debits customer_credit and credits revenue, reverses a '
    'goodwill allowance or recognises a forfeited deposit, and always carries a mandatory '
    'reason in its memo and an audit row written in the same transaction.';

COMMIT;
