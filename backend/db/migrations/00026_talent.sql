-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- ST-TM-04 タレントマネジメント: 人材DB・スキルマップ・配置・パルスサーベイ
-- ===========================================================================
-- This migration implements the talent management domain:
--   TM-020 人材DB (統合プロフィール: skills / employee_skills / certifications)
--   TM-021 配置/異動シミュレーション + 組織図ビュー (placement_simulations[_items])
--   TM-022 エンゲージメント/パルスサーベイ (pulse_surveys / pulse_survey_responses)
--
-- 法令・プライバシー注記:
--   スキルレベル定義・サーベイ最小開示閾値・資格期限アラート日数などは
--   企業/プライバシー方針に依存するため設定化(skills.levels_json /
--   pulse_surveys.min_responses_to_show 等)。人事データ利活用は
--   『人事データ利活用原則・AI事業者ガイドライン(CMP-005)』『個人情報保護法
--   利用目的明示・最小アクセス(CMP-004)』に関わり、利用目的・開示範囲・保持期間は
--   設定化のうえ要専門家(社労士/弁護士/プライバシー担当)確認が前提。本実装は
--   法的助言ではない。健康・心身を示唆し得る自由記述(pulse_survey_responses.free_text)は
--   要配慮個人情報に準じた厳格管理(列暗号・アクセスログ・最小アクセス)とする。

-- ---------------------------------------------------------------------------
-- skills (スキルマスタ / TM-020)
-- ---------------------------------------------------------------------------
-- Per-tenant skill catalog (category/name/level definitions).
-- levels_json: JSON object/array describing level definitions (例 1..5 とラベル).
--   Format: {"min":1,"max":5,"labels":{"1":"初級", ... "5":"エキスパート"}}
--   The min/max bounds are validated in the application layer when assigning
--   employee_skills.level — level definitions are tenant configuration, not
--   hard-coded.
CREATE TABLE skills (
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES tenants(id),
    category    text        NOT NULL DEFAULT '',
    name        text        NOT NULL,
    levels_json jsonb       NOT NULL DEFAULT '{}',
    active      boolean     NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_skills PRIMARY KEY (id),
    -- One skill name per (category) per tenant.
    CONSTRAINT uq_skills_tenant_category_name UNIQUE (tenant_id, category, name),
    -- UNIQUE(id, tenant_id) required for downstream composite FK references
    -- (employee_skills references (skill_id, tenant_id)).
    CONSTRAINT uq_skills_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_skills_lookup ON skills (tenant_id, category, active);

-- ---------------------------------------------------------------------------
-- employee_skills (従業員×スキル = スキルマップの実体 / TM-020)
-- ---------------------------------------------------------------------------
-- level is validated against skills.levels_json (min/max) in the application
-- layer.  acquired_on/expires_on track currency for alerting.
CREATE TABLE employee_skills (
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES tenants(id),
    employee_id uuid        NOT NULL,
    skill_id    uuid        NOT NULL,
    level       integer     NOT NULL DEFAULT 1,
    acquired_on date,
    expires_on  date,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_employee_skills PRIMARY KEY (id),
    CONSTRAINT chk_employee_skills_level CHECK (level >= 0),
    -- [Security] Composite FK: (employee_id, tenant_id) must exist in employees.
    -- Prevents cross-tenant employee references (FK checks bypass RLS).
    CONSTRAINT fk_employee_skills_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- [Security] Composite FK to own-package skills table: (skill_id, tenant_id).
    CONSTRAINT fk_employee_skills_skill_tenant
        FOREIGN KEY (skill_id, tenant_id)
        REFERENCES skills(id, tenant_id)
        MATCH SIMPLE,
    -- One row per (employee, skill) per tenant — upsert target.
    CONSTRAINT uq_employee_skills_emp_skill_tenant UNIQUE (employee_id, skill_id, tenant_id),
    CONSTRAINT uq_employee_skills_id_tenant UNIQUE (id, tenant_id)
);

-- Index tuned for skill search (skill_id + level lower-bound) within a tenant.
CREATE INDEX idx_employee_skills_search ON employee_skills (tenant_id, skill_id, level);
CREATE INDEX idx_employee_skills_employee ON employee_skills (tenant_id, employee_id);
-- Index for expiry alerting.
CREATE INDEX idx_employee_skills_expires ON employee_skills (tenant_id, expires_on);

-- ---------------------------------------------------------------------------
-- employee_certifications (資格管理 / TM-020)
-- ---------------------------------------------------------------------------
-- expires_on drives "期限切れ間近" alerting (threshold days is configuration).
CREATE TABLE employee_certifications (
    id               uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id        uuid        NOT NULL REFERENCES tenants(id),
    employee_id      uuid        NOT NULL,
    name             text        NOT NULL,
    issuer           text        NOT NULL DEFAULT '',
    acquired_on      date,
    expires_on       date,
    renewal_required boolean     NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_employee_certifications PRIMARY KEY (id),
    -- [Security] Composite FK: (employee_id, tenant_id) must exist in employees.
    CONSTRAINT fk_cert_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_employee_certifications_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_employee_certifications_employee ON employee_certifications (tenant_id, employee_id);
CREATE INDEX idx_employee_certifications_expires ON employee_certifications (tenant_id, expires_on);

-- ---------------------------------------------------------------------------
-- placement_simulations (配置/異動シミュレーション ヘッダ / TM-021)
-- ---------------------------------------------------------------------------
-- A draft proposal grouping zero-or-more individual move items.
-- status: draft → applied | discarded.  Only on "applied" are the items
-- written to employee_assignments (発令履歴) — within the SAME transaction.
-- While draft, the simulation NEVER mutates the real org (departments /
-- employee_assignments).
CREATE TABLE placement_simulations (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    name                text        NOT NULL,
    -- status: "draft" | "applied" | "discarded"
    status              text        NOT NULL DEFAULT 'draft',
    created_by_user_id  uuid,
    applied_at          timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_placement_simulations PRIMARY KEY (id),
    CONSTRAINT chk_placement_simulations_status
        CHECK (status IN ('draft', 'applied', 'discarded')),
    CONSTRAINT uq_placement_simulations_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_placement_simulations_lookup ON placement_simulations (tenant_id, status);

-- ---------------------------------------------------------------------------
-- placement_simulation_items (シミュレーション明細 = 個別異動案 / TM-021)
-- ---------------------------------------------------------------------------
-- Each item proposes a move for one employee.  target_department_id is nullable
-- (MATCH SIMPLE skips FK enforcement on NULL).  On apply, each item is mapped
-- to an employee_assignments row.
CREATE TABLE placement_simulation_items (
    id                   uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id            uuid        NOT NULL REFERENCES tenants(id),
    simulation_id        uuid        NOT NULL,
    employee_id          uuid        NOT NULL,
    target_department_id uuid,
    target_position      text,
    target_grade         text,
    effective_from       date        NOT NULL,
    reason               text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_placement_simulation_items PRIMARY KEY (id),
    -- [Security] Composite FK to own-package header: (simulation_id, tenant_id).
    CONSTRAINT fk_psi_simulation_tenant
        FOREIGN KEY (simulation_id, tenant_id)
        REFERENCES placement_simulations(id, tenant_id)
        MATCH SIMPLE,
    -- [Security] Composite FK: (employee_id, tenant_id) must exist in employees.
    CONSTRAINT fk_psi_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- [Security] Composite FK: (target_department_id, tenant_id) must exist in
    -- departments when non-NULL.  MATCH SIMPLE: NULL skips FK enforcement.
    CONSTRAINT fk_psi_department_tenant
        FOREIGN KEY (target_department_id, tenant_id)
        REFERENCES departments(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_psi_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_psi_lookup ON placement_simulation_items (tenant_id, simulation_id);

-- ---------------------------------------------------------------------------
-- pulse_surveys (エンゲージメント/パルスサーベイ / TM-022)
-- ---------------------------------------------------------------------------
-- questions_json: question definitions.
-- anonymous: when true, responses are aggregate-only and respondent identity is
--   never exposed (individual reverse-lookup is prohibited).
-- min_responses_to_show: aggregation results are hidden for any segment with
--   fewer than this many responses (minimum-disclosure threshold; configuration).
CREATE TABLE pulse_surveys (
    id                    uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id             uuid        NOT NULL REFERENCES tenants(id),
    title                 text        NOT NULL,
    questions_json        jsonb       NOT NULL DEFAULT '[]',
    anonymous             boolean     NOT NULL DEFAULT true,
    -- min_responses_to_show: minimum-disclosure threshold (privacy config).
    min_responses_to_show integer     NOT NULL DEFAULT 5,
    starts_on             date,
    ends_on               date,
    -- status: "draft" | "open" | "closed"
    status                text        NOT NULL DEFAULT 'draft',
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_pulse_surveys PRIMARY KEY (id),
    CONSTRAINT chk_pulse_surveys_status
        CHECK (status IN ('draft', 'open', 'closed')),
    CONSTRAINT chk_pulse_surveys_min_responses CHECK (min_responses_to_show >= 1),
    CONSTRAINT uq_pulse_surveys_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_pulse_surveys_lookup ON pulse_surveys (tenant_id, status);

-- ---------------------------------------------------------------------------
-- pulse_survey_responses (サーベイ回答 / TM-022)
-- ---------------------------------------------------------------------------
-- answers_json: structured (non-free-text) answers.
-- respondent_employee_id: nullable; for anonymous surveys it is stored NULL so
--   the response can never be reverse-linked to an individual.  For non-anonymous
--   surveys it references the responding employee (composite FK).
-- free_text (自由記述):  心身状態・人間関係等の機微情報を含み得るため
--   AES-256-GCM(internal/platform/crypto, bytea)で列暗号化する。平文は
--   永続化・ログ・監査に残さない。復号は survey:read_freetext 権限 +
--   サービス層の権限再検証(多層防御)を通過した場合のみ。
CREATE TABLE pulse_survey_responses (
    id                     uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id              uuid        NOT NULL REFERENCES tenants(id),
    survey_id              uuid        NOT NULL,
    -- respondent_employee_id: NULL for anonymous surveys (reverse-lookup
    -- prohibited).  When non-NULL it is enforced to belong to the tenant via
    -- the composite FK below.
    respondent_employee_id uuid,
    answers_json           jsonb       NOT NULL DEFAULT '{}',
    -- free_text holds AES-256-GCM ciphertext of the free-text answer.
    -- SECURITY: plaintext is NEVER stored; do NOT add a text column for it.
    free_text              bytea,
    submitted_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_pulse_survey_responses PRIMARY KEY (id),
    -- [Security] Composite FK to own-package survey header: (survey_id, tenant_id).
    CONSTRAINT fk_resp_survey_tenant
        FOREIGN KEY (survey_id, tenant_id)
        REFERENCES pulse_surveys(id, tenant_id)
        MATCH SIMPLE,
    -- [Security] Composite FK: when respondent_employee_id is non-NULL it must
    -- exist in employees for the same tenant.  MATCH SIMPLE: NULL (anonymous)
    -- skips FK enforcement.
    CONSTRAINT fk_resp_employee_tenant
        FOREIGN KEY (respondent_employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_pulse_survey_responses_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_pulse_survey_responses_survey ON pulse_survey_responses (tenant_id, survey_id);

-- ---------------------------------------------------------------------------
-- RLS — all new tables
-- ---------------------------------------------------------------------------
ALTER TABLE skills                     ENABLE ROW LEVEL SECURITY;
ALTER TABLE skills                     FORCE  ROW LEVEL SECURITY;
ALTER TABLE employee_skills            ENABLE ROW LEVEL SECURITY;
ALTER TABLE employee_skills            FORCE  ROW LEVEL SECURITY;
ALTER TABLE employee_certifications    ENABLE ROW LEVEL SECURITY;
ALTER TABLE employee_certifications    FORCE  ROW LEVEL SECURITY;
ALTER TABLE placement_simulations      ENABLE ROW LEVEL SECURITY;
ALTER TABLE placement_simulations      FORCE  ROW LEVEL SECURITY;
ALTER TABLE placement_simulation_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE placement_simulation_items FORCE  ROW LEVEL SECURITY;
ALTER TABLE pulse_surveys              ENABLE ROW LEVEL SECURITY;
ALTER TABLE pulse_surveys              FORCE  ROW LEVEL SECURITY;
ALTER TABLE pulse_survey_responses     ENABLE ROW LEVEL SECURITY;
ALTER TABLE pulse_survey_responses     FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON skills
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON employee_skills
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON employee_certifications
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON placement_simulations
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON placement_simulation_items
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON pulse_surveys
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON pulse_survey_responses
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to hr_app
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE skills                     TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE employee_skills            TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE employee_certifications    TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE placement_simulations      TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE placement_simulation_items TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE pulse_surveys              TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE pulse_survey_responses     TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE pulse_survey_responses     FROM hr_app;
REVOKE ALL ON TABLE pulse_surveys              FROM hr_app;
REVOKE ALL ON TABLE placement_simulation_items FROM hr_app;
REVOKE ALL ON TABLE placement_simulations      FROM hr_app;
REVOKE ALL ON TABLE employee_certifications    FROM hr_app;
REVOKE ALL ON TABLE employee_skills            FROM hr_app;
REVOKE ALL ON TABLE skills                     FROM hr_app;

DROP TABLE IF EXISTS pulse_survey_responses;
DROP TABLE IF EXISTS pulse_surveys;
DROP TABLE IF EXISTS placement_simulation_items;
DROP TABLE IF EXISTS placement_simulations;
DROP TABLE IF EXISTS employee_certifications;
DROP TABLE IF EXISTS employee_skills;
DROP TABLE IF EXISTS skills;

-- +goose StatementEnd
