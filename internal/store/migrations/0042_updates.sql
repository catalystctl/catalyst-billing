-- 0042_updates.sql
--
-- Updates, telemetry and the extension marketplace (OPS-12, OPS-13, OPS-14, ADM-12,
-- ADM-23). Five decisions shape this file, and each one is recorded because the next
-- person will otherwise "simplify" it into a lie.
--
-- 1. **An update is approved, not applied.** This deployment is a plain binary: `billing
--    serve` and `billing worker` started by whatever supervises them, with the frontend
--    compiled into the artifact and the schema applied by `billing migrate` under a
--    *different* database role (the runtime role cannot DDL — 0002). There is no release
--    signing key in this repository and no supervisor to restart the process, so a
--    self-replacing binary would mean: fetch an unauthenticated artifact, write it over
--    the running executable, and hope. That is not a feature, it is a remote code
--    execution path with a settings page in front of it. So the panel records *which
--    version an operator approved* (`update_approval`) and reports the honest state —
--    "1.5.0 is available and approved; apply it with the deploy procedure" — instead of
--    pretending it can swap itself. `apply` in the API response is a documented
--    instruction, never a button that downloads an executable.
--
-- 2. **The check is cached in the database, not in memory.** A check on every page load
--    is a denial of service against the update server and against us. A cached row is
--    shared by every instance of a multi-process deployment, survives a restart, and lets
--    a failing update server be remembered briefly (a shorter TTL) instead of hammered.
--    `update_check` is a singleton because `updates.check_url` is a single setting: when
--    the URL changes, the cache is replaced rather than accumulated.
--
-- 3. **Telemetry is off by default and has no free-form fields.** The on/off switch is the
--    already-declared `updates.telemetry` setting. The payload is built in code from a
--    fixed allowlist of counts and versions (`internal/updates/telemetry.go`), so there is
--    no column here that could carry a customer, an address or an invoice. `telemetry_sends`
--    records exactly what left the building — the same JSON, stored, so an operator can
--    read it back and see there is nothing identifying in it.
--
-- 4. **A verified extension is a stored artifact plus a digest.** The core loads extensions
--    as separate processes from a path in its environment (`BILLING_GATEWAY_EXTENSION`,
--    `BILLING_PROVISIONER_EXTENSION`); it cannot register one at runtime. So an install
--    stores the exact bytes it verified, records the SHA-256 it verified them against, and
--    optionally stages a copy in a directory the operator nominated. Nothing is executed
--    here. `installed_extensions` therefore means "verified and available", and the API
--    says so in those words.
--
-- 5. **Configuration lives in rows, not in new environment variables.** The marketplace's
--    index URL and staging directory are operator decisions that belong in an audited,
--    editable row (`marketplace_config`), not in a process environment that requires a
--    restart and cannot be changed from the panel. `updates.check_url` and
--    `updates.telemetry` were already declared in `internal/settings`, so they are read
--    from there and not duplicated.
--
-- Nothing here touches an issued invoice, a journal entry or an audit row.

BEGIN;

-- ============================================================ update config ==
--
-- The two pieces of update configuration that are not settings keys: where a telemetry
-- payload would be sent, and the installation's own random identifier. The identifier is
-- `gen_random_uuid()` — generated here, unrelated to any customer, order, address or
-- hostname — and it exists only so the receiver can tell two installations apart. It is
-- returned in the payload preview, so an operator can see exactly what is sent.
--
-- `telemetry_endpoint` may carry an ingest token in its path or query, so it is treated as
-- a credential: the API never returns it, only its host.

CREATE TABLE update_config (
    id                 boolean PRIMARY KEY DEFAULT true CHECK (id),
    telemetry_endpoint text NOT NULL DEFAULT '',
    instance_id        uuid NOT NULL DEFAULT gen_random_uuid(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    -- ON DELETE SET NULL rather than a plain reference: the actor columns below are audit
    -- breadcrumbs, not ownership. A plain reference would make `billing admin purge-test-data`
    -- fail with a foreign-key violation the moment it deleted an operator who had ever
    -- approved a version, which is the wrong way round — the purge exists to remove a test
    -- customer, and refusing because an operator touched a setting is not a safety property.
    updated_by         uuid REFERENCES users (id) ON DELETE SET NULL
);

INSERT INTO update_config (id) VALUES (true) ON CONFLICT DO NOTHING;

COMMENT ON COLUMN update_config.telemetry_endpoint IS
    'Where a telemetry payload is POSTed. Treated as a credential: never returned by a read path.';

-- ============================================================= update check ==
--
-- The cached answer to "is there a newer version". One row: the configured check URL is a
-- single setting, so two rows would be a cache that can disagree with itself.
--
-- A failure is cached too (`failure_code`), with a shorter TTL applied in code: a broken
-- update server must not be asked on every page load either, but it must not be remembered
-- for an hour as if it were an answer.
--
-- `running_version` is stored with the answer because "1.5.0 available" is only meaningful
-- relative to the build that asked. A deployment upgraded in place invalidates its own
-- cache by writing a different `running_version`.

CREATE TABLE update_check (
    id              boolean PRIMARY KEY DEFAULT true CHECK (id),
    source_url      text NOT NULL,
    channel         text NOT NULL,
    running_version text NOT NULL,
    latest_version  text NOT NULL DEFAULT '',
    download_url    text NOT NULL DEFAULT '',
    notes           text NOT NULL DEFAULT '',
    published_at    timestamptz,
    checked_at      timestamptz NOT NULL DEFAULT now(),
    failure_code    text NOT NULL DEFAULT ''
);

-- ========================================================== update approval ==
--
-- The decision, not the action. No row means "no update has been approved", which is the
-- state of a fresh installation and the state after a clear.
--
-- The version is text and is validated by `updates.ParseVersion` before it is written:
-- a CHECK constraint that duplicated a semantic-version parser would be a second, worse
-- parser, and the column is never compared with SQL — the comparison happens in Go, where
-- the running version is known.

CREATE TABLE update_approval (
    id          boolean PRIMARY KEY DEFAULT true CHECK (id),
    version     text NOT NULL CHECK (length(version) > 0 AND length(version) <= 64),
    note        text NOT NULL DEFAULT '',
    approved_by uuid REFERENCES users (id) ON DELETE SET NULL,
    approved_at timestamptz NOT NULL DEFAULT now()
);

-- ====================================================== marketplace config ==
--
-- `index_url` is where the JSON listing is fetched from; empty means "the marketplace is
-- not configured", which is a state the screen renders honestly instead of an empty grid.
-- `install_dir` is where a verified artifact is staged when it is set. Empty means
-- "verified and stored in the database only", which is still a real install: the panel can
-- hand the exact bytes back and the operator places them where the deployment expects.

CREATE TABLE marketplace_config (
    id          boolean PRIMARY KEY DEFAULT true CHECK (id),
    index_url   text NOT NULL DEFAULT '',
    install_dir text NOT NULL DEFAULT '',
    updated_at  timestamptz NOT NULL DEFAULT now(),
    updated_by  uuid REFERENCES users (id) ON DELETE SET NULL
);

INSERT INTO marketplace_config (id) VALUES (true) ON CONFLICT DO NOTHING;

-- ======================================================= marketplace index ==
--
-- The fetched listing, cached with the same reasoning as the update check: a marketplace
-- page that fetches the index on every load is a denial of service against the index host,
-- and a slow index must not make the panel slow. `payload` is the parsed index, stored as
-- JSON so the screen can be rendered without the network; `failure_code` records the last
-- failure so a broken index is visible rather than an empty list.

CREATE TABLE marketplace_index (
    id           boolean PRIMARY KEY DEFAULT true CHECK (id),
    source_url   text NOT NULL,
    fetched_at   timestamptz NOT NULL DEFAULT now(),
    payload      jsonb,
    failure_code text NOT NULL DEFAULT ''
);

-- ===================================================== installed extensions ==
--
-- One row per verified extension version. The digest is the identity that matters: the
-- install path refuses to write a row until the downloaded bytes hash to the digest in the
-- index, and refuses to replace an existing row whose digest differs (a silent replacement
-- is how a "verified" extension becomes an unverified one).
--
-- `pinned_sha256` records that the operator supplied the digest out of band, which is the
-- only way the digest is worth anything against a compromised index. It is nullable
-- because most installs will not be pinned — and the API reports that honestly.
--
-- `staged_path` is where a copy was written when `marketplace_config.install_dir` is set.
-- Empty means the verified bytes live in `installed_extension_artifacts` only.

CREATE TABLE installed_extensions (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
    version       text NOT NULL CHECK (length(version) > 0 AND length(version) <= 64),
    publisher     text NOT NULL DEFAULT '',
    description   text NOT NULL DEFAULT '',
    kind          text NOT NULL DEFAULT 'other'
                  CHECK (kind IN ('gateway', 'provisioner', 'other')),
    sha256        text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    pinned_sha256 text CHECK (pinned_sha256 IS NULL OR pinned_sha256 ~ '^[0-9a-f]{64}$'),
    source_url    text NOT NULL,
    size_bytes    bigint NOT NULL CHECK (size_bytes > 0),
    staged_path   text NOT NULL DEFAULT '',
    installed_by  uuid REFERENCES users (id) ON DELETE SET NULL,
    installed_at  timestamptz NOT NULL DEFAULT now(),
    -- One row per name and version. A re-install of the same bytes is a no-op; the same
    -- version with different bytes is refused rather than overwritten.
    UNIQUE (name, version)
);

CREATE INDEX installed_extensions_name_idx ON installed_extensions (name, installed_at DESC);

-- The artifact itself. It is a separate table so a listing query cannot accidentally read
-- megabytes of binary, and it is bounded in code (8 MiB) before it is written: the panel
-- is not a package registry.
CREATE TABLE installed_extension_artifacts (
    extension_id uuid PRIMARY KEY REFERENCES installed_extensions (id) ON DELETE CASCADE,
    sha256       text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    content      bytea NOT NULL
);

-- ========================================================= telemetry sends ==
--
-- What was sent, when, and what the endpoint answered. The payload is stored because the
-- point of the privacy promise is that an operator can check it: "this is exactly what
-- left". There is no customer field in it by construction, so storing it creates no
-- second copy of anything sensitive.
--
-- Append-only by code: nothing updates or deletes these rows except retention tooling.

CREATE TABLE telemetry_sends (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    endpoint_host  text NOT NULL,
    payload        jsonb NOT NULL,
    payload_sha256 text NOT NULL CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),
    status         text NOT NULL CHECK (status IN ('sent', 'failed')),
    http_status    integer NOT NULL DEFAULT 0,
    failure_code   text NOT NULL DEFAULT '',
    sent_at        timestamptz NOT NULL DEFAULT now(),
    sent_by        uuid REFERENCES users (id) ON DELETE SET NULL
);

CREATE INDEX telemetry_sends_sent_idx ON telemetry_sends (sent_at DESC);

COMMIT;
