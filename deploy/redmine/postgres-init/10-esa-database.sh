#!/bin/sh
# Creates ESA's own role and database next to Redmine's. The official image runs
# this on a new data volume; for an existing volume run it once by hand (see
# docs/ESA_OPERATIONS.md#postgres-state-store). Safe to run again.
set -eu
if [ -z "${ESA_DB_PASSWORD:-}" ]; then
    echo "ESA_DB_PASSWORD is not set; skipping the ESA database"
    exit 0
fi
psql -v ON_ERROR_STOP=1 -v esa_password="$ESA_DB_PASSWORD" --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
SELECT format('CREATE ROLE esa LOGIN PASSWORD %L', :'esa_password')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'esa')\gexec
SELECT 'CREATE DATABASE esa OWNER esa'
 WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'esa')\gexec
REVOKE ALL ON DATABASE esa FROM PUBLIC;
SQL
