#!/bin/sh
# Creates ESA's own role and database next to Redmine's, and sets the role's
# password from ESA_DB_PASSWORD, so .env stays authoritative. Compose runs it
# as the esa-db-init service before every ESA start; it is safe to run again.
# It connects with the libpq variables (PGHOST, PGPASSWORD) as POSTGRES_USER.
set -eu
if [ -z "${ESA_DB_PASSWORD:-}" ]; then
    echo "ESA_DB_PASSWORD is required: ESA keeps its state in this database" >&2
    exit 1
fi
psql -v ON_ERROR_STOP=1 -v esa_password="$ESA_DB_PASSWORD" --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
SELECT format('CREATE ROLE esa LOGIN PASSWORD %L', :'esa_password')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'esa')\gexec
SELECT format('ALTER ROLE esa LOGIN PASSWORD %L', :'esa_password')\gexec
SELECT 'CREATE DATABASE esa OWNER esa'
 WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'esa')\gexec
REVOKE ALL ON DATABASE esa FROM PUBLIC;
SQL
