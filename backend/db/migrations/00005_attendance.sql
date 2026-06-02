-- +goose Up
-- +goose StatementBegin

-- ---------------------------------------------------------------------------
-- LEGAL NOTICE
-- ---------------------------------------------------------------------------
-- The default values for overtime rates, 36-agreement limits, night-work
-- hours, and rounding units below are based on Japanese Labor Standards Law
-- as of the migration authoring date (2026-06-02).
-- Labor law is subject to revision. All configurable values MUST be reviewed
-- by a qualified labor-law professional (社会保険労務士 / 弁護士) and kept
-- up to date with statutory amendments. Do NOT treat these defaults as legal
-- advice or authoritative compliance thresholds.
--
-- Configurable columns in attendance_settings (all per-tenant):
--   overtime_rate, night_rate, holiday_rate, over60_rate  — 割増率 (as decimal)
--   night_start / night_end                               — 深夜時間帯 (時刻)
--   rounding_unit_minutes, break_auto_minutes             — 丸め・休憩自動控除
-- Configurable columns in labor_agreements (per-agreement):
--   monthly_limit_minutes, yearly_limit_minutes           — 36協定上限
--   special_monthly_limit_minutes, special_count_limit    — 特別条項
--   multi_month_avg_limit_minutes                         — 複数月平均上限
-- ---------------------------------------------------------------------------

-- ---------------------------------------------------------------------------
-- attendance_settings (テナント別設定)
-- ---------------------------------------------------------------------------
-- One row per tenant. Seeded with statutory defaults; must be updated by an
-- administrator after review by a qualified professional.
CREATE TABLE attendance_settings (
    id                    uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id             uuid        NOT NULL REFERENCES tenants(id),
    -- 丸め単位 (分): 0 = no rounding, 1 = nearest minute, 15 = 15-min blocks, etc.
    rounding_unit_minutes int         NOT NULL DEFAULT 1,
    -- 割増率: statutory minimums as of 2026. MUST be reviewed with each amendment.
    -- 時間外: 25% (大企業); 中小企業の月60h超は50%は over60_rate で管理
    overtime_rate         numeric(5,4) NOT NULL DEFAULT 1.25,
    -- 深夜: 22:00-05:00, +25%
    night_rate            numeric(5,4) NOT NULL DEFAULT 0.25,
    -- 法定休日: +35% (法定休日労働のみ; 所定休日は overtime_rate 扱い)
    holiday_rate          numeric(5,4) NOT NULL DEFAULT 1.35,
    -- 月60h超の時間外: 50% (中小企業適用は経過措置終了後。設定で切替)
    over60_rate           numeric(5,4) NOT NULL DEFAULT 1.50,
    -- 深夜時間帯 (設定で変更可能; 法令上は22:00-05:00)
    night_start           time        NOT NULL DEFAULT '22:00:00',
    night_end             time        NOT NULL DEFAULT '05:00:00',
    -- 休憩自動控除 (分): 0 = 手動のみ
    break_auto_minutes    int         NOT NULL DEFAULT 0,
    -- 乖離アラート閾値 (分): 打刻と所定の乖離がこの値を超えたらアラート
    deviation_alert_minutes int       NOT NULL DEFAULT 30,
    -- 月60h境界 (分): 月累計残業がこの値を超えた分は over60_rate で計算する。
    -- 法令上は60h×60分=3600が標準。中小企業経過措置等の変更に対応するため設定化。
    -- 要専門家確認・改正追従 (労働基準法第37条4項)
    over60_boundary_minutes int       NOT NULL DEFAULT 3600,
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_attendance_settings PRIMARY KEY (id),
    CONSTRAINT uq_attendance_settings_tenant UNIQUE (tenant_id)
);

-- ---------------------------------------------------------------------------
-- attendance_records (打刻 / 客観的把握 LM-030)
-- ---------------------------------------------------------------------------
CREATE TABLE attendance_records (
    id           uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id    uuid        NOT NULL REFERENCES tenants(id),
    employee_id  uuid        NOT NULL,
    -- 勤務日 (日跨ぎ勤務はいずれか一方の日付を基準日とすること)
    work_date    date        NOT NULL,
    clock_in     timestamptz,
    clock_out    timestamptz,
    -- 休憩時間 (分): 自動控除 + 手入力の合計
    break_minutes int        NOT NULL DEFAULT 0,
    -- 記録元: web / mobile / device / correction (客観性種別 LM-030)
    source       text        NOT NULL DEFAULT 'web',
    -- 補正済みフラグ
    is_corrected bool        NOT NULL DEFAULT false,
    note         text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_attendance_records PRIMARY KEY (id),
    -- [Security] 複合FK: (employee_id, tenant_id) が employees に存在することを保証
    -- RLS はアプリ層でバイパスされる可能性があるため DB 制約でも強制する
    CONSTRAINT fk_attendance_records_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- 同一従業員・同一勤務日の重複を防ぐ
    CONSTRAINT uq_attendance_record_employee_date UNIQUE (tenant_id, employee_id, work_date),
    -- [Security] UNIQUE(id, tenant_id) が attendance_corrections の複合FK 参照先として必要
    -- (FK checks bypass RLS; composite constraint enforces cross-tenant safety at DB level)
    CONSTRAINT uq_attendance_records_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_attendance_records_lookup
    ON attendance_records (tenant_id, employee_id, work_date);

-- ---------------------------------------------------------------------------
-- attendance_corrections (補正履歴 — 客観的記録の変更証跡 LM-030)
-- ---------------------------------------------------------------------------
CREATE TABLE attendance_corrections (
    id                   uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id            uuid        NOT NULL REFERENCES tenants(id),
    attendance_record_id uuid        NOT NULL,
    -- 補正前後を jsonb で保存 (スキーマ変更に追従しやすくするため)
    before_json          jsonb       NOT NULL DEFAULT '{}',
    after_json           jsonb       NOT NULL DEFAULT '{}',
    reason               text        NOT NULL,
    corrected_by_user_id uuid        NOT NULL,
    corrected_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_attendance_corrections PRIMARY KEY (id),
    -- [Security] 複合FK: attendance_records の UNIQUE(id, tenant_id) を参照
    CONSTRAINT fk_corrections_record_tenant
        FOREIGN KEY (attendance_record_id, tenant_id)
        REFERENCES attendance_records(id, tenant_id)
        MATCH SIMPLE
);

CREATE INDEX idx_attendance_corrections_lookup
    ON attendance_corrections (tenant_id, attendance_record_id);

-- ---------------------------------------------------------------------------
-- work_summaries (月次集計 LM-033)
-- ---------------------------------------------------------------------------
CREATE TABLE work_summaries (
    id                uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id         uuid        NOT NULL REFERENCES tenants(id),
    employee_id       uuid        NOT NULL,
    -- 対象月の1日 (YYYY-MM-01)
    period_month      date        NOT NULL,
    scheduled_minutes int         NOT NULL DEFAULT 0,
    actual_minutes    int         NOT NULL DEFAULT 0,
    -- 法定時間外 (月60h以下分)
    overtime_minutes  int         NOT NULL DEFAULT 0,
    -- 深夜時間外 (22:00-05:00 内の実働分)
    night_minutes     int         NOT NULL DEFAULT 0,
    -- 法定休日労働分
    holiday_minutes   int         NOT NULL DEFAULT 0,
    -- 月60h超の時間外分 (割増率が変わる)
    over60_minutes    int         NOT NULL DEFAULT 0,
    computed_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_work_summaries PRIMARY KEY (id),
    CONSTRAINT fk_work_summaries_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_work_summaries_month UNIQUE (tenant_id, employee_id, period_month)
);

CREATE INDEX idx_work_summaries_lookup
    ON work_summaries (tenant_id, employee_id, period_month);

-- ---------------------------------------------------------------------------
-- labor_agreements (36協定 LM-032)
-- ---------------------------------------------------------------------------
-- Default limit values (分):
--   monthly_limit_minutes     = 2700  (45h × 60)
--   yearly_limit_minutes      = 21600 (360h × 60)
--   special_monthly_limit      = 4800  (80h × 60; 複数月平均参考値)
--   multi_month_avg_limit      = 4800  (80h × 60)
-- IMPORTANT: These are statutory defaults as of 2026. Verify with amendments.
CREATE TABLE labor_agreements (
    id                             uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id                      uuid        NOT NULL REFERENCES tenants(id),
    -- 事業場名 (複数事業場対応)
    workplace                      text        NOT NULL,
    valid_from                     date        NOT NULL,
    valid_to                       date        NOT NULL,
    -- 通常条項: 月45h, 年360h (法定上限 — 要専門家確認)
    monthly_limit_minutes          int         NOT NULL DEFAULT 2700,
    yearly_limit_minutes           int         NOT NULL DEFAULT 21600,
    -- 特別条項
    special_clause                 bool        NOT NULL DEFAULT false,
    -- 特別条項発動時の月上限 (null = 特別条項なし)
    special_monthly_limit_minutes  int,
    -- 特別条項の発動可能回数/年 (法令上は6回以下が目安; 要専門家確認)
    special_count_limit            int,
    -- 複数月平均上限 (2〜6か月平均; 法令上80h目安, 健康配慮目的100h; 要専門家確認)
    multi_month_avg_limit_minutes  int         DEFAULT 4800,
    created_at                     timestamptz NOT NULL DEFAULT now(),
    updated_at                     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_labor_agreements PRIMARY KEY (id),
    -- 同一テナント・事業場・有効開始日の重複協定を防ぐ
    CONSTRAINT uq_labor_agreements_workplace_from UNIQUE (tenant_id, workplace, valid_from)
);

CREATE INDEX idx_labor_agreements_tenant
    ON labor_agreements (tenant_id, valid_from, valid_to);

-- ---------------------------------------------------------------------------
-- RLS — 全テーブル共通
-- ---------------------------------------------------------------------------
ALTER TABLE attendance_settings   ENABLE ROW LEVEL SECURITY;
ALTER TABLE attendance_settings   FORCE  ROW LEVEL SECURITY;
ALTER TABLE attendance_records    ENABLE ROW LEVEL SECURITY;
ALTER TABLE attendance_records    FORCE  ROW LEVEL SECURITY;
ALTER TABLE attendance_corrections ENABLE ROW LEVEL SECURITY;
ALTER TABLE attendance_corrections FORCE  ROW LEVEL SECURITY;
ALTER TABLE work_summaries        ENABLE ROW LEVEL SECURITY;
ALTER TABLE work_summaries        FORCE  ROW LEVEL SECURITY;
ALTER TABLE labor_agreements      ENABLE ROW LEVEL SECURITY;
ALTER TABLE labor_agreements      FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON attendance_settings
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON attendance_records
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON attendance_corrections
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON work_summaries
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON labor_agreements
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to hr_app
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE attendance_settings    TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE attendance_records     TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE attendance_corrections TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE work_summaries         TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE labor_agreements       TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE labor_agreements       FROM hr_app;
REVOKE ALL ON TABLE work_summaries         FROM hr_app;
REVOKE ALL ON TABLE attendance_corrections FROM hr_app;
REVOKE ALL ON TABLE attendance_records     FROM hr_app;
REVOKE ALL ON TABLE attendance_settings    FROM hr_app;

DROP TABLE IF EXISTS labor_agreements;
DROP TABLE IF EXISTS work_summaries;
DROP TABLE IF EXISTS attendance_corrections;
DROP TABLE IF EXISTS attendance_records;
DROP TABLE IF EXISTS attendance_settings;

-- +goose StatementEnd
