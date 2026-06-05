// cmd/retention runs the data-retention and disposal jobs.
//
// SINGLE-TENANT MODE (default):
//
//	go run ./cmd/retention \
//	    -tenant-id=<UUID> \
//	    -actor-id=<UUID>  \
//	    [-disposal-method=ciphertext_deleted|key_destroyed] \
//	    [-employee-grace-days=N] \
//	    [-job=mynumber_disposal|ledger_retention|employee_data_policy|document_expiry|all]
//
// ALL-TENANTS MODE:
//
//	go run ./cmd/retention \
//	    -all-tenants \
//	    -actor-id=<UUID>  \
//	    [-disposal-method=ciphertext_deleted|key_destroyed] \
//	    [-employee-grace-days=N] \
//	    [-job=all]
//
//	In -all-tenants mode the binary enumerates every active tenant from the
//	tenants table and runs the retention job sequentially for each one.
//	-tenant-id must NOT be set when -all-tenants is used.
//
//	The same -actor-id is used for all tenants.  This service account MUST
//	hold the required permissions (mynumber:reveal, ledger:write) within
//	every tenant that this job processes.  See docs/ops/retention-service-account.md
//	for provisioning instructions.
//
// All database environment variables (DB_HOST, DB_PORT, DB_USER, DB_PASSWORD,
// DB_NAME, DB_SSLMODE) are read from the environment in the same way as the
// main server binary.
//
// Required environment variables:
//
//	DB_PASSWORD   — application role password (hr_app)
//
// Required flags (single-tenant):
//
//	-tenant-id    — UUID of the tenant to process
//	-actor-id     — UUID of the system service-account user that holds
//	                mynumber:reveal and ledger:write for this tenant
//
// Required flags (all-tenants):
//
//	-all-tenants  — enumerate all active tenants from DB
//	-actor-id     — UUID of the service account (must have perms in ALL tenants)
//
// SECURITY NOTES:
//   - -actor-id must be a valid user ID in the users table with the required
//     permissions; it MUST NOT be a super-admin.  Provision a dedicated,
//     purpose-limited service account per tenant.
//   - This binary connects as the hr_app role (DML only, NOBYPASSRLS) so
//     RLS policies are enforced for every query.
//   - PII is never logged; only opaque UUIDs appear in log output.
//   - In -all-tenants mode, a per-tenant error does NOT abort the remaining
//     tenants; all errors are collected and the binary exits non-zero if any
//     tenant failed.
//
// Cron scaffold:
//   - Invoke this binary from cron / Kubernetes CronJob / pg_cron.
//   - Use -all-tenants for a single cron entry that covers all tenants
//     automatically (recommended for production).
//   - A database-backed job queue (e.g. river, machinery) is a recommended
//     P3 upgrade for production workloads.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/mynumber"
	"github.com/your-org/hr-saas/internal/offer/econtract"
	"github.com/your-org/hr-saas/internal/platform/config"
	"github.com/your-org/hr-saas/internal/platform/db"
	"github.com/your-org/hr-saas/internal/platform/logging"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/retention"
)

func main() {
	tenantIDStr := flag.String("tenant-id", "", "tenant UUID; mutually exclusive with -all-tenants")
	allTenants := flag.Bool("all-tenants", false, "enumerate all active tenants from DB and run for each")
	actorIDStr := flag.String("actor-id", "", "service-account user UUID with required permissions (required)")
	disposalMethod := flag.String("disposal-method", mynumber.MethodCiphertextDeleted,
		"mynumber disposal method: ciphertext_deleted | key_destroyed")
	graceDays := flag.Int("employee-grace-days", 0,
		"employee data grace period in days after contract end (0 = disabled)")
	job := flag.String("job", "all",
		"which job to run: all | mynumber_disposal | ledger_retention | employee_data_policy | document_expiry | econtract_retention")
	flag.Parse()

	logger := logging.New("production")
	slog.SetDefault(logger)

	// Validate mutually-exclusive tenant flags.
	if *allTenants && *tenantIDStr != "" {
		logger.Error("flags -all-tenants and -tenant-id are mutually exclusive")
		os.Exit(1)
	}
	if !*allTenants && *tenantIDStr == "" {
		logger.Error("one of -tenant-id or -all-tenants is required")
		os.Exit(1)
	}

	// actor-id is always required.
	if *actorIDStr == "" {
		logger.Error("flag -actor-id is required")
		os.Exit(1)
	}
	actorID, err := uuid.Parse(*actorIDStr)
	if err != nil {
		logger.Error("invalid -actor-id", "error", err)
		os.Exit(1)
	}

	// Validate disposal method.
	if *disposalMethod != mynumber.MethodCiphertextDeleted &&
		*disposalMethod != mynumber.MethodKeyDestroyed {
		logger.Error("invalid -disposal-method; use ciphertext_deleted or key_destroyed")
		os.Exit(1)
	}

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config load failed", "error", err)
		os.Exit(1)
	}

	// Allow more time in all-tenants mode since the sweep covers every tenant.
	timeout := 10 * time.Minute
	if *allTenants {
		timeout = 60 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	database, err := db.Open(ctx, cfg.DSN(), logger)
	if err != nil {
		logger.Error("database open failed", "error", err)
		os.Exit(1)
	}
	sqlDB, err := database.DB()
	if err != nil {
		logger.Error("get sql.DB failed", "error", err)
		os.Exit(1)
	}
	defer sqlDB.Close()

	tdb := tenantdb.New(database)
	mynumberSvc := mynumber.NewService(tdb)

	retentionCfg := retention.Config{
		DisposalMethod:               *disposalMethod,
		LedgerRetentionFallbackYears: 0,
		EmployeeDataGracePeriod:      time.Duration(*graceDays) * 24 * time.Hour,
	}

	runner := retention.NewRunner(tdb, mynumberSvc, retentionCfg, logger, actorID)

	// econtract retention runner — uses the same tdb / actorID as the main runner.
	// BatchLimit defaults to 500; override via ECONTRACT_RETENTION_BATCH_LIMIT if needed.
	econtractRunner := econtract.NewRetentionRunner(tdb, econtract.DefaultRetentionConfig(), logger, actorID)

	if *allTenants {
		runAllTenants(ctx, database, runner, econtractRunner, logger, *job)
		return
	}

	// Single-tenant mode.
	tenantID, err := uuid.Parse(*tenantIDStr)
	if err != nil {
		logger.Error("invalid -tenant-id", "error", err)
		os.Exit(1)
	}

	logger.Info("retention job starting",
		"tenant_id", tenantID,
		"actor_id", actorID,
		"job", *job,
	)

	runErr := runJobForTenant(ctx, runner, econtractRunner, tenantID, *job, logger)
	if runErr != nil {
		logger.Error("retention job finished with error", "error", runErr)
		os.Exit(1)
	}
	logger.Info("retention job completed successfully")
}

// runAllTenants enumerates every active tenant and runs the retention job for
// each one sequentially.  Per-tenant errors are logged and counted; remaining
// tenants continue processing.  The binary exits non-zero if any tenant failed.
//
// SECURITY: the tenants table query uses the hr_app role (NOBYPASSRLS).
// RLS on the tenants table filters by app.tenant_id; because we need to
// enumerate ALL tenants here we query outside a WithinTenant transaction.
// The hr_app role has SELECT on tenants (granted in migration 00001), and the
// tenants RLS policy allows reading the tenant's own row when app.tenant_id
// matches.  For cross-tenant enumeration we use a direct query on the raw
// *gorm.DB connection with no SET LOCAL, which bypasses RLS.
//
// NOTE: this is intentional and documented: cross-tenant enumeration for the
// system-level cron is a privileged operation.  The binary is run by the
// hr_admin (superuser) or a dedicated service account with BYPASSRLS on
// tenants only.  Never expose this path through the HTTP API.
func runAllTenants(
	ctx context.Context,
	database *gorm.DB,
	runner *retention.Runner,
	econtractRunner *econtract.RetentionRunner,
	logger *slog.Logger,
	job string,
) {
	// Fetch IDs of all active tenants.
	// Only the opaque tenant ID is selected — no tenant name / PII.
	type tenantRow struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	var tenants []tenantRow
	if err := database.WithContext(ctx).
		Raw(`SELECT id FROM tenants WHERE status = 'active' ORDER BY id`).
		Scan(&tenants).Error; err != nil {
		logger.Error("retention: failed to enumerate tenants", "error", err)
		os.Exit(1)
	}

	logger.Info("retention all-tenants sweep starting",
		"tenant_count", len(tenants),
		"job", job,
	)

	var (
		successCount int
		errCount     int
		firstErr     error
	)

	for _, t := range tenants {
		logger.Info("retention: processing tenant", "tenant_id", t.ID)
		if err := runJobForTenant(ctx, runner, econtractRunner, t.ID, job, logger); err != nil {
			logger.Error("retention: tenant failed",
				"tenant_id", t.ID, "error", err)
			errCount++
			if firstErr == nil {
				firstErr = fmt.Errorf("tenant %s: %w", t.ID, err)
			}
			continue
		}
		successCount++
	}

	logger.Info("retention all-tenants sweep finished",
		"succeeded", successCount,
		"failed", errCount,
	)

	if errCount > 0 {
		logger.Error("retention: all-tenants sweep completed with failures",
			"first_error", firstErr)
		os.Exit(1)
	}
	logger.Info("retention all-tenants sweep completed successfully")
}

// runJobForTenant dispatches the correct sub-job (or all jobs) for one tenant.
func runJobForTenant(
	ctx context.Context,
	runner *retention.Runner,
	econtractRunner *econtract.RetentionRunner,
	tenantID uuid.UUID,
	job string,
	logger *slog.Logger,
) error {
	switch job {
	case "all":
		var firstErr error
		record := func(err error) {
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		record(runner.Run(ctx, tenantID))
		record(econtractRunner.Run(ctx, tenantID))
		return firstErr
	case retention.JobMyNumberDisposal:
		return runner.RunMyNumberDisposal(ctx, tenantID)
	case retention.JobLedgerRetention:
		return runner.RunLedgerRetention(ctx, tenantID)
	case retention.JobEmployeeDataPolicy:
		return runner.RunEmployeeDataPolicy(ctx, tenantID)
	case retention.JobDocumentExpiry:
		return runner.RunDocumentExpiry(ctx, tenantID)
	case econtract.JobEContractRetention:
		return econtractRunner.Run(ctx, tenantID)
	default:
		logger.Error("unknown -job value; valid: all | mynumber_disposal | ledger_retention | employee_data_policy | document_expiry | econtract_retention")
		os.Exit(1)
		return nil // unreachable
	}
}
