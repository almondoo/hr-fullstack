-- +goose Up
-- +goose StatementBegin

-- ---------------------------------------------------------------------------
-- audit_logs: add monotonic sequence for hash chain ordering
-- ---------------------------------------------------------------------------
-- A bigserial seq column provides a stable, monotonic ordering for hash chain
-- computation regardless of concurrent INSERTs (advisory lock serialises
-- within a tenant, but the seq ensures correct ordering across restarts).
ALTER TABLE audit_logs
    ADD COLUMN seq bigserial NOT NULL;

-- resource_id in audit_logs was uuid; change to text to allow any string ID.
-- (existing uuid values cast cleanly; new code stores strings directly)
ALTER TABLE audit_logs
    ALTER COLUMN resource_id TYPE text USING resource_id::text;

-- ---------------------------------------------------------------------------
-- users: add role_id FK to roles table
-- ---------------------------------------------------------------------------
ALTER TABLE users
    ADD COLUMN role_id uuid NULL REFERENCES roles(id);

CREATE INDEX idx_users_role_id ON users (role_id);

-- ---------------------------------------------------------------------------
-- Grant new sequence to hr_app
-- ---------------------------------------------------------------------------
-- bigserial creates a sequence; hr_app needs USAGE on it.
GRANT USAGE, SELECT ON SEQUENCE audit_logs_seq_seq TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE USAGE, SELECT ON SEQUENCE audit_logs_seq_seq FROM hr_app;

DROP INDEX IF EXISTS idx_users_role_id;

ALTER TABLE users
    DROP COLUMN IF EXISTS role_id;

ALTER TABLE audit_logs
    ALTER COLUMN resource_id TYPE uuid USING resource_id::uuid;

ALTER TABLE audit_logs
    DROP COLUMN IF EXISTS seq;

-- +goose StatementEnd
