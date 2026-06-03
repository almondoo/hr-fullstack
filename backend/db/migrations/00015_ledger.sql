-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- ST-LM-10  法定三帳簿(労働者名簿・賃金台帳・出勤簿)生成と保存年限管理 + 給与SaaS連携
-- ===========================================================================
-- This migration creates the statutory three-ledger (法定三帳簿) tables plus a
-- per-tenant retention settings table and a payroll-SaaS import-link table.
--
-- LEGAL NOTE (法令値の設定化):
--   Retention periods (原則5年 / 経過措置3年), retention-basis rules (起算日:
--   退職日/最終記入日/最終出勤日), statutory-record-item templates, and the
--   electronic-storage policy (電子帳簿保存法: 真実性・可視性) are NEVER hardcoded.
--   They are held in ledger_settings (per tenant, JSONB / configurable columns)
--   so they can follow legal amendments and transitional measures (経過措置).
--   These values require confirmation by a certified social-insurance/labour
--   consultant (社労士) or lawyer.  This implementation is NOT legal advice.
--
-- Design (Gate0 決定 / 05 §2.5): the three ledgers are kept as SEPARATE tables
-- (worker_rosters / wage_ledgers / attendance_books), NOT one polymorphic table.
--
-- Cross-package references:
--   - employees(id, tenant_id): composite FK (existing stable table, 00004).
--   - employee_assignments / employment_contracts (00004) and
--     attendance_records / work_summaries (00005) are READ by the builders in
--     the service layer; no FK is declared here (they are read sources only).
--   - payroll_links.id is referenced from wage_ledgers.source_payroll_link_id
--     via an OWN-PACKAGE composite FK (both tables live in this migration).

-- ---------------------------------------------------------------------------
-- ledger_settings (テナント別 保存年限・起算日・電子保存方針の設定)
-- ---------------------------------------------------------------------------
-- One row per tenant.  Holds legally-configurable values so the application
-- never hardcodes them (CMP-001 / NFR-011).
--   - default_retention_years: 原則5 / 経過措置3 (configurable; follows 改正).
--   - default_retention_basis: 起算日種別の既定 ('resignation'|'last_entry'|'last_attendance').
--   - electronic_storage_json: 電子帳簿保存法に沿った真実性・可視性の保存方針
--     (確定後改変ルール・出力形式 等) を JSONB で保持。
CREATE TABLE ledger_settings (
    id                       uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id                uuid        NOT NULL REFERENCES tenants(id),
    -- default_retention_years: statutory retention period in years.
    -- 原則5年 / 経過措置3年.  Configurable to follow legal amendments.
    default_retention_years  integer     NOT NULL DEFAULT 5,
    -- default_retention_basis: 起算日種別の既定値.
    --   resignation     = 退職日 (worker roster)
    --   last_entry      = 最終記入日 (wage ledger)
    --   last_attendance = 最終出勤日 (attendance book)
    default_retention_basis  text        NOT NULL DEFAULT 'resignation',
    -- electronic_storage_json: 電子保存方針 (真実性・可視性).
    -- Format example: {"immutable_after_finalize":true,"export_formats":["csv","pdf"]}
    electronic_storage_json  jsonb       NOT NULL DEFAULT '{}',
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_ledger_settings PRIMARY KEY (id),
    CONSTRAINT chk_ledger_settings_basis
        CHECK (default_retention_basis IN ('resignation', 'last_entry', 'last_attendance')),
    CONSTRAINT chk_ledger_settings_years
        CHECK (default_retention_years > 0),
    -- One settings row per tenant.
    CONSTRAINT uq_ledger_settings_tenant UNIQUE (tenant_id),
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_ledger_settings_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_ledger_settings_lookup
    ON ledger_settings (tenant_id);

-- ---------------------------------------------------------------------------
-- payroll_links (給与SaaS 連携取込レコード — 賃金台帳の生成元)
-- ---------------------------------------------------------------------------
-- Records each import from a payroll SaaS adapter (moneyforward/freee/yayoi).
-- provider_ref: opaque provider-side reference ID only.  NO card numbers / PANs
-- / raw tokens are ever received or stored (mock payment / adapter abstraction).
-- imported_payload_json: the normalised wage data taken from the SaaS, used as
-- the source for building wage_ledgers.  Idempotent import keyed by
-- (employee_id, tenant_id, provider, period).
CREATE TABLE payroll_links (
    id                     uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id              uuid        NOT NULL REFERENCES tenants(id),
    employee_id            uuid        NOT NULL,
    -- provider: payroll SaaS adapter identifier.
    provider               text        NOT NULL,
    -- period: 賃金計算期間 (e.g. "2026-06" or "2026-06-01/2026-06-30").
    period                 text        NOT NULL,
    -- provider_ref: opaque provider-side import reference (NOT a token/PAN).
    provider_ref           text        NOT NULL DEFAULT '',
    -- imported_payload_json: normalised wage data from the SaaS (no PAN/token).
    imported_payload_json  jsonb       NOT NULL DEFAULT '{}',
    -- status: import lifecycle.  imported -> consumed (after a wage ledger built).
    status                 text        NOT NULL DEFAULT 'imported',
    imported_at            timestamptz NOT NULL DEFAULT now(),
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_payroll_links PRIMARY KEY (id),
    CONSTRAINT chk_payroll_links_provider
        CHECK (provider IN ('moneyforward', 'freee', 'yayoi', 'mock')),
    CONSTRAINT chk_payroll_links_status
        CHECK (status IN ('imported', 'consumed')),
    -- [Security] Composite FK: (employee_id, tenant_id) must exist in employees.
    -- Prevents cross-tenant import-link creation.
    CONSTRAINT fk_payroll_links_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- Idempotent import: one link per (employee, provider, period) per tenant.
    CONSTRAINT uq_payroll_links_emp_provider_period
        UNIQUE (employee_id, tenant_id, provider, period),
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_payroll_links_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_payroll_links_lookup
    ON payroll_links (tenant_id, employee_id, provider, period);

-- ---------------------------------------------------------------------------
-- worker_rosters (労働者名簿 LM-054)
-- ---------------------------------------------------------------------------
-- One roster per employee per tenant.  roster_json holds the statutory items
-- (氏名・生年月日・履歴・従事業務・雇入/退職年月日 等) assembled by the builder
-- from employees / employee_assignments / employment_contracts.
--   - retention_basis: 起算日種別 (typically 'resignation' = 退職日).
--   - retention_until: 保存満了日 = 起算日 + retention_years (設定値より算定).
--   - finalized_at:    真実性確保 (確定).  NULL until finalized; once set the
--                      record is immutable except for explicit amendment history.
CREATE TABLE worker_rosters (
    id                uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id         uuid        NOT NULL REFERENCES tenants(id),
    employee_id       uuid        NOT NULL,
    -- roster_json: 法定記載事項 (氏名/生年月日/履歴/従事業務/雇入退職年月日 等).
    roster_json       jsonb       NOT NULL DEFAULT '{}',
    -- retention_basis: 起算日種別.
    retention_basis   text        NOT NULL DEFAULT 'resignation',
    -- retention_basis_date: the actual 起算日 used to compute retention_until.
    retention_basis_date date,
    -- retention_until: 保存満了日 (起算日 + retention_years).  NULL = not yet set.
    retention_until   date,
    -- finalized_at: 確定 (真実性).  Set on finalize; blocks further mutation.
    finalized_at      timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_worker_rosters PRIMARY KEY (id),
    CONSTRAINT chk_worker_rosters_basis
        CHECK (retention_basis IN ('resignation', 'last_entry', 'last_attendance')),
    -- [Security] Composite FK: (employee_id, tenant_id) must exist in employees.
    CONSTRAINT fk_worker_rosters_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- One roster per employee per tenant.
    CONSTRAINT uq_worker_rosters_employee_tenant UNIQUE (employee_id, tenant_id),
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_worker_rosters_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_worker_rosters_lookup
    ON worker_rosters (tenant_id, employee_id, retention_until);

-- ---------------------------------------------------------------------------
-- wage_ledgers (賃金台帳 LM-054 / LM-050 / INT-002)
-- ---------------------------------------------------------------------------
-- One ledger per (employee, 賃金計算期間) per tenant.  wage_json holds the
-- statutory items (賃金計算期間・労働日数/時間・基本給/手当/割増/控除 等),
-- normalised from a payroll_links import.
--   - source_payroll_link_id: own-package composite FK to payroll_links.
--   - retention_basis: typically 'last_entry' (最終記入日).
CREATE TABLE wage_ledgers (
    id                     uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id              uuid        NOT NULL REFERENCES tenants(id),
    employee_id            uuid        NOT NULL,
    -- period: 賃金計算期間 (e.g. "2026-06").
    period                 text        NOT NULL,
    -- wage_json: 法定記載事項 (基本給/手当/割増/控除 等).  No PAN/token.
    wage_json              jsonb       NOT NULL DEFAULT '{}',
    -- source_payroll_link_id: the payroll import this ledger was built from.
    source_payroll_link_id uuid,
    retention_basis        text        NOT NULL DEFAULT 'last_entry',
    retention_basis_date   date,
    retention_until        date,
    finalized_at           timestamptz,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_wage_ledgers PRIMARY KEY (id),
    CONSTRAINT chk_wage_ledgers_basis
        CHECK (retention_basis IN ('resignation', 'last_entry', 'last_attendance')),
    -- [Security] Composite FK: (employee_id, tenant_id) must exist in employees.
    CONSTRAINT fk_wage_ledgers_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- Own-package composite FK: (source_payroll_link_id, tenant_id) -> payroll_links.
    CONSTRAINT fk_wage_ledgers_payroll_link_tenant
        FOREIGN KEY (source_payroll_link_id, tenant_id)
        REFERENCES payroll_links(id, tenant_id)
        MATCH SIMPLE,
    -- One ledger per (employee, period) per tenant.
    CONSTRAINT uq_wage_ledgers_emp_period UNIQUE (employee_id, tenant_id, period),
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_wage_ledgers_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_wage_ledgers_lookup
    ON wage_ledgers (tenant_id, employee_id, period);

-- ---------------------------------------------------------------------------
-- attendance_books (出勤簿 LM-054 / LM-031〜033 連携)
-- ---------------------------------------------------------------------------
-- One book per (employee, 対象月) per tenant.  book_json holds the statutory
-- items (労働日数/労働時間/始業終業/休憩/時間外/休日/深夜) assembled by the builder
-- from attendance_records / work_summaries.
--   - retention_basis: typically 'last_attendance' (最終出勤日).
CREATE TABLE attendance_books (
    id                   uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id            uuid        NOT NULL REFERENCES tenants(id),
    employee_id          uuid        NOT NULL,
    -- period_month: 対象月 (e.g. "2026-06").
    period_month         text        NOT NULL,
    -- book_json: 法定記載事項 (労働日数/労働時間/始業終業/休憩/時間外/休日/深夜).
    book_json            jsonb       NOT NULL DEFAULT '{}',
    retention_basis      text        NOT NULL DEFAULT 'last_attendance',
    retention_basis_date date,
    retention_until      date,
    finalized_at         timestamptz,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_attendance_books PRIMARY KEY (id),
    CONSTRAINT chk_attendance_books_basis
        CHECK (retention_basis IN ('resignation', 'last_entry', 'last_attendance')),
    -- [Security] Composite FK: (employee_id, tenant_id) must exist in employees.
    CONSTRAINT fk_attendance_books_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- One book per (employee, period_month) per tenant.
    CONSTRAINT uq_attendance_books_emp_month UNIQUE (employee_id, tenant_id, period_month),
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_attendance_books_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_attendance_books_lookup
    ON attendance_books (tenant_id, employee_id, period_month);

-- ---------------------------------------------------------------------------
-- 真実性 (immutability) enforcement — BEFORE UPDATE trigger
-- ---------------------------------------------------------------------------
-- [Security / 電子帳簿保存法] Defence-in-depth for the finalize-immutability
-- invariant.  The service layer guards rebuilds with a prior SELECT finalized_at
-- check, but under READ COMMITTED that check is a TOCTOU race: a concurrent
-- Finalize (T1) can commit between a builder's SELECT and its ON CONFLICT DO
-- UPDATE (T2), letting T2 silently rewrite a now-finalized row.
--
-- This trigger closes the race at the DB level: once finalized_at is set
-- (OLD.finalized_at IS NOT NULL), ANY UPDATE that changes a business column
-- (the *_json payload or the retention_* fields, or finalized_at itself) is
-- rejected.  The ONLY permitted mutation of a finalized row is a pure
-- updated_at bump (no other column changes), so housekeeping touches remain
-- possible without weakening immutability.  The finalize transition itself is
-- always allowed because at that moment OLD.finalized_at IS NULL (the guard
-- only triggers AFTER the row is finalized).  An explicit amendment-history
-- path would be a separate, audited mechanism — never a silent in-place rewrite.
CREATE OR REPLACE FUNCTION ledger_block_finalized_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    -- Only finalized rows are protected; non-finalized rows mutate freely.
    IF OLD.finalized_at IS NULL THEN
        RETURN NEW;
    END IF;

    -- A finalized row may NOT change finalized_at (no un-finalize / re-finalize).
    IF NEW.finalized_at IS DISTINCT FROM OLD.finalized_at THEN
        RAISE EXCEPTION 'ledger: finalized record is immutable (真実性): % id=%',
            TG_TABLE_NAME, OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    -- Shared retention columns must not change on a finalized row.
    IF NEW.retention_basis IS DISTINCT FROM OLD.retention_basis
        OR NEW.retention_basis_date IS DISTINCT FROM OLD.retention_basis_date
        OR NEW.retention_until IS DISTINCT FROM OLD.retention_until THEN
        RAISE EXCEPTION 'ledger: finalized record is immutable (真実性): % id=%',
            TG_TABLE_NAME, OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    -- Per-table statutory payload column must not change on a finalized row.
    IF TG_TABLE_NAME = 'worker_rosters' THEN
        IF NEW.roster_json IS DISTINCT FROM OLD.roster_json THEN
            RAISE EXCEPTION 'ledger: finalized record is immutable (真実性): % id=%',
                TG_TABLE_NAME, OLD.id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    ELSIF TG_TABLE_NAME = 'wage_ledgers' THEN
        IF NEW.wage_json IS DISTINCT FROM OLD.wage_json
            OR NEW.source_payroll_link_id IS DISTINCT FROM OLD.source_payroll_link_id THEN
            RAISE EXCEPTION 'ledger: finalized record is immutable (真実性): % id=%',
                TG_TABLE_NAME, OLD.id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    ELSIF TG_TABLE_NAME = 'attendance_books' THEN
        IF NEW.book_json IS DISTINCT FROM OLD.book_json THEN
            RAISE EXCEPTION 'ledger: finalized record is immutable (真実性): % id=%',
                TG_TABLE_NAME, OLD.id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_worker_rosters_block_finalized
    BEFORE UPDATE ON worker_rosters
    FOR EACH ROW EXECUTE FUNCTION ledger_block_finalized_mutation();

CREATE TRIGGER trg_wage_ledgers_block_finalized
    BEFORE UPDATE ON wage_ledgers
    FOR EACH ROW EXECUTE FUNCTION ledger_block_finalized_mutation();

CREATE TRIGGER trg_attendance_books_block_finalized
    BEFORE UPDATE ON attendance_books
    FOR EACH ROW EXECUTE FUNCTION ledger_block_finalized_mutation();

-- ---------------------------------------------------------------------------
-- RLS — all new tables (テナント分離)
-- ---------------------------------------------------------------------------
ALTER TABLE ledger_settings   ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_settings   FORCE  ROW LEVEL SECURITY;
ALTER TABLE payroll_links     ENABLE ROW LEVEL SECURITY;
ALTER TABLE payroll_links     FORCE  ROW LEVEL SECURITY;
ALTER TABLE worker_rosters    ENABLE ROW LEVEL SECURITY;
ALTER TABLE worker_rosters    FORCE  ROW LEVEL SECURITY;
ALTER TABLE wage_ledgers      ENABLE ROW LEVEL SECURITY;
ALTER TABLE wage_ledgers      FORCE  ROW LEVEL SECURITY;
ALTER TABLE attendance_books  ENABLE ROW LEVEL SECURITY;
ALTER TABLE attendance_books  FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON ledger_settings
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON payroll_links
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON worker_rosters
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON wage_ledgers
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON attendance_books
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to hr_app
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE ledger_settings   TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE payroll_links     TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE worker_rosters    TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE wage_ledgers      TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE attendance_books  TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TRIGGER IF EXISTS trg_attendance_books_block_finalized ON attendance_books;
DROP TRIGGER IF EXISTS trg_wage_ledgers_block_finalized     ON wage_ledgers;
DROP TRIGGER IF EXISTS trg_worker_rosters_block_finalized   ON worker_rosters;
DROP FUNCTION IF EXISTS ledger_block_finalized_mutation();

REVOKE ALL ON TABLE attendance_books  FROM hr_app;
REVOKE ALL ON TABLE wage_ledgers      FROM hr_app;
REVOKE ALL ON TABLE worker_rosters    FROM hr_app;
REVOKE ALL ON TABLE payroll_links     FROM hr_app;
REVOKE ALL ON TABLE ledger_settings   FROM hr_app;

DROP TABLE IF EXISTS attendance_books;
DROP TABLE IF EXISTS wage_ledgers;
DROP TABLE IF EXISTS worker_rosters;
DROP TABLE IF EXISTS payroll_links;
DROP TABLE IF EXISTS ledger_settings;

-- +goose StatementEnd
