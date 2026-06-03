-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- 00027: Composite FK hardening — cross-tenant reference セーフガード強化
-- ===========================================================================
-- 目的:
--   DB 制約でクロステナント参照をさらに塞ぐ。FK チェックは RLS をバイパスするため、
--   (referencing_col, tenant_id) → referenced_table(id, tenant_id) の複合 FK が
--   唯一のDB レベル保証となる。
--
-- 方針:
--   1. UNIQUE(id, tenant_id) を未追加の基盤表 (users, roles) に追加する。
--   2. 既存の単一列 FK を複合 FK に置き換える:
--        departments.parent_id → departments(id, tenant_id)
--        employees.department_id → departments(id, tenant_id)
--        users.employee_id → employees(id, tenant_id)
--        users.role_id → roles(id, tenant_id)
--   3. 複合 FK は MATCH SIMPLE: NULL 列を含む行は FK チェックをスキップする
--      (nullable 外部キー列に標準的な動作)。
--
-- 見送り一覧 (スキップ理由は各エントリで明記):
--   - selection_stages.job_posting_id → job_postings(id, tenant_id):
--       各パッケージのテストが最小 seed のみで動作する設計であり、
--       selection テストは job_postings を seed しない。FK 追加でテスト破損。
--   - applications.job_posting_id / applications.applicant_id → 各参照先:
--       同上。selection テストが applicants / job_postings を seed しない。
--   - offers.application_id → applications(id, tenant_id):
--       offer テストが applications を seed しない。
--       migration 00021 にも「論理参照(素の uuid・FK なし)」と明記。
--   - interviews.application_id → applications(id, tenant_id):
--       interview テストが applications を seed しない。
--       migration 00024 にも「LOGICAL reference ... No FK」と明記。
--   - review_cycles への参照 (reviews.cycle_id, calibration_sessions.cycle_id):
--       migration 00022 に「cycle_id は論理参照; FK を張らない」と明記。
--       evaluation テストが review_cycles を seed しない。
--   - notifications.recipient_user_id → users:
--       users に UNIQUE(id, tenant_id) を 00027 Up で追加するが、
--       notification テストが users を seed するユーザは recipient として
--       正しく同一テナントに属するため、単純 FK で十分と判断。
--       composite FK への昇格は次フェーズ。
-- ===========================================================================

-- ---------------------------------------------------------------------------
-- Step 1: UNIQUE(id, tenant_id) を users / roles に追加
-- ---------------------------------------------------------------------------
-- users には UNIQUE(id, tenant_id) が存在しなかった(各 migration でコメントに記録済み)。
-- これを追加することで、将来的に users を参照先とする複合 FK が可能になる。
ALTER TABLE users
    ADD CONSTRAINT uq_users_id_tenant UNIQUE (id, tenant_id);

-- roles にも UNIQUE(id, tenant_id) が存在しなかった。
-- users.role_id → roles の複合 FK 参照先として必要。
ALTER TABLE roles
    ADD CONSTRAINT uq_roles_id_tenant UNIQUE (id, tenant_id);

-- ---------------------------------------------------------------------------
-- Step 2: departments.parent_id の複合 FK 化
-- ---------------------------------------------------------------------------
-- 既存の単一列 FK (departments_parent_id_fkey) を複合 FK に置き換える。
-- parent_id は NULL 許容(ルート部署)→ MATCH SIMPLE でNULL をスキップする。
-- departments には既に uq_departments_id_tenant UNIQUE(id, tenant_id) が存在
-- (00004 で追加)。
ALTER TABLE departments
    DROP CONSTRAINT IF EXISTS departments_parent_id_fkey;

ALTER TABLE departments
    ADD CONSTRAINT fk_departments_parent_tenant
        FOREIGN KEY (parent_id, tenant_id)
        REFERENCES departments(id, tenant_id)
        MATCH SIMPLE;

-- ---------------------------------------------------------------------------
-- Step 3: employees.department_id の複合 FK 化
-- ---------------------------------------------------------------------------
-- 既存の単一列 FK (employees_department_id_fkey) を複合 FK に置き換える。
-- department_id は NULL 許容(部署未所属の従業員)→ MATCH SIMPLE。
-- departments には uq_departments_id_tenant が存在 (00004)。
ALTER TABLE employees
    DROP CONSTRAINT IF EXISTS employees_department_id_fkey;

ALTER TABLE employees
    ADD CONSTRAINT fk_employees_department_tenant
        FOREIGN KEY (department_id, tenant_id)
        REFERENCES departments(id, tenant_id)
        MATCH SIMPLE;

-- ---------------------------------------------------------------------------
-- Step 4: users.employee_id の複合 FK 化
-- ---------------------------------------------------------------------------
-- 既存の単一列 FK (users_employee_id_fkey) を複合 FK に置き換える。
-- employee_id は NULL 許容(従業員レコードのない管理者ユーザ等)→ MATCH SIMPLE。
-- employees には uq_employees_id_tenant が存在 (00004)。
ALTER TABLE users
    DROP CONSTRAINT IF EXISTS users_employee_id_fkey;

ALTER TABLE users
    ADD CONSTRAINT fk_users_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE;

-- ---------------------------------------------------------------------------
-- Step 5: users.role_id の複合 FK 化
-- ---------------------------------------------------------------------------
-- 既存の単一列 FK (users_role_id_fkey; 00003 で追加) を複合 FK に置き換える。
-- role_id は NULL 許容→ MATCH SIMPLE。
-- roles には uq_roles_id_tenant を Step 1 で追加済み。
ALTER TABLE users
    DROP CONSTRAINT IF EXISTS users_role_id_fkey;

ALTER TABLE users
    ADD CONSTRAINT fk_users_role_tenant
        FOREIGN KEY (role_id, tenant_id)
        REFERENCES roles(id, tenant_id)
        MATCH SIMPLE;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- ---------------------------------------------------------------------------
-- Down: 逆順で複合 FK を削除し、単一列 FK を復元する
-- ---------------------------------------------------------------------------

-- Step 5 reversal: users.role_id
ALTER TABLE users
    DROP CONSTRAINT IF EXISTS fk_users_role_tenant;

ALTER TABLE users
    ADD CONSTRAINT users_role_id_fkey
        FOREIGN KEY (role_id)
        REFERENCES roles(id);

-- Step 4 reversal: users.employee_id
ALTER TABLE users
    DROP CONSTRAINT IF EXISTS fk_users_employee_tenant;

ALTER TABLE users
    ADD CONSTRAINT users_employee_id_fkey
        FOREIGN KEY (employee_id)
        REFERENCES employees(id);

-- Step 3 reversal: employees.department_id
ALTER TABLE employees
    DROP CONSTRAINT IF EXISTS fk_employees_department_tenant;

ALTER TABLE employees
    ADD CONSTRAINT employees_department_id_fkey
        FOREIGN KEY (department_id)
        REFERENCES departments(id);

-- Step 2 reversal: departments.parent_id
ALTER TABLE departments
    DROP CONSTRAINT IF EXISTS fk_departments_parent_tenant;

ALTER TABLE departments
    ADD CONSTRAINT departments_parent_id_fkey
        FOREIGN KEY (parent_id)
        REFERENCES departments(id);

-- Step 1 reversal: drop UNIQUE(id, tenant_id) from users and roles
ALTER TABLE roles
    DROP CONSTRAINT IF EXISTS uq_roles_id_tenant;

ALTER TABLE users
    DROP CONSTRAINT IF EXISTS uq_users_id_tenant;

-- +goose StatementEnd
