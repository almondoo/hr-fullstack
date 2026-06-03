package notification_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/your-org/hr-saas/internal/notification"
)

// ---------------------------------------------------------------------------
// MockChatSender
// ---------------------------------------------------------------------------

func TestMockChatSender_Send(t *testing.T) {
	s := notification.MockChatSender{Name: "test_mock"}
	assert.Equal(t, "test_mock", s.ChannelName())

	token, err := s.Send(context.Background(), notification.ChatMessage{
		Text:     "テスト通知",
		DeepLink: "https://example.com/deep",
	})
	require.NoError(t, err)
	assert.Contains(t, token, "mock-chat-")
}

func TestMockChatSender_DefaultName(t *testing.T) {
	s := notification.MockChatSender{}
	assert.Equal(t, "mock_chat", s.ChannelName())
}

// ---------------------------------------------------------------------------
// SlackSender construction guard
// ---------------------------------------------------------------------------

func TestNewSlackSender_RequiresWebhookURL(t *testing.T) {
	_, err := notification.NewSlackSender(notification.SlackConfig{WebhookURL: ""})
	require.Error(t, err)
}

func TestSlackSender_Send(t *testing.T) {
	// Use an httptest server to capture the outbound request without a real
	// Slack endpoint.
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody = make([]byte, r.ContentLength)
		_, _ = r.Body.Read(gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender, err := notification.NewSlackSender(notification.SlackConfig{WebhookURL: srv.URL})
	require.NoError(t, err)
	assert.Equal(t, "slack", sender.ChannelName())

	_, err = sender.Send(context.Background(), notification.ChatMessage{
		Text:     "承認が必要です",
		DeepLink: "https://app.example.com/tasks/1",
	})
	require.NoError(t, err)
	assert.Contains(t, string(gotBody), "承認が必要です")
}

func TestSlackSender_Send_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	sender, err := notification.NewSlackSender(notification.SlackConfig{WebhookURL: srv.URL})
	require.NoError(t, err)

	_, err = sender.Send(context.Background(), notification.ChatMessage{Text: "test"})
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// TeamsSender construction guard
// ---------------------------------------------------------------------------

func TestNewTeamsSender_RequiresWebhookURL(t *testing.T) {
	_, err := notification.NewTeamsSender(notification.TeamsConfig{WebhookURL: ""})
	require.Error(t, err)
}

func TestTeamsSender_Send(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody = make([]byte, r.ContentLength)
		_, _ = r.Body.Read(gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender, err := notification.NewTeamsSender(notification.TeamsConfig{WebhookURL: srv.URL})
	require.NoError(t, err)
	assert.Equal(t, "teams", sender.ChannelName())

	_, err = sender.Send(context.Background(), notification.ChatMessage{
		Text:     "Teams通知テスト",
		DeepLink: "https://app.example.com/tasks/2",
	})
	require.NoError(t, err)
	assert.Contains(t, string(gotBody), "Teams通知テスト")
	assert.Contains(t, string(gotBody), "MessageCard")
}

// ---------------------------------------------------------------------------
// LineWorksSender construction guard
// ---------------------------------------------------------------------------

func TestNewLineWorksSender_RequiresAllFields(t *testing.T) {
	cases := []struct {
		name string
		cfg  notification.LineWorksConfig
	}{
		{"missing BotID", notification.LineWorksConfig{ChannelID: "c", ChannelToken: "t"}},
		{"missing ChannelID", notification.LineWorksConfig{BotID: "b", ChannelToken: "t"}},
		{"missing ChannelToken", notification.LineWorksConfig{BotID: "b", ChannelID: "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := notification.NewLineWorksSender(tc.cfg)
			require.Error(t, err)
		})
	}
}

func TestLineWorksSender_ChannelName(t *testing.T) {
	// Construction should succeed with all fields provided (even if the URL is unreachable).
	sender, err := notification.NewLineWorksSender(notification.LineWorksConfig{
		BotID: "12345", ChannelID: "67890", ChannelToken: "dummy-token",
	})
	require.NoError(t, err)
	assert.Equal(t, "line_works", sender.ChannelName())
}

// ---------------------------------------------------------------------------
// Channel constants
// ---------------------------------------------------------------------------

func TestChannelConstants(t *testing.T) {
	assert.Equal(t, "slack", notification.ChannelSlack)
	assert.Equal(t, "teams", notification.ChannelTeams)
	assert.Equal(t, "line_works", notification.ChannelLineWorks)
}

// ---------------------------------------------------------------------------
// NewServiceWithChat wiring (compile-time coverage)
// ---------------------------------------------------------------------------

func TestNewServiceWithChat_NilMailerFallsBack(t *testing.T) {
	// DB-less construction: just verify the constructor does not panic and
	// accepts ChatSender arguments without error.
	mock := notification.MockChatSender{Name: "slack"}
	// NewServiceWithChat with nil tdb is not usable for real calls, but the
	// constructor must not panic.
	svc := notification.NewServiceWithChat(nil, nil, mock)
	assert.NotNil(t, svc)
}
