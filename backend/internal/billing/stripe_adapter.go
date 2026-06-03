// Package billing — このファイルは Stripe 実アダプタ足場 (Issue #7 / P3).
//
// # Stripe 決済実API連携 足場
//
// 現状: 認証情報待ち (Stripe テストアカウント / Secret Key 未取得).
// 取得後に TODO(real-stripe) 箇所を実装し、
// routes.go で NewStripeProvider を WithProvider に渡すこと.
//
// 差し替え手順:
//  1. https://dashboard.stripe.com/ でテストアカウントを作成
//  2. テスト用 Secret Key を取得し環境変数 STRIPE_SECRET_KEY に設定
//  3. 本ファイルの TODO(real-stripe) 箇所を実装
//  4. routes.go でサービスを NewStripeProvider(cfg) に切り替える
//     例: svc = svc.WithProvider(billing.NewStripeProvider(cfg))
//
// セキュリティ制約 (MUST NOT violate):
//   - カード番号 / PAN / 生カードトークンをサーバー側で受け取り・保存してはならない.
//     Stripe Elements / Stripe.js でトークン化し、opaque PaymentMethodID のみ渡すこと.
//   - STRIPE_SECRET_KEY をソースコードにハードコードしてはならない.
//     本番では Secret Manager (AWS Secrets Manager / GCP Secret Manager / Vault 等) で管理.
//   - TLS 証明書検証を無効化してはならない.
//   - Stripe Webhook を使う場合は STRIPE_WEBHOOK_SECRET で署名を必ず検証すること.
//   - エラーメッセージに Secret Key や card 情報を含めてはならない.
//
// PCI DSS 注記:
//   - このシステムはカード情報を一切受け取らない (Stripe が PCI スコープを吸収).
//   - ChargeRequest.Amount は minor units (最小通貨単位) で渡すこと.
//     例: 1000円 = 100000 (Stripe API は銭単位 i.e. JPY は整数扱いのため要注意).
//   - 法令・会計処理 (インボイス制度等) の正確性は専門家確認が前提. 本実装は法的助言ではない.
package billing

import (
	"fmt"
)

// ---------------------------------------------------------------------------
// StripeConfig — Stripe アダプタ設定 (認証情報は環境変数 / Secret Manager で供給)
// ---------------------------------------------------------------------------

// StripeConfig holds configuration for the Stripe payment adapter.
//
// All credential fields are placeholders; real values MUST be supplied via
// environment variables or a Secret Manager and MUST NOT be hardcoded.
//
// Environment variable mapping (see .env.example):
//
//	STRIPE_SECRET_KEY      → SecretKey
//	STRIPE_WEBHOOK_SECRET  → WebhookSecret
//	STRIPE_TEST_MODE       → TestMode ("true"/"false")
//
// Stripe API 参照:
//   - https://stripe.com/docs/api (Charges / PaymentIntents)
//   - https://stripe.com/docs/webhooks (Webhook 署名検証)
type StripeConfig struct {
	// TestMode must be true during development and staging.
	// Set to false ONLY in production with live credentials.
	TestMode bool

	// SecretKey is the Stripe secret API key (sk_test_... or sk_live_...).
	// Supply via STRIPE_SECRET_KEY. MUST NOT be hardcoded.
	// Test key format:  sk_test_...
	// Live key format:  sk_live_...
	SecretKey string

	// WebhookSecret is the Stripe webhook signing secret (whsec_...).
	// Required for webhook signature verification. Supply via STRIPE_WEBHOOK_SECRET.
	// MUST NOT be hardcoded.
	WebhookSecret string

	// TODO(real-stripe): 必要に応じて以下を追加:
	//   IdempotencyKeyPrefix string // 冪等キープレフィックス (env から供給)
	//   MaxRetries          int    // リトライ上限 (default: 3)
}

// ---------------------------------------------------------------------------
// stripeProvider — Stripe 実アダプタ (未実装: 認証情報取得後に実装する)
// ---------------------------------------------------------------------------

// stripeProvider is the scaffold for the real Stripe payment provider.
//
// Replace TODO(real-stripe) sections with the actual Stripe API calls after:
//  1. Stripe test account and Secret Key are obtained.
//  2. The integration approach (Charges API vs PaymentIntents API) is decided.
//     Recommendation: use PaymentIntents for SCA (Strong Customer Authentication)
//     compliance (3D Secure 対応).
//
// Until then, the MockProvider remains active (see service.go NewService).
type stripeProvider struct {
	cfg StripeConfig
}

// NewStripeProvider constructs the Stripe payment provider.
// Returns an error when the config is obviously misconfigured.
//
// Call this from routes.go once credentials are available:
//
//	provider, err := billing.NewStripeProvider(cfg)
//	if err != nil { /* handle */ }
//	svc = svc.WithProvider(provider)
func NewStripeProvider(cfg StripeConfig) (PaymentProvider, error) {
	if cfg.SecretKey == "" {
		return nil, fmt.Errorf("billing: NewStripeProvider: SecretKey is required (set STRIPE_SECRET_KEY)")
	}
	// Prevent accidental live key use in test mode.
	if cfg.TestMode && len(cfg.SecretKey) >= 8 && cfg.SecretKey[:8] == "sk_live_" {
		return nil, fmt.Errorf(
			"billing: NewStripeProvider: live SecretKey (sk_live_...) used with TestMode=true;" +
				" set TestMode=false for production or use a test key (sk_test_...)")
	}
	// TODO(real-stripe): Stripe クライアント初期化.
	// stripe-go を使う場合:
	//   stripe.Key = cfg.SecretKey
	//   stripe.SetBackend(stripe.APIBackend, nil) // デフォルト HTTPS バックエンド
	// または stripe.Client を使ってインスタンスごとに設定する.
	return &stripeProvider{cfg: cfg}, nil
}

// Charge attempts a payment via the Stripe API.
//
// TODO(real-stripe): 実装手順 (PaymentIntents 推奨):
//  1. stripe-go ライブラリ (github.com/stripe/stripe-go/v76) を go.mod に追加.
//     pnpm / npm に相当する: go get github.com/stripe/stripe-go/v76
//     NOTE: go.mod の変更は Tier 2 作業。追加前に担当者に確認すること.
//  2. stripe.PaymentIntentParams を構築:
//     - Amount: req.Amount (minor units; JPY は銭単位ではなく円単位なので要注意)
//     - Currency: req.Currency (小文字, 例: "jpy")
//     - PaymentMethod: フロントエンドから Stripe.js で取得した opaque PaymentMethodID
//     - Confirm: stripe.Bool(true)
//     - IdempotencyKey: req.InvoiceID.String() (冪等保証)
//  3. stripe.PaymentIntentAPI.New(params) を呼び出す.
//  4. レスポンス Status を ChargeResult.Status に変換:
//     "succeeded"         → PaymentSucceeded
//     "requires_action"   → PaymentPending (3DS 認証待ち)
//     その他 / エラー      → PaymentFailed
//  5. PaymentIntent.ID を ProviderRef (opaque) に設定.
//
// セキュリティ:
//   - カード番号 / PAN を絶対に受け取ってはならない (Stripe Elements 経由).
//   - エラーメッセージに SecretKey / card 情報を含めてはならない.
//   - Stripe エラーオブジェクトをそのままクライアントに返さないこと.
func (p *stripeProvider) Charge(req ChargeRequest) ChargeResult {
	// TODO(real-stripe): 実装する (認証情報取得後).
	// 現時点では失敗を返して呼出側がモックへフォールバックできるようにする.
	reason := fmt.Sprintf(
		"stripe real adapter: Charge not yet implemented (invoice_id=%s):"+
			" obtain Stripe test credentials and implement TODO(real-stripe)"+
			" in stripe_adapter.go",
		req.InvoiceID,
	)
	return ChargeResult{
		Provider:      "stripe",
		Status:        PaymentFailed,
		FailureReason: &reason,
	}
}
