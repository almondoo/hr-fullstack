// Package ledger — このファイルは給与SaaS実アダプタ足場 (Issue #7 / P3).
//
// # 給与SaaS実API連携 足場 (マネーフォワード / freee / 弥生)
//
// 現状: 認証情報待ち (各サービスのサンドボックス API キー未取得).
// 取得後に各アダプタの TODO(real-payroll-<provider>) 箇所を実装し、
// routes.go の importer を差し替えること.
//
// 差し替え手順 (例: マネーフォワード):
//  1. マネーフォワード クラウド給与 API のサンドボックスアカウントを取得
//  2. 環境変数 PAYROLL_MF_CLIENT_ID / PAYROLL_MF_CLIENT_SECRET を設定
//  3. 本ファイルの TODO(real-payroll-moneyforward) 箇所を実装
//  4. routes.go の importer を NewMoneyForwardImporter(cfg) に変更
//
// セキュリティ制約 (MUST NOT violate):
//   - TLS 証明書検証を無効化してはならない.
//   - API キー / トークン をソースコードにハードコードしてはならない.
//   - Fetch の戻り値に カード番号 / PAN / 生トークン を含めてはならない.
//     WageJSON には正規化された給与項目 (基本給/手当/控除等) のみ格納する.
//   - エラーメッセージに認証情報・PII を含めてはならない.
//
// 法令注記: 給与計算結果の利用・連携は各サービスの API 利用規約および
// 労働基準法・所得税法等の法令に従うこと. 本実装は法的助言ではない.
package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// マネーフォワード クラウド給与 アダプタ足場
// ---------------------------------------------------------------------------

// MoneyForwardConfig holds configuration for the MoneyForward Cloud Payroll adapter.
//
// Environment variable mapping (see .env.example):
//
//	PAYROLL_MF_CLIENT_ID     → ClientID
//	PAYROLL_MF_CLIENT_SECRET → ClientSecret
//	PAYROLL_MF_BASE_URL      → BaseURL
//	PAYROLL_MF_SANDBOX       → Sandbox ("true"/"false")
//
// API 参照: https://developer.moneyforward.com/apis/payroll (要確認)
type MoneyForwardConfig struct {
	// Sandbox instructs the adapter to use the MoneyForward sandbox environment.
	// Set to true during development; false only in production.
	Sandbox bool

	// BaseURL is the MoneyForward Cloud Payroll API base URL.
	// Sandbox (要確認): https://invoice.moneyforward.com/ (エンドポイント仕様要確認)
	// Supply via PAYROLL_MF_BASE_URL.
	BaseURL string

	// ClientID is the OAuth2 client ID for MoneyForward Cloud Payroll.
	// Supply via PAYROLL_MF_CLIENT_ID. MUST NOT be hardcoded.
	ClientID string

	// ClientSecret is the OAuth2 client secret for MoneyForward Cloud Payroll.
	// Supply via PAYROLL_MF_CLIENT_SECRET. MUST NOT be hardcoded.
	ClientSecret string
}

// moneyForwardImporter is the scaffold for the MoneyForward payroll adapter.
type moneyForwardImporter struct {
	cfg MoneyForwardConfig
}

// NewMoneyForwardImporter constructs the MoneyForward payroll importer.
// Returns an error when the config is obviously misconfigured.
//
// Call this from routes.go once credentials are available:
//
//	imp, err := ledger.NewMoneyForwardImporter(cfg)
//	if err != nil { /* handle */ }
//	h := ledger.NewHandler(svc, imp)
func NewMoneyForwardImporter(cfg MoneyForwardConfig) (PayrollImporter, error) {
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("ledger: NewMoneyForwardImporter: ClientID is required (set PAYROLL_MF_CLIENT_ID)")
	}
	if cfg.ClientSecret == "" {
		return nil, fmt.Errorf("ledger: NewMoneyForwardImporter: ClientSecret is required (set PAYROLL_MF_CLIENT_SECRET)")
	}
	// TODO(real-payroll-moneyforward): HTTP クライアント初期化.
	// - OAuth2 認可コードフロー / クライアントクレデンシャルフロー実装.
	// - TLS 設定 (InsecureSkipVerify は NEVER true).
	return &moneyForwardImporter{cfg: cfg}, nil
}

// Provider returns the MoneyForward provider identifier.
func (m *moneyForwardImporter) Provider() string { return ProviderMoneyForward }

// Fetch retrieves normalised wage data for an employee and period from
// MoneyForward Cloud Payroll.
//
// TODO(real-payroll-moneyforward): 実装手順:
//  1. OAuth2 アクセストークンを取得 (または再利用).
//  2. GET /api/v1/payrolls?employee_id={employeeID}&period={period} を呼び出す
//     (エンドポイント名・パラメータ名は MoneyForward API 仕様で確認).
//  3. レスポンスを PayrollData.WageJSON の正規化スキーマに変換:
//     {"base_pay":<円>,"allowances":<円>,"overtime":<円>,"deductions":<円>,...}
//  4. ProviderRef にレスポンスの opaque ID を設定.
//
// セキュリティ:
//   - WageJSON にカード番号 / PAN / 生トークンを含めてはならない.
//   - エラーメッセージに認証情報・個人情報を含めてはならない.
func (m *moneyForwardImporter) Fetch(_ context.Context, tenantID, employeeID uuid.UUID, period string) (*PayrollData, error) {
	// TODO(real-payroll-moneyforward): 実装する (認証情報取得後).
	return nil, fmt.Errorf(
		"ledger: MoneyForward importer: Fetch not yet implemented"+
			" (tenant=%s employee=%s period=%s): "+
			"obtain MoneyForward sandbox credentials and implement"+
			" TODO(real-payroll-moneyforward) in payroll_real_adapters.go",
		tenantID, employeeID, period,
	)
}

// ---------------------------------------------------------------------------
// freee 人事労務 アダプタ足場
// ---------------------------------------------------------------------------

// FreeeConfig holds configuration for the freee HR/Payroll API adapter.
//
// Environment variable mapping (see .env.example):
//
//	PAYROLL_FREEE_CLIENT_ID     → ClientID
//	PAYROLL_FREEE_CLIENT_SECRET → ClientSecret
//	PAYROLL_FREEE_BASE_URL      → BaseURL
//	PAYROLL_FREEE_SANDBOX       → Sandbox ("true"/"false")
//
// API 参照: https://developer.freee.co.jp/docs/hr (要確認)
type FreeeConfig struct {
	// Sandbox instructs the adapter to use the freee sandbox environment.
	Sandbox bool

	// BaseURL is the freee HR API base URL.
	// Sandbox (要確認): https://api.freee.co.jp/ (エンドポイント仕様要確認)
	// Supply via PAYROLL_FREEE_BASE_URL.
	BaseURL string

	// ClientID is the OAuth2 client ID for freee HR.
	// Supply via PAYROLL_FREEE_CLIENT_ID. MUST NOT be hardcoded.
	ClientID string

	// ClientSecret is the OAuth2 client secret for freee HR.
	// Supply via PAYROLL_FREEE_CLIENT_SECRET. MUST NOT be hardcoded.
	ClientSecret string
}

// freeeImporter is the scaffold for the freee payroll adapter.
type freeeImporter struct {
	cfg FreeeConfig
}

// NewFreeeImporter constructs the freee payroll importer.
// Returns an error when the config is obviously misconfigured.
//
// Call this from routes.go once credentials are available:
//
//	imp, err := ledger.NewFreeeImporter(cfg)
//	if err != nil { /* handle */ }
//	h := ledger.NewHandler(svc, imp)
func NewFreeeImporter(cfg FreeeConfig) (PayrollImporter, error) {
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("ledger: NewFreeeImporter: ClientID is required (set PAYROLL_FREEE_CLIENT_ID)")
	}
	if cfg.ClientSecret == "" {
		return nil, fmt.Errorf("ledger: NewFreeeImporter: ClientSecret is required (set PAYROLL_FREEE_CLIENT_SECRET)")
	}
	// TODO(real-payroll-freee): HTTP クライアント初期化.
	return &freeeImporter{cfg: cfg}, nil
}

// Provider returns the freee provider identifier.
func (f *freeeImporter) Provider() string { return ProviderFreee }

// Fetch retrieves normalised wage data for an employee and period from freee HR.
//
// TODO(real-payroll-freee): 実装手順:
//  1. OAuth2 アクセストークンを取得 (認可コードフロー).
//  2. GET /api/v1/payrolls ... を呼び出す
//     (エンドポイント・パラメータは freee API 仕様で確認).
//  3. レスポンスを PayrollData.WageJSON の正規化スキーマに変換.
//  4. ProviderRef に opaque ID を設定.
//
// セキュリティ: WageJSON にカード番号 / PAN / 生トークンを含めてはならない.
func (f *freeeImporter) Fetch(_ context.Context, tenantID, employeeID uuid.UUID, period string) (*PayrollData, error) {
	// TODO(real-payroll-freee): 実装する (認証情報取得後).
	return nil, fmt.Errorf(
		"ledger: freee importer: Fetch not yet implemented"+
			" (tenant=%s employee=%s period=%s): "+
			"obtain freee sandbox credentials and implement"+
			" TODO(real-payroll-freee) in payroll_real_adapters.go",
		tenantID, employeeID, period,
	)
}

// ---------------------------------------------------------------------------
// 弥生給与 アダプタ足場
// ---------------------------------------------------------------------------

// YayoiConfig holds configuration for the Yayoi Kyuyo (弥生給与) API adapter.
//
// Environment variable mapping (see .env.example):
//
//	PAYROLL_YAYOI_API_KEY  → APIKey
//	PAYROLL_YAYOI_BASE_URL → BaseURL
//	PAYROLL_YAYOI_SANDBOX  → Sandbox ("true"/"false")
//
// API 参照: https://www.yayoi-kk.co.jp/biz/api/ (要確認)
// 注意: 弥生のAPI連携方式 (OAuth2/APIキー等) は公式ドキュメントで確認すること.
type YayoiConfig struct {
	// Sandbox instructs the adapter to use the Yayoi sandbox environment.
	Sandbox bool

	// BaseURL is the Yayoi API base URL.
	// Supply via PAYROLL_YAYOI_BASE_URL. MUST confirm with Yayoi documentation.
	BaseURL string

	// APIKey is the API key for Yayoi Kyuyo.
	// Supply via PAYROLL_YAYOI_API_KEY. MUST NOT be hardcoded.
	// NOTE: 認証方式 (OAuth2 / APIキー) は弥生API仕様で確認すること.
	APIKey string
}

// yayoiImporter is the scaffold for the Yayoi payroll adapter.
type yayoiImporter struct {
	cfg YayoiConfig
}

// NewYayoiImporter constructs the Yayoi payroll importer.
// Returns an error when the config is obviously misconfigured.
//
// Call this from routes.go once credentials are available:
//
//	imp, err := ledger.NewYayoiImporter(cfg)
//	if err != nil { /* handle */ }
//	h := ledger.NewHandler(svc, imp)
func NewYayoiImporter(cfg YayoiConfig) (PayrollImporter, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("ledger: NewYayoiImporter: APIKey is required (set PAYROLL_YAYOI_API_KEY)")
	}
	// TODO(real-payroll-yayoi): HTTP クライアント初期化.
	return &yayoiImporter{cfg: cfg}, nil
}

// Provider returns the Yayoi provider identifier.
func (y *yayoiImporter) Provider() string { return ProviderYayoi }

// Fetch retrieves normalised wage data for an employee and period from Yayoi Kyuyo.
//
// TODO(real-payroll-yayoi): 実装手順:
//  1. APIキー / 認証トークンを使ってエンドポイントを呼び出す
//     (認証方式・エンドポイントは弥生API仕様で確認).
//  2. レスポンスを PayrollData.WageJSON の正規化スキーマに変換.
//  3. ProviderRef に opaque ID を設定.
//
// セキュリティ: WageJSON にカード番号 / PAN / 生トークンを含めてはならない.
func (y *yayoiImporter) Fetch(_ context.Context, tenantID, employeeID uuid.UUID, period string) (*PayrollData, error) {
	// TODO(real-payroll-yayoi): 実装する (認証情報取得後).
	return nil, fmt.Errorf(
		"ledger: Yayoi importer: Fetch not yet implemented"+
			" (tenant=%s employee=%s period=%s): "+
			"obtain Yayoi sandbox credentials and implement"+
			" TODO(real-payroll-yayoi) in payroll_real_adapters.go",
		tenantID, employeeID, period,
	)
}
