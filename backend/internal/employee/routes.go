package employee

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires the employee, assignment, and contract endpoints into
// the given RouterGroup.  The group should already have RequireAuth applied
// by the caller.
//
// Endpoints:
//
//	GET    /employees                            — employee:read
//	POST   /employees                            — employee:write
//	GET    /employees/:id                        — employee:read
//	PUT    /employees/:id                        — employee:write
//	DELETE /employees/:id                        — employee:write
//	POST   /employees/:id/assignments            — employee:write
//	GET    /employees/:id/assignments            — employee:read
//	POST   /employees/:id/contracts              — contract:write
//	GET    /employees/:id/contracts              — contract:read
//	GET    /contracts/:id                        — contract:read
//	PATCH  /contracts/:id/status                 — contract:write
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) {
	svc := NewService(tdb)
	h := NewHandler(svc)

	empRead := platformauth.RequirePermission(tdb, "employee:read")
	empWrite := platformauth.RequirePermission(tdb, "employee:write")
	ctrRead := platformauth.RequirePermission(tdb, "contract:read")
	ctrWrite := platformauth.RequirePermission(tdb, "contract:write")

	emps := rg.Group("/employees")
	emps.Use(requireAuth)

	emps.GET("", empRead, h.ListEmployees)
	emps.POST("", empWrite, h.CreateEmployee)
	emps.GET("/:id", empRead, h.GetEmployee)
	emps.PUT("/:id", empWrite, h.UpdateEmployee)
	emps.DELETE("/:id", empWrite, h.DeleteEmployee)

	// Assignments (nested under employee).
	emps.POST("/:id/assignments", empWrite, h.CreateAssignment)
	emps.GET("/:id/assignments", empRead, h.ListAssignments)

	// Contracts (nested under employee).
	emps.POST("/:id/contracts", ctrWrite, h.CreateContract)
	emps.GET("/:id/contracts", ctrRead, h.ListContracts)

	// Contract lifecycle endpoints (top-level, by contract ID).
	ctrs := rg.Group("/contracts")
	ctrs.Use(requireAuth)

	ctrs.GET("/:id", ctrRead, h.GetContract)
	ctrs.PATCH("/:id/status", ctrWrite, h.UpdateContractStatus)
}
