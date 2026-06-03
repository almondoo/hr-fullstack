package oneonone

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires the 1on1 (ST-TM-03) endpoints.
// The group should already have RequireAuth applied by the caller; each route
// additionally enforces an RBAC permission.
//
// Permission namespace:
//
//	oneonone:read   — read series / sessions / agenda / notes / actions / metadata / settings
//	oneonone:write  — create / mutate series / sessions / agenda / notes / actions / settings
//
// Endpoints:
//
//	POST   /oneonone/series                                   — oneonone:write
//	GET    /oneonone/series                                   — oneonone:read
//	PATCH  /oneonone/series/:series_id/manager               — oneonone:write
//	POST   /oneonone/series/:series_id/close                 — oneonone:write
//	GET    /oneonone/series/:series_id/metadata             — oneonone:read (meta only, no bodies)
//	GET    /oneonone/series/:series_id/open-actions         — oneonone:read
//	POST   /oneonone/series/:series_id/sessions             — oneonone:write
//	GET    /oneonone/series/:series_id/sessions             — oneonone:read
//	PATCH  /oneonone/sessions/:session_id/status            — oneonone:write
//	POST   /oneonone/sessions/:session_id/agenda            — oneonone:write
//	GET    /oneonone/sessions/:session_id/agenda            — oneonone:read
//	POST   /oneonone/sessions/:session_id/agenda/carry-over — oneonone:write
//	POST   /oneonone/sessions/:session_id/notes             — oneonone:write
//	GET    /oneonone/sessions/:session_id/notes             — oneonone:read (visibility-scoped)
//	POST   /oneonone/sessions/:session_id/actions           — oneonone:write
//	PATCH  /oneonone/actions/:action_id/status              — oneonone:write
//	GET    /oneonone/settings                                — oneonone:read
//	PUT    /oneonone/settings                                — oneonone:write
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) {
	svc := NewService(tdb)
	h := NewHandler(svc)

	read := platformauth.RequirePermission(tdb, "oneonone:read")
	write := platformauth.RequirePermission(tdb, "oneonone:write")

	// --- Series ---
	series := rg.Group("/oneonone/series")
	series.Use(requireAuth)
	series.POST("", write, h.CreateSeries)
	series.GET("", read, h.ListSeries)
	series.PATCH("/:series_id/manager", write, h.UpdateSeriesManager)
	series.POST("/:series_id/close", write, h.CloseSeries)
	series.GET("/:series_id/metadata", read, h.GetSeriesMetadata)
	series.GET("/:series_id/open-actions", read, h.ListOpenActions)
	// Sessions nested under a series.
	series.POST("/:series_id/sessions", write, h.CreateSession)
	series.GET("/:series_id/sessions", read, h.ListSessions)

	// --- Sessions (by session_id) ---
	sessions := rg.Group("/oneonone/sessions")
	sessions.Use(requireAuth)
	sessions.PATCH("/:session_id/status", write, h.UpdateSessionStatus)
	sessions.POST("/:session_id/agenda", write, h.AddAgendaItem)
	sessions.GET("/:session_id/agenda", read, h.ListAgendaItems)
	sessions.POST("/:session_id/agenda/carry-over", write, h.CarryOverAgenda)
	sessions.POST("/:session_id/notes", write, h.AddNote)
	sessions.GET("/:session_id/notes", read, h.ListNotes)
	sessions.POST("/:session_id/actions", write, h.AddAction)

	// --- Actions (by action_id) ---
	actions := rg.Group("/oneonone/actions")
	actions.Use(requireAuth)
	actions.PATCH("/:action_id/status", write, h.UpdateActionStatus)

	// --- Settings ---
	settings := rg.Group("/oneonone/settings")
	settings.Use(requireAuth)
	settings.GET("", read, h.GetSettings)
	settings.PUT("", write, h.UpsertSettings)
}
