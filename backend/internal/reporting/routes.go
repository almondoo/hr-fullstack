package reporting

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires the reporting / export and calendar / work-pattern
// endpoints.  The signature is identical across all stories so the central
// router (server.go) can wire it uniformly.
//
// Route-level RBAC (2-segment "resource:action"):
//
//	POST   /reports/definitions                       — reporting:write
//	GET    /reports/definitions                       — reporting:read
//	GET    /reports/run/:report_key                   — reporting:read
//	POST   /reports/exports                           — reporting:export
//	GET    /reports/exports/:job_id                   — reporting:read
//	POST   /reports/exports/:job_id/process           — reporting:export
//	POST   /calendars                                 — reporting:write
//	POST   /calendars/:calendar_id/days               — reporting:write
//	GET    /calendars/:calendar_id/business-day       — reporting:read
//	POST   /work-patterns                             — reporting:write
//	POST   /work-patterns/:work_pattern_id/shifts     — reporting:write
//	POST   /employees/:id/work-pattern                — reporting:write
//	GET    /employees/:id/work-pattern                — reporting:read
//
// The reporting:export_sensitive permission (for マイナンバー/口座/健診 columns)
// is enforced in the service layer when include_sensitive=true.
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) {
	svc := NewService(tdb)
	h := NewHandler(svc)

	read := platformauth.RequirePermission(tdb, "reporting:read")
	write := platformauth.RequirePermission(tdb, "reporting:write")
	export := platformauth.RequirePermission(tdb, "reporting:export")

	// --- Report definitions + execution ---
	reports := rg.Group("/reports")
	reports.Use(requireAuth)
	reports.POST("/definitions", write, h.UpsertReportDefinition)
	reports.GET("/definitions", read, h.ListReportDefinitions)
	reports.GET("/:report_key", read, h.RunReport)

	reports.POST("/exports", export, h.CreateExportJob)
	reports.GET("/exports/:job_id", read, h.GetExportJob)
	reports.POST("/exports/:job_id/process", export, h.ProcessExportJob)

	// --- Company calendars ---
	calendars := rg.Group("/calendars")
	calendars.Use(requireAuth)
	calendars.POST("", write, h.CreateCalendar)
	calendars.POST("/:calendar_id/days", write, h.AddCalendarDay)
	calendars.GET("/:calendar_id/business-day", read, h.IsBusinessDay)

	// --- Work patterns / shifts ---
	workPatterns := rg.Group("/work-patterns")
	workPatterns.Use(requireAuth)
	workPatterns.POST("", write, h.CreateWorkPattern)
	workPatterns.POST("/:work_pattern_id/shifts", write, h.AddShiftPattern)

	// --- Employee work-pattern assignment ---
	empWork := rg.Group("/employees/:id/work-pattern")
	empWork.Use(requireAuth)
	empWork.POST("", write, h.AssignWorkPattern)
	empWork.GET("", read, h.ResolveWorkPattern)
}
