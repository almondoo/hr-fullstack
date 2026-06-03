// Package testdb provides a test harness backed by a real PostgreSQL 17
// container via testcontainers-go.  The container is started exactly once per
// test-binary execution (i.e. once per package under `go test`).  Subsequent
// calls to NewHarness within the same binary reuse the shared container and
// connections, paying only the cost of a TRUNCATE between tests.
//
// Combined with `-p 1` (sequential package execution) this guarantees that at
// most one container exists at any moment, eliminating the Docker-overload
// problem caused by the previous per-call container model.
//
// Data isolation between tests is achieved by TRUNCATE … CASCADE executed by
// TruncateTables.  Each NewHarness call registers a t.Cleanup that truncates
// all application tables so the next test starts with an empty database.
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
	"sync"
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

// allAppTables lists every application table in dependency order (children
// before parents) so that TRUNCATE … CASCADE works in a single pass.
// The goose_db_version table is intentionally excluded.
var allAppTables = []string{
	"leave_usages",
	"leave_requests",
	"leave_grants",
	"leave_settings",
	"attendance_records",
	"payroll_items",
	"payroll_runs",
	"approval_steps",
	"approval_requests",
	"approval_routes",
	"offer_letters",
	"interview_feedbacks",
	"interview_schedules",
	"job_applications",
	"job_postings",
	"applicants",
	"selections",
	"hirings",
	"onboardings",
	"talent_profiles",
	"goal_progress",
	"goals",
	"evaluation_results",
	"evaluations",
	"one_on_ones",
	"notification_preferences",
	"notifications",
	"ledger_entries",
	"self_service_requests",
	"gov_filing_records",
	"work_rules",
	"billing_subscriptions",
	"billing_plans",
	"mynumber_records",
	"reporting_snapshots",
	"audit_logs",
	"sessions",
	"employment_contracts",
	"employee_assignments",
	"employees",
	"users",
	"roles",
	"departments",
	"tenants",
}

// sharedState holds the single container and connections that are shared across
// all NewHarness calls within one test binary.
type sharedState struct {
	adminGORM *gorm.DB
	appGORM   *gorm.DB
	adminSQL  *sql.DB
	appSQL    *sql.DB
}

var (
	once      sync.Once
	shared    *sharedState
	sharedErr error
)

// Harness holds the live database connections for an integration test.
// All Harness values within one test binary point at the same underlying
// container; isolation is enforced by TruncateTables at cleanup time.
type Harness struct {
	// AdminDB is connected as the admin role (postgres).
	// Use it for setup/teardown tasks that require DDL privileges.
	AdminDB *gorm.DB

	// AppDB is connected as hr_app (NOBYPASSRLS).
	// All tenant-scoped business queries must go through this connection.
	AppDB *gorm.DB

	t *testing.T
}

// NewHarness returns a Harness backed by the package-level shared container.
// The first call within a test binary starts the container and runs migrations;
// all subsequent calls reuse that container.
//
// A t.Cleanup is registered that truncates all application tables when the
// test completes, leaving the database empty for the next test.
func NewHarness(t *testing.T) *Harness {
	t.Helper()

	if testing.Short() {
		t.Skip("testdb: skipping integration test (requires Docker; use -short to confirm)")
	}

	once.Do(func() {
		shared, sharedErr = initShared()
	})
	if sharedErr != nil {
		t.Fatalf("testdb: shared container initialisation failed: %v", sharedErr)
	}

	h := &Harness{
		AdminDB: shared.adminGORM,
		AppDB:   shared.appGORM,
		t:       t,
	}

	// Truncate after each test so the next test starts with an empty DB.
	t.Cleanup(h.truncateAll)

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

// truncateAll is the t.Cleanup handler: truncates every application table.
//
// PostgreSQL aborts the entire transaction when any statement within it fails,
// including DDL statements that use IF EXISTS.  To avoid that, we issue each
// TRUNCATE as an independent autocommit statement so a missing table only
// silently skips that one statement rather than rolling back the whole batch.
func (h *Harness) truncateAll() {
	sqlDB, err := h.AdminDB.DB()
	if err != nil {
		h.t.Logf("testdb: cleanup truncate get sql.DB: %v", err)
		return
	}

	for _, tbl := range allAppTables {
		// Each statement is independent (autocommit).  A missing table produces
		// no error because of IF EXISTS; we discard the error anyway.
		_, _ = sqlDB.Exec("TRUNCATE TABLE IF EXISTS " + tbl + " CASCADE")
	}
}

// initShared starts the postgres container, runs migrations, and opens both
// GORM connections.  It is called at most once per test binary via sync.Once.
func initShared() (*sharedState, error) {
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
		return nil, fmt.Errorf("start postgres container: %w", err)
	}

	adminDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("get connection string: %w", err)
	}

	// --- Run migrations as admin ---
	adminSQLDB, err := sql.Open("pgx", adminDSN)
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("open admin sql.DB: %w", err)
	}

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		_ = adminSQLDB.Close()
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("goose set dialect: %w", err)
	}

	// Migrations include GRANT ... TO hr_app, so hr_app must exist first.
	if err := createAppRole(ctx, adminSQLDB); err != nil {
		_ = adminSQLDB.Close()
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("create hr_app role: %w", err)
	}

	if err := goose.Up(adminSQLDB, "migrations"); err != nil {
		_ = adminSQLDB.Close()
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("goose up: %w", err)
	}

	// --- Open GORM connections ---
	silentLogger := gormlogger.Default.LogMode(gormlogger.Silent)
	if os.Getenv("TEST_DB_DEBUG") != "" {
		silentLogger = gormlogger.Default.LogMode(gormlogger.Info)
	}

	adminGORM, err := gorm.Open(postgres.Open(adminDSN), &gorm.Config{
		Logger:                 silentLogger,
		SkipDefaultTransaction: true,
	})
	if err != nil {
		_ = adminSQLDB.Close()
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("open admin GORM: %w", err)
	}

	// Set connection pool limits on admin connection.
	adminSQLConn, err := adminGORM.DB()
	if err != nil {
		_ = adminSQLDB.Close()
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("get admin sql.DB from GORM: %w", err)
	}
	adminSQLConn.SetMaxOpenConns(10)
	adminSQLConn.SetMaxIdleConns(5)

	// Build an hr_app DSN by swapping user/password in the admin DSN.
	// testcontainers returns a host:port DSN; we reconstruct it cleanly.
	host, err := container.Host(ctx)
	if err != nil {
		_ = adminSQLDB.Close()
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("get container host: %w", err)
	}
	mappedPort, err := container.MappedPort(ctx, "5432")
	if err != nil {
		_ = adminSQLDB.Close()
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("get mapped port: %w", err)
	}
	appDSN := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		host, mappedPort.Port(), testAppUser, testAppPass, testDBName,
	)

	appSQLDB, err := sql.Open("pgx", appDSN)
	if err != nil {
		_ = adminSQLDB.Close()
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("open app sql.DB: %w", err)
	}

	appGORM, err := gorm.Open(postgres.Open(appDSN), &gorm.Config{
		Logger:                 silentLogger,
		SkipDefaultTransaction: true,
	})
	if err != nil {
		_ = appSQLDB.Close()
		_ = adminSQLDB.Close()
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("open app GORM: %w", err)
	}

	// Set connection pool limits on app connection.
	// numGoroutines=50 in the concurrency test drives the upper bound; 25
	// open + 10 idle is a comfortable headroom without starving the OS.
	appSQLConn, err := appGORM.DB()
	if err != nil {
		_ = appSQLDB.Close()
		_ = adminSQLDB.Close()
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("get app sql.DB from GORM: %w", err)
	}
	appSQLConn.SetMaxOpenConns(25)
	appSQLConn.SetMaxIdleConns(10)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	_ = logger // available for future use

	return &sharedState{
		adminGORM: adminGORM,
		appGORM:   appGORM,
		adminSQL:  adminSQLDB,
		appSQL:    appSQLDB,
	}, nil
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
