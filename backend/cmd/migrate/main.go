// cmd/migrate runs goose database migrations against the admin DSN.
//
// Usage:
//
//	go run ./cmd/migrate [up|down|status|version]
//
// The command connects with admin credentials (DB_ADMIN_USER /
// DB_ADMIN_PASSWORD) so that it can execute DDL and GRANT statements.
// The hr_app role only has DML privileges and cannot run migrations.
//
// Environment variables (all have sensible defaults for local development):
//
//	DB_HOST, DB_PORT, DB_NAME, DB_SSLMODE  — same as the main server
//	DB_ADMIN_USER     — admin role (defaults to DB_USER if unset)
//	DB_ADMIN_PASSWORD — admin password (defaults to DB_PASSWORD if unset)
package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib" // pgx driver for database/sql
	"github.com/pressly/goose/v3"

	migrations "github.com/your-org/hr-saas/db"
	"github.com/your-org/hr-saas/internal/platform/config"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config load failed", "error", err)
		os.Exit(1)
	}

	command := "up"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	dsn := cfg.AdminDSN()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		logger.Error("open admin connection failed", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		logger.Error("admin ping failed", "error", err)
		os.Exit(1)
	}

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())

	if err := goose.SetDialect("postgres"); err != nil {
		logger.Error("goose set dialect failed", "error", err)
		os.Exit(1)
	}

	switch command {
	case "up":
		err = goose.Up(db, "migrations")
	case "down":
		err = goose.Down(db, "migrations")
	case "status":
		err = goose.Status(db, "migrations")
	case "version":
		err = goose.Version(db, "migrations")
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q; valid: up, down, status, version\n", command)
		os.Exit(1)
	}

	if err != nil {
		logger.Error("migration failed", "command", command, "error", err)
		os.Exit(1)
	}

	logger.Info("migration completed", "command", command)
}
