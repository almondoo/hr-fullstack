-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- ST-ATS-04: 面接調整(カレンダー連携・候補日・リマインド)と評価/フィードバック収集
-- docs/01 §C-2 ATS-012 / ATS-013, INT-005 (Google/Outlook calendar linkage).
--
-- 法令/コンプラ注記:
--   - 評価項目(evaluation_sheets.items_json)・推奨ラベルに差別的/不適切な評価軸を
--     含めない運用が必要なため、項目セットは設定化(テナント別テンプレ)している。
--     要専門家確認(CMP-004/CMP-005 人事データ利活用原則)。本実装は法的助言ではない。
--   - AIによる候補者スコアリングは本MVPでは未導入(人手評価のみ)。将来導入時は
--     AI事業者ガイドライン(CMP-005)準拠が必要 — 設計余地のみ残す。
--   - 面接記録・評価の保持期間は ST-ATS-02 の retention ポリシーに従属(設定化)。
-- ===========================================================================

-- ---------------------------------------------------------------------------
-- interviews (面接)
-- ---------------------------------------------------------------------------
-- application_id is a LOGICAL reference to applications(id) (ST-ATS-03, a
-- separate story's table).  Per the cross-story isolation rule we do NOT add a
-- foreign key to applications; we store a bare uuid + index only and validate
-- tenant scoping in the service layer (RLS + explicit tenant_id WHERE).
--
-- status:  'proposed' | 'confirmed' | 'completed' | 'cancelled'
-- format:  'onsite' | 'online' | 'phone'
-- external_event_id: opaque calendar event id for INT-005 (Google/Outlook).
--   NULL allowed.  Calendar sync is a best-effort side-effect: a sync failure
--   must never corrupt interview data, so this column is loosely coupled and
--   never participates in a constraint.
-- online_url: meeting URL (low sensitivity).
-- scheduled_at: set when the interview is confirmed; kept consistent with the
--   selected interview_slot in a single transaction.
CREATE TABLE interviews (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    -- application_id: LOGICAL reference to applications(id) (ST-ATS-03). No FK.
    application_id      uuid        NOT NULL,
    -- status: 'proposed' | 'confirmed' | 'completed' | 'cancelled'
    status              text        NOT NULL DEFAULT 'proposed',
    -- format: 'onsite' | 'online' | 'phone'
    format              text        NOT NULL DEFAULT 'onsite',
    scheduled_at        timestamptz,
    online_url          text,
    -- external_event_id: opaque calendar event id (INT-005). Best-effort sync.
    external_event_id   text,
    notes               text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_interviews PRIMARY KEY (id),
    CONSTRAINT chk_interviews_status
        CHECK (status IN ('proposed', 'confirmed', 'completed', 'cancelled')),
    CONSTRAINT chk_interviews_format
        CHECK (format IN ('onsite', 'online', 'phone')),
    -- UNIQUE(id, tenant_id) required for downstream composite FK references
    -- (interview_slots / interview_panelists / interview_evaluations).
    CONSTRAINT uq_interviews_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_interviews_lookup
    ON interviews (tenant_id, application_id, status);
-- Index for calendar reconciliation lookups by external event id.
CREATE INDEX idx_interviews_external_event
    ON interviews (tenant_id, external_event_id);

-- ---------------------------------------------------------------------------
-- interview_slots (候補日時の提示・選択)
-- ---------------------------------------------------------------------------
-- (interview_id, tenant_id) composite FK to interviews (own-package table).
-- selected=true marks the confirmed slot; it must be consistent with
-- interviews.scheduled_at (synchronised in a single service transaction).
CREATE TABLE interview_slots (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    interview_id        uuid        NOT NULL,
    candidate_start     timestamptz NOT NULL,
    candidate_end       timestamptz,
    selected            boolean     NOT NULL DEFAULT false,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_interview_slots PRIMARY KEY (id),
    -- [Security] composite FK: (interview_id, tenant_id) must exist in interviews.
    CONSTRAINT fk_interview_slots_interview_tenant
        FOREIGN KEY (interview_id, tenant_id)
        REFERENCES interviews(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_interview_slots_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_interview_slots_lookup
    ON interview_slots (tenant_id, interview_id, selected);

-- ---------------------------------------------------------------------------
-- interview_panelists (面接官アサイン — 多対多)
-- ---------------------------------------------------------------------------
-- (interview_id, tenant_id) composite FK to interviews (own-package table).
-- user_id is a LOGICAL reference to users(id): the users table has only
-- PRIMARY KEY(id) (no UNIQUE(id, tenant_id)), so a composite FK is not
-- possible.  We store a bare uuid and validate (user_id, tenant_id) existence
-- in the service layer (matching the onboarding AssignTask pattern).
-- role: 'interviewer' | 'observer'
CREATE TABLE interview_panelists (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    interview_id        uuid        NOT NULL,
    -- user_id: LOGICAL reference to users(id). No composite FK (users lacks
    -- UNIQUE(id, tenant_id)); tenant scoping enforced in service + RLS.
    user_id             uuid        NOT NULL,
    -- role: 'interviewer' | 'observer'
    role                text        NOT NULL DEFAULT 'interviewer',
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_interview_panelists PRIMARY KEY (id),
    CONSTRAINT chk_interview_panelists_role
        CHECK (role IN ('interviewer', 'observer')),
    -- [Security] composite FK: (interview_id, tenant_id) must exist in interviews.
    CONSTRAINT fk_interview_panelists_interview_tenant
        FOREIGN KEY (interview_id, tenant_id)
        REFERENCES interviews(id, tenant_id)
        MATCH SIMPLE,
    -- One assignment per (interview, user) within a tenant.
    CONSTRAINT uq_interview_panelists_unique
        UNIQUE (tenant_id, interview_id, user_id),
    CONSTRAINT uq_interview_panelists_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_interview_panelists_lookup
    ON interview_panelists (tenant_id, interview_id);
CREATE INDEX idx_interview_panelists_user
    ON interview_panelists (tenant_id, user_id);

-- ---------------------------------------------------------------------------
-- evaluation_sheets (評価シート定義 — テナント別の評価項目テンプレ)
-- ---------------------------------------------------------------------------
-- items_json: structured JSON of evaluation axes (項目名/尺度/重み).
--   Format: [{"key":"tech","label":"技術力","scale":5,"weight":1.0}]
-- Compliance: 評価軸は差別的/不適切な内容を含めない運用が前提(要専門家確認)。
CREATE TABLE evaluation_sheets (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    name                text        NOT NULL,
    items_json          jsonb       NOT NULL DEFAULT '[]',
    active              boolean     NOT NULL DEFAULT true,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_evaluation_sheets PRIMARY KEY (id),
    -- UNIQUE(id, tenant_id) required for downstream composite FK references.
    CONSTRAINT uq_evaluation_sheets_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_evaluation_sheets_lookup
    ON evaluation_sheets (tenant_id, active);

-- ---------------------------------------------------------------------------
-- interview_evaluations (面接官の評価/スコア)
-- ---------------------------------------------------------------------------
-- (interview_id, tenant_id) and (sheet_id, tenant_id) composite FK to
-- own-package tables.  evaluator_user_id and application_id are LOGICAL
-- references (users / applications) and use bare uuid + service-layer checks.
--
-- scores_json: per-item scores. Format: {"tech":4,"culture":5}
-- comment: free-text feedback — selection-sensitive (機微).  It is NOT 要配慮
--   PII (so not encrypted), but reads of the comment are gated by the
--   ats:evaluation:read permission and recorded in the audit log.
-- recommendation: 'strong_yes' | 'yes' | 'neutral' | 'no' | 'strong_no'
-- overall_score: aggregate score (nullable).
--
-- UNIQUE(tenant_id, interview_id, evaluator_user_id): one evaluation per
-- panelist per interview.
CREATE TABLE interview_evaluations (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    interview_id        uuid        NOT NULL,
    -- application_id: LOGICAL reference to applications(id) (ST-ATS-03). No FK.
    application_id      uuid        NOT NULL,
    -- evaluator_user_id: LOGICAL reference to users(id). No composite FK.
    evaluator_user_id   uuid        NOT NULL,
    sheet_id            uuid        NOT NULL,
    scores_json         jsonb       NOT NULL DEFAULT '{}',
    overall_score       numeric(5, 2),
    -- recommendation: 'strong_yes'|'yes'|'neutral'|'no'|'strong_no'
    recommendation      text        NOT NULL DEFAULT 'neutral',
    -- comment: selection-sensitive free text. Gated read + audited.
    comment             text        NOT NULL DEFAULT '',
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_interview_evaluations PRIMARY KEY (id),
    CONSTRAINT chk_interview_evaluations_recommendation
        CHECK (recommendation IN ('strong_yes', 'yes', 'neutral', 'no', 'strong_no')),
    -- [Security] composite FK: (interview_id, tenant_id) must exist in interviews.
    CONSTRAINT fk_interview_evaluations_interview_tenant
        FOREIGN KEY (interview_id, tenant_id)
        REFERENCES interviews(id, tenant_id)
        MATCH SIMPLE,
    -- [Security] composite FK: (sheet_id, tenant_id) must exist in evaluation_sheets.
    CONSTRAINT fk_interview_evaluations_sheet_tenant
        FOREIGN KEY (sheet_id, tenant_id)
        REFERENCES evaluation_sheets(id, tenant_id)
        MATCH SIMPLE,
    -- One evaluation per panelist per interview.
    CONSTRAINT uq_interview_evaluations_unique
        UNIQUE (tenant_id, interview_id, evaluator_user_id),
    CONSTRAINT uq_interview_evaluations_id_tenant UNIQUE (id, tenant_id)
);

-- Index supports aggregation by application (avoids N+1 when rolling up scores).
CREATE INDEX idx_interview_evaluations_application
    ON interview_evaluations (tenant_id, application_id);
CREATE INDEX idx_interview_evaluations_interview
    ON interview_evaluations (tenant_id, interview_id);

-- ---------------------------------------------------------------------------
-- tenant_interview_settings (テナント面接設定)
-- ---------------------------------------------------------------------------
-- Per-tenant toggle for whether panelists may view each other's evaluations
-- (independent-evaluation bias control).  When peer_eval_visible=false the
-- service masks other evaluators' comments/scores on reads.
-- One row per tenant.
CREATE TABLE tenant_interview_settings (
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    -- peer_eval_visible: when false, panelists cannot see other panelists'
    -- evaluations (default false = independent evaluation, bias-safe).
    peer_eval_visible   boolean     NOT NULL DEFAULT false,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_tenant_interview_settings PRIMARY KEY (tenant_id)
);

-- ---------------------------------------------------------------------------
-- RLS — all new tables
-- ---------------------------------------------------------------------------
ALTER TABLE interviews                ENABLE ROW LEVEL SECURITY;
ALTER TABLE interviews                FORCE  ROW LEVEL SECURITY;
ALTER TABLE interview_slots           ENABLE ROW LEVEL SECURITY;
ALTER TABLE interview_slots           FORCE  ROW LEVEL SECURITY;
ALTER TABLE interview_panelists       ENABLE ROW LEVEL SECURITY;
ALTER TABLE interview_panelists       FORCE  ROW LEVEL SECURITY;
ALTER TABLE evaluation_sheets         ENABLE ROW LEVEL SECURITY;
ALTER TABLE evaluation_sheets         FORCE  ROW LEVEL SECURITY;
ALTER TABLE interview_evaluations     ENABLE ROW LEVEL SECURITY;
ALTER TABLE interview_evaluations     FORCE  ROW LEVEL SECURITY;
ALTER TABLE tenant_interview_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_interview_settings FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON interviews
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON interview_slots
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON interview_panelists
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON evaluation_sheets
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON interview_evaluations
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON tenant_interview_settings
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to hr_app
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE interviews                TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE interview_slots           TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE interview_panelists       TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE evaluation_sheets         TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE interview_evaluations     TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE tenant_interview_settings TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE tenant_interview_settings FROM hr_app;
REVOKE ALL ON TABLE interview_evaluations     FROM hr_app;
REVOKE ALL ON TABLE evaluation_sheets         FROM hr_app;
REVOKE ALL ON TABLE interview_panelists       FROM hr_app;
REVOKE ALL ON TABLE interview_slots           FROM hr_app;
REVOKE ALL ON TABLE interviews                FROM hr_app;

DROP TABLE IF EXISTS tenant_interview_settings;
DROP TABLE IF EXISTS interview_evaluations;
DROP TABLE IF EXISTS evaluation_sheets;
DROP TABLE IF EXISTS interview_panelists;
DROP TABLE IF EXISTS interview_slots;
DROP TABLE IF EXISTS interviews;

-- +goose StatementEnd
