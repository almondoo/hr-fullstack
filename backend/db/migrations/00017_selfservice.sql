-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- ST-FND-10: 従業員セルフサービス / CSV一括取込 / ファイル・書類保管
-- ===========================================================================
-- This migration provides three related foundations:
--   (1) self_service_change_requests — 自己情報の更新申請 (申請→承認→反映)
--   (2) csv_import_jobs / csv_import_rows — マスタCSV一括取込 (dry-run 検証 + 適用)
--   (3) documents / document_versions — 暗号化・版管理・保持期間つき書類ストア
--
-- LEGAL/COMPLIANCE NOTE:
--   保持年限 (retention) 等の法令値はこのスキーマにハードコードしない。
--   カテゴリ別の法定保存年限・論理失効方針 (NFR-011 / CMP-006 電子帳簿保存法・
--   e-文書法) は設定テーブル / カテゴリ定義として管理し、改正に追従させること。
--   本実装は技術的下地であり、法的充足や法的助言を保証するものではない。
--   最新の官公庁情報・社労士/弁護士確認を前提とする。

-- ---------------------------------------------------------------------------
-- self_service_change_requests (従業員セルフサービス更新申請)
-- ---------------------------------------------------------------------------
-- 一般従業員が自己の情報の変更を「更新申請」として提出するレコード。
-- 直接マスタ書込みは禁止し、承認エンジン (ST-FND-08, approval_requests) 経由で
-- 人事/承認者がレビュー → 承認時にのみ employees/関連マスタへ反映される。
--
-- changes_json:           機微でない差分 (前後値; 参照のみ)。JSONB。
-- changes_sensitive_enc:  機微項目 (口座/扶養等) の差分を含む場合の AES-256-GCM
--                         暗号文。平文は決して保存しない。bytea。
-- approval_request_id:    approval engine への連携キー (nullable; ルート未設定なら
--                         手動管理のため pending のまま残ることがある)。
CREATE TABLE self_service_change_requests (
    id                    uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id             uuid        NOT NULL REFERENCES tenants(id),
    employee_id           uuid        NOT NULL,
    -- requested_by_user_id: 申請者 (本人) のユーザID。
    -- NOTE: users テーブルには UNIQUE(id, tenant_id) が無いため複合FKは張れない。
    -- onboarding の assignee_user_id と同様、素の uuid 列 + service層での
    -- COUNT(1) WHERE id=? AND tenant_id=? によるテナント整合検証で担保する。
    requested_by_user_id  uuid        NOT NULL,
    -- target_type: 申請対象の種別。アクセス/反映ロジックの単位。
    target_type           text        NOT NULL,
    -- changes_json: 機微でない変更差分 (前後値の参照)。
    changes_json          jsonb       NOT NULL DEFAULT '{}',
    -- changes_sensitive_enc: 機微項目を含む差分の AES-256-GCM 暗号文。平文は保存しない。
    -- SECURITY: 機微平文を格納する text 列を (一時的にも) 追加しないこと。
    changes_sensitive_enc bytea,
    -- approval_request_id: approval engine の論理参照 (approval_requests.id)。
    -- 複合FK (approval_request_id, tenant_id) → approval_requests(id, tenant_id)。
    approval_request_id   uuid,
    -- status: draft|pending|approved|rejected|cancelled
    status                text        NOT NULL DEFAULT 'pending',
    -- reflected_at: 承認後にマスタへ反映された日時 (承認と単一txで設定)。
    reflected_at          timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_self_service_change_requests PRIMARY KEY (id),
    CONSTRAINT chk_sscr_status
        CHECK (status IN ('draft', 'pending', 'approved', 'rejected', 'cancelled')),
    CONSTRAINT chk_sscr_target_type
        CHECK (target_type IN ('employee_profile', 'emergency_contact', 'commute', 'bank_account', 'dependents')),
    -- [Security] 複合FK: (employee_id, tenant_id) が employees に存在することを強制。
    CONSTRAINT fk_sscr_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- [Security] 複合FK: (approval_request_id, tenant_id) が approval_requests に存在。
    CONSTRAINT fk_sscr_approval_request_tenant
        FOREIGN KEY (approval_request_id, tenant_id)
        REFERENCES approval_requests(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_sscr_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_sscr_lookup
    ON self_service_change_requests (tenant_id, employee_id, status);
CREATE INDEX idx_sscr_approval
    ON self_service_change_requests (tenant_id, approval_request_id);

-- ---------------------------------------------------------------------------
-- csv_import_jobs (CSV一括取込ジョブ)
-- ---------------------------------------------------------------------------
-- import_type: employees|departments  — 取込対象マスタ。
-- mode:        dry_run|apply           — 検証のみ / 確定適用。
-- apply_policy: all_or_nothing|skip_errors
--              — all_or_nothing: 全行成功時のみ原子適用 (部分適用なし)。
--              — skip_errors:    成功行のみ適用 + 失敗行スキップ。
-- encoding:    utf-8|shift_jis         — 文字コード (設定で扱う)。
-- status:      pending|validating|validated|applying|completed|failed
CREATE TABLE csv_import_jobs (
    id                   uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id            uuid        NOT NULL REFERENCES tenants(id),
    import_type          text        NOT NULL,
    mode                 text        NOT NULL,
    apply_policy         text        NOT NULL DEFAULT 'all_or_nothing',
    encoding             text        NOT NULL DEFAULT 'utf-8',
    status               text        NOT NULL DEFAULT 'pending',
    total_rows           integer     NOT NULL DEFAULT 0,
    success_rows         integer     NOT NULL DEFAULT 0,
    error_rows           integer     NOT NULL DEFAULT 0,
    -- uploaded_by_user_id: 取込実行ユーザ。users に UNIQUE(id, tenant_id) が無いため
    -- 複合FK は張らず、素の uuid 列 + service層 COUNT 検証で担保する。
    uploaded_by_user_id  uuid        NOT NULL,
    created_at           timestamptz NOT NULL DEFAULT now(),
    completed_at         timestamptz,
    CONSTRAINT pk_csv_import_jobs PRIMARY KEY (id),
    CONSTRAINT chk_csv_jobs_import_type
        CHECK (import_type IN ('employees', 'departments')),
    CONSTRAINT chk_csv_jobs_mode
        CHECK (mode IN ('dry_run', 'apply')),
    CONSTRAINT chk_csv_jobs_apply_policy
        CHECK (apply_policy IN ('all_or_nothing', 'skip_errors')),
    CONSTRAINT chk_csv_jobs_encoding
        CHECK (encoding IN ('utf-8', 'shift_jis')),
    CONSTRAINT chk_csv_jobs_status
        CHECK (status IN ('pending', 'validating', 'validated', 'applying', 'completed', 'failed')),
    CONSTRAINT uq_csv_import_jobs_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_csv_import_jobs_lookup
    ON csv_import_jobs (tenant_id, import_type, status);

-- ---------------------------------------------------------------------------
-- csv_import_rows (CSV取込・行ごとの検証結果)
-- ---------------------------------------------------------------------------
-- row_number:        1始まりのデータ行番号 (ヘッダを除く)。エラーレポート用。
-- raw_data_json:     非機微なパース後行データ (JSONB)。
-- raw_data_enc:      機微PII (扶養/口座等) を含み得る行データの AES-256-GCM 暗号文。
--                    平文は保存しない。bytea。
-- validation_status: valid|invalid
-- errors_json:       行番号付きエラー配列 (必須欠落/型不正/重複コード/越境整合違反等)。
-- applied:           この行が実マスタへ反映されたか。
CREATE TABLE csv_import_rows (
    id                   uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id            uuid        NOT NULL REFERENCES tenants(id),
    job_id               uuid        NOT NULL,
    row_number           integer     NOT NULL,
    raw_data_json        jsonb       NOT NULL DEFAULT '{}',
    -- raw_data_enc: 機微行データの暗号文。SECURITY: 機微平文の text 列を追加しないこと。
    raw_data_enc         bytea,
    validation_status    text        NOT NULL DEFAULT 'valid',
    errors_json          jsonb       NOT NULL DEFAULT '[]',
    applied              boolean     NOT NULL DEFAULT false,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_csv_import_rows PRIMARY KEY (id),
    CONSTRAINT chk_csv_rows_validation_status
        CHECK (validation_status IN ('valid', 'invalid')),
    -- [Security] 複合FK: (job_id, tenant_id) → csv_import_jobs (自パッケージ内表)。
    CONSTRAINT fk_csv_rows_job_tenant
        FOREIGN KEY (job_id, tenant_id)
        REFERENCES csv_import_jobs(id, tenant_id)
        MATCH SIMPLE,
    -- One row entry per (job, row_number).
    CONSTRAINT uq_csv_rows_job_rownum UNIQUE (job_id, row_number),
    CONSTRAINT uq_csv_import_rows_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_csv_import_rows_lookup
    ON csv_import_rows (tenant_id, job_id, validation_status);

-- ---------------------------------------------------------------------------
-- documents (論理書類 = 版の親)
-- ---------------------------------------------------------------------------
-- category:            contract|certificate|payslip|misc — アクセス制御の単位。
-- current_version_id:  最新版 (document_versions.id) の論理参照。
--                      document_versions が documents を参照するため循環FKを避け、
--                      素の uuid 列 + index のみ (FKは張らない)。値は service層で
--                      自テナント内の有効な版IDであることを保証する。
-- owner_employee_id:   nullable (NULLは全社書類)。複合FK (owner_employee_id, tenant_id)。
-- retention_label /    保持期間ポリシー。retention_expires_on 到来で logically_expired=true
-- retention_expires_on (物理削除はしない=論理失効)。法定保存対象は誤削除/早期失効を不可とする。
-- logically_expired:   論理失効フラグ (アクセス制限/マスク対象)。物理DELETEは行わない。
-- legal_hold:          法定保存対象 (true の間は失効・削除を禁止する不変条件用)。
CREATE TABLE documents (
    id                   uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id            uuid        NOT NULL REFERENCES tenants(id),
    owner_employee_id    uuid,
    category             text        NOT NULL,
    title                text        NOT NULL,
    -- current_version_id: 論理参照先 = document_versions.id (循環回避のためFK無し)。
    current_version_id   uuid,
    -- retention_label: 保持年限ラベル (法令値はハードコードせず設定/カテゴリ定義で管理)。
    retention_label      text        NOT NULL DEFAULT 'unspecified',
    retention_expires_on date,
    logically_expired    boolean     NOT NULL DEFAULT false,
    -- legal_hold: 法定保存対象 (誤削除/早期失効ガード)。
    legal_hold           boolean     NOT NULL DEFAULT false,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_documents PRIMARY KEY (id),
    CONSTRAINT chk_documents_category
        CHECK (category IN ('contract', 'certificate', 'payslip', 'misc')),
    -- [Security] 複合FK: (owner_employee_id, tenant_id) → employees。
    -- owner_employee_id が NULL の行は MATCH SIMPLE により制約をスキップ (全社書類)。
    CONSTRAINT fk_documents_owner_employee_tenant
        FOREIGN KEY (owner_employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_documents_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_documents_lookup
    ON documents (tenant_id, category, logically_expired);
CREATE INDEX idx_documents_owner
    ON documents (tenant_id, owner_employee_id);
-- 論理参照 current_version_id の検索用 index。
CREATE INDEX idx_documents_current_version
    ON documents (tenant_id, current_version_id);

-- ---------------------------------------------------------------------------
-- document_versions (書類の各版)
-- ---------------------------------------------------------------------------
-- 新版アップロードで行追加 → documents.current_version_id を更新。旧版は履歴として
-- 残存し物理削除しない。各版に改定者 (uploaded_by_user_id) と改定日時 (uploaded_at)。
--
-- storage_key:   オブジェクトストレージ参照 (実バイナリはDB外)。
-- content_hash:  真実性/改ざん検知 (CMP-006)。
-- enc_key_ref:   保存時暗号化の鍵参照 (本番KMS, 開発は FIELD_ENCRYPTION_KEY 相当)。
-- content_enc:   小サイズ書類を列保存する場合のみ使用する AES-256-GCM 暗号文 (bytea)。
CREATE TABLE document_versions (
    id                   uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id            uuid        NOT NULL REFERENCES tenants(id),
    document_id          uuid        NOT NULL,
    version_no           integer     NOT NULL,
    storage_key          text        NOT NULL DEFAULT '',
    content_hash         text        NOT NULL DEFAULT '',
    mime_type            text        NOT NULL DEFAULT '',
    size_bytes           bigint      NOT NULL DEFAULT 0,
    enc_key_ref          text        NOT NULL DEFAULT '',
    -- content_enc: 小サイズ書類の暗号文 (任意)。SECURITY: 平文 text 列を追加しないこと。
    content_enc          bytea,
    -- uploaded_by_user_id: 改定者。users に UNIQUE(id, tenant_id) が無いため複合FKは
    -- 張らず、素の uuid 列 + service層 COUNT 検証で担保する。
    uploaded_by_user_id  uuid        NOT NULL,
    uploaded_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_document_versions PRIMARY KEY (id),
    -- [Security] 複合FK: (document_id, tenant_id) → documents (自パッケージ内表)。
    CONSTRAINT fk_document_versions_document_tenant
        FOREIGN KEY (document_id, tenant_id)
        REFERENCES documents(id, tenant_id)
        MATCH SIMPLE,
    -- 同一書類内で版番号は一意。
    CONSTRAINT uq_document_versions_doc_versionno UNIQUE (document_id, version_no),
    CONSTRAINT uq_document_versions_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_document_versions_lookup
    ON document_versions (tenant_id, document_id, version_no);

-- ---------------------------------------------------------------------------
-- RLS — all new tables
-- ---------------------------------------------------------------------------
ALTER TABLE self_service_change_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE self_service_change_requests FORCE  ROW LEVEL SECURITY;
ALTER TABLE csv_import_jobs              ENABLE ROW LEVEL SECURITY;
ALTER TABLE csv_import_jobs              FORCE  ROW LEVEL SECURITY;
ALTER TABLE csv_import_rows              ENABLE ROW LEVEL SECURITY;
ALTER TABLE csv_import_rows              FORCE  ROW LEVEL SECURITY;
ALTER TABLE documents                    ENABLE ROW LEVEL SECURITY;
ALTER TABLE documents                    FORCE  ROW LEVEL SECURITY;
ALTER TABLE document_versions            ENABLE ROW LEVEL SECURITY;
ALTER TABLE document_versions            FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON self_service_change_requests
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON csv_import_jobs
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON csv_import_rows
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON documents
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON document_versions
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to hr_app
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE self_service_change_requests TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE csv_import_jobs              TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE csv_import_rows              TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE documents                    TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE document_versions            TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE document_versions            FROM hr_app;
REVOKE ALL ON TABLE documents                    FROM hr_app;
REVOKE ALL ON TABLE csv_import_rows              FROM hr_app;
REVOKE ALL ON TABLE csv_import_jobs              FROM hr_app;
REVOKE ALL ON TABLE self_service_change_requests FROM hr_app;

DROP TABLE IF EXISTS document_versions;
DROP TABLE IF EXISTS documents;
DROP TABLE IF EXISTS csv_import_rows;
DROP TABLE IF EXISTS csv_import_jobs;
DROP TABLE IF EXISTS self_service_change_requests;

-- +goose StatementEnd
