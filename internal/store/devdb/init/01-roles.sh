#!/bin/bash
# Runs once, on first initialisation of an empty data volume.
#
# Establishes the two-role model the architecture calls for:
#
#   $POSTGRES_USER  database owner; runs migrations; may DDL.
#   billing_app     runtime role; may only SELECT/INSERT/UPDATE/DELETE on tables
#                   and use sequences. It cannot create, alter or drop anything.
#
# A runtime process that cannot change the schema turns an entire class of
# incident into an error message instead of data loss.
#
# Written as a shell script rather than .sql so that the role password and the
# owner name come from the environment instead of being hard-coded.

set -Eeuo pipefail

: "${POSTGRES_USER:?POSTGRES_USER must be set}"
: "${APP_ROLE_PASSWORD:?APP_ROLE_PASSWORD must be set}"

psql -v ON_ERROR_STOP=1 \
     --username "$POSTGRES_USER" \
     --dbname "$POSTGRES_DB" \
     --set=owner="$POSTGRES_USER" \
     --set=app_password="$APP_ROLE_PASSWORD" <<'SQL'
-- A dedicated database for the integration test suite, so `go test` never
-- truncates anything a human is looking at.
SELECT format('CREATE DATABASE %I OWNER %I', 'billing_test', :'owner')
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'billing_test')
\gexec

SELECT format('CREATE ROLE billing_app LOGIN PASSWORD %L', :'app_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'billing_app')
\gexec
SQL

# Apply the privilege model to every database the panel uses. Kept as a loop so
# adding a database later is a one-line change.
for db in "$POSTGRES_DB" billing_test; do
  psql -v ON_ERROR_STOP=1 \
       --username "$POSTGRES_USER" \
       --dbname "$db" \
       --set=owner="$POSTGRES_USER" \
       --set=dbname="$db" <<'SQL'
REVOKE ALL ON DATABASE :"dbname" FROM PUBLIC;
GRANT CONNECT ON DATABASE :"dbname" TO billing_app;

-- The runtime role must not be able to create objects in public. PostgreSQL 15
-- already removes this from PUBLIC; being explicit documents the intent and
-- keeps the guarantee if the cluster is downgraded or a template changes.
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO billing_app;

-- Applies to tables and sequences created by future migrations.
ALTER DEFAULT PRIVILEGES FOR ROLE :"owner" IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO billing_app;
ALTER DEFAULT PRIVILEGES FOR ROLE :"owner" IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO billing_app;
SQL
done

echo "billing dev cluster: roles and databases ready"
