#!/usr/bin/env sh
# 20-create-retention-enum-role.sh
#
# Creates the hr_retention_enum PostgreSQL role used by cmd/retention.
#
# This script is mounted into /docker-entrypoint-initdb.d/ so it runs once
# when the container is first initialised (before any application container
# starts).  It must run AFTER 10-create-app-role.sh (lexicographic ordering).
#
# The password is read from the HR_RETENTION_ENUM_DB_PASSWORD environment
# variable so no credentials are stored in the repository.
#
# Design:
#   hr_retention_enum is a standalone role (NOSUPERUSER + NOBYPASSRLS).
#   It does NOT inherit hr_app — instead it receives only the minimum DML
#   grants required by the retention job (cmd/retention / internal/retention).
#
#   Granted tables (derived from job.go and service calls it invokes):
#
#   Cross-tenant enumeration (-all-tenants):
#     tenants                — SELECT (enumerate active tenants)
#     A permissive RLS policy (hr_retention_enum_read_all) allows reading
#     ALL rows; the tenant_isolation policy is unchanged for all other roles.
#
#   Per-tenant DML (all sub-jobs run under WithinTenant via the same connection):
#     retention_job_runs     — SELECT, INSERT, UPDATE  (job-run state tracking)
#     audit_logs             — INSERT                  (audit.Record)
#     audit_logs_seq_seq     — USAGE, SELECT           (bigserial sequence)
#     roles                  — SELECT                  (RBAC: LoadUserPermissions)
#     users                  — SELECT                  (RBAC: LoadUserPermissions)
#
#   RunMyNumberDisposal (job.go -> mynumber.Service.Dispose; migration 00010):
#     mynumber_records       — SELECT, UPDATE
#     mynumber_disposals     — INSERT
#     mynumber_access_logs   — SELECT, INSERT
#     mynumber_access_logs_seq_seq — USAGE, SELECT
#     mynumber_purposes      — SELECT
#
#   RunLedgerRetention (migration 00015 + 00035):
#     worker_rosters         — SELECT, UPDATE  (retention_expired flag)
#     wage_ledgers           — SELECT, UPDATE
#     attendance_books       — SELECT, UPDATE
#
#   RunEmployeeDataPolicy (migration 00004):
#     employees              — SELECT  (terminated/left filter)
#     employment_contracts   — SELECT  (latest end_date join)
#     worker_rosters/wage_ledgers/attendance_books — SELECT (active ledger check)
#
#   RunDocumentExpiry (migration 00017):
#     documents              — SELECT, UPDATE  (logically_expired flag)
#
#   NOTE: ledger_settings is NOT directly queried by job.go; the
#   LedgerRetentionFallbackYears config value is injected at startup via flags.
#   No GRANT on ledger_settings is required.
#
# Security note: the password is passed via psql -v and referenced with
# :'hr_retention_enum_pass' (psql's auto-quoting) so that any character —
# including single quotes — is safe and cannot inject SQL.  Shell variable
# expansion is disabled inside the heredoc (<<-'EOSQL') to prevent accidental
# substitution at the shell level.
#
# See docs/ops/retention-service-account.md for full design rationale.

set -e

: "${HR_RETENTION_ENUM_DB_PASSWORD:?HR_RETENTION_ENUM_DB_PASSWORD environment variable must be set}"
: "${POSTGRES_USER:?POSTGRES_USER environment variable must be set}"
: "${POSTGRES_DB:?POSTGRES_DB environment variable must be set}"

# Check whether the role already exists before creating it.
ROLE_EXISTS=$(psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
                   -tAc "SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'hr_retention_enum'")

if [ "$ROLE_EXISTS" = "1" ]; then
    echo "Role hr_retention_enum already exists — skipping creation."
else
    psql -v ON_ERROR_STOP=1 \
         -v hr_retention_enum_pass="$HR_RETENTION_ENUM_DB_PASSWORD" \
         --username "$POSTGRES_USER" \
         --dbname   "$POSTGRES_DB" \
         <<-'EOSQL'
-- hr_retention_enum: dedicated role for cmd/retention.
-- Standalone (not a member of hr_app); granted only the tables/sequences
-- that job.go and its service calls actually access.
CREATE ROLE hr_retention_enum
    LOGIN
    PASSWORD :'hr_retention_enum_pass'
    NOSUPERUSER
    NOBYPASSRLS
    NOCREATEDB
    NOCREATEROLE;

-- Schema access
GRANT USAGE ON SCHEMA public TO hr_retention_enum;

-- -------------------------------------------------------------------------
-- Cross-tenant enumeration
-- -------------------------------------------------------------------------
GRANT SELECT ON TABLE tenants TO hr_retention_enum;

-- Permissive RLS policy: hr_retention_enum can read ALL rows in tenants.
-- Scoped to this role only; tenant_isolation for hr_app is unchanged.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE tablename = 'tenants'
          AND policyname = 'hr_retention_enum_read_all'
    ) THEN
        EXECUTE $pol$
            CREATE POLICY hr_retention_enum_read_all ON tenants
                AS PERMISSIVE
                FOR SELECT
                TO hr_retention_enum
                USING (true)
        $pol$;
    END IF;
END;
$$;

-- -------------------------------------------------------------------------
-- Per-tenant DML (shared by all sub-jobs)
-- -------------------------------------------------------------------------
-- Job-run state tracking (migration 00030)
GRANT SELECT, INSERT, UPDATE ON TABLE retention_job_runs TO hr_retention_enum;

-- Audit log writes (migration 00001 + 00003)
GRANT INSERT ON TABLE audit_logs TO hr_retention_enum;
GRANT USAGE, SELECT ON SEQUENCE audit_logs_seq_seq TO hr_retention_enum;

-- RBAC lookup: LoadUserPermissions in platform/auth/rbac.go
GRANT SELECT ON TABLE roles TO hr_retention_enum;
GRANT SELECT ON TABLE users TO hr_retention_enum;

-- -------------------------------------------------------------------------
-- RunMyNumberDisposal: job.go -> mynumber.Service.Dispose (migration 00010)
-- -------------------------------------------------------------------------
GRANT SELECT, UPDATE ON TABLE mynumber_records     TO hr_retention_enum;
GRANT INSERT         ON TABLE mynumber_disposals   TO hr_retention_enum;
GRANT SELECT, INSERT ON TABLE mynumber_access_logs TO hr_retention_enum;
GRANT SELECT         ON TABLE mynumber_purposes    TO hr_retention_enum;
GRANT USAGE, SELECT ON SEQUENCE mynumber_access_logs_seq_seq TO hr_retention_enum;

-- -------------------------------------------------------------------------
-- RunLedgerRetention (migration 00015 + 00035)
-- -------------------------------------------------------------------------
GRANT SELECT, UPDATE ON TABLE worker_rosters   TO hr_retention_enum;
GRANT SELECT, UPDATE ON TABLE wage_ledgers     TO hr_retention_enum;
GRANT SELECT, UPDATE ON TABLE attendance_books TO hr_retention_enum;

-- -------------------------------------------------------------------------
-- RunEmployeeDataPolicy (migration 00004)
-- -------------------------------------------------------------------------
GRANT SELECT ON TABLE employees            TO hr_retention_enum;
GRANT SELECT ON TABLE employment_contracts TO hr_retention_enum;
-- worker_rosters/wage_ledgers/attendance_books already granted above

-- -------------------------------------------------------------------------
-- RunDocumentExpiry (migration 00017)
-- -------------------------------------------------------------------------
GRANT SELECT, UPDATE ON TABLE documents TO hr_retention_enum;
EOSQL
    echo "Role hr_retention_enum created with minimum required grants."
fi
