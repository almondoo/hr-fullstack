package offer

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires offer, offer-letter, and offer-response endpoints.
// The group should already have RequireAuth applied by the caller.
//
// The approval engine is constructed internally by NewService so this signature
// stays uniform across all stories (no approval.Service argument).
//
// Permission convention: two-segment "resource:action" (offer namespace).
//
// Endpoints:
//
//	GET    /offers/settings                  — offer:read
//	PUT    /offers/settings                  — offer:write
//	POST   /offers                           — offer:write
//	GET    /offers                           — offer:read
//	GET    /offers/:id                       — offer:read           (salary masked)
//	GET    /offers/:id/sensitive             — offer:read_sensitive (salary decrypted)
//	POST   /offers/:id/submit-approval       — offer:write
//	POST   /offers/:id/send                  — offer:write
//	POST   /offers/:id/rescind               — offer:write
//	POST   /offers/:id/letters               — offer:write
//	GET    /offers/:id/letters               — offer:read
//	POST   /offers/:id/respond               — offer:write
//	GET    /offers/:id/responses             — offer:read
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) {
	svc := NewService(tdb)
	h := NewHandler(svc)

	offerRead := platformauth.RequirePermission(tdb, "offer:read")
	offerWrite := platformauth.RequirePermission(tdb, "offer:write")
	offerSensitive := platformauth.RequirePermission(tdb, "offer:read_sensitive")

	offers := rg.Group("/offers")
	offers.Use(requireAuth)

	// Settings (legal configuration).
	offers.GET("/settings", offerRead, h.GetSettings)
	offers.PUT("/settings", offerWrite, h.UpsertSettings)

	// Offer CRUD + lifecycle.
	offers.POST("", offerWrite, h.CreateOffer)
	offers.GET("", offerRead, h.ListOffers)
	offers.GET("/:id", offerRead, h.GetOffer)
	offers.GET("/:id/sensitive", offerSensitive, h.GetOfferSensitive)
	offers.POST("/:id/submit-approval", offerWrite, h.SubmitForApproval)
	offers.POST("/:id/send", offerWrite, h.SendOffer)
	offers.POST("/:id/rescind", offerWrite, h.RescindOffer)

	// Offer letters (CMP-006 signing evidence).
	offers.POST("/:id/letters", offerWrite, h.IssueLetter)
	offers.GET("/:id/letters", offerRead, h.ListLetters)

	// Candidate responses (ST-ATS-06 trigger on acceptance).
	offers.POST("/:id/respond", offerWrite, h.Respond)
	offers.GET("/:id/responses", offerRead, h.ListResponses)
}
