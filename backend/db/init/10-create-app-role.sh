#!/usr/bin/env sh
# 10-create-app-role.sh
#
# Creates the hr_app PostgreSQL role that the application uses at runtime.
# This script is mounted into /docker-entrypoint-initdb.d/ so it runs once
# when the container is first initialised (before any application container
# starts).
#
# The password is read from the HR_APP_DB_PASSWORD environment variable so
# no credentials are stored in the repository.
#
# Design:
#   - hr_app is NOSUPERUSER + NOBYPASSRLS: RLS policies WILL apply to it.
#   - NOCREATEDB + NOCREATEROLE: least-privilege; migrations run as the
#     admin role (POSTGRES_USER), not hr_app.
#   - The CREATE ROLE is idempotent: if the role already exists, the script
#     skips creation instead of failing.
#
# Security note: the password value never appears in logs — it is substituted
# at runtime from the environment, not hard-coded here.

set -e

: "${HR_APP_DB_PASSWORD:?HR_APP_DB_PASSWORD environment variable must be set}"
: "${POSTGRES_USER:?POSTGRES_USER environment variable must be set}"
: "${POSTGRES_DB:?POSTGRES_DB environment variable must be set}"

psql -v ON_ERROR_STOP=1 \
     --username "$POSTGRES_USER" \
     --dbname   "$POSTGRES_DB" \
     <<-EOSQL
DO \$\$
BEGIN
    IF NOT EXISTS (
        SELECT FROM pg_catalog.pg_roles WHERE rolname = 'hr_app'
    ) THEN
        CREATE ROLE hr_app
            LOGIN
            PASSWORD '${HR_APP_DB_PASSWORD}'
            NOSUPERUSER
            NOBYPASSRLS
            NOCREATEDB
            NOCREATEROLE;
        RAISE NOTICE 'Role hr_app created.';
    ELSE
        RAISE NOTICE 'Role hr_app already exists — skipping creation.';
    END IF;
END
\$\$;
EOSQL
