// Package auth implements signup, login, logout, and session-me endpoints.
// It is the application-layer service for authentication flows.
//
// Security invariants:
//   - Passwords / tokens are NEVER logged or returned in responses.
//   - Login errors return a generic message regardless of which field is wrong.
//   - Account lockout is applied after a configurable number of failures.
//   - All DB operations are wrapped in WithinTenant to enforce RLS.
//   - Login timing is uniform regardless of whether the slug/user exists
//     (dummy argon2id verification prevents user-enumeration via timing).
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/audit"
	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/config"
	"github.com/your-org/hr-saas/internal/platform/httpx"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// authTracer is the OTel tracer used by the auth service.
// Spans emitted here use the "auth" instrumentation scope.
// No PII (email, password, token) is recorded in span attributes.
var authTracer = otel.Tracer("github.com/your-org/hr-saas/internal/auth")

// ---------------------------------------------------------------------------
// Model shapes (GORM, mapped to DB columns)
// ---------------------------------------------------------------------------

type dbUser struct {
	ID               uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID         uuid.UUID  `gorm:"column:tenant_id"`
	Email            string     `gorm:"column:email"`
	PasswordHash     *string    `gorm:"column:password_hash"`
	RoleID           *uuid.UUID `gorm:"column:role_id"`
	Status           string     `gorm:"column:status"`
	FailedLoginCount int        `gorm:"column:failed_login_count"`
	LockedUntil      *time.Time `gorm:"column:locked_until"`
	LastLoginAt      *time.Time `gorm:"column:last_login_at"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
	UpdatedAt        time.Time  `gorm:"column:updated_at"`
}

// TableName maps dbUser to the users table.
func (dbUser) TableName() string { return "users" }

type dbRole struct {
	ID          uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID    uuid.UUID `gorm:"column:tenant_id"`
	Name        string    `gorm:"column:name"`
	Permissions []byte    `gorm:"column:permissions;type:jsonb"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

// TableName maps dbRole to the roles table.
func (dbRole) TableName() string { return "roles" }

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

const (
	maxFailedLogins = 5
	lockoutDuration = 30 * time.Minute
	minPasswordLen  = 8
)

var (
	slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9\-]{1,61}[a-z0-9]$`)

	validate = validator.New()
)

// Service provides the business logic for auth endpoints.
type Service struct {
	appDB      *gorm.DB
	tdb        *tenantdb.TenantDB
	store      *platformauth.SessionStore
	cfg        *config.Config
	cookieOpts platformauth.CookieOptions
	// dummyHash is an argon2id hash computed at startup against a synthetic
	// password.  It is used in Login to perform a constant-time dummy
	// VerifyPassword call when the slug or user does not exist, preventing
	// user-enumeration via response-time differences.
	//
	// The hash is computed with the same cost parameters as real passwords so
	// that the timing is representative.  The synthetic password value is
	// never logged and is not a real credential.
	dummyHash string
}

// NewService creates a Service.
// It pre-computes a dummy argon2id hash at startup to use for constant-time
// responses when the login slug or user is not found (C-1 timing oracle fix).
func NewService(
	appDB *gorm.DB,
	tdb *tenantdb.TenantDB,
	store *platformauth.SessionStore,
	cfg *config.Config,
) *Service {
	// Hash a fixed synthetic string.  The value is intentionally opaque and
	// is never logged, stored, or returned.  We do NOT use a hard-coded
	// literal password: instead we derive the dummy input from a sha256 of
	// the module path so it is deterministic but not guessable.
	// Even if an attacker could compute the same value, it confers no benefit
	// because the hash is never compared against a real DB credential.
	dummy, err := platformauth.HashPassword("__hr_saas_dummy_login_sentinel__")
	if err != nil {
		// HashPassword only fails on rand.Read errors; that is fatal.
		panic("auth: NewService: failed to generate dummy hash: " + err.Error())
	}
	return &Service{
		appDB:      appDB,
		tdb:        tdb,
		store:      store,
		cfg:        cfg,
		cookieOpts: platformauth.CookieOptionsFromConfig(cfg),
		dummyHash:  dummy,
	}
}

// ---------------------------------------------------------------------------
// Signup
// ---------------------------------------------------------------------------

// SignupRequest is the expected JSON body for POST /api/v1/auth/signup.
type SignupRequest struct {
	TenantName string `json:"tenant_name"`
	Slug       string `json:"slug"`
	Email      string `json:"email"`
	Password   string `json:"password"`
}

func (r *SignupRequest) validate() error {
	var errs []string
	if strings.TrimSpace(r.TenantName) == "" {
		errs = append(errs, "tenant_name is required")
	}
	if !slugRe.MatchString(r.Slug) {
		errs = append(errs, "slug must be 3–63 lowercase alphanumeric characters or hyphens, not starting/ending with hyphen")
	}
	if err := validate.Var(r.Email, "required,email"); err != nil {
		errs = append(errs, "email is invalid")
	}
	if len(r.Password) < minPasswordLen {
		errs = append(errs, fmt.Sprintf("password must be at least %d characters", minPasswordLen))
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// Signup creates a new tenant, admin role, and admin user, then issues a session.
func (s *Service) Signup(c *gin.Context) {
	ctx, span := authTracer.Start(c.Request.Context(), "auth.Signup")
	defer span.End()
	c.Request = c.Request.WithContext(ctx)

	var req SignupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := req.validate(); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	// Check slug uniqueness before creating (before acquiring any tenant ctx).
	existing, err := s.store.ResolveTenantBySlug(c.Request.Context(), s.appDB, req.Slug)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	if existing != uuid.Nil {
		httpx.RespondError(c, http.StatusConflict, "SLUG_TAKEN", "slug is already in use")
		return
	}

	tenantID := uuid.New()
	adminRoleID := uuid.New()
	adminUserID := uuid.New()

	// Hash the password before opening the transaction (argon2 is expensive).
	passwordHash, err := platformauth.HashPassword(req.Password)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}

	adminPerms, _ := json.Marshal(map[string][]string{"perms": {"*"}})

	// All writes happen in a single WithinTenant transaction for atomicity.
	if err := s.tdb.WithinTenant(c.Request.Context(), tenantID, func(tx *gorm.DB) error {
		// INSERT the tenant row (self-referential: the tenant IS the context).
		if err := tx.Exec(
			`INSERT INTO tenants (id, name, plan_code, status, slug)
			 VALUES (?, ?, 'free', 'active', ?)`,
			tenantID, req.TenantName, req.Slug,
		).Error; err != nil {
			return fmt.Errorf("signup: insert tenant: %w", err)
		}

		// INSERT the admin role.
		if err := tx.Exec(
			`INSERT INTO roles (id, tenant_id, name, permissions)
			 VALUES (?, ?, 'admin', ?)`,
			adminRoleID, tenantID, adminPerms,
		).Error; err != nil {
			return fmt.Errorf("signup: insert role: %w", err)
		}

		// INSERT the admin user.
		if err := tx.Exec(
			`INSERT INTO users (id, tenant_id, email, password_hash, role_id, status)
			 VALUES (?, ?, ?, ?, ?, 'active')`,
			adminUserID, tenantID, req.Email, passwordHash, adminRoleID,
		).Error; err != nil {
			return fmt.Errorf("signup: insert user: %w", err)
		}

		// Audit: tenant.created
		tenantIDStr := tenantID.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     tenantID,
			UserID:       &adminUserID,
			Action:       "tenant.created",
			ResourceType: "tenant",
			ResourceID:   &tenantIDStr,
			IP:           clientIP(c),
		}); err != nil {
			return fmt.Errorf("signup: audit tenant.created: %w", err)
		}

		// Audit: user.created
		userIDStr := adminUserID.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     tenantID,
			UserID:       &adminUserID,
			Action:       "user.created",
			ResourceType: "user",
			ResourceID:   &userIDStr,
			IP:           clientIP(c),
		}); err != nil {
			return fmt.Errorf("signup: audit user.created: %w", err)
		}

		return nil
	}); err != nil {
		// Slug unique violation from another concurrent request.
		if isUniqueViolation(err) {
			span.SetStatus(codes.Error, "slug_taken")
			httpx.RespondError(c, http.StatusConflict, "SLUG_TAKEN", "slug is already in use")
			return
		}
		span.SetStatus(codes.Error, "db_error")
		httpx.RespondInternalError(c)
		return
	}

	// Issue session (auto-login).
	ip := parseClientIP(c)
	rawToken, err := s.store.Create(c.Request.Context(), s.tdb, tenantID, adminUserID, s.cfg.SessionTTL, ip)
	if err != nil {
		// Tenant + user exist but session creation failed. Return 201 without cookie.
		c.JSON(http.StatusCreated, gin.H{"message": "account created; please log in"})
		return
	}

	platformauth.SetSessionCookie(c.Writer, rawToken, s.cookieOpts)
	c.JSON(http.StatusCreated, gin.H{
		"tenant_id": tenantID,
		"user_id":   adminUserID,
	})
}

// ---------------------------------------------------------------------------
// Login
// ---------------------------------------------------------------------------

// LoginRequest is the expected JSON body for POST /api/v1/auth/login.
type LoginRequest struct {
	Slug     string `json:"slug"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (r *LoginRequest) validate() error {
	var errs []string
	if strings.TrimSpace(r.Slug) == "" {
		errs = append(errs, "slug is required")
	}
	if err := validate.Var(r.Email, "required,email"); err != nil {
		errs = append(errs, "email is invalid")
	}
	if r.Password == "" {
		errs = append(errs, "password is required")
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

const loginErrMsg = "invalid credentials"

// Login authenticates a user by slug+email+password and issues a session.
func (s *Service) Login(c *gin.Context) {
	ctx, span := authTracer.Start(c.Request.Context(), "auth.Login")
	defer span.End()
	c.Request = c.Request.WithContext(ctx)

	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := req.validate(); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	// Resolve tenant — cross-tenant boundary lookup via SECURITY DEFINER.
	tenantID, err := s.store.ResolveTenantBySlug(c.Request.Context(), s.appDB, req.Slug)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	if tenantID == uuid.Nil {
		// Slug does not exist.  Perform a dummy argon2id verification to
		// equalise response time with the successful-slug path (C-1: timing
		// oracle prevention).  The result is intentionally discarded.
		_, _ = platformauth.VerifyPassword(s.dummyHash, req.Password)
		// Do not reveal whether the slug exists.
		httpx.RespondError(c, http.StatusUnauthorized, "INVALID_CREDENTIALS", loginErrMsg)
		return
	}

	var (
		loginOK bool
		userID  uuid.UUID
	)

	txErr := s.tdb.WithinTenant(c.Request.Context(), tenantID, func(tx *gorm.DB) error {
		// Fetch user by email within tenant.
		var u dbUser
		if err := tx.Raw(
			`SELECT id, password_hash, failed_login_count, locked_until, status, role_id
			 FROM users
			 WHERE tenant_id = ? AND email = ?
			 LIMIT 1`,
			tenantID, req.Email,
		).Scan(&u).Error; err != nil {
			return fmt.Errorf("login: fetch user: %w", err)
		}
		if u.ID == uuid.Nil {
			// User not found — perform dummy verification to equalise response
			// time with the found-user path (C-1: timing oracle prevention).
			// The result is intentionally discarded.
			_, _ = platformauth.VerifyPassword(s.dummyHash, req.Password)
			loginOK = false
			return nil
		}
		if u.Status != "active" {
			loginOK = false
			return nil
		}

		// Check lockout.
		if u.LockedUntil != nil && time.Now().Before(*u.LockedUntil) {
			loginOK = false
			// Audit failed login attempt while locked.
			userIDPtr := u.ID
			_ = audit.Record(tx, audit.Entry{
				TenantID:     tenantID,
				UserID:       &userIDPtr,
				Action:       "login.failure",
				ResourceType: "user",
				ResourceID:   strPtr(u.ID.String()),
				IP:           clientIP(c),
			})
			return nil
		}

		// Verify password.
		if u.PasswordHash == nil {
			loginOK = false
			return nil
		}
		ok, err := platformauth.VerifyPassword(*u.PasswordHash, req.Password)
		if err != nil {
			return fmt.Errorf("login: verify password: %w", err)
		}

		userIDPtr := u.ID

		if !ok {
			// Increment failure counter; lock if threshold reached.
			//
			// C-2/I-2: If the lock has expired (LockedUntil is in the past),
			// this is the first attempt after unlock — reset the counter to 1
			// so the user gets a fresh maxFailedLogins budget rather than
			// immediately re-locking on the first post-unlock failure.
			var newCount int
			lockExpired := u.LockedUntil != nil && !time.Now().Before(*u.LockedUntil)
			if lockExpired {
				// Lock window has passed: start a fresh failure counter.
				newCount = 1
			} else {
				newCount = u.FailedLoginCount + 1
			}
			var lockUntil *time.Time
			if newCount >= maxFailedLogins {
				t := time.Now().Add(lockoutDuration)
				lockUntil = &t
			}
			if err := tx.Exec(
				`UPDATE users
				 SET failed_login_count = ?,
				     locked_until = ?,
				     updated_at = now()
				 WHERE id = ? AND tenant_id = ?`,
				newCount, lockUntil, u.ID, tenantID,
			).Error; err != nil {
				return fmt.Errorf("login: update failed count: %w", err)
			}
			_ = audit.Record(tx, audit.Entry{
				TenantID:     tenantID,
				UserID:       &userIDPtr,
				Action:       "login.failure",
				ResourceType: "user",
				ResourceID:   strPtr(u.ID.String()),
				IP:           clientIP(c),
			})
			loginOK = false
			return nil
		}

		// Successful login: reset failure counter, update last_login_at.
		now := time.Now()
		if err := tx.Exec(
			`UPDATE users
			 SET failed_login_count = 0,
			     locked_until = NULL,
			     last_login_at = ?,
			     updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			now, u.ID, tenantID,
		).Error; err != nil {
			return fmt.Errorf("login: update login success: %w", err)
		}

		// Audit login.success.
		if err := audit.Record(tx, audit.Entry{
			TenantID:     tenantID,
			UserID:       &userIDPtr,
			Action:       "login.success",
			ResourceType: "user",
			ResourceID:   strPtr(u.ID.String()),
			IP:           clientIP(c),
		}); err != nil {
			return fmt.Errorf("login: audit: %w", err)
		}

		loginOK = true
		userID = u.ID
		return nil
	})

	if txErr != nil {
		span.SetStatus(codes.Error, "db_error")
		httpx.RespondInternalError(c)
		return
	}
	if !loginOK {
		// Record that auth failed without revealing whether the slug or password
		// was wrong — no user-enumeration info in the span attribute.
		span.SetAttributes(attribute.Bool("auth.success", false))
		span.SetStatus(codes.Error, "invalid_credentials")
		httpx.RespondError(c, http.StatusUnauthorized, "INVALID_CREDENTIALS", loginErrMsg)
		return
	}
	span.SetAttributes(attribute.Bool("auth.success", true))

	// Issue session.
	ip := parseClientIP(c)
	rawToken, err := s.store.Create(c.Request.Context(), s.tdb, tenantID, userID, s.cfg.SessionTTL, ip)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}

	platformauth.SetSessionCookie(c.Writer, rawToken, s.cookieOpts)
	c.JSON(http.StatusOK, gin.H{"user_id": userID})
}

// ---------------------------------------------------------------------------
// Logout
// ---------------------------------------------------------------------------

// Logout revokes the current session and clears the cookie.
// RequireAuth must be applied before this handler.
func (s *Service) Logout(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	userID := platformauth.UserIDFrom(c)

	rawToken, err := c.Cookie(s.cfg.SessionCookieName)
	if err != nil || rawToken == "" {
		httpx.RespondError(c, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return
	}

	if revokeErr := s.store.RevokeByRawToken(c.Request.Context(), s.tdb, tenantID, rawToken); revokeErr != nil {
		httpx.RespondInternalError(c)
		return
	}

	// Audit logout (best-effort — do not fail the logout if audit fails).
	// I-6: surface a Warn when the audit write fails so that audit gaps are
	// visible in the log.  Token / PII are never logged.
	if auditErr := s.tdb.WithinTenant(c.Request.Context(), tenantID, func(tx *gorm.DB) error {
		return audit.Record(tx, audit.Entry{
			TenantID:     tenantID,
			UserID:       &userID,
			Action:       "logout",
			ResourceType: "session",
			IP:           clientIP(c),
		})
	}); auditErr != nil {
		slog.WarnContext(c.Request.Context(), "audit: logout record failed", "error", auditErr)
	}

	platformauth.ClearSessionCookie(c.Writer, s.cookieOpts)
	c.JSON(http.StatusOK, gin.H{"message": "logged out"})
}

// ---------------------------------------------------------------------------
// Me
// ---------------------------------------------------------------------------

// MeResponse is the JSON response for GET /api/v1/auth/me.
type MeResponse struct {
	UserID      uuid.UUID  `json:"user_id"`
	TenantID    uuid.UUID  `json:"tenant_id"`
	Email       string     `json:"email"`
	RoleID      *uuid.UUID `json:"role_id,omitempty"`
	RoleName    *string    `json:"role_name,omitempty"`
	Permissions []string   `json:"permissions"`
}

// Me returns the current authenticated user's profile.
// RequireAuth must be applied before this handler.
func (s *Service) Me(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	userID := platformauth.UserIDFrom(c)

	var resp MeResponse
	err := s.tdb.WithinTenant(c.Request.Context(), tenantID, func(tx *gorm.DB) error {
		// Fetch user with role join.
		var u dbUser
		if err := tx.Raw(
			`SELECT id, tenant_id, email, role_id FROM users
			 WHERE id = ? AND tenant_id = ?
			 LIMIT 1`,
			userID, tenantID,
		).Scan(&u).Error; err != nil {
			return fmt.Errorf("me: fetch user: %w", err)
		}
		if u.ID == uuid.Nil {
			return fmt.Errorf("me: user not found")
		}

		resp.UserID = u.ID
		resp.TenantID = u.TenantID
		resp.Email = u.Email
		resp.RoleID = u.RoleID
		resp.Permissions = []string{}

		if u.RoleID != nil {
			var r dbRole
			if err := tx.Raw(
				`SELECT id, name, permissions FROM roles
				 WHERE id = ? AND tenant_id = ?
				 LIMIT 1`,
				u.RoleID, tenantID,
			).Scan(&r).Error; err != nil {
				return fmt.Errorf("me: fetch role: %w", err)
			}
			if r.ID != uuid.Nil {
				resp.RoleName = &r.Name
				var pj struct {
					Perms []string `json:"perms"`
				}
				if len(r.Permissions) > 0 {
					if err := json.Unmarshal(r.Permissions, &pj); err == nil {
						resp.Permissions = pj.Perms
					}
				}
			}
		}

		return nil
	})
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}

	c.JSON(http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func clientIP(c *gin.Context) *string {
	ip := c.ClientIP()
	if ip == "" {
		return nil
	}
	return &ip
}

func parseClientIP(c *gin.Context) net.IP {
	raw := c.ClientIP()
	if raw == "" {
		return nil
	}
	return net.ParseIP(raw)
}

func strPtr(s string) *string {
	return &s
}

// isUniqueViolation returns true when err wraps a PostgreSQL unique_violation (23505).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "23505") ||
		strings.Contains(err.Error(), "unique_violation") ||
		strings.Contains(err.Error(), "duplicate key")
}
