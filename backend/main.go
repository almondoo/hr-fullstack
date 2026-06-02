// HR SaaS backend – entry point.
//
// Responsibilities of main.go (wire-up only):
//  1. Load typed Config from environment.
//  2. Initialise structured logger.
//  3. Open database connection.
//  4. Build the Gin router.
//  5. Start http.Server with ReadHeaderTimeout (Slowloris mitigation).
//  6. Graceful shutdown on SIGINT / SIGTERM.
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

	"github.com/your-org/hr-saas/internal/platform/config"
	"github.com/your-org/hr-saas/internal/platform/db"
	"github.com/your-org/hr-saas/internal/platform/logging"
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

	// --- 3. Database ---
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

	// --- 4. Router ---
	router := server.New(cfg, database, logger)

	// --- 5. HTTP Server ---
	srv := &http.Server{
		Addr:              ":" + cfg.HTTPPort,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second, // Slowloris mitigation
	}

	go func() {
		logger.Info("server starting", "port", cfg.HTTPPort, "env", cfg.AppEnv)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// --- 6. Graceful shutdown ---
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
