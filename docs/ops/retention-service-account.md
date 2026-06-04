# Retention Job — Service-Account Provisioning Guide

## Overview

The `cmd/retention` binary (and its `-all-tenants` mode) executes automated
data-retention and disposal operations on behalf of a **dedicated, purpose-limited
system service account**.  This document describes how to create that account and
grant it the minimum required permissions.

**SECURITY PRINCIPLE**: the service account must NOT be a super-admin or a
shared administrator account.  It must hold exactly the permissions required for
retention operations and nothing more (principle of least privilege).

---

## Why a Dedicated Service Account?

The retention job:

- Triggers `mynumber.Service.Dispose` — which requires the `mynumber:reveal`
  permission to pass RBAC checks inside the service layer.
- Writes `retention_expired = true` on ledger records — requires `ledger:write`.
- Inserts rows into `audit_logs` (via `audit.Record`) — requires INSERT on
  `audit_logs`.
- Inserts / updates rows in `retention_job_runs` — scoped to the tenant.

None of these operations should be bundled with general HR-admin privileges.

---

## Permissions Required Per Tenant

| Permission / Role  | Purpose                                           |
|--------------------|---------------------------------------------------|
| `mynumber:reveal`  | Required by `mynumber.Service.Dispose` RBAC check |
| `ledger:write`     | Required to set `retention_expired` on ledger rows |
| `audit_logs INSERT`| `audit.Record` writes disposal/expiry audit entries |
| `retention_job_runs INSERT/UPDATE` | job run tracking (migration 00030) |

The exact RBAC role names are defined in `internal/platform/auth/rbac.go`.
The `mynumber:reveal` permission is currently assigned to the `hr_admin` role
in the RBAC configuration; a dedicated `retention_svc` role with only the above
permissions is recommended for production.

---

## Provisioning Steps

### Step 1 — Create the system user row

Insert a user row that represents the retention service account.  Use **synthetic
data only** (never real PII):

```sql
-- Run as hr_admin or via a migration/seed script.
-- Do NOT commit the resulting UUID to the repository.
--
-- NOTE: users has no 'role' column.  The role is assigned via role_id FK
-- (added by migration 00003_auth_rbac_audit.sql).  Insert the user first,
-- then point role_id at the system_retention role in Step 2.
INSERT INTO users (id, tenant_id, email, password_hash, status)
VALUES (
    gen_random_uuid(),
    '<TENANT_UUID>',
    'retention-svc@system.internal',   -- synthetic, non-routable
    NULL,                              -- no password — service account, no login
    'active'
)
RETURNING id;  -- capture this UUID as RETENTION_ACTOR_ID
```

Record the returned UUID.  This is the value you will pass as:
- `-actor-id=<UUID>` (single-tenant mode), or
- `RETENTION_ACTOR_ID=<UUID>` environment variable (all-tenants mode).

**Important**: repeat Step 1 for every tenant that the job will process, OR
provision one shared system user at the system level if your schema supports
tenant-agnostic service accounts (check `users.tenant_id` nullability).

### Step 2 — Assign the minimum RBAC role

Grant only the permissions listed in the table above.  The `system_retention`
role name and its permission set are defined in
`internal/platform/auth/rbac.go` (`RoleSystemRetention`,
`SystemRetentionPermsJSON`).

First, insert the role row for the tenant (idempotent):

```sql
-- Create the system_retention role row for this tenant (if not already present).
-- The permissions JSON matches SystemRetentionPermsJSON in rbac.go.
INSERT INTO roles (id, tenant_id, name, permissions)
VALUES (
    gen_random_uuid(),
    '<TENANT_UUID>',
    'system_retention',
    '{"perms":["mynumber:reveal","ledger:write"]}'::jsonb
)
ON CONFLICT (tenant_id, name) DO NOTHING;
```

Then point the service-account user's `role_id` at the new role row:

```sql
-- Assign the system_retention role to the retention service-account user.
-- users.role_id is a UUID FK to roles.id (migration 00003_auth_rbac_audit.sql).
UPDATE users
SET role_id = (
    SELECT id FROM roles
    WHERE name = 'system_retention'
      AND tenant_id = '<TENANT_UUID>'
    LIMIT 1
)
WHERE id = '<RETENTION_ACTOR_UUID>'
  AND tenant_id = '<TENANT_UUID>';
```

Do NOT grant `hr_admin`, `super_admin`, or any broad role to this account.

### Step 3 — Store the UUID in the secret store

Store `RETENTION_ACTOR_ID` alongside `DB_PASSWORD` in your secret manager
(AWS Secrets Manager / GCP Secret Manager / HashiCorp Vault / Kubernetes Secret).

For Kubernetes:
```sh
kubectl create secret generic hr-retention-secrets \
  --from-literal=DB_PASSWORD='<hr_app role password>' \
  --from-literal=RETENTION_ACTOR_ID='<UUID from Step 1>'
```

For systemd / EnvironmentFile:
```
# /etc/hr-saas/retention.env — chmod 600, owned by root
DB_PASSWORD=<hr_app role password>
RETENTION_ACTOR_ID=<UUID from Step 1>
```

**Never commit the actual UUID or password to the repository.**

### Step 4 — Verify RBAC before first run

Smoke-test the permissions by running the job in dry-run / single-tenant mode
against a non-production tenant:

```sh
go run ./cmd/retention \
  -tenant-id=<TEST_TENANT_UUID> \
  -actor-id=<RETENTION_ACTOR_UUID> \
  -job=ledger_retention
```

Check `retention_job_runs` and `audit_logs` for the expected entries.

---

## Minimum-Privilege Design Rationale

| What the job does NOT have | Why it is excluded |
|----------------------------|--------------------|
| HR admin / super-admin role | Would allow reading arbitrary employee PII; violates least privilege |
| `mynumber:read` (view plaintext) | Disposal goes through `Service.Dispose` which only handles opaque IDs; no plaintext is ever read by the job |
| `employees:write` | Employee-data policy only flags eligibility; it does NOT delete rows |
| DB superuser / BYPASSRLS | All queries run as `hr_app` (NOBYPASSRLS); RLS enforces tenant isolation |

---

## Cross-Tenant Enumeration (all-tenants mode)

When `-all-tenants` is used, the binary issues a **direct query** on the
`tenants` table outside a `WithinTenant` transaction.  This query enumerates
all active tenant IDs.

This cross-tenant SELECT requires bypassing the per-tenant RLS policy on
`tenants` (which normally restricts `hr_app` to its own row only).

### hr_retention_enum — dedicated DB role for cross-tenant enumeration

The retention binary should connect as the `hr_retention_enum` PostgreSQL role
when running in `-all-tenants` mode.  This role:

- Is **NOSUPERUSER + NOBYPASSRLS** at the role attribute level.
- Is a **member of `hr_app`** (inherits DML grants on retention-related tables).
- Has **SELECT on `tenants`** explicitly granted.
- Is covered by a **permissive RLS policy** on `tenants` (`hr_retention_enum_read_all`)
  that allows it to read ALL tenant rows — scoped to this role only, leaving the
  main `tenant_isolation` policy unchanged for all other roles.

This approach is preferred over a blanket `BYPASSRLS` role attribute because it
limits the enumeration privilege to exactly the one table that needs it.

#### Provisioning

`hr_retention_enum` is created by `backend/db/init/20-create-retention-enum-role.sh`,
which runs inside Docker during database initialisation (analogous to
`10-create-app-role.sh` for `hr_app`).

Required environment variable:

```sh
HR_RETENTION_ENUM_DB_PASSWORD=<strong-random-password>
```

For non-Docker deployments (bare PostgreSQL), run the equivalent SQL manually
as a superuser.  The role is **standalone** (no `GRANT hr_app`) and receives
only the tables/sequences that `job.go` and its service calls actually access:

```sql
-- 1. Create the role (replace <PASSWORD> with a strong random password)
CREATE ROLE hr_retention_enum
    LOGIN
    PASSWORD '<PASSWORD>'
    NOSUPERUSER
    NOBYPASSRLS
    NOCREATEDB
    NOCREATEROLE;

-- 2. Schema access
GRANT USAGE ON SCHEMA public TO hr_retention_enum;

-- 3. Cross-tenant enumeration (tenants table + permissive RLS policy)
GRANT SELECT ON TABLE tenants TO hr_retention_enum;

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

-- 4. Per-tenant DML: job-run tracking, audit, RBAC lookup
GRANT SELECT, INSERT, UPDATE ON TABLE retention_job_runs TO hr_retention_enum;
GRANT INSERT                 ON TABLE audit_logs         TO hr_retention_enum;
GRANT USAGE, SELECT ON SEQUENCE audit_logs_seq_seq       TO hr_retention_enum;
GRANT SELECT ON TABLE roles TO hr_retention_enum;
GRANT SELECT ON TABLE users TO hr_retention_enum;

-- 5. RunMyNumberDisposal (job.go -> mynumber.Service.Dispose)
GRANT SELECT, UPDATE ON TABLE mynumber_records     TO hr_retention_enum;
GRANT INSERT         ON TABLE mynumber_disposals   TO hr_retention_enum;
GRANT SELECT, INSERT ON TABLE mynumber_access_logs TO hr_retention_enum;
GRANT SELECT         ON TABLE mynumber_purposes    TO hr_retention_enum;
GRANT USAGE, SELECT ON SEQUENCE mynumber_access_logs_seq_seq TO hr_retention_enum;

-- 6. RunLedgerRetention
GRANT SELECT, UPDATE ON TABLE worker_rosters   TO hr_retention_enum;
GRANT SELECT, UPDATE ON TABLE wage_ledgers     TO hr_retention_enum;
GRANT SELECT, UPDATE ON TABLE attendance_books TO hr_retention_enum;

-- 7. RunEmployeeDataPolicy
GRANT SELECT ON TABLE employees            TO hr_retention_enum;
GRANT SELECT ON TABLE employment_contracts TO hr_retention_enum;

-- 8. RunDocumentExpiry
GRANT SELECT, UPDATE ON TABLE documents TO hr_retention_enum;
```

Store the password in the same secret store as `DB_PASSWORD` (AWS Secrets
Manager / GCP Secret Manager / Kubernetes Secret).  **Never commit the
password to the repository.**

#### Connection environment variables (all-tenants mode)

When running in `-all-tenants` mode, set `DB_USER=hr_retention_enum` and
supply the corresponding `DB_PASSWORD`.  Single-tenant mode continues to use
`DB_USER=hr_app`.

The Kubernetes CronJob example (`infra/jobs/retention-cronjob.yaml`) uses
`DB_USER: hr_app` as a placeholder — update to `hr_retention_enum` when
deploying `-all-tenants` runs.

The exact database role design depends on the target deployment (#9).

---

## Legal Note

All retention-period thresholds, disposal methods, and grace periods controlled
by this job are **configuration values** (not hardcoded).  Before enabling the
job in production, all threshold values must be confirmed against the latest
statutory guidance by a certified social-insurance/labour consultant (社労士)
or lawyer (弁護士).

This document is operational guidance, not legal advice.

---

## Related Files

| File | Purpose |
|------|---------|
| `backend/cmd/retention/main.go` | Binary entry point; `-all-tenants` flag |
| `backend/Dockerfile.retention` | Production Docker image for the retention binary |
| `backend/internal/retention/job.go` | Retention sub-job implementations |
| `backend/internal/platform/auth/rbac.go` | `RoleSystemRetention` constant and `SystemRetentionPerms` |
| `backend/db/init/10-create-app-role.sh` | Provisions the `hr_app` database role (Docker init) |
| `backend/db/init/20-create-retention-enum-role.sh` | Provisions the `hr_retention_enum` database role (Docker init) |
| `backend/db/migrations/00030_retention_job_state.sql` | `retention_job_runs` table |
| `backend/db/migrations/00035_ledger_retention_expired.sql` | `retention_expired` column |
| `infra/jobs/retention-cronjob.yaml` | Kubernetes CronJob example |
| `infra/jobs/retention-compose.yaml` | Docker Compose cron service example |
| `infra/jobs/retention-timer.service` | systemd service unit example |
| `infra/jobs/retention-timer.timer` | systemd timer unit example |
