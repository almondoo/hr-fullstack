package attendance

// routes.go — URL route registration for the attendance domain.
//
// Endpoints:
//
//	GET    /attendance/settings                          — attendance:read
//	PUT    /attendance/settings                          — attendance:write
//	POST   /attendance/records                           — attendance:write
//	GET    /attendance/records                           — attendance:read
//	GET    /attendance/records/:id                       — attendance:read
//	PATCH  /attendance/records/:id/correct               — attendance:write
//	POST   /attendance/summaries/compute                 — attendance:write
//	GET    /attendance/summaries                         — attendance:read
//	POST   /attendance/labor-agreements                  — laboragreement:write
//	GET    /attendance/labor-agreements                  — laboragreement:read
//	GET    /attendance/labor-agreements/alerts           — laboragreement:read

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires the attendance endpoints into the given RouterGroup.
// The group should already have RequireAuth applied by the caller.
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) {
	svc := NewService(tdb)
	h := NewHandler(svc)

	attRead := platformauth.RequirePermission(tdb, "attendance:read")
	attWrite := platformauth.RequirePermission(tdb, "attendance:write")
	laRead := platformauth.RequirePermission(tdb, "laboragreement:read")
	laWrite := platformauth.RequirePermission(tdb, "laboragreement:write")

	att := rg.Group("/attendance")
	att.Use(requireAuth)
	{
		// Settings (テナント設定)
		att.GET("/settings", attRead, h.GetSettings)
		att.PUT("/settings", attWrite, h.UpsertSettings)

		// Attendance records (打刻)
		att.POST("/records", attWrite, h.CreateRecord)
		att.GET("/records", attRead, h.ListRecords)
		att.GET("/records/:id", attRead, h.GetRecord)
		att.PATCH("/records/:id/correct", attWrite, h.CorrectRecord)

		// Work summaries (月次集計)
		att.POST("/summaries/compute", attWrite, h.ComputeSummary)
		att.GET("/summaries", attRead, h.GetSummary)

		// Labor agreements (36協定)
		att.POST("/labor-agreements", laWrite, h.CreateAgreement)
		att.GET("/labor-agreements", laRead, h.ListAgreements)
		att.GET("/labor-agreements/alerts", laRead, h.EvaluateAlerts)
	}
}
