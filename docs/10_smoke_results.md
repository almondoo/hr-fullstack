# 10. Smoke テスト結果 / 疎通・性能確認

> **実施日**: 2026-06-03  
> **ブランチ**: feat/remaining-issues  
> **Issue**: [#5] 疎通・性能確認 (compose smoke / RLS通し / N+1)

---

## 1. 環境

| 項目 | 値 |
|------|-----|
| Docker | 29.5.2 |
| PostgreSQL | 17-alpine |
| Go (ホスト) | 1.26 |
| API ポート | 18080 (ホスト) → 8080 (コンテナ) ※8080 は別サービスが使用中のため |
| DB ポート | 15432 (ホスト) → 5432 (コンテナ) |

---

## 2. 起動フロー

```
docker compose up -d --build   # DB先起動 → api起動
# ポート競合(8080)のため、api は compose run で 18080 にマッピング:
docker compose run --rm -d -p 18080:8080 api sh -c "go mod tidy && air -c .air.toml"
```

**起動結果**: 成功  
- DB コンテナ: healthy  
- API コンテナ: `MIGRATE_ON_STARTUP=true` でマイグレーション適用後に起動確認

### 起動時ブロッカー (解消済み)

| # | 事象 | 原因 | 対処 |
|---|------|------|------|
| 1 | `hr_app` ロール未作成でマイグレーション失敗 | `10-create-app-role.sh` の `PASSWORD :'hr_app_pass'` が PL/pgSQL DO ブロック内では psql 変数展開されない構文バグ | スクリプトを shell-level 条件分岐 + 平文 SQL `CREATE ROLE` に修正（後述） |
| 2 | ホスト 8080 ポート競合 | 別 Node プロセスが 8080 使用中 | `docker compose run -p 18080:8080` で回避。スクリプトのデフォルト URL を 18080 に設定 |

---

## 3. Smoke テスト結果

スクリプト: `scripts/smoke.sh`  
実行コマンド: `bash scripts/smoke.sh http://localhost:18080`

| # | チェック | 結果 |
|---|----------|------|
| 1 | `GET /healthz` → 200 | PASS |
| 2 | `GET /readyz` → 200 | PASS |
| 3 | `GET /api/v1/csrf` → token取得 | PASS |
| 4 | `POST /api/v1/auth/signup` → 201 | PASS |
| 5 | `POST /api/v1/auth/login` → 200 | PASS |
| 6 | `GET /api/v1/auth/me` → 200、user_id一致 | PASS |
| 7 | `POST /api/v1/employees` → 201 | PASS |
| 8 | `GET /api/v1/employees` → 1件 | PASS |
| 9 | RLS 分離: テナントB の employees リストが 0件 | PASS |

**全9チェック PASS**

---

## 4. RLS / テナント分離確認

### 確認方法
稼働スタックで2テナント(A/B)を作成し、テナントBのセッションからテナントAの従業員を取得できないことを検証。

```
# Tenant A: 従業員 EMP001 (Yamada Taro) を作成
# Tenant B: セッション確立後 GET /api/v1/employees → []
```

**結果**: テナントBの employees レスポンスは `{"employees":[]}` (件数=0)  
テナントAの従業員はテナントBから参照不可 → RLS ポリシーが正常動作を確認。

### RLS 実装根拠
- `tenantdb.WithinTenant` が全クエリ実行前に `SET LOCAL app.tenant_id = '<uuid>'` を発行
- 全対象テーブルに `ENABLE ROW LEVEL SECURITY` + `FORCE ROW LEVEL SECURITY` + `tenant_isolation` ポリシー (USING/WITH CHECK)
- アプリロール `hr_app` は `NOBYPASSRLS`

---

## 5. N+1 / クエリパターン調査

### 調査範囲
- `internal/employee/service.go` — ListEmployees, ListAssignments, ListContracts
- `internal/attendance/service.go` — ListRecords, ComputeSummary
- `internal/leave/service.go` — ListGrants, GetBalance
- `internal/offer/service.go` — ListOffers
- `internal/notification/outbox.go` — ProcessOutbox
- `internal/ledger/service.go` — CheckRetention
- `internal/reporting/service.go` — RunReport

### 結果

**N+1 問題: 検出なし**

全 List 系ハンドラは以下のパターンを使用しており、GORM の `Preload` や for ループ内クエリはない:

- `tx.Raw(...).Scan(&slice)` — 単一 SELECT で全行を一括取得
- `tenantdb.WithinTenant` ラッパーは1トランザクション内で完結
- `for range` ループはクエリ結果の in-memory 集計のみ (DB アクセスなし)

#### 特記事項

| パッケージ | パターン | 評価 |
|-----------|----------|------|
| `notification/outbox.go` | `for _, row := range rows { processOutboxRow }` — 行ごとに1回の UPDATE | 意図的設計 (Transactional Outbox = 行単位の分離が必要)。N+1 ではない |
| `ledger/service.go` CheckRetention | 固定3テーブル (worker_rosters, wage_ledgers, attendance_books) への順次クエリ | テーブル数固定で行数に比例しない。問題なし |
| `attendance/service.go` ComputeSummary | `for _, r := range recs` — in-memory 計算のみ | DB コールなし。問題なし |

---

## 6. init スクリプト修正 (`backend/db/init/10-create-app-role.sh`)

### 修正前の問題
PL/pgSQL `DO $$...$$` ブロック内で psql 変数展開 `:'hr_app_pass'` を使用していた。  
psql 変数展開はクライアントサイド (SQL テキスト置換) だが、DO ブロック本体は文字列リテラルとして PostgreSQL に送られるため展開されない。  
結果: `ERROR: syntax error at or near ":"` でロール作成失敗。

### 修正後の実装
shell レベルで `pg_roles` を参照して存在確認 → 未存在時のみ平文 SQL `CREATE ROLE ... PASSWORD :'hr_app_pass'` を実行。  
`:'hr_app_pass'` は DO ブロック外の通常 SQL 文なので psql 変数展開が正常に動作する。

---

## 7. build / vet 結果

```
go build -C backend ./...    → 出力なし (PASS)
go vet -C backend ./...      → 出力なし (PASS)
```

---

## 8. 後始末

```bash
docker compose down    # コンテナ・ネットワーク削除 (volume は保持)
```

---

## 9. 残課題

| 項目 | 優先度 | 内容 |
|------|--------|------|
| ポート競合の根本解決 | 低 | `docker-compose.yml` のホスト側ポートを環境変数化 (`${API_HOST_PORT:-8080}:8080`) すると smoke.sh でのポート引数が不要になる |
| `govulncheck` / `pnpm audit` | 中 | Issue #5 受入条件「セキュリティ通し」の残作業。別タスクで実施 |
| `go test -race` 全スイート | 中 | 疎通確認スコープ外。testcontainers 使用のため `-p 1` 直列で実行要 |
| init スクリプト再テスト | 低 | `docker compose down -v && up --build` で新規ボリュームから再起動しロール作成が成功するか検証 |
