package workrule

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires the work rule and labour agreement endpoints.
// The group should already have RequireAuth applied by the caller; each route
// additionally enforces a RequirePermission middleware.
//
// Permission namespaces:
//
//	workrule:read    — read work rules / versions / acknowledgements / settings
//	workrule:write   — create/update work rules, versions, settings, acknowledge
//	workrule:publish — publish a draft version as the current version
//	agreement:read   — read labour agreement documents
//	agreement:write  — create labour agreement documents
//	agreement:file   — change a labour agreement's electronic filing status
//
// Endpoints:
//
//	GET    /workrules/settings                                   — workrule:read
//	PUT    /workrules/settings                                   — workrule:write
//	POST   /workrules                                            — workrule:write
//	GET    /workrules                                            — workrule:read
//	GET    /workrules/:id                                        — workrule:read
//	POST   /workrules/:id/versions                              — workrule:write
//	GET    /workrules/:id/versions                              — workrule:read
//	POST   /workrule-versions/:version_id/publish              — workrule:publish
//	POST   /workrule-versions/:version_id/acknowledge          — workrule:write
//	GET    /workrule-versions/:version_id/acknowledgements     — workrule:read
//	GET    /workrule-versions/:version_id/unacknowledged       — workrule:read
//	POST   /labor-agreements                                    — agreement:write
//	GET    /labor-agreements                                    — agreement:read
//	GET    /labor-agreements/expiring                           — agreement:read
//	GET    /labor-agreements/:agreement_id                      — agreement:read
//	GET    /labor-agreements/:agreement_id/linked-limits       — agreement:read
//	PATCH  /labor-agreements/:agreement_id/filing-status        — agreement:file
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) {
	svc := NewService(tdb)
	h := NewHandler(svc)

	wrRead := platformauth.RequirePermission(tdb, "workrule:read")
	wrWrite := platformauth.RequirePermission(tdb, "workrule:write")
	wrPublish := platformauth.RequirePermission(tdb, "workrule:publish")
	agRead := platformauth.RequirePermission(tdb, "agreement:read")
	agWrite := platformauth.RequirePermission(tdb, "agreement:write")
	agFile := platformauth.RequirePermission(tdb, "agreement:file")

	// --- Work rules + settings + versions (nested by rule id) ---
	workrules := rg.Group("/workrules")
	workrules.Use(requireAuth)
	workrules.GET("/settings", wrRead, h.GetSettings)
	workrules.PUT("/settings", wrWrite, h.UpsertSettings)
	workrules.POST("", wrWrite, h.CreateWorkRule)
	workrules.GET("", wrRead, h.ListWorkRules)
	workrules.GET("/:id", wrRead, h.GetWorkRule)
	workrules.POST("/:id/versions", wrWrite, h.CreateVersion)
	workrules.GET("/:id/versions", wrRead, h.ListVersions)

	// --- Version-scoped actions (by version_id) ---
	versions := rg.Group("/workrule-versions")
	versions.Use(requireAuth)
	versions.POST("/:version_id/publish", wrPublish, h.PublishVersion)
	versions.POST("/:version_id/acknowledge", wrWrite, h.Acknowledge)
	versions.GET("/:version_id/acknowledgements", wrRead, h.ListAcknowledgements)
	versions.GET("/:version_id/unacknowledged", wrRead, h.ListUnacknowledged)

	// --- Labour agreement documents ---
	agreements := rg.Group("/labor-agreements") //nolint:misspell // URL path is API contract
	agreements.Use(requireAuth)
	agreements.POST("", agWrite, h.CreateAgreement)
	agreements.GET("", agRead, h.ListAgreements)
	agreements.GET("/expiring", agRead, h.ListExpiringAgreements)
	agreements.GET("/:agreement_id", agRead, h.GetAgreement)
	agreements.GET("/:agreement_id/linked-limits", agRead, h.GetLinkedLimits)
	agreements.PATCH("/:agreement_id/filing-status", agFile, h.UpdateFilingStatus)
}
