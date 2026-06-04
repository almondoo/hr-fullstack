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
// All database environment variables (DB_HOST, DB_PORT, DB_USER, DB_PASSWORD,
// DB_NAME, DB_SSLMODE) are read from the environment in the same way as the
// main server binary.
//
// Required environment variable:
//
//	DB_PASSWORD  — application role password (hr_app)
//
// SECURITY NOTES:
//   - This binary connects as the hr_app role (DML only, NOBYPASSRLS) so
//     RLS policies are enforced for every query.
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
	"github.com/your-org/hr-saas/internal/platform/config"
	"github.com/your-org/hr-saas/internal/platform/db"
	"github.com/your-org/hr-saas/internal/platform/logging"
)

func main() {
	logger := logging.New("production")
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config load failed", "error", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
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

	logger.Info("sso-cleanup: deleting expired sso_state rows")

	deleted, err := sso.CleanupExpiredStates(ctx, database)
	if err != nil {
		logger.Error("sso-cleanup: failed", "error", err)
		os.Exit(1)
	}

	logger.Info("sso-cleanup: completed", "deleted_rows", deleted)
}
