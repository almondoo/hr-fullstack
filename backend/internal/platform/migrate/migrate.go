// Package migrate provides the server-side migration runner used during
// startup when MIGRATE_ON_STARTUP=true.
//
// It opens a short-lived admin connection (AdminDSN), runs goose.Up, and
// closes the connection.  The long-lived application pool (hr_app) is opened
// separately in main.go after migrations complete.
package migrate

import (
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	migrations "github.com/your-org/hr-saas/db"
)

// Up runs all pending goose migrations against the given admin DSN.
// It uses the embedded migration FS (db/migrations/*.sql) so no external
// files are needed at runtime.
func Up(adminDSN string) error {
	db, err := sql.Open("pgx", adminDSN)
	if err != nil {
		return fmt.Errorf("migrate: open admin connection: %w", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return fmt.Errorf("migrate: ping admin connection: %w", err)
	}

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("migrate: set dialect: %w", err)
	}

	if err := goose.Up(db, "migrations"); err != nil {
		return fmt.Errorf("migrate: goose up: %w", err)
	}

	return nil
}
