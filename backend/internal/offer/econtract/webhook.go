// Package econtract — このファイルは Webhook 受信ハンドラの足場 (Issue #14).
//
// # 電子契約 Webhook 受信 足場
//
// 現状: provider 認証情報待ち (クラウドサイン / DocuSign 等のサンドボックス未取得).
// 取得後に TODO(real-econtract) 箇所を実装し、
// RegisterWebhookRoutes で実アダプタを差し替えること。
//
// 差し替え手順:
//  1. 各 provider の開発者ポータルでサンドボックスアカウントを取得し、
//     Webhook エンドポイント URL を登録する。
//  2. Webhook 署名検証キーを取得し、環境変数 ECONTRACT_WEBHOOK_SIGNING_KEY に設定。
//  3. 本ファイルの TODO(real-econtract) 箇所を実装する。
//  4. 実アダプタを NewEContractRealAdapter(cfg) で構築し、
//     offer.RegisterRoutes の WithAdapter で差し替える。
//
// Wiring: RegisterWebhookRoutes は非認証ルータグループに配置すること
// (外部 provider が直接コールするため、テナント認証済み /api グループとは分離する)。
//
// セキュリティ制約 (MUST NOT violate):
//   - TLS 証明書検証を無効化してはならない (InsecureSkipVerify 禁止).
//   - Webhook ペイロードの真正性を必ず検証すること (HMAC-SHA256 等).
//   - エラーメッセージ・ログに認証情報 / PII を含めてはならない.
//   - signer_ref には PII を格納してはならない (opaque ID のみ).
//   - リプレイ攻撃対策: タイムスタンプ検証・idempotency チェックを行うこと (TODO).
package econtract

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/audit"
	"github.com/your-org/hr-saas/internal/platform/httpx"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// WebhookHandler handles inbound webhook payloads from the electronic-contract
// service provider.
type WebhookHandler struct {
	tdb     *tenantdb.TenantDB
	cfg     Config
	adapter Adapter
}

// NewWebhookHandler constructs a WebhookHandler.
func NewWebhookHandler(tdb *tenantdb.TenantDB, cfg Config, adapter Adapter) *WebhookHandler {
	return &WebhookHandler{tdb: tdb, cfg: cfg, adapter: adapter}
}

// RegisterWebhookRoutes wires the electronic-contract webhook endpoint on an
// unauthenticated router group (called directly by the external provider).
//
// Endpoints:
//
//	POST /webhooks/econtract — inbound signing events from the provider
//
// Example wiring (in server/router.go or equivalent):
//
//	webhookGroup := router.Group("/webhooks")
//	econtract.RegisterWebhookRoutes(webhookGroup, tdb, cfg, adapter)
func RegisterWebhookRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, cfg Config, adapter Adapter) {
	h := NewWebhookHandler(tdb, cfg, adapter)
	rg.POST("/econtract", h.HandleWebhook)
}

// ---------------------------------------------------------------------------
// Canonical webhook event envelope (provider-normalised)
// ---------------------------------------------------------------------------

// webhookEvent is the normalised inbound webhook event shape.
//
// Real adapters MUST parse the provider-specific payload and map it to this
// shape before calling processEvent. This decouples the domain logic from
// provider-specific JSON structures.
//
// TODO(real-econtract): クラウドサイン / DocuSign / Adobe Sign それぞれの
// Webhook ペイロード仕様を確認し、provider ごとのパーサを実装する。
// クラウドサイン: https://app.cloudsign.jp/api-docs (要確認)
// DocuSign:      https://developers.docusign.com/platform/webhooks/ (要確認)
// Adobe Sign:    https://opensource.adobe.com/acrobat-sign/developer_guide/ (要確認)
type webhookEvent struct {
	// EnvelopeID is the opaque provider reference (書類 ID / envelopeId).
	EnvelopeID string `json:"envelope_id"`
	// Status is the provider-normalised status (use SignStatusXxx constants).
	Status string `json:"status"`
	// SignedAt is the RFC3339 completion time when status is completed.
	SignedAt *string `json:"signed_at,omitempty"`
	// OfferLetterID is the opaque offer_letters.id passed as metadata when
	// creating the signing request. Used to locate the DB row for update.
	OfferLetterID string `json:"offer_letter_id"`
	// TenantID identifies the tenant (passed as metadata when creating the
	// signing request so the webhook can scope the DB update).
	TenantID string `json:"tenant_id"`
}

// HandleWebhook processes inbound webhook events from the electronic-contract
// service provider.
//
// Flow:
//  1. Read body with limit (64 KB).
//  2. Verify HMAC-SHA256 signature when cfg.WebhookSigningKey is set.
//     When the key is empty, verification is skipped and a warning is logged
//     (not recommended for production).
//  3. Parse the normalised webhook event envelope.
//  4. Delegate to processEvent for DB update.
//
// Always returns 200 OK to prevent provider retry storms on non-retryable
// parse errors. Retryable errors (e.g. DB unavailable) return 500.
//
// Production follow-up:
//   - Implement per-provider payload parsers (TODO(real-econtract)).
//   - Add timestamp validation and replay-protection window check.
//   - Confirm Webhook signature algorithm / header name per provider API docs.
func (h *WebhookHandler) HandleWebhook(c *gin.Context) {
	rawBody, err := io.ReadAll(io.LimitReader(c.Request.Body, 64*1024))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "failed to read body")
		return
	}

	// Signature verification.
	if h.cfg.WebhookSigningKey != "" {
		sig := c.GetHeader("X-EContract-Signature")
		if !verifyHMACSHA256(rawBody, sig, h.cfg.WebhookSigningKey) {
			slog.Warn("econtract: webhook: signature verification failed",
				"provider", h.adapter.ProviderLabel(),
			)
			// Return 403 to signal the provider that the key is misconfigured,
			// rather than 200 (which would silently swallow the security failure).
			httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "invalid signature")
			return
		}
	} else {
		// Log at Warn level when signature verification is not configured.
		// In production, always set ECONTRACT_WEBHOOK_SIGNING_KEY.
		slog.Warn("econtract: webhook: signature verification skipped (WebhookSigningKey not configured)",
			"provider", h.adapter.ProviderLabel(),
		)
	}

	// TODO(real-econtract): provider-specific payload parsing.
	// The stub accepts the normalised webhookEvent shape directly.
	// Real adapters must parse the provider payload and map to webhookEvent.
	var ev webhookEvent
	if err := json.Unmarshal(rawBody, &ev); err != nil {
		slog.Warn("econtract: webhook: failed to parse event payload",
			"provider", h.adapter.ProviderLabel(),
			"error", err.Error(),
		)
		// Return 200 to prevent provider from retrying on permanently malformed
		// payloads.
		c.Status(http.StatusOK)
		return
	}

	if retryable := h.processEvent(c.Request.Context(), ev); retryable {
		c.Status(http.StatusInternalServerError)
		return
	}
	c.Status(http.StatusOK)
}

// processEvent updates offer_letters based on the normalised webhook event.
//
// Returns true when the error is retryable (DB unavailable etc.) so the caller
// can return 500 and allow the provider to retry.
//
// Transaction boundary: the offer_letters UPDATE and the audit record are
// written in a single WithinTenant transaction. A failure rolls back both,
// preserving consistency.
func (h *WebhookHandler) processEvent(ctx context.Context, ev webhookEvent) (retryable bool) {
	tenantID, err := uuid.Parse(ev.TenantID)
	if err != nil || tenantID == uuid.Nil {
		slog.Warn("econtract: webhook: invalid or missing tenant_id",
			"provider", h.adapter.ProviderLabel(),
		)
		return false // non-retryable
	}

	letterID, err := uuid.Parse(ev.OfferLetterID)
	if err != nil || letterID == uuid.Nil {
		slog.Warn("econtract: webhook: invalid or missing offer_letter_id",
			"provider", h.adapter.ProviderLabel(),
		)
		return false // non-retryable
	}

	if ev.EnvelopeID == "" {
		slog.Warn("econtract: webhook: empty envelope_id",
			"provider", h.adapter.ProviderLabel(),
		)
		return false
	}

	var signedAt *time.Time
	if ev.SignedAt != nil && *ev.SignedAt != "" {
		t, perr := time.Parse(time.RFC3339, *ev.SignedAt)
		if perr != nil {
			slog.Warn("econtract: webhook: invalid signed_at format",
				"provider", h.adapter.ProviderLabel(),
			)
			return false
		}
		signedAt = &t
	}

	systemActorID := uuid.Nil // webhook has no human actor

	dbErr := h.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		// Idempotent update: only write when the row has not already been marked
		// completed for this envelope (prevents duplicate webhook replays).
		res := tx.Exec(
			`UPDATE offer_letters
			 SET    esign_provider    = ?,
			        esign_envelope_id = ?,
			        signed_at         = ?,
			        updated_at        = now()
			 WHERE  id        = ?
			   AND  tenant_id = ?
			   AND  (signed_at IS NULL OR esign_envelope_id = ?)`,
			h.adapter.ProviderLabel(), ev.EnvelopeID, signedAt,
			letterID, tenantID,
			ev.EnvelopeID,
		)
		if res.Error != nil {
			return res.Error
		}

		idStr := letterID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     tenantID,
			UserID:       &systemActorID,
			Action:       "offer_letter.esign_webhook_received",
			ResourceType: "offer_letter",
			ResourceID:   &idStr,
		})
	})
	if dbErr != nil {
		slog.Error("econtract: webhook: DB update failed",
			"offer_letter_id", letterID.String(),
			"provider", h.adapter.ProviderLabel(),
			"error", dbErr.Error(),
		)
		return true // retryable
	}

	slog.Info("econtract: webhook: processed",
		"offer_letter_id", letterID.String(),
		"provider", h.adapter.ProviderLabel(),
		"status", ev.Status,
	)
	return false
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// verifyHMACSHA256 verifies an HMAC-SHA256 hex-encoded signature over body.
//
// The provider is expected to send:
//
//	X-EContract-Signature: hex(HMAC-SHA256(signingKey, body))
//
// TODO(real-econtract): confirm the exact header name, encoding (hex vs base64),
// and HMAC input (body only vs timestamp+body) per each provider's API docs.
// クラウドサイン / DocuSign / Adobe Sign それぞれで異なる可能性がある。
func verifyHMACSHA256(body []byte, sigHeader, signingKey string) bool {
	if sigHeader == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(signingKey))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(sigHeader))
}
