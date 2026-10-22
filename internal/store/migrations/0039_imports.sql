-- 0039_imports.sql
--
-- The importer (OPS-10, OPS-11): one row per committed import run, and one row per
-- legacy id that has been imported.
--
-- Four decisions are recorded here, because each one is the reason a later "simplification"
-- would quietly break the property that makes an import safe to repeat.
--
-- 1. **A real import is ONE transaction, not a resumable job with a step list.**
--    The alternative — persist a plan, execute it in steps, track progress — buys two
--    things: it survives a crash mid-import, and it can be paused. Neither is worth what it
--    costs here. A dump is a bounded document an operator uploads (the request body is
--    capped at 1 MiB, roughly ten thousand rows across the five files); it is not a feed.
--    A step list would mean a half-imported customer database — customers without their
--    invoices, invoices without their lines — with a progress row explaining why, and every
--    read path in the panel would have to tolerate that state. One transaction means the
--    only two outcomes are "the dump is in" and "the dump is not in", and the dry run has
--    already told the operator which one it will be. A crash is a rollback, and the
--    idempotency below makes the retry cheap rather than merely possible.
--    The consequence is deliberate: the run holds locks for as long as it takes to write a
--    few thousand rows, which is seconds, and a second import waits behind it.
--
-- 2. **Idempotency is a row in `import_records`, not a check in the importer.**
--    Every imported entity carries a natural key from the old system (its legacy id), and
--    `UNIQUE (source, entity, legacy_id)` is what makes re-running the same dump create
--    nothing: the second run finds the mapping and takes the update-or-unchanged path
--    instead of the create path. A service-level "have I seen this id?" would be a cache of
--    that fact with no authority, and the first time it disagreed with the table the run
--    would either duplicate a customer or overwrite one. `UNIQUE (source, entity,
--    target_id)` is the other direction of the same invariant: one target row is mapped at
--    most once, so an update can never be applied to a row that belongs to a different
--    legacy record.
--
-- 3. **A dry run writes nothing at all — not even a run row.**
--    "Reports what would happen" and "records that a dry run happened" are different
--    promises, and the first is the one the operator relies on: after a dry run the
--    database is byte-identical to what it was. So there is no `mode` column here and no
--    dry-run rows to filter out of a report; the run table is a log of what was actually
--    imported. The dry run is not a second implementation of the rules — it runs the same
--    code inside a transaction that is always rolled back, so the counts it reports are the
--    counts a commit would produce.
--
-- 4. **`import_records.target_id` is a uuid with no foreign key, on purpose.**
--    It points into five different tables (customers, invoices, invoice_lines, services,
--    payments), and PostgreSQL cannot express a foreign key that discriminates on a sibling
--    column without a trigger per target table. The cost of the missing key is that a
--    deleted target leaves a dangling mapping; the benefit is that the importer does not
--    need a new trigger every time an entity is added. Deleting an imported customer is
--    already refused by the invoice foreign keys, so the dangling case is close to
--    unreachable.
--
-- Both tables are append-only. A run's report is the evidence of what the import did, and a
-- mapping is a fact about a past import: re-pointing one would silently rewrite history,
-- which is the same reason `audit_events` and the ledger refuse UPDATE.
--
-- Nothing here rewrites an issued invoice, a journal entry or an audit row, and nothing
-- here posts to the ledger. An imported invoice is a record of what the old system did; it
-- is not a revenue event of this one.

CREATE TABLE import_runs (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The dump dialect: 'whmcs' or 'paymenter'. Lower case so the column is greppable and
    -- the unique key below cannot be defeated by case.
    source     text NOT NULL CHECK (source ~ '^[a-z][a-z0-9_]{2,31}$'),
    -- Who ran it. Nullable so a run driven by a future CLI has no fabricated user.
    actor_id   uuid REFERENCES users (id),
    created    int  NOT NULL DEFAULT 0 CHECK (created >= 0),
    updated    int  NOT NULL DEFAULT 0 CHECK (updated >= 0),
    unchanged  int  NOT NULL DEFAULT 0 CHECK (unchanged >= 0),
    rejected   int  NOT NULL DEFAULT 0 CHECK (rejected >= 0),
    -- The whole report, verbatim: per-file counts, every rejection with its row number and
    -- reason, every warning. Stored rather than recomputed because the answer to "why is
    -- this customer here" must not depend on the dump still existing on someone's laptop.
    report     jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX import_runs_created_idx ON import_runs (created_at DESC);

CREATE TABLE import_records (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source     text NOT NULL CHECK (source ~ '^[a-z][a-z0-9_]{2,31}$'),
    entity     text NOT NULL CHECK (entity IN
                   ('customer', 'invoice', 'invoice_line', 'service', 'payment')),
    -- The natural key from the old system, as text: a legacy id is not necessarily a
    -- number, and forcing it into one would reject the dumps that matter.
    legacy_id  text NOT NULL CHECK (legacy_id <> ''),
    target_id  uuid NOT NULL,
    -- The run that first imported this record. A later run that updates the target leaves
    -- this alone: it answers "what did run X bring in", not "what touched this last".
    -- Deferred on purpose: the run row is written *last*, when its report is complete, and
    -- `import_runs` is append-only, so a placeholder report that is later updated is not an
    -- option. Deferring the check to commit lets a mapping name the run that is about to be
    -- inserted without inventing a second, mutable table to hold the report.
    run_id     uuid NOT NULL REFERENCES import_runs (id) DEFERRABLE INITIALLY DEFERRED,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT import_records_natural_key UNIQUE (source, entity, legacy_id),
    CONSTRAINT import_records_one_target UNIQUE (source, entity, target_id)
);

CREATE INDEX import_records_run_idx ON import_records (run_id);

CREATE TRIGGER import_runs_append_only
    BEFORE UPDATE OR DELETE ON import_runs
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

CREATE TRIGGER import_records_append_only
    BEFORE UPDATE OR DELETE ON import_records
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();
