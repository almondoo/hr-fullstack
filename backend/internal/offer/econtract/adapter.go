// Package econtract implements the electronic-contract service adapter scaffold
// for offer letters (INT-009 / LM-002).
//
// # 電子契約サービス連携 足場 (Issue #14)
//
// 設計方針:
//   - EContractAdapter インタフェースで外部電子契約サービス固有操作を抽象化し、
//     実アダプタ (クラウドサイン / DocuSign / Adobe Sign 等) と
//     スタブ実装を差し替え可能にする。
//   - SendSignRequest / SignStatus は provider 非依存の共通型。
//     envelope_id など外部参照は opaque な文字列として持ち、PII を含めない。
//   - EContractConfig は将来の外部サービス認証情報の設定雛形。
//     本番値は環境変数 / Secret Manager で供給し、コードにハードコードしない。
//   - 署名状態は offer_letters の既存カラム (esign_provider, esign_envelope_id,
//     signer_ref, signed_at, content_hash) を活用する。
//     追加マイグレーション不要。
//
// 外部依存 (足場で残る範囲):
//   - クラウドサイン API キー / DocuSign OAuth2 クライアント ID・シークレット が未取得。
//     取得後に real_adapter.go で EContractAdapter を実装し、
//     RegisterWebhookRoutes の WithEContractAdapter で差し替える。
//   - Webhook 署名検証キー (HMAC-SHA256 共有シークレット等) は各 provider の
//     ダッシュボードで発行後に環境変数で供給すること。
//   - 電子書面の法的要件 (電帳法 / e-文書法・真実性・可視性、保存年限) は
//     社労士 / 弁護士との確認が前提。本実装は法的助言ではない。
//
// セキュリティ注記:
//   - 認証情報 (APIKey / ClientSecret 等) は EContractConfig に
//     プレースホルダとしてのみ記述する。実値は環境変数 / Secret Manager で供給する。
//   - signer_ref には PII (氏名・メール等) を格納してはならない。opaque ID のみ。
//   - 危険な既定値 (TLS 検証無効化 / alg=none 等) を持つ実装を追加してはならない。
//   - Webhook 受信時はリクエストの真正性を HMAC / 署名検証で確認すること。
package econtract

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// EContractConfig — 認証情報設定雛形 (実値は環境変数 / Secret Manager で供給)
// ---------------------------------------------------------------------------

// Config holds the configuration for the electronic-contract adapter.
//
// All credential fields are placeholders; real values MUST be supplied via
// environment variables or a Secret Manager and MUST NOT be hardcoded.
//
// Environment variable mapping (see .env.example):
//
//	ECONTRACT_PROVIDER           → Provider  ("cloudsign" / "docusign" / "adobesign" / "stub")
//	ECONTRACT_BASE_URL           → BaseURL
//	ECONTRACT_API_KEY            → APIKey         (クラウドサイン等の APIキー)
//	ECONTRACT_CLIENT_ID          → ClientID        (DocuSign / Adobe Sign OAuth2 クライアント ID)
//	ECONTRACT_CLIENT_SECRET      → ClientSecret    (DocuSign / Adobe Sign OAuth2 クライアントシークレット)
//	ECONTRACT_WEBHOOK_SIGNING_KEY → WebhookSigningKey (Webhook 署名検証共有シークレット)
//	ECONTRACT_SANDBOX_MODE       → SandboxMode     ("true"/"false")
//
// 法令注記: 電子契約サービスの利用登録・認証方式・Webhook 設定手順は
// 各プロバイダの API ドキュメントおよび社労士 / 弁護士との確認が前提。
// 本設定は足場のみであり法的助言ではない。
type Config struct {
	// Provider is the identifier of the electronic-contract service.
	// Recognised values: "cloudsign", "docusign", "adobesign", "stub".
	// Default-safe: use "stub" until real credentials are configured.
	Provider string

	// SandboxMode instructs the adapter to target the sandbox environment.
	// MUST be true during development / testing.
	// Set to false ONLY after obtaining production credentials and approval.
	SandboxMode bool

	// BaseURL is the API base URL for the chosen provider.
	// Supply via ECONTRACT_BASE_URL. Must not be empty in the real adapter.
	BaseURL string

	// APIKey is the API key for providers that use static key auth (e.g. クラウドサイン).
	// Supply via ECONTRACT_API_KEY. MUST NOT be hardcoded.
	APIKey string

	// ClientID is the OAuth2 client ID (e.g. DocuSign / Adobe Sign).
	// Supply via ECONTRACT_CLIENT_ID. MUST NOT be hardcoded.
	ClientID string

	// ClientSecret is the OAuth2 client secret (e.g. DocuSign / Adobe Sign).
	// Supply via ECONTRACT_CLIENT_SECRET. MUST NOT be hardcoded.
	ClientSecret string

	// WebhookSigningKey is the HMAC shared secret for verifying inbound webhook
	// payloads from the provider. Supply via ECONTRACT_WEBHOOK_SIGNING_KEY.
	// When empty, webhook signature verification is skipped (not recommended for
	// production). MUST NOT be hardcoded.
	WebhookSigningKey string
}

// ---------------------------------------------------------------------------
// SendSignRequest / SignStatus — provider-agnostic I/O types
// ---------------------------------------------------------------------------

// SendSignRequest is the input to EContractAdapter.SendSignRequest.
//
// SECURITY: signer_ref MUST be an opaque ID — no PII (名前・メールアドレス等)
// should be included. The caller must map sensitive signer information to
// an opaque reference before constructing this struct.
type SendSignRequest struct {
	// TenantID identifies the tenant (for logging / idempotency; not sent to
	// the external service).
	TenantID uuid.UUID

	// OfferLetterID is the opaque ID of the offer_letters row.
	OfferLetterID uuid.UUID

	// IdempotencyKey prevents duplicate external submissions.
	// Callers should derive it from a stable key (e.g. offer_letter_id + attempt).
	IdempotencyKey string

	// FileRef is the opaque storage reference for the document to be signed
	// (e.g. an S3 key or internal document ID). Not a presigned URL.
	FileRef string

	// ContentHash is the SHA-256 or SHA-512 hex digest of the document for
	// tamper detection (CMP-006 truthfulness requirement).
	ContentHash string

	// SignerRef is an opaque reference to the signer (no PII).
	// The caller must resolve the real signer contact info server-side and pass
	// it to the external service separately via a privileged channel; this field
	// is only stored in offer_letters.signer_ref.
	SignerRef string

	// ExpiresAt, when non-nil, instructs the provider to expire the envelope
	// at the given time if unsigned.
	ExpiresAt *time.Time
}

// SignStatus represents the normalised status of a signing envelope as
// reported by the external provider.
type SignStatus struct {
	// EnvelopeID is the opaque external reference returned by the provider
	// (e.g. クラウドサイン 書類ID / DocuSign envelopeId). Stored in
	// offer_letters.esign_envelope_id.
	EnvelopeID string

	// Status is the provider-normalised status string.
	// Use the SignStatusXxx constants for cross-provider comparisons.
	Status string

	// SignedAt is the time the document was completed / signed, when known.
	SignedAt *time.Time

	// Provider is the adapter-reported provider label stored in
	// offer_letters.esign_provider (e.g. "cloudsign", "docusign").
	Provider string
}

// Normalised SignStatus.Status constants.
// Individual adapters MUST map their provider-specific statuses to these.
const (
	// SignStatusPending means the envelope has been sent and is awaiting signature.
	SignStatusPending = "pending"

	// SignStatusCompleted means all parties have signed.
	SignStatusCompleted = "completed"

	// SignStatusDeclined means the signer declined to sign.
	SignStatusDeclined = "declined"

	// SignStatusExpired means the envelope expired unsigned.
	SignStatusExpired = "expired"

	// SignStatusVoided means the sender voided the envelope.
	SignStatusVoided = "voided"
)

// ---------------------------------------------------------------------------
// EContractAdapter interface
// ---------------------------------------------------------------------------

// Adapter abstracts the external electronic-contract service channel.
//
// The stub implementation (stubAdapter) is used until real service credentials
// are obtained. The real adapter implements this interface without changing
// callers.
//
// Security contract:
//   - Implementations MUST NOT persist, log, or include PII / decrypted
//     sensitive data in any error message or audit record.
//   - Implementations MUST use TLS with certificate validation
//     (InsecureSkipVerify is prohibited).
//   - Implementations MUST validate that SandboxMode is false before
//     targeting the production endpoint.
//   - Implementations MUST verify inbound webhook payload authenticity using
//     the configured WebhookSigningKey before acting on the payload.
type Adapter interface {
	// SendSignRequest dispatches the document to the external service for
	// electronic signature and returns the opaque envelope reference.
	// The call is idempotent via IdempotencyKey.
	SendSignRequest(ctx context.Context, req SendSignRequest) (SignStatus, error)

	// GetSignStatus retrieves the current signing status for the given
	// envelopeID from the external service.
	GetSignStatus(ctx context.Context, envelopeID string) (SignStatus, error)

	// ProviderLabel returns the short label stored in offer_letters.esign_provider
	// (e.g. "cloudsign", "docusign", "stub").
	ProviderLabel() string

	// IsStubProvider reports whether this adapter is the no-op stub used during
	// development / testing. RegisterWebhookRoutes uses this to allow stub
	// adapters to operate without a WebhookSigningKey, while enforcing that
	// non-stub (real) adapters always have a key configured.
	IsStubProvider() bool
}

// ---------------------------------------------------------------------------
// Stub implementation (使用中: 外部認証情報取得前のデフォルト)
// ---------------------------------------------------------------------------

// stubAdapter is the MVP stub EContractAdapter.
// It makes no real network calls. All returned values are deterministic
// mocks derived from the idempotency key (冪等: same key → same ref).
//
// Replace with the real adapter when provider credentials are available.
type stubAdapter struct{}

// NewStubAdapter returns the no-op stub Adapter.
// Used by default until real provider credentials are configured.
func NewStubAdapter() Adapter {
	return stubAdapter{}
}

// SendSignRequest returns a deterministic mock envelope ID.
// No real API call is made (実送信は認証情報取得後)。
func (stubAdapter) SendSignRequest(_ context.Context, req SendSignRequest) (SignStatus, error) {
	envelopeID := fmt.Sprintf("STUB-ECONTRACT-%s", req.IdempotencyKey)
	return SignStatus{
		EnvelopeID: envelopeID,
		Status:     SignStatusPending,
		Provider:   "stub",
	}, nil
}

// GetSignStatus always returns completed for STUB-prefixed envelope IDs.
// The real adapter will poll the provider status endpoint.
func (stubAdapter) GetSignStatus(_ context.Context, envelopeID string) (SignStatus, error) {
	status := SignStatusPending
	if len(envelopeID) > 5 && envelopeID[:5] == "STUB-" {
		now := time.Now()
		return SignStatus{
			EnvelopeID: envelopeID,
			Status:     SignStatusCompleted,
			SignedAt:   &now,
			Provider:   "stub",
		}, nil
	}
	return SignStatus{
		EnvelopeID: envelopeID,
		Status:     status,
		Provider:   "stub",
	}, nil
}

// ProviderLabel returns "stub".
func (stubAdapter) ProviderLabel() string { return "stub" }

// IsStubProvider returns true — the stub adapter does not require a signing key.
func (stubAdapter) IsStubProvider() bool { return true }
