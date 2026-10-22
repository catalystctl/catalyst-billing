-- External tax-rate import: a configured source, a run history, and provenance on every rate.
--
-- The rate table (0020) was written by hand. A real installation does not type 250 jurisdictions
-- into a form, and a rate that changes in January is a fact the operator should not have to
-- remember. So a rate can now arrive from an external document instead — and the moment it can,
-- three questions have to stay answerable:
--
--   * Where did this number come from? `origin`, `source` and `fetched_at` answer it per row.
--     `origin` is not derivable from the other two, because a rate can be typed by a human while
--     a source is configured, and "which of these will an import overwrite" must be readable
--     without inferring it.
--
--   * Who typed this number? A row an operator entered is `manual`, and an import may not
--     silently replace it. That protection is the whole reason `origin` is a column rather than
--     a naming convention: a rule enforced by convention is one an import will eventually break.
--
--   * What did the last refresh actually do? `tax_import_runs` stores the counts and the full
--     per-row report, including every refusal. A rate that changed is a change to what customers
--     are charged, and "the scheduler did something at 03:00" is not an acceptable answer.
--
-- `tax_import_sources` is deliberately one row. An installation sells into jurisdictions from
-- one authority per jurisdiction, and the panel has exactly one rate table; two sources would
-- have to agree on precedence, and a precedence rule nobody can state is a rate nobody can
-- explain. The singleton is enforced by a unique index rather than by a check in code, because a
-- second row would otherwise be one INSERT away in a migration or a fixture.

-- ------------------------------------------------------- provenance on a rate --

-- `manual` is the default so every row that already exists — and every row created through the
-- existing tax screen — keeps the protection without a backfill.
ALTER TABLE tax_rates
    ADD COLUMN origin     text NOT NULL DEFAULT 'manual'
        CHECK (origin IN ('manual', 'import')),
    -- The URL the rate was fetched from, verbatim. It is text rather than a foreign key to
    -- `tax_import_sources` because the source can be repointed at a different document, and a
    -- rate's provenance must not change when it does.
    ADD COLUMN source     text NOT NULL DEFAULT '',
    -- When that fetch happened. NULL for a manual rate: an operator typed it, no document said it.
    ADD COLUMN fetched_at timestamptz;

-- "Which rates would an import replace?" is a query an operator asks from the rate screen, so it
-- gets an index rather than a scan.
CREATE INDEX tax_rates_origin_idx ON tax_rates (origin);

-- ------------------------------------------------------------- the source --

CREATE TABLE tax_import_sources (
    -- One source per installation, enforced by the primary key rather than by a check in code.
    -- A second row would otherwise be one INSERT away in a migration or a fixture, and two
    -- sources would need a precedence rule nobody can state.
    scope   text PRIMARY KEY DEFAULT 'installation' CHECK (scope = 'installation'),
    -- The document. http and https only: file:// and data: are not sources an operator can audit,
    -- and a fetch from the panel's own filesystem is a local-file read dressed up as config.
    url     text NOT NULL CHECK (url ~ '^https?://[^[:space:]]+$'),
    -- `json` or `csv`. The format is declared rather than sniffed: a document that is both a
    -- valid CSV and a valid JSON object is not a thing, but a *guess* that picks the wrong one
    -- imports zero rows and reports success, which is worse than refusing.
    format  text NOT NULL CHECK (format IN ('json', 'csv')),
    -- A source can be configured and switched off. Off means "do not fetch", not "forget it".
    enabled boolean NOT NULL DEFAULT true,
    -- How often the worker should re-fetch. The tick is fixed (see cmd/billing/jobs.go); this is
    -- the installation's answer to "how stale may a rate be", which is a business decision.
    refresh_interval_hours int NOT NULL DEFAULT 24
        CHECK (refresh_interval_hours BETWEEN 1 AND 8760),
    updated_by uuid REFERENCES users (id) ON DELETE SET NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- ------------------------------------------------------------ the history --

CREATE TABLE tax_import_runs (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The URL as it was at fetch time, not a pointer to the row that may since have changed.
    source_url text NOT NULL,
    format     text NOT NULL,
    -- `manual` is an operator pressing Import; `scheduled` is the worker. The distinction
    -- matters because only the manual path can be told to overwrite a manual rate.
    trigger    text NOT NULL CHECK (trigger IN ('manual', 'scheduled')),
    actor_id   uuid REFERENCES users (id) ON DELETE SET NULL,
    fetched_at timestamptz NOT NULL,
    started_at timestamptz NOT NULL DEFAULT now(),
    -- NULL while the run is in flight. A run row that is still `running` an hour later is a
    -- worker that died mid-import, and it is visible rather than absent for exactly that reason.
    finished_at timestamptz,
    outcome    text NOT NULL DEFAULT 'running'
        CHECK (outcome IN ('running', 'ok', 'failed')),
    detail     text NOT NULL DEFAULT '',
    -- Headline counts, duplicated out of the report so "how much has this ever imported" is a
    -- query rather than a JSON unpack.
    rows_total int NOT NULL DEFAULT 0,
    created    int NOT NULL DEFAULT 0,
    updated    int NOT NULL DEFAULT 0,
    unchanged  int NOT NULL DEFAULT 0,
    protected  int NOT NULL DEFAULT 0,
    rejected   int NOT NULL DEFAULT 0,
    -- The per-row report verbatim: what each row became, and why each refusal was refused.
    report     jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX tax_import_runs_recent_idx ON tax_import_runs (started_at DESC);
