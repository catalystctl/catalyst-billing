-- 0036_payment_methods.sql
--
-- Billing agreements (PAY-10): a saved payment method, and the record of what happened
-- when one was charged without the customer present (PAY-11).
--
-- The decision this file records, because it is the one a reviewer will want to revisit:
--
--   A saved method is a *provider token* plus the display metadata a customer needs to
--   recognise the row. It is not a card. The number is entered on the provider's page and
--   never reaches this process, so there is deliberately no column a PAN could go in and
--   no code path that would fill one. That keeps the panel out of card-data scope, which
--   is why the portal screen shows "visa ending 4242" and a delete button instead of a
--   card form.
--
-- Two consequences are enforced here rather than in Go, because a second code path would
-- eventually disagree with the first:
--
--   1. Saving the same token twice replaces the row. The unique key is
--      (customer_id, provider, provider_token), and the service upserts onto it. A
--      customer who re-adds their card must not end up with two rows the pay dialog has to
--      choose between, and a renewal must never have two candidates for "the default".
--
--   2. One default per customer, by partial unique index. "The default for recurring
--      charges" is a single fact; two rows claiming it would make an off-session charge
--      ambiguous, and ambiguity in an automatic charge is how the wrong card gets billed.
--      The service clears the previous default in the same transaction, so the index is
--      never a race the caller has to handle.
--
-- The failure columns exist so a declined off-session charge is *visible* rather than
-- swallowed: the invoice stays open, dunning takes over from there, and the customer's
-- screen can say which card was refused and when.

CREATE TABLE payment_methods (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id   uuid NOT NULL REFERENCES customers (id),
    -- The gateway's own name for itself, e.g. "stripe". Scoped into the default lookup:
    -- a token from a gateway that is no longer configured cannot be charged.
    provider      text NOT NULL CHECK (provider ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
    -- The provider's reusable reference. Opaque here: the core never parses it, never
    -- logs it and never returns it to a browser (see internal/httpapi/payment_methods.go).
    provider_token text NOT NULL CHECK (length(provider_token) BETWEEN 1 AND 512),
    -- Display metadata, supplied by the provider when the method is tokenised. A brand is
    -- provider vocabulary, so it is not constrained to a closed list.
    brand         text NOT NULL DEFAULT '' CHECK (length(brand) <= 40),
    last4         text NOT NULL DEFAULT '' CHECK (last4 = '' OR last4 ~ '^[0-9]{4}$'),
    -- Expiry is optional and both halves or neither: a month without a year cannot be
    -- compared to today, and a guessed year would silently expire a working card.
    expiry_month  int CHECK (expiry_month BETWEEN 1 AND 12),
    expiry_year   int CHECK (expiry_year BETWEEN 1970 AND 9999),
    is_default    boolean NOT NULL DEFAULT false,
    -- A customer's own label ("the joint account"), so the list is recognisable when a
    -- provider gives two cards the same brand and last four.
    label         text NOT NULL DEFAULT '' CHECK (length(label) <= 80),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz,
    last_failure_at   timestamptz,
    last_failure_code text,
    CONSTRAINT payment_methods_expiry_complete CHECK (
        (expiry_month IS NULL AND expiry_year IS NULL)
        OR (expiry_month IS NOT NULL AND expiry_year IS NOT NULL)),
    -- Decision 1: the same token saved again is the same method, not a second one.
    CONSTRAINT payment_methods_token_unique UNIQUE (customer_id, provider, provider_token)
);

-- Decision 2: at most one default per customer.
CREATE UNIQUE INDEX payment_methods_one_default
    ON payment_methods (customer_id)
    WHERE is_default;

-- The list is "this customer's methods, newest first", and the default lookup is the same
-- predicate plus provider, so one index serves both.
CREATE INDEX payment_methods_customer_idx
    ON payment_methods (customer_id, created_at DESC);
