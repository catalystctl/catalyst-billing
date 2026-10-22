-- 0035_integration_config.sql
--
-- The gateway and provisioning-server configuration, and the precedence decision behind it
-- (E4.2 ADM-10, E4.5 ADM-11).
--
-- # The decision
--
-- The panel can now store a gateway's credentials and a provisioner's panel URL and API key,
-- and the runtime reads them. The question this migration records is: what happens when the
-- environment and the panel disagree?
--
-- **The environment wins, per value.** For every credential a provider needs, the effective
-- value is resolved in this order:
--
--   1. the environment variable the extension itself reads (STRIPE_SECRET_KEY,
--      PTERODACTYL_PANEL_URL, …), when it is set and non-empty;
--   2. the row saved in `settings` by the panel (`gateway.secret_key`,
--      `servers.panel_url`, …);
--   3. nothing, and the extension's own validation decides whether that is fatal.
--
-- The reason is that a container deployment declares its credentials in its environment,
-- and that deployment must keep working across the upgrade that introduced the screen. The
-- environment is also the value an operator can see and rotate without a database write. The
-- failure mode of the other order is the worst one available: the panel would display one
-- key while the process used another, and every diagnostic would agree with the panel.
--
-- The provider name follows the same rule, with one addition: `BILLING_GATEWAY_PROVIDER`
-- naming a provider also enables the gateway, because a deployment that names a provider is
-- asking for payments to work. The provisioner has no equivalent variable — a deployment
-- names its provisioner by the binary `BILLING_PROVISIONER_EXTENSION` points at — so
-- `servers.provider` is that provider's only source.
--
-- The read path reports, per field, whether the value came from `environment`, `panel` or
-- nothing, and the screen makes an environment-supplied field read-only. A form that accepts
-- a value the runtime will ignore is worse than no form: the operator saves it, the screen
-- says "saved", and nothing changes.
--
-- # Why the schema changes at all
--
-- The keys already exist (internal/settings declares them; 0020 created the table), so no
-- column is added. What the database gains is the part of the decision it can enforce
-- without trusting the application:
--
--   * A credential in these groups is marked `is_secret` in the row, not only in the code
--     that defines the key. `settings.is_secret` is what the audit trail and every generic
--     read path consult, and a row written by hand through psql must not be able to create a
--     credential the panel would describe as ordinary text. The settings package redacts by
--     definition as well, so this is defence in depth rather than the only guard.
--
-- The comment on the table records the precedence where a psql session will see it, because
-- the next person to debug "the panel shows a different key than Stripe receives" will be
-- looking at the table, not at internal/gatewayconfig/doc.go.

BEGIN;

-- A credential row is a credential in the database too.
--
-- The three keys are exactly the secret fields internal/settings declares for these two
-- groups: a new secret key is a code change (and a change to this constraint), which is the
-- review this invariant is worth.
ALTER TABLE settings
    ADD CONSTRAINT settings_integration_secret_flagged CHECK (
        (group_key, key) NOT IN (
            ('gateway', 'secret_key'),
            ('gateway', 'webhook_secret'),
            ('servers', 'api_key')
        )
        OR is_secret
    );

COMMENT ON TABLE settings IS
    'One row per (group_key, key). A secret is redacted on every read path; is_secret is the '
    'column every generic reader consults. Integration precedence (0035): for gateway.* and '
    'servers.* the extension''s own environment variable wins over the saved row, so a value '
    'read as source=environment is not editable from the panel.';

COMMIT;
