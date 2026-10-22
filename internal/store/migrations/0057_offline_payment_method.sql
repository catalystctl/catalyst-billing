-- 0057_offline_payment_method.sql
--
-- A payment recorded by staff has to say *how* the money arrived. Until now the payments
-- table could express only a gateway payment: provider, provider account and the provider's
-- own payment id. An operator recording a bank transfer had nowhere to put "bank transfer",
-- so the row said provider='manual' and the method was lost — which means the payment list
-- could not tell a cheque from a cash payment, and a reconciliation report could not group
-- offline receipts at all.
--
-- Two columns, and the reasoning behind each is worth keeping because both were tempting to
-- express differently:
--
-- 1. **`method` is a column, not a provider value.** Encoding the method in `provider`
--    ("manual_bank_transfer") would put it in the one field whose meaning is "who took the
--    money". The provider is the *source of truth* for a payment: for a gateway payment it is
--    the thing you query when the numbers disagree. Stretching it to carry a channel would
--    make `provider` mean two things and would break the refund path, which uses it to
--    select the gateway that must be called to give the money back. So the method is its own
--    field and the provider stays honest: 'manual' says a person recorded it, `method` says
--    how it arrived.
--
-- 2. **`method` is nullable.** A gateway payment has no method here, and that is not a gap:
--    the gateway knows whether it was a card, a wallet or a direct debit, it reports that
--    through its own API, and inventing a local value for it would be a second, divergent
--    copy of a fact the provider owns. NULL therefore means "not an offline payment, or one
--    recorded before this column existed"; every row written by the offline path has a
--    value. The CHECK still makes the *closed set* a database fact rather than a convention
--    the handler happens to enforce, so a future code path cannot invent a fifth method.
--
-- `note` is the operator's own sentence ("paid by the customer's accountant, ref on the
-- statement"). It is deliberately free text: it is not used for logic, only for the human
-- reading the payment list six months later. It is capped at 500 characters in the handler,
-- not here, because a length limit is a product decision and not an invariant of the ledger.
--
-- There is deliberately no index. The table is read by id, by provider identity and by
-- received_at (the list's keyset); filtering by method is a screen-level convenience over a
-- set of payments that is small compared to the ledger, and an index that no query needs is
-- write cost for nothing.

ALTER TABLE payments ADD COLUMN method text;
ALTER TABLE payments ADD COLUMN note text;

ALTER TABLE payments ADD CONSTRAINT payments_method_closed_set CHECK (
    method IS NULL OR method IN ('bank_transfer', 'cash', 'cheque', 'other')
);
