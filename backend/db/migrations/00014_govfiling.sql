-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- ST-LM-08 社会保険・労働保険 帳票生成と電子申請 (e-Gov / マイナポータルAPI) 連携
-- ===========================================================================
-- LEGAL DISCLAIMER (法令値の取り扱い):
--   健康保険料率・厚生年金保険料率・標準報酬月額等級表・雇用保険料率・労災保険率・
--   算定/月変の等級判定閾値・各電子申請様式バージョン等の「法令値」は、本マイグレーション
--   およびアプリケーションコードに一切ハードコードしない。これらは insurance_settings の
--   JSONB 列にテナント別に設定し、改正時は設定更新のみで追従する設計とする。
--   設定する具体値は最新の官公庁情報・社労士/弁護士の確認が前提であり、本実装は法的助言で
--   はない。様式改正・料率改定に追従できる構造のみを提供する。

-- ---------------------------------------------------------------------------
-- insurance_settings (社保/労保 テナント別設定 LM-010/011/014)
-- ---------------------------------------------------------------------------
-- テナント別の社会保険・労働保険の設定を JSONB で保持する。
-- 1テナント1行 (uq_insurance_settings_tenant)。
--
-- 法令値はすべてここに設定化する (ハードコード禁止・改正追従):
--   - rate_table_json: 健康保険料率/厚生年金保険料率/雇用保険料率/労災保険率(業種別)等。
--       Format(例): {"health_insurance_rate":"0.0998","pension_rate":"0.183",
--                    "employment_insurance_rate":"0.006","workers_comp_rate_by_industry":{...}}
--   - grade_table_json: 標準報酬月額等級表 (協会けんぽ/健保組合別)。
--       Format(例): {"grades":[{"grade":1,"lower":0,"upper":63000,"monthly":58000}, ...]}
--   - judgement_threshold_json: 算定基礎届/月額変更届の判定閾値 (2等級以上変動等。要社労士確認)。
--       Format(例): {"monthly_change_grade_diff":2, "fixed_wage_change_required":true}
--   - form_version_json: 各電子申請様式 (e-Gov/マイナポータル) のバージョン・項目マッピング。
--       Format(例): {"egov":{"health_insurance_acquire":"v2024.1"}, "myna":{...}}
-- insurer_kind: 提出先区分 (協会けんぽ kyokai / 健保組合 kumiai)。書式差異の切替に使用。
CREATE TABLE insurance_settings (
    id                          uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id                   uuid        NOT NULL REFERENCES tenants(id),
    -- insurer_kind: 'kyokai' (協会けんぽ) | 'kumiai' (健保組合)
    insurer_kind                text        NOT NULL DEFAULT 'kyokai',
    -- 法令値の設定化 (改正追従)。すべて JSONB。具体値は社労士/弁護士確認前提。
    rate_table_json             jsonb       NOT NULL DEFAULT '{}',
    grade_table_json            jsonb       NOT NULL DEFAULT '{}',
    judgement_threshold_json    jsonb       NOT NULL DEFAULT '{}',
    form_version_json           jsonb       NOT NULL DEFAULT '{}',
    created_at                  timestamptz NOT NULL DEFAULT now(),
    updated_at                  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_insurance_settings PRIMARY KEY (id),
    CONSTRAINT chk_insurance_settings_insurer_kind
        CHECK (insurer_kind IN ('kyokai', 'kumiai')),
    -- 1テナント1行。
    CONSTRAINT uq_insurance_settings_tenant UNIQUE (tenant_id),
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_insurance_settings_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_insurance_settings_lookup
    ON insurance_settings (tenant_id);

-- ---------------------------------------------------------------------------
-- gov_filings (電子申請ジョブ本体 LM-010/011/012/013)
-- ---------------------------------------------------------------------------
-- 各行政手続きの電子申請ジョブ。従業員・雇用契約・報酬月額から生成した届出帳票の送信単位。
--
-- filing_type (届出種別):
--   health_insurance_acquire / health_insurance_lose  (健保 資格取得/喪失届)
--   pension_calc / pension_change                      (厚年 算定基礎届/月額変更届)
--   employment_insurance_acquire / employment_insurance_lose / employment_insurance_separation
--                                                      (雇用保険 取得/喪失/離職票)
--   workers_comp_report                                (労災(労働保険) 届出)
-- channel: 送信先チャネル 'egov' (e-Gov) | 'myna' (マイナポータル)。
-- status:  draft → submitted → accepted/returned → completed/error の機械。
-- external_ref: 外部受付番号 (不透明ID。受信後に設定)。
-- idempotency_key: 外部API二重送信防止の冪等キー。テナント内一意。
-- payload_json: 届出帳票データ (参照IDのみ・機微復号値は格納しない)。
--   マイナンバー等は本文に直接保持せず ST-LM-09 マイナンバーストアをトークン/別経路で参照する。
--
-- 機微情報の非格納:
--   マイナンバー等の復号値は payload_json/external_ref/その他いずれの列にも格納しない。
--   公文書本体 (機微を含みうる) は gov_filing_documents.content_enc に暗号化して保持する。
CREATE TABLE gov_filings (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    employee_id         uuid        NOT NULL,
    filing_type         text        NOT NULL,
    -- channel: 'egov' | 'myna'
    channel             text        NOT NULL,
    -- status: 'draft' | 'submitted' | 'accepted' | 'returned' | 'completed' | 'error'
    status              text        NOT NULL DEFAULT 'draft',
    -- payload_json: 届出帳票データ (参照IDのみ・PII/復号値非格納)
    payload_json        jsonb       NOT NULL DEFAULT '{}',
    -- external_ref: 外部受付番号 (不透明ID)
    external_ref        text,
    -- idempotency_key: 冪等キー (外部API二重送信防止)
    idempotency_key     text        NOT NULL,
    submitted_at        timestamptz,
    last_error          text,
    created_by          uuid,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_gov_filings PRIMARY KEY (id),
    CONSTRAINT chk_gov_filings_filing_type
        CHECK (filing_type IN (
            'health_insurance_acquire', 'health_insurance_lose',
            'pension_calc', 'pension_change',
            'employment_insurance_acquire', 'employment_insurance_lose',
            'employment_insurance_separation',
            'workers_comp_report'
        )),
    CONSTRAINT chk_gov_filings_channel
        CHECK (channel IN ('egov', 'myna')),
    CONSTRAINT chk_gov_filings_status
        CHECK (status IN ('draft', 'submitted', 'accepted', 'returned', 'completed', 'error')),
    -- [Security] Composite FK: (employee_id, tenant_id) must exist in employees.
    -- Prevents cross-tenant filing creation.
    CONSTRAINT fk_gov_filings_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- 冪等キーはテナント内一意 (二重送信防止)。
    CONSTRAINT uq_gov_filings_idempotency UNIQUE (tenant_id, idempotency_key),
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_gov_filings_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_gov_filings_lookup
    ON gov_filings (tenant_id, employee_id, filing_type, status);

-- ---------------------------------------------------------------------------
-- gov_filing_documents (申請に紐づく公文書/帳票 LM-013, CMP-006)
-- ---------------------------------------------------------------------------
-- 受付控・決定通知・返戻理由・生成帳票等の公文書本体を申請に紐付けて保存する。
-- 電子帳簿保存法 (真実性・可視性) 配慮: 本体は暗号化して bytea に保持し、論理参照のみで参照。
--
-- content_enc: AES-256-GCM ciphertext (bytea)。公文書本体 (機微を含みうる) の暗号文。
--   平文は DB・ログ・監査 resource_id のいずれにも格納しない。復号は service 層で RBAC 再検証
--   (filing:read_sensitive) を通過した場合のみ・別戻り値で返す。
--   SECURITY: 平文用の text 列を一時的にも追加しないこと。
-- doc_kind: 'receipt' (受付控) | 'decision' (決定通知) | 'return_reason' (返戻理由) |
--           'generated_form' (生成帳票)。
-- retention_label: 保存年限ラベル (法令値。設定化前提。例: '2years'/'4years'/'7years')。
--   具体年限は社労士/弁護士確認前提でありハードコードの法的根拠ではない。
CREATE TABLE gov_filing_documents (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    filing_id           uuid        NOT NULL,
    -- doc_kind: 'receipt' | 'decision' | 'return_reason' | 'generated_form'
    doc_kind            text        NOT NULL,
    -- content_enc: AES-256-GCM ciphertext of the document body. Plaintext is
    -- NEVER stored. Decryption requires filing:read_sensitive (service layer).
    content_enc         bytea,
    -- retention_label: 保存年限ラベル (法令値・設定化前提)
    retention_label     text        NOT NULL DEFAULT '4years',
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_gov_filing_documents PRIMARY KEY (id),
    CONSTRAINT chk_gov_filing_documents_doc_kind
        CHECK (doc_kind IN ('receipt', 'decision', 'return_reason', 'generated_form')),
    -- [Security] Composite FK (own package): (filing_id, tenant_id) must exist
    -- in gov_filings. Cascade nothing; documents are retained.
    CONSTRAINT fk_gov_filing_documents_filing_tenant
        FOREIGN KEY (filing_id, tenant_id)
        REFERENCES gov_filings(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_gov_filing_documents_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_gov_filing_documents_lookup
    ON gov_filing_documents (tenant_id, filing_id, doc_kind);

-- ---------------------------------------------------------------------------
-- gov_filing_status_history (申請ステータス遷移履歴 LM-013)
-- ---------------------------------------------------------------------------
-- 申請のステータス遷移を記録する。返戻理由等の外部メッセージも保持する。
-- from_status/to_status: 遷移前後のステータス。
-- external_message: e-Gov/マイナポータルからの外部メッセージ (返戻理由等)。
CREATE TABLE gov_filing_status_history (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    filing_id           uuid        NOT NULL,
    from_status         text        NOT NULL,
    to_status           text        NOT NULL,
    note                text,
    external_message    text,
    changed_by          uuid,
    changed_at          timestamptz NOT NULL DEFAULT now(),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_gov_filing_status_history PRIMARY KEY (id),
    CONSTRAINT chk_gov_filing_status_history_from
        CHECK (from_status IN ('draft', 'submitted', 'accepted', 'returned', 'completed', 'error')),
    CONSTRAINT chk_gov_filing_status_history_to
        CHECK (to_status IN ('draft', 'submitted', 'accepted', 'returned', 'completed', 'error')),
    -- [Security] Composite FK (own package): (filing_id, tenant_id) in gov_filings.
    CONSTRAINT fk_gov_filing_status_history_filing_tenant
        FOREIGN KEY (filing_id, tenant_id)
        REFERENCES gov_filings(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_gov_filing_status_history_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_gov_filing_status_history_lookup
    ON gov_filing_status_history (tenant_id, filing_id, changed_at);

-- ---------------------------------------------------------------------------
-- RLS — all tables
-- ---------------------------------------------------------------------------
ALTER TABLE insurance_settings          ENABLE ROW LEVEL SECURITY;
ALTER TABLE insurance_settings          FORCE  ROW LEVEL SECURITY;
ALTER TABLE gov_filings                 ENABLE ROW LEVEL SECURITY;
ALTER TABLE gov_filings                 FORCE  ROW LEVEL SECURITY;
ALTER TABLE gov_filing_documents        ENABLE ROW LEVEL SECURITY;
ALTER TABLE gov_filing_documents        FORCE  ROW LEVEL SECURITY;
ALTER TABLE gov_filing_status_history   ENABLE ROW LEVEL SECURITY;
ALTER TABLE gov_filing_status_history   FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON insurance_settings
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON gov_filings
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON gov_filing_documents
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON gov_filing_status_history
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to hr_app
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE insurance_settings        TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE gov_filings               TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE gov_filing_documents      TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE gov_filing_status_history TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE gov_filing_status_history FROM hr_app;
REVOKE ALL ON TABLE gov_filing_documents      FROM hr_app;
REVOKE ALL ON TABLE gov_filings               FROM hr_app;
REVOKE ALL ON TABLE insurance_settings        FROM hr_app;

DROP TABLE IF EXISTS gov_filing_status_history;
DROP TABLE IF EXISTS gov_filing_documents;
DROP TABLE IF EXISTS gov_filings;
DROP TABLE IF EXISTS insurance_settings;

-- +goose StatementEnd
