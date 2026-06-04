-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- 00036  電子契約文書 保管/廃棄ジョブ用カラム追加
-- ===========================================================================
-- PURPOSE:
--   offer_letters テーブルに電子契約文書の保管期限管理に必要な列を追加する。
--   econtract パッケージの RunEContractRetention ジョブが
--   offer_settings.retention_years に基づいて締結済み文書を論理失効させる際に
--   参照・更新する。
--
-- 追加列:
--   retention_expires_on  — 保管期限 (signed_at + offer_settings.retention_years)。
--                            NULL = 未設定 or 未締結。
--   legally_held          — 訴訟保全・調査等によるリーガルホールドフラグ。
--                            true の間は retention_expires_on 経過後も失効処理をスキップ。
--   logically_expired     — 保管期限到来後の論理失効フラグ。
--                            物理削除しない (電子帳簿保存法 真実性維持)。
--   retention_expired_at  — 論理失効が確定した日時 (ジョブ実行時刻)。
--                            監査証跡として保持。
--
-- DESIGN:
--   - 物理削除は一切行わない。失効操作は logical フラグ (logically_expired) のみ。
--   - legally_held = true の行は retension_expires_on に関わらず失効処理対象外。
--   - 冪等性: logically_expired = false の行のみを対象とする WHERE 句により、
--     再実行しても同一行が二重処理されない。
--   - RLS ENABLE + FORCE: offer_letters は 00021 で既に設定済み。
--     本 migration ではポリシー変更は不要 (列追加のみ)。
--
-- LEGAL NOTE:
--   電子契約文書の保存年限 (電子帳簿保存法・e-文書法 等) の確定値は
--   社労士 / 弁護士との一次法令確認が前提。
--   本 migration の DEFAULT 7 は offer_settings.retention_years の既定値に
--   倣った参考値であり、法的根拠を保証するものではない。
--   運用者は offer_settings.retention_years を最新の法令に合わせて設定すること。
--   本実装は法的助言ではない。
--
-- IDEMPOTENCY:
--   ADD COLUMN IF NOT EXISTS を使用するため再実行しても安全。

-- ---------------------------------------------------------------------------
-- offer_letters — 電子契約文書 保管/廃棄 カラム追加
-- ---------------------------------------------------------------------------
ALTER TABLE offer_letters
    ADD COLUMN IF NOT EXISTS retention_expires_on  date,
    ADD COLUMN IF NOT EXISTS legally_held          boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS logically_expired     boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS retention_expired_at  timestamptz;

COMMENT ON COLUMN offer_letters.retention_expires_on IS
    '保管期限 (signed_at + offer_settings.retention_years を基に CalcRetentionExpiry で算出)。'
    'NULL = 未署名 or 保管期限未計算。RunEContractRetention がこの日付を参照して失効判定する。'
    '法令保存年限は offer_settings.retention_years で管理し、ここにはハードコードしない。';

COMMENT ON COLUMN offer_letters.legally_held IS
    'リーガルホールドフラグ。訴訟保全・行政調査等により true にセットされた行は'
    ' retention_expires_on が到来しても RunEContractRetention が失効処理をスキップする。';

COMMENT ON COLUMN offer_letters.logically_expired IS
    '論理失効フラグ。RunEContractRetention により保管期限到来時に true へ更新される。'
    '物理削除は行わない (電子帳簿保存法 真実性維持)。';

COMMENT ON COLUMN offer_letters.retention_expired_at IS
    '論理失効確定日時 (RunEContractRetention 実行時刻)。監査証跡として保持。';

-- 保管期限による失効対象検索インデックス
-- RunEContractRetention の SELECT クエリに対応する partial index。
-- logically_expired = false の行に絞ることで通常運用時のスキャン対象を最小化する。
CREATE INDEX IF NOT EXISTS idx_offer_letters_retention
    ON offer_letters (tenant_id, retention_expires_on)
    WHERE logically_expired = false AND legally_held = false;

-- リーガルホールド行の別途管理用インデックス
CREATE INDEX IF NOT EXISTS idx_offer_letters_legally_held
    ON offer_letters (tenant_id, legally_held)
    WHERE legally_held = true;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_offer_letters_legally_held;
DROP INDEX IF EXISTS idx_offer_letters_retention;

ALTER TABLE offer_letters
    DROP COLUMN IF EXISTS retention_expired_at,
    DROP COLUMN IF EXISTS logically_expired,
    DROP COLUMN IF EXISTS legally_held,
    DROP COLUMN IF EXISTS retention_expires_on;

-- +goose StatementEnd
