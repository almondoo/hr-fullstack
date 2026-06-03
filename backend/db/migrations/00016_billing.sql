-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- ST-FND-04 Billing / Subscriptions / Metered (seat) + flat-rate billing
-- ===========================================================================
-- Legal / accounting note (法令値・会計):
--   税率 (consumption tax rate)・締め日・課金サイクル・通貨・端数処理・トライアル
--   日数・含まれる席数・席単価・定額・解約/返金/日割りポリシー等は本マイグレーション
--   / アプリにハードコードしない。plans カタログ(プラン別) と subscriptions
--   (テナント別の enforcement_mode 等) に設定として保持し、改正に追従できるよう
--   構成化する。本実装は会計・税務の正確性を保証しないモック実装であり、適格請求書
--   (インボイス制度) 対応・正確な税計算は範囲外。最新の官公庁情報および税理士/
--   会計士/弁護士の確認が前提であり、本実装は法的・会計的助言ではない。
--
-- Payment note (決済):
--   決済はモックプロバイダ(provider='mock')。カード番号・PAN・生トークンは一切
--   受領・保存しない。payment_attempts.provider_ref は外部参照の opaque ID のみ。

-- ---------------------------------------------------------------------------
-- plans — グローバルなプランカタログ (全テナント共通)
-- ---------------------------------------------------------------------------
-- GLOBAL CATALOG: tenant_id を持たず RLS を適用しない(全テナント横断で参照可)。
-- tenants.plan_code / subscriptions.plan_code が plan_code を論理参照するマスタ。
-- hr_app には SELECT のみ GRANT。INSERT/UPDATE は管理ロール (migration / 管理 API /
-- 管理 DSN) 前提であり、業務ロール(hr_app)からは書き込めない。
-- monthly_base_fee/per_seat_fee は numeric(12,2)。feature_flags_json でプラン別の
-- 上位機能フラグを保持(法令値ではなく商用設定値)。
CREATE TABLE plans (
    id                uuid          NOT NULL DEFAULT gen_random_uuid(),
    plan_code         text          NOT NULL,
    name              text          NOT NULL,
    -- 定額月額 (法令値ではないが設定化対象。ハードコード禁止)
    monthly_base_fee  numeric(12,2) NOT NULL DEFAULT 0,
    -- 席単価 (従量課金単価)
    per_seat_fee      numeric(12,2) NOT NULL DEFAULT 0,
    -- プランに含まれる席数 (この席数までは席課金なし)
    included_seats    integer       NOT NULL DEFAULT 0,
    -- トライアル日数
    trial_days        integer       NOT NULL DEFAULT 0,
    -- プラン別機能フラグ {"advanced_reporting":true, ...}
    feature_flags_json jsonb        NOT NULL DEFAULT '{}',
    -- 通貨コード (ISO 4217)。端数処理規則等と合わせ設定化。
    currency          text          NOT NULL DEFAULT 'JPY',
    active            boolean       NOT NULL DEFAULT true,
    created_at        timestamptz   NOT NULL DEFAULT now(),
    updated_at        timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT pk_plans PRIMARY KEY (id),
    CONSTRAINT uq_plans_plan_code UNIQUE (plan_code),
    CONSTRAINT chk_plans_monthly_base_fee CHECK (monthly_base_fee >= 0),
    CONSTRAINT chk_plans_per_seat_fee     CHECK (per_seat_fee >= 0),
    CONSTRAINT chk_plans_included_seats   CHECK (included_seats >= 0),
    CONSTRAINT chk_plans_trial_days       CHECK (trial_days >= 0)
);

-- ---------------------------------------------------------------------------
-- subscriptions — テナント単位のサブスクリプション (RLS 適用)
-- ---------------------------------------------------------------------------
-- テナント1件につき有効サブスク1件 (uq_subscriptions_tenant)。
-- status 状態機械: trialing|active|past_due|canceled|expired を CHECK 制約。
-- plan_code は plans.plan_code を論理参照 (グローバルテーブルへの物理 FK は RLS 整合上
-- 張らずテキスト参照。論理参照先: plans.plan_code)。
-- enforcement_mode: soft|hard (席数/機能上限超過時の方針。設定化)。
CREATE TABLE subscriptions (
    id                   uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id            uuid        NOT NULL REFERENCES tenants(id),
    -- 論理参照先: plans.plan_code (グローバルカタログ。物理 FK は張らない)
    plan_code            text        NOT NULL,
    -- status: trialing|active|past_due|canceled|expired
    status               text        NOT NULL DEFAULT 'trialing',
    trial_ends_on        date,
    current_period_start date        NOT NULL,
    current_period_end   date        NOT NULL,
    canceled_at          timestamptz,
    cancel_at_period_end boolean     NOT NULL DEFAULT false,
    -- enforcement_mode: soft (警告+課金) | hard (機能制限)
    enforcement_mode     text        NOT NULL DEFAULT 'soft',
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_subscriptions PRIMARY KEY (id),
    CONSTRAINT chk_subscriptions_status
        CHECK (status IN ('trialing', 'active', 'past_due', 'canceled', 'expired')),
    CONSTRAINT chk_subscriptions_enforcement_mode
        CHECK (enforcement_mode IN ('soft', 'hard')),
    -- One active subscription record per tenant.
    CONSTRAINT uq_subscriptions_tenant UNIQUE (tenant_id),
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_subscriptions_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_subscriptions_lookup
    ON subscriptions (tenant_id, status);

-- ---------------------------------------------------------------------------
-- seat_usage_snapshots — 課金期間ごとの席数スナップショット (従量課金の根拠)
-- ---------------------------------------------------------------------------
-- billable_seats = 課金対象席 (status='active' の users 件数 or active employees 件数)。
-- 複合 FK (subscription_id, tenant_id) → subscriptions。
CREATE TABLE seat_usage_snapshots (
    id                uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id         uuid        NOT NULL REFERENCES tenants(id),
    subscription_id   uuid        NOT NULL,
    period_start      date        NOT NULL,
    period_end        date        NOT NULL,
    -- billable_seats: 課金対象席数 (= active user/employee 数のスナップショット)
    billable_seats    integer     NOT NULL DEFAULT 0,
    -- active_user_count: 算定根拠の active 件数 (監査/再現用)
    active_user_count integer     NOT NULL DEFAULT 0,
    captured_at       timestamptz NOT NULL DEFAULT now(),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_seat_usage_snapshots PRIMARY KEY (id),
    CONSTRAINT chk_seat_usage_snapshots_billable_seats CHECK (billable_seats >= 0),
    CONSTRAINT chk_seat_usage_snapshots_active_user_count CHECK (active_user_count >= 0),
    -- [Security] Composite FK: (subscription_id, tenant_id) must exist in subscriptions.
    CONSTRAINT fk_seat_usage_snapshots_subscription_tenant
        FOREIGN KEY (subscription_id, tenant_id)
        REFERENCES subscriptions(id, tenant_id)
        MATCH SIMPLE,
    -- One snapshot per (tenant, subscription, period_start).
    CONSTRAINT uq_seat_usage_snapshots_period
        UNIQUE (tenant_id, subscription_id, period_start),
    CONSTRAINT uq_seat_usage_snapshots_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_seat_usage_snapshots_lookup
    ON seat_usage_snapshots (tenant_id, subscription_id, period_start);

-- ---------------------------------------------------------------------------
-- invoices — 請求書 (RLS 適用)
-- ---------------------------------------------------------------------------
-- 発行後イミュータブル (金額訂正は新規/クレジット明細で行う)。アプリ層で生成後の
-- 金額更新を禁止する。invoice_number はテナント内連番 (uq tenant_id+invoice_number)。
-- status: draft|open|paid|void|uncollectible を CHECK。金額は numeric(12,2)。
-- 複合 FK (subscription_id, tenant_id) → subscriptions。
CREATE TABLE invoices (
    id                uuid          NOT NULL DEFAULT gen_random_uuid(),
    tenant_id         uuid          NOT NULL REFERENCES tenants(id),
    subscription_id   uuid          NOT NULL,
    -- invoice_number: テナント内連番 (推測困難な表示番号の併設も将来可)
    invoice_number    text          NOT NULL,
    period_start      date          NOT NULL,
    period_end        date          NOT NULL,
    subtotal          numeric(12,2) NOT NULL DEFAULT 0,
    tax_amount        numeric(12,2) NOT NULL DEFAULT 0,
    total             numeric(12,2) NOT NULL DEFAULT 0,
    currency          text          NOT NULL DEFAULT 'JPY',
    -- status: draft|open|paid|void|uncollectible
    status            text          NOT NULL DEFAULT 'draft',
    issued_on         date,
    due_on            date,
    paid_at           timestamptz,
    created_at        timestamptz   NOT NULL DEFAULT now(),
    updated_at        timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT pk_invoices PRIMARY KEY (id),
    CONSTRAINT chk_invoices_status
        CHECK (status IN ('draft', 'open', 'paid', 'void', 'uncollectible')),
    CONSTRAINT chk_invoices_subtotal   CHECK (subtotal >= 0),
    CONSTRAINT chk_invoices_tax_amount CHECK (tax_amount >= 0),
    CONSTRAINT chk_invoices_total      CHECK (total >= 0),
    -- [Security] Composite FK: (subscription_id, tenant_id) must exist in subscriptions.
    CONSTRAINT fk_invoices_subscription_tenant
        FOREIGN KEY (subscription_id, tenant_id)
        REFERENCES subscriptions(id, tenant_id)
        MATCH SIMPLE,
    -- Invoice number unique within a tenant.
    CONSTRAINT uq_invoices_tenant_number UNIQUE (tenant_id, invoice_number),
    CONSTRAINT uq_invoices_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_invoices_lookup
    ON invoices (tenant_id, subscription_id, status);

-- ---------------------------------------------------------------------------
-- invoice_line_items — 請求明細行 (RLS 適用)
-- ---------------------------------------------------------------------------
-- kind: base_fee|seat_overage|discount|tax|credit を CHECK。
-- amount = quantity × unit_price (符号付き、discount/credit は負)。
-- 複合 FK (invoice_id, tenant_id) → invoices。
CREATE TABLE invoice_line_items (
    id          uuid          NOT NULL DEFAULT gen_random_uuid(),
    tenant_id   uuid          NOT NULL REFERENCES tenants(id),
    invoice_id  uuid          NOT NULL,
    -- kind: base_fee|seat_overage|discount|tax|credit
    kind        text          NOT NULL,
    description text          NOT NULL DEFAULT '',
    quantity    numeric(12,2) NOT NULL DEFAULT 0,
    unit_price  numeric(12,2) NOT NULL DEFAULT 0,
    -- amount: 符号付き (discount/credit は負値を許容するため CHECK しない)
    amount      numeric(12,2) NOT NULL DEFAULT 0,
    created_at  timestamptz   NOT NULL DEFAULT now(),
    updated_at  timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT pk_invoice_line_items PRIMARY KEY (id),
    CONSTRAINT chk_invoice_line_items_kind
        CHECK (kind IN ('base_fee', 'seat_overage', 'discount', 'tax', 'credit')),
    -- [Security] Composite FK: (invoice_id, tenant_id) must exist in invoices.
    CONSTRAINT fk_invoice_line_items_invoice_tenant
        FOREIGN KEY (invoice_id, tenant_id)
        REFERENCES invoices(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_invoice_line_items_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_invoice_line_items_lookup
    ON invoice_line_items (tenant_id, invoice_id, kind);

-- ---------------------------------------------------------------------------
-- payment_attempts — モック決済の試行記録 (RLS 適用)
-- ---------------------------------------------------------------------------
-- provider='mock' (将来 'stripe')。provider_ref は外部参照の opaque ID のみ。
-- SECURITY: カード番号・PAN・生トークンは絶対に格納しない (列自体を設けない)。
-- status: pending|succeeded|failed を CHECK。複合 FK (invoice_id, tenant_id)。
CREATE TABLE payment_attempts (
    id             uuid          NOT NULL DEFAULT gen_random_uuid(),
    tenant_id      uuid          NOT NULL REFERENCES tenants(id),
    invoice_id     uuid          NOT NULL,
    provider       text          NOT NULL DEFAULT 'mock',
    -- provider_ref: 外部参照の opaque ID のみ。PAN/トークン等は格納しない。
    provider_ref   text,
    amount         numeric(12,2) NOT NULL DEFAULT 0,
    -- status: pending|succeeded|failed
    status         text          NOT NULL DEFAULT 'pending',
    failure_reason text,
    attempted_at   timestamptz   NOT NULL DEFAULT now(),
    created_at     timestamptz   NOT NULL DEFAULT now(),
    updated_at     timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT pk_payment_attempts PRIMARY KEY (id),
    CONSTRAINT chk_payment_attempts_status
        CHECK (status IN ('pending', 'succeeded', 'failed')),
    CONSTRAINT chk_payment_attempts_amount CHECK (amount >= 0),
    -- [Security] Composite FK: (invoice_id, tenant_id) must exist in invoices.
    CONSTRAINT fk_payment_attempts_invoice_tenant
        FOREIGN KEY (invoice_id, tenant_id)
        REFERENCES invoices(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_payment_attempts_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_payment_attempts_lookup
    ON payment_attempts (tenant_id, invoice_id, status);

-- ---------------------------------------------------------------------------
-- tenant_provisioning — プロビジョニング進捗 (ウィザード骨格、RLS 適用)
-- ---------------------------------------------------------------------------
-- status: pending|in_progress|completed|failed。steps_json: 各初期化ステップの状態。
-- 冪等実行を担保するためのチェックポイント。テナント1件につき1行 (uq_tenant_provisioning)。
CREATE TABLE tenant_provisioning (
    id                 uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id          uuid        NOT NULL REFERENCES tenants(id),
    -- status: pending|in_progress|completed|failed
    status             text        NOT NULL DEFAULT 'pending',
    -- steps_json: {"roles":"done","settings":"done","sample_data":"skipped"}
    steps_json         jsonb       NOT NULL DEFAULT '{}',
    sample_data_loaded boolean     NOT NULL DEFAULT false,
    completed_at       timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_tenant_provisioning PRIMARY KEY (id),
    CONSTRAINT chk_tenant_provisioning_status
        CHECK (status IN ('pending', 'in_progress', 'completed', 'failed')),
    -- One provisioning record per tenant (idempotency anchor).
    CONSTRAINT uq_tenant_provisioning_tenant UNIQUE (tenant_id),
    CONSTRAINT uq_tenant_provisioning_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_tenant_provisioning_lookup
    ON tenant_provisioning (tenant_id, status);

-- ---------------------------------------------------------------------------
-- RLS — tenant-scoped tables only (plans is a global catalog: no RLS)
-- ---------------------------------------------------------------------------
ALTER TABLE subscriptions        ENABLE ROW LEVEL SECURITY;
ALTER TABLE subscriptions        FORCE  ROW LEVEL SECURITY;
ALTER TABLE seat_usage_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE seat_usage_snapshots FORCE  ROW LEVEL SECURITY;
ALTER TABLE invoices             ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoices             FORCE  ROW LEVEL SECURITY;
ALTER TABLE invoice_line_items   ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoice_line_items   FORCE  ROW LEVEL SECURITY;
ALTER TABLE payment_attempts     ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_attempts     FORCE  ROW LEVEL SECURITY;
ALTER TABLE tenant_provisioning  ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_provisioning  FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON subscriptions
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON seat_usage_snapshots
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON invoices
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON invoice_line_items
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON payment_attempts
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON tenant_provisioning
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants
-- ---------------------------------------------------------------------------
-- plans is a GLOBAL catalog (no RLS): hr_app may SELECT only.  INSERT/UPDATE
-- are reserved for the admin/management role (migrations / management API via
-- the admin DSN); the business role (hr_app) must NOT be able to mutate the
-- global plan catalog.
GRANT SELECT ON TABLE plans TO hr_app;

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE subscriptions        TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE seat_usage_snapshots TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE invoices             TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE invoice_line_items   TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE payment_attempts     TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE tenant_provisioning  TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE tenant_provisioning  FROM hr_app;
REVOKE ALL ON TABLE payment_attempts     FROM hr_app;
REVOKE ALL ON TABLE invoice_line_items   FROM hr_app;
REVOKE ALL ON TABLE invoices             FROM hr_app;
REVOKE ALL ON TABLE seat_usage_snapshots FROM hr_app;
REVOKE ALL ON TABLE subscriptions        FROM hr_app;
REVOKE ALL ON TABLE plans                FROM hr_app;

DROP TABLE IF EXISTS tenant_provisioning;
DROP TABLE IF EXISTS payment_attempts;
DROP TABLE IF EXISTS invoice_line_items;
DROP TABLE IF EXISTS invoices;
DROP TABLE IF EXISTS seat_usage_snapshots;
DROP TABLE IF EXISTS subscriptions;
DROP TABLE IF EXISTS plans;

-- +goose StatementEnd
