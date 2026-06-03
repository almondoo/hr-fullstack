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
# Security note: the password is passed via psql -v and referenced with
# :'hr_app_pass' (psql's auto-quoting) so that any character — including
# single quotes — is safe and cannot inject SQL.  Shell variable expansion
# is disabled inside the heredoc (<<-'EOSQL') to prevent accidental
# substitution at the shell level.

set -e

: "${HR_APP_DB_PASSWORD:?HR_APP_DB_PASSWORD environment variable must be set}"
: "${POSTGRES_USER:?POSTGRES_USER environment variable must be set}"
: "${POSTGRES_DB:?POSTGRES_DB environment variable must be set}"

# Check whether the role already exists before creating it.
# psql variable substitution (:'var') does not work inside PL/pgSQL DO blocks,
# so we use a shell-level conditional and a plain SQL CREATE ROLE statement where
# psql can substitute the quoted literal safely.
ROLE_EXISTS=$(psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
                   -tAc "SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'hr_app'")

if [ "$ROLE_EXISTS" = "1" ]; then
    echo "Role hr_app already exists — skipping creation."
else
    psql -v ON_ERROR_STOP=1 \
         -v hr_app_pass="$HR_APP_DB_PASSWORD" \
         --username "$POSTGRES_USER" \
         --dbname   "$POSTGRES_DB" \
         <<-'EOSQL'
CREATE ROLE hr_app
    LOGIN
    PASSWORD :'hr_app_pass'
    NOSUPERUSER
    NOBYPASSRLS
    NOCREATEDB
    NOCREATEROLE;
EOSQL
    echo "Role hr_app created."
fi
