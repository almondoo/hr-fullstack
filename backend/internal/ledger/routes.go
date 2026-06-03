package ledger

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires the statutory three-ledger (法定三帳簿) endpoints.
// The signature is uniform across all stories (central wiring depends on it).
//
// The payroll-SaaS adapter is constructed here as a mock (adapter abstraction;
// real providers are P3) — no extra constructor argument is added.
//
// Endpoints:
//
//	GET    /ledgers/settings                              — ledger:read
//	PUT    /ledgers/settings                              — ledger:write
//	POST   /ledgers/finalise                              — ledger:finalise
//	GET    /ledgers/wage/export                           — ledger:read
//	POST   /employees/:id/ledgers/payroll-import          — ledger:write
//	POST   /employees/:id/ledgers/roster                  — ledger:write
//	GET    /employees/:id/ledgers/roster                  — ledger:read
//	POST   /employees/:id/ledgers/attendance-book         — ledger:write
//	GET    /employees/:id/ledgers/attendance-book         — ledger:read
//	POST   /employees/:id/ledgers/wage                    — ledger:write
//	GET    /employees/:id/ledgers/wage                    — ledger:read
//	GET    /employees/:id/ledgers/retention               — ledger:read
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) {
	svc := NewService(tdb)
	// Payroll-SaaS adapter: MVP mock implementation (real integration is P3).
	importer := NewMockPayrollImporter(ProviderMock)
	h := NewHandler(svc, importer)

	read := platformauth.RequirePermission(tdb, "ledger:read")
	write := platformauth.RequirePermission(tdb, "ledger:write")
	finalise := platformauth.RequirePermission(tdb, "ledger:finalize") //nolint:misspell // ledger:finalize and /finalize are established permission/route contracts

	// --- Tenant-level ledger routes ---
	ledgers := rg.Group("/ledgers")
	ledgers.Use(requireAuth)
	ledgers.GET("/settings", read, h.GetSettings)
	ledgers.PUT("/settings", write, h.UpsertSettings)
	ledgers.POST("/finalize", finalise, h.Finalise) //nolint:misspell // ledger:finalize and /finalize are established permission/route contracts
	ledgers.GET("/wage/export", read, h.ExportWageLedgerCSV)

	// --- Per-employee ledger routes ---
	empLedgers := rg.Group("/employees/:id/ledgers")
	empLedgers.Use(requireAuth)
	empLedgers.POST("/payroll-import", write, h.ImportPayroll)
	empLedgers.POST("/roster", write, h.BuildWorkerRoster)
	empLedgers.GET("/roster", read, h.GetWorkerRoster)
	empLedgers.POST("/attendance-book", write, h.BuildAttendanceBook)
	empLedgers.GET("/attendance-book", read, h.GetAttendanceBook)
	empLedgers.POST("/wage", write, h.BuildWageLedger)
	empLedgers.GET("/wage", read, h.GetWageLedger)
	empLedgers.GET("/retention", read, h.EvaluateRetention)
}
