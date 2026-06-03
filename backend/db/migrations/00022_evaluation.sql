-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- ST-TM-02 評価ワークフロー (自己/上司/二次・360度・キャリブレーション)
-- ===========================================================================
-- This migration implements the talent-management performance review workflow:
--   - review_templates     : per-tenant evaluation sheet templates (JSONB).
--   - reviews              : one review header per (cycle, employee).
--   - review_entries       : per-item answers for each stage (self/primary/...).
--   - review_360_requests  : 360-degree rater invitations (peer/subordinate/...).
--   - calibration_sessions : calibration (評価会議) decision records.
--
-- Legal /制度 note: evaluation scales, item weights, grade mappings, and
-- relative-distribution guidelines depend on each company's HR appraisal system
-- and labour rules.  They are NOT hardcoded — they are stored in
-- rating_scale_json / items_json / settings JSONB so the system follows制度
-- changes.  Reflecting evaluation results into compensation / grade requires
-- 社労士 (labour & social security attorney) review and就業規則 alignment.
-- This implementation is NOT legal advice (docs/05 §4).
--
-- Cross-story logical references (no FK, bare uuid + index per convention):
--   - cycle_id  → review_cycles(id) owned by ST-TM-01.  We do NOT add a FK to
--     another story's table; isolation is enforced by RLS + explicit tenant_id
--     checks and an index for lookup performance.

-- ---------------------------------------------------------------------------
-- review_templates (評価シートテンプレート)
-- ---------------------------------------------------------------------------
-- stages_json:       評価ステージ構成  (e.g. [{"stage":"self"},{"stage":"primary"}])
-- items_json:        評価項目 (配点/コンピテンシー/グレード紐付け)
-- rating_scale_json: 評価尺度→数値マッピング (e.g. {"S":5,"A":4,"B":3,"C":2,"D":1})
CREATE TABLE review_templates (
    id                uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id         uuid        NOT NULL REFERENCES tenants(id),
    name              text        NOT NULL,
    stages_json       jsonb       NOT NULL DEFAULT '[]',
    items_json        jsonb       NOT NULL DEFAULT '[]',
    rating_scale_json jsonb       NOT NULL DEFAULT '{}',
    active            boolean     NOT NULL DEFAULT true,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_review_templates PRIMARY KEY (id),
    -- UNIQUE(id, tenant_id) required for downstream composite FK references.
    CONSTRAINT uq_review_templates_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_review_templates_lookup
    ON review_templates (tenant_id, active);

-- ---------------------------------------------------------------------------
-- reviews (評価ヘッダ: 被評価者1人 × 1期)
-- ---------------------------------------------------------------------------
-- status FSM (enforced in application + CHECK):
--   not_started → self_submitted → primary_submitted → secondary_submitted
--                → calibrated → confirmed
-- final_rating:    確定スコア (加重平均結果)。確定後は不変。
-- adjusted_rating: キャリブレーション調整後 (NULL可)。元 final_rating は保持。
-- cycle_id: 論理参照 review_cycles(id) (ST-TM-01) — 素の uuid 列 (FKを張らない)。
CREATE TABLE reviews (
    id                    uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id             uuid        NOT NULL REFERENCES tenants(id),
    -- cycle_id: logical reference to review_cycles(id) owned by ST-TM-01.
    -- No FK across story tables; index only.
    cycle_id              uuid        NOT NULL,
    template_id           uuid        NOT NULL,
    employee_id           uuid        NOT NULL,
    primary_reviewer_id   uuid,
    secondary_reviewer_id uuid,
    status                text        NOT NULL DEFAULT 'not_started',
    final_rating          numeric(6,3),
    adjusted_rating       numeric(6,3),
    confirmed_at          timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_reviews PRIMARY KEY (id),
    CONSTRAINT chk_reviews_status
        CHECK (status IN ('not_started', 'self_submitted', 'primary_submitted',
                          'secondary_submitted', 'calibrated', 'confirmed')),
    -- [Security] Composite FK: (employee_id, tenant_id) must exist in employees.
    -- Prevents cross-tenant subject assignment.
    CONSTRAINT fk_reviews_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- [Security] Composite FK: primary reviewer must be a same-tenant employee.
    CONSTRAINT fk_reviews_primary_reviewer_tenant
        FOREIGN KEY (primary_reviewer_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- [Security] Composite FK: secondary reviewer must be a same-tenant employee.
    CONSTRAINT fk_reviews_secondary_reviewer_tenant
        FOREIGN KEY (secondary_reviewer_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- [Security] Composite FK: template must belong to the same tenant.
    CONSTRAINT fk_reviews_template_tenant
        FOREIGN KEY (template_id, tenant_id)
        REFERENCES review_templates(id, tenant_id)
        MATCH SIMPLE,
    -- One review per (cycle, employee) within a tenant.
    CONSTRAINT uq_reviews_cycle_employee_tenant UNIQUE (cycle_id, employee_id, tenant_id),
    -- UNIQUE(id, tenant_id) required for downstream composite FK references.
    CONSTRAINT uq_reviews_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_reviews_lookup
    ON reviews (tenant_id, cycle_id, employee_id, status);
CREATE INDEX idx_reviews_primary_reviewer
    ON reviews (tenant_id, primary_reviewer_id);
CREATE INDEX idx_reviews_secondary_reviewer
    ON reviews (tenant_id, secondary_reviewer_id);

-- ---------------------------------------------------------------------------
-- review_entries (評価項目ごとの回答)
-- ---------------------------------------------------------------------------
-- stage: self | primary | secondary | 360
-- reviewer_user_id: the user (評価者) who authored this entry.  For anonymous
--   360 responses the aggregation API suppresses this value (秘匿) — the column
--   is still stored for integrity but never returned for anonymous raters.
-- comment: tenant-readable (RLS + RBAC protected) but NEVER written to audit
--   logs or notification payloads — 評価コメントの機微性に配慮。
CREATE TABLE review_entries (
    id               uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id        uuid        NOT NULL REFERENCES tenants(id),
    review_id        uuid        NOT NULL,
    stage            text        NOT NULL,
    reviewer_user_id uuid,
    item_key         text        NOT NULL,
    score            numeric(6,3),
    comment          text,
    submitted_at     timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_review_entries PRIMARY KEY (id),
    CONSTRAINT chk_review_entries_stage
        CHECK (stage IN ('self', 'primary', 'secondary', '360')),
    -- [Security] Composite FK within own package: entry belongs to a same-tenant review.
    CONSTRAINT fk_review_entries_review_tenant
        FOREIGN KEY (review_id, tenant_id)
        REFERENCES reviews(id, tenant_id)
        MATCH SIMPLE,
    -- One entry per (review, stage, reviewer, item) — re-submission updates it.
    CONSTRAINT uq_review_entries_unique
        UNIQUE (review_id, stage, reviewer_user_id, item_key, tenant_id),
    CONSTRAINT uq_review_entries_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_review_entries_lookup
    ON review_entries (tenant_id, review_id, stage);

-- ---------------------------------------------------------------------------
-- review_360_requests (360度評価の評価者依頼)
-- ---------------------------------------------------------------------------
-- relationship: peer | subordinate | other
-- anonymous: true の場合 rater_employee_id を結果集計から秘匿 (API・監査ログ・
--   JSONレスポンスのいずれにも漏らさない)。最小回答数未満も非表示。
-- status: pending | submitted | declined
CREATE TABLE review_360_requests (
    id                uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id         uuid        NOT NULL REFERENCES tenants(id),
    review_id         uuid        NOT NULL,
    rater_employee_id uuid        NOT NULL,
    relationship      text        NOT NULL DEFAULT 'peer',
    anonymous         boolean     NOT NULL DEFAULT false,
    status            text        NOT NULL DEFAULT 'pending',
    responded_at      timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_review_360_requests PRIMARY KEY (id),
    CONSTRAINT chk_review_360_requests_relationship
        CHECK (relationship IN ('peer', 'subordinate', 'other')),
    CONSTRAINT chk_review_360_requests_status
        CHECK (status IN ('pending', 'submitted', 'declined')),
    -- [Security] Composite FK within own package: same-tenant review.
    CONSTRAINT fk_review_360_requests_review_tenant
        FOREIGN KEY (review_id, tenant_id)
        REFERENCES reviews(id, tenant_id)
        MATCH SIMPLE,
    -- [Security] Composite FK: rater must be a same-tenant employee.
    CONSTRAINT fk_review_360_requests_rater_tenant
        FOREIGN KEY (rater_employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- One invitation per (review, rater) within a tenant.
    CONSTRAINT uq_review_360_requests_review_rater
        UNIQUE (review_id, rater_employee_id, tenant_id),
    CONSTRAINT uq_review_360_requests_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_review_360_requests_lookup
    ON review_360_requests (tenant_id, review_id, status);

-- ---------------------------------------------------------------------------
-- calibration_sessions (評価会議)
-- ---------------------------------------------------------------------------
-- decisions_json: 被評価者ごとの調整を記録
--   [{"review_id":"...","before":3.5,"after":3.0,"reason":"..."}]
-- 元スコアは reviews.final_rating に保持、調整は reviews.adjusted_rating へ反映。
-- cycle_id: 論理参照 review_cycles(id) (ST-TM-01) — 素の uuid 列 (FKを張らない)。
CREATE TABLE calibration_sessions (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    -- cycle_id: logical reference to review_cycles(id) owned by ST-TM-01.
    cycle_id            uuid        NOT NULL,
    name                text        NOT NULL,
    facilitator_user_id uuid,
    status              text        NOT NULL DEFAULT 'open',
    decisions_json      jsonb       NOT NULL DEFAULT '[]',
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_calibration_sessions PRIMARY KEY (id),
    CONSTRAINT chk_calibration_sessions_status
        CHECK (status IN ('open', 'closed')),
    CONSTRAINT uq_calibration_sessions_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_calibration_sessions_lookup
    ON calibration_sessions (tenant_id, cycle_id, status);

-- ---------------------------------------------------------------------------
-- RLS — all new tables
-- ---------------------------------------------------------------------------
ALTER TABLE review_templates     ENABLE ROW LEVEL SECURITY;
ALTER TABLE review_templates     FORCE  ROW LEVEL SECURITY;
ALTER TABLE reviews              ENABLE ROW LEVEL SECURITY;
ALTER TABLE reviews              FORCE  ROW LEVEL SECURITY;
ALTER TABLE review_entries       ENABLE ROW LEVEL SECURITY;
ALTER TABLE review_entries       FORCE  ROW LEVEL SECURITY;
ALTER TABLE review_360_requests  ENABLE ROW LEVEL SECURITY;
ALTER TABLE review_360_requests  FORCE  ROW LEVEL SECURITY;
ALTER TABLE calibration_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE calibration_sessions FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON review_templates
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON reviews
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON review_entries
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON review_360_requests
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON calibration_sessions
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to hr_app
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE review_templates     TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE reviews              TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE review_entries       TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE review_360_requests  TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE calibration_sessions TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE calibration_sessions FROM hr_app;
REVOKE ALL ON TABLE review_360_requests  FROM hr_app;
REVOKE ALL ON TABLE review_entries       FROM hr_app;
REVOKE ALL ON TABLE reviews              FROM hr_app;
REVOKE ALL ON TABLE review_templates     FROM hr_app;

DROP TABLE IF EXISTS calibration_sessions;
DROP TABLE IF EXISTS review_360_requests;
DROP TABLE IF EXISTS review_entries;
DROP TABLE IF EXISTS reviews;
DROP TABLE IF EXISTS review_templates;

-- +goose StatementEnd
