-- 0062_billing_agreements.sql
--
-- The customer's mandate to charge a stored method without them being present (PAY-10,
-- PAY-11), and the link from a settled payment back to the method that paid it.
--
-- 0036 stores a provider token and makes one of them the default. A token alone answers
-- "which card", not "why were we allowed to use it". A year later, when a customer asks
-- why a renewal was charged, the answer has to be a row that records the permission and
-- the evidence around it: who consented, from where, under which terms, and to which
-- provider mandate. That is this table.
--
-- Two decisions are enforced here rather than in Go:
--
--   1. One active agreement per saved method. A method with two active mandates would make
--      "the agreement this charge relied on" ambiguous, and the whole point of the table is
--      that a charge has one answer.
--   2. The provider's mandate reference is unique per provider when it exists. A hosted
--      checkout that is confirmed twice must update the same agreement rather than create a
--      second one. A direct token save has no provider mandate id, and NULL is allowed to
--      repeat, which is what lets a customer save several cards that were tokenised
--      client-side.
--
-- The table deliberately stores no card number and no token: `payment_method_id` points at
-- the token's row, so there is exactly one copy of the credential and removing the method
-- removes the mandate with it.

CREATE TABLE billing_agreements (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id         uuid NOT NULL REFERENCES customers (id) ON DELETE CASCADE,
    -- Set when the hosted mandate has been confirmed and the method row exists. A pending
    -- mandate has no method yet: the customer has not finished entering the card.
    payment_method_id   uuid REFERENCES payment_methods (id) ON DELETE CASCADE,
    provider            text NOT NULL CHECK (provider ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
    -- The provider's own mandate / setup reference. Empty for a client-tokenised method,
    -- which has no separate provider mandate object.
    provider_agreement_id text CHECK (length(provider_agreement_id) <= 512),
    -- The provider's customer reference, when the provider has one. It is an identifier,
    -- never a credential; charging off-session uses it to scope the stored method.
    provider_customer_id  text CHECK (length(provider_customer_id) <= 512),
    status              text NOT NULL DEFAULT 'pending'
                            CHECK (status IN ('pending', 'active', 'cancelled', 'failed', 'expired')),
    -- When the customer gave permission. This is the fact a support agent reads first.
    consent_at          timestamptz,
    -- Where and with what the permission was given, so a later dispute has evidence rather
    -- than a boolean. Both are optional: an operator-created method may have neither.
    consent_ip          text CHECK (length(consent_ip) <= 64),
    consent_user_agent  text CHECK (length(consent_user_agent) <= 512),
    terms_version       text NOT NULL DEFAULT '' CHECK (length(terms_version) <= 80),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    cancelled_at        timestamptz,
    CONSTRAINT billing_agreements_cancelled_consistent CHECK (
        (status = 'cancelled' AND cancelled_at IS NOT NULL)
        OR (status <> 'cancelled'))
);

-- A provider mandate confirmed twice is one agreement.
CREATE UNIQUE INDEX billing_agreements_provider_ref
    ON billing_agreements (provider, provider_agreement_id)
    WHERE provider_agreement_id IS NOT NULL AND provider_agreement_id <> '';

-- One active mandate per saved method.
CREATE UNIQUE INDEX billing_agreements_one_active_per_method
    ON billing_agreements (payment_method_id)
    WHERE status = 'active' AND payment_method_id IS NOT NULL;

CREATE INDEX billing_agreements_customer_idx
    ON billing_agreements (customer_id, created_at DESC);

-- A saved method belongs to its customer's account: deleting the customer must take the
-- credential with them, not fail on a foreign key and leave a token behind. 0036 created
-- this key without a delete rule; this migration replaces it.
ALTER TABLE payment_methods
    DROP CONSTRAINT IF EXISTS payment_methods_customer_id_fkey;
ALTER TABLE payment_methods
    ADD CONSTRAINT payment_methods_customer_id_fkey
    FOREIGN KEY (customer_id) REFERENCES customers (id) ON DELETE CASCADE;

-- Which saved method paid a payment. It is display/audit data on a payment, not part of its
-- identity: the provider payment id remains the unique key, and losing the method row must
-- not delete history.
ALTER TABLE payments
    ADD COLUMN payment_method_id uuid REFERENCES payment_methods (id) ON DELETE SET NULL;

CREATE INDEX payments_method_idx
    ON payments (payment_method_id)
    WHERE payment_method_id IS NOT NULL;
