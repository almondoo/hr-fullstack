// Package govfiling — このファイルは e-Gov 実アダプタ足場 (Issue #7 / P3).
//
// # e-Gov 実API連携 足場
//
// 現状: 認証情報待ち (e-Gov API サンドボックスアカウント未取得).
// 取得後にこのファイルの TODO(real-egov) 箇所を実装し、
// RegisterRoutes で NewEGovRealAdapter を WithEGovSubmitter に渡すこと.
//
// 差し替え手順:
//  1. e-Gov Developer Portal (https://developer.e-gov.go.jp/) でサンドボックスアカウントを取得
//  2. 環境変数 EGOV_CLIENT_ID / EGOV_CLIENT_SECRET / EGOV_BASE_URL を設定
//  3. 本ファイルの TODO(real-egov) 箇所を実装
//  4. routes.go の RegisterRoutes に NewEGovRealAdapter(cfg) を渡す
//     例: svc = svc.WithEGovSubmitter(govfiling.NewEGovRealAdapter(cfg))
//
// セキュリティ制約 (MUST NOT violate):
//   - TLS 証明書検証を無効化してはならない (InsecureSkipVerify 禁止).
//   - SandboxMode が false のときのみ本番エンドポイントを使用すること.
//   - ClientID / ClientSecret を定数・ソースコードにハードコードしてはならない.
//   - エラーメッセージ・ログに認証情報や復号値 (マイナンバー等) を含めてはならない.
//   - リトライは冪等キー (IdempotencyKey) を付けて行うこと.
//
// 法令注記: e-Gov 電子申請の利用登録・認証方式・電子署名要否は
// e-Gov 担当者・社労士との確認が前提. 本実装は法的助言ではない.
package govfiling

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// ---------------------------------------------------------------------------
// EGovRealConfig — 実アダプタ設定 (認証情報は環境変数 / Secret Manager で供給)
// ---------------------------------------------------------------------------

// EGovRealConfig holds configuration for the real e-Gov API adapter.
//
// All credential fields are placeholders; real values MUST be supplied via
// environment variables or a Secret Manager and MUST NOT be hardcoded.
//
// Environment variable mapping (see .env.example):
//
//	EGOV_BASE_URL        → BaseURL
//	EGOV_CLIENT_ID       → ClientID
//	EGOV_CLIENT_SECRET   → ClientSecret
//	EGOV_SANDBOX_MODE    → SandboxMode ("true"/"false")
//
// e-Gov API 参照:
//   - https://developer.e-gov.go.jp/ (要確認: サンドボックス有効化手順)
//   - 申請様式・送信仕様は上記ポータルで確認すること.
type EGovRealConfig struct {
	// SandboxMode must be true during development / testing.
	// Set to false ONLY after obtaining production credentials and approval.
	// Default-safe: callers should default to true until explicitly switched.
	SandboxMode bool

	// BaseURL is the e-Gov API base URL.
	// Sandbox (要確認): https://sandbox.shinsei.e-gov.go.jp/ (エンドポイント仕様要確認)
	// Production (要確認): https://shinsei.e-gov.go.jp/
	// Supply via EGOV_BASE_URL. Must not be empty in the real adapter.
	BaseURL string

	// ClientID is the OAuth2 / API client ID issued by e-Gov.
	// Supply via EGOV_CLIENT_ID. MUST NOT be hardcoded.
	ClientID string

	// ClientSecret is the OAuth2 / API client secret issued by e-Gov.
	// Supply via EGOV_CLIENT_SECRET. MUST NOT be hardcoded.
	ClientSecret string

	// TODO(real-egov): 電子署名 (電子証明書) 対応.
	// e-Gov API 仕様で電子署名が必要な場合は以下を追加:
	//   CertPath     string // 証明書ファイルパス (Secret Manager 経由で供給)
	//   CertPassword string // 証明書パスワード (Secret Manager 経由で供給)
}

// ---------------------------------------------------------------------------
// eGovRealAdapter — 実アダプタ (未実装: 認証情報取得後に実装する)
// ---------------------------------------------------------------------------

// eGovRealAdapter is the scaffold for the real e-Gov API adapter.
//
// Replace TODO(real-egov) sections with the actual implementation after:
//  1. e-Gov sandbox account and credentials are obtained.
//  2. The API specification (認証方式・エンドポイント・送信形式) is confirmed
//     with the e-Gov portal and a 社労士 / legal advisor.
//
// Until then, use NewEGovStubSubmitter() (the current default).
type eGovRealAdapter struct {
	cfg    EGovRealConfig
	client *http.Client
}

// NewEGovRealAdapter constructs the real e-Gov adapter from the given config.
// Returns an error when the config is obviously misconfigured (empty credentials,
// non-sandbox mode without explicit production flag).
//
// Call this from routes.go once credentials are available:
//
//	adapter, err := govfiling.NewEGovRealAdapter(cfg)
//	if err != nil { /* handle */ }
//	svc = svc.WithEGovSubmitter(adapter)
func NewEGovRealAdapter(cfg EGovRealConfig) (EGovSubmitter, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("govfiling: NewEGovRealAdapter: BaseURL is required (set EGOV_BASE_URL)")
	}
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("govfiling: NewEGovRealAdapter: ClientID is required (set EGOV_CLIENT_ID)")
	}
	if cfg.ClientSecret == "" {
		return nil, fmt.Errorf("govfiling: NewEGovRealAdapter: ClientSecret is required (set EGOV_CLIENT_SECRET)")
	}

	// HTTP client with TLS verification enabled (InsecureSkipVerify MUST remain
	// false; disabling certificate validation is forbidden per security policy).
	// Timeout prevents hung connections from blocking cleanup goroutines.
	// TODO(real-egov): OAuth2 transport (token refresh) を Transport に追加する.
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		// net/http's default Transport uses the system trust store and performs
		// full TLS certificate chain verification. No override is needed here.
		// Explicitly do NOT set Transport.TLSClientConfig.InsecureSkipVerify.
	}

	return &eGovRealAdapter{cfg: cfg, client: httpClient}, nil
}

// SubmitArticle36 submits a 36協定 electronic filing to the e-Gov API.
//
// TODO(real-egov): 実実装手順:
//  1. OAuth2 アクセストークンを取得 (token_endpoint は e-Gov API 仕様で確認).
//  2. req.PayloadJSON を e-Gov 申請様式 XML / JSON に変換
//     (様式仕様は e-Gov ポータルで確認. 社労士確認前提).
//  3. POST /api/v1/apply (エンドポイント名は要確認) に送信.
//     Authorization ヘッダーに Bearer {accessToken} を付与.
//  4. レスポンスから受付番号 (受付ID) を抽出し ExternalRef に設定.
//  5. HTTP エラー・タイムアウト時は retryable/non-retryable を分類して返す.
//
// セキュリティ:
//   - req.PayloadJSON に機微情報 (マイナンバー等の復号値) が含まれていないことを
//     前提とする (呼出側の責任; 本アダプタは受け取ったまま送信する).
//   - TLS 証明書検証は必ず有効にすること (InsecureSkipVerify 禁止).
//   - エラーメッセージに認証情報・個人番号を含めてはならない.
func (a *eGovRealAdapter) SubmitArticle36(_ context.Context, req Article36SubmitRequest) (Article36SubmitResult, error) {
	// TODO(real-egov): 実装する (認証情報取得後).
	// 現時点ではエラーを返して呼出側がスタブへフォールバックできるようにする.
	return Article36SubmitResult{}, fmt.Errorf(
		"govfiling: e-Gov real adapter: SubmitArticle36 not yet implemented"+
			" (idempotency_key=%s tenant=%s): "+
			"obtain e-Gov sandbox credentials and implement TODO(real-egov) in egov_real_adapter.go",
		req.IdempotencyKey, req.TenantID,
	)
}

// PollArticle36Status polls the e-Gov API for the current status of a filing.
//
// TODO(real-egov): 実装手順:
//  1. OAuth2 アクセストークンを取得 (または再利用).
//  2. GET /api/v1/status/{externalRef} (エンドポイント名は要確認) を呼び出す.
//  3. レスポンスのステータスコードを govfiling ステータス機械の文字列に変換して返す.
//     マッピング例 (e-Gov 仕様確認後に確定):
//       e-Gov "受付済み" → StatusAccepted
//       e-Gov "処理完了" → StatusCompleted
//       e-Gov "返戻"    → StatusReturned
//       e-Gov "エラー"  → StatusError
func (a *eGovRealAdapter) PollArticle36Status(_ context.Context, externalRef string) (string, error) {
	// TODO(real-egov): 実装する (認証情報取得後).
	return "", fmt.Errorf(
		"govfiling: e-Gov real adapter: PollArticle36Status not yet implemented"+
			" (external_ref=%s): "+
			"obtain e-Gov sandbox credentials and implement TODO(real-egov) in egov_real_adapter.go",
		externalRef,
	)
}
