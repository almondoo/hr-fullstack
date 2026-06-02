// Package logging provides a shared slog-based structured logger.
//
// Security policy:
//   - NEVER log passwords, tokens, session keys, or any credential.
//   - NEVER log personally identifiable information (PII) such as names,
//     email addresses, phone numbers, national IDs (マイナンバー etc.).
//   - Log only opaque identifiers (UUIDs, request IDs) and technical metadata
//     (method, path, status code, duration, error codes without stack values).
//   - In production all log lines are JSON for structured ingestion.
package logging

import (
	"log/slog"
	"os"
)

// New creates a slog.Logger appropriate for the given environment.
//   - development: human-readable text output, DEBUG level.
//   - all other (staging / production): JSON output, INFO level.
//
// New is a pure factory: it does NOT call slog.SetDefault.
// If the caller wants to promote this logger to the package-level default
// (e.g. for third-party code that calls slog.Info directly), it should do so
// explicitly in main.go via slog.SetDefault(logger).
// Keeping this function side-effect-free prevents data races when tests call
// New concurrently with -race enabled.
func New(appEnv string) *slog.Logger {
	var (
		handler slog.Handler
		level   = slog.LevelInfo
	)

	if appEnv == "development" {
		level = slog.LevelDebug
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	} else {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	}

	return slog.New(handler)
}
