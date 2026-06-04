-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- 00035  法定三帳簿 retention_expired フラグ追加
-- ===========================================================================
-- PURPOSE:
--   worker_rosters / wage_ledgers / attendance_books の各行に
--   retention_expired boolean 列を追加する。
--   retention package の RunLedgerRetention が保存年限切れレコードを
--   audit_logs へ書くだけでなく、行レベルで失効フラグを立てられるようにする。
--
-- DESIGN:
--   - NOT NULL DEFAULT false — 既存行は全て「未失効」として扱う。
--   - 失効操作は論理フラグのみ (物理削除なし) — 真実性 (電子帳簿保存法) を維持。
--   - フィールドは retention_until の直後に論理的に配置する。
--   - 既存の ledger_block_finalized_mutation トリガーによる immutability 保護:
--       finalized_at が SET されたレコードでも retention_expired は UPDATE 可。
--       これは合法: 保存年限の到来は確定後に起きる外部イベントであり、
--       レコード内容 (wage_json 等) の改変ではない。
--       → トリガーは業務カラム (*_json, retention_basis*, retention_until) を
--         保護するが retention_expired は保護対象外とする。
--         (migration 00015 の ledger_block_finalized_mutation 参照)
--
-- RLS FORCE:
--   各テーブルは既に migration 00015 で RLS ENABLE + FORCE 済み。
--   本 migration で追加する列のみ; ポリシー変更は不要。
--
-- LEGAL NOTE:
--   保存年限 (原則5年 / 経過措置3年) は ledger_settings.default_retention_years
--   で管理され、本 migration には一切ハードコードしない。
--   法令値の最終確認は社労士/弁護士による一次法令確認が前提。
--   本実装は法的助言ではない。
--
-- IDEMPOTENCY:
--   IF NOT EXISTS / ADD COLUMN IF NOT EXISTS を使用するため
--   再実行しても安全。

-- ---------------------------------------------------------------------------
-- worker_rosters — 労働者名簿
-- ---------------------------------------------------------------------------
ALTER TABLE worker_rosters
    ADD COLUMN IF NOT EXISTS retention_expired boolean NOT NULL DEFAULT false;

COMMENT ON COLUMN worker_rosters.retention_expired IS
    '保存年限失効フラグ。retention_until < now() かつ RunLedgerRetention 実行済み時に true へ更新される。論理削除のみ (物理削除不可 / 真実性維持)。';

CREATE INDEX IF NOT EXISTS idx_worker_rosters_retention_expired
    ON worker_rosters (tenant_id, retention_expired)
    WHERE retention_expired = false;

-- ---------------------------------------------------------------------------
-- wage_ledgers — 賃金台帳
-- ---------------------------------------------------------------------------
ALTER TABLE wage_ledgers
    ADD COLUMN IF NOT EXISTS retention_expired boolean NOT NULL DEFAULT false;

COMMENT ON COLUMN wage_ledgers.retention_expired IS
    '保存年限失効フラグ。retention_until < now() かつ RunLedgerRetention 実行済み時に true へ更新される。論理削除のみ (物理削除不可 / 真実性維持)。';

CREATE INDEX IF NOT EXISTS idx_wage_ledgers_retention_expired
    ON wage_ledgers (tenant_id, retention_expired)
    WHERE retention_expired = false;

-- ---------------------------------------------------------------------------
-- attendance_books — 出勤簿
-- ---------------------------------------------------------------------------
ALTER TABLE attendance_books
    ADD COLUMN IF NOT EXISTS retention_expired boolean NOT NULL DEFAULT false;

COMMENT ON COLUMN attendance_books.retention_expired IS
    '保存年限失効フラグ。retention_until < now() かつ RunLedgerRetention 実行済み時に true へ更新される。論理削除のみ (物理削除不可 / 真実性維持)。';

CREATE INDEX IF NOT EXISTS idx_attendance_books_retention_expired
    ON attendance_books (tenant_id, retention_expired)
    WHERE retention_expired = false;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_worker_rosters_retention_expired;
ALTER TABLE worker_rosters DROP COLUMN IF EXISTS retention_expired;

DROP INDEX IF EXISTS idx_wage_ledgers_retention_expired;
ALTER TABLE wage_ledgers DROP COLUMN IF EXISTS retention_expired;

DROP INDEX IF EXISTS idx_attendance_books_retention_expired;
ALTER TABLE attendance_books DROP COLUMN IF EXISTS retention_expired;

-- +goose StatementEnd
