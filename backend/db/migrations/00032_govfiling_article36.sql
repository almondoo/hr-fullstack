-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- 36協定 e-Gov電子届出 足場 (Issue #21)
-- ===========================================================================
-- スコープ:
--   1. gov_filings.filing_type の CHECK に 'article36_filing' を追加する。
--      (e-Gov 36協定届の申請種別識別子。実送信は外部認証情報待ち)
--   2. labor_agreement_documents に govfiling_id 列を追加する。
--      workrule パッケージから govfiling パッケージへの接続点を提供する。
--      govfiling_id は NULL 許容 (届出フロー開始前 = NULL)。
--      (id, tenant_id) 複合 FK で govfiling の テナント境界を保証する。
--
-- セキュリティ注記:
--   - RLS・テナント分離ポリシーは既存の gov_filings テーブルに設定済み。
--     labor_agreement_documents 側は 00019_workrule.sql で設定済み。
--   - govfiling_id は gov_filings の opaque ID のみを保持する。
--     PII・機微情報を含む列ではない。
--   - マイナンバー等の復号値はいずれの列にも格納しない。
--
-- 法令注記: 36協定様式・e-Gov送信方式・保存年限等の法令値は govfiling の
--   form_version_json / insurance_settings 設定から供給する。本マイグレーション
--   に法令値はハードコードしない。法令値は最新官公庁情報・社労士/弁護士確認前提。
-- ===========================================================================

-- ---------------------------------------------------------------------------
-- 1. gov_filings.filing_type: CHECK 制約を article36_filing を含む形に拡張
-- ---------------------------------------------------------------------------
-- 既存 CHECK 制約を削除して再定義する。
ALTER TABLE gov_filings DROP CONSTRAINT IF EXISTS chk_gov_filings_filing_type;

ALTER TABLE gov_filings ADD CONSTRAINT chk_gov_filings_filing_type
    CHECK (filing_type IN (
        'health_insurance_acquire',
        'health_insurance_lose',
        'pension_calc',
        'pension_change',
        'employment_insurance_acquire',
        'employment_insurance_lose',
        'employment_insurance_separation',
        'workers_comp_report',
        -- 36協定 e-Gov電子届出 (Issue #21 足場)
        -- 実送信は e-Gov サンドボックス認証情報取得後に実装 (外部依存)。
        'article36_filing'
    ));

-- ---------------------------------------------------------------------------
-- 2. labor_agreement_documents: govfiling_id 列の追加
-- ---------------------------------------------------------------------------
-- govfiling_id は workrule → govfiling 接続点。
--   - NULL: 届出フロー未開始 (デフォルト)。
--   - non-NULL: govfiling.CreateArticle36Filing 呼出し後に設定する。
-- 複合 FK (govfiling_id, tenant_id) → gov_filings(id, tenant_id) で
-- テナント境界を保証する。
ALTER TABLE labor_agreement_documents
    ADD COLUMN IF NOT EXISTS govfiling_id uuid;

-- [Security] Composite FK: govfiling_id と tenant_id の組み合わせが
-- gov_filings(id, tenant_id) に存在することを保証する (クロステナント防止)。
-- MATCH SIMPLE: govfiling_id が NULL の場合は制約を無視 (届出前は NULL)。
ALTER TABLE labor_agreement_documents
    ADD CONSTRAINT fk_labor_agreement_docs_govfiling_tenant
        FOREIGN KEY (govfiling_id, tenant_id)
        REFERENCES gov_filings(id, tenant_id)
        MATCH SIMPLE
        ON UPDATE NO ACTION
        ON DELETE NO ACTION;

CREATE INDEX IF NOT EXISTS idx_labor_agreement_docs_govfiling
    ON labor_agreement_documents (tenant_id, govfiling_id)
    WHERE govfiling_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- 逆順で削除する。

DROP INDEX IF EXISTS idx_labor_agreement_docs_govfiling;

ALTER TABLE labor_agreement_documents
    DROP CONSTRAINT IF EXISTS fk_labor_agreement_docs_govfiling_tenant;

ALTER TABLE labor_agreement_documents
    DROP COLUMN IF EXISTS govfiling_id;

-- filing_type CHECK を元の値セットに戻す。
ALTER TABLE gov_filings DROP CONSTRAINT IF EXISTS chk_gov_filings_filing_type;

ALTER TABLE gov_filings ADD CONSTRAINT chk_gov_filings_filing_type
    CHECK (filing_type IN (
        'health_insurance_acquire', 'health_insurance_lose',
        'pension_calc', 'pension_change',
        'employment_insurance_acquire', 'employment_insurance_lose',
        'employment_insurance_separation',
        'workers_comp_report'
    ));

-- +goose StatementEnd
