-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- 00037: notification_chat — チャット通知連携 (Slack / Teams / LINE WORKS)
--
-- 追加内容:
--   1. notification_templates.channel CHECK を slack / teams / line_works に拡張。
--   2. notification_preferences.channel CHECK を同様に拡張。
--   3. tenant_chat_destinations — テナント別チャット送信先設定テーブル。
--      Webhook URL / 認証情報は Secret Manager 参照キー (env 変数名) を保持するのみ。
--      実値・生シークレットは一切保存しない。
--   4. chat_deliveries — チャット配信履歴テーブル (成否・リトライ)。
--
-- セキュリティ原則:
--   - tenant_chat_destinations に実 Webhook URL / トークンを保存しない。
--     env 変数名 (例: "NOTIFICATION_SLACK_WEBHOOK_URL") を保持し、
--     アプリが起動時に対応する環境変数を読む。
--   - chat_deliveries.delivery_token は送信先プラットフォームが返す配信 ID のみ。
--     メッセージ本文・宛先情報は保存しない (PII 禁止)。
--   - RLS FORCE + テナント分離規約を全テーブルに適用。
--
-- Migration 番号: 00034/00035/00036 が使用済み → 00037 を使用。
-- ===========================================================================

-- ---------------------------------------------------------------------------
-- 1. notification_templates.channel CHECK 拡張
-- ---------------------------------------------------------------------------
ALTER TABLE notification_templates
    DROP CONSTRAINT IF EXISTS chk_notification_templates_channel;

ALTER TABLE notification_templates
    ADD CONSTRAINT chk_notification_templates_channel
        CHECK (channel IN ('in_app', 'email', 'slack', 'teams', 'line_works'));

-- ---------------------------------------------------------------------------
-- 2. notification_preferences.channel CHECK 拡張
-- ---------------------------------------------------------------------------
ALTER TABLE notification_preferences
    DROP CONSTRAINT IF EXISTS chk_notification_preferences_channel;

ALTER TABLE notification_preferences
    ADD CONSTRAINT chk_notification_preferences_channel
        CHECK (channel IN ('in_app', 'email', 'slack', 'teams', 'line_works'));

-- ---------------------------------------------------------------------------
-- 3. tenant_chat_destinations — テナント別チャット送信先設定
-- ---------------------------------------------------------------------------
-- セキュリティ設計:
--   - env_key_ref: アプリが起動時に参照する環境変数名を保持する。
--     例: "NOTIFICATION_SLACK_WEBHOOK_URL"
--     実 Webhook URL / トークン値はこのテーブルには保存しない。
--   - channel: 'slack' | 'teams' | 'line_works'
--   - active: false のレコードは送信対象外。
--
-- テナント分離: RLS FORCE で tenant_id = current_setting('app.tenant_id').
CREATE TABLE tenant_chat_destinations (
    id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES tenants(id),
    -- channel: 'slack' | 'teams' | 'line_works'
    channel         text        NOT NULL,
    -- label: 管理画面用の表示名 (例: "#hr-alerts")。PII 禁止。
    label           text        NOT NULL DEFAULT '',
    -- env_key_ref: Webhook URL / トークンを保持する環境変数名 (実値は保存しない)。
    -- 例: "NOTIFICATION_SLACK_WEBHOOK_URL", "NOTIFICATION_TEAMS_WEBHOOK_URL"
    -- SECURITY: env_key_ref は変数名のみ。実シークレットをここに保存してはならない。
    env_key_ref     text        NOT NULL DEFAULT '',
    active          boolean     NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_tenant_chat_destinations PRIMARY KEY (id),
    CONSTRAINT chk_tenant_chat_destinations_channel
        CHECK (channel IN ('slack', 'teams', 'line_works')),
    -- One destination config per (tenant, channel).
    CONSTRAINT uq_tenant_chat_destinations_channel UNIQUE (tenant_id, channel),
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_tenant_chat_destinations_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_tenant_chat_destinations_active
    ON tenant_chat_destinations (tenant_id, channel, active)
    WHERE active = true;

-- RLS
ALTER TABLE tenant_chat_destinations ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_chat_destinations FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON tenant_chat_destinations
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE tenant_chat_destinations TO hr_app;

-- ---------------------------------------------------------------------------
-- 4. chat_deliveries — チャット配信履歴
-- ---------------------------------------------------------------------------
-- 成否・リトライ状態を記録する。メッセージ本文は保存しない (PII 禁止)。
-- status: 'queued' | 'sent' | 'failed'
-- delivery_token: 送信先プラットフォームが返す配信 ID (Slack ts など)。
--                 返さないプラットフォームは空文字。
CREATE TABLE chat_deliveries (
    id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES tenants(id),
    -- notification_id: 対応する notifications 行 (論理参照; RLS で分離)。
    notification_id uuid        NOT NULL,
    -- channel: 'slack' | 'teams' | 'line_works'
    channel         text        NOT NULL,
    -- event_type: 配信イベント種別 (監査用; PII 禁止)。
    event_type      text        NOT NULL DEFAULT '',
    -- status: 配信ステータス
    status          text        NOT NULL DEFAULT 'queued',
    -- attempts: 送信試行回数。
    attempts        integer     NOT NULL DEFAULT 0,
    max_attempts    integer     NOT NULL DEFAULT 3,
    -- last_error: 失敗時の短いエラーコード / カテゴリ (PII 禁止)。
    last_error      text        NOT NULL DEFAULT '',
    -- delivery_token: 送信成功時にプラットフォームが返す識別子 (PII 禁止)。
    delivery_token  text        NOT NULL DEFAULT '',
    sent_at         timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_chat_deliveries PRIMARY KEY (id),
    CONSTRAINT chk_chat_deliveries_channel
        CHECK (channel IN ('slack', 'teams', 'line_works')),
    CONSTRAINT chk_chat_deliveries_status
        CHECK (status IN ('queued', 'sent', 'failed')),
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_chat_deliveries_id_tenant UNIQUE (id, tenant_id)
);

-- 再試行ジョブ用インデックス: queued/failed 行をテナント毎に古い順で取得。
CREATE INDEX idx_chat_deliveries_pending
    ON chat_deliveries (tenant_id, channel, status, created_at)
    WHERE status IN ('queued', 'failed');

-- RLS
ALTER TABLE chat_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE chat_deliveries FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON chat_deliveries
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE chat_deliveries TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- chat_deliveries
REVOKE ALL ON TABLE chat_deliveries FROM hr_app;
DROP TABLE IF EXISTS chat_deliveries;

-- tenant_chat_destinations
REVOKE ALL ON TABLE tenant_chat_destinations FROM hr_app;
DROP TABLE IF EXISTS tenant_chat_destinations;

-- notification_preferences.channel CHECK を元に戻す
ALTER TABLE notification_preferences
    DROP CONSTRAINT IF EXISTS chk_notification_preferences_channel;

ALTER TABLE notification_preferences
    ADD CONSTRAINT chk_notification_preferences_channel
        CHECK (channel IN ('in_app', 'email'));

-- notification_templates.channel CHECK を元に戻す
ALTER TABLE notification_templates
    DROP CONSTRAINT IF EXISTS chk_notification_templates_channel;

ALTER TABLE notification_templates
    ADD CONSTRAINT chk_notification_templates_channel
        CHECK (channel IN ('in_app', 'email'));

-- +goose StatementEnd
