-- +goose Up
-- +goose StatementBegin

-- ---------------------------------------------------------------------------
-- LEGAL NOTICE
-- ---------------------------------------------------------------------------
-- The default values for annual-leave grant tables, proportional-grant tables,
-- five-day obligation threshold, and expiry months below are based on Japanese
-- Labor Standards Law (労働基準法第39条) as of the migration authoring date
-- (2026-06-02).
-- Labor law is subject to revision.  All configurable values MUST be reviewed
-- by a qualified labor-law professional (社会保険労務士 / 弁護士) and kept
-- up to date with statutory amendments.  Do NOT treat these defaults as legal
-- advice or authoritative compliance thresholds.
--
-- Configurable columns in leave_settings (all per-tenant):
--   grant_table_json          — 勤続年数別付与日数表 (勤続月数 → 付与日数)
--   proportional_table_json   — 週所定労働日数別比例付与表
--   five_day_obligation_threshold — 5日取得義務の対象となる年間付与日数の閾値
--   expiry_months             — 年休時効月数 (法定24か月)
--   base_date_rule            — 基準日ルール ("hire_date_anniversary" or "fixed:04-01")
-- ---------------------------------------------------------------------------

-- ---------------------------------------------------------------------------
-- leave_settings (テナント別年休設定)
-- ---------------------------------------------------------------------------
-- One row per tenant.  Stores all configurable leave-law thresholds and
-- grant tables.  Administrators MUST update this after review by a qualified
-- professional.  The JSONB grant tables must be kept current with statutory
-- amendments — the defaults here are NOT legal advice.
--
-- grant_table_json format:
--   Array of {tenure_months_min: int, tenure_months_max: int | null, grant_days: numeric}
--   Example (労基法39条第1項 相当 — 要専門家確認):
--   [
--     {"tenure_months_min": 6,  "tenure_months_max": 17,  "grant_days": 10},
--     {"tenure_months_min": 18, "tenure_months_max": 29,  "grant_days": 11},
--     {"tenure_months_min": 30, "tenure_months_max": 41,  "grant_days": 12},
--     {"tenure_months_min": 42, "tenure_months_max": 53,  "grant_days": 14},
--     {"tenure_months_min": 54, "tenure_months_max": 65,  "grant_days": 16},
--     {"tenure_months_min": 66, "tenure_months_max": 77,  "grant_days": 18},
--     {"tenure_months_min": 78, "tenure_months_max": null, "grant_days": 20}
--   ]
--
-- proportional_table_json format:
--   Array of {weekly_days: numeric, entries: [{tenure_months_min, tenure_months_max, grant_days}]}
--   週所定4日以下・年216日以下の労働者への比例付与 (労基法39条第3項 相当 — 要専門家確認)
CREATE TABLE leave_settings (
    id                              uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id                       uuid        NOT NULL REFERENCES tenants(id),
    -- 基準日ルール: "hire_date_anniversary" (入社日起算) or "fixed:MM-DD" (統一基準日)
    base_date_rule                  text        NOT NULL DEFAULT 'hire_date_anniversary',
    -- 勤続年数別付与日数表 (JSONB; 要専門家確認・改正追従)
    grant_table_json                jsonb       NOT NULL DEFAULT
        '[{"tenure_months_min":6,"tenure_months_max":17,"grant_days":10},{"tenure_months_min":18,"tenure_months_max":29,"grant_days":11},{"tenure_months_min":30,"tenure_months_max":41,"grant_days":12},{"tenure_months_min":42,"tenure_months_max":53,"grant_days":14},{"tenure_months_min":54,"tenure_months_max":65,"grant_days":16},{"tenure_months_min":66,"tenure_months_max":77,"grant_days":18},{"tenure_months_min":78,"tenure_months_max":null,"grant_days":20}]',
    -- 週所定労働日数別比例付与表 (JSONB; 要専門家確認・改正追従)
    proportional_table_json         jsonb       NOT NULL DEFAULT
        '[{"weekly_days":4,"entries":[{"tenure_months_min":6,"tenure_months_max":17,"grant_days":7},{"tenure_months_min":18,"tenure_months_max":29,"grant_days":8},{"tenure_months_min":30,"tenure_months_max":41,"grant_days":9},{"tenure_months_min":42,"tenure_months_max":53,"grant_days":10},{"tenure_months_min":54,"tenure_months_max":65,"grant_days":12},{"tenure_months_min":66,"tenure_months_max":77,"grant_days":13},{"tenure_months_min":78,"tenure_months_max":null,"grant_days":15}]},{"weekly_days":3,"entries":[{"tenure_months_min":6,"tenure_months_max":17,"grant_days":5},{"tenure_months_min":18,"tenure_months_max":29,"grant_days":6},{"tenure_months_min":30,"tenure_months_max":41,"grant_days":6},{"tenure_months_min":42,"tenure_months_max":53,"grant_days":8},{"tenure_months_min":54,"tenure_months_max":65,"grant_days":9},{"tenure_months_min":66,"tenure_months_max":77,"grant_days":10},{"tenure_months_min":78,"tenure_months_max":null,"grant_days":11}]},{"weekly_days":2,"entries":[{"tenure_months_min":6,"tenure_months_max":17,"grant_days":3},{"tenure_months_min":18,"tenure_months_max":29,"grant_days":4},{"tenure_months_min":30,"tenure_months_max":41,"grant_days":4},{"tenure_months_min":42,"tenure_months_max":53,"grant_days":5},{"tenure_months_min":54,"tenure_months_max":65,"grant_days":6},{"tenure_months_min":66,"tenure_months_max":77,"grant_days":6},{"tenure_months_min":78,"tenure_months_max":null,"grant_days":7}]},{"weekly_days":1,"entries":[{"tenure_months_min":6,"tenure_months_max":17,"grant_days":1},{"tenure_months_min":18,"tenure_months_max":29,"grant_days":2},{"tenure_months_min":30,"tenure_months_max":41,"grant_days":2},{"tenure_months_min":42,"tenure_months_max":53,"grant_days":2},{"tenure_months_min":54,"tenure_months_max":65,"grant_days":3},{"tenure_months_min":66,"tenure_months_max":77,"grant_days":3},{"tenure_months_min":78,"tenure_months_max":null,"grant_days":3}]}]',
    -- 5日取得義務の対象閾値 (年間付与日数; 法定10日以上 — 要専門家確認・改正追従)
    five_day_obligation_threshold   int         NOT NULL DEFAULT 10,
    -- 年休時効月数 (法定24か月 — 要専門家確認・改正追従)
    expiry_months                   int         NOT NULL DEFAULT 24,
    updated_at                      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_leave_settings PRIMARY KEY (id),
    -- One row per tenant.
    CONSTRAINT uq_leave_settings_tenant UNIQUE (tenant_id)
);

-- ---------------------------------------------------------------------------
-- leave_grants (年休付与記録)
-- ---------------------------------------------------------------------------
-- Each row records one grant event for one employee.  Multiple rows may exist
-- per employee per year (carry-over + fresh grant).
-- expires_on: the date after which any remaining days from this grant expire
--   (grant_date + expiry_months months, computed at grant time from settings).
CREATE TABLE leave_grants (
    id           uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id    uuid        NOT NULL REFERENCES tenants(id),
    employee_id  uuid        NOT NULL,
    grant_date   date        NOT NULL,
    -- days: number of days granted (numeric to support proportional grants)
    days         numeric(6,1) NOT NULL,
    -- source: "annual_grant" | "proportional_grant" | "carry_over" | "manual"
    source       text        NOT NULL DEFAULT 'annual_grant',
    expires_on   date        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_leave_grants PRIMARY KEY (id),
    -- [Security] Composite FK: (employee_id, tenant_id) must exist in employees
    CONSTRAINT fk_leave_grants_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- UNIQUE(id, tenant_id) required for downstream composite FK references
    CONSTRAINT uq_leave_grants_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_leave_grants_lookup
    ON leave_grants (tenant_id, employee_id, grant_date);

-- ---------------------------------------------------------------------------
-- leave_requests (休暇申請)
-- ---------------------------------------------------------------------------
-- Covers all leave types; the leave_type column distinguishes them.
-- approval_request_id: FK into approval_requests when the leave has gone
-- through the approval engine; null when not yet submitted to approval.
CREATE TABLE leave_requests (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    employee_id         uuid        NOT NULL,
    -- leave_type: annual | special | condolence | maternity | childcare | care | absence
    leave_type          text        NOT NULL,
    start_date          date        NOT NULL,
    end_date            date        NOT NULL,
    -- days: calendar or business days consumed (numeric; may be fractional for half-days)
    days                numeric(6,1) NOT NULL,
    -- status: pending | approved | rejected | cancelled
    status              text        NOT NULL DEFAULT 'pending',
    -- approval_request_id: null until Submit() is called on the approval engine
    approval_request_id uuid,
    reason              text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_leave_requests PRIMARY KEY (id),
    -- [Security] Composite FK: (employee_id, tenant_id) must exist in employees
    CONSTRAINT fk_leave_requests_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- [Security] Composite FK: (approval_request_id, tenant_id) when set
    CONSTRAINT fk_leave_requests_approval_tenant
        FOREIGN KEY (approval_request_id, tenant_id)
        REFERENCES approval_requests(id, tenant_id)
        MATCH SIMPLE,
    -- UNIQUE(id, tenant_id) for downstream composite FK references
    CONSTRAINT uq_leave_requests_id_tenant UNIQUE (id, tenant_id),
    -- Prevent duplicate approved requests for the same employee and date range
    -- (partial index on approved rows only)
    CONSTRAINT chk_leave_requests_dates CHECK (end_date >= start_date)
);

CREATE INDEX idx_leave_requests_lookup
    ON leave_requests (tenant_id, employee_id, status, start_date);

CREATE INDEX idx_leave_requests_approval
    ON leave_requests (tenant_id, approval_request_id)
    WHERE approval_request_id IS NOT NULL;

-- Prevent duplicate approved requests for the same employee and date range.
-- The partial index covers only approved rows so pending/rejected/cancelled
-- duplicates (which are normal workflow states) are unaffected.
CREATE UNIQUE INDEX uix_leave_requests_approved_no_overlap
    ON leave_requests (tenant_id, employee_id, start_date, end_date)
    WHERE status = 'approved';

-- ---------------------------------------------------------------------------
-- leave_usages (年休消化記録)
-- ---------------------------------------------------------------------------
-- Links an approved leave_request back to the grant(s) it consumed.
-- This table provides the precise accounting needed to compute remaining days
-- and to verify the 5-day obligation, handling expiry correctly across grants.
CREATE TABLE leave_usages (
    id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES tenants(id),
    leave_request_id uuid       NOT NULL,
    leave_grant_id  uuid        NOT NULL,
    -- days_used: portion of this grant consumed by this request
    days_used       numeric(6,1) NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_leave_usages PRIMARY KEY (id),
    -- [Security] Composite FKs
    CONSTRAINT fk_leave_usages_request_tenant
        FOREIGN KEY (leave_request_id, tenant_id)
        REFERENCES leave_requests(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT fk_leave_usages_grant_tenant
        FOREIGN KEY (leave_grant_id, tenant_id)
        REFERENCES leave_grants(id, tenant_id)
        MATCH SIMPLE,
    -- One usage row per request+grant combination
    CONSTRAINT uq_leave_usages_request_grant UNIQUE (leave_request_id, leave_grant_id)
);

CREATE INDEX idx_leave_usages_by_request
    ON leave_usages (tenant_id, leave_request_id);

CREATE INDEX idx_leave_usages_by_grant
    ON leave_usages (tenant_id, leave_grant_id);

-- ---------------------------------------------------------------------------
-- RLS — all leave tables
-- ---------------------------------------------------------------------------
ALTER TABLE leave_settings  ENABLE ROW LEVEL SECURITY;
ALTER TABLE leave_settings  FORCE  ROW LEVEL SECURITY;
ALTER TABLE leave_grants    ENABLE ROW LEVEL SECURITY;
ALTER TABLE leave_grants    FORCE  ROW LEVEL SECURITY;
ALTER TABLE leave_requests  ENABLE ROW LEVEL SECURITY;
ALTER TABLE leave_requests  FORCE  ROW LEVEL SECURITY;
ALTER TABLE leave_usages    ENABLE ROW LEVEL SECURITY;
ALTER TABLE leave_usages    FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON leave_settings
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON leave_grants
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON leave_requests
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON leave_usages
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to hr_app
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE leave_settings  TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE leave_grants    TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE leave_requests  TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE leave_usages    TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE leave_usages   FROM hr_app;
REVOKE ALL ON TABLE leave_requests FROM hr_app;
REVOKE ALL ON TABLE leave_grants   FROM hr_app;
REVOKE ALL ON TABLE leave_settings FROM hr_app;

DROP INDEX IF EXISTS uix_leave_requests_approved_no_overlap;
DROP TABLE IF EXISTS leave_usages;
DROP TABLE IF EXISTS leave_requests;
DROP TABLE IF EXISTS leave_grants;
DROP TABLE IF EXISTS leave_settings;

-- +goose StatementEnd
