// cmd/sso-cleanup deletes expired SSO flow state rows from the sso_state table.
//
// SSO flow state entries have a short TTL (10 minutes) and accumulate when
// users abandon the login flow before completing it.  This command is intended
// to run periodically (e.g. hourly via cron or Kubernetes CronJob) to keep
// the table from growing unbounded.
//
// Usage:
//
//	go run ./cmd/sso-cleanup
//
// # Required environment variable
//
//	SYSTEM_DATABASE_URL  — PostgreSQL DSN for the hr_system role (BYPASSRLS).
//	                        Cross-tenant DELETE on sso_state requires BYPASSRLS;
//	                        the hr_app role (NOBYPASSRLS) yields 0 rows due to
//	                        FORCE RLS without an active app.tenant_id session variable.
//
// # Optional fallback
//
// When SYSTEM_DATABASE_URL is not set the binary logs a warning and exits 0
// (non-fatal) so that a missing variable in a mixed-environment deployment does
// not cause alert noise.  The table will grow until the variable is provisioned
// and the job is re-run.
//
// SECURITY NOTES:
//   - This binary MUST connect as the hr_system role (BYPASSRLS) for the
//     cross-tenant DELETE to actually remove rows.  An hr_app connection is
//     silently a no-op due to FORCE RLS.
//   - Only rows where expires_at < now() are deleted. Active (non-expired)
//     rows are never touched regardless of tenant.
//   - PII is never logged; only a deleted-row count appears in log output.
//   - The operation is idempotent; running it more frequently than necessary
//     is safe — it will just report 0 rows deleted.
//
// Cron scaffold:
//   - See infra/jobs/sso-cleanup-cronjob.yaml for a Kubernetes CronJob example.
//   - A simple crontab entry running hourly is sufficient for most deployments:
//       0 * * * * /usr/local/bin/sso-cleanup
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/your-org/hr-saas/internal/auth/sso"
	"github.com/your-org/hr-saas/internal/platform/db"
	"github.com/your-org/hr-saas/internal/platform/logging"
)

func main() {
	logger := logging.New("production")
	slog.SetDefault(logger)

	// Cross-tenant DELETE on sso_state requires BYPASSRLS (hr_system role).
	// The hr_app role (NOBYPASSRLS) is subject to FORCE RLS on sso_state; without
	// an active app.tenant_id session variable the DELETE matches 0 rows, making
	// the cleanup silently a no-op.
	systemDSN := os.Getenv("SYSTEM_DATABASE_URL")
	if systemDSN == "" {
		logger.Warn("sso-cleanup: SYSTEM_DATABASE_URL is not set; " +
			"cross-tenant DELETE requires hr_system (BYPASSRLS) — skipping cleanup. " +
			"Provision SYSTEM_DATABASE_URL to enable sso_state expiry sweeps.")
		// Exit 0 (non-fatal): missing variable should not cause alert noise,
		// but the table will grow until the variable is provisioned.
		os.Exit(0)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Open a connection using the hr_system (BYPASSRLS) DSN so that the DELETE
	// is not restricted by FORCE RLS and spans all tenants in one pass.
	database, err := db.Open(ctx, systemDSN, logger)
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

	logger.Info("sso-cleanup: deleting expired sso_state rows (BYPASSRLS connection)")

	deleted, err := sso.CleanupExpiredStates(ctx, database)
	if err != nil {
		logger.Error("sso-cleanup: failed", "error", err)
		os.Exit(1)
	}

	logger.Info("sso-cleanup: completed", "deleted_rows", deleted)
}
