-- One heartbeat row per worker INSTANCE, not one for the fleet.
--
-- `worker_heartbeat` was a singleton keyed on the constant id 'worker', so two workers overwrote
-- each other's row on every tick. The panel could therefore show "a worker is alive" and could
-- never show "there are two workers running", which is the misconfiguration that actually matters
-- here: `BILLING_EMBEDDED_WORKER=1` plus a separate `billing worker` is an easy way to get one, and
-- the first symptom of it was a burst of `23505` failures in the charge sweep.
--
-- The primary key is already `text`, so nothing structural changes: the id becomes a per-process
-- instance id and the row count becomes the worker count.
--
-- The legacy row is removed rather than left to age out. It cannot beat again — no code writes that
-- id any more — so keeping it would make every installation report one permanently dead instance
-- for a day, which is a false alarm on the exact screen this change exists to make trustworthy.
-- A worker still running the old version will re-create it on its next tick and be visible as an
-- instance until the prune removes it, which is the honest answer during a rolling upgrade.
DELETE FROM worker_heartbeat WHERE id = 'worker';

-- The reader shows instances that beat recently and the writer prunes the rest, so both are range
-- queries on the beat time. Without this the prune is a sequential scan of a table that is small
-- today and is scanned every tick forever.
CREATE INDEX IF NOT EXISTS worker_heartbeat_beat_at_idx ON worker_heartbeat (beat_at);