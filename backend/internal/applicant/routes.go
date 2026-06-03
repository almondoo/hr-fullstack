package applicant

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires applicant (ST-ATS-02) endpoints.
// The group should already have RequireAuth applied by the caller; each route
// additionally enforces a fine-grained RBAC permission.
//
// Permission namespace (spec ST-ATS-02 dependencies):
//
//	ats:applicant:read            — read applicant records (masked contact PII)
//	ats:applicant:read_sensitive  — decrypt contact PII (email/phone)
//	ats:applicant:write           — create/update/merge/consent/documents
//
// Note: ats:applicant:read_sensitive is a three-segment permission; the
// platform HasPermission matches it exactly (a two-segment "ats:*" wildcard does
// NOT grant it, by design — sensitive PII access must be granted explicitly).
//
// Endpoints:
//
//	POST   /applicants                          — ats:applicant:write
//	GET    /applicants                          — ats:applicant:read
//	GET    /applicants/:id                      — ats:applicant:read         (masked contact)
//	GET    /applicants/:id/sensitive            — ats:applicant:read_sensitive (decrypted contact)
//	PATCH  /applicants/:id/status               — ats:applicant:write
//	POST   /applicants/:id/anonymize            — ats:applicant:write
//	POST   /applicants/:id/documents            — ats:applicant:write
//	GET    /applicants/:id/documents            — ats:applicant:read
//	POST   /applicants/:id/consents             — ats:applicant:write
//	GET    /applicants/:id/consents             — ats:applicant:read
//	GET    /applicants/:id/duplicates           — ats:applicant:read
//	POST   /applicants/:id/merge                — ats:applicant:write
//	GET    /applicants/:id/merges               — ats:applicant:read
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) {
	svc := NewService(tdb)
	h := NewHandler(svc)

	read := platformauth.RequirePermission(tdb, "ats:applicant:read")
	readSensitive := platformauth.RequirePermission(tdb, "ats:applicant:read_sensitive")
	write := platformauth.RequirePermission(tdb, "ats:applicant:write")

	grp := rg.Group("/applicants")
	grp.Use(requireAuth)

	grp.POST("", write, h.CreateApplicant)
	grp.GET("", read, h.ListApplicants)
	grp.GET("/:id", read, h.GetApplicant)
	grp.GET("/:id/sensitive", readSensitive, h.GetApplicantSensitive)
	grp.PATCH("/:id/status", write, h.UpdateStatus)
	grp.POST("/:id/anonymize", write, h.Anonymize)

	grp.POST("/:id/documents", write, h.AddDocument)
	grp.GET("/:id/documents", read, h.ListDocuments)

	grp.POST("/:id/consents", write, h.RecordConsent)
	grp.GET("/:id/consents", read, h.ListConsents)

	grp.GET("/:id/duplicates", read, h.FindDuplicates)
	grp.POST("/:id/merge", write, h.Merge)
	grp.GET("/:id/merges", read, h.ListMerges)
}
