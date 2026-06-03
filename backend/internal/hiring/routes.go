package hiring

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires the hiring (onboarding linkage) endpoints.
// The group should already have RequireAuth applied per-route below.
//
// Permission namespace: ats:onboarding (per ST-ATS-06 dependencies).
//
// Endpoints:
//
//	POST   /hiring/conversions                              — ats:onboarding_write
//	GET    /hiring/onboardings                              — ats:onboarding_read
//	GET    /hiring/onboardings/:id                          — ats:onboarding_read
//	PATCH  /hiring/onboardings/:id/status                   — ats:onboarding_write
//	POST   /hiring/onboardings/:id/complete                 — ats:onboarding_write
//	GET    /hiring/onboardings/:id/preboarding-requests     — ats:onboarding_read
//	GET    /hiring/onboardings/:id/surveys                  — ats:onboarding_read
//	POST   /hiring/preboarding-requests                     — ats:onboarding_write
//	PATCH  /hiring/preboarding-requests/:id/status          — ats:onboarding_write
//	POST   /hiring/surveys                                  — ats:onboarding_write
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) {
	svc := NewService(tdb)
	h := NewHandler(svc)

	read := platformauth.RequirePermission(tdb, "ats:onboarding_read")
	write := platformauth.RequirePermission(tdb, "ats:onboarding_write")

	// --- Conversion ---
	conversions := rg.Group("/hiring/conversions")
	conversions.Use(requireAuth)
	conversions.POST("", write, h.ConvertApplicant)

	// --- New-hire onboarding headers + nested children ---
	onboardings := rg.Group("/hiring/onboardings")
	onboardings.Use(requireAuth)
	onboardings.GET("", read, h.ListOnboardings)
	onboardings.GET("/:id", read, h.GetOnboarding)
	onboardings.PATCH("/:id/status", write, h.AdvanceOnboarding)
	onboardings.POST("/:id/complete", write, h.CompleteOnboarding)
	onboardings.GET("/:id/preboarding-requests", read, h.ListPreboardingRequests)
	onboardings.GET("/:id/surveys", read, h.ListSurveys)

	// --- Preboarding requests ---
	preboarding := rg.Group("/hiring/preboarding-requests")
	preboarding.Use(requireAuth)
	preboarding.POST("", write, h.CreatePreboardingRequest)
	preboarding.PATCH("/:id/status", write, h.UpdatePreboardingRequestStatus)

	// --- Surveys (ATS-023 stub) ---
	surveys := rg.Group("/hiring/surveys")
	surveys.Use(requireAuth)
	surveys.POST("", write, h.ScheduleSurvey)
}
