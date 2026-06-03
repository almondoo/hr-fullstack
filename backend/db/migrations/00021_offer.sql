-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- ST-ATS-05  内定/オファー管理 (オファーレター発行・電子署名・受諾管理)
-- ===========================================================================
-- EP-ATS 採用管理 / docs 01 §C-2 ATS-015, CMP-006(電子契約・電子保存=真実性/可視性),
-- INT-009(電子契約サービス連携).
--
-- 法令・コンプライアンス注記:
--   - 電子契約・電子保存の要件(電帳法/e-文書法=真実性・可視性、保存年限)を満たす
--     保存方式・保存年限、およびオファー(労働条件提示)の必須記載事項(労働条件通知に
--     準ずる項目, 労基法依存)は法令値である。本マイグレーションではハードコードせず、
--     offer_settings テーブルに設定化して改正追従する(CMP-001 要専門家確認)。
--   - 法令値は最新の官公庁情報・社労士/弁護士確認が前提。設定化して改正追従。
--     本実装は法的助言ではない。
-- ===========================================================================

-- ---------------------------------------------------------------------------
-- offer_settings (テナント別オファー法令設定 — CMP-006 / CMP-001)
-- ---------------------------------------------------------------------------
-- 法令値(電子保存方式・保存年限・必須記載項目セット・有効期限リードタイム日数)を
-- テナント別に設定化する。コードにハードコードしないための設定テーブル。
-- required_fields_json: オファー必須記載事項セット (労働条件通知に準ずる項目)。
--   Format: {"fields":["position","annual_salary","start_date","employment_type"]}
-- retention_years: 締結文書・オファー条件の保持年限(法定文書保存方針 LM-054 と整合)。
-- esign_storage_mode: 電子保存方式ラベル(真実性・可視性を満たす保存方式)。
-- default_expiry_lead_days: オファー有効期限のデフォルトリードタイム日数。
CREATE TABLE offer_settings (
    id                       uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id                uuid        NOT NULL REFERENCES tenants(id),
    -- required_fields_json: 必須記載事項セット(労基法依存・要専門家確認 CMP-001)
    required_fields_json     jsonb       NOT NULL DEFAULT '{"fields":[]}',
    -- retention_years: 法定保存年限(法令値・改正追従のため設定化)
    retention_years          integer     NOT NULL DEFAULT 7,
    -- esign_storage_mode: 電子保存方式ラベル(電帳法/e-文書法 真実性・可視性)
    esign_storage_mode       text        NOT NULL DEFAULT 'evidence_hash',
    -- default_expiry_lead_days: 有効期限リードタイム既定値(法令値・設定化)
    default_expiry_lead_days integer     NOT NULL DEFAULT 14,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_offer_settings PRIMARY KEY (id),
    CONSTRAINT chk_offer_settings_retention_years
        CHECK (retention_years > 0),
    CONSTRAINT chk_offer_settings_expiry_lead_days
        CHECK (default_expiry_lead_days >= 0),
    -- One settings row per tenant.
    CONSTRAINT uq_offer_settings_tenant UNIQUE (tenant_id),
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_offer_settings_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_offer_settings_lookup ON offer_settings (tenant_id);

-- ---------------------------------------------------------------------------
-- offers (オファー本体)
-- ---------------------------------------------------------------------------
-- application(最終ステージ通過)に対するオファー。役職/年収/入社予定日/雇用区分等の
-- 条件を確定する。状態機械: draft→sent→accepted/declined/expired/rescinded。
--
-- application_id:
--   ST-ATS-03 applications への論理参照。applications は別の新規ストーリー(ST-ATS-03)
--   の表であり、並列ビルド隔離方針により素の uuid 列 + index とし FK は張らない
--   (論理参照先: applications(id, tenant_id))。アプリ層で (application_id, tenant_id)
--   の整合を担保する。
--
-- approval_request_id:
--   ST-FND-08 approval_requests (00006, 安定既存表) への複合 FK。発行承認連携。
--   承認確定後のみ sent 可能(アプリ層で制御)。null は承認不要 or 未提出。
--
-- 機微列(AES-256-GCM, bytea):
--   - annual_salary_enc:        想定年収(機微・要配慮相当, employment に準じる機微度)
--   - compensation_detail_enc:  報酬詳細(賞与・手当等の機微情報)
--   平文は永続化しない。復号は offer:read_sensitive 権限でアプリ層再検証時のみ。
CREATE TABLE offers (
    id                      uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id               uuid        NOT NULL REFERENCES tenants(id),
    -- application_id: ST-ATS-03 applications への論理参照(素の uuid・FK なし)。
    -- 論理参照先: applications(id, tenant_id)。
    application_id          uuid        NOT NULL,
    -- status: draft | sent | accepted | declined | expired | rescinded
    status                  text        NOT NULL DEFAULT 'draft',
    position                text        NOT NULL DEFAULT '',
    -- employment_type: 雇用区分(full_time | part_time | contract | dispatch 等)
    employment_type         text        NOT NULL DEFAULT '',
    start_date              date,
    -- expiry_date: 有効期限。経過で expired 扱い(参照時判定・物理削除しない)。
    expiry_date             date,
    -- annual_salary_enc: 想定年収の AES-256-GCM 暗号文。平文は永続化しない。
    -- SECURITY: 年収平文の text 列を一時的にも追加しないこと。
    annual_salary_enc       bytea,
    -- compensation_detail_enc: 報酬詳細の AES-256-GCM 暗号文。平文は永続化しない。
    compensation_detail_enc bytea,
    -- approval_request_id: ST-FND-08 approval_requests への複合 FK(発行承認連携)。
    approval_request_id     uuid,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_offers PRIMARY KEY (id),
    CONSTRAINT chk_offers_status
        CHECK (status IN ('draft', 'sent', 'accepted', 'declined', 'expired', 'rescinded')),
    -- [Security] Composite FK: (approval_request_id, tenant_id) when set must
    -- exist in approval_requests. Prevents cross-tenant approval linkage.
    CONSTRAINT fk_offers_approval_tenant
        FOREIGN KEY (approval_request_id, tenant_id)
        REFERENCES approval_requests(id, tenant_id)
        MATCH SIMPLE,
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_offers_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_offers_lookup
    ON offers (tenant_id, application_id, status);

CREATE INDEX idx_offers_expiry
    ON offers (tenant_id, status, expiry_date)
    WHERE expiry_date IS NOT NULL;

-- ---------------------------------------------------------------------------
-- offer_letters (オファーレター文書・締結証跡 — CMP-006 真実性/可視性)
-- ---------------------------------------------------------------------------
-- file_ref:          ファイル保管基盤の opaque 参照(ST-ATS-05 では論理参照のみ)。
-- version:           版管理。
-- esign_provider:    電子契約サービス(INT-009)の provider ラベル。
-- esign_envelope_id: 電子契約サービスの opaque 外部 ID(provider_ref)。生トークン不保存。
-- content_hash:      文書の SHA-256 等ハッシュ。改ざん検知(真実性確保)。
-- signed_at:         締結証跡(締結日時)。
-- signer_ref:        署名者の opaque 参照(氏名等 PII は入れない)。
CREATE TABLE offer_letters (
    id                uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id         uuid        NOT NULL REFERENCES tenants(id),
    offer_id          uuid        NOT NULL,
    -- file_ref: ファイル保管基盤の opaque 参照。
    file_ref          text        NOT NULL DEFAULT '',
    version           integer     NOT NULL DEFAULT 1,
    -- esign_provider: INT-009 電子契約サービス provider ラベル。
    esign_provider    text        NOT NULL DEFAULT '',
    -- esign_envelope_id: 電子契約サービスの opaque 外部 ID(provider_ref)。
    esign_envelope_id text        NOT NULL DEFAULT '',
    -- content_hash: 改ざん検知用ハッシュ(真実性・CMP-006)。
    content_hash      text        NOT NULL DEFAULT '',
    -- signer_ref: 署名者 opaque 参照(PII 禁止)。
    signer_ref        text,
    signed_at         timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_offer_letters PRIMARY KEY (id),
    CONSTRAINT chk_offer_letters_version CHECK (version >= 1),
    -- [Security] Composite FK: (offer_id, tenant_id) must exist in offers.
    -- Same-package composite FK prevents cross-tenant letter creation.
    CONSTRAINT fk_offer_letters_offer_tenant
        FOREIGN KEY (offer_id, tenant_id)
        REFERENCES offers(id, tenant_id)
        MATCH SIMPLE,
    -- One letter row per offer per version.
    CONSTRAINT uq_offer_letters_offer_version UNIQUE (offer_id, tenant_id, version),
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_offer_letters_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_offer_letters_lookup
    ON offer_letters (tenant_id, offer_id, version);

-- ---------------------------------------------------------------------------
-- offer_responses (候補者の受諾/辞退応答履歴)
-- ---------------------------------------------------------------------------
-- response('accepted'/'declined')。responded_via('portal'/'esign'/'manual')。
-- 受諾(accepted)応答が ST-ATS-06(候補者→従業員マスタ生成)連携トリガ。
-- 応答は履歴として保持する(複数行可・物理削除しない)。
CREATE TABLE offer_responses (
    id           uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id    uuid        NOT NULL REFERENCES tenants(id),
    offer_id     uuid        NOT NULL,
    -- response: accepted | declined
    response     text        NOT NULL,
    -- responded_via: portal | esign | manual
    responded_via text       NOT NULL DEFAULT 'manual',
    responded_at timestamptz NOT NULL DEFAULT now(),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_offer_responses PRIMARY KEY (id),
    CONSTRAINT chk_offer_responses_response
        CHECK (response IN ('accepted', 'declined')),
    CONSTRAINT chk_offer_responses_via
        CHECK (responded_via IN ('portal', 'esign', 'manual')),
    -- [Security] Composite FK: (offer_id, tenant_id) must exist in offers.
    CONSTRAINT fk_offer_responses_offer_tenant
        FOREIGN KEY (offer_id, tenant_id)
        REFERENCES offers(id, tenant_id)
        MATCH SIMPLE,
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_offer_responses_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_offer_responses_lookup
    ON offer_responses (tenant_id, offer_id, responded_at);

-- ---------------------------------------------------------------------------
-- RLS — all new tenant-scoped tables
-- ---------------------------------------------------------------------------
ALTER TABLE offer_settings  ENABLE ROW LEVEL SECURITY;
ALTER TABLE offer_settings  FORCE  ROW LEVEL SECURITY;
ALTER TABLE offers          ENABLE ROW LEVEL SECURITY;
ALTER TABLE offers          FORCE  ROW LEVEL SECURITY;
ALTER TABLE offer_letters   ENABLE ROW LEVEL SECURITY;
ALTER TABLE offer_letters   FORCE  ROW LEVEL SECURITY;
ALTER TABLE offer_responses ENABLE ROW LEVEL SECURITY;
ALTER TABLE offer_responses FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON offer_settings
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON offers
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON offer_letters
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON offer_responses
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to hr_app
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE offer_settings  TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE offers          TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE offer_letters   TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE offer_responses TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE offer_responses FROM hr_app;
REVOKE ALL ON TABLE offer_letters   FROM hr_app;
REVOKE ALL ON TABLE offers          FROM hr_app;
REVOKE ALL ON TABLE offer_settings  FROM hr_app;

DROP TABLE IF EXISTS offer_responses;
DROP TABLE IF EXISTS offer_letters;
DROP TABLE IF EXISTS offers;
DROP TABLE IF EXISTS offer_settings;

-- +goose StatementEnd
