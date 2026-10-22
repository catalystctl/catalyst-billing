-- 0037_i18n.sql
--
-- Localization: a locale is a row, a message is a row, and the language a customer
-- chose is a column on their account.
--
-- Paymenter ships 24 language files in the source tree, which means a translation is a
-- pull request, a release and a deploy. Four decisions here move that to data, and each
-- is recorded because the next person will otherwise "simplify" it back into a defect:
--
-- 1. **A locale is configuration, like a currency.** `locales` carries the English name
--    (what an operator reads in a list), the native name (what a customer reads in the
--    switcher), whether it is offered and whether it is the default. Exactly one default,
--    enforced by a partial unique index rather than by a service check — the same shape
--    `currencies_one_default` already uses, because "two defaults" is a state no code path
--    should be able to reach. A `CHECK` additionally requires the default to be enabled:
--    a default that is not offered is a configuration that resolves every request to a
--    language the switcher does not show.
--
-- 2. **The catalog is (locale, key, text), and a blank translation is refused.** The
--    fallback order — the locale's text, then the default locale's text, then the key
--    itself — lives in Go (`internal/i18n`), not in SQL, because it has to answer for a
--    key that has no row anywhere. What the database can refuse is the one outcome that
--    must never be stored: `btrim(text) <> ''`. A blank label renders as an empty button,
--    which a customer reads as a design choice and a developer reads as a bug three
--    releases later.
--
-- 3. **The customer's language is a column on `users`, not a settings row.** Resolution is
--    per request — explicit preference, then `Accept-Language`, then the default — and the
--    first step has to be one fact about one account. A key/value settings table would make
--    it a join and an untyped string; a column makes it a typed, cascading reference that
--    the session query can already see. It is nullable on purpose: NULL means "this account
--    never chose", which is what lets the header win for a visitor who has not.
--
-- 4. **Mail translations live in the same catalog, under a `mail.` prefix.** A transactional
--    mail is plain text (`internal/notify` explains why), so its translatable unit is a
--    sentence, and a value substituted into it (a reset link, an invoice number) is data
--    rather than language. A catalog row for a mail sentence therefore uses `{{name}}` for
--    the value and holds the rest as prose. That keeps one place to translate and one
--    fallback rule: a sentence with no translation is sent in the default locale, which is
--    exactly the mail an installation without any translations sends today.
--
-- Nothing here rewrites an issued document, an audit row or a ledger entry. A locale is
-- presentation, like a currency's symbol: it can change how a page reads and never what a
-- number means.

BEGIN;

-- ================================================================== locales ==
--
-- The code is a BCP 47 language tag, stored in the canonical case (language lower-case,
-- region upper-case) so `pt-br` and `pt-BR` cannot become two rows for one language. The
-- pattern is deliberately permissive about the subtag count — `zh-Hans`, `pt-BR` and
-- `sr-Latn-RS` are all real — and strict about the shape, because this value travels in a
-- URL path and in an `Accept-Language` header.

CREATE TABLE locales (
    code        text PRIMARY KEY
                CHECK (code ~ '^[a-z]{2,3}(-[A-Za-z0-9]{2,8})*$'),
    -- The English name, for an operator's list: "German", not "Deutsch".
    name        text NOT NULL CHECK (btrim(name) <> ''),
    -- The name in the language itself, for the customer's switcher: "Deutsch".
    native_name text NOT NULL CHECK (btrim(native_name) <> ''),
    -- A disabled locale keeps every translation and every customer preference that points
    -- at it, but is not offered and never resolves. Deleting the row instead would take
    -- the translated text with it (see the cascade on message_catalog).
    enabled     boolean NOT NULL DEFAULT false,
    -- Exactly one default, by partial unique index below. The default is what a request
    -- with no preference and no usable header resolves to.
    is_default  boolean NOT NULL DEFAULT false,
    sort_order  int     NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    -- A default that is not offered would resolve every request to a language the
    -- switcher cannot show. The index makes "two defaults" impossible; this makes
    -- "an invisible default" impossible.
    CONSTRAINT locales_default_is_enabled CHECK (NOT is_default OR enabled)
);

CREATE UNIQUE INDEX locales_one_default ON locales (is_default) WHERE is_default;
CREATE INDEX locales_offered_idx ON locales (enabled, sort_order, code);

-- =========================================================== message_catalog ==
--
-- One row per (locale, key). The key is dotted and lower-case — `storefront.hero.title`,
-- `mail.password_reset.subject` — so a screen's keys sort together and a prefix can be
-- selected without a second table.
--
-- `text` is the translation for `locale_code` *only*: the fallback to the default locale is
-- a read-time merge in Go, because storing a copy of the English text in every locale would
-- turn "untranslated" into "translated to the same words" and make coverage unknowable.

CREATE TABLE message_catalog (
    locale_code text NOT NULL REFERENCES locales (code) ON DELETE CASCADE,
    key         text NOT NULL
                CHECK (key ~ '^[a-z0-9][a-z0-9_.:-]{0,199}$'),
    -- Never blank. See decision 2 in the header.
    text        text NOT NULL CHECK (btrim(text) <> ''),
    -- Who last changed the wording. A translation is prose an operator edits, and "who
    -- wrote this sentence" is the first question when a customer reports a bad one.
    updated_by  uuid REFERENCES users (id) ON DELETE SET NULL,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (locale_code, key)
);

-- The editor lists one locale's rows ordered by key, and the mail translator reads every
-- `mail.%` row for a locale in one pass. Both are prefix scans on the primary key, but the
-- key index makes the search across locales (a key, in every language) cheap.
CREATE INDEX message_catalog_key_idx ON message_catalog (key);

-- ======================================================== customer preference ==
--
-- Nullable: NULL means "never chose", which is what lets the Accept-Language header decide
-- for a visitor who has not. ON DELETE SET NULL rather than CASCADE: removing a language
-- from the installation must not remove the customer.

ALTER TABLE users
    ADD COLUMN locale text REFERENCES locales (code) ON DELETE SET NULL;

-- ==================================================================== seeds ==
--
-- `en` is the default and the only locale a fresh installation offers, matching
-- `i18n.default_locale`. `de` ships *disabled* with a full set of translations: it is the
-- worked example that makes "enable a language" one click instead of a promise, and it is
-- disabled so that installing the panel does not silently change what a shop's storefront
-- speaks. An operator who wants German turns it on in /locales; one who wants French adds a
-- row and translates there.

INSERT INTO locales (code, name, native_name, enabled, is_default, sort_order) VALUES
    ('en', 'English', 'English', true,  true,  10),
    ('de', 'German',  'Deutsch', false, false, 20)
ON CONFLICT (code) DO NOTHING;

-- The storefront and the locales screen. The English text here is the source of truth for
-- these keys: a view passes the same sentence as its inline fallback, so a key that is
-- missing from the catalog still renders words rather than `storefront.hero.title`.
INSERT INTO message_catalog (locale_code, key, text) VALUES
    -- Storefront navigation and hero.
    ('en', 'storefront.nav.products',      'Products'),
    ('en', 'storefront.nav.signin',        'Sign in'),
    ('en', 'storefront.nav.client_area',   'Client area'),
    ('en', 'storefront.hero.kicker',       'Infrastructure, billed honestly'),
    ('en', 'storefront.hero.title',        'Servers that come with a ledger.'),
    ('en', 'storefront.hero.body',         'Pick a plan, pay in a currency you recognise, and watch the invoice and the server agree. No surprise renewals, no drifted totals.'),
    ('en', 'storefront.hero.plans',        'See the plans'),
    ('en', 'storefront.language.label',    'Language'),
    -- Category filter and section heading.
    ('en', 'storefront.categories.label',  'Categories'),
    ('en', 'storefront.categories.all',    'All products'),
    ('en', 'storefront.section.all',       'All products'),
    ('en', 'storefront.section.sub',       'Every plan we sell, with its starting price.'),
    -- Empty states.
    ('en', 'storefront.empty.catalog_title',  'The catalog is empty'),
    ('en', 'storefront.empty.catalog_body',   'No product is published yet. An operator publishes one from the catalog screen.'),
    ('en', 'storefront.empty.category_title', 'Nothing in this category yet'),
    ('en', 'storefront.empty.category_body',  'Every product in this category is either unpublished or has no price. Try another category.'),
    ('en', 'storefront.empty.show_all',       'Show everything'),
    -- Product card and footer.
    ('en', 'storefront.card.product',           'Product'),
    ('en', 'storefront.card.no_description',    'No description yet.'),
    ('en', 'storefront.card.from',              'from'),
    ('en', 'storefront.card.pricing_on_request','Pricing on request'),
    ('en', 'storefront.footer.client_area',     'Client area'),
    ('en', 'storefront.footer.catalog_api',     'Catalog API'),
    ('en', 'storefront.footer.status',          'Status'),
    -- The locales screen's own chrome. A screen about languages that only speaks one is
    -- the first thing an operator would notice.
    ('en', 'locales.title',            'Locales'),
    ('en', 'locales.subtitle',         'Which languages the panel and the storefront offer, and what each message says in them.'),
    ('en', 'locales.new',              'New locale'),
    ('en', 'locales.banner.title',     'A missing translation falls back'),
    ('en', 'locales.banner.body',      'A key with no row for a locale renders the default locale''s text, and then the key itself. It is never blank: a blank label looks like a design choice.'),
    ('en', 'locales.col.language',     'Language'),
    ('en', 'locales.col.coverage',     'Translated'),
    ('en', 'locales.action.set_default','Set default'),
    ('en', 'locales.action.show',      'Show'),
    ('en', 'locales.action.hide',      'Hide'),
    ('en', 'locales.action.translate', 'Translate'),
    -- Transactional mail. The sentence is the key's text; `{{name}}` stands for a value
    -- that is data rather than language, and the sender substitutes it after matching.
    ('en', 'mail.password_reset.subject',   'Reset your password'),
    ('en', 'mail.password_reset.line.1',    'Someone asked to reset the password for this account.'),
    ('en', 'mail.password_reset.line.2',    'Open this link to choose a new password:'),
    ('en', 'mail.password_reset.line.3',    'The link expires in {{value}} and can be used once.'),
    ('en', 'mail.password_reset.line.4',    'If you did not ask for this, ignore this message; the password is unchanged.'),
    ('en', 'mail.email_verification.subject','Confirm your email address'),
    ('en', 'mail.email_verification.line.1', 'Confirm this address to finish setting up the account.'),
    ('en', 'mail.email_verification.line.2', 'The link expires in {{value}} and can be used once.')
ON CONFLICT (locale_code, key) DO NOTHING;

INSERT INTO message_catalog (locale_code, key, text) VALUES
    ('de', 'storefront.nav.products',      'Produkte'),
    ('de', 'storefront.nav.signin',        'Anmelden'),
    ('de', 'storefront.nav.client_area',   'Kundenbereich'),
    ('de', 'storefront.hero.kicker',       'Infrastruktur, ehrlich abgerechnet'),
    ('de', 'storefront.hero.title',        'Server, die mit einem Hauptbuch kommen.'),
    ('de', 'storefront.hero.body',         'Wähle einen Tarif, zahle in einer Währung, die du kennst, und sieh zu, wie Rechnung und Server übereinstimmen. Keine überraschenden Verlängerungen, keine abweichenden Summen.'),
    ('de', 'storefront.hero.plans',        'Tarife ansehen'),
    ('de', 'storefront.language.label',    'Sprache'),
    ('de', 'storefront.categories.label',  'Kategorien'),
    ('de', 'storefront.categories.all',    'Alle Produkte'),
    ('de', 'storefront.section.all',       'Alle Produkte'),
    ('de', 'storefront.section.sub',       'Jeder Tarif, den wir verkaufen, mit seinem Startpreis.'),
    ('de', 'storefront.empty.catalog_title',  'Der Katalog ist leer'),
    ('de', 'storefront.empty.catalog_body',   'Es ist noch kein Produkt veröffentlicht. Ein Operator veröffentlicht eines im Katalog.'),
    ('de', 'storefront.empty.category_title', 'In dieser Kategorie ist noch nichts'),
    ('de', 'storefront.empty.category_body',  'Jedes Produkt dieser Kategorie ist entweder unveröffentlicht oder hat keinen Preis. Versuche eine andere Kategorie.'),
    ('de', 'storefront.empty.show_all',       'Alles anzeigen'),
    ('de', 'storefront.card.product',           'Produkt'),
    ('de', 'storefront.card.no_description',    'Noch keine Beschreibung.'),
    ('de', 'storefront.card.from',              'ab'),
    ('de', 'storefront.card.pricing_on_request','Preis auf Anfrage'),
    ('de', 'storefront.footer.client_area',     'Kundenbereich'),
    ('de', 'storefront.footer.catalog_api',     'Katalog-API'),
    ('de', 'storefront.footer.status',          'Status'),
    ('de', 'locales.title',            'Sprachen'),
    ('de', 'locales.subtitle',         'Welche Sprachen Panel und Shop anbieten und was jede Meldung darin sagt.'),
    ('de', 'locales.new',              'Neue Sprache'),
    ('de', 'locales.banner.title',     'Eine fehlende Übersetzung fällt zurück'),
    ('de', 'locales.banner.body',      'Ein Schlüssel ohne Eintrag in einer Sprache zeigt den Text der Standardsprache und dann den Schlüssel selbst. Er ist nie leer: eine leere Beschriftung sieht aus wie eine Designentscheidung.'),
    ('de', 'locales.col.language',     'Sprache'),
    ('de', 'locales.col.coverage',     'Übersetzt'),
    ('de', 'locales.action.set_default','Als Standard'),
    ('de', 'locales.action.show',      'Anzeigen'),
    ('de', 'locales.action.hide',      'Ausblenden'),
    ('de', 'locales.action.translate', 'Übersetzen'),
    ('de', 'mail.password_reset.subject',   'Passwort zurücksetzen'),
    ('de', 'mail.password_reset.line.1',    'Jemand hat darum gebeten, das Passwort für dieses Konto zurückzusetzen.'),
    ('de', 'mail.password_reset.line.2',    'Öffne diesen Link, um ein neues Passwort zu wählen:'),
    ('de', 'mail.password_reset.line.3',    'Der Link läuft in {{value}} ab und kann einmal verwendet werden.'),
    ('de', 'mail.password_reset.line.4',    'Wenn du das nicht angefordert hast, ignoriere diese Nachricht; das Passwort bleibt unverändert.'),
    ('de', 'mail.email_verification.subject','Bestätige deine E-Mail-Adresse'),
    ('de', 'mail.email_verification.line.1', 'Bestätige diese Adresse, um die Einrichtung des Kontos abzuschließen.'),
    ('de', 'mail.email_verification.line.2', 'Der Link läuft in {{value}} ab und kann einmal verwendet werden.')
ON CONFLICT (locale_code, key) DO NOTHING;

COMMIT;
