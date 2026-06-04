-- +goose Up
-- ---------------------------------------------------------------------
-- retention_job_runs — バックグラウンド保持/廃棄ジョブの実行履歴
--
-- 用途:
--   各ジョブ実行(run)ごとに1行記録し、次回実行時の重複防止・監査証跡・
--   オペレーション可視性を提供する。
--
-- セキュリティ設計:
--   - PII / 個人番号 / 機微情報はこの表に一切保存しない。
--     affected_count / skipped_count はレコード件数のみ。
--   - RLS: tenant_id スコープ + FORCE (fail-closed)。
--     システム全体集計(テナント横断)は hr_admin ロールが直接参照する。
--   - job_name / status / notes 列は自由テキストだが PII を含めないこと
--     (システム運用情報のみ)。
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS retention_job_runs (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        uuid        NOT NULL REFERENCES tenants (id),
    job_name         text        NOT NULL
        CHECK (job_name IN (
            'mynumber_disposal',
            'ledger_retention',
            'employee_data_policy',
            'document_expiry'
        )),
    status           text        NOT NULL DEFAULT 'running'
        CHECK (status IN ('running', 'completed', 'failed', 'skipped')),
    -- affected_count: 今回のrun で廃棄/失効/処理したレコード件数
    affected_count   int         NOT NULL DEFAULT 0,
    -- skipped_count: 処理対象だったが保留/スキップしたレコード件数
    skipped_count    int         NOT NULL DEFAULT 0,
    notes            text,
    started_at       timestamptz NOT NULL DEFAULT now(),
    finished_at      timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

-- RLS
ALTER TABLE retention_job_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE retention_job_runs FORCE ROW LEVEL SECURITY;

CREATE POLICY rls_retention_job_runs ON retention_job_runs
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE INDEX idx_retention_job_runs_tenant_job
    ON retention_job_runs (tenant_id, job_name, started_at DESC);

COMMENT ON TABLE retention_job_runs IS
    'バックグラウンド保持/廃棄ジョブ(retention package)の実行履歴。PII不可。';

-- +goose Down
DROP TABLE IF EXISTS retention_job_runs;
