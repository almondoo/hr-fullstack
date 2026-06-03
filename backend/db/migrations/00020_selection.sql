-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- ST-ATS-03 選考パイプライン
--   ステージ定義(求人別/テナント標準テンプレ) / 応募(application) /
--   ステージ遷移履歴 / 候補者通知テンプレ
--
-- 法令注記:
--   不採用理由(reason)の記録方針・必須化・選択肢、保持期限(retention)は
--   差別禁止(年齢・性別等 CMP-004 募集差別禁止)および個人情報の利用目的
--   (ST-ATS-02 consent)と整合させる必要があり、最新の官公庁情報・社労士/
--   弁護士確認が前提。閾値/保持年限/必須化フラグはハードコードせず設定テーブル
--   (selection_pipeline_settings)から取得し改正追従する。本実装は法的助言ではない。
--
-- 越境結合防止の方針(隔離規約):
--   - 自パッケージ内の表どうしの参照は複合FK (x_id, tenant_id)
--     REFERENCES own_table(id, tenant_id) を張る。
--   - 他ストーリー表(job_postings=ST-ATS-01 / applicants=ST-ATS-02)への
--     参照は「素の uuid 列 + index のみ」とし FK を張らない。同一テナント
--     所属検証はサービス層(SELECT COUNT(1) ... WHERE id=? AND tenant_id=?)で行う。
--   - users(id) は UNIQUE(id, tenant_id) を持たないため複合FK不可。素の uuid 列。
-- ===========================================================================

-- ---------------------------------------------------------------------------
-- selection_stage_templates (テナント標準 選考ステージテンプレ)
-- ---------------------------------------------------------------------------
-- テナント標準の選考ステージ列テンプレ。求人へステージ初期化する際に複製元と
-- なる。stages_json は順序付きステージ定義の配列。
-- Format: [{"name":"書類選考","stage_type":"screening","position":0}, ...]
CREATE TABLE selection_stage_templates (
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES tenants(id),
    name        text        NOT NULL,
    stages_json jsonb       NOT NULL DEFAULT '[]',
    active      boolean     NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_selection_stage_templates PRIMARY KEY (id),
    -- UNIQUE(id, tenant_id): 下流の複合FK参照用(将来の参照に備える)。
    CONSTRAINT uq_selection_stage_templates_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_selection_stage_templates_lookup
    ON selection_stage_templates (tenant_id, active);

-- ---------------------------------------------------------------------------
-- selection_stages (求人別 選考ステージ定義)
-- ---------------------------------------------------------------------------
-- 求人(job_posting)ごとの順序付き選考ステージ。position で順序を表す。
-- stage_type で終端/分岐判定:
--   'screening' | 'interview' | 'offer' | 'hired' | 'rejected'
--   'hired' / 'rejected' は終端 stage_type。到達時に application.status を確定。
-- job_posting_id は ST-ATS-01 (00011 job_postings) の論理参照。複合FKは張らない
-- (他ストーリー表)。同一テナント所属検証はサービス層で行う。
CREATE TABLE selection_stages (
    id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES tenants(id),
    -- 論理参照: job_postings(id) 同一テナント (ST-ATS-01 / 00011)。複合FKは張らない。
    job_posting_id  uuid        NOT NULL,
    position        integer     NOT NULL,
    name            text        NOT NULL,
    -- stage_type: 'screening' | 'interview' | 'offer' | 'hired' | 'rejected'
    stage_type      text        NOT NULL DEFAULT 'screening',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_selection_stages PRIMARY KEY (id),
    CONSTRAINT chk_selection_stages_stage_type
        CHECK (stage_type IN ('screening', 'interview', 'offer', 'hired', 'rejected')),
    CONSTRAINT chk_selection_stages_position_nonneg
        CHECK (position >= 0),
    -- 同一求人内でステージ順序は一意(順序付けの一貫性を DB 層で保証)。
    CONSTRAINT uq_selection_stages_job_position
        UNIQUE (tenant_id, job_posting_id, position),
    -- UNIQUE(id, tenant_id): applications.current_stage_id / history の複合FK参照用。
    CONSTRAINT uq_selection_stages_id_tenant UNIQUE (id, tenant_id)
);

-- カンバン集計/求人別ステージ取得用(論理参照先の絞り込み)。
CREATE INDEX idx_selection_stages_job
    ON selection_stages (tenant_id, job_posting_id, position);

-- ---------------------------------------------------------------------------
-- applications (応募 = 求人 × 応募者 の選考エンティティ)
-- ---------------------------------------------------------------------------
-- 応募者の選考状態を管理する中核エンティティ。current_stage_id は現在ステージ。
-- status: 'in_progress' | 'rejected' | 'withdrawn' | 'hired'
-- retention_label / retention_expires_on:
--   不採用(rejected)遷移時に ST-ATS-02 の retention/consent ポリシーに従い
--   保持期限を設定する(設定値由来・ハードコード禁止・CMP-004)。
-- applicant_id / job_posting_id は他ストーリー表(ST-ATS-02 / ST-ATS-01)の
--   論理参照。複合FKは張らない。同一テナント所属検証はサービス層で行う。
CREATE TABLE applications (
    id                    uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id             uuid        NOT NULL REFERENCES tenants(id),
    -- 論理参照: job_postings(id) 同一テナント (ST-ATS-01 / 00011)。複合FKは張らない。
    job_posting_id        uuid        NOT NULL,
    -- 論理参照: applicants(id) 同一テナント (ST-ATS-02 / 00018)。複合FKは張らない。
    applicant_id          uuid        NOT NULL,
    -- current_stage_id: 現在ステージ。自パッケージ表のため複合FKを張る(下部 CONSTRAINT)。
    current_stage_id      uuid,
    -- status: 'in_progress' | 'rejected' | 'withdrawn' | 'hired'
    status                text        NOT NULL DEFAULT 'in_progress',
    -- retention_label: テナント設定参照(ハードコード禁止・CMP-004)。
    retention_label       text        NOT NULL DEFAULT 'unset',
    retention_expires_on  date,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_applications PRIMARY KEY (id),
    CONSTRAINT chk_applications_status
        CHECK (status IN ('in_progress', 'rejected', 'withdrawn', 'hired')),
    -- [Security] 複合FK: (current_stage_id, tenant_id) が selection_stages に存在
    -- することを強制(自パッケージ内・クロステナントなステージ参照を DB 層で阻止)。
    CONSTRAINT fk_applications_current_stage_tenant
        FOREIGN KEY (current_stage_id, tenant_id)
        REFERENCES selection_stages(id, tenant_id)
        MATCH SIMPLE,
    -- 同一求人への同一応募者の重複応募を防止。
    CONSTRAINT uq_applications_job_applicant
        UNIQUE (tenant_id, job_posting_id, applicant_id),
    -- UNIQUE(id, tenant_id): history / 面接 / オファーの複合FK参照元。
    CONSTRAINT uq_applications_id_tenant UNIQUE (id, tenant_id)
);

-- カンバン集計の N+1 回避用(求人スコープ × 現在ステージ)。
CREATE INDEX idx_applications_kanban
    ON applications (tenant_id, job_posting_id, current_stage_id);

-- 応募者別の応募検索用。
CREATE INDEX idx_applications_applicant
    ON applications (tenant_id, applicant_id);

-- ---------------------------------------------------------------------------
-- application_stage_history (ステージ遷移履歴 = 進捗の証跡)
-- ---------------------------------------------------------------------------
-- ステージ遷移ごとの履歴。実施ユーザ・日時・理由を残し監査と整合させる。
-- from_stage_id / to_stage_id は自パッケージ表 selection_stages の参照。
--   from_stage_id は初回遷移で NULL(エントリ)になり得るため NULL 許容。
-- moved_by は実行ユーザ。users(id) 同一テナントの論理参照(複合FK不可・素の uuid)。
-- reason は不採用理由等。差別的記述を避ける運用注記(CMP-004)。PII は監査には残さない。
CREATE TABLE application_stage_history (
    id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES tenants(id),
    application_id  uuid        NOT NULL,
    from_stage_id   uuid,
    to_stage_id     uuid        NOT NULL,
    -- moved_by: 実行ユーザ。users(id) 同一テナント論理参照(UNIQUE(id,tenant_id)
    -- が無いため複合FK不可)。同一テナント検証はサービス層で行う。
    moved_by        uuid,
    moved_at        timestamptz NOT NULL DEFAULT now(),
    reason          text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_application_stage_history PRIMARY KEY (id),
    -- [Security] 複合FK: (application_id, tenant_id) が applications に存在することを強制。
    CONSTRAINT fk_application_stage_history_application_tenant
        FOREIGN KEY (application_id, tenant_id)
        REFERENCES applications(id, tenant_id)
        MATCH SIMPLE,
    -- [Security] 複合FK: (to_stage_id, tenant_id) が selection_stages に存在することを強制。
    CONSTRAINT fk_application_stage_history_to_stage_tenant
        FOREIGN KEY (to_stage_id, tenant_id)
        REFERENCES selection_stages(id, tenant_id)
        MATCH SIMPLE,
    -- [Security] 複合FK: (from_stage_id, tenant_id) が selection_stages に存在することを強制
    -- (NULL の場合は MATCH SIMPLE により検証スキップ=エントリ遷移)。
    CONSTRAINT fk_application_stage_history_from_stage_tenant
        FOREIGN KEY (from_stage_id, tenant_id)
        REFERENCES selection_stages(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_application_stage_history_id_tenant UNIQUE (id, tenant_id)
);

-- 応募の履歴一覧(時系列)取得用。
CREATE INDEX idx_application_stage_history_app
    ON application_stage_history (tenant_id, application_id, moved_at);

-- ---------------------------------------------------------------------------
-- candidate_message_templates (候補者向け ステータス通知テンプレ ATS-014)
-- ---------------------------------------------------------------------------
-- ステージ到達トリガで差し込む候補者向けテンプレメール。本文はプレースホルダ方式。
-- 実送信は通知基盤 ST-FND-09 へ委譲(本パッケージはフックのみ)。
-- stage_type 到達トリガとの紐付けで自動差込み。
-- 利用目的は候補者連絡範囲内(ST-ATS-02 consent と整合・CMP-004)。
CREATE TABLE candidate_message_templates (
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES tenants(id),
    -- stage_type: 到達トリガとなるステージ種別
    --   'screening' | 'interview' | 'offer' | 'hired' | 'rejected'
    stage_type  text        NOT NULL,
    name        text        NOT NULL,
    subject     text        NOT NULL DEFAULT '',
    -- body: プレースホルダ方式の本文(例: {{candidate_name}})。氏名等の実 PII は
    -- テンプレ本文には埋め込まず、送信時に通知基盤側で差し替える。
    body        text        NOT NULL DEFAULT '',
    active      boolean     NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_candidate_message_templates PRIMARY KEY (id),
    CONSTRAINT chk_candidate_message_templates_stage_type
        CHECK (stage_type IN ('screening', 'interview', 'offer', 'hired', 'rejected')),
    -- stage_type ごとに有効テンプレは1件(自動差込みの解決を一意化)。
    -- 部分 UNIQUE インデックスで active=true のみに制約。
    CONSTRAINT uq_candidate_message_templates_id_tenant UNIQUE (id, tenant_id)
);

CREATE UNIQUE INDEX uq_candidate_message_templates_active_stage
    ON candidate_message_templates (tenant_id, stage_type)
    WHERE active = true;

CREATE INDEX idx_candidate_message_templates_lookup
    ON candidate_message_templates (tenant_id, stage_type, active);

-- ===========================================================================
-- RLS — 全テーブル(テナント分離)
-- ===========================================================================
ALTER TABLE selection_stage_templates    ENABLE ROW LEVEL SECURITY;
ALTER TABLE selection_stage_templates    FORCE  ROW LEVEL SECURITY;
ALTER TABLE selection_stages             ENABLE ROW LEVEL SECURITY;
ALTER TABLE selection_stages             FORCE  ROW LEVEL SECURITY;
ALTER TABLE applications                 ENABLE ROW LEVEL SECURITY;
ALTER TABLE applications                 FORCE  ROW LEVEL SECURITY;
ALTER TABLE application_stage_history    ENABLE ROW LEVEL SECURITY;
ALTER TABLE application_stage_history    FORCE  ROW LEVEL SECURITY;
ALTER TABLE candidate_message_templates  ENABLE ROW LEVEL SECURITY;
ALTER TABLE candidate_message_templates  FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON selection_stage_templates
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON selection_stages
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON applications
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON application_stage_history
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON candidate_message_templates
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ===========================================================================
-- Grants to hr_app
-- ===========================================================================
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE selection_stage_templates    TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE selection_stages             TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE applications                 TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE application_stage_history    TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE candidate_message_templates  TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE candidate_message_templates  FROM hr_app;
REVOKE ALL ON TABLE application_stage_history    FROM hr_app;
REVOKE ALL ON TABLE applications                 FROM hr_app;
REVOKE ALL ON TABLE selection_stages             FROM hr_app;
REVOKE ALL ON TABLE selection_stage_templates    FROM hr_app;

DROP TABLE IF EXISTS candidate_message_templates;
DROP TABLE IF EXISTS application_stage_history;
DROP TABLE IF EXISTS applications;
DROP TABLE IF EXISTS selection_stages;
DROP TABLE IF EXISTS selection_stage_templates;

-- +goose StatementEnd
