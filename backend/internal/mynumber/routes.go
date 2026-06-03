package mynumber

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires the mynumber endpoints.  The group should already have
// RequireAuth applied per-subgroup below.
//
// Permission model (strict separation of duties / least privilege):
//   - mynumber:write   — collect / dispose-management (write metadata).
//   - mynumber:read    — read record metadata and access logs (no number value).
//   - mynumber:reveal  — DECRYPT / DISPLAY / PROVIDE the 個人番号.  This is the
//     dedicated, strictly-controlled permission; ordinary HR read/write do NOT
//     grant it.  It is enforced here at the route layer AND re-validated in the
//     service layer (defence-in-depth).
//
// Endpoints:
//
//	POST   /mynumbers                          — mynumber:write   (収集・暗号化保管)
//	GET    /mynumbers/:id                      — mynumber:read    (メタデータ参照)
//	GET    /mynumbers/:id/access-logs          — mynumber:read    (利用提供ログ参照)
//	POST   /mynumbers/:id/reveal               — mynumber:reveal  (復号/表示)
//	POST   /mynumbers/:id/provide              — mynumber:reveal  (第三者提供)
//	POST   /mynumbers/:id/dispose              — mynumber:reveal  (廃棄/復号不能化)
//	GET    /employees/:id/mynumbers            — mynumber:read    (本人/扶養家族一覧)
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) {
	svc := NewService(tdb)
	h := NewHandler(svc)

	mnRead := platformauth.RequirePermission(tdb, "mynumber:read")
	mnWrite := platformauth.RequirePermission(tdb, "mynumber:write")
	mnReveal := platformauth.RequirePermission(tdb, "mynumber:reveal")

	// --- Record routes (by record id) ---
	records := rg.Group("/mynumbers")
	records.Use(requireAuth)
	records.POST("", mnWrite, h.Collect)
	records.GET("/:id", mnRead, h.GetRecord)
	records.GET("/:id/access-logs", mnRead, h.ListAccessLogs)
	// Sensitive operations — dedicated reveal permission required.
	records.POST("/:id/reveal", mnReveal, h.Reveal)
	records.POST("/:id/provide", mnReveal, h.Provide)
	records.POST("/:id/dispose", mnReveal, h.Dispose)

	// --- Per-employee listing (本人 + 扶養家族) ---
	empRecords := rg.Group("/employees/:id/mynumbers")
	empRecords.Use(requireAuth)
	empRecords.GET("", mnRead, h.ListRecords)
}
