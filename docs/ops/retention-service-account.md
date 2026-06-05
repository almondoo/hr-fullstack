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

## Legal Note — 法定保存期間(労働基準法・税務関係・マイナンバー)

> **免責**: 以下に記載する保存期間・条文の解釈は、一次法令源との突合により確認した
> 情報ですが、**社会保険労務士（社労士）・税理士・弁護士による一次法令源との確認が前提**
> です。本ドキュメントは法的助言ではありません。

### 労働基準法第109条(記録の保存)

労働者名簿・賃金台帳・出勤簿・雇入・解雇・災害補償・賃金に関する書類等の
法定保存期間は以下のとおりです。

| 期間 | 根拠 | 備考 |
|---|---|---|
| **原則5年** | 労基法第109条(令和2年4月1日改正後) | 現行条文の規定値 |
| **当面の実務統制値: 3年** | 労基法附則第143条第1項の経過措置 | 「当分の間」3年とみなす旨の経過措置。2026年時点も有効、廃止時期未定 |

出典:
- 厚生労働省 <https://www.mhlw.go.jp/content/000617980.pdf>
- 厚生労働省スタートアップ労働条件 <https://www.startup-roudou.mhlw.go.jp/qa/zigyonushi/syuugyoukisoku/q6.html>
- e-Gov 労働基準法 第109条・附則第143条 <https://laws.e-gov.go.jp/law/322AC0000000049>

### 本システムの設計方針

- **デフォルト保持期間は3年**（現行の経過措置に対応）とする。
- **将来の5年化**（経過措置廃止後）に備え、保持閾値は設定値(configuration)で
  管理する設計とする。コードはすでに閾値を運用設定に委譲しており、この設計を追認する。
- 経過措置の廃止が告示された時点で、設定値を3年→5年へ更新し、本ドキュメントを
  改訂すること。

All retention-period thresholds, disposal methods, and grace periods controlled
by this job are **configuration values** (not hardcoded).  Before enabling the
job in production, all threshold values must be confirmed against the latest
statutory guidance by a certified social-insurance/labour consultant (社労士)
or lawyer (弁護士).

This document is operational guidance, not legal advice.

---

### 税務関係書類の保存期間(電子帳簿保存法・所得税法施行規則)

出典:
- 国税庁 No.5930 法人帳簿書類の保存期間
  <https://www.nta.go.jp/taxes/shiraberu/taxanswer/hojin/5930.htm>
- 国税庁 No.2503 扶養控除等申告書等の保存期間
  <https://www.nta.go.jp/taxes/shiraberu/taxanswer/gensen/2503.htm>
- 所得税法施行規則第76条の3

| 書類区分 | 起算点 | 保存期間 | 備考 |
|---|---|---|---|
| 法人帳簿書類(確定申告書等) | 確定申告書提出期限の翌日 | **7年**(原則) | 青色欠損金のある事業年度は10年。平成30年4月前開始分は9年 |
| 扶養控除等申告書等(マイナンバー記載) | 提出期限の属する年の翌年1月10日の翌日 | **7年** | 所規第76条の3。期間経過後マイナンバーを速やかに廃棄 |

**本システムの設計への参考**:
- 上記保存期間7年はretentionジョブの閾値設定の**参考値**である。
- **実際の閾値はシステム設定値として管理し、法務確定(税理士・社労士・弁護士確認)を経て
  設定すること**。コードへの数値ハードコードは行わない。
- 労基法の実務統制値(当面3年)と税務7年が混在する場合は書類種別ごとに閾値を分けること。

---

### マイナンバー廃棄義務との接続

個人番号(マイナンバー)は、事務処理の必要がなくなり**かつ**所管法令の保存期間を経過した
時点で、できるだけ速やかに廃棄・削除する義務がある(番号法の定め)。

本システムの `mynumber.Service.Dispose` および retentionジョブの
`RunMyNumberDisposal` はこの廃棄義務に対応する実装である。実際の廃棄タイミングは
保存期間の設定値(上記の法務確定値)に従うこと。

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
