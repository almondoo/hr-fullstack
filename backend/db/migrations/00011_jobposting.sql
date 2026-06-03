-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- ST-ATS-01: 求人票の作成・公開と求人ステータス管理 (基本ATS基盤)
-- ===========================================================================
-- This migration establishes the foundation of the ATS (Applicant Tracking
-- System) package.  job_postings is the root entity that downstream ATS
-- stories (ST-ATS-02 applicants, ST-ATS-03 selection pipeline, interviews,
-- offers) reference via composite FK.
--
-- 法令・社内規程に関する注意 (legalConfigPoints):
--   - 募集情報・選考記録の保存年限 (retention_label) は法定値ではないが社内
--     規程化が想定されるため、ハードコードせずテナント設定値として保持する。
--     最新の官公庁情報・社労士/弁護士確認のうえ設定化して改正に追従すること。
--     本実装は法的助言ではない。
--   - 募集に関する差別禁止 (年齢制限の例外要件等) のバリデーションは法令依存
--     のため、必須/任意項目セットは将来設定テーブル化し改正追従可能にする想定
--     (要専門家確認)。本ストーリーでは job_postings 本体のみを定義する。
--
-- 将来拡張ポイント (Fast-Follow / 注記):
--   - ATS-002 媒体/エージェント自動連携・外部配信は本ストーリー未実装。
--     public_published フラグと public_slug (opaque値) でスキーマ余地のみ確保。
--   - ATS-003 リファラル/タレントプールも将来拡張。

-- ---------------------------------------------------------------------------
-- job_postings (求人票)
-- ---------------------------------------------------------------------------
-- status:          'draft' | 'open' | 'on_hold' | 'closed'
--   状態機械: draft → open → on_hold ↔ open → closed (closed は終端)。
--   サービス層 (allow-list) と本 CHECK 制約で多層防御する。CHECK は値ドメイン
--   を制約し、遷移可否はサービス層の allow-list が担う。
-- employment_type: 雇用区分 (正社員/契約/パート等)。値は設定化前提でテキスト。
-- department_id:   募集部門。(department_id, tenant_id) 複合FKで departments を
--                  参照し、クロステナント割当を DB 制約で阻止する。
-- recruiter_user_id / hiring_manager_user_id:
--   採用担当 (リクルーター) / 採用マネージャの users 参照。
--   [Security] users テーブルには UNIQUE(id, tenant_id) が存在しないため
--   (00001/00002 参照)、複合FKは張れない。既存 onboarding.assignee_user_id と
--   同じ規約に従い「素の uuid 列 + index」とし、同一テナント所属の検証は
--   サービス層 (SELECT COUNT(1) FROM users WHERE id=? AND tenant_id=?) +
--   RLS で多層防御する。論理参照先: users(id) 同一テナント。
-- salary_range_min / salary_range_max / hiring_budget:
--   想定年収レンジ・採用予算。項目レベル権限 (ats:read_budget) でマネージャ
--   以上のみ閲覧可能 (サービス層で制御)。金額は整数 (円) で保持。
-- public_published: 採用サイト/LP 向け公開可能フラグ。
-- public_slug:      公開用スラッグ。連番非露出の opaque 値 (uuidベース)。
--                   UNIQUE(tenant_id, public_slug) で重複防止。
CREATE TABLE job_postings (
    id                     uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id              uuid        NOT NULL REFERENCES tenants(id),
    title                  text        NOT NULL,
    -- status: 'draft' | 'open' | 'on_hold' | 'closed'
    status                 text        NOT NULL DEFAULT 'draft',
    -- employment_type: 雇用区分 (値は設定化前提のテキスト)
    employment_type        text        NOT NULL,
    department_id          uuid        NOT NULL,
    -- recruiter_user_id / hiring_manager_user_id: 論理参照 users(id) 同一テナント
    -- (複合FK不可のため素の uuid 列 + サービス層検証 + RLS で多層防御)
    recruiter_user_id      uuid,
    hiring_manager_user_id uuid,
    -- 募集要項 (職務内容/応募資格/勤務地/勤務時間等の構造化情報)
    requirements_json      jsonb       NOT NULL DEFAULT '{}',
    -- 想定年収レンジ・採用予算 (項目レベル権限で閲覧制御)
    salary_range_min       bigint,
    salary_range_max       bigint,
    hiring_budget          bigint,
    -- retention_label: 募集情報の保存年限ラベル (法定値ではない・設定化前提)
    retention_label        text        NOT NULL DEFAULT 'unset',
    -- 公開関連 (ATS-002 外部配信は将来拡張。ここではフラグとスラッグのみ)
    public_published       boolean     NOT NULL DEFAULT false,
    public_slug            text        NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_job_postings PRIMARY KEY (id),
    CONSTRAINT chk_job_postings_status
        CHECK (status IN ('draft', 'open', 'on_hold', 'closed')),
    -- [Security] 複合FK: (department_id, tenant_id) が departments に存在する
    -- ことを強制 (クロステナント部門割当を DB 制約で阻止)。
    CONSTRAINT fk_job_postings_department_tenant
        FOREIGN KEY (department_id, tenant_id)
        REFERENCES departments(id, tenant_id)
        MATCH SIMPLE,
    -- public_slug はテナント内で一意 (連番非露出の opaque 値)。
    CONSTRAINT uq_job_postings_tenant_slug UNIQUE (tenant_id, public_slug),
    -- 下流 (applications/selection 等) の複合FK参照元にするため必須。
    CONSTRAINT uq_job_postings_id_tenant UNIQUE (id, tenant_id)
);

-- インデックス: テナント内でステータス・部門による絞り込みを高速化。
CREATE INDEX idx_job_postings_lookup
    ON job_postings (tenant_id, status, department_id);

-- ---------------------------------------------------------------------------
-- job_posting_interviewers (求人への面接官割当 / 多対多)
-- ---------------------------------------------------------------------------
-- job_posting_id: (job_posting_id, tenant_id) 複合FKで job_postings を参照。
--                 自パッケージ内表どうしの参照なので複合FKを張る。
-- user_id:        面接官の users 参照。job_postings.recruiter_user_id と同様、
--                 users に UNIQUE(id, tenant_id) が無いため複合FK不可。素の
--                 uuid 列 + サービス層検証 + RLS で多層防御。論理参照先:
--                 users(id) 同一テナント。
-- UNIQUE(tenant_id, job_posting_id, user_id) で同一面接官の重複割当を防止。
CREATE TABLE job_posting_interviewers (
    id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES tenants(id),
    job_posting_id  uuid        NOT NULL,
    -- user_id: 論理参照 users(id) 同一テナント (複合FK不可・サービス層検証)
    user_id         uuid        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_job_posting_interviewers PRIMARY KEY (id),
    -- [Security] 複合FK: (job_posting_id, tenant_id) が job_postings に存在する
    -- ことを強制 (クロステナント参照を阻止)。自パッケージ内表参照。
    CONSTRAINT fk_jpi_job_posting_tenant
        FOREIGN KEY (job_posting_id, tenant_id)
        REFERENCES job_postings(id, tenant_id)
        MATCH SIMPLE,
    -- 同一求人への同一面接官の重複割当を防止。
    CONSTRAINT uq_jpi_posting_user UNIQUE (tenant_id, job_posting_id, user_id),
    CONSTRAINT uq_jpi_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_jpi_lookup
    ON job_posting_interviewers (tenant_id, job_posting_id);

-- ---------------------------------------------------------------------------
-- RLS — 全新規表
-- ---------------------------------------------------------------------------
ALTER TABLE job_postings              ENABLE ROW LEVEL SECURITY;
ALTER TABLE job_postings              FORCE  ROW LEVEL SECURITY;
ALTER TABLE job_posting_interviewers  ENABLE ROW LEVEL SECURITY;
ALTER TABLE job_posting_interviewers  FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON job_postings
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON job_posting_interviewers
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to hr_app
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE job_postings              TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE job_posting_interviewers  TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE job_posting_interviewers  FROM hr_app;
REVOKE ALL ON TABLE job_postings              FROM hr_app;

DROP TABLE IF EXISTS job_posting_interviewers;
DROP TABLE IF EXISTS job_postings;

-- +goose StatementEnd
