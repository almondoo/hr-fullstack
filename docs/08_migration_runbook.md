# 08. マイグレーション ランブック（本番適用手順）

> **対象**: 本番・ステージング環境への PostgreSQL スキーマ適用、ロールバック、初期セットアップ
> **マイグレーションツール**: [goose v3](https://github.com/pressly/goose)（埋め込み実行: `backend/db/migrations.go`）
> **最終更新**: 2026-06-03

---

## 0. 前提条件

- `DB_ADMIN_USER` / `DB_ADMIN_PASSWORD`（または `ADMIN_DATABASE_URL`）が設定済みであること
- 管理ユーザーは `SUPERUSER` または DDL 権限を持つロールであること（`hr_app` は `NOBYPASSRLS` / DDL 権限なしのため不可）
- `backend` ビルドバイナリまたは `cmd/migrate` バイナリが実行可能であること
- PostgreSQL 17.x が稼働中であること

---

## 1. マイグレーション一覧

| バージョン | ファイル名 | 内容 |
|---|---|---|
| 00001 | `initial_schema.sql` | テナント・部署・役職 基本スキーマ |
| 00002 | `auth.sql` | ユーザー認証テーブル |
| 00003 | `auth_rbac_audit.sql` | RBAC ロール・権限・監査ハッシュチェーン |
| 00004 | `employee_org_contract.sql` | 従業員・組織・契約 |
| 00005 | `attendance.sql` | 勤怠管理 |
| 00006 | `approval_workflow.sql` | 承認ワークフロー |
| 00007 | `leave.sql` | 休暇管理 |
| 00008 | `onboarding.sql` | オンボーディング |
| 00009 | `notification.sql` | 通知 |
| 00010 | `mynumber.sql` | マイナンバー管理（機微PII・暗号化列） |
| 00011 | `jobposting.sql` | 求人票 |
| 00012 | `goal.sql` | 目標管理 |
| 00013 | `reporting.sql` | レポーティング |
| 00014 | `govfiling.sql` | 行政電子申請 |
| 00015 | `ledger.sql` | 台帳 |
| 00016 | `billing.sql` | 請求管理 |
| 00017 | `selfservice.sql` | 従業員セルフサービス |
| 00018 | `applicant.sql` | 応募者管理（ATS） |
| 00019 | `workrule.sql` | 就業規則 |
| 00020 | `selection.sql` | 選考フロー |
| 00021 | `offer.sql` | 内定管理 |
| 00022 | `evaluation.sql` | 評価管理 |
| 00023 | `oneonone.sql` | 1on1 |
| 00024 | `interview.sql` | 面接 |
| 00025 | `hiring.sql` | 採用確定 |
| 00026 | `talent.sql` | タレントマネジメント |
| 00027 | `composite_fk_hardening.sql` | 横断複合 FK ハードニング（UNIQUE 制約 + 複合FK） |

---

## 2. 初回セットアップ（新規データベース）

### 2-1. hr_app ロールの作成

**ローカル開発（Docker）**: `docker-compose up db` を初回起動すると `docker-entrypoint-initdb.d/10-create-app-role.sh` が自動実行されてロールが作成されます。

**本番環境（手動実行）**: 以下のスクリプトをスーパーユーザーで実行してください。

```sql
-- スーパーユーザーとして接続し以下を実行
-- パスワードは Secret Manager 等から取得し、プレースホルダに置換すること

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT FROM pg_catalog.pg_roles WHERE rolname = 'hr_app'
    ) THEN
        CREATE ROLE hr_app
            LOGIN
            PASSWORD 'REPLACE_WITH_ACTUAL_HR_APP_DB_PASSWORD'
            NOSUPERUSER
            NOBYPASSRLS
            NOCREATEDB
            NOCREATEROLE;
        RAISE NOTICE 'Role hr_app created.';
    ELSE
        RAISE NOTICE 'Role hr_app already exists — skipping creation.';
    END IF;
END
$$;
```

> **セキュリティ**: `hr_app` は `NOBYPASSRLS` で作成されます。PostgreSQL の行単位セキュリティ（RLS）ポリシーが必ず適用され、テナント間のデータ越境が防止されます。

### 2-2. データベース作成とマイグレーション

```bash
# 環境変数をロード（本番では Secret Manager からインジェクトされる想定）
export ADMIN_DATABASE_URL="postgres://ADMIN_USER:ADMIN_PASSWORD@HOST:5432/hr_saas?sslmode=verify-full"

# マイグレーション実行（goose Up: 全未適用マイグレーションを適用）
# cmd/migrate は埋め込みマイグレーション（migrations.FS）を使用
cd backend
go run ./cmd/migrate up
```

または goose を直接使用する場合：

```bash
# goose 直接実行（管理DSNを使用）
GOOSE_DRIVER=postgres \
GOOSE_DBSTRING="${ADMIN_DATABASE_URL}" \
goose -dir ./db/migrations up
```

### 2-3. hr_app ロールへの権限付与

マイグレーション後、`hr_app` に各テーブルの使用権限を付与します（マイグレーション SQL 内に含まれている場合は不要）：

```sql
-- スーパーユーザーとして接続
-- スキーマ使用権限
GRANT USAGE ON SCHEMA public TO hr_app;

-- 全テーブルへの読み書き権限（既存）
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO hr_app;

-- シーケンス使用権限（UUID 使用のため通常不要だが念のため）
GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO hr_app;

-- 将来のテーブルにも権限を付与（ALTER DEFAULT PRIVILEGES）
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO hr_app;
```

---

## 3. 本番マイグレーション実行手順（アップグレード）

### 3-1. 事前準備

```bash
# 1. 現在のマイグレーション状態を確認
go run ./cmd/migrate status
# または
GOOSE_DRIVER=postgres GOOSE_DBSTRING="${ADMIN_DATABASE_URL}" goose -dir ./db/migrations status
```

出力例:
```
Applied At                  | Version | Description
-----------------------------|---------|--------------------
2026-01-01 00:00:00 +0000   |   00001 | initial_schema
...（略）...
Pending                      |   00027 | composite_fk_hardening
```

### 3-2. マイグレーション実行

```bash
# 全未適用マイグレーションを適用
go run ./cmd/migrate up

# または特定バージョンまで適用
GOOSE_DRIVER=postgres GOOSE_DBSTRING="${ADMIN_DATABASE_URL}" \
  goose -dir ./db/migrations up-to 00027
```

### 3-3. 事後確認

```bash
# 適用結果を確認（全て Applied になっていること）
go run ./cmd/migrate status
```

---

## 4. ロールバック手順

> **警告**: ロールバックはデータ損失のリスクがあります。本番環境では事前に**DBスナップショット**（RDS スナップショット / pg_dump など）を取得してから実施してください。

### 4-1. 直前のマイグレーションをロールバック

```bash
# 最後に適用したマイグレーションを1つロールバック
GOOSE_DRIVER=postgres GOOSE_DBSTRING="${ADMIN_DATABASE_URL}" \
  goose -dir ./db/migrations down
```

### 4-2. 特定バージョンまでロールバック

```bash
# 指定バージョンより後のマイグレーションをロールバック
GOOSE_DRIVER=postgres GOOSE_DBSTRING="${ADMIN_DATABASE_URL}" \
  goose -dir ./db/migrations down-to 00026
```

### 4-3. ロールバック方針

| 状況 | 推奨対応 |
|---|---|
| マイグレーション中のエラー | `down` で1つロールバック → 問題修正 → 再適用 |
| テーブル追加のみの変更 | `down` でテーブル DROP が実行される（データ消失に注意） |
| 複合FK追加（00027等） | FK のみ `DROP CONSTRAINT` で安全にロールバック可能 |
| データ変換を含む変更 | スナップショット復元を優先検討 |

---

## 5. goose マイグレーションの SQL 構造

各マイグレーションファイルは以下の構造です：

```sql
-- +goose Up
-- +goose StatementBegin
-- 適用 SQL
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- ロールバック SQL
-- +goose StatementEnd
```

---

## 6. RLS（行単位セキュリティ）の本番確認

マイグレーション後、以下を確認してください：

```sql
-- RLS が ENABLED かつ FORCED になっているテーブルを確認
SELECT schemaname, tablename, rowsecurity, forcerowsecurity
FROM pg_tables
WHERE schemaname = 'public'
  AND rowsecurity = true
ORDER BY tablename;
```

`forcerowsecurity = true` のテーブルはスーパーユーザーにもRLSが適用されます。
`hr_app` ロールでのアクセス時は `SET LOCAL app.tenant_id = '<uuid>'` を必ず設定してください（`tenantdb.WithinTenant` が自動実行）。

---

## 7. 注意事項

- **`MIGRATE_ON_STARTUP=true` の本番使用は非推奨**: 複数インスタンス起動時にマイグレーションのロック競合が発生する場合があります。本番では専用の pre-deploy ステップとして実行してください。
- **マイグレーション中の停止は危険**: 途中停止すると `goose_db_version` テーブルの状態が不整合になる場合があります。その場合は手動で状態を修正するか、スナップショットから復元してください。
- **法令関連テーブルの変更は専門家確認を**: `mynumber`（マイナンバー）・`govfiling`（行政申請）等に関わるスキーマ変更は、個人情報保護法・マイナンバー法への適合を社労士・弁護士等の専門家と確認のうえ実施してください。
