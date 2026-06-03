-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- ST-TM-01  目標管理(MBO/OKR)・目標カスケード
-- ===========================================================================
-- Tables:
--   review_cycles        評価サイクル(期) — tenant-scoped config for goals/reviews.
--   goals                個人/組織/部署目標 (MBO or OKR). 自己参照でカスケード。
--   key_results          OKR の KeyResult (goals の子).
--   goal_progress_logs   進捗更新履歴 (追記専用).
--
-- 設計方針(法令値ではないが企業制度差分の設定化):
--   評価手法(MBO/OKR)・ウェイト100%強制(require_weight_100)・進捗算出方式・
--   カスケード最大深さ(max_cascade_depth)はテナント別設定として review_cycles に
--   保持し、アプリ層でハードコードしない。制度差分は設定で追従する。
--   本実装は法的助言ではなく、評価制度の妥当性は各社の人事/社労士確認が前提。

-- ---------------------------------------------------------------------------
-- review_cycles (評価サイクル / 期)
-- ---------------------------------------------------------------------------
-- ST-TM-02 (評価WF) と共用する想定の「期」マスタ。
-- status: draft/active/closed の CHECK 制約で活性化を管理する。
-- 同一テナント内で期間重複は許容する(明示的な期間ユニーク制約は張らない)。
-- require_weight_100: 提出(submitted)時に MBO ウェイト合計100%を必須とするか(設定)。
-- progress_method:    OKR Objective 進捗の算出方式 (average | weighted) の設定。
-- max_cascade_depth:  カスケード親子チェーンの最大深さ(循環検出/暴走防止の設定)。
CREATE TABLE review_cycles (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    name                text        NOT NULL,
    starts_on           date        NOT NULL,
    ends_on             date        NOT NULL,
    goal_due_on         date,
    review_due_on       date,
    -- status: "draft" | "active" | "closed"
    status              text        NOT NULL DEFAULT 'draft',
    -- Tenant-configurable policy (NOT hardcoded in the application layer).
    require_weight_100  boolean     NOT NULL DEFAULT false,
    -- progress_method: "average" | "weighted"
    progress_method     text        NOT NULL DEFAULT 'average',
    max_cascade_depth   integer     NOT NULL DEFAULT 10,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_review_cycles PRIMARY KEY (id),
    CONSTRAINT chk_review_cycles_status
        CHECK (status IN ('draft', 'active', 'closed')),
    CONSTRAINT chk_review_cycles_progress_method
        CHECK (progress_method IN ('average', 'weighted')),
    CONSTRAINT chk_review_cycles_cascade_depth
        CHECK (max_cascade_depth >= 1 AND max_cascade_depth <= 100),
    -- UNIQUE(id, tenant_id) required for downstream composite FK references.
    CONSTRAINT uq_review_cycles_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_review_cycles_lookup
    ON review_cycles (tenant_id, status, starts_on);

-- ---------------------------------------------------------------------------
-- goals (目標 — MBO / OKR)
-- ---------------------------------------------------------------------------
-- method:  "mbo" | "okr" の CHECK。
-- status:  目標設定の有限状態機械 (draft→submitted→approved→in_progress→achieved/closed)。
--          差戻しで submitted→draft。提出/承認は approval パッケージ経由。
-- weight:  MBO 用ウェイト(%) numeric(5,2)。OKR 目標では NULL 可。
-- self_rating: 自己評価ランク(自由記述ラベル)。
-- parent_goal_id: カスケード(上位目標)への自己参照。複合FKで越境を防止。
-- approval_request_id: approval パッケージの申請ID(論理参照, 素のuuid + indexのみ)。
CREATE TABLE goals (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    cycle_id            uuid        NOT NULL,
    employee_id         uuid        NOT NULL,
    parent_goal_id      uuid,
    -- method: "mbo" | "okr"
    method              text        NOT NULL,
    title               text        NOT NULL,
    description         text        NOT NULL DEFAULT '',
    weight              numeric(5,2),
    -- status: see FSM above.
    status              text        NOT NULL DEFAULT 'draft',
    self_rating         text,
    -- progress_pct: latest reflected progress (0..100). For OKR derived from
    -- KeyResults; for MBO updated via goal_progress_logs.
    progress_pct        numeric(5,2) NOT NULL DEFAULT 0,
    -- approval_request_id: logical reference to approval_requests(id) in the
    -- approval package.  Bare uuid + index only (no FK across story tables).
    approval_request_id uuid,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_goals PRIMARY KEY (id),
    CONSTRAINT chk_goals_method
        CHECK (method IN ('mbo', 'okr')),
    CONSTRAINT chk_goals_status
        CHECK (status IN ('draft', 'submitted', 'approved', 'in_progress', 'achieved', 'closed')),
    CONSTRAINT chk_goals_weight
        CHECK (weight IS NULL OR (weight >= 0 AND weight <= 100)),
    CONSTRAINT chk_goals_progress_pct
        CHECK (progress_pct >= 0 AND progress_pct <= 100),
    -- A goal cannot be its own parent (direct self-cycle); multi-level cycles
    -- are prevented in the application layer (recursive ancestor traversal).
    CONSTRAINT chk_goals_not_self_parent
        CHECK (parent_goal_id IS NULL OR parent_goal_id <> id),
    -- [Security] Composite FK: (cycle_id, tenant_id) must exist in review_cycles.
    CONSTRAINT fk_goals_cycle_tenant
        FOREIGN KEY (cycle_id, tenant_id)
        REFERENCES review_cycles(id, tenant_id)
        MATCH SIMPLE,
    -- [Security] Composite FK: (employee_id, tenant_id) must exist in employees.
    CONSTRAINT fk_goals_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- [Security] Self-referential composite FK for cascade: (parent_goal_id,
    -- tenant_id) must exist in goals.  This blocks cross-tenant parent links at
    -- the DB layer (a parent goal in another tenant cannot be referenced).
    CONSTRAINT fk_goals_parent_tenant
        FOREIGN KEY (parent_goal_id, tenant_id)
        REFERENCES goals(id, tenant_id)
        MATCH SIMPLE,
    -- UNIQUE(id, tenant_id) for downstream composite FK references
    -- (key_results, goal_progress_logs, and the self-reference above).
    CONSTRAINT uq_goals_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_goals_lookup
    ON goals (tenant_id, cycle_id, employee_id, status);

CREATE INDEX idx_goals_parent
    ON goals (tenant_id, parent_goal_id);

CREATE INDEX idx_goals_approval_request
    ON goals (tenant_id, approval_request_id);

-- ---------------------------------------------------------------------------
-- key_results (OKR の KeyResult)
-- ---------------------------------------------------------------------------
-- 複合FK fk_kr_goal_tenant → goals(id, tenant_id)。
-- progress_pct はアプリ層で (current-start)/(target-start) から算出し保存
-- (0除算ガード・clamp 0..100)。
CREATE TABLE key_results (
    id              uuid          NOT NULL DEFAULT gen_random_uuid(),
    tenant_id       uuid          NOT NULL REFERENCES tenants(id),
    goal_id         uuid          NOT NULL,
    title           text          NOT NULL,
    metric_unit     text          NOT NULL DEFAULT '',
    start_value     numeric(18,4) NOT NULL DEFAULT 0,
    target_value    numeric(18,4) NOT NULL DEFAULT 0,
    current_value   numeric(18,4) NOT NULL DEFAULT 0,
    progress_pct    numeric(5,2)  NOT NULL DEFAULT 0,
    created_at      timestamptz   NOT NULL DEFAULT now(),
    updated_at      timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT pk_key_results PRIMARY KEY (id),
    CONSTRAINT chk_key_results_progress_pct
        CHECK (progress_pct >= 0 AND progress_pct <= 100),
    -- [Security] Composite FK: (goal_id, tenant_id) must exist in goals.
    CONSTRAINT fk_key_results_goal_tenant
        FOREIGN KEY (goal_id, tenant_id)
        REFERENCES goals(id, tenant_id)
        MATCH SIMPLE,
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_key_results_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_key_results_lookup
    ON key_results (tenant_id, goal_id);

-- ---------------------------------------------------------------------------
-- goal_progress_logs (進捗更新履歴 — 追記専用)
-- ---------------------------------------------------------------------------
-- 誰が・いつ・進捗値・コメント を残す追記専用テーブル。
-- key_result_id は nullable (MBO 目標全体の進捗更新時は NULL)。
-- updated_by_user_id: 論理参照 (users(id)). 素のuuid列 (越境はRLS+goal複合FKで担保)。
CREATE TABLE goal_progress_logs (
    id                  uuid         NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid         NOT NULL REFERENCES tenants(id),
    goal_id             uuid         NOT NULL,
    key_result_id       uuid,
    progress_pct        numeric(5,2) NOT NULL DEFAULT 0,
    comment             text         NOT NULL DEFAULT '',
    updated_by_user_id  uuid,
    created_at          timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_goal_progress_logs PRIMARY KEY (id),
    CONSTRAINT chk_goal_progress_logs_progress_pct
        CHECK (progress_pct >= 0 AND progress_pct <= 100),
    -- [Security] Composite FK: (goal_id, tenant_id) must exist in goals.
    CONSTRAINT fk_goal_progress_logs_goal_tenant
        FOREIGN KEY (goal_id, tenant_id)
        REFERENCES goals(id, tenant_id)
        MATCH SIMPLE,
    -- [Security] Composite FK: (key_result_id, tenant_id) must exist in
    -- key_results when provided (nullable for MBO whole-goal progress updates).
    CONSTRAINT fk_goal_progress_logs_kr_tenant
        FOREIGN KEY (key_result_id, tenant_id)
        REFERENCES key_results(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_goal_progress_logs_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_goal_progress_logs_lookup
    ON goal_progress_logs (tenant_id, goal_id, created_at);

-- ---------------------------------------------------------------------------
-- RLS — all new tables
-- ---------------------------------------------------------------------------
ALTER TABLE review_cycles      ENABLE ROW LEVEL SECURITY;
ALTER TABLE review_cycles      FORCE  ROW LEVEL SECURITY;
ALTER TABLE goals              ENABLE ROW LEVEL SECURITY;
ALTER TABLE goals              FORCE  ROW LEVEL SECURITY;
ALTER TABLE key_results        ENABLE ROW LEVEL SECURITY;
ALTER TABLE key_results        FORCE  ROW LEVEL SECURITY;
ALTER TABLE goal_progress_logs ENABLE ROW LEVEL SECURITY;
ALTER TABLE goal_progress_logs FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON review_cycles
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON goals
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON key_results
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON goal_progress_logs
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to hr_app
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE review_cycles      TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE goals              TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE key_results        TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE goal_progress_logs TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE goal_progress_logs FROM hr_app;
REVOKE ALL ON TABLE key_results        FROM hr_app;
REVOKE ALL ON TABLE goals              FROM hr_app;
REVOKE ALL ON TABLE review_cycles      FROM hr_app;

DROP TABLE IF EXISTS goal_progress_logs;
DROP TABLE IF EXISTS key_results;
DROP TABLE IF EXISTS goals;
DROP TABLE IF EXISTS review_cycles;

-- +goose StatementEnd
