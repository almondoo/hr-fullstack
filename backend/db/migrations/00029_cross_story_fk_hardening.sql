-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- 00029: Cross-story composite FK hardening + leave_settings.created_at
-- ===========================================================================
-- 目的:
--   00027 で users に UNIQUE(id, tenant_id) を追加したことで、
--   cross-story 参照のうち users を参照先とする列を
--   「素の uuid 論理参照」から「複合 FK (id, tenant_id)」に昇格できるようになった。
--   本 migration ではその昇格と、leave_settings の created_at 欠損を補う。
--
-- 変更内容:
--   1. notifications.recipient_user_id
--        → FOREIGN KEY (recipient_user_id, tenant_id)
--           REFERENCES users(id, tenant_id) MATCH SIMPLE
--      (users の UNIQUE(id, tenant_id) は 00027 で追加済み)
--
--   2. notification_outbox.recipient_user_id
--        → FOREIGN KEY (recipient_user_id, tenant_id)
--           REFERENCES users(id, tenant_id) MATCH SIMPLE
--
--   3. notification_outbox.actor_user_id
--        → FOREIGN KEY (actor_user_id, tenant_id)
--           REFERENCES users(id, tenant_id) MATCH SIMPLE
--      (actor_user_id は NULL 許容 = MATCH SIMPLE でスキップ)
--
--   4. leave_settings に created_at を追加。
--      既存行は updated_at 値で埋める(同テーブルに created_at が無かったため)。
--
-- 見送り一覧 (理由は 00027 見送りコメントと同じ — テストseedが最小で FK 破損):
--   - selection_stages.job_posting_id → job_postings(id, tenant_id)
--   - applications.job_posting_id / .applicant_id
--   - offers.application_id → applications(id, tenant_id)
--   - interviews.application_id → applications(id, tenant_id)
--   - reviews.cycle_id / calibration_sessions.cycle_id → review_cycles(id, tenant_id)
-- ===========================================================================

-- ---------------------------------------------------------------------------
-- Step 1: notifications.recipient_user_id → 複合 FK に昇格
-- ---------------------------------------------------------------------------
-- 00009 の「素の uuid 列 + サービス層検証」を複合 FK に置き換える。
-- users の uq_users_id_tenant は 00027 Up で追加済み。
-- recipient_user_id は NOT NULL のため MATCH SIMPLE の NULL スキップは関係ないが
-- 他の FK との統一を保つため明示する。
ALTER TABLE notifications
    ADD CONSTRAINT fk_notifications_recipient_user_tenant
        FOREIGN KEY (recipient_user_id, tenant_id)
        REFERENCES users(id, tenant_id)
        MATCH SIMPLE;

-- ---------------------------------------------------------------------------
-- Step 2: notification_outbox.recipient_user_id → 複合 FK
-- ---------------------------------------------------------------------------
-- 00028 は outbox を新設した際に users の UNIQUE(id, tenant_id) を前提とできなかったため
-- 論理参照のままにしていた。00027 による UNIQUE 追加後、複合 FK を張れる。
ALTER TABLE notification_outbox
    ADD CONSTRAINT fk_notification_outbox_recipient_tenant
        FOREIGN KEY (recipient_user_id, tenant_id)
        REFERENCES users(id, tenant_id)
        MATCH SIMPLE;

-- ---------------------------------------------------------------------------
-- Step 3: notification_outbox.actor_user_id → 複合 FK (MATCH SIMPLE で NULL スキップ)
-- ---------------------------------------------------------------------------
-- actor_user_id は NULL 許容(システム起動イベント等)。
-- MATCH SIMPLE: actor_user_id が NULL の行は FK チェックをスキップする。
ALTER TABLE notification_outbox
    ADD CONSTRAINT fk_notification_outbox_actor_tenant
        FOREIGN KEY (actor_user_id, tenant_id)
        REFERENCES users(id, tenant_id)
        MATCH SIMPLE;

-- ---------------------------------------------------------------------------
-- Step 4: leave_settings.created_at を追加
-- ---------------------------------------------------------------------------
-- 00007 の CREATE TABLE では created_at が抜けており、updated_at のみだった。
-- 既存行は updated_at の値で補完する(スキーマ欠損の補修)。
-- 手順:
--   a. NULL 許容で追加 → 既存行 NULL
--   b. 既存行の NULL を updated_at で埋める
--   c. NOT NULL 制約を追加(全行が非 NULL になってから)
--   d. DEFAULT now() を設定して以降の INSERT で自動補完
ALTER TABLE leave_settings
    ADD COLUMN IF NOT EXISTS created_at timestamptz;

UPDATE leave_settings
    SET created_at = updated_at
    WHERE created_at IS NULL;

ALTER TABLE leave_settings
    ALTER COLUMN created_at SET NOT NULL,
    ALTER COLUMN created_at SET DEFAULT now();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- ---------------------------------------------------------------------------
-- Down: 逆順で制約・列を削除する
-- ---------------------------------------------------------------------------

-- Step 4 reversal: leave_settings.created_at
ALTER TABLE leave_settings
    DROP COLUMN IF EXISTS created_at;

-- Step 3 reversal: notification_outbox.actor_user_id FK
ALTER TABLE notification_outbox
    DROP CONSTRAINT IF EXISTS fk_notification_outbox_actor_tenant;

-- Step 2 reversal: notification_outbox.recipient_user_id FK
ALTER TABLE notification_outbox
    DROP CONSTRAINT IF EXISTS fk_notification_outbox_recipient_tenant;

-- Step 1 reversal: notifications.recipient_user_id FK
ALTER TABLE notifications
    DROP CONSTRAINT IF EXISTS fk_notifications_recipient_user_tenant;

-- +goose StatementEnd
