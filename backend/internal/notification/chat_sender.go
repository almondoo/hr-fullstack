package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// ---------------------------------------------------------------------------
// ChatSender — chat notification transport abstraction
//
// Mirrors the MailSender pattern: an interface behind which Slack, Teams, and
// LINE WORKS adapters (and a mock for development/test) are plugged.
//
// Security posture:
//   - Webhook URLs are secrets; they must come from environment variables /
//     Secret Manager.  They are NEVER logged, committed, or written to DB.
//   - Message bodies must never contain PII (マイナンバー/口座/健診 etc.).
//     Callers must pass only non-sensitive display text + opaque deep links.
//   - ChatDelivery records no user-identifying PII.
// ---------------------------------------------------------------------------

// ChatMessage is the payload passed to ChatSender.Send.
//
// Text must contain only non-sensitive display text.  DeepLink is an opaque
// reference; it MUST NOT embed sensitive PII.
type ChatMessage struct {
	// Text is the notification body rendered for the chat channel.
	// SECURITY: never include sensitive PII (マイナンバー/口座/健診 etc.).
	Text string
	// DeepLink is an opaque URL / reference inserted into the message.
	// SECURITY: must not encode sensitive PII in path or query parameters.
	DeepLink string
}

// ChatSender abstracts a chat notification transport (Slack / Teams / LINE WORKS).
//
// Send dispatches msg to the configured webhook endpoint and returns a
// platform-specific delivery token (or empty string on platforms that give
// none).  Implementations MUST NOT log webhook URLs or message bodies that
// could contain PII.
type ChatSender interface {
	// ChannelName returns a short identifier used in logs (e.g. "slack", "teams").
	ChannelName() string
	Send(ctx context.Context, msg ChatMessage) (deliveryToken string, err error)
}

// ---------------------------------------------------------------------------
// MockChatSender — development / test stub
// ---------------------------------------------------------------------------

// MockChatSender is the development ChatSender: it does not send anything.
// It logs only non-PII metadata (channel name + text length) and returns a
// synthetic delivery token.
type MockChatSender struct {
	// Name optionally overrides the channel name returned by ChannelName.
	Name string
}

// ChannelName implements ChatSender.
func (m MockChatSender) ChannelName() string {
	if m.Name != "" {
		return m.Name
	}
	return "mock_chat"
}

// Send implements ChatSender for development / test.  It never transmits a
// request and never logs the message body or PII.
func (m MockChatSender) Send(_ context.Context, msg ChatMessage) (string, error) {
	slog.Info("notification: mock chat send",
		"channel", m.ChannelName(),
		"text_len", len(msg.Text),
	)
	return "mock-chat-" + m.ChannelName(), nil
}

// ---------------------------------------------------------------------------
// HTTP helper shared by real senders
// ---------------------------------------------------------------------------

// httpClient is the shared transport used by webhook senders.  A 10-second
// timeout is enforced to prevent unbounded goroutine blocking.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// postJSON marshals payload, POSTs to webhookURL, and returns an error if the
// response status is non-2xx.
//
// SECURITY: webhookURL is a secret; callers must never log it.  The response
// body is consumed and discarded to free the connection; it is NOT logged.
func postJSON(ctx context.Context, webhookURL string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("chat: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("chat: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("chat: send request: %w", err)
	}
	defer resp.Body.Close()
	// Drain body to allow connection reuse; discard content (may contain PII).
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("chat: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Slack incoming webhook adapter
// ---------------------------------------------------------------------------

// SlackConfig holds the configuration for the Slack incoming webhook sender.
//
// WebhookURL must be populated from an environment variable (e.g.
// NOTIFICATION_SLACK_WEBHOOK_URL) — never hard-coded.  See .env.example for
// the placeholder.
type SlackConfig struct {
	// WebhookURL is the Slack incoming webhook URL.
	// SECURITY: treat as a secret; never log, commit, or persist in plain form.
	// Source from: NOTIFICATION_SLACK_WEBHOOK_URL environment variable.
	WebhookURL string
}

// SlackSender sends notifications via Slack incoming webhooks.
//
// This is a scaffold implementation: the transport is wired; the message
// format (Block Kit) can be extended by callers via custom templates.
//
// External dependency remaining before production use:
//   - A real NOTIFICATION_SLACK_WEBHOOK_URL must be provisioned and injected
//     at runtime.  No external Go SDK is used (plain HTTPS POST avoids
//     indirect dependency churn).
type SlackSender struct {
	cfg SlackConfig
}

// NewSlackSender constructs a SlackSender.  cfg.WebhookURL must be non-empty.
func NewSlackSender(cfg SlackConfig) (*SlackSender, error) {
	if cfg.WebhookURL == "" {
		return nil, fmt.Errorf("notification: SlackSender: WebhookURL is required")
	}
	return &SlackSender{cfg: cfg}, nil
}

// ChannelName implements ChatSender.
func (s *SlackSender) ChannelName() string { return "slack" }

// slackPayload is the JSON body sent to the Slack incoming webhook endpoint.
// https://api.slack.com/messaging/webhooks
type slackPayload struct {
	Text   string `json:"text"`
	Blocks []any  `json:"blocks,omitempty"`
}

// Send implements ChatSender for Slack.
//
// SECURITY: the webhook URL is never logged.  The message text must not
// contain PII; see ChatMessage.Text documentation.
func (s *SlackSender) Send(ctx context.Context, msg ChatMessage) (string, error) {
	text := msg.Text
	if msg.DeepLink != "" {
		text = fmt.Sprintf("%s\n<%s>", msg.Text, msg.DeepLink)
	}
	payload := slackPayload{Text: text}
	if err := postJSON(ctx, s.cfg.WebhookURL, payload); err != nil {
		return "", fmt.Errorf("notification: slack send: %w", err)
	}
	// Slack incoming webhooks return "ok" as the body but no delivery token.
	return "", nil
}

// ---------------------------------------------------------------------------
// Microsoft Teams incoming webhook adapter
// ---------------------------------------------------------------------------

// TeamsConfig holds the configuration for the Microsoft Teams incoming webhook
// sender.
//
// WebhookURL must be populated from an environment variable (e.g.
// NOTIFICATION_TEAMS_WEBHOOK_URL) — never hard-coded.
type TeamsConfig struct {
	// WebhookURL is the Microsoft Teams incoming webhook URL.
	// SECURITY: treat as a secret; never log, commit, or persist in plain form.
	// Source from: NOTIFICATION_TEAMS_WEBHOOK_URL environment variable.
	WebhookURL string
}

// TeamsSender sends notifications via Microsoft Teams incoming webhooks.
//
// Scaffold: the Adaptive Card format used here is the minimal "MessageCard"
// schema.  Upgrading to Adaptive Cards v1.x is straightforward.
//
// External dependency remaining before production use:
//   - A real NOTIFICATION_TEAMS_WEBHOOK_URL must be provisioned and injected
//     at runtime.
type TeamsSender struct {
	cfg TeamsConfig
}

// NewTeamsSender constructs a TeamsSender.  cfg.WebhookURL must be non-empty.
func NewTeamsSender(cfg TeamsConfig) (*TeamsSender, error) {
	if cfg.WebhookURL == "" {
		return nil, fmt.Errorf("notification: TeamsSender: WebhookURL is required")
	}
	return &TeamsSender{cfg: cfg}, nil
}

// ChannelName implements ChatSender.
func (t *TeamsSender) ChannelName() string { return "teams" }

// teamsPayload is the JSON body for a Teams Incoming Webhook (Legacy MessageCard).
// https://learn.microsoft.com/en-us/microsoftteams/platform/webhooks-and-connectors/how-to/connectors-using
type teamsPayload struct {
	Type       string            `json:"@type"`
	Context    string            `json:"@context"`
	Text       string            `json:"text"`
	PotentialAction []teamsAction `json:"potentialAction,omitempty"`
}

type teamsAction struct {
	Type    string       `json:"@type"`
	Name    string       `json:"name"`
	Targets []teamsTarget `json:"targets"`
}

type teamsTarget struct {
	OS  string `json:"os"`
	URI string `json:"uri"`
}

// Send implements ChatSender for Microsoft Teams.
func (t *TeamsSender) Send(ctx context.Context, msg ChatMessage) (string, error) {
	payload := teamsPayload{
		Type:    "MessageCard",
		Context: "http://schema.org/extensions",
		Text:    msg.Text,
	}
	if msg.DeepLink != "" {
		payload.PotentialAction = []teamsAction{
			{
				Type: "OpenUri",
				Name: "詳細を確認",
				Targets: []teamsTarget{
					{OS: "default", URI: msg.DeepLink},
				},
			},
		}
	}
	if err := postJSON(ctx, t.cfg.WebhookURL, payload); err != nil {
		return "", fmt.Errorf("notification: teams send: %w", err)
	}
	return "", nil
}

// ---------------------------------------------------------------------------
// LINE WORKS Bot webhook adapter
// ---------------------------------------------------------------------------

// LineWorksConfig holds the configuration for the LINE WORKS Bot sender.
//
// All fields must be populated from environment variables — never hard-coded.
// See .env.example for placeholders.
//
// LINE WORKS API 2.0 uses OAuth2 Client Credentials (Service Account + JWT).
// Token acquisition is outside this scaffold's scope; the ChannelToken field
// accepts a pre-obtained Bearer token injected at construction time.  A
// production implementation should fetch and cache the token via the
// https://developers.worksmobile.com/jp/reference/bot-send-message API.
//
// External dependencies remaining before production use:
//   - BotID, ChannelID: provision in the LINE WORKS Developer Console.
//   - ChannelToken: OAuth2 Client Credentials flow against
//     https://auth.worksmobile.com/oauth2/v2.0/token (requires Service Account
//     private key — store in Secret Manager, not env).
//   - Env vars: NOTIFICATION_LINE_WORKS_BOT_ID,
//     NOTIFICATION_LINE_WORKS_CHANNEL_ID,
//     NOTIFICATION_LINE_WORKS_CHANNEL_TOKEN.
type LineWorksConfig struct {
	// BotID is the LINE WORKS Bot ID (numeric string).
	// Source from: NOTIFICATION_LINE_WORKS_BOT_ID environment variable.
	BotID string
	// ChannelID is the target channel ID for the bot.
	// Source from: NOTIFICATION_LINE_WORKS_CHANNEL_ID environment variable.
	ChannelID string
	// ChannelToken is the pre-obtained OAuth2 Bearer token.
	// SECURITY: treat as a secret; never log or commit.
	// Source from: NOTIFICATION_LINE_WORKS_CHANNEL_TOKEN environment variable.
	ChannelToken string
}

// LineWorksSender sends notifications via the LINE WORKS Bot API 2.0.
//
// Scaffold: only text messages are sent.  Rich content (button templates,
// image maps) can be added by extending lineWorksMessage.Content.
type LineWorksSender struct {
	cfg           LineWorksConfig
	apiBase       string                  // injectable for testing; defaults to production endpoint
	// TokenProvider, when non-nil, fetches a fresh OAuth2 token dynamically via
	// the Service Account JWT flow (LineWorksTokenProvider).  When nil the sender
	// falls back to the static cfg.ChannelToken.
	//
	// SECURITY: the token returned by TokenProvider is a secret; it is placed
	// only in the Authorization header and is never logged.
	TokenProvider *LineWorksTokenProvider
}

// lineWorksAPIBase is the LINE WORKS Bot API 2.0 base URL.
const lineWorksAPIBase = "https://www.worksapis.com/v1.0"

// NewLineWorksSender constructs a LineWorksSender.  All cfg fields must be non-empty.
func NewLineWorksSender(cfg LineWorksConfig) (*LineWorksSender, error) {
	if cfg.BotID == "" || cfg.ChannelID == "" || cfg.ChannelToken == "" {
		return nil, fmt.Errorf("notification: LineWorksSender: BotID, ChannelID, and ChannelToken are all required")
	}
	return &LineWorksSender{cfg: cfg, apiBase: lineWorksAPIBase}, nil
}

// ChannelName implements ChatSender.
func (l *LineWorksSender) ChannelName() string { return "line_works" }

// lineWorksMessage is the request body for LINE WORKS Bot message API.
// https://developers.worksmobile.com/jp/reference/bot-send-message
type lineWorksMessage struct {
	Content lineWorksContent `json:"content"`
}

type lineWorksContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Send implements ChatSender for LINE WORKS.
func (l *LineWorksSender) Send(ctx context.Context, msg ChatMessage) (string, error) {
	// Resolve the bearer token: prefer the dynamic TokenProvider when set (OAuth2
	// SA flow with caching); fall back to the static ChannelToken from config.
	// SECURITY: the resolved token is a secret — never log it.
	token := l.cfg.ChannelToken
	if l.TokenProvider != nil {
		var err error
		token, err = l.TokenProvider.Token(ctx)
		if err != nil {
			return "", fmt.Errorf("notification: line_works: token fetch: %w", err)
		}
	}

	text := msg.Text
	if msg.DeepLink != "" {
		text = fmt.Sprintf("%s\n%s", msg.Text, msg.DeepLink)
	}
	payload := lineWorksMessage{
		Content: lineWorksContent{Type: "text", Text: text},
	}

	url := fmt.Sprintf("%s/bots/%s/channels/%s/messages",
		l.apiBase, l.cfg.BotID, l.cfg.ChannelID)

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("notification: line_works marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("notification: line_works build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// SECURITY: Authorization header contains a secret token — never logged.
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("notification: line_works send: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("notification: line_works unexpected status %d", resp.StatusCode)
	}
	return "", nil
}
