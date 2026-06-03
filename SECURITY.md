# セキュリティ / 公開リポジトリの注意

> **このリポジトリは public（一般公開）です。** 誰でも全ファイルとコミット履歴を閲覧できます。
> 個人情報（PII）・秘密情報・トークンは **1度でもコミットすると履歴に残り、削除しても復元され得ます**。
> 「載せない」ことが唯一の防御です。

---

## 絶対にコミットしないもの

- **認証情報 / 秘密鍵**：APIトークン、APIキー、パスワード、`.env`、OAuthクライアントシークレット、
  JWT署名鍵、SSH/TLS秘密鍵（`*.pem` `*.key`）、クラウド認証情報（AWS/GCP/Azure）、`.npmrc` のトークン
- **本番・実環境の接続情報**：本番DBのホスト/ユーザー/パスワード、内部URL、社内エンドポイント
- **実在する個人情報（PII）**：実際の従業員・応募者の氏名/住所/連絡先/口座/評価、**マイナンバー**、
  健康・社会保険情報など。HR領域は特に機微なため、**テスト/シード/サンプルにも実データを使わない**
- **顧客データ**：取引先・利用企業の実データ

## 代わりにすること

- 秘密は **環境変数 / Secret Manager** で注入。リポジトリには `.env.example`（**プレースホルダのみ**）だけを置く
- テスト・シード・スクリーンショットは **合成（ダミー）データ** のみ。`山田太郎`等の明らかな架空値を使う
- ドキュメントやIssue/PRにもログ断片や実IDを貼らない（マスキングする）
- ログに PII・トークンを出力しない（本スケルトンの `requestLogger` はメソッド/パス/ステータスのみ記録）

## 公開前チェックリスト

- [ ] `.env` がコミット対象に入っていない（`git status` で確認、`.gitignore` 済み）
- [ ] `.env.example` の値がすべてプレースホルダ（実値が無い）
- [ ] ソース・設定・テスト・fixtureに実PII / 実トークン / 本番接続情報が無い
- [ ] コミット履歴にも秘密が含まれていない（過去コミットも対象）
- [ ] GitHub の **Secret scanning + Push protection** を有効化（Settings → Code security）
- [ ] CI に依存脆弱性チェック（`govulncheck` / `pnpm audit`）を組込み済み（`.github/workflows/ci.yml` 参照）
- [ ] 必要に応じ `ugrep` でローカルパターン検索を実施
- [ ] `pre-commit` フックでコミット前スキャン（任意だが推奨）

## 秘密混入防止方針

本プロジェクトでは **gitleaks は使用しない**。代わりに以下の三層で秘密の混入を防ぐ。

| 層 | 手段 | タイミング |
|---|---|---|
| 1. コードレビュー | PR レビュー時に目視確認 | push 前後 |
| 2. ローカル検索 | `ugrep -r` でパターン検索 | コミット前（任意） |
| 3. GitHub 機能 | Secret scanning + Push Protection | push 時（自動） |

### GitHub Secret scanning + Push Protection の有効化（必須）

リポジトリを公開する前に、リポジトリ管理者が以下を行うこと：

1. GitHub リポジトリの **Settings → Code security** を開く
2. **Secret scanning** を ON にする
3. **Push protection** を ON にする

Push Protection が有効な場合、GitHub が既知のシークレットパターンを検出すると push がブロックされる。

### ローカルでのパターン確認例（ugrep）

```bash
# API キーやトークンらしき文字列を検索
ugrep -r --include='*.go' --include='*.ts' --include='*.env*' \
  '[A-Za-z0-9_]{20,}' . | grep -v '.env.example'
```

### 依存脆弱性チェック

```bash
# Go
cd backend && go run golang.org/x/vuln/cmd/govulncheck@latest ./...
# Frontend
cd frontend && pnpm audit --audit-level=high
```

## 誤って秘密をコミット/公開してしまったら

1. **その秘密を即時ローテーション/無効化**（鍵・トークンを再発行）。履歴から消すより先にこれを行う
2. 履歴から除去：`git filter-repo`（推奨）または BFG Repo-Cleaner で該当ファイル/文字列を削除し、強制プッシュ
3. 影響範囲を確認（アクセスログ、不正利用の有無）
4. 公開済みの秘密は「漏洩したもの」として扱う（削除＝安全ではない）

## 脆弱性の報告

セキュリティ上の問題を見つけた場合は、公開Issueではなく非公開で連絡してください（連絡先は各自で設定）。
例：`SECURITY` の連絡先メール、または GitHub の Private vulnerability reporting を有効化。

---

## 脆弱性開示ポリシー (Vulnerability Disclosure Policy)

### 対象範囲（スコープ）

**対象（In-scope）**
- 本リポジトリのバックエンド（Go / Gin）コード
- フロントエンド（React Router v7 / Remix）コード
- データベーススキーマ・マイグレーションスクリプト（PostgreSQL RLS、暗号化設計）
- 認証・セッション・CSRF 実装
- Docker / CI 設定

**対象外（Out-of-scope）**
- 利用している OSS ライブラリ自体（上流プロジェクトに直接報告してください）
- 本番インフラ環境（もし存在する場合）のネットワーク侵入テスト・DDoS 試験
- 旧バージョンのブランチに固有の問題（最新 `main` で再現しないもの）

---

### 報告方法

**推奨: GitHub Private Vulnerability Reporting**

GitHub の「Private vulnerability reporting」機能を有効化しています（未有効化の場合は次節を参照）。
以下の URL から非公開で報告してください：

```
https://github.com/almondoo/hr-fullstack/security/advisories/new
```

**代替: メール**

GitHub アカウントをお持ちでない場合は、次のアドレスへご連絡ください:

```
security@example.com
```
> **注意**: この連絡先はプレースホルダです。リポジトリ管理者は本番公開前に実際のアドレスに置き換えてください。

---

### 報告時に含めてほしい情報

- 脆弱性の概要（何が問題で、どのような影響があるか）
- 再現手順（具体的なステップ、コードスニペット、cURL コマンドなど）
- 影響を受けるコンポーネント・ファイル・バージョン
- 可能であれば PoC（概念実証）または スクリーンショット
- 重大度の自己評価（任意）

---

### 対応 SLA 目安

| フェーズ | 目標時間 |
|---|---|
| 受領確認（acknowledgement） | 報告から **72 時間以内** |
| トリアージ完了（severity 判定） | 受領から **7 日以内** |
| 修正リリース（Critical / High） | トリアージから **30 日以内** |
| 修正リリース（Medium / Low） | トリアージから **90 日以内** |
| 公開開示（coordinated disclosure） | 修正リリースから **14 日以内**（報告者との合意のうえ） |

> SLA は目標値であり保証ではありません。影響範囲や上流依存に応じて変動する場合があります。

---

### 対応フロー

```
報告受領
  └─ 受領確認 (72h以内)
       └─ トリアージ・重大度判定 (7日以内)
            ├─ 修正実装・PR (非公開ブランチ)
            ├─ セキュリティアドバイザリ草稿
            └─ リリース・開示
                 └─ CVE 申請（必要に応じて GitHub 経由）
```

---

### セーフハーバー (Safe Harbour)

善意に基づく脆弱性調査・報告を歓迎します。以下の条件を満たす場合は、法的措置を取りません。

- 実際のユーザーデータや本番システムへの影響を最小化している
- 発見した脆弱性を第三者に公開・悪用していない
- このポリシーに従い報告している

---

### GitHub Private Vulnerability Reporting の有効化手順（管理者向け）

本番公開前にリポジトリ管理者が以下の操作を行ってください：

1. GitHub リポジトリの **Settings** を開く
2. **Code security** セクションへ移動
3. **Private vulnerability reporting** を **Enable** にする

有効化後は、外部からのセキュリティ報告が非公開の Draft Security Advisory として届くようになります。
