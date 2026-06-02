package department_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/department"
	"github.com/your-org/hr-saas/internal/platform/audit"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/platform/testdb"
)

// seedTenant inserts a tenant row directly via the admin DB (no RLS context needed).
func seedTenant(t *testing.T, adminDB *gorm.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO tenants (id, name, plan_code, status, slug) VALUES (?, ?, 'free', 'active', ?)`,
		id, "Test Tenant", id.String()[:8],
	).Error)
	return id
}

// TestDepartmentCRUD tests basic create/read/update/delete of departments.
func TestDepartmentCRUD(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := department.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := uuid.New()

	// Seed a user so audit FK is satisfied.
	require.NoError(t, h.AdminDB.Exec(
		`INSERT INTO users (id, tenant_id, email, status) VALUES (?, ?, ?, 'active')`,
		actorID, tenantID, "actor@example.com",
	).Error)

	t.Cleanup(func() {
		h.TruncateTables("audit_logs", "employee_assignments", "employment_contracts",
			"employees", "departments", "users", "sessions", "tenants")
	})

	ip := "127.0.0.1"

	// Create
	dept, err := svc.Create(ctx, department.CreateInput{
		TenantID: tenantID,
		ActorID:  actorID,
		Name:     "Engineering",
		Code:     "ENG",
		IP:       &ip,
	})
	require.NoError(t, err)
	assert.Equal(t, "Engineering", dept.Name)
	assert.Equal(t, "ENG", dept.Code)
	assert.Equal(t, tenantID, dept.TenantID)

	// Get
	got, err := svc.Get(ctx, tenantID, dept.ID)
	require.NoError(t, err)
	assert.Equal(t, dept.ID, got.ID)
	assert.Equal(t, "Engineering", got.Name)

	// List
	depts, err := svc.List(ctx, tenantID)
	require.NoError(t, err)
	assert.Len(t, depts, 1)

	// Create child department
	child, err := svc.Create(ctx, department.CreateInput{
		TenantID: tenantID,
		ActorID:  actorID,
		ParentID: &dept.ID,
		Name:     "Backend",
		Code:     "BE",
		IP:       &ip,
	})
	require.NoError(t, err)
	assert.Equal(t, &dept.ID, child.ParentID)

	depts, err = svc.List(ctx, tenantID)
	require.NoError(t, err)
	assert.Len(t, depts, 2)

	// Update
	updated, err := svc.Update(ctx, department.UpdateInput{
		TenantID: tenantID,
		ID:       dept.ID,
		ActorID:  actorID,
		Name:     "Engineering (Updated)",
		Code:     "ENG2",
		IP:       &ip,
	})
	require.NoError(t, err)
	assert.Equal(t, "Engineering (Updated)", updated.Name)
	assert.Equal(t, "ENG2", updated.Code)

	// Delete the child first (FK constraint), then the parent.
	err = svc.Delete(ctx, tenantID, child.ID, actorID, &ip)
	require.NoError(t, err)

	err = svc.Delete(ctx, tenantID, dept.ID, actorID, &ip)
	require.NoError(t, err)

	_, err = svc.Get(ctx, tenantID, dept.ID)
	assert.ErrorIs(t, err, department.ErrNotFound)
}

// TestDepartmentRLSCrossTenant verifies that tenant A cannot access tenant B's departments.
func TestDepartmentRLSCrossTenant(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := department.NewService(tdb)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	tenantB := seedTenant(t, h.AdminDB)

	actorA := uuid.New()
	actorB := uuid.New()
	require.NoError(t, h.AdminDB.Exec(
		`INSERT INTO users (id, tenant_id, email, status) VALUES (?, ?, ?, 'active'), (?, ?, ?, 'active')`,
		actorA, tenantA, "a@example.com", actorB, tenantB, "b@example.com",
	).Error)

	t.Cleanup(func() {
		h.TruncateTables("audit_logs", "employee_assignments", "employment_contracts",
			"employees", "departments", "users", "sessions", "tenants")
	})

	ip := "127.0.0.1"

	// Create a department under tenant A.
	deptA, err := svc.Create(ctx, department.CreateInput{
		TenantID: tenantA,
		ActorID:  actorA,
		Name:     "Dept A",
		Code:     "DA",
		IP:       &ip,
	})
	require.NoError(t, err)

	// Attempt to read it using tenant B's context — should return not found.
	_, err = svc.Get(ctx, tenantB, deptA.ID)
	assert.ErrorIs(t, err, department.ErrNotFound, "cross-tenant GET must return ErrNotFound")

	// List under tenant B should return empty.
	deptsB, err := svc.List(ctx, tenantB)
	require.NoError(t, err)
	assert.Empty(t, deptsB, "cross-tenant List must return no rows")

	// Attempt to delete tenant A's department using tenant B's context.
	err = svc.Delete(ctx, tenantB, deptA.ID, actorB, &ip)
	assert.ErrorIs(t, err, department.ErrNotFound, "cross-tenant DELETE must return ErrNotFound")
}

// TestDepartmentAudit verifies that mutations record audit entries with no PII.
func TestDepartmentAudit(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := department.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := uuid.New()
	require.NoError(t, h.AdminDB.Exec(
		`INSERT INTO users (id, tenant_id, email, status) VALUES (?, ?, ?, 'active')`,
		actorID, tenantID, "actor@example.com",
	).Error)

	t.Cleanup(func() {
		h.TruncateTables("audit_logs", "employee_assignments", "employment_contracts",
			"employees", "departments", "users", "sessions", "tenants")
	})

	ip := "127.0.0.1"

	dept, err := svc.Create(ctx, department.CreateInput{
		TenantID: tenantID,
		ActorID:  actorID,
		Name:     "Audit Dept",
		Code:     "AUD",
		IP:       &ip,
	})
	require.NoError(t, err)

	_, err = svc.Update(ctx, department.UpdateInput{
		TenantID: tenantID,
		ID:       dept.ID,
		ActorID:  actorID,
		Name:     "Audit Dept Updated",
		Code:     "AUDU",
		IP:       &ip,
	})
	require.NoError(t, err)

	err = svc.Delete(ctx, tenantID, dept.ID, actorID, &ip)
	require.NoError(t, err)

	// Verify chain integrity.
	err = tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		ok, err := audit.VerifyChain(tx, tenantID)
		require.NoError(t, err)
		assert.True(t, ok, "audit chain must be intact")

		// Confirm no PII (department name) appears as resource_id.
		var rows []struct {
			ResourceID *string `gorm:"column:resource_id"`
		}
		if err := tx.Raw(
			`SELECT resource_id FROM audit_logs WHERE tenant_id = ?`, tenantID,
		).Scan(&rows).Error; err != nil {
			return err
		}
		for _, r := range rows {
			if r.ResourceID != nil {
				// resource_id must be a UUID string, not a name.
				_, parseErr := uuid.Parse(*r.ResourceID)
				assert.NoError(t, parseErr, "resource_id must be a UUID, not PII: %s", *r.ResourceID)
			}
		}
		assert.Len(t, rows, 3, "expected 3 audit entries (create, update, delete)")
		return nil
	})
	require.NoError(t, err)
}
