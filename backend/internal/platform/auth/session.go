package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/config"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

const (
	// rawTokenBytes is the number of random bytes in a session token.
	// 32 bytes = 256 bits of entropy — sufficient against brute-force.
	rawTokenBytes = 32
)

// Errors returned by session operations.
var (
	// ErrSessionNotFound is returned when no session matches the token.
	ErrSessionNotFound = errors.New("auth: session not found")

	// ErrSessionExpired is returned when the session exists but has passed its
	// expiry time.
	ErrSessionExpired = errors.New("auth: session expired")

	// ErrSessionRevoked is returned when the session has been explicitly revoked.
	ErrSessionRevoked = errors.New("auth: session revoked")
)

// Session represents a row from the sessions table.
// Only the columns needed by middleware are modelled here; additional columns
// (ip, last_used_at) are written via targeted UPDATE queries.
type Session struct {
	ID          uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID    uuid.UUID  `gorm:"column:tenant_id"`
	UserID      uuid.UUID  `gorm:"column:user_id"`
	TokenHash   string     `gorm:"column:token_hash"`
	ExpiresAt   time.Time  `gorm:"column:expires_at"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	LastUsedAt  *time.Time `gorm:"column:last_used_at"`
	RevokedAt   *time.Time `gorm:"column:revoked_at"`
}

func (Session) TableName() string { return "sessions" }

// sessionRow is the minimal shape returned by auth_resolve_session.
type sessionRow struct {
	TenantID  uuid.UUID  `gorm:"column:tenant_id"`
	UserID    uuid.UUID  `gorm:"column:user_id"`
	ExpiresAt time.Time  `gorm:"column:expires_at"`
	RevokedAt *time.Time `gorm:"column:revoked_at"`
}

// SessionStore provides Create / Resolve / Revoke / ResolveTenantBySlug.
// It embeds no state of its own: all DB access goes through the TenantDB
// (for tenant-scoped paths) or the raw appDB (for SECURITY DEFINER lookups).
type SessionStore struct{}

// NewSessionStore returns a SessionStore.  The struct is stateless; the
// constructor exists for future extension (e.g., caching layer).
func NewSessionStore() *SessionStore {
	return &SessionStore{}
}

// tokenHash computes the hex-encoded SHA-256 hash of rawToken.
// The hash is what gets stored in the database; the raw token is the cookie value.
func tokenHash(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(sum[:])
}

// generateRawToken produces a cryptographically-random base64url token string.
func generateRawToken() (string, error) {
	b := make([]byte, rawTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generate token entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Create inserts a new session for (tenantID, userID) and returns the raw
// token (the value that goes in the Cookie).
//
// The raw token is NEVER persisted; only its SHA-256 hash is stored.
// Create runs inside WithinTenant so RLS enforces tenant isolation.
func (s *SessionStore) Create(
	ctx context.Context,
	tdb *tenantdb.TenantDB,
	tenantID uuid.UUID,
	userID uuid.UUID,
	ttl time.Duration,
	ip net.IP,
) (rawToken string, err error) {
	rawToken, err = generateRawToken()
	if err != nil {
		return "", err
	}

	hash := tokenHash(rawToken)
	expiresAt := time.Now().Add(ttl)

	var ipStr *string
	if ip != nil {
		s := ip.String()
		ipStr = &s
	}

	insertErr := tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		if ipStr != nil {
			return tx.Exec(
				`INSERT INTO sessions (tenant_id, user_id, token_hash, expires_at, ip)
				 VALUES (?, ?, ?, ?, ?::inet)`,
				tenantID, userID, hash, expiresAt, *ipStr,
			).Error
		}
		return tx.Exec(
			`INSERT INTO sessions (tenant_id, user_id, token_hash, expires_at)
			 VALUES (?, ?, ?, ?)`,
			tenantID, userID, hash, expiresAt,
		).Error
	})
	if insertErr != nil {
		// Zero the raw token so the caller cannot accidentally use a token for
		// a session that was not committed.
		return "", fmt.Errorf("auth: create session: %w", insertErr)
	}

	return rawToken, nil
}

// Resolve looks up a session by raw token using the SECURITY DEFINER function
// auth_resolve_session (which bypasses RLS — required because the tenant is
// not known at lookup time).
//
// On success it returns (tenantID, userID, sessionInfo).
// It then updates last_used_at inside WithinTenant (tenant context now known).
//
// gormAppDB must be the hr_app *gorm.DB (not wrapped in TenantDB); the SECURITY
// DEFINER call is intentionally executed outside any tenant transaction.
func (s *SessionStore) Resolve(
	ctx context.Context,
	gormAppDB *gorm.DB,
	tdb *tenantdb.TenantDB,
	rawToken string,
) (tenantID uuid.UUID, userID uuid.UUID, err error) {
	hash := tokenHash(rawToken)

	// Call the SECURITY DEFINER function; this runs without a tenant context.
	var row sessionRow
	result := gormAppDB.WithContext(ctx).Raw(
		`SELECT tenant_id, user_id, expires_at, revoked_at
		 FROM auth_resolve_session($1)`,
		hash,
	).Scan(&row)
	if result.Error != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("auth: resolve session query: %w", result.Error)
	}
	if result.RowsAffected == 0 || row.TenantID == uuid.Nil {
		return uuid.Nil, uuid.Nil, ErrSessionNotFound
	}

	if row.RevokedAt != nil {
		return uuid.Nil, uuid.Nil, ErrSessionRevoked
	}
	if time.Now().After(row.ExpiresAt) {
		return uuid.Nil, uuid.Nil, ErrSessionExpired
	}

	// Update last_used_at inside a tenant-scoped transaction.
	// This is non-critical: a failure must not invalidate an otherwise-valid
	// session, but it should be surfaced in logs for observability.
	// Token/PII is intentionally excluded from the log entry.
	now := time.Now()
	if err := tdb.WithinTenant(ctx, row.TenantID, func(tx *gorm.DB) error {
		return tx.Model(&Session{}).
			Where("token_hash = ? AND tenant_id = ?", hash, row.TenantID).
			Update("last_used_at", now).Error
	}); err != nil {
		slog.WarnContext(ctx, "session: last_used_at update failed", "error", err)
	}

	return row.TenantID, row.UserID, nil
}

// Revoke marks a session as revoked by token hash inside a tenant transaction.
// It is idempotent: revoking an already-revoked session is a no-op.
func (s *SessionStore) Revoke(
	ctx context.Context,
	tdb *tenantdb.TenantDB,
	tenantID uuid.UUID,
	hash string,
) error {
	return tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Model(&Session{}).
			Where("token_hash = ? AND tenant_id = ? AND revoked_at IS NULL", hash, tenantID).
			Update("revoked_at", time.Now()).Error
	})
}

// RevokeByRawToken is a convenience wrapper that accepts the raw token (e.g.
// from the logout Cookie) rather than a pre-computed hash.
func (s *SessionStore) RevokeByRawToken(
	ctx context.Context,
	tdb *tenantdb.TenantDB,
	tenantID uuid.UUID,
	rawToken string,
) error {
	return s.Revoke(ctx, tdb, tenantID, tokenHash(rawToken))
}

// ResolveTenantBySlug calls the SECURITY DEFINER function
// auth_resolve_tenant_by_slug to look up a tenant UUID from its slug.
//
// This is executed outside any tenant transaction — the slug lookup happens
// before the tenant is known.  gormAppDB must be the hr_app *gorm.DB.
//
// Returns (uuid.Nil, nil) when the slug is not found.
func (s *SessionStore) ResolveTenantBySlug(
	ctx context.Context,
	gormAppDB *gorm.DB,
	slug string,
) (uuid.UUID, error) {
	// Scan into *string first: the pgx driver returns the uuid column as a
	// string when selected via a scalar function, and cannot auto-convert it
	// to uuid.UUID directly through database/sql's Scan interface.
	var idStr *string
	result := gormAppDB.WithContext(ctx).Raw(
		`SELECT auth_resolve_tenant_by_slug($1)`,
		slug,
	).Scan(&idStr)
	if result.Error != nil {
		return uuid.Nil, fmt.Errorf("auth: resolve tenant by slug: %w", result.Error)
	}
	if idStr == nil || *idStr == "" {
		return uuid.Nil, nil
	}
	id, err := uuid.Parse(*idStr)
	if err != nil {
		return uuid.Nil, fmt.Errorf("auth: resolve tenant by slug: parse uuid %q: %w", *idStr, err)
	}
	return id, nil
}

// ---------------------------------------------------------------------------
// Cookie helpers
// ---------------------------------------------------------------------------

// CookieOptions controls Cookie attributes.  Values are drawn from Config.
type CookieOptions struct {
	Name     string
	TTL      time.Duration
	Secure   bool
	SameSite http.SameSite
	Path     string
}

// CookieOptionsFromConfig builds CookieOptions from application config.
func CookieOptionsFromConfig(cfg *config.Config) CookieOptions {
	ss := http.SameSiteLaxMode
	switch cfg.SessionCookieSameSite {
	case "strict":
		ss = http.SameSiteStrictMode
	case "none":
		ss = http.SameSiteNoneMode
	}
	return CookieOptions{
		Name:     cfg.SessionCookieName,
		TTL:      cfg.SessionTTL,
		Secure:   cfg.SessionCookieSecure,
		SameSite: ss,
		Path:     "/",
	}
}

// SetSessionCookie writes the session cookie containing rawToken to the
// response.  The cookie is HttpOnly so JavaScript cannot access it.
func SetSessionCookie(w http.ResponseWriter, rawToken string, opts CookieOptions) {
	http.SetCookie(w, &http.Cookie{
		Name:     opts.Name,
		Value:    rawToken,
		Path:     opts.Path,
		HttpOnly: true,
		Secure:   opts.Secure,
		SameSite: opts.SameSite,
		MaxAge:   int(opts.TTL.Seconds()),
	})
}

// ClearSessionCookie removes the session cookie by setting MaxAge to -1.
func ClearSessionCookie(w http.ResponseWriter, opts CookieOptions) {
	http.SetCookie(w, &http.Cookie{
		Name:     opts.Name,
		Value:    "",
		Path:     opts.Path,
		HttpOnly: true,
		Secure:   opts.Secure,
		SameSite: opts.SameSite,
		MaxAge:   -1,
	})
}
