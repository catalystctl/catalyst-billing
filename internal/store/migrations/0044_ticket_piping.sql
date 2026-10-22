-- 0044_ticket_piping.sql
--
-- Email piping (TKT-11): an inbound message becomes a reply.
--
-- The model already existed — `ticket_messages.message_id` with a partial unique index,
-- and `ticket_mail_logs` for "what was sent, to whom, and whether it worked". What was
-- missing was a place to record a message that *arrived*: which ticket it was attached
-- to, which customer it was attributed to, and — the case that matters — why it was
-- refused. Three decisions shape this file.
--
-- 1. **An inbound message is logged in `ticket_mail_logs`, with its refusal reason.** The
--    table was outbound-shaped (message_id, subject, from, to, body, status, error); this
--    migration adds `direction`, `ticket_id`, `customer_id`, `attributed_by` and `reason`.
--    A message that cannot be attributed is *stored* with status `ignored` and a stable
--    reason code (`sender_mismatch`, `ambiguous_sender`, `own_address`, ...), because a
--    silently dropped reply is a customer who thinks they answered and was ignored. The
--    staff queue surfaces these rows; nothing is deleted.
--
-- 2. **Idempotence is enforced by the database, not by the ingest loop.** A partial unique
--    index on `(message_id) WHERE direction = 'inbound' AND message_id IS NOT NULL` makes
--    a second delivery of the same message — an IMAP refetch after a crash, a provider
--    retry — a no-op at the storage layer. The application also checks first, so the
--    common case is a cheap read; the index is what makes the race impossible. Outbound
--    rows are excluded: two different mails may legitimately carry one Message-ID only if
--    we generated it, and we never do.
--
-- 3. **Attachments are refused, not stored.** `ticket_attachments` exists and the portal
--    upload path uses it, but an inbound attachment is an untrusted file from an
--    unauthenticated sender: it would bypass the portal's 10 MB pre-check, its content-type
--    normalisation and its CSP-protected download route. The reply text is stored and the
--    number of parts that were dropped is recorded in `attachments_dropped`, so the support
--    agent can see that something was attached and ask the customer to upload it. Storing
--    it would be a file-upload endpoint reachable by anyone who can send an email.
--
-- `direction` defaults to 'outbound' so every row written before this migration keeps its
-- meaning. `ON DELETE SET NULL` on the two references: the log is evidence of what
-- arrived, and deleting a ticket must not delete the record that the message came in.

BEGIN;

ALTER TABLE ticket_mail_logs
    ADD COLUMN IF NOT EXISTS direction text NOT NULL DEFAULT 'outbound',
    ADD COLUMN IF NOT EXISTS ticket_id uuid REFERENCES tickets (id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS customer_id uuid REFERENCES customers (id) ON DELETE SET NULL,
    -- How the message was matched to its ticket. NULL means it was not.
    ADD COLUMN IF NOT EXISTS attributed_by text,
    -- A stable code for why a message was not attached: sender_mismatch, ambiguous_sender,
    -- own_address, auto_reply, ... Empty when the message was attached.
    ADD COLUMN IF NOT EXISTS reason text NOT NULL DEFAULT '',
    -- MIME parts (or provider attachment objects) that were refused. The reply text is kept;
    -- the files are not. Non-zero tells an agent to ask for the file through the portal.
    ADD COLUMN IF NOT EXISTS attachments_dropped int NOT NULL DEFAULT 0,
    -- Which ingest path delivered it, so an operator can tell a webhook problem from a poller
    -- problem without reading the worker's log.
    ADD COLUMN IF NOT EXISTS source text NOT NULL DEFAULT '';

ALTER TABLE ticket_mail_logs
    DROP CONSTRAINT IF EXISTS ticket_mail_logs_direction_check,
    ADD CONSTRAINT ticket_mail_logs_direction_check
        CHECK (direction IN ('inbound', 'outbound')),
    DROP CONSTRAINT IF EXISTS ticket_mail_logs_attributed_by_check,
    ADD CONSTRAINT ticket_mail_logs_attributed_by_check
        CHECK (attributed_by IS NULL
               OR attributed_by IN ('thread', 'reply_address', 'sender')),
    DROP CONSTRAINT IF EXISTS ticket_mail_logs_attachments_dropped_check,
    ADD CONSTRAINT ticket_mail_logs_attachments_dropped_check
        CHECK (attachments_dropped >= 0),
    DROP CONSTRAINT IF EXISTS ticket_mail_logs_source_check,
    ADD CONSTRAINT ticket_mail_logs_source_check
        CHECK (source IN ('', 'webhook', 'imap'));

-- One inbound delivery per Message-ID, whichever path delivers it. See decision 2.
CREATE UNIQUE INDEX IF NOT EXISTS ticket_mail_logs_inbound_message
    ON ticket_mail_logs (message_id)
    WHERE direction = 'inbound' AND message_id IS NOT NULL;

-- The staff view: newest inbound messages first, optionally only the refused ones.
CREATE INDEX IF NOT EXISTS ticket_mail_logs_inbound_recent
    ON ticket_mail_logs (direction, created_at DESC, id DESC);

CREATE INDEX IF NOT EXISTS ticket_mail_logs_unattributed
    ON ticket_mail_logs (created_at DESC)
    WHERE direction = 'inbound' AND status = 'ignored';

-- The ticket a log row was attached to is read when an agent opens the thread.
CREATE INDEX IF NOT EXISTS ticket_mail_logs_ticket
    ON ticket_mail_logs (ticket_id)
    WHERE ticket_id IS NOT NULL;

COMMIT;
