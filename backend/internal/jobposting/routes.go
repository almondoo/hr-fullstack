package jobposting

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires job posting (ATS foundation) endpoints.
// The signature is identical across all stories so the central wiring in
// server.go can call it uniformly.  The group should already have RequireAuth
// available; it is applied to the group below.
//
// Item-level budget permission (salary range / hiring budget):
//   - Read/List handlers always request budget data from the service
//     (ReadBudget=true).  The service layer re-validates ats:read_budget via
//     LoadUserPermissions inside the transaction and clears the budget fields
//     when the actor lacks the permission.  This is the authoritative
//     item-level gate (defence-in-depth) — no separate HTTP route is needed,
//     which also avoids gin static-vs-wildcard path collisions.
//
// Endpoints:
//
//	POST   /job-postings                                  — ats:write
//	GET    /job-postings                                  — ats:read (budget fields gated in service)
//	GET    /job-postings/:id                              — ats:read (budget fields gated in service)
//	PUT    /job-postings/:id                              — ats:write
//	PATCH  /job-postings/:id/status                       — ats:write
//	POST   /job-postings/:id/interviewers                 — ats:write
//	GET    /job-postings/:id/interviewers                 — ats:read
//	DELETE /job-postings/:id/interviewers/:user_id        — ats:write
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) {
	svc := NewService(tdb)
	h := NewHandler(svc)

	atsRead := platformauth.RequirePermission(tdb, "ats:read")
	atsWrite := platformauth.RequirePermission(tdb, "ats:write")

	grp := rg.Group("/job-postings")
	grp.Use(requireAuth)
	grp.POST("", atsWrite, h.CreateJobPosting)
	grp.GET("", atsRead, h.ListJobPostings)

	grp.GET("/:id", atsRead, h.GetJobPosting)
	grp.PUT("/:id", atsWrite, h.UpdateJobPosting)
	grp.PATCH("/:id/status", atsWrite, h.UpdateStatus)

	// --- Interviewer assignment routes ---
	grp.POST("/:id/interviewers", atsWrite, h.AssignInterviewer)
	grp.GET("/:id/interviewers", atsRead, h.ListInterviewers)
	grp.DELETE("/:id/interviewers/:user_id", atsWrite, h.RemoveInterviewer)
}
