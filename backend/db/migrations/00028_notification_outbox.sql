-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- 00028: notification_outbox — アウトボックスパターン用テーブル
--
-- 各ドメインのサービスが自身のトランザクション内にここへ INSERT する。
-- notification サービスのポーリングジョブ (ProcessOutbox) がここを読み取り、
-- notification.Publish を呼んで配信する。
-- ドメイン間の直接 import を避ける疎結合設計を実現する。
--
-- セキュリティ原則:
--   - payload_json は参照ID (UUID) と非機微な表示値のみを格納する。
--     マイナンバー / 口座番号 / 健診結果などの復号値や平文 PII を含めてはならない。
--   - body_ref (opaque deep-link) にも PII を含めてはならない。
--   - actor_user_id は NULL 可 (システム起動イベント等)。
-- ===========================================================================

CREATE TABLE notification_outbox (
    id               uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id        uuid        NOT NULL REFERENCES tenants(id),
    -- event_type: ドメイン.アクション 形式 (例: "approval.decided", "leave.approved").
    event_type       text        NOT NULL,
    -- actor_user_id: イベントを発生させたユーザ (NULL = システム)。
    actor_user_id    uuid,
    -- recipient_user_id: 配信先ユーザ。
    recipient_user_id uuid       NOT NULL,
    -- resource_type / resource_id: 通知の deep-link 参照先 (opaque UUID)。
    resource_type    text        NOT NULL DEFAULT '',
    resource_id      uuid,
    -- body_ref: opaque deep-link パス / トークン。PII 禁止。
    body_ref         text        NOT NULL DEFAULT '',
    -- dedupe_key: 重複通知抑制キー (NULL = 抑制なし)。
    dedupe_key       text,
    -- status: "pending" | "processed" | "failed"
    status           text        NOT NULL DEFAULT 'pending',
    -- attempts: 処理試行回数。
    attempts         integer     NOT NULL DEFAULT 0,
    -- last_error: 処理失敗時の短いエラーコード / カテゴリ (PII 禁止)。
    last_error       text        NOT NULL DEFAULT '',
    -- processed_at: 処理完了日時。
    processed_at     timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_notification_outbox PRIMARY KEY (id),
    CONSTRAINT chk_notification_outbox_status
        CHECK (status IN ('pending', 'processed', 'failed')),
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_notification_outbox_id_tenant UNIQUE (id, tenant_id)
);

-- Polling index: pending 行を tenant 毎に古い順で取得。
CREATE INDEX idx_notification_outbox_pending
    ON notification_outbox (tenant_id, status, created_at)
    WHERE status = 'pending';

-- RLS
ALTER TABLE notification_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE notification_outbox FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON notification_outbox
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Grants
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE notification_outbox TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE notification_outbox FROM hr_app;
DROP TABLE IF EXISTS notification_outbox;

-- +goose StatementEnd
