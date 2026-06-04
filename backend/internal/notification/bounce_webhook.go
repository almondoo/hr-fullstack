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
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // SNS message signing uses SHA1 per AWS spec
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

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
	// Signature fields for message authenticity verification.
	Signature        string `json:"Signature"`
	SigningCertURL   string `json:"SigningCertURL"`
	Subject          string `json:"Subject"`
	Timestamp        string `json:"Timestamp"`
	SignatureVersion string `json:"SignatureVersion"`
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
//  3. Verify the SNS message signature (fail-closed): fetch the signing cert by
//     SigningCertURL (SSRF-guarded to the exact sns.<region>.amazonaws.com host
//     and /SimpleNotificationService-*.pem path, redirects disabled), then
//     verify the RSA-SHA1 signature per the AWS SNS specification.
//  4. For SubscriptionConfirmation: log the SubscribeURL for manual confirmation
//     (auto-confirm is a security risk — an operator must visit the URL).
//  5. For Notification: parse the SES event and call MarkBounced per recipient.
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

	// SNS message signature verification — fail-closed.
	// We verify for both Notification and SubscriptionConfirmation types.
	if err := verifySNSSignature(env); err != nil {
		slog.Warn("notification: ses bounce webhook: SNS signature verification failed",
			"error", err.Error(),
		)
		httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "SNS signature invalid")
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

// verifySNSSignature verifies the RSA-SHA1 signature of an SNS message.
//
// Algorithm (per AWS docs):
//  1. Validate SigningCertURL is the exact SNS cert endpoint
//     (sns.<region>.amazonaws.com host + /SimpleNotificationService-*.pem path)
//     with redirects disabled on the fetch (SSRF guard).
//  2. Fetch the PEM certificate from the validated URL.
//  3. Build the canonical string-to-sign from the message fields (type-dependent).
//  4. Decode the base64 Signature field and verify against the RSA public key.
//
// Fail-closed: any error (missing fields, invalid cert, bad URL, verify failure)
// returns an error so the caller can reject the request.
//
// SNS message signing uses SHA1 as mandated by the AWS SNS specification.
// This is a protocol requirement, not a security choice.
func verifySNSSignature(env snsEnvelope) error {
	if env.SigningCertURL == "" || env.Signature == "" {
		return fmt.Errorf("sns: missing SigningCertURL or Signature")
	}

	// SSRF guard: the signing cert URL must be an https URL on amazonaws.com.
	certURL, err := url.Parse(env.SigningCertURL)
	if err != nil {
		return fmt.Errorf("sns: invalid SigningCertURL: %w", err)
	}
	if certURL.Scheme != "https" {
		return fmt.Errorf("sns: SigningCertURL must use https")
	}
	// Validate against the exact SNS signing-cert endpoint, not any
	// *.amazonaws.com host: an attacker who can host content on any
	// amazonaws.com subdomain (e.g. an S3 bucket like evil.s3.amazonaws.com)
	// could otherwise serve a forged signing certificate and bypass signature
	// verification entirely.
	host := certURL.Hostname()
	if !snsCertHostRe.MatchString(host) {
		return fmt.Errorf("sns: SigningCertURL host %q is not an SNS signing endpoint", host)
	}
	if !strings.HasPrefix(certURL.Path, "/SimpleNotificationService-") || !strings.HasSuffix(certURL.Path, ".pem") {
		return fmt.Errorf("sns: SigningCertURL path %q is not a valid SNS cert path", certURL.Path)
	}

	// Fetch the signing certificate (bounded response, short timeout).
	certResp, err := snsHTTPClient.Get(env.SigningCertURL) //nolint:noctx // short-lived cert fetch; context would require plumbing
	if err != nil {
		return fmt.Errorf("sns: fetch signing cert: %w", err)
	}
	defer certResp.Body.Close()
	certPEM, err := io.ReadAll(io.LimitReader(certResp.Body, 32*1024))
	if err != nil {
		return fmt.Errorf("sns: read signing cert: %w", err)
	}

	// Parse PEM → DER → x509 certificate → RSA public key.
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return fmt.Errorf("sns: no PEM block found in signing cert response")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("sns: parse signing cert: %w", err)
	}
	rsaPub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("sns: signing cert does not contain an RSA public key")
	}

	// Build the canonical string to sign (field order is type-dependent per AWS docs).
	strToSign := snsStringToSign(env)

	// Decode the base64 signature.
	sigBytes, err := base64.StdEncoding.DecodeString(env.Signature)
	if err != nil {
		return fmt.Errorf("sns: decode signature: %w", err)
	}

	// Verify RSA-SHA1 signature (AWS SNS mandates SHA1).
	h := sha1.New() //nolint:gosec // required by AWS SNS message signing spec
	h.Write([]byte(strToSign))
	digest := h.Sum(nil)
	if err := rsa.VerifyPKCS1v15(rsaPub, 0, digest, sigBytes); err != nil {
		return fmt.Errorf("sns: signature verification failed: %w", err)
	}
	return nil
}

// snsStringToSign builds the canonical string for SNS message signature
// verification per the AWS SNS specification.
// https://docs.aws.amazon.com/sns/latest/dg/sns-verify-signature-of-message.html
func snsStringToSign(env snsEnvelope) string {
	var b strings.Builder
	switch env.Type {
	case "Notification":
		b.WriteString("Message\n")
		b.WriteString(env.Message)
		b.WriteString("\n")
		b.WriteString("MessageId\n")
		b.WriteString(env.MessageID)
		b.WriteString("\n")
		if env.Subject != "" {
			b.WriteString("Subject\n")
			b.WriteString(env.Subject)
			b.WriteString("\n")
		}
		b.WriteString("Timestamp\n")
		b.WriteString(env.Timestamp)
		b.WriteString("\n")
		b.WriteString("TopicArn\n")
		b.WriteString(env.TopicARN)
		b.WriteString("\n")
		b.WriteString("Type\n")
		b.WriteString(env.Type)
		b.WriteString("\n")
	default: // SubscriptionConfirmation, UnsubscribeConfirmation
		b.WriteString("Message\n")
		b.WriteString(env.Message)
		b.WriteString("\n")
		b.WriteString("MessageId\n")
		b.WriteString(env.MessageID)
		b.WriteString("\n")
		b.WriteString("SubscribeURL\n")
		b.WriteString(env.SubscribeURL)
		b.WriteString("\n")
		b.WriteString("Timestamp\n")
		b.WriteString(env.Timestamp)
		b.WriteString("\n")
		b.WriteString("Token\n")
		b.WriteString(env.Token)
		b.WriteString("\n")
		b.WriteString("TopicArn\n")
		b.WriteString(env.TopicARN)
		b.WriteString("\n")
		b.WriteString("Type\n")
		b.WriteString(env.Type)
		b.WriteString("\n")
	}
	return b.String()
}

// snsCertHostRe matches the exact host AWS uses for SNS signing certificates:
// sns.<region>.amazonaws.com.  A loose ".amazonaws.com" suffix check is
// insufficient (see verifySNSSignature for the bypass it would allow).
var snsCertHostRe = regexp.MustCompile(`^sns\.[a-z0-9-]+\.amazonaws\.com$`)

// snsHTTPClient is a dedicated HTTP client for fetching SNS signing certificates.
// It uses a short timeout and disables redirect-following (CheckRedirect returns
// ErrUseLastResponse) so a 3xx from the allowlisted host cannot redirect the
// fetch to an attacker-controlled location and punch through the SSRF allowlist.
var snsHTTPClient = &http.Client{
	Timeout: 5 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
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

// sendGridWebhookMaxAge is the maximum acceptable age of a SendGrid webhook
// event, used for replay-attack prevention.  Events with a timestamp header
// older than this window are rejected.
const sendGridWebhookMaxAge = 5 * time.Minute

// HandleSendGridEvent processes the SendGrid Event Webhook payload.
//
// Flow:
//  1. Read the raw body (bounded read).
//  2. When cfg.SendGridWebhookSigningKey is set:
//     a. Validate the X-Twilio-Email-Event-Webhook-Timestamp is within the
//        allowed window (replay protection).
//     b. Verify the HMAC-SHA256 signature with timestamp+body as the input
//        (per SendGrid Signed Events specification).
//  3. Parse the event array.
//  4. For "bounce" and "spamreport" events, look up matching deliveries by
//     provider_message_id and email hash, then call MarkBounced.
func (h *BounceWebhookHandler) HandleSendGridEvent(c *gin.Context) {
	rawBody, err := io.ReadAll(io.LimitReader(c.Request.Body, 256*1024))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "failed to read body")
		return
	}

	// Signature + timestamp replay-protection (when signing key is configured).
	if h.cfg.SendGridWebhookSigningKey != "" {
		tsHeader := c.GetHeader("X-Twilio-Email-Event-Webhook-Timestamp")
		sigHeader := c.GetHeader("X-Twilio-Email-Event-Webhook-Signature")

		// Timestamp replay-protection: reject events outside the allowed window.
		if !verifyTimestamp(tsHeader, sendGridWebhookMaxAge) {
			slog.Warn("notification: sendgrid webhook: timestamp outside allowed window")
			httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "invalid timestamp")
			return
		}

		// Signature verification: HMAC-SHA256(key, timestamp + body).
		if !verifySignature(rawBody, tsHeader, sigHeader, h.cfg.SendGridWebhookSigningKey) {
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

// verifyTimestamp returns true if the Unix-second timestamp string is within
// maxAge of the current time.  An empty or unparseable timestamp always returns
// false (fail-closed).
func verifyTimestamp(tsHeader string, maxAge time.Duration) bool {
	if tsHeader == "" {
		return false
	}
	tsSec, err := strconv.ParseFloat(tsHeader, 64)
	if err != nil {
		return false
	}
	tsTime := time.Unix(int64(tsSec), int64((tsSec-math.Trunc(tsSec))*1e9))
	age := time.Since(tsTime)
	if age < 0 {
		age = -age // future timestamps from clock skew
	}
	return age <= maxAge
}

// verifySignature verifies the HMAC-SHA256 signature from SendGrid's Signed
// Events feature.
//
// Per the SendGrid specification the HMAC input is the concatenation of the
// timestamp header value and the raw request body (no separator):
//
//	HMAC-SHA256(signingKey, timestampHeader + rawBody)
//
// The result is base64-encoded and compared against the X-Twilio-Email-Event-Webhook-Signature header.
func verifySignature(body []byte, tsHeader, sigHeader, signingKey string) bool {
	if sigHeader == "" {
		return false
	}
	sigBytes, err := base64.StdEncoding.DecodeString(sigHeader)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(signingKey))
	// Input is timestamp + body, concatenated (no separator), per SendGrid spec.
	mac.Write([]byte(tsHeader))
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
// This is a cross-tenant lookup (webhook has no tenant context).  It uses
// s.systemDB — a *gorm.DB connection that bypasses RLS — scoped tightly to
// the two opaque identifiers.  The result set is expected to be a single row
// in practice.
//
// SECURITY: provider_message_id and to_email_hash are opaque identifiers;
// neither reveals PII.  systemDB MUST be the BYPASSRLS connection; it MUST NOT
// be used for any other purpose.
func (s *Service) findDeliveriesByProviderAndHash(providerMsgID, emailHash string) ([]EmailDelivery, error) {
	if s.systemDB == nil {
		// systemDB not wired — log and return empty (safe degraded mode).
		slog.Warn("notification: findDeliveriesByProviderAndHash: systemDB not configured; webhook bounce lookup skipped",
			"provider_message_id_prefix", safePrefix(providerMsgID, 12),
		)
		return nil, nil
	}

	var deliveries []EmailDelivery
	// Execute on systemDB (BYPASSRLS connection) — no WithinTenant wrapper.
	// Query uses placeholder parameters only (no string concatenation).
	result := s.systemDB.Raw(
		`SELECT id, tenant_id, notification_id, to_email_hash,
		        provider, provider_message_id, status, attempts, max_attempts,
		        last_error, sent_at, bounced_at, created_at, updated_at
		 FROM email_deliveries
		 WHERE provider_message_id = ? AND to_email_hash = ?
		 LIMIT 10`,
		providerMsgID, emailHash,
	).Scan(&deliveries)
	if result.Error != nil {
		return nil, fmt.Errorf("notification: findDeliveriesByProviderAndHash: %w", result.Error)
	}
	return deliveries, nil
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
