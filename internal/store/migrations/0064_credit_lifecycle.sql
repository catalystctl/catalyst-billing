-- 0064_credit_lifecycle.sql
--
-- The two halves of "credit follows a price change": the credit a downgrade owes, and the
-- policy switch that decides whether it is posted at all (CRD-06, CRD-10, UPG-04).
--
-- # The problem this closes
--
-- A downgrade is the mirror of an upgrade. An upgrade issues an invoice for the unused part of
-- the period at the higher price; a downgrade owes the customer the unused part at the lower
-- price. The upgrade half was wired: `service.Store.upgrade` creates the prorated invoice in the
-- same transaction as the `service_upgrades` row. The downgrade half stopped at the row: it
-- recorded `prorated_minor` (negative) and reported `CreditPending`, and then nothing in
-- internal/credit ever posted the money. The customer was owed an amount that existed only in a
-- column, and the service kept renewing at the higher price because applying the row is refused
-- until the credit is posted.
--
-- # Why no new table
--
-- A downgrade credit is a credit note with no invoice behind it, which `credit_notes` already
-- models ("a goodwill credit need not correct a specific invoice"). The note is numbered from the
-- same series as every other credit note, so a tax filing keeps one sequence, and the posting is
-- an adjustment that debits revenue and credits `customer_credit` — the same treatment as a
-- goodwill grant, because both are a sales allowance and neither corrects a document's total.
--
-- What the row needs is not a second table but the *link*: which note paid this downgrade, and
-- when. Two columns, both nullable, both written once. The link is what makes posting idempotent
-- without a new idempotency table: a replay reads `credit_note_id`, finds it set, and reports the
-- existing note instead of minting a second one. It is also what makes the state visible — a
-- pending downgrade with a note and no application is a case an operator can see and finish.
--
-- # Why `service_upgrades` and not the ledger alone
--
-- The ledger already refuses a second posting with the same (kind, reference_type, reference_id),
-- and that is the guarantee the posting relies on. But the *decision* "this downgrade has been
-- credited, now move the service" is state about the upgrade, not about the money, and deriving
-- it by scanning the journal would make every read of the upgrade queue a join against entries
-- it does not own. The columns are denormalised evidence, written in the same transaction as the
-- posting, exactly like `invoice_id` on the same row.
--
-- # Why the setting is a migration and not just code
--
-- internal/settings carries a code-side default for every key, so a missing row is a working
-- installation. The row is inserted anyway so the settings page shows the switch as *set* rather
-- than as an invisible default: an operator who wants to stop returning credit on downgrades has
-- to be able to find the switch, and a value only reachable through code is not a switch.
--
-- The CHECK on `service_upgrades` is not extended: a positive row may never carry a credit note,
-- and that is a rule worth stating in the schema because the columns are nullable and a
-- hand-written UPDATE is otherwise free to set one.

BEGIN;

-- ================================================ credits.credit_on_downgrade ===
--
-- Default on: the defect being fixed is a customer owed money that nothing posts, and an
-- installation that does not want to give it back can say so. ON CONFLICT DO NOTHING keeps this
-- migration re-runnable against a database where an operator has already set the key.
INSERT INTO settings (group_key, key, value) VALUES
    ('credits', 'credit_on_downgrade', 'true')
ON CONFLICT (group_key, key) DO NOTHING;

-- ============================================== service_upgrades: the credit ==
--
-- `credit_note_id` is the note that paid the row. It is nullable for the same reason
-- `invoice_id` is: half of all rows (upgrades) have no note, and the ones that do only get one
-- once the credit is posted.
--
-- No ON DELETE behaviour is declared because the table it references is immutable: an issued
-- credit note cannot be deleted (0012's trigger), so there is no cascade to choose.
ALTER TABLE service_upgrades
    ADD COLUMN credit_note_id   uuid REFERENCES credit_notes (id),
    ADD COLUMN credit_posted_at timestamptz;

COMMENT ON COLUMN service_upgrades.credit_note_id IS
    'The credit note that returned the unused part of the period for a downgrade. Null until '
    'internal/credit posts it; set in the same transaction as the ledger entry, so its presence '
    'is evidence the customer was credited rather than a plan to do so.';
COMMENT ON COLUMN service_upgrades.credit_posted_at IS
    'When the downgrade credit was posted. The pair with credit_note_id is what makes posting '
    'idempotent without a second idempotency table.';

-- A row's direction and its document must agree. An upgrade that somehow carried a credit note
-- would be a charge and a refund for the same change, which is how a customer gets paid twice;
-- the constraint refuses it at the row rather than trusting every future writer.
ALTER TABLE service_upgrades
    ADD CONSTRAINT service_upgrades_credit_direction CHECK (
        credit_note_id IS NULL OR prorated_minor < 0
    );

-- The queue a worker scans is "pending downgrades with no credit yet". The existing partial index
-- is on status alone and serves the paid-upgrade scan; this one narrows to the rows that need a
-- posting, so the scan does not read every pending upgrade to find the few downgrades.
CREATE INDEX service_upgrades_pending_downgrade_idx
    ON service_upgrades (created_at)
    WHERE status = 'pending' AND prorated_minor < 0 AND credit_note_id IS NULL;

COMMIT;

-- ============================ a plan change's chosen config options (UPG-02) ===
--
-- A plan change is not only a base price. What a hosting customer actually buys is "this plan,
-- with 8 GB": the renewal engine bills the base price *plus* one line per chosen option, so a
-- change that moves the plan and leaves the options behind charges the wrong number on the very
-- next period. The two halves therefore have to be one priced change, and this table is where the
-- option half waits.
--
-- # Why the options cannot be written to service_configs immediately
--
-- `service_configs` is what the *current* subscription is priced from. An upgrade is applied when
-- its invoice is paid (UPG-05), and until then the service is still on the old price version.
-- Writing the new plan's options early would make the next renewal price the target plan's
-- options against the *old* price version: at best a mispriced period, at worst a selection the
-- catalogue cannot price at all, which the renewal engine correctly refuses and flags as needing
-- review. Either way the customer is billed for something they do not have yet.
--
-- So the selection waits here, keyed by the upgrade row, and `service.Store.apply` copies it into
-- `service_configs` in the same transaction that moves the subscription. One commit: the price and
-- the options that justify it change together or not at all.
--
-- # Why amount_minor is stored
--
-- The same reason `service_configs` stores it: it is the evidence of what the option contributed
-- when it was chosen, so a later catalogue edit cannot silently re-price a change the customer
-- already agreed to. The renewal engine deliberately re-prices from `config_option_prices`; this
-- column is the audit trail, not the billing input.
--
-- `config_option_id` is a plain foreign key with no cascade: an option a pending change depends on
-- must not be deleted underneath it, and the existing delete path already refuses an option a live
-- service has selected.
CREATE TABLE service_upgrade_configs (
    upgrade_id       uuid NOT NULL REFERENCES service_upgrades (id) ON DELETE CASCADE,
    config_option_id uuid NOT NULL REFERENCES config_options (id),
    value            text NOT NULL,
    amount_minor     bigint NOT NULL DEFAULT 0 CHECK (amount_minor >= 0),
    setup_minor      bigint NOT NULL DEFAULT 0 CHECK (setup_minor >= 0),
    PRIMARY KEY (upgrade_id, config_option_id)
);

COMMENT ON TABLE service_upgrade_configs IS
    'The config-option selection a plan change carries (UPG-02). Written when the change is '
    'requested, copied into service_configs in the transaction that applies the change, deleted '
    'when the upgrade is cancelled. It exists so a plan and its options change together rather '
    'than one period apart.';

CREATE INDEX service_upgrade_configs_option_idx ON service_upgrade_configs (config_option_id);
