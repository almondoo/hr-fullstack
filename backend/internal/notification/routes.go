package notification

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires the notification platform endpoints.
// The group should already have RequireAuth applied per-route below.
//
// Permission namespaces:
//   - notification:read           — read inbox / templates / deliveries
//   - notification:write          — manage templates / preferences
//   - notification:read_sensitive — decrypt destination email (sensitive)
//
// Endpoints:
//
//	PUT    /notifications/templates                              — notification:write
//	GET    /notifications/templates                              — notification:read
//	PUT    /notifications/preferences                            — notification:write
//	GET    /notifications                                        — notification:read (own inbox)
//	GET    /notifications/unread-count                           — notification:read (own)
//	POST   /notifications/:id/read                               — notification:read (own; item-level)
//	GET    /notifications/:id/deliveries                         — notification:read
//	POST   /notifications/deliveries/:delivery_id/process        — notification:write
//	GET    /notifications/deliveries/:delivery_id/email          — notification:read_sensitive
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) {
	svc := NewService(tdb)
	h := NewHandler(svc)

	read := platformauth.RequirePermission(tdb, "notification:read")
	write := platformauth.RequirePermission(tdb, "notification:write")
	readSensitive := platformauth.RequirePermission(tdb, "notification:read_sensitive")

	grp := rg.Group("/notifications")
	grp.Use(requireAuth)

	// Templates.
	grp.PUT("/templates", write, h.UpsertTemplate)
	grp.GET("/templates", read, h.ListTemplates)

	// Preferences.
	grp.PUT("/preferences", write, h.SetPreference)

	// Email delivery lifecycle (specific paths registered before /:id wildcards).
	grp.POST("/deliveries/:delivery_id/process", write, h.ProcessDelivery)
	grp.GET("/deliveries/:delivery_id/email", readSensitive, h.GetDeliveryEmail)

	// In-app inbox (own).
	grp.GET("", read, h.ListInbox)
	grp.GET("/unread-count", read, h.UnreadCount)
	grp.POST("/:id/read", read, h.MarkRead)
	grp.GET("/:id/deliveries", read, h.ListDeliveries)
}
