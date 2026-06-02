package leave

import (
	"github.com/gin-gonic/gin"

	"github.com/your-org/hr-saas/internal/approval"
	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires all leave endpoints into the given RouterGroup.
// The group should already have RequireAuth applied by the caller.
//
// Endpoints:
//
//	GET    /leave/settings                                   — leave:read
//	PUT    /leave/settings                                   — leave:admin
//	POST   /leave/grants                                     — leave:admin
//	POST   /leave/grants/compute-annual                      — leave:admin
//	GET    /leave/employees/:employee_id/grants              — leave:read
//	GET    /leave/employees/:employee_id/balance             — leave:read
//	GET    /leave/employees/:employee_id/five-day-obligation — leave:read
//	POST   /leave/requests                                   — leave:write
//	GET    /leave/requests/:id                               — leave:read
//	GET    /leave/employees/:employee_id/requests            — leave:read
//	PATCH  /leave/requests/:id/status                        — leave:admin
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, approvalSvc *approval.Service, requireAuth gin.HandlerFunc) {
	svc := NewService(tdb, approvalSvc)
	h := NewHandler(svc)

	leaveRead := platformauth.RequirePermission(tdb, "leave:read")
	leaveWrite := platformauth.RequirePermission(tdb, "leave:write")
	leaveAdmin := platformauth.RequirePermission(tdb, "leave:admin")

	leave := rg.Group("/leave")
	leave.Use(requireAuth)
	{
		// Settings
		leave.GET("/settings", leaveRead, h.GetSettings)
		leave.PUT("/settings", leaveAdmin, h.UpsertSettings)

		// Grant management
		leave.POST("/grants", leaveAdmin, h.CreateGrant)
		leave.POST("/grants/compute-annual", leaveAdmin, h.ComputeAndGrantAnnual)

		// Per-employee sub-resources
		leave.GET("/employees/:employee_id/grants", leaveRead, h.ListGrants)
		leave.GET("/employees/:employee_id/balance", leaveRead, h.GetBalance)
		leave.GET("/employees/:employee_id/five-day-obligation", leaveRead, h.GetFiveDayObligation)
		leave.GET("/employees/:employee_id/requests", leaveRead, h.ListRequests)

		// Requests
		leave.POST("/requests", leaveWrite, h.CreateRequest)
		leave.GET("/requests/:id", leaveRead, h.GetRequest)
		leave.PATCH("/requests/:id/status", leaveAdmin, h.UpdateRequestStatus)
	}
}
