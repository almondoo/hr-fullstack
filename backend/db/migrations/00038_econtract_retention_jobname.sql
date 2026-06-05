-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- 00038  retention_job_runs.job_name CHECK 制約に 'econtract_retention' を追加
-- ===========================================================================
-- PURPOSE:
--   migration 00030 で定義した retention_job_runs.job_name の CHECK 制約は
--   'mynumber_disposal' / 'ledger_retention' / 'employee_data_policy' /
--   'document_expiry' の4値のみを許可していた。
--   econtract パッケージの RetentionRunner (econtract.JobEContractRetention) が
--   使用する 'econtract_retention' を追加する。
--
-- APPROACH:
--   PostgreSQL は inline CHECK の自動命名規則として
--   "<table>_<column>_check" を使用する。
--   DROP CONSTRAINT IF EXISTS で安全に削除し、名前付き制約として再作成する。
--   冪等性: IF NOT EXISTS / IF EXISTS を使用。
--
-- SECURITY:
--   - RLS 設定は変更しない (00030 で ENABLE / FORCE 済み)。
--   - PII はこの表に保存しない設計のまま変更しない。
--   - 物理削除なし・テナント分離維持。
-- ===========================================================================

-- 既存の inline CHECK 制約 (PostgreSQL 自動命名: retention_job_runs_job_name_check)
-- を削除し、'econtract_retention' を含む形で名前付き制約として再作成する。
ALTER TABLE retention_job_runs
    DROP CONSTRAINT IF EXISTS retention_job_runs_job_name_check;

ALTER TABLE retention_job_runs
    ADD CONSTRAINT chk_retention_job_runs_job_name
        CHECK (job_name IN (
            'mynumber_disposal',
            'ledger_retention',
            'employee_data_policy',
            'document_expiry',
            'econtract_retention'
        ));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Down: 'econtract_retention' を除いた元の4値制約に戻す。
ALTER TABLE retention_job_runs
    DROP CONSTRAINT IF EXISTS chk_retention_job_runs_job_name;

ALTER TABLE retention_job_runs
    ADD CONSTRAINT retention_job_runs_job_name_check
        CHECK (job_name IN (
            'mynumber_disposal',
            'ledger_retention',
            'employee_data_policy',
            'document_expiry'
        ));

-- +goose StatementEnd
