-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- ST-ATS-06  Onboarding linkage (candidate → employee master generation,
--            new-hire onboarding, preboarding IT requests, post-hire surveys)
-- ===========================================================================
-- Bridges the ATS (採用) domain and the LM (労務) domain.  An offer-accepted
-- candidate (内定者) is converted into an employees row, and the existing
-- onboarding assets (onboarding_checklist_templates / onboarding_tasks, see
-- 00008) are reused to generate the actual onboarding tasks in the SAME
-- transaction (孤児防止 / single-tx atomicity).
--
-- Cross-story reference policy (per migration conventions):
--   - References to EXISTING stable tables (employees, departments,
--     onboarding_checklist_templates) use composite FKs (x_id, tenant_id).
--   - References to OTHER new-story tables (applicants — ST-ATS-02,
--     offers — ST-ATS-05) are bare uuid columns + index ONLY (no FK), because
--     those tables live in independently-built slices.  The logical referent is
--     documented in column comments.
--
-- Legal/config note: the mapping of candidate PII (採用選考目的) to employee
-- data (雇用管理目的) must be reconciled with consent/retention policy
-- (ST-ATS-02, CMP-004).  Early-attrition / people-analytics processing
-- (ATS-023, CMP-005) is intentionally left as a minimal scheduling stub here.
-- 法令値・保持年限・利用目的の整合は最新の官公庁情報および社労士/弁護士確認が
-- 前提であり、設定テーブル/設定で改正に追従すること。本実装は法的助言ではない。

-- ---------------------------------------------------------------------------
-- applicant_employee_links (候補者→従業員 変換トレース / 冪等性保証)
-- ---------------------------------------------------------------------------
-- Records the provenance of a candidate→employee conversion and enforces
-- idempotency so the same candidate can never generate two employees.
--   - applicant_id: logical reference to applicants(id) (ST-ATS-02) — bare uuid
--     + index, NO FK (other-story table).
--   - offer_id:     logical reference to offers(id) (ST-ATS-05) — bare uuid,
--     nullable, NO FK (other-story table).
--   - employee_id:  composite FK to employees (existing stable table).
--   - converted_by: (user_id, tenant_id) — bare uuid (users is referenced via
--     a tenant-scoped column; we do not FK to users here to keep the link row
--     insertable even if the actor user is later removed; tenant_id still scopes
--     it under RLS).
CREATE TABLE applicant_employee_links (
    id           uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id    uuid        NOT NULL REFERENCES tenants(id),
    -- applicant_id: logical reference to applicants(id) (ST-ATS-02). No FK.
    applicant_id uuid        NOT NULL,
    -- offer_id: logical reference to offers(id) (ST-ATS-05). No FK. Nullable.
    offer_id     uuid,
    employee_id  uuid        NOT NULL,
    converted_at timestamptz NOT NULL DEFAULT now(),
    -- converted_by: user who performed the conversion. Tenant-scoped uuid.
    converted_by uuid,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_applicant_employee_links PRIMARY KEY (id),
    -- [Idempotency] One employee per applicant per tenant: prevents double
    -- conversion of the same candidate.
    CONSTRAINT uq_applicant_employee_links_applicant
        UNIQUE (tenant_id, applicant_id),
    -- [Idempotency] One source applicant per generated employee per tenant.
    CONSTRAINT uq_applicant_employee_links_employee
        UNIQUE (tenant_id, employee_id),
    -- [Security] Composite FK: (employee_id, tenant_id) must exist in employees.
    CONSTRAINT fk_applicant_employee_links_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_applicant_employee_links_id_tenant UNIQUE (id, tenant_id)
);

-- Index supporting applicant-based lookups (logical FK to applicants).
CREATE INDEX idx_applicant_employee_links_applicant
    ON applicant_employee_links (tenant_id, applicant_id);
CREATE INDEX idx_applicant_employee_links_employee
    ON applicant_employee_links (tenant_id, employee_id);

-- ---------------------------------------------------------------------------
-- new_hire_onboardings (内定者オンボーディング・ヘッダ ATS-020 / ATS-021)
-- ---------------------------------------------------------------------------
-- Header tracking a new hire's onboarding lifecycle.  Actual tasks are the
-- existing onboarding_tasks rows (generated in the same conversion tx).
--   status: 'offer_accepted' → 'preboarding' → 'onboarding' → 'completed'.
--   department_id: composite FK to departments (existing table) for per-dept
--                  template selection.
--   template_id:   composite FK to onboarding_checklist_templates (existing
--                  table, reused asset).  Nullable (no template ⇒ no auto-tasks).
--   employee_id:   composite FK to employees.
--   applicant_id:  logical reference to applicants(id) (ST-ATS-02). No FK.
CREATE TABLE new_hire_onboardings (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    employee_id         uuid        NOT NULL,
    -- applicant_id: logical reference to applicants(id) (ST-ATS-02). No FK.
    applicant_id        uuid        NOT NULL,
    -- department_id: per-department template selection. Composite FK, nullable.
    department_id       uuid,
    -- template_id: reuse of onboarding_checklist_templates. Composite FK, nullable.
    template_id         uuid,
    -- status: 'offer_accepted' | 'preboarding' | 'onboarding' | 'completed'
    status              text        NOT NULL DEFAULT 'offer_accepted',
    expected_start_date date,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_new_hire_onboardings PRIMARY KEY (id),
    CONSTRAINT chk_new_hire_onboardings_status
        CHECK (status IN ('offer_accepted', 'preboarding', 'onboarding', 'completed')),
    -- One onboarding header per employee per tenant.
    CONSTRAINT uq_new_hire_onboardings_employee UNIQUE (tenant_id, employee_id),
    -- [Security] Composite FK: (employee_id, tenant_id) must exist in employees.
    CONSTRAINT fk_new_hire_onboardings_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- [Security] Composite FK: (department_id, tenant_id) must exist in
    -- departments when department_id is non-NULL (MATCH SIMPLE skips NULL).
    CONSTRAINT fk_new_hire_onboardings_department_tenant
        FOREIGN KEY (department_id, tenant_id)
        REFERENCES departments(id, tenant_id)
        MATCH SIMPLE,
    -- [Security] Composite FK: (template_id, tenant_id) must exist in
    -- onboarding_checklist_templates (reused asset) when non-NULL.
    CONSTRAINT fk_new_hire_onboardings_template_tenant
        FOREIGN KEY (template_id, tenant_id)
        REFERENCES onboarding_checklist_templates(id, tenant_id)
        MATCH SIMPLE,
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_new_hire_onboardings_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_new_hire_onboardings_lookup
    ON new_hire_onboardings (tenant_id, status);
CREATE INDEX idx_new_hire_onboardings_applicant
    ON new_hire_onboardings (tenant_id, applicant_id);

-- ---------------------------------------------------------------------------
-- preboarding_requests (入社前のIT手続き等の依頼 ATS-022)
-- ---------------------------------------------------------------------------
-- Pre-start requests (account issuance, equipment, access, etc.).
--   request_type: 'account' | 'equipment' | 'access' | 'other'
--   status:       'requested' | 'in_progress' | 'completed' | 'cancelled'
--   new_hire_onboarding_id: composite FK to new_hire_onboardings (own table).
--   assignee_user_id: tenant-scoped uuid (user responsible). Nullable, no FK
--                     (kept insertable even if the user is later removed; RLS
--                     still scopes by tenant_id).
CREATE TABLE preboarding_requests (
    id                     uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id              uuid        NOT NULL REFERENCES tenants(id),
    new_hire_onboarding_id uuid        NOT NULL,
    -- request_type: 'account' | 'equipment' | 'access' | 'other'
    request_type           text        NOT NULL,
    -- status: 'requested' | 'in_progress' | 'completed' | 'cancelled'
    status                 text        NOT NULL DEFAULT 'requested',
    assignee_user_id       uuid,
    notes                  text,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_preboarding_requests PRIMARY KEY (id),
    CONSTRAINT chk_preboarding_requests_type
        CHECK (request_type IN ('account', 'equipment', 'access', 'other')),
    CONSTRAINT chk_preboarding_requests_status
        CHECK (status IN ('requested', 'in_progress', 'completed', 'cancelled')),
    -- [Security] Composite FK to own table new_hire_onboardings.
    CONSTRAINT fk_preboarding_requests_onboarding_tenant
        FOREIGN KEY (new_hire_onboarding_id, tenant_id)
        REFERENCES new_hire_onboardings(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_preboarding_requests_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_preboarding_requests_lookup
    ON preboarding_requests (tenant_id, new_hire_onboarding_id, status);

-- ---------------------------------------------------------------------------
-- onboarding_surveys (入社後フォローサーベイ / 早期離職アラート ATS-023, Could)
-- ---------------------------------------------------------------------------
-- MVP/extension point ONLY: a schedule slot + status.  The survey answer body
-- and early-attrition predictive analytics are deliberately Future work — they
-- touch people-analytics principles (CMP-005) and require a separate consent /
-- governance design.  No answer payload or PII is stored here.
--   survey_type:  'onboarding_30d' | 'onboarding_90d' | 'early_attrition'
--   status:       'scheduled' | 'sent' | 'responded' | 'cancelled'
--   new_hire_onboarding_id: composite FK to new_hire_onboardings (own table).
--   employee_id:  composite FK to employees.
CREATE TABLE onboarding_surveys (
    id                     uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id              uuid        NOT NULL REFERENCES tenants(id),
    new_hire_onboarding_id uuid        NOT NULL,
    employee_id            uuid        NOT NULL,
    -- survey_type: 'onboarding_30d' | 'onboarding_90d' | 'early_attrition'
    survey_type            text        NOT NULL,
    scheduled_on           date,
    -- status: 'scheduled' | 'sent' | 'responded' | 'cancelled'
    status                 text        NOT NULL DEFAULT 'scheduled',
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_onboarding_surveys PRIMARY KEY (id),
    CONSTRAINT chk_onboarding_surveys_type
        CHECK (survey_type IN ('onboarding_30d', 'onboarding_90d', 'early_attrition')),
    CONSTRAINT chk_onboarding_surveys_status
        CHECK (status IN ('scheduled', 'sent', 'responded', 'cancelled')),
    -- [Security] Composite FK to own table new_hire_onboardings.
    CONSTRAINT fk_onboarding_surveys_onboarding_tenant
        FOREIGN KEY (new_hire_onboarding_id, tenant_id)
        REFERENCES new_hire_onboardings(id, tenant_id)
        MATCH SIMPLE,
    -- [Security] Composite FK: (employee_id, tenant_id) must exist in employees.
    CONSTRAINT fk_onboarding_surveys_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_onboarding_surveys_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_onboarding_surveys_lookup
    ON onboarding_surveys (tenant_id, new_hire_onboarding_id, status);

-- ---------------------------------------------------------------------------
-- RLS — all new tables
-- ---------------------------------------------------------------------------
ALTER TABLE applicant_employee_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE applicant_employee_links FORCE  ROW LEVEL SECURITY;
ALTER TABLE new_hire_onboardings     ENABLE ROW LEVEL SECURITY;
ALTER TABLE new_hire_onboardings     FORCE  ROW LEVEL SECURITY;
ALTER TABLE preboarding_requests     ENABLE ROW LEVEL SECURITY;
ALTER TABLE preboarding_requests     FORCE  ROW LEVEL SECURITY;
ALTER TABLE onboarding_surveys       ENABLE ROW LEVEL SECURITY;
ALTER TABLE onboarding_surveys       FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON applicant_employee_links
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON new_hire_onboardings
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON preboarding_requests
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON onboarding_surveys
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to hr_app
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE applicant_employee_links TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE new_hire_onboardings     TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE preboarding_requests     TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE onboarding_surveys       TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE onboarding_surveys       FROM hr_app;
REVOKE ALL ON TABLE preboarding_requests     FROM hr_app;
REVOKE ALL ON TABLE new_hire_onboardings     FROM hr_app;
REVOKE ALL ON TABLE applicant_employee_links FROM hr_app;

DROP TABLE IF EXISTS onboarding_surveys;
DROP TABLE IF EXISTS preboarding_requests;
DROP TABLE IF EXISTS new_hire_onboardings;
DROP TABLE IF EXISTS applicant_employee_links;

-- +goose StatementEnd
