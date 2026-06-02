-- +goose Up
-- +goose StatementBegin

-- ---------------------------------------------------------------------------
-- tenants: add slug for login-time tenant identification
-- ---------------------------------------------------------------------------
ALTER TABLE tenants
    ADD COLUMN slug text NOT NULL DEFAULT '';

-- Remove the temporary default — new rows must supply a slug explicitly.
ALTER TABLE tenants
    ALTER COLUMN slug DROP DEFAULT;

CREATE UNIQUE INDEX uq_tenants_slug ON tenants (slug);

-- ---------------------------------------------------------------------------
-- users: add authentication columns
-- ---------------------------------------------------------------------------
ALTER TABLE users
    ADD COLUMN failed_login_count int         NOT NULL DEFAULT 0,
    ADD COLUMN locked_until        timestamptz          NULL,
    ADD COLUMN last_login_at       timestamptz          NULL,
    ADD COLUMN mfa_enabled         bool        NOT NULL DEFAULT false;

-- ---------------------------------------------------------------------------
-- sessions: new table (token stored as SHA-256 hash only)
-- ---------------------------------------------------------------------------
CREATE TABLE sessions (
    id            uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES tenants(id),
    user_id       uuid        NOT NULL REFERENCES users(id),
    token_hash    text        NOT NULL,
    expires_at    timestamptz NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz          NULL,
    revoked_at    timestamptz          NULL,
    ip            inet                 NULL,
    CONSTRAINT pk_sessions PRIMARY KEY (id),
    CONSTRAINT uq_sessions_token_hash UNIQUE (token_hash)
);

CREATE INDEX idx_sessions_token_hash  ON sessions (token_hash);
CREATE INDEX idx_sessions_expires_at  ON sessions (expires_at);
CREATE INDEX idx_sessions_tenant_user ON sessions (tenant_id, user_id);

-- RLS: sessions follows the same pattern as every other tenant-scoped table.
ALTER TABLE sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE sessions FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON sessions
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE sessions TO hr_app;

-- ---------------------------------------------------------------------------
-- audit_logs: tamper-detection hash chain columns (values computed in P1.3b)
-- ---------------------------------------------------------------------------
ALTER TABLE audit_logs
    ADD COLUMN prev_hash text NULL,
    ADD COLUMN hash      text NULL;

-- ---------------------------------------------------------------------------
-- SECURITY DEFINER functions for pre-authentication lookups
--
-- These two functions are the ONLY entry points that can cross the RLS
-- boundary without a tenant context.  They are intentionally minimal:
--   - no string concatenation (all values via $N parameters)
--   - SECURITY DEFINER with a fixed search_path to prevent search-path injection
--   - STABLE (no side-effects; results may be cached within a statement)
--   - REVOKE EXECUTE FROM PUBLIC; GRANT EXECUTE TO hr_app — least privilege
--
-- They are required during login, when the tenant is not yet known and
-- WithinTenant() has not been called.
-- ---------------------------------------------------------------------------

-- auth_resolve_tenant_by_slug
-- Returns the tenant UUID for a given slug, or NULL when not found.
CREATE OR REPLACE FUNCTION auth_resolve_tenant_by_slug(p_slug text)
RETURNS uuid
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
    SELECT id
    FROM   public.tenants
    WHERE  slug = $1
    LIMIT  1;
$$;

REVOKE EXECUTE ON FUNCTION auth_resolve_tenant_by_slug(text) FROM PUBLIC;
GRANT  EXECUTE ON FUNCTION auth_resolve_tenant_by_slug(text) TO   hr_app;

-- auth_resolve_session
-- Returns (tenant_id, user_id, expires_at, revoked_at) for a token hash.
-- The caller decides whether the session is live (expires_at > now, revoked_at IS NULL).
-- VOLATILE (not STABLE): sessions rows are mutable — revoked_at and expires_at can
-- change between calls within the same statement, so PostgreSQL must not cache results.
CREATE OR REPLACE FUNCTION auth_resolve_session(p_token_hash text)
RETURNS TABLE (
    tenant_id  uuid,
    user_id    uuid,
    expires_at timestamptz,
    revoked_at timestamptz
)
LANGUAGE sql
VOLATILE
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
    SELECT tenant_id,
           user_id,
           expires_at,
           revoked_at
    FROM   public.sessions
    WHERE  token_hash = $1
    LIMIT  1;
$$;

REVOKE EXECUTE ON FUNCTION auth_resolve_session(text) FROM PUBLIC;
GRANT  EXECUTE ON FUNCTION auth_resolve_session(text) TO   hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP FUNCTION IF EXISTS auth_resolve_session(text);
DROP FUNCTION IF EXISTS auth_resolve_tenant_by_slug(text);

ALTER TABLE audit_logs
    DROP COLUMN IF EXISTS prev_hash,
    DROP COLUMN IF EXISTS hash;

REVOKE ALL ON TABLE sessions FROM hr_app;
DROP TABLE  IF EXISTS sessions;

ALTER TABLE users
    DROP COLUMN IF EXISTS failed_login_count,
    DROP COLUMN IF EXISTS locked_until,
    DROP COLUMN IF EXISTS last_login_at,
    DROP COLUMN IF EXISTS mfa_enabled;

DROP INDEX  IF EXISTS uq_tenants_slug;
ALTER TABLE tenants DROP COLUMN IF EXISTS slug;

-- +goose StatementEnd
