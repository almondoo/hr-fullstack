-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- ST-ATS-02: 応募者データベース
--   (履歴書/職務経歴取込・重複統合・PII保持/同意管理)
-- ===========================================================================
-- ATS-010 (応募者DB・履歴書/職務経歴の取込・解析・重複統合) +
-- ATS-017 (候補者個人情報・保持期間/同意管理・不採用者データ取扱い) +
-- CMP-004 (個人情報保護法・利用目的・保持/削除)。
--
-- 応募者(候補者)は内定前の社外個人であり要保護PIIのため、以下を徹底する:
--   - 連絡先(email/phone)は AES-256-GCM 列暗号(bytea, *_enc)で保管し、
--     ats:applicant:read_sensitive 権限でのみアプリ層復号する(平文は永続化禁止)。
--   - 取得同意(consent)・利用目的(purpose)・保持期限(retention)を明示管理する。
--   - 不採用(rejected)後の保持期限経過データは「物理削除」ではなく
--     「論理失効(匿名化/アクセス制限)」で扱う。本migrationでは行を削除する
--     バックグラウンドジョブは含めず、保持ラベルと失効予定日(retention)を記録する
--     スキーマのみを提供する(既存 offboarding_policies 方式を踏襲)。
--
-- 法令・社内規程に関する注意 (legalConfigPoints):
--   - 候補者データ保持期間(不採用者含む)は法定一律値がないため、retention_label を
--     テナント設定値として保持し(例: 不採用後6ヶ月/1年)、ハードコードしない。
--     最新の官公庁情報・社労士/弁護士確認のうえ設定化して改正・社内規程に追従すること。
--     本実装は法的助言ではない (CMP-004)。
--   - 利用目的の選択肢(purpose)・同意要否は個人情報保護法依存のため設定化前提とし、
--     要専門家確認。purpose 列は値ドメインを CHECK で固定せずテキストで保持する。
--   - 越境移転(再委託先含む)が絡む媒体/エージェント連携は Fast-Follow。同意の有無を
--     applicant_consents で証跡化できる設計余地を残す (CMP-004)。
--
-- 将来拡張ポイント (Fast-Follow / 注記):
--   - 履歴書本文の自動解析(パース)・外部AI解析は本ストーリー未実装。
--     applicant_documents.file_ref(opaque参照)でファイル保管基盤(ST-FND-10想定)
--     連携の余地のみ確保し、ファイル本体PIIはDB列に展開しない。

-- ---------------------------------------------------------------------------
-- applicants (応募者/候補者 本体)
-- ---------------------------------------------------------------------------
-- 社外個人PII:
--   - email_enc / phone_enc: AES-256-GCM 暗号文(bytea)。平文は DB に保存しない。
--     復号は ats:applicant:read_sensitive 権限保持時のみアプリ層で実施する。
--     SECURITY: email/phone の平文 text 列を(一時的にも)追加してはならない。
--   - last_name / first_name: 表示必須のため text。利用目的限定で取扱う。
--   - email_normalized: 重複検知用の正規化メール(小文字化等)。検知補助のための
--     ハッシュ/正規化値であり、生メールの平文保管ではない用途(突合キー)に限定。
--     [Future] よりプライバシー保護を強める場合はソルト付きHMAC等に置換する余地。
--   - birth_date: 重複検知(氏名+生年月日)補助のため保持。
--
-- job_posting_id:
--   論理参照先 job_postings(id) 同一テナント(ST-ATS-01 / 00011)。
--   他ストーリー表への参照のため複合FKは張らず「素の uuid 列 + index」とし、
--   同一テナント所属検証はサービス層(SELECT COUNT(1) ... WHERE id=? AND tenant_id=?)+
--   RLS で多層防御する。タレントプール由来は NULL 許容。
--
-- merged_into_id:
--   自己参照(統合先 applicant)。自パッケージ内表のため複合FK
--   (merged_into_id, tenant_id) REFERENCES applicants(id, tenant_id) を張る。
--   非NULLのとき当該レコードは論理的に merged(統合済み)とみなす。物理削除はしない。
--
-- status:         'applied' | 'screening' | 'interviewing' | 'offered'
--                 | 'hired' | 'rejected' | 'withdrawn'
--   値ドメインを CHECK で固定する。遷移可否はサービス層 allow-list が担う(多層防御)。
-- consent_status: 'granted' | 'withdrawn' | 'unknown'
--   同意の現状態。詳細な利用目的別証跡は applicant_consents に保持する。
-- source:         'direct' | 'agent' | 'referral' | 'job_board' | 'other'
--   応募媒体/経路。
-- retention_label: 保持期間ラベル(テナント設定参照・ハードコード禁止・CMP-004)。
-- retention_expires_on: 保持期限(論理失効予定日)。経過後はアクセス制限/匿名化対象。
-- anonymized_at:   論理失効(匿名化/アクセス制限)を実施した時刻。NULL=未失効。
--                  物理削除はしない(行は残す)。
CREATE TABLE applicants (
    id                    uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id             uuid        NOT NULL REFERENCES tenants(id),
    -- 論理参照: job_postings(id) 同一テナント (ST-ATS-01 / 00011)。複合FKは張らない。
    job_posting_id        uuid,
    -- 自己参照(統合先)。複合FKは下部 CONSTRAINT で張る。
    merged_into_id        uuid,
    last_name             text        NOT NULL,
    first_name            text        NOT NULL,
    -- email_normalized: 重複検知用の正規化メール突合キー(生メール平文保管ではない)
    email_normalized      text,
    birth_date            date,
    -- email_enc / phone_enc: AES-256-GCM 暗号文。平文 text 列は追加禁止。
    email_enc             bytea,
    phone_enc             bytea,
    -- status: 'applied' | 'screening' | 'interviewing' | 'offered' | 'hired'
    --         | 'rejected' | 'withdrawn'
    status                text        NOT NULL DEFAULT 'applied',
    -- consent_status: 'granted' | 'withdrawn' | 'unknown'
    consent_status        text        NOT NULL DEFAULT 'unknown',
    -- source: 'direct' | 'agent' | 'referral' | 'job_board' | 'other'
    source                text        NOT NULL DEFAULT 'direct',
    -- retention_label: テナント設定参照(ハードコード禁止・CMP-004)。
    retention_label       text        NOT NULL DEFAULT 'unset',
    retention_expires_on  date,
    anonymized_at         timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_applicants PRIMARY KEY (id),
    CONSTRAINT chk_applicants_status
        CHECK (status IN ('applied', 'screening', 'interviewing', 'offered',
                          'hired', 'rejected', 'withdrawn')),
    CONSTRAINT chk_applicants_consent_status
        CHECK (consent_status IN ('granted', 'withdrawn', 'unknown')),
    CONSTRAINT chk_applicants_source
        CHECK (source IN ('direct', 'agent', 'referral', 'job_board', 'other')),
    -- [Security] 自己参照の複合FK: (merged_into_id, tenant_id) が applicants に存在
    -- することを強制(クロステナント統合を DB 層で阻止)。
    CONSTRAINT fk_applicants_merged_into_tenant
        FOREIGN KEY (merged_into_id, tenant_id)
        REFERENCES applicants(id, tenant_id)
        MATCH SIMPLE,
    -- UNIQUE(id, tenant_id): 自己参照・下流(documents/consents/merges)の複合FK用。
    CONSTRAINT uq_applicants_id_tenant UNIQUE (id, tenant_id)
);

-- job_posting 別の応募者検索用(論理参照先の絞り込み)。
CREATE INDEX idx_applicants_job_posting
    ON applicants (tenant_id, job_posting_id);

-- 重複検知(正規化メール突合)用。
CREATE INDEX idx_applicants_email_normalized
    ON applicants (tenant_id, email_normalized);

-- 一覧/ステータス絞り込み用。
CREATE INDEX idx_applicants_status
    ON applicants (tenant_id, status);

-- ---------------------------------------------------------------------------
-- applicant_documents (履歴書/職務経歴書/添付)
-- ---------------------------------------------------------------------------
-- file_ref:
--   ファイル保管基盤(ST-FND-10想定)の opaque 参照のみ保持。ファイル本体PIIは
--   DB 列に展開しない。ファイル本体の暗号化・版管理はファイル保管基盤側の責務。
-- doc_type: 'resume' | 'cv' | 'portfolio' | 'other'
CREATE TABLE applicant_documents (
    id            uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES tenants(id),
    applicant_id  uuid        NOT NULL,
    -- doc_type: 'resume' | 'cv' | 'portfolio' | 'other'
    doc_type      text        NOT NULL DEFAULT 'resume',
    -- file_ref: ファイル保管基盤の opaque 参照(PII本体は展開しない)
    file_ref      text        NOT NULL,
    -- file_name: 表示用の論理名(任意)。PII を含み得るため利用目的限定。
    file_name     text        NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_applicant_documents PRIMARY KEY (id),
    CONSTRAINT chk_applicant_documents_doc_type
        CHECK (doc_type IN ('resume', 'cv', 'portfolio', 'other')),
    -- [Security] 複合FK: (applicant_id, tenant_id) が applicants に存在することを強制。
    CONSTRAINT fk_applicant_documents_applicant_tenant
        FOREIGN KEY (applicant_id, tenant_id)
        REFERENCES applicants(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_applicant_documents_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_applicant_documents_applicant
    ON applicant_documents (tenant_id, applicant_id, doc_type);

-- ---------------------------------------------------------------------------
-- applicant_consents (利用目的別 同意取得/撤回 履歴)
-- ---------------------------------------------------------------------------
-- 個人情報保護法の利用目的明示・同意管理 (CMP-004)。
-- purpose:
--   利用目的(例: 採用選考 / タレントプール保持 / 再連絡 等)。値ドメインは
--   法令/社内規程依存で設定化前提のため CHECK では固定せず text で保持する
--   (要専門家確認)。
-- granted_at / withdrawn_at:
--   取得/撤回のタイムスタンプで証跡化。withdrawn_at が非NULLのとき当該目的は撤回済み。
-- cross_border:
--   越境移転(再委託先含む)同意の有無フラグ。Fast-Follow 用の設計余地 (CMP-004)。
CREATE TABLE applicant_consents (
    id            uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES tenants(id),
    applicant_id  uuid        NOT NULL,
    -- purpose: 利用目的(設定化前提・法令依存のため値ドメインは固定しない)
    purpose       text        NOT NULL,
    granted_at    timestamptz,
    withdrawn_at  timestamptz,
    -- cross_border: 越境移転同意の有無(Fast-Follow 設計余地・CMP-004)
    cross_border  boolean     NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_applicant_consents PRIMARY KEY (id),
    -- [Security] 複合FK: (applicant_id, tenant_id) が applicants に存在することを強制。
    CONSTRAINT fk_applicant_consents_applicant_tenant
        FOREIGN KEY (applicant_id, tenant_id)
        REFERENCES applicants(id, tenant_id)
        MATCH SIMPLE,
    -- 同一応募者・同一目的は1行(最新状態が支配)。再取得は UPDATE で扱う。
    CONSTRAINT uq_applicant_consents_applicant_purpose
        UNIQUE (applicant_id, tenant_id, purpose),
    CONSTRAINT uq_applicant_consents_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_applicant_consents_applicant
    ON applicant_consents (tenant_id, applicant_id);

-- ---------------------------------------------------------------------------
-- applicant_merges (重複統合 監査用履歴)
-- ---------------------------------------------------------------------------
-- 重複統合(マージ)の証跡。元レコードは論理的に merged 扱いとし物理削除しない。
-- source_applicant_id / target_applicant_id:
--   いずれも (applicant_id, tenant_id) 複合FKで applicants を参照。
-- merged_by:
--   実行ユーザ。論理参照先 users(id) 同一テナント。users には UNIQUE(id, tenant_id)
--   が無いため(00001/00002 参照)複合FKは張れず「素の uuid 列」とし、同一テナント
--   所属検証はサービス層(SELECT COUNT(1) FROM users WHERE id=? AND tenant_id=?)+
--   RLS で多層防御する。
CREATE TABLE applicant_merges (
    id                   uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id            uuid        NOT NULL REFERENCES tenants(id),
    source_applicant_id  uuid        NOT NULL,
    target_applicant_id  uuid        NOT NULL,
    -- merged_by: 論理参照 users(id) 同一テナント(複合FK不可・素の uuid 列)
    merged_by            uuid,
    merged_at            timestamptz NOT NULL DEFAULT now(),
    -- notes: マージ判断のメモ(任意)。PII を含み得るため利用目的限定。
    notes                text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_applicant_merges PRIMARY KEY (id),
    -- source と target は異なる応募者でなければならない(自己マージ防止)。
    CONSTRAINT chk_applicant_merges_distinct
        CHECK (source_applicant_id <> target_applicant_id),
    -- [Security] 複合FK: source/target いずれも (applicant_id, tenant_id) を強制。
    CONSTRAINT fk_applicant_merges_source_tenant
        FOREIGN KEY (source_applicant_id, tenant_id)
        REFERENCES applicants(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT fk_applicant_merges_target_tenant
        FOREIGN KEY (target_applicant_id, tenant_id)
        REFERENCES applicants(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_applicant_merges_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_applicant_merges_target
    ON applicant_merges (tenant_id, target_applicant_id);

CREATE INDEX idx_applicant_merges_source
    ON applicant_merges (tenant_id, source_applicant_id);

-- ---------------------------------------------------------------------------
-- RLS — all new tables
-- ---------------------------------------------------------------------------
ALTER TABLE applicants          ENABLE ROW LEVEL SECURITY;
ALTER TABLE applicants          FORCE  ROW LEVEL SECURITY;
ALTER TABLE applicant_documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE applicant_documents FORCE  ROW LEVEL SECURITY;
ALTER TABLE applicant_consents  ENABLE ROW LEVEL SECURITY;
ALTER TABLE applicant_consents  FORCE  ROW LEVEL SECURITY;
ALTER TABLE applicant_merges    ENABLE ROW LEVEL SECURITY;
ALTER TABLE applicant_merges    FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON applicants
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON applicant_documents
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON applicant_consents
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON applicant_merges
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to hr_app
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE applicants          TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE applicant_documents TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE applicant_consents  TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE applicant_merges    TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE applicant_merges    FROM hr_app;
REVOKE ALL ON TABLE applicant_consents  FROM hr_app;
REVOKE ALL ON TABLE applicant_documents FROM hr_app;
REVOKE ALL ON TABLE applicants          FROM hr_app;

DROP TABLE IF EXISTS applicant_merges;
DROP TABLE IF EXISTS applicant_consents;
DROP TABLE IF EXISTS applicant_documents;
DROP TABLE IF EXISTS applicants;

-- +goose StatementEnd
