# e-Gov 36協定 電子届出 連携 Runbook

> **法令注記・免責**: e-Gov 電子申請の利用登録手順・認証方式・申請様式の法的解釈は、
> e-Gov 担当者および**社会保険労務士・弁護士による一次法令源との確認が前提**です。
> 本ドキュメントは法的助言ではありません。実運用前に必ず専門家の確認を取得してください。
>
> **令和3年4月1日以降の確認済み事実**: 36協定届(様式第9号系)の e-Gov 電子申請において、
> 電子署名・電子証明書は**不要**です（令和3年4月1日以降、デジタル改革関連法により廃止）。
> 出典: 厚生労働省 <https://www.mhlw.go.jp/stf/seisakunitsuite/bunya/0000184033.html>
> / <https://www.mhlw.go.jp/content/11200000/000919894.pdf>
>
> **openQuestion**: GビズID・e-Govアカウント等の具体的な認証方式の詳細、
> 労働者代表選任要件（過半数労働組合/過半数代表者の要件）については要追加確認。

## 目次

1. [外部前提 (取得・確認が必要なもの)](#1-外部前提)
2. [現在の実装状態](#2-現在の実装状態)
3. [HTTPクライアント堅牢化 (実装済み)](#3-httpクライアント堅牢化)
4. [環境変数の設定](#4-環境変数の設定)
5. [実アダプタへの切替手順](#5-実アダプタへの切替手順)
6. [残 TODO の場所](#6-残-todo-の場所)
7. [sandbox 疎通確認チェックリスト](#7-sandbox-疎通確認チェックリスト)

---

## 1. 外部前提

### 1.1 e-Gov Developer Portal アカウント申請

| 手順 | 参照先 |
|------|--------|
| サンドボックスアカウント申請 | https://developer.e-gov.go.jp/ |
| APIキー (ClientID / ClientSecret) 発行 | 上記ポータルの申請フロー |
| サンドボックス BaseURL の確認 | ポータルのドキュメントで確認 (現時点で URL 仕様非公開) |
| 本番 BaseURL の確認 | 同上 |
| 36協定電子申請 本番サービス | https://shinsei.e-gov.go.jp/ |
| 厚生労働省 36協定電子申請案内 | https://www.mhlw.go.jp/stf/seisakunitsuite/bunya/0000184033.html |

> **注意**: サンドボックス / 本番のエンドポイント URL・認証方式 (OAuth2 フロー種別,
> トークンエンドポイント, スコープ) はポータルのドキュメントで確認すること。
> 本 runbook には仮値を記載しない。

### 1.2 様式・認証方式・電子署名の確認事項

- 36協定届出様式 (様式第9号 / 特別条項含む) の電子申請形式 (XML / JSON) は
  e-Gov ポータルおよび社労士との確認が必要。
- **電子署名（電子証明書）について**: 令和3年4月1日以降、36協定の e-Gov 電子申請に
  電子署名・電子証明書は**不要**です（デジタル改革関連法による改正）。
  出典: 厚生労働省 <https://www.mhlw.go.jp/stf/seisakunitsuite/bunya/0000184033.html>
  もし既存設定・コメントに「電子署名が必要」と記載していた場合は**令和3年4月以降不要**
  に訂正してください。
- **本社一括届出**: 同一内容要件あり。2025年3月31日の労働条件ポータル新チャネルでは
  事業場間同一内容であれば可へ緩和。従来チャネルは本社・各事業場同一要件継続。
  出典: 厚生労働省 <https://www.mhlw.go.jp/new-info/kobetu/roudou/gyousei/kantoku/dl/130419-1a.pdf>
- **openQuestion**: GビズID・e-Govアカウント等の具体的な認証方式の詳細は要追加確認。
  `EGovRealConfig` への認証フィールド追加は確認後に実装すること。

### 1.3 必要な環境変数 (認証情報)

`.env.example` の `e-Gov 電子申請` セクションを参照。実値は絶対にコミットしないこと。

| 環境変数 | 用途 |
|----------|------|
| `EGOV_BASE_URL` | API の BaseURL (サンドボックス / 本番) |
| `EGOV_CLIENT_ID` | OAuth2 クライアント ID |
| `EGOV_CLIENT_SECRET` | OAuth2 クライアントシークレット |
| `EGOV_SANDBOX_MODE` | `true` = サンドボックス、`false` = 本番 |

---

## 2. 現在の実装状態

| コンポーネント | 状態 | ファイル |
|----------------|------|---------|
| `EGovSubmitter` インターフェース | 実装済み | `backend/internal/govfiling/egov_adapter.go` |
| `EGovStubSubmitter` (デフォルト) | 実装済み・テスト用 | `backend/internal/govfiling/egov_adapter.go` |
| `eGovRealAdapter` — HTTP クライアント初期化 | 実装済み (TLS/タイムアウト) | `backend/internal/govfiling/egov_real_adapter.go` |
| `eGovRealAdapter.SubmitArticle36` | **未実装** (認証情報待ち) | 同上 |
| `eGovRealAdapter.PollArticle36Status` | **未実装** (認証情報待ち) | 同上 |
| `RegisterRoutes` での切替ポイント | `WithEGovSubmitter` | `backend/internal/govfiling/routes.go` |

現在のデフォルトはスタブ (`NewEGovStubSubmitter()`)。
実アダプタは認証情報取得後に切り替える (手順は §5 参照)。

---

## 3. HTTPクライアント堅牢化

`NewEGovRealAdapter` が返す `eGovRealAdapter` は以下を保証する:

- **TLS 証明書検証有効**: `net/http` のデフォルト Transport はシステム信頼ストアを使用。
  `InsecureSkipVerify` は設定しない (セキュリティポリシー違反のため禁止)。
- **タイムアウト 30 秒**: `http.Client.Timeout = 30 * time.Second`。
  e-Gov API の応答遅延でリクエストが無限にブロックしない。
- **OAuth2 Transport**: 認証情報取得後に `TODO(real-egov)` を実装し、
  `oauth2.Transport` を `http.Client.Transport` に設定する。

---

## 4. 環境変数の設定

```bash
# .env (コミット禁止) または Secret Manager で設定
EGOV_SANDBOX_MODE=true
EGOV_BASE_URL=<e-Gov ポータルで確認したサンドボックス URL>
EGOV_CLIENT_ID=<申請後に発行される Client ID>
EGOV_CLIENT_SECRET=<申請後に発行される Client Secret>
```

---

## 5. 実アダプタへの切替手順

1. §1 の外部前提をすべて満たしていることを確認する。
2. 環境変数を §4 に従い Secret Manager に登録する。
3. `backend/internal/govfiling/egov_real_adapter.go` の `TODO(real-egov)` を実装する
   (OAuth2 トークン取得・申請ボディ構築・レスポンス解析)。
4. `backend/internal/govfiling/routes.go` の `RegisterRoutes` を修正し、
   スタブの代わりに実アダプタを渡す:

   ```go
   adapter, err := govfiling.NewEGovRealAdapter(govfiling.EGovRealConfig{
       SandboxMode:  os.Getenv("EGOV_SANDBOX_MODE") == "true",
       BaseURL:      os.Getenv("EGOV_BASE_URL"),
       ClientID:     os.Getenv("EGOV_CLIENT_ID"),
       ClientSecret: os.Getenv("EGOV_CLIENT_SECRET"),
   })
   if err != nil {
       // 起動時エラー: 認証情報が揃っていなければ起動を拒否する
       return err
   }
   svc = svc.WithEGovSubmitter(adapter)
   ```

5. サンドボックスで §7 のチェックリストを実行する。
6. 問題がなければ `EGOV_SANDBOX_MODE=false` に切り替え、本番認証情報を注入する。

---

## 6. 残 TODO の場所

| ファイル | 関数 / 箇所 | 内容 |
|----------|------------|------|
| `backend/internal/govfiling/egov_real_adapter.go` | `NewEGovRealAdapter` | `TODO(real-egov)`: OAuth2 Transport 追加 |
| `backend/internal/govfiling/egov_real_adapter.go` | `SubmitArticle36` | `TODO(real-egov)`: トークン取得・申請ボディ送信・受付番号取得 |
| `backend/internal/govfiling/egov_real_adapter.go` | `PollArticle36Status` | `TODO(real-egov)`: ステータスポーリング・ステータス文字列マッピング |
| `backend/internal/govfiling/egov_real_adapter.go` | `EGovRealConfig` | `TODO(real-egov)`: 電子署名フィールド追加 (証明書要否確認後) |

---

## 7. sandbox 疎通確認チェックリスト

認証情報取得後にサンドボックスで以下を確認すること。

- [ ] `NewEGovRealAdapter` がエラーなく初期化できる
- [ ] `SubmitArticle36` がサンドボックスの申請エンドポイントへ送信し、受付番号が返る
- [ ] `PollArticle36Status` がサンドボックスの受付番号でステータスを取得できる
- [ ] TLS 証明書エラーが発生しない (信頼できる証明書チェーン)
- [ ] タイムアウト (30 秒) 以内にレスポンスが返る
- [ ] エラーレスポンス (4xx / 5xx) が適切に `error` として返る
- [ ] ログに認証情報・マイナンバー等の復号値が含まれていない
- [ ] 冪等キー (`IdempotencyKey`) 付きリクエストの再送で二重申請にならない
