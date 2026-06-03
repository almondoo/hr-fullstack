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
//   - リプレイ攻撃対策: タイムスタンプ検証・idempotency チェックを行うこと.
package econtract

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/audit"
	"github.com/your-org/hr-saas/internal/platform/httpx"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// replayWindowSeconds is the maximum allowed age (and future drift) of a
// webhook timestamp before the request is rejected as a potential replay.
const replayWindowSeconds = 5 * 60 // 5 minutes

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
// Security: Routes are registered only when the adapter is a stub provider
// OR when WebhookSigningKey is configured. For non-stub providers with an
// empty key, this function returns a non-nil error and no route is registered,
// preventing accidental unauthenticated webhook exposure in production.
//
// Example wiring (in server/router.go or equivalent):
//
//	webhookGroup := router.Group("/webhooks")
//	if err := econtract.RegisterWebhookRoutes(webhookGroup, tdb, cfg, adapter); err != nil {
//	    log.Fatalf("econtract webhook: %v", err)
//	}
func RegisterWebhookRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, cfg Config, adapter Adapter) error {
	if !adapter.IsStubProvider() && cfg.WebhookSigningKey == "" {
		return fmt.Errorf(
			"econtract: RegisterWebhookRoutes: WebhookSigningKey must be set for non-stub provider %q",
			adapter.ProviderLabel(),
		)
	}
	h := NewWebhookHandler(tdb, cfg, adapter)
	rg.POST("/econtract", h.HandleWebhook)
	return nil
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
//  2. Fail-closed: if WebhookSigningKey is not configured, return 503 immediately.
//  3. Verify X-EContract-Timestamp: reject requests outside the 5-minute window.
//  4. Verify HMAC-SHA256 signature over "<timestamp>.<body>".
//  5. Parse the normalised webhook event envelope.
//  6. Delegate to processEvent for idempotency check + DB update.
//
// Always returns 200 OK to prevent provider retry storms on non-retryable
// parse errors. Retryable errors (e.g. DB unavailable) return 500.
//
// Production follow-up:
//   - Implement per-provider payload parsers (TODO(real-econtract)).
//   - Confirm Webhook signature algorithm / header name per provider API docs.
func (h *WebhookHandler) HandleWebhook(c *gin.Context) {
	// Fail-closed: refuse all requests when signing key is not configured.
	if h.cfg.WebhookSigningKey == "" {
		httpx.RespondError(c, http.StatusServiceUnavailable, "UNCONFIGURED",
			"webhook signature verification not configured")
		return
	}

	rawBody, err := io.ReadAll(io.LimitReader(c.Request.Body, 64*1024))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "failed to read body")
		return
	}

	// Timestamp validation — replay protection step 1.
	tsHeader := c.GetHeader("X-EContract-Timestamp")
	if tsHeader == "" {
		httpx.RespondError(c, http.StatusBadRequest, "MISSING_TIMESTAMP",
			"X-EContract-Timestamp header required")
		return
	}
	tsUnix, parseErr := strconv.ParseInt(tsHeader, 10, 64)
	if parseErr != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_TIMESTAMP",
			"X-EContract-Timestamp must be a Unix epoch integer")
		return
	}
	diff := time.Now().Unix() - tsUnix
	if math.Abs(float64(diff)) > replayWindowSeconds {
		slog.Warn("econtract: webhook: timestamp outside replay window",
			"provider", h.adapter.ProviderLabel(),
		)
		httpx.RespondError(c, http.StatusBadRequest, "TIMESTAMP_EXPIRED",
			"request timestamp outside acceptable window")
		return
	}

	// Signature verification — signed over "<timestamp>.<body>".
	sig := c.GetHeader("X-EContract-Signature")
	if !verifyHMACSHA256(tsHeader, rawBody, sig, h.cfg.WebhookSigningKey) {
		slog.Warn("econtract: webhook: signature verification failed",
			"provider", h.adapter.ProviderLabel(),
		)
		// Return 403 to signal the provider that the key is misconfigured,
		// rather than 200 (which would silently swallow the security failure).
		httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "invalid signature")
		return
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
// Transaction boundary: the idempotency INSERT, the offer_letters UPDATE, and
// the audit record are written in a single WithinTenant transaction. A failure
// rolls back all three, preserving consistency. When the idempotency row already
// exists (duplicate delivery), RowsAffected==0 and processing is skipped without
// error, returning 200 to the provider.
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
	provider := h.adapter.ProviderLabel()

	dbErr := h.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		// Idempotency check: attempt to record this event. If the row already
		// exists (same tenant_id + provider + envelope_id + status), ON CONFLICT
		// DO NOTHING returns RowsAffected == 0, and we skip the rest.
		idempRes := tx.Exec(
			`INSERT INTO econtract_webhook_events
			   (tenant_id, provider, envelope_id, status, received_at)
			 VALUES (?, ?, ?, ?, now())
			 ON CONFLICT (tenant_id, provider, envelope_id, status) DO NOTHING`,
			tenantID, provider, ev.EnvelopeID, ev.Status,
		)
		if idempRes.Error != nil {
			return idempRes.Error
		}
		if idempRes.RowsAffected == 0 {
			// Already processed — skip silently (idempotent delivery).
			slog.Info("econtract: webhook: duplicate event skipped",
				"provider", provider,
			)
			return nil
		}

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
			provider, ev.EnvelopeID, signedAt,
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
			"provider", provider,
			"error", dbErr.Error(),
		)
		return true // retryable
	}

	slog.Info("econtract: webhook: processed",
		"offer_letter_id", letterID.String(),
		"provider", provider,
		"status", ev.Status,
	)
	return false
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// verifyHMACSHA256 verifies an HMAC-SHA256 hex-encoded signature over
// "<timestamp>.<body>".
//
// The provider is expected to send:
//
//	X-EContract-Timestamp: <unix-epoch>
//	X-EContract-Signature: hex(HMAC-SHA256(signingKey, timestamp + "." + body))
//
// Including the timestamp in the HMAC input binds the signature to the specific
// request time, preventing replay attacks where a valid signature is reused
// with a modified timestamp header.
//
// TODO(real-econtract): confirm the exact header name, encoding (hex vs base64),
// and HMAC input per each provider's API docs.
// クラウドサイン / DocuSign / Adobe Sign それぞれで異なる可能性がある。
func verifyHMACSHA256(timestamp string, body []byte, sigHeader, signingKey string) bool {
	if sigHeader == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(signingKey))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(sigHeader))
}
