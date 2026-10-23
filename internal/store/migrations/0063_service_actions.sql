-- 0063_service_actions.sql
--
-- The three things an operator could not do to a service, and the two facts the queue
-- could not carry (SVC-12, SVC-14, SVC-22, CT-12).
--
-- # Why the queue needs a payload
--
-- `service_operations` identifies an operation by its key and its kind, and nothing else.
-- That is sufficient for create/suspend/resume/terminate: each one's instruction is "the
-- thing this service already is". A *resize* is not like that — it says "this service should
-- now be this plan" — and the plan is not derivable from the service row, because the
-- subscription may have been repriced again between the enqueue and the claim. Deriving it
-- at claim time would provision the customer's server to whatever the *latest* upgrade said
-- and mark an older row done, which is a wrong size silently recorded as success.
--
-- So the instruction travels with the row: `payload` is written by the enqueuer and read by
-- the worker, and it is the operation's own immutable intent, exactly as `generation` is its
-- fencing token. It defaults to `{}` so every existing row — and every caller that has no
-- instruction beyond the kind — is unchanged.
--
-- # Why two more kinds, rather than reusing an existing one
--
-- A kind is what the worker dispatches on, and these two do different things:
--
--   delete_remote      destroys the provider-side resource and forgets its id, but leaves the
--                      service row and its billing relationship alone. That is the operator
--                      action "the server is gone from the panel, the customer keeps the
--                      service" — a *terminate* would also end the service, and the two must
--                      not be one operation with a flag, because a retried terminate that ran
--                      the delete_remote branch would leave a paid service marked active with
--                      no server.
--   cancel_subscription ends a provider-side subscription id (a legacy external billing
--                      agreement). It is a call to the payment gateway, not to the
--                      provisioner, so it cannot be any of the existing kinds.
--
-- # Why the external subscription id is a column on the service
--
-- The local `subscriptions` row is this panel's own billing schedule. A legacy or
-- gateway-side subscription is somebody else's record with somebody else's identifier, and
-- cancelling it is an external action that can fail, so it needs to be a fact the panel
-- stores and can retry rather than a field a screen echoes back. It is NOT NULL DEFAULT ''
-- because every existing row has no such id, and because a nullable column would make "never
-- set" and "cleared" indistinguishable in the audit trail.
--
-- Nothing here rewrites an issued invoice, a journal entry or an audit row, and no table is
-- added.

BEGIN;

-- ---------------------------------------------------------------- queue payload --

ALTER TABLE service_operations
    ADD COLUMN payload jsonb NOT NULL DEFAULT '{}'::jsonb;

-- An instruction is an object. A bare array or scalar would be a value with no name, and the
-- worker reads it by key.
ALTER TABLE service_operations
    ADD CONSTRAINT service_operations_payload_object
        CHECK (jsonb_typeof(payload) = 'object');

-- --------------------------------------------------------------- operation kinds --

-- The set is widened, not relaxed: the CHECK still enumerates every kind, so a typo in a
-- kind is still refused at INSERT rather than parked in the queue as an unsupported
-- operation the worker reports as failed.
ALTER TABLE service_operations
    DROP CONSTRAINT service_operations_kind_check;

ALTER TABLE service_operations
    ADD CONSTRAINT service_operations_kind_check
    CHECK (kind IN ('create', 'inspect', 'suspend', 'resume', 'terminate', 'resize',
                    'delete_remote', 'cancel_subscription'));

-- ------------------------------------------------------ external subscription id --

ALTER TABLE services
    ADD COLUMN external_subscription_id text NOT NULL DEFAULT '';

-- The id is printed in an operator's audit trail and sent to a gateway, so leading or
-- trailing whitespace is a defect rather than a preference: a padded id matches nothing at
-- the provider and looks identical on screen.
ALTER TABLE services
    ADD CONSTRAINT services_external_subscription_id_trimmed
        CHECK (external_subscription_id = btrim(external_subscription_id)),
    ADD CONSTRAINT services_external_subscription_id_bounded
        CHECK (char_length(external_subscription_id) <= 200);

-- "Which service owns this gateway subscription" is asked when a gateway reports that a
-- subscription was cancelled at the provider, so the lookup must not scan every service.
CREATE INDEX services_external_subscription_idx
    ON services (external_subscription_id)
    WHERE external_subscription_id <> '';

COMMIT;
