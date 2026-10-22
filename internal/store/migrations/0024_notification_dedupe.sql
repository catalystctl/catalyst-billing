-- 0024_notification_dedupe.sql
--
-- Make the in-app inbox idempotent, the way the mail outbox already is.
--
-- `notification_deliveries` has had a nullable `dedupe_key` with a UNIQUE index since 0014, so a
-- retried webhook enqueues one message. The inbox added in 0020 has no equivalent, and the gap
-- is not theoretical: providers retry webhooks as a matter of course, so a retried
-- `invoice_paid` would write a second unread badge for one event — and the two channels would
-- then disagree, one mail and two notifications, which is the kind of inconsistency that makes
-- an operator distrust the whole notification system.
--
-- A natural key from the content does not work. `new_login_detected` renders the same URL
-- (`/account/security`) for every login, so keying on the URL would collapse two genuine logins
-- into one notice — turning an idempotency fix into a missed security alert. The key has to be
-- derived from the *event instance* (the payment id, the ticket message id, the auth event id),
-- which is exactly what the outbox's `dedupe_key` is.
--
-- Nullable, with a partial unique index, for the same reason the outbox's is: a notice with no
-- natural instance identity (an announcement, a human-triggered message) must be insertable
-- every time, and `NULL` is distinct from itself in a unique index, so those rows never collide.
--
-- With this, `INSERT ... ON CONFLICT (user_id, dedupe_key) DO NOTHING RETURNING id` is exact
-- rather than best-effort, and the caller can tell "created" from "already there" by whether a
-- row came back.

BEGIN;

ALTER TABLE notifications
    ADD COLUMN dedupe_key text;

-- The uniqueness is per user, not global: two customers notified about the same invoice each get
-- their own row, and the key is scoped to the person whose inbox it is.
CREATE UNIQUE INDEX notifications_dedupe_idx
    ON notifications (user_id, dedupe_key)
    WHERE dedupe_key IS NOT NULL;

COMMIT;
