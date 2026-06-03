# 06. 残タスク台帳 / 次セッション ハンドオフ

> **目的**: 文脈ゼロの新セッションが、この1ファイルを起点に**抜け漏れなく即着手**できること。
> 最終更新: 2026-06-03 / 現在地: **P2 全実装完了（コミット済）→ P3 Integration & Hardening 途中**。

---

## 0. 次セッション クイックスタート（まずこれを読む）

1. `git log --oneline -12` で直近コミットを確認（最新 HEAD は `6d0f735` 系の `[P3]`）。
2. このファイル `docs/06_remaining_tasks.md`（残タスク）と `docs/05_implementation_progress.md`（進捗詳細）を読む。
3. 要件の根拠は `docs/01_domain_knowledge.md`（機能カタログ）、確定スタックは `docs/04_tech_stack.md`、ワークフロー全体は `00_master_workflow.md`。
4. **自動ロードされるメモリ**（`~/.claude/projects/.../memory/MEMORY.md` 経由）に従う:
   - `dynamic-workflow-docker-throttle` — Workflowでtestcontainersを乱立させない
   - `test-with-shared-database` — テストはDBを1つ追加(共有)して実行
   - `workflow-model-selection` — 実行系=Sonnet / 計画・レビュー=Opus
   - `no-gitleaks` — gitleaks不使用（代替: レビュー＋ugrep＋Push Protection）
5. §2 の環境・制約を確認 → §3 の残タスクから優先度順に着手。

---

## 1. 現在地（Done / コミット済）

`28ce718`(従前) 以降、本作業で **8コミット**追加（main、push未実施・ローカルのみ）:

| commit | 内容 |
|---|---|
| `59b4122` | EP-FND: notification / billing / selfservice / reporting [ST-FND-09/04/10/11] |
| `405ba75` | EP-LM: mynumber / govfiling / ledger / workrule [ST-LM-09/08/10/11] |
| `f5c730d` | EP-ATS: jobposting / applicant / selection / interview / offer / hiring [ST-ATS-01..06] |
| `be3c207` | EP-TM: goal / evaluation / oneonone / talent [ST-TM-01..04] |
| `e7a1072` | server.go に18ドメイン配線 + docs/05更新 [P2] |
| `4baa1f7` | 脆弱性: go1.26.4 bump(stdlib2件解消) + gorilla/csrf緩和文書化 [P3] |
| `ccdb374` | 既存6パッケージ lint 0化（挙動不変） [P3] |
| `6d0f735` | 横断複合FKハードニング migration `00027` [P3] |

- **P2 残18ストーリー = 全実装完了**（internal 27パッケージ / migration 27本 = `00001`〜`00027`）。
- 各スライス: 実装 → testcontainers実DB検証 → 敵対的セキュリティレビュー → MUST_FIX修正 → lint整理。
- 基盤: RLS(ENABLE+FORCE+`NULLIF(current_setting('app.tenant_id',true),'')::uuid` fail-closed) / 複合FK(id,tenant_id) / `tenantdb.WithinTenant` / 監査ハッシュチェーン(PII非格納) / 機微列AES-256-GCM+RBAC再検証 / 法令値の設定化。

### 確定済み検証（Docker不要）
- `golangci-lint` = **0**（全39パッケージ）/ `go build ./...` `go vet ./...` = **exit 0**
- `govulncheck` = **1残**（`GO-2025-3884` gorilla/csrf@v1.7.3、上流に修正版なし＝緩和済み・許容。§3 T8参照）

---

## 2. 環境・制約（必読）

- **モジュール**: `github.com/your-org/hr-saas` / **Go 1.26.4**（go.mod。ホストは goenv 1.25.7 のため **コマンドは必ず `GOTOOLCHAIN=auto` を付ける**）。
- **ビルド/テスト/lint**（backend ディレクトリ基準）:
  ```
  GOTOOLCHAIN=auto go -C <repo>/backend build ./...
  GOTOOLCHAIN=auto go -C <repo>/backend vet ./...
  GOTOOLCHAIN=auto go -C <repo>/backend test ./internal/<pkg>/... -race      # 単一パッケージ
  make -C <repo>/backend lint        # golangci-lint（.golangci.yml: misspell locale=UK）
  make -C <repo>/backend vuln        # govulncheck（未導入なら go run golang.org/x/vuln/cmd/govulncheck@latest ./...）
  ```
- **🐳 Docker/testcontainers（重要・実害あり）**: テストは `internal/platform/testdb.NewHarness` が**パッケージごとにPostgreSQL17コンテナを起動**する。並列で多数走らせると20本超のコンテナが乱立しDockerを食い潰す（ユーザーがDocker停止に追い込まれた）。**Workflow利用時は「並列フェーズ=build/vet/lintのみ・testは最後に単一フェーズで `go test -p 1`(同時1コンテナ)」を厳守**。根本対策は §3 T2（testdb共有DB化）。
- **コミット方針**: ユーザー承認済みで **main に直接コミットして可**（commit-per-feature・`[ST-xx]`/`[P3]` タグ）。**push は許可があるまでしない**。
- **権限の落とし穴（メインスレッドのBash）**: `>` リダイレクトと `cd ...` 始まりの複合コマンドは権限denyでブロックされる。**絶対パス＋`go -C`/`make -C` を使い、`>`は使わない**（ファイル生成は Write ツール）。サブエージェントは `cd` 等を自由に使える。
- **要件詳細の所在**: 各ストーリーの構造化仕様は本作業中 `tmp/specs/_understand.json` に生成したが **tmp/ はgitignore・揮発性**。権威ある出典は `docs/01`＋`docs/05`。必要なら新セッションで understand 系Workflowを再生成（ただしテストは走らせない設計で）。

---

## 3. 残タスク（優先度順・各タスク自己完結）

### T1. 集約テストの最終1回再確認（検証債務・最優先）
- **背景**: P3の各フェーズではDocker稼働中にフルスイート緑を取得済みだが、最後の集約再確認だけ「ユーザーがDocker停止」と重なり未実施。コードは緑の証跡あり。
- **手順**: Docker安定を確認 → `GOTOOLCHAIN=auto go -C <repo>/backend test ./... -race -p 1 -timeout 1800s -count=1`（**`-p 1`で同時1コンテナ＝低負荷**、約15-25分）。
- **受入**: exit 0。実失敗が出たら修正（00027のFK制約と既存seedの整合に注意）。
- **gotcha**: `-p 1` は遅いがDockerに優しい。`-p 4`以上は乱立するので**使わない**。

### T2. `testdb` 共有DB化リファクタ（Docker根本対策）
- **目的**: パッケージごとの使い捨てコンテナ乱立をやめ、**DBを1つ追加(共有)**してそこへ全テストを接続させる（メモリ `test-with-shared-database`）。
- **着手前にユーザー確認**: 機構を選ぶ ①docker compose の `db` サービス利用 ②単一共有コンテナ(`WithReuse`) ③ローカルPostgres。
- **対象**: `backend/internal/platform/testdb/testdb.go` の `NewHarness`。テスト間分離は**コンテナ使い捨てでなく**別スキーマ/別database作成 or トランザクションロールバックで担保。**`hr_app`(NOBYPASSRLS)ロール＋RLS(`SET LOCAL app.tenant_id`)の意味論を必ず保持**。
- **受入**: フルスイート緑が**コンテナ最大1〜2本**で完走。RLS越境/監査PII非混入/FK越境の検証カバレッジを維持。
- 完了後 T1 とT8の再評価が容易になる。

### T3. cross-story 内部連携（unblocked・機能）
現状、新規18パッケージは互いに**Go import せず uuid参照列のみ**で疎結合。以下の実連携が未実装:
- **通知配信フック**: `approval`/`leave`/`billing`/`govfiling`/`workrule` 等のイベント→ `notification.Publish`。**結合方式を設計**（推奨: 各ドメインが outbox 行をINSERT → notification が処理、で疎結合維持。直接importは循環/結合増に注意）。
- **マイナンバー→社保提供**: `govfiling` が届出時に `mynumber` から番号を**利用提供ログ付きで取得**するフロー（現状は payload参照のみ）。復号値をDB/ログ/監査に残さない原則を厳守。
- **候補者→従業員生成**: `hiring`(ATS-06) がオファー受諾候補者から `employee`/`onboarding` を生成。
- **進め方**: Docker絞り版 ultracode Workflow（並列=build/vet、test=最後に-p1）か feature-dev。

### T4. フロントエンド機能画面（feature-dev推奨・要スコープ確認）
- **現状**: フロントは React Router v7(Remix) BFF で**ログイン画面のみ**。18機能のUIは未実装。
- **進め方**: **feature-dev** で「どの機能から・どの深さで作るか」をスコープ確認 → 設計 → 実装。
- **パターン**（既存）: ルート登録 `frontend/app/routes.ts`、ルートモジュール `frontend/app/routes/*.tsx`(loader/action同居)、API中継 `frontend/app/lib/api.server.ts`(Cookie/CSRF転送・`fetchCsrfToken`→`X-CSRF-Token`)、CSP等は `frontend/app/root.tsx` の `headers` で一括適用。参考実装: `login.tsx` / `dashboard.tsx`。`connect-src 'self'` のためAPIは必ずloader/action経由(BFF)。
- **要ユーザー判断**: 優先機能・MVP範囲。

### T5. P3 疎通・性能確認（docs/05 §5 P3）
- `docker compose up --build` 起動確認: `/healthz` `/readyz` ＋ 主要フロー（login→主要API）。**コンテナ常駐で軽い**ので Docker負荷は問題なし。
- セキュリティ通し: RLS越境一式・認証/CSRF・`govulncheck`/`pnpm audit`。
- 性能: N+1 / インデックス簡易確認（新規18パッケージのListクエリ中心）。

### T6. P4 Release Readiness（docs/05 §5 P4）
- README/運用手順更新、**本番用マルチステージ Dockerfile（非root・最小)**、環境変数一覧（`.env.example` と整合）、移行手順（goose `00001`〜`00027`）、MVP DoD Gate。

### T7. 実外部API連携（🚫 ブロック中・認証情報待ち）
- e-Gov/マイナポータル（社保電子申請）、給与SaaS（マネーフォワード/freee/弥生）、決済（Stripe等）。**現状は全てモックアダプタ**（`govfiling`/`ledger`/`billing` 内にインタフェース定義済み）。
- **ブロック理由**: sandbox認証情報/アカウントが必要。**取得でき次第**、各アダプタのモック実装を実呼び出しに差し替え（インタフェースは既存のまま）。

### T8. 残ハードニング
- **00027で見送った cross-story 複合FK 6件**（各テストのseedが最小データのため過剰制約になり見送り。**T2の共有DB化後にseed整備すれば張れる可能性**）。実テーブル/列名は該当migrationで要確認:
  - `selection_stages.job_posting_id` → `job_postings(id,tenant_id)`（00020）
  - `applications.job_posting_id` / `applications.applicant_id`（00018/00020）
  - `offers.application_id` → `applications`（00021）
  - `interviews.application_id` → `applications`（00024）
  - `reviews.cycle_id` / `calibration_sessions.cycle_id` → `review_cycles`（00022）
  - `notifications.recipient_user_id` → `users(id,tenant_id)`（00009。**users に UNIQUE(id,tenant_id) は00027で追加済み**＝単純FK→複合FK昇格が可能）
- docs/05 §5 既載の繰越: 設定JSON(付与表等)の構造検証、`leave_settings.created_at`、各種境界テスト補強。
- **gorilla/csrf `GO-2025-3884`**: 上流に修正版が出たら go.mod を bump（現状は `server.go` の `csrf.Protect` 付近に緩和コメントあり＝`parseCORSOriginHosts` が完全一致hostのみ・ワイルドカード無しで非該当と判断）。

---

## 4. 着手の型（参考）

- **ultracode/Workflow を使うとき**: 並列フェーズは `go build`/`go vet`/`golangci-lint` のみ。testcontainers テストは**最後の単一エージェント**で `go test ./... -p 1`。複数エージェントに同時 `go test` を絶対させない（メモリ `dynamic-workflow-docker-throttle`）。モデルは実行=Sonnet / レビュー・計画=Opus（メモリ `workflow-model-selection`）。
- **新ドメイン追加の型**: `db/migrations/000NN_<name>.sql`(RLS+複合FK+GRANT hr_app) → `internal/<pkg>/{model,service,handler,routes}.go` → `internal/server/server.go` に `RegisterRoutes(v1, deps.TenantDB, requireAuth)` 1行。手本: `internal/onboarding`（crypto+複合FK+audit）/ `internal/leave`（approval連携）。
- **コミット**: main直、`feat(scope): ... [ST-xx]`、push禁止。

---

## 5. 推奨着手順（次セッション）

1. **T2（testdb共有DB化）** を機構決定のうえ実施 → Docker問題を恒久解消。
2. **T1（集約テスト -p1）** で全緑を確定。
3. **T3（cross-story連携）** または **T4（フロント, feature-dev）** をユーザー優先度で。
4. **T5→T6** で疎通・リリース準備、**T8** ハードニング。
5. **T7** は認証情報入手後。
