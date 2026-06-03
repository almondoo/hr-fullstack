-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- ST-FND-09: Notification platform (in-app + email), templates, delivery
-- records, read management, reminders.
--
-- LEGAL / CONFIG NOTE:
--   - Notification/delivery retention periods (notifications, email_deliveries
--     log retention years) are configuration values, NOT hard-coded here.
--     They must be sourced from a settings table and aligned with the
--     退職者データ削除ポリシー (NFR-011).  The DEFAULT values present on some
--     columns (e.g. max_attempts) are operational placeholders that MUST be
--     made configurable per tenant and reviewed.
--   - Email deliverability (SPF/DKIM/DMARC) and bounce handling are operational
--     settings of the real sending backend (SES/SendGrid).  The MVP uses a mock
--     MailSender.  These are NOT legal/tax constants; the design keeps them
--     swappable via configuration (CORE-017 / INT-012).
--   - The opt-out-prohibited "forced" notification scope (security/legal) is a
--     要検討事項 to be configured; consent/delivery rules under 特商法 /
--     個人情報保護法 require expert (社労士/弁護士) confirmation.
--   This migration encodes structure only.  Values are configurable; this is
--   not legal advice and must follow the latest official guidance with expert
--   review to track regulatory changes.
-- ===========================================================================

-- ---------------------------------------------------------------------------
-- notification_templates (テナント別通知テンプレート)
-- ---------------------------------------------------------------------------
-- Per-tenant subject/body templates resolved by (event_type, channel, locale).
-- When no tenant template exists, the application falls back to a built-in
-- system default template (in code).
-- Body templates contain placeholders for opaque IDs / non-sensitive display
-- values only.  PII (マイナンバー/口座/健診 etc.) MUST NOT appear in templates;
-- detail is referenced via an authenticated in-app deep link (opaque ID).
CREATE TABLE notification_templates (
    id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES tenants(id),
    -- event_type: e.g. "approval.pending", "leave.approved", "billing.payment_failed".
    event_type      text        NOT NULL,
    -- channel: "in_app" | "email"
    channel         text        NOT NULL,
    -- locale: BCP-47-ish locale tag, e.g. "ja", "en".
    locale          text        NOT NULL DEFAULT 'ja',
    subject_template text       NOT NULL DEFAULT '',
    body_template   text        NOT NULL DEFAULT '',
    active          boolean     NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_notification_templates PRIMARY KEY (id),
    CONSTRAINT chk_notification_templates_channel
        CHECK (channel IN ('in_app', 'email')),
    -- One template per (tenant, event_type, channel, locale).
    CONSTRAINT uq_notification_templates_lookup
        UNIQUE (tenant_id, event_type, channel, locale),
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_notification_templates_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_notification_templates_lookup
    ON notification_templates (tenant_id, event_type, channel, locale, active);

-- ---------------------------------------------------------------------------
-- notifications (通知の論理エンティティ / 発行単位)
-- ---------------------------------------------------------------------------
-- One row per (recipient, event) issuance.  recipient_user_id is the target
-- user; only that user may read / mark-read their own notifications (enforced
-- at the service layer via recipient_user_id == authenticated user).
--
-- recipient_user_id: PLAIN uuid (logical reference to users.id).  No composite
--   FK is declared because the existing users table has no UNIQUE(id, tenant_id)
--   constraint (00001) and this migration must not alter existing tables.
--   Tenant membership of the recipient is verified at the service layer with
--   SELECT COUNT(1) FROM users WHERE id=? AND tenant_id=?.  RLS still isolates
--   rows by tenant_id.  This mirrors onboarding_tasks.assignee_user_id.
--
-- resource_type / resource_id: opaque deep-link target (UUID).  body_ref holds
--   an opaque reference string (deep-link path / token) — NEVER sensitive PII.
-- dedupe_key: reminder/idempotency suppression key.  Issuance-level dedupe (one
--   logical reminder = one Publish) is enforced in the app layer; the DB unique
--   index is scoped per (tenant, recipient, channel) so the per-channel rows of a
--   single fan-out can share the key without colliding (see uq_notifications_dedupe).
CREATE TABLE notifications (
    id                uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id         uuid        NOT NULL REFERENCES tenants(id),
    -- recipient_user_id: logical reference to users.id (verified in service layer).
    recipient_user_id uuid        NOT NULL,
    event_type        text        NOT NULL,
    -- channel: "in_app" | "email"
    channel           text        NOT NULL,
    subject           text        NOT NULL DEFAULT '',
    -- body_ref: opaque deep-link reference / rendered non-sensitive snippet.
    -- SECURITY: MUST NOT contain マイナンバー/口座/健診 or other sensitive PII.
    body_ref          text        NOT NULL DEFAULT '',
    resource_type     text        NOT NULL DEFAULT '',
    -- resource_id: opaque UUID of the linked business resource (deep link target).
    resource_id       uuid,
    -- dedupe_key: reminder dedupe / idempotency key (optional).
    dedupe_key        text,
    -- status: lifecycle of the in-app notification logical entity.
    --   "created"   — issued
    --   "delivered" — surfaced to the recipient (in-app list)
    --   "cancelled" — superseded / withdrawn
    status            text        NOT NULL DEFAULT 'created',
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_notifications PRIMARY KEY (id),
    CONSTRAINT chk_notifications_channel
        CHECK (channel IN ('in_app', 'email')),
    CONSTRAINT chk_notifications_status
        CHECK (status IN ('created', 'delivered', 'cancelled')),
    -- UNIQUE(id, tenant_id) for downstream composite FK references.
    CONSTRAINT uq_notifications_id_tenant UNIQUE (id, tenant_id)
);

-- Partial unique index: guard against a TRUE duplicate notification row sharing
-- a dedupe_key — i.e. the same (recipient, channel) being issued twice under one
-- key.  The key is scoped by (recipient_user_id, channel) on purpose: a single
-- Publish fans one event out to one row PER channel PER recipient, and every such
-- row legitimately carries the SAME dedupe_key.  A tenant-wide (tenant_id,
-- dedupe_key) uniqueness would make the 2nd row of any multi-channel /
-- multi-recipient Publish collide and roll back the whole transaction.
--
-- Issuance-level dedupe (suppressing a *repeat* Publish of the same reminder) is
-- enforced at the application layer via a COUNT(1) pre-check on (tenant_id,
-- dedupe_key); this index is only the DB-level safety net for an exact
-- (tenant, recipient, channel, key) duplicate.  NULL dedupe_key rows are
-- unconstrained.
CREATE UNIQUE INDEX uq_notifications_dedupe
    ON notifications (tenant_id, recipient_user_id, channel, dedupe_key)
    WHERE dedupe_key IS NOT NULL;

CREATE INDEX idx_notifications_recipient
    ON notifications (tenant_id, recipient_user_id, channel, status, created_at DESC);

-- ---------------------------------------------------------------------------
-- notification_reads (アプリ内既読管理 / 本人単位)
-- ---------------------------------------------------------------------------
-- Read marker per (notification, recipient).  Existence of a row = read.
-- recipient_user_id must equal the notification's recipient and the
-- authenticated user (enforced at the service layer).  A separate table (rather
-- than a notifications.read_at column) keeps "read" as a per-user event.
CREATE TABLE notification_reads (
    id                uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id         uuid        NOT NULL REFERENCES tenants(id),
    notification_id   uuid        NOT NULL,
    -- recipient_user_id: logical reference to users.id (verified in service layer).
    recipient_user_id uuid        NOT NULL,
    read_at           timestamptz NOT NULL DEFAULT now(),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_notification_reads PRIMARY KEY (id),
    -- [Security] Composite FK: (notification_id, tenant_id) must exist in
    -- notifications.  Prevents cross-tenant read markers.
    CONSTRAINT fk_notification_reads_notification_tenant
        FOREIGN KEY (notification_id, tenant_id)
        REFERENCES notifications(id, tenant_id)
        MATCH SIMPLE,
    -- One read marker per (notification, recipient).
    CONSTRAINT uq_notification_reads_notif_user
        UNIQUE (notification_id, recipient_user_id),
    CONSTRAINT uq_notification_reads_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_notification_reads_lookup
    ON notification_reads (tenant_id, recipient_user_id, notification_id);

-- ---------------------------------------------------------------------------
-- email_deliveries (メール配信記録)
-- ---------------------------------------------------------------------------
-- One row per email send attempt-lifecycle for a notification.
--
-- to_email_enc: AES-256-GCM ciphertext of the destination email address.
--   The raw email address (PII) is NEVER stored in plaintext.  Decryption (rare,
--   e.g. ops bounce reconciliation) requires the notification:read_sensitive
--   permission and is performed in the application layer.  bytea prevents
--   accidental indexing/logging of the encrypted value as text.
-- to_email_hash: deterministic non-reversible hash (hex) of the destination
--   address, used for matching / dedupe WITHOUT exposing the address.
-- provider_message_id: opaque provider message id (bounce/complaint reconciliation).
-- attempts / max_attempts: retry accounting (max_attempts is an operational
--   default and SHOULD be made configurable per tenant — see legal/config note).
-- last_error: short non-PII error code/category only (never the recipient address
--   or message body).
CREATE TABLE email_deliveries (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    notification_id     uuid        NOT NULL,
    -- to_email_hash: hex SHA-256 of destination email (non-reversible, for match).
    to_email_hash       text        NOT NULL DEFAULT '',
    -- to_email_enc: AES-256-GCM ciphertext of the destination email (PII).
    -- SECURITY: plaintext email is NEVER stored.
    to_email_enc        bytea,
    -- provider: "mock" (MVP) | "ses" | "sendgrid" (configurable).
    provider            text        NOT NULL DEFAULT 'mock',
    -- provider_message_id: opaque id returned by the provider (bounce matching).
    provider_message_id text        NOT NULL DEFAULT '',
    -- status: queued -> sent | failed | bounced | complained.
    status              text        NOT NULL DEFAULT 'queued',
    attempts            integer     NOT NULL DEFAULT 0,
    -- max_attempts: operational placeholder — MUST be made configurable.
    max_attempts        integer     NOT NULL DEFAULT 3,
    -- last_error: short non-PII error category only.
    last_error          text        NOT NULL DEFAULT '',
    sent_at             timestamptz,
    bounced_at          timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_email_deliveries PRIMARY KEY (id),
    CONSTRAINT chk_email_deliveries_status
        CHECK (status IN ('queued', 'sent', 'failed', 'bounced', 'complained')),
    -- [Security] Composite FK: (notification_id, tenant_id) must exist in
    -- notifications.  Prevents cross-tenant delivery rows.
    CONSTRAINT fk_email_deliveries_notification_tenant
        FOREIGN KEY (notification_id, tenant_id)
        REFERENCES notifications(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_email_deliveries_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_email_deliveries_lookup
    ON email_deliveries (tenant_id, notification_id, status);

-- Index for retry sweeps: failed rows not yet at max attempts.
CREATE INDEX idx_email_deliveries_retry
    ON email_deliveries (tenant_id, status, attempts);

-- ---------------------------------------------------------------------------
-- notification_preferences (受信者の通知設定 / オプトイン・アウト)
-- ---------------------------------------------------------------------------
-- Per (user, event_type, channel) opt-in flag.  forced=true marks a mandatory
-- notification (security/legal) that ignores opt-out and is always delivered.
-- When no row exists, the application falls back to a built-in default
-- (in_app defaults on; email default depends on the event type — resolved in code).
--
-- user_id: PLAIN uuid (logical reference to users.id), verified at service layer.
CREATE TABLE notification_preferences (
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES tenants(id),
    -- user_id: logical reference to users.id (verified in service layer).
    user_id     uuid        NOT NULL,
    event_type  text        NOT NULL,
    -- channel: "in_app" | "email"
    channel     text        NOT NULL,
    opted_in    boolean     NOT NULL DEFAULT true,
    -- forced: when true, opt-out is ignored (mandatory security/legal notice).
    forced      boolean     NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_notification_preferences PRIMARY KEY (id),
    CONSTRAINT chk_notification_preferences_channel
        CHECK (channel IN ('in_app', 'email')),
    -- One preference per (tenant, user, event_type, channel).
    CONSTRAINT uq_notification_preferences_lookup
        UNIQUE (tenant_id, user_id, event_type, channel),
    CONSTRAINT uq_notification_preferences_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_notification_preferences_lookup
    ON notification_preferences (tenant_id, user_id, event_type, channel);

-- ---------------------------------------------------------------------------
-- RLS — all new tables
-- ---------------------------------------------------------------------------
ALTER TABLE notification_templates     ENABLE ROW LEVEL SECURITY;
ALTER TABLE notification_templates     FORCE  ROW LEVEL SECURITY;
ALTER TABLE notifications              ENABLE ROW LEVEL SECURITY;
ALTER TABLE notifications              FORCE  ROW LEVEL SECURITY;
ALTER TABLE notification_reads         ENABLE ROW LEVEL SECURITY;
ALTER TABLE notification_reads         FORCE  ROW LEVEL SECURITY;
ALTER TABLE email_deliveries           ENABLE ROW LEVEL SECURITY;
ALTER TABLE email_deliveries           FORCE  ROW LEVEL SECURITY;
ALTER TABLE notification_preferences   ENABLE ROW LEVEL SECURITY;
ALTER TABLE notification_preferences   FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON notification_templates
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON notifications
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON notification_reads
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON email_deliveries
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON notification_preferences
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to hr_app
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE notification_templates   TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE notifications            TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE notification_reads       TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE email_deliveries         TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE notification_preferences TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE notification_preferences FROM hr_app;
REVOKE ALL ON TABLE email_deliveries         FROM hr_app;
REVOKE ALL ON TABLE notification_reads       FROM hr_app;
REVOKE ALL ON TABLE notifications            FROM hr_app;
REVOKE ALL ON TABLE notification_templates   FROM hr_app;

DROP TABLE IF EXISTS notification_preferences;
DROP TABLE IF EXISTS email_deliveries;
DROP TABLE IF EXISTS notification_reads;
DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS notification_templates;

-- +goose StatementEnd
