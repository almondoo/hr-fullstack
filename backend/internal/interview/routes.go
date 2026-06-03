package interview

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires the interview scheduling and evaluation endpoints.
// The caller supplies requireAuth; each endpoint additionally enforces a
// two-segment "resource:action" permission via RequirePermission.
//
// Permissions:
//
//	ats:interview:read   — read interviews / slots / panellists
//	ats:interview:write  — create / confirm / transition / assign / calendar
//	ats:evaluation:read  — read interview evaluations (sensitive comments)
//	ats:evaluation:write — submit evaluations, manage sheets / settings
//
// Note: the evaluation-read permission is three segments by design
// ("ats:evaluation:read").  RequirePermission compares the exact string, so a
// matching exact grant (or "*") is required; a two-segment "ats:*" wildcard
// will NOT grant it (see auth.HasPermission docs).
//
// Endpoints:
//
//	POST   /interviews                                    — ats:interview:write
//	GET    /interviews                                    — ats:interview:read
//	GET    /interviews/:id                                — ats:interview:read
//	POST   /interviews/:id/slots                          — ats:interview:write
//	GET    /interviews/:id/slots                          — ats:interview:read
//	POST   /interviews/:id/panelists                      — ats:interview:write
//	GET    /interviews/:id/panelists                      — ats:interview:read
//	POST   /interviews/:id/confirm                        — ats:interview:write
//	POST   /interviews/:id/transition                     — ats:interview:write
//	PATCH  /interviews/:id/external-event                 — ats:interview:write
//	POST   /interviews/:id/evaluations                    — ats:evaluation:write
//	GET    /interviews/:id/evaluations                    — ats:evaluation:read
//	POST   /evaluation-sheets                             — ats:evaluation:write
//	GET    /evaluation-sheets                             — ats:evaluation:read
//	PUT    /interview-settings/peer-eval-visibility       — ats:evaluation:write
//	GET    /applications/:application_id/evaluation-summary — ats:evaluation:read
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) {
	svc := NewService(tdb)
	h := NewHandler(svc)

	ivRead := platformauth.RequirePermission(tdb, "ats:interview:read")
	ivWrite := platformauth.RequirePermission(tdb, "ats:interview:write")
	evalRead := platformauth.RequirePermission(tdb, "ats:evaluation:read")
	evalWrite := platformauth.RequirePermission(tdb, "ats:evaluation:write")

	// --- Interview routes ---
	interviews := rg.Group("/interviews")
	interviews.Use(requireAuth)
	interviews.POST("", ivWrite, h.CreateInterview)
	interviews.GET("", ivRead, h.ListInterviews)
	interviews.GET("/:id", ivRead, h.GetInterview)
	interviews.POST("/:id/slots", ivWrite, h.AddSlot)
	interviews.GET("/:id/slots", ivRead, h.ListSlots)
	interviews.POST("/:id/panellists", ivWrite, h.AddPanelist)
	interviews.GET("/:id/panellists", ivRead, h.ListPanelists)
	interviews.POST("/:id/confirm", ivWrite, h.ConfirmInterview)
	interviews.POST("/:id/transition", ivWrite, h.TransitionInterview)
	interviews.PATCH("/:id/external-event", ivWrite, h.SetExternalEvent)
	interviews.POST("/:id/evaluations", evalWrite, h.SubmitEvaluation)
	interviews.GET("/:id/evaluations", evalRead, h.ListEvaluations)

	// --- Evaluation sheet routes ---
	sheets := rg.Group("/evaluation-sheets")
	sheets.Use(requireAuth)
	sheets.POST("", evalWrite, h.CreateSheet)
	sheets.GET("", evalRead, h.ListSheets)

	// --- Tenant interview settings ---
	settings := rg.Group("/interview-settings")
	settings.Use(requireAuth)
	settings.PUT("/peer-eval-visibility", evalWrite, h.SetPeerEvalVisibility)

	// --- Evaluation aggregation by application ---
	apps := rg.Group("/applications/:application_id")
	apps.Use(requireAuth)
	apps.GET("/evaluation-summary", evalRead, h.SummarizeApplication)
}
