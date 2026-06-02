package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/your-org/hr-saas/internal/platform/config"
	"github.com/your-org/hr-saas/internal/platform/logging"
	"github.com/your-org/hr-saas/internal/server"
)

// newTestConfig returns a minimal Config suitable for unit tests.
// No DB connection is required because /healthz does not touch the database.
func newTestConfig() *config.Config {
	return &config.Config{
		AppEnv:           "development",
		HTTPPort:         "8080",
		DBHost:           "localhost",
		DBPort:           "5432",
		DBUser:           "hr_app",
		DBPassword:       "test-password",
		DBName:           "hr_saas",
		DBSSLMode:        "disable",
		CORSAllowOrigins: "http://localhost:3000",
	}
}

// TestHealthzReturns200 verifies that GET /healthz always returns 200 OK with
// the expected JSON body regardless of database state.
//
// /readyz is intentionally excluded from this package-level test because it
// requires a live database connection (PingContext). Integration tests using
// testcontainers-go will cover /readyz in the test-verifier slice.
func TestHealthzReturns200(t *testing.T) {
	t.Parallel()

	cfg := newTestConfig()
	logger := logging.New(cfg.AppEnv)

	// Pass nil for the *gorm.DB — healthz never touches it.
	// readyz is not exercised here, so nil is safe.
	router := server.New(cfg, nil, logger)
	require.NotNil(t, router)

	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodGet, "/healthz", nil)
	require.NoError(t, err)

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"status":"ok"}`, w.Body.String())
}

// TestHealthzResponseHeaders verifies that the security headers expected by
// the spec are present on every response.
func TestHealthzResponseHeaders(t *testing.T) {
	t.Parallel()

	cfg := newTestConfig()
	logger := logging.New(cfg.AppEnv)
	router := server.New(cfg, nil, logger)

	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodGet, "/healthz", nil)
	require.NoError(t, err)

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "DENY", w.Header().Get("X-Frame-Options"))
}

// TestRequestIDPropagation checks that the X-Request-ID header is echoed back
// when provided by the client.
func TestRequestIDPropagation(t *testing.T) {
	t.Parallel()

	cfg := newTestConfig()
	logger := logging.New(cfg.AppEnv)
	router := server.New(cfg, nil, logger)

	const testID = "test-correlation-id-123"

	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodGet, "/healthz", nil)
	require.NoError(t, err)
	req.Header.Set("X-Request-ID", testID)

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, testID, w.Header().Get("X-Request-ID"))
}

// TestRequestIDGenerated checks that a new X-Request-ID is generated when
// the client does not supply one.
func TestRequestIDGenerated(t *testing.T) {
	t.Parallel()

	cfg := newTestConfig()
	logger := logging.New(cfg.AppEnv)
	router := server.New(cfg, nil, logger)

	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodGet, "/healthz", nil)
	require.NoError(t, err)

	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotEmpty(t, w.Header().Get("X-Request-ID"), "server must generate a request ID")
}
