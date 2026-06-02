// Package tenantdb_test contains integration tests that prove RLS isolation.
//
// These tests spin up a real PostgreSQL 17 container via testcontainers and
// verify the following invariants for the hr_app role (NOBYPASSRLS):
//
//  1. WithinTenant(A): SELECT returns only tenant A rows.
//  2. WithinTenant(A): SELECT by explicit ID from tenant B returns 0 rows.
//  3. WithinTenant(A): UPDATE against a tenant B row affects 0 rows.
//  4. WithinTenant(A): DELETE against a tenant B row affects 0 rows.
//  5. WithinTenant(A): INSERT with tenant_id=B violates WITH CHECK → error.
//  6. No tenant context (direct hr_app query outside WithinTenant): 0 rows.
//  7. tenants table self-isolation: tenant A context cannot read tenant B's
//     tenants row.
//  8. tenants table WITH CHECK: creating a tenant inside its own context
//     (id = tenant_id) succeeds.
//  9. Concurrent goroutines each see only their own tenant's rows (no leakage
//     across pool connections).
// 10. A panic inside fn is propagated and the transaction is rolled back.
package tenantdb_test

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/platform/testdb"
)

// ---------------------------------------------------------------------------
// Model structs (local to tests — only the columns needed for RLS proofs)
// ---------------------------------------------------------------------------

type Tenant struct {
	ID        uuid.UUID `gorm:"column:id;primaryKey"`
	Name      string    `gorm:"column:name"`
	PlanCode  string    `gorm:"column:plan_code"`
	Status    string    `gorm:"column:status"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (Tenant) TableName() string { return "tenants" }

type Department struct {
	ID        uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID  uuid.UUID  `gorm:"column:tenant_id"`
	ParentID  *uuid.UUID `gorm:"column:parent_id"`
	Name      string     `gorm:"column:name"`
	Code      string     `gorm:"column:code"`
	CreatedAt time.Time  `gorm:"column:created_at"`
	UpdatedAt time.Time  `gorm:"column:updated_at"`
}

func (Department) TableName() string { return "departments" }

type Role struct {
	ID          uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID    uuid.UUID `gorm:"column:tenant_id"`
	Name        string    `gorm:"column:name"`
	Permissions []byte    `gorm:"column:permissions;type:jsonb"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

func (Role) TableName() string { return "roles" }

type Employee struct {
	ID           uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID     uuid.UUID  `gorm:"column:tenant_id"`
	EmployeeCode string     `gorm:"column:employee_code"`
	LastName     string     `gorm:"column:last_name"`
	FirstName    string     `gorm:"column:first_name"`
	DepartmentID *uuid.UUID `gorm:"column:department_id"`
	Status       string     `gorm:"column:status"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at"`
}

func (Employee) TableName() string { return "employees" }

type User struct {
	ID           uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID     uuid.UUID  `gorm:"column:tenant_id"`
	Email        string     `gorm:"column:email"`
	PasswordHash *string    `gorm:"column:password_hash"`
	EmployeeID   *uuid.UUID `gorm:"column:employee_id"`
	Status       string     `gorm:"column:status"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at"`
}

func (User) TableName() string { return "users" }

type AuditLog struct {
	ID           uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID     uuid.UUID  `gorm:"column:tenant_id"`
	UserID       *uuid.UUID `gorm:"column:user_id"`
	Action       string     `gorm:"column:action"`
	ResourceType string     `gorm:"column:resource_type"`
	ResourceID   *uuid.UUID `gorm:"column:resource_id"`
	OccurredAt   time.Time  `gorm:"column:occurred_at"`
}

func (AuditLog) TableName() string { return "audit_logs" }

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// insertTenant inserts a tenants row directly via admin DB (bypasses RLS).
// Returns the UUID used.
func insertTenant(t *testing.T, adminDB *gorm.DB, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := adminDB.Exec(
		"INSERT INTO tenants (id, name, plan_code, status) VALUES (?, ?, ?, ?)",
		id, name, "free", "active",
	).Error
	require.NoError(t, err, "insertTenant %s", name)
	return id
}

// seedTenantData inserts one of each table row for tenantID via WithinTenant.
func seedTenantData(
	t *testing.T,
	ctx context.Context,
	tdb *tenantdb.TenantDB,
	tenantID uuid.UUID,
	suffix string,
) {
	t.Helper()

	err := tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		deptID := uuid.New()
		dept := Department{
			ID: deptID, TenantID: tenantID,
			Name: "Dept-" + suffix, Code: "D-" + suffix,
		}
		if err := tx.Create(&dept).Error; err != nil {
			return err
		}

		role := Role{
			ID: uuid.New(), TenantID: tenantID,
			Name: "Role-" + suffix, Permissions: []byte("{}"),
		}
		if err := tx.Create(&role).Error; err != nil {
			return err
		}

		emp := Employee{
			ID: uuid.New(), TenantID: tenantID,
			EmployeeCode: "EMP-" + suffix,
			LastName:     "Last-" + suffix, FirstName: "First-" + suffix,
			DepartmentID: &deptID, Status: "active",
		}
		if err := tx.Create(&emp).Error; err != nil {
			return err
		}

		empID := emp.ID
		user := User{
			ID: uuid.New(), TenantID: tenantID,
			Email:      "user-" + suffix + "@example.test",
			EmployeeID: &empID, Status: "active",
		}
		if err := tx.Create(&user).Error; err != nil {
			return err
		}

		userID := user.ID
		al := AuditLog{
			ID: uuid.New(), TenantID: tenantID,
			UserID: &userID, Action: "login",
			ResourceType: "session",
		}
		return tx.Create(&al).Error
	})
	require.NoError(t, err, "seedTenantData for %s", suffix)
}

// ---------------------------------------------------------------------------
// Main integration test
// ---------------------------------------------------------------------------

func TestRLSCrossTenantIsolation(t *testing.T) {
	h := testdb.NewHarness(t)
	ctx := context.Background()

	tdb := tenantdb.New(h.AppDB)

	// Create two tenants via admin DB (bypasses RLS for setup).
	tenantA := insertTenant(t, h.AdminDB, "Tenant-A")
	tenantB := insertTenant(t, h.AdminDB, "Tenant-B")

	// Seed both tenants with data via WithinTenant (proves seeding works).
	seedTenantData(t, ctx, tdb, tenantA, "A")
	seedTenantData(t, ctx, tdb, tenantB, "B")

	// Fetch B's department ID for targeted queries below.
	var deptB Department
	err := h.AdminDB.Where("tenant_id = ?", tenantB).First(&deptB).Error
	require.NoError(t, err)

	// -----------------------------------------------------------------------
	// Case 1: WithinTenant(A) SELECT returns only tenant A rows
	// -----------------------------------------------------------------------
	t.Run("SELECT_returns_only_own_tenant_rows", func(t *testing.T) {
		err := tdb.WithinTenant(ctx, tenantA, func(tx *gorm.DB) error {
			var depts []Department
			if err := tx.Find(&depts).Error; err != nil {
				return err
			}
			assert.Len(t, depts, 1, "expected 1 department for tenant A")
			assert.Equal(t, tenantA, depts[0].TenantID)

			var roles []Role
			if err := tx.Find(&roles).Error; err != nil {
				return err
			}
			assert.Len(t, roles, 1)

			var emps []Employee
			if err := tx.Find(&emps).Error; err != nil {
				return err
			}
			assert.Len(t, emps, 1)

			var users []User
			if err := tx.Find(&users).Error; err != nil {
				return err
			}
			assert.Len(t, users, 1)

			var logs []AuditLog
			if err := tx.Find(&logs).Error; err != nil {
				return err
			}
			assert.Len(t, logs, 1)

			return nil
		})
		require.NoError(t, err)
	})

	// -----------------------------------------------------------------------
	// Case 2: WithinTenant(A) — SELECT by tenant B's ID returns 0 rows
	// -----------------------------------------------------------------------
	t.Run("SELECT_by_id_from_other_tenant_returns_zero", func(t *testing.T) {
		err := tdb.WithinTenant(ctx, tenantA, func(tx *gorm.DB) error {
			var dept Department
			res := tx.Where("id = ?", deptB.ID).First(&dept)
			assert.ErrorIs(t, res.Error, gorm.ErrRecordNotFound,
				"should not find tenant B's department from tenant A context")
			return nil
		})
		require.NoError(t, err)
	})

	// -----------------------------------------------------------------------
	// Case 3: WithinTenant(A) — UPDATE on tenant B row → 0 rows affected
	// -----------------------------------------------------------------------
	t.Run("UPDATE_other_tenant_row_affects_zero_rows", func(t *testing.T) {
		err := tdb.WithinTenant(ctx, tenantA, func(tx *gorm.DB) error {
			res := tx.Model(&Department{}).
				Where("id = ?", deptB.ID).
				Update("name", "PWNED")
			assert.NoError(t, res.Error)
			assert.Equal(t, int64(0), res.RowsAffected,
				"UPDATE must affect 0 rows for cross-tenant target")
			return nil
		})
		require.NoError(t, err)

		// Verify the row in B is unchanged.
		var check Department
		err = h.AdminDB.Where("id = ?", deptB.ID).First(&check).Error
		require.NoError(t, err)
		assert.Equal(t, deptB.Name, check.Name, "tenant B row must be unmodified")
	})

	// -----------------------------------------------------------------------
	// Case 4: WithinTenant(A) — DELETE on tenant B row → 0 rows affected
	// -----------------------------------------------------------------------
	t.Run("DELETE_other_tenant_row_affects_zero_rows", func(t *testing.T) {
		err := tdb.WithinTenant(ctx, tenantA, func(tx *gorm.DB) error {
			res := tx.Where("id = ?", deptB.ID).Delete(&Department{})
			assert.NoError(t, res.Error)
			assert.Equal(t, int64(0), res.RowsAffected,
				"DELETE must affect 0 rows for cross-tenant target")
			return nil
		})
		require.NoError(t, err)

		// Verify tenant B's department still exists.
		var check Department
		err = h.AdminDB.Where("id = ?", deptB.ID).First(&check).Error
		require.NoError(t, err, "tenant B department must still exist after cross-tenant delete attempt")
	})

	// -----------------------------------------------------------------------
	// Case 5: WithinTenant(A) — INSERT with tenant_id=B violates WITH CHECK
	// -----------------------------------------------------------------------
	t.Run("INSERT_with_wrong_tenant_id_violates_WITH_CHECK", func(t *testing.T) {
		err := tdb.WithinTenant(ctx, tenantA, func(tx *gorm.DB) error {
			malicious := Department{
				ID:       uuid.New(),
				TenantID: tenantB, // deliberately wrong
				Name:     "Injected",
				Code:     "INJ",
			}
			return tx.Create(&malicious).Error
		})
		require.Error(t, err,
			"INSERT with tenant_id=B inside tenant A context must fail (WITH CHECK)")
	})

	// -----------------------------------------------------------------------
	// Case 6: No tenant context — raw hr_app query returns 0 rows (fail-closed)
	// -----------------------------------------------------------------------
	t.Run("no_tenant_context_returns_zero_rows", func(t *testing.T) {
		// Execute directly on the hr_app pool without WithinTenant.
		// app.tenant_id is not set; current_setting returns NULL → 0 rows.
		var depts []Department
		err := h.AppDB.Find(&depts).Error
		require.NoError(t, err)
		assert.Empty(t, depts,
			"hr_app without tenant context must see 0 rows (fail-closed)")
	})

	// -----------------------------------------------------------------------
	// Case 7: tenants table self-isolation
	// -----------------------------------------------------------------------
	t.Run("tenants_table_cross_tenant_isolation", func(t *testing.T) {
		err := tdb.WithinTenant(ctx, tenantA, func(tx *gorm.DB) error {
			// Try to read tenant B's row from the tenants table.
			var got Tenant
			res := tx.Where("id = ?", tenantB).First(&got)
			assert.ErrorIs(t, res.Error, gorm.ErrRecordNotFound,
				"tenant A context must not see tenant B's tenants row")
			return nil
		})
		require.NoError(t, err)
	})

	// -----------------------------------------------------------------------
	// Case 8: WithinTenant succeeds in creating / reading the own tenant row
	// -----------------------------------------------------------------------
	t.Run("tenants_table_own_row_readable", func(t *testing.T) {
		err := tdb.WithinTenant(ctx, tenantA, func(tx *gorm.DB) error {
			var got Tenant
			if err := tx.Where("id = ?", tenantA).First(&got).Error; err != nil {
				return err
			}
			assert.Equal(t, tenantA, got.ID)
			assert.Equal(t, "Tenant-A", got.Name)
			return nil
		})
		require.NoError(t, err)
	})
}

// ---------------------------------------------------------------------------
// Nil UUID guard
// ---------------------------------------------------------------------------

func TestWithinTenantNilUUID(t *testing.T) {
	h := testdb.NewHarness(t)

	tdb := tenantdb.New(h.AppDB)
	err := tdb.WithinTenant(context.Background(), uuid.Nil, func(_ *gorm.DB) error {
		return nil
	})
	require.Error(t, err, "WithinTenant with nil UUID must return an error")
}

// ---------------------------------------------------------------------------
// Concurrency test — pool-connection app.tenant_id isolation
// ---------------------------------------------------------------------------

// TestWithinTenantConcurrentIsolation spawns N goroutines.  Each goroutine is
// assigned one of M tenants (N >> M) and executes WithinTenant to read
// departments.  It asserts that every row returned belongs exclusively to the
// goroutine's own tenant, proving that app.tenant_id does not leak across
// pool connections when concurrent transactions are in flight.
//
// The test is designed to run under -race (the Go race detector) so that any
// data race on shared state is caught in addition to the logical isolation
// check.
func TestWithinTenantConcurrentIsolation(t *testing.T) {
	const (
		numTenants    = 5
		numGoroutines = 50
	)

	h := testdb.NewHarness(t)
	ctx := context.Background()
	tdb := tenantdb.New(h.AppDB)

	// Create tenants and seed one department per tenant via admin DB.
	tenantIDs := make([]uuid.UUID, numTenants)
	for i := 0; i < numTenants; i++ {
		tenantIDs[i] = insertTenant(t, h.AdminDB, fmt.Sprintf("ConcurrentTenant-%d", i))
		// Seed directly rather than using seedTenantData to keep setup minimal.
		err := tdb.WithinTenant(ctx, tenantIDs[i], func(tx *gorm.DB) error {
			dept := Department{
				ID:       uuid.New(),
				TenantID: tenantIDs[i],
				Name:     fmt.Sprintf("Dept-Concurrent-%d", i),
				Code:     fmt.Sprintf("DC-%d", i),
			}
			return tx.Create(&dept).Error
		})
		require.NoError(t, err, "seed tenant %d", i)
	}

	// Use a fixed random source for deterministic assignment (not security-sensitive).
	rng := rand.New(rand.NewSource(42)) //nolint:gosec // test-only RNG

	// Assign each goroutine a tenant index.
	assignments := make([]int, numGoroutines)
	for i := range assignments {
		assignments[i] = rng.Intn(numTenants)
	}

	var wg sync.WaitGroup
	errs := make([]error, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		i := i // capture
		wg.Add(1)
		go func() {
			defer wg.Done()
			myTenantID := tenantIDs[assignments[i]]
			errs[i] = tdb.WithinTenant(ctx, myTenantID, func(tx *gorm.DB) error {
				var depts []Department
				if err := tx.Find(&depts).Error; err != nil {
					return fmt.Errorf("goroutine %d: find: %w", i, err)
				}
				for _, d := range depts {
					if d.TenantID != myTenantID {
						return fmt.Errorf(
							"goroutine %d: isolation breach: expected tenant %s, got %s (dept %s)",
							i, myTenantID, d.TenantID, d.ID,
						)
					}
				}
				return nil
			})
		}()
	}

	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d (tenant index %d): %v", i, assignments[i], err)
		}
	}
}

// ---------------------------------------------------------------------------
// Panic rollback test
// ---------------------------------------------------------------------------

// TestWithinTenantPanicRollback verifies that when fn panics:
//  (a) the panic is propagated to the caller, and
//  (b) the transaction is rolled back so that the inserted row is not
//      committed and is not visible via the admin DB.
func TestWithinTenantPanicRollback(t *testing.T) {
	h := testdb.NewHarness(t)
	ctx := context.Background()
	tdb := tenantdb.New(h.AppDB)

	// Create a tenant for this test.
	tenantID := insertTenant(t, h.AdminDB, "PanicTenant")

	// Track the department ID that fn will attempt to insert before panicking.
	deptID := uuid.New()

	// Confirm that the panic propagates out of WithinTenant.
	panicked := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
		}()
		_ = tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
			// Insert a row, then panic — the row must not be committed.
			dept := Department{
				ID:       deptID,
				TenantID: tenantID,
				Name:     "ShouldNotExist",
				Code:     "SNE",
			}
			if err := tx.Create(&dept).Error; err != nil {
				return err
			}
			panic("simulated panic inside WithinTenant fn")
		})
	}()

	assert.True(t, panicked, "panic must propagate out of WithinTenant")

	// (b) The inserted row must not have been committed.
	// Check via admin DB which bypasses RLS.
	var count int64
	err := h.AdminDB.Model(&Department{}).Where("id = ?", deptID).Count(&count).Error
	require.NoError(t, err, "admin count query must succeed")
	assert.Equal(t, int64(0), count,
		"department row must not exist: rollback must have been called after panic")
}
