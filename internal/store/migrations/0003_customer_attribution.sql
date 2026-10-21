-- 0003_customer_attribution.sql
--
-- Attribute every subledger entry to the customer it concerns.
--
-- Without this, "what does this customer owe" has to be reconstructed by joining
-- each posting back to its document, and a payment posting (which references a
-- payment, not an invoice) drops out of the receivable entirely: the invoice
-- debit is counted, the payment credit is not, and a fully paid customer still
-- looks like they owe money.
--
-- Storing the attribution on the entry makes the receivable a single indexed
-- aggregate and keeps it correct no matter which document produced the entry.

ALTER TABLE journal_entries
    ADD COLUMN customer_id uuid REFERENCES customers (id);

-- Receivable and customer-credit balances are the hot queries.
CREATE INDEX journal_entries_customer_account_idx
    ON journal_entries (customer_id, account_code, currency)
    WHERE customer_id IS NOT NULL;

-- Backfill from the referenced document, so an existing ledger keeps working.
UPDATE journal_entries je
   SET customer_id = i.customer_id
  FROM journal_transactions jt
  JOIN invoices i ON i.id = jt.reference_id
 WHERE jt.id = je.transaction_id
   AND jt.reference_type = 'invoice'
   AND je.customer_id IS NULL;

UPDATE journal_entries je
   SET customer_id = i.customer_id
  FROM journal_transactions jt
  JOIN payment_allocations pa ON pa.payment_id = jt.reference_id
  JOIN invoices i ON i.id = pa.invoice_id
 WHERE jt.id = je.transaction_id
   AND jt.reference_type = 'payment'
   AND je.customer_id IS NULL;
