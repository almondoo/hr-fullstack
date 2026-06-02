// Package auth registers HTTP handlers for authentication endpoints.
// All handlers delegate to Service for business logic.
package auth

import (
	"github.com/gin-gonic/gin"
	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires the auth endpoints into the given RouterGroup.
//
// Protected endpoints (RequireAuth):
//   - POST /logout
//   - GET  /me
//
// Public endpoints (rate-limited at server layer):
//   - POST /signup
//   - POST /login
//
// CSRF token endpoint (safe method, no state mutation):
//   - GET  /csrf
func RegisterRoutes(
	rg *gin.RouterGroup,
	svc *Service,
	store *platformauth.SessionStore,
	appDB interface {
		WithContext(ctx interface{}) interface{}
	}, // unused here, kept for doc
	tdb *tenantdb.TenantDB,
	cookieName string,
	requireAuth gin.HandlerFunc,
) {
	// Public auth routes.
	rg.POST("/signup", svc.Signup)
	rg.POST("/login", svc.Login)

	// Protected auth routes.
	protected := rg.Group("")
	protected.Use(requireAuth)
	protected.POST("/logout", svc.Logout)
	protected.GET("/me", svc.Me)
}
