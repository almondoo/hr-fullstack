-- +goose Up
-- +goose StatementBegin

-- Enable pgcrypto for gen_random_uuid() (PG13 and earlier need it;
-- PG17 provides gen_random_uuid() as a core function without the extension,
-- but the extension is harmless to create and keeps backward compatibility).
-- gen_random_uuid() is used in DEFAULT clauses below.

-- ---------------------------------------------------------------------------
-- tenants
-- ---------------------------------------------------------------------------
CREATE TABLE tenants (
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    name        text        NOT NULL,
    plan_code   text        NOT NULL DEFAULT 'free',
    status      text        NOT NULL DEFAULT 'active',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_tenants PRIMARY KEY (id)
);

-- ---------------------------------------------------------------------------
-- departments
-- ---------------------------------------------------------------------------
CREATE TABLE departments (
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES tenants(id),
    parent_id   uuid        REFERENCES departments(id),
    name        text        NOT NULL,
    code        text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_departments PRIMARY KEY (id)
);

CREATE INDEX idx_departments_tenant_id ON departments (tenant_id);

-- ---------------------------------------------------------------------------
-- roles
-- ---------------------------------------------------------------------------
CREATE TABLE roles (
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES tenants(id),
    name        text        NOT NULL,
    permissions jsonb       NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_roles PRIMARY KEY (id),
    CONSTRAINT uq_roles_tenant_name UNIQUE (tenant_id, name)
);

CREATE INDEX idx_roles_tenant_id ON roles (tenant_id);

-- ---------------------------------------------------------------------------
-- employees
-- ---------------------------------------------------------------------------
CREATE TABLE employees (
    id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES tenants(id),
    employee_code   text        NOT NULL,
    last_name       text        NOT NULL,
    first_name      text        NOT NULL,
    department_id   uuid        REFERENCES departments(id),
    status          text        NOT NULL DEFAULT 'active',
    hired_on        date,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_employees PRIMARY KEY (id),
    CONSTRAINT uq_employees_tenant_code UNIQUE (tenant_id, employee_code)
);

CREATE INDEX idx_employees_tenant_id ON employees (tenant_id);
CREATE INDEX idx_employees_department_id ON employees (department_id);
CREATE INDEX idx_employees_status ON employees (tenant_id, status);

-- ---------------------------------------------------------------------------
-- users
-- ---------------------------------------------------------------------------
-- password_hash is nullable to support future SSO/OAuth flows where no local
-- password is set. employee_id is nullable: a tenant admin user may exist
-- without a corresponding employee record (e.g. external payroll staff).
CREATE TABLE users (
    id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES tenants(id),
    email           text        NOT NULL,
    password_hash   text,
    employee_id     uuid        REFERENCES employees(id),
    status          text        NOT NULL DEFAULT 'active',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_users PRIMARY KEY (id),
    CONSTRAINT uq_users_tenant_email UNIQUE (tenant_id, email)
);

CREATE INDEX idx_users_tenant_id ON users (tenant_id);
CREATE INDEX idx_users_employee_id ON users (employee_id);

-- ---------------------------------------------------------------------------
-- audit_logs
-- ---------------------------------------------------------------------------
-- PII and secrets must never be stored in audit_logs.
-- occurred_at is NOT updated_at-style — once written, rows are immutable.
-- prev_hash/hash columns for tamper detection will be added in P1.3.
CREATE TABLE audit_logs (
    id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES tenants(id),
    user_id         uuid        REFERENCES users(id),
    action          text        NOT NULL,
    resource_type   text        NOT NULL,
    resource_id     uuid,
    ip              inet,
    occurred_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_audit_logs PRIMARY KEY (id)
);

CREATE INDEX idx_audit_logs_tenant_id ON audit_logs (tenant_id);
CREATE INDEX idx_audit_logs_occurred_at ON audit_logs (tenant_id, occurred_at DESC);
CREATE INDEX idx_audit_logs_resource ON audit_logs (tenant_id, resource_type, resource_id);

-- ---------------------------------------------------------------------------
-- RLS: enable row-level security on all tenant-scoped tables
-- ---------------------------------------------------------------------------
-- FORCE ROW LEVEL SECURITY ensures the policy applies even to the table owner.
-- ENABLE ROW LEVEL SECURITY alone would allow the owner to bypass it.
--
-- The policy uses NULLIF(current_setting('app.tenant_id', true), '')::uuid:
--   - current_setting('app.tenant_id', true): the second argument `true`
--     (missing_ok) prevents an error when the GUC key is not registered.
--     However, PostgreSQL returns '' (empty string) rather than NULL when the
--     setting exists but has not been given a value in this session.
--   - NULLIF(..., '') converts '' to NULL before the ::uuid cast, preventing
--     an "invalid input syntax for type uuid" error on an unset context.
--   - NULL::uuid never equals any row's tenant_id, so an unset context
--     produces 0 rows — fail-closed / deny-by-default.
--
-- app.tenant_id MUST be set via SET LOCAL inside every transaction that
-- touches tenant-scoped data. See internal/platform/tenantdb/tenantdb.go.

ALTER TABLE departments  ENABLE ROW LEVEL SECURITY;
ALTER TABLE departments  FORCE ROW LEVEL SECURITY;
ALTER TABLE roles        ENABLE ROW LEVEL SECURITY;
ALTER TABLE roles        FORCE ROW LEVEL SECURITY;
ALTER TABLE employees    ENABLE ROW LEVEL SECURITY;
ALTER TABLE employees    FORCE ROW LEVEL SECURITY;
ALTER TABLE users        ENABLE ROW LEVEL SECURITY;
ALTER TABLE users        FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_logs   ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_logs   FORCE ROW LEVEL SECURITY;

-- tenants itself also has RLS so that one tenant cannot read or mutate
-- another tenant's row even when a bug exposes the raw tenants table.
ALTER TABLE tenants      ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenants      FORCE ROW LEVEL SECURITY;

-- Policies for tenant-scoped tables (departments, roles, employees, users,
-- audit_logs): filter/check by tenant_id column.
CREATE POLICY tenant_isolation ON departments
    USING       (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK  (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON roles
    USING       (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK  (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON employees
    USING       (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK  (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON users
    USING       (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK  (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON audit_logs
    USING       (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK  (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Policy for tenants itself: the row's own id must match the active tenant.
-- This prevents a tenant from reading or modifying sibling tenant rows.
CREATE POLICY tenant_isolation ON tenants
    USING       (id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK  (id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to the application role (hr_app)
-- hr_app is created outside migrations (see db/init/10-create-app-role.sh)
-- to avoid storing the password in the repository.
-- ---------------------------------------------------------------------------
GRANT USAGE ON SCHEMA public TO hr_app;

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE tenants     TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE departments TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE roles       TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE employees   TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE users       TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE audit_logs  TO hr_app;

-- goose tracks its own schema_migrations table; hr_app does not need access.

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE tenants, departments, roles, employees, users, audit_logs FROM hr_app;
REVOKE USAGE ON SCHEMA public FROM hr_app;

DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS employees;
DROP TABLE IF EXISTS roles;
DROP TABLE IF EXISTS departments;
DROP TABLE IF EXISTS tenants;

-- +goose StatementEnd
