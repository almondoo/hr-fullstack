// Package testdb provides a test harness that spins up a real PostgreSQL 17
// container via testcontainers-go, runs the embedded goose migrations, creates
// the hr_app role, and returns GORM connections for both the admin and
// application roles.
//
// Tests that use this package require Docker to be running.  They are guarded
// with t.Skip when testing.Short() is true, so `-short` runs skip them.
//
// Typical usage:
//
//	func TestFoo(t *testing.T) {
//	    h := testdb.NewHarness(t)
//	    // h.AdminDB — admin *gorm.DB (DDL / seeding outside tenant context)
//	    // h.AppDB   — hr_app *gorm.DB (all tenant-scoped business queries)
//	    tdb := tenantdb.New(h.AppDB)
//	    ...
//	}
package testdb

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/pressly/goose/v3"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	migrations "github.com/your-org/hr-saas/db"
)

const (
	testDBName    = "hr_saas_test"
	testAdminUser = "postgres"
	testAdminPass = "postgres_test_admin"
	// hr_app gets a synthetic password that never appears in production config.
	testAppUser = "hr_app"
	testAppPass = "hr_app_test_only_not_production"
)

// Harness holds the live database connections for an integration test.
type Harness struct {
	// AdminDB is connected as the admin role (postgres).
	// Use it for setup/teardown tasks that require DDL privileges.
	AdminDB *gorm.DB

	// AppDB is connected as hr_app (NOBYPASSRLS).
	// All tenant-scoped business queries must go through this connection.
	AppDB *gorm.DB

	t         *testing.T
	container *tcpostgres.PostgresContainer
	adminSQL  *sql.DB
	appSQL    *sql.DB
}

// NewHarness starts a postgres:17-alpine container, runs migrations, creates
// hr_app, and returns a fully initialised Harness.
// The container and both connections are automatically cleaned up when the
// test completes via t.Cleanup.
func NewHarness(t *testing.T) *Harness {
	t.Helper()

	if testing.Short() {
		t.Skip("testdb: skipping integration test (requires Docker; use -short to confirm)")
	}

	ctx := context.Background()

	container, err := tcpostgres.Run(ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase(testDBName),
		tcpostgres.WithUsername(testAdminUser),
		tcpostgres.WithPassword(testAdminPass),
		tcpostgres.WithSQLDriver("pgx"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("testdb: start postgres container: %v", err)
	}

	adminDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(ctx)
		t.Fatalf("testdb: get connection string: %v", err)
	}

	// --- Run migrations as admin ---
	adminSQLDB, err := sql.Open("pgx", adminDSN)
	if err != nil {
		_ = container.Terminate(ctx)
		t.Fatalf("testdb: open admin sql.DB: %v", err)
	}

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		_ = adminSQLDB.Close()
		_ = container.Terminate(ctx)
		t.Fatalf("testdb: goose set dialect: %v", err)
	}

	// Migrations include GRANT ... TO hr_app, so hr_app must exist first.
	if err := createAppRole(ctx, adminSQLDB); err != nil {
		_ = adminSQLDB.Close()
		_ = container.Terminate(ctx)
		t.Fatalf("testdb: create hr_app role: %v", err)
	}

	if err := goose.Up(adminSQLDB, "migrations"); err != nil {
		_ = adminSQLDB.Close()
		_ = container.Terminate(ctx)
		t.Fatalf("testdb: goose up: %v", err)
	}

	// --- Open GORM connections ---
	silentLogger := gormlogger.Default.LogMode(gormlogger.Silent)
	if os.Getenv("TEST_DB_DEBUG") != "" {
		silentLogger = gormlogger.Default.LogMode(gormlogger.Info)
	}

	adminGORM, err := gorm.Open(postgres.Open(adminDSN), &gorm.Config{
		Logger:               silentLogger,
		SkipDefaultTransaction: true,
	})
	if err != nil {
		_ = adminSQLDB.Close()
		_ = container.Terminate(ctx)
		t.Fatalf("testdb: open admin GORM: %v", err)
	}

	// Build an hr_app DSN by swapping user/password in the admin DSN.
	// testcontainers returns a host:port DSN; we reconstruct it cleanly.
	host, err := container.Host(ctx)
	if err != nil {
		_ = adminSQLDB.Close()
		_ = container.Terminate(ctx)
		t.Fatalf("testdb: get container host: %v", err)
	}
	mappedPort, err := container.MappedPort(ctx, "5432")
	if err != nil {
		_ = adminSQLDB.Close()
		_ = container.Terminate(ctx)
		t.Fatalf("testdb: get mapped port: %v", err)
	}
	appDSN := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		host, mappedPort.Port(), testAppUser, testAppPass, testDBName,
	)

	appSQLDB, err := sql.Open("pgx", appDSN)
	if err != nil {
		_ = adminSQLDB.Close()
		_ = container.Terminate(ctx)
		t.Fatalf("testdb: open app sql.DB: %v", err)
	}

	appGORM, err := gorm.Open(postgres.Open(appDSN), &gorm.Config{
		Logger:               silentLogger,
		SkipDefaultTransaction: true,
	})
	if err != nil {
		_ = appSQLDB.Close()
		_ = adminSQLDB.Close()
		_ = container.Terminate(ctx)
		t.Fatalf("testdb: open app GORM: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	_ = logger // available for future use

	h := &Harness{
		AdminDB:   adminGORM,
		AppDB:     appGORM,
		t:         t,
		container: container,
		adminSQL:  adminSQLDB,
		appSQL:    appSQLDB,
	}

	t.Cleanup(h.cleanup)

	return h
}

// TruncateTables removes all rows from the listed tables in a single
// transaction (admin role, bypasses RLS).  Call between sub-tests to
// reset state without restarting the container.
func (h *Harness) TruncateTables(tables ...string) {
	h.t.Helper()
	tx := h.AdminDB.Begin()
	if tx.Error != nil {
		h.t.Fatalf("testdb: truncate begin: %v", tx.Error)
	}
	for _, tbl := range tables {
		// Table names come from test code; they are not user-controlled
		// input, but we keep the pattern safe regardless.
		if err := tx.Exec("TRUNCATE TABLE " + tbl + " CASCADE").Error; err != nil {
			_ = tx.Rollback()
			h.t.Fatalf("testdb: truncate %s: %v", tbl, err)
		}
	}
	if err := tx.Commit().Error; err != nil {
		h.t.Fatalf("testdb: truncate commit: %v", err)
	}
}

// cleanup tears down the container and closes connections.
func (h *Harness) cleanup() {
	ctx := context.Background()
	_ = h.appSQL.Close()
	_ = h.adminSQL.Close()
	_ = h.container.Terminate(ctx)
}

// quoteLiteral returns s wrapped in single quotes with any single-quote and
// backslash characters escaped, producing a safe SQL literal string.
// This is equivalent to libpq's PQescapeLiteral / pq.QuoteLiteral.
//
// NOTE: this helper is intentionally unexported and used only inside
// createAppRole, which itself only accepts the fixed internal constant
// testAppPass.  It must never be called with user-supplied or
// externally-sourced values.
func quoteLiteral(s string) string {
	// Escape backslashes first (before escaping quotes so that backslashes
	// introduced by the quote escape are not double-escaped).
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `''`)
	return "'" + s + "'"
}

// createAppRole creates the hr_app role with NOBYPASSRLS inside the test
// container.  The role is given a synthetic password (testAppPass) that is a
// fixed compile-time constant and is never used outside of tests — it never
// appears in any production configuration.
//
// NOTE: CREATE ROLE does not support query-parameter placeholders for the
// PASSWORD clause, so we use quoteLiteral to safely embed the value.  This
// function is internal-only and does NOT accept external input; calling it
// with runtime or user-supplied values is a violation of this contract.
//
// The creation is idempotent: if hr_app already exists the DO block is a no-op.
func createAppRole(ctx context.Context, db *sql.DB) error {
	// Use quoteLiteral to safely embed the constant password into the DDL.
	// CREATE ROLE PASSWORD does not accept bind parameters, so we escape the
	// literal value.  testAppPass is a fixed synthetic constant defined in
	// this file; it is not derived from external input.
	stmt := `
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT FROM pg_catalog.pg_roles WHERE rolname = 'hr_app'
    ) THEN
        CREATE ROLE hr_app
            LOGIN
            PASSWORD ` + quoteLiteral(testAppPass) + `
            NOSUPERUSER
            NOBYPASSRLS
            NOCREATEDB
            NOCREATEROLE;
    END IF;
END
$$;`
	_, err := db.ExecContext(ctx, stmt)
	if err != nil {
		return fmt.Errorf("create hr_app role: %w", err)
	}
	return nil
}
