-- 0069_session_binding.sql
--
-- Session binding: a session may be judged against the address and the user agent it was
-- signed in from (IDN-16).
--
-- Why this is three columns and not a validation performed on the fly: what a session is
-- bound to is decided at sign-in, and it has to stay decided. If the check read the current
-- policy, flipping the setting would retroactively re-judge every session already open — a
-- staff member who signed in from the office would be signed out mid-shift by an unrelated
-- settings change, and the operator would have no way to tell which of their two toggles
-- did it. So the mode is *recorded on the row* at creation and the request path enforces
-- what was recorded. `bound_mode` empty means "this session was created while binding was
-- off", which is every session that exists before this migration and every one created
-- after it on an installation that leaves the policy off.
--
-- `bound_ip` and `bound_user_agent` are the values as they were at sign-in, not copies of
-- the display columns `ip` and `user_agent`: those two are "where this session was last
-- seen from" for the operator's device list, and a session that moves (a laptop that
-- reconnects on a different network) should still show where it is now. A binding that
-- silently followed the session would enforce nothing.
--
-- The columns are nullable/defaulted so this migration rewrites no row: an existing
-- session keeps working under the policy it was created under, which is the only answer
-- that does not sign an installation out when it deploys.
--
-- The trade-off, recorded here because it is a product decision and not an implementation
-- detail: binding to an IP is a real control against a stolen cookie and a real usability
-- problem against a mobile network that changes address mid-session. It is therefore a
-- policy setting with three modes (`off`, `user_agent`, `ip_and_user_agent`) and a scope
-- (`all`, `staff`, `customers`) rather than a fixed behaviour — an installation that turns
-- it on chooses which sessions pay the price. `internal/settings/group_session.go`
-- declares both keys and defaults them to `off`, because a security control that signs
-- people out on upgrade is one they disable.

BEGIN;

ALTER TABLE sessions
    ADD COLUMN IF NOT EXISTS bound_ip inet,
    ADD COLUMN IF NOT EXISTS bound_user_agent text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS bound_mode text NOT NULL DEFAULT '';

-- The empty string is the historical, unbound state and is legal on purpose: a row written
-- before this migration, or while the policy is off, has nothing to compare. Any other
-- value must be one the enforcement path understands, so a hand-written row cannot claim a
-- mode that means "skip the check".
ALTER TABLE sessions
    ADD CONSTRAINT sessions_bound_mode_check
    CHECK (bound_mode IN ('', 'user_agent', 'ip_and_user_agent'));

COMMENT ON COLUMN sessions.bound_mode IS
    'The binding policy in force when this session was issued: '', user_agent or ip_and_user_agent. Empty means the session is unbound.';
COMMENT ON COLUMN sessions.bound_ip IS
    'The client address recorded at sign-in when bound_mode is ip_and_user_agent. Distinct from ip, which follows the session.';
COMMENT ON COLUMN sessions.bound_user_agent IS
    'The user agent recorded at sign-in when bound_mode is user_agent or ip_and_user_agent.';

-- ------------------------------------------------------------------ problem code --

-- A refused binding is a refusal with a code, not a silent redirect to the sign-in screen:
-- the client can tell "your session is from somewhere else" apart from "you are signed out"
-- and say so. The code is the translatable unit, as with every other problem code.
INSERT INTO message_catalog (locale_code, key, text) VALUES
    ('en', 'error.session_binding_mismatch',
     'This session was signed in from a different address or browser. Sign in again.')
ON CONFLICT (locale_code, key) DO NOTHING;

COMMIT;
