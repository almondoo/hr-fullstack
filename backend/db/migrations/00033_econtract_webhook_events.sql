-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- 00033: econtract_webhook_events — 電子契約 Webhook 冪等テーブル
--
-- 目的:
--   電子契約サービスからの Webhook を受信した際に、重複配信(リプレイ攻撃含む)
--   を防ぐための冪等チェックテーブル。
--
-- 設計方針:
--   - (tenant_id, provider, envelope_id, status) の組み合わせが一意。
--     同一イベントの二重処理を UNIQUE 制約 + ON CONFLICT DO NOTHING で防ぐ。
--   - append-only: INSERT / SELECT のみを hr_app に許可。UPDATE / DELETE は付与しない。
--     改ざん・削除によるリプレイ保護の無効化を防ぐ。
--   - PII 列なし: envelope_id は外部サービスの opaque ID のみ。
--     マイナンバー / 氏名 / メールアドレス等を格納しない。
--   - RLS ENABLE + FORCE + tenant_isolation ポリシー: テナント境界を保証する。
--
-- セキュリティ注記:
--   - RLS によりテナント境界が強制される。hr_app は自テナントの行のみ操作可能。
--   - DELETE / UPDATE を付与しないことで append-only を強制する。
--     イベント行を削除することでリプレイ保護が無効化されるリスクを排除する。
--   - resource_id / envelope_id には PII を格納してはならない (opaque ID のみ)。
--
-- ライフサイクル注記:
--   - 受信済みイベントの保存期間は offer_settings.retention_years に準じる。
--     本テーブルはスキャフォールドであり、保存期間管理ジョブは別途実装する。
-- ===========================================================================

CREATE TABLE econtract_webhook_events (
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES tenants(id),
    -- provider: 電子契約サービス識別子 ("cloudsign" / "docusign" / "adobesign" / "stub")
    provider    text        NOT NULL,
    -- envelope_id: 外部サービスの opaque 書類ID / envelopeId。PII 禁止。
    envelope_id text        NOT NULL,
    -- status: provider-normalised ステータス ("pending" / "completed" / "declined" / "expired" / "voided")
    status      text        NOT NULL,
    -- received_at: Webhook を受信した日時 (server time)。
    received_at timestamptz NOT NULL DEFAULT now(),
    created_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT pk_econtract_webhook_events PRIMARY KEY (id),
    -- 冪等キー: 同テナント・同プロバイダ・同エンベロープ・同ステータスは一意。
    CONSTRAINT uq_econtract_webhook_events_idem
        UNIQUE (tenant_id, provider, envelope_id, status),
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_econtract_webhook_events_id_tenant UNIQUE (id, tenant_id),
    CONSTRAINT chk_econtract_webhook_events_provider
        CHECK (provider IN ('cloudsign', 'docusign', 'adobesign', 'stub')),
    CONSTRAINT chk_econtract_webhook_events_status
        CHECK (status IN ('pending', 'completed', 'declined', 'expired', 'voided'))
);

-- Lookup index: テナント・プロバイダ・エンベロープ別のイベント検索。
CREATE INDEX idx_econtract_webhook_events_lookup
    ON econtract_webhook_events (tenant_id, provider, envelope_id);

-- RLS: テナント境界の強制
ALTER TABLE econtract_webhook_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE econtract_webhook_events FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON econtract_webhook_events
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Grants: append-only (SELECT + INSERT のみ。UPDATE / DELETE は意図的に付与しない)
GRANT SELECT, INSERT ON TABLE econtract_webhook_events TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE econtract_webhook_events FROM hr_app;
DROP TABLE IF EXISTS econtract_webhook_events;

-- +goose StatementEnd
