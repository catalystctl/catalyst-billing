-- 0023_notification_events.sql
--
-- Let the outbox carry every event the product emits.
--
-- `0014_account_lifecycle.sql` declared the outbox's `kind` as a closed set of four:
-- password_reset, email_verification, invoice_issued, dunning_reminder. That was correct when
-- four was the whole catalogue. It is not correct now: a paid invoice, a failed payment, a new
-- order, a provisioned server, a suspension, a termination, a cancellation request, a ticket
-- reply and a login from a new device are all events a customer is told about, and every one of
-- them would be refused at INSERT by this constraint.
--
-- The fix is not to extend the list to thirteen. A closed set that must be migrated once per
-- event is a migration per feature, and the failure mode is the worst kind: the enqueue is
-- refused inside the transaction that recorded the business change, so the change rolls back or
-- — worse, if the enqueue is best-effort — the customer is silently not told.
--
-- So the set moves to Go, as `notify.Event*` constants, and the database keeps a *shape* check.
-- That guard is deliberately modest: it rejects the mistakes that actually happen — an empty
-- kind, a space, a capital, punctuation, an unbounded value — and it does not pretend to be an
-- enumeration, because a pattern cannot distinguish `invoice_paid` from a plausible typo. The
-- real guarantee that a kind is one the code knows is the compile-time constant, and the
-- guarantee that a template exists is the template lookup, which fails loudly rather than
-- sending a blank email.
--
-- Why not a foreign key to `notification_templates(event_key)`: that would be stronger, and it is
-- what a reader expects. It is deliberately not done because the outbox must be able to record
-- an event whose template was deleted, and because the seeding order between a migration that
-- inserts templates and a code path that enqueues would become a boot-time dependency. The
-- render step fails loudly on a missing template, which is the behaviour we want: a recorded
-- event that cannot be rendered is a visible failure, not a lost one.

BEGIN;

ALTER TABLE notification_deliveries
    DROP CONSTRAINT notification_deliveries_kind_check;

-- Lowercase snake_case, 1..63 characters. This rejects the two mistakes that actually happen —
-- an empty kind and a kind with a space or a capital — without pretending to be an enumeration.
ALTER TABLE notification_deliveries
    ADD CONSTRAINT notification_deliveries_kind_shape
    CHECK (kind ~ '^[a-z][a-z0-9_]{0,62}$');

-- The drain loop asks for due work by status and time; the retention sweeper asks by age. Both
-- were already indexed in 0014 by status; this adds the age dimension the email-log viewer needs
-- to page a customer's mail history newest-first.
CREATE INDEX IF NOT EXISTS notification_deliveries_recent_idx
    ON notification_deliveries (created_at DESC);

COMMIT;
