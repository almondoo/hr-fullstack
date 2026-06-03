-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- ST-TM-03  1on1 (アジェンダ・記録・アクション管理)
-- ===========================================================================
-- Manages continuous 1on1 meetings between a manager (上司) and a member (部下).
-- A "series" represents the continuous relationship; sessions accumulate under
-- a series, each carrying agenda items, notes (shared/private), and next actions.
--
-- Privacy / security design (docs/05 §4, CMP-004 個人情報保護法 利用目的明示・最小アクセス):
--   - RLS provides the tenant boundary only.  Participant scope (manager/member)
--     and private-note author scope are enforced in the application (service)
--     layer (defence-in-depth), NOT by RLS:
--       * Participant gate: every body/detail read path (series detail, sessions,
--         agenda, notes, open actions) resolves the actor user → users.employee_id
--         and requires it to equal the series' manager_employee_id or
--         member_employee_id; non-participants are denied (ErrForbidden) even when
--         they hold the oneonone:read RBAC permission.
--       * Private-note author scope: the notes read predicate additionally returns
--         a private note only to its author (visibility='shared'
--         OR (visibility='private' AND author_user_id = :viewer)).
--       * The HR-manager view exposes META only (GetSeriesMetadata: counts /
--         timestamps), never bodies/details, and is intentionally NOT participant-
--         gated so HR can see実施頻度/未完了件数 without reading content.
--   - 1on1 notes may contain sensitive dialogue (health / family circumstances).
--     The audit log stores meta-information only (who accessed/updated when),
--     never the note body.  Notification payloads must never carry the body.
--   - body is plaintext text (not 要配慮PII column-encryption), because the
--     control is access-scoping + non-logging, per the spec encryptedColumns=[].

-- ---------------------------------------------------------------------------
-- one_on_one_series (上司↔部下の継続シリーズ)
-- ---------------------------------------------------------------------------
-- cadence: リマインド用の頻度ラベル (設定値であり法令値ではない).
-- status:  active | closed  (異動などでシリーズを終了する場合 closed).
-- manager_employee_id / member_employee_id: 複合FKで両者が同一テナントの
-- employees に存在することを強制 (クロステナント禁止).
CREATE TABLE one_on_one_series (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL REFERENCES tenants(id),
    manager_employee_id uuid        NOT NULL,
    member_employee_id  uuid        NOT NULL,
    title               text        NOT NULL DEFAULT '',
    -- cadence: weekly | biweekly | monthly | quarterly | adhoc (リマインド用設定値)
    cadence             text        NOT NULL DEFAULT 'biweekly',
    -- status: active | closed
    status              text        NOT NULL DEFAULT 'active',
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_one_on_one_series PRIMARY KEY (id),
    CONSTRAINT chk_one_on_one_series_cadence
        CHECK (cadence IN ('weekly', 'biweekly', 'monthly', 'quarterly', 'adhoc')),
    CONSTRAINT chk_one_on_one_series_status
        CHECK (status IN ('active', 'closed')),
    -- [Security] Composite FK: manager must be an employee in the same tenant.
    CONSTRAINT fk_one_on_one_series_manager_tenant
        FOREIGN KEY (manager_employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- [Security] Composite FK: member must be an employee in the same tenant.
    CONSTRAINT fk_one_on_one_series_member_tenant
        FOREIGN KEY (member_employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    -- UNIQUE(id, tenant_id) required for downstream composite FK references.
    CONSTRAINT uq_one_on_one_series_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_one_on_one_series_lookup
    ON one_on_one_series (tenant_id, manager_employee_id, member_employee_id, status);

-- ---------------------------------------------------------------------------
-- one_on_one_sessions (個々の1on1セッション)
-- ---------------------------------------------------------------------------
-- status:  scheduled | done | canceled
-- summary: 共有メモのヘッダ程度 (本文は one_on_one_notes 側).
-- 複合FK fk_one_on_one_sessions_series_tenant → one_on_one_series(id, tenant_id).
CREATE TABLE one_on_one_sessions (
    id           uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id    uuid        NOT NULL REFERENCES tenants(id),
    series_id    uuid        NOT NULL,
    scheduled_at timestamptz,
    held_at      timestamptz,
    -- status: scheduled | done | canceled
    status       text        NOT NULL DEFAULT 'scheduled',
    summary      text        NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_one_on_one_sessions PRIMARY KEY (id),
    CONSTRAINT chk_one_on_one_sessions_status
        CHECK (status IN ('scheduled', 'done', 'canceled')),
    -- [Security] Composite FK to own series, same tenant.
    CONSTRAINT fk_one_on_one_sessions_series_tenant
        FOREIGN KEY (series_id, tenant_id)
        REFERENCES one_on_one_series(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_one_on_one_sessions_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_one_on_one_sessions_lookup
    ON one_on_one_sessions (tenant_id, series_id, status);

-- ---------------------------------------------------------------------------
-- one_on_one_agenda_items (アジェンダ項目)
-- ---------------------------------------------------------------------------
-- author_user_id: 記入者 (users.id への論理参照; FKは張らない — users は同一
--   テナント境界がRLS+アプリ層で担保され、ここでは UUID 値のみ保持).
-- carried_over_from_id: 前回からの持ち越し元 agenda item (自己参照複合FK).
-- 複合FK fk_one_on_one_agenda_items_session_tenant → one_on_one_sessions(id, tenant_id).
CREATE TABLE one_on_one_agenda_items (
    id                   uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id            uuid        NOT NULL REFERENCES tenants(id),
    session_id           uuid        NOT NULL,
    topic                text        NOT NULL,
    -- author_user_id: 記入者 (users.id 論理参照、FKなし)
    author_user_id       uuid,
    sort_order           integer     NOT NULL DEFAULT 0,
    -- carried_over_from_id: 前回未消化アジェンダの複製元 (自己参照)
    carried_over_from_id uuid,
    created_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_one_on_one_agenda_items PRIMARY KEY (id),
    -- [Security] Composite FK to session, same tenant.
    CONSTRAINT fk_one_on_one_agenda_items_session_tenant
        FOREIGN KEY (session_id, tenant_id)
        REFERENCES one_on_one_sessions(id, tenant_id)
        MATCH SIMPLE,
    -- [Security] Self composite FK: carry-over source is an agenda item in the
    -- same tenant.
    CONSTRAINT fk_one_on_one_agenda_items_carried_over_tenant
        FOREIGN KEY (carried_over_from_id, tenant_id)
        REFERENCES one_on_one_agenda_items(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_one_on_one_agenda_items_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_one_on_one_agenda_items_lookup
    ON one_on_one_agenda_items (tenant_id, session_id, sort_order);

-- ---------------------------------------------------------------------------
-- one_on_one_notes (セッション記録)
-- ---------------------------------------------------------------------------
-- visibility: shared | private
--   - shared : 両参加者 (上司・部下) が閲覧可.
--   - private: author_user_id 本人のみ閲覧可. 相手・人事管理者にも非公開.
-- 可視性の参加者/本人スコープはアプリ層の必須クエリ条件で多層防御
--   (visibility='shared' OR (visibility='private' AND author_user_id = :current)).
-- body は本文 (text). 監査・通知ペイロードに本文を載せない運用を徹底.
-- author_user_id: users.id への論理参照 (FKなし).
CREATE TABLE one_on_one_notes (
    id             uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id      uuid        NOT NULL REFERENCES tenants(id),
    session_id     uuid        NOT NULL,
    -- author_user_id: 記入者 (users.id 論理参照、FKなし). private の可視性判定に使用.
    author_user_id uuid        NOT NULL,
    -- visibility: shared | private
    visibility     text        NOT NULL DEFAULT 'shared',
    body           text        NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_one_on_one_notes PRIMARY KEY (id),
    CONSTRAINT chk_one_on_one_notes_visibility
        CHECK (visibility IN ('shared', 'private')),
    -- [Security] Composite FK to session, same tenant.
    CONSTRAINT fk_one_on_one_notes_session_tenant
        FOREIGN KEY (session_id, tenant_id)
        REFERENCES one_on_one_sessions(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_one_on_one_notes_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_one_on_one_notes_lookup
    ON one_on_one_notes (tenant_id, session_id, visibility, author_user_id);

-- ---------------------------------------------------------------------------
-- one_on_one_actions (ネクストアクション)
-- ---------------------------------------------------------------------------
-- status:  open | done | canceled
-- assignee_employee_id: 担当者 (上司/部下). 複合FKで同一テナント従業員に限定.
-- due_date リマインドは通知基盤フック (ST-FND-09; 未実装のためフックのみ).
-- 未完了 (open) は同一シリーズ横断で次セッション画面へ集約表示する.
-- 完了時 completed_at を設定.
-- 複合FK fk_one_on_one_actions_session_tenant → one_on_one_sessions(id, tenant_id).
CREATE TABLE one_on_one_actions (
    id                   uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id            uuid        NOT NULL REFERENCES tenants(id),
    session_id           uuid        NOT NULL,
    assignee_employee_id uuid        NOT NULL,
    description          text        NOT NULL,
    due_date             date,
    -- status: open | done | canceled
    status               text        NOT NULL DEFAULT 'open',
    completed_at         timestamptz,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_one_on_one_actions PRIMARY KEY (id),
    CONSTRAINT chk_one_on_one_actions_status
        CHECK (status IN ('open', 'done', 'canceled')),
    -- [Security] Composite FK to session, same tenant.
    CONSTRAINT fk_one_on_one_actions_session_tenant
        FOREIGN KEY (session_id, tenant_id)
        REFERENCES one_on_one_sessions(id, tenant_id)
        MATCH SIMPLE,
    -- [Security] Composite FK: assignee must be an employee in the same tenant.
    CONSTRAINT fk_one_on_one_actions_assignee_tenant
        FOREIGN KEY (assignee_employee_id, tenant_id)
        REFERENCES employees(id, tenant_id)
        MATCH SIMPLE,
    CONSTRAINT uq_one_on_one_actions_id_tenant UNIQUE (id, tenant_id)
);

CREATE INDEX idx_one_on_one_actions_lookup
    ON one_on_one_actions (tenant_id, session_id, status);

CREATE INDEX idx_one_on_one_actions_assignee
    ON one_on_one_actions (tenant_id, assignee_employee_id, status, due_date);

-- ---------------------------------------------------------------------------
-- tm_settings (タレントマネジメント設定 — 1on1 開示/保持の設定化)
-- ---------------------------------------------------------------------------
-- 法令値ではないが社内規程・個人情報の利用目的に依存する値を設定化する
-- (legalConfigPoints).  本実装は法的助言ではない。値は社内規程・社労士/弁護士
-- 確認のうえ設定で改正/方針変更に追従すること。
--
--   - hr_manager_body_disclosure: 人事管理者が共有メモ本文を閲覧できるか.
--     既定は false (最小アクセス CMP-004). true に設定された場合のみ、
--     人事管理者メタ閲覧APIが共有メモ本文を返してよい (private は常に非公開).
--   - note_retention_days: 1on1記録の保持期間 (NULL=方針未設定; 物理削除は
--     アプリ層の運用判断。本テーブルは方針値の保持のみ).
-- 1テナント1行 (UNIQUE tenant_id).
CREATE TABLE tm_settings (
    id                           uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id                    uuid        NOT NULL REFERENCES tenants(id),
    -- hr_manager_body_disclosure: 既定 false = 本文は人事管理者にも非公開
    hr_manager_body_disclosure   boolean     NOT NULL DEFAULT false,
    -- note_retention_days: 保持期間 (日). NULL=未設定.
    note_retention_days          integer,
    created_at                   timestamptz NOT NULL DEFAULT now(),
    updated_at                   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_tm_settings PRIMARY KEY (id),
    CONSTRAINT uq_tm_settings_tenant UNIQUE (tenant_id),
    CONSTRAINT uq_tm_settings_id_tenant UNIQUE (id, tenant_id)
);

-- ---------------------------------------------------------------------------
-- RLS — all new tables
-- ---------------------------------------------------------------------------
ALTER TABLE one_on_one_series       ENABLE ROW LEVEL SECURITY;
ALTER TABLE one_on_one_series       FORCE  ROW LEVEL SECURITY;
ALTER TABLE one_on_one_sessions     ENABLE ROW LEVEL SECURITY;
ALTER TABLE one_on_one_sessions     FORCE  ROW LEVEL SECURITY;
ALTER TABLE one_on_one_agenda_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE one_on_one_agenda_items FORCE  ROW LEVEL SECURITY;
ALTER TABLE one_on_one_notes        ENABLE ROW LEVEL SECURITY;
ALTER TABLE one_on_one_notes        FORCE  ROW LEVEL SECURITY;
ALTER TABLE one_on_one_actions      ENABLE ROW LEVEL SECURITY;
ALTER TABLE one_on_one_actions      FORCE  ROW LEVEL SECURITY;
ALTER TABLE tm_settings             ENABLE ROW LEVEL SECURITY;
ALTER TABLE tm_settings             FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON one_on_one_series
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON one_on_one_sessions
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON one_on_one_agenda_items
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON one_on_one_notes
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON one_on_one_actions
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY tenant_isolation ON tm_settings
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ---------------------------------------------------------------------------
-- Grants to hr_app
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE one_on_one_series       TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE one_on_one_sessions     TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE one_on_one_agenda_items TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE one_on_one_notes        TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE one_on_one_actions      TO hr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE tm_settings             TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE tm_settings             FROM hr_app;
REVOKE ALL ON TABLE one_on_one_actions      FROM hr_app;
REVOKE ALL ON TABLE one_on_one_notes        FROM hr_app;
REVOKE ALL ON TABLE one_on_one_agenda_items FROM hr_app;
REVOKE ALL ON TABLE one_on_one_sessions     FROM hr_app;
REVOKE ALL ON TABLE one_on_one_series       FROM hr_app;

DROP TABLE IF EXISTS tm_settings;
DROP TABLE IF EXISTS one_on_one_actions;
DROP TABLE IF EXISTS one_on_one_notes;
DROP TABLE IF EXISTS one_on_one_agenda_items;
DROP TABLE IF EXISTS one_on_one_sessions;
DROP TABLE IF EXISTS one_on_one_series;

-- +goose StatementEnd
