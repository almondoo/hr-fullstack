package auth

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/httpx"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Context keys for values stored in gin.Context.
// Using typed constants prevents collisions with other packages' keys.
const (
	ctxKeyTenantID contextKey = "auth_tenant_id"
	ctxKeyUserID   contextKey = "auth_user_id"
)

// contextKey is the unexported type for auth context values.
// It is identical to httpx.contextKey; redeclared here because contextKey
// is unexported in httpx.  Auth uses its own namespace to avoid collisions.
type contextKey string

// RequireAuth is a Gin middleware that authenticates every request via the
// session cookie.
//
// Flow:
//  1. Read the session cookie name from cookieName.
//  2. If absent → 401 UNAUTHENTICATED.
//  3. Call SessionStore.Resolve(rawToken) via the SECURITY DEFINER path.
//  4. If session not found / expired / revoked → 401 with a stable error code.
//  5. On success: store tenant_id and user_id in gin.Context.
//
// The handler must subsequently call WithinTenant(TenantIDFrom(c), ...) for
// any tenant-scoped database access.
func RequireAuth(
	store *SessionStore,
	appDB *gorm.DB,
	tdb *tenantdb.TenantDB,
	cookieName string,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		rawToken, err := c.Cookie(cookieName)
		if err != nil || rawToken == "" {
			httpx.RespondError(c, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
			c.Abort()
			return
		}

		tenantID, userID, resolveErr := store.Resolve(c.Request.Context(), appDB, tdb, rawToken)
		if resolveErr != nil {
			code, msg := sessionErrToHTTP(resolveErr)
			httpx.RespondError(c, http.StatusUnauthorized, code, msg)
			c.Abort()
			return
		}

		c.Set(string(ctxKeyTenantID), tenantID)
		c.Set(string(ctxKeyUserID), userID)
		c.Next()
	}
}

// sessionErrToHTTP converts a session resolution error to a stable HTTP error
// code and human-readable message.  The code is stable across releases so
// clients can key on it; the message is informational only.
func sessionErrToHTTP(err error) (code, message string) {
	switch {
	case err == ErrSessionNotFound:
		return "UNAUTHENTICATED", "session not found"
	case err == ErrSessionExpired:
		return "SESSION_EXPIRED", "session has expired"
	case err == ErrSessionRevoked:
		return "SESSION_REVOKED", "session has been revoked"
	default:
		return "UNAUTHENTICATED", "authentication required"
	}
}

// TenantIDFrom retrieves the tenant UUID stored by RequireAuth.
// Returns uuid.Nil when the middleware was not applied or did not succeed.
func TenantIDFrom(c *gin.Context) uuid.UUID {
	if v, ok := c.Get(string(ctxKeyTenantID)); ok {
		if id, ok := v.(uuid.UUID); ok {
			return id
		}
	}
	return uuid.Nil
}

// UserIDFrom retrieves the user UUID stored by RequireAuth.
// Returns uuid.Nil when the middleware was not applied or did not succeed.
func UserIDFrom(c *gin.Context) uuid.UUID {
	if v, ok := c.Get(string(ctxKeyUserID)); ok {
		if id, ok := v.(uuid.UUID); ok {
			return id
		}
	}
	return uuid.Nil
}
