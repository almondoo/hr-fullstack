package yearend

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires the year-end adjustment (年末調整) endpoints.
// The signature is uniform across all stories (central wiring depends on it).
//
// Payroll-SaaS adapter: wired as a stub (StubPayrollPusher, ProviderMock).
// Real provider implementations require external credentials (P3).
//
// Endpoints:
//
//	GET    /yearend/settings                                       — yearend:read
//	PUT    /yearend/settings                                       — yearend:write
//	POST   /employees/:id/yearend/submissions                      — yearend:write
//	POST   /employees/:id/yearend/submissions/submit               — yearend:write
//	GET    /employees/:id/yearend/submissions                      — yearend:read  (reveal=true → yearend:reveal)
//	POST   /employees/:id/yearend/calculations                     — yearend:write
//	POST   /employees/:id/yearend/calculations/finalise            — yearend:write
//	GET    /employees/:id/yearend/calculations                     — yearend:read
//	POST   /employees/:id/yearend/reports/withholding-slip         — yearend:write
//	POST   /employees/:id/yearend/payroll-push                     — yearend:write
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) {
	// Payroll-SaaS stub adapter (real integration is P3).
	pusher := NewStubPayrollPusher(ProviderMock)
	svc := NewService(tdb, pusher)
	h := NewHandler(svc)

	read := platformauth.RequirePermission(tdb, "yearend:read")
	write := platformauth.RequirePermission(tdb, "yearend:write")
	// yearend:reveal is enforced in the service layer (defence-in-depth);
	// the route-level middleware is an additional gate for the GET submission endpoint.
	reveal := platformauth.RequirePermission(tdb, "yearend:reveal")

	// --- Tenant-level yearend routes ---
	ye := rg.Group("/yearend")
	ye.Use(requireAuth)
	ye.GET("/settings", read, h.GetSettings)
	ye.PUT("/settings", write, h.UpsertSettings)

	// --- Per-employee yearend routes ---
	empYE := rg.Group("/employees/:id/yearend")
	empYE.Use(requireAuth)

	// Submissions
	empYE.POST("/submissions", write, h.UpsertSubmission)
	empYE.POST("/submissions/submit", write, h.SubmitSubmission)
	// GET without reveal=true uses yearend:read; reveal=true requires yearend:reveal.
	// We register both: the reveal variant requires the stricter permission.
	empYE.GET("/submissions", read, h.GetSubmission)
	// Dedicated reveal endpoint for explicit separation of duties.
	empYE.GET("/submissions/reveal", reveal, h.GetSubmission)

	// Calculations
	empYE.POST("/calculations", write, h.RunCalculation)
	empYE.POST("/calculations/finalise", write, h.FinaliseCalculation)
	empYE.GET("/calculations", read, h.GetCalculation)

	// Reports
	empYE.POST("/reports/withholding-slip", write, h.GenerateWithholdingSlip)

	// Payroll SaaS push (足場)
	empYE.POST("/payroll-push", write, h.PushToPayroll)
}
