package approval

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires the approval-workflow endpoints into the given RouterGroup.
// The group should already have RequireAuth applied by the caller.
//
// Endpoints:
//
//	POST   /approval/routes                          — approval:admin
//	GET    /approval/routes                          — approval:read
//	POST   /approval/requests                        — approval:write
//	GET    /approval/requests/mine                   — approval:read
//	GET    /approval/requests/pending                — approval:read
//	GET    /approval/requests/:id                    — approval:read
//	GET    /approval/requests/:id/steps              — approval:read
//	POST   /approval/requests/:id/decide             — approval:write
//	POST   /approval/requests/:id/cancel             — approval:write
//	PUT    /approval/requests/:id/delegate           — approval:write
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) {
	svc := NewService(tdb)
	h := NewHandler(svc)

	approvalRead := platformauth.RequirePermission(tdb, "approval:read")
	approvalWrite := platformauth.RequirePermission(tdb, "approval:write")
	approvalAdmin := platformauth.RequirePermission(tdb, "approval:admin")

	// Route administration (admin only).
	routes := rg.Group("/approval/routes")
	routes.Use(requireAuth)
	{
		routes.POST("", approvalAdmin, h.CreateRoute)
		routes.GET("", approvalRead, h.ListRoutes)
	}

	// Request lifecycle.
	reqs := rg.Group("/approval/requests")
	reqs.Use(requireAuth)
	{
		// Named sub-paths before the :id wildcard to avoid routing ambiguity.
		reqs.GET("/mine", approvalRead, h.ListMyRequests)
		reqs.GET("/pending", approvalRead, h.ListPendingForMe)

		reqs.POST("", approvalWrite, h.Submit)
		reqs.GET("/:id", approvalRead, h.GetRequest)
		reqs.GET("/:id/steps", approvalRead, h.ListSteps)
		reqs.POST("/:id/decide", approvalWrite, h.Decide)
		reqs.POST("/:id/cancel", approvalWrite, h.Cancel)
		reqs.PUT("/:id/delegate", approvalWrite, h.SetDelegate)
	}
}
