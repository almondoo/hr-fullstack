-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- LM-051  年末調整 (年末調整アンケート収集 / 計算 / 帳票 / 給与SaaS連携足場)
-- ===========================================================================
-- This migration creates the year-end-adjustment (年末調整) tables:
--
--   yearend_settings       — per-tenant legal-year settings (非課税限度額等は設定で管理)
--   yearend_submissions    — employee deduction declarations (控除申告収集)
--   yearend_calculations   — tax calculation results (計算結果)
--   yearend_reports        — report records (帳票生成記録; 源泉徴収票等)
--   yearend_payroll_pushes — payroll-SaaS push status (給与SaaS連携足場)
--
-- Security / compliance:
--   - Sensitive fields (扶養親族情報, 保険料控除, 住宅借入金等) are stored ONLY in
--     encrypted JSON columns (declaration_enc bytea).  Plaintext is NEVER stored
--     in non-encrypted columns, logs, audit resource_ids, or API responses.
--   - RLS (ENABLE + FORCE + fail-closed policy) enforces tenant isolation on all
--     tables.
--   - Composite FK (employee_id, tenant_id) → employees prevents cross-tenant
--     row creation (テナント分離 / Cross-story FK hardening).
--
-- LEGAL NOTE:
--   Deduction limits, rate tables, and form definitions are CONFIGURABLE in
--   yearend_settings so they can follow annual tax-law revisions.  The values in
--   this schema are structural defaults only and MUST be confirmed against the
--   current 国税庁 guidance by a tax accountant / 社労士 before production use.
--   This implementation is NOT legal advice.
--
-- Relation to ledger (法定三帳簿):
--   Once the year-end adjustment is finalised the result feeds the 源泉徴収票,
--   which is a separate mandatory document (ST-LM-10 / LM-051).  The yearend
--   domain is intentionally kept separate from the statutory three-ledger domain
--   (ledger package) to respect distinct legal retention rules and workflows.

-- ---------------------------------------------------------------------------
-- yearend_settings (テナント別 年末調整設定)
-- ---------------------------------------------------------------------------
-- One row per tenant.  Holds legally-configurable values (税率表・控除限度額 etc.)
-- so the application never hardcodes them.  The json columns hold the annual
-- tax-rate tables and deduction ceilings; these MUST be updated each year.
CREATE TABLE yearend_settings (
    id                      uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id               uuid        NOT NULL REFERENCES tenants(id),
    -- tax_year: 対象年 (e.g. 2026).
    tax_year                integer     NOT NULL,
    -- rate_table_json: 税率表 (所得税速算表 / 復興特別所得税率 等) as JSONB.
    -- MUST be updated each year per 国税庁 guidance.
    rate_table_json         jsonb       NOT NULL DEFAULT '{}',
    -- deduction_limits_json: 控除限度額 (配偶者控除・扶養控除・基礎控除上限 等) as JSONB.
    deduction_limits_json   jsonb       NOT NULL DEFAULT '{}',
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_yearend_settings PRIMARY KEY (id),
    CONSTRAINT chk_yearend_settings_year CHECK (tax_year >= 2000 AND tax_year <= 2100),
    -- One settings row per (tenant, tax_year).
    CONSTRAINT uq_yearend_settings_tenant_year UNIQUE (tenant_id, tax_year),
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_yearend_settings_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_yearend_settings_lookup
    ON yearend_settings (tenant_id, tax_year);

-- ---------------------------------------------------------------------------
-- yearend_submissions (控除申告収集)
-- ---------------------------------------------------------------------------
-- One row per (employee, tax_year) per tenant.  Captures the employee's
-- declaration data for the year-end adjustment questionnaire.
--
-- Security:
--   - declaration_enc: AES-256-GCM ciphertext of the entire deduction
--     declaration (扶養親族情報, 保険料控除額, 住宅借入金等特別控除 etc.).
--     Plaintext is NEVER stored outside this encrypted column.
--   - declaration_hash: SHA-256 of the plaintext for integrity checks
--     (改竄検知).  Does NOT reveal the plaintext.
--   - The encrypted column requires the yearend:reveal permission to decrypt
--     (enforced in the service layer, defence-in-depth).
CREATE TABLE yearend_submissions (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    employee_id         uuid        NOT NULL,
    -- tax_year: 対象年.
    tax_year            integer     NOT NULL,
    -- status: submission lifecycle.
    --   draft     → submitted → locked (after calculation finalised)
    status              text        NOT NULL DEFAULT 'draft',
    -- declaration_enc: AES-256-GCM encrypted declaration payload (bytea).
    -- Contains: 扶養親族, 保険料控除, 住宅借入金等.  Plaintext never stored elsewhere.
    declaration_enc     bytea,
    -- declaration_hash: SHA-256 of the plaintext for integrity (改竄検知).
    -- Does NOT contain or reveal the plaintext.
    declaration_hash    text,
    -- submitted_at: timestamp when the employee submitted the declaration.
    submitted_at        timestamptz,
    -- locked_at: timestamp when the record was locked (after final calculation).
    locked_at           timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_yearend_submissions PRIMARY KEY (id),
    CONSTRAINT chk_yearend_submissions_status
        CHECK (status IN ('draft', 'submitted', 'locked')),
    CONSTRAINT chk_yearend_submissions_year CHECK (tax_year >= 2000 AND tax_year <= 2100),
    -- [Security] Composite FK: (employee_id, tenant_id) must exist in employees.
    CONSTRAINT fk_yearend_submissions_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- One submission per (employee, tax_year) per tenant.
    CONSTRAINT uq_yearend_submissions_emp_year UNIQUE (employee_id, tenant_id, tax_year),
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_yearend_submissions_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_yearend_submissions_lookup
    ON yearend_submissions (tenant_id, employee_id, tax_year);
CREATE INDEX idx_yearend_submissions_status
    ON yearend_submissions (tenant_id, tax_year, status);

-- ---------------------------------------------------------------------------
-- yearend_calculations (計算結果)
-- ---------------------------------------------------------------------------
-- One row per (employee, tax_year) per tenant.  Stores the computed year-end
-- adjustment result: 課税所得, 年税額, 過不足税額 etc.
-- All monetary amounts are stored as integers (円単位).
--
-- Security: result_json holds calculated amounts only (no PII / decrypted
-- declaration data).  The source submission_id links back to the encrypted
-- submission but the calculation result itself contains no plaintext PII.
CREATE TABLE yearend_calculations (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    employee_id         uuid        NOT NULL,
    tax_year            integer     NOT NULL,
    -- submission_id: source submission used for this calculation.
    submission_id       uuid        NOT NULL,
    -- status: calculation lifecycle.
    --   pending → completed → finalised
    status              text        NOT NULL DEFAULT 'pending',
    -- result_json: calculation results (課税所得/年税額/過不足税額 etc.).
    -- Contains amounts only; NO decrypted PII from the submission.
    result_json         jsonb       NOT NULL DEFAULT '{}',
    -- calculated_at: when the calculation was last run.
    calculated_at       timestamptz,
    -- finalised_at: when the result was finalised (immutable after this).
    finalised_at        timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_yearend_calculations PRIMARY KEY (id),
    CONSTRAINT chk_yearend_calculations_status
        CHECK (status IN ('pending', 'completed', 'finalised')),
    CONSTRAINT chk_yearend_calculations_year CHECK (tax_year >= 2000 AND tax_year <= 2100),
    -- [Security] Composite FK: (employee_id, tenant_id) must exist in employees.
    CONSTRAINT fk_yearend_calculations_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- Own-package composite FK: (submission_id, tenant_id) → yearend_submissions.
    CONSTRAINT fk_yearend_calculations_submission_tenant
        FOREIGN KEY (submission_id, tenant_id)
        REFERENCES yearend_submissions(id, tenant_id)
        MATCH SIMPLE,
    -- One calculation record per (employee, tax_year) per tenant.
    CONSTRAINT uq_yearend_calculations_emp_year UNIQUE (employee_id, tenant_id, tax_year),
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_yearend_calculations_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_yearend_calculations_lookup
    ON yearend_calculations (tenant_id, employee_id, tax_year);
CREATE INDEX idx_yearend_calculations_status
    ON yearend_calculations (tenant_id, tax_year, status);

-- ---------------------------------------------------------------------------
-- yearend_reports (帳票生成記録)
-- ---------------------------------------------------------------------------
-- Records each generated report (源泉徴収票 / 法定調書合計表 etc.).
-- Actual report content is stored externally (e.g. object storage); only the
-- metadata and an opaque content_ref are stored here.
CREATE TABLE yearend_reports (
    id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES tenants(id),
    employee_id     uuid,
    tax_year        integer     NOT NULL,
    -- report_type: type of report generated.
    --   withholding_slip  = 源泉徴収票 (per-employee)
    --   summary_return    = 法定調書合計表 (per-tenant)
    report_type     text        NOT NULL,
    -- calc_id: the calculation this report was generated from (nullable for
    -- summary-level reports that span multiple employees).
    calc_id         uuid,
    -- content_ref: opaque reference to the stored report content
    -- (e.g. object-storage key).  The actual file is NOT stored in the DB.
    content_ref     text,
    -- format: output format (csv | pdf).
    format          text        NOT NULL DEFAULT 'csv',
    -- generated_at: when this report was generated.
    generated_at    timestamptz NOT NULL DEFAULT now(),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_yearend_reports PRIMARY KEY (id),
    CONSTRAINT chk_yearend_reports_type
        CHECK (report_type IN ('withholding_slip', 'summary_return')),
    CONSTRAINT chk_yearend_reports_format
        CHECK (format IN ('csv', 'pdf')),
    CONSTRAINT chk_yearend_reports_year CHECK (tax_year >= 2000 AND tax_year <= 2100),
    -- [Security] Composite FK for per-employee reports: employee must belong to this tenant.
    CONSTRAINT fk_yearend_reports_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- Own-package composite FK: (calc_id, tenant_id) → yearend_calculations.
    CONSTRAINT fk_yearend_reports_calc_tenant
        FOREIGN KEY (calc_id, tenant_id)
        REFERENCES yearend_calculations(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_yearend_reports_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_yearend_reports_lookup
    ON yearend_reports (tenant_id, tax_year, report_type);

-- ---------------------------------------------------------------------------
-- yearend_payroll_pushes (給与SaaS連携足場)
-- ---------------------------------------------------------------------------
-- Records each attempt to push the year-end adjustment result to a payroll SaaS
-- system.  The actual push is performed by the PayrollPusher adapter (stub in
-- MVP; real integration requires external credentials — P3).
-- provider_ref: opaque provider-side reference.  No credentials / tokens stored.
CREATE TABLE yearend_payroll_pushes (
    id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES tenants(id),
    employee_id     uuid        NOT NULL,
    tax_year        integer     NOT NULL,
    calc_id         uuid        NOT NULL,
    -- provider: payroll SaaS identifier (mirrors ledger.payroll_links providers).
    provider        text        NOT NULL,
    -- status: push lifecycle.
    --   pending → pushed → failed
    status          text        NOT NULL DEFAULT 'pending',
    -- provider_ref: opaque provider-side reference (NOT a credential / token).
    provider_ref    text        NOT NULL DEFAULT '',
    -- pushed_at: when the push succeeded.
    pushed_at       timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_yearend_payroll_pushes PRIMARY KEY (id),
    CONSTRAINT chk_yearend_payroll_pushes_provider
        CHECK (provider IN ('moneyforward', 'freee', 'yayoi', 'mock')),
    CONSTRAINT chk_yearend_payroll_pushes_status
        CHECK (status IN ('pending', 'pushed', 'failed')),
    CONSTRAINT chk_yearend_payroll_pushes_year CHECK (tax_year >= 2000 AND tax_year <= 2100),
    -- [Security] Composite FK: (employee_id, tenant_id) must exist in employees.
    CONSTRAINT fk_yearend_payroll_pushes_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- Own-package composite FK: (calc_id, tenant_id) → yearend_calculations.
    CONSTRAINT fk_yearend_payroll_pushes_calc_tenant
        FOREIGN KEY (calc_id, tenant_id)
        REFERENCES yearend_calculations(id, tenant_id)
        MATCH SIMPLE,
    -- Idempotent: one push record per (employee, tax_year, provider) per tenant.
    CONSTRAINT uq_yearend_payroll_pushes_emp_year_provider
        UNIQUE (employee_id, tenant_id, tax_year, provider),
    CONSTRAINT uq_yearend_payroll_pushes_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_yearend_payroll_pushes_lookup
    ON yearend_payroll_pushes (tenant_id, tax_year, status);

-- ---------------------------------------------------------------------------
-- Immutability trigger for yearend_submissions (locked rows)
-- ---------------------------------------------------------------------------
-- Once locked_at is set, the declaration_enc / declaration_hash must not change.
CREATE OR REPLACE FUNCTION yearend_block_locked_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF OLD.locked_at IS NULL THEN
        RETURN NEW;
    END IF;
    -- A locked submission may not change its declaration data or status.
    IF NEW.declaration_enc IS DISTINCT FROM OLD.declaration_enc
        OR NEW.declaration_hash IS DISTINCT FROM OLD.declaration_hash
        OR NEW.status IS DISTINCT FROM OLD.status THEN
        RAISE EXCEPTION 'yearend: locked submission is immutable: id=%', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_yearend_submissions_block_locked
    BEFORE UPDATE ON yearend_submissions
    FOR EACH ROW EXECUTE FUNCTION yearend_block_locked_mutation();

-- ---------------------------------------------------------------------------
-- RLS — all new tables (テナント分離)
-- ---------------------------------------------------------------------------
ALTER TABLE yearend_settings        ENABLE ROW LEVEL SECURITY;
ALTER TABLE yearend_settings        FORCE  ROW LEVEL SECURITY;
ALTER TABLE yearend_submissions     ENABLE ROW LEVEL SECURITY;
ALTER TABLE yearend_submissions     FORCE  ROW LEVEL SECURITY;
ALTER TABLE yearend_calculations    ENABLE ROW LEVEL SECURITY;
ALTER TABLE yearend_calculations    FORCE  ROW LEVEL SECURITY;
ALTER TABLE yearend_reports         ENABLE ROW LEVEL SECURITY;
ALTER TABLE yearend_reports         FORCE  ROW LEVEL SECURITY;
ALTER TABLE yearend_payroll_pushes  ENABLE ROW LEVEL SECURITY;
ALTER TABLE yearend_payroll_pushes  FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON yearend_settings
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON yearend_submissions
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON yearend_calculations
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON yearend_reports
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON yearend_payroll_pushes
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to hr_app
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE yearend_settings        TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE yearend_submissions     TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE yearend_calculations    TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE yearend_reports         TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE yearend_payroll_pushes  TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TRIGGER IF EXISTS trg_yearend_submissions_block_locked ON yearend_submissions;
DROP FUNCTION IF EXISTS yearend_block_locked_mutation();

REVOKE ALL ON TABLE yearend_payroll_pushes  FROM hr_app;
REVOKE ALL ON TABLE yearend_reports         FROM hr_app;
REVOKE ALL ON TABLE yearend_calculations    FROM hr_app;
REVOKE ALL ON TABLE yearend_submissions     FROM hr_app;
REVOKE ALL ON TABLE yearend_settings        FROM hr_app;

DROP TABLE IF EXISTS yearend_payroll_pushes;
DROP TABLE IF EXISTS yearend_reports;
DROP TABLE IF EXISTS yearend_calculations;
DROP TABLE IF EXISTS yearend_submissions;
DROP TABLE IF EXISTS yearend_settings;

-- +goose StatementEnd
