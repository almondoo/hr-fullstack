package notification

// ---------------------------------------------------------------------------
// BounceWebhookHandler — bounce/complaint intake from SES and SendGrid
//
// Amazon SES delivers bounce and complaint notifications via Amazon SNS.
// This handler handles:
//   - SNS SubscriptionConfirmation messages (GET or POST to subscribe).
//   - SNS Notification messages wrapping SES event payloads (bounce / complaint).
//
// SendGrid delivers webhook events via HTTPS POST (Event Webhook v3).
// This handler parses the SendGrid event array and records bounced/complained
// statuses via Service.MarkBounced.
//
// Security posture:
//   - The SES/SNS webhook endpoint is unauthenticated (called by AWS).
//     SNS message authenticity is verified via X-Amz-Sns-Topic-Arn header
//     allowlist (set via config) — full SNS message signing verification is
//     production follow-up work (requires fetching the signing cert by URL).
//   - The SendGrid webhook endpoint can optionally verify the HMAC-SHA256
//     signature using the SendGrid Event Webhook Signed Events feature.
//     Signature verification is scaffold-only here; production follow-up
//     requires provisioning the SendGrid webhook signing key.
//   - Email addresses from SNS/SendGrid payloads are hashed before lookup to
//     avoid persisting plaintext PII beyond the transit boundary.
//   - Response bodies never include PII or internal error details.
//
// Wiring: RegisterBounceWebhookRoutes must be called on an unauthenticated
// router group (separate from the tenant-authenticated /api group) because
// AWS SNS and SendGrid call these endpoints directly.
// ---------------------------------------------------------------------------

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/your-org/hr-saas/internal/platform/httpx"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// BounceWebhookConfig holds runtime configuration for the webhook handlers.
//
// All secret fields must come from environment variables — never hard-coded.
// See .env.example for placeholders.
type BounceWebhookConfig struct {
	// SNSTopicARN, when non-empty, is the expected value of the
	// X-Amz-Sns-Topic-Arn header.  Requests with a different (or absent) ARN
	// are rejected with 403.
	// Source from: NOTIFICATION_SES_SNS_TOPIC_ARN environment variable.
	SNSTopicARN string

	// SendGridWebhookSigningKey, when non-empty, enables HMAC-SHA256 signature
	// verification on SendGrid event webhook requests.
	// Source from: NOTIFICATION_SENDGRID_WEBHOOK_SIGNING_KEY environment variable.
	// SECURITY: treat as a secret; never log or commit.
	SendGridWebhookSigningKey string

	// SystemActorID is a synthetic UUID used as the actor_id in audit records
	// for webhook-originated status changes (no human actor).
	// Source from: NOTIFICATION_WEBHOOK_ACTOR_ID environment variable.
	// Defaults to uuid.Nil when empty.
	SystemActorID uuid.UUID
}

// BounceWebhookHandler handles bounce/complaint webhook payloads from SES/SNS
// and SendGrid.
type BounceWebhookHandler struct {
	svc *Service
	cfg BounceWebhookConfig
}

// NewBounceWebhookHandler constructs a BounceWebhookHandler.
func NewBounceWebhookHandler(svc *Service, cfg BounceWebhookConfig) *BounceWebhookHandler {
	return &BounceWebhookHandler{svc: svc, cfg: cfg}
}

// RegisterBounceWebhookRoutes wires the bounce/complaint webhook endpoints on
// an unauthenticated router group.
//
// Endpoints:
//
//	POST /webhooks/ses/bounce    — SNS bounce/complaint notifications (SES)
//	POST /webhooks/sendgrid      — SendGrid Event Webhook
//
// Example wiring (in server/router.go or equivalent):
//
//	webhookGroup := router.Group("/webhooks")
//	notification.RegisterBounceWebhookRoutes(webhookGroup, tdb, ...)
func RegisterBounceWebhookRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, svc *Service, cfg BounceWebhookConfig) {
	h := NewBounceWebhookHandler(svc, cfg)
	rg.POST("/ses/bounce", h.HandleSESBounce)
	rg.POST("/sendgrid", h.HandleSendGridEvent)
}

// ---------------------------------------------------------------------------
// Amazon SES / SNS bounce handler
// ---------------------------------------------------------------------------

// snsEnvelope is the top-level SNS message envelope.
// https://docs.aws.amazon.com/sns/latest/dg/sns-message-and-json-formats.html
type snsEnvelope struct {
	Type             string `json:"Type"`
	MessageID        string `json:"MessageId"`
	TopicARN         string `json:"TopicArn"`
	Message          string `json:"Message"`
	SubscribeURL     string `json:"SubscribeURL"`
	UnsubscribeURL   string `json:"UnsubscribeUrl"`
	Token            string `json:"Token"`
}

// sesBounceMessage is the SES event notification embedded in SNS Message.
// https://docs.aws.amazon.com/ses/latest/dg/notification-contents.html
type sesBounceMessage struct {
	NotificationType string    `json:"notificationType"`
	Bounce           *sesBounce `json:"bounce,omitempty"`
	Complaint        *sesComplaint `json:"complaint,omitempty"`
	Mail             sesMail   `json:"mail"`
}

type sesBounce struct {
	BounceType    string             `json:"bounceType"`
	BouncedRecipients []sesRecipient `json:"bouncedRecipients"`
}

type sesComplaint struct {
	ComplainedRecipients []sesRecipient `json:"complainedRecipients"`
}

type sesRecipient struct {
	EmailAddress string `json:"emailAddress"`
}

type sesMail struct {
	MessageID string `json:"messageId"`
}

// HandleSESBounce processes SNS notifications from Amazon SES for bounce and
// complaint events.
//
// Flow:
//  1. Validate the X-Amz-Sns-Topic-Arn header against cfg.SNSTopicARN.
//  2. Parse the SNS envelope.
//  3. For SubscriptionConfirmation: log the SubscribeURL for manual confirmation
//     (auto-confirm is a security risk — an operator must visit the URL).
//  4. For Notification: parse the SES event and call MarkBounced per recipient.
//
// Production follow-up: implement full SNS message signature verification
// (fetch signing cert by SigningCertURL, verify the Signature field).
func (h *BounceWebhookHandler) HandleSESBounce(c *gin.Context) {
	// Validate SNS topic ARN when configured.
	if h.cfg.SNSTopicARN != "" {
		gotARN := c.GetHeader("X-Amz-Sns-Topic-Arn")
		if gotARN != h.cfg.SNSTopicARN {
			slog.Warn("notification: ses bounce webhook: topic ARN mismatch",
				"expected_arn_prefix", h.cfg.SNSTopicARN[:min(len(h.cfg.SNSTopicARN), 20)],
			)
			httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "invalid topic ARN")
			return
		}
	}

	rawBody, err := io.ReadAll(io.LimitReader(c.Request.Body, 64*1024))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "failed to read body")
		return
	}

	var env snsEnvelope
	if err := json.Unmarshal(rawBody, &env); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}

	switch env.Type {
	case "SubscriptionConfirmation":
		// Log the URL for manual operator confirmation — never auto-confirm
		// (auto-confirm allows any SNS topic to subscribe, which is a SSRF risk).
		slog.Info("notification: ses bounce webhook: SNS SubscriptionConfirmation received",
			"subscribe_url_prefix", safePrefix(env.SubscribeURL, 60),
		)
		c.Status(http.StatusOK)
		return

	case "Notification":
		// Parse the SES event notification.
		var sesMsg sesBounceMessage
		if err := json.Unmarshal([]byte(env.Message), &sesMsg); err != nil {
			slog.Warn("notification: ses bounce webhook: failed to parse SES message")
			// Return 200 so SNS does not retry indefinitely on malformed payloads.
			c.Status(http.StatusOK)
			return
		}
		h.processSESNotification(c.Request.Context(), sesMsg, env.MessageID)
		c.Status(http.StatusOK)
		return

	default:
		// Unknown SNS message type — acknowledge to prevent SNS retry storms.
		slog.Warn("notification: ses bounce webhook: unknown SNS type", "type", env.Type)
		c.Status(http.StatusOK)
	}
}

// processSESNotification routes a parsed SES bounce/complaint message to the
// delivery status update logic.
func (h *BounceWebhookHandler) processSESNotification(_ context.Context, sesMsg sesBounceMessage, snsMsgID string) {
	providerMsgID := sesMsg.Mail.MessageID
	if providerMsgID == "" {
		slog.Warn("notification: ses bounce webhook: no mail messageId in SNS notification",
			"sns_message_id", snsMsgID,
		)
		return
	}

	switch sesMsg.NotificationType {
	case "Bounce":
		if sesMsg.Bounce == nil {
			return
		}
		for _, r := range sesMsg.Bounce.BouncedRecipients {
			h.markDeliveryBounced(providerMsgID, r.EmailAddress, DeliveryStatusBounced, snsMsgID)
		}
	case "Complaint":
		if sesMsg.Complaint == nil {
			return
		}
		for _, r := range sesMsg.Complaint.ComplainedRecipients {
			h.markDeliveryBounced(providerMsgID, r.EmailAddress, DeliveryStatusComplained, snsMsgID)
		}
	default:
		// Other SES notification types (e.g. Delivery) — no action needed.
	}
}

// markDeliveryBounced looks up the email_deliveries row by provider_message_id
// and email hash, then calls MarkBounced.
//
// SECURITY: emailAddress is hashed before use for lookup; it is never logged
// or persisted beyond the hash.
func (h *BounceWebhookHandler) markDeliveryBounced(providerMsgID, emailAddress, status, snsMsgID string) {
	emailHash := hashEmail(emailAddress)
	// emailAddress is scrubbed immediately after hashing.
	emailAddress = "" //nolint:ineffassign // intentional security scrub

	deliveries, err := h.svc.findDeliveriesByProviderAndHash(providerMsgID, emailHash)
	if err != nil {
		slog.Error("notification: ses bounce webhook: lookup delivery failed",
			"sns_message_id", snsMsgID,
			"error", err.Error(),
		)
		return
	}
	if len(deliveries) == 0 {
		slog.Info("notification: ses bounce webhook: no matching delivery found",
			"sns_message_id", snsMsgID,
		)
		return
	}

	for _, d := range deliveries {
		ip := "webhook"
		_, markErr := h.svc.MarkBounced(context.Background(), MarkBouncedInput{
			TenantID:   d.TenantID,
			ActorID:    h.cfg.SystemActorID,
			DeliveryID: d.ID,
			Status:     status,
			IP:         &ip,
		})
		if markErr != nil {
			slog.Warn("notification: ses bounce webhook: mark bounced failed",
				"delivery_id", d.ID.String(),
				"status", status,
				"error", markErr.Error(),
			)
		} else {
			slog.Info("notification: ses bounce webhook: delivery marked",
				"delivery_id", d.ID.String(),
				"status", status,
			)
		}
	}
}

// ---------------------------------------------------------------------------
// SendGrid Event Webhook handler
// ---------------------------------------------------------------------------

// sendGridEvent represents a single event in the SendGrid Event Webhook payload.
// https://docs.sendgrid.com/for-developers/tracking-events/event
type sendGridEvent struct {
	Event   string `json:"event"`
	Email   string `json:"email"`
	SGMsgID string `json:"sg_message_id"`
	// Timestamp is Unix seconds; kept as float64 to avoid parsing failures.
	Timestamp float64 `json:"timestamp"`
}

// HandleSendGridEvent processes the SendGrid Event Webhook payload.
//
// Flow:
//  1. Optionally verify HMAC-SHA256 signature when cfg.SendGridWebhookSigningKey is set.
//  2. Parse the event array.
//  3. For "bounce" and "spamreport" events, look up matching deliveries by
//     provider_message_id and email hash, then call MarkBounced.
//
// Production follow-up: provision the Signed Events key in the SendGrid
// dashboard and set NOTIFICATION_SENDGRID_WEBHOOK_SIGNING_KEY.
func (h *BounceWebhookHandler) HandleSendGridEvent(c *gin.Context) {
	rawBody, err := io.ReadAll(io.LimitReader(c.Request.Body, 256*1024))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "failed to read body")
		return
	}

	// Signature verification (optional; skipped when signing key is not configured).
	if h.cfg.SendGridWebhookSigningKey != "" {
		if !verifySignature(rawBody, c.GetHeader("X-Twilio-Email-Event-Webhook-Signature"), h.cfg.SendGridWebhookSigningKey) {
			slog.Warn("notification: sendgrid webhook: signature verification failed")
			httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "invalid signature")
			return
		}
	}

	var events []sendGridEvent
	if err := json.Unmarshal(rawBody, &events); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}

	for i := range events {
		ev := &events[i]
		switch ev.Event {
		case "bounce":
			h.markDeliveryBounced(ev.SGMsgID, ev.Email, DeliveryStatusBounced, fmt.Sprintf("sg-%d", i))
		case "spamreport":
			h.markDeliveryBounced(ev.SGMsgID, ev.Email, DeliveryStatusComplained, fmt.Sprintf("sg-%d", i))
		}
		// Other events (open, click, delivered, etc.) are acknowledged but not actioned.
	}

	c.Status(http.StatusOK)
}

// verifySignature verifies the HMAC-SHA256 signature from SendGrid's Signed
// Events feature.
//
// SendGrid computes: HMAC-SHA256(signingKey, timestamp+body) and base64-encodes it.
// The timestamp is delivered in X-Twilio-Email-Event-Webhook-Timestamp; here we
// verify against the body alone for scaffold simplicity.
//
// Production follow-up: include the timestamp header in the HMAC input and
// validate the timestamp is within an acceptable window (replay protection).
func verifySignature(body []byte, sigHeader, signingKey string) bool {
	if sigHeader == "" {
		return false
	}
	sigBytes, err := base64.StdEncoding.DecodeString(sigHeader)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(signingKey))
	mac.Write(body)
	expected := mac.Sum(nil)
	return hmac.Equal(expected, sigBytes)
}

// ---------------------------------------------------------------------------
// Delivery lookup by provider message ID + email hash
// ---------------------------------------------------------------------------

// findDeliveriesByProviderAndHash returns email_deliveries rows matching the
// given provider_message_id and to_email_hash across all tenants.
//
// This is a cross-tenant lookup (webhook has no tenant context).  RLS is
// bypassed intentionally here; the query is scoped tightly to the two opaque
// identifiers.  The result set is expected to be a single row in practice.
//
// NOTE: this requires a DB session with superuser or rls-bypass role, which is
// how the notification service's TenantDB connection should be configured for
// background/webhook use.  Production follow-up: wire via the existing
// privileged DB connection used by background workers.
func (s *Service) findDeliveriesByProviderAndHash(providerMsgID, emailHash string) ([]EmailDelivery, error) {
	// Scaffold: this method is a stub that returns empty until the privileged
	// DB accessor is wired.  The query is shown for review; actual DB execution
	// is deferred to the production wiring step.
	//
	// Production query (executed outside WithinTenant / without RLS):
	//
	//   SELECT id, tenant_id, notification_id, to_email_hash, provider,
	//          provider_message_id, status, attempts, max_attempts,
	//          last_error, sent_at, bounced_at, created_at, updated_at
	//   FROM email_deliveries
	//   WHERE provider_message_id = $1 AND to_email_hash = $2
	//   LIMIT 10
	//
	_ = providerMsgID
	_ = emailHash
	slog.Info("notification: findDeliveriesByProviderAndHash: scaffold stub — no DB wired",
		"provider_message_id_prefix", safePrefix(providerMsgID, 12),
	)
	return nil, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// safePrefix returns up to n bytes of s, used to log non-sensitive prefixes
// (e.g. SNS topic ARNs, message IDs) without overflowing.
func safePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
