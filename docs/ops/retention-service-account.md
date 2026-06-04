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
INSERT INTO users (id, tenant_id, email, password_hash, role, status)
VALUES (
    gen_random_uuid(),
    '<TENANT_UUID>',
    'retention-svc@system.internal',   -- synthetic, non-routable
    '',                                 -- no password — service account, no login
    'system_retention',                 -- custom role (see Step 2)
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

Grant only the permissions listed in the table above.  If a dedicated
`system_retention` role does not exist yet, create it in `internal/platform/auth/rbac.go`
following the pattern of the existing role definitions, then run the RBAC seed.

```sql
-- Example: assign the retention_svc role to the system user within a tenant.
UPDATE users
SET role = 'system_retention'
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

This cross-tenant SELECT bypasses the per-tenant RLS policy on `tenants`.  It
requires the `hr_app` role to have `SELECT` on `tenants` (granted in migration
00001) and either:

- The `tenants` table RLS policy permits the query (it currently requires
  `app.tenant_id` to match — so in practice an `hr_admin` / `BYPASSRLS` role
  is needed for enumeration), OR
- The database role used for enumeration is granted `BYPASSRLS` on `tenants`
  only (a narrower privilege than full superuser).

**Recommended**: run the retention binary under a dedicated `hr_retention_enum`
database role that has `BYPASSRLS` scoped to `tenants` only, and `hr_app`-level
access (no BYPASSRLS) for all other tables.  This keeps the cross-tenant
enumeration privilege minimal and separate from row-level operations.

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
| `backend/internal/retention/job.go` | Retention sub-job implementations |
| `backend/db/migrations/00030_retention_job_state.sql` | `retention_job_runs` table |
| `backend/db/migrations/00035_ledger_retention_expired.sql` | `retention_expired` column |
| `infra/jobs/retention-cronjob.yaml` | Kubernetes CronJob example |
| `infra/jobs/retention-compose.yaml` | Docker Compose cron service example |
| `infra/jobs/retention-timer.service` | systemd service unit example |
| `infra/jobs/retention-timer.timer` | systemd timer unit example |
