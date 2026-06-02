# 04 / 技術スタック（確定）と安全なライブラリ選定

Phase 2（設計）・Phase 3（実装計画）で**確定スタックとして使用**する。
バージョンは **2026年6月時点** の安定版を基準（実装時は `govulncheck` 等で最新の脆弱性を確認）。

> セキュリティ方針：**「メンテナンスされている・実績がある・脆弱性対応が速い」** ライブラリのみ採用。
> 採用候補に迷う場合は、標準ライブラリ（`log/slog`, `net/http`, `crypto` 等）を優先する。

---

## 1. 確定スタック

| レイヤ | 技術 |
|---|---|
| バックエンド | **Go（golang）** + **Gin**、開発時ホットリロードに **Air** |
| フロントエンド | **React Router v7（Remix）** + **pnpm** |
| データベース | **PostgreSQL** |
| 実行基盤（開発） | **Docker Compose**（`api`(Go) と `db`(PostgreSQL)） |

参照バージョン（2026-06時点の安定版）：
- Go: **1.26.x**（最新安定 1.26.3、2026-05）
- PostgreSQL: **17.x**（pgx は PostgreSQL 14+ をサポート）
- React Router: **7.16.x**、Node: **22 LTS**（v8 で Node 22.12+ が要件化予定のため先取り）
- Air: **github.com/air-verse/air**（旧 `cosmtrek/air` から移管済み。新パスを使用）

### 1.1 確定アーキ判断（DECISIONS — 再選定しない）

| 項目 | 決定 | 補足 |
|---|---|---|
| DB アクセス層 | **GORM**（`gorm.io/driver/postgres`、内部は pgx/v5） | 開発速度優先。**RLS との連携に注意**（§6） |
| マルチテナント分離 | **PostgreSQL RLS + tenant_id** | DB側で越境防止。アプリは tenant コンテキストを毎回設定 |
| 認証 | **セッションCookie**（httpOnly + Secure + SameSite） | Remix(RR v7) と相性良。**CSRF対策必須**。API/モバイルは将来 JWT 併用可 |
| デプロイ先 | **未定** | Phase 2/3 で決定（候補：AWS ECS+RDS / Cloud Run+Cloud SQL / Azure Container Apps+Flexible Server） |

---

## 2. バックエンド推奨ライブラリ（安全側）

| 用途 | ライブラリ | 採用理由 / セキュリティ観点 |
|---|---|---|
| Web フレームワーク | `github.com/gin-gonic/gin` | 実績・保守が厚い。`gin.Recovery()` 必須 |
| ORM / DBアクセス【採用】 | **GORM**（`gorm.io/gorm` + `gorm.io/driver/postgres`、内部 pgx/v5） | 開発速度優先。**Raw SQL は必ずプレースホルダ**、`Where("col = ?", v)` を徹底し文字列連結を禁止。N+1に注意 |
| （代替）型安全クエリ | sqlc + pgx/v5（1.31.x） | 複雑/高性能クエリは部分的にsqlc併用可 |
| マイグレーション | `github.com/pressly/goose/v3`（代替 `golang-migrate/migrate/v4`） | バージョン管理されたスキーマ変更 |
| セッション認証【採用】 | `github.com/gorilla/sessions`（署名/暗号Cookie）または サーバ側セッションテーブル | Web主認証。httpOnly + Secure + SameSite。ログアウト/失効をサーバで管理可能に |
| CSRF対策【必須】 | `github.com/gorilla/csrf` 等 | セッションCookie認証では必須。Remix の action にCSRFトークンを付与 |
| 認証（JWT・任意） | `github.com/golang-jwt/jwt/v5` | 将来のAPI/モバイル併用向け。**alg固定検証**、`alg=none` 拒否、`EdDSA`/`ES256` 推奨 |
| パスワードハッシュ | `golang.org/x/crypto`（**argon2id**、代替 bcrypt） | 計算コスト調整可。平文保存・MD5/SHA1 は禁止 |
| 入力バリデーション | `github.com/go-playground/validator/v10` | 全外部入力を検証（サーバ側必須） |
| UUID | `github.com/google/uuid` | テナント/エンティティID。連番ID露出を避ける |
| 設定（env） | `github.com/caarlos0/env/v11`（代替 envconfig） | 秘密はコードに置かず環境変数/Secret管理 |
| ログ | 標準 `log/slog`（構造化） | 依存ゼロ。**個人情報/秘密をログに出さない** |
| セキュリティヘッダ | `github.com/gin-contrib/secure` | HSTS/関連ヘッダ。最低限は自前ミドルウェアでも可 |
| CORS | `github.com/gin-contrib/cors` | オリジン allowlist を明示（`*` を避ける） |
| レート制限 | `github.com/ulule/limiter/v3` | 総当たり/乱用対策 |
| テスト | `github.com/stretchr/testify` ＋ `testcontainers-go` | 実DBで結合テスト |
| 脆弱性検査 | `golang.org/x/vuln/cmd/govulncheck`（CI必須） | 既知脆弱性を継続検出 |

---

## 3. フロントエンド推奨（安全側）

| 用途 | 技術 | セキュリティ観点 |
|---|---|---|
| ルーティング/SSR | React Router v7（Remix） | loader/action の**サーバ側でのみ機密処理**。CSRF対策必須 |
| パッケージ管理 | pnpm | lockfile固定。`pnpm audit` を CI に組込み |
| スキーマ検証 | `zod` | フォーム/フェッチ境界の型・値検証 |
| 認証連携 | httpOnly + Secure + SameSite Cookie | トークンを localStorage に置かない |
| HTTPヘッダ | CSP / X-Frame-Options 等をサーバで付与 | XSS/クリックジャッキング対策 |
| 静的解析 | TypeScript + ESLint | 型安全・危険パターン検出 |

---

## 4. 推奨プロジェクト構成（モノレポ想定）

```
<repo>/
├─ docker-compose.yml          # api(Go) + db(PostgreSQL)
├─ .env.example
├─ backend/                    # Go (Gin + pgx + air)
│  ├─ Dockerfile.dev
│  ├─ .air.toml
│  ├─ go.mod
│  ├─ main.go                  # エントリ（/healthz, /readyz）
│  ├─ internal/                # ドメイン/サービス/リポジトリ（外部非公開）
│  │  ├─ tenant/  employee/  attendance/ ...（Bounded Context）
│  │  ├─ platform/ (db, config, auth, middleware)
│  │  └─ ...
│  ├─ db/
│  │  ├─ migrations/           # goose
│  │  └─ queries/              # sqlc 用 SQL
│  └─ sqlc.yaml
└─ frontend/                   # React Router v7 (pnpm)
   ├─ package.json
   └─ app/
```

> Go の `internal/` パッケージは外部importを禁止できるため、**ドメイン境界の保護**に有効。

---

## 5. セキュリティ・チェックリスト（実装で必ず満たす）

- [ ] **SQLi対策**：pgx のプレースホルダ／sqlc 生成コードのみ。文字列連結のSQL禁止
- [ ] **パスワード**：argon2id（または bcrypt cost≥12）。平文・可逆暗号は禁止
- [ ] **JWT**：alg を固定検証（`none` 拒否）、短い有効期限＋リフレッシュ、署名鍵は Secret 管理
- [ ] **通信**：本番は TLS 必須（compose の `sslmode=disable` は**ローカル開発限定**）
- [ ] **マルチテナント**：全クエリにテナント境界（RLS or 必須 tenant_id 条件）。越境を実装で防止
- [ ] **機微情報**：マイナンバー等はカラム暗号化/別ストア＋アクセスログ
- [ ] **秘密管理**：`.env` はコミットしない（`.gitignore`）。本番は Secret Manager
- [ ] **依存管理**：`govulncheck`（Go）/ `pnpm audit`（FE）を CI 必須化、Dependabot/renovate
- [ ] **入力検証**：validator/zod で全外部入力を検証。出力エスケープで XSS 防止
- [ ] **CORS/ヘッダ**：オリジン allowlist、セキュリティヘッダ付与
- [ ] **監査ログ**：認証・権限変更・機微データアクセスを記録（改ざん耐性）
- [ ] **レート制限**：認証・公開APIにレート制限
- [ ] **公開リポジトリ対策**：秘密/PII/トークンをコード・サンプル・ログ・**コミット履歴**に含めない。
      `.env` は `.gitignore`、サンプルは `.env.example`（プレースホルダ）のみ。`gitleaks`等で継続スキャン＋GitHub Push Protection 有効化
- [ ] **テスト/シードデータ**：実従業員・応募者・マイナンバー等を使わず、合成（ダミー）データのみ

> いずれもベストプラクティスの要約。最終的な安全性は実装・運用・最新の脆弱性情報で担保すること。

---

## 6. GORM × PostgreSQL RLS 実装ノート（重要）

GORM はコネクションプールを使うため、**「GORMで普通に書けば RLS が効く」わけではない**。
RLS を確実に効かせるには、リクエストごとに **同一トランザクション内でテナント変数を設定** する必要がある。

**設計の要点**
1. **RLS ポリシーをマイグレーションで定義**（テナント別テーブルに付与）：
   ```sql
   ALTER TABLE employees ENABLE ROW LEVEL SECURITY;
   ALTER TABLE employees FORCE ROW LEVEL SECURITY; -- テーブル所有者にも適用
   CREATE POLICY tenant_isolation ON employees
     USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
   ```
2. **アプリ用DBユーザーは非スーパーユーザー**にする（スーパーユーザー/BYPASSRLS はRLSを迂回するため厳禁）。
3. **リクエスト毎にトランザクションを張り、`SET LOCAL` でテナントを設定**してから全クエリを実行：
   ```go
   // 例：ミドルウェア/サービス層でトランザクションを開始し、テナントを束縛
   db.Transaction(func(tx *gorm.DB) error {
       // SET LOCAL はトランザクション終了で自動リセット＝コネクション汚染を防ぐ
       if err := tx.Exec("SET LOCAL app.tenant_id = ?", tenantID).Error; err != nil {
           return err
       }
       // 以降 tx を使ったクエリには RLS が適用される
       return businessLogic(tx)
   })
   ```
4. **`SET LOCAL` を使う**（`SET` ではなく）。トランザクション境界で自動リセットされ、プールの他リクエストに漏れない。
5. **GORM コールバック/共通ヘルパで強制**：開発者が個別に書き忘れても効くよう、
   「テナント付きトランザクション」を唯一の入口にする（生 `db` を業務層に直接渡さない）。
6. **テストで越境を必ず検証**：別テナントIDで他テナント行が取得/更新できないことを自動テスト化。

> 補足：RLS に頼りつつ、アプリ層でも `tenant_id` 条件を入れる「多層防御」が安全。
> RLS は最後の砦であり、アプリ側のスコープ漏れを完全に免責するものではない。

