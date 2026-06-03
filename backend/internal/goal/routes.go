package goal

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires the goal-management (MBO/OKR, cascade) endpoints.
// The signature is uniform across all stories; the approval engine is built
// internally inside NewService (no extra parameter).
//
// Permissions (RBAC, "resource:action" 2-segment):
//
//	goal:write     create/update goals, key results, progress, copy, submit, transition
//	goal:read      read own/visible goals, cycles, cascade, key results, progress logs
//	goal:read_all  list across the whole company (HR admin)
//	goal:approve   approve/return submitted goals (上司承認)
//
// Endpoints:
//
//	POST   /goal-cycles                          — goal:write
//	GET    /goal-cycles                          — goal:read_all
//	GET    /goal-cycles/:cycle_id                — goal:read
//	PATCH  /goal-cycles/:cycle_id/status         — goal:write
//	GET    /goal-cycles/:cycle_id/goals          — goal:read_all
//	POST   /goals                                — goal:write
//	GET    /goals/:goal_id                       — goal:read
//	PUT    /goals/:goal_id                       — goal:write
//	POST   /goals/:goal_id/submit                — goal:write
//	PATCH  /goals/:goal_id/status                — goal:approve
//	POST   /goals/:goal_id/progress              — goal:write
//	GET    /goals/:goal_id/progress              — goal:read
//	GET    /goals/:goal_id/cascade               — goal:read
//	POST   /goals/:goal_id/key-results           — goal:write
//	GET    /goals/:goal_id/key-results           — goal:read
//	PATCH  /key-results/:kr_id/progress          — goal:write
//	POST   /goal-cycles/copy-goals               — goal:write
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) {
	svc := NewService(tdb)
	h := NewHandler(svc)

	read := platformauth.RequirePermission(tdb, "goal:read")
	write := platformauth.RequirePermission(tdb, "goal:write")
	readAll := platformauth.RequirePermission(tdb, "goal:read_all")
	approve := platformauth.RequirePermission(tdb, "goal:approve")

	// --- Review cycle routes ---
	cycles := rg.Group("/goal-cycles")
	cycles.Use(requireAuth)
	cycles.POST("", write, h.CreateCycle)
	cycles.GET("", readAll, h.ListCycles)
	cycles.GET("/:cycle_id", read, h.GetCycle)
	cycles.PATCH("/:cycle_id/status", write, h.UpdateCycleStatus)
	cycles.GET("/:cycle_id/goals", readAll, h.ListGoals)
	// Cross-cycle copy lives here (not under /goals) to avoid a static-vs-param
	// routing conflict with /goals/:goal_id.
	cycles.POST("/copy-goals", write, h.CopyGoals)

	// --- Goal routes ---
	goals := rg.Group("/goals")
	goals.Use(requireAuth)
	goals.POST("", write, h.CreateGoal)
	goals.GET("/:goal_id", read, h.GetGoal)
	goals.PUT("/:goal_id", write, h.UpdateGoal)
	goals.POST("/:goal_id/submit", write, h.SubmitGoal)
	goals.PATCH("/:goal_id/status", approve, h.TransitionGoal)
	goals.POST("/:goal_id/progress", write, h.UpdateGoalProgress)
	goals.GET("/:goal_id/progress", read, h.ListProgressLogs)
	goals.GET("/:goal_id/cascade", read, h.GetCascadeTree)
	goals.POST("/:goal_id/key-results", write, h.AddKeyResult)
	goals.GET("/:goal_id/key-results", read, h.ListKeyResults)

	// --- Key result mutation by kr_id ---
	keyResults := rg.Group("/key-results")
	keyResults.Use(requireAuth)
	keyResults.PATCH("/:kr_id/progress", write, h.UpdateKeyResultProgress)
}
