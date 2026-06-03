-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- ST-LM-11 就業規則・労使協定（36協定含む）の版管理・周知/同意・有効期限管理
--   - LM-055 就業規則の版管理・改定履歴・周知/同意取得
--   - LM-056 労使協定[36協定含む]の作成・電子届出・有効期限/更新アラート
--   - CORE-009 版管理・保持期間 / CMP-009 育児介護休業法の規程反映
--
-- 36協定の上限値そのものは既存 attendance.labor_agreements が保持する。
-- 本ストーリーは「協定文書・版・届出・有効期限/周知/同意のライフサイクル」を
-- 担い、上限値は重複保持せず linked_labor_agreement_id で参照連携する。
--
-- 法令注記: 有効期限の更新アラートのリードタイム、保持期間ポリシー、各種様式
-- 等の法令値は本マイグレーションにハードコードしない。テナント単位の設定表
-- (workrule_settings) から取得し、改正に追従する。法令値は最新の官公庁情報・
-- 社労士/弁護士確認が前提であり、本実装は法的助言ではない。
-- ===========================================================================

-- ---------------------------------------------------------------------------
-- workrule_settings (法令値・運用設定 ST-LM-11 legalConfigPoints)
-- ---------------------------------------------------------------------------
-- テナントごとに 1 行。労使協定の更新アラートのリードタイム(日数)、就業規則・
-- 労使協定の保持期間ポリシー(年数ラベル)を設定化する。ハードコード禁止
-- (legalConfigPoints)。様式テンプレート等は将来 templates_json に格納する。
CREATE TABLE workrule_settings (
    id                          uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id                   uuid        NOT NULL REFERENCES tenants(id),
    -- 労使協定の有効期限到来の何日前に更新アラートを生成するか(リードタイム)。
    -- 法令値ではなく運用設定だが、設定化して改正・運用変更に追従する。
    agreement_alert_lead_days   int         NOT NULL DEFAULT 60,
    -- 就業規則・労使協定の保持期間ポリシーラベル(CORE-009)。物理削除は行わず
    -- 論理的な保持/失効の基準としてのみ用いる。
    retention_policy            text        NOT NULL DEFAULT '5years',
    -- 様式テンプレート・電子届出様式の設定(様式改正追従)。
    templates_json              jsonb       NOT NULL DEFAULT '{}',
    created_at                  timestamptz NOT NULL DEFAULT now(),
    updated_at                  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_workrule_settings PRIMARY KEY (id),
    CONSTRAINT chk_workrule_settings_lead_days
        CHECK (agreement_alert_lead_days >= 0 AND agreement_alert_lead_days <= 3650),
    -- テナントごとに 1 行。
    CONSTRAINT uq_workrule_settings_tenant UNIQUE (tenant_id),
    -- UNIQUE(id, tenant_id) for downstream composite FK references
    CONSTRAINT uq_workrule_settings_id_tenant UNIQUE (id, tenant_id)
);

-- ---------------------------------------------------------------------------
-- work_rules (就業規則ドキュメント LM-055)
-- ---------------------------------------------------------------------------
-- 就業規則の論理単位(本則/各種規程)。版は work_rule_versions に分離する。
-- current_version_id は現行版への参照(素の uuid 列)。同パッケージ内の表だが、
-- work_rules と work_rule_versions は相互参照になるため current_version_id には
-- 複合FKを張らず、論理参照(work_rule_versions(id, tenant_id))とする。
-- 整合性はサービス層が同一トランザクション内で担保する。
CREATE TABLE work_rules (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    title               text        NOT NULL,
    -- category: "main"(本則) | "childcare_caregiving"(育児介護) | "wage" | "other" 等
    category            text        NOT NULL DEFAULT 'main',
    -- current_version_id: 現行版 work_rule_versions.id への論理参照(複合FKは
    -- 張らない: work_rules <-> work_rule_versions の相互参照を避けるため)。
    -- 論理参照先: work_rule_versions(id, tenant_id)
    current_version_id  uuid,
    -- retention_policy: 保持期間ポリシーラベル(CORE-009)。設定値で上書き可能。
    retention_policy    text        NOT NULL DEFAULT '5years',
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_work_rules PRIMARY KEY (id),
    CONSTRAINT chk_work_rules_category
        CHECK (category IN ('main', 'childcare_caregiving', 'wage', 'safety_health', 'other')),
    -- UNIQUE(id, tenant_id) for downstream composite FK references
    CONSTRAINT uq_work_rules_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_work_rules_lookup
    ON work_rules (tenant_id, category);

-- index for current_version_id logical reference lookups
CREATE INDEX idx_work_rules_current_version
    ON work_rules (tenant_id, current_version_id);

-- ---------------------------------------------------------------------------
-- work_rule_versions (就業規則の版・改定履歴 LM-055 / CORE-009 / CMP-009)
-- ---------------------------------------------------------------------------
-- 改定履歴の中核。version(連番) ごとに 1 行。
-- status: "draft" | "published" | "superseded"
--   draft     : 作成中(未周知)
--   published : 現行版として周知中(work_rules.current_version_id が指す)
--   superseded: 新版公開により旧版化
-- requires_expert_review: 改正反映(CMP-009 等)時の要専門家(社労士)確認フラグ。
-- document_ref: ファイル保管参照(ST-FND-10)。不透明な保管キー(PII を含めない)。
CREATE TABLE work_rule_versions (
    id                      uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id               uuid        NOT NULL REFERENCES tenants(id),
    work_rule_id            uuid        NOT NULL,
    -- version: 規則内で 1 から始まる連番
    version                 int         NOT NULL,
    revised_on              date,
    revision_reason         text        NOT NULL DEFAULT '',
    -- document_ref: ST-FND-10 ファイル保管の不透明参照キー(PII 不可)
    document_ref            text,
    -- status: draft / published / superseded
    status                  text        NOT NULL DEFAULT 'draft',
    published_at            timestamptz,
    -- requires_expert_review: CMP-009 等の改正反映で要社労士確認のフラグ
    requires_expert_review  boolean     NOT NULL DEFAULT false,
    created_by              uuid,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_work_rule_versions PRIMARY KEY (id),
    CONSTRAINT chk_work_rule_versions_status
        CHECK (status IN ('draft', 'published', 'superseded')),
    CONSTRAINT chk_work_rule_versions_version_positive
        CHECK (version >= 1),
    -- [Security] 自パッケージ内の複合FK: (work_rule_id, tenant_id) は work_rules に
    -- 存在しなければならない(クロステナント挿入を DB 層でも阻止)。
    CONSTRAINT fk_work_rule_versions_rule_tenant
        FOREIGN KEY (work_rule_id, tenant_id)
        REFERENCES work_rules (id, tenant_id)
        MATCH SIMPLE,
    -- 同一規則内で version は一意
    CONSTRAINT uq_work_rule_versions_rule_version UNIQUE (work_rule_id, version),
    -- UNIQUE(id, tenant_id) for downstream composite FK references
    CONSTRAINT uq_work_rule_versions_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_work_rule_versions_lookup
    ON work_rule_versions (tenant_id, work_rule_id, status);

-- ---------------------------------------------------------------------------
-- work_rule_acknowledgements (従業員ごとの周知既読・同意取得 LM-055)
-- ---------------------------------------------------------------------------
-- published 版を従業員へ周知し、従業員ごとの既読/同意を記録する。
-- consent: "read"(既読のみ) | "agreed"(同意)
-- 1 つの版につき 1 従業員 1 行(再周知は更新)。
CREATE TABLE work_rule_acknowledgements (
    id                      uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id               uuid        NOT NULL,
    work_rule_version_id    uuid        NOT NULL,
    employee_id             uuid        NOT NULL,
    -- consent: read(既読) / agreed(同意)
    consent                 text        NOT NULL DEFAULT 'read',
    acknowledged_at         timestamptz NOT NULL DEFAULT now(),
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_work_rule_acknowledgements PRIMARY KEY (id),
    CONSTRAINT chk_work_rule_acknowledgements_consent
        CHECK (consent IN ('read', 'agreed')),
    -- [Security] 自パッケージ内の複合FK: (work_rule_version_id, tenant_id) は
    -- work_rule_versions に存在しなければならない。
    CONSTRAINT fk_work_rule_acks_version_tenant
        FOREIGN KEY (work_rule_version_id, tenant_id)
        REFERENCES work_rule_versions (id, tenant_id)
        MATCH SIMPLE,
    -- [Security] 既存 employees への複合FK: (employee_id, tenant_id) を強制し
    -- クロステナント挿入を阻止する。
    CONSTRAINT fk_work_rule_acks_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees (id, tenant_id)
        MATCH SIMPLE,
    -- 版 × 従業員でユニーク(再周知は ON CONFLICT で更新)
    CONSTRAINT uq_work_rule_acks_version_employee UNIQUE (work_rule_version_id, employee_id),
    -- UNIQUE(id, tenant_id) for downstream composite FK references
    CONSTRAINT uq_work_rule_acks_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_work_rule_acks_lookup
    ON work_rule_acknowledgements (tenant_id, work_rule_version_id, consent);

CREATE INDEX idx_work_rule_acks_employee
    ON work_rule_acknowledgements (tenant_id, employee_id);

-- ---------------------------------------------------------------------------
-- labor_agreement_documents (労使協定の文書/版管理メタ LM-056)
-- ---------------------------------------------------------------------------
-- 労使協定(36協定/その他)の文書・版・電子届出ステータス・有効期間・更新アラート
-- を管理する。36協定の上限値そのものは既存 attendance.labor_agreements が source
-- of truth。本表では linked_labor_agreement_id でその上限値レコードへ参照連携し、
-- 上限値を重複保持しない。
--
-- linked_labor_agreement_id: 既存 attendance.labor_agreements(id) への論理参照。
--   クロスパッケージ(attendance)参照のため複合FKは張らず、素の uuid 列 + index
--   とする(規約: 既存 employees/departments 以外への参照は FK を張らない)。
--   論理参照先: labor_agreements(id, tenant_id) [package: attendance]
--   整合性(同一テナント所属・存在)はサービス層が RLS 下で検証する。
--
-- agreement_type: "article36"(36協定) | "other"(その他労使協定)
-- filing_status:  "draft" | "filed"(届出済) | "accepted"(受理)
-- renewal_alert_at: valid_to と設定リードタイムから算定した更新アラート発火日。
--   通知連携(ST-FND-09)は別パッケージが renewal_alert_at を読み取って配信する。
CREATE TABLE labor_agreement_documents (
    id                          uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id                   uuid        NOT NULL REFERENCES tenants(id),
    title                       text        NOT NULL,
    -- agreement_type: article36 / other
    agreement_type              text        NOT NULL DEFAULT 'other',
    -- version: 協定内で 1 から始まる連番
    version                     int         NOT NULL DEFAULT 1,
    valid_from                  date        NOT NULL,
    valid_to                    date        NOT NULL,
    -- filing_status: draft / filed / accepted
    filing_status               text        NOT NULL DEFAULT 'draft',
    filed_at                    timestamptz,
    accepted_at                 timestamptz,
    -- document_ref: ST-FND-10 ファイル保管の不透明参照キー(PII 不可)
    document_ref                text,
    -- linked_labor_agreement_id: attendance.labor_agreements(id) への論理参照
    -- (36協定上限値の源泉)。FK は張らない(クロスパッケージ参照)。
    linked_labor_agreement_id   uuid,
    -- renewal_alert_at: 更新アラート発火日(valid_to - 設定リードタイム)。
    renewal_alert_at            date,
    created_by                  uuid,
    created_at                  timestamptz NOT NULL DEFAULT now(),
    updated_at                  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_labor_agreement_documents PRIMARY KEY (id),
    CONSTRAINT chk_labor_agreement_documents_type
        CHECK (agreement_type IN ('article36', 'other')),
    CONSTRAINT chk_labor_agreement_documents_filing_status
        CHECK (filing_status IN ('draft', 'filed', 'accepted')),
    CONSTRAINT chk_labor_agreement_documents_version_positive
        CHECK (version >= 1),
    -- valid_to は valid_from 以降でなければならない
    CONSTRAINT chk_labor_agreement_documents_valid_range
        CHECK (valid_to >= valid_from),
    -- UNIQUE(id, tenant_id) for downstream composite FK references
    CONSTRAINT uq_labor_agreement_documents_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_labor_agreement_documents_lookup
    ON labor_agreement_documents (tenant_id, agreement_type, filing_status);

CREATE INDEX idx_labor_agreement_documents_validity
    ON labor_agreement_documents (tenant_id, valid_to, renewal_alert_at);

-- index for the cross-package logical reference (attendance.labor_agreements)
CREATE INDEX idx_labor_agreement_documents_linked
    ON labor_agreement_documents (tenant_id, linked_labor_agreement_id);

-- ---------------------------------------------------------------------------
-- RLS — all new tables
-- ---------------------------------------------------------------------------
ALTER TABLE workrule_settings           ENABLE ROW LEVEL SECURITY;
ALTER TABLE workrule_settings           FORCE  ROW LEVEL SECURITY;
ALTER TABLE work_rules                  ENABLE ROW LEVEL SECURITY;
ALTER TABLE work_rules                  FORCE  ROW LEVEL SECURITY;
ALTER TABLE work_rule_versions          ENABLE ROW LEVEL SECURITY;
ALTER TABLE work_rule_versions          FORCE  ROW LEVEL SECURITY;
ALTER TABLE work_rule_acknowledgements  ENABLE ROW LEVEL SECURITY;
ALTER TABLE work_rule_acknowledgements  FORCE  ROW LEVEL SECURITY;
ALTER TABLE labor_agreement_documents   ENABLE ROW LEVEL SECURITY;
ALTER TABLE labor_agreement_documents   FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON workrule_settings
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON work_rules
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON work_rule_versions
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON work_rule_acknowledgements
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON labor_agreement_documents
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to hr_app
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE workrule_settings          TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE work_rules                 TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE work_rule_versions         TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE work_rule_acknowledgements TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE labor_agreement_documents  TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE labor_agreement_documents  FROM hr_app;
REVOKE ALL ON TABLE work_rule_acknowledgements FROM hr_app;
REVOKE ALL ON TABLE work_rule_versions         FROM hr_app;
REVOKE ALL ON TABLE work_rules                 FROM hr_app;
REVOKE ALL ON TABLE workrule_settings          FROM hr_app;

DROP TABLE IF EXISTS labor_agreement_documents;
DROP TABLE IF EXISTS work_rule_acknowledgements;
DROP TABLE IF EXISTS work_rule_versions;
DROP TABLE IF EXISTS work_rules;
DROP TABLE IF EXISTS workrule_settings;

-- +goose StatementEnd
