-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- ST-LM-09  マイナンバー 収集・暗号化保管・厳格アクセス制御・利用提供ログ・廃棄
-- ===========================================================================
-- 個人番号 (マイナンバー) を業務テーブルから物理的に分離した専用ストアへ
-- AES-256-GCM 列暗号化 (bytea) で保管する。番号値・復号値はこのストアの
-- number_enc 列以外には一切持たない (平文列は禁止)。表示/復号は専用権限
-- (mynumber:reveal) + 利用目的の二重検証を通過した場合のみアプリ層で実施する。
--
-- マイナンバー法 (CMP-003) / 個人情報保護法 (CMP-004) の安全管理措置に対応:
--   - 技術的安全管理措置: 列暗号化 (NFR-004) + RLS テナント分離 + RBAC
--   - 利用提供ログ: 参照/復号/提供をハッシュチェーンで改ざん耐性化 (NFR-005)
--   - 保管期限管理・廃棄: 論理失効 (status=disposed) + 廃棄証跡記録
--
-- [法令値の注意]
-- 保管期限 (retention_until) の算定ルール・利用目的の限定列挙・廃棄事由/方式・
-- 暗号鍵ローテーション方針はいずれも法令値/運用ポリシーであり、最新の官公庁
-- 情報および社労士/弁護士の確認を前提に「設定」として外部化し改正に追従する
-- こと。本マイグレーションおよびアプリ実装は法的助言ではない。

-- ---------------------------------------------------------------------------
-- mynumber_records (マイナンバー専用分離ストア)
-- ---------------------------------------------------------------------------
-- subject_type: "self" (本人) | "dependent" (扶養家族)
--   扶養家族のマイナンバーは本人 (従業員) に関連付けつつ独立エンティティとして
--   保管・廃棄管理する。dependent_ref には扶養家族の識別 ID (自由構造ではなく
--   ID 参照) を入れる。
-- number_enc: AES-256-GCM 暗号文 (bytea)。個人番号の平文は DB/ログ/監査の
--   いずれにも一切格納しない。SECURITY: number を text 列として保持しては
--   ならない (一時的にも不可)。
-- status: "active" (有効) | "expired" (保管期限到来) | "disposed" (廃棄済み)
--   status=disposed の行は復号・表示・提供のいずれも拒否する。
-- retention_until: 保管期限。算定ルールは設定化 (法令値)。NULL は期限未設定。
CREATE TABLE mynumber_records (
    id            uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES tenants(id),
    employee_id   uuid        NOT NULL,
    -- subject_type: "self" | "dependent"
    subject_type  text        NOT NULL,
    -- dependent_ref: 扶養家族識別 (ID 参照)。self の場合は NULL。
    -- 論理参照先: onboarding (employee_intake_forms.dependents_json の扶養家族)。
    -- 他ストーリー由来の参照のため FK は張らず素の uuid 列に留める。
    dependent_ref uuid,
    -- number_enc: AES-256-GCM(bytea) で個人番号を暗号化保管。平文列は禁止。
    number_enc    bytea,
    -- status: "active" | "expired" | "disposed"
    status        text        NOT NULL DEFAULT 'active',
    collected_at  timestamptz NOT NULL DEFAULT now(),
    retention_until timestamptz,
    disposed_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_mynumber_records PRIMARY KEY (id),
    CONSTRAINT chk_mynumber_records_subject_type
        CHECK (subject_type IN ('self', 'dependent')),
    CONSTRAINT chk_mynumber_records_status
        CHECK (status IN ('active', 'expired', 'disposed')),
    -- [Security] Composite FK: (employee_id, tenant_id) が employees に存在する
    -- ことを強制 (クロステナント挿入を DB 層でも阻止)。
    CONSTRAINT fk_mynumber_records_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- 下流の複合 FK 参照用に必須。
    CONSTRAINT uq_mynumber_records_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_mynumber_records_lookup
    ON mynumber_records (tenant_id, employee_id, subject_type, status);

-- ---------------------------------------------------------------------------
-- mynumber_purposes (利用目的)
-- ---------------------------------------------------------------------------
-- 各マイナンバーの利用目的 (限定列挙: payroll/social_insurance/tax) を記録する。
-- 登録目的と利用時要求目的の突合で目的外利用を拒否する判定に使用する。
-- [法令値] 利用目的の限定列挙は設定で定義可能にし、目的追加に対応すること。
CREATE TABLE mynumber_purposes (
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES tenants(id),
    record_id   uuid        NOT NULL,
    -- purpose: "payroll" | "social_insurance" | "tax"
    purpose     text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_mynumber_purposes PRIMARY KEY (id),
    CONSTRAINT chk_mynumber_purposes_purpose
        CHECK (purpose IN ('payroll', 'social_insurance', 'tax')),
    -- [Security] Composite FK: (record_id, tenant_id) が mynumber_records に
    -- 存在することを強制。
    CONSTRAINT fk_mynumber_purposes_record_tenant
        FOREIGN KEY (record_id, tenant_id)
        REFERENCES mynumber_records(id, tenant_id)
        MATCH SIMPLE,
    -- 同一レコードに同じ目的を重複登録しない。
    CONSTRAINT uq_mynumber_purposes_record_purpose UNIQUE (record_id, purpose),
    CONSTRAINT uq_mynumber_purposes_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_mynumber_purposes_lookup
    ON mynumber_purposes (tenant_id, record_id, purpose);

-- ---------------------------------------------------------------------------
-- mynumber_access_logs (利用提供ログ — 参照/復号/提供)
-- ---------------------------------------------------------------------------
-- すべての参照・復号・第三者提供 (社保手続き等への引渡し) を記録する。
-- ログには who (actor_user_id) / when (occurred_at) / 目的 (purpose) / 対象本人
-- (target_record_id: opaque な UUID) / 提供先 (provided_to) を残し、番号値・
-- 復号値は一切残さない (CMP-003 提供 log)。
--
-- 改ざん耐性: 既存 platform/audit と整合したハッシュチェーン方式。
--   hash = SHA-256(prev_hash | id | action | purpose | target_record_id |
--                  provided_to | actor_user_id | occurred_at)
--   prev_hash は同一 tenant 内の直前行の hash。先頭行は ''。
--   seq は bigserial で DB が採番し、改ざん (並べ替え/末尾削除) を検知可能にする。
CREATE TABLE mynumber_access_logs (
    id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES tenants(id),
    -- target_record_id: 対象マイナンバーレコード (opaque な UUID 参照)。
    target_record_id uuid       NOT NULL,
    actor_user_id   uuid,
    -- action: "view" (参照) | "decrypt" (復号) | "provide" (第三者提供)
    action          text        NOT NULL,
    -- purpose: 利用目的 (payroll/social_insurance/tax)
    purpose         text        NOT NULL,
    -- provided_to: 提供先 (社保手続き等)。provide アクション以外は NULL。
    -- SECURITY: 個人を特定し得る氏名/番号は入れない。提供先システム/手続きの識別子のみ。
    provided_to     text,
    occurred_at     timestamptz NOT NULL DEFAULT now(),
    -- ハッシュチェーン (改ざん耐性)。
    prev_hash       text        NOT NULL DEFAULT '',
    hash            text        NOT NULL,
    seq             bigserial   NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_mynumber_access_logs PRIMARY KEY (id),
    CONSTRAINT chk_mynumber_access_logs_action
        CHECK (action IN ('view', 'decrypt', 'provide')),
    CONSTRAINT chk_mynumber_access_logs_purpose
        CHECK (purpose IN ('payroll', 'social_insurance', 'tax')),
    -- [Security] Composite FK: (target_record_id, tenant_id) が mynumber_records
    -- に存在することを強制。
    CONSTRAINT fk_mynumber_access_logs_record_tenant
        FOREIGN KEY (target_record_id, tenant_id)
        REFERENCES mynumber_records(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_mynumber_access_logs_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_mynumber_access_logs_lookup
    ON mynumber_access_logs (tenant_id, target_record_id, occurred_at);
-- チェーン検証 (seq 昇順) を効率化するインデックス。
CREATE INDEX idx_mynumber_access_logs_chain
    ON mynumber_access_logs (tenant_id, seq);

-- ---------------------------------------------------------------------------
-- mynumber_disposals (廃棄記録)
-- ---------------------------------------------------------------------------
-- 廃棄事実そのものの監査証跡。マイナンバーの廃棄は暗号文の物理削除または
-- キー破棄による復号不能化で行い、本テーブルに廃棄証跡を残す。
-- reason: "retention_expired" (保管期限到来) | "resignation" (退職) | "manual"
-- method: "ciphertext_deleted" (暗号文削除) | "key_destroyed" (キー破棄)
-- [法令値] 廃棄事由・廃棄方式のポリシーは設定化すること。
CREATE TABLE mynumber_disposals (
    id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES tenants(id),
    record_id       uuid        NOT NULL,
    -- reason: "retention_expired" | "resignation" | "manual"
    reason          text        NOT NULL,
    -- method: "ciphertext_deleted" | "key_destroyed"
    method          text        NOT NULL,
    disposed_by     uuid,
    disposed_at     timestamptz NOT NULL DEFAULT now(),
    -- certificate_ref: 廃棄証跡の参照 (証明書/台帳の opaque な識別子)。
    certificate_ref text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_mynumber_disposals PRIMARY KEY (id),
    CONSTRAINT chk_mynumber_disposals_reason
        CHECK (reason IN ('retention_expired', 'resignation', 'manual')),
    CONSTRAINT chk_mynumber_disposals_method
        CHECK (method IN ('ciphertext_deleted', 'key_destroyed')),
    -- [Security] Composite FK: (record_id, tenant_id) が mynumber_records に
    -- 存在することを強制。
    CONSTRAINT fk_mynumber_disposals_record_tenant
        FOREIGN KEY (record_id, tenant_id)
        REFERENCES mynumber_records(id, tenant_id)
        MATCH SIMPLE,
    -- 1 レコードにつき廃棄記録は 1 件 (最初の廃棄が確定)。
    CONSTRAINT uq_mynumber_disposals_record_tenant UNIQUE (record_id, tenant_id),
    CONSTRAINT uq_mynumber_disposals_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_mynumber_disposals_lookup
    ON mynumber_disposals (tenant_id, record_id);

-- ---------------------------------------------------------------------------
-- RLS — all new tables
-- ---------------------------------------------------------------------------
ALTER TABLE mynumber_records     ENABLE ROW LEVEL SECURITY;
ALTER TABLE mynumber_records     FORCE  ROW LEVEL SECURITY;
ALTER TABLE mynumber_purposes    ENABLE ROW LEVEL SECURITY;
ALTER TABLE mynumber_purposes    FORCE  ROW LEVEL SECURITY;
ALTER TABLE mynumber_access_logs ENABLE ROW LEVEL SECURITY;
ALTER TABLE mynumber_access_logs FORCE  ROW LEVEL SECURITY;
ALTER TABLE mynumber_disposals   ENABLE ROW LEVEL SECURITY;
ALTER TABLE mynumber_disposals   FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON mynumber_records
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON mynumber_purposes
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON mynumber_access_logs
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON mynumber_disposals
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to hr_app
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE mynumber_records     TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE mynumber_purposes    TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE mynumber_access_logs TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE mynumber_disposals   TO hr_app;
-- bigserial の seq 採番に必要な sequence の権限を hr_app に付与する。
GRANT USAGE, SELECT ON SEQUENCE mynumber_access_logs_seq_seq TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON SEQUENCE mynumber_access_logs_seq_seq FROM hr_app;
REVOKE ALL ON TABLE mynumber_disposals   FROM hr_app;
REVOKE ALL ON TABLE mynumber_access_logs FROM hr_app;
REVOKE ALL ON TABLE mynumber_purposes    FROM hr_app;
REVOKE ALL ON TABLE mynumber_records     FROM hr_app;

DROP TABLE IF EXISTS mynumber_disposals;
DROP TABLE IF EXISTS mynumber_access_logs;
DROP TABLE IF EXISTS mynumber_purposes;
DROP TABLE IF EXISTS mynumber_records;

-- +goose StatementEnd
