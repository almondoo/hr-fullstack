# hr-fullstack

中小企業（SMB）向けに外販する **HR SaaS** を、
**Claude の dynamic workflow で「要件定義 → 設計 → 実装計画」まで** 進め、
**Go + PostgreSQL** で実装するための OSS プロジェクト雛形です。

対象HR領域：人事労務（入退社・社保・勤怠・休暇）／人事評価・タレントマネジメント（目標・1on1・評価・スキル）／採用管理(ATS)・オンボーディング。

> ⚠️ **このリポジトリは public 公開前提です。** 個人情報（PII）・APIトークン・パスワード・秘密鍵・
> 本番接続情報は **絶対にコミットしないでください**（履歴に残り、削除しても復元され得ます）。
> 秘密は環境変数 / Secret Manager で注入し、サンプルはプレースホルダ、テストは合成データのみ。
> 詳細・公開前チェックリストは **[SECURITY.md](./SECURITY.md)**。

---

## このリポジトリの2つの役割

1. **設計支援（dynamic workflow プロンプト）**
   `00_master_workflow.md` を Claude に貼り付け、`docs/01〜04` を参照して
   要件 → 設計 → 実装計画をフェーズゲート方式で生成します。
2. **実装スキャフォールド**
   `docker-compose.yml` ＋ `backend/`（Go + Gin + Air / PostgreSQL）。最小起動構成。

---

## ディレクトリ構成

```
hr-fullstack/
├─ 00_master_workflow.md     # ← 会話に貼り付ける dynamic workflow 本体
├─ README.md
├─ SECURITY.md               # 公開リポジトリの秘密/PII方針・チェックリスト
├─ .gitignore                # 秘密/PII を除外（検証済み）
├─ .env.example              # プレースホルダのみ。コピーして .env を作成
├─ docker-compose.yml        # api(Go) + db(PostgreSQL)
├─ docs/                     # ← Claude がフェーズ毎に参照する4ファイル
│  ├─ 01_domain_knowledge.md #   要件カタログ（機能/法令/非機能/連携）
│  ├─ 02_phase_playbooks.md  #   各フェーズの手順・ゲート条件
│  ├─ 03_output_templates.md #   成果物テンプレ（要件/設計/実装計画＋台帳）
│  └─ 04_tech_stack.md       #   確定スタック・安全ライブラリ・GORM×RLS実装ノート
└─ backend/
   ├─ Dockerfile.dev / .air.toml / .dockerignore
   ├─ go.mod
   └─ main.go                # /healthz, /readyz（DB接続込み）
```

---

## 使い方A：設計を進める（dynamic workflow）

1. 新しい会話で **`00_master_workflow.md` の全文を貼り付け**る。
2. Claude が `docs/` を読める環境かを確認し、Phase 0 の質問を返す → 回答。
3. 各フェーズ末尾の **ゲート** で `承認` / `修正: …` / `戻る: …` を返す。
4. Phase 1→2→3 で、要件定義書・設計書・実装計画書が順に生成される。

Claude は各フェーズに入った時だけ `docs/` の該当節を読み込みます（一括展開しない＝抜け漏れ防止）。

## 使い方B：アプリを起動する

```bash
cp .env.example .env
# .env の POSTGRES_PASSWORD を強力な値に（例: openssl rand -base64 24）

docker compose up --build
# 確認
curl http://localhost:8080/healthz   # {"status":"ok"}
curl http://localhost:8080/readyz    # {"status":"ready"}（DB接続OK時）
```

初回は `go mod tidy` が gin / gorm を解決し、Air がホットリロードで起動します。

---

## 確定アーキ判断（`docs/04 > 1.1`）

| 項目 | 決定 |
|---|---|
| バックエンド | Go ＋ Gin、開発は Air（`golang:1.26`） |
| フロントエンド | React Router v7（Remix）＋ pnpm（Node 22 LTS） |
| DB | PostgreSQL（`postgres:17`） |
| DBアクセス層 | GORM（内部 pgx/v5） |
| マルチテナント分離 | PostgreSQL RLS + tenant_id（`SET LOCAL` をトランザクションで強制） |
| 認証 | セッションCookie（httpOnly+Secure+SameSite、CSRF必須） |
| デプロイ先 | 未定（Phase 2/3 で決定） |

> **GORM × RLS は「書けば守られる」構成ではありません。** 全テナント系クエリを
> 「テナント付きトランザクション」経由に限定する実装が必須です（`docs/04 > 6`）。

---

## セキュリティ / 公開前

- 秘密は環境変数 / Secret Manager。`.env` はコミットしない（`.gitignore` 済み）。
- gitleaks は使わず、**レビュー＋ugrep＋GitHub Push Protection** で秘密混入を防ぐ（詳細は SECURITY.md）。
- 公開前に GitHub の **Secret scanning + Push protection** を有効化すること（Settings → Code security）。
- CI に `govulncheck`（Go）/ `pnpm audit`（FE）を組込み済み（`.github/workflows/ci.yml`）。
- 法令・制度の記述は 2025–2026 時点の一般情報の要約。実装時は社労士・弁護士と一次情報で確認。

詳細は [SECURITY.md](./SECURITY.md)。

## 次のステップ

- 最小権限DBロール＋ RLS マイグレーション（goose）＋ GORM「テナント付きトランザクション」ヘルパ
- `internal/`（tenant / employee / attendance …）とドメイン境界
- 認証（セッション・argon2id・CSRF）と RBAC ミドルウェア
- `frontend/`（React Router v7 + pnpm）。必要なら compose に `web` サービス追加
