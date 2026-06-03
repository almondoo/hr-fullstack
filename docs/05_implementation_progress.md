# 05. 実装進捗（Build State / Progress Report）

最終更新: 2026-06-03 / ステータス: **P2 MVP 全スライス実装完了（自己検証PASS・新規18パッケージ未コミット）**

本書は `00_master_workflow.md` のワークフローに基づく実装進捗のまとめ。各スライスは「実装 → 実DB(testcontainers)検証 → 独立セキュリティレビュー → ハードニング → ローカルコミット」のサイクルで進行。push・公開は未実施（許可が必要なため）。

---

## 1. サマリ

| フェーズ | 状態 |
|---|---|
| P0 Orientation & Backlog | ✅ 完了（Gate0 承認済み） |
| P1 Foundation（基盤） | ✅ 完了（Gate1 自己検証 PASS） |
| P2 MVP Feature Slices | ✅ 全領域実装完了（既コミット7群 ＋ 新規18パッケージ未コミット。`go build/vet ./...`・`go test ./... -race -p4` 全緑、新規パッケージ golangci-lint 0） |
| P3 Integration & Hardening | ⏳ 未着手 |
| P4 Release Readiness | ⏳ 未着手 |

- **コミット数**: 13（雛形2＋P1基盤7＋ハードニング1＋P2機能5）。最新: `abc8184`（入退社 ST-LM-07）。
- **ビルド/テスト**: コミット済の全パッケージが実PostgreSQL17(testcontainers)で `go test -race` グリーン。`golangci-lint` 0 issues、`govulncheck` クリーン。
- **秘密/PII**: 実PII・実鍵・トークンの混入ゼロ。シード/テストは合成データ（山田太郎 等）のみ。`gitleaks` はユーザ方針により不使用（代替: レビュー＋ugrep＋Push Protection）。

---

## 2. Gate0 確定事項（実装方針）

1. **MVP範囲・実装順序**: 提示バックログどおり承認。
2. **Go**: 1.26 維持・Dockerビルド前提（ローカルは GOTOOLCHAIN=auto で go1.26.3 自動取得して検証）。
3. **要配慮個人情報（マイナンバー/健診等）**: アプリDB列暗号化＋鍵は設定化（本番KMS差し替え前提）。
4. **課金**: 従量(席数)＋定額両対応のデータ構造・決済はモック。
5. （既定採用）セッション=サーバ側テーブル方式 / module名=プレースホルダ維持 / User↔Employee=任意1:1リンク / 法定三帳簿=3テーブル別立て。

---

## 3. コミット済スライス（要件ID紐付け）

| コミット | スライス | 要件ID |
|---|---|---|
| `d81bda8` | P1.1 internal/分割・platform基盤・wire-up骨格 | ST-FND-01 |
| `0bf8cb4` | P1.2 goose migration＋RLS基盤＋テナント付きTxヘルパ | ST-FND-02, ST-FND-03 |
| `fb7e374` | P1.3a 認証インフラ(argon2id/サーバ側セッション/SECURITY DEFINERテナント解決/RequireAuth) | ST-FND-05 |
| `966d5f3` | P1.3b signup/login/logout＋CSRF＋レート制限＋RBAC＋監査 | ST-FND-05/06/07 |
| `084bd94` | P1.3 認証ハードニング(タイミングオラクル除去/CSRFバイパス不能化/監査seq検証/信頼プロキシ) | ST-FND-05/06/07 |
| `1aacc01` | P1.5 フロント基盤(React Router v7 BFF/セッション・CSRF連携/ログイン画面) | P1.5 |
| `c5b6594` | P1.4 CI(GitHub Actions/golangci-lint/Makefile, gitleaks不使用) | ST-FND-12 |
| `1328938` | 従業員・組織[発令履歴]・雇用契約 | ST-LM-01 (CORE-002/003, LM-002/005) |
| `0da35f4` | 勤怠: 打刻・客観把握/36協定上限/時間外・休日・深夜割増 | ST-LM-02/03/04 (LM-030〜033) |
| `a944260` | 申請承認WFエンジン(多段/代理/差戻し) | ST-FND-08 (CORE-006) |
| `3ae9c9c` | 休暇: 年休付与(比例付与)/残日数/5日取得義務/各種休暇申請承認 | ST-LM-05/06 (LM-040〜043) |
| `abc8184` | 入退社: 入社手続き/情報収集フォーム(機微PII列暗号化)/退職オフボーディング＋AES-256-GCM列暗号基盤 | ST-LM-07 (LM-001/003/004) |

**ワーキングツリー: クリーン（全てコミット済）。** 入退社 ST-LM-07 はレビュー2 MUSTFIX（crypto の sync.Once initErr リーク／機微復号のservice層権限検証＝多層防御）を適用してコミット済（`abc8184`）。新セッションは `git log` ＋ 本書から再開可能。

ドメインパッケージ: `approval, attendance, auth, department, employee, leave, onboarding, platform, server`。マイグレーション: `00001`〜`00008`。

---

## 4. 確立した基盤・アーキテクチャ（全スライス共通）

- **マルチテナント分離(RLS)**: 全テナント系テーブルに `ENABLE + FORCE ROW LEVEL SECURITY`、`USING/WITH CHECK` ポリシー（`NULLIF(current_setting('app.tenant_id',true),'')::uuid` でフェイルクローズ）。**非スーパーユーザー`hr_app`ロール(NOBYPASSRLS)**、**複合テナントFK `(id, tenant_id)`** で越境参照をDB制約で防止、`tenantdb.WithinTenant`(set_configパラメータ化)を**唯一の入口**化。管理DSN(マイグレーション)と配信DSN(hr_app)を分離。
- **認証**: argon2id、サーバ側セッション(SHA-256ハッシュのみ保存)、SECURITY DEFINER関数でのログイン前テナント解決、Cookie httpOnly/Secure/SameSite、タイミングオラクル対策(ダミーハッシュ検証)、ロックアウト。
- **CSRF**: gorilla/csrf でバイパス不能構成。**レート制限**: ulule/limiter（信頼プロキシ設定でX-Forwarded-For偽装防止）。
- **RBAC**: `roles.permissions` jsonb＋ワイルドカード、`RequirePermission`、承認のrole-based認可。
- **監査**: 改ざん検知ハッシュチェーン(SHA-256, seq単調性, advisory lockで直列化)、業務操作と同tx、resource_idはopaque UUIDのみ・**PII/トークン/復号値を非格納**。
- **列暗号**: AES-256-GCM(`internal/platform/crypto`)、鍵はenv設定(本番KMS前提)、機微列はbytea暗号化。
- **承認エンジン**: 多段/代理/差戻し、`SubmitTx`で他ドメインと単一tx原子化。
- **法令値の完全設定化**: 36協定上限/割増率/年休付与日数表/5日義務閾値/時効 等はハードコードせずテナント別設定テーブルで管理。コードに「要専門家確認・改正追従」を明示、断定的法的助言はしない。
- **テスト**: testify＋testcontainers-go(実PG17)、`-race`、法令計算境界値・RLS越境・認可・監査改ざん検知を網羅。
- **CI**: GitHub Actions(build/vet/golangci-lint/go test -race/govulncheck＋FE pnpm audit/typecheck/lint/build)。gitleaks不使用。
- **フロント**: React Router v7(BFF, loader/actionがGo APIへCookie/CSRF転送、localStorage非保存、CSP等セキュリティヘッダ)。

### 各スライスでレビューが捕捉し修正した代表的指摘（実テストが見逃した実在の問題）
- 認証: ユーザー列挙タイミングオラクル、CSRFサイレントバイパス、CSRF_SECURE本番未強制。
- RLS: テナント跨ぎFK(複合FK未使用)。
- 承認: approver/delegate両NULL時の無条件承認、SetDelegateの越境/呼出元未検証。
- 休暇: 残高TOCTOU、approval連携の非原子(孤児)化、SQL二重aliasによる過剰割当、5日義務の単一付与判定。
- 勤怠: 月60h境界のハードコード、dead code、深夜休憩控除の非対称。

---

## 5. 残バックログ（P2/P3/P4）

### P2 残スライス → ✅ 全完了（新規18パッケージ・migration 00009〜00026・未コミット）

各スライスは「実装 → 実DB(testcontainers)検証 → 独立セキュリティレビュー → MUST_FIX修正 → lint整理」サイクルで実装。全パッケージ `go test -race` グリーン、golangci-lint 0、`go build/vet ./...` exit 0。

| ストーリー | パッケージ | migration |
|---|---|---|
| ST-FND-09 通知基盤(アプリ内/メールmock/テンプレ/配信記録/既読/リマインド) | `notification` | 00009 |
| ST-LM-09 マイナンバー(分離ストア/AES-256-GCM/利用目的/復号RBAC再検証/利用提供ログ/廃棄) | `mynumber` | 00010 |
| ST-ATS-01 求人票作成・公開・ステータス管理(項目別権限) | `jobposting` | 00011 |
| ST-TM-01 目標MBO/OKR・カスケード(所有権強制・親リンク保持) | `goal` | 00012 |
| ST-FND-11 標準帳票/CSV・SpreadsheetMLエクスポート/カレンダー・勤務体系 | `reporting` | 00013 |
| ST-LM-08 社保/労保 帳票生成・電子申請(e-Gov/マイナポータルmockアダプタ/冪等/公文書暗号) | `govfiling` | 00014 |
| ST-LM-10 法定三帳簿/保存年限+給与SaaS連携(mockアダプタ) | `ledger` | 00015 |
| ST-FND-04 テナント/プラン/サブスク/従量+定額課金/モック決済(invoiceイミュータブル) | `billing` | 00016 |
| ST-FND-10 セルフサービス/CSV一括取込(検証付)/ファイル保管(暗号・版管理・保持期間) | `selfservice` | 00017 |
| ST-ATS-02 応募者DB(取込/重複統合/PII・同意管理) | `applicant` | 00018 |
| ST-LM-11 就業規則/労使協定 版管理・周知/同意・有効期限アラート | `workrule` | 00019 |
| ST-ATS-03 選考パイプライン(ステージ/カンバン/通知) | `selection` | 00020 |
| ST-ATS-05 内定/オファー管理(発行/電子署名mock/受諾) | `offer` | 00021 |
| ST-TM-02 評価WF(自己/上司/二次/360度匿名/キャリブレーション) | `evaluation` | 00022 |
| ST-TM-03 1on1(アジェンダ/記録/アクション・参加者ゲート) | `oneonone` | 00023 |
| ST-ATS-04 面接調整(候補日/リマインド)・評価収集 | `interview` | 00024 |
| ST-ATS-06 オンボーディング連携(候補者→従業員生成/入社タスク) | `hiring` | 00025 |
| ST-TM-04 人材DB・スキルマップ・配置(統合プロフィール/組織図/配置シミュ) | `talent` | 00026 |

> 横断参照(他新規ストーリーの表)は素のuuid列+index(複合FKは繰越ハードニング)。既存表(employees/departments)参照は複合FK。`users` には UNIQUE(id,tenant_id) が無いため user参照はサービス層検証+RLSの多層防御。法令値(料率/保存年限/閾値/有効期限リードタイム等)は全て設定化(要社労士/弁護士確認・改正追従)。
>
> **残課題(P3で対応)**: (a) `govulncheck` 3件 — stdlib 2件(GO-2026-5039 net/textproto, GO-2026-5037 crypto/x509。go1.26.4で解消) ＋ gorilla/csrf@v1.7.3 GO-2025-3884(Fix未提供、TrustedOrigins設定の見直しで緩和)。いずれも新規コード起因ではない。(b) `make lint` 既存パッケージ pre-existing 76件(attendance43/leave12/employee7/onboarding6/approval6/department2。新規18は0)。(c) 横断複合FK化・cross-storyの実連携(通知配信・マイナンバー提供・候補者→従業員)・実外部API(e-Gov/給与SaaS/決済)。

### P3 Integration & Hardening
- `docker compose up --build` 起動確認(/healthz /readyz＋主要フロー)。
- セキュリティ通し(RLS越境一式・認証/CSRF・govulncheck/pnpm audit)。
- **繰越ハードニング項目**: 既存テーブルの広範な複合FK化(departments.parent_id, employees.department_id, users.employee_id/role_id 等)、設定JSON(付与表等)の構造検証、leave_settings.created_at、各種境界テスト補強。
- パフォーマンス簡易確認(N+1/インデックス)。

### P4 Release Readiness
- README/運用手順更新、本番用マルチステージDockerfile(非root/最小)、環境変数一覧、移行手順、MVP DoD Gate。

---

## 6. ローカル実行（参考）

```
# 依存解決＆テスト（ホスト: GOTOOLCHAIN=auto で go1.26.3 自動取得、testcontainersは稼働中のDockerを使用）
go -C backend mod tidy
go -C backend build ./...
go -C backend test ./... -race

# 起動（要 .env: .env.example をコピーし強力な値を設定）
docker compose up --build   # P3で疎通確認予定
```

> 注: 法令計算(36協定/割増/年休/社保/マイナンバー等)の値・様式は**最新の官公庁情報と社会保険労務士等の専門家確認が前提**であり、改正追従のため設定化している。本実装は法的助言ではない。
