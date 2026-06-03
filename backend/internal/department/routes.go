package department

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires the department endpoints into the given RouterGroup.
// The group should already have RequireAuth applied by the caller.
//
// Endpoints:
//
//	GET    /departments          — department:read
//	POST   /departments          — department:write
//	GET    /departments/:id      — department:read
//	PUT    /departments/:id      — department:write
//	DELETE /departments/:id      — department:write
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) {
	svc := NewService(tdb)
	h := NewHandler(svc)

	read := platformauth.RequirePermission(tdb, "department:read")
	write := platformauth.RequirePermission(tdb, "department:write")

	g := rg.Group("/departments")
	g.Use(requireAuth)
	g.GET("", read, h.List)
	g.POST("", write, h.Create)
	g.GET("/:id", read, h.Get)
	g.PUT("/:id", write, h.Update)
	g.DELETE("/:id", write, h.Delete)
}
