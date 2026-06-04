# 07. 環境変数リファレンス

> **対応ファイル**: `.env.example`、`backend/internal/platform/config/config.go`
> **最終更新**: 2026-06-03

本ドキュメントはアプリケーション（バックエンド）が参照する全環境変数を網羅します。
`.env.example` のコメントと対応しており、本番デプロイ前のチェックリストとしても利用できます。

---

## 凡例

| 記号 | 意味 |
|---|---|
| **必須** | 未設定時にサーバーが起動拒否 |
| 必須（本番） | `APP_ENV` が `development` 以外の場合のみ必須 |
| 任意 | デフォルト値あり。未設定でも動作するが本番では推奨設定あり |
| 機密 | 実値をリポジトリにコミットしてはならない。Secret Manager / CI 変数で注入すること |

---

## 1. アプリケーション基本設定

| 変数名 | デフォルト | 必須 | 機密 | 説明 |
|---|---|---|---|---|
| `APP_ENV` | `development` | 任意 | - | 実行環境。`development` / `production` / `staging` 等。`development` 以外では各種安全バリデーションが厳格化される |
| `HTTP_PORT` | `8080` | 任意 | - | HTTP サーバーがリッスンするポート番号 |
| `GIN_MODE` | `debug` | 任意 | - | Gin フレームワークのモード。本番では `release` に設定すること |
| `MIGRATE_ON_STARTUP` | `false` | 任意 | - | `true` にするとサーバー起動時に goose マイグレーションを実行。本番では `false` にして専用 pre-deploy ステップで実行推奨 |

---

## 2. データベース接続（アプリロール: hr_app）

| 変数名 | デフォルト | 必須 | 機密 | 説明 |
|---|---|---|---|---|
| `DATABASE_URL` | - | 任意 | 機密 | PostgreSQL 接続 URL（`postgres://...`）。設定時は `DB_*` 個別変数より優先される |
| `DB_HOST` | `localhost` | 任意 | - | PostgreSQL ホスト名 |
| `DB_PORT` | `5432` | 任意 | - | PostgreSQL ポート番号 |
| `DB_USER` | `hr_app` | 任意 | - | 接続ユーザー名（アプリロール: NOBYPASSRLS） |
| `DB_PASSWORD` | - | **必須** | 機密 | 接続パスワード |
| `DB_NAME` | `hr_saas` | 任意 | - | 接続先データベース名 |
| `DB_SSLMODE` | `disable` | 任意 | - | SSL モード。本番では `require` / `verify-ca` / `verify-full` に設定すること |

> **本番注意**: `DB_SSLMODE=disable` は開発用。本番では必ず `verify-full` を使用し、RDS / Cloud SQL 等のマネージドサービスのサーバー証明書を検証すること。

---

## 3. データベース接続（管理ロール: マイグレーション専用）

| 変数名 | デフォルト | 必須 | 機密 | 説明 |
|---|---|---|---|---|
| `ADMIN_DATABASE_URL` | - | 必須（本番） | 機密 | 管理用接続 URL。`MIGRATE_ON_STARTUP=true` またはマイグレーションコマンド実行時に使用 |
| `DB_ADMIN_USER` | `$DB_USER` | 必須（本番） | - | 管理ユーザー名。本番では `ADMIN_DATABASE_URL` または `DB_ADMIN_USER` のどちらかが必須 |
| `DB_ADMIN_PASSWORD` | `$DB_PASSWORD` | 必須（本番） | 機密 | 管理ユーザーパスワード |

> **本番注意**: `hr_app` ロール（`NOBYPASSRLS`）はDDL権限を持たないため、マイグレーションには必ずスーパーユーザーを使用すること。

---

## 4. Docker Compose: ポート設定（開発時のみ使用）

| 変数名 | デフォルト | 必須 | 機密 | 説明 |
|---|---|---|---|---|
| `POSTGRES_USER` | `postgres` | 任意 | - | Docker 上の PostgreSQL 管理ユーザー名 |
| `POSTGRES_PASSWORD` | - | **必須** | 機密 | Docker 上の PostgreSQL 管理パスワード |
| `POSTGRES_DB` | `hr_saas` | 任意 | - | Docker 上の PostgreSQL データベース名 |
| `HR_APP_DB_PASSWORD` | - | **必須** | 機密 | `hr_app` ロールのパスワード（Docker 初回起動時に設定される） |
| `DB_HOST_PORT` | `15432` | 任意 | - | ホスト側公開ポート番号（他のローカルPostgreSQLとの競合回避用）。コンテナ間通信には影響しない |

---

## 5. セッション Cookie

| 変数名 | デフォルト | 必須 | 機密 | 説明 |
|---|---|---|---|---|
| `SESSION_COOKIE_NAME` | `hr_session` | 任意 | - | セッション Cookie の名前 |
| `SESSION_TTL` | `24h` | 任意 | - | セッション有効期間。Go の duration 文字列（例: `24h`, `8h`, `168h`） |
| `SESSION_COOKIE_SECURE` | `false` | 必須（本番） | - | `true` にすると Cookie に `Secure` 属性を付与（HTTPS 専用）。本番では必ず `true` |
| `SESSION_COOKIE_SAMESITE` | `lax` | 任意 | - | SameSite 属性。`lax` / `strict` / `none`。`none` 使用時は `SESSION_COOKIE_SECURE=true` が必須 |
| `SESSION_HASH_KEY` | - | 必須（本番） | 機密 | HMAC 署名キー（32 バイト以上の hex 文字列）。`openssl rand -hex 32` で生成 |
| `SESSION_BLOCK_KEY` | - | 必須（本番） | 機密 | AES 暗号化キー（16/24/32 バイトの hex 文字列）。`openssl rand -hex 32` で生成 |

---

## 6. CSRF 保護

| 変数名 | デフォルト | 必須 | 機密 | 説明 |
|---|---|---|---|---|
| `CSRF_AUTH_KEY` | - | 必須（本番） | 機密 | gorilla/csrf が使用する 32 バイトキー（64 文字 hex）。`openssl rand -hex 32` で生成。開発環境では未設定時に起動時ランダム生成 |
| `CSRF_SECURE` | `false` | 必須（本番） | - | CSRF Cookie に `Secure` 属性を付与。本番では `true` に設定すること |

---

## 7. フィールドレベル暗号化（機微 PII）

| 変数名 | デフォルト | 必須 | 機密 | 説明 |
|---|---|---|---|---|
| `FIELD_ENCRYPTION_KEY` | - | 必須（本番） | 機密 | AES-256-GCM 用 32 バイトキーを base64 標準エンコードした値。口座番号・マイナンバー等の機微 PII 列暗号化に使用。`openssl rand -base64 32` で生成。開発環境では未設定時に起動時一時キーを生成（再起動後復号不可） |

> **本番注意**: このキーは AWS Secrets Manager / GCP Secret Manager / HashiCorp Vault 等の秘密管理基盤から実行時にインジェクトすること。絶対にリポジトリにコミットしないこと。

---

## 8. CORS

| 変数名 | デフォルト | 必須 | 機密 | 説明 |
|---|---|---|---|---|
| `CORS_ALLOW_ORIGINS` | `http://localhost:3000` | 必須（本番） | - | 許可するオリジンのカンマ区切りリスト。本番では `https://your-domain.example.com` 等を明示設定 |

---

## 9. レート制限

| 変数名 | デフォルト | 必須 | 機密 | 説明 |
|---|---|---|---|---|
| `AUTH_RATE_LIMIT` | `10-M` | 任意 | - | 認証エンドポイント（ログイン・サインアップ）のレート制限。書式: `<回数>-<単位>` (例: `10-M` = 1分10回, `100-H` = 1時間100回) |

---

## 10. 信頼するプロキシ

| 変数名 | デフォルト | 必須 | 機密 | 説明 |
|---|---|---|---|---|
| `TRUSTED_PROXIES` | - | 任意 | - | X-Forwarded-For を信頼するロードバランサー/リバースプロキシのIPアドレスまたはCIDRカンマ区切りリスト。未設定時は転送ヘッダーを無視し直接TCP接続元IPを使用（安全側）。例: `10.0.0.0/8,172.16.0.0/12` |

---

## 本番チェックリスト

機密変数のチェック（実値をリポジトリにコミットしていないか確認）：

- [ ] `DB_PASSWORD` / `HR_APP_DB_PASSWORD` / `POSTGRES_PASSWORD` — Secret Manager からインジェクト
- [ ] `DB_ADMIN_PASSWORD` / `ADMIN_DATABASE_URL` — Secret Manager からインジェクト
- [ ] `SESSION_HASH_KEY` / `SESSION_BLOCK_KEY` — 32 バイト以上のランダム値
- [ ] `CSRF_AUTH_KEY` — 64 文字 hex ランダム値
- [ ] `FIELD_ENCRYPTION_KEY` — base64 エンコード 32 バイトランダム値
- [ ] `DATABASE_URL` / `ADMIN_DATABASE_URL` の接続文字列に実パスワードを含んでいないか確認

本番必須設定の確認：

- [ ] `APP_ENV=production`（または `staging`）
- [ ] `GIN_MODE=release`
- [ ] `DB_SSLMODE=verify-full`（マネージドDB使用時）
- [ ] `SESSION_COOKIE_SECURE=true`
- [ ] `CSRF_SECURE=true`
- [ ] `MIGRATE_ON_STARTUP=false`（pre-deploy ステップで実行）
- [ ] `CORS_ALLOW_ORIGINS` に本番ドメインを明示設定
- [ ] `TRUSTED_PROXIES` にロードバランサーのCIDRを設定（必要な場合）
