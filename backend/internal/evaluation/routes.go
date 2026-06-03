package evaluation

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires the evaluation (performance review) endpoints.
// The group should already have RequireAuth applied by the caller; each route
// additionally enforces a "review:<action>" RBAC permission.
//
// The approval engine is self-constructed inside NewService, so this function
// keeps the unified RegisterRoutes signature shared by all story packages.
//
// Endpoints:
//
//	POST   /evaluations/templates                                   — review:write
//	GET    /evaluations/templates                                   — review:read
//	POST   /evaluations/reviews                                     — review:write
//	GET    /evaluations/reviews                                     — review:read
//	GET    /evaluations/reviews/:id                                 — review:read
//	POST   /evaluations/reviews/:id/entries                         — review:write
//	GET    /evaluations/reviews/:id/entries                         — review:read
//	POST   /evaluations/reviews/:id/submit                          — review:write
//	POST   /evaluations/reviews/:id/confirm                         — review:confirm
//	POST   /evaluations/reviews/:id/360/requests                    — review:write
//	GET    /evaluations/reviews/:id/360/aggregate                   — review:read
//	POST   /evaluations/360/requests/:request_id/respond           — review:write
//	POST   /evaluations/calibration-sessions                        — review:calibrate
//	POST   /evaluations/calibration-sessions/:session_id/decisions  — review:calibrate
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) {
	svc := NewService(tdb)
	h := NewHandler(svc)

	read := platformauth.RequirePermission(tdb, "review:read")
	write := platformauth.RequirePermission(tdb, "review:write")
	confirm := platformauth.RequirePermission(tdb, "review:confirm")
	calibrate := platformauth.RequirePermission(tdb, "review:calibrate")

	grp := rg.Group("/evaluations")
	grp.Use(requireAuth)

	// Templates
	grp.POST("/templates", write, h.CreateTemplate)
	grp.GET("/templates", read, h.ListTemplates)

	// Reviews
	grp.POST("/reviews", write, h.CreateReview)
	grp.GET("/reviews", read, h.ListReviews)
	grp.GET("/reviews/:id", read, h.GetReview)

	// Entries
	grp.POST("/reviews/:id/entries", write, h.UpsertEntry)
	grp.GET("/reviews/:id/entries", read, h.ListEntries)

	// Stage workflow
	grp.POST("/reviews/:id/submit", write, h.SubmitStage)
	grp.POST("/reviews/:id/confirm", confirm, h.ConfirmReview)

	// 360-degree
	grp.POST("/reviews/:id/360/requests", write, h.Create360Request)
	grp.GET("/reviews/:id/360/aggregate", read, h.Aggregate360)
	grp.POST("/360/requests/:request_id/respond", write, h.Submit360Response)

	// Calibration
	grp.POST("/calibration-sessions", calibrate, h.CreateCalibrationSession)
	grp.POST("/calibration-sessions/:session_id/decisions", calibrate, h.ApplyCalibration)
}
