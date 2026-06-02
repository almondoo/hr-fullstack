// Package db provides GORM database connection management.
//
// Design decisions:
//   - SkipDefaultTransaction: true — GORM wraps every write in an implicit
//     transaction by default. We disable this because the RLS pattern
//     (docs/04_tech_stack.md §6) requires explicit transactions with
//     SET LOCAL app.tenant_id, making the implicit transaction wrapper
//     counter-productive.
//   - Connection pool is sized conservatively; tune via DB_POOL_* env vars
//     in a later slice when load profiles are known.
//   - Retry/backoff mirrors the original connectDB logic in main.go to handle
//     the race between the api container and the db container in Docker Compose.
package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

const (
	maxAttempts     = 10
	retryDelay      = 2 * time.Second
	pingTimeout     = 2 * time.Second
	maxOpenConns    = 10
	maxIdleConns    = 5
	connMaxLifetime = time.Hour
)

// Open establishes a GORM connection to PostgreSQL using the provided DSN.
// It retries up to maxAttempts times with a fixed delay to accommodate
// container startup ordering (db may not be ready when api starts).
//
// ctx is honoured throughout: if it is cancelled or its deadline expires the
// function returns immediately with ctx.Err().  This prevents Open from
// blocking past a graceful-shutdown window.
//
// The caller is responsible for closing the underlying *sql.DB:
//
//	sqlDB, _ := db.DB()
//	defer sqlDB.Close()
func Open(ctx context.Context, dsn string, logger *slog.Logger) (*gorm.DB, error) {
	if dsn == "" {
		return nil, errors.New("db: DSN is empty")
	}

	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Respect cancellation before each attempt.
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("db: open cancelled: %w", err)
		}

		gormDB, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
			// Warn-level GORM internal logging; detailed SQL is debug-only
			// to avoid leaking query content (which may include tenant IDs).
			Logger: gormlogger.Default.LogMode(gormlogger.Warn),

			// Disable the implicit transaction wrapper — all writes must use
			// explicit db.Transaction(...) with SET LOCAL app.tenant_id for RLS.
			SkipDefaultTransaction: true,
		})
		if err != nil {
			lastErr = err
			logger.Warn("waiting for database: open failed", "attempt", attempt, "error", err)
			if !sleepOrCancel(ctx, retryDelay) {
				return nil, fmt.Errorf("db: open cancelled: %w", ctx.Err())
			}
			continue
		}

		sqlDB, derr := gormDB.DB()
		if derr != nil {
			lastErr = fmt.Errorf("get sql.DB: %w", derr)
			logger.Warn("waiting for database: get sql.DB failed", "attempt", attempt, "error", lastErr)
			if !sleepOrCancel(ctx, retryDelay) {
				return nil, fmt.Errorf("db: open cancelled: %w", ctx.Err())
			}
			continue
		}

		sqlDB.SetMaxOpenConns(maxOpenConns)
		sqlDB.SetMaxIdleConns(maxIdleConns)
		sqlDB.SetConnMaxLifetime(connMaxLifetime)

		pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
		pingErr := sqlDB.PingContext(pingCtx)
		cancel()

		if pingErr == nil {
			logger.Info("database connected")
			return gormDB, nil
		}

		// Ping failed — close the underlying pool to prevent a connection leak
		// before moving on to the next retry attempt.
		_ = sqlDB.Close()

		lastErr = fmt.Errorf("ping: %w", pingErr)
		logger.Warn("waiting for database: ping failed", "attempt", attempt, "error", lastErr)
		if !sleepOrCancel(ctx, retryDelay) {
			return nil, fmt.Errorf("db: open cancelled: %w", ctx.Err())
		}
	}

	return nil, fmt.Errorf("db: could not connect after %d attempts: %w", maxAttempts, lastErr)
}

// sleepOrCancel waits for d or until ctx is done.
// Returns true if the sleep completed normally, false if ctx was cancelled.
func sleepOrCancel(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// Ping checks database reachability with a bounded context.
// Used by the /readyz handler to report service readiness.
func Ping(ctx context.Context, db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("db: get sql.DB for ping: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	return sqlDB.PingContext(pingCtx)
}
