# 実外部API連携 実装差込 Runbook

> **セキュリティ注記**: 本ドキュメントに実認証情報 (APIキー・シークレット・接続文字列)
> を記載しないこと。実値は Secret Manager または環境変数で管理し、絶対にリポジトリに
> コミットしないこと。
>
> **法令注記**: e-Gov 電子申請・労務 SaaS 連携の利用登録手順・認証方式・
> 電子署名要否・申請様式の法的解釈は、担当 e-Gov 窓口・社労士・弁護士との
> 一次法令確認が前提です。本ドキュメントは法的助言ではありません。

## 概要

本 runbook は以下 4 種類の外部 API 連携について、認証情報の取得手順・
実装を差し込むファイルと関数・切替ポイント・sandbox 疎通確認項目を整理します。

| 連携先 | パッケージ | 現在の状態 | 切替関数 |
|--------|-----------|-----------|---------|
| e-Gov 電子申請 (36協定) | `govfiling` | スタブ | `WithEGovSubmitter` |
| マネーフォワード クラウド給与 | `ledger` | モック | `NewMockPayrollImporter` → 差替 |
| freee 人事労務 | `ledger` | モック | 同上 |
| 弥生給与 | `ledger` | モック | 同上 |
| Stripe | `billing` | スタブ | `BillingProvider` 差替 |

---

## 1. e-Gov 電子申請 (36協定)

詳細は `docs/ops/egov-article36-integration.md` を参照。

### 認証情報の取得手順

1. https://developer.e-gov.go.jp/ でサンドボックスアカウントを申請する。
2. ClientID / ClientSecret が発行されるので Secret Manager に登録する。
3. サンドボックス BaseURL を API ドキュメントで確認する。

### 実装を差し込むファイル:関数

| ファイル | 関数 / 箇所 | 内容 |
|----------|------------|------|
| `backend/internal/govfiling/egov_real_adapter.go` | `NewEGovRealAdapter` | `TODO(real-egov)`: OAuth2 Transport 追加 |
| `backend/internal/govfiling/egov_real_adapter.go` | `SubmitArticle36` | `TODO(real-egov)`: トークン取得・申請送信 |
| `backend/internal/govfiling/egov_real_adapter.go` | `PollArticle36Status` | `TODO(real-egov)`: ステータスポーリング |

### 切替ポイント

`backend/internal/govfiling/routes.go` の `RegisterRoutes` 内:

```go
// 現在 (スタブ):
svc := govfiling.NewService(tdb)
// → govfiling.Service は egovSubmitter=nil のまま (stub fallback)

// 切替後:
adapter, err := govfiling.NewEGovRealAdapter(govfiling.EGovRealConfig{
    SandboxMode:  os.Getenv("EGOV_SANDBOX_MODE") == "true",
    BaseURL:      os.Getenv("EGOV_BASE_URL"),
    ClientID:     os.Getenv("EGOV_CLIENT_ID"),
    ClientSecret: os.Getenv("EGOV_CLIENT_SECRET"),
})
if err != nil { /* 起動時に失敗させる */ }
svc = svc.WithEGovSubmitter(adapter)
```

### sandbox 疎通確認項目

- [ ] `NewEGovRealAdapter` が初期化成功
- [ ] `SubmitArticle36` でサンドボックスへ申請送信・受付番号取得
- [ ] `PollArticle36Status` でステータス取得
- [ ] TLS エラーなし、タイムアウト (30秒) 以内に応答
- [ ] ログに認証情報・個人情報が含まれない

---

## 2. 給与 SaaS — マネーフォワード クラウド給与

### 認証情報の取得手順

1. https://developer.moneyforward.com/apis/payroll でアプリ登録する。
2. OAuth2 ClientID / ClientSecret を取得し Secret Manager に登録する。
3. サンドボックス BaseURL を API ドキュメントで確認する。

### 実装を差し込むファイル:関数

| ファイル | 関数 / 箇所 | 内容 |
|----------|------------|------|
| `backend/internal/ledger/payroll_real_adapters.go` | `NewMoneyForwardAdapter` | `TODO(real-payroll-moneyforward)`: HTTP クライアント + OAuth2 初期化 |
| `backend/internal/ledger/payroll_real_adapters.go` | `moneyForwardAdapter.ImportPayroll` | `TODO(real-payroll-moneyforward)`: 給与データ取得・マッピング |

### 切替ポイント

`backend/internal/ledger/routes.go` の `RegisterRoutes` 内:

```go
// 現在 (モック):
importer := ledger.NewMockPayrollImporter(ledger.ProviderMock)

// 切替後 (マネーフォワード):
importer, err := ledger.NewMoneyForwardAdapter(ledger.MoneyForwardConfig{
    SandboxMode:  os.Getenv("PAYROLL_MF_SANDBOX") == "true",
    BaseURL:      os.Getenv("PAYROLL_MF_BASE_URL"),
    ClientID:     os.Getenv("PAYROLL_MF_CLIENT_ID"),
    ClientSecret: os.Getenv("PAYROLL_MF_CLIENT_SECRET"),
})
if err != nil { /* 起動時に失敗させる */ }
h := ledger.NewHandler(svc, importer)
```

### sandbox 疎通確認項目

- [ ] OAuth2 トークン取得成功
- [ ] 給与データ取得エンドポイントから応答が返る
- [ ] `ImportPayroll` がレコードを正しくドメインモデルにマッピングする
- [ ] TLS エラーなし
- [ ] ログに認証情報・給与金額が含まれない

---

## 3. 給与 SaaS — freee 人事労務

### 認証情報の取得手順

1. https://developer.freee.co.jp/docs/hr でアプリ登録する。
2. OAuth2 ClientID / ClientSecret を取得し Secret Manager に登録する。
3. サンドボックス BaseURL を API ドキュメントで確認する。

### 実装を差し込むファイル:関数

| ファイル | 関数 / 箇所 | 内容 |
|----------|------------|------|
| `backend/internal/ledger/payroll_real_adapters.go` | `NewFreeeAdapter` | `TODO(real-payroll-freee)`: HTTP クライアント + OAuth2 初期化 |
| `backend/internal/ledger/payroll_real_adapters.go` | `freeeAdapter.ImportPayroll` | `TODO(real-payroll-freee)`: 給与データ取得・マッピング |

### 切替ポイント

`backend/internal/ledger/routes.go` の `RegisterRoutes` 内 (§2 と同じ構造):

```go
// 切替後 (freee):
importer, err := ledger.NewFreeeAdapter(ledger.FreeeConfig{
    SandboxMode:  os.Getenv("PAYROLL_FREEE_SANDBOX") == "true",
    BaseURL:      os.Getenv("PAYROLL_FREEE_BASE_URL"),
    ClientID:     os.Getenv("PAYROLL_FREEE_CLIENT_ID"),
    ClientSecret: os.Getenv("PAYROLL_FREEE_CLIENT_SECRET"),
})
```

### sandbox 疎通確認項目

- [ ] OAuth2 トークン取得成功
- [ ] freee HR API からデータ取得
- [ ] TLS エラーなし
- [ ] ログに認証情報が含まれない

---

## 4. 給与 SaaS — 弥生給与

### 認証情報の取得手順

1. https://www.yayoi-kk.co.jp/biz/api/ でAPIキーを申請する
   (認証方式・エンドポイント仕様はポータルで確認すること)。
2. APIキーを取得し Secret Manager に登録する。
3. サンドボックス BaseURL を API ドキュメントで確認する。

### 実装を差し込むファイル:関数

| ファイル | 関数 / 箇所 | 内容 |
|----------|------------|------|
| `backend/internal/ledger/payroll_real_adapters.go` | `NewYayoiAdapter` | `TODO(real-payroll-yayoi)`: HTTP クライアント + APIキー認証初期化 |
| `backend/internal/ledger/payroll_real_adapters.go` | `yayoiAdapter.ImportPayroll` | `TODO(real-payroll-yayoi)`: 給与データ取得・マッピング |

### 切替ポイント

`backend/internal/ledger/routes.go` の `RegisterRoutes` 内:

```go
// 切替後 (弥生):
importer, err := ledger.NewYayoiAdapter(ledger.YayoiConfig{
    SandboxMode: os.Getenv("PAYROLL_YAYOI_SANDBOX") == "true",
    BaseURL:     os.Getenv("PAYROLL_YAYOI_BASE_URL"),
    APIKey:      os.Getenv("PAYROLL_YAYOI_API_KEY"),
})
```

### sandbox 疎通確認項目

- [ ] APIキー認証成功
- [ ] 給与データ取得エンドポイントから応答が返る
- [ ] TLS エラーなし
- [ ] ログに APIキー・給与金額が含まれない

---

## 5. Stripe 決済

> **PCI DSS 注記**: カード番号はサーバー側で受け取らないこと。
> Stripe Elements / Stripe.js を使い PaymentMethod ID のみをサーバーへ送る。
> `sk_live_...` キーは本番 Secret Manager 専用。開発中は `sk_test_...` のみ使用。

### 認証情報の取得手順

1. https://dashboard.stripe.com/ でアカウントを作成する。
2. ダッシュボード → API キー から `sk_test_...` を取得し Secret Manager に登録する。
3. Webhook エンドポイントを登録し、署名検証キー (`whsec_...`) を取得・登録する。
4. 本番移行時は `sk_live_...` を別途 Secret Manager で管理する
   (テストキーと本番キーを混在させない)。

### 実装を差し込むファイル:関数

| ファイル | 関数 / 箇所 | 内容 |
|----------|------------|------|
| `backend/internal/billing/stripe_adapter.go` | `NewStripeProvider` | `TODO(real-stripe)`: Stripe クライアント初期化 |
| `backend/internal/billing/stripe_adapter.go` | `stripeProvider.Charge` | `TODO(real-stripe)`: PaymentIntents API 呼び出し |

### 切替ポイント

`backend/internal/billing/routes.go` の `RegisterRoutes` 内:

```go
// 現在 (スタブ / nil provider):
svc := billing.NewService(tdb)

// 切替後 (Stripe):
provider, err := billing.NewStripeProvider(billing.StripeConfig{
    TestMode:      os.Getenv("STRIPE_TEST_MODE") == "true",
    SecretKey:     os.Getenv("STRIPE_SECRET_KEY"),     // sk_test_... or sk_live_...
    WebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"), // whsec_...
})
if err != nil { /* 起動時に失敗させる */ }
svc = svc.WithBillingProvider(provider)
```

### sandbox 疎通確認項目

- [ ] `NewStripeProvider` が `sk_test_...` で初期化成功
- [ ] テストカード (4242 4242 4242 4242) で PaymentIntent 作成成功
- [ ] Stripe Webhook イベントの署名検証成功
- [ ] TLS エラーなし
- [ ] ログにカード番号・`sk_...` キーが含まれない
- [ ] 本番切替前に `sk_live_...` と `sk_test_...` の混在がないことを確認

---

## 共通セキュリティチェックリスト

実アダプタ差替時に全連携先で必ず確認すること。

- [ ] 認証情報 (APIキー・シークレット・接続文字列) が Secret Manager で管理されている
- [ ] `.env` ファイルに実値が含まれない (プレースホルダのみ)
- [ ] `InsecureSkipVerify` が `false` のまま (TLS 証明書検証有効)
- [ ] タイムアウトが設定されている (デフォルト `http.Client` に依存しない)
- [ ] ログに認証情報・PII・復号値が含まれない
- [ ] エラーレスポンスに認証情報が露出しない
- [ ] 冪等キーが付与されている (重複申請・重複課金防止)
- [ ] サンドボックス / テストモードでのみ開発・テストを行っている
