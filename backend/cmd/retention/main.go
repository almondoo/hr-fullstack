// cmd/retention runs the data-retention and disposal jobs for one tenant.
//
// Usage:
//
//	go run ./cmd/retention \
//	    -tenant-id=<UUID> \
//	    -actor-id=<UUID>  \
//	    [-disposal-method=ciphertext_deleted|key_destroyed] \
//	    [-job=mynumber_disposal|ledger_retention|employee_data_policy|document_expiry|all]
//
// All database environment variables (DB_HOST, DB_PORT, DB_USER, DB_PASSWORD,
// DB_NAME, DB_SSLMODE) are read from the environment in the same way as the
// main server binary.
//
// Required environment variables:
//
//	DB_PASSWORD   — application role password (hr_app)
//
// Required flags:
//
//	-tenant-id    — UUID of the tenant to process
//	-actor-id     — UUID of the system service-account user that holds
//	                mynumber:reveal and ledger:write for this tenant
//
// SECURITY NOTES:
//   - -actor-id must be a valid user ID in the users table with the required
//     permissions; it MUST NOT be a super-admin.  Provision a dedicated,
//     purpose-limited service account per tenant.
//   - This binary connects as the hr_app role (DML only, NOBYPASSRLS) so
//     RLS policies are enforced for every query.
//   - PII is never logged; only opaque UUIDs appear in log output.
//
// Cron scaffold:
//   - Invoke this binary from cron / Kubernetes CronJob / pg_cron.
//   - For multi-tenant SaaS, loop over tenant IDs and invoke once per tenant.
//   - A database-backed job queue (e.g. river, machinery) is a recommended
//     P3 upgrade for production workloads.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/your-org/hr-saas/internal/mynumber"
	"github.com/your-org/hr-saas/internal/platform/config"
	"github.com/your-org/hr-saas/internal/platform/db"
	"github.com/your-org/hr-saas/internal/platform/logging"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/retention"
)

func main() {
	tenantIDStr := flag.String("tenant-id", "", "tenant UUID (required)")
	actorIDStr := flag.String("actor-id", "", "service-account user UUID with required permissions (required)")
	disposalMethod := flag.String("disposal-method", mynumber.MethodCiphertextDeleted,
		"mynumber disposal method: ciphertext_deleted | key_destroyed")
	graceDays := flag.Int("employee-grace-days", 0,
		"employee data grace period in days after contract end (0 = disabled)")
	job := flag.String("job", "all",
		"which job to run: all | mynumber_disposal | ledger_retention | employee_data_policy | document_expiry")
	flag.Parse()

	logger := logging.New("production")
	slog.SetDefault(logger)

	// Validate required flags.
	if *tenantIDStr == "" || *actorIDStr == "" {
		logger.Error("flags -tenant-id and -actor-id are required")
		os.Exit(1)
	}
	tenantID, err := uuid.Parse(*tenantIDStr)
	if err != nil {
		logger.Error("invalid -tenant-id", "error", err)
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
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

	logger.Info("retention job starting",
		"tenant_id", tenantID,
		"actor_id", actorID,
		"job", *job,
	)

	var runErr error
	switch *job {
	case "all":
		runErr = runner.Run(ctx, tenantID)
	case retention.JobMyNumberDisposal:
		runErr = runner.RunMyNumberDisposal(ctx, tenantID)
	case retention.JobLedgerRetention:
		runErr = runner.RunLedgerRetention(ctx, tenantID)
	case retention.JobEmployeeDataPolicy:
		runErr = runner.RunEmployeeDataPolicy(ctx, tenantID)
	case retention.JobDocumentExpiry:
		runErr = runner.RunDocumentExpiry(ctx, tenantID)
	default:
		logger.Error("unknown -job value; valid: all | mynumber_disposal | ledger_retention | employee_data_policy | document_expiry")
		os.Exit(1)
	}

	if runErr != nil {
		logger.Error("retention job finished with error", "error", runErr)
		os.Exit(1)
	}
	logger.Info("retention job completed successfully")
}
