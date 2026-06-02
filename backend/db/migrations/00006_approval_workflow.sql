-- +goose Up
-- +goose StatementBegin

-- ---------------------------------------------------------------------------
-- approval_routes (承認経路設定)
-- ---------------------------------------------------------------------------
-- One row per approval-route definition. A route specifies who approves for a
-- given request_type, optionally scoped to a department. The steps_json column
-- encodes the ordered approval steps as a JSONB array:
--   [
--     {"step": 0, "role": "manager", "user_id": null},
--     {"step": 1, "role": null,      "user_id": "<uuid>"}
--   ]
-- Either role or user_id must be set per step; both may be set (role is the
-- primary approver, user_id overrides if present). Interpretation is in the
-- application layer.
CREATE TABLE approval_routes (
    id            uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES tenants(id),
    request_type  text        NOT NULL,
    -- department_id: nullable — when set, route applies only to requests from
    -- that department; when null, route is the tenant-wide fallback.
    department_id uuid,
    name          text        NOT NULL,
    steps_json    jsonb       NOT NULL DEFAULT '[]',
    active        bool        NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_approval_routes PRIMARY KEY (id),
    -- Composite UNIQUE(id, tenant_id) required so that approval_requests can
    -- reference (route_id, tenant_id) as a composite FK (FK checks bypass RLS).
    CONSTRAINT uq_approval_routes_id_tenant UNIQUE (id, tenant_id),
    -- [Security] Composite FK: (department_id, tenant_id) must exist in
    -- departments when department_id is not NULL.
    CONSTRAINT fk_approval_routes_department_tenant
        FOREIGN KEY (department_id, tenant_id)
        REFERENCES departments(id, tenant_id)
        MATCH SIMPLE
);

CREATE INDEX idx_approval_routes_lookup
    ON approval_routes (tenant_id, request_type, active);

-- ---------------------------------------------------------------------------
-- approval_requests (申請)
-- ---------------------------------------------------------------------------
-- One row per submitted approval request. subject_ref is an opaque ID string
-- that identifies the business object being approved (e.g. a leave-request UUID,
-- a contract UUID). PII must never be stored here — use subject_ref to point
-- to the canonical record.
CREATE TABLE approval_requests (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    request_type        text        NOT NULL,
    -- subject_ref: opaque reference to the target resource (UUID string only,
    -- no PII).  May be empty when the approval request IS the resource.
    subject_ref         text        NOT NULL DEFAULT '',
    requested_by_user_id uuid       NOT NULL,
    route_id            uuid        NOT NULL,
    current_step        int         NOT NULL DEFAULT 0,
    -- status: pending → approved | rejected | returned | cancelled
    status              text        NOT NULL DEFAULT 'pending',
    -- payload_json: request payload (reference IDs only, no PII).
    payload_json        jsonb       NOT NULL DEFAULT '{}',
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_approval_requests PRIMARY KEY (id),
    -- Composite UNIQUE(id, tenant_id) required so that approval_steps can
    -- reference (request_id, tenant_id) as a composite FK.
    CONSTRAINT uq_approval_requests_id_tenant UNIQUE (id, tenant_id),
    -- [Security] Composite FK: (route_id, tenant_id) must exist in
    -- approval_routes, preventing cross-tenant route references.
    CONSTRAINT fk_approval_requests_route_tenant
        FOREIGN KEY (route_id, tenant_id)
        REFERENCES approval_routes(id, tenant_id)
        MATCH SIMPLE
);

CREATE INDEX idx_approval_requests_lookup
    ON approval_requests (tenant_id, requested_by_user_id, status);

CREATE INDEX idx_approval_requests_route
    ON approval_requests (tenant_id, route_id);

-- ---------------------------------------------------------------------------
-- approval_steps (各段の状態 / 決定履歴)
-- ---------------------------------------------------------------------------
-- One row per step per request. Inserted when a request is submitted (all
-- steps up-front). Rows are updated as decisions are made.
-- decided_by_user_id: the actual user who made the decision (may be
-- delegate_user_id when acting as a delegate).
CREATE TABLE approval_steps (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    request_id          uuid        NOT NULL,
    step_index          int         NOT NULL,
    -- approver_user_id: the directly-assigned approver for this step (nullable
    -- when resolved by role at decision time).
    approver_user_id    uuid,
    -- delegate_user_id: a user authorised to decide on behalf of the approver.
    delegate_user_id    uuid,
    -- decision: pending | approved | rejected | returned
    decision            text        NOT NULL DEFAULT 'pending',
    -- decided_by_user_id: the user who actually submitted the decision.
    decided_by_user_id  uuid,
    comment             text,
    decided_at          timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_approval_steps PRIMARY KEY (id),
    -- [Security] Composite FK: (request_id, tenant_id) must exist in
    -- approval_requests, preventing cross-tenant step references.
    CONSTRAINT fk_approval_steps_request_tenant
        FOREIGN KEY (request_id, tenant_id)
        REFERENCES approval_requests(id, tenant_id)
        MATCH SIMPLE,
    -- Enforce unique step index per request.
    CONSTRAINT uq_approval_steps_request_step UNIQUE (request_id, step_index)
);

CREATE INDEX idx_approval_steps_lookup
    ON approval_steps (tenant_id, request_id, step_index);

-- ---------------------------------------------------------------------------
-- RLS — all approval tables
-- ---------------------------------------------------------------------------
ALTER TABLE approval_routes   ENABLE ROW LEVEL SECURITY;
ALTER TABLE approval_routes   FORCE  ROW LEVEL SECURITY;
ALTER TABLE approval_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE approval_requests FORCE  ROW LEVEL SECURITY;
ALTER TABLE approval_steps    ENABLE ROW LEVEL SECURITY;
ALTER TABLE approval_steps    FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON approval_routes
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON approval_requests
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON approval_steps
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to hr_app
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE approval_routes   TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE approval_requests TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE approval_steps    TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE approval_steps    FROM hr_app;
REVOKE ALL ON TABLE approval_requests FROM hr_app;
REVOKE ALL ON TABLE approval_routes   FROM hr_app;

DROP TABLE IF EXISTS approval_steps;
DROP TABLE IF EXISTS approval_requests;
DROP TABLE IF EXISTS approval_routes;

-- +goose StatementEnd
