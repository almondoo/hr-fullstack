package talent

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires the talent-management endpoints (ST-TM-04).
// The router group should already have RequireAuth applied by the caller; this
// function additionally calls grp.Use(requireAuth) on each sub-group and applies
// RequirePermission on every endpoint.
//
// Permission namespaces:
//
//	profile:read        — integrated profile, org tree
//	talent:read_sensitive — unmask compensation/grade fields in the profile
//	skill:read / skill:write — skill master, employee skills, skill map
//	placement:read / placement:write — placement simulations
//	survey:read / survey:write — pulse surveys, responses, aggregation
//	survey:read_freetext — decrypt a response's free-text answer
//
// Endpoints:
//
//	POST   /talent/skills                                   — skill:write
//	GET    /talent/skills                                   — skill:read
//	GET    /talent/skills/:skill_id/holders                 — skill:read
//	GET    /talent/skill-matrix                             — skill:read
//	PUT    /employees/:id/skills                            — skill:write
//	GET    /employees/:id/skills                            — skill:read
//	POST   /employees/:id/certifications                    — skill:write
//	GET    /employees/:id/certifications                    — skill:read
//	GET    /talent/certifications/expiring                  — skill:read
//	GET    /employees/:id/profile                           — profile:read
//	GET    /talent/org-tree                                 — profile:read
//	POST   /talent/placement-simulations                    — placement:write
//	POST   /talent/placement-simulations/:sim_id/items      — placement:write
//	GET    /talent/placement-simulations/:sim_id/items      — placement:read
//	POST   /talent/placement-simulations/:sim_id/apply      — placement:write
//	POST   /talent/placement-simulations/:sim_id/discard    — placement:write
//	POST   /talent/surveys                                  — survey:write
//	PATCH  /talent/surveys/:survey_id/status                — survey:write
//	POST   /talent/surveys/:survey_id/responses             — survey:write
//	GET    /talent/surveys/:survey_id/aggregate             — survey:read
//	GET    /talent/survey-responses/:response_id/free-text  — survey:read_freetext
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) {
	svc := NewService(tdb)
	h := NewHandler(svc)

	profileRead := platformauth.RequirePermission(tdb, "profile:read")
	skillRead := platformauth.RequirePermission(tdb, "skill:read")
	skillWrite := platformauth.RequirePermission(tdb, "skill:write")
	placementRead := platformauth.RequirePermission(tdb, "placement:read")
	placementWrite := platformauth.RequirePermission(tdb, "placement:write")
	surveyRead := platformauth.RequirePermission(tdb, "survey:read")
	surveyWrite := platformauth.RequirePermission(tdb, "survey:write")
	surveyFreeText := platformauth.RequirePermission(tdb, "survey:read_freetext")

	// --- Skill master + skill map + certifications (talent/* group) ---
	talent := rg.Group("/talent")
	talent.Use(requireAuth)
	talent.POST("/skills", skillWrite, h.CreateSkill)
	talent.GET("/skills", skillRead, h.ListSkills)
	talent.GET("/skills/:skill_id/holders", skillRead, h.SearchSkillHolders)
	talent.GET("/skill-matrix", skillRead, h.SkillMatrix)
	talent.GET("/certifications/expiring", skillRead, h.ExpiringCertifications)
	talent.GET("/org-tree", profileRead, h.GetOrgTree)

	// Placement simulations.
	talent.POST("/placement-simulations", placementWrite, h.CreateSimulation)
	talent.POST("/placement-simulations/:sim_id/items", placementWrite, h.AddSimulationItem)
	talent.GET("/placement-simulations/:sim_id/items", placementRead, h.ListSimulationItems)
	talent.POST("/placement-simulations/:sim_id/apply", placementWrite, h.ApplySimulation)
	talent.POST("/placement-simulations/:sim_id/discard", placementWrite, h.DiscardSimulation)

	// Pulse surveys.
	talent.POST("/surveys", surveyWrite, h.CreateSurvey)
	talent.PATCH("/surveys/:survey_id/status", surveyWrite, h.SetSurveyStatus)
	talent.POST("/surveys/:survey_id/responses", surveyWrite, h.SubmitResponse)
	talent.GET("/surveys/:survey_id/aggregate", surveyRead, h.AggregateSurvey)
	talent.GET("/survey-responses/:response_id/free-text", surveyFreeText, h.ReadFreeText)

	// --- Employee-nested routes (employees/:id/*) ---
	emp := rg.Group("/employees/:id")
	emp.Use(requireAuth)
	emp.PUT("/skills", skillWrite, h.AssignSkill)
	emp.GET("/skills", skillRead, h.ListEmployeeSkills)
	emp.POST("/certifications", skillWrite, h.AddCertification)
	emp.GET("/certifications", skillRead, h.ListCertifications)
	emp.GET("/profile", profileRead, h.GetProfile)
}
