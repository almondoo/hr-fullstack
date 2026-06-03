package selfservice

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires self-service change-request, CSV import, and document
// store endpoints.  The group should already have RequireAuth applied by the
// caller; each endpoint additionally enforces a 2-segment RBAC permission.
//
// The approval engine is constructed internally by NewService, so the
// RegisterRoutes signature stays uniform across all stories.
//
// Endpoints:
//
//	POST   /selfservice/change-requests                         — selfservice:write
//	GET    /selfservice/change-requests                         — selfservice:read
//	GET    /selfservice/change-requests/:id                     — selfservice:read
//	GET    /selfservice/change-requests/:id/sensitive           — selfservice:read_sensitive
//	POST   /selfservice/change-requests/:id/approve             — selfservice:approve
//	POST   /selfservice/change-requests/:id/reject              — selfservice:approve
//	POST   /selfservice/imports/validate                        — selfservice:import
//	POST   /selfservice/imports/apply                           — selfservice:import
//	POST   /selfservice/documents                               — documents:write
//	GET    /selfservice/documents                               — documents:read
//	GET    /selfservice/documents/:id                           — documents:read
//	POST   /selfservice/documents/:id/versions                  — documents:write
//	GET    /selfservice/documents/:id/versions                  — documents:read
//	POST   /selfservice/documents/:id/expire                    — documents:admin
//	GET    /selfservice/document-versions/:version_id/download  — documents:download
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) {
	svc := NewService(tdb)
	h := NewHandler(svc)

	ssRead := platformauth.RequirePermission(tdb, "selfservice:read")
	ssWrite := platformauth.RequirePermission(tdb, "selfservice:write")
	ssSensitive := platformauth.RequirePermission(tdb, "selfservice:read_sensitive")
	ssApprove := platformauth.RequirePermission(tdb, "selfservice:approve")
	ssImport := platformauth.RequirePermission(tdb, "selfservice:import")

	docRead := platformauth.RequirePermission(tdb, "documents:read")
	docWrite := platformauth.RequirePermission(tdb, "documents:write")
	docAdmin := platformauth.RequirePermission(tdb, "documents:admin")
	docDownload := platformauth.RequirePermission(tdb, "documents:download")

	// --- Self-service change requests ---
	cr := rg.Group("/selfservice/change-requests")
	cr.Use(requireAuth)

	cr.POST("", ssWrite, h.SubmitChangeRequest)
	cr.GET("", ssRead, h.ListChangeRequests)
	cr.GET("/:id", ssRead, h.GetChangeRequest)
	cr.GET("/:id/sensitive", ssSensitive, h.GetChangeRequestSensitive)
	cr.POST("/:id/approve", ssApprove, h.ApproveChangeRequest)
	cr.POST("/:id/reject", ssApprove, h.RejectChangeRequest)

	// --- CSV bulk import ---
	imports := rg.Group("/selfservice/imports")
	imports.Use(requireAuth)

	imports.POST("/validate", ssImport, h.ValidateCSV)
	imports.POST("/apply", ssImport, h.ApplyCSV)

	// --- Document store ---
	docs := rg.Group("/selfservice/documents")
	docs.Use(requireAuth)

	docs.POST("", docWrite, h.CreateDocument)
	docs.GET("", docRead, h.ListDocuments)
	docs.GET("/:id", docRead, h.GetDocument)
	docs.POST("/:id/versions", docWrite, h.AddVersion)
	docs.GET("/:id/versions", docRead, h.ListVersions)
	docs.POST("/:id/expire", docAdmin, h.ExpireDocument)

	// --- Document version download (separate top-level group by version id) ---
	dv := rg.Group("/selfservice/document-versions")
	dv.Use(requireAuth)

	dv.GET("/:version_id/download", docDownload, h.DownloadVersion)
}
