-- 0058_gateway_event_payment_id.sql
--
-- The webhook inbox stored the provider's raw delivery and nothing else, and the payment
-- processor re-derived the provider payment id by parsing that raw body for a top-level
-- `payment_id`. That works for the fixture gateway, whose body is a flat
-- `{event_id, event_type, payment_id}` document written by this repo's own tests, and it fails
-- for every real provider: a Stripe delivery nests the payment under `data.object.id` and has
-- no top-level `payment_id` at all. The first real sandbox checkout therefore produced
--
--     payments: event evt_3UTWieERFJuvmOf21CDXZJaf carries no payment id
--
-- with the delivery already acknowledged, the invoice still unpaid, and the event retried
-- forever. The processor was asking the wrong component for a fact it had already been told.
--
-- The fact is known at ingest time. `gateway.verify_webhook` is the only component that
-- understands a provider's payload shape — that is the whole reason the core delegates
-- verification to the extension — and it returns a normalised `{event_id, event_type,
-- payment_id, amount}`. `payments.Inbox.Receive` received that normalisation and threw away
-- everything except the event identity, keeping only the raw body. The core cannot re-derive
-- the id later without either provider-specific parsing (which the architecture forbids) or
-- the signature headers, which are deliberately not stored.
--
-- So the normalised id is persisted next to the raw body. Both are kept, and the difference
-- matters: `payment_id` is the core's own key, and `payload` stays the provider's untouched
-- delivery for audit and dispute evidence. The column is nullable because rows ingested before
-- this migration have no normalised id — for those, a provider resend repairs the row
-- (`Inbox.Receive` fills the normalised fields on a redelivery of an unprocessed event), and
-- until then the processor falls back to reading `payment_id` out of the body, which is
-- exactly the behaviour those rows were written under.
--
-- No index: the processor reads a row by its primary key. Nothing filters or joins on the
-- provider payment id here — `payments.provider_payment_id` is where that lookup lives, and it
-- already has the unique constraint that makes double-crediting impossible.

ALTER TABLE gateway_events ADD COLUMN payment_id text;

-- Backfill what the raw body can still tell us: deliveries in the flat shape (the fixture
-- gateway, and anything else that names the payment at the top level). A provider-nested
-- delivery is left NULL on purpose — guessing at `data.object.id` here would be exactly the
-- provider-specific parsing this migration exists to remove from the core.
UPDATE gateway_events
   SET payment_id = payload ->> 'payment_id'
 WHERE payment_id IS NULL
   AND jsonb_typeof(payload) = 'object'
   AND payload ->> 'payment_id' <> '';
