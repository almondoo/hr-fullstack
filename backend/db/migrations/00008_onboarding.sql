-- +goose Up
-- +goose StatementBegin

-- ---------------------------------------------------------------------------
-- onboarding_checklist_templates (入退社チェックリストテンプレート)
-- ---------------------------------------------------------------------------
-- Stores per-tenant templates that generate onboarding_tasks in bulk.
-- items_json: JSON array of task item descriptors.
-- Format: [{"title":"...", "category":"...", "due_offset_days": 0}]
CREATE TABLE onboarding_checklist_templates (
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES tenants(id),
    name        text        NOT NULL,
    -- kind: "onboarding" | "offboarding"
    kind        text        NOT NULL,
    items_json  jsonb       NOT NULL DEFAULT '[]',
    active      boolean     NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_onboarding_checklist_templates PRIMARY KEY (id),
    CONSTRAINT chk_onboarding_checklist_templates_kind
        CHECK (kind IN ('onboarding', 'offboarding')),
    -- UNIQUE(id, tenant_id) required for downstream composite FK references
    CONSTRAINT uq_onboarding_checklist_templates_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_onboarding_checklist_templates_lookup
    ON onboarding_checklist_templates (tenant_id, kind, active);

-- ---------------------------------------------------------------------------
-- onboarding_tasks (入退社タスク LM-001 / LM-004)
-- ---------------------------------------------------------------------------
-- Represents an individual task in an onboarding or offboarding checklist.
-- kind:     "onboarding" | "offboarding"
-- status:   "pending" | "in_progress" | "done" | "skipped"
-- assignee_user_id: the user responsible for completing this task (nullable)
-- completed_at: set when status transitions to "done"
CREATE TABLE onboarding_tasks (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    employee_id         uuid        NOT NULL,
    -- kind: "onboarding" | "offboarding"
    kind                text        NOT NULL,
    title               text        NOT NULL,
    category            text        NOT NULL DEFAULT '',
    -- status: "pending" | "in_progress" | "done" | "skipped"
    status              text        NOT NULL DEFAULT 'pending',
    due_date            date,
    assignee_user_id    uuid,
    completed_at        timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_onboarding_tasks PRIMARY KEY (id),
    CONSTRAINT chk_onboarding_tasks_kind
        CHECK (kind IN ('onboarding', 'offboarding')),
    CONSTRAINT chk_onboarding_tasks_status
        CHECK (status IN ('pending', 'in_progress', 'done', 'skipped')),
    -- [Security] Composite FK: (employee_id, tenant_id) must exist in employees.
    -- Prevents cross-tenant task creation.
    CONSTRAINT fk_onboarding_tasks_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- UNIQUE(id, tenant_id) for downstream composite FK references
    CONSTRAINT uq_onboarding_tasks_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_onboarding_tasks_lookup
    ON onboarding_tasks (tenant_id, employee_id, kind, status);

-- ---------------------------------------------------------------------------
-- employee_intake_forms (入社者情報収集フォーム LM-003)
-- ---------------------------------------------------------------------------
-- Stores the sensitive PII collected from new hires.
--
-- Sensitive PII storage design:
--   - emergency_contact_json, commute_json, dependents_json: JSONB columns
--     for structured non-financial PII (氏名, 続柄, 住所 etc.).
--     These are classified as non-financial PII; they are stored in JSONB for
--     queryability within the tenant.  Access is controlled via RBAC
--     (intake:read / intake:write) and RLS.
--   - bank_account_enc: BYTEA column storing AES-256-GCM ciphertext of the
--     employee's bank account number (口座番号).  The plaintext value is NEVER
--     stored.  Decryption requires the intake:read_sensitive permission and is
--     performed in the application layer using the field encryption key
--     (FIELD_ENCRYPTION_KEY).  The column type is bytea to prevent accidental
--     indexing or logging of the encrypted value as text.
--
-- status: "submitted" | "verified"
-- One form per employee per tenant (UNIQUE constraint).
CREATE TABLE employee_intake_forms (
    id                      uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id               uuid        NOT NULL REFERENCES tenants(id),
    employee_id             uuid        NOT NULL,
    -- emergency_contact_json: 緊急連絡先 (氏名/関係/電話番号 etc.)
    -- Format: {"name":"合成太郎","relationship":"配偶者","phone":"090-0000-0000"}
    emergency_contact_json  jsonb       NOT NULL DEFAULT '{}',
    -- commute_json: 通勤経路情報 (路線/区間/定期代等)
    -- Format: {"route":"自宅〜最寄駅〜勤務地","monthly_cost":12340}
    commute_json            jsonb       NOT NULL DEFAULT '{}',
    -- dependents_json: 扶養家族情報 (配列)
    -- Format: [{"name":"合成花子","relationship":"子","birth_date":"2015-04-01"}]
    dependents_json         jsonb       NOT NULL DEFAULT '[]',
    -- bank_account_enc: AES-256-GCM ciphertext of the bank account number (口座番号).
    -- Plaintext is NEVER stored in the database.
    -- SECURITY: do NOT add a text column for the account number, even temporarily.
    bank_account_enc        bytea,
    -- status: "submitted" | "verified"
    status                  text        NOT NULL DEFAULT 'submitted',
    -- retention_policy: data retention/deletion policy label (e.g. "7years").
    -- Physical deletion is prohibited; use this label to enforce logical expiry.
    -- See LM-004 offboarding: data is never permanently deleted, only marked expired.
    retention_policy        text        NOT NULL DEFAULT '7years',
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_employee_intake_forms PRIMARY KEY (id),
    CONSTRAINT chk_employee_intake_forms_status
        CHECK (status IN ('submitted', 'verified')),
    -- [Security] Composite FK: (employee_id, tenant_id) must exist in employees.
    CONSTRAINT fk_employee_intake_forms_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- One form per employee per tenant.
    CONSTRAINT uq_employee_intake_forms_employee_tenant UNIQUE (employee_id, tenant_id),
    -- UNIQUE(id, tenant_id) for downstream composite FK references
    CONSTRAINT uq_employee_intake_forms_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_employee_intake_forms_lookup
    ON employee_intake_forms (tenant_id, employee_id, status);

-- ---------------------------------------------------------------------------
-- offboarding_policies (退職データ保持ポリシー LM-004)
-- ---------------------------------------------------------------------------
-- Records the data retention decision made at offboarding time for an employee.
-- Physical deletion is prohibited by policy; this table stores when data may
-- be logically expired (e.g. masked, access-restricted) and by whom the
-- decision was recorded.
--
-- This does NOT delete any rows — it is a record of intent and timeline.
-- A background job (outside this migration) enforces the policy by updating
-- access flags / anonymising non-legally-required fields after expires_on.
CREATE TABLE offboarding_policies (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    employee_id         uuid        NOT NULL,
    -- retention_label: e.g. "7years", "5years", "indefinite"
    retention_label     text        NOT NULL DEFAULT '7years',
    -- expires_on: date after which data may be logically expired / anonymised.
    -- NULL means indefinite retention.
    expires_on          date,
    -- recorded_by: user who recorded this policy decision
    recorded_by         uuid,
    notes               text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_offboarding_policies PRIMARY KEY (id),
    -- [Security] Composite FK: (employee_id, tenant_id) must exist in employees.
    CONSTRAINT fk_offboarding_policies_employee_tenant
        FOREIGN KEY (employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- One policy per employee per tenant (most recent governs).
    CONSTRAINT uq_offboarding_policies_employee_tenant UNIQUE (employee_id, tenant_id),
    CONSTRAINT uq_offboarding_policies_id_tenant UNIQUE (id, tenant_id)
);

-- ---------------------------------------------------------------------------
-- employees table: add "leaving" status support
-- ---------------------------------------------------------------------------
-- The employees.status column gains two new allowed values for the offboarding
-- lifecycle:
--   active   → leaving  (resignation accepted, offboarding in progress)
--   leaving  → left     (offboarding complete, employment ended)
--
-- The CHECK constraint on employees is extended via a separate ALTER TABLE.
-- existing CHECK constraint name: chk_employees_status (defined in 00004)
-- We drop and recreate it to add the new values.
ALTER TABLE employees
    DROP CONSTRAINT IF EXISTS chk_employees_status;

ALTER TABLE employees
    ADD CONSTRAINT chk_employees_status
        CHECK (status IN ('active', 'inactive', 'terminated', 'leaving', 'left'));

-- ---------------------------------------------------------------------------
-- RLS — all new tables
-- ---------------------------------------------------------------------------
ALTER TABLE onboarding_checklist_templates ENABLE ROW LEVEL SECURITY;
ALTER TABLE onboarding_checklist_templates FORCE  ROW LEVEL SECURITY;
ALTER TABLE onboarding_tasks               ENABLE ROW LEVEL SECURITY;
ALTER TABLE onboarding_tasks               FORCE  ROW LEVEL SECURITY;
ALTER TABLE employee_intake_forms          ENABLE ROW LEVEL SECURITY;
ALTER TABLE employee_intake_forms          FORCE  ROW LEVEL SECURITY;
ALTER TABLE offboarding_policies           ENABLE ROW LEVEL SECURITY;
ALTER TABLE offboarding_policies           FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON onboarding_checklist_templates
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON onboarding_tasks
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON employee_intake_forms
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON offboarding_policies
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to hr_app
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE onboarding_checklist_templates TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE onboarding_tasks               TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE employee_intake_forms          TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE offboarding_policies           TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE offboarding_policies           FROM hr_app;
REVOKE ALL ON TABLE employee_intake_forms          FROM hr_app;
REVOKE ALL ON TABLE onboarding_tasks               FROM hr_app;
REVOKE ALL ON TABLE onboarding_checklist_templates FROM hr_app;

DROP TABLE IF EXISTS offboarding_policies;
DROP TABLE IF EXISTS employee_intake_forms;
DROP TABLE IF EXISTS onboarding_tasks;
DROP TABLE IF EXISTS onboarding_checklist_templates;

-- Restore employees.status CHECK to original values (before leaving/left were added).
ALTER TABLE employees DROP CONSTRAINT IF EXISTS chk_employees_status;
ALTER TABLE employees ADD CONSTRAINT chk_employees_status
    CHECK (status IN ('active', 'inactive', 'terminated'));

-- +goose StatementEnd
