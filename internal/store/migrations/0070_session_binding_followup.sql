-- 0070_session_binding_followup.sql
--
-- Two follow-ups to 0069. They are a new file rather than an edit because 0069 has been
-- applied: an applied migration is immutable, and the runner records its checksum to enforce
-- exactly that. A correction is a new statement in a new file.
--
-- 1. `carts.abandoned_at` is the marker that makes `cart.abandoned` (CT-13) a fact emitted
--    once instead of a guess repeated on every pass. Without it the abandonment rule would
--    re-announce the same cart on every tick, which floods an extension's queue and trains
--    whoever reads it to ignore the event. It is a timestamp rather than a boolean because
--    "when did we give up on this cart" is the question a merchant asks, and because the
--    marker is cleared when the customer touches the cart again — so a customer who comes
--    back and leaves again is announced twice, which is correct.
--
-- 2. The partial index keeps the scan proportional to live carts rather than to every cart
--    ever created.

BEGIN;

ALTER TABLE carts ADD COLUMN IF NOT EXISTS abandoned_at timestamptz;

COMMENT ON COLUMN carts.abandoned_at IS
    'When the cart was announced as abandoned. NULL means it is still live; it is cleared when the customer touches the cart again.';

CREATE INDEX IF NOT EXISTS carts_live_idx ON carts (updated_at)
    WHERE abandoned_at IS NULL;

COMMIT;
