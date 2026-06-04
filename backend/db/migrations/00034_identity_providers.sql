-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- 00034: identity_providers — SSO/IdP 設定テーブル
--
-- 目的:
--   テナントごとに OIDC / SAML 2.0 IdP 設定を保持する。
--   JIT プロビジョニング・ロールマッピング・許可メールドメインの設定も含む。
--
-- 設計方針:
--   - tenant_id による RLS ENABLE + FORCE + fail-closed ポリシーでテナント境界を保証。
--   - (id, tenant_id) の複合 UNIQUE で downstream composite FK を許可。
--   - クライアントシークレット・SP 秘密鍵は平文で格納しない。
--     代わりに Secret Manager / 環境変数への参照パス (client_secret_ref 等) を格納し、
--     実際の秘密値はアプリ起動時に Secret Manager から取得する。
--   - protocol は 'oidc' / 'saml' のみ許可 (CHECK 制約)。
--   - oidc_config / saml_config は JSONB で格納 (issuer, client_id, allowed_domains,
--     role_mapping_rules 等)。秘密値の参照パスのみを含め、実値は格納しない。
--   - 監査カラム (created_at / updated_at) を必須とする。
--
-- セキュリティ注記:
--   - hr_app には SELECT / INSERT / UPDATE のみ付与 (DELETE は付与しない)。
--     論理削除 (enabled = false) による無効化を使用する。
--   - oidc_config / saml_config に秘密値を直接格納してはならない。
--     client_secret_ref は Secret Manager のシークレット名 / ARN であり、値ではない。
-- ===========================================================================

CREATE TABLE identity_providers (
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES tenants(id),

    -- protocol: 'oidc' または 'saml'
    protocol    text        NOT NULL,

    -- enabled: false の場合このプロバイダは使用不可 (論理削除代用)
    enabled     boolean     NOT NULL DEFAULT true,

    -- display_name: 管理 UI 表示名 (例: "Google Workspace", "Entra ID")
    display_name text       NOT NULL DEFAULT '',

    -- oidc_config: OIDC プロトコル設定 (JSONB)。protocol='oidc' の場合のみ使用。
    -- 格納可能フィールド:
    --   issuer_url         text  — IdP ディスカバリ URL (https://accounts.google.com 等)
    --   client_id          text  — OAuth2 クライアント ID (非秘密)
    --   client_secret_ref  text  — Secret Manager のシークレット名/ARN (値ではない)
    --   redirect_url       text  — コールバック URL
    --   scopes             text[] — 要求スコープ (デフォルト: ["openid","email","profile"])
    --   expected_algorithm text  — JWT 署名アルゴリズム (RS256 等; "none" 禁止)
    --   allowed_audiences  text[] — 許可 aud クレーム値
    -- 秘密値 (client_secret の実値) はこのカラムに格納してはならない。
    oidc_config jsonb       NOT NULL DEFAULT '{}',

    -- saml_config: SAML 2.0 プロトコル設定 (JSONB)。protocol='saml' の場合のみ使用。
    -- 格納可能フィールド:
    --   sp_entity_id         text  — SP エンティティ ID URI
    --   idp_metadata_url     text  — IdP メタデータ URL
    --   idp_certificate_ref  text  — Secret Manager のシークレット名/ARN (IdP 証明書)
    --   acs_url              text  — ACS URL
    --   sp_private_key_ref   text  — Secret Manager の SP 秘密鍵参照 (値ではない)
    --   sp_certificate       text  — SP 証明書 (公開; 秘密鍵ではない)
    --   name_id_format       text  — NameID フォーマット
    --   allowed_clock_skew_s int   — 許容クロックスキュー (秒; デフォルト 30)
    --   attribute_map        jsonb — 属性名マッピング
    -- 秘密値 (idp_certificate の実値・sp_private_key の実値) は
    -- *_ref フィールドで参照するのみで値自体を格納してはならない。
    saml_config jsonb       NOT NULL DEFAULT '{}',

    -- jit_config: JIT プロビジョニング設定 (JSONB)
    -- フィールド:
    --   enabled              bool     — JIT プロビジョニング有効フラグ
    --   default_role         text     — デフォルト割り当てロール名
    --   role_mapping_rules   jsonb[]  — [{idp_group, app_role}, ...]
    --   allowed_email_domains text[]  — 許可メールドメイン (空 = 制限なし)
    jit_config  jsonb       NOT NULL DEFAULT '{}',

    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT pk_identity_providers PRIMARY KEY (id),
    -- Downstream composite FK 用
    CONSTRAINT uq_identity_providers_id_tenant UNIQUE (id, tenant_id),
    CONSTRAINT chk_identity_providers_protocol
        CHECK (protocol IN ('oidc', 'saml'))
);

-- テナント別プロバイダ一覧検索用インデックス
CREATE INDEX idx_identity_providers_tenant
    ON identity_providers (tenant_id, enabled);

-- RLS: テナント境界の強制
ALTER TABLE identity_providers ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity_providers FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON identity_providers
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Grants: SELECT / INSERT / UPDATE のみ (DELETE は付与しない — 論理削除を使用)
GRANT SELECT, INSERT, UPDATE ON TABLE identity_providers TO hr_app;

-- sso_state: OIDC state/PKCE・SAML AuthnRequest ID の一時保存
-- セキュリティ注記:
--   - state / code_verifier / authn_request_id は短命 (TTL: 10 分)。
--   - 使用後に DELETE するか expired 行を定期クリーンアップする。
--   - code_verifier は PKCE の code_verifier (平文) を格納するため、
--     セッション等と同等の扱いが必要。生涯は callback 完了まで。
CREATE TABLE sso_state (
    id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL REFERENCES tenants(id),
    idp_id          uuid        NOT NULL,

    -- state: OIDC の state パラメータ (cryptographically random, 32 bytes base64url)
    -- SAML の RelayState も同値を使用する。
    state           text        NOT NULL,

    -- code_verifier: PKCE code_verifier (OIDC のみ)。空文字は「PKCE なし」を表す。
    code_verifier   text        NOT NULL DEFAULT '',

    -- authn_request_id: SAML AuthnRequest の ID (SAML のみ)。リプレイ防止に使用。
    authn_request_id text       NOT NULL DEFAULT '',

    -- expires_at: state の有効期限 (デフォルト 10 分)
    expires_at      timestamptz NOT NULL DEFAULT (now() + interval '10 minutes'),

    created_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT pk_sso_state PRIMARY KEY (id),
    CONSTRAINT uq_sso_state_tenant_state UNIQUE (tenant_id, state),
    -- Downstream composite FK 用
    CONSTRAINT uq_sso_state_id_tenant UNIQUE (id, tenant_id)
);

-- TTL クリーンアップ用インデックス
CREATE INDEX idx_sso_state_expires_at ON sso_state (expires_at);
-- テナント・state でのルックアップ用インデックス
CREATE INDEX idx_sso_state_tenant_state ON sso_state (tenant_id, state);

-- RLS
ALTER TABLE sso_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE sso_state FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON sso_state
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Grants: フルアクセス (state は使用後 DELETE する)
GRANT SELECT, INSERT, DELETE ON TABLE sso_state TO hr_app;

-- sso_identities: SSO ユーザと内部ユーザの紐付け
-- (tenant_id, idp_id, subject_id) が一意なユーザ識別子
CREATE TABLE sso_identities (
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES tenants(id),
    -- users への複合 FK (uq_users_id_tenant は 00027 で追加済み)
    user_id     uuid        NOT NULL,
    -- identity_providers への複合 FK
    idp_id      uuid        NOT NULL,

    -- subject_id: IdP が発行する安定したユーザ識別子
    --   OIDC: "sub" クレーム
    --   SAML: NameID
    -- PII を含む可能性があるため、将来的には暗号化を検討。
    -- 現時点ではインデックスが必要なため平文格納 (ログ出力禁止)。
    subject_id  text        NOT NULL,

    last_login_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT pk_sso_identities PRIMARY KEY (id),
    CONSTRAINT uq_sso_identities_id_tenant UNIQUE (id, tenant_id),
    -- 同一 IdP での同一 subject は1つのユーザにのみ紐付く
    CONSTRAINT uq_sso_identities_idp_subject
        UNIQUE (tenant_id, idp_id, subject_id),
    -- users への複合 FK
    CONSTRAINT fk_sso_identities_user_tenant
        FOREIGN KEY (user_id, tenant_id)
        REFERENCES users(id, tenant_id),
    -- identity_providers への複合 FK
    CONSTRAINT fk_sso_identities_idp_tenant
        FOREIGN KEY (idp_id, tenant_id)
        REFERENCES identity_providers(id, tenant_id)
);

CREATE INDEX idx_sso_identities_tenant_user
    ON sso_identities (tenant_id, user_id);

-- RLS
ALTER TABLE sso_identities ENABLE ROW LEVEL SECURITY;
ALTER TABLE sso_identities FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON sso_identities
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE ON TABLE sso_identities TO hr_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

REVOKE ALL ON TABLE sso_identities      FROM hr_app;
REVOKE ALL ON TABLE sso_state           FROM hr_app;
REVOKE ALL ON TABLE identity_providers  FROM hr_app;

DROP TABLE IF EXISTS sso_identities;
DROP TABLE IF EXISTS sso_state;
DROP TABLE IF EXISTS identity_providers;

-- +goose StatementEnd
