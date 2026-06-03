package govfiling

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires the govfiling (社会保険・労働保険 帳票/電子申請) endpoints.
// The group should already have RequireAuth applied by the caller; each route
// additionally enforces a "filing:<action>" permission.
//
// Endpoints:
//
//	GET    /govfilings/settings                       — filing:read
//	PUT    /govfilings/settings                       — filing:admin
//	POST   /govfilings/judge/monthly-change           — filing:read
//	POST   /govfilings                                — filing:write
//	GET    /govfilings                                — filing:read   (?employee_id=)
//	GET    /govfilings/:id                            — filing:read
//	POST   /govfilings/:id/submit                     — filing:write
//	PATCH  /govfilings/:id/status                     — filing:write
//	GET    /govfilings/:id/history                    — filing:read
//	POST   /govfilings/:id/documents                  — filing:write
//	GET    /govfilings/:id/documents                  — filing:read
//	GET    /govfiling-documents/:doc_id/content       — filing:read_sensitive
//
// Note: the sensitive document-content route lives under a distinct
// /govfiling-documents prefix (not /govfilings/...) to avoid a gin
// router param/literal conflict with the /govfilings/:id routes.
// RegisterRoutes is defined above (see package-level doc comment).
// An optional MynumberProvider enables 個人番号 provision for social-insurance
// filings; pass nil (or omit) to disable.
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc, mnProvider ...MynumberProvider) {
	svc := NewService(tdb)
	if len(mnProvider) > 0 && mnProvider[0] != nil {
		svc = svc.WithMynumberProvider(mnProvider[0])
	}
	h := NewHandler(svc)

	filingRead := platformauth.RequirePermission(tdb, "filing:read")
	filingWrite := platformauth.RequirePermission(tdb, "filing:write")
	filingAdmin := platformauth.RequirePermission(tdb, "filing:admin")
	filingSensitive := platformauth.RequirePermission(tdb, "filing:read_sensitive")

	grp := rg.Group("/govfilings")
	grp.Use(requireAuth)

	// Settings (法令値の設定化)
	grp.GET("/settings", filingRead, h.GetSettings)
	grp.PUT("/settings", filingAdmin, h.UpsertSettings)

	// Grade judgement (設定駆動)
	grp.POST("/judge/monthly-change", filingRead, h.JudgeMonthlyChange)

	// Filings
	grp.POST("", filingWrite, h.CreateFiling)
	grp.GET("", filingRead, h.ListFilings)
	grp.GET("/:id", filingRead, h.GetFiling)
	grp.POST("/:id/submit", filingWrite, h.SubmitFiling)
	grp.PATCH("/:id/status", filingWrite, h.UpdateStatus)
	grp.GET("/:id/history", filingRead, h.ListStatusHistory)

	// Documents (公文書/帳票)
	grp.POST("/:id/documents", filingWrite, h.AttachDocument)
	grp.GET("/:id/documents", filingRead, h.ListDocuments)

	// Sensitive document content (decrypted body — requires filing:read_sensitive).
	// Under a distinct prefix to avoid a gin param/literal route conflict with
	// the /govfilings/:id group above.
	docs := rg.Group("/govfiling-documents")
	docs.Use(requireAuth)

	docs.GET("/:doc_id/content", filingSensitive, h.GetDocumentContent)
}
