-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- ST-FND-11 — Standard reports / CSV/Excel export + company calendar /
--             work-pattern (shift) masters.
--
-- This migration provisions two foundational subsystems:
--
--   (1) Reporting / export:
--       - report_definitions: per-tenant activation + column selection for the
--         code-defined standard reports (employee roster, attendance monthly,
--         leave status, billing/seat summary).  The report query logic lives in
--         the application layer; this table only governs which reports are
--         active and which columns (incl. sensitive flags) are exposed.
--       - export_jobs: asynchronous CSV/xlsx export jobs.  The generated file is
--         stored encrypted in the ST-FND-10 document store; only an opaque
--         document UUID is referenced here (no PII, no output values stored).
--
--   (2) Calendar / work-pattern masters:
--       - company_calendars + calendar_days: business-day calculation
--         (weekday pattern + holiday / special-day overrides) referenced by
--         attendance and leave.
--       - work_patterns + shift_patterns + employee_work_assignments: work
--         system definitions (fixed / flex / variable / discretionary / shift)
--         with effective periods, resolved by application date.
--
-- LEGAL / CONFIG NOTE:
--   Holidays, prescribed/statutory holidays, prescribed working hours, statutory
--   working-time limits (原則 1日8h/週40h), variable-labour settlement periods,
--   flex core-time, and sensitive-PII export thresholds are all CONFIGURED here
--   (calendar_days / default_weekly_holidays_json / work_patterns.settings_json /
--   report_definitions.columns_json), never hard-coded.  Statutory limits must be
--   kept consistent with the attendance subsystem (LM-032/033 36協定/割増) to
--   avoid double definition.  Values depend on law (祝日法 etc.) and company
--   work rules and require confirmation by a licensed social-insurance / labour
--   attorney (社労士/弁護士).  This implementation is not legal advice and must be
--   updated as the law changes.
-- ===========================================================================

-- ---------------------------------------------------------------------------
-- report_definitions (標準レポート定義: 有効化 + 列選択)
-- ---------------------------------------------------------------------------
-- The report query itself is code-defined (resolved by report_key in the
-- service layer).  This table enables/disables a report per tenant and stores
-- column selection + sensitive flags via columns_json.
--   report_key: employee_roster | attendance_monthly | leave_status | billing_summary
--   columns_json: JSON array of column descriptors, e.g.
--     [{"key":"employee_code","label":"社員番号","sensitive":false}, ...]
--     Columns flagged "sensitive":true (マイナンバー/口座/健診 等) are excluded by
--     default and require the *:export_sensitive elevated permission.
--   params_schema_json: optional JSON schema describing accepted report params.
CREATE TABLE report_definitions (
    id                 uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id          uuid        NOT NULL REFERENCES tenants(id),
    report_key         text        NOT NULL,
    name               text        NOT NULL,
    params_schema_json jsonb       NOT NULL DEFAULT '{}',
    columns_json       jsonb       NOT NULL DEFAULT '[]',
    active             boolean     NOT NULL DEFAULT true,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_report_definitions PRIMARY KEY (id),
    CONSTRAINT chk_report_definitions_report_key
        CHECK (report_key IN ('employee_roster', 'attendance_monthly',
                              'leave_status', 'billing_summary')),
    -- One definition per (tenant, report_key).
    CONSTRAINT uq_report_definitions_tenant_report_key UNIQUE (tenant_id, report_key),
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_report_definitions_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_report_definitions_lookup
    ON report_definitions (tenant_id, report_key, active);

-- ---------------------------------------------------------------------------
-- export_jobs (非同期エクスポートジョブ)
-- ---------------------------------------------------------------------------
-- format: csv | xlsx     status: pending | running | completed | failed
-- requested_by_user_id: logical reference to users.id.  NOT a composite FK
--   because the users table has no UNIQUE(id, tenant_id); existence is verified
--   in the service layer (SELECT COUNT(1) ... WHERE id = ? AND tenant_id = ?).
-- result_document_id: logical reference to the ST-FND-10 documents store
--   (encrypted file).  ST-FND-10 lives in a separate story/migration, so this
--   is a plain uuid column with an index and NO foreign key (cross-story refs
--   use uuid columns only).  An opaque download link is derived from this id.
-- include_sensitive=true requires the *:export_sensitive permission, enforced
--   in the service layer.
-- params_json holds the target period / filters (reference IDs only, no PII).
-- error_message stores a short, non-PII failure reason for failed jobs.
CREATE TABLE export_jobs (
    id                   uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id            uuid        NOT NULL REFERENCES tenants(id),
    report_key           text        NOT NULL,
    format               text        NOT NULL,
    params_json          jsonb       NOT NULL DEFAULT '{}',
    status               text        NOT NULL DEFAULT 'pending',
    requested_by_user_id uuid,
    -- result_document_id: logical reference to ST-FND-10 documents (no FK).
    result_document_id   uuid,
    include_sensitive    boolean     NOT NULL DEFAULT false,
    error_message        text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    completed_at         timestamptz,
    CONSTRAINT pk_export_jobs PRIMARY KEY (id),
    CONSTRAINT chk_export_jobs_report_key
        CHECK (report_key IN ('employee_roster', 'attendance_monthly',
                              'leave_status', 'billing_summary')),
    CONSTRAINT chk_export_jobs_format
        CHECK (format IN ('csv', 'xlsx')),
    CONSTRAINT chk_export_jobs_status
        CHECK (status IN ('pending', 'running', 'completed', 'failed')),
    CONSTRAINT uq_export_jobs_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_export_jobs_lookup
    ON export_jobs (tenant_id, status, created_at);
-- Index for resolving an export job by its result document (opaque download).
CREATE INDEX idx_export_jobs_result_document
    ON export_jobs (tenant_id, result_document_id);

-- ---------------------------------------------------------------------------
-- company_calendars (会社カレンダー: 年度/有効期間単位)
-- ---------------------------------------------------------------------------
-- default_weekly_holidays_json: prescribed weekly-holiday weekday pattern, e.g.
--   {"weekdays":[0,6]}  (0=Sun .. 6=Sat).  CONFIGURED, not hard-coded.
-- effective_from / effective_to: revision support; the correct calendar is
--   resolved by application date.  effective_to NULL = open-ended.
--   (複数拠点対応する場合は将来 location_id を追加。)
CREATE TABLE company_calendars (
    id                           uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id                    uuid        NOT NULL REFERENCES tenants(id),
    name                         text        NOT NULL,
    fiscal_year                  int         NOT NULL,
    default_weekly_holidays_json jsonb       NOT NULL DEFAULT '{"weekdays":[0,6]}',
    active                       boolean     NOT NULL DEFAULT true,
    effective_from               date        NOT NULL,
    effective_to                 date,
    created_at                   timestamptz NOT NULL DEFAULT now(),
    updated_at                   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_company_calendars PRIMARY KEY (id),
    CONSTRAINT chk_company_calendars_effective_range
        CHECK (effective_to IS NULL OR effective_to >= effective_from),
    CONSTRAINT uq_company_calendars_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_company_calendars_lookup
    ON company_calendars (tenant_id, active, effective_from);

-- ---------------------------------------------------------------------------
-- calendar_days (個別日付の上書き: 祝日/特別休業/特別営業)
-- ---------------------------------------------------------------------------
-- Per-date override on top of the weekday pattern.
--   day_type: holiday | business_day | special_holiday
--     - holiday:         a holiday (祝日) overriding the weekday pattern
--     - special_holiday: a special company closure (特別休業日)
--     - business_day:    a special business day (特別営業日) overriding a
--                        weekend/holiday in the weekday pattern
-- (法定休日/所定休日の区分は会社規程依存のため calendar_days/weekly_holidays で
--  管理し、ハードコードしない。)
CREATE TABLE calendar_days (
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES tenants(id),
    calendar_id uuid        NOT NULL,
    date        date        NOT NULL,
    day_type    text        NOT NULL,
    label       text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_calendar_days PRIMARY KEY (id),
    CONSTRAINT chk_calendar_days_day_type
        CHECK (day_type IN ('holiday', 'business_day', 'special_holiday')),
    -- [Security] Composite FK: (calendar_id, tenant_id) must exist in
    -- company_calendars.  Prevents cross-tenant day insertion.
    CONSTRAINT fk_calendar_days_calendar_tenant
        FOREIGN KEY (calendar_id, tenant_id)
        REFERENCES company_calendars(id, tenant_id)
        MATCH SIMPLE,
    -- One override per (calendar, date).
    CONSTRAINT uq_calendar_days_calendar_date UNIQUE (calendar_id, date),
    CONSTRAINT uq_calendar_days_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_calendar_days_lookup
    ON calendar_days (tenant_id, calendar_id, date);

-- ---------------------------------------------------------------------------
-- work_patterns (勤務体系: 固定/フレックス/変形労働/裁量/シフト  LM-034)
-- ---------------------------------------------------------------------------
-- pattern_type: fixed | flex | variable | discretionary | shift
-- scheduled_minutes / break_minutes: prescribed working / break minutes.
-- core_time_json: flex core-time window, e.g. {"start":"10:00","end":"15:00"}.
-- settings_json: pattern-type specific config — variable-labour settlement
--   period, flex settlement, statutory-limit references (kept consistent with
--   the attendance subsystem to avoid double definition), and a reserved slot
--   for future 勤務間インターバル (LM-035) settings.  All CONFIGURED, never
--   hard-coded.
-- effective_from / effective_to: revision support; resolved by application date.
CREATE TABLE work_patterns (
    id               uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id        uuid        NOT NULL REFERENCES tenants(id),
    name             text        NOT NULL,
    pattern_type     text        NOT NULL,
    scheduled_minutes int        NOT NULL DEFAULT 480,
    break_minutes    int         NOT NULL DEFAULT 60,
    core_time_json   jsonb       NOT NULL DEFAULT '{}',
    settings_json    jsonb       NOT NULL DEFAULT '{}',
    effective_from   date        NOT NULL,
    effective_to     date,
    active           boolean     NOT NULL DEFAULT true,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_work_patterns PRIMARY KEY (id),
    CONSTRAINT chk_work_patterns_pattern_type
        CHECK (pattern_type IN ('fixed', 'flex', 'variable',
                               'discretionary', 'shift')),
    CONSTRAINT chk_work_patterns_minutes
        CHECK (scheduled_minutes >= 0 AND break_minutes >= 0),
    CONSTRAINT chk_work_patterns_effective_range
        CHECK (effective_to IS NULL OR effective_to >= effective_from),
    CONSTRAINT uq_work_patterns_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_work_patterns_lookup
    ON work_patterns (tenant_id, pattern_type, active, effective_from);

-- ---------------------------------------------------------------------------
-- shift_patterns (シフトパターン: 始業/終業/休憩)
-- ---------------------------------------------------------------------------
-- Attached to a work_pattern whose pattern_type = shift.
-- start_time / end_time stored as text "HH:MM"; overnight shifts
-- (end_time < start_time) are interpreted in the application layer.
CREATE TABLE shift_patterns (
    id                uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id         uuid        NOT NULL REFERENCES tenants(id),
    work_pattern_id   uuid        NOT NULL,
    name              text        NOT NULL,
    start_time        text        NOT NULL,
    end_time          text        NOT NULL,
    break_minutes     int         NOT NULL DEFAULT 0,
    scheduled_minutes int         NOT NULL DEFAULT 0,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_shift_patterns PRIMARY KEY (id),
    CONSTRAINT chk_shift_patterns_minutes
        CHECK (break_minutes >= 0 AND scheduled_minutes >= 0),
    -- [Security] Composite FK: (work_pattern_id, tenant_id) must exist in
    -- work_patterns.  Prevents cross-tenant shift insertion.
    CONSTRAINT fk_shift_patterns_work_pattern_tenant
        FOREIGN KEY (work_pattern_id, tenant_id)
        REFERENCES work_patterns(id, tenant_id)
        MATCH SIMPLE,
    -- One shift name per work pattern.
    CONSTRAINT uq_shift_patterns_work_pattern_name UNIQUE (work_pattern_id, name),
    CONSTRAINT uq_shift_patterns_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_shift_patterns_lookup
    ON shift_patterns (tenant_id, work_pattern_id);

-- ---------------------------------------------------------------------------
-- employee_work_assignments (従業員への勤務体系割当: 有効期間付き)
-- ---------------------------------------------------------------------------
-- Resolves to one work_pattern for an employee on a given application date.
-- Revisions are expressed with new assignments / new effective periods.
-- Overlap prevention is enforced in the application layer.
CREATE TABLE employee_work_assignments (
    id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES tenants(id),
    employee_id     uuid        NOT NULL,
    work_pattern_id uuid        NOT NULL,
    effective_from  date        NOT NULL,
    effective_to    date,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_employee_work_assignments PRIMARY KEY (id),
    CONSTRAINT chk_employee_work_assignments_effective_range
        CHECK (effective_to IS NULL OR effective_to >= effective_from),
    -- [Security] Composite FK: (employee_id, tenant_id) must exist in employees.
    CONSTRAINT fk_employee_work_assignments_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- [Security] Composite FK: (work_pattern_id, tenant_id) must exist in
    -- work_patterns.
    CONSTRAINT fk_employee_work_assignments_work_pattern_tenant
        FOREIGN KEY (work_pattern_id, tenant_id)
        REFERENCES work_patterns(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_employee_work_assignments_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_employee_work_assignments_lookup
    ON employee_work_assignments (tenant_id, employee_id, effective_from);

-- ---------------------------------------------------------------------------
-- RLS — all new tables
-- ---------------------------------------------------------------------------
ALTER TABLE report_definitions        ENABLE ROW LEVEL SECURITY;
ALTER TABLE report_definitions        FORCE  ROW LEVEL SECURITY;
ALTER TABLE export_jobs               ENABLE ROW LEVEL SECURITY;
ALTER TABLE export_jobs               FORCE  ROW LEVEL SECURITY;
ALTER TABLE company_calendars         ENABLE ROW LEVEL SECURITY;
ALTER TABLE company_calendars         FORCE  ROW LEVEL SECURITY;
ALTER TABLE calendar_days             ENABLE ROW LEVEL SECURITY;
ALTER TABLE calendar_days             FORCE  ROW LEVEL SECURITY;
ALTER TABLE work_patterns             ENABLE ROW LEVEL SECURITY;
ALTER TABLE work_patterns             FORCE  ROW LEVEL SECURITY;
ALTER TABLE shift_patterns            ENABLE ROW LEVEL SECURITY;
ALTER TABLE shift_patterns            FORCE  ROW LEVEL SECURITY;
ALTER TABLE employee_work_assignments ENABLE ROW LEVEL SECURITY;
ALTER TABLE employee_work_assignments FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON report_definitions
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON export_jobs
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON company_calendars
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON calendar_days
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON work_patterns
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON shift_patterns
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON employee_work_assignments
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to hr_app
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE report_definitions        TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE export_jobs               TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE company_calendars         TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE calendar_days             TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE work_patterns             TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE shift_patterns            TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE employee_work_assignments TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE employee_work_assignments FROM hr_app;
REVOKE ALL ON TABLE shift_patterns            FROM hr_app;
REVOKE ALL ON TABLE work_patterns             FROM hr_app;
REVOKE ALL ON TABLE calendar_days             FROM hr_app;
REVOKE ALL ON TABLE company_calendars         FROM hr_app;
REVOKE ALL ON TABLE export_jobs               FROM hr_app;
REVOKE ALL ON TABLE report_definitions        FROM hr_app;

DROP TABLE IF EXISTS employee_work_assignments;
DROP TABLE IF EXISTS shift_patterns;
DROP TABLE IF EXISTS work_patterns;
DROP TABLE IF EXISTS calendar_days;
DROP TABLE IF EXISTS company_calendars;
DROP TABLE IF EXISTS export_jobs;
DROP TABLE IF EXISTS report_definitions;

-- +goose StatementEnd
