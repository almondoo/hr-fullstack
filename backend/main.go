// HR SaaS backend – entry point.
//
// Responsibilities of main.go (wire-up only):
//  1. Load typed Config from environment.
//  2. Initialise structured logger.
//  3. Run goose migrations (admin DSN, if MIGRATE_ON_STARTUP=true).
//  4. Open application database pool (hr_app DSN).
//  5. Build the Gin router.
//  6. Start http.Server with ReadHeaderTimeout (Slowloris mitigation).
//  7. Graceful shutdown on SIGINT / SIGTERM.
//
// Domain logic, middleware, and route registration live in internal/.
// See docs/04_tech_stack.md for architecture decisions.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/config"
	"github.com/your-org/hr-saas/internal/platform/db"
	"github.com/your-org/hr-saas/internal/platform/logging"
	"github.com/your-org/hr-saas/internal/platform/migrate"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/server"
)

func main() {
	// --- 1. Config ---
	cfg, err := config.Load()
	if err != nil {
		// Logger not yet available; write to stderr and exit immediately.
		// Error is a configuration problem, not a secret — safe to print.
		os.Stderr.WriteString("fatal: " + err.Error() + "\n")
		os.Exit(1)
	}

	// --- 2. Logger ---
	logger := logging.New(cfg.AppEnv)
	// Promote to package-level default so third-party code that calls
	// slog.Info / slog.Error directly inherits this configuration.
	slog.SetDefault(logger)

	// --- 3. Migrations (optional, controlled by MIGRATE_ON_STARTUP) ---
	// Runs before the application pool is opened so that hr_app can connect
	// to a fully-migrated schema.  The admin DSN is used (superuser role)
	// because DDL and GRANT statements require elevated privileges.
	//
	// MIGRATE_ON_STARTUP defaults to false (safe for production).  Set it to
	// true explicitly in environments where a single instance runs migrations
	// before traffic is accepted.  In multi-instance production deployments
	// prefer a dedicated pre-deploy migration step to avoid lock contention.
	if cfg.MigrateOnStartup {
		if !cfg.IsDevelopment() {
			logger.Warn("MIGRATE_ON_STARTUP=true in a non-development environment: " +
				"concurrent startup across multiple instances may cause migration lock contention; " +
				"prefer a dedicated pre-deploy migration step in production")
		}
		logger.Info("running database migrations")
		if err := migrate.Up(cfg.AdminDSN()); err != nil {
			logger.Error("migration failed", "error", err)
			os.Exit(1)
		}
		logger.Info("database migrations complete")
	}

	// --- 4. Database (application pool) ---
	// Use a background context with a generous timeout for the startup retry
	// loop. db.Open respects cancellation so a SIGINT during startup aborts
	// cleanly instead of looping until maxAttempts is exhausted.
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dbCancel()

	database, err := db.Open(dbCtx, cfg.DSN(), logger)
	if err != nil {
		logger.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	sqlDB, err := database.DB()
	if err != nil {
		logger.Error("failed to get sql.DB", "error", err)
		os.Exit(1)
	}
	defer sqlDB.Close()

	// --- 5. Router ---
	tdb := tenantdb.New(database)
	sessionStore := platformauth.NewSessionStore()
	deps := server.Deps{
		AppDB:        database,
		TenantDB:     tdb,
		SessionStore: sessionStore,
	}
	router := server.New(cfg, deps, logger)

	// --- 6. HTTP Server ---
	// Use server.Handler(router) to get the CSRF-wrapped handler.
	srv := &http.Server{
		Addr:              ":" + cfg.HTTPPort,
		Handler:           server.Handler(router),
		ReadHeaderTimeout: 5 * time.Second, // Slowloris mitigation
	}

	go func() {
		logger.Info("server starting", "port", cfg.HTTPPort, "env", cfg.AppEnv)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// --- 7. Graceful shutdown ---
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info("shutting down server")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("forced shutdown", "error", err)
	}
	logger.Info("server stopped")
}
