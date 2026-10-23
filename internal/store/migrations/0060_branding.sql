-- 0060_branding.sql
--
-- The tenant's brand overlay: a display name, a logo and an accent colour per colour mode
-- (E6.10).
--
-- # What this is, and what it deliberately is not
--
-- It is one brand per installation, made of four values, layered on top of the two colour
-- themes the panel already has. It is *not* a theme registry: there is no `themes` table,
-- no token set per theme, no per-user selection and no preview surface. A registry is a
-- larger feature with its own schema (every design token becomes a row or a column), its own
-- migration story (a new token is a migration per theme) and its own failure mode (a theme
-- that is missing a token renders an unstyled control). What a tenant actually needs in
-- order to look like themselves is a name, a logo and a colour that reach the panel and the
-- invoice, in both modes — so that is what exists.
--
-- # Why no new table
--
-- The values are configuration, and `settings` is the configuration table: one row per
-- (group_key, key), typed and defaulted in code by internal/settings, audited on every write
-- by the same auditor the money paths use. A `branding` table would be a second, parallel
-- configuration mechanism with its own read path, its own audit story and its own page — for
-- four values. The group is `branding`; the keys are declared in internal/settings.
--
-- # Why the database still gains something
--
-- The code declares the rules (internal/settings `checkHexColourOrEmpty` and
-- `checkUUIDOrEmpty`), and this migration records the same rules in the schema, because a
-- row written by hand through psql must not be able to create a value the renderers cannot
-- draw. Two consequences matter:
--
--   * A colour is stored as `#rrggbb` or not at all. The empty string means "not configured"
--     and is what makes an installation that never opened the branding screen byte-identical
--     to one built before the screen existed — so the constraint has to permit it.
--   * The logo is a reference to `product_images`, the same digest-deduped store the catalog
--     uses for product logos. Not a foreign key: a brand must not be destroyed by deleting a
--     catalog image, and `settings.value` is text by design. Instead the read path treats a
--     dangling reference as "no logo", which is the same fallback as unset — a logo that
--     cannot be served must not break the panel.
--
-- # Why the accents are normalised, and why they are lower case
--
-- The panel compares the stored value against the built-in token to decide whether to emit
-- an override at all, and the invoice compares it against nothing. Two spellings of the same
-- colour ("#3B5BDB" and "#3b5bdb") would make the first comparison lie. Normalising on write
-- means the comparison is a string equality and there is exactly one spelling of a colour in
-- the database.
--
-- # Why the two accents are separate keys
--
-- The panel has two themes, and they are not the same colour space. The built-in accent is
-- #3b5bdb on white and #7b95ff on near-black: the same intent at two luminances, because a
-- mid-tone blue that reads on white is too dark on #0a0d13 and the pale version of it is
-- invisible on white. One stored accent would be legible in one theme and washed out in the
-- other, so there is one per mode and either may be empty.

BEGIN;

-- The colour constraint is a CHECK rather than a trigger so that a violation is reported
-- against the row that caused it, with the value in the message. `~` is a full match here
-- because the pattern is anchored explicitly.
ALTER TABLE settings
    ADD CONSTRAINT settings_branding_accent_is_hex CHECK (
        (group_key, key) NOT IN (
            ('branding', 'accent_light'),
            ('branding', 'accent_dark')
        )
        OR value = ''
        OR value ~ '^#[0-9a-f]{6}$'
    );

-- The logo is a uuid or empty. `portal.ValidID` accepts upper-case hex, so the constraint
-- does too; the read path lower-cases nothing because the column it references is a uuid.
ALTER TABLE settings
    ADD CONSTRAINT settings_branding_logo_is_uuid CHECK (
        (group_key, key) <> ('branding', 'logo_id')
        OR value = ''
        OR value ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
    );

-- A display name is a heading, not a paragraph. The limit is the same shape as the panel's
-- brand line: a name long enough to wrap the sidebar is a data-entry mistake, and letting it
-- run would push the navigation off the screen rather than truncate visibly.
ALTER TABLE settings
    ADD CONSTRAINT settings_branding_name_is_short CHECK (
        (group_key, key) <> ('branding', 'name')
        OR length(value) <= 80
    );

-- Normalise an accent written by hand: the leading "#", lower case, and the three-digit form
-- expanded. The application always writes the six-digit lower-case form (brand.Colour.Hex),
-- so this only ever fires for a psql write — which is exactly the case the constraint exists
-- for. It runs BEFORE the CHECK, so an operator who types `#ABC` or `3b5bdb` gets the
-- canonical value stored rather than a constraint violation for a colour they clearly meant.
CREATE OR REPLACE FUNCTION settings_normalise_branding_accent() RETURNS trigger AS $$
BEGIN
    IF NEW.group_key = 'branding' AND NEW.key IN ('accent_light', 'accent_dark') THEN
        NEW.value := lower(btrim(NEW.value));
        IF NEW.value <> '' AND left(NEW.value, 1) <> '#' THEN
            NEW.value := '#' || NEW.value;
        END IF;
        IF NEW.value ~ '^#[0-9a-f]{3}$' THEN
            NEW.value := '#' || substr(NEW.value, 2, 1) || substr(NEW.value, 2, 1)
                              || substr(NEW.value, 3, 1) || substr(NEW.value, 3, 1)
                              || substr(NEW.value, 4, 1) || substr(NEW.value, 4, 1);
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER settings_branding_normalise_accent
    BEFORE INSERT OR UPDATE ON settings
    FOR EACH ROW
    EXECUTE FUNCTION settings_normalise_branding_accent();

COMMENT ON TABLE settings IS
    'One row per (group_key, key). A secret is redacted on every read path; is_secret is the '
    'column every generic reader consults. Integration precedence (0035): for gateway.* and '
    'servers.* the extension''s own environment variable wins over the saved row, so a value '
    'read as source=environment is not editable from the panel. Branding (0060): the '
    'branding.* group is the tenant''s overlay — name, logo_id and one accent per colour '
    'mode; the empty string means "not configured", which is the fallback that leaves the '
    'built-in appearance untouched.';

COMMIT;
