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
	"sync"
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
// When running on EC2/ECS with an appropriate IAM role, leave AccessKeyID and
// SecretAccessKey empty.  SESSender will automatically fetch temporary
// credentials from the IMDSv2 endpoint and refresh them on expiry.
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
// AWS Signature V4 signing is implemented inline (Go stdlib only; no aws-sdk-go
// dependency).  When AccessKeyID/SecretAccessKey are empty, temporary credentials
// are fetched from the EC2/ECS IMDSv2 endpoint (inline implementation, no SDK).
//
// Production operations:
//   - Configure SES sending quota monitoring and CloudWatch alarms.
//   - Enable SES event publishing (SNS topic) for bounce/complaint webhooks —
//     see BounceWebhookHandler for intake.
type SESSender struct {
	cfg     SESConfig
	baseURL string // injectable for testing; defaults to production endpoint

	// imds holds a cached set of temporary IAM-role credentials fetched from IMDSv2.
	// Protected by imdsMu; refreshed when the credentials are within 60 seconds of expiry.
	imdsMu  sync.Mutex
	imds    *imdsCredentials
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

	// Sign the request with AWS Signature V4.
	// Use static credentials when configured; otherwise fetch temporary
	// credentials from IMDSv2 (EC2/ECS instance role).
	accessKey := s.cfg.AccessKeyID
	secretKey := s.cfg.SecretAccessKey
	sessionToken := ""
	if accessKey == "" || secretKey == "" {
		creds, err := s.fetchIMDSCredentials(ctx)
		if err != nil {
			return "", fmt.Errorf("notification: ses imds credentials: %w", err)
		}
		accessKey = creds.AccessKeyID
		secretKey = creds.SecretAccessKey
		sessionToken = creds.Token
	}
	now := time.Now().UTC()
	if err := signAWSv4(req, bodyBytes, s.cfg.Region, "ses", accessKey, secretKey, sessionToken, now); err != nil {
		return "", fmt.Errorf("notification: ses sign request: %w", err)
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

func signAWSv4(req *http.Request, body []byte, region, service, accessKeyID, secretAccessKey, sessionToken string, now time.Time) error {
	datestamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")

	// Set required headers.
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("host", req.URL.Host)
	if sessionToken != "" {
		// STS temporary credentials require the security token header.
		req.Header.Set("x-amz-security-token", sessionToken)
	}

	// Canonical headers (sorted by lowercase name).
	headerNames := []string{"content-type", "host", "x-amz-date"}
	if sessionToken != "" {
		headerNames = append(headerNames, "x-amz-security-token")
	}
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
// IMDSv2 credential fetching (inline; no aws-sdk-go dependency)
//
// Reference:
//   https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/instancedata-data-retrieval.html
//   https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/iam-roles-for-amazon-ec2.html
//
// Flow:
//  1. PUT /latest/api/token (X-aws-ec2-metadata-token-ttl-seconds: 21600)
//     → receives a short-lived IMDSv2 token.
//  2. GET /latest/meta-data/iam/security-credentials/
//     (X-aws-ec2-metadata-token: <token>) → role name.
//  3. GET /latest/meta-data/iam/security-credentials/<role>
//     (X-aws-ec2-metadata-token: <token>) → JSON with AccessKeyId,
//     SecretAccessKey, Token, Expiration.
//
// Credentials are cached in the SESSender and refreshed when within 60 seconds
// of expiry to avoid per-request IMDS calls.
// ---------------------------------------------------------------------------

// imdsBase is the EC2 Instance Metadata Service v2 base URL.
// On ECS, the same endpoint is reachable via the task metadata credential endpoint
// (ECS_CONTAINER_METADATA_URI_V4/credentials).  For simplicity, this implementation
// targets the EC2 IMDS endpoint; ECS deployments can also inject static credentials
// via IAM Task Role environment variables (AWS_ACCESS_KEY_ID etc.).
const imdsBase = "http://169.254.169.254"

// imdsCredentials holds the temporary IAM credentials obtained from IMDSv2.
type imdsCredentials struct {
	AccessKeyID     string    `json:"AccessKeyId"`
	SecretAccessKey string    `json:"SecretAccessKey"`
	Token           string    `json:"Token"`
	Expiration      time.Time `json:"Expiration"`
}

// imdsHTTPClient is a dedicated HTTP client for IMDS calls.
// The 2-second timeout is intentionally short: if IMDS is unreachable
// (non-EC2/ECS environment) we want a fast failure rather than blocking.
var imdsHTTPClient = &http.Client{Timeout: 2 * time.Second}

// fetchIMDSCredentials returns temporary IAM-role credentials from the EC2/ECS
// IMDSv2 endpoint.  The result is cached; a fresh fetch is triggered only when
// the cached credentials expire within 60 seconds.
//
// SECURITY: the returned AccessKeyID, SecretAccessKey, and Token are secrets
// — they are NEVER logged, returned to callers beyond the signing step, or
// written to any persistent store.
func (s *SESSender) fetchIMDSCredentials(ctx context.Context) (*imdsCredentials, error) {
	s.imdsMu.Lock()
	defer s.imdsMu.Unlock()

	// Return cached credentials if they are still valid (more than 60s remaining).
	if s.imds != nil && time.Until(s.imds.Expiration) > 60*time.Second {
		return s.imds, nil
	}

	// Step 1: obtain a short-lived IMDSv2 session token.
	tokenReq, err := http.NewRequestWithContext(ctx, http.MethodPut,
		imdsBase+"/latest/api/token", nil)
	if err != nil {
		return nil, fmt.Errorf("imds: build token request: %w", err)
	}
	tokenReq.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "21600")
	tokenResp, err := imdsHTTPClient.Do(tokenReq)
	if err != nil {
		return nil, fmt.Errorf("imds: fetch token: %w", err)
	}
	defer tokenResp.Body.Close()
	tokenBytes, err := io.ReadAll(io.LimitReader(tokenResp.Body, 512))
	if err != nil {
		return nil, fmt.Errorf("imds: read token: %w", err)
	}
	if tokenResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("imds: token endpoint returned status %d", tokenResp.StatusCode)
	}
	imdsToken := strings.TrimSpace(string(tokenBytes))

	// Step 2: discover the IAM role name attached to this instance/task.
	roleReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		imdsBase+"/latest/meta-data/iam/security-credentials/", nil)
	if err != nil {
		return nil, fmt.Errorf("imds: build role request: %w", err)
	}
	roleReq.Header.Set("X-aws-ec2-metadata-token", imdsToken)
	roleResp, err := imdsHTTPClient.Do(roleReq)
	if err != nil {
		return nil, fmt.Errorf("imds: fetch role name: %w", err)
	}
	defer roleResp.Body.Close()
	roleBytes, err := io.ReadAll(io.LimitReader(roleResp.Body, 512))
	if err != nil {
		return nil, fmt.Errorf("imds: read role name: %w", err)
	}
	if roleResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("imds: role endpoint returned status %d", roleResp.StatusCode)
	}
	roleName := strings.TrimSpace(string(roleBytes))
	if roleName == "" {
		return nil, fmt.Errorf("imds: no IAM role attached to this instance")
	}
	// Use the first role if multiple are listed (one per line).
	if idx := strings.Index(roleName, "\n"); idx >= 0 {
		roleName = roleName[:idx]
	}
	roleName = strings.TrimSpace(roleName)

	// Step 3: fetch the temporary credentials for the discovered role.
	credURL := imdsBase + "/latest/meta-data/iam/security-credentials/" + roleName
	credReq, err := http.NewRequestWithContext(ctx, http.MethodGet, credURL, nil)
	if err != nil {
		return nil, fmt.Errorf("imds: build credentials request: %w", err)
	}
	credReq.Header.Set("X-aws-ec2-metadata-token", imdsToken)
	credResp, err := imdsHTTPClient.Do(credReq)
	if err != nil {
		return nil, fmt.Errorf("imds: fetch credentials: %w", err)
	}
	defer credResp.Body.Close()
	credBytes, err := io.ReadAll(io.LimitReader(credResp.Body, 4096))
	if err != nil {
		return nil, fmt.Errorf("imds: read credentials: %w", err)
	}
	if credResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("imds: credentials endpoint returned status %d", credResp.StatusCode)
	}

	var creds imdsCredentials
	if err := json.Unmarshal(credBytes, &creds); err != nil {
		return nil, fmt.Errorf("imds: parse credentials: %w", err)
	}
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return nil, fmt.Errorf("imds: credentials response missing AccessKeyId or SecretAccessKey")
	}

	s.imds = &creds
	return &creds, nil
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
