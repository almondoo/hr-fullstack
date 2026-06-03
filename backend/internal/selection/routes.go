package selection

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires the selection-pipeline endpoints.
// The group should already have RequireAuth applied per-route group below.
//
// Permission namespace: ats:* (ats:pipeline / ats:application).
//
// Endpoints:
//
//	POST   /selection/stage-templates                     — ats:pipeline:write → ats:pipeline (2-seg)
//	GET    /selection/stage-templates                     — ats:pipeline (read)
//	POST   /job-postings/:id/selection/stages             — ats:pipeline (write)
//	GET    /job-postings/:id/selection/stages             — ats:pipeline (read)
//	GET    /job-postings/:id/selection/kanban             — ats:application (read)
//	POST   /selection/applications                        — ats:application (write)
//	GET    /selection/applications/:app_id                — ats:application (read)
//	POST   /selection/applications/:app_id/move           — ats:application (write)
//	GET    /selection/applications/:app_id/history        — ats:application (read)
//	POST   /selection/message-templates                   — ats:pipeline (write)
//	GET    /selection/message-templates                   — ats:pipeline (read)
//
// Permissions follow the 2-segment "resource:action" convention. The pipeline
// configuration (stages/templates) uses ats:pipeline_read / ats:pipeline_write;
// applications use ats:application_read / ats:application_write.
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) {
	svc := NewService(tdb)
	h := NewHandler(svc)

	pipelineRead := platformauth.RequirePermission(tdb, "ats:pipeline_read")
	pipelineWrite := platformauth.RequirePermission(tdb, "ats:pipeline_write")
	appRead := platformauth.RequirePermission(tdb, "ats:application_read")
	appWrite := platformauth.RequirePermission(tdb, "ats:application_write")

	// --- Stage templates (tenant standard) ---
	stageTemplates := rg.Group("/selection/stage-templates")
	stageTemplates.Use(requireAuth)
	stageTemplates.POST("", pipelineWrite, h.CreateStageTemplate)
	stageTemplates.GET("", pipelineRead, h.ListStageTemplates)

	// --- Candidate message templates ---
	messageTemplates := rg.Group("/selection/message-templates")
	messageTemplates.Use(requireAuth)
	messageTemplates.POST("", pipelineWrite, h.CreateMessageTemplate)
	messageTemplates.GET("", pipelineRead, h.ListMessageTemplates)

	// --- Per-job-posting stages + kanban ---
	jobStages := rg.Group("/job-postings/:id/selection")
	jobStages.Use(requireAuth)
	jobStages.POST("/stages", pipelineWrite, h.InitStages)
	jobStages.GET("/stages", pipelineRead, h.ListStages)
	jobStages.GET("/kanban", appRead, h.GetKanban)

	// --- Applications + transitions + history ---
	applications := rg.Group("/selection/applications")
	applications.Use(requireAuth)
	applications.POST("", appWrite, h.CreateApplication)
	applications.GET("/:app_id", appRead, h.GetApplication)
	applications.POST("/:app_id/move", appWrite, h.MoveStage)
	applications.GET("/:app_id/history", appRead, h.ListHistory)
}
