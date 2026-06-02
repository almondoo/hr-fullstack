// Package auth_test contains integration tests for signup, login, logout, RBAC,
// CSRF, and audit log hash chain.
//
// Requires Docker (testcontainers/postgres:17-alpine).
// Run with: go test ./internal/auth/... -race -v
// Skip with: go test ./internal/auth/... -short
package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/audit"
	"github.com/your-org/hr-saas/internal/platform/config"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/platform/testdb"
	"github.com/your-org/hr-saas/internal/server"
	"log/slog"
	"os"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func init() {
	gin.SetMode(gin.TestMode)
}

func newTestConfig() *config.Config {
	return &config.Config{
		AppEnv:                "development",
		HTTPPort:              "8080",
		DBHost:                "localhost",
		DBPort:                "5432",
		DBUser:                "hr_app",
		DBPassword:            "test-password",
		DBName:                "hr_saas",
		DBSSLMode:             "disable",
		CORSAllowOrigins:      "http://localhost:3000",
		SessionCookieName:     "hr_session",
		SessionTTL:            24 * time.Hour,
		SessionCookieSecure:   false,
		SessionCookieSameSite: "lax",
		AuthRateLimit:         "100-M", // high limit for tests
	}
}

func newTestServer(h *testdb.Harness, cfg *config.Config) http.Handler {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	tdb := tenantdb.New(h.AppDB)
	store := platformauth.NewSessionStore()
	deps := server.Deps{
		AppDB:        h.AppDB,
		TenantDB:     tdb,
		SessionStore: store,
	}
	r := server.New(cfg, deps, logger)
	return server.Handler(r)
}

// postJSON sends a POST request with a JSON body and returns the recorder.
// It sets Origin: http://localhost:3000 which gorilla/csrf accepts as a trusted
// origin (matching testConfig's CORSAllowOrigins), avoiding the HTTPS Referer check.
func postJSON(t *testing.T, handler http.Handler, path string, body interface{}, cookies []*http.Cookie, csrfToken string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req, err := http.NewRequest(http.MethodPost, path, &buf)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	// Origin header satisfies gorilla/csrf's trusted-origin check in test mode.
	// Must match CORSAllowOrigins in newTestConfig().
	req.Header.Set("Origin", "http://localhost:3000")
	if csrfToken != "" {
		req.Header.Set("X-CSRF-Token", csrfToken)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// getReq sends a GET request and returns the recorder.
func getReq(t *testing.T, handler http.Handler, path string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, path, nil)
	require.NoError(t, err)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// csrfToken fetches the CSRF token via GET /api/v1/csrf.
func csrfToken(t *testing.T, handler http.Handler) (string, []*http.Cookie) {
	t.Helper()
	w := getReq(t, handler, "/api/v1/csrf", nil)
	require.Equal(t, http.StatusOK, w.Code, "csrf endpoint: %s", w.Body.String())
	var resp map[string]string
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	token := resp["csrf_token"]
	require.NotEmpty(t, token)
	return token, w.Result().Cookies()
}

// extractSessionCookie finds the session cookie in a response.
func extractSessionCookie(resp *http.Response, cookieName string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			return c
		}
	}
	return nil
}

// truncateAll resets all tenant-scoped tables between tests.
func truncateAll(h *testdb.Harness) {
	h.TruncateTables("audit_logs", "sessions", "users", "roles", "employees", "departments", "tenants")
}

// ---------------------------------------------------------------------------
// Signup tests
// ---------------------------------------------------------------------------

func TestSignup_Success(t *testing.T) {
	h := testdb.NewHarness(t)
	cfg := newTestConfig()
	handler := newTestServer(h, cfg)

	csrfTok, csrfCookies := csrfToken(t, handler)

	w := postJSON(t, handler, "/api/v1/auth/signup", map[string]string{
		"tenant_name": "Acme Corp",
		"slug":        "acme-corp",
		"email":       "admin@acme.test",
		"password":    "supersecret123",
	}, csrfCookies, csrfTok)

	assert.Equal(t, http.StatusCreated, w.Code, "signup: %s", w.Body.String())

	// Auto-login: session cookie should be set.
	sessionCookie := extractSessionCookie(w.Result(), cfg.SessionCookieName)
	require.NotNil(t, sessionCookie, "session cookie must be set after signup")
	assert.NotEmpty(t, sessionCookie.Value)

	// Verify tenant exists in DB.
	var count int64
	require.NoError(t, h.AdminDB.Raw("SELECT COUNT(*) FROM tenants WHERE slug = ?", "acme-corp").Scan(&count).Error)
	assert.Equal(t, int64(1), count)

	// Verify admin role was created with * permission.
	var roleCount int64
	require.NoError(t, h.AdminDB.Raw("SELECT COUNT(*) FROM roles WHERE name = 'admin'").Scan(&roleCount).Error)
	assert.Equal(t, int64(1), roleCount)

	// Verify admin user was created.
	var userCount int64
	require.NoError(t, h.AdminDB.Raw("SELECT COUNT(*) FROM users WHERE email = ?", "admin@acme.test").Scan(&userCount).Error)
	assert.Equal(t, int64(1), userCount)
}

func TestSignup_SlugConflict(t *testing.T) {
	h := testdb.NewHarness(t)
	cfg := newTestConfig()
	handler := newTestServer(h, cfg)

	csrfTok, csrfCookies := csrfToken(t, handler)

	// First signup.
	w := postJSON(t, handler, "/api/v1/auth/signup", map[string]string{
		"tenant_name": "Acme Corp",
		"slug":        "acme-dup",
		"email":       "admin@acme.test",
		"password":    "supersecret123",
	}, csrfCookies, csrfTok)
	require.Equal(t, http.StatusCreated, w.Code)

	// Refresh CSRF token (cookies may have changed).
	csrfTok2, csrfCookies2 := csrfToken(t, handler)

	// Second signup with same slug.
	w2 := postJSON(t, handler, "/api/v1/auth/signup", map[string]string{
		"tenant_name": "Acme Duplicate",
		"slug":        "acme-dup",
		"email":       "other@acme.test",
		"password":    "supersecret123",
	}, csrfCookies2, csrfTok2)
	assert.Equal(t, http.StatusConflict, w2.Code, "duplicate slug: %s", w2.Body.String())
	var resp map[string]string
	require.NoError(t, json.NewDecoder(w2.Body).Decode(&resp))
	assert.Equal(t, "SLUG_TAKEN", resp["code"])
}

func TestSignup_InvalidInput(t *testing.T) {
	h := testdb.NewHarness(t)
	cfg := newTestConfig()
	handler := newTestServer(h, cfg)

	csrfTok, csrfCookies := csrfToken(t, handler)

	// Short password.
	w := postJSON(t, handler, "/api/v1/auth/signup", map[string]string{
		"tenant_name": "X",
		"slug":        "valid-slug",
		"email":       "a@b.com",
		"password":    "short",
	}, csrfCookies, csrfTok)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ---------------------------------------------------------------------------
// Login tests
// ---------------------------------------------------------------------------

// signupAndLogin signs up a fresh tenant and returns the session cookie + user_id.
func signupAndLogin(t *testing.T, handler http.Handler, cfg *config.Config, slug, email, password string) *http.Cookie {
	t.Helper()
	csrfTok, csrfCookies := csrfToken(t, handler)
	w := postJSON(t, handler, "/api/v1/auth/signup", map[string]string{
		"tenant_name": "Test Tenant",
		"slug":        slug,
		"email":       email,
		"password":    password,
	}, csrfCookies, csrfTok)
	require.Equal(t, http.StatusCreated, w.Code, "signup: %s", w.Body.String())
	cookie := extractSessionCookie(w.Result(), cfg.SessionCookieName)
	require.NotNil(t, cookie)
	return cookie
}

func TestLogin_Success(t *testing.T) {
	h := testdb.NewHarness(t)
	cfg := newTestConfig()
	handler := newTestServer(h, cfg)

	// Signup first.
	csrfTok, csrfCookies := csrfToken(t, handler)
	w := postJSON(t, handler, "/api/v1/auth/signup", map[string]string{
		"tenant_name": "Login Corp",
		"slug":        "login-corp",
		"email":       "admin@login.test",
		"password":    "mysecretpass",
	}, csrfCookies, csrfTok)
	require.Equal(t, http.StatusCreated, w.Code)

	// Login.
	csrfTok2, csrfCookies2 := csrfToken(t, handler)
	w2 := postJSON(t, handler, "/api/v1/auth/login", map[string]string{
		"slug":     "login-corp",
		"email":    "admin@login.test",
		"password": "mysecretpass",
	}, csrfCookies2, csrfTok2)
	assert.Equal(t, http.StatusOK, w2.Code, "login: %s", w2.Body.String())

	sessionCookie := extractSessionCookie(w2.Result(), cfg.SessionCookieName)
	require.NotNil(t, sessionCookie, "session cookie must be set on login")

	// Verify last_login_at was updated.
	var lastLogin *time.Time
	require.NoError(t, h.AdminDB.Raw(
		"SELECT last_login_at FROM users WHERE email = ?", "admin@login.test",
	).Scan(&lastLogin).Error)
	require.NotNil(t, lastLogin, "last_login_at must be set after login")
}

func TestLogin_FailedCountAndLockout(t *testing.T) {
	h := testdb.NewHarness(t)
	cfg := newTestConfig()
	handler := newTestServer(h, cfg)

	// Signup.
	csrfTok, csrfCookies := csrfToken(t, handler)
	w := postJSON(t, handler, "/api/v1/auth/signup", map[string]string{
		"tenant_name": "Lock Corp",
		"slug":        "lock-corp",
		"email":       "admin@lock.test",
		"password":    "correctpassword123",
	}, csrfCookies, csrfTok)
	require.Equal(t, http.StatusCreated, w.Code)

	// Attempt login with wrong password 5 times.
	for i := 0; i < 5; i++ {
		csrfTok3, csrfCookies3 := csrfToken(t, handler)
		w3 := postJSON(t, handler, "/api/v1/auth/login", map[string]string{
			"slug":     "lock-corp",
			"email":    "admin@lock.test",
			"password": "wrongpassword",
		}, csrfCookies3, csrfTok3)
		assert.Equal(t, http.StatusUnauthorized, w3.Code, "attempt %d: %s", i+1, w3.Body.String())
		var resp map[string]string
		require.NoError(t, json.NewDecoder(w3.Body).Decode(&resp))
		// Must use generic error message.
		assert.Equal(t, "INVALID_CREDENTIALS", resp["code"])
		assert.Equal(t, "invalid credentials", resp["message"])
	}

	// After 5 failures, verify failed_login_count = 5 and locked_until is set.
	var failCount int
	var lockedUntil *time.Time
	require.NoError(t, h.AdminDB.Raw(
		"SELECT failed_login_count, locked_until FROM users WHERE email = ?", "admin@lock.test",
	).Row().Scan(&failCount, &lockedUntil))
	assert.Equal(t, 5, failCount)
	require.NotNil(t, lockedUntil, "locked_until must be set after threshold")
	assert.True(t, lockedUntil.After(time.Now()), "locked_until must be in the future")

	// Even with correct password, locked account should be denied.
	csrfTok4, csrfCookies4 := csrfToken(t, handler)
	wLocked := postJSON(t, handler, "/api/v1/auth/login", map[string]string{
		"slug":     "lock-corp",
		"email":    "admin@lock.test",
		"password": "correctpassword123",
	}, csrfCookies4, csrfTok4)
	assert.Equal(t, http.StatusUnauthorized, wLocked.Code, "locked: %s", wLocked.Body.String())
}

func TestLogin_WrongPassword_GenericError(t *testing.T) {
	h := testdb.NewHarness(t)
	cfg := newTestConfig()
	handler := newTestServer(h, cfg)

	csrfTok, csrfCookies := csrfToken(t, handler)
	_ = postJSON(t, handler, "/api/v1/auth/signup", map[string]string{
		"tenant_name": "Generic Corp",
		"slug":        "generic-corp",
		"email":       "admin@generic.test",
		"password":    "realpassword123",
	}, csrfCookies, csrfTok)

	csrfTok2, csrfCookies2 := csrfToken(t, handler)
	w := postJSON(t, handler, "/api/v1/auth/login", map[string]string{
		"slug":     "generic-corp",
		"email":    "admin@generic.test",
		"password": "wrongpassword",
	}, csrfCookies2, csrfTok2)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	var resp map[string]string
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, "INVALID_CREDENTIALS", resp["code"])
	// The message must not reveal which field is wrong.
	assert.Equal(t, "invalid credentials", resp["message"])
}

func TestLogin_UnknownSlug_GenericError(t *testing.T) {
	h := testdb.NewHarness(t)
	cfg := newTestConfig()
	handler := newTestServer(h, cfg)

	csrfTok, csrfCookies := csrfToken(t, handler)
	w := postJSON(t, handler, "/api/v1/auth/login", map[string]string{
		"slug":     "nonexistent-slug",
		"email":    "nobody@example.test",
		"password": "password123",
	}, csrfCookies, csrfTok)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	var resp map[string]string
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, "INVALID_CREDENTIALS", resp["code"])
}

// ---------------------------------------------------------------------------
// Logout tests
// ---------------------------------------------------------------------------

func TestLogout_RevokesSession(t *testing.T) {
	h := testdb.NewHarness(t)
	cfg := newTestConfig()
	handler := newTestServer(h, cfg)

	// Signup (auto-login).
	sessionCookie := signupAndLogin(t, handler, cfg, "logout-corp", "admin@logout.test", "password123!")

	// GET /me should succeed.
	w := getReq(t, handler, "/api/v1/auth/me", []*http.Cookie{sessionCookie})
	assert.Equal(t, http.StatusOK, w.Code, "me before logout: %s", w.Body.String())

	// Logout.
	csrfTok, csrfCookies := csrfToken(t, handler)
	allCookies := append(csrfCookies, sessionCookie)
	wLogout := postJSON(t, handler, "/api/v1/auth/logout", nil, allCookies, csrfTok)
	assert.Equal(t, http.StatusOK, wLogout.Code, "logout: %s", wLogout.Body.String())

	// /me should now return 401.
	wAfter := getReq(t, handler, "/api/v1/auth/me", []*http.Cookie{sessionCookie})
	assert.Equal(t, http.StatusUnauthorized, wAfter.Code, "me after logout must be 401")
}

func TestLogout_RequiresAuth(t *testing.T) {
	h := testdb.NewHarness(t)
	cfg := newTestConfig()
	handler := newTestServer(h, cfg)

	csrfTok, csrfCookies := csrfToken(t, handler)
	w := postJSON(t, handler, "/api/v1/auth/logout", nil, csrfCookies, csrfTok)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ---------------------------------------------------------------------------
// Session expiry E2E
// ---------------------------------------------------------------------------

func TestSession_ExpiredCookieReturns401(t *testing.T) {
	h := testdb.NewHarness(t)
	ctx := context.Background()
	cfg := newTestConfig()
	handler := newTestServer(h, cfg)

	// Signup to get a tenant + user.
	csrfTok, csrfCookies := csrfToken(t, handler)
	w := postJSON(t, handler, "/api/v1/auth/signup", map[string]string{
		"tenant_name": "Expiry Corp",
		"slug":        "expiry-corp",
		"email":       "admin@expiry.test",
		"password":    "expirepassword!",
	}, csrfCookies, csrfTok)
	require.Equal(t, http.StatusCreated, w.Code)

	// Fetch the tenant and user IDs (scan as string to avoid pgx uuid type conversion).
	var tenantIDStr string
	require.NoError(t, h.AdminDB.Raw("SELECT id::text FROM tenants WHERE slug = ?", "expiry-corp").Scan(&tenantIDStr).Error)
	tenantID, err := uuid.Parse(tenantIDStr)
	require.NoError(t, err)

	var userIDStr string
	require.NoError(t, h.AdminDB.Raw("SELECT id::text FROM users WHERE email = ?", "admin@expiry.test").Scan(&userIDStr).Error)
	userID, err := uuid.Parse(userIDStr)
	require.NoError(t, err)

	// Create a session that expires immediately.
	tdb := tenantdb.New(h.AppDB)
	store := platformauth.NewSessionStore()
	rawToken, err := store.Create(ctx, tdb, tenantID, userID, -1*time.Second, nil)
	require.NoError(t, err)

	expiredCookie := &http.Cookie{Name: cfg.SessionCookieName, Value: rawToken}
	wMe := getReq(t, handler, "/api/v1/auth/me", []*http.Cookie{expiredCookie})
	assert.Equal(t, http.StatusUnauthorized, wMe.Code)
}

// ---------------------------------------------------------------------------
// CSRF tests
// ---------------------------------------------------------------------------

func TestCSRF_MissingToken_Returns403(t *testing.T) {
	h := testdb.NewHarness(t)
	cfg := newTestConfig()
	handler := newTestServer(h, cfg)

	// POST without CSRF token should fail.
	var buf bytes.Buffer
	require.NoError(t, json.NewEncoder(&buf).Encode(map[string]string{
		"tenant_name": "CSRF Corp",
		"slug":        "csrf-corp",
		"email":       "a@b.com",
		"password":    "supersecret123",
	}))
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/auth/signup", &buf)
	req.Header.Set("Content-Type", "application/json")
	// Intentionally no X-CSRF-Token header.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code, "missing CSRF token must be 403: %s", w.Body.String())
}

func TestCSRF_InvalidToken_Returns403(t *testing.T) {
	h := testdb.NewHarness(t)
	cfg := newTestConfig()
	handler := newTestServer(h, cfg)

	// Fetch real CSRF cookies but send wrong token.
	_, csrfCookies := csrfToken(t, handler)

	w := postJSON(t, handler, "/api/v1/auth/signup", map[string]string{
		"tenant_name": "CSRF Invalid",
		"slug":        "csrf-invalid",
		"email":       "a@b.com",
		"password":    "supersecret123",
	}, csrfCookies, "invalid-csrf-token-value")
	assert.Equal(t, http.StatusForbidden, w.Code, "invalid CSRF token must be 403: %s", w.Body.String())
}

func TestCSRF_ValidToken_Succeeds(t *testing.T) {
	h := testdb.NewHarness(t)
	cfg := newTestConfig()
	handler := newTestServer(h, cfg)

	// Fetch CSRF token + cookie.
	csrfTok, csrfCookies := csrfToken(t, handler)

	// POST with valid CSRF token should succeed.
	w := postJSON(t, handler, "/api/v1/auth/signup", map[string]string{
		"tenant_name": "CSRF OK Corp",
		"slug":        "csrf-ok-corp",
		"email":       "admin@csrfok.test",
		"password":    "supersecret123",
	}, csrfCookies, csrfTok)
	assert.Equal(t, http.StatusCreated, w.Code, "valid CSRF: %s", w.Body.String())
}

// ---------------------------------------------------------------------------
// RBAC tests
// ---------------------------------------------------------------------------

// seedRBACFixture creates a tenant with two users: an admin and a read-only user.
func seedRBACFixture(t *testing.T, h *testdb.Harness) (tenantID, adminID, readonlyID uuid.UUID, adminPW, readonlyPW string) {
	t.Helper()
	ctx := context.Background()
	tdb := tenantdb.New(h.AppDB)

	tenantID = uuid.New()
	adminRoleID := uuid.New()
	readonlyRoleID := uuid.New()
	adminID = uuid.New()
	readonlyID = uuid.New()

	adminPW = "admin_password123"
	readonlyPW = "readonly_password123"
	adminHash, _ := platformauth.HashPassword(adminPW)
	readonlyHash, _ := platformauth.HashPassword(readonlyPW)

	adminPerms, _ := json.Marshal(map[string][]string{"perms": {"*"}})
	readPerms, _ := json.Marshal(map[string][]string{"perms": {"employee:read"}})

	require.NoError(t, tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		if err := tx.Exec(
			`INSERT INTO tenants (id, name, plan_code, status, slug) VALUES (?, 'RBAC Corp', 'free', 'active', ?)`,
			tenantID, "rbac-corp-"+tenantID.String()[:8],
		).Error; err != nil {
			return err
		}
		if err := tx.Exec(
			`INSERT INTO roles (id, tenant_id, name, permissions) VALUES (?, ?, 'admin', ?)`,
			adminRoleID, tenantID, adminPerms,
		).Error; err != nil {
			return err
		}
		if err := tx.Exec(
			`INSERT INTO roles (id, tenant_id, name, permissions) VALUES (?, ?, 'readonly', ?)`,
			readonlyRoleID, tenantID, readPerms,
		).Error; err != nil {
			return err
		}
		if err := tx.Exec(
			`INSERT INTO users (id, tenant_id, email, password_hash, role_id, status) VALUES (?, ?, ?, ?, ?, 'active')`,
			adminID, tenantID, "admin@rbac.test", adminHash, adminRoleID,
		).Error; err != nil {
			return err
		}
		return tx.Exec(
			`INSERT INTO users (id, tenant_id, email, password_hash, role_id, status) VALUES (?, ?, ?, ?, ?, 'active')`,
			readonlyID, tenantID, "readonly@rbac.test", readonlyHash, readonlyRoleID,
		).Error
	}))

	return
}

func TestRBAC_RequirePermission_AllowsAdmin(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	store := platformauth.NewSessionStore()

	_, adminID, _, _, _ := seedRBACFixture(t, h)

	// Find tenant ID for admin user (scan as string to avoid pgx UUID type issues).
	var tenantIDStr string
	require.NoError(t, h.AdminDB.Raw("SELECT tenant_id::text FROM users WHERE id = ?", adminID.String()).Scan(&tenantIDStr).Error)
	tenantID, err := uuid.Parse(tenantIDStr)
	require.NoError(t, err)

	// Create session for admin.
	ctx := context.Background()
	rawToken, err := store.Create(ctx, tdb, tenantID, adminID, 1*time.Hour, nil)
	require.NoError(t, err)

	r := gin.New()
	r.Use(platformauth.RequireAuth(store, h.AppDB, tdb, "hr_session"))
	r.GET("/protected", platformauth.RequirePermission(tdb, "employee:write"), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req, _ := http.NewRequest(http.MethodGet, "/protected", nil)
	req.AddCookie(&http.Cookie{Name: "hr_session", Value: rawToken})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code, "admin should pass RequirePermission: %s", w.Body.String())
}

func TestRBAC_RequirePermission_DeniesReadonly(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	store := platformauth.NewSessionStore()

	_, _, readonlyID, _, _ := seedRBACFixture(t, h)

	var tenantIDStr string
	require.NoError(t, h.AdminDB.Raw("SELECT tenant_id::text FROM users WHERE id = ?", readonlyID.String()).Scan(&tenantIDStr).Error)
	tenantID, err := uuid.Parse(tenantIDStr)
	require.NoError(t, err)

	ctx := context.Background()
	rawToken, err := store.Create(ctx, tdb, tenantID, readonlyID, 1*time.Hour, nil)
	require.NoError(t, err)

	r := gin.New()
	r.Use(platformauth.RequireAuth(store, h.AppDB, tdb, "hr_session"))
	r.GET("/protected", platformauth.RequirePermission(tdb, "employee:write"), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req, _ := http.NewRequest(http.MethodGet, "/protected", nil)
	req.AddCookie(&http.Cookie{Name: "hr_session", Value: rawToken})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code, "readonly user must be denied employee:write")
}

func TestRBAC_CrossTenantIsolation(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	store := platformauth.NewSessionStore()

	// Create two separate tenants.
	tenantIDA, _, _, _, _ := seedRBACFixture(t, h)
	tenantIDB, adminIDB, _, _, _ := seedRBACFixture(t, h)

	// Confirm they are different tenants.
	require.NotEqual(t, tenantIDA, tenantIDB)

	ctx := context.Background()
	// Create session for tenant B's admin.
	rawToken, err := store.Create(ctx, tdb, tenantIDB, adminIDB, 1*time.Hour, nil)
	require.NoError(t, err)

	// Endpoint checks for tenant B admin's permission (should have *).
	r := gin.New()
	r.Use(platformauth.RequireAuth(store, h.AppDB, tdb, "hr_session"))
	r.GET("/check", platformauth.RequirePermission(tdb, "employee:write"), func(c *gin.Context) {
		// Confirm the tenant in context is tenantB, not tenantA.
		gotTenantID := platformauth.TenantIDFrom(c)
		if gotTenantID != tenantIDB {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "wrong tenant"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req, _ := http.NewRequest(http.MethodGet, "/check", nil)
	req.AddCookie(&http.Cookie{Name: "hr_session", Value: rawToken})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code, "cross tenant: %s", w.Body.String())
}

// ---------------------------------------------------------------------------
// HasPermission unit tests
// ---------------------------------------------------------------------------

func TestHasPermission(t *testing.T) {
	cases := []struct {
		perms []string
		need  string
		want  bool
	}{
		{[]string{"*"}, "employee:read", true},
		{[]string{"*"}, "anything", true},
		{[]string{"employee:*"}, "employee:read", true},
		{[]string{"employee:*"}, "employee:write", true},
		{[]string{"employee:*"}, "tenant:admin", false},
		{[]string{"employee:read"}, "employee:read", true},
		{[]string{"employee:read"}, "employee:write", false},
		{[]string{}, "employee:read", false},
		{[]string{"employee:read", "tenant:admin"}, "tenant:admin", true},
	}
	for _, tc := range cases {
		got := platformauth.HasPermission(tc.perms, tc.need)
		assert.Equal(t, tc.want, got, "perms=%v need=%s", tc.perms, tc.need)
	}
}

// ---------------------------------------------------------------------------
// Audit log hash chain tests
// ---------------------------------------------------------------------------

func TestAudit_RecordAndVerifyChain(t *testing.T) {
	h := testdb.NewHarness(t)
	ctx := context.Background()
	tdb := tenantdb.New(h.AppDB)

	tenantID := uuid.New()
	// Insert tenant as admin (bypasses RLS).
	require.NoError(t, h.AdminDB.Exec(
		"INSERT INTO tenants (id, name, plan_code, status, slug) VALUES (?, 'Audit Corp', 'free', 'active', ?)",
		tenantID, "audit-corp-"+tenantID.String()[:8],
	).Error)

	userID := uuid.New()
	require.NoError(t, tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Exec(
			"INSERT INTO users (id, tenant_id, email, status) VALUES (?, ?, 'audit@test.test', 'active')",
			userID, tenantID,
		).Error
	}))

	// Record several audit events in separate transactions.
	entries := []audit.Entry{
		{TenantID: tenantID, UserID: &userID, Action: "user.created", ResourceType: "user", ResourceID: strPtr(userID.String())},
		{TenantID: tenantID, UserID: &userID, Action: "login.success", ResourceType: "user", ResourceID: strPtr(userID.String())},
		{TenantID: tenantID, UserID: &userID, Action: "logout", ResourceType: "session"},
	}

	for _, e := range entries {
		e := e
		require.NoError(t, tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
			return audit.Record(tx, e)
		}))
	}

	// Verify chain is intact.
	var ok bool
	var verifyErr error
	require.NoError(t, tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		ok, verifyErr = audit.VerifyChain(tx, tenantID)
		return verifyErr
	}))
	assert.True(t, ok, "chain must be valid after sequential inserts")

	// Verify hash chain linkage: prev_hash of row N equals hash of row N-1.
	var rows []struct {
		Seq      int64  `gorm:"column:seq"`
		PrevHash string `gorm:"column:prev_hash"`
		Hash     string `gorm:"column:hash"`
	}
	require.NoError(t, h.AdminDB.Raw(
		"SELECT seq, prev_hash, hash FROM audit_logs WHERE tenant_id = ? ORDER BY seq ASC",
		tenantID,
	).Scan(&rows).Error)
	require.Len(t, rows, 3)
	assert.Equal(t, "", rows[0].PrevHash, "first row prev_hash must be empty")
	assert.Equal(t, rows[0].Hash, rows[1].PrevHash, "row 2 prev_hash must equal row 1 hash")
	assert.Equal(t, rows[1].Hash, rows[2].PrevHash, "row 3 prev_hash must equal row 2 hash")
}

func TestAudit_TamperingDetected(t *testing.T) {
	h := testdb.NewHarness(t)
	ctx := context.Background()
	tdb := tenantdb.New(h.AppDB)

	tenantID := uuid.New()
	require.NoError(t, h.AdminDB.Exec(
		"INSERT INTO tenants (id, name, plan_code, status, slug) VALUES (?, 'Tamper Corp', 'free', 'active', ?)",
		tenantID, "tamper-corp-"+tenantID.String()[:8],
	).Error)

	userID := uuid.New()
	require.NoError(t, tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Exec(
			"INSERT INTO users (id, tenant_id, email, status) VALUES (?, ?, 'tamper@test.test', 'active')",
			userID, tenantID,
		).Error
	}))

	// Insert 3 audit rows.
	for i := 0; i < 3; i++ {
		require.NoError(t, tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
			return audit.Record(tx, audit.Entry{
				TenantID:     tenantID,
				UserID:       &userID,
				Action:       fmt.Sprintf("event.%d", i),
				ResourceType: "test",
			})
		}))
	}

	// Tamper: update the middle row's action (simulate data alteration).
	require.NoError(t, h.AdminDB.Exec(
		`UPDATE audit_logs SET action = 'TAMPERED'
		 WHERE tenant_id = ? AND seq = (
		   SELECT seq FROM audit_logs WHERE tenant_id = ? ORDER BY seq ASC LIMIT 1 OFFSET 1
		 )`,
		tenantID, tenantID,
	).Error)

	// VerifyChain must return false.
	var ok bool
	require.NoError(t, tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		var err error
		ok, err = audit.VerifyChain(tx, tenantID)
		return err
	}))
	assert.False(t, ok, "tampered chain must fail verification")
}

func TestAudit_SignupRecordsEvents(t *testing.T) {
	h := testdb.NewHarness(t)
	cfg := newTestConfig()
	handler := newTestServer(h, cfg)

	csrfTok, csrfCookies := csrfToken(t, handler)
	w := postJSON(t, handler, "/api/v1/auth/signup", map[string]string{
		"tenant_name": "Audit Signup Corp",
		"slug":        "audit-signup-corp",
		"email":       "admin@auditsignup.test",
		"password":    "signuppass123",
	}, csrfCookies, csrfTok)
	require.Equal(t, http.StatusCreated, w.Code)

	// Check that audit_logs has tenant.created and user.created events.
	var tenantIDStr string
	require.NoError(t, h.AdminDB.Raw("SELECT id::text FROM tenants WHERE slug = ?", "audit-signup-corp").Scan(&tenantIDStr).Error)
	tenantID, err := uuid.Parse(tenantIDStr)
	require.NoError(t, err)

	var actions []string
	require.NoError(t, h.AdminDB.Raw(
		"SELECT action FROM audit_logs WHERE tenant_id = ? ORDER BY seq ASC", tenantID.String(),
	).Scan(&actions).Error)

	assert.Contains(t, actions, "tenant.created")
	assert.Contains(t, actions, "user.created")

	// Verify the chain.
	tdb := tenantdb.New(h.AppDB)
	var ok bool
	require.NoError(t, tdb.WithinTenant(context.Background(), tenantID, func(tx *gorm.DB) error {
		var err error
		ok, err = audit.VerifyChain(tx, tenantID)
		return err
	}))
	assert.True(t, ok, "audit chain after signup must be valid")
}

func TestAudit_LoginRecordsEvents(t *testing.T) {
	h := testdb.NewHarness(t)
	cfg := newTestConfig()
	handler := newTestServer(h, cfg)

	// Signup.
	csrfTok, csrfCookies := csrfToken(t, handler)
	_ = postJSON(t, handler, "/api/v1/auth/signup", map[string]string{
		"tenant_name": "Audit Login Corp",
		"slug":        "audit-login-corp",
		"email":       "admin@auditlogin.test",
		"password":    "loginpass123",
	}, csrfCookies, csrfTok)

	// Login success.
	csrfTok2, csrfCookies2 := csrfToken(t, handler)
	_ = postJSON(t, handler, "/api/v1/auth/login", map[string]string{
		"slug":     "audit-login-corp",
		"email":    "admin@auditlogin.test",
		"password": "loginpass123",
	}, csrfCookies2, csrfTok2)

	// Login failure.
	csrfTok3, csrfCookies3 := csrfToken(t, handler)
	_ = postJSON(t, handler, "/api/v1/auth/login", map[string]string{
		"slug":     "audit-login-corp",
		"email":    "admin@auditlogin.test",
		"password": "wrongpass",
	}, csrfCookies3, csrfTok3)

	var tenantIDStr string
	require.NoError(t, h.AdminDB.Raw("SELECT id::text FROM tenants WHERE slug = ?", "audit-login-corp").Scan(&tenantIDStr).Error)
	tenantID, err := uuid.Parse(tenantIDStr)
	require.NoError(t, err)

	var actions []string
	require.NoError(t, h.AdminDB.Raw(
		"SELECT action FROM audit_logs WHERE tenant_id = ? ORDER BY seq ASC", tenantID.String(),
	).Scan(&actions).Error)

	assert.Contains(t, actions, "login.success")
	assert.Contains(t, actions, "login.failure")
}

// ---------------------------------------------------------------------------
// Rate limit test
// ---------------------------------------------------------------------------

func TestRateLimit_LoginReturns429AfterLimit(t *testing.T) {
	h := testdb.NewHarness(t)
	cfg := newTestConfig()
	// Set very tight limit: 3 per minute.
	cfg.AuthRateLimit = "3-M"
	handler := newTestServer(h, cfg)

	csrfTok, csrfCookies := csrfToken(t, handler)
	_ = postJSON(t, handler, "/api/v1/auth/signup", map[string]string{
		"tenant_name": "Rate Corp",
		"slug":        "rate-corp",
		"email":       "admin@rate.test",
		"password":    "ratepassword123",
	}, csrfCookies, csrfTok)

	// Note: signup used 1 of our 3 limit, so 2 remain for login.
	// Actually rate limiting is per-IP. In tests the IP is typically "::1" or "".
	// We need to exceed the rate on login.
	// Create a new handler just for login testing with low limit.
	cfg2 := newTestConfig()
	cfg2.AuthRateLimit = "2-M"
	handler2 := newTestServer(h, cfg2)

	// First two login attempts (rate limit is 2/min).
	for i := 0; i < 2; i++ {
		csrfT, csrfC := csrfToken(t, handler2)
		_ = postJSON(t, handler2, "/api/v1/auth/login", map[string]string{
			"slug":     "rate-corp",
			"email":    "admin@rate.test",
			"password": "wrongpass",
		}, csrfC, csrfT)
	}

	// Third attempt should be rate limited (429).
	csrfT3, csrfC3 := csrfToken(t, handler2)
	w := postJSON(t, handler2, "/api/v1/auth/login", map[string]string{
		"slug":     "rate-corp",
		"email":    "admin@rate.test",
		"password": "wrongpass",
	}, csrfC3, csrfT3)
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "should be rate limited: %s", w.Body.String())
}

// ---------------------------------------------------------------------------
// Me endpoint test
// ---------------------------------------------------------------------------

func TestMe_ReturnsUserProfile(t *testing.T) {
	h := testdb.NewHarness(t)
	cfg := newTestConfig()
	handler := newTestServer(h, cfg)

	sessionCookie := signupAndLogin(t, handler, cfg, "me-corp", "admin@me.test", "mepassword123")

	w := getReq(t, handler, "/api/v1/auth/me", []*http.Cookie{sessionCookie})
	assert.Equal(t, http.StatusOK, w.Code, "me: %s", w.Body.String())

	var resp map[string]interface{}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, "admin@me.test", resp["email"])
	assert.NotNil(t, resp["user_id"])
	assert.NotNil(t, resp["tenant_id"])
	// Admin should have permissions.
	perms, ok := resp["permissions"].([]interface{})
	assert.True(t, ok)
	assert.Contains(t, perms, "*", "admin user must have wildcard permission")

	// Sensitive fields must NOT be present.
	assert.Nil(t, resp["password_hash"], "password_hash must not be returned")
}

// ---------------------------------------------------------------------------
// Lock-release counter reset (C-2/I-2)
// ---------------------------------------------------------------------------

// TestLogin_LockReleaseResetsCounter verifies that after a lockout window has
// expired, the failed_login_count is reset to 1 (not FailedLoginCount+1) on
// the first wrong-password attempt after unlock.  This prevents immediate
// re-locking after a single post-unlock failure.
func TestLogin_LockReleaseResetsCounter(t *testing.T) {
	h := testdb.NewHarness(t)
	cfg := newTestConfig()
	handler := newTestServer(h, cfg)

	// Signup a new tenant.
	csrfTok, csrfCookies := csrfToken(t, handler)
	w := postJSON(t, handler, "/api/v1/auth/signup", map[string]string{
		"tenant_name": "Unlock Corp",
		"slug":        "unlock-corp",
		"email":       "admin@unlock.test",
		"password":    "correctpassword123",
	}, csrfCookies, csrfTok)
	require.Equal(t, http.StatusCreated, w.Code)

	// Drive the account to lockout: 5 wrong-password attempts.
	for i := 0; i < 5; i++ {
		ct, cc := csrfToken(t, handler)
		postJSON(t, handler, "/api/v1/auth/login", map[string]string{
			"slug":     "unlock-corp",
			"email":    "admin@unlock.test",
			"password": "wrongpassword",
		}, cc, ct)
	}

	// Confirm lock is set.
	var lockedUntil *time.Time
	var failCount int
	require.NoError(t, h.AdminDB.Raw(
		"SELECT failed_login_count, locked_until FROM users WHERE email = ?", "admin@unlock.test",
	).Row().Scan(&failCount, &lockedUntil))
	require.NotNil(t, lockedUntil)
	assert.Equal(t, 5, failCount)

	// Simulate lock expiry by moving locked_until into the past.
	require.NoError(t, h.AdminDB.Exec(
		"UPDATE users SET locked_until = now() - interval '1 second' WHERE email = ?",
		"admin@unlock.test",
	).Error)

	// First wrong-password attempt after unlock.
	ct2, cc2 := csrfToken(t, handler)
	w2 := postJSON(t, handler, "/api/v1/auth/login", map[string]string{
		"slug":     "unlock-corp",
		"email":    "admin@unlock.test",
		"password": "wrongpassword",
	}, cc2, ct2)
	assert.Equal(t, http.StatusUnauthorized, w2.Code)

	// Counter must be reset to 1 — not 6 — so the account is not immediately
	// re-locked.
	var newCount int
	var newLockedUntil *time.Time
	require.NoError(t, h.AdminDB.Raw(
		"SELECT failed_login_count, locked_until FROM users WHERE email = ?", "admin@unlock.test",
	).Row().Scan(&newCount, &newLockedUntil))
	assert.Equal(t, 1, newCount, "counter must be reset to 1 after first post-unlock failure")
	assert.Nil(t, newLockedUntil, "account must not be re-locked after a single post-unlock failure")
}

// ---------------------------------------------------------------------------
// Audit seq monotonicity (I-4)
// ---------------------------------------------------------------------------

// TestAudit_SeqMonotonicityDetected verifies that VerifyChain returns false
// when audit log rows are re-ordered (non-monotonic seq), detecting deletion
// or insertion of rows outside of the normal chain order.
func TestAudit_SeqMonotonicityDetected(t *testing.T) {
	h := testdb.NewHarness(t)
	ctx := context.Background()
	tdb := tenantdb.New(h.AppDB)

	tenantID := uuid.New()
	require.NoError(t, h.AdminDB.Exec(
		"INSERT INTO tenants (id, name, plan_code, status, slug) VALUES (?, 'Seq Corp', 'free', 'active', ?)",
		tenantID, "seq-corp-"+tenantID.String()[:8],
	).Error)

	userID := uuid.New()
	require.NoError(t, tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Exec(
			"INSERT INTO users (id, tenant_id, email, status) VALUES (?, ?, 'seq@test.test', 'active')",
			userID, tenantID,
		).Error
	}))

	// Insert 3 audit rows.
	for i := 0; i < 3; i++ {
		i := i
		require.NoError(t, tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
			return audit.Record(tx, audit.Entry{
				TenantID:     tenantID,
				UserID:       &userID,
				Action:       fmt.Sprintf("event.%d", i),
				ResourceType: "test",
			})
		}))
	}

	// Verify chain is valid before tampering.
	require.NoError(t, tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		ok, err := audit.VerifyChain(tx, tenantID)
		if err != nil {
			return err
		}
		require.True(t, ok, "chain must be valid before seq tampering")
		return nil
	}))

	// Tamper: swap the seq values of rows 1 and 2 to break monotonicity.
	// We do this via two updates through AdminDB (bypasses RLS).
	var seqs []int64
	require.NoError(t, h.AdminDB.Raw(
		"SELECT seq FROM audit_logs WHERE tenant_id = ? ORDER BY seq ASC", tenantID.String(),
	).Scan(&seqs).Error)
	require.Len(t, seqs, 3)

	// Set seq of row[0] to a very large value to break strict monotonicity.
	require.NoError(t, h.AdminDB.Exec(
		`UPDATE audit_logs SET seq = 9999 WHERE tenant_id = ? AND seq = ?`,
		tenantID.String(), seqs[0],
	).Error)

	// VerifyChain must now detect non-monotonic seq.
	var ok bool
	require.NoError(t, tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		var err error
		ok, err = audit.VerifyChain(tx, tenantID)
		return err
	}))
	assert.False(t, ok, "non-monotonic seq must fail verification")
}

// ---------------------------------------------------------------------------
// Helper
// ---------------------------------------------------------------------------

func strPtr(s string) *string { return &s }

func TestSignupValidation_InvalidSlug(t *testing.T) {
	h := testdb.NewHarness(t)
	cfg := newTestConfig()
	handler := newTestServer(h, cfg)

	csrfTok, csrfCookies := csrfToken(t, handler)
	cases := []string{
		"AB",          // uppercase
		"a",           // too short
		"-start",      // starts with hyphen
		"end-",        // ends with hyphen
		"has space",   // space
		strings.Repeat("a", 64), // too long
	}
	for _, slug := range cases {
		csrfTok, csrfCookies = csrfToken(t, handler)
		w := postJSON(t, handler, "/api/v1/auth/signup", map[string]string{
			"tenant_name": "X",
			"slug":        slug,
			"email":       "a@b.com",
			"password":    "password123",
		}, csrfCookies, csrfTok)
		assert.Equal(t, http.StatusBadRequest, w.Code, "slug %q should fail validation", slug)
	}
}
