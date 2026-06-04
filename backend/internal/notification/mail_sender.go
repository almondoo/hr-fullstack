package notification

// ---------------------------------------------------------------------------
// MailSender — production email transport adapters
//
// This file provides scaffold implementations of the MailSender interface for
// Amazon SES (via SES API v2 HTTPS) and SendGrid (via Mail Send API v3).
// Both adapters are intentionally SDK-free (plain HTTPS POST) to avoid
// indirect dependency churn, following the same pattern as chat_sender.go.
//
// Security posture:
//   - API keys / credentials are injected via constructor config structs and
//     MUST originate from environment variables or a Secret Manager — they are
//     NEVER logged, committed, or written to DB.
//   - The recipient email address is PII: it is passed to Send() as plaintext
//     only for transmission; implementations must not log it or include it in
//     error messages.
//   - Log entries record only non-PII metadata (hash prefix, subject length).
//
// External dependencies remaining before production use:
//   - SES:      AWS_REGION, NOTIFICATION_SES_FROM_ADDRESS,
//               NOTIFICATION_SES_ACCESS_KEY_ID, NOTIFICATION_SES_SECRET_ACCESS_KEY
//               (or an EC2/ECS IAM role that grants ses:SendEmail).
//               AWS Signature V4 signing is implemented inline (no aws-sdk-go).
//   - SendGrid: NOTIFICATION_SENDGRID_API_KEY, NOTIFICATION_SENDGRID_FROM_ADDRESS.
//
// SPF / DKIM / DMARC configuration is documented in docs/email_deliverability.md.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Amazon SES adapter (SES API v2, SendEmail action)
// ---------------------------------------------------------------------------

// SESConfig holds the configuration for the Amazon SES v2 mail sender.
//
// All credential fields must be populated from environment variables or an IAM
// instance role — they must NEVER be hard-coded or committed.
// See .env.example for placeholders.
//
// When running on EC2/ECS/Lambda with an appropriate IAM role, set
// AccessKeyID and SecretAccessKey to empty strings; the SES API will use the
// instance metadata credentials instead (this scaffold does not implement IMDSv2
// credential fetching — that is production follow-up work).
type SESConfig struct {
	// Region is the AWS region for the SES endpoint (e.g. "ap-northeast-1").
	// Source from: AWS_REGION environment variable.
	Region string
	// FromAddress is the verified sender email address or "Display Name <addr@example.com>".
	// SECURITY: this is non-PII config; it may be logged at startup only.
	// Source from: NOTIFICATION_SES_FROM_ADDRESS environment variable.
	FromAddress string
	// AccessKeyID is the AWS access key id for SES request signing.
	// SECURITY: treat as a secret; never log, commit, or persist.
	// Source from: NOTIFICATION_SES_ACCESS_KEY_ID environment variable.
	// Leave empty to rely on EC2/ECS instance role (production recommendation).
	AccessKeyID string
	// SecretAccessKey is the AWS secret access key for SES request signing.
	// SECURITY: treat as a secret; never log, commit, or persist.
	// Source from: NOTIFICATION_SES_SECRET_ACCESS_KEY environment variable.
	// Leave empty to rely on EC2/ECS instance role (production recommendation).
	SecretAccessKey string
}

// SESSender sends transactional email via Amazon SES API v2.
//
// Scaffold status: the AWS Signature V4 signing is implemented inline so no
// external AWS SDK dependency is required.  The HMAC-SHA256 implementation is
// Go stdlib only.
//
// Production follow-up:
//   - Implement IMDSv2 credential fetching when AccessKeyID/SecretAccessKey are
//     empty (required for EC2/ECS deployments without static credentials).
//   - Configure SES sending quota monitoring and CloudWatch alarms.
//   - Enable SES event publishing (SNS topic) for bounce/complaint webhooks —
//     see BounceWebhookHandler for intake.
type SESSender struct {
	cfg     SESConfig
	baseURL string // injectable for testing; defaults to production endpoint
}

// sesSendV2URL returns the SES API v2 SendEmail endpoint for the given region.
func sesSendV2URL(region string) string {
	return fmt.Sprintf("https://email.%s.amazonaws.com/v2/email/outbound-emails", region)
}

// NewSESSender constructs a SESSender.  Region and FromAddress must be non-empty.
func NewSESSender(cfg SESConfig) (*SESSender, error) {
	if cfg.Region == "" {
		return nil, fmt.Errorf("notification: SESSender: Region is required")
	}
	if cfg.FromAddress == "" {
		return nil, fmt.Errorf("notification: SESSender: FromAddress is required")
	}
	return &SESSender{cfg: cfg, baseURL: sesSendV2URL(cfg.Region)}, nil
}

// sesEmailPayload is the JSON body for the SES v2 SendEmail API.
// https://docs.aws.amazon.com/ses/latest/APIReference-V2/API_SendEmail.html
type sesEmailPayload struct {
	FromEmailAddress string       `json:"FromEmailAddress"`
	Destination      sesDestination `json:"Destination"`
	Content          sesContent   `json:"Content"`
}

type sesDestination struct {
	ToAddresses []string `json:"ToAddresses"`
}

type sesContent struct {
	Simple sesSimpleContent `json:"Simple"`
}

type sesSimpleContent struct {
	Subject sesMessagePart `json:"Subject"`
	Body    sesBody        `json:"Body"`
}

type sesMessagePart struct {
	Data    string `json:"Data"`
	Charset string `json:"Charset"`
}

type sesBody struct {
	Text sesMessagePart `json:"Text"`
	HTML sesMessagePart `json:"Html,omitempty"`
}

// Send implements MailSender for Amazon SES.
//
// SECURITY: the recipient address (to) is passed to the HTTP request but is
// never logged or included in error messages.  API credentials are never logged.
func (s *SESSender) Send(ctx context.Context, to, subject, body string) (string, error) {
	payload := sesEmailPayload{
		FromEmailAddress: s.cfg.FromAddress,
		Destination:      sesDestination{ToAddresses: []string{to}},
		Content: sesContent{
			Simple: sesSimpleContent{
				Subject: sesMessagePart{Data: subject, Charset: "UTF-8"},
				Body: sesBody{
					Text: sesMessagePart{Data: body, Charset: "UTF-8"},
				},
			},
		},
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("notification: ses marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("notification: ses build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Sign the request with AWS Signature V4 when static credentials are
	// configured.  When credentials are empty, the request is sent unsigned
	// (suitable only for local testing behind a signing proxy or with STS
	// credentials injected at the transport layer).
	if s.cfg.AccessKeyID != "" && s.cfg.SecretAccessKey != "" {
		now := time.Now().UTC()
		if err := signAWSv4(req, bodyBytes, s.cfg.Region, "ses", s.cfg.AccessKeyID, s.cfg.SecretAccessKey, now); err != nil {
			return "", fmt.Errorf("notification: ses sign request: %w", err)
		}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("notification: ses send request: %w", err)
	}
	defer resp.Body.Close()

	// Read a bounded portion of the response to extract the message ID on
	// success, or log a non-PII error category on failure.
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Log status only — never the response body (may contain PII in error messages).
		slog.Warn("notification: ses send failed",
			"status", resp.StatusCode,
			"subject_len", len(subject),
		)
		return "", fmt.Errorf("notification: ses unexpected status %d", resp.StatusCode)
	}

	// Extract MessageId from the JSON response.
	var result struct {
		MessageID string `json:"MessageId"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil || result.MessageID == "" {
		// Non-fatal: we sent successfully but couldn't parse the ID.
		return "ses-unknown-id", nil
	}
	return result.MessageID, nil
}

// ---------------------------------------------------------------------------
// AWS Signature V4 signing (stdlib only; no aws-sdk-go dependency)
// ---------------------------------------------------------------------------
//
// Reference: https://docs.aws.amazon.com/general/latest/gr/sigv4_signing.html
// This is a minimal implementation covering the SendEmail use case (POST with
// JSON body, single region/service).  It is NOT a general-purpose AWS signer.

func signAWSv4(req *http.Request, body []byte, region, service, accessKeyID, secretAccessKey string, now time.Time) error {
	datestamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")

	// Set required headers.
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("host", req.URL.Host)

	// Canonical headers (sorted by lowercase name).
	headerNames := []string{"content-type", "host", "x-amz-date"}
	sort.Strings(headerNames)
	var canonHeaders strings.Builder
	for _, h := range headerNames {
		canonHeaders.WriteString(h)
		canonHeaders.WriteString(":")
		canonHeaders.WriteString(strings.TrimSpace(req.Header.Get(h)))
		canonHeaders.WriteString("\n")
	}
	signedHeaders := strings.Join(headerNames, ";")

	// Payload hash.
	payloadHash := sha256hex(body)

	// Canonical request.
	canonReq := strings.Join([]string{
		req.Method,
		req.URL.Path,
		req.URL.RawQuery,
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	// Credential scope.
	credScope := strings.Join([]string{datestamp, region, service, "aws4_request"}, "/")

	// String to sign.
	strToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credScope,
		sha256hex([]byte(canonReq)),
	}, "\n")

	// Signing key derivation.
	signingKey := deriveSigningKey(secretAccessKey, datestamp, region, service)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(strToSign)))

	// Authorization header.
	authHeader := fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKeyID, credScope, signedHeaders, signature,
	)
	req.Header.Set("Authorization", authHeader)
	return nil
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func deriveSigningKey(secretAccessKey, datestamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secretAccessKey), []byte(datestamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

// ---------------------------------------------------------------------------
// SendGrid adapter (Mail Send API v3)
// ---------------------------------------------------------------------------

// SendGridConfig holds the configuration for the SendGrid mail sender.
//
// All credential fields must be populated from environment variables — they
// must NEVER be hard-coded or committed.  See .env.example for placeholders.
type SendGridConfig struct {
	// APIKey is the SendGrid API key with Mail Send permission.
	// SECURITY: treat as a secret; never log, commit, or persist in plain form.
	// Source from: NOTIFICATION_SENDGRID_API_KEY environment variable.
	APIKey string
	// FromAddress is the verified sender email address or "Display Name <addr@example.com>".
	// SECURITY: this is non-PII config; it may be logged at startup only.
	// Source from: NOTIFICATION_SENDGRID_FROM_ADDRESS environment variable.
	FromAddress string
}

// sendGridAPIBase is the SendGrid Mail Send API v3 endpoint.
const sendGridAPIBase = "https://api.sendgrid.com/v3/mail/send"

// SendGridSender sends transactional email via the SendGrid Mail Send API v3.
//
// Scaffold status: the transport is wired; no external SendGrid SDK is used
// (plain HTTPS POST avoids indirect dependency churn).
//
// Production follow-up:
//   - Enable SendGrid Event Webhook for bounce/complaint intake — see
//     BounceWebhookHandler in bounce_webhook.go.
//   - Configure SendGrid's IP Warmup and Dedicated IP when sending volume
//     warrants it.
//   - Enable Click/Open tracking settings as appropriate for your privacy policy.
type SendGridSender struct {
	cfg     SendGridConfig
	apiBase string // injectable for testing; defaults to sendGridAPIBase
}

// NewSendGridSender constructs a SendGridSender.
// Both APIKey and FromAddress must be non-empty.
func NewSendGridSender(cfg SendGridConfig) (*SendGridSender, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("notification: SendGridSender: APIKey is required")
	}
	if cfg.FromAddress == "" {
		return nil, fmt.Errorf("notification: SendGridSender: FromAddress is required")
	}
	return &SendGridSender{cfg: cfg, apiBase: sendGridAPIBase}, nil
}

// sendGridPayload is the JSON body for the SendGrid Mail Send API.
// https://docs.sendgrid.com/api-reference/mail-send/mail-send
type sendGridPayload struct {
	From             sendGridAddress       `json:"from"`
	Subject          string                `json:"subject"`
	Personalizations []sendGridPersonalisation `json:"personalizations"`
	Content          []sendGridContent     `json:"content"`
}

type sendGridAddress struct {
	Email string `json:"email"`
}

type sendGridPersonalisation struct {
	To []sendGridAddress `json:"to"`
}

type sendGridContent struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// Send implements MailSender for SendGrid.
//
// SECURITY: the recipient address (to) is passed in the HTTP request body but
// is never logged or included in error messages.  The API key is never logged.
func (s *SendGridSender) Send(ctx context.Context, to, subject, body string) (string, error) {
	payload := sendGridPayload{
		From:    sendGridAddress{Email: s.cfg.FromAddress},
		Subject: subject,
		Personalizations: []sendGridPersonalisation{
			{To: []sendGridAddress{{Email: to}}},
		},
		Content: []sendGridContent{
			{Type: "text/plain", Value: body},
		},
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("notification: sendgrid marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiBase, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("notification: sendgrid build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// SECURITY: Authorization header contains a secret API key — never logged.
	req.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("notification: sendgrid send request: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("notification: sendgrid send failed",
			"status", resp.StatusCode,
			"subject_len", len(subject),
		)
		return "", fmt.Errorf("notification: sendgrid unexpected status %d", resp.StatusCode)
	}

	// SendGrid returns 202 Accepted with an X-Message-Id header.
	msgID := resp.Header.Get("X-Message-Id")
	if msgID == "" {
		msgID = "sendgrid-unknown-id"
	}
	return msgID, nil
}
