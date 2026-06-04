// Package govfiling — this file defines the e-Gov 電子申請アダプタの抽象化.
//
// # 36協定 e-Gov 電子届出 足場 (Issue #21)
//
// 設計方針:
//   - EGovSubmitter インタフェースで e-Gov 固有操作を抽象化し、実アダプタ(P3)と
//     スタブ実装を差し替え可能にする。
//   - Article36SubmitRequest / Article36SubmitResult は 36協定届に特化した
//     入出力型。PayloadJSON には参照 ID のみを格納し、機微情報は格納しない。
//   - EGovConfig は将来の e-Gov サンドボックス/本番認証情報の設定雛形。
//     本番値は環境変数 / Secret Manager で供給し、コードにハードコードしない。
//
// 外部依存 (足場で残る範囲):
//   - e-Gov API の OAuth2 クライアント ID / シークレット / エンドポイント URL が未取得。
//     取得後に egov_real_adapter.go で EGovSubmitter を実装し、
//     RegisterRoutes の WithSubmitter / WithEGovSubmitter で差し替える。
//   - 電子署名 (電子証明書) の要否・方式は e-Gov API 仕様確認後に決定する
//     (社労士/弁護士・e-Gov担当者との確認前提)。
//
// セキュリティ注記:
//   - 認証情報 (ClientID / ClientSecret / AccessToken 等) は EGovConfig に
//     プレースホルダとしてのみ記述する。実値は環境変数 / Secret Manager で供給する。
//   - マイナンバー等の復号値を Article36SubmitRequest に格納してはならない。
//     必要な場合は SubmitRequest.NumberPlaintext (transient) を使用する。
//   - 危険な既定値 (TLS検証無効・alg=none 等) を持つ実装を追加してはならない。
package govfiling

import (
	"context"
	"fmt"
)

// ---------------------------------------------------------------------------
// EGov config (認証情報設定雛形 — 実値は環境変数 / Secret Manager で供給)
// ---------------------------------------------------------------------------

// EGovConfig holds the configuration for the e-Gov API adapter.
//
// All fields are placeholders; real values MUST be supplied via environment
// variables or a Secret Manager and MUST NOT be hardcoded in source code.
//
// e-Gov API reference:
//   - https://developer.e-gov.go.jp/ (e-Gov Developer Portal — 要確認)
//   - API 仕様・サンドボックスエンドポイントは上記で確認すること。
//
// Legal/operational note: e-Gov 電子申請の利用登録・審査・認証方式・
// サンドボックス有効化は e-Gov 担当者・社労士との確認が前提。本設定は
// 足場のみであり法的助言ではない。
type EGovConfig struct {
	// SandboxMode instructs the adapter to target the e-Gov sandbox environment.
	// MUST be false in production. Default: true (safe — no real submission).
	SandboxMode bool

	// BaseURL is the e-Gov API base URL.
	// Production:  https://shinsei.e-gov.go.jp/  (要確認)
	// Sandbox:     https://sandbox.shinsei.e-gov.go.jp/  (要確認)
	// Leave empty to use the stub; the real adapter validates this field.
	BaseURL string

	// ClientID is the OAuth2 / API client ID issued by e-Gov.
	// Supply via EGOV_CLIENT_ID environment variable. MUST NOT be hardcoded.
	ClientID string

	// ClientSecret is the OAuth2 / API client secret issued by e-Gov.
	// Supply via EGOV_CLIENT_SECRET environment variable. MUST NOT be hardcoded.
	ClientSecret string

	// TODO(p3): 電子署名 (電子証明書) 対応。e-Gov API 仕様確認後に追加する。
	// CertPath string
	// CertPassword string
}

// ---------------------------------------------------------------------------
// Article36 submit request / result types
// ---------------------------------------------------------------------------

// Article36SubmitRequest is the input to EGovSubmitter.SubmitArticle36.
//
// PayloadJSON must contain only reference IDs / non-sensitive filing data;
// decrypted 機微情報 (マイナンバー等) MUST NOT be included.
// The idempotency key prevents duplicate external submissions.
type Article36SubmitRequest struct {
	// TenantID identifies the tenant (for logging only; not sent to e-Gov).
	TenantID string
	// IdempotencyKey prevents duplicate submissions (外部API冪等キー).
	IdempotencyKey string
	// PayloadJSON holds reference IDs and non-sensitive filing metadata.
	// SECURITY: MUST NOT contain 機微情報 or decrypted personal numbers.
	PayloadJSON []byte
	// LaborAgreementDocID is the opaque ID of the agreement document row
	// (labour_agreement_documents). Included as a reference only;
	// not sent to e-Gov in plaintext.
	LaborAgreementDocID string //nolint:misspell // field name matches DB schema contract
}

// Article36SubmitResult is the result returned by EGovSubmitter.SubmitArticle36.
type Article36SubmitResult struct {
	// ExternalRef is the opaque e-Gov acceptance reference number (受付番号).
	// Set to a deterministic mock value by the stub.
	ExternalRef string
}

// ---------------------------------------------------------------------------
// EGovSubmitter interface
// ---------------------------------------------------------------------------

// EGovSubmitter abstracts the e-Gov channel for 36協定 electronic filings.
//
// The stub implementation (eGovStubSubmitter) is used until the real e-Gov
// API credentials are obtained. The real adapter (P3) will implement this
// interface without changing callers.
//
// Security contract:
//   - Implementations MUST NOT persist, log, or include PersonalNumber/PII
//     in any error message or audit record.
//   - Implementations MUST use TLS with certificate validation (no TLS skip).
//   - Implementations MUST validate that SandboxMode is false before
//     targeting the production endpoint.
type EGovSubmitter interface {
	// SubmitArticle36 submits a 36協定 filing to e-Gov and returns the
	// acceptance reference number. The call is idempotent via IdempotencyKey.
	SubmitArticle36(ctx context.Context, req Article36SubmitRequest) (Article36SubmitResult, error)

	// PollArticle36Status polls e-Gov for the current status of a previously
	// submitted 36協定 filing identified by externalRef.
	// Returns the current status string as reported by e-Gov.
	// Callers map the string to the govfiling status machine.
	PollArticle36Status(ctx context.Context, externalRef string) (string, error)
}

// ---------------------------------------------------------------------------
// Stub implementation (使用中: 外部認証情報取得前のデフォルト)
// ---------------------------------------------------------------------------

// eGovStubSubmitter is the MVP stub adapter for the e-Gov 36協定 channel.
// It makes no real network calls. All returned values are deterministic
// mocks derived from the idempotency key (冪等: same key → same ref).
//
// Replace with the real adapter when e-Gov API credentials are available (P3).
type eGovStubSubmitter struct{}

// NewEGovStubSubmitter returns the no-op stub EGovSubmitter.
// Used by default until real e-Gov credentials are configured.
func NewEGovStubSubmitter() EGovSubmitter {
	return eGovStubSubmitter{}
}

// SubmitArticle36 returns a deterministic mock acceptance reference.
// No real e-Gov API call is made (実送信は P3 / 認証情報取得後)。
func (eGovStubSubmitter) SubmitArticle36(_ context.Context, req Article36SubmitRequest) (Article36SubmitResult, error) {
	// Deterministic opaque ref keyed by idempotency key (stub only).
	ref := fmt.Sprintf("STUB-EGOV-36-%s", req.IdempotencyKey)
	return Article36SubmitResult{ExternalRef: ref}, nil
}

// PollArticle36Status always returns "accepted" for the stub.
// The real adapter will poll the e-Gov status endpoint (P3).
func (eGovStubSubmitter) PollArticle36Status(_ context.Context, externalRef string) (string, error) {
	// Stub: treat any STUB-prefixed ref as accepted, others as submitted.
	if len(externalRef) > 5 && externalRef[:5] == "STUB-" {
		return StatusAccepted, nil
	}
	return StatusSubmitted, nil
}
