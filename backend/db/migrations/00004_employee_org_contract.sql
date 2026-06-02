-- +goose Up
-- +goose StatementBegin

-- ---------------------------------------------------------------------------
-- employees: add HR attribute columns (skip existing columns)
-- ---------------------------------------------------------------------------
-- employee_code, status, hired_on are already in 00001.
-- Add the columns that are not yet present.
ALTER TABLE employees
    ADD COLUMN IF NOT EXISTS email           text,
    ADD COLUMN IF NOT EXISTS employment_type text NOT NULL DEFAULT 'full_time';

-- Unique index on (tenant_id, email) — sparse (only non-null rows), so we
-- use a partial index. Employees without email (legacy rows, temporary staff)
-- are unaffected.
CREATE UNIQUE INDEX IF NOT EXISTS uq_employees_tenant_email
    ON employees (tenant_id, email)
    WHERE email IS NOT NULL;

-- [Security: MUSTFIX 1] Add UNIQUE(id, tenant_id) on employees so that
-- composite foreign keys from child tables (employee_assignments,
-- employment_contracts) can reference (employee_id, tenant_id) together.
-- This prevents cross-tenant FK references at the DB-constraint level,
-- because FK checks bypass RLS.
ALTER TABLE employees
    ADD CONSTRAINT uq_employees_id_tenant UNIQUE (id, tenant_id);

-- [Security: MUSTFIX 1] Add UNIQUE(id, tenant_id) on departments for the
-- same reason — employee_assignments references department_id.
ALTER TABLE departments
    ADD CONSTRAINT uq_departments_id_tenant UNIQUE (id, tenant_id);

-- ---------------------------------------------------------------------------
-- employee_assignments (発令履歴)
-- ---------------------------------------------------------------------------
CREATE TABLE employee_assignments (
    id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES tenants(id),
    -- [Security: MUSTFIX 1] Composite FK: ensures (employee_id, tenant_id)
    -- must exist in employees, preventing cross-tenant employee references.
    employee_id     uuid        NOT NULL,
    -- [Security: MUSTFIX 1] Composite FK: ensures (department_id, tenant_id)
    -- must exist in departments when department_id is not NULL.
    -- MATCH SIMPLE: NULL department_id skips FK enforcement (nullable column).
    department_id   uuid,
    position        text,
    grade           text,
    effective_from  date        NOT NULL,
    effective_to    date,
    reason          text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_employee_assignments PRIMARY KEY (id),
    CONSTRAINT fk_assignments_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT fk_assignments_department_tenant
        FOREIGN KEY (department_id, tenant_id)
        REFERENCES departments(id, tenant_id)
        MATCH SIMPLE
);

CREATE INDEX idx_employee_assignments_lookup
    ON employee_assignments (tenant_id, employee_id, effective_from);

-- ---------------------------------------------------------------------------
-- employment_contracts (雇用契約)
-- ---------------------------------------------------------------------------
CREATE TABLE employment_contracts (
    id                 uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id          uuid        NOT NULL REFERENCES tenants(id),
    -- [Security: MUSTFIX 1] Composite FK: ensures (employee_id, tenant_id)
    -- must exist in employees, preventing cross-tenant employee references.
    employee_id        uuid        NOT NULL,
    contract_type      text        NOT NULL,
    start_date         date        NOT NULL,
    end_date           date,
    working_conditions jsonb       NOT NULL DEFAULT '{}',
    status             text        NOT NULL DEFAULT 'draft',
    signed_at          timestamptz,
    document_ref       text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_employment_contracts PRIMARY KEY (id),
    CONSTRAINT fk_contracts_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE
);

CREATE INDEX idx_employment_contracts_lookup
    ON employment_contracts (tenant_id, employee_id);

-- ---------------------------------------------------------------------------
-- RLS for new tables
-- ---------------------------------------------------------------------------
ALTER TABLE employee_assignments  ENABLE ROW LEVEL SECURITY;
ALTER TABLE employee_assignments  FORCE  ROW LEVEL SECURITY;
ALTER TABLE employment_contracts  ENABLE ROW LEVEL SECURITY;
ALTER TABLE employment_contracts  FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON employee_assignments
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON employment_contracts
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to hr_app
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE employee_assignments TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE employment_contracts TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE employment_contracts  FROM hr_app;
REVOKE ALL ON TABLE employee_assignments  FROM hr_app;

DROP TABLE IF EXISTS employment_contracts;
DROP TABLE IF EXISTS employee_assignments;

ALTER TABLE departments
    DROP CONSTRAINT IF EXISTS uq_departments_id_tenant;

ALTER TABLE employees
    DROP CONSTRAINT IF EXISTS uq_employees_id_tenant;

DROP INDEX IF EXISTS uq_employees_tenant_email;

ALTER TABLE employees
    DROP COLUMN IF EXISTS employment_type,
    DROP COLUMN IF EXISTS email;

-- +goose StatementEnd
