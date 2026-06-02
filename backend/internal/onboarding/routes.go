package onboarding

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires onboarding, intake form, and offboarding endpoints.
// The group should already have RequireAuth applied by the caller.
//
// Endpoints:
//
//	POST   /onboarding/templates                          — onboarding:write
//	GET    /onboarding/templates                          — onboarding:read
//	POST   /employees/:id/onboarding/tasks/generate       — onboarding:write
//	GET    /employees/:id/onboarding/tasks                — onboarding:read
//	PATCH  /onboarding/tasks/:task_id/status              — onboarding:write
//	PATCH  /onboarding/tasks/:task_id/assign              — onboarding:write
//	POST   /employees/:id/intake                          — intake:write
//	GET    /employees/:id/intake                          — intake:read
//	GET    /employees/:id/intake/sensitive                — intake:read_sensitive
//	POST   /employees/:id/intake/verify                   — intake:write
//	POST   /employees/:id/offboarding                     — onboarding:write
//	POST   /employees/:id/offboarding/complete            — onboarding:write
//	GET    /employees/:id/offboarding/policy              — onboarding:read
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) {
	svc := NewService(tdb)
	h := NewHandler(svc)

	obRead := platformauth.RequirePermission(tdb, "onboarding:read")
	obWrite := platformauth.RequirePermission(tdb, "onboarding:write")
	intakeRead := platformauth.RequirePermission(tdb, "intake:read")
	intakeWrite := platformauth.RequirePermission(tdb, "intake:write")
	intakeSensitive := platformauth.RequirePermission(tdb, "intake:read_sensitive")

	// --- Checklist template routes ---
	templates := rg.Group("/onboarding/templates")
	templates.Use(requireAuth)
	{
		templates.POST("", obWrite, h.CreateTemplate)
		templates.GET("", obRead, h.ListTemplates)
	}

	// --- Task routes (nested under employee) ---
	// Generate and list tasks.
	empTasks := rg.Group("/employees/:id/onboarding/tasks")
	empTasks.Use(requireAuth)
	{
		empTasks.POST("/generate", obWrite, h.GenerateTasks)
		empTasks.GET("", obRead, h.ListTasks)
	}

	// Top-level task mutation routes (by task_id).
	tasks := rg.Group("/onboarding/tasks")
	tasks.Use(requireAuth)
	{
		tasks.PATCH("/:task_id/status", obWrite, h.UpdateTaskStatus)
		tasks.PATCH("/:task_id/assign", obWrite, h.AssignTask)
	}

	// --- Intake form routes ---
	intake := rg.Group("/employees/:id/intake")
	intake.Use(requireAuth)
	{
		intake.POST("", intakeWrite, h.SubmitIntakeForm)
		// Standard read (masked bank account):
		intake.GET("", intakeRead, h.GetIntakeForm)
		// Sensitive read (decrypted bank account — requires intake:read_sensitive):
		intake.GET("/sensitive", intakeSensitive, h.GetIntakeFormSensitive)
		intake.POST("/verify", intakeWrite, h.VerifyIntakeForm)
	}

	// --- Offboarding routes ---
	offboard := rg.Group("/employees/:id/offboarding")
	offboard.Use(requireAuth)
	{
		offboard.POST("", obWrite, h.InitiateOffboarding)
		offboard.POST("/complete", obWrite, h.CompleteOffboarding)
		offboard.GET("/policy", obRead, h.GetOffboardingPolicy)
	}
}
